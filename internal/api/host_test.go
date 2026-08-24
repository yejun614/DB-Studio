package api

import (
	"context"
	"strings"
	"testing"

	"dbstudio/internal/crypto"
	"dbstudio/internal/model"
	"dbstudio/internal/store"
)

// 호스트 화면에는 컴퓨터 이름·디스크 경로·시스템 로그가 담긴다. 특정 DB를 볼 수 있는
// 권한으로는 볼 수 없어야 하고, 그 규칙은 라우팅·미들웨어·핸들러가 모두 맞아야
// 성립하므로 HTTP로 확인한다.

func addUserWithRole(t *testing.T, e *testEnv, username string, role model.Role) {
	t.Helper()
	hash, err := crypto.HashPassword(testPassword)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if _, err := e.st.CreateUser(context.Background(), store.CreateUserParams{
		Username: username, DisplayName: username, Role: role, PasswordHash: hash,
	}); err != nil {
		t.Fatalf("create user: %v", err)
	}
}

func TestHostViewNeedsConnectionAdmin(t *testing.T) {
	e := newTestEnv(t)
	addUserWithRole(t, e, "bob", model.RoleMember)

	admin := login(t, e, "alice") // 부트스트랩 계정은 슈퍼 어드민이다
	if status, body := admin.do("GET", "/api/v1/monitor/host", nil); status != 200 {
		t.Fatalf("어드민이 호스트 상태를 못 봅니다: %d %v", status, body)
	}

	member := login(t, e, "bob")
	if status, _ := member.do("GET", "/api/v1/monitor/host", nil); status != 403 {
		t.Errorf("일반 멤버에게 호스트 상태가 열려 있습니다: %d", status)
	}
	if status, _ := member.do("PUT", "/api/v1/monitor/host/thresholds",
		map[string]any{"cpuWarn": 10}); status != 403 {
		t.Errorf("일반 멤버가 임계값을 바꿀 수 있습니다: %d", status)
	}
}

func TestHostThresholdsRoundTrip(t *testing.T) {
	e := newTestEnv(t)
	c := login(t, e, "alice")

	status, body := c.do("PUT", "/api/v1/monitor/host/thresholds", map[string]any{
		"cpuWarn": 70, "cpuCrit": 90, "memWarn": 75, "memCrit": 92,
		"diskWarn": 80, "diskCrit": 93, "sustainSec": 60, "osLogEnabled": false,
	})
	if status != 200 {
		t.Fatalf("저장 = %d: %v", status, body)
	}
	th, _ := body["thresholds"].(map[string]any)
	if th["cpuWarn"] != 70.0 || th["diskCrit"] != 93.0 || th["sustainSec"] != 60.0 {
		t.Errorf("저장한 값이 돌아오지 않았습니다: %v", th)
	}
	if th["osLogEnabled"] != false {
		t.Errorf("시스템 로그 설정이 반영되지 않았습니다: %v", th["osLogEnabled"])
	}

	// 범위를 벗어난 값은 기본값으로 되돌린다. 0이 그대로 저장되면 "항상 위반"이 되어
	// 이벤트가 끝없이 열린다.
	status, body = c.do("PUT", "/api/v1/monitor/host/thresholds",
		map[string]any{"cpuWarn": 0, "cpuCrit": 0})
	if status != 200 {
		t.Fatalf("저장 = %d: %v", status, body)
	}
	th, _ = body["thresholds"].(map[string]any)
	if th["cpuWarn"] == 0.0 || th["cpuCrit"] == 0.0 {
		t.Errorf("0이 그대로 저장됐습니다: %v", th)
	}
}

func TestHostEventsHiddenFromMembers(t *testing.T) {
	e := newTestEnv(t)
	addUserWithRole(t, e, "bob", model.RoleMember)

	value := 97.0
	if _, _, err := e.st.OpenEvent(context.Background(), store.OpenEventParams{
		Kind: store.EventHost, Severity: store.SeverityCritical, Metric: "host.disk:/",
		Message: "디스크가 찼습니다", Value: &value,
	}); err != nil {
		t.Fatalf("open event: %v", err)
	}

	// 커넥션 관리자에게는 보인다.
	_, body := login(t, e, "alice").do("GET", "/api/v1/monitor/events?state=open", nil)
	events, _ := body["events"].([]any)
	if len(events) != 1 {
		t.Fatalf("어드민에게 호스트 이벤트가 보이지 않습니다: %v", body)
	}

	// 커넥션이 하나도 없는 멤버에게는 보이지 않는다.
	// 이 이벤트에는 커넥션이 없으므로, 권한 판정을 커넥션 목록에만 맡기면
	// "목록이 비어 있어 전체 공개"가 되는 실수가 나기 쉽다.
	_, body = login(t, e, "bob").do("GET", "/api/v1/monitor/events?state=open", nil)
	events, _ = body["events"].([]any)
	if len(events) != 0 {
		t.Errorf("일반 멤버에게 호스트 이벤트가 노출됩니다: %v", body)
	}
	summary := map[string]any{}
	if _, ov := login(t, e, "bob").do("GET", "/api/v1/monitor/overview", nil); ov != nil {
		summary, _ = ov["summary"].(map[string]any)
	}
	if summary["openCritical"] != 0.0 {
		t.Errorf("요약 배지에 호스트 이벤트가 새어 나옵니다: %v", summary)
	}
}

// 타입 카탈로그는 ERD 화면이 타입 고르개를 그리는 근거다. 목록이 비거나
// dialect 확인이 없으면 화면은 고를 것이 없거나 엉뚱한 목록을 그린다.
func TestERDTypeCatalog(t *testing.T) {
	e := newTestEnv(t)
	c := login(t, e, "alice")

	status, body := c.do("GET", "/api/v1/erd/types?dialect=postgres", nil)
	if status != 200 {
		t.Fatalf("catalog = %d: %v", status, body)
	}
	types, _ := body["types"].([]any)
	if len(types) < 20 {
		t.Fatalf("타입이 너무 적습니다: %d개", len(types))
	}
	if body["arrays"] != true {
		t.Error("PostgreSQL은 배열을 지원한다고 알려야 합니다")
	}
	if body["autoIncrement"] == "" {
		t.Error("자동 증가 이름이 비어 있습니다")
	}

	if status, _ := c.do("GET", "/api/v1/erd/types", nil); status != 400 {
		t.Errorf("dialect 없이 = %d, 400이어야 합니다", status)
	}
	if status, _ := c.do("GET", "/api/v1/erd/types?dialect=nosuchdb", nil); status != 400 {
		t.Errorf("모르는 dialect = %d, 400이어야 합니다", status)
	}
}

// 알림 설정은 슈퍼 어드민만 다룬다. 웹훅 주소는 그것 하나로 채널에 글을 쓸 수 있는
// 비밀이므로, 응답에 그대로 실려 나가서도 안 된다.
func TestNotifySettingsFlow(t *testing.T) {
	e := newTestEnv(t)
	addUserWithRole(t, e, "bob", model.RoleAdmin)
	c := login(t, e, "alice")

	// 어드민(슈퍼 어드민 아님)은 볼 수 없다.
	if status, _ := login(t, e, "bob").do("GET", "/api/v1/notify/", nil); status != 403 {
		t.Errorf("어드민에게 알림 설정이 열려 있습니다: %d", status)
	}

	status, body := c.do("PUT", "/api/v1/notify/", map[string]any{
		"enabled": true, "webhookUrl": "https://mm.example.com/hooks/abcdefgh",
		"channel": "db-alerts", "minSeverity": "critical",
		"kinds": []string{"host", "connectivity"}, "includeResolved": true,
		"appUrl": "https://db.example.com/",
	})
	if status != 200 {
		t.Fatalf("저장 = %d: %v", status, body)
	}
	saved, _ := body["settings"].(map[string]any)
	url, _ := saved["webhookUrl"].(string)
	if strings.Contains(url, "abcdefgh") {
		t.Errorf("웹훅 주소가 그대로 응답에 실렸습니다: %q", url)
	}
	if !strings.Contains(url, "mm.example.com") {
		t.Errorf("어디로 가는지는 남아야 합니다: %q", url)
	}

	// 주소를 보내지 않은 저장은 기존 주소를 지우지 않아야 한다.
	// (화면은 마스킹된 값을 보여주므로 주소를 바꾸지 않으면 그 칸이 오지 않는다.)
	status, _ = c.do("PUT", "/api/v1/notify/", map[string]any{
		"enabled": true, "minSeverity": "warning", "includeResolved": false,
	})
	if status != 200 {
		t.Fatalf("두 번째 저장 = %d", status)
	}
	cfg, err := e.st.NotifySettings(context.Background())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if cfg.WebhookURL != "https://mm.example.com/hooks/abcdefgh" {
		t.Errorf("웹훅이 사라졌습니다: %q", cfg.WebhookURL)
	}
	if cfg.MinSeverity != store.SeverityWarning || cfg.IncludeResolved {
		t.Errorf("설정이 반영되지 않았습니다: %+v", cfg)
	}

	// 주소 없이 켤 수는 없다. 켜졌다고 표시되는데 아무것도 가지 않는 상태가 가장 나쁘다.
	e2 := newTestEnv(t)
	c2 := login(t, e2, "alice")
	if status, _ := c2.do("PUT", "/api/v1/notify/", map[string]any{"enabled": true}); status != 400 {
		t.Errorf("주소 없이 켜기 = %d, 400이어야 합니다", status)
	}
	// 형식이 아닌 주소도 막는다.
	if status, _ := c2.do("PUT", "/api/v1/notify/",
		map[string]any{"webhookUrl": "mm.example.com/hooks/x"}); status != 400 {
		t.Errorf("http가 아닌 주소 = %d, 400이어야 합니다", status)
	}
	// 모르는 종류를 조용히 무시하면 "고른 것만 온다"는 약속이 깨진다.
	if status, _ := c2.do("PUT", "/api/v1/notify/",
		map[string]any{"kinds": []string{"nosuchkind"}}); status != 400 {
		t.Errorf("모르는 종류 = %d, 400이어야 합니다", status)
	}
	if status, _ := c2.do("PUT", "/api/v1/notify/",
		map[string]any{"provider": "telegram"}); status != 400 {
		t.Errorf("모르는 메신저 = %d, 400이어야 합니다", status)
	}
}

// 메신저는 Mattermost·Slack·Discord 를 고를 수 있어야 하고, 고른 값이 저장·응답에
// 그대로 남아야 한다. 저장된 적 없는 서버도 화면이 고르개를 그릴 수 있게 기본값을 준다.
//
// 개수가 아니라 **이름**으로 확인하는 이유: 개수를 못박으면 메신저를 하나 더할 때마다
// 이 시험이 깨지는데, 그 실패는 "무엇이 잘못됐다"를 말해 주지 않는다.
func TestNotifyProviderChoice(t *testing.T) {
	e := newTestEnv(t)
	c := login(t, e, "alice")

	status, body := c.do("GET", "/api/v1/notify/", nil)
	if status != 200 {
		t.Fatalf("조회 = %d", status)
	}
	settings, _ := body["settings"].(map[string]any)
	if settings["provider"] != "mattermost" {
		t.Errorf("기본 메신저 = %v", settings["provider"])
	}
	providers, _ := body["providers"].([]any)
	seen := map[string]bool{}
	for _, raw := range providers {
		p, _ := raw.(map[string]any)
		value, _ := p["value"].(string)
		seen[value] = true
		// 안내가 비어 있으면 화면이 "이 주소를 어디서 만드는가"를 말해 줄 수 없다.
		if note, _ := p["note"].(string); note == "" {
			t.Errorf("메신저 %q 에 안내가 없습니다", value)
		}
	}
	for _, want := range []string{
		store.ProviderMattermost, store.ProviderSlack, store.ProviderDiscord,
	} {
		if !seen[want] {
			t.Errorf("메신저 %q 를 고를 수 없습니다", want)
		}
	}

	if status, _ = c.do("PUT", "/api/v1/notify/", map[string]any{
		"provider": "slack", "webhookUrl": "https://hooks.slack.com/services/T1/B1/xyz",
		"enabled": true, "minSeverity": "warning",
	}); status != 200 {
		t.Fatalf("저장 = %d", status)
	}
	cfg, err := e.st.NotifySettings(context.Background())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if cfg.Kind() != store.ProviderSlack {
		t.Errorf("저장된 메신저 = %q", cfg.Kind())
	}
	// 메신저를 보내지 않은 저장은 지금 값을 지켜야 한다(부분 저장에서 되돌아가면 안 된다).
	if status, _ = c.do("PUT", "/api/v1/notify/",
		map[string]any{"enabled": true, "minSeverity": "critical"}); status != 200 {
		t.Fatalf("두 번째 저장 = %d", status)
	}
	cfg, _ = e.st.NotifySettings(context.Background())
	if cfg.Kind() != store.ProviderSlack {
		t.Errorf("메신저가 되돌아갔습니다: %q", cfg.Kind())
	}

	// 디스코드도 같은 경로로 저장된다(아는 메신저 목록에 들어 있어야 통과한다).
	if status, _ = c.do("PUT", "/api/v1/notify/", map[string]any{
		"provider": "discord", "webhookUrl": "https://discord.com/api/webhooks/1/abc",
		"enabled": true, "minSeverity": "warning",
	}); status != 200 {
		t.Fatalf("디스코드 저장 = %d", status)
	}
	cfg, _ = e.st.NotifySettings(context.Background())
	if cfg.Kind() != store.ProviderDiscord {
		t.Errorf("저장된 메신저 = %q", cfg.Kind())
	}
}
