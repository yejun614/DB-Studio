package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"dbstudio/internal/store"
)

// 허용 모델 목록은 "이 키로 어떤 모델까지 쓸 수 있는가"를 정하는 장치다.
// 판정이 저장·세션 선택·실제 호출 세 곳에서 같아야 하며, 어느 하나라도 빠지면
// 화면에서 고를 수 없는 모델이 API로는 통한다.

func TestNormalizeModels(t *testing.T) {
	got, err := normalizeModels([]string{" gpt-4o ", "gpt-4o", "", "  ", "o3-mini"})
	if err != nil {
		t.Fatalf("normalizeModels: %v", err)
	}
	// 공백 제거, 빈 값 제거, 중복 제거. 순서는 유지한다 — 첫 항목이 기본 모델의
	// 후보이므로 정렬해 버리면 사용자가 고른 우선순위가 사라진다.
	if len(got) != 2 || got[0] != "gpt-4o" || got[1] != "o3-mini" {
		t.Errorf("정리 결과 = %v", got)
	}

	if _, err := normalizeModels([]string{strings.Repeat("가", 121)}); err == nil {
		t.Error("너무 긴 모델 이름이 통과했다")
	}

	many := make([]string, maxAllowedModels+1)
	for i := range many {
		many[i] = string(rune('a'+i%26)) + strings.Repeat("x", i)
	}
	if _, err := normalizeModels(many); err == nil {
		t.Errorf("상한(%d개)을 넘긴 목록이 통과했다", maxAllowedModels)
	}
}

func TestModelAllowed(t *testing.T) {
	// 목록이 비어 있으면 제한이 없다. 기본값이 "아무것도 못 쓴다"가 되면
	// 이미 돌고 있는 설치의 어시스턴트가 업데이트만으로 멈춘다.
	open := &store.AIProvider{Name: "p"}
	if !modelAllowed(open, "무엇이든") {
		t.Error("빈 목록은 제한이 없어야 한다")
	}
	if !modelAllowed(nil, "x") {
		t.Error("프로바이더가 없으면 판정할 것도 없다")
	}

	limited := &store.AIProvider{Name: "p", Models: []string{"gpt-4o", "o3-mini"}}
	if !modelAllowed(limited, "gpt-4o") || !modelAllowed(limited, " o3-mini ") {
		t.Error("목록에 있는 모델이 거부됐다")
	}
	if modelAllowed(limited, "gpt-4o-mini") {
		t.Error("목록에 없는 모델이 통과했다")
	}
}

// 기본 모델이 허용 목록 밖에 있으면 아무도 고르지 않은 모델이 모든 새 대화에
// 쓰이게 된다 — 목록을 정한 의미가 사라진다.
func TestProviderRequestDefaultModelMustBeAllowed(t *testing.T) {
	req := aiProviderRequest{
		Name: "p", Provider: "openai",
		DefaultModel: "gpt-4o-mini", Models: []string{"gpt-4o"},
	}
	if err := req.validate(); err == nil {
		t.Error("목록 밖의 기본 모델이 통과했다")
	}

	// 기본 모델을 비워 두면 첫 항목으로 채운다. 비워 둔 채 저장되면
	// 대화를 시작할 때마다 "모델이 지정되지 않았습니다"가 나온다.
	filled := aiProviderRequest{Name: "p", Provider: "openai", Models: []string{"gpt-4o", "o3"}}
	if err := filled.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if filled.DefaultModel != "gpt-4o" {
		t.Errorf("기본 모델 = %q, 첫 항목이어야 한다", filled.DefaultModel)
	}

	// 목록이 비면 기본 모델은 자유 입력이다(제한 없음).
	free := aiProviderRequest{Name: "p", Provider: "openai", DefaultModel: "llama3.1"}
	if err := free.validate(); err != nil {
		t.Errorf("제한 없는 설정이 거부됐다: %v", err)
	}
}

// 화면에서 고를 수 없는 모델이 API로는 통하면 안 된다.
func TestSessionModelIsGated(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	key := "sk-test"
	p, err := e.st.CreateAIProvider(ctx, store.SaveAIProviderParams{
		Name: "limited", Provider: "openai", DefaultModel: "gpt-4o",
		Models: []string{"gpt-4o"}, APIKey: &key, Enabled: true, IsDefault: true,
	})
	if err != nil {
		t.Fatalf("create provider: %v", err)
	}
	if len(p.Models) != 1 || p.Models[0] != "gpt-4o" {
		t.Fatalf("저장된 허용 모델 = %v", p.Models)
	}

	c := e.client(t)
	if status, body := c.do("POST", "/api/v1/auth/login", map[string]any{
		"username": "alice", "password": testPassword,
	}); status != http.StatusOK {
		t.Fatalf("로그인 = %d (%v)", status, body)
	}

	status, body := c.do("POST", "/api/v1/ai/sessions", map[string]any{
		"title": "금지된 모델", "providerId": p.ID, "model": "gpt-4o-mini",
	})
	if status != http.StatusBadRequest {
		t.Fatalf("허용되지 않은 모델 = %d, want 400 (%v)", status, body)
	}
	if body["error"] != "model_not_allowed" {
		t.Errorf("error = %v, want model_not_allowed", body["error"])
	}

	// 허용된 모델은 통과한다.
	status, body = c.do("POST", "/api/v1/ai/sessions", map[string]any{
		"title": "허용된 모델", "providerId": p.ID, "model": "gpt-4o",
	})
	if status != http.StatusCreated {
		t.Fatalf("허용된 모델 = %d (%v)", status, body)
	}
	sess, _ := body["session"].(map[string]any)
	sessID, _ := sess["id"].(string)

	// 수정으로도 빠져나갈 수 없어야 한다.
	status, body = c.do("PATCH", "/api/v1/ai/sessions/"+sessID, map[string]any{
		"model": "gpt-4o-mini",
	})
	if status != http.StatusBadRequest {
		t.Errorf("수정으로 우회 = %d, want 400 (%v)", status, body)
	}

	// 모델을 비우는 것은 "프로바이더 기본값을 쓴다"는 뜻이므로 허용된다.
	if status, body := c.do("PATCH", "/api/v1/ai/sessions/"+sessID, map[string]any{
		"model": "",
	}); status != http.StatusOK {
		t.Errorf("모델 비우기 = %d (%v)", status, body)
	}
}

// 관리자가 목록에서 모델을 빼면, 그 모델을 쓰던 기존 대화는 조용히 다른 모델로
// 바뀌는 대신 이유를 말하고 멈춰야 한다. 조용히 바꾸면 비용도 답도 달라진 이유를
// 아무도 모른다.
func TestProviderConfigRejectsRevokedModel(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	key := "sk-test"
	p, err := e.st.CreateAIProvider(ctx, store.SaveAIProviderParams{
		Name: "limited", Provider: "openai", DefaultModel: "gpt-4o",
		Models: []string{"gpt-4o"}, APIKey: &key, Enabled: true, IsDefault: true,
	})
	if err != nil {
		t.Fatalf("create provider: %v", err)
	}
	sess, err := e.st.CreateAISession(ctx, store.CreateAISessionParams{
		UserID: e.user.ID, Title: "대화", ProviderID: p.ID, Model: "gpt-4o-mini",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	_, _, cfgErr := e.srv.providerConfig(ctx, sess)
	if cfgErr == nil {
		t.Fatal("허용 목록에서 빠진 모델로 호출이 준비됐다")
	}
	if !strings.Contains(cfgErr.Error(), "gpt-4o-mini") {
		t.Errorf("오류에 문제의 모델 이름이 없다: %v", cfgErr)
	}
}
