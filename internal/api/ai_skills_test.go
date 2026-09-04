package api

import (
	"regexp"
	"strings"
	"testing"
)

var skillSlot = regexp.MustCompile(`\{\{(\w+)\}\}`)

// 스킬의 지시문과 인자는 서로를 가리킨다. 어긋나면 두 가지가 조용히 일어난다.
//
//   - 지시문에만 있는 자리({{focus}})는 화면이 채우지 못해 그대로 남고, 모델은
//     그 글자를 지시로 읽는다.
//   - 인자에만 있는 값은 사람에게 묻고 나서 아무 데도 쓰이지 않는다.
//
// 둘 다 화면에서는 "답이 좀 이상하다"로만 보인다.
func TestSkillPromptsMatchArgs(t *testing.T) {
	for _, sk := range aiSkills() {
		keys := map[string]bool{}
		for _, a := range sk.Args {
			if a.Key == "" || a.Label == "" || a.Type == "" {
				t.Errorf("%s: 인자에 빈 칸이 있습니다: %+v", sk.ID, a)
			}
			switch a.Type {
			case "connection", "erd", "text", "number":
			default:
				t.Errorf("%s: 화면이 모르는 인자 종류입니다: %q", sk.ID, a.Type)
			}
			keys[a.Key] = true
		}

		used := map[string]bool{}
		for _, m := range skillSlot.FindAllStringSubmatch(sk.Prompt, -1) {
			used[m[1]] = true
			if !keys[m[1]] {
				t.Errorf("%s: 지시문의 {{%s}} 를 채울 인자가 없습니다", sk.ID, m[1])
			}
		}
		for key := range keys {
			if !used[key] {
				t.Errorf("%s: 인자 %q 를 물어보고 쓰지 않습니다", sk.ID, key)
			}
		}
	}
}

// 스킬은 툴 이름을 부른다. 툴 이름이 바뀌었는데 지시문이 그대로면, 모델은 없는 툴을
// 부르려다 실패하고 엉뚱한 방법을 찾는다 — 그 실패는 답변 안에서만 보인다.
func TestSkillPromptsNameRealTools(t *testing.T) {
	known := map[string]bool{}
	for name := range aiTools() {
		known[name] = true
	}
	// 지시문에 나오는 툴 이름은 소문자와 밑줄로만 이뤄진 낱말이다. 그중 우리가
	// 아는 이름 꼴(동사_명사)만 확인한다 — 평범한 낱말까지 확인하려 들면 한국어
	// 문장 전체를 훑게 된다.
	word := regexp.MustCompile(`\b[a-z]+(?:_[a-z_]+)+\b`)
	for _, sk := range aiSkills() {
		for _, name := range word.FindAllString(sk.Prompt, -1) {
			// 자리표시자와 컬럼 예시는 툴이 아니다.
			if strings.Contains(sk.Prompt, "{{"+name+"}}") || name == "created_at" ||
				name == "pg_stat_user_indexes" {
				continue
			}
			if !known[name] {
				t.Errorf("%s: 없는 툴을 부릅니다: %s", sk.ID, name)
			}
		}
	}
}

// 권한이 없는 사람에게 스킬을 보여 주면, 눌러서 승인 화면까지 간 뒤에 거부된다.
func TestSkillsAreFilteredByPermission(t *testing.T) {
	found := false
	for _, sk := range aiSkills() {
		if sk.ID == "sql_benchmark_macro" {
			found = true
			if sk.RequiresPerm == "" {
				t.Error("매크로를 만드는 스킬에 권한 요구가 없습니다")
			}
		}
	}
	if !found {
		t.Fatal("sql_benchmark_macro 스킬이 사라졌습니다")
	}
}

// 지시문이 비어 있으면 스킬은 아무것도 하지 않는 단추가 된다.
func TestSkillsHaveContent(t *testing.T) {
	seen := map[string]bool{}
	for _, sk := range aiSkills() {
		if seen[sk.ID] {
			t.Errorf("스킬 id 가 겹칩니다: %s", sk.ID)
		}
		seen[sk.ID] = true
		if strings.TrimSpace(sk.Name) == "" || strings.TrimSpace(sk.Description) == "" {
			t.Errorf("%s: 이름이나 설명이 비었습니다", sk.ID)
		}
		if len(strings.TrimSpace(sk.Prompt)) < 40 {
			t.Errorf("%s: 지시문이 너무 짧습니다", sk.ID)
		}
		if skillPromptFor(sk.ID) == "" {
			t.Errorf("%s: id 로 지시문을 찾지 못합니다", sk.ID)
		}
	}
}
