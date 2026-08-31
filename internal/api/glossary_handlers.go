package api

import (
	"errors"
	"strings"

	"github.com/gofiber/fiber/v2"

	"dbstudio/internal/store"
)

// 용어 사전.
//
// 프로젝트마다 하나씩 있다. 사전은 팀의 약속이고 팀이 다르면 약속도 다르다 —
// "주문"이 한쪽에서는 결제 전 장바구니이고 다른 쪽에서는 배송 지시서인 일은 실제로
// 있다. 앱 하나에 사전이 하나뿐이면 그 둘 중 하나는 사전에 적지 못한 채로 쓰게 된다.
//
// 읽기도 쓰기도 **참여자 누구나** 한다. 사전에 말을 올리는 사람은 설계하는 사람이고,
// 그때마다 관리자를 찾아야 하면 사전은 쓰이지 않는다 — 그러면 사전 밖의 약속이
// 생기고, 그것이 사전이 있는 것보다 나쁘다.
//
// 지우기만 좁다: 만든 사람과 커넥션 관리자다. 되돌릴 수 없는 동작과 함께 하는 동작을
// 같은 문턱에 두면, 함께 쓰게 열어 준 대가로 사고가 따라온다. 이것은 팀의 약속이라 아무나 바꾸면
// 약속이 아니게 되지만, 아무나 볼 수 없으면 지킬 수도 없다 — 설계하는 사람이
// 찾아보는 것이 이 표의 유일한 쓸모다.

const maxTermLen = 80

type glossaryRequest struct {
	ProjectID string `json:"projectId"`
	Term      string `json:"term"`
	Physical  string `json:"physical"`
	Note      string `json:"note"`
	Cat1      string `json:"cat1"`
	Cat2      string `json:"cat2"`
	Cat3      string `json:"cat3"`
}

func (s *Server) handleListGlossary(c *fiber.Ctx) error {
	project, perr := s.requireProject(c, c.Query("project"))
	if perr != nil {
		return perr
	}
	terms, err := s.st.ListGlossary(c.Context(), project.ID, c.Query("q"), c.QueryInt("limit", 500))
	if err != nil {
		return err
	}
	// 분류 목록은 검색어와 무관하게 사전 전체에서 모은다. 새 용어를 넣는 사람은
	// "이미 쓰이던 분류"를 골라야 하는데, 그것이 지금 화면에 걸린 검색 결과에만
	// 있으라는 법이 없다.
	cats, err := s.st.GlossaryCategories(c.Context(), project.ID)
	if err != nil {
		return err
	}
	u := currentUser(c)
	return c.JSON(fiber.Map{
		"project":    project,
		"terms":      terms,
		"categories": cats,
		// 화면이 단추를 그릴지 판단할 수 있어야 한다. 눌러 보고서야 거부되는 단추는
		// "왜 안 되지"를 남기고, 그 답은 화면 어디에도 없다.
		//
		// 둘로 나눈 이유: 올리고 고치는 것은 참여자 누구나(여기까지 왔다면 참여자다),
		// 남이 올린 말을 지우는 것은 관리자다. 하나로 두면 그 차이를 화면이 그릴 수
		// 없어서, 참여자에게 지우기 단추까지 보이거나 올리기 단추까지 사라진다.
		"canWrite":  true,
		"canManage": u.Role.CanManageConnections(),
		// 내 것인지 판단할 기준. 자기가 올린 말은 자기가 지울 수 있다.
		"userId": u.ID,
	})
}

func (s *Server) handleCreateGlossaryTerm(c *fiber.Ctx) error {
	p, ok := glossaryParams(c)
	if !ok {
		// 거절 응답은 glossaryParams 가 이미 썼다.
		return nil
	}
	project, perr := s.requireProject(c, p.ProjectID)
	if perr != nil {
		return perr
	}
	p.ProjectID = project.ID
	u := currentUser(c)
	p.CreatedBy = u.ID

	term, cerr := s.st.CreateGlossaryTerm(c.Context(), p)
	if errors.Is(cerr, store.ErrDuplicateTerm) {
		return fail(c, fiber.StatusConflict, "duplicate_term",
			"이미 사전에 있는 용어입니다. 같은 말이 두 번 오르면 어느 쪽이 약속인지 알 수 없습니다")
	}
	if cerr != nil {
		return cerr
	}

	s.audit(c, store.AuditParams{
		Action: "glossary.create", TargetType: "glossary_term", TargetID: term.ID,
		Detail: map[string]any{"term": term.Term, "physical": term.Physical},
	})
	return c.Status(fiber.StatusCreated).JSON(fiber.Map{"term": term})
}

func (s *Server) handleUpdateGlossaryTerm(c *fiber.Ctx) error {
	id := c.Params("termId")
	before, err := s.st.GetGlossaryTerm(c.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		return fail(c, fiber.StatusNotFound, "not_found", "용어를 찾을 수 없습니다")
	}
	if err != nil {
		return err
	}
	// 고칠 수 있는 사람인지는 그 말이 든 프로젝트로 판단한다. 요청이 들고 온
	// 프로젝트가 아니라 — 그러면 남의 사전을 자기 프로젝트 이름으로 고칠 수 있다.
	if _, perr := s.requireProject(c, before.ProjectID); perr != nil {
		return perr
	}
	p, ok := glossaryParams(c)
	if !ok {
		return nil
	}
	// 프로젝트는 옮기지 않는다. 옮기면 그 말을 쓰던 쪽에서 소리 없이 사라진다.
	p.ProjectID = before.ProjectID

	term, uerr := s.st.UpdateGlossaryTerm(c.Context(), id, p)
	if errors.Is(uerr, store.ErrDuplicateTerm) {
		return fail(c, fiber.StatusConflict, "duplicate_term", "이미 사전에 있는 용어입니다")
	}
	if errors.Is(uerr, store.ErrNotFound) {
		return fail(c, fiber.StatusNotFound, "not_found", "용어를 찾을 수 없습니다")
	}
	if uerr != nil {
		return uerr
	}

	// 무엇이 무엇으로 바뀌었는지를 남긴다. 물리명이 바뀌면 그 뒤에 만들어지는
	// 이름이 달라지므로, "언제부터 규칙이 바뀌었는가"가 필요해진다.
	s.audit(c, store.AuditParams{
		Action: "glossary.update", TargetType: "glossary_term", TargetID: id,
		Detail: map[string]any{
			"term": term.Term, "physical": term.Physical,
			"fromTerm": before.Term, "fromPhysical": before.Physical,
		},
	})
	return c.JSON(fiber.Map{"term": term})
}

func (s *Server) handleDeleteGlossaryTerm(c *fiber.Ctx) error {
	id := c.Params("termId")
	before, err := s.st.GetGlossaryTerm(c.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		return fail(c, fiber.StatusNotFound, "not_found", "용어를 찾을 수 없습니다")
	}
	if err != nil {
		return err
	}
	if _, perr := s.requireProject(c, before.ProjectID); perr != nil {
		return perr
	}
	// 남이 올린 말은 관리자만 지운다.
	//
	// 올리고 고치는 것은 함께 하는 일이지만 지우는 것은 되돌릴 수 없다. 자기가 올린
	// 것을 거두는 길은 열어 둔다 — 잘못 올린 사람이 그것을 치울 수 없으면, 치워
	// 달라고 부탁하는 동안 틀린 약속이 사전에 남는다.
	u := currentUser(c)
	if before.CreatedBy != u.ID && !u.Role.CanManageConnections() {
		return fail(c, fiber.StatusForbidden, "forbidden",
			"남이 올린 용어는 관리자만 지울 수 있습니다. 뜻이 틀렸다면 고치거나, 관리자에게 알려 주세요")
	}
	if err := s.st.DeleteGlossaryTerm(c.Context(), id); err != nil {
		return err
	}
	s.audit(c, store.AuditParams{
		Action: "glossary.delete", TargetType: "glossary_term", TargetID: id,
		Detail: map[string]any{"term": before.Term, "physical": before.Physical},
	})
	return c.JSON(fiber.Map{"ok": true})
}

// handleBulkGlossary는 여러 줄을 한 번에 올린다.
//
// 사전을 처음 들이는 팀은 이미 어딘가에 목록을 갖고 있다(엑셀, 위키, 남의 표준).
// 그것을 한 줄씩 옮겨 적게 하면 사전은 시작되지 않는다. 붙여넣기 한 번으로 들어와야
// 그다음부터 사전이 사전 노릇을 한다.
//
// 이미 있는 말은 건너뛴다. 실패로 끊지 않는 이유: 목록의 절반이 이미 들어 있는 것이
// 보통이고, 그때 "3번째 줄이 중복입니다"로 멈추면 사람이 그 줄을 지우고 다시
// 붙여넣는 일을 반복하게 된다. 무엇을 건너뛰었는지는 결과로 돌려준다.
func (s *Server) handleBulkGlossary(c *fiber.Ctx) error {
	var body struct {
		ProjectID string `json:"projectId"`
		Text      string `json:"text"`
	}
	if err := c.BodyParser(&body); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "요청 본문을 해석할 수 없습니다")
	}
	project, perr := s.requireProject(c, body.ProjectID)
	if perr != nil {
		return perr
	}
	u := currentUser(c)

	added := []*store.GlossaryTerm{}
	skipped := []string{}
	invalid := []string{}
	for _, line := range strings.Split(body.Text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// 쉼표·탭 어느 쪽이든 받는다. 엑셀에서 복사하면 탭이고, 손으로 적으면
		// 쉼표다 — 둘 중 하나만 받으면 그 사실을 사람이 알아내야 한다.
		sep := ","
		if strings.Contains(line, "\t") {
			sep = "\t"
		}
		// 칸은 용어, 물리명, 설명, 대·중·소 여섯이다. 설명에 쉼표가 들어가면 그
		// 뒤가 분류로 잘리므로, 그런 목록은 탭 구분(엑셀 복사)으로 넣게 안내한다.
		parts := strings.SplitN(line, sep, 6)
		if len(parts) < 2 {
			invalid = append(invalid, line)
			continue
		}
		p := store.SaveGlossaryParams{
			ProjectID: project.ID,
			Term:      strings.TrimSpace(parts[0]),
			Physical:  strings.TrimSpace(parts[1]),
			CreatedBy: u.ID,
		}
		for i, into := range []*string{&p.Note, &p.Cat1, &p.Cat2, &p.Cat3} {
			if len(parts) > i+2 {
				*into = strings.TrimSpace(parts[i+2])
			}
		}
		if p.Term == "" || p.Physical == "" || !lengthsOK(p) {
			invalid = append(invalid, line)
			continue
		}
		term, err := s.st.CreateGlossaryTerm(c.Context(), p)
		if errors.Is(err, store.ErrDuplicateTerm) {
			skipped = append(skipped, p.Term)
			continue
		}
		if err != nil {
			return err
		}
		added = append(added, term)
	}

	s.audit(c, store.AuditParams{
		Action: "glossary.bulk", TargetType: "glossary_term", TargetID: "",
		Detail: map[string]any{
			"added": len(added), "skipped": len(skipped), "invalid": len(invalid),
		},
	})
	return c.JSON(fiber.Map{
		"added": added, "skipped": skipped, "invalid": invalid,
	})
}

// glossaryParams는 입력을 검사한다. 거절했으면 두 번째 값이 false다.
//
// error 대신 bool 을 돌려주는 이유: 이 파일의 fail() 은 응답을 쓰고 **nil** 을
// 돌려준다. 그것을 error 로 넘기면 부르는 쪽이 "성공"으로 읽고 빈 값으로 계속
// 진행한다 — 이 저장소에서 실제로 그렇게 500 이 난 적이 있다.
func glossaryParams(c *fiber.Ctx) (store.SaveGlossaryParams, bool) {
	var req glossaryRequest
	if err := c.BodyParser(&req); err != nil {
		fail(c, fiber.StatusBadRequest, "bad_request", "요청 본문을 해석할 수 없습니다")
		return store.SaveGlossaryParams{}, false
	}
	p := store.SaveGlossaryParams{
		ProjectID: strings.TrimSpace(req.ProjectID),
		Term:      strings.TrimSpace(req.Term),
		Physical:  strings.TrimSpace(req.Physical),
		Note:      strings.TrimSpace(req.Note),
		// 분류는 셋 다 비어 있을 수 있다. 처음부터 분류 체계를 세우고 시작하는
		// 팀은 없고, 필수로 만들면 아무 말이나 넣게 된다.
		Cat1: strings.TrimSpace(req.Cat1),
		Cat2: strings.TrimSpace(req.Cat2),
		Cat3: strings.TrimSpace(req.Cat3),
	}
	if p.Term == "" {
		fail(c, fiber.StatusBadRequest, "invalid_term", "용어를 입력하세요")
		return p, false
	}
	if p.Physical == "" {
		fail(c, fiber.StatusBadRequest, "invalid_physical",
			"물리명을 입력하세요. 그것이 없으면 사전이 답하지 못합니다")
		return p, false
	}
	if !lengthsOK(p) {
		fail(c, fiber.StatusBadRequest, "too_long", "용어·물리명·분류는 80자 이내입니다")
		return p, false
	}
	return p, true
}

// lengthsOK는 이름 칸의 길이를 함께 본다.
//
// 설명은 재지 않는다 — 그것은 문장이고, 길다고 해서 사전이 망가지지 않는다. 나머지는
// 이름이라, 화면의 한 칸에 들어가지 않으면 표가 읽히지 않는다.
func lengthsOK(p store.SaveGlossaryParams) bool {
	for _, v := range []string{p.Term, p.Physical, p.Cat1, p.Cat2, p.Cat3} {
		if len([]rune(v)) > maxTermLen {
			return false
		}
	}
	return true
}
