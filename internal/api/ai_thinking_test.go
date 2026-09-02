package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"dbstudio/internal/store"
)

// aiChatRaw는 프로바이더를 붙이고 한 번 대화한 SSE 원문이다.
func aiChatRaw(t *testing.T, e *testEnv, c *client, replies [][]string) string {
	t.Helper()
	fake := newFakeLLM(t, replies)
	key := "sk-test"
	if _, err := e.st.CreateAIProvider(context.Background(), store.SaveAIProviderParams{
		Name: "fake", Provider: "openai", BaseURL: fake.srv.URL,
		DefaultModel: "gpt-4o", APIKey: &key, Enabled: true, IsDefault: true,
	}); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	status, body := c.do("POST", "/api/v1/ai/sessions", map[string]any{"title": "검사"})
	if status != http.StatusCreated {
		t.Fatalf("create session = %d: %v", status, body)
	}
	sess, _ := body["session"].(map[string]any)
	sessID, _ := sess["id"].(string)

	status, raw := c.doRaw("POST", "/api/v1/ai/sessions/"+sessID+"/chat",
		map[string]any{"message": "안녕"})
	if status != http.StatusOK {
		t.Fatalf("chat = %d: %s", status, raw)
	}
	return raw
}

// 생각은 화면까지 와야 한다.
//
// 예전에는 생각 델타를 읽는 곳이 아예 없어서 json.Unmarshal이 조용히 버렸다.
// 생각만 하고 끝난 차례는 **빈 답풍선**으로 보였다 — 오류도 안내도 없이.
func TestThinkingReachesTheBrowser(t *testing.T) {
	e := newTestEnv(t)
	c := login(t, e, "alice")

	raw := aiChatRaw(t, e, c, [][]string{{
		// Ollama는 reasoning, 다른 호환 서버는 reasoning_content를 쓴다. 둘 다 받는다.
		`{"choices":[{"delta":{"content":"","reasoning":"먼저 무엇을 물었는지 본다. "}}]}`,
		`{"choices":[{"delta":{"content":"","reasoning_content":"짧게 답하면 된다."}}]}`,
		`{"choices":[{"delta":{"content":"안녕하세요."}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		"[DONE]",
	}})

	if !strings.Contains(raw, "event: thinking") {
		t.Errorf("생각이 화면으로 가지 않았습니다:\n%s", raw)
	}
	if !strings.Contains(raw, "먼저 무엇을 물었는지") {
		t.Errorf("reasoning 델타가 빠졌습니다:\n%s", raw)
	}
	if !strings.Contains(raw, "짧게 답하면 된다") {
		t.Errorf("reasoning_content 델타가 빠졌습니다:\n%s", raw)
	}
	if !strings.Contains(raw, "안녕하세요") {
		t.Errorf("정작 답이 빠졌습니다:\n%s", raw)
	}

	// 생각은 이력에 저장하지 않는다. 저장하면 다음 차례의 문맥을 결론이 아닌
	// 글로 채우고, 새로고침하면 어차피 사라질 것을 DB에 남기게 된다.
	_, body := c.do("GET", "/api/v1/ai/sessions", nil)
	_ = body
	msgs, err := e.st.ListAIMessages(context.Background(), lastSessionID(t, e), 100)
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	for _, m := range msgs {
		if strings.Contains(m.Text, "먼저 무엇을 물었는지") {
			t.Error("생각이 대화 이력에 저장됐습니다")
		}
	}
}

// 길이 한계로 잘린 답은 잘렸다고 말해야 한다.
//
// 예전에는 finish_reason을 아무도 읽지 않았다. 그래서 문장 중간에서 끊긴 답이
// 끝까지 온 답과 화면에서 똑같아 보였고, 사람은 모델이 그렇게 답했다고 믿었다.
func TestLengthStopIsToldToTheUser(t *testing.T) {
	e := newTestEnv(t)
	c := login(t, e, "alice")

	raw := aiChatRaw(t, e, c, [][]string{{
		`{"choices":[{"delta":{"content":"잘린 부분(songs 나"}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"length"}]}`,
		"[DONE]",
	}})

	if !strings.Contains(raw, "event: notice") {
		t.Errorf("잘렸다는 안내가 없습니다:\n%s", raw)
	}
	if !strings.Contains(raw, "길이 한계") {
		t.Errorf("안내가 이유를 말하지 않습니다:\n%s", raw)
	}
}

// 끝났다는 표시 없이 닫힌 스트림도 그렇게 말해야 한다.
func TestIncompleteStreamIsToldToTheUser(t *testing.T) {
	e := newTestEnv(t)
	c := login(t, e, "alice")

	// finish_reason도 [DONE]도 없이 끊는다. 로컬 모델이 내려갔을 때의 모습이다.
	raw := aiChatRaw(t, e, c, [][]string{{
		`{"choices":[{"delta":{"content":"답을 쓰다가"}}]}`,
	}})

	if !strings.Contains(raw, "event: notice") {
		t.Errorf("끊겼다는 안내가 없습니다:\n%s", raw)
	}
	if !strings.Contains(raw, "연결이 끊겼습니다") {
		t.Errorf("안내가 이유를 말하지 않습니다:\n%s", raw)
	}
}

// 정상적으로 끝난 답에는 아무 안내도 붙지 않는다.
//
// 이 검사가 없으면 위의 두 안내를 넉넉하게 붙이는 쪽으로 흘러가고, 그러면
// 모든 답에 안내가 달려 정작 잘린 답에서 아무도 읽지 않게 된다.
func TestNormalTurnHasNoNotice(t *testing.T) {
	e := newTestEnv(t)
	c := login(t, e, "alice")

	raw := aiChatRaw(t, e, c, [][]string{{
		`{"choices":[{"delta":{"content":"다 썼습니다."}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		"[DONE]",
	}})

	if strings.Contains(raw, "event: notice") {
		t.Errorf("멀쩡한 답에 안내가 붙었습니다:\n%s", raw)
	}
}

func lastSessionID(t *testing.T, e *testEnv) string {
	t.Helper()
	list, err := e.st.ListAISessions(context.Background(), e.user.ID, true, 10)
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}
	if len(list) == 0 {
		t.Fatal("세션이 없습니다")
	}
	return list[0].ID
}
