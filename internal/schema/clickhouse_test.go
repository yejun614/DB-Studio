package schema

import (
	"strings"
	"testing"
)

// ClickHouse 는 널 허용을 **타입 안에** 둔다. 다른 방언처럼 NOT NULL 을 뒤에 붙이면
// 서버가 문장을 거절한다.
func TestClickHouseNullabilityLivesInType(t *testing.T) {
	sc := &Schema{Dialect: "clickhouse", Shape: ShapeRelational, Tables: []*Table{{
		Name: "events",
		Columns: []*Column{
			{Name: "id", Position: 1, Type: LogicalType{Base: TypeBigInt}},
			{Name: "note", Position: 2, Type: LogicalType{Base: TypeText}, Nullable: true},
		},
		PrimaryKey: &PrimaryKey{Columns: []string{"id"}},
	}}}
	sql := BuildPlan("clickhouse", Diff(&Schema{
		Dialect: "clickhouse", Shape: ShapeRelational}, sc)).UpSQL()

	if strings.Contains(sql, "NOT NULL") {
		t.Errorf("NOT NULL 이 나갔습니다 — ClickHouse 는 이 문법을 거절합니다:\n%s", sql)
	}
	if !strings.Contains(sql, "Nullable(String)") {
		t.Errorf("널 허용 컬럼이 Nullable 로 감싸이지 않았습니다:\n%s", sql)
	}
	if !strings.Contains(sql, "`id` Int64") {
		t.Errorf("널을 담지 않는 컬럼이 감싸였습니다:\n%s", sql)
	}
}

// 정렬 키 없는 MergeTree 표는 서버가 만들지 않는다. 기본키를 정렬 키로 쓴다.
func TestClickHouseCreateTableHasEngineAndOrder(t *testing.T) {
	sc := &Schema{Dialect: "clickhouse", Shape: ShapeRelational, Tables: []*Table{{
		Name:       "events",
		Options:    map[string]string{"engine": "ReplacingMergeTree", "partition_by": "toYYYYMM(at)"},
		Columns:    []*Column{{Name: "id", Position: 1, Type: LogicalType{Base: TypeBigInt}}},
		PrimaryKey: &PrimaryKey{Columns: []string{"id"}},
	}}}
	sql := BuildPlan("clickhouse", Diff(&Schema{
		Dialect: "clickhouse", Shape: ShapeRelational}, sc)).UpSQL()

	for _, want := range []string{"ENGINE = ReplacingMergeTree", "ORDER BY (`id`)", "PARTITION BY toYYYYMM(at)"} {
		if !strings.Contains(sql, want) {
			t.Errorf("%s 가 없습니다:\n%s", want, sql)
		}
	}
	// 표 안에 PRIMARY KEY 절을 또 내면 ORDER BY 와 어긋날 때 서버가 거절한다.
	if strings.Contains(sql, "PRIMARY KEY (") {
		t.Errorf("PRIMARY KEY 절이 엔진 절과 중복해서 나갔습니다:\n%s", sql)
	}
}

// 정렬 키도 기본키도 없으면 만들어지기는 하되 그 사실을 경고로 말해야 한다.
func TestClickHouseWarnsWithoutSortKey(t *testing.T) {
	sc := &Schema{Dialect: "clickhouse", Shape: ShapeRelational, Tables: []*Table{{
		Name:    "raw",
		Columns: []*Column{{Name: "line", Position: 1, Type: LogicalType{Base: TypeText}}},
	}}}
	plan := BuildPlan("clickhouse", Diff(&Schema{
		Dialect: "clickhouse", Shape: ShapeRelational}, sc))
	if !strings.Contains(plan.UpSQL(), "ORDER BY tuple()") {
		t.Errorf("정렬하지 않는 표로 만들지 않았습니다:\n%s", plan.UpSQL())
	}
	if len(plan.Warnings) == 0 {
		t.Error("정렬 키가 없다는 경고가 없습니다")
	}
}

// 외래키·체크·인덱스는 문법이 없거나 다르다. 문장을 만들어 내면 실행하는 순간
// 문법 오류가 나고, 그때는 계획의 절반이 이미 적용된 뒤다.
func TestClickHouseSkipsUnsupportedChanges(t *testing.T) {
	base := func() *Schema {
		return &Schema{Dialect: "clickhouse", Shape: ShapeRelational, Tables: []*Table{{
			Name: "orders", Options: map[string]string{},
			Columns: []*Column{
				{Name: "id", Position: 1, Type: LogicalType{Base: TypeBigInt}},
				{Name: "member_id", Position: 2, Type: LogicalType{Base: TypeBigInt}},
			},
			PrimaryKey: &PrimaryKey{Columns: []string{"id"}},
		}}}
	}
	from, to := base(), base()
	to.Tables[0].ForeignKeys = []*ForeignKey{{
		Name: "fk_member", Columns: []string{"member_id"},
		RefTable: "members", RefColumns: []string{"id"},
	}}
	to.Tables[0].Indexes = []*Index{{
		Name: "idx_member", Columns: []IndexPart{{Column: "member_id"}},
	}}
	plan := BuildPlan("clickhouse", Diff(from, to))

	up := plan.UpSQL()
	if strings.Contains(up, "FOREIGN KEY") || strings.Contains(up, "CREATE INDEX") {
		t.Errorf("ClickHouse 에 없는 문장이 나갔습니다:\n%s", up)
	}
	if len(plan.Warnings) < 2 {
		t.Errorf("건너뛴 이유가 경고에 없습니다: %v", plan.Warnings)
	}
}

// 타입 읽기: 감싼 것을 벗기고 알맹이를 알아본다.
func TestParseClickHouseTypes(t *testing.T) {
	cases := []struct {
		raw      string
		base     BaseType
		nullable bool
	}{
		{"String", TypeText, false},
		{"Nullable(String)", TypeText, true},
		{"LowCardinality(Nullable(String))", TypeText, true},
		{"UInt64", TypeBigInt, false},
		{"DateTime64(3)", TypeTimestamp, false},
		{"Decimal(18, 4)", TypeDecimal, false},
		{"Array(UInt32)", TypeArray, false},
		{"Map(String, UInt64)", TypeDocument, false},
		{"FixedString(16)", TypeChar, false},
		{"UUID", TypeUUID, false},
	}
	for _, tc := range cases {
		got := ParseType("clickhouse", tc.raw)
		if got.Base != tc.base {
			t.Errorf("ParseType(%q).Base = %q, 기댓값 %q", tc.raw, got.Base, tc.base)
		}
		if _, nullable := UnwrapClickHouseType(tc.raw); nullable != tc.nullable {
			t.Errorf("UnwrapClickHouseType(%q) 널 허용 = %v, 기댓값 %v", tc.raw, nullable, tc.nullable)
		}
	}
	// 배열은 감쌀 수 없다. Nullable(Array(...)) 은 서버가 거절한다.
	arr := ParseType("clickhouse", "Array(String)")
	if got := ClickHouseColumnType(arr, "Array(String)", true); got != "Array(String)" {
		t.Errorf("배열을 Nullable 로 감쌌습니다: %q", got)
	}
}

// 식으로 된 정렬 키의 괄호를 잘라 먹으면 안 된다.
func TestUnwrapParensKeepsExpressions(t *testing.T) {
	cases := map[string]string{
		"(a, b)":                  "a, b",
		"toYYYYMM(at)":            "toYYYYMM(at)",
		"(toYYYYMM(at), user_id)": "toYYYYMM(at), user_id",
		"a":                       "a",
	}
	for in, want := range cases {
		if got := unwrapParens(in); got != want {
			t.Errorf("unwrapParens(%q) = %q, 기댓값 %q", in, got, want)
		}
	}
}
