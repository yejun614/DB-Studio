package macro

import (
	"context"
	"strings"
	"testing"
	"time"

	lua "github.com/yuin/gopher-lua"
)

func TestValidateRejectsBrokenGraphs(t *testing.T) {
	known := KnownTypes()

	cases := []struct {
		name  string
		graph string
		want  string // 이 문구가 치명적 오류 메시지에 있어야 한다
	}{
		{
			name:  "시작 노드 없음",
			graph: `{"nodes":[{"id":"a","type":"log"}],"edges":[]}`,
			want:  "시작 노드가 없습니다",
		},
		{
			name: "시작 노드 둘",
			graph: `{"nodes":[{"id":"a","type":"start"},{"id":"b","type":"start"}],
				"edges":[]}`,
			want: "시작 노드는 하나여야",
		},
		{
			name:  "알 수 없는 종류",
			graph: `{"nodes":[{"id":"s","type":"start"},{"id":"x","type":"rm -rf"}],"edges":[]}`,
			want:  "알 수 없는 노드 종류",
		},
		{
			name: "노드 ID 중복",
			graph: `{"nodes":[{"id":"s","type":"start"},{"id":"s","type":"log"}],
				"edges":[]}`,
			want: "노드 ID가 중복",
		},
		{
			name: "없는 노드로 가는 연결",
			graph: `{"nodes":[{"id":"s","type":"start"}],
				"edges":[{"id":"e","from":"s","fromPort":"out","to":"ghost"}]}`,
			want: "존재하지 않는 노드로",
		},
		{
			name: "사용자 노드에 정의가 없음",
			graph: `{"nodes":[{"id":"s","type":"start"},{"id":"c","type":"custom"}],
				"edges":[]}`,
			want: "어떤 정의를 쓰는지",
		},
		{
			name:  "출력 변수 이름이 식별자가 아님",
			graph: `{"nodes":[{"id":"s","type":"start","output":"has space"}],"edges":[]}`,
			want:  "출력 변수 이름은",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g, err := ParseGraph(tc.graph)
			if err != nil {
				t.Fatalf("ParseGraph: %v", err)
			}
			issues := g.Validate(known)
			if !HasFatal(issues) {
				t.Fatalf("치명적 오류가 나와야 하는데 통과했다: %+v", issues)
			}
			found := false
			for _, i := range issues {
				if i.Fatal && strings.Contains(i.Message, tc.want) {
					found = true
				}
			}
			if !found {
				t.Fatalf("메시지 %q 를 찾지 못했다: %+v", tc.want, issues)
			}
		})
	}
}

// 닿지 않는 노드는 경고여야 한다. 만들다 만 가지를 남겨 두고 다른 부분을 손보는 것은
// 정상적인 작업 방식이고, 그때마다 저장이 막히면 편집이 불가능해진다.
func TestUnreachableNodeIsWarningNotError(t *testing.T) {
	g, err := ParseGraph(`{"nodes":[
		{"id":"s","type":"start"},
		{"id":"orphan","type":"log","params":{"message":"hi"}}],"edges":[]}`)
	if err != nil {
		t.Fatalf("ParseGraph: %v", err)
	}
	issues := g.Validate(KnownTypes())
	if HasFatal(issues) {
		t.Fatalf("저장이 막혀서는 안 된다: %+v", issues)
	}
	if len(issues) != 1 || issues[0].NodeID != "orphan" {
		t.Fatalf("고아 노드에 대한 경고가 있어야 한다: %+v", issues)
	}
}

func TestNextFollowsPorts(t *testing.T) {
	g, err := ParseGraph(`{"nodes":[
		{"id":"s","type":"start"},{"id":"b","type":"branch"},
		{"id":"y","type":"log"},{"id":"n","type":"log"}],
		"edges":[
		{"id":"e1","from":"s","fromPort":"out","to":"b"},
		{"id":"e2","from":"b","fromPort":"true","to":"y"},
		{"id":"e3","from":"b","fromPort":"false","to":"n"}]}`)
	if err != nil {
		t.Fatalf("ParseGraph: %v", err)
	}
	if got := g.Next("b", PortTrue); len(got) != 1 || got[0] != "y" {
		t.Fatalf("true 포트는 y로 가야 한다: %v", got)
	}
	if got := g.Next("b", PortFalse); len(got) != 1 || got[0] != "n" {
		t.Fatalf("false 포트는 n으로 가야 한다: %v", got)
	}
	if got := g.Next("b", PortOut); len(got) != 0 {
		t.Fatalf("out 포트에는 연결이 없어야 한다: %v", got)
	}
}

// 샌드박스가 새면 매크로 권한이 곧 파일 시스템 접근 권한이 된다.
// 이 테스트는 "블랙리스트가 아니라 화이트리스트"라는 결정이 지켜지는지 본다.
func TestSandboxHasNoEscapeHatches(t *testing.T) {
	L := newSandbox(context.Background())
	defer L.Close()

	for _, name := range []string{
		"io", "os", "package", "debug", "require",
		"dofile", "loadfile", "load", "loadstring",
	} {
		if v := L.GetGlobal(name); v != lua.LNil {
			t.Errorf("%s 가 노출되어 있다: %v", name, v)
		}
	}
	// 반대로 있어야 하는 것들.
	for _, name := range []string{"table", "string", "math", "pairs", "ipairs", "tostring"} {
		if v := L.GetGlobal(name); v == lua.LNil {
			t.Errorf("%s 가 없다", name)
		}
	}
}

// 무한 루프는 컨텍스트로 멈춘다. GopherLua에는 명령 수 훅이 없으므로
// 이것이 유일한 방어선이고, 그래서 동작을 반드시 확인해야 한다.
func TestSandboxStopsInfiniteLoop(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	L := newSandbox(ctx)
	defer L.Close()

	done := make(chan error, 1)
	go func() { done <- L.DoString(`while true do end`) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("무한 루프가 오류 없이 끝났다")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("무한 루프가 멈추지 않았다")
	}
}

func TestLuaGoRoundTrip(t *testing.T) {
	L := newSandbox(context.Background())
	defer L.Close()

	in := map[string]any{
		"name":  "홍길동",
		"count": float64(3),
		"ok":    true,
		"rows":  []any{map[string]any{"id": float64(1)}, map[string]any{"id": float64(2)}},
	}
	out, ok := luaToGo(goToLua(L, in)).(map[string]any)
	if !ok {
		t.Fatalf("맵으로 돌아와야 한다")
	}
	if out["name"] != "홍길동" || out["count"] != float64(3) || out["ok"] != true {
		t.Fatalf("스칼라가 보존되지 않았다: %+v", out)
	}
	rows, ok := out["rows"].([]any)
	if !ok || len(rows) != 2 {
		t.Fatalf("배열이 배열로 돌아와야 한다: %+v", out["rows"])
	}
	first, ok := rows[0].(map[string]any)
	if !ok || first["id"] != float64(1) {
		t.Fatalf("중첩 표가 보존되지 않았다: %+v", rows[0])
	}
}

// Lua에는 배열과 맵의 구분이 없지만 JSON과 템플릿에는 있다.
// 이 판정이 틀리면 행 목록이 객체가 되어 반복문이 돌지 않는다.
func TestTableToGoDistinguishesArrayFromMap(t *testing.T) {
	L := newSandbox(context.Background())
	defer L.Close()

	if err := L.DoString(`arr = {10, 20, 30}; obj = {a = 1}; sparse = {[1] = "x", [3] = "y"}`); err != nil {
		t.Fatalf("DoString: %v", err)
	}
	if _, ok := luaToGo(L.GetGlobal("arr")).([]any); !ok {
		t.Errorf("연속된 정수 키는 배열이어야 한다")
	}
	if _, ok := luaToGo(L.GetGlobal("obj")).(map[string]any); !ok {
		t.Errorf("문자열 키는 맵이어야 한다")
	}
	if _, ok := luaToGo(L.GetGlobal("sparse")).(map[string]any); !ok {
		t.Errorf("빈틈 있는 정수 키는 맵으로 떨어져야 한다")
	}
}

func TestResolveSubstitutesVariables(t *testing.T) {
	r := &runner{vars: map[string]any{
		"limit": float64(10),
		"name":  "orders",
		"row":   map[string]any{"id": float64(42)},
	}}

	cases := []struct{ in, want string }{
		{"SELECT * FROM ${name} LIMIT ${limit}", "SELECT * FROM orders LIMIT 10"},
		{"id = ${row.id}", "id = 42"},
		{"없는 것은 ${missing} 그대로", "없는 것은 ${missing} 그대로"},
		{"치환할 것 없음", "치환할 것 없음"},
	}
	for _, tc := range cases {
		if got := r.resolve(tc.in); got != tc.want {
			t.Errorf("resolve(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// 숫자를 문자열로 만들 때 정수는 정수로 보여야 한다.
// "LIMIT 10.000000"은 문법 오류이고, 이 실수는 조용히 SQL을 깨뜨린다.
func TestStringifyKeepsIntegersWhole(t *testing.T) {
	if got := stringify(float64(10)); got != "10" {
		t.Errorf("stringify(10) = %q", got)
	}
	if got := stringify(2.5); got != "2.5" {
		t.Errorf("stringify(2.5) = %q", got)
	}
	if got := stringify(nil); got != "" {
		t.Errorf("stringify(nil) = %q", got)
	}
}

func TestNormalizeParamsAppliesDefaultsAndTypes(t *testing.T) {
	g, err := ParseGraph(`{"nodes":[{"id":"s","type":"start"}],"edges":[],"params":[
		{"name":"limit","type":"number","default":25},
		{"name":"dryRun","type":"boolean"},
		{"name":"note","type":"string"}]}`)
	if err != nil {
		t.Fatalf("ParseGraph: %v", err)
	}

	got, err := normalizeParams(g, map[string]any{"dryRun": "true"})
	if err != nil {
		t.Fatalf("normalizeParams: %v", err)
	}
	if got["limit"] != float64(25) {
		t.Errorf("기본값이 적용되어야 한다: %v", got["limit"])
	}
	if got["dryRun"] != true {
		t.Errorf("문자열 true는 참이어야 한다: %v", got["dryRun"])
	}
	if got["note"] != "" {
		t.Errorf("기본값 없는 문자열은 빈 문자열이어야 한다: %v", got["note"])
	}
}

func TestNormalizeParamsRejectsMissingRequired(t *testing.T) {
	g, err := ParseGraph(`{"nodes":[{"id":"s","type":"start"}],"edges":[],"params":[
		{"name":"target","type":"string","required":true,"label":"대상"}]}`)
	if err != nil {
		t.Fatalf("ParseGraph: %v", err)
	}
	if _, err := normalizeParams(g, nil); err == nil {
		t.Fatal("필수 파라미터가 없으면 거부해야 한다")
	}
}

// 모든 내장 노드는 실행기를 가져야 한다. 팔레트에는 있는데 실행기가 없으면
// 사용자는 그것을 그래프에 넣어 저장까지 한 뒤 실행에서 실패한다.
func TestEverySpecHasExecutor(t *testing.T) {
	for _, spec := range Specs() {
		if _, ok := executors[spec.Type]; !ok {
			t.Errorf("%s 노드에 실행기가 없다", spec.Type)
		}
	}
}

// 권한이 필요한 노드에는 요구 사항이 적혀 있어야 한다.
// 이 표를 보고 화면이 실행 버튼을 막으므로, 빠지면 권한 없이 실행 가능해 보인다.
func TestPrivilegedNodesDeclareRequirements(t *testing.T) {
	need := map[string]bool{
		TypeSQLQuery: true, TypeSQLExec: true, TypeDataQuery: true, TypeDataMutate: true,
		TypeShell: true, TypeMacroCall: true, TypeCapture: true, TypeIntrospect: true,
	}
	for _, spec := range Specs() {
		if !need[spec.Type] {
			continue
		}
		if spec.NeedsCap == "" && spec.NeedsLevel == "" && spec.NeedsPerm == "" {
			t.Errorf("%s 노드에 권한 요구가 선언되지 않았다", spec.Type)
		}
	}
}

func TestTruthy(t *testing.T) {
	cases := []struct {
		in   any
		want bool
	}{
		{nil, false}, {false, true == false}, {true, true},
		{float64(0), false}, {float64(1), true},
		{"", false}, {"false", false}, {"0", false}, {"x", true},
		{[]any{}, false}, {[]any{1}, true},
	}
	for _, tc := range cases {
		if got := truthy(tc.in); got != tc.want {
			t.Errorf("truthy(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// 트리거 파라미터 식은 이벤트 정보를 그대로 받아 값으로 바꿔야 한다.
// 이것이 없으면 이벤트 종류마다 트리거를 하나씩 만들어야 한다.
func TestEvalParamExprs(t *testing.T) {
	vars := map[string]any{
		"trigger": map[string]any{"kind": "event", "name": "야간 점검"},
		"event": map[string]any{
			"severity": "critical", "message": "디스크 부족", "connectionId": "c1",
		},
		"now": "2026-08-14T10:00:00Z",
	}

	out, problems := EvalParamExprs(t.Context(), map[string]string{
		"conn":   "event.connectionId",
		"title":  `"[" .. event.severity .. "] " .. event.message`,
		"urgent": "event.severity == 'critical'",
		"count":  "return 1 + 2",
		"when":   "now",
	}, vars)

	if len(problems) != 0 {
		t.Fatalf("계산 실패: %v", problems)
	}
	if out["conn"] != "c1" {
		t.Errorf("conn = %v", out["conn"])
	}
	if out["title"] != "[critical] 디스크 부족" {
		t.Errorf("title = %v", out["title"])
	}
	if out["urgent"] != true {
		t.Errorf("urgent = %v", out["urgent"])
	}
	if out["count"] != float64(3) {
		t.Errorf("count = %v (%T)", out["count"], out["count"])
	}
	if out["when"] != "2026-08-14T10:00:00Z" {
		t.Errorf("when = %v", out["when"])
	}
}

// 식 하나가 틀려도 나머지는 넘어가야 한다. 자동 실행은 아무도 보고 있지 않으므로
// 하나 때문에 전부 멈추면 그 사실조차 늦게 안다.
func TestEvalParamExprsIsolatesFailures(t *testing.T) {
	out, problems := EvalParamExprs(t.Context(), map[string]string{
		"good": "1 + 1",
		"bad":  "nonexistent.field.deep",
	}, map[string]any{})

	if out["good"] != float64(2) {
		t.Errorf("good = %v", out["good"])
	}
	if _, ok := out["bad"]; ok {
		t.Error("실패한 식은 값을 남기지 않아야 한다")
	}
	if len(problems) != 1 {
		t.Fatalf("문제 %d건, want 1: %v", len(problems), problems)
	}
	if !strings.Contains(problems[0], "bad") {
		t.Errorf("어느 키가 실패했는지 알려야 한다: %v", problems)
	}
}

// 파라미터 식은 값을 정하는 자리이지 일을 하는 자리가 아니다.
// DB·셸 함수가 닿으면 트리거가 매크로를 거치지 않고 부수효과를 낼 수 있다.
func TestEvalParamExprsHasNoHostFunctions(t *testing.T) {
	for _, expr := range []string{"db_query", "shell_run", "http_get", "os", "io", "require"} {
		out, _ := EvalParamExprs(t.Context(), map[string]string{"x": expr}, map[string]any{})
		if out["x"] != nil {
			t.Errorf("%s 가 노출되어 있다: %v", expr, out["x"])
		}
	}
}
