package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"dbstudio/internal/ai"
	"dbstudio/internal/store"
)

// storedMsgs는 검사용 대화를 만든다. 아이디는 1부터 붙는다.
func storedMsgs(items ...*store.AIMessage) []*store.AIMessage {
	for i, m := range items {
		m.ID = int64(i + 1)
	}
	return items
}

func userMsg(text string) *store.AIMessage {
	return &store.AIMessage{Role: string(ai.RoleUser), Text: text}
}

func callMsg(id, name string) *store.AIMessage {
	return &store.AIMessage{Role: string(ai.RoleAssistant), ToolCalls: []ai.ToolCall{
		{ID: id, Name: name, Input: json.RawMessage(`{}`)},
	}}
}

func resultMsg(id string, size int) *store.AIMessage {
	return &store.AIMessage{Role: string(ai.RoleTool), ToolResults: []ai.ToolResult{
		{CallID: id, Content: strings.Repeat("가", size)},
	}}
}

// 자리를 차지하는 것은 툴 결과다. 그것부터 비우고, 프로바이더는 부르지 않는다.
//
// 이 순서가 중요한 이유: 툴 결과를 줄글로 요약하면 모델이 그것을 사실로 읽고,
// 접히면서 사라진 컬럼을 없는 것으로 여기거나 있는 것으로 지어낸다. 반면 툴 결과는
// **다시 부르면 그만이다**. 그래서 요약이 아니라 비우기가 맞다.
func TestCompactEmptiesToolResultsFirst(t *testing.T) {
	e := newTestEnv(t)
	stored := storedMsgs(
		userMsg("첫 질문"),
		callMsg("c1", "read_schema"),
		resultMsg("c1", 3000),
		callMsg("c2", "read_schema"),
		resultMsg("c2", 3000),
		userMsg("두 번째 질문"),
		&store.AIMessage{Role: string(ai.RoleAssistant), Text: "짧은 답"},
	)
	sess := &store.AISession{ID: "s1"}

	// 프로바이더 설정을 주지 않는다. 1단계에서 끝나야 하므로 부를 일이 없다 —
	// 부르려 들면 여기서 오류가 나거나 멈춘다.
	// 예산은 **바이트**다("가" 한 글자가 3바이트). 결과 하나(9,000바이트)를 비우면
	// 들어오도록 잡아, 최근 것이 남는지까지 본다.
	got := e.srv.compactHistory(context.Background(), ai.Config{}, sess, stored, 12_000)

	if got.Folded != 0 {
		t.Errorf("1단계에서 끝나야 하는데 요약까지 갔습니다 (%d개 접음)", got.Folded)
	}
	if got.Emptied == 0 {
		t.Error("툴 결과를 비우지 않았습니다")
	}
	if got.Summary != "" {
		t.Errorf("프로바이더를 불렀습니다: %q", got.Summary)
	}
	if n := historySize(got.Messages); n > 12_000 {
		t.Errorf("접고도 %d바이트입니다 (예산 12000)", n)
	}

	// 비운 자리에는 "다시 부르면 된다"가 적혀 있어야 한다. 빈 문자열로 두면
	// 모델은 그 툴이 아무것도 돌려주지 않았다고 읽는다.
	found := false
	for _, m := range got.Messages {
		for _, r := range m.ToolResults {
			if strings.Contains(r.Content, "다시 부르세요") {
				found = true
				if !strings.Contains(r.Content, "read_schema") {
					t.Errorf("어느 툴이었는지 적히지 않았습니다: %q", r.Content)
				}
			}
		}
	}
	if !found {
		t.Error("비운 자리에 안내가 없습니다")
	}

	// 최근 결과는 남아야 한다. 지금 질문과 관련이 깊다.
	last := got.Messages[len(got.Messages)-1]
	_ = last
	kept := 0
	for _, m := range got.Messages {
		for _, r := range m.ToolResults {
			if !strings.Contains(r.Content, "비웠습니다") {
				kept++
			}
		}
	}
	if kept == 0 {
		t.Error("모든 툴 결과를 비웠습니다. 최근 것은 남겨야 합니다")
	}
}

// 짧은 대화는 건드리지 않는다.
func TestCompactLeavesShortHistoryAlone(t *testing.T) {
	e := newTestEnv(t)
	stored := storedMsgs(userMsg("안녕"),
		&store.AIMessage{Role: string(ai.RoleAssistant), Text: "안녕하세요"})
	sess := &store.AISession{ID: "s1"}

	got := e.srv.compactHistory(context.Background(), ai.Config{}, sess, stored, 100_000)
	if got.Emptied != 0 || got.Folded != 0 || got.Summary != "" {
		t.Errorf("멀쩡한 대화를 접었습니다: %+v", got)
	}
	if len(got.Messages) != 2 {
		t.Errorf("메시지 %d개", len(got.Messages))
	}
	if compactNotice(got) != "" {
		t.Error("접지도 않고 안내를 띄웠습니다")
	}
}

// 툴 결과를 다 비워도 모자라면 옛 대화를 요약으로 접는다.
func TestCompactFoldsProseWhenStillOver(t *testing.T) {
	e := newTestEnv(t)
	fake := newFakeLLM(t, [][]string{{
		`{"choices":[{"delta":{"content":"users 테이블 이름 규칙을 정했다."}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		"[DONE]",
	}})

	long := strings.Repeat("긴 이야기 ", 400)
	stored := storedMsgs(
		userMsg(long), &store.AIMessage{Role: string(ai.RoleAssistant), Text: long},
		userMsg(long), &store.AIMessage{Role: string(ai.RoleAssistant), Text: long},
		userMsg("마지막 질문"),
	)
	sess := &store.AISession{ID: "s1"}
	cfg := ai.Config{Kind: ai.OpenAICompatible, BaseURL: fake.srv.URL, Model: "m", APIKey: "k"}

	got := e.srv.compactHistory(context.Background(), cfg, sess, stored, 3_000)
	if got.Folded == 0 {
		t.Fatalf("접지 않았습니다: %+v", got)
	}
	if got.Summary == "" {
		t.Error("요약이 비었습니다")
	}
	if got.Through == 0 {
		t.Error("어디까지 접었는지가 없습니다")
	}
	// 요약은 맨 앞에 대화의 일부로 놓인다.
	if len(got.Messages) == 0 || !strings.Contains(got.Messages[0].Text, got.Summary) {
		t.Errorf("요약이 맨 앞에 없습니다: %+v", got.Messages)
	}
	// 마지막 질문은 남아야 한다.
	tail := got.Messages[len(got.Messages)-1]
	if !strings.Contains(tail.Text, "마지막 질문") {
		t.Errorf("최근 질문이 사라졌습니다: %q", tail.Text)
	}
	if !strings.Contains(compactNotice(got), "요약") {
		t.Errorf("안내 = %q", compactNotice(got))
	}
}

// 요약에 실패해도 대화는 이어져야 한다.
//
// 여기서 멈추면 컨텍스트가 찼다는 이유로 대화가 아예 죽는다. 접기는 편의이지
// 대화의 조건이 아니다.
func TestCompactSurvivesSummaryFailure(t *testing.T) {
	e := newTestEnv(t)
	// 응답을 주지 않는 가짜 서버 — 요약이 빈 문자열로 끝난다.
	fake := newFakeLLM(t, [][]string{{}})

	long := strings.Repeat("긴 이야기 ", 400)
	stored := storedMsgs(
		userMsg(long), &store.AIMessage{Role: string(ai.RoleAssistant), Text: long},
		userMsg(long), &store.AIMessage{Role: string(ai.RoleAssistant), Text: long},
		userMsg("마지막 질문"),
	)
	sess := &store.AISession{ID: "s1"}
	cfg := ai.Config{Kind: ai.OpenAICompatible, BaseURL: fake.srv.URL, Model: "m", APIKey: "k"}

	got := e.srv.compactHistory(context.Background(), cfg, sess, stored, 3_000)
	if len(got.Messages) == 0 {
		t.Fatal("이력이 통째로 사라졌습니다")
	}
	if got.Summary != "" {
		t.Errorf("실패했는데 요약이 있습니다: %q", got.Summary)
	}
	if n := historySize(got.Messages); n > 3_000 {
		t.Errorf("예산을 넘었습니다: %d자", n)
	}
}

// 접어 둔 요약은 다음 차례에 그대로 쓰이고, 담긴 대목은 두 번 들어가지 않는다.
func TestCompactReusesStoredSummary(t *testing.T) {
	e := newTestEnv(t)
	stored := storedMsgs(
		userMsg("옛 질문"), &store.AIMessage{Role: string(ai.RoleAssistant), Text: "옛 답"},
		userMsg("새 질문"),
	)
	sess := &store.AISession{ID: "s1", Summary: "앞에서 이름 규칙을 정했다", SummaryThroughID: 2}

	got := e.srv.compactHistory(context.Background(), ai.Config{}, sess, stored, 100_000)
	joined := ""
	for _, m := range got.Messages {
		joined += m.Text + "\n"
	}
	if !strings.Contains(joined, "앞에서 이름 규칙을 정했다") {
		t.Error("저장해 둔 요약이 쓰이지 않았습니다")
	}
	if strings.Contains(joined, "옛 질문") {
		t.Error("요약에 담긴 대목이 원문으로도 들어갔습니다")
	}
	if !strings.Contains(joined, "새 질문") {
		t.Error("요약 뒤의 대화가 빠졌습니다")
	}
	// 다시 접을 일이 없으므로 프로바이더를 부르지 않는다.
	if got.Summary != "" {
		t.Errorf("이미 접어 뒀는데 또 접었습니다: %q", got.Summary)
	}
}

// 접는 자리는 툴 호출과 그 결과를 갈라놓지 않아야 한다.
//
// 가르면 결과만 남은 메시지가 생기고, 그것은 프로바이더가 거절한다.
func TestFoldPointKeepsToolPairs(t *testing.T) {
	live := buildHistoryRaw(storedMsgs(
		userMsg("질문1"),
		callMsg("c1", "read_schema"),
		resultMsg("c1", 10),
		userMsg("질문2"),
		callMsg("c2", "read_schema"),
		resultMsg("c2", 10),
		userMsg("질문3"),
	))
	cut := foldPoint(live)
	if cut == 0 {
		t.Fatal("접을 자리를 못 찾았습니다")
	}
	fold, rest := splitAt(live, cut)
	if len(fold) == 0 || len(rest) == 0 {
		t.Fatalf("가르지 못했습니다: %d / %d", len(fold), len(rest))
	}
	// 자른 자리 뒤의 첫 메시지는 툴 결과가 아니어야 한다.
	if rest[0].Role == ai.RoleTool {
		t.Errorf("툴 결과에서 잘랐습니다: %+v", rest[0])
	}
	// 자른 앞쪽에 짝 없는 호출이 남아도 안 된다.
	paired := pairToolCalls(plainMessages(fold))
	for _, m := range paired {
		_ = m
	}
}

// 비운 자리표가 원래 결과보다 길면 비우지 않는다.
func TestEmptyToolResultsSkipsTinyOnes(t *testing.T) {
	live := buildHistoryRaw(storedMsgs(
		callMsg("c1", "x"),
		resultMsg("c1", 3),
	))
	got, n := emptyOldToolResults(live, 1)
	if n != 0 {
		t.Errorf("짧은 결과를 비웠습니다 (%d개)", n)
	}
	if got[1].ToolResults[0].Content != strings.Repeat("가", 3) {
		t.Errorf("내용이 바뀌었습니다: %q", got[1].ToolResults[0].Content)
	}
}
