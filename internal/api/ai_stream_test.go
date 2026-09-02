package api

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/valyala/fasthttp"
)

// fakeConn은 쓰기 마감만 받아 적는 가짜 연결이다.
type fakeConn struct {
	net.Conn
	mu        sync.Mutex
	deadlines []time.Time
}

func (c *fakeConn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deadlines = append(c.deadlines, t)
	return nil
}

func (c *fakeConn) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.deadlines)
}

// 쓰기 마감은 **쓸 때마다** 다시 걸려야 한다.
//
// 한 번만 걸면 그 마감이 스트림 전체를 재게 되고(fasthttp가 그렇게 건다), 오래
// 생각한 답변은 다 쓰지 못한 채 연결이 끊긴다.
func TestSSEWriterRefreshesWriteDeadlineEachSend(t *testing.T) {
	var buf bytes.Buffer
	conn := &fakeConn{}
	out := &sseWriter{w: bufio.NewWriter(&buf), conn: conn}

	before := time.Now()
	if err := out.send("first", map[string]int{"i": 1}); err != nil {
		t.Fatalf("첫 쓰기: %v", err)
	}
	if err := out.send("second", map[string]int{"i": 2}); err != nil {
		t.Fatalf("둘째 쓰기: %v", err)
	}

	if got := conn.count(); got != 2 {
		t.Fatalf("마감을 쓸 때마다 걸어야 한다: %d번 걸렸다", got)
	}
	for i, d := range conn.deadlines {
		if !d.After(before.Add(streamWriteWait / 2)) {
			t.Errorf("%d번째 마감이 너무 이르다: %v (기준 %v)", i, d, before)
		}
	}
	body := buf.String()
	if !strings.Contains(body, "event: first") || !strings.Contains(body, "event: second") {
		t.Errorf("이벤트가 다 나가지 않았다: %q", body)
	}
}

// 연결을 못 꺼낸 경우(테스트의 가짜 연결)에도 쓰기는 되어야 한다.
func TestSSEWriterWithoutConnStillSends(t *testing.T) {
	var buf bytes.Buffer
	out := &sseWriter{w: bufio.NewWriter(&buf)}
	if err := out.send("hello", map[string]string{"a": "b"}); err != nil {
		t.Fatalf("쓰기: %v", err)
	}
	if !strings.Contains(buf.String(), `"a":"b"`) {
		t.Errorf("본문이 비었다: %q", buf.String())
	}
}

// 하트비트는 조용한 구간을 메우고, 멈추라고 하면 멈춘다.
//
// 멈춘 뒤에도 쓰면 스트림이 끝난 연결에 쓰는 셈이 된다.
func TestSSEWriterHeartbeatStops(t *testing.T) {
	var buf bytes.Buffer
	out := &sseWriter{w: bufio.NewWriter(&buf)}
	stop := out.heartbeat(20 * time.Millisecond)
	time.Sleep(120 * time.Millisecond)
	stop()

	if n := strings.Count(buf.String(), "event: ping"); n == 0 {
		t.Fatalf("하트비트가 아무것도 보내지 않았다: %q", buf.String())
	}
	after := buf.Len()
	time.Sleep(80 * time.Millisecond)
	if buf.Len() != after {
		t.Errorf("멈춘 뒤에도 썼다: %d → %d", after, buf.Len())
	}
	// 두 번 불러도 안전해야 한다(핸들러의 defer 와 오류 경로가 겹칠 수 있다).
	stop()
}

// 서버의 WriteTimeout 보다 오래 걸리는 스트림이 끝까지 나가는가.
//
// 이것이 실제로 났던 버그다: WriteTimeout 60초가 스트림 **전체**에 걸려,
// 1분 넘게 생각한 AI 답변이 도중에 끊겼다(브라우저에는 ERR_HTTP2_PROTOCOL_ERROR).
//
// 진짜 소켓으로 띄우는 이유: app.Test 의 메모리 파이프는 쓰기 마감을 버퍼가 찼을
// 때만 본다(fasthttputil.pipeConn.Write). 그래서 그쪽에서는 이 버그가 재현되지
// 않는다 — 재현되지 않는 자리에서 통과한 테스트는 아무것도 지켜 주지 않는다.
func TestStreamOutlivesWriteTimeout(t *testing.T) {
	app := fiber.New(fiber.Config{
		DisableStartupMessage: true,
		WriteTimeout:          150 * time.Millisecond,
	})
	var srv *Server
	app.Get("/stream", func(c *fiber.Ctx) error {
		c.Set(fiber.HeaderContentType, "text/event-stream")
		conn := srv.streamConn(c)
		c.Context().SetBodyStreamWriter(fasthttp.StreamWriter(func(w *bufio.Writer) {
			out := &sseWriter{w: w, conn: conn}
			for i := 0; i < 8; i += 1 {
				if out.send("tick", map[string]int{"i": i}) != nil {
					return
				}
				time.Sleep(70 * time.Millisecond)
			}
			_ = out.send("done", map[string]bool{"ok": true})
		}))
		return nil
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("소켓을 열 수 없다: %v", err)
	}
	go func() { _ = app.Listener(ln) }()
	defer func() { _ = app.Shutdown() }()

	res, err := http.Get("http://" + ln.Addr().String() + "/stream")
	if err != nil {
		t.Fatalf("요청: %v", err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	// 끊긴 응답은 여기서 오류가 되거나 본문이 잘린 채 끝난다. 둘 다 실패다.
	if err != nil {
		t.Fatalf("마감(150ms)보다 오래 걸린 스트림이 끊겼다: %v (읽은 것 %d바이트)", err, len(body))
	}
	if !strings.Contains(string(body), "event: done") {
		t.Fatalf("스트림이 끝까지 오지 않았다: %q", body)
	}
	if n := strings.Count(string(body), "event: tick"); n != 8 {
		t.Errorf("중간 이벤트가 빠졌다: tick %d개", n)
	}
}
