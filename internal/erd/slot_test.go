package erd

import (
	"fmt"
	"testing"

	"dbstudio/internal/schema"
)

// rectOf는 검사에서 쓰는, 그 키가 차지하는 사각형이다.
func rectOf(t *testing.T, doc *Document, key string) cardRect {
	t.Helper()
	for _, r := range doc.occupiedRects() {
		b := doc.Layout[key]
		if b != nil && r.x == b.X && r.y == b.Y {
			return r
		}
	}
	t.Fatalf("%s 의 사각형을 찾지 못했습니다", key)
	return cardRect{}
}

// 새 표는 사람이 옮겨 둔 카드를 덮지 않아야 한다.
//
// 예전에는 격자점이 정확히 같은지만 봤다. 그래서 카드를 한 번이라도 옮긴
// 문서에서는 새 카드가 그 위에 겹쳐 놓였고, 겹친 카드는 끌어서 옮길 수도 없다
// (위에 있는 것을 잡게 된다).
func TestNewTableDoesNotCoverMovedCards(t *testing.T) {
	doc := NewDocument("d", "문서", "", "mysql")
	apply(t, doc, OpTableAdd, `{"name":"members","withId":true}`)

	// 사람이 카드를 격자에서 비껴 놓는다. 첫 격자점(80,80)을 덮는 자리다.
	doc.Layout["members"] = &Box{X: 90, Y: 90}

	apply(t, doc, OpTableAdd, `{"name":"orders","withId":true}`)

	a, b := rectOf(t, doc, "members"), rectOf(t, doc, "orders")
	if a.overlaps(b) {
		t.Errorf("새 카드가 옮겨 둔 카드를 덮었습니다: members=%+v orders=%+v", a, b)
	}
}

// 한 번에 여러 표를 넣어도 서로 겹치지 않아야 한다.
//
// SQL 불러오기가 이 길로 온다. 표를 하나씩 넣으므로, 방금 넣은 것이 다음 자리를
// 고를 때 셈에 들어가지 않으면 열 개가 한 자리에 쌓인다.
func TestImportedTablesDoNotOverlapEachOther(t *testing.T) {
	doc := NewDocument("d", "문서", "", "mysql")
	// 도면에 이미 뭔가 있고 사람이 옮겨 둔 상태로 만든다(빈 초안이면 AutoLayout 이
	// 전부 다시 놓아서 이 길을 지나지 않는다).
	apply(t, doc, OpTableAdd, `{"name":"kept","withId":true}`)
	doc.Layout["kept"] = &Box{X: 100, Y: 100}

	tables := []*schema.Table{}
	for i := 0; i < 12; i++ {
		cols := []*schema.Column{}
		// 컬럼 수를 제각각으로 둔다. 높이가 다 같으면 겹침을 못 본다.
		for c := 0; c <= i*2; c++ {
			cols = append(cols, &schema.Column{
				Name: fmt.Sprintf("c%d", c),
				Type: schema.LogicalType{Base: schema.TypeBigInt},
			})
		}
		tables = append(tables, &schema.Table{
			Name: fmt.Sprintf("t%d", i), Columns: cols,
		})
	}
	for _, tbl := range tables {
		mergeTable(doc, tbl)
	}

	rects := doc.occupiedRects()
	for i := 0; i < len(rects); i++ {
		for j := i + 1; j < len(rects); j++ {
			if rects[i].overlaps(rects[j]) {
				t.Fatalf("카드 둘이 겹칩니다: %+v · %+v", rects[i], rects[j])
			}
		}
	}
	if len(rects) != 13 {
		t.Errorf("사각형이 %d개입니다(기대 13개)", len(rects))
	}
}

// 메모와 그룹도 덮지 않는다. 그룹은 카드를 담는 상자라, 그 안에 새 카드를 놓으면
// 아무도 넣지 않은 카드가 그 묶음에 들어가 있게 된다.
func TestNewTableAvoidsNotesAndGroups(t *testing.T) {
	doc := NewDocument("d", "문서", "", "mysql")
	// 첫 격자점 주변을 메모와 그룹으로 덮는다.
	doc.Notes = append(doc.Notes, &Note{ID: "n1", Text: "메모", X: 60, Y: 60})
	doc.Groups = append(doc.Groups, &Group{
		ID: "g1", Label: "묶음", X: 380, Y: 60, W: 300, H: 300,
	})

	apply(t, doc, OpTableAdd, `{"name":"users","withId":true}`)
	box := doc.Layout["users"]
	card := cardRect{x: box.X, y: box.Y, w: cardDefW, h: CardHeight(1)}

	note := cardRect{x: 60, y: 60, w: noteDefW, h: noteDefH}
	group := cardRect{x: 380, y: 60, w: 300, h: 300}
	if card.overlaps(note) {
		t.Errorf("새 카드가 메모를 덮었습니다: %+v", card)
	}
	if card.overlaps(group) {
		t.Errorf("새 카드가 그룹을 덮었습니다: %+v", card)
	}
}

// 빈 도면의 첫 카드는 원점에 놓인다. 겹침을 피한다고 첫 카드를 엉뚱한 곳에
// 두면, 새 문서를 만들 때마다 도면이 비어 보인다.
func TestFirstCardGoesToOrigin(t *testing.T) {
	doc := NewDocument("d", "문서", "", "mysql")
	apply(t, doc, OpTableAdd, `{"name":"users","withId":true}`)
	box := doc.Layout["users"]
	if box.X != layoutOriginX || box.Y != layoutOriginY {
		t.Errorf("첫 카드가 (%.0f, %.0f) 에 놓였습니다", box.X, box.Y)
	}
}
