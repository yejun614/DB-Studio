package dbx

import (
	"fmt"
	"strconv"
	"strings"

	"dbstudio/internal/model"
)

// SQL 방언 차이를 이 파일에 가둔다.
//
// 데이터 조회는 "SELECT * FROM t WHERE ... ORDER BY ... LIMIT ..."이라는 한 문장을
// 다섯 방언으로 쓰는 일이고, 차이는 네 가지뿐이다: 식별자 인용, 파라미터 표기,
// 페이지 절, 대소문자 무시 LIKE. 그 넷을 함수로 만들어 두면 쿼리 빌더는 방언을
// 신경 쓰지 않아도 된다. 반대로 빌더 안에 방언 분기가 흩어지면, 새 방언을 넣을 때
// 어디를 고쳐야 하는지 알 수 없게 된다.

// quoteIdent는 식별자를 방언에 맞게 인용한다.
//
// 인용은 선택이 아니라 필수다. 인용하지 않으면 "order"라는 이름의 테이블을 열 수
// 없고, 무엇보다 이름이 SQL로 해석될 여지가 남는다. 이름은 사용자가 고른 것이 아니라
// DB에서 읽어 온 것이지만, 그 목록도 결국 문자열이므로 같은 규칙으로 다룬다.
func quoteIdent(kind model.DBKind, name string) string {
	switch kind {
	case model.KindMySQL, model.KindClickHouse:
		return "`" + strings.ReplaceAll(name, "`", "``") + "`"
	case model.KindMSSQL:
		return "[" + strings.ReplaceAll(name, "]", "]]") + "]"
	default:
		// PostgreSQL · Oracle · SQLite는 모두 표준 큰따옴표를 쓴다.
		return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
	}
}

// qualify는 네임스페이스가 있으면 붙여서 인용한다.
func qualify(kind model.DBKind, ref TableRef) string {
	name := quoteIdent(kind, ref.Name)
	if ref.Namespace == "" {
		return name
	}
	return quoteIdent(kind, ref.Namespace) + "." + name
}

// placeholder는 n번째(1부터) 파라미터 표기를 만든다.
func placeholder(kind model.DBKind, n int) string {
	switch kind {
	case model.KindPostgres:
		return "$" + strconv.Itoa(n)
	case model.KindOracle:
		return ":" + strconv.Itoa(n)
	case model.KindMSSQL:
		return "@p" + strconv.Itoa(n)
	default:
		return "?"
	}
}

// paramBuilder는 파라미터 목록과 표기를 함께 관리한다.
// 번호를 손으로 세면 반드시 어긋나고, 어긋난 결과는 "값이 하나 밀린 UPDATE"다.
type paramBuilder struct {
	kind   model.DBKind
	values []any
}

func newParams(kind model.DBKind) *paramBuilder {
	return &paramBuilder{kind: kind, values: []any{}}
}

// Add는 값을 추가하고 그 자리표시자를 반환한다.
func (p *paramBuilder) Add(v any) string {
	p.values = append(p.values, v)
	return placeholder(p.kind, len(p.values))
}

func (p *paramBuilder) Values() []any { return p.values }

// pageClause는 페이지 절을 만든다. hasOrder는 호출부가 ORDER BY를 이미 붙였는지다.
//
// 정렬이 없으면 DB는 순서를 보장하지 않으므로 페이지를 넘길 때 같은 행이 다시 나오거나
// 건너뛰어질 수 있다. 그래서 호출부는 기본키로라도 정렬하려 하지만, 뷰나 기본키 없는
// 테이블에는 정렬할 것이 없다. 예전에는 그 경우를 에러로 막았는데, 그러면 Oracle·MS-SQL
// 에서만 뷰를 아예 열 수 없었다 — 순서가 흔들릴 수 있다는 것과 못 보는 것은 다른 문제이고,
// 흔들림은 결과에 note로 알리면 된다(호출부가 unstableOrderNote를 붙인다).
//
// 방언 차이: Oracle의 행 제한 절(12c+)은 ORDER BY가 없어도 되지만, MS-SQL의 OFFSET/FETCH는
// 문법상 ORDER BY를 반드시 요구한다. 그래서 MS-SQL에만 관용구인 ORDER BY (SELECT NULL)을
// 넣는다 — 실제로 정렬하지 않으면서 문법만 만족시킨다.
func pageClause(kind model.DBKind, p *paramBuilder, limit, offset int, hasOrder bool) string {
	switch kind {
	case model.KindMSSQL, model.KindOracle:
		prefix := ""
		if !hasOrder && kind == model.KindMSSQL {
			prefix = " ORDER BY (SELECT NULL)"
		}
		return prefix + fmt.Sprintf(" OFFSET %s ROWS FETCH NEXT %s ROWS ONLY",
			p.Add(offset), p.Add(limit))
	default:
		return fmt.Sprintf(" LIMIT %s OFFSET %s", p.Add(limit), p.Add(offset))
	}
}

// unstableOrderNote는 정렬 없이 페이지를 나눌 때 붙이는 경고다.
// 화면이 이것을 보여주지 않으면 사용자는 페이지를 넘기다 행이 중복·누락되는 것을
// 자기 데이터의 문제로 오해한다.
const unstableOrderNote = "정렬 기준이 없어 페이지 사이의 행 순서가 보장되지 않습니다. " +
	"컬럼을 하나 골라 정렬하면 안정적으로 넘길 수 있습니다."

// containsExpr은 대소문자를 구분하지 않는 부분 일치 조건을 만든다.
//
// 검색은 사람이 기억을 더듬어 치는 것이므로 대소문자를 구분하면 대부분 아무것도
// 찾지 못한다. 방언마다 방법이 다르다: PostgreSQL은 ILIKE, Oracle은 함수 인덱스가
// 없으면 UPPER 비교, 나머지는 기본 대조가 이미 대소문자를 무시한다.
func containsExpr(kind model.DBKind, col string, p *paramBuilder, value string) string {
	return likeExpr(kind, col, p, "%"+escapeLike(value)+"%")
}

func prefixExpr(kind model.DBKind, col string, p *paramBuilder, value string) string {
	return likeExpr(kind, col, p, escapeLike(value)+"%")
}

// likeExpr은 방언별 LIKE 조건을 만든다. 패턴은 이미 escapeLike를 거친 값이다.
func likeExpr(kind model.DBKind, col string, p *paramBuilder, pattern string) string {
	esc := likeEscapeClause(kind)
	switch kind {
	case model.KindPostgres:
		// 숫자·날짜 컬럼도 검색 대상이 되도록 텍스트로 캐스트한다.
		return fmt.Sprintf("CAST(%s AS TEXT) ILIKE %s %s", col, p.Add(pattern), esc)
	case model.KindOracle:
		return fmt.Sprintf("UPPER(TO_CHAR(%s)) LIKE UPPER(%s) %s", col, p.Add(pattern), esc)
	case model.KindMSSQL:
		return fmt.Sprintf("CAST(%s AS NVARCHAR(MAX)) LIKE %s %s", col, p.Add(pattern), esc)
	case model.KindMySQL:
		return fmt.Sprintf("CAST(%s AS CHAR) LIKE %s %s", col, p.Add(pattern), esc)
	case model.KindClickHouse:
		// ClickHouse 에는 ESCAPE 절이 없고, ilike 가 대소문자를 무시한다.
		// toString 은 널을 만나면 널을 돌려주므로 널 컬럼도 그냥 걸러진다.
		return fmt.Sprintf("toString(%s) ILIKE %s", col, p.Add(pattern))
	default: // SQLite
		return fmt.Sprintf("CAST(%s AS TEXT) LIKE %s %s", col, p.Add(pattern), esc)
	}
}

// likeEscapeClause는 ESCAPE 절을 방언에 맞게 만든다.
//
// 이스케이프 문자는 어디서나 백슬래시 하나지만, 그것을 문자열 리터럴로 적는 법이
// 다르다. MySQL은 리터럴 안의 백슬래시를 다시 이스케이프 문자로 읽으므로 '\' 는
// 닫히지 않은 문자열이 되어 문장 전체가 1064 문법 오류로 죽는다("모든 컬럼에서
// 검색"이 MySQL에서만 실패하던 원인이다). 표준 쪽(PostgreSQL·SQLite·MSSQL·Oracle)은
// '\' 가 백슬래시 하나 그대로다.
func likeEscapeClause(kind model.DBKind) string {
	if kind == model.KindMySQL {
		return `ESCAPE '\\'`
	}
	return `ESCAPE '\'`
}

// escapeLike는 LIKE 패턴에서 특별한 의미를 갖는 문자를 무력화한다.
// 사용자가 "50%"를 검색했을 때 %가 와일드카드로 해석되면 전혀 다른 결과가 나온다.
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// listObjectsSQL은 조회 가능한 테이블/뷰 목록 쿼리를 방언별로 만든다.
//
// 스키마 introspect(introspect_*.go)를 재사용하지 않는 이유: 그쪽은 컬럼·인덱스·
// 제약·주석까지 DB 전체를 읽는다. 데이터 화면은 목록을 여는 데만 그 비용을 낼 수
// 없다(introspect의 타임아웃이 90초인 것이 그 증거다). 여기서는 이름과 대략적인
// 행 수만 있으면 되고, 그것은 카탈로그 한 번 조회로 끝난다.
//
// owner는 Oracle 전용이다(다른 방언은 무시한다). 비어 있으면 접속 계정의 스키마를 본다.
// 이 값은 introspect와 같은 커넥션 옵션에서 오며, 둘이 어긋나면 구조 화면과 데이터 화면이
// 서로 다른 스키마를 보게 된다 — 같은 커넥션인데 테이블 목록이 다르다는 뜻이다.
func listObjectsSQL(kind model.DBKind, database, owner string) (string, []any) {
	switch kind {
	case model.KindMySQL:
		return `SELECT TABLE_SCHEMA, TABLE_NAME,
			CASE WHEN TABLE_TYPE = 'VIEW' THEN 'view' ELSE 'table' END,
			COALESCE(TABLE_ROWS, -1), COALESCE(TABLE_COMMENT, '')
			FROM information_schema.TABLES
			WHERE TABLE_SCHEMA = COALESCE(NULLIF(?, ''), DATABASE())
			ORDER BY TABLE_NAME`, []any{database}

	case model.KindPostgres:
		// reltuples는 ANALYZE 시점의 추정치다. 정확하지 않지만 목록에서 원하는 것은
		// "큰 테이블인가 작은 테이블인가"이고, 그 판단에는 충분하다.
		return `SELECT n.nspname, c.relname,
			CASE c.relkind WHEN 'v' THEN 'view' WHEN 'm' THEN 'view' ELSE 'table' END,
			CASE WHEN c.reltuples < 0 THEN -1 ELSE c.reltuples::bigint END,
			COALESCE(obj_description(c.oid, 'pg_class'), '')
			FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE c.relkind IN ('r', 'p', 'v', 'm')
			  AND n.nspname NOT IN ('pg_catalog', 'information_schema')
			  AND n.nspname NOT LIKE 'pg_toast%'
			  AND NOT EXISTS (SELECT 1 FROM pg_depend d
			                  WHERE d.objid = c.oid AND d.deptype = 'e')
			ORDER BY n.nspname, c.relname`, nil

	case model.KindMSSQL:
		return `SELECT s.name, t.name, 'table',
			COALESCE((SELECT SUM(p.rows) FROM sys.partitions p
			          WHERE p.object_id = t.object_id AND p.index_id IN (0, 1)), -1),
			COALESCE(CAST(ep.value AS NVARCHAR(MAX)), '')
			FROM sys.tables t
			JOIN sys.schemas s ON s.schema_id = t.schema_id
			LEFT JOIN sys.extended_properties ep
			     ON ep.major_id = t.object_id AND ep.minor_id = 0 AND ep.name = 'MS_Description'
			WHERE t.is_ms_shipped = 0
			UNION ALL
			SELECT s.name, v.name, 'view', -1, ''
			FROM sys.views v
			JOIN sys.schemas s ON s.schema_id = v.schema_id
			WHERE v.is_ms_shipped = 0
			ORDER BY 1, 2`, nil

	case model.KindOracle:
		// 한 스키마만 본다. all_tables 전체는 시스템 스키마를 수백 개 끌고 와서
		// 목록이 쓸모없어진다. 기본은 접속 계정이고, owner 옵션으로 바꿀 수 있다
		// (접속 계정이 다른 스키마의 객체를 관리하는 경우가 흔하다).
		// 딕셔너리의 이름은 대문자이므로 옵션 값도 대문자로 맞춘다.
		return `SELECT owner, table_name, 'table', COALESCE(num_rows, -1), ''
			FROM all_tables
			WHERE owner = COALESCE(NULLIF(UPPER(:1), ' '), SYS_CONTEXT('USERENV', 'CURRENT_SCHEMA'))
			UNION ALL
			SELECT owner, view_name, 'view', -1, ''
			FROM all_views
			WHERE owner = COALESCE(NULLIF(UPPER(:2), ' '), SYS_CONTEXT('USERENV', 'CURRENT_SCHEMA'))
			ORDER BY 1, 2`, []any{oracleOwner(owner), oracleOwner(owner)}

	case model.KindClickHouse:
		// system.tables 하나로 표와 뷰가 함께 나온다. 엔진 이름이 뷰인지를 말한다
		// (View·MaterializedView·LiveView). total_rows 는 엔진에 따라 NULL 이다.
		return `SELECT database, name,
			CASE WHEN engine LIKE '%View' THEN 'view' ELSE 'table' END,
			COALESCE(toInt64(total_rows), -1), comment
			FROM system.tables
			WHERE database = coalesce(nullIf(?, ''), currentDatabase())
			  AND NOT is_temporary
			ORDER BY name`, []any{database}

	default: // SQLite
		return `SELECT '', name, CASE type WHEN 'view' THEN 'view' ELSE 'table' END, -1, ''
			FROM sqlite_master
			WHERE type IN ('table', 'view') AND name NOT LIKE 'sqlite\_%' ESCAPE '\'
			ORDER BY name`, nil
	}
}

// primaryKeySQL은 한 테이블의 기본키 컬럼을 순서대로 읽는 쿼리를 만든다.
//
// 기본키가 왜 중요한가: 이 앱은 기본키로만 행을 수정한다. 기본키가 없으면 어떤
// 행을 고칠지 명확히 지정할 방법이 없고(같은 값의 행이 여러 개일 수 있다),
// 그런 테이블은 편집 불가로 표시한다. 조용히 WHERE 절을 만들어 실행하면
// 한 행을 고치려던 UPDATE가 여러 행을 바꾼다.
func primaryKeySQL(kind model.DBKind, ref TableRef) (string, []any) {
	switch kind {
	case model.KindMySQL:
		return `SELECT COLUMN_NAME FROM information_schema.KEY_COLUMN_USAGE
			WHERE CONSTRAINT_NAME = 'PRIMARY'
			  AND TABLE_SCHEMA = COALESCE(NULLIF(?, ''), DATABASE()) AND TABLE_NAME = ?
			ORDER BY ORDINAL_POSITION`, []any{ref.Namespace, ref.Name}

	case model.KindPostgres:
		return `SELECT a.attname FROM pg_index i
			JOIN pg_class c ON c.oid = i.indrelid
			JOIN pg_namespace n ON n.oid = c.relnamespace
			JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum = ANY(i.indkey)
			WHERE i.indisprimary AND c.relname = $1
			  AND n.nspname = COALESCE(NULLIF($2, ''), current_schema())
			ORDER BY array_position(i.indkey, a.attnum)`, []any{ref.Name, ref.Namespace}

	case model.KindMSSQL:
		return `SELECT c.name FROM sys.indexes i
			JOIN sys.index_columns ic ON ic.object_id = i.object_id AND ic.index_id = i.index_id
			JOIN sys.columns c ON c.object_id = i.object_id AND c.column_id = ic.column_id
			JOIN sys.tables t ON t.object_id = i.object_id
			JOIN sys.schemas s ON s.schema_id = t.schema_id
			WHERE i.is_primary_key = 1 AND t.name = @p1
			  AND s.name = COALESCE(NULLIF(@p2, ''), SCHEMA_NAME())
			ORDER BY ic.key_ordinal`, []any{ref.Name, ref.Namespace}

	case model.KindOracle:
		return `SELECT cc.column_name FROM all_constraints c
			JOIN all_cons_columns cc
			  ON cc.owner = c.owner AND cc.constraint_name = c.constraint_name
			WHERE c.constraint_type = 'P' AND c.table_name = :1
			  AND c.owner = COALESCE(NULLIF(:2, ' '), SYS_CONTEXT('USERENV', 'CURRENT_SCHEMA'))
			ORDER BY cc.position`, []any{ref.Name, oracleOwner(ref.Namespace)}

	case model.KindClickHouse:
		// ClickHouse 의 "기본키"는 정렬 키다. 유일성을 강제하지 않으므로 이것으로
		// 행 하나를 확실히 집을 수는 없다 — 그래서 데이터 화면이 이 표를 편집
		// 불가로 두는 것이 맞다. 그래도 목록을 돌려주는 이유는 화면이 그 컬럼을
		// 열쇠 표시로 그려 "무엇으로 찾는 표인가"를 보여주기 때문이다.
		return `SELECT name FROM system.columns
			WHERE database = coalesce(nullIf(?, ''), currentDatabase())
			  AND table = ? AND is_in_primary_key
			ORDER BY position`, []any{ref.Namespace, ref.Name}

	default: // SQLite
		// PRAGMA는 파라미터를 받지 않으므로 이름을 직접 넣는다. 인용은 필수다.
		return `SELECT name FROM pragma_table_info(` + sqliteLiteral(ref.Name) + `)
			WHERE pk > 0 ORDER BY pk`, nil
	}
}

// oracleOwner는 Oracle의 빈 문자열 문제를 피한다.
// Oracle은 빈 문자열을 NULL로 취급하므로 NULLIF(:2, ”)가 언제나 NULL이 되어
// 조건이 무너진다. 공백 한 칸을 센티넬로 써서 그 함정을 피한다.
func oracleOwner(ns string) string {
	if ns == "" {
		return " "
	}
	return ns
}

// sqliteLiteral은 SQLite 문자열 리터럴을 만든다. PRAGMA 함수에만 쓴다.
func sqliteLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// countSQL은 조건에 맞는 행 수를 세는 쿼리다.
func countSQL(kind model.DBKind, ref TableRef, where string) string {
	q := "SELECT count(*) FROM " + qualify(kind, ref)
	if where != "" {
		q += " WHERE " + where
	}
	return q
}

// foreignKeySQL은 이 표의 외래키를 (컬럼, 대상 스키마, 대상 표, 대상 컬럼) 로 읽는다.
//
// 데이터 화면이 FK를 따라가려면 "이 컬럼의 값이 어느 표의 어느 컬럼을 가리키는가"만
// 있으면 된다. 제약 이름이나 동작(ON DELETE)은 구조 화면의 관심사이고, 여기서는
// 한 번의 조회를 더 하는 비용을 줄이는 편이 낫다.
//
// 복합 외래키는 컬럼마다 한 줄로 나온다. 화면은 그중 눌린 컬럼 하나로 대상 행을
// 좁히므로, 복합키의 나머지 컬럼이 빠지면 여러 행이 나올 수 있다 — 그 사실은
// 결과에 몇 건인지 함께 보여주는 것으로 드러난다.
func foreignKeySQL(kind model.DBKind, ref TableRef) (string, []any) {
	switch kind {
	case model.KindMySQL:
		return `SELECT COLUMN_NAME, COALESCE(REFERENCED_TABLE_SCHEMA, ''), REFERENCED_TABLE_NAME, REFERENCED_COLUMN_NAME
			FROM information_schema.KEY_COLUMN_USAGE
			WHERE REFERENCED_TABLE_NAME IS NOT NULL
			  AND TABLE_SCHEMA = COALESCE(NULLIF(?, ''), DATABASE()) AND TABLE_NAME = ?
			ORDER BY ORDINAL_POSITION`, []any{ref.Namespace, ref.Name}

	case model.KindPostgres:
		return `SELECT kcu.column_name, ccu.table_schema, ccu.table_name, ccu.column_name
			FROM information_schema.table_constraints tc
			JOIN information_schema.key_column_usage kcu
			  ON kcu.constraint_name = tc.constraint_name AND kcu.constraint_schema = tc.constraint_schema
			JOIN information_schema.constraint_column_usage ccu
			  ON ccu.constraint_name = tc.constraint_name AND ccu.constraint_schema = tc.constraint_schema
			WHERE tc.constraint_type = 'FOREIGN KEY'
			  AND tc.table_name = $1
			  AND tc.table_schema = COALESCE(NULLIF($2, ''), current_schema())`, []any{ref.Name, ref.Namespace}

	case model.KindMSSQL:
		return `SELECT pc.name, rs.name, rt.name, rc.name
			FROM sys.foreign_key_columns fkc
			JOIN sys.tables pt ON pt.object_id = fkc.parent_object_id
			JOIN sys.schemas ps ON ps.schema_id = pt.schema_id
			JOIN sys.columns pc ON pc.object_id = fkc.parent_object_id AND pc.column_id = fkc.parent_column_id
			JOIN sys.tables rt ON rt.object_id = fkc.referenced_object_id
			JOIN sys.schemas rs ON rs.schema_id = rt.schema_id
			JOIN sys.columns rc ON rc.object_id = fkc.referenced_object_id AND rc.column_id = fkc.referenced_column_id
			WHERE pt.name = @p1 AND ps.name = COALESCE(NULLIF(@p2, ''), SCHEMA_NAME())`, []any{ref.Name, ref.Namespace}

	case model.KindOracle:
		return `SELECT cc.column_name, rc.owner, rc.table_name, rcc.column_name
			FROM all_constraints c
			JOIN all_cons_columns cc ON cc.owner = c.owner AND cc.constraint_name = c.constraint_name
			JOIN all_constraints rc ON rc.owner = c.r_owner AND rc.constraint_name = c.r_constraint_name
			JOIN all_cons_columns rcc
			  ON rcc.owner = rc.owner AND rcc.constraint_name = rc.constraint_name
			 AND rcc.position = cc.position
			WHERE c.constraint_type = 'R' AND c.table_name = :1
			  AND c.owner = COALESCE(NULLIF(:2, ' '), SYS_CONTEXT('USERENV', 'CURRENT_SCHEMA'))`,
			[]any{ref.Name, oracleOwner(ref.Namespace)}

	default: // SQLite
		// PRAGMA는 파라미터를 받지 않는다. "from"이 이 표의 컬럼, "table"·"to"가 대상이다.
		// to가 NULL이면 대상 표의 기본키를 가리킨다는 뜻이라 그대로 빈 값으로 둔다.
		return `SELECT "from", '', "table", COALESCE("to", '')
			FROM pragma_foreign_key_list(` + sqliteLiteral(ref.Name) + `)`, nil
	}
}
