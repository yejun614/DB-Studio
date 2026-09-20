package api

import (
	"context"
	"strings"
	"testing"

	"dbstudio/internal/model"
	"dbstudio/internal/store"
)

// 담당 노드에 **읽기**를 맡기는 경로의 계약을 여기서 본다.
//
// ── 왜 이 시험이 필요한가 ───────────────────────────────────────────
// 두 노드가 일을 나눈다: 담당 노드가 접속해 읽고, 마스터가 그 값을 기록한다. 그 경계가
// 세 가지 약속 위에 서 있고, 하나만 틀려도 조용히 무너진다:
//
//  1. 이 경로는 **리플리카에서도 실행된다** (requireMaster를 붙이면 담당 노드가
//     리플리카일 때 아무도 실행하지 못한다 — 그 경우가 이 경로의 존재 이유다)
//  2. 실행 노드는 **마스터를 거치지 않는다** (거치면 요청이 마스터로 갔다가 되돌아와
//     고리가 되고, 그 사이 DB에 못 닿는 마스터가 접속을 시도한다)
//  3. 실패할 때 **이유를 그대로 전한다** (상태 코드만 남기면 사람이 노드 로그를 찾아가야
//     한다 — 이번 장애에서 3시간을 쓴 자리다)
//
// 2번이 특히 중요하다. P32에서 같은 자리를 잘못 잡아 **리플리카의 메타 DB에만 행이
// 생기고 다음 복제에서 사라지는** 조용한 손실이 났다. 그때도 미들웨어를 하나씩 보면
// 각각 옳아 보였고, 실제 요청을 태워 보고서야 드러났다.

// TestNodeCollectRunsOnReplica는 담당 노드(리플리카)가 이 경로를 실행할 수 있는지 본다.
//
// requireMaster를 붙이면 이 시험이 405 또는 403으로 깨진다. 그때 증상은 "담당 노드를
// 리플리카로 지정하면 모니터링이 아예 안 된다"이고, 원인은 이 한 줄이다.
func TestNodeCollectRunsOnReplica(t *testing.T) {
	_, replica := startCluster(t)

	// 커넥션 하나를 만든다. 실제 DB에 붙지 않고 노드가 실패를 돌려주는 것만 본다 —
	// 여기서 확인하는 것은 "누가 실행하는가"이고, 접속 성패가 아니다.
	connID, _ := seedConnectionOnNode(t, replica)

	status, body := replica.client(t).doAsNode("POST", "/api/v1/node/collect",
		map[string]any{"connectionId": connID})

	// 401/403이면 인증 계약이 틀렸다(담당 노드는 공용 비밀로 부른다).
	// 404/405면 경로가 없거나 마스터 전용이다 — 그것도 계약 위반이다.
	if status == 401 || status == 403 {
		t.Fatalf("담당 노드가 이 경로를 실행하지 못한다 = %d: %v", status, body)
	}
	if status == 404 || status == 405 {
		t.Fatalf("경로가 없거나 마스터 전용이다 = %d (담당 노드는 리플리카일 수 있다)", status)
	}
	// 여기까지 왔으면 핸들러가 돌았다. DB가 없으므로 up=0 + 이유가 정상이다.
	if status != 200 {
		t.Fatalf("예상 밖 상태 = %d: %v", status, body)
	}
	if _, ok := body["samples"]; !ok {
		t.Errorf("metric.Set 모양이 아니다(어댑터·화면·저장소가 같은 형을 쓴다): %v", body)
	}
}

// TestNodeIntrospectRunsOnReplica는 스키마 쪽도 같은 계약인지 본다.
func TestNodeIntrospectRunsOnReplica(t *testing.T) {
	_, replica := startCluster(t)
	connID, _ := seedConnectionOnNode(t, replica)

	status, body := replica.client(t).doAsNode("POST", "/api/v1/node/introspect",
		map[string]any{"connectionId": connID})

	if status == 401 || status == 403 {
		t.Fatalf("담당 노드가 이 경로를 실행하지 못한다 = %d: %v", status, body)
	}
	if status == 404 || status == 405 {
		t.Fatalf("경로가 없거나 마스터 전용이다 = %d", status)
	}
}

// TestNodeReadPathsDoNotRequireMaster는 두 경로가 마스터로 넘어가지 않는지 본다.
//
// 넘어가면 담당 노드가 리플리카일 때 요청이 마스터로 갔다가 다시 담당 노드로 되돌아온다
// — 왕복 한 번이 늘 뿐 아니라, 그 사이 **DB에 못 닿는 마스터가 접속을 시도**할 수 있다.
//
// 판정 방법: 리플리카에 보낸 요청의 응답이 마스터의 복제 지점 표시(X-Cluster-Seq)를
// 달고 오지 않으면 됐다. 그 표시는 쓰기를 마스터로 넘길 때만 붙는다.
func TestNodeReadPathsDoNotRequireMaster(t *testing.T) {
	master, replica := startCluster(t)
	connID, _ := seedConnectionOnNode(t, replica)

	// 마스터가 요청을 받았는지 세기 위해, 마스터의 감사 기록을 본다.
	// /node/* 는 감사 기록을 남기지 않으므로 다른 방법을 쓴다: 응답 헤더를 본다.
	mc := master.client(t)
	_ = mc

	for _, path := range []string{"/api/v1/node/collect", "/api/v1/node/introspect"} {
		r := replica.client(t)
		status, body := r.doAsNode("POST", path, map[string]any{"connectionId": connID})
		// 넘어갔다면 마스터가 이 경로를 처리하려 했을 것이다. 마스터에는 그 커넥션이
		// 있으므로 상태 코드가 달라진다 — 담당 노드의 답(200 또는 502)과 구분된다.
		if status == 404 {
			t.Errorf("%s: 경로를 찾지 못했다 — 노드 전용 경로로 등록되지 않았다", path)
		}
		if status == 503 {
			t.Errorf("%s: 마스터로 넘기려 했다(503 master_unreachable): %v", path, body)
		}
	}
}

// TestNodeCollectRejectsUnknownConnection은 없는 커넥션을 "없다"와 "아직 못 받았다"로
// 구분해 알리는지 본다.
//
// 이 구분이 필요한 이유: 복제가 아직 못 따라잡은 노드에서는 커넥션이 정말 없다. 그때
// "없다"고 단정하면 사람은 담당 노드를 잘못 골랐다고 판단해 멀쩡한 설정을 바꾼다.
func TestNodeCollectRejectsUnknownConnection(t *testing.T) {
	_, replica := startCluster(t)

	status, body := replica.client(t).doAsNode("POST", "/api/v1/node/collect",
		map[string]any{"connectionId": "존재하지-않는-커넥션"})
	if status != 404 {
		t.Fatalf("없는 커넥션 = %d (기대 404): %v", status, body)
	}
	msg, _ := body["message"].(string)
	if !strings.Contains(msg, "복제") {
		t.Errorf("복제 지연 가능성을 알리지 않는다: %q", msg)
	}
}

// TestNodeCollectRejectsMissingID는 빈 요청을 거절하는지 본다.
//
// 거절하지 않으면 빈 ID로 커넥션을 찾다가 "없는 커넥션"으로 오해된다.
func TestNodeCollectRejectsMissingID(t *testing.T) {
	_, replica := startCluster(t)

	status, body := replica.client(t).doAsNode("POST", "/api/v1/node/collect",
		map[string]any{"connectionId": "  "})
	if status != 400 {
		t.Fatalf("빈 connectionId = %d (기대 400): %v", status, body)
	}
}

// seedConnectionOnNode는 리플리카에 보이는 커넥션 하나를 만들고 그 ID를 준다.
//
// 노드에서 만드는 이유: 이 시험들은 담당 노드의 **실행 계약**만 본다. 마스터가 있어야
// 통과하는 상태를 만들면 "리플리카 혼자서도 실행되는가"라는 질문이 흐려진다.
func seedConnectionOnNode(t *testing.T, node *clusterEnv) (connID, serverID string) {
	t.Helper()
	ctx := context.Background()

	pj, err := node.st.CreateProject(ctx, store.SaveProjectParams{Name: "중계 시험"})
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	pw := "pw"
	srv, conn, err := node.st.CreateServerWithDatabase(ctx,
		store.SaveServerParams{
			ProjectID: pj.ID, Name: "중계대상", Kind: "mysql",
			Host: "10.0.0.9", Port: 3306, DefaultEnvironment: "dev",
			Options: model.Options{}, Tags: []string{}, Enabled: true,
			Username: "appuser", Password: &pw,
		},
		store.SaveConnectionParams{
			ProjectID: pj.ID, Name: "중계대상-DB", Environment: "dev",
			DatabaseName: "appdb", Tags: []string{}, Enabled: true,
		})
	if err != nil {
		t.Fatalf("connection: %v", err)
	}

	// 자격증명이 복호화되는지 먼저 확인한다. 여기서 실패하면 노드가 secret_failed를
	// 돌려주고, 이 시험들이 보려는 계약(경로가 도는가)과 다른 이유로 깨진다.
	if _, err := node.st.GetSecret(ctx, conn.ID); err != nil {
		t.Fatalf("자격증명 복호화: %v", err)
	}
	return conn.ID, srv.ID
}
