package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"dbstudio/internal/model"
)

var (
	ErrNotFound      = errors.New("not found")
	ErrDuplicateName = errors.New("name already exists")
)

const userColumns = `u.id, u.username, u.email, u.display_name, u.role, u.password_hash,
	u.must_change_password, u.status, u.last_login_at, u.last_login_ip, u.avatar, u.perms,
	u.created_at, u.updated_at, u.created_by,
	(SELECT version FROM user_avatars WHERE user_id = u.id),
	-- 2단계 인증은 **확인을 마친 등록**만 켜진 것으로 본다.
	-- 시작만 하고 앱에 등록하지 않은 상태를 켜짐으로 세면 그 사람은 로그인할 수 없다.
	EXISTS (SELECT 1 FROM user_totp t WHERE t.user_id = u.id AND t.confirmed_at IS NOT NULL)`

func scanUser(row interface{ Scan(...any) error }) (*model.User, error) {
	var u model.User
	var lastLogin, createdAt, updatedAt sql.NullString
	var createdBy sql.NullString
	var perms string
	var avatarVersion sql.NullInt64
	var mustChange, totpEnabled int
	err := row.Scan(&u.ID, &u.Username, &u.Email, &u.DisplayName, &u.Role, &u.PasswordHash,
		&mustChange, &u.Status, &lastLogin, &u.LastLoginIP, &u.Avatar, &perms,
		&createdAt, &updatedAt, &createdBy, &avatarVersion, &totpEnabled)
	if err != nil {
		return nil, err
	}
	u.Perms = model.PermsFromString(perms)
	u.AvatarVersion = int(avatarVersion.Int64)
	u.MustChangePassword = mustChange != 0
	u.TOTPEnabled = totpEnabled != 0
	u.LastLoginAt = parseTimePtr(lastLogin)
	u.CreatedAt = parseTime(createdAt.String)
	u.UpdatedAt = parseTime(updatedAt.String)
	u.CreatedBy = createdBy.String
	return &u, nil
}

// CountUsers는 전체 사용자 수를 반환한다. 부트스트랩 판단에 사용한다.
func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM users`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count users: %w", err)
	}
	return n, nil
}

// CreateUserParams는 사용자 생성 입력이다.
type CreateUserParams struct {
	Username           string
	Email              string
	DisplayName        string
	Role               model.Role
	PasswordHash       string
	MustChangePassword bool
	CreatedBy          string
}

// CreateUser는 사용자를 생성하고 기본 접근 정책(빈 allowlist)을 함께 만든다.
func (s *Store) CreateUser(ctx context.Context, p CreateUserParams) (*model.User, error) {
	id := uuid.NewString()
	now := nowString()
	mustChange := 0
	if p.MustChangePassword {
		mustChange = 1
	}
	var createdBy any
	if p.CreatedBy != "" {
		createdBy = p.CreatedBy
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `INSERT INTO users
		(id, username, username_lower, email, display_name, role, password_hash,
		 must_change_password, status, created_at, updated_at, created_by)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'active', ?, ?, ?)`,
		id, p.Username, strings.ToLower(p.Username), p.Email, p.DisplayName, string(p.Role),
		p.PasswordHash, mustChange, now, now, createdBy)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrDuplicateName
		}
		return nil, fmt.Errorf("insert user: %w", err)
	}

	// 기본 정책: superadmin은 전체 접근(migrate), 그 외는 빈 allowlist(아무것도 접근 못 함)로 시작.
	// 데이터 능력은 슈퍼 어드민을 제외하면 항상 빈 집합에서 출발한다 — 새 계정이
	// 만들어지는 것만으로 고객 데이터를 읽을 수 있게 되면 안 된다.
	mode, level := model.AccessAllowlist, model.LevelMonitor
	caps := ""
	if p.Role == model.RoleSuperadmin {
		mode, level = model.AccessAll, model.LevelMigrate
		caps = model.CapsToString(model.AllCapabilities())
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO user_db_access (user_id, mode, default_level, default_caps, updated_at) VALUES (?, ?, ?, ?, ?)`,
		id, string(mode), string(level), caps, now); err != nil {
		return nil, fmt.Errorf("insert access policy: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return s.GetUser(ctx, id)
}

func (s *Store) GetUser(ctx context.Context, id string) (*model.User, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users u WHERE u.id = ?`, id)
	u, err := scanUser(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get user: %w", err)
	}
	return u, nil
}

func (s *Store) GetUserByUsername(ctx context.Context, username string) (*model.User, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+userColumns+` FROM users u WHERE u.username_lower = ?`, strings.ToLower(username))
	u, err := scanUser(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get user by username: %w", err)
	}
	return u, nil
}

func (s *Store) ListUsers(ctx context.Context) ([]*model.User, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+userColumns+` FROM users u ORDER BY
			CASE u.role WHEN 'superadmin' THEN 0 WHEN 'admin' THEN 1 ELSE 2 END, u.username_lower`)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	defer rows.Close()

	out := []*model.User{}
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, fmt.Errorf("scan user: %w", err)
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate users: %w", err)
	}
	return out, nil
}

// UpdateUserParams의 nil 필드는 변경하지 않는다.
type UpdateUserParams struct {
	Email       *string
	DisplayName *string
	Role        *model.Role
	Status      *model.UserStatus
	Avatar      *string
	Perms       *[]model.Perm
}

func (s *Store) UpdateUser(ctx context.Context, id string, p UpdateUserParams) (*model.User, error) {
	sets := []string{}
	args := []any{}
	if p.Email != nil {
		sets = append(sets, "email = ?")
		args = append(args, *p.Email)
	}
	if p.DisplayName != nil {
		sets = append(sets, "display_name = ?")
		args = append(args, *p.DisplayName)
	}
	if p.Role != nil {
		sets = append(sets, "role = ?")
		args = append(args, string(*p.Role))
	}
	if p.Status != nil {
		sets = append(sets, "status = ?")
		args = append(args, string(*p.Status))
	}
	if p.Avatar != nil {
		sets = append(sets, "avatar = ?")
		args = append(args, *p.Avatar)
	}
	if p.Perms != nil {
		sets = append(sets, "perms = ?")
		args = append(args, model.PermsToString(*p.Perms))
	}
	if len(sets) == 0 {
		return s.GetUser(ctx, id)
	}
	sets = append(sets, "updated_at = ?")
	args = append(args, nowString(), id)

	res, err := s.db.ExecContext(ctx,
		`UPDATE users SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("update user: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrNotFound
	}
	return s.GetUser(ctx, id)
}

// SetPassword는 비밀번호 해시를 갱신하고 변경 강제 플래그를 설정한다.
func (s *Store) SetPassword(ctx context.Context, id, hash string, mustChange bool) error {
	flag := 0
	if mustChange {
		flag = 1
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE users SET password_hash = ?, must_change_password = ?, updated_at = ? WHERE id = ?`,
		hash, flag, nowString(), id)
	if err != nil {
		return fmt.Errorf("set password: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// TouchLastLogin은 마지막 로그인 시각과 그때의 접속 IP를 기록한다.
//
// 시각과 IP를 한 UPDATE로 함께 쓰는 이유: 둘이 따로 갱신되면 "언제"와 "어디서"가
// 서로 다른 로그인의 것이 될 수 있다. 화면에서 그 둘을 나란히 보여주므로 짝이 맞아야 한다.
func (s *Store) TouchLastLogin(ctx context.Context, id, ip string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE users SET last_login_at = ?, last_login_ip = ? WHERE id = ?`,
		nowString(), ip, id)
	if err != nil {
		return fmt.Errorf("touch last login: %w", err)
	}
	return nil
}

// DeleteUser는 사용자와 연관 세션/권한을 함께 제거한다(FK ON DELETE CASCADE).
func (s *Store) DeleteUser(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete user: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// CountSuperadmins는 활성 슈퍼어드민 수를 반환한다. 마지막 슈퍼어드민 보호에 사용한다.
func (s *Store) CountSuperadmins(ctx context.Context, activeOnly bool) (int, error) {
	q := `SELECT count(*) FROM users WHERE role = 'superadmin'`
	if activeOnly {
		q += ` AND status = 'active'`
	}
	var n int
	if err := s.db.QueryRowContext(ctx, q).Scan(&n); err != nil {
		return 0, fmt.Errorf("count superadmins: %w", err)
	}
	return n, nil
}

func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") ||
		strings.Contains(msg, "constraint failed: UNIQUE")
}
