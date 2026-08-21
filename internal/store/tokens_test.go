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
