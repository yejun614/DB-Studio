package api

import (
	"strings"
	"testing"

	"dbstudio/internal/ai"
)

// 이력 예산은 프로바이더의 컨텍스트 크기를 따라야 한다.
//
// 예전에는 12만 자 하나가 모든 프로바이더에 쓰였다. 그 값은 어느 쪽으로든 틀린다:
// Claude(20만 토큰)에서는 멀쩡한 이력을 이유 없이 버리고, 로컬 Ollama(기본 4~8천
// 토큰)에서는 넘치는 줄도 모르고 보낸다 — 그러면 Ollama가 말없이 앞을 잘라내고,
// 시스템 프롬프트가 먼저 사라진다.
func TestHistoryBudgetFollowsContext(t *testing.T) {
	// 모르면 예전 기본값 그대로. 이미 돌고 있는 설치가 갑자기 달라지면 안 된다.
	if got := historyBudget(0); got != defaultHistoryChars {
		t.Errorf("모를 때 = %d, 기대 %d", got, defaultHistoryChars)
	}
	if got := historyBudget(-1); got != defaultHistoryChars {
		t.Errorf("음수일 때 = %d, 기대 %d", got, defaultHistoryChars)
	}

	small := historyBudget(8192)   // 로컬 Ollama의 흔한 기본값
	large := historyBudget(200000) // Claude
	if small >= defaultHistoryChars {
		t.Errorf("작은 컨텍스트(%d)가 기본값(%d)보다 넉넉합니다", small, defaultHistoryChars)
	}
	if large <= defaultHistoryChars {
		t.Errorf("큰 컨텍스트(%d)가 기본값(%d)보다 빡빡합니다", large, defaultHistoryChars)
	}
	if small >= large {
		t.Errorf("8k 예산 %d 가 200k 예산 %d 보다 큽니다", small, large)
	}

	// 아무리 작아도 바닥은 있다. 직전 질문 하나도 못 넣을 바에는 넣고 프로바이더의
	// 판단에 맡긴다 — 적어도 그쪽은 오류로 알려주기라도 한다.
	if got := historyBudget(64); got < 4_000 {
		t.Errorf("아주 작은 컨텍스트의 예산 = %d, 바닥이 없습니다", got)
	}
}

// 예산을 넘으면 오래된 것부터 버린다.
func TestTrimHistoryHonoursBudget(t *testing.T) {
	msg := func(text string) ai.Message {
		return ai.Message{Role: ai.RoleUser, Text: text}
	}
	long := strings.Repeat("가", 3_000)
	all := []ai.Message{msg("첫 질문"), msg(long), msg(long), msg("마지막 질문")}

	// 넉넉하면 그대로 둔다.
	if got := trimHistory(all, 100_000); len(got) != 4 {
		t.Errorf("넉넉할 때 %d개가 남았습니다", len(got))
	}

	// 빡빡하면 뒤쪽(최근)만 남는다. 최근 대화가 지금 질문과 관련이 깊고,
	// 툴 결과는 특히 최근 것이 유효하다.
	got := trimHistory(all, 4_000)
	if len(got) == 0 || len(got) >= 4 {
		t.Fatalf("빡빡할 때 %d개가 남았습니다: %v", len(got), got)
	}
	if got[len(got)-1].Text != "마지막 질문" {
		t.Errorf("마지막 메시지가 사라졌습니다: %q", got[len(got)-1].Text)
	}

	// 0은 "정하지 않음"이라 기본값으로 돈다.
	if len(trimHistory(all, 0)) != 4 {
		t.Error("예산 0에서 이력이 잘렸습니다")
	}
}
