package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"dbstudio/internal/model"
)

// 매크로 저장소.
//
// 버전은 추가만 된다. 편집은 언제나 새 버전을 만들고, 롤백도 옛 그래프를 복사한
// 새 버전을 만든다. 덮어쓰기를 허용하면 "그때 실행된 것"을 재구성할 수 없게 되고,
// 실행 이력이 가리키는 버전이 다른 내용으로 바뀌어 있을 수 있다.
//
// 조회 함수 대부분이 MacroViewer를 받는다. 매크로는 기본이 비공개이므로 "무엇이
// 있는가"부터 사람마다 다르고, 그 판정을 호출부에 맡기면 어딘가는 반드시 빠뜨린다
// (접근 규칙은 macroaccess.go).

// Macro는 매크로 한 개의 메타데이터다. 그래프는 버전에 있다.
type Macro struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	Description    string    `json:"description"`
	CurrentVersion int       `json:"currentVersion"`
	CreatedBy      string    `json:"createdBy,omitempty"`
	CreatedByName  string    `json:"createdByName"`
	UpdatedByName  string    `json:"updatedByName"`
	CreatedAt      time.Time `json:"createdAt"`
	UpdatedAt      time.Time `json:"updatedAt"`
	// VersionCount는 목록 화면이 "몇 번 고쳐졌는가"를 보여주기 위한 값이다.
	VersionCount int `json:"versionCount"`
	// LastRun은 마지막 실행 요약이다. 목록에서 상태를 바로 볼 수 있어야
	// 실패한 매크로를 찾으려고 하나씩 열어보지 않는다.
	LastRunAt     *time.Time `json:"lastRunAt,omitempty"`
	LastRunStatus string     `json:"lastRunStatus,omitempty"`

	// ---- 접근 제어 ----
	Visibility        model.MacroVisibility   `json:"visibility"`
	PublicAccess      model.MacroPublicAccess `json:"publicAccess"`
	CollaboratorCount int                     `json:"collaboratorCount"`
	// Access는 이 결과를 읽은 사람의 권한이다. 조회한 뷰어에 따라 달라지는 값이므로
	// 매크로의 속성이 아니지만, 화면이 버튼을 그리려면 매번 함께 필요하다.
	Access model.MacroAccess `json:"access"`
	// CanEdit/CanManage/CanDelete는 Access를 화면이 쓰기 좋게 편 것이다.
	// 프런트에서 등급을 다시 비교하게 두면 사다리 순서가 두 곳에 존재하게 된다.
	CanEdit   bool `json:"canEdit"`
	CanManage bool `json:"canManage"`
	CanDelete bool `json:"canDelete"`
	// IsCollaborator는 뷰어 본인이 협업자로 지정되어 있는지다.
	IsCollaborator bool `json:"isCollaborator"`
}

// applyAccess는 뷰어 기준 권한을 채운다.
func (m *Macro) applyAccess(v MacroViewer) {
	m.Access = model.ResolveMacroAccess(v.User, model.MacroOwnership{
		CreatedBy:      m.CreatedBy,
		Visibility:     m.Visibility,
		PublicAccess:   m.PublicAccess,
		IsCollaborator: m.IsCollaborator,
	})
	m.CanEdit = m.Access.CanEdit()
	m.CanManage = m.Access.CanManage()
	m.CanDelete = m.Access.CanDelete()
}

// MacroVersion은 저장된 그래프 한 판이다.
type MacroVersion struct {
	MacroID    string    `json:"macroId"`
	Version    int       `json:"version"`
	Graph      string    `json:"graph"`
	Note       string    `json:"note"`
	AuthorID   string    `json:"authorId,omitempty"`
	AuthorName string    `json:"authorName"`
	CreatedAt  time.Time `json:"createdAt"`
}

// macroColumns의 마지막 두 항목은 뷰어에 따라 달라진다. 그래서 이 목록을 쓰는 조회는
// **뷰어 ID를 첫 인자로** 넘겨야 한다(SELECT 절이 WHERE보다 먼저 오므로).
const macroColumns = `m.id, m.name, m.description, m.current_version, m.created_by,
	m.created_by_name, m.updated_by_name, m.created_at, m.updated_at,
	m.visibility, m.public_access,
	(SELECT count(*) FROM macro_versions v WHERE v.macro_id = m.id),
	(SELECT r.started_at FROM macro_runs r WHERE r.macro_id = m.id ORDER BY r.started_at DESC LIMIT 1),
	(SELECT r.status FROM macro_runs r WHERE r.macro_id = m.id ORDER BY r.started_at DESC LIMIT 1),
	(SELECT count(*) FROM macro_collaborators c WHERE c.macro_id = m.id),
	EXISTS (SELECT 1 FROM macro_collaborators c WHERE c.macro_id = m.id AND c.user_id = ?)`

func scanMacro(row interface{ Scan(...any) error }, v MacroViewer) (*Macro, error) {
	var m Macro
	var createdBy, lastRunAt, lastStatus sql.NullString
	var createdAt, updatedAt, visibility, publicAccess string
	var isCollaborator int
	err := row.Scan(&m.ID, &m.Name, &m.Description, &m.CurrentVersion, &createdBy,
		&m.CreatedByName, &m.UpdatedByName, &createdAt, &updatedAt,
		&visibility, &publicAccess,
		&m.VersionCount, &lastRunAt, &lastStatus,
		&m.CollaboratorCount, &isCollaborator)
	if err != nil {
		return nil, err
	}
	m.CreatedBy = createdBy.String
	m.CreatedAt = parseTime(createdAt)
	m.UpdatedAt = parseTime(updatedAt)
	m.LastRunAt = parseTimePtr(lastRunAt)
	m.LastRunStatus = lastStatus.String
	m.Visibility = model.MacroVisibility(visibility)
	m.PublicAccess = model.MacroPublicAccess(publicAccess)
	m.IsCollaborator = isCollaborator != 0
	m.applyAccess(v)
	return &m, nil
}

// CreateMacro는 매크로와 첫 버전을 함께 만든다.
//
// 빈 매크로를 만들고 버전을 나중에 만들게 하면 current_version=0인 상태가 존재하게
// 되고, 그 상태에서 실행 버튼을 누르면 무엇을 실행해야 하는지 답이 없다.
//
// 새 매크로는 **비공개로 태어난다**(스키마 기본값). 만드는 순간 남에게 보이면
// 반쯤 만든 것과 실험한 것이 남의 목록을 채우고, 무엇보다 공유는 하겠다고 눌러야
// 일어나는 일이지 안 막았기 때문에 일어나는 일이 아니다.
func (s *Store) CreateMacro(ctx context.Context, name, description, graph string,
	author *model.User, authorName string) (*Macro, error) {
	id := uuid.NewString()
	now := nowString()
	authorID := ""
	if author != nil {
		authorID = author.ID
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `INSERT INTO macros
		(id, name, name_lower, description, current_version, created_by, created_by_name,
		 created_at, updated_at, updated_by_name)
		VALUES (?, ?, ?, ?, 1, ?, ?, ?, ?, ?)`,
		id, name, strings.ToLower(name), description, nullString(authorID), authorName,
		now, now, authorName)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrDuplicateName
		}
		return nil, fmt.Errorf("insert macro: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO macro_versions
		(macro_id, version, graph, note, author_id, author_name, created_at)
		VALUES (?, 1, ?, '최초 작성', ?, ?, ?)`,
		id, graph, nullString(authorID), authorName, now); err != nil {
		return nil, fmt.Errorf("insert macro version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return s.GetMacro(ctx, id, MacroViewer{User: author})
}

// GetMacro는 매크로를 읽고 뷰어 기준 권한(Macro.Access)을 함께 채운다.
//
// 여기서 권한 없음을 ErrNotFound로 바꾸지 **않는다.** 호출부마다 필요한 응답이 다르고
// (API는 404, 엔진은 "접근 불가" 오류), 무엇보다 조용히 없는 것처럼 구는 조회 함수는
// 나중에 반드시 누군가를 속인다. 판정은 읽은 쪽이 Access를 보고 한다.
func (s *Store) GetMacro(ctx context.Context, id string, v MacroViewer) (*Macro, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+macroColumns+` FROM macros m WHERE m.id = ?`, v.id(), id)
	m, err := scanMacro(row, v)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get macro: %w", err)
	}
	return m, nil
}

// ListMacros는 뷰어가 볼 수 있는 매크로만 반환한다.
// 비공개 매크로는 목록에 이름조차 나오지 않는다 — 그것이 비공개의 뜻이다.
func (s *Store) ListMacros(ctx context.Context, v MacroViewer) ([]*Macro, error) {
	where, args := macroVisibleWhere(v, "m")
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+macroColumns+` FROM macros m WHERE 1 = 1`+where+` ORDER BY m.updated_at DESC`,
		append([]any{v.id()}, args...)...)
	if err != nil {
		return nil, fmt.Errorf("list macros: %w", err)
	}
	defer rows.Close()

	out := []*Macro{}
	for rows.Next() {
		m, err := scanMacro(rows, v)
		if err != nil {
			return nil, fmt.Errorf("scan macro: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate macros: %w", err)
	}
	return out, nil
}

// UpdateMacroMeta는 이름과 설명만 바꾼다. 그래프는 버전으로만 바뀐다.
func (s *Store) UpdateMacroMeta(ctx context.Context, id, name, description, actorName string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE macros SET name = ?, name_lower = ?, description = ?,
			updated_at = ?, updated_by_name = ? WHERE id = ?`,
		name, strings.ToLower(name), description, nowString(), actorName, id)
	if err != nil {
		if isUniqueViolation(err) {
			return ErrDuplicateName
		}
		return fmt.Errorf("update macro: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// CreateMacroVersion은 새 버전을 저장하고 그것을 현재 버전으로 만든다.
// 버전 번호는 트랜잭션 안에서 계산해 동시 저장에도 충돌하지 않는다.
func (s *Store) CreateMacroVersion(ctx context.Context, macroID, graph, note, authorID, authorName string) (int, error) {
	now := nowString()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()

	var next int
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(version), 0) + 1 FROM macro_versions WHERE macro_id = ?`,
		macroID).Scan(&next); err != nil {
		return 0, fmt.Errorf("next version: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `INSERT INTO macro_versions
		(macro_id, version, graph, note, author_id, author_name, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		macroID, next, graph, note, nullString(authorID), authorName, now); err != nil {
		return 0, fmt.Errorf("insert version: %w", err)
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE macros SET current_version = ?, updated_at = ?, updated_by_name = ? WHERE id = ?`,
		next, now, authorName, macroID)
	if err != nil {
		return 0, fmt.Errorf("update current version: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, ErrNotFound
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	return next, nil
}

func (s *Store) GetMacroVersion(ctx context.Context, macroID string, version int) (*MacroVersion, error) {
	q := `SELECT macro_id, version, graph, note, author_id, author_name, created_at
		FROM macro_versions WHERE macro_id = ? AND version = ?`
	args := []any{macroID, version}
	if version <= 0 {
		// 0은 "현재 버전"을 뜻한다. 호출부가 매크로를 먼저 읽어 버전을 알아내야 하는
		// 왕복을 없앤다.
		q = `SELECT v.macro_id, v.version, v.graph, v.note, v.author_id, v.author_name, v.created_at
			FROM macro_versions v JOIN macros m ON m.id = v.macro_id AND m.current_version = v.version
			WHERE v.macro_id = ?`
		args = []any{macroID}
	}

	var v MacroVersion
	var authorID sql.NullString
	var createdAt string
	err := s.db.QueryRowContext(ctx, q, args...).Scan(
		&v.MacroID, &v.Version, &v.Graph, &v.Note, &authorID, &v.AuthorName, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get macro version: %w", err)
	}
	v.AuthorID = authorID.String
	v.CreatedAt = parseTime(createdAt)
	return &v, nil
}

// ListMacroVersions는 버전 목록을 최신순으로 반환한다. 그래프 본문은 제외한다 —
// 목록 화면에 그래프 JSON 수십 개를 보내는 것은 낭비다.
func (s *Store) ListMacroVersions(ctx context.Context, macroID string) ([]*MacroVersion, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT macro_id, version, note, author_id, author_name, created_at
		 FROM macro_versions WHERE macro_id = ? ORDER BY version DESC`, macroID)
	if err != nil {
		return nil, fmt.Errorf("list macro versions: %w", err)
	}
	defer rows.Close()

	out := []*MacroVersion{}
	for rows.Next() {
		var v MacroVersion
		var authorID sql.NullString
		var createdAt string
		if err := rows.Scan(&v.MacroID, &v.Version, &v.Note, &authorID, &v.AuthorName, &createdAt); err != nil {
			return nil, fmt.Errorf("scan macro version: %w", err)
		}
		v.AuthorID = authorID.String
		v.CreatedAt = parseTime(createdAt)
		out = append(out, &v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate macro versions: %w", err)
	}
	return out, nil
}

func (s *Store) DeleteMacro(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM macros WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete macro: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ---------- 사용자 노드 정의 ----------

// NodeDef의 접근 제어는 범위에 따라 갈린다.
//
//   - scope='global': 자기 자신이 주인이다. 공개 설정과 협업자를 따로 가진다.
//   - scope='macro' : 소속 매크로의 판정을 그대로 물려받는다. 매크로 전용 노드는
//     그 매크로의 일부이고, 따로 공유 설정을 두면 "매크로는 넘겨줬는데 그 안의
//     노드는 못 고치는" 상태가 만들어진다.
type NodeDef struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Scope   string `json:"scope"` // global | macro
	MacroID string `json:"macroId,omitempty"`
	// MacroName은 매크로 전용 노드가 어느 매크로의 것인지 알려준다.
	// 목록에서 전역 노드와 섞여 보이므로 이름이 없으면 "만들었는데 어디서도
	// 못 쓰는 노드"로 오해하게 된다.
	MacroName      string    `json:"macroName,omitempty"`
	Description    string    `json:"description"`
	Fields         string    `json:"fields"` // JSON 배열
	Ports          string    `json:"ports"`  // JSON 배열
	Script         string    `json:"script"`
	CurrentVersion int       `json:"currentVersion"`
	CreatedBy      string    `json:"createdBy,omitempty"`
	CreatedByName  string    `json:"createdByName"`
	CreatedAt      time.Time `json:"createdAt"`
	UpdatedAt      time.Time `json:"updatedAt"`

	// ---- 접근 제어 (scope='global' 에서만 읽힌다) ----
	Visibility        model.MacroVisibility   `json:"visibility"`
	PublicAccess      model.MacroPublicAccess `json:"publicAccess"`
	CollaboratorCount int                     `json:"collaboratorCount"`
	IsCollaborator    bool                    `json:"isCollaborator"`

	Access    model.MacroAccess `json:"access"`
	CanEdit   bool              `json:"canEdit"`
	CanManage bool              `json:"canManage"`
	CanDelete bool              `json:"canDelete"`
}

// nodeDefColumns는 macro_node_defs를 d로, 소속 매크로를 pm으로 참조한다.
// 뷰어 ID를 **두 번** 받는다(노드 협업자, 매크로 협업자) — 순서에 주의.
const nodeDefColumns = `d.id, d.name, d.scope, d.macro_id, d.description, d.fields, d.ports,
	d.script, d.current_version, d.created_by, d.created_by_name, d.created_at, d.updated_at,
	d.visibility, d.public_access,
	(SELECT count(*) FROM macro_node_def_collaborators nc WHERE nc.def_id = d.id),
	EXISTS (SELECT 1 FROM macro_node_def_collaborators nc
		WHERE nc.def_id = d.id AND nc.user_id = ?),
	pm.created_by, pm.visibility, pm.public_access, COALESCE(pm.name, ''),
	EXISTS (SELECT 1 FROM macro_collaborators mc
		WHERE mc.macro_id = d.macro_id AND mc.user_id = ?)`

// nodeDefFrom은 nodeDefColumns가 전제하는 FROM 절이다.
// 매크로 전용 노드의 판정을 SQL 한 번에 끝내기 위해 소속 매크로를 함께 읽는다.
const nodeDefFrom = ` FROM macro_node_defs d LEFT JOIN macros pm ON pm.id = d.macro_id`

func scanNodeDef(row interface{ Scan(...any) error }, v MacroViewer) (*NodeDef, error) {
	var d NodeDef
	var macroID, createdBy sql.NullString
	var parentCreatedBy, parentVisibility, parentPublic sql.NullString
	var createdAt, updatedAt, visibility, publicAccess string
	var isCollaborator, isMacroCollaborator int
	err := row.Scan(&d.ID, &d.Name, &d.Scope, &macroID, &d.Description, &d.Fields, &d.Ports,
		&d.Script, &d.CurrentVersion, &createdBy, &d.CreatedByName, &createdAt, &updatedAt,
		&visibility, &publicAccess, &d.CollaboratorCount, &isCollaborator,
		&parentCreatedBy, &parentVisibility, &parentPublic, &d.MacroName, &isMacroCollaborator)
	if err != nil {
		return nil, err
	}
	d.MacroID = macroID.String
	d.CreatedBy = createdBy.String
	d.CreatedAt = parseTime(createdAt)
	d.UpdatedAt = parseTime(updatedAt)
	d.Visibility = model.MacroVisibility(visibility)
	d.PublicAccess = model.MacroPublicAccess(publicAccess)
	d.IsCollaborator = isCollaborator != 0

	owner := model.MacroOwnership{
		CreatedBy:      d.CreatedBy,
		Visibility:     d.Visibility,
		PublicAccess:   d.PublicAccess,
		IsCollaborator: d.IsCollaborator,
	}
	if d.Scope == "macro" {
		owner = model.MacroOwnership{
			CreatedBy:      parentCreatedBy.String,
			Visibility:     model.MacroVisibility(parentVisibility.String),
			PublicAccess:   model.MacroPublicAccess(parentPublic.String),
			IsCollaborator: isMacroCollaborator != 0,
		}
	}
	d.Access = model.ResolveMacroAccess(v.User, owner)
	d.CanEdit = d.Access.CanEdit()
	d.CanManage = d.Access.CanManage()
	// 삭제 조건은 범위마다 다르다.
	//
	// 전역 노드는 만든 사람만 지운다 — 어느 매크로가 그것을 쓰는지 지우는 사람은
	// 알 수 없고, 지우면 그 매크로들이 전부 깨진다.
	// 매크로 전용 노드는 협업자도 지운다. 그 노드는 매크로의 일부이고, 매크로를
	// 지우면 어차피 함께 사라지며, 무엇이 그것을 쓰는지는 그 매크로 안에서 다 보인다.
	if d.Scope == "macro" {
		d.CanDelete = d.Access.CanManage()
	} else {
		d.CanDelete = d.Access.CanDelete()
	}

	// 볼 수 없는 노드의 스크립트는 실어 나르지 않는다.
	//
	// 목록에서 걸러지고도 여기까지 오는 경로가 있다: 편집 화면은 그래프가 참조하는
	// 노드를 이름과 포트라도 그려야 캔버스가 깨지지 않으므로, 접근 권한이 없는 노드도
	// 껍데기만 함께 내려보낸다(api.macroPalette). 스크립트는 그 껍데기에 낄 것이 아니다.
	if !d.Access.CanView() {
		d.Script = ""
	}
	return &d, nil
}

type SaveNodeDefParams struct {
	Name        string
	Scope       string
	MacroID     string
	Description string
	Fields      string
	Ports       string
	Script      string
	Note        string
	AuthorID    string
	AuthorName  string
	// Viewer는 저장 후 되돌려 줄 결과의 권한을 계산하기 위한 것이다.
	// 대개 작성자 본인이지만, 협업자나 슈퍼어드민이 고치는 경우도 있다.
	Viewer MacroViewer
}

func (s *Store) CreateNodeDef(ctx context.Context, p SaveNodeDefParams) (*NodeDef, error) {
	id := uuid.NewString()
	now := nowString()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `INSERT INTO macro_node_defs
		(id, name, scope, macro_id, description, fields, ports, script,
		 current_version, created_by, created_by_name, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?, ?, ?)`,
		id, p.Name, p.Scope, nullString(p.MacroID), p.Description, p.Fields, p.Ports, p.Script,
		nullString(p.AuthorID), p.AuthorName, now, now); err != nil {
		return nil, fmt.Errorf("insert node def: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO macro_node_def_versions
		(def_id, version, script, fields, ports, note, author_name, created_at)
		VALUES (?, 1, ?, ?, ?, ?, ?, ?)`,
		id, p.Script, p.Fields, p.Ports, "최초 작성", p.AuthorName, now); err != nil {
		return nil, fmt.Errorf("insert node def version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return s.GetNodeDef(ctx, id, p.Viewer)
}

// UpdateNodeDef는 정의를 고치고 새 버전을 남긴다.
func (s *Store) UpdateNodeDef(ctx context.Context, id string, p SaveNodeDefParams) (*NodeDef, error) {
	now := nowString()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()

	var next int
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(version), 0) + 1 FROM macro_node_def_versions WHERE def_id = ?`,
		id).Scan(&next); err != nil {
		return nil, fmt.Errorf("next node def version: %w", err)
	}

	res, err := tx.ExecContext(ctx, `UPDATE macro_node_defs
		SET name = ?, description = ?, fields = ?, ports = ?, script = ?,
		    current_version = ?, updated_at = ? WHERE id = ?`,
		p.Name, p.Description, p.Fields, p.Ports, p.Script, next, now, id)
	if err != nil {
		return nil, fmt.Errorf("update node def: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO macro_node_def_versions
		(def_id, version, script, fields, ports, note, author_name, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		id, next, p.Script, p.Fields, p.Ports, p.Note, p.AuthorName, now); err != nil {
		return nil, fmt.Errorf("insert node def version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return s.GetNodeDef(ctx, id, p.Viewer)
}

func (s *Store) GetNodeDef(ctx context.Context, id string, v MacroViewer) (*NodeDef, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+nodeDefColumns+nodeDefFrom+` WHERE d.id = ?`, v.id(), v.id(), id)
	d, err := scanNodeDef(row, v)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get node def: %w", err)
	}
	return d, nil
}

// ListNodeDefs는 실행 엔진이 쓰는 **거르지 않은** 목록이다.
//
// 실행이 공개 설정을 보지 않는 이유: 그러지 않으면 남의 비공개 전역 노드를 하나 쓰는
// 공개 매크로가 만든 사람 외에는 아무에게도 돌지 않는다. 실행 권한은 노드 종류와
// 커넥션 권한으로 판정되고(Engine.Blockers), 공개 설정은 "팔레트에서 보이고 고칠 수
// 있는가"를 정할 뿐이다. 둘을 섞으면 매크로를 공유하는 순간 조각조각 깨진다.
func (s *Store) ListNodeDefs(ctx context.Context, macroID string) ([]*NodeDef, error) {
	return s.queryNodeDefs(ctx, SystemViewer(), macroID, "", nil)
}

// ListVisibleNodeDefs는 화면(팔레트·노드 관리)이 쓰는 목록이다.
//
// 전역 노드는 각자의 공개 설정으로 거른다. 매크로 전용 노드는 거르지 않는다 —
// 소속 매크로를 볼 수 있어야 이 함수까지 오고, 그렇다면 그 매크로의 노드도 볼 수 있다.
func (s *Store) ListVisibleNodeDefs(ctx context.Context, macroID string, v MacroViewer) ([]*NodeDef, error) {
	if !v.sees() {
		return []*NodeDef{}, nil
	}
	where, args := "", []any(nil)
	if !v.isSuper() {
		where = ` AND (d.scope = 'macro'
			OR d.visibility = 'public' OR d.created_by = ?
			OR EXISTS (SELECT 1 FROM macro_node_def_collaborators nc
				WHERE nc.def_id = d.id AND nc.user_id = ?))`
		args = []any{v.id(), v.id()}
	}
	return s.queryNodeDefs(ctx, v, macroID, where, args)
}

// queryNodeDefs는 전역 노드와, macroID가 주어지면 그 매크로 전용 노드를 함께 반환한다.
func (s *Store) queryNodeDefs(ctx context.Context, v MacroViewer, macroID, where string, whereArgs []any) ([]*NodeDef, error) {
	args := append([]any{v.id(), v.id(), nullString(macroID)}, whereArgs...)
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+nodeDefColumns+nodeDefFrom+`
		 WHERE (d.scope = 'global' OR (d.scope = 'macro' AND d.macro_id = ?))`+where+`
		 ORDER BY d.scope, d.name`, args...)
	if err != nil {
		return nil, fmt.Errorf("list node defs: %w", err)
	}
	defer rows.Close()

	out := []*NodeDef{}
	for rows.Next() {
		d, err := scanNodeDef(rows, v)
		if err != nil {
			return nil, fmt.Errorf("scan node def: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate node defs: %w", err)
	}
	return out, nil
}

type NodeDefVersion struct {
	DefID      string    `json:"defId"`
	Version    int       `json:"version"`
	Script     string    `json:"script"`
	Fields     string    `json:"fields"`
	Ports      string    `json:"ports"`
	Note       string    `json:"note"`
	AuthorName string    `json:"authorName"`
	CreatedAt  time.Time `json:"createdAt"`
}

func (s *Store) ListNodeDefVersions(ctx context.Context, defID string) ([]*NodeDefVersion, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT def_id, version, script, fields, ports, note, author_name, created_at
		 FROM macro_node_def_versions WHERE def_id = ? ORDER BY version DESC`, defID)
	if err != nil {
		return nil, fmt.Errorf("list node def versions: %w", err)
	}
	defer rows.Close()

	out := []*NodeDefVersion{}
	for rows.Next() {
		var v NodeDefVersion
		var createdAt string
		if err := rows.Scan(&v.DefID, &v.Version, &v.Script, &v.Fields, &v.Ports,
			&v.Note, &v.AuthorName, &createdAt); err != nil {
			return nil, fmt.Errorf("scan node def version: %w", err)
		}
		v.CreatedAt = parseTime(createdAt)
		out = append(out, &v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate node def versions: %w", err)
	}
	return out, nil
}

func (s *Store) DeleteNodeDef(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM macro_node_defs WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete node def: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ---------- 실행 이력 ----------

type MacroRun struct {
	ID          string         `json:"id"`
	MacroID     string         `json:"macroId,omitempty"`
	MacroName   string         `json:"macroName"`
	Version     int            `json:"version"`
	Status      string         `json:"status"`
	ActorID     string         `json:"actorId,omitempty"`
	ActorName   string         `json:"actorName"`
	ActorIP     string         `json:"actorIp,omitempty"`
	Params      map[string]any `json:"params"`
	Trigger     string         `json:"trigger"`
	ParentRunID string         `json:"parentRunId,omitempty"`
	StartedAt   time.Time      `json:"startedAt"`
	FinishedAt  *time.Time     `json:"finishedAt,omitempty"`
	DurationMs  int64          `json:"durationMs"`
	NodeCount   int            `json:"nodeCount"`
	Error       string         `json:"error,omitempty"`
}

type CreateRunParams struct {
	MacroID     string
	MacroName   string
	Version     int
	ActorID     string
	ActorName   string
	ActorIP     string
	Params      map[string]any
	Trigger     string
	ParentRunID string
}

func (s *Store) CreateMacroRun(ctx context.Context, p CreateRunParams) (string, error) {
	id := uuid.NewString()
	paramJSON, err := json.Marshal(p.Params)
	if err != nil {
		paramJSON = []byte("{}")
	}
	trigger := p.Trigger
	if trigger == "" {
		trigger = "manual"
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO macro_runs
		(id, macro_id, macro_name, version, status, actor_id, actor_name, actor_ip,
		 params, trigger, parent_run_id, started_at)
		VALUES (?, ?, ?, ?, 'running', ?, ?, ?, ?, ?, ?, ?)`,
		id, nullString(p.MacroID), p.MacroName, p.Version, nullString(p.ActorID),
		p.ActorName, p.ActorIP, string(paramJSON), trigger, nullString(p.ParentRunID),
		nowString()); err != nil {
		return "", fmt.Errorf("insert macro run: %w", err)
	}
	return id, nil
}

// AppendRunLog는 로그 한 줄을 남긴다.
//
// seq를 SELECT MAX+1로 계산하지 않고 호출부가 정하는 이유: 실행은 한 고루틴에서
// 순차적으로 일어나므로 호출부가 이미 순서를 알고 있고, 매 줄마다 조회를 한 번 더
// 하는 것은 로그가 수천 줄일 때 그대로 비용이 된다.
func (s *Store) AppendRunLog(ctx context.Context, runID string, seq int, level, nodeID, node, message string, detail map[string]any) error {
	detailJSON := "{}"
	if len(detail) > 0 {
		if b, err := json.Marshal(detail); err == nil {
			detailJSON = string(b)
		}
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO macro_run_logs
		(run_id, seq, at, level, node_id, node, message, detail)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		runID, seq, nowString(), level, nodeID, node, message, detailJSON); err != nil {
		return fmt.Errorf("insert run log: %w", err)
	}
	return nil
}

func (s *Store) FinishMacroRun(ctx context.Context, runID, status, errMsg string, durationMs int64, nodeCount int) error {
	_, err := s.db.ExecContext(ctx, `UPDATE macro_runs
		SET status = ?, error = ?, finished_at = ?, duration_ms = ?, node_count = ?
		WHERE id = ?`,
		status, errMsg, nowString(), durationMs, nodeCount, runID)
	if err != nil {
		return fmt.Errorf("finish macro run: %w", err)
	}
	return nil
}

const runColumns = `id, macro_id, macro_name, version, status, actor_id, actor_name,
	actor_ip, params, trigger, parent_run_id, started_at, finished_at, duration_ms,
	node_count, error`

func scanRun(row interface{ Scan(...any) error }) (*MacroRun, error) {
	var r MacroRun
	var macroID, actorID, parent, finishedAt sql.NullString
	var params, startedAt string
	err := row.Scan(&r.ID, &macroID, &r.MacroName, &r.Version, &r.Status, &actorID,
		&r.ActorName, &r.ActorIP, &params, &r.Trigger, &parent, &startedAt, &finishedAt,
		&r.DurationMs, &r.NodeCount, &r.Error)
	if err != nil {
		return nil, err
	}
	r.MacroID = macroID.String
	r.ActorID = actorID.String
	r.ParentRunID = parent.String
	r.StartedAt = parseTime(startedAt)
	r.FinishedAt = parseTimePtr(finishedAt)
	r.Params = map[string]any{}
	_ = json.Unmarshal([]byte(params), &r.Params)
	return &r, nil
}

func (s *Store) GetMacroRun(ctx context.Context, id string) (*MacroRun, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+runColumns+` FROM macro_runs WHERE id = ?`, id)
	r, err := scanRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get macro run: %w", err)
	}
	return r, nil
}

// runVisibleWhere는 실행 이력에 덧붙일 가시성 조건이다.
//
// 두 갈래로 보인다: **자기가 돌린 것**과 **볼 수 있는 매크로의 것**.
// 앞의 것이 필요한 이유는 매크로가 나중에 비공개로 바뀌거나 아예 삭제될 수 있어서다.
// 자기가 무엇을 실행했는지는 남이 설정을 바꿨다고 사라져서는 안 된다 — 그 기록은
// 매크로의 것이기도 하지만 실행한 사람의 것이기도 하다.
func runVisibleWhere(v MacroViewer) (string, []any) {
	if v.isSuper() {
		return "", nil
	}
	if !v.sees() {
		return ` AND 0`, nil
	}
	inner, args := macroVisibleWhere(v, "m")
	q := ` AND (r.actor_id = ? OR EXISTS (SELECT 1 FROM macros m
		WHERE m.id = r.macro_id` + inner + `))`
	return q, append([]any{v.id()}, args...)
}

// ListMacroRuns는 실행 이력을 최신순으로 반환한다.
// macroID가 비어 있으면 볼 수 있는 전체를 본다(실행 이력 화면).
func (s *Store) ListMacroRuns(ctx context.Context, macroID, status string, limit int, v MacroViewer) ([]*MacroRun, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	q := `SELECT ` + runColumns + ` FROM macro_runs r WHERE 1 = 1`
	args := []any{}
	if macroID != "" {
		q += ` AND r.macro_id = ?`
		args = append(args, macroID)
	}
	if status != "" {
		q += ` AND r.status = ?`
		args = append(args, status)
	}
	where, wargs := runVisibleWhere(v)
	q += where
	args = append(args, wargs...)
	q += ` ORDER BY started_at DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list macro runs: %w", err)
	}
	defer rows.Close()

	out := []*MacroRun{}
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, fmt.Errorf("scan macro run: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate macro runs: %w", err)
	}
	return out, nil
}

type RunLogEntry struct {
	Seq     int            `json:"seq"`
	At      time.Time      `json:"at"`
	Level   string         `json:"level"`
	NodeID  string         `json:"nodeId,omitempty"`
	Node    string         `json:"node,omitempty"`
	Message string         `json:"message"`
	Detail  map[string]any `json:"detail,omitempty"`
}

// ListRunLogs는 실행 로그를 순서대로 반환한다.
// afterSeq를 주면 그 다음 줄부터 반환한다(SSE 재접속 시 빠진 부분 보충).
func (s *Store) ListRunLogs(ctx context.Context, runID string, afterSeq int) ([]RunLogEntry, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT seq, at, level, node_id, node, message, detail
		 FROM macro_run_logs WHERE run_id = ? AND seq > ? ORDER BY seq`, runID, afterSeq)
	if err != nil {
		return nil, fmt.Errorf("list run logs: %w", err)
	}
	defer rows.Close()

	out := []RunLogEntry{}
	for rows.Next() {
		var e RunLogEntry
		var at, detail string
		if err := rows.Scan(&e.Seq, &at, &e.Level, &e.NodeID, &e.Node, &e.Message, &detail); err != nil {
			return nil, fmt.Errorf("scan run log: %w", err)
		}
		e.At = parseTime(at)
		if detail != "" && detail != "{}" {
			e.Detail = map[string]any{}
			_ = json.Unmarshal([]byte(detail), &e.Detail)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate run logs: %w", err)
	}
	return out, nil
}

// MarkStaleRunsFailed는 실행 중 상태로 남은 기록을 정리한다.
//
// 앱이 실행 도중 죽으면 그 행은 영원히 'running'으로 남는다. 화면은 그것을
// "지금 돌고 있음"으로 보여주고, 사용자는 끝나기를 기다린다. 부팅할 때 한 번
// 정리하는 것이 정확하다 — 우리는 그 실행이 이어지지 않았음을 안다.
func (s *Store) MarkStaleRunsFailed(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE macro_runs SET status = 'failed', error = '앱이 재시작되어 실행이 중단되었습니다',
			finished_at = ? WHERE status = 'running'`, nowString())
	if err != nil {
		return 0, fmt.Errorf("mark stale runs: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}
