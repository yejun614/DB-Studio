package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"

	"dbstudio/internal/auth"
	"dbstudio/internal/clock"
	"dbstudio/internal/config"
	"dbstudio/internal/crypto"
	"dbstudio/internal/model"
	"dbstudio/internal/monitor"
	"dbstudio/internal/store"
	"dbstudio/internal/totp"
)

// 2단계 인증은 라우팅·쿠키·미들웨어가 모두 맞아야 동작한다.
// 서비스 계층 시험(internal/auth)만으로는 그 연결을 확인할 수 없어 여기서 HTTP로 돈다.

type testEnv struct {
	srv  *Server
	st   *store.Store
	clk  *clock.Clock
	user *model.User
	// project는 이 시험이 쓰는 프로젝트다. 자원은 모두 프로젝트 안에 있으므로
	// (0037) 커넥션을 만들려면 먼저 하나가 있어야 한다.
	project *store.Project
}

// join은 사용자를 시험용 프로젝트에 넣는다.
//
// 프로젝트 참여는 등급보다 앞선 관문이라, 이것을 빼먹으면 등급을 아무리 줘도
// 거부된다 — 그리고 그 거부 이유는 "권한 없음"이라 원인을 짚기 어렵다.
func (e *testEnv) join(t *testing.T, userID string) {
	t.Helper()
	ctx := context.Background()
	p, err := e.st.GetAccessPolicy(ctx, userID)
	if err != nil {
		t.Fatalf("policy: %v", err)
	}
	p.Projects = append(p.Projects, e.project.ID)
	if err := e.st.SetAccessPolicy(ctx, p); err != nil {
		t.Fatalf("join project: %v", err)
	}
}

const testPassword = "correct-horse-9"

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()

	cfg, err := config.Load([]string{"-data", dir, "-monitor=false", "-log-file="})
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	box, err := crypto.NewSecretBox(make([]byte, 32))
	if err != nil {
		t.Fatalf("secret box: %v", err)
	}
	st, err := store.Open(ctx, filepath.Join(dir, "meta.db"), box)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	hash, err := crypto.HashPassword(testPassword)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	u, err := st.CreateUser(ctx, store.CreateUserParams{
		Username: "alice", DisplayName: "Alice", Role: model.RoleSuperadmin, PasswordHash: hash,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	pj, err := st.CreateProject(ctx, store.SaveProjectParams{
		Name: "테스트 프로젝트", ActorID: u.ID,
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	clk := clock.New(0)
	authn := auth.NewService(st, time.Hour, clk)
	authz := auth.NewAuthorizer(st)
	mon := monitor.NewManager(st, monitor.DefaultConfig())
	srv := New(cfg, st, authn, authz, mon, os.DirFS(dir))

	return &testEnv{srv: srv, st: st, clk: clk, user: u, project: pj}
}

// client는 쿠키를 들고 다니는 최소 클라이언트다.
// 실제 브라우저처럼 동작해야 챌린지 쿠키 경로 문제까지 드러난다.
type client struct {
	t       *testing.T
	srv     *Server
	cookies map[string]string
}

func (e *testEnv) client(t *testing.T) *client {
	return &client{t: t, srv: e.srv, cookies: map[string]string{}}
}

func (c *client) do(method, path string, body any) (int, map[string]any) {
	c.t.Helper()
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			c.t.Fatalf("marshal: %v", err)
		}
		reader = strings.NewReader(string(b))
	}
	req := httptest.NewRequest(method, path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-Requested-With", "dbstudio")
	for k, v := range c.cookies {
		req.AddCookie(&http.Cookie{Name: k, Value: v})
	}

	res, err := c.srv.App().Test(req, -1)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer res.Body.Close()
	for _, ck := range res.Cookies() {
		if ck.MaxAge < 0 || ck.Value == "" {
			delete(c.cookies, ck.Name)
			continue
		}
		c.cookies[ck.Name] = ck.Value
	}

	raw, _ := io.ReadAll(res.Body)
	out := map[string]any{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &out)
	}
	return res.StatusCode, out
}

// enrollTOTP는 HTTP로 2단계 인증을 등록하고 복구 코드를 돌려준다.
func (c *client) enrollTOTP(t *testing.T, e *testEnv, userID string) []string {
	t.Helper()
	status, body := c.do("POST", "/api/v1/auth/totp/setup", nil)
	if status != 200 {
		t.Fatalf("setup = %d: %v", status, body)
	}
	enr, _ := body["enrollment"].(map[string]any)
	if enr["secret"] == "" || enr["qr"] == "" {
		t.Fatalf("등록 정보가 비어 있습니다: %v", enr)
	}

	rec, err := e.st.GetTOTP(context.Background(), userID)
	if err != nil {
		t.Fatalf("get enrollment: %v", err)
	}
	status, body = c.do("POST", "/api/v1/auth/totp/confirm",
		map[string]string{"code": e.code(t, rec, 0)})
	if status != 200 {
		t.Fatalf("confirm = %d: %v", status, body)
	}
	raw, _ := body["recoveryCodes"].([]any)
	codes := make([]string, 0, len(raw))
	for _, v := range raw {
		codes = append(codes, v.(string))
	}
	return codes
}

// code는 인증 앱이 만들 코드를 흉내 낸다.
func (e *testEnv) code(t *testing.T, rec *store.TOTPEnrollment, shift time.Duration) string {
	t.Helper()
	code, err := totp.CodeAt(rec.Secret, e.clk.BaseNow().Add(rec.Skew+shift),
		totp.Params{Digits: rec.Digits, Period: rec.Period})
	if err != nil {
		t.Fatalf("code: %v", err)
	}
	return code
}

func (e *testEnv) enrollment(t *testing.T) *store.TOTPEnrollment {
	t.Helper()
	rec, err := e.st.GetTOTP(context.Background(), e.user.ID)
	if err != nil {
		t.Fatalf("get enrollment: %v", err)
	}
	return rec
}

// 로그인 → 등록 → 재로그인의 한살이를 HTTP로 확인한다.
func TestTOTPLoginFlowOverHTTP(t *testing.T) {
	e := newTestEnv(t)
	c := e.client(t)

	status, body := c.do("POST", "/api/v1/auth/login",
		map[string]string{"username": "alice", "password": testPassword})
	if status != 200 {
		t.Fatalf("login = %d: %v", status, body)
	}
	if _, ok := c.cookies[auth.SessionCookieName]; !ok {
		t.Fatal("세션 쿠키가 발급되지 않았습니다")
	}

	c.enrollTOTP(t, e, e.user.ID)

	// 등록하면 /auth/me가 켜짐으로 보고해야 한다(화면이 이 값으로 카드를 그린다).
	_, me := c.do("GET", "/api/v1/auth/me", nil)
	user, _ := me["user"].(map[string]any)
	if user["totpEnabled"] != true {
		t.Errorf("auth/me의 totpEnabled = %v, want true", user["totpEnabled"])
	}

	// 새 브라우저로 로그인하면 이제 코드를 물어야 한다.
	fresh := e.client(t)
	status, body = fresh.do("POST", "/api/v1/auth/login",
		map[string]string{"username": "alice", "password": testPassword})
	if status != 200 {
		t.Fatalf("login = %d: %v", status, body)
	}
	if _, ok := body["user"]; ok {
		t.Error("코드를 내기 전에 사용자 정보가 노출되었습니다")
	}
	if body["twoFactor"] == nil {
		t.Fatalf("2단계 요구가 응답에 없습니다: %v", body)
	}
	if _, ok := fresh.cookies[auth.SessionCookieName]; ok {
		t.Fatal("코드를 내기 전에 세션 쿠키가 발급되었습니다")
	}
	if _, ok := fresh.cookies[auth.ChallengeCookieName]; !ok {
		t.Fatal("챌린지 쿠키가 발급되지 않았습니다")
	}

	// 챌린지만 들고 다른 API를 부를 수 없어야 한다.
	if status, _ := fresh.do("GET", "/api/v1/users/", nil); status != 401 {
		t.Errorf("챌린지 상태에서 /users = %d, want 401", status)
	}

	// 틀린 코드는 거절된다.
	if status, _ := fresh.do("POST", "/api/v1/auth/login/totp",
		map[string]string{"code": "000000"}); status != 401 {
		t.Error("틀린 코드가 통과했습니다")
	}

	rec := e.enrollment(t)
	status, body = fresh.do("POST", "/api/v1/auth/login/totp",
		map[string]string{"code": e.code(t, rec, rec.Period)})
	if status != 200 {
		t.Fatalf("2단계 로그인 = %d: %v", status, body)
	}
	if _, ok := fresh.cookies[auth.SessionCookieName]; !ok {
		t.Fatal("2단계를 마쳤는데 세션이 없습니다")
	}
	if _, ok := fresh.cookies[auth.ChallengeCookieName]; ok {
		t.Error("소비된 챌린지 쿠키가 남아 있습니다")
	}
	if status, _ := fresh.do("GET", "/api/v1/users/", nil); status != 200 {
		t.Error("로그인 후에도 API를 쓸 수 없습니다")
	}
}

// 의무화 정책: 등록하지 않은 사용자는 등록 경로 외에는 막혀야 한다.
func TestTOTPRequiredPolicyGatesAPI(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()

	// 다른 사용자를 하나 더 만든다. 정책은 슈퍼 어드민이 켜지만 영향은 전원에게 간다.
	hash, _ := crypto.HashPassword(testPassword)
	member, err := e.st.CreateUser(ctx, store.CreateUserParams{
		Username: "bob", Role: model.RoleMember, PasswordHash: hash,
	})
	if err != nil {
		t.Fatal(err)
	}

	admin := e.client(t)
	admin.do("POST", "/api/v1/auth/login", map[string]string{"username": "alice", "password": testPassword})

	// 본인이 2FA를 켜지 않은 상태에서는 의무화할 수 없다.
	// (되돌릴 수 있는 사람이 스스로 잠기는 상태를 막는다.)
	status, body := admin.do("PUT", "/api/v1/security/", map[string]bool{"totpRequired": true})
	if status != 400 {
		t.Fatalf("본인 미등록 상태의 의무화 = %d: %v", status, body)
	}

	admin.enrollTOTP(t, e, e.user.ID)
	status, body = admin.do("PUT", "/api/v1/security/", map[string]bool{"totpRequired": true})
	if status != 200 {
		t.Fatalf("의무화 = %d: %v", status, body)
	}

	// 미등록 사용자는 로그인은 되지만 다른 API가 막힌다.
	bob := e.client(t)
	if status, _ := bob.do("POST", "/api/v1/auth/login",
		map[string]string{"username": "bob", "password": testPassword}); status != 200 {
		t.Fatal("미등록 사용자가 로그인조차 못 했습니다")
	}
	status, body = bob.do("GET", "/api/v1/connections/", nil)
	if status != 403 || body["error"] != "totp_setup_required" {
		t.Fatalf("미등록 사용자의 API 호출 = %d %v, want 403 totp_setup_required", status, body)
	}
	// 등록 경로는 열려 있어야 한다. 막으면 등록할 방법이 없다.
	if status, _ := bob.do("GET", "/api/v1/auth/totp", nil); status != 200 {
		t.Error("등록 상태 조회가 막혔습니다")
	}
	if status, _ := bob.do("POST", "/api/v1/auth/totp/setup", nil); status != 200 {
		t.Error("등록 시작이 막혔습니다")
	}

	rec, err := e.st.GetTOTP(ctx, member.ID)
	if err != nil {
		t.Fatal(err)
	}
	if status, body := bob.do("POST", "/api/v1/auth/totp/confirm",
		map[string]string{"code": e.code(t, rec, 0)}); status != 200 {
		t.Fatalf("등록 확인 = %d: %v", status, body)
	}
	// 등록을 마치면 원래 쓰던 API가 열린다.
	if status, _ := bob.do("GET", "/api/v1/connections/", nil); status != 200 {
		t.Error("등록을 마쳤는데도 API가 막혀 있습니다")
	}

	// 의무화 상태에서는 본인이 끌 수 없다.
	if status, body := bob.do("POST", "/api/v1/auth/totp/disable",
		map[string]string{"password": testPassword}); status != 403 || body["error"] != "totp_enforced" {
		t.Errorf("의무화 상태의 해제 = %d %v", status, body)
	}
}

// 슈퍼 어드민의 초기화는 대상의 세션과 2FA를 함께 끊어야 한다.
func TestSuperadminResetsUserTOTP(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	hash, _ := crypto.HashPassword(testPassword)
	member, err := e.st.CreateUser(ctx, store.CreateUserParams{
		Username: "bob", Role: model.RoleMember, PasswordHash: hash,
	})
	if err != nil {
		t.Fatal(err)
	}

	bob := e.client(t)
	bob.do("POST", "/api/v1/auth/login", map[string]string{"username": "bob", "password": testPassword})
	bob.enrollTOTP(t, e, member.ID)

	admin := e.client(t)
	admin.do("POST", "/api/v1/auth/login", map[string]string{"username": "alice", "password": testPassword})
	if status, body := admin.do("POST", "/api/v1/users/"+member.ID+"/totp/reset", nil); status != 200 {
		t.Fatalf("초기화 = %d: %v", status, body)
	}

	if fresh, _ := e.st.GetUser(ctx, member.ID); fresh.TOTPEnabled {
		t.Error("초기화 후에도 2FA가 켜져 있습니다")
	}
	// 대상의 열린 세션도 끊긴다. 계정을 되찾는 상황에서 옛 화면이 살아 있으면 안 된다.
	if status, _ := bob.do("GET", "/api/v1/auth/me", nil); status != 401 {
		t.Error("초기화 후에도 대상의 세션이 살아 있습니다")
	}

	// 멤버는 남의 2FA를 초기화할 수 없다.
	bob2 := e.client(t)
	bob2.do("POST", "/api/v1/auth/login", map[string]string{"username": "bob", "password": testPassword})
	if status, _ := bob2.do("POST", "/api/v1/users/"+e.user.ID+"/totp/reset", nil); status != 403 {
		t.Error("멤버가 남의 2FA를 초기화할 수 있습니다")
	}
}

// 복구 코드로도 2단계를 통과할 수 있어야 한다.
func TestRecoveryCodeLoginOverHTTP(t *testing.T) {
	e := newTestEnv(t)
	c := e.client(t)
	c.do("POST", "/api/v1/auth/login", map[string]string{"username": "alice", "password": testPassword})
	codes := c.enrollTOTP(t, e, e.user.ID)
	if len(codes) == 0 {
		t.Fatal("복구 코드가 발급되지 않았습니다")
	}

	fresh := e.client(t)
	fresh.do("POST", "/api/v1/auth/login", map[string]string{"username": "alice", "password": testPassword})
	if status, body := fresh.do("POST", "/api/v1/auth/login/totp",
		map[string]string{"code": codes[0]}); status != 200 {
		t.Fatalf("복구 코드 로그인 = %d: %v", status, body)
	}
	if _, ok := fresh.cookies[auth.SessionCookieName]; !ok {
		t.Fatal("복구 코드로 로그인했는데 세션이 없습니다")
	}
}

// 보안 화면은 아직 등록하지 않은 인원 수를 알려 줘야 한다.
// 그 숫자를 모른 채 의무화를 켜면 다음 날 문의가 몰린다.
func TestSecurityStatusReportsEnrollment(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	hash, _ := crypto.HashPassword(testPassword)
	if _, err := e.st.CreateUser(ctx, store.CreateUserParams{
		Username: "bob", Role: model.RoleMember, PasswordHash: hash,
	}); err != nil {
		t.Fatal(err)
	}

	admin := e.client(t)
	admin.do("POST", "/api/v1/auth/login", map[string]string{"username": "alice", "password": testPassword})
	admin.enrollTOTP(t, e, e.user.ID)

	status, body := admin.do("GET", "/api/v1/security/", nil)
	if status != 200 {
		t.Fatalf("보안 상태 = %d: %v", status, body)
	}
	summary, _ := body["totp"].(map[string]any)
	if summary["enrolled"] != float64(1) || summary["missing"] != float64(1) {
		t.Errorf("등록 현황 = %v, want enrolled 1 / missing 1", summary)
	}
	// 내부 시계 상태도 함께 나와야 한다. 시각 문제를 진단할 유일한 화면이다.
	clockInfo, _ := body["clock"].(map[string]any)
	if clockInfo == nil || clockInfo["internalTime"] == nil {
		t.Errorf("시계 상태가 없습니다: %v", body)
	}
}

// 등록 강제 경로 목록은 보안에 직결되므로 값 자체를 고정한다.
// 여기에 경로를 하나 더할 때마다 "2FA 없이 부를 수 있는 API"가 하나 늘어난다.
func TestTOTPSetupExemptList(t *testing.T) {
	app := fiberTestApp(t)
	allowed := []string{
		"/api/v1/auth/totp", "/api/v1/auth/totp/setup", "/api/v1/auth/totp/confirm",
		"/api/v1/auth/me", "/api/v1/auth/logout", "/api/v1/auth/password", "/api/v1/meta",
	}
	blocked := []string{
		"/api/v1/connections/", "/api/v1/users/", "/api/v1/auth/tokens",
		"/api/v1/auth/totp/disable", "/api/v1/auth/totp/recovery", "/api/v1/security/",
	}
	for _, p := range allowed {
		if !app(p) {
			t.Errorf("%s 가 막혔습니다 (등록을 마칠 수 없게 됩니다)", p)
		}
	}
	for _, p := range blocked {
		if app(p) {
			t.Errorf("%s 가 2FA 없이 열려 있습니다", p)
		}
	}
}

// fiberTestApp은 경로 하나를 isTOTPSetupExempt에 넣어 보는 도우미를 만든다.
// c.Path()는 라우팅을 거쳐야 채워지므로 최소 앱을 하나 세운다.
func fiberTestApp(t *testing.T) func(string) bool {
	t.Helper()
	return func(path string) bool {
		var got bool
		app := fiber.New()
		app.All("/*", func(c *fiber.Ctx) error {
			got = isTOTPSetupExempt(c)
			return c.SendStatus(fiber.StatusNoContent)
		})
		if _, err := app.Test(httptest.NewRequest("GET", path, nil), -1); err != nil {
			t.Fatalf("test %s: %v", path, err)
		}
		return got
	}
}
