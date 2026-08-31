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
	// MigrationClosed는 실행하지 않기로 한 계획이다. 지우는 대신 닫아 두면
	// "이런 계획을 세웠다가 접었다"는 사실과 그때의 리뷰가 남는다.
	MigrationClosed = "closed"
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
	MigrationDraft:    {MigrationInReview, MigrationClosed},
	MigrationInReview: {MigrationApproved, MigrationRejected, MigrationDraft, MigrationClosed},
	// 승인 후에도 계획을 고칠 수 있어야 한다 — 다만 그때는 승인이 무효가 되므로
	// draft로 돌아간다(핸들러가 리뷰 기록을 함께 지운다).
	//
	// in_review·rejected 로도 돌아갈 수 있다: 실행 전이라면 리뷰어가 마음을 바꿀 수
	// 있어야 한다. 승인을 거두면 다시 리뷰 중이고, 뒤늦게 문제를 찾으면 반려다.
	// "이미 승인됐으니 이제 못 막는다"가 되어서는 안 된다.
	MigrationApproved: {
		MigrationApplied, MigrationFailed, MigrationInReview, MigrationRejected,
		MigrationDraft, MigrationClosed,
	},
	// 반려도 되돌릴 수 있다. 잘못 눌렀거나, 지적한 것이 그 자리에서 설명된 경우가
	// 있다. 되돌리는 길이 "계획을 초안으로 되돌려 리뷰 기록을 지우기"뿐이면
	// 사람들은 반려를 누르기를 망설이게 되고, 그러면 반려는 쓰이지 않는 버튼이 된다.
	MigrationRejected: {MigrationInReview, MigrationApproved, MigrationDraft, MigrationClosed},
	// 실패한 마이그레이션은 부분 적용 상태일 수 있다. 사람이 상태를 확인한 뒤
	// 다시 계획을 세우도록 draft로만 돌린다. 닫을 수는 있다 — 부분 적용을 손으로
	// 정리하고 이 계획은 더 쓰지 않기로 하는 경우가 있다.
	MigrationFailed: {MigrationDraft, MigrationClosed},
	// 적용된 계획도 닫을 수 있다. 목록에 영원히 남아 있으면 "지금 볼 것"과 "끝난 것"이
	// 섞여, 목록은 훑을수록 무거워진다.
	//
	// 닫아도 사실이 사라지지는 않는다. 닫기 전 상태를 적어 두었다가 다시 열 때 그
	// 자리(적용됨)로 돌려보내므로, 롤백할 길도 그대로 남는다(SetMigrationStatus).
	MigrationApplied: {MigrationRolledBack, MigrationClosed},
	// 닫은 계획은 다시 열 수 있다. 초안으로 돌아가는 이유: 닫혀 있는 동안 대상
	// DB가 바뀌었을 수 있고, 그때의 승인을 그대로 인정하면 지금 구조를 아무도
	// 보지 않은 채 실행할 수 있다(draft 전이가 리뷰 기록을 지운다).
	//
	// 적용됨으로도 돌아갈 수 있다 — 다만 **적용된 상태에서 닫은 계획만** 그렇다.
	// 그 검사는 표가 아니라 SetMigrationStatus가 한다(closed_from을 봐야 한다).
	// 그러지 않으면 초안에서 닫은 계획을 "적용됨"으로 만들 수 있고, 그것은 실행
	// 기록 없는 적용이다.
	MigrationClosed: {MigrationDraft, MigrationApplied},
	// 롤백된 계획은 **다시 실행할 수 있다**(승인됨으로 되돌린다).
	//
	// 롤백은 "이 변경을 물린다"이지 "이 계획을 버린다"가 아니다. 실행 중 문제가
	// 생겨 일단 되돌렸다가, 원인을 고친 뒤 같은 변경을 다시 넣는 일이 흔하다.
	// 그때마다 계획을 새로 만들고 다시 승인을 받아야 한다면, 사람들은 롤백을
	// 누르기를 망설이게 된다 — 되돌리기가 비싸지면 아무도 되돌리지 않는다.
	//
	// 승인 기록은 그대로 둔다(초안으로 되돌리기와 다른 점이다). 계획의 내용이
	// 바뀌지 않았고, 롤백으로 DB가 실행 전 구조로 돌아왔으므로 그때의 승인은
	// 여전히 "지금 이 구조에 이 변경을 해도 좋다"는 뜻이다. 구조가 실제로 그런지는
	// 사전 검사가 기준 지문으로 다시 확인한다.
	//
	// 닫을 수도 있다. 되돌린 뒤 "이 변경은 하지 않기로 했다"고 정하는 것이 롤백의
	// 흔한 결말인데, 닫을 길이 없으면 그 계획은 목록에서 영원히 "롤백됨"으로 남아
	// 아직 할 일인지 끝난 일인지 구분되지 않는다. 적용됨과 다른 점: 롤백된 계획은
	// DB에 남긴 것이 없으므로, 닫아도 "지금 DB가 어떻게 됐는가"의 답이 사라지지
	// 않는다(그 답은 롤백 버전과 활동 기록에 있다).
	MigrationRolledBack: {MigrationApproved, MigrationClosed},
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

	Status string `json:"status"`
	// ClosedFrom은 닫기 전의 상태다. 다시 열 때 어디로 돌아갈지가 여기에 있다.
	// 빈 값은 "닫힌 적이 없거나, 이 항목이 생기기 전에 닫혔다"는 뜻이다.
	ClosedFrom        string          `json:"closedFrom,omitempty"`
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

	// AssigneeID는 이 마이그레이션을 끌고 가는 사람이다. 비어 있으면 아직 정하지 않았다.
	AssigneeID   string `json:"assigneeId,omitempty"`
	AssigneeName string `json:"assigneeName,omitempty"`
	// Reviewers는 검토를 부탁받은 사람들이다(리뷰 결정과 다르다 — Reviews 참고).
	Reviewers []*MigrationReviewer `json:"reviewers"`
}

// MigrationReviewer는 검토를 부탁받은 사람이다.
//
// MigrationReview(결정)와 이름이 비슷해 헷갈리기 쉬우므로 뜻을 적어 둔다.
// 이것은 "봐 달라"는 요청이고, 저것은 "봤다"는 기록이다. 한 사람이 리뷰어로
// 지정되었는데 아직 결정하지 않은 상태가 정상적으로 존재한다.
type MigrationReviewer struct {
	MigrationID string    `json:"migrationId,omitempty"`
	UserID      string    `json:"userId"`
	Name        string    `json:"name"`
	AddedBy     string    `json:"addedBy,omitempty"`
	AddedAt     time.Time `json:"addedAt"`
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
	// Undo는 이 문장이 "적용"이 아니라 "되돌리기"였음을 뜻한다.
	//
	// 실패 뒤 앞부분을 되돌린 문장이 같은 기록에 이어 붙기 때문에, 구분이 없으면
	// 되돌린 것까지 적용된 것으로 읽힌다.
	Undo bool `json:"undo,omitempty"`
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
	// 담당자는 만든 사람으로 시작한다. 계획을 만든 사람이 그것을 끌고 가는 것이
	// 기본이고, 비워 두면 "누가 맡았나"의 답이 대부분 빈칸이 되어 그 칸을 아무도
	// 보지 않게 된다. 다른 사람에게 넘기는 것은 지정 대화상자에서 한다.
	_, err = s.db.ExecContext(ctx, `INSERT INTO migrations
		(id, connection_id, doc_id, title, from_version, rollback_to_version, base_fingerprint,
		 target_schema_json, up_sql, down_sql, plan_json, diff_json,
		 destructive_count, irreversible, status, created_by, assignee_id, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, p.ConnectionID, nullString(p.DocID), p.Title, p.FromVersion, p.RollbackTo, p.BaseFinger,
		string(targetJSON), p.Plan.UpSQL(), p.Plan.DownSQL(), string(planJSON), string(diffJSON),
		p.Diff.DestructiveCount, string(irrJSON), MigrationDraft,
		nullString(p.CreatedBy), nullString(p.CreatedBy), now, now)
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
	reviewers, err := s.ListMigrationReviewers(ctx, id)
	if err != nil {
		return nil, err
	}
	m.Reviewers = reviewers
	return m, nil
}

const migrationSelect = `SELECT
	m.id, m.connection_id, COALESCE(m.doc_id, ''), m.title,
	m.from_version, m.to_version, fv.version_no, tv.version_no,
	m.rollback_to_version, rv.version_no,
	m.base_fingerprint, m.up_sql, m.down_sql, m.plan_json, m.diff_json, m.target_schema_json,
	m.destructive_count, m.irreversible, m.status, m.closed_from, m.applied_statements, m.execution_log,
	m.error, COALESCE(m.created_by, ''), m.created_at, m.updated_at,
	m.applied_at, COALESCE(m.applied_by, ''), m.rolled_back_at,
	COALESCE(m.assignee_id, ''), COALESCE(au.display_name, au.username, '')
	FROM migrations m
	LEFT JOIN schema_versions fv ON fv.id = m.from_version
	LEFT JOIN schema_versions tv ON tv.id = m.to_version
	LEFT JOIN schema_versions rv ON rv.id = m.rollback_to_version
	LEFT JOIN users au ON au.id = m.assignee_id`

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
		&m.DestructiveCount, &irrJSON, &m.Status, &m.ClosedFrom, &m.AppliedStatements, &execJSON,
		&m.Error, &m.CreatedBy, &createdAt, &updatedAt,
		&appliedAt, &m.AppliedBy, &rolledBackAt,
		&m.AssigneeID, &m.AssigneeName); err != nil {
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
	// 닫힌 계획을 적용됨으로 되돌리는 것은 "닫기 전에 적용돼 있었다"는 사실이
	// 있을 때만이다. 그 사실이 없으면 실행 기록 없는 적용이 된다.
	if current == MigrationClosed && next == MigrationApplied {
		var from string
		if err := tx.QueryRowContext(ctx,
			`SELECT closed_from FROM migrations WHERE id = ?`, id).Scan(&from); err != nil {
			return fmt.Errorf("read closed_from: %w", err)
		}
		if from != MigrationApplied {
			return &InvalidTransitionError{From: current, To: next}
		}
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE migrations SET status = ?, updated_at = ? WHERE id = ?`,
		next, nowString(), id); err != nil {
		return fmt.Errorf("update status: %w", err)
	}
	// 닫을 때는 어디서 닫았는지를 적어 두고, 열 때는 지운다. 다시 열 때 그 자리로
	// 돌려보내야 "적용된 계획"이 "실행하지 않은 초안"으로 둔갑하지 않는다.
	//
	// 닫기와 무관한 전이에서는 이 컬럼을 건드리지 않는다. 늘 쓰면 이 컬럼이 없던
	// 시절의 DB(옛 마이그레이션을 시험하는 자리)에서 상태 변경 자체가 깨진다.
	if next == MigrationClosed || current == MigrationClosed {
		closedFrom := ""
		if next == MigrationClosed {
			closedFrom = current
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE migrations SET closed_from = ? WHERE id = ?`, closedFrom, id); err != nil {
			return fmt.Errorf("update closed_from: %w", err)
		}
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

// GetMigrationReview는 리뷰 한 건을 가져온다.
//
// migrationID를 함께 받는 이유: id는 표 전체에서 하나뿐인 번호라서, 그것만으로
// 찾으면 다른 마이그레이션의 리뷰를 이 계획의 것으로 다루게 된다. 권한 검사는
// 계획 단위로 이뤄지므로 소속이 맞는지가 검사의 전제다.
func (s *Store) GetMigrationReview(ctx context.Context, migrationID string, id int64) (*MigrationReview, error) {
	var r MigrationReview
	var createdAt string
	err := s.db.QueryRowContext(ctx, `SELECT
		id, migration_id, COALESCE(reviewer_id, ''), reviewer_name, decision, comment, created_at
		FROM migration_reviews WHERE id = ? AND migration_id = ?`, id, migrationID).
		Scan(&r.ID, &r.MigrationID, &r.ReviewerID, &r.ReviewerName,
			&r.Decision, &r.Comment, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get review: %w", err)
	}
	r.CreatedAt = parseTime(createdAt)
	return &r, nil
}

// UpdateMigrationReviewComment는 리뷰의 내용만 고친다.
//
// 결정(decision)은 건드리지 않는다. 승인·반려를 바꾸는 일은 승인 수와 상태를
// 움직이므로 AddMigrationReview(다시 결정하기)를 지나가야 하고, 그래야 "언제
// 무엇으로 바뀌었는가"가 기록으로 남는다. 여기서 고치는 것은 적어 둔 말뿐이다.
func (s *Store) UpdateMigrationReviewComment(ctx context.Context, migrationID string, id int64, comment string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE migration_reviews SET comment = ? WHERE id = ? AND migration_id = ?`,
		comment, id, migrationID)
	if err != nil {
		return fmt.Errorf("update review comment: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteMigrationReview는 리뷰 한 건을 지운다.
func (s *Store) DeleteMigrationReview(ctx context.Context, migrationID string, id int64) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM migration_reviews WHERE id = ? AND migration_id = ?`, id, migrationID)
	if err != nil {
		return fmt.Errorf("delete review: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
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

// ---------- 담당자 · 리뷰어 ----------

// ListMigrationReviewers는 검토를 부탁받은 사람들을 돌려준다.
//
// 이름을 users에서 조인해 오는 이유: 지정 시점의 이름을 복사해 두면 사람이 이름을
// 바꾼 뒤 화면과 실제가 어긋난다. 리뷰 결정(migration_reviews)은 반대로 그 시점의
// 이름을 남긴다 — 그것은 "그때 누가 승인했는가"라는 기록이라 지금의 이름으로
// 바뀌면 안 된다.
func (s *Store) ListMigrationReviewers(ctx context.Context, migrationID string) ([]*MigrationReviewer, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT
		r.migration_id, r.user_id, COALESCE(u.display_name, u.username, ''),
		COALESCE(r.added_by, ''), r.added_at
		FROM migration_reviewers r
		LEFT JOIN users u ON u.id = r.user_id
		WHERE r.migration_id = ?
		ORDER BY r.added_at, r.user_id`, migrationID)
	if err != nil {
		return nil, fmt.Errorf("list migration reviewers: %w", err)
	}
	defer rows.Close()

	out := []*MigrationReviewer{}
	for rows.Next() {
		var r MigrationReviewer
		var addedAt string
		if err := rows.Scan(&r.MigrationID, &r.UserID, &r.Name, &r.AddedBy, &addedAt); err != nil {
			return nil, fmt.Errorf("scan migration reviewer: %w", err)
		}
		r.AddedAt = parseTime(addedAt)
		out = append(out, &r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate migration reviewers: %w", err)
	}
	return out, nil
}

// SetMigrationAssignment는 담당자와 리뷰어를 한 번에 정한다.
//
// 둘을 한 트랜잭션에서 바꾸는 이유: 화면이 한 대화상자에서 함께 고르므로, 절반만
// 저장되면 사용자는 무엇이 반영됐는지 알 수 없다.
//
// 리뷰어는 지우고 다시 넣는다(합집합이 아니다). 지정은 "지금 이 사람들에게
// 부탁한다"는 현재 상태이며, 뺀 사람이 남아 있으면 그 사람은 자기 이름이 왜
// 붙어 있는지 알 수 없다. 이미 남긴 리뷰 결정은 다른 표에 있어 그대로 남는다.
//
// updated_at은 건드리지 않는다. 그 값은 "계획이 언제 바뀌었나"를 뜻하고 목록의
// 수정 시각으로 쓰인다 — 담당자를 바꿨다고 계획이 바뀐 것처럼 보이면 안 된다.
func (s *Store) SetMigrationAssignment(ctx context.Context, migrationID, assigneeID string, reviewerIDs []string, actorID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin set assignment: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx,
		`UPDATE migrations SET assignee_id = ? WHERE id = ?`, nullString(assigneeID), migrationID)
	if err != nil {
		return fmt.Errorf("set assignee: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM migration_reviewers WHERE migration_id = ?`, migrationID); err != nil {
		return fmt.Errorf("clear reviewers: %w", err)
	}
	now := nowString()
	seen := map[string]bool{}
	for _, id := range reviewerIDs {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO migration_reviewers (migration_id, user_id, added_by, added_at)
			 VALUES (?, ?, ?, ?)`, migrationID, id, nullString(actorID), now); err != nil {
			return fmt.Errorf("add reviewer: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit set assignment: %w", err)
	}
	return nil
}

// MigrationPerson은 담당자·리뷰어 후보다.
//
// /users 를 쓰지 않는 이유는 매크로 공유와 같다: 그 경로는 슈퍼어드민 전용이고
// 비밀번호 해시까지 실어 나른다. 여기서 필요한 것은 이름과 id 뿐이다.
type MigrationPerson struct {
	ID          string `json:"id"`
	Username    string `json:"username"`
	DisplayName string `json:"displayName"`
	Role        string `json:"role"`
	// Level은 이 사람이 대상 커넥션에 대해 가진 등급이다. API가 채운다.
	Level string `json:"level,omitempty"`
}

// ListActivePeople은 활성 사용자 전체를 이름만으로 돌려준다.
//
// 누가 후보인지(대상 커넥션을 만질 수 있는지)는 접근 정책을 판정해야 알 수 있고,
// 그 판정은 store가 아니라 auth의 일이다. 여기서는 재료만 준다.
func (s *Store) ListActivePeople(ctx context.Context) ([]MigrationPerson, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, username, COALESCE(display_name, ''), role FROM users
		 WHERE status = 'active' ORDER BY username`)
	if err != nil {
		return nil, fmt.Errorf("list active people: %w", err)
	}
	defer rows.Close()

	out := []MigrationPerson{}
	for rows.Next() {
		var p MigrationPerson
		if err := rows.Scan(&p.ID, &p.Username, &p.DisplayName, &p.Role); err != nil {
			return nil, fmt.Errorf("scan person: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
