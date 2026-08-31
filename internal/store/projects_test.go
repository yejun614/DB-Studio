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
	// 서버도 프로젝트 안에 있다. 프로젝트마다 따로 등록하는 것이 실제 쓰임이다.
	srv1 := mkServerIn(t, ctx, st, a.ID, "pg-1")
	srv2 := mkServerIn(t, ctx, st, b.ID, "pg-2")
	srv3 := mkServerIn(t, ctx, st, a.ID, "pg-3")

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
	if err := mk(a.ID, srv3.ID, "other"); !errors.Is(err, ErrDuplicateName) {
		t.Errorf("같은 프로젝트의 같은 이름 = %v, 막아야 합니다", err)
	}
	// 서버 이름도 프로젝트 안에서만 유일하다.
	if _, err := st.CreateServer(ctx, SaveServerParams{
		ProjectID: b.ID, Name: "pg-1", Kind: model.KindPostgres,
		Host: "10.0.0.9", Port: 5432, DefaultEnvironment: model.EnvDev, Enabled: true,
	}); err != nil {
		t.Errorf("다른 프로젝트의 같은 서버 이름이 막혔습니다: %v", err)
	}
}

// DB는 언제나 그 서버의 프로젝트에 들어간다.
//
// 근거가 둘이면 어긋날 수 있고, 어긋나는 순간 어느 쪽이 참인지 권한 판정이 답하지
// 못한다. 그래서 저장 계층이 서버 쪽 값을 참으로 삼는다.
func TestConnectionFollowsItsServerProject(t *testing.T) {
	ctx, st := projectFixture(t)
	a, _ := st.CreateProject(ctx, SaveProjectParams{Name: "결제"})
	b, _ := st.CreateProject(ctx, SaveProjectParams{Name: "물류"})
	srv := mkServerIn(t, ctx, st, a.ID, "pg")

	// 다른 프로젝트를 적어 넣으면 막는다. 조용히 고치면 사람이 고른 것과 저장된
	// 것이 달라지고, 그 사실은 목록에서 사라진 형태로만 드러난다.
	if _, err := st.CreateConnection(ctx, SaveConnectionParams{
		ProjectID: b.ID, ServerID: srv.ID, Name: "운영 DB",
		Environment: model.EnvDev, DatabaseName: "appdb", Enabled: true,
	}); !errors.Is(err, ErrProjectMismatch) {
		t.Errorf("서버와 다른 프로젝트 = %v, ErrProjectMismatch 여야 합니다", err)
	}

	// 비워 두면 서버에서 채운다.
	conn, err := st.CreateConnection(ctx, SaveConnectionParams{
		ServerID: srv.ID, Name: "운영 DB",
		Environment: model.EnvDev, DatabaseName: "appdb", Enabled: true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if conn.ProjectID != a.ID {
		t.Errorf("DB의 프로젝트 = %q, 서버의 %q 여야 합니다", conn.ProjectID, a.ID)
	}
}

func mkServerIn(t *testing.T, ctx context.Context, st *Store, projectID, name string) *model.Server {
	t.Helper()
	pw := "pw"
	srv, err := st.CreateServer(ctx, SaveServerParams{
		ProjectID: projectID, Name: name, Kind: model.KindPostgres,
		Host: "10.0.0.1", Port: 5432, DefaultEnvironment: model.EnvDev,
		Enabled: true, Username: "app", Password: &pw,
	})
	if err != nil {
		t.Fatalf("create server %s: %v", name, err)
	}
	return srv
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
	srv := mkServerIn(t, ctx, st, p.ID, "pg")
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
	// 서버가 남아 있으면 아직 빈 프로젝트가 아니다. 서버에는 접속 정보와
	// 자격증명이 들어 있어서, 프로젝트만 지우고 남기면 갈 곳 없는 비밀이 된다.
	if err := st.DeleteProject(ctx, p.ID); !errors.Is(err, ErrProjectInUse) {
		t.Errorf("서버가 남았는데 삭제됐습니다: %v", err)
	}
	if err := st.DeleteServer(ctx, srv.ID); err != nil {
		t.Fatalf("delete server: %v", err)
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

// 서버 목록도 프로젝트로 좁힌다.
//
// 이 시험이 없었을 때 실제로 빠뜨렸다: 함수 서명만 바꾸고 WHERE 절을 넣지 않아,
// 새 프로젝트를 열었는데 남의 팀 서버가 호스트 이름까지 그대로 떠 있었다.
// 목록에서 새는 것은 화면을 열어 보기 전까지 아무도 모른다.
func TestServerListIsScopedToProject(t *testing.T) {
	ctx, st := projectFixture(t)
	a, _ := st.CreateProject(ctx, SaveProjectParams{Name: "결제"})
	b, _ := st.CreateProject(ctx, SaveProjectParams{Name: "물류"})
	mkServerIn(t, ctx, st, a.ID, "pg-결제")
	mkServerIn(t, ctx, st, b.ID, "pg-물류")

	only, err := st.ListServers(ctx, []string{a.ID})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(only) != 1 || only[0].Name != "pg-결제" {
		t.Errorf("한 프로젝트로 좁힌 결과 = %v", serverNames(only))
	}

	// nil은 "제한 없음"(슈퍼 어드민), 빈 슬라이스는 "하나도 없음"이다. 이 둘을
	// 섞으면 참여하지 않은 사람에게 전부가 보인다.
	all, _ := st.ListServers(ctx, nil)
	if len(all) != 2 {
		t.Errorf("제한 없음 = %v", serverNames(all))
	}
	none, _ := st.ListServers(ctx, []string{})
	if len(none) != 0 {
		t.Errorf("볼 수 있는 프로젝트가 없는데 %v 가 왔습니다", serverNames(none))
	}
}

func serverNames(list []*model.Server) []string {
	out := make([]string, 0, len(list))
	for _, s := range list {
		out = append(out, s.Name)
	}
	return out
}
