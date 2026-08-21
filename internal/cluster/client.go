package cluster

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"dbstudio/internal/store"
)

// 마스터를 부르는 쪽(리플리카)의 클라이언트.
//
// 인증은 공용 비밀 하나다. 노드마다 계정을 두지 않는 이유: 노드는 사람이 아니고,
// 자격증명을 하나씩 발급하면 그것을 나눠 주고 회수하는 절차가 또 필요해진다.
// 대신 이 비밀은 "클러스터에 들어올 수 있다"는 뜻이므로 커넥션 비밀번호와 같은 급으로
// 다뤄야 한다(환경변수로 주고, 로그·화면에 절대 싣지 않는다).

// JoinRequest는 노드가 클러스터에 참여할 때 보내는 것이다.
type JoinRequest struct {
	NodeID   string `json:"nodeId"`
	Name     string `json:"name"`
	Address  string `json:"address"`
	Version  string `json:"version"`
	Platform string `json:"platform"`
}

// JoinResponse는 마스터의 답이다.
type JoinResponse struct {
	MasterID   string `json:"masterId"`
	MasterName string `json:"masterName"`
	MasterSeq  int64  `json:"masterSeq"`
	MinSeq     int64  `json:"minSeq"`
	// SchemaVersion은 마스터의 메타 DB 스키마 번호다. 다르면 복제가 어긋날 수 있다.
	SchemaVersion int `json:"schemaVersion"`
}

// HeartbeatRequest는 살아 있음과 복제 지점, 그리고 이 컴퓨터의 상태를 알린다.
type HeartbeatRequest struct {
	NodeID       string `json:"nodeId"`
	AppliedSeq   int64  `json:"appliedSeq"`
	HostSnapshot any    `json:"hostSnapshot,omitempty"`
}

// HeartbeatResponse는 마스터의 최신 복제 지점이다.
type HeartbeatResponse struct {
	MasterSeq int64 `json:"masterSeq"`
	MinSeq    int64 `json:"minSeq"`
}

// ChangesResponse는 복제 로그 조각이다.
type ChangesResponse struct {
	MinSeq  int64              `json:"minSeq"`
	MaxSeq  int64              `json:"maxSeq"`
	Changes []store.ReplChange `json:"changes"`
	// NeedSnapshot이 참이면 요청한 지점이 이미 잘려 나가 이어 붙일 수 없다.
	NeedSnapshot bool `json:"needSnapshot"`
}

func (c *Cluster) post(ctx context.Context, path string, body, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.MasterURL+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.cfg.Secret)
	return c.do(req, out)
}

func (c *Cluster) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.cfg.MasterURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.Secret)
	return c.do(req, out)
}

func (c *Cluster) do(req *http.Request, out any) error {
	res, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("마스터에 닿지 못했습니다: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return fmt.Errorf("마스터가 %d 로 답했습니다: %s", res.StatusCode, string(bytes.TrimSpace(snippet)))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(res.Body).Decode(out)
}

// Join은 클러스터에 참여를 알린다.
func (c *Cluster) Join(ctx context.Context, req JoinRequest) (*JoinResponse, error) {
	var out JoinResponse
	if err := c.post(ctx, "/api/v1/node/join", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Heartbeat는 살아 있음을 알리고 마스터의 최신 지점을 받는다.
func (c *Cluster) Heartbeat(ctx context.Context) (*HeartbeatResponse, error) {
	req := HeartbeatRequest{NodeID: c.id, AppliedSeq: c.Applied()}
	if c.hostSnap != nil {
		req.HostSnapshot = c.hostSnap()
	}
	var out HeartbeatResponse
	if err := c.post(ctx, "/api/v1/node/heartbeat", req, &out); err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.masterSeq = out.MasterSeq
	c.mu.Unlock()
	return &out, nil
}

// FetchChanges는 since 이후의 변경을 받아온다.
func (c *Cluster) FetchChanges(ctx context.Context, since int64, limit int) (*ChangesResponse, error) {
	var out ChangesResponse
	path := fmt.Sprintf("/api/v1/node/changes?since=%d&limit=%d", since, limit)
	if err := c.get(ctx, path, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// FetchSnapshot은 마스터의 메타 DB 스냅샷을 임시 파일로 받아온다.
//
// 메모리에 담지 않는 이유: 스냅샷은 메타 DB 전체이고 수백 MB가 될 수 있다.
// 그것을 통째로 들고 있으면 복제 한 번이 노드의 메모리를 결정한다.
func (c *Cluster) FetchSnapshot(ctx context.Context, dir string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.cfg.MasterURL+"/api/v1/node/snapshot", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.Secret)
	res, err := c.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("마스터에 닿지 못했습니다: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return "", fmt.Errorf("스냅샷 요청이 %d 로 거부됐습니다: %s", res.StatusCode, string(snippet))
	}

	path := filepath.Join(dir, "snapshot-incoming.db")
	f, err := os.Create(path)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(f, res.Body); err != nil {
		f.Close()
		os.Remove(path)
		return "", fmt.Errorf("스냅샷을 받는 중 끊겼습니다: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return "", err
	}
	return path, nil
}

// SendAudit은 이 노드에서 실행한 작업의 감사 기록을 마스터에 남긴다.
//
// 리플리카가 자기 DB에 쓰지 않는 이유: 그 행은 다음 복제 때 사라진다(마스터에는 없는
// 행이므로). 기록이 조용히 없어지는 것은 감사 로그에서 가장 나쁜 실패다.
func (c *Cluster) SendAudit(ctx context.Context, p store.AuditParams) error {
	return c.post(ctx, "/api/v1/node/audit", p, nil)
}
