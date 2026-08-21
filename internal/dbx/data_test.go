package dbx

import (
	"strings"
	"testing"

	"dbstudio/internal/model"
)

// 세미콜론으로 자르면 문자열·주석·달러 인용 안의 세미콜론에서 문장이 조각난다.
// 사용자는 자기가 쓴 SQL이 틀렸다고 오해하게 되므로 이 분리는 정확해야 한다.
func TestSplitSQL(t *testing.T) {
	cases := []struct {
		name string
		kind model.DBKind
		in   string
		want []string
	}{
		{
			name: "단순 두 문장",
			kind: model.KindPostgres,
			in:   "SELECT 1; SELECT 2;",
			want: []string{"SELECT 1", "SELECT 2"},
		},
		{
			name: "문자열 안의 세미콜론",
			kind: model.KindPostgres,
			in:   "SELECT 'a;b'; SELECT 2",
			want: []string{"SELECT 'a;b'", "SELECT 2"},
		},
		{
			name: "이스케이프된 작은따옴표",
			kind: model.KindPostgres,
			in:   "SELECT 'it''s; here'; SELECT 2",
			want: []string{"SELECT 'it''s; here'", "SELECT 2"},
		},
		{
			name: "줄 주석 안의 세미콜론",
			kind: model.KindPostgres,
			in:   "SELECT 1 -- 주석; 아님\n; SELECT 2",
			want: []string{"SELECT 1 -- 주석; 아님", "SELECT 2"},
		},
		{
			name: "블록 주석 안의 세미콜론",
			kind: model.KindPostgres,
			in:   "SELECT /* a; b */ 1; SELECT 2",
			want: []string{"SELECT /* a; b */ 1", "SELECT 2"},
		},
		{
			name: "PostgreSQL 달러 인용",
			kind: model.KindPostgres,
			in:   "CREATE FUNCTION f() RETURNS int AS $$ BEGIN RETURN 1; END; $$ LANGUAGE plpgsql; SELECT 1",
			want: []string{
				"CREATE FUNCTION f() RETURNS int AS $$ BEGIN RETURN 1; END; $$ LANGUAGE plpgsql",
				"SELECT 1",
			},
		},
		{
			name: "MySQL 백슬래시 이스케이프",
			kind: model.KindMySQL,
			in:   `SELECT 'a\'; b'; SELECT 2`,
			want: []string{`SELECT 'a\'; b'`, "SELECT 2"},
		},
		{
			name: "MySQL 해시 주석",
			kind: model.KindMySQL,
			in:   "SELECT 1 # 주석; 아님\n; SELECT 2",
			want: []string{"SELECT 1 # 주석; 아님", "SELECT 2"},
		},
		{
			name: "백틱 식별자 안의 세미콜론",
			kind: model.KindMySQL,
			in:   "SELECT `we;ird`; SELECT 2",
			want: []string{"SELECT `we;ird`", "SELECT 2"},
		},
		{
			name: "빈 문장은 버린다",
			kind: model.KindSQLite,
			in:   "SELECT 1;;;   ;SELECT 2;",
			want: []string{"SELECT 1", "SELECT 2"},
		},
		{
			name: "세미콜론 없는 한 문장",
			kind: model.KindSQLite,
			in:   "SELECT 1",
			want: []string{"SELECT 1"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := splitSQL(tc.kind, tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("문장 수 %d, want %d: %q", len(got), len(tc.want), got)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("문장 %d = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// 잘린 입력에서 패닉이 나면 안 된다. 콘솔은 타이핑 중에도 저장될 수 있고,
// 사용자는 무엇이든 붙여 넣는다.
func TestSplitSQLSurvivesTruncatedInput(t *testing.T) {
	inputs := []string{
		"SELECT 'unterminated",
		"SELECT /* unterminated",
		"SELECT $$ unterminated",
		"SELECT `unterminated",
		"--",
		"",
		"$",
	}
	for _, in := range inputs {
		for _, kind := range []model.DBKind{model.KindPostgres, model.KindMySQL, model.KindSQLite} {
			splitSQL(kind, in) // 패닉이 나지 않으면 통과
		}
	}
}

func TestQuoteIdentEscapesPerDialect(t *testing.T) {
	cases := []struct {
		kind model.DBKind
		in   string
		want string
	}{
		{model.KindMySQL, "order", "`order`"},
		{model.KindMySQL, "we`ird", "`we``ird`"},
		{model.KindMSSQL, "order", "[order]"},
		{model.KindMSSQL, "we]ird", "[we]]ird]"},
		{model.KindPostgres, "order", `"order"`},
		{model.KindPostgres, `we"ird`, `"we""ird"`},
		{model.KindSQLite, "table", `"table"`},
		{model.KindOracle, "SELECT", `"SELECT"`},
	}
	for _, tc := range cases {
		if got := quoteIdent(tc.kind, tc.in); got != tc.want {
			t.Errorf("quoteIdent(%s, %q) = %q, want %q", tc.kind, tc.in, got, tc.want)
		}
	}
}

func TestPlaceholderPerDialect(t *testing.T) {
	cases := []struct {
		kind model.DBKind
		n    int
		want string
	}{
		{model.KindPostgres, 2, "$2"},
		{model.KindOracle, 3, ":3"},
		{model.KindMSSQL, 1, "@p1"},
		{model.KindMySQL, 5, "?"},
		{model.KindSQLite, 5, "?"},
	}
	for _, tc := range cases {
		if got := placeholder(tc.kind, tc.n); got != tc.want {
			t.Errorf("placeholder(%s, %d) = %q, want %q", tc.kind, tc.n, got, tc.want)
		}
	}
}

// 파라미터 번호를 손으로 세면 반드시 어긋나고, 어긋난 결과는
// "값이 하나 밀린 UPDATE"다. 빌더가 그 일을 대신하는지 확인한다.
func TestParamBuilderNumbersInOrder(t *testing.T) {
	p := newParams(model.KindPostgres)
	if got := p.Add("a"); got != "$1" {
		t.Errorf("첫 번째 = %q", got)
	}
	if got := p.Add(2); got != "$2" {
		t.Errorf("두 번째 = %q", got)
	}
	values := p.Values()
	if len(values) != 2 || values[0] != "a" || values[1] != 2 {
		t.Errorf("값이 순서대로 쌓여야 한다: %v", values)
	}
}

// 정렬할 것이 없는 대상(뷰, 기본키 없는 테이블)도 조회는 되어야 한다.
// 예전에는 여기서 에러를 냈고, 그래서 Oracle·MS-SQL에서만 뷰를 열 수 없었다.
// 방언 차이는 하나뿐이다: MS-SQL의 OFFSET/FETCH만 문법상 ORDER BY를 요구한다.
func TestPageClauseWithoutOrder(t *testing.T) {
	for _, kind := range []model.DBKind{model.KindMSSQL, model.KindOracle} {
		clause := pageClause(kind, newParams(kind), 10, 0, false)
		if !strings.Contains(clause, "OFFSET") || !strings.Contains(clause, "FETCH NEXT") {
			t.Errorf("%s: OFFSET/FETCH 절이어야 한다: %q", kind, clause)
		}
		wantFiller := kind == model.KindMSSQL
		if got := strings.Contains(clause, "ORDER BY (SELECT NULL)"); got != wantFiller {
			t.Errorf("%s: ORDER BY (SELECT NULL) 유무 = %v, 기대 %v (%q)",
				kind, got, wantFiller, clause)
		}

		withOrder := pageClause(kind, newParams(kind), 10, 20, true)
		if strings.Contains(withOrder, "ORDER BY") {
			t.Errorf("%s: 이미 정렬이 있으면 절을 덧붙이면 안 된다: %q", kind, withOrder)
		}
		if !strings.Contains(withOrder, "OFFSET") || !strings.Contains(withOrder, "FETCH NEXT") {
			t.Errorf("%s: OFFSET/FETCH 절이어야 한다: %q", kind, withOrder)
		}
	}
	for _, kind := range []model.DBKind{model.KindPostgres, model.KindMySQL, model.KindSQLite} {
		clause := pageClause(kind, newParams(kind), 10, 0, false)
		if !strings.Contains(clause, "LIMIT") {
			t.Errorf("%s: LIMIT 절이어야 한다: %q", kind, clause)
		}
		if strings.Contains(clause, "ORDER BY") {
			t.Errorf("%s: 정렬 절을 덧붙이면 안 된다: %q", kind, clause)
		}
	}
}

// 사용자가 "50%"를 검색했을 때 %가 와일드카드로 해석되면
// 전혀 다른 결과가 나오고, 그 이유를 설명할 방법이 없다.
func TestEscapeLike(t *testing.T) {
	got := escapeLike(`50% _ \`)
	if !strings.Contains(got, `\%`) || !strings.Contains(got, `\_`) {
		t.Errorf("메타문자가 이스케이프되지 않았다: %q", got)
	}
}

// MySQL은 문자열 리터럴 안의 백슬래시를 다시 이스케이프로 읽는다.
// ESCAPE '\' 를 그대로 보내면 문자열이 닫히지 않아 검색이 통째로 1064로 죽는다.
func TestLikeEscapeClausePerDialect(t *testing.T) {
	if got := likeEscapeClause(model.KindMySQL); got != `ESCAPE '\\'` {
		t.Errorf("MySQL = %s, want ESCAPE '\\\\'", got)
	}
	for _, kind := range []model.DBKind{
		model.KindPostgres, model.KindSQLite, model.KindMSSQL, model.KindOracle,
	} {
		if got := likeEscapeClause(kind); got != `ESCAPE '\'` {
			t.Errorf("%s = %s, want ESCAPE '\\'", kind, got)
		}
	}
}

// 검색 조건이 실제로 어떤 문장이 되는지 고정한다.
// 파라미터는 자리표시자로 나가야 한다(값을 문장에 이어 붙이면 주입 통로가 된다).
func TestContainsExprBuildsParameterizedLike(t *testing.T) {
	cases := []struct {
		kind model.DBKind
		want string
	}{
		{model.KindMySQL, "CAST(`c` AS CHAR) LIKE ? ESCAPE '\\\\'"},
		{model.KindSQLite, `CAST("c" AS TEXT) LIKE ? ESCAPE '\'`},
		{model.KindPostgres, `CAST("c" AS TEXT) ILIKE $1 ESCAPE '\'`},
	}
	for _, tc := range cases {
		p := newParams(tc.kind)
		got := containsExpr(tc.kind, quoteIdent(tc.kind, "c"), p, "50%")
		if got != tc.want {
			t.Errorf("%s = %q, want %q", tc.kind, got, tc.want)
		}
		if len(p.Values()) != 1 {
			t.Errorf("%s: 인자 %d개, want 1", tc.kind, len(p.Values()))
		}
		// 사용자가 친 %는 와일드카드가 아니라 글자여야 한다.
		if v, _ := p.Values()[0].(string); v != `%50\%%` {
			t.Errorf("%s: 패턴 = %q", tc.kind, p.Values()[0])
		}
	}
}

func TestIsReadOnlyStatement(t *testing.T) {
	readOnly := []string{
		"SELECT 1",
		"  select * from t",
		"-- 주석\nSELECT 1",
		"/* 주석 */ SELECT 1",
		"WITH x AS (SELECT 1) SELECT * FROM x",
		"SHOW TABLES",
		"EXPLAIN SELECT 1",
		"PRAGMA table_info(t)",
		"DESC t",
		"DESCRIBE t",
		// USE는 세션의 기본 DB를 옮길 뿐이다. 조회하러 다니는 흐름을 막지 않는다.
		"USE appdb",
		"use appdb",
		"USE\tappdb",
		"USE\nappdb",
		"-- 옮기고\nUSE appdb",
	}
	for _, s := range readOnly {
		if !isReadOnlyStatement(s) {
			t.Errorf("조회로 판정되어야 한다: %q", s)
		}
	}

	writes := []string{
		"UPDATE t SET a = 1",
		"DELETE FROM t",
		"DROP TABLE t",
		"INSERT INTO t VALUES (1)",
		// CTE 뒤에 쓰기가 오는 PostgreSQL 형태. 앞 단어만 보면 놓친다.
		"WITH x AS (SELECT 1) INSERT INTO t SELECT * FROM x",
		"WITH d AS (DELETE FROM t RETURNING *) SELECT * FROM d",
		// 낱말 경계를 보지 않으면 USE·DESC 로 잘못 읽히는 것들.
		"USER_LOCK('x')",
		"DESCEND_ORDER_PROC()",
		"USEXYZ",
		// ANALYZE가 붙은 EXPLAIN은 **문장을 실제로 실행한다**.
		// 읽기 전용 표시가 거짓말이 되는 자리다.
		"EXPLAIN ANALYZE DELETE FROM t",
		"EXPLAIN (ANALYZE, BUFFERS) UPDATE t SET a = 1",
		"explain analyse insert into t values (1)",
		"EXPLAIN ANALYZE WITH d AS (DELETE FROM t RETURNING *) SELECT * FROM d",
	}
	for _, s := range writes {
		if isReadOnlyStatement(s) {
			t.Errorf("쓰기로 판정되어야 한다: %q", s)
		}
	}
}

func TestNormalizeValue(t *testing.T) {
	// 유효한 UTF-8 바이트는 문자열이 된다(MySQL 드라이버가 대부분 []byte를 준다).
	if got, _ := normalizeValue([]byte("안녕"), true); got != "안녕" {
		t.Errorf("UTF-8 바이트는 문자열이어야 한다: %v", got)
	}
	// 바이너리는 16진 표기로 떨어진다. 그대로 보내면 JSON이 깨진다.
	got, _ := normalizeValue([]byte{0xff, 0xfe, 0x00}, true)
	s, ok := got.(string)
	if !ok || !strings.HasPrefix(s, "0x") {
		t.Errorf("바이너리는 16진 문자열이어야 한다: %v", got)
	}
	if got, _ := normalizeValue(nil, true); got != nil {
		t.Errorf("nil은 nil이어야 한다: %v", got)
	}
	if got, _ := normalizeValue(int64(7), true); got != int64(7) {
		t.Errorf("정수는 그대로여야 한다: %v", got)
	}
}

// 긴 값은 목록에서 잘리고, 잘렸다는 사실이 함께 나와야 한다.
// 잘린 값을 전체 값으로 착각해 저장하면 데이터가 손상된다.
func TestNormalizeValueTruncatesAndReports(t *testing.T) {
	long := strings.Repeat("가", maxCellLen+100)
	got, cut := normalizeValue(long, true)
	if !cut {
		t.Fatal("잘렸다고 알려야 한다")
	}
	if len([]rune(got.(string))) != maxCellLen {
		t.Errorf("문자 경계에서 잘라야 한다: %d자", len([]rune(got.(string))))
	}

	// full 모드(편집용 단일 행 조회)에서는 자르지 않는다.
	full, cutFull := normalizeValue(long, false)
	if cutFull || len([]rune(full.(string))) != len([]rune(long)) {
		t.Error("full 모드에서는 원본 그대로여야 한다")
	}
}

func TestIsNumericType(t *testing.T) {
	numeric := []string{"INT", "BIGINT", "int8", "NUMERIC", "DECIMAL", "NUMBER", "DOUBLE PRECISION", "REAL", "MONEY", "SERIAL"}
	for _, s := range numeric {
		if !isNumericType(s) {
			t.Errorf("%s 는 수치형이어야 한다", s)
		}
	}
	for _, s := range []string{"TEXT", "VARCHAR", "DATE", "BOOLEAN", "JSONB", "UUID"} {
		if isNumericType(s) {
			t.Errorf("%s 는 수치형이 아니다", s)
		}
	}
}

// PostgreSQL은 정수 컬럼에 문자열 파라미터를 주면 그냥 실패한다.
// 화면은 모든 입력을 문자열로 보내므로 여기서 되돌려 놓아야 한다.
func TestCoerceConvertsNumericFilters(t *testing.T) {
	numCol := &DataColumn{Name: "id", Type: "INTEGER", Numeric: true}
	if got := coerce(numCol, "42"); got != int64(42) {
		t.Errorf("정수로 변환되어야 한다: %v (%T)", got, got)
	}
	if got := coerce(numCol, "4.5"); got != 4.5 {
		t.Errorf("실수로 변환되어야 한다: %v", got)
	}
	// 숫자로 읽히지 않으면 문자열 그대로 보내 DB가 판단하게 한다.
	if got := coerce(numCol, "abc"); got != "abc" {
		t.Errorf("변환 실패 시 원본이어야 한다: %v", got)
	}
	textCol := &DataColumn{Name: "name", Type: "TEXT"}
	if got := coerce(textCol, "42"); got != "42" {
		t.Errorf("문자열 컬럼은 변환하지 않는다: %v", got)
	}
}

// 구조 화면(introspect)은 owner 옵션을 보는데 데이터 화면이 보지 않으면,
// 같은 커넥션인데 두 화면의 테이블 목록이 달라진다.
func TestListObjectsSQLOracleHonorsOwner(t *testing.T) {
	_, args := listObjectsSQL(model.KindOracle, "FREEPDB1", "appuser")
	for _, a := range args {
		if a != "appuser" {
			t.Errorf("owner 인자가 전달되지 않았습니다: %v", args)
		}
	}
	if len(args) == 0 {
		t.Fatal("owner를 지정했는데 인자가 없습니다")
	}

	// 비어 있으면 접속 계정을 쓰도록 센티넬(공백)이 간다.
	// Oracle은 빈 문자열을 NULL로 보므로 ''를 그대로 보내면 조건이 무너진다.
	_, empty := listObjectsSQL(model.KindOracle, "FREEPDB1", "")
	for _, a := range empty {
		if a != " " {
			t.Errorf("빈 owner는 공백 센티넬이어야 합니다: %v", empty)
		}
	}

	// 다른 방언은 owner를 무시한다.
	if _, args := listObjectsSQL(model.KindPostgres, "appdb", "someone"); args != nil {
		t.Errorf("PostgreSQL은 owner를 쓰지 않습니다: %v", args)
	}
}

func TestQualifyAddsNamespace(t *testing.T) {
	ref := TableRef{Namespace: "public", Name: "users"}
	if got := qualify(model.KindPostgres, ref); got != `"public"."users"` {
		t.Errorf("qualify = %q", got)
	}
	bare := TableRef{Name: "users"}
	if got := qualify(model.KindPostgres, bare); got != `"users"` {
		t.Errorf("네임스페이스가 없으면 붙이지 않는다: %q", got)
	}
}

func TestRowQueryLimits(t *testing.T) {
	if got := (RowQuery{}).EffectiveLimit(); got != DefaultRowLimit {
		t.Errorf("기본 한도 = %d", got)
	}
	if got := (RowQuery{Limit: 100000}).EffectiveLimit(); got != MaxRowLimit {
		t.Errorf("상한을 넘으면 상한으로 = %d", got)
	}
	if got := (RowQuery{Limit: 10}).EffectiveLimit(); got != 10 {
		t.Errorf("범위 안의 값은 그대로 = %d", got)
	}
}

func TestFilterOpValidation(t *testing.T) {
	for _, op := range []FilterOp{OpEq, OpNe, OpLt, OpLte, OpGt, OpGte, OpContains, OpPrefix, OpIsNull, OpNotNull} {
		if !op.Valid() {
			t.Errorf("%s 는 유효해야 한다", op)
		}
	}
	if FilterOp("drop").Valid() {
		t.Error("모르는 연산은 거부해야 한다")
	}
	if OpIsNull.NeedsValue() || OpNotNull.NeedsValue() {
		t.Error("NULL 조건에는 값이 필요 없다")
	}
	if !OpEq.NeedsValue() {
		t.Error("= 조건에는 값이 필요하다")
	}
}

// 관계형 어댑터는 데이터 기능을 전부 지원하고, Redis는 필터·정렬이 없다.
// 화면이 이 값으로 컨트롤을 그리므로 어긋나면 동작하지 않는 버튼이 생긴다.
func TestDataCapsPerKind(t *testing.T) {
	for _, kind := range []model.DBKind{
		model.KindMySQL, model.KindPostgres, model.KindMSSQL, model.KindOracle, model.KindSQLite,
	} {
		caps := DataCapsFor(kind)
		if !caps.Browse || !caps.Filter || !caps.Sort || !caps.Mutate || !caps.Statement {
			t.Errorf("%s: 관계형은 데이터 기능을 모두 지원해야 한다: %+v", kind, caps)
		}
	}
	mongo := DataCapsFor(model.KindMongoDB)
	if !mongo.Browse || !mongo.Mutate || mongo.StatementLabel != "명령" {
		t.Errorf("MongoDB 기능이 잘못되었다: %+v", mongo)
	}
	redis := DataCapsFor(model.KindRedis)
	if redis.Filter || redis.Sort {
		t.Errorf("Redis에는 필터·정렬이 없다: %+v", redis)
	}
	if !redis.Browse || !redis.Mutate {
		t.Errorf("Redis도 조회·수정은 된다: %+v", redis)
	}
}

// 되돌릴 수 없는 명령은 콘솔에서 막는다. 권한과 별개로,
// 한 줄 잘못 쳐서 일어나기에는 결과가 너무 크다.
func TestRedisBlockedCommands(t *testing.T) {
	for _, cmd := range []string{"FLUSHALL", "flushdb", "SHUTDOWN", "EVAL", "CONFIG"} {
		blocked := redisBlockedCommand(cmd)
		if cmd == "CONFIG" {
			// CONFIG는 읽기 목적으로도 자주 쓰이므로 막지 않는다.
			if blocked {
				t.Errorf("CONFIG는 막지 않는다")
			}
			continue
		}
		if !blocked {
			t.Errorf("%s 는 막아야 한다", cmd)
		}
	}
	if redisBlockedCommand("GET") {
		t.Error("GET을 막아서는 안 된다")
	}
}

func TestRedisSplitArgs(t *testing.T) {
	got, err := redisSplitArgs(`SET user:1 "홍 길동"`)
	if err != nil {
		t.Fatalf("redisSplitArgs: %v", err)
	}
	want := []string{"SET", "user:1", "홍 길동"}
	if len(got) != len(want) {
		t.Fatalf("인자 수 %d: %q", len(got), got)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("인자 %d = %q, want %q", i, got[i], want[i])
		}
	}
	if _, err := redisSplitArgs(`SET k "열린 따옴표`); err == nil {
		t.Error("닫히지 않은 따옴표는 오류여야 한다")
	}
}

// 검색어의 정규식 메타문자가 그대로 해석되면 결과를 설명할 수 없다.
func TestRegexpQuote(t *testing.T) {
	got := regexpQuote("a.b*c")
	if strings.Contains(got, ".b") && !strings.Contains(got, `\.`) {
		t.Errorf("점이 이스케이프되지 않았다: %q", got)
	}
	if !strings.Contains(got, `\*`) {
		t.Errorf("별표가 이스케이프되지 않았다: %q", got)
	}
}
