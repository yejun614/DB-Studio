// Package cloud는 DB Studio 자신의 데이터(메타 DB)를 클라우드에 잇는다.
//
// 둘이다.
//   - Cloudflare D1 동기화: 메타 DB의 변경을 모아 두었다가 주기적으로 D1에 반영한다. 새 환경에서
//     같은 D1을 연결하면 그 내용을 받아 이어서 운영한다.
//   - R2·S3 백업: 메타 DB를 통째로 떠서(암호화해) 오브젝트 스토리지에 올리고, 고른 백업으로 되돌린다.
//
// 로컬 SQLite는 그대로 진실의 원본이다. 화면·API는 여전히 로컬에 읽고 쓰고, 클라우드는 그 뒤를
// 따라간다. 클라우드를 요청 경로에 끼우면 Cloudflare 가 느린 순간 앱 전체가 느려진다.
package cloud

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"dbstudio/internal/applog"
	"dbstudio/internal/buildinfo"
	"dbstudio/internal/store"
)

// Options는 서비스가 바깥에서 받아야 하는 것이다.
type Options struct {
	// DataDir 아래 cloud/에 임시 파일과 덮어쓰기 전 안전 사본을 둔다.
	DataDir string
	// NodeName은 D1의 주인 이름으로 보인다("이 D1은 지금 ○○가 쓰고 있습니다").
	NodeName string
	// IsMaster·IsReplica는 클러스터 역할이다. 마스터에서는 복제 트리거를 클러스터가 관리한다.
	IsMaster  func() bool
	IsReplica func() bool
	// OnReplaced는 메타 DB를 통째로 바꾼 뒤 불린다. 메모리에 문서를 들고 있는 것(ERD 방)을
	// 닫게 한다 — 남겨 두면 다음 압축 때 옛 문서가 방금 받아 온 것을 덮어쓴다.
	OnReplaced func(reason string)
	Log        *slog.Logger
}

// Service는 D1 동기화와 오브젝트 스토리지 백업을 맡는다.
type Service struct {
	st  *store.Store
	opt Options
	log *slog.Logger

	// opMu는 데이터를 움직이는 일을 한 줄로 세운다. 정기 동기화가 가져오기(pull) 도중에
	// 끼어들면 반쯤 바뀐 메타 DB를 D1에 올린다.
	opMu sync.Mutex

	mu      sync.Mutex
	d1      D1Status
	obj     ObjectStatus
	job     *Job
	lastJob *Job
	// triggersOn은 단일 서버에서 이 서비스가 복제 트리거를 달아 두었는지다.
	triggersOn  bool
	lastD1At    time.Time
	lastSchedAt time.Time

	kick chan struct{}
}

// New는 서비스를 만든다. Run을 불러야 정기 작업이 돈다.
func New(st *store.Store, opt Options) *Service {
	if opt.Log == nil {
		opt.Log = slog.Default()
	}
	if opt.IsMaster == nil {
		opt.IsMaster = func() bool { return false }
	}
	if opt.IsReplica == nil {
		opt.IsReplica = func() bool { return false }
	}
	return &Service{st: st, opt: opt, log: opt.Log, kick: make(chan struct{}, 1)}
}

// D1Status는 화면에 보일 D1 동기화 상태다.
type D1Status struct {
	// State: off | ok | pending | setup | conflict | error | replica
	State   string `json:"state"`
	Message string `json:"message,omitempty"`

	PushedSeq  int64       `json:"pushedSeq"`
	Pending    int64       `json:"pending"`
	LastSyncAt *time.Time  `json:"lastSyncAt,omitempty"`
	LastSent   int         `json:"lastSent"`
	LastError  string      `json:"lastError,omitempty"`
	ErrorAt    *time.Time  `json:"errorAt,omitempty"`
	NextSyncAt *time.Time  `json:"nextSyncAt,omitempty"`
	Remote     *RemoteInfo `json:"remote,omitempty"`
}

// ObjectStatus는 화면에 보일 백업 상태다.
type ObjectStatus struct {
	LastBackupAt *time.Time `json:"lastBackupAt,omitempty"`
	LastKey      string     `json:"lastKey,omitempty"`
	LastError    string     `json:"lastError,omitempty"`
	ErrorAt      *time.Time `json:"errorAt,omitempty"`
	NextBackupAt *time.Time `json:"nextBackupAt,omitempty"`
}

// Job은 오래 걸리는 일 하나(전체 올리기·가져오기·백업·복구)다.
//
// 요청 하나로 끝내지 않고 배경에서 돌리는 이유: 메타 DB가 크면 몇 분이 걸리는데, HTTP 요청은
// 그 전에 끊긴다(쓰기 제한 60초). 끊긴 뒤에도 일은 계속되는데 화면은 실패로 보여 준다 —
// 그 상태에서 다시 누르면 같은 일이 두 번 돈다.
type Job struct {
	Kind       string     `json:"kind"`
	Label      string     `json:"label"`
	Stage      string     `json:"stage"`
	Done       int64      `json:"done"`
	StartedAt  time.Time  `json:"startedAt"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
	Error      string     `json:"error,omitempty"`
	Result     string     `json:"result,omitempty"`
	// Relogin이면 메타 DB를 통째로 바꿨다는 뜻이다. 화면은 다시 로그인하게 한다.
	Relogin bool `json:"relogin,omitempty"`
}

// StatusView는 상태 API가 돌려주는 것이다.
type StatusView struct {
	Role    string       `json:"role"`
	D1      D1Status     `json:"d1"`
	Object  ObjectStatus `json:"object"`
	Job     *Job         `json:"job,omitempty"`
	LastJob *Job         `json:"lastJob,omitempty"`
}

// ErrBusy는 다른 작업이 돌고 있다는 뜻이다.
var ErrBusy = errors.New("다른 클라우드 작업이 진행 중입니다. 끝난 뒤 다시 시도하세요")

// ErrReplica는 리플리카에서 부를 수 없다는 뜻이다.
var ErrReplica = errors.New("리플리카에서는 클라우드 작업을 하지 않습니다 — 마스터에서 하세요")

func (s *Service) role() string {
	switch {
	case s.opt.IsReplica():
		return "replica"
	case s.opt.IsMaster():
		return "master"
	}
	return "standalone"
}

// Status는 지금 상태다.
func (s *Service) Status(ctx context.Context) StatusView {
	s.mu.Lock()
	v := StatusView{Role: s.role(), D1: s.d1, Object: s.obj}
	if s.job != nil {
		j := *s.job
		v.Job = &j
	}
	if s.lastJob != nil {
		j := *s.lastJob
		v.LastJob = &j
	}
	s.mu.Unlock()

	if v.Role == "replica" {
		v.D1.State = "replica"
		return v
	}
	// 보낼 변경 수는 매번 계산한다. 이 값이 멈춰 있으면 "동기화가 도는가"를 화면이 거짓으로 답한다.
	if pushed, ok, err := s.st.LocalStateInt(ctx, store.StateD1PushedSeq); err == nil && ok {
		if _, hi, err := s.st.ReplBounds(ctx); err == nil && hi > pushed {
			v.D1.Pending = hi - pushed
		}
		v.D1.PushedSeq = pushed
	}
	if t, err := s.st.LocalStateTime(ctx, store.StateObjLastBackup); err == nil && !t.IsZero() {
		v.Object.LastBackupAt = &t
	}
	if cfg, err := s.st.ObjectSettings(ctx); err == nil && cfg.ScheduleEnabled && cfg.Ready() {
		if next := s.nextBackupAt(ctx, cfg); !next.IsZero() {
			v.Object.NextBackupAt = &next
		}
	}
	return v
}

// Kick은 다음 동기화를 기다리지 않고 곧바로 돌게 한다.
func (s *Service) Kick() {
	s.mu.Lock()
	s.lastD1At = time.Time{}
	s.mu.Unlock()
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// startJob은 배경 작업을 시작한다. 이미 하나가 돌고 있으면 ErrBusy다.
func (s *Service) startJob(kind, label string, fn func(ctx context.Context, j *Job) (string, error)) (*Job, error) {
	if s.opt.IsReplica() {
		return nil, ErrReplica
	}
	if !s.opMu.TryLock() {
		return nil, ErrBusy
	}
	j := &Job{Kind: kind, Label: label, Stage: "시작", StartedAt: time.Now()}
	s.mu.Lock()
	s.job = j
	s.mu.Unlock()

	go func() {
		defer applog.Recover("cloud." + kind)
		defer s.opMu.Unlock()
		// 요청의 컨텍스트와 떼어 둔다. 화면을 닫아도 작업은 끝까지 가야 한다 —
		// 가져오기가 절반에서 멈추는 것은 시작하지 않은 것보다 나쁘다.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
		defer cancel()
		result, err := fn(ctx, j)

		now := time.Now()
		s.mu.Lock()
		j.FinishedAt = &now
		j.Stage = "끝"
		if err != nil {
			j.Error = err.Error()
			s.log.Warn("클라우드 작업 실패", "kind", kind, "err", err)
		} else {
			j.Result = result
			s.log.Info("클라우드 작업 완료", "kind", kind, "result", result)
		}
		s.job = nil
		s.lastJob = j
		s.mu.Unlock()
	}()
	return j, nil
}

// progress는 작업의 단계와 진행 수를 바꾼다.
func (s *Service) progress(j *Job, stage string, done int64) {
	s.mu.Lock()
	j.Stage = stage
	j.Done = done
	s.mu.Unlock()
}

// Run은 정기 작업(D1 동기화, 정기 백업)을 돈다. ctx가 끝나면 반환한다.
//
// 리플리카에서는 돌지 않는다. 리플리카의 메타 DB는 마스터의 사본이고, 같은 D1·같은 버킷에
// 노드마다 올리면 같은 일이 노드 수만큼 일어난다(매크로 자동 실행을 마스터에서만 돌리는 것과 같다).
func (s *Service) Run(ctx context.Context) {
	if s.opt.IsReplica() {
		s.log.Info("리플리카에서는 클라우드 동기화·백업을 하지 않습니다 (마스터가 합니다)")
		return
	}
	s.startup(ctx)

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-s.kick:
		}
		s.tickD1(ctx)
		s.tickBackup(ctx)
	}
}

// ---------- 임시 파일 ----------

func (s *Service) workDir(sub string) (string, error) {
	dir := filepath.Join(s.opt.DataDir, "cloud", sub)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

func (s *Service) tempPath(name string) (string, error) {
	dir, err := s.workDir("tmp")
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, fmt.Sprintf("%s-%d", name, time.Now().UnixNano())), nil
}

// safetyCopy는 메타 DB를 통째로 바꾸기 전에 지금 상태를 떠 둔다.
//
// 가져오기·복구는 이 서버의 데이터를 덮어쓴다. 고른 것이 틀렸다는 사실은 대개 덮어쓴 뒤에야
// 안다. 그때 돌아갈 곳이 있어야 한다. 종류마다 최근 3개만 남긴다 — 메타 DB가 크면 디스크를 먹는다.
func (s *Service) safetyCopy(ctx context.Context, kind string) (string, error) {
	dir, err := s.workDir("safety")
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, fmt.Sprintf("before-%s-%s.db", kind, time.Now().UTC().Format("20060102T150405Z")))
	if err := s.st.SnapshotTo(ctx, path); err != nil {
		return "", err
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "before-"+kind+"-*.db"))
	sort.Strings(matches)
	for len(matches) > 3 {
		_ = os.Remove(matches[0])
		matches = matches[1:]
	}
	return path, nil
}

// keepCloudSettings는 메타 DB를 통째로 바꾸는 동안 클라우드 설정을 지킨다.
//
// 설정도 app_settings에 있어서 함께 덮인다. 받아 온 쪽의 설정이 옛 토큰이거나 비어 있으면,
// 방금 연결해 쓰던 D1·버킷을 그 순간 잃는다 — 복구를 마치자마자 다음 백업이 실패하는 식이다.
// 지금 쓰고 있는 연결이 정답이다.
func (s *Service) keepCloudSettings(ctx context.Context) (func(context.Context) error, error) {
	d1, err := s.st.D1Settings(ctx)
	if err != nil {
		return nil, err
	}
	obj, err := s.st.ObjectSettings(ctx)
	if err != nil {
		return nil, err
	}
	hasD1 := d1.UpdatedAt != nil
	hasObj := obj.UpdatedAt != nil
	return func(ctx context.Context) error {
		if hasD1 {
			if err := s.st.SaveD1Settings(ctx, *d1, d1.UpdatedBy); err != nil {
				return err
			}
		}
		if hasObj {
			if err := s.st.SaveObjectSettings(ctx, *obj, obj.UpdatedBy); err != nil {
				return err
			}
		}
		return nil
	}, nil
}

// afterReplace는 메타 DB를 통째로 바꾼 뒤 할 일이다.
func (s *Service) afterReplace(reason string) {
	s.st.InvalidateCaches()
	if s.opt.OnReplaced != nil {
		s.opt.OnReplaced(reason)
	}
}

func appVersion() string { return buildinfo.Get().Version }

func firstLine(err error) string {
	msg := err.Error()
	if i := strings.IndexByte(msg, '\n'); i >= 0 {
		msg = msg[:i]
	}
	return msg
}
