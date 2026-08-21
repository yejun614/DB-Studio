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
)

// VCSIntegration은 Git 저장소 연동 설정이다.
//
// Token 필드는 API 응답에 절대 포함되지 않는다(json:"-"). 토큰이 한 번 응답에 실리면
// 브라우저 캐시·로그·확장 프로그램을 거쳐 어디로든 갈 수 있다. 화면에는 "설정됨"
// 여부만 보여준다.
type VCSIntegration struct {
	ID             string     `json:"id"`
	Name           string     `json:"name"`
	Provider       string     `json:"provider"`
	BaseURL        string     `json:"baseUrl,omitempty"`
	Repo           string     `json:"repo"`
	DefaultBranch  string     `json:"defaultBranch"`
	BranchTemplate string     `json:"branchTemplate"`
	PathTemplate   string     `json:"pathTemplate"`
	Username       string     `json:"username,omitempty"`
	Token          string     `json:"-"`
	HasToken       bool       `json:"hasToken"`
	ConnectionID   string     `json:"connectionId,omitempty"`
	Enabled        bool       `json:"enabled"`
	LastCheckAt    *time.Time `json:"lastCheckAt,omitempty"`
	LastCheckOK    *bool      `json:"lastCheckOk,omitempty"`
	LastCheckMsg   string     `json:"lastCheckMsg,omitempty"`
	// OwnerID는 이 연동의 주인이다. 조회·수정·삭제는 주인에게만 허용된다
	// (슈퍼 어드민도 예외가 아니다 — 개인 Git 계정의 토큰이 담기기 때문이다).
	OwnerID   string    `json:"ownerId,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// SaveVCSParams는 연동 생성/수정 입력이다.
type SaveVCSParams struct {
	ID             string
	Name           string
	Provider       string
	BaseURL        string
	Repo           string
	DefaultBranch  string
	BranchTemplate string
	PathTemplate   string
	Username       string
	// Token이 nil이면 기존 토큰을 유지한다. 수정 화면에서 토큰을 비워 두고 저장하는
	// 것이 "토큰을 지운다"로 해석되면, 이름만 고치려던 사용자의 연동이 망가진다.
	Token        *string
	ConnectionID string
	Enabled      bool
	// OwnerID는 생성할 때 반드시 채운다. 수정에서는 쓰지 않는다 —
	// 주인이 바뀌는 일은 없다.
	OwnerID string
}

func (s *Store) CreateVCSIntegration(ctx context.Context, p SaveVCSParams) (*VCSIntegration, error) {
	token := ""
	if p.Token != nil {
		token = *p.Token
	}
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("토큰을 입력하세요")
	}
	sealed, err := s.secret.Seal(token)
	if err != nil {
		return nil, fmt.Errorf("seal token: %w", err)
	}

	id := p.ID
	if id == "" {
		id = uuid.NewString()
	}
	now := nowString()
	if strings.TrimSpace(p.OwnerID) == "" {
		return nil, errors.New("연동에는 주인이 있어야 합니다")
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO vcs_integrations
		(id, owner_id, name, provider, base_url, repo, default_branch, branch_template,
		 path_template, username, token_enc, connection_id, enabled, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, p.OwnerID, p.Name, p.Provider, p.BaseURL, p.Repo, p.DefaultBranch,
		p.BranchTemplate, p.PathTemplate, p.Username, sealed,
		nullString(p.ConnectionID), boolInt(p.Enabled), now, now)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrDuplicateName
		}
		return nil, fmt.Errorf("insert vcs integration: %w", err)
	}
	return s.GetVCSIntegration(ctx, id, p.OwnerID, false)
}

func (s *Store) UpdateVCSIntegration(ctx context.Context, p SaveVCSParams) (*VCSIntegration, error) {
	// 토큰을 바꾸지 않는 경우와 바꾸는 경우를 나눈다. COALESCE로 한 문장에 담으면
	// "빈 문자열로 지우기"와 "유지"를 구분할 수 없다.
	args := []any{
		p.Name, p.Provider, p.BaseURL, p.Repo, p.DefaultBranch,
		p.BranchTemplate, p.PathTemplate, p.Username,
		nullString(p.ConnectionID), boolInt(p.Enabled), nowString(),
	}
	tokenClause := ""
	if p.Token != nil {
		if strings.TrimSpace(*p.Token) == "" {
			return nil, errors.New("토큰을 비워 둘 수 없습니다. 바꾸지 않으려면 입력하지 마세요")
		}
		sealed, err := s.secret.Seal(*p.Token)
		if err != nil {
			return nil, fmt.Errorf("seal token: %w", err)
		}
		tokenClause = ", token_enc = ?"
		args = append(args, sealed)
	}
	args = append(args, p.ID, p.OwnerID)

	// WHERE에 주인을 함께 넣는다. 남의 연동은 "없는 것"이 되어 ErrNotFound가 된다.
	res, err := s.db.ExecContext(ctx, `UPDATE vcs_integrations SET
		name = ?, provider = ?, base_url = ?, repo = ?, default_branch = ?,
		branch_template = ?, path_template = ?, username = ?,
		connection_id = ?, enabled = ?, updated_at = ?`+tokenClause+`
		WHERE id = ? AND owner_id = ?`, args...)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrDuplicateName
		}
		return nil, fmt.Errorf("update vcs integration: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrNotFound
	}
	return s.GetVCSIntegration(ctx, p.ID, p.OwnerID, false)
}

// GetVCSIntegration은 연동을 읽는다. withToken이 true면 토큰을 복호화해 담는다.
//
// 토큰 복호화를 기본값으로 두지 않는 이유: 목록·화면 표시 경로에서 실수로 토큰을
// 메모리에 올리고 응답에 흘리는 일을 구조적으로 막는다.
// ownerID를 함께 받는 이유: "찾은 뒤에 주인을 비교한다"는 호출부마다 반복되고,
// 한 곳이라도 빠뜨리면 남의 Git 계정을 읽는 경로가 된다. 질의 자체를 좁혀
// 그 실수를 할 자리를 없앤다. 없는 것과 남의 것은 똑같이 ErrNotFound다 —
// 존재 여부조차 알려줄 이유가 없다.
func (s *Store) GetVCSIntegration(ctx context.Context, id, ownerID string, withToken bool) (*VCSIntegration, error) {
	row := s.db.QueryRowContext(ctx, vcsSelect+` WHERE id = ? AND owner_id = ?`, id, ownerID)
	v, err := s.scanVCS(row, withToken)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return v, err
}

const vcsSelect = `SELECT
	id, name, provider, base_url, repo, default_branch, branch_template, path_template,
	username, token_enc, COALESCE(connection_id, ''), enabled,
	last_check_at, last_check_ok, last_check_msg,
	owner_id, created_at, updated_at
	FROM vcs_integrations`

// ListVCSIntegrations는 연동 목록을 반환한다 (토큰 제외).
//
// connectionID가 비어 있지 않으면 그 커넥션에서 쓸 수 있는 연동만 반환한다:
// 그 커넥션 전용 연동 + 커넥션이 지정되지 않은 공용 연동.
func (s *Store) ListVCSIntegrations(ctx context.Context, ownerID, connectionID string) ([]*VCSIntegration, error) {
	query := vcsSelect + ` WHERE owner_id = ?`
	args := []any{ownerID}
	if connectionID != "" {
		query += ` AND (connection_id IS NULL OR connection_id = ?)`
		args = append(args, connectionID)
	}
	query += ` ORDER BY name`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list vcs integrations: %w", err)
	}
	defer rows.Close()

	out := []*VCSIntegration{}
	for rows.Next() {
		v, err := s.scanVCS(rows, false)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate vcs integrations: %w", err)
	}
	return out, nil
}

func (s *Store) scanVCS(row interface{ Scan(...any) error }, withToken bool) (*VCSIntegration, error) {
	var v VCSIntegration
	var tokenEnc, createdAt, updatedAt string
	var enabled int
	var lastCheckAt sql.NullString
	var lastCheckOK sql.NullInt64

	if err := row.Scan(&v.ID, &v.Name, &v.Provider, &v.BaseURL, &v.Repo,
		&v.DefaultBranch, &v.BranchTemplate, &v.PathTemplate, &v.Username, &tokenEnc,
		&v.ConnectionID, &enabled, &lastCheckAt, &lastCheckOK, &v.LastCheckMsg,
		&v.OwnerID, &createdAt, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		return nil, fmt.Errorf("scan vcs integration: %w", err)
	}

	v.Enabled = enabled != 0
	v.HasToken = tokenEnc != ""
	v.CreatedAt, v.UpdatedAt = parseTime(createdAt), parseTime(updatedAt)
	v.LastCheckAt = parseTimePtr(lastCheckAt)
	if lastCheckOK.Valid {
		ok := lastCheckOK.Int64 != 0
		v.LastCheckOK = &ok
	}
	if withToken && tokenEnc != "" {
		token, err := s.secret.Open(tokenEnc)
		if err != nil {
			return nil, fmt.Errorf("open vcs token: %w", err)
		}
		v.Token = token
	}
	return &v, nil
}

func (s *Store) DeleteVCSIntegration(ctx context.Context, id, ownerID string) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM vcs_integrations WHERE id = ? AND owner_id = ?`, id, ownerID)
	if err != nil {
		return fmt.Errorf("delete vcs integration: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// RecordVCSCheck는 연결 확인 결과를 기록한다.
func (s *Store) RecordVCSCheck(ctx context.Context, id string, ok bool, message string) error {
	if len(message) > 1000 {
		message = message[:1000]
	}
	_, err := s.db.ExecContext(ctx, `UPDATE vcs_integrations SET
		last_check_at = ?, last_check_ok = ?, last_check_msg = ?, updated_at = ?
		WHERE id = ?`, nowString(), boolInt(ok), message, nowString(), id)
	if err != nil {
		return fmt.Errorf("record vcs check: %w", err)
	}
	return nil
}

// ---------- 푸시 이력 ----------

// VCSPush는 한 번의 푸시 기록이다.
type VCSPush struct {
	ID              int64     `json:"id"`
	IntegrationID   string    `json:"integrationId"`
	IntegrationName string    `json:"integrationName,omitempty"`
	Provider        string    `json:"provider,omitempty"`
	MigrationID     string    `json:"migrationId,omitempty"`
	MigrationTitle  string    `json:"migrationTitle,omitempty"`
	Branch          string    `json:"branch"`
	BranchCreated   bool      `json:"branchCreated"`
	CommitSHA       string    `json:"commitSha,omitempty"`
	CommitURL       string    `json:"commitUrl,omitempty"`
	PRNumber        *int      `json:"prNumber,omitempty"`
	PRURL           string    `json:"prUrl,omitempty"`
	PRExisting      bool      `json:"prExisting"`
	Files           []string  `json:"files"`
	Status          string    `json:"status"`
	Error           string    `json:"error,omitempty"`
	ActorID         string    `json:"actorId,omitempty"`
	ActorName       string    `json:"actorName,omitempty"`
	CreatedAt       time.Time `json:"createdAt"`
}

func (s *Store) RecordVCSPush(ctx context.Context, p *VCSPush) error {
	if p.Files == nil {
		p.Files = []string{}
	}
	filesJSON, err := json.Marshal(p.Files)
	if err != nil {
		return fmt.Errorf("marshal files: %w", err)
	}
	if len(p.Error) > 2000 {
		p.Error = p.Error[:2000]
	}
	now := nowString()
	res, err := s.db.ExecContext(ctx, `INSERT INTO vcs_pushes
		(integration_id, migration_id, migration_title, branch, branch_created,
		 commit_sha, commit_url, pr_number, pr_url, pr_existing, files, status, error,
		 actor_id, actor_name, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.IntegrationID, nullString(p.MigrationID), p.MigrationTitle, p.Branch,
		boolInt(p.BranchCreated), p.CommitSHA, p.CommitURL, p.PRNumber, p.PRURL,
		boolInt(p.PRExisting), string(filesJSON), p.Status, p.Error,
		nullString(p.ActorID), p.ActorName, now)
	if err != nil {
		return fmt.Errorf("insert vcs push: %w", err)
	}
	p.ID, _ = res.LastInsertId()
	p.CreatedAt = parseTime(now)
	return nil
}

// ListVCSPushes는 푸시 이력을 반환한다.
// migrationID가 비어 있지 않으면 그 마이그레이션의 이력만 반환한다.
func (s *Store) ListVCSPushes(ctx context.Context, migrationID string, limit int) ([]*VCSPush, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	query := `SELECT
		p.id, p.integration_id, COALESCE(i.name, ''), COALESCE(i.provider, ''),
		COALESCE(p.migration_id, ''), p.migration_title, p.branch, p.branch_created,
		p.commit_sha, p.commit_url, p.pr_number, p.pr_url, p.pr_existing,
		p.files, p.status, p.error, COALESCE(p.actor_id, ''), p.actor_name, p.created_at
		FROM vcs_pushes p
		LEFT JOIN vcs_integrations i ON i.id = p.integration_id`
	args := []any{}
	if migrationID != "" {
		query += ` WHERE p.migration_id = ?`
		args = append(args, migrationID)
	}
	query += ` ORDER BY p.id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list vcs pushes: %w", err)
	}
	defer rows.Close()

	out := []*VCSPush{}
	for rows.Next() {
		var p VCSPush
		var filesJSON, createdAt string
		var branchCreated, prExisting int
		var prNumber sql.NullInt64
		if err := rows.Scan(&p.ID, &p.IntegrationID, &p.IntegrationName, &p.Provider,
			&p.MigrationID, &p.MigrationTitle, &p.Branch, &branchCreated,
			&p.CommitSHA, &p.CommitURL, &prNumber, &p.PRURL, &prExisting,
			&filesJSON, &p.Status, &p.Error, &p.ActorID, &p.ActorName, &createdAt); err != nil {
			return nil, fmt.Errorf("scan vcs push: %w", err)
		}
		p.BranchCreated = branchCreated != 0
		p.PRExisting = prExisting != 0
		if prNumber.Valid {
			n := int(prNumber.Int64)
			p.PRNumber = &n
		}
		p.Files = []string{}
		_ = json.Unmarshal([]byte(filesJSON), &p.Files)
		p.CreatedAt = parseTime(createdAt)
		out = append(out, &p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate vcs pushes: %w", err)
	}
	return out, nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
