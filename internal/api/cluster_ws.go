package api

import (
	"log/slog"
	"net/http"
	"strings"
	"time"

	fws "github.com/fasthttp/websocket"
	"github.com/gofiber/contrib/websocket"

	"dbstudio/internal/applog"
	"dbstudio/internal/auth"
)

// ERD 실시간 편집 소켓의 노드 간 중계.
//
// 왜 필요한가: 실시간 편집은 메모리에 있는 방(erdhub)에서 op 순서를 정하고, 확정된 op를
// 메타 DB에 적는다. 리플리카가 그 일을 하면 두 가지가 동시에 깨진다 — 같은 문서를 두 노드가
// 각자 편집하면 순서가 갈리고, 리플리카가 적은 op는 다음 복제에서 사라진다.
//
// 그래서 리플리카는 소켓을 **중계한다**. 브라우저는 자기가 접속한 노드와 이야기하고,
// 그 노드는 같은 세션 쿠키로 마스터에 소켓을 열어 프레임을 양쪽으로 흘린다. 결과적으로
// 모든 편집자가 마스터의 방 하나에 모이므로, 노드가 달라도 서로의 커서가 보인다.

// proxyERDSocket은 브라우저 소켓과 마스터 소켓을 이어 준다.
func (s *Server) proxyERDSocket(client *websocket.Conn) {
	defer applog.Recover("erd.ws.proxy")
	defer client.Close()

	docID, _ := client.Locals("erdDocID").(string)
	if docID == "" {
		return
	}
	master := s.cluster.Config().MasterURL
	target := wsScheme(master) + "/api/v1/erd/documents/" + docID + "/socket"

	hdr := http.Header{}
	if token := client.Cookies(auth.SessionCookieName); token != "" {
		hdr.Set("Cookie", auth.SessionCookieName+"="+token)
	}
	// Origin을 마스터 자신으로 맞춘다. 마스터는 Origin과 Host가 같은지 보고 교차 출처
	// 연결을 막는데(erd_ws.go의 checkSameOrigin), 여기서 브라우저의 Origin(리플리카 주소)을
	// 그대로 넘기면 정상적인 중계가 그 검사에 걸린다.
	hdr.Set("Origin", master)

	dialer := fws.Dialer{HandshakeTimeout: 15 * time.Second}
	upstream, res, err := dialer.Dial(target, hdr)
	if err != nil {
		status := 0
		if res != nil {
			status = res.StatusCode
		}
		slog.Warn("마스터로 ERD 소켓을 잇지 못했습니다", "doc", docID, "status", status, "err", err)
		// 브라우저에 이유를 남기고 닫는다. 조용히 끊으면 화면은 "연결 끊김"만 반복한다.
		_ = client.WriteMessage(websocket.CloseMessage, fws.FormatCloseMessage(
			fws.CloseTryAgainLater, "마스터 노드에 닿지 못했습니다"))
		return
	}
	defer upstream.Close()

	// 양방향으로 흘린다. 한쪽이 끊기면 다른 쪽도 닫아 goroutine이 남지 않게 한다.
	done := make(chan struct{}, 2)
	go func() {
		defer applog.Recover("erd.ws.proxy.up")
		defer func() { done <- struct{}{} }()
		for {
			mt, msg, err := client.ReadMessage()
			if err != nil {
				return
			}
			if err := upstream.WriteMessage(mt, msg); err != nil {
				return
			}
		}
	}()
	go func() {
		defer applog.Recover("erd.ws.proxy.down")
		defer func() { done <- struct{}{} }()
		for {
			mt, msg, err := upstream.ReadMessage()
			if err != nil {
				return
			}
			if err := client.WriteMessage(mt, msg); err != nil {
				return
			}
		}
	}()
	<-done
}

// wsScheme은 http(s) 주소를 ws(s) 주소로 바꾼다.
func wsScheme(url string) string {
	switch {
	case strings.HasPrefix(url, "https://"):
		return "wss://" + strings.TrimPrefix(url, "https://")
	case strings.HasPrefix(url, "http://"):
		return "ws://" + strings.TrimPrefix(url, "http://")
	default:
		return url
	}
}
