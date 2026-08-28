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

// 승인·반려는 리뷰어로 지정된 사람만 남길 수 있다.
//
// 의견은 누구나 남길 수 있어야 한다 — 지나가다 본 사람의 지적까지 막으면 그 말은
// 앱 밖으로 나가고 계획 옆에 남지 않는다.
func TestOnlyDesignatedReviewersDecide(t *testing.T) {
	e, conn, mig := assignEnv(t)
	dana := member(t, e, "dana", conn.ID, model.LevelMigrate)
	member(t, e, "bob", conn.ID, model.LevelMigrate)

	alice := loginAs(t, e, "alice")
	// 담당자는 리뷰어가 될 수 없으므로 리뷰어만 정한다(이 시험의 주제는 지정 여부다).
	status, body := alice.do("PUT", "/api/v1/migrations/"+mig.ID+"/assignment",
		map[string]any{"assigneeId": "", "reviewerIds": []string{dana.ID}})
	if status != 200 {
		t.Fatalf("지정 = %d: %v", status, body)
	}
	// 리뷰어를 지정했으니 리뷰 중이어야 한다.
	got, err := e.st.GetMigration(context.Background(), mig.ID, false)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != store.MigrationInReview {
		t.Fatalf("상태 = %q, 기대 in_review", got.Status)
	}

	// 지정되지 않은 사람: 승인·반려는 막히고 의견은 남는다.
	bobC := loginAs(t, e, "bob")
	for _, decision := range []string{"approved", "rejected"} {
		status, body = bobC.do("POST", "/api/v1/migrations/"+mig.ID+"/review",
			map[string]any{"decision": decision, "comment": "그냥"})
		if status != 403 {
			t.Errorf("지정되지 않은 사람의 %s = %d, 기대 403 (%v)", decision, status, body)
		}
	}
	status, body = bobC.do("POST", "/api/v1/migrations/"+mig.ID+"/review",
		map[string]any{"decision": "comment", "comment": "이 인덱스는 새벽에 거는 게 좋겠습니다"})
	if status != 200 {
		t.Fatalf("의견 = %d: %v", status, body)
	}
	got, err = e.st.GetMigration(context.Background(), mig.ID, false)
	if err != nil {
		t.Fatalf("get 2: %v", err)
	}
	if got.Status != store.MigrationInReview {
		t.Errorf("의견 뒤 상태 = %q, 기대 in_review (의견은 상태를 바꾸지 않는다)", got.Status)
	}
	if store.ApprovalCount(got.Reviews) != 0 {
		t.Errorf("의견이 승인으로 세어졌다: %d", store.ApprovalCount(got.Reviews))
	}

	// 지정된 사람의 승인은 통과한다.
	danaC := loginAs(t, e, "dana")
	status, body = danaC.do("POST", "/api/v1/migrations/"+mig.ID+"/review",
		map[string]any{"decision": "approved", "comment": "확인했습니다"})
	if status != 200 {
		t.Fatalf("리뷰어 승인 = %d: %v", status, body)
	}
	got, err = e.st.GetMigration(context.Background(), mig.ID, false)
	if err != nil {
		t.Fatalf("get 3: %v", err)
	}
	if got.Status != store.MigrationApproved {
		t.Errorf("승인 뒤 상태 = %q, 기대 approved", got.Status)
	}
}

// 담당자는 리뷰어가 될 수 없다.
//
// 자기가 끌고 가는 계획을 자기가 검토하는 것은 검토가 아니다. 승인 수만으로는 막지
// 못한다 — 한 명만 필요한 계획에서는 담당자가 스스로 승인하고 끝낼 수 있다.
func TestAssigneeCannotBeReviewer(t *testing.T) {
	e, conn, mig := assignEnv(t)
	dana := member(t, e, "dana", conn.ID, model.LevelMigrate)
	erin := member(t, e, "erin", conn.ID, model.LevelMigrate)
	alice := loginAs(t, e, "alice")

	status, body := alice.do("PUT", "/api/v1/migrations/"+mig.ID+"/assignment",
		map[string]any{"assigneeId": dana.ID, "reviewerIds": []string{dana.ID}})
	if status != 400 {
		t.Fatalf("자기 검토 지정 = %d: %v", status, body)
	}
	if body["error"] != "self_review" {
		t.Errorf("사유 = %v", body["error"])
	}
	// 거절된 지정은 아무것도 저장하지 않아야 한다. 절반만 남으면 화면과 저장이
	// 어긋난 채로 남는다.
	got, err := e.st.GetMigration(context.Background(), mig.ID, false)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.AssigneeID != "" || len(got.Reviewers) != 0 {
		t.Errorf("거절된 지정이 저장되었습니다: assignee=%q reviewers=%d",
			got.AssigneeID, len(got.Reviewers))
	}

	// 다른 사람을 리뷰어로 두면 통과한다.
	if status, body := alice.do("PUT", "/api/v1/migrations/"+mig.ID+"/assignment",
		map[string]any{"assigneeId": dana.ID, "reviewerIds": []string{erin.ID}}); status != 200 {
		t.Fatalf("정상 지정 = %d: %v", status, body)
	}
}

// 슈퍼 어드민은 리뷰어로 지정되지 않아도 승인·반려할 수 있다.
//
// 지정한 리뷰어가 자리를 비운 사이 계획이 영원히 멈춰 있으면, 사람들은 이 흐름을
// 우회하는 다른 길(콘솔에서 직접 실행)을 찾는다. 막다른 길을 만들지 않는 것이
// 규칙을 지키게 하는 방법이다.
func TestSuperadminReviewsWithoutDesignation(t *testing.T) {
	e, conn, mig := assignEnv(t)
	dana := member(t, e, "dana", conn.ID, model.LevelMigrate)
	erin := member(t, e, "erin", conn.ID, model.LevelMigrate)

	// alice(슈퍼 어드민)가 dana 를 리뷰어로 지정한다. alice 는 리뷰어가 아니다.
	alice := loginAs(t, e, "alice")
	if status, body := alice.do("PUT", "/api/v1/migrations/"+mig.ID+"/assignment",
		map[string]any{"assigneeId": erin.ID, "reviewerIds": []string{dana.ID}}); status != 200 {
		t.Fatalf("지정 = %d: %v", status, body)
	}

	// 지정되지 않은 일반 사용자는 여전히 막힌다.
	erinC := loginAs(t, e, "erin")
	if status, _ := erinC.do("POST", "/api/v1/migrations/"+mig.ID+"/review",
		map[string]any{"decision": "approved"}); status != 403 {
		t.Errorf("지정되지 않은 사용자의 승인 = %d, 403이어야 합니다", status)
	}

	// 슈퍼 어드민은 통과한다.
	status, body := alice.do("POST", "/api/v1/migrations/"+mig.ID+"/review",
		map[string]any{"decision": "approved", "comment": "리뷰어가 자리를 비워 대신 봅니다"})
	if status != 200 {
		t.Fatalf("슈퍼 어드민 승인 = %d: %v", status, body)
	}
	got, err := e.st.GetMigration(context.Background(), mig.ID, false)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != store.MigrationApproved {
		t.Errorf("상태 = %q, 기대 approved", got.Status)
	}
	// 누가 결정했는지는 남아야 한다.
	if len(got.Reviews) != 1 || got.Reviews[0].ReviewerID != aliceID(t, e) {
		t.Errorf("리뷰 기록 = %+v", got.Reviews)
	}
}

// 슈퍼 어드민은 자기가 맡은 계획도 승인할 수 있다.
//
// 계획을 만든 사람이 곧 담당자가 되므로(기본값), 슈퍼 어드민이 만든 계획은 예외가
// 없으면 아무도 승인하지 못하는 상태가 된다 — 혼자 쓰는 설치나 운영자가 한 명뿐인
// 팀에서 그것은 막다른 길이다.
func TestSuperadminApprovesOwnAssignment(t *testing.T) {
	e, conn, mig := assignEnv(t)
	dana := member(t, e, "dana", conn.ID, model.LevelMigrate)
	alice := loginAs(t, e, "alice")
	me := aliceID(t, e)

	// alice 가 담당자다(계획을 만든 사람이 기본 담당자다). 리뷰어는 dana —
	// 담당자는 리뷰어가 될 수 없으므로 다른 사람이어야 하고, 이 지정으로 리뷰가 열린다.
	if status, body := alice.do("PUT", "/api/v1/migrations/"+mig.ID+"/assignment",
		map[string]any{"assigneeId": me, "reviewerIds": []string{dana.ID}}); status != 200 {
		t.Fatalf("지정 = %d: %v", status, body)
	}

	status, body := alice.do("POST", "/api/v1/migrations/"+mig.ID+"/review",
		map[string]any{"decision": "approved", "comment": "직접 확인했습니다"})
	if status != 200 {
		t.Fatalf("담당자인 슈퍼 어드민의 승인 = %d: %v", status, body)
	}
	got, err := e.st.GetMigration(context.Background(), mig.ID, false)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != store.MigrationApproved {
		t.Errorf("상태 = %q, 기대 approved", got.Status)
	}
}

// 담당자는 자기가 맡은 계획을 승인할 수 없다. 기준은 작성자가 아니라 담당자다.
//
// 지정 규칙(담당자 ≠ 리뷰어)이 생기기 전에 저장된 자료에서는 담당자가 리뷰어로 남아
// 있을 수 있다. 그때도 자기 승인이 되어서는 안 된다.
func TestAssigneeCannotApprove(t *testing.T) {
	e, conn, mig := assignEnv(t)
	ctx := context.Background()
	dana := member(t, e, "dana", conn.ID, model.LevelMigrate)

	// 예전 자료를 흉내 낸다: 저장소에 직접 담당자 = 리뷰어로 넣는다.
	if err := e.st.SetMigrationAssignment(ctx, mig.ID, dana.ID, []string{dana.ID},
		aliceID(t, e)); err != nil {
		t.Fatalf("지정: %v", err)
	}
	if err := e.st.SetMigrationStatus(ctx, mig.ID, store.MigrationInReview); err != nil {
		t.Fatalf("상태: %v", err)
	}

	danaC := loginAs(t, e, "dana")
	status, body := danaC.do("POST", "/api/v1/migrations/"+mig.ID+"/review",
		map[string]any{"decision": "approved"})
	if status != 403 {
		t.Fatalf("담당자의 자기 승인 = %d: %v", status, body)
	}
	if body["error"] != "self_approval" {
		t.Errorf("사유 = %v", body["error"])
	}

	// 의견은 남길 수 있다. 담당자가 자기 계획에 설명을 다는 것까지 막을 이유는 없다.
	if status, body := danaC.do("POST", "/api/v1/migrations/"+mig.ID+"/review",
		map[string]any{"decision": "comment", "comment": "인덱스는 새벽에 겁니다"}); status != 200 {
		t.Fatalf("담당자의 의견 = %d: %v", status, body)
	}
	// 반려는 막지 않는다. 자기가 맡은 계획을 접는 것은 스스로 통과시키는 것과 다르다.
	if status, body := danaC.do("POST", "/api/v1/migrations/"+mig.ID+"/review",
		map[string]any{"decision": "rejected", "comment": "다시 만들겠습니다"}); status != 200 {
		t.Fatalf("담당자의 반려 = %d: %v", status, body)
	}
}

// 리뷰어는 마음을 바꿀 수 있다: 반려 → 승인 → 다시 반려.
//
// 되돌리는 길이 "계획을 초안으로 되돌려 리뷰 기록을 통째로 지우기"뿐이면 다른 사람의
// 승인까지 사라진다. 그러면 사람들은 반려를 누르기를 망설이고, 반려는 쓰이지 않는
// 버튼이 된다. 상태는 남아 있는 결정에서 다시 계산되므로 어느 방향이든 따라온다.
func TestReviewerChangesDecision(t *testing.T) {
	e, conn, mig := assignEnv(t)
	ctx := context.Background()
	dana := member(t, e, "dana", conn.ID, model.LevelMigrate)

	alice := loginAs(t, e, "alice")
	if status, body := alice.do("PUT", "/api/v1/migrations/"+mig.ID+"/assignment",
		map[string]any{"assigneeId": "", "reviewerIds": []string{dana.ID}}); status != 200 {
		t.Fatalf("지정 = %d: %v", status, body)
	}

	danaC := loginAs(t, e, "dana")
	statusOf := func(what string) string {
		t.Helper()
		got, err := e.st.GetMigration(ctx, mig.ID, false)
		if err != nil {
			t.Fatalf("get(%s): %v", what, err)
		}
		return got.Status
	}

	// 반려한다.
	if code, body := danaC.do("POST", "/api/v1/migrations/"+mig.ID+"/review",
		map[string]any{"decision": "rejected", "comment": "인덱스가 빠졌습니다"}); code != 200 {
		t.Fatalf("반려 = %d: %v", code, body)
	}
	if got := statusOf("반려"); got != store.MigrationRejected {
		t.Fatalf("반려 뒤 상태 = %q", got)
	}

	// 마음을 바꾼다. 반려된 상태에서도 결정을 남길 수 있어야 한다.
	if code, body := danaC.do("POST", "/api/v1/migrations/"+mig.ID+"/review",
		map[string]any{"decision": "approved", "comment": "설명 듣고 확인했습니다"}); code != 200 {
		t.Fatalf("반려 뒤 승인 = %d: %v", code, body)
	}
	if got := statusOf("승인"); got != store.MigrationApproved {
		t.Errorf("승인으로 바꾼 뒤 상태 = %q, 기대 approved", got)
	}

	// 다시 문제를 발견하면 승인된 계획도 막을 수 있다.
	if code, body := danaC.do("POST", "/api/v1/migrations/"+mig.ID+"/review",
		map[string]any{"decision": "rejected", "comment": "역시 안 되겠습니다"}); code != 200 {
		t.Fatalf("승인 뒤 반려 = %d: %v", code, body)
	}
	if got := statusOf("재반려"); got != store.MigrationRejected {
		t.Errorf("다시 반려한 뒤 상태 = %q, 기대 rejected", got)
	}

	// 한 사람의 결정은 하나로 남는다(저장소가 이전 결정을 대신한다). 승인 수를 셀 때
	// "같은 사람이 두 번 승인"이 두 표가 되어서는 안 되기 때문이다. 무엇을 언제
	// 결정했는지는 감사 로그에 그대로 쌓인다.
	got, err := e.st.GetMigration(ctx, mig.ID, true)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.Reviews) != 1 {
		t.Errorf("리뷰 기록 = %d건, 기대 1건(마지막 결정)", len(got.Reviews))
	}
	if len(got.Reviews) == 1 && got.Reviews[0].Decision != store.ReviewRejected {
		t.Errorf("남은 결정 = %q, 기대 rejected", got.Reviews[0].Decision)
	}

	// 실행된 계획의 결정은 바꿀 수 없다.
	if err := e.st.SetMigrationStatus(ctx, mig.ID, store.MigrationDraft); err != nil {
		t.Fatalf("draft: %v", err)
	}
	if err := e.st.SetMigrationStatus(ctx, mig.ID, store.MigrationInReview); err != nil {
		t.Fatalf("in_review: %v", err)
	}
	if err := e.st.SetMigrationStatus(ctx, mig.ID, store.MigrationApproved); err != nil {
		t.Fatalf("approved: %v", err)
	}
	if err := e.st.SetMigrationStatus(ctx, mig.ID, store.MigrationApplied); err != nil {
		t.Fatalf("applied: %v", err)
	}
	if code, _ := danaC.do("POST", "/api/v1/migrations/"+mig.ID+"/review",
		map[string]any{"decision": "rejected"}); code != 409 {
		t.Errorf("적용된 계획의 반려 = %d, 409여야 합니다", code)
	}
}

// aliceID는 시험 환경의 슈퍼 어드민 id다.
func aliceID(t *testing.T, e *testEnv) string {
	t.Helper()
	u, err := e.st.GetUserByUsername(context.Background(), "alice")
	if err != nil {
		t.Fatalf("alice 조회: %v", err)
	}
	return u.ID
}

// 반려된 계획은 리뷰어를 다시 지정하면 다시 리뷰로 들어가고, 그때 지난 결정은 지워진다.
//
// 지우지 않으면 반려가 남아 있어, 새 검토가 들어오는 즉시 다시 반려로 돌아간다.
func TestReassignReopensRejectedReview(t *testing.T) {
	e, conn, mig := assignEnv(t)
	dana := member(t, e, "dana", conn.ID, model.LevelMigrate)

	alice := loginAs(t, e, "alice")
	// 담당자는 리뷰어가 될 수 없으므로 여기서는 리뷰어만 정한다.
	if status, body := alice.do("PUT", "/api/v1/migrations/"+mig.ID+"/assignment",
		map[string]any{"assigneeId": "", "reviewerIds": []string{dana.ID}}); status != 200 {
		t.Fatalf("지정 = %d: %v", status, body)
	}

	danaC := loginAs(t, e, "dana")
	if status, body := danaC.do("POST", "/api/v1/migrations/"+mig.ID+"/review",
		map[string]any{"decision": "rejected", "comment": "인덱스가 빠졌습니다"}); status != 200 {
		t.Fatalf("반려 = %d: %v", status, body)
	}
	got, err := e.st.GetMigration(context.Background(), mig.ID, false)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != store.MigrationRejected {
		t.Fatalf("상태 = %q, 기대 rejected", got.Status)
	}

	// 다시 부탁한다.
	if status, body := alice.do("PUT", "/api/v1/migrations/"+mig.ID+"/assignment",
		map[string]any{"assigneeId": "", "reviewerIds": []string{dana.ID}}); status != 200 {
		t.Fatalf("재지정 = %d: %v", status, body)
	}
	got, err = e.st.GetMigration(context.Background(), mig.ID, false)
	if err != nil {
		t.Fatalf("get 2: %v", err)
	}
	if got.Status != store.MigrationInReview {
		t.Errorf("재지정 뒤 상태 = %q, 기대 in_review", got.Status)
	}
	if len(got.Reviews) != 0 {
		t.Errorf("지난 반려가 남았다: %d건 (남으면 새 검토가 들어오자마자 다시 반려된다)", len(got.Reviews))
	}
	if len(got.Reviewers) != 1 {
		t.Errorf("리뷰어 지정 = %d명, 기대 1명", len(got.Reviewers))
	}
}
