package api

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/google/uuid"

	"dbstudio/internal/erd"
)

// 손으로 그린 것 같은 배치(organic).
//
// 흐름 배치(packFlow)는 "부모는 왼쪽, 자식은 오른쪽"을 지킨다. 그런데 사람이 직접 그린
// 도면을 나란히 놓고 보면 사람은 방향을 거의 신경 쓰지 않는다. 대신 세 가지를 지킨다.
//
//  1. 이어진 것은 가깝게. 연결이 많은 표(회원, 앨범)가 가운데 서고 자식들이 위·아래·옆을
//     둘러싼다. 엄격한 열 배치에서는 허브의 자식들이 한 열에 길게 늘어서 선이 부채처럼
//     멀리 퍼진다.
//  2. 두 무리를 잇는 표는 그 사이에. 회원과 앨범을 함께 가리키는 표(좋아요, 재생 기록)는
//     두 허브 사이 가운데에 놓인다. 열 배치에서는 그 표가 두 부모보다 오른쪽 끝으로 밀려
//     두 선이 모두 길어진다.
//  3. 그룹은 작게 뭉치고 그룹 사이는 넓게 비운다. 그 여백으로 그룹을 넘는 선이 지나간다.
//
// 그래서 힘으로 푼다: 선은 용수철(정해진 길이로 당긴다), 모든 카드 쌍은 서로 밀어내고,
// 같은 그룹은 그룹 중심으로 약하게 당긴다. 난수를 쓰지 않는다 — 시작점은 흐름 배치이고
// 계산 순서도 고정이라, 같은 도면에서 같은 결과가 나온다(되돌렸다 다시 해도 같은 그림).
// 힘으로 푼 뒤에는 겹침을 마저 밀어내고, 그룹 테두리를 씌우고, 비슷한 높이의 카드를
// 한 줄로 맞춘다 — 손으로 그린 도면이 정돈돼 보이는 것은 그 줄 맞춤 덕분이다.

const (
	// 선 하나가 바라는 길이(두 카드 테두리 사이). 까마귀발과 이름표가 들어갈 만큼.
	organicEdgeGap   = 110.0
	organicEdgeGapIn = 60.0
	// 같은 그룹 카드 사이, 다른 무리 카드 사이에 남길 최소 여백.
	organicGapIn  = 50.0
	organicGapOut = 140.0
	organicIters  = 360
	// 그룹 중심으로 당기는 힘. 약하면 두 장짜리 그룹도 바깥 선에 끌려 도면을 가로지르게
	// 늘어나고, 그 그룹이 다른 무리 사이를 막는다.
	organicCohesion = 0.12
	// 줄 맞춤: 윗변·왼변이 이만큼 안으로 가까우면 같은 줄로 본다.
	organicAlign = 36.0
	organicSnap  = 10.0
)

// oNode는 힘 배치의 카드 하나다. x, y 는 **가운데** 좌표다.
type oNode struct {
	id    string
	w, h  float64
	x, y  float64
	group int // -1 이면 그룹 밖
}

// organicFrame은 배치가 끝난 그룹의 테두리다.
type organicFrame struct {
	x, y, w, h float64
}

// organicLayout은 카드들을 힘으로 놓고 그룹 테두리를 돌려준다. 카드 좌표는 nodes 에
// 직접 쓴다(가운데 좌표). 그룹 테두리는 안쪽 여백(padX·padTop·padBot)을 포함한다.
func organicLayout(nodes []*oNode, edges []flowEdge, nGroups int, padX, padTop, padBot float64) []organicFrame {
	n := len(nodes)
	frames := make([]organicFrame, nGroups)
	if n == 0 {
		return frames
	}
	idx := make(map[string]int, n)
	for i, nd := range nodes {
		idx[nd.id] = i
	}
	type link struct{ a, b int }
	links := []link{}
	degree := make([]int, n)
	for _, e := range edges {
		a, okA := idx[e.child]
		b, okB := idx[e.parent]
		if !okA || !okB || a == b {
			continue
		}
		links = append(links, link{a, b})
		degree[a]++
		degree[b]++
	}

	// 1. 힘. 온도(한 번에 움직일 수 있는 거리)를 높게 시작해 식힌다.
	dx := make([]float64, n)
	dy := make([]float64, n)
	gx := make([]float64, nGroups)
	gy := make([]float64, nGroups)
	gc := make([]int, nGroups)
	for it := 0; it < organicIters; it++ {
		temp := 160.0 * math.Pow(1-float64(it)/organicIters, 1.6)
		temp = max(temp, 2)
		for i := range dx {
			dx[i], dy[i] = 0, 0
		}

		// 밀어내기. 카드 크기를 보고 "이만큼은 떨어져야 한다"를 정한 뒤, 그 안으로 들어온
		// 만큼 민다. 점으로 보고 밀면 키 큰 카드끼리 위아래로 겹친다.
		for i := 0; i < n; i++ {
			a := nodes[i]
			if a.group < 0 && degree[i] == 0 {
				continue // 고립된 표는 끝에 따로 세운다(parkIsolated). 힘에 끼면 남을 밀기만 한다.
			}
			for j := i + 1; j < n; j++ {
				b := nodes[j]
				if b.group < 0 && degree[j] == 0 {
					continue
				}
				gap := organicGapOut
				if a.group >= 0 && a.group == b.group {
					gap = organicGapIn
				}
				sx := (a.w+b.w)/2 + gap
				sy := (a.h+b.h)/2 + gap
				ddx, ddy := b.x-a.x, b.y-a.y
				q := max(math.Abs(ddx)/sx, math.Abs(ddy)/sy)
				if q >= 1.4 {
					continue
				}
				if ddx == 0 && ddy == 0 {
					// 같은 자리면 순서로 비켜 세운다(난수 대신).
					ddx, ddy = float64(j-i), 0.5
				}
				d := math.Hypot(ddx, ddy)
				f := 90 * (1.4 - q)
				fx, fy := f*ddx/d, f*ddy/d
				dx[i] -= fx
				dy[i] -= fy
				dx[j] += fx
				dy[j] += fy
			}
		}

		// 용수철. 바라는 길이는 테두리 사이 거리로 잡는다 — 가운데 사이로 잡으면 큰 카드에
		// 붙은 선만 짧아진다.
		for _, l := range links {
			a, b := nodes[l.a], nodes[l.b]
			ddx, ddy := b.x-a.x, b.y-a.y
			d := math.Hypot(ddx, ddy)
			if d < 1 {
				continue
			}
			ux, uy := ddx/d, ddy/d
			// 같은 그룹 안의 선은 짧게 — 그룹이 촘촘해야 그룹 사이 여백이 산다. 그룹을
			// 넘는 선은 그 여백을 지나야 하므로 넉넉하게.
			edgeGap := organicEdgeGap
			if a.group >= 0 && a.group == b.group {
				edgeGap = organicEdgeGapIn
			}
			want := rayExit(a.w, a.h, ux, uy) + rayExit(b.w, b.h, ux, uy) + edgeGap
			// 허브에 붙은 용수철은 약하게 — 안 그러면 허브가 자식 스무 개에 끌려
			// 이리저리 흔들리고, 자식들은 허브 위로 모여 든다.
			k := 0.12 / math.Sqrt(float64(min(degree[l.a], degree[l.b])))
			if a.group >= 0 && a.group == b.group {
				k *= 2 // 그룹 안의 선은 더 세게 — 그룹이 바깥 선에 끌려 늘어나지 않게.
			}
			f := k * (d - want)
			fx, fy := f*ux, f*uy
			dx[l.a] += fx
			dy[l.a] += fy
			dx[l.b] -= fx
			dy[l.b] -= fy
		}

		// 그룹은 제 중심으로 모인다. 그룹이 흩어지면 테두리가 커져 다른 그룹을 덮는다.
		for g := range gx {
			gx[g], gy[g], gc[g] = 0, 0, 0
		}
		for _, nd := range nodes {
			if nd.group >= 0 {
				gx[nd.group] += nd.x
				gy[nd.group] += nd.y
				gc[nd.group]++
			}
		}
		for i, nd := range nodes {
			if nd.group < 0 || gc[nd.group] < 2 {
				continue
			}
			cx, cy := gx[nd.group]/float64(gc[nd.group]), gy[nd.group]/float64(gc[nd.group])
			dx[i] += organicCohesion * (cx - nd.x)
			dy[i] += organicCohesion * (cy - nd.y)
		}

		// 전체 중심으로 아주 약하게. 서로 이어지지 않은 무리가 밀어내기만 받으면 끝없이
		// 멀어져 도면이 빈 공간투성이가 된다.
		cx, cy, cn := 0.0, 0.0, 0
		for i, nd := range nodes {
			if degree[i] > 0 {
				cx += nd.x
				cy += nd.y
				cn++
			}
		}
		if cn > 0 {
			cx, cy = cx/float64(cn), cy/float64(cn)
			for i, nd := range nodes {
				if degree[i] > 0 {
					dx[i] += 0.01 * (cx - nd.x)
					dy[i] += 0.01 * (cy - nd.y)
				}
			}
		}

		for i, nd := range nodes {
			l := math.Hypot(dx[i], dy[i])
			if l > temp {
				dx[i], dy[i] = dx[i]/l*temp, dy[i]/l*temp
			}
			nd.x += dx[i]
			nd.y += dy[i]
		}
	}

	// 2. 줄 맞춤. 손으로 그린 도면은 카드 윗변과 왼변이 줄을 선다.
	pairs := make([][2]int, 0, len(links))
	for _, l := range links {
		pairs = append(pairs, [2]int{l.a, l.b})
	}
	alignLinked(nodes, pairs)
	alignGroups(nodes)

	// 3. 남은 겹침을 마저 민다. 다른 무리 사이에는 그룹 테두리가 들어설 자리까지 남긴다.
	outer := padX*2 + organicGapIn
	separate(nodes, func(a, b *oNode) float64 {
		if a.group >= 0 && a.group == b.group {
			return organicGapIn
		}
		return outer
	})

	// 4. 그룹 테두리를 씌우고, 테두리끼리·테두리와 남의 카드가 겹치면 그룹째 민다.
	for round := 0; round < 200; round++ {
		frames = groupFrames(nodes, nGroups, padX, padTop, padBot)
		if !pushFrames(nodes, frames) {
			break
		}
	}
	frames = groupFrames(nodes, nGroups, padX, padTop, padBot)

	// 그룹 밖 카드는 두 그룹 사이에서 이쪽저쪽으로 밀리다 끝내 테두리에 걸쳐 남을 수 있다
	// (밀어내기는 수렴을 보장하지 않는다). 그래서 마지막에 **가장 가까운 빈자리**로 옮긴다.
	// 주변을 가까운 순서로 훑으므로 반드시 끝나고, 같은 입력이면 같은 자리다.
	settleFree(nodes, frames, degree)

	// 고립된 표(어떤 선에도 걸리지 않은 것)는 힘으로 놓으면 밀려나 구석으로 날아간다.
	// 사람은 그런 표를 도면 아래에 가지런히 모아 둔다.
	parkIsolated(nodes, frames, degree)

	// 5. 격자에 붙인다. 좌표가 10 단위면 사람이 이어서 손볼 때 맞추기 쉽다.
	for _, nd := range nodes {
		nd.x = math.Round((nd.x-nd.w/2)/organicSnap)*organicSnap + nd.w/2
		nd.y = math.Round((nd.y-nd.h/2)/organicSnap)*organicSnap + nd.h/2
	}
	return groupFrames(nodes, nGroups, padX, padTop, padBot)
}

// blocked는 카드 i 를 (x, y)(가운데)에 두면 테두리나 다른 카드와 겹치는지 본다.
func blocked(nodes []*oNode, frames []organicFrame, i int, x, y float64) bool {
	a := nodes[i]
	l, t, r, b := x-a.w/2, y-a.h/2, x+a.w/2, y+a.h/2
	for _, f := range frames {
		if f.w == 0 {
			continue
		}
		g := organicGapIn
		if l < f.x+f.w+g && f.x-g < r && t < f.y+f.h+g && f.y-g < b {
			return true
		}
	}
	for j, o := range nodes {
		if j == i {
			continue
		}
		g := organicGapIn
		if l < o.x+o.w/2+g && o.x-o.w/2-g < r && t < o.y+o.h/2+g && o.y-o.h/2-g < b {
			return true
		}
	}
	return false
}

// settleFree는 겹친 그룹 밖 카드를 가장 가까운 빈자리로 옮긴다. 연결이 많은 카드부터
// 자리를 잡는다 — 허브가 먼저 서야 나머지가 그 둘레를 찾는다.
func settleFree(nodes []*oNode, frames []organicFrame, degree []int) {
	order := []int{}
	for i, nd := range nodes {
		if nd.group < 0 && degree[i] > 0 {
			order = append(order, i)
		}
	}
	sort.SliceStable(order, func(a, b int) bool { return degree[order[a]] > degree[order[b]] })
	const step = 20.0
	for _, i := range order {
		nd := nodes[i]
		if !blocked(nodes, frames, i, nd.x, nd.y) {
			continue
		}
		// 정사각형 고리를 한 칸씩 넓히며, 고리 안에서는 가까운 점부터 본다.
		for ring := 1; ring <= 150; ring++ {
			type cand struct{ x, y, d float64 }
			cs := []cand{}
			for k := -ring; k <= ring; k++ {
				for _, p := range [][2]int{{k, -ring}, {k, ring}, {-ring, k}, {ring, k}} {
					x, y := nd.x+float64(p[0])*step, nd.y+float64(p[1])*step
					cs = append(cs, cand{x, y, math.Hypot(x-nd.x, y-nd.y)})
				}
			}
			sort.SliceStable(cs, func(a, b int) bool { return cs[a].d < cs[b].d })
			found := false
			for _, c := range cs {
				if !blocked(nodes, frames, i, c.x, c.y) {
					nd.x, nd.y = c.x, c.y
					found = true
					break
				}
			}
			if found {
				break
			}
		}
	}
}

// parkIsolated는 어떤 선에도 걸리지 않은 그룹 밖 카드를 도면 아래에 한 줄로 세운다.
func parkIsolated(nodes []*oNode, frames []organicFrame, degree []int) {
	minX, maxX, maxY := math.Inf(1), math.Inf(-1), math.Inf(-1)
	isolated := []*oNode{}
	for i, nd := range nodes {
		if nd.group < 0 && degree[i] == 0 {
			isolated = append(isolated, nd)
			continue
		}
		minX, maxX = min(minX, nd.x-nd.w/2), max(maxX, nd.x+nd.w/2)
		maxY = max(maxY, nd.y+nd.h/2)
	}
	for _, f := range frames {
		if f.w == 0 {
			continue
		}
		minX, maxX, maxY = min(minX, f.x), max(maxX, f.x+f.w), max(maxY, f.y+f.h)
	}
	if len(isolated) == 0 {
		return
	}
	if math.IsInf(minX, 1) {
		minX, maxX, maxY = 0, 1600, -organicGapOut
	}
	sort.SliceStable(isolated, func(a, b int) bool { return isolated[a].id < isolated[b].id })
	x, y, rowH := minX, maxY+organicGapOut, 0.0
	for _, nd := range isolated {
		if x > minX && x+nd.w > max(maxX, minX+1200) {
			x, y, rowH = minX, y+rowH+organicGapIn, 0
		}
		nd.x, nd.y = x+nd.w/2, y+nd.h/2
		x += nd.w + organicGapIn
		rowH = max(rowH, nd.h)
	}
}

// organicBest는 시작점을 바꿔 가며 여러 번 풀고 가장 읽기 쉬운 결과를 고른다.
//
// 힘 배치는 시작점에 따라 다른 자리에서 굳는다(국소해). 한 번만 풀면 두 무리를 잇는
// 표가 엉뚱한 바깥에 붙은 채 끝나기도 한다. 난수 대신 시작점을 **뒤집고 돌린** 네 벌로
// 푼다 — 그대로, 좌우 반전, 상하 반전, 가로세로 바꿈. 같은 입력이면 같은 네 벌이라
// 결과도 같다. 고르는 기준은 사람에게 보이는 것: 카드를 관통하는 선, 교차, 선 길이.
func organicBest(nodes []*oNode, edges []flowEdge, nGroups int, padX, padTop, padBot float64) []organicFrame {
	type variant func(x, y float64) (float64, float64)
	variants := []variant{
		func(x, y float64) (float64, float64) { return x, y },
		func(x, y float64) (float64, float64) { return -x, y },
		func(x, y float64) (float64, float64) { return x, -y },
		func(x, y float64) (float64, float64) { return y, x },
	}
	// 시작점을 원점 기준으로 맞춘다. 지금 도면이 어디에 놓여 있었는지가 결과에 새면 안 된다 —
	// 대칭인 두 후보(좌우 반전 등)의 점수는 거의 같아서, 평행이동만큼의 부동소수점 오차가
	// 어느 쪽이 이길지를 바꾼다. 실제로 같은 도면을 두 번 정리하면 그룹이 위아래로 뒤바뀌었다.
	minX, minY := math.Inf(1), math.Inf(1)
	for _, nd := range nodes {
		minX, minY = min(minX, nd.x), min(minY, nd.y)
	}
	for _, nd := range nodes {
		nd.x, nd.y = math.Round(nd.x-minX), math.Round(nd.y-minY)
	}

	var best []*oNode
	var bestFrames []organicFrame
	bestScore := math.Inf(1)
	for _, v := range variants {
		trial := make([]*oNode, len(nodes))
		for i, nd := range nodes {
			c := *nd
			c.x, c.y = v(nd.x, nd.y)
			trial[i] = &c
		}
		frames := organicLayout(trial, edges, nGroups, padX, padTop, padBot)
		// 사실상 같은 점수면 앞의 후보를 지킨다(오차로 순서가 뒤집히지 않게).
		if s := organicScore(trial, edges); best == nil || s < bestScore-1e-6*max(1, bestScore) {
			best, bestFrames, bestScore = trial, frames, s
		}
	}
	for i := range nodes {
		*nodes[i] = *best[i]
	}
	return bestFrames
}

// organicScore는 배치의 읽기 어려움이다(작을수록 좋다). flowMetrics 와 같은 규칙으로 센다.
// 관통 하나는 교차 하나보다 무겁다 — 관통한 선은 화면이 돌려 그리느라 길게 꺾인다.
func organicScore(nodes []*oNode, edges []flowEdge) float64 {
	rects := make(map[string]erd.Rect, len(nodes))
	for _, nd := range nodes {
		rects[nd.id] = erd.Rect{X: nd.x - nd.w/2, Y: nd.y - nd.h/2, W: nd.w, H: nd.h}
	}
	type seg struct {
		a, b flowPoint
		e    flowEdge
	}
	segs := []seg{}
	score := 0.0
	for _, e := range edges {
		cr, okc := rects[e.child]
		pr, okp := rects[e.parent]
		if !okc || !okp {
			continue
		}
		a, b := attach(cr, pr)
		segs = append(segs, seg{a, b, e})
		score += math.Hypot(b.x-a.x, b.y-a.y)
		for key, r := range rects {
			if key == e.child || key == e.parent {
				continue
			}
			in := erd.Rect{X: r.X + 4, Y: r.Y + 4, W: r.W - 8, H: r.H - 8}
			if segmentHitsRect(a, b, in) {
				score += 600
			}
		}
	}
	for i := 0; i < len(segs); i++ {
		for j := i + 1; j < len(segs); j++ {
			x, y := segs[i].e, segs[j].e
			if x.child == y.child || x.child == y.parent || x.parent == y.child || x.parent == y.parent {
				continue
			}
			if segmentsCross(segs[i].a, segs[i].b, segs[j].a, segs[j].b) {
				score += 300
			}
		}
	}
	return score
}

// rayExit은 가운데에서 (ux, uy) 방향으로 나가는 선이 w×h 카드의 테두리에 닿는 거리다.
func rayExit(w, h, ux, uy float64) float64 {
	tx, ty := math.Inf(1), math.Inf(1)
	if math.Abs(ux) > 1e-9 {
		tx = w / 2 / math.Abs(ux)
	}
	if math.Abs(uy) > 1e-9 {
		ty = h / 2 / math.Abs(uy)
	}
	return min(tx, ty)
}

// alignLinked는 같은 그룹 안에서 이어진 두 카드가 대각선으로 어긋나 있으면 가까운 축으로
// 줄을 맞춘다. 대각선 선은 화면이 꺾어 그리고(가로·세로 중 긴 쪽으로 붙는다) 그룹을
// 헐겁게 만든다 — 사람은 이어진 두 표를 나란히 또는 위아래로 둔다.
//
// 카드 크기의 절반보다 덜 어긋난 것만 맞춘다. 더 멀면 그 사이에 다른 카드가 있을 수
// 있고, 억지로 맞추면 그 카드를 밀어낸다.
func alignLinked(nodes []*oNode, pairs [][2]int) {
	for _, p := range pairs {
		a, b := nodes[p[0]], nodes[p[1]]
		if a.group < 0 || a.group != b.group {
			continue
		}
		dTop := (b.y - b.h/2) - (a.y - a.h/2)
		dLeft := (b.x - b.w/2) - (a.x - a.w/2)
		horiz := math.Abs(b.x-a.x) >= math.Abs(b.y-a.y)
		// 마주 보는 변 사이의 틈. 화면은 가로·세로 중 가운데 차이가 큰 쪽으로 선을 붙이는데,
		// 그 방향의 틈이 거의 없으면 선이 카드 모서리를 끼고 S자로 꺾인다.
		gapX := math.Abs(b.x-a.x) - (a.w+b.w)/2
		gapY := math.Abs(b.y-a.y) - (a.h+b.h)/2
		switch {
		case horiz && math.Abs(dTop) < min(a.h, b.h)/2:
			// 나란히 선 쌍은 윗변을 맞춘다. 이 표(자식)를 부모 쪽에 맞춘다.
			a.y += dTop
		case !horiz && math.Abs(dLeft) < min(a.w, b.w)/2:
			a.x += dLeft
		case horiz && gapX < organicEdgeGapIn/2 && gapY > 0:
			// 가로로 붙을 선인데 가로 틈이 없다 → 위아래로 쌓는다(가운데를 맞춘다).
			a.x = b.x
		case !horiz && gapY < organicEdgeGapIn/2 && gapX > 0:
			a.y = b.y
		}
	}
}

// alignGroups는 같은 무리 안에서 윗변(또는 왼변)이 가까운 카드를 한 줄로 맞춘다.
func alignGroups(nodes []*oNode) {
	byGroup := map[int][]*oNode{}
	keys := []int{}
	for _, nd := range nodes {
		if _, ok := byGroup[nd.group]; !ok {
			keys = append(keys, nd.group)
		}
		byGroup[nd.group] = append(byGroup[nd.group], nd)
	}
	sort.Ints(keys)
	snap := func(list []*oNode, get func(*oNode) float64, set func(*oNode, float64)) {
		sorted := append([]*oNode(nil), list...)
		sort.SliceStable(sorted, func(i, j int) bool { return get(sorted[i]) < get(sorted[j]) })
		for s := 0; s < len(sorted); {
			e := s + 1
			for e < len(sorted) && get(sorted[e])-get(sorted[s]) <= organicAlign {
				e++
			}
			if e-s > 1 {
				sum := 0.0
				for _, nd := range sorted[s:e] {
					sum += get(nd)
				}
				avg := sum / float64(e-s)
				for _, nd := range sorted[s:e] {
					set(nd, avg)
				}
			}
			s = e
		}
	}
	for _, g := range keys {
		list := byGroup[g]
		snap(list, func(n *oNode) float64 { return n.y - n.h/2 }, func(n *oNode, v float64) { n.y = v + n.h/2 })
		snap(list, func(n *oNode) float64 { return n.x - n.w/2 }, func(n *oNode, v float64) { n.x = v + n.w/2 })
	}
}

// separate는 겹친(여백 포함) 카드 쌍을 덜 겹친 축으로 반씩 민다. 줄 맞춤을 덜 깨려고
// 한 축으로만 민다.
func separate(nodes []*oNode, gapOf func(a, b *oNode) float64) {
	for pass := 0; pass < 400; pass++ {
		moved := false
		for i := 0; i < len(nodes); i++ {
			for j := i + 1; j < len(nodes); j++ {
				a, b := nodes[i], nodes[j]
				gap := gapOf(a, b)
				ox := (a.w+b.w)/2 + gap - math.Abs(b.x-a.x)
				oy := (a.h+b.h)/2 + gap - math.Abs(b.y-a.y)
				if ox <= 0.5 || oy <= 0.5 {
					continue
				}
				moved = true
				if ox < oy {
					s := sign(b.x-a.x, i, j)
					a.x -= s * ox / 2
					b.x += s * ox / 2
				} else {
					s := sign(b.y-a.y, i, j)
					a.y -= s * oy / 2
					b.y += s * oy / 2
				}
			}
		}
		if !moved {
			return
		}
	}
}

func sign(v float64, i, j int) float64 {
	switch {
	case v > 0:
		return 1
	case v < 0:
		return -1
	case i < j:
		return 1
	}
	return -1
}

func groupFrames(nodes []*oNode, nGroups int, padX, padTop, padBot float64) []organicFrame {
	frames := make([]organicFrame, nGroups)
	seen := make([]bool, nGroups)
	minX := make([]float64, nGroups)
	minY := make([]float64, nGroups)
	maxX := make([]float64, nGroups)
	maxY := make([]float64, nGroups)
	for _, nd := range nodes {
		g := nd.group
		if g < 0 {
			continue
		}
		l, t, r, b := nd.x-nd.w/2, nd.y-nd.h/2, nd.x+nd.w/2, nd.y+nd.h/2
		if !seen[g] {
			seen[g] = true
			minX[g], minY[g], maxX[g], maxY[g] = l, t, r, b
			continue
		}
		minX[g], minY[g] = min(minX[g], l), min(minY[g], t)
		maxX[g], maxY[g] = max(maxX[g], r), max(maxY[g], b)
	}
	for g := range frames {
		if !seen[g] {
			continue
		}
		frames[g] = organicFrame{
			x: minX[g] - padX, y: minY[g] - padTop,
			w: maxX[g] - minX[g] + 2*padX, h: maxY[g] - minY[g] + padTop + padBot,
		}
	}
	return frames
}

// pushFrames는 겹친 그룹 테두리, 그리고 테두리에 걸린 남의 카드를 민다. 움직였으면 true.
func pushFrames(nodes []*oNode, frames []organicFrame) bool {
	const gap = organicGapIn
	moved := false
	shift := func(g int, ddx, ddy float64) {
		for _, nd := range nodes {
			if nd.group == g {
				nd.x += ddx
				nd.y += ddy
			}
		}
		frames[g].x += ddx
		frames[g].y += ddy
	}
	overlap := func(a, b organicFrame) (float64, float64) {
		ox := min(a.x+a.w, b.x+b.w) - max(a.x, b.x) + gap
		oy := min(a.y+a.h, b.y+b.h) - max(a.y, b.y) + gap
		return ox, oy
	}
	for g := range frames {
		if frames[g].w == 0 {
			continue
		}
		for h := g + 1; h < len(frames); h++ {
			if frames[h].w == 0 {
				continue
			}
			ox, oy := overlap(frames[g], frames[h])
			if ox <= 0.5 || oy <= 0.5 {
				continue
			}
			moved = true
			a, b := frames[g], frames[h]
			if ox < oy {
				s := sign((b.x+b.w/2)-(a.x+a.w/2), g, h)
				shift(g, -s*ox/2, 0)
				shift(h, s*ox/2, 0)
			} else {
				s := sign((b.y+b.h/2)-(a.y+a.h/2), g, h)
				shift(g, 0, -s*oy/2)
				shift(h, 0, s*oy/2)
			}
		}
	}
	// 그룹 밖 카드(다른 그룹의 카드 포함)가 테두리에 걸리면 카드 쪽을 민다.
	for g, f := range frames {
		if f.w == 0 {
			continue
		}
		for i, nd := range nodes {
			if nd.group == g {
				continue
			}
			c := organicFrame{x: nd.x - nd.w/2, y: nd.y - nd.h/2, w: nd.w, h: nd.h}
			ox, oy := overlap(f, c)
			if ox <= 0.5 || oy <= 0.5 {
				continue
			}
			moved = true
			if nd.group >= 0 {
				// 남의 그룹 카드면 그 그룹째 민다(그룹끼리 겹친 것과 같다).
				if ox < oy {
					shift(nd.group, sign(nd.x-(f.x+f.w/2), g, i)*ox, 0)
				} else {
					shift(nd.group, 0, sign(nd.y-(f.y+f.h/2), g, i)*oy)
				}
				continue
			}
			if ox < oy {
				nd.x += sign(nd.x-(f.x+f.w/2), g, i) * ox
			} else {
				nd.y += sign(nd.y-(f.y+f.h/2), g, i) * oy
			}
		}
	}
	return moved
}

// planOrganic은 도면 전체를 힘 배치로 다시 놓는 계획을 만든다(arrange_layout 의 mode=organic).
//
// 그룹 소속은 이렇게 정한다: groups[].tables 로 적은 것이 우선이고, 적지 않은 기존 그룹은
// 지금 그 안에 통째로 들어 있는 표를 그대로 데려간다 — 사람이 정한 묶음을 모델이 언급하지
// 않았다고 풀어 버리면 안 된다. 어느 그룹에도 없는 표는 그룹 밖 카드로 자유롭게 놓인다
// (두 무리를 잇는 표가 그 사이에 서는 것이 이 자리다).
func planOrganic(doc *erd.Document, sc *layoutScene, in *arrangeIn, edges []flowEdge, gap float64) (
	[]*groupPlan, map[string]*cardMove, []noteMove, error) {

	if len(in.Tables)+len(in.Views)+len(in.Blocks) > 0 || in.AutoPlace {
		return nil, nil, nil, fmt.Errorf("mode=organic 은 도면 전체를 서버가 놓습니다. " +
			"tables·views·blocks·autoPlace 는 함께 쓰지 않습니다 — 묶음은 groups[].tables 로만 알려주세요")
	}

	type ogroup struct {
		plan *groupPlan
		keys []string
		old  *erd.Group
	}
	groups := []*ogroup{}
	owner := map[string]int{}
	touched := map[string]bool{}
	newGroups := 0

	for i, gi := range in.Groups {
		og := &ogroup{plan: &groupPlan{payload: map[string]any{}}}
		if gi.New {
			label := ""
			if gi.Label != nil {
				label = strings.TrimSpace(*gi.Label)
			}
			if label == "" || len(gi.Tables) == 0 {
				return nil, nil, nil, fmt.Errorf("groups[%d]: 새 그룹에는 label 과 tables 가 필요합니다", i)
			}
			newGroups++
			if len(doc.Groups)+newGroups > erd.MaxGroups {
				return nil, nil, nil, fmt.Errorf("그룹은 %d개까지 만들 수 있습니다", erd.MaxGroups)
			}
			og.plan.add = true
			og.plan.name = label
			og.plan.payload["id"] = uuid.NewString()
			og.plan.payload["label"] = label
		} else {
			g, err := findGroup(doc, gi.Group)
			if err != nil {
				return nil, nil, nil, fmt.Errorf("groups[%d]: %w", i, err)
			}
			if touched[g.ID] {
				return nil, nil, nil, fmt.Errorf("그룹 %s 을(를) 두 번 적었습니다", groupName(g))
			}
			touched[g.ID] = true
			og.old = g
			og.plan.name = groupName(g)
			og.plan.payload["id"] = g.ID
			if gi.Label != nil {
				og.plan.payload["label"] = strings.TrimSpace(*gi.Label)
			}
		}
		for _, name := range gi.Tables {
			t := findERDTable(doc, name)
			if t == nil {
				return nil, nil, nil, fmt.Errorf("groups[%d]: 테이블 %q을(를) 찾을 수 없습니다", i, name)
			}
			if prev, ok := owner[t.Key()]; ok {
				return nil, nil, nil, fmt.Errorf("%s 을(를) 두 그룹(%s, %s)에 넣었습니다",
					t.Name, groups[prev].plan.name, og.plan.name)
			}
			owner[t.Key()] = len(groups)
			og.keys = append(og.keys, t.Key())
		}
		groups = append(groups, og)
	}

	// 적지 않은 기존 그룹과, tables 없이 적은 기존 그룹은 지금 든 표를 데려간다.
	byID := map[string]int{}
	for i, og := range groups {
		if og.old != nil {
			byID[og.old.ID] = i
		}
	}
	for _, g := range doc.Groups {
		if _, ok := byID[g.ID]; !ok {
			byID[g.ID] = len(groups)
			groups = append(groups, &ogroup{
				plan: &groupPlan{name: groupName(g), payload: map[string]any{"id": g.ID}},
				old:  g,
			})
		}
	}
	for _, t := range sc.tables {
		if _, ok := owner[t.Key()]; ok {
			continue
		}
		g := sc.groupOf(sc.rects[t.Key()])
		if g == nil {
			continue
		}
		gi := byID[g.ID]
		if len(in.Groups) > 0 && gi < len(in.Groups) && len(in.Groups[gi].Tables) > 0 {
			continue // 표를 적어 준 그룹은 적은 것만이다.
		}
		owner[t.Key()] = gi
		groups[gi].keys = append(groups[gi].keys, t.Key())
	}

	// 시작점: 흐름 배치. 힘 배치는 시작점에 따라 결과가 달라지므로, 관계 방향이 이미
	// 정리된 자리에서 출발해야 같은 도면에서 같은 그림이 나오고 엉킨 채로 굳지 않는다.
	units := []*packUnit{}
	unitGroup := []int{}
	for gi, og := range groups {
		if len(og.keys) == 0 {
			continue
		}
		items := make([]packItem, 0, len(og.keys))
		for _, k := range og.keys {
			items = append(items, packItem{id: k, w: sc.rects[k].W, h: sc.rects[k].H})
		}
		rel, w, h := packFlow(items, edges, max(2*gap, 80), gap, arrangeFlowRows)
		u := &packUnit{name: og.plan.name, keys: og.keys, rel: rel, w: w, h: h}
		units = append(units, u)
		unitGroup = append(unitGroup, gi)
	}
	for _, t := range sc.tables {
		if _, ok := owner[t.Key()]; ok {
			continue
		}
		r := sc.rects[t.Key()]
		units = append(units, &packUnit{name: t.Name, keys: []string{t.Key()},
			rel: map[string]flowPoint{t.Key(): {}}, w: r.W, h: r.H})
		unitGroup = append(unitGroup, -1)
	}
	if err := autoPlaceUnits(sc, units, edges, nil); err != nil {
		return nil, nil, nil, err
	}

	nodes := []*oNode{}
	for ui, u := range units {
		for _, k := range u.keys {
			r := sc.rects[k]
			p := u.rel[k]
			nodes = append(nodes, &oNode{id: k, w: r.W, h: r.H,
				x: u.x + p.x + r.W/2, y: u.y + p.y + r.H/2, group: unitGroup[ui]})
		}
	}
	frames := organicBest(nodes, edges, len(groups), arrangePadX, arrangePadTop, arrangePadBot)

	// 원점: 주지 않으면 지금 도면(표와 그룹 테두리)의 왼쪽 위. 도면이 엉뚱한 곳으로 날아가지
	// 않게. 테두리를 함께 보는 이유: 결과는 테두리를 원점에 맞추므로, 표만 보면 다시 정리할
	// 때마다 머리 여백만큼 아래로 밀린다.
	ox, oy := math.Inf(1), math.Inf(1)
	for _, t := range sc.tables {
		ox, oy = min(ox, sc.rects[t.Key()].X), min(oy, sc.rects[t.Key()].Y)
	}
	for _, g := range doc.Groups {
		r := g.Rect()
		ox, oy = min(ox, r.X), min(oy, r.Y)
	}
	if in.Origin != nil {
		var err error
		if ox, err = checkCoord("origin", in.Origin.X); err != nil {
			return nil, nil, nil, err
		}
		if oy, err = checkCoord("origin", in.Origin.Y); err != nil {
			return nil, nil, nil, err
		}
	}
	if math.IsInf(ox, 1) {
		ox, oy = 80, 80
	}
	minX, minY, maxX := math.Inf(1), math.Inf(1), math.Inf(-1)
	for _, nd := range nodes {
		minX, minY = min(minX, nd.x-nd.w/2), min(minY, nd.y-nd.h/2)
		maxX = max(maxX, nd.x+nd.w/2)
	}
	for _, f := range frames {
		if f.w == 0 {
			continue
		}
		minX, minY = min(minX, f.x), min(minY, f.y)
		maxX = max(maxX, f.x+f.w)
	}
	shiftX, shiftY := ox-minX, oy-minY

	moves := map[string]*cardMove{}
	for _, nd := range nodes {
		moves[nd.id] = &cardMove{key: nd.id,
			x: math.Round(nd.x - nd.w/2 + shiftX), y: math.Round(nd.y - nd.h/2 + shiftY), by: "organic"}
	}
	// 뷰는 표를 읽는 것이라 흐름의 끝이다. 표들의 오른쪽에 한 줄로 세운다.
	vy := oy
	for _, v := range sc.views {
		r := sc.rects[v.Key()]
		moves[v.Key()] = &cardMove{key: v.Key(), view: true,
			x: math.Round(maxX + shiftX + organicGapOut), y: math.Round(vy), by: "organic"}
		vy += r.H + organicGapIn
	}

	plans := []*groupPlan{}
	notes := []noteMove{}
	movedNotes := map[string]bool{}
	for gi, og := range groups {
		f := frames[gi]
		if f.w == 0 {
			if og.plan.add {
				return nil, nil, nil, fmt.Errorf("그룹 %s 에 들어갈 표가 없습니다", og.plan.name)
			}
			continue // 표가 없는 기존 그룹은 건드리지 않는다.
		}
		nx, ny := math.Round(f.x+shiftX), math.Round(f.y+shiftY)
		og.plan.payload["x"], og.plan.payload["y"] = nx, ny
		og.plan.payload["w"], og.plan.payload["h"] = math.Round(f.w), math.Round(f.h)
		plans = append(plans, og.plan)
		if og.old == nil {
			continue
		}
		// 그룹 안에 붙여 둔 메모는 그룹을 따라간다(그룹 왼쪽 위 기준으로 같은 자리).
		old := og.old.Rect()
		for _, n := range doc.Notes {
			if movedNotes[n.ID] || !old.Contains(n.Rect()) {
				continue
			}
			movedNotes[n.ID] = true
			notes = append(notes, noteMove{id: n.ID, x: n.X + nx - old.X, y: n.Y + ny - old.Y})
		}
	}
	return plans, moves, notes, nil
}
