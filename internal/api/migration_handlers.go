package api

import (
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/gofiber/fiber/v2"

	"dbstudio/internal/migrate"
	"dbstudio/internal/model"
	"dbstudio/internal/schema"
	"dbstudio/internal/store"
)

// ---------- 스키마 버전 ----------

// handleListVersions는 커넥션의 버전 이력을 반환한다.
func (s *Server) handleListVersions(c *fiber.Ctx) error {
	conn, _, err := s.resolveSchemaAccess(c, c.Params("id"))
	if err != nil {
		return err
	}
	versions, err := s.st.ListSchemaVersions(c.Context(), conn.ID, c.QueryInt("limit", 100))
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{
		"connection": connSummary(conn),
		"versions":   versions,
	})
}

// handleGetVersion은 버전 하나의 스키마 전체를 반환한다.
func (s *Server) handleGetVersion(c *fiber.Ctx) error {
	conn, _, err := s.resolveSchemaAccess(c, c.Params("id"))
	if err != nil {
		return err
	}
	id, perr := strconv.ParseInt(c.Params("versionId"), 10, 64)
	if perr != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "버전 ID가 올바르지 않습니다")
	}
	v, err := s.st.GetSchemaVersion(c.Context(), id, true)
	if errors.Is(err, store.ErrNotFound) {
		return fiber.NewError(fiber.StatusNotFound, "버전을 찾을 수 없습니다")
	}
	if err != nil {
		return err
	}
	// 버전은 커넥션에 종속된다. 다른 커넥션의 버전을 이 경로로 읽으면
	// 권한 검사를 우회하게 되므로 소속을 확인한다.
	if v.ConnectionID != conn.ID {
		return fiber.NewError(fiber.StatusNotFound, "이 커넥션의 버전이 아닙니다")
	}
	return c.JSON(fiber.Map{"connection": connSummary(conn), "version": v})
}

// handleCaptureVersion은 현재 스키마를 버전으로 확정한다.
//
// 두 상황에 쓴다:
//   - 최초 기준선 등록 (버전이 없는 커넥션)
//   - 외부 편집으로 바뀐 상태를 이력에 남기기 (드리프트 이벤트의 후속 조치)
//
// 어느 쪽인지는 서버가 판단한다 — 사용자가 잘못 고를 여지를 없앤다.
func (s *Server) handleCaptureVersion(c *fiber.Ctx) error {
	conn, adapter, err := s.resolveSchemaAccess(c, c.Params("id"))
	if err != nil {
		return err
	}
	// 버전 확정은 이력을 만드는 행위이므로 ERD 등급 이상을 요구한다.
	d, err := s.requireLevel(c, conn.ID, model.LevelERD)
	if err != nil {
		return err
	}
	if !d.Allowed {
		return fiber.NewError(fiber.StatusForbidden, d.Reason)
	}

	var body struct {
		Note string `json:"note"`
	}
	_ = c.BodyParser(&body)

	current, ierr := s.introspectConnection(c, conn, adapter)
	if ierr != nil {
		return failDetail(c, fiber.StatusBadGateway, "introspect_failed",
			"현재 스키마를 읽지 못했습니다", ierr.Error())
	}

	prev, err := s.st.LatestSchemaVersion(c.Context(), conn.ID, true)
	if err != nil {
		return err
	}
	source := store.VersionSourceImport
	summary := []string{}
	if prev != nil {
		source = store.VersionSourceExternal
		if prev.Schema != nil {
			// 이전 버전과의 차이를 요약으로 남긴다. "외부에서 무엇이 바뀌었나"가
			// 이 기능의 핵심 정보다.
			diff := schema.Diff(prev.Schema, current)
			for _, ch := range diff.Changes {
				summary = append(summary, ch.Summary)
			}
			if len(summary) == 0 {
				return c.JSON(fiber.Map{
					"created": false,
					"version": prev,
					"message": "이전 버전과 구조가 같아 새 버전을 만들지 않았습니다",
				})
			}
		}
	}

	u := currentUser(c)
	version, created, err := s.st.SaveSchemaVersion(c.Context(), store.SaveVersionParams{
		ConnectionID: conn.ID, Schema: current, Source: source,
		Note: strings.TrimSpace(body.Note), ChangeSummary: summary,
		AuthorID: u.ID, AuthorName: displayName(u),
	})
	if err != nil {
		return err
	}
	// 외부 편집을 이력에 남겼으면 그것을 알리던 이벤트는 그 순간 끝난다.
	//
	// 사람이 따로 닫아 주기를 기다리면, 이미 처리한 일이 계속 "열린 문제"로 남아
	// 대시보드의 심각 건수를 부풀리고, 같은 버튼을 한 번 더 누르게 만든다
	// (두 번째 등록은 바뀐 것이 없어 아무 일도 하지 않지만 사용자는 그것을 모른다).
	resolved := int64(0)
	if created && source == store.VersionSourceExternal {
		n, rerr := s.st.ResolveDriftEvents(c.Context(), conn.ID)
		if rerr != nil {
			// 버전은 이미 저장됐다. 이벤트 정리 실패로 전체를 실패로 만들지 않는다.
			slog.Error("드리프트 이벤트 해소 실패", "connection", conn.Name, "err", rerr)
		}
		resolved = n
	}

	s.audit(c, store.AuditParams{
		Action: "version.capture", TargetType: "connection", TargetID: conn.ID,
		Detail: map[string]any{
			"name": conn.Name, "source": source, "versionNo": version.VersionNo,
			"changes": len(summary), "created": created, "resolvedEvents": resolved,
		},
	})
	return c.JSON(fiber.Map{
		"created": created, "version": version, "changes": summary,
		"resolvedEvents": resolved,
	})
}

// handleVersionDiff는 두 버전을 비교한다. to를 생략하면 현재 DB와 비교한다.
func (s *Server) handleVersionDiff(c *fiber.Ctx) error {
	conn, adapter, err := s.resolveSchemaAccess(c, c.Params("id"))
	if err != nil {
		return err
	}
	fromID, perr := strconv.ParseInt(c.Query("from"), 10, 64)
	if perr != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "비교 시작 버전을 지정하세요")
	}
	from, err := s.st.GetSchemaVersion(c.Context(), fromID, true)
	if errors.Is(err, store.ErrNotFound) {
		return fiber.NewError(fiber.StatusNotFound, "시작 버전을 찾을 수 없습니다")
	}
	if err != nil {
		return err
	}
	if from.ConnectionID != conn.ID {
		return fiber.NewError(fiber.StatusNotFound, "이 커넥션의 버전이 아닙니다")
	}

	var toSchema *schema.Schema
	toLabel := "현재 DB"
	if raw := strings.TrimSpace(c.Query("to")); raw != "" {
		toID, perr := strconv.ParseInt(raw, 10, 64)
		if perr != nil {
			return fail(c, fiber.StatusBadRequest, "bad_request", "비교 대상 버전이 올바르지 않습니다")
		}
		to, err := s.st.GetSchemaVersion(c.Context(), toID, true)
		if errors.Is(err, store.ErrNotFound) {
			return fiber.NewError(fiber.StatusNotFound, "비교 대상 버전을 찾을 수 없습니다")
		}
		if err != nil {
			return err
		}
		if to.ConnectionID != conn.ID {
			return fiber.NewError(fiber.StatusNotFound, "이 커넥션의 버전이 아닙니다")
		}
		toSchema = to.Schema
		toLabel = "버전 " + strconv.Itoa(to.VersionNo)
	} else {
		current, ierr := s.introspectConnection(c, conn, adapter)
		if ierr != nil {
			return failDetail(c, fiber.StatusBadGateway, "introspect_failed",
				"현재 스키마를 읽지 못했습니다", ierr.Error())
		}
		toSchema = current
	}

	diff := schema.Diff(from.Schema, toSchema)
	plan := schema.BuildPlan(string(conn.Kind), diff)
	return c.JSON(fiber.Map{
		"connection": connSummary(conn),
		"from":       fiber.Map{"versionNo": from.VersionNo, "label": "버전 " + strconv.Itoa(from.VersionNo)},
		"to":         fiber.Map{"label": toLabel},
		"diff":       diff,
		"plan":       plan,
	})
}

// ---------- 마이그레이션 ----------

// handleListMigrations는 접근 가능한 커넥션의 마이그레이션 목록을 반환한다.
func (s *Server) handleListMigrations(c *fiber.Ctx) error {
	ids, byID, err := s.accessibleConnectionIDs(c)
	if err != nil {
		return err
	}
	if ids == nil {
		ids = []string{}
	}
	status := strings.TrimSpace(c.Query("status"))
	if status != "" && !validMigrationStatus(status) {
		return fail(c, fiber.StatusBadRequest, "bad_request", "알 수 없는 상태입니다")
	}
	migs, err := s.st.ListMigrations(c.Context(), ids, status, c.QueryInt("limit", 100))
	if err != nil {
		return err
	}
	items := make([]fiber.Map, 0, len(migs))
	for _, m := range migs {
		item := fiber.Map{"migration": m}
		if conn := byID[m.ConnectionID]; conn != nil {
			item["connection"] = connSummary(conn)
			item["requiredApprovals"] = migrate.RequiredApprovals(conn, m.DestructiveCount)
		}
		items = append(items, item)
	}
	return c.JSON(fiber.Map{"items": items})
}

// handleCreateMigration은 ERD 초안에서 마이그레이션 계획을 만든다.
func (s *Server) handleCreateMigration(c *fiber.Ctx) error {
	var body struct {
		DocID string `json:"docId"`
		Title string `json:"title"`
	}
	if err := c.BodyParser(&body); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "요청 본문을 해석할 수 없습니다")
	}
	// 마이그레이션 생성에는 migrate 등급이 필요하다. 계획을 만드는 것 자체는
	// 실행이 아니지만, 실행 대상을 정의하는 행위이므로 ERD 설계와 구분한다.
	doc, conn, _, err := s.resolveERDDocument(c, body.DocID, model.LevelMigrate)
	if err != nil {
		return err
	}
	// 구조 문서는 실제 DB의 사본이라 자기 자신과의 차이가 언제나 없다. 후보 목록에서
	// 이미 빼 두었지만, 문서 id를 직접 보내는 길이 남아 있으므로 여기서도 막는다.
	if doc.Kind == store.DocKindStructure {
		return fail(c, fiber.StatusBadRequest, "structure_document",
			"구조 문서는 지금 DB의 모습이라 마이그레이션을 만들 수 없습니다. "+
				"고칠 것이 있으면 초안을 만들어 그 위에서 설계하세요")
	}
	if err := requireERDConnection(conn, "마이그레이션을 생성"); err != nil {
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

	diff := schema.Diff(current, doc.Schema)
	if diff.IsEmpty() {
		return fail(c, fiber.StatusBadRequest, "no_changes",
			"초안과 대상 DB의 구조가 같아 만들 마이그레이션이 없습니다")
	}
	plan := schema.BuildPlan(string(conn.Kind), diff)
	if len(plan.Up) == 0 {
		return failDetail(c, fiber.StatusBadRequest, "no_statements",
			"변경은 있지만 실행할 SQL을 만들 수 없습니다",
			strings.Join(plan.Warnings, " / "))
	}

	// 계획을 만드는 시점에는 버전을 만들지 않는다(롤백 계획과 같은 규칙).
	//
	// 이력의 한 줄은 "DB 구조가 이렇게 되어 있었다"는 사실이어야 하는데, 계획은 아직
	// 아무것도 바꾸지 않았다. 반려되거나 잊힌 계획이 남긴 줄은 아무 일도 가리키지
	// 못한다. 버전은 실행이 끝난 뒤 결과로 등록된다(runner).
	//
	// 기준 버전은 지금 구조가 그 버전과 같을 때만 채운다. 어긋난 상태에서 최신 버전을
	// 적어 두면 이력에 없는 상태를 있는 것처럼 말하게 된다. "무엇으로부터의 변경인가"는
	// base_fingerprint와 diff에 남고, 실행 직전 사전 검사도 그 지문을 본다.
	u := currentUser(c)
	latest, err := s.st.LatestSchemaVersion(c.Context(), conn.ID, false)
	if err != nil {
		return err
	}
	var fromID *int64
	if latest != nil && latest.Fingerprint == current.Fingerprint() {
		fromID = &latest.ID
	}

	title := strings.TrimSpace(body.Title)
	if title == "" {
		title = doc.Name
	}
	mig, err := s.st.CreateMigration(c.Context(), store.CreateMigrationParams{
		ConnectionID: conn.ID, DocID: doc.ID, Title: title,
		FromVersion: fromID, BaseFinger: current.Fingerprint(),
		TargetSchema: doc.Schema, Plan: plan, Diff: diff, CreatedBy: u.ID,
	})
	if err != nil {
		return err
	}
	s.audit(c, store.AuditParams{
		Action: "migration.create", TargetType: "migration", TargetID: mig.ID,
		Detail: map[string]any{
			"connection": conn.Name, "title": title, "document": doc.Name,
			"changes": len(diff.Changes), "destructive": diff.DestructiveCount,
			"statements": len(plan.Up),
		},
	})
	return c.Status(fiber.StatusCreated).JSON(fiber.Map{
		"migration":         mig,
		"connection":        connSummary(conn),
		"requiredApprovals": migrate.RequiredApprovals(conn, mig.DestructiveCount),
	})
}

// 버전 롤백.
//
// "v3으로 되돌린다"는 결국 **현재 구조에서 v3 구조로 가는 마이그레이션**이다.
// 그래서 전용 실행 경로를 만들지 않고 기존 워크플로(리뷰 → 승인 → 프리체크 → 실행)를
// 그대로 탄다. 전용 경로를 만들면 승인 없이 구조를 바꾸는 길이 하나 더 생기고,
// 그것이 이 앱이 막으려는 바로 그 상황이다.
//
// GET은 미리보기(무엇이 바뀌는지·어떤 SQL이 실행되는지), POST는 계획 생성이다.
// 되돌리기는 대개 급한 상황에서 눌리므로, 누르기 전에 무엇이 일어날지 보여줘야 한다.
func (s *Server) rollbackTarget(c *fiber.Ctx) (*model.Connection, *store.SchemaVersion, *schema.Schema, error) {
	conn, adapter, err := s.resolveSchemaAccess(c, c.Params("id"))
	if err != nil {
		return nil, nil, nil, err
	}
	// 계획을 만드는 것 자체는 실행이 아니지만 실행 대상을 정의하는 행위다.
	// ERD에서 마이그레이션을 만들 때와 같은 등급을 요구한다.
	d, err := s.requireLevel(c, conn.ID, model.LevelMigrate)
	if err != nil {
		return nil, nil, nil, err
	}
	if !d.Allowed {
		return nil, nil, nil, fiber.NewError(fiber.StatusForbidden, d.Reason)
	}

	id, perr := strconv.ParseInt(c.Params("versionId"), 10, 64)
	if perr != nil {
		return nil, nil, nil, fiber.NewError(fiber.StatusBadRequest, "버전 ID가 올바르지 않습니다")
	}
	target, err := s.st.GetSchemaVersion(c.Context(), id, true)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil, nil, fiber.NewError(fiber.StatusNotFound, "버전을 찾을 수 없습니다")
	}
	if err != nil {
		return nil, nil, nil, err
	}
	if target.ConnectionID != conn.ID {
		return nil, nil, nil, fiber.NewError(fiber.StatusNotFound, "이 커넥션의 버전이 아닙니다")
	}
	if target.Schema == nil {
		return nil, nil, nil, fiber.NewError(fiber.StatusBadRequest, "이 버전에는 스키마 본문이 없습니다")
	}

	current, ierr := s.introspectConnection(c, conn, adapter)
	if ierr != nil {
		return nil, nil, nil, failDetail(c, fiber.StatusBadGateway, "introspect_failed",
			"대상 DB의 현재 스키마를 읽지 못했습니다", ierr.Error())
	}
	return conn, target, current, nil
}

// handleVersionRollbackPreview는 되돌렸을 때 무엇이 바뀌는지 보여준다.
func (s *Server) handleVersionRollbackPreview(c *fiber.Ctx) error {
	conn, target, current, err := s.rollbackTarget(c)
	if err != nil {
		return err
	}

	diff := schema.Diff(current, target.Schema)
	plan := schema.BuildPlan(string(conn.Kind), diff)

	return c.JSON(fiber.Map{
		"connection": connSummary(conn),
		"version":    fiber.Map{"id": target.ID, "versionNo": target.VersionNo, "createdAt": target.CreatedAt},
		"diff":       diff,
		"plan":       plan,
		// 되돌릴 것이 없으면 화면이 버튼을 감춘다. 계획을 만들어 봐야 빈 마이그레이션이다.
		"empty": diff.IsEmpty(),
	})
}

// handleVersionRollback은 되돌리기 마이그레이션 계획을 만든다.
func (s *Server) handleVersionRollback(c *fiber.Ctx) error {
	conn, target, current, err := s.rollbackTarget(c)
	if err != nil {
		return err
	}
	u := currentUser(c)

	diff := schema.Diff(current, target.Schema)
	if diff.IsEmpty() {
		return fail(c, fiber.StatusBadRequest, "no_changes",
			"현재 구조가 이미 이 버전과 같습니다")
	}
	plan := schema.BuildPlan(string(conn.Kind), diff)
	if len(plan.Up) == 0 {
		return failDetail(c, fiber.StatusBadRequest, "no_statements",
			"되돌릴 변경은 있지만 실행할 SQL을 만들 수 없습니다",
			strings.Join(plan.Warnings, " / "))
	}

	// 계획을 만드는 시점에는 버전을 만들지 않는다.
	//
	// 이력의 한 줄은 "DB 구조가 이렇게 되어 있었다"는 사실이어야 하는데, 계획은
	// 아직 아무것도 바꾸지 않았다. 승인되지 않거나 반려되면 그 줄은 영영 뜻을 갖지
	// 못한 채 남는다. 순서는 계획 → 리뷰 → 승인 → 실행이고, 버전은 실행이 끝난 뒤
	// 결과로 등록된다(runner).
	//
	// 그러면 "무엇으로부터 되돌렸는가"는 어디에 남는가: 마이그레이션 자체다.
	// base_fingerprint 는 계획 시점의 실제 구조를 가리키고, diff 에는 되돌릴 변경이
	// 그대로 들어 있다. 실행 직전 사전 검사가 그 지문과 실제 DB를 대조하므로,
	// 기준선 버전이 없다고 해서 안전장치가 약해지지는 않는다.
	latest, err := s.st.LatestSchemaVersion(c.Context(), conn.ID, false)
	if err != nil {
		return err
	}
	// 기준 버전은 "지금 구조가 그 버전과 같을 때"만 채운다. 외부 편집으로 어긋난
	// 상태에서 최신 버전을 적어 두면, 이력에 없는 상태를 있는 것처럼 말하게 된다.
	var fromID *int64
	fromNo := 0
	if latest != nil && latest.Fingerprint == current.Fingerprint() {
		fromID = &latest.ID
		fromNo = latest.VersionNo
	}

	var body struct {
		Note string `json:"note"`
	}
	_ = c.BodyParser(&body)
	title := fmt.Sprintf("v%d 으로 롤백", target.VersionNo)
	if note := strings.TrimSpace(body.Note); note != "" {
		title += " — " + note
	}

	mig, err := s.st.CreateMigration(c.Context(), store.CreateMigrationParams{
		ConnectionID: conn.ID, Title: title,
		FromVersion: fromID, RollbackTo: &target.ID,
		BaseFinger:   current.Fingerprint(),
		TargetSchema: target.Schema, Plan: plan, Diff: diff, CreatedBy: u.ID,
	})
	if err != nil {
		return err
	}

	s.audit(c, store.AuditParams{
		Action: "version.rollback.plan", TargetType: "migration", TargetID: mig.ID,
		Detail: map[string]any{
			"connection": conn.Name, "toVersion": target.VersionNo,
			"fromVersion": fromNo, "changes": len(diff.Changes),
			"destructive": diff.DestructiveCount, "statements": len(plan.Up),
		},
	})
	return c.Status(fiber.StatusCreated).JSON(fiber.Map{
		"migration": mig,
		"message": fmt.Sprintf("v%d 으로 되돌리는 계획을 만들었습니다. 검토·승인 후 실행하세요.",
			target.VersionNo),
	})
}

// handleGetMigration은 마이그레이션 상세를 반환한다.
func (s *Server) handleGetMigration(c *fiber.Ctx) error {
	mig, conn, err := s.resolveMigration(c, c.Params("migId"), model.LevelMonitor)
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{
		"migration":         mig,
		"connection":        connSummary(conn),
		"requiredApprovals": migrate.RequiredApprovals(conn, mig.DestructiveCount),
		"approvals":         store.ApprovalCount(mig.Reviews),
		"backupConfigured":  s.migrator.BackupConfigured(),
	})
}

// handleMigrationStatus는 상태를 전이한다 (리뷰 요청, 초안으로 되돌리기).
func (s *Server) handleMigrationStatus(c *fiber.Ctx) error {
	mig, conn, err := s.resolveMigration(c, c.Params("migId"), model.LevelMigrate)
	if err != nil {
		return err
	}
	var body struct {
		Status string `json:"status"`
	}
	if err := c.BodyParser(&body); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "요청 본문을 해석할 수 없습니다")
	}
	next := strings.TrimSpace(body.Status)
	// 실행/롤백은 전용 엔드포인트에서만 일어난다. 상태만 바꿔서 "적용됨"으로
	// 만들 수 있으면 실행 기록 없는 적용이 생긴다.
	switch next {
	case store.MigrationInReview, store.MigrationDraft, store.MigrationClosed:
	default:
		return fail(c, fiber.StatusBadRequest, "bad_request",
			"이 엔드포인트로는 리뷰 요청(in_review)·초안(draft)·닫기(closed)로만 바꿀 수 있습니다")
	}

	if err := s.st.SetMigrationStatus(c.Context(), mig.ID, next); err != nil {
		var ite *store.InvalidTransitionError
		if errors.As(err, &ite) {
			return fail(c, fiber.StatusConflict, "invalid_transition", ite.Error())
		}
		return err
	}
	s.audit(c, store.AuditParams{
		Action: "migration.status", TargetType: "migration", TargetID: mig.ID,
		Detail: map[string]any{"connection": conn.Name, "from": mig.Status, "to": next},
	})
	return c.JSON(fiber.Map{"status": next})
}

// handleReviewMigration은 승인/반려/의견을 기록한다.
func (s *Server) handleReviewMigration(c *fiber.Ctx) error {
	mig, conn, err := s.resolveMigration(c, c.Params("migId"), model.LevelMigrate)
	if err != nil {
		return err
	}
	var body struct {
		Decision string `json:"decision"`
		Comment  string `json:"comment"`
	}
	if err := c.BodyParser(&body); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "요청 본문을 해석할 수 없습니다")
	}
	decision := strings.TrimSpace(body.Decision)
	switch decision {
	case store.ReviewApproved, store.ReviewRejected, store.ReviewComment:
	default:
		return fail(c, fiber.StatusBadRequest, "bad_request",
			"결정은 approved, rejected, comment 중 하나여야 합니다")
	}
	if mig.Status != store.MigrationInReview && decision != store.ReviewComment {
		return fail(c, fiber.StatusConflict, "invalid_state",
			"리뷰 중인 마이그레이션만 승인/반려할 수 있습니다")
	}

	u := currentUser(c)
	// 승인·반려는 리뷰어로 지정된 사람만 남긴다.
	//
	// 의견은 누구나 남길 수 있게 두는 이유: 지나가다 본 사람이 "이 인덱스는 운영
	// 시간에 걸면 안 됩니다"라고 적는 길까지 막으면, 그 말은 앱 밖으로 나가고
	// 계획 옆에 남지 않는다. 반면 승인은 책임이 따르는 결정이므로, 부탁받은
	// 사람의 것이어야 "누구에게 물어봤고 누가 답했는가"가 이력에서 이어진다.
	// 승인·반려는 리뷰어로 지정된 사람의 일이다. 의견은 누구나 남길 수 있다.
	//
	// 슈퍼 어드민은 예외다. 지정한 리뷰어가 휴가를 갔거나 계정이 잠긴 상황에서
	// 계획이 영원히 멈춰 있으면, 사람들은 이 흐름을 우회하는 다른 길(콘솔에서 직접
	// 실행)을 찾는다. 막다른 길을 만들지 않는 것이 규칙을 지키게 하는 방법이다.
	// 누가 결정했는지는 리뷰 기록과 감사 로그에 그대로 남는다.
	if decision != store.ReviewComment && !isDesignatedReviewer(mig, u.ID) &&
		u.Role != model.RoleSuperadmin {
		return fail(c, fiber.StatusForbidden, "not_reviewer",
			"리뷰어로 지정된 사람만 승인·반려할 수 있습니다. 의견은 누구나 남길 수 있습니다")
	}
	// 자기가 만든 계획을 자기가 승인하는 것은 검토가 아니다.
	// 승인 2명이 필요한 경우(운영·파괴적 변경)에 이 규칙이 실질적인 안전장치가 된다.
	if decision == store.ReviewApproved && mig.CreatedBy == u.ID &&
		migrate.RequiredApprovals(conn, mig.DestructiveCount) > 1 {
		return fail(c, fiber.StatusForbidden, "self_approval",
			"본인이 만든 계획은 승인할 수 없습니다. 다른 검토자의 승인이 필요합니다")
	}

	review := &store.MigrationReview{
		MigrationID: mig.ID, ReviewerID: u.ID, ReviewerName: displayName(u),
		Decision: decision, Comment: strings.TrimSpace(body.Comment),
	}
	if err := s.st.AddMigrationReview(c.Context(), review); err != nil {
		return err
	}

	// 결정에 따라 상태를 자동 전이한다. 사용자가 "승인"과 "상태 변경"을 따로
	// 눌러야 하면 승인만 하고 잊는 경우가 생긴다.
	reviews, err := s.st.ListMigrationReviews(c.Context(), mig.ID)
	if err != nil {
		return err
	}
	required := migrate.RequiredApprovals(conn, mig.DestructiveCount)
	approvals := store.ApprovalCount(reviews)
	nextStatus := mig.Status
	switch {
	case store.HasRejection(reviews):
		nextStatus = store.MigrationRejected
	case approvals >= required && mig.Status == store.MigrationInReview:
		nextStatus = store.MigrationApproved
	}
	if nextStatus != mig.Status {
		if err := s.st.SetMigrationStatus(c.Context(), mig.ID, nextStatus); err != nil {
			var ite *store.InvalidTransitionError
			if !errors.As(err, &ite) {
				return err
			}
		}
	}

	s.audit(c, store.AuditParams{
		Action: "migration.review", TargetType: "migration", TargetID: mig.ID,
		Detail: map[string]any{
			"connection": conn.Name, "decision": decision,
			"approvals": approvals, "required": required, "status": nextStatus,
		},
	})
	return c.JSON(fiber.Map{
		"review": review, "approvals": approvals,
		"requiredApprovals": required, "status": nextStatus,
	})
}

// isDesignatedReviewer는 그 사람이 이 마이그레이션의 리뷰어로 지정되어 있는지 본다.
func isDesignatedReviewer(mig *store.Migration, userID string) bool {
	for _, r := range mig.Reviewers {
		if r.UserID == userID {
			return true
		}
	}
	return false
}

// handlePrecheckMigration은 실행 전 검사만 수행한다.
// 실행 버튼을 누르기 전에 무엇이 막고 있는지 보여주기 위한 것이다.
func (s *Server) handlePrecheckMigration(c *fiber.Ctx) error {
	mig, conn, err := s.resolveMigration(c, c.Params("migId"), model.LevelMigrate)
	if err != nil {
		return err
	}
	secret, err := s.st.GetSecret(c.Context(), conn.ID)
	if err != nil {
		return err
	}
	pc, err := s.migrator.Check(c.Context(), conn, secret, mig)
	if err != nil {
		return failDetail(c, fiber.StatusBadGateway, "precheck_failed",
			"사전 검사를 수행하지 못했습니다", err.Error())
	}
	return c.JSON(fiber.Map{"precheck": pc, "connection": connSummary(conn)})
}

// handleApplyMigration은 마이그레이션을 실행한다.
func (s *Server) handleApplyMigration(c *fiber.Ctx) error {
	mig, conn, err := s.resolveMigration(c, c.Params("migId"), model.LevelMigrate)
	if err != nil {
		return err
	}
	var body struct {
		Confirm    string `json:"confirm"`
		SkipBackup bool   `json:"skipBackup"`
	}
	_ = c.BodyParser(&body)

	// 운영 DB와 파괴적 변경은 확인 문구를 요구한다. 버튼 하나로 되돌릴 수 없는
	// 일이 일어나면 안 된다.
	if need := confirmPhrase(conn, mig); need != "" {
		if strings.TrimSpace(body.Confirm) != need {
			return failDetail(c, fiber.StatusBadRequest, "confirm_required",
				"실행을 확인하려면 확인 문구를 정확히 입력하세요", need)
		}
	}

	secret, err := s.st.GetSecret(c.Context(), conn.ID)
	if err != nil {
		return err
	}
	u := currentUser(c)
	res, err := s.migrator.Apply(c.Context(), migrate.ApplyParams{
		Conn: conn, Secret: secret, Mig: mig, Actor: u, SkipBackup: body.SkipBackup,
	})
	if err != nil {
		var blocked *migrate.BlockedError
		if errors.As(err, &blocked) {
			s.audit(c, store.AuditParams{
				Action: "migration.apply", TargetType: "migration", TargetID: mig.ID,
				Result: "denied",
				Detail: map[string]any{"connection": conn.Name, "blockers": blocked.Blockers},
			})
			return c.Status(fiber.StatusConflict).JSON(fiber.Map{
				"error": "blocked", "message": "실행 조건을 만족하지 않습니다",
				"blockers": blocked.Blockers,
			})
		}
		s.audit(c, store.AuditParams{
			Action: "migration.apply", TargetType: "migration", TargetID: mig.ID,
			Result: "error",
			Detail: map[string]any{"connection": conn.Name, "error": err.Error()},
		})
		return failDetail(c, fiber.StatusBadGateway, "apply_failed",
			"마이그레이션을 실행하지 못했습니다", err.Error())
	}

	result := "ok"
	if res.Status != store.MigrationApplied {
		result = "error"
	}
	s.audit(c, store.AuditParams{
		Action: "migration.apply", TargetType: "migration", TargetID: mig.ID,
		Result: result,
		Detail: map[string]any{
			"connection": conn.Name, "title": mig.Title, "status": res.Status,
			"applied": res.Report.Applied, "statements": len(mig.Plan.Up),
			"error": res.Error, "postDiff": len(res.PostDiff),
		},
	})
	// 이 커넥션의 스키마가 바뀌었으므로 드리프트 기준선을 새로 잡아둔다.
	// 그러지 않으면 우리가 방금 한 변경이 "외부 편집"으로 감지된다.
	if res.Status == store.MigrationApplied {
		s.rebaselineDrift(c, conn.ID)
	}
	return c.JSON(fiber.Map{"result": res, "connection": connSummary(conn)})
}

// handleRollbackMigration은 down SQL을 실행해 되돌린다.
func (s *Server) handleRollbackMigration(c *fiber.Ctx) error {
	mig, conn, err := s.resolveMigration(c, c.Params("migId"), model.LevelMigrate)
	if err != nil {
		return err
	}
	var body struct {
		Confirm         string `json:"confirm"`
		ContinueOnError bool   `json:"continueOnError"`
	}
	_ = c.BodyParser(&body)

	if conn.Environment == model.EnvProd && strings.TrimSpace(body.Confirm) != conn.Name {
		return failDetail(c, fiber.StatusBadRequest, "confirm_required",
			"운영 DB 롤백은 커넥션 이름을 입력해 확인해야 합니다", conn.Name)
	}

	secret, err := s.st.GetSecret(c.Context(), conn.ID)
	if err != nil {
		return err
	}
	u := currentUser(c)
	res, err := s.migrator.Rollback(c.Context(), migrate.ApplyParams{
		Conn: conn, Secret: secret, Mig: mig, Actor: u,
	}, body.ContinueOnError)
	if err != nil {
		var blocked *migrate.BlockedError
		if errors.As(err, &blocked) {
			return c.Status(fiber.StatusConflict).JSON(fiber.Map{
				"error": "blocked", "message": "롤백 조건을 만족하지 않습니다",
				"blockers": blocked.Blockers,
			})
		}
		return failDetail(c, fiber.StatusBadGateway, "rollback_failed",
			"롤백을 실행하지 못했습니다", err.Error())
	}
	s.audit(c, store.AuditParams{
		Action: "migration.rollback", TargetType: "migration", TargetID: mig.ID,
		Detail: map[string]any{
			"connection": conn.Name, "title": mig.Title, "status": res.Status,
			"applied": res.Report.Applied, "continueOnError": body.ContinueOnError,
		},
	})
	if res.Status == store.MigrationRolledBack {
		s.rebaselineDrift(c, conn.ID)
	}
	return c.JSON(fiber.Map{"result": res, "connection": connSummary(conn)})
}

func (s *Server) handleDeleteMigration(c *fiber.Ctx) error {
	mig, conn, err := s.resolveMigration(c, c.Params("migId"), model.LevelMigrate)
	if err != nil {
		return err
	}
	// 실행된 마이그레이션은 이력이므로 지우지 않는다. 지울 수 있으면
	// "누가 언제 무엇을 실행했는가"의 기록이 사라진다.
	if mig.Status == store.MigrationApplied || mig.Status == store.MigrationRolledBack {
		return fail(c, fiber.StatusConflict, "immutable",
			"실행된 마이그레이션은 삭제할 수 없습니다 (실행 이력이 사라집니다)")
	}
	if err := s.st.DeleteMigration(c.Context(), mig.ID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fiber.NewError(fiber.StatusNotFound, "마이그레이션을 찾을 수 없습니다")
		}
		return err
	}
	s.audit(c, store.AuditParams{
		Action: "migration.delete", TargetType: "migration", TargetID: mig.ID,
		Detail: map[string]any{"connection": conn.Name, "title": mig.Title},
	})
	return c.JSON(fiber.Map{"deleted": true})
}

// ---------- 공용 ----------

func validMigrationStatus(status string) bool {
	switch status {
	case store.MigrationDraft, store.MigrationInReview, store.MigrationApproved,
		store.MigrationRejected, store.MigrationApplied, store.MigrationRolledBack,
		store.MigrationFailed, store.MigrationClosed:
		return true
	}
	return false
}

// confirmPhrase는 실행 확인에 요구할 문구를 정한다. 빈 문자열이면 확인이 필요 없다.
func confirmPhrase(conn *model.Connection, mig *store.Migration) string {
	if conn.Environment == model.EnvProd || mig.DestructiveCount > 0 {
		return conn.Name
	}
	return ""
}

// resolveMigration은 마이그레이션을 읽고 대상 커넥션 권한을 확인한다.
func (s *Server) resolveMigration(c *fiber.Ctx, migID string, need model.Level) (*store.Migration, *model.Connection, error) {
	migID = strings.TrimSpace(migID)
	if migID == "" {
		return nil, nil, fiber.NewError(fiber.StatusBadRequest, "마이그레이션 ID가 없습니다")
	}
	mig, err := s.st.GetMigration(c.Context(), migID, true)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil, fiber.NewError(fiber.StatusNotFound, "마이그레이션을 찾을 수 없습니다")
	}
	if err != nil {
		return nil, nil, err
	}
	conn, err := s.st.GetConnection(c.Context(), mig.ConnectionID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil, fiber.NewError(fiber.StatusNotFound, "대상 커넥션이 없습니다")
	}
	if err != nil {
		return nil, nil, err
	}
	d, err := s.requireLevel(c, conn.ID, need)
	if err != nil {
		return nil, nil, err
	}
	if !d.Allowed {
		return nil, nil, fiber.NewError(fiber.StatusForbidden, d.Reason)
	}
	return mig, conn, nil
}

// rebaselineDrift는 스키마 스냅샷 기준선을 현재 상태로 갱신한다.
//
// 우리가 방금 실행한 변경이 곧바로 "외부 편집" 드리프트로 감지되면 알림이 무의미해진다.
// 실패해도 마이그레이션 결과를 뒤집을 이유는 없으므로 감사 로그만 남긴다.
func (s *Server) rebaselineDrift(c *fiber.Ctx, connID string) {
	if s.monitor == nil {
		return
	}
	if _, _, err := s.monitor.CheckDriftByID(c.Context(), connID); err != nil {
		s.audit(c, store.AuditParams{
			Action: "migration.rebaseline", TargetType: "connection", TargetID: connID,
			Result: "error", Detail: map[string]any{"error": err.Error()},
		})
	}
}

func displayName(u *model.User) string {
	if u == nil {
		return ""
	}
	if strings.TrimSpace(u.DisplayName) != "" {
		return u.DisplayName
	}
	return u.Username
}

// ---------- 담당자 · 리뷰어 ----------

// handleListMigrationPeople은 담당자·리뷰어로 지정할 수 있는 사람을 돌려준다.
//
// 아무나 담지 않는다. 마이그레이션을 만지려면 대상 커넥션에 migrate 등급이 필요하고,
// 그 등급이 없는 사람을 지정하면 그 사람은 열어 보지도 못한다 — 고를 수는 있는데
// 누르면 403이 나는 목록은 권한 설정이 잘못된 것처럼 보인다.
func (s *Server) handleListMigrationPeople(c *fiber.Ctx) error {
	_, conn, err := s.resolveMigration(c, c.Params("migId"), model.LevelMigrate)
	if err != nil {
		return err
	}
	people, err := s.st.ListActivePeople(c.Context())
	if err != nil {
		return err
	}

	out := []store.MigrationPerson{}
	for _, p := range people {
		u, err := s.st.GetUser(c.Context(), p.ID)
		if err != nil {
			// 목록을 만드는 중에 사용자가 지워질 수 있다. 그 한 명 때문에
			// 대화상자 전체가 열리지 않는 것이 더 나쁘다.
			continue
		}
		d, err := s.authz.ResolveScope(c.Context(), u, conn.Scope())
		if err != nil {
			return err
		}
		if !d.Allowed || !d.Level.Includes(model.LevelMigrate) {
			continue
		}
		p.Level = string(d.Level)
		out = append(out, p)
	}
	return c.JSON(fiber.Map{"items": out})
}

// handleSetMigrationAssignment는 담당자와 리뷰어를 정한다.
//
// 상태를 가리지 않는다(초안이든 적용 완료든 바꿀 수 있다). 실행이 끝난 뒤에도
// "누가 맡았던 일인가"는 이력으로 남을 값이고, 실행 중 담당자가 바뀌는 일도 있다.
func (s *Server) handleSetMigrationAssignment(c *fiber.Ctx) error {
	mig, conn, err := s.resolveMigration(c, c.Params("migId"), model.LevelMigrate)
	if err != nil {
		return err
	}
	var body struct {
		AssigneeID  string   `json:"assigneeId"`
		ReviewerIDs []string `json:"reviewerIds"`
	}
	if err := c.BodyParser(&body); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "요청 본문을 해석할 수 없습니다")
	}

	// 지정 대상이 실제로 이 마이그레이션을 만질 수 있는지 서버에서 다시 본다.
	// 화면이 걸러 주지만, 화면을 거치지 않는 요청도 있고 권한은 그 사이에 바뀐다.
	assignee := strings.TrimSpace(body.AssigneeID)
	if assignee != "" {
		if code, msg := s.assignableReason(c, conn, assignee); code != "" {
			return fail(c, fiber.StatusBadRequest, code, msg)
		}
	}
	reviewers := []string{}
	seen := map[string]bool{}
	for _, id := range body.ReviewerIDs {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		if code, msg := s.assignableReason(c, conn, id); code != "" {
			return fail(c, fiber.StatusBadRequest, code, msg)
		}
		seen[id] = true
		reviewers = append(reviewers, id)
	}

	// 담당자는 리뷰어가 될 수 없다.
	//
	// 자기가 끌고 가는 계획을 자기가 검토하는 것은 검토가 아니다. 승인 수만으로는
	// 이것을 막지 못한다 — 한 명만 필요한 계획에서는 담당자가 스스로 승인하고 끝낼 수
	// 있고, 그러면 리뷰 단계는 이름만 남는다. 화면에서도 고를 수 없게 해 두었지만,
	// 화면을 거치지 않는 요청이 있으므로 여기서 다시 본다.
	if assignee != "" && seen[assignee] {
		who := assignee
		if target, err := s.st.GetUser(c.Context(), assignee); err == nil {
			who = displayName(target)
		}
		return fail(c, fiber.StatusBadRequest, "self_review",
			fmt.Sprintf("%s 은(는) 담당자이므로 리뷰어로 지정할 수 없습니다. "+
				"담당자를 바꾸거나 다른 사람에게 검토를 부탁하세요", who))
	}

	u := currentUser(c)
	if err := s.st.SetMigrationAssignment(c.Context(), mig.ID, assignee, reviewers, u.ID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fiber.NewError(fiber.StatusNotFound, "마이그레이션을 찾을 수 없습니다")
		}
		return err
	}

	// 리뷰어를 정했으면 그것이 곧 리뷰 요청이다.
	//
	// 지정해 놓고 "리뷰 요청"을 따로 눌러야 하면 그 한 걸음을 빠뜨리는 일이 생기고,
	// 부탁받은 사람은 화면에 뜨지도 않는 계획을 기다리게 된다. 반대로 초안이 아닌
	// 상태(이미 리뷰 중·승인됨·반려됨)에서는 건드리지 않는다 — 리뷰어를 한 명 더
	// 부르는 것이 승인을 무르는 일이 되어서는 안 된다.
	if len(reviewers) > 0 {
		// 반려된 계획은 초안을 거쳐 간다. draft 전이가 지난 결정을 지우기 때문이다 —
		// 반려가 남아 있으면 새 검토가 들어오는 즉시 다시 반려 상태로 돌아간다.
		steps := []string{}
		switch mig.Status {
		case store.MigrationDraft:
			steps = []string{store.MigrationInReview}
		case store.MigrationRejected:
			steps = []string{store.MigrationDraft, store.MigrationInReview}
		}
		for _, next := range steps {
			if err := s.st.SetMigrationStatus(c.Context(), mig.ID, next); err != nil {
				var ite *store.InvalidTransitionError
				if !errors.As(err, &ite) {
					return err
				}
				// 전이할 수 없는 상태였다면 지정만 남기고 넘어간다. 지정 자체는 이미
				// 저장됐고, 그것을 되돌리는 편이 더 놀랍다.
				break
			}
		}
	}

	updated, err := s.st.GetMigration(c.Context(), mig.ID, true)
	if err != nil {
		return err
	}
	s.audit(c, store.AuditParams{
		Action: "migration.assigned", TargetType: "migration", TargetID: mig.ID,
		Detail: map[string]any{
			"connection": conn.Name, "title": mig.Title,
			"assignee": updated.AssigneeName, "reviewers": len(updated.Reviewers),
			"status": updated.Status,
		},
	})
	return c.JSON(fiber.Map{
		"migration":         updated,
		"connection":        connSummary(conn),
		"approvals":         store.ApprovalCount(updated.Reviews),
		"requiredApprovals": migrate.RequiredApprovals(conn, updated.DestructiveCount),
	})
}

// assignableReason은 그 사람을 지정할 수 없는 이유를 (코드, 문구)로 돌려준다.
// 지정할 수 있으면 빈 코드를 돌려준다.
//
// error가 아니라 값으로 돌려주는 이유: 이 파일의 fail()은 응답을 쓰고 nil을
// 반환한다. 그것을 그대로 넘기면 호출부의 err != nil 검사가 통과해 버려, 400을
// 쓴 뒤에도 저장이 이어진다 — 실제로 그렇게 새어 나갔다.
func (s *Server) assignableReason(c *fiber.Ctx, conn *model.Connection, userID string) (string, string) {
	u, err := s.st.GetUser(c.Context(), userID)
	if err != nil {
		// 조회 실패와 없는 사용자를 굳이 나누지 않는다. 어느 쪽이든 이 id로는
		// 지정할 수 없고, 지정을 이어 가면 외래키에서 500으로 터진다.
		return "unknown_user", "없는 사용자입니다"
	}
	if model.UserStatus(u.Status) != model.UserActive {
		return "inactive_user", fmt.Sprintf("%s 은 비활성화된 계정입니다", displayName(u))
	}
	d, err := s.authz.ResolveScope(c.Context(), u, conn.Scope())
	if err != nil || !d.Allowed || !d.Level.Includes(model.LevelMigrate) {
		return "no_access", fmt.Sprintf("%s 은 %s 의 마이그레이션 권한이 없습니다",
			displayName(u), conn.Name)
	}
	return "", ""
}
