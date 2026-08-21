package totp

// QR 코드 인코더.
//
// 왜 직접 구현하는가: otpauth URI를 손으로 옮겨 적게 하면 32글자 base32를 오타 없이
// 입력해야 하고, 그 실패는 "2FA는 원래 어렵다"는 인상으로 남는다. QR은 이 기능이
// 실제로 쓰이느냐를 가르는 부분이다. 반면 이 앱은 외부망 없이 도는 단일 바이너리라
// CDN의 QR 라이브러리를 쓸 수 없고, 새 의존성을 인증 경로에 넣고 싶지도 않다.
//
// 범위를 좁게 잡았다: **바이트 모드, 오류정정 L/M, 버전 1~10**만 만든다.
// otpauth URI는 길어야 200바이트 안쪽이므로 이 범위로 충분하고, 표를 좁게 두면
// 눈으로 검증할 수 있다. 범위를 벗어나면 에러를 돌려주고, 화면은 그때 수동 입력
// 안내로 내려간다 — QR이 없다고 등록 자체가 막히지는 않는다.
//
// 참고: ISO/IEC 18004. 배치·마스킹 규칙의 표현은 Project Nayuki의 공개 설명을 따랐다.

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// ErrTooLong은 지원 범위(버전 10) 안에 담을 수 없는 입력이다.
var ErrTooLong = errors.New("QR 코드로 만들기에 내용이 너무 깁니다")

// ecLevel은 오류정정 수준이다. 값은 형식 정보에 그대로 들어가는 2비트다.
type ecLevel int

const (
	ecM ecLevel = 0 // 00
	ecL ecLevel = 1 // 01
)

// totalCodewords[v]는 버전 v의 전체 코드워드 수(데이터 + 오류정정)다.
var totalCodewords = [11]int{0, 26, 44, 70, 100, 134, 172, 196, 242, 292, 346}

// blockSpec은 한 블록당 오류정정 코드워드 수와 블록 개수다.
//
// 데이터 코드워드를 블록에 나누는 규칙은 표가 필요 없다: 전체 데이터 코드워드를
// 블록 수로 나눈 몫이 짧은 블록의 길이이고, 나머지 개수만큼 한 칸 긴 블록이 된다.
// (표준의 그룹1/그룹2가 바로 이것이다.)
type blockSpec struct{ ecPerBlock, blocks int }

var blockSpecs = map[ecLevel][11]blockSpec{
	ecL: {
		{}, {7, 1}, {10, 1}, {15, 1}, {20, 1}, {26, 1},
		{18, 2}, {20, 2}, {24, 2}, {30, 2}, {18, 4},
	},
	ecM: {
		{}, {10, 1}, {16, 1}, {26, 1}, {18, 2}, {24, 2},
		{16, 4}, {18, 4}, {22, 4}, {22, 5}, {26, 5},
	},
}

// alignmentCenters[v]는 정렬 패턴 중심의 행·열 좌표다.
var alignmentCenters = [11][]int{
	nil, nil,
	{6, 18}, {6, 22}, {6, 26}, {6, 30}, {6, 34},
	{6, 22, 38}, {6, 24, 42}, {6, 26, 46}, {6, 28, 50},
}

const maxVersion = 10

// matrix는 모듈 격자다. dark는 검은 모듈, fixed는 함수 패턴(마스킹 대상이 아님)이다.
type matrix struct {
	size  int
	dark  []bool
	fixed []bool
}

func newMatrix(size int) *matrix {
	return &matrix{size: size, dark: make([]bool, size*size), fixed: make([]bool, size*size)}
}

func (m *matrix) at(row, col int) bool { return m.dark[row*m.size+col] }

func (m *matrix) set(row, col int, dark bool) {
	m.dark[row*m.size+col] = dark
}

func (m *matrix) setFunction(row, col int, dark bool) {
	if row < 0 || col < 0 || row >= m.size || col >= m.size {
		return
	}
	m.dark[row*m.size+col] = dark
	m.fixed[row*m.size+col] = true
}

func (m *matrix) isFixed(row, col int) bool { return m.fixed[row*m.size+col] }

// Encode는 문자열을 QR 모듈 격자로 만든다. 결과는 [행][열] 순서다.
func encodeQR(text string) ([][]bool, error) {
	data := []byte(text)

	version, level, ok := pickVersion(len(data))
	if !ok {
		return nil, ErrTooLong
	}

	codewords := buildCodewords(data, version, level)
	m := newMatrix(version*4 + 17)
	drawFunctionPatterns(m, version, level)
	placeCodewords(m, codewords)

	// 마스크는 8가지를 모두 적용해 보고 벌점이 가장 낮은 것을 고른다.
	// 규칙이 요구하는 절차이며, 목적은 판독기가 혼동할 무늬(파인더를 닮은 배열,
	// 큰 단색 덩어리)를 피하는 것이다.
	best, bestPenalty := 0, -1
	for mask := 0; mask < 8; mask++ {
		applyMask(m, mask)
		drawFormatInfo(m, level, mask)
		p := penalty(m)
		if bestPenalty < 0 || p < bestPenalty {
			best, bestPenalty = mask, p
		}
		applyMask(m, mask) // XOR이므로 같은 마스크를 한 번 더 적용하면 원상복구된다
	}
	applyMask(m, best)
	drawFormatInfo(m, level, best)

	out := make([][]bool, m.size)
	for r := 0; r < m.size; r++ {
		out[r] = m.dark[r*m.size : (r+1)*m.size]
	}
	return out, nil
}

// pickVersion은 내용을 담을 수 있는 가장 작은 버전을 고른다.
// 같은 버전이면 오류정정이 높은 M을 먼저 시도한다 — 화면의 QR은 카메라 각도와
// 화면 반사 때문에 종이보다 읽기 어렵고, 여분이 많을수록 한 번에 읽힌다.
func pickVersion(n int) (int, ecLevel, bool) {
	for v := 1; v <= maxVersion; v++ {
		for _, lvl := range []ecLevel{ecM, ecL} {
			if n <= byteCapacity(v, lvl) {
				return v, lvl, true
			}
		}
	}
	return 0, 0, false
}

func dataCodewords(v int, lvl ecLevel) int {
	spec := blockSpecs[lvl][v]
	return totalCodewords[v] - spec.ecPerBlock*spec.blocks
}

// charCountBits는 바이트 모드의 문자 개수 필드 폭이다(버전 10 이상은 16비트).
func charCountBits(v int) int {
	if v >= 10 {
		return 16
	}
	return 8
}

func byteCapacity(v int, lvl ecLevel) int {
	bits := dataCodewords(v, lvl)*8 - 4 - charCountBits(v)
	if bits < 0 {
		return 0
	}
	return bits / 8
}

// buildCodewords는 데이터 비트열을 만들고 블록으로 나눠 오류정정을 붙인 뒤
// 표준 순서(데이터 인터리브 → 오류정정 인터리브)로 늘어놓는다.
func buildCodewords(data []byte, v int, lvl ecLevel) []byte {
	total := dataCodewords(v, lvl)

	var bs bitBuffer
	bs.append(0b0100, 4) // 바이트 모드
	bs.append(uint32(len(data)), charCountBits(v))
	for _, b := range data {
		bs.append(uint32(b), 8)
	}
	// 종단자는 최대 4비트이며, 남은 공간이 그보다 적으면 그만큼만 넣는다.
	if pad := total*8 - bs.len(); pad > 0 {
		bs.append(0, min(4, pad))
	}
	bs.append(0, (8-bs.len()%8)%8) // 바이트 경계 맞추기
	// 남은 자리는 0xEC/0x11을 번갈아 채운다(표준이 정한 값이다).
	for pad := byte(0xEC); len(bs.bytes) < total; pad ^= 0xEC ^ 0x11 {
		bs.bytes = append(bs.bytes, pad)
	}

	spec := blockSpecs[lvl][v]
	shortLen := total / spec.blocks
	longCount := total % spec.blocks // 한 칸 더 긴 블록의 개수

	gen := rsGenerator(spec.ecPerBlock)
	blocks := make([][]byte, spec.blocks)
	ecBlocks := make([][]byte, spec.blocks)
	pos := 0
	for i := 0; i < spec.blocks; i++ {
		n := shortLen
		if i >= spec.blocks-longCount {
			n++
		}
		blocks[i] = bs.bytes[pos : pos+n]
		pos += n
		ecBlocks[i] = rsRemainder(blocks[i], gen)
	}

	out := make([]byte, 0, totalCodewords[v])
	for i := 0; i < shortLen+1; i++ {
		for _, b := range blocks {
			if i < len(b) {
				out = append(out, b[i])
			}
		}
	}
	for i := 0; i < spec.ecPerBlock; i++ {
		for _, b := range ecBlocks {
			out = append(out, b[i])
		}
	}
	return out
}

// ---------- 비트 버퍼 ----------

type bitBuffer struct {
	bytes []byte
	bits  int // 마지막 바이트에서 사용 중인 비트 수
}

func (b *bitBuffer) len() int { return len(b.bytes)*8 - (8-b.bits)%8 }

func (b *bitBuffer) append(value uint32, n int) {
	for i := n - 1; i >= 0; i-- {
		bit := (value>>uint(i))&1 == 1
		if b.bits%8 == 0 {
			b.bytes = append(b.bytes, 0)
		}
		if bit {
			b.bytes[len(b.bytes)-1] |= 1 << uint(7-b.bits%8)
		}
		b.bits = (b.bits + 1) % 8
	}
}

// ---------- 리드-솔로몬 (GF(2^8), 원시 다항식 0x11D) ----------

func gfMul(a, b byte) byte {
	var p byte
	for i := 0; i < 8; i++ {
		if b&1 != 0 {
			p ^= a
		}
		hi := a&0x80 != 0
		a <<= 1
		if hi {
			a ^= 0x1D
		}
		b >>= 1
	}
	return p
}

// rsGenerator는 차수 degree의 생성 다항식 계수를 내림차순으로 돌려준다.
// 최고차항 계수는 항상 1이므로 담지 않는다 — 담으면 나눗셈 루프가 매번 그것을 건너뛰어야 한다.
func rsGenerator(degree int) []byte {
	gen := make([]byte, degree)
	gen[degree-1] = 1 // 상수항 1에서 시작한다
	root := byte(1)
	for i := 0; i < degree; i++ {
		// gen = gen * (x - α^i). GF(2)에서 뺄셈은 XOR이다.
		for j := 0; j < degree; j++ {
			gen[j] = gfMul(gen[j], root)
			if j+1 < degree {
				gen[j] ^= gen[j+1]
			}
		}
		root = gfMul(root, 2)
	}
	return gen
}

// rsRemainder는 데이터에 대한 오류정정 코드워드를 만든다.
func rsRemainder(data, gen []byte) []byte {
	rem := make([]byte, len(gen))
	for _, b := range data {
		factor := b ^ rem[0]
		copy(rem, rem[1:])
		rem[len(rem)-1] = 0
		for i, g := range gen {
			rem[i] ^= gfMul(g, factor)
		}
	}
	return rem
}

// ---------- 함수 패턴 ----------

func drawFunctionPatterns(m *matrix, v int, lvl ecLevel) {
	size := m.size

	// 타이밍 패턴: 6번 행·열의 명암 교대. 판독기가 모듈 간격을 재는 기준선이다.
	for i := 0; i < size; i++ {
		m.setFunction(6, i, i%2 == 0)
		m.setFunction(i, 6, i%2 == 0)
	}

	// 파인더 세 개와 그 분리자.
	for _, p := range [][2]int{{0, 0}, {0, size - 7}, {size - 7, 0}} {
		drawFinder(m, p[0], p[1])
	}

	// 정렬 패턴. 파인더와 겹치는 세 모서리는 건너뛴다.
	centers := alignmentCenters[v]
	for _, r := range centers {
		for _, c := range centers {
			if (r == 6 && c == 6) || (r == 6 && c == size-7) || (r == size-7 && c == 6) {
				continue
			}
			drawAlignment(m, r, c)
		}
	}

	// 형식 정보 자리를 예약해 둔다. 값은 마스크가 정해진 뒤에 쓴다.
	drawFormatInfo(m, lvl, 0)

	// 버전 7 이상은 버전 정보 블록을 둘 갖는다.
	if v >= 7 {
		drawVersionInfo(m, v)
	}
}

func drawFinder(m *matrix, row, col int) {
	// 8x8 영역(파인더 7x7 + 분리자 1줄)을 한 번에 그린다.
	for dr := -1; dr <= 7; dr++ {
		for dc := -1; dc <= 7; dc++ {
			r, c := row+dr, col+dc
			if r < 0 || c < 0 || r >= m.size || c >= m.size {
				continue
			}
			dist := max(abs(dr-3), abs(dc-3))
			m.setFunction(r, c, dist != 2 && dist <= 3)
		}
	}
}

func drawAlignment(m *matrix, row, col int) {
	for dr := -2; dr <= 2; dr++ {
		for dc := -2; dc <= 2; dc++ {
			m.setFunction(row+dr, col+dc, max(abs(dr), abs(dc)) != 1)
		}
	}
}

// drawFormatInfo는 오류정정 수준과 마스크 번호를 두 곳에 적는다.
//
// 두 벌을 두는 이유는 표준이 정한 것이다: 한쪽 모서리가 훼손되어도 나머지로 읽는다.
func drawFormatInfo(m *matrix, lvl ecLevel, mask int) {
	data := int(lvl)<<3 | mask
	rem := data
	for i := 0; i < 10; i++ {
		rem = (rem << 1) ^ ((rem >> 9) * 0x537)
	}
	bits := (data<<10 | rem) ^ 0x5412

	get := func(i int) bool { return (bits>>uint(i))&1 == 1 }

	// 왼쪽 위 모서리.
	for i := 0; i <= 5; i++ {
		m.setFunction(i, 8, get(i))
	}
	m.setFunction(7, 8, get(6))
	m.setFunction(8, 8, get(7))
	m.setFunction(8, 7, get(8))
	for i := 9; i < 15; i++ {
		m.setFunction(8, 14-i, get(i))
	}

	// 오른쪽 위 + 왼쪽 아래.
	size := m.size
	for i := 0; i < 8; i++ {
		m.setFunction(8, size-1-i, get(i))
	}
	for i := 8; i < 15; i++ {
		m.setFunction(size-15+i, 8, get(i))
	}
	// 항상 검은 모듈. 규격이 고정한 자리다.
	m.setFunction(size-8, 8, true)
}

func drawVersionInfo(m *matrix, v int) {
	rem := v
	for i := 0; i < 12; i++ {
		rem = (rem << 1) ^ ((rem >> 11) * 0x1F25)
	}
	bits := v<<12 | rem

	for i := 0; i < 18; i++ {
		bit := (bits>>uint(i))&1 == 1
		a := m.size - 11 + i%3
		b := i / 3
		m.setFunction(b, a, bit)
		m.setFunction(a, b, bit)
	}
}

// ---------- 데이터 배치 ----------

// placeCodewords는 오른쪽 아래에서 시작해 두 열씩 지그재그로 올라가며 채운다.
func placeCodewords(m *matrix, data []byte) {
	size := m.size
	i := 0 // 비트 인덱스
	for right := size - 1; right >= 1; right -= 2 {
		if right == 6 {
			right = 5 // 6번 열은 타이밍 패턴이므로 건너뛴다
		}
		for vert := 0; vert < size; vert++ {
			for j := 0; j < 2; j++ {
				col := right - j
				upward := ((right + 1) & 2) == 0
				row := vert
				if upward {
					row = size - 1 - vert
				}
				if m.isFixed(row, col) {
					continue
				}
				// 비트가 떨어지면 0(밝음)으로 둔다. 표준의 나머지 비트(remainder bits)가
				// 정확히 이것이며, 마스킹 대상에는 그대로 포함된다.
				dark := false
				if i < len(data)*8 {
					dark = (data[i>>3]>>uint(7-i&7))&1 == 1
				}
				m.set(row, col, dark)
				i++
			}
		}
	}
}

func applyMask(m *matrix, mask int) {
	for row := 0; row < m.size; row++ {
		for col := 0; col < m.size; col++ {
			if m.isFixed(row, col) {
				continue
			}
			if maskBit(mask, row, col) {
				m.set(row, col, !m.at(row, col))
			}
		}
	}
}

func maskBit(mask, row, col int) bool {
	switch mask {
	case 0:
		return (row+col)%2 == 0
	case 1:
		return row%2 == 0
	case 2:
		return col%3 == 0
	case 3:
		return (row+col)%3 == 0
	case 4:
		return (row/2+col/3)%2 == 0
	case 5:
		return row*col%2+row*col%3 == 0
	case 6:
		return (row*col%2+row*col%3)%2 == 0
	default:
		return ((row+col)%2+row*col%3)%2 == 0
	}
}

// penalty는 표준의 네 가지 벌점 규칙을 합산한다. 낮을수록 읽기 좋은 무늬다.
func penalty(m *matrix) int {
	size := m.size
	total := 0

	// 규칙 1: 같은 색이 5개 이상 이어지는 구간.
	line := func(get func(i int) bool) {
		run, prev := 1, get(0)
		for i := 1; i < size; i++ {
			cur := get(i)
			if cur == prev {
				run++
				continue
			}
			if run >= 5 {
				total += 3 + (run - 5)
			}
			run, prev = 1, cur
		}
		if run >= 5 {
			total += 3 + (run - 5)
		}
	}
	for i := 0; i < size; i++ {
		row, col := i, i
		line(func(j int) bool { return m.at(row, j) })
		line(func(j int) bool { return m.at(j, col) })
	}

	// 규칙 2: 같은 색 2x2 덩어리.
	for r := 0; r < size-1; r++ {
		for c := 0; c < size-1; c++ {
			v := m.at(r, c)
			if v == m.at(r, c+1) && v == m.at(r+1, c) && v == m.at(r+1, c+1) {
				total += 3
			}
		}
	}

	// 규칙 3: 파인더를 닮은 1:1:3:1:1 무늬(양옆 여백 포함).
	pattern := []bool{true, false, true, true, true, false, true, false, false, false, false}
	matches := func(get func(i int) bool, start int, rev bool) bool {
		for k := 0; k < len(pattern); k++ {
			idx := start + k
			if rev {
				idx = start + len(pattern) - 1 - k
			}
			if get(idx) != pattern[k] {
				return false
			}
		}
		return true
	}
	for i := 0; i < size; i++ {
		row, col := i, i
		rowGet := func(j int) bool { return m.at(row, j) }
		colGet := func(j int) bool { return m.at(j, col) }
		for s := 0; s+len(pattern) <= size; s++ {
			if matches(rowGet, s, false) || matches(rowGet, s, true) {
				total += 40
			}
			if matches(colGet, s, false) || matches(colGet, s, true) {
				total += 40
			}
		}
	}

	// 규칙 4: 검은 모듈 비율이 50%에서 멀어질수록 벌점.
	// (45-5k)% ≤ 비율 ≤ (55+5k)% 를 만족하는 가장 작은 k를 구해 10을 곱한다.
	dark := 0
	for _, v := range m.dark {
		if v {
			dark++
		}
	}
	n := len(m.dark)
	k := (abs(dark*20-n*10)+n-1)/n - 1
	total += k * 10
	return total
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// ---------- 출력 ----------

// QRSVG는 내용을 QR 코드 SVG 문서로 만든다.
//
// 색을 지정하지 않고 currentColor를 쓰는 이유: 이 앱은 라이트/다크 테마를 오간다.
// 검정으로 굽어 두면 다크 테마에서 배경과 붙어 읽히지 않는다. 대신 밝은 모듈에는
// 흰 바탕을 깔아 준다 — QR은 명암 반전을 견디지 못하는 판독기가 아직 많다.
func QRSVG(text string) (string, error) {
	mods, err := encodeQR(text)
	if err != nil {
		return "", err
	}
	const quiet = 4 // 규격이 요구하는 여백. 이것이 없으면 카메라가 코드 경계를 못 찾는다.
	size := len(mods) + quiet*2

	var path strings.Builder
	for r, row := range mods {
		for c := 0; c < len(row); c++ {
			if !row[c] {
				continue
			}
			// 가로로 이어진 검은 모듈을 한 사각형으로 묶어 문서 크기를 줄인다.
			start := c
			for c+1 < len(row) && row[c+1] {
				c++
			}
			fmt.Fprintf(&path, "M%d %dh%dv1h-%dz", start+quiet, r+quiet, c-start+1, c-start+1)
		}
	}

	return fmt.Sprintf(
		`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" shape-rendering="crispEdges">`+
			`<rect width="%d" height="%d" fill="#ffffff"/><path fill="#000000" d="%s"/></svg>`,
		size, size, size, size, path.String()), nil
}

// QRDataURI는 SVG를 data: URI로 감싼다. <img src>에 그대로 넣을 수 있고,
// 문자열을 innerHTML로 붙이지 않아도 되므로 프론트엔드에서 다룰 위험이 없다.
func QRDataURI(text string) (string, error) {
	svg, err := QRSVG(text)
	if err != nil {
		return "", err
	}
	return "data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString([]byte(svg)), nil
}
