package schema

import "strings"

// 타입 카탈로그: "이 DB에서 고를 수 있는 타입은 무엇이고, 각각 어떤 인자를 받는가".
//
// 왜 서버에 두는가: 화면이 타입 목록을 들고 있으면 DB 종류가 늘어날 때마다 두 곳을
// 고쳐야 하고, 두 목록이 어긋나면 화면에서 고른 타입을 서버가 모르는 상태가 된다.
// 파싱(ParseType)과 렌더링(RenderType)이 이미 여기 있으므로 고르는 목록도 같은 자리에
// 있는 것이 맞다.
//
// 이 목록은 "쓸 수 있는 타입"이지 "권장 타입"이 아니다. 오래된 타입(MS-SQL의 TEXT,
// Oracle의 LONG)도 남겨 둔다 — 기존 DB를 ERD로 옮겨 그릴 때 그 타입이 실제로 있기
// 때문이다. 대신 Note에 왜 피해야 하는지 적는다.

// 타입이 받는 인자의 모양.
const (
	// ParamNone은 인자가 없는 타입이다(BOOLEAN, DATE).
	ParamNone = ""
	// ParamLength는 길이 하나다(VARCHAR(255)).
	ParamLength = "length"
	// ParamPrecision은 전체 자릿수와 소수 자릿수다(DECIMAL(10,2)).
	ParamPrecision = "precision"
	// ParamValues는 값 목록이다(ENUM('a','b')).
	ParamValues = "values"
	// ParamFraction은 초의 소수 자릿수다(TIMESTAMP(6)).
	ParamFraction = "fraction"
)

// 타입 분류. 화면에서 묶어 보여주는 단위이며, 사람이 타입을 찾는 순서에 맞춘다.
const (
	CatNumber = "숫자"
	CatText   = "문자"
	CatTime   = "날짜·시간"
	CatBinary = "이진"
	CatStruct = "구조·문서"
	CatSpace  = "공간"
	CatOther  = "기타"
)

// TypeDef는 고를 수 있는 타입 하나다.
type TypeDef struct {
	Name     string `json:"name"`  // DDL에 쓰는 이름
	Label    string `json:"label"` // 화면에 보여줄 이름
	Category string `json:"category"`
	// Param은 이 타입이 받는 인자의 모양이다(위 상수).
	Param string `json:"param,omitempty"`
	// Default는 인자의 기본 제안값이다("255", "10,2", "6").
	Default string `json:"default,omitempty"`
	// Max는 길이 상한이다. 0이면 상한을 모르거나 없다.
	Max int `json:"max,omitempty"`
	// Unsigned가 참이면 UNSIGNED를 붙일 수 있다(MySQL 숫자형).
	Unsigned bool `json:"unsigned,omitempty"`
	// Identity가 참이면 이 타입에 자동 증가를 붙일 수 있다.
	//
	// 타입마다 표시하는 이유: 자동 증가는 "정수 계열에만"이라는 규칙이 DB마다 조금씩
	// 다르다(MS-SQL은 소수 자릿수 0인 DECIMAL에도 붙고, SQLite는 INTEGER 하나뿐이다).
	// 화면이 이름을 보고 짐작하면 그 예외를 매번 다시 구현하게 된다.
	Identity bool `json:"identity,omitempty"`
	// Note는 고를 때 알아야 할 한 줄이다(권장·주의).
	Note string `json:"note,omitempty"`
}

// DefaultSuggestion은 기본값 칸에서 제안하는 식이다.
//
// 서버가 주는 이유: 함수 이름이 DB마다 다르다(now() / GETDATE() / SYSTIMESTAMP).
// 화면이 짐작하면 그 차이를 화면에서 다시 구현하게 되고, DB를 하나 더 지원할 때마다
// 두 곳을 고쳐야 한다.
type DefaultSuggestion struct {
	// Expr은 그대로 기본값 칸에 들어갈 식이다.
	Expr string `json:"expr"`
	// Label은 그 식이 무엇인지 한 줄로 적은 것이다.
	Label string `json:"label"`
	// For가 비어 있으면 어떤 타입에서든 제안한다. 채워져 있으면 그 분류에서만
	// 제안한다 — 문자 컬럼에 now()를 먼저 보여줄 이유는 없다.
	For []string `json:"for,omitempty"`
}

// Catalog는 한 dialect의 타입 목록과 그 DB에서만 되는 것들이다.
type Catalog struct {
	Dialect string    `json:"dialect"`
	Types   []TypeDef `json:"types"`
	// Defaults는 기본값 칸의 제안 목록이다(자동 완성).
	Defaults []DefaultSuggestion `json:"defaults,omitempty"`
	// Arrays가 참이면 타입 뒤에 []를 붙여 배열을 만들 수 있다(PostgreSQL).
	Arrays bool `json:"arrays"`
	// AutoIncrement는 이 DB에서 자동 증가를 부르는 이름이다. 화면의 설명에 쓴다.
	AutoIncrement string `json:"autoIncrement,omitempty"`
	// AutoIncrementNote는 자동 증가에 걸린 제약이다(SQLite처럼 조건이 있는 경우).
	AutoIncrementNote string `json:"autoIncrementNote,omitempty"`
}

// 기본값 제안 목록.
//
// 타입에 어울리는 것만 주지 않고 그 DB에서 쓸 수 있는 것을 모두 담는 이유: 기본값은
// 타입만으로 정해지지 않는다. 문자 컬럼에 시각을 문자열로 넣기도 하고, 숫자 컬럼에
// 시퀀스를 물리기도 한다. 화면은 어울리는 것을 먼저 보여주되 나머지도 찾을 수 있게 한다.
//
// 인자가 있는 함수는 **예시 인자까지** 적어 둔다. 이름만 주면 괄호 안에 무엇을 넣어야
// 하는지가 다시 숙제가 되고, 그 답은 대개 이 앱 밖(문서)에 있다.
var (
	// 값 리터럴. 어느 DB에서나 같다.
	literalDefaults = []DefaultSuggestion{
		{Expr: "0", Label: "0", For: []string{CatNumber}},
		{Expr: "1", Label: "1", For: []string{CatNumber}},
		{Expr: "''", Label: "빈 문자열", For: []string{CatText}},
		{Expr: "TRUE", Label: "참", For: []string{CatOther, CatNumber}},
		{Expr: "FALSE", Label: "거짓", For: []string{CatOther, CatNumber}},
	}

	postgresDefaults = append(append([]DefaultSuggestion{}, literalDefaults...), []DefaultSuggestion{
		{Expr: "now()", Label: "지금 시각 (트랜잭션 시작 시각)", For: []string{CatTime}},
		{Expr: "CURRENT_TIMESTAMP", Label: "지금 시각 (표준 문법)", For: []string{CatTime}},
		{Expr: "CURRENT_DATE", Label: "오늘 날짜", For: []string{CatTime}},
		{Expr: "CURRENT_TIME", Label: "지금 시각(시간만)", For: []string{CatTime}},
		{Expr: "LOCALTIMESTAMP", Label: "지금 시각 (타임존 없이)", For: []string{CatTime}},
		{Expr: "clock_timestamp()", Label: "실제 지금 시각 (문장마다 다시 읽음)", For: []string{CatTime}},
		{Expr: "statement_timestamp()", Label: "이 문장이 시작된 시각", For: []string{CatTime}},
		{Expr: "date_trunc('day', now())", Label: "오늘 0시", For: []string{CatTime}},
		{Expr: "now() + interval '7 days'", Label: "7일 뒤", For: []string{CatTime}},
		{Expr: "gen_random_uuid()", Label: "무작위 UUID (13+ 기본 제공)", For: []string{CatOther, CatText}},
		{Expr: "uuid_generate_v4()", Label: "무작위 UUID (uuid-ossp 확장)", For: []string{CatOther, CatText}},
		{Expr: "nextval('시퀀스이름')", Label: "시퀀스 다음 값", For: []string{CatNumber}},
		{Expr: "'{}'::jsonb", Label: "빈 JSON 객체", For: []string{CatStruct}},
		{Expr: "'[]'::jsonb", Label: "빈 JSON 배열", For: []string{CatStruct}},
		{Expr: "'{}'::text[]", Label: "빈 배열", For: []string{CatText}},
		{Expr: "CURRENT_USER", Label: "지금 접속한 사용자", For: []string{CatText}},
		{Expr: "md5(random()::text)", Label: "무작위 문자열", For: []string{CatText}},
		{Expr: "floor(random() * 100)::int", Label: "무작위 정수 (0~99)", For: []string{CatNumber}},
	}...)

	// MySQL 8.0.13 미만에서는 식을 기본값으로 쓸 수 없고, 그 이상에서도 괄호가 필요하다
	// (CURRENT_TIMESTAMP 계열만 예외다). 그래서 식에는 괄호를 붙여 둔다.
	mysqlDefaults = append(append([]DefaultSuggestion{}, literalDefaults...), []DefaultSuggestion{
		{Expr: "CURRENT_TIMESTAMP", Label: "지금 시각", For: []string{CatTime}},
		{Expr: "CURRENT_TIMESTAMP(6)", Label: "지금 시각 (마이크로초까지)", For: []string{CatTime}},
		{Expr: "CURRENT_DATE", Label: "오늘 날짜", For: []string{CatTime}},
		{Expr: "CURRENT_TIME", Label: "지금 시각(시간만)", For: []string{CatTime}},
		{Expr: "(NOW())", Label: "지금 시각 (함수 형태, 괄호 필요)", For: []string{CatTime}},
		{Expr: "(UTC_TIMESTAMP())", Label: "지금 시각 (UTC)", For: []string{CatTime}},
		{Expr: "(CURRENT_TIMESTAMP + INTERVAL 7 DAY)", Label: "7일 뒤", For: []string{CatTime}},
		{Expr: "(UUID())", Label: "무작위 UUID (8.0.13+)", For: []string{CatText}},
		{Expr: "(UUID_TO_BIN(UUID()))", Label: "무작위 UUID (16바이트 이진)", For: []string{CatBinary}},
		{Expr: "(JSON_OBJECT())", Label: "빈 JSON 객체 (8.0.13+)", For: []string{CatStruct}},
		{Expr: "(JSON_ARRAY())", Label: "빈 JSON 배열 (8.0.13+)", For: []string{CatStruct}},
		{Expr: "(CURRENT_USER())", Label: "지금 접속한 사용자", For: []string{CatText}},
		{Expr: "(FLOOR(RAND() * 100))", Label: "무작위 정수 (0~99)", For: []string{CatNumber}},
	}...)

	mssqlDefaults = append(append([]DefaultSuggestion{}, literalDefaults...), []DefaultSuggestion{
		{Expr: "SYSDATETIME()", Label: "지금 시각 (정밀)", For: []string{CatTime}},
		{Expr: "SYSUTCDATETIME()", Label: "지금 시각 (UTC)", For: []string{CatTime}},
		{Expr: "SYSDATETIMEOFFSET()", Label: "지금 시각 (타임존 포함)", For: []string{CatTime}},
		{Expr: "GETDATE()", Label: "지금 시각 (예전 방식)", For: []string{CatTime}},
		{Expr: "GETUTCDATE()", Label: "지금 시각 UTC (예전 방식)", For: []string{CatTime}},
		{Expr: "CONVERT(date, GETDATE())", Label: "오늘 날짜", For: []string{CatTime}},
		{Expr: "DATEADD(day, 7, SYSDATETIME())", Label: "7일 뒤", For: []string{CatTime}},
		{Expr: "NEWID()", Label: "무작위 UUID", For: []string{CatOther}},
		{Expr: "NEWSEQUENTIALID()", Label: "순차 UUID (인덱스 조각화가 적다)", For: []string{CatOther}},
		{Expr: "NEXT VALUE FOR 시퀀스이름", Label: "시퀀스 다음 값", For: []string{CatNumber}},
		{Expr: "CAST(0 AS BIT)", Label: "거짓 (BIT)", For: []string{CatNumber}},
		{Expr: "SUSER_SNAME()", Label: "지금 접속한 로그인 이름", For: []string{CatText}},
		{Expr: "'{}'", Label: "빈 JSON 객체 (문자열로 저장)", For: []string{CatText}},
	}...)

	oracleDefaults = append(append([]DefaultSuggestion{}, literalDefaults...), []DefaultSuggestion{
		{Expr: "SYSTIMESTAMP", Label: "지금 시각 (타임존 포함)", For: []string{CatTime}},
		{Expr: "SYSDATE", Label: "지금 시각", For: []string{CatTime}},
		{Expr: "CURRENT_TIMESTAMP", Label: "지금 시각 (세션 타임존)", For: []string{CatTime}},
		{Expr: "CURRENT_DATE", Label: "오늘 날짜 (세션 타임존)", For: []string{CatTime}},
		{Expr: "LOCALTIMESTAMP", Label: "지금 시각 (타임존 없이)", For: []string{CatTime}},
		{Expr: "TRUNC(SYSDATE)", Label: "오늘 0시", For: []string{CatTime}},
		{Expr: "SYSDATE + 7", Label: "7일 뒤", For: []string{CatTime}},
		{Expr: "SYS_GUID()", Label: "무작위 GUID (16바이트)", For: []string{CatBinary, CatText}},
		{Expr: "시퀀스이름.NEXTVAL", Label: "시퀀스 다음 값", For: []string{CatNumber}},
		{Expr: "USER", Label: "지금 접속한 사용자", For: []string{CatText}},
		{Expr: "TO_CHAR(SYSDATE, 'YYYY-MM-DD')", Label: "오늘 날짜 (문자열)", For: []string{CatText}},
	}...)

	sqliteDefaults = append(append([]DefaultSuggestion{}, literalDefaults...), []DefaultSuggestion{
		{Expr: "CURRENT_TIMESTAMP", Label: "지금 시각 (UTC, 'YYYY-MM-DD HH:MM:SS')", For: []string{CatTime, CatText}},
		{Expr: "CURRENT_DATE", Label: "오늘 날짜 (UTC)", For: []string{CatTime, CatText}},
		{Expr: "CURRENT_TIME", Label: "지금 시각(시간만, UTC)", For: []string{CatTime, CatText}},
		{Expr: "(datetime('now'))", Label: "지금 시각 (UTC)", For: []string{CatTime, CatText}},
		{Expr: "(datetime('now','localtime'))", Label: "지금 시각 (현지)", For: []string{CatTime, CatText}},
		{Expr: "(date('now'))", Label: "오늘 날짜", For: []string{CatTime, CatText}},
		{Expr: "(strftime('%Y-%m-%dT%H:%M:%fZ','now'))", Label: "지금 시각 (ISO 8601, 밀리초)", For: []string{CatText}},
		{Expr: "(unixepoch())", Label: "지금 시각 (유닉스 초, 3.38+)", For: []string{CatNumber}},
		{Expr: "(strftime('%s','now'))", Label: "지금 시각 (유닉스 초, 예전 방식)", For: []string{CatText, CatNumber}},
		{Expr: "(hex(randomblob(16)))", Label: "무작위 16바이트 (UUID 대용)", For: []string{CatText}},
		{Expr: "(abs(random() % 100))", Label: "무작위 정수 (0~99)", For: []string{CatNumber}},
		{Expr: "('{}')", Label: "빈 JSON 객체 (문자열로 저장)", For: []string{CatText, CatStruct}},
	}...)
)

// TypeCatalog는 dialect의 카탈로그를 돌려준다. 모르는 dialect면 공통 타입만 준다.
func TypeCatalog(dialect string) Catalog {
	switch strings.ToLower(strings.TrimSpace(dialect)) {
	case "postgres":
		return Catalog{
			Dialect: "postgres", Types: postgresTypes, Arrays: true,
			Defaults:          postgresDefaults,
			AutoIncrement:     "GENERATED BY DEFAULT AS IDENTITY",
			AutoIncrementNote: "정수 계열에만 붙일 수 있습니다. 예전 방식인 serial 타입을 골라도 같은 뜻이 됩니다.",
		}
	case "mysql", "mariadb":
		return Catalog{
			Dialect: "mysql", Types: mysqlTypes,
			Defaults:          mysqlDefaults,
			AutoIncrement:     "AUTO_INCREMENT",
			AutoIncrementNote: "정수 계열에만 붙일 수 있고, 그 컬럼은 키(대개 기본키)여야 합니다.",
		}
	case "mssql", "sqlserver":
		return Catalog{
			Dialect: "mssql", Types: mssqlTypes,
			Defaults:          mssqlDefaults,
			AutoIncrement:     "IDENTITY(1,1)",
			AutoIncrementNote: "정수·decimal(스케일 0) 계열에만 붙일 수 있습니다.",
		}
	case "oracle":
		return Catalog{
			Dialect: "oracle", Types: oracleTypes,
			Defaults:          oracleDefaults,
			AutoIncrement:     "GENERATED BY DEFAULT AS IDENTITY",
			AutoIncrementNote: "12c 이상에서 씁니다. 그 이전 버전은 시퀀스와 트리거로 만들어야 합니다.",
		}
	case "sqlite":
		return Catalog{
			Dialect: "sqlite", Types: sqliteTypes,
			Defaults:      sqliteDefaults,
			AutoIncrement: "AUTOINCREMENT",
			AutoIncrementNote: "INTEGER PRIMARY KEY 컬럼에만 붙일 수 있습니다. " +
				"그 조건이면 AUTOINCREMENT를 적지 않아도 rowid가 자동으로 채워집니다.",
		}
	}
	return Catalog{Dialect: dialect, Types: commonCatalog}
}

// commonCatalog는 dialect를 모를 때 쓰는 최소 목록이다.
var commonCatalog = []TypeDef{
	{Name: "INTEGER", Label: "정수", Category: CatNumber, Identity: true},
	{Name: "BIGINT", Label: "큰 정수", Category: CatNumber, Identity: true},
	{Name: "DECIMAL", Label: "고정 소수", Category: CatNumber, Param: ParamPrecision, Default: "10,2"},
	{Name: "VARCHAR", Label: "가변 문자열", Category: CatText, Param: ParamLength, Default: "255"},
	{Name: "TEXT", Label: "긴 문자열", Category: CatText},
	{Name: "BOOLEAN", Label: "참/거짓", Category: CatOther},
	{Name: "DATE", Label: "날짜", Category: CatTime},
	{Name: "TIMESTAMP", Label: "날짜+시각", Category: CatTime},
}

var postgresTypes = []TypeDef{
	{Name: "SMALLINT", Label: "작은 정수 (2바이트)", Category: CatNumber, Identity: true},
	{Name: "INTEGER", Label: "정수 (4바이트)", Category: CatNumber, Identity: true},
	{Name: "BIGINT", Label: "큰 정수 (8바이트)", Category: CatNumber, Identity: true},
	{Name: "NUMERIC", Label: "고정 소수", Category: CatNumber, Param: ParamPrecision, Default: "10,2",
		Note: "돈처럼 반올림 오차가 있으면 안 되는 값에 씁니다."},
	{Name: "REAL", Label: "실수 (4바이트)", Category: CatNumber},
	{Name: "DOUBLE PRECISION", Label: "실수 (8바이트)", Category: CatNumber},
	{Name: "MONEY", Label: "통화", Category: CatNumber,
		Note: "소수 자릿수가 서버 설정(lc_monetary)에 달려 있어 이식성이 낮습니다."},
	{Name: "SMALLSERIAL", Label: "자동 증가 (작은 정수)", Category: CatNumber,
		Note: "예전 방식입니다. 새 설계에는 정수 + 자동 증가를 권합니다."},
	{Name: "SERIAL", Label: "자동 증가 (정수)", Category: CatNumber,
		Note: "예전 방식입니다. 새 설계에는 정수 + 자동 증가를 권합니다."},
	{Name: "BIGSERIAL", Label: "자동 증가 (큰 정수)", Category: CatNumber,
		Note: "예전 방식입니다. 새 설계에는 큰 정수 + 자동 증가를 권합니다."},

	{Name: "VARCHAR", Label: "가변 문자열", Category: CatText, Param: ParamLength, Default: "255", Max: 10485760},
	{Name: "CHAR", Label: "고정 길이 문자열", Category: CatText, Param: ParamLength, Default: "1", Max: 10485760,
		Note: "짧아도 길이만큼 공백으로 채웁니다."},
	{Name: "TEXT", Label: "긴 문자열", Category: CatText,
		Note: "PostgreSQL에서는 VARCHAR와 성능 차이가 없습니다. 길이 제한이 필요 없으면 이쪽이 낫습니다."},
	{Name: "CITEXT", Label: "대소문자 무시 문자열", Category: CatText,
		Note: "citext 확장이 설치되어 있어야 합니다."},
	{Name: "NAME", Label: "식별자 문자열 (63바이트)", Category: CatText, Note: "시스템 카탈로그용 타입입니다."},

	{Name: "DATE", Label: "날짜", Category: CatTime},
	{Name: "TIME", Label: "시각", Category: CatTime, Param: ParamFraction, Default: "6"},
	{Name: "TIME WITH TIME ZONE", Label: "시각 (타임존 포함)", Category: CatTime, Param: ParamFraction, Default: "6"},
	{Name: "TIMESTAMP", Label: "날짜+시각", Category: CatTime, Param: ParamFraction, Default: "6"},
	{Name: "TIMESTAMPTZ", Label: "날짜+시각 (타임존 포함)", Category: CatTime, Param: ParamFraction, Default: "6",
		Note: "여러 지역에서 쓰는 값이면 이쪽을 권합니다. 저장은 UTC로 됩니다."},
	{Name: "INTERVAL", Label: "기간", Category: CatTime},

	{Name: "BYTEA", Label: "이진 데이터", Category: CatBinary},
	{Name: "BIT", Label: "비트열 (고정)", Category: CatBinary, Param: ParamLength, Default: "1"},
	{Name: "BIT VARYING", Label: "비트열 (가변)", Category: CatBinary, Param: ParamLength, Default: "8"},

	{Name: "JSONB", Label: "JSON (이진, 색인 가능)", Category: CatStruct,
		Note: "새 설계에는 JSON보다 JSONB를 권합니다 — 색인이 되고 조회가 빠릅니다."},
	{Name: "JSON", Label: "JSON (원문 보존)", Category: CatStruct},
	{Name: "XML", Label: "XML", Category: CatStruct},
	{Name: "HSTORE", Label: "키-값 쌍", Category: CatStruct, Note: "hstore 확장이 필요합니다."},
	{Name: "TSVECTOR", Label: "전문 검색 벡터", Category: CatStruct},

	{Name: "POINT", Label: "점", Category: CatSpace},
	{Name: "LINE", Label: "직선", Category: CatSpace},
	{Name: "LSEG", Label: "선분", Category: CatSpace},
	{Name: "BOX", Label: "사각형", Category: CatSpace},
	{Name: "PATH", Label: "경로", Category: CatSpace},
	{Name: "POLYGON", Label: "다각형", Category: CatSpace},
	{Name: "CIRCLE", Label: "원", Category: CatSpace},
	{Name: "GEOMETRY", Label: "지오메트리 (PostGIS)", Category: CatSpace, Note: "postgis 확장이 필요합니다."},
	{Name: "GEOGRAPHY", Label: "지리 좌표 (PostGIS)", Category: CatSpace, Note: "postgis 확장이 필요합니다."},

	{Name: "BOOLEAN", Label: "참/거짓", Category: CatOther},
	{Name: "UUID", Label: "UUID", Category: CatOther},
	{Name: "INET", Label: "IP 주소", Category: CatOther},
	{Name: "CIDR", Label: "IP 대역", Category: CatOther},
	{Name: "MACADDR", Label: "MAC 주소", Category: CatOther},
	{Name: "INT4RANGE", Label: "정수 범위", Category: CatOther},
	{Name: "NUMRANGE", Label: "수 범위", Category: CatOther},
	{Name: "TSRANGE", Label: "시각 범위", Category: CatOther},
	{Name: "TSTZRANGE", Label: "시각 범위 (타임존 포함)", Category: CatOther},
	{Name: "DATERANGE", Label: "날짜 범위", Category: CatOther},
}

var mysqlTypes = []TypeDef{
	{Name: "TINYINT", Label: "아주 작은 정수 (1바이트)", Category: CatNumber, Unsigned: true, Identity: true},
	{Name: "SMALLINT", Label: "작은 정수 (2바이트)", Category: CatNumber, Unsigned: true, Identity: true},
	{Name: "MEDIUMINT", Label: "중간 정수 (3바이트)", Category: CatNumber, Unsigned: true, Identity: true},
	{Name: "INT", Label: "정수 (4바이트)", Category: CatNumber, Unsigned: true, Identity: true},
	{Name: "BIGINT", Label: "큰 정수 (8바이트)", Category: CatNumber, Unsigned: true, Identity: true},
	{Name: "DECIMAL", Label: "고정 소수", Category: CatNumber, Param: ParamPrecision, Default: "10,2", Unsigned: true,
		Note: "돈처럼 반올림 오차가 있으면 안 되는 값에 씁니다."},
	{Name: "FLOAT", Label: "실수 (4바이트)", Category: CatNumber, Unsigned: true},
	{Name: "DOUBLE", Label: "실수 (8바이트)", Category: CatNumber, Unsigned: true},
	{Name: "BIT", Label: "비트열", Category: CatNumber, Param: ParamLength, Default: "1", Max: 64},

	{Name: "VARCHAR", Label: "가변 문자열", Category: CatText, Param: ParamLength, Default: "255", Max: 65535,
		Note: "행 전체가 65,535바이트를 넘을 수 없어, 긴 문자열은 TEXT가 낫습니다."},
	{Name: "CHAR", Label: "고정 길이 문자열", Category: CatText, Param: ParamLength, Default: "1", Max: 255},
	{Name: "TINYTEXT", Label: "긴 문자열 (255바이트)", Category: CatText},
	{Name: "TEXT", Label: "긴 문자열 (64KB)", Category: CatText},
	{Name: "MEDIUMTEXT", Label: "긴 문자열 (16MB)", Category: CatText},
	{Name: "LONGTEXT", Label: "긴 문자열 (4GB)", Category: CatText},
	{Name: "ENUM", Label: "정해진 값 중 하나", Category: CatText, Param: ParamValues, Default: "a,b",
		Note: "값을 추가하려면 테이블을 고쳐야 합니다. 자주 바뀌는 목록이면 별도 표를 권합니다."},
	{Name: "SET", Label: "정해진 값의 조합", Category: CatText, Param: ParamValues, Default: "a,b"},

	{Name: "DATE", Label: "날짜", Category: CatTime},
	{Name: "TIME", Label: "시각", Category: CatTime, Param: ParamFraction, Default: "0", Max: 6},
	{Name: "DATETIME", Label: "날짜+시각", Category: CatTime, Param: ParamFraction, Default: "0", Max: 6,
		Note: "타임존을 저장하지 않습니다. 값을 넣은 그대로 돌려줍니다."},
	{Name: "TIMESTAMP", Label: "날짜+시각 (UTC 변환)", Category: CatTime, Param: ParamFraction, Default: "0", Max: 6,
		Note: "세션 타임존으로 변환해 저장합니다. 2038년 상한이 있습니다."},
	{Name: "YEAR", Label: "연도", Category: CatTime},

	{Name: "BINARY", Label: "고정 길이 이진", Category: CatBinary, Param: ParamLength, Default: "16", Max: 255},
	{Name: "VARBINARY", Label: "가변 길이 이진", Category: CatBinary, Param: ParamLength, Default: "255", Max: 65535},
	{Name: "TINYBLOB", Label: "이진 (255바이트)", Category: CatBinary},
	{Name: "BLOB", Label: "이진 (64KB)", Category: CatBinary},
	{Name: "MEDIUMBLOB", Label: "이진 (16MB)", Category: CatBinary},
	{Name: "LONGBLOB", Label: "이진 (4GB)", Category: CatBinary},

	{Name: "JSON", Label: "JSON", Category: CatStruct,
		Note: "MySQL 5.7 이상. 색인하려면 생성 컬럼을 함께 만들어야 합니다."},

	{Name: "GEOMETRY", Label: "지오메트리", Category: CatSpace},
	{Name: "POINT", Label: "점", Category: CatSpace},
	{Name: "LINESTRING", Label: "선", Category: CatSpace},
	{Name: "POLYGON", Label: "다각형", Category: CatSpace},
	{Name: "MULTIPOINT", Label: "점 집합", Category: CatSpace},
	{Name: "MULTILINESTRING", Label: "선 집합", Category: CatSpace},
	{Name: "MULTIPOLYGON", Label: "다각형 집합", Category: CatSpace},
	{Name: "GEOMETRYCOLLECTION", Label: "도형 집합", Category: CatSpace},

	{Name: "BOOLEAN", Label: "참/거짓", Category: CatOther,
		Note: "실제로는 TINYINT(1)입니다. 0이 거짓, 그 외가 참입니다."},
}

var mssqlTypes = []TypeDef{
	{Name: "TINYINT", Label: "아주 작은 정수 (0~255)", Category: CatNumber, Identity: true},
	{Name: "SMALLINT", Label: "작은 정수 (2바이트)", Category: CatNumber, Identity: true},
	{Name: "INT", Label: "정수 (4바이트)", Category: CatNumber, Identity: true},
	{Name: "BIGINT", Label: "큰 정수 (8바이트)", Category: CatNumber, Identity: true},
	{Name: "DECIMAL", Label: "고정 소수", Category: CatNumber, Param: ParamPrecision, Default: "10,2", Identity: true},
	{Name: "NUMERIC", Label: "고정 소수 (DECIMAL과 같음)", Category: CatNumber, Param: ParamPrecision, Default: "10,2", Identity: true},
	{Name: "FLOAT", Label: "실수", Category: CatNumber, Param: ParamLength, Default: "53", Max: 53},
	{Name: "REAL", Label: "실수 (4바이트)", Category: CatNumber},
	{Name: "MONEY", Label: "통화", Category: CatNumber},
	{Name: "SMALLMONEY", Label: "통화 (작은 범위)", Category: CatNumber},
	{Name: "BIT", Label: "참/거짓", Category: CatNumber, Note: "0, 1, NULL만 담습니다."},

	{Name: "NVARCHAR", Label: "가변 유니코드 문자열", Category: CatText, Param: ParamLength, Default: "255", Max: 4000,
		Note: "한글을 담는다면 N 계열(NVARCHAR/NCHAR)을 권합니다."},
	{Name: "NVARCHAR(MAX)", Label: "긴 유니코드 문자열", Category: CatText},
	{Name: "VARCHAR", Label: "가변 문자열", Category: CatText, Param: ParamLength, Default: "255", Max: 8000},
	{Name: "VARCHAR(MAX)", Label: "긴 문자열", Category: CatText},
	{Name: "NCHAR", Label: "고정 길이 유니코드 문자열", Category: CatText, Param: ParamLength, Default: "1", Max: 4000},
	{Name: "CHAR", Label: "고정 길이 문자열", Category: CatText, Param: ParamLength, Default: "1", Max: 8000},
	{Name: "TEXT", Label: "긴 문자열 (옛 타입)", Category: CatText,
		Note: "더 이상 권장되지 않습니다. VARCHAR(MAX)를 쓰세요."},
	{Name: "NTEXT", Label: "긴 유니코드 문자열 (옛 타입)", Category: CatText,
		Note: "더 이상 권장되지 않습니다. NVARCHAR(MAX)를 쓰세요."},

	{Name: "DATE", Label: "날짜", Category: CatTime},
	{Name: "TIME", Label: "시각", Category: CatTime, Param: ParamFraction, Default: "7", Max: 7},
	{Name: "DATETIME2", Label: "날짜+시각", Category: CatTime, Param: ParamFraction, Default: "7", Max: 7,
		Note: "새 설계에는 DATETIME 대신 이쪽을 권합니다 — 범위와 정밀도가 넓습니다."},
	{Name: "DATETIME", Label: "날짜+시각 (옛 타입)", Category: CatTime},
	{Name: "SMALLDATETIME", Label: "날짜+시각 (분 단위)", Category: CatTime},
	{Name: "DATETIMEOFFSET", Label: "날짜+시각 (타임존 포함)", Category: CatTime, Param: ParamFraction, Default: "7", Max: 7},

	{Name: "VARBINARY", Label: "가변 길이 이진", Category: CatBinary, Param: ParamLength, Default: "255", Max: 8000},
	{Name: "VARBINARY(MAX)", Label: "긴 이진", Category: CatBinary},
	{Name: "BINARY", Label: "고정 길이 이진", Category: CatBinary, Param: ParamLength, Default: "16", Max: 8000},
	{Name: "IMAGE", Label: "이진 (옛 타입)", Category: CatBinary,
		Note: "더 이상 권장되지 않습니다. VARBINARY(MAX)를 쓰세요."},

	{Name: "XML", Label: "XML", Category: CatStruct},

	{Name: "GEOGRAPHY", Label: "지리 좌표", Category: CatSpace},
	{Name: "GEOMETRY", Label: "지오메트리", Category: CatSpace},

	{Name: "UNIQUEIDENTIFIER", Label: "GUID", Category: CatOther},
	{Name: "ROWVERSION", Label: "행 버전", Category: CatOther, Note: "값을 직접 넣을 수 없습니다."},
	{Name: "HIERARCHYID", Label: "계층 경로", Category: CatOther},
	{Name: "SQL_VARIANT", Label: "임의 타입", Category: CatOther},
}

var oracleTypes = []TypeDef{
	{Name: "NUMBER", Label: "수", Category: CatNumber, Param: ParamPrecision, Default: "10,0", Identity: true,
		Note: "소수 자릿수를 0으로 두면 정수입니다. 자릿수를 비우면 임의 정밀도입니다."},
	{Name: "INTEGER", Label: "정수", Category: CatNumber, Note: "NUMBER(38)의 다른 이름입니다.", Identity: true},
	{Name: "FLOAT", Label: "실수", Category: CatNumber, Param: ParamLength, Default: "126"},
	{Name: "BINARY_FLOAT", Label: "실수 (4바이트)", Category: CatNumber},
	{Name: "BINARY_DOUBLE", Label: "실수 (8바이트)", Category: CatNumber},

	{Name: "VARCHAR2", Label: "가변 문자열", Category: CatText, Param: ParamLength, Default: "255", Max: 32767,
		Note: "Oracle에서는 VARCHAR가 아니라 VARCHAR2를 씁니다."},
	{Name: "NVARCHAR2", Label: "가변 유니코드 문자열", Category: CatText, Param: ParamLength, Default: "255", Max: 32767},
	{Name: "CHAR", Label: "고정 길이 문자열", Category: CatText, Param: ParamLength, Default: "1", Max: 2000},
	{Name: "NCHAR", Label: "고정 길이 유니코드 문자열", Category: CatText, Param: ParamLength, Default: "1", Max: 2000},
	{Name: "CLOB", Label: "긴 문자열", Category: CatText},
	{Name: "NCLOB", Label: "긴 유니코드 문자열", Category: CatText},
	{Name: "LONG", Label: "긴 문자열 (옛 타입)", Category: CatText,
		Note: "테이블당 하나만 쓸 수 있습니다. 새 설계에는 CLOB을 쓰세요."},

	{Name: "DATE", Label: "날짜+시각", Category: CatTime,
		Note: "Oracle의 DATE에는 시·분·초가 함께 들어갑니다."},
	{Name: "TIMESTAMP", Label: "날짜+시각 (정밀)", Category: CatTime, Param: ParamFraction, Default: "6", Max: 9},
	{Name: "TIMESTAMP WITH TIME ZONE", Label: "날짜+시각 (타임존 포함)", Category: CatTime, Param: ParamFraction, Default: "6", Max: 9},
	{Name: "TIMESTAMP WITH LOCAL TIME ZONE", Label: "날짜+시각 (세션 타임존)", Category: CatTime, Param: ParamFraction, Default: "6", Max: 9},
	{Name: "INTERVAL YEAR TO MONTH", Label: "기간 (연·월)", Category: CatTime},
	{Name: "INTERVAL DAY TO SECOND", Label: "기간 (일·시간)", Category: CatTime},

	{Name: "RAW", Label: "고정 길이 이진", Category: CatBinary, Param: ParamLength, Default: "16", Max: 2000},
	{Name: "BLOB", Label: "이진", Category: CatBinary},
	{Name: "BFILE", Label: "외부 파일 참조", Category: CatBinary},
	{Name: "LONG RAW", Label: "이진 (옛 타입)", Category: CatBinary, Note: "새 설계에는 BLOB을 쓰세요."},

	{Name: "JSON", Label: "JSON", Category: CatStruct, Note: "Oracle 21c 이상입니다. 그 이전에는 CLOB + 체크 제약을 씁니다."},
	{Name: "XMLTYPE", Label: "XML", Category: CatStruct},

	{Name: "SDO_GEOMETRY", Label: "지오메트리 (Spatial)", Category: CatSpace},

	{Name: "BOOLEAN", Label: "참/거짓", Category: CatOther,
		Note: "Oracle 23c 이상입니다. 그 이전에는 NUMBER(1) 또는 CHAR(1)로 표현합니다."},
	{Name: "ROWID", Label: "행 주소", Category: CatOther},
}

var sqliteTypes = []TypeDef{
	{Name: "INTEGER", Label: "정수", Category: CatNumber, Identity: true,
		Note: "기본키에 이 타입을 쓰면 rowid가 되어 자동 증가합니다."},
	{Name: "REAL", Label: "실수", Category: CatNumber},
	{Name: "NUMERIC", Label: "수 (친화성)", Category: CatNumber},
	{Name: "DECIMAL", Label: "고정 소수", Category: CatNumber, Param: ParamPrecision, Default: "10,2",
		Note: "SQLite는 자릿수를 강제하지 않습니다. 표기로만 남습니다."},
	{Name: "BOOLEAN", Label: "참/거짓", Category: CatOther, Note: "0과 1로 저장됩니다."},

	{Name: "TEXT", Label: "문자열", Category: CatText},
	{Name: "VARCHAR", Label: "가변 문자열", Category: CatText, Param: ParamLength, Default: "255",
		Note: "길이는 강제되지 않습니다. 다른 DB로 옮길 때를 위한 표기입니다."},

	{Name: "DATE", Label: "날짜", Category: CatTime, Note: "문자열이나 숫자로 저장됩니다."},
	{Name: "DATETIME", Label: "날짜+시각", Category: CatTime, Note: "문자열이나 숫자로 저장됩니다."},

	{Name: "BLOB", Label: "이진", Category: CatBinary},

	{Name: "JSON", Label: "JSON", Category: CatStruct, Note: "TEXT로 저장되며 JSON 함수로 다룹니다."},
}
