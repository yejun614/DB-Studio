package erdhub

import (
	"context"
	"testing"
)

// TestBusyHoldsOrder는 메타 DB가 바쁠 때 편집을 버리지 않고, 순서도 뒤집지 않는지 본다.
//
// 바쁨은 실패가 아니라 "조금 뒤에"다. 서버는 busy 로 알리고 그 묶음의 나머지 조각을 버리며,
// 클라이언트가 같은 편집을 처음부터 다시 보내면 처음 온 것처럼 적용한다. 예전에는 저장 실패로
// 알렸고, 화면은 문서를 통째로 다시 받아 방금 한 편집을 지웠다.
//
// 쓰기 잠금을 붙잡아 진짜 SQLITE_BUSY 를 만든다. busy_timeout(5초)을 다 기다리므로 느리다.
func TestBusyHoldsOrder(t *testing.T) {
	if testing.Short() {
		t.Skip("busy_timeout 을 기다리므로 -short 에서는 건너뛴다")
	}
	ctx, st, hub, docID := fixture(t)
	c := join(t, ctx, hub, docID, "c1", "홍길동", true)
	drain(c)

	lock, err := st.DB().Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lock.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		t.Fatal(err)
	}

	// 대기열이 커서 두 조각으로 나뉜 한 번의 전송(gen 1).
	first := `{"type":"ops","gen":1,"ops":[{"id":"t1","kind":"table.add","payload":{"name":"users"}}]}`
	second := `{"type":"ops","gen":1,"ops":[{"id":"c1","kind":"column.add","payload":{"table":"users","name":"x","type":"int"}}]}`
	if err := c.Handle(ctx, []byte(first)); err != nil {
		t.Fatal(err)
	}
	busy := recv(t, c, "busy")
	if busy["opId"] != "t1" {
		t.Fatalf("busy 가 가리키는 편집 %v, 기대 t1", busy["opId"])
	}
	// 앞 조각이 저장되지 않았으므로 뒤 조각은 적용하지 않는다(순서가 뒤집힌다).
	if err := c.Handle(ctx, []byte(second)); err != nil {
		t.Fatal(err)
	}

	if _, err := lock.ExecContext(context.Background(), `ROLLBACK`); err != nil {
		t.Fatal(err)
	}
	lock.Close()
	expectSilence(t, c)

	// 다시 보낸다: 확인받지 못한 편집 전부를 처음부터(gen 2).
	retry := `{"type":"ops","gen":2,"ops":[` +
		`{"id":"t1","kind":"table.add","payload":{"name":"users"}},` +
		`{"id":"c1","kind":"column.add","payload":{"table":"users","name":"x","type":"int"}}]}`
	if err := c.Handle(ctx, []byte(retry)); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"t1", "c1"} {
		msg := recv(t, c, "ops")
		ops := msg["ops"].([]any)
		if got := ops[0].(map[string]any)["id"]; got != want {
			t.Fatalf("적용 순서: %v, 기대 %s", got, want)
		}
	}
}
