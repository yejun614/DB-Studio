package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/gofiber/fiber/v2"

	"dbstudio/internal/dbx"
	"dbstudio/internal/model"
	"dbstudio/internal/schema"
	"dbstudio/internal/store"
)

// 스키마 읽기를 담당 노드에 맡기는 통로.
//
// ── 왜 접속 지점이 하나뿐인데 따로 파일을 두는가 ─────────────────────
// 스키마를 읽는 자리는 이 앱에 여러 곳이지만 **실제로 DB에 붙는 함수는 하나**다
// (`introspectConnection`). 그래서 그 함수 한 곳만 고치면 그것을 지나는 모든 기능이
// 함께 담당 노드를 지난다:
//
//	버전 캡처        현재 스키마를 읽어 이력으로 확정
//	스키마 diff      두 스키마 비교
//	마이그레이션     계획 수립·적용 전 확인
//	구조 화면        현재 스키마를 ERD로
//	ERD 편집         저장된 스키마와 실제 DB 대조
//	설명 수정        코멘트 계획 수립
//
// 각 핸들러에서 따로 판단하면 반드시 한 곳이 빠지고, 그때 증상은 **"이 화면에서만 안 된다"**
// 다. P32가 담당 노드 판정을 조회 한 곳(`connColumns`)에 모아 둔 것과 같은 판단이다.
//
// ── 노드는 왜 ID만 받는가 ──────────────────────────────────────────
// 지표 중계와 같다. 노드가 자기 복제본에서 호스트·계정·비밀번호를 스스로 꺼낸다.
// 값까지 넘기면 자격증명이 클러스터 채널로 오가고(그 비밀 하나가 새면 클러스터 전체 DB의
// 비밀번호가 함께 샌다), 두 노드가 서로 다른 값을 보게 될 수도 있다.

// nodeIntrospectRequest는 "이 커넥션의 스키마를 읽어 달라"는 요청이다.
type nodeIntrospectRequest struct {
	ConnectionID string `json:"connectionId"`
}

// handleNodeIntrospect는 이 노드에서 실제로 그 DB에 붙어 스키마를 읽고 돌려준다.
//
// 기록하지 않는다. 버전 캡처가 여기서 행을 만들면 리플리카의 메타 DB에만 생기고 다음
// 복제에서 사라진다 — 그 조용한 손실이 P32에서 실제로 일어났던 일이다(serverRouteProxies
// 주석 참고). 그래서 이 경로는 **읽기만** 하고, 행을 만드는 것은 마스터가 한다.
func (s *Server) handleNodeIntrospect(c *fiber.Ctx) error {
	var req nodeIntrospectRequest
	if err := c.BodyParser(&req); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "요청 형식이 올바르지 않습니다")
	}
	id := strings.TrimSpace(req.ConnectionID)
	if id == "" {
		return fail(c, fiber.StatusBadRequest, "bad_request", "connectionId가 필요합니다")
	}

	conn, err := s.st.GetConnection(c.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		return fail(c, fiber.StatusNotFound, "not_found",
			"이 노드의 복제본에 그 커넥션이 없습니다(복제가 아직 못 따라잡았을 수 있습니다)")
	}
	if err != nil {
		return err
	}
	adapter, err := dbx.Get(conn.Kind)
	if err != nil {
		return fail(c, fiber.StatusBadRequest, "unsupported_kind", err.Error())
	}
	if !adapter.Capabilities().Introspect {
		return fail(c, fiber.StatusBadRequest, "unsupported_kind",
			"이 데이터베이스 종류는 스키마 조회를 지원하지 않습니다")
	}

	secret, err := s.st.GetSecret(c.Context(), id)
	if err != nil {
		return failDetail(c, fiber.StatusBadGateway, "secret_failed",
			"자격증명을 복호화하지 못했습니다", err.Error())
	}

	ctx, cancel := context.WithTimeout(c.Context(), introspectTimeout)
	defer cancel()

	sc, err := adapter.Introspect(ctx, dbx.Target{Conn: conn, Secret: secret})
	if err != nil {
		// 이쪽은 up=0으로 접을 수 없다. 스키마가 없으면 화면이 그릴 것이 없으므로,
		// 실패 이유를 그대로 올려보내 사람이 읽게 한다.
		return failDetail(c, fiber.StatusBadGateway, "introspect_failed",
			"스키마를 읽지 못했습니다", err.Error())
	}
	if sc == nil {
		return fail(c, fiber.StatusBadGateway, "introspect_failed", "스키마를 읽지 못했습니다")
	}
	return c.JSON(sc)
}

// introspectOnNode는 담당 노드에 스키마 읽기를 맡긴다.
//
// 돌려주는 값이 (스키마, 맡겼는가, 에러)인 이유: 부르는 쪽이 "맡겼는데 실패"와 "맡길 곳이
// 없어 여기서 할 차례"를 구분해야 한다. 맡길 곳이 없으면 여기서 읽어야 하므로 그 판단을
// 부르는 쪽에 남긴다(이 함수가 스스로 읽기 시작하면 호출 방향이 뒤집혀 순환이 된다).
func (s *Server) introspectOnNode(ctx context.Context, conn *model.Connection) (*schema.Schema, bool, error) {
	if s.cluster == nil || !s.cluster.Enabled() || conn.NodeID == "" ||
		conn.NodeID == s.cluster.NodeID() {
		return nil, false, nil
	}
	out, err := s.callNode(ctx, conn.NodeID, "/api/v1/node/introspect",
		fiber.Map{"connectionId": conn.ID}, "스키마 읽기")
	if err != nil {
		return nil, true, err
	}
	sc := &schema.Schema{}
	if err := json.Unmarshal(out, sc); err != nil {
		return nil, true, fmt.Errorf("담당 노드가 보낸 스키마를 이해하지 못했습니다: %w", err)
	}
	return sc, true, nil
}
