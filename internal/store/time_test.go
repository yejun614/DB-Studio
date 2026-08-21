package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"dbstudio/internal/crypto"
	"dbstudio/internal/metric"
	"dbstudio/internal/model"
)

// TestTimeStringOrderingMatchesChronological은 저장 형식의 문자열 순서가
// 시간 순서와 일치하는지 확인한다.
//
// 이 성질이 깨지면 SQLite의 시간 범위 조건(`ts >= ?`, `ORDER BY ts`)이 조용히
// 틀린 결과를 낸다. time.RFC3339Nano는 소수점 뒤 0을 제거해 자리수가 가변이므로
// 정확히 정초인 값과 소수부가 있는 값의 비교가 뒤집힌다.
func TestTimeStringOrderingMatchesChronological(t *testing.T) {
	base := time.Date(2026, 8, 13, 19, 31, 40, 0, time.UTC)

	// 문제를 일으키는 조합을 명시적으로 포함한다:
	// 정초(소수부 없음), 1자리 소수부, 9자리 소수부.
	times := []time.Time{
		base,                             // 소수부 없음 → RFC3339Nano는 "…:40Z"
		base.Add(1 * time.Nanosecond),    // 9자리
		base.Add(100 * time.Millisecond), // RFC3339Nano는 "…:40.1Z"
		base.Add(500 * time.Millisecond),
		base.Add(999999999 * time.Nanosecond),
		base.Add(1 * time.Second),
	}

	for i := 1; i < len(times); i++ {
		prev, cur := times[i-1], times[i]
		prevStr, curStr := formatTime(prev), formatTime(cur)

		if !(prevStr < curStr) {
			t.Errorf("문자열 순서가 시간 순서와 다릅니다:\n  %v → %q\n  %v → %q",
				prev, prevStr, cur, curStr)
		}
		// 저장 형식은 항상 같은 길이여야 한다. 길이가 다르면 비교가 어긋난다.
		if len(prevStr) != len(curStr) {
			t.Errorf("저장 형식의 길이가 일정하지 않습니다: %d(%q) vs %d(%q)",
				len(prevStr), prevStr, len(curStr), curStr)
		}
	}

	// RFC3339Nano로는 실제로 순서가 뒤집힌다는 것을 확인해 두어,
	// 누군가 형식을 되돌리면 이 테스트가 이유를 설명하게 한다.
	exact := base.Format(time.RFC3339Nano)
	withFraction := base.Add(500 * time.Millisecond).Format(time.RFC3339Nano)
	if exact < withFraction {
		t.Errorf("RFC3339Nano가 이 케이스에서 정상 동작합니다. 전제를 재확인하세요: %q < %q",
			exact, withFraction)
	} else {
		t.Logf("확인: RFC3339Nano는 순서가 뒤집힘 (%q > %q). 고정 폭 형식이 필요한 이유",
			exact, withFraction)
	}
}

// TestParseTimeAcceptsBothFormats는 형식 변경 전에 기록된 값도 읽히는지 확인한다.
func TestParseTimeAcceptsBothFormats(t *testing.T) {
	want := time.Date(2026, 8, 13, 19, 31, 40, 500000000, time.UTC)

	for _, s := range []string{
		want.Format(timeLayout),
		want.Format(time.RFC3339Nano),
		want.Format(time.RFC3339),
	} {
		got := parseTime(s)
		if s == want.Format(time.RFC3339) {
			// RFC3339는 소수부를 버리므로 초 단위까지만 일치한다.
			if got.Unix() != want.Unix() {
				t.Errorf("parseTime(%q) = %v, 기대값(초 단위) %v", s, got, want)
			}
			continue
		}
		if !got.Equal(want) {
			t.Errorf("parseTime(%q) = %v, 기대값 %v", s, got, want)
		}
	}

	if !parseTime("").IsZero() {
		t.Error("빈 문자열은 zero time이어야 합니다")
	}
	if !parseTime("not-a-time").IsZero() {
		t.Error("파싱 불가한 값은 zero time이어야 합니다")
	}
}

// TestTimeRangeQueryAtSecondBoundary는 정초 경계에서 범위 조회가 정확한지 확인한다.
//
// 이것이 이 형식 변경의 실제 목적이다: 폴링이 정확히 정초에 걸리는 일은 드물지 않고,
// 그때 범위 조회가 그 샘플을 빠뜨리거나 잘못 포함하면 차트와 룰이 어긋난다.
func TestTimeRangeQueryAtSecondBoundary(t *testing.T) {
	ctx := context.Background()
	key := make([]byte, 32)
	box, err := crypto.NewSecretBox(key)
	if err != nil {
		t.Fatalf("secret box: %v", err)
	}
	st, err := Open(ctx, filepath.Join(t.TempDir(), "t.db"), box)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	pw := "pw"
	_, conn, err := st.CreateServerWithDatabase(ctx,
		SaveServerParams{
			Name: "c", Kind: model.KindMySQL, DefaultEnvironment: model.EnvDev,
			Host: "h", Port: 1, Options: model.Options{}, Tags: []string{},
			Enabled: true, Password: &pw,
		},
		SaveConnectionParams{
			Name: "c", Environment: model.EnvDev, DatabaseName: "d",
			Tags: []string{}, Enabled: true,
		})
	if err != nil {
		t.Fatalf("create connection: %v", err)
	}

	// 정초와 소수부가 있는 시각을 섞어 저장한다.
	base := time.Now().UTC().Truncate(time.Second).Add(-10 * time.Minute)
	stamps := []time.Time{
		base,                             // 정초
		base.Add(500 * time.Millisecond), // 소수부
		base.Add(1 * time.Second),        // 정초
		base.Add(1500 * time.Millisecond),
	}
	for i, ts := range stamps {
		set := metric.NewSet()
		set.CollectedAt = ts
		set.Gauge("probe", float64(i), metric.UnitCount)
		if err := st.SaveSamples(ctx, conn.ID, set, ""); err != nil {
			t.Fatalf("save %d: %v", i, err)
		}
	}

	// base 이후 전체를 요청하면 4개 모두 들어와야 한다.
	series, err := st.QuerySeries(ctx, SeriesQuery{
		ConnectionID: conn.ID, Metrics: []string{"probe"},
		From: base, To: base.Add(2 * time.Second), MaxPoints: 1000,
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	totalSamples := 0
	minV, maxV := 1e9, -1e9
	for _, p := range series[0].Points {
		totalSamples++
		if p.Min < minV {
			minV = p.Min
		}
		if p.Max > maxV {
			maxV = p.Max
		}
	}
	if totalSamples == 0 {
		t.Fatal("범위 조회가 아무것도 반환하지 않았습니다")
	}
	if minV != 0 || maxV != 3 {
		t.Errorf("정초 경계 샘플이 누락되었습니다: 값 범위 %g~%g (0~3 기대)", minV, maxV)
	}

	// 두 번째 정초 이후만 요청하면 앞의 두 개는 제외되어야 한다.
	series, err = st.QuerySeries(ctx, SeriesQuery{
		ConnectionID: conn.ID, Metrics: []string{"probe"},
		From: base.Add(1 * time.Second), To: base.Add(2 * time.Second), MaxPoints: 1000,
	})
	if err != nil {
		t.Fatalf("query narrowed: %v", err)
	}
	minV, maxV = 1e9, -1e9
	for _, p := range series[0].Points {
		if p.Min < minV {
			minV = p.Min
		}
		if p.Max > maxV {
			maxV = p.Max
		}
	}
	if minV != 2 || maxV != 3 {
		t.Errorf("좁힌 범위에 이전 샘플이 섞였습니다: 값 범위 %g~%g (2~3 기대)", minV, maxV)
	}
}
