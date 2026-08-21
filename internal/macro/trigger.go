package macro

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"dbstudio/internal/model"
	"dbstudio/internal/store"
)

// 매크로 자동 실행.
//
// 두 종류를 한 곳에서 다룬다: 시각이 되면(schedule), 조건이 맞으면(event).
// 어느 쪽이든 결국 같은 일을 한다 — **소유자를 불러와 그 사람의 권한으로 실행한다.**
//
// 소유자 권한으로 도는 것이 이 기능에서 가장 중요한 결정이다. 서비스 계정을 만들어
// 그 권한으로 돌리면 "툴은 호출자의 권한으로 실행된다"는 앱 전체의 규칙이 무너지고,
// 자동 실행이 권한 상승 통로가 된다. 대신 소유자의 계정이 잠기거나 권한이 회수되면
// 그 트리거는 실행 시점에 실패한다 — 그것이 옳은 동작이다.

// tickInterval은 스케줄을 확인하는 주기다.
//
// cron의 최소 단위가 1분이므로 그보다 자주 볼 이유가 없고, 정확히 1분이면 경계에서
// 한 번 놓칠 수 있다. 30초면 어느 분이든 최소 한 번은 확인한다.
const tickInterval = 30 * time.Second

// missedGrace는 "놓친 실행"으로 볼 시간이다.
//
// 앱이 꺼져 있던 동안의 실행은 따라잡지 않는다. 새벽 3시 정리 작업을 오전 9시에
// 실행하는 것은 대개 틀린 일이고, 밤새 꺼져 있었다면 여덟 번을 몰아 실행하게 된다.
// 이 시간 안이면 실행하고, 넘으면 건너뛰고 다음 시각을 잡는다.
const missedGrace = time.Hour

// maxTriggerFailures는 연속 실패 상한이다. 넘으면 트리거가 스스로 꺼진다.
// 영원히 실패하는 트리거는 로그만 채우고 아무도 보지 않는다.
const maxTriggerFailures = 10

// Scheduler는 트리거를 감시하고 매크로를 시작한다.
type Scheduler struct {
	engine *Engine
	// lastEventFire는 이벤트 트리거의 디바운스 상태다.
	//
	// DB의 last_fired_at을 쓰지 않고 메모리에 두는 이유: 재시작하면 다시 세는 것이
	// 맞기 때문이다. 앱이 꺼졌다 켜진 뒤 첫 이벤트는 억제하지 않는 편이 안전하다.
	//
	// 잠금이 필요한 이유: 이벤트 알림은 모니터가 이벤트마다 고루틴으로 보내므로
	// EventOpened가 동시에 여러 번 돌 수 있다. 디바운스 자체도 그 경합에서 정확해야
	// 한다 — 같은 순간 들어온 두 이벤트가 둘 다 "마지막 발화 없음"을 보면 억제가
	// 통째로 무력해진다.
	mu            sync.Mutex
	lastEventFire map[string]time.Time
}

// allowEventFire는 디바운스를 판정하고 통과시킬 때 발화 시각을 기록한다.
// 확인과 기록이 한 잠금 안에 있어야 동시 이벤트에서도 간격이 지켜진다.
func (s *Scheduler) allowEventFire(id string, minInterval int, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if last, ok := s.lastEventFire[id]; ok {
		gap := time.Duration(minInterval) * time.Second
		if gap > 0 && now.Sub(last) < gap {
			return false
		}
	}
	s.lastEventFire[id] = now
	return true
}

func (e *Engine) Scheduler() *Scheduler {
	if e.scheduler == nil {
		e.scheduler = &Scheduler{engine: e, lastEventFire: map[string]time.Time{}}
	}
	return e.scheduler
}

// Run은 스케줄 루프를 돈다. 컨텍스트가 끝날 때까지 반환하지 않는다.
func (s *Scheduler) Run(ctx context.Context) {
	// 부팅 직후 한 번 정리한다. 앱이 꺼져 있던 동안 next_run_at이 과거가 된
	// 트리거들의 다음 시각을 다시 잡아야 한다.
	s.reschedule(ctx)

	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.tick(ctx)
		}
	}
}

// reschedule은 next_run_at이 비었거나 과거인 트리거의 다음 시각을 다시 계산한다.
func (s *Scheduler) reschedule(ctx context.Context) {
	triggers, err := s.engine.st.ListTriggers(ctx, "")
	if err != nil {
		s.engine.log.Error("트리거 목록을 읽지 못했습니다", "err", err)
		return
	}
	now := time.Now()
	for _, t := range triggers {
		s.reconcileOutcome(ctx, t)

		if t.Kind != store.TriggerSchedule || !t.Enabled {
			continue
		}
		// 놓친 실행이 유예 시간 안이면 그대로 둔다 — tick이 곧 집어간다.
		if t.NextRunAt != nil && now.Sub(*t.NextRunAt) <= missedGrace {
			continue
		}
		if t.NextRunAt != nil {
			s.engine.log.Warn("앱이 꺼져 있는 동안 지난 실행을 건너뜁니다",
				"trigger", t.Name, "예정", t.NextRunAt.Format(time.RFC3339))
		}
		s.planNext(ctx, t, now)
	}
}

// reconcileOutcome은 "시작됨"에서 멈춘 트리거를 실제 실행 결과로 맞춘다.
//
// 필요한 이유: 결과는 실행 고루틴이 끝나면서 적는데, 그 전에 앱이 죽으면 트리거는
// 영영 "시작됨"으로 남는다. 부팅 때 한 번 실행 기록을 보고 맞춰 주면 목록이
// 거짓말을 하지 않는다. 실행 기록 쪽은 MarkStaleRunsFailed가 이미 정리해 둔다.
func (s *Scheduler) reconcileOutcome(ctx context.Context, t *store.MacroTrigger) {
	if t.LastStatus != "started" || t.LastRunID == "" {
		return
	}
	run, err := s.engine.st.GetMacroRun(ctx, t.LastRunID)
	if err != nil || run.Status == "running" {
		return
	}
	if err := s.engine.st.RecordTriggerFire(ctx, t.ID, run.ID, run.Status, run.Error); err != nil {
		s.engine.log.Error("트리거 결과 보정 실패", "trigger", t.Name, "err", err)
		return
	}
	s.engine.log.Info("중단된 트리거 실행의 결과를 보정했습니다",
		"trigger", t.Name, "run", run.ID, "status", run.Status)
}

// planNext는 다음 실행 시각을 계산해 저장한다.
func (s *Scheduler) planNext(ctx context.Context, t *store.MacroTrigger, after time.Time) {
	schedule, err := ParseSchedule(t.Cron)
	if err != nil {
		s.disable(ctx, t, "cron 식을 읽을 수 없습니다: "+err.Error())
		return
	}
	loc, err := LoadLocation(t.Timezone)
	if err != nil {
		s.disable(ctx, t, err.Error())
		return
	}
	next, ok := schedule.Next(after.In(loc))
	if !ok {
		s.disable(ctx, t, "이 cron 식은 실행될 시각이 없습니다: "+t.Cron)
		return
	}
	utc := next.UTC()
	if err := s.engine.st.SetTriggerNextRun(ctx, t.ID, &utc); err != nil {
		s.engine.log.Error("다음 실행 시각을 저장하지 못했습니다", "trigger", t.Name, "err", err)
	}
}

func (s *Scheduler) disable(ctx context.Context, t *store.MacroTrigger, reason string) {
	s.engine.log.Warn("트리거를 비활성화합니다", "trigger", t.Name, "reason", reason)
	if err := s.engine.st.DisableTriggerWithReason(ctx, t.ID, reason); err != nil {
		s.engine.log.Error("트리거 비활성화 실패", "trigger", t.Name, "err", err)
	}
}

// tick은 지금 실행할 때가 된 트리거를 처리한다.
func (s *Scheduler) tick(ctx context.Context) {
	now := time.Now()
	due, err := s.engine.st.DueTriggers(ctx, now)
	if err != nil {
		s.engine.log.Error("예정된 트리거를 읽지 못했습니다", "err", err)
		return
	}
	for _, t := range due {
		// 다음 시각을 **먼저** 잡는다. 실행이 오래 걸리거나 실패해도 같은 트리거가
		// 다음 tick에서 다시 잡히지 않아야 한다.
		s.planNext(ctx, t, now)

		if t.NextRunAt != nil && now.Sub(*t.NextRunAt) > missedGrace {
			s.engine.log.Warn("유예 시간을 넘긴 실행을 건너뜁니다", "trigger", t.Name)
			_ = s.engine.st.RecordTriggerFire(ctx, t.ID, "", "skipped", "유예 시간을 넘겨 건너뜀")
			continue
		}
		s.fire(ctx, t, nil)
	}
}

// EventOpened는 새 모니터링 이벤트를 받는다(monitor.EventSink 구현).
//
// 새로 만들어진 이벤트만 온다. 같은 원인이 반복되어 occurrences만 오르는 경우까지
// 받으면 지표가 임계치 근처에서 흔들릴 때마다 매크로가 시작된다.
func (s *Scheduler) EventOpened(ctx context.Context, ev *store.Event) {
	if ev == nil {
		return
	}
	triggers, err := s.engine.st.EventTriggers(ctx, ev.ConnectionID)
	if err != nil {
		s.engine.log.Error("이벤트 트리거를 읽지 못했습니다", "err", err)
		return
	}
	now := time.Now()
	for _, t := range triggers {
		if !matchesEvent(t, ev) {
			continue
		}
		// 디바운스. 같은 트리거가 연달아 터지면 매크로가 문제를 키운다.
		if !s.allowEventFire(t.ID, t.MinIntervalSec, now) {
			s.engine.log.Debug("이벤트 트리거를 억제했습니다(최소 간격)",
				"trigger", t.Name, "minIntervalSec", t.MinIntervalSec)
			continue
		}
		s.fire(ctx, t, ev)
	}
}

// matchesEvent는 트리거 조건이 이벤트와 맞는지 본다. 빈 조건은 "전부"다.
func matchesEvent(t *store.MacroTrigger, ev *store.Event) bool {
	if t.EventKind != "" && t.EventKind != ev.Kind {
		return false
	}
	if t.EventMetric != "" && t.EventMetric != ev.Metric {
		return false
	}
	if t.EventSeverity != "" {
		want := store.Severity(t.EventSeverity)
		if ev.Severity.Rank() < want.Rank() {
			return false
		}
	}
	return true
}

// fire는 트리거 하나를 실행한다.
//
// 실패해도 여기서 오류를 반환하지 않는다. 자동 실행은 아무도 응답을 기다리지 않으므로
// 결과를 트리거 행과 실행 기록에 남기는 것이 유일한 보고 수단이다.
func (s *Scheduler) fire(ctx context.Context, t *store.MacroTrigger, ev *store.Event) {
	owner, err := s.resolveOwner(ctx, t)
	if err != nil {
		s.disable(ctx, t, err.Error())
		_ = s.engine.st.RecordTriggerFire(ctx, t.ID, "", "failed", err.Error())
		return
	}

	// 소유자 기준으로 읽는다. 소유자가 매크로 접근 권한을 잃으면(비공개로 바뀌었거나
	// 협업자에서 빠졌거나) 여기서 실패하고, 연속 실패가 쌓이면 트리거는 스스로 꺼진다.
	m, ver, graph, err := s.engine.load(ctx, t.MacroID, 0, owner)
	if err != nil {
		s.recordFailure(ctx, t, "매크로를 읽지 못했습니다: "+err.Error())
		return
	}

	// 실행 전 권한 판정. 화면·MCP와 같은 계산을 쓴다 — 소유자의 권한이 회수되면
	// 자동 실행도 멈춰야 한다.
	blockers, err := s.engine.Blockers(ctx, owner, graph)
	if err != nil {
		s.recordFailure(ctx, t, "권한을 판정하지 못했습니다: "+err.Error())
		return
	}
	if len(blockers) > 0 {
		reasons := make([]string, 0, len(blockers))
		for _, b := range blockers {
			reasons = append(reasons, b.Node+": "+b.Reason)
		}
		s.recordFailure(ctx, t, "소유자에게 실행 권한이 없습니다 — "+strings.Join(reasons, " / "))
		return
	}

	if t.SkipIfRunning {
		// 소유자 시점으로 본다. 이 시점에는 소유자가 매크로를 볼 수 있음이 확인되어 있고
		// (위 load), "누가 돌리고 있든" 겹치면 건너뛰는 것이 이 옵션의 뜻이다.
		running, err := s.engine.st.ListMacroRuns(ctx, t.MacroID, "running", 1,
			store.MacroViewer{User: owner})
		if err == nil && len(running) > 0 {
			s.engine.log.Info("이전 실행이 끝나지 않아 건너뜁니다", "trigger", t.Name)
			_ = s.engine.st.RecordTriggerFire(ctx, t.ID, "", "skipped", "이전 실행이 아직 진행 중")
			return
		}
	}

	params := maps.Clone(t.Params)
	if params == nil {
		params = map[string]any{}
	}

	// 식으로 정한 파라미터를 이 자리에서 계산한다.
	//
	// 트리거 문맥(trigger·event)과 now를 넘긴다. 이벤트로 시작된 매크로가 "무엇 때문에
	// 시작됐는가"를 파라미터로 받으려면 이 계산이 실행 직전에 일어나야 한다.
	if len(t.ParamExprs) > 0 {
		vars := triggerContext(t, ev)
		computed, problems := EvalParamExprs(ctx, t.ParamExprs, map[string]any{
			"trigger": vars,
			"event":   vars["event"],
			"now":     time.Now().Format(time.RFC3339),
		})
		maps.Copy(params, computed)
		if len(problems) > 0 {
			// 실패한 식은 파라미터를 비운 채 진행한다. 여기서 실행 자체를 막으면
			// 식 하나 때문에 자동화가 통째로 멈춘다 — 대신 기록에 남긴다.
			s.engine.log.Warn("트리거 파라미터 식 계산 실패",
				"trigger", t.Name, "problems", strings.Join(problems, " / "))
		}
	}

	triggerID := t.ID
	triggerName := t.Name
	runID, err := s.engine.Start(ctx, RunRequest{
		MacroID: m.ID, Version: ver, Actor: owner,
		ActorIP: "trigger:" + t.Kind,
		Params:  params,
		Trigger: t.Kind,
		// 이벤트 정보는 파라미터가 아니라 실행 문맥의 변수로 넘긴다.
		// 파라미터는 매크로가 미리 선언한 것만 받으므로, 선언하지 않은 매크로에서는
		// 이벤트 정보가 조용히 사라진다.
		TriggerContext: triggerContext(t, ev),
		// 실행이 끝나면 결과를 트리거 행에 되돌려 적는다. 자동 실행은 아무도 보고
		// 있지 않으므로, 이 기록이 유일한 보고 수단이다.
		OnFinish: func(runID, status, errMsg string) {
			s.recordOutcome(triggerID, triggerName, runID, status, errMsg)
		},
	})
	if err != nil {
		s.recordFailure(ctx, t, err.Error())
		return
	}
	_ = s.engine.st.RecordTriggerFire(ctx, t.ID, runID, "started", "")
	s.engine.log.Info("트리거로 매크로를 시작했습니다",
		"trigger", t.Name, "macro", m.Name, "run", runID, "owner", owner.Username)
}

// recordOutcome은 끝난 실행의 결과를 트리거 행에 적는다.
//
// 자체 컨텍스트를 쓰는 이유: 이 함수는 실행 고루틴의 끝에서 불리고, 그 컨텍스트는
// 실행이 시간 제한에 걸렸을 때 이미 취소되어 있다. 하필 그때(=실패했을 때)
// 결과를 남기지 못하면 트리거 목록에는 "시작됨"만 영원히 남는다.
func (s *Scheduler) recordOutcome(triggerID, triggerName, runID, status, errMsg string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := s.engine.st.RecordTriggerFire(ctx, triggerID, runID, status, errMsg); err != nil {
		s.engine.log.Error("트리거 결과 기록 실패", "trigger", triggerName, "err", err)
		return
	}
	if status == "success" {
		return
	}
	s.engine.log.Warn("트리거 실행이 성공하지 않았습니다",
		"trigger", triggerName, "run", runID, "status", status, "err", errMsg)

	// 매번 실패하는 트리거는 스스로 꺼진다. 여기서도 확인해야 하는 이유:
	// 가장 흔한 실패는 매크로 자체가 실행 중에 죽는 경우인데, 그것은 시작에
	// 성공했으므로 recordFailure 경로를 지나지 않는다.
	fresh, err := s.engine.st.GetTrigger(ctx, triggerID)
	if err == nil && fresh.FailCount >= maxTriggerFailures {
		s.disable(ctx, fresh, fmt.Sprintf("연속 %d회 실패해 자동으로 비활성화했습니다: %s",
			fresh.FailCount, errMsg))
	}
}

func (s *Scheduler) recordFailure(ctx context.Context, t *store.MacroTrigger, reason string) {
	s.engine.log.Warn("트리거 실행 실패", "trigger", t.Name, "reason", reason)
	if err := s.engine.st.RecordTriggerFire(ctx, t.ID, "", "failed", reason); err != nil {
		s.engine.log.Error("트리거 결과 기록 실패", "trigger", t.Name, "err", err)
	}
	// 연속 실패가 쌓이면 스스로 끈다.
	if fresh, err := s.engine.st.GetTrigger(ctx, t.ID); err == nil && fresh.FailCount >= maxTriggerFailures {
		s.disable(ctx, t, fmt.Sprintf("연속 %d회 실패해 자동으로 비활성화했습니다: %s",
			fresh.FailCount, reason))
	}
}

// resolveOwner는 소유자를 불러온다. 실행 권한의 근거가 되는 값이다.
func (s *Scheduler) resolveOwner(ctx context.Context, t *store.MacroTrigger) (*model.User, error) {
	if t.OwnerID == "" {
		return nil, errors.New("소유자가 없는 트리거입니다")
	}
	u, err := s.engine.st.GetUser(ctx, t.OwnerID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, errors.New("소유자 계정이 삭제되었습니다")
	}
	if err != nil {
		return nil, err
	}
	if model.UserStatus(u.Status) == model.UserDisabled {
		return nil, errors.New("소유자 계정이 비활성화되었습니다")
	}
	if !u.HasPerm(model.PermMacro) {
		return nil, errors.New("소유자에게 매크로 사용 권한이 없습니다")
	}
	return u, nil
}

// triggerContext는 매크로 안에서 볼 수 있는 트리거 정보다.
func triggerContext(t *store.MacroTrigger, ev *store.Event) map[string]any {
	out := map[string]any{
		"kind": t.Kind,
		"name": t.Name,
		"id":   t.ID,
	}
	if ev == nil {
		return out
	}
	event := map[string]any{
		"id":           float64(ev.ID),
		"kind":         ev.Kind,
		"severity":     string(ev.Severity),
		"message":      ev.Message,
		"connectionId": ev.ConnectionID,
		"metric":       ev.Metric,
		"occurrences":  float64(ev.Occurrences),
	}
	if ev.Value != nil {
		event["value"] = *ev.Value
	}
	if ev.Threshold != nil {
		event["threshold"] = *ev.Threshold
	}
	out["event"] = event
	return out
}

// ---------- 권한 판정 ----------

// Blocker는 실행을 막는 이유 하나다.
type Blocker struct {
	NodeID string `json:"nodeId"`
	Node   string `json:"node"`
	Reason string `json:"reason"`
}

// Blockers는 이 사용자가 그래프를 실행할 수 없는 이유를 모은다.
//
// 화면(실행 버튼 비활성화), MCP·AI 툴(실행 전 확인), 자동 실행(소유자 권한 확인)이
// 모두 이 함수를 쓴다. 같은 판정을 세 곳에서 따로 구현하면 어느 하나가 느슨해지고,
// 느슨한 쪽이 곧 우회 경로가 된다.
func (e *Engine) Blockers(ctx context.Context, u *model.User, g *Graph) ([]Blocker, error) {
	byType := make(map[string]NodeSpec, len(specs))
	for _, spec := range Specs() {
		byType[spec.Type] = spec
	}

	conns, err := e.st.ListConnections(ctx)
	if err != nil {
		return nil, err
	}
	effective, err := e.authz.EffectiveAccessList(ctx, u, conns)
	if err != nil {
		return nil, err
	}
	access := make(map[string]model.EffectiveAccess, len(effective))
	for _, a := range effective {
		access[a.ConnectionID] = a
	}
	names := make(map[string]string, len(conns))
	for _, c := range conns {
		names[c.ID] = c.Name
	}

	out := []Blocker{}
	add := func(n *Node, label, reason string) {
		out = append(out, Blocker{NodeID: n.ID, Node: label, Reason: reason})
	}

	for _, n := range g.Nodes {
		if n.Disabled {
			continue
		}
		spec, ok := byType[n.Type]
		if !ok {
			continue
		}
		label := n.Label
		if label == "" {
			label = spec.Label
		}

		if spec.NeedsPerm != "" && !u.HasPerm(spec.NeedsPerm) {
			add(n, label, spec.NeedsPerm.Label()+" 권한이 없습니다")
		}
		if spec.NeedsShell && !e.cfg.AllowShell {
			add(n, label, "서버가 셸 실행을 허용하지 않습니다(-allow-shell 없이 실행 중)")
		}

		// 커넥션이 ${변수}로 지정된 경우는 실행 전에 알 수 없다.
		// 모른다고 막으면 정상적인 매크로를 못 돌리므로 확인할 수 있는 것만 확인한다.
		connID, _ := n.Params["connection"].(string)
		if connID == "" || strings.Contains(connID, "${") {
			continue
		}
		// 전체 DB는 대상이 하나가 아니다. 접근할 수 있는 것만 골라 도는 것이
		// 그 값의 정의이므로(execBackupAll), 여기서 막을 것이 없다.
		if connID == ConnectionAll {
			continue
		}
		label2 := names[connID]
		if label2 == "" {
			label2 = connID
		}
		a, known := access[connID]
		if !known || !a.Accessible {
			if spec.NeedsCap != "" || spec.NeedsLevel != "" {
				add(n, label, label2+" 에 접근할 수 없습니다")
			}
			continue
		}
		if spec.NeedsCap != "" && !slices.Contains(a.Caps, spec.NeedsCap) {
			add(n, label, label2+": "+spec.NeedsCap.Label()+" 권한이 없습니다")
		}
		if spec.NeedsLevel != "" && !a.Level.Includes(spec.NeedsLevel) {
			add(n, label, label2+": "+string(spec.NeedsLevel)+" 등급이 필요합니다")
		}
	}
	return out, nil
}
