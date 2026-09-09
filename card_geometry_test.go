package dbstudio

import (
	"io/fs"
	"regexp"
	"strconv"
	"testing"

	"dbstudio/internal/erd"
)

// 서버가 아는 카드 치수가 그리는 쪽과 같은지 본다.
//
// 왜 필요한가: 서버는 새 카드를 놓을 좌표를 정해야 하고, 그러려면 카드가 얼마나
// 크게 그려지는지 알아야 한다. 그 값은 그리는 쪽(erdcanvas.js)에 있다. 두 벌로
// 두는 것은 어쩔 수 없지만, 어긋나면 초기 배치에서 카드가 겹친다.
//
// **이 검사는 그 값을 손으로 적지 않는다.** 예전 검사가 그렇게 했고(erd 패키지 안에
// 0:38, 1:58 … 로), 그래서 JS 의 머리글 높이가 30에서 34로 바뀐 뒤에도 통과했다.
// 지키려던 값을 베껴 두면 검사가 아니라 사본이 된다 — 사본은 원본이 바뀌어도
// 아무 말을 하지 않는다.
func TestCardGeometryMatchesCanvas(t *testing.T) {
	web, err := WebFS()
	if err != nil {
		t.Fatalf("자산을 열지 못했습니다: %v", err)
	}
	src, err := fs.ReadFile(web, "js/core/erdcanvas.js")
	if err != nil {
		t.Fatalf("erdcanvas.js 를 읽지 못했습니다: %v", err)
	}

	js := jsNumbers(t, string(src), "HEAD_H", "ROW_H", "CARD_PAD")

	// erdcanvas.js 의 cardHeight(rows) = HEAD_H + rows * ROW_H + CARD_PAD
	for _, n := range []int{0, 1, 3, 14, 40} {
		want := js["HEAD_H"] + float64(n)*js["ROW_H"] + js["CARD_PAD"]
		if got := erd.CardHeight(n); got != want {
			t.Errorf("CardHeight(%d) = %.0f, erdcanvas.js 는 %.0f "+
				"(HEAD_H %.0f + %d*ROW_H %.0f + CARD_PAD %.0f)",
				n, got, want, js["HEAD_H"], n, js["ROW_H"], js["CARD_PAD"])
		}
	}
}

// jsNumbers는 `const NAME = 123;` 꼴에서 숫자를 꺼낸다(export 가 붙어도 읽는다).
func jsNumbers(t *testing.T, src string, names ...string) map[string]float64 {
	t.Helper()
	out := make(map[string]float64, len(names))
	for _, name := range names {
		re := regexp.MustCompile(`(?m)^(?:export\s+)?const\s+` + name +
			`\s*=\s*([0-9]+(?:\.[0-9]+)?)\s*;`)
		m := re.FindStringSubmatch(src)
		if m == nil {
			t.Fatalf("erdcanvas.js 에서 %s 를 찾지 못했습니다. "+
				"이름이 바뀌었다면 이 검사도 함께 고쳐야 합니다", name)
		}
		v, err := strconv.ParseFloat(m[1], 64)
		if err != nil {
			t.Fatalf("%s 의 값 %q 를 읽지 못했습니다: %v", name, m[1], err)
		}
		out[name] = v
	}
	return out
}
