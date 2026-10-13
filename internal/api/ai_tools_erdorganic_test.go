package api

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"

	"dbstudio/internal/erd"
)

// 손으로 그린 것 같은 배치(organic)가 지키는 것:
//   - 허브의 자식은 허브 가까이에 선다
//   - 두 무리를 잇는 표는 그 사이에 선다
//   - 아무와도 이어지지 않은 표는 도면 아래에 모인다
//   - 겹치거나 그룹 테두리에 걸친 카드가 없다
//   - 같은 도면에서 같은 결과가 나온다

// hubsFixture는 허브 둘(members·songs)과 각자의 자식 셋, 두 허브를 함께 가리키는 bridge,
// 그리고 아무와도 이어지지 않은 lonely 다.
func hubsFixture(t *testing.T) (*testEnv, *erdToolContext, map[string]*erdTool) {
	t.Helper()
	e := newTestEnv(t)
	c := login(t, e, "alice")
	docID := createStandalone(t, e, c, "손그림", "postgres")
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
	names := []string{"lonely", "bridge", "s3", "s2", "s1", "songs", "m3", "m2", "m1", "members"}
	for i, n := range names {
		cols := []string{`{"name":"id","type":"bigint"}`, `{"name":"a","type":"bigint"}`, `{"name":"b","type":"bigint"}`}
		for k := 0; k < i%4; k++ {
			cols = append(cols, fmt.Sprintf(`{"name":"x%d","type":"text"}`, k))
		}
		run("add_table", fmt.Sprintf(`{"name":%q,"columns":[%s],"primaryKey":["id"]}`, n, strings.Join(cols, ",")))
	}
	fk := 0
	link := func(child, col, parent string) {
		fk++
		run("add_foreign_key", fmt.Sprintf(
			`{"table":%q,"name":"fk%d","columns":[%q],"refTable":%q,"refColumns":["id"]}`, child, fk, col, parent))
	}
	for _, k := range []string{"m1", "m2", "m3"} {
		link(k, "a", "members")
	}
	for _, k := range []string{"s1", "s2", "s3"} {
		link(k, "a", "songs")
	}
	link("bridge", "a", "members")
	link("bridge", "b", "songs")
	return e, ec, impls
}

const hubGroups = `[
	{"new":true,"label":"회원","tables":["members","m1","m2","m3"]},
	{"new":true,"label":"음원","tables":["songs","s1","s2","s3"]}]`

func center(r erd.Rect) (float64, float64) { return r.X + r.W/2, r.Y + r.H/2 }

func TestERDOrganicLooksHandDrawn(t *testing.T) {
	e, ec, impls := hubsFixture(t)

	out, err := impls["arrange_layout"].Run(ec, json.RawMessage(`{"mode":"organic","groups":`+hubGroups+`}`))
	if err != nil {
		t.Fatalf("arrange_layout: %v", err)
	}
	var res struct {
		Flow      flowStats `json:"flow"`
		Overlaps  []string  `json:"overlaps"`
		Straddles []string  `json:"straddlingGroupEdge"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("결과가 JSON 이 아닙니다: %v\n%s", err, out)
	}
	if len(res.Overlaps) > 0 || len(res.Straddles) > 0 {
		t.Errorf("겹침 %v, 테두리 걸침 %v", res.Overlaps, res.Straddles)
	}
	if res.Flow.ThroughCards != 0 || res.Flow.Crossings != 0 {
		t.Errorf("단순한 도면인데 선이 카드를 지나거나 엇갈립니다: %+v", res.Flow)
	}

	doc := layoutDoc(t, e, ec)
	r := doc.CardRects()

	// 허브의 자식은 허브 가까이에. 테두리 사이가 카드 한 장 폭을 넘지 않는다.
	for hub, kids := range map[string][]string{"members": {"m1", "m2", "m3"}, "songs": {"s1", "s2", "s3"}} {
		for _, k := range kids {
			a, b := attach(r[k], r[hub])
			if l := math.Hypot(b.x-a.x, b.y-a.y); l > 260 {
				t.Errorf("%s → %s 선이 %.0fpx 입니다. 허브의 자식은 허브 가까이에 서야 합니다", k, hub, l)
			}
		}
	}

	// 두 무리를 잇는 표는 그 사이에. 두 허브를 잇는 선분에 내린 발이 선분 가운데쯤이고,
	// 두 허브로 가는 선이 모두 짧다. (두 그룹이 붙어 있으면 그 틈에는 카드가 못 들어가므로
	// 틈의 옆 여백에 선다 — 그래서 선분과의 거리가 아니라 두 선의 길이로 본다.)
	mx, my := center(r["members"])
	sx, sy := center(r["songs"])
	bx, by := center(r["bridge"])
	vx, vy := sx-mx, sy-my
	tt := ((bx-mx)*vx + (by-my)*vy) / (vx*vx + vy*vy)
	if tt < 0.2 || tt > 0.8 {
		t.Errorf("bridge 가 두 허브 사이에 있지 않습니다 (선분 위치 %.2f)", tt)
	}
	for _, hub := range []string{"members", "songs"} {
		a, b := attach(r["bridge"], r[hub])
		if l := math.Hypot(b.x-a.x, b.y-a.y); l > 400 {
			t.Errorf("bridge → %s 선이 %.0fpx 입니다", hub, l)
		}
	}

	// 고립된 표는 맨 아래.
	lonelyTop := r["lonely"].Y
	for k, rc := range r {
		if k != "lonely" && rc.Y+rc.H > lonelyTop {
			t.Errorf("lonely(%v) 가 %s(아래 끝 %v) 보다 위에 있습니다", lonelyTop, k, rc.Y+rc.H)
		}
	}
	for _, g := range doc.Groups {
		if g.Y+g.H > lonelyTop {
			t.Errorf("lonely 가 그룹 %s 보다 위에 있습니다", g.Label)
		}
	}
}

// 같은 도면·같은 묶음이면 같은 그림이어야 한다. 힘 배치에 난수를 쓰면 "정리" 버튼을 누를
// 때마다 도면이 달라져, 사람이 익힌 자리가 매번 사라진다.
func TestERDOrganicIsDeterministic(t *testing.T) {
	e, ec, impls := hubsFixture(t)
	if _, err := impls["arrange_layout"].Run(ec, json.RawMessage(`{"mode":"organic","groups":`+hubGroups+`}`)); err != nil {
		t.Fatalf("arrange_layout: %v", err)
	}
	first := layoutDoc(t, e, ec).CardRects()

	// 기존 그룹을 적지 않아도 지금 든 표를 그대로 데려가므로 같은 묶음이다.
	if _, err := impls["arrange_layout"].Run(ec, json.RawMessage(`{"mode":"organic"}`)); err != nil {
		t.Fatalf("arrange_layout: %v", err)
	}
	doc := layoutDoc(t, e, ec)
	second := doc.CardRects()
	for k, r := range first {
		if second[k] != r {
			t.Errorf("%s 의 자리가 달라졌습니다: %+v → %+v", k, r, second[k])
		}
	}
	// 기존 그룹의 표는 그 그룹 안에 남아 있어야 한다.
	for _, g := range doc.Groups {
		want := map[string][]string{"회원": {"members", "m1", "m2", "m3"}, "음원": {"songs", "s1", "s2", "s3"}}[g.Label]
		for _, k := range want {
			if !g.Rect().Contains(second[k]) {
				t.Errorf("%s 가 그룹 %s 밖으로 나왔습니다", k, g.Label)
			}
		}
	}
}

func TestERDOrganicRejectsManualFields(t *testing.T) {
	_, ec, impls := hubsFixture(t)
	for _, body := range []string{
		`{"mode":"organic","tables":[{"table":"m1","x":0,"y":0}]}`,
		`{"mode":"organic","blocks":[{"tables":["m1"]}]}`,
		`{"mode":"organic","autoPlace":true}`,
		`{"mode":"organic","groups":[{"new":true,"label":"빈 그룹"}]}`,
		`{"mode":"sideways"}`,
	} {
		if _, err := impls["arrange_layout"].Run(ec, json.RawMessage(body)); err == nil {
			t.Errorf("통과했습니다: %s", body)
		}
	}
}

// autoPlace 에서 묶음에 적지 않은 그룹 밖 표도 함께 놓여야 한다. 제자리에 남으면 새로 놓인
// 묶음이 그 위를 덮어, 표가 남의 그룹 안에 갇힌 것처럼 보인다.
func TestERDAutoPlaceCarriesLooseTables(t *testing.T) {
	_, ec, impls := shopFixture(t)
	out, err := impls["arrange_layout"].Run(ec, json.RawMessage(`{
		"autoPlace":true,
		"groups":[{"new":true,"label":"주문","tables":["orders","order_items"]}]
	}`))
	if err != nil {
		t.Fatalf("arrange_layout: %v", err)
	}
	if strings.Contains(out, "overlaps") || strings.Contains(out, "straddlingGroupEdge") {
		t.Errorf("그룹 밖 표가 묶음과 겹쳤습니다: %s", out)
	}
}
