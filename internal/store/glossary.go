package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// 용어 사전.
//
// 논리명과 물리명을 팀이 같은 규칙으로 쓰기 위한 표다. "회원 번호"를 누구는
// member_no 로, 누구는 mbr_num 으로 적으면 같은 것이 두 이름을 갖는다.
//
// 앱 전체에 하나만 둔다(문서마다 두지 않는다). 이것은 그림 하나의 사정이 아니라
// 팀의 약속이고, 문서마다 사전이 따로 있으면 문서마다 다른 약속이 생긴다.

// GlossaryTerm은 사전의 한 줄이다.
type GlossaryTerm struct {
	ID       string `json:"id"`
	Term     string `json:"term"`
	Physical string `json:"physical"`
	Note     string `json:"note,omitempty"`

	CreatedBy   string `json:"createdBy,omitempty"`
	CreatedName string `json:"createdName,omitempty"`
	CreatedAt   string `json:"createdAt"`
	UpdatedAt   string `json:"updatedAt"`
}

// ErrDuplicateTerm은 이미 사전에 있는 말이다.
var ErrDuplicateTerm = errors.New("이미 있는 용어입니다")

const glossarySelect = `SELECT g.id, g.term, g.physical, g.note,
	COALESCE(g.created_by, ''), COALESCE(u.display_name, u.username, ''),
	g.created_at, g.updated_at
	FROM glossary_terms g
	LEFT JOIN users u ON u.id = g.created_by`

// ListGlossary는 사전을 읽는다. q가 있으면 용어·물리명·설명에서 찾는다.
//
// 세 곳을 모두 뒤지는 이유: 사람은 "회원"으로도, "member"로도, "가입"으로도 찾는다.
// 어느 칸에서 찾을지 고르게 하면 그 고르개 자체가 한 걸음이 되고, 사전은 찾기
// 귀찮은 것이 되면 아무도 보지 않는다.
func (s *Store) ListGlossary(ctx context.Context, q string, limit int) ([]*GlossaryTerm, error) {
	query := glossarySelect
	args := []any{}
	if q = strings.TrimSpace(q); q != "" {
		query += ` WHERE g.term LIKE ? COLLATE NOCASE
			OR g.physical LIKE ? COLLATE NOCASE
			OR g.note LIKE ? COLLATE NOCASE`
		like := "%" + q + "%"
		args = append(args, like, like, like)
	}
	// 사전은 찾아보는 것이므로 가나다순이 맞다. 최근 순으로 두면 같은 말을 찾을
	// 때마다 자리가 달라진다.
	query += ` ORDER BY g.term COLLATE NOCASE LIMIT ?`
	if limit <= 0 || limit > 2000 {
		limit = 500
	}
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list glossary: %w", err)
	}
	defer rows.Close()

	out := []*GlossaryTerm{}
	for rows.Next() {
		t, err := scanGlossary(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) GetGlossaryTerm(ctx context.Context, id string) (*GlossaryTerm, error) {
	t, err := scanGlossary(s.db.QueryRowContext(ctx, glossarySelect+` WHERE g.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return t, err
}

func scanGlossary(row interface{ Scan(...any) error }) (*GlossaryTerm, error) {
	var t GlossaryTerm
	if err := row.Scan(&t.ID, &t.Term, &t.Physical, &t.Note,
		&t.CreatedBy, &t.CreatedName, &t.CreatedAt, &t.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		return nil, fmt.Errorf("scan glossary: %w", err)
	}
	return &t, nil
}

// SaveGlossaryParams는 사전 한 줄의 입력이다.
type SaveGlossaryParams struct {
	Term      string
	Physical  string
	Note      string
	CreatedBy string
}

// CreateGlossaryTerm은 사전에 한 줄을 더한다.
func (s *Store) CreateGlossaryTerm(ctx context.Context, p SaveGlossaryParams) (*GlossaryTerm, error) {
	id := uuid.NewString()
	now := nowString()
	_, err := s.db.ExecContext(ctx, `INSERT INTO glossary_terms
		(id, term, physical, note, created_by, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id, p.Term, p.Physical, p.Note, nullString(p.CreatedBy), now, now)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrDuplicateTerm
		}
		return nil, fmt.Errorf("insert glossary: %w", err)
	}
	return s.GetGlossaryTerm(ctx, id)
}

// UpdateGlossaryTerm은 한 줄을 고친다.
func (s *Store) UpdateGlossaryTerm(ctx context.Context, id string, p SaveGlossaryParams) (*GlossaryTerm, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE glossary_terms
		SET term = ?, physical = ?, note = ?, updated_at = ? WHERE id = ?`,
		p.Term, p.Physical, p.Note, nowString(), id)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrDuplicateTerm
		}
		return nil, fmt.Errorf("update glossary: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrNotFound
	}
	return s.GetGlossaryTerm(ctx, id)
}

func (s *Store) DeleteGlossaryTerm(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM glossary_terms WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete glossary: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
