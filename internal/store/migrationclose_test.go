package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"dbstudio/internal/crypto"
	"dbstudio/internal/model"
	"dbstudio/internal/schema"
)

// 0031은 migrations 표를 새로 만들어 옮긴다(SQLite는 CHECK를 바꿀 수 없다).
//
// 옮기는 마이그레이션은 무손실이어야 하고, 자식 표(리뷰·리뷰어)가 CASCADE로 함께
// 지워지지 않아야 한다 — 그것이 -- +no-foreign-keys 를 붙인 이유다. 30번까지만 적용한
// DB를 만들고 거기에 데이터를 넣은 뒤 나머지를 적용한다. 이렇게 하지 않으면 "옛
// 상태에서 시작한다"는 조건을 만들 수 없고, 그러면 이 마이그레이션은 검증되지 않은 채
// 남의 운영 DB에서 처음 실행된다.
func TestMigrationCloseMigrationIsLossless(t *testing.T) {
	ctx := context.Background()
	box, err := crypto.NewSecretBox(make([]byte, 32))
	if err != nil {
		t.Fatalf("secret box: %v", err)
	}
	path := filepath.Join(t.TempDir(), "close.db")
	db, err := sql.Open("sqlite", strings.ReplaceAll(path, "\\", "/")+
		"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	st := &Store{db: db, secret: box}
	if err := st.migrateTo(ctx, 30); err != nil {
		t.Fatalf("migrate to 30: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	owner := mkUser(t, ctx, st, "maker")
	srv := mkServer(t, ctx, st, "pg")
	conn := addDB(t, ctx, st, srv, "appdb")
	mig := mkMigrationFor(t, ctx, st, conn.ID, owner.ID, "주문 표 추가")

	if err := st.AddMigrationReview(ctx, &MigrationReview{
		MigrationID: mig.ID, ReviewerID: owner.ID, ReviewerName: "maker",
		Decision: ReviewApproved, Comment: "좋습니다",
	}); err != nil {
		t.Fatalf("리뷰: %v", err)
	}
	if err := st.SetMigrationAssignment(ctx, mig.ID, "", []string{owner.ID}, owner.ID); err != nil {
		t.Fatalf("리뷰어 지정: %v", err)
	}
	if err := st.SetMigrationStatus(ctx, mig.ID, MigrationInReview); err != nil {
		t.Fatalf("리뷰 중: %v", err)
	}
	// 0030 이전에 만들어진 행처럼 담당자를 비운다. 0031의 백필이 이것을 채워야 한다.
	if _, err := db.ExecContext(ctx, `UPDATE migrations SET assignee_id = NULL`); err != nil {
		t.Fatalf("담당자 비우기: %v", err)
	}

	if err := st.migrate(ctx); err != nil {
		t.Fatalf("migrate rest: %v", err)
	}

	got, err := st.GetMigration(ctx, mig.ID, true)
	if err != nil {
		t.Fatalf("get after migrate: %v", err)
	}
	if got.Title != "주문 표 추가" || got.Status != MigrationInReview || got.UpSQL == "" {
		t.Errorf("내용이 달라졌다: title=%q status=%q upSQL=%q", got.Title, got.Status, got.UpSQL)
	}
	if got.ConnectionID != conn.ID || got.CreatedBy != owner.ID {
		t.Errorf("소속·작성자가 달라졌다: %+v", got)
	}
	// 담당자가 비어 있던 행은 만든 사람으로 채워져야 한다.
	if got.AssigneeID != owner.ID || got.AssigneeName != "maker" {
		t.Errorf("담당자 기본값이 채워지지 않았다: %q/%q", got.AssigneeID, got.AssigneeName)
	}
	// 자식 표가 CASCADE로 함께 지워지지 않아야 한다.
	if len(got.Reviews) != 1 {
		t.Errorf("리뷰 %d건, 기대 1건 (표를 옮기면서 사라졌다)", len(got.Reviews))
	}
	if len(got.Reviewers) != 1 {
		t.Errorf("리뷰어 %d명, 기대 1명 (표를 옮기면서 사라졌다)", len(got.Reviewers))
	}

	// 새 상태를 실제로 저장할 수 있어야 한다(CHECK 제약이 바뀌었는지).
	if err := st.SetMigrationStatus(ctx, mig.ID, MigrationClosed); err != nil {
		t.Fatalf("닫기: %v", err)
	}
	if got, err = st.GetMigration(ctx, mig.ID, false); err != nil {
		t.Fatalf("get: %v", err)
	} else if got.Status != MigrationClosed {
		t.Errorf("상태 = %q, 기대 closed", got.Status)
	}

	// 닫은 계획은 다시 열 수 있고, 그때 승인은 무효가 된다(지정은 남는다).
	if err := st.SetMigrationStatus(ctx, mig.ID, MigrationDraft); err != nil {
		t.Fatalf("다시 열기: %v", err)
	}
	got, err = st.GetMigration(ctx, mig.ID, false)
	if err != nil {
		t.Fatalf("get 2: %v", err)
	}
	if got.Status != MigrationDraft {
		t.Errorf("상태 = %q, 기대 draft", got.Status)
	}
	if len(got.Reviews) != 0 {
		t.Errorf("다시 연 뒤 리뷰 %d건, 기대 0건 (초안으로 돌아가면 승인은 무효다)", len(got.Reviews))
	}
	if len(got.Reviewers) != 1 {
		t.Errorf("다시 연 뒤 리뷰어 지정 %d명, 기대 1명 (지정은 결정과 다르다)", len(got.Reviewers))
	}
}

// 실행된 마이그레이션은 닫을 수 없다 — 이력이기 때문이다.
func TestAppliedMigrationCannotBeClosed(t *testing.T) {
	ctx, st := assignFixture(t)
	mig := mkMigration(t, ctx, st)

	for _, next := range []string{MigrationInReview, MigrationApproved, MigrationApplied} {
		if err := st.SetMigrationStatus(ctx, mig.ID, next); err != nil {
			t.Fatalf("%s: %v", next, err)
		}
	}
	err := st.SetMigrationStatus(ctx, mig.ID, MigrationClosed)
	var ite *InvalidTransitionError
	if !errors.As(err, &ite) {
		t.Fatalf("적용된 마이그레이션을 닫을 수 있었다: %v", err)
	}
}

// 새로 만든 마이그레이션의 담당자는 만든 사람이다.
func TestNewMigrationAssigneeIsCreator(t *testing.T) {
	ctx, st := assignFixture(t)
	owner := mkUser(t, ctx, st, "owner")
	srv := mkServer(t, ctx, st, "pg")
	conn := addDB(t, ctx, st, srv, "appdb")

	mig := mkMigrationFor(t, ctx, st, conn.ID, owner.ID, "담당자 기본값 확인")
	if mig.AssigneeID != owner.ID || mig.AssigneeName != "owner" {
		t.Errorf("담당자 = %q/%q, 기대 %q/owner", mig.AssigneeID, mig.AssigneeName, owner.ID)
	}
}

func mkMigrationFor(t *testing.T, ctx context.Context, st *Store, connID, createdBy, title string) *Migration {
	t.Helper()
	before := &schema.Schema{Dialect: "postgres", Shape: schema.ShapeRelational}
	after := &schema.Schema{Dialect: "postgres", Shape: schema.ShapeRelational, Tables: []*schema.Table{{
		Name:    "orders",
		Columns: []*schema.Column{{Name: "id", Type: schema.LogicalType{Base: schema.TypeInt}}},
	}}}
	diff := schema.Diff(before, after)
	mig, err := st.CreateMigration(ctx, CreateMigrationParams{
		ConnectionID: connID, Title: title,
		BaseFinger: before.Fingerprint(), TargetSchema: after,
		Plan: schema.BuildPlan(string(model.KindPostgres), diff), Diff: diff,
		CreatedBy: createdBy,
	})
	if err != nil {
		t.Fatalf("create migration: %v", err)
	}
	return mig
}
