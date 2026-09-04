package erd

import (
	"strings"

	"dbstudio/internal/schema"
)

// 뷰(VIEW) 편집.
//
// 뷰를 초안에 두는 이유: 실제 DB에는 뷰가 있고, 그것을 읽는 사람에게 뷰는 "표처럼
// 조회하는 무엇"이다. 도면에 표만 있으면 그 사람은 화면에 없는 것을 머릿속으로
// 이어 붙여야 하고, 설계 리뷰에서 "이 집계는 어디서 나오나"에 답할 곳이 없다.
//
// 본문(Definition)은 SELECT 한 덩어리를 **글자 그대로** 담는다. 컬럼으로 쪼개
// 담지 않는 이유가 둘이다.
//
//  1. 뷰의 컬럼은 SELECT 의 결과이지 사람이 정하는 목록이 아니다. 쪼개 두면 본문과
//     컬럼 목록이 어긋날 수 있고, 어긋난 쪽을 진짜로 삼을 근거가 없다.
//  2. 마이그레이션은 이 문자열을 CREATE OR REPLACE VIEW … AS 뒤에 그대로 붙인다.
//     한 번 다시 적어 내면 사람이 적어 둔 줄바꿈과 주석이 사라진다.
//
// 좌표는 표와 같은 Layout 지도에 담는다. 한 스키마 안에서 표와 뷰는 이름을 나눠
// 쓰므로(둘 다 같은 이름이면 DB가 거부한다) 열쇠가 겹치지 않는다 — 그리고 겹치는
// 이름을 초안에서 막는 것이 아래 validateViewName 이다.

type viewPayload struct {
	// Key는 고칠 대상이다(view.update / view.delete).
	Key string `json:"key"`
	// 이하는 값이다. update 에서는 보낸 것만 바뀐다(패치).
	Name       *string `json:"name"`
	Namespace  *string `json:"namespace"`
	Definition *string `json:"definition"`
	Comment    *string `json:"comment"`
	// 좌표는 add 에서만 쓴다. 화면이 "지금 보고 있는 가운데"를 알려 준다.
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

// maxViewDefinition은 본문 길이의 상한이다.
//
// 상한이 필요한 이유: 이 문자열은 op 로그와 문서에 그대로 담기고, 브라우저는 그것을
// 카드 안에 그린다. 100KB 짜리 SELECT 를 담으면 문서를 여는 모든 사람이 그 값을
// 내려받는다. 실무의 뷰는 몇 KB 를 넘지 않는다.
const maxViewDefinition = 20000

func applyViewAdd(doc *Document, op *Op) error {
	var p viewPayload
	if err := decode(op, &p); err != nil {
		return err
	}
	name, ns, err := validateViewName(doc, p, "")
	if err != nil {
		return err
	}
	def, err := validateViewDefinition(p.Definition)
	if err != nil {
		return err
	}
	v := &schema.View{Namespace: ns, Name: name, Definition: def}
	if p.Comment != nil {
		v.Comment = strings.TrimSpace(*p.Comment)
	}
	doc.Schema.Views = append(doc.Schema.Views, v)
	doc.Schema.Sort()
	doc.Layout[v.Key()] = &Box{X: p.X, Y: p.Y}
	return nil
}

func applyViewUpdate(doc *Document, op *Op) error {
	var p viewPayload
	if err := decode(op, &p); err != nil {
		return err
	}
	target := doc.findView(p.Key)
	if target == nil {
		return notFound("뷰 %s 을(를) 찾을 수 없습니다", p.Key)
	}
	oldKey := target.Key()

	if p.Name != nil || p.Namespace != nil {
		name, ns, err := validateViewName(doc, p, oldKey)
		if err != nil {
			return err
		}
		target.Name, target.Namespace = name, ns
	}
	if p.Definition != nil {
		def, err := validateViewDefinition(p.Definition)
		if err != nil {
			return err
		}
		target.Definition = def
	}
	if p.Comment != nil {
		target.Comment = strings.TrimSpace(*p.Comment)
	}

	// 이름이 바뀌면 좌표도 새 열쇠로 옮긴다. 옮기지 않으면 카드가 화면 왼쪽 위로
	// 튀고(좌표가 없는 것으로 보이므로), 사람은 자기가 놓아 둔 자리를 잃는다.
	if newKey := target.Key(); newKey != oldKey {
		if box, ok := doc.Layout[oldKey]; ok {
			delete(doc.Layout, oldKey)
			doc.Layout[newKey] = box
		}
	}
	doc.Schema.Sort()
	return nil
}

func applyViewDelete(doc *Document, op *Op) error {
	var p viewPayload
	if err := decode(op, &p); err != nil {
		return err
	}
	key := strings.ToLower(strings.TrimSpace(p.Key))
	for i, v := range doc.Schema.Views {
		if v.Key() != key {
			continue
		}
		doc.Schema.Views = append(doc.Schema.Views[:i], doc.Schema.Views[i+1:]...)
		delete(doc.Layout, key)
		return nil
	}
	return notFound("뷰 %s 을(를) 찾을 수 없습니다", p.Key)
}

// applyViewMove는 뷰 카드의 자리를 옮긴다(레이아웃 전용).
//
// table.move 와 나누는 이유: 그쪽은 대상이 표인지 확인하고 접기·색·아이콘·컬럼
// 아이콘까지 함께 다룬다. 뷰에는 그중 좌표와 폭만 있고, 표가 아닌 열쇠를 그 함수에
// 넣으면 "테이블을 찾을 수 없습니다"로 거부된다.
func applyViewMove(doc *Document, op *Op) error {
	var p struct {
		Key   string   `json:"key"`
		X     float64  `json:"x"`
		Y     float64  `json:"y"`
		Width *float64 `json:"width"`
	}
	if err := decode(op, &p); err != nil {
		return err
	}
	v := doc.findView(p.Key)
	if v == nil {
		return notFound("뷰 %s 을(를) 찾을 수 없습니다", p.Key)
	}
	box := doc.Layout[v.Key()]
	if box == nil {
		box = &Box{}
		doc.Layout[v.Key()] = box
	}
	box.X, box.Y = p.X, p.Y
	if p.Width != nil {
		box.W = clampCardWidth(*p.Width)
	}
	return nil
}

// validateViewName은 이름과 스키마를 확인하고 겹침을 막는다.
//
// **표 이름과도 겹칠 수 없다.** 한 스키마 안에서 표와 뷰는 이름을 나눠 쓰므로
// 겹치면 대상 DB가 그 DDL 을 거부한다 — 초안에서 막지 않으면 그 사실은 마이그레이션을
// 실행하는 순간에야 드러난다.
func validateViewName(doc *Document, p viewPayload, selfKey string) (string, string, error) {
	name := ""
	if p.Name != nil {
		name = strings.TrimSpace(*p.Name)
	}
	if name == "" && selfKey != "" {
		if cur := doc.findView(selfKey); cur != nil {
			name = cur.Name
		}
	}
	checked, err := validateIdent("뷰", name)
	if err != nil {
		return "", "", err
	}
	ns := ""
	if p.Namespace != nil {
		ns = strings.TrimSpace(*p.Namespace)
	} else if selfKey != "" {
		if cur := doc.findView(selfKey); cur != nil {
			ns = cur.Namespace
		}
	}
	key := strings.ToLower(checked)
	if ns != "" {
		key = strings.ToLower(ns + "." + checked)
	}
	if key != selfKey {
		if doc.findView(key) != nil {
			return "", "", conflict("뷰 %s 이(가) 이미 있습니다", key)
		}
		if doc.findTable(key) != nil {
			return "", "", conflict("%s 은(는) 테이블 이름으로 이미 쓰이고 있습니다 — "+
				"한 스키마에서 표와 뷰는 같은 이름을 쓸 수 없습니다", key)
		}
	}
	return checked, ns, nil
}

func validateViewDefinition(def *string) (string, error) {
	if def == nil {
		return "", invalid("뷰 정의(SELECT)를 적어야 합니다")
	}
	out := strings.TrimSpace(*def)
	// 끝의 세미콜론은 뗀다. 정의는 CREATE VIEW … AS 뒤에 그대로 붙으므로,
	// 남아 있으면 만들어진 DDL 에 세미콜론이 둘이 된다.
	out = strings.TrimSuffix(strings.TrimSpace(out), ";")
	out = strings.TrimSpace(out)
	if out == "" {
		return "", invalid("뷰 정의(SELECT)를 적어야 합니다")
	}
	if len(out) > maxViewDefinition {
		return "", invalid("뷰 정의가 너무 깁니다 (%d자 제한)", maxViewDefinition)
	}
	return out, nil
}

// findView는 열쇠로 뷰를 찾는다(이름만 준 경우도 받는다).
func (d *Document) findView(nameOrKey string) *schema.View {
	want := strings.ToLower(strings.TrimSpace(nameOrKey))
	if want == "" || d.Schema == nil {
		return nil
	}
	for _, v := range d.Schema.Views {
		if v.Key() == want || strings.ToLower(v.Name) == want {
			return v
		}
	}
	return nil
}
