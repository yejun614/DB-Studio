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

// 스냅샷 출처
const (
	SnapshotSourceMonitor   = "monitor"   // 폴러가 주기적으로 수집
	SnapshotSourceManual    = "manual"    // 사용자가 요청
	SnapshotSourceMigration = "migration" // 마이그레이션 직후
)

// SchemaSnapshot은 특정 시점의 스키마 전체다.
// 드리프트 감지(무엇이 바뀌었는지 보여주기)와 P7 버전 등록의 기반이 된다.
type SchemaSnapshot struct {
	ID            int64          `json:"id"`
	ConnectionID  string         `json:"connectionId"`
	Fingerprint   string         `json:"fingerprint"`
	CapturedAt    time.Time      `json:"capturedAt"`
	Source        string         `json:"source"`
	ChangeSummary []string       `json:"changeSummary"`
	Schema        *schema.Schema `json:"schema,omitempty"`
}

// SaveSchemaSnapshot은 스냅샷을 저장한다.
// 지문이 이전과 같으면 저장하지 않고 기존 스냅샷을 반환한다 —
// 폴링마다 같은 스키마를 쌓으면 저장소가 금방 커진다.
func (s *Store) SaveSchemaSnapshot(ctx context.Context, connectionID string, sc *schema.Schema, source string, changes []string) (*SchemaSnapshot, bool, error) {
	fingerprint := sc.Fingerprint()

	prev, err := s.LatestSchemaSnapshot(ctx, connectionID, false)
	if err != nil {
		return nil, false, err
	}
	if prev != nil && prev.Fingerprint == fingerprint {
		return prev, false, nil
	}

	schemaJSON, err := json.Marshal(sc)
	if err != nil {
		return nil, false, fmt.Errorf("marshal schema: %w", err)
	}
	if changes == nil {
		changes = []string{}
	}
	changesJSON, err := json.Marshal(changes)
	if err != nil {
		return nil, false, fmt.Errorf("marshal change summary: %w", err)
	}

	now := nowString()
	res, err := s.db.ExecContext(ctx, `INSERT INTO schema_snapshots
		(connection_id, fingerprint, captured_at, schema_json, source, change_summary)
		VALUES (?, ?, ?, ?, ?, ?)`,
		connectionID, fingerprint, now, string(schemaJSON), source, string(changesJSON))
	if err != nil {
		return nil, false, fmt.Errorf("insert schema snapshot: %w", err)
	}
	id, _ := res.LastInsertId()

	return &SchemaSnapshot{
		ID: id, ConnectionID: connectionID, Fingerprint: fingerprint,
		CapturedAt: parseTime(now), Source: source, ChangeSummary: changes, Schema: sc,
	}, true, nil
}

// LatestSchemaSnapshot은 가장 최근 스냅샷을 반환한다. 없으면 nil, nil이다.
// withSchema가 false면 schema_json을 디코딩하지 않아 지문 비교가 저렴하다.
func (s *Store) LatestSchemaSnapshot(ctx context.Context, connectionID string, withSchema bool) (*SchemaSnapshot, error) {
	cols := `id, connection_id, fingerprint, captured_at, source, change_summary`
	if withSchema {
		cols += `, schema_json`
	}
	row := s.db.QueryRowContext(ctx, `SELECT `+cols+` FROM schema_snapshots
		WHERE connection_id = ? ORDER BY id DESC LIMIT 1`, connectionID)

	snap, err := scanSnapshot(row, withSchema)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return snap, nil
}

func (s *Store) GetSchemaSnapshot(ctx context.Context, id int64) (*SchemaSnapshot, error) {
	row := s.db.QueryRowContext(ctx, `SELECT
		id, connection_id, fingerprint, captured_at, source, change_summary, schema_json
		FROM schema_snapshots WHERE id = ?`, id)
	snap, err := scanSnapshot(row, true)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return snap, nil
}

// ListSchemaSnapshots는 커넥션의 스냅샷 이력을 반환한다 (스키마 본문 제외).
func (s *Store) ListSchemaSnapshots(ctx context.Context, connectionID string, limit int) ([]*SchemaSnapshot, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `SELECT
		id, connection_id, fingerprint, captured_at, source, change_summary
		FROM schema_snapshots WHERE connection_id = ?
		ORDER BY id DESC LIMIT ?`, connectionID, limit)
	if err != nil {
		return nil, fmt.Errorf("list schema snapshots: %w", err)
	}
	defer rows.Close()

	out := []*SchemaSnapshot{}
	for rows.Next() {
		snap, err := scanSnapshot(rows, false)
		if err != nil {
			return nil, err
		}
		out = append(out, snap)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate schema snapshots: %w", err)
	}
	return out, nil
}

func scanSnapshot(row interface{ Scan(...any) error }, withSchema bool) (*SchemaSnapshot, error) {
	var snap SchemaSnapshot
	var capturedAt, changesJSON string
	var schemaJSON string

	var err error
	if withSchema {
		err = row.Scan(&snap.ID, &snap.ConnectionID, &snap.Fingerprint,
			&capturedAt, &snap.Source, &changesJSON, &schemaJSON)
	} else {
		err = row.Scan(&snap.ID, &snap.ConnectionID, &snap.Fingerprint,
			&capturedAt, &snap.Source, &changesJSON)
	}
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		return nil, fmt.Errorf("scan schema snapshot: %w", err)
	}

	snap.CapturedAt = parseTime(capturedAt)
	snap.ChangeSummary = []string{}
	_ = json.Unmarshal([]byte(changesJSON), &snap.ChangeSummary)

	if withSchema && schemaJSON != "" {
		var sc schema.Schema
		if err := json.Unmarshal([]byte(schemaJSON), &sc); err != nil {
			return nil, fmt.Errorf("unmarshal snapshot schema: %w", err)
		}
		snap.Schema = &sc
	}
	return &snap, nil
}

// PurgeSchemaSnapshots는 커넥션별로 최근 N개만 남기고 나머지를 삭제한다.
// 시간 기준이 아닌 개수 기준인 이유: 스키마는 자주 바뀌지 않으므로
// 오래된 스냅샷도 "직전 상태"로서 가치가 있다.
func (s *Store) PurgeSchemaSnapshots(ctx context.Context, keepPerConnection int) (int64, error) {
	if keepPerConnection <= 0 {
		keepPerConnection = 50
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM schema_snapshots WHERE id NOT IN (
			SELECT id FROM (
				SELECT id, ROW_NUMBER() OVER (
					PARTITION BY connection_id ORDER BY id DESC) AS rn
				FROM schema_snapshots
			) WHERE rn <= ?
		)`, keepPerConnection)
	if err != nil {
		return 0, fmt.Errorf("purge schema snapshots: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}
