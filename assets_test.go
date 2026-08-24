package dbstudio

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

// 프론트엔드에는 컴파일러가 없다.
//
// 그래서 "부르는데 어디에도 없는 이름"이 조용히 남는다. 그 코드가 실제로 도는 순간에만
// ReferenceError가 나고, 그 자리에서 렌더가 멈춘다 — 화면에는 아무 일도 일어나지 않은
// 것처럼 보인다. ERD 인스펙터가 그랬다: 테이블을 눌러도 오른쪽 패널이 그대로였고,
// 원인은 `truncate`를 import하지 않은 한 줄이었다.
//
// 그 한 줄을 잡자고 node 도구 사슬을 들여올 이유는 없다(이 저장소에는 없다). 필요한
// 검사는 좁다: **모듈이 부르는 이름이 그 모듈 안에 있거나 import되어 있는가.**
// 그것만 Go 시험으로 확인한다 — `go test ./...` 에 얹히므로 따로 기억할 것도 없다.
//
// 이 검사는 완전하지 않다(스코프를 따지지 않는다). 넓게 잡아 거짓 경보를 내느니
// 놓치는 쪽을 택했다 — 경보가 일상이 되면 아무도 읽지 않는다.

var (
	reImportNames = regexp.MustCompile(`import\s*\{([^}]*)\}`)
	reImportBare  = regexp.MustCompile(`import\s+(\w+)\s+from`)
	reDeclared    = regexp.MustCompile(`(?:function\s*\*?|class|const|let|var)\s+([A-Za-z_$][\w$]*)`)
	// 메서드 선언부. `async foo(a, b) {` 과 `foo(a) {` 를 모두 잡는다.
	reMethod = regexp.MustCompile(`(?m)^\s*(?:async\s+|static\s+|get\s+|set\s+)*([A-Za-z_$][\w$]*)\s*\(([^()]*)\)\s*\{`)
	// 함수 선언부. 이름과 **매개변수**를 함께 걷는다 — 콜백을 구조 분해로 받는
	// 서명(`function openModal({ title, body })`)이 흔해서, 매개변수를 놓치면
	// 그 콜백을 부르는 자리가 전부 "없는 이름"으로 잡힌다.
	reFuncDecl = regexp.MustCompile(`function\s*\*?\s*([A-Za-z_$][\w$]*)?\s*\(([^()]*)\)`)
	// 화살표 함수와 구조 분해에서 이름을 걷는다. 넓게 걷어야 거짓 경보가 줄어든다.
	reArrow   = regexp.MustCompile(`\(([^()]*)\)\s*=>`)
	reDestruc = regexp.MustCompile(`(?:const|let|var)\s*\{([^}]*)\}`)
	// 호출 자리. 앞에 `.`이나 `#`이 있으면 그 객체(또는 클래스)의 것이므로 여기서 볼 일이 아니다.
	reCall = regexp.MustCompile(`(^|[^.#\w$])([A-Za-z_$][\w$]*)\s*\(`)
)

// jsKeywords는 뒤에 괄호가 오지만 호출이 아닌 것들이다.
var jsKeywords = map[string]bool{
	"if": true, "for": true, "while": true, "switch": true, "catch": true,
	"return": true, "typeof": true, "instanceof": true, "function": true,
	"new": true, "delete": true, "void": true, "yield": true, "do": true,
	"else": true, "case": true, "await": true, "async": true, "in": true,
	"of": true, "this": true, "super": true, "class": true, "throw": true,
	"import": true, "export": true, "default": true, "constructor": true,
}

// browserGlobals는 브라우저가 주는 이름들이다.
var browserGlobals = []string{
	"console", "document", "window", "location", "navigator", "history", "fetch",
	"setTimeout", "clearTimeout", "setInterval", "clearInterval", "queueMicrotask",
	"requestAnimationFrame", "cancelAnimationFrame", "structuredClone", "matchMedia",
	"alert", "confirm", "prompt", "getComputedStyle", "localStorage", "sessionStorage",
	"Object", "Array", "String", "Number", "Boolean", "Math", "JSON", "Date", "RegExp",
	"Map", "Set", "WeakMap", "WeakSet", "Promise", "Error", "Symbol", "Proxy", "Reflect",
	"BigInt", "Intl", "CSS", "URL", "URLSearchParams", "AbortController", "Blob", "File",
	"FileReader", "FormData", "Image", "Event", "CustomEvent", "Node", "Element",
	"HTMLElement", "SVGElement", "DOMParser", "WebSocket", "EventSource", "Worker",
	"TextDecoder", "TextEncoder", "IntersectionObserver", "ResizeObserver",
	"MutationObserver", "performance", "crypto", "globalThis", "isNaN", "isFinite",
	"parseInt", "parseFloat", "encodeURIComponent", "decodeURIComponent",
	"encodeURI", "decodeURI",
}

// stripNonCode는 주석·문자열·정규식 리터럴을 공백으로 지운다.
//
// 정규식으로 한 번에 지우지 않는 이유가 있다. 문자열 안에 `//`가 들어 있고
// (`'https?://…'`) 주석 안에 따옴표가 들어 있다. 어느 쪽을 먼저 지워도 나머지가
// 망가지므로, 한 글자씩 훑으며 지금 어디에 있는지를 알고 지우는 수밖에 없다.
//
// 줄 수와 길이를 유지하려고 지운 자리를 공백으로 채운다.
func stripNonCode(src string) string {
	out := []byte(src)
	blank := func(i int) {
		if out[i] != '\n' {
			out[i] = ' '
		}
	}
	// prevCode는 마지막으로 본 코드 글자다. `/`가 나눗셈인지 정규식의 시작인지
	// 가르는 데 쓴다 — 자바스크립트에서 이것은 앞 문맥으로만 알 수 있다.
	prevCode := byte(0)
	for i := 0; i < len(out); i++ {
		c := out[i]
		switch {
		case c == '/' && i+1 < len(out) && out[i+1] == '/':
			for i < len(out) && out[i] != '\n' {
				blank(i)
				i++
			}
		case c == '/' && i+1 < len(out) && out[i+1] == '*':
			for i < len(out) && !(out[i] == '*' && i+1 < len(out) && out[i+1] == '/') {
				blank(i)
				i++
			}
			if i < len(out) {
				blank(i)
			}
			if i+1 < len(out) {
				blank(i + 1)
			}
			i++
		case c == '\'' || c == '"' || c == '`':
			quote := c
			blank(i)
			i++
			for i < len(out) && out[i] != quote {
				if out[i] == '\\' {
					blank(i)
					i++
				}
				if i < len(out) {
					blank(i)
					i++
				}
			}
			if i < len(out) {
				blank(i)
			}
			prevCode = '"'
		case c == '/' && startsRegex(prevCode):
			blank(i)
			i++
			for i < len(out) && out[i] != '/' && out[i] != '\n' {
				if out[i] == '\\' {
					blank(i)
					i++
				}
				if out[i] == '[' { // 문자 클래스 안의 /는 끝이 아니다
					for i < len(out) && out[i] != ']' && out[i] != '\n' {
						blank(i)
						i++
					}
				}
				if i < len(out) {
					blank(i)
					i++
				}
			}
			if i < len(out) && out[i] == '/' {
				blank(i)
			}
			prevCode = 'r'
		default:
			if c != ' ' && c != '\t' && c != '\n' && c != '\r' {
				prevCode = c
			}
		}
	}
	return string(out)
}

// startsRegex는 이 자리의 `/`가 정규식의 시작인지 본다.
// 나눗셈은 값 뒤에 오고, 정규식은 연산자·여는 괄호·구문 경계 뒤에 온다.
func startsRegex(prev byte) bool {
	switch prev {
	case 0, '(', ',', '=', ':', '[', '!', '&', '|', '?', '{', '}', ';', '+', '-', '*', '%', '<', '>', '~', '^':
		return true
	}
	return false
}

func TestFrontendHasNoUnknownCalls(t *testing.T) {
	files, err := fs.Glob(embedded, "web/js/*/*.js")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	top, err := fs.Glob(embedded, "web/js/*.js")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	files = append(files, top...)
	if len(files) < 10 {
		t.Fatalf("프론트엔드 파일을 찾지 못했습니다 (%d개) — 경로 규칙이 바뀌었는지 확인하세요", len(files))
	}

	for _, path := range files {
		raw, err := fs.ReadFile(embedded, path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		// 주석·문자열·정규식 리터럴을 걷어낸다. 그 안의 낱말은 코드가 아니다.
		src := stripNonCode(string(raw))

		known := map[string]bool{}
		for _, name := range browserGlobals {
			known[name] = true
		}
		for name := range jsKeywords {
			known[name] = true
		}
		addNames := func(list string) {
			for _, part := range strings.Split(list, ",") {
				part = strings.TrimSpace(part)
				// `a as b`, `a = 1`, `a: b`, `...rest` 에서 이름만 남긴다.
				if i := strings.LastIndex(part, " as "); i >= 0 {
					part = part[i+4:]
				}
				part = strings.TrimPrefix(strings.TrimSpace(part), "...")
				if i := strings.IndexAny(part, "=:"); i >= 0 {
					part = part[:i]
				}
				part = strings.TrimSpace(strings.Trim(part, "{}[] "))
				if part != "" {
					known[part] = true
				}
			}
		}
		for _, m := range reImportNames.FindAllStringSubmatch(src, -1) {
			addNames(m[1])
		}
		for _, m := range reImportBare.FindAllStringSubmatch(src, -1) {
			known[m[1]] = true
		}
		for _, m := range reDeclared.FindAllStringSubmatch(src, -1) {
			known[m[1]] = true
		}
		for _, m := range reMethod.FindAllStringSubmatch(src, -1) {
			known[m[1]] = true
			addNames(m[2])
		}
		for _, m := range reFuncDecl.FindAllStringSubmatch(src, -1) {
			if m[1] != "" {
				known[m[1]] = true
			}
			addNames(m[2])
		}
		for _, m := range reArrow.FindAllStringSubmatch(src, -1) {
			addNames(m[1])
		}
		for _, m := range reDestruc.FindAllStringSubmatch(src, -1) {
			addNames(m[1])
		}

		reported := map[string]bool{}
		for _, m := range reCall.FindAllStringSubmatch(src, -1) {
			name := m[2]
			if known[name] || reported[name] {
				continue
			}
			reported[name] = true
			t.Errorf("%s: %s(...) 를 부르는데 이 모듈에 정의도 import도 없습니다 "+
				"— 그 코드가 실행되는 순간 ReferenceError로 렌더가 멈춥니다", path, name)
		}
	}
}

// 자바스크립트가 심는 CSS 변수는 스타일시트가 실제로 써야 한다.
//
// ERD 카드의 색이 그랬다. JS는 `--card-accent`를 사각형에 붙였는데 CSS 규칙은
// 그것이 **묶음(g)**에 있을 때만 걸리도록 쓰여 있었고, 메모의 `--note-accent`는
// 아예 아무 규칙도 쓰지 않았다. 둘 다 "색을 골라도 아무 일이 없다"로 나타나는데,
// 오류가 없으니 어디를 봐야 할지 알 수 없다.
//
// 여기서 잡는 것은 그중 확실한 절반이다: **정의만 하고 아무도 쓰지 않는 변수.**
// 어느 요소에 붙였는지까지는 보지 않는다 — 그것까지 정적으로 따지려면 DOM 구조를
// 재현해야 하고, 그 검사는 곧 실제 코드보다 자주 틀린다.
func TestInlineCSSVarsAreUsed(t *testing.T) {
	css, err := fs.ReadFile(embedded, "web/css/app.css")
	if err != nil {
		t.Fatalf("css: %v", err)
	}
	sheet := string(css)

	files, err := fs.Glob(embedded, "web/js/*/*.js")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	// `--이름:` 형태로 심는 자리만 본다(인라인 style 문자열).
	setter := regexp.MustCompile(`--[a-z][a-z0-9-]*\s*:`)

	seen := map[string]bool{}
	for _, path := range files {
		raw, err := fs.ReadFile(embedded, path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		for _, match := range setter.FindAllString(string(raw), -1) {
			name := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(match), ":"))
			if seen[name] {
				continue
			}
			seen[name] = true
			if !strings.Contains(sheet, "var("+name+")") {
				t.Errorf("%s: %s 를 심지만 app.css가 쓰지 않습니다 — 화면에는 아무 변화가 없습니다",
					path, name)
			}
		}
	}
	if len(seen) == 0 {
		t.Fatal("인라인 CSS 변수를 하나도 찾지 못했습니다 — 검사가 무력해졌는지 확인하세요")
	}
}

// TestManualBodiesAreClosedProperly는 설명서 본문의 백틱이 이스케이프되어 있는지 본다.
//
// 이 검사가 있는 이유(실제로 겪었다): 설명서 문장에 코드 조각을 적으면서 백틱을
// 이스케이프하지 않으면 그 자리에서 템플릿 문자열이 끝나고, 뒤따르는 글자가 코드로
// 해석된다. 문법 오류가 아니라 **모듈 평가 시점의 예외**가 되므로 node --check 도,
// 눈으로 읽는 것도 통과한다. 그런데 manual.js 는 main.js 가 처음에 불러오는 모듈이라,
// 그 예외 하나로 앱 전체가 "불러오는 중"에서 멈춘다.
//
// 규칙은 단순하다: 본문은 `body: ` 다음의 백틱에서 시작해 백틱 + 쉼표로 끝난다.
// 그 사이에서 끝나면 이스케이프를 빠뜨린 것이다.
func TestManualBodiesAreClosedProperly(t *testing.T) {
	raw, err := fs.ReadFile(embedded, "web/js/pages/manual.js")
	if err != nil {
		t.Fatalf("manual.js: %v", err)
	}
	src := string(raw)
	const open = "body: `"

	bodies := 0
	for i := 0; ; {
		start := strings.Index(src[i:], open)
		if start < 0 {
			break
		}
		start += i + len(open)
		bodies++

		end := -1
		for j := start; j < len(src); j++ {
			if src[j] == '`' && src[j-1] != '\\' {
				end = j
				break
			}
		}
		if end < 0 {
			t.Fatalf("%d번째 본문이 닫히지 않았습니다", bodies)
		}
		// 본문이 제대로 끝났다면 바로 뒤가 쉼표다.
		if rest := strings.TrimLeft(src[end+1:], " \t\r\n"); !strings.HasPrefix(rest, ",") {
			line := 1 + strings.Count(src[:end], "\n")
			t.Errorf("manual.js:%d 본문이 여기서 끊겼습니다 — 백틱을 \\` 로 이스케이프하세요 (뒤: %.40q)",
				line, src[end:])
		}
		i = end + 1
	}
	if bodies < 10 {
		t.Fatalf("설명서 본문을 %d개만 찾았습니다 — 검사가 무력해졌는지 확인하세요", bodies)
	}
}
