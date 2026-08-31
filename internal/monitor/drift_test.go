package monitor

import (
	"context"
	"flag"
	"os"
	"strings"
	"testing"
	"time"

	"dbstudio/internal/dbx"
	"dbstudio/internal/model"
	"dbstudio/internal/store"
)

var runIntegration = flag.Bool("integration", false, "run drift tests against docker databases")

// TestDriftDetection은 스키마 외부 변경 감지를 실제 DB에 대해 검증한다.
//
// 이 기능의 목적은 "이 앱을 거치지 않은 변경"을 잡는 것이므로, 테스트도 앱의
// 마이그레이션 경로가 아니라 드라이버로 직접 DDL을 실행해 외부 변경을 재현한다.
//
// 검증 항목:
//  1. 첫 확인은 기준선만 저장하고 이벤트를 만들지 않는다 (커넥션 등록마다 경고가 뜨면 안 된다)
//  2. 변경이 없으면 스냅샷을 새로 쌓지 않는다 (폴링마다 저장하면 저장소가 커진다)
//  3. 외부 변경이 있으면 감지하고 무엇이 바뀌었는지 요약한다
//  4. 파괴적 변경이면 심각도를 올린다
//  5. 드리프트 이벤트는 누적하지 않고 매번 새로 만든다 (변경 시점이 뭉개지면 안 된다)
func TestDriftDetection(t *testing.T) {
	if !*runIntegration && os.Getenv("DBSTUDIO_INTEGRATION") == "" {
		t.Skip("통합 테스트: -integration 플래그 또는 DBSTUDIO_INTEGRATION 환경변수 필요")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	st := newTestStore(t)
	// 전용 데이터베이스를 쓴다. 지문은 데이터베이스 내 모든 테이블을 포함하므로
	// 다른 테스트와 같은 DB를 공유하면 "외부 변경"이 서로 섞인다.
	// go test는 패키지를 병렬로 실행하므로 이 격리가 없으면 결과가 비결정적이다.
	conn, adapter, target := newIsolatedMySQL(t, ctx, st, "drift_detect", model.EnvProd)

	// 드라이버로 직접 DDL을 실행한다. 앱의 마이그레이션 경로를 쓰지 않는 것이 요점이다.
	exec := newDirectExecutor(t, adapter, target)
	exec.run(ctx, `CREATE TABLE drift_probe_tbl (
		id BIGINT NOT NULL AUTO_INCREMENT,
		name VARCHAR(64) NOT NULL,
		PRIMARY KEY (id)
	) ENGINE=InnoDB`)

	if _, err := st.SeedBuiltinRules(ctx); err != nil {
		t.Fatalf("seed rules: %v", err)
	}
	m := NewManager(st, DefaultConfig())
	driftRule := findDriftRule(t, ctx, st)

	// ---- 1. 첫 확인: 기준선만 저장 ----
	if err := m.CheckDrift(ctx, conn, adapter, target, driftRule); err != nil {
		t.Fatalf("baseline check: %v", err)
	}
	baseline, err := st.LatestSchemaSnapshot(ctx, conn.ID, false)
	if err != nil || baseline == nil {
		t.Fatalf("기준선 스냅샷이 저장되지 않았습니다: %v", err)
	}
	if len(baseline.ChangeSummary) != 1 || !strings.Contains(baseline.ChangeSummary[0], "기준선") {
		t.Errorf("기준선 표시가 없습니다: %v", baseline.ChangeSummary)
	}
	if _, total, _ := st.ListEvents(ctx, store.EventFilter{Kind: store.EventDrift}); total != 0 {
		t.Errorf("첫 확인에서 드리프트 이벤트가 생성되었습니다 (%d건). "+
			"커넥션을 등록할 때마다 경고가 뜨게 됩니다", total)
	}

	// ---- 2. 변경 없으면 스냅샷을 쌓지 않는다 ----
	if err := m.CheckDrift(ctx, conn, adapter, target, driftRule); err != nil {
		t.Fatalf("no-change check: %v", err)
	}
	snaps, err := st.ListSchemaSnapshots(ctx, conn.ID, 10)
	if err != nil {
		t.Fatalf("list snapshots: %v", err)
	}
	if len(snaps) != 1 {
		t.Errorf("변경이 없는데 스냅샷이 %d개로 늘었습니다", len(snaps))
	}

	// ---- 3. 안전한 외부 변경(컬럼 추가) 감지 ----
	exec.run(ctx, `ALTER TABLE drift_probe_tbl ADD COLUMN email VARCHAR(255) NULL`)

	if err := m.CheckDrift(ctx, conn, adapter, target, driftRule); err != nil {
		t.Fatalf("additive change check: %v", err)
	}
	after, err := st.LatestSchemaSnapshot(ctx, conn.ID, false)
	if err != nil {
		t.Fatalf("latest snapshot: %v", err)
	}
	if after.ID == baseline.ID {
		t.Fatal("외부 변경 후에도 새 스냅샷이 저장되지 않았습니다")
	}
	if after.Fingerprint == baseline.Fingerprint {
		t.Error("구조가 바뀌었는데 지문이 동일합니다")
	}
	if !containsSubstring(after.ChangeSummary, "email") {
		t.Errorf("변경 요약에 추가된 컬럼이 없습니다: %v", after.ChangeSummary)
	}

	events, total, err := st.ListEvents(ctx, store.EventFilter{Kind: store.EventDrift})
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if total != 1 {
		t.Fatalf("드리프트 이벤트가 1건이어야 합니다: %d건", total)
	}
	additive := events[0]
	if additive.Severity != store.SeverityWarning {
		t.Errorf("컬럼 추가는 경고여야 합니다: %s", additive.Severity)
	}
	changes, ok := additive.Detail["changes"].([]any)
	if !ok || len(changes) == 0 {
		t.Errorf("이벤트 상세에 변경 목록이 없습니다: %v", additive.Detail)
	}
	if dc, ok := additive.Detail["destructive"].(float64); !ok || dc != 0 {
		t.Errorf("컬럼 추가는 파괴적 변경 0건이어야 합니다: %v", additive.Detail["destructive"])
	}
	t.Logf("추가 변경 감지: %s", additive.Message)

	// ---- 4. 파괴적 외부 변경(컬럼 삭제)은 심각도를 올린다 ----
	exec.run(ctx, `ALTER TABLE drift_probe_tbl DROP COLUMN email`)

	if err := m.CheckDrift(ctx, conn, adapter, target, driftRule); err != nil {
		t.Fatalf("destructive change check: %v", err)
	}
	events, total, err = st.ListEvents(ctx, store.EventFilter{Kind: store.EventDrift})
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	// ---- 5. 이전 드리프트 이벤트는 해소되고 새 이벤트가 열려야 한다 ----
	if total != 2 {
		t.Fatalf("드리프트 이벤트가 2건이어야 합니다 (변경마다 새 이벤트): %d건", total)
	}
	var open, resolved *store.Event
	for _, e := range events {
		if e.State == "open" {
			open = e
		} else {
			resolved = e
		}
	}
	if open == nil || resolved == nil {
		t.Fatalf("이전 이벤트는 해소되고 새 이벤트가 열려 있어야 합니다: %+v", events)
	}
	if resolved.ID != additive.ID {
		t.Errorf("해소된 이벤트가 이전 이벤트가 아닙니다: %d vs %d", resolved.ID, additive.ID)
	}
	if open.Severity != store.SeverityCritical {
		t.Errorf("파괴적 외부 변경은 심각으로 올라가야 합니다: %s", open.Severity)
	}
	if dc, ok := open.Detail["destructive"].(float64); !ok || dc < 1 {
		t.Errorf("파괴적 변경 건수가 기록되지 않았습니다: %v", open.Detail["destructive"])
	}
	t.Logf("파괴적 변경 감지: [%s] %s", open.Severity, open.Message)

	// ---- 6. CheckDriftByID(사용자 수동 확인) 경로 ----
	exec.run(ctx, `ALTER TABLE drift_probe_tbl ADD COLUMN note TEXT NULL`)

	snap, changed, err := m.CheckDriftByID(ctx, conn.ID)
	if err != nil {
		t.Fatalf("manual check: %v", err)
	}
	if !changed {
		t.Error("수동 확인이 변경을 감지하지 못했습니다")
	}
	if !containsSubstring(snap.ChangeSummary, "note") {
		t.Errorf("수동 확인의 변경 요약이 잘못되었습니다: %v", snap.ChangeSummary)
	}

	// 변경이 없으면 changed=false여야 한다.
	_, changed, err = m.CheckDriftByID(ctx, conn.ID)
	if err != nil {
		t.Fatalf("manual recheck: %v", err)
	}
	if changed {
		t.Error("변경이 없는데 수동 확인이 변경으로 보고했습니다")
	}
}

// TestDriftWithoutRule은 룰이 없으면 스냅샷만 갱신하고 이벤트를 만들지 않는지 확인한다.
func TestDriftWithoutRule(t *testing.T) {
	if !*runIntegration && os.Getenv("DBSTUDIO_INTEGRATION") == "" {
		t.Skip("통합 테스트: -integration 플래그 필요")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	st := newTestStore(t)
	conn, adapter, target := newIsolatedMySQL(t, ctx, st, "drift_norule", model.EnvDev)

	exec := newDirectExecutor(t, adapter, target)
	exec.run(ctx, `CREATE TABLE drift_norule_tbl (id INT NOT NULL, PRIMARY KEY (id))`)

	m := NewManager(st, DefaultConfig())
	// rule=nil: 룰이 비활성이거나 없는 상황
	if err := m.CheckDrift(ctx, conn, adapter, target, nil); err != nil {
		t.Fatalf("baseline: %v", err)
	}
	exec.run(ctx, `ALTER TABLE drift_norule_tbl ADD COLUMN extra INT NULL`)
	if err := m.CheckDrift(ctx, conn, adapter, target, nil); err != nil {
		t.Fatalf("change: %v", err)
	}

	snaps, err := st.ListSchemaSnapshots(ctx, conn.ID, 10)
	if err != nil {
		t.Fatalf("list snapshots: %v", err)
	}
	if len(snaps) != 2 {
		t.Errorf("룰이 없어도 스냅샷은 갱신되어야 합니다: %d개", len(snaps))
	}
	if _, total, _ := st.ListEvents(ctx, store.EventFilter{Kind: store.EventDrift}); total != 0 {
		t.Errorf("룰이 없으면 이벤트를 만들지 않아야 합니다: %d건", total)
	}
}

// ---------- 헬퍼 ----------

// newIsolatedMySQL은 테스트 전용 MySQL 데이터베이스를 만들고 그것을 가리키는
// 커넥션을 등록한다.
//
// 전용 DB가 필요한 이유: 스키마 지문은 데이터베이스 내 모든 테이블을 포함한다.
// 여러 테스트가 같은 DB를 공유하면 한 테스트의 테이블 생성이 다른 테스트에게는
// "외부 변경"으로 보인다. go test는 패키지를 병렬 실행하므로 이 격리 없이는
// 실행 조합에 따라 결과가 달라진다.
func newIsolatedMySQL(t *testing.T, ctx context.Context, st *store.Store, name string, env model.Environment) (*model.Connection, dbx.Adapter, dbx.Target) {
	t.Helper()

	adapter, err := dbx.Get(model.KindMySQL)
	if err != nil {
		t.Fatalf("adapter: %v", err)
	}
	pw := "rootpw123"

	// 먼저 기본 DB로 접속해 전용 DB를 만든다.
	bootstrapConn := &model.Connection{
		Kind: model.KindMySQL, Host: "127.0.0.1", Port: 13306,
		DatabaseName: "appdb", Options: model.Options{},
	}
	bootstrapTarget := dbx.Target{
		Conn: bootstrapConn, Secret: &model.Secret{Username: "root", Password: pw},
	}
	if _, err := adapter.Ping(ctx, bootstrapTarget); err != nil {
		t.Skipf("MySQL 접속 불가 (컨테이너 미실행?): %v", err)
	}

	dbName := "dbstudio_" + name
	if err := dbx.ExecRaw(ctx, adapter, bootstrapTarget, "DROP DATABASE IF EXISTS "+dbName); err != nil {
		t.Fatalf("전용 DB 정리 실패: %v", err)
	}
	if err := dbx.ExecRaw(ctx, adapter, bootstrapTarget, "CREATE DATABASE "+dbName); err != nil {
		t.Fatalf("전용 DB 생성 실패: %v", err)
	}
	t.Cleanup(func() {
		_ = dbx.ExecRaw(context.Background(), adapter, bootstrapTarget, "DROP DATABASE IF EXISTS "+dbName)
	})

	_, conn, err := st.CreateServerWithDatabase(ctx,
		store.SaveServerParams{ProjectID: testProjectID(t, ctx, st),
			Name: name, Kind: model.KindMySQL, DefaultEnvironment: env,
			Host: "127.0.0.1", Port: 13306,
			Options: model.Options{}, Tags: []string{}, Enabled: true,
			Username: "root", Password: &pw,
		},
		store.SaveConnectionParams{
			ProjectID: testProjectID(t, ctx, st),
			Name:      name, Environment: env, DatabaseName: dbName,
			Tags: []string{}, Enabled: true,
		})
	if err != nil {
		t.Fatalf("create connection: %v", err)
	}
	secret, err := st.GetSecret(ctx, conn.ID)
	if err != nil {
		t.Fatalf("secret: %v", err)
	}
	return conn, adapter, dbx.Target{Conn: conn, Secret: secret}
}

// directExecutor는 앱의 마이그레이션 경로를 우회해 DDL을 직접 실행한다.
// 외부 편집을 재현하는 것이 목적이므로 이 우회가 의도적이다.
type directExecutor struct {
	t       *testing.T
	adapter dbx.Adapter
	target  dbx.Target
}

func newDirectExecutor(t *testing.T, adapter dbx.Adapter, target dbx.Target) *directExecutor {
	return &directExecutor{t: t, adapter: adapter, target: target}
}

func (e *directExecutor) run(ctx context.Context, sql string) {
	e.t.Helper()
	if err := dbx.ExecRaw(ctx, e.adapter, e.target, sql); err != nil {
		// DROP IF EXISTS 류는 실패해도 무해하므로 로그만 남긴다.
		if strings.Contains(strings.ToUpper(sql), "IF EXISTS") {
			return
		}
		e.t.Fatalf("DDL 실행 실패:\n%s\n오류: %v", sql, err)
	}
}

func findDriftRule(t *testing.T, ctx context.Context, st *store.Store) *store.Rule {
	t.Helper()
	rules, err := st.ListRules(ctx)
	if err != nil {
		t.Fatalf("list rules: %v", err)
	}
	for _, r := range rules {
		if r.Kind == store.EventDrift {
			return r
		}
	}
	t.Fatal("드리프트 룰을 찾지 못했습니다")
	return nil
}

func containsSubstring(list []string, needle string) bool {
	for _, s := range list {
		if strings.Contains(s, needle) {
			return true
		}
	}
	return false
}
