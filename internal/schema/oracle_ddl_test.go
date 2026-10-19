package schema

import (
	"strings"
	"testing"
)

// Oracle 은 DEFAULT 를 제약(NOT NULL·CHECK)보다 앞에만 받는다. 다른 DB 와 같은 순서로
// 내면 기본값이 있는 NOT NULL 컬럼마다 ORA-00907 로 멈춘다.
func TestOracleDefaultBeforeNotNull(t *testing.T) {
	col := &Column{
		Name: "active", Position: 1, Type: LogicalType{Base: TypeBool},
		HasDefault: true, Default: "1",
	}
	sc := &Schema{Dialect: "oracle", Shape: ShapeRelational, Tables: []*Table{{
		Name: "app", Columns: []*Column{col},
	}}}
	up := BuildPlan("oracle", Diff(&Schema{Dialect: "oracle", Shape: ShapeRelational}, sc)).UpSQL()
	if !strings.Contains(up, `"active" NUMBER(1) DEFAULT 1 NOT NULL`) {
		t.Errorf("DEFAULT 가 NOT NULL 앞에 있지 않습니다:\n%s", up)
	}

	// 다른 DB 의 출력은 그대로다.
	sc.Dialect = "postgres"
	up = BuildPlan("postgres", Diff(&Schema{Dialect: "postgres", Shape: ShapeRelational}, sc)).UpSQL()
	if !strings.Contains(up, `"active" boolean NOT NULL DEFAULT 1`) {
		t.Errorf("PostgreSQL 출력이 바뀌었습니다:\n%s", up)
	}
}

// ADD COLUMN 은 MS-SQL·Oracle 에 없는 문법이다. 기준을 고른 내보내기와 마이그레이션이
// 그 DB 에서 첫 컬럼 추가부터 거부된다.
func TestAddColumnKeywordPerDialect(t *testing.T) {
	base := func(d string, cols ...*Column) *Schema {
		return &Schema{Dialect: d, Shape: ShapeRelational, Tables: []*Table{{Name: "member", Columns: cols}}}
	}
	id := &Column{Name: "id", Position: 1, Type: LogicalType{Base: TypeBigInt}}
	nick := &Column{Name: "nick", Position: 2, Type: LogicalType{Base: TypeVarchar, Length: 30}, Nullable: true}

	want := map[string]string{
		"oracle":   `ALTER TABLE "member" ADD "nick" VARCHAR2(30)`,
		"mssql":    `ALTER TABLE [member] ADD [nick] nvarchar(30)`,
		"postgres": `ALTER TABLE "member" ADD COLUMN "nick" varchar(30)`,
		"mysql":    "ALTER TABLE `member` ADD COLUMN `nick` VARCHAR(30)",
	}
	for d, stmt := range want {
		up := BuildPlan(d, Diff(base(d, id), base(d, id, nick))).UpSQL()
		if !strings.Contains(up, stmt) {
			t.Errorf("%s: %q 가 없습니다:\n%s", d, stmt, up)
		}
		// 컬럼 삭제를 되돌릴 때도 같은 문법이다.
		down := BuildPlan(d, Diff(base(d, id, nick), base(d, id))).DownSQL()
		if !strings.Contains(down, stmt) {
			t.Errorf("%s 되돌리기: %q 가 없습니다:\n%s", d, stmt, down)
		}
	}
}
