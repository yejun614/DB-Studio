package monitor

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"dbstudio/internal/applog"
	"dbstudio/internal/hostmon"
	"dbstudio/internal/store"
)

// 호스트(=DB Studio가 도는 컴퓨터) 감시.
//
// 커넥션 폴러와 한 파일에 두지 않은 이유: 저 쪽은 "커넥션 목록을 돌며 DB에 묻는다"가
// 전부지만 여기에는 커넥션이 없다. 룰도 커넥션별 룰 표를 쓰지 않고 전역 임계값 하나를
// 쓴다. 두 규칙을 한 루프에 섞으면 어느 쪽 조건을 고치는지 알 수 없게 된다.

// HostConfig는 호스트 감시 설정이다. 임계값은 여기 없다 — 그것은 운영 중에 바뀌므로
// 설정 표(store.HostThresholds)에서 매번 읽는다.
type HostConfig struct {
	Enabled   bool
	Interval  time.Duration
	Retention time.Duration
	// OSLogPath는 읽을 시스템 로그 파일이다. 비어 있으면 흔한 경로를 찾아본다.
	OSLogPath string
	// StartupNote는 이전 실행이 종료 기록을 남기지 않았을 때의 설명이다.
	// 비어 있으면 정상 종료였다는 뜻이다.
	StartupNote string
	// Version은 시작 이벤트에 남길 앱 버전이다.
	Version string
}

func DefaultHostConfig() HostConfig {
	return HostConfig{
		Enabled:   true,
		Interval:  30 * time.Second,
		Retention: 48 * time.Hour,
	}
}

// HostMonitor는 호스트 지표를 모으고 임계값을 평가한다.
type HostMonitor struct {
	st  *store.Store
	col *hostmon.Collector
	cfg HostConfig
	// observeOnly는 "모으되 저장하지 않는다"다. 클러스터 리플리카에서 켠다.
	observeOnly bool

	mu sync.RWMutex
	// last는 화면이 즉시 쓸 수 있는 최신 표본이다. DB를 거치지 않는 이유는
	// 폴링 주기 사이에 들어온 요청도 같은 값을 보아야 하기 때문이다.
	last *hostmon.Snapshot
	// over는 지표별로 "임계를 넘기 시작한 시각"이다. 지속 조건 판정에 쓴다.
	over map[string]time.Time
	// opened는 지금 열려 있는 호스트 이벤트의 지표다.
	//
	// DB에 물어보지 않고 기억하는 이유: 정상 범위일 때마다 UPDATE를 날리면 아무 일도
	// 없는 서버가 30초마다 SQLite에 쓰기 잠금을 건다. 시작할 때 한 번 DB에서 읽어
	// 맞춘다(loadOpen) — 그러지 않으면 재시작 직후에는 이미 열린 이벤트를 모른다.
	opened map[string]bool
	sink   EventSink
	// oslog는 시스템 로그 리더다. 첫 호출 때 저장된 위치를 읽어 만든다.
	oslog *hostmon.OSLog
	// oslogNote는 시스템 로그를 못 읽는 이유다. 화면에 그대로 보여준다.
	oslogNote string
}

func NewHostMonitor(st *store.Store, cfg HostConfig) *HostMonitor {
	return &HostMonitor{
		st: st, col: hostmon.New(), cfg: cfg,
		over: map[string]time.Time{}, opened: map[string]bool{},
	}
}

func (h *HostMonitor) SetEventSink(s EventSink) { h.sink = s }

// SetObserveOnly는 지표를 **모으기만 하고 저장하지 않게** 한다.
//
// 클러스터 리플리카가 쓴다. 자세한 이유는 sample의 주석에 있다.
func (h *HostMonitor) SetObserveOnly(v bool) { h.observeOnly = v }

func (h *HostMonitor) Config() HostConfig { return h.cfg }

// Latest는 마지막 표본이다. 아직 없으면 nil이다.
func (h *HostMonitor) Latest() *hostmon.Snapshot {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.last
}

// Run은 감시 루프를 시작한다.
func (h *HostMonitor) Run(ctx context.Context) {
	if !h.cfg.Enabled {
		slog.Warn("호스트 모니터링이 비활성화되어 있습니다 (-host-monitor=false)")
		return
	}
	slog.Info("호스트 모니터 시작", "interval", h.cfg.Interval)

	h.loadOpen(ctx)
	h.recordStartup(ctx)

	ticker := time.NewTicker(h.cfg.Interval)
	defer ticker.Stop()
	// 보존 정리와 시스템 로그 읽기는 지표 수집보다 훨씬 드물게 한다.
	slow := time.NewTicker(5 * time.Minute)
	defer slow.Stop()

	h.sample(ctx)
	h.readOSLog(ctx)

	for {
		select {
		case <-ctx.Done():
			slog.Info("호스트 모니터 종료")
			return
		case <-ticker.C:
			h.sample(ctx)
		case <-slow.C:
			h.readOSLog(ctx)
			h.purge(ctx)
		}
	}
}

// sample은 한 번 읽어 저장하고 임계값을 평가한다.
func (h *HostMonitor) sample(ctx context.Context) {
	defer applog.Recover("hostmon.sample")

	snap := h.col.Sample()
	h.mu.Lock()
	h.last = snap
	h.mu.Unlock()

	// 관측 전용 모드(클러스터 리플리카)에서는 여기까지다.
	//
	// 기록하지 않는 이유: 리플리카의 메타 DB는 마스터의 복사본이므로, 여기 쓴 지표는
	// 다음 복제에서 사라진다. 대신 방금 담아 둔 last가 하트비트에 실려 마스터의 노드
	// 목록에 남는다 — 각 서버의 상태는 보이면서도 사라질 쓰기를 하지 않는다.
	if h.observeOnly {
		return
	}

	prev, err := h.st.HostState(ctx)
	if err != nil {
		slog.Error("호스트 상태 조회 실패", "err", err)
	}

	bootAt := ""
	if !snap.Info.BootAt.IsZero() {
		bootAt = snap.Info.BootAt.UTC().Format(time.RFC3339)
	}
	if err := h.st.SaveHostSamples(ctx, snap.At, hostSamples(snap), snap, bootAt); err != nil {
		slog.Error("호스트 지표 저장 실패", "err", err)
		return
	}

	h.checkBoot(ctx, prev, snap)

	th, err := h.st.HostThresholds(ctx)
	if err != nil {
		slog.Warn("호스트 임계값을 읽지 못해 기본값으로 판정합니다", "err", err)
		th = store.DefaultHostThresholds()
	}
	h.evaluate(ctx, snap, th)
}

// hostSamples는 스냅샷에서 저장할 지표를 고른다.
//
// 못 읽은 값은 아예 넣지 않는다(0을 넣지 않는다). 차트에서 "0%"와 "모름"은 전혀 다른
// 뜻인데, 0으로 채우면 둘을 영원히 구분할 수 없다.
func hostSamples(s *hostmon.Snapshot) []store.HostSample {
	out := []store.HostSample{}
	add := func(name string, v float64, unit string) {
		out = append(out, store.HostSample{Metric: name, Value: v, Unit: unit})
	}
	if s.CPUPercent != nil {
		add(MetricHostCPU, *s.CPUPercent, "percent")
	}
	if s.Load1 != nil {
		add(MetricHostLoad1, *s.Load1, "count")
	}
	if s.MemTotal > 0 {
		add(MetricHostMemory, s.MemUsedPercent(), "percent")
		add(MetricHostMemUsed, float64(s.MemUsed), "bytes")
	}
	if s.SwapTotal > 0 {
		add(MetricHostSwap, float64(s.SwapUsed)/float64(s.SwapTotal)*100, "percent")
	}
	if s.NetRXRate != nil {
		add(MetricHostNetRX, *s.NetRXRate, "bytes_per_sec")
	}
	if s.NetTXRate != nil {
		add(MetricHostNetTX, *s.NetTXRate, "bytes_per_sec")
	}
	if s.ProcRSS > 0 {
		add(MetricHostProcRSS, float64(s.ProcRSS), "bytes")
	}
	for _, d := range s.Disks {
		if d.Total == 0 {
			continue
		}
		add(DiskMetric(d.Mount), d.UsedPercent(), "percent")
	}
	return out
}

// 호스트 지표 이름.
//
// 커넥션 지표(metric 패키지)와 이름 공간을 나눈 이유: 같은 표에 섞이지는 않지만
// 화면과 API에서는 나란히 나오고, "cpu"가 어느 쪽 CPU인지 묻게 되는 순간 그 화면은
// 신뢰를 잃는다.
const (
	MetricHostCPU     = "host.cpu"
	MetricHostLoad1   = "host.load1"
	MetricHostMemory  = "host.memory"
	MetricHostMemUsed = "host.memory.used"
	MetricHostSwap    = "host.swap"
	MetricHostNetRX   = "host.net.rx"
	MetricHostNetTX   = "host.net.tx"
	MetricHostProcRSS = "host.proc.rss"
	// MetricHostDiskPrefix 뒤에 마운트 지점이 붙는다(host.disk:C:, host.disk:/).
	MetricHostDiskPrefix = "host.disk:"
)

// DiskMetric은 마운트 지점의 지표 이름이다.
func DiskMetric(mount string) string { return MetricHostDiskPrefix + mount }

// evaluate는 임계값을 평가해 이벤트를 열고 닫는다.
func (h *HostMonitor) evaluate(ctx context.Context, s *hostmon.Snapshot, th store.HostThresholds) {
	sustain := time.Duration(th.SustainSec) * time.Second

	if s.CPUPercent != nil {
		h.checkSustained(ctx, sustainCheck{
			metric: MetricHostCPU, label: "CPU 사용률", value: *s.CPUPercent,
			warn: th.CPUWarn, crit: th.CPUCrit, sustain: sustain, at: s.At,
		})
	}
	if s.MemTotal > 0 {
		h.checkSustained(ctx, sustainCheck{
			metric: MetricHostMemory, label: "메모리 사용률", value: s.MemUsedPercent(),
			warn: th.MemWarn, crit: th.MemCrit, sustain: sustain, at: s.At,
			detail: map[string]any{
				"used": s.MemUsed, "total": s.MemTotal,
			},
		})
	}
	for _, d := range s.Disks {
		if d.Total == 0 {
			continue
		}
		// 디스크는 지속 조건을 두지 않는다. 사용률은 순간적으로 튀지 않고,
		// 찼다면 그 순간부터 이미 문제다.
		h.checkSustained(ctx, sustainCheck{
			metric: DiskMetric(d.Mount), label: "디스크 사용률 (" + d.Mount + ")",
			value: d.UsedPercent(), warn: th.DiskWarn, crit: th.DiskCrit, at: s.At,
			detail: map[string]any{
				"mount": d.Mount, "free": d.Free, "total": d.Total,
			},
		})
	}
}

// sustainCheck는 임계 판정 한 건의 입력이다.
type sustainCheck struct {
	metric  string
	label   string
	value   float64
	warn    float64
	crit    float64
	sustain time.Duration
	at      time.Time
	detail  map[string]any
}

// checkSustained는 한 지표의 임계 위반을 판정한다.
//
// 해소 기준을 경고선보다 낮게 잡는 이유(히스테리시스): 임계선 바로 위아래에서 값이
// 흔들리면 열고 닫기를 반복해 이벤트 목록이 같은 이야기로 가득 찬다.
func (h *HostMonitor) checkSustained(ctx context.Context, c sustainCheck) {
	severity := store.Severity("")
	threshold := c.warn
	switch {
	case c.value >= c.crit:
		severity, threshold = store.SeverityCritical, c.crit
	case c.value >= c.warn:
		severity, threshold = store.SeverityWarning, c.warn
	}

	if severity == "" {
		h.mu.Lock()
		open := h.opened[c.metric]
		// 해소는 경고선의 95% 아래로 내려왔을 때만 한다. 그 사이의 값은
		// "아직 위험 근처"이므로 열어 둔 채 둔다.
		inBand := open && c.value > c.warn*0.95
		if !inBand {
			delete(h.over, c.metric)
			delete(h.opened, c.metric)
		}
		h.mu.Unlock()
		if inBand || !open {
			return
		}
		closed, err := h.st.ResolveEvents(ctx, "", store.EventHost, c.metric, "")
		if err != nil {
			slog.Error("호스트 이벤트 해소 실패", "metric", c.metric, "err", err)
		}
		notifyResolved(ctx, h.sink, closed)
		return
	}

	h.mu.Lock()
	since, ok := h.over[c.metric]
	if !ok {
		since = c.at
		h.over[c.metric] = since
	}
	h.mu.Unlock()

	if c.sustain > 0 && c.at.Sub(since) < c.sustain {
		// 아직 지속 조건을 만족하지 않았다. 화면의 카드에는 이미 값이 보인다.
		return
	}

	detail := map[string]any{"since": since.Format(time.RFC3339)}
	for k, v := range c.detail {
		detail[k] = v
	}
	msg := fmt.Sprintf("%s이 %.0f%% 입니다 (기준 %.0f%%)", c.label, c.value, threshold)
	if c.sustain > 0 {
		msg = fmt.Sprintf("%s이 %s 동안 %.0f%% 이상입니다 (기준 %.0f%%)",
			c.label, humanDuration(c.at.Sub(since)), c.value, threshold)
	}
	value, th := c.value, threshold
	id, created, err := h.st.OpenEvent(ctx, store.OpenEventParams{
		Kind: store.EventHost, Severity: severity, Metric: c.metric,
		Message: msg, Value: &value, Threshold: &th, Detail: detail,
	})
	if err != nil {
		slog.Error("호스트 이벤트 개시 실패", "metric", c.metric, "err", err)
		return
	}
	h.mu.Lock()
	h.opened[c.metric] = true
	h.mu.Unlock()
	if created {
		slog.Warn("호스트 임계 초과", "metric", c.metric, "value", c.value, "severity", severity)
	}
	notifyEvent(ctx, h.sink, h.st, id, created)
}

// humanDuration은 지속 시간을 사람이 읽는 말로 바꾼다.
func humanDuration(d time.Duration) string {
	switch {
	case d >= time.Hour:
		return fmt.Sprintf("%d시간", int(d.Hours()))
	case d >= time.Minute:
		return fmt.Sprintf("%d분", int(d.Minutes()))
	default:
		return fmt.Sprintf("%d초", int(d.Seconds()))
	}
}

// checkBoot은 부팅 시각이 바뀌었으면 재부팅 이벤트를 만든다.
//
// 첫 관측에는 이벤트를 만들지 않는다. 그때는 "재부팅되었다"가 아니라 "이제 보기
// 시작했다"이고, 그 둘을 구분하지 않으면 이 기능을 켠 모든 서버가 재부팅했다고 말한다.
func (h *HostMonitor) checkBoot(ctx context.Context, prev *store.HostStateRecord, s *hostmon.Snapshot) {
	if s.Info.BootAt.IsZero() || prev == nil || prev.BootAt == "" {
		return
	}
	before, err := time.Parse(time.RFC3339, prev.BootAt)
	if err != nil {
		return
	}
	// 부팅 시각은 매번 조금씩 흔들린다(가동 시간에서 역산하므로).
	// 2분 이상 차이 날 때만 다른 부팅으로 본다.
	if diff := s.Info.BootAt.Sub(before); diff > -2*time.Minute && diff < 2*time.Minute {
		return
	}

	id, created, err := h.st.OpenEvent(ctx, store.OpenEventParams{
		Kind: store.EventHost, Severity: store.SeverityWarning, Metric: "host.boot",
		Message: fmt.Sprintf("호스트가 재부팅되었습니다 (부팅 시각 %s)",
			s.Info.BootAt.Local().Format("2006-01-02 15:04")),
		Detail: map[string]any{
			"hostname": s.Info.Hostname,
			"bootAt":   s.Info.BootAt.Format(time.RFC3339),
			"previous": prev.BootAt,
		},
	})
	if err != nil {
		slog.Error("재부팅 이벤트 개시 실패", "err", err)
		return
	}
	if created {
		slog.Warn("호스트 재부팅 감지", "bootAt", s.Info.BootAt)
	}
	notifyEvent(ctx, h.sink, h.st, id, created)
}

// recordStartup은 앱이 시작했다는 사실과, 이전 실행이 어떻게 끝났는지를 남긴다.
//
// 로그에도 남는 내용을 이벤트로 다시 만드는 이유: 로그는 서버에 접근할 수 있는
// 사람만 본다. "언제부터 이 앱이 안 돌고 있었는가"는 화면에서 답할 수 있어야 한다.
func (h *HostMonitor) recordStartup(ctx context.Context) {
	severity := store.SeverityInfo
	msg := "DB Studio가 시작되었습니다"
	if h.cfg.Version != "" {
		msg += " (버전 " + h.cfg.Version + ")"
	}
	metricName := "host.app.start"
	detail := map[string]any{"at": time.Now().Format(time.RFC3339)}

	if h.cfg.StartupNote != "" {
		severity = store.SeverityWarning
		metricName = "host.app.crash"
		msg = "이전 실행이 종료 기록을 남기지 않았습니다 (강제 종료·전원 차단·크래시)"
		detail["previous"] = h.cfg.StartupNote
	}

	id, created, err := h.st.OpenEvent(ctx, store.OpenEventParams{
		Kind: store.EventHost, Severity: severity, Metric: metricName,
		Message: msg, Detail: detail,
	})
	if err != nil {
		slog.Error("시작 이벤트 기록 실패", "err", err)
		return
	}
	// 정상 시작은 조치할 것이 없으므로 바로 해소한다. 목록에는 기록으로 남는다.
	// 비정상 종료는 열어 둔다 — 사람이 보고 확인 처리해야 할 일이다.
	if severity == store.SeverityInfo {
		if err := h.st.ResolveEventByID(ctx, id); err != nil {
			slog.Debug("시작 이벤트 해소 실패", "err", err)
		}
	}
	notifyEvent(ctx, h.sink, h.st, id, created)
}

// readOSLog는 시스템 로그의 새 오류 줄을 이벤트로 만든다.
func (h *HostMonitor) readOSLog(ctx context.Context) {
	defer applog.Recover("hostmon.oslog")

	th, err := h.st.HostThresholds(ctx)
	if err == nil && !th.OSLogEnabled {
		h.setOSLogNote("시스템 로그 감시가 꺼져 있습니다")
		return
	}

	if h.oslog == nil {
		state, err := h.st.HostState(ctx)
		if err != nil {
			slog.Warn("시스템 로그 위치를 읽지 못했습니다", "err", err)
		}
		savedPath, savedOffset := "", int64(0)
		if state != nil {
			savedPath, savedOffset = state.OSLogPath, state.OSLogOffset
		}
		h.oslog = hostmon.NewOSLog(h.cfg.OSLogPath, savedPath, savedOffset)
	}
	if !h.oslog.Available() {
		h.setOSLogNote("읽을 시스템 로그를 찾지 못했습니다")
		return
	}

	entries, note := h.oslog.Read()
	h.setOSLogNote(note)
	// 위치는 항목이 없어도 저장한다. 그러지 않으면 다음 읽기가 같은 구간을 다시 훑는다.
	if err := h.st.SaveOSLogOffset(ctx, h.oslog.Path(), h.oslog.Offset()); err != nil {
		slog.Warn("시스템 로그 위치를 저장하지 못했습니다", "err", err)
	}
	if len(entries) == 0 {
		return
	}

	// 여러 줄을 한 이벤트로 묶는다. 줄마다 이벤트를 만들면 디스크 하나가 죽을 때
	// 같은 이야기로 목록이 가득 찬다.
	lines := make([]string, 0, len(entries))
	for _, e := range entries {
		lines = append(lines, e.Message)
	}
	count := float64(len(entries))
	id, created, err := h.st.OpenEvent(ctx, store.OpenEventParams{
		Kind: store.EventHost, Severity: store.SeverityWarning, Metric: "host.oslog",
		Message: fmt.Sprintf("시스템 로그에 오류가 기록되었습니다: %s", entries[0].Message),
		Value:   &count,
		Detail: map[string]any{
			"source": h.oslog.Path(),
			"lines":  lines,
		},
	})
	if err != nil {
		slog.Error("시스템 로그 이벤트 개시 실패", "err", err)
		return
	}
	if created {
		slog.Warn("시스템 로그 오류", "source", h.oslog.Path(), "count", len(entries))
	}
	notifyEvent(ctx, h.sink, h.st, id, created)
}

func (h *HostMonitor) setOSLogNote(note string) {
	h.mu.Lock()
	h.oslogNote = note
	h.mu.Unlock()
}

// OSLogNote는 시스템 로그를 못 읽는 이유다(읽고 있으면 빈 문자열).
func (h *HostMonitor) OSLogNote() string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.oslogNote
}

// loadOpen은 이미 열려 있는 호스트 이벤트를 기억에 채운다.
func (h *HostMonitor) loadOpen(ctx context.Context) {
	events, _, err := h.st.ListEvents(ctx, store.EventFilter{
		Kind: store.EventHost, State: "open", Limit: 200,
	})
	if err != nil {
		slog.Warn("열린 호스트 이벤트를 읽지 못했습니다", "err", err)
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, e := range events {
		if e.Metric != "" {
			h.opened[e.Metric] = true
		}
	}
}

// purge는 보존기간이 지난 호스트 지표를 지운다.
func (h *HostMonitor) purge(ctx context.Context) {
	if h.cfg.Retention <= 0 {
		return
	}
	n, err := h.st.PurgeHostSamples(ctx, time.Now().Add(-h.cfg.Retention))
	if err != nil {
		slog.Error("호스트 지표 정리 실패", "err", err)
		return
	}
	if n > 0 {
		slog.Debug("호스트 지표 정리", "deleted", n)
	}
}
