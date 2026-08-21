package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dbstudio/internal/auth"
	"dbstudio/internal/store"
)

// REST API는 라우팅·토큰 인증·툴 게이트가 모두 맞아야 동작한다.
// 어느 하나가 어긋나면 그 사실이 "스크립트가 401을 받는다"로만 나타나므로 HTTP로 돈다.

// issueToken은 테스트용 API 토큰을 발급하고 원문을 돌려준다.
func (e *testEnv) issueToken(t *testing.T, scope string) string {
	t.Helper()
	_, raw, err := e.srv.authn.IssueAPIToken(context.Background(), store.CreateTokenParams{
		UserID: e.user.ID, Name: "test-" + scope, Scope: scope,
	})
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}
	return raw
}

// bearer는 Bearer 토큰으로 요청을 보낸다.
//
// client(쿠키)를 쓰지 않는 것이 요점이다 — 이 경로는 쿠키를 받지 않아야 한다.
func (e *testEnv) bearer(t *testing.T, method, path, token, body string) (int, http.Header, map[string]any) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	res, err := e.srv.App().Test(req, -1)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer res.Body.Close()

	raw, _ := io.ReadAll(res.Body)
	out := map[string]any{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &out)
	}
	return res.StatusCode, res.Header, out
}

// 토큰 없이 통과하면 이 API는 인터넷에 열린 DB 콘솔이 된다.
func TestRESTRequiresBearerToken(t *testing.T) {
	e := newTestEnv(t)

	status, header, body := e.bearer(t, "GET", restBasePath, "", "")
	if status != http.StatusUnauthorized {
		t.Fatalf("토큰 없는 요청 = %d, want 401 (body=%v)", status, body)
	}
	// 클라이언트가 "자격증명 문제"와 "요청 내용 문제"를 구분할 수 있어야 한다.
	if got := header.Get("WWW-Authenticate"); !strings.Contains(got, "Bearer") {
		t.Errorf("WWW-Authenticate = %q", got)
	}

	if status, _, _ := e.bearer(t, "GET", restBasePath, "dbs_없는토큰", ""); status != http.StatusUnauthorized {
		t.Errorf("잘못된 토큰 = %d, want 401", status)
	}
}

// 세션 쿠키로는 통할 수 없다. 통하면 브라우저가 자격증명을 자동으로 실어 보내게 되어
// 다른 사이트가 이 엔드포인트를 부를 수 있다(CSRF).
func TestRESTRejectsSessionCookie(t *testing.T) {
	e := newTestEnv(t)
	c := e.client(t)
	if status, body := c.do("POST", "/api/v1/auth/login", map[string]any{
		"username": "alice", "password": testPassword,
	}); status != http.StatusOK {
		t.Fatalf("로그인 = %d (%v)", status, body)
	}
	if c.cookies[auth.SessionCookieName] == "" {
		t.Fatal("세션 쿠키가 발급되지 않았다")
	}

	// 쿠키만 들고 REST API를 부른다.
	req := httptest.NewRequest("GET", restBasePath, nil)
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: c.cookies[auth.SessionCookieName]})
	res, err := e.srv.App().Test(req, -1)
	if err != nil {
		t.Fatalf("test: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("세션 쿠키로 REST API = %d, want 401", res.StatusCode)
	}
}

// 읽기 토큰에는 변경 툴이 목록에도 나오지 않아야 한다.
// 보이는데 부르면 거부되는 툴은 클라이언트가 계속 시도하게 만든다.
func TestRESTToolListRespectsScope(t *testing.T) {
	e := newTestEnv(t)

	read := restTools(t, e, e.issueToken(t, store.TokenScopeRead))
	for _, tool := range read {
		if tool.Mutating {
			t.Errorf("읽기 토큰 목록에 변경 툴 %q 가 있다", tool.Name)
		}
	}
	if len(read) == 0 {
		t.Fatal("읽기 토큰에 조회 툴이 하나도 없다")
	}

	write := restTools(t, e, e.issueToken(t, store.TokenScopeWrite))
	if len(write) <= len(read) {
		t.Errorf("쓰기 토큰 툴 %d개 <= 읽기 토큰 %d개 — 범위가 목록에 반영되지 않았다",
			len(write), len(read))
	}
	// 경로를 함께 주지 않으면 클라이언트가 이름을 문자열로 이어 붙이게 된다.
	for _, tool := range write {
		if tool.Path != restBasePath+"/"+tool.Name {
			t.Errorf("%s: path = %q", tool.Name, tool.Path)
		}
		if tool.InputSchema == nil {
			t.Errorf("%s: inputSchema가 없다 — 인자를 만들 수 없다", tool.Name)
		}
	}
}

func restTools(t *testing.T, e *testEnv, token string) []restTool {
	t.Helper()
	status, _, body := e.bearer(t, "GET", restBasePath, token, "")
	if status != http.StatusOK {
		t.Fatalf("툴 목록 = %d (%v)", status, body)
	}
	raw, err := json.Marshal(body["tools"])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var tools []restTool
	if err := json.Unmarshal(raw, &tools); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return tools
}

// 목록에서 감추는 것은 편의이고, 실제 관문은 호출 시점 판정이다.
// 이름을 직접 적어 부르는 클라이언트가 통과하면 범위 구분은 없는 것과 같다.
func TestRESTWriteToolNeedsWriteToken(t *testing.T) {
	e := newTestEnv(t)
	token := e.issueToken(t, store.TokenScopeRead)

	status, _, body := e.bearer(t, "POST", restBasePath+"/create_erd_document", token,
		`{"name":"초안","connection":"없는 DB"}`)
	if status != http.StatusForbidden {
		t.Fatalf("읽기 토큰으로 변경 툴 = %d, want 403 (%v)", status, body)
	}
	// 권한 부족과 범위 부족은 다른 문제다. 코드가 같으면 클라이언트는
	// "토큰을 다시 발급하면 된다"는 사실을 알 수 없다.
	if body["error"] != "read_only_token" {
		t.Errorf("error = %v, want read_only_token", body["error"])
	}

	// 같은 이유로 스키마 조회도 막힌다.
	if status, _, _ := e.bearer(t, "GET", restBasePath+"/create_erd_document", token, ""); status != http.StatusForbidden {
		t.Errorf("읽기 토큰으로 변경 툴 스키마 = %d, want 403", status)
	}
}

func TestRESTUnknownTool(t *testing.T) {
	e := newTestEnv(t)
	status, _, body := e.bearer(t, "POST", restBasePath+"/drop_everything",
		e.issueToken(t, store.TokenScopeWrite), "{}")
	if status != http.StatusNotFound {
		t.Fatalf("없는 툴 = %d, want 404 (%v)", status, body)
	}
	if body["error"] != "unknown_tool" {
		t.Errorf("error = %v, want unknown_tool", body["error"])
	}
	// 다음에 무엇을 해야 하는지 오류 문구만 보고 알 수 있어야 한다.
	if detail, _ := body["detail"].(string); !strings.Contains(detail, restBasePath) {
		t.Errorf("detail = %q — 목록 경로 안내가 없다", detail)
	}
}

// 인자가 없는 툴을 부르는 데 `-d '{}'` 를 요구할 이유가 없다.
// 그리고 결과는 파싱된 JSON으로 와야 한다 — 클라이언트가 두 번 파싱하지 않도록.
func TestRESTCallReturnsParsedJSON(t *testing.T) {
	e := newTestEnv(t)
	token := e.issueToken(t, store.TokenScopeRead)

	status, _, body := e.bearer(t, "POST", restBasePath+"/list_connections", token, "")
	if status != http.StatusOK {
		t.Fatalf("툴 실행 = %d (%v)", status, body)
	}
	if body["tool"] != "list_connections" {
		t.Errorf("tool = %v", body["tool"])
	}
	if _, ok := body["result"]; !ok {
		t.Errorf("result가 없다: %v", body)
	}
	if _, ok := body["text"]; ok {
		t.Errorf("JSON 결과에 text가 함께 왔다: %v", body)
	}
	if mutating, _ := body["mutating"].(bool); mutating {
		t.Error("조회 툴이 mutating으로 표시됐다")
	}
}

// 툴이 거절한 것은 요청 내용의 문제다. 200에 오류를 숨기면 `if res.ok` 분기가
// 조용히 틀리고, 500으로 답하면 서버 고장으로 오인된다.
func TestRESTToolFailureIsUnprocessable(t *testing.T) {
	e := newTestEnv(t)
	status, _, body := e.bearer(t, "POST", restBasePath+"/get_connection_status",
		e.issueToken(t, store.TokenScopeRead), `{"connection":"없는 커넥션"}`)
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("툴 오류 = %d, want 422 (%v)", status, body)
	}
	if body["error"] != "tool_failed" {
		t.Errorf("error = %v, want tool_failed", body["error"])
	}
	if msg, _ := body["message"].(string); msg == "" {
		t.Error("툴이 돌려준 이유가 비었다")
	}
}

func TestRESTRejectsNonObjectBody(t *testing.T) {
	e := newTestEnv(t)
	token := e.issueToken(t, store.TokenScopeRead)

	for _, body := range []string{`[1,2]`, `"문자열"`, `{망가진`} {
		status, _, res := e.bearer(t, "POST", restBasePath+"/list_connections", token, body)
		if status != http.StatusBadRequest {
			t.Errorf("본문 %s = %d, want 400 (%v)", body, status, res)
		}
	}
}

// 클라이언트가 붙자마자 "이 토큰이 살아 있는가, 무엇을 할 수 있는가"를
// 툴을 부르지 않고 확인할 수 있어야 한다.
func TestRESTIdentity(t *testing.T) {
	e := newTestEnv(t)
	status, _, body := e.bearer(t, "GET", "/api/me", e.issueToken(t, store.TokenScopeRead), "")
	if status != http.StatusOK {
		t.Fatalf("/api/me = %d (%v)", status, body)
	}
	user, _ := body["user"].(map[string]any)
	if user["username"] != "alice" {
		t.Errorf("user = %v", user)
	}
	token, _ := body["token"].(map[string]any)
	if token["scope"] != store.TokenScopeRead {
		t.Errorf("token = %v", token)
	}
	if count, _ := body["toolCount"].(float64); count <= 0 {
		t.Errorf("toolCount = %v", body["toolCount"])
	}
}

// 폐기한 토큰이 계속 통하면 폐기 버튼은 장식이다.
func TestRESTRevokedTokenIsRejected(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	saved, raw, err := e.srv.authn.IssueAPIToken(ctx, store.CreateTokenParams{
		UserID: e.user.ID, Name: "폐기 대상", Scope: store.TokenScopeRead,
	})
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if status, _, _ := e.bearer(t, "GET", restBasePath, raw, ""); status != http.StatusOK {
		t.Fatalf("폐기 전 = %d, want 200", status)
	}
	if err := e.st.RevokeAPIToken(ctx, saved.ID, e.user.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if status, _, _ := e.bearer(t, "GET", restBasePath, raw, ""); status != http.StatusUnauthorized {
		t.Errorf("폐기 후 = %d, want 401", status)
	}
}

// 정의되지 않은 경로는 401이 아니라 404여야 한다.
// 토큰 미들웨어를 /api 전체에 걸면 오타 난 프론트엔드 요청이 401을 받고,
// 화면은 그것을 세션 만료로 읽어 로그아웃한다.
func TestUnknownAPIPathStaysNotFound(t *testing.T) {
	e := newTestEnv(t)
	status, _, _ := e.bearer(t, "GET", "/api/없는경로", "", "")
	if status != http.StatusNotFound {
		t.Errorf("없는 경로 = %d, want 404", status)
	}
}

func TestRESTArguments(t *testing.T) {
	// 빈 본문과 null은 "인자 없음"이다.
	for _, in := range []string{"", "   ", "null", "{}"} {
		got, err := restArguments([]byte(in))
		if err != nil {
			t.Errorf("restArguments(%q): %v", in, err)
			continue
		}
		if string(got) != "{}" && in != "{}" {
			t.Errorf("restArguments(%q) = %s, want {}", in, got)
		}
	}

	got, err := restArguments([]byte(`{"connection":"x"}`))
	if err != nil || string(got) != `{"connection":"x"}` {
		t.Errorf("객체 = %s, err=%v", got, err)
	}

	// 툴 스키마는 전부 객체다. 배열·문자열은 여기서 걸러야 오류가 400으로 나온다.
	for _, in := range []string{`[1]`, `"x"`, `12`, `{`} {
		if _, err := restArguments([]byte(in)); err == nil {
			t.Errorf("restArguments(%q)가 통과했다", in)
		}
	}
}

func TestRESTResult(t *testing.T) {
	raw, text := restResult(`{"ok":true}`)
	if text != "" || string(raw) != `{"ok":true}` {
		t.Errorf("JSON 결과 = raw:%s text:%q", raw, text)
	}

	// 결과가 상한을 넘으면 asJSON이 잘라내고 안내 문장을 붙이므로 더 이상 JSON이 아니다.
	// 그때 잘린 JSON을 억지로 심으면 클라이언트는 그것이 결과 전부라고 믿는다.
	truncated := `{"rows":[1,2` + "\n… (결과가 너무 커서 잘렸습니다)"
	raw, text = restResult(truncated)
	if raw != nil || text != truncated {
		t.Errorf("잘린 결과 = raw:%s text:%q", raw, text)
	}

	if raw, text := restResult(""); raw != nil || text != "" {
		t.Errorf("빈 결과 = raw:%s text:%q", raw, text)
	}
}
