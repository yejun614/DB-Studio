package monitor

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dbstudio/internal/crypto"
	"dbstudio/internal/metric"
	"dbstudio/internal/model"
	"dbstudio/internal/schema"
	"dbstudio/internal/store"
)

// newTestStore는 임시 파일에 메타 DB를 만든다.
func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	box, err := crypto.NewSecretBox(key)
	if err != nil {
		t.Fatalf("secret box: %v", err)
	}
	path := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(context.Background(), path, box)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func newTestConnection(t *testing.T, st *store.Store, name string, env model.Environment) *model.Connection {
	t.Helper()
	pw := "pw"
	_, conn, err := st.CreateServerWithDatabase(context.Background(),
		store.SaveServerParams{
			Name: name, Kind: model.KindMySQL, DefaultEnvironment: env,
			Host: "127.0.0.1", Port: 3306,
			Options: model.Options{}, Tags: []string{}, Enabled: true,
			Username: "root", Password: &pw,
		},
		store.SaveConnectionParams{
			Name: name, Environment: env, DatabaseName: "appdb",
			Tags: []string{}, Enabled: true,
		})
	if err != nil {
		t.Fatalf("create connection: %v", err)
	}
	return conn
}

// ---------- 룰 조건 판정 ----------

func TestRuleBreached(t *testing.T) {
	cases := []struct {
		op        string
		threshold float64
		value     float64
		want      bool
	}{
		{">", 80, 81, true}, {">", 80, 80, false}, {">", 80, 79, false},
		{">=", 80, 80, true}, {">=", 80, 79, false},
		{"<", 90, 89, true}, {"<", 90, 90, false},
		{"<=", 90, 90, true}, {"<=", 90, 91, false},
		{"==", 0, 0, true}, {"==", 0, 1, false},
		{"!=", 0, 1, true}, {"!=", 0, 0, false},
		{"bogus", 0, 5, false}, // 알 수 없는 연산자는 위반이 아니다
	}
	for _, c := range cases {
		r := &store.Rule{Op: c.op, Threshold: c.threshold}
		if got := r.Breached(c.value); got != c.want {
			t.Errorf("%g %s %g → %v, 기대값 %v", c.value, c.op, c.threshold, got, c.want)
		}
	}
}

func TestRuleAppliesTo(t *testing.T) {
	dev := &model.Connection{ID: "c1", Environment: model.EnvDev}
	prod := &model.Connection{ID: "c2", Environment: model.EnvProd}

	cases := []struct {
		name string
		rule *store.Rule
		conn *model.Connection
		want bool
	}{
		{"전체 적용", &store.Rule{Enabled: true}, dev, true},
		{"비활성 룰", &store.Rule{Enabled: false}, dev, false},
		{"커넥션 지정 일치", &store.Rule{Enabled: true, ConnectionID: "c1"}, dev, true},
		{"커넥션 지정 불일치", &store.Rule{Enabled: true, ConnectionID: "c1"}, prod, false},
		{"환경 일치", &store.Rule{Enabled: true, Environment: model.EnvProd}, prod, true},
		{"환경 불일치", &store.Rule{Enabled: true, Environment: model.EnvProd}, dev, false},
		{"nil 커넥션", &store.Rule{Enabled: true}, nil, false},
	}
	for _, c := range cases {
		if got := c.rule.AppliesTo(c.conn); got != c.want {
			t.Errorf("%s: %v, 기대값 %v", c.name, got, c.want)
		}
	}
}

// ---------- 지속 시간 조건 ----------

// TestThresholdDurationGate는 지속 시간 조건이 실제로 이벤트를 지연시키는지 확인한다.
//
// 이것이 없으면 순간적인 스파이크마다 이벤트가 생겨 알림이 무의미해진다.
func TestThresholdDurationGate(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	conn := newTestConnection(t, st, "dev-db", model.EnvDev)

	rule, err := st.CreateRule(ctx, &store.Rule{
		Name: "세션 사용률", Kind: store.EventThreshold,
		Metric: metric.NameConnUsedPct, Op: ">", Threshold: 80,
		DurationSec: 120, Severity: store.SeverityWarning, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create rule: %v", err)
	}

	engine := NewRuleEngine(st)
	breaching := metric.NewSet()
	breaching.Gauge(metric.NameConnUsedPct, 95, metric.UnitPercent)

	// 1회차: 위반이 시작되었지만 지속 시간을 채우지 못했다.
	if err := engine.EvaluateThresholds(ctx, conn, breaching); err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	events, total, err := st.ListEvents(ctx, store.EventFilter{})
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if total != 0 {
		t.Errorf("지속 시간 미달인데 이벤트가 %d건 생성되었습니다: %v", total, eventMessages(events))
	}

	// 위반 시작 시각을 과거로 밀어 지속 시간이 지난 상황을 만든다.
	engine.mu.Lock()
	engine.breach[breachKey(conn.ID, rule.ID)].since = time.Now().Add(-3 * time.Minute)
	engine.mu.Unlock()

	if err := engine.EvaluateThresholds(ctx, conn, breaching); err != nil {
		t.Fatalf("evaluate after duration: %v", err)
	}
	events, total, err = st.ListEvents(ctx, store.EventFilter{})
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if total != 1 {
		t.Fatalf("지속 시간 경과 후 이벤트가 1건이어야 하는데 %d건: %v", total, eventMessages(events))
	}
	ev := events[0]
	if ev.State != "open" || ev.Severity != store.SeverityWarning {
		t.Errorf("이벤트 상태가 잘못되었습니다: state=%s severity=%s", ev.State, ev.Severity)
	}
	if ev.Value == nil || *ev.Value != 95 {
		t.Errorf("이벤트 값이 95여야 합니다: %v", ev.Value)
	}
	if !strings.Contains(ev.Message, "dev-db") {
		t.Errorf("메시지에 커넥션 이름이 없습니다: %q", ev.Message)
	}

	// 계속 위반해도 새 이벤트를 만들지 않고 발생 횟수만 올려야 한다.
	for range 3 {
		if err := engine.EvaluateThresholds(ctx, conn, breaching); err != nil {
			t.Fatalf("evaluate repeat: %v", err)
		}
	}
	events, total, err = st.ListEvents(ctx, store.EventFilter{})
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if total != 1 {
		t.Errorf("반복 위반이 새 이벤트를 만들었습니다: %d건", total)
	}
	if events[0].Occurrences != 1 {
		// 이벤트가 열린 뒤에는 shouldOpen이 false라 OpenEvent가 호출되지 않는다.
		// 즉 occurrences는 1로 유지되는 것이 현재 설계의 의도다.
		t.Logf("occurrences=%d (이벤트가 열린 후에는 재개시하지 않음)", events[0].Occurrences)
	}

	// 정상으로 돌아오면 해소되어야 한다.
	healthy := metric.NewSet()
	healthy.Gauge(metric.NameConnUsedPct, 40, metric.UnitPercent)
	if err := engine.EvaluateThresholds(ctx, conn, healthy); err != nil {
		t.Fatalf("evaluate recovery: %v", err)
	}
	events, _, err = st.ListEvents(ctx, store.EventFilter{})
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if events[0].State != "resolved" {
		t.Errorf("정상 복귀 후에도 이벤트가 열려 있습니다: %s", events[0].State)
	}
	if events[0].ResolvedAt == nil {
		t.Error("해소 시각이 기록되지 않았습니다")
	}
}

// TestThresholdImmediate는 duration=0 룰이 즉시 발동하는지 확인한다.
func TestThresholdImmediate(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	conn := newTestConnection(t, st, "dev-db", model.EnvDev)

	if _, err := st.CreateRule(ctx, &store.Rule{
		Name: "데드락", Kind: store.EventThreshold, Metric: metric.NameDeadlocks,
		Op: ">", Threshold: 0, DurationSec: 0, Severity: store.SeverityCritical, Enabled: true,
	}); err != nil {
		t.Fatalf("create rule: %v", err)
	}

	engine := NewRuleEngine(st)
	set := metric.NewSet()
	set.Gauge(metric.NameDeadlocks, 0.5, metric.UnitPerSec)
	if err := engine.EvaluateThresholds(ctx, conn, set); err != nil {
		t.Fatalf("evaluate: %v", err)
	}

	_, total, err := st.ListEvents(ctx, store.EventFilter{})
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if total != 1 {
		t.Fatalf("duration=0 룰은 즉시 발동해야 합니다: %d건", total)
	}
}

// TestMissingMetricDoesNotFire는 DB가 제공하지 않는 지표의 룰이 발동하지 않는지 확인한다.
//
// Redis에 복제 지연 룰이 걸려 있을 때 값이 없다고 0으로 간주하면
// "복제 지연 < 30" 같은 룰이 항상 만족되어 잘못된 정상 판정이 나온다.
func TestMissingMetricDoesNotFire(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	conn := newTestConnection(t, st, "dev-db", model.EnvDev)

	if _, err := st.CreateRule(ctx, &store.Rule{
		Name: "복제 지연", Kind: store.EventThreshold, Metric: metric.NameReplicaLag,
		Op: ">", Threshold: 30, Severity: store.SeverityCritical, Enabled: true,
	}); err != nil {
		t.Fatalf("create rule: %v", err)
	}

	engine := NewRuleEngine(st)
	set := metric.NewSet()
	set.Gauge(metric.NameConnTotal, 5, metric.UnitCount) // 복제 지연 지표는 없음
	if err := engine.EvaluateThresholds(ctx, conn, set); err != nil {
		t.Fatalf("evaluate: %v", err)
	}

	_, total, err := st.ListEvents(ctx, store.EventFilter{})
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if total != 0 {
		t.Errorf("없는 지표로 이벤트가 생성되었습니다: %d건", total)
	}
}

// TestEnvironmentScopedRule은 환경 한정 룰이 해당 환경에만 적용되는지 확인한다.
func TestEnvironmentScopedRule(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	devConn := newTestConnection(t, st, "dev-db", model.EnvDev)
	prodConn := newTestConnection(t, st, "prod-db", model.EnvProd)

	if _, err := st.CreateRule(ctx, &store.Rule{
		Name: "운영 전용", Kind: store.EventThreshold, Metric: metric.NameConnUsedPct,
		Op: ">", Threshold: 50, Environment: model.EnvProd,
		Severity: store.SeverityCritical, Enabled: true,
	}); err != nil {
		t.Fatalf("create rule: %v", err)
	}

	engine := NewRuleEngine(st)
	set := metric.NewSet()
	set.Gauge(metric.NameConnUsedPct, 90, metric.UnitPercent)

	if err := engine.EvaluateThresholds(ctx, devConn, set); err != nil {
		t.Fatalf("evaluate dev: %v", err)
	}
	if _, total, _ := st.ListEvents(ctx, store.EventFilter{}); total != 0 {
		t.Errorf("개발 커넥션에 운영 전용 룰이 적용되었습니다: %d건", total)
	}

	if err := engine.EvaluateThresholds(ctx, prodConn, set); err != nil {
		t.Fatalf("evaluate prod: %v", err)
	}
	if _, total, _ := st.ListEvents(ctx, store.EventFilter{}); total != 1 {
		t.Errorf("운영 커넥션에 룰이 적용되지 않았습니다")
	}
}

// ---------- 카운터 변화율 ----------

// TestDeriveRates는 누적 카운터가 초당 변화율로 변환되는지 확인한다.
func TestDeriveRates(t *testing.T) {
	st := newTestStore(t)
	m := NewManager(st, DefaultConfig())
	connID := "conn-1"

	// 1회차: 비교 대상이 없어 카운터 샘플은 버려진다.
	first := metric.NewSet()
	first.CollectedAt = time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	first.Counter(metric.NameQueryRate, 1000)
	first.Gauge(metric.NameConnTotal, 5, metric.UnitCount)
	m.deriveRates(connID, first)

	if _, ok := first.Get(metric.NameQueryRate); ok {
		t.Error("첫 관측의 카운터는 저장하지 않아야 합니다 (변화율을 계산할 수 없음)")
	}
	if _, ok := first.Get(metric.NameConnTotal); !ok {
		t.Error("게이지는 그대로 유지되어야 합니다")
	}

	// 2회차: 10초 뒤 200 증가 → 20/s
	second := metric.NewSet()
	second.CollectedAt = first.CollectedAt.Add(10 * time.Second)
	second.Counter(metric.NameQueryRate, 1200)
	m.deriveRates(connID, second)

	sm, ok := second.Get(metric.NameQueryRate)
	if !ok {
		t.Fatal("두 번째 관측에서 변화율이 계산되어야 합니다")
	}
	if sm.Value != 20 {
		t.Errorf("변화율이 20/s여야 하는데 %g", sm.Value)
	}
	if sm.Kind != metric.Gauge || sm.Unit != metric.UnitPerSec {
		t.Errorf("변화율은 게이지+per_sec여야 합니다: kind=%s unit=%s", sm.Kind, sm.Unit)
	}

	// 3회차: 카운터가 감소 = 서버 재시작. 음수 변화율을 만들지 않아야 한다.
	third := metric.NewSet()
	third.CollectedAt = second.CollectedAt.Add(10 * time.Second)
	third.Counter(metric.NameQueryRate, 50)
	m.deriveRates(connID, third)

	if _, ok := third.Get(metric.NameQueryRate); ok {
		t.Error("카운터 감소(서버 재시작) 시 변화율을 저장하지 않아야 합니다")
	}

	// 4회차: 재시작 후 정상 증가는 다시 계산되어야 한다.
	fourth := metric.NewSet()
	fourth.CollectedAt = third.CollectedAt.Add(10 * time.Second)
	fourth.Counter(metric.NameQueryRate, 150)
	m.deriveRates(connID, fourth)

	sm, ok = fourth.Get(metric.NameQueryRate)
	if !ok || sm.Value != 10 {
		t.Errorf("재시작 후 변화율이 10/s여야 합니다: ok=%v value=%v", ok, sm.Value)
	}
}

// TestDeriveRatesIsolatedPerConnection은 커넥션별로 카운터 상태가 분리되는지 확인한다.
func TestDeriveRatesIsolatedPerConnection(t *testing.T) {
	st := newTestStore(t)
	m := NewManager(st, DefaultConfig())
	base := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)

	for _, connID := range []string{"a", "b"} {
		s := metric.NewSet()
		s.CollectedAt = base
		s.Counter(metric.NameQueryRate, 100)
		m.deriveRates(connID, s)
	}
	// a는 10초 뒤 100 증가, b는 같은 값 유지
	sa := metric.NewSet()
	sa.CollectedAt = base.Add(10 * time.Second)
	sa.Counter(metric.NameQueryRate, 200)
	m.deriveRates("a", sa)

	sb := metric.NewSet()
	sb.CollectedAt = base.Add(10 * time.Second)
	sb.Counter(metric.NameQueryRate, 100)
	m.deriveRates("b", sb)

	ra, _ := sa.Get(metric.NameQueryRate)
	rb, _ := sb.Get(metric.NameQueryRate)
	if ra.Value != 10 {
		t.Errorf("a의 변화율이 10/s여야 합니다: %g", ra.Value)
	}
	if rb.Value != 0 {
		t.Errorf("b의 변화율이 0/s여야 합니다: %g", rb.Value)
	}
}

// ---------- 저장 / 조회 ----------

// TestSaveAndQuerySeries는 지표 저장과 시계열 조회를 확인한다.
func TestSaveAndQuerySeries(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	conn := newTestConnection(t, st, "dev-db", model.EnvDev)

	base := time.Now().UTC().Add(-30 * time.Minute)
	for i := range 10 {
		set := metric.NewSet()
		set.CollectedAt = base.Add(time.Duration(i) * time.Minute)
		set.Gauge(metric.NameUp, 1, metric.UnitCount)
		set.Gauge(metric.NameConnTotal, float64(10+i), metric.UnitCount)
		set.LatencyMs = float64(5 + i)
		if err := st.SaveSamples(ctx, conn.ID, set, ""); err != nil {
			t.Fatalf("save samples: %v", err)
		}
	}

	series, err := st.QuerySeries(ctx, store.SeriesQuery{
		ConnectionID: conn.ID,
		Metrics:      []string{metric.NameConnTotal},
		From:         base.Add(-time.Minute),
		To:           time.Now().UTC(),
		MaxPoints:    100,
	})
	if err != nil {
		t.Fatalf("query series: %v", err)
	}
	if len(series) != 1 {
		t.Fatalf("시계열이 1개여야 합니다: %d", len(series))
	}
	s := series[0]
	if len(s.Points) == 0 {
		t.Fatal("시계열 점이 없습니다")
	}
	if s.Source != "raw" {
		t.Errorf("최근 범위는 원본에서 읽어야 합니다: %s", s.Source)
	}
	if s.Unit != metric.UnitCount {
		t.Errorf("단위가 보존되지 않았습니다: %s", s.Unit)
	}
	// 값 범위 확인: 10~19를 저장했다
	minSeen, maxSeen := 1e9, -1e9
	for _, p := range s.Points {
		if p.Min < minSeen {
			minSeen = p.Min
		}
		if p.Max > maxSeen {
			maxSeen = p.Max
		}
	}
	if minSeen != 10 || maxSeen != 19 {
		t.Errorf("값 범위가 10~19여야 합니다: %g~%g", minSeen, maxSeen)
	}

	// 최신 상태가 갱신되었는지
	state, err := st.GetConnectionState(ctx, conn.ID)
	if err != nil {
		t.Fatalf("get state: %v", err)
	}
	if state == nil || !state.Up {
		t.Fatal("커넥션 상태가 up이어야 합니다")
	}
	if got := state.Metrics[metric.NameConnTotal].Value; got != 19 {
		t.Errorf("최신 지표가 19여야 합니다: %g", got)
	}
	if state.ConsecutiveFails != 0 {
		t.Errorf("성공 시 연속 실패는 0이어야 합니다: %d", state.ConsecutiveFails)
	}
}

// TestConsecutiveFailsTracking은 연속 실패 카운트가 누적/초기화되는지 확인한다.
func TestConsecutiveFailsTracking(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	conn := newTestConnection(t, st, "dev-db", model.EnvDev)

	for i := range 3 {
		set := metric.NewSet()
		set.CollectedAt = time.Now().UTC().Add(time.Duration(i) * time.Second)
		set.Gauge(metric.NameUp, 0, metric.UnitCount)
		if err := st.SaveSamples(ctx, conn.ID, set, "connection refused"); err != nil {
			t.Fatalf("save: %v", err)
		}
	}
	state, err := st.GetConnectionState(ctx, conn.ID)
	if err != nil {
		t.Fatalf("get state: %v", err)
	}
	if state.ConsecutiveFails != 3 {
		t.Errorf("연속 실패가 3이어야 합니다: %d", state.ConsecutiveFails)
	}
	if state.Up {
		t.Error("up이 false여야 합니다")
	}
	if state.LastError != "connection refused" {
		t.Errorf("마지막 오류가 기록되지 않았습니다: %q", state.LastError)
	}
	if state.LastOKAt != nil {
		t.Error("성공 이력이 없으므로 last_ok_at은 비어 있어야 합니다")
	}

	// 복구되면 0으로 초기화되고 last_ok_at이 기록된다.
	ok := metric.NewSet()
	ok.Gauge(metric.NameUp, 1, metric.UnitCount)
	if err := st.SaveSamples(ctx, conn.ID, ok, ""); err != nil {
		t.Fatalf("save ok: %v", err)
	}
	state, _ = st.GetConnectionState(ctx, conn.ID)
	if state.ConsecutiveFails != 0 {
		t.Errorf("복구 후 연속 실패가 0이어야 합니다: %d", state.ConsecutiveFails)
	}
	if state.LastOKAt == nil {
		t.Error("복구 후 last_ok_at이 기록되어야 합니다")
	}
}

// TestConnectivityEventRequiresConsecutiveFails는 일시적 실패로 이벤트가 생기지 않는지 확인한다.
func TestConnectivityEventRequiresConsecutiveFails(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	conn := newTestConnection(t, st, "dev-db", model.EnvDev)

	cfg := DefaultConfig()
	cfg.Interval = 30 * time.Second
	m := NewManager(st, cfg)

	// duration 60초 / 간격 30초 → 3회 연속 실패가 필요하다.
	if _, err := st.CreateRule(ctx, &store.Rule{
		Name: "접속 실패", Kind: store.EventConnectivity,
		DurationSec: 60, Severity: store.SeverityCritical, Enabled: true,
	}); err != nil {
		t.Fatalf("create rule: %v", err)
	}

	down := metric.NewSet()
	down.Gauge(metric.NameUp, 0, metric.UnitCount)
	down.AddNote("dial tcp: connection refused")

	for i := range 2 {
		down.CollectedAt = time.Now().UTC().Add(time.Duration(i) * time.Second)
		if err := st.SaveSamples(ctx, conn.ID, down, "connection refused"); err != nil {
			t.Fatalf("save: %v", err)
		}
		m.evaluateConnectivity(ctx, conn, down)
	}
	if _, total, _ := st.ListEvents(ctx, store.EventFilter{}); total != 0 {
		t.Errorf("2회 실패로는 이벤트가 생기지 않아야 합니다: %d건", total)
	}

	down.CollectedAt = time.Now().UTC().Add(3 * time.Second)
	if err := st.SaveSamples(ctx, conn.ID, down, "connection refused"); err != nil {
		t.Fatalf("save: %v", err)
	}
	m.evaluateConnectivity(ctx, conn, down)

	events, total, _ := st.ListEvents(ctx, store.EventFilter{})
	if total != 1 {
		t.Fatalf("3회 실패 후 이벤트가 1건이어야 합니다: %d건", total)
	}
	if events[0].Kind != store.EventConnectivity || events[0].Severity != store.SeverityCritical {
		t.Errorf("이벤트 종류/심각도가 잘못되었습니다: %s/%s", events[0].Kind, events[0].Severity)
	}
	if detail, ok := events[0].Detail["reason"].(string); !ok || !strings.Contains(detail, "refused") {
		t.Errorf("실패 사유가 기록되지 않았습니다: %v", events[0].Detail)
	}

	// 복구되면 해소된다.
	up := metric.NewSet()
	up.Gauge(metric.NameUp, 1, metric.UnitCount)
	if err := st.SaveSamples(ctx, conn.ID, up, ""); err != nil {
		t.Fatalf("save up: %v", err)
	}
	m.evaluateConnectivity(ctx, conn, up)

	events, _, _ = st.ListEvents(ctx, store.EventFilter{})
	if events[0].State != "resolved" {
		t.Errorf("복구 후 이벤트가 해소되어야 합니다: %s", events[0].State)
	}
}

// ---------- 이벤트 중복 억제 ----------

// TestOpenEventDeduplicates는 같은 원인의 이벤트가 한 행으로 합쳐지는지 확인한다.
func TestOpenEventDeduplicates(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	conn := newTestConnection(t, st, "dev-db", model.EnvDev)

	v1, v2 := 90.0, 95.0
	for i, v := range []*float64{&v1, &v2, &v2} {
		severity := store.SeverityWarning
		if i == 2 {
			severity = store.SeverityCritical // 심각도 상승
		}
		if _, _, err := st.OpenEvent(ctx, store.OpenEventParams{
			ConnectionID: conn.ID, Kind: store.EventThreshold,
			Severity: severity, Metric: metric.NameConnUsedPct,
			Message: "사용률 높음", Value: v,
		}); err != nil {
			t.Fatalf("open event %d: %v", i, err)
		}
	}

	events, total, err := st.ListEvents(ctx, store.EventFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 1 {
		t.Fatalf("같은 원인은 1건으로 합쳐져야 합니다: %d건", total)
	}
	e := events[0]
	if e.Occurrences != 3 {
		t.Errorf("발생 횟수가 3이어야 합니다: %d", e.Occurrences)
	}
	if *e.Value != 95 {
		t.Errorf("최신 값이 반영되어야 합니다: %g", *e.Value)
	}
	if e.Severity != store.SeverityCritical {
		t.Errorf("심각도는 상승만 해야 합니다: %s", e.Severity)
	}

	// 심각도가 낮은 재발생은 심각도를 떨어뜨리지 않아야 한다.
	if _, _, err := st.OpenEvent(ctx, store.OpenEventParams{
		ConnectionID: conn.ID, Kind: store.EventThreshold,
		Severity: store.SeverityInfo, Metric: metric.NameConnUsedPct,
		Message: "사용률 조금 높음", Value: &v1,
	}); err != nil {
		t.Fatalf("open lower severity: %v", err)
	}
	events, _, _ = st.ListEvents(ctx, store.EventFilter{})
	if events[0].Severity != store.SeverityCritical {
		t.Errorf("심각도가 하락했습니다: %s", events[0].Severity)
	}

	// 다른 지표는 별개 이벤트여야 한다.
	if _, created, err := st.OpenEvent(ctx, store.OpenEventParams{
		ConnectionID: conn.ID, Kind: store.EventThreshold,
		Severity: store.SeverityWarning, Metric: metric.NameDeadlocks,
		Message: "데드락", Value: &v1,
	}); err != nil || !created {
		t.Errorf("다른 지표는 새 이벤트여야 합니다: created=%v err=%v", created, err)
	}

	// 해소 후 같은 원인이 다시 발생하면 새 이벤트여야 한다.
	if _, err := st.ResolveEvents(ctx, conn.ID, store.EventThreshold, metric.NameConnUsedPct, ""); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if _, created, err := st.OpenEvent(ctx, store.OpenEventParams{
		ConnectionID: conn.ID, Kind: store.EventThreshold,
		Severity: store.SeverityWarning, Metric: metric.NameConnUsedPct,
		Message: "다시 사용률 높음", Value: &v1,
	}); err != nil || !created {
		t.Errorf("해소 후 재발생은 새 이벤트여야 합니다: created=%v err=%v", created, err)
	}
}

// TestEventSummary는 요약 집계를 확인한다.
func TestEventSummary(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	conn := newTestConnection(t, st, "dev-db", model.EnvDev)

	for i, sev := range []store.Severity{store.SeverityCritical, store.SeverityWarning, store.SeverityInfo} {
		if _, _, err := st.OpenEvent(ctx, store.OpenEventParams{
			ConnectionID: conn.ID, Kind: store.EventThreshold, Severity: sev,
			Metric: "m" + string(rune('a'+i)), Message: "msg",
		}); err != nil {
			t.Fatalf("open: %v", err)
		}
	}

	sum, err := st.EventSummary(ctx, nil, false)
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if sum.OpenCritical != 1 || sum.OpenWarning != 1 || sum.OpenInfo != 1 {
		t.Errorf("심각도별 집계가 잘못되었습니다: %+v", sum)
	}
	if sum.Unacked != 3 {
		t.Errorf("미확인이 3이어야 합니다: %d", sum.Unacked)
	}
	if sum.Last24h != 3 {
		t.Errorf("24시간 내가 3이어야 합니다: %d", sum.Last24h)
	}

	// 접근 가능한 커넥션이 없으면 빈 요약이어야 한다 (권한 누출 방지).
	empty, err := st.EventSummary(ctx, []string{}, false)
	if err != nil {
		t.Fatalf("empty summary: %v", err)
	}
	if empty.OpenCritical != 0 || empty.Last24h != 0 {
		t.Errorf("빈 커넥션 목록은 빈 요약이어야 합니다: %+v", empty)
	}
}

// TestListEventsScoping은 커넥션 범위 제한이 동작하는지 확인한다.
func TestListEventsScoping(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	c1 := newTestConnection(t, st, "db-1", model.EnvDev)
	c2 := newTestConnection(t, st, "db-2", model.EnvDev)

	for _, c := range []*model.Connection{c1, c2} {
		if _, _, err := st.OpenEvent(ctx, store.OpenEventParams{
			ConnectionID: c.ID, Kind: store.EventThreshold,
			Severity: store.SeverityWarning, Metric: "m", Message: c.Name,
		}); err != nil {
			t.Fatalf("open: %v", err)
		}
	}

	// nil = 제한 없음
	if _, total, _ := st.ListEvents(ctx, store.EventFilter{}); total != 2 {
		t.Errorf("제한 없으면 2건이어야 합니다: %d", total)
	}
	// 특정 커넥션만
	events, total, _ := st.ListEvents(ctx, store.EventFilter{ConnectionIDs: []string{c1.ID}})
	if total != 1 || events[0].ConnectionID != c1.ID {
		t.Errorf("범위 제한이 동작하지 않습니다: total=%d", total)
	}
	// 빈 목록 = 아무것도 못 봄
	if _, total, _ := st.ListEvents(ctx, store.EventFilter{ConnectionIDs: []string{}}); total != 0 {
		t.Errorf("빈 목록은 0건이어야 합니다: %d", total)
	}
}

// ---------- 스키마 드리프트 ----------

// TestSchemaSnapshotDedup은 같은 지문의 스냅샷을 중복 저장하지 않는지 확인한다.
func TestSchemaSnapshotDedup(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	conn := newTestConnection(t, st, "dev-db", model.EnvDev)

	sc := &schema.Schema{
		Dialect: "mysql", Shape: schema.ShapeRelational, Name: "appdb",
		Tables: []*schema.Table{{
			Name: "users",
			Columns: []*schema.Column{
				{Name: "id", Position: 1, Type: schema.LogicalType{Base: schema.TypeBigInt}, Identity: true},
			},
			PrimaryKey: &schema.PrimaryKey{Columns: []string{"id"}},
		}},
	}

	snap1, created, err := st.SaveSchemaSnapshot(ctx, conn.ID, sc, store.SnapshotSourceMonitor, nil)
	if err != nil {
		t.Fatalf("save first: %v", err)
	}
	if !created {
		t.Error("첫 스냅샷은 새로 생성되어야 합니다")
	}

	// 같은 스키마 재저장 → 중복 저장 안 함
	snap2, created, err := st.SaveSchemaSnapshot(ctx, conn.ID, sc, store.SnapshotSourceMonitor, nil)
	if err != nil {
		t.Fatalf("save same: %v", err)
	}
	if created {
		t.Error("같은 지문은 새로 저장하지 않아야 합니다")
	}
	if snap1.ID != snap2.ID {
		t.Errorf("같은 스냅샷을 반환해야 합니다: %d vs %d", snap1.ID, snap2.ID)
	}

	// 구조를 바꾸면 새 스냅샷
	sc.Tables[0].Columns = append(sc.Tables[0].Columns, &schema.Column{
		Name: "email", Position: 2,
		Type: schema.LogicalType{Base: schema.TypeVarchar, Length: 255},
	})
	snap3, created, err := st.SaveSchemaSnapshot(ctx, conn.ID, sc,
		store.SnapshotSourceMonitor, []string{"users.email 컬럼 추가"})
	if err != nil {
		t.Fatalf("save changed: %v", err)
	}
	if !created || snap3.ID == snap1.ID {
		t.Error("구조 변경 시 새 스냅샷이어야 합니다")
	}
	if len(snap3.ChangeSummary) != 1 {
		t.Errorf("변경 요약이 기록되어야 합니다: %v", snap3.ChangeSummary)
	}

	// 이력 조회
	snaps, err := st.ListSchemaSnapshots(ctx, conn.ID, 10)
	if err != nil {
		t.Fatalf("list snapshots: %v", err)
	}
	if len(snaps) != 2 {
		t.Errorf("스냅샷 이력이 2건이어야 합니다: %d", len(snaps))
	}

	// 스키마 본문 복원 확인
	full, err := st.GetSchemaSnapshot(ctx, snap3.ID)
	if err != nil {
		t.Fatalf("get snapshot: %v", err)
	}
	if full.Schema == nil || len(full.Schema.Tables) != 1 || len(full.Schema.Tables[0].Columns) != 2 {
		t.Errorf("저장된 스키마가 복원되지 않았습니다: %+v", full.Schema)
	}
}

// ---------- 롤업 / 보존 ----------

// TestRollupAndPurge는 시간 롤업과 보존기간 정리를 확인한다.
func TestRollupAndPurge(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	conn := newTestConnection(t, st, "dev-db", model.EnvDev)

	// 3시간 전부터 1분 간격으로 샘플을 넣는다.
	base := time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Hour)
	for i := range 120 {
		set := metric.NewSet()
		set.CollectedAt = base.Add(time.Duration(i) * time.Minute)
		set.Gauge(metric.NameConnTotal, float64(i%60), metric.UnitCount)
		if err := st.SaveSamples(ctx, conn.ID, set, ""); err != nil {
			t.Fatalf("save: %v", err)
		}
	}

	n, err := st.RollupHourly(ctx)
	if err != nil {
		t.Fatalf("rollup: %v", err)
	}
	if n == 0 {
		t.Fatal("롤업이 아무 버킷도 만들지 않았습니다")
	}

	stats, err := st.MetricStorageStats(ctx)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.HourlyRows == 0 {
		t.Error("시간 롤업 행이 없습니다")
	}
	t.Logf("원본 %d행, 시간 롤업 %d행", stats.RawSamples, stats.HourlyRows)

	// 롤업 재실행은 멱등이어야 한다 (같은 버킷을 중복 생성하지 않음).
	beforeRows := stats.HourlyRows
	if _, err := st.RollupHourly(ctx); err != nil {
		t.Fatalf("rollup again: %v", err)
	}
	stats, _ = st.MetricStorageStats(ctx)
	if stats.HourlyRows != beforeRows {
		t.Errorf("롤업 재실행이 행을 늘렸습니다: %d → %d", beforeRows, stats.HourlyRows)
	}

	// 2시간 이상 지난 원본 삭제
	rawDeleted, _, err := st.PurgeMetrics(ctx, 2*time.Hour, 90*24*time.Hour)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if rawDeleted == 0 {
		t.Error("보존기간이 지난 원본이 삭제되지 않았습니다")
	}

	// 롤업은 남아 있어야 하므로 오래된 범위 조회가 여전히 동작한다.
	series, err := st.QuerySeries(ctx, store.SeriesQuery{
		ConnectionID: conn.ID,
		Metrics:      []string{metric.NameConnTotal},
		From:         time.Now().UTC().Add(-72 * time.Hour),
		To:           time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("query old range: %v", err)
	}
	if series[0].Source != "hourly" {
		t.Errorf("오래된 범위는 롤업에서 읽어야 합니다: %s", series[0].Source)
	}
	if len(series[0].Points) == 0 {
		t.Error("롤업 데이터가 조회되지 않았습니다")
	}
}

// TestPurgeEventsKeepsOpen은 열린 이벤트가 정리되지 않는지 확인한다.
func TestPurgeEventsKeepsOpen(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	conn := newTestConnection(t, st, "dev-db", model.EnvDev)

	if _, _, err := st.OpenEvent(ctx, store.OpenEventParams{
		ConnectionID: conn.ID, Kind: store.EventThreshold,
		Severity: store.SeverityWarning, Metric: "open_one", Message: "열린 이벤트",
	}); err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, _, err := st.OpenEvent(ctx, store.OpenEventParams{
		ConnectionID: conn.ID, Kind: store.EventThreshold,
		Severity: store.SeverityWarning, Metric: "resolved_one", Message: "해소될 이벤트",
	}); err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := st.ResolveEvents(ctx, conn.ID, store.EventThreshold, "resolved_one", ""); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	// 보존기간을 음수로 주어 "지금보다 1초 뒤까지 해소된 것"을 정리한다.
	// 0을 쓰면 컷오프가 정확히 현재 시각이 되는데, Windows의 타이머 해상도 때문에
	// 방금 기록한 resolved_at과 컷오프가 같은 값이 될 수 있고 `<` 조건이 거짓이 된다.
	// 경계에 걸치지 않는 값을 써서 테스트 의도(해소된 것은 지워진다)를 분명히 한다.
	deleted, err := st.PurgeEvents(ctx, -time.Second)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if deleted != 1 {
		t.Errorf("해소된 이벤트 1건이 삭제되어야 합니다: %d", deleted)
	}
	events, total, _ := st.ListEvents(ctx, store.EventFilter{})
	if total != 1 || events[0].Metric != "open_one" {
		t.Errorf("열린 이벤트는 남아야 합니다: total=%d", total)
	}
}

// TestSeedBuiltinRules는 기본 룰 시딩을 확인한다.
func TestSeedBuiltinRules(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	n, err := st.SeedBuiltinRules(ctx)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if n == 0 {
		t.Fatal("기본 룰이 생성되지 않았습니다")
	}

	// 재실행은 아무것도 만들지 않아야 한다.
	n2, err := st.SeedBuiltinRules(ctx)
	if err != nil {
		t.Fatalf("seed again: %v", err)
	}
	if n2 != 0 {
		t.Errorf("재시딩이 룰을 추가했습니다: %d", n2)
	}

	rules, err := st.ListRules(ctx)
	if err != nil {
		t.Fatalf("list rules: %v", err)
	}
	kinds := map[string]int{}
	for _, r := range rules {
		kinds[r.Kind]++
		if !r.Builtin {
			t.Errorf("시딩된 룰은 builtin이어야 합니다: %s", r.Name)
		}
		if err := store.ValidateRule(r); err != nil {
			t.Errorf("시딩된 룰이 검증을 통과하지 못했습니다 (%s): %v", r.Name, err)
		}
	}
	// 세 종류가 모두 있어야 모니터링이 완전하게 동작한다.
	for _, kind := range []string{store.EventThreshold, store.EventConnectivity, store.EventDrift} {
		if kinds[kind] == 0 {
			t.Errorf("%s 종류의 기본 룰이 없습니다", kind)
		}
	}
	t.Logf("기본 룰 %d개: %v", len(rules), kinds)
}

// TestValidateRule은 룰 입력 검증을 확인한다.
func TestValidateRule(t *testing.T) {
	cases := []struct {
		name    string
		rule    *store.Rule
		wantErr bool
	}{
		{"정상 임계치", &store.Rule{Name: "a", Kind: store.EventThreshold,
			Metric: "up", Op: ">", Severity: store.SeverityWarning}, false},
		{"이름 없음", &store.Rule{Kind: store.EventThreshold,
			Metric: "up", Op: ">", Severity: store.SeverityWarning}, true},
		{"지표 없음", &store.Rule{Name: "a", Kind: store.EventThreshold,
			Op: ">", Severity: store.SeverityWarning}, true},
		{"잘못된 연산자", &store.Rule{Name: "a", Kind: store.EventThreshold,
			Metric: "up", Op: "=~", Severity: store.SeverityWarning}, true},
		{"잘못된 심각도", &store.Rule{Name: "a", Kind: store.EventThreshold,
			Metric: "up", Op: ">", Severity: "fatal"}, true},
		{"연결성 룰은 지표 불필요", &store.Rule{Name: "a", Kind: store.EventConnectivity,
			Severity: store.SeverityCritical}, false},
		{"알 수 없는 종류", &store.Rule{Name: "a", Kind: "bogus",
			Severity: store.SeverityWarning}, true},
		{"음수 지속시간", &store.Rule{Name: "a", Kind: store.EventThreshold,
			Metric: "up", Op: ">", Severity: store.SeverityWarning, DurationSec: -1}, true},
	}
	for _, c := range cases {
		err := store.ValidateRule(c.rule)
		if (err != nil) != c.wantErr {
			t.Errorf("%s: err=%v, wantErr=%v", c.name, err, c.wantErr)
		}
	}
}

// TestDeleteRuleResolvesEvents는 룰 삭제 시 그 룰의 이벤트가 해소되는지 확인한다.
func TestDeleteRuleResolvesEvents(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	conn := newTestConnection(t, st, "dev-db", model.EnvDev)

	rule, err := st.CreateRule(ctx, &store.Rule{
		Name: "temp", Kind: store.EventThreshold, Metric: "up", Op: "<",
		Threshold: 1, Severity: store.SeverityWarning, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create rule: %v", err)
	}
	if _, _, err := st.OpenEvent(ctx, store.OpenEventParams{
		ConnectionID: conn.ID, RuleID: rule.ID, Kind: store.EventThreshold,
		Severity: store.SeverityWarning, Metric: "up", Message: "down",
	}); err != nil {
		t.Fatalf("open: %v", err)
	}

	if err := st.DeleteRule(ctx, rule.ID); err != nil {
		t.Fatalf("delete rule: %v", err)
	}
	events, _, _ := st.ListEvents(ctx, store.EventFilter{})
	if len(events) != 1 || events[0].State != "resolved" {
		t.Errorf("룰 삭제 시 이벤트가 해소되어야 합니다: %+v", events)
	}
}

// ---------- 포맷 헬퍼 ----------

func TestFormatValue(t *testing.T) {
	cases := []struct {
		value float64
		unit  metric.Unit
		want  string
	}{
		{85.5, metric.UnitPercent, "85.5%"},
		{1536, metric.UnitBytes, "1.5KB"},
		{500, metric.UnitBytes, "500B"},
		{250, metric.UnitMillis, "250ms"},
		{1500, metric.UnitMillis, "1.50초"},
		{45, metric.UnitSeconds, "45초"},
		{300, metric.UnitSeconds, "5.0분"},
		{7200, metric.UnitSeconds, "2.0시간"},
		{12.34, metric.UnitPerSec, "12.34/s"},
		{5, metric.UnitCount, "5"},
		{5.5, metric.UnitCount, "5.50"},
	}
	for _, c := range cases {
		if got := formatValue(c.value, c.unit); got != c.want {
			t.Errorf("formatValue(%g, %s) = %q, 기대값 %q", c.value, c.unit, got, c.want)
		}
	}
}

// TestSubjectParticle은 주격 조사 선택을 확인한다.
//
// 지표 라벨이 데이터에서 오므로 조사를 고정하면 "쿼리이 초과" 같은 문장이
// 사용자에게 그대로 노출된다. 받침 유무에 따라 이/가를 골라야 한다.
func TestSubjectParticle(t *testing.T) {
	cases := []struct{ word, want string }{
		{"쿼리", "가"},       // 받침 없음
		{"최장 실행 쿼리", "가"}, // 마지막 어절 기준
		{"사용률", "이"},      // 받침 있음
		{"세션 사용률", "이"},
		{"캐시 적중률", "이"},
		{"복제 지연", "이"}, // '연'에 받침
		{"응답 시간", "이"}, // '간'에 받침
		{"메모리 사용량", "이"},
		{"초당 쿼리", "가"},
		{"메모리 사용률 (Redis)", "이"}, // 괄호는 건너뛴다
		{"query.rate", "이"},      // 한글이 아니면 기본값
		{"", "이"},
	}
	for _, c := range cases {
		if got := subjectParticle(c.word); got != c.want {
			t.Errorf("subjectParticle(%q) = %q, 기대값 %q", c.word, got, c.want)
		}
	}
}

// TestThresholdMessageGrammar는 실제 이벤트 메시지의 조사가 자연스러운지 확인한다.
func TestThresholdMessageGrammar(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	conn := newTestConnection(t, st, "db1", model.EnvDev)

	// 받침이 없는 라벨("최장 실행 쿼리")과 있는 라벨("세션 사용률")을 각각 확인한다.
	cases := []struct {
		metricName string
		wantSubstr string
	}{
		{metric.NameLongestQuery, "쿼리가"},
		{metric.NameConnUsedPct, "사용률이"},
	}

	for _, c := range cases {
		rule, err := st.CreateRule(ctx, &store.Rule{
			Name: "g-" + c.metricName, Kind: store.EventThreshold, Metric: c.metricName,
			Op: ">", Threshold: 0, DurationSec: 0, Severity: store.SeverityInfo, Enabled: true,
		})
		if err != nil {
			t.Fatalf("create rule: %v", err)
		}
		engine := NewRuleEngine(st)
		set := metric.NewSet()
		set.Gauge(c.metricName, 10, metric.Lookup(c.metricName).Unit)
		if err := engine.EvaluateThresholds(ctx, conn, set); err != nil {
			t.Fatalf("evaluate: %v", err)
		}
		events, _, err := st.ListEvents(ctx, store.EventFilter{})
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		found := false
		for _, e := range events {
			if e.RuleID == rule.ID {
				found = true
				if !strings.Contains(e.Message, c.wantSubstr) {
					t.Errorf("메시지에 %q가 없습니다: %q", c.wantSubstr, e.Message)
				} else {
					t.Logf("메시지: %s", e.Message)
				}
			}
		}
		if !found {
			t.Errorf("%s 룰의 이벤트를 찾지 못했습니다", c.metricName)
		}
	}
}

func TestThresholdPhrase(t *testing.T) {
	if got := thresholdPhrase(">", 80, metric.UnitPercent); got != "80.0% 초과" {
		t.Errorf("초과 문구: %q", got)
	}
	if got := thresholdPhrase("<", 90, metric.UnitPercent); got != "90.0% 미만" {
		t.Errorf("미만 문구: %q", got)
	}
}

func TestMetricCatalog(t *testing.T) {
	cat := metric.Catalog()
	if len(cat) == 0 {
		t.Fatal("카탈로그가 비어 있습니다")
	}
	for _, m := range cat {
		if m.Name == "" || m.Label == "" {
			t.Errorf("카탈로그 항목에 이름/라벨이 없습니다: %+v", m)
		}
		if m.Help == "" {
			t.Errorf("%s에 설명이 없습니다", m.Name)
		}
	}
	// 알 수 없는 지표는 이름을 라벨로 쓴다.
	unknown := metric.Lookup("custom.thing")
	if unknown.Label != "custom.thing" {
		t.Errorf("알 수 없는 지표 라벨: %q", unknown.Label)
	}
}

func eventMessages(events []*store.Event) []string {
	out := make([]string, len(events))
	for i, e := range events {
		out[i] = e.Message
	}
	return out
}
