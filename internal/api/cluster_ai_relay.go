package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/gofiber/fiber/v2"

	"dbstudio/internal/dblog"
	"dbstudio/internal/dbx"
	"dbstudio/internal/model"
	"dbstudio/internal/store"
)

// AI 어시스턴트의 DB 도구를 담당 노드에 맡기는 통로.
//
// ── 왜 도구까지 맡기는가 ───────────────────────────────────────────
// 어시스턴트는 화면과 **다른 길**로 DB에 접속한다. 화면은 라우팅 미들웨어를 지나지만
// 도구는 핸들러 안에서 어댑터를 직접 부르므로, 라우팅 허용 목록을 아무리 늘려도
// 담당 노드를 지나지 않는다.
//
// 그래서 이 도구들은 담당 노드가 지정된 DB에서 이렇게 실패했다:
//
//	사용자: "이 DB 구조 좀 봐 줘"        → introspect_schema → 1045
//	사용자: "이 DB 슬로우 쿼리 찾아 줘"  → search_logs       → 1045
//
// 화면은 멀쩡한데 어시스턴트만 실패하는 모양이라, 원인을 도구 쪽에서 찾게 만든다.
// 지표·스키마 중계와 같은 원칙으로 해결한다: **담당 노드가 접속해 읽고, 값만 돌려준다.**

// nodeLogsRequest는 "이 커넥션의 로그를 읽어 달라"는 요청이다.
//
// 필터를 함께 보내는 이유(스키마·지표와 다른 점): 로그 조회는 "무엇을 찾는가"가 결과를
// 정하므로 필터 없이는 읽을 것이 없다. 필터에는 자격증명이 없으므로 채널로 보내도
// 안전하다 — 커넥션 ID만 보내는 것과 같은 경계다.
type nodeLogsRequest struct {
	ConnectionID string        `json:"connectionId"`
	Filter       *dblog.Filter `json:"filter"`
}

// nodeExploreRequest는 "이 커넥션을 탐색해 달라"는 요청이다(Mongo/Redis).
type nodeExploreRequest struct {
	ConnectionID string `json:"connectionId"`
}

// handleNodeLogs는 이 노드에서 실제로 그 DB에 붙어 로그를 읽고 돌려준다.
func (s *Server) handleNodeLogs(c *fiber.Ctx) error {
	var req nodeLogsRequest
	if err := c.BodyParser(&req); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "요청 형식이 올바르지 않습니다")
	}
	conn, adapter, err := s.nodeReadable(c, req.ConnectionID, "로그 조회")
	if err != nil {
		return err
	}
	if !adapter.Capabilities().Logs {
		return fiber.NewError(fiber.StatusBadRequest,
			"이 DB 종류는 로그 조회를 지원하지 않습니다")
	}
	secret, err := s.st.GetSecret(c.Context(), conn.ID)
	if err != nil {
		return fiber.NewError(fiber.StatusBadGateway,
			"자격증명을 복호화하지 못했습니다: "+err.Error())
	}

	f := req.Filter
	if f == nil {
		f = &dblog.Filter{}
	}
	f.Normalize()

	ctx, cancel := context.WithTimeout(c.Context(), logQueryTimeout)
	defer cancel()

	res, err := adapter.Logs(ctx, dbx.Target{Conn: conn, Secret: secret}, f)
	if err != nil {
		// 로그 조회는 "왜 못 읽었는지"가 결과만큼 중요하므로 이유를 그대로 올린다.
		return fiber.NewError(fiber.StatusBadGateway, "로그를 읽지 못했습니다: "+err.Error())
	}
	if res == nil {
		res = &dblog.Result{}
	}
	return c.JSON(res)
}

// handleNodeExplore는 이 노드에서 실제로 그 DB에 붙어 탐색하고 돌려준다.
func (s *Server) handleNodeExplore(c *fiber.Ctx) error {
	var req nodeExploreRequest
	if err := c.BodyParser(&req); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "요청 형식이 올바르지 않습니다")
	}
	conn, adapter, err := s.nodeReadable(c, req.ConnectionID, "전용 탐색")
	if err != nil {
		return err
	}
	if !adapter.Capabilities().Explore {
		return fiber.NewError(fiber.StatusBadRequest,
			"이 DB 종류는 전용 탐색을 지원하지 않습니다")
	}
	secret, err := s.st.GetSecret(c.Context(), conn.ID)
	if err != nil {
		return fiber.NewError(fiber.StatusBadGateway,
			"자격증명을 복호화하지 못했습니다: "+err.Error())
	}

	ctx, cancel := context.WithTimeout(c.Context(), exploreTimeout)
	defer cancel()

	res, err := dbx.DoExplore(ctx, dbx.Target{Conn: conn, Secret: secret})
	if err != nil {
		return fiber.NewError(fiber.StatusBadGateway, "탐색하지 못했습니다: "+err.Error())
	}
	if res == nil {
		res = &dbx.Explore{}
	}
	return c.JSON(res)
}

// nodeReadable은 노드 전용 읽기 경로들이 공통으로 하는 앞단이다.
//
// 네 곳(지표·스키마·로그·탐색)이 같은 검사를 반복하므로 한 곳에 모은다. 갈라 두면
// 새 경로를 하나 추가할 때 검사 하나를 빠뜨리고, 그 증상은 "이 노드에서는 되는데
// 저 노드에서는 안 된다"로 나타난다 — 원인을 찾기 가장 어려운 모양이다.
//
// ── 실패할 때 반드시 non-nil 에러를 돌려준다 ───────────────────────
// 이 저장소에는 같은 함정이 이미 두 번 적혀 있다(monitor_handlers.go의
// resolveMonitorAccess 주석: "응답을 직접 쓰는 헬퍼를 쓰면 nil 에러가 되어 호출부의
// 검사를 통과해버린다"). 이 함수를 처음 쓸 때 실제로 그렇게 만들었고, 그 결과는
// **nil 포인터 패닉**이었다: fail()이 응답을 쓰고 nil을 돌려주면 부르는 쪽의
// `if err != nil`을 그대로 통과해 버리고, 이어서 `adapter.Capabilities()`가 nil을
// 건드린다. 그래서 여기서는 fiber.NewError를 돌려준다 — 상태 코드는 errorHandler가
// 만들므로 응답을 두 번 쓰지 않는다.
func (s *Server) nodeReadable(c *fiber.Ctx, connID, what string) (*model.Connection, dbx.Adapter, error) {
	id := strings.TrimSpace(connID)
	if id == "" {
		return nil, nil, fiber.NewError(fiber.StatusBadRequest, "connectionId가 필요합니다")
	}
	conn, err := s.st.GetConnection(c.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		// "없다"와 "아직 못 받았다"는 사람이 할 일이 다르다. 복제가 못 따라잡은
		// 노드에서는 커넥션이 정말 없으므로, 단정하면 멀쩡한 설정을 바꾸게 된다.
		return nil, nil, fiber.NewError(fiber.StatusNotFound,
			"이 노드의 복제본에 그 커넥션이 없습니다(복제가 아직 못 따라잡았을 수 있습니다)")
	}
	if err != nil {
		return nil, nil, err
	}
	adapter, err := dbx.Get(conn.Kind)
	if err != nil {
		return nil, nil, fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	_ = what
	return conn, adapter, nil
}

// ---------- 마스터 쪽: 도구에서 부르는 통로 ----------

// relayLogs는 담당 노드가 있으면 그 노드에 로그 조회를 맡긴다.
//
// (결과, 맡겼는가, 에러)를 돌려주는 이유는 introspectOnNode와 같다 — 부르는 쪽이
// "맡겼는데 실패"와 "내가 할 차례"를 구분해야 한다.
func (s *Server) relayLogs(ctx context.Context, conn *model.Connection, f *dblog.Filter) (*dblog.Result, bool, error) {
	if !s.relayNeeded(conn) {
		return nil, false, nil
	}
	out, err := s.callNode(ctx, conn.NodeID, "/api/v1/node/logs",
		fiber.Map{"connectionId": conn.ID, "filter": f}, "로그 조회")
	if err != nil {
		return nil, true, err
	}
	res := &dblog.Result{}
	if err := json.Unmarshal(out, res); err != nil {
		return nil, true, fmt.Errorf("담당 노드가 보낸 로그를 이해하지 못했습니다: %w", err)
	}
	return res, true, nil
}

// relayExplore는 담당 노드가 있으면 그 노드에 탐색을 맡긴다.
func (s *Server) relayExplore(ctx context.Context, conn *model.Connection) (*dbx.Explore, bool, error) {
	if !s.relayNeeded(conn) {
		return nil, false, nil
	}
	out, err := s.callNode(ctx, conn.NodeID, "/api/v1/node/explore",
		fiber.Map{"connectionId": conn.ID}, "전용 탐색")
	if err != nil {
		return nil, true, err
	}
	res := &dbx.Explore{}
	if err := json.Unmarshal(out, res); err != nil {
		return nil, true, fmt.Errorf("담당 노드가 보낸 탐색 결과를 이해하지 못했습니다: %w", err)
	}
	return res, true, nil
}

// relayNeeded는 이 커넥션을 담당 노드에 맡겨야 하는지 답한다.
//
// 두 조건을 한 곳에 모은 이유: 네 곳(지표·스키마·로그·탐색)이 같은 판단을 한다.
// 갈라 두면 한 곳만 조건이 빠지고, 그 증상은 "일부 기능만 안 된다"로 나타난다 —
// 이번 장애가 정확히 그 모양이었다.
func (s *Server) relayNeeded(conn *model.Connection) bool {
	return s.cluster != nil && s.cluster.Enabled() &&
		conn.NodeID != "" && conn.NodeID != s.cluster.NodeID()
}

// localNodeName은 이 프로세스가 도는 노드의 이름이다(클러스터가 아니면 빈 값).
func (s *Server) localNodeName() string {
	if s.cluster == nil {
		return ""
	}
	return s.cluster.Name()
}

// nodeName은 노드 ID의 사람이 읽는 이름을 준다. 못 찾으면 ID를 그대로 쓴다.
func (s *Server) nodeName(ctx context.Context, nodeID string) string {
	if nodeID == "" {
		return s.localNodeName()
	}
	node, err := s.st.GetClusterNode(ctx, nodeID)
	if err != nil || node == nil || strings.TrimSpace(node.Name) == "" {
		return nodeID
	}
	return node.Name
}

// annotateOrigin은 실패 문구에 "어느 노드가 어느 주소로 시도했는지"를 붙인다.
//
// ── 왜 필요한가 ────────────────────────────────────────────────────
// 이 앱에서 이 문구를 읽는 사람은 둘이다. 하나는 화면을 보는 사람이고, 다른 하나는
// **어시스턴트의 모델**이다. 모델은 이 문구를 근거로 "계정에 그 주소를 넣으세요"라고
// 설명하는데, 출처가 없으면 모델도 사람도 계정 범위를 의심하지 못하고 커넥션 설정을
// 뒤지게 된다.
//
// ── 붙이는 것은 인증 거절일 때만 ───────────────────────────────────
// dbx.Origin이 1045 원문에서 출발 주소를 뽑는 것과 같은 경계다. 타임아웃이나 호스트
// 미해석에는 출발 주소라는 개념 자체가 없으므로, 노드 이름만 덧붙이면 읽는 사람이
// 계정을 의심하게 되고 진짜 원인(망·주소)에서 멀어진다.
//
// 그래도 노드 이름은 남긴다 — "어느 노드가 시도했는가"는 어떤 실패에서도 조사에 필요한
// 값이고, 그 노드가 망 밖에 있어서 못 닿은 것일 수 있다. 주소만 조건부로 붙인다.
func (s *Server) annotateOrigin(ctx context.Context, conn *model.Connection, err error) error {
	if err == nil {
		return nil
	}
	origin := dbx.ParseOrigin(err.Error())
	// 출발 주소를 못 뽑았으면(인증 실패가 아니면) 원문 그대로 둔다.
	if origin.Host == "" {
		return err
	}
	// 실행 노드: 담당 노드가 있으면 그 노드가 시도했고, 없으면 여기(요청을 받은 노드)다.
	name := s.localNodeName()
	if conn != nil && conn.NodeID != "" {
		name = s.nodeName(ctx, conn.NodeID)
	}
	return errors.New(origin.WithNode(name).Annotate(err.Error()))
}
