package erd

// 카드 기하. 서버가 좌표를 정하는 길(새 카드 자리 찾기, AI 배치 툴)이 모두 같은
// 크기 판정을 쓰도록 한 곳에 둔다. 판정이 두 벌이면 한쪽은 "겹치지 않는다"고 하고
// 다른 쪽은 그 자리에 카드를 놓는다.

// Rect는 캔버스 위의 사각형이다(왼쪽 위 기준, 화면 좌표와 같은 px).
type Rect struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	W float64 `json:"w"`
	H float64 `json:"h"`
}

// Overlaps는 두 사각형이 넓이를 가진 채로 겹치는지 본다(변이 닿기만 한 것은 아니다).
func (r Rect) Overlaps(o Rect) bool {
	return r.X < o.X+o.W && o.X < r.X+r.W &&
		r.Y < o.Y+o.H && o.Y < r.Y+r.H
}

// Contains는 o 가 이 사각형 안에 통째로 들어 있는지 본다.
func (r Rect) Contains(o Rect) bool {
	return o.X >= r.X && o.Y >= r.Y &&
		o.X+o.W <= r.X+r.W && o.Y+o.H <= r.Y+r.H
}

// CardSize는 표·뷰 카드 하나가 그려지는 크기다.
//
// 표는 화면과 같은 규칙으로 정확하다(폭은 Box.W, 높이는 컬럼 수와 접힘). 뷰는
// 정의문이 몇 줄로 접히는지를 그리는 쪽만 알아서 격자 한 칸으로 넉넉히 잡는다.
// 스키마에 없는 키(지워진 표의 좌표가 남은 것)도 격자 한 칸이다.
func (d *Document) CardSize(key string) (w, h float64) {
	return d.cardSizer()(key)
}

// cardSizer는 CardSize를 여러 번 부를 때 쓰는 판정기다. 표 목록을 한 번만 훑는다 —
// 카드마다 표를 찾으면 표가 많은 문서에서 자리 찾기가 표 수의 제곱이 된다.
func (d *Document) cardSizer() func(key string) (float64, float64) {
	cols := make(map[string]int, len(d.Schema.Tables))
	for _, t := range d.Schema.Tables {
		cols[t.Key()] = len(t.Columns)
	}
	views := make(map[string]bool, len(d.Schema.Views))
	for _, v := range d.Schema.Views {
		views[v.Key()] = true
	}
	return func(key string) (float64, float64) {
		b := d.Layout[key]
		w := cardDefW
		if b != nil && b.W > 0 {
			w = clampCardWidth(b.W)
		}
		switch {
		case b != nil && b.Collapsed:
			return w, cardHeadH
		case views[key]:
			return w, layoutStepY
		}
		n, ok := cols[key]
		if !ok {
			return w, layoutStepY
		}
		return w, CardHeight(n)
	}
}

// CardRects는 자리가 정해진 표·뷰 카드 전부의 사각형이다(키 → 사각형).
func (d *Document) CardRects() map[string]Rect {
	size := d.cardSizer()
	out := make(map[string]Rect, len(d.Layout))
	for key, b := range d.Layout {
		if b == nil {
			continue
		}
		w, h := size(key)
		out[key] = Rect{X: b.X, Y: b.Y, W: w, H: h}
	}
	return out
}

// Rect는 그룹이 차지하는 사각형이다. 크기가 비어 있으면 화면의 기본값(320×240)이다.
func (g *Group) Rect() Rect {
	w, h := g.W, g.H
	if w <= 0 {
		w = 320
	}
	if h <= 0 {
		h = 240
	}
	return Rect{X: g.X, Y: g.Y, W: w, H: h}
}

// Rect는 메모가 차지하는 사각형이다. 크기를 모르면 넉넉한 기본값으로 잡는다.
func (n *Note) Rect() Rect {
	w, h := n.W, n.H
	if w <= 0 {
		w = noteDefW
	}
	if h <= 0 {
		h = noteDefH
	}
	return Rect{X: n.X, Y: n.Y, W: w, H: h}
}

// MaxGroups는 문서 하나에 둘 수 있는 그룹 수다(group.add 가 거부하는 경계).
const MaxGroups = maxGroups
