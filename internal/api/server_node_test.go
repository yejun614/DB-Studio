package api

import (
	"context"
	"strings"
	"testing"

	"dbstudio/internal/model"
	"dbstudio/internal/store"
)

// 담당 노드가 서버 등급이 되면서 새로 생긴 **HTTP 계약**을 여기서 확인한다.
//
// 상속·영향 범위 계산·마이그레이션 승격은 store 패키지의 시험이 본다
// (internal/store/servernode_test.go). 여기서 보는 것은 그 위의 계약이다:
// 저장 시점에 담당 노드를 검증하는가, 담당 노드가 리플리카여도 노드 전용 경로를
// 쓸 수 있는가, 주소를 모를 때 무엇을 하는가.
//
// 나누는 이유: 같은 규칙을 두 곳에서 시험하면 한쪽이 옛 전제(담당 노드가 DB 등급이던
// 시절)로 남아 있다가 조용히 통과한다.

// newServerNodeEnv는 프로젝트 하나와 서버 하나를 만든 환경을 준다.
func newServerNodeEnv(t *testing.T) (*clusterEnv, context.Context, string, string) {
	t.Helper()
	master, _ := startCluster(t)
	ctx := context.Background()

	c := master.client(t)
	if status, body := c.do("POST", "/api/v1/auth/login",
		map[string]string{"username": "alice", "password": testPassword}); status != 200 {
		t.Fatalf("로그인 실패 = %d: %v", status, body)
	}
	status, body := c.do("POST", "/api/v1/projects/", map[string]any{"name": "테스트 프로젝트"})
	if status != 201 {
		t.Fatalf("프로젝트 생성 = %d: %v", status, body)
	}
	made, _ := body["project"].(map[string]any)
	return master, ctx, made["id"].(string), c.cookies["dbstudio_session"]
}

// TestServerNodeRejectsUnknownNode는 저장 시점에 오타를 막는지 본다.
//
// 조용한 실패를 막는 자리다. 저장을 통과하면 그 서버의 요청이 전부 502로 떨어지는데,
// 그때 화면에는 "담당 노드를 찾을 수 없습니다"만 뜨고 어느 값이 틀렸는지는 안 나온다.
func TestServerNodeRejectsUnknownNode(t *testing.T) {
	master, _, projectID, _ := newServerNodeEnv(t)

	c := master.client(t)
	if status, _ := c.do("POST", "/api/v1/auth/login",
		map[string]string{"username": "alice", "password": testPassword}); status != 200 {
		t.Fatal("로그인 실패")
	}

	status, body := c.do("POST", "/api/v1/servers", map[string]any{
		"projectId": projectID,
		"name":      "pg-bad-node", "kind": "postgres",
		"host": "10.0.0.9", "port": 5432, "defaultEnvironment": "dev",
		"nodeId": "존재하지-않는-노드",
	})
	if status != 400 {
		t.Fatalf("없는 노드로 서버 생성 = %d (기대 400): %v", status, body)
	}
	if code, _ := body["error"].(string); code != "invalid_node" {
		t.Errorf("오류 코드 %v, 기대 invalid_node", body["error"])
	}

	// 빈 값은 통과해야 한다 — "요청을 받은 노드가 접속한다"는 정상 상태다.
	status, body = c.do("POST", "/api/v1/servers", map[string]any{
		"projectId": projectID,
		"name":      "pg-no-node", "kind": "postgres",
		"host": "10.0.0.9", "port": 5432, "defaultEnvironment": "dev",
	})
	if status != 201 && status != 200 {
		t.Fatalf("담당 없이 서버 생성 = %d (기대 200/201): %v", status, body)
	}
}

// TestNodeServerDatabasesIsNotMasterOnly는 담당 노드가 리플리카여도 그 경로를 쓸 수
// 있는지 본다.
//
// 다른 /node/* 경로는 전부 마스터 전용이다. 이 경로만 예외인 이유가 이 시험의 제목이다:
// 담당 노드는 리플리카일 수 있고, 그때 "그 DB에 닿는 노드가 목록을 읽는다"가 성립해야
// 사설망 DB의 "DB 목록 불러오기"가 살아난다. requireMaster를 실수로 붙이면 이 기능이
// 통째로 죽는데, 증상은 "마스터에서는 되는데 리플리카 담당이면 409"다.
func TestNodeServerDatabasesIsNotMasterOnly(t *testing.T) {
	_, replica := startCluster(t)

	// 자격증명 없이도 마스터 전용 관문에 막히지 않는지 본다(비밀만 맞으면 통과).
	// 실제 접속은 하지 않으므로 없는 서버로 404를 기대한다 — 404가 온다는 것은
	// requireMaster(409)를 지나 핸들러까지 왔다는 뜻이다.
	c := replica.client(t)
	c.cookies = map[string]string{} // 사람 세션과 무관한 호출이다
	status, body := c.doAsNode("POST", "/api/v1/node/server-databases",
		map[string]any{"serverId": "없는-서버"})
	if status == 409 {
		t.Fatalf("리플리카가 마스터 전용으로 거절했습니다(=409). "+
			"이 경로에는 requireMaster가 없어야 합니다: %v", body)
	}
	if status != 404 {
		t.Fatalf("없는 서버 = %d (기대 404): %v", status, body)
	}
}

// TestServerNodeRouteRequiresAddress는 담당 노드의 주소를 모를 때 무엇을 하는지 본다.
//
// 주소가 비어 있으면 넘길 수 없다. 그때 "넘겼는데 실패"가 아니라 **넘기지 않고**
// 이유를 말해야 한다 — 그래야 사람이 그 노드에 -cluster-advertise를 주고 다시 띄운다는
// 조치를 알 수 있다.
func TestServerNodeRouteRequiresAddress(t *testing.T) {
	master, ctx, projectID, _ := newServerNodeEnv(t)

	// 주소 없는 노드를 하나 만들어 담당으로 지정한다.
	if err := master.st.UpsertClusterNode(ctx, store.ClusterNode{
		ID: "노드-no-addr", Name: "no-addr", Role: store.NodeRoleReplica, Address: "",
	}); err != nil {
		t.Fatalf("노드 등록: %v", err)
	}
	srv, err := master.st.CreateServer(ctx, store.SaveServerParams{
		ProjectID: projectID, Name: "srv-no-addr", Kind: model.KindPostgres,
		Host: "10.0.0.9", Port: 5432, DefaultEnvironment: model.EnvDev,
		Enabled: true, Tags: []string{}, NodeID: "노드-no-addr",
	})
	if err != nil {
		t.Fatalf("서버 생성: %v", err)
	}

	c := master.client(t)
	if status, _ := c.do("POST", "/api/v1/auth/login",
		map[string]string{"username": "alice", "password": testPassword}); status != 200 {
		t.Fatal("로그인 실패")
	}
	status, body := c.do("GET", "/api/v1/servers/"+srv.ID+"/databases", nil)
	if status != 502 {
		t.Fatalf("주소 없는 담당 노드 = %d (기대 502): %v", status, body)
	}
	if code, _ := body["error"].(string); code != "node_unavailable" {
		t.Errorf("오류 코드 %v, 기대 node_unavailable", body["error"])
	}
	if msg, _ := body["message"].(string); !contains(msg, "주소") {
		t.Errorf("사유가 주소 때문임을 알려주지 않습니다: %v", body)
	}
}

// contains는 짧은 부분 문자열 확인이다.
func contains(s, sub string) bool { return strings.Contains(s, sub) }

