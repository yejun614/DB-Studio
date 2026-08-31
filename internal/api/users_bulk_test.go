package api

import (
	"context"
	"testing"

	"dbstudio/internal/model"
	"dbstudio/internal/store"
)

// 여러 사람을 한 번에, 같은 권한으로.
//
// 같은 값을 손으로 다섯 번 적는 일에서는 반드시 한 번이 어긋나고, 어긋난 그 한 명은
// "왜 나만 안 보이지"로 며칠 뒤에 발견된다.
func TestBulkCreateUsersSharesOnePolicy(t *testing.T) {
	e := newTestEnv(t)
	alice := loginAs(t, e, "alice")

	code, body := alice.do("POST", "/api/v1/users/bulk", map[string]any{
		"text":     "# 주석\ndev1, 홍길동, dev1@example.com\ndev2\t김철수\n망!가!진\ndev1, 두 번째 줄\n",
		"role":     "member",
		"projects": []string{e.project.ID},
	})
	if code != 201 {
		t.Fatalf("여러 명 추가 = %d: %v", code, body)
	}
	created, _ := body["created"].([]any)
	invalid, _ := body["invalid"].([]any)
	if len(created) != 2 {
		t.Fatalf("만들어진 계정 = %d개, 기대 2개: %v", len(created), body)
	}
	if len(invalid) != 1 {
		t.Errorf("형식 오류 = %d줄, 기대 1줄", len(invalid))
	}

	// 임시 비밀번호는 계정마다 다르다. 하나를 나눠 쓰면 그것이 새는 순간 전부 열린다.
	seen := map[string]bool{}
	for _, raw := range created {
		row, _ := raw.(map[string]any)
		pw, _ := row["temporaryPassword"].(string)
		if pw == "" {
			t.Fatalf("임시 비밀번호가 없습니다: %v", row)
		}
		if seen[pw] {
			t.Error("두 계정이 같은 임시 비밀번호를 받았습니다")
		}
		seen[pw] = true

		u, _ := row["user"].(map[string]any)
		if u["mustChangePassword"] != true {
			t.Errorf("%v: 첫 로그인 변경 강제가 꺼져 있습니다", u["username"])
		}
		if u["role"] != "member" {
			t.Errorf("%v: 역할 = %v", u["username"], u["role"])
		}
	}

	// 둘 다 같은 프로젝트에 들어갔다.
	ctx := context.Background()
	for _, name := range []string{"dev1", "dev2"} {
		id := mustUserID(t, e, name)
		ok, err := e.st.IsProjectMember(ctx, e.project.ID, id)
		if err != nil {
			t.Fatalf("참여 확인: %v", err)
		}
		if !ok {
			t.Errorf("%s 가 프로젝트에 들어가지 않았습니다", name)
		}
	}

	// 이름과 이메일도 줄에서 읽는다.
	_, list := alice.do("GET", "/api/v1/users/", nil)
	for _, u := range asList(list["users"]) {
		if u["username"] == "dev1" && u["displayName"] != "홍길동" {
			t.Errorf("이름을 읽지 못했습니다: %v", u["displayName"])
		}
		if u["username"] == "dev1" && u["email"] != "dev1@example.com" {
			t.Errorf("이메일을 읽지 못했습니다: %v", u["email"])
		}
		if u["username"] == "dev2" && u["displayName"] != "김철수" {
			t.Errorf("탭 구분을 읽지 못했습니다: %v", u["displayName"])
		}
	}

	// 두 번 올리면 이미 있는 것은 건너뛰고 멈추지 않는다.
	code, again := alice.do("POST", "/api/v1/users/bulk", map[string]any{
		"text": "dev1\ndev3\n", "role": "member",
	})
	if code != 201 {
		t.Fatalf("두 번째 = %d: %v", code, again)
	}
	if got, _ := again["created"].([]any); len(got) != 1 {
		t.Errorf("두 번째로 만들어진 계정 = %d개, 기대 1개", len(got))
	}
	if got, _ := again["skipped"].([]any); len(got) != 1 {
		t.Errorf("건너뛴 계정 = %d개, 기대 1개(dev1)", len(got))
	}
}

// 권한 원본을 고르면 그 사람의 정책과 전역 권한이 그대로 복사된다.
//
// "김대리와 같은 권한으로"가 실제로 부탁받는 형태다. 등급을 손으로 다시 적게 하면
// 그 자리에서 어긋난다.
func TestBulkCreateUsersCopiesPolicy(t *testing.T) {
	e, conn, _ := assignEnv(t)
	alice := loginAs(t, e, "alice")

	// dana에게 이 DB의 마이그레이션 등급과 매크로 권한을 준다.
	dana := member(t, e, "dana", conn.ID, model.LevelMigrate)
	ctx := context.Background()
	policy, err := e.st.GetAccessPolicy(ctx, dana.ID)
	if err != nil {
		t.Fatalf("policy: %v", err)
	}
	policy.DefaultCaps = []model.Capability{model.CapDataRead}
	if err := e.st.SetAccessPolicy(ctx, policy); err != nil {
		t.Fatalf("set policy: %v", err)
	}
	perms := []model.Perm{model.PermMacro}
	if _, err := e.st.UpdateUser(ctx, dana.ID, store.UpdateUserParams{Perms: &perms}); err != nil {
		t.Fatalf("perms: %v", err)
	}

	code, body := alice.do("POST", "/api/v1/users/bulk", map[string]any{
		"text": "dev1\ndev2\n", "role": "member", "copyFrom": dana.ID,
		"projects": []string{e.project.ID},
	})
	if code != 201 {
		t.Fatalf("복사 추가 = %d: %v", code, body)
	}

	for _, name := range []string{"dev1", "dev2"} {
		id := mustUserID(t, e, name)
		got, err := e.st.GetAccessPolicy(ctx, id)
		if err != nil {
			t.Fatalf("policy %s: %v", name, err)
		}
		if got.Capabilities[conn.ID] != model.LevelMigrate {
			t.Errorf("%s: DB 등급 = %v, 원본과 같아야 합니다", name, got.Capabilities[conn.ID])
		}
		if len(got.DefaultCaps) != 1 || got.DefaultCaps[0] != model.CapDataRead {
			t.Errorf("%s: 데이터 능력 = %v", name, got.DefaultCaps)
		}
		u, err := e.st.GetUser(ctx, id)
		if err != nil {
			t.Fatalf("get %s: %v", name, err)
		}
		if len(u.Perms) != 1 || u.Perms[0] != model.PermMacro {
			t.Errorf("%s: 전역 권한 = %v", name, u.Perms)
		}
	}
}

// 슈퍼 어드민은 권한 원본이 될 수 없다.
//
// 그 권한은 역할에서 나오므로 정책에 적혀 있는 것이 없다. 복사해 놓고 "같은 권한"이라
// 하면 아무 권한도 없는 계정이 조용히 생긴다.
func TestBulkCreateUsersRejectsSuperadminTemplate(t *testing.T) {
	e := newTestEnv(t)
	alice := loginAs(t, e, "alice")

	code, body := alice.do("POST", "/api/v1/users/bulk", map[string]any{
		"text": "dev1\n", "role": "member", "copyFrom": e.user.ID,
	})
	if code != 400 {
		t.Errorf("슈퍼 어드민 복사 = %d, 400이어야 합니다: %v", code, body)
	}
	// 거절했으면 계정도 만들어지지 않아야 한다. 권한을 먼저 읽는 이유가 그것이다.
	_, list := alice.do("GET", "/api/v1/users/", nil)
	for _, u := range asList(list["users"]) {
		if u["username"] == "dev1" {
			t.Error("거절했는데 계정이 남았습니다")
		}
	}
}

// 슈퍼 어드민만 여러 명을 만들 수 있다(단건 생성과 같은 관문).
func TestBulkCreateUsersNeedsSuperadmin(t *testing.T) {
	e := newTestEnv(t)
	mkUserRole(t, e, "carol", model.RoleAdmin)
	carol := loginAs(t, e, "carol")

	if code, _ := carol.do("POST", "/api/v1/users/bulk",
		map[string]any{"text": "dev1\n", "role": "member"}); code != 403 {
		t.Errorf("어드민 = %d, 403이어야 합니다", code)
	}
}
