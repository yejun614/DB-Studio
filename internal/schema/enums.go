package schema

import (
	"fmt"
	"strings"
)

// ENUM 을 DB 종류에 맞는 모양으로 바꾼다.
//
// ENUM 은 DB 마다 사는 곳이 다르다. PostgreSQL 에서는 이름 붙은 타입(CREATE TYPE …
// AS ENUM)이고 컬럼은 그 이름을 가리킨다. MySQL·ClickHouse 에서는 컬럼 타입 안에
// 값 목록이 들어 있고, 나머지 DB 에는 ENUM 이 없어 문자열 + CHECK 로 지킨다.
//
// 초안은 어느 DB 로도 내보낼 수 있어야 하므로 두 표현이 섞여 있다: 도메인이나 실제
// DB 에서 온 컬럼은 이름(EnumName)과 값(Values)을 함께 들고, 타입 칸에 값 목록을 바로
// 적은 컬럼은 값만 든다. 비교·DDL 생성 직전에 한쪽으로 모은다 — 그래야 같은 설계를
// 같은 DB 와 견줄 때 "같다"가 나온다.

// IsSetType은 MySQL 의 SET 인지다. SET 은 ENUM 과 같은 논리 타입으로 읽히지만 값을
// 여럿 품으므로('a,b') 이름 붙은 ENUM 이나 IN (...) 제약으로 옮기면 멀쩡한 값이 거부된다.
func IsSetType(raw string) bool {
	s := strings.ToLower(strings.TrimSpace(raw))
	if !strings.HasPrefix(s, "set") {
		return false
	}
	return strings.HasPrefix(strings.TrimSpace(s[len("set"):]), "(")
}

// EnumTypeString은 값 목록을 ENUM('a', 'b') 표기로 쓴다. 도메인의 타입 칸처럼
// 사람이 읽고 다시 해석되는 자리에 쓴다.
func EnumTypeString(values []string) string {
	return "ENUM(" + quoteList(values, "'") + ")"
}

// enumsFor는 sc 를 dialect 의 ENUM 모양으로 맞춘 사본을 돌려준다. 바꿀 것이 없으면
// sc 를 그대로 돌려준다.
func enumsFor(sc *Schema, dialect string) *Schema {
	if sc == nil || !hasEnums(sc) {
		return sc
	}
	out := sc.Clone()
	if dialect == "postgres" {
		nameInlineEnums(out)
	} else {
		inlineNamedEnums(out)
	}
	return out
}

func hasEnums(sc *Schema) bool {
	if len(sc.Enums) > 0 {
		return true
	}
	for _, t := range sc.Tables {
		for _, c := range t.Columns {
			if c.Type.Base == TypeEnum {
				return true
			}
		}
	}
	return false
}

// findEnum은 이름으로 ENUM 을 찾는다. 같은 네임스페이스의 것을 먼저 고른다 — 컬럼의
// EnumName 에는 네임스페이스가 없다.
func findEnum(sc *Schema, ns, name string) *Enum {
	var loose *Enum
	for _, e := range sc.Enums {
		if !strings.EqualFold(e.Name, name) {
			continue
		}
		if strings.EqualFold(e.Namespace, ns) {
			return e
		}
		if loose == nil {
			loose = e
		}
	}
	return loose
}

// nameInlineEnums는 PostgreSQL 용이다. 값만 든 컬럼에 타입 이름을 지어 주고, 모든
// ENUM 컬럼이 Enums 의 타입 하나를 가리키게 한다.
//
// 이름은 도메인이 있으면 도메인 이름, 없으면 <표>_<컬럼>이다. 사람이 이름을 정하려면
// 도메인을 만들면 된다. 같은 이름이 다른 값 목록으로 이미 쓰이고 있으면 뒤에 _enum,
// _2 … 를 붙인다(한 타입을 두 값 목록이 나눠 쓸 수는 없다).
func nameInlineEnums(sc *Schema) {
	// 네임스페이스 없이 만든 타입(도메인)을 어느 스키마에 둘지 정하려고 쓰는 표의
	// 네임스페이스를 모은다.
	usedIn := map[*Enum]map[string]bool{}
	use := func(e *Enum, ns string) {
		if usedIn[e] == nil {
			usedIn[e] = map[string]bool{}
		}
		usedIn[e][strings.ToLower(ns)] = true
	}

	for _, t := range sc.Tables {
		for _, c := range t.Columns {
			if c.Type.Base != TypeEnum || IsSetType(c.RawType) {
				continue
			}
			if c.Type.EnumName != "" {
				e := findEnum(sc, t.Namespace, c.Type.EnumName)
				if e == nil && len(c.Type.Values) > 0 {
					e = &Enum{Name: c.Type.EnumName, Values: append([]string(nil), c.Type.Values...)}
					sc.Enums = append(sc.Enums, e)
				}
				if e != nil {
					use(e, t.Namespace)
				}
				continue
			}
			if len(c.Type.Values) == 0 {
				continue
			}
			base := c.Domain
			if base == "" {
				base = t.Name + "_" + c.Name
			}
			e := claimEnumName(sc, t.Namespace, base, c.Type.Values)
			c.Type.EnumName = e.Name
			use(e, t.Namespace)
		}
	}

	for e, nss := range usedIn {
		if e.Namespace != "" || len(nss) != 1 {
			continue
		}
		for ns := range nss {
			e.Namespace = ns
		}
	}
}

// claimEnumName은 base 이름의 타입을 찾거나 만든다. 같은 이름이 같은 값 목록이면
// 그것을 같이 쓴다.
func claimEnumName(sc *Schema, ns, base string, values []string) *Enum {
	for n := 1; ; n++ {
		name := base
		switch {
		case n == 2:
			name = base + "_enum"
		case n > 2:
			name = fmt.Sprintf("%s_%d", base, n-1)
		}
		e := findEnum(sc, ns, name)
		if e == nil {
			e = &Enum{Name: name, Values: append([]string(nil), values...)}
			sc.Enums = append(sc.Enums, e)
			return e
		}
		if sameValues(e.Values, values) {
			return e
		}
	}
}

func sameValues(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// inlineNamedEnums는 PostgreSQL 밖의 DB 용이다. 이름 붙은 타입이 없으므로 컬럼이
// 값 목록을 직접 들게 하고 타입 목록은 비운다.
//
// 이름을 지우는 것이 중요하다. 이름이 남아 있으면 비교가 이름으로 견주어, 도메인의
// 값을 바꿔도 MySQL 컬럼은 "그대로"라고 판정된다.
func inlineNamedEnums(sc *Schema) {
	for _, t := range sc.Tables {
		for _, c := range t.Columns {
			if c.Type.Base != TypeEnum || c.Type.EnumName == "" {
				continue
			}
			if len(c.Type.Values) == 0 {
				if e := findEnum(sc, t.Namespace, c.Type.EnumName); e != nil {
					c.Type.Values = append([]string(nil), e.Values...)
				}
			}
			if len(c.Type.Values) > 0 {
				c.Type.EnumName = ""
			}
		}
	}
	sc.Enums = nil
}
