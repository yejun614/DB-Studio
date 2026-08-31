package api

import (
	"context"
	"testing"

	"dbstudio/internal/store"
)

// 재발급은 값만 바꾼다: 옛 값은 그 자리에서 401, 새 값은 통한다.
//
// 저장 계층 시험이 따로 있지만 여기서 봐야 하는 것이 있다 — HTTP를 지나면서 정말
// 옛 값이 죽는가. 이 기능의 값어치는 전부 거기에 있다.
func TestRotateTokenOverHTTP(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()

	saved, oldRaw, err := e.srv.authn.IssueAPIToken(ctx, store.CreateTokenParams{
		UserID: e.user.ID, Name: "재발급 대상", Scope: store.TokenScopeRead,
	})
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if status, _, _ := e.bearer(t, "GET", restBasePath, oldRaw, ""); status != 200 {
		t.Fatalf("재발급 전 = %d, want 200", status)
	}

	c := loginAs(t, e, e.user.Username)
	status, body := c.do("POST", "/api/v1/auth/tokens/"+saved.ID+"/rotate", map[string]any{})
	if status != 200 {
		t.Fatalf("재발급 = %d: %v", status, body)
	}
	newRaw, _ := body["value"].(string)
	if newRaw == "" || newRaw == oldRaw {
		t.Fatalf("새 값이 나오지 않았습니다: %q", newRaw)
	}

	if got, _, _ := e.bearer(t, "GET", restBasePath, oldRaw, ""); got != 401 {
		t.Errorf("재발급 뒤 옛 값 = %d, want 401", got)
	}
	if got, _, _ := e.bearer(t, "GET", restBasePath, newRaw, ""); got != 200 {
		t.Errorf("재발급 뒤 새 값 = %d, want 200", got)
	}

	// 이름·범위는 그대로여야 한다. 클라이언트 설정이 가리키는 것은 이름이다.
	after, err := e.st.GetAPIToken(ctx, saved.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if after.Name != saved.Name || after.Scope != saved.Scope {
		t.Errorf("이름·범위가 바뀌었습니다: %q/%q", after.Name, after.Scope)
	}
	if after.RotatedAt == nil {
		t.Error("재발급 시각이 비어 있습니다")
	}
}

// 남의 토큰은 재발급할 수 없고, 있는지조차 알려주지 않는다.
func TestRotateTokenIsOwnerOnly(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()

	other, err := e.st.CreateUser(ctx, store.CreateUserParams{
		Username: "otherowner", Role: "member", PasswordHash: "x",
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	saved, _, err := e.srv.authn.IssueAPIToken(ctx, store.CreateTokenParams{
		UserID: other.ID, Name: "남의 것", Scope: store.TokenScopeRead,
	})
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	c := loginAs(t, e, e.user.Username)
	if status, _ := c.do("POST", "/api/v1/auth/tokens/"+saved.ID+"/rotate", map[string]any{}); status != 404 {
		t.Errorf("남의 토큰 재발급 = %d, want 404 (있는지도 알려주지 않는다)", status)
	}
	after, err := e.st.GetAPIToken(ctx, saved.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if after.RotatedAt != nil {
		t.Error("남의 토큰이 재발급되었습니다")
	}
}

// 폐기 엔드포인트는 없다.
//
// 할 수 있는 일이 셋이면 "폐기와 삭제는 뭐가 다른가"를 매번 생각해야 해서, 값
// 바꾸기와 지우기 둘만 남겼다. 화면에서만 단추를 감추고 경로를 열어 두면 그 셋째
// 길은 문서에도 화면에도 없이 살아 있게 된다.
func TestRevokeEndpointIsGone(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()

	saved, _, err := e.srv.authn.IssueAPIToken(ctx, store.CreateTokenParams{
		UserID: e.user.ID, Name: "폐기 대상", Scope: store.TokenScopeRead,
	})
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	c := loginAs(t, e, e.user.Username)
	if status, _ := c.do("POST", "/api/v1/auth/tokens/"+saved.ID+"/revoke", map[string]any{}); status != 404 {
		t.Errorf("폐기 경로 = %d, want 404", status)
	}
	// 그래도 지우는 길은 남아 있어야 한다.
	if status, _ := c.do("DELETE", "/api/v1/auth/tokens/"+saved.ID, nil); status != 200 {
		t.Errorf("삭제 = %d, want 200", status)
	}
}
