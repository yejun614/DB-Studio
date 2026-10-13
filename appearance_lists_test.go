package dbstudio

import (
	"io/fs"
	"regexp"
	"slices"
	"strings"
	"testing"

	"dbstudio/internal/erd"
)

// 서버가 아는 색·아이콘 목록이 그리는 쪽과 같은지 본다.
//
// 서버는 AI 툴·MCP 로 들어온 값을 이 목록으로 거른다. 화면에 색이나 아이콘이 하나 더해졌는데
// 서버 목록이 그대로면, 사람은 고를 수 있는 것을 어시스턴트는 "그런 색은 없다"며 거절한다.
// 반대로 화면에서 빠졌는데 서버가 받아 주면, 어시스턴트가 칠한 카드는 고르개에 불이 들어오지
// 않는다. card_geometry_test 와 같은 이유로 **JS 파일을 직접 읽어** 견준다.
func TestAppearanceListsMatchWeb(t *testing.T) {
	web, err := WebFS()
	if err != nil {
		t.Fatalf("자산을 열지 못했습니다: %v", err)
	}
	read := func(name string) string {
		t.Helper()
		src, err := fs.ReadFile(web, name)
		if err != nil {
			t.Fatalf("%s 를 읽지 못했습니다: %v", name, err)
		}
		return string(src)
	}

	// TINTS: { value: '#ef4444', label: '빨강' },
	tints := read("js/core/tints.js")
	body := jsArray(t, tints, "TINTS")
	re := regexp.MustCompile(`value:\s*'([^']*)',\s*label:\s*'([^']*)'`)
	var js []erd.Tint
	for _, m := range re.FindAllStringSubmatch(body, -1) {
		js = append(js, erd.Tint{Value: m[1], Label: m[2]})
	}
	if len(js) != len(erd.Tints) {
		t.Fatalf("색 수가 다릅니다: tints.js %d, 서버 %d", len(js), len(erd.Tints))
	}
	for i := range js {
		if js[i].Value != erd.Tints[i].Value || js[i].Label != erd.Tints[i].Label {
			t.Errorf("%d번째 색: tints.js %s %s, 서버 %s %s",
				i, js[i].Value, js[i].Label, erd.Tints[i].Value, erd.Tints[i].Label)
		}
	}

	if got := jsStrings(t, jsArray(t, read("js/pages/erdeditor.js"), "TABLE_ICONS")); !slices.Equal(got, erd.TableIcons) {
		t.Errorf("표 아이콘이 다릅니다:\n erdeditor.js %v\n 서버        %v", got, erd.TableIcons)
	}
	if got := jsStrings(t, jsArray(t, read("js/core/colicon.js"), "COLUMN_ICONS")); !slices.Equal(got, erd.ColumnIcons) {
		t.Errorf("컬럼 아이콘이 다릅니다:\n colicon.js %v\n 서버      %v", got, erd.ColumnIcons)
	}
}

// jsArray는 `const NAME = [ ... ];` 의 대괄호 안을 꺼낸다(export 가 붙어도 읽는다).
func jsArray(t *testing.T, src, name string) string {
	t.Helper()
	re := regexp.MustCompile(`(?ms)^(?:export\s+)?const\s+` + name + `\s*=\s*\[(.*?)\n\];`)
	m := re.FindStringSubmatch(src)
	if m == nil {
		t.Fatalf("%s 를 찾지 못했습니다. 이름이 바뀌었다면 이 검사도 함께 고쳐야 합니다", name)
	}
	return m[1]
}

// jsStrings는 배열 본문에서 작은따옴표 문자열을 차례로 꺼낸다(주석은 건너뛴다).
func jsStrings(t *testing.T, body string) []string {
	t.Helper()
	var lines []string
	for _, line := range strings.Split(body, "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		lines = append(lines, line)
	}
	out := []string{}
	for _, m := range regexp.MustCompile(`'([^']*)'`).FindAllStringSubmatch(strings.Join(lines, "\n"), -1) {
		out = append(out, m[1])
	}
	return out
}
