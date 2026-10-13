package api

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"dbstudio/internal/erd"
)

// 관계선의 흐름.
//
// 도면이 읽히는지는 카드가 겹치지 않는 것만으로 정해지지 않는다. 관계선이 한 방향으로
// 흐르는지가 더 크다 — 회원 → 주문 → 주문상세가 왼쪽에서 오른쪽으로 늘어서면 선을
// 따라 읽으면 되지만, 선이 오른쪽으로 갔다 왼쪽으로 되돌아오고 남의 카드를 가로지르면
// 사람은 선을 하나씩 손가락으로 짚어야 한다.
//
// 여기 있는 두 가지가 그 기준을 서버에 둔다.
//   - flowMetrics: 지금 배치에서 선이 얼마나 자연스러운지 센다. 모델이 "정리했다"고
//     말하기 전에 숫자로 확인하게 하는 것이 목적이다.
//   - packFlow: 카드 묶음을 흐름 순서로 놓는다(계층 배치의 가벼운 판).
//
// 방향은 **부모(가리켜지는 표)가 왼쪽, 자식이 오른쪽**이다. 서버의 첫 배치(erd.AutoLayout)와
// 같고, 화면(erdroute.js)은 두 카드 중심의 가로 차이가 세로 차이보다 크면 선을 오른쪽 변
// → 왼쪽 변으로 붙인다 — 가로로 늘어선 계층이 곧은 선이 된다.

// flowEdge는 관계선 하나다(자식 → 부모). 같은 쌍의 외래키 여러 개는 한 줄이다 —
// 화면도 같은 자리에 겹쳐 그리므로 흐름으로는 하나다.
type flowEdge struct {
	child, parent string
}

// flowEdges는 문서의 관계선을 모은다. 자기 참조와 문서 밖을 가리키는 것은 뺀다.
func flowEdges(sc *layoutScene) []flowEdge {
	seen := map[flowEdge]bool{}
	out := []flowEdge{}
	for _, t := range sc.tables {
		for _, fk := range t.ForeignKeys {
			ns := fk.RefNamespace
			if ns == "" {
				ns = t.Namespace
			}
			to := strings.ToLower(qualified(ns, fk.RefTable))
			if to == t.Key() {
				continue
			}
			if _, ok := sc.names[to]; !ok {
				continue
			}
			e := flowEdge{child: t.Key(), parent: to}
			if seen[e] {
				continue
			}
			seen[e] = true
			out = append(out, e)
		}
	}
	return out
}

type flowPoint struct{ x, y float64 }

// attach는 화면이 선을 붙이는 두 점이다(erdroute.js 의 naturalSides 와 같은 규칙).
func attach(a, b erd.Rect) (flowPoint, flowPoint) {
	ac := flowPoint{a.X + a.W/2, a.Y + a.H/2}
	bc := flowPoint{b.X + b.W/2, b.Y + b.H/2}
	if math.Abs(bc.x-ac.x) > math.Abs(bc.y-ac.y) {
		if bc.x > ac.x {
			return flowPoint{a.X + a.W, ac.y}, flowPoint{b.X, bc.y}
		}
		return flowPoint{a.X, ac.y}, flowPoint{b.X + b.W, bc.y}
	}
	if bc.y > ac.y {
		return flowPoint{ac.x, a.Y + a.H}, flowPoint{bc.x, b.Y}
	}
	return flowPoint{ac.x, a.Y}, flowPoint{bc.x, b.Y + b.H}
}

// flowStats는 배치 하나의 흐름 점수다. 모두 적을수록 좋다.
type flowStats struct {
	Edges int `json:"edges"`
	// Backward는 자식이 부모보다 왼쪽에 있는 선이다(흐름을 거스른다).
	Backward int `json:"backward"`
	// ThroughCards는 곧게 그으면 다른 카드를 지나는 선이다. 화면은 그 선을 돌려
	// 그리거나 카드 위로 덮어 그린다 — 어느 쪽이든 읽기 어렵다.
	ThroughCards int `json:"throughCards"`
	// Crossings는 서로 교차하는 선의 쌍이다.
	Crossings int `json:"crossings"`
	// TotalLength는 선 길이의 합(px)이다. 같은 점수끼리는 짧은 쪽이 낫다.
	TotalLength int `json:"totalLength"`

	BackwardList []string `json:"backwardEdges,omitempty"`
	ThroughList  []string `json:"edgesThroughCards,omitempty"`
	LongestList  []string `json:"longestEdges,omitempty"`
}

// flowMetrics는 배치의 흐름을 센다. detail 이면 어느 선인지 목록도 싣는다.
func flowMetrics(sc *layoutScene, rects map[string]erd.Rect, edges []flowEdge, detail bool) flowStats {
	st := flowStats{Edges: len(edges)}
	type seg struct {
		a, b flowPoint
		e    flowEdge
		l    float64
	}
	segs := make([]seg, 0, len(edges))
	for _, e := range edges {
		cr, pr := rects[e.child], rects[e.parent]
		a, b := attach(cr, pr)
		l := math.Hypot(b.x-a.x, b.y-a.y)
		segs = append(segs, seg{a: a, b: b, e: e, l: l})
		st.TotalLength += int(l)
		label := sc.names[e.child] + " → " + sc.names[e.parent]

		// 같은 열에 위아래로 놓인 쌍은 거스르는 것으로 치지 않는다(카드 폭의 절반 이내).
		if cr.X+cr.W/2 < pr.X+pr.W/2-min(cr.W, pr.W)/2 {
			st.Backward++
			if detail {
				st.BackwardList = append(st.BackwardList, label)
			}
		}
		for key, r := range rects {
			if key == e.child || key == e.parent {
				continue
			}
			// 가장자리를 스치는 것은 지나는 것으로 보지 않는다(화면도 틈을 두고 돌린다).
			in := erd.Rect{X: r.X + 4, Y: r.Y + 4, W: r.W - 8, H: r.H - 8}
			if segmentHitsRect(a, b, in) {
				st.ThroughCards++
				if detail {
					st.ThroughList = append(st.ThroughList, fmt.Sprintf("%s (%s 을 지남)", label, sc.names[key]))
				}
				break
			}
		}
	}
	for i := 0; i < len(segs); i++ {
		for j := i + 1; j < len(segs); j++ {
			x, y := segs[i].e, segs[j].e
			// 끝점을 나누는 선은 그 카드에서 만나는 것이지 교차가 아니다.
			if x.child == y.child || x.child == y.parent || x.parent == y.child || x.parent == y.parent {
				continue
			}
			if segmentsCross(segs[i].a, segs[i].b, segs[j].a, segs[j].b) {
				st.Crossings++
			}
		}
	}
	if detail {
		sort.SliceStable(segs, func(i, j int) bool { return segs[i].l > segs[j].l })
		for i := 0; i < len(segs) && i < 5; i++ {
			st.LongestList = append(st.LongestList, fmt.Sprintf("%s → %s (%dpx)",
				sc.names[segs[i].e.child], sc.names[segs[i].e.parent], int(segs[i].l)))
		}
		st.BackwardList = capList(st.BackwardList)
		st.ThroughList = capList(st.ThroughList)
	}
	return st
}

func cross(o, a, b flowPoint) float64 {
	return (a.x-o.x)*(b.y-o.y) - (a.y-o.y)*(b.x-o.x)
}

// segmentsCross는 두 선분이 서로의 안쪽에서 교차하는지 본다(끝이 닿기만 한 것은 아니다).
func segmentsCross(p1, p2, q1, q2 flowPoint) bool {
	d1, d2 := cross(q1, q2, p1), cross(q1, q2, p2)
	d3, d4 := cross(p1, p2, q1), cross(p1, p2, q2)
	return ((d1 > 0 && d2 < 0) || (d1 < 0 && d2 > 0)) &&
		((d3 > 0 && d4 < 0) || (d3 < 0 && d4 > 0))
}

// segmentHitsRect는 선분이 사각형 안을 지나는지 본다(Liang-Barsky, erdroute.js 와 같은 방법).
func segmentHitsRect(p, q flowPoint, r erd.Rect) bool {
	if r.W <= 0 || r.H <= 0 {
		return false
	}
	d := [2]float64{q.x - p.x, q.y - p.y}
	s := [2]float64{p.x, p.y}
	lo := [2]float64{r.X, r.Y}
	hi := [2]float64{r.X + r.W, r.Y + r.H}
	t0, t1 := 0.0, 1.0
	for i := 0; i < 2; i++ {
		if math.Abs(d[i]) < 1e-9 {
			if s[i] < lo[i] || s[i] > hi[i] {
				return false
			}
			continue
		}
		a, b := (lo[i]-s[i])/d[i], (hi[i]-s[i])/d[i]
		if a > b {
			a, b = b, a
		}
		t0, t1 = max(t0, a), min(t1, b)
		if t0 > t1 {
			return false
		}
	}
	return true
}

// ---------- 흐름 배치 ----------

const (
	// 묶음 안 열 하나에 세우는 카드 수의 한계(erd.AutoLayout 의 layoutRowsMax 와 같은 값).
	arrangeFlowRows = 6
	// 묶음끼리 놓을 때의 간격. 묶음 사이 선은 묶음 안의 선보다 길고 많이 모이므로
	// 가로를 더 넓게 둔다.
	arrangeUnitGapX = 160.0
	arrangeUnitGapY = 80.0
	arrangeUnitRows = 4
)

// autoPlaceUnits는 채워 둔 묶음들을 묶음 사이의 관계 방향으로 놓는다.
//
// 묶음을 카드 하나처럼 보고 packFlow 를 한 번 더 쓴다. 묶음 A 의 표가 묶음 B 의 표를
// 가리키면 A → B 이고, 그러면 B 가 A 의 왼쪽에 선다. 원점은 따로 주지 않으면 이 묶음들의
// 표가 지금 놓인 자리의 왼쪽 위다 — 도면이 엉뚱한 곳으로 날아가지 않게.
func autoPlaceUnits(sc *layoutScene, units []*packUnit, edges []flowEdge, origin *struct {
	X *float64 `json:"x"`
	Y *float64 `json:"y"`
}) error {
	ox, oy := math.Inf(1), math.Inf(1)
	for _, u := range units {
		for _, k := range u.keys {
			ox, oy = min(ox, sc.rects[k].X), min(oy, sc.rects[k].Y)
		}
	}
	if origin != nil {
		var err error
		if ox, err = checkCoord("origin", origin.X); err != nil {
			return err
		}
		if oy, err = checkCoord("origin", origin.Y); err != nil {
			return err
		}
	}

	owner := map[string]string{}
	items := make([]packItem, len(units))
	for i, u := range units {
		id := fmt.Sprint(i)
		items[i] = packItem{id: id, w: u.w, h: u.h}
		for _, k := range u.keys {
			owner[k] = id
		}
	}
	seen := map[flowEdge]bool{}
	unitEdges := []flowEdge{}
	for _, e := range edges {
		c, okc := owner[e.child]
		p, okp := owner[e.parent]
		if !okc || !okp || c == p {
			continue
		}
		ue := flowEdge{child: c, parent: p}
		if !seen[ue] {
			seen[ue] = true
			unitEdges = append(unitEdges, ue)
		}
	}
	pos, _, _ := packFlow(items, unitEdges, arrangeUnitGapX, arrangeUnitGapY, arrangeUnitRows)
	for i, u := range units {
		p := pos[fmt.Sprint(i)]
		u.x, u.y = math.Round(ox+p.x), math.Round(oy+p.y)
	}
	return nil
}

// groupFlow는 그룹 사이를 넘는 선을 방향별로 센다("주문 → 회원 (3)").
//
// 묶음끼리의 자리를 정할 때 가장 먼저 필요한 값이다. 표 사이 관계 수백 줄을 모델이
// 직접 세게 하면 틀리고, 틀리면 묶음 순서가 거꾸로 선다.
func groupFlow(sc *layoutScene, edges []flowEdge) []string {
	const outside = "(그룹 밖)"
	of := func(key string) string {
		if g := sc.groupOf(sc.rects[key]); g != nil {
			return groupName(g)
		}
		return outside
	}
	count := map[[2]string]int{}
	order := [][2]string{}
	for _, e := range edges {
		k := [2]string{of(e.child), of(e.parent)}
		if k[0] == k[1] {
			continue
		}
		if count[k] == 0 {
			order = append(order, k)
		}
		count[k]++
	}
	sort.SliceStable(order, func(i, j int) bool { return count[order[i]] > count[order[j]] })
	out := make([]string, 0, len(order))
	for _, k := range order {
		out = append(out, fmt.Sprintf("%s → %s (%d)", k[0], k[1], count[k]))
	}
	return capList(out)
}

// packItem은 흐름 배치에 넣는 것 하나다(카드든 그룹이든 크기만 있으면 된다).
type packItem struct {
	id   string
	w, h float64
}

// packFlow는 묶음을 흐름 순서로 놓는다. 좌표는 묶음의 왼쪽 위(0, 0) 기준이다.
//
// 계층 배치(Sugiyama)의 가벼운 판이다.
//  1. 열: 아무것도 가리키지 않는 것이 0열, 나머지는 가리키는 것들보다 한 칸 오른쪽.
//     그다음 **부모를 자식 쪽으로 당긴다** — 자식이 3열에만 있는 부모를 0열에 두면 그
//     선이 1·2열을 가로지른다. 묶음 안에서 아무와도 이어지지 않은 것은 **맨 오른쪽 열**로
//     따로 보낸다 — 이어진 것들 사이에 끼면 선이 그 위를 지난다.
//  2. 여러 열을 건너는 선에는 건너는 열마다 **빈 통로**(크기만 있는 가짜 항목)를 끼운다.
//     통로도 순서 정하기와 높이 맞추기에 참여하므로, 중간 열에 그 선이 지나갈 틈이 남는다.
//     통로가 없으면 그 선은 중간 열의 카드를 그대로 관통한다.
//  3. 열 안의 순서: 이웃한 열의 자리 평균(무게중심)으로 몇 번 오가며 정렬한다. 교차를
//     줄이는 가장 값싼 방법이고, 같은 입력이면 같은 결과가 나온다(안정 정렬).
//  4. 높이: 자식은 부모들의 가운데 높이에 맞춰 놓되 위의 카드와 겹치지 않게 내린다.
//     부모와 같은 높이면 선이 곧은 가로선이 된다.
//
// 열 하나에 진짜 카드가 maxRows 를 넘으면 옆으로 나눠 쓴다(세로로 끝없이 길어진 도면은
// 한 화면에 담기지 않는다). maxRows 가 0이면 나누지 않는다.
func packFlow(items []packItem, edges []flowEdge, gapX, gapY float64, maxRows int) (map[string]flowPoint, float64, float64) {
	pos := make(map[string]flowPoint, len(items))
	if len(items) == 0 {
		return pos, 0, 0
	}
	size := make(map[string]packItem, len(items))
	for _, it := range items {
		size[it.id] = it
	}
	parents := map[string][]string{}
	children := map[string][]string{}
	linked := map[string]bool{}
	for _, e := range edges {
		if _, ok := size[e.child]; !ok {
			continue
		}
		if _, ok := size[e.parent]; !ok {
			continue
		}
		parents[e.child] = append(parents[e.child], e.parent)
		children[e.parent] = append(children[e.parent], e.child)
		linked[e.child], linked[e.parent] = true, true
	}

	// 1. 열. 순환이 있으면 항목 수만큼 돈 뒤 멈춘다(끝나지 않는 계산보다 조금 어긋난 배치가 낫다).
	depth := map[string]int{}
	for round := 0; round <= len(items); round++ {
		changed := false
		for _, it := range items {
			want := 0
			for _, p := range parents[it.id] {
				want = max(want, depth[p]+1)
			}
			if want > depth[it.id] && want < len(items) {
				depth[it.id] = want
				changed = true
			}
		}
		if !changed {
			break
		}
	}
	// 부모를 자식 쪽으로 당긴다. 가장 오른쪽 열부터 보면 한 번에 자리가 잡힌다(당겨진
	// 부모가 다시 그 부모를 당길 수 있다). 자식보다 한 칸 왼쪽을 넘지 않으므로 방향은
	// 그대로이고, 순환이 있어 그 조건이 어긋나 있으면 당기지 않는다.
	order := append([]packItem(nil), items...)
	sort.SliceStable(order, func(i, j int) bool { return depth[order[i].id] > depth[order[j].id] })
	for round := 0; round < 3; round++ {
		for _, it := range order {
			kids := children[it.id]
			if len(kids) == 0 {
				continue
			}
			want := len(items)
			for _, c := range kids {
				want = min(want, depth[c]-1)
			}
			if want > depth[it.id] {
				depth[it.id] = want
			}
		}
	}

	maxDepth := 0
	for _, it := range items {
		if linked[it.id] {
			maxDepth = max(maxDepth, depth[it.id])
		}
	}
	layers := make([][]string, maxDepth+2)
	anyLinked := len(linked) > 0
	for _, it := range items {
		switch {
		case !anyLinked:
			layers[0] = append(layers[0], it.id)
		case linked[it.id]:
			layers[depth[it.id]] = append(layers[depth[it.id]], it.id)
		default:
			layers[maxDepth+1] = append(layers[maxDepth+1], it.id)
		}
	}

	// 2. 여러 열을 건너는 선에 통로를 끼운다. 통로는 결과 좌표에 나오지 않는다.
	dummy := map[string]bool{}
	nd := 0
	for _, it := range items {
		c := it.id
		ps := parents[c]
		for i, p := range ps {
			if depth[c]-depth[p] <= 1 {
				continue
			}
			prev := p
			for d := depth[p] + 1; d < depth[c]; d++ {
				id := fmt.Sprintf("\x00d%d", nd)
				nd++
				dummy[id] = true
				size[id] = packItem{id: id, w: 0, h: flowChannelH}
				depth[id] = d
				layers[d] = append(layers[d], id)
				// prev → id 로 잇는다(부모 쪽 목록에서 원래 자식을 통로로 바꾼다).
				replace(children[prev], c, id)
				parents[id] = []string{prev}
				children[id] = []string{c}
				prev = id
			}
			ps[i] = prev
		}
	}
	for len(layers) > 0 && len(layers[len(layers)-1]) == 0 {
		layers = layers[:len(layers)-1]
	}

	// 3. 열 안의 순서.
	rank := map[string]float64{}
	for _, l := range layers {
		for i, id := range l {
			rank[id] = float64(i) / float64(max(len(l), 1))
		}
	}
	sortBy := func(l []string, nb map[string][]string) {
		score := make(map[string]float64, len(l))
		for _, id := range l {
			sum, n := 0.0, 0
			for _, o := range nb[id] {
				sum += rank[o]
				n++
			}
			if n == 0 {
				score[id] = rank[id] // 이웃이 없으면 지금 자리를 지킨다.
				continue
			}
			score[id] = sum / float64(n)
		}
		sort.SliceStable(l, func(i, j int) bool { return score[l[i]] < score[l[j]] })
		for i, id := range l {
			rank[id] = float64(i) / float64(max(len(l), 1))
		}
	}
	for iter := 0; iter < 4; iter++ {
		for i := 1; i < len(layers); i++ {
			sortBy(layers[i], parents)
		}
		for i := len(layers) - 2; i >= 0; i-- {
			sortBy(layers[i], children)
		}
	}

	// 열이 너무 길면 나눈다. 통로는 세지 않는다 — 통로 때문에 진짜 카드가 옆 칸으로
	// 밀려나면 그 자체로 새 긴 선이 생긴다.
	cols := [][]string{}
	for _, l := range layers {
		cur := []string{}
		real := 0
		for _, id := range l {
			if !dummy[id] && maxRows > 0 && real == maxRows {
				cols = append(cols, cur)
				cur, real = []string{}, 0
			}
			cur = append(cur, id)
			if !dummy[id] {
				real++
			}
		}
		cols = append(cols, cur)
	}

	// 4. 좌표.
	at := map[string]flowPoint{}
	x, totalW, totalH := 0.0, 0.0, 0.0
	for _, col := range cols {
		colW := 0.0
		for _, id := range col {
			colW = max(colW, size[id].w)
		}
		y := 0.0
		for _, id := range col {
			it := size[id]
			want := y
			if ps := parents[id]; len(ps) > 0 {
				sum, n := 0.0, 0
				for _, p := range ps {
					if pp, ok := at[p]; ok {
						sum += pp.y + size[p].h/2
						n++
					}
				}
				if n > 0 {
					want = max(y, sum/float64(n)-it.h/2)
				}
			}
			want = math.Round(want)
			at[id] = flowPoint{x: x, y: want}
			y = want + it.h + gapY
			if !dummy[id] {
				pos[id] = at[id]
				totalH = max(totalH, want+it.h)
				totalW = max(totalW, x+it.w)
			}
		}
		x += colW + gapX
	}
	return pos, totalW, totalH
}

// flowChannelH는 여러 열을 건너는 선에 남기는 통로의 높이다(gapY 와 함께 쓰인다).
const flowChannelH = 24.0

func replace(list []string, from, to string) {
	for i, v := range list {
		if v == from {
			list[i] = to
			return
		}
	}
}

// packGrid는 적은 순서 그대로 왼쪽→오른쪽, 위→아래로 채운다. 열은 가장 넓은 카드,
// 줄은 가장 높은 카드에 맞춘다 — 칸 크기를 하나로 두면 컬럼 많은 표 하나 때문에
// 모든 줄이 벌어진다.
func packGrid(items []packItem, cols int, gap float64) (map[string]flowPoint, float64, float64) {
	pos := make(map[string]flowPoint, len(items))
	if len(items) == 0 {
		return pos, 0, 0
	}
	if cols <= 0 {
		cols = int(math.Ceil(math.Sqrt(float64(len(items)))))
	}
	cols = min(cols, len(items))
	rows := (len(items) + cols - 1) / cols
	colW := make([]float64, cols)
	rowH := make([]float64, rows)
	for i, it := range items {
		colW[i%cols] = max(colW[i%cols], it.w)
		rowH[i/cols] = max(rowH[i/cols], it.h)
	}
	for i, it := range items {
		c, r := i%cols, i/cols
		x, y := 0.0, 0.0
		for j := 0; j < c; j++ {
			x += colW[j] + gap
		}
		for j := 0; j < r; j++ {
			y += rowH[j] + gap
		}
		pos[it.id] = flowPoint{x: x, y: y}
	}
	w, h := gap*float64(cols-1), gap*float64(rows-1)
	for _, v := range colW {
		w += v
	}
	for _, v := range rowH {
		h += v
	}
	return pos, w, h
}
