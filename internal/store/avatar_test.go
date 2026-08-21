package store

import (
	"testing"
	"time"

	"dbstudio/internal/model"
)

// TestAvatarRoundTrip은 아바타가 저장되고 모든 조회 경로에서 함께 돌아오는지 확인한다.
// 세션 조회(LookupSession)를 빠뜨리면 사이드바가 로그인 직후에만 아이콘을 잃는,
// 재현하기 까다로운 증상이 된다.
func TestAvatarRoundTrip(t *testing.T) {
	ctx, st := userFixture(t)
	u := mkUser(t, ctx, st, "picker")

	if u.Avatar != "" {
		t.Errorf("새 사용자의 기본 아바타 = %q, 기대 빈 값", u.Avatar)
	}

	want := "role-dba"
	if _, err := st.UpdateUser(ctx, u.ID, UpdateUserParams{Avatar: &want}); err != nil {
		t.Fatalf("update: %v", err)
	}

	got, err := st.GetUser(ctx, u.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Avatar != want {
		t.Errorf("GetUser avatar = %q, 기대 %q", got.Avatar, want)
	}

	list, err := st.ListUsers(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, item := range list {
		if item.ID == u.ID && item.Avatar != want {
			t.Errorf("ListUsers avatar = %q, 기대 %q", item.Avatar, want)
		}
	}

	token := "token-for-picker"
	if _, err := st.CreateSession(ctx, token, u.ID, time.Hour, "192.0.2.7", "test"); err != nil {
		t.Fatalf("create session: %v", err)
	}
	_, su, err := st.LookupSession(ctx, token)
	if err != nil {
		t.Fatalf("lookup session: %v", err)
	}
	if su.Avatar != want {
		t.Errorf("LookupSession avatar = %q, 기대 %q", su.Avatar, want)
	}

	// 해제도 되어야 한다. 고를 수만 있고 되돌릴 수 없으면 선택이 아니다.
	none := ""
	if _, err := st.UpdateUser(ctx, u.ID, UpdateUserParams{Avatar: &none}); err != nil {
		t.Fatalf("clear: %v", err)
	}
	cleared, err := st.GetUser(ctx, u.ID)
	if err != nil {
		t.Fatalf("get after clear: %v", err)
	}
	if cleared.Avatar != "" {
		t.Errorf("해제 후 avatar = %q", cleared.Avatar)
	}
}

// TestAvatarOnlyUpdateKeepsOtherFields는 아바타만 바꿀 때 다른 필드가 유지되는지 확인한다.
func TestAvatarOnlyUpdateKeepsOtherFields(t *testing.T) {
	ctx, st := userFixture(t)
	u := mkUser(t, ctx, st, "keeper")

	email := "keeper@example.com"
	if _, err := st.UpdateUser(ctx, u.ID, UpdateUserParams{Email: &email}); err != nil {
		t.Fatalf("set email: %v", err)
	}

	avatar := "person-glasses"
	got, err := st.UpdateUser(ctx, u.ID, UpdateUserParams{Avatar: &avatar})
	if err != nil {
		t.Fatalf("update avatar: %v", err)
	}
	if got.Email != email {
		t.Errorf("email = %q, 기대 %q", got.Email, email)
	}
	if got.DisplayName != "keeper" {
		t.Errorf("displayName = %q", got.DisplayName)
	}
	if got.Role != model.RoleMember {
		t.Errorf("role = %q", got.Role)
	}
}
