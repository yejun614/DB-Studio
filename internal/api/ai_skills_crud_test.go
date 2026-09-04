package api

import (
	"net/http"
	"strings"
	"testing"
)

// 사용자가 만든 스킬의 왕복. 만들고 → 목록에 앱의 것과 함께 나오고 → 고치고 → 지운다.
func TestUserSkillLifecycle(t *testing.T) {
	e := newTestEnv(t)
	c := login(t, e, "alice")

	status, body := c.do("POST", "/api/v1/ai/skills", map[string]any{
		"name":        "감사 컬럼 붙이기",
		"description": "생성일시·수정일시를 붙입니다",
		"prompt":      "{{document}} 초안의 {{table}} 표에 감사 컬럼을 붙여 주세요.",
		"args": []map[string]any{
			{"key": "document", "label": "초안", "type": "erd"},
			{"key": "table", "label": "표 이름", "type": "text"},
		},
	})
	if status != http.StatusCreated {
		t.Fatalf("create = %d: %v", status, body)
	}
	skill, _ := body["skill"].(map[string]any)
	id, _ := skill["id"].(string)
	if id == "" {
		t.Fatalf("만든 스킬에 id 가 없습니다: %v", body)
	}
	if mine, _ := skill["mine"].(bool); !mine {
		t.Error("내가 만든 스킬이 내 것으로 오지 않습니다")
	}
	if builtin, _ := skill["builtin"].(bool); builtin {
		t.Error("사용자 스킬이 앱의 것으로 표시됐습니다")
	}

	// 목록에는 앱의 것과 내 것이 함께 나온다.
	status, body = c.do("GET", "/api/v1/ai/skills", nil)
	if status != http.StatusOK {
		t.Fatalf("list = %d: %v", status, body)
	}
	items, _ := body["items"].([]any)
	names := map[string]bool{}
	builtins := 0
	for _, it := range items {
		m, _ := it.(map[string]any)
		name, _ := m["name"].(string)
		names[name] = true
		if b, _ := m["builtin"].(bool); b {
			builtins++
		}
	}
	if !names["감사 컬럼 붙이기"] {
		t.Errorf("만든 스킬이 목록에 없습니다: %v", names)
	}
	if builtins == 0 {
		t.Error("앱의 스킬이 목록에서 사라졌습니다")
	}

	status, body = c.do("PUT", "/api/v1/ai/skills/"+id, map[string]any{
		"name":   "감사 컬럼 붙이기 v2",
		"prompt": "{{document}} 에 감사 컬럼을 붙여 주세요.",
		"args": []map[string]any{
			{"key": "document", "label": "초안", "type": "erd"},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("update = %d: %v", status, body)
	}

	if status, body = c.do("DELETE", "/api/v1/ai/skills/"+id, nil); status != http.StatusNoContent {
		t.Fatalf("delete = %d: %v", status, body)
	}
	status, body = c.do("GET", "/api/v1/ai/skills", nil)
	items, _ = body["items"].([]any)
	for _, it := range items {
		m, _ := it.(map[string]any)
		if name, _ := m["name"].(string); strings.HasPrefix(name, "감사 컬럼") {
			t.Errorf("지운 스킬이 목록에 남았습니다: %s", name)
		}
	}
}

// 지시문과 입력값이 어긋나면 저장을 막는다. 둘 다 화면에서는 "답이 좀 이상하다"로만
// 보이는 종류의 어긋남이라, 만들 때 막지 않으면 알아챌 자리가 없다.
func TestUserSkillValidation(t *testing.T) {
	e := newTestEnv(t)
	c := login(t, e, "alice")

	cases := []struct {
		name string
		body map[string]any
		want string
	}{
		{"이름 없음", map[string]any{"name": "  ", "prompt": "무언가"}, "이름"},
		{"지시문 없음", map[string]any{"name": "스킬", "prompt": " "}, "지시문"},
		{
			"채울 값이 없는 자리",
			map[string]any{"name": "스킬", "prompt": "{{missing}} 를 봐 주세요"},
			"{{missing}}",
		},
		{
			"쓰이지 않는 입력값",
			map[string]any{"name": "스킬", "prompt": "그냥 봐 주세요",
				"args": []map[string]any{{"key": "unused", "type": "text"}}},
			"unused",
		},
		{
			"모르는 입력값 종류",
			map[string]any{"name": "스킬", "prompt": "{{x}} 봐 주세요",
				"args": []map[string]any{{"key": "x", "type": "colour"}}},
			"종류",
		},
		{
			"열쇠가 겹침",
			map[string]any{"name": "스킬", "prompt": "{{x}} 봐 주세요",
				"args": []map[string]any{{"key": "x", "type": "text"}, {"key": "x", "type": "text"}}},
			"겹칩니다",
		},
	}
	for _, tc := range cases {
		status, body := c.do("POST", "/api/v1/ai/skills", tc.body)
		if status != http.StatusBadRequest {
			t.Errorf("%s: status = %d (400 이어야 한다): %v", tc.name, status, body)
			continue
		}
		msg, _ := body["message"].(string)
		if !strings.Contains(msg, tc.want) {
			t.Errorf("%s: 거절 사유가 이유를 말하지 않습니다: %q", tc.name, msg)
		}
	}
}

// 공유한 스킬은 남도 **쓸** 수 있지만 고치는 것은 주인뿐이다. 남이 쓰고 있는 글을
// 아무나 바꿀 수 있으면 "어제 쓰던 스킬이 오늘 다른 말을 한다"가 된다.
func TestSharedSkillIsReadOnlyForOthers(t *testing.T) {
	e := newTestEnv(t)
	owner := login(t, e, "alice")
	addMember(t, e, "bob")
	other := login(t, e, "bob")

	status, body := owner.do("POST", "/api/v1/ai/skills", map[string]any{
		"name":   "팀 리뷰 규칙",
		"prompt": "{{document}} 를 우리 팀 규칙으로 리뷰해 주세요.",
		"args":   []map[string]any{{"key": "document", "label": "초안", "type": "erd"}},
		"shared": true,
	})
	if status != http.StatusCreated {
		t.Fatalf("create = %d: %v", status, body)
	}
	skill, _ := body["skill"].(map[string]any)
	id, _ := skill["id"].(string)

	// 남도 목록에서 본다.
	status, body = other.do("GET", "/api/v1/ai/skills", nil)
	if status != http.StatusOK {
		t.Fatalf("list = %d: %v", status, body)
	}
	items, _ := body["items"].([]any)
	seen := false
	for _, it := range items {
		m, _ := it.(map[string]any)
		if name, _ := m["name"].(string); name == "팀 리뷰 규칙" {
			seen = true
			if mine, _ := m["mine"].(bool); mine {
				t.Error("남의 스킬이 내 것으로 표시됩니다")
			}
			if owner, _ := m["owner"].(string); owner == "" {
				t.Error("공유된 스킬에 만든 사람이 없습니다")
			}
		}
	}
	if !seen {
		t.Error("공유한 스킬이 남의 목록에 없습니다")
	}

	// 그러나 고치거나 지울 수는 없다.
	if status, _ = other.do("PUT", "/api/v1/ai/skills/"+id, map[string]any{
		"name": "바꿔치기", "prompt": "무언가 다른 것",
	}); status != http.StatusForbidden {
		t.Errorf("남이 고칠 수 있습니다: status = %d", status)
	}
	if status, _ = other.do("DELETE", "/api/v1/ai/skills/"+id, nil); status != http.StatusForbidden {
		t.Errorf("남이 지울 수 있습니다: status = %d", status)
	}

	// 공유하지 않은 스킬은 아예 보이지 않는다.
	if _, cbody := owner.do("POST", "/api/v1/ai/skills", map[string]any{
		"name": "내 메모", "prompt": "혼자 쓰는 지시문입니다",
	}); cbody == nil {
		t.Fatal("두 번째 스킬을 만들지 못했습니다")
	}
	_, body = other.do("GET", "/api/v1/ai/skills", nil)
	items, _ = body["items"].([]any)
	for _, it := range items {
		m, _ := it.(map[string]any)
		if name, _ := m["name"].(string); name == "내 메모" {
			t.Error("공유하지 않은 스킬이 남에게 보입니다")
		}
	}
}
