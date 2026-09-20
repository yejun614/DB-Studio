package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"

	"dbstudio/internal/dbx"
	"dbstudio/internal/metric"
	"dbstudio/internal/model"
	"dbstudio/internal/schema"
	"dbstudio/internal/store"
)

// 담당 노드에 **읽기**를 맡기는 통로.
//
// ── 이 파일이 하는 일 ──────────────────────────────────────────────
// 폴러는 마스터에서만 돈다(리플리카가 폴링하면 같은 DB를 두 번 세고, 쓴 행은 다음 복제에서
// 사라진다). 그런데 담당 노드가 지정된 DB는 **마스터가 닿지 못할 수 있다** — 그 DB가 담당
// 노드의 사설망 안에 있기 때문이고, 그 사실이 담당 노드를 지정한 이유다.
//
// 그래서 일을 나눈다. 담당 노드가 **접속해서 읽고**, 마스터가 **기록한다.** 넘기는 것은
// 읽은 결과 값뿐이다:
//
//	지표   metric.Set    (이미 JSON으로 직렬화 가능)
//	스키마 schema.Schema (이미 JSON으로 직렬화 가능)
//
// 전송 형식을 새로 만들지 않은 이유가 그것이다 — 두 값 모두 화면·저장소로 그대로 흘러가는
// 형태라 노드 사이에서만 다른 모양이 될 이유가 없다.
//
// 자격증명은 넘기지 않는다. 담당 노드는 자기 메타 DB **복제본**에서 커넥션과 자격증명을
// 꺼내 쓴다. 클러스터 비밀 하나로 자격증명까지 오가게 하면 그 비밀 하나가 새는 순간
// 클러스터 전체 DB의 비밀번호가 함께 샌다 — server-test와 같은 판단이다(P32).
//
// ── 왜 requireMaster를 붙이지 않는가 ───────────────────────────────
// 이 경로를 부르는 곳은 마스터지만, **실행하는 곳은 담당 노드**이고 담당 노드는
// 리플리카일 수 있다. 그것이 이 경로들의 존재 이유다(server-databases·server-test와 같다).

// nodeCollector는 monitor.Collector를 구현한다.
//
// monitor가 이 구현을 모르게 두는 것이 핵심이다. 폴러는 "누군가 대신 읽어 준다"까지만
// 알고, 그 누군가가 HTTP로 다른 노드에 물어본다는 사실은 여기(api)에만 있다.
type nodeCollector struct {
	srv *Server
}

// LocalNodeID는 이 프로세스가 도는 노드의 ID다.
//
// 클러스터가 아니면 빈 값이다. 그때는 담당 노드라는 개념 자체가 없으므로 폴러가
// 이 값을 물어볼 일도 없다(모든 커넥션의 NodeID가 비어 있다).
func (c *nodeCollector) LocalNodeID() string {
	if c.srv.cluster == nil {
		return ""
	}
	return c.srv.cluster.NodeID()
}

// Collect는 담당 노드에 그 커넥션의 지표를 물어 온다.
func (c *nodeCollector) Collect(ctx context.Context, conn *model.Connection) (*metric.Set, error) {
	out, err := c.srv.callNode(ctx, conn.NodeID, "/api/v1/node/collect",
		fiber.Map{"connectionId": conn.ID}, "지표")
	if err != nil {
		return nil, err
	}
	set := metric.NewSet()
	if err := json.Unmarshal(out, set); err != nil {
		return nil, fmt.Errorf("담당 노드가 보낸 지표를 이해하지 못했습니다: %w", err)
	}
	return set, nil
}

// Introspect는 담당 노드에 그 커넥션의 스키마를 물어 온다.
//
// 스키마를 읽는 HTTP 경로는 화면용(`introspectConnection`)과 이쪽 둘이다. 화면용은
// 담당 노드로 **프록시**되지만(라우팅 허용 목록에 있다), 드리프트 확인은 폴러가 부르는
// 것이라 프록시될 요청 자체가 없다 — 그래서 여기서 같은 노드 경로를 직접 부른다.
func (c *nodeCollector) Introspect(ctx context.Context, conn *model.Connection) (*schema.Schema, error) {
	out, err := c.srv.callNode(ctx, conn.NodeID, "/api/v1/node/introspect",
		fiber.Map{"connectionId": conn.ID}, "스키마 읽기")
	if err != nil {
		return nil, err
	}
	sc := &schema.Schema{}
	if err := json.Unmarshal(out, sc); err != nil {
		return nil, fmt.Errorf("담당 노드가 보낸 스키마를 이해하지 못했습니다: %w", err)
	}
	return sc, nil
}

// callNode는 담당 노드의 /node/* 경로를 부르고 본문을 돌려준다.
//
// what은 실패 문구에 들어갈 말이다("지표", "스키마"). 노드에 닿지 못한 것과 노드가
// 거절한 것을 구분해 적는 이유: 둘은 고칠 자리가 다르다(전자는 망·주소, 후자는 그 노드가
// 보는 메타 DB 상태).
func (s *Server) callNode(ctx context.Context, nodeID, path string, body any, what string) ([]byte, error) {
	if s.cluster == nil || !s.cluster.Enabled() {
		return nil, errors.New("클러스터가 아닙니다")
	}
	node, err := s.st.GetClusterNode(ctx, nodeID)
	if err != nil {
		return nil, fmt.Errorf("담당 노드를 찾을 수 없습니다 (id %s): %w", nodeID, err)
	}
	if node.Status != "active" || strings.TrimSpace(node.Address) == "" {
		return nil, fmt.Errorf("담당 노드 \"%s\" 의 주소를 알 수 없습니다", node.Name)
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	target := strings.TrimRight(node.Address, "/") + path
	req, err := http.NewRequestWithContext(ctx, fiber.MethodPost, target, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
	req.Header.Set(fiber.HeaderAuthorization, "Bearer "+s.cluster.Config().Secret)

	res, err := (&http.Client{Timeout: nodeRouteTimeout}).Do(req)
	if err != nil {
		slog.Warn("담당 노드에 "+what+"를 맡기지 못했습니다",
			"node", node.Name, "address", node.Address, "err", err)
		return nil, fmt.Errorf("담당 노드 \"%s\" 에 닿지 못했습니다: %w", node.Name, err)
	}
	defer res.Body.Close()
	reply, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, fmt.Errorf("담당 노드 \"%s\" 의 응답을 읽지 못했습니다: %w", node.Name, err)
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, fmt.Errorf("담당 노드 \"%s\" 가 %d 로 답했습니다: %s",
			node.Name, res.StatusCode, nodeErrorMessage(reply))
	}
	return reply, nil
}

// nodeErrorMessage는 노드가 보낸 실패 본문에서 사람이 읽을 문구를 꺼낸다.
//
// 문구를 꺼내는 이유: 노드가 보낸 실패의 원인(예: 자격증명 복호화 실패)이 그대로
// 마스터의 실패 문구에 이어져야 사람이 한 화면에서 원인까지 읽는다. 상태 코드만
// 남기면 "502"라는 답을 받고 다시 노드 로그를 찾아가야 한다.
func nodeErrorMessage(body []byte) string {
	var e struct {
		Message string `json:"message"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal(body, &e); err == nil {
		if m := strings.TrimSpace(e.Message); m != "" {
			return m
		}
		if m := strings.TrimSpace(e.Error); m != "" {
			return m
		}
	}
	if s := strings.TrimSpace(string(body)); s != "" {
		return s
	}
	return "이유를 알 수 없습니다"
}

// nodeCollectRequest는 "이 커넥션의 지표를 읽어 달라"는 요청이다.
//
// 커넥션 ID만 보내는 이유: 노드는 자기 복제본에서 나머지(호스트·계정·비밀번호)를
// 스스로 꺼낸다. 값까지 보내면 마스터와 노드가 서로 다른 값을 볼 수 있고(그 상태를
// 판단하는 근거가 갈라진다), 자격증명이 채널로 오간다.
type nodeCollectRequest struct {
	ConnectionID string `json:"connectionId"`
}

// handleNodeCollect는 이 노드에서 실제로 그 DB에 붙어 지표를 읽고 돌려준다.
//
// 기록은 하지 않는다. 이 노드가 자기 메타 DB에 쓰면 다음 복제에서 그 행이 사라진다 —
// 적는 것은 언제나 마스터의 일이다(그래서 리플리카는 폴러를 돌리지 않는다).
func (s *Server) handleNodeCollect(c *fiber.Ctx) error {
	var req nodeCollectRequest
	if err := c.BodyParser(&req); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "요청 형식이 올바르지 않습니다")
	}
	id := strings.TrimSpace(req.ConnectionID)
	if id == "" {
		return fail(c, fiber.StatusBadRequest, "bad_request", "connectionId가 필요합니다")
	}

	conn, err := s.st.GetConnection(c.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		// 이 노드의 복제본에 그 커넥션이 없다. 아직 복제를 못 따라잡았을 수 있으므로
		// 그 사실을 그대로 알린다 — "없다"와 "아직 못 받았다"는 사람이 할 일이 다르다.
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
	if !adapter.Capabilities().Monitor {
		return fail(c, fiber.StatusBadRequest, "unsupported_kind",
			"이 DB 종류는 지표 수집을 지원하지 않습니다")
	}

	secret, err := s.st.GetSecret(c.Context(), id)
	if err != nil {
		// 복호화 실패가 여기서 나온다. 마스터 키가 노드마다 다르면 이것이 원인이고,
		// 증상은 "복제는 정상인데 이 노드에서만 접속이 실패한다"로 나타난다.
		return failDetail(c, fiber.StatusBadGateway, "secret_failed",
			"자격증명을 복호화하지 못했습니다", err.Error())
	}

	ctx, cancel := context.WithTimeout(c.Context(), nodeRouteTimeout)
	defer cancel()

	set, err := adapter.Metrics(ctx, dbx.Target{Conn: conn, Secret: secret})
	if err != nil {
		// 어댑터의 규칙과 같게 up=0으로 접는다. 접속 실패는 "물어봤는데 죽어 있다"이고,
		// 그것은 에러가 아니라 관측값이다 — 에러로 만들면 왜 죽었는지가 사라진다.
		set = metric.NewSet()
		set.Gauge(metric.NameUp, 0, metric.UnitCount)
		set.AddNote("지표 수집 불가: %v", err)
	}
	if set == nil {
		set = metric.NewSet()
	}
	if set.CollectedAt.IsZero() {
		set.CollectedAt = time.Now().UTC()
	}
	// 응답은 받은 그대로 돌려준다(metric.Set). 어느 노드가 실행했는지는 **마스터**가
	// 자기 노드 목록에서 알고 그쪽에서 붙인다 — 노드가 자기 이름을 실어 보내면
	// 이름이 바뀌었을 때 두 곳이 다른 이름을 말하게 된다(server-test가 testedBy를
	// 마스터에서 붙이는 것과 같은 판단이다).
	return c.JSON(set)
}
