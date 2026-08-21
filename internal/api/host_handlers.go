package api

import (
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"

	"dbstudio/internal/monitor"
	"dbstudio/internal/store"
)

// 호스트(=DB Studio가 도는 컴퓨터) 화면의 API.
//
// 커넥션 모니터링과 경로를 나란히 두되(/monitor/host) 권한 기준은 다르다: 여기에는
// 호스트 이름·디스크 경로·시스템 로그가 담기고, 그것은 특정 DB를 볼 수 있는 사람이
// 아니라 서버를 운영하는 사람의 정보다.

// handleHostOverview는 최신 스냅샷과 설정을 돌려준다.
func (s *Server) handleHostOverview(c *fiber.Ctx) error {
	th, err := s.st.HostThresholds(c.Context())
	if err != nil {
		return err
	}
	body := fiber.Map{"thresholds": th}

	if s.hostMon == nil {
		body["enabled"] = false
		body["note"] = "호스트 모니터링이 꺼져 있습니다 (-host-monitor=false)"
		return c.JSON(body)
	}

	cfg := s.hostMon.Config()
	body["enabled"] = cfg.Enabled
	body["intervalSec"] = int(cfg.Interval.Seconds())
	body["retentionHours"] = int(cfg.Retention.Hours())
	if note := s.hostMon.OSLogNote(); note != "" {
		body["osLogNote"] = note
	}

	if snap := s.hostMon.Latest(); snap != nil {
		body["snapshot"] = snap
	} else if state, err := s.st.HostState(c.Context()); err == nil && state != nil {
		// 아직 이번 실행의 첫 표본이 없으면 저장된 마지막 값을 보여준다.
		// 방금 재시작한 서버의 화면이 30초 동안 비어 있을 이유는 없다.
		body["snapshot"] = state.Snapshot
		body["stale"] = true
	}
	return c.JSON(body)
}

// handleHostSeries는 호스트 지표의 시계열을 돌려준다.
//
// 커넥션 지표와 핸들러를 나눈 이유: 저 쪽은 롤업 표를 함께 보고 커넥션 권한을 따진다.
// 한 함수에 두 규칙을 넣으면 어느 쪽 조건이 적용되는지 읽어서는 알 수 없게 된다.
func (s *Server) handleHostSeries(c *fiber.Ctx) error {
	from, to, err := parseTimeRange(c)
	if err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_range", err.Error())
	}

	names := []string{}
	for _, m := range strings.Split(c.Query("metrics"), ",") {
		if m = strings.TrimSpace(m); m != "" {
			names = append(names, m)
		}
	}
	if len(names) == 0 {
		names = []string{monitor.MetricHostCPU, monitor.MetricHostMemory}
	}
	if len(names) > 12 {
		return fail(c, fiber.StatusBadRequest, "too_many_metrics", "지표는 한 번에 12개까지 조회할 수 있습니다")
	}

	series := make([]fiber.Map, 0, len(names))
	for _, name := range names {
		points, err := s.st.HostSeries(c.Context(), name, from, to)
		if err != nil {
			return err
		}
		series = append(series, fiber.Map{"metric": name, "points": points})
	}
	return c.JSON(fiber.Map{"series": series, "from": from, "to": to})
}

// handleHostMetricNames는 저장된 지표 이름을 돌려준다.
// 디스크는 마운트마다 이름이 달라 프런트엔드가 미리 알 수 없다.
func (s *Server) handleHostMetricNames(c *fiber.Ctx) error {
	names, err := s.st.HostMetricNames(c.Context(), time.Now().Add(-24*time.Hour))
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"metrics": names})
}

// handleSaveHostThresholds는 임계값을 저장한다.
func (s *Server) handleSaveHostThresholds(c *fiber.Ctx) error {
	var in store.HostThresholds
	if err := c.BodyParser(&in); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "요청 형식이 올바르지 않습니다")
	}
	u := currentUser(c)
	if err := s.st.SaveHostThresholds(c.Context(), in, u.ID); err != nil {
		return err
	}
	s.audit(c, store.AuditParams{Action: "host.thresholds.updated", TargetType: "host"})

	saved, err := s.st.HostThresholds(c.Context())
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"thresholds": saved})
}
