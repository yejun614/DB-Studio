package api

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"dbstudio/internal/model"
	"dbstudio/internal/store"
)

var dockerToolNames = []string{
	"describe_db_catalog", "list_db_containers", "get_db_container_logs",
	"export_db_compose", "create_db_container", "control_db_container",
	"remove_db_container",
}

func toolNames(u *model.User, hints toolHints) []string {
	tools, _ := availableTools(u, hints)
	out := make([]string, 0, len(tools))
	for _, t := range tools {
		out = append(out, t.Name)
	}
	return out
}

// 도커 툴은 **문 둘을 다 지나야** 보인다.
//
// 스위치가 꺼진 서버에서 권한만 보고 툴을 내놓으면 모델이 그것을 부르고, 매번
// "이 서버는 도커 기능이 꺼져 있습니다"를 받는다 — 토큰을 쓰고 대화도 지저분해진다.
func TestDockerToolsNeedSwitchAndPermission(t *testing.T) {
	withPerm := &model.User{
		Role: model.RoleMember, Status: model.UserActive,
		Perms: []model.Perm{model.PermDockerManage},
	}
	// 권한 관문은 멤버로 본다. 슈퍼 어드민은 모든 전역 권한을 암묵적으로
	// 가지므로(model.User.HasPerm), 그 역할로는 이 관문이 확인되지 않는다.
	noPerm := &model.User{Role: model.RoleMember, Status: model.UserActive}
	admin := &model.User{Role: model.RoleSuperadmin, Status: model.UserActive}

	// 스위치가 꺼져 있으면 권한이 있어도 안 보인다. 슈퍼 어드민도 마찬가지다 —
	// 스위치는 프로세스를 띄우는 사람이 정하는 것이고 역할로 넘을 수 없다.
	for _, u := range []*model.User{withPerm, admin} {
		off := toolNames(u, toolHints{DockerEnabled: false})
		for _, name := range dockerToolNames {
			if slices.Contains(off, name) {
				t.Errorf("스위치가 꺼졌는데 %s 가 보입니다 (%s)", name, u.Role)
			}
		}
	}

	// 스위치가 켜져 있어도 권한이 없으면 안 보인다.
	noRight := toolNames(noPerm, toolHints{DockerEnabled: true})
	for _, name := range dockerToolNames {
		if slices.Contains(noRight, name) {
			t.Errorf("권한이 없는데 %s 가 보입니다", name)
		}
	}

	// 둘 다 있으면 전부 보인다.
	on := toolNames(withPerm, toolHints{DockerEnabled: true})
	for _, name := range dockerToolNames {
		if !slices.Contains(on, name) {
			t.Errorf("%s 가 보이지 않습니다", name)
		}
	}
}

// 목록에서 감추는 것은 편의이고, 실제 방어는 실행 시점이다.
//
// MCP 와 REST 는 이름으로 바로 부를 수 있다. 그 길에서 문이 없으면 문이 없는 것이다.
func TestDockerToolsRefuseWhenSwitchIsOff(t *testing.T) {
	e := newTestEnv(t)
	if e.srv.cfg.AllowDocker {
		t.Fatal("검사 서버가 도커를 켠 채로 떴습니다")
	}
	u := &model.User{
		ID: e.user.ID, Role: model.RoleSuperadmin, Status: model.UserActive,
		Perms: []model.Perm{model.PermDockerManage},
	}
	tc := &toolContext{ctx: context.Background(), srv: e.srv, user: u}

	registry := aiTools()
	for _, name := range dockerToolNames {
		def := registry[name]
		if def == nil {
			t.Fatalf("%s 툴이 등록되지 않았습니다", name)
		}
		var err error
		switch {
		case def.Run != nil:
			_, err = def.Run(tc, json.RawMessage(`{}`))
		default:
			_, _, err = def.Propose(tc, json.RawMessage(`{}`))
		}
		if err == nil || !strings.Contains(err.Error(), "꺼져 있습니다") {
			t.Errorf("%s: 스위치가 꺼졌는데 막지 않았습니다 (%v)", name, err)
		}
	}
}

// 카탈로그는 사람이 정하지 않는 칸을 내보내지 않는다.
//
// 숨긴 칸은 우리가 같은 값을 두 길로 보내는 자리다(Redis 의 헬스체크용
// 환경변수). 모델이 그것까지 정하려 들면 두 값이 엇갈린다.
func TestDescribeDBCatalogHidesInternalFields(t *testing.T) {
	e := newTestEnv(t)
	e.srv.cfg.AllowDocker = true
	u := &model.User{
		ID: e.user.ID, Role: model.RoleSuperadmin, Status: model.UserActive,
		Perms: []model.Perm{model.PermDockerManage},
	}
	tc := &toolContext{ctx: context.Background(), srv: e.srv, user: u}

	out, err := toolDescribeDBCatalog(tc, json.RawMessage(`{"kind":"redis"}`))
	if err != nil {
		t.Fatalf("%v", err)
	}
	var res struct {
		Recipes []struct {
			ID     string `json:"id"`
			Fields []struct {
				Key      string `json:"key"`
				Secret   bool   `json:"secret"`
				Required bool   `json:"required"`
			} `json:"fields"`
		} `json:"recipes"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("결과가 JSON 이 아닙니다: %v", err)
	}
	if len(res.Recipes) != 1 || res.Recipes[0].ID != "redis" {
		t.Fatalf("%+v", res.Recipes)
	}
	// Redis 의 비밀번호는 두 길로 간다(인자 + 헬스체크용 환경변수). 칸은 하나여야 한다.
	count := 0
	for _, f := range res.Recipes[0].Fields {
		if f.Key == "password" {
			count++
			if !f.Secret || !f.Required {
				t.Errorf("비밀번호 칸의 표시가 잘못됐습니다: %+v", f)
			}
		}
	}
	if count != 1 {
		t.Errorf("비밀번호 칸이 %d개입니다 (숨긴 칸이 새어 나왔습니다)", count)
	}

	// 모르는 종류는 이유와 함께 거절한다.
	if _, err := toolDescribeDBCatalog(tc, json.RawMessage(`{"kind":"oracle"}`)); err == nil ||
		!strings.Contains(err.Error(), "이미지") {
		t.Errorf("오라클을 왜 못 만드는지 말하지 않습니다: %v", err)
	}
}

// 만들기 제안에 비밀번호가 실려서는 안 된다.
//
// 승인하는 사람이 무엇을 만드는지는 봐야 하지만, 그 미리보기는 대화 기록에
// 남는다. 거기 비밀번호가 있으면 대화를 볼 수 있는 사람 모두가 그것을 본다.
func TestCreateDBContainerProposalHidesPassword(t *testing.T) {
	e := newTestEnv(t)
	e.srv.cfg.AllowDocker = true
	u := &model.User{
		ID: e.user.ID, Role: model.RoleSuperadmin, Status: model.UserActive,
		Perms: []model.Perm{model.PermDockerManage},
	}
	tc := &toolContext{ctx: context.Background(), srv: e.srv, user: u}

	args := json.RawMessage(`{"kind":"postgres","name":"pg1","project":"` + e.project.Name + `",
		"values":{"database":"appdb","password":"S3cret-도망간다"}}`)
	summary, preview, err := proposeCreateDBContainer(tc, args)
	if err != nil {
		t.Fatalf("%v", err)
	}
	if !strings.Contains(summary, "pg1") {
		t.Errorf("요약 = %q", summary)
	}
	blob, _ := json.Marshal(preview)
	if strings.Contains(string(blob), "S3cret-도망간다") {
		t.Errorf("미리보기에 비밀번호가 실렸습니다: %s", blob)
	}
	if !strings.Contains(string(blob), "가림") {
		t.Errorf("가렸다는 표시가 없습니다: %s", blob)
	}
	// 아무것도 만들어지지 않아야 한다. 제안은 보여주기만 하는 단계다.
	list, err := e.st.ListDBInstances(context.Background(), "")
	if err != nil {
		t.Fatalf("%v", err)
	}
	if len(list) != 0 {
		t.Errorf("제안 단계에서 %d개가 만들어졌습니다", len(list))
	}
}

// 칸 이름을 지어내면 어디를 봐야 하는지 알려 준다.
//
// 모델이 가장 흔히 하는 실수다. 그냥 "값이 잘못됐습니다"라고 하면 모델은 다른
// 이름을 또 지어낸다.
func TestCreateDBContainerPointsAtTheCatalog(t *testing.T) {
	e := newTestEnv(t)
	e.srv.cfg.AllowDocker = true
	u := &model.User{
		ID: e.user.ID, Role: model.RoleSuperadmin, Status: model.UserActive,
		Perms: []model.Perm{model.PermDockerManage},
	}
	tc := &toolContext{ctx: context.Background(), srv: e.srv, user: u}

	// 필수 칸(비밀번호)을 빠뜨렸다.
	args := json.RawMessage(`{"kind":"postgres","name":"pg1","project":"` + e.project.Name + `",
		"values":{"database":"appdb","maxConn":"100"}}`)
	if _, _, err := proposeCreateDBContainer(tc, args); err == nil ||
		!strings.Contains(err.Error(), "describe_db_catalog") {
		t.Errorf("카탈로그를 가리키지 않습니다: %v", err)
	}

	// 없는 프로젝트.
	args = json.RawMessage(`{"kind":"postgres","name":"pg1","project":"없는프로젝트",
		"values":{"password":"pw1234!"}}`)
	if _, _, err := proposeCreateDBContainer(tc, args); err == nil ||
		!strings.Contains(err.Error(), "프로젝트") {
		t.Errorf("없는 프로젝트를 받아들였습니다: %v", err)
	}
}

// 다른 프로젝트의 DB 는 툴로도 보이지 않는다.
//
// 여기서만 느슨하면 화면에서 막은 것을 어시스턴트로 우회할 수 있다.
func TestDockerToolsStayInsideProjects(t *testing.T) {
	e := newTestEnv(t)
	e.srv.cfg.AllowDocker = true
	ctx := context.Background()

	other, err := e.st.CreateProject(ctx, store.SaveProjectParams{Name: "남의것"})
	if err != nil {
		t.Fatalf("%v", err)
	}
	if _, err := e.st.CreateDBInstance(ctx, store.CreateDBInstanceParams{
		ProjectID: other.ID, Name: "secret-pg", Kind: "postgres",
		Image: "postgres:17-alpine", Version: "17-alpine",
		ContainerName: "dbstudio-secret-pg", Port: 5432,
	}); err != nil {
		t.Fatalf("%v", err)
	}

	// 참여하지 않은 멤버.
	member := &model.User{
		ID: "다른사람", Role: model.RoleMember, Status: model.UserActive,
		Perms: []model.Perm{model.PermDockerManage},
	}
	tc := &toolContext{ctx: ctx, srv: e.srv, user: member}

	out, err := toolListDBContainers(tc, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("%v", err)
	}
	if strings.Contains(out, "secret-pg") {
		t.Errorf("남의 프로젝트의 DB 가 목록에 보입니다: %s", out)
	}
	if _, err := tc.findDBInstance("secret-pg"); err == nil {
		t.Error("남의 프로젝트의 DB 를 이름으로 찾을 수 있습니다")
	}
}
