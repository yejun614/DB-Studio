package monitor

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"dbstudio/internal/dbx"
	"dbstudio/internal/metric"
	"dbstudio/internal/model"
	"dbstudio/internal/schema"
	"dbstudio/internal/store"
)

// 이 시험들이 고정하는 것: **담당 노드가 있으면 폴러가 그 노드를 지나는가.**
//
// ── 왜 이 시험이 이번 작업의 핵심인가 ──────────────────────────────
// 고친 것은 한 줄이다(`collect`가 담당 노드를 보는가). 그런데 그 한 줄이 없어서
// 운영에서 이런 일이 났다:
//
//	같은 DB의 스키마·데이터·SQL 콘솔  → 정상 (담당 노드를 지난다)
//	모니터링 지표·버전 캡처·백업     → 1045 (마스터가 직접 접속한다)
//
// 화면별로 성공/실패가 갈리니 사람은 원인을 오류 문구에서 찾았고, 문구에는 어느 노드가
// 어느 IP로 시도했는지가 없어서 커넥션 설정과 클러스터 화면을 오가며 3시간을 썼다.
//
// 그래서 두 방향을 다 본다:
//
//	담당 노드가 있으면 → 맡긴다 (그리고 여기서는 접속하지 않는다)
//	담당 노드가 없으면 → 예전과 똑같이 여기서 한다
//
// 뒤쪽이 없으면 "항상 맡긴다"는 구현도 통과하고, 그때 증상은 단일 서버 모드에서
// 지표가 전부 사라지는 것이다.

// fakeCollector는 담당 노드 대신 답하는 가짜다.
//
// 몇 번 불렸는지 세는 이유: "맡겼다"와 "맡기고 여기서도 읽었다"는 다르다. 뒤쪽이면
// 지표가 두 번 수집되고(같은 DB에 두 번 붙는다) 카운터 변화율이 두 배로 튄다.
type fakeCollector struct {
	localID string
	// collectCalls는 지표를 맡긴 횟수다.
	collectCalls atomic.Int64
	// introspectCalls는 스키마를 맡긴 횟수다.
	introspectCalls atomic.Int64
	// got는 마지막으로 맡겨진 커넥션이다(어느 커넥션을 맡겼는지 본다).
	got *model.Connection
	// set / err는 노드가 돌려줄 답이다.
	set *metric.Set
	err error
	// schema / schemaErr는 스키마 답이다.
	schema    *schema.Schema
	schemaErr error
}

func (f *fakeCollector) LocalNodeID() string { return f.localID }

func (f *fakeCollector) Collect(_ context.Context, conn *model.Connection) (*metric.Set, error) {
	f.collectCalls.Add(1)
	f.got = conn
	if f.err != nil {
		return nil, f.err
	}
	if f.set != nil {
		return f.set.Clone(), nil
	}
	set := metric.NewSet()
	set.Gauge(metric.NameUp, 1, metric.UnitCount)
	return set, nil
}

func (f *fakeCollector) Introspect(_ context.Context, conn *model.Connection) (*schema.Schema, error) {
	f.introspectCalls.Add(1)
	f.got = conn
	if f.schemaErr != nil {
		return nil, f.schemaErr
	}
	if f.schema != nil {
		return f.schema, nil
	}
	return &schema.Schema{Dialect: "mysql", Name: "from-node"}, nil
}

// targetOf는 dbx.Target 하나를 만든다.
//
// 자격증명은 쓰지 않는다 — 이 시험들이 보는 것은 "누가 읽는가"이지 "무엇으로
// 접속하는가"가 아니다. 가짜 어댑터가 Target을 무시하므로 빈 Secret으로 충분하다.
func targetOf(conn *model.Connection) dbx.Target {
	return dbx.Target{Conn: conn, Secret: &model.Secret{Username: "appuser"}}
}

// connOnNode는 담당 노드를 지정한 커넥션 하나를 만든다.
//
// 담당 노드는 서버 등급 사실이다(P32). 그래서 서버에 넣는다 — 커넥션에 직접 넣으면
// 이 시험이 재현하려는 모양(서버를 따라가는 DB)이 아니게 된다.
func connOnNode(t *testing.T, st *store.Store, name, nodeID string) *model.Connection {
	t.Helper()
	ctx := context.Background()
	pj, err := st.CreateProject(ctx, store.SaveProjectParams{Name: "pj-" + name})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	pw := "pw"
	_, conn, err := st.CreateServerWithDatabase(ctx,
		store.SaveServerParams{
			ProjectID: pj.ID, Name: "srv-" + name, Kind: model.KindMySQL,
			Host: "10.0.0.7", Port: 3306, DefaultEnvironment: model.EnvDev,
			Options: model.Options{}, Tags: []string{}, Enabled: true,
			Username: "appuser", Password: &pw,
			NodeID: nodeID,
		},
		store.SaveConnectionParams{
			ProjectID: pj.ID, Name: name, Environment: model.EnvDev,
			DatabaseName: "appdb", Tags: []string{}, Enabled: true,
		})
	if err != nil {
		t.Fatalf("create connection: %v", err)
	}
	return conn
}

// TestPollGoesToResponsibleNode는 담당 노드가 있으면 그 노드가 읽는지 본다.
//
// 이 시험이 이번 작업의 본체다. 실패하면 운영에서 모니터링 화면이 다시 먹통이 된다.
func TestPollGoesToResponsibleNode(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	conn := connOnNode(t, st, "운영", "node-replica")

	// 가짜 어댑터는 **접속하면 안 된다.** 여기서 Metrics가 불리면 "맡기고 여기서도
	// 읽었다"는 뜻이고, 그때는 같은 DB에 두 번 붙는다.
	local := &countingAdapter{fakeAdapter: fakeAdapter{kind: model.KindMySQL}}

	node := &fakeCollector{localID: "node-master"}
	m := NewManager(st, DefaultConfig())
	m.SetCollector(node)

	m.pollOne(ctx, conn, local)

	if n := node.collectCalls.Load(); n != 1 {
		t.Errorf("담당 노드에 맡긴 횟수 = %d (기대 1)", n)
	}
	if n := local.calls.Load(); n != 0 {
		t.Errorf("맡겼는데 여기서도 접속했다 (로컬 수집 %d회) — 같은 DB에 두 번 붙는다", n)
	}
	if node.got == nil || node.got.ID != conn.ID {
		t.Errorf("맡긴 커넥션이 다르다: %+v (기대 %s)", node.got, conn.ID)
	}
}

// TestPollStaysLocalWhenThisNodeIsResponsible는 담당 노드가 나면 왕복하지 않는지 본다.
//
// 왕복하면 담당 노드가 자기 자신에게 요청을 보내는 모양이 되고(주소가 자기 주소),
// 클러스터를 쓰지 않는 배포에서는 아예 갈 곳이 없다.
func TestPollStaysLocalWhenThisNodeIsResponsible(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	conn := connOnNode(t, st, "로컬", "node-master")

	local := &countingAdapter{fakeAdapter: fakeAdapter{kind: model.KindMySQL}}
	node := &fakeCollector{localID: "node-master"}
	m := NewManager(st, DefaultConfig())
	m.SetCollector(node)

	m.pollOne(ctx, conn, local)

	if n := node.collectCalls.Load(); n != 0 {
		t.Errorf("담당이 나인데 맡겼다 (맡긴 횟수 %d) — 자기 자신을 부르는 모양이 된다", n)
	}
	if n := local.calls.Load(); n != 1 {
		t.Errorf("로컬 수집 횟수 = %d (기대 1)", n)
	}
}

// TestPollStaysLocalWithoutHomeNode는 담당 노드가 없으면 예전과 같은지 본다.
//
// 담당 노드를 지정하지 않은 DB가 대부분이다(단일 서버 배포는 전부 그렇다). 여기서
// 무언가를 맡기려 하면 그 배포들이 전부 조용히 지표를 잃는다.
func TestPollStaysLocalWithoutHomeNode(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	conn := connOnNode(t, st, "담당없음", "")

	local := &countingAdapter{fakeAdapter: fakeAdapter{kind: model.KindMySQL}}
	node := &fakeCollector{localID: "node-master"}
	m := NewManager(st, DefaultConfig())
	m.SetCollector(node)

	m.pollOne(ctx, conn, local)

	if n := node.collectCalls.Load(); n != 0 {
		t.Errorf("담당 노드가 없는데 맡겼다 (맡긴 횟수 %d)", n)
	}
	if n := local.calls.Load(); n != 1 {
		t.Errorf("로컬 수집 횟수 = %d (기대 1)", n)
	}
}

// TestPollWithoutCollectorIsUnchanged는 통로를 붙이지 않아도 도는지 본다.
//
// 클러스터를 쓰지 않는 배포와 기존 시험이 이 상태다. 이 시험이 없으면 "collector가
// nil이면 죽는다"는 구현도 통과한다.
func TestPollWithoutCollectorIsUnchanged(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	conn := connOnNode(t, st, "클러스터아님", "node-replica")

	local := &countingAdapter{fakeAdapter: fakeAdapter{kind: model.KindMySQL}}
	m := NewManager(st, DefaultConfig())
	// SetCollector를 부르지 않는다.

	m.pollOne(ctx, conn, local)

	if n := local.calls.Load(); n != 1 {
		t.Errorf("로컬 수집 횟수 = %d (기대 1)", n)
	}
}

// TestNodeUnreachableIsRecordedAsFailure는 노드에 못 닿은 것을 **실패로 적는지** 본다.
//
// 건너뛰면 화면에는 마지막 성공 값이 남고 사람은 그것을 "지금도 정상"으로 읽는다.
// 담당 노드에 못 물어본 것도 그 DB를 볼 수 없는 상태이므로 실패다.
//
// 그리고 그 실패 문구에 **노드 이름**이 들어가야 한다 — 이번 장애에서 3시간을 쓴
// 이유가 문구에 그 값이 없어서였다.
func TestNodeUnreachableIsRecordedAsFailure(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	conn := connOnNode(t, st, "운영", "node-replica")

	// 노드 등록부에 이름을 넣는다. 문구에 ID가 아니라 이름이 나와야 한다.
	if err := st.UpsertClusterNode(ctx, store.ClusterNode{
		ID: "node-replica", Name: "replica-b", Role: store.NodeRoleReplica,
	}); err != nil {
		t.Fatalf("upsert node: %v", err)
	}

	local := &countingAdapter{fakeAdapter: fakeAdapter{kind: model.KindMySQL}}
	node := &fakeCollector{localID: "node-master", err: errors.New("담당 노드에 닿지 못했습니다")}
	m := NewManager(st, DefaultConfig())
	m.SetCollector(node)

	set, ok := m.collect(ctx, conn, local)
	if !ok {
		t.Fatal("수집 결과가 버려졌다 — 건너뛰면 화면에 옛 값이 남는다")
	}
	up, has := set.Get(metric.NameUp)
	if !has || up.Value != 0 {
		t.Errorf("up = %v (있음 %v), 기대 0", up, has)
	}
	if len(set.Notes) == 0 {
		t.Fatal("실패 이유가 없다 — 화면이 무엇을 말할 수 없다")
	}
	if !strings.Contains(set.Notes[0], "replica-b") {
		t.Errorf("실패 문구에 노드 이름이 없다: %q", set.Notes[0])
	}
}

// TestDriftIntrospectGoesToResponsibleNode는 드리프트 확인도 담당 노드를 지나는지 본다.
//
// 지표만 고치고 여기를 남겨 두면 증상이 **"모니터링은 되는데 드리프트만 안 된다"** 로
// 바뀔 뿐이다. 같은 원인(마스터가 그 사설망에 닿지 못한다)에서 나오는 일이라
// 한쪽만 고치는 것은 반쪽 수정이다.
func TestDriftIntrospectGoesToResponsibleNode(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	conn := connOnNode(t, st, "운영", "node-replica")

	local := &countingAdapter{fakeAdapter: fakeAdapter{kind: model.KindMySQL}}
	node := &fakeCollector{localID: "node-master"}
	m := NewManager(st, DefaultConfig())
	m.SetCollector(node)

	sc, err := m.introspectForDrift(ctx, conn, local, targetOf(conn))
	if err != nil {
		t.Fatalf("introspect: %v", err)
	}
	if n := node.introspectCalls.Load(); n != 1 {
		t.Errorf("담당 노드에 맡긴 횟수 = %d (기대 1)", n)
	}
	if sc == nil || sc.Name != "from-node" {
		t.Errorf("노드가 준 스키마가 아니다: %+v", sc)
	}
}

// TestDriftIntrospectStaysLocalWithoutHomeNode는 담당이 없으면 여기서 읽는지 본다.
func TestDriftIntrospectStaysLocalWithoutHomeNode(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	conn := connOnNode(t, st, "담당없음", "")

	want := &schema.Schema{Dialect: "mysql", Name: "local"}
	local := &countingAdapter{fakeAdapter: fakeAdapter{kind: model.KindMySQL, schema: want}}
	node := &fakeCollector{localID: "node-master"}
	m := NewManager(st, DefaultConfig())
	m.SetCollector(node)

	sc, err := m.introspectForDrift(ctx, conn, local, targetOf(conn))
	if err != nil {
		t.Fatalf("introspect: %v", err)
	}
	if n := node.introspectCalls.Load(); n != 0 {
		t.Errorf("담당 노드가 없는데 맡겼다 (맡긴 횟수 %d)", n)
	}
	if sc == nil || sc.Name != "local" {
		t.Errorf("로컬 스키마가 아니다: %+v", sc)
	}
}

// TestNodeIntrospectFailureIsNotSwallowed는 노드가 스키마를 못 준 것을 삼키지 않는지 본다.
//
// 스키마가 없으면 비교할 것이 없다. "변경 없음"으로 접으면 감시가 조용히 멈추고,
// 그 사실은 다음에 실제 변경이 일어나 아무도 몰랐을 때 드러난다.
func TestNodeIntrospectFailureIsNotSwallowed(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	conn := connOnNode(t, st, "운영", "node-replica")

	local := &countingAdapter{fakeAdapter: fakeAdapter{kind: model.KindMySQL}}
	node := &fakeCollector{localID: "node-master", schemaErr: errors.New("담당 노드에 닿지 못했습니다")}
	m := NewManager(st, DefaultConfig())
	m.SetCollector(node)

	if _, err := m.introspectForDrift(ctx, conn, local, targetOf(conn)); err == nil {
		t.Error("노드 실패가 삼켜졌다 — 감시가 조용히 멈춘다")
	}
}
