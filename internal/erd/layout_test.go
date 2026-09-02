package erd

import (
	"fmt"
	"testing"

	"dbstudio/internal/schema"
)

// 컬럼이 많은 표는 아래 카드를 덮지 않는다.
//
// 컬럼을 전부 그리기로 하면서 카드 높이가 표마다 크게 달라졌다. 고정 격자에 놓으면
// 컬럼이 열두 개만 넘어도 아래 줄 카드 위에 얹힌다 — 그러면 문서를 처음 열었을 때
// 두 표가 겹친 채로 보이고, 사람이 하나씩 끌어내려야 한다.
func TestAutoLayoutStacksByHeight(t *testing.T) {
	mk := func(name string, n int) *schema.Table {
		cols := make([]*schema.Column, n)
		for i := range cols {
			cols[i] = &schema.Column{Name: "c", Position: i + 1}
		}
		return &schema.Table{Name: name, Columns: cols}
	}
	// 한 줄(4칸)을 채우고 그다음 줄로 넘어가게 다섯 개를 둔다.
	// 첫 표는 컬럼 40개짜리 큰 표다.
	sc := &schema.Schema{Tables: []*schema.Table{
		mk("big", 40), mk("b", 3), mk("c", 3), mk("d", 3), mk("under_big", 3),
	}}

	layout := AutoLayout(sc)
	big := layout[sc.Tables[0].Key()]
	under := layout[sc.Tables[4].Key()]

	if big.X != under.X {
		t.Fatalf("같은 열에 놓이지 않았습니다: %v vs %v", big.X, under.X)
	}
	bottom := big.Y + CardHeight(40)
	if under.Y < bottom {
		t.Errorf("아래 카드가 큰 카드를 덮습니다: 큰 카드 %.0f~%.0f, 아래 카드 %.0f",
			big.Y, bottom, under.Y)
	}

	// 작은 표들끼리는 예전 격자와 같은 세로 간격을 지킨다. 간격이 이유 없이
	// 좁아지면 카드가 붙어 보이고, 넓어지면 도면이 늘어난다.
	small := &schema.Schema{Tables: []*schema.Table{
		mk("a", 2), mk("b", 2), mk("c", 2), mk("d", 2), mk("e", 2),
	}}
	got := AutoLayout(small)
	first := got[small.Tables[0].Key()]
	second := got[small.Tables[1].Key()]
	if first.X != second.X {
		t.Fatalf("관계가 없는 표들이 같은 열에 놓이지 않았습니다: %v vs %v", first.X, second.X)
	}
	if second.Y-first.Y != layoutStepY {
		t.Errorf("작은 표의 세로 간격 = %.0f, 기대 %.0f", second.Y-first.Y, layoutStepY)
	}
}

// 가리켜지는 표가 왼쪽, 가리키는 표가 오른쪽에 선다.
//
// 왜 이 규칙인가: 격자로 나열하면 관계선이 도면을 가로지른다. 회원과 주문이 대각선
// 으로 마주 보는 그림에서 무엇이 무엇을 가리키는지는 선을 눈으로 따라가야 알 수 있다.
// 참조되는 쪽을 왼쪽에 세우면 회원 → 주문 → 주문상세 처럼 읽는 순서대로 늘어선다.
func TestAutoLayoutOrdersByReference(t *testing.T) {
	col := func(name string) *schema.Column { return &schema.Column{Name: name, Position: 1} }
	members := &schema.Table{Name: "members", Columns: []*schema.Column{col("id")}}
	orders := &schema.Table{
		Name:    "orders",
		Columns: []*schema.Column{col("id"), col("member_id")},
		ForeignKeys: []*schema.ForeignKey{{
			Name: "fk_orders_member", Columns: []string{"member_id"},
			RefTable: "members", RefColumns: []string{"id"},
		}},
	}
	items := &schema.Table{
		Name:    "order_items",
		Columns: []*schema.Column{col("id"), col("order_id")},
		ForeignKeys: []*schema.ForeignKey{{
			Name: "fk_items_order", Columns: []string{"order_id"},
			RefTable: "orders", RefColumns: []string{"id"},
		}},
	}
	// 목록 순서는 일부러 뒤집어 둔다 — 배치가 순서가 아니라 관계를 보는지 확인한다.
	sc := &schema.Schema{Tables: []*schema.Table{items, orders, members}}

	layout := AutoLayout(sc)
	mx := layout["members"].X
	ox := layout["orders"].X
	ix := layout["order_items"].X
	if !(mx < ox && ox < ix) {
		t.Fatalf("열 순서가 관계를 따르지 않습니다: members %.0f, orders %.0f, order_items %.0f",
			mx, ox, ix)
	}
	if ox-mx != layoutStepX || ix-ox != layoutStepX {
		t.Errorf("열 간격이 %.0f/%.0f 입니다 (기대 %.0f)", ox-mx, ix-ox, layoutStepX)
	}
}

// 한 열에 너무 많이 쌓이지 않는다.
//
// 관계가 없는 스키마에서는 모든 표가 0열에 몰린다. 그대로 두면 세로로 5000px 짜리
// 한 줄이 되어 어떤 배율에서도 한 화면에 들어오지 않는다.
func TestAutoLayoutWrapsTallColumns(t *testing.T) {
	tables := make([]*schema.Table, 0, 20)
	for i := 0; i < 20; i += 1 {
		tables = append(tables, &schema.Table{
			Name:    fmt.Sprintf("t%02d", i),
			Columns: []*schema.Column{{Name: "id", Position: 1}},
		})
	}
	sc := &schema.Schema{Tables: tables}
	layout := AutoLayout(sc)

	perColumn := map[float64]int{}
	for _, b := range layout {
		perColumn[b.X] += 1
	}
	if len(perColumn) < 2 {
		t.Fatalf("스무 개가 한 열에 쌓였습니다: %v", perColumn)
	}
	for x, n := range perColumn {
		if n > layoutRowsMax {
			t.Errorf("x=%.0f 열에 %d개가 쌓였습니다 (한계 %d)", x, n, layoutRowsMax)
		}
	}
}

// 순환 참조(A→B→A)가 있어도 끝나고, 카드가 겹치지 않는다.
//
// 열 번호는 "가리키는 표보다 오른쪽"으로 정하는데, 순환에서는 그 조건을 동시에
// 만족시킬 수 없다. 끝나지 않는 계산보다 조금 어긋난 배치가 낫다.
func TestAutoLayoutSurvivesCycle(t *testing.T) {
	a := &schema.Table{
		Name:    "a",
		Columns: []*schema.Column{{Name: "id", Position: 1}, {Name: "b_id", Position: 2}},
		ForeignKeys: []*schema.ForeignKey{{
			Name: "fk_a_b", Columns: []string{"b_id"}, RefTable: "b", RefColumns: []string{"id"},
		}},
	}
	b := &schema.Table{
		Name:    "b",
		Columns: []*schema.Column{{Name: "id", Position: 1}, {Name: "a_id", Position: 2}},
		ForeignKeys: []*schema.ForeignKey{{
			Name: "fk_b_a", Columns: []string{"a_id"}, RefTable: "a", RefColumns: []string{"id"},
		}},
	}
	sc := &schema.Schema{Tables: []*schema.Table{a, b}}

	layout := AutoLayout(sc)
	if len(layout) != 2 {
		t.Fatalf("좌표가 %d개입니다", len(layout))
	}
	if layout["a"].X == layout["b"].X && layout["a"].Y == layout["b"].Y {
		t.Errorf("두 카드가 같은 자리에 놓였습니다: %+v", layout["a"])
	}
}

// CardHeight는 그리는 쪽과 같은 값을 내야 한다(erdcanvas.js: 30 + 20n + 8).
func TestCardHeightMatchesCanvas(t *testing.T) {
	cases := map[int]float64{0: 38, 1: 58, 14: 318, 40: 838}
	for n, want := range cases {
		if got := CardHeight(n); got != want {
			t.Errorf("CardHeight(%d) = %.0f, 기대 %.0f", n, got, want)
		}
	}
}

// 카드 폭은 보낸 경우에만 바뀌고, 읽을 수 있는 범위로 잘린다.
//
// 옮기기만 하는 op가 폭을 0으로 되돌리면, 넓혀 둔 카드가 남이 한 번 끌 때마다 기본
// 폭으로 돌아간다 — 같은 문서를 함께 보는 사람에게는 "자꾸 좁아진다"로 보인다.
func TestTableMoveKeepsWidthUnlessSent(t *testing.T) {
	doc := NewDocument("d", "문서", "", "mysql")
	apply(t, doc, OpTableAdd, `{"name":"users"}`)

	// 폭을 넓힌다.
	apply(t, doc, OpTableMove, `{"key":"users","x":10,"y":20,"width":420}`)
	box := doc.Layout["users"]
	if box == nil || box.W != 420 {
		t.Fatalf("폭이 저장되지 않았습니다: %+v", box)
	}

	// 그냥 옮기기만 하면 폭은 그대로다.
	apply(t, doc, OpTableMove, `{"key":"users","x":50,"y":60}`)
	if doc.Layout["users"].W != 420 {
		t.Errorf("옮기기가 폭을 지웠습니다: %v", doc.Layout["users"].W)
	}

	// 범위 밖은 잘린다. 화면을 거치지 않는 길(AI 툴·API)로도 들어올 수 있다.
	apply(t, doc, OpTableMove, `{"key":"users","x":50,"y":60,"width":5}`)
	if got := doc.Layout["users"].W; got != cardMinW {
		t.Errorf("너무 좁은 폭 = %v, %v로 잘려야 합니다", got, cardMinW)
	}
	apply(t, doc, OpTableMove, `{"key":"users","x":50,"y":60,"width":9000}`)
	if got := doc.Layout["users"].W; got != cardMaxW {
		t.Errorf("너무 넓은 폭 = %v, %v로 잘려야 합니다", got, cardMaxW)
	}

	// 0은 "정하지 않음"이라 그대로 둔다(화면이 기본값을 쓴다).
	apply(t, doc, OpTableMove, `{"key":"users","x":50,"y":60,"width":0}`)
	if got := doc.Layout["users"].W; got != 0 {
		t.Errorf("0 = %v, 기본값으로 되돌아가야 합니다", got)
	}
}
