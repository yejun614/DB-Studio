package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"dbstudio/internal/store"
)

// flakyLLM은 앞의 n번을 500으로 답하고 그다음부터 정상 응답을 준다.
type flakyLLM struct {
	srv     *httptest.Server
	mu      sync.Mutex
	calls   int
	fail    int
	replies []string
}

func newFlakyLLM(t *testing.T, fail int, replies []string) *flakyLLM {
	t.Helper()
	f := &flakyLLM{fail: fail, replies: replies}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		f.mu.Lock()
		f.calls++
		n := f.calls
		f.mu.Unlock()
		if n <= f.fail {
			// 상류의 일시적 실패. Ollama Cloud가 실제로 이렇게 답하는 것을 겪었다.
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `{"error":"Internal Server Error (ref: test)"}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		for _, line := range f.replies {
			fmt.Fprintf(w, "data: %s\n\n", line)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *flakyLLM) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func chatWith(t *testing.T, e *testEnv, c *client, base string) string {
	t.Helper()
	key := "sk-test"
	if _, err := e.st.CreateAIProvider(context.Background(), store.SaveAIProviderParams{
		Name: "flaky", Provider: "openai", BaseURL: base,
		DefaultModel: "m", APIKey: &key, Enabled: true, IsDefault: true,
	}); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	status, body := c.do("POST", "/api/v1/ai/sessions", map[string]any{"title": "재시도"})
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

// 일시적인 5xx 하나로 차례가 죽으면 안 된다.
//
// 이 테스트가 생긴 이유: 툴을 쓰는 대화는 한 차례에 프로바이더를 여러 번 부른다.
// 실제로 Ollama Cloud가 같은 요청에 절반쯤 500을 냈고, 그때마다 그 차례에 조회하고
// 고친 것이 전부 헛일이 됐다.
func TestTransientFailureIsRetried(t *testing.T) {
	e := newTestEnv(t)
	c := login(t, e, "alice")
	fake := newFlakyLLM(t, 1, []string{
		`{"choices":[{"delta":{"content":"다시 해서 됐습니다."}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		"[DONE]",
	})

	raw := chatWith(t, e, c, fake.srv.URL)

	if !strings.Contains(raw, "다시 해서 됐습니다") {
		t.Errorf("재시도하지 않았습니다:\n%s", raw)
	}
	if strings.Contains(raw, "event: error") {
		t.Errorf("일시적 실패가 오류로 올라갔습니다:\n%s", raw)
	}
	if n := fake.count(); n != 2 {
		t.Errorf("프로바이더 호출 %d회, 2회여야 합니다", n)
	}
}

// 계속 실패하면 결국 오류로 알린다. 조용히 삼키면 안 된다.
func TestPersistentFailureStillReported(t *testing.T) {
	e := newTestEnv(t)
	c := login(t, e, "alice")
	fake := newFlakyLLM(t, 99, nil)

	raw := chatWith(t, e, c, fake.srv.URL)

	if !strings.Contains(raw, "event: error") {
		t.Errorf("계속 실패했는데 오류가 없습니다:\n%s", raw)
	}
	// 처음 한 번 + 재시도 두 번.
	if n := fake.count(); n != 1+retryAttempts {
		t.Errorf("프로바이더 호출 %d회, %d회여야 합니다", n, 1+retryAttempts)
	}
}

// 4xx는 다시 하지 않는다. 요청이 잘못된 것이라 같은 답이 온다.
func TestClientErrorIsNotRetried(t *testing.T) {
	e := newTestEnv(t)
	c := login(t, e, "alice")

	var calls int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		mu.Lock()
		calls++
		mu.Unlock()
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":{"message":"모델 이름이 틀렸습니다"}}`)
	}))
	defer srv.Close()

	raw := chatWith(t, e, c, srv.URL)
	if !strings.Contains(raw, "event: error") {
		t.Errorf("오류가 올라오지 않았습니다:\n%s", raw)
	}
	mu.Lock()
	n := calls
	mu.Unlock()
	if n != 1 {
		t.Errorf("4xx 를 %d회 시도했습니다. 한 번이어야 합니다", n)
	}
}

// 글자가 이미 나간 뒤에는 다시 하지 않는다.
//
// 다시 하면 같은 말이 두 번 보이고, 이미 실행된 툴이 두 번 실행된다.
func TestNoRetryAfterOutputStarted(t *testing.T) {
	e := newTestEnv(t)
	c := login(t, e, "alice")

	var calls int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		mu.Lock()
		calls++
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		// 글자를 흘린 **뒤에** 스트림 안에서 오류를 낸다.
		for _, line := range []string{
			`{"choices":[{"delta":{"content":"여기까지 왔"}}]}`,
			`{"error":{"message":"상류가 끊겼습니다"}}`,
		} {
			fmt.Fprintf(w, "data: %s\n\n", line)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer srv.Close()

	raw := chatWith(t, e, c, srv.URL)
	mu.Lock()
	n := calls
	mu.Unlock()
	if n != 1 {
		t.Errorf("글자가 나간 뒤에 %d회 시도했습니다. 한 번이어야 합니다", n)
	}
	// 나간 글자는 한 번만 보여야 한다.
	if c := strings.Count(raw, "여기까지 왔"); c != 1 {
		t.Errorf("같은 글자가 %d번 나갔습니다", c)
	}
	_ = json.Valid
}
