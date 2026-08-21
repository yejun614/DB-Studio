package macro

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	// 시간대 데이터베이스를 바이너리에 넣는다.
	//
	// 스케줄은 "매일 새벽 3시(Asia/Seoul)"처럼 시간대와 함께 지정되는데, Windows나
	// 최소 컨테이너에는 시스템 tzdata가 없어 LoadLocation이 실패한다. 배포 환경에 따라
	// 스케줄이 다른 시각에 도는 것은 스케줄러로서 치명적이고, 그 대가는 450KB다.
	//
	// main이 아니라 여기서 넣는 이유: 시간대를 필요로 하는 것은 이 파일이다.
	// main에 두면 스케줄러를 쓰는 다른 바이너리(테스트 포함)가 조용히 tzdata 없이
	// 돌게 되고, 그 차이는 "내 컴퓨터에선 되는데"로만 드러난다.
	_ "time/tzdata"
)

// cron 식 해석.
//
// 라이브러리를 쓰지 않는다. 필요한 것은 표준 5필드 cron이고, 그것은 표 다섯 개와
// "다음 시각 찾기" 한 함수로 끝난다. 반면 라이브러리를 들이면 초 필드·@every·
// 자체 스케줄러 고루틴처럼 우리가 쓰지 않는 것들이 함께 들어오고, 그중 하나가
// 우리 실행 모델(소유자 권한·중복 방지·놓친 실행)과 어긋나기 시작한다.
//
// 지원 문법: `분 시 일 월 요일`, 각 필드에 `*`, `숫자`, `a-b`, `a,b,c`, `*/n`, `a-b/n`.
// 요일은 0=일요일, 7도 일요일로 받는다(둘 다 쓰는 관습이 있다).

// Schedule은 해석된 cron 식이다.
type Schedule struct {
	minutes  []bool // 60
	hours    []bool // 24
	days     []bool // 32 (1-31)
	months   []bool // 13 (1-12)
	weekdays []bool // 7 (0-6)
	// dayRestricted/weekdayRestricted는 일·요일 필드가 * 인지 기록한다.
	//
	// cron의 오래된 규칙 하나: 둘 다 지정되면 **둘 중 하나만 맞아도** 실행한다
	// (예: "1일 또는 월요일"). 둘 중 하나가 *이면 나머지만 본다.
	// 이 규칙을 모르면 "매월 1일과 매주 월요일"을 표현할 방법이 없다.
	dayRestricted     bool
	weekdayRestricted bool
	spec              string
}

func (s *Schedule) Spec() string { return s.spec }

// ParseSchedule은 cron 식을 해석한다.
func ParseSchedule(spec string) (*Schedule, error) {
	fields := strings.Fields(strings.TrimSpace(spec))
	if len(fields) != 5 {
		return nil, fmt.Errorf("cron 식은 5개 필드여야 합니다 (분 시 일 월 요일): %q", spec)
	}

	s := &Schedule{spec: strings.Join(fields, " ")}
	var err error
	if s.minutes, err = parseField(fields[0], 0, 59, "분"); err != nil {
		return nil, err
	}
	if s.hours, err = parseField(fields[1], 0, 23, "시"); err != nil {
		return nil, err
	}
	if s.days, err = parseField(fields[2], 1, 31, "일"); err != nil {
		return nil, err
	}
	if s.months, err = parseField(fields[3], 1, 12, "월"); err != nil {
		return nil, err
	}
	if s.weekdays, err = parseWeekday(fields[4]); err != nil {
		return nil, err
	}
	s.dayRestricted = fields[2] != "*"
	s.weekdayRestricted = fields[4] != "*"
	return s, nil
}

func parseField(field string, min, max int, name string) ([]bool, error) {
	slots := make([]bool, max+1)
	for part := range strings.SplitSeq(field, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, fmt.Errorf("%s 필드가 비어 있습니다", name)
		}

		step := 1
		if base, stepStr, found := strings.Cut(part, "/"); found {
			n, err := strconv.Atoi(stepStr)
			if err != nil || n <= 0 {
				return nil, fmt.Errorf("%s 필드의 간격이 올바르지 않습니다: %s", name, part)
			}
			step = n
			part = base
		}

		lo, hi := min, max
		switch {
		case part == "*":
			// 전체 범위
		case strings.Contains(part, "-"):
			a, b, _ := strings.Cut(part, "-")
			var err error
			if lo, err = strconv.Atoi(strings.TrimSpace(a)); err != nil {
				return nil, fmt.Errorf("%s 필드를 읽을 수 없습니다: %s", name, part)
			}
			if hi, err = strconv.Atoi(strings.TrimSpace(b)); err != nil {
				return nil, fmt.Errorf("%s 필드를 읽을 수 없습니다: %s", name, part)
			}
		default:
			n, err := strconv.Atoi(part)
			if err != nil {
				return nil, fmt.Errorf("%s 필드를 읽을 수 없습니다: %s", name, part)
			}
			lo, hi = n, n
		}

		if lo < min || hi > max || lo > hi {
			return nil, fmt.Errorf("%s 필드의 범위가 %d~%d 를 벗어났습니다: %s", name, min, max, part)
		}
		for v := lo; v <= hi; v += step {
			slots[v] = true
		}
	}
	return slots, nil
}

func parseWeekday(field string) ([]bool, error) {
	// 7을 일요일로 받기 위해 0~7로 읽은 뒤 7을 0으로 접는다.
	raw, err := parseField(field, 0, 7, "요일")
	if err != nil {
		return nil, err
	}
	slots := make([]bool, 7)
	for i := 0; i <= 7; i++ {
		if raw[i] {
			slots[i%7] = true
		}
	}
	return slots, nil
}

// Next는 after 이후의 첫 실행 시각을 반환한다.
//
// 후보를 1분씩 늘려가며 확인한다. 영리한 방법(필드별로 다음 값을 계산해 건너뛰기)도
// 있지만, 최악의 경우(2월 30일처럼 영원히 오지 않는 날짜)를 다루려면 어차피 상한이
// 필요하고, 그 상한 안에서는 단순한 쪽이 틀릴 여지가 없다. 4년치를 훑어도 200만 번이며
// 스케줄 하나당 몇 밀리초다.
func (s *Schedule) Next(after time.Time) (time.Time, bool) {
	// 초·나노초를 버리고 다음 분부터 본다. 같은 분에 두 번 실행하지 않기 위해서다.
	t := after.Truncate(time.Minute).Add(time.Minute)

	// 윤년 주기를 넘겨 4년까지 본다. 그 안에 없으면 영원히 오지 않는 식이다
	// (예: "2월 30일").
	limit := t.AddDate(4, 0, 0)
	for t.Before(limit) {
		if s.matches(t) {
			return t, true
		}
		next := t.Add(time.Minute)
		// 서머타임으로 시계가 되돌아가면 같은 시각을 반복할 수 있다.
		// 진행하지 못하면 한 시간을 건너뛰어 무한 루프를 막는다.
		if !next.After(t) {
			next = t.Add(time.Hour)
		}
		t = next
	}
	return time.Time{}, false
}

func (s *Schedule) matches(t time.Time) bool {
	if !s.minutes[t.Minute()] || !s.hours[t.Hour()] || !s.months[int(t.Month())] {
		return false
	}
	day := s.days[t.Day()]
	weekday := s.weekdays[int(t.Weekday())]

	switch {
	case s.dayRestricted && s.weekdayRestricted:
		// 옛 cron 규칙: 둘 다 지정되면 OR다.
		return day || weekday
	case s.dayRestricted:
		return day
	case s.weekdayRestricted:
		return weekday
	default:
		return true
	}
}

// Describe는 cron 식을 사람이 읽는 문장으로 바꾼다.
//
// 화면에 원문만 보여주면 "0 3 * * 1"이 무슨 뜻인지 아는 사람만 쓸 수 있다.
// 흔한 형태 몇 가지를 알아보고, 나머지는 원문 그대로 둔다 — 모든 cron 식을 한국어로
// 옮기려 들면 그 자체가 버그의 원천이 된다.
func Describe(spec string) string {
	fields := strings.Fields(strings.TrimSpace(spec))
	if len(fields) != 5 {
		return spec
	}
	minute, hour, day, month, weekday := fields[0], fields[1], fields[2], fields[3], fields[4]

	// 매분
	if minute == "*" && hour == "*" && day == "*" && month == "*" && weekday == "*" {
		return "매분"
	}
	// N분마다
	if strings.HasPrefix(minute, "*/") && hour == "*" && day == "*" && month == "*" && weekday == "*" {
		return strings.TrimPrefix(minute, "*/") + "분마다"
	}
	// 매시 N분
	if isNumber(minute) && hour == "*" && day == "*" && month == "*" && weekday == "*" {
		return "매시 " + minute + "분"
	}
	// N시간마다
	if isNumber(minute) && strings.HasPrefix(hour, "*/") && day == "*" && month == "*" && weekday == "*" {
		return strings.TrimPrefix(hour, "*/") + "시간마다 (" + minute + "분)"
	}
	// 매일 HH:MM
	if isNumber(minute) && isNumber(hour) && day == "*" && month == "*" && weekday == "*" {
		return fmt.Sprintf("매일 %s:%s", pad2(hour), pad2(minute))
	}
	// 매주 요일 HH:MM
	if isNumber(minute) && isNumber(hour) && day == "*" && month == "*" && isNumber(weekday) {
		return fmt.Sprintf("매주 %s요일 %s:%s", weekdayName(weekday), pad2(hour), pad2(minute))
	}
	// 매월 D일 HH:MM
	if isNumber(minute) && isNumber(hour) && isNumber(day) && month == "*" && weekday == "*" {
		return fmt.Sprintf("매월 %s일 %s:%s", day, pad2(hour), pad2(minute))
	}
	return spec
}

func isNumber(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func pad2(s string) string {
	if len(s) == 1 {
		return "0" + s
	}
	return s
}

func weekdayName(s string) string {
	names := []string{"일", "월", "화", "수", "목", "금", "토", "일"}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 || n > 7 {
		return s
	}
	return names[n]
}

// LoadLocation은 시간대 이름을 해석한다. 빈 값이면 서버 지역 시간이다.
//
// time/tzdata를 함께 넣는 이유(cmd에서 import): Windows나 최소 컨테이너에는 시간대
// 데이터베이스가 없어 LoadLocation이 실패한다. "매일 새벽 3시(한국 시간)"가 배포
// 환경에 따라 동작하지 않는 것은 스케줄러로서 치명적이다.
func LoadLocation(name string) (*time.Location, error) {
	if strings.TrimSpace(name) == "" {
		return time.Local, nil
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("알 수 없는 시간대입니다: %s", name)
	}
	return loc, nil
}
