package monitor

import (
	"context"
	"testing"
	"time"

	"dbstudio/internal/hostmon"
	"dbstudio/internal/store"
)

// 호스트 감시의 시험은 **판정**을 본다. 값을 읽는 부분(hostmon)은 그 패키지에서
// 따로 확인한다 — 여기서 실제 CPU를 읽으면 시험 결과가 그날 서버 사정에 달라진다.

func newHostMonitor(t *testing.T) (*HostMonitor, *store.Store) {
	t.Helper()
	st := newTestStore(t)
	return NewHostMonitor(st, DefaultHostConfig()), st
}

// openHostEvents는 열려 있는 호스트 이벤트를 지표별로 모은다.
func openHostEvents(t *testing.T, st *store.Store) map[string]*store.Event {
	t.Helper()
	events, _, err := st.ListEvents(context.Background(), store.EventFilter{
		Kind: store.EventHost, State: "open", Limit: 100,
	})
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	out := map[string]*store.Event{}
	for _, e := range events {
		out[e.Metric] = e
	}
	return out
}

func TestSustainedNeedsDuration(t *testing.T) {
	h, st := newHostMonitor(t)
	ctx := context.Background()
	start := time.Now()

	// 임계를 넘었지만 아직 지속 조건을 만족하지 않았다.
	// 여기서 바로 이벤트를 내면 백업이 도는 5분마다 알림이 온다.
	h.checkSustained(ctx, sustainCheck{
		metric: MetricHostCPU, label: "CPU 사용률", value: 92,
		warn: 85, crit: 95, sustain: 5 * time.Minute, at: start,
	})
	if len(openHostEvents(t, st)) != 0 {
		t.Fatal("지속 조건을 만족하기 전에 이벤트가 생겼다")
	}

	// 5분이 지나도록 계속 넘어 있으면 그때 알린다.
	h.checkSustained(ctx, sustainCheck{
		metric: MetricHostCPU, label: "CPU 사용률", value: 92,
		warn: 85, crit: 95, sustain: 5 * time.Minute, at: start.Add(6 * time.Minute),
	})
	ev := openHostEvents(t, st)[MetricHostCPU]
	if ev == nil {
		t.Fatal("지속 조건을 만족했는데 이벤트가 없다")
	}
	if ev.Severity != store.SeverityWarning {
		t.Errorf("심각도 = %s, warning이어야 한다", ev.Severity)
	}

	// 심각 수준으로 올라가면 같은 이벤트의 심각도가 오른다.
	h.checkSustained(ctx, sustainCheck{
		metric: MetricHostCPU, label: "CPU 사용률", value: 99,
		warn: 85, crit: 95, sustain: 5 * time.Minute, at: start.Add(7 * time.Minute),
	})
	if ev := openHostEvents(t, st)[MetricHostCPU]; ev.Severity != store.SeverityCritical {
		t.Errorf("심각도 = %s, critical이어야 한다", ev.Severity)
	}
}

func TestHysteresisKeepsEventOpen(t *testing.T) {
	h, st := newHostMonitor(t)
	ctx := context.Background()
	now := time.Now()

	// 디스크는 지속 조건이 없다: 찼다면 그 순간부터 문제다.
	h.checkSustained(ctx, sustainCheck{
		metric: "host.disk:/", label: "디스크 사용률 (/)", value: 96,
		warn: 85, crit: 95, at: now,
	})
	if openHostEvents(t, st)["host.disk:/"] == nil {
		t.Fatal("디스크 임계 초과 이벤트가 없다")
	}

	// 경고선 바로 아래(85의 95% = 80.75 위)로 내려온 것만으로는 닫지 않는다.
	// 임계선 근처에서 흔들릴 때마다 열고 닫으면 목록이 같은 이야기로 가득 찬다.
	h.checkSustained(ctx, sustainCheck{
		metric: "host.disk:/", label: "디스크 사용률 (/)", value: 84,
		warn: 85, crit: 95, at: now.Add(time.Minute),
	})
	if openHostEvents(t, st)["host.disk:/"] == nil {
		t.Error("경고선 바로 아래인데 이벤트가 닫혔다")
	}

	// 충분히 내려오면 닫는다.
	h.checkSustained(ctx, sustainCheck{
		metric: "host.disk:/", label: "디스크 사용률 (/)", value: 60,
		warn: 85, crit: 95, at: now.Add(2 * time.Minute),
	})
	if openHostEvents(t, st)["host.disk:/"] != nil {
		t.Error("정상으로 돌아왔는데 이벤트가 열려 있다")
	}
}

func TestBootEventOnlyOnChange(t *testing.T) {
	h, st := newHostMonitor(t)
	ctx := context.Background()
	boot := time.Now().Add(-48 * time.Hour).Truncate(time.Second)

	snap := &hostmon.Snapshot{At: time.Now()}
	snap.Info.BootAt = boot

	// 첫 관측: 비교 대상이 없다. 여기서 이벤트를 내면 이 기능을 켠 모든 서버가
	// "재부팅되었다"고 말한다.
	h.checkBoot(ctx, nil, snap)
	if len(openHostEvents(t, st)) != 0 {
		t.Fatal("첫 관측에 재부팅 이벤트가 생겼다")
	}

	// 같은 부팅 시각이 조금 흔들린 것(가동 시간에서 역산하므로)은 재부팅이 아니다.
	prev := &store.HostStateRecord{BootAt: boot.UTC().Format(time.RFC3339)}
	snap.Info.BootAt = boot.Add(40 * time.Second)
	h.checkBoot(ctx, prev, snap)
	if len(openHostEvents(t, st)) != 0 {
		t.Fatal("몇 초 차이를 재부팅으로 판정했다")
	}

	// 실제로 재부팅되면 알린다.
	snap.Info.BootAt = boot.Add(30 * time.Hour)
	h.checkBoot(ctx, prev, snap)
	if openHostEvents(t, st)["host.boot"] == nil {
		t.Error("재부팅했는데 이벤트가 없다")
	}
}

func TestStartupEventMarksUncleanShutdown(t *testing.T) {
	ctx := context.Background()

	// 정상 시작은 기록만 남기고 바로 해소한다 — 조치할 것이 없다.
	st := newTestStore(t)
	clean := NewHostMonitor(st, HostConfig{Version: "test"})
	clean.recordStartup(ctx)
	if len(openHostEvents(t, st)) != 0 {
		t.Error("정상 시작 이벤트가 열린 채 남았다")
	}
	all, _, err := st.ListEvents(ctx, store.EventFilter{Kind: store.EventHost, Limit: 10})
	if err != nil || len(all) != 1 {
		t.Fatalf("시작 기록이 남지 않았다: %v (%d건)", err, len(all))
	}

	// 비정상 종료는 열어 둔다. 사람이 보고 확인해야 할 일이다.
	st2 := newTestStore(t)
	dirty := NewHostMonitor(st2, HostConfig{StartupNote: "강제 종료로 보입니다"})
	dirty.recordStartup(ctx)
	ev := openHostEvents(t, st2)["host.app.crash"]
	if ev == nil {
		t.Fatal("비정상 종료 이벤트가 없다")
	}
	if ev.Severity != store.SeverityWarning {
		t.Errorf("심각도 = %s, warning이어야 한다", ev.Severity)
	}
}

func TestHostSamplesSkipUnknownValues(t *testing.T) {
	// 못 읽은 값은 0으로 채우지 않고 아예 넣지 않는다.
	// 차트에서 "0%"와 "모름"은 전혀 다른 뜻이다.
	s := &hostmon.Snapshot{
		At: time.Now(), MemTotal: 1000, MemUsed: 250,
		Disks: []hostmon.Disk{{Mount: "/", Total: 100, Free: 10}, {Mount: "/none"}},
	}
	got := map[string]float64{}
	for _, sm := range hostSamples(s) {
		got[sm.Metric] = sm.Value
	}
	if _, ok := got[MetricHostCPU]; ok {
		t.Error("읽지 못한 CPU가 지표로 저장됐다")
	}
	if got[MetricHostMemory] != 25 {
		t.Errorf("메모리 사용률 = %v, 25여야 한다", got[MetricHostMemory])
	}
	if got["host.disk:/"] != 90 {
		t.Errorf("디스크 사용률 = %v, 90이어야 한다", got["host.disk:/"])
	}
	if _, ok := got["host.disk:/none"]; ok {
		t.Error("크기를 모르는 디스크가 지표로 저장됐다")
	}
}

func TestOpenEventsSurviveRestart(t *testing.T) {
	// 재시작하면 기억(opened)이 비지만, 이미 열린 이벤트는 DB에 있다.
	// 그것을 읽어 오지 않으면 값이 정상으로 돌아와도 영원히 닫히지 않는다.
	h, st := newHostMonitor(t)
	ctx := context.Background()
	h.checkSustained(ctx, sustainCheck{
		metric: MetricHostMemory, label: "메모리 사용률", value: 97,
		warn: 85, crit: 95, at: time.Now(),
	})

	restarted := NewHostMonitor(st, DefaultHostConfig())
	restarted.loadOpen(ctx)
	restarted.checkSustained(ctx, sustainCheck{
		metric: MetricHostMemory, label: "메모리 사용률", value: 40,
		warn: 85, crit: 95, at: time.Now(),
	})
	if openHostEvents(t, st)[MetricHostMemory] != nil {
		t.Error("재시작 뒤 정상으로 돌아왔는데 이벤트가 닫히지 않았다")
	}
}
