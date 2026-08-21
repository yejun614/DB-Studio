package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Severity는 이벤트 심각도다.
type Severity string

const (
	SeverityInfo     Severity = "info"
	SeverityWarning  Severity = "warning"
	SeverityCritical Severity = "critical"
)

func (s Severity) Valid() bool {
	switch s {
	case SeverityInfo, SeverityWarning, SeverityCritical:
		return true
	}
	return false
}

// Rank는 심각도 비교용 정수다.
func (s Severity) Rank() int {
	switch s {
	case SeverityCritical:
		return 3
	case SeverityWarning:
		return 2
	case SeverityInfo:
		return 1
	}
	return 0
}

// 이벤트 종류
const (
	EventThreshold    = "threshold"     // 임계치 위반
	EventConnectivity = "connectivity"  // 접속 실패
	EventDrift        = "drift"         // 스키마가 앱 외부에서 변경됨
	EventCollectError = "collect_error" // 지표 수집 부분 실패
	EventHost         = "host"          // DB Studio가 도는 컴퓨터(호스트)의 상태
	EventCluster      = "cluster"       // 클러스터 노드의 상태(소식 끊김 등)
)

// Event는 이벤트 한 건이다.
type Event struct {
	ID           int64          `json:"id"`
	ConnectionID string         `json:"connectionId,omitempty"`
	RuleID       string         `json:"ruleId,omitempty"`
	Kind         string         `json:"kind"`
	Severity     Severity       `json:"severity"`
	State        string         `json:"state"`
	Metric       string         `json:"metric,omitempty"`
	Message      string         `json:"message"`
	Value        *float64       `json:"value"`
	Threshold    *float64       `json:"threshold"`
	Detail       map[string]any `json:"detail"`
	StartedAt    time.Time      `json:"startedAt"`
	UpdatedAt    time.Time      `json:"updatedAt"`
	ResolvedAt   *time.Time     `json:"resolvedAt"`
	AckedAt      *time.Time     `json:"ackedAt"`
	AckedBy      string         `json:"ackedBy,omitempty"`
	Occurrences  int            `json:"occurrences"`
}

// OpenEventParams는 이벤트 개시 입력이다.
type OpenEventParams struct {
	ConnectionID string
	RuleID       string
	Kind         string
	Severity     Severity
	Metric       string
	Message      string
	Value        *float64
	Threshold    *float64
	Detail       map[string]any
}

// OpenEvent는 이벤트를 개시하거나, 같은 원인의 열린 이벤트가 이미 있으면 갱신한다.
//
// 중복을 새 행으로 쌓지 않는 이유: 임계치를 30초마다 위반하면 하루에 2880건이 생겨
// 타임라인이 무의미해진다. 대신 occurrences를 올리고 최신 값을 반영한다.
// 반환값은 이벤트 ID와 "새로 생성되었는지" 여부다.
func (s *Store) OpenEvent(ctx context.Context, p OpenEventParams) (int64, bool, error) {
	now := nowString()
	detailJSON := "{}"
	if len(p.Detail) > 0 {
		b, err := json.Marshal(p.Detail)
		if err != nil {
			return 0, false, fmt.Errorf("marshal event detail: %w", err)
		}
		detailJSON = string(b)
	}

	var connID, ruleID any
	if p.ConnectionID != "" {
		connID = p.ConnectionID
	}
	if p.RuleID != "" {
		ruleID = p.RuleID
	}

	// 같은 (커넥션, 종류, 지표, 룰)로 열려 있는 이벤트를 찾는다.
	var existingID int64
	err := s.db.QueryRowContext(ctx, `
		SELECT id FROM events
		WHERE state = 'open' AND kind = ? AND metric = ?
		  AND COALESCE(connection_id, '') = COALESCE(?, '')
		  AND COALESCE(rule_id, '') = COALESCE(?, '')
		ORDER BY id DESC LIMIT 1`,
		p.Kind, p.Metric, connID, ruleID).Scan(&existingID)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		res, err := s.db.ExecContext(ctx, `INSERT INTO events
			(connection_id, rule_id, kind, severity, state, metric, message, value, threshold,
			 detail, started_at, updated_at, occurrences)
			VALUES (?, ?, ?, ?, 'open', ?, ?, ?, ?, ?, ?, ?, 1)`,
			connID, ruleID, p.Kind, string(p.Severity), p.Metric, p.Message,
			nullFloat(p.Value), nullFloat(p.Threshold), detailJSON, now, now)
		if err != nil {
			return 0, false, fmt.Errorf("insert event: %w", err)
		}
		id, _ := res.LastInsertId()
		return id, true, nil

	case err != nil:
		return 0, false, fmt.Errorf("lookup open event: %w", err)

	default:
		// 심각도는 올라가기만 한다. 잠깐 완화됐다고 critical을 warning으로 낮추면
		// 타임라인에서 최악의 상태를 놓친다.
		// 임계값도 함께 갱신한다. 운영 중에 기준을 바꾸면(호스트 임계값 화면 등)
		// 열려 있던 이벤트만 옛 숫자를 들고 남아, 본문("기준 85%")과 표의 임계값이
		// 서로 다른 말을 하게 된다.
		if _, err := s.db.ExecContext(ctx, `UPDATE events SET
			occurrences = occurrences + 1, updated_at = ?, value = ?, threshold = ?, message = ?, detail = ?,
			severity = CASE
				WHEN ? = 'critical' THEN 'critical'
				WHEN ? = 'warning' AND severity = 'info' THEN 'warning'
				ELSE severity END
			WHERE id = ?`,
			now, nullFloat(p.Value), nullFloat(p.Threshold), p.Message, detailJSON,
			string(p.Severity), string(p.Severity), existingID); err != nil {
			return 0, false, fmt.Errorf("update event: %w", err)
		}
		return existingID, false, nil
	}
}

// ResolveEvents는 조건에 맞는 열린 이벤트를 해소 처리하고, 실제로 닫힌 것들을 돌려준다.
// 룰이 정상으로 돌아오거나 접속이 복구되면 호출한다.
//
// 개수가 아니라 이벤트를 돌려주는 이유: 알림은 "무엇이 끝났는가"를 말해야 한다.
// 개수만 알면 "3건이 해소되었습니다"밖에 보낼 수 없고, 그 문장은 채널에서 아무
// 쓸모가 없다. 닫기 전에 대상을 먼저 읽어 두는 방식이라 UPDATE와 SELECT가 두 번
// 도는데, 해소는 드문 일이므로 그 비용보다 알림의 내용이 중요하다.
func (s *Store) ResolveEvents(ctx context.Context, connectionID, kind, metricName, ruleID string) ([]*Event, error) {
	var connID, rid any
	if connectionID != "" {
		connID = connectionID
	}
	if ruleID != "" {
		rid = ruleID
	}
	const where = `state = 'open' AND kind = ? AND metric = ?
		  AND COALESCE(connection_id, '') = COALESCE(?, '')
		  AND COALESCE(rule_id, '') = COALESCE(?, '')`

	rows, err := s.db.QueryContext(ctx, `SELECT
		id, connection_id, rule_id, kind, severity, state, metric, message, value, threshold,
		detail, started_at, updated_at, resolved_at, acked_at, acked_by, occurrences
		FROM events WHERE `+where, kind, metricName, connID, rid)
	if err != nil {
		return nil, fmt.Errorf("list resolving events: %w", err)
	}
	defer rows.Close()
	out := []*Event{}
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate resolving events: %w", err)
	}
	if len(out) == 0 {
		return out, nil
	}

	now := nowString()
	if _, err := s.db.ExecContext(ctx, `UPDATE events
		SET state = 'resolved', resolved_at = ?, updated_at = ?
		WHERE `+where, now, now, kind, metricName, connID, rid); err != nil {
		return nil, fmt.Errorf("resolve events: %w", err)
	}
	resolvedAt := parseTime(now)
	for _, e := range out {
		e.State, e.ResolvedAt = "resolved", &resolvedAt
	}
	return out, nil
}

// ResolveDriftEvents는 한 커넥션의 열린 스키마 변경(드리프트) 이벤트를 모두 닫는다.
//
// ResolveEvents와 달리 룰을 따지지 않는다. 외부 편집을 버전으로 등록하면 "앱 밖에서
// 바뀐 상태를 모르고 있다"는 문제 자체가 끝나므로, 어느 룰이 열었든 함께 닫혀야 한다.
func (s *Store) ResolveDriftEvents(ctx context.Context, connectionID string) (int64, error) {
	now := nowString()
	res, err := s.db.ExecContext(ctx, `UPDATE events
		SET state = 'resolved', resolved_at = ?, updated_at = ?
		WHERE state = 'open' AND kind = ? AND connection_id = ?`,
		now, now, EventDrift, connectionID)
	if err != nil {
		return 0, fmt.Errorf("resolve drift events: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// AckEvent는 이벤트를 확인 처리한다.
func (s *Store) AckEvent(ctx context.Context, id int64, userID string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE events SET acked_at = ?, acked_by = ?, updated_at = ? WHERE id = ?`,
		nowString(), userID, nowString(), id)
	if err != nil {
		return fmt.Errorf("ack event: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ResolveEventByID는 사용자가 이벤트를 수동으로 닫을 때 쓴다.
func (s *Store) ResolveEventByID(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE events SET state = 'resolved', resolved_at = ?, updated_at = ?
		 WHERE id = ? AND state = 'open'`, nowString(), nowString(), id)
	if err != nil {
		return fmt.Errorf("resolve event: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// GetEvent는 이벤트 하나를 조회한다.
func (s *Store) GetEvent(ctx context.Context, id int64) (*Event, error) {
	row := s.db.QueryRowContext(ctx, `SELECT
		id, connection_id, rule_id, kind, severity, state, metric, message, value, threshold,
		detail, started_at, updated_at, resolved_at, acked_at, acked_by, occurrences
		FROM events WHERE id = ?`, id)
	e, err := scanEvent(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return e, nil
}

// EventFilter는 이벤트 조회 조건이다.
type EventFilter struct {
	ConnectionIDs []string
	// IncludeSystem은 커넥션에 속하지 않은 이벤트(호스트 상태 등)까지 볼지다.
	//
	// ConnectionIDs와 따로 두는 이유: 시스템 이벤트에는 붙일 커넥션 ID가 없어
	// 목록에 넣을 방법이 없고, 그렇다고 목록이 비면 전체 공개가 되어서도 안 된다.
	// 권한 판단(커넥션 관리자 이상)은 호출자가 한다.
	IncludeSystem bool
	Kind          string
	Severity      Severity
	State         string
	Since         *time.Time
	Until         *time.Time
	OnlyUnacked   bool
	Limit         int
	Offset        int
}

func (s *Store) ListEvents(ctx context.Context, f EventFilter) ([]*Event, int, error) {
	where := []string{"1 = 1"}
	args := []any{}

	// 커넥션 목록이 비어 있으면 "접근 가능한 커넥션이 없음"을 뜻하므로
	// 전체 조회로 새지 않도록 빈 결과를 반환한다.
	if f.ConnectionIDs != nil {
		if len(f.ConnectionIDs) == 0 && !f.IncludeSystem {
			return []*Event{}, 0, nil
		}
		where = append(where, connScopeClause(f.ConnectionIDs, f.IncludeSystem, &args))
	}
	if f.Kind != "" {
		where = append(where, "kind = ?")
		args = append(args, f.Kind)
	}
	if f.Severity != "" {
		where = append(where, "severity = ?")
		args = append(args, string(f.Severity))
	}
	if f.State != "" {
		where = append(where, "state = ?")
		args = append(args, f.State)
	}
	if f.OnlyUnacked {
		where = append(where, "acked_at IS NULL")
	}
	if f.Since != nil {
		where = append(where, "started_at >= ?")
		args = append(args, formatTime(*f.Since))
	}
	if f.Until != nil {
		where = append(where, "started_at <= ?")
		args = append(args, formatTime(*f.Until))
	}
	clause := strings.Join(where, " AND ")

	var total int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM events WHERE `+clause, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count events: %w", err)
	}

	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `SELECT
		id, connection_id, rule_id, kind, severity, state, metric, message, value, threshold,
		detail, started_at, updated_at, resolved_at, acked_at, acked_by, occurrences
		FROM events WHERE `+clause+`
		ORDER BY CASE state WHEN 'open' THEN 0 ELSE 1 END,
		         CASE severity WHEN 'critical' THEN 0 WHEN 'warning' THEN 1 ELSE 2 END,
		         started_at DESC
		LIMIT ? OFFSET ?`, append(args, limit, f.Offset)...)
	if err != nil {
		return nil, 0, fmt.Errorf("list events: %w", err)
	}
	defer rows.Close()

	out := []*Event{}
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate events: %w", err)
	}
	return out, total, nil
}

func scanEvent(row interface{ Scan(...any) error }) (*Event, error) {
	var e Event
	var connID, ruleID, resolvedAt, ackedAt, ackedBy sql.NullString
	var value, threshold sql.NullFloat64
	var detail, started, updated string

	if err := row.Scan(&e.ID, &connID, &ruleID, &e.Kind, &e.Severity, &e.State,
		&e.Metric, &e.Message, &value, &threshold, &detail,
		&started, &updated, &resolvedAt, &ackedAt, &ackedBy, &e.Occurrences); err != nil {
		return nil, fmt.Errorf("scan event: %w", err)
	}
	e.ConnectionID = connID.String
	e.RuleID = ruleID.String
	if value.Valid {
		e.Value = &value.Float64
	}
	if threshold.Valid {
		e.Threshold = &threshold.Float64
	}
	e.Detail = map[string]any{}
	_ = json.Unmarshal([]byte(detail), &e.Detail)
	e.StartedAt = parseTime(started)
	e.UpdatedAt = parseTime(updated)
	e.ResolvedAt = parseTimePtr(resolvedAt)
	e.AckedAt = parseTimePtr(ackedAt)
	e.AckedBy = ackedBy.String
	return &e, nil
}

// EventSummary는 이벤트 현황 요약이다. 대시보드 배지에 쓴다.
type EventSummary struct {
	OpenCritical int `json:"openCritical"`
	OpenWarning  int `json:"openWarning"`
	OpenInfo     int `json:"openInfo"`
	Unacked      int `json:"unacked"`
	Last24h      int `json:"last24h"`
}

// connScopeClause는 "이 사용자가 볼 수 있는 이벤트" 조건을 만든다.
//
// 목록과 요약이 같은 함수를 쓰는 이유: 두 곳의 조건이 어긋나면 배지에는 숫자가
// 있는데 목록은 비어 있는(또는 그 반대의) 화면이 나오고, 그때 사람들은 알림 자체를
// 믿지 않게 된다.
func connScopeClause(ids []string, includeSystem bool, args *[]any) string {
	parts := []string{}
	if len(ids) > 0 {
		placeholders := make([]string, len(ids))
		for i, id := range ids {
			placeholders[i] = "?"
			*args = append(*args, id)
		}
		parts = append(parts, "connection_id IN ("+strings.Join(placeholders, ",")+")")
	}
	if includeSystem {
		parts = append(parts, "connection_id IS NULL")
	}
	if len(parts) == 0 {
		return "1 = 0"
	}
	return "(" + strings.Join(parts, " OR ") + ")"
}

func (s *Store) EventSummary(ctx context.Context, connectionIDs []string, includeSystem bool) (*EventSummary, error) {
	sum := &EventSummary{}
	args := []any{}
	scope := ""
	if connectionIDs != nil {
		if len(connectionIDs) == 0 && !includeSystem {
			return sum, nil
		}
		scope = " AND " + connScopeClause(connectionIDs, includeSystem, &args)
	}

	row := s.db.QueryRowContext(ctx, `SELECT
		COUNT(*) FILTER (WHERE state = 'open' AND severity = 'critical'),
		COUNT(*) FILTER (WHERE state = 'open' AND severity = 'warning'),
		COUNT(*) FILTER (WHERE state = 'open' AND severity = 'info'),
		COUNT(*) FILTER (WHERE state = 'open' AND acked_at IS NULL),
		COUNT(*) FILTER (WHERE started_at >= ?)
		FROM events WHERE 1 = 1`+scope,
		append([]any{formatTime(time.Now().Add(-24 * time.Hour))}, args...)...)

	if err := row.Scan(&sum.OpenCritical, &sum.OpenWarning, &sum.OpenInfo,
		&sum.Unacked, &sum.Last24h); err != nil {
		return nil, fmt.Errorf("event summary: %w", err)
	}
	return sum, nil
}

// PurgeEvents는 해소된 오래된 이벤트를 정리한다. 열린 이벤트는 남긴다.
func (s *Store) PurgeEvents(ctx context.Context, retain time.Duration) (int64, error) {
	cutoff := formatTime(time.Now().Add(-retain))
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM events WHERE state = 'resolved' AND resolved_at < ?`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("purge events: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

func nullFloat(v *float64) any {
	if v == nil {
		return nil
	}
	return *v
}
