package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"dbstudio/internal/store"
)

// 컬럼 삭제는 초안 편집 툴 가운데 유일하게 승인을 거친다. 그 사실이 깨지는 방식은
// 둘이고, 둘 다 화면에서는 구별되지 않는다.
//
//   - 승인을 건너뛰고 바로 지워진다 → 사람이 보기 전에 컬럼이 사라진다.
//   - 승인해도 지워지지 않는다 → 승인 버튼이 아무 일도 하지 않는다.
//
// 그래서 왕복 전체를 한 시험에 담는다: 제안이 만들어지고, 그 시점에는 컬럼이 그대로
// 있고, 승인 뒤에 사라진다.
func TestERDChatDeleteColumnNeedsApproval(t *testing.T) {
	e := newTestEnv(t)
	c := login(t, e, "alice")
	docID := createStandalone(t, e, c, "삭제 승인", "postgres")

	// 지울 컬럼이 있는 표를 먼저 만든다(툴로 만들면 시험이 두 기능에 얽힌다).
	status, body := c.do("POST", "/api/v1/erd/documents/"+docID+"/import", map[string]any{
		"sql": "CREATE TABLE orders (id BIGINT PRIMARY KEY, memo VARCHAR(200), " +
			"CONSTRAINT uq_orders_memo UNIQUE (memo));",
		"mode": "replace",
	})
	if status != http.StatusOK {
		t.Fatalf("import = %d: %v", status, body)
	}

	fake := newFakeLLM(t, [][]string{
		{
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"delete_column","arguments":"{\"table\":\"orders\",\"column\":\"memo\"}"}}]}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
			"[DONE]",
		},
		{
			`{"choices":[{"delta":{"content":"승인을 요청했습니다."}}]}`,
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

	status, body = c.do("POST", "/api/v1/erd/documents/"+docID+"/ai/sessions",
		map[string]any{"title": "정리"})
	if status != http.StatusCreated {
		t.Fatalf("create session = %d: %v", status, body)
	}
	sess, _ := body["session"].(map[string]any)
	sessID, _ := sess["id"].(string)

	status, raw := c.doRaw("POST", "/api/v1/ai/sessions/"+sessID+"/chat",
		map[string]any{"message": "memo 컬럼 지워줘"})
	if status != http.StatusOK {
		t.Fatalf("chat = %d: %s", status, raw)
	}
	// 화면은 승인 카드를 받아야 한다.
	if !strings.Contains(raw, "event: pending_action") {
		t.Fatalf("승인 제안이 화면에 오지 않았습니다:\n%s", raw)
	}
	// 제안에는 함께 영향받는 것들이 담겨야 한다 — 승인 화면이 하는 일이 그것이다.
	if !strings.Contains(raw, "uq_orders_memo") {
		t.Errorf("제안에 함께 영향받는 인덱스가 없습니다:\n%s", raw)
	}

	// 아직 지워지지 않았다.
	doc, err := e.st.GetERDDocument(context.Background(), docID)
	if err != nil {
		t.Fatalf("get document: %v", err)
	}
	if doc.Schema.Tables[0].Column("memo") == nil {
		t.Fatal("승인 전에 컬럼이 지워졌습니다")
	}

	// 제안 목록에서 아이디를 찾는다(화면이 하는 것과 같은 경로).
	status, body = c.do("GET", "/api/v1/ai/sessions/"+sessID, nil)
	if status != http.StatusOK {
		t.Fatalf("get session = %d: %v", status, body)
	}
	actions, _ := body["pendingActions"].([]any)
	if len(actions) != 1 {
		t.Fatalf("제안이 %d개입니다: %v", len(actions), body["pendingActions"])
	}
	action, _ := actions[0].(map[string]any)
	actionID, _ := action["id"].(string)
	if name, _ := action["toolName"].(string); name != "erd_delete_column" {
		t.Errorf("제안의 툴 이름이 %q 입니다(앱 레지스트리 이름이어야 승인이 실행됩니다)", name)
	}

	status, body = c.do("POST",
		"/api/v1/ai/sessions/"+sessID+"/actions/"+actionID, map[string]any{"decision": "approve"})
	if status != http.StatusOK {
		t.Fatalf("approve = %d: %v", status, body)
	}

	doc, err = e.st.GetERDDocument(context.Background(), docID)
	if err != nil {
		t.Fatalf("get document: %v", err)
	}
	if doc.Schema.Tables[0].Column("memo") != nil {
		t.Fatal("승인했는데 컬럼이 남아 있습니다")
	}
}

// 거부하면 그대로 남는다. 거부가 조용히 실행으로 이어지면 승인 화면은 장식이 된다.
func TestERDChatDeleteColumnRejected(t *testing.T) {
	e := newTestEnv(t)
	c := login(t, e, "alice")
	docID := createStandalone(t, e, c, "삭제 거부", "postgres")
	status, body := c.do("POST", "/api/v1/erd/documents/"+docID+"/import", map[string]any{
		"sql":  "CREATE TABLE orders (id BIGINT PRIMARY KEY, memo VARCHAR(200));",
		"mode": "replace",
	})
	if status != http.StatusOK {
		t.Fatalf("import = %d: %v", status, body)
	}

	fake := newFakeLLM(t, [][]string{
		{
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"delete_column","arguments":"{\"table\":\"orders\",\"column\":\"memo\"}"}}]}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
			"[DONE]",
		},
		{
			`{"choices":[{"delta":{"content":"승인을 요청했습니다."}}]}`,
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
	status, body = c.do("POST", "/api/v1/erd/documents/"+docID+"/ai/sessions",
		map[string]any{"title": "정리"})
	if status != http.StatusCreated {
		t.Fatalf("create session = %d: %v", status, body)
	}
	sess, _ := body["session"].(map[string]any)
	sessID, _ := sess["id"].(string)
	if status, raw := c.doRaw("POST", "/api/v1/ai/sessions/"+sessID+"/chat",
		map[string]any{"message": "memo 지워"}); status != http.StatusOK {
		t.Fatalf("chat = %d: %s", status, raw)
	}

	status, body = c.do("GET", "/api/v1/ai/sessions/"+sessID, nil)
	if status != http.StatusOK {
		t.Fatalf("get session = %d: %v", status, body)
	}
	actions, _ := body["pendingActions"].([]any)
	if len(actions) != 1 {
		t.Fatalf("제안이 %d개입니다", len(actions))
	}
	action, _ := actions[0].(map[string]any)
	actionID, _ := action["id"].(string)

	if status, body = c.do("POST",
		"/api/v1/ai/sessions/"+sessID+"/actions/"+actionID,
		map[string]any{"decision": "reject"}); status != http.StatusOK {
		t.Fatalf("reject = %d: %v", status, body)
	}
	doc, err := e.st.GetERDDocument(context.Background(), docID)
	if err != nil {
		t.Fatalf("get document: %v", err)
	}
	if doc.Schema.Tables[0].Column("memo") == nil {
		t.Fatal("거부했는데 컬럼이 지워졌습니다")
	}
}

// 제안에 문서 아이디가 채워지지 않으면 승인 실행이 "어느 초안이냐"로 끝난다.
// 이 대화의 툴은 문서가 정해져 있어 모델이 document 를 보내지 않기 때문이다.
func TestWithDocumentID(t *testing.T) {
	got, err := withDocumentID(json.RawMessage(`{"table":"orders","column":"memo"}`), "doc-1")
	if err != nil {
		t.Fatalf("withDocumentID: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(got, &m); err != nil {
		t.Fatalf("결과가 JSON이 아닙니다: %v", err)
	}
	if m["document"] != "doc-1" || m["column"] != "memo" {
		t.Errorf("인자가 잘못 채워졌습니다: %s", got)
	}
	// 모델이 보낸 값은 건드리지 않는다. 조용히 바꿔치기하면 엉뚱한 초안이 지워진다.
	got, err = withDocumentID(json.RawMessage(`{"document":"other","column":"memo"}`), "doc-1")
	if err != nil {
		t.Fatalf("withDocumentID: %v", err)
	}
	_ = json.Unmarshal(got, &m)
	if m["document"] != "other" {
		t.Errorf("모델이 지정한 문서가 덮어써졌습니다: %s", got)
	}
	// 인자가 비어 있어도 문서는 채워져야 한다.
	got, err = withDocumentID(nil, "doc-1")
	if err != nil {
		t.Fatalf("withDocumentID(nil): %v", err)
	}
	_ = json.Unmarshal(got, &m)
	if m["document"] != "doc-1" {
		t.Errorf("빈 인자에 문서가 채워지지 않았습니다: %s", got)
	}
}
