package api

import (
	"slices"
	"strings"
	"testing"

	"dbstudio/internal/model"
)

// 툴 레지스트리는 화면·MCP·어시스턴트가 함께 쓰는 한 벌이다.
// 정의가 어긋나면 그 사실이 "모델이 부르는 순간"에야 드러나는데, 그때는 이미
// 사용자 대화 중이고 오류 문구도 모델이 지어낸 것과 섞여 원인을 찾기 어렵다.
func TestToolRegistryIsWellFormed(t *testing.T) {
	for name, def := range aiTools() {
		if name != def.Name {
			t.Errorf("레지스트리 키 %q 와 Name %q 가 다르다", name, def.Name)
		}
		if strings.TrimSpace(def.Description) == "" {
			t.Errorf("%s: 설명이 비었다 — 모델이 언제 쓸지 판단할 수 없다", name)
		}
		if def.Schema == nil || def.Schema["type"] != "object" {
			t.Errorf("%s: 스키마가 객체가 아니다: %v", name, def.Schema)
		}
		// required에 적힌 키는 properties에 있어야 한다.
		props, _ := def.Schema["properties"].(map[string]any)
		if req, ok := def.Schema["required"].([]string); ok {
			for _, key := range req {
				if _, has := props[key]; !has {
					t.Errorf("%s: required의 %q 가 properties에 없다", name, key)
				}
			}
		}

		if def.Mutating {
			// 쓰기 툴은 제안과 실행이 짝을 이뤄야 한다. 하나만 있으면
			// 승인 화면이 비거나 승인해도 아무 일이 없다.
			if def.Propose == nil || def.Apply == nil {
				t.Errorf("%s: 쓰기 툴에 Propose/Apply가 모두 필요하다", name)
			}
			if def.Run != nil {
				t.Errorf("%s: 쓰기 툴에 Run이 있으면 모델이 승인 없이 실행하게 된다", name)
			}
		} else if def.Run == nil {
			t.Errorf("%s: 읽기 툴에 Run이 없다", name)
		}
	}
}

// P14·P15 기능이 실제로 노출되는지 고정한다.
// 기능을 만들고 툴 등록을 빠뜨리는 것은 조용한 누락이라 눈에 띄지 않는다.
func TestInfraToolsAreRegistered(t *testing.T) {
	registry := aiTools()
	want := []string{
		"list_servers", "list_server_databases", "register_databases",
		"list_macro_triggers", "create_macro_trigger",
		"set_macro_trigger_enabled", "delete_macro_trigger",
	}
	for _, name := range want {
		if _, ok := registry[name]; !ok {
			t.Errorf("%s 툴이 등록되지 않았다", name)
		}
	}
}

// 툴 목록은 사용자의 권한에 따라 줄어야 한다.
//
// 보이는데 부르면 거부되는 툴은 모델이 계속 시도하며 토큰을 쓰고, 사용자에게는
// "되는 줄 알았는데 안 된다"로 보인다. 노출 단계에서 걸러야 한다.
func TestToolVisibilityFollowsPermissions(t *testing.T) {
	names := func(u *model.User, hints toolHints) []string {
		tools, _ := availableTools(u, hints)
		out := make([]string, 0, len(tools))
		for _, t := range tools {
			out = append(out, t.Name)
		}
		return out
	}

	member := &model.User{Role: model.RoleMember, Status: model.UserActive}
	admin := &model.User{Role: model.RoleAdmin, Status: model.UserActive}
	macroMember := &model.User{
		Role: model.RoleMember, Status: model.UserActive,
		Perms: []model.Perm{model.PermMacro},
	}

	memberTools := names(member, toolHints{})
	if slices.Contains(memberTools, "list_server_databases") {
		t.Error("멤버에게 서버 DB 목록 조회가 보인다 — 커넥션 관리자만 쓸 수 있다")
	}
	if slices.Contains(memberTools, "register_databases") {
		t.Error("멤버에게 DB 등록이 보인다")
	}
	if !slices.Contains(memberTools, "list_servers") {
		t.Error("서버 목록은 누구나 볼 수 있어야 한다(접근 가능한 DB만 담긴다)")
	}
	if slices.Contains(memberTools, "list_macro_triggers") {
		t.Error("매크로 권한이 없는데 트리거 툴이 보인다")
	}

	adminTools := names(admin, toolHints{})
	for _, want := range []string{"list_server_databases", "register_databases"} {
		if !slices.Contains(adminTools, want) {
			t.Errorf("어드민에게 %s 가 보이지 않는다 — 커넥션 관리는 어드민의 일이다", want)
		}
	}
	// 어드민이라도 매크로 권한이 없으면 트리거 툴은 보이지 않는다.
	if slices.Contains(adminTools, "create_macro_trigger") {
		t.Error("매크로 권한 없는 어드민에게 트리거 생성이 보인다")
	}

	macroTools := names(macroMember, toolHints{})
	for _, want := range []string{
		"list_macro_triggers", "create_macro_trigger",
		"set_macro_trigger_enabled", "delete_macro_trigger",
	} {
		if !slices.Contains(macroTools, want) {
			t.Errorf("매크로 권한자에게 %s 가 보이지 않는다", want)
		}
	}
}

// 자격증명을 받는 툴은 없어야 한다.
//
// 인자로 받으면 비밀번호가 대화 기록에 남고 AI 프로바이더로 전송된다.
// 서버 등록·수정을 툴로 만들지 않은 이유가 이것이고, 나중에 누군가 편의를 위해
// 되살릴 수 있으므로 여기서 고정한다.
func TestNoToolAcceptsCredentials(t *testing.T) {
	banned := []string{"password", "secret", "passwd", "credential", "token", "apikey", "api_key"}
	for name, def := range aiTools() {
		props, _ := def.Schema["properties"].(map[string]any)
		for key := range props {
			lower := strings.ToLower(key)
			for _, bad := range banned {
				if strings.Contains(lower, bad) {
					t.Errorf("%s 툴이 %q 를 인자로 받는다 — 비밀은 툴 인자로 오가면 안 된다", name, key)
				}
			}
		}
	}
}
