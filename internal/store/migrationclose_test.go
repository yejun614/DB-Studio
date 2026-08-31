package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

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
	srvID := mkServerRow(t, ctx, st, "pg")
	connID := mkConnRow(t, ctx, st, srvID, "appdb")
	migID := mkMigrationRow(t, ctx, st, connID, owner.ID, "주문 표 추가")

	if err := st.AddMigrationReview(ctx, &MigrationReview{
		MigrationID: migID, ReviewerID: owner.ID, ReviewerName: "maker",
		Decision: ReviewApproved, Comment: "좋습니다",
	}); err != nil {
		t.Fatalf("리뷰: %v", err)
	}
	if err := st.SetMigrationAssignment(ctx, migID, "", []string{owner.ID}, owner.ID); err != nil {
		t.Fatalf("리뷰어 지정: %v", err)
	}
	if err := st.SetMigrationStatus(ctx, migID, MigrationInReview); err != nil {
		t.Fatalf("리뷰 중: %v", err)
	}
	// 0030 이전에 만들어진 행처럼 담당자를 비운다. 0031의 백필이 이것을 채워야 한다.
	if _, err := db.ExecContext(ctx, `UPDATE migrations SET assignee_id = NULL`); err != nil {
		t.Fatalf("담당자 비우기: %v", err)
	}

	if err := st.migrate(ctx); err != nil {
		t.Fatalf("migrate rest: %v", err)
	}

	got, err := st.GetMigration(ctx, migID, true)
	if err != nil {
		t.Fatalf("get after migrate: %v", err)
	}
	if got.Title != "주문 표 추가" || got.Status != MigrationInReview || got.UpSQL == "" {
		t.Errorf("내용이 달라졌다: title=%q status=%q upSQL=%q", got.Title, got.Status, got.UpSQL)
	}
	if got.ConnectionID != connID || got.CreatedBy != owner.ID {
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
	if err := st.SetMigrationStatus(ctx, migID, MigrationClosed); err != nil {
		t.Fatalf("닫기: %v", err)
	}
	if got, err = st.GetMigration(ctx, migID, false); err != nil {
		t.Fatalf("get: %v", err)
	} else if got.Status != MigrationClosed {
		t.Errorf("상태 = %q, 기대 closed", got.Status)
	}

	// 닫은 계획은 다시 열 수 있고, 그때 승인은 무효가 된다(지정은 남는다).
	if err := st.SetMigrationStatus(ctx, migID, MigrationDraft); err != nil {
		t.Fatalf("다시 열기: %v", err)
	}
	got, err = st.GetMigration(ctx, migID, false)
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

	// 0037은 connections 표도 새로 만들어 옮긴다. 옛 DB에서 시작한 이 시험이
	// 그것까지 지나왔으므로, 그 커넥션이 기본 프로젝트에 들어갔는지 여기서 본다.
	// 들어가지 못했다면 앱은 뜨지만 아무에게도 보이지 않는 DB가 된다.
	conn, err := st.GetConnection(ctx, connID)
	if err != nil {
		t.Fatalf("커넥션이 사라졌다: %v", err)
	}
	if conn.ProjectID != DefaultProjectID {
		t.Errorf("커넥션의 프로젝트 = %q, 기대 %q", conn.ProjectID, DefaultProjectID)
	}
	// 서버도 함께 옮겨져야 한다(0038). 서버가 갈 곳을 잃으면 그 아래 DB는
	// 목록에 뜨지 않는다.
	srv, err := st.GetServer(ctx, srvID)
	if err != nil {
		t.Fatalf("서버가 사라졌다: %v", err)
	}
	if srv.ProjectID != DefaultProjectID {
		t.Errorf("서버의 프로젝트 = %q, 기대 %q", srv.ProjectID, DefaultProjectID)
	}
	// 있던 사람은 모두 기본 프로젝트의 참여자가 되어야 한다.
	ok, err := st.IsProjectMember(ctx, DefaultProjectID, owner.ID)
	if err != nil {
		t.Fatalf("참여 확인: %v", err)
	}
	if !ok {
		t.Error("있던 사용자가 기본 프로젝트에 들어가지 않았다 — 앱을 올리는 순간 권한을 잃는다")
	}
}

// mkServerRow는 옛 스키마 위에 서버 행 하나를 넣는다(다시 읽지 않는다).
//
// mkServer를 쓸 수 없는 이유는 mkConnRow와 같다: 그것은 프로젝트를 먼저 만드는데,
// 여기서는 projects 표가 아직 없는 시점(30번)에서 시작한다.
func mkServerRow(t *testing.T, ctx context.Context, st *Store, name string) string {
	t.Helper()
	id := "srv_" + uuid.NewString()
	now := nowString()
	if _, err := st.db.ExecContext(ctx, `INSERT INTO servers
		(id, name, name_lower, kind, host, port, options, default_environment,
		 tags, note, enabled, created_by, created_at, updated_at)
		VALUES (?, ?, ?, 'postgres', '10.0.0.1', 5432, '{}', 'dev', '', '', 1, NULL, ?, ?)`,
		id, name, name, now, now); err != nil {
		t.Fatalf("서버 행: %v", err)
	}
	return id
}

// mkConnRow는 옛 스키마 위에 커넥션 행 하나를 넣는다(다시 읽지 않는다).
//
// addDB를 쓸 수 없는 이유는 mkMigrationRow와 같다: 그것은 프로젝트를 먼저 만드는데,
// 여기서는 projects 표가 아직 없는 시점(30번)에서 시작한다.
func mkConnRow(t *testing.T, ctx context.Context, st *Store, serverID, db string) string {
	t.Helper()
	id := uuid.NewString()
	now := nowString()
	if _, err := st.db.ExecContext(ctx, `INSERT INTO connections
		(id, server_id, name, name_lower, environment, database_name, tags, note,
		 enabled, created_by, created_at, updated_at, node_id)
		VALUES (?, ?, ?, ?, 'dev', ?, '', '', 1, NULL, ?, ?, '')`,
		id, serverID, db, db, db, now, now); err != nil {
		t.Fatalf("커넥션 행: %v", err)
	}
	return id
}

// 적용된 계획도 닫을 수 있고, 다시 열면 적용됨으로 돌아온다.
//
// 목록에 영원히 남으면 "지금 볼 것"과 "끝난 것"이 섞인다. 그렇다고 다시 열 때
// 초안으로 보내면, DB에 들어가 있는 변경이 "아직 실행하지 않은 초안"으로 보이고
// 롤백 버튼도 사라진다 — 되돌릴 방법이 화면에서 없어지는 것이다.
func TestAppliedMigrationClosesAndReopensAsApplied(t *testing.T) {
	ctx, st := assignFixture(t)
	mig := mkMigration(t, ctx, st)

	for _, next := range []string{MigrationInReview, MigrationApproved, MigrationApplied} {
		if err := st.SetMigrationStatus(ctx, mig.ID, next); err != nil {
			t.Fatalf("%s: %v", next, err)
		}
	}
	if err := st.SetMigrationStatus(ctx, mig.ID, MigrationClosed); err != nil {
		t.Fatalf("적용된 계획 닫기: %v", err)
	}
	closed, err := st.GetMigration(ctx, mig.ID, false)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if closed.ClosedFrom != MigrationApplied {
		t.Errorf("닫기 전 상태 = %q, 기대 applied", closed.ClosedFrom)
	}

	if err := st.SetMigrationStatus(ctx, mig.ID, MigrationApplied); err != nil {
		t.Fatalf("다시 열기: %v", err)
	}
	got, err := st.GetMigration(ctx, mig.ID, false)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != MigrationApplied {
		t.Errorf("다시 연 뒤 상태 = %q, 기대 applied", got.Status)
	}
	// 다시 열었으니 "닫기 전 상태"는 지워져야 한다. 남아 있으면 다음에 초안에서
	// 닫았을 때도 적용됨으로 돌아갈 수 있게 된다.
	if got.ClosedFrom != "" {
		t.Errorf("다시 연 뒤에도 닫기 전 상태가 남았습니다: %q", got.ClosedFrom)
	}
}

// 초안에서 닫은 계획은 적용됨으로 열 수 없다 — 실행 기록 없는 적용이 되기 때문이다.
func TestClosedDraftCannotReopenAsApplied(t *testing.T) {
	ctx, st := assignFixture(t)
	mig := mkMigration(t, ctx, st)

	if err := st.SetMigrationStatus(ctx, mig.ID, MigrationClosed); err != nil {
		t.Fatalf("닫기: %v", err)
	}
	err := st.SetMigrationStatus(ctx, mig.ID, MigrationApplied)
	var ite *InvalidTransitionError
	if !errors.As(err, &ite) {
		t.Fatalf("초안에서 닫은 계획을 적용됨으로 열 수 있었다: %v", err)
	}
	// 초안으로는 열린다.
	if err := st.SetMigrationStatus(ctx, mig.ID, MigrationDraft); err != nil {
		t.Fatalf("초안으로 열기: %v", err)
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

// mkMigrationRow는 옛 스키마 위에 계획 행 하나를 넣는다(다시 읽지 않는다).
//
// CreateMigration은 넣은 뒤 GetMigration으로 되읽는데, 그 SELECT는 언제나 **지금**
// 스키마를 가리킨다. 옛 상태(30번까지만 적용)를 만들어 놓고 시험하는 이 파일에서는
// 그것을 쓸 수 없다 — 나중에 컬럼이 하나 늘 때마다 이 시험이 엉뚱하게 깨진다.
func mkMigrationRow(t *testing.T, ctx context.Context, st *Store, connID, createdBy, title string) string {
	t.Helper()
	before := &schema.Schema{Dialect: "postgres", Shape: schema.ShapeRelational}
	after := &schema.Schema{Dialect: "postgres", Shape: schema.ShapeRelational, Tables: []*schema.Table{{
		Name:    "orders",
		Columns: []*schema.Column{{Name: "id", Type: schema.LogicalType{Base: schema.TypeInt}}},
	}}}
	diff := schema.Diff(before, after)
	plan := schema.BuildPlan(string(model.KindPostgres), diff)
	planJSON, _ := json.Marshal(plan)
	diffJSON, _ := json.Marshal(diff)
	targetJSON, _ := json.Marshal(after)

	id := uuid.NewString()
	now := nowString()
	if _, err := st.db.ExecContext(ctx, `INSERT INTO migrations
		(id, connection_id, title, base_fingerprint, target_schema_json,
		 up_sql, down_sql, plan_json, diff_json, destructive_count, irreversible,
		 status, created_by, assignee_id, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '[]', ?, ?, ?, ?, ?)`,
		id, connID, title, before.Fingerprint(), string(targetJSON),
		plan.UpSQL(), plan.DownSQL(), string(planJSON), string(diffJSON),
		diff.DestructiveCount, MigrationDraft, createdBy, createdBy, now, now); err != nil {
		t.Fatalf("insert migration: %v", err)
	}
	return id
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
