package dbx

import (
	"strings"
	"testing"
	"time"

	"dbstudio/internal/model"
)

// 리터럴 인코딩이 틀리면 복구가 실패하거나, 더 나쁘게는 **다른 데이터가 들어간다**.
// 후자는 아무도 알아채지 못한 채 지나가므로 여기가 이 패키지에서 가장 중요한 테스트다.
func TestSQLLiteralEscapesQuotes(t *testing.T) {
	for _, kind := range []model.DBKind{
		model.KindMySQL, model.KindPostgres, model.KindMSSQL, model.KindOracle, model.KindSQLite,
	} {
		got := SQLLiteral(kind, "O'Brien")
		if got != "'O''Brien'" {
			t.Errorf("%s: 작은따옴표 이스케이프 = %q", kind, got)
		}
	}
}

// MySQL은 기본 설정에서 백슬래시를 이스케이프 문자로 해석한다. 그대로 두면
// "C:\new"의 \n이 줄바꿈이 되어 복구 후 값이 달라진다 — 조용히 틀리는 종류의 버그다.
func TestSQLLiteralEscapesBackslashOnlyForMySQL(t *testing.T) {
	if got := SQLLiteral(model.KindMySQL, `back\slash`); got != `'back\\slash'` {
		t.Errorf("MySQL 백슬래시 = %q", got)
	}
	// 나머지 방언에서 백슬래시를 이스케이프하면 반대로 값이 늘어난다.
	for _, kind := range []model.DBKind{model.KindPostgres, model.KindSQLite, model.KindMSSQL} {
		if got := SQLLiteral(kind, `back\slash`); got != `'back\slash'` {
			t.Errorf("%s 백슬래시 = %q", kind, got)
		}
	}
}

func TestSQLLiteralNullAndNumbers(t *testing.T) {
	if got := SQLLiteral(model.KindSQLite, nil); got != "NULL" {
		t.Errorf("nil = %q", got)
	}
	if got := SQLLiteral(model.KindSQLite, int64(-42)); got != "-42" {
		t.Errorf("정수 = %q", got)
	}
	if got := SQLLiteral(model.KindSQLite, 1.5); got != "1.5" {
		t.Errorf("실수 = %q", got)
	}
	// NaN·무한대는 SQL 리터럴이 없다. 값을 지어내는 것보다 NULL이 정직하다.
	if got := SQLLiteral(model.KindSQLite, mustNaN()); got != "NULL" {
		t.Errorf("NaN = %q", got)
	}
}

func mustNaN() float64 {
	zero := 0.0
	return zero / zero
}

func TestSQLLiteralBool(t *testing.T) {
	// PostgreSQL만 진짜 불리언 타입을 가진다. 나머지는 정수와 호환된다.
	if got := SQLLiteral(model.KindPostgres, true); got != "TRUE" {
		t.Errorf("PostgreSQL true = %q", got)
	}
	if got := SQLLiteral(model.KindMySQL, true); got != "1" {
		t.Errorf("MySQL true = %q", got)
	}
	if got := SQLLiteral(model.KindMySQL, false); got != "0" {
		t.Errorf("MySQL false = %q", got)
	}
}

func TestSQLLiteralBytes(t *testing.T) {
	binary := []byte{0xde, 0xad, 0xbe, 0xef}
	cases := map[model.DBKind]string{
		model.KindMySQL:    "X'deadbeef'",
		model.KindSQLite:   "X'deadbeef'",
		model.KindMSSQL:    "0xdeadbeef",
		model.KindPostgres: "decode('deadbeef', 'hex')",
		model.KindOracle:   "HEXTORAW('deadbeef')",
	}
	for kind, want := range cases {
		if got := SQLLiteral(kind, binary); got != want {
			t.Errorf("%s 바이너리 = %q, want %q", kind, got, want)
		}
	}

	// UTF-8로 읽히는 바이트열은 문자열로 본다. MySQL 드라이버는 TEXT 컬럼도
	// []byte로 주므로, 이것을 16진으로 쓰면 텍스트가 바이너리로 복구된다.
	if got := SQLLiteral(model.KindMySQL, []byte("안녕")); got != "'안녕'" {
		t.Errorf("UTF-8 바이트 = %q", got)
	}
	// 제어문자가 섞여 있으면 텍스트가 아니라고 본다.
	if got := SQLLiteral(model.KindSQLite, []byte{0x01, 0x02}); !strings.HasPrefix(got, "X'") {
		t.Errorf("제어문자 바이트 = %q", got)
	}
}

// Oracle의 HEXTORAW는 2000바이트가 상한이다. 넘는 값을 그대로 쓰면 복구가 실패하는데,
// 실패 지점이 파일 한가운데라 원인을 찾기 어렵다. 사실을 남기고 NULL로 둔다.
func TestSQLLiteralOracleLargeBinary(t *testing.T) {
	big := make([]byte, 3000)
	for i := range big {
		big[i] = byte(i % 251)
	}
	got := SQLLiteral(model.KindOracle, big)
	if !strings.HasPrefix(got, "NULL") || !strings.Contains(got, "3000") {
		t.Errorf("큰 바이너리 = %q", got)
	}
}

func TestSQLLiteralTime(t *testing.T) {
	when := time.Date(2026, 8, 14, 10, 30, 0, 0, time.UTC)

	// Oracle은 문자열을 날짜로 암묵 변환하지 않는다(NLS 설정에 좌우된다).
	got := SQLLiteral(model.KindOracle, when)
	if !strings.HasPrefix(got, "TO_TIMESTAMP(") {
		t.Errorf("Oracle 시각 = %q", got)
	}
	if got := SQLLiteral(model.KindPostgres, when); !strings.HasPrefix(got, "TIMESTAMP '") {
		t.Errorf("PostgreSQL 시각 = %q", got)
	}
	if got := SQLLiteral(model.KindMySQL, when); got != "'2026-08-14 10:30:00'" {
		t.Errorf("MySQL 시각 = %q", got)
	}
}

// NUL은 어느 DB도 문자열 안에 받아들이지 않는다. 남겨 두면 복구 전체가 실패하므로
// 그 문자만 뺀다 — 값 하나를 조금 잃는 것이 파일 전체를 못 쓰게 되는 것보다 낫다.
func TestSQLLiteralDropsNul(t *testing.T) {
	got := SQLLiteral(model.KindPostgres, "a\x00b")
	if strings.Contains(got, "\x00") {
		t.Errorf("NUL이 남았다: %q", got)
	}
	if got != "'ab'" {
		t.Errorf("NUL 제거 결과 = %q", got)
	}
}
