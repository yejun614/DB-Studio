package dblog

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"unicode"
)

// Normalize는 SQL을 그룹화 가능한 형태로 정규화한다.
//
// 목적: 리터럴만 다른 같은 쿼리를 하나로 묶어 "어떤 쿼리가 비싼가"를 집계한다.
// `SELECT * FROM users WHERE id = 1` 과 `... id = 2` 는 같은 쿼리로 봐야 한다.
//
// 수행하는 변환:
//  1. 주석 제거 (-- 줄 주석, /* */ 블록 주석)
//  2. 문자열 리터럴 → ?  (인용부호 안의 내용은 건드리지 않고 통째로 치환)
//  3. 숫자 리터럴 → ?
//  4. IN (?, ?, ?) → IN (?)   목록 길이만 다른 쿼리를 합친다
//  5. VALUES (?, ?), (?, ?) → VALUES (?)   벌크 INSERT를 합친다
//  6. 공백 정규화
//  7. 키워드 대문자화 (식별자는 원형 유지)
//
// 직접 구현하는 이유: DB가 다이제스트를 제공하는 경우(MySQL DIGEST_TEXT,
// PostgreSQL pg_stat_statements)에는 그것을 쓰지만, 원본 슬로우 로그나
// Redis SLOWLOG처럼 원문만 주는 소스는 직접 정규화해야 같은 축으로 집계된다.
func Normalize(sql string) string {
	if strings.TrimSpace(sql) == "" {
		return ""
	}
	s := stripComments(sql)
	s = replaceLiterals(s)
	s = collapseValueLists(s)
	s = collapseWhitespace(s)
	s = upperKeywords(s)
	return strings.TrimSpace(s)
}

// Digest는 정규화된 쿼리의 해시다. 같은 구조면 같은 값이 나온다.
func Digest(normalized string) string {
	if normalized == "" {
		return ""
	}
	// 대소문자와 후행 세미콜론 차이로 다른 다이제스트가 나오지 않게 한다.
	key := strings.ToLower(strings.TrimRight(strings.TrimSpace(normalized), ";"))
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:8])
}

// NormalizeAndDigest는 정규화와 해시를 함께 수행한다.
func NormalizeAndDigest(sql string) (string, string) {
	n := Normalize(sql)
	return n, Digest(n)
}

// stripComments는 SQL 주석을 제거한다.
// 문자열 리터럴 안의 `--` 나 `/*` 는 주석이 아니므로 인용 상태를 추적해야 한다.
func stripComments(s string) string {
	var b strings.Builder
	b.Grow(len(s))

	runes := []rune(s)
	var quote rune // 0이면 인용 밖

	for i := 0; i < len(runes); i++ {
		r := runes[i]

		if quote != 0 {
			b.WriteRune(r)
			switch {
			case r == '\\' && quote != '`' && i+1 < len(runes):
				// 백슬래시 이스케이프: 다음 문자를 그대로 통과시킨다.
				i++
				b.WriteRune(runes[i])
			case r == quote:
				// 같은 인용부호가 두 번 연속이면 이스케이프된 인용부호다.
				if i+1 < len(runes) && runes[i+1] == quote {
					i++
					b.WriteRune(runes[i])
				} else {
					quote = 0
				}
			}
			continue
		}

		switch {
		case r == '\'' || r == '"' || r == '`':
			quote = r
			b.WriteRune(r)

		case r == '-' && i+1 < len(runes) && runes[i+1] == '-':
			// 줄 주석: 개행까지 버린다. 개행은 토큰 구분을 위해 공백으로 남긴다.
			for i < len(runes) && runes[i] != '\n' {
				i++
			}
			b.WriteRune(' ')

		case r == '#' && i+1 < len(runes):
			// MySQL의 # 줄 주석
			for i < len(runes) && runes[i] != '\n' {
				i++
			}
			b.WriteRune(' ')

		case r == '/' && i+1 < len(runes) && runes[i+1] == '*':
			// 블록 주석: 닫는 */ 까지 버린다.
			i += 2
			for i+1 < len(runes) && !(runes[i] == '*' && runes[i+1] == '/') {
				i++
			}
			i++ // '/' 위치로 이동 (루프의 i++가 그 다음으로 넘긴다)
			b.WriteRune(' ')

		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// replaceLiterals는 문자열/숫자 리터럴을 ? 로 바꾼다.
func replaceLiterals(s string) string {
	var b strings.Builder
	b.Grow(len(s))

	runes := []rune(s)
	for i := 0; i < len(runes); i++ {
		r := runes[i]

		// 식별자 인용(`, ")은 리터럴이 아니므로 그대로 둔다.
		// 단, 표준 SQL에서 "는 식별자이고 MySQL에서는 문자열일 수 있다.
		// 문자열로 취급하면 컬럼명이 ?로 바뀌어 쿼리 구조가 뭉개지므로
		// 홑따옴표만 문자열 리터럴로 본다.
		if r == '\'' {
			i = skipQuoted(runes, i, '\'')
			b.WriteRune('?')
			continue
		}
		if r == '`' || r == '"' {
			end := skipQuoted(runes, i, r)
			for j := i; j <= end && j < len(runes); j++ {
				b.WriteRune(runes[j])
			}
			i = end
			continue
		}

		// 숫자 리터럴: 식별자 중간의 숫자(col2)는 건드리지 않는다.
		if isDigit(r) && !isIdentPart(prevRune(runes, i)) {
			i = skipNumber(runes, i)
			b.WriteRune('?')
			continue
		}
		// 부호 있는 숫자나 소수점으로 시작하는 숫자
		if (r == '.' || r == '-' || r == '+') && i+1 < len(runes) && isDigit(runes[i+1]) &&
			!isIdentPart(prevRune(runes, i)) {
			// '-'가 연산자인지 부호인지는 구분할 수 없지만, 어느 쪽이든
			// 뒤의 숫자는 ?로 바뀌어야 하므로 부호는 그대로 두고 숫자만 치환한다.
			if r != '.' {
				b.WriteRune(r)
				i++
			}
			i = skipNumber(runes, i)
			b.WriteRune('?')
			continue
		}

		b.WriteRune(r)
	}
	return b.String()
}

// skipQuoted는 여는 인용부호 위치에서 닫는 위치의 인덱스를 반환한다.
func skipQuoted(runes []rune, start int, quote rune) int {
	i := start + 1
	for i < len(runes) {
		switch {
		case runes[i] == '\\' && quote != '`' && i+1 < len(runes):
			i += 2
		case runes[i] == quote:
			// 두 번 연속이면 이스케이프
			if i+1 < len(runes) && runes[i+1] == quote {
				i += 2
				continue
			}
			return i
		default:
			i++
		}
	}
	return len(runes) - 1
}

// skipNumber는 숫자 리터럴의 마지막 인덱스를 반환한다.
// 16진수(0x1F), 지수 표기(1.5e-3)를 함께 처리한다.
func skipNumber(runes []rune, start int) int {
	i := start
	// 16진수
	if runes[i] == '0' && i+1 < len(runes) && (runes[i+1] == 'x' || runes[i+1] == 'X') {
		i += 2
		for i < len(runes) && isHexDigit(runes[i]) {
			i++
		}
		return i - 1
	}
	seenDot := false
	seenExp := false
	for i < len(runes) {
		r := runes[i]
		switch {
		case isDigit(r):
			i++
		case r == '.' && !seenDot && !seenExp:
			seenDot = true
			i++
		case (r == 'e' || r == 'E') && !seenExp && i+1 < len(runes) &&
			(isDigit(runes[i+1]) || ((runes[i+1] == '-' || runes[i+1] == '+') && i+2 < len(runes) && isDigit(runes[i+2]))):
			seenExp = true
			i++
			if runes[i] == '-' || runes[i] == '+' {
				i++
			}
		default:
			return i - 1
		}
	}
	return len(runes) - 1
}

// collapseValueLists는 길이만 다른 값 목록을 하나로 합친다.
//
// `IN (?, ?, ?)` → `IN (?)`, `VALUES (?, ?), (?, ?)` → `VALUES (?)`.
// 이 처리가 없으면 IN 목록 길이마다 다른 다이제스트가 생겨
// 사실상 같은 쿼리가 수백 개로 흩어진다.
func collapseValueLists(s string) string {
	out := s
	// (?, ?, ?) 형태를 (?)로 — 안쪽부터 반복 적용해 중첩도 처리한다.
	for {
		next := collapsePlaceholderGroup(out)
		if next == out {
			break
		}
		out = next
	}
	// VALUES (?), (?), (?) → VALUES (?)
	out = collapseRepeatedGroups(out)
	return out
}

// collapsePlaceholderGroup은 "(?, ?, …)" 를 "(?)"로 한 번 축약한다.
func collapsePlaceholderGroup(s string) string {
	runes := []rune(s)
	for i := 0; i < len(runes); i++ {
		if runes[i] != '(' {
			continue
		}
		// 여는 괄호 뒤가 "?, ?" 패턴인지 확인한다.
		j := i + 1
		count := 0
		valid := true
		for j < len(runes) {
			// 공백 건너뛰기
			for j < len(runes) && isSpace(runes[j]) {
				j++
			}
			if j < len(runes) && runes[j] == ')' {
				break
			}
			if j < len(runes) && runes[j] == '?' {
				count++
				j++
			} else {
				valid = false
				break
			}
			for j < len(runes) && isSpace(runes[j]) {
				j++
			}
			if j < len(runes) && runes[j] == ',' {
				j++
				continue
			}
			break
		}
		if !valid || count < 2 || j >= len(runes) || runes[j] != ')' {
			continue
		}
		return string(runes[:i]) + "(?)" + string(runes[j+1:])
	}
	return s
}

// collapseRepeatedGroups는 "(?), (?), (?)" 를 "(?)"로 축약한다.
func collapseRepeatedGroups(s string) string {
	for {
		idx := indexRepeatedGroup(s)
		if idx < 0 {
			return s
		}
		// "(?)" 다음의 ", (?)" 반복 구간을 잘라낸다.
		end := idx + 3
		for {
			k := end
			for k < len(s) && (s[k] == ' ' || s[k] == '\t' || s[k] == '\n') {
				k++
			}
			if k < len(s) && s[k] == ',' {
				k++
				for k < len(s) && (s[k] == ' ' || s[k] == '\t' || s[k] == '\n') {
					k++
				}
				if strings.HasPrefix(s[k:], "(?)") {
					end = k + 3
					continue
				}
			}
			break
		}
		if end == idx+3 {
			return s
		}
		s = s[:idx+3] + s[end:]
	}
}

// indexRepeatedGroup은 "(?)" 뒤에 ", (?)"가 이어지는 첫 위치를 찾는다.
func indexRepeatedGroup(s string) int {
	from := 0
	for {
		i := strings.Index(s[from:], "(?)")
		if i < 0 {
			return -1
		}
		i += from
		k := i + 3
		for k < len(s) && (s[k] == ' ' || s[k] == '\t' || s[k] == '\n') {
			k++
		}
		if k < len(s) && s[k] == ',' {
			k++
			for k < len(s) && (s[k] == ' ' || s[k] == '\t' || s[k] == '\n') {
				k++
			}
			if strings.HasPrefix(s[k:], "(?)") {
				return i
			}
		}
		from = i + 3
		if from >= len(s) {
			return -1
		}
	}
}

// collapseWhitespace는 연속 공백을 하나로 줄이고 구두점 주변을 정리한다.
func collapseWhitespace(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	lastSpace := false
	for _, r := range s {
		if isSpace(r) {
			if !lastSpace {
				b.WriteRune(' ')
				lastSpace = true
			}
			continue
		}
		lastSpace = false
		b.WriteRune(r)
	}
	out := b.String()
	// 콤마 앞 공백과 괄호 안쪽 공백은 의미가 없으므로 제거해
	// 포맷만 다른 쿼리가 같은 다이제스트를 갖게 한다.
	out = strings.ReplaceAll(out, " ,", ",")
	out = strings.ReplaceAll(out, "( ", "(")
	out = strings.ReplaceAll(out, " )", ")")
	return out
}

// sqlKeywords는 대문자화할 예약어다.
// 전부 담을 필요는 없다 — 구조를 읽는 데 필요한 것만 통일하면
// "select"와 "SELECT"가 같은 다이제스트를 갖는다.
var sqlKeywords = map[string]bool{
	"select": true, "from": true, "where": true, "and": true, "or": true, "not": true,
	"insert": true, "into": true, "values": true, "update": true, "set": true,
	"delete": true, "join": true, "left": true, "right": true, "inner": true,
	"outer": true, "full": true, "cross": true, "on": true, "using": true,
	"group": true, "by": true, "order": true, "having": true, "limit": true,
	"offset": true, "union": true, "all": true, "distinct": true, "as": true,
	"in": true, "is": true, "null": true, "like": true, "between": true,
	"exists": true, "case": true, "when": true, "then": true, "else": true, "end": true,
	"create": true, "alter": true, "drop": true, "table": true, "index": true,
	"view": true, "primary": true, "key": true, "foreign": true, "references": true,
	"begin": true, "commit": true, "rollback": true, "with": true, "returning": true,
	"asc": true, "desc": true, "count": true, "sum": true, "avg": true, "min": true,
	"max": true, "inner_join": true, "for": true, "lock": true, "share": true,
}

// upperKeywords는 예약어만 대문자로 만든다. 식별자는 원형을 유지해
// 어떤 테이블/컬럼인지 읽을 수 있게 한다.
func upperKeywords(s string) string {
	var b strings.Builder
	b.Grow(len(s))

	runes := []rune(s)
	i := 0
	for i < len(runes) {
		r := runes[i]

		// 인용된 식별자/리터럴은 통째로 통과시킨다.
		if r == '\'' || r == '"' || r == '`' {
			end := skipQuoted(runes, i, r)
			for j := i; j <= end && j < len(runes); j++ {
				b.WriteRune(runes[j])
			}
			i = end + 1
			continue
		}

		if !isIdentStart(r) {
			b.WriteRune(r)
			i++
			continue
		}
		start := i
		for i < len(runes) && isIdentPart(runes[i]) {
			i++
		}
		word := string(runes[start:i])
		if sqlKeywords[strings.ToLower(word)] {
			b.WriteString(strings.ToUpper(word))
		} else {
			b.WriteString(word)
		}
	}
	return b.String()
}

// ---------- 문자 분류 ----------

func isSpace(r rune) bool {
	return r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '\v' || r == '\f'
}
func isDigit(r rune) bool { return r >= '0' && r <= '9' }

func isHexDigit(r rune) bool {
	return isDigit(r) || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
}

func isIdentStart(r rune) bool {
	return unicode.IsLetter(r) || r == '_' || r == '$'
}

func isIdentPart(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '$'
}

func prevRune(runes []rune, i int) rune {
	if i == 0 {
		return ' '
	}
	return runes[i-1]
}

// FirstLine은 여러 줄 메시지의 첫 줄만 반환한다. 목록 표시에 쓴다.
func FirstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}
