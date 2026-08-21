package api

import (
	"io"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v2"

	"dbstudio/internal/config"
)

// newIPProbe는 clientIP가 무엇을 반환하는지 그대로 돌려주는 앱을 만든다.
// 실제 서버와 같은 fiberConfig를 쓰는 것이 요점이다 — 설정이 다르면 테스트가 의미 없다.
func newIPProbe(cfg *config.Config) *fiber.App {
	app := fiber.New(fiberConfig(cfg))
	app.Get("/ip", func(c *fiber.Ctx) error { return c.SendString(clientIP(c)) })
	return app
}

func probeIP(t *testing.T, app *fiber.App, xff string) string {
	t.Helper()
	req := httptest.NewRequest("GET", "/ip", nil)
	// httptest 요청의 원격 주소는 fasthttp에서 0.0.0.0으로 보이므로,
	// 값 자체보다 "헤더를 썼는가"를 판단 기준으로 삼는다.
	if xff != "" {
		req.Header.Set(fiber.HeaderXForwardedFor, xff)
	}
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("test request: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return string(body)
}

// TestClientIPIgnoresProxyHeaderByDefault는 기본 설정에서 X-Forwarded-For를
// 무시하는지 확인한다. 이것이 깨지면 앱에 직접 닿을 수 있는 누구나
// 자기 IP를 위조해 로그인 기록·감사 로그를 오염시킬 수 있다.
func TestClientIPIgnoresProxyHeaderByDefault(t *testing.T) {
	app := newIPProbe(&config.Config{TrustProxy: false})
	got := probeIP(t, app, "9.9.9.9")
	if got == "9.9.9.9" {
		t.Error("trust-proxy가 꺼진 상태에서 X-Forwarded-For를 신뢰했습니다 (IP 위조 가능)")
	}
	if got == "" {
		t.Error("IP가 비었습니다. 빈 값은 \"기록 없음\"과 구분되지 않습니다")
	}
}

// TestClientIPUsesProxyHeaderWhenTrusted는 신뢰하는 프록시가 보낸 헤더를 쓰는지 확인한다.
func TestClientIPUsesProxyHeaderWhenTrusted(t *testing.T) {
	// app.Test의 원격 주소는 0.0.0.0이므로 그것을 신뢰 목록에 넣어
	// "프록시 뒤" 상황을 만든다.
	cfg := &config.Config{TrustProxy: true, TrustedProxies: []string{"0.0.0.0/0"}}
	app := newIPProbe(cfg)

	cases := []struct{ name, xff, want string }{
		{"단일 IP", "203.0.113.55", "203.0.113.55"},
		// 체인의 맨 앞이 실제 클라이언트다. 헤더 전체가 저장되면 IP가 아닌 문자열이 남는다.
		{"프록시 체인", "198.51.100.20, 10.0.0.5", "198.51.100.20"},
		{"IPv6", "2001:db8::1234", "2001:db8::1234"},
	}
	for _, tc := range cases {
		if got := probeIP(t, app, tc.xff); got != tc.want {
			t.Errorf("%s: clientIP = %q, %q를 기대했습니다", tc.name, got, tc.want)
		}
	}
}

// TestClientIPFallsBackOnBadHeader는 헤더가 없거나 형식이 틀릴 때
// 빈 문자열이 아니라 실제 원격 주소로 되돌아가는지 확인한다.
//
// IP 검증을 켜면 Fiber는 잘못된 헤더에 빈 문자열을 돌려준다. 그것을 그대로 저장하면
// 화면에서 "IP 기록 없음"으로 보여 실제로는 접속했는데 기록이 없는 것처럼 된다.
func TestClientIPFallsBackOnBadHeader(t *testing.T) {
	cfg := &config.Config{TrustProxy: true, TrustedProxies: []string{"0.0.0.0/0"}}
	app := newIPProbe(cfg)

	for _, xff := range []string{"", "not-an-ip", "999.999.999.999", ","} {
		got := probeIP(t, app, xff)
		if got == "" {
			t.Errorf("X-Forwarded-For=%q 에서 IP가 비었습니다", xff)
		}
		if got == xff && xff != "" {
			t.Errorf("X-Forwarded-For=%q 를 IP로 그대로 저장했습니다", xff)
		}
	}
}

// TestDefaultTrustedProxiesArePrivate는 기본 신뢰 목록이 사설망/루프백인지 확인한다.
// 공개 대역이 섞이면 인터넷에서 온 위조 헤더를 신뢰하게 된다.
func TestDefaultTrustedProxiesArePrivate(t *testing.T) {
	cfg, err := config.Load([]string{"-data", t.TempDir()})
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if len(cfg.TrustedProxies) == 0 {
		t.Fatal("기본 신뢰 프록시 목록이 비어 있습니다")
	}
	for _, p := range cfg.TrustedProxies {
		switch p {
		case "127.0.0.0/8", "::1/128", "10.0.0.0/8", "172.16.0.0/12",
			"192.168.0.0/16", "169.254.0.0/16", "fc00::/7", "fe80::/10":
		default:
			t.Errorf("기본 목록에 사설망이 아닌 항목이 있습니다: %q", p)
		}
	}
}
