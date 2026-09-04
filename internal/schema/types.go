package schema

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// BaseType은 DB별 타입을 비교 가능한 논리 타입으로 정규화한 값이다.
//
// 정규화가 필요한 이유: MySQL의 `int(11)`, PostgreSQL의 `integer`, MS-SQL의 `int`,
// Oracle의 `NUMBER(10,0)`은 모두 같은 의도다. 원본 문자열로만 비교하면
// "DB를 옮겼을 뿐인데 모든 컬럼이 변경됨"으로 잡힌다.
type BaseType string

const (
	TypeUnknown     BaseType = "unknown"
	TypeBool        BaseType = "bool"
	TypeSmallInt    BaseType = "smallint"
	TypeInt         BaseType = "int"
	TypeBigInt      BaseType = "bigint"
	TypeDecimal     BaseType = "decimal"
	TypeFloat       BaseType = "float"
	TypeDouble      BaseType = "double"
	TypeChar        BaseType = "char"
	TypeVarchar     BaseType = "varchar"
	TypeText        BaseType = "text"
	TypeBinary      BaseType = "binary"
	TypeBlob        BaseType = "blob"
	TypeDate        BaseType = "date"
	TypeTime        BaseType = "time"
	TypeTimestamp   BaseType = "timestamp"   // 타임존 없음
	TypeTimestampTZ BaseType = "timestamptz" // 타임존 포함
	TypeUUID        BaseType = "uuid"
	TypeJSON        BaseType = "json"
	TypeEnum        BaseType = "enum"
	TypeArray       BaseType = "array"
	TypeGeometry    BaseType = "geometry"
	TypeInterval    BaseType = "interval"
	TypeObjectID    BaseType = "objectid" // MongoDB
	TypeDocument    BaseType = "document" // MongoDB 중첩 문서
)

// LogicalType은 정규화된 타입과 그 파라미터다.
type LogicalType struct {
	Base      BaseType     `json:"base"`
	Length    int          `json:"length,omitempty"`    // varchar(n), char(n)
	Precision int          `json:"precision,omitempty"` // decimal(p,s)
	Scale     int          `json:"scale,omitempty"`
	Unsigned  bool         `json:"unsigned,omitempty"`
	Values    []string     `json:"values,omitempty"`   // 인라인 enum (MySQL)
	EnumName  string       `json:"enumName,omitempty"` // 이름 붙은 enum (PostgreSQL)
	Element   *LogicalType `json:"element,omitempty"`  // 배열 원소 타입
}

// Canonical은 비교와 지문 계산에 쓰는 정규 문자열이다.
func (t LogicalType) Canonical() string {
	var b strings.Builder
	b.WriteString(string(t.Base))
	switch t.Base {
	case TypeVarchar, TypeChar, TypeBinary:
		if t.Length > 0 {
			fmt.Fprintf(&b, "(%d)", t.Length)
		}
	case TypeDecimal:
		if t.Precision > 0 {
			fmt.Fprintf(&b, "(%d,%d)", t.Precision, t.Scale)
		}
	case TypeEnum:
		if t.EnumName != "" {
			fmt.Fprintf(&b, ":%s", strings.ToLower(t.EnumName))
		} else if len(t.Values) > 0 {
			fmt.Fprintf(&b, "(%s)", strings.Join(t.Values, "|"))
		}
	case TypeArray:
		if t.Element != nil {
			fmt.Fprintf(&b, "<%s>", t.Element.Canonical())
		}
	}
	if t.Unsigned {
		b.WriteString(" unsigned")
	}
	return b.String()
}

// Equal은 두 타입이 논리적으로 같은지 판단한다.
func (t LogicalType) Equal(other LogicalType) bool { return t.Canonical() == other.Canonical() }

// IsWidening은 이 타입에서 other로의 변경이 데이터 손실 없이 가능한지 추정한다.
// 마이그레이션에서 파괴적 변경을 경고하기 위한 보수적 판정이다.
func (t LogicalType) IsWidening(other LogicalType) bool {
	rank := map[BaseType]int{TypeSmallInt: 1, TypeInt: 2, TypeBigInt: 3}
	if a, ok := rank[t.Base]; ok {
		if b, ok2 := rank[other.Base]; ok2 {
			return b >= a
		}
	}
	// 문자열 길이 확장은 안전, 축소는 위험하다.
	if t.Base == other.Base && (t.Base == TypeVarchar || t.Base == TypeChar || t.Base == TypeBinary) {
		if t.Length == 0 || other.Length == 0 {
			return other.Length == 0 // 무제한으로 가는 것은 안전
		}
		return other.Length >= t.Length
	}
	if (t.Base == TypeVarchar || t.Base == TypeChar) && other.Base == TypeText {
		return true
	}
	if t.Base == TypeFloat && other.Base == TypeDouble {
		return true
	}
	return t.Equal(other)
}

// ---------- raw 타입 → 논리 타입 파싱 ----------

var typeParamsRE = regexp.MustCompile(`^\s*([a-zA-Z0-9_ ]+?)\s*(?:\(\s*([^)]*)\s*\))?\s*(unsigned|signed)?\s*(?:zerofill)?\s*$`)

// ParseType은 dialect별 raw 타입 문자열을 논리 타입으로 변환한다.
// 인식하지 못한 타입은 TypeUnknown으로 두고 RawType을 신뢰한다 —
// 잘못 추측해서 엉뚱한 DDL을 만드는 것보다 모른다고 표시하는 편이 안전하다.
func ParseType(dialect, raw string) LogicalType {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return LogicalType{Base: TypeUnknown}
	}
	lower := strings.ToLower(raw)

	// ClickHouse 는 타입 문법 자체가 다르다(Nullable·LowCardinality·Array 로 감싼다).
	// 아래의 공통 파서에 넣으면 Nullable(String) 이 통째로 알 수 없는 타입이 된다.
	if dialect == "clickhouse" {
		return parseClickHouseType(raw)
	}

	// PostgreSQL 배열 표기: integer[] 또는 _int4
	if dialect == "postgres" {
		if strings.HasSuffix(lower, "[]") {
			el := ParseType(dialect, strings.TrimSuffix(raw, "[]"))
			return LogicalType{Base: TypeArray, Element: &el}
		}
		if strings.HasPrefix(lower, "_") {
			el := ParseType(dialect, raw[1:])
			return LogicalType{Base: TypeArray, Element: &el}
		}
	}

	// MySQL 인라인 enum/set: enum('a','b')
	if strings.HasPrefix(lower, "enum(") || strings.HasPrefix(lower, "set(") {
		inner := raw[strings.Index(raw, "(")+1 : strings.LastIndex(raw, ")")]
		vals := []string{}
		for _, part := range splitTopLevel(inner) {
			vals = append(vals, strings.Trim(strings.TrimSpace(part), "'\""))
		}
		return LogicalType{Base: TypeEnum, Values: vals}
	}

	m := typeParamsRE.FindStringSubmatch(lower)
	name, params := lower, ""
	if m != nil {
		name = strings.TrimSpace(m[1])
		params = strings.TrimSpace(m[2])
	}
	unsigned := m != nil && m[3] == "unsigned"

	t := LogicalType{Base: baseTypeOf(dialect, name), Unsigned: unsigned}
	p1, p2 := parseTwoInts(params)

	switch t.Base {
	case TypeVarchar, TypeChar, TypeBinary:
		t.Length = p1
	case TypeDecimal:
		t.Precision, t.Scale = p1, p2
		// Oracle NUMBER(p,0)은 정수 의도이므로 정수로 정규화한다.
		// 그렇지 않으면 다른 DB의 int 컬럼과 항상 다르게 잡힌다.
		if dialect == "oracle" && p2 == 0 && p1 > 0 {
			switch {
			case p1 <= 4:
				return LogicalType{Base: TypeSmallInt}
			case p1 <= 9:
				return LogicalType{Base: TypeInt}
			case p1 <= 18:
				return LogicalType{Base: TypeBigInt}
			}
		}
	case TypeUnknown:
		// 알 수 없는 타입도 파라미터는 보존해 사용자가 원인을 파악할 수 있게 한다.
		t.Length = p1
	}

	// MySQL tinyint(1)은 관례적으로 boolean이다.
	if dialect == "mysql" && name == "tinyint" && p1 == 1 {
		return LogicalType{Base: TypeBool}
	}
	return t
}

func baseTypeOf(dialect, name string) BaseType {
	if bt, ok := commonTypes[name]; ok {
		return bt
	}
	if per, ok := dialectTypes[dialect]; ok {
		if bt, ok := per[name]; ok {
			return bt
		}
	}
	return TypeUnknown
}

var commonTypes = map[string]BaseType{
	"boolean": TypeBool, "bool": TypeBool, "bit": TypeBool,
	"tinyint": TypeSmallInt, "smallint": TypeSmallInt, "int2": TypeSmallInt,
	"mediumint": TypeInt, "int": TypeInt, "integer": TypeInt, "int4": TypeInt,
	"bigint": TypeBigInt, "int8": TypeBigInt,
	"decimal": TypeDecimal, "numeric": TypeDecimal, "number": TypeDecimal,
	"dec": TypeDecimal, "money": TypeDecimal, "smallmoney": TypeDecimal,
	"float": TypeFloat, "real": TypeFloat, "float4": TypeFloat, "binary_float": TypeFloat,
	"double": TypeDouble, "double precision": TypeDouble, "float8": TypeDouble, "binary_double": TypeDouble,
	"char": TypeChar, "character": TypeChar, "nchar": TypeChar, "bpchar": TypeChar,
	"varchar": TypeVarchar, "varchar2": TypeVarchar, "nvarchar": TypeVarchar,
	"nvarchar2": TypeVarchar, "character varying": TypeVarchar,
	"text": TypeText, "tinytext": TypeText, "mediumtext": TypeText, "longtext": TypeText,
	"ntext": TypeText, "clob": TypeText, "nclob": TypeText,
	"binary": TypeBinary, "varbinary": TypeBinary, "raw": TypeBinary,
	"blob": TypeBlob, "tinyblob": TypeBlob, "mediumblob": TypeBlob, "longblob": TypeBlob,
	"bytea": TypeBlob, "image": TypeBlob, "bfile": TypeBlob, "long raw": TypeBlob,
	"date": TypeDate,
	"time": TypeTime, "time without time zone": TypeTime,
	"datetime": TypeTimestamp, "datetime2": TypeTimestamp, "smalldatetime": TypeTimestamp,
	"timestamp": TypeTimestamp, "timestamp without time zone": TypeTimestamp,
	"timestamptz": TypeTimestampTZ, "timestamp with time zone": TypeTimestampTZ,
	"datetimeoffset": TypeTimestampTZ, "timestamp with local time zone": TypeTimestampTZ,
	"timetz": TypeTimestampTZ, "time with time zone": TypeTimestampTZ,
	"uuid": TypeUUID, "uniqueidentifier": TypeUUID,
	"json": TypeJSON, "jsonb": TypeJSON,
	"interval": TypeInterval,
	"geometry": TypeGeometry, "geography": TypeGeometry, "point": TypeGeometry,
	"sdo_geometry": TypeGeometry,
}

var dialectTypes = map[string]map[string]BaseType{
	"postgres": {
		"serial": TypeInt, "serial4": TypeInt, "bigserial": TypeBigInt, "serial8": TypeBigInt,
		"smallserial": TypeSmallInt, "serial2": TypeSmallInt,
		"citext": TypeText, "name": TypeVarchar, "xml": TypeText,
		"inet": TypeVarchar, "cidr": TypeVarchar, "macaddr": TypeVarchar,
		"tsvector": TypeText, "bit varying": TypeBinary, "varbit": TypeBinary,
	},
	"mysql": {
		"year": TypeSmallInt, "set": TypeEnum,
	},
	"mssql": {
		"xml": TypeText, "sql_variant": TypeUnknown, "rowversion": TypeBinary, "timestamp_mssql": TypeBinary,
	},
	"oracle": {
		"long": TypeText, "rowid": TypeVarchar, "urowid": TypeVarchar,
		"binary_integer": TypeInt, "xmltype": TypeText,
	},
	"sqlite": {
		// SQLite는 타입 친화성(affinity)만 가지므로 선언된 이름을 최대한 존중한다.
		"": TypeText, "any": TypeUnknown,
	},
	"mongodb": {
		"objectid": TypeObjectID, "document": TypeDocument, "array": TypeArray,
		"string": TypeText, "long": TypeBigInt, "decimal128": TypeDecimal,
		"binary": TypeBlob, "null": TypeUnknown, "regex": TypeText, "mixed": TypeUnknown,
	},
}

// ---------- 논리 타입 → dialect DDL 타입 ----------

// RenderType은 논리 타입을 대상 dialect의 DDL 타입 문자열로 변환한다.
// preferRaw가 있고 대상 dialect가 원본과 같으면 원본을 그대로 써서 손실을 막는다.
func RenderType(dialect string, t LogicalType, preferRaw string) string {
	if preferRaw != "" && t.Base == TypeUnknown {
		// 정규화에 실패한 타입은 원본을 그대로 내보낸다. 추측보다 낫다.
		return preferRaw
	}
	switch dialect {
	case "mysql":
		return renderMySQL(t)
	case "postgres":
		return renderPostgres(t)
	case "mssql":
		return renderMSSQL(t)
	case "oracle":
		return renderOracle(t)
	case "sqlite":
		return renderSQLite(t)
	case "clickhouse":
		return renderClickHouse(t)
	}
	if preferRaw != "" {
		return preferRaw
	}
	return string(t.Base)
}

func renderMySQL(t LogicalType) string {
	suffix := ""
	if t.Unsigned {
		suffix = " UNSIGNED"
	}
	switch t.Base {
	case TypeBool:
		return "TINYINT(1)"
	case TypeSmallInt:
		return "SMALLINT" + suffix
	case TypeInt:
		return "INT" + suffix
	case TypeBigInt:
		return "BIGINT" + suffix
	case TypeDecimal:
		return decimalSpec("DECIMAL", t) + suffix
	case TypeFloat:
		return "FLOAT" + suffix
	case TypeDouble:
		return "DOUBLE" + suffix
	case TypeChar:
		return lengthSpec("CHAR", t, 1)
	case TypeVarchar:
		return lengthSpec("VARCHAR", t, 255)
	case TypeText:
		return "TEXT"
	case TypeBinary:
		return lengthSpec("VARBINARY", t, 255)
	case TypeBlob:
		return "LONGBLOB"
	case TypeDate:
		return "DATE"
	case TypeTime:
		return "TIME"
	case TypeTimestamp:
		return "DATETIME"
	case TypeTimestampTZ:
		// MySQL의 TIMESTAMP는 UTC 저장 + 세션 타임존 변환으로 tz 의미에 가장 가깝다.
		return "TIMESTAMP"
	case TypeUUID:
		return "CHAR(36)"
	case TypeJSON:
		return "JSON"
	case TypeEnum:
		if len(t.Values) > 0 {
			return "ENUM(" + quoteList(t.Values, "'") + ")"
		}
		return "VARCHAR(255)"
	case TypeArray, TypeDocument:
		return "JSON"
	case TypeGeometry:
		return "GEOMETRY"
	case TypeInterval:
		return "BIGINT"
	case TypeObjectID:
		return "CHAR(24)"
	}
	return "TEXT"
}

func renderPostgres(t LogicalType) string {
	switch t.Base {
	case TypeBool:
		return "boolean"
	case TypeSmallInt:
		return "smallint"
	case TypeInt:
		return "integer"
	case TypeBigInt:
		return "bigint"
	case TypeDecimal:
		return decimalSpec("numeric", t)
	case TypeFloat:
		return "real"
	case TypeDouble:
		return "double precision"
	case TypeChar:
		return lengthSpec("char", t, 1)
	case TypeVarchar:
		if t.Length > 0 {
			return fmt.Sprintf("varchar(%d)", t.Length)
		}
		return "text"
	case TypeText:
		return "text"
	case TypeBinary, TypeBlob:
		return "bytea"
	case TypeDate:
		return "date"
	case TypeTime:
		return "time"
	case TypeTimestamp:
		return "timestamp"
	case TypeTimestampTZ:
		return "timestamptz"
	case TypeUUID:
		return "uuid"
	case TypeJSON:
		return "jsonb"
	case TypeEnum:
		if t.EnumName != "" {
			return t.EnumName
		}
		return "text"
	case TypeArray:
		if t.Element != nil {
			return renderPostgres(*t.Element) + "[]"
		}
		return "jsonb"
	case TypeDocument:
		return "jsonb"
	case TypeGeometry:
		return "geometry"
	case TypeInterval:
		return "interval"
	case TypeObjectID:
		return "char(24)"
	}
	return "text"
}

func renderMSSQL(t LogicalType) string {
	switch t.Base {
	case TypeBool:
		return "bit"
	case TypeSmallInt:
		return "smallint"
	case TypeInt:
		return "int"
	case TypeBigInt:
		return "bigint"
	case TypeDecimal:
		return decimalSpec("decimal", t)
	case TypeFloat:
		return "real"
	case TypeDouble:
		return "float"
	case TypeChar:
		return lengthSpec("nchar", t, 1)
	case TypeVarchar:
		if t.Length > 0 {
			return fmt.Sprintf("nvarchar(%d)", t.Length)
		}
		return "nvarchar(max)"
	case TypeText:
		return "nvarchar(max)"
	case TypeBinary:
		if t.Length > 0 {
			return fmt.Sprintf("varbinary(%d)", t.Length)
		}
		return "varbinary(max)"
	case TypeBlob:
		return "varbinary(max)"
	case TypeDate:
		return "date"
	case TypeTime:
		return "time"
	case TypeTimestamp:
		return "datetime2"
	case TypeTimestampTZ:
		return "datetimeoffset"
	case TypeUUID:
		return "uniqueidentifier"
	case TypeJSON, TypeArray, TypeDocument:
		return "nvarchar(max)"
	case TypeEnum:
		return "nvarchar(255)"
	case TypeGeometry:
		return "geometry"
	case TypeInterval:
		return "bigint"
	case TypeObjectID:
		return "char(24)"
	}
	return "nvarchar(max)"
}

func renderOracle(t LogicalType) string {
	switch t.Base {
	case TypeBool:
		return "NUMBER(1)"
	case TypeSmallInt:
		return "NUMBER(5)"
	case TypeInt:
		return "NUMBER(10)"
	case TypeBigInt:
		return "NUMBER(19)"
	case TypeDecimal:
		return decimalSpec("NUMBER", t)
	case TypeFloat:
		return "BINARY_FLOAT"
	case TypeDouble:
		return "BINARY_DOUBLE"
	case TypeChar:
		return lengthSpec("CHAR", t, 1)
	case TypeVarchar:
		if t.Length > 0 {
			return fmt.Sprintf("VARCHAR2(%d)", t.Length)
		}
		return "VARCHAR2(4000)"
	case TypeText:
		return "CLOB"
	case TypeBinary:
		if t.Length > 0 && t.Length <= 2000 {
			return fmt.Sprintf("RAW(%d)", t.Length)
		}
		return "BLOB"
	case TypeBlob:
		return "BLOB"
	case TypeDate:
		return "DATE"
	case TypeTime:
		// Oracle에는 순수 TIME 타입이 없다. INTERVAL로 근사한다.
		return "INTERVAL DAY(0) TO SECOND(6)"
	case TypeTimestamp:
		return "TIMESTAMP"
	case TypeTimestampTZ:
		return "TIMESTAMP WITH TIME ZONE"
	case TypeUUID:
		return "RAW(16)"
	case TypeJSON, TypeArray, TypeDocument:
		return "CLOB"
	case TypeEnum:
		return "VARCHAR2(255)"
	case TypeGeometry:
		return "SDO_GEOMETRY"
	case TypeInterval:
		return "INTERVAL DAY TO SECOND"
	case TypeObjectID:
		return "CHAR(24)"
	}
	return "CLOB"
}

func renderSQLite(t LogicalType) string {
	// SQLite는 타입 친화성만 가진다. 다른 DB로 오갈 때 의도가 남도록
	// 친화성이 결정되는 이름을 고른다.
	switch t.Base {
	case TypeBool, TypeSmallInt, TypeInt, TypeBigInt:
		return "INTEGER"
	case TypeDecimal:
		return decimalSpec("NUMERIC", t)
	case TypeFloat, TypeDouble:
		return "REAL"
	case TypeChar, TypeVarchar:
		if t.Length > 0 {
			return fmt.Sprintf("VARCHAR(%d)", t.Length)
		}
		return "TEXT"
	case TypeBinary, TypeBlob:
		return "BLOB"
	case TypeDate:
		return "DATE"
	case TypeTime:
		return "TIME"
	case TypeTimestamp, TypeTimestampTZ:
		return "DATETIME"
	}
	return "TEXT"
}

// ---------- 내부 유틸 ----------

func lengthSpec(name string, t LogicalType, def int) string {
	n := t.Length
	if n <= 0 {
		n = def
	}
	return fmt.Sprintf("%s(%d)", name, n)
}

func decimalSpec(name string, t LogicalType) string {
	if t.Precision <= 0 {
		return name
	}
	if t.Scale <= 0 {
		return fmt.Sprintf("%s(%d)", name, t.Precision)
	}
	return fmt.Sprintf("%s(%d,%d)", name, t.Precision, t.Scale)
}

func parseTwoInts(params string) (int, int) {
	if params == "" {
		return 0, 0
	}
	parts := strings.SplitN(params, ",", 2)
	a, _ := strconv.Atoi(strings.TrimSpace(parts[0]))
	b := 0
	if len(parts) == 2 {
		b, _ = strconv.Atoi(strings.TrimSpace(parts[1]))
	}
	return a, b
}

// splitTopLevel은 따옴표 안의 콤마를 무시하고 콤마로 분리한다.
func splitTopLevel(s string) []string {
	out := []string{}
	var cur strings.Builder
	var quote rune
	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			}
			cur.WriteRune(r)
		case r == '\'' || r == '"':
			quote = r
			cur.WriteRune(r)
		case r == ',':
			out = append(out, cur.String())
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

func quoteList(values []string, q string) string {
	parts := make([]string, len(values))
	for i, v := range values {
		parts[i] = q + strings.ReplaceAll(v, q, q+q) + q
	}
	return strings.Join(parts, ", ")
}

var wsRE = regexp.MustCompile(`\s+`)

// normalizeExpr는 체크 제약식이나 기본값 표현식을 비교 가능한 형태로 다듬는다.
// DB가 돌려주는 표현식은 공백, 괄호, 대소문자, 타입 캐스트가 제각각이라
// 그대로 비교하면 바뀌지 않은 제약이 계속 변경으로 잡힌다.
func normalizeExpr(expr string) string {
	s := strings.TrimSpace(strings.ToLower(expr))
	s = wsRE.ReplaceAllString(s, " ")
	// PostgreSQL이 붙이는 타입 캐스트 제거: 'x'::text → 'x'
	s = regexp.MustCompile(`::[a-z0-9_ ]+(\[\])?`).ReplaceAllString(s, "")
	// 전체를 감싼 중복 괄호 제거
	for strings.HasPrefix(s, "((") && strings.HasSuffix(s, "))") {
		s = strings.TrimSpace(s[1 : len(s)-1])
	}
	s = strings.ReplaceAll(s, "( ", "(")
	s = strings.ReplaceAll(s, " )", ")")
	return s
}

// NormalizeDefault는 기본값 표현식을 dialect 차이를 흡수해 비교 가능하게 만든다.
func NormalizeDefault(dialect, def string) string {
	s := normalizeExpr(def)
	s = strings.TrimSuffix(s, "()")
	switch s {
	case "current_timestamp", "now", "getdate", "sysdate", "systimestamp", "current_timestamp(6)":
		return "current_timestamp"
	case "true", "1", "b'1'":
		// 불리언 기본값은 dialect마다 표기가 다르지만 컬럼 타입이 bool일 때만 의미가 같다.
		// 타입 정보 없이 판단하면 정수 기본값 1과 혼동되므로 원본을 유지한다.
		return s
	case "null":
		return ""
	}
	return s
}

func hashString(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:16])
}
