package api

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"

	"dbstudio/internal/dbx"
	"dbstudio/internal/migrate"
	"dbstudio/internal/model"
	"dbstudio/internal/schema"
	"dbstudio/internal/store"
)

// introspect는 대상 DB의 카탈로그를 여러 번 조회하므로 연결 테스트보다 넉넉한 상한을 둔다.
const introspectTimeout = 90 * time.Second

// resolveSchemaAccess는 커넥션을 읽고 모니터링 등급 이상의 권한을 확인한다.
// 스키마 조회는 구조 정보만 노출하므로 monitor 등급에서 허용한다.
//
// 실패 시 *fiber.Error를 반환한다. fail() 같은 "응답을 직접 쓰는" 헬퍼를 여기서
// 쓰면 안 된다 — 그것들은 성공적으로 응답을 쓴 뒤 nil을 반환하므로 호출부의
// err != nil 검사를 통과해버리고, 결과적으로 nil 커넥션으로 진행하게 된다.
// handleSchemaComments는 테이블·컬럼 설명(주석)을 고치는 마이그레이션 계획을 만든다.
//
// 스키마 화면에서 설명을 바로 고쳐 실행하지 않는 이유: 설명은 DB에 저장되는 구조의
// 일부이고(COMMENT), 그것을 바꾸는 것은 DDL이다. 이 앱에서 DDL은 언제나 계획 →
// 리뷰 → 승인 → 실행을 거친다. 설명이라고 그 길을 비켜 가게 하면, 승인 없이 DB를
// 바꾸는 문이 하나 더 생긴다 — 그것이 이 앱이 막으려는 바로 그 상황이다.
//
// 대신 사람이 하는 일은 줄인다: 여러 줄을 한 번에 고쳐 한 계획으로 만든다.
func (s *Server) handleSchemaComments(c *fiber.Ctx) error {
	var body struct {
		Title string `json:"title"`
		Items []struct {
			Namespace string `json:"namespace"`
			Table     string `json:"table"`
			Column    string `json:"column"`
			Comment   string `json:"comment"`
		} `json:"items"`
	}
	if err := c.BodyParser(&body); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "요청 본문을 해석할 수 없습니다")
	}
	if len(body.Items) == 0 {
		return fail(c, fiber.StatusBadRequest, "no_changes", "고칠 설명이 없습니다")
	}

	conn, adapter, err := s.resolveSchemaAccess(c, c.Params("id"))
	if err != nil {
		return err
	}
	// 계획을 만드는 것 자체는 실행이 아니지만 실행 대상을 정의하는 행위다.
	// ERD에서 마이그레이션을 만들 때와 같은 등급을 요구한다.
	d, err := s.requireLevel(c, conn.ID, model.LevelMigrate)
	if err != nil {
		return err
	}
	if !d.Allowed {
		return fail(c, fiber.StatusForbidden, "forbidden", d.Reason)
	}

	current, ierr := s.introspectConnection(c, conn, adapter)
	if ierr != nil {
		return failDetail(c, fiber.StatusBadGateway, "introspect_failed",
			"대상 DB의 스키마를 읽지 못했습니다", ierr.Error())
	}

	// 고친 결과를 담을 사본. 원본을 직접 고치면 diff의 "이전"이 사라진다.
	next := current.Clone()
	changed := 0
	for _, it := range body.Items {
		key := strings.ToLower(strings.TrimSpace(it.Table))
		if ns := strings.TrimSpace(it.Namespace); ns != "" {
			key = strings.ToLower(ns + "." + it.Table)
		}
		tbl := next.Table(key)
		if tbl == nil {
			return fail(c, fiber.StatusBadRequest, "table_not_found",
				fmt.Sprintf("테이블 %s 을(를) 찾을 수 없습니다. 화면을 새로 고쳐 주세요", key))
		}
		comment := strings.TrimSpace(it.Comment)
		if col := strings.TrimSpace(it.Column); col != "" {
			target := tbl.Column(col)
			if target == nil {
				return fail(c, fiber.StatusBadRequest, "column_not_found",
					fmt.Sprintf("%s 에 컬럼 %s 이(가) 없습니다. 화면을 새로 고쳐 주세요", key, col))
			}
			if target.Comment == comment {
				continue
			}
			target.Comment = comment
			changed++
			continue
		}
		if tbl.Comment == comment {
			continue
		}
		tbl.Comment = comment
		changed++
	}
	if changed == 0 {
		return fail(c, fiber.StatusBadRequest, "no_changes", "바뀐 설명이 없습니다")
	}

	diff := schema.Diff(current, next)
	if diff.IsEmpty() {
		return fail(c, fiber.StatusBadRequest, "no_changes", "바뀐 설명이 없습니다")
	}
	plan := schema.BuildPlan(string(conn.Kind), diff)
	if len(plan.Up) == 0 {
		// SQLite처럼 주석을 저장하지 않는 DB가 여기로 온다. 계획을 만들어 두면
		// 아무것도 하지 않는 마이그레이션이 승인 대기 목록에 남는다.
		return failDetail(c, fiber.StatusBadRequest, "no_statements",
			"이 DB에서는 설명을 저장할 수 없습니다",
			strings.Join(plan.Warnings, " / "))
	}

	u := currentUser(c)
	latest, err := s.st.LatestSchemaVersion(c.Context(), conn.ID, false)
	if err != nil {
		return err
	}
	// 기준 버전은 지금 구조가 그 버전과 같을 때만 채운다(마이그레이션 생성과 같은 규칙).
	var fromID *int64
	if latest != nil && latest.Fingerprint == current.Fingerprint() {
		fromID = &latest.ID
	}

	title := strings.TrimSpace(body.Title)
	if title == "" {
		title = fmt.Sprintf("설명 수정 %d건", changed)
	}
	mig, err := s.st.CreateMigration(c.Context(), store.CreateMigrationParams{
		ConnectionID: conn.ID, Title: title,
		FromVersion: fromID, BaseFinger: current.Fingerprint(),
		TargetSchema: next, Plan: plan, Diff: diff, CreatedBy: u.ID,
	})
	if err != nil {
		return err
	}
	s.audit(c, store.AuditParams{
		Action: "schema.comments.plan", TargetType: "migration", TargetID: mig.ID,
		Detail: map[string]any{
			"connection": conn.Name, "title": title,
			"changes": changed, "statements": len(plan.Up),
		},
	})
	return c.Status(fiber.StatusCreated).JSON(fiber.Map{
		"migration":         mig,
		"connection":        connSummary(conn),
		"requiredApprovals": migrate.RequiredApprovals(conn, mig.DestructiveCount),
	})
}

func (s *Server) resolveSchemaAccess(c *fiber.Ctx, connID string) (*model.Connection, dbx.Adapter, error) {
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
	if !adapter.Capabilities().Introspect {
		return nil, nil, fiber.NewError(fiber.StatusBadRequest,
			"이 데이터베이스 종류는 스키마 조회를 지원하지 않습니다")
	}
	return conn, adapter, nil
}

// introspectConnection은 커넥션의 스키마를 실제로 읽어온다.
func (s *Server) introspectConnection(c *fiber.Ctx, conn *model.Connection, adapter dbx.Adapter) (*schema.Schema, error) {
	secret, err := s.st.GetSecret(c.Context(), conn.ID)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(c.Context(), introspectTimeout)
	defer cancel()

	return adapter.Introspect(ctx, dbx.Target{Conn: conn, Secret: secret})
}

// handleGetSchema는 커넥션의 현재 스키마를 반환한다.
//
// 쿼리 파라미터:
//
//	summary=1  테이블 목록과 통계만 (컬럼/인덱스 상세 제외)
//	table=이름  특정 테이블만
func (s *Server) handleGetSchema(c *fiber.Ctx) error {
	conn, adapter, err := s.resolveSchemaAccess(c, c.Params("id"))
	if err != nil {
		return err
	}

	sc, err := s.introspectConnection(c, conn, adapter)
	if err != nil {
		s.audit(c, store.AuditParams{
			Action: "schema.read", TargetType: "connection", TargetID: conn.ID,
			Result: "error", Detail: map[string]any{"name": conn.Name, "error": err.Error()},
		})
		return failDetail(c, fiber.StatusBadGateway, "introspect_failed",
			"스키마를 읽지 못했습니다", err.Error())
	}

	s.audit(c, store.AuditParams{
		Action: "schema.read", TargetType: "connection", TargetID: conn.ID,
		Detail: map[string]any{"name": conn.Name, "tables": len(sc.Tables)},
	})

	if name := strings.TrimSpace(c.Query("table")); name != "" {
		tbl := findTableByName(sc, name)
		if tbl == nil {
			return fail(c, fiber.StatusNotFound, "not_found", "테이블을 찾을 수 없습니다")
		}
		return c.JSON(fiber.Map{
			"connection": connSummary(conn),
			"dialect":    sc.Dialect,
			"shape":      sc.Shape,
			"table":      tbl,
		})
	}

	resp := fiber.Map{
		"connection":  connSummary(conn),
		"dialect":     sc.Dialect,
		"shape":       sc.Shape,
		"name":        sc.Name,
		"capturedAt":  sc.CapturedAt,
		"stats":       sc.Stats(),
		"fingerprint": sc.Fingerprint(),
		"notes":       sc.Notes,
	}

	// 큰 스키마에서 전체 상세를 매번 내려보내면 응답이 수 MB가 된다.
	// 목록 화면은 summary만 쓰고, 상세는 테이블을 펼칠 때 받아간다.
	if c.Query("summary") == "1" {
		resp["tables"] = tableSummaries(sc)
		resp["views"] = sc.Views
		resp["enums"] = sc.Enums
		return c.JSON(resp)
	}

	resp["tables"] = sc.Tables
	resp["views"] = sc.Views
	resp["enums"] = sc.Enums
	resp["sequences"] = sc.Sequences
	return c.JSON(resp)
}

type tableSummary struct {
	Namespace   string `json:"namespace"`
	Name        string `json:"name"`
	Display     string `json:"display"`
	Comment     string `json:"comment,omitempty"`
	Columns     int    `json:"columns"`
	Indexes     int    `json:"indexes"`
	ForeignKeys int    `json:"foreignKeys"`
	HasPK       bool   `json:"hasPk"`
	RowEstimate int64  `json:"rowEstimate"`
	SizeBytes   int64  `json:"sizeBytes"`
}

func tableSummaries(sc *schema.Schema) []tableSummary {
	out := make([]tableSummary, 0, len(sc.Tables))
	for _, t := range sc.Tables {
		out = append(out, tableSummary{
			Namespace: t.Namespace, Name: t.Name, Display: t.Display(),
			Comment: t.Comment, Columns: len(t.Columns), Indexes: len(t.Indexes),
			ForeignKeys: len(t.ForeignKeys), HasPK: t.PrimaryKey != nil,
			RowEstimate: t.RowEstimate, SizeBytes: t.SizeBytes,
		})
	}
	return out
}

func findTableByName(sc *schema.Schema, name string) *schema.Table {
	for _, t := range sc.Tables {
		if strings.EqualFold(t.Display(), name) || strings.EqualFold(t.Name, name) {
			return t
		}
	}
	return nil
}

// connSummary는 스키마 응답에 붙이는 커넥션 요약이다. 자격증명은 포함하지 않는다.
func connSummary(conn *model.Connection) fiber.Map {
	return fiber.Map{
		"id":          conn.ID,
		"name":        conn.Name,
		"kind":        conn.Kind,
		"environment": conn.Environment,
	}
}

type schemaDiffRequest struct {
	// TargetConnectionID는 비교 대상 커넥션이다. 이 커넥션의 스키마가 "목표 상태"가 된다.
	TargetConnectionID string `json:"targetConnectionId"`
}

// handleSchemaDiff는 두 커넥션의 스키마를 비교하고 마이그레이션 계획을 만든다.
//
// 방향: :id (현재 상태) → targetConnectionId (목표 상태).
// 즉 :id 를 target과 같게 만들기 위한 변경 목록과 DDL을 생성한다.
func (s *Server) handleSchemaDiff(c *fiber.Ctx) error {
	var req schemaDiffRequest
	if err := c.BodyParser(&req); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "요청 형식이 올바르지 않습니다")
	}
	if strings.TrimSpace(req.TargetConnectionID) == "" {
		return fail(c, fiber.StatusBadRequest, "bad_request", "비교 대상 커넥션을 지정하세요")
	}
	fromID := c.Params("id")
	if req.TargetConnectionID == fromID {
		return fail(c, fiber.StatusBadRequest, "same_connection", "같은 커넥션끼리는 비교할 수 없습니다")
	}

	// 양쪽 모두에 대해 권한을 확인한다. 한쪽만 접근 가능한 사용자가
	// diff를 통해 다른 쪽 구조를 알아내지 못하게 해야 한다.
	fromConn, fromAdapter, err := s.resolveSchemaAccess(c, fromID)
	if err != nil {
		return err
	}
	toConn, toAdapter, err := s.resolveSchemaAccess(c, req.TargetConnectionID)
	if err != nil {
		return err
	}

	fromSchema, err := s.introspectConnection(c, fromConn, fromAdapter)
	if err != nil {
		return failDetail(c, fiber.StatusBadGateway, "introspect_failed",
			fromConn.Name+" 스키마를 읽지 못했습니다", err.Error())
	}
	toSchema, err := s.introspectConnection(c, toConn, toAdapter)
	if err != nil {
		return failDetail(c, fiber.StatusBadGateway, "introspect_failed",
			toConn.Name+" 스키마를 읽지 못했습니다", err.Error())
	}

	diff := schema.Diff(fromSchema, toSchema)
	// DDL은 변경이 적용될 쪽(from)의 dialect로 생성한다.
	plan := schema.BuildPlan(fromSchema.Dialect, diff)

	// dialect가 다르면 타입 매핑이 개입하므로 사용자에게 알린다.
	if fromSchema.Dialect != toSchema.Dialect {
		plan.Warnings = append(plan.Warnings,
			"서로 다른 DB 종류를 비교했습니다("+fromSchema.Dialect+" ↔ "+toSchema.Dialect+"). "+
				"타입은 논리 타입으로 정규화해 비교했으며, 생성된 DDL은 검토가 필요합니다")
	}

	s.audit(c, store.AuditParams{
		Action: "schema.diff", TargetType: "connection", TargetID: fromID,
		Detail: map[string]any{
			"from": fromConn.Name, "to": toConn.Name,
			"changes": len(diff.Changes), "destructive": diff.DestructiveCount,
		},
	})

	return c.JSON(fiber.Map{
		"from": fiber.Map{
			"connection": connSummary(fromConn),
			"dialect":    fromSchema.Dialect,
			"stats":      fromSchema.Stats(),
			"notes":      fromSchema.Notes,
		},
		"to": fiber.Map{
			"connection": connSummary(toConn),
			"dialect":    toSchema.Dialect,
			"stats":      toSchema.Stats(),
			"notes":      toSchema.Notes,
		},
		"diff": diff,
		"plan": plan,
	})
}

// handleSchemaDDL은 현재 스키마 전체를 만드는 CREATE 스크립트를 생성한다.
// 새 환경 구축이나 ERD 초기 임포트에 쓴다.
// versionForQuery는 version= 쿼리가 가리키는 스키마 버전을 읽는다.
//
// 쿼리가 없으면 (nil, nil)이다 — 호출자는 현재 DB를 읽는다.
//
// 소속 확인을 여기 두는 이유: 버전은 커넥션에 종속되고, 권한은 커넥션 단위로 판정된다.
// 다른 커넥션의 버전 ID를 이 경로로 넘기면 그 판정을 우회하게 된다. 확인을 부르는 쪽에
// 맡기면 새 경로가 생길 때마다 잊을 수 있으므로 읽기와 같은 자리에 둔다.
func (s *Server) versionForQuery(c *fiber.Ctx, conn *model.Connection) (*store.SchemaVersion, error) {
	raw := strings.TrimSpace(c.Query("version"))
	if raw == "" {
		return nil, nil
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return nil, fiber.NewError(fiber.StatusBadRequest, "버전 ID가 올바르지 않습니다")
	}
	v, err := s.st.GetSchemaVersion(c.Context(), id, true)
	if errors.Is(err, store.ErrNotFound) {
		return nil, fiber.NewError(fiber.StatusNotFound, "버전을 찾을 수 없습니다")
	}
	if err != nil {
		return nil, err
	}
	if v.ConnectionID != conn.ID {
		return nil, fiber.NewError(fiber.StatusNotFound, "이 커넥션의 버전이 아닙니다")
	}
	if v.Schema == nil {
		return nil, fiber.NewError(fiber.StatusBadRequest, "이 버전에는 스키마 본문이 없습니다")
	}
	return v, nil
}

func (s *Server) handleSchemaDDL(c *fiber.Ctx) error {
	conn, adapter, err := s.resolveSchemaAccess(c, c.Params("id"))
	if err != nil {
		return err
	}

	// version=<id> 면 그 버전의 스키마로 만든다. 옛 구조의 CREATE 스크립트를 보려고
	// 그 시점의 DB를 되살릴 수는 없으므로, 저장된 스냅샷이 유일한 출처다.
	version, err := s.versionForQuery(c, conn)
	if err != nil {
		return err
	}

	var sc *schema.Schema
	if version != nil {
		sc = version.Schema
	} else {
		live, ierr := s.introspectConnection(c, conn, adapter)
		if ierr != nil {
			return failDetail(c, fiber.StatusBadGateway, "introspect_failed",
				"스키마를 읽지 못했습니다", ierr.Error())
		}
		sc = live
	}

	// 대상 dialect를 지정하면 다른 DB 종류용 스크립트를 만들 수 있다.
	dialect := strings.TrimSpace(c.Query("dialect"))
	if dialect == "" {
		dialect = sc.Dialect
	} else if !model.DBKind(dialect).Valid() {
		return fail(c, fiber.StatusBadRequest, "invalid_dialect", "알 수 없는 DB 종류입니다")
	}

	empty := &schema.Schema{Dialect: dialect, Shape: schema.ShapeRelational}
	diff := schema.Diff(empty, sc)
	plan := schema.BuildPlan(dialect, diff)
	if dialect != sc.Dialect {
		plan.Warnings = append(plan.Warnings,
			sc.Dialect+" → "+dialect+" 로 타입을 변환했습니다. 실행 전 검토가 필요합니다")
	}

	detail := map[string]any{"name": conn.Name, "dialect": dialect, "statements": len(plan.Up)}
	out := fiber.Map{
		"connection": connSummary(conn),
		"dialect":    dialect,
		"plan":       plan,
		"upSql":      plan.UpSQL(),
		"downSql":    plan.DownSQL(),
	}
	if version != nil {
		detail["version"] = version.VersionNo
		out["version"] = fiber.Map{
			"id": version.ID, "versionNo": version.VersionNo,
			"createdAt": version.CreatedAt, "note": version.Note,
		}
	}
	s.audit(c, store.AuditParams{
		Action: "schema.ddl", TargetType: "connection", TargetID: conn.ID, Detail: detail,
	})

	return c.JSON(out)
}
