package api

import (
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"

	"dbstudio/internal/metric"
	"dbstudio/internal/model"
	"dbstudio/internal/store"
)

// accessibleConnectionIDs는 현재 사용자가 모니터링 등급 이상으로 볼 수 있는
// 커넥션 ID 목록을 반환한다.
//
// nil이 아닌 빈 슬라이스를 반환할 수 있다는 점이 중요하다. 저장 계층은 nil을
// "제한 없음"으로, 빈 슬라이스를 "아무것도 볼 수 없음"으로 해석한다.
// 이 구분이 없으면 권한 없는 사용자에게 전체 이벤트가 노출된다.
func (s *Server) accessibleConnectionIDs(c *fiber.Ctx) ([]string, map[string]*model.Connection, error) {
	u := currentUser(c)
	all, err := s.st.ListConnections(c.Context())
	if err != nil {
		return nil, nil, err
	}
	accessible, _, err := s.authz.FilterAccessible(c.Context(), u, all, model.LevelMonitor)
	if err != nil {
		return nil, nil, err
	}
	// 프로젝트로 한 번 더 좁힌다.
	//
	// 권한 판정이 이미 프로젝트를 관문으로 쓰므로 여기서 새로 막히는 것은 없다.
	// 이것은 **보고 있는 프로젝트**로 목록을 맞추는 일이다 — 이 함수를 지나는
	// 화면(ERD·마이그레이션·이벤트·로그·모니터링)이 모두 같은 답을 쓰게 된다.
	scope, err := s.projectFilter(c)
	if err != nil {
		return nil, nil, err
	}
	ids := make([]string, 0, len(accessible))
	byID := make(map[string]*model.Connection, len(accessible))
	for _, conn := range accessible {
		if !inProjects(scope, conn.ProjectID) {
			continue
		}
		ids = append(ids, conn.ID)
		byID[conn.ID] = conn
	}
	return ids, byID, nil
}

// canSeeSystemEvents는 커넥션에 속하지 않은 이벤트(호스트 상태 등)를 볼 수 있는지다.
//
// 커넥션 관리 권한을 기준으로 삼는 이유: 호스트 이벤트는 "DB Studio가 도는 컴퓨터"의
// 이야기라 특정 커넥션의 열람 권한으로는 판단할 수 없고, 디스크 경로나 호스트 이름처럼
// 서버 운영자만 알아야 할 것이 담긴다. assertEventAckAccess도 같은 기준을 쓴다.
func canSeeSystemEvents(c *fiber.Ctx) bool {
	u := currentUser(c)
	return u != nil && u.Role.CanManageConnections()
}

// handleMonitorOverview는 대시보드 첫 화면에 필요한 모든 것을 한 번에 반환한다.
// 커넥션 목록, 최신 상태, 이벤트 요약을 따로 요청하면 화면이 단계적으로 채워져 산만하다.
func (s *Server) handleMonitorOverview(c *fiber.Ctx) error {
	ids, byID, err := s.accessibleConnectionIDs(c)
	if err != nil {
		return err
	}

	states, err := s.st.ListConnectionStates(c.Context())
	if err != nil {
		return err
	}
	summary, err := s.st.EventSummary(c.Context(), ids, canSeeSystemEvents(c))
	if err != nil {
		return err
	}

	// 열린 이벤트를 커넥션별로 집계해 카드에 배지로 표시한다.
	openEvents, _, err := s.st.ListEvents(c.Context(), store.EventFilter{
		ConnectionIDs: ids, State: "open", Limit: 500,
	})
	if err != nil {
		return err
	}
	openByConn := map[string]map[string]int{}
	for _, e := range openEvents {
		if openByConn[e.ConnectionID] == nil {
			openByConn[e.ConnectionID] = map[string]int{}
		}
		openByConn[e.ConnectionID][string(e.Severity)]++
	}

	items := make([]fiber.Map, 0, len(ids))
	for _, id := range ids {
		conn := byID[id]
		item := fiber.Map{
			"connection": connSummary(conn),
			"enabled":    conn.Enabled,
			"openEvents": openByConn[id],
			"tags":       conn.Tags,
		}
		if st, ok := states[id]; ok {
			item["state"] = st
		}
		items = append(items, item)
	}

	cfg := s.monitor.Config()
	return c.JSON(fiber.Map{
		"items":   items,
		"summary": summary,
		"config": fiber.Map{
			"intervalSec":       int(cfg.Interval.Seconds()),
			"schemaIntervalSec": int(cfg.SchemaInterval.Seconds()),
			"rawRetentionHours": int(cfg.RawRetention.Hours()),
		},
	})
}

// handleMonitorMetrics는 지표 시계열을 반환한다.
//
// 쿼리 파라미터:
//
//	metrics=a,b,c   조회할 지표 (생략하면 주요 지표)
//	from, to        RFC3339 시각 (생략하면 최근 1시간)
//	range=1h|6h|24h|7d  간편 범위 지정
//	points=240      최대 점 개수
func (s *Server) handleMonitorMetrics(c *fiber.Ctx) error {
	connID := c.Params("id")
	if _, _, err := s.resolveMonitorAccess(c, connID); err != nil {
		return err
	}

	from, to, err := parseTimeRange(c)
	if err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_range", err.Error())
	}

	var metrics []string
	if raw := strings.TrimSpace(c.Query("metrics")); raw != "" {
		for _, m := range strings.Split(raw, ",") {
			if m = strings.TrimSpace(m); m != "" {
				metrics = append(metrics, m)
			}
		}
	} else {
		// 지표를 지정하지 않으면 실제로 수집된 것 중 주요 지표만 고른다.
		// 전부 반환하면 응답이 커지고 차트가 30개씩 그려진다.
		available, err := s.st.ListMetricNames(c.Context(), connID)
		if err != nil {
			return err
		}
		for _, name := range available {
			if metric.Lookup(name).Primary {
				metrics = append(metrics, name)
			}
		}
		if len(metrics) == 0 {
			metrics = available
		}
	}

	points := 240
	if v := c.Query("points"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 2000 {
			points = n
		}
	}

	series, err := s.st.QuerySeries(c.Context(), store.SeriesQuery{
		ConnectionID: connID, Metrics: metrics,
		From: from, To: to, MaxPoints: points,
	})
	if err != nil {
		return err
	}

	// 지표 표시 정보를 함께 보내 프론트엔드가 라벨/단위를 하드코딩하지 않게 한다.
	meta := make([]metric.Meta, 0, len(series))
	for _, sr := range series {
		m := metric.Lookup(sr.Metric)
		if sr.Unit != "" {
			m.Unit = sr.Unit
		}
		meta = append(meta, m)
	}

	return c.JSON(fiber.Map{
		"connectionId": connID,
		"from":         from,
		"to":           to,
		"series":       series,
		"meta":         meta,
	})
}

// handleMonitorAvailableMetrics는 커넥션에서 수집된 지표 목록을 반환한다.
func (s *Server) handleMonitorAvailableMetrics(c *fiber.Ctx) error {
	connID := c.Params("id")
	if _, _, err := s.resolveMonitorAccess(c, connID); err != nil {
		return err
	}
	names, err := s.st.ListMetricNames(c.Context(), connID)
	if err != nil {
		return err
	}
	out := make([]metric.Meta, 0, len(names))
	for _, n := range names {
		out = append(out, metric.Lookup(n))
	}
	return c.JSON(fiber.Map{"metrics": out})
}

// resolveMonitorAccess는 모니터링 등급 권한을 확인한다.
// 실패 시 *fiber.Error를 반환한다 (응답을 직접 쓰는 헬퍼를 쓰면 nil 에러가 되어
// 호출부의 검사를 통과해버린다).
func (s *Server) resolveMonitorAccess(c *fiber.Ctx, connID string) (*model.Connection, *store.ConnectionState, error) {
	conn, err := s.st.GetConnection(c.Context(), connID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil, fiber.NewError(fiber.StatusNotFound, "커넥션을 찾을 수 없습니다")
	}
	if err != nil {
		return nil, nil, err
	}
	d, err := s.requireLevel(c, connID, model.LevelMonitor)
	if err != nil {
		return nil, nil, err
	}
	if !d.Allowed {
		return nil, nil, fiber.NewError(fiber.StatusForbidden, d.Reason)
	}
	state, err := s.st.GetConnectionState(c.Context(), connID)
	if err != nil {
		return nil, nil, err
	}
	return conn, state, nil
}

// parseTimeRange는 from/to 또는 range 파라미터를 시간 범위로 해석한다.
func parseTimeRange(c *fiber.Ctx) (time.Time, time.Time, error) {
	now := time.Now().UTC()

	if r := strings.TrimSpace(c.Query("range")); r != "" {
		d, err := parseRangeShorthand(r)
		if err != nil {
			return time.Time{}, time.Time{}, err
		}
		return now.Add(-d), now, nil
	}

	from := now.Add(-time.Hour)
	to := now
	if v := c.Query("from"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return time.Time{}, time.Time{}, errors.New("from 형식이 올바르지 않습니다 (RFC3339 필요)")
		}
		from = t.UTC()
	}
	if v := c.Query("to"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return time.Time{}, time.Time{}, errors.New("to 형식이 올바르지 않습니다 (RFC3339 필요)")
		}
		to = t.UTC()
	}
	if !to.After(from) {
		return time.Time{}, time.Time{}, errors.New("to는 from보다 이후여야 합니다")
	}
	// 과도한 범위는 롤업으로도 무거우므로 상한을 둔다.
	if to.Sub(from) > 180*24*time.Hour {
		return time.Time{}, time.Time{}, errors.New("조회 범위는 180일을 넘을 수 없습니다")
	}
	return from, to, nil
}

func parseRangeShorthand(r string) (time.Duration, error) {
	switch r {
	case "15m":
		return 15 * time.Minute, nil
	case "1h":
		return time.Hour, nil
	case "6h":
		return 6 * time.Hour, nil
	case "24h", "1d":
		return 24 * time.Hour, nil
	case "7d":
		return 7 * 24 * time.Hour, nil
	case "30d":
		return 30 * 24 * time.Hour, nil
	}
	return 0, errors.New("range는 15m, 1h, 6h, 24h, 7d, 30d 중 하나여야 합니다")
}

// ---------- 이벤트 ----------

func (s *Server) handleListEvents(c *fiber.Ctx) error {
	ids, byID, err := s.accessibleConnectionIDs(c)
	if err != nil {
		return err
	}

	f := store.EventFilter{
		ConnectionIDs: ids,
		IncludeSystem: canSeeSystemEvents(c),
		Kind:          c.Query("kind"),
		Severity:      store.Severity(c.Query("severity")),
		State:         c.Query("state"),
		OnlyUnacked:   c.Query("unacked") == "1",
	}
	// 특정 커넥션으로 좁히는 경우에도 접근 가능 목록 안에서만 허용한다.
	if connID := strings.TrimSpace(c.Query("connectionId")); connID != "" {
		if _, ok := byID[connID]; !ok {
			return fail(c, fiber.StatusForbidden, "forbidden", "이 커넥션의 이벤트를 볼 권한이 없습니다")
		}
		// 한 커넥션으로 좁혔으면 호스트 이벤트는 뺀다. "이 DB의 이벤트"를 물었는데
		// 컴퓨터 전체 이야기가 섞여 나오면 목록을 좁힌 의미가 없다.
		f.ConnectionIDs, f.IncludeSystem = []string{connID}, false
	}
	if f.Severity != "" && !f.Severity.Valid() {
		return fail(c, fiber.StatusBadRequest, "invalid_severity", "알 수 없는 심각도입니다")
	}
	if v := c.Query("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			f.Limit = n
		}
	}
	if v := c.Query("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			f.Offset = n
		}
	}

	events, total, err := s.st.ListEvents(c.Context(), f)
	if err != nil {
		return err
	}

	// 커넥션 이름을 함께 보내 프론트엔드가 별도 조회를 하지 않게 한다.
	names := map[string]string{}
	for id, conn := range byID {
		names[id] = conn.Name
	}
	return c.JSON(fiber.Map{"events": events, "total": total, "connectionNames": names})
}

func (s *Server) handleAckEvent(c *fiber.Ctx) error {
	id, err := strconv.ParseInt(c.Params("id"), 10, 64)
	if err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "이벤트 ID가 올바르지 않습니다")
	}
	if err := s.assertEventAccess(c, id); err != nil {
		return err
	}

	u := currentUser(c)
	if err := s.st.AckEvent(c.Context(), id, u.ID); errors.Is(err, store.ErrNotFound) {
		return fail(c, fiber.StatusNotFound, "not_found", "이벤트를 찾을 수 없습니다")
	} else if err != nil {
		return err
	}
	s.audit(c, store.AuditParams{
		Action: "event.acked", TargetType: "event", TargetID: c.Params("id"),
	})
	return c.JSON(fiber.Map{"ok": true})
}

func (s *Server) handleResolveEvent(c *fiber.Ctx) error {
	id, err := strconv.ParseInt(c.Params("id"), 10, 64)
	if err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "이벤트 ID가 올바르지 않습니다")
	}
	if err := s.assertEventAccess(c, id); err != nil {
		return err
	}

	if err := s.st.ResolveEventByID(c.Context(), id); errors.Is(err, store.ErrNotFound) {
		return fail(c, fiber.StatusNotFound, "not_found", "열려 있는 이벤트를 찾을 수 없습니다")
	} else if err != nil {
		return err
	}
	s.audit(c, store.AuditParams{
		Action: "event.resolved", TargetType: "event", TargetID: c.Params("id"),
	})
	return c.JSON(fiber.Map{"ok": true})
}

// assertEventAccess는 이벤트가 속한 커넥션에 대한 권한을 확인한다.
func (s *Server) assertEventAccess(c *fiber.Ctx, eventID int64) error {
	event, err := s.st.GetEvent(c.Context(), eventID)
	if errors.Is(err, store.ErrNotFound) {
		return fiber.NewError(fiber.StatusNotFound, "이벤트를 찾을 수 없습니다")
	}
	if err != nil {
		return err
	}
	// 커넥션에 속하지 않은 시스템 이벤트는 커넥션 관리자만 다룰 수 있다.
	if event.ConnectionID == "" {
		if !currentUser(c).Role.CanManageConnections() {
			return fiber.NewError(fiber.StatusForbidden, "이 이벤트에 접근할 권한이 없습니다")
		}
		return nil
	}
	d, err := s.requireLevel(c, event.ConnectionID, model.LevelMonitor)
	if err != nil {
		return err
	}
	if !d.Allowed {
		return fiber.NewError(fiber.StatusForbidden, d.Reason)
	}
	return nil
}

// ---------- 룰 ----------

func (s *Server) handleListRules(c *fiber.Ctx) error {
	rules, err := s.st.ListRules(c.Context())
	if err != nil {
		return err
	}
	// 특정 DB를 겨냥한 룰은 그 DB가 보이는 사람에게만 보인다.
	//
	// 룰 이름과 임계값에는 그 DB의 사정이 담긴다("결제-운영 커넥션 90% 초과").
	// 대상이 안 보이는데 룰만 보이면 그 자체가 남의 프로젝트 이야기를 흘리는 셈이고,
	// 고칠 수도 없는 줄이 목록에 남는다.
	_, byID, err := s.accessibleConnectionIDs(c)
	if err != nil {
		return err
	}
	// 조건 설명을 함께 보내 목록 화면이 연산자 조합을 다시 조립하지 않게 한다.
	described := make([]fiber.Map, 0, len(rules))
	for _, r := range rules {
		// 대상이 없는 룰은 전체에 걸리는 규칙이라 그대로 둔다.
		if r.ConnectionID != "" && byID[r.ConnectionID] == nil {
			continue
		}
		described = append(described, fiber.Map{"rule": r, "describe": r.Describe()})
	}
	return c.JSON(fiber.Map{"rules": described, "metrics": metric.Catalog()})
}

type ruleRequest struct {
	Name         string            `json:"name"`
	ConnectionID string            `json:"connectionId"`
	Environment  model.Environment `json:"environment"`
	Kind         string            `json:"kind"`
	Metric       string            `json:"metric"`
	Op           string            `json:"op"`
	Threshold    float64           `json:"threshold"`
	DurationSec  int               `json:"durationSec"`
	Severity     store.Severity    `json:"severity"`
	Enabled      *bool             `json:"enabled"`
	Description  string            `json:"description"`
}

func (r *ruleRequest) toRule(id string) *store.Rule {
	enabled := true
	if r.Enabled != nil {
		enabled = *r.Enabled
	}
	return &store.Rule{
		ID: id, Name: strings.TrimSpace(r.Name),
		ConnectionID: r.ConnectionID, Environment: r.Environment,
		Kind: r.Kind, Metric: strings.TrimSpace(r.Metric), Op: r.Op,
		Threshold: r.Threshold, DurationSec: r.DurationSec,
		Severity: r.Severity, Enabled: enabled, Description: r.Description,
	}
}

func (s *Server) handleCreateRule(c *fiber.Ctx) error {
	var req ruleRequest
	if err := c.BodyParser(&req); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "요청 형식이 올바르지 않습니다")
	}
	rule := req.toRule("")
	if err := store.ValidateRule(rule); err != nil {
		return fail(c, fiber.StatusBadRequest, "invalid_rule", err.Error())
	}
	if err := s.assertRuleScope(c, rule); err != nil {
		return err
	}

	created, err := s.st.CreateRule(c.Context(), rule)
	if err != nil {
		return err
	}
	s.monitor.Engine().InvalidateRules()
	s.audit(c, store.AuditParams{
		Action: "monitor.rule.created", TargetType: "rule", TargetID: created.ID,
		Detail: map[string]any{"name": created.Name, "metric": created.Metric, "kind": created.Kind},
	})
	return c.Status(fiber.StatusCreated).JSON(fiber.Map{"rule": created})
}

func (s *Server) handleUpdateRule(c *fiber.Ctx) error {
	id := c.Params("id")
	if _, err := s.st.GetRule(c.Context(), id); errors.Is(err, store.ErrNotFound) {
		return fail(c, fiber.StatusNotFound, "not_found", "룰을 찾을 수 없습니다")
	} else if err != nil {
		return err
	}

	var req ruleRequest
	if err := c.BodyParser(&req); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "요청 형식이 올바르지 않습니다")
	}
	rule := req.toRule(id)
	if err := store.ValidateRule(rule); err != nil {
		return fail(c, fiber.StatusBadRequest, "invalid_rule", err.Error())
	}
	if err := s.assertRuleScope(c, rule); err != nil {
		return err
	}

	updated, err := s.st.UpdateRule(c.Context(), rule)
	if errors.Is(err, store.ErrNotFound) {
		return fail(c, fiber.StatusNotFound, "not_found", "룰을 찾을 수 없습니다")
	}
	if err != nil {
		return err
	}
	s.monitor.Engine().InvalidateRules()
	s.audit(c, store.AuditParams{
		Action: "monitor.rule.updated", TargetType: "rule", TargetID: id,
		Detail: map[string]any{"name": updated.Name, "enabled": updated.Enabled},
	})
	return c.JSON(fiber.Map{"rule": updated})
}

func (s *Server) handleDeleteRule(c *fiber.Ctx) error {
	id := c.Params("id")
	rule, err := s.st.GetRule(c.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		return fail(c, fiber.StatusNotFound, "not_found", "룰을 찾을 수 없습니다")
	}
	if err != nil {
		return err
	}

	if err := s.st.DeleteRule(c.Context(), id); err != nil {
		return err
	}
	s.monitor.Engine().InvalidateRules()
	s.audit(c, store.AuditParams{
		Action: "monitor.rule.deleted", TargetType: "rule", TargetID: id,
		Detail: map[string]any{"name": rule.Name},
	})
	return c.JSON(fiber.Map{"ok": true})
}

// assertRuleScope는 룰이 지정한 커넥션에 대한 권한을 확인한다.
// 커넥션을 지정하지 않은 전역 룰은 커넥션 관리자만 만들 수 있다.
func (s *Server) assertRuleScope(c *fiber.Ctx, rule *store.Rule) error {
	u := currentUser(c)
	if rule.ConnectionID == "" {
		if !u.Role.CanManageConnections() {
			return fiber.NewError(fiber.StatusForbidden,
				"전체 커넥션에 적용되는 룰은 관리자만 만들 수 있습니다")
		}
		return nil
	}
	d, err := s.requireLevel(c, rule.ConnectionID, model.LevelMonitor)
	if err != nil {
		return err
	}
	if !d.Allowed {
		return fiber.NewError(fiber.StatusForbidden, d.Reason)
	}
	return nil
}

// ---------- 스키마 드리프트 ----------

// handleCheckDrift는 스키마 외부 변경을 즉시 확인한다.
func (s *Server) handleCheckDrift(c *fiber.Ctx) error {
	connID := c.Params("id")
	conn, _, err := s.resolveMonitorAccess(c, connID)
	if err != nil {
		return err
	}

	snap, changed, err := s.monitor.CheckDriftByID(c.Context(), connID)
	if err != nil {
		return failDetail(c, fiber.StatusBadGateway, "drift_check_failed",
			"스키마를 확인하지 못했습니다", err.Error())
	}

	s.audit(c, store.AuditParams{
		Action: "monitor.drift.checked", TargetType: "connection", TargetID: connID,
		Detail: map[string]any{"name": conn.Name, "changed": changed},
	})
	return c.JSON(fiber.Map{"changed": changed, "snapshot": snap})
}

// handleListSnapshots는 스키마 스냅샷 이력을 반환한다.
func (s *Server) handleListSnapshots(c *fiber.Ctx) error {
	connID := c.Params("id")
	if _, _, err := s.resolveMonitorAccess(c, connID); err != nil {
		return err
	}
	limit := 50
	if v := c.Query("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	snaps, err := s.st.ListSchemaSnapshots(c.Context(), connID, limit)
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"snapshots": snaps})
}

// handleGetSnapshot는 스냅샷 하나를 스키마 본문과 함께 반환한다.
func (s *Server) handleGetSnapshot(c *fiber.Ctx) error {
	id, err := strconv.ParseInt(c.Params("snapshotId"), 10, 64)
	if err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "스냅샷 ID가 올바르지 않습니다")
	}
	connID := c.Params("id")
	if _, _, err := s.resolveMonitorAccess(c, connID); err != nil {
		return err
	}

	snap, err := s.st.GetSchemaSnapshot(c.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		return fail(c, fiber.StatusNotFound, "not_found", "스냅샷을 찾을 수 없습니다")
	}
	if err != nil {
		return err
	}
	// 다른 커넥션의 스냅샷을 ID만 바꿔 읽는 것을 막는다.
	if snap.ConnectionID != connID {
		return fail(c, fiber.StatusNotFound, "not_found", "스냅샷을 찾을 수 없습니다")
	}
	return c.JSON(fiber.Map{"snapshot": snap})
}

// handleStorageStats는 지표 저장 현황을 반환한다.
func (s *Server) handleStorageStats(c *fiber.Ctx) error {
	stats, err := s.st.MetricStorageStats(c.Context())
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"storage": stats})
}
