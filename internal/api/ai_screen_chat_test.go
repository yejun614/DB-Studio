package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"dbstudio/internal/store"
)

// 화면 보고가 **모델에게 실제로 닿는지**를 확인한다.
//
// screenPrompt 만 시험하면 부족하다: 이 기능이 성립하려면 "화면이 알린다 → 브라우저가
// 함께 보낸다 → 핸들러가 시스템 프롬프트에 담는다 → 프로바이더 요청에 실린다"가 한 줄로
// 이어져야 하고, 그 사이 어디가 끊겨도 증상은 똑같다: 물어보면 모델이 "어느 테이블
// 말씀이신가요?"라고 되묻는다. 화면에서는 구별할 수 없는 실패다.
func TestChatSendsScreenToProvider(t *testing.T) {
	e := newTestEnv(t)
	c := login(t, e, "alice")

	fake := newFakeLLM(t, [][]string{
		{
			`{"choices":[{"delta":{"content":"orders 테이블을 보고 계시네요."}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
			"[DONE]",
		},
	})
	key := "sk-test"
	if _, err := e.st.CreateAIProvider(context.Background(), store.SaveAIProviderParams{
		Name: "fake", Provider: "openai", BaseURL: fake.srv.URL,
		DefaultModel: "gpt-4o", APIKey: &key, Enabled: true, IsDefault: true,
	}); err != nil {
		t.Fatalf("create provider: %v", err)
	}

	status, body := c.do("POST", "/api/v1/ai/sessions", map[string]any{"title": "질문"})
	if status != http.StatusCreated {
		t.Fatalf("create session = %d: %v", status, body)
	}
	sess, _ := body["session"].(map[string]any)
	sessID, _ := sess["id"].(string)

	status, raw := c.doRaw("POST", "/api/v1/ai/sessions/"+sessID+"/chat", map[string]any{
		"message": "이 테이블에 인덱스가 필요할까?",
		"screen": map[string]any{
			"path":  "/data?conn=abc123&table=orders",
			"label": "데이터",
			"detail": []string{
				"보고 있는 DB: shop-prod / shop (postgres, 운영)",
				"보고 있는 테이블: public.orders",
			},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("chat = %d: %s", status, raw)
	}
	if len(fake.requests) == 0 {
		t.Fatal("프로바이더가 호출되지 않았습니다")
	}

	system := systemTextOf(t, fake.requests[0])
	for _, want := range []string{"데이터", "public.orders", "shop-prod"} {
		if !strings.Contains(system, want) {
			t.Errorf("시스템 프롬프트에 화면 정보 %q 가 없습니다:\n%s", want, system)
		}
	}
	// 기본 지침을 덮어쓰면 안 된다. 덧붙이는 것이다.
	if !strings.Contains(system, "DB Studio의 어시스턴트") {
		t.Errorf("기본 지침이 사라졌습니다:\n%s", system)
	}
}

// 화면 정보를 보내지 않는 클라이언트도 그대로 동작해야 한다(옛 화면, 다른 도구).
func TestChatWithoutScreen(t *testing.T) {
	e := newTestEnv(t)
	c := login(t, e, "alice")

	fake := newFakeLLM(t, [][]string{
		{
			`{"choices":[{"delta":{"content":"네."}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
			"[DONE]",
		},
	})
	key := "sk-test"
	if _, err := e.st.CreateAIProvider(context.Background(), store.SaveAIProviderParams{
		Name: "fake", Provider: "openai", BaseURL: fake.srv.URL,
		DefaultModel: "gpt-4o", APIKey: &key, Enabled: true, IsDefault: true,
	}); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	status, body := c.do("POST", "/api/v1/ai/sessions", map[string]any{"title": "질문"})
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
	system := systemTextOf(t, fake.requests[0])
	if strings.Contains(system, "지금 사용자가 보고 있는 화면") {
		t.Errorf("화면 정보가 없는데 화면 문단이 붙었습니다:\n%s", system)
	}
}

// systemTextOf는 프로바이더 요청에서 시스템 메시지 글을 꺼낸다.
func systemTextOf(t *testing.T, req map[string]any) string {
	t.Helper()
	msgs, _ := req["messages"].([]any)
	for _, it := range msgs {
		m, _ := it.(map[string]any)
		if role, _ := m["role"].(string); role != "system" {
			continue
		}
		if s, ok := m["content"].(string); ok {
			return s
		}
	}
	t.Fatalf("시스템 메시지를 찾지 못했습니다: %+v", req)
	return ""
}
