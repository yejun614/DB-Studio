package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"dbstudio/internal/clock"
	"dbstudio/internal/crypto"
	"dbstudio/internal/model"
	"dbstudio/internal/store"
)

const SessionCookieName = "dbstudio_session"

// 세션 last_seen_at을 갱신하는 최소 간격. 매 요청마다 쓰기를 유발하지 않기 위한 값.
const touchInterval = time.Minute

var (
	ErrInvalidCredentials = errors.New("아이디 또는 비밀번호가 올바르지 않습니다")
	ErrAccountDisabled    = errors.New("비활성화된 계정입니다")
	ErrNoSession          = errors.New("세션이 없거나 만료되었습니다")
	// ErrInvalidToken은 API 토큰이 없거나 폐기·만료된 경우다.
	//
	// 세 경우를 하나의 오류로 합치는 것이 의도다: 어느 쪽인지 알려주면
	// "이 토큰은 존재하지만 폐기됐다"는 정보를 주게 되고, 그것은 토큰을 주운 사람에게
	// 유일하게 쓸모 있는 정보다.
	ErrInvalidToken = errors.New("API 토큰이 유효하지 않습니다")
)

// TokenPrefix는 API 토큰의 접두사다. 로그나 저장소에서 눈으로 알아보게 한다.
const TokenPrefix = "dbs_"

// tokenTouchInterval은 토큰 사용 기록을 갱신하는 최소 간격이다.
// MCP 클라이언트는 툴 목록을 자주 물어보므로 세션보다 넉넉하게 잡는다.
const tokenTouchInterval = 5 * time.Minute

// Service는 로그인/로그아웃/세션 검증과 비밀번호 변경을 담당한다.
type Service struct {
	st         *store.Store
	sessionTTL time.Duration
	// clock은 TOTP 계산에 쓰는 내부 시계다. time.Now()를 직접 쓰지 않는 이유는
	// internal/clock 패키지 주석에 적어 두었다.
	clock *clock.Clock
}

func NewService(st *store.Store, sessionTTL time.Duration, clk *clock.Clock) *Service {
	return &Service{st: st, sessionTTL: sessionTTL, clock: clk}
}

func (s *Service) SessionTTL() time.Duration { return s.sessionTTL }

// LoginResult는 로그인 결과다. Token은 쿠키로만 전달한다.
type LoginResult struct {
	Token   string
	User    *model.User
	Session *model.Session

	// Challenge가 비어 있지 않으면 **아직 로그인된 것이 아니다.** 비밀번호는 맞았고
	// 2단계 인증 코드를 기다리는 상태이며, 이 값이 그 상태를 가리키는 표다.
	Challenge string

	// Method는 두 번째 요소로 무엇을 썼는지다("totp" 또는 "recovery").
	// 감사 로그가 이 둘을 구분해야 한다 — 복구 코드가 쓰였다는 것은
	// 누군가 인증 앱을 잃었거나 종이가 남의 손에 들어갔다는 신호다.
	Method string
}

// 두 번째 요소의 종류.
const (
	MethodTOTP     = "totp"
	MethodRecovery = "recovery"
)

// NeedsTOTP는 2단계 인증이 남았는지 반환한다.
func (r *LoginResult) NeedsTOTP() bool { return r != nil && r.Challenge != "" }

// Login은 자격증명을 검증한다.
//
// 2단계 인증을 켠 계정이면 세션 대신 챌린지를 돌려준다 — 이 단계에서 세션을 만들면
// 두 번째 요소는 형식일 뿐이고, 비밀번호만 아는 사람이 이미 들어와 있게 된다.
func (s *Service) Login(ctx context.Context, username, password, ip, ua string) (*LoginResult, error) {
	u, err := s.st.GetUserByUsername(ctx, username)
	if errors.Is(err, store.ErrNotFound) {
		// 사용자 존재 여부가 응답 시간으로 드러나지 않도록 더미 해시를 검증한다.
		_, _ = crypto.VerifyPassword(password, dummyHash)
		return nil, ErrInvalidCredentials
	}
	if err != nil {
		return nil, fmt.Errorf("lookup user: %w", err)
	}

	ok, err := crypto.VerifyPassword(password, u.PasswordHash)
	if err != nil {
		return nil, fmt.Errorf("verify password: %w", err)
	}
	if !ok {
		return nil, ErrInvalidCredentials
	}
	if model.UserStatus(u.Status) == model.UserDisabled {
		return nil, ErrAccountDisabled
	}

	// 2단계 인증을 마친 계정은 여기서 멈춘다.
	//
	// 비밀번호 변경이 강제된 계정도 예외가 아니다. "비밀번호를 바꿔야 하니 두 번째
	// 요소는 건너뛰자"는 순간, 임시 비밀번호를 손에 넣은 사람이 그 경로로 들어온다.
	e, err := s.st.GetTOTP(ctx, u.ID)
	switch {
	case err != nil && !errors.Is(err, store.ErrNotFound):
		return nil, fmt.Errorf("lookup totp: %w", err)
	case e.Confirmed():
		challenge, err := crypto.RandomToken(32)
		if err != nil {
			return nil, fmt.Errorf("generate challenge token: %w", err)
		}
		if _, err := s.st.CreateTOTPChallenge(ctx, challenge, u.ID, ip, ua, ChallengeTTL); err != nil {
			return nil, fmt.Errorf("create totp challenge: %w", err)
		}
		return &LoginResult{User: u, Challenge: challenge}, nil
	}

	return s.issueLoginSession(ctx, u, ip, ua)
}

// issueLoginSession은 인증이 모두 끝난 뒤 세션을 발급한다.
// 한 곳에 모아 두는 이유: 로그인 경로가 둘(1단계만, 2단계까지)이 되었고,
// 세션 발급과 로그인 기록이 어느 한쪽에서만 일어나면 감사 기록에 구멍이 생긴다.
func (s *Service) issueLoginSession(ctx context.Context, u *model.User, ip, ua string) (*LoginResult, error) {
	token, err := crypto.RandomToken(32)
	if err != nil {
		return nil, fmt.Errorf("generate session token: %w", err)
	}
	sess, err := s.st.CreateSession(ctx, token, u.ID, s.sessionTTL, ip, ua)
	if err != nil {
		return nil, fmt.Errorf("create session: %w", err)
	}
	if err := s.st.TouchLastLogin(ctx, u.ID, ip); err != nil {
		return nil, fmt.Errorf("touch last login: %w", err)
	}
	return &LoginResult{Token: token, User: u, Session: sess}, nil
}

// Authenticate는 토큰으로 현재 사용자와 세션을 되살린다.
func (s *Service) Authenticate(ctx context.Context, token string) (*model.User, *model.Session, error) {
	if token == "" {
		return nil, nil, ErrNoSession
	}
	sess, u, err := s.st.LookupSession(ctx, token)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil, ErrNoSession
	}
	if err != nil {
		return nil, nil, fmt.Errorf("lookup session: %w", err)
	}
	s.st.TouchSession(ctx, sess, touchInterval)
	return u, sess, nil
}

// IssueAPIToken은 새 API 토큰을 만들고 **원문을 한 번만** 반환한다.
//
// 원문을 저장하지 않으므로 이 반환값을 잃으면 되찾을 방법이 없다. 그것이 의도다 —
// 되찾을 수 있는 토큰은 DB가 유출되면 함께 유출된다.
func (s *Service) IssueAPIToken(ctx context.Context, p store.CreateTokenParams) (*store.APIToken, string, error) {
	raw, err := crypto.RandomToken(32)
	if err != nil {
		return nil, "", fmt.Errorf("generate api token: %w", err)
	}
	token := TokenPrefix + raw

	// 접두사는 목록에서 어느 토큰인지 알아보기 위한 것이다. 원문을 복원할 수 없을
	// 만큼만 남긴다(8자면 눈으로 구별되면서 무차별 대입에는 쓸모가 없다).
	p.Token = token
	p.Prefix = token[:min(len(token), len(TokenPrefix)+8)]

	saved, err := s.st.CreateAPIToken(ctx, p)
	if err != nil {
		return nil, "", err
	}
	return saved, token, nil
}

// AuthenticateToken은 API 토큰으로 사용자를 찾는다.
//
// 세션 인증(Authenticate)과 나란히 두는 이유: 두 경로 모두 "요청 → 사용자"를 만드는
// 유일한 통로여야 한다. 어느 한쪽이 다른 규칙(비활성 계정 허용 등)을 쓰기 시작하면
// 그 차이가 곧 우회 경로가 된다.
func (s *Service) AuthenticateToken(ctx context.Context, raw, ip string) (*model.User, *store.APIToken, error) {
	if raw == "" {
		return nil, nil, ErrInvalidToken
	}
	t, u, err := s.st.LookupAPIToken(ctx, raw)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil, ErrInvalidToken
	}
	if err != nil {
		return nil, nil, fmt.Errorf("lookup api token: %w", err)
	}
	if !t.Active() {
		return nil, nil, ErrInvalidToken
	}
	// 비활성 계정의 토큰은 죽는다. 계정을 잠갔는데 토큰이 살아 있으면 잠근 것이 아니다.
	if model.UserStatus(u.Status) == model.UserDisabled {
		return nil, nil, ErrAccountDisabled
	}
	// 비밀번호 변경이 강제된 사용자도 막는다. 화면에서 막아 둔 사람이
	// 토큰으로는 통과한다면 그 게이트는 없는 것과 같다.
	if u.MustChangePassword {
		return nil, nil, ErrInvalidToken
	}
	// 2단계 인증 의무화도 같은 이유로 여기에 있어야 한다. 화면에서 등록을 마칠
	// 때까지 막아 놓았는데 토큰으로는 모든 API가 열린다면 의무화한 것이 아니다.
	if !u.TOTPEnabled {
		policy, err := s.st.SecurityPolicy(ctx)
		if err != nil {
			return nil, nil, err
		}
		if policy.TOTPRequired {
			return nil, nil, ErrTOTPRequired
		}
	}
	s.st.TouchAPIToken(ctx, t, ip, tokenTouchInterval)
	return u, t, nil
}

func (s *Service) Logout(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	return s.st.DeleteSession(ctx, token)
}

// ChangeOwnPassword는 현재 비밀번호를 확인한 뒤 교체하고, 다른 세션을 모두 무효화한다.
func (s *Service) ChangeOwnPassword(ctx context.Context, userID, current, next string) error {
	u, err := s.st.GetUser(ctx, userID)
	if err != nil {
		return fmt.Errorf("get user: %w", err)
	}
	ok, err := crypto.VerifyPassword(current, u.PasswordHash)
	if err != nil {
		return fmt.Errorf("verify current password: %w", err)
	}
	if !ok {
		return ErrInvalidCredentials
	}
	if err := crypto.CheckPasswordPolicy(next); err != nil {
		return err
	}
	if current == next {
		return &crypto.PasswordPolicyError{Reason: "새 비밀번호가 기존 비밀번호와 같습니다"}
	}
	hash, err := crypto.HashPassword(next)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}
	if err := s.st.SetPassword(ctx, userID, hash, false); err != nil {
		return fmt.Errorf("set password: %w", err)
	}
	// 비밀번호가 바뀌면 기존 세션은 모두 폐기한다. 호출부가 새 세션을 발급해야 한다.
	if err := s.st.DeleteUserSessions(ctx, userID); err != nil {
		return fmt.Errorf("invalidate sessions: %w", err)
	}
	return nil
}

// ResetPassword는 관리자가 다른 사용자의 비밀번호를 재설정한다.
// 반환값은 생성된 임시 비밀번호이며, 대상 사용자는 다음 로그인 시 변경을 강제받는다.
func (s *Service) ResetPassword(ctx context.Context, targetID string, explicit string) (string, error) {
	password := explicit
	if password == "" {
		generated, err := crypto.GeneratePassword(20)
		if err != nil {
			return "", fmt.Errorf("generate password: %w", err)
		}
		password = generated
	} else if err := crypto.CheckPasswordPolicy(password); err != nil {
		return "", err
	}
	hash, err := crypto.HashPassword(password)
	if err != nil {
		return "", fmt.Errorf("hash password: %w", err)
	}
	if err := s.st.SetPassword(ctx, targetID, hash, true); err != nil {
		return "", fmt.Errorf("set password: %w", err)
	}
	if err := s.st.DeleteUserSessions(ctx, targetID); err != nil {
		return "", fmt.Errorf("invalidate sessions: %w", err)
	}
	return password, nil
}

// IssueSession은 비밀번호 변경 직후처럼 기존 세션을 폐기한 뒤 새 세션이 필요한 경우에 쓴다.
func (s *Service) IssueSession(ctx context.Context, userID, ip, ua string) (string, error) {
	token, err := crypto.RandomToken(32)
	if err != nil {
		return "", fmt.Errorf("generate session token: %w", err)
	}
	if _, err := s.st.CreateSession(ctx, token, userID, s.sessionTTL, ip, ua); err != nil {
		return "", fmt.Errorf("create session: %w", err)
	}
	return token, nil
}

// dummyHash는 존재하지 않는 사용자에 대해서도 argon2 검증 비용을 지불해
// 사용자 열거(user enumeration)를 어렵게 만든다. 값 자체는 의미 없다.
const dummyHash = "$argon2id$v=19$m=65536,t=3,p=4$AAAAAAAAAAAAAAAAAAAAAA$" +
	"NGKKKMj5o5tPRxvMlBGGHwzMQmJTRHTQ4KOKmuFcqBE"
