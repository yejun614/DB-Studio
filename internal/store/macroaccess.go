package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"dbstudio/internal/model"
)

// 매크로 접근 제어의 저장 계층.
//
// 판정 규칙 자체는 model.ResolveMacroAccess 한 곳에 있다. 여기서는 그 함수가
// 필요로 하는 세 가지(작성자·공개 설정·협업자 여부)를 읽어 오고, 목록 조회에서는
// 같은 규칙을 SQL로 옮겨 **볼 수 없는 것이 애초에 나오지 않게** 한다.
//
// 규칙을 두 벌(Go와 SQL)로 쓰는 것은 위험하지만, 대안은 전체를 읽어 Go에서 거르는
// 것이다. 매크로가 수천 개가 되면 그것은 목록 한 번에 전부를 스캔한다는 뜻이고,
// 무엇보다 "실행 이력"처럼 LIMIT이 걸린 조회에서는 거르고 나면 페이지가 텅 빈다.
// 그래서 SQL 쪽을 이 파일 안에 모아 두고 한눈에 대조할 수 있게 한다.

// MacroViewer는 "누가 보고 있는가"다.
//
// 저장 계층이 사용자 전체를 받는 이유: 판정에 역할(슈퍼어드민)과 전역 권한(macro)이
// 함께 필요하고, 이것을 ID만으로 줄이면 호출부마다 조건을 다시 짜야 한다.
type MacroViewer struct {
	User *model.User
}

// SystemViewer는 사람이 아닌 경로(부팅 정리 등)를 위한 뷰어다.
// 아무것도 못 본다 — 시스템 경로는 접근 제어가 필요한 조회를 하지 않아야 한다.
func SystemViewer() MacroViewer { return MacroViewer{} }

func (v MacroViewer) id() string {
	if v.User == nil {
		return ""
	}
	return v.User.ID
}

// sees는 이 뷰어가 매크로를 볼 자격이 조금이라도 있는지다.
// 매크로 권한 자체가 없으면 공개 매크로도 보이지 않는다.
func (v MacroViewer) sees() bool { return v.User.HasPerm(model.PermMacro) }

func (v MacroViewer) isSuper() bool {
	return v.sees() && v.User.Role == model.RoleSuperadmin
}

// macroVisibleWhere는 목록 조회에 덧붙일 가시성 조건이다.
// alias는 macros 테이블의 별칭이다(예: "m").
//
// 반환 문자열은 언제나 " AND ..." 로 시작한다 — 호출부가 WHERE 1 = 1 뒤에 그냥
// 이어 붙일 수 있어야 조건이 있고 없고에 따라 문장을 다시 짜지 않는다.
func macroVisibleWhere(v MacroViewer, alias string) (string, []any) {
	if v.isSuper() {
		return "", nil
	}
	if !v.sees() {
		// 조건을 비워 두면 전체가 보인다. 권한이 없을 때는 명시적으로 아무것도 없다.
		return ` AND 0`, nil
	}
	q := fmt.Sprintf(` AND (%[1]s.visibility = 'public' OR %[1]s.created_by = ?
		OR EXISTS (SELECT 1 FROM macro_collaborators c
			WHERE c.macro_id = %[1]s.id AND c.user_id = ?))`, alias)
	return q, []any{v.id(), v.id()}
}

// ---------- 협업자 ----------

// MacroCollaborator는 매크로 하나에 초대된 사람이다.
type MacroCollaborator struct {
	UserID      string    `json:"userId"`
	Username    string    `json:"username"`
	DisplayName string    `json:"displayName"`
	AddedByName string    `json:"addedByName"`
	AddedAt     time.Time `json:"addedAt"`
	// HasMacroPerm은 이 사람이 매크로 메뉴 권한을 실제로 가지고 있는지다.
	//
	// 협업자로 지정했다고 권한이 생기지는 않는다. 권한 없는 사람을 초대해 두고
	// "왜 안 보인대?" 하는 상황이 흔하므로, 화면이 그 자리에서 알려줄 수 있게 함께 내려준다.
	HasMacroPerm bool `json:"hasMacroPerm"`
	// Disabled는 비활성화된 계정이다. 권한과 같은 이유로 함께 보여준다.
	Disabled bool `json:"disabled"`
}

const collaboratorColumns = `c.user_id, u.username, u.display_name, u.role, u.perms, u.status,
	c.added_name, c.added_at`

func scanCollaborators(rows *sql.Rows) ([]MacroCollaborator, error) {
	defer rows.Close()
	out := []MacroCollaborator{}
	for rows.Next() {
		var c MacroCollaborator
		var role, perms, status, addedAt string
		if err := rows.Scan(&c.UserID, &c.Username, &c.DisplayName, &role, &perms,
			&status, &c.AddedByName, &addedAt); err != nil {
			return nil, fmt.Errorf("scan collaborator: %w", err)
		}
		c.AddedAt = parseTime(addedAt)
		c.Disabled = model.UserStatus(status) == model.UserDisabled
		c.HasMacroPerm = !c.Disabled &&
			(model.Role(role) == model.RoleSuperadmin ||
				containsPerm(model.PermsFromString(perms), model.PermMacro))
		out = append(out, c)
	}
	return out, rows.Err()
}

func containsPerm(perms []model.Perm, want model.Perm) bool {
	return slices.Contains(perms, want)
}

func (s *Store) ListMacroCollaborators(ctx context.Context, macroID string) ([]MacroCollaborator, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+collaboratorColumns+` FROM macro_collaborators c
		 JOIN users u ON u.id = c.user_id
		 WHERE c.macro_id = ? ORDER BY u.username`, macroID)
	if err != nil {
		return nil, fmt.Errorf("list macro collaborators: %w", err)
	}
	return scanCollaborators(rows)
}

// AddMacroCollaborator는 협업자를 추가한다. 이미 있으면 조용히 성공한다 —
// 두 사람이 같은 사람을 동시에 초대하는 것은 오류가 아니다.
func (s *Store) AddMacroCollaborator(ctx context.Context, macroID, userID, addedBy, addedName string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO macro_collaborators
		(macro_id, user_id, added_by, added_name, added_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (macro_id, user_id) DO NOTHING`,
		macroID, userID, nullString(addedBy), addedName, nowString())
	if err != nil {
		return fmt.Errorf("add macro collaborator: %w", err)
	}
	return nil
}

func (s *Store) RemoveMacroCollaborator(ctx context.Context, macroID, userID string) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM macro_collaborators WHERE macro_id = ? AND user_id = ?`, macroID, userID)
	if err != nil {
		return fmt.Errorf("remove macro collaborator: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetMacroVisibility는 공개 범위를 바꾼다.
//
// updated_at을 함께 건드리지 않는 이유: 목록의 "수정" 열은 매크로의 내용이 언제
// 바뀌었는가를 뜻한다. 공유 설정을 바꾼 것을 거기에 섞으면 아무것도 안 바뀐 매크로가
// 목록 맨 위로 올라온다. 누가 언제 공개를 바꿨는지는 감사 로그가 답한다.
func (s *Store) SetMacroVisibility(ctx context.Context, id string,
	visibility model.MacroVisibility, public model.MacroPublicAccess) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE macros SET visibility = ?, public_access = ? WHERE id = ?`,
		string(visibility), string(public), id)
	if err != nil {
		return fmt.Errorf("set macro visibility: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ---------- 전역 커스텀 노드 ----------
//
// 매크로 전용 노드(scope='macro')는 소속 매크로의 판정을 물려받으므로 자체 협업자가
// 없다. 아래 함수들은 전역 노드에만 쓰인다.

func (s *Store) ListNodeDefCollaborators(ctx context.Context, defID string) ([]MacroCollaborator, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+collaboratorColumns+`
		 FROM macro_node_def_collaborators c
		 JOIN users u ON u.id = c.user_id
		 WHERE c.def_id = ? ORDER BY u.username`, defID)
	if err != nil {
		return nil, fmt.Errorf("list node def collaborators: %w", err)
	}
	return scanCollaborators(rows)
}

func (s *Store) AddNodeDefCollaborator(ctx context.Context, defID, userID, addedBy, addedName string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO macro_node_def_collaborators
		(def_id, user_id, added_by, added_name, added_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (def_id, user_id) DO NOTHING`,
		defID, userID, nullString(addedBy), addedName, nowString())
	if err != nil {
		return fmt.Errorf("add node def collaborator: %w", err)
	}
	return nil
}

func (s *Store) RemoveNodeDefCollaborator(ctx context.Context, defID, userID string) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM macro_node_def_collaborators WHERE def_id = ? AND user_id = ?`, defID, userID)
	if err != nil {
		return fmt.Errorf("remove node def collaborator: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) SetNodeDefVisibility(ctx context.Context, id string,
	visibility model.MacroVisibility, public model.MacroPublicAccess) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE macro_node_defs SET visibility = ?, public_access = ? WHERE id = ?`,
		string(visibility), string(public), id)
	if err != nil {
		return fmt.Errorf("set node def visibility: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ---------- 협업자 후보 ----------

// MacroPerson은 협업자로 초대할 수 있는 사람이다.
type MacroPerson struct {
	ID          string `json:"id"`
	Username    string `json:"username"`
	DisplayName string `json:"displayName"`
	Role        string `json:"role"`
}

// ListMacroPeople은 매크로 권한을 가진 활성 사용자를 반환한다.
//
// /users 를 쓰지 않는 이유: 그 경로는 슈퍼어드민 전용이고 비밀번호 해시까지 실어
// 나른다. 협업자 선택에는 이름 세 개면 충분하고, 그 목록은 매크로를 쓰는 사람이라면
// 볼 수 있어야 초대라는 동작이 성립한다.
//
// 권한이 없는 사람을 애초에 목록에서 빼는 이유: 초대해도 아무것도 못 하기 때문이다.
// 정말 필요하면 관리자가 권한을 준 뒤 초대하면 된다.
func (s *Store) ListMacroPeople(ctx context.Context, exclude string) ([]MacroPerson, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, username, display_name, role, perms FROM users
		 WHERE status = 'active' AND id <> ? ORDER BY username`, exclude)
	if err != nil {
		return nil, fmt.Errorf("list macro people: %w", err)
	}
	defer rows.Close()

	out := []MacroPerson{}
	for rows.Next() {
		var p MacroPerson
		var perms string
		if err := rows.Scan(&p.ID, &p.Username, &p.DisplayName, &p.Role, &perms); err != nil {
			return nil, fmt.Errorf("scan macro person: %w", err)
		}
		if model.Role(p.Role) != model.RoleSuperadmin &&
			!containsPerm(model.PermsFromString(perms), model.PermMacro) {
			continue
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetMacroPerson은 초대 대상이 실제로 초대 가능한 사람인지 확인한다.
func (s *Store) GetMacroPerson(ctx context.Context, userID string) (*MacroPerson, error) {
	var p MacroPerson
	var perms, status string
	err := s.db.QueryRowContext(ctx,
		`SELECT id, username, display_name, role, perms, status FROM users WHERE id = ?`, userID).
		Scan(&p.ID, &p.Username, &p.DisplayName, &p.Role, &perms, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get macro person: %w", err)
	}
	if model.UserStatus(status) == model.UserDisabled {
		return nil, ErrNotFound
	}
	return &p, nil
}
