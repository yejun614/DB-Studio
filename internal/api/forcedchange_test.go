package api

import (
	"context"
	"testing"

	"dbstudio/internal/crypto"
	"dbstudio/internal/model"
	"dbstudio/internal/store"
)

// 비밀번호를 바꾸지 않고 로그아웃할 수 있어야 한다.
//
// 강제 변경 화면은 앱 전체를 가리고 서 있다. 나갈 문이 없으면 임시 비밀번호를 받아 든
// 사람이 한 번 로그인한 컴퓨터에서, 그 컴퓨터의 주인은 자기 계정으로 들어갈 방법이
// 없어진다. 그리고 그것은 강제의 우회가 아니다 — 그 계정으로 다시 들어오면 같은
// 화면이 그대로 다시 선다.
func TestForcedPasswordChangeAllowsLogout(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()

	hash, err := crypto.HashPassword(testPassword)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if _, err := e.st.CreateUser(ctx, store.CreateUserParams{
		Username: "newbie", DisplayName: "Newbie", Role: model.RoleMember,
		PasswordHash: hash, MustChangePassword: true,
	}); err != nil {
		t.Fatalf("create user: %v", err)
	}

	c := loginAs(t, e, "newbie")

	// 화면을 그리는 데 필요한 것은 열려 있다.
	if code, body := c.do("GET", "/api/v1/auth/me", nil); code != 200 {
		t.Fatalf("auth/me = %d: %v", code, body)
	}
	// 그 밖의 것은 막혀 있다(강제가 살아 있다).
	code, body := c.do("GET", "/api/v1/connections/", nil)
	if code != 403 || body["error"] != "password_change_required" {
		t.Errorf("다른 API = %d %v, 막혀 있어야 합니다", code, body["error"])
	}

	// 나가는 문은 열려 있다.
	if code, body := c.do("POST", "/api/v1/auth/logout", nil); code != 200 {
		t.Fatalf("로그아웃 = %d: %v", code, body)
	}
	// 나간 뒤 다른 계정으로 들어올 수 있다.
	alice := loginAs(t, e, "alice")
	if code, body := alice.do("GET", "/api/v1/connections/", nil); code != 200 {
		t.Errorf("다른 계정 로그인 뒤 = %d: %v", code, body)
	}

	// 강제는 그대로 남는다. 다시 들어오면 같은 화면이 선다.
	again := loginAs(t, e, "newbie")
	_, me := again.do("GET", "/api/v1/auth/me", nil)
	user, _ := me["user"].(map[string]any)
	if user["mustChangePassword"] != true {
		t.Errorf("로그아웃으로 강제가 풀렸습니다: %v", user["mustChangePassword"])
	}
}
