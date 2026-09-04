package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// 사용자가 만드는 AI 스킬.
//
// 앱이 들고 있는 스킬(api.aiSkills)은 여기 담기지 않는다. 그것들은 툴 이름을 부르는
// 글이라 툴이 바뀌면 함께 바뀌어야 하고, DB에 복사해 두면 업그레이드한 설치에서 옛
// 글이 남는다. 화면은 둘을 합쳐 보여주되 앱의 것은 고칠 수 없다.

// AISkill은 사용자가 만든 스킬 하나다.
type AISkill struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Icon        string `json:"icon,omitempty"`
	Prompt      string `json:"prompt"`
	// Args는 화면이 물어볼 값들이다(JSON 배열 문자열 그대로 오간다).
	Args string `json:"-"`
	// Shared면 모두가 목록에서 본다.
	Shared bool `json:"shared"`

	CreatedBy   string `json:"createdBy,omitempty"`
	CreatedName string `json:"createdName,omitempty"`
	CreatedAt   string `json:"createdAt"`
	UpdatedAt   string `json:"updatedAt"`
}

const aiSkillSelect = `SELECT s.id, s.name, s.description, s.icon, s.prompt, s.args,
	s.shared, COALESCE(s.created_by, ''), COALESCE(u.display_name, u.username, ''),
	s.created_at, s.updated_at
	FROM ai_skills s
	LEFT JOIN users u ON u.id = s.created_by`

func scanAISkill(rows *sql.Rows) (*AISkill, error) {
	var sk AISkill
	var shared int
	if err := rows.Scan(&sk.ID, &sk.Name, &sk.Description, &sk.Icon, &sk.Prompt,
		&sk.Args, &shared, &sk.CreatedBy, &sk.CreatedName,
		&sk.CreatedAt, &sk.UpdatedAt); err != nil {
		return nil, err
	}
	sk.Shared = shared != 0
	return &sk, nil
}

// ListAISkills는 이 사람이 쓸 수 있는 스킬을 읽는다(내 것 + 공유된 것).
func (s *Store) ListAISkills(ctx context.Context, userID string) ([]*AISkill, error) {
	rows, err := s.db.QueryContext(ctx,
		aiSkillSelect+` WHERE s.shared = 1 OR s.created_by = ? ORDER BY s.name COLLATE NOCASE`,
		userID)
	if err != nil {
		return nil, fmt.Errorf("스킬 목록: %w", err)
	}
	defer rows.Close()

	out := []*AISkill{}
	for rows.Next() {
		sk, serr := scanAISkill(rows)
		if serr != nil {
			return nil, serr
		}
		out = append(out, sk)
	}
	return out, rows.Err()
}

// GetAISkill은 스킬 하나를 읽는다. 볼 수 있는지는 부르는 쪽이 판정한다 —
// 조용히 없는 것처럼 구는 조회 함수는 나중에 반드시 누군가를 속인다.
func (s *Store) GetAISkill(ctx context.Context, id string) (*AISkill, error) {
	rows, err := s.db.QueryContext(ctx, aiSkillSelect+` WHERE s.id = ?`, id)
	if err != nil {
		return nil, fmt.Errorf("스킬 조회: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, ErrNotFound
	}
	return scanAISkill(rows)
}

// SaveAISkillParams는 만들거나 고칠 때의 값이다.
type SaveAISkillParams struct {
	Name        string
	Description string
	Icon        string
	Prompt      string
	Args        string
	Shared      bool
}

func (s *Store) CreateAISkill(ctx context.Context, p SaveAISkillParams, ownerID string) (*AISkill, error) {
	id := uuid.NewString()
	now := nowString()
	shared := 0
	if p.Shared {
		shared = 1
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO ai_skills
		(id, name, description, icon, prompt, args, shared, created_by, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, strings.TrimSpace(p.Name), p.Description, p.Icon, p.Prompt, p.Args,
		shared, nullString(ownerID), now, now); err != nil {
		return nil, fmt.Errorf("스킬 저장: %w", err)
	}
	return s.GetAISkill(ctx, id)
}

func (s *Store) UpdateAISkill(ctx context.Context, id string, p SaveAISkillParams) (*AISkill, error) {
	shared := 0
	if p.Shared {
		shared = 1
	}
	res, err := s.db.ExecContext(ctx, `UPDATE ai_skills
		SET name = ?, description = ?, icon = ?, prompt = ?, args = ?, shared = ?, updated_at = ?
		WHERE id = ?`,
		strings.TrimSpace(p.Name), p.Description, p.Icon, p.Prompt, p.Args, shared,
		nowString(), id)
	if err != nil {
		return nil, fmt.Errorf("스킬 수정: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrNotFound
	}
	return s.GetAISkill(ctx, id)
}

func (s *Store) DeleteAISkill(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM ai_skills WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("스킬 삭제: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
