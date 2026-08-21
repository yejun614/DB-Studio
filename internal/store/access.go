package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"dbstudio/internal/model"
)

// 접근 정책은 두 층으로 저장된다: 서버 단위와 커넥션(DB) 단위.
// 판정에서 좁은 쪽이 이긴다 — 그 규칙은 auth.resolveWithPolicy에 있다.
// 여기서는 저장과 조회만 한다.

// GetAccessPolicy는 사용자의 접근 정책 전체를 읽는다.
// 정책 행이 없으면 가장 보수적인 기본값(빈 allowlist)을 반환한다.
func (s *Store) GetAccessPolicy(ctx context.Context, userID string) (*model.AccessPolicy, error) {
	p := &model.AccessPolicy{
		UserID:             userID,
		Mode:               model.AccessAllowlist,
		DefaultLevel:       model.LevelMonitor,
		Items:              []string{},
		Capabilities:       map[string]model.Level{},
		DefaultCaps:        []model.Capability{},
		CapOverrides:       map[string][]model.Capability{},
		ServerItems:        []string{},
		ServerCapabilities: map[string]model.Level{},
		ServerCapOverrides: map[string][]model.Capability{},
	}

	var mode, level, defaultCaps, updatedAt string
	err := s.db.QueryRowContext(ctx,
		`SELECT mode, default_level, default_caps, updated_at FROM user_db_access WHERE user_id = ?`, userID).
		Scan(&mode, &level, &defaultCaps, &updatedAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// 기본값 유지
	case err != nil:
		return nil, fmt.Errorf("get access policy: %w", err)
	default:
		p.Mode = model.AccessMode(mode)
		p.DefaultLevel = model.Level(level)
		p.DefaultCaps = model.CapsFromString(defaultCaps)
		p.UpdatedAt = parseTime(updatedAt)
	}

	if p.Items, err = s.accessIDs(ctx,
		`SELECT connection_id FROM user_db_access_items WHERE user_id = ?`, userID); err != nil {
		return nil, err
	}
	if p.ServerItems, err = s.accessIDs(ctx,
		`SELECT server_id FROM user_server_access_items WHERE user_id = ?`, userID); err != nil {
		return nil, err
	}
	if p.Capabilities, err = s.accessLevels(ctx,
		`SELECT connection_id, level FROM user_db_capability WHERE user_id = ?`, userID); err != nil {
		return nil, err
	}
	if p.ServerCapabilities, err = s.accessLevels(ctx,
		`SELECT server_id, level FROM user_server_capability WHERE user_id = ?`, userID); err != nil {
		return nil, err
	}
	if p.CapOverrides, err = s.accessCaps(ctx,
		`SELECT connection_id, caps FROM user_db_data_caps WHERE user_id = ?`, userID); err != nil {
		return nil, err
	}
	if p.ServerCapOverrides, err = s.accessCaps(ctx,
		`SELECT server_id, caps FROM user_server_data_caps WHERE user_id = ?`, userID); err != nil {
		return nil, err
	}
	return p, nil
}

func (s *Store) accessIDs(ctx context.Context, query, userID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, query, userID)
	if err != nil {
		return nil, fmt.Errorf("list access items: %w", err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan access item: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (s *Store) accessLevels(ctx context.Context, query, userID string) (map[string]model.Level, error) {
	rows, err := s.db.QueryContext(ctx, query, userID)
	if err != nil {
		return nil, fmt.Errorf("list levels: %w", err)
	}
	defer rows.Close()
	out := map[string]model.Level{}
	for rows.Next() {
		var id, lv string
		if err := rows.Scan(&id, &lv); err != nil {
			return nil, fmt.Errorf("scan level: %w", err)
		}
		out[id] = model.Level(lv)
	}
	return out, rows.Err()
}

func (s *Store) accessCaps(ctx context.Context, query, userID string) (map[string][]model.Capability, error) {
	rows, err := s.db.QueryContext(ctx, query, userID)
	if err != nil {
		return nil, fmt.Errorf("list data caps: %w", err)
	}
	defer rows.Close()
	out := map[string][]model.Capability{}
	for rows.Next() {
		var id, caps string
		if err := rows.Scan(&id, &caps); err != nil {
			return nil, fmt.Errorf("scan data cap: %w", err)
		}
		out[id] = model.CapsFromString(caps)
	}
	return out, rows.Err()
}

// SetAccessPolicy는 사용자의 접근 정책을 통째로 교체한다.
//
// 부분 갱신이 아니라 전체 교체인 이유: 권한은 "지금 무엇이 허용되어 있는가"를 화면에서
// 통째로 보고 정하는 값이다. 부분 갱신이면 화면에 없던 옛 항목이 남아 조용히 살아 있게 된다.
func (s *Store) SetAccessPolicy(ctx context.Context, p *model.AccessPolicy) error {
	now := nowString()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `INSERT INTO user_db_access (user_id, mode, default_level, default_caps, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (user_id) DO UPDATE SET mode = excluded.mode,
			default_level = excluded.default_level, default_caps = excluded.default_caps,
			updated_at = excluded.updated_at`,
		p.UserID, string(p.Mode), string(p.DefaultLevel), model.CapsToString(p.DefaultCaps), now); err != nil {
		return fmt.Errorf("upsert access policy: %w", err)
	}

	// mode=all에서는 목록이 의미 없으므로 저장하지 않는다.
	items, serverItems := p.Items, p.ServerItems
	if p.Mode == model.AccessAll {
		items, serverItems = nil, nil
	}
	writes := []struct {
		table, idCol string
		ids          []string
		levels       map[string]model.Level
		caps         map[string][]model.Capability
	}{
		{table: "user_db_access_items", idCol: "connection_id", ids: items},
		{table: "user_server_access_items", idCol: "server_id", ids: serverItems},
		{table: "user_db_capability", idCol: "connection_id", levels: p.Capabilities},
		{table: "user_server_capability", idCol: "server_id", levels: p.ServerCapabilities},
		{table: "user_db_data_caps", idCol: "connection_id", caps: p.CapOverrides},
		{table: "user_server_data_caps", idCol: "server_id", caps: p.ServerCapOverrides},
	}

	for _, w := range writes {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM `+w.table+` WHERE user_id = ?`, p.UserID); err != nil {
			return fmt.Errorf("clear %s: %w", w.table, err)
		}
		for _, id := range w.ids {
			if _, err := tx.ExecContext(ctx,
				`INSERT OR IGNORE INTO `+w.table+` (user_id, `+w.idCol+`) VALUES (?, ?)`,
				p.UserID, id); err != nil {
				return fmt.Errorf("insert into %s: %w", w.table, err)
			}
		}
		for id, level := range w.levels {
			if _, err := tx.ExecContext(ctx,
				`INSERT OR REPLACE INTO `+w.table+` (user_id, `+w.idCol+`, level, updated_at) VALUES (?, ?, ?, ?)`,
				p.UserID, id, string(level), now); err != nil {
				return fmt.Errorf("insert into %s: %w", w.table, err)
			}
		}
		for id, caps := range w.caps {
			if _, err := tx.ExecContext(ctx,
				`INSERT OR REPLACE INTO `+w.table+` (user_id, `+w.idCol+`, caps, updated_at) VALUES (?, ?, ?, ?)`,
				p.UserID, id, model.CapsToString(caps), now); err != nil {
				return fmt.Errorf("insert into %s: %w", w.table, err)
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}
