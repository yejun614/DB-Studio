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

	"dbstudio/internal/ai"
)

// ---------- 프로바이더 ----------

// AIProvider는 LLM 프로바이더 설정이다.
//
// APIKey는 `json:"-"`이라 응답에 실리지 않는다. 화면에는 설정 여부만 보여준다.
type AIProvider struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Provider     string `json:"provider"`
	BaseURL      string `json:"baseUrl,omitempty"`
	APIKey       string `json:"-"`
	HasKey       bool   `json:"hasKey"`
	DefaultModel string `json:"defaultModel,omitempty"`
	// Models는 이 프로바이더로 쓸 수 있는 모델 목록이다.
	//
	// 비어 있으면 제한이 없다(어떤 모델 이름이든 쓸 수 있다). 관리자가 목록을 채우면
	// 그때부터 화이트리스트가 된다 — 제한은 명시적으로 고른 결과여야 하고,
	// 기본값이 "아무것도 못 쓴다"가 되면 기존 설치가 조용히 멈춘다.
	Models       []string   `json:"models"`
	Enabled      bool       `json:"enabled"`
	IsDefault    bool       `json:"isDefault"`
	LastCheckAt  *time.Time `json:"lastCheckAt,omitempty"`
	LastCheckOK  *bool      `json:"lastCheckOk,omitempty"`
	LastCheckMsg string     `json:"lastCheckMsg,omitempty"`
	CreatedBy    string     `json:"createdBy,omitempty"`
	CreatedAt    time.Time  `json:"createdAt"`
	UpdatedAt    time.Time  `json:"updatedAt"`
}

// SaveAIProviderParams는 프로바이더 생성/수정 입력이다.
type SaveAIProviderParams struct {
	ID           string
	Name         string
	Provider     string
	BaseURL      string
	DefaultModel string
	Models       []string
	// APIKey가 nil이면 기존 키를 유지한다 (수정 화면에서 비워 두는 것은 "유지"다).
	APIKey    *string
	Enabled   bool
	IsDefault bool
	CreatedBy string
}

func (s *Store) CreateAIProvider(ctx context.Context, p SaveAIProviderParams) (*AIProvider, error) {
	if p.APIKey == nil || strings.TrimSpace(*p.APIKey) == "" {
		return nil, errors.New("API 키를 입력하세요")
	}
	sealed, err := s.secret.Seal(*p.APIKey)
	if err != nil {
		return nil, fmt.Errorf("seal api key: %w", err)
	}
	id := p.ID
	if id == "" {
		id = uuid.NewString()
	}
	now := nowString()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin create ai provider: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `INSERT INTO ai_providers
		(id, name, provider, base_url, api_key_enc, default_model, allowed_models,
		 enabled, is_default, created_by, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, p.Name, p.Provider, p.BaseURL, sealed, p.DefaultModel, encodeModels(p.Models),
		boolInt(p.Enabled), boolInt(p.IsDefault), nullString(p.CreatedBy), now, now); err != nil {
		if isUniqueViolation(err) {
			return nil, ErrDuplicateName
		}
		return nil, fmt.Errorf("insert ai provider: %w", err)
	}
	if err := clearOtherDefaults(ctx, tx, id, p.IsDefault); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit create ai provider: %w", err)
	}
	return s.GetAIProvider(ctx, id, false)
}

// clearOtherDefaults는 기본 프로바이더를 하나로 유지한다.
// 둘 이상이면 새 세션이 어느 것을 쓸지 예측할 수 없다.
func clearOtherDefaults(ctx context.Context, tx *sql.Tx, keepID string, isDefault bool) error {
	if !isDefault {
		return nil
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE ai_providers SET is_default = 0 WHERE id <> ?`, keepID); err != nil {
		return fmt.Errorf("clear other defaults: %w", err)
	}
	return nil
}

func (s *Store) UpdateAIProvider(ctx context.Context, p SaveAIProviderParams) (*AIProvider, error) {
	args := []any{
		p.Name, p.Provider, p.BaseURL, p.DefaultModel, encodeModels(p.Models),
		boolInt(p.Enabled), boolInt(p.IsDefault), nowString(),
	}
	keyClause := ""
	if p.APIKey != nil {
		if strings.TrimSpace(*p.APIKey) == "" {
			return nil, errors.New("API 키를 비워 둘 수 없습니다. 바꾸지 않으려면 입력하지 마세요")
		}
		sealed, err := s.secret.Seal(*p.APIKey)
		if err != nil {
			return nil, fmt.Errorf("seal api key: %w", err)
		}
		keyClause = ", api_key_enc = ?"
		args = append(args, sealed)
	}
	args = append(args, p.ID)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin update ai provider: %w", err)
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `UPDATE ai_providers SET
		name = ?, provider = ?, base_url = ?, default_model = ?, allowed_models = ?,
		enabled = ?, is_default = ?, updated_at = ?`+keyClause+`
		WHERE id = ?`, args...)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrDuplicateName
		}
		return nil, fmt.Errorf("update ai provider: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrNotFound
	}
	if err := clearOtherDefaults(ctx, tx, p.ID, p.IsDefault); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit update ai provider: %w", err)
	}
	return s.GetAIProvider(ctx, p.ID, false)
}

const aiProviderSelect = `SELECT
	id, name, provider, base_url, api_key_enc, default_model, allowed_models,
	enabled, is_default, last_check_at, last_check_ok, last_check_msg,
	COALESCE(created_by, ''), created_at, updated_at
	FROM ai_providers`

// encodeModels는 허용 모델 목록을 저장 형식으로 만든다.
//
// nil을 "[]"로 적는 이유: 열이 NOT NULL이고, 읽는 쪽이 빈 문자열과 빈 배열을
// 따로 다루게 되면 "제한 없음"의 표현이 두 가지가 된다.
func encodeModels(models []string) string {
	if len(models) == 0 {
		return "[]"
	}
	data, err := json.Marshal(models)
	if err != nil {
		return "[]"
	}
	return string(data)
}

// decodeModels는 저장된 목록을 읽는다.
//
// 깨진 값에 오류를 내지 않고 빈 목록으로 읽는 이유: 이 열이 망가졌다고 프로바이더
// 조회 전체가 실패하면 화면이 열리지 않아 고칠 수단까지 사라진다. 빈 목록은
// "제한 없음"이므로 어시스턴트는 계속 동작하고, 관리자가 다시 저장하면 복구된다.
func decodeModels(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "[]" {
		return []string{}
	}
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return []string{}
	}
	return out
}

func (s *Store) GetAIProvider(ctx context.Context, id string, withKey bool) (*AIProvider, error) {
	row := s.db.QueryRowContext(ctx, aiProviderSelect+` WHERE id = ?`, id)
	p, err := s.scanAIProvider(row, withKey)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return p, err
}

// DefaultAIProvider는 새 세션이 쓸 프로바이더를 고른다.
// is_default가 없으면 활성 프로바이더 중 이름 순으로 첫 번째를 쓴다.
func (s *Store) DefaultAIProvider(ctx context.Context, withKey bool) (*AIProvider, error) {
	row := s.db.QueryRowContext(ctx, aiProviderSelect+`
		WHERE enabled = 1 ORDER BY is_default DESC, name LIMIT 1`)
	p, err := s.scanAIProvider(row, withKey)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return p, err
}

func (s *Store) ListAIProviders(ctx context.Context) ([]*AIProvider, error) {
	rows, err := s.db.QueryContext(ctx, aiProviderSelect+` ORDER BY is_default DESC, name`)
	if err != nil {
		return nil, fmt.Errorf("list ai providers: %w", err)
	}
	defer rows.Close()

	out := []*AIProvider{}
	for rows.Next() {
		p, err := s.scanAIProvider(rows, false)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate ai providers: %w", err)
	}
	return out, nil
}

func (s *Store) scanAIProvider(row interface{ Scan(...any) error }, withKey bool) (*AIProvider, error) {
	var p AIProvider
	var keyEnc, models, createdAt, updatedAt string
	var enabled, isDefault int
	var lastCheckAt sql.NullString
	var lastCheckOK sql.NullInt64

	if err := row.Scan(&p.ID, &p.Name, &p.Provider, &p.BaseURL, &keyEnc, &p.DefaultModel,
		&models, &enabled, &isDefault, &lastCheckAt, &lastCheckOK, &p.LastCheckMsg,
		&p.CreatedBy, &createdAt, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		return nil, fmt.Errorf("scan ai provider: %w", err)
	}
	p.Enabled = enabled != 0
	p.IsDefault = isDefault != 0
	p.HasKey = keyEnc != ""
	p.Models = decodeModels(models)
	p.CreatedAt, p.UpdatedAt = parseTime(createdAt), parseTime(updatedAt)
	p.LastCheckAt = parseTimePtr(lastCheckAt)
	if lastCheckOK.Valid {
		ok := lastCheckOK.Int64 != 0
		p.LastCheckOK = &ok
	}
	if withKey && keyEnc != "" {
		key, err := s.secret.Open(keyEnc)
		if err != nil {
			return nil, fmt.Errorf("open ai api key: %w", err)
		}
		p.APIKey = key
	}
	return &p, nil
}

func (s *Store) DeleteAIProvider(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM ai_providers WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete ai provider: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) RecordAIProviderCheck(ctx context.Context, id string, ok bool, message string) error {
	if len(message) > 1000 {
		message = message[:1000]
	}
	_, err := s.db.ExecContext(ctx, `UPDATE ai_providers SET
		last_check_at = ?, last_check_ok = ?, last_check_msg = ?, updated_at = ?
		WHERE id = ?`, nowString(), boolInt(ok), message, nowString(), id)
	if err != nil {
		return fmt.Errorf("record ai provider check: %w", err)
	}
	return nil
}

// ---------- 세션 ----------

// AISession은 하나의 대화다.
type AISession struct {
	ID           string `json:"id"`
	UserID       string `json:"userId"`
	Title        string `json:"title"`
	ProviderID   string `json:"providerId,omitempty"`
	ProviderName string `json:"providerName,omitempty"`
	Model        string `json:"model,omitempty"`
	ConnectionID string `json:"connectionId,omitempty"`
	// ERDDocumentID가 있으면 그 ERD 초안에 대한 대화다.
	// 그때는 툴 상자도 그 문서를 고치는 것으로 바뀐다(api/ai_tools_erd.go).
	ERDDocumentID string    `json:"erdDocumentId,omitempty"`
	InputTokens   int       `json:"inputTokens"`
	OutputTokens  int       `json:"outputTokens"`
	Archived      bool      `json:"archived"`
	CreatedAt     time.Time `json:"createdAt"`
	UpdatedAt     time.Time `json:"updatedAt"`
	// MessageCount는 목록 표시용이다.
	MessageCount int `json:"messageCount"`
	// PendingCount는 승인을 기다리는 제안 수다. 목록에서 눈에 띄게 표시한다.
	PendingCount int `json:"pendingCount"`
}

// CreateAISessionParams는 새 대화의 입력이다.
//
// 인자를 구조체로 바꾼 이유: 문자열 다섯 개가 나란히 있는 호출은 순서를 한 번
// 바꿔 적어도 컴파일러가 잡아주지 못한다(전부 string이다).
type CreateAISessionParams struct {
	UserID        string
	Title         string
	ProviderID    string
	Model         string
	ConnectionID  string
	ERDDocumentID string
}

func (s *Store) CreateAISession(ctx context.Context, p CreateAISessionParams) (*AISession, error) {
	id := uuid.NewString()
	now := nowString()
	_, err := s.db.ExecContext(ctx, `INSERT INTO ai_sessions
		(id, user_id, title, provider_id, model, connection_id, erd_document_id, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, p.UserID, p.Title, nullString(p.ProviderID), p.Model,
		nullString(p.ConnectionID), nullString(p.ERDDocumentID), now, now)
	if err != nil {
		return nil, fmt.Errorf("insert ai session: %w", err)
	}
	return s.GetAISession(ctx, id)
}

const aiSessionSelect = `SELECT
	s.id, s.user_id, s.title, COALESCE(s.provider_id, ''), COALESCE(p.name, ''),
	s.model, COALESCE(s.connection_id, ''), COALESCE(s.erd_document_id, ''),
	s.input_tokens, s.output_tokens,
	s.archived, s.created_at, s.updated_at,
	(SELECT COUNT(*) FROM ai_messages m WHERE m.session_id = s.id),
	(SELECT COUNT(*) FROM ai_pending_actions a WHERE a.session_id = s.id AND a.status = 'pending')
	FROM ai_sessions s
	LEFT JOIN ai_providers p ON p.id = s.provider_id`

func (s *Store) GetAISession(ctx context.Context, id string) (*AISession, error) {
	row := s.db.QueryRowContext(ctx, aiSessionSelect+` WHERE s.id = ?`, id)
	sess, err := scanAISession(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return sess, err
}

// ListAISessions는 한 사용자의 세션만 반환한다.
//
// userID를 필수로 받는 이유: 대화에는 그 사람의 권한으로 조회한 데이터가 들어 있다.
// "전체 조회" 경로를 아예 만들지 않으면 실수로 남의 대화를 노출할 수 없다.
// ListERDAISessions는 한 ERD 초안에 대한 내 대화 목록이다.
//
// 문서로 좁히는 것과 사용자로 좁히는 것을 **함께** 한다. AI 대화는 개인의 것이므로
// 같은 문서를 열어도 남의 대화는 보이지 않는다.
func (s *Store) ListERDAISessions(ctx context.Context, docID, userID string) ([]*AISession, error) {
	if strings.TrimSpace(docID) == "" || strings.TrimSpace(userID) == "" {
		return nil, errors.New("문서와 사용자를 지정하세요")
	}
	rows, err := s.db.QueryContext(ctx, aiSessionSelect+`
		WHERE s.erd_document_id = ? AND s.user_id = ? AND s.archived = 0
		ORDER BY s.updated_at DESC LIMIT 50`, docID, userID)
	if err != nil {
		return nil, fmt.Errorf("list erd ai sessions: %w", err)
	}
	defer rows.Close()
	out := []*AISession{}
	for rows.Next() {
		sess, err := scanAISession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sess)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate erd ai sessions: %w", err)
	}
	return out, nil
}

func (s *Store) ListAISessions(ctx context.Context, userID string, includeArchived bool, limit int) ([]*AISession, error) {
	if strings.TrimSpace(userID) == "" {
		return nil, errors.New("사용자 ID가 필요합니다")
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	// 초안에 매인 대화는 여기 나오지 않는다. 그 대화는 ERD 화면에 속하고 툴 상자도
	// 다르다 — 어시스턴트 목록에서 열면 문서 편집 툴이 붙은 대화를 DB 대화인 줄 알고
	// 이어가게 된다. 초안 대화는 ListERDAISessions로 문서별로 본다.
	query := aiSessionSelect + ` WHERE s.user_id = ? AND s.erd_document_id IS NULL`
	if !includeArchived {
		query += ` AND s.archived = 0`
	}
	query += ` ORDER BY s.updated_at DESC LIMIT ?`

	rows, err := s.db.QueryContext(ctx, query, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("list ai sessions: %w", err)
	}
	defer rows.Close()

	out := []*AISession{}
	for rows.Next() {
		sess, err := scanAISession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sess)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate ai sessions: %w", err)
	}
	return out, nil
}

func scanAISession(row interface{ Scan(...any) error }) (*AISession, error) {
	var s AISession
	var createdAt, updatedAt string
	var archived int
	if err := row.Scan(&s.ID, &s.UserID, &s.Title, &s.ProviderID, &s.ProviderName,
		&s.Model, &s.ConnectionID, &s.ERDDocumentID, &s.InputTokens, &s.OutputTokens,
		&archived, &createdAt, &updatedAt, &s.MessageCount, &s.PendingCount); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		return nil, fmt.Errorf("scan ai session: %w", err)
	}
	s.Archived = archived != 0
	s.CreatedAt, s.UpdatedAt = parseTime(createdAt), parseTime(updatedAt)
	return &s, nil
}

// UpdateAISessionMeta는 제목·모델·대상 커넥션·보관 여부를 바꾼다.
func (s *Store) UpdateAISessionMeta(ctx context.Context, id, title, providerID, model, connectionID string, archived bool) error {
	res, err := s.db.ExecContext(ctx, `UPDATE ai_sessions SET
		title = ?, provider_id = ?, model = ?, connection_id = ?, archived = ?, updated_at = ?
		WHERE id = ?`,
		title, nullString(providerID), model, nullString(connectionID),
		boolInt(archived), nowString(), id)
	if err != nil {
		return fmt.Errorf("update ai session: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) DeleteAISession(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM ai_sessions WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete ai session: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// AddAISessionUsage는 토큰 사용량을 누적한다.
func (s *Store) AddAISessionUsage(ctx context.Context, id string, in, out int) error {
	_, err := s.db.ExecContext(ctx, `UPDATE ai_sessions SET
		input_tokens = input_tokens + ?, output_tokens = output_tokens + ?, updated_at = ?
		WHERE id = ?`, in, out, nowString(), id)
	if err != nil {
		return fmt.Errorf("add session usage: %w", err)
	}
	return nil
}

// ---------- 메시지 ----------

// AIMessage는 대화의 한 항목이다.
type AIMessage struct {
	ID           int64           `json:"id"`
	SessionID    string          `json:"sessionId"`
	Role         string          `json:"role"`
	Text         string          `json:"text,omitempty"`
	ToolCalls    []ai.ToolCall   `json:"toolCalls,omitempty"`
	ToolResults  []ai.ToolResult `json:"toolResults,omitempty"`
	InputTokens  int             `json:"inputTokens,omitempty"`
	OutputTokens int             `json:"outputTokens,omitempty"`
	Error        string          `json:"error,omitempty"`
	CreatedAt    time.Time       `json:"createdAt"`
}

func (s *Store) AddAIMessage(ctx context.Context, m *AIMessage) error {
	if m.ToolCalls == nil {
		m.ToolCalls = []ai.ToolCall{}
	}
	if m.ToolResults == nil {
		m.ToolResults = []ai.ToolResult{}
	}
	callsJSON, err := json.Marshal(m.ToolCalls)
	if err != nil {
		return fmt.Errorf("marshal tool calls: %w", err)
	}
	resultsJSON, err := json.Marshal(m.ToolResults)
	if err != nil {
		return fmt.Errorf("marshal tool results: %w", err)
	}
	now := nowString()
	res, err := s.db.ExecContext(ctx, `INSERT INTO ai_messages
		(session_id, role, text, tool_calls, tool_results, input_tokens, output_tokens, error, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.SessionID, m.Role, m.Text, string(callsJSON), string(resultsJSON),
		m.InputTokens, m.OutputTokens, m.Error, now)
	if err != nil {
		return fmt.Errorf("insert ai message: %w", err)
	}
	m.ID, _ = res.LastInsertId()
	m.CreatedAt = parseTime(now)

	// 세션의 updated_at을 함께 올려 목록 정렬이 실제 활동을 반영하게 한다.
	if _, err := s.db.ExecContext(ctx,
		`UPDATE ai_sessions SET updated_at = ? WHERE id = ?`, now, m.SessionID); err != nil {
		return fmt.Errorf("touch ai session: %w", err)
	}
	return nil
}

func (s *Store) ListAIMessages(ctx context.Context, sessionID string, limit int) ([]*AIMessage, error) {
	if limit <= 0 || limit > 1000 {
		limit = 500
	}
	rows, err := s.db.QueryContext(ctx, `SELECT
		id, session_id, role, text, tool_calls, tool_results,
		input_tokens, output_tokens, error, created_at
		FROM ai_messages WHERE session_id = ? ORDER BY id LIMIT ?`, sessionID, limit)
	if err != nil {
		return nil, fmt.Errorf("list ai messages: %w", err)
	}
	defer rows.Close()

	out := []*AIMessage{}
	for rows.Next() {
		var m AIMessage
		var callsJSON, resultsJSON, createdAt string
		if err := rows.Scan(&m.ID, &m.SessionID, &m.Role, &m.Text, &callsJSON, &resultsJSON,
			&m.InputTokens, &m.OutputTokens, &m.Error, &createdAt); err != nil {
			return nil, fmt.Errorf("scan ai message: %w", err)
		}
		m.ToolCalls = []ai.ToolCall{}
		_ = json.Unmarshal([]byte(callsJSON), &m.ToolCalls)
		m.ToolResults = []ai.ToolResult{}
		_ = json.Unmarshal([]byte(resultsJSON), &m.ToolResults)
		m.CreatedAt = parseTime(createdAt)
		out = append(out, &m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate ai messages: %w", err)
	}
	return out, nil
}

// GetAIMessage는 한 대화 안의 메시지 하나를 읽는다.
//
// 세션 아이디를 함께 받는 이유: 아이디만으로 찾으면 남의 대화의 메시지를 가리키는
// 요청이 통과한다. 메시지 아이디는 앱 전체에서 증가하는 숫자라 추측하기도 쉽다.
func (s *Store) GetAIMessage(ctx context.Context, sessionID string, id int64) (*AIMessage, error) {
	var m AIMessage
	var callsJSON, resultsJSON, createdAt string
	err := s.db.QueryRowContext(ctx, `SELECT
		id, session_id, role, text, tool_calls, tool_results,
		input_tokens, output_tokens, error, created_at
		FROM ai_messages WHERE id = ? AND session_id = ?`, id, sessionID).
		Scan(&m.ID, &m.SessionID, &m.Role, &m.Text, &callsJSON, &resultsJSON,
			&m.InputTokens, &m.OutputTokens, &m.Error, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get ai message: %w", err)
	}
	m.ToolCalls = []ai.ToolCall{}
	_ = json.Unmarshal([]byte(callsJSON), &m.ToolCalls)
	m.ToolResults = []ai.ToolResult{}
	_ = json.Unmarshal([]byte(resultsJSON), &m.ToolResults)
	m.CreatedAt = parseTime(createdAt)
	return &m, nil
}

// TruncateAIMessagesFrom은 그 메시지와 그 뒤에 온 것을 모두 지운다.
//
// 말을 고쳐 다시 보내는 것은 새 말을 더하는 일이 아니라 **그 자리부터 다시 하는**
// 일이다. 고친 말 뒤에 옛 답이 남아 있으면 대화는 있지도 않았던 문답이 되고, 다음
// 요청의 문맥으로 그 옛 답이 그대로 모델에게 간다.
//
// 남은 개수를 함께 돌려준다. 첫 말을 고쳤다면 대화가 비게 되고, 그때는 그 말에서
// 뽑아 둔 제목도 옛 말이라 다시 정해야 한다.
func (s *Store) TruncateAIMessagesFrom(ctx context.Context, sessionID string, fromID int64) (int64, int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM ai_messages WHERE session_id = ? AND id >= ?`, sessionID, fromID)
	if err != nil {
		return 0, 0, fmt.Errorf("truncate ai messages: %w", err)
	}
	removed, _ := res.RowsAffected()

	var left int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM ai_messages WHERE session_id = ?`, sessionID).Scan(&left); err != nil {
		return removed, 0, fmt.Errorf("count ai messages: %w", err)
	}
	return removed, left, nil
}

// ---------- 보류 중인 제안 ----------

// 제안 상태.
const (
	PendingStatusPending  = "pending"
	PendingStatusApproved = "approved"
	PendingStatusRejected = "rejected"
	PendingStatusFailed   = "failed"
	PendingStatusExpired  = "expired"
)

// AIPendingAction은 사용자 승인을 기다리는 쓰기 제안이다.
type AIPendingAction struct {
	ID         string          `json:"id"`
	SessionID  string          `json:"sessionId"`
	MessageID  *int64          `json:"messageId,omitempty"`
	ToolCallID string          `json:"toolCallId"`
	ToolName   string          `json:"toolName"`
	Arguments  json.RawMessage `json:"arguments"`
	Summary    string          `json:"summary"`
	Preview    json.RawMessage `json:"preview,omitempty"`
	Status     string          `json:"status"`
	Result     string          `json:"result,omitempty"`
	Error      string          `json:"error,omitempty"`
	DecidedBy  string          `json:"decidedBy,omitempty"`
	DecidedAt  *time.Time      `json:"decidedAt,omitempty"`
	CreatedAt  time.Time       `json:"createdAt"`
}

func (s *Store) CreateAIPendingAction(ctx context.Context, a *AIPendingAction) error {
	if a.ID == "" {
		a.ID = uuid.NewString()
	}
	if len(a.Arguments) == 0 {
		a.Arguments = json.RawMessage("{}")
	}
	if len(a.Preview) == 0 {
		a.Preview = json.RawMessage("{}")
	}
	now := nowString()
	_, err := s.db.ExecContext(ctx, `INSERT INTO ai_pending_actions
		(id, session_id, message_id, tool_call_id, tool_name, arguments, summary, preview,
		 status, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ID, a.SessionID, a.MessageID, a.ToolCallID, a.ToolName,
		string(a.Arguments), a.Summary, string(a.Preview), PendingStatusPending, now)
	if err != nil {
		return fmt.Errorf("insert pending action: %w", err)
	}
	a.Status = PendingStatusPending
	a.CreatedAt = parseTime(now)
	return nil
}

func (s *Store) GetAIPendingAction(ctx context.Context, id string) (*AIPendingAction, error) {
	row := s.db.QueryRowContext(ctx, pendingSelect+` WHERE id = ?`, id)
	a, err := scanPending(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return a, err
}

const pendingSelect = `SELECT
	id, session_id, message_id, tool_call_id, tool_name, arguments, summary, preview,
	status, result, error, COALESCE(decided_by, ''), decided_at, created_at
	FROM ai_pending_actions`

func (s *Store) ListAIPendingActions(ctx context.Context, sessionID string) ([]*AIPendingAction, error) {
	rows, err := s.db.QueryContext(ctx, pendingSelect+` WHERE session_id = ? ORDER BY id`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("list pending actions: %w", err)
	}
	defer rows.Close()

	out := []*AIPendingAction{}
	for rows.Next() {
		a, err := scanPending(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate pending actions: %w", err)
	}
	return out, nil
}

func scanPending(row interface{ Scan(...any) error }) (*AIPendingAction, error) {
	var a AIPendingAction
	var args, preview, createdAt string
	var messageID sql.NullInt64
	var decidedAt sql.NullString
	if err := row.Scan(&a.ID, &a.SessionID, &messageID, &a.ToolCallID, &a.ToolName,
		&args, &a.Summary, &preview, &a.Status, &a.Result, &a.Error,
		&a.DecidedBy, &decidedAt, &createdAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		return nil, fmt.Errorf("scan pending action: %w", err)
	}
	if messageID.Valid {
		a.MessageID = &messageID.Int64
	}
	a.Arguments = json.RawMessage(args)
	a.Preview = json.RawMessage(preview)
	a.DecidedAt = parseTimePtr(decidedAt)
	a.CreatedAt = parseTime(createdAt)
	return &a, nil
}

// DecideAIPendingAction은 제안의 결과를 기록한다.
//
// pending 상태만 결정할 수 있게 조건을 SQL에 넣는 이유: 두 요청이 동시에 승인하면
// 툴이 두 번 실행된다. 상태 검사와 갱신을 한 문장으로 묶으면 그것을 막을 수 있다.
func (s *Store) DecideAIPendingAction(ctx context.Context, id, status, result, errMsg, deciderID string) error {
	if len(result) > 8000 {
		result = result[:8000] + "…"
	}
	if len(errMsg) > 2000 {
		errMsg = errMsg[:2000]
	}
	res, err := s.db.ExecContext(ctx, `UPDATE ai_pending_actions SET
		status = ?, result = ?, error = ?, decided_by = ?, decided_at = ?
		WHERE id = ? AND status = ?`,
		status, result, errMsg, nullString(deciderID), nowString(), id, PendingStatusPending)
	if err != nil {
		return fmt.Errorf("decide pending action: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// 이미 결정된 제안이다. 호출자는 이것을 409로 응답해야 한다.
		return ErrAlreadyDecided
	}
	return nil
}

// ErrAlreadyDecided는 이미 승인/거부된 제안을 다시 결정하려 했음을 뜻한다.
var ErrAlreadyDecided = errors.New("이미 처리된 제안입니다")

// ExpireAIPendingActions는 세션의 보류 제안을 만료시킨다.
//
// 새 사용자 메시지가 오면 이전 제안은 문맥을 잃는다. 그대로 두면 한참 뒤에
// 승인 버튼을 눌러 예상하지 못한 시점에 실행될 수 있다.
func (s *Store) ExpireAIPendingActions(ctx context.Context, sessionID string) (int64, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE ai_pending_actions SET
		status = ?, decided_at = ? WHERE session_id = ? AND status = ?`,
		PendingStatusExpired, nowString(), sessionID, PendingStatusPending)
	if err != nil {
		return 0, fmt.Errorf("expire pending actions: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}
