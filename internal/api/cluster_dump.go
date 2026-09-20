package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"

	"dbstudio/internal/backup"
	"dbstudio/internal/model"
	"dbstudio/internal/store"
)

// 백업을 담당 노드에 맡기는 통로 (P33-1).
//
// ── 왜 필요한가 ────────────────────────────────────────────────────
// 백업 요청(`POST /connections/:id/backups`)은 **쓰기**라서 리플리카에서 오면 마스터로
// 넘어간다. 그러면 마스터가 덤프를 시도하는데, 담당 노드가 지정된 DB는 마스터가 닿지
// 못할 수 있다 — 그것이 담당 노드를 지정한 이유다. 그래서 그 자리에서 1045가 났다
// (버전 캡처와 같은 원인).
//
// 그래서 마스터가 그 일을 담당 노드에 맡긴다. 지표·스키마·로그는 **값**만 받으면 됐지만
// 백업은 **파일**이라 한 단계가 더 있다: 노드가 덤프해 마스터가 보관한다.
//
// ── 흐름 ──────────────────────────────────────────────────────────
//	① 사용자 → 마스터: POST /connections/:id/backups
//	② 마스터: 담당 노드가 따로 있으면 그 노드에 POST /api/v1/node/dump
//	   (이름은 마스터가 만들어 함께 보낸다 — 노드가 이름을 만들면 경로 조작 검사가 필요해진다)
//	③ 노드: 자기 DB에 붙어 덤프 → 그 이름으로 본문에 흘려보낸다
//	④ 마스터: 받아서 보관 → 백업 기록을 만들고 202로 답한다
//
// 마스터가 노드에서 받아 가는 방향이 아닌 이유: 그러려면 마스터가 모든 노드에 닿을 수
// 있어야 하고, 그것이 이 클러스터가 피하려는 상황이다 — 호스트 지표를 하트비트에 실어
// 올리는 것과 같은 판단(clusternodes.go).

// nodeDumpRequest는 마스터가 담당 노드에 보내는 "이 커넥션을 떠서 올려라" 요청이다.
//
// 커넥션 ID와 옵션·이름만 보낸다. 자격증명은 보내지 않는다 — 노드가 자기 복제본에서
// 꺼내 쓴다(다른 중계 경로와 같은 판단: 그 비밀이 새면 클러스터 전체 DB의 비밀번호가 샌다).
type nodeDumpRequest struct {
	ConnectionID string   `json:"connectionId"`
	Name         string   `json:"name"`
	Scope        string   `json:"scope"`
	Tables       []string `json:"tables,omitempty"`
	DropIfExists bool     `json:"dropIfExists"`
}

// backupOnNode는 담당 노드에 백업을 맡기고, 그 답을 이 노드의 백업으로 기록한다.
//
// 돌려주는 (기록할 파일 이름, 맡겼는가, 에러):
//   - 맡길 곳이 없으면 handled=false → 부르는 쪽이 지금처럼 로컬에서 백업한다
//   - 맡겼으면 이름이 돌아온다 → 그 이름으로 기록을 만든다
func (s *Server) backupOnNode(c *fiber.Ctx, conn *model.Connection, req createBackupRequest, actor *model.User) (string, bool, error) {
	if s.cluster == nil || !s.cluster.Enabled() || conn.NodeID == "" ||
		conn.NodeID == s.cluster.NodeID() {
		return "", false, nil
	}
	node, err := s.st.GetClusterNode(c.Context(), conn.NodeID)
	if err != nil {
		return "", true, fiber.NewError(fiber.StatusBadGateway,
			"담당 노드를 찾을 수 없습니다. 서버 설정에서 담당 노드를 다시 고르세요")
	}
	if node.Status != "active" || strings.TrimSpace(node.Address) == "" {
		return "", true, fiber.NewError(fiber.StatusBadGateway,
			"담당 노드 \""+node.Name+"\" 의 주소를 알 수 없어 백업할 수 없습니다")
	}

	// 이름은 **마스터가** 만든다. 노드가 이름을 만들어 올리면 마스터가 그것을 검사해야
	// 하고(경로 조작), 검사를 빠뜨린 날이 곧 디렉터리 탈출이 된다. 마스터가 정해 보내면
	// 경계를 넘는 값이 없다.
	name, err := s.backups.AllocateDumpName(string(conn.Kind))
	if err != nil {
		return "", true, fiber.NewError(fiber.StatusBadRequest, err.Error())
	}

	body, err := json.Marshal(nodeDumpRequest{
		ConnectionID: conn.ID, Name: name, Scope: req.Scope,
		Tables: req.Tables, DropIfExists: req.DropIfExists,
	})
	if err != nil {
		return "", true, err
	}
	target := strings.TrimRight(node.Address, "/") + "/api/v1/node/dump"
	httpReq, err := http.NewRequestWithContext(c.Context(), fiber.MethodPost, target, strings.NewReader(string(body)))
	if err != nil {
		return "", true, err
	}
	httpReq.Header.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
	httpReq.Header.Set(fiber.HeaderAuthorization, "Bearer "+s.cluster.Config().Secret)

	// 덤프는 클 수 있다. 노드 사이 일반 상한(5분)보다 넉넉히 잡는다.
	client := &http.Client{Timeout: dumpRelayTimeout}
	res, err := client.Do(httpReq)
	if err != nil {
		slog.Warn("담당 노드에 백업을 맡기지 못했습니다",
			"node", node.Name, "connection", conn.Name, "err", err)
		return "", true, fiber.NewError(fiber.StatusBadGateway,
			"담당 노드 \""+node.Name+"\" 에 닿지 못해 백업할 수 없습니다")
	}
	defer res.Body.Close()

	if res.StatusCode < 200 || res.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(res.Body, 8192))
		return "", true, fiber.NewError(fiber.StatusBadGateway,
			"담당 노드 \""+node.Name+"\" 가 "+fmt.Sprint(res.StatusCode)+" 로 답했습니다: "+
				nodeErrorMessage(raw))
	}

	// 담당 노드가 보낸 본문이 곧 덤프 파일이다. 받아서 이 노드의 백업 디렉터리에 둔다.
	saved, err := s.backups.ReceiveDump(name, res.Body)
	if err != nil {
		slog.Error("담당 노드의 덤프를 받지 못했습니다",
			"node", node.Name, "connection", conn.Name, "err", err)
		return "", true, fiber.NewError(fiber.StatusBadGateway,
			"담당 노드 \""+node.Name+"\" 의 덤프를 받지 못했습니다: "+err.Error())
	}
	return saved, true, nil
}

// handleNodeDump는 담당 노드가 이 요청을 받아 덤프하고 본문으로 흘려보낸다.
//
// ── 이 경로만 requireMaster를 붙이지 않는 이유 ─────────────────────
// 다른 /node/* 읽기 경로는 담당 노드가 **실행**해야 해서 붙이지 않았고, 이 경로는
// 담당 노드가 **덤프를 만들어** 올려야 해서 역시 붙이지 않는다. 담당 노드는 리플리카일
// 수 있고, 그것이 이 경로의 존재 이유다.
//
// 기록은 남기지 않는다. 이 노드가 자기 메타 DB에 백업 행을 만들면 다음 복제에서 사라진다 —
// 적는 것은 마스터의 일이다(P32의 조용한 손실과 같은 자리).
func (s *Server) handleNodeDump(c *fiber.Ctx) error {
	var req nodeDumpRequest
	if err := c.BodyParser(&req); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "요청 형식이 올바르지 않습니다")
	}
	id := strings.TrimSpace(req.ConnectionID)
	if id == "" {
		return fail(c, fiber.StatusBadRequest, "bad_request", "connectionId가 필요합니다")
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return fail(c, fiber.StatusBadRequest, "bad_request", "name이 필요합니다")
	}
	if req.Scope == "" {
		req.Scope = backup.ScopeFull
	}
	if !backup.ValidScope(req.Scope) {
		return fail(c, fiber.StatusBadRequest, "invalid_scope", "알 수 없는 덤프 범위입니다")
	}

	conn, err := s.st.GetConnection(c.Context(), id)
	if err == store.ErrNotFound {
		return fail(c, fiber.StatusNotFound, "not_found",
			"이 노드의 복제본에 그 커넥션이 없습니다(복제가 아직 못 따라잡았을 수 있습니다)")
	}
	if err != nil {
		return err
	}
	secret, err := s.st.GetSecret(c.Context(), id)
	if err != nil {
		return failDetail(c, fiber.StatusBadGateway, "secret_failed",
			"자격증명을 복호화하지 못했습니다", err.Error())
	}

	// ── 본문으로 흘려보낸다 ────────────────────────────────────────
	// 파일로 만들었다가 읽어 보내지 않는다: 노드에 임시 파일을 남기면 지우는 일이 하나
	// 더 생기고, 지우지 못한 파일이 조용히 디스크를 채운다. 받는 쪽(마스터)이 잘린 것을
	// 구분할 수 있으므로 중간 파일 없이도 안전하다.
	c.Set(fiber.HeaderContentType, "application/octet-stream")
	c.Set("X-Dump-Name", name)
	c.Status(fiber.StatusOK)

	// ── 컨텍스트를 스트리밍이 끝날 때까지 살려 둔다 ────────────────
	//
	// 여기서 `defer cancel()` 을 쓰면 안 된다. SendStream 은 본문을 **다 보내기 전에**
	// 반환하므로(스트림을 넘겨주는 것이 전부다), 핸들러가 끝나는 순간 컨텍스트가 죽고
	// 덤프가 "context canceled" 로 중단된다. 실제로 그렇게 만들었다가 2노드 실측에서
	// 빈 본문(0바이트)이 나왔고, 원인은 덤프 코드가 아니라 이 한 줄이었다.
	//
	// 그래서 취소는 **덤프 goroutine 이 끝난 뒤**에 부른다(fileWriter 가 맡는다).
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Context()), dumpRelayTimeout)

	p := backup.StreamParams{
		Target: backup.Target{Conn: conn, Secret: secret},
		Options: backup.Options{
			Scope: req.Scope, Tables: req.Tables, DropIfExists: req.DropIfExists,
		},
		Trigger: "manual",
	}
	return c.SendStream(&fileWriter{c: c, ctx: ctx, cancel: cancel, srv: s, p: p})
}

// fileWriter는 덤프를 응답 본문에 흘려보내는 io.Writer다.
//
// c.SendStream이 io.Reader를 받으므로 writer를 reader로 맞춰 준다. 파이프를 쓰는 이유:
// 덤프를 만드는 코드는 io.Writer만 알면 되고(어디로 가는지 모른다), 이쪽은 Fiber의
// 본문 스트림에 쓴다. 중간 파일도, 메모리 버퍼도 없다.
type fileWriter struct {
	c   *fiber.Ctx
	ctx context.Context
	// cancel은 덤프가 끝난 뒤에 부른다. 핸들러가 아니라 여기서 부르는 이유는
	// SendStream이 본문을 다 보내기 전에 반환하기 때문이다(cluster_dump.go 주석 참고).
	cancel context.CancelFunc
	srv    *Server
	p      backup.StreamParams
	pr     *io.PipeReader
	pw     *io.PipeWriter
	// started는 덤프 goroutine을 한 번만 띄우기 위한 표시다.
	started bool
	err     error
	done    chan struct{}
}

func (f *fileWriter) attach() {
	if f.started {
		return
	}
	f.started = true
	f.pr, f.pw = io.Pipe()
	f.done = make(chan struct{})
	go func() {
		defer close(f.done)
		// 취소는 여기서 부른다. 핸들러에서 defer로 두면 스트리밍 도중에 컨텍스트가
		// 죽어 덤프가 중단된다(SendStream은 다 보내기 전에 반환한다).
		defer f.cancel()
		// ── 덤프 실패를 조용히 넘기지 않는다 ────────────────────────
		// 노드는 응답을 흘려보내기 시작한 뒤에 덤프가 실패하면 상태 코드를 바꿀 수 없다.
		// 그래서 마스터는 "200 OK + 잘린 본문"을 받는다.
		//
		// 마스터가 gzip 검사로 그 파일을 거절하므로(ReceiveDump) 잘못된 백업이 남지는
		// 않는다. 다만 **왜 실패했는지**가 노드 로그에도 남아야 조사가 시작된다 —
		// 없으면 "백업이 왜 안 되지"만 남고 원인을 찾을 자리가 사라진다.
		err := f.srv.backups.WriteDumpTo(f.ctx, f.pw, f.p)
		if err != nil {
			slog.Error("담당 노드에서 덤프를 만들지 못했습니다",
				"connection", f.p.Target.Conn.Name, "scope", f.p.Options.Scope, "err", err)
		}
		// 에러를 실어 닫으면 읽는 쪽이 그 사실을 안다(잘린 본문이 성공으로 전송되지 않는다).
		f.pw.CloseWithError(err)
	}()
}

func (f *fileWriter) Read(p []byte) (int, error) {
	f.attach()
	return f.pr.Read(p)
}

// dumpRelayTimeout은 덤프 하나를 노드 사이로 옮기는 데 허용하는 시간이다.
//
// 노드 사이 일반 상한(5분)보다 넉넉하다. 큰 DB의 덤프는 그보다 오래 걸리고, 중간에
// 끊으면 다시 해야 한다(잘린 파일을 남기지 않으므로 안전하다).
const dumpRelayTimeout = 2 * time.Hour
