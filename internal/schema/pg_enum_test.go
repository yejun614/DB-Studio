package schema

import (
	"strings"
	"testing"
)

func enumCol(name string, values ...string) *Column {
	raw := EnumTypeString(values)
	return &Column{Name: name, RawType: raw, Type: ParseType("mysql", raw)}
}

func pgEmpty() *Schema { return &Schema{Dialect: "postgres", Shape: ShapeRelational} }

func tableOf(name string, cols ...*Column) *Table {
	for i, c := range cols {
		c.Position = i + 1
	}
	return &Table{Name: name, Columns: cols}
}

// PostgreSQL 에서 ENUM 은 이름 붙은 타입이다. 값 목록만 적은 컬럼도 <표>_<컬럼>
// 이름으로 CREATE TYPE 이 나가고, 컬럼은 그 이름을 가리킨다.
func TestPostgresInlineEnumGetsType(t *testing.T) {
	sc := &Schema{Dialect: "mysql", Shape: ShapeRelational, Tables: []*Table{
		tableOf("app", enumCol("status", "DEVELOPING", "RUNNING")),
	}}
	up := BuildPlan("postgres", Diff(pgEmpty(), sc)).UpSQL()

	typ := strings.Index(up, `CREATE TYPE "app_status" AS ENUM ('DEVELOPING', 'RUNNING')`)
	table := strings.Index(up, `CREATE TABLE`)
	if typ < 0 || table < 0 || typ > table {
		t.Fatalf("타입이 표보다 먼저 만들어지지 않았습니다:\n%s", up)
	}
	if !strings.Contains(up, `"status" "app_status" NOT NULL`) {
		t.Errorf("컬럼이 타입을 가리키지 않습니다:\n%s", up)
	}
	if strings.Contains(up, "CHECK") {
		t.Errorf("타입이 있는데 CHECK 까지 붙었습니다:\n%s", up)
	}
	// 원본은 건드리지 않는다. 문서의 스키마가 PostgreSQL 모양으로 바뀌면 MySQL 로
	// 내보낼 때 값 목록을 잃는다.
	if sc.Tables[0].Columns[0].Type.EnumName != "" || len(sc.Enums) != 0 {
		t.Errorf("비교가 원본 스키마를 고쳤습니다: %+v / %+v", sc.Tables[0].Columns[0].Type, sc.Enums)
	}
}

// 도메인을 쓰면 도메인 이름이 타입 이름이고, 같은 도메인의 컬럼은 타입 하나를 같이
// 쓴다. 도메인 없이 같은 값 목록을 쓰는 컬럼은 컬럼마다 따로다.
func TestPostgresEnumNaming(t *testing.T) {
	a, b := enumCol("status", "on", "off"), enumCol("status", "on", "off")
	a.Domain, b.Domain = "switch", "switch"
	c := enumCol("mode", "on", "off")
	sc := &Schema{Dialect: "postgres", Shape: ShapeRelational, Tables: []*Table{
		tableOf("lamp", a, c), tableOf("fan", b),
	}}
	up := BuildPlan("postgres", Diff(pgEmpty(), sc)).UpSQL()

	if n := strings.Count(up, `CREATE TYPE "switch"`); n != 1 {
		t.Errorf("도메인 타입이 %d번 만들어졌습니다:\n%s", n, up)
	}
	if !strings.Contains(up, `CREATE TYPE "lamp_mode"`) {
		t.Errorf("도메인 없는 컬럼의 타입이 없습니다:\n%s", up)
	}
	if strings.Count(up, `"status" "switch"`) != 2 {
		t.Errorf("두 표가 같은 타입을 쓰지 않습니다:\n%s", up)
	}
}

// 지은 이름이 다른 값 목록으로 이미 쓰이고 있으면 뒤에 붙여 피한다. 한 타입을 두 값
// 목록이 나눠 쓸 수는 없다.
func TestPostgresEnumNameCollision(t *testing.T) {
	sc := &Schema{Dialect: "postgres", Shape: ShapeRelational,
		Enums:  []*Enum{{Name: "app_status", Values: []string{"x"}}},
		Tables: []*Table{tableOf("app", enumCol("status", "a", "b"))},
	}
	up := BuildPlan("postgres", Diff(pgEmpty(), sc)).UpSQL()
	if !strings.Contains(up, `CREATE TYPE "app_status_enum" AS ENUM ('a', 'b')`) ||
		!strings.Contains(up, `"status" "app_status_enum"`) {
		t.Errorf("이름 충돌을 피하지 않았습니다:\n%s", up)
	}
}

// 대문자·한글이 든 이름은 따옴표로 만들고 따옴표로 가리켜야 같은 타입이다.
func TestPostgresEnumNameIsQuoted(t *testing.T) {
	col := enumCol("status", "a")
	col.Domain = "앱상태"
	sc := &Schema{Dialect: "postgres", Shape: ShapeRelational, Tables: []*Table{tableOf("app", col)}}
	up := BuildPlan("postgres", Diff(pgEmpty(), sc)).UpSQL()
	if !strings.Contains(up, `CREATE TYPE "앱상태"`) || !strings.Contains(up, `"status" "앱상태"`) {
		t.Errorf("타입 이름이 같은 식으로 인용되지 않았습니다:\n%s", up)
	}
}

// 내보낸 SQL 로 만든 DB 를 다시 읽으면 초안과 같아야 한다. 같지 않으면 아무것도 고치지
// 않은 초안이 매번 변경으로 잡힌다.
func TestPostgresEnumDraftMatchesDatabase(t *testing.T) {
	draft := &Schema{Dialect: "postgres", Shape: ShapeRelational, Tables: []*Table{
		tableOf("app", enumCol("status", "a", "b")),
	}}
	// 실제 DB 는 이렇게 읽힌다(introspect_postgres): 컬럼은 이름만, 값은 타입 쪽에.
	db := &Schema{Dialect: "postgres", Shape: ShapeRelational,
		Enums: []*Enum{{Name: "app_status", Values: []string{"a", "b"}}},
		Tables: []*Table{tableOf("app", &Column{
			Name: "status", RawType: "app_status",
			Type: LogicalType{Base: TypeEnum, EnumName: "app_status"},
		})},
	}
	if d := Diff(db, draft); len(d.Changes) != 0 {
		t.Errorf("같은 구조를 다르다고 봤습니다: %+v", d.Changes)
	}

	// 값을 하나 더하면 타입만 고친다.
	draft.Tables[0].Columns[0] = enumCol("status", "a", "b", "c")
	draft.Tables[0].Columns[0].Position = 1
	up := BuildPlan("postgres", Diff(db, draft)).UpSQL()
	if !strings.Contains(up, `ALTER TYPE "app_status" ADD VALUE IF NOT EXISTS 'c'`) {
		t.Errorf("값 추가가 ALTER TYPE 이 아닙니다:\n%s", up)
	}
	if strings.Contains(up, "ALTER COLUMN") {
		t.Errorf("컬럼까지 고쳤습니다:\n%s", up)
	}
}

// 도메인 이름을 바꾸면 새 타입으로 옮겨 간다. ENUM 끼리는 바로 변환되지 않고, 기본값은
// 옛 타입의 값이라 따라오지 않는다 — text 를 거치고 기본값을 떼었다 붙인다.
func TestPostgresEnumRenameMovesColumns(t *testing.T) {
	mk := func(domain string) *Schema {
		col := enumCol("status", "a", "b")
		col.Domain = domain
		col.Type.EnumName = domain
		col.HasDefault, col.Default = true, "'a'"
		return &Schema{Dialect: "postgres", Shape: ShapeRelational,
			Enums:  []*Enum{{Name: domain, Values: []string{"a", "b"}}},
			Tables: []*Table{tableOf("app", col)},
		}
	}
	plan := BuildPlan("postgres", Diff(mk("state"), mk("app_state")))
	up := plan.UpSQL()

	order := []string{
		`CREATE TYPE "app_state"`,
		`ALTER TABLE "app" ALTER COLUMN "status" DROP DEFAULT`,
		`ALTER TABLE "app" ALTER COLUMN "status" TYPE "app_state" USING "status"::text::"app_state"`,
		`ALTER TABLE "app" ALTER COLUMN "status" SET DEFAULT 'a'`,
		`DROP TYPE IF EXISTS "state"`,
	}
	at := 0
	for _, want := range order {
		i := strings.Index(up[at:], want)
		if i < 0 {
			t.Fatalf("%q 가 순서대로 없습니다:\n%s", want, up)
		}
		at += i
	}

	down := plan.DownSQL()
	if !strings.Contains(down, `TYPE "state" USING "status"::text::"state"`) {
		t.Errorf("되돌리기가 옛 타입으로 옮기지 않습니다:\n%s", down)
	}
	if strings.Index(down, `DROP DEFAULT`) > strings.Index(down, `USING "status"::text::"state"`) {
		t.Errorf("되돌리기에서 기본값을 떼기 전에 타입을 바꿉니다:\n%s", down)
	}
}

// PostgreSQL 밖에서는 이름 붙은 타입이 없다. 이름만 든 컬럼(실제 PostgreSQL 에서 온
// 것)도 값 목록으로 풀어 MySQL 은 ENUM(...), 나머지는 CHECK 로 낸다.
func TestNamedEnumOnOtherDialects(t *testing.T) {
	mk := func() *Schema {
		return &Schema{Dialect: "postgres", Shape: ShapeRelational,
			Enums: []*Enum{{Name: "app_status", Values: []string{"a", "b"}}},
			Tables: []*Table{tableOf("app", &Column{
				Name: "status", RawType: "app_status", Nullable: true,
				Type: LogicalType{Base: TypeEnum, EnumName: "app_status"},
			})},
		}
	}
	want := map[string]string{
		"mysql": "`status` ENUM('a', 'b')",
		"mssql": "[status] nvarchar(255) CHECK ([status] IN ('a', 'b'))",
	}
	for d, def := range want {
		up := BuildPlan(d, Diff(&Schema{Dialect: d, Shape: ShapeRelational}, mk())).UpSQL()
		if !strings.Contains(up, def) {
			t.Errorf("%s: %q 가 없습니다:\n%s", d, def, up)
		}
		if strings.Contains(up, "CREATE TYPE") {
			t.Errorf("%s 에 CREATE TYPE 이 나갔습니다:\n%s", d, up)
		}
	}

	// MySQL 에서는 값으로 견준다. 이름으로 견주면 도메인의 값을 바꿔도 "그대로"가 된다.
	from := &Schema{Dialect: "mysql", Shape: ShapeRelational, Tables: []*Table{tableOf("app", enumCol("status", "a", "b"))}}
	to := mk()
	to.Dialect = "mysql"
	to.Enums[0].Values = []string{"a", "b", "c"}
	if up := BuildPlan("mysql", Diff(from, to)).UpSQL(); !strings.Contains(up, "MODIFY COLUMN `status` ENUM('a', 'b', 'c')") {
		t.Errorf("값을 바꿨는데 컬럼을 고치지 않습니다:\n%s", up)
	}
}

// 직접 입력한 ENUM 배열(my_enum[])은 겉은 배열로 알아보지만 원소를 모른다. 그 원소를
// 기본값 text 로 그리면 text[] 가 나가고, 값은 들어가므로 오류도 나지 않는다.
// 모르는 원소가 있으면 적은 그대로 내보내야 한다.
func TestPostgresArrayOfUnknownKeepsRaw(t *testing.T) {
	col := func(name, raw string) *Column {
		return &Column{Name: name, RawType: raw, Type: ParseType("postgres", raw)}
	}
	sc := &Schema{Dialect: "postgres", Shape: ShapeRelational,
		Enums: []*Enum{{Name: "data_source_permission", Values: []string{"read", "write"}}},
		Tables: []*Table{tableOf("data_source",
			col("permission", "data_source_permission[]"),
			col("tags", "varchar(20)[]"),
		)},
	}
	up := BuildPlan("postgres", Diff(pgEmpty(), sc)).UpSQL()
	if !strings.Contains(up, `"permission" data_source_permission[]`) {
		t.Errorf("직접 입력한 배열 타입이 바뀌었습니다:\n%s", up)
	}
	if !strings.Contains(up, `"tags" varchar(20)[]`) {
		t.Errorf("알아보는 배열 타입이 정규화되지 않았습니다:\n%s", up)
	}
}
