package dbx

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

	"dbstudio/internal/model"
	"dbstudio/internal/schema"
)

// TestDDLRoundTrip은 DDL 생성기의 정확성을 실제 실행으로 검증한다.
//
// 절차: 원본 스키마 introspect → 빈 스키마와 diff → up SQL 생성 →
//
//	별도의 빈 데이터베이스/스키마에 실행 → 다시 introspect → 원본과 diff.
//
// 마지막 diff가 비어 있어야 한다. 비어 있지 않다면 생성된 DDL이 원본 구조를
// 정확히 재현하지 못한 것이며, 그대로 두면 P7 마이그레이션에서 스키마가 틀어진다.
func TestDDLRoundTrip(t *testing.T) {
	if !*runIntegration && os.Getenv("DBSTUDIO_INTEGRATION") == "" {
		t.Skip("통합 테스트: -integration 플래그 또는 DBSTUDIO_INTEGRATION 환경변수 필요")
	}

	cases := []struct {
		name string
		kind model.DBKind
		// source는 원본 스키마를 만들 대상이다.
		source *model.Connection
		secret *model.Secret
		setup  []string
		clean  []string
		// prepare는 왕복 대상(빈 네임스페이스)을 만들고, 그곳을 가리키는 커넥션을 돌려준다.
		prepare func(t *testing.T, ctx context.Context, db *sql.DB, base *model.Connection) (*model.Connection, func())
	}{
		{
			name: "mysql",
			kind: model.KindMySQL,
			source: &model.Connection{
				Kind: model.KindMySQL, Host: "127.0.0.1", Port: 13306,
				DatabaseName: "appdb", Options: model.Options{},
			},
			secret: &model.Secret{Username: "root", Password: "rootpw123"},
			setup:  mysqlSetup,
			clean:  []string{`DROP VIEW IF EXISTS it_user_view`, `DROP TABLE IF EXISTS it_order_item`, `DROP TABLE IF EXISTS it_orders`, `DROP TABLE IF EXISTS it_users`},
			prepare: func(t *testing.T, ctx context.Context, db *sql.DB, base *model.Connection) (*model.Connection, func()) {
				if _, err := db.ExecContext(ctx, `DROP DATABASE IF EXISTS it_roundtrip`); err != nil {
					t.Fatalf("대상 DB 정리 실패: %v", err)
				}
				if _, err := db.ExecContext(ctx, `CREATE DATABASE it_roundtrip DEFAULT CHARSET=utf8mb4`); err != nil {
					t.Fatalf("대상 DB 생성 실패: %v", err)
				}
				dst := *base
				dst.DatabaseName = "it_roundtrip"
				return &dst, func() {
					_, _ = db.ExecContext(context.Background(), `DROP DATABASE IF EXISTS it_roundtrip`)
				}
			},
		},
		{
			name: "postgres",
			kind: model.KindPostgres,
			source: &model.Connection{
				Kind: model.KindPostgres, Host: "127.0.0.1", Port: 15432,
				DatabaseName: "appdb", Options: model.Options{"sslmode": "disable"},
			},
			secret: &model.Secret{Username: "postgres", Password: "rootpw123"},
			setup:  postgresSetup,
			clean:  []string{`DROP VIEW IF EXISTS it_user_view`, `DROP TABLE IF EXISTS it_order_item`, `DROP TABLE IF EXISTS it_orders`, `DROP TABLE IF EXISTS it_users`, `DROP TYPE IF EXISTS it_status`},
			prepare: func(t *testing.T, ctx context.Context, db *sql.DB, base *model.Connection) (*model.Connection, func()) {
				if _, err := db.ExecContext(ctx, `DROP SCHEMA IF EXISTS it_roundtrip CASCADE`); err != nil {
					t.Fatalf("대상 스키마 정리 실패: %v", err)
				}
				if _, err := db.ExecContext(ctx, `CREATE SCHEMA it_roundtrip`); err != nil {
					t.Fatalf("대상 스키마 생성 실패: %v", err)
				}
				dst := *base
				dst.Options = model.Options{"sslmode": "disable", "search_path": "it_roundtrip"}
				return &dst, func() {
					_, _ = db.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS it_roundtrip CASCADE`)
				}
			},
		},
		{
			name: "mssql",
			kind: model.KindMSSQL,
			source: &model.Connection{
				Kind: model.KindMSSQL, Host: "127.0.0.1", Port: 11433,
				DatabaseName: "master", Options: model.Options{"schema": "dbo"},
			},
			secret: &model.Secret{Username: "sa", Password: "RootPw123!"},
			setup:  mssqlSetup,
			clean:  []string{`DROP VIEW IF EXISTS it_user_view`, `DROP TABLE IF EXISTS it_order_item`, `DROP TABLE IF EXISTS it_orders`, `DROP TABLE IF EXISTS it_users`},
			prepare: func(t *testing.T, ctx context.Context, db *sql.DB, base *model.Connection) (*model.Connection, func()) {
				// MS-SQL은 CREATE SCHEMA를 배치의 첫 문장으로 요구하므로 별도로 실행한다.
				execIgnore(ctx, db, []string{
					`DROP VIEW IF EXISTS it_rt.it_user_view`,
					`DROP TABLE IF EXISTS it_rt.it_order_item`,
					`DROP TABLE IF EXISTS it_rt.it_orders`,
					`DROP TABLE IF EXISTS it_rt.it_users`,
					`DROP SCHEMA IF EXISTS it_rt`,
				})
				if _, err := db.ExecContext(ctx, `CREATE SCHEMA it_rt`); err != nil {
					t.Fatalf("대상 스키마 생성 실패: %v", err)
				}
				dst := *base
				dst.Options = model.Options{"schema": "it_rt"}
				return &dst, func() {
					execIgnore(context.Background(), db, []string{
						`DROP VIEW IF EXISTS it_rt.it_user_view`,
						`DROP TABLE IF EXISTS it_rt.it_order_item`,
						`DROP TABLE IF EXISTS it_rt.it_orders`,
						`DROP TABLE IF EXISTS it_rt.it_users`,
						`DROP SCHEMA IF EXISTS it_rt`,
					})
				}
			},
		},
		{
			name: "sqlite",
			kind: model.KindSQLite,
			source: &model.Connection{
				Kind: model.KindSQLite, Options: model.Options{},
			},
			secret: &model.Secret{},
			setup:  sqliteSetup,
			prepare: func(t *testing.T, ctx context.Context, db *sql.DB, base *model.Connection) (*model.Connection, func()) {
				dst := *base
				dst.DatabaseName = strings.ReplaceAll(t.TempDir(), "\\", "/") + "/roundtrip.db"
				return &dst, func() {}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.kind == model.KindSQLite {
				tc.source.DatabaseName = strings.ReplaceAll(t.TempDir(), "\\", "/") + "/source.db"
			}
			adapter, err := Get(tc.kind)
			if err != nil {
				t.Fatalf("어댑터 없음: %v", err)
			}
			srcTarget := Target{Conn: tc.source, Secret: tc.secret}

			ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
			defer cancel()

			if _, err := adapter.Ping(ctx, srcTarget); err != nil {
				t.Skipf("접속 불가 (컨테이너 미실행?): %v", err)
			}

			sa := adapter.(*sqlAdapter)
			srcDB, err := sa.open(srcTarget, 2)
			if err != nil {
				t.Fatalf("원본 open: %v", err)
			}
			// Close는 가장 먼저 등록해 가장 마지막에 실행되게 한다 (t.Cleanup은 LIFO).
			// defer로 닫으면 아래 정리 구문이 닫힌 커넥션으로 실행되어 무음 실패한다.
			t.Cleanup(func() { srcDB.Close() })

			// 원본 스키마 준비
			execIgnore(ctx, srcDB, tc.clean)
			t.Cleanup(func() { execIgnore(context.Background(), srcDB, tc.clean) })
			for _, stmt := range tc.setup {
				if _, err := srcDB.ExecContext(ctx, stmt); err != nil {
					t.Fatalf("원본 setup 실패:\n%s\n오류: %v", stmt, err)
				}
			}

			original, err := adapter.Introspect(ctx, srcTarget)
			if err != nil {
				t.Fatalf("원본 introspect: %v", err)
			}

			// 왕복 대상 준비
			dstConn, cleanup := tc.prepare(t, ctx, srcDB, tc.source)
			t.Cleanup(cleanup)
			dstTarget := Target{Conn: dstConn, Secret: tc.secret}

			// 빈 스키마 → 원본 구조 계획 생성
			empty := &schema.Schema{Dialect: original.Dialect, Shape: schema.ShapeRelational}
			plan := schema.BuildPlan(string(tc.kind), schema.Diff(empty, original))
			for _, w := range plan.Warnings {
				t.Logf("plan warning: %s", w)
			}

			dstDB, err := sa.open(dstTarget, 2)
			if err != nil {
				t.Fatalf("대상 open: %v", err)
			}
			defer dstDB.Close()

			// PostgreSQL은 생성된 DDL이 네임스페이스를 명시하지 않으므로 search_path를 맞춘다.
			if tc.kind == model.KindPostgres {
				if _, err := dstDB.ExecContext(ctx, `SET search_path TO it_roundtrip`); err != nil {
					t.Fatalf("search_path 설정 실패: %v", err)
				}
			}

			applied := 0
			for i, stmt := range plan.Up {
				sql := rewriteForNamespace(tc.kind, stmt.SQL, original, dstConn)
				if _, err := dstDB.ExecContext(ctx, sql); err != nil {
					t.Fatalf("생성된 DDL %d/%d 실행 실패:\n%s\n오류: %v",
						i+1, len(plan.Up), sql, err)
				}
				applied++
			}
			t.Logf("%d개 DDL 문장 실행 성공", applied)

			// 다시 읽어서 원본과 비교
			restored, err := adapter.Introspect(ctx, dstTarget)
			if err != nil {
				t.Fatalf("복원본 introspect: %v", err)
			}

			// 네임스페이스가 다르므로 비교 전에 통일한다.
			stripNamespaces(original)
			stripNamespaces(restored)

			diff := schema.Diff(original, restored)
			if !diff.IsEmpty() {
				t.Errorf("왕복 후 diff가 %d건 발생했습니다 (생성된 DDL이 원본을 재현하지 못함):", len(diff.Changes))
				for _, c := range diff.Changes {
					t.Errorf("  [%s] %s", c.Kind, c.Summary)
					for k, v := range c.Attrs {
						t.Errorf("      %s = %q", k, v)
					}
				}
				t.Logf("실행된 up SQL:\n%s", plan.UpSQL())
			}
		})
	}
}

// rewriteForNamespace는 원본 네임스페이스를 대상 네임스페이스로 바꾼다.
// PostgreSQL/MS-SQL은 생성된 DDL에 원본 스키마 이름이 박혀 있어 그대로 실행하면
// 원본을 덮어쓴다.
func rewriteForNamespace(kind model.DBKind, sql string, original *schema.Schema, dst *model.Connection) string {
	var target, open, close string
	switch kind {
	case model.KindPostgres:
		target = dst.Options.GetOr("search_path", "public")
		open, close = `"`, `"`
	case model.KindMSSQL:
		target = dst.Options.GetOr("schema", "dbo")
		open, close = "[", "]"
	default:
		return sql
	}

	namespaces := map[string]bool{}
	for _, tbl := range original.Tables {
		if tbl.Namespace != "" && !strings.EqualFold(tbl.Namespace, target) {
			namespaces[tbl.Namespace] = true
		}
	}
	for _, e := range original.Enums {
		if e.Namespace != "" && !strings.EqualFold(e.Namespace, target) {
			namespaces[e.Namespace] = true
		}
	}
	for _, v := range original.Views {
		if v.Namespace != "" && !strings.EqualFold(v.Namespace, target) {
			namespaces[v.Namespace] = true
		}
	}

	out := sql
	for ns := range namespaces {
		// 인용된 형태: "public". → "it_rt".
		out = strings.ReplaceAll(out, open+ns+close+".", open+target+close+".")
		// MS-SQL의 확장 속성 문장은 스키마 이름을 문자열 리터럴로 넘긴다.
		if kind == model.KindMSSQL {
			out = strings.ReplaceAll(out, "N'"+ns+"'", "N'"+target+"'")
			out = strings.ReplaceAll(out, "["+ns+"].", "["+target+"].")
		}
	}
	return out
}

// stripNamespaces는 비교를 위해 네임스페이스를 제거한다.
// 왕복 테스트는 "구조가 같은가"를 보는 것이고 네임스페이스 이름은 의도적으로 다르다.
func stripNamespaces(s *schema.Schema) {
	for _, tbl := range s.Tables {
		tbl.Namespace = ""
		for _, fk := range tbl.ForeignKeys {
			fk.RefNamespace = ""
		}
	}
	for _, v := range s.Views {
		v.Namespace = ""
	}
	for _, e := range s.Enums {
		e.Namespace = ""
	}
}
