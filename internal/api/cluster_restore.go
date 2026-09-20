package api

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"

	"dbstudio/internal/backup"
	"dbstudio/internal/model"
	"dbstudio/internal/store"
)

// 복구를 담당 노드에 맡기는 통로 (P33-2).
//
// ── 왜 필요한가 ────────────────────────────────────────────────────
// 복구는 백업의 역방향이고, 백업과 같은 문제를 갖는다:
//
//	· 요청이 **쓰기**라서 리플리카에서 오면 마스터로 넘어온다
//	· 담당 노드가 지정된 DB는 **마스터가 닿지 못할 수 있다** — 담당 노드를 지정한 이유다
//
// 그래서 마스터가 복구를 시도하면 1045로 죽는다(백업·버전 캡처와 같은 원인).
//
// ── 방향: 노드가 마스터에서 파일을 **받아 온다** ────────────────────
// 백업은 노드가 만들어 마스터로 올렸다. 복구는 반대인데, 처음에는 마스터가 파일을 노드의
// 요청 **본문**에 실어 밀어 넣으려 했다. 그 방법은 두 가지를 막았다:
//
//	· Fiber의 요청 본문 스트리밍을 켜야 한다(전역 설정 변경 — 모든 POST에 영향)
//	· 본문 크기 상한(BodyLimit)을 함께 올려야 한다(그러면 아무 엔드포인트나 큰 본문을 받는다)
//
// 그래서 방향을 뒤집었다: **노드가 마스터에서 파일을 받아 온다.** 노드는 이미 마스터 주소와
// 클러스터 비밀을 알고 있고(복제·하트비트가 그 길을 쓴다), 마스터가 파일을 흘려보내는 것은
// 스냅샷 경로에 선례가 있다(`GET /api/v1/node/snapshot` → `SetBodyStream`).
//
//	마스터 : "이 백업으로 복구하라" (작은 JSON) → 담당 노드
//	노드   : GET /api/v1/node/dump-file?name=... 로 파일을 받아 자기 DB에 적용
//	노드   : (문장 수, 실패 문장, 오류)를 JSON 으로 돌려준다
//	마스터 : 그 답으로 복구 기록을 남긴다 (기록은 언제나 마스터의 일이다)
//
// ── 진행 상황을 노드가 남기지 않는 대가 ───────────────────────────
// 노드가 자기 메타 DB 에 복구 행을 만들면 다음 복제에서 사라진다. 그래서 노드는 진행률을
// 적지 않고 **끝난 결과만** 돌려준다 — 긴 복구 동안 화면이 진행률을 보여주지 못한다.
// 사라질 행에 적는 것보다 정직하다(그 행을 읽는 화면은 어차피 마스터에 있다).

// nodeRestoreRequest 는 마스터가 담당 노드에 보내는 요청이다.
//
// 파일을 싣지 않는다 — 노드가 이름으로 받아 온다. 그래서 이 요청은 작은 JSON 이고,
// 전역 설정(요청 본문 스트리밍·크기 상한)을 건드리지 않아도 된다.
type nodeRestoreRequest struct {
	ConnectionID string `json:"connectionId"`
	FileName     string `json:"fileName"`
}

// restoreOnNode 는 담당 노드에 복구를 맡기고 그 결과를 돌려준다.
//
// 돌려주는 (결과, 맡겼는가, 에러): 맡길 곳이 없으면 handled=false → 부르는 쪽이 로컬에서
// 복구한다. 맡겼으면 결과가 돌아온다(실패도 결과 안에 담긴다 — 노드가 실행까지 갔기 때문).
func (s *Server) restoreOnNode(c *fiber.Ctx, b *store.Backup, conn *model.Connection) (*backup.RestoreResult, bool, error) {
	if s.cluster == nil || !s.cluster.Enabled() || conn.NodeID == "" ||
		conn.NodeID == s.cluster.NodeID() {
		return nil, false, nil
	}
	node, err := s.st.GetClusterNode(c.Context(), conn.NodeID)
	if err != nil {
		return nil, true, fiber.NewError(fiber.StatusBadGateway,
			"담당 노드를 찾을 수 없습니다. 서버 설정에서 담당 노드를 다시 고르세요")
	}
	if node.Status != "active" || strings.TrimSpace(node.Address) == "" {
		return nil, true, fiber.NewError(fiber.StatusBadGateway,
			"담당 노드 \""+node.Name+"\" 의 주소를 알 수 없어 복구할 수 없습니다")
	}

	body, err := json.Marshal(nodeRestoreRequest{
		ConnectionID: conn.ID, FileName: b.FileName,
	})
	if err != nil {
		return nil, true, err
	}
	target := strings.TrimRight(node.Address, "/") + "/api/v1/node/restore"
	req, err := http.NewRequestWithContext(c.Context(), fiber.MethodPost, target, strings.NewReader(string(body)))
	if err != nil {
		return nil, true, err
	}
	req.Header.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
	req.Header.Set(fiber.HeaderAuthorization, "Bearer "+s.cluster.Config().Secret)

	// 복구는 길다. 노드 사이 일반 상한(5분)보다 훨씬 넉넉히 잡는다.
	res, err := (&http.Client{Timeout: restoreRelayTimeout}).Do(req)
	if err != nil {
		slog.Warn("담당 노드에 복구를 맡기지 못했습니다",
			"node", node.Name, "connection", conn.Name, "err", err)
		return nil, true, fiber.NewError(fiber.StatusBadGateway,
			"담당 노드 \""+node.Name+"\" 에 닿지 못해 복구할 수 없습니다")
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))

	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, true, fiber.NewError(fiber.StatusBadGateway,
			"담당 노드 \""+node.Name+"\" 가 "+strconv.Itoa(res.StatusCode)+" 로 답했습니다: "+
				nodeErrorMessage(raw))
	}
	out := &backup.RestoreResult{}
	if err := json.Unmarshal(raw, out); err != nil {
		return nil, true, fiber.NewError(fiber.StatusBadGateway,
			"담당 노드의 복구 결과를 이해하지 못했습니다")
	}
	return out, true, nil
}

// handleNodeRestore 는 담당 노드가 마스터의 백업을 받아 자기 DB 에 적용한다.
//
// 이 경로도 requireMaster 를 붙이지 않는다 — 담당 노드는 리플리카일 수 있고, 그 노드만
// 그 DB 에 닿는다.
//
// 기록을 남기지 않는다: 이 노드가 자기 메타 DB 에 복구 행을 만들면 다음 복제에서 사라진다.
// 그래서 결과만 돌려주고, 적는 것은 마스터가 한다.
func (s *Server) handleNodeRestore(c *fiber.Ctx) error {
	var req nodeRestoreRequest
	if err := c.BodyParser(&req); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "요청 형식이 올바르지 않습니다")
	}
	connID := strings.TrimSpace(req.ConnectionID)
	if connID == "" {
		return fail(c, fiber.StatusBadRequest, "bad_request", "connectionId가 필요합니다")
	}
	name := strings.TrimSpace(req.FileName)
	if name == "" {
		return fail(c, fiber.StatusBadRequest, "bad_request", "fileName이 필요합니다")
	}

	conn, err := s.st.GetConnection(c.Context(), connID)
	if err == store.ErrNotFound {
		return fail(c, fiber.StatusNotFound, "not_found",
			"이 노드의 복제본에 그 커넥션이 없습니다(복제가 아직 못 따라잡았을 수 있습니다)")
	}
	if err != nil {
		return err
	}
	secret, err := s.st.GetSecret(c.Context(), connID)
	if err != nil {
		return failDetail(c, fiber.StatusBadGateway, "secret_failed",
			"자격증명을 복호화하지 못했습니다", err.Error())
	}

	ctx, cancel := context.WithTimeout(c.Context(), restoreRelayTimeout)
	defer cancel()

	// 마스터에서 파일을 받아 온다.
	raw, err := s.fetchDumpFromMaster(ctx, name)
	if err != nil {
		return failDetail(c, fiber.StatusBadGateway, "fetch_failed",
			"마스터에서 백업 파일을 받지 못했습니다", err.Error())
	}
	defer raw.Close()

	// ── 받은 것을 푼다 ──────────────────────────────────────────────
	// 백업 파일은 gzip 이다(마스터가 그렇게 보관한다). 마스터는 파일을 **바이트 그대로**
	// 흘려보내므로(그게 스트리밍의 뜻이다) 푸는 것은 받는 쪽의 일이다.
	//
	// 실측에서 이것을 빠뜨려 MySQL 이 gzip 헤더(`\x1f\x8b`)를 SQL 로 받고 1064 로 죽었다.
	// 로컬 경로는 OpenDumpFile 이 풀어 주므로 그쪽 코드만 보면 드러나지 않는다.
	reader, err := gzip.NewReader(raw)
	if err != nil {
		return failDetail(c, fiber.StatusBadGateway, "corrupt_dump",
			"백업 파일이 손상되었습니다", err.Error())
	}
	defer reader.Close()

	p := backup.RestoreParams{
		Kind:   string(conn.Kind),
		Target: backup.Target{Conn: conn, Secret: secret},
	}
	// 진행률을 넘기지 않는다(nil) — 이 노드에는 복구 행이 없다.
	done, total, failed, applyErr := s.backups.ApplyStream(ctx, reader, p, nil)

	out := backup.RestoreResult{Done: done, Total: total, FailedStatement: failed}
	if applyErr != nil {
		out.Error = applyErr.Error()
		slog.Error("담당 노드의 복구가 실패했습니다",
			"connection", conn.Name, "done", done, "total", total, "err", applyErr)
	} else {
		slog.Info("담당 노드에서 복구를 마쳤습니다",
			"connection", conn.Name, "done", done, "total", total)
	}
	// 실패해도 200 으로 돌려준다. 응답은 "실행해 봤다"는 뜻이고 실행 결과는 본문에 담긴다 —
	// 마스터가 그것을 기록에 적는다. 상태 코드로 실패를 알리면 마스터가 **어디까지 갔는지**를
	// 잃고, 그 숫자가 복구에서 가장 중요한 정보다(절반 적용된 DB에 다시 부으면 중복 키가 쏟아진다).
	return c.JSON(out)
}

// fetchDumpFromMaster 는 마스터에서 백업 파일을 받아 온다.
//
// 노드가 파일을 **당겨 오는** 방향이다. 마스터가 밀어 넣으려면 요청 본문 스트리밍을 켜고
// 크기 상한을 올려야 하는데(전역 설정이라 모든 POST에 영향), 당겨 오면 그 변경이 필요 없다.
// 마스터 주소와 클러스터 비밀은 이 노드가 이미 알고 있다(복제·하트비트가 그 길을 쓴다).
func (s *Server) fetchDumpFromMaster(ctx context.Context, name string) (io.ReadCloser, error) {
	base := ""
	if s.cluster != nil {
		base = strings.TrimRight(strings.TrimSpace(s.cluster.Config().MasterURL), "/")
	}
	if base == "" {
		return nil, fmt.Errorf("마스터 주소가 설정되지 않았습니다(-cluster-master)")
	}
	target := base + "/api/v1/node/dump-file?name=" + url.QueryEscape(name)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set(fiber.HeaderAuthorization, "Bearer "+s.cluster.Config().Secret)

	res, err := (&http.Client{Timeout: restoreRelayTimeout}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("마스터에 닿지 못했습니다: %w", err)
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		defer res.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(res.Body, 8192))
		return nil, fmt.Errorf("마스터가 %d 로 답했습니다: %s", res.StatusCode, nodeErrorMessage(raw))
	}
	return res.Body, nil
}

// handleNodeDumpFile 은 마스터가 보관한 백업 파일을 노드에게 흘려보낸다.
//
// 스냅샷 경로(`handleClusterSnapshot`)와 같은 이유로 `SetBodyStream`을 쓴다: 백업은
// 수백 MB가 될 수 있고, 메모리에 담으면 노드 하나가 복구할 때마다 마스터의 메모리를
// 그만큼 먹는다. 파일로 떠서 흘려보내는 것도 같은 판단이다(경로는 디스크에 이미 있다).
//
// requireMaster 를 붙인다. 이 일은 **마스터가 자기 파일을 내주는 것**이고, 리플리카가
// 받아도 그 파일은 거기 없다(백업은 마스터에 보관된다 — P33-1).
func (s *Server) handleNodeDumpFile(c *fiber.Ctx) error {
	name := strings.TrimSpace(c.Query("name"))
	if name == "" {
		return fail(c, fiber.StatusBadRequest, "bad_request", "name이 필요합니다")
	}
	// FilePath 를 지난다 — 이름은 마스터가 만든 것이지만, 경로를 조립하는 곳에서는
	// 언제나 확인한다는 규칙이 여기에도 적용된다.
	path, err := s.backups.FilePath(name)
	if err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", err.Error())
	}
	f, err := os.Open(path)
	if err != nil {
		// 파일이 없는 것은 정상적인 실패다(보존 기간이 지나 지워졌을 수 있다).
		// 그 사실을 그대로 알린다 — 빈 본문을 보내면 복구가 "문장 0개"로 끝난다.
		return fail(c, fiber.StatusNotFound, "not_found",
			"백업 파일이 없습니다. 보존 기간이 지나 삭제되었을 수 있습니다")
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	c.Set(fiber.HeaderContentType, "application/octet-stream")
	c.Response().SetBodyStream(f, int(info.Size()))
	return nil
}

// restoreRelayTimeout 은 복구 하나를 노드 사이로 옮기고 실행하는 데 허용하는 시간이다.
//
// 노드 사이 일반 상한(5분)보다 훨씬 넉넉하다. 큰 DB 의 복구는 몇십 분이 걸리고, 중간에
// 끊으면 **어디까지 적용됐는지**가 흐려진다(복구는 다시 실행될 수 있지만 그 사실을 아는
// 것이 다음 판단의 근거다).
const restoreRelayTimeout = 2 * time.Hour
