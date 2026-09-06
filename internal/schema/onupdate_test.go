package schema

import (
	"strings"
	"testing"
)

func timeCol(name, raw, onUpdate string) *Column {
	return &Column{
		Name: name, Position: 1, RawType: raw,
		Type: ParseType("mysql", raw), OnUpdate: onUpdate,
	}
}

// ON UPDATE 는 DEFAULT 뒤에 온다(MySQL 의 문법 순서다). 두 값은 서로 다른 순간을
// 말하므로 한 컬럼에 함께 있는 것이 흔하다.
func TestOnUpdateInCreateTable(t *testing.T) {
	col := timeCol("updated_at", "datetime(3)", "CURRENT_TIMESTAMP(3)")
	col.HasDefault, col.Default = true, "CURRENT_TIMESTAMP(3)"
	sc := &Schema{Dialect: "mysql", Shape: ShapeRelational, Tables: []*Table{{
		Name: "posts", Columns: []*Column{col},
	}}}
	sql := BuildPlan("mysql", Diff(&Schema{Dialect: "mysql", Shape: ShapeRelational}, sc)).UpSQL()

	want := "DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3)"
	if !strings.Contains(sql, want) {
		t.Errorf("%q 가 DDL 에 없습니다:\n%s", want, sql)
	}
}

// MySQL 말고는 이 문법이 없다. 조용히 빼면 "ERD 에는 있는데 DB 에는 없는" 컬럼이
// 되므로 무엇을 해야 하는지까지 경고에 적어야 한다.
func TestOnUpdateWarnsOnOtherDialects(t *testing.T) {
	sc := &Schema{Dialect: "postgres", Shape: ShapeRelational, Tables: []*Table{{
		Name: "posts",
		Columns: []*Column{{
			Name: "updated_at", Position: 1, RawType: "timestamp",
			Type: LogicalType{Base: TypeTimestamp}, OnUpdate: "CURRENT_TIMESTAMP",
		}},
	}}}
	plan := BuildPlan("postgres", Diff(&Schema{
		Dialect: "postgres", Shape: ShapeRelational}, sc))

	if strings.Contains(plan.UpSQL(), "ON UPDATE") {
		t.Errorf("PostgreSQL DDL 에 ON UPDATE 가 나갔습니다:\n%s", plan.UpSQL())
	}
	found := false
	for _, w := range plan.Warnings {
		if strings.Contains(w, "트리거") {
			found = true
		}
	}
	if !found {
		t.Errorf("무엇을 해야 하는지 적지 않았습니다: %v", plan.Warnings)
	}
}

// MySQL 의 MODIFY COLUMN 은 정의 전체를 다시 쓴다. 자동 갱신을 빼먹으면 다른 것을
// 고쳤을 뿐인데 자동 갱신이 **조용히 사라진다**.
func TestAlterColumnKeepsOnUpdate(t *testing.T) {
	base := func(comment string) *Schema {
		col := timeCol("updated_at", "datetime", "CURRENT_TIMESTAMP")
		col.Comment = comment
		return &Schema{Dialect: "mysql", Shape: ShapeRelational, Tables: []*Table{{
			Name: "posts", Columns: []*Column{col},
		}}}
	}
	plan := BuildPlan("mysql", Diff(base(""), base("수정 시각")))
	up := plan.UpSQL()
	if !strings.Contains(up, "MODIFY COLUMN") {
		t.Fatalf("MODIFY COLUMN 이 아닙니다:\n%s", up)
	}
	if !strings.Contains(up, "ON UPDATE CURRENT_TIMESTAMP") {
		t.Errorf("주석만 고쳤는데 자동 갱신이 사라졌습니다:\n%s", up)
	}
}

// 표기가 여럿이다. 그대로 견주면 아무것도 고치지 않은 표가 매번 변경으로 잡힌다.
func TestOnUpdateComparisonIgnoresSpelling(t *testing.T) {
	same := [][2]string{
		{"CURRENT_TIMESTAMP", "current_timestamp()"},
		{"CURRENT_TIMESTAMP", "now()"},
		{"CURRENT_TIMESTAMP(0)", "CURRENT_TIMESTAMP"},
		{"current_timestamp(3)", "CURRENT_TIMESTAMP(3)"},
	}
	for _, pair := range same {
		from := &Schema{Dialect: "mysql", Shape: ShapeRelational, Tables: []*Table{{
			Name: "posts", Columns: []*Column{timeCol("updated_at", "datetime", pair[0])},
		}}}
		to := &Schema{Dialect: "mysql", Shape: ShapeRelational, Tables: []*Table{{
			Name: "posts", Columns: []*Column{timeCol("updated_at", "datetime", pair[1])},
		}}}
		if d := Diff(from, to); len(d.Changes) != 0 {
			t.Errorf("%q 와 %q 를 다르다고 봤습니다: %+v", pair[0], pair[1], d.Changes)
		}
	}

	// 정밀도가 다르면 다른 것이다. 밀리초가 늘 0이 되는 종류의 어긋남이라
	// 뭉뚱그리면 안 된다.
	from := &Schema{Dialect: "mysql", Shape: ShapeRelational, Tables: []*Table{{
		Name: "posts", Columns: []*Column{timeCol("updated_at", "datetime(3)", "CURRENT_TIMESTAMP")},
	}}}
	to := &Schema{Dialect: "mysql", Shape: ShapeRelational, Tables: []*Table{{
		Name: "posts", Columns: []*Column{timeCol("updated_at", "datetime(3)", "CURRENT_TIMESTAMP(3)")},
	}}}
	if d := Diff(from, to); len(d.Changes) != 1 {
		t.Errorf("정밀도 차이를 못 잡았습니다: %+v", d.Changes)
	}
}

// 자동 갱신을 붙일 수 있는 타입은 카탈로그가 말한다. 화면이 이름을 보고 짐작하면
// 그 규칙이 화면에 한 벌 더 생긴다.
func TestCatalogMarksOnUpdateTypes(t *testing.T) {
	cat := TypeCatalog("mysql")
	if cat.OnUpdateLabel == "" || len(cat.OnUpdateExprs) == 0 {
		t.Fatal("MySQL 카탈로그에 자동 갱신 정보가 없습니다")
	}
	allowed := map[string]bool{}
	for _, tp := range cat.Types {
		if tp.OnUpdate {
			allowed[tp.Name] = true
		}
	}
	for _, name := range []string{"DATETIME", "TIMESTAMP"} {
		if !allowed[name] {
			t.Errorf("%s 에 자동 갱신을 붙일 수 없다고 표시돼 있습니다", name)
		}
	}
	for _, name := range []string{"INT", "VARCHAR", "DATE"} {
		if allowed[name] {
			t.Errorf("%s 에 자동 갱신을 붙일 수 있다고 표시돼 있습니다", name)
		}
	}
	// 이 기능이 없는 DB 는 라벨이 비어 있어야 한다. 화면은 그것으로 칸을 감춘다.
	for _, d := range []string{"postgres", "mssql", "oracle", "sqlite"} {
		if TypeCatalog(d).OnUpdateLabel != "" {
			t.Errorf("%s 에 자동 갱신 칸이 생겼습니다", d)
		}
	}
}

// 시각 타입의 소수 자릿수를 잃으면 안 된다.
//
// DATETIME(3) 과 DATETIME 은 다른 컬럼이다. 값은 들어가지만 밀리초가 늘 0 이 되어
// 오류가 나지 않는다 — "시계가 이상하다"로만 보이는 종류다. 그리고 MySQL 의
// ON UPDATE CURRENT_TIMESTAMP(3) 은 컬럼의 자릿수가 같아야 해서, 여기서 잃으면
// 그 문장이 서버에 거절된다.
func TestTimePrecisionSurvivesRoundTrip(t *testing.T) {
	cases := []struct{ dialect, raw, want string }{
		{"mysql", "datetime(3)", "DATETIME(3)"},
		// MySQL 의 TIMESTAMP 는 논리 타입에서 DATETIME 과 같은 것으로 정규화된다
		// (예전부터 그랬다). 여기서 보려는 것은 자릿수가 살아남는가다.
		{"mysql", "timestamp(6)", "DATETIME(6)"},
		{"mysql", "time(3)", "TIME(3)"},
		{"mysql", "datetime", "DATETIME"},
		{"postgres", "timestamp(3)", "timestamp(3)"},
		{"mssql", "datetime2(3)", "datetime2(3)"},
		{"oracle", "TIMESTAMP(6)", "TIMESTAMP(6)"},
	}
	for _, tc := range cases {
		lt := ParseType(tc.dialect, tc.raw)
		if got := RenderType(tc.dialect, lt, ""); got != tc.want {
			t.Errorf("%s %q → %q, 기댓값 %q", tc.dialect, tc.raw, got, tc.want)
		}
	}
}

// 자릿수가 다르면 다른 컬럼이다. 견주는 쪽도 그것을 알아야 한다.
func TestTimePrecisionIsCompared(t *testing.T) {
	mk := func(raw string) *Schema {
		return &Schema{Dialect: "mysql", Shape: ShapeRelational, Tables: []*Table{{
			Name: "posts", Columns: []*Column{timeCol("at", raw, "")},
		}}}
	}
	if d := Diff(mk("datetime"), mk("datetime(3)")); len(d.Changes) != 1 {
		t.Errorf("자릿수 차이를 못 잡았습니다: %+v", d.Changes)
	}
	if d := Diff(mk("datetime(3)"), mk("datetime(3)")); len(d.Changes) != 0 {
		t.Errorf("같은 것을 다르다고 봤습니다: %+v", d.Changes)
	}
}
