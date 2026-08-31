package monitor

import (
	"context"
	"testing"
	"time"

	"dbstudio/internal/dblog"
	"dbstudio/internal/dbx"
	"dbstudio/internal/metric"
	"dbstudio/internal/model"
	"dbstudio/internal/schema"
	"dbstudio/internal/store"
)

// fakeAdapter는 원하는 스키마를 그대로 돌려주는 어댑터다.
// 샘플링 기반 스키마의 변동을 Docker 없이 재현하기 위해 쓴다.
type fakeAdapter struct {
	kind   model.DBKind
	schema *schema.Schema
}

func (a *fakeAdapter) Kind() model.DBKind { return a.kind }
func (a *fakeAdapter) Capabilities() dbx.Capabilities {
	return dbx.Capabilities{Introspect: true, Monitor: true}
}
func (a *fakeAdapter) DefaultPort() int          { return 0 }
func (a *fakeAdapter) Validate(dbx.Target) error { return nil }
func (a *fakeAdapter) Ping(context.Context, dbx.Target) (*dbx.ServerInfo, error) {
	return &dbx.ServerInfo{Version: "fake"}, nil
}
func (a *fakeAdapter) Introspect(context.Context, dbx.Target) (*schema.Schema, error) {
	return a.schema, nil
}
func (a *fakeAdapter) Metrics(context.Context, dbx.Target) (*metric.Set, error) {
	return metric.NewSet(), nil
}
func (a *fakeAdapter) Logs(context.Context, dbx.Target, *dblog.Filter) (*dblog.Result, error) {
	return &dblog.Result{}, nil
}
func (a *fakeAdapter) ExecDDL(context.Context, dbx.Target, []string, dbx.ExecOptions) (*dbx.ExecReport, error) {
	return nil, dbx.ErrNotImplemented
}
func (a *fakeAdapter) Redacted(dbx.Target) string { return "fake://" }

// keyspaceSchema는 Redis introspect가 만드는 것과 같은 모양의 스키마다.
// 그룹 주석에 키 개수가 들어 있어 관측마다 값이 달라진다.
func keyspaceSchema(keyCount int) *schema.Schema {
	return &schema.Schema{
		Dialect: "redis", Shape: schema.ShapeKeyspace, Name: "db0",
		CapturedAt: time.Now().UTC(),
		Tables: []*schema.Table{{
			Name:    "session:*",
			Comment: "키 " + itoa(keyCount) + "개",
			Columns: []*schema.Column{{Name: "value", Position: 1, RawType: "string"}},
			Options: map[string]string{},
		}},
		Views: []*schema.View{},
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := ""
	for n > 0 {
		digits = string(rune('0'+n%10)) + digits
		n /= 10
	}
	return digits
}

// TestObservedStructureIsNotDrift는 샘플링 기반 스키마의 변동이
// 드리프트 이벤트를 만들지 않는지 확인한다.
//
// MongoDB/Redis의 스키마는 "관찰된 구조"다. 키 개수나 문서 샘플이 달라지면 지문이
// 달라지지만 그것은 앱 외부에서 스키마를 고친 것이 아니다. 게다가 diff는 관계형만
// 지원하므로 변경 목록이 항상 비어 있다. 이 구분이 없으면 15분마다
// "변경되었습니다 (0건)"라는 내용 없는 경고와 이벤트가 쌓이고,
// 사용자는 경고 자체를 무시하게 된다.
func TestObservedStructureIsNotDrift(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	if _, err := st.SeedBuiltinRules(ctx); err != nil {
		t.Fatalf("seed rules: %v", err)
	}

	_, conn, err := st.CreateServerWithDatabase(ctx,
		store.SaveServerParams{
			Name: "fake-redis", Kind: model.KindRedis, DefaultEnvironment: model.EnvDev,
			Host: "127.0.0.1", Port: 6379, Enabled: true,
		},
		store.SaveConnectionParams{
			ProjectID: testProjectID(t, ctx, st),
			Name:      "fake-redis", Environment: model.EnvDev, DatabaseName: "0", Enabled: true,
		})
	if err != nil {
		t.Fatalf("create connection: %v", err)
	}

	adapter := &fakeAdapter{kind: model.KindRedis, schema: keyspaceSchema(10)}
	target := dbx.Target{Conn: conn, Secret: &model.Secret{}}
	m := NewManager(st, DefaultConfig())
	rule := findDriftRule(t, ctx, st)

	// 1) 첫 확인은 기준선만 저장한다.
	if err := m.CheckDrift(ctx, conn, adapter, target, rule); err != nil {
		t.Fatalf("baseline: %v", err)
	}
	base, err := st.LatestSchemaSnapshot(ctx, conn.ID, false)
	if err != nil || base == nil {
		t.Fatalf("기준선이 저장되지 않았습니다: %v", err)
	}

	// 2) 키 개수만 달라진다 = 지문은 바뀌지만 외부 편집은 아니다.
	adapter.schema = keyspaceSchema(37)
	if base.Fingerprint == adapter.schema.Fingerprint() {
		t.Fatal("테스트 전제가 깨졌습니다: 지문이 달라져야 한다")
	}
	if err := m.CheckDrift(ctx, conn, adapter, target, rule); err != nil {
		t.Fatalf("second check: %v", err)
	}

	_, total, err := st.ListEvents(ctx, store.EventFilter{Kind: store.EventDrift})
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if total != 0 {
		t.Errorf("샘플링 변동으로 드리프트 이벤트가 %d건 생성되었습니다. "+
			"Mongo/Redis는 폴링마다 지문이 달라지므로 내용 없는 경고가 쌓입니다", total)
	}

	// 스냅샷은 갱신되어야 한다 — 이력으로서는 의미가 있다.
	latest, err := st.LatestSchemaSnapshot(ctx, conn.ID, false)
	if err != nil || latest == nil {
		t.Fatalf("스냅샷 조회 실패: %v", err)
	}
	if latest.Fingerprint == base.Fingerprint {
		t.Error("관찰된 구조가 달라졌는데 스냅샷이 갱신되지 않았습니다")
	}
	if len(latest.ChangeSummary) == 0 {
		t.Error("스냅샷에 이유가 적혀 있지 않습니다")
	}
}

// TestFingerprintChangeWithoutStructuralDiff는 관계형 스키마에서 지문만 달라진 경우
// (정규화 차이 등) 이벤트를 만들지 않는지 확인한다.
// "0건 변경되었습니다"는 읽는 사람에게 아무 정보도 주지 못한다.
func TestFingerprintChangeWithoutStructuralDiff(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	if _, err := st.SeedBuiltinRules(ctx); err != nil {
		t.Fatalf("seed rules: %v", err)
	}
	_, conn, err := st.CreateServerWithDatabase(ctx,
		store.SaveServerParams{
			Name: "fake-sql", Kind: model.KindSQLite, DefaultEnvironment: model.EnvDev,
			Enabled: true,
		},
		store.SaveConnectionParams{
			ProjectID: testProjectID(t, ctx, st),
			Name:      "fake-sql", Environment: model.EnvDev, DatabaseName: "x.db", Enabled: true,
		})
	if err != nil {
		t.Fatalf("create connection: %v", err)
	}

	// 관계형 스키마: 뷰 정의만 공백이 다르다. 지문에는 이름만 들어가므로
	// 지문이 같아지지 않도록 주석을 바꿔 지문 차이를 만든다.
	build := func(comment string) *schema.Schema {
		return &schema.Schema{
			Dialect: "sqlite", Shape: schema.ShapeRelational, Name: "main",
			CapturedAt: time.Now().UTC(),
			Tables: []*schema.Table{{
				Name: "t", Comment: comment,
				Columns: []*schema.Column{{Name: "id", Position: 1, RawType: "INTEGER"}},
				Options: map[string]string{},
			}},
			Views: []*schema.View{},
		}
	}
	adapter := &fakeAdapter{kind: model.KindSQLite, schema: build("before")}
	target := dbx.Target{Conn: conn, Secret: &model.Secret{}}
	m := NewManager(st, DefaultConfig())
	rule := findDriftRule(t, ctx, st)

	if err := m.CheckDrift(ctx, conn, adapter, target, rule); err != nil {
		t.Fatalf("baseline: %v", err)
	}
	// 관계형이므로 주석 변경은 실제 변경으로 보고되어야 한다.
	adapter.schema = build("after")
	if err := m.CheckDrift(ctx, conn, adapter, target, rule); err != nil {
		t.Fatalf("second check: %v", err)
	}
	_, total, err := st.ListEvents(ctx, store.EventFilter{Kind: store.EventDrift})
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if total != 1 {
		t.Errorf("관계형 스키마의 실제 변경이 %d건 보고되었습니다 (1건이어야 합니다). "+
			"조용해지는 것과 놓치는 것은 다르다", total)
	}
}
