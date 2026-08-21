package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"dbstudio/internal/schema"
)

// 버전의 출처.
const (
	VersionSourceImport   = "initial_import"
	VersionSourceMigrated = "migration"
	VersionSourceExternal = "external_edit"
	VersionSourceRollback = "rollback"
)

// SchemaVersion은 확정된 스키마 이력의 한 점이다.
type SchemaVersion struct {
	ID            int64          `json:"id"`
	ConnectionID  string         `json:"connectionId"`
	VersionNo     int            `json:"versionNo"`
	Fingerprint   string         `json:"fingerprint"`
	Source        string         `json:"source"`
	Note          string         `json:"note,omitempty"`
	ChangeSummary []string       `json:"changeSummary"`
	AuthorID      string         `json:"authorId,omitempty"`
	AuthorName    string         `json:"authorName,omitempty"`
	CreatedAt     time.Time      `json:"createdAt"`
	Schema        *schema.Schema `json:"schema,omitempty"`
	// Stats는 본문을 내려보내지 않는 목록 화면용 요약이다.
	Stats *schema.Stats `json:"stats,omitempty"`
}

// SaveVersionParams는 버전 등록 입력이다.
type SaveVersionParams struct {
	ConnectionID  string
	Schema        *schema.Schema
	Source        string
	Note          string
	ChangeSummary []string
	AuthorID      string
	AuthorName    string
}

// SaveSchemaVersion은 새 버전을 등록한다.
//
// 같은 지문이 이미 최신 버전이면 새로 만들지 않고 그것을 반환한다(created=false).
// 같은 구조를 여러 번 등록하면 이력이 의미를 잃고, "무엇이 언제 바뀌었나"를 읽을 수 없다.
//
// version_no 부여와 삽입을 한 트랜잭션에 묶는 이유: 두 요청이 같은 번호를 받으면
// UNIQUE 제약으로 한쪽이 실패하는데, 그 실패는 사용자 입장에서 설명할 수 없다.
func (s *Store) SaveSchemaVersion(ctx context.Context, p SaveVersionParams) (*SchemaVersion, bool, error) {
	if p.Schema == nil {
		return nil, false, errors.New("스키마가 없습니다")
	}
	fingerprint := p.Schema.Fingerprint()
	schemaJSON, err := json.Marshal(p.Schema)
	if err != nil {
		return nil, false, fmt.Errorf("marshal schema: %w", err)
	}
	if p.ChangeSummary == nil {
		p.ChangeSummary = []string{}
	}
	summaryJSON, err := json.Marshal(p.ChangeSummary)
	if err != nil {
		return nil, false, fmt.Errorf("marshal change summary: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, fmt.Errorf("begin save version: %w", err)
	}
	defer tx.Rollback()

	var lastNo int
	var lastFingerprint string
	var lastID int64
	err = tx.QueryRowContext(ctx, `SELECT id, version_no, fingerprint
		FROM schema_versions WHERE connection_id = ?
		ORDER BY version_no DESC LIMIT 1`, p.ConnectionID).Scan(&lastID, &lastNo, &lastFingerprint)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, false, fmt.Errorf("read last version: %w", err)
	}
	if lastFingerprint == fingerprint {
		existing, gerr := s.GetSchemaVersion(ctx, lastID, false)
		if gerr != nil {
			return nil, false, gerr
		}
		return existing, false, nil
	}

	now := nowString()
	res, err := tx.ExecContext(ctx, `INSERT INTO schema_versions
		(connection_id, version_no, fingerprint, schema_json, source, note,
		 change_summary, author_id, author_name, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ConnectionID, lastNo+1, fingerprint, string(schemaJSON), p.Source, p.Note,
		string(summaryJSON), nullString(p.AuthorID), p.AuthorName, now)
	if err != nil {
		return nil, false, fmt.Errorf("insert schema version: %w", err)
	}
	id, _ := res.LastInsertId()
	if err := tx.Commit(); err != nil {
		return nil, false, fmt.Errorf("commit save version: %w", err)
	}

	stats := p.Schema.Stats()
	return &SchemaVersion{
		ID: id, ConnectionID: p.ConnectionID, VersionNo: lastNo + 1,
		Fingerprint: fingerprint, Source: p.Source, Note: p.Note,
		ChangeSummary: p.ChangeSummary, AuthorID: p.AuthorID, AuthorName: p.AuthorName,
		CreatedAt: parseTime(now), Schema: p.Schema, Stats: &stats,
	}, true, nil
}

// GetSchemaVersion은 버전 하나를 읽는다.
func (s *Store) GetSchemaVersion(ctx context.Context, id int64, withSchema bool) (*SchemaVersion, error) {
	row := s.db.QueryRowContext(ctx, `SELECT
		id, connection_id, version_no, fingerprint, source, note, change_summary,
		COALESCE(author_id, ''), author_name, created_at, schema_json
		FROM schema_versions WHERE id = ?`, id)
	v, err := scanVersion(row, withSchema)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return v, err
}

// LatestSchemaVersion은 커넥션의 최신 버전을 반환한다. 없으면 nil, nil이다.
func (s *Store) LatestSchemaVersion(ctx context.Context, connectionID string, withSchema bool) (*SchemaVersion, error) {
	row := s.db.QueryRowContext(ctx, `SELECT
		id, connection_id, version_no, fingerprint, source, note, change_summary,
		COALESCE(author_id, ''), author_name, created_at, schema_json
		FROM schema_versions WHERE connection_id = ?
		ORDER BY version_no DESC LIMIT 1`, connectionID)
	v, err := scanVersion(row, withSchema)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return v, err
}

// ListSchemaVersions는 버전 이력을 최신순으로 반환한다 (스키마 본문 제외, 통계만).
func (s *Store) ListSchemaVersions(ctx context.Context, connectionID string, limit int) ([]*SchemaVersion, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `SELECT
		id, connection_id, version_no, fingerprint, source, note, change_summary,
		COALESCE(author_id, ''), author_name, created_at, schema_json
		FROM schema_versions WHERE connection_id = ?
		ORDER BY version_no DESC LIMIT ?`, connectionID, limit)
	if err != nil {
		return nil, fmt.Errorf("list schema versions: %w", err)
	}
	defer rows.Close()

	out := []*SchemaVersion{}
	for rows.Next() {
		// 목록에서는 본문 대신 통계만 필요하지만, 통계는 스키마를 디코딩해야 얻는다.
		// 커넥션당 버전 수는 많지 않으므로 디코딩 후 본문을 버린다.
		v, err := scanVersion(rows, true)
		if err != nil {
			return nil, err
		}
		if v.Schema != nil {
			stats := v.Schema.Stats()
			v.Stats = &stats
			v.Schema = nil
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate schema versions: %w", err)
	}
	return out, nil
}

func scanVersion(row interface{ Scan(...any) error }, withSchema bool) (*SchemaVersion, error) {
	var v SchemaVersion
	var summaryJSON, createdAt, schemaJSON string
	if err := row.Scan(&v.ID, &v.ConnectionID, &v.VersionNo, &v.Fingerprint, &v.Source,
		&v.Note, &summaryJSON, &v.AuthorID, &v.AuthorName, &createdAt, &schemaJSON); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		return nil, fmt.Errorf("scan schema version: %w", err)
	}
	v.CreatedAt = parseTime(createdAt)
	v.ChangeSummary = []string{}
	_ = json.Unmarshal([]byte(summaryJSON), &v.ChangeSummary)
	if withSchema {
		var sc schema.Schema
		if err := json.Unmarshal([]byte(schemaJSON), &sc); err != nil {
			return nil, fmt.Errorf("unmarshal version schema: %w", err)
		}
		v.Schema = &sc
		stats := sc.Stats()
		v.Stats = &stats
	}
	return &v, nil
}
