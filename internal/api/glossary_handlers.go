package api

import (
	"errors"
	"strings"

	"github.com/gofiber/fiber/v2"

	"dbstudio/internal/store"
)

// 용어 사전.
//
// 읽기는 누구나, 고치기는 커넥션 관리자만이다. 이것은 팀의 약속이라 아무나 바꾸면
// 약속이 아니게 되지만, 아무나 볼 수 없으면 지킬 수도 없다 — 설계하는 사람이
// 찾아보는 것이 이 표의 유일한 쓸모다.

const maxTermLen = 80

type glossaryRequest struct {
	Term     string `json:"term"`
	Physical string `json:"physical"`
	Note     string `json:"note"`
}

func (s *Server) handleListGlossary(c *fiber.Ctx) error {
	terms, err := s.st.ListGlossary(c.Context(), c.Query("q"), c.QueryInt("limit", 500))
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{
		"terms": terms,
		// 고칠 수 있는 사람인지 화면이 알아야 한다. 눌러 보고서야 거부되는 버튼은
		// "왜 안 되지"를 남기고, 그 답은 화면 어디에도 없다.
		"canManage": currentUser(c).Role.CanManageConnections(),
	})
}

func (s *Server) handleCreateGlossaryTerm(c *fiber.Ctx) error {
	p, ok := glossaryParams(c)
	if !ok {
		// 거절 응답은 glossaryParams 가 이미 썼다.
		return nil
	}
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
	p, ok := glossaryParams(c)
	if !ok {
		return nil
	}

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
		Text string `json:"text"`
	}
	if err := c.BodyParser(&body); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "요청 본문을 해석할 수 없습니다")
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
		parts := strings.SplitN(line, sep, 3)
		if len(parts) < 2 {
			invalid = append(invalid, line)
			continue
		}
		p := store.SaveGlossaryParams{
			Term:      strings.TrimSpace(parts[0]),
			Physical:  strings.TrimSpace(parts[1]),
			CreatedBy: u.ID,
		}
		if len(parts) > 2 {
			p.Note = strings.TrimSpace(parts[2])
		}
		if p.Term == "" || p.Physical == "" ||
			len([]rune(p.Term)) > maxTermLen || len([]rune(p.Physical)) > maxTermLen {
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
		Term:     strings.TrimSpace(req.Term),
		Physical: strings.TrimSpace(req.Physical),
		Note:     strings.TrimSpace(req.Note),
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
	if len([]rune(p.Term)) > maxTermLen || len([]rune(p.Physical)) > maxTermLen {
		fail(c, fiber.StatusBadRequest, "too_long", "용어와 물리명은 80자 이내입니다")
		return p, false
	}
	return p, true
}
