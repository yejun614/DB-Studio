package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"dbstudio/internal/model"
)

// 커넥션은 "관리 대상 DB 하나"다. 접속 정보와 자격증명은 서버(servers.go)에 있고
// 여기서는 조인으로 채운다 — 이 구조체를 쓰는 쪽(권한·지표·ERD·마이그레이션·백업)은
// 서버가 생긴 것을 몰라도 된다.

const connColumns = `c.id, c.name, s.kind, c.environment, s.host, s.port, c.database_name,
	s.options, c.tags, c.note, c.enabled, s.enabled,
	c.last_check_at, c.last_check_ok, c.last_check_msg,
	c.created_by, c.created_at, c.updated_at, COALESCE(sec.username, ''),
	c.server_id, s.name, c.node_id`

const connFrom = ` FROM connections c
	JOIN servers s ON s.id = c.server_id
	LEFT JOIN server_secrets sec ON sec.server_id = c.server_id`

func scanConnection(row interface{ Scan(...any) error }) (*model.Connection, error) {
	var c model.Connection
	var options, tags, createdAt, updatedAt string
	var lastCheckAt, createdBy sql.NullString
	var lastCheckOK sql.NullInt64
	var enabled, serverEnabled int
	err := row.Scan(&c.ID, &c.Name, &c.Kind, &c.Environment, &c.Host, &c.Port, &c.DatabaseName,
		&options, &tags, &c.Note, &enabled, &serverEnabled,
		&lastCheckAt, &lastCheckOK, &c.LastCheckMsg,
		&createdBy, &createdAt, &updatedAt, &c.Username,
		&c.ServerID, &c.ServerName, &c.NodeID)
	if err != nil {
		return nil, err
	}
	c.Options = model.UnmarshalOptions(options)
	c.Tags = model.TagsFromString(tags)
	c.SelfEnabled = enabled != 0
	c.ServerEnabled = serverEnabled != 0
	// Enabled는 실효값이다. 서버를 끄면 그 아래 DB가 전부 꺼져야 하고,
	// 앱 곳곳의 `if !conn.Enabled` 관문이 그것을 그대로 따르게 하기 위해서다.
	c.Enabled = c.SelfEnabled && c.ServerEnabled
	c.LastCheckAt = parseTimePtr(lastCheckAt)
	if lastCheckOK.Valid {
		ok := lastCheckOK.Int64 != 0
		c.LastCheckOK = &ok
	}
	c.CreatedBy = createdBy.String
	c.CreatedAt = parseTime(createdAt)
	c.UpdatedAt = parseTime(updatedAt)
	return &c, nil
}

// SaveConnectionParams는 커넥션(관리 대상 DB) 생성/수정 입력이다.
//
// 접속 정보와 자격증명이 없는 것이 요점이다 — 그것들은 서버가 갖는다.
type SaveConnectionParams struct {
	ServerID     string
	Name         string
	Environment  model.Environment
	DatabaseName string
	// NodeID는 이 DB에 접속할 클러스터 노드다(비우면 요청을 받은 노드).
	NodeID  string
	Tags    []string
	Note    string
	Enabled bool
	ActorID string
}

func (s *Store) CreateConnection(ctx context.Context, p SaveConnectionParams) (*model.Connection, error) {
	id := uuid.NewString()
	now := nowString()
	var createdBy any
	if p.ActorID != "" {
		createdBy = p.ActorID
	}

	_, err := s.db.ExecContext(ctx, `INSERT INTO connections
		(id, server_id, name, name_lower, environment, database_name, tags, note,
		 enabled, created_by, created_at, updated_at, node_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, p.ServerID, p.Name, strings.ToLower(p.Name), string(p.Environment),
		p.DatabaseName, model.TagsToString(p.Tags), p.Note,
		boolInt(p.Enabled), createdBy, now, now, p.NodeID)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrDuplicateName
		}
		return nil, fmt.Errorf("insert connection: %w", err)
	}
	return s.GetConnection(ctx, id)
}

func (s *Store) UpdateConnection(ctx context.Context, id string, p SaveConnectionParams) (*model.Connection, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE connections SET
		name = ?, name_lower = ?, environment = ?, database_name = ?,
		tags = ?, note = ?, enabled = ?, updated_at = ?, node_id = ? WHERE id = ?`,
		p.Name, strings.ToLower(p.Name), string(p.Environment), p.DatabaseName,
		model.TagsToString(p.Tags), p.Note, boolInt(p.Enabled), nowString(), p.NodeID, id)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrDuplicateName
		}
		return nil, fmt.Errorf("update connection: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrNotFound
	}
	return s.GetConnection(ctx, id)
}

// ListConnectionsByServer는 한 서버에 속한 DB만 반환한다.
func (s *Store) ListConnectionsByServer(ctx context.Context, serverID string) ([]*model.Connection, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+connColumns+connFrom+
		` WHERE c.server_id = ? ORDER BY c.name_lower`, serverID)
	if err != nil {
		return nil, fmt.Errorf("list connections by server: %w", err)
	}
	defer rows.Close()

	out := []*model.Connection{}
	for rows.Next() {
		c, err := scanConnection(rows)
		if err != nil {
			return nil, fmt.Errorf("scan connection: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) GetConnection(ctx context.Context, id string) (*model.Connection, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+connColumns+connFrom+` WHERE c.id = ?`, id)
	c, err := scanConnection(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get connection: %w", err)
	}
	return c, nil
}

func (s *Store) ListConnections(ctx context.Context) ([]*model.Connection, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+connColumns+connFrom+
		` ORDER BY CASE c.environment WHEN 'prod' THEN 0 ELSE 1 END, c.name_lower`)
	if err != nil {
		return nil, fmt.Errorf("list connections: %w", err)
	}
	defer rows.Close()

	out := []*model.Connection{}
	for rows.Next() {
		c, err := scanConnection(rows)
		if err != nil {
			return nil, fmt.Errorf("scan connection: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate connections: %w", err)
	}
	return out, nil
}

// ImpactItem은 커넥션을 지웠을 때 함께 없어지는(또는 연결이 끊기는) 것 하나다.
type ImpactItem struct {
	Key   string `json:"key"`
	Count int    `json:"count"`
	// Kept가 참이면 기록 자체는 남고 커넥션 연결만 끊긴다.
	Kept bool `json:"kept"`
}

// impactSources는 세는 대상이다. 커넥션을 참조하는 표가 늘면 여기에 한 줄 더한다.
//
// 이 목록이 화면의 경고문이 된다. 삭제는 되돌릴 수 없는데 무엇이 함께 사라지는지
// 말해 주지 않으면, ERD를 반나절 그려 둔 사람이 그 사실을 삭제 뒤에 알게 된다.
var impactSources = []struct {
	key   string
	table string
	kept  bool
}{
	{"erd", "erd_documents", false},
	{"migration", "migrations", false},
	{"version", "schema_versions", false},
	{"snapshot", "schema_snapshots", false},
	{"rule", "monitor_rules", false},
	{"event", "events", false},
	{"trigger", "macro_triggers", false},
	{"vcs", "vcs_integrations", false},
	{"metric", "metric_samples", false},
	{"access", "user_db_access_items", false},
	{"backup", "backups", true},
	{"ai", "ai_sessions", true},
}

// ConnectionImpact는 커넥션 삭제로 영향을 받는 것들을 센다.
func (s *Store) ConnectionImpact(ctx context.Context, id string) ([]ImpactItem, error) {
	parts := make([]string, 0, len(impactSources))
	args := make([]any, 0, len(impactSources))
	for _, src := range impactSources {
		// 표 이름은 위 목록의 상수이므로 값 바인딩 대상이 아니다.
		parts = append(parts,
			fmt.Sprintf("SELECT '%s' AS k, COUNT(*) AS n FROM %s WHERE connection_id = ?", src.key, src.table))
		args = append(args, id)
	}

	rows, err := s.db.QueryContext(ctx, strings.Join(parts, " UNION ALL "), args...)
	if err != nil {
		return nil, fmt.Errorf("connection impact: %w", err)
	}
	defer rows.Close()

	counts := map[string]int{}
	for rows.Next() {
		var k string
		var n int
		if err := rows.Scan(&k, &n); err != nil {
			return nil, fmt.Errorf("connection impact: %w", err)
		}
		counts[k] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("connection impact: %w", err)
	}

	// 0인 것은 내보내지 않는다. "ERD 0개가 삭제됩니다"를 늘어놓으면
	// 정작 0이 아닌 한 줄이 묻힌다.
	out := make([]ImpactItem, 0, len(impactSources))
	for _, src := range impactSources {
		if n := counts[src.key]; n > 0 {
			out = append(out, ImpactItem{Key: src.key, Count: n, Kept: src.kept})
		}
	}
	return out, nil
}

func (s *Store) DeleteConnection(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM connections WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete connection: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// GetSecret은 이 DB에 접속할 자격증명을 반환한다. 실제로는 소속 서버의 것이다.
//
// 시그니처를 커넥션 ID 그대로 둔 이유: 호출부(20여 곳)가 보는 단위는 여전히 "이 DB"이고,
// 자격증명이 어디에 저장되는지는 저장 계층의 사정이다.
func (s *Store) GetSecret(ctx context.Context, connectionID string) (*model.Secret, error) {
	var username, pwEnc, extraEnc string
	err := s.db.QueryRowContext(ctx,
		`SELECT sec.username, sec.password_enc, sec.extra_enc
		 FROM connections c JOIN server_secrets sec ON sec.server_id = c.server_id
		 WHERE c.id = ?`, connectionID).Scan(&username, &pwEnc, &extraEnc)
	if errors.Is(err, sql.ErrNoRows) {
		return &model.Secret{Extra: map[string]string{}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get secret: %w", err)
	}
	return s.openSecret(username, pwEnc, extraEnc)
}

// RecordConnectionCheck는 연결 테스트 결과를 커넥션에 기록한다.
//
// 리플리카에서는 기록하지 않는다. 담당 노드로 넘어온 접속 테스트가 여기서 실행될 수
// 있는데, 그 결과를 로컬에 쓰면 다음 복제에서 사라진다. 테스트를 누른 사람은 응답으로
// 결과를 곧바로 보고, 목록의 "마지막 확인"은 마스터의 폴러가 유지한다.
func (s *Store) RecordConnectionCheck(ctx context.Context, id string, ok bool, msg string) error {
	if s.IsReplica() {
		return nil
	}
	flag := 0
	if ok {
		flag = 1
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE connections SET last_check_at = ?, last_check_ok = ?, last_check_msg = ? WHERE id = ?`,
		nowString(), flag, truncate(msg, 1000), id)
	if err != nil {
		return fmt.Errorf("record check: %w", err)
	}
	return nil
}

func (s *Store) sealExtra(extra map[string]string) (string, error) {
	if len(extra) == 0 {
		return "", nil
	}
	b, err := json.Marshal(extra)
	if err != nil {
		return "", fmt.Errorf("marshal extra: %w", err)
	}
	sealed, err := s.secret.Seal(string(b))
	if err != nil {
		return "", fmt.Errorf("seal extra: %w", err)
	}
	return sealed, nil
}
