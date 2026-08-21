package erd

import (
	"strings"

	"dbstudio/internal/schema"
)

// 도메인(재사용 타입) 편집.
//
// 도메인은 이 앱 안에서만 사는 개념이고, 컬럼에는 언제나 **구체 타입**이 함께 남는다.
// 그래서 마이그레이션·비교·DDL 생성은 도메인을 몰라도 되고, 도메인을 지워도 설계는
// 그대로 성립한다. 도메인이 하는 일은 "같은 뜻의 컬럼을 같은 타입으로 유지하는 것"
// 하나이며, 그 유지는 도메인을 고칠 때 쓰는 컬럼들을 함께 고치는 것으로 이뤄진다.

type domainAddPayload struct {
	Name     string  `json:"name"`
	Type     string  `json:"type"`
	Nullable *bool   `json:"nullable,omitempty"`
	Default  *string `json:"default,omitempty"`
	Comment  *string `json:"comment,omitempty"`
}

func applyDomainAdd(doc *Document, op *Op) error {
	var p domainAddPayload
	if err := decode(op, &p); err != nil {
		return err
	}
	name, err := validateIdent("도메인", p.Name)
	if err != nil {
		return err
	}
	if doc.findDomain(name) != nil {
		return conflict("도메인 %s 이(가) 이미 있습니다", name)
	}
	if _, _, err := parseColumnType(doc, p.Type); err != nil {
		return err
	}
	d := &Domain{Name: name, Type: strings.TrimSpace(p.Type)}
	if p.Nullable != nil {
		v := *p.Nullable
		d.Nullable = &v
	}
	if p.Default != nil {
		d.Default = strings.TrimSpace(*p.Default)
	}
	if p.Comment != nil {
		d.Comment = strings.TrimSpace(*p.Comment)
	}
	doc.Domains = append(doc.Domains, d)
	return nil
}

type domainUpdatePayload struct {
	Name     string  `json:"name"`
	NewName  *string `json:"newName,omitempty"`
	Type     *string `json:"type,omitempty"`
	Nullable *bool   `json:"nullable,omitempty"`
	// ClearNullable/ClearDefault는 "도메인이 이 속성에 관여하지 않는다"로 되돌린다.
	// 값을 비우는 것(NULL 불가, 기본값 없음)과 관여하지 않는 것은 다른 뜻이다.
	ClearNullable bool    `json:"clearNullable,omitempty"`
	Default       *string `json:"default,omitempty"`
	Comment       *string `json:"comment,omitempty"`
}

func applyDomainUpdate(doc *Document, op *Op) error {
	var p domainUpdatePayload
	if err := decode(op, &p); err != nil {
		return err
	}
	d := doc.findDomain(p.Name)
	if d == nil {
		return notFound("도메인 %s 을(를) 찾을 수 없습니다", p.Name)
	}
	oldName := d.Name

	if p.NewName != nil {
		name, err := validateIdent("도메인", *p.NewName)
		if err != nil {
			return err
		}
		if other := doc.findDomain(name); other != nil && other != d {
			return conflict("도메인 %s 이(가) 이미 있습니다", name)
		}
		d.Name = name
	}
	if p.Type != nil {
		if _, _, err := parseColumnType(doc, *p.Type); err != nil {
			return err
		}
		d.Type = strings.TrimSpace(*p.Type)
	}
	if p.ClearNullable {
		d.Nullable = nil
	} else if p.Nullable != nil {
		v := *p.Nullable
		d.Nullable = &v
	}
	if p.Default != nil {
		d.Default = strings.TrimSpace(*p.Default)
	}
	if p.Comment != nil {
		d.Comment = strings.TrimSpace(*p.Comment)
	}

	// 이 도메인을 쓰는 컬럼에 새 정의를 흘려보낸다. 이것이 도메인의 존재 이유다 —
	// 정의만 고치고 컬럼이 그대로면 "이름을 붙인 메모"에 지나지 않는다.
	return applyDomainToColumns(doc, oldName, d)
}

type domainDeletePayload struct {
	Name string `json:"name"`
}

func applyDomainDelete(doc *Document, op *Op) error {
	var p domainDeletePayload
	if err := decode(op, &p); err != nil {
		return err
	}
	d := doc.findDomain(p.Name)
	if d == nil {
		return notFound("도메인 %s 을(를) 찾을 수 없습니다", p.Name)
	}
	// 쓰던 컬럼의 타입은 그대로 두고 연결만 끊는다. 타입까지 지우면 도메인 하나를
	// 정리하려다 설계의 여러 컬럼이 타입을 잃는다.
	key := d.Key()
	for _, t := range doc.Schema.Tables {
		for _, c := range t.Columns {
			if strings.EqualFold(c.Domain, key) || strings.EqualFold(c.Domain, d.Name) {
				c.Domain = ""
			}
		}
	}
	kept := doc.Domains[:0]
	for _, cur := range doc.Domains {
		if cur == d {
			continue
		}
		kept = append(kept, cur)
	}
	doc.Domains = kept
	return nil
}

// findDomain은 대소문자를 무시하고 도메인을 찾는다.
func (d *Document) findDomain(name string) *Domain {
	key := strings.ToLower(strings.TrimSpace(name))
	if key == "" {
		return nil
	}
	for _, cur := range d.Domains {
		if cur.Key() == key {
			return cur
		}
	}
	return nil
}

// applyDomainToColumns는 도메인을 쓰는 모든 컬럼을 도메인의 정의에 맞춘다.
func applyDomainToColumns(doc *Document, oldName string, d *Domain) error {
	for _, t := range doc.Schema.Tables {
		for _, c := range t.Columns {
			if !strings.EqualFold(c.Domain, oldName) {
				continue
			}
			if err := setColumnDomain(doc, c, d); err != nil {
				return err
			}
		}
	}
	return nil
}

// setColumnDomain은 컬럼에 도메인을 입힌다.
//
// 타입은 언제나 덮어쓰고, NULL 허용과 기본값은 도메인이 정한 경우에만 덮어쓴다.
// 이 구분이 필요한 이유: "이메일" 도메인은 타입은 정해 주지만, 어떤 표에서는 필수이고
// 어떤 표에서는 선택일 수 있다.
func setColumnDomain(doc *Document, col *schema.Column, d *Domain) error {
	lt, raw, err := parseColumnType(doc, d.Type)
	if err != nil {
		return err
	}
	col.Type, col.RawType = lt, raw
	col.Domain = d.Name
	if d.Nullable != nil {
		col.Nullable = *d.Nullable
	}
	if d.Default != "" {
		col.HasDefault = true
		col.Default = d.Default
	}
	return nil
}
