package api

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"dbstudio/internal/ai"
	"dbstudio/internal/model"
	"dbstudio/internal/store"
)

// maxToolRounds는 한 번의 사용자 메시지에 허용할 툴 왕복 횟수다.
//
// 상한이 필요한 이유: 모델이 같은 툴을 반복 호출하는 루프에 빠지면 토큰과 시간이
// 무한히 소모된다. 실무에서 5~6회면 대부분의 조사가 끝나므로 8로 둔다.
const maxToolRounds = 8

// systemPrompt는 어시스턴트의 역할과 제약을 알려준다.
//
// 여기에 "쓰기는 제안만 가능하다"를 명시하는 이유: 그 사실을 모르면 모델이 실행에
// 성공했다고 사용자에게 말한 뒤 실제로는 승인 대기 중인 상황이 생긴다.
const systemPrompt = `당신은 DB Studio의 어시스턴트입니다. 이 앱은 여러 데이터베이스(개발/운영)의
접근 권한, 모니터링, 로그 분석, ERD 설계, 스키마 버전과 마이그레이션, Git 연동을 관리합니다.

원칙:
- 툴은 지금 대화하는 사용자의 권한으로 실행됩니다. 권한이 없다는 오류가 오면 그 사실을
  사용자에게 그대로 알리고, 우회를 시도하지 마세요.
- 쓰기·변경 작업(초안 생성, 마이그레이션 생성/실행, 버전 확정, Git 푸시)은 당신이 직접
  실행할 수 없습니다. 그런 툴을 호출하면 "제안"이 만들어지고 사용자가 화면에서 승인해야
  실행됩니다. 그러므로 "실행했습니다"가 아니라 "승인을 요청했습니다"라고 말하세요.
- **예외는 ERD 초안 편집(erd_ 로 시작하는 툴)입니다.** 이것은 바로 반영되므로
  "반영했습니다"라고 말하세요. 초안은 실제 데이터베이스가 아니고, 모든 변경이 편집
  이력에 남으며 같은 문서를 열어 둔 사람의 화면에도 그 자리에서 나타납니다.
  고치기 전에 erd_read_schema 로 지금 상태를 보고, 어느 초안인지 모르면
  list_erd_documents 를 먼저 부르세요(document 인자에 이름이나 ID를 넘깁니다).
  그 초안을 실제 DB에 반영하는 것은 마이그레이션이고, 그것은 여전히 승인이 필요합니다.
  초안을 지우는 툴은 없습니다 — 삭제가 필요하면 화면에서 하도록 안내하세요.
- 운영(prod) 데이터베이스를 다룰 때는 위험을 먼저 설명하세요. 파괴적 변경(테이블·컬럼 삭제,
  타입 축소)은 데이터 손실을 뜻합니다.
- 추측하지 말고 툴로 확인하세요. 커넥션 이름을 모르면 list_connections를 먼저 부르세요.
- 답변은 한국어로, 간결하게. 숫자와 이름은 툴 결과에서 가져온 값을 그대로 쓰세요.`

// sessionPrompt는 이 대화에만 해당하는 사실을 시스템 프롬프트에 덧붙인다.
//
// 대상 DB를 알려주는 것이 핵심이다. 사용자는 대화를 만들 때 DB를 고르고 화면 머리에
// 그 이름을 보고 있는데, 모델에게 그 사실을 전하지 않으면 "어느 DB를 볼까요?"라고
// 되묻는다. 사용자 입장에서는 이미 고른 것을 다시 묻는 셈이라 앱이 고장 난 것처럼 보인다.
//
// 이름만 주지 않고 종류·환경까지 주는 이유: 모델이 방언(MySQL vs PostgreSQL)과
// 위험도(운영이면 파괴적 변경을 먼저 경고해야 한다)를 판단할 근거가 된다.
func sessionPrompt(conn *model.Connection) string {
	if conn == nil {
		return systemPrompt
	}
	env := "개발"
	if conn.Environment == model.EnvProd {
		env = "운영"
	}
	return systemPrompt + fmt.Sprintf(`

이 대화의 대상 데이터베이스는 %q 입니다 (종류: %s, 환경: %s). 사용자가 다른 DB를 명시하지
않으면 이 커넥션을 쓰세요 — 어느 DB인지 다시 묻지 마세요. 툴의 connection 인자에는
이 이름을 그대로 넘기면 됩니다.`, conn.Name, conn.Kind, env)
}

// erdSystemPrompt는 ERD 초안 대화의 지침이다.
//
// 앱 전체 어시스턴트의 지침을 그대로 쓰지 않는 이유: 저쪽은 "쓰기는 제안만 가능하다"를
// 전제로 말하는데 여기서는 툴이 바로 적용된다. 그 차이를 알려주지 않으면 모델이
// "승인을 요청했습니다"라고 말하고, 사용자는 승인 버튼을 찾다가 이미 반영된 것을
// 발견하게 된다.
func erdSystemPrompt(docName, dialect string) string {
	return fmt.Sprintf(`당신은 DB Studio의 ERD 설계 도우미입니다. 지금 %q 초안(문법: %s) 하나를
사용자와 함께 설계하고 있습니다.

원칙:
- 툴로 만든 변경은 **즉시 초안에 반영되고 같은 문서를 열어 둔 모든 사람에게 보입니다.**
  승인 단계가 없으므로 "승인을 요청했습니다"가 아니라 "반영했습니다"라고 말하세요.
- 고치기 전에 read_schema로 지금 상태를 확인하세요. 이름이 겹치거나 없는 테이블을
  참조하면 툴이 거부합니다.
- 이것은 초안입니다. 실제 데이터베이스는 사용자가 마이그레이션을 만들어 실행할 때
  바뀌며, 그 단계는 이 대화 밖에 있습니다.
- 지우는 툴은 제공되지 않습니다. 삭제가 필요하면 화면에서 직접 하도록 안내하세요.
- 타입은 %s 문법에 맞는 것을 쓰세요.
- 답변은 한국어로, 간결하게. 무엇을 왜 그렇게 했는지 한두 줄로 덧붙이세요 —
  이 대화는 나중에 다른 사람과 공유될 수 있습니다.`, docName, dialect, dialect)
}

// ---------- SSE 전송 ----------

// sseWriter는 SSE 이벤트를 쓴다.
//
// fasthttp의 스트림 라이터는 핸들러가 반환한 뒤 실행되므로 *fiber.Ctx를 만질 수 없다.
// 필요한 것은 bufio.Writer 하나뿐이다.
type sseWriter struct {
	w *bufio.Writer
}

// send는 이벤트를 쓰고 즉시 플러시한다.
//
// 플러시하지 않으면 버퍼가 찰 때까지 브라우저에 아무것도 도착하지 않아 "스트리밍"이
// 아니게 된다. 쓰기 오류는 클라이언트가 끊었다는 뜻이므로 그대로 반환해 루프를 멈춘다.
func (s *sseWriter) send(event string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", event, data); err != nil {
		return err
	}
	return s.w.Flush()
}

// ---------- 에이전트 루프 ----------

// agentRun은 한 번의 사용자 메시지 처리에 필요한 상태다.
type agentRun struct {
	srv     *Server
	tc      *toolContext
	session *store.AISession
	cfg     ai.Config
	// system은 이 대화의 시스템 프롬프트다 (대상 DB 안내가 붙어 있을 수 있다).
	system   string
	tools    []ai.Tool
	registry map[string]*aiTool
	// erd가 채워져 있으면 이 대화는 ERD 초안 하나를 고치는 대화다.
	// 그때는 registry 대신 이 상자의 툴을 쓴다(승인 없이 바로 적용된다 —
	// 그 판단의 근거는 ai_tools_erd.go에 적어 두었다).
	erd      *erdToolContext
	erdTools map[string]*erdTool
	out      *sseWriter
}

// run은 사용자 메시지에 대한 응답을 생성한다.
//
// 흐름: 대화 이력 + 툴 정의 → 프로바이더 스트림 → 텍스트는 그대로 중계,
// 툴 호출은 실행(읽기) 또는 제안 생성(쓰기) → 결과를 붙여 다시 요청.
// 쓰기 제안이 하나라도 생기면 루프를 멈춘다 — 사용자의 승인 없이는 다음 단계를
// 진행할 수 없고, 승인 후 새 요청으로 대화가 이어진다.
func (r *agentRun) run(ctx context.Context, history []ai.Message) {
	provider, err := ai.Get(r.cfg.Kind)
	if err != nil {
		r.fail(err)
		return
	}

	for round := 0; round < maxToolRounds; round++ {
		stream, err := provider.Stream(ctx, r.cfg, ai.Request{
			Model: r.cfg.Model, System: r.system,
			Messages: history, Tools: r.tools,
		})
		if err != nil {
			r.fail(err)
			return
		}

		var text strings.Builder
		var calls []ai.ToolCall
		var usage ai.Usage
		stopReason := ""
		streamErr := error(nil)

		for ev := range stream {
			switch ev.Type {
			case ai.EventThinking:
				// 생각은 저장하지 않고 화면으로만 보낸다. 이력에 쌓으면 다음
				// 차례의 문맥을 결론이 아닌 글로 채우고, 새로고침하면 어차피
				// 사라질 것을 DB에 남기게 된다.
				if err := r.out.send("thinking", map[string]string{"text": ev.Text}); err != nil {
					r.saveAssistant(text.String(), nil, usage, "")
					return
				}
			case ai.EventText:
				text.WriteString(ev.Text)
				if err := r.out.send("text", map[string]string{"text": ev.Text}); err != nil {
					// 클라이언트가 끊었다. 지금까지의 응답은 저장하고 조용히 끝낸다.
					r.saveAssistant(text.String(), nil, usage, "")
					return
				}
			case ai.EventToolCall:
				if ev.ToolCall != nil {
					calls = append(calls, *ev.ToolCall)
				}
			case ai.EventDone:
				stopReason = ev.StopReason
				if ev.Usage != nil {
					usage = *ev.Usage
				}
			case ai.EventError:
				streamErr = ev.Err
			}
		}

		if streamErr != nil {
			r.saveAssistant(text.String(), calls, usage, streamErr.Error())
			r.fail(streamErr)
			return
		}

		msgID := r.saveAssistant(text.String(), calls, usage, "")
		if usage.InputTokens > 0 || usage.OutputTokens > 0 {
			_ = r.srv.st.AddAISessionUsage(r.tc.ctx, r.session.ID,
				usage.InputTokens, usage.OutputTokens)
		}
		// 답이 온전히 끝난 것인지 알린다.
		//
		// 예전에는 이 값을 읽지 않았다. 그래서 길이 한계로 문장 중간에서 잘린
		// 답과 끝까지 온 답이 화면에서 똑같아 보였다 — 사람은 모델이 그렇게
		// 답했다고 믿고, 잘렸다는 사실은 아무 데도 적히지 않았다.
		if note := stopNotice(stopReason); note != "" {
			_ = r.out.send("notice", map[string]string{"message": note})
		}
		if len(calls) == 0 {
			// 툴을 부르지 않았으면 이 턴은 끝이다.
			r.done("end")
			return
		}

		history = append(history, ai.Message{
			Role: ai.RoleAssistant, Text: text.String(), ToolCalls: calls,
		})

		results, paused := r.executeCalls(ctx, msgID, calls)
		if len(results) > 0 {
			// 툴 결과도 대화 이력에 남긴다. 새로고침 후에도 무엇을 조회했는지 보인다.
			if err := r.srv.st.AddAIMessage(r.tc.ctx, &store.AIMessage{
				SessionID: r.session.ID, Role: string(ai.RoleTool), ToolResults: results,
			}); err != nil {
				slog.Error("AI 툴 결과 저장 실패", "session", r.session.ID, "error", err)
			}
			history = append(history, ai.Message{Role: ai.RoleTool, ToolResults: results})
		}
		if paused {
			// 승인 대기. 사용자가 결정하면 별도 요청으로 대화가 이어진다.
			r.done("awaiting_approval")
			return
		}
	}

	// 상한에 걸렸다. 조용히 끊으면 사용자는 응답이 멈춘 이유를 모른다.
	_ = r.out.send("notice", map[string]string{
		"message": fmt.Sprintf("툴 호출이 %d회를 넘어 중단했습니다. 질문을 좁혀 다시 시도하세요", maxToolRounds),
	})
	r.done("max_rounds")
}

// stopNotice는 끝난 이유가 사람에게 알릴 만한 것이면 그 문장이다.
//
// 정상 종료(stop·tool_calls·빈 값)에는 아무 말도 하지 않는다. 잘 끝난 답마다
// 안내가 붙으면 그 안내는 곧 배경이 되고, 정작 잘린 답에서도 읽히지 않는다.
func stopNotice(reason string) string {
	switch reason {
	case ai.StopReasonLength:
		return "모델의 길이 한계에 닿아 답이 중간에서 끊겼습니다. " +
			"생각이 긴 모델은 생각에도 이 한계를 씁니다 — 질문을 좁히거나, " +
			"이어서 답해 달라고 요청하세요"
	case ai.StopReasonIncomplete:
		return "응답이 끝났다는 표시 없이 연결이 끊겼습니다. 답이 중간에서 " +
			"멈췄을 수 있습니다 — 로컬 모델이라면 메모리가 모자라 내려갔는지 확인하세요"
	default:
		return ""
	}
}

// executeCalls는 툴 호출을 처리한다.
//
// 반환값 paused가 true면 쓰기 제안이 만들어졌다는 뜻이다.
func (r *agentRun) executeCalls(ctx context.Context, msgID int64, calls []ai.ToolCall) ([]ai.ToolResult, bool) {
	if r.erd != nil {
		return r.executeERDCalls(calls), false
	}

	results := []ai.ToolResult{}
	paused := false

	for _, call := range calls {
		tool := r.registry[call.Name]

		// 호출 사실을 먼저 알린다. 알 수 없는 툴이거나 권한이 없어 거절되는 경우에도
		// 사용자는 무엇이 시도됐는지 봐야 한다 — 화면에 아무것도 나오지 않으면
		// 모델이 왜 갑자기 "할 수 없다"고 답하는지 알 수 없다.
		_ = r.out.send("tool_call", map[string]any{
			"id": call.ID, "name": call.Name,
			"arguments": json.RawMessage(call.Input),
			"mutating":  tool != nil && tool.Mutating,
		})

		if tool == nil {
			results = append(results,
				r.failCall(call, fmt.Sprintf("%q 라는 툴은 없습니다", call.Name)))
			continue
		}
		// 노출 단계에서 걸렀더라도 실행 시점에 다시 검사한다.
		// 모델이 (또는 조작된 요청이) 목록에 없는 툴 이름을 보낼 수 있다.
		if tool.SuperadminOnly && !r.tc.user.Role.CanManageUsers() {
			r.tc.audit("ai.tool.denied", "tool", call.Name, "denied",
				map[string]any{"reason": "superadmin 전용"})
			results = append(results,
				r.failCall(call, "이 툴은 슈퍼 어드민만 사용할 수 있습니다"))
			continue
		}

		if tool.Mutating {
			result, err := r.proposeAction(ctx, msgID, tool, call)
			if err != nil {
				results = append(results, r.failCall(call, err.Error()))
				continue
			}
			paused = true
			results = append(results, ai.ToolResult{CallID: call.ID, Content: result})
			continue
		}

		content, err := r.runTool(ctx, tool, call)
		if err != nil {
			results = append(results, r.failCall(call, err.Error()))
			continue
		}
		results = append(results, ai.ToolResult{CallID: call.ID, Content: content})
		// 결과 전체를 화면에 보내지 않는 이유: 수십 KB의 JSON은 화면에서 읽히지 않고
		// 모델이 그것을 요약해 답할 것이다. 화면에는 크기만 알린다.
		_ = r.out.send("tool_result", map[string]any{
			"id": call.ID, "name": call.Name, "size": len(content),
			"preview": truncateForUI(content, 400),
		})
	}
	return results, paused
}

// executeERDCalls는 ERD 초안 편집 툴을 실행한다.
//
// 승인 단계가 없으므로 paused도 없다. 대신 실행 결과가 즉시 문서에 반영되고
// 열어 둔 사람 모두의 화면에 나타난다(Hub.SubmitOp).
func (r *agentRun) executeERDCalls(calls []ai.ToolCall) []ai.ToolResult {
	results := []ai.ToolResult{}
	for _, call := range calls {
		tool := r.erdTools[call.Name]
		_ = r.out.send("tool_call", map[string]any{
			"id": call.ID, "name": call.Name,
			"arguments": json.RawMessage(call.Input),
			// read_schema만 읽기다. 나머지는 문서를 바꾼다 —
			// 화면이 그 구분을 표시할 수 있어야 한다.
			"mutating": tool != nil && call.Name != "read_schema",
		})
		if tool == nil {
			results = append(results,
				r.failCall(call, fmt.Sprintf("%q 라는 툴은 없습니다", call.Name)))
			continue
		}
		if !r.tc.srv.erdCanEdit(r.erd, r.tc.user) {
			results = append(results, r.failCall(call, "이 초안을 편집할 권한이 없습니다"))
			continue
		}

		start := time.Now()
		content, err := tool.Run(r.erd, call.Input)
		r.tc.audit("erd.ai.tool", "erd_document", r.erd.docID, errResult(err), map[string]any{
			"tool": call.Name, "arguments": string(call.Input),
			"durationMs": time.Since(start).Milliseconds(), "error": errString(err),
		})
		if err != nil {
			results = append(results, r.failCall(call, err.Error()))
			continue
		}
		results = append(results, ai.ToolResult{CallID: call.ID, Content: content})
		_ = r.out.send("tool_result", map[string]any{
			"id": call.ID, "name": call.Name, "size": len(content),
			"preview": truncateForUI(content, 400),
		})
	}
	return results
}

// failCall은 툴 호출 실패를 화면과 모델에 동시에 알린다.
//
// 두 곳 모두에 알려야 한다: 모델은 결과를 받아 다른 방법을 찾고,
// 사용자는 왜 원하는 답이 나오지 않았는지 알게 된다.
func (r *agentRun) failCall(call ai.ToolCall, reason string) ai.ToolResult {
	_ = r.out.send("tool_result", map[string]any{
		"id": call.ID, "name": call.Name, "error": reason,
	})
	return ai.ToolResult{CallID: call.ID, IsError: true, Content: reason}
}

// runTool은 읽기 툴을 실행한다.
func (r *agentRun) runTool(ctx context.Context, tool *aiTool, call ai.ToolCall) (string, error) {
	if tool.Run == nil {
		return "", fmt.Errorf("%s 툴은 실행할 수 없습니다", tool.Name)
	}
	// 툴 하나가 오래 걸려 전체 대화를 붙잡지 않도록 상한을 둔다.
	toolCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	tc := *r.tc
	tc.ctx = toolCtx
	start := time.Now()
	content, err := tool.Run(&tc, call.Input)
	r.tc.audit("ai.tool.run", "tool", tool.Name, errResult(err), map[string]any{
		"arguments": string(call.Input), "durationMs": time.Since(start).Milliseconds(),
		"error": errString(err),
	})
	if err != nil {
		return "", err
	}
	return content, nil
}

// proposeAction은 쓰기 툴의 제안을 만들어 저장하고 화면에 알린다.
func (r *agentRun) proposeAction(ctx context.Context, msgID int64, tool *aiTool, call ai.ToolCall) (string, error) {
	if tool.Propose == nil {
		return "", fmt.Errorf("%s 툴은 제안을 만들 수 없습니다", tool.Name)
	}
	toolCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	tc := *r.tc
	tc.ctx = toolCtx

	summary, preview, err := tool.Propose(&tc, call.Input)
	if err != nil {
		r.tc.audit("ai.tool.propose", "tool", tool.Name, "error", map[string]any{
			"arguments": string(call.Input), "error": err.Error(),
		})
		return "", err
	}

	previewJSON, merr := json.Marshal(preview)
	if merr != nil {
		previewJSON = []byte("{}")
	}
	action := &store.AIPendingAction{
		SessionID: r.session.ID, MessageID: &msgID,
		ToolCallID: call.ID, ToolName: tool.Name,
		Arguments: json.RawMessage(call.Input),
		Summary:   summary, Preview: previewJSON,
	}
	if err := r.srv.st.CreateAIPendingAction(r.tc.ctx, action); err != nil {
		return "", err
	}
	r.tc.audit("ai.tool.propose", "tool", tool.Name, "", map[string]any{
		"arguments": string(call.Input), "summary": summary, "actionId": action.ID,
	})
	_ = r.out.send("pending_action", action)

	// 모델에게는 "승인 대기"임을 알려, 실행됐다고 착각하지 않게 한다.
	return fmt.Sprintf("사용자 승인 대기 중입니다 (제안 ID %s): %s. "+
		"승인 여부가 결정되면 결과가 전달됩니다. 지금은 실행되지 않았습니다.",
		action.ID, summary), nil
}

func (r *agentRun) saveAssistant(text string, calls []ai.ToolCall, usage ai.Usage, errMsg string) int64 {
	if strings.TrimSpace(text) == "" && len(calls) == 0 && errMsg == "" {
		return 0
	}
	msg := &store.AIMessage{
		SessionID: r.session.ID, Role: string(ai.RoleAssistant), Text: text,
		ToolCalls: calls, InputTokens: usage.InputTokens,
		OutputTokens: usage.OutputTokens, Error: errMsg,
	}
	if err := r.srv.st.AddAIMessage(r.tc.ctx, msg); err != nil {
		slog.Error("AI 메시지 저장 실패", "session", r.session.ID, "error", err)
		return 0
	}
	return msg.ID
}

func (r *agentRun) fail(err error) {
	_ = r.out.send("error", map[string]string{"message": err.Error()})
	_ = r.out.send("done", map[string]string{"reason": "error"})
}

func (r *agentRun) done(reason string) {
	_ = r.out.send("done", map[string]string{"reason": reason})
}

func errResult(err error) string {
	if err != nil {
		return "error"
	}
	return ""
}

func errString(err error) string {
	if err != nil {
		return err.Error()
	}
	return ""
}

func truncateForUI(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// ---------- 대화 이력 → 프로바이더 메시지 ----------

// buildHistory는 저장된 메시지를 프로바이더에 보낼 형태로 바꾼다.
//
// 오류로 끝난 assistant 메시지를 건너뛰는 이유: 실패한 턴을 다시 보내면 모델이
// 그 실패를 사실로 받아들여 같은 실수를 반복한다.
func buildHistory(messages []*store.AIMessage, budget int) []ai.Message {
	out := make([]ai.Message, 0, len(messages))
	for _, m := range messages {
		switch m.Role {
		case string(ai.RoleUser):
			out = append(out, ai.Message{Role: ai.RoleUser, Text: m.Text})
		case string(ai.RoleAssistant):
			if m.Error != "" && strings.TrimSpace(m.Text) == "" && len(m.ToolCalls) == 0 {
				continue
			}
			out = append(out, ai.Message{
				Role: ai.RoleAssistant, Text: m.Text, ToolCalls: m.ToolCalls,
			})
		case string(ai.RoleTool):
			if len(m.ToolResults) == 0 {
				continue
			}
			out = append(out, ai.Message{Role: ai.RoleTool, ToolResults: m.ToolResults})
		}
	}
	return trimHistory(pairToolCalls(out), budget)
}

// pairToolCalls는 짝이 맞지 않는 툴 호출과 결과를 걷어낸다.
//
// 프로바이더는 이 짝을 엄격하게 본다: 결과 없는 호출도, 호출 없는 결과도 요청 전체를
// 400으로 거부한다. 그리고 그 오류는 **대화를 지우기 전까지 회복되지 않는다** —
// 이력은 매 질문마다 그대로 다시 실려 나가기 때문이다.
//
// 짝이 깨지는 경로가 실제로 있다. 툴 호출이 담긴 assistant 메시지를 저장하지 못하면
// (인자가 깨져 직렬화가 실패하는 경우가 그랬다) 그 뒤에 저장되는 툴 결과만 남는다.
// 원인은 고쳤지만 이미 그렇게 저장된 대화가 남아 있으므로, 읽는 쪽에서도 정리한다.
func pairToolCalls(msgs []ai.Message) []ai.Message {
	calls := map[string]bool{}
	results := map[string]bool{}
	for _, m := range msgs {
		for _, c := range m.ToolCalls {
			calls[c.ID] = true
		}
		for _, r := range m.ToolResults {
			results[r.CallID] = true
		}
	}

	out := make([]ai.Message, 0, len(msgs))
	for _, m := range msgs {
		if len(m.ToolCalls) > 0 {
			kept := make([]ai.ToolCall, 0, len(m.ToolCalls))
			for _, c := range m.ToolCalls {
				if results[c.ID] {
					kept = append(kept, c)
				}
			}
			m.ToolCalls = kept
		}
		if len(m.ToolResults) > 0 {
			kept := make([]ai.ToolResult, 0, len(m.ToolResults))
			for _, r := range m.ToolResults {
				if calls[r.CallID] {
					kept = append(kept, r)
				}
			}
			m.ToolResults = kept
		}
		// 걷어내고 나서 아무것도 남지 않은 메시지는 보내지 않는다.
		// 빈 assistant 메시지나 빈 tool 메시지도 거부 사유가 된다.
		if strings.TrimSpace(m.Text) == "" && len(m.ToolCalls) == 0 && len(m.ToolResults) == 0 {
			continue
		}
		out = append(out, m)
	}
	return out
}

// defaultHistoryChars는 컨텍스트 크기를 모를 때 쓰는 상한이다.
//
// 예전에는 이 값 하나가 모든 프로바이더에 쓰였다. 그때의 근거는 "넘치면
// 프로바이더가 오류로 알려준다"였는데, **Ollama에서는 그 가정이 틀렸다.**
// Ollama는 컨텍스트를 넘는 프롬프트를 오류 없이 앞에서 잘라낸다 — 시스템
// 프롬프트가 먼저 사라지고, 모델은 자기가 무엇을 하던 중인지 모르는 채로 답한다.
// 그래서 크기를 아는 프로바이더에서는 그 값으로 예산을 잡는다(historyBudget).
const defaultHistoryChars = 120_000

// charsPerToken은 문자 수를 토큰 수로 어림하는 비율이다.
//
// 보수적으로 잡는 이유: 넘치면 조용히 잘라내는 프로바이더가 있으니, 적게 잡아
// 남기는 쪽의 손해가 훨씬 작다. 한국어는 한 글자가 대략 1~2토큰이고 영문·JSON은
// 한 토큰이 3~4자다. 툴 결과는 영문 키와 한국어 값이 섞이므로 그 사이를 잡는다.
const charsPerToken = 2

// historyShare는 컨텍스트에서 대화 이력에 내주는 몫이다.
//
// 나머지는 시스템 프롬프트, 툴 정의(수십 개라 작지 않다), 그리고 모델이 쓸 답이
// 가져간다. 생각이 긴 모델은 답 몫을 더 쓴다.
const historyShare = 0.6

// historyBudget은 이 컨텍스트 크기에서 이력에 허용할 문자 수다.
//
// 0(모름)이면 예전 기본값을 그대로 쓴다. 이미 돌고 있는 설치가 이 계산이 생겼다는
// 이유로 갑자기 이력을 더 버리면 안 된다.
func historyBudget(contextTokens int) int {
	if contextTokens <= 0 {
		return defaultHistoryChars
	}
	n := int(float64(contextTokens) * charsPerToken * historyShare)
	// 너무 작으면 직전 질문 하나도 못 넣는다. 그럴 바에는 넣고 프로바이더의
	// 판단에 맡긴다 — 적어도 그쪽은 오류로 알려주기라도 한다.
	if n < 4_000 {
		return 4_000
	}
	return n
}

// trimHistory는 오래된 메시지를 잘라낸다.
//
// 앞에서 자르는 이유: 최근 대화가 현재 질문과 관련이 깊고, 툴 결과는 특히 최근 것이
// 유효하다(지표·로그는 시간이 지나면 의미가 없다).
func trimHistory(messages []ai.Message, budget int) []ai.Message {
	if budget <= 0 {
		budget = defaultHistoryChars
	}
	total := 0
	for _, m := range messages {
		total += messageSize(m)
	}
	if total <= budget {
		return messages
	}
	// 뒤에서부터 담다가 상한에 닿으면 멈춘다.
	kept := []ai.Message{}
	size := 0
	for i := len(messages) - 1; i >= 0; i-- {
		s := messageSize(messages[i])
		if size+s > budget && len(kept) > 0 {
			break
		}
		kept = append([]ai.Message{messages[i]}, kept...)
		size += s
	}
	// 첫 메시지가 툴 결과면 그것에 대응하는 assistant 툴 호출이 없어 프로바이더가
	// 거부한다. 짝이 맞는 지점까지 더 잘라낸다.
	for len(kept) > 0 && kept[0].Role == ai.RoleTool {
		kept = kept[1:]
	}
	return kept
}

func messageSize(m ai.Message) int {
	n := len(m.Text)
	for _, c := range m.ToolCalls {
		n += len(c.Name) + len(c.Input)
	}
	for _, r := range m.ToolResults {
		n += len(r.Content)
	}
	return n
}

// providerConfig는 세션이 쓸 프로바이더 설정을 만든다.
func (s *Server) providerConfig(ctx context.Context, sess *store.AISession) (ai.Config, *store.AIProvider, error) {
	var p *store.AIProvider
	var err error
	if sess.ProviderID != "" {
		p, err = s.st.GetAIProvider(ctx, sess.ProviderID, true)
		if err != nil {
			return ai.Config{}, nil, err
		}
	} else {
		p, err = s.st.DefaultAIProvider(ctx, true)
		if err != nil {
			return ai.Config{}, nil, err
		}
	}
	if p == nil {
		return ai.Config{}, nil, fmt.Errorf("사용할 수 있는 AI 프로바이더가 없습니다. 먼저 API 키를 등록하세요")
	}
	if !p.Enabled {
		return ai.Config{}, nil, fmt.Errorf("프로바이더 %q 가 비활성 상태입니다", p.Name)
	}
	model := sess.Model
	if model == "" {
		model = p.DefaultModel
	}
	if strings.TrimSpace(model) == "" {
		return ai.Config{}, nil, fmt.Errorf("사용할 모델이 지정되지 않았습니다 (프로바이더 %q 의 기본 모델을 설정하세요)", p.Name)
	}
	// 허용 목록은 실제 호출 직전에도 확인한다. 세션을 만든 뒤에 관리자가 목록에서
	// 모델을 뺄 수 있고, 그때 조용히 다른 모델로 바꿔 부르면 비용도 답도 달라진
	// 이유를 아무도 모른다. 막고 알려주는 편이 낫다 — 화면에서 모델만 바꾸면 된다.
	if !modelAllowed(p, model) {
		return ai.Config{}, nil, fmt.Errorf(
			"이 대화의 모델 %q 는 %s 에서 더 이상 허용되지 않습니다. 대화 설정에서 모델을 다시 고르세요 (허용: %s)",
			model, p.Name, strings.Join(p.Models, ", "))
	}
	return ai.Config{
		Kind: ai.Kind(p.Provider), BaseURL: p.BaseURL,
		APIKey: p.APIKey, Model: model, ContextTokens: p.ContextTokens,
	}, p, nil
}

// newToolContext는 툴 실행 문맥을 만든다.
func (s *Server) newToolContext(ctx context.Context, u *model.User, ip string, sess *store.AISession) *toolContext {
	return &toolContext{ctx: ctx, srv: s, user: u, ip: ip, session: sess}
}
