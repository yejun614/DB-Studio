package api

import (
	"context"
	"strings"
	"testing"

	"dbstudio/internal/provision"
	"dbstudio/internal/store"
)

// 비밀은 레시피가 정하는 대로 갈라져야 한다.
//
// 화면이 정하면 필드를 더할 때 한쪽만 고치게 되고, 그때 비밀번호가 평문 칸으로
// 들어간다 — 그 칸은 목록 응답에 그대로 실려 나간다.
func TestSplitSecretsFollowsRecipe(t *testing.T) {
	r := provision.Find("postgres")
	if r == nil {
		t.Fatal("레시피를 찾지 못했습니다")
	}
	plain, secret := splitSecrets(r, map[string]string{
		"database": "appdb", "username": "app", "password": "pw1234!",
		"hostPort": "0",
	})
	if secret["password"] != "pw1234!" {
		t.Errorf("비밀이 갈라지지 않았습니다: %v", secret)
	}
	if _, ok := plain["password"]; ok {
		t.Error("비밀번호가 평문 칸에 남아 있습니다")
	}
	if plain["database"] != "appdb" || plain["username"] != "app" {
		t.Errorf("평문 값이 빠졌습니다: %v", plain)
	}
	// 레시피가 모르는 열쇠는 평문이다. 비밀로 오해해 숨기면 화면이 자기가
	// 보낸 값을 다시 못 본다.
	plain, _ = splitSecrets(r, map[string]string{"모르는것": "x"})
	if plain["모르는것"] != "x" {
		t.Errorf("모르는 열쇠가 사라졌습니다: %v", plain)
	}
}

// 모든 레시피의 비밀 필드가 실제로 갈라지는지 본다.
//
// 하나씩 적어 두지 않는 이유: 레시피를 더할 때 이 검사도 함께 고쳐야 한다면
// 고치는 것을 잊고, 그러면 새 DB 의 비밀번호가 평문으로 저장된다.
func TestSplitSecretsCoversEveryRecipe(t *testing.T) {
	for _, r := range provision.Catalog() {
		vals := map[string]string{}
		for _, f := range r.Fields {
			vals[f.Key] = "값-" + f.Key
		}
		plain, secret := splitSecrets(r, vals)
		for _, f := range r.Fields {
			if !f.Secret {
				continue
			}
			if _, ok := plain[f.Key]; ok {
				t.Errorf("%s: %s 가 평문 칸에 있습니다", r.ID, f.Key)
			}
			if secret[f.Key] == "" {
				t.Errorf("%s: %s 가 비밀 칸에 없습니다", r.ID, f.Key)
			}
		}
	}
}

// 등록할 계정은 레시피가 안다.
//
// 사람이 정하는 것과 이미지가 정해 둔 것이 섞여 있어서, 이것이 어긋나면
// 커넥션은 만들어지는데 붙을 수 없다 — 그리고 그 실패는 "계정이 틀렸다"로
// 보여서 사람이 자기가 적은 값을 의심하게 된다.
func TestRecipeAdminAccount(t *testing.T) {
	cases := []struct {
		id   string
		vals map[string]string
		want string
	}{
		{"postgres", map[string]string{"username": "app"}, "app"},
		{"postgres", nil, "postgres"}, // 비우면 필드 기본값
		{"mysql", nil, "root"},
		{"mariadb", nil, "root"},
		{"mongodb", map[string]string{"username": "admin"}, "admin"},
		{"clickhouse", nil, "default"},
		{"mssql", nil, "sa"},
		{"redis", nil, ""}, // 계정이 없다
	}
	for _, tc := range cases {
		r := provision.Find(tc.id)
		if r == nil {
			t.Errorf("%s: 레시피가 없습니다", tc.id)
			continue
		}
		if got := r.AdminAccount(tc.vals); got != tc.want {
			t.Errorf("%s: 계정 = %q (기대 %q)", tc.id, got, tc.want)
		}
	}
}

// 붙을 주소는 우리가 어디에 있는지에 달려 있다.
//
// 컨테이너 안에서 127.0.0.1 은 우리 자신이다. 그 주소로 등록하면 "connection
// refused"만 남고, 그 메시지로는 주소가 틀렸다는 것을 알 수 없다.
func TestConnectTarget(t *testing.T) {
	in := &store.DBInstance{
		ContainerName: "dbstudio-pg1", Port: 5432, HostPort: 32768,
	}

	host, port, _ := connectTarget(in, false)
	if host != "127.0.0.1" || port != 32768 {
		t.Errorf("호스트에서 = %s:%d (기대 127.0.0.1:32768)", host, port)
	}

	host, port, note := connectTarget(in, true)
	if host != "dbstudio-pg1" || port != 5432 {
		t.Errorf("컨테이너에서 = %s:%d (기대 dbstudio-pg1:5432)", host, port)
	}
	// 메모에 네트워크 이름이 있어야 한다. 접속이 안 될 때 볼 곳이 그것뿐이다.
	if !strings.Contains(note, provision.NetworkName) {
		t.Errorf("메모에 네트워크 이름이 없습니다: %q", note)
	}
}

func TestVersionOf(t *testing.T) {
	cases := map[string]string{
		"postgres:17-alpine":                  "17-alpine",
		"clickhouse/clickhouse-server:24.8":   "24.8",
		"mcr.microsoft.com/mssql/server:2022": "2022",
		"redis":                               "latest",
	}
	for image, want := range cases {
		if got := versionOf(image); got != want {
			t.Errorf("%s → %q (기대 %q)", image, got, want)
		}
	}
}

// ---------- HTTP ----------

// 스위치가 꺼져 있으면 모든 길이 막혀야 한다.
//
// 라우트 하나만 열려 있어도 도커 소켓에 닿는 길이 생긴다. 목록으로 적어 두는
// 이유: 새 라우트를 더할 때 여기 한 줄을 더하게 되고, 더하지 않으면 이 검사가
// 그것을 잡지 못한다는 사실이 눈에 보인다.
func TestDockerRoutesClosedWhenDisabled(t *testing.T) {
	e := newTestEnv(t)
	c := e.client(t)
	if status, body := c.do("POST", "/api/v1/auth/login",
		map[string]string{"username": "alice", "password": testPassword}); status != 200 {
		t.Fatalf("로그인 = %d: %v", status, body)
	}
	if e.srv.cfg.AllowDocker {
		t.Fatal("검사 서버가 도커를 켠 채로 떴습니다")
	}

	paths := []struct{ method, path string }{
		{"GET", "/api/v1/docker/status"},
		{"GET", "/api/v1/docker/catalog"},
		{"POST", "/api/v1/docker/plan"},
		{"GET", "/api/v1/docker/instances"},
		{"POST", "/api/v1/docker/instances"},
		{"GET", "/api/v1/docker/instances/x"},
		{"POST", "/api/v1/docker/instances/x/start"},
		{"POST", "/api/v1/docker/instances/x/stop"},
		{"POST", "/api/v1/docker/instances/x/restart"},
		{"DELETE", "/api/v1/docker/instances/x"},
		{"GET", "/api/v1/docker/instances/x/logs"},
	}
	for _, p := range paths {
		status, body := c.do(p.method, p.path, map[string]string{})
		if status != 403 {
			t.Errorf("%s %s = %d (기대 403)", p.method, p.path, status)
			continue
		}
		// "도커가 꺼져 있다"라고 말해야 한다. "권한이 없다"로 답하면 사람은
		// 권한을 달라고 요청하고, 받아도 아무것도 되지 않는다.
		if body["error"] != "docker_disabled" {
			t.Errorf("%s %s → %v (기대 docker_disabled)", p.method, p.path, body["error"])
		}
	}
}

// 스위치를 켜면 카탈로그와 미리보기가 열린다. 아무것도 만들지 않는다.
func TestDockerCatalogAndPlan(t *testing.T) {
	e := newTestEnv(t)
	e.srv.cfg.AllowDocker = true
	c := e.client(t)
	if status, _ := c.do("POST", "/api/v1/auth/login",
		map[string]string{"username": "alice", "password": testPassword}); status != 200 {
		t.Fatal("로그인 실패")
	}

	status, body := c.do("GET", "/api/v1/docker/catalog", nil)
	if status != 200 {
		t.Fatalf("카탈로그 = %d: %v", status, body)
	}
	recipes, _ := body["recipes"].([]any)
	if len(recipes) < 5 {
		t.Errorf("레시피가 %d개입니다", len(recipes))
	}
	// 목록에 없는 것도 이유와 함께 와야 한다.
	if un, _ := body["unsupported"].(map[string]any); un["oracle"] == nil {
		t.Errorf("못 만드는 것의 이유가 없습니다: %v", body["unsupported"])
	}
	// 카탈로그에 비밀 값이 실려서는 안 된다. 기본값 칸에 비밀번호를 넣어 두면
	// 그것이 곧 모두가 아는 비밀번호가 된다.
	for _, raw := range recipes {
		r, _ := raw.(map[string]any)
		fields, _ := r["fields"].([]any)
		for _, fraw := range fields {
			f, _ := fraw.(map[string]any)
			if f["secret"] == true && f["default"] != nil && f["default"] != "" {
				t.Errorf("%v: 비밀 필드에 기본값이 있습니다 (%v)", r["id"], f["key"])
			}
		}
	}

	status, body = c.do("POST", "/api/v1/docker/plan", map[string]any{
		"kind": "postgres", "name": "pg1",
		"values": map[string]string{"database": "appdb", "password": "pw1234!"},
	})
	if status != 200 {
		t.Fatalf("미리보기 = %d: %v", status, body)
	}
	plan, _ := body["plan"].(map[string]any)
	if plan["container"] != "dbstudio-pg1" {
		t.Errorf("컨테이너 이름 = %v", plan["container"])
	}
	if !strings.HasPrefix(plan["image"].(string), "postgres:") {
		t.Errorf("이미지 = %v", plan["image"])
	}

	// 이름이 잘못되면 400 이어야 한다. 이 이름이 컨테이너·볼륨 이름이 된다.
	status, body = c.do("POST", "/api/v1/docker/plan", map[string]any{
		"kind": "postgres", "name": "../탈출",
		"values": map[string]string{"database": "appdb", "password": "pw"},
	})
	if status != 400 || body["error"] != "invalid_plan" {
		t.Errorf("잘못된 이름 = %d %v", status, body["error"])
	}
}

// 만들기는 프로젝트를 반드시 받아야 한다.
//
// 프로젝트 밖에서 만든 DB 는 커넥션이 될 수 없고(ErrNoProject), 목록에도
// 권한 판정에도 나타나지 않는다 — 만든 사람에게도 보이지 않는 유령이 된다.
func TestCreateDBInstanceNeedsProject(t *testing.T) {
	e := newTestEnv(t)
	e.srv.cfg.AllowDocker = true
	c := e.client(t)
	if status, _ := c.do("POST", "/api/v1/auth/login",
		map[string]string{"username": "alice", "password": testPassword}); status != 200 {
		t.Fatal("로그인 실패")
	}

	status, _ := c.do("POST", "/api/v1/docker/instances", map[string]any{
		"kind": "postgres", "name": "pg1",
		"values": map[string]string{"database": "appdb", "password": "pw1234!"},
	})
	if status != 400 {
		t.Errorf("프로젝트 없이 = %d (기대 400)", status)
	}
	status, _ = c.do("POST", "/api/v1/docker/instances", map[string]any{
		"projectId": "없는프로젝트", "kind": "postgres", "name": "pg1",
		"values": map[string]string{"database": "appdb", "password": "pw1234!"},
	})
	if status != 404 {
		t.Errorf("없는 프로젝트로 = %d (기대 404)", status)
	}
}

// 목록은 프로젝트 밖으로 나가지 않는다.
func TestListDBInstancesStaysInProject(t *testing.T) {
	e := newTestEnv(t)
	e.srv.cfg.AllowDocker = true
	c := e.client(t)
	if status, _ := c.do("POST", "/api/v1/auth/login",
		map[string]string{"username": "alice", "password": testPassword}); status != 200 {
		t.Fatal("로그인 실패")
	}

	// 다른 프로젝트에 인스턴스를 하나 만든다. 슈퍼 어드민이라도 ?project= 로
	// 좁히면 그 프로젝트의 것만 보여야 한다.
	ctx := context.Background()
	other, err := e.st.CreateProject(ctx, store.SaveProjectParams{Name: "다른곳"})
	if err != nil {
		t.Fatalf("프로젝트: %v", err)
	}
	if _, err := e.st.CreateDBInstance(ctx, store.CreateDBInstanceParams{
		ProjectID: other.ID, Name: "pg-other", Kind: "postgres",
		Image: "postgres:17-alpine", Version: "17-alpine",
		ContainerName: "dbstudio-pg-other", Port: 5432,
	}); err != nil {
		t.Fatalf("인스턴스: %v", err)
	}

	status, body := c.do("GET", "/api/v1/docker/instances?project="+e.project.ID, nil)
	if status != 200 {
		t.Fatalf("목록 = %d: %v", status, body)
	}
	items, _ := body["items"].([]any)
	if len(items) != 0 {
		t.Errorf("빈 프로젝트에서 %d개가 보입니다", len(items))
	}
}
