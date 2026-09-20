package api

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	"dbstudio/internal/model"
)

// 백업을 담당 노드에 맡기는 판단을 시험한다.
//
// ── 왜 이 시험이 필요한가 ───────────────────────────────────────────
// 백업은 지표·스키마와 사정이 다르다. **파일이 남는 일**이라 값 중계로는 해결되지 않고,
// 게다가 백업 요청은 **쓰기**라서 리플리카에서 오면 마스터로 넘어온다 — 그러면 닿지도
// 못하는 마스터가 덤프를 시도하고 1045로 죽는다(버전 캡처와 같은 원인).
//
// 그래서 판단할 것이 셋이다:
//
//	① 담당 노드가 따로 있으면 맡긴다 (여기가 틀리면 백업이 계속 1045로 죽는다)
//	② 이 노드가 담당이거나 담당이 없으면 여기서 한다 (여기가 틀리면 자기 자신을 부른다)
//	③ 파일 이름은 **마스터가** 만든다 (노드가 만들면 경로 조작 검사가 필요해진다)

// TestBackupDelegationDecision은 "맡겨야 하는가" 판단이 옳은지 본다.
//
// 두 노드를 실제로 띄우는 이유: 담당 노드 ID를 노드 목록에서 읽어야 하므로 가짜 값으로는
// "담당이 나"와 "담당이 남"을 구분할 수 없다.
func TestBackupDelegationDecision(t *testing.T) {
	master, replica := startCluster(t)

	t.Run("담당이 남이면 맡긴다", func(t *testing.T) {
		// 막히는 것은 노드 뒤의 DB 이고, 그 노드만 닿는다 — 맡기는 것이 이 기능의 이유다.
		conn := &model.Connection{NodeID: replica.node.NodeID()}
		if !master.srv.relayNeeded(conn) {
			t.Error("맡기지 않았다 — 닿지도 못하는 마스터가 덤프를 시도하게 된다")
		}
	})

	t.Run("담당이 나면 맡기지 않는다", func(t *testing.T) {
		// 자기 자신에게 부탁하는 모양이 된다.
		conn := &model.Connection{NodeID: master.node.NodeID()}
		if master.srv.relayNeeded(conn) {
			t.Error("맡겼다 — 자기 자신을 부르는 모양이 된다")
		}
	})

	t.Run("담당이 없으면 맡기지 않는다", func(t *testing.T) {
		// 담당을 지정하지 않은 DB가 대부분이다. 여기서 맡기려 하면 갈 곳이 없다.
		conn := &model.Connection{NodeID: ""}
		if master.srv.relayNeeded(conn) {
			t.Error("맡겼다 — 담당 노드가 없는데 갈 곳이 없다")
		}
	})
}

// TestDumpNameIsAllocatedByTheReceiver는 이름을 받는 쪽(마스터)이 만드는지 본다.
//
// ── 왜 이것이 안전 경계인가 ────────────────────────────────────────
// 처음 설계는 노드가 이름을 만들어 올리고 마스터가 그것을 검사하는 모양이었다. 그러면
// 검사가 한 곳 더 생기고, 그 검사를 빠뜨린 날이 곧 디렉터리 탈출(`../../etc/x`)이 된다.
// 지금은 이름이 **경계를 넘지 않는다** — 마스터가 만들어 내려보내고 노드는 그대로 쓴다.
//
// 확장자를 보존하는 이유는 그 값이 **복구기가 읽는 방법**을 정하기 때문이다. 잃으면
// 파일은 있어도 열 수 없고, 그 사실은 복구 시점에 드러난다.
func TestDumpNameIsAllocatedByTheReceiver(t *testing.T) {
	master, _ := startCluster(t)

	for _, kind := range []model.DBKind{model.KindMySQL, model.KindMongoDB, model.KindRedis} {
		name, err := master.srv.backups.AllocateDumpName(string(kind))
		if err != nil {
			t.Errorf("%s: %v", kind, err)
			continue
		}
		if !strings.HasSuffix(name, ".gz") {
			t.Errorf("%s → %q (확장자가 없으면 복구기가 읽는 방법을 잃는다)", kind, name)
		}
		// 경로가 백업 디렉터리 안이어야 한다.
		if _, err := master.srv.backups.FilePath(name); err != nil {
			t.Errorf("%s → FilePath: %v", kind, err)
		}
	}
}

// TestBackupDelegationUsesHomeNodeNotRequestingNode는 담당 노드의 **주소**로 부르는지 본다.
//
// 요청을 받은 노드로 보내면 원래 문제(닿지 못하는 노드가 시도한다)가 그대로 남는다.
// 이 시험은 주소 선택 자체를 고정한다 — 담당 노드가 목록에서 내려갔으면 맡기지 않고
// 이유를 말해야 한다(조용히 로컬에서 시도하면 1045가 다시 난다).
func TestBackupDelegationUsesHomeNodeNotRequestingNode(t *testing.T) {
	master, replica := startCluster(t)
	ctx := context.Background()

	// 담당 노드를 목록에서 내린다(주소를 알 수 없는 상태).
	//
	// **마스터의 메타 DB**에 쓴다. 노드 목록의 주인은 마스터이고, 리플리카에 쓰면 다음
	// 복제에서 사라진다(이 시험이 재현하려는 상태가 아니라 다른 상태를 만든다).
	if err := master.st.RemoveClusterNode(ctx, replica.node.NodeID()); err != nil {
		t.Fatalf("remove node: %v", err)
	}
	waitFor(t, 5*time.Second, "노드 내리기가 마스터에 반영되지 않았습니다", func() bool {
		n, err := master.st.GetClusterNode(context.Background(), replica.node.NodeID())
		return err == nil && n.Status != "active"
	})

	conn := &model.Connection{NodeID: replica.node.NodeID()}
	// 판단(relayNeeded)은 여전히 "맡긴다"여야 한다. 주소가 없다고 로컬에서 시도하면
	// 닿지도 못하는 마스터가 덤프를 시도해 1045가 다시 난다 — 그때는 화면에 그 이유가 뜬다.
	if !master.srv.relayNeeded(conn) {
		t.Error("담당 노드가 내려갔다고 로컬에서 시도하면 원래 문제가 그대로 남는다")
	}
}

// TestSeedBackupTargetIsReachable은 시험용 커넥션의 담당 노드를 바꿀 수 있는지 확인한다.
//
// 이 시험 파일의 다른 시험들이 담당 노드를 지정하는데, 그 지정이 실제로 먹지 않으면
// "맡긴다"가 우연히 참이 되어 아무것도 증명하지 못한다.
func TestSeedBackupTargetIsReachable(t *testing.T) {
	master, replica := startCluster(t)
	connID, serverID := seedConnectionOnNode(t, master)

	if err := master.st.SetServerNode(context.Background(), serverID, replica.node.NodeID()); err != nil {
		t.Fatalf("set node: %v", err)
	}
	conn, err := master.st.GetConnection(context.Background(), connID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if conn.NodeID != replica.node.NodeID() {
		t.Fatalf("담당 노드가 지정되지 않았다: %q (기대 %q) — 이 파일의 다른 시험이 무의미해진다",
			conn.NodeID, replica.node.NodeID())
	}
	// 담당이 남이므로 맡겨야 한다.
	if !master.srv.relayNeeded(conn) {
		t.Error("담당이 남인데 맡기지 않는다")
	}
}

// TestRestoreDelegationDecision은 복구도 담당 노드로 가는지 본다 (P33-2).
//
// 복구는 백업의 역방향이고 같은 문제를 갖는다: 요청이 쓰기라 마스터로 넘어오는데,
// 담당 노드가 지정된 DB는 마스터가 닿지 못할 수 있다. 복구는 되돌릴 수 없는 일이라
// (운영 DB에 덮어쓴다) 여기서 틀리면 피해가 백업 실패보다 크다.
func TestRestoreDelegationDecision(t *testing.T) {
	master, replica := startCluster(t)

	t.Run("담당이 남이면 그 노드가 복구한다", func(t *testing.T) {
		conn := &model.Connection{NodeID: replica.node.NodeID()}
		if !master.srv.relayNeeded(conn) {
			t.Error("맡기지 않았다 — 닿지도 못하는 마스터가 복구를 시도하게 된다")
		}
	})
	t.Run("담당이 나면 여기서 복구한다", func(t *testing.T) {
		conn := &model.Connection{NodeID: master.node.NodeID()}
		if master.srv.relayNeeded(conn) {
			t.Error("맡겼다 — 자기 자신을 부르는 모양이 된다")
		}
	})
}

// TestDumpFileIsServedFromMaster는 노드가 파일을 당겨 올 수 있는지 본다.
//
// ── 방향이 한 번 뒤집혔다 (기록해 둔다) ────────────────────────────
// 처음에는 마스터가 파일을 노드의 요청 **본문**에 실어 밀어 넣으려 했다. 그 방법은
// Fiber의 요청 본문 스트리밍과 크기 상한(BodyLimit, 8MB)을 **전역으로** 바꿔야 했다 —
// 모든 POST에 영향이 가고, 아무 엔드포인트나 큰 본문을 받게 된다.
//
// 그래서 뒤집었다: 노드가 마스터에서 파일을 **받아 온다**. 노드는 이미 마스터 주소와
// 클러스터 비밀을 알고 있고(복제·하트비트가 그 길을 쓴다), 마스터가 파일을 흘려보내는
// 것은 스냅샷 경로에 선례가 있다(SetBodyStream).
//
// 이 시험은 그 경로가 **마스터 전용**인지 본다 — 리플리카가 받아도 그 파일은 거기 없다.
func TestDumpFileIsServedFromMaster(t *testing.T) {
	master, replica := startCluster(t)

	// 리플리카는 이 경로를 부를 수 없다. requireMaster가 409(not_master)로 막는다 —
	// 백업은 마스터에 보관되므로(P33-1) 리플리카에는 내줄 파일이 없다.
	// 401/403이 아니라 409인 이유: 인증은 통과했다(공용 비밀은 맞다). **역할**이 아니라는
	// 뜻이고, 그래서 화면이 "노드로 복구를 넘길 수 없다"를 정확히 설명할 수 있다.
	status, body := replica.client(t).doAsNode("GET", "/api/v1/node/dump-file?name=x.sql.gz", nil)
	if status != 409 {
		t.Errorf("리플리카가 이 경로를 부를 수 있다 = %d (기대 409 not_master): %v", status, body)
	}
	if code, _ := body["error"].(string); code != "not_master" {
		t.Errorf("거절 이유가 다르다(역할 문제임을 알려야 한다): %v", body)
	}

	// 마스터에서 부르면 "파일 없음"이 정직하게 온다(빈 본문이면 복구가 문장 0개로 끝난다).
	status, body = master.client(t).doAsNode("GET", "/api/v1/node/dump-file?name=없는파일.sql.gz", nil)
	if status != 404 {
		t.Errorf("없는 파일 = %d (기대 404): %v", status, body)
	}
	msg, _ := body["message"].(string)
	if !strings.Contains(msg, "없") {
		t.Errorf("이유가 무엇인지 알려주지 않는다: %q", msg)
	}
}

// TestDumpFileValidatesName은 이름으로 경로를 조작하지 못하는지 본다.
//
// 이름은 마스터가 만든 것이지만, 경로를 조립하는 곳에서는 언제나 확인한다는 것이
// 이 저장소의 규칙이다(FilePath 주석). 그 규칙이 지켜지지 않는 날이 탈출이 되는 날이다.
func TestDumpFileValidatesName(t *testing.T) {
	master, _ := startCluster(t)

	status, _ := master.client(t).doAsNode("GET",
		"/api/v1/node/dump-file?name="+url.QueryEscape("../../../etc/passwd.sql.gz"), nil)
	if status == 200 {
		t.Error("경로가 섞인 이름으로 파일을 내줬다")
	}
}
