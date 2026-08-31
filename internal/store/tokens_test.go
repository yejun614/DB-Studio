package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"dbstudio/internal/crypto"
	"dbstudio/internal/model"
)

func tokenFixture(t *testing.T) (context.Context, *Store, string) {
	t.Helper()
	ctx := context.Background()
	box, err := crypto.NewSecretBox(make([]byte, 32))
	if err != nil {
		t.Fatalf("secret box: %v", err)
	}
	st, err := Open(ctx, filepath.Join(t.TempDir(), "token.db"), box)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	u, err := st.CreateUser(ctx, CreateUserParams{
		Username: "tokenuser", Role: model.RoleMember, PasswordHash: "x",
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	return ctx, st, u.ID
}

// 값만 다시 발급하면 옛 값은 그 자리에서 죽고, 나머지는 그대로여야 한다.
//
// 이 둘이 이 기능의 전부다. 옛 값이 계속 통하면 재발급은 아무것도 막지 못하고,
// 이름이나 범위가 바뀌면 클라이언트 설정이 가리키던 것이 달라진다.
func TestRotateAPITokenReplacesValueOnly(t *testing.T) {
	ctx, st, userID := tokenFixture(t)
	const oldRaw = "dbs_oldvalue0000"
	const newRaw = "dbs_newvalue1111"

	expires := time.Now().Add(72 * time.Hour)
	made, err := st.CreateAPIToken(ctx, CreateTokenParams{
		UserID: userID, Name: "노트북", Scope: TokenScopeWrite,
		Token: oldRaw, Prefix: "dbs_oldva", ExpiresAt: &expires,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// 한 번 써 둔다. 재발급 뒤 이 기록이 지워져야 "새 값이 들어갔는가"를 볼 수 있다.
	st.TouchAPIToken(ctx, made, "10.0.0.1", 0)

	if err := st.RotateAPIToken(ctx, made.ID, userID, newRaw, "dbs_newva"); err != nil {
		t.Fatalf("rotate: %v", err)
	}

	// 옛 값은 더 이상 통하지 않는다.
	if _, _, err := st.LookupAPIToken(ctx, oldRaw); !errors.Is(err, ErrNotFound) {
		t.Errorf("재발급 뒤에도 옛 값이 통합니다: %v", err)
	}
	// 새 값은 통한다.
	got, _, err := st.LookupAPIToken(ctx, newRaw)
	if err != nil {
		t.Fatalf("새 값으로 찾기: %v", err)
	}
	if got.ID != made.ID {
		t.Errorf("다른 토큰이 되었습니다: %q → %q", made.ID, got.ID)
	}

	after, err := st.GetAPIToken(ctx, made.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if after.Name != "노트북" || after.Scope != TokenScopeWrite {
		t.Errorf("이름·범위가 바뀌었습니다: %q/%q", after.Name, after.Scope)
	}
	if after.ExpiresAt == nil || !after.ExpiresAt.Equal(*made.ExpiresAt) {
		t.Errorf("만료가 바뀌었습니다: %v → %v", made.ExpiresAt, after.ExpiresAt)
	}
	if after.Prefix != "dbs_newva" {
		t.Errorf("접두사 = %q, 새 값의 것이어야 합니다", after.Prefix)
	}
	if after.RotatedAt == nil {
		t.Error("재발급 시각이 비어 있습니다 — 목록이 옛 발급 시각을 보여주게 됩니다")
	}
	if !after.CreatedAt.Equal(made.CreatedAt) {
		t.Errorf("처음 만든 시각이 바뀌었습니다: %v → %v", made.CreatedAt, after.CreatedAt)
	}
	// 마지막 사용 기록은 없는 값의 것이므로 지워져야 한다.
	if after.LastUsedAt != nil || after.LastUsedIP != "" {
		t.Errorf("옛 값의 사용 기록이 남았습니다: %v / %q", after.LastUsedAt, after.LastUsedIP)
	}
}

// 폐기된 토큰은 되살리지 않는다.
//
// 되살리는 길을 열면 "폐기했다"는 기록이 무슨 뜻인지 흐려진다. 그때는 지우고 새로
// 만드는 것이 맞다.
func TestRotateAPITokenRefusesRevoked(t *testing.T) {
	ctx, st, userID := tokenFixture(t)
	made, err := st.CreateAPIToken(ctx, CreateTokenParams{
		UserID: userID, Name: "폐기됨", Scope: TokenScopeRead,
		Token: "dbs_deadvalue", Prefix: "dbs_deadv",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := st.RevokeAPIToken(ctx, made.ID, userID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if err := st.RotateAPIToken(ctx, made.ID, userID, "dbs_newvalue", "dbs_newva"); !errors.Is(err, ErrNotFound) {
		t.Errorf("폐기된 토큰의 값을 다시 발급했습니다: %v", err)
	}
}

// 남의 토큰은 재발급할 수 없다. 저장 계층에서도 막혀야 한다.
func TestRotateAPITokenIsOwnerOnly(t *testing.T) {
	ctx, st, userID := tokenFixture(t)
	made, err := st.CreateAPIToken(ctx, CreateTokenParams{
		UserID: userID, Name: "내 것", Scope: TokenScopeRead,
		Token: "dbs_minevalue", Prefix: "dbs_minev",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := st.RotateAPIToken(ctx, made.ID, "someone-else", "dbs_stolen", "dbs_stole"); !errors.Is(err, ErrNotFound) {
		t.Errorf("남의 토큰을 재발급했습니다: %v", err)
	}
	if _, _, err := st.LookupAPIToken(ctx, "dbs_minevalue"); err != nil {
		t.Errorf("원래 값이 망가졌습니다: %v", err)
	}
}

// 원문은 저장하지 않는다. DB가 유출되어도 토큰으로 로그인할 수 없어야 한다.
func TestAPITokenStoresHashOnly(t *testing.T) {
	ctx, st, userID := tokenFixture(t)
	const raw = "dbs_supersecretvalue"

	if _, err := st.CreateAPIToken(ctx, CreateTokenParams{
		UserID: userID, Name: "t", Scope: TokenScopeRead, Token: raw, Prefix: "dbs_super",
	}); err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}

	var count int
	if err := st.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM api_tokens WHERE token_hash = ?`, raw).Scan(&count); err != nil {
		t.Fatalf("query: %v", err)
	}
	if count != 0 {
		t.Error("원문이 그대로 저장되어 있다")
	}
	if err := st.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM api_tokens WHERE token_hash = ?`, HashToken(raw)).Scan(&count); err != nil {
		t.Fatalf("query: %v", err)
	}
	if count != 1 {
		t.Error("해시로 찾을 수 없다")
	}
}

func TestLookupAPIToken(t *testing.T) {
	ctx, st, userID := tokenFixture(t)
	const raw = "dbs_lookupme"

	saved, err := st.CreateAPIToken(ctx, CreateTokenParams{
		UserID: userID, Name: "t", Scope: TokenScopeWrite, Token: raw, Prefix: "dbs_look",
	})
	if err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}

	found, u, err := st.LookupAPIToken(ctx, raw)
	if err != nil {
		t.Fatalf("LookupAPIToken: %v", err)
	}
	if found.ID != saved.ID || u.ID != userID || found.Scope != TokenScopeWrite {
		t.Errorf("잘못된 결과: %+v", found)
	}

	if _, _, err := st.LookupAPIToken(ctx, "dbs_wrong"); !errors.Is(err, ErrNotFound) {
		t.Errorf("없는 토큰 = %v", err)
	}
}

// 폐기는 행을 지우지 않는다. "이 토큰이 언제까지 살아 있었는가"가 사고 조사에 필요하다.
func TestRevokeKeepsRecord(t *testing.T) {
	ctx, st, userID := tokenFixture(t)
	const raw = "dbs_revokeme"

	saved, err := st.CreateAPIToken(ctx, CreateTokenParams{
		UserID: userID, Name: "t", Scope: TokenScopeRead, Token: raw,
	})
	if err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}
	if !saved.Active() {
		t.Fatal("새 토큰이 비활성이다")
	}

	if err := st.RevokeAPIToken(ctx, saved.ID, userID); err != nil {
		t.Fatalf("RevokeAPIToken: %v", err)
	}
	after, _, err := st.LookupAPIToken(ctx, raw)
	if err != nil {
		t.Fatalf("폐기 후 조회 실패: %v", err)
	}
	if after.Active() {
		t.Error("폐기했는데 여전히 활성이다")
	}
	if after.RevokedAt == nil {
		t.Error("폐기 시각이 기록되지 않았다")
	}

	// 두 번 폐기하면 알려준다(이미 폐기된 것을 조용히 성공시키지 않는다).
	if err := st.RevokeAPIToken(ctx, saved.ID, userID); !errors.Is(err, ErrNotFound) {
		t.Errorf("두 번째 폐기 = %v", err)
	}
}

// 남의 토큰은 건드릴 수 없다. user_id 조건이 빠지면 ID만 알면 남의 토큰을 폐기할 수 있다.
func TestTokenOwnershipEnforced(t *testing.T) {
	ctx, st, userID := tokenFixture(t)
	other, err := st.CreateUser(ctx, CreateUserParams{
		Username: "other", Role: model.RoleMember, PasswordHash: "x",
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	saved, err := st.CreateAPIToken(ctx, CreateTokenParams{
		UserID: userID, Name: "t", Scope: TokenScopeRead, Token: "dbs_mine",
	})
	if err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}

	if err := st.RevokeAPIToken(ctx, saved.ID, other.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("남의 토큰 폐기 = %v", err)
	}
	if err := st.DeleteAPIToken(ctx, saved.ID, other.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("남의 토큰 삭제 = %v", err)
	}
}

// 만료된 토큰은 조회는 되지만 활성이 아니다.
// 조회와 판정을 나눈 이유: 호출부가 "없음"과 "만료"를 구분해 기록할 수 있어야 한다.
func TestExpiredTokenIsInactive(t *testing.T) {
	ctx, st, userID := tokenFixture(t)
	past := time.Now().Add(-time.Hour)

	saved, err := st.CreateAPIToken(ctx, CreateTokenParams{
		UserID: userID, Name: "t", Scope: TokenScopeRead, Token: "dbs_expired", ExpiresAt: &past,
	})
	if err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}
	if saved.Active() {
		t.Error("만료된 토큰이 활성이다")
	}
	if saved.ExpiresAt == nil {
		t.Error("만료 시각이 저장되지 않았다")
	}
}

// 사용자를 지우면 토큰도 함께 사라진다(FK CASCADE).
// 계정을 지웠는데 그 토큰이 살아 있으면 지운 것이 아니다.
func TestTokensDieWithUser(t *testing.T) {
	ctx, st, userID := tokenFixture(t)
	if _, err := st.CreateAPIToken(ctx, CreateTokenParams{
		UserID: userID, Name: "t", Scope: TokenScopeRead, Token: "dbs_cascade",
	}); err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}
	if err := st.DeleteUser(ctx, userID); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	if _, _, err := st.LookupAPIToken(ctx, "dbs_cascade"); !errors.Is(err, ErrNotFound) {
		t.Errorf("사용자 삭제 후 토큰이 남아 있다: %v", err)
	}
}

func TestValidTokenScope(t *testing.T) {
	if !ValidTokenScope(TokenScopeRead) || !ValidTokenScope(TokenScopeWrite) {
		t.Error("정의된 범위가 거부되었다")
	}
	if ValidTokenScope("admin") {
		t.Error("모르는 범위가 통과했다")
	}
	// 모르는 값이 들어오면 가장 좁은 범위로 떨어진다.
	ctx, st, userID := tokenFixture(t)
	saved, err := st.CreateAPIToken(ctx, CreateTokenParams{
		UserID: userID, Name: "t", Scope: "superpower", Token: "dbs_scope",
	})
	if err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}
	if saved.Scope != TokenScopeRead {
		t.Errorf("모르는 범위가 %q 로 저장되었다", saved.Scope)
	}
}
