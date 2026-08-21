package monitor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"dbstudio/internal/applog"
	"dbstudio/internal/dbx"
	"dbstudio/internal/metric"
	"dbstudio/internal/model"
	"dbstudio/internal/store"
)

// Config는 모니터링 동작 설정이다.
type Config struct {
	// Interval은 지표 수집 주기다.
	Interval time.Duration
	// SchemaInterval은 스키마 드리프트 확인 주기다. 지표보다 훨씬 드물게 한다 —
	// introspect는 카탈로그를 여러 번 조회하므로 지표 수집보다 훨씬 비싸다.
	SchemaInterval time.Duration
	// Timeout은 한 번의 수집에 허용하는 최대 시간이다.
	Timeout time.Duration
	// RawRetention은 원본 샘플 보존기간이다.
	RawRetention time.Duration
	// HourlyRetention은 시간 롤업 보존기간이다.
	HourlyRetention time.Duration
	// EventRetention은 해소된 이벤트 보존기간이다.
	EventRetention time.Duration
	// SnapshotsPerConnection은 커넥션별로 보관할 스키마 스냅샷 수다.
	SnapshotsPerConnection int
	// MaxConcurrent는 동시에 폴링할 커넥션 수 상한이다.
	// 커넥션이 수십 개일 때 한꺼번에 접속하면 앱과 DB 양쪽에 부담이 된다.
	MaxConcurrent int
}

func DefaultConfig() Config {
	return Config{
		Interval:               30 * time.Second,
		SchemaInterval:         15 * time.Minute,
		Timeout:                20 * time.Second,
		RawRetention:           48 * time.Hour,
		HourlyRetention:        90 * 24 * time.Hour,
		EventRetention:         30 * 24 * time.Hour,
		SnapshotsPerConnection: 50,
		MaxConcurrent:          8,
	}
}

// Manager는 폴링 루프를 관리한다.
//
// 커넥션별로 goroutine을 띄우지 않고 단일 루프에서 전체를 순회하는 이유:
// 커넥션 목록이 런타임에 바뀌므로(추가/삭제/비활성화) goroutine 생명주기를
// 목록 변화에 맞춰 관리하면 누수와 경쟁이 생기기 쉽다. 한 루프가 매 주기마다
// 현재 목록을 읽어 처리하면 목록 변화가 자동으로 반영된다.
// EventSink는 새로 생긴 이벤트를 알린다.
//
// 인터페이스로 두고 구현을 모르게 하는 이유: 이벤트에 반응하는 것은 모니터링의 일이
// 아니다. 지금은 매크로 자동 실행이 이것을 구현하지만, 모니터가 매크로를 알게 되면
// 두 기능이 서로를 붙잡아 어느 쪽도 따로 시험할 수 없게 된다.
type EventSink interface {
	EventOpened(ctx context.Context, ev *store.Event)
}

// ResolveSink는 "그 문제가 끝났다"까지 알고 싶은 수신자가 함께 구현한다.
//
// EventSink에 합치지 않고 선택적 인터페이스로 둔 이유: 매크로 자동 실행은 해소에
// 반응할 이유가 없다(무언가를 실행하는 방아쇠는 문제가 생겼을 때다). 반대로 메신저
// 알림은 해소를 알려야 채널만 보고 지금 상태를 알 수 있다. 필요 없는 쪽에 빈 메서드를
// 강요하지 않는다.
type ResolveSink interface {
	EventResolved(ctx context.Context, ev *store.Event)
}

// FanOut은 여러 수신자에게 같은 이벤트를 전한다.
//
// 수신자를 하나만 둘 수 없는 이유: 이벤트는 매크로를 깨우기도 하고 메신저로 나가기도
// 한다. 둘은 서로를 몰라야 하며(한쪽이 느리다고 다른 쪽이 막혀서도 안 된다),
// 그 조합을 아는 것은 부팅 절차의 몫이다.
type FanOut []EventSink

func (f FanOut) EventOpened(ctx context.Context, ev *store.Event) {
	for _, sink := range f {
		if sink != nil {
			sink.EventOpened(ctx, ev)
		}
	}
}

func (f FanOut) EventResolved(ctx context.Context, ev *store.Event) {
	for _, sink := range f {
		if rs, ok := sink.(ResolveSink); ok {
			rs.EventResolved(ctx, ev)
		}
	}
}

type Manager struct {
	st     *store.Store
	engine *RuleEngine
	cfg    Config
	sink   EventSink

	mu sync.Mutex
	// counters는 누적 카운터의 이전 값이다. 변화율 계산에 쓴다.
	counters map[string]counterSample
	// lastSchemaCheck는 커넥션별 마지막 드리프트 확인 시각이다.
	lastSchemaCheck map[string]time.Time
	// pollNow는 즉시 폴링 요청 채널이다. 커넥션이 추가되면 기다리지 않고 수집한다.
	pollNow chan string
	running bool
}

type counterSample struct {
	value float64
	at    time.Time
}

func NewManager(st *store.Store, cfg Config) *Manager {
	return &Manager{
		st:              st,
		engine:          NewRuleEngine(st),
		cfg:             cfg,
		counters:        map[string]counterSample{},
		lastSchemaCheck: map[string]time.Time{},
		pollNow:         make(chan string, 32),
	}
}

func (m *Manager) Engine() *RuleEngine { return m.engine }
func (m *Manager) Config() Config      { return m.cfg }

// SetEventSink는 이벤트 수신자를 등록한다. 부팅 시 한 번 부른다.
func (m *Manager) SetEventSink(s EventSink) {
	m.sink = s
	m.engine.sink = s
}

// notifyEvent는 새 이벤트를 수신자에게 알린다.
//
// created가 false면 알리지 않는다. 같은 원인이 반복되어 occurrences만 오르는 경우까지
// 알리면, 지표가 임계치 근처에서 흔들릴 때마다 자동 실행이 걸린다.
//
// 고루틴으로 넘기는 이유: 수신자가 매크로를 시작할 수도 있고 그것은 폴링보다 느리다.
// 폴러가 그 시간을 기다리면 다음 수집 주기가 밀린다.
func notifyEvent(ctx context.Context, sink EventSink, st *store.Store, id int64, created bool) {
	if sink == nil || !created || id <= 0 {
		return
	}
	ev, err := st.GetEvent(ctx, id)
	if err != nil {
		slog.Error("이벤트를 다시 읽지 못해 알리지 못했습니다", "event", id, "err", err)
		return
	}
	// 폴링 컨텍스트가 끝나도 수신자의 일은 이어져야 한다.
	go sink.EventOpened(context.WithoutCancel(ctx), ev)
}

// notifyResolved는 닫힌 이벤트를 수신자에게 알린다.
//
// 수신자가 ResolveSink를 구현하지 않았으면 아무 일도 하지 않는다.
func notifyResolved(ctx context.Context, sink EventSink, events []*store.Event) {
	rs, ok := sink.(ResolveSink)
	if !ok || len(events) == 0 {
		return
	}
	// 폴링 컨텍스트가 끝나도 알림은 나가야 한다(전송은 폴링보다 느리다).
	out := context.WithoutCancel(ctx)
	for _, ev := range events {
		go rs.EventResolved(out, ev)
	}
}

// TriggerPoll은 특정 커넥션을 즉시 폴링하도록 요청한다.
// 커넥션 등록 직후 대시보드가 빈 상태로 보이지 않게 하기 위함이다.
// 채널이 가득 차면 다음 주기에 처리되므로 조용히 버린다.
func (m *Manager) TriggerPoll(connectionID string) {
	select {
	case m.pollNow <- connectionID:
	default:
	}
}

// Run은 폴링 루프를 시작한다. ctx가 취소되면 반환한다.
func (m *Manager) Run(ctx context.Context) {
	m.mu.Lock()
	if m.running {
		m.mu.Unlock()
		return
	}
	m.running = true
	m.mu.Unlock()

	slog.Info("모니터링 폴러 시작",
		"interval", m.cfg.Interval, "schemaInterval", m.cfg.SchemaInterval)

	ticker := time.NewTicker(m.cfg.Interval)
	defer ticker.Stop()

	// 유지보수(롤업/보존 정리)는 폴링보다 훨씬 드물게 돌린다.
	maintenance := time.NewTicker(10 * time.Minute)
	defer maintenance.Stop()

	// 시작 직후 한 번 수집해 대시보드가 즉시 데이터를 갖게 한다.
	m.pollAll(ctx)

	for {
		select {
		case <-ctx.Done():
			slog.Info("모니터링 폴러 종료")
			return
		case <-ticker.C:
			m.pollAll(ctx)
		case connID := <-m.pollNow:
			m.pollOneByID(ctx, connID)
		case <-maintenance.C:
			m.maintain(ctx)
		}
	}
}

// pollAll은 활성 커넥션 전체를 폴링한다.
func (m *Manager) pollAll(ctx context.Context) {
	conns, err := m.st.ListConnections(ctx)
	if err != nil {
		slog.Error("커넥션 목록 조회 실패", "err", err)
		return
	}

	// 동시 실행 수를 세마포어로 제한한다.
	sem := make(chan struct{}, max(1, m.cfg.MaxConcurrent))
	var wg sync.WaitGroup

	for _, conn := range conns {
		if !conn.Enabled {
			continue
		}
		adapter, err := dbx.Get(conn.Kind)
		if err != nil || !adapter.Capabilities().Monitor {
			continue
		}

		wg.Add(1)
		go func(conn *model.Connection, adapter dbx.Adapter) {
			// 커넥션 하나에서 패닉이 나도 폴러 전체(=프로세스)가 죽지 않게 한다.
			// 드라이버는 예상 못한 응답에 패닉할 수 있고, 그 대가가 서버 종료여서는 안 된다.
			defer applog.Recover("monitor.poll:" + conn.Name)
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			m.pollOne(ctx, conn, adapter)
		}(conn, adapter)
	}
	wg.Wait()
}

func (m *Manager) pollOneByID(ctx context.Context, connectionID string) {
	conn, err := m.st.GetConnection(ctx, connectionID)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			slog.Error("커넥션 조회 실패", "id", connectionID, "err", err)
		}
		return
	}
	if !conn.Enabled {
		return
	}
	adapter, err := dbx.Get(conn.Kind)
	if err != nil || !adapter.Capabilities().Monitor {
		return
	}
	m.pollOne(ctx, conn, adapter)
}

// pollOne은 한 커넥션의 지표를 수집하고 룰을 평가한다.
func (m *Manager) pollOne(ctx context.Context, conn *model.Connection, adapter dbx.Adapter) {
	secret, err := m.st.GetSecret(ctx, conn.ID)
	if err != nil {
		slog.Error("자격증명 복호화 실패", "connection", conn.Name, "err", err)
		return
	}

	pollCtx, cancel := context.WithTimeout(ctx, m.cfg.Timeout)
	defer cancel()

	target := dbx.Target{Conn: conn, Secret: secret}
	set, err := adapter.Metrics(pollCtx, target)
	if err != nil {
		// 설정 오류 등으로 수집 자체가 불가능한 경우다.
		// up=0 샘플을 만들어 상태 화면에 이유를 표시한다.
		set = metric.NewSet()
		set.Gauge(metric.NameUp, 0, metric.UnitCount)
		set.AddNote("지표 수집 불가: %v", err)
	}

	// 누적 카운터를 초당 변화율로 바꾼다.
	m.deriveRates(conn.ID, set)

	reachable := true
	if up, ok := set.Get(metric.NameUp); ok {
		reachable = up.Value != 0
	}
	lastError := ""
	if !reachable {
		if len(set.Notes) > 0 {
			lastError = set.Notes[0]
		} else {
			lastError = "접속 실패"
		}
	}

	// 커넥션 화면의 연결 상태도 여기서 갱신한다.
	//
	// 지금까지 이 값을 쓰는 것은 "테스트" 버튼뿐이었다. 모니터가 30초마다 같은
	// 것을 확인하고 있는데도 DB 커넥션 화면은 사람이 눌러 주기 전까지 영원히
	// "확인 전"이었다 — 모니터링 화면은 전부 초록인데 커넥션 화면만 회색이었다.
	if err := m.st.RecordConnectionCheck(ctx, conn.ID, reachable, lastError); err != nil {
		slog.Warn("연결 상태 기록 실패", "connection", conn.Name, "err", err)
	}

	if err := m.st.SaveSamples(ctx, conn.ID, set, lastError); err != nil {
		slog.Error("지표 저장 실패", "connection", conn.Name, "err", err)
		return
	}

	m.evaluateConnectivity(ctx, conn, set)

	if err := m.engine.EvaluateThresholds(ctx, conn, set); err != nil {
		slog.Error("룰 평가 실패", "connection", conn.Name, "err", err)
	}

	m.maybeCheckDrift(ctx, conn, adapter, target)
}

// deriveRates는 누적 카운터를 초당 변화율로 변환한다.
//
// 카운터가 감소하면 서버가 재시작된 것이므로 그 구간의 변화율은 계산하지 않는다.
// 음수 변화율을 그대로 저장하면 차트에 스파이크가 생기고 룰이 잘못 발동한다.
func (m *Manager) deriveRates(connID string, set *metric.Set) {
	now := set.CollectedAt
	out := make([]metric.Sample, 0, len(set.Samples))

	m.mu.Lock()
	defer m.mu.Unlock()

	for _, sm := range set.Samples {
		if sm.Kind != metric.Counter {
			out = append(out, sm)
			continue
		}
		key := connID + "\x00" + sm.Name
		prev, ok := m.counters[key]
		m.counters[key] = counterSample{value: sm.Value, at: now}

		if !ok {
			// 첫 관측에는 비교 대상이 없다. 0으로 채우면 실제로 트래픽이 없는 것과
			// 구분되지 않으므로 이 샘플은 저장하지 않는다.
			continue
		}
		elapsed := now.Sub(prev.at).Seconds()
		if elapsed <= 0 || sm.Value < prev.value {
			continue
		}
		out = append(out, metric.Sample{
			Name:  sm.Name,
			Value: (sm.Value - prev.value) / elapsed,
			Kind:  metric.Gauge,
			Unit:  metric.UnitPerSec,
		})
	}
	set.Samples = out
}

// evaluateConnectivity는 접속 실패/복구 이벤트를 관리한다.
//
// 연속 실패 횟수 기반으로 판정한다: 한 번의 일시적 실패로 알림을 내면
// 네트워크 순간 단절마다 이벤트가 쌓인다. 룰의 duration_sec을 폴링 간격으로 나눠
// 필요한 연속 실패 횟수를 구한다.
func (m *Manager) evaluateConnectivity(ctx context.Context, conn *model.Connection, set *metric.Set) {
	rule := m.engine.connectivityRule(ctx, conn)
	if rule == nil {
		return
	}

	up, ok := set.Get(metric.NameUp)
	isUp := ok && up.Value > 0

	if isUp {
		closed, err := m.st.ResolveEvents(ctx, conn.ID, store.EventConnectivity, "", rule.ID)
		if err != nil {
			slog.Error("접속 이벤트 해소 실패", "connection", conn.Name, "err", err)
		}
		notifyResolved(ctx, m.sink, closed)
		return
	}

	state, err := m.st.GetConnectionState(ctx, conn.ID)
	if err != nil {
		slog.Error("커넥션 상태 조회 실패", "connection", conn.Name, "err", err)
		return
	}
	fails := 1
	if state != nil {
		fails = state.ConsecutiveFails
	}

	needed := 1
	if rule.DurationSec > 0 && m.cfg.Interval > 0 {
		needed = int(time.Duration(rule.DurationSec)*time.Second/m.cfg.Interval) + 1
	}
	if fails < needed {
		// 아직 지속 조건을 만족하지 않았다. 상태 화면에는 이미 up=0으로 보인다.
		return
	}

	reason := "접속 실패"
	if len(set.Notes) > 0 {
		reason = set.Notes[0]
	}
	value := float64(fails)
	eventID, created, err := m.st.OpenEvent(ctx, store.OpenEventParams{
		ConnectionID: conn.ID,
		RuleID:       rule.ID,
		Kind:         store.EventConnectivity,
		Severity:     rule.Severity,
		Message:      fmt.Sprintf("%s: 접속할 수 없습니다 (연속 %d회 실패)", conn.Name, fails),
		Value:        &value,
		Detail: map[string]any{
			"connection":  conn.Name,
			"environment": conn.Environment,
			"reason":      reason,
		},
	})
	if err != nil {
		slog.Error("접속 이벤트 개시 실패", "connection", conn.Name, "err", err)
		return
	}
	if created {
		slog.Error("접속 실패 이벤트", "connection", conn.Name, "fails", fails, "reason", reason)
	}
	notifyEvent(ctx, m.sink, m.st, eventID, created)
}

// maintain은 롤업과 보존기간 정리를 수행한다.
func (m *Manager) maintain(ctx context.Context) {
	if n, err := m.st.RollupHourly(ctx); err != nil {
		slog.Error("시간 롤업 실패", "err", err)
	} else if n > 0 {
		slog.Debug("시간 롤업 완료", "buckets", n)
	}

	rawDeleted, hourlyDeleted, err := m.st.PurgeMetrics(ctx, m.cfg.RawRetention, m.cfg.HourlyRetention)
	if err != nil {
		slog.Error("지표 보존 정리 실패", "err", err)
	} else if rawDeleted > 0 || hourlyDeleted > 0 {
		slog.Info("지표 보존 정리", "raw", rawDeleted, "hourly", hourlyDeleted)
	}

	if n, err := m.st.PurgeEvents(ctx, m.cfg.EventRetention); err != nil {
		slog.Error("이벤트 정리 실패", "err", err)
	} else if n > 0 {
		slog.Info("이벤트 정리", "deleted", n)
	}

	if n, err := m.st.PurgeSchemaSnapshots(ctx, m.cfg.SnapshotsPerConnection); err != nil {
		slog.Error("스냅샷 정리 실패", "err", err)
	} else if n > 0 {
		slog.Info("스냅샷 정리", "deleted", n)
	}
}
