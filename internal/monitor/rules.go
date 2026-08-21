// Package monitor는 대상 DB를 주기적으로 폴링해 지표를 저장하고,
// 임계치 룰을 평가해 이벤트를 만든다.
package monitor

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"dbstudio/internal/metric"
	"dbstudio/internal/model"
	"dbstudio/internal/store"
)

// breachState는 한 (커넥션, 룰) 조합의 위반 지속 상태다.
//
// 메모리에만 두는 이유: 지속 시간 조건은 "연속으로 위반 중"이라는 런타임 사실이고,
// 앱이 재시작되면 관측이 끊겼으므로 처음부터 다시 세는 것이 옳다.
// DB에 두면 재시작 후 "그 사이에도 계속 위반했다"고 잘못 가정하게 된다.
type breachState struct {
	since time.Time
	// opened는 이미 이벤트를 만들었는지다. 중복 생성을 막는다.
	opened bool
}

// RuleEngine은 룰 평가와 이벤트 개시/해소를 담당한다.
type RuleEngine struct {
	st   *store.Store
	sink EventSink

	mu     sync.Mutex
	breach map[string]*breachState // "connID\x00ruleID" → 상태
	// rules는 주기적으로 새로 읽어 캐시한다. 매 폴링마다 DB를 읽지 않기 위함이다.
	rules       []*store.Rule
	rulesLoaded time.Time
}

func NewRuleEngine(st *store.Store) *RuleEngine {
	return &RuleEngine{st: st, breach: map[string]*breachState{}}
}

// rulesTTL은 룰 캐시 유효기간이다. 사용자가 룰을 바꾸면 이 시간 내에 반영된다.
const rulesTTL = 30 * time.Second

// Rules는 캐시된 룰 목록을 반환한다.
func (e *RuleEngine) Rules(ctx context.Context) ([]*store.Rule, error) {
	e.mu.Lock()
	fresh := time.Since(e.rulesLoaded) < rulesTTL && e.rules != nil
	cached := e.rules
	e.mu.Unlock()
	if fresh {
		return cached, nil
	}

	rules, err := e.st.ListRules(ctx)
	if err != nil {
		return nil, fmt.Errorf("load rules: %w", err)
	}
	e.mu.Lock()
	e.rules = rules
	e.rulesLoaded = time.Now()
	e.mu.Unlock()
	return rules, nil
}

// InvalidateRules는 룰 캐시를 즉시 무효화한다. 룰 변경 API가 호출한다.
func (e *RuleEngine) InvalidateRules() {
	e.mu.Lock()
	e.rules = nil
	e.rulesLoaded = time.Time{}
	e.mu.Unlock()
}

func breachKey(connID, ruleID string) string { return connID + "\x00" + ruleID }

// EvaluateThresholds는 수집 결과에 대해 임계치 룰을 평가한다.
//
// 지속 시간 조건 처리: 위반이 처음 관측되면 시작 시각만 기록하고 이벤트는 만들지 않는다.
// 위반이 duration_sec 이상 이어지면 이벤트를 개시하고, 정상으로 돌아오면 해소한다.
func (e *RuleEngine) EvaluateThresholds(ctx context.Context, conn *model.Connection, set *metric.Set) error {
	rules, err := e.Rules(ctx)
	if err != nil {
		return err
	}
	now := time.Now()

	for _, rule := range rules {
		if rule.Kind != store.EventThreshold || !rule.AppliesTo(conn) {
			continue
		}
		sample, ok := set.Get(rule.Metric)
		if !ok {
			// 이 DB 종류가 해당 지표를 제공하지 않는다.
			// 예: Redis에 복제 지연 룰. 위반도 정상도 아니므로 상태를 건드리지 않는다.
			continue
		}

		key := breachKey(conn.ID, rule.ID)
		if !rule.Breached(sample.Value) {
			e.clearBreach(ctx, key, conn, rule)
			continue
		}

		e.mu.Lock()
		st := e.breach[key]
		if st == nil {
			st = &breachState{since: now}
			e.breach[key] = st
		}
		elapsed := now.Sub(st.since)
		shouldOpen := !st.opened && elapsed >= time.Duration(rule.DurationSec)*time.Second
		if shouldOpen {
			st.opened = true
		}
		e.mu.Unlock()

		if !shouldOpen {
			continue
		}

		meta := metric.Lookup(rule.Metric)
		value := sample.Value
		threshold := rule.Threshold
		message := fmt.Sprintf("%s: %s%s %s (현재 %s)",
			conn.Name, meta.Label, subjectParticle(meta.Label),
			thresholdPhrase(rule.Op, rule.Threshold, meta.Unit),
			formatValue(value, meta.Unit))

		eventID, created, err := e.st.OpenEvent(ctx, store.OpenEventParams{
			ConnectionID: conn.ID,
			RuleID:       rule.ID,
			Kind:         store.EventThreshold,
			Severity:     rule.Severity,
			Metric:       rule.Metric,
			Message:      message,
			Value:        &value,
			Threshold:    &threshold,
			Detail: map[string]any{
				"rule":        rule.Name,
				"connection":  conn.Name,
				"environment": conn.Environment,
				"durationSec": rule.DurationSec,
				"unit":        meta.Unit,
			},
		})
		if err != nil {
			return fmt.Errorf("open threshold event: %w", err)
		}
		if created {
			slog.Warn("모니터링 이벤트 개시", "connection", conn.Name,
				"rule", rule.Name, "metric", rule.Metric, "value", value)
		}
		notifyEvent(ctx, e.sink, e.st, eventID, created)
	}
	return nil
}

// clearBreach는 위반 상태를 지우고, 이벤트가 열려 있었다면 해소한다.
func (e *RuleEngine) clearBreach(ctx context.Context, key string, conn *model.Connection, rule *store.Rule) {
	e.mu.Lock()
	st := e.breach[key]
	wasOpened := st != nil && st.opened
	delete(e.breach, key)
	e.mu.Unlock()

	if !wasOpened {
		return
	}
	closed, err := e.st.ResolveEvents(ctx, conn.ID, store.EventThreshold, rule.Metric, rule.ID)
	if err != nil {
		slog.Error("이벤트 해소 실패", "connection", conn.Name, "rule", rule.Name, "err", err)
		return
	}
	notifyResolved(ctx, e.sink, closed)
	slog.Info("모니터링 이벤트 해소", "connection", conn.Name, "rule", rule.Name)
}

// ForgetConnection은 커넥션이 삭제되거나 비활성화될 때 상태를 정리한다.
func (e *RuleEngine) ForgetConnection(connID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for key := range e.breach {
		if len(key) > len(connID) && key[:len(connID)] == connID && key[len(connID)] == 0 {
			delete(e.breach, key)
		}
	}
}

// connectivityRule은 접속 실패 감지 룰을 찾는다. 없으면 nil이다.
func (e *RuleEngine) connectivityRule(ctx context.Context, conn *model.Connection) *store.Rule {
	rules, err := e.Rules(ctx)
	if err != nil {
		return nil
	}
	for _, r := range rules {
		if r.Kind == store.EventConnectivity && r.AppliesTo(conn) {
			return r
		}
	}
	return nil
}

// driftRule은 스키마 드리프트 감지 룰을 찾는다. 없으면 nil이다.
func (e *RuleEngine) driftRule(ctx context.Context, conn *model.Connection) *store.Rule {
	rules, err := e.Rules(ctx)
	if err != nil {
		return nil
	}
	for _, r := range rules {
		if r.Kind == store.EventDrift && r.AppliesTo(conn) {
			return r
		}
	}
	return nil
}

// subjectParticle은 앞 글자에 맞는 주격 조사(이/가)를 고른다.
//
// 지표 라벨이 데이터에서 오므로 조사를 고정하면 "쿼리이", "사용률가" 같은
// 어색한 문장이 사용자에게 그대로 노출된다. 한글 음절은 받침 유무를
// (코드 - 0xAC00) % 28 로 판별할 수 있다.
func subjectParticle(word string) string {
	runes := []rune(strings.TrimSpace(word))
	if len(runes) == 0 {
		return "이"
	}
	last := runes[len(runes)-1]

	// 괄호로 끝나면 그 앞의 글자를 본다: "메모리 사용률 (Redis)" 같은 라벨 대응.
	for i := len(runes) - 1; i >= 0 && (last == ')' || last == ']'); i-- {
		if i == 0 {
			return "이"
		}
		last = runes[i-1]
	}

	if last >= 0xAC00 && last <= 0xD7A3 {
		if (last-0xAC00)%28 == 0 {
			return "가" // 받침 없음
		}
		return "이" // 받침 있음
	}
	// 한글이 아니면(영문 지표명 등) 받침 있는 것으로 취급해 "이"를 쓴다.
	return "이"
}

// thresholdPhrase는 룰 조건을 한국어 문구로 만든다.
func thresholdPhrase(op string, threshold float64, unit metric.Unit) string {
	value := formatValue(threshold, unit)
	switch op {
	case ">":
		return value + " 초과"
	case ">=":
		return value + " 이상"
	case "<":
		return value + " 미만"
	case "<=":
		return value + " 이하"
	case "==":
		return value + "와 같음"
	case "!=":
		return value + "와 다름"
	}
	return value
}

// formatValue는 단위에 맞게 값을 사람이 읽는 문자열로 만든다.
func formatValue(v float64, unit metric.Unit) string {
	switch unit {
	case metric.UnitPercent:
		return fmt.Sprintf("%.1f%%", v)
	case metric.UnitBytes:
		return formatBytes(v)
	case metric.UnitMillis:
		if v >= 1000 {
			return fmt.Sprintf("%.2f초", v/1000)
		}
		return fmt.Sprintf("%.0fms", v)
	case metric.UnitSeconds:
		return formatDuration(v)
	case metric.UnitPerSec:
		return fmt.Sprintf("%.2f/s", v)
	}
	if v == float64(int64(v)) {
		return fmt.Sprintf("%d", int64(v))
	}
	return fmt.Sprintf("%.2f", v)
}

func formatBytes(v float64) string {
	units := []string{"B", "KB", "MB", "GB", "TB"}
	i := 0
	for v >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%.0f%s", v, units[i])
	}
	return fmt.Sprintf("%.1f%s", v, units[i])
}

func formatDuration(sec float64) string {
	switch {
	case sec < 60:
		return fmt.Sprintf("%.0f초", sec)
	case sec < 3600:
		return fmt.Sprintf("%.1f분", sec/60)
	case sec < 86400:
		return fmt.Sprintf("%.1f시간", sec/3600)
	}
	return fmt.Sprintf("%.1f일", sec/86400)
}
