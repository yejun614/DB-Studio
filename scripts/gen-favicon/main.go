// Command gen-favicon은 파비콘 자산을 생성한다.
//
//	go run ./scripts/gen-favicon
//
// 왜 생성기를 두는가: SVG와 래스터(ICO/PNG)를 따로 손으로 만들면 도형이 조금씩
// 어긋나고, 한쪽만 고치는 일이 반드시 생긴다. 기하 정의를 한 곳에 두고 두 형식을
// 같은 숫자에서 뽑아내면 그 문제가 사라진다.
//
// 외부 도구(ImageMagick 등)를 쓰지 않는 이유: 이 프로젝트는 표준 라이브러리만으로
// 빌드되는 단일 바이너리를 전제로 한다. 아이콘 하나 때문에 빌드 환경에 의존성을
// 추가하지 않는다.
package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"path/filepath"
)

// 기하는 100×100 좌표계에서 정의한다. 사이드바 브랜드 아이콘(원통형 DB)과 같은 형태다.
//
// 선(stroke) 대신 채운 실루엣을 쓰는 이유: 16px에서 1.8px 선은 뭉개져서 무엇인지
// 알 수 없게 된다. 파비콘은 형태만 남는 크기이므로 면으로 그린다.
const (
	size = 100.0

	radius = 22.0 // 배경 둥근 사각형

	cx = 50.0
	rx = 30.0 // 원통 반지름
	ry = 10.0 // 타원 세로 반지름 (원근)
	// 위/아래 타원 중심. 16px에서도 형태가 남도록 배경 대비 크게 잡는다.
	topY = 26.0
	botY = 72.0
	// 디스크 구분선(배경색으로 잘라낸다). 16px에서 100:6 = 0.96px이므로
	// 이보다 얇으면 서브픽셀이 되어 뭉개진다.
	bandW  = 6.0
	band1Y = 41.0
	band2Y = 57.0
)

// 색: 밝은 탭 바에서는 어두운 배경 + 밝은 글리프.
// 래스터는 하나뿐이므로 어느 탭 바에서도 읽히는 이 조합을 쓴다.
var (
	bgLight = color.NRGBA{0x18, 0x18, 0x1b, 0xff} // zinc-950
	fgLight = color.NRGBA{0xfa, 0xfa, 0xfa, 0xff} // zinc-50
	// SVG는 prefers-color-scheme으로 반전한다(다크 탭 바에서 더 자연스럽다).
	bgDark = "#fafafa"
	fgDark = "#18181b"
)

func main() {
	root, err := repoRoot()
	if err != nil {
		fail(err)
	}
	webDir := filepath.Join(root, "web")

	// 1) SVG — 주 파비콘. 어느 크기에서도 선명하고 다크 모드에 반응한다.
	if err := os.WriteFile(filepath.Join(webDir, "favicon.svg"), []byte(svgSource()), 0o644); err != nil {
		fail(err)
	}
	fmt.Println("wrote web/favicon.svg")

	// 2) ICO — 브라우저가 /favicon.ico를 자동으로 요청한다. 파일이 없으면
	//    SPA 폴백이 index.html을 돌려주고, 브라우저는 그것을 아이콘으로 해석하려 한다.
	ico, err := buildICO(16, 32, 48)
	if err != nil {
		fail(err)
	}
	if err := os.WriteFile(filepath.Join(webDir, "favicon.ico"), ico, 0o644); err != nil {
		fail(err)
	}
	fmt.Printf("wrote web/favicon.ico (%d bytes, 16/32/48)\n", len(ico))

	// 3) apple-touch-icon — iOS 홈 화면.
	//    모서리를 직각으로 둔다: iOS가 스스로 둥근 마스크를 씌우므로
	//    여기서 둥글게 만들면 두 번 잘려 모서리에 흰 틈이 생긴다.
	if err := writePNG(filepath.Join(webDir, "apple-touch-icon.png"), render(180, 0)); err != nil {
		fail(err)
	}
	fmt.Println("wrote web/apple-touch-icon.png (180x180)")
}

// render는 지정한 픽셀 크기로 아이콘을 그린다.
//
// 안티앨리어싱은 4×4 슈퍼샘플링으로 만든다. 표준 라이브러리에는 벡터 래스터라이저가
// 없지만, 도형이 원과 사각형뿐이라 점마다 내부/외부를 판정하면 충분하다.
func render(px int, corner float64) *image.NRGBA {
	const ss = 4
	img := image.NewNRGBA(image.Rect(0, 0, px, px))
	scale := size / float64(px)

	for y := range px {
		for x := range px {
			var bgHits, fgHits int
			for sy := range ss {
				for sx := range ss {
					// 픽셀 안에서 균등하게 샘플링한다.
					ux := (float64(x) + (float64(sx)+0.5)/ss) * scale
					uy := (float64(y) + (float64(sy)+0.5)/ss) * scale
					if !inRoundedRect(ux, uy, corner) {
						continue
					}
					bgHits++
					if inGlyph(ux, uy) && !inBand(ux, uy) {
						fgHits++
					}
				}
			}
			total := float64(ss * ss)
			if bgHits == 0 {
				continue
			}
			// 배경과 글리프를 커버리지 비율로 섞고, 알파는 둥근 사각형 커버리지로 둔다.
			alpha := float64(bgHits) / total
			fgRatio := float64(fgHits) / float64(bgHits)
			img.SetNRGBA(x, y, mix(bgLight, fgLight, fgRatio, alpha))
		}
	}
	return img
}

func mix(bg, fg color.NRGBA, fgRatio, alpha float64) color.NRGBA {
	lerp := func(a, b uint8) uint8 {
		return uint8(math.Round(float64(a)*(1-fgRatio) + float64(b)*fgRatio))
	}
	return color.NRGBA{
		R: lerp(bg.R, fg.R), G: lerp(bg.G, fg.G), B: lerp(bg.B, fg.B),
		A: uint8(math.Round(alpha * 255)),
	}
}

// inRoundedRect는 배경 사각형 내부인지 판정한다. corner=0이면 직각 모서리다.
func inRoundedRect(x, y, corner float64) bool {
	if x < 0 || x > size || y < 0 || y > size {
		return false
	}
	if corner <= 0 {
		return true
	}
	// 각 모서리에서만 원 판정을 하고 나머지는 사각형으로 처리한다.
	dx := math.Max(corner-x, x-(size-corner))
	dy := math.Max(corner-y, y-(size-corner))
	if dx <= 0 || dy <= 0 {
		return true
	}
	return dx*dx+dy*dy <= corner*corner
}

// inGlyph는 원통(위 타원 + 몸통 + 아래 타원) 내부인지 판정한다.
func inGlyph(x, y float64) bool {
	if inEllipse(x, y, cx, topY, rx, ry) || inEllipse(x, y, cx, botY, rx, ry) {
		return true
	}
	return x >= cx-rx && x <= cx+rx && y >= topY && y <= botY
}

// inBand는 디스크를 구분하는 곡선(배경색으로 잘라낼 영역) 내부인지 판정한다.
//
// 직선이 아니라 타원의 아래쪽 호를 따라가는 곡선이어야 원통의 원근과 맞는다.
func inBand(x, y float64) bool {
	return onLowerArc(x, y, band1Y) || onLowerArc(x, y, band2Y)
}

func onLowerArc(x, y, centerY float64) bool {
	t := (x - cx) / rx
	if t < -1 || t > 1 {
		return false
	}
	arcY := centerY + ry*math.Sqrt(1-t*t)
	return math.Abs(y-arcY) <= bandW/2
}

func inEllipse(x, y, ecx, ecy, erx, ery float64) bool {
	dx := (x - ecx) / erx
	dy := (y - ecy) / ery
	return dx*dx+dy*dy <= 1
}

// svgSource는 래스터와 같은 기하로 SVG를 만든다.
//
// 잘라내는 선을 배경색 stroke로 그리는 이유: mask나 clip-path를 쓰면 다크 모드 반전에서
// 색을 두 곳에서 관리해야 한다. 배경색으로 덧그리면 색 정의가 두 개로 끝난다.
func svgSource() string {
	arc := func(centerY float64) string {
		// 아래쪽 호: 왼쪽 끝 → 오른쪽 끝
		return fmt.Sprintf("M %.1f %.1f A %.1f %.1f 0 0 0 %.1f %.1f",
			cx-rx, centerY, rx, ry, cx+rx, centerY)
	}
	return fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 100 100" width="100" height="100">
  <!-- 생성 파일입니다. 수정하지 말고 scripts/gen-favicon 을 고쳐 다시 생성하세요. -->
  <style>
    :root { --bg: %s; --fg: %s; }
    @media (prefers-color-scheme: dark) { :root { --bg: %s; --fg: %s; } }
  </style>
  <rect width="100" height="100" rx="%.0f" fill="var(--bg)"/>
  <g fill="var(--fg)">
    <ellipse cx="%.1f" cy="%.1f" rx="%.1f" ry="%.1f"/>
    <path d="M %.1f %.1f V %.1f A %.1f %.1f 0 0 0 %.1f %.1f V %.1f Z"/>
  </g>
  <g fill="none" stroke="var(--bg)" stroke-width="%.1f" stroke-linecap="butt">
    <path d="%s"/>
    <path d="%s"/>
  </g>
</svg>
`,
		hex(bgLight), hex(fgLight), bgDark, fgDark,
		radius,
		cx, topY, rx, ry,
		cx-rx, topY, botY, rx, ry, cx+rx, botY, topY,
		bandW,
		arc(band1Y), arc(band2Y))
}

func hex(c color.NRGBA) string { return fmt.Sprintf("#%02x%02x%02x", c.R, c.G, c.B) }

// buildICO는 여러 크기의 PNG를 하나의 ICO로 묶는다.
//
// ICO 안에 PNG를 넣는 것은 Vista 이후 표준이며 현재 브라우저는 모두 지원한다.
// 여러 크기를 담는 이유: 브라우저는 탭(16), 즐겨찾기(32), 작업 표시줄(48)에서
// 서로 다른 크기를 쓰고, 하나만 넣으면 축소 품질이 브라우저에 맡겨진다.
func buildICO(sizes ...int) ([]byte, error) {
	type entry struct {
		size int
		data []byte
	}
	entries := make([]entry, 0, len(sizes))
	for _, s := range sizes {
		var buf bytes.Buffer
		if err := png.Encode(&buf, render(s, radius)); err != nil {
			return nil, err
		}
		entries = append(entries, entry{size: s, data: buf.Bytes()})
	}

	var out bytes.Buffer
	// ICONDIR
	_ = binary.Write(&out, binary.LittleEndian, uint16(0)) // reserved
	_ = binary.Write(&out, binary.LittleEndian, uint16(1)) // type: icon
	_ = binary.Write(&out, binary.LittleEndian, uint16(len(entries)))

	offset := 6 + 16*len(entries)
	for _, e := range entries {
		dim := byte(e.size)
		if e.size >= 256 {
			dim = 0 // ICO에서 0은 256을 뜻한다
		}
		out.WriteByte(dim)                                               // width
		out.WriteByte(dim)                                               // height
		out.WriteByte(0)                                                 // 팔레트 색 수 (트루컬러는 0)
		out.WriteByte(0)                                                 // reserved
		_ = binary.Write(&out, binary.LittleEndian, uint16(1))           // color planes
		_ = binary.Write(&out, binary.LittleEndian, uint16(32))          // bits per pixel
		_ = binary.Write(&out, binary.LittleEndian, uint32(len(e.data))) // 데이터 크기
		_ = binary.Write(&out, binary.LittleEndian, uint32(offset))      // 데이터 오프셋
		offset += len(e.data)
	}
	for _, e := range entries {
		out.Write(e.data)
	}
	return out.Bytes(), nil
}

func writePNG(path string, img image.Image) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return png.Encode(f, img)
}

// repoRoot는 go.mod가 있는 디렉터리를 찾는다. 어디서 실행해도 같은 곳에 쓰기 위해서다.
func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("go.mod를 찾지 못했습니다 (리포지토리 안에서 실행하세요)")
		}
		dir = parent
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "실패:", err)
	os.Exit(1)
}
