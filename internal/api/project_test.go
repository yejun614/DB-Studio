package api

import (
	"context"
	"testing"

	"dbstudio/internal/crypto"
	"dbstudio/internal/model"
	"dbstudio/internal/store"
)

// mkUserRole은 주어진 역할의 계정을 만든다(프로젝트에는 넣지 않는다).
func mkUserRole(t *testing.T, e *testEnv, name string, role model.Role) *model.User {
	t.Helper()
	hash, err := crypto.HashPassword(testPassword)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	u, err := e.st.CreateUser(context.Background(), store.CreateUserParams{
		Username: name, DisplayName: name, Role: role, PasswordHash: hash,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	return u
}

// 목록은 참여한 것만 보여준다(슈퍼 어드민은 전부).
//
// 보이지만 열면 거부되는 줄은 권한 설정이 잘못된 것처럼 보인다. 프로젝트 이름
// 자체가 남에게 알려서는 안 되는 것일 수도 있다.
func TestProjectListIsScopedToMembership(t *testing.T) {
	e := newTestEnv(t)
	alice := loginAs(t, e, "alice") // 슈퍼 어드민, 시험용 프로젝트의 참여자

	if code, body := alice.do("POST", "/api/v1/projects/",
		map[string]any{"name": "물류"}); code != 201 {
		t.Fatalf("프로젝트 생성 = %d: %v", code, body)
	}

	mkUserRole(t, e, "dana", model.RoleMember)
	e.join(t, mustUserID(t, e, "dana"))
	dana := loginAs(t, e, "dana")

	_, body := dana.do("GET", "/api/v1/projects/", nil)
	list, _ := body["projects"].([]any)
	if len(list) != 1 {
		t.Fatalf("참여자가 본 프로젝트 = %d개, 1개여야 합니다: %v", len(list), body)
	}
	if body["canManage"] != false {
		t.Errorf("일반 사용자에게 canManage = %v", body["canManage"])
	}

	_, all := alice.do("GET", "/api/v1/projects/", nil)
	if got, _ := all["projects"].([]any); len(got) != 2 {
		t.Errorf("슈퍼 어드민이 본 프로젝트 = %d개, 2개여야 합니다", len(got))
	}

	// 일반 사용자는 프로젝트를 만들 수 없다. 프로젝트를 만드는 일은 곧 그 안에
	// DB를 등록하겠다는 뜻이고, 그것은 커넥션 관리자의 일이다.
	if code, _ := dana.do("POST", "/api/v1/projects/", map[string]any{"name": "몰래"}); code != 403 {
		t.Errorf("일반 사용자 생성 = %d, 403이어야 합니다", code)
	}
}

// 참여하지 않은 프로젝트는 있는지조차 알려주지 않는다.
func TestForeignProjectLooksMissing(t *testing.T) {
	e := newTestEnv(t)
	alice := loginAs(t, e, "alice")
	_, body := alice.do("POST", "/api/v1/projects/", map[string]any{"name": "이직-검토"})
	made, _ := body["project"].(map[string]any)
	secret, _ := made["id"].(string)

	mkUserRole(t, e, "dana", model.RoleMember)
	e.join(t, mustUserID(t, e, "dana"))
	dana := loginAs(t, e, "dana")

	// 403이면 "그 자리에 무언가 있다"는 사실을 알려 주게 된다.
	if code, got := dana.do("GET", "/api/v1/projects/"+secret, nil); code != 404 {
		t.Errorf("남의 프로젝트 조회 = %d, 404여야 합니다: %v", code, got)
	}
}

// 커넥션 관리자 예외도 프로젝트 밖으로는 나가지 않는다.
//
// 목록 화면은 관리 편의를 위해 접근 등급이 없는 DB도 보여준다. 그 예외가 프로젝트를
// 넘어가면 참여하지 않은 팀의 DB 이름이 그대로 뜨고, 프로젝트를 나눈 이유가 목록
// 한 곳에서 무너진다.
func TestConnectionListStaysInsideProjects(t *testing.T) {
	e, conn, _ := assignEnv(t)

	// 어드민이지만 이 프로젝트의 참여자는 아니다.
	mkUserRole(t, e, "carol", model.RoleAdmin)
	carol := loginAs(t, e, "carol")
	_, body := carol.do("GET", "/api/v1/connections/", nil)
	if items, _ := body["items"].([]any); len(items) != 0 {
		t.Errorf("참여하지 않은 프로젝트의 DB가 관리자에게 보입니다: %v", items)
	}

	// 참여하면 보인다.
	e.join(t, mustUserID(t, e, "carol"))
	_, body = carol.do("GET", "/api/v1/connections/", nil)
	items, _ := body["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("참여 뒤 DB = %d개, 1개여야 합니다", len(items))
	}
	first, _ := items[0].(map[string]any)
	got, _ := first["connection"].(map[string]any)
	if got["id"] != conn.ID {
		t.Errorf("다른 DB가 왔습니다: %v", got["id"])
	}

	// ?project= 로 좁히면 그 프로젝트만 남는다.
	_, other := carol.do("GET", "/api/v1/connections/?project=없는-프로젝트", nil)
	if items, _ := other["items"].([]any); len(items) != 0 {
		t.Errorf("없는 프로젝트로 좁혔는데 %d개가 왔습니다", len(items))
	}
}

// 커넥션은 프로젝트 없이 만들 수 없고, 참여하지 않은 프로젝트에도 만들 수 없다.
//
// 어디에도 속하지 않은 커넥션은 목록에도 권한 판정에도 나타나지 않는다 — 만들 수는
// 있는데 아무도 볼 수 없는 유령이 된다.
func TestConnectionNeedsAProjectYouCanSee(t *testing.T) {
	e := newTestEnv(t)
	alice := loginAs(t, e, "alice")
	_, body := alice.do("POST", "/api/v1/projects/", map[string]any{"name": "물류"})
	made, _ := body["project"].(map[string]any)
	otherProject, _ := made["id"].(string)

	mkUserRole(t, e, "carol", model.RoleAdmin)
	e.join(t, mustUserID(t, e, "carol"))
	carol := loginAs(t, e, "carol")

	base := map[string]any{
		"name": "새 DB", "kind": "postgres", "environment": "dev",
		"host": "127.0.0.1", "port": 5432, "databaseName": "appdb",
		"username": "app", "password": "pw",
	}
	no := map[string]any{}
	for k, v := range base {
		no[k] = v
	}
	if code, got := carol.do("POST", "/api/v1/connections/", no); code != 400 {
		t.Errorf("프로젝트 없는 생성 = %d, 400이어야 합니다: %v", code, got)
	}

	foreign := map[string]any{"projectId": otherProject}
	for k, v := range base {
		foreign[k] = v
	}
	if code, got := carol.do("POST", "/api/v1/connections/", foreign); code != 404 {
		t.Errorf("남의 프로젝트에 생성 = %d, 404여야 합니다: %v", code, got)
	}
}

// 자원이 든 프로젝트는 지우지 않는다.
//
// 프로젝트 삭제 단추 하나로 DB 열 개와 그 아래 ERD·마이그레이션이 한꺼번에
// 사라진다면, 그것은 무엇을 지우는지 말할 수 없는 단추다.
func TestProjectWithResourcesIsNotDeleted(t *testing.T) {
	e, conn, _ := assignEnv(t)
	alice := loginAs(t, e, "alice")

	code, body := alice.do("DELETE", "/api/v1/projects/"+e.project.ID, nil)
	if code != 409 {
		t.Fatalf("자원이 든 프로젝트 삭제 = %d, 409여야 합니다: %v", code, body)
	}
	if body["error"] != "project_in_use" {
		t.Errorf("사유 = %v", body["error"])
	}

	if code, _ := alice.do("DELETE", "/api/v1/connections/"+conn.ID, nil); code != 200 {
		t.Fatalf("커넥션 삭제 실패")
	}
	// 서버도 프로젝트 안에 있다. 남아 있으면 아직 빈 프로젝트가 아니다.
	if code, body := alice.do("DELETE", "/api/v1/projects/"+e.project.ID, nil); code != 409 {
		t.Errorf("서버가 남았는데 삭제 = %d: %v", code, body)
	}
	if code, _ := alice.do("DELETE", "/api/v1/servers/"+conn.ServerID, nil); code != 200 {
		t.Fatalf("서버 삭제 실패")
	}
	if code, body := alice.do("DELETE", "/api/v1/projects/"+e.project.ID, nil); code != 200 {
		t.Errorf("빈 프로젝트 삭제 = %d: %v", code, body)
	}
}

// 사전은 프로젝트마다 다르다.
//
// 같은 말이 프로젝트마다 다른 것을 가리킬 수 있다("주문"이 한쪽에서는 장바구니,
// 다른 쪽에서는 배송 지시서). 앱 하나에 사전이 하나뿐이면 둘 중 하나는 적지 못한다.
func TestGlossaryIsPerProject(t *testing.T) {
	e := newTestEnv(t)
	alice := loginAs(t, e, "alice")
	_, body := alice.do("POST", "/api/v1/projects/", map[string]any{"name": "물류"})
	made, _ := body["project"].(map[string]any)
	second, _ := made["id"].(string)

	for _, pid := range []string{e.project.ID, second} {
		if code, got := alice.do("POST", "/api/v1/glossary/", map[string]any{
			"projectId": pid, "term": "주문", "physical": "order",
		}); code != 201 {
			t.Fatalf("%s 에 추가 = %d: %v", pid, code, got)
		}
	}

	_, first := alice.do("GET", "/api/v1/glossary/?project="+e.project.ID, nil)
	if terms, _ := first["terms"].([]any); len(terms) != 1 {
		t.Errorf("첫 프로젝트의 용어 = %d개, 1개여야 합니다", len(terms))
	}

	// 참여하지 않은 프로젝트의 사전은 열리지 않는다.
	mkUserRole(t, e, "dana", model.RoleMember)
	e.join(t, mustUserID(t, e, "dana"))
	dana := loginAs(t, e, "dana")
	if code, _ := dana.do("GET", "/api/v1/glossary/?project="+second, nil); code != 404 {
		t.Errorf("남의 사전 조회 = %d, 404여야 합니다", code)
	}
}

// 독립 ERD 초안은 프로젝트가 유일한 울타리다.
func TestStandaloneERDStaysInProject(t *testing.T) {
	e := newTestEnv(t)
	alice := loginAs(t, e, "alice")
	docID := createStandalone(t, e, alice, "설계", "postgres")

	_, body := alice.do("POST", "/api/v1/projects/", map[string]any{"name": "물류"})
	made, _ := body["project"].(map[string]any)
	second, _ := made["id"].(string)

	// 다른 프로젝트에만 참여한 사람에게는 보이지 않는다.
	dana := mkUserRole(t, e, "dana", model.RoleMember)
	if err := e.st.SetProjectMembers(context.Background(), second, []string{dana.ID}); err != nil {
		t.Fatalf("참여자 설정: %v", err)
	}
	danaClient := loginAs(t, e, "dana")

	if code, got := danaClient.do("GET", "/api/v1/erd/documents/"+docID, nil); code != 404 {
		t.Errorf("남의 초안 열기 = %d, 404여야 합니다: %v", code, got)
	}
	_, list := danaClient.do("GET", "/api/v1/erd/documents/", nil)
	if items, _ := list["items"].([]any); len(items) != 0 {
		t.Errorf("남의 초안이 목록에 %d건 보입니다", len(items))
	}
}

func mustUserID(t *testing.T, e *testEnv, name string) string {
	t.Helper()
	users, err := e.st.ListUsers(context.Background())
	if err != nil {
		t.Fatalf("list users: %v", err)
	}
	for _, u := range users {
		if u.Username == name {
			return u.ID
		}
	}
	t.Fatalf("사용자 %s 를 찾을 수 없습니다", name)
	return ""
}

// 서버도 프로젝트 안에 있다.
//
// 접속 정보와 자격증명이 서버에 붙어 있어서 프로젝트마다 따로 등록한다. 남의
// 프로젝트 서버가 목록에 뜨면 호스트 이름과 계정이 그대로 노출된다.
func TestServersAreScopedToProject(t *testing.T) {
	e, conn, _ := assignEnv(t)
	alice := loginAs(t, e, "alice")

	_, body := alice.do("POST", "/api/v1/projects/", map[string]any{"name": "물류"})
	made, _ := body["project"].(map[string]any)
	second, _ := made["id"].(string)

	// 새 프로젝트에는 서버가 하나도 없다.
	_, empty := alice.do("GET", "/api/v1/servers/?project="+second, nil)
	if items, _ := empty["items"].([]any); len(items) != 0 {
		t.Errorf("새 프로젝트에 서버가 %d개 보입니다", len(items))
	}
	// 원래 프로젝트에는 있다.
	_, mine := alice.do("GET", "/api/v1/servers/?project="+e.project.ID, nil)
	if items, _ := mine["items"].([]any); len(items) != 1 {
		t.Fatalf("원래 프로젝트의 서버 = %d개, 1개여야 합니다", len(items))
	}

	// 같은 이름의 서버를 다른 프로젝트에 만들 수 있다.
	code, got := alice.do("POST", "/api/v1/servers", map[string]any{
		"projectId": second, "name": "pg", "kind": "postgres",
		"host": "10.0.0.9", "port": 5432, "defaultEnvironment": "dev",
	})
	if code != 201 {
		t.Fatalf("다른 프로젝트에 같은 이름 = %d: %v", code, got)
	}

	// 프로젝트 없이는 만들 수 없다.
	if code, _ := alice.do("POST", "/api/v1/servers", map[string]any{
		"name": "떠도는", "kind": "postgres", "host": "h", "port": 5432,
		"defaultEnvironment": "dev",
	}); code != 400 {
		t.Errorf("프로젝트 없는 서버 생성 = %d, 400이어야 합니다", code)
	}

	// 서버의 프로젝트와 다른 곳에 DB를 붙일 수 없다. 근거가 둘이면 어긋난다.
	if code, got := alice.do("POST", "/api/v1/connections/", map[string]any{
		"projectId": second, "serverId": conn.ServerID, "name": "남의 서버에",
		"environment": "dev", "databaseName": "other",
	}); code != 400 || got["error"] != "project_mismatch" {
		t.Errorf("다른 프로젝트 서버에 DB = %d %v", code, got["error"])
	}

	// 참여하지 않은 사람에게는 서버가 있는지조차 알려주지 않는다.
	mkUserRole(t, e, "dana", model.RoleMember)
	e.join(t, mustUserID(t, e, "dana"))
	dana := loginAs(t, e, "dana")
	if code, _ := dana.do("GET", "/api/v1/servers/"+conn.ServerID, nil); code != 200 {
		t.Errorf("참여자가 서버를 못 봅니다: %d", code)
	}
	_, danaList := dana.do("GET", "/api/v1/servers/?project="+second, nil)
	if items, _ := danaList["items"].([]any); len(items) != 0 {
		t.Errorf("참여하지 않은 프로젝트의 서버가 %d개 보입니다", len(items))
	}
}
