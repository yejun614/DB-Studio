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

// 매크로 자동 실행 트리거.
//
// 정기 실행과 조건 실행을 한 테이블에 둔 이유는 마이그레이션 파일에 적어 두었다.

const (
	TriggerSchedule = "schedule"
	TriggerEvent    = "event"
)

func ValidTriggerKind(k string) bool { return k == TriggerSchedule || k == TriggerEvent }

type MacroTrigger struct {
	ID      string         `json:"id"`
	MacroID string         `json:"macroId"`
	Name    string         `json:"name"`
	Kind    string         `json:"kind"`
	Enabled bool           `json:"enabled"`
	Params  map[string]any `json:"params"`
	// ParamExprs는 실행 시점에 계산할 파라미터다(키 → Lua 식).
	// 같은 키가 Params에도 있으면 이쪽이 이긴다.
	ParamExprs map[string]string `json:"paramExprs,omitempty"`

	Cron      string     `json:"cron,omitempty"`
	Timezone  string     `json:"timezone,omitempty"`
	NextRunAt *time.Time `json:"nextRunAt,omitempty"`

	EventKind      string `json:"eventKind,omitempty"`
	EventSeverity  string `json:"eventSeverity,omitempty"`
	EventMetric    string `json:"eventMetric,omitempty"`
	ConnectionID   string `json:"connectionId,omitempty"`
	MinIntervalSec int    `json:"minIntervalSec"`

	SkipIfRunning bool   `json:"skipIfRunning"`
	OwnerID       string `json:"ownerId,omitempty"`
	OwnerName     string `json:"ownerName"`

	LastFiredAt *time.Time `json:"lastFiredAt,omitempty"`
	LastRunID   string     `json:"lastRunId,omitempty"`
	LastStatus  string     `json:"lastStatus,omitempty"`
	LastError   string     `json:"lastError,omitempty"`
	FailCount   int        `json:"failCount"`

	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`

	// MacroName은 목록 화면이 쓰는 조인 결과다.
	MacroName string `json:"macroName,omitempty"`
}

type SaveTriggerParams struct {
	MacroID        string
	Name           string
	Kind           string
	Enabled        bool
	Params         map[string]any
	ParamExprs     map[string]string
	Cron           string
	Timezone       string
	NextRunAt      *time.Time
	EventKind      string
	EventSeverity  string
	EventMetric    string
	ConnectionID   string
	MinIntervalSec int
	SkipIfRunning  bool
	OwnerID        string
	OwnerName      string
}

func (s *Store) CreateTrigger(ctx context.Context, p SaveTriggerParams) (*MacroTrigger, error) {
	id := uuid.NewString()
	now := nowString()
	paramJSON := marshalParams(p.Params)

	if _, err := s.db.ExecContext(ctx, `INSERT INTO macro_triggers
		(id, macro_id, name, kind, enabled, params, param_exprs, cron, timezone, next_run_at,
		 event_kind, event_severity, event_metric, connection_id, min_interval_sec,
		 skip_if_running, owner_id, owner_name, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, p.MacroID, p.Name, p.Kind, boolInt(p.Enabled), paramJSON, marshalExprs(p.ParamExprs),
		p.Cron, p.Timezone, timePtrString(p.NextRunAt),
		p.EventKind, p.EventSeverity, p.EventMetric, nullString(p.ConnectionID),
		p.MinIntervalSec, boolInt(p.SkipIfRunning),
		nullString(p.OwnerID), p.OwnerName, now, now); err != nil {
		return nil, fmt.Errorf("insert trigger: %w", err)
	}
	return s.GetTrigger(ctx, id)
}

// UpdateTrigger는 트리거 설정을 통째로 교체한다.
// 소유자는 바꾸지 않는다 — 소유자가 바뀌면 실행 권한이 바뀌는 것이므로
// 그것은 "수정"이 아니라 새 트리거를 만드는 일이다.
func (s *Store) UpdateTrigger(ctx context.Context, id string, p SaveTriggerParams) (*MacroTrigger, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE macro_triggers SET
		name = ?, enabled = ?, params = ?, param_exprs = ?, cron = ?, timezone = ?, next_run_at = ?,
		event_kind = ?, event_severity = ?, event_metric = ?, connection_id = ?,
		min_interval_sec = ?, skip_if_running = ?, updated_at = ?,
		-- 설정을 고치면 실패 카운트를 초기화한다. 고친 이유가 대개 그 실패이기 때문이다.
		fail_count = 0, last_error = ''
		WHERE id = ?`,
		p.Name, boolInt(p.Enabled), marshalParams(p.Params), marshalExprs(p.ParamExprs),
		p.Cron, p.Timezone,
		timePtrString(p.NextRunAt), p.EventKind, p.EventSeverity, p.EventMetric,
		nullString(p.ConnectionID), p.MinIntervalSec, boolInt(p.SkipIfRunning),
		nowString(), id)
	if err != nil {
		return nil, fmt.Errorf("update trigger: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrNotFound
	}
	return s.GetTrigger(ctx, id)
}

func (s *Store) SetTriggerEnabled(ctx context.Context, id string, enabled bool) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE macro_triggers SET enabled = ?, updated_at = ? WHERE id = ?`,
		boolInt(enabled), nowString(), id)
	if err != nil {
		return fmt.Errorf("set trigger enabled: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) DeleteTrigger(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM macro_triggers WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete trigger: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

const triggerColumns = `t.id, t.macro_id, t.name, t.kind, t.enabled, t.params, t.param_exprs,
	t.cron, t.timezone, t.next_run_at, t.event_kind, t.event_severity, t.event_metric,
	t.connection_id, t.min_interval_sec, t.skip_if_running, t.owner_id, t.owner_name,
	t.last_fired_at, t.last_run_id, t.last_status, t.last_error, t.fail_count,
	t.created_at, t.updated_at, m.name`

func scanTrigger(row interface{ Scan(...any) error }) (*MacroTrigger, error) {
	var t MacroTrigger
	var enabled, skip int
	var params, paramExprs, createdAt, updatedAt string
	var nextRun, lastFired, connID, ownerID, lastRunID, macroName sql.NullString

	if err := row.Scan(&t.ID, &t.MacroID, &t.Name, &t.Kind, &enabled, &params, &paramExprs,
		&t.Cron, &t.Timezone, &nextRun, &t.EventKind, &t.EventSeverity, &t.EventMetric,
		&connID, &t.MinIntervalSec, &skip, &ownerID, &t.OwnerName,
		&lastFired, &lastRunID, &t.LastStatus, &t.LastError, &t.FailCount,
		&createdAt, &updatedAt, &macroName); err != nil {
		return nil, err
	}
	t.Enabled = enabled != 0
	t.SkipIfRunning = skip != 0
	t.ConnectionID = connID.String
	t.OwnerID = ownerID.String
	t.LastRunID = lastRunID.String
	t.MacroName = macroName.String
	t.NextRunAt = parseTimePtr(nextRun)
	t.LastFiredAt = parseTimePtr(lastFired)
	t.CreatedAt = parseTime(createdAt)
	t.UpdatedAt = parseTime(updatedAt)
	t.Params = map[string]any{}
	_ = json.Unmarshal([]byte(params), &t.Params)
	t.ParamExprs = map[string]string{}
	_ = json.Unmarshal([]byte(paramExprs), &t.ParamExprs)
	return &t, nil
}

const triggerFrom = ` FROM macro_triggers t LEFT JOIN macros m ON m.id = t.macro_id`

func (s *Store) GetTrigger(ctx context.Context, id string) (*MacroTrigger, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+triggerColumns+triggerFrom+` WHERE t.id = ?`, id)
	t, err := scanTrigger(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get trigger: %w", err)
	}
	return t, nil
}

// ListTriggers는 트리거를 반환한다. macroID가 비면 전체다.
//
// **거르지 않는다.** 스케줄러가 쓰는 목록이므로 그래야 한다 — 비공개 매크로의
// 자동 실행도 예정대로 돌아야 하고, 그 실행의 권한 판정은 소유자 기준으로 따로 한다
// (macro.Scheduler.fire). 화면에 내보낼 목록은 ListVisibleTriggers를 쓴다.
func (s *Store) ListTriggers(ctx context.Context, macroID string) ([]*MacroTrigger, error) {
	return s.queryTriggers(ctx, macroID, "", nil)
}

// ListVisibleTriggers는 뷰어가 볼 수 있는 매크로의 트리거만 반환한다.
func (s *Store) ListVisibleTriggers(ctx context.Context, macroID string, v MacroViewer) ([]*MacroTrigger, error) {
	where, args := macroVisibleWhere(v, "m")
	return s.queryTriggers(ctx, macroID, where, args)
}

func (s *Store) queryTriggers(ctx context.Context, macroID, extraWhere string, extraArgs []any) ([]*MacroTrigger, error) {
	q := `SELECT ` + triggerColumns + triggerFrom + ` WHERE 1 = 1`
	args := []any{}
	if macroID != "" {
		q += ` AND t.macro_id = ?`
		args = append(args, macroID)
	}
	q += extraWhere
	args = append(args, extraArgs...)
	q += ` ORDER BY t.created_at DESC`

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list triggers: %w", err)
	}
	defer rows.Close()

	out := []*MacroTrigger{}
	for rows.Next() {
		t, err := scanTrigger(rows)
		if err != nil {
			return nil, fmt.Errorf("scan trigger: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// DueTriggers는 지금 실행할 때가 된 스케줄 트리거를 반환한다.
func (s *Store) DueTriggers(ctx context.Context, now time.Time) ([]*MacroTrigger, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+triggerColumns+triggerFrom+`
		WHERE t.kind = 'schedule' AND t.enabled = 1
		  AND t.next_run_at IS NOT NULL AND t.next_run_at <= ?
		ORDER BY t.next_run_at`, formatTime(now))
	if err != nil {
		return nil, fmt.Errorf("list due triggers: %w", err)
	}
	defer rows.Close()

	out := []*MacroTrigger{}
	for rows.Next() {
		t, err := scanTrigger(rows)
		if err != nil {
			return nil, fmt.Errorf("scan due trigger: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// EventTriggers는 이벤트에 반응할 수 있는 트리거를 반환한다.
//
// 조건을 SQL로 다 거르지 않고 커넥션만 좁히는 이유: 심각도 비교는 문자열 순서와
// 다르고(info < warning < critical), 빈 값이 "전부"를 뜻하는 규칙까지 SQL에 넣으면
// 조건문이 읽을 수 없게 된다. 트리거 수는 많아야 수십 개이므로 Go에서 거른다.
func (s *Store) EventTriggers(ctx context.Context, connectionID string) ([]*MacroTrigger, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+triggerColumns+triggerFrom+`
		WHERE t.kind = 'event' AND t.enabled = 1
		  AND (t.connection_id IS NULL OR t.connection_id = ?)`, connectionID)
	if err != nil {
		return nil, fmt.Errorf("list event triggers: %w", err)
	}
	defer rows.Close()

	out := []*MacroTrigger{}
	for rows.Next() {
		t, err := scanTrigger(rows)
		if err != nil {
			return nil, fmt.Errorf("scan event trigger: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// SetTriggerNextRun은 다음 예정 시각만 갱신한다.
func (s *Store) SetTriggerNextRun(ctx context.Context, id string, next *time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE macro_triggers SET next_run_at = ? WHERE id = ?`, timePtrString(next), id)
	if err != nil {
		return fmt.Errorf("set next run: %w", err)
	}
	return nil
}

// RecordTriggerFire는 발화 결과를 기록한다.
//
// 성공하면 실패 카운트를 0으로 되돌린다. 누적만 하면 어쩌다 한 번 실패한 트리거가
// 몇 달 뒤에 상한에 닿아 꺼진다 — 연속 실패를 세는 것이 의도다.
func (s *Store) RecordTriggerFire(ctx context.Context, id, runID, status, errMsg string) error {
	failExpr := "0"
	if status != "started" && status != "success" {
		failExpr = "fail_count + 1"
	}
	_, err := s.db.ExecContext(ctx, `UPDATE macro_triggers
		SET last_fired_at = ?, last_run_id = ?, last_status = ?, last_error = ?,
		    fail_count = `+failExpr+`
		WHERE id = ?`,
		nowString(), nullString(runID), status, errMsg, id)
	if err != nil {
		return fmt.Errorf("record trigger fire: %w", err)
	}
	return nil
}

// DisableTriggerWithReason은 트리거를 끄고 이유를 남긴다.
// 소유자가 비활성화되었거나 연속 실패가 쌓인 경우에 쓴다.
func (s *Store) DisableTriggerWithReason(ctx context.Context, id, reason string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE macro_triggers SET enabled = 0, last_status = 'disabled', last_error = ?,
			next_run_at = NULL, updated_at = ? WHERE id = ?`,
		reason, nowString(), id)
	if err != nil {
		return fmt.Errorf("disable trigger: %w", err)
	}
	return nil
}

// marshalExprs는 파라미터 식 묶음을 저장 형식으로 만든다.
// 빈 식은 버린다 — 저장해 두면 실행 때마다 빈 식을 계산하려다 실패한다.
func marshalExprs(m map[string]string) string {
	clean := map[string]string{}
	for k, v := range m {
		if strings.TrimSpace(v) != "" {
			clean[k] = v
		}
	}
	if len(clean) == 0 {
		return "{}"
	}
	b, err := json.Marshal(clean)
	if err != nil {
		return "{}"
	}
	return string(b)
}

func marshalParams(p map[string]any) string {
	if len(p) == 0 {
		return "{}"
	}
	b, err := json.Marshal(p)
	if err != nil {
		return "{}"
	}
	return string(b)
}
