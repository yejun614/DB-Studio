package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"dbstudio/internal/model"
)

// HashToken은 세션 토큰을 저장용 해시로 변환한다.
// DB에는 해시만 두어, 메타 DB가 유출되어도 세션을 재사용할 수 없게 한다.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func (s *Store) CreateSession(ctx context.Context, token, userID string, ttl time.Duration, ip, ua string) (*model.Session, error) {
	now := time.Now().UTC()
	sess := &model.Session{
		ID:         HashToken(token),
		UserID:     userID,
		CreatedAt:  now,
		ExpiresAt:  now.Add(ttl),
		LastSeenAt: now,
		IP:         ip,
		UserAgent:  truncate(ua, 512),
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO sessions
		(id, user_id, created_at, expires_at, last_seen_at, ip, user_agent)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		sess.ID, sess.UserID,
		formatTime(sess.CreatedAt),
		formatTime(sess.ExpiresAt),
		formatTime(sess.LastSeenAt),
		sess.IP, sess.UserAgent)
	if err != nil {
		return nil, fmt.Errorf("insert session: %w", err)
	}
	return sess, nil
}

// LookupSession은 토큰으로 세션과 사용자를 한 번에 조회한다.
// 만료되었거나 사용자가 비활성이면 ErrNotFound를 반환한다.
func (s *Store) LookupSession(ctx context.Context, token string) (*model.Session, *model.User, error) {
	id := HashToken(token)
	row := s.db.QueryRowContext(ctx, `SELECT
		s.id, s.user_id, s.created_at, s.expires_at, s.last_seen_at, s.ip, s.user_agent,
		u.id, u.username, u.email, u.display_name, u.role, u.password_hash,
		u.must_change_password, u.status, u.last_login_at, u.last_login_ip, u.avatar,
		-- perms를 빠뜨리면 모든 인증 요청이 "전역 권한 없음"으로 보인다.
		-- 슈퍼 어드민은 역할로 통과하므로 증상이 멤버에게만 나타나고,
		-- 권한을 부여한 화면에서는 값이 제대로 보여서 원인을 찾기 어렵다.
		u.perms,
		u.created_at, u.updated_at, u.created_by,
		-- 2단계 인증 등록 여부. 의무화 정책이 켜져 있으면 미들웨어가 이 값으로
		-- 미등록 사용자를 걸러내므로, 모든 인증 요청이 이것을 알아야 한다.
		EXISTS (SELECT 1 FROM user_totp t WHERE t.user_id = u.id AND t.confirmed_at IS NOT NULL)
		FROM sessions s JOIN users u ON u.id = s.user_id WHERE s.id = ?`, id)

	var sess model.Session
	var sCreated, sExpires, sLastSeen string
	var u model.User
	var mustChange, totpEnabled int
	var perms string
	var lastLogin, uCreated, uUpdated, createdBy sql.NullString

	err := row.Scan(&sess.ID, &sess.UserID, &sCreated, &sExpires, &sLastSeen, &sess.IP, &sess.UserAgent,
		&u.ID, &u.Username, &u.Email, &u.DisplayName, &u.Role, &u.PasswordHash,
		&mustChange, &u.Status, &lastLogin, &u.LastLoginIP, &u.Avatar, &perms,
		&uCreated, &uUpdated, &createdBy, &totpEnabled)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, fmt.Errorf("lookup session: %w", err)
	}

	sess.CreatedAt = parseTime(sCreated)
	sess.ExpiresAt = parseTime(sExpires)
	sess.LastSeenAt = parseTime(sLastSeen)

	if time.Now().UTC().After(sess.ExpiresAt) {
		// 만료 세션은 즉시 정리하고 없는 것으로 취급한다.
		_, _ = s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, id)
		return nil, nil, ErrNotFound
	}
	if model.UserStatus(u.Status) == model.UserDisabled {
		return nil, nil, ErrNotFound
	}

	u.MustChangePassword = mustChange != 0
	u.TOTPEnabled = totpEnabled != 0
	u.Perms = model.PermsFromString(perms)
	u.LastLoginAt = parseTimePtr(lastLogin)
	u.CreatedAt = parseTime(uCreated.String)
	u.UpdatedAt = parseTime(uUpdated.String)
	u.CreatedBy = createdBy.String
	return &sess, &u, nil
}

// TouchSession은 last_seen_at을 갱신한다. 매 요청마다 쓰기를 유발하지 않도록
// 마지막 갱신이 minInterval보다 오래된 경우에만 UPDATE를 실행한다.
func (s *Store) TouchSession(ctx context.Context, sess *model.Session, minInterval time.Duration) {
	// 리플리카에서는 쓰지 않는다. 여기서 갱신해 봐야 다음 복제 때 마스터의 값으로
	// 덮이므로, 쓰기 경합만 만들고 남는 것이 없다.
	if s.IsReplica() {
		return
	}
	if time.Since(sess.LastSeenAt) < minInterval {
		return
	}
	_, _ = s.db.ExecContext(ctx, `UPDATE sessions SET last_seen_at = ? WHERE id = ?`,
		nowString(), sess.ID)
}

func (s *Store) DeleteSession(ctx context.Context, token string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, HashToken(token))
	if err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	return nil
}

// DeleteUserSessions는 한 사용자의 모든 세션을 무효화한다.
// 역할 변경, 비활성화, 비밀번호 변경 시 호출한다.
func (s *Store) DeleteUserSessions(ctx context.Context, userID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, userID)
	if err != nil {
		return fmt.Errorf("delete user sessions: %w", err)
	}
	return nil
}

// PurgeExpiredSessions는 만료 세션을 정리하고 삭제 건수를 반환한다.
func (s *Store) PurgeExpiredSessions(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at < ?`, nowString())
	if err != nil {
		return 0, fmt.Errorf("purge sessions: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
