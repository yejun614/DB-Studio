package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeOllama는 /api/chat 을 흉내낸다. 보낸 요청을 기록한다.
func fakeOllama(t *testing.T, lines []string) (*httptest.Server, *map[string]any) {
	t.Helper()
	seen := map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/api/tags") {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"models":[{"name":"a:latest","details":{"context_length":8192}},`+
				`{"name":"b:latest","details":{"context_length":0}}]}`)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&seen)
		w.Header().Set("Content-Type", "application/x-ndjson")
		flusher, _ := w.(http.Flusher)
		for _, l := range lines {
			fmt.Fprintln(w, l)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

func drain(t *testing.T, ch <-chan Event) (text, think string, calls []ToolCall, done Event) {
	t.Helper()
	for ev := range ch {
		switch ev.Type {
		case EventText:
			text += ev.Text
		case EventThinking:
			think += ev.Text
		case EventToolCall:
			calls = append(calls, *ev.ToolCall)
		case EventDone:
			done = ev
		case EventError:
			t.Fatalf("스트림 오류: %v", ev.Err)
		}
	}
	return
}

// 컨텍스트 크기가 실제로 요청에 실려야 한다.
//
// 이 어댑터가 존재하는 이유가 그것이다. OpenAI 호환 엔드포인트로는 num_ctx를 보낼
// 방법이 없고, Ollama는 컨텍스트를 넘는 프롬프트를 오류 없이 앞에서 잘라낸다.
func TestOllamaSendsContextSize(t *testing.T) {
	srv, seen := fakeOllama(t, []string{
		`{"message":{"role":"assistant","thinking":"어떻게 답할까. "},"done":false}`,
		`{"message":{"role":"assistant","content":"안녕하세요."},"done":false}`,
		`{"message":{"role":"assistant","content":""},"done":true,"done_reason":"stop",` +
			`"prompt_eval_count":11,"eval_count":22}`,
	})

	p := &ollamaProvider{}
	cfg := Config{Kind: Ollama, BaseURL: srv.URL, Model: "a:latest", ContextTokens: 8192}
	ch, err := p.Stream(context.Background(), cfg, Request{
		Model: "a:latest", System: "너는 도우미다",
		Messages: []Message{{Role: RoleUser, Text: "안녕"}},
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	text, think, _, done := drain(t, ch)

	if text != "안녕하세요." {
		t.Errorf("답 = %q", text)
	}
	if think != "어떻게 답할까. " {
		t.Errorf("생각 = %q", think)
	}
	if done.StopReason != "stop" {
		t.Errorf("끝난 이유 = %q", done.StopReason)
	}
	if done.Usage == nil || done.Usage.InputTokens != 11 || done.Usage.OutputTokens != 22 {
		t.Errorf("사용량 = %+v", done.Usage)
	}

	opts, _ := (*seen)["options"].(map[string]any)
	if opts == nil {
		t.Fatalf("options가 실리지 않았습니다: %v", *seen)
	}
	if n, _ := opts["num_ctx"].(float64); int(n) != 8192 {
		t.Errorf("num_ctx = %v, 기대 8192", opts["num_ctx"])
	}
	// 시스템 프롬프트는 첫 메시지로 간다.
	msgs, _ := (*seen)["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("메시지 %d개: %v", len(msgs), msgs)
	}
	first, _ := msgs[0].(map[string]any)
	if first["role"] != "system" || first["content"] != "너는 도우미다" {
		t.Errorf("첫 메시지 = %v", first)
	}
}

// 툴 호출과 툴 결과가 Ollama 모양으로 오가야 한다.
//
// 두 가지가 OpenAI 규약과 다르다: arguments가 문자열이 아니라 **객체**이고,
// 툴 결과에는 호출 ID 대신 **툴 이름**을 붙인다.
func TestOllamaToolRoundTrip(t *testing.T) {
	srv, seen := fakeOllama(t, []string{
		`{"message":{"role":"assistant","content":"","tool_calls":[` +
			`{"id":"call_x","function":{"name":"get_weather","arguments":{"city":"Seoul"}}}]},"done":false}`,
		`{"message":{"role":"assistant","content":""},"done":true,"done_reason":"stop"}`,
	})

	p := &ollamaProvider{}
	cfg := Config{Kind: Ollama, BaseURL: srv.URL, Model: "a:latest"}
	ch, err := p.Stream(context.Background(), cfg, Request{
		Model: "a:latest",
		Messages: []Message{
			{Role: RoleUser, Text: "날씨"},
			{Role: RoleAssistant, ToolCalls: []ToolCall{
				{ID: "c1", Name: "get_weather", Input: json.RawMessage(`{"city":"Seoul"}`)},
			}},
			{Role: RoleTool, ToolResults: []ToolResult{{CallID: "c1", Content: `{"temp":7}`}}},
		},
		Tools: []Tool{{Name: "get_weather", Description: "날씨", Schema: nil}},
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	_, _, calls, _ := drain(t, ch)

	if len(calls) != 1 || calls[0].Name != "get_weather" || calls[0].ID != "call_x" {
		t.Fatalf("툴 호출 = %+v", calls)
	}
	if got := string(calls[0].Input); got != `{"city":"Seoul"}` {
		t.Errorf("인자 = %s", got)
	}

	msgs, _ := (*seen)["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("메시지 %d개: %v", len(msgs), msgs)
	}
	// 보낸 assistant 호출의 arguments는 객체여야 한다.
	asst, _ := msgs[1].(map[string]any)
	tcs, _ := asst["tool_calls"].([]any)
	if len(tcs) != 1 {
		t.Fatalf("보낸 툴 호출 = %v", asst)
	}
	fn, _ := tcs[0].(map[string]any)["function"].(map[string]any)
	if _, ok := fn["arguments"].(map[string]any); !ok {
		t.Errorf("arguments가 객체가 아닙니다: %T %v", fn["arguments"], fn["arguments"])
	}
	// 툴 결과에는 이름이 붙어야 한다. 앞선 호출에서 찾아 온다.
	tool, _ := msgs[2].(map[string]any)
	if tool["role"] != "tool" || tool["tool_name"] != "get_weather" {
		t.Errorf("툴 결과 = %v", tool)
	}
}

// done_reason을 우리 이름으로 옮긴다.
func TestOllamaStopReasons(t *testing.T) {
	cases := map[string]string{
		"stop":   "stop",
		"length": StopReasonLength,
		"load":   StopReasonIncomplete,
		"unload": StopReasonIncomplete,
	}
	for in, want := range cases {
		if got := ollamaStopReason(in); got != want {
			t.Errorf("ollamaStopReason(%q) = %q, 기대 %q", in, got, want)
		}
	}
}

// done 없이 끊긴 스트림은 끊겼다고 말한다.
func TestOllamaIncompleteStream(t *testing.T) {
	srv, _ := fakeOllama(t, []string{
		`{"message":{"role":"assistant","content":"쓰다가"},"done":false}`,
	})
	p := &ollamaProvider{}
	ch, err := p.Stream(context.Background(),
		Config{Kind: Ollama, BaseURL: srv.URL, Model: "a"}, Request{Model: "a"})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	_, _, _, done := drain(t, ch)
	if done.StopReason != StopReasonIncomplete {
		t.Errorf("끝난 이유 = %q, 기대 %q", done.StopReason, StopReasonIncomplete)
	}
}

// 주소를 어떻게 적든 네이티브 경로를 찾아가야 한다.
//
// OpenAI 호환 주소를 그대로 붙여 넣는 사람이 많다. /v1 을 안 떼면 404가 나고,
// 그 원인은 화면에서 알 수 없다.
func TestOllamaRootTrimsV1(t *testing.T) {
	p := &ollamaProvider{}
	cases := map[string]string{
		"":                              OllamaLocalBaseURL,
		"http://localhost:11434":        "http://localhost:11434",
		"http://localhost:11434/":       "http://localhost:11434",
		"http://localhost:11434/v1":     "http://localhost:11434",
		"http://localhost:11434/v1/":    "http://localhost:11434",
		"https://ollama.com":            "https://ollama.com",
		"https://ollama.com/v1":         "https://ollama.com",
		"  https://ollama.com/v1/     ": "https://ollama.com",
	}
	for in, want := range cases {
		if got := p.root(Config{BaseURL: in}); got != want {
			t.Errorf("root(%q) = %q, 기대 %q", in, got, want)
		}
	}
}

// 키가 없으면 Authorization을 붙이지 않는다.
//
// 로컬 Ollama는 키가 필요 없다. 빈 Bearer를 보내면 Cloud가 400으로 거절한다.
func TestOllamaOmitsEmptyKey(t *testing.T) {
	p := &ollamaProvider{}
	if h := p.headers(Config{}); h != nil {
		t.Errorf("빈 키에 헤더가 붙었습니다: %v", h)
	}
	if h := p.headers(Config{APIKey: "  "}); h != nil {
		t.Errorf("공백 키에 헤더가 붙었습니다: %v", h)
	}
	if h := p.headers(Config{APIKey: "k"}); h["Authorization"] != "Bearer k" {
		t.Errorf("헤더 = %v", h)
	}
}

// 모델 목록과 컨텍스트 크기를 읽는다.
func TestOllamaModelsAndContext(t *testing.T) {
	srv, _ := fakeOllama(t, nil)
	cfg := Config{Kind: Ollama, BaseURL: srv.URL}

	p := &ollamaProvider{}
	models, err := p.Models(context.Background(), cfg)
	if err != nil {
		t.Fatalf("models: %v", err)
	}
	if len(models) != 2 || models[0] != "a:latest" {
		t.Errorf("모델 = %v", models)
	}

	sizes, err := OllamaModelContext(context.Background(), cfg)
	if err != nil {
		t.Fatalf("context: %v", err)
	}
	if sizes["a:latest"] != 8192 {
		t.Errorf("a:latest 컨텍스트 = %d", sizes["a:latest"])
	}
	// 0은 "모름"이므로 지도에 넣지 않는다. 넣으면 화면이 0을 채워 넣는다.
	if _, ok := sizes["b:latest"]; ok {
		t.Errorf("모르는 크기가 지도에 들어갔습니다: %v", sizes)
	}
}

// 모델 하나의 컨텍스트 크기를 물어본다.
//
// 목록(/api/tags)에 크기가 함께 오는 것은 로컬뿐이다. Ollama Cloud는 넣어 주지
// 않는데 모델마다 크기가 크게 다르므로(gpt-oss:120b 131,072 · glm-5.3 1,048,576),
// 고른 모델 하나를 /api/show 로 따로 묻는다.
func TestOllamaShowContext(t *testing.T) {
	var asked map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/api/show") {
			http.NotFound(w, r)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&asked)
		w.Header().Set("Content-Type", "application/json")
		// 열쇠 이름은 아키텍처마다 다르다. 이름을 맞히지 않고 접미사로 찾는다.
		fmt.Fprint(w, `{"model_info":{"general.architecture":"glm_dsa_moe",`+
			`"glm_dsa_moe.embedding_length":0,"glm_dsa_moe.context_length":1048576}}`)
	}))
	defer srv.Close()

	cfg := Config{Kind: Ollama, BaseURL: srv.URL}
	n, err := OllamaShowContext(context.Background(), cfg, "glm-5.3")
	if err != nil {
		t.Fatalf("show: %v", err)
	}
	if n != 1048576 {
		t.Errorf("컨텍스트 = %d, 기대 1048576", n)
	}
	if asked["model"] != "glm-5.3" {
		t.Errorf("물어본 모델 = %q", asked["model"])
	}

	// 이름이 비면 묻지 않는다(요청 한 번을 아낀다).
	if n, err := OllamaShowContext(context.Background(), cfg, "  "); err != nil || n != 0 {
		t.Errorf("빈 이름 = %d, %v", n, err)
	}
}

// 크기를 모르는 응답에는 0을 준다. 오류가 아니다 — 사람이 적으면 된다.
func TestOllamaShowContextUnknown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"model_info":{"general.architecture":"x"}}`)
	}))
	defer srv.Close()

	n, err := OllamaShowContext(context.Background(), Config{BaseURL: srv.URL}, "x")
	if err != nil {
		t.Fatalf("show: %v", err)
	}
	if n != 0 {
		t.Errorf("모르는 크기 = %d, 기대 0", n)
	}
}
