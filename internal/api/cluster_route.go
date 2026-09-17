package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"

	"dbstudio/internal/dbx"
	"dbstudio/internal/model"
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

// ---------- 서버 라우팅 ----------
//
// 커넥션 라우팅과 무엇이 다른가.
//
// 커넥션(`/connections/:id/*`)은 **이미 등록된 DB에 접속하는** 요청이라 그 DB의 담당
// 노드로 넘기면 끝이다. 서버(`/servers/:id/*`)는 다르다 — 거기서 하는 일은 두 종류다.
//
//	DB 목록 읽기   그 서버에 실제로 접속해야만 된다. 담당 노드가 아니면 아예 못 한다.
//	행 만들기      메타 DB에 쓴다. 리플리카에서 하면 다음 복제 때 사라진다.
//
// 그래서 **한 요청을 두 노드가 나눠 한다**: 담당 노드가 실제 접속을 하고(목록·검증),
// 마스터가 그 결과로 행을 만든다. 넘기는 것은 `POST /api/v1/node/server-databases`
// 하나뿐이고, 그 답을 받아 행을 만드는 것은 평소의 쓰기 경로(마스터에서 실행)다.
//
// 왜 이 경로는 `clusterForward`보다 **앞**에 있어야 하는가: 담당 노드가 리플리카일 때
// 뒤에 두면 요청이 먼저 마스터로 넘어가 그 DB에 닿지 못하는 마스터가 목록을 읽으려
// 한다. 순서가 곧 정확성이다.

// serverRouteableSuffix는 담당 노드로 넘길 수 있는 서버 하위 경로다.
//
// 담당 노드에 닿아야만 되는 것만 넣는다. 서버 수정(PUT)·삭제(DELETE)·합치기(merge)는
// 메타 DB의 일이라 넘기지 않는다 — 넘기면 리플리카가 자기 메타 DB를 고치려 한다.
var serverRouteableSuffix = map[string]bool{
	"/databases": true,
}

// clusterRouteServer는 담당 노드가 따로 있는 서버의 요청을 그 노드로 넘긴다.
func (s *Server) clusterRouteServer(c *fiber.Ctx) error {
	if s.cluster == nil || !s.cluster.Enabled() || c.Get(execHeader) != "" {
		return c.Next()
	}
	id, suffix, ok := splitServerPath(c.Path())
	if !ok || !serverRouteableSuffix[suffix] {
		return c.Next()
	}
	srv, err := s.st.GetServer(c.Context(), id)
	if err != nil || srv.NodeID == "" || srv.NodeID == s.cluster.NodeID() {
		// 담당이 없거나 나라면 여기서 실행한다. 커넥션 라우팅과 같은 판단이며,
		// 내가 담당이면 이 요청이 마스터로 다시 넘어가지 않게 표시만 남긴다.
		if err == nil && srv.NodeID != "" && srv.NodeID == s.cluster.NodeID() {
			c.Locals(localClusterExec, true)
		}
		return c.Next()
	}

	node, err := s.st.GetClusterNode(c.Context(), srv.NodeID)
	if err != nil {
		return fail(c, fiber.StatusBadGateway, "unknown_node",
			"이 서버의 담당 노드를 찾을 수 없습니다. 서버 설정에서 담당 노드를 다시 고르세요")
	}
	if node.Status != "active" || strings.TrimSpace(node.Address) == "" {
		return fail(c, fiber.StatusBadGateway, "node_unavailable",
			"담당 노드 \""+node.Name+"\" 의 주소를 알 수 없어 요청을 넘길 수 없습니다")
	}
	return s.proxyTo(c, node)
}

// splitServerPath는 /api/v1/servers/:id/<suffix> 를 쪼갠다.
func splitServerPath(path string) (id, suffix string, ok bool) {
	const prefix = "/api/v1/servers/"
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

// ---------- 노드에서 도는 몫 ----------

// nodeServerDatabasesRequest는 담당 노드에게 "네가 그 서버에 접속해 이름을 알려 달라"는
// 요청이다. 자격증명은 **보내지 않는다** — 노드가 자기 메타 DB(복제본)에서 꺼내 쓴다.
//
// 보내지 않는 이유가 중요하다. 클러스터 비밀 하나로 노드 사이를 인증하는데, 그 비밀로
// 커넥션 비밀번호까지 주고받으면 그 비밀 하나가 새는 순간 클러스터의 모든 DB 자격증명이
// 함께 샌다. 지금 구조에서 그 비밀로 얻을 수 있는 것은 메타 DB의 복사본까지다.
type nodeServerDatabasesRequest struct {
	ServerID string `json:"serverId"`
}

// handleNodeServerDatabases는 이 노드가 실제로 그 서버에 접속해 DB 이름과 검증 결과를
// 돌려준다. 서버 등록·수정에서 "그 DB에 닿는가"를 확인하는 자리이기도 하다.
//
// 마스터 전용으로 두지 않는 이유(다른 /node/* 경로와 다르다): 이 일은 **담당 노드가**
// 해야 하는 일이고, 담당 노드는 리플리카일 수 있다.
func (s *Server) handleNodeServerDatabases(c *fiber.Ctx) error {
	var req nodeServerDatabasesRequest
	if err := c.BodyParser(&req); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "요청 형식이 올바르지 않습니다")
	}
	srv, err := s.st.GetServer(c.Context(), strings.TrimSpace(req.ServerID))
	if errors.Is(err, store.ErrNotFound) {
		return fail(c, fiber.StatusNotFound, "not_found", "서버를 찾을 수 없습니다")
	}
	if err != nil {
		return err
	}
	sec, err := s.st.GetServerSecret(c.Context(), srv.ID)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(c.Context(), nodeRouteTimeout)
	defer cancel()

	list, err := dbx.ListDatabases(ctx, srv, sec)
	if err != nil {
		return failDetail(c, fiber.StatusBadGateway, "list_failed",
			"DB 목록을 읽지 못했습니다: "+err.Error(), string(srv.Kind))
	}
	dbx.SortDatabases(list)
	return c.JSON(fiber.Map{"databases": list})
}

// nodeServerTestRequest는 담당 노드에게 "네가 이 대상에 붙어 봐 달라"는 요청이다.
type nodeServerTestRequest struct {
	// ServerID가 있으면 노드가 자기 메타 DB(복제본)에서 자격증명을 꺼내 쓴다.
	ServerID     string            `json:"serverId"`
	Kind         model.DBKind      `json:"kind"`
	Host         string            `json:"host"`
	Port         int               `json:"port"`
	Options      model.Options     `json:"options"`
	DatabaseName string            `json:"databaseName"`
	Username     string            `json:"username"`
	Environment  model.Environment `json:"environment"`

	// Password는 **아직 저장되지 않은** 값을 시험할 때만 온다.
	//
	// ── 이 칸의 경계 ───────────────────────────────────────────────
	// 서버를 새로 등록하면서 "연결 테스트"를 누르면 그 비밀번호는 어디에도 저장되어
	// 있지 않다. 노드가 확인하려면 받아야 하고, 받지 않으면 그 버튼이 새 서버에서는
	// 동작하지 않는다 — 그러면 사람은 저장한 뒤에야 담당 노드를 잘못 골랐음을 안다.
	//
	// 저장된 서버를 시험할 때는 이 칸을 **비워 보낸다.** 노드가 자기 복제본에서
	// 꺼내므로 클러스터 비밀로 오갈 이유가 없다.
	Password string `json:"password"`
}

// handleNodeServerTest는 이 노드에서 실제로 붙어 보고 결과를 돌려준다.
func (s *Server) handleNodeServerTest(c *fiber.Ctx) error {
	var req nodeServerTestRequest
	if err := c.BodyParser(&req); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "요청 형식이 올바르지 않습니다")
	}

	secret := &model.Secret{Username: strings.TrimSpace(req.Username), Password: req.Password}
	kind := req.Kind
	if id := strings.TrimSpace(req.ServerID); id != "" {
		srv, err := s.st.GetServer(c.Context(), id)
		if errors.Is(err, store.ErrNotFound) {
			return fail(c, fiber.StatusNotFound, "not_found", "서버를 찾을 수 없습니다")
		}
		if err != nil {
			return err
		}
		stored, err := s.st.GetServerSecret(c.Context(), srv.ID)
		if err != nil {
			return err
		}
		kind = srv.Kind
		if secret.Username == "" {
			secret.Username = stored.Username
		}
		if secret.Password == "" {
			secret.Password = stored.Password
		}
		if secret.Extra == nil {
			secret.Extra = stored.Extra
		}
	}

	adapter, err := dbx.Get(kind)
	if err != nil {
		return fail(c, fiber.StatusBadRequest, "unsupported_kind", err.Error())
	}
	probe := &model.Connection{
		Kind: kind, Host: strings.TrimSpace(req.Host), Port: req.Port,
		DatabaseName: strings.TrimSpace(req.DatabaseName),
		Environment:  req.Environment, Options: req.Options,
	}

	ctx, cancel := context.WithTimeout(c.Context(), nodeRouteTimeout)
	defer cancel()

	info, pingErr := adapter.Ping(ctx, dbx.Target{Conn: probe, Secret: secret})
	if pingErr != nil {
		return c.JSON(fiber.Map{"ok": false, "message": pingErr.Error(),
			"target": adapter.Redacted(dbx.Target{Conn: probe, Secret: secret})})
	}
	return c.JSON(fiber.Map{"ok": true, "server": info,
		"target": adapter.Redacted(dbx.Target{Conn: probe, Secret: secret})})
}

// ---------- 담당 노드로 시험을 넘기는 쪽 ----------

// testNodeFor는 이 접속 시험을 어느 노드에 맡겨야 하는지 고른다. 빈 값이면 여기서 한다.
//
// 두 갈래인 이유: 시험은 두 자리에서 온다.
//
//	저장된 서버 수정 화면  서버의 담당 노드가 답이다(그 노드가 실제로 접속한다)
//	새 서버 등록 화면      아직 저장 전이라 담당 노드가 폼에만 있다. 폼이 들고 온 값을 쓴다
//
// 커넥션 하나의 시험(`/connections/:id/test`)은 여기 오지 않는다 — 그쪽은 라우팅
// 허용 목록에 이미 들어 있어 담당 노드로 그대로 넘어간다(실효 담당 노드는 store가
// 서버 값까지 합쳐서 준다).
func (s *Server) testNodeFor(c *fiber.Ctx, serverID, formNodeID string) string {
	if serverID = strings.TrimSpace(serverID); serverID != "" {
		srv, err := s.st.GetServer(c.Context(), serverID)
		if err != nil {
			return strings.TrimSpace(formNodeID)
		}
		return srv.NodeID
	}
	return strings.TrimSpace(formNodeID)
}

// testOnNode는 접속 시험을 그 서버의 담당 노드에 맡긴다.
//
// ── 왜 넘기는가 ────────────────────────────────────────────────────
// 시험의 목적이 "담당 노드에서 이 DB에 닿는가"를 아는 것이다. 마스터에서 붙여 보고
// 성공하면 그 사실은 아무것도 말해 주지 않는다 — 요청을 실제로 실행할 노드는 담당
// 노드이고, 그 노드가 닿지 못하면 등록해도 영원히 접속 실패다. 사설망 DB에서
// "테스트는 성공인데 등록하니 안 된다"가 정확히 여기서 나온다.
//
// 넘길 수 없으면(담당 없음·클러스터 아님) nil을 돌려주고 부르는 쪽이 여기서 시험한다.
func (s *Server) testOnNode(c *fiber.Ctx, nodeID string, req nodeServerTestRequest) (fiber.Map, error) {
	nodeID = strings.TrimSpace(nodeID)
	if nodeID == "" || s.cluster == nil || !s.cluster.Enabled() || nodeID == s.cluster.NodeID() {
		return nil, nil
	}
	node, err := s.st.GetClusterNode(c.Context(), nodeID)
	if err != nil {
		return nil, fiber.NewError(fiber.StatusBadGateway,
			"이 서버의 담당 노드를 찾을 수 없습니다. 서버 설정에서 담당 노드를 다시 고르세요")
	}
	if node.Status != "active" || strings.TrimSpace(node.Address) == "" {
		return nil, fiber.NewError(fiber.StatusBadGateway,
			"담당 노드 \""+node.Name+"\" 의 주소를 알 수 없어 시험할 수 없습니다")
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	target := strings.TrimRight(node.Address, "/") + "/api/v1/node/server-test"
	httpReq, err := http.NewRequestWithContext(c.Context(), fiber.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
	if s.cluster != nil {
		httpReq.Header.Set(fiber.HeaderAuthorization, "Bearer "+s.cluster.Config().Secret)
	}
	client := &http.Client{Timeout: nodeRouteTimeout}
	res, err := client.Do(httpReq)
	if err != nil {
		slog.Warn("담당 노드에 접속 시험을 넘기지 못했습니다",
			"node", node.Name, "host", req.Host, "err", err)
		return nil, fiber.NewError(fiber.StatusBadGateway,
			"담당 노드 \""+node.Name+"\" 에 닿지 못해 시험할 수 없습니다")
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, fiber.NewError(fiber.StatusBadGateway, "담당 노드의 응답을 읽지 못했습니다")
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		var failure struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(raw, &failure)
		msg := strings.TrimSpace(failure.Message)
		if msg == "" {
			msg = "담당 노드 \"" + node.Name + "\" 가 " + strconv.Itoa(res.StatusCode) + " 로 답했습니다"
		}
		return nil, fiber.NewError(fiber.StatusBadGateway, msg)
	}
	var out fiber.Map
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fiber.NewError(fiber.StatusBadGateway, "담당 노드의 응답을 이해하지 못했습니다")
	}
	// 어느 노드가 시험했는지 남긴다. 조사할 때 이 한 줄이 없으면 "왜 여기서는 되고
	// 저기서는 안 되는가"의 출발점을 찾을 수 없다.
	out["testedBy"] = node.Name
	return out, nil
}
