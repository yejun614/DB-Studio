package api

import (
	"errors"
	"strings"

	"github.com/gofiber/fiber/v2"

	"dbstudio/internal/model"
	"dbstudio/internal/store"
)

// 매크로 공유 API — 공개 범위와 협업자.
//
// 매크로 본문 API(macro_handlers.go)와 파일을 나눈 이유: 공유는 "무엇을 하는
// 매크로인가"와 아무 상관이 없는 별개의 관심사이고, 커스텀 노드에도 같은 것이
// 그대로 한 벌 더 필요하다. 두 대상이 같은 요청 형식과 같은 검증을 쓴다.
//
// 권한은 모두 manage 이상이다. 공유 설정을 바꾸는 것은 매크로를 고치는 것보다
// 무겁다 — 공개+수정으로 열린 매크로에서 아무나 협업자를 추가할 수 있으면
// 소유자만 할 수 있어야 할 일이 사실상 전원에게 열린다.

type visibilityRequest struct {
	Visibility   string `json:"visibility"`
	PublicAccess string `json:"publicAccess"`
}

// parse는 요청을 검증한다.
//
// 비공개일 때도 publicAccess를 받아 저장한다. 공개 → 비공개 → 다시 공개를 오갈 때
// 직전 선택이 남아 있어야 매번 다시 고르지 않는다.
func (r visibilityRequest) parse() (model.MacroVisibility, model.MacroPublicAccess, string) {
	vis := model.MacroVisibility(strings.TrimSpace(r.Visibility))
	if !vis.Valid() {
		return "", "", "공개 범위는 private 또는 public 이어야 합니다"
	}
	pub := model.MacroPublicAccess(strings.TrimSpace(r.PublicAccess))
	if pub == "" {
		pub = model.MacroPublicView
	}
	if !pub.Valid() {
		return "", "", "공개 권한은 view 또는 edit 이어야 합니다"
	}
	return vis, pub, ""
}

// ---------- 매크로 ----------

func (s *Server) handleUpdateMacroAccess(c *fiber.Ctx) error {
	m, err := s.requireMacro(c, c.Params("id"), model.MacroAccessManage)
	if err != nil {
		return err
	}
	var req visibilityRequest
	if err := c.BodyParser(&req); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "요청 형식이 올바르지 않습니다")
	}
	vis, pub, msg := req.parse()
	if msg != "" {
		return fail(c, fiber.StatusBadRequest, "invalid_visibility", msg)
	}
	if err := s.st.SetMacroVisibility(c.Context(), m.ID, vis, pub); err != nil {
		return err
	}

	// 공개 설정 변경은 반드시 감사 로그로 남긴다. 매크로 하나가 전원에게 열리는
	// 순간이고, 나중에 "언제부터 열려 있었는가"를 답할 수 있어야 한다.
	s.audit(c, store.AuditParams{
		Action: "macro.access.changed", TargetType: "macro", TargetID: m.ID,
		Detail: map[string]any{
			"name": m.Name,
			"from": map[string]any{"visibility": m.Visibility, "publicAccess": m.PublicAccess},
			"to":   map[string]any{"visibility": vis, "publicAccess": pub},
		},
	})
	updated, err := s.st.GetMacro(c.Context(), m.ID, viewer(c))
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"macro": updated})
}

func (s *Server) handleListMacroCollaborators(c *fiber.Ctx) error {
	// 조회 권한이면 볼 수 있다. 누구와 함께 쓰는 매크로인지는 그것을 쓰는 사람이
	// 알아야 하는 정보다 — 문제가 생겼을 때 누구에게 물어볼지가 여기 있다.
	m, err := s.requireMacro(c, c.Params("id"), model.MacroAccessView)
	if err != nil {
		return err
	}
	list, err := s.st.ListMacroCollaborators(c.Context(), m.ID)
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"items": list})
}

type collaboratorRequest struct {
	UserID string `json:"userId"`
}

func (s *Server) handleAddMacroCollaborator(c *fiber.Ctx) error {
	u := currentUser(c)
	m, err := s.requireMacro(c, c.Params("id"), model.MacroAccessManage)
	if err != nil {
		return err
	}
	var req collaboratorRequest
	if err := c.BodyParser(&req); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "요청 형식이 올바르지 않습니다")
	}
	person, msg := s.resolveCollaborator(c, req.UserID, m.CreatedBy)
	if msg != "" {
		return fail(c, fiber.StatusBadRequest, "invalid_user", msg)
	}
	if err := s.st.AddMacroCollaborator(c.Context(), m.ID, person.ID, u.ID, displayName(u)); err != nil {
		return err
	}
	s.audit(c, store.AuditParams{
		Action: "macro.collaborator.added", TargetType: "macro", TargetID: m.ID,
		Detail: map[string]any{"name": m.Name, "user": person.Username},
	})
	list, err := s.st.ListMacroCollaborators(c.Context(), m.ID)
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"items": list})
}

func (s *Server) handleRemoveMacroCollaborator(c *fiber.Ctx) error {
	m, err := s.requireMacro(c, c.Params("id"), model.MacroAccessManage)
	if err != nil {
		return err
	}
	userID := c.Params("userId")
	err = s.st.RemoveMacroCollaborator(c.Context(), m.ID, userID)
	if errors.Is(err, store.ErrNotFound) {
		return fail(c, fiber.StatusNotFound, "not_found", "협업자가 아닙니다")
	}
	if err != nil {
		return err
	}
	s.audit(c, store.AuditParams{
		Action: "macro.collaborator.removed", TargetType: "macro", TargetID: m.ID,
		Detail: map[string]any{"name": m.Name, "userId": userID},
	})
	list, err := s.st.ListMacroCollaborators(c.Context(), m.ID)
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"items": list})
}

// ---------- 전역 커스텀 노드 ----------
//
// 매크로 전용 노드에는 공유 설정이 없다(소속 매크로를 따른다). 아래 핸들러는
// 그런 요청을 400으로 돌려보낸다 — 조용히 무시하면 화면이 바뀐 줄 알고 넘어간다.

func (s *Server) requireGlobalNodeDef(c *fiber.Ctx) (*store.NodeDef, error) {
	def, err := s.requireNodeDef(c, c.Params("defId"), model.MacroAccessManage)
	if err != nil {
		return nil, err
	}
	if def.Scope != "global" {
		return nil, fiber.NewError(fiber.StatusBadRequest,
			"매크로 전용 노드는 소속 매크로의 공유 설정을 따릅니다")
	}
	return def, nil
}

func (s *Server) handleUpdateNodeDefAccess(c *fiber.Ctx) error {
	def, err := s.requireGlobalNodeDef(c)
	if err != nil {
		return err
	}
	var req visibilityRequest
	if err := c.BodyParser(&req); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "요청 형식이 올바르지 않습니다")
	}
	vis, pub, msg := req.parse()
	if msg != "" {
		return fail(c, fiber.StatusBadRequest, "invalid_visibility", msg)
	}
	if err := s.st.SetNodeDefVisibility(c.Context(), def.ID, vis, pub); err != nil {
		return err
	}
	s.audit(c, store.AuditParams{
		Action: "macro.node.access.changed", TargetType: "macro_node", TargetID: def.ID,
		Detail: map[string]any{
			"name": def.Name,
			"from": map[string]any{"visibility": def.Visibility, "publicAccess": def.PublicAccess},
			"to":   map[string]any{"visibility": vis, "publicAccess": pub},
		},
	})
	updated, err := s.st.GetNodeDef(c.Context(), def.ID, viewer(c))
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"nodeDef": updated})
}

func (s *Server) handleListNodeDefCollaborators(c *fiber.Ctx) error {
	def, err := s.requireNodeDef(c, c.Params("defId"), model.MacroAccessView)
	if err != nil {
		return err
	}
	if def.Scope != "global" {
		return c.JSON(fiber.Map{"items": []any{}})
	}
	list, err := s.st.ListNodeDefCollaborators(c.Context(), def.ID)
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"items": list})
}

func (s *Server) handleAddNodeDefCollaborator(c *fiber.Ctx) error {
	u := currentUser(c)
	def, err := s.requireGlobalNodeDef(c)
	if err != nil {
		return err
	}
	var req collaboratorRequest
	if err := c.BodyParser(&req); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "요청 형식이 올바르지 않습니다")
	}
	person, msg := s.resolveCollaborator(c, req.UserID, def.CreatedBy)
	if msg != "" {
		return fail(c, fiber.StatusBadRequest, "invalid_user", msg)
	}
	if err := s.st.AddNodeDefCollaborator(c.Context(), def.ID, person.ID, u.ID, displayName(u)); err != nil {
		return err
	}
	s.audit(c, store.AuditParams{
		Action: "macro.node.collaborator.added", TargetType: "macro_node", TargetID: def.ID,
		Detail: map[string]any{"name": def.Name, "user": person.Username},
	})
	list, err := s.st.ListNodeDefCollaborators(c.Context(), def.ID)
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"items": list})
}

func (s *Server) handleRemoveNodeDefCollaborator(c *fiber.Ctx) error {
	def, err := s.requireGlobalNodeDef(c)
	if err != nil {
		return err
	}
	userID := c.Params("userId")
	err = s.st.RemoveNodeDefCollaborator(c.Context(), def.ID, userID)
	if errors.Is(err, store.ErrNotFound) {
		return fail(c, fiber.StatusNotFound, "not_found", "협업자가 아닙니다")
	}
	if err != nil {
		return err
	}
	s.audit(c, store.AuditParams{
		Action: "macro.node.collaborator.removed", TargetType: "macro_node", TargetID: def.ID,
		Detail: map[string]any{"name": def.Name, "userId": userID},
	})
	list, err := s.st.ListNodeDefCollaborators(c.Context(), def.ID)
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"items": list})
}

// ---------- 공통 ----------

// resolveCollaborator는 초대 대상을 검증한다.
func (s *Server) resolveCollaborator(c *fiber.Ctx, userID, ownerID string) (*store.MacroPerson, string) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return nil, "초대할 사용자를 선택하세요"
	}
	// 작성자를 협업자로 넣으면 목록에 자기가 두 번 나오고, 협업자에서 빼는 것으로
	// 소유권이 사라지는 것처럼 보인다. 소유자는 협업자가 아니라 소유자다.
	if userID == ownerID {
		return nil, "만든 사람은 이미 모든 권한을 가지고 있습니다"
	}
	person, err := s.st.GetMacroPerson(c.Context(), userID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, "사용자를 찾을 수 없거나 비활성 계정입니다"
	}
	if err != nil {
		return nil, "사용자를 확인하지 못했습니다"
	}
	return person, ""
}

// handleListMacroPeople은 협업자로 초대할 수 있는 사람 목록이다.
//
// /users(슈퍼어드민 전용)를 대신하는 좁은 창구다. 이름만 나가고, 매크로 권한이
// 있는 활성 계정으로 제한된다 — 초대해도 아무것도 못 하는 사람을 고르게 두면
// 왜 안 되는지 알 방법이 없다.
func (s *Server) handleListMacroPeople(c *fiber.Ctx) error {
	people, err := s.st.ListMacroPeople(c.Context(), currentUser(c).ID)
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"items": people})
}
