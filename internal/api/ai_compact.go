package api

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"dbstudio/internal/ai"
	"dbstudio/internal/store"
)

// 컨텍스트가 찼을 때 옛 대화를 접는다.
//
// 예전에는 예산을 넘으면 오래된 메시지를 **그냥 버렸다**(trimHistory). 최근 것이 더
// 유효하다는 판단은 맞지만, 버린 사실이 아무 데도 적히지 않았다. 사람은 "아까 말한
// 그거"라고 하는데 모델에게는 그 말이 없다.
//
// # 무엇을 접는가 — 두 단계로 나눈 이유
//
// 이 앱의 대화에서 자리를 차지하는 것은 사람의 말이 아니라 **툴 결과**다. 실제로
// 겪은 대화에서 사람의 말은 수백 자였는데 툴 결과 하나가 22,319자였다. 사람이 한
// 이야기를 아무리 잘 요약해도 자리는 거의 안 줄어든다는 뜻이다.
//
// 그런데 툴 결과는 **요약하면 안 된다.** 스키마 덤프를 줄글로 접어 두면 모델은 그것을
// 사실로 읽고, 접히면서 사라진 컬럼을 없는 것으로 여기거나 있는 것으로 지어낸다.
// 반대로 툴 결과는 **다시 부르면 그만이다** — 같은 툴을 같은 인자로 부르면 최신 값이
// 온다. 그래서 순서가 이렇게 된다.
//
//	1단계: 오래된 툴 결과를 비운다. 무엇을 불렀는지는 남기고 "다시 부르면 된다"고
//	       적어 둔다. 프로바이더를 부르지 않으므로 **공짜다**.
//	2단계: 그래도 넘치면 사람과 모델이 주고받은 말을 한 문단으로 접는다. 이때만
//	       프로바이더를 한 번 더 부른다. 이 요약은 세션에 저장해 두고 다시 쓴다.
//
// 대부분의 대화는 1단계에서 끝난다.

// histMsg는 저장된 아이디를 달고 다니는 메시지다.
//
// 아이디를 끝까지 들고 가는 이유: 어디까지 접었는지를 세션에 남겨야 하는데,
// 변환 과정에서 걸러지는 메시지가 있어(오류로 끝난 턴, 결과 없는 툴 메시지)
// 자리 번호로는 원본과 짝을 맞출 수 없다.
type histMsg struct {
	ai.Message
	ID int64
}

func plainMessages(list []histMsg) []ai.Message {
	out := make([]ai.Message, 0, len(list))
	for _, m := range list {
		out = append(out, m.Message)
	}
	return out
}

// compactResult는 접은 결과다.
type compactResult struct {
	Messages []ai.Message
	// Emptied는 비운 툴 결과의 수다.
	Emptied int
	// Folded는 요약으로 접은 메시지 수다.
	Folded int
	// Summary는 이번에 새로 만든 요약이다. 저장할 것이 없으면 비어 있다.
	Summary string
	// Through는 그 요약이 담고 있는 마지막 메시지 아이디다.
	Through int64
}

// toolResultPlaceholder는 비운 툴 결과 자리에 남기는 글이다.
//
// 빈 문자열로 두지 않는 이유: 모델은 그 툴이 아무것도 돌려주지 않았다고 읽는다.
// 무엇이 있었는지와 어떻게 되찾는지를 함께 적어야 다시 부를 결심을 한다.
func toolResultPlaceholder(name string, size int) string {
	if name == "" {
		name = "그 툴"
	}
	return fmt.Sprintf(
		"(%s 의 결과 %d자는 문맥이 차서 비웠습니다. 그 내용이 필요하면 %s 을(를) 다시 "+
			"부르세요. 기억에 의존해 답하지 마세요.)", name, size, name)
}

// emptyOldToolResults는 예산에 들어올 때까지 오래된 툴 결과를 비운다.
//
// 앞에서부터 비우는 이유: 최근 결과일수록 지금 질문과 관련이 깊고, 지표·로그는
// 시간이 지나면 의미가 없다.
func emptyOldToolResults(msgs []histMsg, budget int) ([]histMsg, int) {
	total := 0
	for _, m := range msgs {
		total += messageSize(m.Message)
	}
	if total <= budget {
		return msgs, 0
	}

	// 툴 이름은 앞선 assistant 메시지의 호출에서 찾는다.
	names := map[string]string{}
	for _, m := range msgs {
		for _, c := range m.ToolCalls {
			names[c.ID] = c.Name
		}
	}

	out := make([]histMsg, len(msgs))
	copy(out, msgs)
	emptied := 0
	for i := range out {
		if total <= budget {
			break
		}
		if out[i].Role != ai.RoleTool || len(out[i].ToolResults) == 0 {
			continue
		}
		results := make([]ai.ToolResult, len(out[i].ToolResults))
		copy(results, out[i].ToolResults)
		changed := false
		for j := range results {
			body := results[j].Content
			note := toolResultPlaceholder(names[results[j].CallID], len(body))
			// 자리표가 원래 결과보다 길면 접는 뜻이 없다.
			if len(note) >= len(body) {
				continue
			}
			total -= len(body) - len(note)
			results[j].Content = note
			changed = true
			emptied++
		}
		if changed {
			out[i].ToolResults = results
		}
	}
	return out, emptied
}

// summaryPrompt는 옛 대화를 접을 때 쓰는 지시다.
//
// "무엇을 하기로 했는가"를 앞세우는 이유: 접힌 대화에서 다시 필요해지는 것은 대개
// 결론과 약속이지 과정이 아니다. 그리고 툴 결과를 옮겨 적지 말라고 못 박는다 —
// 그것을 요약에 넣으면 낡은 값이 사실처럼 굳는다.
const summaryPrompt = `아래는 한 대화의 앞부분이다. 뒤에 이어질 대화가 참고할 수 있도록 요약하라.

규칙:
- 한국어로, 열 줄을 넘기지 마라.
- **무엇을 하기로 했는지, 무엇이 정해졌는지**를 먼저 적어라.
- 사용자가 밝힌 제약·선호(이름 규칙, 쓰는 DB, 하지 말라고 한 것)를 빠뜨리지 마라.
- 툴이 돌려준 값(스키마, 지표, 로그 내용)은 **옮겨 적지 마라.** 그것은 다시 조회하면
  되고, 요약에 적어 두면 낡은 값이 사실처럼 굳는다. "무엇을 조회했다"까지만 적어라.
- 인사말과 과정 설명은 빼라.`

// summarize는 프로바이더를 한 번 불러 옛 대화를 접는다.
func (s *Server) summarize(ctx context.Context, cfg ai.Config, msgs []ai.Message) (string, error) {
	provider, err := ai.Get(cfg.Kind)
	if err != nil {
		return "", err
	}
	// 접을 대화를 글로 옮겨 한 번에 넘긴다. 원래 역할 그대로 넘기지 않는 이유:
	// 툴 호출과 결과의 짝이 맞아야 한다는 규칙에 걸리고, 여기서 필요한 것은
	// 대화의 재현이 아니라 읽을거리다.
	var b strings.Builder
	for _, m := range msgs {
		switch m.Role {
		case ai.RoleUser:
			b.WriteString("사용자: " + m.Text + "\n")
		case ai.RoleAssistant:
			if strings.TrimSpace(m.Text) != "" {
				b.WriteString("어시스턴트: " + m.Text + "\n")
			}
			for _, c := range m.ToolCalls {
				b.WriteString("  (툴 호출: " + c.Name + ")\n")
			}
		case ai.RoleTool:
			// 내용은 넣지 않는다. 위 규칙과 같은 이유이고, 어차피 이 단계에 오기
			// 전에 대부분 비워져 있다.
			b.WriteString("  (툴 결과 도착)\n")
		}
	}

	stream, err := provider.Stream(ctx, cfg, ai.Request{
		Model: cfg.Model, System: summaryPrompt,
		Messages: []ai.Message{{Role: ai.RoleUser, Text: b.String()}},
	})
	if err != nil {
		return "", err
	}
	var out strings.Builder
	for ev := range stream {
		switch ev.Type {
		case ai.EventText:
			out.WriteString(ev.Text)
		case ai.EventError:
			return "", ev.Err
		}
	}
	return strings.TrimSpace(out.String()), nil
}

// summaryMessage는 요약을 대화 맨 앞에 놓을 메시지로 만든다.
//
// 시스템 프롬프트에 붙이지 않고 대화의 첫 마디로 두는 이유: 시스템 프롬프트는
// "너는 무엇이다"를 말하는 자리다. 거기에 옛 대화를 섞으면 모델이 그것을 지시로
// 읽는다. 대화의 일부는 대화 자리에 두는 편이 맞다.
//
// 어시스턴트의 대답을 함께 두는 이유: 사용자 메시지만 둘이 연달아 오면 그것을
// 거부하는 프로바이더가 있다.
func summaryMessage(summary string) []ai.Message {
	if strings.TrimSpace(summary) == "" {
		return nil
	}
	return []ai.Message{
		{Role: ai.RoleUser, Text: "지금까지 나눈 이야기의 요약이다. 이어서 답하라.\n\n" + summary},
		{Role: ai.RoleAssistant, Text: "요약을 확인했습니다. 이어서 돕겠습니다."},
	}
}

// compactHistory는 예산에 맞을 때까지 이력을 접는다.
//
// 접기가 필요 없으면 아무것도 하지 않고 그대로 돌려준다. 프로바이더를 부르는 것은
// 1단계로 모자랄 때뿐이다.
func (s *Server) compactHistory(ctx context.Context, cfg ai.Config, sess *store.AISession,
	stored []*store.AIMessage, budget int) compactResult {

	live := buildHistoryRaw(stored)

	// 이미 접어 둔 요약이 있으면 그 뒤의 것만 남기고 앞에 요약을 놓는다.
	var head []ai.Message
	if sess.Summary != "" && sess.SummaryThroughID > 0 {
		kept := make([]histMsg, 0, len(live))
		for _, m := range live {
			if m.ID > sess.SummaryThroughID {
				kept = append(kept, m)
			}
		}
		head = summaryMessage(sess.Summary)
		live = kept
	}

	join := func(list []histMsg) []ai.Message {
		return append(append([]ai.Message{}, head...), pairToolCalls(plainMessages(list))...)
	}

	if historySize(join(live)) <= budget {
		return compactResult{Messages: join(live)}
	}

	// 1단계: 오래된 툴 결과를 비운다. 공짜다.
	live, emptied := emptyOldToolResults(live, budget-historySize(head))
	if historySize(join(live)) <= budget {
		return compactResult{Messages: join(live), Emptied: emptied}
	}

	// 2단계: 앞쪽을 한 문단으로 접는다.
	cut := foldPoint(live)
	fold, rest := splitAt(live, cut)
	if cut <= 0 || len(fold) == 0 {
		// 접을 옛 이야기가 없다(툴 결과 하나가 예산보다 크다). 예전처럼 자른다.
		return compactResult{Messages: trimHistory(join(live), budget), Emptied: emptied}
	}

	text, err := s.summarize(ctx, cfg,
		append(append([]ai.Message{}, head...), plainMessages(fold)...))
	if err != nil || text == "" {
		// 요약에 실패해도 대화는 이어져야 한다. 예전처럼 자르고 넘어간다 —
		// 여기서 멈추면 컨텍스트가 찼다는 이유로 대화가 아예 죽는다.
		if err != nil {
			slog.Warn("대화 요약 실패", "session", sess.ID, "error", err)
		}
		return compactResult{Messages: trimHistory(join(live), budget), Emptied: emptied}
	}

	// 요약은 자르지 않는다. trimHistory 는 앞에서부터 버리는데 요약은 맨 앞에 있어서
	// 가장 먼저 버려진다 — 접느라 프로바이더를 부르고 그 결과를 곧바로 버리는 셈이다.
	// 뒤쪽만 예산에 맞추고 요약은 그 위에 얹는다.
	sum := summaryMessage(text)
	tail := trimHistory(pairToolCalls(plainMessages(rest)), budget-historySize(sum))
	return compactResult{
		Messages: append(sum, tail...),
		Emptied:  emptied, Folded: len(fold),
		Summary: text, Through: cut,
	}
}

// foldPoint는 어디까지 접을지 정한다. 접을 것이 없으면 0.
//
// 절반쯤에서 자르는 이유: 딱 맞게만 접으면 다음 차례에 또 접게 되고, 접을 때마다
// 프로바이더를 부른다. 넉넉히 접어 두면 그 값을 자주 내지 않는다.
//
// 사용자 메시지 앞에서 끊는 이유: 그 자리가 이야기의 매듭이고, 툴 호출과 그 결과를
// 갈라놓지 않는 안전한 자리다.
func foldPoint(live []histMsg) int64 {
	if len(live) < 4 {
		return 0
	}
	half := len(live) / 2

	// 자를 수 있는 자리는 **바로 뒤가 툴 결과가 아닌** 곳뿐이다. 그 사이를 자르면
	// 남는 쪽이 결과만 있고 호출이 없는 메시지로 시작하는데, 프로바이더가 그것을
	// 거절한다.
	canCutAfter := func(i int) bool {
		return i+1 >= len(live) || live[i+1].Role != ai.RoleTool
	}

	// 사용자 메시지 앞이 가장 좋은 매듭이다. 그 자리가 이야기가 바뀌는 곳이다.
	cut := int64(0)
	for i := 1; i < half; i++ {
		if live[i].Role == ai.RoleUser && canCutAfter(i-1) {
			cut = live[i-1].ID
		}
	}
	if cut != 0 {
		return cut
	}
	// 앞쪽 절반에 매듭이 없다. 뒤로 물러나며 자를 수 있는 자리를 찾는다.
	for i := half - 1; i >= 0; i-- {
		if canCutAfter(i) {
			return live[i].ID
		}
	}
	return 0
}

// splitAt은 아이디를 기준으로 접을 것과 남길 것을 가른다.
func splitAt(live []histMsg, cut int64) (fold, rest []histMsg) {
	for _, m := range live {
		if m.ID <= cut {
			fold = append(fold, m)
		} else {
			rest = append(rest, m)
		}
	}
	return fold, rest
}

func historySize(msgs []ai.Message) int {
	n := 0
	for _, m := range msgs {
		n += messageSize(m)
	}
	return n
}

// compactNotice는 접은 일을 사람에게 알리는 문장이다. 접지 않았으면 빈 문자열.
//
// 알리는 이유: 접기는 조용히 일어나면 안 된다. "아까 말한 그거"가 통하지 않는
// 순간이 오는데, 그때 이유를 모르면 모델이 고장 난 것으로 보인다.
func compactNotice(r compactResult) string {
	switch {
	case r.Folded > 0 && r.Emptied > 0:
		return fmt.Sprintf(
			"문맥이 차서 옛 대화 %d개를 요약으로 접고, 오래된 툴 결과 %d개를 비웠습니다. "+
				"비운 결과가 필요하면 어시스턴트가 다시 조회합니다", r.Folded, r.Emptied)
	case r.Folded > 0:
		return fmt.Sprintf("문맥이 차서 옛 대화 %d개를 요약으로 접었습니다", r.Folded)
	case r.Emptied > 0:
		return fmt.Sprintf(
			"문맥이 차서 오래된 툴 결과 %d개를 비웠습니다. 필요하면 어시스턴트가 다시 조회합니다",
			r.Emptied)
	default:
		return ""
	}
}
