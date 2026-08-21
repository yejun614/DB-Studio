package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"dbstudio/internal/crypto"
	"dbstudio/internal/model"
)

func userFixture(t *testing.T) (context.Context, *Store) {
	t.Helper()
	ctx := context.Background()
	box, err := crypto.NewSecretBox(make([]byte, 32))
	if err != nil {
		t.Fatalf("secret box: %v", err)
	}
	st, err := Open(ctx, filepath.Join(t.TempDir(), "users.db"), box)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return ctx, st
}

func mkUser(t *testing.T, ctx context.Context, st *Store, name string) *model.User {
	t.Helper()
	u, err := st.CreateUser(ctx, CreateUserParams{
		Username: name, DisplayName: name, Role: model.RoleMember,
		PasswordHash: "hash", MustChangePassword: false,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	return u
}

// TestLastLoginRecordsIP는 로그인 시각과 그때의 IP가 함께 저장되고
// 모든 조회 경로에서 함께 돌아오는지 확인한다.
//
// 세션 테이블에도 IP가 있지만 세션은 만료되면 사라진다. 사용자 행에 남기지 않으면
// "마지막으로 어디서 접속했는가"를 나중에 알 수 없다.
func TestLastLoginRecordsIP(t *testing.T) {
	ctx, st := userFixture(t)
	u := mkUser(t, ctx, st, "alice")

	if u.LastLoginAt != nil || u.LastLoginIP != "" {
		t.Errorf("새 사용자에 로그인 기록이 있습니다: at=%v ip=%q", u.LastLoginAt, u.LastLoginIP)
	}

	if err := st.TouchLastLogin(ctx, u.ID, "203.0.113.9"); err != nil {
		t.Fatalf("touch: %v", err)
	}

	got, err := st.GetUser(ctx, u.ID)
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	if got.LastLoginIP != "203.0.113.9" {
		t.Errorf("GetUser의 IP = %q", got.LastLoginIP)
	}
	if got.LastLoginAt == nil {
		t.Error("GetUser의 로그인 시각이 없습니다")
	}

	// 목록(사용자 관리 화면이 쓰는 경로)에서도 보여야 한다.
	list, err := st.ListUsers(ctx)
	if err != nil {
		t.Fatalf("list users: %v", err)
	}
	var found *model.User
	for _, x := range list {
		if x.ID == u.ID {
			found = x
		}
	}
	if found == nil {
		t.Fatal("목록에 사용자가 없습니다")
	}
	if found.LastLoginIP != "203.0.113.9" {
		t.Errorf("ListUsers의 IP = %q", found.LastLoginIP)
	}

	// 다시 로그인하면 시각과 IP가 함께 갱신되어야 한다.
	// 하나만 갱신되면 화면에서 "언제"와 "어디서"가 다른 로그인의 것이 된다.
	before := *found.LastLoginAt
	if err := st.TouchLastLogin(ctx, u.ID, "198.51.100.7"); err != nil {
		t.Fatalf("touch again: %v", err)
	}
	again, err := st.GetUser(ctx, u.ID)
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	if again.LastLoginIP != "198.51.100.7" {
		t.Errorf("두 번째 IP = %q", again.LastLoginIP)
	}
	if again.LastLoginAt.Before(before) {
		t.Errorf("로그인 시각이 뒤로 갔습니다: %v → %v", before, again.LastLoginAt)
	}
}

// TestLastLoginIPInSession은 세션 조회 경로(LookupSession)에서도 값이 채워지는지 확인한다.
// 이 경로는 매 요청의 인증에 쓰이며, 컬럼을 추가할 때 함께 고치지 않으면
// 스캔 순서가 어긋나 인증 자체가 깨진다.
func TestLastLoginIPInSession(t *testing.T) {
	ctx, st := userFixture(t)
	u := mkUser(t, ctx, st, "bob")
	if err := st.TouchLastLogin(ctx, u.ID, "192.0.2.44"); err != nil {
		t.Fatalf("touch: %v", err)
	}

	token := "token-for-bob"
	if _, err := st.CreateSession(ctx, token, u.ID, 3600_000_000_000, "192.0.2.44", "test-agent"); err != nil {
		t.Fatalf("create session: %v", err)
	}
	sess, user, err := st.LookupSession(ctx, token)
	if err != nil {
		t.Fatalf("lookup session: %v", err)
	}
	if user.Username != "bob" {
		t.Errorf("세션 조회의 사용자 = %q (스캔 순서가 어긋났을 수 있습니다)", user.Username)
	}
	if user.LastLoginIP != "192.0.2.44" {
		t.Errorf("세션 조회의 마지막 로그인 IP = %q", user.LastLoginIP)
	}
	if sess.IP != "192.0.2.44" {
		t.Errorf("세션 IP = %q", sess.IP)
	}
}

// TestLastLoginIPEmptyForLegacyRows는 컬럼 추가 이전에 로그인한 행을 확인한다.
// 마이그레이션 기본값이 비어 있으므로 화면은 "기록 없음"으로 구분해 보여줄 수 있어야 한다.
func TestLastLoginIPEmptyForLegacyRows(t *testing.T) {
	ctx, st := userFixture(t)
	u := mkUser(t, ctx, st, "carol")

	// 컬럼이 없던 시절의 갱신을 흉내낸다: 시각만 있고 IP는 비어 있다.
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE users SET last_login_at = ? WHERE id = ?`, nowString(), u.ID); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, err := st.GetUser(ctx, u.ID)
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	if got.LastLoginAt == nil {
		t.Fatal("로그인 시각이 없습니다")
	}
	if got.LastLoginIP != "" {
		t.Errorf("옛 행의 IP = %q, 빈 문자열이어야 합니다", got.LastLoginIP)
	}
}

// TestPermsSurviveSessionLookup은 전역 권한이 세션 조회 경로에서도 함께 오는지 확인한다.
//
// 이것이 빠지면 증상이 고약하다: 권한을 주는 화면에서는 값이 제대로 보이고(GetUser는
// perms를 읽는다), 실제 요청만 "권한 없음"으로 막힌다. 게다가 슈퍼 어드민은 역할로
// 통과하므로 관리자가 직접 눌러 보면 잘 된다 — 멤버에게만 나타나는 버그가 된다.
func TestPermsSurviveSessionLookup(t *testing.T) {
	ctx, st := userFixture(t)
	u := mkUser(t, ctx, st, "macrouser")

	perms := []model.Perm{model.PermMacro, model.PermHTTPCall}
	if _, err := st.UpdateUser(ctx, u.ID, UpdateUserParams{Perms: &perms}); err != nil {
		t.Fatalf("update: %v", err)
	}

	token := "token-for-macrouser"
	if _, err := st.CreateSession(ctx, token, u.ID, time.Hour, "192.0.2.9", "test"); err != nil {
		t.Fatalf("create session: %v", err)
	}
	_, su, err := st.LookupSession(ctx, token)
	if err != nil {
		t.Fatalf("lookup session: %v", err)
	}
	if !su.HasPerm(model.PermMacro) || !su.HasPerm(model.PermHTTPCall) {
		t.Fatalf("LookupSession perms = %v, 기대 %v", su.Perms, perms)
	}
	if su.HasPerm(model.PermScriptRun) {
		t.Errorf("주지 않은 권한이 붙었다: %v", su.Perms)
	}
}
