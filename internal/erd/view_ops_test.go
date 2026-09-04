package erd

import (
	"encoding/json"
	"strings"
	"testing"

	"dbstudio/internal/schema"
)

func viewDoc(t *testing.T) *Document {
	t.Helper()
	return &Document{
		ID:     "d1",
		Schema: &schema.Schema{Tables: []*schema.Table{{Name: "orders"}}},
		Layout: map[string]*Box{"orders": {X: 10, Y: 20}},
	}
}

func do(t *testing.T, doc *Document, kind Kind, payload string) error {
	t.Helper()
	return Apply(doc, &Op{ID: "op", Kind: kind, Payload: json.RawMessage(payload)})
}

// 뷰를 더하면 스키마와 좌표에 함께 들어가야 한다. 좌표가 없으면 카드가 화면 왼쪽 위에
// 겹쳐 쌓이고, 여러 개를 만든 사람은 하나만 만들어진 줄 안다.
func TestViewAddPlacesCard(t *testing.T) {
	doc := viewDoc(t)
	if err := do(t, doc, OpViewAdd,
		`{"name":"daily_sales","definition":"SELECT 1;","x":100,"y":200}`); err != nil {
		t.Fatalf("view.add: %v", err)
	}
	if len(doc.Schema.Views) != 1 {
		t.Fatalf("뷰 수: %d", len(doc.Schema.Views))
	}
	v := doc.Schema.Views[0]
	// 끝의 세미콜론은 떼어야 한다. CREATE VIEW … AS 뒤에 그대로 붙기 때문이다.
	if v.Definition != "SELECT 1" {
		t.Errorf("정의: %q", v.Definition)
	}
	box := doc.Layout["daily_sales"]
	if box == nil || box.X != 100 || box.Y != 200 {
		t.Errorf("좌표: %+v", box)
	}
}

// 표와 뷰는 한 스키마에서 이름을 나눠 쓴다. 겹친 채로 담으면 그 초안이 만드는 DDL 은
// 대상 DB 가 거부하고, 그 사실은 마이그레이션을 실행하는 순간에야 드러난다.
func TestViewNameCannotCollideWithTable(t *testing.T) {
	doc := viewDoc(t)
	err := do(t, doc, OpViewAdd, `{"name":"orders","definition":"SELECT 1"}`)
	if err == nil {
		t.Fatal("표 이름과 겹치는 뷰가 만들어졌습니다")
	}
	if !strings.Contains(err.Error(), "테이블 이름") {
		t.Errorf("거절 사유가 이유를 말하지 않습니다: %v", err)
	}
}

// 정의가 없는 뷰는 DDL 로 만들 수 없다. 만들어 두면 내보내기에서 경고만 남고
// 그 뷰는 어디에도 만들어지지 않는다.
func TestViewNeedsDefinition(t *testing.T) {
	doc := viewDoc(t)
	if err := do(t, doc, OpViewAdd, `{"name":"v1"}`); err == nil {
		t.Fatal("정의 없는 뷰가 만들어졌습니다")
	}
	if err := do(t, doc, OpViewAdd, `{"name":"v1","definition":"   "}`); err == nil {
		t.Fatal("빈 정의로 뷰가 만들어졌습니다")
	}
}

// 이름을 바꾸면 좌표도 새 열쇠로 따라가야 한다. 따라가지 않으면 카드가 화면 왼쪽
// 위로 튀고, 사람은 자기가 놓아 둔 자리를 잃는다.
func TestViewRenameKeepsPosition(t *testing.T) {
	doc := viewDoc(t)
	if err := do(t, doc, OpViewAdd,
		`{"name":"sales","definition":"SELECT 1","x":300,"y":400}`); err != nil {
		t.Fatalf("view.add: %v", err)
	}
	if err := do(t, doc, OpViewUpdate, `{"key":"sales","name":"sales_daily"}`); err != nil {
		t.Fatalf("view.update: %v", err)
	}
	if doc.Layout["sales"] != nil {
		t.Error("옛 열쇠의 좌표가 남아 있습니다")
	}
	box := doc.Layout["sales_daily"]
	if box == nil || box.X != 300 || box.Y != 400 {
		t.Errorf("좌표가 따라오지 않았습니다: %+v", box)
	}
	// 정의는 건드리지 않았으므로 그대로여야 한다(패치 규칙).
	if doc.Schema.Views[0].Definition != "SELECT 1" {
		t.Errorf("정의가 사라졌습니다: %q", doc.Schema.Views[0].Definition)
	}
}

func TestViewDeleteRemovesLayout(t *testing.T) {
	doc := viewDoc(t)
	if err := do(t, doc, OpViewAdd, `{"name":"v1","definition":"SELECT 1"}`); err != nil {
		t.Fatalf("view.add: %v", err)
	}
	if err := do(t, doc, OpViewDelete, `{"key":"v1"}`); err != nil {
		t.Fatalf("view.delete: %v", err)
	}
	if len(doc.Schema.Views) != 0 {
		t.Errorf("뷰가 남았습니다: %+v", doc.Schema.Views)
	}
	if _, ok := doc.Layout["v1"]; ok {
		t.Error("좌표가 남았습니다")
	}
}

// view.move 는 좌표만 바꾸는 op 다. 구조 op 로 잡히면 뷰 카드를 옮길 때마다 문서
// 지문이 바뀌어, 대상 DB 와 다르다고(드리프트) 보고된다.
func TestViewMoveIsNotStructural(t *testing.T) {
	if OpViewMove.Structural() {
		t.Error("view.move 가 구조 op 로 잡힙니다")
	}
	for _, k := range []Kind{OpViewAdd, OpViewUpdate, OpViewDelete} {
		if !k.Structural() {
			t.Errorf("%s 가 구조 op 가 아닙니다", k)
		}
	}
	doc := viewDoc(t)
	if err := do(t, doc, OpViewAdd, `{"name":"v1","definition":"SELECT 1"}`); err != nil {
		t.Fatalf("view.add: %v", err)
	}
	if err := do(t, doc, OpViewMove, `{"key":"v1","x":11,"y":22}`); err != nil {
		t.Fatalf("view.move: %v", err)
	}
	if box := doc.Layout["v1"]; box == nil || box.X != 11 || box.Y != 22 {
		t.Errorf("좌표: %+v", doc.Layout["v1"])
	}
}

// 빈 초안에 뷰만 있는 스크립트를 불러오면 그 뷰도 자리를 받아야 한다.
func TestAutoLayoutPlacesViews(t *testing.T) {
	sc := &schema.Schema{
		Tables: []*schema.Table{{Name: "orders"}, {Name: "members"}},
		Views:  []*schema.View{{Name: "daily"}, {Name: "monthly"}},
	}
	out := AutoLayout(sc)
	for _, key := range []string{"orders", "members", "daily", "monthly"} {
		if out[key] == nil {
			t.Fatalf("%s 의 자리가 없습니다", key)
		}
	}
	// 뷰는 표 오른쪽에 선다 — 무엇이 실체이고 무엇이 그것을 읽는 것인지가 보여야 한다.
	if out["daily"].X <= out["orders"].X {
		t.Errorf("뷰가 표 오른쪽에 있지 않습니다: 뷰 %v, 표 %v", out["daily"].X, out["orders"].X)
	}
	if out["daily"].X != out["monthly"].X || out["daily"].Y == out["monthly"].Y {
		t.Errorf("뷰 둘이 한 자리에 겹칩니다: %+v / %+v", out["daily"], out["monthly"])
	}
}
