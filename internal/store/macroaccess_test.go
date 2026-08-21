package store

import (
	"context"
	"strings"
	"testing"

	"dbstudio/internal/model"
)

// 접근 규칙은 두 벌로 존재한다: model.ResolveMacroAccess(Go)와 목록 조회의 WHERE 절(SQL).
// 그렇게 둔 이유는 macroaccess.go에 적혀 있지만, 그렇다면 **두 벌이 같은 답을 낸다는
// 것을 시험이 붙잡고 있어야 한다.** 여기서 확인하는 것이 그것이다.
//
// 특히 위험한 실패는 조용하다: 목록에 남의 비공개 매크로가 섞여 나오는 것은
// 아무 오류도 내지 않고, 눈치채는 사람은 그것을 볼 자격이 없는 사람뿐이다.

// accessFixture는 사용자 셋과 매크로 셋을 만든다.
//
//	owner    — 세 매크로 모두의 작성자
//	collab   — private 매크로의 협업자
//	stranger — 아무 관계 없는 매크로 권한자
func accessFixture(t *testing.T) (accessPeople, *Store) {
	t.Helper()
	c, store := macroFixture(t)

	mk := func(id, name string, role model.Role) *model.User {
		if _, err := store.db.ExecContext(c, `INSERT INTO users
			(id, username, username_lower, role, password_hash, perms, created_at, updated_at)
			VALUES (?, ?, ?, ?, 'x', 'macro', ?, ?)`,
			id, name, strings.ToLower(name), string(role), nowString(), nowString()); err != nil {
			t.Fatalf("insert user %s: %v", id, err)
		}
		return &model.User{
			ID: id, Username: name, Role: role, Status: model.UserActive,
			Perms: []model.Perm{model.PermMacro},
		}
	}
	return accessPeople{
		ctx:      c,
		collab:   mk("u-collab", "collab", model.RoleMember),
		stranger: mk("u-stranger", "stranger", model.RoleMember),
		admin:    mk("u-admin", "admin", model.RoleSuperadmin),
	}, store
}

type accessPeople struct {
	ctx                     context.Context
	collab, stranger, admin *model.User
}

func TestListMacrosHidesPrivate(t *testing.T) {
	f, st := accessFixture(t)
	ctx := f.ctx

	priv, err := st.CreateMacro(ctx, "비공개", "", emptyGraph, testAuthor, "작성자")
	if err != nil {
		t.Fatalf("CreateMacro: %v", err)
	}
	// 새 매크로는 비공개로 태어난다. 이 기본값이 뒤집히면 정책 전체가 무의미해진다.
	if priv.Visibility != model.MacroPrivate {
		t.Fatalf("새 매크로는 비공개여야 한다: %q", priv.Visibility)
	}

	shared, err := st.CreateMacro(ctx, "공개", "", emptyGraph, testAuthor, "작성자")
	if err != nil {
		t.Fatalf("CreateMacro: %v", err)
	}
	if err := st.SetMacroVisibility(ctx, shared.ID, model.MacroPublic, model.MacroPublicView); err != nil {
		t.Fatalf("SetMacroVisibility: %v", err)
	}

	teamed, err := st.CreateMacro(ctx, "협업", "", emptyGraph, testAuthor, "작성자")
	if err != nil {
		t.Fatalf("CreateMacro: %v", err)
	}
	if err := st.AddMacroCollaborator(ctx, teamed.ID, f.collab.ID, testAuthor.ID, "작성자"); err != nil {
		t.Fatalf("AddMacroCollaborator: %v", err)
	}

	names := func(u *model.User) []string {
		list, err := st.ListMacros(ctx, MacroViewer{User: u})
		if err != nil {
			t.Fatalf("ListMacros: %v", err)
		}
		out := make([]string, 0, len(list))
		for _, m := range list {
			out = append(out, m.Name)
		}
		return out
	}

	if got := names(testAuthor); len(got) != 3 {
		t.Errorf("작성자는 자기 것을 전부 봐야 한다: %v", got)
	}
	if got := names(f.admin); len(got) != 3 {
		t.Errorf("슈퍼어드민은 전부 봐야 한다: %v", got)
	}
	if got := names(f.stranger); len(got) != 1 || got[0] != "공개" {
		t.Errorf("남에게는 공개된 것만 보여야 한다: %v", got)
	}
	if got := names(f.collab); len(got) != 2 {
		t.Errorf("협업자는 공개 + 초대받은 것을 봐야 한다: %v", got)
	}
	// 협업자로도 '비공개' 매크로는 보이지 않아야 한다.
	for _, n := range names(f.collab) {
		if n == "비공개" {
			t.Error("초대받지 않은 비공개 매크로가 목록에 나왔다")
		}
	}
}

// GetMacro는 볼 수 없는 매크로도 반환하되 Access=none으로 표시한다.
// 이것은 의도된 것이다 — 조용히 ErrNotFound로 바꾸는 조회 함수는 나중에 누군가를 속인다.
func TestGetMacroReportsAccess(t *testing.T) {
	f, st := accessFixture(t)
	ctx := f.ctx

	m, err := st.CreateMacro(ctx, "비공개", "", emptyGraph, testAuthor, "작성자")
	if err != nil {
		t.Fatalf("CreateMacro: %v", err)
	}

	mine, err := st.GetMacro(ctx, m.ID, MacroViewer{User: testAuthor})
	if err != nil {
		t.Fatalf("GetMacro: %v", err)
	}
	if mine.Access != model.MacroAccessOwn || !mine.CanDelete {
		t.Errorf("작성자 권한이 잘못되었다: %v", mine.Access)
	}

	theirs, err := st.GetMacro(ctx, m.ID, MacroViewer{User: f.stranger})
	if err != nil {
		t.Fatalf("GetMacro: %v", err)
	}
	if theirs.Access != model.MacroAccessNone {
		t.Errorf("남에게는 none이어야 한다: %v", theirs.Access)
	}

	// 협업자로 초대하면 관리까지, 삭제는 여전히 안 된다.
	if err := st.AddMacroCollaborator(ctx, m.ID, f.collab.ID, testAuthor.ID, "작성자"); err != nil {
		t.Fatalf("AddMacroCollaborator: %v", err)
	}
	got, err := st.GetMacro(ctx, m.ID, MacroViewer{User: f.collab})
	if err != nil {
		t.Fatalf("GetMacro: %v", err)
	}
	if !got.IsCollaborator || got.Access != model.MacroAccessManage {
		t.Errorf("협업자 권한이 잘못되었다: %v", got.Access)
	}
	if got.CanDelete {
		t.Error("협업자는 삭제할 수 없어야 한다")
	}
	if got.CollaboratorCount != 1 {
		t.Errorf("협업자 수 = %d", got.CollaboratorCount)
	}
}

// 협업자 추가는 두 번 눌러도 오류가 아니다. 두 사람이 같은 사람을 동시에
// 초대하는 것은 실수가 아니라 흔한 동시성이다.
func TestAddCollaboratorIsIdempotent(t *testing.T) {
	f, st := accessFixture(t)
	ctx := f.ctx
	m, err := st.CreateMacro(ctx, "m", "", emptyGraph, testAuthor, "작성자")
	if err != nil {
		t.Fatalf("CreateMacro: %v", err)
	}
	for range 2 {
		if err := st.AddMacroCollaborator(ctx, m.ID, f.collab.ID, testAuthor.ID, "작성자"); err != nil {
			t.Fatalf("AddMacroCollaborator: %v", err)
		}
	}
	list, err := st.ListMacroCollaborators(ctx, m.ID)
	if err != nil {
		t.Fatalf("ListMacroCollaborators: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("협업자는 1명이어야 한다: %d", len(list))
	}
	if !list[0].HasMacroPerm || list[0].Disabled {
		t.Errorf("협업자 상태가 잘못되었다: %+v", list[0])
	}

	if err := st.RemoveMacroCollaborator(ctx, m.ID, f.collab.ID); err != nil {
		t.Fatalf("RemoveMacroCollaborator: %v", err)
	}
	if err := st.RemoveMacroCollaborator(ctx, m.ID, f.collab.ID); err == nil {
		t.Error("없는 협업자를 지우면 ErrNotFound여야 한다")
	}
}

// 매크로 전용 노드는 소속 매크로의 권한을 물려받고, 전역 노드는 자기 것을 쓴다.
func TestNodeDefAccessFollowsScope(t *testing.T) {
	f, st := accessFixture(t)
	ctx := f.ctx

	m, err := st.CreateMacro(ctx, "m", "", emptyGraph, testAuthor, "작성자")
	if err != nil {
		t.Fatalf("CreateMacro: %v", err)
	}
	if err := st.AddMacroCollaborator(ctx, m.ID, f.collab.ID, testAuthor.ID, "작성자"); err != nil {
		t.Fatalf("AddMacroCollaborator: %v", err)
	}

	scoped, err := st.CreateNodeDef(ctx, SaveNodeDefParams{
		Name: "전용", Scope: "macro", MacroID: m.ID, Script: "return 1",
		AuthorID: testAuthor.ID, AuthorName: "작성자",
		Viewer: MacroViewer{User: testAuthor},
	})
	if err != nil {
		t.Fatalf("CreateNodeDef(macro): %v", err)
	}
	global, err := st.CreateNodeDef(ctx, SaveNodeDefParams{
		Name: "전역", Scope: "global", Script: "return 1",
		AuthorID: testAuthor.ID, AuthorName: "작성자",
		Viewer: MacroViewer{User: testAuthor},
	})
	if err != nil {
		t.Fatalf("CreateNodeDef(global): %v", err)
	}

	// 협업자는 매크로 전용 노드를 고치고 지울 수 있다 — 그 노드는 매크로의 일부다.
	got, err := st.GetNodeDef(ctx, scoped.ID, MacroViewer{User: f.collab})
	if err != nil {
		t.Fatalf("GetNodeDef: %v", err)
	}
	if !got.CanEdit || !got.CanDelete {
		t.Errorf("협업자는 매크로 전용 노드를 고치고 지울 수 있어야 한다: %v", got.Access)
	}

	// 같은 사람이 작성자의 비공개 전역 노드는 건드릴 수 없다.
	got, err = st.GetNodeDef(ctx, global.ID, MacroViewer{User: f.collab})
	if err != nil {
		t.Fatalf("GetNodeDef: %v", err)
	}
	if got.Access != model.MacroAccessNone {
		t.Errorf("남의 비공개 전역 노드 = %v, want none", got.Access)
	}
	// 볼 수 없는 노드의 스크립트는 실려 나가지 않아야 한다.
	if got.Script != "" {
		t.Errorf("스크립트가 새어 나갔다: %q", got.Script)
	}

	// 전역 노드는 만든 사람만 지운다(협업자도 지울 수 없다).
	if err := st.AddNodeDefCollaborator(ctx, global.ID, f.collab.ID, testAuthor.ID, "작성자"); err != nil {
		t.Fatalf("AddNodeDefCollaborator: %v", err)
	}
	got, err = st.GetNodeDef(ctx, global.ID, MacroViewer{User: f.collab})
	if err != nil {
		t.Fatalf("GetNodeDef: %v", err)
	}
	if !got.CanEdit || got.CanDelete {
		t.Errorf("전역 노드의 협업자는 고칠 수만 있어야 한다: edit=%v delete=%v",
			got.CanEdit, got.CanDelete)
	}
}

// 팔레트 목록은 걸러지지만, 실행이 쓰는 목록은 걸러지지 않는다.
//
// 이것을 섞으면 남의 비공개 전역 노드를 하나 쓰는 공개 매크로가 만든 사람 외에는
// 아무에게도 돌지 않는다. 공개 설정은 "보이고 고칠 수 있는가"이지 "실행되는가"가 아니다.
func TestNodeDefListsDifferForRunAndPalette(t *testing.T) {
	f, st := accessFixture(t)
	ctx := f.ctx

	m, err := st.CreateMacro(ctx, "m", "", emptyGraph, testAuthor, "작성자")
	if err != nil {
		t.Fatalf("CreateMacro: %v", err)
	}
	if _, err := st.CreateNodeDef(ctx, SaveNodeDefParams{
		Name: "비공개 전역", Scope: "global", Script: "return 1",
		AuthorID: testAuthor.ID, AuthorName: "작성자",
		Viewer: MacroViewer{User: testAuthor},
	}); err != nil {
		t.Fatalf("CreateNodeDef: %v", err)
	}

	visible, err := st.ListVisibleNodeDefs(ctx, m.ID, MacroViewer{User: f.stranger})
	if err != nil {
		t.Fatalf("ListVisibleNodeDefs: %v", err)
	}
	if len(visible) != 0 {
		t.Errorf("남의 팔레트에는 비공개 전역 노드가 없어야 한다: %d개", len(visible))
	}

	forRun, err := st.ListNodeDefs(ctx, m.ID)
	if err != nil {
		t.Fatalf("ListNodeDefs: %v", err)
	}
	if len(forRun) != 1 {
		t.Errorf("실행용 목록은 걸러지지 않아야 한다: %d개", len(forRun))
	}
}

// 실행 이력은 두 갈래로 보인다: 내가 돌린 것과, 볼 수 있는 매크로의 것.
// 앞의 것이 없으면 매크로가 비공개로 바뀌는 순간 내 실행 기록이 사라진다.
func TestListMacroRunsVisibility(t *testing.T) {
	f, st := accessFixture(t)
	ctx := f.ctx

	m, err := st.CreateMacro(ctx, "m", "", emptyGraph, testAuthor, "작성자")
	if err != nil {
		t.Fatalf("CreateMacro: %v", err)
	}
	// 공개 상태에서 남이 한 번 돌린다.
	if err := st.SetMacroVisibility(ctx, m.ID, model.MacroPublic, model.MacroPublicView); err != nil {
		t.Fatalf("SetMacroVisibility: %v", err)
	}
	if _, err := st.CreateMacroRun(ctx, CreateRunParams{
		MacroID: m.ID, MacroName: m.Name, Version: 1,
		ActorID: f.stranger.ID, ActorName: f.stranger.Username,
	}); err != nil {
		t.Fatalf("CreateMacroRun: %v", err)
	}
	// 작성자도 한 번 돌린다.
	if _, err := st.CreateMacroRun(ctx, CreateRunParams{
		MacroID: m.ID, MacroName: m.Name, Version: 1,
		ActorID: testAuthor.ID, ActorName: testAuthor.Username,
	}); err != nil {
		t.Fatalf("CreateMacroRun: %v", err)
	}

	count := func(u *model.User) int {
		runs, err := st.ListMacroRuns(ctx, "", "", 0, MacroViewer{User: u})
		if err != nil {
			t.Fatalf("ListMacroRuns: %v", err)
		}
		return len(runs)
	}
	if n := count(f.stranger); n != 2 {
		t.Errorf("공개 매크로의 이력은 전부 보여야 한다: %d", n)
	}

	// 다시 비공개로 되돌린다.
	if err := st.SetMacroVisibility(ctx, m.ID, model.MacroPrivate, model.MacroPublicView); err != nil {
		t.Fatalf("SetMacroVisibility: %v", err)
	}
	if n := count(f.stranger); n != 1 {
		t.Errorf("비공개가 된 뒤에는 자기가 돌린 것만 남아야 한다: %d", n)
	}
	if n := count(testAuthor); n != 2 {
		t.Errorf("작성자는 여전히 전부 봐야 한다: %d", n)
	}
	if n := count(f.admin); n != 2 {
		t.Errorf("슈퍼어드민은 전부 봐야 한다: %d", n)
	}
}

// 초대 후보는 매크로 권한이 있는 활성 계정으로 제한된다.
// 초대해도 아무것도 못 하는 사람을 고르게 두면 왜 안 되는지 알 방법이 없다.
func TestListMacroPeopleFiltersByPerm(t *testing.T) {
	f, st := accessFixture(t)
	ctx := f.ctx

	if _, err := st.db.ExecContext(ctx, `INSERT INTO users
		(id, username, username_lower, role, password_hash, perms, created_at, updated_at)
		VALUES ('u-none', 'nomacro', 'nomacro', 'member', 'x', '', ?, ?)`,
		nowString(), nowString()); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	if _, err := st.db.ExecContext(ctx, `INSERT INTO users
		(id, username, username_lower, role, password_hash, perms, status, created_at, updated_at)
		VALUES ('u-off', 'off', 'off', 'member', 'x', 'macro', 'disabled', ?, ?)`,
		nowString(), nowString()); err != nil {
		t.Fatalf("insert user: %v", err)
	}

	people, err := st.ListMacroPeople(ctx, testAuthor.ID)
	if err != nil {
		t.Fatalf("ListMacroPeople: %v", err)
	}
	for _, p := range people {
		if p.ID == "u-none" {
			t.Error("매크로 권한 없는 사람이 후보에 있다")
		}
		if p.ID == "u-off" {
			t.Error("비활성 계정이 후보에 있다")
		}
		if p.ID == testAuthor.ID {
			t.Error("자기 자신이 후보에 있다")
		}
	}
	// 셋(collab, stranger, admin)은 후보여야 한다.
	if len(people) != 3 {
		t.Errorf("후보 3명이어야 한다: %d", len(people))
	}
	if _, err := st.GetMacroPerson(ctx, "u-off"); err == nil {
		t.Error("비활성 계정은 초대 대상이 아니어야 한다")
	}
	if _, err := st.GetMacroPerson(ctx, f.collab.ID); err != nil {
		t.Errorf("활성 계정은 초대 대상이어야 한다: %v", err)
	}
}
