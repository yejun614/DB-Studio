package store

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"dbstudio/internal/crypto"
	"dbstudio/internal/model"
)

func clusterFixture(t *testing.T, name string) (context.Context, *Store) {
	t.Helper()
	ctx := context.Background()
	box, err := crypto.NewSecretBox(make([]byte, 32))
	if err != nil {
		t.Fatalf("secret box: %v", err)
	}
	st, err := Open(ctx, filepath.Join(t.TempDir(), name+".db"), box)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return ctx, st
}

// seedUser는 복제해 볼 행 하나를 만든다.
func seedUser(t *testing.T, ctx context.Context, st *Store, username string) string {
	t.Helper()
	u, err := st.CreateUser(ctx, CreateUserParams{
		Username: username, DisplayName: username, Role: model.RoleMember,
		PasswordHash: "x",
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	return u.ID
}

// TestReplicationRoundTrip은 마스터의 변경이 리플리카에 그대로 도착하는지 본다.
//
// 이 시험이 지키는 것: 복제는 "대체로 맞는" 것으로는 쓸모가 없다. 한 행이라도 빠지면
// 두 노드는 조용히 다른 답을 하고, 그 사실은 한참 뒤에야 드러난다.
func TestReplicationRoundTrip(t *testing.T) {
	ctx, master := clusterFixture(t, "master")
	_, replica := clusterFixture(t, "replica")

	if _, err := master.InstallReplTriggers(ctx); err != nil {
		t.Fatalf("트리거 설치: %v", err)
	}

	id := seedUser(t, ctx, master, "alice")

	changes, err := master.ReplChanges(ctx, 0, 100)
	if err != nil {
		t.Fatalf("changes: %v", err)
	}
	if len(changes) == 0 {
		t.Fatal("변경이 기록되지 않았습니다 (트리거가 동작하지 않습니다)")
	}
	last, err := replica.ApplyReplChanges(ctx, changes)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if last != changes[len(changes)-1].Seq {
		t.Errorf("적용 지점 %d, 기대 %d", last, changes[len(changes)-1].Seq)
	}

	got, err := replica.GetUser(ctx, id)
	if err != nil {
		t.Fatalf("리플리카에서 사용자를 찾지 못했습니다: %v", err)
	}
	if got.Username != "alice" {
		t.Errorf("사용자 이름 %q, 기대 alice", got.Username)
	}

	// 적용 지점은 다시 읽어도 남아 있어야 한다. 재시작하면 그 지점부터 이어 간다.
	applied, err := replica.ReplApplied(ctx)
	if err != nil || applied != last {
		t.Errorf("저장된 적용 지점 %d(%v), 기대 %d", applied, err, last)
	}

	// 삭제도 따라와야 한다. upsert만 복제하면 리플리카에는 지운 것이 살아남는다.
	if err := master.DeleteUser(ctx, id); err != nil {
		t.Fatalf("delete: %v", err)
	}
	more, err := master.ReplChanges(ctx, last, 100)
	if err != nil {
		t.Fatalf("changes 2: %v", err)
	}
	if _, err := replica.ApplyReplChanges(ctx, more); err != nil {
		t.Fatalf("apply 2: %v", err)
	}
	if _, err := replica.GetUser(ctx, id); err == nil {
		t.Error("삭제가 복제되지 않았습니다")
	}
}

// TestSnapshotBootstrap은 뒤처진 노드를 스냅샷으로 맞추는 경로를 본다.
func TestSnapshotBootstrap(t *testing.T) {
	ctx, master := clusterFixture(t, "master")
	_, replica := clusterFixture(t, "replica")

	if _, err := master.InstallReplTriggers(ctx); err != nil {
		t.Fatalf("트리거: %v", err)
	}
	seedUser(t, ctx, master, "bob")

	// 리플리카에는 마스터에 없는 행이 있다. 스냅샷은 그것까지 정리해야 한다 —
	// 남겨 두면 그 노드에만 있는 유령 계정이 로그인할 수 있는 상태로 남는다.
	ghost := seedUser(t, ctx, replica, "ghost")

	path := filepath.Join(t.TempDir(), "snap.db")
	if err := master.SnapshotTo(ctx, path); err != nil {
		t.Fatalf("스냅샷: %v", err)
	}
	seq, err := replica.LoadSnapshot(ctx, path)
	if err != nil {
		t.Fatalf("스냅샷 적용: %v", err)
	}
	_, maxSeq, err := master.ReplBounds(ctx)
	if err != nil {
		t.Fatalf("bounds: %v", err)
	}
	if seq != maxSeq {
		t.Errorf("스냅샷 지점 %d, 마스터 %d", seq, maxSeq)
	}
	if _, err := replica.GetUserByUsername(ctx, "bob"); err != nil {
		t.Errorf("마스터의 행이 오지 않았습니다: %v", err)
	}
	if _, err := replica.GetUser(ctx, ghost); err == nil {
		t.Error("리플리카에만 있던 행이 남아 있습니다")
	}

	// 스냅샷 이후의 변경은 그 지점부터 이어 붙는다.
	seedUser(t, ctx, master, "carol")
	changes, err := master.ReplChanges(ctx, seq, 100)
	if err != nil {
		t.Fatalf("changes: %v", err)
	}
	if _, err := replica.ApplyReplChanges(ctx, changes); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if _, err := replica.GetUserByUsername(ctx, "carol"); err != nil {
		t.Errorf("스냅샷 이후 변경이 오지 않았습니다: %v", err)
	}
}

// TestReplLogPrune은 로그 정리가 최신 기록을 건드리지 않는지 본다.
func TestReplLogPrune(t *testing.T) {
	ctx, master := clusterFixture(t, "master")
	if _, err := master.InstallReplTriggers(ctx); err != nil {
		t.Fatalf("트리거: %v", err)
	}
	for i := range 5 {
		seedUser(t, ctx, master, fmt.Sprintf("u%d", i))
	}
	minBefore, maxBefore, _ := master.ReplBounds(ctx)
	if minBefore == 0 || maxBefore == 0 {
		t.Fatal("복제 로그가 비어 있습니다")
	}
	if _, err := master.PruneReplLog(ctx, 0, 2); err != nil {
		t.Fatalf("prune: %v", err)
	}
	minAfter, maxAfter, _ := master.ReplBounds(ctx)
	if maxAfter != maxBefore {
		t.Errorf("정리가 최신 로그를 지웠습니다: %d → %d", maxBefore, maxAfter)
	}
	if minAfter <= minBefore {
		t.Errorf("오래된 로그가 지워지지 않았습니다: min %d → %d", minBefore, minAfter)
	}
}

// TestReplicaModeDoesNotWrite는 리플리카가 자기 DB에 남기지 않는 것들을 확인한다.
func TestReplicaModeDoesNotWrite(t *testing.T) {
	ctx, st := clusterFixture(t, "replicamode")

	var forwarded int
	st.SetReplicaMode(func(ctx context.Context, p AuditParams) error {
		forwarded++
		return nil
	})
	if err := st.Audit(ctx, AuditParams{Action: "test.action"}); err != nil {
		t.Fatalf("audit: %v", err)
	}
	if forwarded != 1 {
		t.Errorf("감사 기록이 마스터로 전달되지 않았습니다 (%d건)", forwarded)
	}
	var n int
	if err := st.DB().QueryRowContext(ctx, `SELECT count(*) FROM audit_logs`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("리플리카가 감사 로그를 로컬에 썼습니다 (%d건)", n)
	}
}

// TestClusterNodeRegistry는 노드 목록의 등록·하트비트·내리기를 본다.
func TestClusterNodeRegistry(t *testing.T) {
	ctx, st := clusterFixture(t, "nodes")

	if err := st.UpsertClusterNode(ctx, ClusterNode{
		ID: "n1", Name: "seoul", Role: NodeRoleMaster, Address: "http://a:8080", Version: "1.0",
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := st.UpsertClusterNode(ctx, ClusterNode{
		ID: "n2", Name: "busan", Role: NodeRoleReplica, Address: "http://b:8080",
	}); err != nil {
		t.Fatalf("upsert 2: %v", err)
	}
	if err := st.TouchClusterNode(ctx, "n2", 42, `{"cpu":{"usedPercent":12}}`); err != nil {
		t.Fatalf("touch: %v", err)
	}
	nodes, err := st.ListClusterNodes(ctx)
	if err != nil || len(nodes) != 2 {
		t.Fatalf("목록 %d개(%v), 기대 2개", len(nodes), err)
	}
	// 마스터가 먼저 온다 — 화면에서 기준 노드를 찾으려고 목록을 훑지 않아도 되게.
	if nodes[0].Role != NodeRoleMaster {
		t.Errorf("첫 노드가 마스터가 아닙니다: %s", nodes[0].Role)
	}
	n2, err := st.GetClusterNode(ctx, "n2")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if n2.AppliedSeq != 42 || n2.HostSnapshot == "" || n2.HostAt == nil {
		t.Errorf("하트비트가 반영되지 않았습니다: %+v", n2)
	}

	// 하트비트에 호스트 상태가 없으면 마지막 값을 지우지 않는다.
	if err := st.TouchClusterNode(ctx, "n2", 43, ""); err != nil {
		t.Fatalf("touch 2: %v", err)
	}
	again, _ := st.GetClusterNode(ctx, "n2")
	if again.HostSnapshot == "" {
		t.Error("호스트 상태가 빈 하트비트에 지워졌습니다")
	}

	if err := st.TouchClusterNode(ctx, "없는노드", 1, ""); err != ErrNotFound {
		t.Errorf("모르는 노드의 하트비트: %v, 기대 ErrNotFound", err)
	}

	if err := st.RemoveClusterNode(ctx, "n2"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	left, _ := st.GetClusterNode(ctx, "n2")
	if left.Status != "left" {
		t.Errorf("상태 %q, 기대 left", left.Status)
	}
	// 행을 지우지 않는다: 이벤트와 커넥션이 이 ID를 가리키고 있다.
	if left.Name != "busan" {
		t.Error("내려간 노드의 정보가 사라졌습니다")
	}
}

// TestStaleClusterNodes는 소식이 끊긴 노드를 골라내는지 본다.
func TestStaleClusterNodes(t *testing.T) {
	ctx, st := clusterFixture(t, "stale")
	if err := st.UpsertClusterNode(ctx, ClusterNode{ID: "n1", Name: "a", Role: NodeRoleReplica}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	old := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano)
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE cluster_nodes SET last_seen_at = ? WHERE id = 'n1'`, old); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	stale, err := st.StaleClusterNodes(ctx, time.Minute)
	if err != nil || len(stale) != 1 {
		t.Fatalf("끊긴 노드 %d개(%v), 기대 1개", len(stale), err)
	}
	fresh, err := st.StaleClusterNodes(ctx, 2*time.Hour)
	if err != nil || len(fresh) != 0 {
		t.Fatalf("기준이 넉넉할 때 %d개(%v), 기대 0개", len(fresh), err)
	}
}

// TestReplicationKeepsChildRows는 부모 행의 복제가 자식 행을 지우지 않는지 본다.
//
// 이 시험이 있는 이유: 처음 구현은 INSERT OR REPLACE로 행을 맞췄다. REPLACE는 충돌하는
// 행을 **지우고 다시 넣기** 때문에 ON DELETE CASCADE가 깨어나, 사용자 행 하나가 복제될
// 때마다 그 사람의 세션이 통째로 사라졌다. 증상은 "그 노드에서만 로그인이 풀린다"였고,
// 오류도 경고도 남지 않았다.
func TestReplicationKeepsChildRows(t *testing.T) {
	ctx, master := clusterFixture(t, "master")
	_, replica := clusterFixture(t, "replica")
	if _, err := master.InstallReplTriggers(ctx); err != nil {
		t.Fatalf("트리거: %v", err)
	}

	id := seedUser(t, ctx, master, "alice")
	if _, err := master.CreateSession(ctx, "tok-1", id, time.Hour, "127.0.0.1", "agent"); err != nil {
		t.Fatalf("세션: %v", err)
	}
	// 세션이 생긴 **뒤에** 사용자 행이 다시 바뀌는 상황을 만든다(로그인 시각 갱신 등
	// 실제로 매 로그인마다 일어나는 순서다).
	if err := master.TouchLastLogin(ctx, id, "127.0.0.1"); err != nil {
		t.Fatalf("로그인 기록: %v", err)
	}

	changes, err := master.ReplChanges(ctx, 0, 100)
	if err != nil {
		t.Fatalf("changes: %v", err)
	}
	if _, err := replica.ApplyReplChanges(ctx, changes); err != nil {
		t.Fatalf("apply: %v", err)
	}

	var n int
	if err := replica.DB().QueryRowContext(ctx, `SELECT count(*) FROM sessions`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("리플리카의 세션 %d개, 기대 1개 (부모 행 복제가 자식을 지웠습니다)", n)
	}
}
