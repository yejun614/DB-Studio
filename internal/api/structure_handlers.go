package api

import (
	"errors"
	"math"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"dbstudio/internal/erd"
	"dbstudio/internal/model"
	"dbstudio/internal/schema"
	"dbstudio/internal/store"
)

// 구조 화면.
//
// ERD 설계 화면과 같은 캔버스를 쓰지만 목적이 반대다. 저쪽은 "만들고 싶은 것"을
// 그리고, 이쪽은 "지금 있는 것"을 보여준다. 그래서 스키마는 편집할 수 없고,
// 편집할 수 있는 것은 사람이 얹은 정리(위치·메모·묶음)뿐이다.
//
// 그 정리는 커넥션마다 하나 있는 **구조 문서**(erd_documents.kind='structure')에 있고,
// 같은 DB를 보는 사람들이 함께 고친다. 그래서 ERD 편집기가 쓰던 것이 그대로 붙는다:
// 실시간 방·커서·채팅·되돌리기, 그리고 클러스터에서의 소켓 중계까지.
// (0022는 이것을 계정별로 두었다. 바꾼 이유는 0032에 적혀 있다.)
//
// 버전을 고를 수 있게 한 이유: 구조를 보는 일은 대개 "언제부터 이랬지"와 함께 온다.
// 최신만 보여주면 그 질문에 답하려고 다른 화면으로 나가야 한다.

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

	// 버전 읽기와 소속 확인은 schema_handlers.go 의 versionForQuery 한 곳에 있다.
	// SQL 생성 경로도 같은 함수를 쓰므로 확인이 한쪽에서만 빠지는 일이 없다.
	v, verr := s.versionForQuery(c, conn)
	if verr != nil {
		return verr
	}
	if v != nil {
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

	doc, placed, err := s.structureDocument(c, conn, sc, v == nil)
	if err != nil {
		return err
	}

	// 편집 등급을 함께 알려 준다. 화면이 도구를 보여줄지 정하는 근거이고,
	// 소켓도 같은 기준으로 판정하므로 둘이 어긋나지 않는다.
	canEdit := false
	if d, lerr := s.requireLevel(c, conn.ID, model.LevelERD); lerr == nil {
		canEdit = d.Allowed
	}

	return c.JSON(fiber.Map{
		"connection": connSummary(conn),
		"source":     source,
		"dialect":    sc.Dialect,
		"schema":     sc,
		"layout":     doc.Layout,
		"notes":      doc.Notes,
		"groups":     doc.Groups,
		// documentId는 실시간 방의 열쇠다. 방은 **DB 기준**이라 과거 시점을 보는
		// 중에도 같은 방에 있다 — 누가 접속해 있는지, 무엇을 이야기하는지는 보고
		// 있는 시점과 무관한 사실이다. 커서만 같은 시점을 보는 사람끼리 보인다.
		"documentId": doc.ID,
		// 편집은 현재 시점에서만 한다. 과거 화면의 좌표로 카드를 옮기면 지금을 보는
		// 사람의 화면이 이유 없이 흔들린다.
		"canEdit": canEdit && v == nil,
		// 새로 자리를 잡은 테이블 수. 화면이 "N개가 새로 놓였습니다"를 알릴 수 있다.
		"placed":      placed,
		"stats":       sc.Stats(),
		"notesFromDB": sc.Notes,
	})
}

// structureDocument는 이 커넥션의 구조 문서를 열고, 스키마 레이어를 지금 것으로 맞춘다.
//
// 문서를 처음 만들 때 그 사람의 옛 개인 정리(0022의 erd_structure_views)를 씨앗으로
// 옮긴다. 공유로 바뀌었다고 그동안 정리한 것이 사라지면, 사람은 바뀐 것이 아니라
// 잃은 것으로 받아들인다.
func (s *Server) structureDocument(c *fiber.Ctx, conn *model.Connection, sc *schema.Schema, live bool) (*erd.Document, int, error) {
	ctx := c.Context()
	docID, err := s.st.GetStructureDocumentID(ctx, conn.ID)
	if errors.Is(err, store.ErrNotFound) {
		docID, err = s.createStructureDocument(c, conn, sc)
	}
	if err != nil {
		return nil, 0, err
	}
	doc, err := s.st.GetERDDocument(ctx, docID)
	if err != nil {
		return nil, 0, err
	}

	// 과거 버전을 보는 중이면 문서의 정리만 얹고 스키마는 건드리지 않는다.
	// 그 시점 구조를 지금 것으로 갈아 끼우면 보러 온 것이 사라진다.
	layout := map[string]*erd.Box{}
	for k, b := range doc.Layout {
		layout[k] = b
	}
	placed := placeMissing(sc, layout)
	if !live {
		doc.Layout = layout
		return doc, placed, nil
	}

	// 실제 DB가 문서의 스냅샷과 다르면 갈아 끼운다. 지문으로 비교하는 이유:
	// 테이블을 옮기기만 해도 달라지는 값이 아니라 구조만 반영하는 값이다.
	if doc.Schema == nil || doc.Schema.Fingerprint() != sc.Fingerprint() || placed > 0 {
		if err := s.erdHub.RefreshSchema(ctx, docID, sc, layout); err != nil {
			return nil, 0, err
		}
		doc, err = s.st.GetERDDocument(ctx, docID)
		if err != nil {
			return nil, 0, err
		}
	}
	return doc, placed, nil
}

// createStructureDocument는 커넥션의 구조 문서를 만든다.
func (s *Server) createStructureDocument(c *fiber.Ctx, conn *model.Connection, sc *schema.Schema) (string, error) {
	ctx := c.Context()
	u := currentUser(c)
	seed, err := s.st.GetStructureView(ctx, u.ID, conn.ID)
	if err != nil {
		return "", err
	}
	layout := seed.Layout
	if layout == nil {
		layout = map[string]*erd.Box{}
	}
	placeMissing(sc, layout)

	doc := &erd.Document{
		ID:           uuid.NewString(),
		Name:         conn.Name + " 구조",
		ConnectionID: conn.ID,
		Dialect:      string(conn.Kind),
		Status:       "applied",
		Kind:         store.DocKindStructure,
		Schema:       sc,
		Layout:       layout,
		Notes:        seed.Notes,
		Groups:       seed.Groups,
	}
	if err := s.st.CreateERDDocument(ctx, doc, u.ID, "구조 화면의 공유 정리", nil); err != nil {
		return "", err
	}
	s.audit(c, store.AuditParams{
		Action: "structure.document.created", TargetType: "erd_document", TargetID: doc.ID,
		Detail: map[string]any{"connection": conn.Name, "seededNotes": len(seed.Notes)},
	})
	return doc.ID, nil
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
