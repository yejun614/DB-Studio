package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// 업로드한 프로필 이미지.
//
// users 테이블이 아니라 별도 테이블에 두는 이유는 users가 로그인마다 읽히는
// 뜨거운 행이기 때문이다. 아이콘을 고른 사람의 행을 BLOB 열이 있는 테이블에서
// 읽고 싶지 않다.

type AvatarImage struct {
	UserID    string    `json:"userId"`
	Mime      string    `json:"mime"`
	Bytes     []byte    `json:"-"`
	Size      int       `json:"size"`
	Width     int       `json:"width"`
	Height    int       `json:"height"`
	Source    string    `json:"source"`
	SourceURI string    `json:"sourceUri,omitempty"`
	Version   int       `json:"version"`
	UpdatedAt time.Time `json:"updatedAt"`
}

type SaveAvatarParams struct {
	UserID    string
	Mime      string
	Bytes     []byte
	Width     int
	Height    int
	Source    string
	SourceURI string
}

// SaveUserAvatar는 이미지를 저장하고 users.avatar를 'upload'로 맞춘다.
//
// 두 테이블을 한 트랜잭션으로 묶는 것이 중요하다. 이미지만 저장되고 users.avatar가
// 바뀌지 않으면 아무 일도 일어나지 않은 것처럼 보이고, 반대면 깨진 이미지가 그려진다.
func (s *Store) SaveUserAvatar(ctx context.Context, p SaveAvatarParams) (int, error) {
	now := nowString()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()

	// version은 이미지가 바뀔 때마다 올라간다. 이미지 URL에 붙여 캐시를 무효화한다 —
	// 경로가 그대로면 사진을 바꿔도 브라우저가 예전 것을 계속 보여준다.
	var version int
	err = tx.QueryRowContext(ctx,
		`SELECT COALESCE(version, 0) + 1 FROM user_avatars WHERE user_id = ?`, p.UserID).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		version = 1
	} else if err != nil {
		return 0, fmt.Errorf("next avatar version: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `INSERT INTO user_avatars
		(user_id, mime, bytes, size, width, height, source, source_uri, version, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (user_id) DO UPDATE SET
			mime = excluded.mime, bytes = excluded.bytes, size = excluded.size,
			width = excluded.width, height = excluded.height, source = excluded.source,
			source_uri = excluded.source_uri, version = excluded.version,
			updated_at = excluded.updated_at`,
		p.UserID, p.Mime, p.Bytes, len(p.Bytes), p.Width, p.Height, p.Source, p.SourceURI,
		version, now); err != nil {
		return 0, fmt.Errorf("save avatar: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE users SET avatar = 'upload', updated_at = ? WHERE id = ?`, now, p.UserID); err != nil {
		return 0, fmt.Errorf("set avatar flag: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	return version, nil
}

// GetUserAvatar는 이미지를 읽는다. withBytes가 false면 메타데이터만 읽는다 —
// 목록 화면이 크기를 알고 싶을 때 수백 KB를 함께 읽을 이유가 없다.
func (s *Store) GetUserAvatar(ctx context.Context, userID string, withBytes bool) (*AvatarImage, error) {
	cols := `user_id, mime, size, width, height, source, source_uri, version, updated_at`
	if withBytes {
		cols = `user_id, mime, size, width, height, source, source_uri, version, updated_at, bytes`
	}
	row := s.db.QueryRowContext(ctx, `SELECT `+cols+` FROM user_avatars WHERE user_id = ?`, userID)

	var a AvatarImage
	var updatedAt string
	targets := []any{&a.UserID, &a.Mime, &a.Size, &a.Width, &a.Height, &a.Source,
		&a.SourceURI, &a.Version, &updatedAt}
	if withBytes {
		targets = append(targets, &a.Bytes)
	}
	if err := row.Scan(targets...); errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	} else if err != nil {
		return nil, fmt.Errorf("get avatar: %w", err)
	}
	a.UpdatedAt = parseTime(updatedAt)
	return &a, nil
}

// DeleteUserAvatar는 이미지를 지우고 아이콘 선택을 초기화한다.
func (s *Store) DeleteUserAvatar(ctx context.Context, userID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `DELETE FROM user_avatars WHERE user_id = ?`, userID); err != nil {
		return fmt.Errorf("delete avatar: %w", err)
	}
	// avatar가 'upload'였던 경우만 비운다. 아이콘을 고른 뒤 옛 업로드를 지우는
	// 경우에 그 선택까지 날려서는 안 된다.
	if _, err := tx.ExecContext(ctx,
		`UPDATE users SET avatar = '', updated_at = ? WHERE id = ? AND avatar = 'upload'`,
		nowString(), userID); err != nil {
		return fmt.Errorf("clear avatar flag: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}
