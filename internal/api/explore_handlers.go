package api

import (
	"context"
	"errors"
	"time"

	"github.com/gofiber/fiber/v2"

	"dbstudio/internal/dbx"
	"dbstudio/internal/model"
	"dbstudio/internal/store"
)

// 탐색은 컬렉션마다 통계와 샘플링을 반복하거나 키를 표본 스캔하므로
// introspect와 같은 수준의 여유를 준다.
const exploreTimeout = 90 * time.Second

// handleExplore는 MongoDB/Redis 특화 조회 결과를 반환한다.
//
// 스키마 화면(/schema)과 분리한 이유: 두 DB에는 테이블·컬럼으로 표현할 수 없는
// 정보(컬렉션 저장 크기, 인덱스 사용 횟수, 메모리 정책, TTL 없는 키)가 있고,
// 그것이 실제 운영에서 필요한 정보다. 관계형 모델에 억지로 맞추면 둘 다 나빠진다.
//
// 권한은 스키마 조회와 같은 모니터링 등급이다. 구조와 통계만 노출하며
// 문서나 값 자체는 반환하지 않는다 — 데이터 접근은 이 앱의 권한 모델에 없는 개념이다.
func (s *Server) handleExplore(c *fiber.Ctx) error {
	connID := c.Params("id")
	conn, err := s.st.GetConnection(c.Context(), connID)
	if errors.Is(err, store.ErrNotFound) {
		return fiber.NewError(fiber.StatusNotFound, "커넥션을 찾을 수 없습니다")
	}
	if err != nil {
		return err
	}

	d, err := s.requireLevel(c, connID, model.LevelMonitor)
	if err != nil {
		return err
	}
	if !d.Allowed {
		return fiber.NewError(fiber.StatusForbidden, d.Reason)
	}

	adapter, err := dbx.Get(conn.Kind)
	if err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", err.Error())
	}
	if !adapter.Capabilities().Explore {
		// 관계형 DB는 스키마 화면이 같은 역할을 한다. 빈 결과를 주면
		// "지원하지 않는다"와 "읽을 것이 없다"를 구분할 수 없다.
		return fail(c, fiber.StatusBadRequest, "not_supported",
			"이 데이터베이스 종류는 전용 탐색 화면이 없습니다. 스키마 화면을 사용하세요")
	}
	if !conn.Enabled {
		return fail(c, fiber.StatusBadRequest, "disabled", "비활성화된 커넥션입니다")
	}

	secret, err := s.st.GetSecret(c.Context(), conn.ID)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(c.Context(), exploreTimeout)
	defer cancel()

	result, err := dbx.DoExplore(ctx, dbx.Target{Conn: conn, Secret: secret})
	if err != nil {
		s.audit(c, store.AuditParams{
			Action: "explore.read", TargetType: "connection", TargetID: conn.ID,
			Result: "error", Detail: map[string]any{"name": conn.Name, "error": err.Error()},
		})
		if errors.Is(err, dbx.ErrNotImplemented) {
			return fail(c, fiber.StatusBadRequest, "not_supported",
				"이 데이터베이스 종류는 전용 탐색 화면이 없습니다")
		}
		return failDetail(c, fiber.StatusBadGateway, "explore_failed",
			"대상 데이터베이스를 조회하지 못했습니다", err.Error())
	}

	s.audit(c, store.AuditParams{
		Action: "explore.read", TargetType: "connection", TargetID: conn.ID,
		Detail: exploreAuditDetail(conn.Name, result),
	})

	return c.JSON(fiber.Map{
		"connection": connSummary(conn),
		"level":      d.Level,
		"explore":    result,
	})
}

// exploreAuditDetail은 감사 로그에 남길 규모 정보를 만든다.
// 키 이름이나 필드 값은 남기지 않는다 — 감사 로그가 데이터 유출 경로가 되면 안 된다.
func exploreAuditDetail(name string, r *dbx.Explore) map[string]any {
	detail := map[string]any{"name": name, "shape": string(r.Shape)}
	if r.Document != nil {
		detail["collections"] = len(r.Document.Collections)
	}
	if r.Keyspace != nil {
		detail["scannedKeys"] = r.Keyspace.Scanned
		detail["groups"] = len(r.Keyspace.Groups)
	}
	return detail
}
