package auth

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dbstudio/internal/clock"
	"dbstudio/internal/crypto"
	"dbstudio/internal/model"
	"dbstudio/internal/store"
	"dbstudio/internal/totp"
)

func totpFixture(t *testing.T) (context.Context, *store.Store, *Service, *model.User) {
	t.Helper()
	ctx := context.Background()
	box, err := crypto.NewSecretBox(make([]byte, 32))
	if err != nil {
		t.Fatalf("secret box: %v", err)
	}
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "auth.db"), box)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	hash, err := crypto.HashPassword(testPassword)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	u, err := st.CreateUser(ctx, store.CreateUserParams{
		Username: "alice", DisplayName: "Alice", Role: model.RoleMember, PasswordHash: hash,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	return ctx, st, NewService(st, time.Hour, clock.New(0)), u
}

const testPassword = "correct-horse-9"

// codeFor는 서비스가 그 사용자에게 쓸 유효 시각으로 코드를 만든다.
// 인증 앱이 하는 일을 시험 안에서 대신한다.
func codeFor(t *testing.T, s *Service, e *store.TOTPEnrollment, shift time.Duration) string {
	t.Helper()
	at := s.clock.BaseNow().Add(e.Skew + shift)
	code, err := totp.CodeAt(e.Secret, at, totp.Params{Digits: e.Digits, Period: e.Period})
	if err != nil {
		t.Fatalf("code: %v", err)
	}
	return code
}

func enroll(t *testing.T, ctx context.Context, st *store.Store, s *Service, u *model.User) *store.TOTPEnrollment {
	t.Helper()
	if _, err := s.BeginTOTPEnrollment(ctx, u); err != nil {
		t.Fatalf("begin: %v", err)
	}
	e, err := st.GetTOTP(ctx, u.ID)
	if err != nil {
		t.Fatalf("get enrollment: %v", err)
	}
	if _, err := s.ConfirmTOTPEnrollment(ctx, u.ID, codeFor(t, s, e, 0)); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	e, _ = st.GetTOTP(ctx, u.ID)
	return e
}

// 2단계 인증을 켜지 않은 계정은 지금까지처럼 한 번에 로그인된다.
// 기본값은 자율이므로, 기능을 추가했다는 이유로 기존 사용자가 막히면 안 된다.
func TestLoginWithoutTOTPIsUnchanged(t *testing.T) {
	ctx, _, s, _ := totpFixture(t)
	res, err := s.Login(ctx, "alice", testPassword, "10.0.0.1", "agent")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if res.NeedsTOTP() {
		t.Fatal("2FA를 켜지 않았는데 코드를 요구했습니다")
	}
	if res.Token == "" {
		t.Fatal("세션이 발급되지 않았습니다")
	}
}

// 등록의 계약: 확인을 마치기 전에는 2FA가 켜지지 않는다.
// QR만 띄우고 앱에 등록하지 않은 사람이 자기 계정에서 잠기면 안 된다.
func TestEnrollmentIsNotActiveUntilConfirmed(t *testing.T) {
	ctx, st, s, u := totpFixture(t)

	enr, err := s.BeginTOTPEnrollment(ctx, u)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if enr.Secret == "" || enr.URI == "" {
		t.Fatal("등록 정보가 비어 있습니다")
	}
	if enr.QR == "" {
		t.Error("QR을 만들지 못했습니다")
	}

	res, err := s.Login(ctx, "alice", testPassword, "", "")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if res.NeedsTOTP() {
		t.Fatal("확인 전 등록인데 코드를 요구했습니다")
	}

	e, _ := st.GetTOTP(ctx, u.ID)
	codes, err := s.ConfirmTOTPEnrollment(ctx, u.ID, codeFor(t, s, e, 0))
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if len(codes) != recoveryCodeCount {
		t.Errorf("복구 코드 %d개, want %d개", len(codes), recoveryCodeCount)
	}

	res, err = s.Login(ctx, "alice", testPassword, "", "")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if !res.NeedsTOTP() {
		t.Fatal("확인을 마쳤는데 코드를 묻지 않았습니다")
	}
	// 1단계만 통과한 상태에서는 세션이 나오면 안 된다.
	if res.Token != "" {
		t.Fatal("코드를 내기 전에 세션이 발급되었습니다")
	}
}

func TestTwoStepLogin(t *testing.T) {
	ctx, st, s, u := totpFixture(t)
	e := enroll(t, ctx, st, s, u)

	res, err := s.Login(ctx, "alice", testPassword, "10.0.0.1", "agent")
	if err != nil {
		t.Fatalf("login: %v", err)
	}

	// 등록 직후에는 last_step이 방금 코드로 차 있으므로 다음 스텝의 코드를 쓴다.
	e, _ = st.GetTOTP(ctx, u.ID)
	code := codeFor(t, s, e, e.Period)
	done, err := s.CompleteTOTPLogin(ctx, res.Challenge, code, "10.0.0.1", "agent")
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if done.Token == "" {
		t.Fatal("세션이 발급되지 않았습니다")
	}
	// 챌린지는 한 번 쓰면 사라져야 한다.
	if _, err := s.CompleteTOTPLogin(ctx, res.Challenge, code, "", ""); !errors.Is(err, ErrChallengeInvalid) {
		t.Errorf("소비된 챌린지 재사용 = %v, want ErrChallengeInvalid", err)
	}
}

// 같은 코드를 두 번 쓸 수 없어야 한다(RFC 6238이 요구하는 성질).
func TestCodeCannotBeReused(t *testing.T) {
	ctx, st, s, u := totpFixture(t)
	e := enroll(t, ctx, st, s, u)

	code := codeFor(t, s, e, e.Period)
	res, _ := s.Login(ctx, "alice", testPassword, "", "")
	if _, err := s.CompleteTOTPLogin(ctx, res.Challenge, code, "", ""); err != nil {
		t.Fatalf("first use: %v", err)
	}

	res2, _ := s.Login(ctx, "alice", testPassword, "", "")
	_, err := s.CompleteTOTPLogin(ctx, res2.Challenge, code, "", "")
	if !errors.Is(err, ErrTOTPReused) {
		t.Errorf("같은 코드 재사용 = %v, want ErrTOTPReused", err)
	}
}

func TestWrongCodeLocksAfterRepeatedFailures(t *testing.T) {
	ctx, st, s, u := totpFixture(t)
	enroll(t, ctx, st, s, u)

	var lastErr error
	for i := 0; i < maxCodeFailures; i++ {
		res, _ := s.Login(ctx, "alice", testPassword, "", "")
		_, lastErr = s.CompleteTOTPLogin(ctx, res.Challenge, "000000", "", "")
	}
	if !errors.Is(lastErr, ErrTOTPLocked) {
		t.Fatalf("%d회 실패 후 = %v, want ErrTOTPLocked", maxCodeFailures, lastErr)
	}
	// 잠긴 동안에는 올바른 코드도 통하지 않아야 한다.
	e, _ := st.GetTOTP(ctx, u.ID)
	res, _ := s.Login(ctx, "alice", testPassword, "", "")
	_, err := s.CompleteTOTPLogin(ctx, res.Challenge, codeFor(t, s, e, e.Period), "", "")
	if !errors.Is(err, ErrTOTPLocked) {
		t.Errorf("잠금 중 올바른 코드 = %v, want ErrTOTPLocked", err)
	}
}

// 한 챌린지에서 무한히 시도할 수 없어야 한다.
func TestChallengeAttemptLimit(t *testing.T) {
	ctx, st, s, u := totpFixture(t)
	enroll(t, ctx, st, s, u)

	res, _ := s.Login(ctx, "alice", testPassword, "", "")
	var err error
	for i := 0; i <= maxChallengeAttempts; i++ {
		_, err = s.CompleteTOTPLogin(ctx, res.Challenge, "000000", "", "")
	}
	if !errors.Is(err, ErrChallengeInvalid) && !errors.Is(err, ErrTOTPLocked) {
		t.Errorf("시도 한도 초과 = %v", err)
	}
	// 챌린지가 폐기되었는지 확인한다.
	if _, err := st.LookupTOTPChallenge(ctx, res.Challenge); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("한도를 넘긴 챌린지가 살아 있습니다: %v", err)
	}
	_ = u
}

// 시계가 크게 어긋난 경우: 그 코드로 로그인시켜 주지는 않지만,
// 보정을 끝내 두어 **다음 코드**는 통과해야 한다.
func TestClockResyncRecoversWithoutWideningLoginWindow(t *testing.T) {
	ctx, st, s, u := totpFixture(t)
	e := enroll(t, ctx, st, s, u)

	// 서버 시계가 갑자기 7분 뒤로 밀린 상황을 흉내 낸다.
	// (인증 앱은 정상이고, 우리 쪽 유효 시각만 7분 어긋난다.)
	const jump = 7 * time.Minute
	if err := st.RecordTOTPResync(ctx, u.ID, e.Skew-jump, e.LastStep); err != nil {
		t.Fatal(err)
	}
	e, _ = st.GetTOTP(ctx, u.ID)

	// 인증 앱의 코드는 원래 보정값 기준이다.
	appCode := codeFor(t, s, e, jump+e.Period)
	res, _ := s.Login(ctx, "alice", testPassword, "", "")
	_, err := s.CompleteTOTPLogin(ctx, res.Challenge, appCode, "", "")
	if !errors.Is(err, ErrTOTPResynced) {
		t.Fatalf("첫 시도 = %v, want ErrTOTPResynced (로그인은 거절하되 보정은 해야 한다)", err)
	}

	// 보정이 반영되었는지 확인하고, 다음 코드로 로그인되는지 본다.
	e2, _ := st.GetTOTP(ctx, u.ID)
	if diff := e2.Skew - e.Skew; diff < jump-time.Minute || diff > jump+time.Minute {
		t.Fatalf("보정값이 %v 움직였습니다, want ≈%v", diff, jump)
	}
	// 보정 뒤 인증 앱이 다음에 보여줄 코드. 재동기화가 방금 코드의 스텝을 소비했으므로
	// 한 칸 뒤의 코드여야 하고, 그것은 좁은 창(±1스텝) 안에 들어온다.
	nextCode := codeFor(t, s, e2, e2.Period)
	res2, _ := s.Login(ctx, "alice", testPassword, "", "")
	done, err := s.CompleteTOTPLogin(ctx, res2.Challenge, nextCode, "", "")
	if err != nil {
		t.Fatalf("보정 후 다음 코드로 로그인 실패: %v", err)
	}
	if done.Token == "" {
		t.Fatal("세션이 발급되지 않았습니다")
	}
}

// 등록 시점에는 서버 시계가 크게 틀려 있어도 그 자리에서 맞춰야 한다.
// 여기서 실패하면 시계가 틀린 서버에서는 아무도 2FA를 켤 수 없다.
func TestEnrollmentCalibratesBadServerClock(t *testing.T) {
	ctx, st, s, u := totpFixture(t)

	if _, err := s.BeginTOTPEnrollment(ctx, u); err != nil {
		t.Fatal(err)
	}
	e, _ := st.GetTOTP(ctx, u.ID)

	// 서버 시계가 6분 느린 상황: 인증 앱의 코드는 우리 시각보다 6분 앞서 있다.
	const off = 6 * time.Minute
	if _, err := s.ConfirmTOTPEnrollment(ctx, u.ID, codeFor(t, s, e, off)); err != nil {
		t.Fatalf("확인 실패: %v", err)
	}

	e, _ = st.GetTOTP(ctx, u.ID)
	if diff := e.Skew - off; diff > 30*time.Second || diff < -30*time.Second {
		t.Errorf("학습된 보정값 = %v, want ≈%v", e.Skew, off)
	}
	// 전역 시계도 그 사실을 배워야 새 사용자가 처음부터 맞은 자리에서 시작한다.
	if s.clock.Offset() <= 0 {
		t.Error("전역 시계가 아무것도 배우지 못했습니다")
	}

	// 그리고 이 보정값으로 실제 로그인이 되어야 한다.
	res, _ := s.Login(ctx, "alice", testPassword, "", "")
	if _, err := s.CompleteTOTPLogin(ctx, res.Challenge, codeFor(t, s, e, e.Period), "", ""); err != nil {
		t.Fatalf("보정 후 로그인 실패: %v", err)
	}
}

func TestRecoveryCodeLogin(t *testing.T) {
	ctx, st, s, u := totpFixture(t)
	if _, err := s.BeginTOTPEnrollment(ctx, u); err != nil {
		t.Fatal(err)
	}
	e, _ := st.GetTOTP(ctx, u.ID)
	codes, err := s.ConfirmTOTPEnrollment(ctx, u.ID, codeFor(t, s, e, 0))
	if err != nil {
		t.Fatal(err)
	}

	res, _ := s.Login(ctx, "alice", testPassword, "10.0.0.5", "")
	done, err := s.CompleteTOTPLogin(ctx, res.Challenge, codes[3], "10.0.0.5", "")
	if err != nil {
		t.Fatalf("복구 코드 로그인 실패: %v", err)
	}
	if done.Token == "" {
		t.Fatal("세션이 발급되지 않았습니다")
	}

	// 한 번 쓴 복구 코드는 죽는다.
	res2, _ := s.Login(ctx, "alice", testPassword, "", "")
	if _, err := s.CompleteTOTPLogin(ctx, res2.Challenge, codes[3], "", ""); !errors.Is(err, ErrTOTPInvalid) {
		t.Errorf("쓴 복구 코드 재사용 = %v, want ErrTOTPInvalid", err)
	}

	// 대문자·하이픈 없이 적어도 통과해야 한다(종이에서 옮겨 적는 상황).
	res3, _ := s.Login(ctx, "alice", testPassword, "", "")
	messy := strings.ToUpper(strings.ReplaceAll(codes[4], "-", " "))
	if _, err := s.CompleteTOTPLogin(ctx, res3.Challenge, messy, "", ""); err != nil {
		t.Errorf("형식이 흐트러진 복구 코드를 거부했습니다: %v", err)
	}
}

func TestDisableRequiresPasswordAndRespectsPolicy(t *testing.T) {
	ctx, st, s, u := totpFixture(t)
	enroll(t, ctx, st, s, u)

	if err := s.DisableTOTP(ctx, u.ID, "wrong-password"); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("틀린 비밀번호로 해제 = %v, want ErrInvalidCredentials", err)
	}
	if fresh, _ := st.GetUser(ctx, u.ID); !fresh.TOTPEnabled {
		t.Fatal("틀린 비밀번호인데 해제되었습니다")
	}

	// 의무화되어 있으면 본인도 끌 수 없다.
	if err := st.SetTOTPRequired(ctx, true, "admin"); err != nil {
		t.Fatal(err)
	}
	if err := s.DisableTOTP(ctx, u.ID, testPassword); !errors.Is(err, ErrTOTPEnforced) {
		t.Errorf("의무화 상태에서 해제 = %v, want ErrTOTPEnforced", err)
	}

	if err := st.SetTOTPRequired(ctx, false, "admin"); err != nil {
		t.Fatal(err)
	}
	if err := s.DisableTOTP(ctx, u.ID, testPassword); err != nil {
		t.Fatalf("해제 실패: %v", err)
	}
	if fresh, _ := st.GetUser(ctx, u.ID); fresh.TOTPEnabled {
		t.Error("해제 후에도 켜짐으로 보입니다")
	}
	// 해제하면 복구 코드도 함께 사라져야 한다.
	if _, total, _ := st.CountRecoveryCodes(ctx, u.ID); total != 0 {
		t.Errorf("복구 코드가 %d개 남았습니다", total)
	}
}

// 관리자 초기화 뒤에는 그 사이 발급된 챌린지도 무효여야 한다.
func TestResetInvalidatesPendingChallenge(t *testing.T) {
	ctx, st, s, u := totpFixture(t)
	enroll(t, ctx, st, s, u)

	res, _ := s.Login(ctx, "alice", testPassword, "", "")
	if err := s.ResetTOTP(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CompleteTOTPLogin(ctx, res.Challenge, "000000", "", ""); !errors.Is(err, ErrChallengeInvalid) {
		t.Errorf("초기화 후 챌린지 = %v, want ErrChallengeInvalid", err)
	}
}

// 비활성화된 계정은 1단계와 2단계 사이에서도 막혀야 한다.
func TestDisabledAccountCannotCompleteLogin(t *testing.T) {
	ctx, st, s, u := totpFixture(t)
	e := enroll(t, ctx, st, s, u)

	res, _ := s.Login(ctx, "alice", testPassword, "", "")
	disabled := model.UserDisabled
	if _, err := st.UpdateUser(ctx, u.ID, store.UpdateUserParams{Status: &disabled}); err != nil {
		t.Fatal(err)
	}
	_, err := s.CompleteTOTPLogin(ctx, res.Challenge, codeFor(t, s, e, e.Period), "", "")
	if !errors.Is(err, ErrAccountDisabled) {
		t.Errorf("비활성 계정의 2단계 = %v, want ErrAccountDisabled", err)
	}
}

// 의무화 정책이 켜지면 API 토큰도 막혀야 한다.
// 화면에서 막아 둔 사람이 토큰으로 통과하면 의무화한 것이 아니다.
func TestTokenBlockedWhenTOTPRequired(t *testing.T) {
	ctx, st, s, u := totpFixture(t)

	_, raw, err := s.IssueAPIToken(ctx, store.CreateTokenParams{
		UserID: u.ID, Name: "test", Scope: store.TokenScopeRead,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AuthenticateToken(ctx, raw, ""); err != nil {
		t.Fatalf("정책 없이 토큰 인증 실패: %v", err)
	}

	if err := st.SetTOTPRequired(ctx, true, "admin"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AuthenticateToken(ctx, raw, ""); !errors.Is(err, ErrTOTPRequired) {
		t.Errorf("의무화 상태 토큰 인증 = %v, want ErrTOTPRequired", err)
	}

	// 2FA를 등록하면 다시 통해야 한다.
	enroll(t, ctx, st, s, u)
	if _, _, err := s.AuthenticateToken(ctx, raw, ""); err != nil {
		t.Errorf("등록 후 토큰 인증 실패: %v", err)
	}
}

func TestStatusReportsRecoveryCodes(t *testing.T) {
	ctx, st, s, u := totpFixture(t)

	st0, err := s.TOTPStatus(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if st0.Enabled || st0.Pending || st0.Required {
		t.Errorf("초기 상태가 이상합니다: %+v", st0)
	}

	enroll(t, ctx, st, s, u)
	st1, _ := s.TOTPStatus(ctx, u.ID)
	if !st1.Enabled || st1.RecoveryRemaining != recoveryCodeCount {
		t.Errorf("등록 후 상태: %+v", st1)
	}
}
