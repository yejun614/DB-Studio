package ai

import (
	"context"
	"encoding/json"
	"strings"
)

// anthropicProvider는 Anthropic Messages API 구현이다.
type anthropicProvider struct{}

func (p *anthropicProvider) Kind() Kind { return Anthropic }

// anthropicVersion은 API 버전 헤더 값이다.
// 고정하지 않으면 기본 버전이 바뀔 때 응답 형태가 달라질 수 있다.
const anthropicVersion = "2023-06-01"

func (p *anthropicProvider) root(cfg Config) string {
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if base == "" {
		return "https://api.anthropic.com"
	}
	return base
}

func (p *anthropicProvider) headers(cfg Config) map[string]string {
	return map[string]string{
		"x-api-key":         cfg.APIKey,
		"anthropic-version": anthropicVersion,
	}
}

func (p *anthropicProvider) Models(ctx context.Context, cfg Config) ([]string, error) {
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := getJSON(ctx, p.root(cfg)+"/v1/models", p.headers(cfg), &out); err != nil {
		return nil, err
	}
	models := make([]string, 0, len(out.Data))
	for _, m := range out.Data {
		models = append(models, m.ID)
	}
	return models, nil
}

// anthropic 요청 본문 구조.
//
// 이 API의 특징 두 가지가 변환에 영향을 준다:
//   - system은 messages 배열이 아니라 최상위 필드다
//   - 툴 결과는 role=user 메시지의 tool_result 블록이다 (전용 role이 없다)
type anthropicRequest struct {
	Model       string             `json:"model"`
	MaxTokens   int                `json:"max_tokens"`
	System      string             `json:"system,omitempty"`
	Messages    []anthropicMessage `json:"messages"`
	Tools       []anthropicTool    `json:"tools,omitempty"`
	Stream      bool               `json:"stream"`
	Temperature *float64           `json:"temperature,omitempty"`
}

type anthropicMessage struct {
	Role    string           `json:"role"`
	Content []anthropicBlock `json:"content"`
}

type anthropicBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	// tool_result
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   string `json:"content,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`
}

type anthropicTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
}

// toAnthropic은 정규화된 메시지를 Anthropic 형식으로 바꾼다.
func toAnthropic(messages []Message) []anthropicMessage {
	out := make([]anthropicMessage, 0, len(messages))
	for _, m := range messages {
		switch m.Role {
		case RoleTool:
			// 툴 결과는 user 메시지로 보낸다. 여러 결과를 한 메시지에 담을 수 있고,
			// 그렇게 하는 것이 병렬 툴 호출에 대한 올바른 응답 형태다.
			blocks := make([]anthropicBlock, 0, len(m.ToolResults))
			for _, r := range m.ToolResults {
				blocks = append(blocks, anthropicBlock{
					Type: "tool_result", ToolUseID: r.CallID,
					Content: r.Content, IsError: r.IsError,
				})
			}
			if len(blocks) == 0 {
				continue
			}
			out = append(out, anthropicMessage{Role: "user", Content: blocks})

		case RoleAssistant:
			blocks := []anthropicBlock{}
			if strings.TrimSpace(m.Text) != "" {
				blocks = append(blocks, anthropicBlock{Type: "text", Text: m.Text})
			}
			for _, tc := range m.ToolCalls {
				// 유효하지 않은 JSON은 여기서 걸러 낸다. 그대로 두면 요청 본문을
				// 만드는 json.Marshal 자체가 실패해 대화가 통째로 멈춘다.
				args, _ := NormalizeToolArgs(string(tc.Input))
				blocks = append(blocks, anthropicBlock{
					Type: "tool_use", ID: tc.ID, Name: tc.Name, Input: json.RawMessage(args),
				})
			}
			if len(blocks) == 0 {
				continue
			}
			out = append(out, anthropicMessage{Role: "assistant", Content: blocks})

		default:
			if strings.TrimSpace(m.Text) == "" {
				continue
			}
			out = append(out, anthropicMessage{
				Role: "user", Content: []anthropicBlock{{Type: "text", Text: m.Text}},
			})
		}
	}
	return out
}

func (p *anthropicProvider) Stream(ctx context.Context, cfg Config, req Request) (<-chan Event, error) {
	model := req.Model
	if model == "" {
		model = cfg.Model
	}
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		// Anthropic은 max_tokens를 필수로 요구한다. 툴을 여러 번 쓰는 대화에서
		// 너무 작으면 응답이 잘리므로 넉넉한 기본값을 둔다.
		maxTokens = 4096
	}

	tools := make([]anthropicTool, 0, len(req.Tools))
	for _, t := range req.Tools {
		schema := t.Schema
		if schema == nil {
			schema = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		tools = append(tools, anthropicTool{
			Name: t.Name, Description: t.Description, InputSchema: schema,
		})
	}

	body := anthropicRequest{
		Model: model, MaxTokens: maxTokens, System: req.System,
		Messages: toAnthropic(req.Messages), Tools: tools,
		Stream: true, Temperature: req.Temperature,
	}

	out := make(chan Event, 32)
	go func() {
		defer close(out)
		streamCtx, cancel := context.WithTimeout(ctx, streamTimeout)
		defer cancel()

		// 툴 인자는 input_json_delta로 조각조각 도착한다. 블록 인덱스별로 모아
		// content_block_stop에서 완성한다.
		type pending struct {
			id   string
			name string
			json strings.Builder
		}
		blocks := map[int]*pending{}
		var stopReason string
		var usage Usage
		// failed는 오류를 이미 내보냈는지 표시한다.
		//
		// 이것이 없으면 오류 뒤에 EventDone이 따라붙어, 마지막 이벤트만 보는 소비자가
		// "정상 종료"로 오해한다. 스트림은 오류 또는 완료 중 하나로만 끝나야 한다.
		failed := false

		err := postSSE(streamCtx, p.root(cfg)+"/v1/messages", p.headers(cfg), body,
			func(raw []byte) bool {
				var ev struct {
					Type  string `json:"type"`
					Index int    `json:"index"`
					Delta struct {
						Type        string `json:"type"`
						Text        string `json:"text"`
						PartialJSON string `json:"partial_json"`
						StopReason  string `json:"stop_reason"`
					} `json:"delta"`
					ContentBlock struct {
						Type  string          `json:"type"`
						ID    string          `json:"id"`
						Name  string          `json:"name"`
						Input json.RawMessage `json:"input"`
					} `json:"content_block"`
					Message struct {
						Usage struct {
							InputTokens  int `json:"input_tokens"`
							OutputTokens int `json:"output_tokens"`
						} `json:"usage"`
					} `json:"message"`
					Usage struct {
						InputTokens  int `json:"input_tokens"`
						OutputTokens int `json:"output_tokens"`
					} `json:"usage"`
					Error struct {
						Type    string `json:"type"`
						Message string `json:"message"`
					} `json:"error"`
				}
				if err := json.Unmarshal(raw, &ev); err != nil {
					// 해석할 수 없는 줄은 무시한다. 프로바이더가 새 이벤트 종류를
					// 추가해도 대화가 끊기지 않아야 한다.
					return true
				}

				switch ev.Type {
				case "message_start":
					usage.InputTokens = ev.Message.Usage.InputTokens

				case "content_block_start":
					if ev.ContentBlock.Type == "tool_use" {
						blocks[ev.Index] = &pending{id: ev.ContentBlock.ID, name: ev.ContentBlock.Name}
					}

				case "content_block_delta":
					switch ev.Delta.Type {
					case "text_delta":
						if ev.Delta.Text != "" {
							return sendEvent(streamCtx, out, Event{Type: EventText, Text: ev.Delta.Text})
						}
					case "input_json_delta":
						if b := blocks[ev.Index]; b != nil {
							b.json.WriteString(ev.Delta.PartialJSON)
						}
					}

				case "content_block_stop":
					b := blocks[ev.Index]
					if b == nil {
						return true
					}
					delete(blocks, ev.Index)
					// 인자가 없는 툴은 partial_json이 오지 않으므로 빈 값이 정상이고,
					// 그때 "{}"가 된다. 그 밖의 깨진 모양도 여기서 유효한 JSON으로
					// 만든다 — 이 값은 이력에 저장되어 이후 모든 요청에 실려 나간다.
					input, _ := NormalizeToolArgs(b.json.String())
					return sendEvent(streamCtx, out, Event{
						Type: EventToolCall,
						ToolCall: &ToolCall{
							ID: b.id, Name: b.name, Input: json.RawMessage(input),
						},
					})

				case "message_delta":
					if ev.Delta.StopReason != "" {
						stopReason = ev.Delta.StopReason
					}
					if ev.Usage.OutputTokens > 0 {
						usage.OutputTokens = ev.Usage.OutputTokens
					}

				case "message_stop":
					return false

				case "error":
					msg := ev.Error.Message
					if msg == "" {
						msg = ev.Error.Type
					}
					failed = true
					sendEvent(streamCtx, out, Event{
						Type: EventError,
						Err:  &APIError{Status: 0, Message: msg, Body: string(raw)},
					})
					return false
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
		sendEvent(streamCtx, out, Event{
			Type: EventDone, StopReason: stopReason, Usage: &usage,
		})
	}()
	return out, nil
}
