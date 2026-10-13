package api

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"dbstudio/internal/erd"
)

// 흐름 배치가 지키는 것: 부모는 왼쪽, 자식은 오른쪽, 선은 남의 카드를 지나지 않는다.
// 숫자로 확인할 수 있어야 모델도 확인할 수 있다.

// shopFixture는 회원 ← 주문 ← 주문상세 → 상품, 그리고 아무와도 이어지지 않은 설정 표다.
func shopFixture(t *testing.T) (*testEnv, *erdToolContext, map[string]*erdTool) {
	t.Helper()
	e := newTestEnv(t)
	c := login(t, e, "alice")
	docID := createStandalone(t, e, c, "흐름", "postgres")
	_, impls := erdAITools()
	ec := &erdToolContext{
		tc:    &toolContext{ctx: context.Background(), srv: e.srv, user: e.user},
		docID: docID,
	}
	run := func(tool, body string) {
		t.Helper()
		if _, err := impls[tool].Run(ec, json.RawMessage(body)); err != nil {
			t.Fatalf("%s %s: %v", tool, body, err)
		}
	}
	// 일부러 자식부터 만든다. 만든 순서대로 놓이면 흐름이 거꾸로 선다.
	for _, name := range []string{"order_items", "orders", "settings", "products", "users"} {
		run("add_table", fmt.Sprintf(
			`{"name":%q,"columns":[{"name":"id","type":"bigint"},{"name":"ref_a","type":"bigint"},{"name":"ref_b","type":"bigint"}],"primaryKey":["id"]}`, name))
	}
	run("add_foreign_key", `{"table":"orders","name":"fk1","columns":["ref_a"],"refTable":"users","refColumns":["id"]}`)
	run("add_foreign_key", `{"table":"order_items","name":"fk2","columns":["ref_a"],"refTable":"orders","refColumns":["id"]}`)
	run("add_foreign_key", `{"table":"order_items","name":"fk3","columns":["ref_b"],"refTable":"products","refColumns":["id"]}`)
	return e, ec, impls
}

func centerOf(r erd.Rect) (float64, float64) { return r.X + r.W/2, r.Y + r.H/2 }

func TestERDFlowLayoutPutsParentsLeft(t *testing.T) {
	e, ec, impls := shopFixture(t)

	// 순서를 거꾸로 적어도 흐름이 자리를 정해야 한다.
	out, err := impls["arrange_layout"].Run(ec, json.RawMessage(`{
		"groups":[{"new":true,"label":"주문","x":0,"y":0,
			"tables":["order_items","orders","settings","products","users"]}]
	}`))
	if err != nil {
		t.Fatalf("arrange_layout: %v", err)
	}
	r := layoutDoc(t, e, ec).CardRects()
	ux, uy := centerOf(r["users"])
	ox, oy := centerOf(r["orders"])
	ix, _ := centerOf(r["order_items"])
	px, _ := centerOf(r["products"])
	sx, _ := centerOf(r["settings"])
	if !(ux < ox && ox < ix) {
		t.Errorf("users → orders → order_items 가 왼쪽에서 오른쪽으로 서지 않았습니다: %v %v %v", ux, ox, ix)
	}
	if !(px < ix) {
		t.Errorf("products 가 그것을 가리키는 order_items 보다 왼쪽에 있어야 합니다: %v %v", px, ix)
	}
	// 자식은 부모 높이에 맞춘다 — 선이 곧은 가로선이 된다.
	if uy != oy {
		t.Errorf("orders 가 users 와 같은 높이가 아닙니다: %v %v", uy, oy)
	}
	// 아무와도 이어지지 않은 표는 맨 오른쪽 열이다. 이어진 것들 사이에 끼면 선이 그 위를 지난다.
	if !(sx > ix) {
		t.Errorf("settings 가 맨 오른쪽 열이 아닙니다: %v (order_items %v)", sx, ix)
	}

	var res struct {
		Flow flowStats `json:"flow"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("결과가 JSON 이 아닙니다: %v\n%s", err, out)
	}
	if res.Flow.Edges != 3 || res.Flow.Backward != 0 || res.Flow.ThroughCards != 0 || res.Flow.Crossings != 0 {
		t.Errorf("흐름 점수 = %+v", res.Flow)
	}
}

// 묶음끼리도 같은 방향이어야 한다. 묶음 안만 흐르고 묶음 사이 선이 거꾸로 가면
// 도면 전체로는 여전히 엉켜 보인다.
func TestERDAutoPlaceOrdersGroupsByRelations(t *testing.T) {
	e, ec, impls := shopFixture(t)

	out, err := impls["arrange_layout"].Run(ec, json.RawMessage(`{
		"autoPlace":true, "origin":{"x":100,"y":100},
		"groups":[
			{"new":true,"label":"주문","tables":["orders","order_items"]},
			{"new":true,"label":"회원","tables":["users"]},
			{"new":true,"label":"상품","tables":["products"]}
		],
		"blocks":[{"tables":["settings"]}]
	}`))
	if err != nil {
		t.Fatalf("arrange_layout: %v", err)
	}
	doc := layoutDoc(t, e, ec)
	byLabel := map[string]erd.Rect{}
	for _, g := range doc.Groups {
		byLabel[g.Label] = g.Rect()
	}
	if !(byLabel["회원"].X < byLabel["주문"].X && byLabel["상품"].X < byLabel["주문"].X) {
		t.Errorf("부모 묶음(회원·상품)이 주문보다 왼쪽에 있어야 합니다: %+v", byLabel)
	}
	if byLabel["회원"].Overlaps(byLabel["상품"]) || byLabel["회원"].Overlaps(byLabel["주문"]) ||
		byLabel["상품"].Overlaps(byLabel["주문"]) {
		t.Errorf("묶음끼리 겹칩니다: %+v", byLabel)
	}
	minX, minY := 1e9, 1e9
	for _, r := range byLabel {
		minX, minY = min(minX, r.X), min(minY, r.Y)
	}
	if minX != 100 || minY != 100 {
		t.Errorf("origin 에서 시작하지 않았습니다: (%v, %v)", minX, minY)
	}
	var res struct {
		Flow     flowStats `json:"flow"`
		Overlaps []string  `json:"overlaps"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("결과가 JSON 이 아닙니다: %v", err)
	}
	if res.Flow.Backward != 0 || len(res.Overlaps) > 0 {
		t.Errorf("흐름 %+v, 겹침 %v", res.Flow, res.Overlaps)
	}
}

// 지표는 나쁜 배치를 나쁘다고 말해야 한다. 늘 0을 돌려주는 지표는 모델을 안심시킬 뿐이다.
func TestERDFlowMetricsFlagBadLayout(t *testing.T) {
	_, ec, impls := shopFixture(t)

	// 자식을 부모 왼쪽에 두고, users 와 orders 사이 한가운데에 settings 를 끼운다.
	if _, err := impls["arrange_layout"].Run(ec, json.RawMessage(`{"tables":[
		{"table":"orders","x":0,"y":0},
		{"table":"settings","x":400,"y":0},
		{"table":"users","x":800,"y":0},
		{"table":"order_items","x":0,"y":600},
		{"table":"products","x":800,"y":600}
	]}`)); err != nil {
		t.Fatalf("arrange_layout: %v", err)
	}
	out, err := impls["read_layout"].Run(ec, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("read_layout: %v", err)
	}
	var res struct {
		Flow flowStats `json:"flow"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("결과가 JSON 이 아닙니다: %v", err)
	}
	if res.Flow.Backward < 2 {
		t.Errorf("거꾸로 선 선을 세지 못했습니다: %+v", res.Flow)
	}
	if res.Flow.ThroughCards < 1 || len(res.Flow.ThroughList) == 0 {
		t.Errorf("settings 를 관통하는 선을 세지 못했습니다: %+v", res.Flow)
	}
}

func TestSegmentGeometry(t *testing.T) {
	p := func(x, y float64) flowPoint { return flowPoint{x, y} }
	if !segmentsCross(p(0, 0), p(10, 10), p(0, 10), p(10, 0)) {
		t.Error("X 자 교차를 못 봤습니다")
	}
	if segmentsCross(p(0, 0), p(10, 0), p(0, 5), p(10, 5)) {
		t.Error("평행선을 교차로 봤습니다")
	}
	if !segmentHitsRect(p(0, 5), p(100, 5), erd.Rect{X: 40, Y: 0, W: 20, H: 10}) {
		t.Error("사각형을 지나는 선을 못 봤습니다")
	}
	if segmentHitsRect(p(0, 50), p(100, 50), erd.Rect{X: 40, Y: 0, W: 20, H: 10}) {
		t.Error("비켜 가는 선을 지난다고 봤습니다")
	}
}
