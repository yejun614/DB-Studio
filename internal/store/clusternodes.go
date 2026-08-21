package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// 클러스터 노드 등록부.
//
// 마스터의 메타 DB에 있고 복제되므로 어느 노드에 접속해도 같은 목록이 보인다.
// 이 표에 쓰는 것은 언제나 마스터다(리플리카는 하트비트로 알리기만 한다).

// ClusterNode는 클러스터에 참여한 노드 하나다.
type ClusterNode struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Role       string    `json:"role"`
	Address    string    `json:"address"`
	Version    string    `json:"version"`
	Platform   string    `json:"platform"`
	JoinedAt   time.Time `json:"joinedAt"`
	LastSeenAt time.Time `json:"lastSeenAt"`
	AppliedSeq int64     `json:"appliedSeq"`
	// HostSnapshot은 그 노드가 도는 컴퓨터의 최신 상태다(hostmon.Snapshot JSON).
	HostSnapshot string     `json:"-"`
	HostAt       *time.Time `json:"hostAt,omitempty"`
	Status       string     `json:"status"`
}

const (
	NodeRoleMaster  = "master"
	NodeRoleReplica = "replica"
)

const nodeCols = `id, name, role, address, version, platform, joined_at, last_seen_at,
	applied_seq, host_snapshot, host_at, status`

func scanNode(sc interface{ Scan(...any) error }) (*ClusterNode, error) {
	var n ClusterNode
	var joined, seen, hostAt string
	if err := sc.Scan(&n.ID, &n.Name, &n.Role, &n.Address, &n.Version, &n.Platform,
		&joined, &seen, &n.AppliedSeq, &n.HostSnapshot, &hostAt, &n.Status); err != nil {
		return nil, err
	}
	n.JoinedAt = parseTime(joined)
	n.LastSeenAt = parseTime(seen)
	if hostAt != "" {
		t := parseTime(hostAt)
		n.HostAt = &t
	}
	return &n, nil
}

// UpsertClusterNode는 노드 등록/갱신이다(마스터 자신도 여기에 들어간다).
func (s *Store) UpsertClusterNode(ctx context.Context, n ClusterNode) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	joined := now
	if !n.JoinedAt.IsZero() {
		joined = n.JoinedAt.UTC().Format(time.RFC3339Nano)
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO cluster_nodes (id, name, role, address, version, platform,
			joined_at, last_seen_at, applied_seq, host_snapshot, host_at, status)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, '{}', '', 'active')
		ON CONFLICT (id) DO UPDATE SET
			name = excluded.name, role = excluded.role, address = excluded.address,
			version = excluded.version, platform = excluded.platform,
			last_seen_at = excluded.last_seen_at, status = 'active'`,
		n.ID, n.Name, n.Role, n.Address, n.Version, n.Platform, joined, now, n.AppliedSeq)
	if err != nil {
		return fmt.Errorf("upsert cluster node: %w", err)
	}
	return nil
}

// TouchClusterNode는 하트비트를 기록한다.
//
// 호스트 스냅샷을 함께 받는 이유: 노드마다 따로 지표를 조회하러 다니면 마스터가 모든
// 노드에 접속할 수 있어야 한다. 하트비트는 어차피 노드가 마스터로 보내는 것이므로,
// 방화벽이 한쪽 방향만 열려 있어도 각 서버의 상태를 한 화면에 모을 수 있다.
func (s *Store) TouchClusterNode(ctx context.Context, id string, appliedSeq int64, hostSnapshot string) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	hostAt := ""
	if strings.TrimSpace(hostSnapshot) != "" && hostSnapshot != "{}" {
		hostAt = now
	} else {
		hostSnapshot = "{}"
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE cluster_nodes
		SET last_seen_at = ?, applied_seq = ?, status = 'active',
			host_snapshot = CASE WHEN ? = '' THEN host_snapshot ELSE ? END,
			host_at       = CASE WHEN ? = '' THEN host_at       ELSE ? END
		WHERE id = ?`, now, appliedSeq, hostAt, hostSnapshot, hostAt, hostAt, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ListClusterNodes는 노드 목록이다.
func (s *Store) ListClusterNodes(ctx context.Context) ([]*ClusterNode, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+nodeCols+`
		FROM cluster_nodes ORDER BY role = 'master' DESC, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*ClusterNode{}
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// GetClusterNode는 노드 하나다.
func (s *Store) GetClusterNode(ctx context.Context, id string) (*ClusterNode, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+nodeCols+` FROM cluster_nodes WHERE id = ?`, id)
	n, err := scanNode(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return n, err
}

// RemoveClusterNode는 노드를 목록에서 내린다.
//
// 지우지 않고 표시만 바꾸는 이유: 이벤트와 커넥션이 node_id로 그 노드를 가리키고 있고,
// 행이 사라지면 "어느 서버에서 난 일인지"를 영영 알 수 없게 된다.
func (s *Store) RemoveClusterNode(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE cluster_nodes SET status = 'left' WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// StaleClusterNodes는 기준 시간보다 오래 소식이 없는 활성 노드다.
func (s *Store) StaleClusterNodes(ctx context.Context, olderThan time.Duration) ([]*ClusterNode, error) {
	cutoff := time.Now().UTC().Add(-olderThan).Format(time.RFC3339Nano)
	rows, err := s.db.QueryContext(ctx, `SELECT `+nodeCols+`
		FROM cluster_nodes WHERE status = 'active' AND last_seen_at < ? ORDER BY name`, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*ClusterNode{}
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}
