package sqlimport

import (
	"testing"

	"dbstudio/internal/schema"
)

// ClickHouse 표는 꼬리에 붙는 절이 구조 그 자체다. 담지 않으면 엔진도 정렬 키도
// 보관 기간도 없는 다른 표가 되는데, 경고 한 줄 나오지 않는다.
func TestClickHouseTableTailBecomesOptions(t *testing.T) {
	res, err := Parse("clickhouse", `
CREATE TABLE metrics_daily (
  day Date,
  tenant LowCardinality(String),
  metric LowCardinality(String),
  total UInt64,
  avg_ms Nullable(Float64)
) ENGINE = SummingMergeTree ORDER BY (tenant, metric, day) PARTITION BY toYYYYMM(day)
TTL day + toIntervalDay(365)
COMMENT '테넌트별 일일 집계';`)
	if err != nil {
		t.Fatal(err)
	}
	tbl := findTable(res, "metrics_daily")
	if tbl == nil {
		t.Fatal("표를 읽지 못했습니다")
	}
	want := map[string]string{
		"engine":       "SummingMergeTree",
		"order_by":     "tenant, metric, day",
		"partition_by": "toYYYYMM(day)",
		"ttl":          "day + toIntervalDay(365)",
	}
	for k, v := range want {
		if got := tbl.Options[k]; got != v {
			t.Errorf("%s = %q, 기대: %q", k, got, v)
		}
	}
	if tbl.Comment != "테넌트별 일일 집계" {
		t.Errorf("주석: %q", tbl.Comment)
	}
	// 정렬 키가 기본키가 된다. ERD 가 그것을 열쇠로 그린다.
	if tbl.PrimaryKey == nil || len(tbl.PrimaryKey.Columns) != 3 {
		t.Errorf("정렬 키에서 기본키를 만들지 못했습니다: %+v", tbl.PrimaryKey)
	}
}

// 널 허용은 타입 안에 있다. 다른 방언의 규칙을 그대로 쓰면 모든 컬럼이 널
// 허용으로 들어오고, DDL 을 쓸 때 전부 Nullable 로 감싸진다.
func TestClickHouseNullabilityComesFromType(t *testing.T) {
	res, err := Parse("clickhouse", `
CREATE TABLE t (
  day Date,
  tenant LowCardinality(String),
  avg_ms Nullable(Float64),
  note LowCardinality(Nullable(String))
) ENGINE = MergeTree ORDER BY (day);`)
	if err != nil {
		t.Fatal(err)
	}
	tbl := findTable(res, "t")
	want := map[string]bool{
		"day": false, "tenant": false, "avg_ms": true, "note": true,
	}
	for name, nullable := range want {
		c := tbl.Column(name)
		if c == nil {
			t.Fatalf("%s 컬럼이 없습니다", name)
		}
		if c.Nullable != nullable {
			t.Errorf("%s 널 허용 = %v, 기대: %v", name, c.Nullable, nullable)
		}
	}
}

// RawType 은 "DB가 말한 그대로"여야 한다. 여는 괄호 뒤에 공백이 끼면 뜻은 같아도
// 원문과 다른 글자가 된다.
func TestBracketTypeKeepsItsShape(t *testing.T) {
	res, err := Parse("clickhouse", `
CREATE TABLE t (
  a LowCardinality(String),
  b Nullable(Float64),
  c Array(Nullable(UInt64)),
  d DateTime64(3)
) ENGINE = MergeTree ORDER BY tuple();`)
	if err != nil {
		t.Fatal(err)
	}
	tbl := findTable(res, "t")
	want := map[string]string{
		"a": "LowCardinality(String)",
		"b": "Nullable(Float64)",
		"c": "Array(Nullable(UInt64))",
		"d": "DateTime64(3)",
	}
	for name, raw := range want {
		if got := tbl.Column(name).RawType; got != raw {
			t.Errorf("%s 타입 = %q, 기대: %q", name, got, raw)
		}
	}
}

// 정렬하지 않는 표(ORDER BY tuple())도 그대로 읽어야 한다.
// 여기서 tuple() 을 컬럼 이름으로 보면 없는 컬럼이 기본키가 된다.
func TestClickHouseOrderByTuple(t *testing.T) {
	res, err := Parse("clickhouse", `
CREATE TABLE t (a UInt64) ENGINE = MergeTree ORDER BY tuple();`)
	if err != nil {
		t.Fatal(err)
	}
	tbl := findTable(res, "t")
	if got := tbl.Options["order_by"]; got != "tuple()" {
		t.Errorf("order_by = %q, 기대: tuple()", got)
	}
	if tbl.PrimaryKey != nil {
		t.Errorf("없는 컬럼이 기본키가 됐습니다: %+v", tbl.PrimaryKey)
	}
}

// findTable은 결과에서 표 하나를 찾는다.
func findTable(res *Result, name string) *schema.Table {
	for _, t := range res.Tables {
		if t.Name == name {
			return t
		}
	}
	return nil
}
