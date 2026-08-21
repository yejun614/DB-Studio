package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"dbstudio/internal/crypto"
)

func hostFixture(t *testing.T) (context.Context, *Store) {
	t.Helper()
	ctx := context.Background()
	box, err := crypto.NewSecretBox(make([]byte, 32))
	if err != nil {
		t.Fatalf("secret box: %v", err)
	}
	st, err := Open(ctx, filepath.Join(t.TempDir(), "host.db"), box)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return ctx, st
}

func TestHostSamplesAndSeries(t *testing.T) {
	ctx, st := hostFixture(t)
	now := time.Now().UTC().Truncate(time.Second)

	snap := map[string]any{"cpuPercent": 41.5, "info": map[string]any{"hostname": "srv1"}}
	for i := range 3 {
		at := now.Add(time.Duration(i) * time.Minute)
		err := st.SaveHostSamples(ctx, at, []HostSample{
			{Metric: "host.cpu", Value: float64(10 + i), Unit: "percent"},
			{Metric: "host.disk:/", Value: 80, Unit: "percent"},
		}, snap, at.Format(time.RFC3339))
		if err != nil {
			t.Fatalf("save: %v", err)
		}
	}

	points, err := st.HostSeries(ctx, "host.cpu", now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatalf("series: %v", err)
	}
	if len(points) != 3 {
		t.Fatalf("점 개수 = %d, 3이어야 한다", len(points))
	}
	// 원본 한 점이므로 평균·최소·최대가 같다. 화면(차트)이 커넥션 지표와 같은
	// 구조를 기대하므로 세 값이 모두 채워져 있어야 한다.
	if points[0].Avg != 10 || points[0].Min != 10 || points[0].Max != 10 {
		t.Errorf("첫 점 = %+v", points[0])
	}
	if !points[0].Ts.Before(points[2].Ts) {
		t.Error("시간순으로 정렬되지 않았습니다")
	}

	// 구간을 벗어난 점은 나오지 않아야 한다. 그러지 않으면 "1시간" 차트가
	// 실제로는 전체 이력을 그린다.
	narrow, err := st.HostSeries(ctx, "host.cpu", now.Add(90*time.Second), now.Add(time.Hour))
	if err != nil {
		t.Fatalf("series: %v", err)
	}
	if len(narrow) != 1 {
		t.Errorf("좁은 구간의 점 개수 = %d, 1이어야 한다", len(narrow))
	}

	names, err := st.HostMetricNames(ctx, now.Add(-time.Hour))
	if err != nil {
		t.Fatalf("metric names: %v", err)
	}
	if len(names) != 2 || names[0] != "host.cpu" || names[1] != "host.disk:/" {
		t.Errorf("지표 이름 = %v", names)
	}

	state, err := st.HostState(ctx)
	if err != nil || state == nil {
		t.Fatalf("state: %v %v", state, err)
	}
	var got map[string]any
	if err := json.Unmarshal(state.Snapshot, &got); err != nil {
		t.Fatalf("스냅샷을 다시 읽을 수 없습니다: %v", err)
	}
	if got["cpuPercent"] != 41.5 {
		t.Errorf("스냅샷이 그대로 저장되지 않았습니다: %v", got)
	}

	// 로그 위치는 지표 저장이 덮어쓰지 않아야 한다. 덮어쓰면 이미 읽은 자리를 잊어
	// 같은 시스템 로그 오류가 다시 이벤트가 된다.
	if err := st.SaveOSLogOffset(ctx, "System", 4242); err != nil {
		t.Fatalf("save offset: %v", err)
	}
	if err := st.SaveHostSamples(ctx, now.Add(time.Hour),
		[]HostSample{{Metric: "host.cpu", Value: 5}}, snap, ""); err != nil {
		t.Fatalf("save: %v", err)
	}
	state, _ = st.HostState(ctx)
	if state.OSLogPath != "System" || state.OSLogOffset != 4242 {
		t.Errorf("로그 위치가 지워졌습니다: %+v", state)
	}

	deleted, err := st.PurgeHostSamples(ctx, now.Add(90*time.Second))
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if deleted != 4 { // 첫 두 시점의 지표 2종
		t.Errorf("지워진 표본 = %d, 4여야 한다", deleted)
	}
}

func TestHostStateEmptyBeforeFirstSample(t *testing.T) {
	ctx, st := hostFixture(t)
	// 아직 아무것도 없으면 오류가 아니라 "없음"이다. 오류로 만들면 화면이
	// 첫 수집 전에 빨간 상자를 띄운다.
	state, err := st.HostState(ctx)
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	if state != nil {
		t.Errorf("표본이 없는데 상태가 있습니다: %+v", state)
	}
}

func TestHostThresholdsDefaultsAndNormalize(t *testing.T) {
	ctx, st := hostFixture(t)

	th, err := st.HostThresholds(ctx)
	if err != nil {
		t.Fatalf("thresholds: %v", err)
	}
	if th != DefaultHostThresholds() {
		t.Errorf("저장된 값이 없으면 기본값이어야 한다: %+v", th)
	}

	// 뒤집힌 쌍은 맞바꾼다. 경고가 심각보다 높으면 심각 이벤트는 영원히 안 나온다.
	saved := HostThresholds{
		CPUWarn: 95, CPUCrit: 80, MemWarn: 90, MemCrit: 95,
		DiskWarn: 70, DiskCrit: 200, SustainSec: 120, OSLogEnabled: true,
	}
	if err := st.SaveHostThresholds(ctx, saved, ""); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := st.HostThresholds(ctx)
	if err != nil {
		t.Fatalf("thresholds: %v", err)
	}
	if got.CPUWarn != 80 || got.CPUCrit != 95 {
		t.Errorf("뒤집힌 CPU 쌍이 그대로입니다: %+v", got)
	}
	if got.DiskCrit != DefaultHostThresholds().DiskCrit {
		t.Errorf("범위를 벗어난 디스크 심각값이 그대로입니다: %+v", got)
	}
	if got.SustainSec != 120 {
		t.Errorf("지속 시간 = %d, 120이어야 한다", got.SustainSec)
	}
}
