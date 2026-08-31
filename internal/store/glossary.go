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
	// Cat1~3은 대·중·소 분류다. 셋 다 비어 있을 수 있다 — 처음부터 분류 체계를
	// 세우고 시작하는 팀은 없고, 필수로 만들면 아무 말이나 넣게 된다.
	Cat1 string `json:"cat1,omitempty"`
	Cat2 string `json:"cat2,omitempty"`
	Cat3 string `json:"cat3,omitempty"`

	CreatedBy   string `json:"createdBy,omitempty"`
	CreatedName string `json:"createdName,omitempty"`
	CreatedAt   string `json:"createdAt"`
	UpdatedAt   string `json:"updatedAt"`
}

// ErrDuplicateTerm은 이미 사전에 있는 말이다.
var ErrDuplicateTerm = errors.New("이미 있는 용어입니다")

const glossarySelect = `SELECT g.id, g.term, g.physical, g.note,
	g.cat1, g.cat2, g.cat3,
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
		// 분류도 함께 뒤진다. "회원"으로 찾는 사람은 그 말 자체를 찾을 수도, 그
		// 덩어리를 찾을 수도 있다 — 어느 쪽인지 되묻지 않고 둘 다 보여준다.
		query += ` WHERE g.term LIKE ? COLLATE NOCASE
			OR g.physical LIKE ? COLLATE NOCASE
			OR g.note LIKE ? COLLATE NOCASE
			OR g.cat1 LIKE ? COLLATE NOCASE
			OR g.cat2 LIKE ? COLLATE NOCASE
			OR g.cat3 LIKE ? COLLATE NOCASE`
		like := "%" + q + "%"
		args = append(args, like, like, like, like, like, like)
	}
	// 사전은 찾아보는 것이므로 가나다순이 맞다. 최근 순으로 두면 같은 말을 찾을
	// 때마다 자리가 달라진다.
	// 분류가 있으면 분류 순으로 모아 보여준다. 사전을 훑는 사람은 덩어리로 읽는다.
	// 분류가 없는 것은 뒤로 간다 — 아직 자리를 못 정한 것들이다.
	query += ` ORDER BY (g.cat1 = '') , g.cat1 COLLATE NOCASE,
		g.cat2 COLLATE NOCASE, g.cat3 COLLATE NOCASE, g.term COLLATE NOCASE LIMIT ?`
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
		&t.Cat1, &t.Cat2, &t.Cat3,
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
	Cat1      string
	Cat2      string
	Cat3      string
	CreatedBy string
}

// CreateGlossaryTerm은 사전에 한 줄을 더한다.
func (s *Store) CreateGlossaryTerm(ctx context.Context, p SaveGlossaryParams) (*GlossaryTerm, error) {
	id := uuid.NewString()
	now := nowString()
	_, err := s.db.ExecContext(ctx, `INSERT INTO glossary_terms
		(id, term, physical, note, cat1, cat2, cat3, created_by, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, p.Term, p.Physical, p.Note, p.Cat1, p.Cat2, p.Cat3,
		nullString(p.CreatedBy), now, now)
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
		SET term = ?, physical = ?, note = ?, cat1 = ?, cat2 = ?, cat3 = ?, updated_at = ?
		WHERE id = ?`,
		p.Term, p.Physical, p.Note, p.Cat1, p.Cat2, p.Cat3, nowString(), id)
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

// GlossaryCategories는 실제로 쓰인 분류 조합을 모은다.
//
// 분류를 따로 표로 두지 않으므로 목록은 여기서 만든다. 조합(대·중·소)을 그대로
// 돌려주는 이유: 화면이 "이 대분류 아래에서 쓰인 중분류"를 제안하려면 어느 분류가
// 어느 분류 아래에 있었는지를 알아야 한다.
func (s *Store) GlossaryCategories(ctx context.Context) ([][3]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT cat1, cat2, cat3
		FROM glossary_terms
		WHERE cat1 <> '' OR cat2 <> '' OR cat3 <> ''
		ORDER BY cat1 COLLATE NOCASE, cat2 COLLATE NOCASE, cat3 COLLATE NOCASE`)
	if err != nil {
		return nil, fmt.Errorf("list glossary categories: %w", err)
	}
	defer rows.Close()

	out := [][3]string{}
	for rows.Next() {
		var c [3]string
		if err := rows.Scan(&c[0], &c[1], &c[2]); err != nil {
			return nil, fmt.Errorf("scan category: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
