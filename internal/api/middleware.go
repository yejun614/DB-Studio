package api

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"

	"dbstudio/internal/auth"
	"dbstudio/internal/model"
	"dbstudio/internal/store"
)

// Fiber Locals 키. 문자열 오타를 막기 위해 상수로 둔다.
const (
	localUser    = "user"
	localSession = "session"
)

// apiError는 프론트엔드가 코드로 분기할 수 있는 에러 응답 형식이다.
type apiError struct {
	Error   string `json:"error"`   // 기계용 코드
	Message string `json:"message"` // 사용자 표시용 한국어 메시지
	Detail  string `json:"detail,omitempty"`
}

func fail(c *fiber.Ctx, status int, code, message string) error {
	return c.Status(status).JSON(apiError{Error: code, Message: message})
}

func failDetail(c *fiber.Ctx, status int, code, message, detail string) error {
	return c.Status(status).JSON(apiError{Error: code, Message: message, Detail: detail})
}

// errorHandler는 핸들러에서 반환된 에러를 JSON 또는 HTML로 변환한다.
// requestLog은 문제 있는 요청만 기록한다.
//
// 모든 요청을 기록하지 않는 이유: 정적 자산과 폴링(대시보드는 주기적으로 조회한다)이
// 로그를 채우면 정작 필요한 줄을 찾을 수 없다. 그래서 세 가지만 남긴다.
//   - 5xx: 서버가 잘못한 것
//   - 느린 요청: 원인 조사의 출발점
//   - debug 레벨: 켜면 전부
//
// 이 미들웨어는 오류 응답을 만들지 않는다. 응답 생성은 errorHandler의 몫이고
// 여기서는 결과만 관찰한다.
func requestLog(c *fiber.Ctx) error {
	// 정적 자산은 조사에 쓸모가 없고 개수가 압도적으로 많다.
	if !strings.HasPrefix(c.Path(), "/api/") {
		return c.Next()
	}
	start := time.Now()
	err := c.Next()
	elapsed := time.Since(start)
	status := c.Response().StatusCode()

	attrs := []any{
		"method", c.Method(), "path", c.Path(), "status", status,
		"ms", elapsed.Milliseconds(), "ip", clientIP(c),
	}
	if u := currentUser(c); u != nil {
		attrs = append(attrs, "user", u.Username)
	}
	if err != nil {
		attrs = append(attrs, "err", err.Error())
	}

	switch {
	case status >= 500:
		slog.Error("요청 실패", attrs...)
	case elapsed >= slowRequest:
		slog.Warn("느린 요청", append(attrs, "threshold", slowRequest)...)
	default:
		slog.Debug("요청", attrs...)
	}
	return err
}

// slowRequest는 이 시간을 넘긴 요청을 경고로 남기는 기준이다.
// 스키마 조회나 마이그레이션 실행은 원래 오래 걸리므로 넉넉하게 잡는다 —
// 기준이 낮으면 경고가 일상이 되고, 일상이 된 경고는 읽히지 않는다.
const slowRequest = 5 * time.Second

func errorHandler(c *fiber.Ctx, err error) error {
	status := fiber.StatusInternalServerError
	code := "internal_error"
	message := "서버 내부 오류가 발생했습니다"

	var fe *fiber.Error
	if errors.As(err, &fe) {
		status = fe.Code
		message = fe.Message
		switch status {
		case fiber.StatusNotFound:
			code = "not_found"
		case fiber.StatusUnauthorized:
			code = "unauthorized"
		case fiber.StatusForbidden:
			code = "forbidden"
		case fiber.StatusBadRequest:
			code = "bad_request"
		default:
			code = "error"
		}
	} else {
		// fiber.Error가 아닌 오류는 핸들러가 예상하지 못한 것이다.
		// 스택은 없지만 어떤 요청에서 났는지는 남겨야 조사할 수 있다.
		slog.Error("처리되지 않은 요청 오류",
			"method", c.Method(), "path", c.Path(), "ip", clientIP(c), "err", err)
	}

	if strings.HasPrefix(c.Path(), "/api/") {
		return c.Status(status).JSON(apiError{Error: code, Message: message})
	}
	return c.Status(status).SendString(message)
}

// requireAuth는 세션 쿠키를 검증하고 Locals에 사용자를 심는다.
// 상태 변경 요청에는 커스텀 헤더를 요구해 SameSite=Lax와 함께 CSRF를 막는다.
func (s *Server) requireAuth(c *fiber.Ctx) error {
	token := c.Cookies(auth.SessionCookieName)
	u, sess, err := s.authn.Authenticate(c.Context(), token)
	if err != nil {
		if errors.Is(err, auth.ErrNoSession) {
			s.clearSessionCookie(c)
			return fail(c, fiber.StatusUnauthorized, "unauthorized", "로그인이 필요합니다")
		}
		return err
	}

	if isStateChanging(c.Method()) && c.Get("X-Requested-With") != "dbstudio" {
		return fail(c, fiber.StatusForbidden, "csrf",
			"잘못된 요청입니다 (X-Requested-With 헤더 누락)")
	}

	// 비밀번호 변경이 강제된 사용자는 변경 API와 조회 API만 사용할 수 있다.
	if u.MustChangePassword && !isPasswordChangeExempt(c) {
		return fail(c, fiber.StatusForbidden, "password_change_required",
			"비밀번호를 먼저 변경해야 합니다")
	}

	// 2단계 인증이 의무화되었는데 아직 등록하지 않은 사용자도 같은 방식으로 막는다.
	//
	// 로그인 자체를 막지 않는 이유: 막으면 등록할 방법이 없다. 대신 들어오게 하되
	// 등록 경로만 열어 둔다 — 비밀번호 강제 변경과 같은 구조이며, 프론트엔드도
	// 같은 방식(전용 화면으로 전환)으로 처리한다.
	if !u.TOTPEnabled && !isTOTPSetupExempt(c) {
		policy, err := s.st.SecurityPolicy(c.Context())
		if err != nil {
			return err
		}
		if policy.TOTPRequired {
			return fail(c, fiber.StatusForbidden, "totp_setup_required",
				"2단계 인증을 먼저 설정해야 합니다")
		}
	}

	c.Locals(localUser, u)
	c.Locals(localSession, sess)
	return c.Next()
}

func isStateChanging(method string) bool {
	switch method {
	case fiber.MethodPost, fiber.MethodPut, fiber.MethodPatch, fiber.MethodDelete:
		return true
	}
	return false
}

// isPasswordChangeExempt는 비밀번호 변경 강제 상태에서도 허용할 경로를 판별한다.
func isPasswordChangeExempt(c *fiber.Ctx) bool {
	p := c.Path()
	switch p {
	case "/api/v1/auth/password", "/api/v1/auth/me", "/api/v1/auth/logout", "/api/v1/meta":
		return true
	}
	return false
}

// isTOTPSetupExempt는 2단계 인증 등록 강제 상태에서도 허용할 경로를 판별한다.
//
// 목록을 짧게 유지하는 것이 중요하다. 여기 들어간 경로는 "2FA 없이 부를 수 있는
// API"이므로, 하나 늘릴 때마다 의무화의 의미가 그만큼 줄어든다. 지금 있는 것은
// 등록을 마치는 데 꼭 필요한 것과, 화면을 그리는 데 필요한 조회뿐이다.
func isTOTPSetupExempt(c *fiber.Ctx) bool {
	switch c.Path() {
	case "/api/v1/auth/totp", "/api/v1/auth/totp/setup", "/api/v1/auth/totp/confirm",
		"/api/v1/auth/me", "/api/v1/auth/logout", "/api/v1/auth/password", "/api/v1/meta":
		return true
	}
	return false
}

// requireRole은 지정한 역할 중 하나를 가진 사용자만 통과시킨다.
func (s *Server) requireRole(roles ...model.Role) fiber.Handler {
	return func(c *fiber.Ctx) error {
		u := currentUser(c)
		if u == nil {
			return fail(c, fiber.StatusUnauthorized, "unauthorized", "로그인이 필요합니다")
		}
		for _, r := range roles {
			if u.Role == r {
				return c.Next()
			}
		}
		s.auditDenied(c, "role.denied", string(u.Role))
		return fail(c, fiber.StatusForbidden, "forbidden", "이 작업을 수행할 권한이 없습니다")
	}
}

// requireConnManager는 커넥션을 등록/수정할 수 있는 역할만 통과시킨다.
func (s *Server) requireConnManager(c *fiber.Ctx) error {
	u := currentUser(c)
	if u == nil {
		return fail(c, fiber.StatusUnauthorized, "unauthorized", "로그인이 필요합니다")
	}
	if !u.Role.CanManageConnections() {
		s.auditDenied(c, "connection.manage.denied", string(u.Role))
		return fail(c, fiber.StatusForbidden, "forbidden", "커넥션을 관리할 권한이 없습니다")
	}
	return c.Next()
}

func currentUser(c *fiber.Ctx) *model.User {
	u, _ := c.Locals(localUser).(*model.User)
	return u
}

func currentSession(c *fiber.Ctx) *model.Session {
	sess, _ := c.Locals(localSession).(*model.Session)
	return sess
}

// clientIP는 기록에 쓸 클라이언트 IP를 반환한다.
//
// 프록시 헤더가 없거나 형식이 틀리면 c.IP()가 빈 문자열을 돌려준다(IP 검증을 켠 결과).
// 빈 값을 저장하면 화면에서 "IP 기록 없음"과 구분되지 않으므로 실제 원격 주소로 되돌린다.
func clientIP(c *fiber.Ctx) string {
	if ip := c.IP(); ip != "" {
		return ip
	}
	return c.Context().RemoteIP().String()
}

// setSessionCookie는 세션 쿠키를 발급한다.
func (s *Server) setSessionCookie(c *fiber.Ctx, token string) {
	c.Cookie(&fiber.Cookie{
		Name:     auth.SessionCookieName,
		Value:    token,
		Path:     "/",
		HTTPOnly: true,
		Secure:   s.cfg.SecureCookie,
		SameSite: "Lax",
		Expires:  time.Now().Add(s.authn.SessionTTL()),
	})
}

func (s *Server) clearSessionCookie(c *fiber.Ctx) {
	c.Cookie(&fiber.Cookie{
		Name:     auth.SessionCookieName,
		Value:    "",
		Path:     "/",
		HTTPOnly: true,
		Secure:   s.cfg.SecureCookie,
		SameSite: "Lax",
		Expires:  time.Now().Add(-time.Hour),
		MaxAge:   -1,
	})
}

// audit은 감사 로그를 기록한다. 감사 실패가 본 작업을 실패시키지 않도록 로깅만 한다.
func (s *Server) audit(c *fiber.Ctx, p store.AuditParams) {
	if u := currentUser(c); u != nil {
		if p.ActorID == "" {
			p.ActorID = u.ID
		}
		if p.ActorName == "" {
			p.ActorName = u.Username
		}
	}
	if p.IP == "" {
		p.IP = clientIP(c)
	}
	if err := s.st.Audit(c.Context(), p); err != nil {
		slog.Error("failed to write audit log", "action", p.Action, "err", err)
	}
}

// auditDirect는 fiber 컨텍스트 없이 감사 로그를 남긴다.
//
// SSE 스트리밍과 AI 툴 실행은 핸들러가 반환한 뒤에 동작하므로 *fiber.Ctx를 만질 수
// 없다(이미 해제된 메모리다). 그래서 행위자와 IP를 값으로 받는 경로가 필요하다.
func (s *Server) auditDirect(ctx context.Context, actorID, actorName, ip string, p store.AuditParams) {
	if p.ActorID == "" {
		p.ActorID = actorID
	}
	if p.ActorName == "" {
		p.ActorName = actorName
	}
	if p.IP == "" {
		p.IP = ip
	}
	if err := s.st.Audit(ctx, p); err != nil {
		slog.Error("failed to write audit log", "action", p.Action, "err", err)
	}
}

func (s *Server) auditDenied(c *fiber.Ctx, action, detail string) {
	s.audit(c, store.AuditParams{
		Action: action,
		Result: "denied",
		Detail: map[string]any{"path": c.Path(), "method": c.Method(), "role": detail},
	})
}
