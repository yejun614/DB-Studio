package api

import (
	"errors"
	"slices"
	"strings"

	"github.com/gofiber/fiber/v2"

	"dbstudio/internal/auth"
	"dbstudio/internal/crypto"
	"dbstudio/internal/model"
	"dbstudio/internal/store"
)

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (s *Server) handleLogin(c *fiber.Ctx) error {
	var req loginRequest
	if err := c.BodyParser(&req); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "요청 형식이 올바르지 않습니다")
	}
	req.Username = strings.TrimSpace(req.Username)
	if req.Username == "" || req.Password == "" {
		return fail(c, fiber.StatusBadRequest, "bad_request", "아이디와 비밀번호를 입력하세요")
	}

	res, err := s.authn.Login(c.Context(), req.Username, req.Password, clientIP(c), c.Get(fiber.HeaderUserAgent))
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrInvalidCredentials), errors.Is(err, auth.ErrAccountDisabled):
			s.audit(c, store.AuditParams{
				ActorName: req.Username,
				Action:    store.ActionLoginFailure,
				Result:    "denied",
				Detail:    map[string]any{"reason": err.Error()},
			})
			// 계정 비활성 여부는 알려주되, 존재하지 않는 계정과 잘못된 비밀번호는 구분하지 않는다.
			return fail(c, fiber.StatusUnauthorized, "invalid_credentials", err.Error())
		default:
			return err
		}
	}

	// 2단계 인증을 켠 계정은 여기서 끝나지 않는다. 세션 대신 챌린지 쿠키를 주고
	// 코드를 기다린다. 사용자 정보는 아직 돌려주지 않는다 — 이 시점에 사람이
	// 확인된 것이 아니므로, 이름이나 권한을 알려 줄 이유가 없다.
	if res.NeedsTOTP() {
		s.setChallengeCookie(c, res.Challenge)
		s.audit(c, store.AuditParams{
			ActorID:   res.User.ID,
			ActorName: res.User.Username,
			Action:    store.ActionLoginSuccess,
			Result:    "pending",
			Detail:    map[string]any{"stage": "password", "twoFactor": true},
		})
		return c.JSON(fiber.Map{
			"twoFactor": fiber.Map{
				"required":  true,
				"expiresIn": int(auth.ChallengeTTL.Seconds()),
			},
		})
	}

	s.setSessionCookie(c, res.Token)
	s.audit(c, store.AuditParams{
		ActorID:   res.User.ID,
		ActorName: res.User.Username,
		Action:    store.ActionLoginSuccess,
	})
	return c.JSON(fiber.Map{"user": res.User})
}

func (s *Server) handleLogout(c *fiber.Ctx) error {
	token := c.Cookies(auth.SessionCookieName)
	// 로그아웃은 인증 미들웨어를 거치지 않으므로 사용자 정보를 직접 조회해 감사에 남긴다.
	if u, _, err := s.authn.Authenticate(c.Context(), token); err == nil {
		s.audit(c, store.AuditParams{ActorID: u.ID, ActorName: u.Username, Action: store.ActionLogout})
	}
	if err := s.authn.Logout(c.Context(), token); err != nil {
		return err
	}
	s.clearSessionCookie(c)
	return c.JSON(fiber.Map{"ok": true})
}

func (s *Server) handleMe(c *fiber.Ctx) error {
	u := currentUser(c)
	sess := currentSession(c)

	// 권한 UI를 그리기 위해 자신의 정책 요약을 함께 반환한다.
	policy, err := s.st.GetAccessPolicy(c.Context(), u.ID)
	if err != nil {
		return err
	}
	security, err := s.st.SecurityPolicy(c.Context())
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{
		"user": u,
		"session": fiber.Map{
			"expiresAt": sess.ExpiresAt,
			"createdAt": sess.CreatedAt,
		},
		"permissions": fiber.Map{
			"manageUsers":       u.Role.CanManageUsers(),
			"manageConnections": u.Role.CanManageConnections(),
			"accessMode":        policy.Mode,
			"defaultLevel":      policy.DefaultLevel,
			// 메뉴를 그릴지 판단하는 데 쓴다. 실제 판정은 서버가 매 요청마다 다시 하므로
			// 이 값은 편의일 뿐이고, 조작해도 권한이 늘어나지 않는다.
			"macro":        u.HasPerm(model.PermMacro),
			"scriptRun":    u.HasPerm(model.PermScriptRun),
			"shellEnabled": s.cfg.AllowShell,
			// totpRequired는 화면이 "해제" 버튼을 감추고 등록을 안내하는 데 쓴다.
			// 실제 강제는 서버 미들웨어가 하므로, 이 값을 고쳐도 권한이 늘지 않는다.
			"totpRequired": security.TOTPRequired,
			// anyData는 데이터 메뉴를 보여줄지 결정한다. 커넥션마다 다르므로
			// "하나라도 있으면"으로 판단한다.
			"anyData": hasAnyCap(u, policy, model.CapDataRead) || hasAnyCap(u, policy, model.CapSQLRun),
		},
	})
}

// hasAnyCap은 이 사용자가 어느 커넥션에서든 그 능력을 가질 수 있는지 본다.
//
// 정확한 답은 커넥션 목록을 순회해야 나오지만, 이 값의 용도는 메뉴를 그릴지
// 정하는 것뿐이다. 넉넉하게 판단해서 메뉴가 보이면 그 화면이 "권한 있는 커넥션
// 없음"을 설명하고, 반대로 빡빡하게 판단해 메뉴를 숨기면 사용자는 기능이 없다고
// 생각한다. 후자가 더 나쁘다.
func hasAnyCap(u *model.User, p *model.AccessPolicy, cap model.Capability) bool {
	if u.Role == model.RoleSuperadmin {
		return true
	}
	// allowlist에서 목록이 비어 있으면 기본 능력이 적용될 커넥션 자체가 없다.
	hasScope := p.Mode != model.AccessAllowlist || len(p.Items) > 0
	if hasScope && slices.Contains(p.DefaultCaps, cap) {
		return true
	}
	for _, caps := range p.CapOverrides {
		if slices.Contains(caps, cap) {
			return true
		}
	}
	return false
}

type changePasswordRequest struct {
	CurrentPassword string `json:"currentPassword"`
	NewPassword     string `json:"newPassword"`
}

func (s *Server) handleChangeOwnPassword(c *fiber.Ctx) error {
	u := currentUser(c)
	var req changePasswordRequest
	if err := c.BodyParser(&req); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "요청 형식이 올바르지 않습니다")
	}

	err := s.authn.ChangeOwnPassword(c.Context(), u.ID, req.CurrentPassword, req.NewPassword)
	switch {
	case errors.Is(err, auth.ErrInvalidCredentials):
		return fail(c, fiber.StatusBadRequest, "invalid_credentials", "현재 비밀번호가 올바르지 않습니다")
	case err != nil:
		var pol *crypto.PasswordPolicyError
		if errors.As(err, &pol) {
			return fail(c, fiber.StatusBadRequest, "password_policy", pol.Reason)
		}
		return err
	}

	// ChangeOwnPassword가 모든 세션을 폐기했으므로 새 세션을 발급해 로그인 상태를 유지한다.
	token, err := s.authn.IssueSession(c.Context(), u.ID, clientIP(c), c.Get(fiber.HeaderUserAgent))
	if err != nil {
		return err
	}
	s.setSessionCookie(c, token)
	s.audit(c, store.AuditParams{Action: store.ActionPasswordChanged, TargetType: "user", TargetID: u.ID})

	updated, err := s.st.GetUser(c.Context(), u.ID)
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"ok": true, "user": updated})
}

// requireLevel은 커넥션 단위 권한을 확인하고 판정 결과를 반환한다.
// 커넥션 관련 핸들러는 반드시 이 함수를 통해 권한을 확인해야 한다.
func (s *Server) requireLevel(c *fiber.Ctx, connectionID string, need model.Level) (auth.Decision, error) {
	u := currentUser(c)
	d, err := s.authz.Can(c.Context(), u, connectionID, need)
	if err != nil {
		return d, err
	}
	if !d.Allowed {
		s.audit(c, store.AuditParams{
			Action:     "connection.access.denied",
			TargetType: "connection",
			TargetID:   connectionID,
			Result:     "denied",
			Detail:     map[string]any{"need": need, "reason": d.Reason},
		})
	}
	return d, nil
}

// requireCap은 커넥션 단위 데이터 능력을 확인한다. requireLevel과 짝을 이룬다.
func (s *Server) requireCap(c *fiber.Ctx, connectionID string, need model.Capability) (auth.Decision, error) {
	u := currentUser(c)
	d, err := s.authz.CanCap(c.Context(), u, connectionID, need)
	if err != nil {
		return d, err
	}
	if !d.Allowed {
		s.audit(c, store.AuditParams{
			Action:     "connection.data.denied",
			TargetType: "connection",
			TargetID:   connectionID,
			Result:     "denied",
			Detail:     map[string]any{"need": need, "reason": d.Reason},
		})
	}
	return d, nil
}

// requirePerm은 전역 권한을 요구하는 미들웨어를 만든다.
func (s *Server) requirePerm(p model.Perm) fiber.Handler {
	return func(c *fiber.Ctx) error {
		u := currentUser(c)
		if u == nil {
			return fail(c, fiber.StatusUnauthorized, "unauthorized", "로그인이 필요합니다")
		}
		if !u.HasPerm(p) {
			s.auditDenied(c, "perm.denied", string(p))
			return fail(c, fiber.StatusForbidden, "forbidden", p.Label()+" 권한이 없습니다")
		}
		return c.Next()
	}
}
