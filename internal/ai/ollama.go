package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// ollamaProvider는 Ollama의 **네이티브** API(/api/chat)를 쓴다.
//
// OpenAI 호환 어댑터로도 Ollama에 붙을 수 있는데 왜 따로 두는가:
//
//  1. **컨텍스트 크기를 정할 수 없다.** Ollama의 /v1/chat/completions 는
//     options.num_ctx 를 받지 않는다. 그런데 Ollama는 컨텍스트를 넘는 프롬프트를
//     오류 없이 **앞에서 잘라낸다** — 시스템 프롬프트와 처음의 지시가 먼저 사라지고,
//     모델은 자기가 무엇을 하던 중인지 모르는 채로 답한다. 사람 눈에는 "갑자기
//     엉뚱한 소리를 한다"로 보이고 그 이유는 아무 데도 적히지 않는다.
//     네이티브 API에서는 num_ctx 를 명시할 수 있다.
//  2. 툴 호출이 한 덩어리로 온다. 호환 계층은 조각을 모아 붙여야 하는데,
//     그 자리에서 index 를 안 주는 구현 때문에 이미 한 번 데인 적이 있다.
//  3. 생각(thinking)이 규약 밖 필드가 아니라 message.thinking 으로 온다.
//
// Ollama Cloud(https://ollama.com)도 같은 API를 쓴다. 다른 것은 주소와 API 키뿐이다.
type ollamaProvider struct{}

func (p *ollamaProvider) Kind() Kind { return Ollama }

// OllamaCloudBaseURL은 Ollama Cloud의 주소다. 화면이 안내에 그대로 쓴다.
const OllamaCloudBaseURL = "https://ollama.com"

// OllamaLocalBaseURL은 로컬 Ollama의 기본 주소다.
const OllamaLocalBaseURL = "http://localhost:11434"

func (p *ollamaProvider) root(cfg Config) string {
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if base == "" {
		return OllamaLocalBaseURL
	}
	// OpenAI 호환 주소를 그대로 붙여 넣는 사람이 많다. 네이티브 API는 /v1 아래가
	// 아니므로 떼어 준다 — 안 떼면 404가 나고, 그 원인은 화면에서 알 수 없다.
	return strings.TrimSuffix(base, "/v1")
}

func (p *ollamaProvider) headers(cfg Config) map[string]string {
	// 로컬 Ollama는 키를 무시하고, Cloud는 이것으로 인증한다. 키가 없으면 붙이지
	// 않는다 — 빈 Bearer 를 보내면 Cloud가 400으로 거절한다.
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil
	}
	return map[string]string{"Authorization": "Bearer " + cfg.APIKey}
}

// ollamaTags는 /api/tags 응답이다.
type ollamaTags struct {
	Models []struct {
		Name    string `json:"name"`
		Details struct {
			ContextLength int `json:"context_length"`
		} `json:"details"`
	} `json:"models"`
}

func (p *ollamaProvider) Models(ctx context.Context, cfg Config) ([]string, error) {
	var out ollamaTags
	if err := getJSON(ctx, p.root(cfg)+"/api/tags", p.headers(cfg), &out); err != nil {
		return nil, err
	}
	models := make([]string, 0, len(out.Models))
	for _, m := range out.Models {
		models = append(models, m.Name)
	}
	return models, nil
}

// OllamaModelContext는 모델별 컨텍스트 크기다(토큰). 모르면 0.
//
// 화면이 이것을 써서 컨텍스트 칸을 미리 채운다. 사람이 모델 카드를 찾아 옮겨 적는
// 대신 서버가 아는 값을 쓰는 편이 낫다 — 손으로 적으면 틀리고, 틀린 값은 조용히
// 이력을 지우거나 조용히 넘치게 한다.
//
// 로컬 Ollama는 /api/tags 에 context_length 를 실어 준다. Cloud는 주지 않으므로
// 그때는 0이고, 화면이 사람에게 묻는다.
func OllamaModelContext(ctx context.Context, cfg Config) (map[string]int, error) {
	p := &ollamaProvider{}
	var out ollamaTags
	if err := getJSON(ctx, p.root(cfg)+"/api/tags", p.headers(cfg), &out); err != nil {
		return nil, err
	}
	sizes := map[string]int{}
	for _, m := range out.Models {
		if m.Details.ContextLength > 0 {
			sizes[m.Name] = m.Details.ContextLength
		}
	}
	return sizes, nil
}

// ---------- 요청 ----------

type ollamaRequest struct {
	Model    string          `json:"model"`
	Messages []ollamaMessage `json:"messages"`
	Tools    []openaiTool    `json:"tools,omitempty"`
	Stream   bool            `json:"stream"`
	Options  *ollamaOptions  `json:"options,omitempty"`
}

type ollamaOptions struct {
	// NumCtx는 이 요청이 쓸 컨텍스트 크기다. 이 어댑터가 존재하는 이유다.
	NumCtx int `json:"num_ctx,omitempty"`
	// NumPredict는 생성할 토큰 수의 상한이다. 0이면 보내지 않는다(무제한).
	NumPredict  int      `json:"num_predict,omitempty"`
	Temperature *float64 `json:"temperature,omitempty"`
}

type ollamaMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	// ToolName은 이 결과가 어느 툴의 것인지다(role=tool 일 때).
	ToolName  string           `json:"tool_name,omitempty"`
	ToolCalls []ollamaToolCall `json:"tool_calls,omitempty"`
}

type ollamaToolCall struct {
	ID       string             `json:"id,omitempty"`
	Function ollamaToolCallFunc `json:"function"`
}

type ollamaToolCallFunc struct {
	Name string `json:"name"`
	// Arguments는 **객체**다. OpenAI 규약처럼 문자열이 아니다.
	Arguments json.RawMessage `json:"arguments"`
}

// ollamaMessages는 정규화된 대화를 Ollama 형식으로 옮긴다.
func ollamaMessages(system string, msgs []Message) []ollamaMessage {
	out := make([]ollamaMessage, 0, len(msgs)+1)
	if strings.TrimSpace(system) != "" {
		out = append(out, ollamaMessage{Role: "system", Content: system})
	}

	// 툴 결과에는 이름을 붙여야 한다. 우리 ToolResult는 호출 ID만 들고 있으므로
	// 앞선 assistant 메시지의 호출에서 이름을 찾아 온다.
	names := map[string]string{}
	for _, m := range msgs {
		for _, c := range m.ToolCalls {
			names[c.ID] = c.Name
		}
	}

	for _, m := range msgs {
		switch m.Role {
		case RoleTool:
			for _, r := range m.ToolResults {
				out = append(out, ollamaMessage{
					Role: "tool", ToolName: names[r.CallID], Content: r.Content,
				})
			}
		case RoleAssistant:
			msg := ollamaMessage{Role: "assistant", Content: m.Text}
			for _, c := range m.ToolCalls {
				args := c.Input
				// 인자가 비어 있으면 빈 객체로 보낸다. null 을 보내면 거절하는
				// 모델이 있다.
				if len(args) == 0 {
					args = json.RawMessage(`{}`)
				}
				msg.ToolCalls = append(msg.ToolCalls, ollamaToolCall{
					ID: c.ID, Function: ollamaToolCallFunc{Name: c.Name, Arguments: args},
				})
			}
			out = append(out, msg)
		default:
			out = append(out, ollamaMessage{Role: "user", Content: m.Text})
		}
	}
	return out
}

// ---------- 응답 ----------

type ollamaChunk struct {
	Message struct {
		Content   string           `json:"content"`
		Thinking  string           `json:"thinking"`
		ToolCalls []ollamaToolCall `json:"tool_calls"`
	} `json:"message"`
	Done       bool   `json:"done"`
	DoneReason string `json:"done_reason"`
	// 토큰 사용량. 마지막 조각에만 실린다.
	PromptEvalCount int    `json:"prompt_eval_count"`
	EvalCount       int    `json:"eval_count"`
	Error           string `json:"error"`
}

func (p *ollamaProvider) Stream(ctx context.Context, cfg Config, req Request) (<-chan Event, error) {
	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = strings.TrimSpace(cfg.Model)
	}
	if model == "" {
		return nil, fmt.Errorf("모델 이름이 필요합니다")
	}

	body := ollamaRequest{
		Model:    model,
		Messages: ollamaMessages(req.System, req.Messages),
		Tools:    openaiTools(req.Tools),
		Stream:   true,
	}
	if cfg.ContextTokens > 0 || req.MaxTokens > 0 || req.Temperature != nil {
		body.Options = &ollamaOptions{
			NumCtx: cfg.ContextTokens, NumPredict: req.MaxTokens, Temperature: req.Temperature,
		}
	}

	out := make(chan Event, 16)
	streamCtx, cancel := context.WithTimeout(ctx, streamTimeout)
	go func() {
		defer close(out)
		defer cancel()

		stopReason := ""
		usage := Usage{}
		failed := false
		calls := 0

		err := postNDJSON(streamCtx, p.root(cfg)+"/api/chat", p.headers(cfg), body,
			func(raw []byte) bool {
				var ch ollamaChunk
				if err := json.Unmarshal(raw, &ch); err != nil {
					// 한 줄을 못 읽는다고 대화를 끝내지 않는다. 다음 줄이 멀쩡할 수 있다.
					return true
				}
				if ch.Error != "" {
					failed = true
					sendEvent(streamCtx, out, Event{
						Type: EventError,
						Err:  &APIError{Message: ch.Error, Body: string(raw)},
					})
					return false
				}
				if think := ch.Message.Thinking; think != "" {
					if !sendEvent(streamCtx, out, Event{Type: EventThinking, Text: think}) {
						return false
					}
				}
				if text := ch.Message.Content; text != "" {
					if !sendEvent(streamCtx, out, Event{Type: EventText, Text: text}) {
						return false
					}
				}
				for _, tc := range ch.Message.ToolCalls {
					calls++
					id := tc.ID
					// ID를 주지 않는 판이 있다. 우리 쪽에서 만들어 붙인다 —
					// 툴 결과를 어느 호출에 대응시킬지가 이것으로 정해진다.
					if id == "" {
						id = fmt.Sprintf("call_%d", calls)
					}
					args := tc.Function.Arguments
					if len(args) == 0 {
						args = json.RawMessage(`{}`)
					}
					if !sendEvent(streamCtx, out, Event{
						Type:     EventToolCall,
						ToolCall: &ToolCall{ID: id, Name: tc.Function.Name, Input: args},
					}) {
						return false
					}
				}
				if ch.Done {
					stopReason = ch.DoneReason
					usage.InputTokens = ch.PromptEvalCount
					usage.OutputTokens = ch.EvalCount
				}
				return true
			})

		if err != nil {
			sendEvent(streamCtx, out, Event{Type: EventError, Err: err})
			return
		}
		if failed {
			return
		}
		// done 없이 닫혔으면 그렇게 말한다(openai 어댑터와 같은 이유).
		if stopReason == "" {
			stopReason = StopReasonIncomplete
		}
		sendEvent(streamCtx, out, Event{
			Type: EventDone, StopReason: ollamaStopReason(stopReason), Usage: &usage,
		})
	}()
	return out, nil
}

// ollamaStopReason은 Ollama의 done_reason을 우리 이름으로 옮긴다.
func ollamaStopReason(reason string) string {
	switch reason {
	case "length":
		return StopReasonLength
	case "load", "unload":
		// 모델을 올리고 내리느라 끝난 것이지 답을 마친 것이 아니다.
		return StopReasonIncomplete
	default:
		return reason
	}
}
