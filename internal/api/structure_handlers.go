package api

import (
	"errors"
	"math"
	"strconv"
	"strings"

	"github.com/gofiber/fiber/v2"

	"dbstudio/internal/erd"
	"dbstudio/internal/schema"
	"dbstudio/internal/store"
)

// 구조 화면.
//
// ERD 설계 화면과 같은 캔버스를 쓰지만 목적이 반대다. 저쪽은 "만들고 싶은 것"을
// 그리고, 이쪽은 "지금 있는 것"을 보여준다. 그래서 스키마는 편집할 수 없고,
// 편집할 수 있는 것은 읽는 사람의 정리(위치·메모·묶음)뿐이며 그것은 계정별로 남는다.
//
// 버전을 고를 수 있게 한 이유: 구조를 보는 일은 대개 "언제부터 이랬지"와 함께 온다.
// 최신만 보여주면 그 질문에 답하려고 다른 화면으로 나가야 한다.

// maxStructureNotes는 한 사람이 붙일 수 있는 메모·그룹 수의 상한이다.
// 개인 설정이지만 서버가 저장하므로, 무한히 늘어날 수 있는 값에는 상한이 있어야 한다.
const (
	maxStructureNotes  = 200
	maxStructureGroups = 100
)

// handleGetStructure는 커넥션의 구조를 ERD 형태로 반환한다.
//
// 쿼리 파라미터:
//
//	version=<id>  그 버전의 스키마 (생략하면 현재 DB를 직접 읽는다)
func (s *Server) handleGetStructure(c *fiber.Ctx) error {
	conn, adapter, err := s.resolveSchemaAccess(c, c.Params("id"))
	if err != nil {
		return err
	}

	var sc *schema.Schema
	source := fiber.Map{"kind": "live"}

	if raw := strings.TrimSpace(c.Query("version")); raw != "" {
		id, perr := strconv.ParseInt(raw, 10, 64)
		if perr != nil {
			return fail(c, fiber.StatusBadRequest, "bad_request", "버전 ID가 올바르지 않습니다")
		}
		v, verr := s.st.GetSchemaVersion(c.Context(), id, true)
		if errors.Is(verr, store.ErrNotFound) {
			return fiber.NewError(fiber.StatusNotFound, "버전을 찾을 수 없습니다")
		}
		if verr != nil {
			return verr
		}
		// 버전은 커넥션에 종속된다. 다른 커넥션의 버전을 이 경로로 읽으면
		// 권한 검사를 우회하게 되므로 소속을 확인한다.
		if v.ConnectionID != conn.ID {
			return fiber.NewError(fiber.StatusNotFound, "이 커넥션의 버전이 아닙니다")
		}
		if v.Schema == nil {
			return fail(c, fiber.StatusBadRequest, "no_schema", "이 버전에는 스키마 본문이 없습니다")
		}
		sc = v.Schema
		source = fiber.Map{
			"kind": "version", "id": v.ID, "versionNo": v.VersionNo,
			"createdAt": v.CreatedAt, "note": v.Note,
		}
	} else {
		live, ierr := s.introspectConnection(c, conn, adapter)
		if ierr != nil {
			return failDetail(c, fiber.StatusBadGateway, "introspect_failed",
				"스키마를 읽지 못했습니다", ierr.Error())
		}
		sc = live
		source["capturedAt"] = sc.CapturedAt
		source["fingerprint"] = sc.Fingerprint()
	}
	sc.Sort()

	u := currentUser(c)
	view, err := s.st.GetStructureView(c.Context(), u.ID, conn.ID)
	if err != nil {
		return err
	}

	// 저장된 배치에 없는 테이블은 자동 배치로 자리를 준다.
	//
	// 스키마는 계속 바뀌므로 새 테이블은 언제나 생긴다. 좌표가 없으면 전부 (0,0)에
	// 겹쳐 쌓이는데, 그러면 테이블 하나가 추가됐을 뿐인데 화면이 망가진 것처럼 보인다.
	placed := placeMissing(sc, view.Layout)

	return c.JSON(fiber.Map{
		"connection": connSummary(conn),
		"source":     source,
		"dialect":    sc.Dialect,
		"schema":     sc,
		"layout":     view.Layout,
		"notes":      view.Notes,
		"groups":     view.Groups,
		// 새로 자리를 잡은 테이블 수. 화면이 "N개가 새로 놓였습니다"를 알릴 수 있다.
		"placed":      placed,
		"stats":       sc.Stats(),
		"notesFromDB": sc.Notes,
	})
}

// 새 카드를 놓을 때 기존 카드와 겹쳤다고 볼 거리.
// 카드 폭(260)과 격자 간격(320×260) 사이의 값이라, 격자 위의 이웃끼리는 겹치지 않고
// 손으로 옮겨 둔 카드 위에는 놓이지 않는다.
const (
	placeClearX = 280.0
	placeClearY = 200.0
)

// placeMissing은 좌표가 없는 테이블에 빈 자리를 준다. 새로 놓인 수를 돌려준다.
//
// 같은 좌표만 피하는 것으로는 부족하다. 사람이 카드를 (11, 22) 같은 자리로 옮겨
// 두었다면 격자점 (80, 80)은 "비어 있지만" 그 카드와 겹친다. 새 테이블이 기존
// 카드 위에 얹히면 둘 다 읽을 수 없게 되므로, 겹침으로 판단한다.
func placeMissing(sc *schema.Schema, layout map[string]*erd.Box) int {
	if layout == nil {
		return 0
	}
	taken := make([][2]float64, 0, len(layout))
	for _, b := range layout {
		taken = append(taken, [2]float64{b.X, b.Y})
	}
	overlaps := func(x, y float64) bool {
		for _, p := range taken {
			if math.Abs(p[0]-x) < placeClearX && math.Abs(p[1]-y) < placeClearY {
				return true
			}
		}
		return false
	}

	placed := 0
	slot := 0
	for _, t := range sc.Tables {
		key := t.Key()
		if _, ok := layout[key]; ok {
			continue
		}
		// erd.AutoLayout과 같은 격자를 쓴다. 두 화면의 초기 배치가 같아야
		// 설계 화면과 구조 화면을 오갈 때 같은 그림으로 읽힌다.
		for {
			x, y := erd.SlotAt(slot)
			slot++
			// 상한을 두어 어떤 배치에서도 반드시 끝난다. 격자를 다 뒤졌는데도
			// 빈 자리가 없으면 겹치더라도 놓는다 — 안 보이는 것보다 낫다.
			if overlaps(x, y) && slot < 4096 {
				continue
			}
			taken = append(taken, [2]float64{x, y})
			layout[key] = &erd.Box{X: x, Y: y}
			break
		}
		placed++
	}
	return placed
}

// handleSaveStructureView는 이 사람의 배치를 저장한다.
func (s *Server) handleSaveStructureView(c *fiber.Ctx) error {
	// 모니터링 등급이면 충분하다. 저장되는 것은 스키마가 아니라 이 사람의 화면이며,
	// 다른 사람에게 보이지 않는다.
	conn, _, err := s.resolveSchemaAccess(c, c.Params("id"))
	if err != nil {
		return err
	}

	var body store.StructureView
	if err := c.BodyParser(&body); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "요청 본문을 해석할 수 없습니다")
	}
	if len(body.Notes) > maxStructureNotes {
		return fail(c, fiber.StatusBadRequest, "too_many",
			"메모는 "+strconv.Itoa(maxStructureNotes)+"개까지 붙일 수 있습니다")
	}
	if len(body.Groups) > maxStructureGroups {
		return fail(c, fiber.StatusBadRequest, "too_many",
			"그룹은 "+strconv.Itoa(maxStructureGroups)+"개까지 만들 수 있습니다")
	}
	for _, n := range body.Notes {
		if len([]rune(n.Text)) > 4000 {
			return fail(c, fiber.StatusBadRequest, "bad_request", "메모가 너무 깁니다 (4000자 제한)")
		}
	}

	u := currentUser(c)
	if err := s.st.SaveStructureView(c.Context(), u.ID, conn.ID, &body); err != nil {
		return err
	}
	// 감사 로그에 남기지 않는다. 자기 화면의 카드 위치를 옮긴 기록이 쌓이면
	// 정작 조사해야 할 항목이 그 사이에 묻힌다.
	return c.JSON(fiber.Map{"saved": true})
}
