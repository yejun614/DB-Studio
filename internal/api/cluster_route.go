package api

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"

	"dbstudio/internal/store"
)

// 노드 라우팅.
//
// 분산 환경에서는 어떤 DB가 특정 서버에서만 닿는다(사설망, 방화벽, VPN). 그 커넥션에
// "담당 노드"를 지정해 두면, 어느 노드로 들어온 요청이든 그 노드에서 실행된다 —
// 사용자는 어느 화면에서 눌렀는지 신경 쓸 필요가 없다.
//
// 무엇을 넘기고 무엇을 넘기지 않는가: 대상 DB에 **접속하는** 요청만 넘긴다. 넘겨받은
// 노드가 리플리카일 수 있고, 리플리카는 자기 메타 DB에 쓸 수 없기 때문이다. 그래서
// 메타 DB에 행을 남기는 작업(백업 기록, 버전 캡처, 마이그레이션 적용)은 넘기지 않고
// 마스터에서 처리한다. 감사 기록은 예외다 — 그것만은 마스터로 따로 전달된다.

// routableSuffix는 담당 노드로 넘길 수 있는 커넥션 하위 경로다.
//
// 허용 목록으로 둔 이유: 새 경로가 생겼을 때 기본값이 "넘긴다"이면, 메타 DB에 쓰는
// 경로가 리플리카에서 실행되어 그 기록이 조용히 사라진다. 모르는 것은 넘기지 않는다.
var routableSuffix = map[string]bool{
	"/data/objects":    true,
	"/data/query":      true,
	"/data/mutate":     true,
	"/data/batch":      true,
	"/statement":       true,
	"/statement/check": true,
	"/schema":          true,
	"/schema/ddl":      true,
	"/schema/diff":     true,
	"/explore":         true,
	"/logs":            true,
	"/logs/sources":    true,
	"/test":            true,
}

// execHeader가 붙은 요청은 "네가 실행하라"는 뜻이다. 이 표시가 있으면 다시 넘기지 않는다
// (넘기면 두 노드가 서로에게 요청을 던지는 고리가 된다).
const execHeader = "X-Cluster-Exec"

// localClusterExec은 "이 요청은 내가 담당인 DB에 대한 것"이라는 표시다.
const localClusterExec = "clusterExecLocal"

// clusterRoute는 담당 노드가 따로 있는 커넥션 요청을 그 노드로 넘긴다.
func (s *Server) clusterRoute(c *fiber.Ctx) error {
	if s.cluster == nil || !s.cluster.Enabled() || c.Get(execHeader) != "" {
		return c.Next()
	}
	connID, suffix, ok := splitConnPath(c.Path())
	if !ok || !routableSuffix[suffix] {
		return c.Next()
	}
	conn, err := s.st.GetConnection(c.Context(), connID)
	if err != nil || conn.NodeID == "" || conn.NodeID == s.cluster.NodeID() {
		// 담당이 없거나 나라면 여기서 실행한다. 커넥션을 못 찾는 경우도 그대로 통과시킨다 —
		// "없는 커넥션"이라는 답은 핸들러가 이미 제대로 한다.
		if err == nil && conn.NodeID != "" && conn.NodeID == s.cluster.NodeID() {
			// 내가 담당인 DB는 마스터로 넘기지 않는다. 넘기면 마스터가 다시 나에게
			// 되돌려 보내므로(그 DB에 닿는 노드는 나뿐이다) 왕복만 한 번 더 는다.
			c.Locals(localClusterExec, true)
		}
		return c.Next()
	}

	node, err := s.st.GetClusterNode(c.Context(), conn.NodeID)
	if err != nil {
		return fail(c, fiber.StatusBadGateway, "unknown_node",
			"이 DB의 담당 노드를 찾을 수 없습니다. 커넥션 설정에서 담당 노드를 다시 고르세요")
	}
	if node.Status != "active" || strings.TrimSpace(node.Address) == "" {
		return fail(c, fiber.StatusBadGateway, "node_unavailable",
			"담당 노드 \""+node.Name+"\" 의 주소를 알 수 없어 요청을 넘길 수 없습니다")
	}
	return s.proxyTo(c, node)
}

// splitConnPath는 /api/v1/connections/:id/<suffix> 를 쪼갠다.
func splitConnPath(path string) (id, suffix string, ok bool) {
	const prefix = "/api/v1/connections/"
	if !strings.HasPrefix(path, prefix) {
		return "", "", false
	}
	rest := path[len(prefix):]
	slash := strings.Index(rest, "/")
	if slash < 0 {
		return "", "", false
	}
	return rest[:slash], rest[slash:], true
}

// proxyTo는 요청을 다른 노드로 그대로 넘기고 그 답을 돌려준다.
func (s *Server) proxyTo(c *fiber.Ctx, node *store.ClusterNode) error {
	target := strings.TrimRight(node.Address, "/") + c.OriginalURL()
	req, err := http.NewRequestWithContext(c.Context(), c.Method(), target, bytes.NewReader(c.Body()))
	if err != nil {
		return err
	}
	for _, h := range []string{
		fiber.HeaderContentType, fiber.HeaderCookie, fiber.HeaderAccept,
		"X-Requested-With", fiber.HeaderUserAgent,
	} {
		if v := c.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}
	req.Header.Set(fiber.HeaderXForwardedFor, clientIP(c))
	req.Header.Set(execHeader, node.ID)

	client := &http.Client{Timeout: nodeRouteTimeout}
	res, err := client.Do(req)
	if err != nil {
		slog.Warn("담당 노드로 요청을 넘기지 못했습니다",
			"node", node.Name, "address", node.Address, "path", c.Path(), "err", err)
		return fail(c, fiber.StatusBadGateway, "node_unreachable",
			"담당 노드 \""+node.Name+"\" 에 닿지 못했습니다")
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return fail(c, fiber.StatusBadGateway, "node_read", "담당 노드의 응답을 읽지 못했습니다")
	}
	if ct := res.Header.Get(fiber.HeaderContentType); ct != "" {
		c.Set(fiber.HeaderContentType, ct)
	}
	// 어느 노드가 실제로 실행했는지 남긴다. 조사할 때 이 한 줄이 없으면 같은 요청이
	// 왜 어떤 노드에서는 되고 어떤 노드에서는 안 되는지 알 수 없다.
	c.Set("X-Cluster-Node", node.Name)
	return c.Status(res.StatusCode).Send(body)
}

// nodeRouteTimeout은 담당 노드의 답을 기다리는 시간이다.
// 질의 실행이 포함되므로 넉넉히 잡는다.
const nodeRouteTimeout = 5 * time.Minute
