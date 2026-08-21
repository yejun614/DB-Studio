package macro

import (
	"testing"
	"time"
)

// cron 해석은 눈으로 검산하기 어렵고, 틀렸다는 사실은 실행되지 않은 다음 날 아침에야
// 드러난다. 그래서 여기서만큼은 예상 시각을 하나하나 적어 둔다.

func mustSchedule(t *testing.T, spec string) *Schedule {
	t.Helper()
	s, err := ParseSchedule(spec)
	if err != nil {
		t.Fatalf("ParseSchedule(%q): %v", spec, err)
	}
	return s
}

func TestParseScheduleRejectsBadSpecs(t *testing.T) {
	bad := []string{
		"",
		"* * * *",         // 필드 부족
		"* * * * * *",     // 필드 초과
		"60 * * * *",      // 분 범위 초과
		"* 24 * * *",      // 시 범위 초과
		"* * 0 * *",       // 일은 1부터
		"* * 32 * *",      // 일 범위 초과
		"* * * 13 *",      // 월 범위 초과
		"* * * * 8",       // 요일 범위 초과
		"*/0 * * * *",     // 간격 0
		"*/-1 * * * *",    // 음수 간격
		"5-1 * * * *",     // 뒤집힌 범위
		"a * * * *",       // 숫자가 아님
		"1,,2 * * * *",    // 빈 항목
		"* * * * mon",     // 이름 요일은 지원하지 않는다
		"0 0 1 1 1 extra", // 꼬리 필드
	}
	for _, spec := range bad {
		if _, err := ParseSchedule(spec); err == nil {
			t.Errorf("ParseSchedule(%q): 오류를 기대했지만 통과했다", spec)
		}
	}
}

func TestScheduleNext(t *testing.T) {
	// 2024-03-15는 금요일이다.
	base := time.Date(2024, 3, 15, 10, 30, 15, 0, time.UTC)

	cases := []struct {
		spec string
		want time.Time
		note string
	}{
		{"* * * * *", time.Date(2024, 3, 15, 10, 31, 0, 0, time.UTC),
			"매분 — 초는 버리고 다음 분"},
		{"*/10 * * * *", time.Date(2024, 3, 15, 10, 40, 0, 0, time.UTC),
			"10분마다"},
		{"0 * * * *", time.Date(2024, 3, 15, 11, 0, 0, 0, time.UTC),
			"매시 정각"},
		{"0 3 * * *", time.Date(2024, 3, 16, 3, 0, 0, 0, time.UTC),
			"오늘 3시는 지났으므로 내일"},
		{"0 9 * * 1", time.Date(2024, 3, 18, 9, 0, 0, 0, time.UTC),
			"금요일에서 본 다음 월요일"},
		{"0 4 1 * *", time.Date(2024, 4, 1, 4, 0, 0, 0, time.UTC),
			"매월 1일"},
		{"30 10 * * *", time.Date(2024, 3, 16, 10, 30, 0, 0, time.UTC),
			"같은 분에 두 번 실행하지 않는다"},
		{"0 0 29 2 *", time.Date(2028, 2, 29, 0, 0, 0, 0, time.UTC),
			"2월 29일 — 다음 윤년까지 찾아간다"},
		{"0 0 1-5 * *", time.Date(2024, 4, 1, 0, 0, 0, 0, time.UTC),
			"3월 15일에서 본 다음 1~5일"},
		{"15,45 * * * *", time.Date(2024, 3, 15, 10, 45, 0, 0, time.UTC),
			"목록"},
		{"0 0 1 * 1", time.Date(2024, 3, 18, 0, 0, 0, 0, time.UTC),
			"일·요일이 모두 지정되면 OR — 4월 1일보다 3월 18일(월)이 먼저"},
	}

	for _, tc := range cases {
		s := mustSchedule(t, tc.spec)
		got, ok := s.Next(base)
		if !ok {
			t.Errorf("%s: 다음 시각을 찾지 못했다 (%s)", tc.spec, tc.note)
			continue
		}
		if !got.Equal(tc.want) {
			t.Errorf("%s (%s): got %s, want %s", tc.spec, tc.note, got, tc.want)
		}
	}
}

func TestScheduleNextImpossible(t *testing.T) {
	// 2월 30일은 영원히 오지 않는다. 4년 상한 안에서 없다고 답해야 한다 —
	// 무한 루프에 빠지면 스케줄러 고루틴 전체가 멈춘다.
	s := mustSchedule(t, "0 0 30 2 *")
	if got, ok := s.Next(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)); ok {
		t.Fatalf("2월 30일에서 시각을 찾았다: %s", got)
	}
}

func TestScheduleNextUsesLocation(t *testing.T) {
	// 시간대는 "몇 시에 도는가"를 결정한다. 서울 새벽 3시는 UTC 18시다.
	seoul, err := LoadLocation("Asia/Seoul")
	if err != nil {
		t.Skipf("시간대 데이터를 쓸 수 없다: %v", err)
	}
	s := mustSchedule(t, "0 3 * * *")
	next, ok := s.Next(time.Date(2024, 3, 15, 10, 0, 0, 0, time.UTC).In(seoul))
	if !ok {
		t.Fatal("다음 시각이 없다")
	}
	if next.Hour() != 3 {
		t.Errorf("서울 기준 시각이 3시가 아니다: %s", next)
	}
	want := time.Date(2024, 3, 15, 18, 0, 0, 0, time.UTC)
	if !next.UTC().Equal(want) {
		t.Errorf("UTC 환산이 다르다: got %s, want %s", next.UTC(), want)
	}
}

func TestScheduleNextIsStrictlyIncreasing(t *testing.T) {
	// 미리보기 화면은 Next를 이어서 부른다. 같은 시각을 두 번 돌려주면
	// 목록에 같은 줄이 반복되고, 스케줄러라면 같은 분에 두 번 실행된다.
	s := mustSchedule(t, "*/15 * * * *")
	cursor := time.Date(2024, 3, 15, 10, 0, 0, 0, time.UTC)
	prev := cursor
	for range 10 {
		next, ok := s.Next(cursor)
		if !ok {
			t.Fatal("다음 시각이 없다")
		}
		if !next.After(prev) {
			t.Fatalf("시각이 나아가지 않는다: %s → %s", prev, next)
		}
		prev, cursor = next, next
	}
}

func TestWeekdaySevenIsSunday(t *testing.T) {
	// 0과 7 둘 다 일요일로 쓰는 관습이 있다. 한쪽만 받으면 남의 crontab을
	// 옮겨 붙인 사람이 이유 없이 실패한다.
	base := time.Date(2024, 3, 15, 12, 0, 0, 0, time.UTC) // 금
	zero, _ := mustSchedule(t, "0 0 * * 0").Next(base)
	seven, _ := mustSchedule(t, "0 0 * * 7").Next(base)
	if !zero.Equal(seven) {
		t.Fatalf("요일 0과 7이 다르다: %s vs %s", zero, seven)
	}
	if zero.Weekday() != time.Sunday {
		t.Fatalf("일요일이 아니다: %s", zero)
	}
}

func TestDescribe(t *testing.T) {
	cases := map[string]string{
		"* * * * *":    "매분",
		"*/10 * * * *": "10분마다",
		"0 * * * *":    "매시 0분",
		"0 */6 * * *":  "6시간마다 (0분)",
		"0 3 * * *":    "매일 03:00",
		"30 9 * * 1":   "매주 월요일 09:30",
		"0 4 1 * *":    "매월 1일 04:00",
		"0 0 * * 7":    "매주 일요일 00:00",
		// 알아보지 못하는 식은 원문 그대로 둔다. 억지로 옮기면 틀린 설명이 된다.
		"0 0 1,15 * *": "0 0 1,15 * *",
		"엉망":           "엉망",
	}
	for spec, want := range cases {
		if got := Describe(spec); got != want {
			t.Errorf("Describe(%q) = %q, want %q", spec, got, want)
		}
	}
}

func TestLoadLocation(t *testing.T) {
	if loc, err := LoadLocation("  "); err != nil || loc != time.Local {
		t.Errorf("빈 이름은 서버 지역 시간이어야 한다: %v %v", loc, err)
	}
	if _, err := LoadLocation("Mars/Olympus"); err == nil {
		t.Error("없는 시간대인데 통과했다")
	}
	// time/tzdata를 넣은 목적이 이것이다. Windows나 최소 컨테이너에서도 서울이
	// 해석되어야 "매일 새벽 3시(한국 시간)"가 배포 환경과 무관하게 동작한다.
	if _, err := LoadLocation("Asia/Seoul"); err != nil {
		t.Errorf("Asia/Seoul을 해석하지 못했다: %v", err)
	}
}
