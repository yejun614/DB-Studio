package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// 프로젝트는 자원의 울타리다.
//
// 커넥션과 독립 ERD 초안과 용어 사전이 프로젝트에 속하고, 그 아래 달린 것들
// (마이그레이션·버전·스냅샷·백업·구조 문서)은 커넥션을 따라 저절로 딸려 간다.
// 그래서 이 파일이 아는 표는 셋뿐이다 — 나머지는 알 필요가 없다.
//
// 서버 컴퓨터·매크로·클러스터·사용자는 프로젝트 밖에 있다. 한 대의 서버가 여러
// 프로젝트의 DB를 담고 매크로 하나가 두 프로젝트를 오갈 수 있어서, 그런 것을
// 프로젝트에 매면 "어느 프로젝트의 권한으로 도는가"라는 답 없는 물음이 생긴다.

// DefaultProjectID는 프로젝트가 생기기 전부터 있던 자원이 들어간 곳이다(0037).
// 새 설치에는 없다 — 옮길 것이 없으면 만들지 않는다.
const DefaultProjectID = "default"

type Project struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Note string `json:"note,omitempty"`

	CreatedBy   string `json:"createdBy,omitempty"`
	CreatedName string `json:"createdName,omitempty"`
	CreatedAt   string `json:"createdAt"`
	UpdatedAt   string `json:"updatedAt"`

	// 아래 셋은 목록에서만 채운다. "지워도 되는 프로젝트인가"를 화면이 눌러 보기
	// 전에 알아야 하고, 사람은 이름보다 규모로 프로젝트를 알아본다.
	Connections int `json:"connections"`
	Documents   int `json:"documents"`
	Members     int `json:"members"`
}

// ProjectMember는 프로젝트를 볼 수 있는 사람 하나다.
type ProjectMember struct {
	UserID  string `json:"userId"`
	Name    string `json:"name"`
	Login   string `json:"login"`
	Role    string `json:"role"`
	Status  string `json:"status"`
	AddedAt string `json:"addedAt"`
}

var ErrProjectInUse = errors.New("project has resources")

const projectSelect = `SELECT p.id, p.name, p.note, COALESCE(p.created_by, ''),
	COALESCE(u.display_name, u.username, ''), p.created_at, p.updated_at,
	(SELECT count(*) FROM connections c WHERE c.project_id = p.id),
	(SELECT count(*) FROM erd_documents d WHERE d.project_id = p.id AND d.kind <> 'structure'),
	(SELECT count(*) FROM project_members m WHERE m.project_id = p.id)
	FROM projects p
	LEFT JOIN users u ON u.id = p.created_by`

func scanProject(row interface{ Scan(...any) error }) (*Project, error) {
	var p Project
	if err := row.Scan(&p.ID, &p.Name, &p.Note, &p.CreatedBy, &p.CreatedName,
		&p.CreatedAt, &p.UpdatedAt, &p.Connections, &p.Documents, &p.Members); err != nil {
		return nil, err
	}
	return &p, nil
}

// ListProjects는 프로젝트를 이름순으로 반환한다.
//
// forUser가 비어 있으면 전부다(슈퍼 어드민). 값이 있으면 그 사람이 참여한 것만
// 돌려준다 — 목록에 보이지만 열면 거부되는 줄은, 권한 설정이 잘못된 것처럼 보인다.
func (s *Store) ListProjects(ctx context.Context, forUser string) ([]*Project, error) {
	query := projectSelect
	args := []any{}
	if forUser != "" {
		query += ` WHERE EXISTS (SELECT 1 FROM project_members m
			WHERE m.project_id = p.id AND m.user_id = ?)`
		args = append(args, forUser)
	}
	query += ` ORDER BY p.name_lower`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	defer rows.Close()

	out := []*Project{}
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, fmt.Errorf("scan project: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) GetProject(ctx context.Context, id string) (*Project, error) {
	p, err := scanProject(s.db.QueryRowContext(ctx, projectSelect+` WHERE p.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get project: %w", err)
	}
	return p, nil
}

type SaveProjectParams struct {
	Name    string
	Note    string
	ActorID string
}

// CreateProject는 프로젝트를 만들고 만든 사람을 참여자로 넣는다.
//
// 만든 사람을 자동으로 넣는 이유: 그러지 않으면 프로젝트를 만든 다음 화면이
// 비어 있고, 자기가 만든 것에 자기를 초대해야 한다. 그 한 걸음은 설명할 수 없다.
func (s *Store) CreateProject(ctx context.Context, p SaveProjectParams) (*Project, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin project: %w", err)
	}
	defer tx.Rollback()

	id := uuid.NewString()
	now := nowString()
	var createdBy any
	if p.ActorID != "" {
		createdBy = p.ActorID
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO projects
		(id, name, name_lower, note, created_by, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id, p.Name, strings.ToLower(p.Name), p.Note, createdBy, now, now); err != nil {
		if isUniqueViolation(err) {
			return nil, ErrDuplicateName
		}
		return nil, fmt.Errorf("insert project: %w", err)
	}
	if p.ActorID != "" {
		if _, err := tx.ExecContext(ctx, `INSERT INTO project_members
			(project_id, user_id, added_at) VALUES (?, ?, ?)`, id, p.ActorID, now); err != nil {
			return nil, fmt.Errorf("insert creator member: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit project: %w", err)
	}
	return s.GetProject(ctx, id)
}

func (s *Store) UpdateProject(ctx context.Context, id string, p SaveProjectParams) (*Project, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE projects
		SET name = ?, name_lower = ?, note = ?, updated_at = ? WHERE id = ?`,
		p.Name, strings.ToLower(p.Name), p.Note, nowString(), id)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrDuplicateName
		}
		return nil, fmt.Errorf("update project: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrNotFound
	}
	return s.GetProject(ctx, id)
}

// DeleteProject는 빈 프로젝트만 지운다.
//
// 안에 든 것을 함께 지우지 않는 이유: 커넥션 하나를 지우는 것도 무엇이 함께
// 사라지는지 세어 보여 준 뒤에 하는 일이다(ConnectionImpact). 프로젝트를 지우는
// 단추 하나로 그 모든 것이 한꺼번에 사라진다면, 그 단추는 무엇을 지우는지 말할 수
// 없는 단추다. 비우는 일은 자원 화면에서 하나씩 하고, 이 함수는 껍데기만 치운다.
func (s *Store) DeleteProject(ctx context.Context, id string) error {
	used, err := s.projectResourceCount(ctx, id)
	if err != nil {
		return err
	}
	if used > 0 {
		return ErrProjectInUse
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM projects WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete project: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) projectResourceCount(ctx context.Context, id string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT
		(SELECT count(*) FROM connections WHERE project_id = ?)
		+ (SELECT count(*) FROM erd_documents WHERE project_id = ?)
		+ (SELECT count(*) FROM glossary_terms WHERE project_id = ?)`,
		id, id, id).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count project resources: %w", err)
	}
	return n, nil
}

// ---------- 참여자 ----------

func (s *Store) ListProjectMembers(ctx context.Context, projectID string) ([]*ProjectMember, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT m.user_id,
		COALESCE(u.display_name, ''), COALESCE(u.username, ''), COALESCE(u.role, ''),
		COALESCE(u.status, ''), m.added_at
		FROM project_members m
		JOIN users u ON u.id = m.user_id
		WHERE m.project_id = ?
		ORDER BY u.username_lower`, projectID)
	if err != nil {
		return nil, fmt.Errorf("list project members: %w", err)
	}
	defer rows.Close()

	out := []*ProjectMember{}
	for rows.Next() {
		var m ProjectMember
		if err := rows.Scan(&m.UserID, &m.Name, &m.Login, &m.Role, &m.Status, &m.AddedAt); err != nil {
			return nil, fmt.Errorf("scan project member: %w", err)
		}
		out = append(out, &m)
	}
	return out, rows.Err()
}

// SetProjectMembers는 참여자 명단을 통째로 갈아 끼운다.
//
// 한 사람씩 더하고 빼는 API를 두지 않는 이유: 화면이 명단을 보여 주고 고치는
// 형태라, 부분 갱신으로 두면 "화면에 보이는 명단"과 "저장된 명단"이 갈라지는
// 순간이 생긴다. 통째로 보내면 마지막에 본 것이 곧 저장되는 것이다.
func (s *Store) SetProjectMembers(ctx context.Context, projectID string, userIDs []string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin members: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `DELETE FROM project_members WHERE project_id = ?`, projectID); err != nil {
		return fmt.Errorf("clear members: %w", err)
	}
	now := nowString()
	seen := map[string]bool{}
	for _, id := range userIDs {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		if _, err := tx.ExecContext(ctx, `INSERT INTO project_members
			(project_id, user_id, added_at) VALUES (?, ?, ?)`, projectID, id, now); err != nil {
			return fmt.Errorf("insert member: %w", err)
		}
	}
	return tx.Commit()
}

// ProjectIDsForUser는 한 사람이 참여한 프로젝트 아이디를 반환한다.
// 권한 판정(auth.resolveWithPolicy)의 관문이 이 목록이다.
func (s *Store) ProjectIDsForUser(ctx context.Context, userID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT project_id FROM project_members WHERE user_id = ?`, userID)
	if err != nil {
		return nil, fmt.Errorf("list user projects: %w", err)
	}
	defer rows.Close()

	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan user project: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// IsProjectMember는 한 사람이 그 프로젝트에 참여했는지 본다.
//
// 커넥션이 없는 자원(독립 ERD 초안, 용어)의 관문이다. 커넥션이 있는 자원은
// auth 쪽 판정이 프로젝트까지 함께 보므로 이것을 따로 부르지 않는다.
func (s *Store) IsProjectMember(ctx context.Context, projectID, userID string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM project_members
		WHERE project_id = ? AND user_id = ?`, projectID, userID).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("check project member: %w", err)
	}
	return n > 0, nil
}
