package erd

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"dbstudio/internal/schema"
)

// 되돌리기의 구현: op마다 역연산을 손으로 쓰는 대신, 적용 전후를 비교해
// "원래대로 돌려놓는 op" 하나를 만든다.
//
// 종류마다 역연산을 쓰는 방식은 24가지 op에 24가지 규칙이 생기고, 그중 몇은
// 단순하지 않다. 테이블 삭제는 컬럼·인덱스·외래키·제약을 함께 지우고 **다른
// 테이블의 외래키까지** 정리하며, 컬럼 삭제도 마찬가지다. 그 규칙을 두 벌
// (적용과 역연산) 유지하면 언젠가 한쪽만 고쳐지고, 그 증상은 "되돌렸는데 뭔가
// 사라져 있다"로 나타난다 — 사용자가 가장 믿을 수 없게 되는 종류의 버그다.
//
// 비교 기반이면 규칙이 한 곳(Apply)에만 있다. 대신 문서 크기에 비례하는 비용을
// 매 편집마다 치른다. 이미 op마다 문서를 통째로 복제(Clone)하고 있으므로 같은
// 차수이고, 실제로는 사람이 편집하는 속도(분당 몇 번)에서 문제가 되지 않는다.

// OpRestore는 문서의 일부를 지정한 내용으로 되돌린다.
//
// 되돌리기 전용 op이며 사용자가 직접 만들지 않는다. Diff가 만들어 내고, 이 op를
// 다시 Diff에 넣으면 그 역(다시실행)이 나온다 — 그래서 다시실행에 별도 규칙이 없다.
const OpRestore Kind = "doc.restore"

// RestorePayload는 "이것들을 이 내용으로 두고, 저것들은 없앤다"이다.
//
// 패치가 아니라 전체 교체인 이유: 되돌리기의 목적은 정확히 그 시점의 내용으로
// 돌아가는 것이다. 다른 편집 op와 달리 여기서는 LWW 병합이 오히려 방해가 된다.
type RestorePayload struct {
	Tables     []*schema.Table `json:"tables,omitempty"`
	DropTables []string        `json:"dropTables,omitempty"`
	// TableOrder는 복원 후 테이블 순서다. 순서는 생성되는 DDL의 순서이므로
	// 되돌린 뒤에 달라지면 마이그레이션 diff에 이유 없는 변경이 섞인다.
	TableOrder []string `json:"tableOrder,omitempty"`

	Boxes     map[string]*Box `json:"boxes,omitempty"`
	DropBoxes []string        `json:"dropBoxes,omitempty"`

	Notes     []*Note  `json:"notes,omitempty"`
	DropNotes []string `json:"dropNotes,omitempty"`

	Groups     []*Group `json:"groups,omitempty"`
	DropGroups []string `json:"dropGroups,omitempty"`

	Enums     []*schema.Enum `json:"enums,omitempty"`
	DropEnums []string       `json:"dropEnums,omitempty"`

	Domains     []*Domain `json:"domains,omitempty"`
	DropDomains []string  `json:"dropDomains,omitempty"`

	// Expect는 "되돌리기 직전에 이것들이 이 모습이어야 한다"이다.
	// 키는 "table:users" 처럼 종류를 앞에 붙이고, 값은 내용의 지문이다.
	// 빈 문자열은 "없어야 한다"를 뜻한다.
	//
	// 이 확인이 없으면 되돌리기가 남의 편집을 조용히 지운다. 내가 지운 테이블을
	// 되살리는 사이에 다른 사람이 같은 이름으로 새 테이블을 만들었다면, 복원은
	// 그것을 통째로 덮어쓴다 — 그 사람 입장에서는 방금 만든 것이 이유 없이
	// 사라진 것이다. 좌표(Layout)에는 걸지 않는다. 그것은 숫자 두 개이고,
	// 남이 카드를 조금 옮겼다고 되돌리기를 막으면 쓸 수 없다.
	Expect map[string]string `json:"expect,omitempty"`
}

func (p *RestorePayload) empty() bool {
	return len(p.Tables) == 0 && len(p.DropTables) == 0 && len(p.Boxes) == 0 &&
		len(p.DropBoxes) == 0 && len(p.Notes) == 0 && len(p.DropNotes) == 0 &&
		len(p.Groups) == 0 && len(p.DropGroups) == 0 &&
		len(p.Enums) == 0 && len(p.DropEnums) == 0 &&
		len(p.Domains) == 0 && len(p.DropDomains) == 0
}

func applyRestore(doc *Document, op *Op) error {
	var p RestorePayload
	if err := decode(op, &p); err != nil {
		return err
	}
	if err := checkExpectations(doc, p.Expect); err != nil {
		return err
	}

	// 지우기가 먼저다. 이름을 바꾼 편집을 되돌릴 때는 새 이름을 없애고 옛 이름을
	// 넣는 것이 한 번에 일어나야 하는데, 순서가 반대면 "이미 있습니다"가 된다.
	for _, key := range p.DropTables {
		dropTable(doc, key)
	}
	for _, tbl := range p.Tables {
		if tbl == nil || strings.TrimSpace(tbl.Name) == "" {
			return invalid("복원할 테이블 정보가 비어 있습니다")
		}
		dropTable(doc, tbl.Key())
		doc.Schema.Tables = append(doc.Schema.Tables, tbl)
	}
	if len(p.TableOrder) > 0 {
		reorderTables(doc, p.TableOrder)
	}

	for _, key := range p.DropBoxes {
		delete(doc.Layout, key)
	}
	for key, box := range p.Boxes {
		if doc.Layout == nil {
			doc.Layout = map[string]*Box{}
		}
		doc.Layout[key] = box
	}

	doc.Notes = restoreByID(doc.Notes, p.Notes, p.DropNotes,
		func(n *Note) string { return n.ID })
	doc.Groups = restoreByID(doc.Groups, p.Groups, p.DropGroups,
		func(g *Group) string { return g.ID })
	doc.Schema.Enums = restoreByID(doc.Schema.Enums, p.Enums, p.DropEnums,
		func(e *schema.Enum) string { return e.Key() })
	doc.Domains = restoreByID(doc.Domains, p.Domains, p.DropDomains,
		func(d *Domain) string { return d.Key() })

	// 되돌리기는 다른 사람의 편집 위에서 일어난다. 내가 만든 테이블을 지웠다가
	// 되살리는 사이에 다른 사람이 그 참조를 지웠을 수 있고, 그 반대도 있다.
	// 여기서 막지 않으면 없는 테이블을 가리키는 외래키가 남아 마이그레이션
	// 단계에서야 터진다 — 그때는 원인이 되돌리기였다는 것을 알 수 없다.
	return validateRefs(doc)
}

func dropTable(doc *Document, key string) {
	kept := doc.Schema.Tables[:0]
	for _, t := range doc.Schema.Tables {
		if t.Key() == key {
			continue
		}
		kept = append(kept, t)
	}
	doc.Schema.Tables = kept
}

// reorderTables는 주어진 키 순서대로 테이블을 정렬한다.
// 목록에 없는 테이블(그 사이에 남이 추가한 것)은 뒤에 그대로 붙인다.
func reorderTables(doc *Document, order []string) {
	rank := make(map[string]int, len(order))
	for i, key := range order {
		rank[key] = i
	}
	ranked := make([]*schema.Table, 0, len(doc.Schema.Tables))
	rest := make([]*schema.Table, 0, len(doc.Schema.Tables))
	for _, t := range doc.Schema.Tables {
		if _, ok := rank[t.Key()]; ok {
			ranked = append(ranked, t)
		} else {
			rest = append(rest, t)
		}
	}
	for i := 0; i < len(ranked); i++ {
		for j := i + 1; j < len(ranked); j++ {
			if rank[ranked[j].Key()] < rank[ranked[i].Key()] {
				ranked[i], ranked[j] = ranked[j], ranked[i]
			}
		}
	}
	doc.Schema.Tables = append(ranked, rest...)
}

func restoreByID[T any](cur []T, set []T, drop []string, id func(T) string) []T {
	gone := map[string]bool{}
	for _, key := range drop {
		gone[key] = true
	}
	replaced := map[string]T{}
	for _, item := range set {
		replaced[id(item)] = item
	}
	out := make([]T, 0, len(cur)+len(set))
	seen := map[string]bool{}
	for _, item := range cur {
		key := id(item)
		if gone[key] {
			continue
		}
		if rep, ok := replaced[key]; ok {
			out = append(out, rep)
			seen[key] = true
			continue
		}
		out = append(out, item)
	}
	for _, item := range set {
		if !seen[id(item)] {
			out = append(out, item)
		}
	}
	return out
}

// validateRefs는 외래키가 가리키는 테이블이 실제로 있는지 본다.
func validateRefs(doc *Document) error {
	keys := map[string]bool{}
	for _, t := range doc.Schema.Tables {
		keys[t.Key()] = true
	}
	for _, t := range doc.Schema.Tables {
		for _, fk := range t.ForeignKeys {
			if !keys[fk.RefKey()] {
				return conflict(
					"%s 의 외래키 %s 가 참조하는 테이블이 없습니다 (다른 사용자가 지웠을 수 있습니다)",
					t.Display(), fk.Name)
			}
		}
	}
	return nil
}

// expectations는 지금 문서의 지문을 모은다. 되돌리기가 적용될 때 이 값들이
// 그대로여야 한다 — 하나라도 다르면 그 사이에 누군가 그것을 고쳤다는 뜻이다.
func checkExpectations(doc *Document, expect map[string]string) error {
	if len(expect) == 0 {
		return nil
	}
	now := map[string]string{}
	for _, t := range doc.Schema.Tables {
		now["table:"+t.Key()] = fingerprint(t)
	}
	for _, n := range doc.Notes {
		now["note:"+n.ID] = fingerprint(n)
	}
	for _, g := range doc.Groups {
		now["group:"+g.ID] = fingerprint(g)
	}
	for _, e := range doc.Schema.Enums {
		now["enum:"+e.Key()] = fingerprint(e)
	}
	for _, d := range doc.Domains {
		now["domain:"+d.Key()] = fingerprint(d)
	}
	for key, want := range expect {
		got := now[key]
		if got == want {
			continue
		}
		name := key[strings.Index(key, ":")+1:]
		switch {
		case want == "":
			return conflict("%s 은(는) 다른 사용자가 그 사이에 만들었습니다. 되돌릴 수 없습니다", name)
		case got == "":
			return conflict("%s 은(는) 다른 사용자가 그 사이에 지웠습니다. 되돌릴 수 없습니다", name)
		default:
			return conflict("%s 은(는) 다른 사용자가 그 사이에 고쳤습니다. 되돌리면 그 편집이 사라집니다", name)
		}
	}
	return nil
}

// tableFingerprint는 지금 그 자리에 있는 것의 지문이다.
// 없으면 빈 문자열이며, 그것은 "되돌릴 때도 없어야 한다"를 뜻한다.
func tableFingerprint(tables map[string]*schema.Table, key string) string {
	tbl, ok := tables[key]
	if !ok {
		return ""
	}
	return fingerprint(tbl)
}

func fingerprint(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return "?"
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:8])
}

// Diff는 from을 to로 되돌리는 op를 만든다. 바뀐 것이 없으면 nil이다.
//
// 방향에 주의: 편집을 되돌리려면 Diff(적용 후, 적용 전)이다.
func Diff(from, to *Document) *Op {
	p := &RestorePayload{}

	fromTables := tableMap(from)
	toTables := tableMap(to)
	for key, tbl := range toTables {
		if !sameJSON(fromTables[key], tbl) {
			p.Tables = append(p.Tables, tbl)
		}
	}
	for key := range fromTables {
		if _, ok := toTables[key]; !ok {
			p.DropTables = append(p.DropTables, key)
		}
	}
	// 순서는 내용이 하나라도 달라졌을 때만 싣는다. 좌표만 바뀐 편집에까지
	// 테이블 목록 전체를 붙이면 payload가 이유 없이 커진다.
	if len(p.Tables) > 0 || len(p.DropTables) > 0 || !sameTableOrder(from, to) {
		p.TableOrder = tableOrder(to)
	}

	for key, box := range to.Layout {
		if !sameJSON(from.Layout[key], box) {
			if p.Boxes == nil {
				p.Boxes = map[string]*Box{}
			}
			p.Boxes[key] = box
		}
	}
	for key := range from.Layout {
		if _, ok := to.Layout[key]; !ok {
			p.DropBoxes = append(p.DropBoxes, key)
		}
	}

	p.Notes, p.DropNotes = diffByID(from.Notes, to.Notes, func(n *Note) string { return n.ID })
	p.Groups, p.DropGroups = diffByID(from.Groups, to.Groups, func(g *Group) string { return g.ID })
	p.Enums, p.DropEnums = diffByID(from.Schema.Enums, to.Schema.Enums,
		func(e *schema.Enum) string { return e.Key() })
	p.Domains, p.DropDomains = diffByID(from.Domains, to.Domains,
		func(d *Domain) string { return d.Key() })

	if p.empty() {
		return nil
	}
	// 손댈 대상마다 "지금 이 모습이어야 한다"를 함께 적는다.
	// from은 이 편집 직후의 문서이므로, 아무도 건드리지 않았다면 되돌릴 때도 같다.
	p.Expect = map[string]string{}
	for _, tbl := range p.Tables {
		p.Expect["table:"+tbl.Key()] = tableFingerprint(fromTables, tbl.Key())
	}
	for _, key := range p.DropTables {
		p.Expect["table:"+key] = tableFingerprint(fromTables, key)
	}
	markExpect(p.Expect, "note", from.Notes, p.Notes, p.DropNotes,
		func(n *Note) string { return n.ID })
	markExpect(p.Expect, "group", from.Groups, p.Groups, p.DropGroups,
		func(g *Group) string { return g.ID })
	markExpect(p.Expect, "enum", from.Schema.Enums, p.Enums, p.DropEnums,
		func(e *schema.Enum) string { return e.Key() })
	markExpect(p.Expect, "domain", from.Domains, p.Domains, p.DropDomains,
		func(d *Domain) string { return d.Key() })
	raw, err := json.Marshal(p)
	if err != nil {
		// RestorePayload는 전부 직렬화 가능한 타입이다. 여기 오면 프로그래밍 오류다.
		panic(fmt.Sprintf("erd: 복원 payload 직렬화 실패: %v", err))
	}
	return &Op{Kind: OpRestore, Payload: raw}
}

// markExpect는 건드릴 항목들의 현재 지문을 적는다. 지금 없는 것은 빈 문자열이며,
// 그것은 "되돌릴 때도 없어야 한다"는 뜻이다(내가 추가한 것을 되돌리는 경우).
func markExpect[T any](expect map[string]string, prefix string, cur, set []T, drop []string, id func(T) string) {
	now := map[string]string{}
	for _, item := range cur {
		now[id(item)] = fingerprint(item)
	}
	for _, item := range set {
		expect[prefix+":"+id(item)] = now[id(item)]
	}
	for _, key := range drop {
		expect[prefix+":"+key] = now[key]
	}
}

func diffByID[T any](from, to []T, id func(T) string) (set []T, drop []string) {
	fromMap := map[string]T{}
	for _, item := range from {
		fromMap[id(item)] = item
	}
	toMap := map[string]bool{}
	for _, item := range to {
		key := id(item)
		toMap[key] = true
		if old, ok := fromMap[key]; !ok || !sameJSON(old, item) {
			set = append(set, item)
		}
	}
	for key := range fromMap {
		if !toMap[key] {
			drop = append(drop, key)
		}
	}
	return set, drop
}

func tableMap(doc *Document) map[string]*schema.Table {
	out := make(map[string]*schema.Table, len(doc.Schema.Tables))
	for _, t := range doc.Schema.Tables {
		out[t.Key()] = t
	}
	return out
}

func tableOrder(doc *Document) []string {
	out := make([]string, 0, len(doc.Schema.Tables))
	for _, t := range doc.Schema.Tables {
		out = append(out, t.Key())
	}
	return out
}

func sameTableOrder(a, b *Document) bool {
	x, y := tableOrder(a), tableOrder(b)
	if len(x) != len(y) {
		return false
	}
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

// sameJSON은 두 값을 직렬화해 비교한다.
//
// reflect.DeepEqual을 쓰지 않는 이유: 포인터 필드가 많아 "같은 내용, 다른 주소"가
// 흔하고(문서는 op마다 복제된다), 시간 필드처럼 단조롭지 않은 값도 섞여 있다.
// 어차피 payload로 나갈 것과 같은 표현으로 비교하는 편이 예측 가능하다.
func sameJSON(a, b any) bool {
	if isNil(a) != isNil(b) {
		return false
	}
	if isNil(a) {
		return true
	}
	ja, err1 := json.Marshal(a)
	jb, err2 := json.Marshal(b)
	if err1 != nil || err2 != nil {
		return false
	}
	return string(ja) == string(jb)
}

func isNil(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case *schema.Table:
		return t == nil
	case *schema.Enum:
		return t == nil
	case *Box:
		return t == nil
	case *Note:
		return t == nil
	case *Group:
		return t == nil
	}
	return false
}
