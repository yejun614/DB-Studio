package api

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"dbstudio/internal/erd"
	"dbstudio/internal/model"
	"dbstudio/internal/sqlimport"
	"dbstudio/internal/store"
)

// maxImportSQLBytes는 받아들일 스크립트 크기 상한이다.
// 전체 덤프를 붙여 넣는 흐름을 감당하면서, 한 요청이 서버 메모리를 크게 먹지 못하게 한다.
const maxImportSQLBytes = 2 << 20 // 2MB

// handleERDImportSQL은 SQL 스크립트를 읽어 초안에 반영한다.
//
// 미리보기와 적용이 같은 경로다(dryRun 플래그). 두 경로로 나누면 파싱과 병합 계산이
// 두 벌이 되고, 그중 하나만 고치는 날 미리보기가 거짓말을 시작한다.
func (s *Server) handleERDImportSQL(c *fiber.Ctx) error {
	doc, conn, _, err := s.resolveERDDocument(c, c.Params("docId"), model.LevelERD)
	if err != nil {
		return err
	}
	// 읽기 전용 참여자는 불러올 수 없다. 이것은 문서 전체를 바꾸는 편집이다.
	if conn != nil {
		d, lerr := s.requireLevel(c, conn.ID, model.LevelERD)
		if lerr != nil {
			return lerr
		}
		if !d.Allowed {
			return fiber.NewError(fiber.StatusForbidden, d.Reason)
		}
	}

	var body struct {
		SQL    string `json:"sql"`
		Label  string `json:"label"`
		DryRun bool   `json:"dryRun"`
	}
	if err := c.BodyParser(&body); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "요청 본문을 해석할 수 없습니다")
	}
	script := strings.TrimSpace(body.SQL)
	if script == "" {
		return fail(c, fiber.StatusBadRequest, "bad_request", "불러올 SQL을 입력하세요")
	}
	if len(script) > maxImportSQLBytes {
		return fail(c, fiber.StatusBadRequest, "too_large",
			"스크립트가 너무 큽니다 (2MB 제한). 테이블 정의 부분만 나눠서 불러오세요")
	}

	// 파싱은 초안의 방언으로 한다. 타입 문자열의 해석(길이·부호·정밀도)이 여기서
	// 갈리므로, 다른 방언으로 읽으면 컬럼 타입이 조용히 달라진다.
	dialect := doc.Schema.Dialect
	if dialect == "" {
		dialect = doc.Dialect
	}
	parsed, perr := sqlimport.Parse(dialect, script)
	if perr != nil {
		return failDetail(c, fiber.StatusBadRequest, "parse_failed",
			"SQL에서 테이블 정의를 읽지 못했습니다", perr.Error())
	}

	summary := erd.SummarizeImportAll(doc, parsed.Tables, parsed.Drops,
		parsed.Views, parsed.ViewDrops)
	resp := fiber.Map{
		"summary":    summary,
		"notes":      parsed.Notes,
		"statements": parsed.Statements,
		"dialect":    dialect,
	}

	if body.DryRun {
		resp["applied"] = false
		return c.JSON(resp)
	}

	payload, merr := json.Marshal(map[string]any{
		"tables":    parsed.Tables,
		"enums":     parsed.Enums,
		"views":     parsed.Views,
		"drops":     parsed.Drops,
		"viewDrops": parsed.ViewDrops,
		"label":     strings.TrimSpace(body.Label),
	})
	if merr != nil {
		return merr
	}
	u := currentUser(c)
	op := &erd.Op{
		ID: uuid.NewString(), Kind: erd.OpSchemaImport, Payload: payload,
		Actor: u.ID, ActorName: displayName(u), BaseSeq: doc.Seq,
	}

	next, aerr := s.erdHub.SubmitOp(c.Context(), doc.ID, op)
	if aerr != nil {
		var opErr *erd.Error
		if errors.As(aerr, &opErr) {
			return fail(c, fiber.StatusBadRequest, opErr.Code, opErr.Reason)
		}
		if errors.Is(aerr, store.ErrNotFound) {
			return fiber.NewError(fiber.StatusNotFound, "문서를 찾을 수 없습니다")
		}
		return aerr
	}

	s.audit(c, store.AuditParams{
		Action: "erd.import", TargetType: "erd_document", TargetID: doc.ID,
		Detail: map[string]any{
			"name": doc.Name, "connection": connName(conn), "label": body.Label,
			"added": len(summary.Added), "updated": len(summary.Updated),
			"dropped": len(summary.Dropped), "statements": parsed.Statements,
			"views": len(summary.ViewsAdded) + len(summary.ViewsUpdated),
		},
	})

	resp["applied"] = true
	resp["document"] = next
	return c.JSON(resp)
}
