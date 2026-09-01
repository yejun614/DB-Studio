package erd

import (
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

	// 작은 표들만 있으면 예전 격자와 같은 간격을 지킨다. 배치가 이유 없이
	// 흐트러지면 쓰던 사람에게는 고장으로 보인다.
	small := &schema.Schema{Tables: []*schema.Table{
		mk("a", 2), mk("b", 2), mk("c", 2), mk("d", 2), mk("e", 2),
	}}
	got := AutoLayout(small)
	first := got[small.Tables[0].Key()]
	fifth := got[small.Tables[4].Key()]
	if fifth.Y-first.Y != layoutStepY {
		t.Errorf("작은 표의 세로 간격 = %.0f, 기대 %.0f", fifth.Y-first.Y, layoutStepY)
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
