package api

import (
	"context"
	"errors"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/gofiber/contrib/websocket"
	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"dbstudio/internal/applog"
	"dbstudio/internal/erdhub"
	"dbstudio/internal/model"
	"dbstudio/internal/store"
)

// WebSocket 타임아웃.
//
// 핑/퐁으로 죽은 연결을 걷어내는 이유: TCP는 상대가 조용히 사라진 것을 알려주지
// 않는다. 그대로 두면 프레즌스 목록에 유령 참여자가 남고, 방이 영원히 닫히지 않아
// 메모리와 스냅샷 압축 시점을 잃는다.
const (
	wsWriteWait  = 10 * time.Second
	wsPongWait   = 60 * time.Second
	wsPingPeriod = 25 * time.Second
	// wsReadLimit은 한 메시지의 최대 크기다. op 묶음과 채팅을 담기에 충분하면서
	// 한 연결이 메모리를 크게 먹지 못하게 제한한다.
	wsReadLimit = 256 * 1024
)

// erdWSUpgrade는 WebSocket 업그레이드 전에 인증·권한·CSRF를 확인한다.
//
// 브라우저의 WebSocket API는 커스텀 헤더를 붙일 수 없어 X-Requested-With 검사를
// 쓸 수 없다. 대신 핸드셰이크에 반드시 실리는 Origin을 Host와 비교한다 —
// 이것이 WebSocket에서의 올바른 CSRF 방어다. (교차 출처 페이지가 사용자의 쿠키로
// 소켓을 여는 것을 막는다.)
func (s *Server) erdWSUpgrade(c *fiber.Ctx) error {
	if !websocket.IsWebSocketUpgrade(c) {
		return fiber.NewError(fiber.StatusUpgradeRequired, "WebSocket 연결이 필요합니다")
	}
	if err := checkSameOrigin(c); err != nil {
		return err
	}

	doc, conn, _, err := s.resolveERDDocument(c, c.Params("docId"), model.LevelMonitor)
	if err != nil {
		return err
	}
	// 편집 권한은 별도로 판정한다. 모니터링 등급은 읽기 전용 참여가 된다 —
	// 리뷰어가 초안을 보고 의견을 남길 수 있어야 리뷰가 성립한다.
	//
	// 대상 DB가 없는 독립 초안에는 등급을 물을 커넥션이 없다. 그 초안은 어떤
	// 데이터베이스도 가리키지 않으므로 로그인한 사람이면 함께 편집한다.
	canEdit := true
	if conn != nil {
		d, lerr := s.requireLevel(c, conn.ID, model.LevelERD)
		if lerr != nil {
			return lerr
		}
		canEdit = d.Allowed
	}

	u := currentUser(c)
	name := strings.TrimSpace(u.DisplayName)
	if name == "" {
		name = u.Username
	}
	c.Locals("erdDocID", doc.ID)
	c.Locals("erdCanEdit", canEdit)
	c.Locals("erdUserID", u.ID)
	c.Locals("erdUserName", name)

	s.audit(c, store.AuditParams{
		Action: "erd.open", TargetType: "erd_document", TargetID: doc.ID,
		Detail: map[string]any{
			"name": doc.Name, "connection": connName(conn), "canEdit": canEdit,
		},
	})
	return c.Next()
}

// checkSameOrigin은 WebSocket 핸드셰이크의 Origin이 이 서버인지 확인한다.
func checkSameOrigin(c *fiber.Ctx) error {
	origin := strings.TrimSpace(c.Get("Origin"))
	if origin == "" {
		// 브라우저는 항상 Origin을 보낸다. 없는 것은 브라우저가 아닌 클라이언트이며,
		// 그때는 쿠키 자동 전송이라는 CSRF의 전제가 성립하지 않으므로 허용한다
		// (테스트 도구와 CLI가 여기에 해당한다).
		return nil
	}
	u, err := url.Parse(origin)
	if err != nil {
		return fiber.NewError(fiber.StatusForbidden, "잘못된 Origin 헤더입니다")
	}
	if !strings.EqualFold(u.Host, c.Hostname()) {
		return fiber.NewError(fiber.StatusForbidden,
			"다른 출처에서의 연결은 허용되지 않습니다")
	}
	return nil
}

// handleERDSocket은 업그레이드된 연결을 허브에 붙이고 읽기/쓰기 펌프를 돌린다.
// handleERDSocket은 업그레이드된 연결을 처리한다.
//
// Fiber의 websocket 핸들러는 요청 goroutine 밖에서 실행되므로 recover 미들웨어가
// 감싸지 않는다. 읽기 루프에서 패닉이 나면 프로세스가 죽는다.
func (s *Server) handleERDSocket(conn *websocket.Conn) {
	defer applog.Recover("erd.ws.read")

	docID, _ := conn.Locals("erdDocID").(string)
	canEdit, _ := conn.Locals("erdCanEdit").(bool)
	userID, _ := conn.Locals("erdUserID").(string)
	userName, _ := conn.Locals("erdUserName").(string)
	if docID == "" {
		_ = conn.Close()
		return
	}

	// 리플리카에서는 방을 열지 않고 마스터로 중계한다. 이유는 cluster_ws.go에 있다.
	if s.cluster != nil && s.cluster.IsReplica() {
		s.proxyERDSocket(conn)
		return
	}

	// 업그레이드된 뒤에는 요청 컨텍스트가 끝나므로 별도 컨텍스트를 쓴다.
	// c.Context()를 그대로 들고 있으면 첫 op를 저장할 때 이미 취소된 컨텍스트가 된다.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	client, err := s.erdHub.Join(ctx, docID, uuid.NewString(), erdhub.Participant{
		UserID: userID, UserName: userName, CanEdit: canEdit,
	})
	if err != nil {
		msg := "문서를 열지 못했습니다"
		if errors.Is(err, store.ErrNotFound) {
			msg = "문서를 찾을 수 없습니다"
		}
		_ = conn.WriteJSON(fiber.Map{"type": "error", "message": msg})
		_ = conn.Close()
		return
	}
	defer client.Leave()

	conn.SetReadLimit(wsReadLimit)
	_ = conn.SetReadDeadline(time.Now().Add(wsPongWait))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(wsPongWait))
	})

	// 쓰기 펌프. WebSocket은 동시 쓰기를 허용하지 않으므로 모든 쓰기를
	// 이 goroutine 하나로 모은다.
	writeDone := make(chan struct{})
	go func() {
		// 이 goroutine은 Fiber의 recover 미들웨어 밖에서 돈다.
		// 여기서 패닉이 나면 프로세스 전체가 죽으므로 직접 잡아 기록한다.
		defer applog.Recover("erd.ws.write")
		defer close(writeDone)
		ping := time.NewTicker(wsPingPeriod)
		defer ping.Stop()
		for {
			select {
			case data, ok := <-client.Out():
				if !ok {
					return
				}
				_ = conn.SetWriteDeadline(time.Now().Add(wsWriteWait))
				if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
					return
				}
			case <-ping.C:
				_ = conn.SetWriteDeadline(time.Now().Add(wsWriteWait))
				if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
					return
				}
			case <-client.Closed():
				// 허브가 이 연결을 버렸다(느린 소비자, 문서 삭제 등).
				_ = conn.SetWriteDeadline(time.Now().Add(wsWriteWait))
				_ = conn.WriteMessage(websocket.CloseMessage,
					websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
				_ = conn.Close()
				return
			case <-ctx.Done():
				return
			}
		}
	}()

	// 읽기 펌프는 이 goroutine에서 돈다. 여기서 반환하면 연결이 끝난다.
	for {
		msgType, data, err := conn.ReadMessage()
		if err != nil {
			break
		}
		if msgType != websocket.TextMessage {
			continue
		}
		if err := client.Handle(ctx, data); err != nil {
			slog.Warn("ERD 메시지 처리 실패", "doc", docID, "error", err)
			break
		}
	}

	cancel()
	<-writeDone
}
