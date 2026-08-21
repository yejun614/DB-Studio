package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"dbstudio/internal/metric"
	"dbstudio/internal/model"
)

// Rule은 임계치 감시 규칙이다.
type Rule struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	ConnectionID string            `json:"connectionId,omitempty"` // 빈 값이면 전체 적용
	Environment  model.Environment `json:"environment,omitempty"`  // 빈 값이면 환경 무관
	Kind         string            `json:"kind"`
	Metric       string            `json:"metric"`
	Op           string            `json:"op"`
	Threshold    float64           `json:"threshold"`
	DurationSec  int               `json:"durationSec"`
	Severity     Severity          `json:"severity"`
	Enabled      bool              `json:"enabled"`
	Description  string            `json:"description,omitempty"`
	Builtin      bool              `json:"builtin"`
	CreatedAt    time.Time         `json:"createdAt"`
	UpdatedAt    time.Time         `json:"updatedAt"`
}

// AppliesTo는 이 룰이 특정 커넥션에 적용되는지 판단한다.
func (r *Rule) AppliesTo(conn *model.Connection) bool {
	if !r.Enabled || conn == nil {
		return false
	}
	if r.ConnectionID != "" && r.ConnectionID != conn.ID {
		return false
	}
	if r.Environment != "" && r.Environment != conn.Environment {
		return false
	}
	return true
}

// Breached는 값이 조건을 위반했는지 판단한다.
func (r *Rule) Breached(value float64) bool {
	switch r.Op {
	case ">":
		return value > r.Threshold
	case ">=":
		return value >= r.Threshold
	case "<":
		return value < r.Threshold
	case "<=":
		return value <= r.Threshold
	case "==":
		return value == r.Threshold
	case "!=":
		return value != r.Threshold
	}
	return false
}

// Describe는 룰 조건을 사람이 읽는 문장으로 만든다.
func (r *Rule) Describe() string {
	meta := metric.Lookup(r.Metric)
	base := fmt.Sprintf("%s %s %g", meta.Label, r.Op, r.Threshold)
	if meta.Unit == metric.UnitPercent {
		base += "%"
	}
	if r.DurationSec > 0 {
		base += fmt.Sprintf(" (%d초 지속)", r.DurationSec)
	}
	return base
}

var validOps = map[string]bool{">": true, ">=": true, "<": true, "<=": true, "==": true, "!=": true}

// ValidateRule은 룰 입력을 검증한다.
func ValidateRule(r *Rule) error {
	if strings.TrimSpace(r.Name) == "" {
		return errors.New("룰 이름을 입력하세요")
	}
	switch r.Kind {
	case "", EventThreshold:
		r.Kind = EventThreshold
		if strings.TrimSpace(r.Metric) == "" {
			return errors.New("감시할 지표를 선택하세요")
		}
		if !validOps[r.Op] {
			return errors.New("비교 연산자가 올바르지 않습니다")
		}
	case EventConnectivity, EventDrift:
		// 이 종류는 지표/연산자가 필요 없다. 감지 로직이 고정되어 있다.
		r.Metric = ""
		r.Op = ""
	default:
		return fmt.Errorf("알 수 없는 룰 종류입니다: %s", r.Kind)
	}
	if !r.Severity.Valid() {
		return errors.New("심각도가 올바르지 않습니다")
	}
	if r.Environment != "" && !r.Environment.Valid() {
		return errors.New("환경이 올바르지 않습니다")
	}
	if r.DurationSec < 0 || r.DurationSec > 86400 {
		return errors.New("지속 시간은 0~86400초 사이여야 합니다")
	}
	return nil
}

const ruleColumns = `id, name, connection_id, environment, kind, metric, op, threshold,
	duration_sec, severity, enabled, description, builtin, created_at, updated_at`

func scanRule(row interface{ Scan(...any) error }) (*Rule, error) {
	var r Rule
	var connID, env sql.NullString
	var enabled, builtin int
	var created, updated string
	if err := row.Scan(&r.ID, &r.Name, &connID, &env, &r.Kind, &r.Metric, &r.Op,
		&r.Threshold, &r.DurationSec, &r.Severity, &enabled, &r.Description,
		&builtin, &created, &updated); err != nil {
		return nil, err
	}
	r.ConnectionID = connID.String
	r.Environment = model.Environment(env.String)
	r.Enabled = enabled != 0
	r.Builtin = builtin != 0
	r.CreatedAt = parseTime(created)
	r.UpdatedAt = parseTime(updated)
	return &r, nil
}

func (s *Store) ListRules(ctx context.Context) ([]*Rule, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+ruleColumns+` FROM monitor_rules ORDER BY
			CASE severity WHEN 'critical' THEN 0 WHEN 'warning' THEN 1 ELSE 2 END, name`)
	if err != nil {
		return nil, fmt.Errorf("list rules: %w", err)
	}
	defer rows.Close()

	out := []*Rule{}
	for rows.Next() {
		r, err := scanRule(rows)
		if err != nil {
			return nil, fmt.Errorf("scan rule: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate rules: %w", err)
	}
	return out, nil
}

func (s *Store) GetRule(ctx context.Context, id string) (*Rule, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+ruleColumns+` FROM monitor_rules WHERE id = ?`, id)
	r, err := scanRule(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get rule: %w", err)
	}
	return r, nil
}

func (s *Store) CreateRule(ctx context.Context, r *Rule) (*Rule, error) {
	if r.ID == "" {
		r.ID = uuid.NewString()
	}
	now := nowString()
	if _, err := s.db.ExecContext(ctx, `INSERT INTO monitor_rules
		(id, name, connection_id, environment, kind, metric, op, threshold, duration_sec,
		 severity, enabled, description, builtin, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.Name, nullString(r.ConnectionID), nullString(string(r.Environment)),
		r.Kind, r.Metric, r.Op, r.Threshold, r.DurationSec, string(r.Severity),
		boolToInt(r.Enabled), r.Description, boolToInt(r.Builtin), now, now); err != nil {
		return nil, fmt.Errorf("insert rule: %w", err)
	}
	return s.GetRule(ctx, r.ID)
}

func (s *Store) UpdateRule(ctx context.Context, r *Rule) (*Rule, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE monitor_rules SET
		name = ?, connection_id = ?, environment = ?, kind = ?, metric = ?, op = ?,
		threshold = ?, duration_sec = ?, severity = ?, enabled = ?, description = ?, updated_at = ?
		WHERE id = ?`,
		r.Name, nullString(r.ConnectionID), nullString(string(r.Environment)),
		r.Kind, r.Metric, r.Op, r.Threshold, r.DurationSec, string(r.Severity),
		boolToInt(r.Enabled), r.Description, nowString(), r.ID)
	if err != nil {
		return nil, fmt.Errorf("update rule: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrNotFound
	}
	return s.GetRule(ctx, r.ID)
}

func (s *Store) DeleteRule(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()

	// 이벤트 해소를 반드시 룰 삭제보다 먼저 해야 한다.
	// events.rule_id는 ON DELETE SET NULL이므로 룰을 먼저 지우면 rule_id가 NULL이 되어
	// 이 UPDATE가 대상을 찾지 못하고, 근거 없는 이벤트가 영구히 열린 채 남는다.
	now := nowString()
	if _, err := tx.ExecContext(ctx,
		`UPDATE events SET state = 'resolved', resolved_at = ?, updated_at = ?
		 WHERE rule_id = ? AND state = 'open'`, now, now, id); err != nil {
		return fmt.Errorf("resolve events of deleted rule: %w", err)
	}

	res, err := tx.ExecContext(ctx, `DELETE FROM monitor_rules WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete rule: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// SeedBuiltinRules는 룰이 하나도 없을 때 기본 룰을 만든다.
//
// 기본값을 제공하는 이유: 빈 룰 목록으로 시작하면 모니터링이 아무 이벤트도 내지 않아
// "동작하지 않는 것처럼" 보인다. 운영 환경에 더 엄격한 임계치를 별도로 둔다.
func (s *Store) SeedBuiltinRules(ctx context.Context) (int, error) {
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM monitor_rules`).Scan(&count); err != nil {
		return 0, fmt.Errorf("count rules: %w", err)
	}
	if count > 0 {
		return 0, nil
	}

	defaults := []*Rule{
		{
			// connectivity/drift 룰은 비교 연산자를 쓰지 않으므로 Op를 비워둔다.
			// 감지 로직이 고정되어 있어 지표와 임계치가 의미를 갖지 않는다.
			Name: "접속 실패", Kind: EventConnectivity, Op: "", Severity: SeverityCritical,
			DurationSec: 60, Enabled: true, Builtin: true,
			Description: "지표 수집이 60초 이상 연속 실패하면 알립니다",
		},
		{
			Name: "스키마 외부 변경", Kind: EventDrift, Op: "", Severity: SeverityWarning,
			Enabled: true, Builtin: true,
			Description: "이 앱을 거치지 않고 스키마가 변경되면 알립니다",
		},
		{
			Name: "세션 사용률 높음", Kind: EventThreshold, Metric: metric.NameConnUsedPct,
			Op: ">", Threshold: 80, DurationSec: 120, Severity: SeverityWarning,
			Enabled: true, Builtin: true,
			Description: "커넥션 상한의 80%를 2분 이상 초과하면 신규 접속 거부가 임박합니다",
		},
		{
			Name: "세션 사용률 위험 (운영)", Kind: EventThreshold, Metric: metric.NameConnUsedPct,
			Op: ">", Threshold: 95, DurationSec: 30, Severity: SeverityCritical,
			Environment: model.EnvProd, Enabled: true, Builtin: true,
			Description: "운영 DB의 커넥션 상한이 거의 소진되었습니다",
		},
		{
			Name: "캐시 적중률 저하", Kind: EventThreshold, Metric: metric.NameCacheHitRatio,
			Op: "<", Threshold: 90, DurationSec: 600, Severity: SeverityWarning,
			Enabled: true, Builtin: true,
			Description: "적중률이 10분 이상 90% 아래면 디스크 I/O가 병목일 수 있습니다",
		},
		{
			Name: "슬로우 쿼리 급증", Kind: EventThreshold, Metric: metric.NameSlowQueryRate,
			Op: ">", Threshold: 1, DurationSec: 120, Severity: SeverityWarning,
			Enabled: true, Builtin: true,
			Description: "초당 1건 이상의 슬로우 쿼리가 2분 이상 지속됩니다",
		},
		{
			Name: "장시간 실행 쿼리", Kind: EventThreshold, Metric: metric.NameLongestQuery,
			Op: ">", Threshold: 300, DurationSec: 0, Severity: SeverityWarning,
			Enabled: true, Builtin: true,
			Description: "5분 이상 실행 중인 쿼리가 있습니다",
		},
		{
			Name: "복제 지연", Kind: EventThreshold, Metric: metric.NameReplicaLag,
			Op: ">", Threshold: 30, DurationSec: 60, Severity: SeverityCritical,
			Enabled: true, Builtin: true,
			Description: "복제 지연이 30초를 1분 이상 초과합니다",
		},
		{
			Name: "데드락 발생", Kind: EventThreshold, Metric: metric.NameDeadlocks,
			Op: ">", Threshold: 0, DurationSec: 0, Severity: SeverityWarning,
			Enabled: true, Builtin: true,
			Description: "데드락이 발생했습니다",
		},
		{
			Name: "응답 시간 저하", Kind: EventThreshold, Metric: metric.NameLatency,
			Op: ">", Threshold: 1000, DurationSec: 120, Severity: SeverityWarning,
			Enabled: true, Builtin: true,
			Description: "지표 수집 응답이 1초를 2분 이상 초과합니다",
		},
		{
			Name: "메모리 사용률 높음 (Redis)", Kind: EventThreshold, Metric: "memory.used_pct",
			Op: ">", Threshold: 85, DurationSec: 120, Severity: SeverityWarning,
			Enabled: true, Builtin: true,
			Description: "maxmemory의 85%를 초과했습니다. 키 축출이 시작될 수 있습니다",
		},
		{
			Name: "키 축출 발생 (Redis)", Kind: EventThreshold, Metric: metric.NameEvictedRate,
			Op: ">", Threshold: 0, DurationSec: 60, Severity: SeverityCritical,
			Enabled: true, Builtin: true,
			Description: "메모리 부족으로 키가 삭제되고 있습니다",
		},
		{
			// 스토리지(하둡·Ceph)용 기본 룰.
			//
			// 종류별로 나누지 않은 이유: 지표 이름을 공유하므로(storage.*) 룰 하나가
			// 두 종류에 다 걸린다. 다른 종류의 커넥션에는 그 지표가 없어 아무 일도
			// 일어나지 않는다.
			Name: "스토리지 용량 부족", Kind: EventThreshold, Metric: metric.NameStorageUsedPct,
			Op: ">", Threshold: 85, DurationSec: 300, Severity: SeverityWarning,
			Enabled: true, Builtin: true,
			Description: "클러스터 용량의 85%를 5분 이상 쓰면 알립니다. HDFS는 이 지점을 넘으면 블록 배치가 한쪽으로 몰리기 시작합니다",
		},
		{
			Name: "스토리지 용량 위험", Kind: EventThreshold, Metric: metric.NameStorageUsedPct,
			Op: ">", Threshold: 95, DurationSec: 60, Severity: SeverityCritical,
			Enabled: true, Builtin: true,
			Description: "용량의 95%를 넘었습니다. 쓰기가 곧 실패합니다",
		},
		{
			Name: "스토리지 노드 이상", Kind: EventThreshold, Metric: metric.NameStorageNodesDown,
			Op: ">", Threshold: 0, DurationSec: 120, Severity: SeverityWarning,
			Enabled: true, Builtin: true,
			Description: "데이터노드(HDFS) 또는 OSD(Ceph)가 2분 이상 빠져 있으면 알립니다. 복제본이 줄어든 상태입니다",
		},
		{
			Name: "스토리지 클러스터 상태 위험", Kind: EventThreshold, Metric: metric.NameStorageHealth,
			Op: ">=", Threshold: 2, DurationSec: 60, Severity: SeverityCritical,
			Enabled: true, Builtin: true,
			Description: "Ceph HEALTH_ERR 또는 HDFS 손실·손상 블록입니다. 데이터가 이미 유실됐을 수 있습니다",
		},
		{
			Name: "HDFS 손실 블록", Kind: EventThreshold, Metric: metric.NameHDFSMissingBlocks,
			Op: ">", Threshold: 0, DurationSec: 60, Severity: SeverityCritical,
			Enabled: true, Builtin: true,
			Description: "복제본이 하나도 남지 않은 블록이 있습니다",
		},
	}

	for _, r := range defaults {
		if _, err := s.CreateRule(ctx, r); err != nil {
			return 0, fmt.Errorf("seed rule %q: %w", r.Name, err)
		}
	}
	return len(defaults), nil
}
