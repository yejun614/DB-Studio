package erdhub

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"dbstudio/internal/erd"
	"dbstudio/internal/store"
)

// 되돌리기에서 가장 위험한 실수는 "마지막 편집"을 문서 단위로 잡는 것이다.
// 그러면 내가 Ctrl+Z를 눌렀을 때 남이 방금 한 일이 사라진다.

func undo(t *testing.T, ctx context.Context, c *Client) {
	t.Helper()
	if err := c.Handle(ctx, []byte(`{"type":"undo"}`)); err != nil {
		t.Fatalf("undo: %v", err)
	}
}

func redo(t *testing.T, ctx context.Context, c *Client) {
	t.Helper()
	if err := c.Handle(ctx, []byte(`{"type":"redo"}`)); err != nil {
		t.Fatalf("redo: %v", err)
	}
}

func tableNames(doc *erd.Document) []string {
	out := []string{}
	for _, t := range doc.Schema.Tables {
		out = append(out, t.Name)
	}
	return out
}

// reload는 저장된 문서를 다시 읽는다.
func reload(t *testing.T, ctx context.Context, st *store.Store, docID string) *erd.Document {
	t.Helper()
	doc, err := st.GetERDDocument(ctx, docID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	return doc
}

// sendBatch는 같은 묶음 이름을 단 op 여러 개를 한 메시지로 보낸다.
func sendBatch(t *testing.T, ctx context.Context, c *Client, batch string, ops ...[3]string) {
	t.Helper()
	parts := make([]string, 0, len(ops))
	for _, op := range ops {
		parts = append(parts, fmt.Sprintf(`{"id":%q,"kind":%q,"payload":%s,"batch":%q}`,
			op[0], op[1], op[2], batch))
	}
	msg := fmt.Sprintf(`{"type":"ops","ops":[%s]}`, strings.Join(parts, ","))
	if err := c.Handle(ctx, []byte(msg)); err != nil {
		t.Fatalf("handle ops: %v", err)
	}
}

// 스택 깊이는 op 수가 아니라 되돌리기 횟수다.
//
// op 수로 자르면 묶음의 앞부분만 잘려 나가, 되돌렸을 때 함께 옮긴 것의 절반만
// 제자리로 돌아온다. 아무도 만든 적 없는 배치가 남는 셈이다.
func TestUndoDepthCountsSteps(t *testing.T) {
	stack := []*erd.Op{}
	// 큰 묶음 하나(깊이보다 많은 op) + 낱개 편집 여러 개.
	for i := 0; i < undoDepth+10; i++ {
		stack = push(stack, &erd.Op{ID: fmt.Sprintf("b%d", i), Batch: "big"})
	}
	if got := steps(stack); got != 1 {
		t.Fatalf("묶음 하나의 되돌리기 횟수 = %d (1이어야 합니다)", got)
	}
	if len(stack) != undoDepth+10 {
		t.Errorf("묶음이 잘렸습니다: op %d개 남음", len(stack))
	}

	for i := 0; i < undoDepth; i++ {
		stack = push(stack, &erd.Op{ID: fmt.Sprintf("s%d", i)})
	}
	if got := steps(stack); got != undoDepth {
		t.Errorf("되돌리기 횟수 = %d, 기대값 %d", got, undoDepth)
	}
	// 가장 오래된 것(큰 묶음)이 통째로 밀려났어야 한다.
	for _, op := range stack {
		if op.Batch == "big" {
			t.Fatal("밀려난 묶음의 op가 남아 있습니다(반쪽 되돌리기가 됩니다)")
		}
	}
}

// 한 동작에서 나온 편집은 한 번에 되돌아가야 한다.
//
// 여러 카드를 골라 함께 끌면 카드마다 op가 하나씩 생긴다. 묶이지 않으면 Ctrl+Z가
// 카드를 하나씩 되돌려, 한 번 옮긴 것을 다섯 번 되돌려야 한다. 그동안 화면에는
// 아무도 만든 적 없는 중간 배치가 남는다 — 사람이 보기에 그것은 고장이다.
func TestUndoRestoresWholeBatch(t *testing.T) {
	ctx, st, hub, docID := fixture(t)
	c := join(t, ctx, hub, docID, "c1", "A", true)
	drain(c)

	for _, name := range []string{"users", "orders", "items"} {
		sendOp(t, ctx, c, "add-"+name, erd.OpTableAdd, fmt.Sprintf(`{"name":%q}`, name))
		recv(t, c, "ops")
	}
	// 셋을 같은 자리에서 시작시킨다(되돌린 뒤 확인할 기준점).
	sendBatch(t, ctx, c, "place",
		[3]string{"p1", string(erd.OpTableMove), `{"key":"users","x":10,"y":10}`},
		[3]string{"p2", string(erd.OpTableMove), `{"key":"orders","x":20,"y":20}`},
		[3]string{"p3", string(erd.OpTableMove), `{"key":"items","x":30,"y":30}`})
	drain(c)

	// 셋을 함께 옮긴다(한 묶음).
	sendBatch(t, ctx, c, "drag-1",
		[3]string{"m1", string(erd.OpTableMove), `{"key":"users","x":110,"y":10}`},
		[3]string{"m2", string(erd.OpTableMove), `{"key":"orders","x":120,"y":20}`},
		[3]string{"m3", string(erd.OpTableMove), `{"key":"items","x":130,"y":30}`})
	drain(c)

	undo(t, ctx, c)
	doc := reload(t, ctx, st, docID)
	for key, want := range map[string]float64{"users": 10, "orders": 20, "items": 30} {
		if got := doc.Layout[key].X; got != want {
			t.Errorf("%s x = %v, 기대값 %v (묶음이 한 번에 되돌아가지 않았습니다)", key, got, want)
		}
	}

	// 다시실행도 한 번에 되돌아온다.
	redo(t, ctx, c)
	doc = reload(t, ctx, st, docID)
	for key, want := range map[string]float64{"users": 110, "orders": 120, "items": 130} {
		if got := doc.Layout[key].X; got != want {
			t.Errorf("다시실행 후 %s x = %v, 기대값 %v", key, got, want)
		}
	}

	// 묶음 밖의 편집까지 함께 딸려 가면 안 된다. 한 번 더 되돌리면 이번에는
	// 묶음 이전의 배치(place)까지 돌아가야 한다 — 그 앞의 테이블 추가가 아니라.
	undo(t, ctx, c)
	undo(t, ctx, c)
	doc = reload(t, ctx, st, docID)
	if len(doc.Schema.Tables) != 3 {
		t.Errorf("테이블 수 = %d (묶음 되돌리기가 옆 편집까지 되돌렸습니다)", len(doc.Schema.Tables))
	}
}

func TestUndoOnlyTouchesMyEdit(t *testing.T) {
	ctx, st, hub, docID := fixture(t)
	a := join(t, ctx, hub, docID, "c1", "A", true)
	b := join(t, ctx, hub, docID, "c2", "B", true)
	drain(a)
	drain(b)

	sendOp(t, ctx, a, "a1", erd.OpTableAdd, `{"name":"users"}`)
	recv(t, a, "ops")
	sendOp(t, ctx, b, "b1", erd.OpTableAdd, `{"name":"orders"}`)
	recv(t, b, "ops")
	drain(a)
	drain(b)

	// A가 되돌린다. 사라져야 하는 것은 users뿐이다.
	undo(t, ctx, a)
	if msg := recv(t, a, "ops"); msg["document"] == nil {
		t.Fatal("되돌리기가 문서를 함께 보내지 않았습니다")
	}
	doc, err := st.GetERDDocument(ctx, docID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	got := tableNames(doc)
	if len(got) != 1 || got[0] != "orders" {
		t.Fatalf("되돌린 뒤 테이블 = %v, [orders]여야 합니다", got)
	}

	// B의 되돌리기는 B의 편집을 지운다.
	drain(b)
	undo(t, ctx, b)
	recv(t, b, "ops")
	doc, _ = st.GetERDDocument(ctx, docID)
	if len(doc.Schema.Tables) != 0 {
		t.Errorf("B가 되돌린 뒤 테이블 = %v", tableNames(doc))
	}
}

func TestRedoRestoresUndoneEdit(t *testing.T) {
	ctx, st, hub, docID := fixture(t)
	a := join(t, ctx, hub, docID, "c1", "A", true)
	drain(a)

	sendOp(t, ctx, a, "a1", erd.OpTableAdd, `{"name":"users"}`)
	recv(t, a, "ops")
	drain(a)

	undo(t, ctx, a)
	recv(t, a, "ops")
	if doc, _ := st.GetERDDocument(ctx, docID); len(doc.Schema.Tables) != 0 {
		t.Fatalf("되돌린 뒤에도 테이블이 남아 있습니다: %v", tableNames(doc))
	}

	drain(a)
	redo(t, ctx, a)
	recv(t, a, "ops")
	doc, _ := st.GetERDDocument(ctx, docID)
	if got := tableNames(doc); len(got) != 1 || got[0] != "users" {
		t.Fatalf("다시실행 뒤 테이블 = %v", got)
	}

	// 다시실행할 것이 더 없으면 알려야 한다.
	drain(a)
	redo(t, ctx, a)
	if msg := recv(t, a, "error"); msg["message"] == "" {
		t.Error("빈 다시실행에 이유가 없습니다")
	}
}

// 되돌린 뒤에 새로 편집하면 다시실행할 미래는 사라진다.
// 남겨 두면 한참 전에 되돌린 것이 엉뚱한 시점에 되살아난다.
func TestNewEditClearsRedo(t *testing.T) {
	ctx, st, hub, docID := fixture(t)
	a := join(t, ctx, hub, docID, "c1", "A", true)
	drain(a)

	sendOp(t, ctx, a, "a1", erd.OpTableAdd, `{"name":"users"}`)
	recv(t, a, "ops")
	undo(t, ctx, a)
	recv(t, a, "ops")

	sendOp(t, ctx, a, "a2", erd.OpTableAdd, `{"name":"orders"}`)
	recv(t, a, "ops")
	drain(a)

	redo(t, ctx, a)
	if msg := recv(t, a, "error"); msg["message"] == "" {
		t.Error("새 편집 뒤에도 다시실행이 남아 있습니다")
	}
	doc, _ := st.GetERDDocument(ctx, docID)
	if got := tableNames(doc); len(got) != 1 || got[0] != "orders" {
		t.Errorf("테이블 = %v", got)
	}
}

// 되돌릴 것이 없으면 조용히 무시하지 않고 이유를 말한다.
// 아무 반응이 없으면 사용자는 버튼이 고장 났다고 생각한다.
func TestUndoWithEmptyStack(t *testing.T) {
	ctx, _, hub, docID := fixture(t)
	a := join(t, ctx, hub, docID, "c1", "A", true)
	drain(a)

	undo(t, ctx, a)
	msg := recv(t, a, "error")
	if msg["message"] == "" {
		t.Error("빈 되돌리기에 이유가 없습니다")
	}
}

// 읽기 권한만 있는 사람은 되돌릴 수도 없다.
func TestUndoNeedsEditPermission(t *testing.T) {
	ctx, _, hub, docID := fixture(t)
	viewer := join(t, ctx, hub, docID, "c9", "V", false)
	drain(viewer)

	undo(t, ctx, viewer)
	if msg := recv(t, viewer, "error"); msg["message"] == "" {
		t.Error("권한 없는 되돌리기가 조용히 지나갔습니다")
	}
}

// 되돌리기 스택은 방보다 오래 산다 — 새로고침해도 방금 한 편집을 되돌릴 수 있어야 한다.
func TestUndoSurvivesRoomRebuild(t *testing.T) {
	ctx, st, hub, docID := fixture(t)
	a := join(t, ctx, hub, docID, "c1", "A", true)
	drain(a)
	sendOp(t, ctx, a, "a1", erd.OpTableAdd, `{"name":"users"}`)
	recv(t, a, "ops")
	a.Leave() // 마지막 참여자가 나가 방이 접힌다

	back := join(t, ctx, hub, docID, "c2", "A", true)
	init := recv(t, back, "init")
	if init["canUndo"] != true {
		t.Fatalf("재접속 init에 되돌리기 상태가 없습니다: %v", init)
	}
	drain(back)

	undo(t, ctx, back)
	recv(t, back, "ops")
	if doc, _ := st.GetERDDocument(ctx, docID); len(doc.Schema.Tables) != 0 {
		t.Errorf("재접속 후 되돌리기가 적용되지 않았습니다: %v", tableNames(doc))
	}
}

// 되돌리기가 남의 편집을 조용히 지워서는 안 된다.
//
// 내가 지운 테이블을 되살리는 사이에 다른 사람이 같은 이름으로 새 테이블을
// 만들었다면, 복원은 그것을 통째로 덮어쓴다. 그 사람 입장에서는 방금 만든 것이
// 이유 없이 사라진 것이므로, 되돌리기를 거절하고 이유를 말해야 한다.
func TestUndoRefusesToClobberOthers(t *testing.T) {
	ctx, st, hub, docID := fixture(t)
	a := join(t, ctx, hub, docID, "c1", "A", true)
	b := join(t, ctx, hub, docID, "c2", "B", true)
	drain(a)
	drain(b)

	sendOp(t, ctx, a, "a1", erd.OpTableAdd, `{"name":"users"}`)
	recv(t, a, "ops")
	sendOp(t, ctx, a, "a2", erd.OpTableDelete, `{"key":"users"}`)
	recv(t, a, "ops")
	// 그 사이에 B가 같은 이름으로 다른 테이블을 만들었다.
	sendOp(t, ctx, b, "b1", erd.OpTableAdd, `{"name":"users","comment":"B가 만든 것"}`)
	recv(t, b, "ops")
	drain(a)

	undo(t, ctx, a)
	msg := recv(t, a, "error")
	if msg["message"] == "" {
		t.Fatal("남의 편집을 덮어쓰는 되돌리기가 조용히 통과했습니다")
	}

	doc, _ := st.GetERDDocument(ctx, docID)
	if len(doc.Schema.Tables) != 1 || doc.Schema.Tables[0].Comment != "B가 만든 것" {
		t.Errorf("B의 테이블이 덮어써졌습니다: %+v", doc.Schema.Tables)
	}
}

// 내 편집만 되돌리는 것이지, 남이 그 사이에 같은 테이블을 고쳤다면 멈춘다.
func TestUndoStopsWhenTargetChanged(t *testing.T) {
	ctx, _, hub, docID := fixture(t)
	a := join(t, ctx, hub, docID, "c1", "A", true)
	b := join(t, ctx, hub, docID, "c2", "B", true)
	drain(a)
	drain(b)

	sendOp(t, ctx, a, "a1", erd.OpTableAdd, `{"name":"users"}`)
	recv(t, a, "ops")
	sendOp(t, ctx, a, "a2", erd.OpColumnAdd, `{"table":"users","name":"email","type":"text"}`)
	recv(t, a, "ops")
	// B가 같은 테이블에 컬럼을 하나 더 붙였다.
	sendOp(t, ctx, b, "b1", erd.OpColumnAdd, `{"table":"users","name":"name","type":"text"}`)
	recv(t, b, "ops")
	drain(a)

	// A가 컬럼 추가를 되돌리면 B의 컬럼까지 사라진다(복원은 테이블 통째 교체다).
	// 그래서 거절하고 이유를 말한다.
	undo(t, ctx, a)
	if msg := recv(t, a, "error"); msg["message"] == "" {
		t.Error("남의 편집이 섞인 테이블을 조용히 되돌렸습니다")
	}
}

// 여러 번 되돌리기. 내 편집이 겹겹이 쌓인 뒤에도 한 칸씩 정확히 물러나야 한다.
//
// 스택의 각 항목은 "그때 이 모습이어야 한다"를 함께 들고 있으므로, 순서대로
// 풀리지 않으면 두 번째 되돌리기부터 거절된다.
func TestUndoTwiceInOrder(t *testing.T) {
	ctx, st, hub, docID := fixture(t)
	a := join(t, ctx, hub, docID, "c1", "A", true)
	drain(a)

	sendOp(t, ctx, a, "a1", erd.OpTableAdd, `{"name":"users"}`)
	recv(t, a, "ops")
	sendOp(t, ctx, a, "a2", erd.OpColumnAdd, `{"table":"users","name":"email","type":"text"}`)
	recv(t, a, "ops")
	drain(a)

	undo(t, ctx, a) // 컬럼 추가를 되돌린다
	recv(t, a, "ops")
	doc, _ := st.GetERDDocument(ctx, docID)
	if len(doc.Schema.Tables) != 1 || len(doc.Schema.Tables[0].Columns) != 0 {
		t.Fatalf("첫 되돌리기 결과: %+v", doc.Schema.Tables)
	}

	drain(a)
	undo(t, ctx, a) // 테이블 추가를 되돌린다
	recv(t, a, "ops")
	doc, _ = st.GetERDDocument(ctx, docID)
	if len(doc.Schema.Tables) != 0 {
		t.Fatalf("두 번째 되돌리기 결과: %v", tableNames(doc))
	}

	// 다시실행 두 번이면 처음 자리로 돌아온다.
	drain(a)
	redo(t, ctx, a)
	recv(t, a, "ops")
	drain(a)
	redo(t, ctx, a)
	recv(t, a, "ops")
	doc, _ = st.GetERDDocument(ctx, docID)
	if len(doc.Schema.Tables) != 1 || len(doc.Schema.Tables[0].Columns) != 1 {
		t.Errorf("다시실행 두 번 뒤: %+v", doc.Schema.Tables)
	}
}
