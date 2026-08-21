// Package sqlimport은 DDL 스크립트를 읽어 스키마 IR로 옮긴다.
//
// 왜 파서를 직접 쓰는가: 대상이 다섯 종류(MySQL·PostgreSQL·MS-SQL·Oracle·SQLite)이고
// 각각의 완전한 문법을 지원하는 Pure Go 라이브러리는 없다. 그리고 여기서 필요한
// 것은 완전한 파싱이 아니다 — **ERD로 그릴 수 있는 것**(테이블·컬럼·키·인덱스·
// 제약)만 읽어내면 되고, 나머지는 "해석하지 못했다"고 알리면 된다.
//
// 그래서 이 파서의 규칙은 하나다: **모르는 것은 조용히 버리지 않는다.**
// 해석하지 못한 문장은 Notes에 남아 화면에 그대로 보인다. 조용히 넘어가면
// 사용자는 불러오기가 끝난 뒤에야 테이블 몇 개가 빠진 것을 발견하게 된다.
package sqlimport

import (
	"strings"
	"unicode"
)

type tokenKind int

const (
	tWord   tokenKind = iota // 예약어와 인용하지 않은 식별자
	tIdent                   // 인용된 식별자 ("a", `a`, [a])
	tString                  // 문자열 리터럴
	tNumber
	tPunct    // ( ) , ; .
	tOperator // 나머지 기호
)

type token struct {
	kind tokenKind
	// text는 원문 그대로다. 기본값 식과 CHECK 식을 되살릴 때 쓴다.
	text string
	// val은 인용을 벗긴 값이다. 식별자 비교는 이 값으로 한다.
	val string
	// upper는 예약어 비교용이다. tWord에서만 채운다.
	upper string
	pos   int
}

func (t token) isWord(w string) bool { return t.kind == tWord && t.upper == w }

// isName은 이름으로 쓸 수 있는 토큰인지 본다.
// 예약어도 이름이 될 수 있다(`key`, `comment` 라는 컬럼은 흔하다).
func (t token) isName() bool { return t.kind == tWord || t.kind == tIdent }

func (t token) isPunct(p string) bool { return t.kind == tPunct && t.text == p }

// lex는 스크립트를 토큰으로 나눈다.
//
// dialect마다 인용 규칙이 조금씩 다르지만(백틱은 MySQL, 대괄호는 MS-SQL) 전부
// 받아들인다. 불러오는 SQL이 어디서 왔는지는 사용자도 늘 아는 것이 아니고,
// 남의 방언 인용부호를 만났다고 파싱을 포기하면 얻을 것이 없다.
func lex(src string) []token {
	out := []token{}
	n := len(src)
	i := 0

	for i < n {
		c := src[i]

		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			i++
			continue
		}

		// 줄 주석
		if (c == '-' && i+1 < n && src[i+1] == '-') || c == '#' {
			for i < n && src[i] != '\n' {
				i++
			}
			continue
		}
		// 블록 주석
		if c == '/' && i+1 < n && src[i+1] == '*' {
			i += 2
			for i < n && !(src[i] == '*' && i+1 < n && src[i+1] == '/') {
				i++
			}
			i = min(n, i+2)
			continue
		}

		// 달러 인용 (PostgreSQL 함수 본문). 통째로 문자열로 본다.
		if c == '$' {
			if tag, width := dollarTag(src[i:]); width > 0 {
				start := i
				closeAt := strings.Index(src[i+width:], tag)
				if closeAt < 0 {
					i = n
				} else {
					i = i + width + closeAt + len(tag)
				}
				out = append(out, token{kind: tString, text: src[start:i], val: src[start:i], pos: start})
				continue
			}
		}

		// 문자열
		if c == '\'' {
			start := i
			i++
			var b strings.Builder
			for i < n {
				if src[i] == '\\' && i+1 < n {
					b.WriteByte(src[i+1])
					i += 2
					continue
				}
				if src[i] == '\'' {
					if i+1 < n && src[i+1] == '\'' {
						b.WriteByte('\'')
						i += 2
						continue
					}
					i++
					break
				}
				b.WriteByte(src[i])
				i++
			}
			out = append(out, token{kind: tString, text: src[start:i], val: b.String(), pos: start})
			continue
		}

		// 인용된 식별자
		if c == '"' || c == '`' || c == '[' {
			closer := byte('"')
			switch c {
			case '`':
				closer = '`'
			case '[':
				closer = ']'
			}
			start := i
			i++
			var b strings.Builder
			for i < n {
				if src[i] == closer {
					if closer != ']' && i+1 < n && src[i+1] == closer {
						b.WriteByte(closer)
						i += 2
						continue
					}
					i++
					break
				}
				b.WriteByte(src[i])
				i++
			}
			out = append(out, token{kind: tIdent, text: src[start:i], val: b.String(), pos: start})
			continue
		}

		if c >= '0' && c <= '9' {
			start := i
			for i < n && (src[i] >= '0' && src[i] <= '9' || src[i] == '.' ||
				src[i] == 'e' || src[i] == 'E' ||
				((src[i] == '+' || src[i] == '-') && (src[i-1] == 'e' || src[i-1] == 'E'))) {
				i++
			}
			out = append(out, token{kind: tNumber, text: src[start:i], val: src[start:i], pos: start})
			continue
		}

		if isIdentStart(rune(c)) {
			start := i
			for i < n && isIdentPart(rune(src[i])) {
				i++
			}
			text := src[start:i]
			out = append(out, token{
				kind: tWord, text: text, val: text, upper: strings.ToUpper(text), pos: start,
			})
			continue
		}

		if strings.IndexByte("(),;.", c) >= 0 {
			out = append(out, token{kind: tPunct, text: string(c), val: string(c), pos: i})
			i++
			continue
		}

		start := i
		for i < n && strings.IndexByte("=<>!+-*/%|&^~:@?", src[i]) >= 0 {
			i++
		}
		if i == start {
			i++
		}
		out = append(out, token{kind: tOperator, text: src[start:i], val: src[start:i], pos: start})
	}
	return out
}

// isIdentStart는 식별자의 첫 글자로 쓸 수 있는지 본다.
// 다국어 식별자를 허용한다 — 한글 테이블명은 실제로 쓰인다.
func isIdentStart(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || r > 127
}

func isIdentPart(r rune) bool {
	return isIdentStart(r) || unicode.IsDigit(r) || r == '$'
}

// dollarTag는 $$ 또는 $tag$ 를 알아본다.
func dollarTag(s string) (string, int) {
	if len(s) == 0 || s[0] != '$' {
		return "", 0
	}
	for i := 1; i < len(s) && i < 64; i++ {
		if s[i] == '$' {
			return s[:i+1], i + 1
		}
		if !isIdentPart(rune(s[i])) {
			return "", 0
		}
	}
	return "", 0
}
