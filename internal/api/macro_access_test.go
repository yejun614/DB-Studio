package api

import (
	"context"
	"testing"

	"dbstudio/internal/crypto"
	"dbstudio/internal/model"
	"dbstudio/internal/store"
)

// 매크로 접근 정책을 HTTP로 확인한다.
//
// 저장 계층 시험(store)은 규칙이 맞는지 보고, 이 시험은 **그 규칙이 모든 경로에
// 실제로 걸려 있는지**를 본다. 판정 함수가 아무리 옳아도 핸들러 하나가 그것을 부르지
// 않으면 그 경로는 열려 있고, 그 사실은 저장 계층 시험에 잡히지 않는다.

// macroMember는 매크로 권한을 가진 일반 사용자를 만든다.
func macroMember(t *testing.T, e *testEnv, name string) *model.User {
	t.Helper()
	hash, err := crypto.HashPassword(testPassword)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	u, err := e.st.CreateUser(context.Background(), store.CreateUserParams{
		Username: name, DisplayName: name, Role: model.RoleMember, PasswordHash: hash,
	})
	if err != nil {
		t.Fatalf("create user %s: %v", name, err)
	}
	perms := []model.Perm{model.PermMacro}
	u, err = e.st.UpdateUser(context.Background(), u.ID, store.UpdateUserParams{Perms: &perms})
	if err != nil {
		t.Fatalf("grant macro perm to %s: %v", name, err)
	}
	return u
}

func loginAs(t *testing.T, e *testEnv, name string) *client {
	t.Helper()
	c := e.client(t)
	status, body := c.do("POST", "/api/v1/auth/login",
		map[string]string{"username": name, "password": testPassword})
	if status != 200 {
		t.Fatalf("login %s = %d: %v", name, status, body)
	}
	return c
}

// 새 매크로는 비공개이고, 남에게는 존재하지 않는 것처럼 보여야 한다.
//
// 404를 확인하는 것이 핵심이다. 403은 "그런 매크로가 있다"를 알려주고,
// 비공개 매크로에서는 그 사실 자체가 새어 나가면 안 되는 정보다.
func TestPrivateMacroIsInvisible(t *testing.T) {
	e := newTestEnv(t)
	macroMember(t, e, "bob")
	macroMember(t, e, "carol")

	bob := loginAs(t, e, "bob")
	status, body := bob.do("POST", "/api/v1/macros/",
		map[string]string{"name": "밤 정리", "description": ""})
	if status != 201 {
		t.Fatalf("create = %d: %v", status, body)
	}
	m, _ := body["macro"].(map[string]any)
	id, _ := m["id"].(string)
	if m["visibility"] != "private" {
		t.Errorf("새 매크로는 비공개여야 한다: %v", m["visibility"])
	}
	if m["access"] != "owner" {
		t.Errorf("만든 사람은 owner여야 한다: %v", m["access"])
	}

	carol := loginAs(t, e, "carol")
	if status, _ := carol.do("GET", "/api/v1/macros/"+id, nil); status != 404 {
		t.Errorf("남에게는 404여야 한다(403이면 존재가 새어 나간다): %d", status)
	}
	if status, _ := carol.do("POST", "/api/v1/macros/"+id+"/run", nil); status != 404 {
		t.Errorf("실행도 404여야 한다: %d", status)
	}
	if status, _ := carol.do("DELETE", "/api/v1/macros/"+id, nil); status != 404 {
		t.Errorf("삭제도 404여야 한다: %d", status)
	}
	status, body = carol.do("GET", "/api/v1/macros/", nil)
	if status != 200 {
		t.Fatalf("list = %d", status)
	}
	if items, _ := body["items"].([]any); len(items) != 0 {
		t.Errorf("남의 목록에 비공개 매크로가 나왔다: %v", items)
	}
}

// 공개(조회+실행)는 볼 수 있게 하되 고칠 수는 없게 한다.
// 여기서는 404가 아니라 403이 맞다 — 존재를 이미 공개했기 때문이다.
func TestPublicViewAllowsRunNotEdit(t *testing.T) {
	e := newTestEnv(t)
	macroMember(t, e, "bob")
	macroMember(t, e, "carol")

	bob := loginAs(t, e, "bob")
	_, body := bob.do("POST", "/api/v1/macros/", map[string]string{"name": "공유용"})
	id, _ := body["macro"].(map[string]any)["id"].(string)

	status, _ := bob.do("PUT", "/api/v1/macros/"+id+"/access",
		map[string]string{"visibility": "public", "publicAccess": "view"})
	if status != 200 {
		t.Fatalf("access = %d", status)
	}

	carol := loginAs(t, e, "carol")
	status, got := carol.do("GET", "/api/v1/macros/"+id, nil)
	if status != 200 {
		t.Fatalf("공개 매크로는 열려야 한다: %d", status)
	}
	m, _ := got["macro"].(map[string]any)
	if m["access"] != "view" || m["canEdit"] != false {
		t.Errorf("조회 권한이어야 한다: %v", m)
	}

	// 수정 경로는 전부 403.
	for _, call := range []struct{ method, path string }{
		{"PATCH", "/api/v1/macros/" + id},
		{"POST", "/api/v1/macros/" + id + "/versions"},
		{"POST", "/api/v1/macros/" + id + "/versions/1/restore"},
	} {
		status, _ := carol.do(call.method, call.path, map[string]string{"name": "바꿔치기"})
		if status != 403 {
			t.Errorf("%s %s = %d, want 403", call.method, call.path, status)
		}
	}
	// 삭제와 공유 설정, 자동 실행은 공개+수정이어도 소유자·협업자만 한다.
	if status, _ := carol.do("DELETE", "/api/v1/macros/"+id, nil); status != 403 {
		t.Errorf("삭제 = %d, want 403", status)
	}
	if status, _ := carol.do("PUT", "/api/v1/macros/"+id+"/access",
		map[string]string{"visibility": "private"}); status != 403 {
		t.Errorf("공유 설정 = %d, want 403", status)
	}
}

// 공개+수정은 그래프를 고칠 수 있게 하되 관리와 삭제는 열지 않는다.
func TestPublicEditStopsAtManage(t *testing.T) {
	e := newTestEnv(t)
	macroMember(t, e, "bob")
	macroMember(t, e, "carol")

	bob := loginAs(t, e, "bob")
	_, body := bob.do("POST", "/api/v1/macros/", map[string]string{"name": "함께 고치는 것"})
	id, _ := body["macro"].(map[string]any)["id"].(string)
	bob.do("PUT", "/api/v1/macros/"+id+"/access",
		map[string]string{"visibility": "public", "publicAccess": "edit"})

	carol := loginAs(t, e, "carol")
	status, _ := carol.do("PATCH", "/api/v1/macros/"+id,
		map[string]string{"name": "이름 바꿈", "description": "캐럴이 고침"})
	if status != 200 {
		t.Errorf("공개+수정이면 이름을 바꿀 수 있어야 한다: %d", status)
	}
	if status, _ := carol.do("POST", "/api/v1/macros/"+id+"/triggers",
		map[string]any{"name": "매일", "kind": "schedule", "cron": "0 3 * * *"}); status != 403 {
		t.Errorf("자동 실행은 관리 권한이어야 한다: %d, want 403", status)
	}
	if status, _ := carol.do("DELETE", "/api/v1/macros/"+id, nil); status != 403 {
		t.Errorf("삭제 = %d, want 403", status)
	}
}

// 협업자는 수정·관리까지 하고 삭제만 못 한다.
func TestCollaboratorCanManageButNotDelete(t *testing.T) {
	e := newTestEnv(t)
	macroMember(t, e, "bob")
	carolUser := macroMember(t, e, "carol")

	bob := loginAs(t, e, "bob")
	_, body := bob.do("POST", "/api/v1/macros/", map[string]string{"name": "둘이 하는 것"})
	id, _ := body["macro"].(map[string]any)["id"].(string)

	status, out := bob.do("POST", "/api/v1/macros/"+id+"/collaborators",
		map[string]string{"userId": carolUser.ID})
	if status != 200 {
		t.Fatalf("협업자 추가 = %d: %v", status, out)
	}
	if items, _ := out["items"].([]any); len(items) != 1 {
		t.Fatalf("협업자 1명이어야 한다: %v", out["items"])
	}

	carol := loginAs(t, e, "carol")
	status, got := carol.do("GET", "/api/v1/macros/"+id, nil)
	if status != 200 {
		t.Fatalf("협업자는 비공개 매크로를 열 수 있어야 한다: %d", status)
	}
	m, _ := got["macro"].(map[string]any)
	if m["access"] != "manage" || m["canManage"] != true || m["canDelete"] != false {
		t.Errorf("협업자 권한이 잘못되었다: %v", m)
	}

	// 관리(공유 설정)는 되고 삭제는 안 된다.
	if status, _ := carol.do("PUT", "/api/v1/macros/"+id+"/access",
		map[string]string{"visibility": "public", "publicAccess": "view"}); status != 200 {
		t.Errorf("협업자는 공유 설정을 바꿀 수 있어야 한다: %d", status)
	}
	if status, _ := carol.do("DELETE", "/api/v1/macros/"+id, nil); status != 403 {
		t.Errorf("협업자 삭제 = %d, want 403", status)
	}

	// 소유자가 협업자를 빼면 다시 조회 등급으로 내려간다(공개+조회이므로).
	if status, _ := bob.do("DELETE",
		"/api/v1/macros/"+id+"/collaborators/"+carolUser.ID, nil); status != 200 {
		t.Errorf("협업자 제외 = %d", status)
	}
	_, got = carol.do("GET", "/api/v1/macros/"+id, nil)
	if m, _ := got["macro"].(map[string]any); m["access"] != "view" {
		t.Errorf("제외 후 권한 = %v, want view", m["access"])
	}
}

// 슈퍼어드민은 모든 매크로를 조회·수정·관리·삭제한다.
func TestSuperadminSeesEveryMacro(t *testing.T) {
	e := newTestEnv(t)
	macroMember(t, e, "bob")

	bob := loginAs(t, e, "bob")
	_, body := bob.do("POST", "/api/v1/macros/", map[string]string{"name": "비공개"})
	id, _ := body["macro"].(map[string]any)["id"].(string)

	// alice는 newTestEnv가 만드는 슈퍼어드민이다.
	alice := loginAs(t, e, "alice")
	status, got := alice.do("GET", "/api/v1/macros/"+id, nil)
	if status != 200 {
		t.Fatalf("슈퍼어드민은 볼 수 있어야 한다: %d", status)
	}
	if m, _ := got["macro"].(map[string]any); m["access"] != "owner" {
		t.Errorf("슈퍼어드민 권한 = %v, want owner", m["access"])
	}
	if status, _ := alice.do("DELETE", "/api/v1/macros/"+id, nil); status != 200 {
		t.Errorf("슈퍼어드민 삭제 = %d", status)
	}
}

// 실행 기록은 매크로가 비공개로 돌아가도 실행한 본인에게는 남아야 한다.
// 그 기록은 매크로의 것이기도 하지만 실행한 사람의 것이기도 하다.
func TestRunHistoryFollowsAccess(t *testing.T) {
	e := newTestEnv(t)
	macroMember(t, e, "bob")
	macroMember(t, e, "carol")

	bob := loginAs(t, e, "bob")
	_, body := bob.do("POST", "/api/v1/macros/", map[string]string{"name": "돌려보는 것"})
	id, _ := body["macro"].(map[string]any)["id"].(string)
	bob.do("PUT", "/api/v1/macros/"+id+"/access",
		map[string]string{"visibility": "public", "publicAccess": "view"})

	// 캐럴이 공개 상태에서 한 번 돌린다(시작 노드뿐이라 곧바로 끝난다).
	carol := loginAs(t, e, "carol")
	status, out := carol.do("POST", "/api/v1/macros/"+id+"/run", nil)
	if status != 200 {
		t.Fatalf("공개 매크로 실행 = %d: %v", status, out)
	}
	runID, _ := out["runId"].(string)

	// 다시 비공개로 돌린다.
	bob.do("PUT", "/api/v1/macros/"+id+"/access", map[string]string{"visibility": "private"})

	if status, _ := carol.do("GET", "/api/v1/macros/"+id, nil); status != 404 {
		t.Errorf("비공개가 된 매크로 = %d, want 404", status)
	}
	if status, _ := carol.do("GET", "/api/v1/macros/runs/"+runID, nil); status != 200 {
		t.Errorf("자기가 돌린 실행 기록은 남아야 한다: %d", status)
	}

	// 아무 관계 없는 사람에게는 그 기록도 보이지 않는다.
	macroMember(t, e, "dave")
	dave := loginAs(t, e, "dave")
	if status, _ := dave.do("GET", "/api/v1/macros/runs/"+runID, nil); status != 404 {
		t.Errorf("남의 실행 기록 = %d, want 404", status)
	}
}

// 매크로 권한 자체가 없으면 공개 매크로도 보이지 않는다.
// 공개는 앱 안에서의 공개이지 익명 공개가 아니다.
func TestPublicStillRequiresMacroPerm(t *testing.T) {
	e := newTestEnv(t)
	macroMember(t, e, "bob")

	hash, err := crypto.HashPassword(testPassword)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if _, err := e.st.CreateUser(context.Background(), store.CreateUserParams{
		Username: "eve", DisplayName: "Eve", Role: model.RoleMember, PasswordHash: hash,
	}); err != nil {
		t.Fatalf("create user: %v", err)
	}

	bob := loginAs(t, e, "bob")
	_, body := bob.do("POST", "/api/v1/macros/", map[string]string{"name": "공개"})
	id, _ := body["macro"].(map[string]any)["id"].(string)
	bob.do("PUT", "/api/v1/macros/"+id+"/access",
		map[string]string{"visibility": "public", "publicAccess": "edit"})

	eve := loginAs(t, e, "eve")
	// 그룹 미들웨어(requirePerm)가 막으므로 403이다 — 여기서는 매크로 하나가 아니라
	// 매크로 메뉴 전체가 없는 것이라 존재를 숨길 것이 없다.
	if status, _ := eve.do("GET", "/api/v1/macros/"+id, nil); status != 403 {
		t.Errorf("매크로 권한 없는 사용자 = %d, want 403", status)
	}
}
