package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"dbstudio/internal/crypto"
	"dbstudio/internal/model"
)

func macroFixture(t *testing.T) (context.Context, *Store) {
	t.Helper()
	ctx := context.Background()
	box, err := crypto.NewSecretBox(make([]byte, 32))
	if err != nil {
		t.Fatalf("secret box: %v", err)
	}
	st, err := Open(ctx, filepath.Join(t.TempDir(), "macro.db"), box)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	// macros.created_by는 users를 참조한다. 매크로가 만든 사람의 것이 된 뒤로
	// 작성자는 실제로 존재하는 계정이어야 한다.
	if _, err := st.db.ExecContext(ctx, `INSERT INTO users
		(id, username, username_lower, role, password_hash, perms, created_at, updated_at)
		VALUES (?, ?, ?, 'member', 'x', 'macro', ?, ?)`,
		testAuthor.ID, testAuthor.Username, strings.ToLower(testAuthor.Username),
		nowString(), nowString()); err != nil {
		t.Fatalf("insert author: %v", err)
	}
	return ctx, st
}

const emptyGraph = `{"nodes":[{"id":"s","type":"start"}],"edges":[],"params":[]}`

// 매크로는 만든 사람의 것이므로 저장 함수가 사람을 요구한다.
// 접근 제어 자체의 시험은 macroaccess_test.go에 있고, 여기서는 작성자 본인 시점으로
// 본다 — 버전 관리와 실행 이력은 접근 제어와 독립적인 관심사다.
func macroUser(id, name string) *model.User {
	return &model.User{
		ID: id, Username: name, Role: model.RoleMember,
		Status: model.UserActive, Perms: []model.Perm{model.PermMacro},
	}
}

var testAuthor = macroUser("u-author", "tester")

func authorView() MacroViewer { return MacroViewer{User: testAuthor} }

// 매크로는 만들어지는 순간 실행 가능한 버전을 가져야 한다.
// current_version=0인 상태가 존재하면 "무엇을 실행해야 하는가"에 답이 없다.
func TestCreateMacroMakesFirstVersion(t *testing.T) {
	ctx, st := macroFixture(t)

	m, err := st.CreateMacro(ctx, "야간 정리", "설명", emptyGraph, testAuthor, "관리자")
	if err != nil {
		t.Fatalf("CreateMacro: %v", err)
	}
	if m.CurrentVersion != 1 {
		t.Errorf("첫 버전은 1이어야 한다: %d", m.CurrentVersion)
	}
	if m.VersionCount != 1 {
		t.Errorf("버전 수 = %d", m.VersionCount)
	}

	v, err := st.GetMacroVersion(ctx, m.ID, 0) // 0 = 현재 버전
	if err != nil {
		t.Fatalf("GetMacroVersion: %v", err)
	}
	if v.Version != 1 || v.Graph != emptyGraph {
		t.Errorf("현재 버전이 잘못되었다: v%d", v.Version)
	}
}

func TestCreateMacroRejectsDuplicateName(t *testing.T) {
	ctx, st := macroFixture(t)
	if _, err := st.CreateMacro(ctx, "같은 이름", "", emptyGraph, testAuthor, "a"); err != nil {
		t.Fatalf("CreateMacro: %v", err)
	}
	_, err := st.CreateMacro(ctx, "같은 이름", "", emptyGraph, testAuthor, "b")
	if !errors.Is(err, ErrDuplicateName) {
		t.Fatalf("중복 이름은 거부해야 한다: %v", err)
	}
}

// 버전은 추가만 된다. 덮어쓰기를 허용하면 실행 이력이 가리키는 버전이
// 다른 내용으로 바뀌어 있을 수 있다.
func TestVersionsAreAppendOnly(t *testing.T) {
	ctx, st := macroFixture(t)
	m, err := st.CreateMacro(ctx, "m", "", emptyGraph, testAuthor, "a")
	if err != nil {
		t.Fatalf("CreateMacro: %v", err)
	}

	second := `{"nodes":[{"id":"s","type":"start"},{"id":"l","type":"log"}],"edges":[],"params":[]}`
	v2, err := st.CreateMacroVersion(ctx, m.ID, second, "로그 추가", "", "b")
	if err != nil {
		t.Fatalf("CreateMacroVersion: %v", err)
	}
	if v2 != 2 {
		t.Fatalf("다음 버전은 2여야 한다: %d", v2)
	}

	// 옛 버전은 그대로 남아 있어야 한다.
	old, err := st.GetMacroVersion(ctx, m.ID, 1)
	if err != nil {
		t.Fatalf("GetMacroVersion(1): %v", err)
	}
	if old.Graph != emptyGraph {
		t.Error("옛 버전의 내용이 바뀌었다")
	}

	updated, err := st.GetMacro(ctx, m.ID, authorView())
	if err != nil {
		t.Fatalf("GetMacro: %v", err)
	}
	if updated.CurrentVersion != 2 || updated.UpdatedByName != "b" {
		t.Errorf("현재 버전과 수정자가 갱신되어야 한다: v%d by %s",
			updated.CurrentVersion, updated.UpdatedByName)
	}

	versions, err := st.ListMacroVersions(ctx, m.ID)
	if err != nil {
		t.Fatalf("ListMacroVersions: %v", err)
	}
	if len(versions) != 2 || versions[0].Version != 2 {
		t.Errorf("최신순 2건이어야 한다: %d건", len(versions))
	}
	// 목록에는 그래프 본문이 실리지 않는다. 수십 개의 그래프 JSON을 보낼 이유가 없다.
	if versions[0].Graph != "" {
		t.Error("목록에 그래프 본문이 실려 있다")
	}
}

// 롤백은 포인터를 옮기지 않고 새 버전을 만든다.
// 포인터만 옮기면 그 뒤의 버전들이 미래에 남아 이력이 시간순으로 읽히지 않는다.
func TestRestoreCreatesNewVersion(t *testing.T) {
	ctx, st := macroFixture(t)
	m, err := st.CreateMacro(ctx, "m", "", emptyGraph, testAuthor, "a")
	if err != nil {
		t.Fatalf("CreateMacro: %v", err)
	}
	broken := `{"nodes":[{"id":"s","type":"start"},{"id":"x","type":"log"}],"edges":[],"params":[]}`
	if _, err := st.CreateMacroVersion(ctx, m.ID, broken, "실수", "", "b"); err != nil {
		t.Fatalf("CreateMacroVersion: %v", err)
	}

	// v1의 그래프로 되돌린다 = v3을 만든다.
	v1, err := st.GetMacroVersion(ctx, m.ID, 1)
	if err != nil {
		t.Fatalf("GetMacroVersion: %v", err)
	}
	v3, err := st.CreateMacroVersion(ctx, m.ID, v1.Graph, "v1으로 되돌림", "", "c")
	if err != nil {
		t.Fatalf("CreateMacroVersion: %v", err)
	}
	if v3 != 3 {
		t.Fatalf("되돌리기도 새 버전이어야 한다: %d", v3)
	}

	current, err := st.GetMacroVersion(ctx, m.ID, 0)
	if err != nil {
		t.Fatalf("GetMacroVersion(current): %v", err)
	}
	if current.Version != 3 || current.Graph != emptyGraph {
		t.Errorf("현재 버전이 v1의 내용이어야 한다: v%d", current.Version)
	}
}

// 매크로를 지웠다고 실행 기록까지 사라지면, "누가 무엇을 실행했는가"를 지우는
// 방법이 매크로 삭제가 되어 버린다.
func TestRunSurvivesMacroDeletion(t *testing.T) {
	ctx, st := macroFixture(t)
	m, err := st.CreateMacro(ctx, "m", "", emptyGraph, testAuthor, "a")
	if err != nil {
		t.Fatalf("CreateMacro: %v", err)
	}
	runID, err := st.CreateMacroRun(ctx, CreateRunParams{
		MacroID: m.ID, MacroName: m.Name, Version: 1, ActorName: "홍길동",
		Params: map[string]any{"limit": float64(5)},
	})
	if err != nil {
		t.Fatalf("CreateMacroRun: %v", err)
	}
	if err := st.FinishMacroRun(ctx, runID, "success", "", 120, 3); err != nil {
		t.Fatalf("FinishMacroRun: %v", err)
	}
	if err := st.DeleteMacro(ctx, m.ID); err != nil {
		t.Fatalf("DeleteMacro: %v", err)
	}

	run, err := st.GetMacroRun(ctx, runID)
	if err != nil {
		t.Fatalf("삭제 후에도 실행 기록은 남아야 한다: %v", err)
	}
	if run.MacroName != "m" || run.Status != "success" || run.ActorName != "홍길동" {
		t.Errorf("기록 내용이 보존되어야 한다: %+v", run)
	}
	if run.MacroID != "" {
		t.Errorf("매크로 참조는 비워져야 한다: %q", run.MacroID)
	}
	if run.Params["limit"] != float64(5) {
		t.Errorf("파라미터가 보존되어야 한다: %+v", run.Params)
	}
}

func TestRunLogsAreOrderedAndResumable(t *testing.T) {
	ctx, st := macroFixture(t)
	runID, err := st.CreateMacroRun(ctx, CreateRunParams{MacroName: "m", Version: 1})
	if err != nil {
		t.Fatalf("CreateMacroRun: %v", err)
	}
	for i := 1; i <= 5; i++ {
		if err := st.AppendRunLog(ctx, runID, i, "info", "n1", "노드", "줄", nil); err != nil {
			t.Fatalf("AppendRunLog: %v", err)
		}
	}

	all, err := st.ListRunLogs(ctx, runID, 0)
	if err != nil {
		t.Fatalf("ListRunLogs: %v", err)
	}
	if len(all) != 5 || all[0].Seq != 1 || all[4].Seq != 5 {
		t.Fatalf("순서대로 5건이어야 한다: %d건", len(all))
	}

	// afterSeq는 SSE 재접속 시 빠진 부분만 받기 위한 것이다.
	rest, err := st.ListRunLogs(ctx, runID, 3)
	if err != nil {
		t.Fatalf("ListRunLogs(after=3): %v", err)
	}
	if len(rest) != 2 || rest[0].Seq != 4 {
		t.Errorf("4번부터 2건이어야 한다: %+v", rest)
	}
}

// 앱이 실행 도중 죽으면 그 행은 영원히 'running'으로 남는다.
// 부팅 시 정리하지 않으면 화면은 끝나지 않는 실행을 계속 보여준다.
func TestMarkStaleRunsFailed(t *testing.T) {
	ctx, st := macroFixture(t)
	runID, err := st.CreateMacroRun(ctx, CreateRunParams{MacroName: "m", Version: 1})
	if err != nil {
		t.Fatalf("CreateMacroRun: %v", err)
	}
	done, err := st.CreateMacroRun(ctx, CreateRunParams{MacroName: "m", Version: 1})
	if err != nil {
		t.Fatalf("CreateMacroRun: %v", err)
	}
	if err := st.FinishMacroRun(ctx, done, "success", "", 10, 1); err != nil {
		t.Fatalf("FinishMacroRun: %v", err)
	}

	n, err := st.MarkStaleRunsFailed(ctx)
	if err != nil {
		t.Fatalf("MarkStaleRunsFailed: %v", err)
	}
	if n != 1 {
		t.Errorf("실행 중이던 1건만 바꿔야 한다: %d", n)
	}
	stale, err := st.GetMacroRun(ctx, runID)
	if err != nil {
		t.Fatalf("GetMacroRun: %v", err)
	}
	if stale.Status != "failed" || stale.Error == "" {
		t.Errorf("실패로 표시하고 이유를 남겨야 한다: %+v", stale)
	}
	// 이미 끝난 기록은 건드리지 않는다.
	finished, err := st.GetMacroRun(ctx, done)
	if err != nil {
		t.Fatalf("GetMacroRun: %v", err)
	}
	if finished.Status != "success" {
		t.Errorf("끝난 기록을 바꿔서는 안 된다: %s", finished.Status)
	}
}

// 전역 노드는 모든 매크로에서, 매크로 전용 노드는 그 매크로에서만 보여야 한다.
func TestNodeDefScoping(t *testing.T) {
	ctx, st := macroFixture(t)
	a, err := st.CreateMacro(ctx, "A", "", emptyGraph, testAuthor, "u")
	if err != nil {
		t.Fatalf("CreateMacro: %v", err)
	}
	b, err := st.CreateMacro(ctx, "B", "", emptyGraph, testAuthor, "u")
	if err != nil {
		t.Fatalf("CreateMacro: %v", err)
	}

	if _, err := st.CreateNodeDef(ctx, SaveNodeDefParams{
		Name: "전역 노드", Scope: "global", Script: "return 1", AuthorName: "u",
	}); err != nil {
		t.Fatalf("CreateNodeDef(global): %v", err)
	}
	if _, err := st.CreateNodeDef(ctx, SaveNodeDefParams{
		Name: "A 전용", Scope: "macro", MacroID: a.ID, Script: "return 2", AuthorName: "u",
	}); err != nil {
		t.Fatalf("CreateNodeDef(macro): %v", err)
	}

	forA, err := st.ListNodeDefs(ctx, a.ID)
	if err != nil {
		t.Fatalf("ListNodeDefs(A): %v", err)
	}
	if len(forA) != 2 {
		t.Errorf("A에서는 전역 + 전용 = 2개여야 한다: %d", len(forA))
	}

	forB, err := st.ListNodeDefs(ctx, b.ID)
	if err != nil {
		t.Fatalf("ListNodeDefs(B): %v", err)
	}
	if len(forB) != 1 || forB[0].Name != "전역 노드" {
		t.Errorf("B에서는 전역 1개만 보여야 한다: %d", len(forB))
	}
}

// 전역 노드는 여러 매크로가 함께 쓴다. 누군가 고쳐서 깨졌을 때 되돌릴 수 없으면
// 그 노드를 쓰는 매크로가 전부 멈춘다.
func TestNodeDefVersioning(t *testing.T) {
	ctx, st := macroFixture(t)
	def, err := st.CreateNodeDef(ctx, SaveNodeDefParams{
		Name: "n", Scope: "global", Script: "return 1", Ports: `["out"]`,
		AuthorID: testAuthor.ID, AuthorName: "u", Viewer: authorView(),
	})
	if err != nil {
		t.Fatalf("CreateNodeDef: %v", err)
	}
	if def.CurrentVersion != 1 {
		t.Fatalf("첫 버전은 1이어야 한다: %d", def.CurrentVersion)
	}

	updated, err := st.UpdateNodeDef(ctx, def.ID, SaveNodeDefParams{
		Name: "n", Scope: "global", Script: "return 2", Ports: `["out"]`,
		Note: "값 변경", AuthorID: testAuthor.ID, AuthorName: "v", Viewer: authorView(),
	})
	if err != nil {
		t.Fatalf("UpdateNodeDef: %v", err)
	}
	if updated.CurrentVersion != 2 || updated.Script != "return 2" {
		t.Errorf("갱신 결과가 잘못되었다: v%d %q", updated.CurrentVersion, updated.Script)
	}

	versions, err := st.ListNodeDefVersions(ctx, def.ID)
	if err != nil {
		t.Fatalf("ListNodeDefVersions: %v", err)
	}
	if len(versions) != 2 {
		t.Fatalf("버전 2건이어야 한다: %d", len(versions))
	}
	// 옛 스크립트가 남아 있어야 되돌릴 수 있다.
	if versions[1].Script != "return 1" {
		t.Errorf("v1 스크립트가 보존되어야 한다: %q", versions[1].Script)
	}
}

func TestListMacroRunsFilters(t *testing.T) {
	ctx, st := macroFixture(t)
	m, err := st.CreateMacro(ctx, "m", "", emptyGraph, testAuthor, "u")
	if err != nil {
		t.Fatalf("CreateMacro: %v", err)
	}
	ok, _ := st.CreateMacroRun(ctx, CreateRunParams{MacroID: m.ID, MacroName: "m", Version: 1})
	bad, _ := st.CreateMacroRun(ctx, CreateRunParams{MacroID: m.ID, MacroName: "m", Version: 1})
	_ = st.FinishMacroRun(ctx, ok, "success", "", 1, 1)
	_ = st.FinishMacroRun(ctx, bad, "failed", "터짐", 1, 1)

	failed, err := st.ListMacroRuns(ctx, m.ID, "failed", 0, authorView())
	if err != nil {
		t.Fatalf("ListMacroRuns: %v", err)
	}
	if len(failed) != 1 || failed[0].ID != bad {
		t.Errorf("실패 1건만 나와야 한다: %d건", len(failed))
	}

	all, err := st.ListMacroRuns(ctx, "", "", 0, authorView())
	if err != nil {
		t.Fatalf("ListMacroRuns(all): %v", err)
	}
	if len(all) != 2 {
		t.Errorf("전체 2건이어야 한다: %d건", len(all))
	}
}
