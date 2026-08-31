package api

import (
	"errors"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"

	"dbstudio/internal/store"
)

// API 토큰 관리.
//
// 토큰은 **자기 것만** 만들고 지운다. 남의 토큰을 발급할 수 있으면 그것은 곧
// 남의 권한으로 행동할 수단을 주는 것이고, 이 앱에는 그런 개념이 없다
// (슈퍼 어드민도 예외가 아니다 — 대신 사용자를 비활성화하면 그 토큰도 함께 죽는다).

const (
	maxTokenNameLen = 60
	// maxTokenTTL은 만료를 지정했을 때의 상한이다. 만료 없는 토큰도 허용하지만
	// 그것은 사용자가 명시적으로 고른 결과여야 한다.
	maxTokenTTLDays = 3650
)

type createTokenRequest struct {
	Name  string `json:"name"`
	Scope string `json:"scope"`
	// ExpiresInDays가 0이면 만료 없음이다.
	ExpiresInDays int `json:"expiresInDays"`
}

func (s *Server) handleListTokens(c *fiber.Ctx) error {
	u := currentUser(c)
	tokens, err := s.st.ListAPITokens(c.Context(), u.ID)
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{
		"tokens": tokens,
		// MCP 접속 주소를 함께 준다. 화면이 이것을 그대로 복사해 클라이언트 설정에
		// 넣을 수 있어야 한다 — 사용자가 경로를 외우게 할 이유가 없다.
		"mcpPath": "/mcp",
		// 같은 토큰으로 부르는 REST API의 뿌리. 같은 이유로 함께 준다.
		"apiPath": restBasePath,
	})
}

func (s *Server) handleCreateToken(c *fiber.Ctx) error {
	u := currentUser(c)

	var req createTokenRequest
	if err := c.BodyParser(&req); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "요청 형식이 올바르지 않습니다")
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return fail(c, fiber.StatusBadRequest, "invalid_name", "토큰 이름을 입력하세요")
	}
	if len([]rune(name)) > maxTokenNameLen {
		return fail(c, fiber.StatusBadRequest, "invalid_name", "토큰 이름은 60자 이내로 입력하세요")
	}
	scope := req.Scope
	if scope == "" {
		scope = store.TokenScopeRead
	}
	if !store.ValidTokenScope(scope) {
		return fail(c, fiber.StatusBadRequest, "invalid_scope", "알 수 없는 범위입니다")
	}
	if req.ExpiresInDays < 0 || req.ExpiresInDays > maxTokenTTLDays {
		return fail(c, fiber.StatusBadRequest, "invalid_expiry", "만료 일수가 올바르지 않습니다")
	}

	var expiresAt *time.Time
	if req.ExpiresInDays > 0 {
		t := time.Now().Add(time.Duration(req.ExpiresInDays) * 24 * time.Hour)
		expiresAt = &t
	}

	saved, raw, err := s.authn.IssueAPIToken(c.Context(), store.CreateTokenParams{
		UserID: u.ID, Name: name, Scope: scope, ExpiresAt: expiresAt,
	})
	if err != nil {
		return err
	}

	s.audit(c, store.AuditParams{
		Action: "token.created", TargetType: "api_token", TargetID: saved.ID,
		Detail: map[string]any{
			"name": saved.Name, "scope": saved.Scope, "prefix": saved.Prefix,
			"expiresAt": saved.ExpiresAt,
		},
	})

	// 원문은 여기서 한 번만 나간다. 저장하지 않으므로 다시 볼 수 없다.
	return c.Status(fiber.StatusCreated).JSON(fiber.Map{
		"token": saved,
		"value": raw,
		"note":  "이 값은 다시 표시되지 않습니다. 지금 복사해 두세요.",
	})
}

func (s *Server) handleRevokeToken(c *fiber.Ctx) error {
	u := currentUser(c)
	id := c.Params("tokenId")

	t, err := s.st.GetAPIToken(c.Context(), id)
	if errors.Is(err, store.ErrNotFound) || (err == nil && t.UserID != u.ID) {
		// 남의 토큰은 "없음"으로 답한다. 존재 여부를 알려주지 않는다.
		return fail(c, fiber.StatusNotFound, "not_found", "토큰을 찾을 수 없습니다")
	}
	if err != nil {
		return err
	}

	if err := s.st.RevokeAPIToken(c.Context(), id, u.ID); errors.Is(err, store.ErrNotFound) {
		return fail(c, fiber.StatusBadRequest, "already_revoked", "이미 폐기된 토큰입니다")
	} else if err != nil {
		return err
	}

	s.audit(c, store.AuditParams{
		Action: "token.revoked", TargetType: "api_token", TargetID: id,
		Detail: map[string]any{"name": t.Name, "scope": t.Scope, "prefix": t.Prefix},
	})
	return c.JSON(fiber.Map{"ok": true})
}

// handleRotateToken은 토큰의 **값만** 다시 발급한다.
//
// 값이 샜을 때 할 일은 대개 "이 토큰을 버리고 새로 만들기"가 아니라 "이 토큰의 값만
// 바꾸기"다. 클라이언트 설정에서 토큰을 가리키는 것은 이름이고, 새로 만들어 옮기면
// 설정을 고칠 곳이 늘어나며 그 사이 옛 토큰을 지우는 것을 잊는다.
//
// 이름·범위·만료는 그대로다. 만료가 얼마 남지 않았다면 재발급해도 그대로 얼마 남지
// 않은 채다 — 그때 필요한 것은 새 토큰이다.
func (s *Server) handleRotateToken(c *fiber.Ctx) error {
	u := currentUser(c)
	id := c.Params("tokenId")

	t, err := s.st.GetAPIToken(c.Context(), id)
	if errors.Is(err, store.ErrNotFound) || (err == nil && t.UserID != u.ID) {
		// 남의 토큰은 "없음"으로 답한다. 존재 여부를 알려주지 않는다.
		return fail(c, fiber.StatusNotFound, "not_found", "토큰을 찾을 수 없습니다")
	}
	if err != nil {
		return err
	}
	if t.RevokedAt != nil {
		return fail(c, fiber.StatusBadRequest, "revoked",
			"폐기된 토큰은 값을 다시 발급할 수 없습니다. 지우고 새로 만드세요")
	}

	raw, err := s.authn.RotateAPIToken(c.Context(), id, u.ID)
	if errors.Is(err, store.ErrNotFound) {
		return fail(c, fiber.StatusNotFound, "not_found", "토큰을 찾을 수 없습니다")
	}
	if err != nil {
		return err
	}

	// 옛 값이 언제 죽었는지가 사고 조사에서 필요한 사실이다. 접두사를 둘 다 남긴다.
	s.audit(c, store.AuditParams{
		Action: "token.rotated", TargetType: "api_token", TargetID: id,
		Detail: map[string]any{
			"name": t.Name, "scope": t.Scope, "oldPrefix": t.Prefix,
			"expiresAt": t.ExpiresAt,
		},
	})

	saved, err := s.st.GetAPIToken(c.Context(), id)
	if err != nil {
		return err
	}
	// 원문은 여기서 한 번만 나간다. 발급과 같은 모양으로 돌려주어 화면이 같은 자리를
	// 쓸 수 있게 한다.
	return c.JSON(fiber.Map{
		"token": saved,
		"value": raw,
		"note":  "이 값은 다시 표시되지 않습니다. 지금 복사해 두세요.",
	})
}

func (s *Server) handleDeleteToken(c *fiber.Ctx) error {
	u := currentUser(c)
	id := c.Params("tokenId")

	t, err := s.st.GetAPIToken(c.Context(), id)
	if errors.Is(err, store.ErrNotFound) || (err == nil && t.UserID != u.ID) {
		return fail(c, fiber.StatusNotFound, "not_found", "토큰을 찾을 수 없습니다")
	}
	if err != nil {
		return err
	}
	if err := s.st.DeleteAPIToken(c.Context(), id, u.ID); err != nil {
		return err
	}
	s.audit(c, store.AuditParams{
		Action: "token.deleted", TargetType: "api_token", TargetID: id,
		Detail: map[string]any{"name": t.Name, "prefix": t.Prefix},
	})
	return c.JSON(fiber.Map{"ok": true})
}
