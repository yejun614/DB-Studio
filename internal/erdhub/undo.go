package erdhub

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"

	"dbstudio/internal/erd"
)

// 되돌리기는 **사람마다** 따로다.
//
// 문서 하나를 여럿이 함께 고치는 곳에서 "마지막 편집"을 문서 단위로 잡으면, 내가
// Ctrl+Z를 눌렀을 때 남이 방금 한 일이 사라진다. 그것은 되돌리기가 아니라 사고다.
// 그래서 스택은 (문서, 사용자)마다 하나이며, 되돌리기는 언제나 **내가 한 편집**의
// 역만 적용한다.
//
// 되돌리는 op는 새로운 편집으로 기록된다(op-log에 남고 모두에게 브로드캐스트된다).
// 로그에서 지우는 방식이 아니다 — 서버 권위 op-log에서 과거를 고치면 그 사이에
// 남이 쌓아 올린 편집의 기준이 통째로 흔들린다.

const (
	// undoDepth는 한 사람이 한 문서에서 되돌릴 수 있는 횟수다.
	//
	// 무제한으로 두면 복원 payload(문서 일부의 전체 사본)가 메모리에 계속 쌓인다.
	// 30번이면 "방금 한 실수"를 되돌리기에 충분하고, 그보다 멀리 가야 하는 일은
	// 편집 이력 화면에서 확인하는 편이 맞다.
	undoDepth = 30
	// undoTTL이 지나면 스택을 버린다. 브라우저를 새로고침해도 되돌리기가 남아
	// 있도록 방(room)의 수명과 분리하되, 어제 열어 둔 탭의 스택까지 들고 있지는 않는다.
	undoTTL = 30 * time.Minute
)

// intent는 지금 적용하는 op가 무엇인지 알려준다. 스택을 어느 쪽으로 옮길지가 달라진다.
type intent int

const (
	intentEdit intent = iota // 보통 편집: 되돌리기 쌓기, 다시실행 비우기
	intentUndo               // 되돌리기 실행: 다시실행 쌓기
	intentRedo               // 다시실행 실행: 되돌리기 쌓기
)

type undoStack struct {
	undo    []*erd.Op
	redo    []*erd.Op
	touched time.Time
}

type undoStore struct {
	mu     sync.Mutex
	stacks map[string]*undoStack // "docID\x00userID"
}

func newUndoStore() *undoStore {
	return &undoStore{stacks: map[string]*undoStack{}}
}

func undoKey(docID, userID string) string { return docID + "\x00" + userID }

// record는 방금 적용된 편집의 역연산을 갈무리한다.
func (s *undoStore) record(docID, userID string, inverse *erd.Op, in intent) {
	if userID == "" || inverse == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked()

	key := undoKey(docID, userID)
	st := s.stacks[key]
	if st == nil {
		st = &undoStack{}
		s.stacks[key] = st
	}
	st.touched = time.Now()

	switch in {
	case intentUndo:
		st.redo = push(st.redo, inverse)
	default:
		st.undo = push(st.undo, inverse)
		if in == intentEdit {
			// 되돌린 뒤에 새로 편집하면 다시실행할 미래는 없어진다.
			// 남겨 두면 한참 전에 되돌린 것이 엉뚱한 시점에 되살아난다.
			st.redo = nil
		}
	}
}

// pop은 적용할 되돌리기(또는 다시실행) op를 꺼낸다. 없으면 nil이다.
func (s *undoStore) pop(docID, userID string, redo bool) *erd.Op {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked()

	st := s.stacks[undoKey(docID, userID)]
	if st == nil {
		return nil
	}
	st.touched = time.Now()
	if redo {
		op, rest := popLast(st.redo)
		st.redo = rest
		return op
	}
	op, rest := popLast(st.undo)
	st.undo = rest
	return op
}

// state는 버튼을 켤지 끌지다.
func (s *undoStore) state(docID, userID string) (canUndo, canRedo bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.stacks[undoKey(docID, userID)]
	if st == nil {
		return false, false
	}
	return len(st.undo) > 0, len(st.redo) > 0
}

// clear는 되돌릴 수 없는 편집이 들어왔을 때 스택을 비운다.
//
// 비우지 않으면 그 다음 되돌리기가 "그 편집"이 아니라 그 이전 편집을 되돌린다.
// 사용자가 보기에는 엉뚱한 것이 사라지는 셈이라, 아무것도 되돌리지 않는 편이 낫다.
func (s *undoStore) clear(docID, userID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.stacks, undoKey(docID, userID))
}

func (s *undoStore) pruneLocked() {
	cutoff := time.Now().Add(-undoTTL)
	for key, st := range s.stacks {
		if st.touched.Before(cutoff) {
			delete(s.stacks, key)
		}
	}
}

func push(stack []*erd.Op, op *erd.Op) []*erd.Op {
	stack = append(stack, op)
	if len(stack) > undoDepth {
		stack = stack[len(stack)-undoDepth:]
	}
	return stack
}

func popLast(stack []*erd.Op) (*erd.Op, []*erd.Op) {
	if len(stack) == 0 {
		return nil, stack
	}
	return stack[len(stack)-1], stack[:len(stack)-1]
}

// ---------- 방과 클라이언트 쪽 ----------

// recordUndo는 적용된 편집의 역연산을 그 사람의 스택에 넣는다.
//
// 역연산은 종류별 규칙이 아니라 적용 전후의 비교로 만든다(erd.Diff). 이유는
// internal/erd/undo.go에 적어 두었다.
func (r *room) recordUndo(prev, next *erd.Document, op *erd.Op, in intent) {
	if op.Actor == "" {
		return
	}
	// SQL 불러오기는 문서를 통째로 갈아엎는다. 비교로 역연산을 만들 수는 있지만
	// payload가 문서 크기만 하므로, 그것을 30개까지 들고 있는 대신 되돌리기를 포기한다.
	// 대신 스택을 비운다 — 그러지 않으면 다음 Ctrl+Z가 그 이전 편집을 되돌려,
	// 사용자가 보기에는 엉뚱한 것이 사라진다.
	if op.Kind == erd.OpSchemaImport {
		r.hub.undos.clear(r.id, op.Actor)
		return
	}
	r.hub.undos.record(r.id, op.Actor, erd.Diff(next, prev), in)
}

// UndoState는 이 사람이 지금 되돌리기·다시실행을 할 수 있는지다.
func (h *Hub) UndoState(docID, userID string) (canUndo, canRedo bool) {
	return h.undos.state(docID, userID)
}

func (c *Client) undoStateMessage() []byte {
	canUndo, canRedo := c.room.hub.undos.state(c.room.id, c.p.UserID)
	return mustJSON(map[string]any{
		"type": "undo_state", "canUndo": canUndo, "canRedo": canRedo,
	})
}

func (c *Client) sendUndoState() {
	c.enqueue(c.undoStateMessage())
}

// handleUndo는 되돌리기(또는 다시실행)를 적용한다.
func (c *Client) handleUndo(ctx context.Context, redo bool) error {
	label := "되돌릴"
	if redo {
		label = "다시실행할"
	}
	if !c.p.CanEdit {
		c.sendError("이 문서를 편집할 권한이 없습니다")
		return nil
	}
	r := c.room
	op := r.hub.undos.pop(r.id, c.p.UserID, redo)
	if op == nil {
		c.sendError(label + " 편집이 없습니다")
		return nil
	}
	// 매번 새 ID를 붙인다. 되돌리기는 새로운 편집으로 기록되며, 같은 ID를 다시
	// 쓰면 재전송으로 오인되어 조용히 무시된다.
	op.ID = uuid.NewString()
	op.Actor, op.ActorName = c.p.UserID, c.p.UserName

	in := intentUndo
	if redo {
		in = intentRedo
	}
	applied, rejectErr, err := r.submit(ctx, op, in)
	if err != nil {
		r.hub.log.Error("ERD 되돌리기 저장 실패", "doc", r.id, "error", err)
		c.sendError("되돌리지 못했습니다: " + err.Error())
		return nil
	}
	if rejectErr != nil {
		// 그 사이에 다른 사람이 대상을 지웠거나 바꿔 놓았다. 스택에 되돌려 넣지
		// 않는다 — 이미 맞지 않는 복원이고, 다시 눌러도 같은 이유로 실패한다.
		c.sendError(label + " 수 없습니다: " + rejectErr.Reason)
		c.sendUndoState()
		return nil
	}
	if applied {
		r.broadcast(r.opMessage(op))
	}
	c.sendUndoState()
	return nil
}
