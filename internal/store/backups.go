package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// 백업(논리 덤프) 기록.
//
// 파일은 디스크에, 기록은 여기에 있다. 둘이 어긋나는 경우가 반드시 생기므로
// (사람이 파일을 지우거나, 앱이 파일을 쓰는 중에 죽거나) 조회할 때 파일 존재 여부를
// 함께 알려준다 — 목록에 있는 백업을 복구하려다 "파일이 없습니다"를 만나는 것보다
// 목록에서 미리 보이는 편이 낫다.

type Backup struct {
	ID             string         `json:"id"`
	ConnectionID   string         `json:"connectionId,omitempty"`
	ConnectionName string         `json:"connectionName"`
	ConnectionKind string         `json:"connectionKind"`
	Scope          string         `json:"scope"`
	Format         string         `json:"format"`
	Status         string         `json:"status"`
	FileName       string         `json:"fileName,omitempty"`
	SizeBytes      int64          `json:"sizeBytes"`
	TableCount     int            `json:"tableCount"`
	RowCount       int64          `json:"rowCount"`
	StatementCount int            `json:"statementCount"`
	Options        map[string]any `json:"options"`
	Note           string         `json:"note,omitempty"`
	Error          string         `json:"error,omitempty"`
	Progress       string         `json:"progress,omitempty"`
	ActorID        string         `json:"actorId,omitempty"`
	ActorName      string         `json:"actorName"`
	Trigger        string         `json:"trigger"`
	StartedAt      time.Time      `json:"startedAt"`
	FinishedAt     *time.Time     `json:"finishedAt,omitempty"`
	DurationMs     int64          `json:"durationMs"`
	// FileMissing은 기록은 있는데 파일이 없는 상태다. 조회할 때 채워진다.
	FileMissing bool `json:"fileMissing,omitempty"`
}

type CreateBackupParams struct {
	ConnectionID   string
	ConnectionName string
	ConnectionKind string
	Scope          string
	Format         string
	FileName       string
	Options        map[string]any
	Note           string
	ActorID        string
	ActorName      string
	Trigger        string
}

func (s *Store) CreateBackup(ctx context.Context, p CreateBackupParams) (string, error) {
	id := uuid.NewString()
	optJSON := "{}"
	if len(p.Options) > 0 {
		if b, err := json.Marshal(p.Options); err == nil {
			optJSON = string(b)
		}
	}
	trigger := p.Trigger
	if trigger == "" {
		trigger = "manual"
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO backups
		(id, connection_id, connection_name, connection_kind, scope, format, status,
		 file_name, options, note, actor_id, actor_name, trigger, started_at)
		VALUES (?, ?, ?, ?, ?, ?, 'running', ?, ?, ?, ?, ?, ?, ?)`,
		id, nullString(p.ConnectionID), p.ConnectionName, p.ConnectionKind, p.Scope,
		p.Format, p.FileName, optJSON, p.Note, nullString(p.ActorID), p.ActorName,
		trigger, nowString()); err != nil {
		return "", fmt.Errorf("insert backup: %w", err)
	}
	return id, nil
}

// UpdateBackupProgress는 진행 상황 한 줄을 갱신한다.
//
// 별도 로그 테이블을 두지 않는 이유: 덤프가 만드는 정보는 "지금 어느 테이블의 몇
// 번째 행"이라는 숫자 하나이고, 그것은 덮어쓰면 되는 값이다. 줄이 쌓이는 매크로
// 로그와는 성격이 다르다.
func (s *Store) UpdateBackupProgress(ctx context.Context, id, progress string, rows int64, tables int) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE backups SET progress = ?, row_count = ?, table_count = ? WHERE id = ?`,
		progress, rows, tables, id)
	if err != nil {
		return fmt.Errorf("update backup progress: %w", err)
	}
	return nil
}

type FinishBackupParams struct {
	Status         string
	Error          string
	FileName       string
	SizeBytes      int64
	TableCount     int
	RowCount       int64
	StatementCount int
	DurationMs     int64
}

func (s *Store) FinishBackup(ctx context.Context, id string, p FinishBackupParams) error {
	_, err := s.db.ExecContext(ctx, `UPDATE backups
		SET status = ?, error = ?, file_name = ?, size_bytes = ?, table_count = ?,
		    row_count = ?, statement_count = ?, duration_ms = ?, finished_at = ?, progress = ''
		WHERE id = ?`,
		p.Status, p.Error, p.FileName, p.SizeBytes, p.TableCount, p.RowCount,
		p.StatementCount, p.DurationMs, nowString(), id)
	if err != nil {
		return fmt.Errorf("finish backup: %w", err)
	}
	return nil
}

const backupColumns = `id, connection_id, connection_name, connection_kind, scope, format,
	status, file_name, size_bytes, table_count, row_count, statement_count, options,
	note, error, progress, actor_id, actor_name, trigger, started_at, finished_at, duration_ms`

func scanBackup(row interface{ Scan(...any) error }) (*Backup, error) {
	var b Backup
	var connID, actorID, finishedAt sql.NullString
	var options, startedAt string
	err := row.Scan(&b.ID, &connID, &b.ConnectionName, &b.ConnectionKind, &b.Scope,
		&b.Format, &b.Status, &b.FileName, &b.SizeBytes, &b.TableCount, &b.RowCount,
		&b.StatementCount, &options, &b.Note, &b.Error, &b.Progress, &actorID,
		&b.ActorName, &b.Trigger, &startedAt, &finishedAt, &b.DurationMs)
	if err != nil {
		return nil, err
	}
	b.ConnectionID = connID.String
	b.ActorID = actorID.String
	b.StartedAt = parseTime(startedAt)
	b.FinishedAt = parseTimePtr(finishedAt)
	b.Options = map[string]any{}
	_ = json.Unmarshal([]byte(options), &b.Options)
	return &b, nil
}

func (s *Store) GetBackup(ctx context.Context, id string) (*Backup, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+backupColumns+` FROM backups WHERE id = ?`, id)
	b, err := scanBackup(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get backup: %w", err)
	}
	return b, nil
}

// ListBackups는 백업 목록을 최신순으로 반환한다.
func (s *Store) ListBackups(ctx context.Context, connectionID string, limit int) ([]*Backup, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	q := `SELECT ` + backupColumns + ` FROM backups WHERE 1 = 1`
	args := []any{}
	if connectionID != "" {
		q += ` AND connection_id = ?`
		args = append(args, connectionID)
	}
	q += ` ORDER BY started_at DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list backups: %w", err)
	}
	defer rows.Close()

	out := []*Backup{}
	for rows.Next() {
		b, err := scanBackup(rows)
		if err != nil {
			return nil, fmt.Errorf("scan backup: %w", err)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate backups: %w", err)
	}
	return out, nil
}

func (s *Store) DeleteBackup(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM backups WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete backup: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ExpiredBackups는 보존 기간이 지난 백업을 반환한다.
// 파일을 지우는 것은 호출부의 몫이다 — 저장 계층은 파일 시스템을 모른다.
func (s *Store) ExpiredBackups(ctx context.Context, before time.Time) ([]*Backup, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+backupColumns+` FROM backups WHERE started_at < ? AND status != 'running'`,
		formatTime(before))
	if err != nil {
		return nil, fmt.Errorf("list expired backups: %w", err)
	}
	defer rows.Close()

	out := []*Backup{}
	for rows.Next() {
		b, err := scanBackup(rows)
		if err != nil {
			return nil, fmt.Errorf("scan expired backup: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// MarkStaleBackupsFailed는 실행 중으로 남은 백업·복구 기록을 정리한다.
// 매크로 실행과 같은 이유다 — 앱이 죽으면 그 작업은 이어지지 않는다.
func (s *Store) MarkStaleBackupsFailed(ctx context.Context) (int64, error) {
	const msg = "앱이 재시작되어 중단되었습니다"
	now := nowString()
	var total int64
	res, err := s.db.ExecContext(ctx,
		`UPDATE backups SET status = 'failed', error = ?, finished_at = ? WHERE status = 'running'`,
		msg, now)
	if err != nil {
		return 0, fmt.Errorf("mark stale backups: %w", err)
	}
	n, _ := res.RowsAffected()
	total += n

	res, err = s.db.ExecContext(ctx,
		`UPDATE backup_restores SET status = 'failed', error = ?, finished_at = ? WHERE status = 'running'`,
		msg, now)
	if err != nil {
		return total, fmt.Errorf("mark stale restores: %w", err)
	}
	n, _ = res.RowsAffected()
	return total + n, nil
}

// ---------- 복구 ----------

type Restore struct {
	ID              string     `json:"id"`
	BackupID        string     `json:"backupId,omitempty"`
	BackupLabel     string     `json:"backupLabel"`
	ConnectionID    string     `json:"connectionId,omitempty"`
	ConnectionName  string     `json:"connectionName"`
	Status          string     `json:"status"`
	StatementsTotal int        `json:"statementsTotal"`
	StatementsDone  int        `json:"statementsDone"`
	FailedStatement string     `json:"failedStatement,omitempty"`
	Error           string     `json:"error,omitempty"`
	Progress        string     `json:"progress,omitempty"`
	ActorID         string     `json:"actorId,omitempty"`
	ActorName       string     `json:"actorName"`
	StartedAt       time.Time  `json:"startedAt"`
	FinishedAt      *time.Time `json:"finishedAt,omitempty"`
	DurationMs      int64      `json:"durationMs"`
}

type CreateRestoreParams struct {
	BackupID       string
	BackupLabel    string
	ConnectionID   string
	ConnectionName string
	ActorID        string
	ActorName      string
}

func (s *Store) CreateRestore(ctx context.Context, p CreateRestoreParams) (string, error) {
	id := uuid.NewString()
	if _, err := s.db.ExecContext(ctx, `INSERT INTO backup_restores
		(id, backup_id, backup_label, connection_id, connection_name, status,
		 actor_id, actor_name, started_at)
		VALUES (?, ?, ?, ?, ?, 'running', ?, ?, ?)`,
		id, nullString(p.BackupID), p.BackupLabel, nullString(p.ConnectionID),
		p.ConnectionName, nullString(p.ActorID), p.ActorName, nowString()); err != nil {
		return "", fmt.Errorf("insert restore: %w", err)
	}
	return id, nil
}

func (s *Store) UpdateRestoreProgress(ctx context.Context, id string, done, total int, progress string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE backup_restores SET statements_done = ?, statements_total = ?, progress = ? WHERE id = ?`,
		done, total, progress, id)
	if err != nil {
		return fmt.Errorf("update restore progress: %w", err)
	}
	return nil
}

type FinishRestoreParams struct {
	Status          string
	Error           string
	FailedStatement string
	StatementsDone  int
	StatementsTotal int
	DurationMs      int64
}

func (s *Store) FinishRestore(ctx context.Context, id string, p FinishRestoreParams) error {
	_, err := s.db.ExecContext(ctx, `UPDATE backup_restores
		SET status = ?, error = ?, failed_statement = ?, statements_done = ?,
		    statements_total = ?, duration_ms = ?, finished_at = ?, progress = ''
		WHERE id = ?`,
		p.Status, p.Error, p.FailedStatement, p.StatementsDone, p.StatementsTotal,
		p.DurationMs, nowString(), id)
	if err != nil {
		return fmt.Errorf("finish restore: %w", err)
	}
	return nil
}

const restoreColumns = `id, backup_id, backup_label, connection_id, connection_name, status,
	statements_total, statements_done, failed_statement, error, progress,
	actor_id, actor_name, started_at, finished_at, duration_ms`

func scanRestore(row interface{ Scan(...any) error }) (*Restore, error) {
	var r Restore
	var backupID, connID, actorID, finishedAt sql.NullString
	var startedAt string
	err := row.Scan(&r.ID, &backupID, &r.BackupLabel, &connID, &r.ConnectionName,
		&r.Status, &r.StatementsTotal, &r.StatementsDone, &r.FailedStatement,
		&r.Error, &r.Progress, &actorID, &r.ActorName, &startedAt, &finishedAt, &r.DurationMs)
	if err != nil {
		return nil, err
	}
	r.BackupID = backupID.String
	r.ConnectionID = connID.String
	r.ActorID = actorID.String
	r.StartedAt = parseTime(startedAt)
	r.FinishedAt = parseTimePtr(finishedAt)
	return &r, nil
}

func (s *Store) GetRestore(ctx context.Context, id string) (*Restore, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+restoreColumns+` FROM backup_restores WHERE id = ?`, id)
	r, err := scanRestore(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get restore: %w", err)
	}
	return r, nil
}

func (s *Store) ListRestores(ctx context.Context, connectionID string, limit int) ([]*Restore, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	q := `SELECT ` + restoreColumns + ` FROM backup_restores WHERE 1 = 1`
	args := []any{}
	if connectionID != "" {
		q += ` AND connection_id = ?`
		args = append(args, connectionID)
	}
	q += ` ORDER BY started_at DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list restores: %w", err)
	}
	defer rows.Close()

	out := []*Restore{}
	for rows.Next() {
		r, err := scanRestore(rows)
		if err != nil {
			return nil, fmt.Errorf("scan restore: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
