package api

import (
	"errors"
	"regexp"
	"strconv"
	"strings"

	"github.com/gofiber/fiber/v2"

	"dbstudio/internal/crypto"
	"dbstudio/internal/model"
	"dbstudio/internal/store"
)

var usernamePattern = regexp.MustCompile(`^[a-zA-Z0-9._-]{3,32}$`)

func (s *Server) handleListUsers(c *fiber.Ctx) error {
	users, err := s.st.ListUsers(c.Context())
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"users": users})
}

func (s *Server) handleGetUser(c *fiber.Ctx) error {
	u, err := s.st.GetUser(c.Context(), c.Params("id"))
	if errors.Is(err, store.ErrNotFound) {
		return fail(c, fiber.StatusNotFound, "not_found", "사용자를 찾을 수 없습니다")
	}
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"user": u})
}

type createUserRequest struct {
	Username    string     `json:"username"`
	Email       string     `json:"email"`
	DisplayName string     `json:"displayName"`
	Role        model.Role `json:"role"`
	Password    string     `json:"password"` // 비어 있으면 임시 비밀번호를 생성해 반환한다
}

func (s *Server) handleCreateUser(c *fiber.Ctx) error {
	var req createUserRequest
	if err := c.BodyParser(&req); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "요청 형식이 올바르지 않습니다")
	}
	req.Username = strings.TrimSpace(req.Username)
	req.Email = strings.TrimSpace(req.Email)
	req.DisplayName = strings.TrimSpace(req.DisplayName)

	if !usernamePattern.MatchString(req.Username) {
		return fail(c, fiber.StatusBadRequest, "invalid_username",
			"아이디는 영문/숫자/._- 조합 3~32자여야 합니다")
	}
	if req.Role == "" {
		req.Role = model.RoleMember
	}
	if !req.Role.Valid() {
		return fail(c, fiber.StatusBadRequest, "invalid_role", "알 수 없는 역할입니다")
	}

	// 비밀번호를 지정하지 않으면 임시 비밀번호를 생성하고 첫 로그인 시 변경을 강제한다.
	password := req.Password
	generated := false
	if password == "" {
		pw, err := crypto.GeneratePassword(20)
		if err != nil {
			return err
		}
		password = pw
		generated = true
	} else if err := crypto.CheckPasswordPolicy(password); err != nil {
		var pol *crypto.PasswordPolicyError
		if errors.As(err, &pol) {
			return fail(c, fiber.StatusBadRequest, "password_policy", pol.Reason)
		}
		return err
	}

	hash, err := crypto.HashPassword(password)
	if err != nil {
		return err
	}
	actor := currentUser(c)
	u, err := s.st.CreateUser(c.Context(), store.CreateUserParams{
		Username:           req.Username,
		Email:              req.Email,
		DisplayName:        req.DisplayName,
		Role:               req.Role,
		PasswordHash:       hash,
		MustChangePassword: true, // 관리자가 만든 계정은 항상 첫 로그인 시 변경
		CreatedBy:          actor.ID,
	})
	if errors.Is(err, store.ErrDuplicateName) {
		return fail(c, fiber.StatusConflict, "duplicate", "이미 사용 중인 아이디입니다")
	}
	if err != nil {
		return err
	}

	s.audit(c, store.AuditParams{
		Action: store.ActionUserCreated, TargetType: "user", TargetID: u.ID,
		Detail: map[string]any{"username": u.Username, "role": u.Role},
	})

	resp := fiber.Map{"user": u}
	// 생성된 비밀번호는 이 응답에서 단 한 번만 노출된다.
	if generated {
		resp["temporaryPassword"] = password
	}
	return c.Status(fiber.StatusCreated).JSON(resp)
}

type updateUserRequest struct {
	Email       *string           `json:"email"`
	DisplayName *string           `json:"displayName"`
	Role        *model.Role       `json:"role"`
	Status      *model.UserStatus `json:"status"`
	// Perms는 전역 권한(매크로 사용, 셸 실행)이다.
	//
	// 화면에서는 커넥션별 권한과 함께 접근 권한 화면(PUT /users/:id/access)에서
	// 설정한다 — 권한을 두 곳에 나눠 두면 어느 쪽이 최신인지 알 수 없다.
	// 여기서도 계속 받는 이유는 계정 속성만 바꾸려는 API 호출을 막을 이유가 없어서다.
	Perms *[]model.Perm `json:"perms"`
}

func (s *Server) handleUpdateUser(c *fiber.Ctx) error {
	id := c.Params("id")
	actor := currentUser(c)

	var req updateUserRequest
	if err := c.BodyParser(&req); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "요청 형식이 올바르지 않습니다")
	}
	if req.Role != nil && !req.Role.Valid() {
		return fail(c, fiber.StatusBadRequest, "invalid_role", "알 수 없는 역할입니다")
	}
	if req.Status != nil && *req.Status != model.UserActive && *req.Status != model.UserDisabled {
		return fail(c, fiber.StatusBadRequest, "invalid_status", "알 수 없는 상태입니다")
	}
	if req.Perms != nil {
		for _, p := range *req.Perms {
			if !p.Valid() {
				return failDetail(c, fiber.StatusBadRequest, "invalid_perm", "알 수 없는 권한입니다", string(p))
			}
		}
	}

	target, err := s.st.GetUser(c.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		return fail(c, fiber.StatusNotFound, "not_found", "사용자를 찾을 수 없습니다")
	}
	if err != nil {
		return err
	}

	// 자기 자신의 역할 강등이나 비활성화는 막는다. 잠금 상태를 만들 수 있다.
	if target.ID == actor.ID {
		if req.Role != nil && *req.Role != actor.Role {
			return fail(c, fiber.StatusBadRequest, "self_demote", "자신의 역할은 변경할 수 없습니다")
		}
		if req.Status != nil && *req.Status == model.UserDisabled {
			return fail(c, fiber.StatusBadRequest, "self_disable", "자신의 계정은 비활성화할 수 없습니다")
		}
	}

	// 마지막 활성 슈퍼 어드민이 사라지는 변경을 차단한다.
	losingSuperadmin := target.Role == model.RoleSuperadmin &&
		((req.Role != nil && *req.Role != model.RoleSuperadmin) ||
			(req.Status != nil && *req.Status == model.UserDisabled))
	if losingSuperadmin {
		n, err := s.st.CountSuperadmins(c.Context(), true)
		if err != nil {
			return err
		}
		if n <= 1 {
			return fail(c, fiber.StatusBadRequest, "last_superadmin",
				"마지막 슈퍼 어드민입니다. 다른 슈퍼 어드민을 먼저 지정하세요")
		}
	}

	updated, err := s.st.UpdateUser(c.Context(), id, store.UpdateUserParams{
		Email:       req.Email,
		DisplayName: req.DisplayName,
		Role:        req.Role,
		Status:      req.Status,
		Perms:       req.Perms,
	})
	if errors.Is(err, store.ErrNotFound) {
		return fail(c, fiber.StatusNotFound, "not_found", "사용자를 찾을 수 없습니다")
	}
	if err != nil {
		return err
	}

	// 역할·상태·전역 권한이 바뀌면 기존 세션의 권한 캐시가 낡으므로 즉시 무효화한다.
	// (권한을 회수했는데 이미 열려 있는 화면에서 계속 쓸 수 있으면 회수가 아니다.)
	if req.Role != nil || req.Status != nil || req.Perms != nil {
		if err := s.st.DeleteUserSessions(c.Context(), id); err != nil {
			return err
		}
	}

	detail := map[string]any{"username": updated.Username}
	if req.Role != nil {
		detail["role"] = *req.Role
	}
	if req.Status != nil {
		detail["status"] = *req.Status
	}
	if req.Perms != nil {
		detail["perms"] = model.PermsToString(*req.Perms)
	}
	s.audit(c, store.AuditParams{
		Action: store.ActionUserUpdated, TargetType: "user", TargetID: id, Detail: detail,
	})
	return c.JSON(fiber.Map{"user": updated})
}

func (s *Server) handleDeleteUser(c *fiber.Ctx) error {
	id := c.Params("id")
	actor := currentUser(c)
	if id == actor.ID {
		return fail(c, fiber.StatusBadRequest, "self_delete", "자신의 계정은 삭제할 수 없습니다")
	}

	target, err := s.st.GetUser(c.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		return fail(c, fiber.StatusNotFound, "not_found", "사용자를 찾을 수 없습니다")
	}
	if err != nil {
		return err
	}
	if target.Role == model.RoleSuperadmin {
		n, err := s.st.CountSuperadmins(c.Context(), false)
		if err != nil {
			return err
		}
		if n <= 1 {
			return fail(c, fiber.StatusBadRequest, "last_superadmin",
				"마지막 슈퍼 어드민은 삭제할 수 없습니다")
		}
	}

	if err := s.st.DeleteUser(c.Context(), id); err != nil {
		return err
	}
	s.audit(c, store.AuditParams{
		Action: store.ActionUserDeleted, TargetType: "user", TargetID: id,
		Detail: map[string]any{"username": target.Username},
	})
	return c.JSON(fiber.Map{"ok": true})
}

type resetPasswordRequest struct {
	Password string `json:"password"` // 비어 있으면 임시 비밀번호를 생성한다
}

func (s *Server) handleResetPassword(c *fiber.Ctx) error {
	id := c.Params("id")
	var req resetPasswordRequest
	// 본문이 없어도 허용한다(임시 비밀번호 생성 케이스).
	_ = c.BodyParser(&req)

	if _, err := s.st.GetUser(c.Context(), id); errors.Is(err, store.ErrNotFound) {
		return fail(c, fiber.StatusNotFound, "not_found", "사용자를 찾을 수 없습니다")
	} else if err != nil {
		return err
	}

	password, err := s.authn.ResetPassword(c.Context(), id, req.Password)
	if err != nil {
		var pol *crypto.PasswordPolicyError
		if errors.As(err, &pol) {
			return fail(c, fiber.StatusBadRequest, "password_policy", pol.Reason)
		}
		return err
	}
	s.audit(c, store.AuditParams{
		Action: store.ActionUserPasswordSet, TargetType: "user", TargetID: id,
	})

	resp := fiber.Map{"ok": true}
	if req.Password == "" {
		resp["temporaryPassword"] = password
	}
	return c.JSON(resp)
}

func (s *Server) handleListAudit(c *fiber.Ctx) error {
	f := store.AuditFilter{
		ActorID:    c.Query("actorId"),
		Action:     c.Query("action"),
		TargetType: c.Query("targetType"),
		TargetID:   c.Query("targetId"),
	}
	if v := c.Query("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			f.Limit = n
		}
	}
	if v := c.Query("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			f.Offset = n
		}
	}
	entries, total, err := s.st.ListAudit(c.Context(), f)
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"entries": entries, "total": total})
}
