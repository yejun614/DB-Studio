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
	for _, dialect := range []string{"mssql", "oracle", "sqlite"} {
		sc := &Schema{Dialect: dialect, Shape: ShapeRelational, Tables: []*Table{{
			Name: "posts",
			Columns: []*Column{{
				Name: "updated_at", Position: 1, RawType: "timestamp",
				Type: LogicalType{Base: TypeTimestamp}, OnUpdate: "CURRENT_TIMESTAMP",
			}},
		}}}
		plan := BuildPlan(dialect, Diff(&Schema{
			Dialect: dialect, Shape: ShapeRelational}, sc))

		if strings.Contains(plan.UpSQL(), "ON UPDATE") {
			t.Errorf("%s DDL 에 ON UPDATE 가 나갔습니다:\n%s", dialect, plan.UpSQL())
		}
		found := false
		for _, w := range plan.Warnings {
			if strings.Contains(w, "트리거") {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: 무엇을 해야 하는지 적지 않았습니다: %v", dialect, plan.Warnings)
		}
	}
}

// PostgreSQL 은 표를 만들 때 트리거로 대신 만들어 준다. MySQL 에서 옮겨 온
// updated_at 이 "고쳐도 그대로인" 컬럼이 되는 것을 경고만으로는 막지 못했다.
func TestOnUpdateBecomesTriggerOnPostgres(t *testing.T) {
	sc := &Schema{Dialect: "postgres", Shape: ShapeRelational, Tables: []*Table{{
		Name: "posts",
		Columns: []*Column{
			{Name: "id", Position: 1, Type: LogicalType{Base: TypeBigInt}},
			{
				Name: "updated_at", Position: 2, RawType: "datetime(3)",
				Type: LogicalType{Base: TypeTimestamp, Precision: 3}, OnUpdate: "current_timestamp(3)",
			},
		},
	}}}
	plan := BuildPlan("postgres", Diff(&Schema{Dialect: "postgres", Shape: ShapeRelational}, sc))
	up := plan.UpSQL()

	for _, want := range []string{
		`CREATE OR REPLACE FUNCTION "posts_on_update"() RETURNS trigger`,
		`IF NEW."updated_at" IS NOT DISTINCT FROM OLD."updated_at" THEN NEW."updated_at" := CURRENT_TIMESTAMP(3);`,
		`CREATE TRIGGER "posts_on_update" BEFORE UPDATE ON "posts" FOR EACH ROW EXECUTE FUNCTION "posts_on_update"()`,
	} {
		if !strings.Contains(up, want) {
			t.Errorf("%q 가 없습니다:\n%s", want, up)
		}
	}
	if strings.Contains(up, "ON UPDATE CURRENT") {
		t.Errorf("컬럼 정의에 ON UPDATE 가 나갔습니다:\n%s", up)
	}
	for _, w := range plan.Warnings {
		if strings.Contains(w, "ON UPDATE") {
			t.Errorf("트리거로 대신했는데 생략했다고 경고합니다: %s", w)
		}
	}
	// 함수는 표를 지운 뒤에 지워야 한다. 먼저 지우면 트리거가 물고 있어 실패한다.
	down := plan.DownSQL()
	table, fn := strings.Index(down, `DROP TABLE`), strings.Index(down, `DROP FUNCTION IF EXISTS "posts_on_update"()`)
	if table < 0 || fn < 0 || fn < table {
		t.Errorf("되돌리기 순서가 틀렸습니다:\n%s", down)
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

// 표를 지운 변경을 되돌리면 표 → 인덱스 → 트리거 순서로 다시 만들어야 하고, 만든
// 표를 도로 지우면 안 된다. 순서가 뒤집히면 없는 표에 인덱스를 걸다 실패한다.
func TestDropTableRollbackOrder(t *testing.T) {
	sc := &Schema{Dialect: "postgres", Shape: ShapeRelational, Tables: []*Table{{
		Name: "posts",
		Columns: []*Column{
			{Name: "title", Position: 1, Type: LogicalType{Base: TypeText}},
			{
				Name: "updated_at", Position: 2,
				Type: LogicalType{Base: TypeTimestamp}, OnUpdate: "CURRENT_TIMESTAMP",
			},
		},
		Indexes: []*Index{{Name: "ix_title", Columns: []IndexPart{{Column: "title"}}}},
	}}}
	down := BuildPlan("postgres", Diff(sc, &Schema{Dialect: "postgres", Shape: ShapeRelational})).DownSQL()

	at := func(s string) int { return strings.Index(down, s) }
	table, index, fn, trigger := at("CREATE TABLE"), at("CREATE INDEX"), at("CREATE OR REPLACE FUNCTION"), at("CREATE TRIGGER")
	if table < 0 || !(table < index && index < fn && fn < trigger) {
		t.Errorf("되살리는 순서가 틀렸습니다:\n%s", down)
	}
	if strings.Contains(down, "DROP TABLE") {
		t.Errorf("되살린 표를 다시 지웁니다:\n%s", down)
	}
}

// pgPosts는 posts 표다. onUpdate 에 적은 컬럼은 자동 갱신 컬럼이 된다.
func pgPosts(cols ...*Column) *Schema {
	all := append([]*Column{{Name: "id", Position: 1, Type: LogicalType{Base: TypeBigInt}}}, cols...)
	for i, c := range all {
		c.Position = i + 1
	}
	return &Schema{Dialect: "postgres", Shape: ShapeRelational, Tables: []*Table{{Name: "posts", Columns: all}}}
}

func pgTimeCol(name, onUpdate string) *Column {
	return &Column{Name: name, Type: LogicalType{Base: TypeTimestamp}, Nullable: true, OnUpdate: onUpdate}
}

func noOnUpdateWarning(t *testing.T, plan *Plan) {
	t.Helper()
	for _, w := range plan.Warnings {
		if strings.Contains(w, "ON UPDATE") {
			t.Errorf("트리거로 대신했는데 생략했다고 경고합니다: %s", w)
		}
	}
}

// 이미 있는 표에 자동 갱신 컬럼을 더해도 트리거가 생겨야 한다. 표를 처음 만들 때만
// 만들면, 초안에 updated_at 을 나중에 더한 사람은 "고쳐도 그대로인" 컬럼을 얻는다.
func TestOnUpdateTriggerOnAddColumn(t *testing.T) {
	plan := BuildPlan("postgres", Diff(pgPosts(), pgPosts(pgTimeCol("updated_at", "CURRENT_TIMESTAMP"))))
	up, down := plan.UpSQL(), plan.DownSQL()

	at := func(sql, s string) int { return strings.Index(sql, s) }
	add, fn, trig := at(up, "ADD COLUMN"), at(up, `CREATE OR REPLACE FUNCTION "posts_on_update"()`),
		at(up, `CREATE TRIGGER "posts_on_update" BEFORE UPDATE ON "posts"`)
	if add < 0 || !(add < fn && fn < trig) {
		t.Errorf("컬럼 → 함수 → 트리거 순서가 아닙니다:\n%s", up)
	}
	if !strings.Contains(up, `NEW."updated_at" := CURRENT_TIMESTAMP;`) {
		t.Errorf("함수 본문에 컬럼이 없습니다:\n%s", up)
	}
	noOnUpdateWarning(t, plan)

	// 되돌리면 트리거 → 함수 → 컬럼 순서로 지운다. 트리거가 물고 있는 함수는 지울 수 없다.
	dTrig, dFn, dCol := at(down, `DROP TRIGGER IF EXISTS "posts_on_update" ON "posts"`),
		at(down, `DROP FUNCTION IF EXISTS "posts_on_update"()`), at(down, "DROP COLUMN")
	if dTrig < 0 || !(dTrig < dFn && dFn < dCol) {
		t.Errorf("되돌리기 순서가 틀렸습니다:\n%s", down)
	}

	// 문장 번호가 컬럼 추가의 것이어야 그 변경만 되돌릴 때 트리거도 함께 돌아간다.
	for _, st := range append(plan.Up, plan.Down...) {
		if st.Seq != 1 {
			t.Errorf("변경 번호가 %d 입니다: %s", st.Seq, st.SQL)
		}
	}
}

// 있던 컬럼에 자동 갱신을 켜고 끄는 것도 같다.
func TestOnUpdateTriggerOnAlterColumn(t *testing.T) {
	off := pgPosts(pgTimeCol("updated_at", ""))
	on := pgPosts(pgTimeCol("updated_at", "CURRENT_TIMESTAMP(3)"))

	plan := BuildPlan("postgres", Diff(off, on))
	if up := plan.UpSQL(); !strings.Contains(up, `NEW."updated_at" := CURRENT_TIMESTAMP(3);`) ||
		!strings.Contains(up, "CREATE TRIGGER") {
		t.Errorf("켰는데 트리거가 없습니다:\n%s", up)
	}
	if down := plan.DownSQL(); !strings.Contains(down, `DROP FUNCTION IF EXISTS "posts_on_update"()`) {
		t.Errorf("되돌리기가 함수를 지우지 않습니다:\n%s", down)
	}
	noOnUpdateWarning(t, plan)

	// 마지막 하나를 끄면 트리거와 함수를 지운다. 빈 함수를 남기면 다시 읽을 때 설명할 수 없다.
	plan = BuildPlan("postgres", Diff(on, off))
	up := plan.UpSQL()
	if strings.Contains(up, "CREATE OR REPLACE FUNCTION") {
		t.Errorf("껐는데 함수를 다시 만듭니다:\n%s", up)
	}
	trig, fn := strings.Index(up, `DROP TRIGGER IF EXISTS "posts_on_update"`), strings.Index(up, `DROP FUNCTION IF EXISTS "posts_on_update"()`)
	if trig < 0 || fn < trig {
		t.Errorf("트리거 → 함수 순서로 지우지 않습니다:\n%s", up)
	}
	if down := plan.DownSQL(); !strings.Contains(down, `NEW."updated_at" := CURRENT_TIMESTAMP(3);`) {
		t.Errorf("되돌리기가 옛 트리거를 되살리지 않습니다:\n%s", down)
	}
}

// 자동 갱신 컬럼이 둘인 표에서 하나를 빼면 함수는 남은 하나로 다시 쓴다.
func TestOnUpdateTriggerKeepsOtherColumns(t *testing.T) {
	both := pgPosts(pgTimeCol("updated_at", "CURRENT_TIMESTAMP"), pgTimeCol("touched_at", "CURRENT_TIMESTAMP"))
	one := pgPosts(pgTimeCol("updated_at", "CURRENT_TIMESTAMP"))

	plan := BuildPlan("postgres", Diff(both, one))
	up := plan.UpSQL()
	if strings.Contains(up, "DROP FUNCTION") {
		t.Errorf("남은 컬럼이 있는데 함수를 지웁니다:\n%s", up)
	}
	if !strings.Contains(up, `NEW."updated_at"`) || strings.Contains(up, `NEW."touched_at"`) {
		t.Errorf("남은 컬럼만으로 다시 쓰지 않았습니다:\n%s", up)
	}
	if down := plan.DownSQL(); !strings.Contains(down, `NEW."touched_at"`) {
		t.Errorf("되돌리기가 뺀 컬럼을 되살리지 않습니다:\n%s", down)
	}
}

// 한 표의 컬럼 변경이 여럿이어도 함수는 한 번만, 모든 컬럼이 제자리에 있을 때 쓴다.
func TestOnUpdateTriggerOncePerTable(t *testing.T) {
	from := pgPosts(pgTimeCol("old_at", "CURRENT_TIMESTAMP"), pgTimeCol("seen_at", ""))
	to := pgPosts(pgTimeCol("seen_at", "CURRENT_TIMESTAMP"), pgTimeCol("updated_at", "CURRENT_TIMESTAMP"))
	plan := BuildPlan("postgres", Diff(from, to))
	up := plan.UpSQL()

	if n := strings.Count(up, "CREATE OR REPLACE FUNCTION"); n != 1 {
		t.Fatalf("함수를 %d번 씁니다:\n%s", n, up)
	}
	fn := strings.Index(up, "CREATE OR REPLACE FUNCTION")
	for _, s := range []string{"ADD COLUMN", "DROP COLUMN", `ALTER TABLE "posts" ALTER COLUMN`} {
		if i := strings.Index(up, s); i > fn {
			t.Errorf("%s 가 함수보다 뒤에 있습니다:\n%s", s, up)
		}
	}
	if !strings.Contains(up, `NEW."seen_at"`) || !strings.Contains(up, `NEW."updated_at"`) || strings.Contains(up, `NEW."old_at"`) {
		t.Errorf("함수 본문이 바뀐 뒤의 컬럼과 다릅니다:\n%s", up)
	}
}

// 자동 갱신과 상관없는 변경에는 트리거를 건드리지 않는다. 건드리면 주석 하나 고친
// 마이그레이션마다 함수가 다시 쓰인다.
func TestOnUpdateTriggerUntouchedByOtherChanges(t *testing.T) {
	a := pgPosts(pgTimeCol("updated_at", "CURRENT_TIMESTAMP"))
	b := pgPosts(pgTimeCol("updated_at", "now()"))
	b.Tables[0].Columns[1].Comment = "고친 시각"
	up := BuildPlan("postgres", Diff(a, b)).UpSQL()
	if strings.Contains(up, "FUNCTION") || strings.Contains(up, "TRIGGER") {
		t.Errorf("주석만 고쳤는데 트리거를 건드립니다:\n%s", up)
	}
}

// 표를 지우면 트리거는 함께 사라지지만 함수는 남는다. 남은 함수는 초안에 보이지
// 않아 이 앱으로는 지울 길이 없다.
func TestDropTableDropsOnUpdateFunction(t *testing.T) {
	up := BuildPlan("postgres", Diff(pgPosts(pgTimeCol("updated_at", "CURRENT_TIMESTAMP")),
		&Schema{Dialect: "postgres", Shape: ShapeRelational})).UpSQL()
	table, fn := strings.Index(up, "DROP TABLE"), strings.Index(up, `DROP FUNCTION IF EXISTS "posts_on_update"()`)
	if table < 0 || fn < table {
		t.Errorf("표를 지운 뒤에 함수를 지우지 않습니다:\n%s", up)
	}
}

// 다른 DB 에서 있던 컬럼에 자동 갱신을 켜면 문장이 하나도 나가지 않는다. 경고가
// 없으면 반영된 줄 안다.
func TestOnUpdateAlterWarnsOnOtherDialects(t *testing.T) {
	for _, dialect := range []string{"mssql", "oracle", "sqlite"} {
		mk := func(onUpdate string) *Schema {
			return &Schema{Dialect: dialect, Shape: ShapeRelational, Tables: []*Table{{
				Name: "posts", Columns: []*Column{{
					Name: "updated_at", Position: 1, RawType: "timestamp",
					Type: LogicalType{Base: TypeTimestamp}, OnUpdate: onUpdate,
				}},
			}}}
		}
		plan := BuildPlan(dialect, Diff(mk(""), mk("CURRENT_TIMESTAMP")))
		found := false
		for _, w := range plan.Warnings {
			if strings.Contains(w, "트리거") {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: 자동 갱신을 켰는데 경고가 없습니다: %v", dialect, plan.Warnings)
		}
	}
}
