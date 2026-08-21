// Package backup은 논리 덤프를 만들고 되돌린다.
//
// **논리 덤프**다. mysqldump가 만드는 것과 같은 성격의, 사람이 읽을 수 있는 텍스트
// 파일이며 그 안에는 구조를 만드는 문장과 값을 넣는 문장이 들어 있다.
//
// 물리 백업(파일 수준 스냅숏, PITR)은 여기서 하지 않는다. 그것은 DB마다 도구와 절차가
// 다르고, 잘못 만든 물리 백업은 없는 것보다 위험하다. 이 앱은 그 자리에 이미
// `-backup-cmd` 훅을 두었다 — 조직이 쓰는 도구를 그대로 부르는 것이 정직하다.
// 여기서 만드는 것은 "지금 이 구조와 값을 다른 곳에 다시 세울 수 있는 파일"이다.
//
// 작업은 비동기다. 덤프는 몇 분씩 걸릴 수 있고, HTTP 요청 하나를 그동안 붙잡고 있으면
// 프록시 타임아웃에 걸린다. 진행 상황은 기록 행의 숫자로 갱신되고 화면이 그것을 읽는다
// (매크로처럼 줄 단위 로그를 스트리밍하지 않는 이유: 덤프가 만드는 정보는
// "지금 어느 테이블의 몇 번째 행"이라는 덮어쓰면 되는 값 하나뿐이다).
package backup

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"dbstudio/internal/dbx"
	"dbstudio/internal/model"
	"dbstudio/internal/store"
)

// Scope는 덤프에 무엇을 담을지다.
const (
	ScopeFull   = "full"   // 구조 + 데이터
	ScopeSchema = "schema" // 구조만
	ScopeData   = "data"   // 데이터만 (구조는 이미 있다고 전제)
)

func ValidScope(s string) bool {
	switch s {
	case ScopeFull, ScopeSchema, ScopeData:
		return true
	}
	return false
}

// Format은 파일 내용의 모양이다. 복구기가 이 값으로 읽는 방법을 정한다.
const (
	FormatSQL   = "sql"   // 관계형: 세미콜론으로 끝나는 SQL 문장
	FormatJSONL = "jsonl" // MongoDB: 줄 단위 확장 JSON
	FormatRedis = "redis" // Redis: 줄 단위 명령
)

// FormatFor는 DB 종류에 맞는 덤프 형식을 반환한다.
func FormatFor(kind model.DBKind) string {
	switch kind {
	case model.KindMongoDB:
		return FormatJSONL
	case model.KindRedis:
		return FormatRedis
	default:
		return FormatSQL
	}
}

// Config는 백업 동작의 한계값이다.
type Config struct {
	Dir        string
	MaxBytes   int64
	Retention  time.Duration
	RowBatch   int
	StmtChunk  int
	ScopeLimit time.Duration // 작업 하나의 시간 상한
}

// Options는 덤프를 만들 때 고르는 것들이다.
type Options struct {
	Scope string `json:"scope"`
	// Tables가 비어 있으면 전부. 채워져 있으면 그 목록만("스키마.이름" 또는 "이름").
	Tables []string `json:"tables,omitempty"`
	// DropIfExists면 각 테이블 생성 앞에 DROP 문을 넣는다.
	//
	// 기본값은 꺼짐이다. 켜진 덤프는 **복구할 때 대상의 기존 테이블을 지운다** —
	// 그것이 필요한 경우가 많지만, 그 사실이 파일 안에 들어가므로 만들 때 의식적으로
	// 골라야 한다. 나중에 "이 백업이 뭘 지우는지" 모르는 상태가 되면 안 된다.
	DropIfExists bool   `json:"dropIfExists"`
	Note         string `json:"note,omitempty"`
}

// Service는 백업·복구 작업을 관리한다.
type Service struct {
	st  *store.Store
	cfg Config
	log *slog.Logger

	mu      sync.Mutex
	running map[string]context.CancelFunc
}

func New(st *store.Store, cfg Config, log *slog.Logger) *Service {
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = 2048 << 20
	}
	if cfg.RowBatch <= 0 {
		cfg.RowBatch = 500
	}
	if cfg.StmtChunk <= 0 {
		cfg.StmtChunk = 500
	}
	if cfg.ScopeLimit <= 0 {
		cfg.ScopeLimit = 2 * time.Hour
	}
	if log == nil {
		log = slog.Default()
	}
	return &Service{st: st, cfg: cfg, log: log, running: map[string]context.CancelFunc{}}
}

func (s *Service) Config() Config { return s.cfg }

// EnsureDir는 백업 디렉터리를 만든다. 부팅 시 한 번 부른다 —
// 첫 백업을 누른 뒤에야 "디렉터리를 만들 수 없다"를 알게 되면 늦다.
func (s *Service) EnsureDir() error {
	if err := os.MkdirAll(s.cfg.Dir, 0o700); err != nil {
		return fmt.Errorf("백업 디렉터리를 만들 수 없습니다(%s): %w", s.cfg.Dir, err)
	}
	return nil
}

// FilePath는 백업 파일의 전체 경로다.
//
// 파일 이름은 기록에 저장된 값을 그대로 쓰되 경로 구분자가 섞이지 않았는지 확인한다.
// 이름은 앱이 만든 것이지만(UUID 기반), 경로를 조립하는 곳에서는 언제나 확인한다 —
// 그 규칙이 지켜지지 않는 날이 오면 그때가 디렉터리 탈출이 되는 날이다.
func (s *Service) FilePath(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("파일 이름이 없습니다")
	}
	if name != filepath.Base(name) || strings.ContainsAny(name, `/\`) {
		return "", fmt.Errorf("잘못된 파일 이름입니다: %s", name)
	}
	return filepath.Join(s.cfg.Dir, name), nil
}

// Cancel은 진행 중인 작업을 중단한다.
func (s *Service) Cancel(jobID string) error {
	s.mu.Lock()
	cancel, ok := s.running[jobID]
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("진행 중인 작업이 아닙니다")
	}
	cancel()
	return nil
}

func (s *Service) IsRunning(jobID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.running[jobID]
	return ok
}

func (s *Service) track(jobID string, cancel context.CancelFunc) {
	s.mu.Lock()
	s.running[jobID] = cancel
	s.mu.Unlock()
}

func (s *Service) untrack(jobID string) {
	s.mu.Lock()
	delete(s.running, jobID)
	s.mu.Unlock()
}

// Target은 작업 대상 DB다.
type Target struct {
	Conn   *model.Connection
	Secret *model.Secret
}

func (t Target) dbx() dbx.Target { return dbx.Target{Conn: t.Conn, Secret: t.Secret} }

// Purge는 보존 기간이 지난 백업을 지운다.
//
// 파일과 기록을 함께 지운다. 기록만 지우면 디스크에 고아 파일이 남고, 파일만 지우면
// 목록에 복구할 수 없는 항목이 남는다.
func (s *Service) Purge(ctx context.Context) (int, error) {
	if s.cfg.Retention <= 0 {
		return 0, nil
	}
	expired, err := s.st.ExpiredBackups(ctx, time.Now().Add(-s.cfg.Retention))
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, b := range expired {
		if b.FileName != "" {
			if path, perr := s.FilePath(b.FileName); perr == nil {
				if rerr := os.Remove(path); rerr != nil && !os.IsNotExist(rerr) {
					s.log.Warn("만료된 백업 파일을 지우지 못했습니다", "file", path, "err", rerr)
					continue
				}
			}
		}
		if derr := s.st.DeleteBackup(ctx, b.ID); derr != nil {
			s.log.Warn("만료된 백업 기록을 지우지 못했습니다", "id", b.ID, "err", derr)
			continue
		}
		removed++
	}
	return removed, nil
}

// Remove는 백업 하나를 파일과 함께 지운다.
func (s *Service) Remove(ctx context.Context, b *store.Backup) error {
	if b.FileName != "" {
		path, err := s.FilePath(b.FileName)
		if err == nil {
			if rerr := os.Remove(path); rerr != nil && !os.IsNotExist(rerr) {
				return fmt.Errorf("백업 파일을 지우지 못했습니다: %w", rerr)
			}
		}
	}
	return s.st.DeleteBackup(ctx, b.ID)
}

// WaitFor는 작업이 끝날 때까지 기다린다.
//
// 진행 상황을 이벤트로 밀어내지 않고 폴링으로 기다리는 이유: 기다리는 쪽은 매크로
// 노드 하나뿐이고, 그것이 알고 싶은 것은 "끝났는가"라는 한 가지다. 그 하나를 위해
// 알림 배관을 놓는 것보다 0.5초마다 행 하나를 읽는 편이 단순하고 틀릴 여지가 적다.
func (s *Service) WaitFor(ctx context.Context, id string) (*store.Backup, error) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		b, err := s.st.GetBackup(ctx, id)
		if err != nil {
			return nil, err
		}
		if b.Status != "running" {
			return b, nil
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			// 기다리던 쪽이 취소되면 백업도 멈춘다. 아무도 기다리지 않는 백업을
			// 계속 돌리면 그 결과를 볼 사람이 없다.
			_ = s.Cancel(id)
			return nil, ctx.Err()
		}
	}
}

// FileExists는 기록에 대응하는 파일이 실제로 있는지 확인한다.
func (s *Service) FileExists(b *store.Backup) bool {
	if b.FileName == "" {
		return false
	}
	path, err := s.FilePath(b.FileName)
	if err != nil {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// statusFor는 작업 종료 상태를 정한다.
// 취소는 실패가 아니다 — 사용자가 의도한 결과이므로 실패 목록에 섞이면 안 된다.
func statusFor(err error, canceled bool) (string, string) {
	switch {
	case canceled || (err != nil && strings.Contains(err.Error(), context.Canceled.Error())):
		return "canceled", "사용자가 작업을 취소했습니다"
	case err != nil:
		return "failed", err.Error()
	default:
		return "success", ""
	}
}
