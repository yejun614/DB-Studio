package dblog

import (
	"strings"
	"testing"
	"time"
)

// TestNormalize는 SQL 정규화가 리터럴만 다른 쿼리를 같은 형태로 만드는지 확인한다.
func TestNormalize(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			"숫자 리터럴",
			"SELECT * FROM users WHERE id = 42",
			"SELECT * FROM users WHERE id = ?",
		},
		{
			"문자열 리터럴",
			"SELECT * FROM users WHERE email = 'a@example.com'",
			"SELECT * FROM users WHERE email = ?",
		},
		{
			"여러 리터럴",
			"UPDATE users SET name = 'kim', age = 30 WHERE id = 7",
			"UPDATE users SET name = ?, age = ? WHERE id = ?",
		},
		{
			"소수와 음수",
			"SELECT * FROM t WHERE price > 10.5 AND delta = -3",
			"SELECT * FROM t WHERE price > ? AND delta = -?",
		},
		{
			"지수 표기",
			"SELECT * FROM t WHERE x = 1.5e-3",
			"SELECT * FROM t WHERE x = ?",
		},
		{
			"16진수",
			"SELECT * FROM t WHERE mask = 0xFF",
			"SELECT * FROM t WHERE mask = ?",
		},
		{
			"IN 목록 축약",
			"SELECT * FROM t WHERE id IN (1, 2, 3, 4, 5)",
			"SELECT * FROM t WHERE id IN (?)",
		},
		{
			"IN 목록 문자열",
			"SELECT * FROM t WHERE code IN ('a', 'b', 'c')",
			"SELECT * FROM t WHERE code IN (?)",
		},
		{
			"단일 값 IN은 그대로",
			"SELECT * FROM t WHERE id IN (1)",
			"SELECT * FROM t WHERE id IN (?)",
		},
		{
			"벌크 INSERT 축약",
			"INSERT INTO t (a, b) VALUES (1, 'x'), (2, 'y'), (3, 'z')",
			"INSERT INTO t (a, b) VALUES (?)",
		},
		{
			"줄 주석 제거",
			"SELECT id -- 사용자 식별자\nFROM users WHERE id = 1",
			"SELECT id FROM users WHERE id = ?",
		},
		{
			"블록 주석 제거",
			"SELECT /* hint */ id FROM users WHERE id = 1",
			"SELECT id FROM users WHERE id = ?",
		},
		{
			"MySQL # 주석",
			"SELECT id FROM users # 주석\nWHERE id = 1",
			"SELECT id FROM users WHERE id = ?",
		},
		{
			"공백 정규화",
			"SELECT   *\n\tFROM    users\n  WHERE  id  =  1",
			"SELECT * FROM users WHERE id = ?",
		},
		{
			"키워드 대문자화",
			"select * from users where id = 1",
			"SELECT * FROM users WHERE id = ?",
		},
		{
			"식별자는 원형 유지",
			"SELECT userName, createdAt FROM AppUsers WHERE userId = 1",
			"SELECT userName, createdAt FROM AppUsers WHERE userId = ?",
		},
		{
			"인용된 식별자 보존",
			"SELECT `user name` FROM `my table` WHERE id = 1",
			"SELECT `user name` FROM `my table` WHERE id = ?",
		},
		{
			"큰따옴표 식별자 보존",
			`SELECT "userName" FROM "AppUsers" WHERE id = 1`,
			`SELECT "userName" FROM "AppUsers" WHERE id = ?`,
		},
		{
			"식별자 안의 숫자는 유지",
			"SELECT col2, addr1 FROM t1 WHERE id = 5",
			"SELECT col2, addr1 FROM t1 WHERE id = ?",
		},
		{
			"문자열 안의 주석 기호는 주석이 아니다",
			"SELECT * FROM t WHERE note = 'a -- b /* c */'",
			"SELECT * FROM t WHERE note = ?",
		},
		{
			"문자열 안의 인용부호 이스케이프",
			"SELECT * FROM t WHERE s = 'it''s'",
			"SELECT * FROM t WHERE s = ?",
		},
		{
			"백슬래시 이스케이프",
			`SELECT * FROM t WHERE s = 'a\'b'`,
			"SELECT * FROM t WHERE s = ?",
		},
		{
			"JOIN 구문",
			"select u.id from users u left join orders o on o.user_id = u.id where u.id = 3",
			"SELECT u.id FROM users u LEFT JOIN orders o ON o.user_id = u.id WHERE u.id = ?",
		},
		{
			"LIMIT/OFFSET",
			"SELECT * FROM t ORDER BY id DESC LIMIT 20 OFFSET 100",
			"SELECT * FROM t ORDER BY id DESC LIMIT ? OFFSET ?",
		},
		{
			"빈 문자열",
			"",
			"",
		},
		{
			"공백만",
			"   \n\t ",
			"",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Normalize(c.in)
			if got != c.want {
				t.Errorf("입력:\n  %q\n결과:\n  %q\n기대:\n  %q", c.in, got, c.want)
			}
		})
	}
}

// TestNormalizeGroupsEquivalentQueries는 정규화의 실제 목적을 확인한다:
// 리터럴만 다른 쿼리들이 하나의 다이제스트로 묶여야 한다.
func TestNormalizeGroupsEquivalentQueries(t *testing.T) {
	groups := [][]string{
		{
			"SELECT * FROM users WHERE id = 1",
			"SELECT * FROM users WHERE id = 2",
			"SELECT * FROM users WHERE id = 99999",
			"select * from users where id = 3",
			"SELECT  *  FROM  users  WHERE  id = 4",
			"SELECT * FROM users WHERE id = 5 -- 주석",
		},
		{
			"SELECT * FROM t WHERE id IN (1,2)",
			"SELECT * FROM t WHERE id IN (1,2,3,4,5,6,7,8,9,10)",
			"SELECT * FROM t WHERE id IN (100)",
		},
		{
			"INSERT INTO logs (msg) VALUES ('a')",
			"INSERT INTO logs (msg) VALUES ('completely different message')",
		},
	}

	for gi, group := range groups {
		digests := map[string][]string{}
		for _, q := range group {
			n, d := NormalizeAndDigest(q)
			digests[d] = append(digests[d], n)
		}
		if len(digests) != 1 {
			t.Errorf("그룹 %d: 같은 구조인데 다이제스트가 %d개로 갈렸습니다", gi, len(digests))
			for d, ns := range digests {
				t.Errorf("  %s → %q", d, ns[0])
			}
		}
	}
}

// TestNormalizeDistinguishesDifferentQueries는 구조가 다른 쿼리가
// 합쳐지지 않는지 확인한다. 과도한 정규화는 분석을 무의미하게 만든다.
func TestNormalizeDistinguishesDifferentQueries(t *testing.T) {
	queries := []string{
		"SELECT * FROM users WHERE id = 1",
		"SELECT * FROM orders WHERE id = 1",        // 다른 테이블
		"SELECT id FROM users WHERE id = 1",        // 다른 컬럼
		"SELECT * FROM users WHERE email = 'x'",    // 다른 조건 컬럼
		"UPDATE users SET name = 'x' WHERE id = 1", // 다른 구문
		"DELETE FROM users WHERE id = 1",
		"SELECT * FROM users WHERE id = 1 ORDER BY name",
	}

	seen := map[string]string{}
	for _, q := range queries {
		_, d := NormalizeAndDigest(q)
		if prev, ok := seen[d]; ok {
			t.Errorf("구조가 다른 쿼리가 같은 다이제스트를 가집니다:\n  %q\n  %q", prev, q)
		}
		seen[d] = q
	}
}

// TestDigestStability는 다이제스트가 사소한 차이에 흔들리지 않는지 확인한다.
func TestDigestStability(t *testing.T) {
	base := "SELECT * FROM users WHERE id = ?"
	variants := []string{
		base,
		base + ";",
		strings.ToLower(base),
		"  " + base + "  ",
	}
	first := Digest(variants[0])
	for _, v := range variants[1:] {
		if got := Digest(v); got != first {
			t.Errorf("다이제스트가 불안정합니다: %q → %s (기준 %s)", v, got, first)
		}
	}
	if Digest("") != "" {
		t.Error("빈 문자열의 다이제스트는 빈 문자열이어야 합니다")
	}
}

// TestNormalizeDoesNotPanic은 비정상 입력에도 죽지 않는지 확인한다.
// 로그에서 온 쿼리는 잘려 있거나 인용부호가 닫히지 않았을 수 있다.
func TestNormalizeDoesNotPanic(t *testing.T) {
	inputs := []string{
		"SELECT * FROM t WHERE s = 'unclosed",
		"SELECT * FROM t WHERE s = \"unclosed",
		"SELECT * FROM `unclosed",
		"/* unclosed block comment",
		"SELECT * FROM t WHERE id = ",
		"((((((",
		"))))))",
		"?,?,?,?",
		"0x",
		"1.2.3.4",
		"1e",
		"--",
		"#",
		"'",
		strings.Repeat("SELECT ", 500),
		strings.Repeat("(?, ", 200) + "?)",
		"SELECT * FROM t WHERE 한글컬럼 = '한글값'",
		"\x00\x01\x02",
	}
	for _, in := range inputs {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("입력 %q 에서 panic: %v", truncateForMsg(in), r)
				}
			}()
			n := Normalize(in)
			_ = Digest(n)
		}()
	}
}

// TestNormalizeUnicodeIdentifiers는 한글 등 비ASCII 식별자를 보존하는지 확인한다.
func TestNormalizeUnicodeIdentifiers(t *testing.T) {
	got := Normalize("SELECT 이름, 나이 FROM 사용자 WHERE 아이디 = 5")
	want := "SELECT 이름, 나이 FROM 사용자 WHERE 아이디 = ?"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestNormalizeNestedPlaceholders는 중첩 괄호에서도 축약이 동작하는지 확인한다.
func TestNormalizeNestedPlaceholders(t *testing.T) {
	got := Normalize("SELECT * FROM t WHERE (a, b) IN ((1, 2), (3, 4))")
	// (?, ?) → (?) 가 두 번 적용되고, 반복 그룹이 하나로 합쳐진다.
	if !strings.Contains(got, "IN ((?))") && !strings.Contains(got, "IN (?)") {
		t.Errorf("중첩 목록이 축약되지 않았습니다: %q", got)
	}
	t.Logf("결과: %q", got)
}

// ---------- 필터 / 정렬 ----------

func TestFilterDefaults(t *testing.T) {
	var f Filter
	f.Normalize()
	if f.To.IsZero() || f.From.IsZero() {
		t.Error("시간 범위 기본값이 채워지지 않았습니다")
	}
	if f.To.Sub(f.From) != time.Hour {
		t.Errorf("기본 범위가 1시간이어야 합니다: %v", f.To.Sub(f.From))
	}
	if f.StatsOrderBy != "total" {
		t.Errorf("기본 정렬이 total이어야 합니다: %s", f.StatsOrderBy)
	}
	if f.EffectiveLimit() != 200 {
		t.Errorf("기본 상한이 200이어야 합니다: %d", f.EffectiveLimit())
	}
	f.Limit = 99999
	if f.EffectiveLimit() != 2000 {
		t.Errorf("상한이 2000으로 제한되어야 합니다: %d", f.EffectiveLimit())
	}
}

func TestFilterWantsSource(t *testing.T) {
	var all Filter
	if !all.WantsSource(SourceSlowQuery) || !all.WantsSource(SourceErrorLog) {
		t.Error("소스를 지정하지 않으면 전부 조회해야 합니다")
	}
	only := Filter{Sources: []SourceKind{SourceSlowQuery}}
	if !only.WantsSource(SourceSlowQuery) {
		t.Error("지정한 소스는 조회해야 합니다")
	}
	if only.WantsSource(SourceErrorLog) {
		t.Error("지정하지 않은 소스는 건너뛰어야 합니다")
	}
}

func TestSortStats(t *testing.T) {
	stats := []QueryStat{
		{Digest: "a", Calls: 100, TotalMs: 500, MeanMs: 5, MaxMs: 20},
		{Digest: "b", Calls: 2, TotalMs: 1000, MeanMs: 500, MaxMs: 900},
		{Digest: "c", Calls: 1000, TotalMs: 800, MeanMs: 0.8, MaxMs: 5},
	}

	cases := []struct {
		orderBy string
		wantTop string
	}{
		{"total", "b"}, // 총 소요 시간이 가장 큼
		{"mean", "b"},  // 평균이 가장 느림
		{"calls", "c"}, // 호출이 가장 많음
		{"max", "b"},   // 최대가 가장 큼
		{"", "b"},      // 기본값 = total
		{"bogus", "b"}, // 알 수 없는 값도 total로 취급
	}
	for _, c := range cases {
		cp := append([]QueryStat(nil), stats...)
		SortStats(cp, c.orderBy)
		if cp[0].Digest != c.wantTop {
			t.Errorf("orderBy=%q → 1위 %s, 기대 %s", c.orderBy, cp[0].Digest, c.wantTop)
		}
	}
}

func TestValidStatsOrder(t *testing.T) {
	for _, v := range []string{"", "total", "mean", "calls", "max"} {
		if !ValidStatsOrder(v) {
			t.Errorf("%q는 유효해야 합니다", v)
		}
	}
	for _, v := range []string{"bogus", "TOTAL", "sum"} {
		if ValidStatsOrder(v) {
			t.Errorf("%q는 무효해야 합니다", v)
		}
	}
}

func TestSeverityRank(t *testing.T) {
	if SeverityError.Rank() <= SeverityWarning.Rank() {
		t.Error("error가 warning보다 높아야 합니다")
	}
	if SeverityFatal.Rank() <= SeverityError.Rank() {
		t.Error("fatal이 error보다 높아야 합니다")
	}
	if Severity("bogus").Rank() != 0 {
		t.Error("알 수 없는 심각도는 0이어야 합니다")
	}
	for _, s := range []Severity{SeverityDebug, SeverityInfo, SeverityWarning, SeverityError, SeverityFatal} {
		if !s.Valid() {
			t.Errorf("%s는 유효해야 합니다", s)
		}
	}
	if Severity("bogus").Valid() {
		t.Error("알 수 없는 심각도는 무효해야 합니다")
	}
}

// ---------- Result 헬퍼 ----------

func TestResultHelpers(t *testing.T) {
	r := NewResult()
	r.AddNote("첫 번째")
	r.AddNote("첫 번째") // 중복
	r.AddNote("두 번째")
	if len(r.Notes) != 2 {
		t.Errorf("중복 노트가 제거되지 않았습니다: %v", r.Notes)
	}

	r.MarkSource(SourceSlowQuery, "슬로우", true, 5, "")
	r.MarkSource(SourceSlowQuery, "슬로우", false, 0, "활성화 필요")
	if len(r.Sources) != 1 {
		t.Errorf("같은 소스가 중복 등록되었습니다: %d", len(r.Sources))
	}
	if r.Sources[0].Available || r.Sources[0].Hint != "활성화 필요" {
		t.Errorf("소스 상태가 갱신되지 않았습니다: %+v", r.Sources[0])
	}

	base := time.Now().UTC()
	r.Entries = []Entry{
		{At: base.Add(-2 * time.Minute), Message: "오래된 것"},
		{At: base, Message: "최신"},
		{At: base.Add(-time.Minute), Message: "중간"},
	}
	r.SortEntries()
	if r.Entries[0].Message != "최신" || r.Entries[2].Message != "오래된 것" {
		t.Errorf("최근 항목이 먼저 오지 않았습니다: %v", []string{
			r.Entries[0].Message, r.Entries[1].Message, r.Entries[2].Message,
		})
	}
}

func TestTruncateQuery(t *testing.T) {
	short := "SELECT 1"
	if TruncateQuery(short, 100) != short {
		t.Error("짧은 쿼리는 그대로 반환해야 합니다")
	}
	long := strings.Repeat("x", 500)
	got := TruncateQuery(long, 100)
	if len(got) >= 500 || !strings.HasSuffix(got, "(잘림)") {
		t.Errorf("긴 쿼리가 잘리지 않았습니다: len=%d", len(got))
	}
}

func TestFirstLine(t *testing.T) {
	if got := FirstLine("첫 줄\n둘째 줄"); got != "첫 줄" {
		t.Errorf("got %q", got)
	}
	if got := FirstLine("  단일 줄  "); got != "단일 줄" {
		t.Errorf("got %q", got)
	}
	if got := FirstLine("a\r\nb"); got != "a" {
		t.Errorf("got %q", got)
	}
}

func truncateForMsg(s string) string {
	if len(s) > 60 {
		return s[:60] + "…"
	}
	return s
}
