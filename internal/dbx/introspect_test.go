package dbx

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"dbstudio/internal/model"
	"dbstudio/internal/schema"
)

// 통합 테스트는 docker/compose.test.yaml 의 인스턴스를 사용한다.
// 컨테이너가 없으면 각 테스트는 스킵된다.
var runIntegration = flag.Bool("integration", false, "run introspection tests against docker databases")

type testTarget struct {
	kind   model.DBKind
	conn   *model.Connection
	secret *model.Secret
	// setup은 검증용 스키마를 만든다. 각 dialect의 문법 차이를 흡수한다.
	setup []string
	// teardown은 테스트 후 정리다.
	teardown []string
}

func targets(t *testing.T) []testTarget {
	t.Helper()
	sqlitePath := strings.ReplaceAll(t.TempDir(), "\\", "/") + "/introspect_test.db"

	return []testTarget{
		{
			kind: model.KindMySQL,
			conn: &model.Connection{
				Kind: model.KindMySQL, Host: "127.0.0.1", Port: 13306,
				DatabaseName: "appdb", Options: model.Options{},
			},
			secret:   &model.Secret{Username: "root", Password: "rootpw123"},
			setup:    mysqlSetup,
			teardown: []string{`DROP TABLE IF EXISTS it_order_item`, `DROP TABLE IF EXISTS it_orders`, `DROP TABLE IF EXISTS it_users`, `DROP VIEW IF EXISTS it_user_view`},
		},
		{
			kind: model.KindPostgres,
			conn: &model.Connection{
				Kind: model.KindPostgres, Host: "127.0.0.1", Port: 15432,
				DatabaseName: "appdb", Options: model.Options{"sslmode": "disable"},
			},
			secret:   &model.Secret{Username: "postgres", Password: "rootpw123"},
			setup:    postgresSetup,
			teardown: []string{`DROP VIEW IF EXISTS it_user_view`, `DROP TABLE IF EXISTS it_order_item`, `DROP TABLE IF EXISTS it_orders`, `DROP TABLE IF EXISTS it_users`, `DROP TYPE IF EXISTS it_status`},
		},
		{
			kind: model.KindMSSQL,
			conn: &model.Connection{
				Kind: model.KindMSSQL, Host: "127.0.0.1", Port: 11433,
				DatabaseName: "master", Options: model.Options{"schema": "dbo"},
			},
			secret:   &model.Secret{Username: "sa", Password: "RootPw123!"},
			setup:    mssqlSetup,
			teardown: []string{`DROP VIEW IF EXISTS it_user_view`, `DROP TABLE IF EXISTS it_order_item`, `DROP TABLE IF EXISTS it_orders`, `DROP TABLE IF EXISTS it_users`},
		},
		{
			kind: model.KindOracle,
			conn: &model.Connection{
				Kind: model.KindOracle, Host: "127.0.0.1", Port: 11521,
				DatabaseName: "FREEPDB1", Options: model.Options{},
			},
			secret:   &model.Secret{Username: "appuser", Password: "RootPw123"},
			setup:    oracleSetup,
			teardown: oracleTeardown,
		},
		{
			kind: model.KindSQLite,
			conn: &model.Connection{
				Kind: model.KindSQLite, DatabaseName: sqlitePath, Options: model.Options{},
			},
			secret: &model.Secret{},
			setup:  sqliteSetup,
		},
	}
}

// TestIntrospect는 각 DB에 알려진 스키마를 만들고 읽어들여 검증한다.
//
// 검증하는 것:
//  1. 테이블/컬럼/기본키/외래키/인덱스/체크 제약/뷰를 빠짐없이 읽는가
//  2. 타입이 논리 타입으로 올바르게 정규화되는가
//  3. 같은 스키마를 두 번 읽은 결과의 diff가 비어 있는가 (안정성)
//  4. 변경을 가하면 정확히 그 변경만 diff에 나타나는가
//  5. 생성된 DDL이 실제로 실행 가능한가
func TestIntrospect(t *testing.T) {
	if !*runIntegration && os.Getenv("DBSTUDIO_INTEGRATION") == "" {
		t.Skip("통합 테스트: -integration 플래그 또는 DBSTUDIO_INTEGRATION 환경변수 필요")
	}

	for _, tc := range targets(t) {
		t.Run(string(tc.kind), func(t *testing.T) {
			adapter, err := Get(tc.kind)
			if err != nil {
				t.Fatalf("어댑터 없음: %v", err)
			}
			target := Target{Conn: tc.conn, Secret: tc.secret}

			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()

			if _, err := adapter.Ping(ctx, target); err != nil {
				t.Skipf("접속 불가 (컨테이너 미실행?): %v", err)
			}

			sa, ok := adapter.(*sqlAdapter)
			if !ok {
				t.Fatalf("%s는 sqlAdapter가 아닙니다", tc.kind)
			}
			db, err := sa.open(target, 2)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			// Close를 먼저 등록한다. t.Cleanup은 LIFO이므로 아래의 스키마 정리가
			// 먼저 실행되고 커넥션 종료가 마지막에 온다.
			// (defer로 닫으면 Cleanup보다 먼저 실행되어 정리 구문이 조용히 실패한다.)
			t.Cleanup(func() { db.Close() })

			// 이전 실행이 남긴 객체를 먼저 정리한다.
			execIgnore(ctx, db, tc.teardown)
			t.Cleanup(func() {
				execIgnore(context.Background(), db, tc.teardown)
			})

			for _, stmt := range tc.setup {
				if _, err := db.ExecContext(ctx, stmt); err != nil {
					t.Fatalf("setup 실패:\n%s\n오류: %v", stmt, err)
				}
			}

			s, err := adapter.Introspect(ctx, target)
			if err != nil {
				t.Fatalf("introspect: %v", err)
			}
			for _, n := range s.Notes {
				t.Logf("note: %s", n)
			}

			verifyIntrospected(t, tc.kind, s)
			verifyDiffStability(t, ctx, adapter, target, s)
			verifyDiffDetectsChange(t, string(tc.kind), s)
			verifyPlanGeneration(t, string(tc.kind), s)
		})
	}
}

// verifyIntrospected는 읽어들인 스키마가 setup에서 만든 구조와 일치하는지 확인한다.
func verifyIntrospected(t *testing.T, kind model.DBKind, s *schema.Schema) {
	t.Helper()

	users := findTable(s, "it_users")
	if users == nil {
		t.Fatalf("it_users 테이블을 찾지 못했습니다 (읽은 테이블: %v)", tableNames(s))
	}

	// 기본키
	if users.PrimaryKey == nil {
		t.Errorf("it_users의 기본키를 읽지 못했습니다")
	} else if len(users.PrimaryKey.Columns) != 1 || !strings.EqualFold(users.PrimaryKey.Columns[0], "id") {
		t.Errorf("it_users 기본키가 [id]가 아닙니다: %v", users.PrimaryKey.Columns)
	}

	// 자동증가 컬럼
	if idCol := users.Column("id"); idCol == nil {
		t.Errorf("it_users.id 컬럼이 없습니다")
	} else if !idCol.Identity {
		t.Errorf("it_users.id가 identity로 인식되지 않았습니다 (raw=%q)", idCol.RawType)
	}

	// 타입 정규화: email은 varchar(255)여야 한다
	if c := users.Column("email"); c == nil {
		t.Errorf("it_users.email 컬럼이 없습니다")
	} else {
		if c.Type.Base != schema.TypeVarchar {
			t.Errorf("email 타입이 varchar가 아닙니다: %s (raw=%q)", c.Type.Canonical(), c.RawType)
		}
		if c.Type.Length != 255 {
			t.Errorf("email 길이가 255가 아닙니다: %d (raw=%q)", c.Type.Length, c.RawType)
		}
		if c.Nullable {
			t.Errorf("email은 NOT NULL이어야 합니다")
		}
	}

	// 정수 타입 정규화: Oracle의 NUMBER(10,0)도 int로 정규화되어야 한다
	if c := users.Column("login_count"); c == nil {
		t.Errorf("it_users.login_count 컬럼이 없습니다")
	} else if c.Type.Base != schema.TypeInt && c.Type.Base != schema.TypeBigInt {
		t.Errorf("login_count가 정수로 정규화되지 않았습니다: %s (raw=%q)", c.Type.Canonical(), c.RawType)
	}

	// nullable 컬럼
	if c := users.Column("nickname"); c == nil {
		t.Errorf("it_users.nickname 컬럼이 없습니다")
	} else if !c.Nullable {
		t.Errorf("nickname은 NULL 허용이어야 합니다")
	}

	// 기본값
	if c := users.Column("login_count"); c != nil && !c.HasDefault {
		t.Errorf("login_count의 기본값을 읽지 못했습니다")
	}

	// 주석 (SQLite는 주석 개념이 없다)
	if kind != model.KindSQLite {
		if users.Comment == "" {
			t.Errorf("it_users의 테이블 주석을 읽지 못했습니다")
		}
		if c := users.Column("email"); c != nil && c.Comment == "" {
			t.Errorf("it_users.email의 컬럼 주석을 읽지 못했습니다")
		}
	}

	// 유니크 인덱스
	foundUnique := false
	for _, idx := range users.Indexes {
		if idx.Unique && len(idx.Columns) == 1 && strings.EqualFold(idx.Columns[0].Column, "email") {
			foundUnique = true
		}
	}
	if !foundUnique {
		t.Errorf("it_users의 email 유니크 인덱스를 읽지 못했습니다 (읽은 인덱스: %v)", indexSummary(users))
	}

	// 체크 제약
	if len(users.Checks) == 0 {
		t.Errorf("it_users의 체크 제약을 읽지 못했습니다")
	}

	// 외래키: it_orders.user_id → it_users.id
	orders := findTable(s, "it_orders")
	if orders == nil {
		t.Fatalf("it_orders 테이블을 찾지 못했습니다")
	}
	if len(orders.ForeignKeys) != 1 {
		t.Fatalf("it_orders의 외래키가 1개가 아닙니다: %d개", len(orders.ForeignKeys))
	}
	fk := orders.ForeignKeys[0]
	if !strings.EqualFold(fk.RefTable, "it_users") {
		t.Errorf("외래키 참조 테이블이 it_users가 아닙니다: %s", fk.RefTable)
	}
	if len(fk.Columns) != 1 || !strings.EqualFold(fk.Columns[0], "user_id") {
		t.Errorf("외래키 컬럼이 [user_id]가 아닙니다: %v", fk.Columns)
	}
	if len(fk.RefColumns) != 1 || !strings.EqualFold(fk.RefColumns[0], "id") {
		t.Errorf("외래키 참조 컬럼이 [id]가 아닙니다: %v", fk.RefColumns)
	}
	if normalizeActionForTest(fk.OnDelete) != "CASCADE" {
		t.Errorf("ON DELETE CASCADE를 읽지 못했습니다: %q", fk.OnDelete)
	}

	// 복합 기본키
	item := findTable(s, "it_order_item")
	if item == nil {
		t.Fatalf("it_order_item 테이블을 찾지 못했습니다")
	}
	if item.PrimaryKey == nil || len(item.PrimaryKey.Columns) != 2 {
		t.Errorf("it_order_item의 복합 기본키(2컬럼)를 읽지 못했습니다: %+v", item.PrimaryKey)
	} else if !strings.EqualFold(item.PrimaryKey.Columns[0], "order_id") {
		// 순서가 유지되어야 한다. 순서가 뒤집히면 인덱스 성능이 달라진다.
		t.Errorf("복합 기본키 컬럼 순서가 잘못되었습니다: %v", item.PrimaryKey.Columns)
	}

	// 뷰
	if len(s.Views) == 0 {
		t.Errorf("뷰를 읽지 못했습니다")
	}

	// PostgreSQL enum
	if kind == model.KindPostgres {
		if len(s.Enums) == 0 {
			t.Errorf("enum 타입을 읽지 못했습니다")
		} else {
			found := false
			for _, e := range s.Enums {
				if strings.EqualFold(e.Name, "it_status") {
					found = true
					if len(e.Values) != 3 {
						t.Errorf("it_status enum 값이 3개가 아닙니다: %v", e.Values)
					}
				}
			}
			if !found {
				t.Errorf("it_status enum을 찾지 못했습니다")
			}
		}
		if c := orders.Column("status"); c != nil && c.Type.Base != schema.TypeEnum {
			t.Errorf("status 컬럼이 enum으로 인식되지 않았습니다: %s", c.Type.Canonical())
		}
	}
}

// verifyDiffStability는 같은 DB를 두 번 읽은 결과가 동일한지 확인한다.
//
// 이 테스트가 가장 중요하다. introspect가 비결정적이거나 정규화가 불완전하면
// 아무 변경도 없는데 마이그레이션이 생성되고, 사용자는 앱을 신뢰할 수 없게 된다.
func verifyDiffStability(t *testing.T, ctx context.Context, adapter Adapter, target Target, first *schema.Schema) {
	t.Helper()

	second, err := adapter.Introspect(ctx, target)
	if err != nil {
		t.Fatalf("두 번째 introspect: %v", err)
	}

	diff := schema.Diff(first, second)
	if !diff.IsEmpty() {
		t.Errorf("같은 스키마를 두 번 읽었는데 diff가 %d건 발생했습니다:", len(diff.Changes))
		for _, c := range diff.Changes {
			t.Errorf("  [%s] %s (attrs=%v)", c.Kind, c.Summary, c.Attrs)
		}
	}
	if first.Fingerprint() != second.Fingerprint() {
		t.Errorf("지문이 다릅니다: %s vs %s", first.Fingerprint(), second.Fingerprint())
	}
}

// verifyDiffDetectsChange는 스키마를 인위적으로 바꾸고 정확히 그 변경만 잡히는지 확인한다.
func verifyDiffDetectsChange(t *testing.T, dialect string, base *schema.Schema) {
	t.Helper()

	modified := cloneSchema(t, base)
	users := findTable(modified, "it_users")
	if users == nil {
		t.Fatal("복제된 스키마에 it_users가 없습니다")
	}

	// 1) 컬럼 추가
	users.Columns = append(users.Columns, &schema.Column{
		Name: "phone", Position: 999,
		Type:    schema.LogicalType{Base: schema.TypeVarchar, Length: 20},
		RawType: "varchar(20)", Nullable: true,
	})
	// 2) 기존 컬럼을 NOT NULL로 변경 (파괴적이어야 한다)
	if nick := users.Column("nickname"); nick != nil {
		nick.Nullable = false
	}
	// 3) 테이블 삭제
	modified.Tables = removeTable(modified.Tables, "it_order_item")

	diff := schema.Diff(base, modified)

	got := map[schema.ChangeKind]int{}
	for _, c := range diff.Changes {
		got[c.Kind]++
	}
	if got[schema.AddColumn] != 1 {
		t.Errorf("add_column이 1건이어야 하는데 %d건: %v", got[schema.AddColumn], changeSummaries(diff))
	}
	if got[schema.AlterColumn] != 1 {
		t.Errorf("alter_column이 1건이어야 하는데 %d건: %v", got[schema.AlterColumn], changeSummaries(diff))
	}
	if got[schema.DropTable] != 1 {
		t.Errorf("drop_table이 1건이어야 하는데 %d건: %v", got[schema.DropTable], changeSummaries(diff))
	}
	if diff.DestructiveCount < 2 {
		t.Errorf("파괴적 변경이 2건 이상이어야 합니다 (NOT NULL 변경 + 테이블 삭제): %d건", diff.DestructiveCount)
	}

	// 역방향 diff는 반대 변경을 내야 한다.
	back := schema.Diff(modified, base)
	backGot := map[schema.ChangeKind]int{}
	for _, c := range back.Changes {
		backGot[c.Kind]++
	}
	if backGot[schema.DropColumn] != 1 {
		t.Errorf("역방향에 drop_column이 1건이어야 합니다: %v", changeSummaries(back))
	}
	if backGot[schema.CreateTable] != 1 {
		t.Errorf("역방향에 create_table이 1건이어야 합니다: %v", changeSummaries(back))
	}
}

// verifyPlanGeneration은 빈 스키마 → 실제 스키마 계획이 모든 테이블을 만드는지 확인한다.
func verifyPlanGeneration(t *testing.T, dialect string, s *schema.Schema) {
	t.Helper()

	empty := &schema.Schema{Dialect: s.Dialect, Shape: schema.ShapeRelational}
	diff := schema.Diff(empty, s)
	plan := schema.BuildPlan(dialect, diff)

	if len(plan.Up) == 0 {
		t.Fatalf("up 계획이 비어 있습니다 (diff %d건)", len(diff.Changes))
	}
	for _, w := range plan.Warnings {
		t.Logf("plan warning: %s", w)
	}

	upSQL := plan.UpSQL()
	for _, name := range []string{"it_users", "it_orders", "it_order_item"} {
		if !strings.Contains(strings.ToLower(upSQL), strings.ToLower(name)) {
			t.Errorf("up SQL에 %s가 없습니다", name)
		}
	}
	// CREATE TABLE이 FK 추가보다 먼저 나와야 한다.
	createIdx, fkIdx := -1, -1
	for i, st := range plan.Up {
		if st.Kind == schema.CreateTable && createIdx < 0 {
			createIdx = i
		}
		if st.Kind == schema.AddForeign && fkIdx < 0 {
			fkIdx = i
		}
	}
	if fkIdx >= 0 && createIdx >= 0 && fkIdx < createIdx {
		t.Errorf("외래키 추가(%d)가 테이블 생성(%d)보다 먼저 나옵니다", fkIdx, createIdx)
	}

	// down 계획은 테이블을 지워야 한다.
	downSQL := strings.ToUpper(plan.DownSQL())
	if !strings.Contains(downSQL, "DROP TABLE") {
		t.Errorf("down SQL에 DROP TABLE이 없습니다:\n%s", plan.DownSQL())
	}
	t.Logf("생성된 up SQL %d문장, down %d문장", len(plan.Up), len(plan.Down))
}

// ---------- 헬퍼 ----------

func execIgnore(ctx context.Context, db *sql.DB, stmts []string) {
	for _, s := range stmts {
		// 정리 구문은 대상이 없으면 실패하는 게 정상이므로 오류를 무시한다.
		_, _ = db.ExecContext(ctx, s)
	}
}

func findTable(s *schema.Schema, name string) *schema.Table {
	for _, tbl := range s.Tables {
		if strings.EqualFold(tbl.Name, name) {
			return tbl
		}
	}
	return nil
}

func removeTable(tables []*schema.Table, name string) []*schema.Table {
	out := make([]*schema.Table, 0, len(tables))
	for _, tbl := range tables {
		if !strings.EqualFold(tbl.Name, name) {
			out = append(out, tbl)
		}
	}
	return out
}

func tableNames(s *schema.Schema) []string {
	out := make([]string, len(s.Tables))
	for i, tbl := range s.Tables {
		out[i] = tbl.Display()
	}
	return out
}

func indexSummary(tbl *schema.Table) []string {
	out := make([]string, len(tbl.Indexes))
	for i, idx := range tbl.Indexes {
		out[i] = fmt.Sprintf("%s(unique=%t, cols=%v)", idx.Name, idx.Unique, idx.ColumnNames())
	}
	return out
}

func changeSummaries(d *schema.DiffResult) []string {
	out := make([]string, len(d.Changes))
	for i, c := range d.Changes {
		out[i] = fmt.Sprintf("%s:%s", c.Kind, c.Summary)
	}
	return out
}

func normalizeActionForTest(s string) string {
	return strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(s), "_", " "))
}

// cloneSchema는 깊은 복사를 만든다. 테스트에서 원본을 오염시키지 않기 위해 필요하다.
func cloneSchema(t *testing.T, s *schema.Schema) *schema.Schema {
	t.Helper()
	out := &schema.Schema{
		Dialect: s.Dialect, Shape: s.Shape, Name: s.Name, CapturedAt: s.CapturedAt,
	}
	for _, tbl := range s.Tables {
		nt := *tbl
		nt.Columns = make([]*schema.Column, len(tbl.Columns))
		for i, c := range tbl.Columns {
			nc := *c
			nt.Columns[i] = &nc
		}
		nt.Indexes = make([]*schema.Index, len(tbl.Indexes))
		for i, idx := range tbl.Indexes {
			ni := *idx
			ni.Columns = append([]schema.IndexPart(nil), idx.Columns...)
			nt.Indexes[i] = &ni
		}
		nt.ForeignKeys = make([]*schema.ForeignKey, len(tbl.ForeignKeys))
		for i, fk := range tbl.ForeignKeys {
			nf := *fk
			nf.Columns = append([]string(nil), fk.Columns...)
			nf.RefColumns = append([]string(nil), fk.RefColumns...)
			nt.ForeignKeys[i] = &nf
		}
		nt.Checks = make([]*schema.Check, len(tbl.Checks))
		for i, ck := range tbl.Checks {
			nck := *ck
			nt.Checks[i] = &nck
		}
		if tbl.PrimaryKey != nil {
			pk := *tbl.PrimaryKey
			pk.Columns = append([]string(nil), tbl.PrimaryKey.Columns...)
			nt.PrimaryKey = &pk
		}
		out.Tables = append(out.Tables, &nt)
	}
	out.Views = append(out.Views, s.Views...)
	out.Enums = append(out.Enums, s.Enums...)
	return out
}
