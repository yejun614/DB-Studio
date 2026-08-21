package api

import (
	"encoding/json"
	"testing"

	"dbstudio/internal/ai"
	"dbstudio/internal/store"
)

// 짝이 맞지 않는 툴 호출·결과는 이력에서 빠져야 한다.
//
// 프로바이더는 이 짝을 엄격하게 본다: 호출 없는 결과도, 결과 없는 호출도 요청 전체를
// 400으로 거부한다("Request contains an invalid argument"). 그리고 이력은 매 질문마다
// 그대로 다시 실려 나가므로, 한 번 깨진 대화는 지우기 전까지 회복되지 않는다.
//
// 실제로 그런 대화가 만들어지는 경로가 있었다: 툴 호출 인자가 깨져 assistant 메시지
// 저장이 실패하면 그 뒤에 저장되는 툴 결과만 남는다.
func TestBuildHistoryDropsOrphanToolResults(t *testing.T) {
	history := buildHistory([]*store.AIMessage{
		{Role: string(ai.RoleUser), Text: "질문"},
		// assistant 메시지가 저장되지 못해 결과만 남은 상태.
		{Role: string(ai.RoleTool), ToolResults: []ai.ToolResult{
			{CallID: "고아", Content: "결과"},
		}},
		{Role: string(ai.RoleUser), Text: "다시 질문"},
	})

	for _, m := range history {
		if len(m.ToolResults) > 0 {
			t.Errorf("호출 없는 툴 결과가 남았다: %+v", m)
		}
	}
	if len(history) != 2 {
		t.Errorf("메시지 수 = %d, want 2 (사용자 질문 둘): %+v", len(history), history)
	}
}

func TestBuildHistoryDropsUnansweredToolCalls(t *testing.T) {
	history := buildHistory([]*store.AIMessage{
		{Role: string(ai.RoleUser), Text: "질문"},
		{Role: string(ai.RoleAssistant), Text: "확인합니다", ToolCalls: []ai.ToolCall{
			{ID: "c1", Name: "list_connections", Input: json.RawMessage(`{}`)},
			{ID: "c2", Name: "get_metrics", Input: json.RawMessage(`{}`)},
		}},
		// c2의 결과가 저장되지 못했다.
		{Role: string(ai.RoleTool), ToolResults: []ai.ToolResult{{CallID: "c1", Content: "[]"}}},
	})

	var calls []ai.ToolCall
	for _, m := range history {
		calls = append(calls, m.ToolCalls...)
	}
	if len(calls) != 1 || calls[0].ID != "c1" {
		t.Errorf("남은 호출 = %+v, 결과가 있는 c1만 남아야 한다", calls)
	}
	// 텍스트는 살아 있어야 한다 — 호출 하나가 빠졌다고 답변까지 버릴 이유가 없다.
	if history[1].Text != "확인합니다" {
		t.Errorf("assistant 텍스트가 사라졌다: %+v", history[1])
	}
}

// 정상적인 대화는 그대로 통과해야 한다.
func TestBuildHistoryKeepsPairedCalls(t *testing.T) {
	history := buildHistory([]*store.AIMessage{
		{Role: string(ai.RoleUser), Text: "질문"},
		{Role: string(ai.RoleAssistant), ToolCalls: []ai.ToolCall{
			{ID: "c1", Name: "a", Input: json.RawMessage(`{}`), Signature: "SIG"},
		}},
		{Role: string(ai.RoleTool), ToolResults: []ai.ToolResult{{CallID: "c1", Content: "ok"}}},
		{Role: string(ai.RoleAssistant), Text: "끝났습니다"},
	})
	if len(history) != 4 {
		t.Fatalf("메시지 수 = %d, want 4: %+v", len(history), history)
	}
	if len(history[1].ToolCalls) != 1 || history[1].ToolCalls[0].Signature != "SIG" {
		t.Errorf("호출이 온전하지 않다: %+v", history[1].ToolCalls)
	}
	if len(history[2].ToolResults) != 1 {
		t.Errorf("결과가 사라졌다: %+v", history[2])
	}
}
