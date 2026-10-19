package sqlimport

import (
	"fmt"
	"strings"

	"dbstudio/internal/schema"
)

// PostgreSQL 의 이름 붙은 ENUM.
//
// CREATE TYPE … AS ENUM 을 읽지 않으면 그 타입을 쓰는 컬럼은 "order_status" 라는
// 글자만 남고, 초안을 내보낼 때 CREATE TYPE 이 빠져 스크립트가 첫 표에서 멈춘다.
// pg_dump 출력과 마이그레이션 스크립트 모두 이 문장으로 시작하므로 빠뜨릴 수 없다.

// createType은 CREATE TYPE 문장을 읽는다. ENUM 만 담는다 — 복합 타입·범위 타입은
// 컬럼 하나의 타입으로 그릴 수 없다.
func (p *parser) createType(start int) {
	ns, name, ok := p.qualifiedName()
	if !ok {
		p.note(start, "타입 이름을 읽지 못했습니다")
		p.skipToStatementEnd()
		return
	}
	if !p.accept("AS", "ENUM") {
		p.note(start, "ENUM 이 아닌 타입(복합·범위 등)은 ERD 에 담지 않습니다")
		p.skipToStatementEnd()
		return
	}
	values, ok := p.stringList()
	if !ok || len(values) == 0 {
		p.note(start, "ENUM 값 목록을 읽지 못했습니다")
		p.skipToStatementEnd()
		return
	}
	e := &schema.Enum{Namespace: ns, Name: name, Values: values}
	for i, old := range p.res.Enums {
		if old.Key() == e.Key() {
			p.res.Enums[i] = e
			p.skipToStatementEnd()
			return
		}
	}
	p.res.Enums = append(p.res.Enums, e)
	p.skipToStatementEnd()
}

// alterType은 ALTER TYPE … ADD VALUE 를 읽는다. 마이그레이션 스크립트가 값을 더하는
// 방법이 이것뿐이라, 읽지 않으면 불러온 초안의 값 목록이 실제보다 짧다.
func (p *parser) alterType(start int) {
	ns, name, ok := p.qualifiedName()
	if !ok {
		p.note(start, "타입 이름을 읽지 못했습니다")
		p.skipToStatementEnd()
		return
	}
	var target *schema.Enum
	for _, e := range p.res.Enums {
		if e.Key() == key(ns, name) {
			target = e
		}
	}
	if target == nil || !p.accept("ADD", "VALUE") {
		if target == nil {
			p.note(start, fmt.Sprintf("이 스크립트에 %s 의 정의가 없어 반영하지 못했습니다", display(ns, name)))
		} else {
			p.note(start, "ENUM 값 추가(ADD VALUE) 밖의 변경은 반영하지 않았습니다")
		}
		p.skipToStatementEnd()
		return
	}
	p.accept("IF", "NOT", "EXISTS")
	v := p.peek()
	if v.kind != tString {
		p.note(start, "추가할 ENUM 값을 읽지 못했습니다")
		p.skipToStatementEnd()
		return
	}
	p.i++
	value := v.val
	for _, cur := range target.Values {
		if cur == value {
			p.skipToStatementEnd()
			return
		}
	}

	// BEFORE/AFTER 가 있으면 그 자리에 넣는다. 값의 순서가 곧 정렬 순서다.
	at := len(target.Values)
	before := p.accept("BEFORE")
	if before || p.accept("AFTER") {
		if ref := p.peek(); ref.kind == tString {
			p.i++
			for i, cur := range target.Values {
				if cur == ref.val {
					at = i
					if !before {
						at = i + 1
					}
				}
			}
		}
	}
	target.Values = append(target.Values[:at], append([]string{value}, target.Values[at:]...)...)
	p.skipToStatementEnd()
}

// stringList는 ( 'a', 'b' ) 를 읽는다.
func (p *parser) stringList() ([]string, bool) {
	if !p.acceptPunct("(") {
		return nil, false
	}
	out := []string{}
	for !p.done() {
		if p.acceptPunct(")") {
			return out, true
		}
		t := p.next()
		switch {
		case t.kind == tString:
			out = append(out, t.val)
		case t.isPunct(","):
		default:
			return nil, false
		}
	}
	return nil, false
}

// resolveEnumColumns는 이름 붙은 ENUM 을 가리키는 컬럼의 타입을 그 ENUM 으로 바꾼다.
//
// 컬럼을 읽는 순간에는 이름이 ENUM 인지 알 수 없다(타입 정의가 뒤에 올 수도 있다).
// 다 읽은 뒤에 한 번 맞춘다. 값도 함께 담는다 — 그래야 이 초안을 MySQL 로 내보낼 때
// ENUM(...) 이 된다.
func (p *parser) resolveEnumColumns() {
	if len(p.res.Enums) == 0 {
		return
	}
	for _, t := range p.res.Tables {
		for _, c := range t.Columns {
			e := p.enumFor(t.Namespace, c.RawType)
			if e == nil {
				continue
			}
			c.Type = schema.LogicalType{
				Base: schema.TypeEnum, EnumName: e.Name,
				Values: append([]string(nil), e.Values...),
			}
		}
	}
}

// enumFor는 컬럼 타입 글자가 가리키는 ENUM 이다. "public"."order_status" 처럼
// 인용·스키마가 붙은 표기도 받는다.
func (p *parser) enumFor(tableNS, raw string) *schema.Enum {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.ContainsAny(raw, "([") {
		return nil
	}
	parts := strings.Split(raw, ".")
	for i, part := range parts {
		parts[i] = strings.Trim(strings.TrimSpace(part), "\"`")
	}
	name := parts[len(parts)-1]
	ns := tableNS
	if len(parts) > 1 {
		ns = parts[len(parts)-2]
	}
	var loose *schema.Enum
	for _, e := range p.res.Enums {
		if !strings.EqualFold(e.Name, name) {
			continue
		}
		if strings.EqualFold(e.Namespace, ns) {
			return e
		}
		loose = e
	}
	return loose
}
