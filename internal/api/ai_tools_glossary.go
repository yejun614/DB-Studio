package api

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"dbstudio/internal/store"
)

// 용어 사전 툴.
//
// 왜 필요한가: 용어 사전은 "이 팀에서 이 말은 이 물리명으로 쓴다"는 약속이다. ERD를
// 짜는 동안 그 약속이 정해지는데, 정작 사전에 적는 일은 화면을 따로 열어야 했다.
// 그래서 약속과 사전이 어긋나고, 어긋난 사전은 아무도 안 본다.
//
// 지우는 툴은 두지 않는다. ERD 초안에서와 같은 이유다 — 되돌리기가 닿지 않는
// 자리라 지우는 일은 사람이 화면에서 한다. 게다가 사전은 여러 사람이 함께 쓰는
// 것이고, 남이 올린 말을 지우는 것은 화면에서도 관리자만 할 수 있다.
//
// 승인을 거치지 않고 바로 쓰는 이유: 사전은 실제 데이터베이스가 아니고, 되돌리기가
// 한 줄 지우기로 끝난다. 서른 개의 말을 등록할 때마다 승인 창이 서른 번 뜨면
// 아무도 이 툴을 쓰지 않는다. 대신 무엇을 넣었는지 결과로 또렷이 돌려준다.

// glossaryProject는 이 사람이 쓸 수 있는 프로젝트를 정한다.
//
// 이름을 안 주면 볼 수 있는 프로젝트가 하나뿐일 때만 그것을 고른다. 여럿일 때
// 아무거나 고르면 남의 팀 사전에 말이 들어간다 — 사전은 팀의 약속이고, 팀이 다르면
// 약속도 다르다.
func (tc *toolContext) glossaryProject(name string) (*store.Project, error) {
	list, err := tc.srv.st.ListProjects(tc.ctx, tc.user.ID)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, fmt.Errorf("참여 중인 프로젝트가 없습니다")
	}
	needle := strings.ToLower(strings.TrimSpace(name))
	if needle == "" {
		if len(list) == 1 {
			return list[0], nil
		}
		names := make([]string, 0, len(list))
		for _, p := range list {
			names = append(names, p.Name)
		}
		return nil, fmt.Errorf(
			"어느 프로젝트의 사전인지 알려주세요. 쓸 수 있는 프로젝트: %s",
			strings.Join(names, ", "))
	}
	for _, p := range list {
		if strings.ToLower(p.Name) == needle || strings.ToLower(p.ID) == needle {
			return p, nil
		}
	}
	return nil, fmt.Errorf("프로젝트 %q 을(를) 찾을 수 없거나 참여하고 있지 않습니다", name)
}

type glossaryOut struct {
	ID       string `json:"id"`
	Term     string `json:"term"`
	Physical string `json:"physical"`
	Note     string `json:"note,omitempty"`
	Cat1     string `json:"cat1,omitempty"`
	Cat2     string `json:"cat2,omitempty"`
	Cat3     string `json:"cat3,omitempty"`
	Author   string `json:"author,omitempty"`
}

func glossaryItems(list []*store.GlossaryTerm) []glossaryOut {
	out := make([]glossaryOut, 0, len(list))
	for _, t := range list {
		out = append(out, glossaryOut{
			ID: t.ID, Term: t.Term, Physical: t.Physical, Note: t.Note,
			Cat1: t.Cat1, Cat2: t.Cat2, Cat3: t.Cat3, Author: t.CreatedName,
		})
	}
	return out
}

func toolSearchGlossary(tc *toolContext, args json.RawMessage) (string, error) {
	var in struct {
		Project string `json:"project"`
		Q       string `json:"q"`
		Limit   int    `json:"limit"`
	}
	if err := parseArgs(args, &in); err != nil {
		return "", err
	}
	project, err := tc.glossaryProject(in.Project)
	if err != nil {
		return "", err
	}
	limit := in.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	list, err := tc.srv.st.ListGlossary(tc.ctx, project.ID, in.Q, limit)
	if err != nil {
		return "", err
	}
	cats, err := tc.srv.st.GlossaryCategories(tc.ctx, project.ID)
	if err != nil {
		return "", err
	}
	// 분류 목록도 함께 준다. 새 말을 넣을 때 있는 분류를 쓰게 하려면, 무엇이 있는지
	// 먼저 보여야 한다 — 안 보이면 매번 새 분류를 만들어 사전이 흩어진다.
	seen := map[string]bool{}
	catNames := []string{}
	for _, c := range cats {
		for _, name := range c {
			if name == "" || seen[name] {
				continue
			}
			seen[name] = true
			catNames = append(catNames, name)
		}
	}
	slices.Sort(catNames)
	return asJSON(map[string]any{
		"project": project.Name, "count": len(list),
		"terms": glossaryItems(list), "categories": catNames,
	})
}

func toolAddGlossaryTerm(tc *toolContext, args json.RawMessage) (string, error) {
	var in struct {
		Project  string `json:"project"`
		Term     string `json:"term"`
		Physical string `json:"physical"`
		Note     string `json:"note"`
		Cat1     string `json:"cat1"`
		Cat2     string `json:"cat2"`
		Cat3     string `json:"cat3"`
	}
	if err := parseArgs(args, &in); err != nil {
		return "", err
	}
	if strings.TrimSpace(in.Term) == "" || strings.TrimSpace(in.Physical) == "" {
		return "", fmt.Errorf("용어와 물리명을 모두 적어주세요")
	}
	project, err := tc.glossaryProject(in.Project)
	if err != nil {
		return "", err
	}

	// 이미 있는 말인지 먼저 본다. 같은 말이 두 줄이면 사전이 답을 두 개 주고,
	// 그 순간 사전이 아니라 목록이 된다.
	existing, err := tc.srv.st.ListGlossary(tc.ctx, project.ID, in.Term, 20)
	if err != nil {
		return "", err
	}
	for _, t := range existing {
		if strings.EqualFold(t.Term, strings.TrimSpace(in.Term)) {
			return "", fmt.Errorf(
				"%q 은(는) 이미 사전에 있습니다(물리명 %s). 고치려면 update_glossary_term 을 쓰세요",
				t.Term, t.Physical)
		}
	}

	item, err := tc.srv.st.CreateGlossaryTerm(tc.ctx, store.SaveGlossaryParams{
		ProjectID: project.ID, Term: in.Term, Physical: in.Physical, Note: in.Note,
		Cat1: in.Cat1, Cat2: in.Cat2, Cat3: in.Cat3, CreatedBy: tc.user.ID,
	})
	if err != nil {
		return "", err
	}
	return asJSON(map[string]any{
		"ok": true, "project": project.Name, "added": glossaryItems([]*store.GlossaryTerm{item})[0],
	})
}

func toolUpdateGlossaryTerm(tc *toolContext, args json.RawMessage) (string, error) {
	var in struct {
		Project  string  `json:"project"`
		Term     string  `json:"term"`
		ID       string  `json:"id"`
		NewTerm  *string `json:"newTerm"`
		Physical *string `json:"physical"`
		Note     *string `json:"note"`
		Cat1     *string `json:"cat1"`
		Cat2     *string `json:"cat2"`
		Cat3     *string `json:"cat3"`
	}
	if err := parseArgs(args, &in); err != nil {
		return "", err
	}
	project, err := tc.glossaryProject(in.Project)
	if err != nil {
		return "", err
	}

	before, err := tc.findGlossaryTerm(project.ID, in.ID, in.Term)
	if err != nil {
		return "", err
	}

	// 보낸 것만 바꾼다. 안 보낸 칸을 빈 값으로 덮으면 남이 적어 둔 설명과 분류가
	// 이름 하나 고치는 김에 사라진다.
	next := store.SaveGlossaryParams{
		ProjectID: project.ID,
		Term:      before.Term, Physical: before.Physical, Note: before.Note,
		Cat1: before.Cat1, Cat2: before.Cat2, Cat3: before.Cat3,
	}
	if in.NewTerm != nil {
		next.Term = *in.NewTerm
	}
	if in.Physical != nil {
		next.Physical = *in.Physical
	}
	if in.Note != nil {
		next.Note = *in.Note
	}
	if in.Cat1 != nil {
		next.Cat1 = *in.Cat1
	}
	if in.Cat2 != nil {
		next.Cat2 = *in.Cat2
	}
	if in.Cat3 != nil {
		next.Cat3 = *in.Cat3
	}

	item, err := tc.srv.st.UpdateGlossaryTerm(tc.ctx, before.ID, next)
	if err != nil {
		return "", err
	}
	return asJSON(map[string]any{
		"ok": true, "project": project.Name,
		"updated": glossaryItems([]*store.GlossaryTerm{item})[0],
	})
}

// findGlossaryTerm은 아이디나 용어로 한 줄을 찾는다.
//
// 프로젝트를 함께 보는 이유: 아이디만으로 찾으면 참여하지 않은 프로젝트의 말을
// 고칠 수 있다. 사전은 팀의 약속이고 남의 팀 약속은 남의 것이다.
func (tc *toolContext) findGlossaryTerm(projectID, id, term string) (*store.GlossaryTerm, error) {
	if strings.TrimSpace(id) != "" {
		item, err := tc.srv.st.GetGlossaryTerm(tc.ctx, id)
		if err != nil {
			return nil, err
		}
		if item.ProjectID != projectID {
			return nil, fmt.Errorf("그 용어는 이 프로젝트의 것이 아닙니다")
		}
		return item, nil
	}
	needle := strings.TrimSpace(term)
	if needle == "" {
		return nil, fmt.Errorf("고칠 용어의 이름이나 id 를 알려주세요")
	}
	list, err := tc.srv.st.ListGlossary(tc.ctx, projectID, needle, 20)
	if err != nil {
		return nil, err
	}
	for _, t := range list {
		if strings.EqualFold(t.Term, needle) {
			return t, nil
		}
	}
	return nil, fmt.Errorf("용어 %q 을(를) 사전에서 찾을 수 없습니다", needle)
}
