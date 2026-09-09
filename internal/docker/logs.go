package docker

import (
	"bufio"
	"context"
	"encoding/binary"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// 컨테이너 로그.
//
// ── 프레임을 풀어야 하는 이유 ───────────────────────────────────────
// 도커의 로그 스트림은 그냥 바이트가 아니다. 8바이트 머리말이 붙은 프레임의
// 연속이고, 그 머리말이 이 조각이 stdout 인지 stderr 인지를 말한다.
//
//	[0]    스트림 종류 (1=stdout, 2=stderr)
//	[1:4]  0
//	[4:8]  길이 (빅 엔디언)
//
// **단, Tty 로 띄운 컨테이너는 머리말이 없다.** 터미널은 두 스트림을 하나로
// 합치기 때문이다. 그래서 읽기 전에 그 컨테이너의 Tty 값을 알아야 한다 —
// 모르고 프레임으로 읽으면 로그의 첫 8바이트가 잘려 나가고, 그 증상은
// "로그 첫 줄이 이상하게 깨져 보인다"로만 나타난다.
//
// 대부분의 DB 이미지는 Tty 없이 돈다. 하지만 사람이 도커에서 직접 만든
// 컨테이너를 우리가 읽을 수도 있으므로, 짐작하지 않고 살펴본다.

// LogLine은 로그 한 줄이다.
type LogLine struct {
	// Stream은 stdout 또는 stderr 다.
	//
	// 갈라서 주는 이유: DB 는 평범한 시작 로그를 stderr 로 내는 것이 흔하다
	// (ClickHouse·PostgreSQL 이 그렇다). 화면이 stderr 를 빨갛게 칠하면
	// 정상적으로 뜬 DB 가 온통 오류처럼 보인다 — 색은 화면이 정하되, 어느
	// 쪽에서 왔는지는 알려 줘야 한다.
	Stream string `json:"stream"`
	// Timestamp는 도커가 붙인 시각이다(Timestamps 를 켰을 때).
	Timestamp string `json:"timestamp,omitempty"`
	Text      string `json:"text"`
}

// LogOptions는 무엇을 어떻게 읽을지다.
type LogOptions struct {
	// Tail은 끝에서 몇 줄을 읽을지다. 0 이면 100 줄.
	Tail int
	// Follow면 새 줄이 생길 때마다 이어서 준다(끝나지 않는다).
	Follow bool
	// Timestamps면 도커가 각 줄에 시각을 붙인다.
	Timestamps bool
	// Since는 이 시각 이후만 읽는다(유닉스 초). 0 이면 전부.
	Since int64
	// Stdout·Stderr 는 어느 쪽을 읽을지다. 둘 다 false 면 둘 다 읽는다.
	Stdout bool
	Stderr bool
}

// LogStream은 열려 있는 로그 스트림이다. 다 읽었으면 Close 해야 한다.
type LogStream struct {
	body io.ReadCloser
	tty  bool
}

// Close는 스트림을 닫는다.
//
// 따라가는(Follow) 스트림에서는 이것이 유일한 멈추는 방법이다. ctx 를 취소해도
// 되지만, 화면이 창을 닫았을 때 부를 것은 이쪽이다.
func (s *LogStream) Close() error { return s.body.Close() }

// Logs는 로그 스트림을 연다.
//
// Tty 여부를 먼저 살펴본다(호출이 하나 더 늘지만 짐작하지 않는다).
func (c *Client) Logs(ctx context.Context, id string, opt LogOptions) (*LogStream, error) {
	ins, err := c.InspectContainer(ctx, id)
	if err != nil {
		return nil, err
	}

	tail := opt.Tail
	if tail <= 0 {
		tail = 100
	}
	stdout, stderr := opt.Stdout, opt.Stderr
	if !stdout && !stderr {
		stdout, stderr = true, true
	}
	params := map[string]string{
		"stdout": boolParam(stdout),
		"stderr": boolParam(stderr),
		"tail":   strconv.Itoa(tail),
	}
	if opt.Follow {
		params["follow"] = "true"
	}
	if opt.Timestamps {
		params["timestamps"] = "true"
	}
	if opt.Since > 0 {
		params["since"] = strconv.FormatInt(opt.Since, 10)
	}

	path := "/" + APIVersion + "/containers/" + url.PathEscape(id) + "/logs" + q(params)
	res, err := c.do(ctx, http.MethodGet, path, nil, "로그 읽기")
	if err != nil {
		return nil, err
	}
	return &LogStream{body: res.Body, tty: ins.Config.Tty}, nil
}

// Lines는 로그를 한 줄씩 흘려준다.
//
// 채널이 아니라 이터레이터인 이유: 부르는 쪽이 도중에 멈추는 것이 흔한 일이다
// (화면을 닫는다, ctx 가 끝난다). 채널로 주면 멈춘 뒤에 쓰는 쪽이 막히고,
// 그것을 피하려면 또 하나의 채널을 두어야 한다.
func (s *LogStream) Lines() func(func(LogLine) bool) {
	if s.tty {
		return s.plainLines
	}
	return s.framedLines
}

// plainLines는 Tty 컨테이너의 로그다(프레임이 없다).
func (s *LogStream) plainLines(yield func(LogLine) bool) {
	sc := bufio.NewScanner(s.body)
	sc.Buffer(make([]byte, 0, 64<<10), maxLogLine)
	for sc.Scan() {
		if !yield(parseLogLine("stdout", sc.Text())) {
			return
		}
	}
}

// framedLines는 8바이트 머리말이 붙은 로그를 푼다.
func (s *LogStream) framedLines(yield func(LogLine) bool) {
	head := make([]byte, 8)
	r := bufio.NewReaderSize(s.body, 64<<10)
	for {
		if _, err := io.ReadFull(r, head); err != nil {
			return // EOF 도 여기로 온다. 정상적인 끝이다.
		}
		size := binary.BigEndian.Uint32(head[4:8])
		if size == 0 {
			continue
		}
		// 한 프레임의 크기를 막는다. 머리말이 어긋나면(우리가 Tty 를 잘못 봤다면)
		// 여기 들어오는 숫자가 쓰레기가 되고, 그대로 믿으면 기가바이트를 잡는다.
		if size > maxLogFrame {
			return
		}
		buf := make([]byte, size)
		if _, err := io.ReadFull(r, buf); err != nil {
			return
		}
		stream := "stdout"
		if head[0] == 2 {
			stream = "stderr"
		}
		// 한 프레임에 여러 줄이 들어 있을 수 있다.
		for _, line := range strings.Split(strings.TrimRight(string(buf), "\n"), "\n") {
			if !yield(parseLogLine(stream, line)) {
				return
			}
		}
	}
}

// parseLogLine은 도커가 앞에 붙인 시각을 떼어 낸다.
//
// Timestamps 를 켜면 줄 앞에 RFC3339Nano 가 붙는다. 그것을 떼어 내는 이유:
// 화면이 시각을 따로 다뤄야 한다(정렬·필터). 글자로 남겨 두면 화면이 다시
// 문자열을 잘라야 하고, 시각이 없는 줄에서 그 자르기가 첫 낱말을 먹는다.
func parseLogLine(stream, raw string) LogLine {
	line := LogLine{Stream: stream, Text: raw}
	space := strings.IndexByte(raw, ' ')
	if space <= 0 {
		return line
	}
	head := raw[:space]
	// 시각인지 대충 본다. 파싱까지 하지 않는 이유: 도커가 주는 모양은 정해져
	// 있고, 아닌 줄에까지 파서를 돌리면 큰 로그에서 그것이 비용이 된다.
	if len(head) >= 20 && head[4] == '-' && head[7] == '-' && head[10] == 'T' {
		line.Timestamp = head
		line.Text = raw[space+1:]
	}
	return line
}

const (
	// maxLogLine은 Tty 로그 한 줄의 상한이다.
	maxLogLine = 1 << 20
	// maxLogFrame은 프레임 하나의 상한이다.
	maxLogFrame = 4 << 20
)
