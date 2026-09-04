package schema

import "strings"

// 표와 데이터베이스의 저장 설정(엔진·문자셋·정렬·테이블스페이스).
//
// 이 목록이 한 곳에 있어야 하는 이유: 같은 지식이 세 곳에 필요하다 — 화면이 고르개를
// 그릴 때, DDL 을 만들 때, 실제 DB 에서 읽은 값을 견줄 때. 세 곳에 흩어 두면 화면에는
// 있는데 DDL 에는 안 나가는 설정이 생기고, 그 어긋남은 마이그레이션을 실행하는
// 순간에야 드러난다.
//
// 방언마다 있는 것만 둔다. MySQL 의 ENGINE 은 PostgreSQL 에 없고, PostgreSQL 의
// TABLESPACE 는 SQLite 에 없다. 없는 칸을 회색으로 보여 주는 것보다 아예 없는 편이
// "이 DB 에서 정할 수 있는 것"을 정확히 말한다.

// OptionSpec은 설정 칸 하나다.
type OptionSpec struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Help  string `json:"help,omitempty"`
	// Kind는 화면이 그릴 입력칸이다: select | text.
	Kind string `json:"kind"`
	// Choices는 select 의 후보다. 목록에 없는 값도 적을 수 있어야 하는 칸(문자셋 등)은
	// text 로 둔다 — DB 마다 쓸 수 있는 값이 다르고, 우리가 아는 것이 전부가 아니다.
	Choices []string `json:"choices,omitempty"`
	// Placeholder는 자유 입력칸의 예시다.
	Placeholder string `json:"placeholder,omitempty"`
}

// TableOptionSpecs는 이 방언에서 **표마다** 정할 수 있는 설정이다.
func TableOptionSpecs(dialect string) []OptionSpec {
	switch strings.ToLower(strings.TrimSpace(dialect)) {
	case "mysql":
		return []OptionSpec{
			{
				Key: "engine", Label: "엔진", Kind: "select",
				Choices: []string{"InnoDB", "MyISAM", "MEMORY", "ARCHIVE", "CSV"},
				Help:    "InnoDB 만 외래키와 트랜잭션을 지원합니다.",
			},
			{
				Key: "charset", Label: "문자셋", Kind: "text", Placeholder: "utf8mb4",
				Help: "비워 두면 데이터베이스의 기본 문자셋을 따릅니다.",
			},
			{
				Key: "collation", Label: "정렬 규칙", Kind: "text",
				Placeholder: "utf8mb4_0900_ai_ci",
				Help:        "문자셋에 속한 규칙이어야 합니다. 비교와 정렬 순서를 정합니다.",
			},
		}
	case "clickhouse":
		// ClickHouse 의 표 설정은 다른 DB 의 그것과 무게가 다르다. 엔진과 정렬 키는
		// "성능 조정"이 아니라 **표가 무엇인가**를 정한다 — 정렬 키 없는 MergeTree
		// 표는 아예 만들어지지 않고, 엔진을 바꾸면 중복 처리 규칙이 달라진다.
		return []OptionSpec{
			{
				Key: "engine", Label: "엔진", Kind: "select",
				Choices: []string{
					"MergeTree", "ReplacingMergeTree", "SummingMergeTree",
					"AggregatingMergeTree", "CollapsingMergeTree", "Log", "Memory",
				},
				Help: "MergeTree 계열만 정렬 키·파티션·TTL 을 씁니다.",
			},
			{
				Key: "order_by", Label: "정렬 키 (ORDER BY)", Kind: "text",
				Placeholder: "기본키를 따릅니다",
				Help:        "이 순서로 정렬해 저장합니다. 조회 조건에 자주 쓰는 컬럼을 앞에 두세요.",
			},
			{
				Key: "partition_by", Label: "파티션 키", Kind: "text",
				Placeholder: "toYYYYMM(created_at)",
				Help:        "너무 잘게 나누면 파티션 수가 폭발해 오히려 느려집니다. 월 단위가 무난합니다.",
			},
			{
				Key: "ttl", Label: "보관 기간 (TTL)", Kind: "text",
				Placeholder: "created_at + INTERVAL 90 DAY",
				Help:        "이 조건을 넘긴 행은 병합 때 지워집니다.",
			},
		}
	case "postgres":
		return []OptionSpec{
			{
				Key: "tablespace", Label: "테이블스페이스", Kind: "text", Placeholder: "pg_default",
				Help: "이 표를 어느 저장 공간에 둘지. 비워 두면 데이터베이스의 기본값입니다.",
			},
		}
	case "oracle":
		return []OptionSpec{
			{
				Key: "tablespace", Label: "테이블스페이스", Kind: "text", Placeholder: "USERS",
				Help: "비워 두면 계정의 기본 테이블스페이스에 만들어집니다.",
			},
		}
	default:
		// MS-SQL 은 파일그룹이 있지만 표 단위로 정하는 일이 드물고, SQLite 에는 개념이
		// 없다. 정할 수 없는 것을 칸으로 두면 "여기서 정했는데 왜 안 되나"가 된다.
		return nil
	}
}

// DatabaseOptionSpecs는 **데이터베이스 전체**에 걸리는 설정이다.
//
// 표 설정과 나누는 이유: 이것은 CREATE DATABASE 에 붙는 값이고, 표는 비워 두면 이것을
// 물려받는다. 한 목록에 섞으면 "이 문자셋은 이 표의 것인가 DB 의 것인가"가 화면에서
// 사라진다.
func DatabaseOptionSpecs(dialect string) []OptionSpec {
	switch strings.ToLower(strings.TrimSpace(dialect)) {
	case "mysql":
		return []OptionSpec{
			{Key: "charset", Label: "기본 문자셋", Kind: "text", Placeholder: "utf8mb4"},
			{Key: "collation", Label: "기본 정렬 규칙", Kind: "text", Placeholder: "utf8mb4_0900_ai_ci"},
		}
	case "postgres":
		return []OptionSpec{
			{Key: "encoding", Label: "인코딩", Kind: "text", Placeholder: "UTF8"},
			{
				Key: "lc_collate", Label: "정렬 로캘(LC_COLLATE)", Kind: "text",
				Placeholder: "C", Help: "만들 때만 정할 수 있습니다. 나중에 바꾸려면 DB를 다시 만들어야 합니다.",
			},
			{Key: "lc_ctype", Label: "문자 분류 로캘(LC_CTYPE)", Kind: "text", Placeholder: "C"},
			{Key: "template", Label: "템플릿", Kind: "text", Placeholder: "template0"},
		}
	case "mssql":
		return []OptionSpec{
			{Key: "collation", Label: "정렬 규칙", Kind: "text", Placeholder: "Korean_Wansung_CI_AS"},
		}
	default:
		return nil
	}
}

// KnownOptionKeys는 이 방언에서 우리가 다루는 표 설정 열쇠다.
//
// 견줄 때 쓴다. 실제 DB 에서 읽어 온 값에는 우리가 모르는 열쇠도 들어 있고(통계,
// 샘플 수), 그것들까지 견주면 있지도 않은 변경이 잡힌다.
func KnownOptionKeys(dialect string) map[string]bool {
	out := map[string]bool{}
	for _, sp := range TableOptionSpecs(dialect) {
		out[sp.Key] = true
	}
	return out
}

// OptionLabel은 열쇠의 사람 말 이름이다(모르는 열쇠는 그대로 돌려준다).
func OptionLabel(dialect, key string) string {
	for _, sp := range TableOptionSpecs(dialect) {
		if sp.Key == key {
			return sp.Label
		}
	}
	for _, sp := range DatabaseOptionSpecs(dialect) {
		if sp.Key == key {
			return sp.Label
		}
	}
	return key
}
