package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"dbstudio/internal/metric"
)

// SaveSamples는 한 번의 수집 결과를 원본 시계열에 기록하고 최신 상태를 갱신한다.
// 한 트랜잭션으로 묶어 목록 화면이 "기록됐지만 상태는 옛것"을 보지 않게 한다.
func (s *Store) SaveSamples(ctx context.Context, connectionID string, set *metric.Set, lastError string) error {
	ts := formatTime(set.CollectedAt)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO metric_samples (connection_id, metric, ts, value, unit) VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare insert sample: %w", err)
	}
	defer stmt.Close()

	latest := map[string]any{}
	for _, sm := range set.Samples {
		if _, err := stmt.ExecContext(ctx, connectionID, sm.Name, ts, sm.Value, string(sm.Unit)); err != nil {
			return fmt.Errorf("insert sample %s: %w", sm.Name, err)
		}
		latest[sm.Name] = map[string]any{"value": sm.Value, "unit": sm.Unit}
	}

	up := 0
	if sm, ok := set.Get(metric.NameUp); ok && sm.Value > 0 {
		up = 1
	}
	notesJSON, err := json.Marshal(set.Notes)
	if err != nil {
		return fmt.Errorf("marshal notes: %w", err)
	}
	metricsJSON, err := json.Marshal(latest)
	if err != nil {
		return fmt.Errorf("marshal metrics: %w", err)
	}

	// 연속 실패 횟수는 상태 행에서 누적한다. 성공하면 0으로 되돌린다.
	// 이 값으로 "일시적 끊김"과 "지속적 장애"를 구분한다.
	var lastOK any
	if up == 1 {
		lastOK = ts
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO connection_state
		(connection_id, up, last_polled_at, last_ok_at, last_error, latency_ms, notes, metrics_json, consecutive_fails)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (connection_id) DO UPDATE SET
			up = excluded.up,
			last_polled_at = excluded.last_polled_at,
			last_ok_at = COALESCE(excluded.last_ok_at, connection_state.last_ok_at),
			last_error = excluded.last_error,
			latency_ms = excluded.latency_ms,
			notes = excluded.notes,
			metrics_json = excluded.metrics_json,
			consecutive_fails = CASE WHEN excluded.up = 1 THEN 0
			                         ELSE connection_state.consecutive_fails + 1 END`,
		connectionID, up, ts, lastOK, truncate(lastError, 500),
		set.LatencyMs, string(notesJSON), string(metricsJSON), boolToInt(up == 0)); err != nil {
		return fmt.Errorf("upsert connection state: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ConnectionState는 커넥션의 최신 모니터링 상태다.
type ConnectionState struct {
	ConnectionID     string                    `json:"connectionId"`
	Up               bool                      `json:"up"`
	LastPolledAt     *time.Time                `json:"lastPolledAt"`
	LastOKAt         *time.Time                `json:"lastOkAt"`
	LastError        string                    `json:"lastError,omitempty"`
	LatencyMs        float64                   `json:"latencyMs"`
	Notes            []string                  `json:"notes"`
	Metrics          map[string]MetricSnapshot `json:"metrics"`
	ConsecutiveFails int                       `json:"consecutiveFails"`
}

type MetricSnapshot struct {
	Value float64     `json:"value"`
	Unit  metric.Unit `json:"unit"`
}

// ListConnectionStates는 모든 커넥션의 최신 상태를 반환한다.
func (s *Store) ListConnectionStates(ctx context.Context) (map[string]*ConnectionState, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT connection_id, up, last_polled_at, last_ok_at,
		last_error, latency_ms, notes, metrics_json, consecutive_fails FROM connection_state`)
	if err != nil {
		return nil, fmt.Errorf("list connection states: %w", err)
	}
	defer rows.Close()

	out := map[string]*ConnectionState{}
	for rows.Next() {
		st, err := scanConnectionState(rows)
		if err != nil {
			return nil, err
		}
		out[st.ConnectionID] = st
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate connection states: %w", err)
	}
	return out, nil
}

// GetConnectionState는 한 커넥션의 최신 상태를 반환한다. 없으면 nil, nil이다.
func (s *Store) GetConnectionState(ctx context.Context, connectionID string) (*ConnectionState, error) {
	row := s.db.QueryRowContext(ctx, `SELECT connection_id, up, last_polled_at, last_ok_at,
		last_error, latency_ms, notes, metrics_json, consecutive_fails
		FROM connection_state WHERE connection_id = ?`, connectionID)
	st, err := scanConnectionState(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return st, nil
}

func scanConnectionState(row interface{ Scan(...any) error }) (*ConnectionState, error) {
	var st ConnectionState
	var up, fails int
	var polled, okAt sql.NullString
	var notesJSON, metricsJSON string
	err := row.Scan(&st.ConnectionID, &up, &polled, &okAt, &st.LastError,
		&st.LatencyMs, &notesJSON, &metricsJSON, &fails)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, err
		}
		return nil, fmt.Errorf("scan connection state: %w", err)
	}
	st.Up = up != 0
	st.ConsecutiveFails = fails
	st.LastPolledAt = parseTimePtr(polled)
	st.LastOKAt = parseTimePtr(okAt)
	st.Notes = []string{}
	_ = json.Unmarshal([]byte(notesJSON), &st.Notes)
	st.Metrics = map[string]MetricSnapshot{}
	_ = json.Unmarshal([]byte(metricsJSON), &st.Metrics)
	return &st, nil
}

// ---------- 시계열 조회 ----------

// SeriesPoint는 차트의 한 점이다.
type SeriesPoint struct {
	Ts  time.Time `json:"ts"`
	Avg float64   `json:"avg"`
	Min float64   `json:"min"`
	Max float64   `json:"max"`
}

// Series는 한 지표의 시계열이다.
type Series struct {
	Metric string        `json:"metric"`
	Unit   metric.Unit   `json:"unit"`
	Points []SeriesPoint `json:"points"`
	// Source는 원본(raw)에서 왔는지 롤업(hourly)에서 왔는지다.
	// 사용자가 데이터 해상도를 오해하지 않게 명시한다.
	Source string `json:"source"`
	// BucketSec은 한 점이 대표하는 시간 폭이다.
	BucketSec int `json:"bucketSec"`
}

// SeriesQuery는 시계열 조회 조건이다.
type SeriesQuery struct {
	ConnectionID string
	Metrics      []string
	From         time.Time
	To           time.Time
	// MaxPoints는 반환할 점의 최대 개수다. 이보다 많으면 시간 버킷으로 묶는다.
	// 브라우저에 수만 개의 점을 보내도 화면에는 픽셀 수만큼만 그려지므로 낭비다.
	MaxPoints int
}

// rawRetention은 원본 샘플 보존기간이다. 이보다 오래된 범위는 롤업에서 읽는다.
const rawRetention = 48 * time.Hour

// QuerySeries는 지표별 시계열을 반환한다.
//
// 요청 범위가 원본 보존기간 안에 있으면 원본을 시간 버킷으로 묶어 반환하고,
// 그보다 오래된 범위는 시간 단위 롤업에서 읽는다.
func (s *Store) QuerySeries(ctx context.Context, q SeriesQuery) ([]Series, error) {
	if len(q.Metrics) == 0 {
		return []Series{}, nil
	}
	if q.MaxPoints <= 0 {
		q.MaxPoints = 240
	}
	if q.To.IsZero() {
		q.To = time.Now().UTC()
	}
	if q.From.IsZero() {
		q.From = q.To.Add(-time.Hour)
	}

	useRaw := time.Since(q.From) <= rawRetention
	span := q.To.Sub(q.From)
	bucketSec := int(span.Seconds()) / q.MaxPoints
	if bucketSec < 1 {
		bucketSec = 1
	}

	out := make([]Series, 0, len(q.Metrics))
	for _, m := range q.Metrics {
		var series Series
		var err error
		if useRaw {
			series, err = s.querySeriesRaw(ctx, q, m, bucketSec)
		} else {
			series, err = s.querySeriesHourly(ctx, q, m)
		}
		if err != nil {
			return nil, err
		}
		out = append(out, series)
	}
	return out, nil
}

// querySeriesRaw는 원본 샘플을 초 단위 버킷으로 묶어 집계한다.
//
// SQLite에는 date_trunc가 없으므로 unixepoch를 버킷 크기로 나눈 몫으로 그룹화한다.
func (s *Store) querySeriesRaw(ctx context.Context, q SeriesQuery, metricName string, bucketSec int) (Series, error) {
	series := Series{Metric: metricName, Points: []SeriesPoint{}, Source: "raw", BucketSec: bucketSec}

	rows, err := s.db.QueryContext(ctx, `
		SELECT CAST(unixepoch(ts) / ? AS INTEGER) * ? AS bucket,
		       AVG(value), MIN(value), MAX(value), MAX(unit)
		FROM metric_samples
		WHERE connection_id = ? AND metric = ? AND ts >= ? AND ts <= ?
		GROUP BY bucket
		ORDER BY bucket`,
		bucketSec, bucketSec, q.ConnectionID, metricName,
		formatTime(q.From), formatTime(q.To))
	if err != nil {
		return series, fmt.Errorf("query raw series %s: %w", metricName, err)
	}
	defer rows.Close()

	for rows.Next() {
		var bucket int64
		var avg, min, max float64
		var unit sql.NullString
		if err := rows.Scan(&bucket, &avg, &min, &max, &unit); err != nil {
			return series, fmt.Errorf("scan raw series: %w", err)
		}
		series.Points = append(series.Points, SeriesPoint{
			Ts: time.Unix(bucket, 0).UTC(), Avg: avg, Min: min, Max: max,
		})
		if unit.Valid && series.Unit == "" {
			series.Unit = metric.Unit(unit.String)
		}
	}
	if err := rows.Err(); err != nil {
		return series, fmt.Errorf("iterate raw series: %w", err)
	}
	return series, nil
}

func (s *Store) querySeriesHourly(ctx context.Context, q SeriesQuery, metricName string) (Series, error) {
	series := Series{Metric: metricName, Points: []SeriesPoint{}, Source: "hourly", BucketSec: 3600}

	rows, err := s.db.QueryContext(ctx, `
		SELECT bucket, avg_value, min_value, max_value, unit
		FROM metric_hourly
		WHERE connection_id = ? AND metric = ? AND bucket >= ? AND bucket <= ?
		ORDER BY bucket`,
		q.ConnectionID, metricName,
		formatTime(q.From), formatTime(q.To))
	if err != nil {
		return series, fmt.Errorf("query hourly series %s: %w", metricName, err)
	}
	defer rows.Close()

	for rows.Next() {
		var bucket, unit string
		var avg, min, max float64
		if err := rows.Scan(&bucket, &avg, &min, &max, &unit); err != nil {
			return series, fmt.Errorf("scan hourly series: %w", err)
		}
		series.Points = append(series.Points, SeriesPoint{
			Ts: parseTime(bucket), Avg: avg, Min: min, Max: max,
		})
		if series.Unit == "" {
			series.Unit = metric.Unit(unit)
		}
	}
	if err := rows.Err(); err != nil {
		return series, fmt.Errorf("iterate hourly series: %w", err)
	}
	return series, nil
}

// ListMetricNames는 커넥션에 대해 수집된 지표 이름 목록을 반환한다.
func (s *Store) ListMetricNames(ctx context.Context, connectionID string) ([]string, error) {
	// 최근 데이터만 보면 되므로 범위를 제한해 전체 스캔을 피한다.
	since := formatTime(time.Now().Add(-6 * time.Hour))
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT metric FROM metric_samples
		 WHERE connection_id = ? AND ts >= ? ORDER BY metric`, connectionID, since)
	if err != nil {
		return nil, fmt.Errorf("list metric names: %w", err)
	}
	defer rows.Close()

	out := []string{}
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			return nil, fmt.Errorf("scan metric name: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate metric names: %w", err)
	}
	return out, nil
}

// ---------- 롤업 / 보존 ----------

// RollupHourly는 완결된 시간대의 원본 샘플을 시간 단위로 집계한다.
//
// 진행 중인 시간대는 건너뛴다 — 아직 샘플이 더 들어올 것이므로
// 집계하면 잘못된 평균이 고정된다.
func (s *Store) RollupHourly(ctx context.Context) (int64, error) {
	cutoff := formatTime(time.Now().Truncate(time.Hour))

	// strftime으로 정시 문자열을 만들어 버킷 키로 쓴다.
	// 소수점 자리수까지 timeLayout과 정확히 같게 맞춰야 한다 —
	// 자리수가 다르면 조회 시 문자열 비교가 시간 순서와 어긋난다.
	// ON CONFLICT로 재실행 시 같은 결과가 되도록(멱등) 만든다.
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO metric_hourly (connection_id, metric, bucket, unit, samples, avg_value, min_value, max_value)
		SELECT connection_id, metric,
		       strftime('%Y-%m-%dT%H:00:00.000000000Z', ts) AS bucket,
		       MAX(unit), COUNT(*), AVG(value), MIN(value), MAX(value)
		FROM metric_samples
		WHERE ts < ?
		GROUP BY connection_id, metric, bucket
		ON CONFLICT (connection_id, metric, bucket) DO UPDATE SET
			samples = excluded.samples,
			avg_value = excluded.avg_value,
			min_value = excluded.min_value,
			max_value = excluded.max_value,
			unit = excluded.unit`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("rollup hourly: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// PurgeMetrics는 보존기간이 지난 지표를 삭제한다.
func (s *Store) PurgeMetrics(ctx context.Context, rawRetain, hourlyRetain time.Duration) (int64, int64, error) {
	rawCutoff := formatTime(time.Now().Add(-rawRetain))
	res, err := s.db.ExecContext(ctx, `DELETE FROM metric_samples WHERE ts < ?`, rawCutoff)
	if err != nil {
		return 0, 0, fmt.Errorf("purge raw samples: %w", err)
	}
	rawDeleted, _ := res.RowsAffected()

	hourlyCutoff := formatTime(time.Now().Add(-hourlyRetain))
	res, err = s.db.ExecContext(ctx, `DELETE FROM metric_hourly WHERE bucket < ?`, hourlyCutoff)
	if err != nil {
		return rawDeleted, 0, fmt.Errorf("purge hourly rollups: %w", err)
	}
	hourlyDeleted, _ := res.RowsAffected()
	return rawDeleted, hourlyDeleted, nil
}

// MetricStorageStats는 지표 저장 현황이다. 설정 화면에서 용량을 보여주는 데 쓴다.
type MetricStorageStats struct {
	RawSamples   int64      `json:"rawSamples"`
	HourlyRows   int64      `json:"hourlyRows"`
	OldestRawAt  *time.Time `json:"oldestRawAt"`
	OldestHourly *time.Time `json:"oldestHourlyAt"`
}

func (s *Store) MetricStorageStats(ctx context.Context) (*MetricStorageStats, error) {
	st := &MetricStorageStats{}
	var oldestRaw, oldestHourly sql.NullString

	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*), MIN(ts) FROM metric_samples`).Scan(&st.RawSamples, &oldestRaw); err != nil {
		return nil, fmt.Errorf("raw stats: %w", err)
	}
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*), MIN(bucket) FROM metric_hourly`).Scan(&st.HourlyRows, &oldestHourly); err != nil {
		return nil, fmt.Errorf("hourly stats: %w", err)
	}
	st.OldestRawAt = parseTimePtr(oldestRaw)
	st.OldestHourly = parseTimePtr(oldestHourly)
	return st, nil
}

var _ = strings.TrimSpace
