package api

import (
	"context"
	"testing"

	"dbstudio/internal/crypto"
	"dbstudio/internal/model"
	"dbstudio/internal/schema"
	"dbstudio/internal/store"
)

// 담당자·리뷰어 지정을 HTTP로 확인한다.
//
// 저장 계층 시험이 아니라 여기서 봐야 하는 것이 있다: 거절이 **실제로 저장을
// 막는가**. 이 파일의 fail()은 응답을 쓰고 nil을 반환하므로, 거절 경로를 그대로
// 넘겨받아 err != nil 로 검사하면 400을 쓰고도 저장이 이어진다. 실제로 그렇게
// 새어 나갔고, 저장 계층 시험에는 잡히지 않았다.

func assignEnv(t *testing.T) (*testEnv, *model.Connection, *store.Migration) {
	t.Helper()
	ctx := context.Background()
	e := newTestEnv(t)

	pw := "pw"
	srv, err := e.st.CreateServer(ctx, store.SaveServerParams{
		Name: "pg", Kind: model.KindPostgres, Host: "127.0.0.1", Port: 5432,
		DefaultEnvironment: model.EnvDev, Enabled: true, Username: "app", Password: &pw,
	})
	if err != nil {
		t.Fatalf("create server: %v", err)
	}
	conn, err := e.st.CreateConnection(ctx, store.SaveConnectionParams{
		ServerID: srv.ID, Name: "pg / appdb", Environment: model.EnvDev,
		DatabaseName: "appdb", Enabled: true,
	})
	if err != nil {
		t.Fatalf("create connection: %v", err)
	}

	before := &schema.Schema{Dialect: "postgres", Shape: schema.ShapeRelational}
	after := &schema.Schema{Dialect: "postgres", Shape: schema.ShapeRelational, Tables: []*schema.Table{{
		Name:    "orders",
		Columns: []*schema.Column{{Name: "id", Type: schema.LogicalType{Base: schema.TypeInt}}},
	}}}
	diff := schema.Diff(before, after)
	mig, err := e.st.CreateMigration(ctx, store.CreateMigrationParams{
		ConnectionID: conn.ID, Title: "주문 표 추가",
		BaseFinger: before.Fingerprint(), TargetSchema: after,
		Plan: schema.BuildPlan(string(model.KindPostgres), diff), Diff: diff,
	})
	if err != nil {
		t.Fatalf("create migration: %v", err)
	}
	return e, conn, mig
}

// member는 이 DB에 대한 등급을 가진 일반 사용자를 만든다.
// level이 비어 있으면 접근 권한이 없는 사람이다.
func member(t *testing.T, e *testEnv, name string, connID string, level model.Level) *model.User {
	t.Helper()
	ctx := context.Background()
	hash, err := crypto.HashPassword(testPassword)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	u, err := e.st.CreateUser(ctx, store.CreateUserParams{
		Username: name, DisplayName: name, Role: model.RoleMember, PasswordHash: hash,
	})
	if err != nil {
		t.Fatalf("create user %s: %v", name, err)
	}
	if level == "" {
		return u
	}
	p, err := e.st.GetAccessPolicy(ctx, u.ID)
	if err != nil {
		t.Fatalf("policy: %v", err)
	}
	p.Mode = model.AccessAllowlist
	p.DefaultLevel = model.LevelNone
	p.Items = []string{connID}
	p.Capabilities = map[string]model.Level{connID: level}
	if err := e.st.SetAccessPolicy(ctx, p); err != nil {
		t.Fatalf("set policy: %v", err)
	}
	return u
}

func TestAssignMigrationPeople(t *testing.T) {
	e, conn, mig := assignEnv(t)
	dana := member(t, e, "dana", conn.ID, model.LevelMigrate)
	eli := member(t, e, "eli", conn.ID, "")

	alice := loginAs(t, e, "alice")

	// 후보 목록에는 migrate 등급이 있는 사람만 나온다.
	status, body := alice.do("GET", "/api/v1/migrations/"+mig.ID+"/people", nil)
	if status != 200 {
		t.Fatalf("people = %d: %v", status, body)
	}
	names := map[string]bool{}
	for _, raw := range body["items"].([]any) {
		item := raw.(map[string]any)
		names[item["username"].(string)] = true
	}
	if !names["dana"] || !names["alice"] {
		t.Errorf("후보에 dana·alice 가 있어야 합니다: %v", names)
	}
	if names["eli"] {
		t.Errorf("접근 권한이 없는 eli 가 후보에 있습니다: %v", names)
	}

	// 정상 지정.
	status, body = alice.do("PUT", "/api/v1/migrations/"+mig.ID+"/assignment",
		map[string]any{"assigneeId": dana.ID, "reviewerIds": []string{e.user.ID}})
	if status != 200 {
		t.Fatalf("지정 = %d: %v", status, body)
	}

	// 권한 없는 사람을 지정하면 거절되고, **저장된 값이 바뀌지 않아야** 한다.
	status, _ = alice.do("PUT", "/api/v1/migrations/"+mig.ID+"/assignment",
		map[string]any{"assigneeId": eli.ID, "reviewerIds": []string{}})
	if status != 400 {
		t.Errorf("권한 없는 담당자 지정 = %d, 기대 400", status)
	}
	got, err := e.st.GetMigration(context.Background(), mig.ID, false)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.AssigneeID != dana.ID {
		t.Errorf("거절 뒤 담당자 = %q, 기대 %q (거절이 저장을 막지 못했습니다)", got.AssigneeID, dana.ID)
	}
	if len(got.Reviewers) != 1 {
		t.Errorf("거절 뒤 리뷰어 %d명, 기대 1명 (거절이 저장을 막지 못했습니다)", len(got.Reviewers))
	}

	// 리뷰어 쪽도 같다.
	status, _ = alice.do("PUT", "/api/v1/migrations/"+mig.ID+"/assignment",
		map[string]any{"assigneeId": dana.ID, "reviewerIds": []string{eli.ID}})
	if status != 400 {
		t.Errorf("권한 없는 리뷰어 지정 = %d, 기대 400", status)
	}

	// 없는 사용자는 400이어야 한다. 그대로 진행하면 외래키에서 500으로 터진다.
	status, _ = alice.do("PUT", "/api/v1/migrations/"+mig.ID+"/assignment",
		map[string]any{"assigneeId": "no-such-user", "reviewerIds": []string{}})
	if status != 400 {
		t.Errorf("없는 사용자 지정 = %d, 기대 400", status)
	}

	got, err = e.st.GetMigration(context.Background(), mig.ID, false)
	if err != nil {
		t.Fatalf("get 2: %v", err)
	}
	if got.AssigneeID != dana.ID || len(got.Reviewers) != 1 {
		t.Errorf("거절이 이어진 뒤 지정이 바뀌었습니다: 담당 %q, 리뷰어 %d명",
			got.AssigneeID, len(got.Reviewers))
	}
}

// 접근 권한이 없는 사람은 지정 자체를 할 수 없어야 한다.
func TestAssignRequiresMigrateLevel(t *testing.T) {
	e, conn, mig := assignEnv(t)
	member(t, e, "eli", conn.ID, "")
	member(t, e, "dana", conn.ID, model.LevelMigrate)

	eli := loginAs(t, e, "eli")
	status, _ := eli.do("PUT", "/api/v1/migrations/"+mig.ID+"/assignment",
		map[string]any{"assigneeId": "", "reviewerIds": []string{}})
	if status != 403 && status != 404 {
		t.Errorf("권한 없는 사용자의 지정 = %d, 기대 403 또는 404", status)
	}
}
