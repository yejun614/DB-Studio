package erdhub

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"dbstudio/internal/crypto"
	"dbstudio/internal/erd"
	"dbstudio/internal/model"
	"dbstudio/internal/store"
)

// ---------- 테스트 하네스 ----------

func fixture(t *testing.T) (context.Context, *store.Store, *Hub, string) {
	t.Helper()
	ctx := context.Background()
	box, err := crypto.NewSecretBox(make([]byte, 32))
	if err != nil {
		t.Fatalf("secret box: %v", err)
	}
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "hub.db"), box)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	// 이 스토어에 만든 사용자 캐시를 시험이 끝날 때 지운다.
	//
	// 캐시 키가 스토어 **포인터**라서, 시험이 끝나 회수된 주소에 다음 스토어가
	// 그대로 앉으면 앞 시험의 사용자 ID를 돌려준다. 그 ID는 새 DB에 없으므로
	// erd_ops.actor_id 외래키가 깨지고, 증상은 "가끔 다른 시험이 실패한다"가 된다.
	t.Cleanup(func() { forgetUsers(st) })

	pj, err := st.CreateProject(ctx, store.SaveProjectParams{Name: "테스트 프로젝트"})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	pw := "pw"
	_, conn, err := st.CreateServerWithDatabase(ctx,
		store.SaveServerParams{
			Name: "c", Kind: model.KindPostgres, DefaultEnvironment: model.EnvDev,
			Host: "h", Port: 1, Options: model.Options{}, Tags: []string{},
			Enabled: true, Password: &pw,
		},
		store.SaveConnectionParams{
			ProjectID: pj.ID,
			Name:      "c", Environment: model.EnvDev, DatabaseName: "d",
			Tags: []string{}, Enabled: true,
		})
	if err != nil {
		t.Fatalf("create connection: %v", err)
	}

	doc := erd.NewDocument("doc1", "초안", conn.ID, "postgres")
	if err := st.CreateERDDocument(ctx, doc, "", "", nil); err != nil {
		t.Fatalf("create doc: %v", err)
	}

	// 테스트 로그는 버린다. 경고 경로를 일부러 타는 테스트가 있어 출력이 시끄럽다.
	hub := New(st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return ctx, st, hub, "doc1"
}

// userIDs는 이름 → 실제 사용자 ID다. erd_ops.actor_id / erd_chat_messages.user_id가
// users를 참조하므로, 없는 ID를 쓰면 외래키 위반으로 저장이 실패한다.
var userIDs = map[string]string{}
var userMu sync.Mutex

func userKeyPrefix(st *store.Store) string { return fmt.Sprintf("%p/", st) }

func forgetUsers(st *store.Store) {
	userMu.Lock()
	defer userMu.Unlock()
	prefix := userKeyPrefix(st)
	for key := range userIDs {
		if strings.HasPrefix(key, prefix) {
			delete(userIDs, key)
		}
	}
}

func ensureUser(t *testing.T, ctx context.Context, st *store.Store, name string) string {
	t.Helper()
	userMu.Lock()
	defer userMu.Unlock()
	key := userKeyPrefix(st) + name
	if id, ok := userIDs[key]; ok {
		return id
	}
	u, err := st.CreateUser(ctx, store.CreateUserParams{
		Username: name, DisplayName: name, Role: model.RoleMember, PasswordHash: "x",
	})
	if err != nil {
		t.Fatalf("create user %s: %v", name, err)
	}
	userIDs[key] = u.ID
	return u.ID
}

func join(t *testing.T, ctx context.Context, hub *Hub, docID, clientID, name string, canEdit bool) *Client {
	t.Helper()
	c, err := hub.Join(ctx, docID, clientID, Participant{
		UserID: ensureUser(t, ctx, hub.st, name), UserName: name, CanEdit: canEdit,
	})
	if err != nil {
		t.Fatalf("join %s: %v", name, err)
	}
	t.Cleanup(c.Leave)
	return c
}

// recv는 다음 메시지를 기다린다. 종류를 지정하면 그 종류가 나올 때까지 건너뛴다
// (프레즌스 브로드캐스트가 섞여 오기 때문이다).
//
// 기다리지 않은 error 메시지가 오면 즉시 실패한다. 조용히 건너뛰면 모든 실패가
// "시간 초과"로만 보여서 원인을 찾을 수 없다.
func recv(t *testing.T, c *Client, wantType string) map[string]any {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case raw := <-c.Out():
			var msg map[string]any
			if err := json.Unmarshal(raw, &msg); err != nil {
				t.Fatalf("메시지 해석 실패: %v (%s)", err, raw)
			}
			if wantType == "" || msg["type"] == wantType {
				return msg
			}
			if msg["type"] == "error" {
				t.Fatalf("%q 를 기다리는 중 서버 오류를 받았습니다: %v", wantType, msg["message"])
			}
		case <-deadline:
			t.Fatalf("%q 메시지를 기다리다 시간이 초과되었습니다", wantType)
		}
	}
}

// drain은 지금까지 쌓인 메시지를 비운다.
func drain(c *Client) {
	for {
		select {
		case <-c.Out():
		default:
			return
		}
	}
}

// expectSilence는 "더 처리되지 않았다"를 확인한다.
//
// undo_state는 편집 결과가 아니라 버튼 상태 알림이라 무시한다 — 이것까지 실패로
// 보면 되돌리기 기능을 넣는 순간 재전송 시험이 이유 없이 깨진다.
func expectSilence(t *testing.T, c *Client) {
	t.Helper()
	deadline := time.After(250 * time.Millisecond)
	for {
		select {
		case raw := <-c.Out():
			var msg map[string]any
			_ = json.Unmarshal(raw, &msg)
			if msg["type"] == "undo_state" {
				continue
			}
			t.Errorf("재전송이 다시 처리되었습니다: %s", raw)
			return
		case <-deadline:
			return
		}
	}
}

func sendOp(t *testing.T, ctx context.Context, c *Client, id string, kind erd.Kind, payload string) {
	t.Helper()
	msg := fmt.Sprintf(`{"type":"ops","ops":[{"id":%q,"kind":%q,"payload":%s}]}`, id, kind, payload)
	if err := c.Handle(ctx, []byte(msg)); err != nil {
		t.Fatalf("handle ops: %v", err)
	}
}

// ---------- 테스트 ----------

func TestJoinSendsInitState(t *testing.T) {
	ctx, _, hub, docID := fixture(t)
	c := join(t, ctx, hub, docID, "c1", "홍길동", true)

	init := recv(t, c, "init")
	if init["seq"].(float64) != 0 {
		t.Errorf("seq = %v", init["seq"])
	}
	you := init["you"].(map[string]any)
	if you["userName"] != "홍길동" || you["canEdit"] != true {
		t.Errorf("you = %+v", you)
	}
	if you["color"] == "" {
		t.Error("참여자 색이 비어 있습니다")
	}
	if init["document"] == nil {
		t.Error("문서가 없습니다")
	}
	if _, ok := init["chat"]; !ok {
		t.Error("채팅 이력 필드가 없습니다")
	}
}

// 편집은 보낸 사람에게도 브로드캐스트된다. 자기 op를 특별 취급하지 않으므로
// 클라이언트의 정합 경로가 하나뿐이다.
func TestOpBroadcastToAllIncludingSender(t *testing.T) {
	ctx, _, hub, docID := fixture(t)
	a := join(t, ctx, hub, docID, "c1", "A", true)
	b := join(t, ctx, hub, docID, "c2", "B", true)
	drain(a)
	drain(b)

	sendOp(t, ctx, a, "op1", erd.OpTableAdd, `{"name":"users","withId":true}`)

	for _, c := range []*Client{a, b} {
		msg := recv(t, c, "ops")
		ops := msg["ops"].([]any)
		if len(ops) != 1 {
			t.Fatalf("op 수 = %d", len(ops))
		}
		op := ops[0].(map[string]any)
		if op["id"] != "op1" {
			t.Errorf("op id = %v", op["id"])
		}
		if op["seq"].(float64) != 1 {
			t.Errorf("seq = %v", op["seq"])
		}
		if op["actorName"] != "A" {
			t.Errorf("actorName = %v (서버가 채워야 합니다)", op["actorName"])
		}
	}
}

// 거부는 보낸 사람에게만 가고, 최신 상태를 함께 보내 낙관적 적용을 정합시킨다.
func TestRejectGoesOnlyToSenderWithState(t *testing.T) {
	ctx, _, hub, docID := fixture(t)
	a := join(t, ctx, hub, docID, "c1", "A", true)
	b := join(t, ctx, hub, docID, "c2", "B", true)
	drain(a)
	drain(b)

	sendOp(t, ctx, a, "bad1", erd.OpColumnAdd, `{"table":"nope","name":"x","type":"int"}`)

	msg := recv(t, a, "reject")
	if msg["code"] != "not_found" {
		t.Errorf("code = %v", msg["code"])
	}
	if msg["opId"] != "bad1" {
		t.Errorf("opId = %v", msg["opId"])
	}
	if msg["document"] == nil {
		t.Error("거부 메시지에 최신 상태가 없습니다 — 클라이언트가 되돌릴 방법이 없습니다")
	}
	// B는 아무것도 받지 않아야 한다.
	select {
	case raw := <-b.Out():
		t.Errorf("다른 참여자에게 메시지가 갔습니다: %s", raw)
	case <-time.After(200 * time.Millisecond):
	}
}

// 읽기 전용 참여자는 편집할 수 없지만 채팅은 할 수 있어야 한다.
// 리뷰어가 의견을 남기는 것이 리뷰의 목적이다.
func TestReadOnlyParticipant(t *testing.T) {
	ctx, _, hub, docID := fixture(t)
	viewer := join(t, ctx, hub, docID, "c1", "리뷰어", false)
	drain(viewer)

	sendOp(t, ctx, viewer, "op1", erd.OpTableAdd, `{"name":"users"}`)
	msg := recv(t, viewer, "error")
	if msg["message"] == nil || msg["message"] == "" {
		t.Error("권한 오류 메시지가 없습니다")
	}

	if err := viewer.Handle(ctx, []byte(`{"type":"chat","body":"이 테이블 이름이 맞나요?"}`)); err != nil {
		t.Fatalf("chat: %v", err)
	}
	chat := recv(t, viewer, "chat")
	m := chat["message"].(map[string]any)
	if m["body"] != "이 테이블 이름이 맞나요?" {
		t.Errorf("채팅 = %+v", m)
	}
}

// 동시 편집의 핵심 검증: 여러 클라이언트가 동시에 op를 보내도
// seq가 겹치지 않고, 저장된 op-log를 재생한 결과가 메모리 상태와 같아야 한다.
func TestConcurrentOpsGetUniqueSeq(t *testing.T) {
	ctx, st, hub, docID := fixture(t)

	const clients, opsEach = 4, 15
	cs := make([]*Client, clients)
	for i := range cs {
		cs[i] = join(t, ctx, hub, docID, fmt.Sprintf("c%d", i), fmt.Sprintf("U%d", i), true)
	}
	// 송신 버퍼가 넘치지 않게 계속 비워준다. 실제 환경에서는 쓰기 펌프가 이 역할을 한다.
	stop := make(chan struct{})
	var drained sync.WaitGroup
	for _, c := range cs {
		drained.Add(1)
		go func(c *Client) {
			defer drained.Done()
			for {
				select {
				case <-c.Out():
				case <-stop:
					return
				}
			}
		}(c)
	}

	var wg sync.WaitGroup
	for i, c := range cs {
		wg.Add(1)
		go func(i int, c *Client) {
			defer wg.Done()
			for j := 0; j < opsEach; j++ {
				name := fmt.Sprintf("t_%d_%d", i, j)
				msg := fmt.Sprintf(`{"type":"ops","ops":[{"id":%q,"kind":"table.add","payload":{"name":%q}}]}`,
					name, name)
				if err := c.Handle(ctx, []byte(msg)); err != nil {
					t.Errorf("handle: %v", err)
					return
				}
			}
		}(i, c)
	}
	wg.Wait()
	close(stop)
	drained.Wait()

	// 저장된 op의 seq가 1..N으로 빈틈없이 유일해야 한다.
	ops, err := st.ListERDOps(ctx, docID, 0)
	if err != nil {
		t.Fatalf("list ops: %v", err)
	}
	if len(ops) != clients*opsEach {
		t.Fatalf("저장된 op 수 = %d, 기대값 %d", len(ops), clients*opsEach)
	}
	for i, op := range ops {
		if op.Seq != int64(i+1) {
			t.Fatalf("op %d 의 seq = %d (빈틈 또는 중복)", i, op.Seq)
		}
	}

	// 재생 결과가 메모리 상태와 같아야 한다. 다르면 새로고침만으로 문서가 달라진다.
	reloaded, err := st.GetERDDocument(ctx, docID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := len(reloaded.Schema.Tables); got != clients*opsEach {
		t.Errorf("재생된 테이블 수 = %d, 기대값 %d", got, clients*opsEach)
	}
	live := hub.Participants(docID)
	if len(live) != clients {
		t.Errorf("참여자 수 = %d", len(live))
	}
}

// 확인받지 못한 op를 재전송하면 두 번 적용되어서는 안 된다.
func TestOpResendIsIdempotent(t *testing.T) {
	ctx, st, hub, docID := fixture(t)
	a := join(t, ctx, hub, docID, "c1", "A", true)
	drain(a)

	sendOp(t, ctx, a, "same", erd.OpColumnAdd, `{"table":"users","name":"x","type":"int"}`)
	recv(t, a, "reject") // users 테이블이 없으므로 거부된다

	sendOp(t, ctx, a, "op-add", erd.OpTableAdd, `{"name":"users"}`)
	recv(t, a, "ops")
	// 같은 op를 다시 보낸다.
	sendOp(t, ctx, a, "op-add", erd.OpTableAdd, `{"name":"users"}`)

	// 두 번째는 조용히 무시되어야 한다 (브로드캐스트도, 오류도 없다).
	expectSilence(t, a)
	reloaded, err := st.GetERDDocument(ctx, docID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(reloaded.Schema.Tables) != 1 {
		t.Errorf("테이블 수 = %d (재전송이 두 번 적용되었습니다)", len(reloaded.Schema.Tables))
	}
}

// 재접속 시나리오: 방이 접힌 뒤 다시 만들어져도 재전송을 걸러내야 한다.
// 네트워크가 끊긴 클라이언트는 확인받지 못한 op를 다시 보내는데, 그 사이
// 마지막 참여자가 나가 방이 사라졌을 수 있다.
func TestOpResendAfterRoomRebuild(t *testing.T) {
	ctx, st, hub, docID := fixture(t)
	a := join(t, ctx, hub, docID, "c1", "A", true)
	drain(a)
	sendOp(t, ctx, a, "pending-op", erd.OpTableAdd, `{"name":"users"}`)
	recv(t, a, "ops")
	a.Leave() // 방이 접힌다

	if counts := hub.ActiveCounts(); len(counts) != 0 {
		t.Fatalf("방이 남아 있습니다: %+v", counts)
	}

	// 같은 사람이 다시 들어와 확인받지 못했다고 생각한 op를 재전송한다.
	b := join(t, ctx, hub, docID, "c2", "A", true)
	drain(b)
	sendOp(t, ctx, b, "pending-op", erd.OpTableAdd, `{"name":"users"}`)

	expectSilence(t, b)
	reloaded, err := st.GetERDDocument(ctx, docID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(reloaded.Schema.Tables) != 1 {
		t.Errorf("테이블 수 = %d (재전송이 두 번 적용되었습니다)", len(reloaded.Schema.Tables))
	}
}

// 놓친 op가 있는 클라이언트는 그 op만 받아 따라잡을 수 있어야 한다.
func TestResyncSendsMissingOps(t *testing.T) {
	ctx, _, hub, docID := fixture(t)
	a := join(t, ctx, hub, docID, "c1", "A", true)
	drain(a)

	sendOp(t, ctx, a, "o1", erd.OpTableAdd, `{"name":"t1"}`)
	sendOp(t, ctx, a, "o2", erd.OpTableAdd, `{"name":"t2"}`)
	sendOp(t, ctx, a, "o3", erd.OpTableAdd, `{"name":"t3"}`)
	drain(a)

	// seq 1까지만 봤다고 알린다.
	if err := a.Handle(ctx, []byte(`{"type":"resync","sinceSeq":1}`)); err != nil {
		t.Fatalf("resync: %v", err)
	}
	msg := recv(t, a, "ops")
	ops := msg["ops"].([]any)
	if len(ops) != 2 {
		t.Fatalf("보충된 op 수 = %d, 기대값 2", len(ops))
	}
	if ops[0].(map[string]any)["seq"].(float64) != 2 {
		t.Errorf("첫 보충 op seq = %v", ops[0].(map[string]any)["seq"])
	}
}

// 압축으로 op가 사라졌으면 상태 전체를 보내야 한다.
func TestResyncFallsBackToFullState(t *testing.T) {
	ctx, st, hub, docID := fixture(t)
	a := join(t, ctx, hub, docID, "c1", "A", true)
	drain(a)
	sendOp(t, ctx, a, "o1", erd.OpTableAdd, `{"name":"t1"}`)
	sendOp(t, ctx, a, "o2", erd.OpTableAdd, `{"name":"t2"}`)
	drain(a)

	// op를 모두 압축해 없앤다.
	doc, err := st.GetERDDocument(ctx, docID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if ok, err := st.CompactERDDocument(ctx, doc, 0); err != nil || !ok {
		t.Fatalf("compact: ok=%t err=%v", ok, err)
	}

	if err := a.Handle(ctx, []byte(`{"type":"resync","sinceSeq":0}`)); err != nil {
		t.Fatalf("resync: %v", err)
	}
	msg := recv(t, a, "state")
	if msg["document"] == nil {
		t.Error("상태 전체가 오지 않았습니다")
	}
	if msg["seq"].(float64) != 2 {
		t.Errorf("seq = %v", msg["seq"])
	}
}

func TestPresenceJoinLeaveAndCursor(t *testing.T) {
	ctx, _, hub, docID := fixture(t)
	a := join(t, ctx, hub, docID, "c1", "A", true)
	recv(t, a, "init")

	b := join(t, ctx, hub, docID, "c2", "B", true)
	recv(t, b, "init")

	// A는 B의 참여를 프레즌스로 알게 된다.
	msg := recv(t, a, "presence")
	people := msg["participants"].([]any)
	if len(people) != 2 {
		t.Fatalf("참여자 수 = %d", len(people))
	}
	// 목록 순서는 안정적이어야 한다 (맵 순회 순서를 그대로 쓰면 화면이 흔들린다).
	first := people[0].(map[string]any)["userName"]
	if first != "A" {
		t.Errorf("첫 참여자 = %v, 이름 순으로 정렬되어야 합니다", first)
	}

	// 커서 이동은 보낸 사람을 제외하고 전달된다.
	drain(a)
	drain(b)
	if err := b.Handle(ctx, []byte(`{"type":"presence","cursor":{"x":10,"y":20}}`)); err != nil {
		t.Fatalf("presence: %v", err)
	}
	cur := recv(t, a, "cursor")
	if cur["clientId"] != "c2" {
		t.Errorf("clientId = %v", cur["clientId"])
	}
	select {
	case raw := <-b.Out():
		t.Errorf("자기 커서가 자신에게 되돌아왔습니다: %s", raw)
	case <-time.After(150 * time.Millisecond):
	}

	// 선택 변경은 참여자 목록으로 전달된다 (커서보다 빈도가 낮다).
	if err := b.Handle(ctx, []byte(`{"type":"presence","selection":"users"}`)); err != nil {
		t.Fatalf("presence: %v", err)
	}
	sel := recv(t, a, "presence")
	found := false
	for _, p := range sel["participants"].([]any) {
		if p.(map[string]any)["selection"] == "users" {
			found = true
		}
	}
	if !found {
		t.Error("선택 상태가 전달되지 않았습니다")
	}

	// 이탈도 알려야 한다.
	drain(a)
	b.Leave()
	left := recv(t, a, "presence")
	if len(left["participants"].([]any)) != 1 {
		t.Errorf("이탈 후 참여자 수 = %d", len(left["participants"].([]any)))
	}
}

// 같은 사용자가 색을 유지해야 한다. 재접속마다 색이 바뀌면 "저 커서가 누구였는지"를 잃는다.
func TestColorIsStablePerClient(t *testing.T) {
	a := colorFor("user1client1")
	b := colorFor("user1client1")
	if a != b {
		t.Errorf("같은 씨앗에서 다른 색: %s vs %s", a, b)
	}
	if colorFor("user1c1") == colorFor("user2c1") {
		t.Log("서로 다른 사용자가 같은 색을 받았습니다 (팔레트 크기상 가능하지만 드물어야 합니다)")
	}
}

// 느린 소비자는 방 전체를 멈추지 않고 끊겨야 한다.
func TestSlowConsumerIsDropped(t *testing.T) {
	ctx, _, hub, docID := fixture(t)
	slow := join(t, ctx, hub, docID, "c1", "느림", true)
	fast := join(t, ctx, hub, docID, "c2", "빠름", true)

	// slow의 채널을 비우지 않고 버퍼를 넘긴다.
	for i := 0; i < sendBuffer+20; i++ {
		drain(fast)
		name := fmt.Sprintf("t%d", i)
		sendOp(t, ctx, fast, name, erd.OpTableAdd, `{"name":"`+name+`"}`)
	}

	select {
	case <-slow.Closed():
	case <-time.After(3 * time.Second):
		t.Fatal("느린 클라이언트가 정리되지 않았습니다")
	}

	// 빠른 클라이언트는 계속 동작해야 한다.
	drain(fast)
	sendOp(t, ctx, fast, "after", erd.OpTableAdd, `{"name":"after"}`)
	recv(t, fast, "ops")
}

// 마지막 참여자가 나가면 상태를 굳혀 다음 로딩을 싸게 만든다.
func TestLastLeaverCompacts(t *testing.T) {
	ctx, st, hub, docID := fixture(t)
	a := join(t, ctx, hub, docID, "c1", "A", true)
	drain(a)
	for i := 0; i < 3; i++ {
		name := fmt.Sprintf("t%d", i)
		sendOp(t, ctx, a, name, erd.OpTableAdd, `{"name":"`+name+`"}`)
	}
	drain(a)

	a.Leave()

	// 방이 접혔는지 확인한다.
	if counts := hub.ActiveCounts(); len(counts) != 0 {
		t.Errorf("방이 남아 있습니다: %+v", counts)
	}
	// 스냅샷이 굳었으므로, op를 모두 지워도 상태가 유지된다.
	doc, err := st.GetERDDocument(ctx, docID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(doc.Schema.Tables) != 3 {
		t.Errorf("테이블 수 = %d", len(doc.Schema.Tables))
	}
}

func TestCloseDocumentNotifiesClients(t *testing.T) {
	ctx, _, hub, docID := fixture(t)
	a := join(t, ctx, hub, docID, "c1", "A", true)
	drain(a)

	hub.CloseDocument(docID, "문서가 삭제되었습니다")

	msg := recv(t, a, "closed")
	if msg["reason"] != "문서가 삭제되었습니다" {
		t.Errorf("reason = %v", msg["reason"])
	}
	select {
	case <-a.Closed():
	case <-time.After(time.Second):
		t.Error("연결이 닫히지 않았습니다")
	}
}

// 없는 문서에 참여하려 하면 명확히 실패해야 한다.
func TestJoinMissingDocument(t *testing.T) {
	ctx, _, hub, _ := fixture(t)
	if _, err := hub.Join(ctx, "nope", "c1", Participant{UserName: "A", CanEdit: true}); err == nil {
		t.Fatal("없는 문서 참여가 성공했습니다")
	}
}

// 잘못된 메시지로 연결이 끊기거나 패닉이 나서는 안 된다.
func TestMalformedMessages(t *testing.T) {
	ctx, _, hub, docID := fixture(t)
	a := join(t, ctx, hub, docID, "c1", "A", true)
	drain(a)

	inputs := []string{
		``, `{`, `[]`, `null`, `"x"`, `{"type":""}`, `{"type":"nonsense"}`,
		`{"type":"ops"}`, `{"type":"ops","ops":[]}`,
		`{"type":"ops","ops":[{"kind":"table.add","payload":{"name":"x"}}]}`, // op id 없음
		`{"type":"chat","body":"   "}`,
		`{"type":"presence"}`,
		`{"type":"resync","sinceSeq":-5}`,
	}
	for _, in := range inputs {
		if err := a.Handle(ctx, []byte(in)); err != nil {
			t.Errorf("%q 에서 연결이 끊겼습니다: %v", in, err)
		}
		drain(a)
	}

	// 여전히 정상 동작해야 한다.
	sendOp(t, ctx, a, "ok", erd.OpTableAdd, `{"name":"users"}`)
	recv(t, a, "ops")
}

// 한 메시지에 과도한 op를 넣는 것은 거부한다.
func TestTooManyOpsRejected(t *testing.T) {
	ctx, _, hub, docID := fixture(t)
	a := join(t, ctx, hub, docID, "c1", "A", true)
	drain(a)

	ops := make([]string, 0, maxOpsPerMessage+1)
	for i := 0; i <= maxOpsPerMessage; i++ {
		ops = append(ops, fmt.Sprintf(`{"id":"o%d","kind":"table.add","payload":{"name":"t%d"}}`, i, i))
	}
	msg := `{"type":"ops","ops":[` + joinStrings(ops, ",") + `]}`
	if err := a.Handle(ctx, []byte(msg)); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if got := recv(t, a, "error"); got["message"] == nil {
		t.Error("오류 메시지가 없습니다")
	}
}

func joinStrings(parts []string, sep string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += sep
		}
		out += p
	}
	return out
}

// 채팅은 영속해야 한다 — "왜 이렇게 설계했는가"의 기록이기 때문이다.
func TestChatPersistsAcrossRooms(t *testing.T) {
	ctx, _, hub, docID := fixture(t)
	a := join(t, ctx, hub, docID, "c1", "A", true)
	drain(a)
	if err := a.Handle(ctx, []byte(`{"type":"chat","body":"주문 테이블을 분리하죠","targetKey":"orders"}`)); err != nil {
		t.Fatalf("chat: %v", err)
	}
	recv(t, a, "chat")
	a.Leave()

	// 새로 들어온 사람이 이력을 본다.
	b := join(t, ctx, hub, docID, "c2", "B", true)
	init := recv(t, b, "init")
	chat := init["chat"].([]any)
	if len(chat) != 1 {
		t.Fatalf("채팅 이력 수 = %d", len(chat))
	}
	m := chat[0].(map[string]any)
	if m["body"] != "주문 테이블을 분리하죠" || m["targetKey"] != "orders" {
		t.Errorf("메시지 = %+v", m)
	}
}

// 구조 문서에서는 스키마를 고치는 op가 통과하면 안 된다.
//
// 화이트리스트를 시험으로 고정하는 이유: 새 op 종류가 생겼을 때 기본이 "허용"이면
// 그것이 구조 화면에서 실제 DB와 화면을 갈라놓는 길이 되어도 아무도 모른다.
func TestStructureAllowsOnlyAnnotations(t *testing.T) {
	allowed := []erd.Kind{
		erd.OpTableMove,
		erd.OpNoteAdd, erd.OpNoteUpdate, erd.OpNoteDelete,
		erd.OpGroupAdd, erd.OpGroupUpdate, erd.OpGroupDelete,
	}
	for _, k := range allowed {
		if !allowedInStructure(k) {
			t.Errorf("%s 가 막혔습니다 — 구조 화면에서 정리는 할 수 있어야 합니다", k)
		}
	}
	blocked := []erd.Kind{
		erd.OpTableAdd, erd.OpTableUpdate, erd.OpTableDelete, erd.OpTableDuplicate,
		erd.OpColumnAdd, erd.OpColumnUpdate, erd.OpColumnDelete,
		erd.OpIndexAdd, erd.OpFKAdd, erd.OpPKSet, erd.OpEnumAdd,
		erd.OpDomainAdd, erd.OpSchemaImport,
	}
	for _, k := range blocked {
		if allowedInStructure(k) {
			t.Errorf("%s 가 구조 문서에서 통과합니다 — 실제 DB와 화면이 갈라집니다", k)
		}
	}
}
