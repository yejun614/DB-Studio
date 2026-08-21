package store

import (
	"errors"
	"testing"
	"time"
)

func TestTOTPLifecycle(t *testing.T) {
	ctx, st := userFixture(t)
	u := mkUser(t, ctx, st, "alice")

	if _, err := st.GetTOTP(ctx, u.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("등록 전 조회 = %v, want ErrNotFound", err)
	}
	// 새 사용자는 2FA가 꺼진 상태로 보여야 한다.
	if fresh, _ := st.GetUser(ctx, u.ID); fresh.TOTPEnabled {
		t.Error("등록하지 않은 사용자가 켜짐으로 보입니다")
	}

	err := st.StartTOTP(ctx, StartTOTPParams{
		UserID: u.ID, Secret: "JBSWY3DPEHPK3PXP", Digits: 6,
		Period: 30 * time.Second, Skew: 90 * time.Second,
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	e, err := st.GetTOTP(ctx, u.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if e.Secret != "JBSWY3DPEHPK3PXP" {
		t.Errorf("공유 비밀이 복원되지 않았습니다: %q", e.Secret)
	}
	if e.Skew != 90*time.Second || e.Period != 30*time.Second || e.Digits != 6 {
		t.Errorf("파라미터가 어긋납니다: %+v", e)
	}
	if e.Confirmed() {
		t.Error("시작만 했는데 확인된 상태입니다")
	}
	// 확인 전에는 사용자 목록에서도 꺼진 것으로 보여야 한다.
	if fresh, _ := st.GetUser(ctx, u.ID); fresh.TOTPEnabled {
		t.Error("확인 전 등록이 켜짐으로 보입니다")
	}

	if err := st.ConfirmTOTP(ctx, u.ID, 60*time.Second, 12345); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	e, _ = st.GetTOTP(ctx, u.ID)
	if !e.Confirmed() || e.Skew != 60*time.Second || e.LastStep != 12345 {
		t.Errorf("확인 결과가 반영되지 않았습니다: %+v", e)
	}
	if fresh, _ := st.GetUser(ctx, u.ID); !fresh.TOTPEnabled {
		t.Error("확인된 등록이 꺼짐으로 보입니다")
	}

	// 확인된 등록은 새 QR로 덮이면 안 된다. 덮이면 기존 인증 앱이 조용히 죽는다.
	err = st.StartTOTP(ctx, StartTOTPParams{UserID: u.ID, Secret: "AAAAAAAAAAAAAAAA"})
	if !errors.Is(err, ErrTOTPConfirmed) {
		t.Fatalf("확인된 등록 위에 재시작 = %v, want ErrTOTPConfirmed", err)
	}
	if e, _ := st.GetTOTP(ctx, u.ID); e.Secret != "JBSWY3DPEHPK3PXP" {
		t.Error("확인된 등록의 공유 비밀이 바뀌었습니다")
	}

	if err := st.DeleteTOTP(ctx, u.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := st.GetTOTP(ctx, u.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("삭제 후 조회 = %v, want ErrNotFound", err)
	}
}

func TestTOTPFailureLock(t *testing.T) {
	ctx, st := userFixture(t)
	u := mkUser(t, ctx, st, "bob")
	if err := st.StartTOTP(ctx, StartTOTPParams{UserID: u.ID, Secret: "JBSWY3DPEHPK3PXP"}); err != nil {
		t.Fatal(err)
	}

	for i := 1; i < 5; i++ {
		locked, err := st.RecordTOTPFailure(ctx, u.ID, 5, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if locked != nil {
			t.Fatalf("%d회 실패에 잠겼습니다 (한도 5)", i)
		}
	}
	locked, err := st.RecordTOTPFailure(ctx, u.ID, 5, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if locked == nil {
		t.Fatal("한도를 넘었는데 잠기지 않았습니다")
	}
	e, _ := st.GetTOTP(ctx, u.ID)
	if !e.Locked(time.Now().UTC()) {
		t.Error("잠금 상태가 반영되지 않았습니다")
	}
	// 성공하면 잠금과 카운터가 함께 풀려야 한다.
	if err := st.RecordTOTPSuccess(ctx, u.ID, 0, 999); err != nil {
		t.Fatal(err)
	}
	e, _ = st.GetTOTP(ctx, u.ID)
	if e.Locked(time.Now().UTC()) || e.Failures != 0 {
		t.Errorf("성공 후에도 잠겨 있습니다: %+v", e)
	}
}

func TestRecoveryCodesAreSingleUse(t *testing.T) {
	ctx, st := userFixture(t)
	u := mkUser(t, ctx, st, "carol")

	codes := []string{"aaaa-bbbb", "cccc-dddd", "eeee-ffff"}
	if err := st.ReplaceRecoveryCodes(ctx, u.ID, codes); err != nil {
		t.Fatal(err)
	}
	remaining, total, err := st.CountRecoveryCodes(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if remaining != 3 || total != 3 {
		t.Fatalf("남은/전체 = %d/%d, want 3/3", remaining, total)
	}

	ok, err := st.UseRecoveryCode(ctx, u.ID, "cccc-dddd", "10.0.0.1")
	if err != nil || !ok {
		t.Fatalf("복구 코드를 쓰지 못했습니다: ok=%v err=%v", ok, err)
	}
	// 같은 코드를 두 번 쓸 수 없어야 한다.
	ok, _ = st.UseRecoveryCode(ctx, u.ID, "cccc-dddd", "10.0.0.1")
	if ok {
		t.Error("이미 쓴 복구 코드가 다시 통과했습니다")
	}
	// 남의 코드로도 안 된다.
	other := mkUser(t, ctx, st, "dave")
	ok, _ = st.UseRecoveryCode(ctx, other.ID, "aaaa-bbbb", "10.0.0.1")
	if ok {
		t.Error("다른 사용자의 복구 코드가 통과했습니다")
	}

	remaining, total, _ = st.CountRecoveryCodes(ctx, u.ID)
	if remaining != 2 || total != 3 {
		t.Errorf("남은/전체 = %d/%d, want 2/3", remaining, total)
	}

	// 재발급하면 옛 코드는 전부 죽는다.
	if err := st.ReplaceRecoveryCodes(ctx, u.ID, []string{"1111-2222"}); err != nil {
		t.Fatal(err)
	}
	if ok, _ := st.UseRecoveryCode(ctx, u.ID, "aaaa-bbbb", ""); ok {
		t.Error("재발급 후에도 옛 코드가 통과했습니다")
	}
}

func TestTOTPChallenge(t *testing.T) {
	ctx, st := userFixture(t)
	u := mkUser(t, ctx, st, "erin")

	ch, err := st.CreateTOTPChallenge(ctx, "token-abc", u.ID, "10.0.0.9", "agent", 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if ch.UserID != u.ID {
		t.Fatalf("userID = %s", ch.UserID)
	}
	// 원문이 아니라 해시로 저장되어야 한다.
	if ch.ID == "token-abc" {
		t.Error("챌린지 토큰 원문이 저장되었습니다")
	}

	got, err := st.LookupTOTPChallenge(ctx, "token-abc")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if got.UserID != u.ID {
		t.Errorf("조회된 사용자 = %s", got.UserID)
	}
	if _, err := st.LookupTOTPChallenge(ctx, "wrong"); !errors.Is(err, ErrNotFound) {
		t.Errorf("틀린 토큰 조회 = %v, want ErrNotFound", err)
	}

	n, err := st.BumpTOTPChallengeAttempt(ctx, "token-abc")
	if err != nil || n != 1 {
		t.Fatalf("시도 횟수 = %d, err = %v", n, err)
	}
	if n, _ := st.BumpTOTPChallengeAttempt(ctx, "token-abc"); n != 2 {
		t.Errorf("두 번째 시도 = %d, want 2", n)
	}

	if err := st.DeleteTOTPChallenge(ctx, "token-abc"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.LookupTOTPChallenge(ctx, "token-abc"); !errors.Is(err, ErrNotFound) {
		t.Errorf("삭제 후 조회 = %v, want ErrNotFound", err)
	}
}

func TestTOTPChallengeExpires(t *testing.T) {
	ctx, st := userFixture(t)
	u := mkUser(t, ctx, st, "frank")

	if _, err := st.CreateTOTPChallenge(ctx, "old", u.ID, "", "", -time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := st.LookupTOTPChallenge(ctx, "old"); !errors.Is(err, ErrNotFound) {
		t.Errorf("만료된 챌린지가 살아 있습니다: %v", err)
	}

	if _, err := st.CreateTOTPChallenge(ctx, "old2", u.ID, "", "", -time.Minute); err != nil {
		t.Fatal(err)
	}
	n, err := st.PurgeExpiredTOTPChallenges(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("정리된 챌린지 = %d, want 1", n)
	}
}

// 사용자를 지우면 2FA 흔적도 함께 사라져야 한다(FK CASCADE).
func TestTOTPCascadesOnUserDelete(t *testing.T) {
	ctx, st := userFixture(t)
	u := mkUser(t, ctx, st, "grace")
	if err := st.StartTOTP(ctx, StartTOTPParams{UserID: u.ID, Secret: "JBSWY3DPEHPK3PXP"}); err != nil {
		t.Fatal(err)
	}
	if err := st.ReplaceRecoveryCodes(ctx, u.ID, []string{"x"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateTOTPChallenge(ctx, "tok", u.ID, "", "", time.Minute); err != nil {
		t.Fatal(err)
	}

	if err := st.DeleteUser(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetTOTP(ctx, u.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("등록이 남았습니다: %v", err)
	}
	if _, total, _ := st.CountRecoveryCodes(ctx, u.ID); total != 0 {
		t.Errorf("복구 코드가 %d개 남았습니다", total)
	}
	if _, err := st.LookupTOTPChallenge(ctx, "tok"); !errors.Is(err, ErrNotFound) {
		t.Errorf("챌린지가 남았습니다: %v", err)
	}
}

func TestSecurityPolicyDefaultsAndCache(t *testing.T) {
	ctx, st := userFixture(t)

	p, err := st.SecurityPolicy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// 기본값은 "각자 자율"이다. 설치하자마자 아무도 로그인 못 하는 상태가 되면 안 된다.
	if p.TOTPRequired {
		t.Error("기본값이 의무화로 되어 있습니다")
	}

	if err := st.SetTOTPRequired(ctx, true, "admin-id"); err != nil {
		t.Fatal(err)
	}
	p, _ = st.SecurityPolicy(ctx)
	if !p.TOTPRequired {
		t.Fatal("설정이 반영되지 않았습니다 (캐시가 낡았을 수 있습니다)")
	}
	if p.UpdatedBy != "admin-id" || p.UpdatedAt == nil {
		t.Errorf("변경 기록이 없습니다: %+v", p)
	}

	if err := st.SetTOTPRequired(ctx, false, "admin-id"); err != nil {
		t.Fatal(err)
	}
	if p, _ := st.SecurityPolicy(ctx); p.TOTPRequired {
		t.Error("끈 설정이 캐시에 남아 있습니다")
	}
}

func TestClockOffsetRoundTrip(t *testing.T) {
	ctx, st := userFixture(t)

	d, err := st.ClockOffset(ctx)
	if err != nil || d != 0 {
		t.Fatalf("기본 보정값 = %v, err = %v", d, err)
	}
	if err := st.SaveClockOffset(ctx, -125*time.Second); err != nil {
		t.Fatal(err)
	}
	if d, _ := st.ClockOffset(ctx); d != -125*time.Second {
		t.Errorf("보정값 = %v, want -125s", d)
	}
	// 값이 깨져 있으면 0으로 되돌린다 — 이상한 값으로 시계를 끌고 가는 것보다 낫다.
	if err := st.SetSetting(ctx, SettingClockOffset, "not-a-number", ""); err != nil {
		t.Fatal(err)
	}
	if d, _ := st.ClockOffset(ctx); d != 0 {
		t.Errorf("깨진 값에서 %v를 읽었습니다, want 0", d)
	}
}
