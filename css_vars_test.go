package dbstudio

import (
	"io/fs"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// 스타일시트가 **없는 변수**를 쓰고 있지 않은지 본다.
//
// 왜 이런 검사가 필요한가: 없는 사용자 정의 속성을 쓰면 CSS 는 오류를 내지 않는다.
// 그 선언 하나가 통째로 무효가 되고, 상속되는 속성이면 조상의 값을 물려받는다.
// 실제로 뷰 카드의 머리띠가 그렇게 깨져 있었다 — hsl(var(--h-accent) / 0.14) 이라고
// 적혀 있었지만 --h-accent 라는 변수는 없었고, fill 이 상속 속성이라 초깃값인
// **검정**이 그려졌다. 어두운 테마에서는 검은 바탕에 묻혀 아무도 못 봤고, 밝은
// 테마에서만 흰 카드 위의 검은 띠로 드러났다.
//
// 눈으로는 못 잡는 종류다. 오류도 경고도 없고, 테마 하나에서만 보이며, 그 테마를
// 안 쓰는 사람에게는 영영 보이지 않는다.
func TestCSSVariablesAreDefined(t *testing.T) {
	web, err := WebFS()
	if err != nil {
		t.Fatalf("자산을 열지 못했습니다: %v", err)
	}
	css, err := fs.ReadFile(web, "css/app.css")
	if err != nil {
		t.Fatalf("app.css 를 읽지 못했습니다: %v", err)
	}
	// 주석은 빼고 본다. 설명 안의 예시(hsl(var(--h-x) / 0.5))까지 세면 늘 실패한다.
	text := regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(string(css), " ")

	// 정의: "--이름:" 형태의 선언.
	defined := map[string]bool{}
	for _, m := range regexp.MustCompile(`(--[a-zA-Z0-9-]+)\s*:`).FindAllStringSubmatch(text, -1) {
		defined[m[1]] = true
	}

	// 화면이 인라인 style 로 넣어 주는 것들. 규칙 쪽에서도 그 존재를 조건으로
	// 걸고 있어(예: [style*="--card-accent"]) 시트에는 정의가 없는 것이 맞다.
	fromScript := map[string]bool{"--card-accent": true}

	// 대체값이 있는 것(var(--x, 기본))은 없어도 된다. 그것이 대체값의 쓰임이다.
	uses := regexp.MustCompile(`var\(\s*(--[a-zA-Z0-9-]+)\s*\)`).FindAllStringSubmatch(text, -1)
	missing := map[string]bool{}
	for _, m := range uses {
		name := m[1]
		if defined[name] || fromScript[name] {
			continue
		}
		missing[name] = true
	}
	if len(missing) == 0 {
		return
	}
	names := make([]string, 0, len(missing))
	for name := range missing {
		names = append(names, name)
	}
	sort.Strings(names)
	t.Errorf("정의되지 않은 CSS 변수를 쓰고 있습니다: %s\n"+
		"없는 변수를 쓰면 그 선언이 조용히 무효가 됩니다. "+
		"팔레트에 더하거나, 없어도 되는 값이면 var(--이름, 대체값) 으로 적으세요",
		strings.Join(names, ", "))
}
