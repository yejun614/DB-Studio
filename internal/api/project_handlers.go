package api

import (
	"errors"
	"strings"

	"github.com/gofiber/fiber/v2"

	"dbstudio/internal/model"
	"dbstudio/internal/store"
)

// 프로젝트.
//
// 볼 수 있는 사람의 규칙은 하나다: **슈퍼 어드민은 전부, 나머지는 참여한 것만.**
// 어드민에게 예외를 두지 않는 이유는 권한 판정(auth.resolveWithPolicy)이 이미 그
// 규칙이기 때문이다. 화면과 판정이 다른 규칙을 쓰면 목록에는 보이는데 열면 거부되는
// 프로젝트가 생기고, 그 어긋남은 "권한 설정이 잘못됐나"로만 보인다.
//
// 고치는 것(만들기·이름·참여자·삭제)은 커넥션 관리자다. 프로젝트를 만드는 일은
// 곧 그 안에 DB를 등록하겠다는 뜻이고, 그것이 커넥션 관리자의 일이다.

const maxProjectNameLen = 60

func (s *Server) handleListProjects(c *fiber.Ctx) error {
	u := currentUser(c)
	scope := u.ID
	if u.Role == model.RoleSuperadmin {
		scope = "" // 전부
	}
	list, err := s.st.ListProjects(c.Context(), scope)
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{
		"projects": list,
		// 화면이 "프로젝트 만들기" 단추를 그릴지 정한다. 눌러 보고서야 거부되는
		// 단추는 "왜 안 되지"를 남기고, 그 답은 화면 어디에도 없다.
		"canManage": u.Role.CanManageConnections(),
	})
}

func (s *Server) handleGetProject(c *fiber.Ctx) error {
	p, err := s.requireProject(c, c.Params("projectId"))
	if err != nil {
		return err
	}
	members, err := s.st.ListProjectMembers(c.Context(), p.ID)
	if err != nil {
		return err
	}
	u := currentUser(c)
	return c.JSON(fiber.Map{
		"project": p, "members": members,
		"canManage": u.Role.CanManageConnections(),
		// 참여자 명단을 고치려면 사용자 목록을 볼 수 있어야 한다.
		"canManageMembers": u.Role.CanManageUsers(),
	})
}

func (s *Server) handleCreateProject(c *fiber.Ctx) error {
	name, note, ok := projectBody(c)
	if !ok {
		return nil
	}
	u := currentUser(c)
	p, err := s.st.CreateProject(c.Context(), store.SaveProjectParams{
		Name: name, Note: note, ActorID: u.ID,
	})
	if errors.Is(err, store.ErrDuplicateName) {
		return fail(c, fiber.StatusConflict, "duplicate", "같은 이름의 프로젝트가 이미 있습니다")
	}
	if err != nil {
		return err
	}
	s.audit(c, store.AuditParams{
		Action: "project.create", TargetType: "project", TargetID: p.ID,
		Detail: map[string]any{"name": p.Name},
	})
	return c.Status(fiber.StatusCreated).JSON(fiber.Map{"project": p})
}

func (s *Server) handleUpdateProject(c *fiber.Ctx) error {
	before, err := s.requireProject(c, c.Params("projectId"))
	if err != nil {
		return err
	}
	name, note, ok := projectBody(c)
	if !ok {
		return nil
	}
	p, uerr := s.st.UpdateProject(c.Context(), before.ID, store.SaveProjectParams{Name: name, Note: note})
	if errors.Is(uerr, store.ErrDuplicateName) {
		return fail(c, fiber.StatusConflict, "duplicate", "같은 이름의 프로젝트가 이미 있습니다")
	}
	if uerr != nil {
		return uerr
	}
	s.audit(c, store.AuditParams{
		Action: "project.update", TargetType: "project", TargetID: p.ID,
		Detail: map[string]any{"name": p.Name, "fromName": before.Name},
	})
	return c.JSON(fiber.Map{"project": p})
}

// handleDeleteProject는 빈 프로젝트만 지운다.
//
// 안에 든 것을 함께 지우지 않는 이유: 커넥션 하나를 지우는 것도 무엇이 함께
// 사라지는지 세어 보여 준 뒤에 하는 일이다. 프로젝트 삭제 단추 하나로 DB 열 개와
// 그 아래 ERD·마이그레이션·버전이 한꺼번에 사라진다면, 그것은 무엇을 지우는지 말할
// 수 없는 단추다.
func (s *Server) handleDeleteProject(c *fiber.Ctx) error {
	p, err := s.requireProject(c, c.Params("projectId"))
	if err != nil {
		return err
	}
	derr := s.st.DeleteProject(c.Context(), p.ID)
	if errors.Is(derr, store.ErrProjectInUse) {
		return fail(c, fiber.StatusConflict, "project_in_use",
			"안에 든 것을 먼저 비우세요. 프로젝트를 지운다고 그 안의 DB와 설계가 함께 사라지지는 않습니다")
	}
	if errors.Is(derr, store.ErrNotFound) {
		return fail(c, fiber.StatusNotFound, "not_found", "프로젝트를 찾을 수 없습니다")
	}
	if derr != nil {
		return derr
	}
	s.audit(c, store.AuditParams{
		Action: "project.delete", TargetType: "project", TargetID: p.ID,
		Detail: map[string]any{"name": p.Name},
	})
	return c.JSON(fiber.Map{"ok": true})
}

// handleSetProjectMembers는 참여자 명단을 통째로 갈아 끼운다.
func (s *Server) handleSetProjectMembers(c *fiber.Ctx) error {
	p, err := s.requireProject(c, c.Params("projectId"))
	if err != nil {
		return err
	}
	var body struct {
		Members []string `json:"members"`
	}
	if err := c.BodyParser(&body); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "요청 본문을 해석할 수 없습니다")
	}
	if err := s.st.SetProjectMembers(c.Context(), p.ID, body.Members); err != nil {
		return err
	}
	members, err := s.st.ListProjectMembers(c.Context(), p.ID)
	if err != nil {
		return err
	}
	s.audit(c, store.AuditParams{
		Action: "project.members", TargetType: "project", TargetID: p.ID,
		Detail: map[string]any{"name": p.Name, "count": len(members)},
	})
	return c.JSON(fiber.Map{"members": members})
}

// projectBody는 이름과 설명을 검사한다. 거절했으면 세 번째 값이 false다.
//
// error 대신 bool을 돌려주는 이유: 이 패키지의 fail()은 응답을 쓰고 nil을 돌려준다.
// 그것을 error로 넘기면 부르는 쪽이 "성공"으로 읽는다.
func projectBody(c *fiber.Ctx) (string, string, bool) {
	var body struct {
		Name string `json:"name"`
		Note string `json:"note"`
	}
	if err := c.BodyParser(&body); err != nil {
		fail(c, fiber.StatusBadRequest, "bad_request", "요청 본문을 해석할 수 없습니다")
		return "", "", false
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		fail(c, fiber.StatusBadRequest, "invalid_name", "프로젝트 이름을 입력하세요")
		return "", "", false
	}
	if len([]rune(name)) > maxProjectNameLen {
		fail(c, fiber.StatusBadRequest, "invalid_name", "프로젝트 이름은 60자 이내입니다")
		return "", "", false
	}
	return name, strings.TrimSpace(body.Note), true
}

// requireProject는 프로젝트를 읽고 볼 수 있는 사람인지 확인한다.
//
// "없음"과 "볼 수 없음"에 같은 404를 주는 이유: 프로젝트 이름 자체가 남에게 알려서는
// 안 되는 것일 수 있다("이직-검토"). 403은 그 자리에 무언가 있다는 사실을 알려 준다.
func (s *Server) requireProject(c *fiber.Ctx, id string) (*store.Project, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, fiber.NewError(fiber.StatusBadRequest, "프로젝트를 고르세요")
	}
	p, err := s.st.GetProject(c.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, fiber.NewError(fiber.StatusNotFound, "프로젝트를 찾을 수 없습니다")
	}
	if err != nil {
		return nil, err
	}
	ok, err := s.canSeeProject(c, id)
	if err != nil {
		return nil, err
	}
	if !ok {
		s.auditDenied(c, "project.denied", id)
		return nil, fiber.NewError(fiber.StatusNotFound, "프로젝트를 찾을 수 없습니다")
	}
	return p, nil
}

// canSeeProject는 프로젝트 참여 여부를 본다(슈퍼 어드민은 언제나 참).
func (s *Server) canSeeProject(c *fiber.Ctx, projectID string) (bool, error) {
	u := currentUser(c)
	if u == nil {
		return false, nil
	}
	if u.Role == model.RoleSuperadmin {
		return true, nil
	}
	return s.st.IsProjectMember(c.Context(), projectID, u.ID)
}

// visibleProjectIDs는 이 사람이 볼 수 있는 프로젝트 아이디를 준다.
//
// 슈퍼 어드민에게는 nil을 준다 — "제한 없음"이다. 빈 슬라이스는 "하나도 없다"라서
// 뜻이 정반대다. 부르는 쪽이 그 둘을 반드시 구분해야 한다.
func (s *Server) visibleProjectIDs(c *fiber.Ctx) ([]string, error) {
	u := currentUser(c)
	if u == nil {
		return []string{}, nil
	}
	if u.Role == model.RoleSuperadmin {
		return nil, nil
	}
	ids, err := s.st.ProjectIDsForUser(c.Context(), u.ID)
	if err != nil {
		return nil, err
	}
	if ids == nil {
		ids = []string{}
	}
	return ids, nil
}

// projectFilter는 목록 요청의 ?project= 를 읽어 무엇으로 좁힐지 정한다.
//
// 값이 있으면 그 프로젝트 하나로(볼 수 없는 곳이면 빈 목록), 없으면 볼 수 있는
// 전체로 좁힌다. nil은 제한 없음이다.
func (s *Server) projectFilter(c *fiber.Ctx) ([]string, error) {
	want := strings.TrimSpace(c.Query("project"))
	if want == "" {
		return s.visibleProjectIDs(c)
	}
	ok, err := s.canSeeProject(c, want)
	if err != nil {
		return nil, err
	}
	if !ok {
		return []string{}, nil
	}
	return []string{want}, nil
}

// inProjects는 좁힘 목록에 드는지 본다. nil은 제한 없음이다.
func inProjects(ids []string, projectID string) bool {
	if ids == nil {
		return true
	}
	for _, id := range ids {
		if id == projectID {
			return true
		}
	}
	return false
}
