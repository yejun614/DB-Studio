package bootstrap

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dbstudio/internal/crypto"
	"dbstudio/internal/model"
	"dbstudio/internal/store"
)

func fixture(t *testing.T) (context.Context, *store.Store) {
	t.Helper()
	ctx := context.Background()
	box, err := crypto.NewSecretBox(make([]byte, 32))
	if err != nil {
		t.Fatalf("secret box: %v", err)
	}
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "meta.db"), box)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return ctx, st
}

// 부트스트랩은 사용자가 없을 때만 계정을 만든다.
func TestEnsureSuperadminOnlyOnce(t *testing.T) {
	ctx, st := fixture(t)

	first, err := EnsureSuperadmin(ctx, st)
	if err != nil {
		t.Fatalf("%v", err)
	}
	if !first.Created || first.Username != DefaultSuperadminUsername {
		t.Fatalf("%+v", first)
	}
	if len(first.Password) < 16 {
		t.Errorf("비밀번호가 짧습니다: %d자", len(first.Password))
	}

	again, err := EnsureSuperadmin(ctx, st)
	if err != nil {
		t.Fatalf("%v", err)
	}
	if again.Created {
		t.Error("이미 사용자가 있는데 또 만들었습니다")
	}
}

// 다시 정한 비밀번호로 로그인되고, 예전 것으로는 안 된다.
//
// 해시가 바뀌었는지만 보면 부족하다 — 형식이 맞는 해시를 넣어도 그 비밀번호를
// 받지 못하는 상태가 될 수 있고, 그때 화면에서는 "비밀번호가 틀렸다"로만 보인다.
func TestResetPasswordReplacesTheHash(t *testing.T) {
	ctx, st := fixture(t)
	boot, err := EnsureSuperadmin(ctx, st)
	if err != nil {
		t.Fatalf("%v", err)
	}

	res, err := ResetPassword(ctx, st, DefaultSuperadminUsername)
	if err != nil {
		t.Fatalf("%v", err)
	}
	if res.Password == boot.Password {
		t.Fatal("같은 비밀번호가 나왔습니다")
	}
	if len(res.Password) < 16 {
		t.Errorf("비밀번호가 짧습니다: %d자", len(res.Password))
	}

	u, err := st.GetUserByUsername(ctx, DefaultSuperadminUsername)
	if err != nil {
		t.Fatalf("%v", err)
	}
	if ok, err := crypto.VerifyPassword(res.Password, u.PasswordHash); err != nil || !ok {
		t.Errorf("새 비밀번호가 통하지 않습니다: ok=%v err=%v", ok, err)
	}
	if ok, _ := crypto.VerifyPassword(boot.Password, u.PasswordHash); ok {
		t.Error("예전 비밀번호가 아직 통합니다")
	}
	// 새 비밀번호는 터미널에 찍힌다. 첫 로그인에서 바꾸게 해야 그 값이 오래 살지 않는다.
	if !u.MustChangePassword {
		t.Error("변경 강제가 켜지지 않았습니다")
	}
}

// 세션을 지워야 한다.
//
// 남겨 두면 예전 비밀번호로 받은 쿠키가 그대로 살아 있다 — 비밀번호를 바꿨는데
// 그 계정을 쥔 사람은 아무 일도 없었던 것처럼 계속 쓴다.
func TestResetPasswordRevokesSessions(t *testing.T) {
	ctx, st := fixture(t)
	if _, err := EnsureSuperadmin(ctx, st); err != nil {
		t.Fatalf("%v", err)
	}
	u, err := st.GetUserByUsername(ctx, DefaultSuperadminUsername)
	if err != nil {
		t.Fatalf("%v", err)
	}
	const token = "쿠키에-들어가는-값"
	if _, err := st.CreateSession(ctx, token, u.ID, time.Hour, "127.0.0.1", "검사"); err != nil {
		t.Fatalf("세션: %v", err)
	}
	if _, _, err := st.LookupSession(ctx, token); err != nil {
		t.Fatalf("만든 세션을 못 찾습니다: %v", err)
	}

	if _, err := ResetPassword(ctx, st, DefaultSuperadminUsername); err != nil {
		t.Fatalf("%v", err)
	}
	if _, _, err := st.LookupSession(ctx, token); err == nil {
		t.Error("세션이 살아 있습니다")
	}
}

// 감사 기록이 남아야 한다. 이 명령은 권한 화면을 거치지 않으므로,
// 남는 곳이 여기뿐이다.
func TestResetPasswordIsAudited(t *testing.T) {
	ctx, st := fixture(t)
	if _, err := EnsureSuperadmin(ctx, st); err != nil {
		t.Fatalf("%v", err)
	}
	if _, err := ResetPassword(ctx, st, DefaultSuperadminUsername); err != nil {
		t.Fatalf("%v", err)
	}

	logs, _, err := st.ListAudit(ctx, store.AuditFilter{Action: store.ActionUserPasswordSet, Limit: 10})
	if err != nil {
		t.Fatalf("감사 조회: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("감사 기록이 %d줄입니다", len(logs))
	}
	if logs[0].ActorName != "cli" {
		t.Errorf("행위자 = %q (기대 cli)", logs[0].ActorName)
	}
	// 비밀번호가 감사 기록에 실려서는 안 된다. 여기 남으면 감사 화면을 볼 수
	// 있는 사람 모두가 그 값을 본다.
	detail := logs[0].Detail
	for k, v := range detail {
		if s, ok := v.(string); ok && len(s) >= 16 && !strings.Contains(s, " ") && k != "username" {
			t.Errorf("감사 기록에 비밀번호처럼 보이는 값이 있습니다: %s=%q", k, s)
		}
	}
}

// 아이디를 비우면 슈퍼 어드민이다. 이 명령이 필요한 순간의 대개가 그 계정이다.
func TestResetPasswordDefaultsToSuperadmin(t *testing.T) {
	ctx, st := fixture(t)
	if _, err := EnsureSuperadmin(ctx, st); err != nil {
		t.Fatalf("%v", err)
	}
	res, err := ResetPassword(ctx, st, "  ")
	if err != nil {
		t.Fatalf("%v", err)
	}
	if res.Username != DefaultSuperadminUsername {
		t.Errorf("아이디 = %q", res.Username)
	}
}

// 슈퍼 어드민이 아닌 계정도 바꿀 수 있다.
//
// 막지 않는 이유는 ResetPassword 의 주석에 있다 — 데이터 디렉터리와 master.key 를
// 가진 사람은 이미 모든 것을 가진 사람이라, 역할을 따져도 얻는 것이 없다.
func TestResetPasswordWorksForAnyUser(t *testing.T) {
	ctx, st := fixture(t)
	boot, err := EnsureSuperadmin(ctx, st)
	if err != nil {
		t.Fatalf("%v", err)
	}
	hash, err := crypto.HashPassword("처음-비밀번호-1")
	if err != nil {
		t.Fatalf("%v", err)
	}
	if _, err := st.CreateUser(ctx, store.CreateUserParams{
		Username: "dev1", DisplayName: "개발자", Role: model.RoleMember, PasswordHash: hash,
	}); err != nil {
		t.Fatalf("%v", err)
	}

	res, err := ResetPassword(ctx, st, "dev1")
	if err != nil {
		t.Fatalf("%v", err)
	}
	u, _ := st.GetUserByUsername(ctx, "dev1")
	if ok, _ := crypto.VerifyPassword(res.Password, u.PasswordHash); !ok {
		t.Error("새 비밀번호가 통하지 않습니다")
	}
	// 다른 계정은 건드리지 않는다. 슈퍼 어드민의 비밀번호가 그대로여야 한다 —
	// 여기서 "변경 강제"를 보면 안 되는 이유: 부트스트랩이 그것을 이미 켜 둔다.
	admin, _ := st.GetUserByUsername(ctx, DefaultSuperadminUsername)
	if ok, _ := crypto.VerifyPassword(boot.Password, admin.PasswordHash); !ok {
		t.Error("엉뚱한 계정의 비밀번호가 바뀌었습니다")
	}
}

// 없는 아이디에는 있는 아이디들을 알려 준다.
//
// 오타 하나로 실패했을 때 그것을 모르면 사람은 데이터 디렉터리를 의심하고
// 엉뚱한 곳을 뒤진다.
func TestResetPasswordUnknownUserListsKnownOnes(t *testing.T) {
	ctx, st := fixture(t)
	if _, err := EnsureSuperadmin(ctx, st); err != nil {
		t.Fatalf("%v", err)
	}
	_, err := ResetPassword(ctx, st, "supperadmin")
	if err == nil {
		t.Fatal("없는 계정을 바꿨습니다")
	}
	if !strings.Contains(err.Error(), "superadmin") {
		t.Errorf("있는 아이디를 알려 주지 않습니다: %v", err)
	}
}
