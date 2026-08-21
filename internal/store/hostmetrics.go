package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// 호스트(=DB Studio가 도는 컴퓨터) 지표 저장.
//
// 커넥션 지표와 나란한 구조지만 표가 따로다(0027 마이그레이션의 주석 참고).
// 롤업 표는 두지 않는다: 호스트는 하나뿐이라 보존기간 안의 원본만으로도 행 수가
// 크지 않고(30초 간격이면 48시간에 지표당 5760행), 장기 추세는 이 앱이 답할 질문이
// 아니다 — 그것은 인프라 모니터링 도구의 몫이다.

// SettingHostThresholds는 호스트 임계값(JSON)이다.
//
// 플래그가 아니라 설정으로 둔 이유: "디스크 몇 %에서 알릴 것인가"는 서버를 띄우는
// 순간이 아니라 운영하면서 정해지는 값이다. 플래그로만 받으면 숫자 하나를 고치려고
// 프로세스를 재시작해야 한다.
const SettingHostThresholds = "host.thresholds"

// HostThresholds는 호스트 이벤트를 낼 기준이다. 값은 모두 퍼센트다.
type HostThresholds struct {
	CPUWarn  float64 `json:"cpuWarn"`
	CPUCrit  float64 `json:"cpuCrit"`
	MemWarn  float64 `json:"memWarn"`
	MemCrit  float64 `json:"memCrit"`
	DiskWarn float64 `json:"diskWarn"`
	DiskCrit float64 `json:"diskCrit"`
	// SustainSec은 CPU·메모리가 이 시간 이상 계속 넘어야 이벤트를 내는 기준이다.
	// 순간 최고치로 알리면 백업이나 인덱스 재작성 때마다 알림이 온다.
	SustainSec int `json:"sustainSec"`
	// OSLogEnabled는 시스템 로그에서 오류 줄을 읽어 이벤트로 만들지다.
	OSLogEnabled bool `json:"osLogEnabled"`
}

// DefaultHostThresholds는 아무 설정도 없을 때의 기준이다.
//
// 디스크를 CPU보다 낮게(85/95) 잡은 이유: CPU가 90%인 서버는 느릴 뿐이지만 디스크가
// 꽉 찬 DB 서버는 쓰기를 멈춘다. 디스크는 미리 알아야 손을 쓸 수 있다.
func DefaultHostThresholds() HostThresholds {
	return HostThresholds{
		CPUWarn: 85, CPUCrit: 95,
		MemWarn: 85, MemCrit: 95,
		DiskWarn: 85, DiskCrit: 95,
		SustainSec: 300, OSLogEnabled: true,
	}
}

// Normalize는 범위를 벗어난 값을 기본값으로 되돌린다.
//
// 저장할 때와 읽을 때 모두 부른다. 읽을 때도 하는 이유: 예전 버전이 남긴 값이나
// 손으로 고친 DB가 있을 수 있고, 임계값이 0이면 "항상 위반"이 되어 이벤트가 끝없이 쌓인다.
func (t HostThresholds) Normalize() HostThresholds {
	d := DefaultHostThresholds()
	fix := func(v, def float64) float64 {
		if v <= 0 || v > 100 {
			return def
		}
		return v
	}
	t.CPUWarn, t.CPUCrit = fix(t.CPUWarn, d.CPUWarn), fix(t.CPUCrit, d.CPUCrit)
	t.MemWarn, t.MemCrit = fix(t.MemWarn, d.MemWarn), fix(t.MemCrit, d.MemCrit)
	t.DiskWarn, t.DiskCrit = fix(t.DiskWarn, d.DiskWarn), fix(t.DiskCrit, d.DiskCrit)
	// 경고가 심각보다 높으면 심각 이벤트는 영원히 나오지 않는다. 뒤집힌 쌍은 맞바꾼다.
	swap := func(w, c float64) (float64, float64) {
		if w > c {
			return c, w
		}
		return w, c
	}
	t.CPUWarn, t.CPUCrit = swap(t.CPUWarn, t.CPUCrit)
	t.MemWarn, t.MemCrit = swap(t.MemWarn, t.MemCrit)
	t.DiskWarn, t.DiskCrit = swap(t.DiskWarn, t.DiskCrit)
	if t.SustainSec < 0 || t.SustainSec > 3600 {
		t.SustainSec = d.SustainSec
	}
	return t
}

// HostThresholds는 저장된 임계값을 돌려준다. 없으면 기본값이다.
func (s *Store) HostThresholds(ctx context.Context) (HostThresholds, error) {
	raw, err := s.GetSetting(ctx, SettingHostThresholds)
	if errors.Is(err, ErrNotFound) {
		return DefaultHostThresholds(), nil
	}
	if err != nil {
		return DefaultHostThresholds(), err
	}
	t := DefaultHostThresholds()
	if err := json.Unmarshal([]byte(raw), &t); err != nil {
		// 값이 깨졌다고 모니터링을 멈출 이유는 없다. 기본값으로 돈다.
		return DefaultHostThresholds(), nil
	}
	return t.Normalize(), nil
}

// SaveHostThresholds는 임계값을 저장한다.
func (s *Store) SaveHostThresholds(ctx context.Context, t HostThresholds, actorID string) error {
	b, err := json.Marshal(t.Normalize())
	if err != nil {
		return fmt.Errorf("marshal host thresholds: %w", err)
	}
	return s.SetSetting(ctx, SettingHostThresholds, string(b), actorID)
}

// HostSample은 저장할 지표 한 점이다.
type HostSample struct {
	Metric string
	Value  float64
	Unit   string
}

// HostStateRecord는 최신 상태다.
type HostStateRecord struct {
	At          time.Time       `json:"at"`
	Snapshot    json.RawMessage `json:"snapshot"`
	BootAt      string          `json:"bootAt,omitempty"`
	OSLogPath   string          `json:"osLogPath,omitempty"`
	OSLogOffset int64           `json:"osLogOffset,omitempty"`
}

// SaveHostSamples는 지표 점들과 최신 스냅샷을 한 트랜잭션에 쓴다.
//
// 둘을 나눠 쓰면 "차트에는 있는데 카드에는 없는" 순간이 생긴다. 폴링 주기가 짧아
// 눈에 띄지 않을 것 같지만, 장애 중에는 바로 그 한 번을 보게 된다.
func (s *Store) SaveHostSamples(ctx context.Context, at time.Time, samples []HostSample,
	snapshot any, bootAt string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin host samples: %w", err)
	}
	defer tx.Rollback()

	ts := formatTime(at)
	for _, sp := range samples {
		unit := sp.Unit
		if unit == "" {
			unit = "count"
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO host_samples (metric, ts, value, unit) VALUES (?, ?, ?, ?)`,
			sp.Metric, ts, sp.Value, unit); err != nil {
			return fmt.Errorf("insert host sample: %w", err)
		}
	}

	raw, err := json.Marshal(snapshot)
	if err != nil {
		return fmt.Errorf("marshal host snapshot: %w", err)
	}
	// os_log_* 는 여기서 건드리지 않는다. 로그 읽기는 다른 주기로 돌고,
	// 여기서 덮어쓰면 이미 읽은 자리를 잊어 같은 오류를 다시 이벤트로 만든다.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO host_state (id, at, snapshot, boot_at) VALUES (1, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET at = excluded.at, snapshot = excluded.snapshot,
			boot_at = excluded.boot_at`,
		ts, string(raw), bootAt); err != nil {
		return fmt.Errorf("save host state: %w", err)
	}
	return tx.Commit()
}

// HostState는 최신 상태를 읽는다. 아직 표본이 없으면 nil을 돌려준다.
func (s *Store) HostState(ctx context.Context) (*HostStateRecord, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT at, snapshot, boot_at, os_log_path, os_log_offset FROM host_state WHERE id = 1`)
	var at, snapshot, bootAt, logPath string
	var offset int64
	if err := row.Scan(&at, &snapshot, &bootAt, &logPath, &offset); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("read host state: %w", err)
	}
	return &HostStateRecord{
		At: parseTime(at), Snapshot: json.RawMessage(snapshot),
		BootAt: bootAt, OSLogPath: logPath, OSLogOffset: offset,
	}, nil
}

// SaveOSLogOffset은 시스템 로그를 어디까지 읽었는지 기록한다.
func (s *Store) SaveOSLogOffset(ctx context.Context, path string, offset int64) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO host_state (id, at, os_log_path, os_log_offset)
		VALUES (1, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET os_log_path = excluded.os_log_path,
			os_log_offset = excluded.os_log_offset`,
		formatTime(time.Now()), path, offset)
	if err != nil {
		return fmt.Errorf("save os log offset: %w", err)
	}
	return nil
}

// HostSeries는 한 지표의 시계열을 돌려준다.
func (s *Store) HostSeries(ctx context.Context, metric string, since, until time.Time) ([]SeriesPoint, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT ts, value FROM host_samples
		WHERE metric = ? AND ts >= ? AND ts <= ?
		ORDER BY ts`, metric, formatTime(since), formatTime(until))
	if err != nil {
		return nil, fmt.Errorf("query host series: %w", err)
	}
	defer rows.Close()

	out := []SeriesPoint{}
	for rows.Next() {
		var ts string
		var v float64
		if err := rows.Scan(&ts, &v); err != nil {
			return nil, fmt.Errorf("scan host series: %w", err)
		}
		// 원본 한 점이라 평균·최소·최대가 모두 같다. 그래도 같은 구조체를 쓰는 이유는
		// 화면(차트)이 커넥션 지표와 호스트 지표를 구분하지 않아도 되게 하기 위함이다.
		out = append(out, SeriesPoint{Ts: parseTime(ts), Avg: v, Min: v, Max: v})
	}
	return out, rows.Err()
}

// HostMetricNames는 저장된 지표 이름을 돌려준다.
// 디스크는 마운트마다 이름이 달라(host.disk./ 등) 미리 알 수 없다.
func (s *Store) HostMetricNames(ctx context.Context, since time.Time) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT metric FROM host_samples WHERE ts >= ? ORDER BY metric`,
		formatTime(since))
	if err != nil {
		return nil, fmt.Errorf("query host metrics: %w", err)
	}
	defer rows.Close()

	out := []string{}
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			return nil, fmt.Errorf("scan host metric: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// PurgeHostSamples는 보존기간이 지난 표본을 지운다.
func (s *Store) PurgeHostSamples(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM host_samples WHERE ts < ?`, formatTime(before))
	if err != nil {
		return 0, fmt.Errorf("purge host samples: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}
