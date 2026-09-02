// Package ai는 LLM 프로바이더 어댑터를 제공한다.
//
// 어댑터가 두 종류인 이유(계획에서 결정됨): ① Anthropic 네이티브 Messages API,
// ② OpenAI 호환 chat/completions. 두 번째는 base_url을 사용자가 지정할 수 있어
// OpenAI 본체·로컬 LLM(ollama, vLLM)·게이트웨이를 하나의 어댑터로 커버한다.
//
// 이 패키지는 앱의 도메인을 모른다. 툴의 "의미"는 호출자가 정하고, 여기서는
// 툴 정의를 프로바이더 형식으로 옮기고 모델이 호출한 툴을 정규화된 형태로 돌려준다.
// 그래서 에이전트 루프(툴 실행·권한 판정)는 api 계층에 있고, 여기에는 프로토콜만 있다.
package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Kind는 프로바이더 종류다.
type Kind string

const (
	// Anthropic은 Messages API를 쓴다.
	Anthropic Kind = "anthropic"
	// OpenAICompatible은 /v1/chat/completions 규약을 따르는 모든 엔드포인트다.
	OpenAICompatible Kind = "openai"
)

func (k Kind) Valid() bool {
	return k == Anthropic || k == OpenAICompatible
}

func (k Kind) Label() string {
	switch k {
	case Anthropic:
		return "Anthropic"
	case OpenAICompatible:
		return "OpenAI 호환"
	}
	return string(k)
}

// Config는 한 프로바이더에 접근하기 위한 설정이다.
type Config struct {
	Kind Kind
	// BaseURL이 비어 있으면 각 서비스의 기본 주소를 쓴다.
	BaseURL string
	APIKey  string
	Model   string
}

// Role은 대화 참여자다.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	// RoleTool은 툴 실행 결과를 담는 메시지다. 프로바이더별로 표현이 다르므로
	// (Anthropic은 user 메시지의 tool_result 블록, OpenAI는 role=tool)
	// 정규화된 형태로 두고 어댑터가 변환한다.
	RoleTool Role = "tool"
)

// Message는 대화의 한 항목이다.
type Message struct {
	Role Role   `json:"role"`
	Text string `json:"text,omitempty"`
	// ToolCalls는 assistant가 요청한 툴 호출이다.
	ToolCalls []ToolCall `json:"toolCalls,omitempty"`
	// ToolResults는 RoleTool 메시지가 담는 실행 결과다.
	ToolResults []ToolResult `json:"toolResults,omitempty"`
}

// ToolCall은 모델이 요청한 툴 호출이다.
type ToolCall struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
	// Signature는 프로바이더가 이 호출에 붙여 보낸 불투명한 값이다.
	//
	// Gemini 같은 사고(thinking) 모델은 툴 호출마다 사고 과정의 서명을 함께 주고,
	// 다음 요청에 **그대로 돌려주기를 요구한다.** 돌려주지 않으면 400으로 거부한다
	// ("Function call is missing a thought_signature in functionCall parts").
	//
	// 우리가 해석할 값이 아니다. 받은 그대로 보관했다가 그대로 되돌려 보낸다 —
	// 그래서 이름도 프로바이더 중립으로 두었고, 대화 이력에 함께 저장된다
	// (저장하지 않으면 새로고침 뒤 이어지는 대화가 같은 오류로 죽는다).
	Signature string `json:"signature,omitempty"`
}

// ToolResult는 툴 실행 결과다.
type ToolResult struct {
	CallID  string `json:"callId"`
	Content string `json:"content"`
	IsError bool   `json:"isError,omitempty"`
}

// Tool은 모델에게 노출하는 툴 정의다.
type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Schema      map[string]any `json:"schema"`
}

// Request는 한 번의 생성 요청이다.
type Request struct {
	Model     string
	System    string
	Messages  []Message
	Tools     []Tool
	MaxTokens int
	// Temperature가 nil이면 프로바이더 기본값을 쓴다.
	Temperature *float64
}

// EventType은 스트림 이벤트 종류다.
type EventType string

const (
	// EventText는 생성된 텍스트 조각이다.
	EventText EventType = "text"
	// EventToolCall은 완성된 툴 호출이다 (인자 JSON이 모두 도착한 뒤 한 번).
	EventToolCall EventType = "tool_call"
	// EventThinking은 모델이 생각하는 동안 나오는 글이다.
	//
	// 답과 나눠 두는 이유: 생각은 답이 아니다. 답에 섞으면 사람이 그것을 결론으로
	// 읽고, 이력에 저장하면 다음 차례의 문맥을 쓸데없이 채운다. 화면에는 "지금
	// 생각하고 있다"는 신호로만 쓴다.
	EventThinking EventType = "thinking"
	// EventDone은 응답이 끝났음을 알린다.
	EventDone EventType = "done"
	// EventError는 스트림 중 발생한 오류다.
	EventError EventType = "error"
)

// Event는 스트림에서 나오는 하나의 사건이다.
type Event struct {
	Type       EventType
	Text       string
	ToolCall   *ToolCall
	StopReason string
	Usage      *Usage
	Err        error
}

// 스트림이 끝난 이유. 프로바이더마다 이름이 다르므로 여기서 하나로 모은다.
const (
	// StopReasonLength는 길이 한계로 잘렸다는 뜻이다(OpenAI: length,
	// Anthropic: max_tokens). 답이 문장 중간에서 끊긴다.
	StopReasonLength = "length"
	// StopReasonIncomplete는 끝났다는 표시 없이 연결이 닫혔다는 뜻이다.
	//
	// 이것을 따로 두는 이유: 그냥 성공으로 넘기면 끊긴 답과 끝까지 온 답을
	// 구별할 수 없다. 로컬 LLM이 메모리 부족으로 죽거나 프록시가 시간 초과로
	// 끊었을 때가 정확히 이 경우다.
	StopReasonIncomplete = "incomplete"
)

// Usage는 토큰 사용량이다. 비용 추적과 컨텍스트 관리에 쓴다.
type Usage struct {
	InputTokens  int `json:"inputTokens"`
	OutputTokens int `json:"outputTokens"`
}

// Provider는 프로바이더별 구현이다.
type Provider interface {
	Kind() Kind

	// Models는 사용 가능한 모델 목록을 조회한다.
	// 지원하지 않으면 ErrNotSupported를 반환한다 — 그때 UI는 직접 입력을 받는다.
	Models(ctx context.Context, cfg Config) ([]string, error)

	// Stream은 응답을 스트리밍한다.
	//
	// 채널은 EventDone 또는 EventError로 끝나고 반드시 닫힌다. 호출자가 ctx를
	// 취소하면 스트림이 중단되고 채널이 닫힌다 — 사용자가 화면을 떠났을 때
	// 요청이 계속 살아 있으면 토큰과 커넥션이 새어나간다.
	Stream(ctx context.Context, cfg Config, req Request) (<-chan Event, error)
}

// ErrNotSupported는 프로바이더가 그 기능을 제공하지 않음을 뜻한다.
var ErrNotSupported = errors.New("이 프로바이더는 지원하지 않는 기능입니다")

// Get은 종류에 맞는 프로바이더를 반환한다.
func Get(kind Kind) (Provider, error) {
	switch kind {
	case Anthropic:
		return &anthropicProvider{}, nil
	case OpenAICompatible:
		return &openaiProvider{}, nil
	}
	return nil, fmt.Errorf("지원하지 않는 AI 프로바이더입니다: %s", kind)
}

// ---------- HTTP ----------

// streamTimeout은 스트리밍 응답 전체의 상한이다.
// 툴을 여러 번 호출하는 대화는 길어질 수 있어 넉넉하게 둔다.
const streamTimeout = 10 * time.Minute

// client는 공용 HTTP 클라이언트다.
//
// Timeout을 설정하지 않는 이유: 스트리밍 응답은 본문을 읽는 동안 연결이 열려 있어야
// 하고, Client.Timeout은 본문 읽기까지 포함하므로 긴 응답이 중간에 끊긴다.
// 대신 호출자가 context로 제어하고, 연결 단계에만 상한을 둔다.
var client = &http.Client{
	Transport: &http.Transport{
		ResponseHeaderTimeout: 60 * time.Second,
		IdleConnTimeout:       90 * time.Second,
	},
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		// API 키가 담긴 헤더가 다른 호스트로 전달되면 안 된다.
		return errors.New("리다이렉트를 따르지 않습니다 (API 키 유출 방지)")
	},
}

// APIError는 프로바이더가 돌려준 오류다.
type APIError struct {
	Status  int
	Message string
	Body    string
}

func (e *APIError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("HTTP %d: %s", e.Status, e.Message)
	}
	return fmt.Sprintf("HTTP %d: %s", e.Status, truncate(e.Body, 300))
}

// ValidateBaseURL은 프로바이더 주소를 검증한다.
//
// vcs 패키지와 같은 규칙이다: 평문 http로 API 키를 보내지 않도록 https를 요구하되,
// 로컬 LLM(ollama 등)은 http://localhost로 도는 것이 정상이므로 사설망은 허용한다.
func ValidateBaseURL(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("주소를 해석할 수 없습니다: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("주소는 http 또는 https여야 합니다 (받은 값: %s)", u.Scheme)
	}
	if u.Host == "" {
		return errors.New("주소에 호스트가 없습니다")
	}
	if u.User != nil {
		return errors.New("주소에 자격증명을 넣지 마세요. API 키는 별도 필드에 입력합니다")
	}
	if u.Scheme == "http" && !isPrivateHost(u.Hostname()) {
		return errors.New("http는 사설망 주소에만 허용됩니다. API 키가 평문으로 전송되므로 https를 사용하세요")
	}
	return nil
}

func truncate(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// sendEvent는 채널에 이벤트를 보내되 컨텍스트 취소를 존중한다.
// 이것을 빠뜨리면 소비자가 사라진 뒤 goroutine이 영구히 블록된다.
func sendEvent(ctx context.Context, out chan<- Event, ev Event) bool {
	select {
	case out <- ev:
		return true
	case <-ctx.Done():
		return false
	}
}
