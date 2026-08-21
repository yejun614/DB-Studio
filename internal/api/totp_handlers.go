package api

import (
	"errors"
	"time"

	"github.com/gofiber/fiber/v2"

	"dbstudio/internal/auth"
	"dbstudio/internal/store"
)

// 2단계 인증 API.
//
// 응답 규칙 하나를 지킨다: **어떤 오류든 "이 계정이 존재하는가"를 알려주지 않는다.**
// 로그인 1단계가 이미 그 규칙을 지키고 있으므로, 2단계에서 "그런 사용자 없음"과
// "코드 틀림"을 구분해 주면 1단계에서 막아 둔 것이 새어 나간다.

// setChallengeCookie는 2단계를 기다리는 상태를 쿠키에 담는다.
//
// 응답 본문이 아니라 쿠키인 이유: HttpOnly로 두면 스크립트가 읽지 못하므로,
// XSS가 하나 있어도 "비밀번호를 통과한 상태"를 훔쳐 갈 수 없다. 프론트엔드는
// 이 값을 알 필요가 없다 — 코드를 보낼 때 브라우저가 알아서 붙인다.
func (s *Server) setChallengeCookie(c *fiber.Ctx, token string) {
	c.Cookie(&fiber.Cookie{
		Name:     auth.ChallengeCookieName,
		Value:    token,
		Path:     "/api/v1/auth",
		HTTPOnly: true,
		Secure:   s.cfg.SecureCookie,
		SameSite: "Lax",
		Expires:  time.Now().Add(auth.ChallengeTTL),
	})
}

func (s *Server) clearChallengeCookie(c *fiber.Ctx) {
	c.Cookie(&fiber.Cookie{
		Name:     auth.ChallengeCookieName,
		Value:    "",
		Path:     "/api/v1/auth",
		HTTPOnly: true,
		Secure:   s.cfg.SecureCookie,
		SameSite: "Lax",
		Expires:  time.Now().Add(-time.Hour),
		MaxAge:   -1,
	})
}

type totpLoginRequest struct {
	Code string `json:"code"`
}

// handleLoginTOTP는 로그인 2단계다. 코드는 인증 앱의 것이거나 복구 코드다.
func (s *Server) handleLoginTOTP(c *fiber.Ctx) error {
	var req totpLoginRequest
	if err := c.BodyParser(&req); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "요청 형식이 올바르지 않습니다")
	}
	challenge := c.Cookies(auth.ChallengeCookieName)

	res, err := s.authn.CompleteTOTPLogin(c.Context(), challenge, req.Code,
		clientIP(c), c.Get(fiber.HeaderUserAgent))
	if err != nil {
		return s.failTOTPLogin(c, err)
	}

	s.clearChallengeCookie(c)
	s.setSessionCookie(c, res.Token)

	action := store.ActionLoginSuccess
	if res.Method == auth.MethodRecovery {
		action = store.ActionTOTPRecoveryUsed
	}
	s.audit(c, store.AuditParams{
		ActorID: res.User.ID, ActorName: res.User.Username, Action: action,
		Detail: map[string]any{"method": res.Method},
	})
	if res.Method == auth.MethodRecovery {
		// 성공 로그인도 함께 남긴다. 로그인 목록만 보는 사람이 이 접속을 놓치면 안 된다.
		s.audit(c, store.AuditParams{
			ActorID: res.User.ID, ActorName: res.User.Username, Action: store.ActionLoginSuccess,
			Detail: map[string]any{"method": res.Method},
		})
	}
	return c.JSON(fiber.Map{"user": res.User})
}

// failTOTPLogin은 2단계 실패를 응답으로 바꾸고 감사에 남긴다.
func (s *Server) failTOTPLogin(c *fiber.Ctx, err error) error {
	// 어떤 실패든 기록한다. 코드 실패가 반복되는 것은 계정을 노리는 신호이거나
	// 시계가 어긋났다는 신호이며, 둘 다 사람이 봐야 한다.
	code, status := "invalid_totp", fiber.StatusUnauthorized
	switch {
	case errors.Is(err, auth.ErrChallengeInvalid):
		code, status = "challenge_expired", fiber.StatusUnauthorized
		s.clearChallengeCookie(c)
	case errors.Is(err, auth.ErrTOTPLocked):
		code, status = "totp_locked", fiber.StatusTooManyRequests
	case errors.Is(err, auth.ErrTOTPResynced):
		// 실패이긴 하지만 사용자가 할 일이 분명하다: 다음 코드를 넣으면 된다.
		code, status = "totp_resynced", fiber.StatusUnauthorized
	case errors.Is(err, auth.ErrTOTPReused):
		code = "totp_reused"
	case errors.Is(err, auth.ErrAccountDisabled):
		code, status = "invalid_credentials", fiber.StatusUnauthorized
		s.clearChallengeCookie(c)
	case errors.Is(err, auth.ErrTOTPInvalid):
		// 기본값 그대로
	default:
		return err
	}

	action := store.ActionTOTPFailure
	if errors.Is(err, auth.ErrTOTPResynced) {
		action = store.ActionTOTPResync
	}
	s.audit(c, store.AuditParams{
		Action: action, Result: "denied",
		Detail: map[string]any{"reason": code},
	})
	return fail(c, status, code, err.Error())
}

// ---------- 등록 / 해제 ----------

func (s *Server) handleTOTPStatus(c *fiber.Ctx) error {
	st, err := s.authn.TOTPStatus(c.Context(), currentUser(c).ID)
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"totp": st})
}

// handleTOTPSetup은 새 공유 비밀을 만들어 QR과 함께 돌려준다.
//
// GET이 아니라 POST인 이유: 부르는 것만으로 서버 상태가 바뀐다(이전에 만들어 둔
// 미확인 비밀이 버려진다). 브라우저나 프록시가 미리 불러 볼 수 있는 자리에 두면
// 사용자가 QR을 읽는 동안 비밀이 갈릴 수 있다.
func (s *Server) handleTOTPSetup(c *fiber.Ctx) error {
	u := currentUser(c)
	enr, err := s.authn.BeginTOTPEnrollment(c.Context(), u)
	if errors.Is(err, store.ErrTOTPConfirmed) {
		return fail(c, fiber.StatusConflict, "totp_already_enabled",
			"이미 2단계 인증이 설정되어 있습니다. 다시 설정하려면 먼저 해제하세요")
	}
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"enrollment": enr})
}

type totpConfirmRequest struct {
	Code string `json:"code"`
}

func (s *Server) handleTOTPConfirm(c *fiber.Ctx) error {
	u := currentUser(c)
	var req totpConfirmRequest
	if err := c.BodyParser(&req); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "요청 형식이 올바르지 않습니다")
	}

	codes, err := s.authn.ConfirmTOTPEnrollment(c.Context(), u.ID, req.Code)
	switch {
	case errors.Is(err, auth.ErrTOTPNotEnrolled):
		return fail(c, fiber.StatusBadRequest, "totp_not_started",
			"등록이 시작되지 않았습니다. 화면을 새로 열어 다시 시도하세요")
	case errors.Is(err, store.ErrTOTPConfirmed):
		return fail(c, fiber.StatusConflict, "totp_already_enabled", "이미 2단계 인증이 설정되어 있습니다")
	case errors.Is(err, auth.ErrTOTPLocked):
		return fail(c, fiber.StatusTooManyRequests, "totp_locked", err.Error())
	case errors.Is(err, auth.ErrTOTPInvalid):
		s.audit(c, store.AuditParams{
			Action: store.ActionTOTPFailure, Result: "denied",
			TargetType: "user", TargetID: u.ID,
			Detail: map[string]any{"phase": "enroll"},
		})
		return fail(c, fiber.StatusBadRequest, "invalid_totp", err.Error())
	case err != nil:
		return err
	}

	s.audit(c, store.AuditParams{Action: store.ActionTOTPEnabled, TargetType: "user", TargetID: u.ID})

	updated, err := s.st.GetUser(c.Context(), u.ID)
	if err != nil {
		return err
	}
	// 복구 코드는 여기서 한 번만 나간다. 서버는 해시만 갖고 있다.
	return c.JSON(fiber.Map{"ok": true, "recoveryCodes": codes, "user": updated})
}

type totpDisableRequest struct {
	Password string `json:"password"`
}

func (s *Server) handleTOTPDisable(c *fiber.Ctx) error {
	u := currentUser(c)
	var req totpDisableRequest
	if err := c.BodyParser(&req); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "요청 형식이 올바르지 않습니다")
	}

	err := s.authn.DisableTOTP(c.Context(), u.ID, req.Password)
	switch {
	case errors.Is(err, auth.ErrTOTPEnforced):
		return fail(c, fiber.StatusForbidden, "totp_enforced", err.Error())
	case errors.Is(err, auth.ErrInvalidCredentials):
		return fail(c, fiber.StatusBadRequest, "invalid_credentials", "비밀번호가 올바르지 않습니다")
	case errors.Is(err, auth.ErrTOTPNotEnrolled):
		return fail(c, fiber.StatusBadRequest, "totp_not_enabled", err.Error())
	case err != nil:
		return err
	}

	s.audit(c, store.AuditParams{Action: store.ActionTOTPDisabled, TargetType: "user", TargetID: u.ID})
	updated, err := s.st.GetUser(c.Context(), u.ID)
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"ok": true, "user": updated})
}

type totpRecoveryRequest struct {
	Code string `json:"code"`
}

// handleTOTPRecoveryCodes는 복구 코드를 새로 발급한다. 옛 코드는 즉시 죽는다.
func (s *Server) handleTOTPRecoveryCodes(c *fiber.Ctx) error {
	u := currentUser(c)
	var req totpRecoveryRequest
	if err := c.BodyParser(&req); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "요청 형식이 올바르지 않습니다")
	}

	codes, err := s.authn.RegenerateRecoveryCodes(c.Context(), u.ID, req.Code)
	switch {
	case errors.Is(err, auth.ErrTOTPNotEnrolled):
		return fail(c, fiber.StatusBadRequest, "totp_not_enabled", err.Error())
	case errors.Is(err, auth.ErrTOTPLocked):
		return fail(c, fiber.StatusTooManyRequests, "totp_locked", err.Error())
	case errors.Is(err, auth.ErrTOTPResynced):
		return fail(c, fiber.StatusBadRequest, "totp_resynced", err.Error())
	case errors.Is(err, auth.ErrTOTPReused):
		return fail(c, fiber.StatusBadRequest, "totp_reused", err.Error())
	case errors.Is(err, auth.ErrTOTPInvalid):
		return fail(c, fiber.StatusBadRequest, "invalid_totp", err.Error())
	case err != nil:
		return err
	}

	s.audit(c, store.AuditParams{Action: store.ActionTOTPRecoveryReset, TargetType: "user", TargetID: u.ID})
	return c.JSON(fiber.Map{"ok": true, "recoveryCodes": codes})
}

// ---------- 슈퍼 어드민: 정책과 시계 ----------

func (s *Server) handleGetSecurity(c *fiber.Ctx) error {
	policy, err := s.st.SecurityPolicy(c.Context())
	if err != nil {
		return err
	}
	users, err := s.st.ListUsers(c.Context())
	if err != nil {
		return err
	}
	// 의무화를 켜기 전에 "몇 명이 아직 안 켰는가"를 보여준다. 그 숫자를 모른 채
	// 켜면 다음 날 아침 문의가 몰린다.
	missing := 0
	for _, u := range users {
		if !u.TOTPEnabled {
			missing++
		}
	}
	return c.JSON(fiber.Map{
		"policy": policy,
		"clock":  s.authn.Clock().Status(),
		"totp": fiber.Map{
			"enrolled":   len(users) - missing,
			"missing":    missing,
			"totalUsers": len(users),
		},
	})
}

type securityPolicyRequest struct {
	TOTPRequired *bool `json:"totpRequired"`
}

func (s *Server) handlePutSecurity(c *fiber.Ctx) error {
	var req securityPolicyRequest
	if err := c.BodyParser(&req); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "요청 형식이 올바르지 않습니다")
	}
	if req.TOTPRequired == nil {
		return fail(c, fiber.StatusBadRequest, "no_changes", "변경할 내용이 없습니다")
	}

	actor := currentUser(c)
	// 의무화를 켜는 사람이 스스로는 켜 두지 않았다면 막는다.
	//
	// 켜자마자 자기 자신이 등록 화면에 갇히기 때문이 아니다(그 화면은 열려 있다).
	// 이 설정을 되돌릴 수 있는 사람이 슈퍼 어드민뿐인데, 그 사람이 2FA를 켜는 데
	// 실패하면(휴대폰이 없다거나) 아무도 되돌릴 수 없는 상태가 만들어진다.
	if *req.TOTPRequired && !actor.TOTPEnabled {
		return fail(c, fiber.StatusBadRequest, "self_totp_required",
			"먼저 본인 계정에 2단계 인증을 설정한 뒤 의무화하세요")
	}

	if err := s.st.SetTOTPRequired(c.Context(), *req.TOTPRequired, actor.ID); err != nil {
		return err
	}
	s.audit(c, store.AuditParams{
		Action: store.ActionSecurityUpdated,
		Detail: map[string]any{"totpRequired": *req.TOTPRequired},
	})

	policy, err := s.st.SecurityPolicy(c.Context())
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"ok": true, "policy": policy})
}

// handleResetUserTOTP는 다른 사용자의 2단계 인증을 초기화한다.
//
// 인증 앱을 잃고 복구 코드도 없는 사람을 위한 마지막 경로다. 초기화하면 그 사람은
// 비밀번호만으로 로그인할 수 있게 되므로(의무화 상태면 즉시 재등록 화면으로 간다),
// 반드시 감사 로그에 남는다.
func (s *Server) handleResetUserTOTP(c *fiber.Ctx) error {
	id := c.Params("id")
	target, err := s.st.GetUser(c.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		return fail(c, fiber.StatusNotFound, "not_found", "사용자를 찾을 수 없습니다")
	}
	if err != nil {
		return err
	}

	err = s.authn.ResetTOTP(c.Context(), id)
	if errors.Is(err, auth.ErrTOTPNotEnrolled) {
		return fail(c, fiber.StatusBadRequest, "totp_not_enabled",
			"이 사용자는 2단계 인증을 설정하지 않았습니다")
	}
	if err != nil {
		return err
	}

	s.audit(c, store.AuditParams{
		Action: store.ActionTOTPReset, TargetType: "user", TargetID: id,
		Detail: map[string]any{"username": target.Username},
	})
	return c.JSON(fiber.Map{"ok": true})
}
