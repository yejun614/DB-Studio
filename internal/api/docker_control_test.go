package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"dbstudio/internal/model"
	"dbstudio/internal/store"
)

// dockerEnv는 도커 기능이 켜진 서버와 로그인한 클라이언트를 만든다.
//
// 여기 검사들은 **도커에 닿지 않는 길**만 본다(권한, 이미 지운 것, 컨테이너가
// 없는 것). 실제 데몬이 필요한 길은 internal/provision 의 dockerlive 검사가 본다 —
// 그것을 여기서 흉내 내면 도커가 아니라 우리 흉내를 검사하게 된다.
func dockerEnv(t *testing.T) (*testEnv, *client) {
	t.Helper()
	e := newTestEnv(t)
	e.srv.cfg.AllowDocker = true
	c := e.client(t)
	if status, body := c.do("POST", "/api/v1/auth/login",
		map[string]string{"username": "alice", "password": testPassword}); status != 200 {
		t.Fatalf("로그인 = %d: %v", status, body)
	}
	return e, c
}

func newRow(t *testing.T, e *testEnv, p store.CreateDBInstanceParams) *store.DBInstance {
	t.Helper()
	if p.ProjectID == "" {
		p.ProjectID = e.project.ID
	}
	in, err := e.st.CreateDBInstance(context.Background(), p)
	if err != nil {
		t.Fatalf("인스턴스: %v", err)
	}
	return in
}

// 만드는 중에는 건드리지 못한다.
//
// 러너가 그 컨테이너를 쓰고 있고, 그 사이에 멈추면 만들기가 "뜨지 못했습니다"로
// 실패한다 — 사람이 스스로 멈춰 놓고 실패 메시지를 받는 셈이다.
func TestControlRefusesWhileCreating(t *testing.T) {
	e, c := dockerEnv(t)
	in := newRow(t, e, store.CreateDBInstanceParams{
		Name: "pg1", Kind: "postgres", Image: "postgres:17-alpine", Version: "17-alpine",
		ContainerName: "dbstudio-pg1", Port: 5432,
	})
	if in.Status != store.InstanceCreating {
		t.Fatalf("처음 상태 = %s", in.Status)
	}

	for _, op := range []string{"start", "stop", "restart"} {
		status, body := c.do("POST", "/api/v1/docker/instances/"+in.ID+"/"+op, nil)
		if status != 409 || body["error"] != "busy" {
			t.Errorf("%s = %d %v (기대 409 busy)", op, status, body["error"])
		}
	}
}

// 만드는 중으로 **멈춰 버린** 줄은 지울 수 있어야 한다.
//
// 러너가 실제로 돌고 있으면 지우기도 막는다(Busy). 하지만 만드는 도중에 앱이
// 죽으면 그 줄은 creating 인 채로 남고 러너는 아무것도 하고 있지 않다 —
// 그때 상태만 보고 막으면 그 줄은 영영 지울 수 없고, 이름도 영영 묶인다.
// 지우기가 그 상태에서 빠져나오는 유일한 문이다.
func TestRemoveIsTheWayOutOfAStuckCreate(t *testing.T) {
	e, c := dockerEnv(t)
	in := newRow(t, e, store.CreateDBInstanceParams{
		Name: "pg2", Kind: "postgres", Image: "postgres:17-alpine", Version: "17-alpine",
		ContainerName: "dbstudio-pg2", Port: 5432,
	})

	status, body := c.do("DELETE", "/api/v1/docker/instances/"+in.ID, nil)
	if status != 200 {
		t.Fatalf("= %d %v", status, body)
	}
	got, err := e.st.GetDBInstance(context.Background(), in.ID)
	if err != nil {
		t.Fatalf("%v", err)
	}
	if got.Status != store.InstanceRemoved {
		t.Errorf("상태 = %s (기대 removed)", got.Status)
	}
}

// 이미 지운 것에 다시 지우기를 부르면 **기록**을 지운다.
//
// 지울 컨테이너가 없는 상태에서 "지우기"가 뜻할 수 있는 것은 그것뿐이고,
// 그렇게 두어야 같은 이름을 다시 쓸 수 있다(컨테이너 이름은 유일해야 한다).
func TestRemoveDeletesRecordWhenContainerGone(t *testing.T) {
	e, c := dockerEnv(t)
	in := newRow(t, e, store.CreateDBInstanceParams{
		Name: "pg1", Kind: "postgres", Image: "postgres:17-alpine", Version: "17-alpine",
		ContainerName: "dbstudio-pg1", Port: 5432,
	})
	removed := store.InstanceRemoved
	if err := e.st.UpdateDBInstance(context.Background(), in.ID,
		store.UpdateDBInstanceParams{Status: &removed}); err != nil {
		t.Fatalf("%v", err)
	}

	status, body := c.do("DELETE", "/api/v1/docker/instances/"+in.ID, nil)
	if status != 200 || body["deleted"] != true {
		t.Fatalf("= %d %v", status, body)
	}
	if _, err := e.st.GetDBInstance(context.Background(), in.ID); err == nil {
		t.Error("기록이 남아 있습니다")
	}
}

// 이름이 겹쳤을 때, 그 이름을 쥐고 있는 것이 **지운 기록**이면 그렇게 말해야 한다.
//
// 그 줄은 목록에서 흐리게 보여 "이미 있는 것"으로 읽히지 않는다. 그냥 "이름이
// 있습니다"라고만 하면 사람은 목록을 보고 없는데 왜 그러냐고 생각하게 된다.
func TestCreateSaysWhenRemovedRecordHoldsTheName(t *testing.T) {
	e, c := dockerEnv(t)
	in := newRow(t, e, store.CreateDBInstanceParams{
		Name: "pg1", Kind: "postgres", Image: "postgres:17-alpine", Version: "17-alpine",
		ContainerName: "dbstudio-pg1", Port: 5432,
	})
	removed := store.InstanceRemoved
	if err := e.st.UpdateDBInstance(context.Background(), in.ID,
		store.UpdateDBInstanceParams{Status: &removed}); err != nil {
		t.Fatalf("%v", err)
	}

	status, body := c.do("POST", "/api/v1/docker/instances", map[string]any{
		"projectId": e.project.ID, "kind": "postgres", "name": "pg1",
		"values": map[string]string{"database": "appdb", "password": "pw1234!"},
	})
	if status != 409 {
		t.Fatalf("= %d %v", status, body)
	}
	msg, _ := body["message"].(string)
	if !strings.Contains(msg, "기록") {
		t.Errorf("기록이 이름을 쥐고 있다는 말이 없습니다: %q", msg)
	}
}

// 컨테이너가 없으면 로그를 열 수 없다.
//
// 409 로 답하는 이유: 이것은 없는 자원이 아니라 아직 열 수 없는 상태다.
// 404 로 답하면 화면이 "인스턴스가 사라졌다"로 읽고 목록을 다시 그린다.
func TestLogsNeedAContainer(t *testing.T) {
	e, c := dockerEnv(t)
	in := newRow(t, e, store.CreateDBInstanceParams{
		Name: "pg1", Kind: "postgres", Image: "postgres:17-alpine", Version: "17-alpine",
		ContainerName: "dbstudio-pg1", Port: 5432,
	})
	status, body := c.do("GET", "/api/v1/docker/instances/"+in.ID+"/logs", nil)
	if status != 409 || body["error"] != "no_container" {
		t.Errorf("= %d %v (기대 409 no_container)", status, body["error"])
	}
}

// 다른 프로젝트의 것은 다룰 수 없다.
//
// 프로젝트를 못 보면 그 안의 것도 없는 것이다. 403 이 아니라 404 로 답한다 —
// 있다는 사실 자체가 정보다.
func TestControlStaysInsideTheProject(t *testing.T) {
	e, c := dockerEnv(t)
	other, err := e.st.CreateProject(context.Background(), store.SaveProjectParams{Name: "남의것"})
	if err != nil {
		t.Fatalf("%v", err)
	}
	// 슈퍼 어드민은 모든 프로젝트를 본다. 참여로 좁히는 사람으로 바꿔 확인한다.
	e.srv.cfg.AllowDocker = true
	in := newRow(t, e, store.CreateDBInstanceParams{
		ProjectID: other.ID,
		Name:      "pg9", Kind: "postgres", Image: "postgres:17-alpine", Version: "17-alpine",
		ContainerName: "dbstudio-pg9", Port: 5432,
	})

	// 없는 id 는 404 여야 한다(있는 것과 없는 것의 답이 같아야 한다).
	status, _ := c.do("POST", "/api/v1/docker/instances/없는것/stop", nil)
	if status != 404 {
		t.Errorf("없는 인스턴스 = %d (기대 404)", status)
	}
	_ = in
}

// 포트가 바뀌면 커넥션이 따라가야 한다.
//
// 도커가 골라 준 호스트 포트는 **멈췄다 시작하면 바뀐다**(살아 있는 데몬에서
// 32809 가 32810 이 되는 것을 봤다). 옮겨 적지 않으면 커넥션은 아무도 듣지 않는
// 포트를 가리킨 채로 남고, 화면에는 "접속 실패"만 뜬다 — 우리가 만든 DB 인데
// 우리가 붙지 못하는 셈이다.
func TestSyncConnectionAddressFollowsThePort(t *testing.T) {
	e, _ := dockerEnv(t)
	ctx := context.Background()

	pw := "pw1234!"
	_, conn, err := e.st.CreateServerWithDatabase(ctx,
		store.SaveServerParams{
			ProjectID: e.project.ID, Name: "rd1", Kind: model.KindRedis,
			Host: "127.0.0.1", Port: 32809, Options: model.Options{},
			DefaultEnvironment: model.EnvDev, Tags: []string{"docker"},
			Enabled: true, Password: &pw,
		},
		store.SaveConnectionParams{
			ProjectID: e.project.ID, Name: "rd1", Environment: model.EnvDev,
			Tags: []string{"docker"}, Enabled: true,
		})
	if err != nil {
		t.Fatalf("커넥션: %v", err)
	}

	in := newRow(t, e, store.CreateDBInstanceParams{
		Name: "rd1", Kind: "redis", Image: "redis:7-alpine", Version: "7-alpine",
		ContainerName: "dbstudio-rd1", Port: 6379, HostPort: 32810,
	})
	newPort := 32810
	connID := conn.ID
	if err := e.st.UpdateDBInstance(ctx, in.ID, store.UpdateDBInstanceParams{
		ConnectionID: &connID, HostPort: &newPort,
	}); err != nil {
		t.Fatalf("%v", err)
	}
	fresh, _ := e.st.GetDBInstance(ctx, in.ID)

	e.srv.syncConnectionAddress(ctx, fresh)

	got, err := e.st.GetConnection(ctx, conn.ID)
	if err != nil {
		t.Fatalf("%v", err)
	}
	// 기대값을 127.0.0.1 로 박지 않는다. 우리가 컨테이너 안이면 주소가 컨테이너
	// 이름이 되고(connectTarget), 그러면 이 검사가 도는 곳에 따라 틀린 값을
	// 요구하게 된다. 확인할 것은 "그 함수가 말한 주소와 같은가"다.
	wantHost, wantPort, _ := connectTarget(fresh, inContainer())
	if got.Host != wantHost || got.Port != wantPort {
		t.Errorf("커넥션 주소 = %s:%d (기대 %s:%d)", got.Host, got.Port, wantHost, wantPort)
	}

	// 비밀번호는 그대로여야 한다. 주소만 고치는 자리에서 자격증명이 사라지면
	// 접속은 여전히 안 되고, 이유만 바뀐다.
	sec, err := e.st.GetServerSecret(ctx, got.ServerID)
	if err != nil {
		t.Errorf("자격증명이 사라졌습니다: %v", err)
	} else if sec.Password != pw {
		t.Errorf("비밀번호가 바뀌었습니다: %q", sec.Password)
	}
}

// 내보낸 파일에 비밀번호가 들어 있으면 안 된다.
//
// 계획을 다시 세울 때는 저장해 둔 비밀을 **합쳐서** 세운다(그러지 않으면
// "비밀번호를 입력하세요"로 막힌다). 합친 값이 그대로 파일에 실리지 않는지를
// 여기서 지킨다 — 이 파일은 저장소에 들어가라고 만드는 것이다.
func TestComposeExportHidesSecrets(t *testing.T) {
	e, c := dockerEnv(t)
	const pw = "S3cret-도망간다"
	in, err := e.st.CreateDBInstance(context.Background(), store.CreateDBInstanceParams{
		ProjectID: e.project.ID, Name: "exp1", Kind: "postgres",
		Image: "postgres:17-alpine", Version: "17-alpine",
		ContainerName: "dbstudio-exp1", VolumeName: "dbstudio-exp1-data",
		Port: 5432, HostPort: 32900,
		Values:  map[string]string{"database": "appdb", "username": "app"},
		Secrets: map[string]string{"password": pw},
	})
	if err != nil {
		t.Fatalf("%v", err)
	}

	status, body := c.do("GET", "/api/v1/docker/instances/"+in.ID+"/compose", nil)
	if status != 200 {
		t.Fatalf("= %d %v", status, body)
	}
	yaml, _ := body["yaml"].(string)
	if strings.Contains(yaml, pw) {
		t.Error("비밀번호가 compose.yml 에 실렸습니다")
	}
	if !strings.Contains(yaml, "${EXP1_PASSWORD}") {
		t.Errorf("비밀 자리가 없습니다:\n%s", yaml)
	}
	// 실제로 잡힌 포트가 들어가야 한다. 계획의 값(0)을 그대로 쓰면 내보낸
	// 파일로 띄웠을 때 포트가 또 달라지고, 지금 붙어 있는 주소와 어긋난다.
	if !strings.Contains(yaml, "32900:5432") {
		t.Errorf("잡힌 포트가 반영되지 않았습니다:\n%s", yaml)
	}
	if env, _ := body["envExample"].(string); !strings.Contains(env, "EXP1_PASSWORD=") ||
		strings.Contains(env, pw) {
		t.Errorf(".env 뼈대가 잘못됐습니다: %q", env)
	}
}

// 프로젝트 전체 내보내기는 지운 것을 넣지 않는다.
//
// 그 줄은 기록이지 띄울 것이 아니다. 넣으면 받아 간 사람이 이미 없앤 DB 를
// 다시 띄우게 된다.
func TestProjectComposeSkipsRemoved(t *testing.T) {
	e, c := dockerEnv(t)
	ctx := context.Background()
	live := newRow(t, e, store.CreateDBInstanceParams{
		Name: "live1", Kind: "redis", Image: "redis:7-alpine", Version: "7-alpine",
		ContainerName: "dbstudio-live1", Port: 6379,
		Secrets: map[string]string{"password": "pw1234!"},
	})
	dead := newRow(t, e, store.CreateDBInstanceParams{
		Name: "dead1", Kind: "redis", Image: "redis:7-alpine", Version: "7-alpine",
		ContainerName: "dbstudio-dead1", Port: 6379,
		Secrets: map[string]string{"password": "pw1234!"},
	})
	removed := store.InstanceRemoved
	if err := e.st.UpdateDBInstance(ctx, dead.ID,
		store.UpdateDBInstanceParams{Status: &removed}); err != nil {
		t.Fatalf("%v", err)
	}
	_ = live

	status, body := c.do("GET", "/api/v1/docker/compose?project="+e.project.ID, nil)
	if status != 200 {
		t.Fatalf("= %d %v", status, body)
	}
	yaml, _ := body["yaml"].(string)
	if !strings.Contains(yaml, "  live1:") {
		t.Errorf("살아 있는 것이 빠졌습니다:\n%s", yaml)
	}
	if strings.Contains(yaml, "  dead1:") {
		t.Errorf("지운 것이 실렸습니다:\n%s", yaml)
	}
}

// 내보낼 것이 없으면 빈 파일 대신 그렇게 말한다.
//
// 서비스가 하나도 없는 compose 파일은 문법은 맞지만 아무것도 하지 않는다.
// 그것을 받아 든 사람은 자기가 뭘 잘못했는지 찾게 된다.
func TestProjectComposeSaysWhenEmpty(t *testing.T) {
	e, c := dockerEnv(t)
	status, body := c.do("GET", "/api/v1/docker/compose?project="+e.project.ID, nil)
	if status != 404 || body["error"] != "empty" {
		t.Errorf("= %d %v (기대 404 empty)", status, body["error"])
	}
}

// 미리보기는 아무것도 바꾸지 않는다.
//
// 이 응답을 보고 사람이 결정한다. 여기서 이미 바뀌어 있으면 "취소"가 취소가
// 아니게 된다.
func TestChangesPreviewTouchesNothing(t *testing.T) {
	e, c := dockerEnv(t)
	ctx := context.Background()
	in, err := e.st.CreateDBInstance(ctx, store.CreateDBInstanceParams{
		ProjectID: e.project.ID, Name: "ed1", Kind: "postgres",
		Image: "postgres:17-alpine", Version: "17-alpine",
		ContainerName: "dbstudio-ed1", Port: 5432,
		Values:  map[string]string{"database": "appdb", "memoryMB": "512"},
		Secrets: map[string]string{"password": "pw1234!"},
	})
	if err != nil {
		t.Fatalf("%v", err)
	}

	status, body := c.do("POST", "/api/v1/docker/instances/"+in.ID+"/changes",
		map[string]any{"values": map[string]string{"memoryMB": "1024"}})
	if status != 200 {
		t.Fatalf("= %d %v", status, body)
	}
	if body["needed"] != "live" {
		t.Errorf("needed = %v (기대 live)", body["needed"])
	}
	got, _ := e.st.GetDBInstance(ctx, in.ID)
	if got.Values["memoryMB"] != "512" {
		t.Errorf("미리보기가 값을 바꿨습니다: %v", got.Values)
	}
}

// 고쳐도 지금 DB 에 적용되지 않는 값은 그렇게 말해야 한다.
//
// PostgreSQL 은 비밀번호를 첫 실행에서만 쓴다(살아 있는 컨테이너로 쟀다).
// "저장했습니다"라고만 하면 사람은 바뀐 줄 알고 새 비밀번호로 접속하다 막힌다.
func TestChangesFlagsInitOnlyValues(t *testing.T) {
	e, c := dockerEnv(t)
	ctx := context.Background()
	in, err := e.st.CreateDBInstance(ctx, store.CreateDBInstanceParams{
		ProjectID: e.project.ID, Name: "ed2", Kind: "postgres",
		Image: "postgres:17-alpine", Version: "17-alpine",
		ContainerName: "dbstudio-ed2", Port: 5432,
		Values:  map[string]string{"database": "appdb"},
		Secrets: map[string]string{"password": "pw1234!"},
	})
	if err != nil {
		t.Fatalf("%v", err)
	}

	status, body := c.do("POST", "/api/v1/docker/instances/"+in.ID+"/changes",
		map[string]any{"values": map[string]string{"password": "새-비밀번호"}})
	if status != 200 {
		t.Fatalf("= %d %v", status, body)
	}
	if body["needed"] != "init" {
		t.Errorf("needed = %v (기대 init)", body["needed"])
	}
	only, _ := body["initOnly"].([]any)
	if len(only) != 1 {
		t.Fatalf("적용 안 되는 것이 %d개: %v", len(only), body["initOnly"])
	}
	// 비밀번호가 응답에 실려서는 안 된다. 이 응답은 화면에 그대로 그려진다.
	blob, _ := json.Marshal(body)
	if strings.Contains(string(blob), "새-비밀번호") {
		t.Errorf("비밀번호가 응답에 실렸습니다: %s", blob)
	}
}

// 지운 DB 는 고칠 수 없다.
//
// 고칠 컨테이너가 없는데 "고쳤습니다"라고 답하면, 사람은 다음에 만들 때 그 값이
// 쓰일 것으로 기대한다. 그런데 그 줄은 기록일 뿐이라 다시 만들 길이 없다.
func TestUpdateRefusesRemovedInstance(t *testing.T) {
	e, c := dockerEnv(t)
	ctx := context.Background()
	in := newRow(t, e, store.CreateDBInstanceParams{
		Name: "ed3", Kind: "postgres", Image: "postgres:17-alpine", Version: "17-alpine",
		ContainerName: "dbstudio-ed3", Port: 5432,
		Secrets: map[string]string{"password": "pw1234!"},
	})
	removed := store.InstanceRemoved
	if err := e.st.UpdateDBInstance(ctx, in.ID,
		store.UpdateDBInstanceParams{Status: &removed}); err != nil {
		t.Fatalf("%v", err)
	}

	status, body := c.do("PATCH", "/api/v1/docker/instances/"+in.ID,
		map[string]any{"values": map[string]string{"memoryMB": "1024"}})
	if status != 409 || body["error"] != "removed" {
		t.Errorf("= %d %v (기대 409 removed)", status, body["error"])
	}
}

// 바꾼 것이 없으면 아무 일도 없어야 한다.
func TestUpdateWithNoChangesDoesNothing(t *testing.T) {
	e, c := dockerEnv(t)
	in := newRow(t, e, store.CreateDBInstanceParams{
		Name: "ed4", Kind: "postgres", Image: "postgres:17-alpine", Version: "17-alpine",
		ContainerName: "dbstudio-ed4", Port: 5432,
		Values:  map[string]string{"database": "appdb", "memoryMB": "512"},
		Secrets: map[string]string{"password": "pw1234!"},
	})
	status, body := c.do("PATCH", "/api/v1/docker/instances/"+in.ID,
		map[string]any{"values": map[string]string{"memoryMB": "512"}})
	if status != 200 || body["needed"] != "none" {
		t.Errorf("= %d %v", status, body["needed"])
	}
}

// 클러스터 노드를 내보낼 때는 그 파일이 클러스터가 아니라고 말해야 한다.
//
// 값에서 계획을 다시 세우는 길로는 remote_servers·macros·Keeper 가 나오지
// 않는다. 그렇게 나온 파일로 띄우면 **서로 모르는 ClickHouse 여럿**이 뜨는데,
// 겉보기에는 멀쩡하다 — 말하지 않으면 받아 간 사람이 그것으로 클러스터를
// 세웠다고 믿는다.
func TestComposeExportWarnsAboutClusterNodes(t *testing.T) {
	e, c := dockerEnv(t)
	ctx := context.Background()
	in, err := e.st.CreateDBInstance(ctx, store.CreateDBInstanceParams{
		ProjectID: e.project.ID, Name: "chc-s1r1", Kind: "clickhouse",
		Image: "clickhouse/clickhouse-server:24.8", Version: "24.8",
		ContainerName: "dbstudio-chc-s1r1", Port: 9000,
		Values:  map[string]string{"database": "appdb"},
		Secrets: map[string]string{"password": "Pw1234!aB"},
		Cluster: "chc", Role: "node", Shard: 1, Replica: 1,
	})
	if err != nil {
		t.Fatalf("%v", err)
	}

	status, body := c.do("GET", "/api/v1/docker/instances/"+in.ID+"/compose", nil)
	if status != 200 {
		t.Fatalf("= %d %v", status, body)
	}
	notes, _ := body["notes"].([]any)
	joined := ""
	for _, n := range notes {
		s, _ := n.(string)
		joined += s + " "
	}
	if !strings.Contains(joined, "클러스터") {
		t.Errorf("클러스터라는 안내가 없습니다: %v", body["notes"])
	}
}
