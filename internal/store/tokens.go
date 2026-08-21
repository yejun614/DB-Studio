package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"dbstudio/internal/model"
)

// API 토큰.
//
// 세션과 같은 규칙을 따른다: **해시만 저장하고 원문은 발급 순간에만 존재한다.**
// 다른 점은 수명과 용도다 — 세션은 브라우저의 것이고 짧고 자동으로 갱신되지만,
// 토큰은 프로그램의 것이고 길고 사람이 명시적으로 폐기한다.

// TokenScope는 토큰이 할 수 있는 일의 범위다.
const (
	TokenScopeRead  = "read"
	TokenScopeWrite = "write"
)

func ValidTokenScope(s string) bool { return s == TokenScopeRead || s == TokenScopeWrite }

type APIToken struct {
	ID         string     `json:"id"`
	UserID     string     `json:"userId"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	Scope      string     `json:"scope"`
	CreatedAt  time.Time  `json:"createdAt"`
	ExpiresAt  *time.Time `json:"expiresAt,omitempty"`
	LastUsedAt *time.Time `json:"lastUsedAt,omitempty"`
	LastUsedIP string     `json:"lastUsedIp,omitempty"`
	RevokedAt  *time.Time `json:"revokedAt,omitempty"`
}

// Active는 지금 쓸 수 있는 토큰인지 반환한다.
func (t *APIToken) Active() bool {
	if t.RevokedAt != nil {
		return false
	}
	if t.ExpiresAt != nil && time.Now().After(*t.ExpiresAt) {
		return false
	}
	return true
}

type CreateTokenParams struct {
	UserID string
	Name   string
	Scope  string
	// Token은 원문이다. 해시만 저장하고 이 값은 호출부가 사용자에게 한 번 보여준 뒤 버린다.
	Token     string
	Prefix    string
	ExpiresAt *time.Time
}

func (s *Store) CreateAPIToken(ctx context.Context, p CreateTokenParams) (*APIToken, error) {
	id := uuid.NewString()
	scope := p.Scope
	if !ValidTokenScope(scope) {
		scope = TokenScopeRead
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO api_tokens
		(id, user_id, name, token_hash, prefix, scope, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		id, p.UserID, p.Name, HashToken(p.Token), p.Prefix, scope,
		nowString(), timePtrString(p.ExpiresAt)); err != nil {
		return nil, fmt.Errorf("insert api token: %w", err)
	}
	return s.GetAPIToken(ctx, id)
}

const tokenColumns = `id, user_id, name, prefix, scope, created_at, expires_at,
	last_used_at, last_used_ip, revoked_at`

func scanToken(row interface{ Scan(...any) error }) (*APIToken, error) {
	var t APIToken
	var createdAt string
	var expiresAt, lastUsedAt, revokedAt sql.NullString
	if err := row.Scan(&t.ID, &t.UserID, &t.Name, &t.Prefix, &t.Scope, &createdAt,
		&expiresAt, &lastUsedAt, &t.LastUsedIP, &revokedAt); err != nil {
		return nil, err
	}
	t.CreatedAt = parseTime(createdAt)
	t.ExpiresAt = parseTimePtr(expiresAt)
	t.LastUsedAt = parseTimePtr(lastUsedAt)
	t.RevokedAt = parseTimePtr(revokedAt)
	return &t, nil
}

func (s *Store) GetAPIToken(ctx context.Context, id string) (*APIToken, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+tokenColumns+` FROM api_tokens WHERE id = ?`, id)
	t, err := scanToken(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get api token: %w", err)
	}
	return t, nil
}

// ListAPITokens는 한 사용자의 토큰을 최신순으로 반환한다.
// 폐기된 것도 함께 반환한다 — "언제 폐기했는가"도 기록이다.
func (s *Store) ListAPITokens(ctx context.Context, userID string) ([]*APIToken, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+tokenColumns+` FROM api_tokens WHERE user_id = ? ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, fmt.Errorf("list api tokens: %w", err)
	}
	defer rows.Close()

	out := []*APIToken{}
	for rows.Next() {
		t, err := scanToken(rows)
		if err != nil {
			return nil, fmt.Errorf("scan api token: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// LookupAPIToken은 원문 토큰으로 사용자와 토큰 정보를 찾는다.
//
// 폐기·만료 여부를 여기서 판단하지 않고 그대로 돌려주는 이유: 호출부가
// "없는 토큰"과 "폐기된 토큰"을 구분해 감사 로그에 다르게 남길 수 있어야 한다.
// 다만 응답 메시지는 둘을 구분하지 않는다(토큰의 존재 여부를 알려주지 않는다).
func (s *Store) LookupAPIToken(ctx context.Context, raw string) (*APIToken, *model.User, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+tokenColumns+` FROM api_tokens WHERE token_hash = ?`, HashToken(raw))
	t, err := scanToken(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, fmt.Errorf("lookup api token: %w", err)
	}
	u, err := s.GetUser(ctx, t.UserID)
	if err != nil {
		return nil, nil, err
	}
	return t, u, nil
}

// TouchAPIToken은 마지막 사용 시각과 IP를 기록한다.
//
// 매 요청마다 쓰지 않고 간격을 두는 이유는 세션과 같다 — MCP 클라이언트는 툴 목록을
// 자주 물어보므로, 그때마다 쓰면 메타 DB에 의미 없는 쓰기가 쌓인다.
func (s *Store) TouchAPIToken(ctx context.Context, t *APIToken, ip string, minInterval time.Duration) {
	if t.LastUsedAt != nil && time.Since(*t.LastUsedAt) < minInterval && t.LastUsedIP == ip {
		return
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE api_tokens SET last_used_at = ?, last_used_ip = ? WHERE id = ?`,
		nowString(), ip, t.ID); err != nil {
		return
	}
	now := time.Now().UTC()
	t.LastUsedAt = &now
	t.LastUsedIP = ip
}

// RevokeAPIToken은 토큰을 폐기한다. 행을 지우지 않고 표시만 하는 이유:
// "이 토큰이 언제까지 살아 있었는가"가 사고 조사에 필요하다.
func (s *Store) RevokeAPIToken(ctx context.Context, id, userID string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE api_tokens SET revoked_at = ? WHERE id = ? AND user_id = ? AND revoked_at IS NULL`,
		nowString(), id, userID)
	if err != nil {
		return fmt.Errorf("revoke api token: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteAPIToken은 기록까지 지운다. 폐기와 달리 흔적이 남지 않으므로
// 사용자가 목록을 정리할 때만 쓴다.
func (s *Store) DeleteAPIToken(ctx context.Context, id, userID string) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM api_tokens WHERE id = ? AND user_id = ?`, id, userID)
	if err != nil {
		return fmt.Errorf("delete api token: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
