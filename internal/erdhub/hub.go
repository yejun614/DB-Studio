// Package erdhub은 ERD 문서의 실시간 동시 편집을 중개한다.
//
// 이 패키지는 전송 계층(WebSocket)을 모른다. 클라이언트는 바이트 채널로만
// 이야기하므로, 동시 편집·충돌·재접속 같은 어려운 부분을 소켓 없이 테스트할 수 있다.
// WebSocket 읽기/쓰기 펌프는 api 계층이 담당한다.
//
// 동기화 규약:
//
//	클라이언트 → {type:"ops", ops:[...], baseSeq}
//	서버        → 검증 → seq 부여 → 저장 → 모든 구독자에게 {type:"ops"} 브로드캐스트
//	거부되면    → 그 클라이언트에게만 {type:"reject", ...} + 최신 상태 전체
//
// 브로드캐스트는 보낸 사람에게도 간다. 자기 op를 특별 취급하지 않으므로 경로가
// 하나뿐이고, 클라이언트는 op.id로 자기 것을 알아보고 낙관적 적용을 정합한다.
package erdhub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"dbstudio/internal/erd"
	"dbstudio/internal/store"
)

// compactEvery는 몇 개의 op마다 스냅샷을 굳힐지 정한다.
// 너무 자주 하면 큰 스키마를 반복해서 다시 쓰고, 너무 드물면 재접속 로딩이 느려진다.
const compactEvery = 100

// keepOpsAfterCompact은 압축 후에도 남겨둘 최근 op 수다.
// 변경 이력을 화면에서 보여주기 위해 남기며, P7이 이름 변경 의도를 읽는 근거이기도 하다.
const keepOpsAfterCompact = 200

// sendBuffer는 클라이언트별 송신 버퍼 크기다.
// 버퍼가 가득 차면 그 연결을 끊는다 — 느린 클라이언트 하나를 기다리며 방 전체를
// 멈추면 나머지 참여자의 편집이 막힌다.
const sendBuffer = 64

// maxChatLen은 채팅 한 줄의 길이 제한이다.
const maxChatLen = 4000

// maxOpsPerMessage는 한 메시지에 담을 수 있는 op 수다.
// 드래그 중 좌표 op가 묶여 오는 것을 허용하면서, 한 번에 과도한 작업을 막는다.
const maxOpsPerMessage = 64

// Hub는 문서별 방을 관리한다.
type Hub struct {
	st  *store.Store
	log *slog.Logger

	mu    sync.Mutex
	rooms map[string]*room

	// undos는 사람마다의 되돌리기 스택이다. 방(room)과 수명을 나눈 이유는
	// undo.go에 적어 두었다 — 새로고침으로 방이 비어도 되돌리기는 남아야 한다.
	undos *undoStore
}

func New(st *store.Store, log *slog.Logger) *Hub {
	if log == nil {
		log = slog.Default()
	}
	return &Hub{st: st, log: log, rooms: map[string]*room{}, undos: newUndoStore()}
}

// room은 한 문서의 권위 상태와 구독자를 들고 있다.
//
// 락 순서는 항상 Hub.mu → room.mu 다. 반대로 잡는 경로를 만들면 교착한다.
type room struct {
	hub *Hub
	id  string

	mu              sync.Mutex
	doc             *erd.Document
	clients         map[*Client]bool
	opsSinceCompact int

	// seenOps는 이미 적용한 op의 ID다. 재접속한 클라이언트는 확인받지 못한 op를
	// 다시 보내는데, 그 op가 이미 적용됐는지 검증보다 먼저 알아야 한다.
	// 그렇지 않으면 "테이블이 이미 있습니다" 같은 엉뚱한 거부가 나간다.
	// order는 오래된 ID를 버리기 위한 FIFO다 (무한히 쌓이면 메모리가 샌다).
	seenOps   map[string]bool
	seenOrder []string
}

// seenOpsLimit은 기억해 둘 op ID 수다. 재전송은 재접속 직후에만 일어나므로
// 최근 것만 알면 충분하다.
const seenOpsLimit = 1024

// markSeen은 op ID를 기억한다. 호출자가 room.mu를 들고 있어야 한다.
func (r *room) markSeen(id string) {
	if r.seenOps[id] {
		return
	}
	r.seenOps[id] = true
	r.seenOrder = append(r.seenOrder, id)
	if len(r.seenOrder) > seenOpsLimit {
		drop := r.seenOrder[0]
		r.seenOrder = r.seenOrder[1:]
		delete(r.seenOps, drop)
	}
}

// Participant는 접속하는 사람의 정보다.
type Participant struct {
	UserID   string
	UserName string
	// CanEdit이 false면 op를 보낼 수 없다(읽기 전용 참여). 채팅은 허용한다 —
	// 리뷰어가 의견을 남기는 것이 리뷰의 목적이므로 편집 권한과 분리한다.
	CanEdit bool
}

// Client는 한 연결이다. 같은 사용자가 탭을 두 개 열면 Client도 두 개다.
type Client struct {
	ID   string
	room *room
	p    Participant

	send   chan []byte
	closed chan struct{}

	// presence는 커서 위치와 선택 중인 객체다. 메모리에만 두며 영속하지 않는다 —
	// 연결이 끊긴 사람의 커서를 저장해 둘 이유가 없다.
	presenceMu sync.Mutex
	cursor     *Point
	selection  string
}

type Point struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

// PresenceView는 참여자 목록의 한 항목이다.
type PresenceView struct {
	ClientID  string `json:"clientId"`
	UserID    string `json:"userId,omitempty"`
	UserName  string `json:"userName"`
	Color     string `json:"color"`
	CanEdit   bool   `json:"canEdit"`
	Cursor    *Point `json:"cursor,omitempty"`
	Selection string `json:"selection,omitempty"`
}

// ---------- 참여 / 이탈 ----------

// Join은 문서 방에 참여한다. 방이 없으면 저장소에서 문서를 복원해 만든다.
func (h *Hub) Join(ctx context.Context, docID, clientID string, p Participant) (*Client, error) {
	h.mu.Lock()
	r := h.rooms[docID]
	if r == nil {
		doc, err := h.st.GetERDDocument(ctx, docID)
		if err != nil {
			h.mu.Unlock()
			return nil, err
		}
		r = &room{
			hub: h, id: docID, doc: doc, clients: map[*Client]bool{},
			seenOps: map[string]bool{},
		}
		// 저장된 op ID를 기억해 두어야 방이 다시 만들어진 뒤에 온 재전송도 걸러진다.
		if ids, err := h.st.RecentERDOpIDs(ctx, docID, seenOpsLimit); err == nil {
			for _, id := range ids {
				r.markSeen(id)
			}
		} else {
			h.log.Warn("ERD op ID 이력 로딩 실패", "doc", docID, "error", err)
		}
		h.rooms[docID] = r
	}
	h.mu.Unlock()

	c := &Client{
		ID: clientID, room: r, p: p,
		send:   make(chan []byte, sendBuffer),
		closed: make(chan struct{}),
	}

	// 채팅 이력은 락을 잡기 전에 읽는다. 방 락 안에서 DB를 읽으면 그 시간 동안
	// 다른 참여자의 편집이 모두 멈춘다.
	chat, err := h.st.ListERDChatMessages(ctx, docID, 100)
	if err != nil {
		h.log.Warn("ERD 채팅 로딩 실패", "doc", docID, "error", err)
		chat = []*store.ERDChatMessage{}
	}

	r.mu.Lock()
	r.clients[c] = true
	initMsg := r.initMessageLocked(c, chat)
	participants := r.presenceLocked()
	r.mu.Unlock()

	// 최초 상태는 참여자 본인에게만 보낸다. 본인은 init에 참여자 목록이 이미
	// 들어 있으므로 참여 알림에서 제외한다 — 같은 정보를 두 번 보내면
	// 클라이언트가 어느 것을 기준으로 삼아야 할지 모호해진다.
	c.enqueue(initMsg)
	r.broadcastExcept(c, mustJSON(map[string]any{"type": "presence", "participants": participants}))
	return c, nil
}

// Leave는 연결을 정리한다. 마지막 참여자가 나가면 방을 접고 상태를 굳힌다.
func (c *Client) Leave() {
	r := c.room
	h := r.hub

	h.mu.Lock()
	r.mu.Lock()
	if !r.clients[c] {
		r.mu.Unlock()
		h.mu.Unlock()
		return
	}
	delete(r.clients, c)
	empty := len(r.clients) == 0
	doc := r.doc
	participants := r.presenceLocked()
	if empty {
		delete(h.rooms, r.id)
	}
	r.mu.Unlock()
	h.mu.Unlock()

	close(c.closed)

	if empty {
		// 방을 접기 전에 현재 상태를 스냅샷으로 굳혀 다음 로딩을 싸게 만든다.
		// 실패해도 op-log가 남아 있어 상태는 안전하므로 로그만 남긴다.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := h.st.CompactERDDocument(ctx, doc, keepOpsAfterCompact); err != nil {
			h.log.Warn("ERD 문서 압축 실패", "doc", r.id, "error", err)
		}
		return
	}
	r.broadcast(mustJSON(map[string]any{"type": "presence", "participants": participants}))
}

// Out은 이 클라이언트에게 보낼 메시지 채널이다. api의 쓰기 펌프가 소비한다.
func (c *Client) Out() <-chan []byte { return c.send }

// Closed는 허브가 이 연결을 버렸을 때 닫힌다(느린 소비자, 방 종료 등).
func (c *Client) Closed() <-chan struct{} { return c.closed }

func (c *Client) CanEdit() bool { return c.p.CanEdit }

// ---------- 수신 메시지 ----------

type inbound struct {
	Type      string          `json:"type"`
	Ops       []*erd.Op       `json:"ops,omitempty"`
	BaseSeq   int64           `json:"baseSeq,omitempty"`
	Cursor    *Point          `json:"cursor,omitempty"`
	Selection *string         `json:"selection,omitempty"`
	Body      string          `json:"body,omitempty"`
	TargetKey string          `json:"targetKey,omitempty"`
	SinceSeq  int64           `json:"sinceSeq,omitempty"`
	Extra     json.RawMessage `json:"-"`
}

// Handle은 클라이언트가 보낸 한 메시지를 처리한다.
//
// 오류를 반환하는 것은 "연결을 끊어야 하는 상황"뿐이다. 편집 거부처럼 정상적인
// 실패는 그 클라이언트에게 reject 메시지로 알리고 nil을 반환한다.
func (c *Client) Handle(ctx context.Context, raw []byte) error {
	var msg inbound
	if err := json.Unmarshal(raw, &msg); err != nil {
		c.sendError("메시지를 해석할 수 없습니다")
		return nil
	}
	switch msg.Type {
	case "ops":
		return c.handleOps(ctx, &msg)
	case "presence":
		c.handlePresence(&msg)
		return nil
	case "chat":
		return c.handleChat(ctx, &msg)
	case "resync":
		c.handleResync(ctx, msg.SinceSeq)
		return nil
	case "undo":
		return c.handleUndo(ctx, false)
	case "redo":
		return c.handleUndo(ctx, true)
	case "ping":
		c.enqueue(mustJSON(map[string]any{"type": "pong"}))
		return nil
	}
	c.sendError("알 수 없는 메시지 종류입니다: " + msg.Type)
	return nil
}

func (c *Client) handleOps(ctx context.Context, msg *inbound) error {
	if !c.p.CanEdit {
		c.sendError("이 문서를 편집할 권한이 없습니다")
		return nil
	}
	if len(msg.Ops) == 0 {
		return nil
	}
	if len(msg.Ops) > maxOpsPerMessage {
		c.sendError(fmt.Sprintf("한 번에 보낼 수 있는 편집은 %d개까지입니다", maxOpsPerMessage))
		return nil
	}

	r := c.room
	changed := false
	for _, op := range msg.Ops {
		if strings.TrimSpace(op.ID) == "" {
			c.sendError("편집 ID가 없습니다")
			return nil
		}
		op.Actor, op.ActorName = c.p.UserID, c.p.UserName
		if op.BaseSeq == 0 {
			op.BaseSeq = msg.BaseSeq
		}

		applied, rejectErr, err := r.submit(ctx, op, intentEdit)
		if err != nil {
			// 저장 실패는 이 연결의 문제가 아니라 서버 문제다. 알리고 계속 살려둔다.
			r.hub.log.Error("ERD op 저장 실패", "doc", r.id, "kind", op.Kind, "error", err)
			c.sendError("편집을 저장하지 못했습니다: " + err.Error())
			return nil
		}
		if rejectErr != nil {
			// 낙관적으로 적용한 클라이언트는 지금 틀린 상태를 보고 있다.
			// 최신 상태를 함께 보내 되돌릴 필요 없이 정합되게 한다.
			c.enqueue(r.rejectMessage(op, rejectErr))
			return nil
		}
		if applied {
			changed = true
			r.broadcast(r.opMessage(op))
		}
	}
	// 되돌리기 버튼의 상태는 편집한 사람에게만 달라진다. 재전송처럼 아무것도
	// 적용되지 않은 경우에는 보내지 않는다 — 바뀐 것이 없다.
	if changed {
		c.sendUndoState()
	}
	return nil
}

// opMessage는 브로드캐스트할 op 메시지를 만든다.
//
// 구조를 바꾼 op에는 적용 후 문서 전체를 함께 담는다. 이유가 중요하다: 그렇게 하지
// 않으면 클라이언트가 op 적용 로직(검증·제약 정리·이름 변경 전파)을 JavaScript로
// 다시 구현해야 하고, 두 구현이 어긋나는 순간 사용자마다 다른 스키마를 보게 된다.
// 구조 편집은 사람의 속도(분당 몇 번)로 일어나므로 문서를 다시 보내는 비용이 싸다.
//
// 반대로 이동·메모 op는 초당 수십 번 발생하는데 레이아웃 좌표 대입은 틀릴 여지가
// 없다. 그래서 이 둘만 op 자체를 보내고 클라이언트가 직접 반영한다.
func (r *room) opMessage(op *erd.Op) []byte {
	payload := map[string]any{"type": "ops", "ops": []*erd.Op{op}}
	if op.Kind.Structural() {
		r.mu.Lock()
		payload["document"] = r.doc
		payload["seq"] = r.doc.Seq
		r.mu.Unlock()
	}
	return mustJSON(payload)
}

// submit은 op를 검증·적용·저장한다. 이 함수 전체가 방 락 안에서 실행되므로
// 문서마다 op 순서가 하나로 정해진다.
//
// 반환값: applied(브로드캐스트할지), rejectErr(사용자에게 알릴 거부 사유), err(서버 오류)
func (r *room) submit(ctx context.Context, op *erd.Op, in intent) (bool, *erd.Error, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// 재전송 확인을 검증보다 먼저 한다. 순서를 뒤집으면 이미 적용된 op가
	// "이미 있습니다" 충돌로 거부되어, 사용자에게 없던 오류를 보여주게 된다.
	if r.seenOps[op.ID] {
		return false, nil, nil
	}

	// 사본에 먼저 적용한다. 검증이 적용 도중 실패할 수 있어, 원본에 바로 쓰면
	// 부분 변경된 상태가 남고 이후 모든 op가 그 위에서 동작한다.
	next := r.doc.Clone()
	if err := erd.Apply(next, op); err != nil {
		var opErr *erd.Error
		if errors.As(err, &opErr) {
			return false, opErr, nil
		}
		return false, &erd.Error{Code: "invalid", Reason: err.Error()}, nil
	}

	if err := r.hub.st.AppendERDOp(ctx, r.doc, op); err != nil {
		if errors.Is(err, store.ErrOpConflict) {
			// seenOps가 놓친 재전송(압축으로 ID를 잊은 아주 오래된 op)이다.
			// 저장 계층의 유니크 제약이 마지막 방어선 역할을 한다.
			r.markSeen(op.ID)
			return false, nil, nil
		}
		return false, nil, err
	}
	r.markSeen(op.ID)

	// 저장이 성공한 뒤에 상태를 교체한다. AppendERDOp이 doc.Seq를 올려주므로
	// 사본에도 같은 seq를 옮겨 담는다.
	next.Seq = r.doc.Seq
	prev := r.doc
	r.doc = next
	r.recordUndo(prev, next, op, in)

	if op.Kind.Structural() {
		r.opsSinceCompact++
	}
	if r.opsSinceCompact >= compactEvery {
		if ok, err := r.hub.st.CompactERDDocument(ctx, r.doc, keepOpsAfterCompact); err != nil {
			r.hub.log.Warn("ERD 문서 압축 실패", "doc", r.id, "error", err)
		} else if ok {
			r.opsSinceCompact = 0
		}
	}
	return true, nil, nil
}

// SubmitOp은 소켓 밖에서 온 편집을 적용한다(SQL 불러오기 같은 REST 경로).
//
// **Hub 락을 끝까지 쥔다.** 그러지 않으면 이 함수가 저장소에 직접 쓰는 사이에
// 누군가 문서를 열 수 있고, 그 방은 이 op가 반영되기 **전**의 문서를 메모리에
// 들고 시작한다. 그 뒤 방에서 일어나는 모든 편집은 사라진 상태 위에서 계산되고,
// 그 어긋남은 아무 오류도 내지 않은 채 사람마다 다른 그림으로 나타난다.
// 불러오기는 사람이 몇 분에 한 번 누르는 동작이므로, 그동안 참여/이탈이 잠시
// 기다리는 편이 훨씬 싸다.
//
// 반환값은 적용 후의 문서다. 호출자가 결과를 그대로 응답에 담을 수 있게 한다.
func (h *Hub) SubmitOp(ctx context.Context, docID string, op *erd.Op) (*erd.Document, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	r := h.rooms[docID]
	if r != nil {
		applied, rejectErr, err := r.submit(ctx, op, intentEdit)
		if err != nil {
			return nil, err
		}
		if rejectErr != nil {
			return nil, rejectErr
		}
		if applied {
			// 구조 op이므로 문서 전체가 함께 나간다. 열어 둔 사람의 화면이
			// 그 자리에서 새 스키마로 바뀐다.
			r.broadcast(r.opMessage(op))
		}
		r.mu.Lock()
		doc := r.doc
		r.mu.Unlock()
		return doc, nil
	}

	// 아무도 열어 두지 않은 문서. 저장소의 상태에 직접 적용한다.
	doc, err := h.st.GetERDDocument(ctx, docID)
	if err != nil {
		return nil, err
	}
	next := doc.Clone()
	if err := erd.Apply(next, op); err != nil {
		return nil, err
	}
	if err := h.st.AppendERDOp(ctx, doc, op); err != nil {
		if errors.Is(err, store.ErrOpConflict) {
			// 같은 op가 이미 들어갔다. 재전송이므로 성공으로 본다.
			return doc, nil
		}
		return nil, err
	}
	next.Seq = doc.Seq
	// 소켓 밖에서 온 편집(AI 툴·SQL 불러오기)도 되돌릴 수 있어야 한다.
	// 방이 없다는 것은 아무도 열어 두지 않았다는 뜻일 뿐, 편집한 사람은 있다.
	if op.Actor != "" && op.Kind != erd.OpSchemaImport {
		h.undos.record(docID, op.Actor, erd.Diff(next, doc), intentEdit)
	}
	// 곧바로 굳혀 둔다. 이 op는 문서를 통째로 바꾸므로, 스냅샷 없이 op 로그에만
	// 남겨 두면 다음에 문서를 열 때 큰 payload를 다시 재생해야 한다.
	if _, err := h.st.CompactERDDocument(ctx, next, keepOpsAfterCompact); err != nil {
		h.log.Warn("ERD 문서 압축 실패", "doc", docID, "error", err)
	}
	return next, nil
}

func (c *Client) handlePresence(msg *inbound) {
	c.presenceMu.Lock()
	if msg.Cursor != nil {
		c.cursor = msg.Cursor
	}
	selectionChanged := false
	if msg.Selection != nil {
		if c.selection != *msg.Selection {
			selectionChanged = true
		}
		c.selection = *msg.Selection
	}
	cursor := c.cursor
	c.presenceMu.Unlock()

	if selectionChanged {
		// 선택 변경은 참여자 목록 전체를 다시 보낸다 (빈도가 낮다).
		c.room.mu.Lock()
		participants := c.room.presenceLocked()
		c.room.mu.Unlock()
		c.room.broadcast(mustJSON(map[string]any{"type": "presence", "participants": participants}))
		return
	}
	if cursor == nil {
		return
	}
	// 커서 이동은 빈도가 높으므로 목록 전체 대신 최소 정보만 보낸다.
	c.room.broadcastExcept(c, mustJSON(map[string]any{
		"type": "cursor", "clientId": c.ID, "cursor": cursor,
	}))
}

func (c *Client) handleChat(ctx context.Context, msg *inbound) error {
	body := strings.TrimSpace(msg.Body)
	if body == "" {
		return nil
	}
	if len([]rune(body)) > maxChatLen {
		c.sendError(fmt.Sprintf("메시지가 너무 깁니다 (%d자 제한)", maxChatLen))
		return nil
	}
	m := &store.ERDChatMessage{
		DocID: c.room.id, UserID: c.p.UserID, UserName: c.p.UserName,
		Body: body, Kind: store.ChatKindMessage, TargetKey: msg.TargetKey,
	}
	if err := c.room.hub.st.AddERDChatMessage(ctx, m); err != nil {
		c.room.hub.log.Error("ERD 채팅 저장 실패", "doc", c.room.id, "error", err)
		c.sendError("메시지를 저장하지 못했습니다")
		return nil
	}
	c.room.broadcast(mustJSON(map[string]any{"type": "chat", "message": m}))
	return nil
}

// handleResync는 클라이언트가 자기 상태를 신뢰할 수 없을 때 호출된다.
// sinceSeq 이후의 op만 보낼 수 있으면 그렇게 하고, 그 op가 이미 압축되어 없으면
// 상태 전체를 보낸다.
func (c *Client) handleResync(ctx context.Context, sinceSeq int64) {
	r := c.room
	r.mu.Lock()
	currentSeq := r.doc.Seq
	r.mu.Unlock()

	if sinceSeq > 0 && sinceSeq <= currentSeq {
		ops, err := r.hub.st.ListERDOps(ctx, r.id, sinceSeq)
		if err == nil && len(ops) > 0 && ops[0].Seq == sinceSeq+1 {
			c.enqueue(mustJSON(map[string]any{"type": "ops", "ops": ops}))
			return
		}
		if err == nil && sinceSeq == currentSeq {
			return // 이미 최신이다
		}
	}
	r.mu.Lock()
	msg := r.stateMessageLocked()
	r.mu.Unlock()
	c.enqueue(msg)
}

// ---------- 메시지 만들기 ----------

func (r *room) initMessageLocked(c *Client, chat []*store.ERDChatMessage) []byte {
	// 되돌리기 스택은 방보다 오래 산다. 새로고침해서 다시 들어온 사람도
	// 자기가 방금 한 편집을 되돌릴 수 있어야 하므로 처음부터 상태를 알려준다.
	canUndo, canRedo := r.hub.undos.state(r.id, c.p.UserID)
	return mustJSON(map[string]any{
		"type": "init", "canUndo": canUndo, "canRedo": canRedo,
		"you": PresenceView{
			ClientID: c.ID, UserID: c.p.UserID, UserName: c.p.UserName,
			Color: colorFor(c.p.UserID + c.ID), CanEdit: c.p.CanEdit,
		},
		"document":     r.doc,
		"seq":          r.doc.Seq,
		"participants": r.presenceLocked(),
		"chat":         chat,
	})
}

func (r *room) stateMessageLocked() []byte {
	return mustJSON(map[string]any{
		"type": "state", "document": r.doc, "seq": r.doc.Seq,
	})
}

func (r *room) rejectMessage(op *erd.Op, opErr *erd.Error) []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return mustJSON(map[string]any{
		"type": "reject", "opId": op.ID, "kind": op.Kind,
		"code": opErr.Code, "reason": opErr.Reason,
		"document": r.doc, "seq": r.doc.Seq,
	})
}

// presenceLocked은 참여자 목록을 만든다. 호출자가 room.mu를 들고 있어야 한다.
func (r *room) presenceLocked() []PresenceView {
	out := make([]PresenceView, 0, len(r.clients))
	for c := range r.clients {
		c.presenceMu.Lock()
		view := PresenceView{
			ClientID: c.ID, UserID: c.p.UserID, UserName: c.p.UserName,
			Color: colorFor(c.p.UserID + c.ID), CanEdit: c.p.CanEdit,
			Cursor: c.cursor, Selection: c.selection,
		}
		c.presenceMu.Unlock()
		out = append(out, view)
	}
	// 맵 순회 순서는 무작위다. 목록이 매번 뒤바뀌면 화면의 참여자 순서가 흔들린다.
	sort.Slice(out, func(i, j int) bool {
		if out[i].UserName != out[j].UserName {
			return out[i].UserName < out[j].UserName
		}
		return out[i].ClientID < out[j].ClientID
	})
	return out
}

func (c *Client) sendError(reason string) {
	c.enqueue(mustJSON(map[string]any{"type": "error", "message": reason}))
}

// enqueue는 이 클라이언트에게 메시지를 보낸다.
// 버퍼가 가득 찼으면 연결을 버린다 — 느린 소비자를 기다리면 방 전체가 멈춘다.
func (c *Client) enqueue(data []byte) {
	select {
	case <-c.closed:
		return
	default:
	}
	select {
	case c.send <- data:
	default:
		c.room.hub.log.Warn("ERD 클라이언트 송신 버퍼 초과, 연결을 끊습니다",
			"doc", c.room.id, "client", c.ID, "user", c.p.UserName)
		go c.Leave()
	}
}

func (r *room) broadcast(data []byte) { r.broadcastExcept(nil, data) }

func (r *room) broadcastExcept(skip *Client, data []byte) {
	r.mu.Lock()
	targets := make([]*Client, 0, len(r.clients))
	for c := range r.clients {
		if c == skip {
			continue
		}
		targets = append(targets, c)
	}
	r.mu.Unlock()

	// 방 락을 놓고 보낸다. enqueue가 느린 클라이언트를 정리하려고 Leave를 호출할 수
	// 있고, Leave는 Hub.mu → room.mu 순으로 락을 잡으므로 락을 들고 있으면 교착한다.
	for _, c := range targets {
		c.enqueue(data)
	}
}

// ---------- 브로드캐스트 유틸 ----------

// Broadcast는 문서를 보고 있는 모든 참여자에게 알린다.
// 문서 이름 변경이나 상태 전환처럼 REST 경로에서 일어난 변경을 알릴 때 쓴다.
func (h *Hub) Broadcast(docID string, payload map[string]any) {
	h.mu.Lock()
	r := h.rooms[docID]
	h.mu.Unlock()
	if r == nil {
		return
	}
	r.broadcast(mustJSON(payload))
}

// Participants는 지금 문서를 보고 있는 사람들이다. 목록 화면에서 "3명 편집 중"을 표시한다.
func (h *Hub) Participants(docID string) []PresenceView {
	h.mu.Lock()
	r := h.rooms[docID]
	h.mu.Unlock()
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.presenceLocked()
}

// ActiveCounts는 방마다 참여자 수를 반환한다.
func (h *Hub) ActiveCounts() map[string]int {
	h.mu.Lock()
	rooms := make([]*room, 0, len(h.rooms))
	for _, r := range h.rooms {
		rooms = append(rooms, r)
	}
	h.mu.Unlock()

	out := map[string]int{}
	for _, r := range rooms {
		r.mu.Lock()
		out[r.id] = len(r.clients)
		r.mu.Unlock()
	}
	return out
}

// CloseDocument는 문서가 삭제될 때 방을 정리한다.
// 삭제된 문서의 op를 계속 받으면 저장 실패가 반복된다.
func (h *Hub) CloseDocument(docID, reason string) {
	h.mu.Lock()
	r := h.rooms[docID]
	if r != nil {
		delete(h.rooms, docID)
	}
	h.mu.Unlock()
	if r == nil {
		return
	}
	r.mu.Lock()
	clients := make([]*Client, 0, len(r.clients))
	for c := range r.clients {
		clients = append(clients, c)
	}
	r.clients = map[*Client]bool{}
	r.mu.Unlock()

	notice := mustJSON(map[string]any{"type": "closed", "reason": reason})
	for _, c := range clients {
		select {
		case c.send <- notice:
		default:
		}
		close(c.closed)
	}
}

// colorFor는 참여자 색을 결정적으로 고른다.
// 무작위로 뽑으면 재접속마다 색이 바뀌어 "저 커서가 누구였는지"를 잃는다.
func colorFor(seed string) string {
	palette := []string{
		"#4f8ef7", "#f76c6c", "#4ec9a0", "#f7b32b", "#a978f7",
		"#3fb8d4", "#e86ea4", "#7fb800", "#ff8c42", "#6b7cff",
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(seed))
	return palette[int(h.Sum32())%len(palette)]
}

func mustJSON(v any) []byte {
	data, err := json.Marshal(v)
	if err != nil {
		// 여기서 실패하는 것은 코드 오류다(직렬화 불가한 값). 연결을 끊는 대신
		// 오류 메시지를 보내 원인을 남긴다.
		return []byte(`{"type":"error","message":"응답을 만들지 못했습니다"}`)
	}
	return data
}
