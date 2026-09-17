package api

import (
	"context"
	"testing"
	"time"

	"dbstudio/internal/model"
	"dbstudio/internal/store"
)

// 담당 노드가 리플리카일 때 서버 하위 요청이 어디서 도는지 본다.
//
// ── 왜 이 시험이 필요한가 ───────────────────────────────────────────
// 한 요청을 두 노드가 나눠 한다는 것이 이 기능의 핵심이다. 담당 노드는 그 서버에
// **실제로 접속**하고(목록 읽기), 행을 만드는 것은 **마스터**가 한다. 둘의 경계가
// 미들웨어 두 개(serverRoute · forward)의 상호작용으로 정해지므로, 한 줄만 틀려도
// 경계가 사라진다.
//
// 실제로 그런 버그가 있었다: 서버 라우팅이 "내가 담당이다"를 표시하면 쓰기 전달이
// 그 표시를 보고 넘기지 않았다. 그래서 리플리카가 담당인 서버에 DB를 추가하면
// **리플리카의 메타 DB에 행이 생기고 다음 복제에서 사라졌다** — 저장은 성공으로
// 보이고, 목록을 새로 고치면 없다. 이 코드베이스가 가장 나쁘다고 부르는 실패다.

// TestServerRouteDoesNotProxyWrites는 그 버그의 원인을 직접 고정한다.
//
// 미들웨어의 판단 자체를 본다 — 실제 넘김 없이 "넘기는가"만 보므로 두 프로세스의
// 타이밍에 기대지 않고도 경계를 고정할 수 있다.
func TestServerRouteDoesNotProxyWrites(t *testing.T) {
	cases := []struct {
		method string
		want   bool
		why    string
	}{
		{"GET", true,
			"DB 목록 읽기는 그 DB에 실제로 닿는 노드만 할 수 있다 — 담당 노드로 넘겨야 한다"},
		{"POST", false,
			"DB 추가는 메타 DB에 행을 남긴다. 담당 노드로 넘기면 리플리카에서 실행되어 " +
				"그 행이 다음 복제에서 사라진다"},
		{"PUT", false, "서버 수정도 메타 DB의 일이다"},
		{"DELETE", false, "서버 삭제도 메타 DB의 일이다"},
		{"PATCH", false, "메타 DB의 일이다"},
		{"HEAD", true, "읽기다"},
	}
	for _, tc := range cases {
		if got := serverRouteProxies(tc.method); got != tc.want {
			t.Errorf("%s: 넘김 = %v (기대 %v) — %s", tc.method, got, tc.want, tc.why)
		}
	}
}

// seedResponsibleServer는 마스터에 프로젝트·서버(+첫 DB)를 만들고, 담당 노드를
// 리플리카로 지정한 뒤 그 사실이 **양쪽에** 자리 잡을 때까지 기다린다.
//
// 양쪽을 기다리는 이유: 마스터는 넘길지 판단할 때 자기 메타 DB를 보고, 리플리카는
// 미들웨어에서 자기 복제본을 본다. 한쪽만 맞으면 시험이 재현하려는 상태가 아니다.
func seedResponsibleServer(t *testing.T, master, replica *clusterEnv) (serverID string, cookie map[string]string) {
	t.Helper()
	ctx := context.Background()

	mc := master.client(t)
	if status, body := mc.do("POST", "/api/v1/auth/login",
		map[string]string{"username": "alice", "password": testPassword}); status != 200 {
		t.Fatalf("로그인 실패 = %d: %v", status, body)
	}
	status, body := mc.do("POST", "/api/v1/projects/", map[string]any{"name": "테스트 프로젝트"})
	if status != 201 {
		t.Fatalf("프로젝트 생성 = %d: %v", status, body)
	}
	made, _ := body["project"].(map[string]any)
	projectID, _ := made["id"].(string)

	status, body = mc.do("POST", "/api/v1/servers", map[string]any{
		"projectId": projectID, "name": "pg-resp", "kind": "postgres",
		"host": "10.0.0.9", "port": 5432, "defaultEnvironment": "dev",
		"databaseName": "appdb",
	})
	if status != 200 && status != 201 {
		t.Fatalf("서버 생성 = %d: %v", status, body)
	}

	servers, err := master.st.ListServers(ctx, nil)
	if err != nil {
		t.Fatalf("서버 목록: %v", err)
	}
	for _, s := range servers {
		if s.Name == "pg-resp" {
			serverID = s.ID
		}
	}
	if serverID == "" {
		t.Fatal("만든 서버를 찾지 못했습니다")
	}

	// 마스터에 지정하고, 그 사실이 리플리카로 복제되기를 기다린다.
	// 복제를 기다리지 않고 리플리카에 직접 쓰면 시험이 재현하려는 상태
	// ("담당 노드를 바꾼 직후의 창")가 아니라 다른 상태를 만든다.
	if err := master.st.SetServerNode(ctx, serverID, replica.node.NodeID()); err != nil {
		t.Fatalf("마스터에 담당 노드 지정: %v", err)
	}
	waitFor(t, 5*time.Second, "담당 노드가 리플리카로 복제되지 않았습니다", func() bool {
		srv, err := replica.st.GetServer(context.Background(), serverID)
		return err == nil && srv.NodeID == replica.node.NodeID()
	})
	return serverID, mc.cookies
}

// TestReplicaResponsibleServerWriteGoesToMaster는 두 노드로 실제 요청을 태워
// 그 경계가 end-to-end로 성립하는지 본다.
//
// 이것이 이 기능에서 가장 중요한 시험이다. 미들웨어 순서(serverRoute가 forward보다
// 앞)와 표시 규칙(읽기만)이 둘 다 맞아야 통과한다.
func TestReplicaResponsibleServerWriteGoesToMaster(t *testing.T) {
	master, replica := startCluster(t)
	ctx := context.Background()
	serverID, _ := seedResponsibleServer(t, master, replica)

	// 리플리카에서 로그인한다(세션 쿠키는 마스터가 만들고 복제로 넘어온다).
	rc := replica.client(t)
	if status, body := rc.do("POST", "/api/v1/auth/login",
		map[string]string{"username": "alice", "password": testPassword}); status != 200 {
		t.Fatalf("리플리카 로그인 = %d: %v", status, body)
	}

	// ── 쓰기: DB 추가는 마스터로 넘어가야 한다 ──────────────────────
	//
	// 담당 노드에 실제 DB가 없으므로 이름 대조는 확인 못 함으로 끝나고, 막지 않는다
	// (이름 직접 입력은 원래 지원하는 길이다). 그 규칙까지 함께 시험된다.
	status, body := rc.do("POST", "/api/v1/servers/"+serverID+"/databases", map[string]any{
		"databases": []string{"second"}, "environment": "dev",
	})
	if status == 503 {
		t.Fatalf("쓰기가 마스터로 넘어가지 않았습니다(마스터는 살아 있음) = 503: %v", body)
	}

	// 마스터에 그 행이 생겼는가 — "넘어갔다"의 증거다.
	//
	// 리플리카의 DB에 직접 생겼는지 보지 않는 이유: 전달이 맞다면 그 행은 **복제로**
	// 들어온다. 그 둘을 구분하는 것이 이 시험의 요점이다.
	if !hasDatabase(ctx, master.st, serverID, "second") {
		mList, lerr := master.st.ListConnectionsByServer(ctx, serverID)
		rList, rerr := replica.st.ListConnectionsByServer(ctx, serverID)
		names := func(list []*model.Connection) []string {
			out := make([]string, 0, len(list))
			for _, c := range list {
				out = append(out, c.DatabaseName)
			}
			return out
		}
		t.Errorf("DB 추가가 마스터에 반영되지 않았습니다. 응답 = %d\n"+
			"  마스터의 DB 목록 = %v (err=%v)\n"+
			"  리플리카의 DB 목록 = %v (err=%v)\n"+
			"  대상 서버 = %q, 마스터 주소 = %q",
			status, names(mList), lerr, names(rList), rerr, serverID, master.url)
	}

	// 그리고 리플리카에도 (복제로) 보여야 한다.
	waitFor(t, 5*time.Second, "추가한 DB가 리플리카에 복제되지 않았습니다", func() bool {
		return hasDatabase(context.Background(), replica.st, serverID, "second")
	})
}

// hasDatabase는 그 서버 아래에 그 이름의 DB가 등록되어 있는지 본다.
func hasDatabase(ctx context.Context, st *store.Store, serverID, name string) bool {
	list, err := st.ListConnectionsByServer(ctx, serverID)
	if err != nil {
		return false
	}
	for _, c := range list {
		if c.DatabaseName == name {
			return true
		}
	}
	return false
}

// TestServerNodeChangeIsReplicatedToReplica는 담당 노드 변경이 복제로 전달되는지 본다.
//
// 이 시험이 없으면 위 시험이 "양쪽에 직접 써서" 통과할 수 있고, 그러면 운영에서
// 담당 노드를 바꾼 직후의 창(마스터는 아는데 리플리카는 모르는 상태)이 검증되지
// 않는다. 그 창에서는 리플리카가 담당 노드를 모르므로 스스로 실행을 시도한다.
func TestServerNodeChangeIsReplicatedToReplica(t *testing.T) {
	master, replica := startCluster(t)
	ctx := context.Background()
	serverID, _ := seedResponsibleServer(t, master, replica)

	// 이제 담당을 다른 노드로 바꾼다. 그 변경이 리플리카로 흘러야 한다.
	other := replica.node.NodeID() + "-x"
	if err := master.st.SetServerNode(ctx, serverID, other); err != nil {
		t.Fatalf("담당 노드 변경: %v", err)
	}
	waitFor(t, 5*time.Second, "담당 노드 변경이 리플리카로 복제되지 않았습니다 — "+
		"리플리카는 옛 값을 보고 엉뚱한 노드로 요청을 넘깁니다", func() bool {
		srv, err := replica.st.GetServer(context.Background(), serverID)
		return err == nil && srv.NodeID == other
	})
}
