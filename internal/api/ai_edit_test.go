package api

import (
	"context"
	"testing"

	"dbstudio/internal/ai"
	"dbstudio/internal/store"
)

// 말을 고쳐 다시 보내면 그 자리부터 다시 시작한다.
//
// 새 말을 뒤에 붙이는 것이 아니다. 옛 문답이 뒤에 남으면 대화는 있지도 않았던
// 흐름이 되고, 다음 요청의 문맥으로 그 옛 답이 그대로 모델에게 간다.
func TestTruncateAIMessagesFromReplacesTail(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()

	sess, err := e.st.CreateAISession(ctx, store.CreateAISessionParams{
		Title: "첫 질문입니다", UserID: e.user.ID,
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	add := func(role ai.Role, text string) int64 {
		t.Helper()
		m := &store.AIMessage{SessionID: sess.ID, Role: string(role), Text: text}
		if err := e.st.AddAIMessage(ctx, m); err != nil {
			t.Fatalf("add %s: %v", text, err)
		}
		return m.ID
	}
	first := add(ai.RoleUser, "첫 질문입니다")
	add(ai.RoleAssistant, "첫 답입니다")
	second := add(ai.RoleUser, "둘째 질문입니다")
	add(ai.RoleAssistant, "둘째 답입니다")

	// 둘째 말부터 지우면 첫 문답만 남는다.
	removed, left, err := e.st.TruncateAIMessagesFrom(ctx, sess.ID, second)
	if err != nil {
		t.Fatalf("truncate: %v", err)
	}
	if removed != 2 || left != 2 {
		t.Errorf("지운 것 %d개 / 남은 것 %d개, 기대 2 / 2", removed, left)
	}
	got, _ := e.st.ListAIMessages(ctx, sess.ID, 0)
	if len(got) != 2 || got[0].Text != "첫 질문입니다" || got[1].Text != "첫 답입니다" {
		t.Errorf("남은 대화 = %v", messageTexts(got))
	}

	// 첫 말부터 지우면 아무것도 남지 않는다 — 제목을 다시 정해야 한다는 신호다.
	_, left, err = e.st.TruncateAIMessagesFrom(ctx, sess.ID, first)
	if err != nil {
		t.Fatalf("truncate 2: %v", err)
	}
	if left != 0 {
		t.Errorf("남은 것 = %d개, 0개여야 합니다", left)
	}
}

// 남의 대화의 메시지는 가리킬 수 없다.
//
// 메시지 아이디는 앱 전체에서 증가하는 숫자라 추측하기 쉽다. 세션을 함께 확인하지
// 않으면 남의 대화를 지우는 통로가 된다.
func TestGetAIMessageIsScopedToSession(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()

	mine, err := e.st.CreateAISession(ctx, store.CreateAISessionParams{Title: "내 것", UserID: e.user.ID})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	theirs, err := e.st.CreateAISession(ctx, store.CreateAISessionParams{Title: "남의 것", UserID: e.user.ID})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	m := &store.AIMessage{SessionID: theirs.ID, Role: string(ai.RoleUser), Text: "남의 말"}
	if err := e.st.AddAIMessage(ctx, m); err != nil {
		t.Fatalf("add: %v", err)
	}

	if _, err := e.st.GetAIMessage(ctx, mine.ID, m.ID); err == nil {
		t.Error("다른 대화의 메시지를 읽었습니다")
	}
	if _, err := e.st.GetAIMessage(ctx, theirs.ID, m.ID); err != nil {
		t.Errorf("자기 대화의 메시지를 못 읽습니다: %v", err)
	}

	// 지우기도 그 대화 안에서만 듣는다.
	removed, _, err := e.st.TruncateAIMessagesFrom(ctx, mine.ID, m.ID)
	if err != nil {
		t.Fatalf("truncate: %v", err)
	}
	if removed != 0 {
		t.Errorf("다른 대화의 메시지 %d개를 지웠습니다", removed)
	}
}

func messageTexts(list []*store.AIMessage) []string {
	out := make([]string, 0, len(list))
	for _, m := range list {
		out = append(out, m.Text)
	}
	return out
}
