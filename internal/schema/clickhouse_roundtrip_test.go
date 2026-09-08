package schema

import "testing"

// 손대지 않은 컬럼은 왕복해도 같은 타입이어야 한다.
//
// 논리 타입에는 ClickHouse 만 아는 것들을 담을 자리가 없다. 그대로 다시 쓰면
// LowCardinality 가 풀리고 UInt32 가 UInt64 로 넓어지는데, 둘 다 오류 없이
// 지나가서 "같은 표를 다시 만들었다"고 믿게 된다.
func TestClickHouseColumnTypeKeepsRaw(t *testing.T) {
	cases := []struct {
		raw      string
		nullable bool
		want     string
	}{
		{"LowCardinality(String)", false, "LowCardinality(String)"},
		{"LowCardinality(Nullable(String))", true, "LowCardinality(Nullable(String))"},
		// 널을 허용하게 바꿔도 사전 압축은 바깥에 남는다.
		// Nullable(LowCardinality(...)) 는 서버가 거절하는 조합이다.
		{"LowCardinality(String)", true, "LowCardinality(Nullable(String))"},
		{"UInt32", false, "UInt32"},
		{"UInt8", false, "UInt8"},
		{"Int128", false, "Int128"},
		{"Nullable(UInt64)", true, "Nullable(UInt64)"},
		{"Nullable(UInt64)", false, "UInt64"},
		{"DateTime64(3)", false, "DateTime64(3)"},
		{"FixedString(16)", false, "FixedString(16)"},
		{"IPv4", false, "IPv4"},
		{"Array(String)", true, "Array(String)"},
	}
	for _, c := range cases {
		t.Run(c.raw, func(t *testing.T) {
			got := ClickHouseColumnType(parseClickHouseType(c.raw), c.raw, c.nullable)
			if got != c.want {
				t.Errorf("%s(널허용=%v) → %s, 기대: %s", c.raw, c.nullable, got, c.want)
			}
		})
	}
}

// 사람이 타입을 바꿨으면 논리 타입이 이긴다. 원래 문자열을 무조건 지키면
// 편집이 반영되지 않는다.
func TestClickHouseColumnTypeFollowsEdit(t *testing.T) {
	// String 이던 컬럼을 정수로 바꿨다.
	got := ClickHouseColumnType(LogicalType{Base: TypeBigInt}, "LowCardinality(String)", false)
	if got != "Int64" {
		t.Errorf("바뀐 타입이 반영되지 않았습니다: %s", got)
	}
	// 자릿수를 바꿨다.
	got = ClickHouseColumnType(LogicalType{Base: TypeDecimal, Precision: 10, Scale: 2},
		"Decimal(38, 10)", false)
	if got != "Decimal(10, 2)" {
		t.Errorf("바뀐 자릿수가 반영되지 않았습니다: %s", got)
	}
}
