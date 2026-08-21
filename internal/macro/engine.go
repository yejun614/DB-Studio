package macro

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"dbstudio/internal/auth"
	"dbstudio/internal/backup"
	"dbstudio/internal/model"
	"dbstudio/internal/store"
)

// 실행 엔진.
//
// 실행에 대한 세 가지 약속이 있고, 이 파일은 그것을 지키기 위해 존재한다.
//
//  1. **저장된 것만 실행한다.** Run은 그래프를 인자로 받지 않고 매크로 ID와 버전을
//     받는다. 편집 중인 내용을 그대로 실행할 방법이 없어야 "무엇이 실행됐는지"가
//     언제나 버전 하나로 특정된다.
//  2. **실행자의 권한으로 실행한다.** 매크로를 만든 사람의 권한이 아니다. 그러지
//     않으면 권한 높은 사람이 만든 매크로가 권한 상승 통로가 된다.
//  3. **끝은 셋 중 하나다.** 성공·실패·취소. 어느 쪽이든 반드시 기록되고,
//     그래서 '실행 중'으로 영원히 남는 기록이 없다(앱이 죽는 경우는 부팅 시 정리한다).

// Config는 실행 한도와 기능 스위치다.
type Config struct {
	AllowShell   bool
	ShellTimeout time.Duration
	RunTimeout   time.Duration
	LuaTimeout   time.Duration
	HTTP         HTTPConfig
}

// DriftChecker는 드리프트 확인 노드가 쓰는 최소 인터페이스다.
//
// monitor.Manager를 그대로 받지 않는 이유: 매크로 엔진이 모니터링 전체에 의존하면
// 모니터링을 끈 배포에서 매크로도 못 만들게 되고, 테스트에서 이 노드 하나 때문에
// 폴러를 세워야 한다. 필요한 것은 함수 하나다.
type DriftChecker interface {
	CheckDriftByID(ctx context.Context, connectionID string) (*store.SchemaSnapshot, bool, error)
}

// Backuper는 백업 노드가 쓰는 최소 인터페이스다.
// DriftChecker와 같은 이유로 좁게 잡는다 — 백업 서비스 전체에 매이지 않는다.
type Backuper interface {
	StartBackup(ctx context.Context, p backup.StartBackupParams) (string, error)
	WaitFor(ctx context.Context, id string) (*store.Backup, error)
}

// Engine은 매크로를 실행한다.
type Engine struct {
	st      *store.Store
	authz   *auth.Authorizer
	cfg     Config
	log     *slog.Logger
	drift   DriftChecker
	backups Backuper
	// scheduler는 자동 실행 담당이다. 처음 요청될 때 만든다 —
	// 트리거를 쓰지 않는 배포에서 고루틴이 도는 것을 피한다.
	scheduler *Scheduler

	mu      sync.Mutex
	running map[string]*runHandle
	// subs는 실행별 로그 구독자다. SSE 스트림이 여기에 붙는다.
	subs map[string][]chan store.RunLogEntry
}

type runHandle struct {
	cancel context.CancelFunc
	// canceled는 취소가 사용자 요청이었는지 구분한다. 타임아웃과 사용자 취소는
	// 둘 다 컨텍스트 취소로 나타나지만 기록에 남길 이름이 다르다.
	canceled bool
	actorID  string
	macroID  string
	started  time.Time
}

func New(st *store.Store, authz *auth.Authorizer, cfg Config, drift DriftChecker,
	backups Backuper, log *slog.Logger) *Engine {
	if cfg.RunTimeout <= 0 {
		cfg.RunTimeout = 15 * time.Minute
	}
	if cfg.ShellTimeout <= 0 {
		cfg.ShellTimeout = 2 * time.Minute
	}
	if cfg.LuaTimeout <= 0 {
		cfg.LuaTimeout = time.Minute
	}
	if log == nil {
		log = slog.Default()
	}
	return &Engine{
		st: st, authz: authz, cfg: cfg, drift: drift, backups: backups, log: log,
		running: map[string]*runHandle{},
		subs:    map[string][]chan store.RunLogEntry{},
	}
}

func (e *Engine) Config() Config { return e.cfg }

// maxDepth는 매크로가 매크로를 호출하는 최대 깊이다.
// 순환 호출(A가 B를, B가 A를)은 검사로 잡기 어렵지만 깊이 제한으로는 반드시 멈춘다.
const maxDepth = 5

// maxSteps는 한 실행에서 처리할 수 있는 노드 실행 횟수다.
// 반복 노드가 있는 그래프는 정지 여부를 정적으로 알 수 없으므로 상한이 필요하다.
const maxSteps = 10_000

// RunRequest는 실행 요청이다.
type RunRequest struct {
	MacroID string
	// Version이 0이면 현재 버전을 쓴다.
	Version int
	Actor   *model.User
	ActorIP string
	Params  map[string]any
	// Trigger와 ParentRunID는 다른 매크로가 호출한 경우에 채워진다.
	Trigger     string
	ParentRunID string
	// TriggerContext는 자동 실행에서 넘어오는 정보다(어떤 트리거였는지, 어떤 이벤트였는지).
	//
	// 파라미터가 아니라 별도 필드인 이유: 파라미터는 매크로가 미리 선언한 것만 받는다
	// (normalizeParams가 모르는 키를 버린다). 이벤트 정보를 파라미터로 넘기면
	// 선언하지 않은 매크로에서는 조용히 사라진다. 변수 가방에 직접 넣어야 언제나 보인다.
	TriggerContext map[string]any
	// OnFinish는 실행이 끝난 뒤 결과와 함께 불린다.
	//
	// 필요한 이유: Start는 실행 ID만 주고 곧바로 반환하므로, 시작시킨 쪽은 결과를
	// 알 방법이 없다. 사람이 눌렀다면 화면이 로그 스트림을 보고 있으니 문제가 없지만,
	// 자동 실행은 아무도 보고 있지 않다 — 트리거 목록의 "마지막 결과"가 영원히
	// "시작됨"에 머물고, 매번 실패하는 트리거의 연속 실패 카운트도 오르지 않아
	// 자동 비활성화가 동작하지 않는다.
	//
	// 실행 고루틴에서 불리므로 오래 걸리는 일을 하면 안 된다.
	OnFinish func(runID, status, errMsg string)
	depth    int
}

// Start는 실행을 시작하고 실행 ID를 즉시 반환한다.
//
// 동기 실행이 아닌 이유: 매크로는 몇 분씩 돌 수 있고, HTTP 요청 하나를 그동안
// 붙잡고 있으면 브라우저·프록시 타임아웃에 걸린다. 진행 상황은 로그 스트림으로 본다.
func (e *Engine) Start(ctx context.Context, req RunRequest) (string, error) {
	m, version, graph, err := e.load(ctx, req.MacroID, req.Version, req.Actor)
	if err != nil {
		return "", err
	}

	params, err := normalizeParams(graph, req.Params)
	if err != nil {
		return "", err
	}

	runID, err := e.st.CreateMacroRun(ctx, store.CreateRunParams{
		MacroID: m.ID, MacroName: m.Name, Version: version,
		ActorID: req.Actor.ID, ActorName: req.Actor.Username, ActorIP: req.ActorIP,
		Params: params, Trigger: req.Trigger, ParentRunID: req.ParentRunID,
	})
	if err != nil {
		return "", err
	}

	// 요청 컨텍스트에서 파생하지 않는다. HTTP 응답이 끝나면 그 컨텍스트는 취소되고,
	// 그러면 실행이 시작하자마자 죽는다.
	runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), e.cfg.RunTimeout)

	e.mu.Lock()
	e.running[runID] = &runHandle{
		cancel: cancel, actorID: req.Actor.ID, macroID: m.ID, started: time.Now(),
	}
	e.mu.Unlock()

	go func() {
		defer cancel()
		e.execute(runCtx, runID, m, version, graph, params, req)
	}()
	return runID, nil
}

// RunNested는 매크로 안에서 다른 매크로를 실행하고 끝날 때까지 기다린다.
// 호출한 노드가 결과를 받아야 하므로 동기다.
func (e *Engine) RunNested(ctx context.Context, req RunRequest) (*store.MacroRun, error) {
	if req.depth >= maxDepth {
		return nil, fmt.Errorf("매크로 호출이 %d단계를 넘었습니다(순환 호출일 수 있습니다)", maxDepth)
	}
	m, version, graph, err := e.load(ctx, req.MacroID, req.Version, req.Actor)
	if err != nil {
		return nil, err
	}
	params, err := normalizeParams(graph, req.Params)
	if err != nil {
		return nil, err
	}
	runID, err := e.st.CreateMacroRun(ctx, store.CreateRunParams{
		MacroID: m.ID, MacroName: m.Name, Version: version,
		ActorID: req.Actor.ID, ActorName: req.Actor.Username, ActorIP: req.ActorIP,
		Params: params, Trigger: "macro", ParentRunID: req.ParentRunID,
	})
	if err != nil {
		return nil, err
	}
	e.execute(ctx, runID, m, version, graph, params, req)
	return e.st.GetMacroRun(ctx, runID)
}

// load는 실행할 매크로와 그래프를 읽는다.
//
// 여기서 접근 권한을 확인하는 것이 중요하다. 실행 경로는 셋인데(화면·AI 툴·자동 실행)
// 그중 자동 실행과 매크로 안의 "다른 매크로 호출" 노드는 HTTP 핸들러를 거치지 않는다.
// 이 확인이 없으면 공개 매크로 하나가 남의 비공개 매크로를 부르는 통로가 되고,
// 소유자가 매크로를 비공개로 되돌려도 남이 만들어 둔 트리거는 계속 돈다.
func (e *Engine) load(ctx context.Context, macroID string, version int, actor *model.User) (*store.Macro, int, *Graph, error) {
	m, err := e.st.GetMacro(ctx, macroID, store.MacroViewer{User: actor})
	if err != nil {
		return nil, 0, nil, err
	}
	if !m.Access.CanRun() {
		return nil, 0, nil, fmt.Errorf("매크로 %q 에 접근할 권한이 없습니다", m.Name)
	}
	v, err := e.st.GetMacroVersion(ctx, macroID, version)
	if err != nil {
		return nil, 0, nil, err
	}
	g, err := ParseGraph(v.Graph)
	if err != nil {
		return nil, 0, nil, err
	}
	return m, v.Version, g, nil
}

// Cancel은 실행 중인 매크로를 중단한다.
func (e *Engine) Cancel(runID string, actor *model.User) error {
	e.mu.Lock()
	h, ok := e.running[runID]
	if ok {
		h.canceled = true
	}
	e.mu.Unlock()
	if !ok {
		return fmt.Errorf("실행 중이 아닙니다")
	}
	h.cancel()
	return nil
}

// IsRunning은 실행 중인지 반환한다.
func (e *Engine) IsRunning(runID string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	_, ok := e.running[runID]
	return ok
}

// Subscribe는 실행 로그 스트림을 구독한다. 반환된 함수로 해제한다.
func (e *Engine) Subscribe(runID string) (<-chan store.RunLogEntry, func()) {
	// 버퍼를 넉넉히 잡는다. 느린 구독자 때문에 실행이 멈추면 안 되고,
	// 그래도 넘치면 그 줄은 버린다(로그는 DB에 이미 남아 있으므로 복구 가능하다).
	ch := make(chan store.RunLogEntry, 256)
	e.mu.Lock()
	e.subs[runID] = append(e.subs[runID], ch)
	e.mu.Unlock()

	return ch, func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		list := e.subs[runID]
		for i, c := range list {
			if c == ch {
				e.subs[runID] = append(list[:i], list[i+1:]...)
				close(c)
				break
			}
		}
		if len(e.subs[runID]) == 0 {
			delete(e.subs, runID)
		}
	}
}

func (e *Engine) publish(runID string, entry store.RunLogEntry) {
	e.mu.Lock()
	subs := append([]chan store.RunLogEntry(nil), e.subs[runID]...)
	e.mu.Unlock()
	for _, ch := range subs {
		select {
		case ch <- entry:
		default:
			// 구독자가 밀렸다. 실행을 붙잡는 것보다 한 줄을 흘리는 편이 낫다.
		}
	}
}

// runner는 실행 한 번의 상태다.
type runner struct {
	engine  *Engine
	ctx     context.Context
	runID   string
	macro   *store.Macro
	version int
	graph   *Graph
	actor   *model.User
	actorIP string
	depth   int

	vars  map[string]any
	seq   int
	steps int
	// currentNode는 Lua 호스트 함수가 로그를 남길 때 어느 노드인지 알기 위한 값이다.
	// 인자로 넘기면 호스트 함수 시그니처가 전부 지저분해지고, 실행은 한 고루틴에서
	// 순차적으로 일어나므로 이 필드로 충분하다.
	currentNode *Node
	// nodeDefs는 이 매크로에서 쓸 수 있는 사용자 노드 정의다(전역 + 이 매크로 전용).
	nodeDefs map[string]*store.NodeDef
	// stopped는 fail 노드나 취소로 실행이 끝났음을 나타낸다.
	stopped bool
}

func (e *Engine) execute(ctx context.Context, runID string, m *store.Macro, version int, g *Graph, params map[string]any, req RunRequest) {
	start := time.Now()
	defer func() {
		e.mu.Lock()
		delete(e.running, runID)
		e.mu.Unlock()
	}()

	r := &runner{
		engine: e, ctx: ctx, runID: runID, macro: m, version: version, graph: g,
		actor: req.Actor, actorIP: req.ActorIP, depth: req.depth,
		vars:     map[string]any{},
		nodeDefs: map[string]*store.NodeDef{},
	}
	maps.Copy(r.vars, params)
	r.vars["params"] = params
	// 자동 실행이면 무엇이 이 실행을 시작했는지 변수로 넣는다.
	// 이벤트 트리거의 매크로는 대부분 그 이벤트 내용에 따라 동작이 갈린다.
	if req.TriggerContext != nil {
		r.vars["trigger"] = req.TriggerContext
	}

	defs, err := e.st.ListNodeDefs(ctx, m.ID)
	if err != nil {
		r.log("error", nil, "노드 정의를 읽지 못했습니다: "+err.Error(), nil)
	}
	for _, d := range defs {
		r.nodeDefs[d.ID] = d
	}

	r.log("info", nil, fmt.Sprintf("매크로 실행 시작 — %s v%d (실행자 %s)", m.Name, version, req.Actor.Username),
		map[string]any{"params": params})

	// 패닉을 잡아 실패로 기록한다. Lua 호스트 함수나 드라이버에서 패닉이 나면
	// 서버 전체가 죽는 것이 아니라 이 실행만 실패해야 한다.
	var runErr error
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				runErr = fmt.Errorf("실행 중 내부 오류: %v", rec)
				e.log.Error("매크로 실행 중 패닉", "run", runID, "macro", m.Name, "panic", rec)
			}
		}()
		start := g.Start()
		if start == nil {
			runErr = errors.New("시작 노드가 없습니다")
			return
		}
		runErr = r.walk(start.ID)
	}()

	status := "success"
	errMsg := ""
	switch {
	case runErr != nil && r.wasCanceled():
		status = "canceled"
		errMsg = "사용자가 실행을 취소했습니다"
	case runErr != nil && errors.Is(ctx.Err(), context.DeadlineExceeded):
		status = "failed"
		errMsg = fmt.Sprintf("실행 시간 제한(%s)을 넘었습니다", e.cfg.RunTimeout)
	case runErr != nil:
		status = "failed"
		errMsg = runErr.Error()
	case r.wasCanceled():
		status = "canceled"
		errMsg = "사용자가 실행을 취소했습니다"
	}

	level := "info"
	if status == "failed" {
		level = "error"
	} else if status == "canceled" {
		level = "warn"
	}
	r.log(level, nil, fmt.Sprintf("실행 종료: %s (%d개 노드, %s)",
		statusLabel(status), r.steps, time.Since(start).Round(time.Millisecond)),
		map[string]any{"status": status, "error": errMsg})

	if err := e.st.FinishMacroRun(context.WithoutCancel(ctx), runID, status, errMsg,
		time.Since(start).Milliseconds(), r.steps); err != nil {
		e.log.Error("매크로 실행 기록 갱신 실패", "run", runID, "err", err)
	}
	if req.OnFinish != nil {
		req.OnFinish(runID, status, errMsg)
	}
}

func (r *runner) wasCanceled() bool {
	r.engine.mu.Lock()
	defer r.engine.mu.Unlock()
	h, ok := r.engine.running[r.runID]
	return ok && h.canceled
}

func statusLabel(status string) string {
	switch status {
	case "success":
		return "성공"
	case "failed":
		return "실패"
	case "canceled":
		return "취소"
	}
	return status
}

// walk는 노드 하나를 실행하고 이어지는 노드로 넘어간다.
func (r *runner) walk(nodeID string) error {
	current := nodeID
	for current != "" {
		if err := r.ctx.Err(); err != nil {
			return err
		}
		if r.stopped {
			return nil
		}
		r.steps++
		if r.steps > maxSteps {
			return fmt.Errorf("노드 실행 횟수가 상한(%d)을 넘었습니다. 무한 반복일 수 있습니다", maxSteps)
		}

		n := r.graph.Node(current)
		if n == nil {
			return fmt.Errorf("연결된 노드를 찾을 수 없습니다: %s", current)
		}
		if n.Disabled {
			r.log("debug", n, "비활성 노드를 건너뜁니다", nil)
			current = r.single(n, PortOut)
			continue
		}

		port, err := r.runNode(n)
		if err != nil {
			switch {
			case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
				// 취소·시간 초과는 노드의 실패가 아니다. "실패: context canceled"를
				// 남기면 사용자는 자기 매크로가 깨졌다고 읽는다. 마지막 줄이
				// 무슨 일이 있었는지 정확히 말하므로 여기서는 중단 사실만 남긴다.
				r.log("warn", n, "실행이 중단되어 이 노드에서 멈췄습니다", nil)
				return err
			case n.ContinueOnError:
				r.log("warn", n, "실패했지만 계속 진행합니다: "+err.Error(), nil)
				port = PortOut
			default:
				r.log("error", n, "실패: "+err.Error(), nil)
				return err
			}
		}
		if r.stopped {
			return nil
		}

		next := r.graph.Next(n.ID, port)
		switch len(next) {
		case 0:
			return nil
		case 1:
			current = next[0]
		default:
			// 갈래가 여럿이면 앞의 것들을 먼저 끝까지 실행하고 마지막 갈래로 이어간다.
			// 재귀 대신 마지막 하나만 반복문으로 넘기는 이유는 긴 직선 흐름에서
			// 스택이 깊어지지 않게 하기 위해서다.
			for _, id := range next[:len(next)-1] {
				if err := r.walk(id); err != nil {
					return err
				}
			}
			current = next[len(next)-1]
		}
	}
	return nil
}

func (r *runner) single(n *Node, port string) string {
	next := r.graph.Next(n.ID, port)
	if len(next) == 0 {
		return ""
	}
	return next[0]
}

// runNode는 노드 하나를 실행하고 다음 포트 이름을 반환한다.
func (r *runner) runNode(n *Node) (string, error) {
	exec, ok := executors[n.Type]
	if !ok {
		if n.Type == TypeCustom {
			return r.runCustom(n)
		}
		return "", fmt.Errorf("알 수 없는 노드 종류입니다: %s", n.Type)
	}
	result, port, err := exec(r, n)
	if err != nil {
		return "", err
	}
	if result != nil {
		r.vars[r.outputName(n)] = result
	}
	if port == "" {
		port = PortOut
	}
	return port, nil
}

func (r *runner) outputName(n *Node) string {
	if n.Output != "" {
		return n.Output
	}
	return n.ID
}

// log는 실행 로그 한 줄을 남기고 구독자에게 흘려보낸다.
func (r *runner) log(level string, n *Node, message string, detail map[string]any) {
	r.seq++
	entry := store.RunLogEntry{
		Seq: r.seq, At: time.Now().UTC(), Level: level, Message: message, Detail: detail,
	}
	if n != nil {
		entry.NodeID = n.ID
		entry.Node = nodeLabel(n)
	}
	// 저장은 취소된 컨텍스트에서도 되어야 한다. 취소 직후의 마지막 로그가
	// 가장 중요한 줄인 경우가 많다("사용자가 취소함").
	if err := r.engine.st.AppendRunLog(context.WithoutCancel(r.ctx), r.runID, entry.Seq,
		level, entry.NodeID, entry.Node, message, detail); err != nil {
		r.engine.log.Error("매크로 로그 기록 실패", "run", r.runID, "err", err)
	}
	r.engine.publish(r.runID, entry)
}

func nodeLabel(n *Node) string {
	if n.Label != "" {
		return n.Label
	}
	if spec, ok := specByType[n.Type]; ok {
		return spec.Label
	}
	return n.Type
}

// ---------- 파라미터 ----------

// normalizeParams는 사용자가 넘긴 실행 파라미터를 정의에 맞춰 정리한다.
func normalizeParams(g *Graph, in map[string]any) (map[string]any, error) {
	out := map[string]any{}
	for _, def := range g.Params {
		v, ok := in[def.Name]
		if !ok || v == nil || v == "" {
			if def.Default != nil {
				v = def.Default
			} else if def.Required {
				return nil, fmt.Errorf("필수 파라미터가 비어 있습니다: %s", paramLabel(def))
			} else {
				v = defaultForType(def.Type)
			}
		}
		switch def.Type {
		case "number":
			f, err := toFloat(v)
			if err != nil {
				return nil, fmt.Errorf("%s 은(는) 숫자여야 합니다", paramLabel(def))
			}
			out[def.Name] = f
		case "boolean":
			out[def.Name] = toBool(v)
		default:
			out[def.Name] = fmt.Sprint(v)
		}
	}
	return out, nil
}

func paramLabel(p *ParamDef) string {
	if p.Label != "" {
		return p.Label
	}
	return p.Name
}

func defaultForType(t string) any {
	switch t {
	case "number":
		return float64(0)
	case "boolean":
		return false
	default:
		return ""
	}
}

func toFloat(v any) (float64, error) {
	switch val := v.(type) {
	case float64:
		return val, nil
	case int:
		return float64(val), nil
	case int64:
		return float64(val), nil
	case string:
		return strconv.ParseFloat(strings.TrimSpace(val), 64)
	default:
		return 0, fmt.Errorf("숫자가 아닙니다")
	}
}

func toBool(v any) bool {
	switch val := v.(type) {
	case bool:
		return val
	case string:
		b, err := strconv.ParseBool(strings.TrimSpace(val))
		return err == nil && b
	case float64:
		return val != 0
	default:
		return false
	}
}

// ---------- 파라미터 치환 ----------

// varPattern은 ${이름} 형태의 참조를 찾는다.
// 점 표기(${row.name})까지 허용해 Lua 노드가 만든 표를 그대로 참조할 수 있게 한다.
var varPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z0-9_]+)*)\}`)

// resolve는 문자열 안의 ${변수}를 실행 문맥의 값으로 바꾼다.
//
// 템플릿을 쓰는 이유: 노드 설정은 대부분 문자열 한 줄(SQL, 경로, 메시지)이고,
// 그 안에 앞 노드의 결과를 끼워 넣는 것이 가장 흔한 요구다. 이것이 없으면
// 값을 옮기기 위해 Lua 노드를 사이마다 넣어야 한다.
//
// 주의: 이 치환은 SQL 문자열에도 적용되므로 값이 SQL로 해석될 수 있다.
// 그래서 db.query의 파라미터 바인딩(? 자리표시자)을 별도로 제공하고,
// 사용자 입력을 SQL에 끼워 넣을 때는 그쪽을 쓰도록 노드 설명에 적어 둔다.
func (r *runner) resolve(s string) string {
	if !strings.Contains(s, "${") {
		return s
	}
	return varPattern.ReplaceAllStringFunc(s, func(match string) string {
		path := varPattern.FindStringSubmatch(match)[1]
		v, ok := r.lookup(path)
		if !ok {
			return match
		}
		return stringify(v)
	})
}

func (r *runner) lookup(path string) (any, bool) {
	parts := strings.Split(path, ".")
	var cur any = r.vars
	for _, part := range parts {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[part]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

func stringify(v any) string {
	switch val := v.(type) {
	case nil:
		return ""
	case string:
		return val
	case float64:
		// 정수로 떨어지면 정수처럼 보여야 한다. "LIMIT 10.000000"은 문법 오류다.
		if val == float64(int64(val)) {
			return strconv.FormatInt(int64(val), 10)
		}
		return strconv.FormatFloat(val, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(val)
	case map[string]any, []any:
		b, err := json.Marshal(val)
		if err != nil {
			return fmt.Sprint(val)
		}
		return string(b)
	default:
		return fmt.Sprint(val)
	}
}

// str은 노드 파라미터를 문자열로 읽고 변수를 치환한다.
func (r *runner) str(n *Node, key string) string {
	v, ok := n.Params[key]
	if !ok || v == nil {
		return ""
	}
	return r.resolve(stringify(v))
}

func (r *runner) rawStr(n *Node, key string) string {
	v, ok := n.Params[key]
	if !ok || v == nil {
		return ""
	}
	return stringify(v)
}

func (r *runner) num(n *Node, key string, def float64) float64 {
	s := r.str(n, key)
	if s == "" {
		return def
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return def
	}
	return f
}

func (r *runner) flag(n *Node, key string) bool {
	v, ok := n.Params[key]
	if !ok {
		return false
	}
	return toBool(v)
}

// ---------- 권한 ----------

// connection은 노드가 지정한 커넥션을 읽고 필요한 능력을 확인한다.
//
// 권한 판정을 auth.Authorizer로 하는 것이 핵심이다. 매크로가 자체 판정 로직을
// 가지면 화면에서 막힌 일이 매크로로는 되는 상태가 생긴다.
func (r *runner) connection(id string, need model.Capability) (*model.Connection, *model.Secret, error) {
	if strings.TrimSpace(id) == "" {
		return nil, nil, fmt.Errorf("커넥션이 지정되지 않았습니다")
	}
	conn, err := r.engine.st.GetConnection(r.ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil, fmt.Errorf("커넥션을 찾을 수 없습니다: %s", id)
	}
	if err != nil {
		return nil, nil, err
	}
	d, err := r.engine.authz.CanCap(r.ctx, r.actor, conn.ID, need)
	if err != nil {
		return nil, nil, err
	}
	if !d.Allowed {
		return nil, nil, fmt.Errorf("%s: %s (%s)", conn.Name, d.Reason, need.Label())
	}
	if !conn.Enabled {
		return nil, nil, fmt.Errorf("%s 은(는) 비활성화된 커넥션입니다", conn.Name)
	}
	secret, err := r.engine.st.GetSecret(r.ctx, conn.ID)
	if err != nil {
		return nil, nil, err
	}
	return conn, secret, nil
}

// levelConnection은 등급(Level)이 필요한 노드용이다.
func (r *runner) levelConnection(id string, need model.Level) (*model.Connection, *model.Secret, error) {
	if strings.TrimSpace(id) == "" {
		return nil, nil, fmt.Errorf("커넥션이 지정되지 않았습니다")
	}
	conn, err := r.engine.st.GetConnection(r.ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil, fmt.Errorf("커넥션을 찾을 수 없습니다: %s", id)
	}
	if err != nil {
		return nil, nil, err
	}
	d, err := r.engine.authz.Can(r.ctx, r.actor, conn.ID, need)
	if err != nil {
		return nil, nil, err
	}
	if !d.Allowed {
		return nil, nil, fmt.Errorf("%s: %s", conn.Name, d.Reason)
	}
	secret, err := r.engine.st.GetSecret(r.ctx, conn.ID)
	if err != nil {
		return nil, nil, err
	}
	return conn, secret, nil
}
