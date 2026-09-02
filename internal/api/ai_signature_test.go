package api

import (
	"context"
	"encoding/json"
	"testing"

	"dbstudio/internal/ai"
	"dbstudio/internal/store"
)

// 사고 서명은 대화 이력에 살아남아야 한다.
//
// 어댑터가 서명을 읽고 되돌려 보내도(internal/ai 시험) 그것만으로는 부족하다.
// 대화는 저장되었다가 다음 질문에서 이력으로 다시 조립되므로, 그 왕복에서 서명이
// 떨어지면 증상은 "새로고침한 뒤 대화를 이어가면 400이 난다"로 나타난다 —
// 같은 대화에서는 되고 나중에는 안 되는, 원인 찾기 가장 나쁜 형태다.
func TestToolCallSignatureSurvivesHistory(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()

	sess, err := e.st.CreateAISession(ctx, store.CreateAISessionParams{
		UserID: e.user.ID, Title: "대화",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if err := e.st.AddAIMessage(ctx, &store.AIMessage{
		SessionID: sess.ID, Role: string(ai.RoleAssistant), Text: "조회합니다",
		ToolCalls: []ai.ToolCall{{
			ID: "c1", Name: "list_connections",
			Input: json.RawMessage(`{}`), Signature: "SIG-1",
		}},
	}); err != nil {
		t.Fatalf("add message: %v", err)
	}
	if err := e.st.AddAIMessage(ctx, &store.AIMessage{
		SessionID: sess.ID, Role: string(ai.RoleTool),
		ToolResults: []ai.ToolResult{{CallID: "c1", Content: "[]"}},
	}); err != nil {
		t.Fatalf("add tool result: %v", err)
	}

	stored, err := e.st.ListAIMessages(ctx, sess.ID, 0)
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	history := buildHistory(stored, 0)

	var found *ai.ToolCall
	for i := range history {
		if len(history[i].ToolCalls) > 0 {
			found = &history[i].ToolCalls[0]
		}
	}
	if found == nil {
		t.Fatalf("이력에 툴 호출이 없다: %+v", history)
	}
	if found.Signature != "SIG-1" {
		t.Errorf("이력의 서명 = %q, 기대값 SIG-1", found.Signature)
	}
}
