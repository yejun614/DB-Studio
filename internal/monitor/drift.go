package monitor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"dbstudio/internal/dbx"
	"dbstudio/internal/model"
	"dbstudio/internal/schema"
	"dbstudio/internal/store"
)

// maybeCheckDrift는 주기가 되었으면 스키마 드리프트를 확인한다.
//
// 드리프트 = 이 앱을 거치지 않고 대상 DB의 스키마가 변경된 상태.
// 마이그레이션을 앱으로만 하도록 강제할 수는 없으므로, 외부 변경을 빨리 알아채
// 버전 이력에 반영할 수 있게 하는 것이 목적이다(P7의 "외부 편집으로 버전 등록").
func (m *Manager) maybeCheckDrift(ctx context.Context, conn *model.Connection, adapter dbx.Adapter, target dbx.Target) {
	if !adapter.Capabilities().Introspect {
		return
	}
	rule := m.engine.driftRule(ctx, conn)
	if rule == nil {
		return
	}

	m.mu.Lock()
	last := m.lastSchemaCheck[conn.ID]
	due := time.Since(last) >= m.cfg.SchemaInterval
	if due {
		m.lastSchemaCheck[conn.ID] = time.Now()
	}
	m.mu.Unlock()
	if !due {
		return
	}

	if err := m.CheckDrift(ctx, conn, adapter, target, rule); err != nil {
		slog.Error("스키마 드리프트 확인 실패", "connection", conn.Name, "err", err)
	}
}

// CheckDrift는 현재 스키마를 읽어 마지막 스냅샷과 비교한다.
//
// 반환값은 변경이 감지되었는지 여부다. rule이 nil이면 이벤트를 만들지 않고
// 스냅샷만 갱신한다 (사용자가 수동으로 확인하는 경우).
func (m *Manager) CheckDrift(ctx context.Context, conn *model.Connection, adapter dbx.Adapter, target dbx.Target, rule *store.Rule) error {
	// introspect는 지표 수집보다 오래 걸리므로 별도의 넉넉한 상한을 준다.
	introCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	current, err := adapter.Introspect(introCtx, target)
	if err != nil {
		// 스키마를 못 읽는 것은 드리프트가 아니다. 접속 이벤트가 이미 다룬다.
		return fmt.Errorf("introspect: %w", err)
	}

	prev, err := m.st.LatestSchemaSnapshot(ctx, conn.ID, true)
	if err != nil {
		return fmt.Errorf("load previous snapshot: %w", err)
	}

	// 첫 관측: 비교 대상이 없으므로 기준선만 저장한다.
	// 이때 드리프트 이벤트를 만들면 커넥션을 등록할 때마다 경고가 뜬다.
	if prev == nil {
		if _, _, err := m.st.SaveSchemaSnapshot(ctx, conn.ID, current,
			store.SnapshotSourceMonitor, []string{"최초 기준선"}); err != nil {
			return fmt.Errorf("save baseline snapshot: %w", err)
		}
		slog.Info("스키마 기준선 저장", "connection", conn.Name,
			"fingerprint", current.Fingerprint(), "tables", len(current.Tables))
		return nil
	}

	if prev.Fingerprint == current.Fingerprint() {
		return nil
	}

	// 무엇이 바뀌었는지 계산한다. 지문만으로는 "바뀌었다"까지만 알 수 있다.
	var summary []string
	var diff *schema.DiffResult
	if prev.Schema != nil {
		diff = schema.Diff(prev.Schema, current)
		for _, c := range diff.Changes {
			summary = append(summary, c.Summary)
		}
	} else {
		summary = []string{"이전 스냅샷의 상세를 읽을 수 없어 변경 내역을 계산하지 못했습니다"}
	}

	// 변경 내용을 설명할 수 없으면 이벤트를 만들지 않는다.
	//
	// MongoDB/Redis는 스키마가 "관찰된 구조"다. 문서 샘플이 달라지거나 키 수가 바뀌면
	// 지문이 달라지지만 그것은 외부 편집이 아니다. 게다가 diff 자체가 관계형만
	// 지원하므로(Unsupported) 변경 목록이 항상 비어 있다. 이대로 두면
	// 15분마다 "변경되었습니다 (0건)"라는 내용 없는 경고와 이벤트가 쌓인다.
	if diff != nil && len(diff.Unsupported) > 0 {
		if _, _, err := m.st.SaveSchemaSnapshot(ctx, conn.ID, current,
			store.SnapshotSourceMonitor,
			[]string{"관찰된 구조가 달라졌습니다 (샘플링 기반이므로 외부 편집으로 보지 않습니다)"}); err != nil {
			return fmt.Errorf("save observed snapshot: %w", err)
		}
		slog.Debug("관찰된 구조 변화 (드리프트로 보지 않음)",
			"connection", conn.Name, "shape", string(current.Shape),
			"reason", diff.Unsupported[0])
		return nil
	}

	snap, _, err := m.st.SaveSchemaSnapshot(ctx, conn.ID, current, store.SnapshotSourceMonitor, summary)
	if err != nil {
		return fmt.Errorf("save drifted snapshot: %w", err)
	}

	// 지문은 달라졌는데 설명할 변경이 없는 경우다(정규화 차이 등).
	// "0건 변경되었습니다"는 읽는 사람에게 아무 정보도 주지 않으면서
	// 경고를 무시하는 습관만 만든다. 스냅샷만 갱신하고 사실을 그대로 적는다.
	if len(summary) == 0 {
		slog.Info("스키마 지문이 달라졌지만 구조 변경은 없습니다",
			"connection", conn.Name, "snapshot", snap.ID,
			"prevFingerprint", prev.Fingerprint, "fingerprint", snap.Fingerprint)
		return nil
	}

	if rule == nil {
		return nil
	}

	destructive := 0
	if diff != nil {
		destructive = diff.DestructiveCount
	}
	severity := rule.Severity
	// 파괴적 변경이 섞인 외부 편집은 더 심각하게 취급한다.
	if destructive > 0 && severity != store.SeverityCritical {
		severity = store.SeverityCritical
	}

	changeCount := len(summary)
	value := float64(changeCount)
	message := fmt.Sprintf("%s: 앱 외부에서 스키마가 변경되었습니다 (%d건)", conn.Name, changeCount)

	detail := map[string]any{
		"connection":      conn.Name,
		"environment":     conn.Environment,
		"snapshotId":      snap.ID,
		"previousId":      prev.ID,
		"fingerprint":     snap.Fingerprint,
		"prevFingerprint": prev.Fingerprint,
		"destructive":     destructive,
	}
	// 변경 목록은 길 수 있으므로 앞부분만 이벤트에 담고 전체는 스냅샷에서 본다.
	if len(summary) > 10 {
		detail["changes"] = summary[:10]
		detail["truncated"] = len(summary) - 10
	} else {
		detail["changes"] = summary
	}

	// 드리프트는 매번 새 이벤트여야 한다. 같은 이벤트에 누적하면
	// 어떤 변경이 언제 있었는지가 뭉개진다. 그래서 이전 열린 드리프트 이벤트를
	// 먼저 해소하고 새로 개시한다.
	// 여기서 닫히는 것은 "문제가 끝나서"가 아니라 새 것으로 갈아 끼우기 위해서다.
	// 그래서 해소 알림을 보내지 않는다 — 보내면 채널에 "해소"와 "발생"이 나란히 찍혀
	// 무엇이 실제로 정리되었는지 읽을 수 없다.
	if _, err := m.st.ResolveEvents(ctx, conn.ID, store.EventDrift, "", rule.ID); err != nil {
		return fmt.Errorf("resolve previous drift event: %w", err)
	}
	eventID, _, err := m.st.OpenEvent(ctx, store.OpenEventParams{
		ConnectionID: conn.ID,
		RuleID:       rule.ID,
		Kind:         store.EventDrift,
		Severity:     severity,
		Message:      message,
		Value:        &value,
		Detail:       detail,
	})
	if err != nil {
		return fmt.Errorf("open drift event: %w", err)
	}
	// 드리프트는 직전 이벤트를 해소하고 새로 여는 구조라 언제나 새 이벤트다.
	notifyEvent(ctx, m.sink, m.st, eventID, true)

	slog.Warn("스키마 외부 변경 감지", "connection", conn.Name,
		"changes", changeCount, "destructive", destructive)
	return nil
}

// CheckDriftByID는 사용자 요청으로 드리프트를 즉시 확인한다.
func (m *Manager) CheckDriftByID(ctx context.Context, connectionID string) (*store.SchemaSnapshot, bool, error) {
	conn, err := m.st.GetConnection(ctx, connectionID)
	if err != nil {
		return nil, false, err
	}
	adapter, err := dbx.Get(conn.Kind)
	if err != nil {
		return nil, false, err
	}
	if !adapter.Capabilities().Introspect {
		return nil, false, errors.New("이 DB 종류는 스키마 조회를 지원하지 않습니다")
	}
	secret, err := m.st.GetSecret(ctx, conn.ID)
	if err != nil {
		return nil, false, err
	}

	before, err := m.st.LatestSchemaSnapshot(ctx, conn.ID, false)
	if err != nil {
		return nil, false, err
	}

	target := dbx.Target{Conn: conn, Secret: secret}
	rule := m.engine.driftRule(ctx, conn)
	if err := m.CheckDrift(ctx, conn, adapter, target, rule); err != nil {
		return nil, false, err
	}

	// 확인 주기를 리셋해 방금 확인한 결과가 곧 다시 덮이지 않게 한다.
	m.mu.Lock()
	m.lastSchemaCheck[conn.ID] = time.Now()
	m.mu.Unlock()

	after, err := m.st.LatestSchemaSnapshot(ctx, conn.ID, false)
	if err != nil {
		return nil, false, err
	}
	changed := before == nil || after == nil || before.Fingerprint != after.Fingerprint
	return after, changed, nil
}
