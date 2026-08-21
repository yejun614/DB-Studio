package dbx

import (
	"context"
	"encoding/hex"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"dbstudio/internal/model"
)

// 덤프에 필요한 저수준 도구.
//
// 데이터 화면(data.go)의 조회와 나누는 이유가 분명하다. 화면은 **보여주기 위해**
// 값을 가공한다 — 바이너리를 16진 문자열로 바꾸고, 긴 문자열을 자르고, 시각을 고정
// 형식으로 만든다. 덤프에서 그 값을 쓰면 복구했을 때 원본과 다른 데이터가 들어간다.
// 그래서 덤프는 드라이버가 준 값을 그대로 받아 방언별 리터럴로 직접 쓴다.

// RowStreamer는 원본 값을 배치로 흘려보낼 수 있는 어댑터가 구현한다.
type RowStreamer interface {
	StreamRows(ctx context.Context, t Target, ref TableRef, batch int,
		fn func(cols []DataColumn, rows [][]any) error) error
}

// StreamRows는 테이블 전체를 커서 하나로 읽어 배치마다 fn을 부른다.
//
// 페이지 단위(LIMIT/OFFSET)로 읽지 않는 이유: OFFSET은 뒤로 갈수록 앞의 행을 다시
// 세므로 큰 테이블에서 전체 시간이 행 수의 제곱에 가까워진다. 덤프는 처음부터 끝까지
// 한 번 읽으면 되는 작업이므로 커서 하나가 맞다.
func (a *sqlAdapter) StreamRows(ctx context.Context, t Target, ref TableRef, batch int,
	fn func(cols []DataColumn, rows [][]any) error) error {
	if batch <= 0 {
		batch = 500
	}
	db, err := a.open(t, 1)
	if err != nil {
		return err
	}
	defer db.Close()

	rows, err := db.QueryContext(ctx, "SELECT * FROM "+qualify(a.kind, ref))
	if err != nil {
		return fmt.Errorf("%s 조회 실패: %w", ref, err)
	}
	defer rows.Close()

	cts, err := rows.ColumnTypes()
	if err != nil {
		return fmt.Errorf("%s 컬럼 정보를 읽지 못했습니다: %w", ref, err)
	}
	cols := make([]DataColumn, 0, len(cts))
	for _, ct := range cts {
		nullable, ok := ct.Nullable()
		if !ok {
			nullable = true
		}
		cols = append(cols, DataColumn{
			Name: ct.Name(), Type: ct.DatabaseTypeName(), Nullable: nullable,
			Numeric: isNumericType(ct.DatabaseTypeName()),
		})
	}

	buf := make([][]any, 0, batch)
	for rows.Next() {
		// 취소는 rows.Next()가 아니라 여기서 확인한다. 드라이버에 따라
		// 컨텍스트 취소가 진행 중인 커서를 즉시 끊지 않는다.
		if err := ctx.Err(); err != nil {
			return err
		}
		holders := make([]any, len(cols))
		pointers := make([]any, len(cols))
		for i := range holders {
			pointers[i] = &holders[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			return fmt.Errorf("%s 행 스캔 실패: %w", ref, err)
		}
		// 드라이버가 재사용하는 버퍼([]byte)를 그대로 넘기면 다음 Scan에서 덮인다.
		// 배치가 쌓이는 동안 값이 바뀌므로 여기서 복사한다.
		row := make([]any, len(holders))
		for i, v := range holders {
			if b, ok := v.([]byte); ok {
				cp := make([]byte, len(b))
				copy(cp, b)
				row[i] = cp
				continue
			}
			row[i] = v
		}
		buf = append(buf, row)

		if len(buf) >= batch {
			if err := fn(cols, buf); err != nil {
				return err
			}
			buf = make([][]any, 0, batch)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("%s 행 순회 실패: %w", ref, err)
	}
	if len(buf) > 0 {
		return fn(cols, buf)
	}
	// 빈 테이블이어도 컬럼 정보는 알려준다. 호출부가 헤더를 쓸지 판단해야 한다.
	return fn(cols, nil)
}

// QuoteIdent는 식별자를 방언에 맞게 인용한다(덤프 생성용 공개 창구).
func QuoteIdent(kind model.DBKind, name string) string { return quoteIdent(kind, name) }

// QualifyTable은 네임스페이스를 포함한 인용 이름을 만든다.
func QualifyTable(kind model.DBKind, namespace, name string) string {
	return qualify(kind, TableRef{Namespace: namespace, Name: name})
}

// SplitStatements는 스크립트를 문장 단위로 나눈다(복구용 공개 창구).
func SplitStatements(kind model.DBKind, script string) []string { return splitSQL(kind, script) }

// SQLLiteral은 값을 방언에 맞는 SQL 리터럴로 만든다.
//
// 이 함수가 틀리면 복구가 통째로 실패하거나, 더 나쁘게는 **다른 데이터가 들어간다**.
// 그래서 추측하지 않는다 — 모르는 타입은 문자열로 만들고, 그것이 틀리면 복구 시점에
// DB가 거부한다. 조용히 맞는 척하는 것보다 낫다.
func SQLLiteral(kind model.DBKind, v any) string {
	switch val := v.(type) {
	case nil:
		return "NULL"
	case bool:
		return boolLiteral(kind, val)
	case int64:
		return strconv.FormatInt(val, 10)
	case int32:
		return strconv.FormatInt(int64(val), 10)
	case int:
		return strconv.Itoa(val)
	case float64:
		return floatLiteral(val)
	case float32:
		return floatLiteral(float64(val))
	case time.Time:
		return timeLiteral(kind, val)
	case []byte:
		// 유효한 UTF-8이라도 바이너리 컬럼일 수 있다. 그런데 텍스트 컬럼의 값을
		// 16진으로 쓰면 복구 후 값이 달라지는 DB가 있다(문자열과 바이트열이
		// 서로 다른 타입인 PostgreSQL). 그래서 UTF-8이면 문자열로 본다 —
		// 텍스트를 텍스트로 되돌리는 쪽이 흔하고, 진짜 바이너리는 대개 UTF-8이 아니다.
		if isPrintableUTF8(val) {
			return stringLiteral(kind, string(val))
		}
		return bytesLiteral(kind, val)
	case string:
		return stringLiteral(kind, val)
	default:
		return stringLiteral(kind, fmt.Sprint(val))
	}
}

func boolLiteral(kind model.DBKind, v bool) string {
	if kind == model.KindPostgres {
		if v {
			return "TRUE"
		}
		return "FALSE"
	}
	// 나머지는 불리언 타입이 없거나 정수와 호환된다.
	if v {
		return "1"
	}
	return "0"
}

func floatLiteral(v float64) string {
	// NaN·무한대는 SQL 리터럴이 없다. 값을 잃는 것보다 NULL로 두고
	// 그 사실이 드러나게 하는 편이 낫다(복구 후 비교하면 바로 보인다).
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return "NULL"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

func timeLiteral(kind model.DBKind, t time.Time) string {
	const layout = "2006-01-02 15:04:05.999999999"
	s := t.Format(layout)
	switch kind {
	case model.KindOracle:
		// Oracle은 문자열을 날짜로 암묵 변환하지 않는다(NLS 설정에 따라 달라진다).
		// 형식을 명시해야 어느 환경에서 복구해도 같은 값이 들어간다.
		return fmt.Sprintf("TO_TIMESTAMP('%s', 'YYYY-MM-DD HH24:MI:SS.FF')", s)
	case model.KindPostgres:
		return fmt.Sprintf("TIMESTAMP '%s'", s)
	default:
		return "'" + s + "'"
	}
}

func bytesLiteral(kind model.DBKind, b []byte) string {
	enc := hex.EncodeToString(b)
	switch kind {
	case model.KindPostgres:
		return fmt.Sprintf("decode('%s', 'hex')", enc)
	case model.KindMSSQL:
		return "0x" + enc
	case model.KindOracle:
		// HEXTORAW는 2000바이트가 상한이다. 넘으면 BLOB 조립이 필요한데,
		// 그것은 방언별 PL/SQL이 되므로 덤프에 넣지 않고 사실을 남긴다.
		if len(b) > 2000 {
			return fmt.Sprintf("NULL /* %d바이트 바이너리: Oracle 리터럴 상한(2000)을 넘어 생략됨 */", len(b))
		}
		return fmt.Sprintf("HEXTORAW('%s')", enc)
	default: // MySQL · SQLite
		return "X'" + enc + "'"
	}
}

func stringLiteral(kind model.DBKind, s string) string {
	escaped := strings.ReplaceAll(s, "'", "''")
	if kind == model.KindMySQL {
		// MySQL은 기본 설정에서 백슬래시를 이스케이프 문자로 해석한다.
		// 그대로 두면 "C:\new"의 \n이 줄바꿈이 되어 복구 후 값이 달라진다.
		escaped = strings.ReplaceAll(escaped, `\`, `\\`)
	}
	// 제어문자는 리터럴 안에 그대로 두어도 대부분의 DB가 받아들이지만,
	// NUL은 어디서도 받지 않는다. 값을 잃지 않으려면 남겨야 하므로 그대로 두되
	// NUL만 뺀다 — NUL이 든 문자열은 애초에 대부분의 DB가 저장하지 못한다.
	escaped = strings.ReplaceAll(escaped, "\x00", "")
	return "'" + escaped + "'"
}

// isPrintableUTF8은 바이트열을 텍스트로 볼지 판단한다.
func isPrintableUTF8(b []byte) bool {
	if len(b) == 0 {
		return true
	}
	if !utf8.Valid(b) {
		return false
	}
	// 제어문자가 섞여 있으면 텍스트가 아니라고 본다(줄바꿈·탭은 예외).
	for _, c := range b {
		if c < 0x20 && c != '\n' && c != '\r' && c != '\t' {
			return false
		}
	}
	return true
}

// PrimaryKeyColumns는 테이블의 기본키 컬럼을 반환한다(덤프 정렬용 공개 창구).
func PrimaryKeyColumns(ctx context.Context, t Target, ref TableRef) []string {
	a, err := Get(t.Conn.Kind)
	if err != nil {
		return nil
	}
	sa, ok := a.(*sqlAdapter)
	if !ok {
		return nil
	}
	db, err := sa.open(t, 1)
	if err != nil {
		return nil
	}
	defer db.Close()
	return sa.primaryKey(ctx, db, ref)
}

// ExecScript는 문장 목록을 순서대로 실행하며 진행 상황을 알린다.
//
// ExecDDL(마이그레이션 실행기가 쓰는 것)을 그대로 쓰지 않는 이유: 그쪽은 모든 문장의
// 실행 기록을 메모리에 쌓는다. 마이그레이션은 수십 문장이지만 복구는 수십만 문장이
// 될 수 있어 그 기록만으로 메모리가 바닥난다. 여기서는 진행 숫자만 넘긴다.
//
// onProgress가 false를 반환하면 중단한다(사용자 취소).
func ExecScript(ctx context.Context, t Target, stmts []string,
	onProgress func(done int, current string) bool) (int, string, error) {
	a, err := Get(t.Conn.Kind)
	if err != nil {
		return 0, "", err
	}
	sa, ok := a.(*sqlAdapter)
	if !ok {
		return 0, "", fmt.Errorf("%w: %s 스크립트 실행", ErrNotImplemented, t.Conn.Kind)
	}
	db, err := sa.open(t, 1)
	if err != nil {
		return 0, "", err
	}
	defer db.Close()

	// 커넥션 하나로 고정한다. 세션 설정(예: 외래키 검사 끄기)이 문장 사이에 유지되어야
	// 하는데, 풀에서 매번 다른 커넥션을 받으면 그 설정이 사라진다.
	conn, err := db.Conn(ctx)
	if err != nil {
		return 0, "", fmt.Errorf("접속 실패: %w", err)
	}
	defer conn.Close()

	for i, stmt := range stmts {
		if err := ctx.Err(); err != nil {
			return i, stmt, err
		}
		if onProgress != nil && !onProgress(i, stmt) {
			return i, stmt, context.Canceled
		}
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			return i, stmt, fmt.Errorf("%d번째 문장 실패: %w", i+1, err)
		}
	}
	return len(stmts), "", nil
}
