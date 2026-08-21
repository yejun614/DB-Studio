package totp

import (
	"strings"
	"testing"
)

// alphaPow는 GF(256)의 α^e를 구한다. 표준 문서가 생성 다항식 계수를 α의 지수로
// 적어 두므로, 비교하려면 이 변환이 필요하다.
func alphaPow(e int) byte {
	v := byte(1)
	for i := 0; i < e; i++ {
		v = gfMul(v, 2)
	}
	return v
}

// 생성 다항식은 표준(ISO/IEC 18004 부속서 A)이 값을 못 박아 둔 것이라
// 우리 GF 산술이 맞는지 확인하는 가장 확실한 기준이 된다.
func TestRSGeneratorMatchesSpec(t *testing.T) {
	cases := map[int][]int{
		// 최고차항(α^0)을 뺀 나머지 계수의 α 지수, 내림차순.
		7:  {87, 229, 146, 149, 238, 102, 21},
		10: {251, 67, 46, 61, 118, 70, 64, 94, 32, 45},
	}
	for degree, exps := range cases {
		got := rsGenerator(degree)
		if len(got) != len(exps) {
			t.Fatalf("degree %d: 길이 %d, want %d", degree, len(got), len(exps))
		}
		for i, e := range exps {
			if want := alphaPow(e); got[i] != want {
				t.Errorf("degree %d 계수 %d: got %d, want α^%d = %d", degree, i, got[i], e, want)
			}
		}
	}
}

// 형식 정보와 버전 정보는 BCH 부호이고, 표준이 완성된 비트열을 표로 실어 두었다.
// 여기서 어긋나면 판독기가 오류정정 수준을 잘못 읽어 아무것도 못 읽는다.
func TestFormatAndVersionBits(t *testing.T) {
	// (오류정정 M, 마스크 0)의 형식 비트는 0x5412이다. M의 2비트가 00,
	// 마스크가 000이므로 BCH 나머지도 0이고 결국 XOR 마스크 자체가 남는다.
	m := newMatrix(21)
	drawFormatInfo(m, ecM, 0)
	if got := readFormatBits(m); got != 0x5412 {
		t.Errorf("형식 비트(M, 마스크0) = %#x, want 0x5412", got)
	}
	// (오류정정 L, 마스크 0)은 0x77C4이다.
	m = newMatrix(21)
	drawFormatInfo(m, ecL, 0)
	if got := readFormatBits(m); got != 0x77C4 {
		t.Errorf("형식 비트(L, 마스크0) = %#x, want 0x77C4", got)
	}

	// 버전 7의 버전 정보는 0x07C94이다.
	size := 7*4 + 17
	m = newMatrix(size)
	drawVersionInfo(m, 7)
	var bits int
	for i := 17; i >= 0; i-- {
		bits <<= 1
		if m.at(i/3, size-11+i%3) {
			bits |= 1
		}
	}
	if bits != 0x07C94 {
		t.Errorf("버전 비트(7) = %#x, want 0x07C94", bits)
	}
}

// readFormatBits는 왼쪽 위 모서리에 적힌 15비트를 되읽는다.
func readFormatBits(m *matrix) int {
	var bits int
	get := func(row, col int) int {
		if m.at(row, col) {
			return 1
		}
		return 0
	}
	for i := 14; i >= 9; i-- {
		bits = bits<<1 | get(8, 14-i)
	}
	bits = bits<<1 | get(8, 7)
	bits = bits<<1 | get(8, 8)
	bits = bits<<1 | get(7, 8)
	for i := 5; i >= 0; i-- {
		bits = bits<<1 | get(i, 8)
	}
	return bits
}

// 용량 표가 서로 모순되지 않는지 본다. 이 표는 손으로 옮겨 적은 것이라
// 한 칸만 틀려도 특정 길이의 입력에서만 조용히 깨진다.
func TestCapacityTable(t *testing.T) {
	// 널리 알려진 바이트 모드 용량과 대조한다.
	cases := []struct {
		version int
		level   ecLevel
		want    int
	}{
		{1, ecL, 17}, {1, ecM, 14},
		{5, ecM, 84}, {7, ecM, 122},
		{9, ecL, 230}, {10, ecM, 213}, {10, ecL, 271},
	}
	for _, tc := range cases {
		if got := byteCapacity(tc.version, tc.level); got != tc.want {
			t.Errorf("v%d 레벨%d 용량 = %d, want %d", tc.version, tc.level, got, tc.want)
		}
	}
	// 전체 코드워드 = 데이터 + 오류정정이 모든 버전에서 성립해야 한다.
	for lvl, specs := range blockSpecs {
		for v := 1; v <= maxVersion; v++ {
			s := specs[v]
			if dataCodewords(v, lvl)+s.ecPerBlock*s.blocks != totalCodewords[v] {
				t.Errorf("v%d 레벨%d: 코드워드 합이 %d와 맞지 않습니다", v, lvl, totalCodewords[v])
			}
		}
	}
}

// 구조 검사: 파인더·타이밍·크기가 규격대로인지 본다.
func TestEncodeStructure(t *testing.T) {
	mods, err := encodeQR("otpauth://totp/DB%20Studio:alice?secret=" +
		"JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP&issuer=DB+Studio&algorithm=SHA1&digits=6&period=30")
	if err != nil {
		t.Fatal(err)
	}
	size := len(mods)
	if (size-17)%4 != 0 || size < 21 || size > 57 {
		t.Fatalf("격자 크기 %d는 버전 1~10의 크기가 아닙니다", size)
	}
	for _, row := range mods {
		if len(row) != size {
			t.Fatalf("정사각형이 아닙니다: %d != %d", len(row), size)
		}
	}

	// 파인더 세 곳: 가운데 3x3이 검고 그 둘레 한 겹이 밝아야 한다.
	for _, p := range [][2]int{{0, 0}, {0, size - 7}, {size - 7, 0}} {
		for dr := 0; dr < 7; dr++ {
			for dc := 0; dc < 7; dc++ {
				dist := max(abs(dr-3), abs(dc-3))
				want := dist != 2 && dist <= 3
				if got := mods[p[0]+dr][p[1]+dc]; got != want {
					t.Fatalf("파인더(%d,%d)의 (%d,%d)가 %v, want %v", p[0], p[1], dr, dc, got, want)
				}
			}
		}
	}

	// 타이밍 패턴: 6번 행·열이 명암 교대.
	for i := 8; i < size-8; i++ {
		if mods[6][i] != (i%2 == 0) || mods[i][6] != (i%2 == 0) {
			t.Fatalf("타이밍 패턴이 %d에서 어긋납니다", i)
		}
	}
}

// 리드-솔로몬 부호어는 생성 다항식의 근 α^0…α^(n-1)에서 값이 0이어야 한다.
// 이 검사는 인코딩과 다른 계산(다항식 평가)이므로 rsRemainder의 오류를 잡는다.
func TestRSSyndromesAreZero(t *testing.T) {
	data := []byte("otpauth://totp/DB Studio:alice?secret=JBSWY3DPEHPK3PXP")
	for _, degree := range []int{7, 10, 16, 18, 22, 26} {
		gen := rsGenerator(degree)
		ec := rsRemainder(data, gen)
		codeword := append(append([]byte{}, data...), ec...)
		for i := 0; i < degree; i++ {
			root := alphaPow(i)
			var acc byte
			for _, c := range codeword { // 호너 법
				acc = gfMul(acc, root) ^ c
			}
			if acc != 0 {
				t.Fatalf("degree %d: α^%d 에서 신드롬이 %d (0이어야 합니다)", degree, i, acc)
			}
		}
	}
}

func TestQRSVG(t *testing.T) {
	uri := URI("DB Studio", "alice", "JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP", DefaultParams())
	svg, err := QRSVG(uri)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(svg, "<svg ") || !strings.HasSuffix(svg, "</svg>") {
		t.Fatalf("SVG 형식이 아닙니다: %.60s", svg)
	}
	if !strings.Contains(svg, "<path") {
		t.Error("어두운 모듈이 하나도 그려지지 않았습니다")
	}
	uri2, err := QRDataURI(uri)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(uri2, "data:image/svg+xml;base64,") {
		t.Errorf("data URI 형식이 아닙니다: %.40s", uri2)
	}
}

func TestQRTooLong(t *testing.T) {
	if _, err := QRSVG(strings.Repeat("A", 400)); err == nil {
		t.Error("지원 범위를 넘는 입력을 받아들였습니다")
	}
}
