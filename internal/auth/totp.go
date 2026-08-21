package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"dbstudio/internal/clock"
	"dbstudio/internal/crypto"
	"dbstudio/internal/model"
	"dbstudio/internal/store"
	"dbstudio/internal/totp"
)

// 2단계 인증(TOTP).
//
// 로그인은 두 단계로 나뉜다.
//
//	1단계: 아이디·비밀번호 → 서버가 챌린지를 만들고 짧은 쿠키로 내려보낸다.
//	2단계: 인증 코드 → 챌린지를 소비하고 진짜 세션을 발급한다.
//
// 1단계에서 세션을 만들지 않는 것이 핵심이다. "비밀번호는 맞았다"는 상태를 세션으로
// 표현하면, 세션을 읽는 모든 코드가 그 예외를 알아야 하고 하나라도 모르면 그것이
// 우회로가 된다. 챌린지는 세션이 아닌 별도 테이블에 있고 할 수 있는 일이 하나뿐이다.

const (
	// ChallengeCookieName은 2단계를 기다리는 상태를 담는 쿠키다.
	// 응답 본문이 아니라 쿠키로 주는 이유: HttpOnly로 두면 스크립트가 읽지 못한다.
	ChallengeCookieName = "dbstudio_2fa"
	// ChallengeTTL은 코드를 넣기까지 허용하는 시간이다.
	// 인증 앱을 열어 코드를 읽는 데 필요한 시간보다 넉넉하되, 자리를 비운 사이
	// 남이 이어서 진행할 만큼 길면 안 된다.
	ChallengeTTL = 5 * time.Minute

	// maxChallengeAttempts는 한 챌린지에서 허용하는 코드 입력 횟수다.
	maxChallengeAttempts = 5
	// maxCodeFailures/codeLockFor는 계정 단위 잠금이다. 챌린지를 새로 만들어
	// 시도 횟수를 초기화하는 우회를 막는다.
	maxCodeFailures = 5
	codeLockFor     = 5 * time.Minute

	// verifyWindow는 로그인에서 받아들이는 시각 오차다(±1스텝 = ±30초).
	// 이 값을 넓히면 훔쳐본 코드의 수명이 그만큼 길어진다.
	verifyWindow = 1
	// resyncWindow는 시계 차이를 **찾아내기 위해서만** 훑는 폭이다(±30스텝 = ±15분).
	// 여기서 맞았다고 로그인시키지는 않는다(store.RecordTOTPResync 주석 참고).
	resyncWindow = 30

	// recoveryCodeCount는 발급하는 복구 코드 개수다.
	recoveryCodeCount = 10

	// totpIssuer는 인증 앱 목록에 표시될 이름이다.
	totpIssuer = "DB Studio"
)

var (
	// ErrTOTPRequired는 이 계정이 2단계 인증을 마쳐야 한다는 뜻이다.
	ErrTOTPRequired = errors.New("2단계 인증이 필요합니다")
	// ErrTOTPInvalid는 코드가 틀렸다는 뜻이다.
	ErrTOTPInvalid = errors.New("인증 코드가 올바르지 않습니다")
	// ErrTOTPReused는 이미 쓴 코드를 다시 낸 경우다.
	ErrTOTPReused = errors.New("이미 사용한 인증 코드입니다. 다음 코드가 나올 때까지 기다리세요")
	// ErrTOTPResynced는 코드 자체는 맞지만 시각이 크게 어긋나 있었다는 뜻이다.
	// 보정은 끝났으므로 다음 코드로 다시 시도하면 된다.
	ErrTOTPResynced = errors.New("시각 차이를 보정했습니다. 인증 앱의 다음 코드를 입력하세요")
	// ErrTOTPLocked는 실패가 누적되어 잠긴 상태다.
	ErrTOTPLocked = errors.New("인증 코드를 여러 번 틀려 잠시 잠겼습니다")
	// ErrChallengeInvalid는 챌린지가 없거나 만료된 경우다.
	ErrChallengeInvalid = errors.New("인증 시간이 지났습니다. 처음부터 다시 로그인하세요")
	// ErrTOTPNotEnrolled는 2단계 인증을 켜지 않은 계정에 대한 요청이다.
	ErrTOTPNotEnrolled = errors.New("2단계 인증이 설정되어 있지 않습니다")
	// ErrTOTPEnforced는 정책상 끌 수 없는 2단계 인증을 끄려 한 경우다.
	ErrTOTPEnforced = errors.New("관리자가 2단계 인증을 의무화했으므로 해제할 수 없습니다")
)

// Clock은 이 서비스가 쓰는 내부 시계를 반환한다. 상태 화면이 쓴다.
func (s *Service) Clock() *clock.Clock { return s.clock }

// Enrollment는 등록 화면에 필요한 값들이다.
type Enrollment struct {
	// Secret은 base32 공유 비밀이다. 등록 화면에서 한 번 보여주고 저장하지 않는다.
	Secret string `json:"secret"`
	// FormattedSecret은 손으로 옮겨 적기 좋게 끊어 놓은 것이다.
	FormattedSecret string `json:"formattedSecret"`
	URI             string `json:"uri"`
	// QR은 SVG data URI다. 만들지 못했으면 빈 문자열이며, 화면은 수동 입력으로 내려간다.
	QR     string `json:"qr,omitempty"`
	Digits int    `json:"digits"`
	Period int    `json:"period"`
}

// Status는 한 사용자의 2단계 인증 상태다.
type Status struct {
	Enabled bool `json:"enabled"`
	// Pending은 비밀은 만들었지만 확인을 마치지 않은 상태다.
	Pending     bool       `json:"pending"`
	Required    bool       `json:"required"`
	ConfirmedAt *time.Time `json:"confirmedAt,omitempty"`
	LastUsedAt  *time.Time `json:"lastUsedAt,omitempty"`
	// RecoveryRemaining/RecoveryTotal은 남은 복구 코드 수다.
	RecoveryRemaining int `json:"recoveryRemaining"`
	RecoveryTotal     int `json:"recoveryTotal"`
	// SkewSeconds는 이 계정에 적용 중인 시각 보정값이다.
	// 화면에 보여 주는 이유: "왜 내 코드만 안 맞는가"에 답할 수 있는 유일한 숫자다.
	SkewSeconds int `json:"skewSeconds"`
}

// TOTPStatus는 화면에 보여줄 상태를 모은다.
func (s *Service) TOTPStatus(ctx context.Context, userID string) (*Status, error) {
	policy, err := s.st.SecurityPolicy(ctx)
	if err != nil {
		return nil, err
	}
	out := &Status{Required: policy.TOTPRequired}

	e, err := s.st.GetTOTP(ctx, userID)
	if errors.Is(err, store.ErrNotFound) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	out.Enabled = e.Confirmed()
	out.Pending = !e.Confirmed()
	out.ConfirmedAt = e.ConfirmedAt
	out.LastUsedAt = e.LastUsedAt
	out.SkewSeconds = int(e.Skew.Round(time.Second) / time.Second)
	if out.Enabled {
		remaining, total, err := s.st.CountRecoveryCodes(ctx, userID)
		if err != nil {
			return nil, err
		}
		out.RecoveryRemaining, out.RecoveryTotal = remaining, total
	}
	return out, nil
}

// BeginTOTPEnrollment는 새 공유 비밀을 만들고 등록 화면에 필요한 값을 돌려준다.
//
// 이 시점에는 아직 2단계 인증이 켜지지 않는다. 확인(ConfirmTOTPEnrollment)까지
// 마쳐야 로그인에서 코드를 묻는다 — QR만 띄워 놓고 앱에 등록하지 않은 사람이
// 자기 계정에서 잠기는 일을 막기 위해서다.
func (s *Service) BeginTOTPEnrollment(ctx context.Context, u *model.User) (*Enrollment, error) {
	secret, err := totp.GenerateSecret()
	if err != nil {
		return nil, err
	}
	p := totp.DefaultParams()

	// 출발점은 지금까지 학습한 전역 보정값이다. 서버 시계가 5분 틀린 곳이라면
	// 첫 사용자가 그 사실을 알려 주었고, 다음 사용자부터는 처음부터 맞은 자리에서 시작한다.
	err = s.st.StartTOTP(ctx, store.StartTOTPParams{
		UserID: u.ID, Secret: secret, Digits: p.Digits, Period: p.Period,
		Skew: s.clock.Offset(),
	})
	if err != nil {
		return nil, err
	}

	account := u.Username
	uri := totp.URI(totpIssuer, account, secret, p)
	qr, qrErr := totp.QRDataURI(uri)
	if qrErr != nil {
		// QR을 못 만들어도 등록은 계속된다. 수동 입력 경로가 있기 때문이다.
		slog.Warn("QR 코드를 만들지 못했습니다 (수동 입력으로 안내합니다)", "err", qrErr)
	}

	return &Enrollment{
		Secret:          secret,
		FormattedSecret: totp.FormatSecret(secret),
		URI:             uri,
		QR:              qr,
		Digits:          p.Digits,
		Period:          int(p.Period / time.Second),
	}, nil
}

// ConfirmTOTPEnrollment는 코드를 확인해 등록을 확정하고 복구 코드를 발급한다.
//
// 여기서는 넓은 창으로 훑는다. 로그인과 달리 이 순간의 코드는 **시각을 재는 자**이기
// 때문이다: 사용자는 방금 앱에서 읽은 값을 넣었으므로, 몇 칸 어긋났는지가 곧
// 우리 시계가 얼마나 틀렸는지다. 그 값을 여기서 확정해 두어야 다음 로그인부터
// 좁은 창으로 검사할 수 있다.
func (s *Service) ConfirmTOTPEnrollment(ctx context.Context, userID, code string) ([]string, error) {
	e, err := s.st.GetTOTP(ctx, userID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrTOTPNotEnrolled
	}
	if err != nil {
		return nil, err
	}
	if e.Confirmed() {
		return nil, store.ErrTOTPConfirmed
	}
	if e.Locked(time.Now().UTC()) {
		return nil, ErrTOTPLocked
	}

	p := totp.Params{Digits: e.Digits, Period: e.Period}
	delta, step, ok := totp.Verify(e.Secret, code, s.clock.BaseNow().Add(e.Skew), resyncWindow, p)
	if !ok {
		if _, err := s.st.RecordTOTPFailure(ctx, userID, maxCodeFailures, codeLockFor); err != nil {
			return nil, err
		}
		return nil, ErrTOTPInvalid
	}

	skew := e.Skew + time.Duration(delta)*e.Period
	s.clock.Learn(skew, clock.WeightEnroll)

	if err := s.st.ConfirmTOTP(ctx, userID, skew, step); err != nil {
		return nil, err
	}

	codes, err := generateRecoveryCodes()
	if err != nil {
		return nil, err
	}
	if err := s.st.ReplaceRecoveryCodes(ctx, userID, codes); err != nil {
		return nil, err
	}
	return codes, nil
}

// DisableTOTP은 2단계 인증을 끈다. 현재 비밀번호를 다시 확인한다.
//
// 비밀번호를 묻는 이유: 자리를 비운 사이 열린 화면에서 몇 번의 클릭만으로 두 번째
// 요소가 사라지면, 그 요소는 있으나 마나다.
func (s *Service) DisableTOTP(ctx context.Context, userID, password string) error {
	policy, err := s.st.SecurityPolicy(ctx)
	if err != nil {
		return err
	}
	if policy.TOTPRequired {
		return ErrTOTPEnforced
	}

	u, err := s.st.GetUser(ctx, userID)
	if err != nil {
		return err
	}
	ok, err := crypto.VerifyPassword(password, u.PasswordHash)
	if err != nil {
		return fmt.Errorf("verify password: %w", err)
	}
	if !ok {
		return ErrInvalidCredentials
	}

	if err := s.st.DeleteTOTP(ctx, userID); errors.Is(err, store.ErrNotFound) {
		return ErrTOTPNotEnrolled
	} else if err != nil {
		return err
	}
	return nil
}

// ResetTOTP은 관리자가 다른 사용자의 2단계 인증을 초기화한다.
// 인증 앱을 잃고 복구 코드도 없는 사람을 위한 마지막 경로다.
func (s *Service) ResetTOTP(ctx context.Context, targetID string) error {
	if err := s.st.DeleteTOTP(ctx, targetID); errors.Is(err, store.ErrNotFound) {
		return ErrTOTPNotEnrolled
	} else if err != nil {
		return err
	}
	// 세션도 함께 끊는다. 초기화는 보통 계정을 되찾거나 빼앗긴 상황에서 쓰이므로,
	// 열려 있던 화면이 계속 살아 있으면 초기화한 의미가 없다.
	return s.st.DeleteUserSessions(ctx, targetID)
}

// RegenerateRecoveryCodes는 복구 코드를 새로 발급한다. 옛 코드는 즉시 죽는다.
func (s *Service) RegenerateRecoveryCodes(ctx context.Context, userID, code string) ([]string, error) {
	e, err := s.st.GetTOTP(ctx, userID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && !e.Confirmed()) {
		return nil, ErrTOTPNotEnrolled
	}
	if err != nil {
		return nil, err
	}
	// 지금 인증 앱을 들고 있는 사람만 재발급할 수 있다. 화면을 열어 둔 것만으로
	// 새 복구 코드를 뽑을 수 있으면 복구 코드는 두 번째 요소가 아니게 된다.
	if err := s.verifyEnrolledCode(ctx, e, code); err != nil {
		return nil, err
	}

	codes, err := generateRecoveryCodes()
	if err != nil {
		return nil, err
	}
	if err := s.st.ReplaceRecoveryCodes(ctx, userID, codes); err != nil {
		return nil, err
	}
	return codes, nil
}

// verifyEnrolledCode는 등록을 마친 사용자의 코드를 검증하고 시각 보정을 갱신한다.
//
// 이 함수가 이 기능의 심장이다. 세 가지를 함께 한다:
//   - 좁은 창(±30초)으로 검증한다.
//   - 성공하면 어긋난 만큼을 사용자 보정값에 반영하고 전역 시계에도 알린다(느린 드리프트 추적).
//   - 실패했지만 넓은 창에서는 맞는 코드라면, 인증은 거절하되 보정값만 고친다(시계 재동기화).
func (s *Service) verifyEnrolledCode(ctx context.Context, e *store.TOTPEnrollment, code string) error {
	now := time.Now().UTC()
	if e.Locked(now) {
		return ErrTOTPLocked
	}
	p := totp.Params{Digits: e.Digits, Period: e.Period}
	at := s.clock.BaseNow().Add(e.Skew)

	if delta, step, ok := totp.Verify(e.Secret, code, at, verifyWindow, p); ok {
		// 재사용 방지. 같은 코드는 30초 동안 유효하므로, 이 검사가 없으면
		// 어깨너머로 본 코드나 가로챈 코드를 그 안에 다시 쓸 수 있다.
		if step <= e.LastStep {
			return ErrTOTPReused
		}
		skew := e.Skew + time.Duration(delta)*e.Period
		s.clock.Learn(skew, clock.WeightLogin)
		return s.st.RecordTOTPSuccess(ctx, e.UserID, skew, step)
	}

	// 좁은 창에서 틀렸다. 코드가 틀린 것인지 시계가 틀린 것인지 구분한다.
	if delta, step, ok := totp.Verify(e.Secret, code, at, resyncWindow, p); ok && step > e.LastStep {
		skew := e.Skew + time.Duration(delta)*e.Period
		s.clock.Learn(skew, clock.WeightEnroll)
		if err := s.st.RecordTOTPResync(ctx, e.UserID, skew, step); err != nil {
			return err
		}
		slog.Warn("2단계 인증 시각을 재동기화했습니다",
			"user", e.UserID, "보정", skew.Round(time.Second),
			"hint", "서버 시계가 어긋나 있을 수 있습니다. 보안 설정 화면에서 확인하세요")
		// 재동기화도 실패로 센다. 넓은 창을 무한정 두드릴 수 있으면
		// 시도 횟수 제한이 없는 것과 같다.
		if _, err := s.st.RecordTOTPFailure(ctx, e.UserID, maxCodeFailures, codeLockFor); err != nil {
			return err
		}
		return ErrTOTPResynced
	}

	locked, err := s.st.RecordTOTPFailure(ctx, e.UserID, maxCodeFailures, codeLockFor)
	if err != nil {
		return err
	}
	if locked != nil {
		return ErrTOTPLocked
	}
	return ErrTOTPInvalid
}

// CompleteTOTPLogin은 2단계를 마치고 세션을 발급한다.
//
// code는 인증 앱의 코드이거나 복구 코드다. 둘을 한 입력칸에서 받는 이유는
// 인증 앱을 잃은 사람이 "복구 코드는 어디에 넣지"를 고민하지 않게 하기 위해서다.
func (s *Service) CompleteTOTPLogin(ctx context.Context, challenge, code, ip, ua string) (*LoginResult, error) {
	if challenge == "" {
		return nil, ErrChallengeInvalid
	}
	ch, err := s.st.LookupTOTPChallenge(ctx, challenge)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrChallengeInvalid
	}
	if err != nil {
		return nil, err
	}

	attempts, err := s.st.BumpTOTPChallengeAttempt(ctx, challenge)
	if err != nil {
		return nil, err
	}
	if attempts > maxChallengeAttempts {
		_ = s.st.DeleteTOTPChallenge(ctx, challenge)
		return nil, ErrChallengeInvalid
	}

	u, err := s.st.GetUser(ctx, ch.UserID)
	if err != nil {
		return nil, err
	}
	// 1단계와 2단계 사이에 계정이 잠겼을 수 있다. 그 사이를 비워 두면
	// "비활성화했는데 로그인되는" 창이 5분 생긴다.
	if model.UserStatus(u.Status) == model.UserDisabled {
		_ = s.st.DeleteTOTPChallenge(ctx, challenge)
		return nil, ErrAccountDisabled
	}

	e, err := s.st.GetTOTP(ctx, u.ID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && !e.Confirmed()) {
		// 1단계 통과 뒤 관리자가 2FA를 초기화한 경우다. 챌린지를 버리고
		// 처음부터 다시 로그인하게 한다 — 여기서 세션을 내주면 초기화가 곧 무방비다.
		_ = s.st.DeleteTOTPChallenge(ctx, challenge)
		return nil, ErrChallengeInvalid
	}
	if err != nil {
		return nil, err
	}

	method, err := s.consumeSecondFactor(ctx, u.ID, e, code, ip)
	if err != nil {
		return nil, err
	}

	_ = s.st.DeleteTOTPChallenge(ctx, challenge)
	res, err := s.issueLoginSession(ctx, u, ip, ua)
	if err != nil {
		return nil, err
	}
	res.Method = method
	return res, nil
}

// consumeSecondFactor는 인증 앱 코드와 복구 코드를 모두 받아 처리한다.
func (s *Service) consumeSecondFactor(ctx context.Context, userID string, e *store.TOTPEnrollment, code, ip string) (string, error) {
	normalized := totp.NormalizeCode(code)
	// 자리수로 갈라 본다. 인증 앱 코드는 6자리 숫자, 복구 코드는 16글자다.
	if len(normalized) == e.Digits {
		return MethodTOTP, s.verifyEnrolledCode(ctx, e, normalized)
	}

	ok, err := s.st.UseRecoveryCode(ctx, userID, normalizeRecoveryCode(code), ip)
	if err != nil {
		return "", err
	}
	if !ok {
		if _, err := s.st.RecordTOTPFailure(ctx, userID, maxCodeFailures, codeLockFor); err != nil {
			return "", err
		}
		return "", ErrTOTPInvalid
	}
	slog.Info("복구 코드로 로그인했습니다", "user", userID, "ip", ip,
		"hint", "인증 앱을 다시 등록하고 복구 코드를 재발급하도록 안내하세요")
	return MethodRecovery, nil
}

func generateRecoveryCodes() ([]string, error) {
	codes := make([]string, 0, recoveryCodeCount)
	for i := 0; i < recoveryCodeCount; i++ {
		c, err := crypto.GenerateRecoveryCode()
		if err != nil {
			return nil, err
		}
		codes = append(codes, c)
	}
	return codes, nil
}

// normalizeRecoveryCode는 저장 형태(소문자, 하이픈 포함)로 맞춘다.
// 사용자가 하이픈을 빼거나 대문자로 적어도 통과해야 한다.
func normalizeRecoveryCode(code string) string {
	cleaned := totp.NormalizeCode(strings.ToLower(code))
	if len(cleaned) != 16 {
		return cleaned
	}
	return cleaned[0:4] + "-" + cleaned[4:8] + "-" + cleaned[8:12] + "-" + cleaned[12:16]
}
