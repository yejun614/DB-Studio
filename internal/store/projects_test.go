package store

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"testing"

	"dbstudio/internal/crypto"
	"dbstudio/internal/model"
)

func projectFixture(t *testing.T) (context.Context, *Store) {
	t.Helper()
	ctx := context.Background()
	box, err := crypto.NewSecretBox(make([]byte, 32))
	if err != nil {
		t.Fatalf("secret box: %v", err)
	}
	st, err := Open(ctx, filepath.Join(t.TempDir(), "projects.db"), box)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return ctx, st
}

// 새 설치에는 프로젝트가 없다.
//
// 기본 프로젝트를 미리 만들어 두면 이 층이 있다는 사실이 감춰지고, 사람들은 그
// 한 칸에 모든 팀의 DB를 그대로 쌓는다. 옮길 것이 있는 앱에만 만든다(0037).
func TestFreshInstallHasNoProject(t *testing.T) {
	ctx, st := projectFixture(t)
	list, err := st.ListProjects(ctx, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("새 설치의 프로젝트 = %d개, 0개여야 합니다", len(list))
	}
}

// 만든 사람은 자동으로 참여자가 된다.
//
// 그러지 않으면 프로젝트를 만든 직후 화면이 비어 있고, 자기가 만든 것에 자기를
// 초대해야 한다. 그 한 걸음은 설명할 수 없다.
func TestProjectCreatorJoins(t *testing.T) {
	ctx, st := projectFixture(t)
	u := mkUser(t, ctx, st, "maker")

	p, err := st.CreateProject(ctx, SaveProjectParams{Name: "결제", ActorID: u.ID})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	ok, err := st.IsProjectMember(ctx, p.ID, u.ID)
	if err != nil {
		t.Fatalf("member: %v", err)
	}
	if !ok {
		t.Error("만든 사람이 참여자가 아닙니다")
	}
	if p.Members != 1 {
		t.Errorf("참여자 수 = %d, 기대 1", p.Members)
	}
}

// 목록은 참여한 것만 보여준다.
//
// 보이지만 열면 거부되는 줄은 권한 설정이 잘못된 것처럼 보인다. 그리고 프로젝트
// 이름 자체가 남에게 알려서는 안 되는 것일 수 있다("이직-검토").
func TestProjectListIsScopedToMembership(t *testing.T) {
	ctx, st := projectFixture(t)
	mine := mkUser(t, ctx, st, "mine")
	other := mkUser(t, ctx, st, "other")

	if _, err := st.CreateProject(ctx, SaveProjectParams{Name: "결제", ActorID: mine.ID}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.CreateProject(ctx, SaveProjectParams{Name: "물류", ActorID: other.ID}); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := st.ListProjects(ctx, mine.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 || got[0].Name != "결제" {
		t.Errorf("참여한 프로젝트 = %v", projectNames(got))
	}
	// 빈 사용자 아이디는 "전부"다(슈퍼 어드민).
	all, err := st.ListProjects(ctx, "")
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("전체 = %v", projectNames(all))
	}
}

// 같은 이름의 DB가 프로젝트마다 있을 수 있다.
//
// 예전에는 커넥션 이름이 앱 전체에서 유일했다. 그러면 다른 팀이 이미 "운영 DB"를
// 쓰고 있을 때, 보이지도 않는 이름 때문에 등록이 막힌다 — 원인을 짚을 방법이 없는
// 실패다.
func TestConnectionNameIsUniquePerProject(t *testing.T) {
	ctx, st := projectFixture(t)
	a, err := st.CreateProject(ctx, SaveProjectParams{Name: "결제"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	b, err := st.CreateProject(ctx, SaveProjectParams{Name: "물류"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	srv1 := mkServer(t, ctx, st, "pg-1")
	srv2 := mkServer(t, ctx, st, "pg-2")

	mk := func(project, server, db string) error {
		_, err := st.CreateConnection(ctx, SaveConnectionParams{
			ProjectID: project, ServerID: server, Name: "운영 DB",
			Environment: model.EnvDev, DatabaseName: db, Enabled: true,
		})
		return err
	}
	if err := mk(a.ID, srv1.ID, "appdb"); err != nil {
		t.Fatalf("첫 등록: %v", err)
	}
	if err := mk(b.ID, srv2.ID, "appdb"); err != nil {
		t.Errorf("다른 프로젝트의 같은 이름이 막혔습니다: %v", err)
	}
	if err := mk(a.ID, srv2.ID, "other"); !errors.Is(err, ErrDuplicateName) {
		t.Errorf("같은 프로젝트의 같은 이름 = %v, 막아야 합니다", err)
	}
}

// 빈 프로젝트만 지운다.
//
// 안에 든 것을 함께 지우면, 그 단추는 무엇을 지우는지 말할 수 없는 단추가 된다.
// 커넥션 하나를 지우는 것도 무엇이 함께 사라지는지 세어 보여 준 뒤에 하는 일이다.
func TestProjectWithResourcesIsNotDeleted(t *testing.T) {
	ctx, st := projectFixture(t)
	p, err := st.CreateProject(ctx, SaveProjectParams{Name: "결제"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	srv := mkServer(t, ctx, st, "pg")
	conn, err := st.CreateConnection(ctx, SaveConnectionParams{
		ProjectID: p.ID, ServerID: srv.ID, Name: "운영 DB",
		Environment: model.EnvDev, DatabaseName: "appdb", Enabled: true,
	})
	if err != nil {
		t.Fatalf("create connection: %v", err)
	}

	if err := st.DeleteProject(ctx, p.ID); !errors.Is(err, ErrProjectInUse) {
		t.Errorf("자원이 든 프로젝트 삭제 = %v, ErrProjectInUse 여야 합니다", err)
	}
	if err := st.DeleteConnection(ctx, conn.ID); err != nil {
		t.Fatalf("delete connection: %v", err)
	}
	if err := st.DeleteProject(ctx, p.ID); err != nil {
		t.Errorf("빈 프로젝트 삭제: %v", err)
	}
}

// 참여자 명단은 통째로 갈아 끼운다. 부분 갱신이면 화면의 명단과 저장된 명단이
// 갈라지는 순간이 생긴다.
func TestSetProjectMembersReplaces(t *testing.T) {
	ctx, st := projectFixture(t)
	p, err := st.CreateProject(ctx, SaveProjectParams{Name: "결제"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	a := mkUser(t, ctx, st, "a")
	b := mkUser(t, ctx, st, "b")

	if err := st.SetProjectMembers(ctx, p.ID, []string{a.ID, b.ID, a.ID}); err != nil {
		t.Fatalf("set: %v", err)
	}
	members, err := st.ListProjectMembers(ctx, p.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(members) != 2 {
		t.Fatalf("참여자 = %d명, 기대 2명(중복은 한 번)", len(members))
	}

	if err := st.SetProjectMembers(ctx, p.ID, []string{b.ID}); err != nil {
		t.Fatalf("replace: %v", err)
	}
	members, _ = st.ListProjectMembers(ctx, p.ID)
	if len(members) != 1 || members[0].UserID != b.ID {
		t.Errorf("교체 뒤 참여자 = %v", members)
	}
}

// 접근 정책은 참여 프로젝트를 함께 읽고 쓴다.
//
// 권한 판정(auth)이 정책 하나만 읽고 결론을 내기 때문이다. 여기서 빠지면 판정은
// "참여하지 않았다"로 떨어지고, 그 이유는 권한 화면 어디에도 적혀 있지 않다.
func TestAccessPolicyCarriesProjects(t *testing.T) {
	ctx, st := projectFixture(t)
	u := mkUser(t, ctx, st, "dana")
	p, err := st.CreateProject(ctx, SaveProjectParams{Name: "결제"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	policy, err := st.GetAccessPolicy(ctx, u.ID)
	if err != nil {
		t.Fatalf("get policy: %v", err)
	}
	if len(policy.Projects) != 0 {
		t.Errorf("처음 참여 목록 = %v, 비어 있어야 합니다", policy.Projects)
	}
	policy.Projects = []string{p.ID}
	if err := st.SetAccessPolicy(ctx, policy); err != nil {
		t.Fatalf("set policy: %v", err)
	}

	again, err := st.GetAccessPolicy(ctx, u.ID)
	if err != nil {
		t.Fatalf("get policy 2: %v", err)
	}
	if !slices.Contains(again.Projects, p.ID) {
		t.Errorf("참여 목록이 저장되지 않았습니다: %v", again.Projects)
	}
	// 프로젝트 화면과 권한 화면이 같은 표를 쓴다.
	members, _ := st.ListProjectMembers(ctx, p.ID)
	if len(members) != 1 {
		t.Errorf("프로젝트 쪽에서 본 참여자 = %d명, 기대 1명", len(members))
	}
}

func projectNames(list []*Project) []string {
	out := make([]string, 0, len(list))
	for _, p := range list {
		out = append(out, p.Name)
	}
	return out
}
