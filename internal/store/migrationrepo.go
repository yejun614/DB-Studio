package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"dbstudio/internal/schema"
)

// 마이그레이션 상태.
const (
	MigrationDraft      = "draft"
	MigrationInReview   = "in_review"
	MigrationApproved   = "approved"
	MigrationRejected   = "rejected"
	MigrationApplied    = "applied"
	MigrationRolledBack = "rolled_back"
	MigrationFailed     = "failed"
)

// 리뷰 결정.
const (
	ReviewApproved = "approved"
	ReviewRejected = "rejected"
	ReviewComment  = "comment"
)

// allowedTransitions는 상태 전이 규칙이다.
//
// 규칙을 표로 한곳에 모으는 이유: 전이 검사가 핸들러마다 흩어지면 새 경로를 추가할 때
// 한 곳을 빠뜨리게 되고, 그러면 "승인 없이 실행" 같은 구멍이 생긴다.
var allowedTransitions = map[string][]string{
	MigrationDraft:    {MigrationInReview},
	MigrationInReview: {MigrationApproved, MigrationRejected, MigrationDraft},
	// 승인 후에도 계획을 고칠 수 있어야 한다 — 다만 그때는 승인이 무효가 되므로
	// draft로 돌아간다(핸들러가 리뷰 기록을 함께 지운다).
	MigrationApproved: {MigrationApplied, MigrationFailed, MigrationDraft},
	MigrationRejected: {MigrationDraft},
	// 실패한 마이그레이션은 부분 적용 상태일 수 있다. 사람이 상태를 확인한 뒤
	// 다시 계획을 세우도록 draft로만 돌린다.
	MigrationFailed:  {MigrationDraft},
	MigrationApplied: {MigrationRolledBack},
	// 롤백된 마이그레이션은 이력이다. 다시 실행하려면 새 계획을 만든다 —
	// 롤백 후 같은 계획을 재실행하는 것은 기준 상태가 달라 안전하지 않다.
	MigrationRolledBack: {},
}

// CanTransition은 상태 전이가 허용되는지 판단한다.
func CanTransition(from, to string) bool {
	for _, allowed := range allowedTransitions[from] {
		if allowed == to {
			return true
		}
	}
	return false
}

// Migration은 하나의 마이그레이션 실행 단위다.
type Migration struct {
	ID           string `json:"id"`
	ConnectionID string `json:"connectionId"`
	DocID        string `json:"docId,omitempty"`
	Title        string `json:"title"`
	FromVersion  *int64 `json:"fromVersion,omitempty"`
	ToVersion    *int64 `json:"toVersion,omitempty"`
	// FromVersionNo/ToVersionNo는 화면 표시용 번호다 (조인 결과).
	FromVersionNo *int `json:"fromVersionNo,omitempty"`
	ToVersionNo   *int `json:"toVersionNo,omitempty"`
	// RollbackTo는 이 마이그레이션이 되돌아가려는 버전이다. 비어 있으면 일반 마이그레이션이다.
	// 채워져 있으면 적용 후 등록되는 버전의 source가 'rollback'이 된다.
	RollbackTo   *int64 `json:"rollbackTo,omitempty"`
	RollbackToNo *int   `json:"rollbackToNo,omitempty"`
	BaseFinger   string `json:"baseFingerprint"`
	UpSQL        string `json:"upSql"`
	DownSQL      string `json:"downSql"`

	Plan             *schema.Plan       `json:"plan,omitempty"`
	Diff             *schema.DiffResult `json:"diff,omitempty"`
	TargetSchema     *schema.Schema     `json:"targetSchema,omitempty"`
	DestructiveCount int                `json:"destructiveCount"`
	Irreversible     []string           `json:"irreversible"`

	Status            string          `json:"status"`
	AppliedStatements int             `json:"appliedStatements"`
	ExecutionLog      []ExecutionStep `json:"executionLog"`
	Error             string          `json:"error,omitempty"`

	CreatedBy    string     `json:"createdBy,omitempty"`
	CreatedAt    time.Time  `json:"createdAt"`
	UpdatedAt    time.Time  `json:"updatedAt"`
	AppliedAt    *time.Time `json:"appliedAt,omitempty"`
	AppliedBy    string     `json:"appliedBy,omitempty"`
	RolledBackAt *time.Time `json:"rolledBackAt,omitempty"`

	Reviews []*MigrationReview `json:"reviews,omitempty"`
}

// ExecutionStep은 문장 하나의 실행 결과다.
//
// 문장 단위로 남기는 이유: MySQL·Oracle은 DDL이 암묵적 커밋이라 트랜잭션으로 되돌릴 수
// 없다. 중간에 실패하면 "어디까지 적용됐는가"가 복구의 유일한 출발점이다.
type ExecutionStep struct {
	Index      int    `json:"index"`
	SQL        string `json:"sql"`
	DurationMs int64  `json:"durationMs"`
	Error      string `json:"error,omitempty"`
	// RowsAffected는 DDL에서는 대개 0이지만, 데이터 이동을 포함한 문장에서는 의미가 있다.
	RowsAffected int64 `json:"rowsAffected,omitempty"`
}

// MigrationReview는 리뷰 한 건이다.
type MigrationReview struct {
	ID           int64     `json:"id"`
	MigrationID  string    `json:"migrationId"`
	ReviewerID   string    `json:"reviewerId,omitempty"`
	ReviewerName string    `json:"reviewerName"`
	Decision     string    `json:"decision"`
	Comment      string    `json:"comment,omitempty"`
	CreatedAt    time.Time `json:"createdAt"`
}

// CreateMigrationParams는 마이그레이션 생성 입력이다.
type CreateMigrationParams struct {
	ConnectionID string
	DocID        string
	Title        string
	FromVersion  *int64
	// RollbackTo가 채워지면 이 마이그레이션은 그 버전으로 되돌리는 것이며,
	// 적용 후 등록되는 스키마 버전의 source가 'rollback'이 된다.
	RollbackTo   *int64
	BaseFinger   string
	TargetSchema *schema.Schema
	Plan         *schema.Plan
	Diff         *schema.DiffResult
	CreatedBy    string
}

func (s *Store) CreateMigration(ctx context.Context, p CreateMigrationParams) (*Migration, error) {
	if p.Plan == nil || p.Diff == nil || p.TargetSchema == nil {
		return nil, errors.New("계획·차이·목표 스키마가 모두 필요합니다")
	}
	planJSON, err := json.Marshal(p.Plan)
	if err != nil {
		return nil, fmt.Errorf("marshal plan: %w", err)
	}
	diffJSON, err := json.Marshal(p.Diff)
	if err != nil {
		return nil, fmt.Errorf("marshal diff: %w", err)
	}
	targetJSON, err := json.Marshal(p.TargetSchema)
	if err != nil {
		return nil, fmt.Errorf("marshal target schema: %w", err)
	}
	irreversible := p.Plan.Irreversible
	if irreversible == nil {
		irreversible = []string{}
	}
	irrJSON, _ := json.Marshal(irreversible)

	id := uuid.NewString()
	now := nowString()
	_, err = s.db.ExecContext(ctx, `INSERT INTO migrations
		(id, connection_id, doc_id, title, from_version, rollback_to_version, base_fingerprint,
		 target_schema_json, up_sql, down_sql, plan_json, diff_json,
		 destructive_count, irreversible, status, created_by, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, p.ConnectionID, nullString(p.DocID), p.Title, p.FromVersion, p.RollbackTo, p.BaseFinger,
		string(targetJSON), p.Plan.UpSQL(), p.Plan.DownSQL(), string(planJSON), string(diffJSON),
		p.Diff.DestructiveCount, string(irrJSON), MigrationDraft,
		nullString(p.CreatedBy), now, now)
	if err != nil {
		return nil, fmt.Errorf("insert migration: %w", err)
	}
	return s.GetMigration(ctx, id, true)
}

// GetMigration은 마이그레이션을 읽는다. withBody가 false면 SQL과 계획 본문을 뺀다.
func (s *Store) GetMigration(ctx context.Context, id string, withBody bool) (*Migration, error) {
	row := s.db.QueryRowContext(ctx, migrationSelect+` WHERE m.id = ?`, id)
	m, err := scanMigration(row, withBody)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	reviews, err := s.ListMigrationReviews(ctx, id)
	if err != nil {
		return nil, err
	}
	m.Reviews = reviews
	return m, nil
}

const migrationSelect = `SELECT
	m.id, m.connection_id, COALESCE(m.doc_id, ''), m.title,
	m.from_version, m.to_version, fv.version_no, tv.version_no,
	m.rollback_to_version, rv.version_no,
	m.base_fingerprint, m.up_sql, m.down_sql, m.plan_json, m.diff_json, m.target_schema_json,
	m.destructive_count, m.irreversible, m.status, m.applied_statements, m.execution_log,
	m.error, COALESCE(m.created_by, ''), m.created_at, m.updated_at,
	m.applied_at, COALESCE(m.applied_by, ''), m.rolled_back_at
	FROM migrations m
	LEFT JOIN schema_versions fv ON fv.id = m.from_version
	LEFT JOIN schema_versions tv ON tv.id = m.to_version
	LEFT JOIN schema_versions rv ON rv.id = m.rollback_to_version`

// ListMigrations는 마이그레이션 목록을 반환한다 (본문 제외).
//
// connectionIDs의 nil과 빈 슬라이스는 뜻이 다르다: nil은 제한 없음,
// 빈 슬라이스는 "접근 가능한 커넥션이 없음"이다. 이 구분을 놓치면 권한이 없는
// 사용자에게 전체 이력이 노출된다.
func (s *Store) ListMigrations(ctx context.Context, connectionIDs []string, status string, limit int) ([]*Migration, error) {
	if connectionIDs != nil && len(connectionIDs) == 0 {
		return []*Migration{}, nil
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	query := migrationSelect
	where := []string{}
	args := []any{}
	if connectionIDs != nil {
		marks := make([]string, len(connectionIDs))
		for i, id := range connectionIDs {
			marks[i] = "?"
			args = append(args, id)
		}
		where = append(where, "m.connection_id IN ("+strings.Join(marks, ",")+")")
	}
	if status != "" {
		where = append(where, "m.status = ?")
		args = append(args, status)
	}
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY m.created_at DESC LIMIT ?"
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list migrations: %w", err)
	}
	defer rows.Close()

	out := []*Migration{}
	for rows.Next() {
		m, err := scanMigration(rows, false)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate migrations: %w", err)
	}
	return out, nil
}

func scanMigration(row interface{ Scan(...any) error }, withBody bool) (*Migration, error) {
	var m Migration
	var fromVersion, toVersion sql.NullInt64
	var fromNo, toNo sql.NullInt64
	var rollbackTo, rollbackNo sql.NullInt64
	var planJSON, diffJSON, targetJSON, irrJSON, execJSON string
	var createdAt, updatedAt string
	var appliedAt, rolledBackAt sql.NullString

	if err := row.Scan(&m.ID, &m.ConnectionID, &m.DocID, &m.Title,
		&fromVersion, &toVersion, &fromNo, &toNo, &rollbackTo, &rollbackNo,
		&m.BaseFinger, &m.UpSQL, &m.DownSQL, &planJSON, &diffJSON, &targetJSON,
		&m.DestructiveCount, &irrJSON, &m.Status, &m.AppliedStatements, &execJSON,
		&m.Error, &m.CreatedBy, &createdAt, &updatedAt,
		&appliedAt, &m.AppliedBy, &rolledBackAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		return nil, fmt.Errorf("scan migration: %w", err)
	}

	if fromVersion.Valid {
		m.FromVersion = &fromVersion.Int64
	}
	if toVersion.Valid {
		m.ToVersion = &toVersion.Int64
	}
	if fromNo.Valid {
		n := int(fromNo.Int64)
		m.FromVersionNo = &n
	}
	if toNo.Valid {
		n := int(toNo.Int64)
		m.ToVersionNo = &n
	}
	if rollbackTo.Valid {
		m.RollbackTo = &rollbackTo.Int64
	}
	if rollbackNo.Valid {
		n := int(rollbackNo.Int64)
		m.RollbackToNo = &n
	}
	m.CreatedAt, m.UpdatedAt = parseTime(createdAt), parseTime(updatedAt)
	m.AppliedAt = parseTimePtr(appliedAt)
	m.RolledBackAt = parseTimePtr(rolledBackAt)

	m.Irreversible = []string{}
	_ = json.Unmarshal([]byte(irrJSON), &m.Irreversible)
	m.ExecutionLog = []ExecutionStep{}
	_ = json.Unmarshal([]byte(execJSON), &m.ExecutionLog)

	// diff는 목록에서도 변경 건수를 보여주므로 항상 디코딩한다.
	var diff schema.DiffResult
	if err := json.Unmarshal([]byte(diffJSON), &diff); err == nil {
		m.Diff = &diff
	}

	if !withBody {
		m.UpSQL, m.DownSQL = "", ""
		return &m, nil
	}
	var plan schema.Plan
	if err := json.Unmarshal([]byte(planJSON), &plan); err == nil {
		m.Plan = &plan
	}
	var target schema.Schema
	if err := json.Unmarshal([]byte(targetJSON), &target); err == nil {
		m.TargetSchema = &target
	}
	return &m, nil
}

// SetMigrationStatus는 상태를 전이 규칙에 따라 바꾼다.
//
// 규칙 위반은 ErrInvalidTransition으로 구분해 반환한다. 호출자가 409로 응답해
// "왜 안 되는가"를 사용자에게 설명할 수 있어야 한다.
func (s *Store) SetMigrationStatus(ctx context.Context, id, next string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin set status: %w", err)
	}
	defer tx.Rollback()

	var current string
	if err := tx.QueryRowContext(ctx,
		`SELECT status FROM migrations WHERE id = ?`, id).Scan(&current); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("read status: %w", err)
	}
	if current == next {
		return nil
	}
	if !CanTransition(current, next) {
		return &InvalidTransitionError{From: current, To: next}
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE migrations SET status = ?, updated_at = ? WHERE id = ?`,
		next, nowString(), id); err != nil {
		return fmt.Errorf("update status: %w", err)
	}
	// 초안으로 되돌리면 그동안의 리뷰는 더 이상 유효하지 않다.
	// 승인을 남겨두면 계획을 바꾼 뒤에도 승인된 것으로 보인다.
	if next == MigrationDraft {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM migration_reviews WHERE migration_id = ?`, id); err != nil {
			return fmt.Errorf("clear reviews: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit set status: %w", err)
	}
	return nil
}

// InvalidTransitionError는 허용되지 않는 상태 전이다.
type InvalidTransitionError struct {
	From string
	To   string
}

func (e *InvalidTransitionError) Error() string {
	return fmt.Sprintf("상태를 %s 에서 %s 로 바꿀 수 없습니다", statusLabel(e.From), statusLabel(e.To))
}

func statusLabel(status string) string {
	switch status {
	case MigrationDraft:
		return "초안"
	case MigrationInReview:
		return "리뷰 중"
	case MigrationApproved:
		return "승인됨"
	case MigrationRejected:
		return "반려됨"
	case MigrationApplied:
		return "적용됨"
	case MigrationRolledBack:
		return "롤백됨"
	case MigrationFailed:
		return "실패"
	}
	return status
}

// AddMigrationReview는 리뷰를 기록한다.
// 같은 사람이 다시 결정하면 이전 결정을 대체한다 — 승인 수를 사람 단위로 세기 때문이다.
func (s *Store) AddMigrationReview(ctx context.Context, r *MigrationReview) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin add review: %w", err)
	}
	defer tx.Rollback()

	if r.Decision != ReviewComment && r.ReviewerID != "" {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM migration_reviews
			 WHERE migration_id = ? AND reviewer_id = ? AND decision <> ?`,
			r.MigrationID, r.ReviewerID, ReviewComment); err != nil {
			return fmt.Errorf("replace previous decision: %w", err)
		}
	}
	now := nowString()
	res, err := tx.ExecContext(ctx, `INSERT INTO migration_reviews
		(migration_id, reviewer_id, reviewer_name, decision, comment, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		r.MigrationID, nullString(r.ReviewerID), r.ReviewerName, r.Decision, r.Comment, now)
	if err != nil {
		return fmt.Errorf("insert review: %w", err)
	}
	r.ID, _ = res.LastInsertId()
	r.CreatedAt = parseTime(now)
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit add review: %w", err)
	}
	return nil
}

func (s *Store) ListMigrationReviews(ctx context.Context, migrationID string) ([]*MigrationReview, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT
		id, migration_id, COALESCE(reviewer_id, ''), reviewer_name, decision, comment, created_at
		FROM migration_reviews WHERE migration_id = ? ORDER BY id`, migrationID)
	if err != nil {
		return nil, fmt.Errorf("list reviews: %w", err)
	}
	defer rows.Close()

	out := []*MigrationReview{}
	for rows.Next() {
		var r MigrationReview
		var createdAt string
		if err := rows.Scan(&r.ID, &r.MigrationID, &r.ReviewerID, &r.ReviewerName,
			&r.Decision, &r.Comment, &createdAt); err != nil {
			return nil, fmt.Errorf("scan review: %w", err)
		}
		r.CreatedAt = parseTime(createdAt)
		out = append(out, &r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate reviews: %w", err)
	}
	return out, nil
}

// ApprovalCount는 유효한 승인자 수를 센다.
//
// 사람 단위로 세는 이유: 한 사람이 여러 번 승인해도 검토자는 한 명이다.
// 운영 DB에 2차 승인을 요구하는 것은 "다른 사람이 한 번 더 봤다"는 의미이므로,
// 같은 사람의 반복을 세면 그 장치가 무력해진다.
func ApprovalCount(reviews []*MigrationReview) int {
	// 같은 사람의 마지막 결정만 본다.
	last := map[string]string{}
	for _, r := range reviews {
		if r.Decision == ReviewComment {
			continue
		}
		key := r.ReviewerID
		if key == "" {
			key = r.ReviewerName
		}
		last[key] = r.Decision
	}
	n := 0
	for _, decision := range last {
		if decision == ReviewApproved {
			n++
		}
	}
	return n
}

// HasRejection은 반려가 남아 있는지 본다.
func HasRejection(reviews []*MigrationReview) bool {
	last := map[string]string{}
	for _, r := range reviews {
		if r.Decision == ReviewComment {
			continue
		}
		key := r.ReviewerID
		if key == "" {
			key = r.ReviewerName
		}
		last[key] = r.Decision
	}
	for _, decision := range last {
		if decision == ReviewRejected {
			return true
		}
	}
	return false
}

// RecordMigrationRun은 실행 결과를 기록한다.
//
// 성공/실패를 한 함수로 받는 이유: 실패 경로에서 기록을 빠뜨리면 부분 적용 상태를
// 알 방법이 사라진다. 호출자가 실수하기 어렵게 한쪽 문을 만든다.
type RunResult struct {
	Status     string // applied | failed | rolled_back
	Steps      []ExecutionStep
	Applied    int
	Error      string
	ToVersion  *int64
	ActorID    string
	RolledBack bool
}

func (s *Store) RecordMigrationRun(ctx context.Context, id string, r RunResult) error {
	if r.Steps == nil {
		r.Steps = []ExecutionStep{}
	}
	stepsJSON, err := json.Marshal(r.Steps)
	if err != nil {
		return fmt.Errorf("marshal execution log: %w", err)
	}
	now := nowString()

	var appliedAt any
	var rolledBackAt any
	if r.Status == MigrationApplied {
		appliedAt = now
	}
	if r.RolledBack {
		rolledBackAt = now
	}

	_, err = s.db.ExecContext(ctx, `UPDATE migrations SET
		status = ?, applied_statements = ?, execution_log = ?, error = ?,
		to_version = COALESCE(?, to_version),
		applied_at = COALESCE(?, applied_at),
		applied_by = COALESCE(?, applied_by),
		rolled_back_at = COALESCE(?, rolled_back_at),
		updated_at = ?
		WHERE id = ?`,
		r.Status, r.Applied, string(stepsJSON), r.Error,
		r.ToVersion, appliedAt, nullString(r.ActorID), rolledBackAt, now, id)
	if err != nil {
		return fmt.Errorf("record migration run: %w", err)
	}
	return nil
}

// UpdateMigrationPlan은 초안 상태의 계획을 다시 만들어 덮어쓴다.
// 기준 상태가 바뀌었을 때(다른 사람이 먼저 적용) 계획을 갱신하는 데 쓴다.
func (s *Store) UpdateMigrationPlan(ctx context.Context, id string, p CreateMigrationParams) error {
	planJSON, err := json.Marshal(p.Plan)
	if err != nil {
		return fmt.Errorf("marshal plan: %w", err)
	}
	diffJSON, err := json.Marshal(p.Diff)
	if err != nil {
		return fmt.Errorf("marshal diff: %w", err)
	}
	targetJSON, err := json.Marshal(p.TargetSchema)
	if err != nil {
		return fmt.Errorf("marshal target: %w", err)
	}
	irreversible := p.Plan.Irreversible
	if irreversible == nil {
		irreversible = []string{}
	}
	irrJSON, _ := json.Marshal(irreversible)

	res, err := s.db.ExecContext(ctx, `UPDATE migrations SET
		from_version = ?, base_fingerprint = ?, target_schema_json = ?,
		up_sql = ?, down_sql = ?, plan_json = ?, diff_json = ?,
		destructive_count = ?, irreversible = ?, updated_at = ?
		WHERE id = ? AND status IN (?, ?)`,
		p.FromVersion, p.BaseFinger, string(targetJSON),
		p.Plan.UpSQL(), p.Plan.DownSQL(), string(planJSON), string(diffJSON),
		p.Diff.DestructiveCount, string(irrJSON), nowString(),
		id, MigrationDraft, MigrationRejected)
	if err != nil {
		return fmt.Errorf("update migration plan: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// 초안/반려 상태가 아니면 계획을 바꿀 수 없다. 승인된 계획이 조용히 바뀌면
		// 승인의 의미가 없어진다.
		return &InvalidTransitionError{From: "이미 검토 중이거나 실행된 마이그레이션", To: "계획 수정"}
	}
	return nil
}

func (s *Store) DeleteMigration(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM migrations WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete migration: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
