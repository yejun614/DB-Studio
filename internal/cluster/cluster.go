// Package cluster는 여러 서버에서 뜬 DB Studio를 하나처럼 다루게 한다.
//
// 구조는 마스터-리플리카다. 마스터가 메타 DB의 유일한 주인이고, 리플리카는 그 내용을
// 복제해 읽기를 처리하며 변경은 마스터로 넘긴다.
//
// 왜 이 구조인가: 이 앱의 상태는 SQLite 파일 하나다. 여러 노드가 각자 쓰면 같은 행을
// 서로 다르게 고칠 수 있고, SQLite에는 그 충돌을 풀 방법이 없다. 쓰기를 한 곳으로 모으면
// 충돌 자체가 생기지 않는다 — "누가 이겼는가"를 사람이 나중에 조사하는 일이 없어진다.
//
// 그 대신 잃는 것은 마스터가 멈췄을 때의 쓰기다. 그때 리플리카는 **읽기 전용으로 계속
// 동작한다**: 화면은 열리고 지표와 이력은 그대로 보인다. 자동 승격은 하지 않는다 —
// 노드가 서로를 못 볼 때 각자 자신을 마스터로 올리면 두 개의 진실이 생기고, 그것은
// 잠깐의 정지보다 훨씬 나쁘다. 승격은 운영자가 역할을 바꿔 재시작해서 한다.
package cluster

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"dbstudio/internal/crypto"
	"dbstudio/internal/store"
)

// 역할.
const (
	RoleStandalone = "standalone" // 클러스터를 쓰지 않는다(기본값)
	RoleMaster     = "master"
	RoleReplica    = "replica"
)

// Config는 프로세스를 띄울 때 정해지는 클러스터 설정이다.
type Config struct {
	Role      string
	MasterURL string // 리플리카가 부를 마스터 주소 (http://host:port)
	Secret    string // 노드 사이 공용 비밀. 이것이 곧 클러스터 참여 자격이다
	NodeName  string // 화면에 보일 이름. 비우면 호스트 이름
	Advertise string // 다른 노드가 이 노드를 부를 주소

	SyncInterval      time.Duration
	HeartbeatInterval time.Duration
	LogKeep           time.Duration // 복제 로그 보존 기간
	LogMaxRows        int           // 복제 로그 최대 줄 수
}

// DefaultConfig는 기본값이다.
func DefaultConfig() Config {
	return Config{
		Role:              RoleStandalone,
		SyncInterval:      2 * time.Second,
		HeartbeatInterval: 10 * time.Second,
		LogKeep:           24 * time.Hour,
		LogMaxRows:        200_000,
	}
}

// Validate는 설정이 말이 되는지 본다.
//
// 여기서 막는 이유: 잘못된 클러스터 설정은 조용히 동작한다. 비밀이 빠진 리플리카는
// 계속 401을 받으며 "복제가 안 되는 것처럼" 보이고, 그 사실을 알아채는 것은 대개
// 데이터가 어긋난 뒤다.
func (c Config) Validate() error {
	switch c.Role {
	case "", RoleStandalone:
		return nil
	case RoleMaster:
		if strings.TrimSpace(c.Secret) == "" {
			return errors.New("클러스터 비밀(-cluster-secret 또는 DBSTUDIO_CLUSTER_SECRET)이 필요합니다")
		}
		return nil
	case RoleReplica:
		if strings.TrimSpace(c.Secret) == "" {
			return errors.New("클러스터 비밀(-cluster-secret 또는 DBSTUDIO_CLUSTER_SECRET)이 필요합니다")
		}
		if strings.TrimSpace(c.MasterURL) == "" {
			return errors.New("리플리카에는 마스터 주소(-cluster-master)가 필요합니다")
		}
		if !strings.HasPrefix(c.MasterURL, "http://") && !strings.HasPrefix(c.MasterURL, "https://") {
			return errors.New("마스터 주소는 http:// 또는 https:// 로 시작해야 합니다")
		}
		return nil
	default:
		return fmt.Errorf("알 수 없는 클러스터 역할입니다: %q (standalone | master | replica)", c.Role)
	}
}

// Status는 화면과 API가 보는 클러스터 상태다.
type Status struct {
	Enabled   bool   `json:"enabled"`
	Role      string `json:"role"`
	NodeID    string `json:"nodeId"`
	NodeName  string `json:"nodeName"`
	Address   string `json:"address"`
	MasterURL string `json:"masterUrl,omitempty"`

	// AppliedSeq는 이 노드가 적용한 마지막 복제 지점, MasterSeq는 마스터의 최신 지점이다.
	AppliedSeq int64 `json:"appliedSeq"`
	MasterSeq  int64 `json:"masterSeq"`
	Lag        int64 `json:"lag"`

	LastSyncAt *time.Time `json:"lastSyncAt,omitempty"`
	LastError  string     `json:"lastError,omitempty"`
	Connected  bool       `json:"connected"`
}

// Cluster는 이 노드의 클러스터 참여 상태다.
type Cluster struct {
	cfg    Config
	st     *store.Store
	log    *slog.Logger
	id     string
	client *http.Client

	// hostSnap은 하트비트에 실을 이 컴퓨터의 상태를 준다. nil일 수 있다.
	hostSnap func() any
	// snapshotDir는 받아온 스냅샷을 잠시 두는 곳이다. 데이터 디렉터리를 쓰는 이유는
	// 임시 디렉터리가 메타 DB보다 작은 볼륨에 있는 경우가 흔하기 때문이다.
	snapshotDir string

	mu         sync.RWMutex
	applied    int64
	masterSeq  int64
	lastSync   time.Time
	lastErr    string
	connected  bool
	joined     bool
	kick       chan struct{}
	appliedSig chan struct{} // 적용 지점이 올라갈 때마다 닫히고 새로 만들어진다
}

// New는 클러스터 참여자를 만든다. 노드 ID는 데이터 디렉터리의 파일에 남는다.
//
// DB가 아니라 파일에 두는 이유: 리플리카의 메타 DB는 스냅샷으로 통째로 교체될 수 있다.
// 자기 정체성을 그 안에 두면 스냅샷을 받는 순간 자기가 누구인지 잊는다.
func New(cfg Config, st *store.Store, dataDir string, log *slog.Logger) (*Cluster, error) {
	if log == nil {
		log = slog.Default()
	}
	if cfg.Role == "" {
		cfg.Role = RoleStandalone
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if cfg.NodeName == "" {
		host, _ := os.Hostname()
		if host == "" {
			host = "node"
		}
		cfg.NodeName = host
	}
	cfg.MasterURL = strings.TrimRight(strings.TrimSpace(cfg.MasterURL), "/")
	cfg.Advertise = strings.TrimRight(strings.TrimSpace(cfg.Advertise), "/")

	c := &Cluster{
		cfg: cfg, st: st, log: log, snapshotDir: dataDir,
		// 타임아웃을 넉넉히 두는 이유: 스냅샷 전송은 DB 크기에 비례한다.
		client:     &http.Client{Timeout: 10 * time.Minute},
		kick:       make(chan struct{}, 1),
		appliedSig: make(chan struct{}),
	}
	if cfg.Role == RoleStandalone {
		return c, nil
	}
	id, err := loadNodeID(dataDir)
	if err != nil {
		return nil, err
	}
	c.id = id
	return c, nil
}

// loadNodeID는 이 노드의 고정 ID를 읽거나 새로 만든다.
func loadNodeID(dataDir string) (string, error) {
	path := filepath.Join(dataDir, "node.id")
	raw, err := os.ReadFile(path)
	if err == nil {
		if id := strings.TrimSpace(string(raw)); id != "" {
			return id, nil
		}
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("노드 ID를 읽지 못했습니다: %w", err)
	}
	id, err := crypto.RandomToken(12)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(id), 0o600); err != nil {
		return "", fmt.Errorf("노드 ID를 저장하지 못했습니다: %w", err)
	}
	return id, nil
}

func (c *Cluster) Config() Config { return c.cfg }
func (c *Cluster) NodeID() string { return c.id }
func (c *Cluster) Name() string   { return c.cfg.NodeName }
func (c *Cluster) Role() string   { return c.cfg.Role }

func (c *Cluster) Enabled() bool   { return c.cfg.Role != RoleStandalone }
func (c *Cluster) IsMaster() bool  { return c.cfg.Role == RoleMaster }
func (c *Cluster) IsReplica() bool { return c.cfg.Role == RoleReplica }

// SetHostSnapshot은 하트비트에 실을 호스트 상태 제공자를 붙인다.
func (c *Cluster) SetHostSnapshot(f func() any) { c.hostSnap = f }

// SecretOK는 들어온 비밀이 맞는지 본다(시간 일정 비교).
func (c *Cluster) SecretOK(given string) bool {
	want := c.cfg.Secret
	if want == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(given), []byte(want)) == 1
}

// Status는 지금 상태를 돌려준다.
func (c *Cluster) Status(ctx context.Context) Status {
	c.mu.RLock()
	st := Status{
		Enabled: c.Enabled(), Role: c.cfg.Role, NodeID: c.id, NodeName: c.cfg.NodeName,
		Address: c.cfg.Advertise, MasterURL: c.cfg.MasterURL,
		AppliedSeq: c.applied, MasterSeq: c.masterSeq, LastError: c.lastErr,
		Connected: c.connected,
	}
	if !c.lastSync.IsZero() {
		t := c.lastSync
		st.LastSyncAt = &t
	}
	c.mu.RUnlock()

	if c.IsMaster() {
		// 마스터는 자기 자신이 기준점이다. 지연은 0이고, 최신 지점은 로그의 끝이다.
		if _, max, err := c.st.ReplBounds(ctx); err == nil {
			st.AppliedSeq, st.MasterSeq = max, max
		}
		st.Connected = true
	}
	st.Lag = st.MasterSeq - st.AppliedSeq
	if st.Lag < 0 {
		st.Lag = 0
	}
	return st
}

func (c *Cluster) setErr(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err == nil {
		c.lastErr = ""
		c.connected = true
		return
	}
	c.lastErr = err.Error()
	c.connected = false
}

// setApplied는 적용 지점을 올리고, 기다리는 요청들을 깨운다.
func (c *Cluster) setApplied(seq int64) {
	c.mu.Lock()
	if seq > c.applied {
		c.applied = seq
	}
	sig := c.appliedSig
	c.appliedSig = make(chan struct{})
	c.mu.Unlock()
	close(sig)
}

// Applied는 이 노드가 적용한 마지막 복제 지점이다.
func (c *Cluster) Applied() int64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.applied
}

// Kick은 다음 동기화를 즉시 돌린다.
//
// 필요한 이유: 리플리카에서 무언가를 바꾸면 그 변경은 마스터에 적용된다. 주기를
// 기다렸다가 받아오면 방금 저장한 사람이 옛 화면을 본다 — "저장했는데 안 바뀌었다".
func (c *Cluster) Kick() {
	select {
	case c.kick <- struct{}{}:
	default:
	}
}

// WaitApplied는 지정한 복제 지점까지 따라잡을 때까지 기다린다.
//
// 타임아웃이 지나면 false를 돌려주되 요청을 실패시키지는 않는다(호출자 판단). 늦게
// 반영되는 것과 실패는 다르다 — 변경은 이미 마스터에 저장되어 있다.
func (c *Cluster) WaitApplied(ctx context.Context, seq int64, timeout time.Duration) bool {
	if seq <= 0 || !c.IsReplica() {
		return true
	}
	deadline := time.After(timeout)
	for {
		c.mu.RLock()
		applied := c.applied
		sig := c.appliedSig
		c.mu.RUnlock()
		if applied >= seq {
			return true
		}
		c.Kick()
		select {
		case <-sig:
		case <-deadline:
			return false
		case <-ctx.Done():
			return false
		}
	}
}
