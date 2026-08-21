package api

import (
	"context"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"

	"dbstudio/internal/dblog"
	"dbstudio/internal/dbx"
	"dbstudio/internal/model"
	"dbstudio/internal/store"
)

// 로그 조회는 여러 소스를 순차로 읽으므로 지표 수집보다 넉넉한 상한을 둔다.
const logQueryTimeout = 60 * time.Second

// handleGetLogs는 대상 DB의 로그 항목과 쿼리 통계를 반환한다.
//
// 쿼리 파라미터:
//
//	range=1h            간편 시간 범위 (또는 from/to)
//	sources=a,b         조회할 소스 (생략하면 전부)
//	severity=warning    이 심각도 이상만
//	q=검색어             메시지/쿼리 부분 문자열 검색
//	regex=1             q를 정규식으로 해석
//	minDuration=100     이보다 느린 쿼리만 (ms)
//	order=total|mean|calls|max   통계 정렬 기준
//	limit=200
func (s *Server) handleGetLogs(c *fiber.Ctx) error {
	connID := c.Params("id")
	conn, adapter, err := s.resolveLogAccess(c, connID)
	if err != nil {
		return err
	}

	filter, err := parseLogFilter(c)
	if err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_filter", err.Error())
	}

	secret, err := s.st.GetSecret(c.Context(), connID)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(c.Context(), logQueryTimeout)
	defer cancel()

	res, err := adapter.Logs(ctx, dbx.Target{Conn: conn, Secret: secret}, filter)
	if err != nil {
		s.audit(c, store.AuditParams{
			Action: "logs.read", TargetType: "connection", TargetID: connID,
			Result: "error", Detail: map[string]any{"name": conn.Name, "error": err.Error()},
		})
		return failDetail(c, fiber.StatusBadGateway, "log_query_failed",
			"로그를 읽지 못했습니다", err.Error())
	}

	s.audit(c, store.AuditParams{
		Action: "logs.read", TargetType: "connection", TargetID: connID,
		Detail: map[string]any{
			"name": conn.Name, "entries": len(res.Entries), "stats": len(res.Stats),
			"search": filter.Search, "sources": filter.Sources,
		},
	})

	return c.JSON(fiber.Map{
		"connection": connSummary(conn),
		"from":       filter.From,
		"to":         filter.To,
		"entries":    res.Entries,
		"stats":      res.Stats,
		"sources":    res.Sources,
		"notes":      res.Notes,
		"truncated":  res.Truncated,
		"orderBy":    filter.StatsOrderBy,
	})
}

// handleLogSources는 이 커넥션에서 어떤 로그 소스를 쓸 수 있는지만 빠르게 확인한다.
// 화면을 그리기 전에 소스 목록을 알아야 필터 UI를 구성할 수 있다.
func (s *Server) handleLogSources(c *fiber.Ctx) error {
	connID := c.Params("id")
	conn, adapter, err := s.resolveLogAccess(c, connID)
	if err != nil {
		return err
	}
	secret, err := s.st.GetSecret(c.Context(), connID)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(c.Context(), logQueryTimeout)
	defer cancel()

	// 항목을 최소로 요청해 소스 가용성만 확인한다.
	probe := &dblog.Filter{Limit: 1}
	probe.Normalize()
	res, err := adapter.Logs(ctx, dbx.Target{Conn: conn, Secret: secret}, probe)
	if err != nil {
		return failDetail(c, fiber.StatusBadGateway, "log_query_failed",
			"로그 소스를 확인하지 못했습니다", err.Error())
	}
	return c.JSON(fiber.Map{
		"connection": connSummary(conn),
		"sources":    res.Sources,
		"notes":      res.Notes,
	})
}

// resolveLogAccess는 로그 조회 권한과 지원 여부를 확인한다.
//
// 로그에는 실행된 쿼리 원문이 포함되고 그 안에 리터럴 값(개인정보 등)이 들어갈 수
// 있다. 모니터링 등급을 기준으로 하는 것은 계획에 정의된 대로이며, 화면에서
// 그 사실을 사용자에게 알린다.
func (s *Server) resolveLogAccess(c *fiber.Ctx, connID string) (*model.Connection, dbx.Adapter, error) {
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
	adapter, err := dbx.Get(conn.Kind)
	if err != nil {
		return nil, nil, fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	if !adapter.Capabilities().Logs {
		return nil, nil, fiber.NewError(fiber.StatusBadRequest,
			"이 데이터베이스 종류는 로그 조회를 지원하지 않습니다")
	}
	return conn, adapter, nil
}

// parseLogFilter는 쿼리 파라미터를 로그 필터로 변환한다.
func parseLogFilter(c *fiber.Ctx) (*dblog.Filter, error) {
	from, to, err := parseTimeRange(c)
	if err != nil {
		return nil, err
	}

	f := &dblog.Filter{From: from, To: to}

	if raw := strings.TrimSpace(c.Query("sources")); raw != "" {
		for _, s := range strings.Split(raw, ",") {
			s = strings.TrimSpace(s)
			if s == "" {
				continue
			}
			kind := dblog.SourceKind(s)
			if _, ok := dblog.SourceLabels[kind]; !ok {
				return nil, errors.New("알 수 없는 로그 소스입니다: " + s)
			}
			f.Sources = append(f.Sources, kind)
		}
	}

	if sev := strings.TrimSpace(c.Query("severity")); sev != "" {
		severity := dblog.Severity(sev)
		if !severity.Valid() {
			return nil, errors.New("알 수 없는 심각도입니다: " + sev)
		}
		f.MinSeverity = severity
	}

	f.Search = strings.TrimSpace(c.Query("q"))
	f.Regex = c.Query("regex") == "1"
	if f.Regex && f.Search != "" {
		// 잘못된 정규식은 조회 전에 거부한다. 나중에 조용히 부분 문자열 검색으로
		// 대체되면 사용자는 자기 패턴이 동작했다고 오해한다.
		if _, err := regexp.Compile(f.Search); err != nil {
			return nil, errors.New("정규식이 올바르지 않습니다: " + err.Error())
		}
	}

	if v := strings.TrimSpace(c.Query("minDuration")); v != "" {
		ms, err := strconv.ParseFloat(v, 64)
		if err != nil || ms < 0 {
			return nil, errors.New("minDuration은 0 이상의 숫자(ms)여야 합니다")
		}
		f.MinDurationMs = ms
	}

	order := strings.TrimSpace(c.Query("order"))
	if !dblog.ValidStatsOrder(order) {
		return nil, errors.New("order는 total, mean, calls, max 중 하나여야 합니다")
	}
	f.StatsOrderBy = order

	if v := c.Query("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			f.Limit = n
		}
	}

	f.Normalize()
	return f, nil
}

// handleLogMeta는 로그 화면이 필요한 정적 메타데이터를 제공한다.
func (s *Server) handleLogMeta(c *fiber.Ctx) error {
	sources := make([]fiber.Map, 0, len(dblog.SourceLabels))
	// 순서를 고정해 UI 필터가 매번 같은 순서로 보이게 한다.
	for _, kind := range []dblog.SourceKind{
		dblog.SourceSlowQuery, dblog.SourceStatements, dblog.SourceCurrent,
		dblog.SourceErrorLog, dblog.SourceProfiler, dblog.SourceSlowLog,
	} {
		sources = append(sources, fiber.Map{
			"kind":  kind,
			"label": dblog.Label(kind),
			"help":  sourceHelp[kind],
		})
	}

	return c.JSON(fiber.Map{
		"sources": sources,
		"severities": []fiber.Map{
			{"value": dblog.SeverityDebug, "label": "디버그"},
			{"value": dblog.SeverityInfo, "label": "정보"},
			{"value": dblog.SeverityWarning, "label": "경고"},
			{"value": dblog.SeverityError, "label": "오류"},
			{"value": dblog.SeverityFatal, "label": "치명적"},
		},
		"statsOrders": []fiber.Map{
			{"value": "total", "label": "총 소요 시간", "help": "개선 효과가 가장 큰 순서입니다"},
			{"value": "mean", "label": "평균 소요 시간", "help": "한 번 실행이 가장 느린 순서입니다"},
			{"value": "calls", "label": "호출 횟수", "help": "가장 자주 실행된 순서입니다"},
			{"value": "max", "label": "최대 소요 시간", "help": "최악의 사례가 큰 순서입니다"},
		},
	})
}

// sourceHelp는 각 로그 소스가 무엇을 보여주는지 설명한다.
// 소스마다 필요한 DB 설정이 달라 사용자가 기대를 조정할 수 있어야 한다.
var sourceHelp = map[dblog.SourceKind]string{
	dblog.SourceSlowQuery: "설정된 임계 시간을 넘긴 쿼리. MySQL은 slow_query_log와 log_output=TABLE이 필요합니다",
	dblog.SourceStatements: "쿼리별 누적 실행 통계. 개선 대상을 고르는 데 쓰며 " +
		"PostgreSQL은 pg_stat_statements 확장, MySQL은 performance_schema가 필요합니다",
	dblog.SourceCurrent:  "지금 실행 중인 쿼리. 멈춘 쿼리나 락 대기를 찾을 때 봅니다",
	dblog.SourceErrorLog: "서버가 기록한 오류/경고. 파일로만 기록되는 경우 SQL로 읽지 못할 수 있습니다",
	dblog.SourceProfiler: "MongoDB의 system.profile. db.setProfilingLevel로 활성화합니다",
	dblog.SourceSlowLog:  "Redis SLOWLOG. slowlog-log-slower-than 설정으로 임계값을 정합니다",
}
