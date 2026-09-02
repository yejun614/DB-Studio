package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
)

// openaiProvider는 /v1/chat/completions 규약을 따르는 모든 엔드포인트를 다룬다.
//
// OpenAI 본체뿐 아니라 로컬 LLM(ollama, vLLM, LM Studio)과 게이트웨이가 같은 규약을
// 쓴다. 그래서 base_url을 사용자가 지정할 수 있게 하면 어댑터 하나로 여러 서비스를
// 커버할 수 있다 — 계획 단계에서 어댑터를 두 종류로 정한 근거다.
type openaiProvider struct{}

func (p *openaiProvider) Kind() Kind { return OpenAICompatible }

// GoogleCompatHost는 Google Gemini의 OpenAI 호환 엔드포인트 호스트다.
//
// 이 호스트만 따로 아는 이유는 경로 규칙이 다르기 때문이다: 다른 서비스는 루트가
// `/v1`로 끝나지만 Google은 `/v1beta/openai`이고, 거기에 `/v1`을 덧붙이면 404가 난다.
// 접미사 규칙("openai로 끝나면 붙이지 않는다")으로는 가를 수 없다 — Groq의 루트가
// `/openai/v1`이라 같은 규칙이 반대로 작용한다.
const GoogleCompatHost = "generativelanguage.googleapis.com"

// GoogleCompatBaseURL은 그 기본 주소다. 화면이 안내 문구에 그대로 쓴다.
const GoogleCompatBaseURL = "https://" + GoogleCompatHost + "/v1beta/openai"

func (p *openaiProvider) root(cfg Config) string {
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if base == "" {
		return "https://api.openai.com/v1"
	}
	if isGoogleCompat(base) {
		return base
	}
	// 사용자가 /v1까지 넣었으면 중복해서 붙이지 않는다. 로컬 LLM 문서마다
	// 표기가 달라(http://localhost:11434/v1 vs http://localhost:8000)
	// 이 처리가 없으면 404의 원인을 찾기 어렵다.
	if strings.HasSuffix(base, "/v1") {
		return base
	}
	return base + "/v1"
}

// isGoogleCompat은 주소가 Google의 OpenAI 호환 엔드포인트인지 본다.
func isGoogleCompat(base string) bool {
	u, err := url.Parse(base)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Hostname(), GoogleCompatHost)
}

func (p *openaiProvider) headers(cfg Config) map[string]string {
	return map[string]string{"Authorization": "Bearer " + cfg.APIKey}
}

func (p *openaiProvider) Models(ctx context.Context, cfg Config) ([]string, error) {
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := getJSON(ctx, p.root(cfg)+"/models", p.headers(cfg), &out); err != nil {
		return nil, err
	}
	models := make([]string, 0, len(out.Data))
	for _, m := range out.Data {
		models = append(models, m.ID)
	}
	return models, nil
}

type openaiRequest struct {
	Model    string          `json:"model"`
	Messages []openaiMessage `json:"messages"`
	Tools    []openaiTool    `json:"tools,omitempty"`
	Stream   bool            `json:"stream"`
	// MaxTokens는 0이면 보내지 않는다. 로컬 LLM은 이 필드를 모델 한계보다
	// 크게 주면 거부하는 경우가 있다.
	MaxTokens   int      `json:"max_tokens,omitempty"`
	Temperature *float64 `json:"temperature,omitempty"`
	// StreamOptions는 스트림 마지막에 usage를 받기 위한 OpenAI 확장이다.
	// 지원하지 않는 서버는 무시하므로 항상 보내도 안전하다.
	StreamOptions *openaiStreamOptions `json:"stream_options,omitempty"`
}

type openaiStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type openaiMessage struct {
	Role    string `json:"role"`
	Content string `json:"content,omitempty"`
	// ToolCalls는 assistant 메시지가 요청한 툴 호출이다.
	ToolCalls []openaiToolCall `json:"tool_calls,omitempty"`
	// ToolCallID는 role=tool 메시지가 어느 호출에 대한 결과인지 가리킨다.
	ToolCallID string `json:"tool_call_id,omitempty"`
}

type openaiToolCall struct {
	ID       string             `json:"id"`
	Type     string             `json:"type"`
	Function openaiToolFunction `json:"function"`
	// ExtraContent는 OpenAI 규약에 없는 프로바이더 확장 칸이다.
	//
	// Google이 사고 서명(thought_signature)을 여기에 담아 보내고, 다음 요청에 같은
	// 모양으로 돌려주기를 요구한다. 서명이 없으면 필드를 통째로 생략한다 —
	// 이 칸을 모르는 서버(로컬 LLM 등)에 빈 객체를 보내면 거부당할 수 있다.
	ExtraContent *openaiExtraContent `json:"extra_content,omitempty"`
}

type openaiExtraContent struct {
	Google *openaiGoogleExtra `json:"google,omitempty"`
}

type openaiGoogleExtra struct {
	ThoughtSignature string `json:"thought_signature,omitempty"`
}

// signatureExtra는 서명을 전송 형식으로 감싼다. 서명이 없으면 nil이다.
func signatureExtra(sig string) *openaiExtraContent {
	if strings.TrimSpace(sig) == "" {
		return nil
	}
	return &openaiExtraContent{Google: &openaiGoogleExtra{ThoughtSignature: sig}}
}

type openaiToolFunction struct {
	Name string `json:"name"`
	// Arguments는 JSON 문자열이다 (객체가 아니다). 이 차이가 변환에서 자주 틀린다.
	Arguments string `json:"arguments"`
}

type openaiTool struct {
	Type     string                `json:"type"`
	Function openaiToolFunctionDef `json:"function"`
}

type openaiToolFunctionDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// toOpenAI는 정규화된 메시지를 OpenAI 형식으로 바꾼다.
func toOpenAI(system string, messages []Message) []openaiMessage {
	out := []openaiMessage{}
	if strings.TrimSpace(system) != "" {
		// OpenAI는 system을 messages 배열의 첫 항목으로 받는다 (Anthropic과 다르다).
		out = append(out, openaiMessage{Role: "system", Content: system})
	}
	for _, m := range messages {
		switch m.Role {
		case RoleTool:
			// 툴 결과는 결과마다 별도의 role=tool 메시지다 (Anthropic은 한 메시지에
			// 여러 블록을 담는다). 이 차이를 놓치면 병렬 툴 호출 응답이 깨진다.
			for _, r := range m.ToolResults {
				content := r.Content
				if r.IsError && !strings.HasPrefix(content, "오류") {
					content = "오류: " + content
				}
				out = append(out, openaiMessage{
					Role: "tool", ToolCallID: r.CallID, Content: content,
				})
			}

		case RoleAssistant:
			msg := openaiMessage{Role: "assistant", Content: m.Text}
			for _, tc := range m.ToolCalls {
				// 이력의 인자도 반드시 유효한 JSON이어야 한다. 예전에 잘못 조립되어
				// 저장된 호출이 하나라도 섞여 있으면 프로바이더가 요청 전체를 400으로
				// 거부하고, 그 대화는 새 질문마다 같은 오류를 낸다.
				args, _ := NormalizeToolArgs(string(tc.Input))
				msg.ToolCalls = append(msg.ToolCalls, openaiToolCall{
					ID: tc.ID, Type: "function",
					Function:     openaiToolFunction{Name: tc.Name, Arguments: args},
					ExtraContent: signatureExtra(tc.Signature),
				})
			}
			if msg.Content == "" && len(msg.ToolCalls) == 0 {
				continue
			}
			out = append(out, msg)

		default:
			if strings.TrimSpace(m.Text) == "" {
				continue
			}
			out = append(out, openaiMessage{Role: "user", Content: m.Text})
		}
	}
	return out
}

func (p *openaiProvider) Stream(ctx context.Context, cfg Config, req Request) (<-chan Event, error) {
	model := req.Model
	if model == "" {
		model = cfg.Model
	}
	tools := make([]openaiTool, 0, len(req.Tools))
	for _, t := range req.Tools {
		params := t.Schema
		if params == nil {
			params = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		tools = append(tools, openaiTool{
			Type: "function",
			Function: openaiToolFunctionDef{
				Name: t.Name, Description: t.Description, Parameters: params,
			},
		})
	}

	body := openaiRequest{
		Model: model, Messages: toOpenAI(req.System, req.Messages), Tools: tools,
		Stream: true, MaxTokens: req.MaxTokens, Temperature: req.Temperature,
		StreamOptions: &openaiStreamOptions{IncludeUsage: true},
	}

	out := make(chan Event, 32)
	go func() {
		defer close(out)
		streamCtx, cancel := context.WithTimeout(ctx, streamTimeout)
		defer cancel()

		// 툴 호출은 조각조각 도착하므로 모았다가 스트림이 끝날 때 한꺼번에 내보낸다.
		// 도중에 내보내면 인자가 불완전한 툴 호출을 실행하게 된다.
		//
		// 어느 조각이 어느 호출인지는 슬롯 키로 가른다(자세한 규칙은 toolargs.go).
		calls := map[string]*toolCallBuf{}
		order := []string{}
		lastKey := ""
		var stopReason string
		var usage Usage
		// failed는 오류를 이미 내보냈는지 표시한다. 오류 뒤에 EventDone이 붙으면
		// 마지막 이벤트만 보는 소비자가 정상 종료로 오해한다.
		failed := false

		flush := func() bool {
			for _, key := range order {
				c := calls[key]
				if c == nil || c.name == "" {
					continue
				}
				args, repaired := NormalizeToolArgs(c.args.String())
				if repaired {
					// 여기까지 왔다는 것은 위의 슬롯 분리로도 가르지 못한 모양이 있었다는 뜻이다.
					// 잘라낸 뒤에도 대화는 이어져야 하지만, 무엇을 버렸는지는 남긴다.
					slog.Warn("툴 호출 인자를 복구했습니다 (프로바이더가 규약과 다르게 보냈습니다)",
						"tool", c.name, "raw", truncate(c.args.String(), 300))
				}
				if !sendEvent(streamCtx, out, Event{
					Type: EventToolCall,
					ToolCall: &ToolCall{
						ID: c.id, Name: c.name, Input: json.RawMessage(args), Signature: c.sig,
					},
				}) {
					return false
				}
			}
			calls = map[string]*toolCallBuf{}
			order = nil
			lastKey = ""
			return true
		}

		err := postSSE(streamCtx, p.root(cfg)+"/chat/completions", p.headers(cfg), body,
			func(raw []byte) bool {
				// OpenAI는 스트림 종료를 "data: [DONE]"으로 알린다.
				if string(raw) == "[DONE]" {
					return false
				}
				var chunk struct {
					Choices []struct {
						Delta struct {
							Content string `json:"content"`
							// 생각(reasoning) 델타. 규약에 없는 필드라 서버마다
							// 이름이 다르다 — Ollama는 reasoning, DeepSeek·다수의
							// 호환 서버는 reasoning_content, 일부는 thinking을 쓴다.
							// 셋 다 받는 이유: 안 읽으면 조용히 버려지고, 생각만
							// 하다 끝난 차례는 **빈 답**으로 보인다. 그게 지금
							// 로컬 모델에서 겪는 증상이다.
							Reasoning        string `json:"reasoning"`
							ReasoningContent string `json:"reasoning_content"`
							Thinking         string `json:"thinking"`
							ToolCalls        []struct {
								// Index를 포인터로 받는 이유: 규약은 필수라고 하지만
								// 보내지 않는 구현이 있다(Gemini의 OpenAI 호환 계층).
								// 값 타입으로 받으면 없는 것과 0번이 구분되지 않아
								// 한 턴의 모든 호출이 0번 슬롯에 겹친다.
								Index    *int   `json:"index"`
								ID       string `json:"id"`
								Function struct {
									Name      string `json:"name"`
									Arguments string `json:"arguments"`
								} `json:"function"`
								// Google이 사고 서명을 여기에 실어 보낸다. 규약 밖의
								// 필드지만 읽지 않으면 다음 요청이 거부된다.
								ExtraContent struct {
									Google struct {
										ThoughtSignature string `json:"thought_signature"`
									} `json:"google"`
								} `json:"extra_content"`
							} `json:"tool_calls"`
						} `json:"delta"`
						FinishReason string `json:"finish_reason"`
					} `json:"choices"`
					Usage *struct {
						PromptTokens     int `json:"prompt_tokens"`
						CompletionTokens int `json:"completion_tokens"`
					} `json:"usage"`
					Error *struct {
						Message string `json:"message"`
						Type    string `json:"type"`
					} `json:"error"`
				}
				if err := json.Unmarshal(raw, &chunk); err != nil {
					return true
				}
				if chunk.Error != nil {
					msg := chunk.Error.Message
					if msg == "" {
						msg = chunk.Error.Type
					}
					failed = true
					sendEvent(streamCtx, out, Event{
						Type: EventError,
						Err:  &APIError{Message: msg, Body: string(raw)},
					})
					return false
				}
				if chunk.Usage != nil {
					usage.InputTokens = chunk.Usage.PromptTokens
					usage.OutputTokens = chunk.Usage.CompletionTokens
				}
				for _, ch := range chunk.Choices {
					if think := firstNonEmpty(
						ch.Delta.Reasoning, ch.Delta.ReasoningContent, ch.Delta.Thinking,
					); think != "" {
						if !sendEvent(streamCtx, out, Event{Type: EventThinking, Text: think}) {
							return false
						}
					}
					if ch.Delta.Content != "" {
						if !sendEvent(streamCtx, out, Event{Type: EventText, Text: ch.Delta.Content}) {
							return false
						}
					}
					for _, tc := range ch.Delta.ToolCalls {
						key := slotKey(tc.Index, tc.ID, lastKey)
						c := calls[key]
						frag := tc.Function.Arguments

						// index가 없는 구현에서는 서로 다른 호출이 같은 키로 들어올 수
						// 있다. 이미 완성된 JSON 뒤에 새 객체가 붙는다면 그것은 이어짐이
						// 아니라 **다음 호출**이다 — 붙여 버리면 `{...}{...}` 가 되어
						// 두 호출이 모두 못 쓰게 된다.
						if splitsCall(c.completedArgs(), frag) {
							if c.repeats(tc.Function.Name, frag) {
								// 같은 호출을 통째로 다시 보낸 것이다. 인자는 이미
								// 있으므로 버리고, 이 조각에 실려 온 나머지(서명 등)만
								// 흡수한다. 새 슬롯을 만들면 같은 툴이 두 번 실행된다.
								frag = ""
							} else {
								key = fmt.Sprintf("%s#%d", key, len(order))
								c = nil
							}
						}

						if c == nil {
							c = &toolCallBuf{}
							calls[key] = c
							order = append(order, key)
						}
						lastKey = key

						if tc.ID != "" {
							c.id = tc.ID
						}
						if tc.Function.Name != "" {
							c.name = tc.Function.Name
						}
						if sig := tc.ExtraContent.Google.ThoughtSignature; sig != "" {
							c.sig = sig
						}
						c.args.WriteString(frag)
					}
					if ch.FinishReason != "" {
						stopReason = ch.FinishReason
						if ch.FinishReason == "tool_calls" {
							if !flush() {
								return false
							}
						}
					}
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
		// finish_reason이 오지 않는 서버(일부 로컬 LLM)를 위해 마지막에 한 번 더 비운다.
		if !flush() {
			return
		}
		// 끝났다는 표시 없이 닫혔으면 그렇게 말한다.
		//
		// 예전에는 그냥 성공이었다. 그래서 로컬 LLM이 메모리 부족으로 죽거나
		// 프록시가 끊었을 때, 문장 중간에서 멈춘 답이 **끝까지 온 답과 똑같이**
		// 보였다. 오류로 올리지 않고 이유만 붙이는 이유는 여기까지 온 글은
		// 쓸모가 있고, 판단은 위에서 하기 때문이다.
		if stopReason == "" {
			stopReason = StopReasonIncomplete
		}
		sendEvent(streamCtx, out, Event{Type: EventDone, StopReason: stopReason, Usage: &usage})
	}()
	return out, nil
}

// firstNonEmpty는 처음으로 비어 있지 않은 값이다.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
