package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dbstudio/internal/store"
)

// 초안 대화의 왕복 전체를 확인한다.
//
// 조각별 시험(툴이 문서를 고친다, 세션이 문서에 매인다)만으로는 부족하다.
// 이 기능이 성립하려면 "이 세션은 ERD용이다 → 초안 툴만 넘긴다 → 모델이 부른 툴이
// 문서를 고친다 → 화면이 그 사실을 SSE로 본다"가 한 줄로 이어져야 하고, 그 사이
// 어디가 끊겨도 증상은 똑같다: 대화는 되는데 그림이 안 바뀐다.

// fakeLLMServer는 OpenAI 호환 스트림을 흉내낸다. 순서대로 한 번씩 응답한다.
type fakeLLMServer struct {
	srv      *httptest.Server
	replies  [][]string // 요청 n번째에 내보낼 SSE data 줄들
	requests []map[string]any
}

func newFakeLLM(t *testing.T, replies [][]string) *fakeLLMServer {
	f := &fakeLLMServer{replies: replies}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		req := map[string]any{}
		_ = json.Unmarshal(raw, &req)
		n := len(f.requests)
		f.requests = append(f.requests, req)

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		flusher, _ := w.(http.Flusher)
		if n >= len(f.replies) {
			return
		}
		for _, line := range f.replies[n] {
			fmt.Fprintf(w, "data: %s\n\n", line)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// doRaw는 SSE처럼 JSON이 아닌 응답을 그대로 읽는다.
func (c *client) doRaw(method, path string, body any) (int, string) {
	c.t.Helper()
	var reader io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		reader = strings.NewReader(string(b))
	}
	req := httptest.NewRequest(method, path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-Requested-With", "dbstudio")
	for k, v := range c.cookies {
		req.AddCookie(&http.Cookie{Name: k, Value: v})
	}
	res, err := c.srv.App().Test(req, -1)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(raw)
}

func TestERDChatAppliesToolCall(t *testing.T) {
	e := newTestEnv(t)
	c := login(t, e, "alice")
	docID := createStandalone(t, e, c, "AI 대화", "postgres")

	// 1회차: 툴 호출. 2회차: 결과를 받고 마무리 문장.
	fake := newFakeLLM(t, [][]string{
		{
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"add_table","arguments":"{\"name\":\"orders\"}"}}]}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
			"[DONE]",
		},
		{
			`{"choices":[{"delta":{"content":"orders 테이블을 추가했습니다."}}]}`,
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

	status, body := c.do("POST", "/api/v1/erd/documents/"+docID+"/ai/sessions",
		map[string]any{"title": "설계"})
	if status != http.StatusCreated {
		t.Fatalf("create session = %d: %v", status, body)
	}
	sess, _ := body["session"].(map[string]any)
	sessID, _ := sess["id"].(string)

	status, raw := c.doRaw("POST", "/api/v1/ai/sessions/"+sessID+"/chat",
		map[string]any{"message": "orders 테이블을 만들어 줘"})
	if status != http.StatusOK {
		t.Fatalf("chat = %d: %s", status, raw)
	}

	// 화면은 툴이 돌았다는 사실을 봐야 한다 — 캔버스가 갑자기 바뀌는데 이유가
	// 아무 데도 없으면 누가 고쳤는지 알 수 없다.
	if !strings.Contains(raw, "event: tool_call") || !strings.Contains(raw, "add_table") {
		t.Errorf("툴 호출이 화면에 알려지지 않았습니다:\n%s", raw)
	}
	if !strings.Contains(raw, "orders 테이블을 추가했습니다") {
		t.Errorf("마무리 응답이 없습니다:\n%s", raw)
	}

	// 그리고 실제로 문서가 바뀌어야 한다.
	doc, err := e.st.GetERDDocument(context.Background(), docID)
	if err != nil {
		t.Fatalf("get document: %v", err)
	}
	if len(doc.Schema.Tables) != 1 || doc.Schema.Tables[0].Name != "orders" {
		t.Fatalf("문서가 바뀌지 않았습니다: %+v", doc.Schema.Tables)
	}

	// 모델에게 넘긴 툴 상자에는 초안 툴만 있어야 한다. 실제 DB를 건드리는 툴이
	// 하나라도 섞이면 초안 대화가 운영 DB에 손을 댈 수 있게 된다.
	if len(fake.requests) == 0 {
		t.Fatal("프로바이더가 호출되지 않았습니다")
	}
	names := toolNamesOf(t, fake.requests[0])
	if len(names) == 0 {
		t.Fatal("툴이 하나도 전달되지 않았습니다")
	}
	for _, n := range names {
		// 새 툴을 더할 때는 이 목록에도 적어야 한다. 접두사만 늘리지 않고 이름을
		// 그대로 적는 이유: 그래야 다음에 더해지는 툴이 여기서 한 번 걸린다.
		if !strings.HasPrefix(n, "add_") && !strings.HasPrefix(n, "update_") &&
			n != "read_schema" && n != "set_primary_key" && n != "duplicate_table" &&
			n != "set_logical_names" && n != "list_domains" && n != "detach_domain" {
			t.Errorf("초안 대화에 낯선 툴이 있습니다: %s", n)
		}
		if n == "run_query" || n == "list_connections" || n == "apply_migration" {
			t.Errorf("실제 DB를 건드리는 툴이 초안 대화에 노출됐습니다: %s", n)
		}
	}
}

func toolNamesOf(t *testing.T, req map[string]any) []string {
	t.Helper()
	arr, _ := req["tools"].([]any)
	out := []string{}
	for _, it := range arr {
		m, _ := it.(map[string]any)
		fn, _ := m["function"].(map[string]any)
		if name, _ := fn["name"].(string); name != "" {
			out = append(out, name)
		}
	}
	return out
}
