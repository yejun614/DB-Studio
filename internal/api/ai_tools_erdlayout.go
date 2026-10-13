package api

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/google/uuid"

	"dbstudio/internal/erd"
	"dbstudio/internal/schema"
)

// ERD 도면의 배치를 읽고 고치는 툴.
//
// 배치는 구조가 아니다. 좌표·그룹은 마이그레이션에도 구조 지문에도 들어가지 않으므로
// 다른 초안 편집 툴처럼 승인 없이 바로 반영된다. 대신 **한 번 부른 재배치 전체가 한
// 묶음(batch)** 이 된다 — 표 마흔 개를 옮긴 것을 Ctrl+Z 마흔 번으로 되돌리게 하면
// 그것은 되돌리기가 아니라 고장이다. 사람이 여러 개를 골라 끌 때와 같은 규칙이다.
//
// 좌표 계산을 모델에게 전부 맡기지 않는 이유: 카드 높이는 컬럼 수로 정해지고 폭은
// 카드마다 다르다. 모델이 그 덧셈을 표마다 하면 반드시 몇 개는 틀리고, 틀린 것은
// 겹친 카드로 나타난다. 관계선의 흐름은 더 그렇다 — 표 스무 개의 열과 순서를 머릿속으로
// 정해 선이 엇갈리지 않게 하는 것은 계산이지 판단이 아니다. 그래서 "이 표들을 한 묶음으로"
// 만 받으면 흐름 배치(ai_tools_erdflow.go)와 그룹 크기 맞추기는 서버가 한다. 모델은
// **무엇을 함께 둘지**를 정하고, **어느 열 몇 px 인지**는 서버가 정한다.

const (
	// 그룹 안 격자의 여백. 머리 여백은 그룹 이름(erdcanvas.js 의 y=22)이 카드에
	// 가리지 않을 만큼이다.
	arrangePadX     = 24.0
	arrangePadTop   = 44.0
	arrangePadBot   = 24.0
	arrangeGapDef   = 40.0
	arrangeGapMin   = 16.0
	arrangeGapMax   = 200.0
	arrangeMaxOps   = 600
	arrangeCoordAbs = 100_000.0

	// read_layout 한 번에 담을 대략의 바이트 수(read_schema 와 같은 이유로 상한보다 낮다).
	erdLayoutBudget = 16_000
	// 결과에 싣는 겹침 목록의 상한. 수백 개를 다 적어도 모델이 읽는 것은 앞쪽뿐이다.
	arrangeIssueMax = 30
)

// erdLayoutTools는 배치 툴 정의다. erdTools 가 이것을 함께 등록한다.
func erdLayoutTools() []*erdTool {
	packFields := func(extra map[string]any) map[string]any {
		props := map[string]any{
			"tables": map[string]any{
				"type": "array",
				"description": "채울 표 이름. layout=flow 면 순서는 동점일 때만 쓰이고 자리는 관계가 정한다. " +
					"layout=grid 면 적은 순서대로 왼쪽→오른쪽, 위→아래",
				"items": map[string]any{"type": "string"},
			},
			"layout": str("flow(기본): 관계 방향으로 열을 나눠 부모를 왼쪽, 자식을 오른쪽에 두고 " +
				"선이 엇갈리지 않게 순서를 정하며 자식을 부모 높이에 맞춘다. " +
				"grid: 관계와 상관없이 적은 순서대로 격자에 채운다(관계가 거의 없는 묶음용)"),
			"columns": num("layout=grid 일 때 열 수 (생략하면 표 수의 제곱근)"),
		}
		for k, v := range extra {
			props[k] = v
		}
		return objectSchema(props)
	}

	return []*erdTool{
		{
			Name: "read_layout",
			Description: "이 초안의 도면 배치를 읽는다. 표·뷰 카드의 좌표와 크기(x, y, w, h — px, 왼쪽 위 기준), " +
				"그룹의 사각형과 그 안에 든 카드, 메모, 표 사이의 관계(외래키 방향 자식 → 부모), " +
				"그룹 사이를 넘는 관계(groupFlow), 지금 겹쳐 있는 카드를 반환한다. " +
				"flow 는 관계선이 얼마나 읽기 쉬운지의 점수다: throughCards(다른 카드를 관통하는 선), " +
				"crossings(서로 교차하는 선 쌍), totalLength(선 길이 합)는 적을수록 좋다. " +
				"backward(자식이 부모보다 왼쪽)는 참고값이다 — 허브를 가운데 두면 자식이 양쪽에 서므로 " +
				"0 이 목표가 아니다. 재배치하기 전에 먼저 부른다. " +
				"카드 높이는 컬럼 수로 정해지므로 바꿀 수 없다(뷰 높이는 어림값이다). " +
				"표가 많아 한 번에 다 들어가지 않으면 nextOffset 을 알려주므로 offset 을 바꿔 이어서 부른다.",
			Schema: objectSchema(map[string]any{
				"group":  str("이 그룹(ID 또는 이름) 안의 카드만 본다 (생략하면 전체)"),
				"offset": num("몇 번째 표부터 읽을지 (기본 0). 이어 읽을 때 쓴다"),
			}),
			Run: erdReadLayout,
		},
		{
			Name: "arrange_layout",
			Description: "표·뷰 카드와 그룹의 자리를 한 번에 바꾼다. 한 번 부른 것 전체가 한 묶음이라 " +
				"사용자가 Ctrl+Z 한 번으로 통째로 되돌릴 수 있다. 구조(컬럼·키)는 바뀌지 않는다.\n" +
				"표의 x, y 를 직접 계산하지 말고 묶음으로 넘긴다 — 좌표는 서버가 계산한다.\n" +
				"- mode=\"organic\" (도면을 보기 좋게 정리할 때 권장): 도면 전체를 사람이 손으로 그린 것처럼 " +
				"다시 놓는다. 이어진 표끼리 당기고 모든 카드가 서로 밀어내는 힘 배치라, 연결이 많은 표(허브)가 " +
				"무리 가운데에 서고 자식들이 둘레를 감싸며, 두 무리를 잇는 표는 그 사이에 선다. 그룹은 작게 " +
				"뭉치고 그룹 사이는 넓게 비우며, 이어진 카드는 줄을 맞추고, 아무와도 이어지지 않은 표는 도면 " +
				"아래에 모은다. 묶음은 groups[].tables 로만 알려준다(적지 않은 기존 그룹은 지금 든 표를 " +
				"그대로 데려가고, 어느 그룹에도 없는 표는 그룹 밖 카드로 놓인다). tables·views·blocks·" +
				"autoPlace 는 함께 쓰지 않는다. 같은 도면·같은 묶음이면 같은 결과가 나온다.\n" +
				"- mode 생략(manual): 아래 항목으로 원하는 부분만 고친다. 묶음 안 기본 배치는 layout=flow" +
				"(부모가 왼쪽, 자식이 오른쪽인 열 배치)다.\n" +
				"- groups[].tables: 그 표들을 그룹 안에 채우고 그룹 크기를 내용에 맞춘다(layout 기본 flow). " +
				"그룹의 x, y 를 주면 그 자리에서, 생략하면 그룹의 지금 자리에서 채운다.\n" +
				"- blocks[]: 그룹 테두리 없이 표 묶음을 같은 방식으로 채운다(그룹 밖 표용).\n" +
				"- autoPlace=true: 채운 그룹·블록들을 **묶음 사이의 관계 방향**으로 서버가 놓는다" +
				"(부모 묶음이 왼쪽). 이때 그룹·블록의 x, y 는 무시되고 origin(생략하면 그 표들의 지금 " +
				"왼쪽 위)에서 시작한다. 여러 묶음을 한꺼번에 정리할 때 쓴다.\n" +
				"- tables/views: 카드 하나를 지정한 좌표로 옮긴다(x, y 는 카드의 왼쪽 위). 미세 조정용.\n" +
				"- groups[] 에 tables 없이 x, y 만 주면 그룹을 옮기고, 그 안에 통째로 들어 있던 카드·메모도 " +
				"함께 옮긴다(moveContents=false 면 그룹만).\n" +
				"- new=true 면 그룹을 새로 만든다(label 필요, autoPlace 로 채우지 않으면 x, y 도 필요).\n" +
				"결과의 flowBefore·flow 로 선이 나아졌는지(throughCards·crossings·totalLength) 비교하고, " +
				"overlaps·straddlingGroupEdge·edgesThroughCards 가 남아 있으면 묶음을 고쳐 다시 부른다.",
			Schema: objectSchema(map[string]any{
				"groups": map[string]any{
					"type":        "array",
					"description": "채우거나 옮길 그룹 목록",
					"items": packFields(map[string]any{
						"group": str("그룹 ID 또는 이름 (new=true 면 생략)"),
						"new":   boolp("true 면 그룹을 새로 만든다"),
						"label": str("그룹 이름 (새로 만들 때 필수, 기존 그룹이면 이름 바꾸기)"),
						"x":     num("그룹 왼쪽 위 x (px)"),
						"y":     num("그룹 왼쪽 위 y (px)"),
						"w":     num("폭 (선택). tables 를 주면 내용에 맞춘 값보다 작아지지 않는다"),
						"h":     num("높이 (선택). tables 를 주면 내용에 맞춘 값보다 작아지지 않는다"),
						"moveContents": boolp(
							"tables 없이 그룹을 옮길 때 안에 든 카드·메모도 함께 옮긴다 (기본 true)"),
					}),
				},
				"blocks": map[string]any{
					"type":        "array",
					"description": "그룹 테두리 없이 채울 표 묶음 (autoPlace 가 아니면 x, y 필요)",
					"items": packFields(map[string]any{
						"x": num("묶음 왼쪽 위 x (px)"),
						"y": num("묶음 왼쪽 위 y (px)"),
					}),
				},
				"mode": str("organic: 도면 전체를 손으로 그린 것처럼 힘 배치로 다시 놓는다(권장). " +
					"생략하면 manual(아래 항목대로)"),
				"autoPlace": boolp("true 면 groups[].tables·blocks 로 채운 묶음들을 관계 방향으로 서버가 놓는다"),
				"origin": objectSchema(map[string]any{
					"x": num("autoPlace 의 시작 x"),
					"y": num("autoPlace 의 시작 y"),
				}),
				"tables": map[string]any{
					"type":        "array",
					"description": "좌표를 직접 정할 표 목록 (묶음에 넣은 표는 여기 적지 않는다)",
					"items": objectSchema(map[string]any{
						"table": str("테이블 이름"),
						"x":     num("왼쪽 위 x (px)"),
						"y":     num("왼쪽 위 y (px)"),
						"width": num("카드 폭 (선택, 180~720). 생략하면 지금 폭 그대로"),
					}, "table", "x", "y"),
				},
				"views": map[string]any{
					"type":        "array",
					"description": "좌표를 직접 정할 뷰 목록. 뷰는 표를 읽는 것이라 표들의 오른쪽 끝에 둔다",
					"items": objectSchema(map[string]any{
						"view": str("뷰 이름"),
						"x":    num("왼쪽 위 x (px)"),
						"y":    num("왼쪽 위 y (px)"),
					}, "view", "x", "y"),
				},
				"gap": num("묶음 안 카드 사이 간격 px (기본 40, 16~200). flow 의 열 사이는 이것의 두 배"),
			}),
			Run: erdArrangeLayout,
		},
	}
}

// ---------- 읽기 ----------

type layoutCardOut struct {
	Name      string  `json:"name"`
	Logical   string  `json:"logical,omitempty"`
	X         float64 `json:"x"`
	Y         float64 `json:"y"`
	W         float64 `json:"w"`
	H         float64 `json:"h"`
	Collapsed bool    `json:"collapsed,omitempty"`
	Group     string  `json:"group,omitempty"`
	// Unplaced면 문서에 좌표가 없어 화면이 기본 자리(80, 80)에 그리고 있다.
	Unplaced bool `json:"unplaced,omitempty"`
}

type layoutGroupOut struct {
	ID    string   `json:"id"`
	Label string   `json:"label,omitempty"`
	X     float64  `json:"x"`
	Y     float64  `json:"y"`
	W     float64  `json:"w"`
	H     float64  `json:"h"`
	Cards []string `json:"cards,omitempty"`
}

// layoutScene은 문서의 배치를 툴이 다루기 좋은 모양으로 펼친 것이다.
type layoutScene struct {
	doc    *erd.Document
	rects  map[string]erd.Rect
	names  map[string]string // 카드 키 → 보여줄 이름
	tables []*schema.Table
	views  []*schema.View
}

func newLayoutScene(doc *erd.Document) *layoutScene {
	sc := &layoutScene{doc: doc, rects: doc.CardRects(), names: map[string]string{}}
	for _, t := range doc.Schema.Tables {
		sc.names[t.Key()] = qualified(t.Namespace, t.Name)
		sc.tables = append(sc.tables, t)
		if _, ok := sc.rects[t.Key()]; !ok {
			w, h := doc.CardSize(t.Key())
			sc.rects[t.Key()] = erd.Rect{X: 80, Y: 80, W: w, H: h}
		}
	}
	for _, v := range doc.Schema.Views {
		sc.names[v.Key()] = qualified(v.Namespace, v.Name)
		sc.views = append(sc.views, v)
		if _, ok := sc.rects[v.Key()]; !ok {
			w, h := doc.CardSize(v.Key())
			sc.rects[v.Key()] = erd.Rect{X: 80, Y: 80, W: w, H: h}
		}
	}
	return sc
}

func qualified(ns, name string) string {
	if ns == "" {
		return name
	}
	return ns + "." + name
}

// cardKeys는 표·뷰 카드의 키를 문서 순서로 돌려준다(같은 문서에서 같은 결과가 나와야 한다).
func (sc *layoutScene) cardKeys() []string {
	out := make([]string, 0, len(sc.tables)+len(sc.views))
	for _, t := range sc.tables {
		out = append(out, t.Key())
	}
	for _, v := range sc.views {
		out = append(out, v.Key())
	}
	return out
}

// groupOf는 카드를 통째로 담고 있는 그룹 가운데 가장 작은 것이다.
//
// 소속을 데이터로 두지 않으므로(erd.Group 주석) 화면에서 보이는 그대로 판정한다:
// 안에 들어 있으면 그 그룹의 것이다. 그룹이 겹쳐 있으면 안쪽(작은 쪽)이 더 구체적이다.
func (sc *layoutScene) groupOf(r erd.Rect) *erd.Group {
	var best *erd.Group
	for _, g := range sc.doc.Groups {
		gr := g.Rect()
		if !gr.Contains(r) {
			continue
		}
		if best == nil || gr.W*gr.H < best.Rect().W*best.Rect().H {
			best = g
		}
	}
	return best
}

// relations는 표 사이의 관계(자식 → 부모)를 한 쌍에 한 줄로 만든다.
func (sc *layoutScene) relations(edges []flowEdge, only map[string]bool) []string {
	out := []string{}
	for _, e := range edges {
		if only != nil && !only[e.child] && !only[e.parent] {
			continue
		}
		out = append(out, sc.names[e.child]+" → "+sc.names[e.parent])
	}
	return out
}

// issues는 겹친 카드 쌍과 그룹 경계에 걸친 카드를 찾는다.
//
// 걸친 카드를 따로 세는 이유: 겹침은 아니지만 도면에서 가장 어지러워 보이는 모양이고,
// 그 카드가 어느 그룹 것인지 사람도 알 수 없게 된다.
func (sc *layoutScene) issues() (overlaps, straddles []string) {
	keys := sc.cardKeys()
	for i := 0; i < len(keys); i++ {
		a := sc.rects[keys[i]]
		for j := i + 1; j < len(keys); j++ {
			if a.Overlaps(sc.rects[keys[j]]) {
				overlaps = append(overlaps, sc.names[keys[i]]+" ↔ "+sc.names[keys[j]])
			}
		}
		for _, g := range sc.doc.Groups {
			gr := g.Rect()
			if gr.Overlaps(a) && !gr.Contains(a) {
				straddles = append(straddles, fmt.Sprintf("%s (그룹 %s)", sc.names[keys[i]], groupName(g)))
			}
		}
	}
	return overlaps, straddles
}

func groupName(g *erd.Group) string {
	if g.Label != "" {
		return g.Label
	}
	return g.ID
}

// findGroup은 ID 또는 이름으로 그룹을 찾는다. 이름이 겹치면 ID 로 고르게 한다.
func findGroup(doc *erd.Document, want string) (*erd.Group, error) {
	want = strings.TrimSpace(want)
	if want == "" {
		return nil, fmt.Errorf("group 이 비어 있습니다. 그룹 ID 또는 이름을 적어주세요")
	}
	if g := doc.Group(want); g != nil {
		return g, nil
	}
	var found *erd.Group
	n := 0
	for _, g := range doc.Groups {
		if strings.EqualFold(strings.TrimSpace(g.Label), want) {
			found = g
			n++
		}
	}
	switch {
	case n == 0:
		return nil, fmt.Errorf("그룹 %q 을(를) 찾을 수 없습니다. read_layout 으로 그룹 목록을 확인하세요", want)
	case n > 1:
		return nil, fmt.Errorf("이름이 %q 인 그룹이 %d개입니다. ID 로 지정하세요 — read_layout 이 ID 를 함께 돌려줍니다", want, n)
	}
	return found, nil
}

func erdReadLayout(ec *erdToolContext, args json.RawMessage) (string, error) {
	var in struct {
		Group  string `json:"group"`
		Offset int    `json:"offset"`
	}
	if err := parseArgs(args, &in); err != nil {
		return "", err
	}
	doc, err := ec.document()
	if err != nil {
		return "", err
	}
	sc := newLayoutScene(doc)

	var only *erd.Group
	if strings.TrimSpace(in.Group) != "" {
		if only, err = findGroup(doc, in.Group); err != nil {
			return "", err
		}
	}

	groups := make([]layoutGroupOut, 0, len(doc.Groups))
	for _, g := range doc.Groups {
		if only != nil && g != only {
			continue
		}
		r := g.Rect()
		groups = append(groups, layoutGroupOut{ID: g.ID, Label: g.Label, X: r.X, Y: r.Y, W: r.W, H: r.H})
	}
	groupIdx := map[*erd.Group]int{}
	for i := range groups {
		groupIdx[doc.Group(groups[i].ID)] = i
	}

	// 카드는 들어가는 만큼 담는다(read_schema 와 같은 규칙). 그룹·관계는 작아서 매번 싣는다.
	all := sc.cardKeys()
	start := min(max(in.Offset, 0), len(all))
	cards := []layoutCardOut{}
	shown := map[string]bool{}
	size, next := 0, 0
	for i := start; i < len(all); i++ {
		key := all[i]
		r := sc.rects[key]
		g := sc.groupOf(r)
		if only != nil && g != only {
			continue
		}
		box := doc.Layout[key]
		out := layoutCardOut{Name: sc.names[key], X: r.X, Y: r.Y, W: r.W, H: r.H, Unplaced: box == nil}
		if box != nil {
			out.Logical, out.Collapsed = box.Logical, box.Collapsed
		}
		if g != nil {
			out.Group = groupName(g)
			if idx, ok := groupIdx[g]; ok {
				groups[idx].Cards = append(groups[idx].Cards, out.Name)
			}
		}
		n := len(out.Name) + len(out.Logical) + len(out.Group) + 90
		if len(cards) > 0 && size+n > erdLayoutBudget {
			next = i
			break
		}
		cards = append(cards, out)
		shown[key] = true
		size += n
	}

	notes := []map[string]any{}
	for _, n := range doc.Notes {
		r := n.Rect()
		if only != nil && !only.Rect().Contains(r) {
			continue
		}
		text := []rune(strings.TrimSpace(n.Text))
		if len(text) > 30 {
			text = append(text[:30], '…')
		}
		notes = append(notes, map[string]any{"id": n.ID, "text": string(text), "x": r.X, "y": r.Y, "w": r.W, "h": r.H})
	}

	overlaps, straddles := sc.issues()
	edges := flowEdges(sc)
	res := map[string]any{
		"document":  doc.Name,
		"cardCount": len(all),
		"offset":    start,
		"cards":     cards,
		"groups":    groups,
		"relations": sc.relations(edges, shown),
		// 흐름 점수는 언제나 **도면 전체**의 것이다. 나눠 읽는 중이어도 고칠 대상은
		// 도면 하나이고, 페이지마다 다른 숫자를 주면 비교할 수가 없다.
		"flow": flowMetrics(sc, sc.rects, edges, true),
	}
	if gf := groupFlow(sc, edges); len(gf) > 0 {
		res["groupFlow"] = gf
	}
	if len(notes) > 0 {
		res["notes"] = notes
	}
	if len(overlaps) > 0 {
		res["overlaps"] = capList(overlaps)
	}
	if len(straddles) > 0 {
		res["straddlingGroupEdge"] = capList(straddles)
	}
	if next > 0 {
		res["nextOffset"] = next
		res["note"] = fmt.Sprintf("카드 %d개 중 일부만 담았습니다. 나머지는 offset=%d 으로 이어서 부르세요.", len(all), next)
	}
	return asJSON(res)
}

func capList(list []string) []string {
	if len(list) <= arrangeIssueMax {
		return list
	}
	out := append([]string{}, list[:arrangeIssueMax]...)
	return append(out, fmt.Sprintf("… 외 %d건", len(list)-arrangeIssueMax))
}

// ---------- 재배치 ----------

type arrangeIn struct {
	Tables []struct {
		Table string   `json:"table"`
		X     *float64 `json:"x"`
		Y     *float64 `json:"y"`
		Width *float64 `json:"width"`
	} `json:"tables"`
	Views []struct {
		View string   `json:"view"`
		X    *float64 `json:"x"`
		Y    *float64 `json:"y"`
	} `json:"views"`
	Groups []struct {
		Group        string   `json:"group"`
		New          bool     `json:"new"`
		Label        *string  `json:"label"`
		X            *float64 `json:"x"`
		Y            *float64 `json:"y"`
		W            *float64 `json:"w"`
		H            *float64 `json:"h"`
		Tables       []string `json:"tables"`
		Columns      int      `json:"columns"`
		Layout       string   `json:"layout"`
		MoveContents *bool    `json:"moveContents"`
	} `json:"groups"`
	Blocks []struct {
		X       *float64 `json:"x"`
		Y       *float64 `json:"y"`
		Tables  []string `json:"tables"`
		Layout  string   `json:"layout"`
		Columns int      `json:"columns"`
	} `json:"blocks"`
	// Mode가 organic 이면 도면 전체를 힘 배치로 다시 놓는다(ai_tools_erdorganic.go).
	Mode      string `json:"mode"`
	AutoPlace bool   `json:"autoPlace"`
	Origin    *struct {
		X *float64 `json:"x"`
		Y *float64 `json:"y"`
	} `json:"origin"`
	Gap *float64 `json:"gap"`
}

// groupPlan은 그룹 하나에 보낼 op 다.
type groupPlan struct {
	add     bool
	payload map[string]any
	name    string
	// 그룹만 옮길 때 함께 옮길 내용물(옛 자리 기준)과 이동량.
	follow bool
	old    erd.Rect
	dx, dy float64
}

// packUnit은 한 덩어리로 채워 놓는 묶음이다(표를 채우는 그룹, 또는 테두리 없는 블록).
type packUnit struct {
	name string
	// plan이 있으면 그룹이다. 자리가 정해지면 그룹의 좌표·크기도 함께 정해진다.
	plan *groupPlan
	keys []string
	// rel은 묶음 왼쪽 위 기준의 카드 자리다(그룹이면 안쪽 여백까지 들어 있다).
	rel  map[string]flowPoint
	w, h float64
	x, y float64
}

// cardMove는 카드 하나를 어디로 옮길지다.
type cardMove struct {
	key   string
	view  bool
	x, y  float64
	width *float64
	// by는 이 자리를 누가 정했는지다(겹치는 지시를 짚어 거절할 때 쓴다).
	by string
}

func checkCoord(what string, v *float64) (float64, error) {
	if v == nil {
		return 0, fmt.Errorf("%s 의 좌표가 없습니다", what)
	}
	if math.IsNaN(*v) || math.Abs(*v) > arrangeCoordAbs {
		return 0, fmt.Errorf("%s 의 좌표 %.0f 가 너무 멉니다 (±%.0f 안이어야 합니다)", what, *v, arrangeCoordAbs)
	}
	return math.Round(*v), nil
}

func erdArrangeLayout(ec *erdToolContext, args json.RawMessage) (string, error) {
	var in arrangeIn
	if err := parseArgs(args, &in); err != nil {
		return "", err
	}
	organic := strings.EqualFold(strings.TrimSpace(in.Mode), "organic")
	if len(in.Tables)+len(in.Views)+len(in.Groups)+len(in.Blocks) == 0 && !organic {
		return "", fmt.Errorf("옮길 것이 없습니다. tables·views·groups·blocks 중 하나 이상을 적어주세요")
	}
	gap := arrangeGapDef
	if in.Gap != nil {
		gap = min(max(*in.Gap, arrangeGapMin), arrangeGapMax)
	}

	doc, err := ec.document()
	if err != nil {
		return "", err
	}
	sc := newLayoutScene(doc)

	// 먼저 전부 확인하고, 문서에는 마지막에 한꺼번에 보낸다. 중간에 하나가 틀려
	// 절반만 옮겨진 도면은 처음보다 나쁘다.
	moves := map[string]*cardMove{}
	claim := func(m *cardMove) error {
		if prev, ok := moves[m.key]; ok {
			return fmt.Errorf("%s 의 자리를 두 곳에서 정했습니다(%s, %s). 한 곳만 남기세요",
				sc.names[m.key], prev.by, m.by)
		}
		moves[m.key] = m
		return nil
	}

	for _, t := range in.Tables {
		tbl := findERDTable(doc, t.Table)
		if tbl == nil {
			return "", fmt.Errorf("테이블 %q을(를) 찾을 수 없습니다", t.Table)
		}
		x, err := checkCoord(tbl.Name, t.X)
		if err != nil {
			return "", err
		}
		y, err := checkCoord(tbl.Name, t.Y)
		if err != nil {
			return "", err
		}
		if err := claim(&cardMove{key: tbl.Key(), x: x, y: y, width: t.Width, by: "tables"}); err != nil {
			return "", err
		}
	}
	for _, v := range in.Views {
		key := ""
		for _, sv := range doc.Schema.Views {
			if strings.EqualFold(sv.Name, strings.TrimSpace(v.View)) || sv.Key() == strings.ToLower(strings.TrimSpace(v.View)) {
				key = sv.Key()
				break
			}
		}
		if key == "" {
			return "", fmt.Errorf("뷰 %q을(를) 찾을 수 없습니다", v.View)
		}
		x, err := checkCoord(v.View, v.X)
		if err != nil {
			return "", err
		}
		y, err := checkCoord(v.View, v.Y)
		if err != nil {
			return "", err
		}
		if err := claim(&cardMove{key: key, view: true, x: x, y: y, by: "views"}); err != nil {
			return "", err
		}
	}

	plans := []*groupPlan{}
	touched := map[string]bool{}
	newGroups := 0
	edges := flowEdges(sc)
	flowBefore := flowMetrics(sc, sc.rects, edges, false)

	switch strings.ToLower(strings.TrimSpace(in.Mode)) {
	case "", "manual":
	case "organic":
		plans, moves, notes, err := planOrganic(doc, sc, &in, edges, gap)
		if err != nil {
			return "", err
		}
		return ec.commitArrangement(sc, flowBefore, plans, moves, notes)
	default:
		return "", fmt.Errorf("mode 는 organic 또는 manual 입니다 (%q)", in.Mode)
	}

	// 채울 묶음(표가 든 그룹, 블록)은 먼저 **자기 왼쪽 위 기준으로** 채워 크기를 안다.
	// 자리는 그다음에 정한다 — autoPlace 는 묶음 크기를 알아야 묶음끼리를 놓을 수 있다.
	units := []*packUnit{}
	pack := func(where string, names []string, layout string, columns int, padX, padTop, padBot float64) (*packUnit, error) {
		u := &packUnit{name: where, rel: map[string]flowPoint{}}
		items := make([]packItem, 0, len(names))
		seen := map[string]bool{}
		for _, name := range names {
			tbl := findERDTable(doc, name)
			if tbl == nil {
				return nil, fmt.Errorf("%s: 테이블 %q을(를) 찾을 수 없습니다", where, name)
			}
			if seen[tbl.Key()] {
				return nil, fmt.Errorf("%s: %s 을(를) 두 번 적었습니다", where, tbl.Name)
			}
			seen[tbl.Key()] = true
			r := sc.rects[tbl.Key()]
			items = append(items, packItem{id: tbl.Key(), w: r.W, h: r.H})
		}
		var rel map[string]flowPoint
		var cw, ch float64
		switch strings.ToLower(strings.TrimSpace(layout)) {
		case "", "flow":
			// 열 사이는 줄 사이보다 넓게 둔다. 선이 지나가고 까마귀발이 놓일 자리다.
			rel, cw, ch = packFlow(items, edges, max(2*gap, 80), gap, arrangeFlowRows)
		case "grid":
			rel, cw, ch = packGrid(items, columns, gap)
		default:
			return nil, fmt.Errorf("%s: layout 은 flow 또는 grid 입니다 (%q)", where, layout)
		}
		for _, it := range items {
			p := rel[it.id]
			u.rel[it.id] = flowPoint{x: p.x + padX, y: p.y + padTop}
			u.keys = append(u.keys, it.id)
		}
		u.w, u.h = cw+2*padX, ch+padTop+padBot
		return u, nil
	}

	for i, gi := range in.Groups {
		p := &groupPlan{payload: map[string]any{}}
		var cur *erd.Group
		if gi.New {
			label := ""
			if gi.Label != nil {
				label = strings.TrimSpace(*gi.Label)
			}
			if label == "" {
				return "", fmt.Errorf("groups[%d]: 새 그룹에는 label 이 필요합니다", i)
			}
			if (gi.X == nil || gi.Y == nil) && (!in.AutoPlace || len(gi.Tables) == 0) {
				return "", fmt.Errorf("groups[%d]: 새 그룹 %q 에는 x, y 가 필요합니다"+
					"(autoPlace 로 표를 채울 때는 빼도 됩니다)", i, label)
			}
			newGroups++
			if len(doc.Groups)+newGroups > erd.MaxGroups {
				return "", fmt.Errorf("그룹은 %d개까지 만들 수 있습니다", erd.MaxGroups)
			}
			p.add = true
			p.name = label
			p.payload["id"] = uuid.NewString()
			cur = &erd.Group{Label: label, W: 320, H: 240}
		} else {
			g, err := findGroup(doc, gi.Group)
			if err != nil {
				return "", fmt.Errorf("groups[%d]: %w", i, err)
			}
			if touched[g.ID] {
				return "", fmt.Errorf("그룹 %s 을(를) 두 번 적었습니다. 한 번만 적으세요", groupName(g))
			}
			touched[g.ID] = true
			cur = g
			p.name = groupName(g)
			p.payload["id"] = g.ID
		}
		if gi.Label != nil {
			p.payload["label"] = strings.TrimSpace(*gi.Label)
		}

		gx, gy := cur.X, cur.Y
		if gi.X != nil {
			if gx, err = checkCoord("그룹 "+p.name, gi.X); err != nil {
				return "", err
			}
		}
		if gi.Y != nil {
			if gy, err = checkCoord("그룹 "+p.name, gi.Y); err != nil {
				return "", err
			}
		}
		p.payload["x"], p.payload["y"] = gx, gy
		cr := cur.Rect()
		w, h := cr.W, cr.H
		if gi.W != nil {
			w = *gi.W
		}
		if gi.H != nil {
			h = *gi.H
		}

		switch {
		case len(gi.Tables) > 0:
			u, err := pack("그룹 "+p.name, gi.Tables, gi.Layout, gi.Columns, arrangePadX, arrangePadTop, arrangePadBot)
			if err != nil {
				return "", err
			}
			// 크기를 직접 준 경우에도 내용보다 작아지지는 않는다 — 작아지면 방금 채운
			// 카드가 그룹 경계에 걸친다.
			if gi.W != nil {
				u.w = max(u.w, w)
			}
			if gi.H != nil {
				u.h = max(u.h, h)
			}
			u.plan = p
			u.x, u.y = gx, gy
			units = append(units, u)
		case !p.add && (gx != cur.X || gy != cur.Y) && (gi.MoveContents == nil || *gi.MoveContents):
			p.follow = true
			p.old = cur.Rect()
			p.dx, p.dy = gx-cur.X, gy-cur.Y
		}
		if gi.W != nil || gi.H != nil || p.add {
			p.payload["w"], p.payload["h"] = math.Round(w), math.Round(h)
		}
		plans = append(plans, p)
	}

	for i, bi := range in.Blocks {
		if len(bi.Tables) == 0 {
			return "", fmt.Errorf("blocks[%d]: tables 가 비어 있습니다", i)
		}
		u, err := pack(fmt.Sprintf("blocks[%d]", i), bi.Tables, bi.Layout, bi.Columns, 0, 0, 0)
		if err != nil {
			return "", err
		}
		if !in.AutoPlace {
			if u.x, err = checkCoord(u.name, bi.X); err != nil {
				return "", err
			}
			if u.y, err = checkCoord(u.name, bi.Y); err != nil {
				return "", err
			}
		}
		units = append(units, u)
	}

	// 묶음끼리의 자리. autoPlace 면 묶음 사이의 관계 방향으로 열을 잡는다 — 묶음 안과
	// 같은 규칙(부모 묶음이 왼쪽)이라, 묶음을 넘는 선도 같은 방향으로 흐른다.
	if in.AutoPlace && len(units) > 0 {
		// 어느 묶음에도 적지 않은 그룹 밖 표도 한 장짜리 묶음으로 함께 놓는다. 빼 두면
		// 그 표는 제자리에 남고, 새로 놓인 묶음이 그 위를 덮는다 — 실제로 그렇게 표가
		// 그룹 안에 갇혀 보이는 도면이 나왔다. 기존 그룹 안의 표는 그 그룹의 것이라 두고,
		// 직접 좌표를 준 표도 그 자리가 이긴다.
		inUnit := map[string]bool{}
		for _, u := range units {
			for _, k := range u.keys {
				inUnit[k] = true
			}
		}
		for _, t := range sc.tables {
			k := t.Key()
			if inUnit[k] || moves[k] != nil || sc.groupOf(sc.rects[k]) != nil {
				continue
			}
			r := sc.rects[k]
			units = append(units, &packUnit{name: t.Name, keys: []string{k},
				rel: map[string]flowPoint{k: {}}, w: r.W, h: r.H})
		}
		if err := autoPlaceUnits(sc, units, edges, in.Origin); err != nil {
			return "", err
		}
	}

	for _, u := range units {
		if u.plan != nil {
			u.plan.payload["x"], u.plan.payload["y"] = u.x, u.y
			u.plan.payload["w"], u.plan.payload["h"] = math.Round(u.w), math.Round(u.h)
		}
		for _, k := range u.keys {
			r := u.rel[k]
			if err := claim(&cardMove{key: k, x: math.Round(u.x + r.x), y: math.Round(u.y + r.y), by: u.name}); err != nil {
				return "", err
			}
		}
	}

	// 그룹을 따라 움직이는 내용물. 직접 자리를 정한 카드는 그 자리가 이긴다 —
	// "그룹을 옮기되 이 표만은 여기에" 가 자연스러운 요청이기 때문이다.
	noteMoves := []noteMove{}
	movedNotes := map[string]bool{}
	views := viewKeySet(doc)
	for _, p := range plans {
		if !p.follow {
			continue
		}
		for _, key := range sc.cardKeys() {
			if _, ok := moves[key]; ok {
				continue
			}
			r := sc.rects[key]
			if doc.Layout[key] == nil || !p.old.Contains(r) {
				continue
			}
			_, isView := views[key]
			moves[key] = &cardMove{key: key, view: isView, x: r.X + p.dx, y: r.Y + p.dy, by: "그룹 " + p.name}
		}
		for _, n := range doc.Notes {
			if movedNotes[n.ID] || !p.old.Contains(n.Rect()) {
				continue
			}
			movedNotes[n.ID] = true
			noteMoves = append(noteMoves, noteMove{id: n.ID, x: n.X + p.dx, y: n.Y + p.dy})
		}
	}

	return ec.commitArrangement(sc, flowBefore, plans, moves, noteMoves)
}

// noteMove는 메모 하나를 어디로 옮길지다(그룹을 따라 움직이는 메모).
type noteMove struct {
	id   string
	x, y float64
}

// commitArrangement는 확정된 배치를 한 묶음으로 보내고, 옮긴 뒤의 도면을 다시 읽어
// 결과를 만든다. 손으로 정한 배치와 힘 배치(organic)가 같은 길로 나간다 — 묶음 이름,
// 순서, 결과 모양이 갈리면 한쪽만 되돌리기가 깨진다.
func (ec *erdToolContext) commitArrangement(sc *layoutScene, flowBefore flowStats, plans []*groupPlan,
	moves map[string]*cardMove, noteMoves []noteMove) (string, error) {
	if total := len(plans) + len(moves) + len(noteMoves); total > arrangeMaxOps {
		return "", fmt.Errorf("한 번에 옮길 수 있는 것은 %d개까지입니다(요청 %d개). 나눠서 부르세요", arrangeMaxOps, total)
	}

	// 보내기. 그룹이 먼저다 — 카드를 먼저 옮기면 그 사이에 열어 둔 화면에서 카드가
	// 옛 그룹 위를 지나가는 모습이 잠깐 보인다.
	batch := uuid.NewString()
	for _, p := range plans {
		kind := erd.OpGroupUpdate
		if p.add {
			kind = erd.OpGroupAdd
		}
		if _, err := ec.submitBatch(kind, p.payload, batch); err != nil {
			return "", fmt.Errorf("그룹 %s 을(를) 옮기지 못했습니다: %w", p.name, err)
		}
	}
	keys := make([]string, 0, len(moves))
	for k := range moves {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		m := moves[k]
		payload := map[string]any{"key": m.key, "x": math.Round(m.x), "y": math.Round(m.y)}
		kind := erd.OpTableMove
		if m.view {
			kind = erd.OpViewMove
		}
		if m.width != nil {
			payload["width"] = *m.width
		}
		if _, err := ec.submitBatch(kind, payload, batch); err != nil {
			return "", fmt.Errorf("%s 을(를) 옮기지 못했습니다: %w", sc.names[k], err)
		}
	}
	for _, n := range noteMoves {
		payload := map[string]any{"id": n.id, "x": math.Round(n.x), "y": math.Round(n.y)}
		if _, err := ec.submitBatch(erd.OpNoteUpdate, payload, batch); err != nil {
			return "", fmt.Errorf("메모를 옮기지 못했습니다: %w", err)
		}
	}

	// 옮긴 뒤의 도면을 다시 읽어 남은 문제를 알려준다. 모델이 자기 계산을 믿고
	// "깔끔해졌습니다"라고 말하기 전에 실제 결과를 보게 하는 것이 요점이다.
	after, err := ec.document()
	if err != nil {
		return "", err
	}
	asc := newLayoutScene(after)
	overlaps, straddles := asc.issues()
	flowAfter := flowMetrics(asc, asc.rects, flowEdges(asc), true)
	groups := []layoutGroupOut{}
	for _, p := range plans {
		g := after.Group(p.payload["id"].(string))
		if g == nil {
			continue
		}
		r := g.Rect()
		groups = append(groups, layoutGroupOut{ID: g.ID, Label: g.Label, X: r.X, Y: r.Y, W: r.W, H: r.H})
	}
	res := map[string]any{
		"ok":         true,
		"movedCards": len(moves),
		"movedNotes": len(noteMoves),
		"groups":     groups,
		// 전후를 함께 준다. 숫자가 나빠졌으면 모델이 그 사실을 보고 고치거나 알려야 한다 —
		// "정리했습니다"는 이 숫자가 좋아졌을 때만 할 수 있는 말이다.
		"flowBefore": flowBefore,
		"flow":       flowAfter,
		"note":       "한 묶음으로 반영했습니다. 사용자가 도면에서 Ctrl+Z 한 번으로 통째로 되돌릴 수 있습니다.",
	}
	if len(overlaps) > 0 {
		res["overlaps"] = capList(overlaps)
	}
	if len(straddles) > 0 {
		res["straddlingGroupEdge"] = capList(straddles)
	}
	return asJSON(res)
}

func viewKeySet(doc *erd.Document) map[string]struct{} {
	out := make(map[string]struct{}, len(doc.Schema.Views))
	for _, v := range doc.Schema.Views {
		out[v.Key()] = struct{}{}
	}
	return out
}
