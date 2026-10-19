package erd

import (
	"strings"

	"dbstudio/internal/schema"
)

// ENUM 도메인.
//
// 타입이 값 목록 ENUM('a','b') 인 도메인은 이름 붙은 ENUM 타입 하나를 **소유한다**.
// 이름이 같고, 값이 같고, 그 도메인을 쓰는 컬럼은 그 타입을 가리킨다(EnumName).
// PostgreSQL 로 내보내면 그것이 CREATE TYPE <도메인> AS ENUM 이 되고, 같은 도메인의
// 컬럼은 그 타입 하나를 같이 쓴다. 다른 DB 로 내보내면 비교가 이름을 지우고 값 목록만
// 남긴다(schema.enumsFor) — 그래서 도메인은 어느 DB 로든 같은 뜻으로 옮겨 간다.
//
// 타입은 Schema.Enums 에 산다. 도메인과 따로 두는 것은 실제 DB 에서 온 타입이 있기
// 때문이다: 도메인 없이도 존재하고, 아무 컬럼도 쓰지 않아도 DB 에 있다. 그것까지
// 도메인으로만 들고 있으면 도메인을 지우는 순간 마이그레이션이 DROP TYPE 을 낸다.

// ownedEnumValues는 도메인이 소유하는 ENUM 의 값이다. ENUM 도메인이 아니면 nil 이다.
//
// 타입 칸에 다른 ENUM 의 이름을 적은 도메인(별칭)은 소유자가 아니다 — 그 타입의
// 주인은 따로 있다.
func ownedEnumValues(doc *Document, d *Domain) []string {
	if schema.IsSetType(d.Type) {
		return nil
	}
	lt, _, err := parseColumnType(doc, d.Type)
	if err != nil || lt.Base != schema.TypeEnum || lt.EnumName != "" || len(lt.Values) == 0 {
		return nil
	}
	return lt.Values
}

// findSchemaEnum은 이름으로 ENUM 타입을 찾는다(대소문자 무시). 컬럼의 EnumName 에
// 네임스페이스가 없으므로 이름만 본다.
func findSchemaEnum(sc *schema.Schema, name string) *schema.Enum {
	for _, e := range sc.Enums {
		if strings.EqualFold(e.Name, strings.TrimSpace(name)) {
			return e
		}
	}
	return nil
}

func enumInUse(doc *Document, name string) bool {
	for _, t := range doc.Schema.Tables {
		for _, c := range t.Columns {
			if c.Type.Base == schema.TypeEnum && strings.EqualFold(c.Type.EnumName, name) {
				return true
			}
		}
	}
	return false
}

func removeSchemaEnum(sc *schema.Schema, target *schema.Enum) {
	kept := sc.Enums[:0]
	for _, e := range sc.Enums {
		if e != target {
			kept = append(kept, e)
		}
	}
	sc.Enums = kept
}

// checkDomainEnumName은 ENUM 도메인의 이름이 다른 ENUM 타입과 겹치지 않는지 본다.
// oldName 의 타입(이 도메인이 이미 가진 것)은 겹침이 아니다.
func checkDomainEnumName(doc *Document, oldName, newName string) error {
	other := findSchemaEnum(doc.Schema, newName)
	if other == nil || strings.EqualFold(other.Name, oldName) {
		return nil
	}
	if enumInUse(doc, other.Name) {
		return conflict("ENUM 타입 %s 이(가) 이미 있습니다. 다른 이름을 쓰세요", other.Name)
	}
	return nil
}

// syncDomainEnum은 도메인 d 가 소유할 ENUM 타입을 맞춘다. oldName 은 바꾸기 전 이름이다.
//
//   - ENUM 도메인이면 타입을 만들거나(같은 이름의 타입이 있으면 그것을 맡는다) 이름과
//     값을 도메인에 맞춘다. 이름이 바뀌면 그 타입을 가리키던 컬럼도 따라간다.
//   - ENUM 이 아니게 되었으면, 아무 컬럼도 쓰지 않게 된 옛 타입을 지운다.
func syncDomainEnum(doc *Document, oldName string, d *Domain) {
	values := ownedEnumValues(doc, d)
	e := findSchemaEnum(doc.Schema, oldName)
	if values == nil {
		if e != nil && !enumInUse(doc, e.Name) {
			removeSchemaEnum(doc.Schema, e)
		}
		return
	}
	if e == nil {
		e = findSchemaEnum(doc.Schema, d.Name)
	}
	// 같은 이름을 가진 다른 타입이 있으면(쓰는 컬럼이 없는 것만 여기까지 온다) 비운다.
	if other := findSchemaEnum(doc.Schema, d.Name); other != nil && other != e {
		removeSchemaEnum(doc.Schema, other)
	}
	if e == nil {
		doc.Schema.Enums = append(doc.Schema.Enums, &schema.Enum{
			Name: d.Name, Values: append([]string(nil), values...),
		})
		return
	}
	if e.Name != d.Name {
		for _, t := range doc.Schema.Tables {
			for _, c := range t.Columns {
				if c.Type.Base == schema.TypeEnum && strings.EqualFold(c.Type.EnumName, e.Name) {
					c.Type.EnumName = d.Name
				}
			}
		}
		e.Name = d.Name
	}
	e.Values = append([]string(nil), values...)
}

// linkEnumColumns는 도메인이 소유한 타입을 가리키지만 아직 도메인에 묶이지 않은
// 컬럼을 그 도메인에 묶는다. 실제 DB 나 SQL 에서 온 컬럼이 이렇다.
func linkEnumColumns(doc *Document, d *Domain) error {
	for _, t := range doc.Schema.Tables {
		for _, c := range t.Columns {
			if c.Domain != "" || c.Type.Base != schema.TypeEnum || !strings.EqualFold(c.Type.EnumName, d.Name) {
				continue
			}
			if err := setColumnDomain(doc, c, d); err != nil {
				return err
			}
		}
	}
	return nil
}

// adoptEnum은 ENUM 타입 e 를 도메인으로 맡는다. 같은 이름의 도메인이 없을 때만 만든다.
//
// 도메인 이름 규칙에 맞지 않거나 값에 따옴표가 든 타입은 맡지 않는다(타입 칸의
// ENUM('…') 표기로 되살릴 수 없다). 그런 타입도 Schema.Enums 에 그대로 남아 내보내기와
// 비교는 이전처럼 된다.
func adoptEnum(doc *Document, e *schema.Enum) error {
	if e == nil || doc.findDomain(e.Name) != nil || len(e.Values) == 0 {
		return nil
	}
	if _, err := validateIdent("도메인", e.Name); err != nil {
		return nil
	}
	for _, v := range e.Values {
		if strings.ContainsAny(v, "'\"\\") {
			return nil
		}
	}
	d := &Domain{Name: e.Name, Type: schema.EnumTypeString(e.Values)}
	doc.Domains = append(doc.Domains, d)
	return linkEnumColumns(doc, d)
}

// AdoptEnumsAsDomains는 문서의 이름 붙은 ENUM 타입을 모두 도메인으로 맡는다.
// 실제 DB 에서 초안을 만들 때 쓴다 — 사람은 ENUM 타입을 도메인 탭에서만 본다.
func AdoptEnumsAsDomains(doc *Document) error {
	for _, e := range doc.Schema.Enums {
		if err := adoptEnum(doc, e); err != nil {
			return err
		}
	}
	return nil
}
