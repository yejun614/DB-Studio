package api

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"dbstudio/internal/dbx"
	"dbstudio/internal/erd"
	"dbstudio/internal/model"
	"dbstudio/internal/schema"
	"dbstudio/internal/store"
)

// handleListERDDocuments는 접근 가능한 커넥션의 ERD 문서 목록을 반환한다.
func (s *Server) handleListERDDocuments(c *fiber.Ctx) error {
	ids, byID, err := s.accessibleConnectionIDs(c)
	if err != nil {
		return err
	}
	// 빈 슬라이스와 nil의 구분이 중요하다: 저장 계층은 nil을 "제한 없음"으로 읽는다.
	// 접근 가능한 커넥션이 없는 사용자에게 전체 문서를 보여주면 안 된다.
	if ids == nil {
		ids = []string{}
	}
	// 독립 초안에는 커넥션이 없어 위 목록으로 걸러지지 않는다. 프로젝트가 그
	// 초안들의 유일한 울타리다.
	scope, err := s.projectFilter(c)
	if err != nil {
		return err
	}
	docs, err := s.st.ListERDDocuments(c.Context(), ids, scope, 0)
	if err != nil {
		return err
	}

	u := currentUser(c)
	active := s.erdHub.ActiveCounts()
	items := make([]fiber.Map, 0, len(docs))
	for _, d := range docs {
		item := fiber.Map{
			"document":      d,
			"activeEditors": active[d.ID],
			// 목록에서 삭제·설정 버튼을 그릴지 결정한다. 눌러 보고서야 거부되는
			// 버튼은 "왜 안 되지"를 남기고, 그 답은 화면 어디에도 없다.
			"canManage": canManageERD(d.ConnectionID, d.CreatedBy, u),
		}
		if conn := byID[d.ConnectionID]; conn != nil {
			item["connection"] = connSummary(conn)
		}
		items = append(items, item)
	}
	return c.JSON(fiber.Map{"items": items})
}

// canManageERD는 문서의 삭제·설정 변경 권한을 판정한다.
//
// 커넥션이 붙은 문서는 지금까지처럼 커넥션 권한(ERD 등급)이 근거이므로 여기서는
// 항상 허용한다 — 호출부가 이미 등급을 확인한 뒤다.
//
// 독립 초안에는 그 근거가 없다. 편집은 누구나 할 수 있게 두되(설계는 함께 하는
// 일이고, 실제 DB를 건드리지 않는다), 남의 초안을 지우거나 이름을 바꾸는 것은
// 만든 사람과 어드민으로 좁힌다. 되돌릴 수 없는 동작과 함께 하는 동작을 같은
// 문턱에 두면, 협업을 열어 준 대가로 사고가 따라온다.
func canManageERD(connectionID, createdBy string, u *model.User) bool {
	if connectionID != "" {
		return true
	}
	if u == nil {
		return false
	}
	if u.Role == model.RoleSuperadmin || u.Role == model.RoleAdmin {
		return true
	}
	return createdBy != "" && createdBy == u.ID
}

// handleCreateERDDocument는 새 초안을 만든다.
//
// 두 가지 출발점을 지원한다:
//   - fromConnection=true: 대상 DB의 현재 스키마를 읽어 그 위에서 시작한다.
//     실무에서는 대부분 이쪽이다 — 이미 있는 스키마를 고치는 일이 더 많다.
//   - 그렇지 않으면 빈 캔버스에서 시작한다.
//
// 세 번째 출발점이 P16에서 더해졌다:
//   - connectionId가 비어 있으면 대상 DB 없는 **독립 초안**이다. dialect를 사람이
//     고르고, 나중에 SQL로 내보내거나 다른 초안으로 옮겨 쓴다. 설계는 DB를 만들기
//     전에 시작되는 일이 더 많고, "어디에 적용할지"는 그때 정하면 된다.
func (s *Server) handleCreateERDDocument(c *fiber.Ctx) error {
	var body struct {
		Name           string `json:"name"`
		ProjectID      string `json:"projectId"`
		ConnectionID   string `json:"connectionId"`
		Dialect        string `json:"dialect"`
		Note           string `json:"note"`
		FromConnection bool   `json:"fromConnection"`
	}
	if err := c.BodyParser(&body); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "요청 본문을 해석할 수 없습니다")
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		return fail(c, fiber.StatusBadRequest, "bad_request", "문서 이름을 입력하세요")
	}
	if len([]rune(name)) > 120 {
		return fail(c, fiber.StatusBadRequest, "bad_request", "문서 이름이 너무 깁니다 (120자 제한)")
	}

	// 독립 초안. 대상이 없으므로 dialect와 프로젝트를 직접 받는다.
	if strings.TrimSpace(body.ConnectionID) == "" {
		return s.createStandaloneERDDocument(c, name, body.ProjectID, body.Dialect, body.Note)
	}

	conn, err := s.resolveERDConnection(c, body.ConnectionID)
	if err != nil {
		return err
	}

	docID := uuid.NewString()
	var doc *erd.Document
	var sourceSnapshotID *int64

	if body.FromConnection {
		adapter, aerr := s.erdAdapterFor(conn)
		if aerr != nil {
			return aerr
		}
		sc, ierr := s.introspectConnection(c, conn, adapter)
		if ierr != nil {
			return failDetail(c, fiber.StatusBadGateway, "introspect_failed",
				"대상 DB의 스키마를 읽지 못했습니다", ierr.Error())
		}
		// 어떤 상태를 기준으로 그린 초안인지 남긴다. P7의 마이그레이션 프리체크가
		// "그 사이 DB가 바뀌었는지"를 이 기준으로 판단한다.
		snap, _, serr := s.st.SaveSchemaSnapshot(c.Context(), conn.ID, sc, store.SnapshotSourceManual, nil)
		if serr == nil && snap != nil {
			sourceSnapshotID = &snap.ID
		}
		doc = erd.FromSchema(docID, name, conn.ID, sc)
	} else {
		doc = erd.NewDocument(docID, name, conn.ID, string(conn.Kind))
	}

	u := currentUser(c)
	if err := s.st.CreateERDDocument(c.Context(), doc, u.ID, strings.TrimSpace(body.Note), sourceSnapshotID); err != nil {
		return err
	}
	s.audit(c, store.AuditParams{
		Action: "erd.create", TargetType: "erd_document", TargetID: docID,
		Detail: map[string]any{
			"name": name, "connection": conn.Name,
			"fromConnection": body.FromConnection, "tables": len(doc.Schema.Tables),
		},
	})
	return c.Status(fiber.StatusCreated).JSON(fiber.Map{
		"document":   doc,
		"connection": connSummary(conn),
	})
}

// createStandaloneERDDocument는 대상 DB 없는 초안을 만든다.
//
// 권한을 따로 묻지 않는다. 이 문서는 어떤 데이터베이스도 가리키지 않으므로
// 만들어서 할 수 있는 일이 "그림을 그리는 것"뿐이고, 그 문턱을 세우면 정작
// 설계를 시작하려는 사람이 매번 권한을 요청하게 된다.
func (s *Server) createStandaloneERDDocument(c *fiber.Ctx, name, projectID, dialect, note string) error {
	// 대상 DB가 없으므로 프로젝트가 이 초안의 유일한 울타리다. 없으면 아무에게도
	// 보이지 않는 문서가 되고, 만든 사람조차 목록에서 찾지 못한다.
	project, perr := s.requireProject(c, projectID)
	if perr != nil {
		return perr
	}
	dialect = strings.TrimSpace(dialect)
	if dialect == "" {
		return fail(c, fiber.StatusBadRequest, "bad_request",
			"대상 커넥션이 없으면 어떤 DB 문법으로 설계할지 골라야 합니다")
	}
	if !erdDialectOK(dialect) {
		return fail(c, fiber.StatusBadRequest, "invalid_dialect",
			"ERD 설계를 지원하지 않는 DB 종류입니다")
	}

	docID := uuid.NewString()
	doc := erd.NewDocument(docID, name, "", dialect)
	doc.ProjectID = project.ID
	u := currentUser(c)
	if err := s.st.CreateERDDocument(c.Context(), doc, u.ID, strings.TrimSpace(note), nil); err != nil {
		return err
	}
	s.audit(c, store.AuditParams{
		Action: "erd.create", TargetType: "erd_document", TargetID: docID,
		Detail: map[string]any{"name": name, "standalone": true, "dialect": dialect},
	})
	return c.Status(fiber.StatusCreated).JSON(fiber.Map{"document": doc})
}

// erdDialectOK는 그 DB 종류로 ERD 를 그릴 수 있는지다.
//
// 관계형이 아닌 종류는 테이블·제약이 없어 그릴 것이 없고, 마이그레이션도 만들 수
// 없다(어댑터의 Migrate 능력이 그 판정이다).
func erdDialectOK(dialect string) bool {
	kind := model.DBKind(strings.TrimSpace(dialect))
	if !kind.Valid() {
		return false
	}
	adapter, err := dbx.Get(kind)
	return err == nil && adapter.Capabilities().Migrate
}

// updateERDTarget은 문서가 향하는 대상 DB와 문법을 바꾼다.
//
// 규칙은 하나다: **대상이 있으면 문법은 그 대상의 것이다.** 두 값을 따로 받아 두면
// "PostgreSQL 커넥션을 향하는데 문법은 MySQL"인 문서가 생기고, 그 문서가 만드는
// 마이그레이션은 대상 DB가 거부한다 — 그것도 실행하는 순간에야 알게 된다.
// 문법을 직접 고르는 것은 대상이 없는 초안에서만 뜻이 있다.
func (s *Server) updateERDTarget(c *fiber.Ctx, doc *erd.Document,
	meta *store.ERDDocumentMeta, wantConn, wantDialect *string) error {
	connID := doc.ConnectionID
	if wantConn != nil {
		connID = strings.TrimSpace(*wantConn)
	}
	dialect := doc.Dialect
	projectID := meta.ProjectID

	if connID != "" {
		// 붙이려는 커넥션에 편집 등급이 있어야 한다. 볼 수만 있는 DB를 향하게
		// 해 두면, 그 문서로 만든 마이그레이션은 만들 수는 있어도 실행할 수 없다.
		conn, err := s.resolveERDConnection(c, connID)
		if err != nil {
			return err
		}
		dialect = string(conn.Kind)
		projectID = conn.ProjectID
	} else if wantDialect != nil {
		next := strings.TrimSpace(*wantDialect)
		if !erdDialectOK(next) {
			return fail(c, fiber.StatusBadRequest, "invalid_dialect",
				"ERD 설계를 지원하지 않는 DB 종류입니다")
		}
		dialect = next
	}

	if connID == doc.ConnectionID && dialect == doc.Dialect {
		return nil
	}
	if err := s.st.UpdateERDDocumentTarget(c.Context(), doc.ID, connID, dialect, projectID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fiber.NewError(fiber.StatusNotFound, "문서를 찾을 수 없습니다")
		}
		return err
	}
	s.audit(c, store.AuditParams{
		Action: "erd.retarget", TargetType: "erd_document", TargetID: doc.ID,
		Detail: map[string]any{
			"from": doc.ConnectionID, "to": connID,
			"fromDialect": doc.Dialect, "dialect": dialect,
		},
	})
	// 열려 있는 편집기들이 같은 문서를 보고 있다. 대상이 바뀐 것은 다음에 문서를
	// 받을 때 반영된다(소켓은 스키마 op 만 다룬다) — 화면 쪽에서 다시 읽는다.
	doc.ConnectionID = connID
	doc.Dialect = dialect
	return nil
}

// handleDuplicateERDDocument는 초안을 베낀다.
//
// 왜 있는가: 설계는 "지금 것을 두고 다른 안을 시험해 보는" 일이 잦다. 그런데 그때
// 쓸 수 있는 길이 SQL 내보내기 → 새 초안 → 불러오기뿐이었고, 그 길로는 배치·색·
// 아이콘·논리명·도메인·메모가 모두 사라진다 — 정작 설계를 읽게 만드는 것들이다.
//
// 권한은 **읽기**를 요구한다(LevelMonitor). 사본은 새 문서이지만 담기는 내용은
// 이미 볼 수 있는 것이고, 사본을 고칠 수 있는지는 그 사본의 대상 커넥션 등급이
// 원본과 똑같이 판정한다.
func (s *Server) handleDuplicateERDDocument(c *fiber.Ctx) error {
	doc, conn, _, err := s.resolveERDDocument(c, c.Params("docId"), model.LevelMonitor)
	if err != nil {
		return err
	}
	// 구조 문서는 커넥션마다 하나다(store.GetStructureDocumentID). 그것을 베끼면
	// 같은 대상을 가리키는 구조 문서가 둘이 되어, "지금 DB의 모습"을 어느 쪽에서
	// 봐야 하는지 답할 수 없게 된다.
	if doc.Kind == store.DocKindStructure {
		return fail(c, fiber.StatusBadRequest, "not_duplicable",
			"구조 문서는 복제할 수 없습니다. 설계 초안을 복제하세요")
	}

	var body struct {
		Name string `json:"name"`
	}
	// 본문이 없어도 된다. 이름을 안 주면 "<원본> 사본"이다.
	_ = c.BodyParser(&body)
	name := strings.TrimSpace(body.Name)
	if name == "" {
		name = doc.Name + " 사본"
	}
	if len([]rune(name)) > 120 {
		return fail(c, fiber.StatusBadRequest, "bad_request", "문서 이름이 너무 깁니다 (120자 제한)")
	}

	meta, err := s.st.GetERDDocumentMeta(c.Context(), doc.ID)
	if err != nil {
		return err
	}

	dup := doc.Clone()
	dup.ID = uuid.NewString()
	dup.Name = name
	// 사본은 언제나 초안이다. 승인된 문서를 베끼면 "승인된 사본"이 되는데, 그
	// 승인은 이 내용에 대해 아무도 한 적이 없다.
	dup.Status = erd.StatusDraft
	dup.Kind = store.DocKindDraft
	// 편집 순번은 0에서 시작한다. 원본의 순번을 물려받으면 사본의 편집 이력이
	// 비어 있는데 "편집 300회"로 보인다.
	dup.Seq = 0

	u := currentUser(c)
	// 기준 스냅샷은 물려받지 않는다(nil).
	//
	// 물려받는 편이 뜻으로는 맞다 — 사본도 같은 상태를 보고 그린 그림이다. 다만
	// 이 값은 지금 코드베이스에서 **쓰기만 하고 아무도 읽지 않는다**. 읽는 곳이
	// 생기면 그때 문서 메타에 실어 함께 옮기면 된다. 읽지 않는 값을 옮기려고
	// 저장 계층을 먼저 고치면, 무엇을 위한 필드인지 알 수 없는 코드가 남는다.
	if err := s.st.CreateERDDocument(c.Context(), dup, u.ID, meta.Note, nil); err != nil {
		return err
	}
	s.audit(c, store.AuditParams{
		Action: "erd.duplicate", TargetType: "erd_document", TargetID: dup.ID,
		Detail: map[string]any{
			"name": name, "from": doc.ID, "fromName": doc.Name,
			"tables": len(dup.Schema.Tables),
		},
	})
	var connInfo any
	if conn != nil {
		connInfo = connSummary(conn)
	}
	return c.Status(fiber.StatusCreated).JSON(fiber.Map{
		"document":   dup,
		"connection": connInfo,
	})
}

// handleGetERDDocument는 문서 전체를 반환한다.
// WebSocket을 열지 않고 읽기만 할 때(목록에서 미리보기) 사용한다.
func (s *Server) handleGetERDDocument(c *fiber.Ctx) error {
	doc, conn, _, err := s.resolveERDDocument(c, c.Params("docId"), model.LevelMonitor)
	if err != nil {
		return err
	}
	u := currentUser(c)

	// 독립 초안은 대상이 없으므로 등급을 물을 곳도 없다. 로그인한 사람이면 편집한다.
	canEdit := true
	var connInfo any
	if conn != nil {
		d, lerr := s.requireLevel(c, conn.ID, model.LevelERD)
		if lerr != nil {
			return lerr
		}
		canEdit = d.Allowed
		connInfo = connSummary(conn)
	}
	meta, err := s.st.GetERDDocumentMeta(c.Context(), doc.ID)
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{
		"document":     doc,
		"connection":   connInfo,
		"canEdit":      canEdit,
		"canManage":    canManageERD(doc.ConnectionID, meta.CreatedBy, u),
		"note":         meta.Note,
		"participants": s.erdHub.Participants(doc.ID),
	})
}

// handleUpdateERDDocument는 이름·상태·메모를 바꾼다.
// 스키마 구조는 op로만 바뀐다 — REST로도 바꿀 수 있게 하면 op-log가 실제 변경
// 이력을 온전히 담지 못하게 되고, 그 순간 협업 동기화의 근거가 무너진다.
// ---------- 문서에 매인 AI 대화 ----------

// handleListERDAISessions는 이 초안에 대한 **내** AI 대화 목록을 반환한다.
//
// 참여자들이 함께 쓰는 방 대화와 다른 것이다. 모델과의 시행착오가 방에 흘러가면
// 정작 사람끼리의 결정이 묻히므로, AI 대화는 개인의 것으로 두고 남길 만한 답만
// 사용자가 골라 방으로 공유한다.
func (s *Server) handleListERDAISessions(c *fiber.Ctx) error {
	// 목록은 문서를 볼 수 있으면 된다. 편집 등급을 요구하면 읽기 전용으로 보는
	// 사람이 대화 탭을 열 때마다 403을 받는다 — 볼 수 없는 것은 AI 세션이 아니라
	// 편집이고, 그 거절은 실제로 고치려 할 때 나와야 한다.
	doc, _, _, err := s.resolveERDDocument(c, c.Params("docId"), model.LevelMonitor)
	if err != nil {
		return err
	}
	u := currentUser(c)
	sessions, err := s.st.ListERDAISessions(c.Context(), doc.ID, u.ID)
	if err != nil {
		return err
	}
	providers, err := s.st.ListAIProviders(c.Context())
	if err != nil {
		return err
	}
	enabled := []*store.AIProvider{}
	for _, p := range providers {
		if p.Enabled && p.HasKey {
			enabled = append(enabled, p)
		}
	}
	tools, _ := erdAITools()
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		names = append(names, t.Name)
	}
	return c.JSON(fiber.Map{
		"sessions": sessions, "providers": enabled, "tools": names,
	})
}

func (s *Server) handleCreateERDAISession(c *fiber.Ctx) error {
	doc, _, _, err := s.resolveERDDocument(c, c.Params("docId"), model.LevelERD)
	if err != nil {
		return err
	}
	var body struct {
		Title      string `json:"title"`
		ProviderID string `json:"providerId"`
		Model      string `json:"model"`
	}
	_ = c.BodyParser(&body)

	if err := s.checkSessionModel(c.Context(),
		strings.TrimSpace(body.ProviderID), strings.TrimSpace(body.Model)); err != nil {
		return fail(c, fiber.StatusBadRequest, "model_not_allowed", err.Error())
	}
	title := strings.TrimSpace(body.Title)
	if title == "" {
		title = "새 대화"
	}
	u := currentUser(c)
	sess, err := s.st.CreateAISession(c.Context(), store.CreateAISessionParams{
		UserID: u.ID, Title: title,
		ProviderID:    strings.TrimSpace(body.ProviderID),
		Model:         strings.TrimSpace(body.Model),
		ERDDocumentID: doc.ID,
	})
	if err != nil {
		return err
	}
	s.audit(c, store.AuditParams{
		Action: "erd.ai.session", TargetType: "erd_document", TargetID: doc.ID,
		Detail: map[string]any{"sessionId": sess.ID, "title": title},
	})
	return c.Status(fiber.StatusCreated).JSON(fiber.Map{"session": sess})
}

func (s *Server) handleUpdateERDDocument(c *fiber.Ctx) error {
	doc, conn, _, err := s.resolveERDDocument(c, c.Params("docId"), model.LevelERD)
	if err != nil {
		return err
	}
	var body struct {
		Name   *string `json:"name"`
		Status *string `json:"status"`
		Note   *string `json:"note"`
		// 대상 DB와 문법. 빈 문자열은 "대상 없음"이라는 뜻이라 포인터로 받는다 —
		// 값을 보내지 않은 것과 비워 달라는 것은 다른 요청이다.
		ConnectionID *string `json:"connectionId"`
		Dialect      *string `json:"dialect"`
	}
	if err := c.BodyParser(&body); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "요청 본문을 해석할 수 없습니다")
	}

	meta, err := s.requireERDManage(c, doc)
	if err != nil {
		return err
	}
	name, status, note := meta.Name, meta.Status, meta.Note
	if body.Name != nil {
		name = strings.TrimSpace(*body.Name)
		if name == "" {
			return fail(c, fiber.StatusBadRequest, "bad_request", "문서 이름을 입력하세요")
		}
	}
	if body.Status != nil {
		status = strings.TrimSpace(*body.Status)
		if !validERDStatus(status) {
			return fail(c, fiber.StatusBadRequest, "bad_request",
				"문서 상태는 draft, in_review, applied, archived 중 하나여야 합니다")
		}
	}
	if body.Note != nil {
		note = strings.TrimSpace(*body.Note)
	}

	// 대상 DB·문법을 함께 바꿀 수 있다.
	//
	// 왜 필요한가: 대상 없이 시작한 초안이 실제 DB를 갖게 되는 것이 흔한 흐름이다
	// (그림을 먼저 그리고 나중에 붙인다). 그때까지는 SQL 내보내기만 되고
	// 마이그레이션은 만들 수 없었는데, 그 길이 문서를 새로 만드는 것뿐이었다.
	if body.ConnectionID != nil || body.Dialect != nil {
		if err := s.updateERDTarget(c, doc, meta, body.ConnectionID, body.Dialect); err != nil {
			return err
		}
	}

	if err := s.st.UpdateERDDocumentMeta(c.Context(), doc.ID, name, status, note); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fiber.NewError(fiber.StatusNotFound, "문서를 찾을 수 없습니다")
		}
		return err
	}
	s.audit(c, store.AuditParams{
		Action: "erd.update", TargetType: "erd_document", TargetID: doc.ID,
		Detail: map[string]any{"name": name, "status": status, "connection": connName(conn)},
	})
	// 편집 중인 사람들에게 알린다. 문서 이름이 바뀐 것을 모르면 다른 문서를 보고
	// 있다고 착각한다.
	s.erdHub.Broadcast(doc.ID, map[string]any{
		"type": "meta", "name": name, "status": status, "note": note,
	})
	return c.JSON(fiber.Map{"document": fiber.Map{
		"id": doc.ID, "name": name, "status": status, "note": note,
	}})
}

func (s *Server) handleDeleteERDDocument(c *fiber.Ctx) error {
	doc, conn, _, err := s.resolveERDDocument(c, c.Params("docId"), model.LevelERD)
	if err != nil {
		return err
	}
	if _, err := s.requireERDManage(c, doc); err != nil {
		return err
	}
	// 구조 문서는 커넥션에 딸린 것이지 사람이 만든 초안이 아니다. 지우면 그 DB를
	// 보는 모두의 메모와 묶음이 함께 사라지고, 다음에 화면을 열면 빈 문서가 새로
	// 만들어져 "사라졌다"는 사실조차 남지 않는다.
	if doc.Kind == store.DocKindStructure {
		return fail(c, fiber.StatusBadRequest, "structure_document",
			"구조 문서는 지울 수 없습니다. 구조 화면의 메모·묶음은 그 화면에서 지웁니다")
	}
	if err := s.st.DeleteERDDocument(c.Context(), doc.ID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fiber.NewError(fiber.StatusNotFound, "문서를 찾을 수 없습니다")
		}
		return err
	}
	// 방을 먼저 닫지 않으면 남아 있는 편집자의 op가 계속 저장 실패한다.
	s.erdHub.CloseDocument(doc.ID, "문서가 삭제되었습니다")
	s.audit(c, store.AuditParams{
		Action: "erd.delete", TargetType: "erd_document", TargetID: doc.ID,
		Detail: map[string]any{"name": doc.Name, "connection": connName(conn)},
	})
	return c.JSON(fiber.Map{"deleted": true})
}

// connName은 감사 로그에 적을 커넥션 이름이다.
// 독립 초안에는 커넥션이 없으므로 빈칸 대신 그 사실을 적는다 — 로그를 읽는 사람이
// "이름이 왜 비었지"를 다시 조사하게 만들지 않는다.
func connName(conn *model.Connection) string {
	if conn == nil {
		return "(대상 없음)"
	}
	return conn.Name
}

// handleERDOps는 문서의 편집 이력을 반환한다.
// 누가 무엇을 언제 바꿨는지 추적하고, P7이 이름 변경 의도를 읽는 근거다.
func (s *Server) handleERDOps(c *fiber.Ctx) error {
	doc, _, _, err := s.resolveERDDocument(c, c.Params("docId"), model.LevelMonitor)
	if err != nil {
		return err
	}
	after := int64(c.QueryInt("after", 0))
	ops, err := s.st.ListERDOps(c.Context(), doc.ID, after)
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"ops": ops, "seq": doc.Seq})
}

// handleERDChat는 채팅 이력을 반환한다. 실시간 수신은 WebSocket이 담당하고,
// 이 엔드포인트는 화면 진입 전 미리보기와 이력 조회용이다.
func (s *Server) handleERDChat(c *fiber.Ctx) error {
	doc, _, _, err := s.resolveERDDocument(c, c.Params("docId"), model.LevelMonitor)
	if err != nil {
		return err
	}
	msgs, err := s.st.ListERDChatMessages(c.Context(), doc.ID, c.QueryInt("limit", 200))
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"messages": msgs})
}

// handleERDDiff는 초안과 대상 DB의 현재 스키마를 비교한다.
//
// ERD 화면에서 "지금 이 초안을 적용하면 무엇이 바뀌는가"를 보여준다. P7의
// 마이그레이션 계획이 같은 diff를 쓰므로, 여기서 보이는 것과 실제 적용 결과가 같다.
func (s *Server) handleERDDiff(c *fiber.Ctx) error {
	doc, conn, _, err := s.resolveERDDocument(c, c.Params("docId"), model.LevelMonitor)
	if err != nil {
		return err
	}
	if err := requireERDConnection(conn, "현재 DB와 비교"); err != nil {
		return err
	}
	adapter, err := s.erdAdapterFor(conn)
	if err != nil {
		return err
	}
	current, ierr := s.introspectConnection(c, conn, adapter)
	if ierr != nil {
		return failDetail(c, fiber.StatusBadGateway, "introspect_failed",
			"대상 DB의 스키마를 읽지 못했습니다", ierr.Error())
	}

	// 방향: 현재 DB → 초안. 즉 "초안대로 만들려면 무엇을 해야 하는가"다.
	// P7의 마이그레이션 계획이 이 diff를 그대로 쓰므로 화면과 실행 결과가 일치한다.
	diff := schema.Diff(current, doc.Schema)
	plan := schema.BuildPlan(current.Dialect, diff)

	s.audit(c, store.AuditParams{
		Action: "erd.diff", TargetType: "erd_document", TargetID: doc.ID,
		Detail: map[string]any{
			"name": doc.Name, "connection": conn.Name,
			"changes": len(diff.Changes), "destructive": diff.DestructiveCount,
		},
	})

	return c.JSON(fiber.Map{
		"document":   fiber.Map{"id": doc.ID, "name": doc.Name, "seq": doc.Seq},
		"connection": connSummary(conn),
		"current": fiber.Map{
			"dialect": current.Dialect, "stats": current.Stats(),
			"fingerprint": current.Fingerprint(), "notes": current.Notes,
		},
		"draft": fiber.Map{"dialect": doc.Schema.Dialect, "stats": doc.Schema.Stats()},
		"diff":  diff,
		"plan":  plan,
	})
}

// handleERDDDL은 초안 전체를 만드는 CREATE 스크립트를 생성한다.
//
// 대상 DB와의 diff(handleERDDiff)와 다르다. 저쪽은 "지금 DB를 이 초안처럼 만들려면
// 무엇을 해야 하는가"이고, 이쪽은 "이 초안대로 처음부터 만들려면 무엇이 필요한가"다.
// 그래서 빈 스키마를 출발점으로 삼는다 — 커넥션이 없는 초안에도 답할 수 있는
// 유일한 형태이며, 실제로 그때 가장 필요한 결과물이다.
func (s *Server) handleERDDDL(c *fiber.Ctx) error {
	doc, conn, _, err := s.resolveERDDocument(c, c.Params("docId"), model.LevelMonitor)
	if err != nil {
		return err
	}

	// 다른 종류로 옮겨 심는 경우를 위해 대상 방언을 바꿀 수 있게 한다.
	// 타입 변환이 개입하므로 경고를 함께 돌려준다.
	dialect := strings.TrimSpace(c.Query("dialect"))
	if dialect == "" {
		dialect = doc.Schema.Dialect
	}
	if dialect == "" {
		dialect = doc.Dialect
	}
	if !model.DBKind(dialect).Valid() {
		return fail(c, fiber.StatusBadRequest, "invalid_dialect", "알 수 없는 DB 종류입니다")
	}

	// 기준을 고를 수 있다. 비워 두면 빈 스키마에서 출발하는 "처음부터 만드는"
	// 스크립트이고(대상이 없는 초안에서 유일하게 얻을 수 있는 결과물이다),
	// 지금 DB나 특정 버전을 고르면 거기서 이 설계로 가는 변경 SQL이 나온다.
	baseSpec := strings.TrimSpace(c.Query("base"))
	from := &schema.Schema{Dialect: dialect, Shape: schema.ShapeRelational}
	baseLabel := "빈 스키마"
	if baseSpec != "" {
		// 대상 DB가 없는 초안에는 기준으로 삼을 것이 없다. erdAdapterFor는 커넥션이
		// 있다고 보고 conn.Kind 를 읽으므로, 여기서 먼저 막지 않으면 nil 로 터진다.
		if conn == nil {
			return fail(c, fiber.StatusBadRequest, "no_connection",
				"대상 DB가 없는 문서는 처음부터 만드는 스크립트만 뽑을 수 있습니다")
		}
		adapter, aerr := s.erdAdapterFor(conn)
		if aerr != nil {
			return aerr
		}
		base, berr := s.resolveBase(c, conn, adapter, baseSpec)
		if berr != nil {
			return berr
		}
		from = base.Schema
		baseLabel = base.Label
	}
	diff := schema.Diff(from, doc.Schema)
	plan := schema.BuildPlan(dialect, diff)
	// 처음부터 만드는 스크립트에만 CREATE DATABASE 를 붙인다. 기준이 실제 DB 나
	// 특정 버전이면 그 데이터베이스는 이미 있다.
	if baseSpec == "" {
		prependTargetDatabase(plan, doc)
	}
	if dialect != doc.Schema.Dialect && doc.Schema.Dialect != "" {
		plan.Warnings = append(plan.Warnings,
			doc.Schema.Dialect+" → "+dialect+" 로 타입을 변환했습니다. 실행 전 검토가 필요합니다")
	}

	s.audit(c, store.AuditParams{
		Action: "erd.export", TargetType: "erd_document", TargetID: doc.ID,
		Detail: map[string]any{
			"name": doc.Name, "connection": connName(conn),
			"dialect": dialect, "statements": len(plan.Up), "base": baseLabel,
		},
	})

	return c.JSON(fiber.Map{
		"document":  fiber.Map{"id": doc.ID, "name": doc.Name, "dialect": doc.Schema.Dialect},
		"dialect":   dialect,
		"plan":      plan,
		"upSql":     plan.UpSQL(),
		"downSql":   plan.DownSQL(),
		"base":      baseLabel,
		"changes":   len(diff.Changes),
		"stats":     doc.Schema.Stats(),
		"fromEmpty": baseSpec == "",
	})
}

// dryRunTimeout은 미리 실행해 보기 전체에 주는 시간이다. 그림자 DB를 만들고,
// 기준 구조를 세우고, 계획을 돌리는 데까지가 그 안이다.
//
// 기준 구조가 크면 seed가 오래 걸린다. 그래도 상한은 있어야 한다 — 검사 하나가
// 대상 서버의 연결을 하염없이 붙들고 있으면 그것이 새 사고가 된다.
const (
	dryRunTimeout          = 3 * time.Minute
	dryRunStatementTimeout = 60 * time.Second
)

// sqlList는 계획 문장에서 SQL만 뽑는다.
func sqlList(stmts []schema.Statement) []string {
	out := make([]string, 0, len(stmts))
	for _, s := range stmts {
		out = append(out, s.SQL)
	}
	return out
}

// handleERDDryRun은 계획을 만들기 전에 SQL이 실제로 실행되는지 확인한다.
//
// 계획을 만들고 나서야 SQL이 깨진 것을 아는 흐름은 비싸다: 만들고, 리뷰를 받고,
// 실행하고, 실패를 보고, 초안을 고치고, 다시 처음부터 — 한 바퀴가 사람 여럿의
// 시간이다. "이 문장이 이 DB에서 실행되는가"는 계획을 만들기 전에 물어볼 수 있다.
//
// 대상 DB는 손대지 않는다. 그림자 DB를 새로 만들어 기준 구조를 세우고, 계획을
// 실행해 본 뒤 통째로 지운다(dbx.DryRunDDL).
func (s *Server) handleERDDryRun(c *fiber.Ctx) error {
	// 그림자 DB를 만드는 일은 서버에 쓰는 동작이다. 읽기 권한만으로 열어 주지 않는다.
	doc, conn, _, err := s.resolveERDDocument(c, c.Params("docId"), model.LevelMigrate)
	if err != nil {
		return err
	}
	if err := requireERDConnection(conn, "미리 실행"); err != nil {
		return err
	}
	var body struct {
		Base    string `json:"base"`
		Dialect string `json:"dialect"`
	}
	if err := c.BodyParser(&body); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "요청 본문을 해석할 수 없습니다")
	}

	dialect := strings.TrimSpace(body.Dialect)
	if dialect == "" {
		dialect = string(conn.Kind)
	}
	// 다른 종류의 SQL은 이 서버에서 실행해 볼 수 없다. 검사하지 못하는 것과 계획이
	// 틀린 것은 사람이 할 일이 다르므로, 여기서 분명히 갈라 말한다.
	if dialect != string(conn.Kind) {
		return c.JSON(fiber.Map{"dryRun": &dbx.DryRunReport{
			FailedIndex: -1,
			Skipped: fmt.Sprintf("%s 로 만든 SQL은 %s 서버에서 미리 실행해 볼 수 없습니다",
				dialect, conn.Kind),
		}})
	}

	adapter, err := s.erdAdapterFor(conn)
	if err != nil {
		return err
	}
	// 기준이 비어 있으면 "처음부터 만드는" 스크립트다. 그때 그림자 DB는 빈 채로
	// 두고 계획만 실행한다.
	from := &schema.Schema{Dialect: dialect, Shape: schema.ShapeRelational}
	baseLabel := "빈 스키마"
	if strings.TrimSpace(body.Base) != "" {
		base, berr := s.resolveBase(c, conn, adapter, body.Base)
		if berr != nil {
			return berr
		}
		from = base.Schema
		baseLabel = base.Label
	}

	plan := schema.BuildPlan(dialect, schema.Diff(from, doc.Schema))
	if len(plan.Up) == 0 {
		return c.JSON(fiber.Map{"dryRun": &dbx.DryRunReport{
			OK: true, FailedIndex: -1, Steps: []dbx.ExecStep{},
		}, "base": baseLabel, "statements": 0})
	}

	secret, err := s.st.GetSecret(c.Context(), conn.ID)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(c.Context(), dryRunTimeout)
	defer cancel()

	report, err := dbx.DryRunDDL(ctx, adapter, dbx.Target{Conn: conn, Secret: secret},
		seedStatements(dialect, from, doc.Schema), sqlList(plan.Up),
		dbx.ExecOptions{StatementTimeout: dryRunStatementTimeout})
	if err != nil {
		return failDetail(c, fiber.StatusBadGateway, "dryrun_failed",
			"미리 실행해 보지 못했습니다", err.Error())
	}

	s.audit(c, store.AuditParams{
		Action: "erd.dryrun", TargetType: "erd_document", TargetID: doc.ID,
		Result: dryRunResult(report),
		Detail: map[string]any{
			"name": doc.Name, "connection": connName(conn), "dialect": dialect,
			"base": baseLabel, "statements": len(plan.Up),
			"ok": report.OK, "skipped": report.Skipped,
		},
	})
	return c.JSON(fiber.Map{
		"dryRun": report, "base": baseLabel, "statements": len(plan.Up),
		"warnings": plan.Warnings,
	})
}

// dryRunResult는 감사 로그의 결과 칸을 정한다.
//
// 감사 로그는 ok·denied·error 셋만 안다. "검사하지 못함"은 그중 무엇도 아니지만
// denied가 가장 가깝다 — 하려다 못 한 것이지 계획이 틀린 것이 아니다. 진짜 사유는
// detail의 skipped에 그대로 남는다.
func dryRunResult(r *dbx.DryRunReport) string {
	switch {
	case r.Skipped != "":
		return "denied"
	case !r.OK:
		return "error"
	}
	return "ok"
}

// seedStatements는 그림자 DB에 기준 구조를 세우는 문장이다.
//
// 스키마(네임스페이스)를 먼저 만든다. PostgreSQL·MS-SQL의 계획은 테이블 이름에
// 스키마를 붙여 쓰는데, 갓 만든 그림자 DB에는 public 말고는 아무것도 없다 — 그것
// 없이 seed를 돌리면 "스키마가 없다"는 엉뚱한 실패로 검사가 끝난다.
//
// 목표 스키마의 네임스페이스도 함께 만든다. 계획이 새 스키마에 테이블을 만드는
// 경우가 있고, 그 CREATE SCHEMA는 계획에 들어 있지 않다.
func seedStatements(dialect string, from, target *schema.Schema) []string {
	out := []string{}
	for _, ns := range namespacesOf(from, target) {
		switch dialect {
		case string(model.KindPostgres):
			out = append(out, `CREATE SCHEMA IF NOT EXISTS "`+ns+`"`)
		case string(model.KindMSSQL):
			out = append(out, fmt.Sprintf(
				"IF SCHEMA_ID('%s') IS NULL EXEC('CREATE SCHEMA [%s]')", ns, ns))
		}
	}
	out = append(out, sqlList(schema.BuildPlan(dialect,
		schema.Diff(&schema.Schema{Dialect: dialect, Shape: schema.ShapeRelational}, from)).Up)...)
	return out
}

// namespacesOf는 두 스키마에 나오는 네임스페이스를 순서대로 모은다.
func namespacesOf(schemas ...*schema.Schema) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, sc := range schemas {
		if sc == nil {
			continue
		}
		for _, t := range sc.Tables {
			ns := strings.TrimSpace(t.Namespace)
			if ns == "" || seen[ns] {
				continue
			}
			seen[ns] = true
			out = append(out, ns)
		}
	}
	return out
}

// ---------- 공용 헬퍼 ----------

func validERDStatus(status string) bool {
	switch status {
	case erd.StatusDraft, erd.StatusInReview, erd.StatusApplied, erd.StatusArchived:
		return true
	}
	return false
}

// resolveERDConnection은 문서를 만들 대상 커넥션을 확인한다.
// ERD 등급 이상이 필요하다 — 설계 초안을 만드는 것은 조회가 아니다.
func (s *Server) resolveERDConnection(c *fiber.Ctx, connID string) (*model.Connection, error) {
	connID = strings.TrimSpace(connID)
	if connID == "" {
		return nil, fiber.NewError(fiber.StatusBadRequest, "대상 커넥션을 선택하세요")
	}
	conn, err := s.st.GetConnection(c.Context(), connID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, fiber.NewError(fiber.StatusNotFound, "커넥션을 찾을 수 없습니다")
	}
	if err != nil {
		return nil, err
	}
	d, err := s.requireLevel(c, connID, model.LevelERD)
	if err != nil {
		return nil, err
	}
	if !d.Allowed {
		return nil, fiber.NewError(fiber.StatusForbidden, d.Reason)
	}
	return conn, nil
}

// erdAdapterFor는 ERD 설계가 가능한 DB인지 확인한다.
// MongoDB/Redis는 스키마 개념이 없어 ERD·마이그레이션 대상이 아니다.
func (s *Server) erdAdapterFor(conn *model.Connection) (dbx.Adapter, error) {
	adapter, err := dbx.Get(conn.Kind)
	if err != nil {
		return nil, fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	if !adapter.Capabilities().Migrate {
		return nil, fiber.NewError(fiber.StatusBadRequest,
			"이 데이터베이스 종류는 ERD 설계와 마이그레이션을 지원하지 않습니다")
	}
	return adapter, nil
}

// resolveERDDocument는 문서를 읽고 대상 커넥션에 대한 권한을 확인한다.
//
// 문서 자체에는 권한이 없다. 권한은 대상 커넥션에 붙어 있으므로, 문서를 통해
// 우회 접근하는 경로가 생기지 않도록 항상 커넥션 기준으로 판정한다.
//
// 독립 초안(커넥션 없음)에는 그 근거가 없다. 반환되는 커넥션이 nil이며,
// 호출부는 커넥션이 있어야만 되는 일(diff·마이그레이션)을 그 자리에서 막아야 한다.
// 여기서 대신 막지 않는 이유: 무엇이 필요한지는 부르는 쪽이 알고, 여기서 뭉뚱그리면
// "왜 안 되는지"를 설명할 수 없는 오류가 된다.
func (s *Server) resolveERDDocument(c *fiber.Ctx, docID string, need model.Level) (*erd.Document, *model.Connection, bool, error) {
	docID = strings.TrimSpace(docID)
	if docID == "" {
		return nil, nil, false, fiber.NewError(fiber.StatusBadRequest, "문서 ID가 없습니다")
	}
	doc, err := s.st.GetERDDocument(c.Context(), docID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil, false, fiber.NewError(fiber.StatusNotFound, "문서를 찾을 수 없습니다")
	}
	if err != nil {
		return nil, nil, false, err
	}
	if doc.ConnectionID == "" {
		// 독립 초안은 대상이 없어 등급을 물을 곳이 없다. 프로젝트 참여가 그
		// 자리를 대신한다 — 아무나 볼 수 있게 두면 프로젝트를 나눈 의미가 없다.
		ok, perr := s.canSeeProject(c, doc.ProjectID)
		if perr != nil {
			return nil, nil, false, perr
		}
		if !ok {
			return nil, nil, false, fiber.NewError(fiber.StatusNotFound, "문서를 찾을 수 없습니다")
		}
		return doc, nil, true, nil
	}
	conn, err := s.st.GetConnection(c.Context(), doc.ConnectionID)
	if errors.Is(err, store.ErrNotFound) {
		// erd_documents.connection_id는 ON DELETE CASCADE이므로 정상적으로는 없는 상황이다.
		return nil, nil, false, fiber.NewError(fiber.StatusNotFound, "문서의 대상 커넥션이 없습니다")
	}
	if err != nil {
		return nil, nil, false, err
	}
	d, err := s.requireLevel(c, conn.ID, need)
	if err != nil {
		return nil, nil, false, err
	}
	if !d.Allowed {
		return nil, nil, false, fiber.NewError(fiber.StatusForbidden, d.Reason)
	}
	return doc, conn, d.Allowed, nil
}

// requireERDConnection은 대상 DB가 있어야만 되는 기능을 위한 확인이다.
func requireERDConnection(conn *model.Connection, what string) error {
	if conn != nil {
		return nil
	}
	return fiber.NewError(fiber.StatusBadRequest,
		"이 초안에는 대상 데이터베이스가 없어 "+what+"할 수 없습니다. "+
			"SQL 내보내기로 스크립트를 받거나, 대상이 있는 초안에서 진행하세요")
}

// requireERDManage는 삭제·설정 변경 권한을 확인하고 메타데이터를 함께 돌려준다.
func (s *Server) requireERDManage(c *fiber.Ctx, doc *erd.Document) (*store.ERDDocumentMeta, error) {
	meta, err := s.st.GetERDDocumentMeta(c.Context(), doc.ID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, fiber.NewError(fiber.StatusNotFound, "문서를 찾을 수 없습니다")
	}
	if err != nil {
		return nil, err
	}
	if !canManageERD(doc.ConnectionID, meta.CreatedBy, currentUser(c)) {
		return nil, fiber.NewError(fiber.StatusForbidden,
			"이 초안을 만든 사람 또는 어드민만 설정을 바꾸거나 삭제할 수 있습니다")
	}
	return meta, nil
}

// handleERDTypeCatalog는 "이 DB에서 고를 수 있는 타입"을 돌려준다.
//
// 목록을 서버가 쥐고 있는 이유는 catalog.go의 주석과 같다: 화면이 따로 들고 있으면
// DB 종류가 늘 때마다 두 곳을 고쳐야 하고, 어긋나면 화면에서 고른 타입을 서버가
// 모르는 상태가 된다.
func (s *Server) handleERDTypeCatalog(c *fiber.Ctx) error {
	dialect := strings.TrimSpace(c.Query("dialect"))
	if dialect == "" {
		return fail(c, fiber.StatusBadRequest, "missing_dialect", "dialect 를 지정해야 합니다")
	}
	if !model.DBKind(dialect).Valid() {
		return fail(c, fiber.StatusBadRequest, "invalid_dialect", "알 수 없는 DB 종류입니다")
	}
	return c.JSON(schema.TypeCatalog(dialect))
}
