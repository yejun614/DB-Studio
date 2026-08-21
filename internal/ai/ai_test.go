package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ---------- 하네스 ----------

// fakeLLM은 SSE 스트림을 흉내내는 서버다.
//
// 실제 프로바이더에 붙여 테스트할 수 없으므로, 각 API의 스트림 형식을 "내가 이해한
// 대로" 적고 파서가 그 이해와 일치하는지 확인한다. 여기서 잡히는 것은 이벤트 종류
// 처리, 툴 인자 조립, 종료 판정의 실수다.
type fakeLLM struct {
	t      *testing.T
	server *httptest.Server
	// lastRequest는 마지막 요청 본문이다 (변환 검증용).
	lastRequest map[string]any
	lastHeaders http.Header
	lastPath    string
	// chunks는 순서대로 내보낼 SSE data 줄이다.
	chunks []string
	// status가 200이 아니면 그 상태로 즉시 실패한다.
	status int
	body   string
	// delay는 각 줄 사이 지연이다 (취소 검증용).
	delay time.Duration
}

func newFake(t *testing.T) *fakeLLM {
	f := &fakeLLM{t: t, status: 200}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		f.lastHeaders = r.Header.Clone()
		f.lastPath = r.URL.Path
		if len(raw) > 0 {
			f.lastRequest = map[string]any{}
			if err := json.Unmarshal(raw, &f.lastRequest); err != nil {
				f.t.Errorf("요청 본문을 해석할 수 없습니다: %v (%s)", err, raw)
			}
		}
		if f.status != 200 {
			w.WriteHeader(f.status)
			_, _ = w.Write([]byte(f.body))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		flusher, _ := w.(http.Flusher)
		for _, c := range f.chunks {
			// 클라이언트가 끊었으면 즉시 멈춘다. 실제 서버도 그렇게 동작하며,
			// 그러지 않으면 취소 테스트에서 httptest.Server.Close가 남은 쓰기를
			// 기다리며 블록된다.
			select {
			case <-r.Context().Done():
				return
			default:
			}
			fmt.Fprintf(w, "data: %s\n\n", c)
			if flusher != nil {
				flusher.Flush()
			}
			if f.delay > 0 {
				select {
				case <-time.After(f.delay):
				case <-r.Context().Done():
					return
				}
			}
		}
	}))
	t.Cleanup(f.server.Close)
	return f
}

// collect는 스트림을 끝까지 읽어 이벤트를 모은다.
func collect(t *testing.T, ch <-chan Event) []Event {
	t.Helper()
	out := []Event{}
	deadline := time.After(10 * time.Second)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, ev)
		case <-deadline:
			t.Fatalf("스트림이 끝나지 않았습니다 (받은 이벤트 %d개)", len(out))
		}
	}
}

func textOf(events []Event) string {
	var b strings.Builder
	for _, ev := range events {
		if ev.Type == EventText {
			b.WriteString(ev.Text)
		}
	}
	return b.String()
}

func toolCallsOf(events []Event) []ToolCall {
	out := []ToolCall{}
	for _, ev := range events {
		if ev.Type == EventToolCall && ev.ToolCall != nil {
			out = append(out, *ev.ToolCall)
		}
	}
	return out
}

func lastEvent(events []Event) Event {
	if len(events) == 0 {
		return Event{}
	}
	return events[len(events)-1]
}

// ---------- Anthropic ----------

func TestAnthropicTextStream(t *testing.T) {
	f := newFake(t)
	f.chunks = []string{
		`{"type":"message_start","message":{"usage":{"input_tokens":25}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"안녕"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"하세요"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7}}`,
		`{"type":"message_stop"}`,
	}

	p, _ := Get(Anthropic)
	cfg := Config{Kind: Anthropic, BaseURL: f.server.URL, APIKey: "sk-test", Model: "claude-x"}
	ch, err := p.Stream(context.Background(), cfg, Request{
		System: "너는 DB 도우미다", Messages: []Message{{Role: RoleUser, Text: "안녕"}},
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	events := collect(t, ch)

	if got := textOf(events); got != "안녕하세요" {
		t.Errorf("텍스트 = %q", got)
	}
	done := lastEvent(events)
	if done.Type != EventDone {
		t.Fatalf("마지막 이벤트 = %s", done.Type)
	}
	if done.StopReason != "end_turn" {
		t.Errorf("stop reason = %q", done.StopReason)
	}
	if done.Usage == nil || done.Usage.InputTokens != 25 || done.Usage.OutputTokens != 7 {
		t.Errorf("usage = %+v", done.Usage)
	}

	// 요청 형식 확인
	if f.lastPath != "/v1/messages" {
		t.Errorf("경로 = %s", f.lastPath)
	}
	if got := f.lastHeaders.Get("x-api-key"); got != "sk-test" {
		t.Errorf("x-api-key = %q (Anthropic은 이 헤더를 쓴다)", got)
	}
	if f.lastHeaders.Get("anthropic-version") == "" {
		t.Error("anthropic-version 헤더가 없습니다")
	}
	if f.lastHeaders.Get("Authorization") != "" {
		t.Error("Anthropic에 Authorization 헤더를 보냈습니다")
	}
	// system은 최상위 필드여야 한다 (messages 배열이 아니다).
	if f.lastRequest["system"] != "너는 DB 도우미다" {
		t.Errorf("system = %v", f.lastRequest["system"])
	}
	msgs, _ := f.lastRequest["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("메시지 수 = %d (system이 섞였을 수 있습니다)", len(msgs))
	}
	// max_tokens는 필수다.
	if mt, ok := f.lastRequest["max_tokens"].(float64); !ok || mt <= 0 {
		t.Errorf("max_tokens = %v (Anthropic은 필수)", f.lastRequest["max_tokens"])
	}
}

// 툴 인자는 input_json_delta로 조각조각 온다. 조립이 틀리면 인자가 깨진 툴을 실행한다.
func TestAnthropicToolCallAssembly(t *testing.T) {
	f := newFake(t)
	f.chunks = []string{
		`{"type":"message_start","message":{"usage":{"input_tokens":10}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"확인해보겠습니다."}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_01","name":"list_connections","input":{}}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"env\""}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":":\"prod\""}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"}"}}`,
		`{"type":"content_block_stop","index":1}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":30}}`,
		`{"type":"message_stop"}`,
	}

	p, _ := Get(Anthropic)
	cfg := Config{Kind: Anthropic, BaseURL: f.server.URL, APIKey: "k", Model: "m"}
	ch, _ := p.Stream(context.Background(), cfg, Request{
		Messages: []Message{{Role: RoleUser, Text: "운영 DB 목록"}},
		Tools: []Tool{{
			Name: "list_connections", Description: "커넥션 목록",
			Schema: map[string]any{"type": "object", "properties": map[string]any{}},
		}},
	})
	events := collect(t, ch)

	if got := textOf(events); got != "확인해보겠습니다." {
		t.Errorf("텍스트 = %q", got)
	}
	calls := toolCallsOf(events)
	if len(calls) != 1 {
		t.Fatalf("툴 호출 수 = %d", len(calls))
	}
	if calls[0].ID != "toolu_01" || calls[0].Name != "list_connections" {
		t.Errorf("툴 호출 = %+v", calls[0])
	}
	var input map[string]any
	if err := json.Unmarshal(calls[0].Input, &input); err != nil {
		t.Fatalf("조립된 인자가 올바른 JSON이 아닙니다: %v (%s)", err, calls[0].Input)
	}
	if input["env"] != "prod" {
		t.Errorf("인자 = %+v", input)
	}
	if lastEvent(events).StopReason != "tool_use" {
		t.Errorf("stop reason = %q", lastEvent(events).StopReason)
	}

	// 툴 정의는 input_schema 필드로 전달되어야 한다.
	tools, _ := f.lastRequest["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("툴 수 = %d", len(tools))
	}
	tool, _ := tools[0].(map[string]any)
	if _, ok := tool["input_schema"]; !ok {
		t.Errorf("input_schema가 없습니다: %+v (Anthropic은 parameters가 아니다)", tool)
	}
}

// 인자가 없는 툴은 partial_json이 오지 않는다. 그때 빈 문자열을 그대로 쓰면
// JSON 파싱이 실패해 툴이 호출되지 못한다.
func TestAnthropicToolCallWithoutArgs(t *testing.T) {
	f := newFake(t)
	f.chunks = []string{
		`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"t1","name":"ping","input":{}}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use"}}`,
		`{"type":"message_stop"}`,
	}
	p, _ := Get(Anthropic)
	ch, _ := p.Stream(context.Background(),
		Config{Kind: Anthropic, BaseURL: f.server.URL, APIKey: "k", Model: "m"},
		Request{Messages: []Message{{Role: RoleUser, Text: "x"}}})
	calls := toolCallsOf(collect(t, ch))
	if len(calls) != 1 {
		t.Fatalf("툴 호출 수 = %d", len(calls))
	}
	if string(calls[0].Input) != "{}" {
		t.Errorf("빈 인자 = %q, {} 여야 합니다", calls[0].Input)
	}
}

// 병렬 툴 호출: 두 블록이 번갈아 도착해도 각자의 인자가 섞이지 않아야 한다.
func TestAnthropicParallelToolCalls(t *testing.T) {
	f := newFake(t)
	f.chunks = []string{
		`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"a","name":"first"}}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"b","name":"second"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"x\":1}"}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"y\":2}"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_stop","index":1}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use"}}`,
		`{"type":"message_stop"}`,
	}
	p, _ := Get(Anthropic)
	ch, _ := p.Stream(context.Background(),
		Config{Kind: Anthropic, BaseURL: f.server.URL, APIKey: "k", Model: "m"},
		Request{Messages: []Message{{Role: RoleUser, Text: "x"}}})
	calls := toolCallsOf(collect(t, ch))
	if len(calls) != 2 {
		t.Fatalf("툴 호출 수 = %d", len(calls))
	}
	byName := map[string]string{}
	for _, c := range calls {
		byName[c.Name] = string(c.Input)
	}
	if byName["first"] != `{"x":1}` {
		t.Errorf("first 인자 = %s", byName["first"])
	}
	if byName["second"] != `{"y":2}` {
		t.Errorf("second 인자 = %s", byName["second"])
	}
}

// 툴 결과는 user 메시지의 tool_result 블록으로 보내야 한다 (전용 role이 없다).
func TestAnthropicToolResultConversion(t *testing.T) {
	f := newFake(t)
	f.chunks = []string{`{"type":"message_stop"}`}
	p, _ := Get(Anthropic)
	ch, _ := p.Stream(context.Background(),
		Config{Kind: Anthropic, BaseURL: f.server.URL, APIKey: "k", Model: "m"},
		Request{Messages: []Message{
			{Role: RoleUser, Text: "커넥션 알려줘"},
			{Role: RoleAssistant, Text: "확인합니다", ToolCalls: []ToolCall{
				{ID: "t1", Name: "list_connections", Input: json.RawMessage(`{}`)},
			}},
			{Role: RoleTool, ToolResults: []ToolResult{
				{CallID: "t1", Content: "dev-mysql, prod-pg"},
			}},
		}})
	collect(t, ch)

	msgs, _ := f.lastRequest["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("메시지 수 = %d", len(msgs))
	}
	assistant, _ := msgs[1].(map[string]any)
	blocks, _ := assistant["content"].([]any)
	if len(blocks) != 2 {
		t.Fatalf("assistant 블록 수 = %d (텍스트 + tool_use)", len(blocks))
	}
	toolUse, _ := blocks[1].(map[string]any)
	if toolUse["type"] != "tool_use" || toolUse["id"] != "t1" {
		t.Errorf("tool_use 블록 = %+v", toolUse)
	}
	result, _ := msgs[2].(map[string]any)
	if result["role"] != "user" {
		t.Errorf("툴 결과 role = %v, Anthropic은 user여야 합니다", result["role"])
	}
	rblocks, _ := result["content"].([]any)
	rb, _ := rblocks[0].(map[string]any)
	if rb["type"] != "tool_result" || rb["tool_use_id"] != "t1" {
		t.Errorf("tool_result 블록 = %+v", rb)
	}
}

// 스트림 중간의 error 이벤트를 오류로 전달해야 한다.
func TestAnthropicStreamError(t *testing.T) {
	f := newFake(t)
	f.chunks = []string{
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"시작"}}`,
		`{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`,
	}
	p, _ := Get(Anthropic)
	ch, _ := p.Stream(context.Background(),
		Config{Kind: Anthropic, BaseURL: f.server.URL, APIKey: "k", Model: "m"},
		Request{Messages: []Message{{Role: RoleUser, Text: "x"}}})
	events := collect(t, ch)

	last := lastEvent(events)
	if last.Type != EventError {
		t.Fatalf("마지막 이벤트 = %s (오류를 기대)", last.Type)
	}
	if !strings.Contains(last.Err.Error(), "Overloaded") {
		t.Errorf("오류 = %v", last.Err)
	}
	// 오류 전에 온 텍스트는 유지되어야 한다.
	if textOf(events) != "시작" {
		t.Errorf("텍스트 = %q", textOf(events))
	}
}

func TestAnthropicHTTPError(t *testing.T) {
	f := newFake(t)
	f.status = 401
	f.body = `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`

	p, _ := Get(Anthropic)
	ch, _ := p.Stream(context.Background(),
		Config{Kind: Anthropic, BaseURL: f.server.URL, APIKey: "bad", Model: "m"},
		Request{Messages: []Message{{Role: RoleUser, Text: "x"}}})
	last := lastEvent(collect(t, ch))
	if last.Type != EventError {
		t.Fatalf("이벤트 = %s", last.Type)
	}
	if !strings.Contains(last.Err.Error(), "invalid x-api-key") {
		t.Errorf("오류 메시지가 원인을 담지 않습니다: %v", last.Err)
	}
}

// ---------- OpenAI 호환 ----------

func TestOpenAITextStream(t *testing.T) {
	f := newFake(t)
	f.chunks = []string{
		`{"choices":[{"delta":{"role":"assistant","content":""}}]}`,
		`{"choices":[{"delta":{"content":"안녕"}}]}`,
		`{"choices":[{"delta":{"content":"하세요"}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":12,"completion_tokens":5}}`,
		`[DONE]`,
	}

	p, _ := Get(OpenAICompatible)
	cfg := Config{Kind: OpenAICompatible, BaseURL: f.server.URL, APIKey: "sk-x", Model: "gpt-x"}
	ch, err := p.Stream(context.Background(), cfg, Request{
		System: "시스템 지침", Messages: []Message{{Role: RoleUser, Text: "안녕"}},
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	events := collect(t, ch)

	if got := textOf(events); got != "안녕하세요" {
		t.Errorf("텍스트 = %q", got)
	}
	done := lastEvent(events)
	if done.Type != EventDone || done.StopReason != "stop" {
		t.Errorf("종료 = %+v", done)
	}
	if done.Usage == nil || done.Usage.InputTokens != 12 {
		t.Errorf("usage = %+v", done.Usage)
	}
	if f.lastPath != "/v1/chat/completions" {
		t.Errorf("경로 = %s", f.lastPath)
	}
	if got := f.lastHeaders.Get("Authorization"); got != "Bearer sk-x" {
		t.Errorf("Authorization = %q", got)
	}
	// system은 messages의 첫 항목이어야 한다 (Anthropic과 반대).
	msgs, _ := f.lastRequest["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("메시지 수 = %d", len(msgs))
	}
	first, _ := msgs[0].(map[string]any)
	if first["role"] != "system" || first["content"] != "시스템 지침" {
		t.Errorf("첫 메시지 = %+v (system이어야 합니다)", first)
	}
}

// 툴 인자는 index별 arguments 조각으로 온다. id와 name은 첫 조각에만 있다.
func TestOpenAIToolCallAssembly(t *testing.T) {
	f := newFake(t)
	f.chunks = []string{
		`{"choices":[{"delta":{"content":"조회합니다."}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"get_metrics","arguments":""}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"conn\""}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":":\"dev\"}"}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`[DONE]`,
	}

	p, _ := Get(OpenAICompatible)
	ch, _ := p.Stream(context.Background(),
		Config{Kind: OpenAICompatible, BaseURL: f.server.URL, APIKey: "k", Model: "m"},
		Request{
			Messages: []Message{{Role: RoleUser, Text: "지표 보여줘"}},
			Tools: []Tool{{
				Name: "get_metrics", Description: "지표 조회",
				Schema: map[string]any{"type": "object", "properties": map[string]any{}},
			}},
		})
	events := collect(t, ch)

	if got := textOf(events); got != "조회합니다." {
		t.Errorf("텍스트 = %q", got)
	}
	calls := toolCallsOf(events)
	if len(calls) != 1 {
		t.Fatalf("툴 호출 수 = %d", len(calls))
	}
	if calls[0].ID != "call_1" || calls[0].Name != "get_metrics" {
		t.Errorf("툴 호출 = %+v", calls[0])
	}
	var input map[string]any
	if err := json.Unmarshal(calls[0].Input, &input); err != nil {
		t.Fatalf("인자 조립 실패: %v (%s)", err, calls[0].Input)
	}
	if input["conn"] != "dev" {
		t.Errorf("인자 = %+v", input)
	}

	// 툴 정의는 {type:"function", function:{...parameters}} 형태여야 한다.
	tools, _ := f.lastRequest["tools"].([]any)
	tool, _ := tools[0].(map[string]any)
	if tool["type"] != "function" {
		t.Errorf("툴 type = %v", tool["type"])
	}
	fn, _ := tool["function"].(map[string]any)
	if _, ok := fn["parameters"]; !ok {
		t.Errorf("parameters가 없습니다: %+v (OpenAI는 input_schema가 아니다)", fn)
	}
}

// finish_reason 없이 [DONE]만 오는 서버(일부 로컬 LLM)에서도 툴 호출이 나와야 한다.
func TestOpenAIToolCallWithoutFinishReason(t *testing.T) {
	f := newFake(t)
	f.chunks = []string{
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"ping","arguments":"{}"}}]}}]}`,
		`[DONE]`,
	}
	p, _ := Get(OpenAICompatible)
	ch, _ := p.Stream(context.Background(),
		Config{Kind: OpenAICompatible, BaseURL: f.server.URL, APIKey: "k", Model: "m"},
		Request{Messages: []Message{{Role: RoleUser, Text: "x"}}})
	calls := toolCallsOf(collect(t, ch))
	if len(calls) != 1 {
		t.Fatalf("툴 호출 수 = %d (finish_reason이 없어도 내보내야 합니다)", len(calls))
	}
	if calls[0].Name != "ping" {
		t.Errorf("툴 = %+v", calls[0])
	}
}

// 툴 결과는 결과마다 별도의 role=tool 메시지여야 한다.
func TestOpenAIToolResultConversion(t *testing.T) {
	f := newFake(t)
	f.chunks = []string{`[DONE]`}
	p, _ := Get(OpenAICompatible)
	ch, _ := p.Stream(context.Background(),
		Config{Kind: OpenAICompatible, BaseURL: f.server.URL, APIKey: "k", Model: "m"},
		Request{Messages: []Message{
			{Role: RoleUser, Text: "질문"},
			{Role: RoleAssistant, ToolCalls: []ToolCall{
				{ID: "c1", Name: "a", Input: json.RawMessage(`{}`)},
				{ID: "c2", Name: "b", Input: json.RawMessage(`{}`)},
			}},
			{Role: RoleTool, ToolResults: []ToolResult{
				{CallID: "c1", Content: "결과1"},
				{CallID: "c2", Content: "실패", IsError: true},
			}},
		}})
	collect(t, ch)

	msgs, _ := f.lastRequest["messages"].([]any)
	// user + assistant + tool + tool = 4
	if len(msgs) != 4 {
		t.Fatalf("메시지 수 = %d (툴 결과마다 별도 메시지여야 합니다): %v", len(msgs), msgs)
	}
	assistant, _ := msgs[1].(map[string]any)
	calls, _ := assistant["tool_calls"].([]any)
	if len(calls) != 2 {
		t.Fatalf("tool_calls 수 = %d", len(calls))
	}
	call, _ := calls[0].(map[string]any)
	fn, _ := call["function"].(map[string]any)
	// arguments는 문자열이어야 한다 (객체가 아니다).
	if _, ok := fn["arguments"].(string); !ok {
		t.Errorf("arguments 타입 = %T, 문자열이어야 합니다", fn["arguments"])
	}
	third, _ := msgs[2].(map[string]any)
	if third["role"] != "tool" || third["tool_call_id"] != "c1" {
		t.Errorf("툴 결과 메시지 = %+v", third)
	}
	fourth, _ := msgs[3].(map[string]any)
	if !strings.Contains(fmt.Sprint(fourth["content"]), "실패") {
		t.Errorf("오류 결과 내용 = %v", fourth["content"])
	}
}

func TestOpenAIStreamError(t *testing.T) {
	f := newFake(t)
	f.chunks = []string{
		`{"error":{"message":"context length exceeded","type":"invalid_request_error"}}`,
	}
	p, _ := Get(OpenAICompatible)
	ch, _ := p.Stream(context.Background(),
		Config{Kind: OpenAICompatible, BaseURL: f.server.URL, APIKey: "k", Model: "m"},
		Request{Messages: []Message{{Role: RoleUser, Text: "x"}}})
	last := lastEvent(collect(t, ch))
	if last.Type != EventError {
		t.Fatalf("이벤트 = %s", last.Type)
	}
	if !strings.Contains(last.Err.Error(), "context length") {
		t.Errorf("오류 = %v", last.Err)
	}
}

// ---------- 공통 ----------

// 컨텍스트를 취소하면 스트림이 멈추고 채널이 닫혀야 한다.
// 그러지 않으면 사용자가 화면을 떠난 뒤에도 goroutine과 토큰이 계속 소모된다.
func TestStreamCancellation(t *testing.T) {
	for _, kind := range []Kind{Anthropic, OpenAICompatible} {
		t.Run(string(kind), func(t *testing.T) {
			f := newFake(t)
			f.delay = 50 * time.Millisecond
			// 많은 조각을 보내 취소가 중간에 일어나게 한다.
			for i := 0; i < 200; i++ {
				if kind == Anthropic {
					f.chunks = append(f.chunks,
						`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"x"}}`)
				} else {
					f.chunks = append(f.chunks, `{"choices":[{"delta":{"content":"x"}}]}`)
				}
			}

			ctx, cancel := context.WithCancel(context.Background())
			p, _ := Get(kind)
			ch, err := p.Stream(ctx, Config{Kind: kind, BaseURL: f.server.URL, APIKey: "k", Model: "m"},
				Request{Messages: []Message{{Role: RoleUser, Text: "x"}}})
			if err != nil {
				t.Fatalf("stream: %v", err)
			}
			// 몇 개만 읽고 취소한다.
			read := 0
			for ev := range ch {
				read++
				if read == 3 {
					cancel()
				}
				_ = ev
				if read > 250 {
					t.Fatal("취소 후에도 계속 읽혔습니다")
				}
			}
			// 채널이 닫혔다는 것 자체가 goroutine이 끝났다는 뜻이다.
			if read < 3 {
				t.Errorf("읽은 이벤트 = %d", read)
			}
			cancel()
		})
	}
}

// 알 수 없는 이벤트 종류가 와도 스트림이 끊기지 않아야 한다.
// 프로바이더가 새 이벤트를 추가하는 일은 실제로 일어난다.
func TestUnknownEventsIgnored(t *testing.T) {
	f := newFake(t)
	f.chunks = []string{
		`{"type":"ping"}`,
		`{"type":"brand_new_event","payload":{"x":1}}`,
		`not json at all`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"살아있음"}}`,
		`{"type":"message_stop"}`,
	}
	p, _ := Get(Anthropic)
	ch, _ := p.Stream(context.Background(),
		Config{Kind: Anthropic, BaseURL: f.server.URL, APIKey: "k", Model: "m"},
		Request{Messages: []Message{{Role: RoleUser, Text: "x"}}})
	events := collect(t, ch)
	if textOf(events) != "살아있음" {
		t.Errorf("텍스트 = %q", textOf(events))
	}
	if lastEvent(events).Type != EventDone {
		t.Errorf("마지막 이벤트 = %s", lastEvent(events).Type)
	}
}

// 큰 툴 인자가 한 줄에 담겨 와도 잘리지 않아야 한다.
// 기본 스캐너 버퍼(64KB)로는 부족한 경우가 있고, 넘치면 조용히 멈춘다.
func TestLargeToolArgumentLine(t *testing.T) {
	big := strings.Repeat("가", 100_000)
	payload, _ := json.Marshal(map[string]any{
		"type": "content_block_delta", "index": 0,
		"delta": map[string]any{"type": "input_json_delta",
			"partial_json": `{"sql":"` + big + `"}`},
	})
	f := newFake(t)
	f.chunks = []string{
		`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"t","name":"run"}}`,
		string(payload),
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_stop"}`,
	}
	p, _ := Get(Anthropic)
	ch, _ := p.Stream(context.Background(),
		Config{Kind: Anthropic, BaseURL: f.server.URL, APIKey: "k", Model: "m"},
		Request{Messages: []Message{{Role: RoleUser, Text: "x"}}})
	calls := toolCallsOf(collect(t, ch))
	if len(calls) != 1 {
		t.Fatalf("툴 호출 수 = %d (큰 줄에서 스트림이 끊겼습니다)", len(calls))
	}
	var input map[string]string
	if err := json.Unmarshal(calls[0].Input, &input); err != nil {
		t.Fatalf("큰 인자 파싱 실패: %v", err)
	}
	if len([]rune(input["sql"])) != 100_000 {
		t.Errorf("인자 길이 = %d", len([]rune(input["sql"])))
	}
}

func TestModelsList(t *testing.T) {
	for _, kind := range []Kind{Anthropic, OpenAICompatible} {
		t.Run(string(kind), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				want := "/v1/models"
				if r.URL.Path != want {
					t.Errorf("경로 = %s, 기대값 %s", r.URL.Path, want)
				}
				_, _ = w.Write([]byte(`{"data":[{"id":"model-a"},{"id":"model-b"}]}`))
			}))
			defer srv.Close()

			p, _ := Get(kind)
			models, err := p.Models(context.Background(),
				Config{Kind: kind, BaseURL: srv.URL, APIKey: "k"})
			if err != nil {
				t.Fatalf("models: %v", err)
			}
			if len(models) != 2 || models[0] != "model-a" {
				t.Errorf("모델 = %v", models)
			}
		})
	}
}

// 사고 모델(Gemini)은 툴 호출마다 서명을 붙여 보내고 다음 요청에 그대로 돌려주기를
// 요구한다. 흘리면 그 다음 요청이 400으로 거부되고("missing a thought_signature"),
// 증상은 "툴을 한 번 부른 뒤 대화가 죽는다"로 나타난다.
func TestOpenAIThoughtSignatureCaptured(t *testing.T) {
	f := newFake(t)
	f.chunks = []string{
		// 서명은 이름·id와 같은 조각에 올 수도, 인자 조각 뒤에 따로 올 수도 있다.
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"list_connections","arguments":"{}"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"extra_content":{"google":{"thought_signature":"SIG-1"}}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`[DONE]`,
	}
	p, _ := Get(OpenAICompatible)
	ch, _ := p.Stream(context.Background(),
		Config{Kind: OpenAICompatible, BaseURL: f.server.URL, APIKey: "k", Model: "m"},
		Request{Messages: []Message{{Role: RoleUser, Text: "커넥션 목록"}}})

	calls := toolCallsOf(collect(t, ch))
	if len(calls) != 1 {
		t.Fatalf("툴 호출 수 = %d", len(calls))
	}
	if calls[0].Signature != "SIG-1" {
		t.Errorf("서명 = %q, 기대값 SIG-1 — 흘리면 다음 요청이 400으로 거부된다", calls[0].Signature)
	}
}

// 받은 서명은 다음 요청의 같은 자리에 그대로 실려 나가야 한다.
func TestOpenAIThoughtSignatureReplayed(t *testing.T) {
	f := newFake(t)
	f.chunks = []string{`[DONE]`}
	p, _ := Get(OpenAICompatible)
	ch, _ := p.Stream(context.Background(),
		Config{Kind: OpenAICompatible, BaseURL: f.server.URL, APIKey: "k", Model: "m"},
		Request{Messages: []Message{
			{Role: RoleUser, Text: "질문"},
			{Role: RoleAssistant, ToolCalls: []ToolCall{
				{ID: "c1", Name: "a", Input: json.RawMessage(`{}`), Signature: "SIG-1"},
				{ID: "c2", Name: "b", Input: json.RawMessage(`{}`)},
			}},
			{Role: RoleTool, ToolResults: []ToolResult{
				{CallID: "c1", Content: "결과1"}, {CallID: "c2", Content: "결과2"},
			}},
		}})
	collect(t, ch)

	msgs, _ := f.lastRequest["messages"].([]any)
	assistant, _ := msgs[1].(map[string]any)
	calls, _ := assistant["tool_calls"].([]any)
	if len(calls) != 2 {
		t.Fatalf("tool_calls 수 = %d", len(calls))
	}

	first, _ := calls[0].(map[string]any)
	extra, ok := first["extra_content"].(map[string]any)
	if !ok {
		t.Fatalf("extra_content가 없습니다: %+v", first)
	}
	google, _ := extra["google"].(map[string]any)
	if google["thought_signature"] != "SIG-1" {
		t.Errorf("돌려보낸 서명 = %v", google["thought_signature"])
	}

	// 서명이 없는 호출에는 칸 자체가 없어야 한다. 이 필드를 모르는 서버
	// (로컬 LLM, OpenAI 본체)에 빈 객체를 보내면 거부당할 수 있다.
	second, _ := calls[1].(map[string]any)
	if _, has := second["extra_content"]; has {
		t.Errorf("서명 없는 호출에 extra_content가 붙었습니다: %+v", second)
	}
}

// index 없이 오는 병렬 툴 호출.
//
// 규약은 index로 호출을 가르라고 하지만 Gemini의 OpenAI 호환 계층은 그 필드를 보내지
// 않는다. index를 int로 받아 0으로 뭉개면 두 호출의 인자가 이어 붙어
// `{"connection":"운영"}{"connection":"개발"}` 이 되고, 툴은
// "invalid character '{' after top-level value"로 실패한다.
func TestOpenAIParallelToolCallsWithoutIndex(t *testing.T) {
	f := newFake(t)
	f.chunks = []string{
		`{"choices":[{"delta":{"tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_connection_status","arguments":"{\"connection\":\"운영\"}"},"extra_content":{"google":{"thought_signature":"SIG-A"}}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"id":"call_2","type":"function","function":{"name":"get_connection_status","arguments":"{\"connection\":\"개발\"}"},"extra_content":{"google":{"thought_signature":"SIG-B"}}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`[DONE]`,
	}
	p, _ := Get(OpenAICompatible)
	ch, _ := p.Stream(context.Background(),
		Config{Kind: OpenAICompatible, BaseURL: f.server.URL, APIKey: "k", Model: "m"},
		Request{Messages: []Message{{Role: RoleUser, Text: "두 DB 상태"}}})

	calls := toolCallsOf(collect(t, ch))
	if len(calls) != 2 {
		t.Fatalf("툴 호출 수 = %d, want 2 — index가 없으면 호출이 뭉개진다: %+v", len(calls), calls)
	}
	for i, want := range []string{"운영", "개발"} {
		var in map[string]any
		if err := json.Unmarshal(calls[i].Input, &in); err != nil {
			t.Fatalf("%d번 인자가 JSON이 아니다: %v (%s)", i, err, calls[i].Input)
		}
		if in["connection"] != want {
			t.Errorf("%d번 인자 = %v, want %s", i, in, want)
		}
	}
	// 서명도 각자의 호출에 붙어야 한다. 섞이면 다음 요청이 거부된다.
	if calls[0].Signature != "SIG-A" || calls[1].Signature != "SIG-B" {
		t.Errorf("서명 = %q / %q", calls[0].Signature, calls[1].Signature)
	}
	if calls[0].ID != "call_1" || calls[1].ID != "call_2" {
		t.Errorf("id = %q / %q", calls[0].ID, calls[1].ID)
	}
}

// index도 id도 없이 두 호출이 이어져 오는 경우.
// 이때 가를 수 있는 근거는 문법뿐이다 — 완결된 JSON 뒤의 `{`는 이어짐일 수 없다.
func TestOpenAIToolCallsSplitOnCompleteJSON(t *testing.T) {
	f := newFake(t)
	f.chunks = []string{
		`{"choices":[{"delta":{"tool_calls":[{"function":{"name":"a","arguments":"{\"x\":1}"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"function":{"name":"b","arguments":"{\"y\":2}"}}]}}]}`,
		`[DONE]`,
	}
	p, _ := Get(OpenAICompatible)
	ch, _ := p.Stream(context.Background(),
		Config{Kind: OpenAICompatible, BaseURL: f.server.URL, APIKey: "k", Model: "m"},
		Request{Messages: []Message{{Role: RoleUser, Text: "x"}}})

	calls := toolCallsOf(collect(t, ch))
	if len(calls) != 2 {
		t.Fatalf("툴 호출 수 = %d, want 2: %+v", len(calls), calls)
	}
	if calls[0].Name != "a" || calls[1].Name != "b" {
		t.Errorf("이름 = %q / %q", calls[0].Name, calls[1].Name)
	}
	if string(calls[0].Input) != `{"x":1}` || string(calls[1].Input) != `{"y":2}` {
		t.Errorf("인자 = %s / %s", calls[0].Input, calls[1].Input)
	}
}

// 같은 호출을 통째로 다시 보내는 구현도 있다. 그것을 새 호출로 가르면
// 같은 툴이 두 번 실행되고, 변경 툴이면 승인 요청이 두 개 생긴다.
func TestOpenAIRepeatedToolCallNotDuplicated(t *testing.T) {
	f := newFake(t)
	f.chunks = []string{
		`{"choices":[{"delta":{"tool_calls":[{"id":"c1","function":{"name":"a","arguments":"{\"x\":1}"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"id":"c1","function":{"name":"a","arguments":"{\"x\":1}"},"extra_content":{"google":{"thought_signature":"SIG"}}}]}}]}`,
		`[DONE]`,
	}
	p, _ := Get(OpenAICompatible)
	ch, _ := p.Stream(context.Background(),
		Config{Kind: OpenAICompatible, BaseURL: f.server.URL, APIKey: "k", Model: "m"},
		Request{Messages: []Message{{Role: RoleUser, Text: "x"}}})

	calls := toolCallsOf(collect(t, ch))
	if len(calls) != 1 {
		t.Fatalf("툴 호출 수 = %d, want 1 (같은 호출의 되풀이다): %+v", len(calls), calls)
	}
	if string(calls[0].Input) != `{"x":1}` {
		t.Errorf("인자 = %s", calls[0].Input)
	}
	// 되풀이된 조각에 실려 온 서명은 흡수해야 한다.
	if calls[0].Signature != "SIG" {
		t.Errorf("서명 = %q — 되풀이 조각의 서명을 버렸다", calls[0].Signature)
	}
}

// 이력에 이미 저장된 깨진 인자는 요청을 만들 때 걸러 낸다.
// 그러지 않으면 그 대화는 새 질문마다 400을 받고 회복되지 않는다.
func TestOpenAIRepairsStoredToolArgs(t *testing.T) {
	f := newFake(t)
	f.chunks = []string{`[DONE]`}
	p, _ := Get(OpenAICompatible)
	ch, _ := p.Stream(context.Background(),
		Config{Kind: OpenAICompatible, BaseURL: f.server.URL, APIKey: "k", Model: "m"},
		Request{Messages: []Message{
			{Role: RoleUser, Text: "질문"},
			{Role: RoleAssistant, ToolCalls: []ToolCall{
				{ID: "c1", Name: "a", Input: json.RawMessage(`{"x":1}{"y":2}`)},
			}},
			{Role: RoleTool, ToolResults: []ToolResult{{CallID: "c1", Content: "실패", IsError: true}}},
		}})
	collect(t, ch)

	msgs, _ := f.lastRequest["messages"].([]any)
	assistant, _ := msgs[1].(map[string]any)
	calls, _ := assistant["tool_calls"].([]any)
	call, _ := calls[0].(map[string]any)
	fn, _ := call["function"].(map[string]any)
	args, _ := fn["arguments"].(string)
	if !json.Valid([]byte(args)) {
		t.Errorf("이력의 인자가 여전히 깨져 있다: %s", args)
	}
	if args != `{"x":1}` {
		t.Errorf("복구된 인자 = %s", args)
	}
}

func TestNormalizeToolArgs(t *testing.T) {
	cases := []struct {
		raw          string
		want         string
		wantRepaired bool
	}{
		{"", "{}", false},
		{"   ", "{}", false},
		{`{"a":1}`, `{"a":1}`, false},
		// 조립이 어긋나 두 값이 이어 붙은 경우 — 첫 값만 살린다.
		{`{"a":1}{"b":2}`, `{"a":1}`, true},
		{`{}{}`, `{}`, true},
		{`{"a":1} {"a":1}`, `{"a":1}`, true},
		// 하나도 건질 수 없으면 빈 객체다. 툴이 "인자가 없다"고 답하고,
		// 그것은 모델이 읽고 고칠 수 있는 오류다.
		{`{"a":`, "{}", true},
		{`쓰레기`, "{}", true},
	}
	for _, tc := range cases {
		got, repaired := NormalizeToolArgs(tc.raw)
		if got != tc.want || repaired != tc.wantRepaired {
			t.Errorf("NormalizeToolArgs(%q) = %q,%v — 기대값 %q,%v",
				tc.raw, got, repaired, tc.want, tc.wantRepaired)
		}
		if !json.Valid([]byte(got)) {
			t.Errorf("NormalizeToolArgs(%q)가 유효하지 않은 JSON을 돌려줬다: %s", tc.raw, got)
		}
	}
}

func TestOpenAIRootHandling(t *testing.T) {
	p := &openaiProvider{}
	cases := []struct{ base, want string }{
		{"", "https://api.openai.com/v1"},
		{"http://localhost:11434/v1", "http://localhost:11434/v1"},
		{"http://localhost:8000", "http://localhost:8000/v1"},
		{"https://gw.corp.com/", "https://gw.corp.com/v1"},
		// Google의 호환 루트는 /v1beta/openai 이고 /v1을 덧붙이면 404다.
		{GoogleCompatBaseURL, GoogleCompatBaseURL},
		{GoogleCompatBaseURL + "/", GoogleCompatBaseURL},
		// 접미사 규칙으로 가를 수 없다는 것을 고정한다: Groq은 /openai/v1이 루트다.
		{"https://api.groq.com/openai", "https://api.groq.com/openai/v1"},
	}
	for _, tc := range cases {
		t.Run(tc.base, func(t *testing.T) {
			if got := p.root(Config{BaseURL: tc.base}); got != tc.want {
				t.Errorf("root = %q, 기대값 %q", got, tc.want)
			}
		})
	}
}

func TestValidateBaseURL(t *testing.T) {
	cases := []struct {
		raw     string
		wantErr string
	}{
		{"", ""},
		{"https://api.openai.com/v1", ""},
		{"http://localhost:11434/v1", ""},
		{"http://127.0.0.1:8000", ""},
		{"http://192.168.0.42:8080/v1", ""},
		{"http://api.openai.com", "사설망"},
		{"ws://x.com", "http 또는 https"},
		{"https://user:pw@gw.com", "자격증명"},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			err := ValidateBaseURL(tc.raw)
			if tc.wantErr == "" {
				if err != nil {
					t.Errorf("허용해야 하는데 거부: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("오류 = %v, %q 를 포함해야 합니다", err, tc.wantErr)
			}
		})
	}
}

// API 키가 리다이렉트로 다른 호스트에 전달되면 안 된다.
func TestNoRedirectFollowing(t *testing.T) {
	var leaked string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked = r.Header.Get("x-api-key") + r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/v1/models", http.StatusMovedPermanently)
	}))
	defer origin.Close()

	p, _ := Get(Anthropic)
	if _, err := p.Models(context.Background(),
		Config{Kind: Anthropic, BaseURL: origin.URL, APIKey: "secret-key"}); err == nil {
		t.Fatal("리다이렉트를 따라가 성공했습니다")
	}
	if leaked != "" {
		t.Errorf("키가 다른 호스트로 전달되었습니다: %q", leaked)
	}
}

func TestUnknownKind(t *testing.T) {
	if _, err := Get(Kind("gemini")); err == nil {
		t.Error("알 수 없는 종류가 통과했습니다")
	}
	if Kind("gemini").Valid() {
		t.Error("Valid()가 true를 반환했습니다")
	}
	if !Anthropic.Valid() || !OpenAICompatible.Valid() {
		t.Error("지원 종류가 거부되었습니다")
	}
}
