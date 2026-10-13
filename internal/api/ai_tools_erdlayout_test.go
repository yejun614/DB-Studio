package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"dbstudio/internal/erd"
)

// 배치 툴은 좌표를 다룬다. 확인할 것은 "옮겼다고 말한 대로 문서가 옮겨졌는가"와
// "옮긴 결과가 겹치지 않는가" 두 가지다 — 겹친 카드는 사람이 잡을 수도 없다.

func layoutFixture(t *testing.T) (*testEnv, *erdToolContext, map[string]*erdTool) {
	t.Helper()
	e := newTestEnv(t)
	c := login(t, e, "alice")
	docID := createStandalone(t, e, c, "배치", "postgres")

	_, impls := erdAITools()
	ec := &erdToolContext{
		tc:    &toolContext{ctx: context.Background(), srv: e.srv, user: e.user},
		docID: docID,
	}
	for _, body := range []string{
		`{"name":"users","columns":[{"name":"id","type":"bigint"},{"name":"email","type":"text"}],"primaryKey":["id"]}`,
		`{"name":"orders","columns":[{"name":"id","type":"bigint"},{"name":"user_id","type":"bigint"},{"name":"total","type":"numeric"},{"name":"memo","type":"text"},{"name":"at","type":"timestamp"}],"primaryKey":["id"]}`,
		`{"name":"items","columns":[{"name":"id","type":"bigint"}],"primaryKey":["id"]}`,
	} {
		if _, err := impls["add_table"].Run(ec, json.RawMessage(body)); err != nil {
			t.Fatalf("add_table: %v", err)
		}
	}
	if _, err := impls["add_foreign_key"].Run(ec, json.RawMessage(
		`{"table":"orders","name":"fk_orders_user","columns":["user_id"],"refTable":"users","refColumns":["id"]}`)); err != nil {
		t.Fatalf("add_foreign_key: %v", err)
	}
	return e, ec, impls
}

func layoutDoc(t *testing.T, e *testEnv, ec *erdToolContext) *erd.Document {
	t.Helper()
	doc, err := e.st.GetERDDocument(context.Background(), ec.docID)
	if err != nil {
		t.Fatalf("get document: %v", err)
	}
	return doc
}

func TestERDArrangeFillsGroupWithoutOverlap(t *testing.T) {
	e, ec, impls := layoutFixture(t)

	out, err := impls["arrange_layout"].Run(ec, json.RawMessage(`{
		"groups":[{"new":true,"label":"주문","x":1000,"y":500,"tables":["users","orders","items"],"layout":"grid","columns":2}]
	}`))
	if err != nil {
		t.Fatalf("arrange_layout: %v", err)
	}
	if strings.Contains(out, "overlaps") || strings.Contains(out, "straddling") {
		t.Errorf("서버가 계산한 격자인데 겹침이 보고됐습니다: %s", out)
	}

	doc := layoutDoc(t, e, ec)
	if len(doc.Groups) != 1 {
		t.Fatalf("그룹 수 = %d", len(doc.Groups))
	}
	g := doc.Groups[0]
	if g.Label != "주문" || g.X != 1000 || g.Y != 500 {
		t.Errorf("그룹 = %+v", g)
	}
	rects := doc.CardRects()
	keys := []string{"users", "orders", "items"}
	for i, k := range keys {
		r, ok := rects[k]
		if !ok {
			t.Fatalf("%s 의 좌표가 없습니다", k)
		}
		if !g.Rect().Contains(r) {
			t.Errorf("%s(%+v) 가 그룹(%+v) 밖으로 나갔습니다", k, r, g.Rect())
		}
		for _, other := range keys[i+1:] {
			if r.Overlaps(rects[other]) {
				t.Errorf("%s 와 %s 가 겹칩니다", k, other)
			}
		}
	}
	// 적은 순서대로 왼쪽→오른쪽, 위→아래다. 순서가 지켜지지 않으면 모델이 관계를
	// 따라 고른 이웃이 의미를 잃는다.
	if !(rects["users"].X < rects["orders"].X && rects["users"].Y == rects["orders"].Y) {
		t.Errorf("첫 줄 순서가 다릅니다: users %+v, orders %+v", rects["users"], rects["orders"])
	}
	if !(rects["items"].Y > rects["users"].Y && rects["items"].X == rects["users"].X) {
		t.Errorf("둘째 줄 자리가 다릅니다: items %+v", rects["items"])
	}
}

// 그룹만 옮기면 안에 든 카드도 따라와야 한다. 안 따라오면 그룹은 빈 상자가 되고
// 카드들은 아무 묶음에도 속하지 않은 채 남는다.
func TestERDArrangeMovesGroupContents(t *testing.T) {
	e, ec, impls := layoutFixture(t)
	if _, err := impls["arrange_layout"].Run(ec, json.RawMessage(`{
		"groups":[{"new":true,"label":"회원","x":0,"y":0,"tables":["users"]}],
		"tables":[{"table":"items","x":2000,"y":2000}]
	}`)); err != nil {
		t.Fatalf("arrange_layout: %v", err)
	}
	before := layoutDoc(t, e, ec)
	u0 := before.CardRects()["users"]
	i0 := before.CardRects()["items"]

	out, err := impls["arrange_layout"].Run(ec, json.RawMessage(`{"groups":[{"group":"회원","x":300,"y":100}]}`))
	if err != nil {
		t.Fatalf("arrange_layout: %v", err)
	}
	after := layoutDoc(t, e, ec)
	u1 := after.CardRects()["users"]
	if u1.X != u0.X+300 || u1.Y != u0.Y+100 {
		t.Errorf("그룹 안 카드가 따라오지 않았습니다: %+v → %+v (%s)", u0, u1, out)
	}
	if after.CardRects()["items"] != i0 {
		t.Error("그룹 밖 카드까지 움직였습니다")
	}

	// moveContents=false 면 그룹만 간다.
	if _, err := impls["arrange_layout"].Run(ec, json.RawMessage(
		`{"groups":[{"group":"회원","x":5000,"y":5000,"moveContents":false}]}`)); err != nil {
		t.Fatalf("arrange_layout: %v", err)
	}
	if layoutDoc(t, e, ec).CardRects()["users"] != u1 {
		t.Error("moveContents=false 인데 카드가 움직였습니다")
	}
}

// 잘못된 지시는 하나도 반영하지 않고 거절해야 한다. 절반만 옮겨진 도면은 처음보다 나쁘다.
func TestERDArrangeRejectsBeforeApplying(t *testing.T) {
	e, ec, impls := layoutFixture(t)
	before := layoutDoc(t, e, ec)

	cases := map[string]string{
		"없는 표":       `{"tables":[{"table":"users","x":1,"y":1},{"table":"ghost","x":1,"y":1}]}`,
		"두 곳에서 정한 표": `{"tables":[{"table":"users","x":1,"y":1}],"groups":[{"new":true,"label":"g","x":0,"y":0,"tables":["users"]}]}`,
		"좌표 없음":      `{"tables":[{"table":"users"}]}`,
		"이름 없는 새 그룹": `{"groups":[{"new":true,"x":0,"y":0}]}`,
		"없는 그룹":      `{"groups":[{"group":"없음","x":0,"y":0}]}`,
		"빈 요청":       `{}`,
	}
	for name, body := range cases {
		if _, err := impls["arrange_layout"].Run(ec, json.RawMessage(body)); err == nil {
			t.Errorf("%s: 통과했습니다", name)
		}
	}
	after := layoutDoc(t, e, ec)
	if after.Seq != before.Seq {
		t.Errorf("거절된 요청이 문서를 바꿨습니다 (seq %d → %d)", before.Seq, after.Seq)
	}
}

// read_layout 은 배치를 고치기 전에 필요한 것을 한 번에 준다: 좌표·크기·소속·관계·겹침.
func TestERDReadLayoutReportsGeometry(t *testing.T) {
	_, ec, impls := layoutFixture(t)
	if _, err := impls["arrange_layout"].Run(ec, json.RawMessage(`{
		"groups":[{"new":true,"label":"주문","x":0,"y":0,"tables":["orders"]}],
		"tables":[{"table":"users","x":3000,"y":0},{"table":"items","x":3100,"y":20}]
	}`)); err != nil {
		t.Fatalf("arrange_layout: %v", err)
	}
	out, err := impls["read_layout"].Run(ec, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("read_layout: %v", err)
	}
	var res struct {
		Cards []struct {
			Name  string  `json:"name"`
			H     float64 `json:"h"`
			Group string  `json:"group"`
		} `json:"cards"`
		Relations []string `json:"relations"`
		Overlaps  []string `json:"overlaps"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("결과가 JSON 이 아닙니다: %v\n%s", err, out)
	}
	byName := map[string]int{}
	for i, c := range res.Cards {
		byName[c.Name] = i
	}
	if c := res.Cards[byName["orders"]]; c.Group != "주문" || c.H != erd.CardHeight(5) {
		t.Errorf("orders = %+v", c)
	}
	if len(res.Relations) != 1 || res.Relations[0] != "orders → users" {
		t.Errorf("관계 = %v", res.Relations)
	}
	if len(res.Overlaps) != 1 || !strings.Contains(res.Overlaps[0], "users") || !strings.Contains(res.Overlaps[0], "items") {
		t.Errorf("겹침 = %v", res.Overlaps)
	}
}

// 배치 툴은 일반 어시스턴트·MCP 에도 erd_ 접두사로 함께 나가야 한다.
func TestERDLayoutToolsAreExposedToAssistant(t *testing.T) {
	names := map[string]bool{}
	for _, tool := range erdDocTools() {
		names[tool.Name] = true
	}
	for _, want := range []string{"erd_read_layout", "erd_arrange_layout", "erd_set_appearance"} {
		if !names[want] {
			t.Errorf("%s 이(가) 일반 툴 상자에 없습니다", want)
		}
	}
}
