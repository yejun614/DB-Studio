package schema

import (
	"fmt"
	"strings"
)

// ClickHouse 의 타입과 DDL.
//
// 다른 관계형 DB 와 갈리는 것이 셋이다.
//
//  1. **널 허용이 타입에 있다.** 다른 DB 는 컬럼에 NOT NULL 을 붙이지만 ClickHouse 는
//     타입 자체를 Nullable(String) 으로 감싼다. 널을 허용하면 값마다 표시 비트가
//     하나 더 붙고 색인이 그만큼 커지므로, 감싸지 않은 것이 기본이다.
//  2. **정렬 키가 곧 구조다.** MergeTree 는 ORDER BY 로 정렬해 저장하고 그 순서가
//     읽기 성능을 정한다. 정렬 키 없는 MergeTree 표는 만들 수 없다.
//  3. **외래키가 없다.** 참조 무결성을 강제하지 않는다 — 열 지향 저장에서 그 검사는
//     쓰기마다 임의 접근을 만들고, 그것이 이 DB 가 빠른 이유를 지운다.
//
// 셋을 모르는 채로 다른 방언의 DDL 을 그대로 내면 문법 오류가 나거나(정렬 키),
// 조용히 다른 표가 만들어진다(널 허용).

// unwrapClickHouseType은 감싼 타입을 벗겨 알맹이와 널 허용 여부를 돌려준다.
//
// LowCardinality 는 사전 압축이라 논리 타입이 바뀌지 않는다. Nullable 은 널 허용을
// 뜻하므로 벗기면서 그 사실을 함께 돌려준다.
func unwrapClickHouseType(raw string) (inner string, nullable bool) {
	inner = strings.TrimSpace(raw)
	for {
		lower := strings.ToLower(inner)
		switch {
		case strings.HasPrefix(lower, "nullable(") && strings.HasSuffix(inner, ")"):
			inner = strings.TrimSpace(inner[len("nullable(") : len(inner)-1])
			nullable = true
		case strings.HasPrefix(lower, "lowcardinality(") && strings.HasSuffix(inner, ")"):
			inner = strings.TrimSpace(inner[len("lowcardinality(") : len(inner)-1])
		default:
			return inner, nullable
		}
	}
}

// parseClickHouseType은 ClickHouse 타입 문자열을 논리 타입으로 바꾼다.
func parseClickHouseType(raw string) LogicalType {
	inner, _ := unwrapClickHouseType(raw)
	lower := strings.ToLower(inner)

	if strings.HasPrefix(lower, "array(") && strings.HasSuffix(inner, ")") {
		el := parseClickHouseType(inner[len("array(") : len(inner)-1])
		return LogicalType{Base: TypeArray, Element: &el}
	}
	// Map·Tuple·Nested 는 어떤 논리 타입으로도 옮길 수 없다. 문서 타입으로 두면
	// 다른 DB 의 JSON 컬럼과 나란히 보이고, 그것이 실제 쓰임에 가장 가깝다.
	switch {
	case strings.HasPrefix(lower, "map("), strings.HasPrefix(lower, "tuple("),
		strings.HasPrefix(lower, "nested("), lower == "json", lower == "object('json')":
		return LogicalType{Base: TypeDocument}
	case strings.HasPrefix(lower, "enum8("), strings.HasPrefix(lower, "enum16("):
		inner := inner[strings.Index(inner, "(")+1 : strings.LastIndex(inner, ")")]
		vals := []string{}
		for _, part := range splitTopLevel(inner) {
			// Enum8('a' = 1, 'b' = 2) — 이름만 취한다.
			name := strings.TrimSpace(part)
			if eq := strings.Index(name, "="); eq >= 0 {
				name = strings.TrimSpace(name[:eq])
			}
			vals = append(vals, strings.Trim(name, "'\""))
		}
		return LogicalType{Base: TypeEnum, Values: vals}
	case strings.HasPrefix(lower, "fixedstring("):
		t := LogicalType{Base: TypeChar}
		t.Length, _ = parseTwoInts(inner[len("fixedstring(") : len(inner)-1])
		return t
	case strings.HasPrefix(lower, "decimal"):
		t := LogicalType{Base: TypeDecimal}
		if open := strings.Index(inner, "("); open >= 0 {
			t.Precision, t.Scale = parseTwoInts(inner[open+1 : len(inner)-1])
		}
		return t
	case strings.HasPrefix(lower, "datetime64"):
		return LogicalType{Base: TypeTimestamp}
	}

	name := lower
	if open := strings.Index(name, "("); open >= 0 {
		name = strings.TrimSpace(name[:open])
	}
	if bt, ok := clickHouseTypes[name]; ok {
		t := LogicalType{Base: bt}
		// 부호 없는 정수라는 사실을 잃지 않는다. 다른 DB 로 옮길 때 범위를
		// 넓혀 잡아야 하는 근거가 이것이다.
		t.Unsigned = strings.HasPrefix(name, "uint")
		return t
	}
	return LogicalType{Base: TypeUnknown}
}

var clickHouseTypes = map[string]BaseType{
	"int8": TypeSmallInt, "int16": TypeSmallInt,
	"int32": TypeInt, "int64": TypeBigInt, "int128": TypeDecimal, "int256": TypeDecimal,
	"uint8": TypeSmallInt, "uint16": TypeInt, "uint32": TypeBigInt,
	"uint64": TypeBigInt, "uint128": TypeDecimal, "uint256": TypeDecimal,
	"float32": TypeFloat, "float64": TypeDouble,
	"bool": TypeBool, "boolean": TypeBool,
	"string": TypeText, "uuid": TypeUUID,
	"date": TypeDate, "date32": TypeDate,
	"datetime": TypeTimestamp,
	"ipv4":     TypeVarchar, "ipv6": TypeVarchar,
}

// renderClickHouse는 논리 타입을 ClickHouse 타입으로 쓴다.
//
// 널 허용은 여기서 다루지 않는다 — 감싸는 것은 컬럼을 아는 쪽(columnDef)의 일이고,
// 여기서 감싸면 배열의 원소까지 Nullable 이 되어 뜻이 달라진다.
func renderClickHouse(t LogicalType) string {
	switch t.Base {
	case TypeBool:
		return "Bool"
	case TypeSmallInt:
		if t.Unsigned {
			return "UInt16"
		}
		return "Int16"
	case TypeInt:
		if t.Unsigned {
			return "UInt32"
		}
		return "Int32"
	case TypeBigInt:
		if t.Unsigned {
			return "UInt64"
		}
		return "Int64"
	case TypeDecimal:
		if t.Precision > 0 {
			return fmt.Sprintf("Decimal(%d, %d)", t.Precision, t.Scale)
		}
		return "Decimal(38, 10)"
	case TypeFloat:
		return "Float32"
	case TypeDouble:
		return "Float64"
	case TypeChar:
		if t.Length > 0 {
			return fmt.Sprintf("FixedString(%d)", t.Length)
		}
		return "String"
	case TypeVarchar, TypeText, TypeJSON:
		// 길이 제한이 없다. String 하나가 모든 문자열이고, 길이를 붙이면
		// FixedString 이 되어 **고정 길이**가 된다 — 뜻이 완전히 다르다.
		return "String"
	case TypeUUID:
		return "UUID"
	case TypeDate:
		return "Date"
	case TypeTime:
		// 시각만 담는 타입이 없다. 문자열로 두는 것이 잘못된 날짜를 붙이는 것보다 낫다.
		return "String"
	case TypeTimestamp, TypeTimestampTZ:
		return "DateTime64(3)"
	case TypeBinary, TypeBlob:
		return "String"
	case TypeEnum:
		if len(t.Values) == 0 {
			return "String"
		}
		parts := make([]string, 0, len(t.Values))
		for i, v := range t.Values {
			parts = append(parts, fmt.Sprintf("%s = %d", quoteLiteral(v), i+1))
		}
		return "Enum16(" + strings.Join(parts, ", ") + ")"
	case TypeArray:
		if t.Element != nil {
			return "Array(" + renderClickHouse(*t.Element) + ")"
		}
		return "Array(String)"
	case TypeDocument:
		return "String"
	}
	return "String"
}

// ClickHouseColumnType은 널 허용까지 반영한 컬럼 타입이다.
//
// 널 허용을 타입으로 감싸는 것이 ClickHouse 의 방식이다. NOT NULL 을 뒤에 붙이는
// 다른 방언과 달라서, 컬럼 정의를 쓰는 쪽이 이 함수를 거쳐야 한다.
//
// 손대지 않은 컬럼은 **원래 타입 문자열을 그대로 쓴다.** 논리 타입으로 다시
// 쓰면 ClickHouse 만 아는 것들이 조용히 사라지기 때문이다. 재 보면 두 가지가
// 그랬다.
//
//   - LowCardinality(String) 이 String 이 된다. 사전 압축이 풀리는 것이라
//     같은 표가 아닌데, 논리 타입에는 그것을 담을 자리가 없다.
//   - UInt32 가 UInt64 가 된다. uint32 를 큰 정수로 읽는 것은 **다른 DB 로
//     옮길 때** 범위가 넘치지 않게 하려는 것이라 그 자체로는 옳지만, 같은
//     ClickHouse 로 돌아올 때는 컬럼이 두 배로 넓어진다.
//
// 그래서 원래 문자열이 아직 같은 뜻이면 그것을 쓰고, 사람이 타입을 바꿨을
// 때만(둘의 뜻이 갈릴 때만) 논리 타입으로 다시 쓴다.
func ClickHouseColumnType(t LogicalType, raw string, nullable bool) string {
	base := strings.TrimSpace(raw)
	if base == "" || parseClickHouseType(base).Canonical() != t.Canonical() {
		base = renderClickHouse(t)
	}

	inner, alreadyNullable := stripClickHouseNullable(base)
	if !nullable {
		return inner
	}
	if alreadyNullable {
		return base
	}
	// 배열은 감쌀 수 없다(Nullable(Array(...)) 는 지원되지 않는다). 빈 배열이
	// 곧 "없음"이라 감쌀 이유도 없다.
	if strings.HasPrefix(strings.ToLower(inner), "array(") {
		return inner
	}
	// LowCardinality 는 바깥에 남는다. Nullable(LowCardinality(String)) 은
	// 지원되지 않는 조합이고, 서버가 쓰는 모양은 LowCardinality(Nullable(String)) 이다.
	if lc, ok := cutClickHouseWrapper(inner, "lowcardinality"); ok {
		return "LowCardinality(Nullable(" + lc + "))"
	}
	return "Nullable(" + inner + ")"
}

// stripClickHouseNullable은 Nullable 만 벗기고 나머지 껍질은 남긴다.
//
// unwrapClickHouseType 과 다른 점이 여기다. 그쪽은 논리 타입을 알아내려고
// LowCardinality 까지 벗기는데, 쓰는 쪽에서 그 결과를 그대로 내보내면
// 사전 압축이 사라진다.
func stripClickHouseNullable(raw string) (inner string, nullable bool) {
	s := strings.TrimSpace(raw)
	if v, ok := cutClickHouseWrapper(s, "nullable"); ok {
		return v, true
	}
	if v, ok := cutClickHouseWrapper(s, "lowcardinality"); ok {
		if n, ok := cutClickHouseWrapper(v, "nullable"); ok {
			return "LowCardinality(" + n + ")", true
		}
	}
	return s, false
}

// cutClickHouseWrapper는 감싼 타입 한 겹을 벗긴다. 이름이 다르면 그대로 둔다.
func cutClickHouseWrapper(raw, name string) (string, bool) {
	s := strings.TrimSpace(raw)
	if !strings.HasPrefix(strings.ToLower(s), name+"(") || !strings.HasSuffix(s, ")") {
		return s, false
	}
	return strings.TrimSpace(s[len(name)+1 : len(s)-1]), true
}

// ClickHouseEngineClause는 CREATE TABLE 뒤에 붙는 절이다.
//
// 정렬 키가 반드시 있어야 하는 이유: MergeTree 계열은 ORDER BY 로 정렬해 저장하고
// 그 순서가 읽기 성능을 정한다. 없으면 서버가 문장을 거절한다. 기본키를 정렬 키로
// 쓰고, 그것도 없으면 tuple()(정렬하지 않음)로 둔다 — 문장이 거절되는 것보다는
// 만들어지는 편이 낫고, 화면이 그 사실을 경고로 말한다.
func ClickHouseEngineClause(t *Table, ident func(string) string) string {
	engine := strings.TrimSpace(t.Options["engine"])
	if engine == "" {
		engine = "MergeTree"
	}
	var b strings.Builder
	fmt.Fprintf(&b, " ENGINE = %s", engine)

	// 정렬 키를 받지 않는 엔진이 있다. Kafka·Log·Memory 처럼 병합할 파트가
	// 없는 것들이다. 거기에 ORDER BY 를 붙이면 서버가 문장을 거절한다 —
	// 실제로 Kafka 표에 ORDER BY tuple() 이 나가고 있었다.
	if !clickHouseEngineTakesOrder(engine) {
		if part := strings.TrimSpace(t.Options["partition_by"]); part != "" {
			fmt.Fprintf(&b, " PARTITION BY %s", part)
		}
		writeClickHouseTail(&b, t)
		return b.String()
	}

	order := strings.TrimSpace(t.Options["order_by"])
	if order == "" && t.PrimaryKey != nil && len(t.PrimaryKey.Columns) > 0 {
		cols := make([]string, 0, len(t.PrimaryKey.Columns))
		for _, c := range t.PrimaryKey.Columns {
			cols = append(cols, ident(c))
		}
		order = strings.Join(cols, ", ")
	}
	if order == "" {
		// tuple() 이 "정렬하지 않는다"는 뜻이다. 괄호로 한 번 더 감싸면
		// (tuple) 이 되어 **tuple 이라는 이름의 컬럼**을 가리키게 되고, 그런
		// 컬럼은 없으므로 서버가 문장을 거절한다.
		b.WriteString(" ORDER BY tuple()")
	} else {
		fmt.Fprintf(&b, " ORDER BY (%s)", unwrapParens(order))
	}

	if part := strings.TrimSpace(t.Options["partition_by"]); part != "" {
		fmt.Fprintf(&b, " PARTITION BY %s", part)
	}
	writeClickHouseTail(&b, t)
	return b.String()
}

// writeClickHouseTail은 어느 엔진에나 같은 모양으로 붙는 뒷부분을 쓴다.
//
// SETTINGS 는 엔진에 따라 표를 표이게 하는 전부다. Kafka 엔진은 브로커 주소와
// 토픽 이름이 거기 있어서, 빼고 만들면 서버가 문장을 거절한다.
func writeClickHouseTail(b *strings.Builder, t *Table) {
	if ttl := strings.TrimSpace(t.Options["ttl"]); ttl != "" {
		fmt.Fprintf(b, " TTL %s", ttl)
	}
	if st := strings.TrimSpace(t.Options["settings"]); st != "" {
		fmt.Fprintf(b, " SETTINGS %s", st)
	}
	if t.Comment != "" {
		fmt.Fprintf(b, " COMMENT %s", quoteLiteral(t.Comment))
	}
}

// clickHouseEngineTakesOrder는 그 엔진이 정렬 키를 받는지다.
//
// 정렬 키는 MergeTree 계열의 것이다. 병합할 파트가 없는 엔진(Kafka·Log·
// Memory·Distributed 등)에 붙이면 문법 오류가 난다.
func clickHouseEngineTakesOrder(engine string) bool {
	name := engine
	if open := strings.Index(name, "("); open >= 0 {
		name = name[:open]
	}
	return strings.Contains(strings.ToLower(strings.TrimSpace(name)), "mergetree")
}

// UnwrapClickHouseType은 감싼 타입을 벗겨 알맹이와 널 허용 여부를 돌려준다.
//
// 밖에서도 필요한 이유: 스키마를 읽는 쪽(dbx)이 "이 컬럼이 널을 담을 수 있는가"를
// 판단해야 하는데, ClickHouse 는 그 답이 컬럼이 아니라 **타입 문자열**에 있다.
// 벗기는 규칙이 두 곳에 있으면 언젠가 갈라진다.
func UnwrapClickHouseType(raw string) (inner string, nullable bool) {
	return unwrapClickHouseType(raw)
}

// unwrapParens는 식 전체를 감싼 괄호 한 겹만 벗긴다.
//
// strings.Trim 을 쓰지 않는 이유: 그것은 양 끝의 괄호를 **몇 개든** 벗겨서
// "toYYYYMM(at)" 같은 식의 닫는 괄호까지 가져간다.
func unwrapParens(expr string) string {
	expr = strings.TrimSpace(expr)
	if !strings.HasPrefix(expr, "(") || !strings.HasSuffix(expr, ")") {
		return expr
	}
	depth := 0
	for i, r := range expr {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
			// 마지막 문자에서야 0이 되어야 **전체**를 감싼 괄호다.
			if depth == 0 && i != len(expr)-1 {
				return expr
			}
		}
	}
	return strings.TrimSpace(expr[1 : len(expr)-1])
}
