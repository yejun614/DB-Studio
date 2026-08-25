package store

import (
	"context"
	"path/filepath"
	"testing"

	"dbstudio/internal/crypto"
	"dbstudio/internal/model"
	"dbstudio/internal/schema"
)

func assignFixture(t *testing.T) (context.Context, *Store) {
	t.Helper()
	ctx := context.Background()
	box, err := crypto.NewSecretBox(make([]byte, 32))
	if err != nil {
		t.Fatalf("secret box: %v", err)
	}
	st, err := Open(ctx, filepath.Join(t.TempDir(), "assign.db"), box)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return ctx, st
}

func mkMigration(t *testing.T, ctx context.Context, st *Store) *Migration {
	t.Helper()
	srv := mkServer(t, ctx, st, "pg")
	conn := addDB(t, ctx, st, srv, "appdb")

	before := &schema.Schema{}
	after := &schema.Schema{Tables: []*schema.Table{{
		Name:    "orders",
		Columns: []*schema.Column{{Name: "id", Type: schema.LogicalType{Base: schema.TypeInt}}},
	}}}
	diff := schema.Diff(before, after)
	plan := schema.BuildPlan(string(model.KindPostgres), diff)

	mig, err := st.CreateMigration(ctx, CreateMigrationParams{
		ConnectionID: conn.ID, Title: "주문 표 추가",
		BaseFinger: before.Fingerprint(), TargetSchema: after,
		Plan: plan, Diff: diff,
	})
	if err != nil {
		t.Fatalf("create migration: %v", err)
	}
	return mig
}

// 지정은 현재 상태다: 다시 지정하면 뺀 사람은 사라져야 한다.
//
// 합집합으로 쌓이면 리뷰어를 교체할 방법이 없고, 뺀 사람은 자기 이름이 왜 아직
// 붙어 있는지 알 수 없다.
func TestSetMigrationAssignmentReplacesReviewers(t *testing.T) {
	ctx, st := assignFixture(t)
	mig := mkMigration(t, ctx, st)
	owner := mkUser(t, ctx, st, "owner")
	a := mkUser(t, ctx, st, "reviewer-a")
	b := mkUser(t, ctx, st, "reviewer-b")

	if err := st.SetMigrationAssignment(ctx, mig.ID, owner.ID, []string{a.ID, b.ID}, owner.ID); err != nil {
		t.Fatalf("첫 지정: %v", err)
	}
	got, err := st.GetMigration(ctx, mig.ID, false)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.AssigneeID != owner.ID {
		t.Errorf("담당자 = %q, 기대 %q", got.AssigneeID, owner.ID)
	}
	if got.AssigneeName != "owner" {
		t.Errorf("담당자 이름 = %q, 기대 %q", got.AssigneeName, "owner")
	}
	if len(got.Reviewers) != 2 {
		t.Fatalf("리뷰어 %d명, 기대 2명", len(got.Reviewers))
	}

	// b를 빼고 a만 남긴다.
	if err := st.SetMigrationAssignment(ctx, mig.ID, owner.ID, []string{a.ID}, owner.ID); err != nil {
		t.Fatalf("재지정: %v", err)
	}
	got, err = st.GetMigration(ctx, mig.ID, false)
	if err != nil {
		t.Fatalf("get 2: %v", err)
	}
	if len(got.Reviewers) != 1 || got.Reviewers[0].UserID != a.ID {
		t.Fatalf("재지정 뒤 리뷰어 = %+v, 기대 [a]", got.Reviewers)
	}
}

// 리뷰어 지정을 바꿔도 이미 남긴 리뷰 결정은 남아야 한다.
//
// 지정은 "봐 달라"는 요청이고 결정은 "봤다"는 기록이다. 요청을 고쳤다고 승인이
// 사라지면 승인 수 게이트가 조용히 풀린다 — 실행을 막던 조건이 없어진 것을
// 아무도 눈치채지 못한다.
func TestReassignKeepsExistingReviews(t *testing.T) {
	ctx, st := assignFixture(t)
	mig := mkMigration(t, ctx, st)
	a := mkUser(t, ctx, st, "reviewer-a")
	b := mkUser(t, ctx, st, "reviewer-b")

	if err := st.SetMigrationAssignment(ctx, mig.ID, "", []string{a.ID}, a.ID); err != nil {
		t.Fatalf("지정: %v", err)
	}
	if err := st.AddMigrationReview(ctx, &MigrationReview{
		MigrationID: mig.ID, ReviewerID: a.ID, ReviewerName: "reviewer-a",
		Decision: ReviewApproved,
	}); err != nil {
		t.Fatalf("리뷰: %v", err)
	}
	// a를 리뷰어 목록에서 빼고 b를 넣는다.
	if err := st.SetMigrationAssignment(ctx, mig.ID, "", []string{b.ID}, b.ID); err != nil {
		t.Fatalf("재지정: %v", err)
	}

	got, err := st.GetMigration(ctx, mig.ID, false)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if ApprovalCount(got.Reviews) != 1 {
		t.Errorf("승인 수 = %d, 기대 1 (지정을 바꿔도 결정은 남아야 한다)", ApprovalCount(got.Reviews))
	}
	if len(got.Reviewers) != 1 || got.Reviewers[0].UserID != b.ID {
		t.Errorf("리뷰어 = %+v, 기대 [b]", got.Reviewers)
	}
}

// 담당자를 비우는 것은 정상 동작이다("아직 정하지 않음"으로 되돌리기).
func TestClearAssignee(t *testing.T) {
	ctx, st := assignFixture(t)
	mig := mkMigration(t, ctx, st)
	owner := mkUser(t, ctx, st, "owner")

	if err := st.SetMigrationAssignment(ctx, mig.ID, owner.ID, nil, owner.ID); err != nil {
		t.Fatalf("지정: %v", err)
	}
	if err := st.SetMigrationAssignment(ctx, mig.ID, "", nil, owner.ID); err != nil {
		t.Fatalf("해제: %v", err)
	}
	got, err := st.GetMigration(ctx, mig.ID, false)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.AssigneeID != "" || got.AssigneeName != "" {
		t.Errorf("해제 뒤 담당자 = %q/%q, 기대 빈 값", got.AssigneeID, got.AssigneeName)
	}
}

// 사용자가 지워져도 마이그레이션은 남아야 한다(실행 이력이다).
// 담당자는 비워지고 리뷰어 지정은 함께 사라진다.
func TestDeletedUserDoesNotRemoveMigration(t *testing.T) {
	ctx, st := assignFixture(t)
	mig := mkMigration(t, ctx, st)
	owner := mkUser(t, ctx, st, "owner")
	rev := mkUser(t, ctx, st, "reviewer")

	if err := st.SetMigrationAssignment(ctx, mig.ID, owner.ID, []string{rev.ID}, owner.ID); err != nil {
		t.Fatalf("지정: %v", err)
	}
	if err := st.DeleteUser(ctx, owner.ID); err != nil {
		t.Fatalf("담당자 삭제: %v", err)
	}
	if err := st.DeleteUser(ctx, rev.ID); err != nil {
		t.Fatalf("리뷰어 삭제: %v", err)
	}

	got, err := st.GetMigration(ctx, mig.ID, false)
	if err != nil {
		t.Fatalf("삭제 뒤 get: %v", err)
	}
	if got.AssigneeID != "" {
		t.Errorf("담당자 = %q, 기대 빈 값", got.AssigneeID)
	}
	if len(got.Reviewers) != 0 {
		t.Errorf("리뷰어 = %+v, 기대 없음", got.Reviewers)
	}
}
