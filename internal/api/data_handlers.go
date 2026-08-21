package api

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"

	"dbstudio/internal/dbx"
	"dbstudio/internal/model"
	"dbstudio/internal/store"
)

// 데이터 조회·수정·SQL 실행 API.
//
// 이 파일의 모든 핸들러는 두 가지를 지킨다.
//
//  1. **권한은 requireCap을 통과한다.** 등급(requireLevel)이 아니라 능력이다.
//     마이그레이션 권한이 있어도 데이터 조회 권한이 없으면 여기서 막힌다.
//  2. **감사 로그에 값을 남기지 않는다.** 무엇을 조회했는지(테이블, 행 수)는 남기고
//     조회된 값은 남기지 않는다. 감사 로그가 데이터 유출 경로가 되면 안 된다.
//     쓰기는 예외적으로 실행된 문장을 남기지만, 값은 파라미터로 분리되어 있다.

// 데이터 조회는 인덱스가 없는 컬럼 검색이나 큰 테이블의 count(*)에서 오래 걸릴 수
// 있다. 스키마 읽기보다 짧게 잡는 이유는 사용자가 화면 앞에서 기다리는 동작이기
// 때문이다 — 1분을 넘기면 대개 조건을 고쳐야 하는 상황이다.
const dataTimeout = 60 * time.Second

// SQL 실행은 사용자가 의도적으로 무거운 문장을 돌릴 수 있으므로 더 길게 잡는다.
const statementTimeout = 5 * time.Minute

// dataTarget은 커넥션을 읽고 능력을 확인한 뒤 접속 대상을 만든다.
// 네 개의 핸들러가 같은 앞부분을 갖고 있어 한 곳으로 모았다.
func (s *Server) dataTarget(c *fiber.Ctx, need model.Capability) (*dbx.Target, error) {
	connID := c.Params("id")
	conn, err := s.st.GetConnection(c.Context(), connID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, fiber.NewError(fiber.StatusNotFound, "커넥션을 찾을 수 없습니다")
	}
	if err != nil {
		return nil, err
	}

	d, err := s.requireCap(c, connID, need)
	if err != nil {
		return nil, err
	}
	if !d.Allowed {
		return nil, fiber.NewError(fiber.StatusForbidden, d.Reason)
	}
	if !conn.Enabled {
		return nil, fiber.NewError(fiber.StatusBadRequest, "비활성화된 커넥션입니다")
	}

	secret, err := s.st.GetSecret(c.Context(), conn.ID)
	if err != nil {
		return nil, err
	}
	return &dbx.Target{Conn: conn, Secret: secret}, nil
}

// handleDataObjects는 조회 가능한 테이블/컬렉션/키 그룹을 나열한다.
func (s *Server) handleDataObjects(c *fiber.Ctx) error {
	t, err := s.dataTarget(c, model.CapDataRead)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(c.Context(), dataTimeout)
	defer cancel()

	objects, err := dbx.DoListObjects(ctx, *t)
	if err != nil {
		if errors.Is(err, dbx.ErrNotImplemented) {
			return fail(c, fiber.StatusBadRequest, "not_supported",
				"이 데이터베이스 종류는 데이터 조회를 지원하지 않습니다")
		}
		return failDetail(c, fiber.StatusBadGateway, "data_failed",
			"대상 데이터베이스를 조회하지 못했습니다", err.Error())
	}

	s.audit(c, store.AuditParams{
		Action: "data.objects", TargetType: "connection", TargetID: t.Conn.ID,
		Detail: map[string]any{"name": t.Conn.Name, "count": len(objects)},
	})
	return c.JSON(fiber.Map{
		"connection": connSummary(t.Conn),
		"objects":    objects,
		"support":    dbx.DataCapsFor(t.Conn.Kind),
	})
}

type dataQueryRequest struct {
	Namespace string       `json:"namespace"`
	Table     string       `json:"table"`
	Limit     int          `json:"limit"`
	Offset    int          `json:"offset"`
	OrderBy   string       `json:"orderBy"`
	Desc      bool         `json:"desc"`
	Search    string       `json:"search"`
	Filters   []dbx.Filter `json:"filters"`
	WithTotal bool         `json:"withTotal"`
	// Full은 편집을 위해 한 행을 통째로 읽을 때만 true다. 목록 조회에서 켜면
	// 큰 TEXT 컬럼이 그대로 실려 응답이 수십 MB가 된다.
	Full bool `json:"full"`
}

// handleDataQuery는 한 페이지를 조회한다.
//
// GET이 아니라 POST인 이유: 필터가 구조화된 목록이라 쿼리 문자열에 담으면
// 인코딩 규칙을 양쪽에서 맞춰야 하고, 조건이 몇 개만 늘어도 URL 길이 한계에 닿는다.
// 조회이지만 본문이 필요한 요청이며, CSRF 헤더 검사는 그대로 적용된다.
func (s *Server) handleDataQuery(c *fiber.Ctx) error {
	t, err := s.dataTarget(c, model.CapDataRead)
	if err != nil {
		return err
	}

	var req dataQueryRequest
	if err := c.BodyParser(&req); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "요청 형식이 올바르지 않습니다")
	}
	if strings.TrimSpace(req.Table) == "" {
		return fail(c, fiber.StatusBadRequest, "bad_request", "대상을 지정하세요")
	}
	if req.Offset < 0 {
		req.Offset = 0
	}
	for _, f := range req.Filters {
		if !f.Op.Valid() {
			return failDetail(c, fiber.StatusBadRequest, "invalid_filter",
				"알 수 없는 조건입니다", string(f.Op))
		}
	}

	ctx, cancel := context.WithTimeout(c.Context(), dataTimeout)
	defer cancel()

	page, err := dbx.DoQueryRows(ctx, *t, dbx.RowQuery{
		Table:     dbx.TableRef{Namespace: req.Namespace, Name: req.Table},
		Limit:     req.Limit,
		Offset:    req.Offset,
		OrderBy:   req.OrderBy,
		Desc:      req.Desc,
		Filters:   req.Filters,
		Search:    req.Search,
		WithTotal: req.WithTotal,
		Full:      req.Full,
	})
	if err != nil {
		s.audit(c, store.AuditParams{
			Action: "data.query", TargetType: "connection", TargetID: t.Conn.ID,
			Result: "error",
			Detail: map[string]any{"table": req.Table, "error": err.Error()},
		})
		if errors.Is(err, dbx.ErrNotImplemented) {
			return fail(c, fiber.StatusBadRequest, "not_supported",
				"이 데이터베이스 종류는 데이터 조회를 지원하지 않습니다")
		}
		return failDetail(c, fiber.StatusBadGateway, "data_failed",
			"데이터를 조회하지 못했습니다", err.Error())
	}

	// 감사 로그에 검색어는 남기지만 결과 값은 남기지 않는다.
	// 검색어는 "누가 무엇을 찾고 있었는가"라는 조사에 필요한 정보이고,
	// 그 자체가 개인정보인 경우(주민번호를 검색어로 넣는 등)는 있을 수 있으나
	// 그때는 검색 행위 자체가 기록되어야 할 사건이다.
	s.audit(c, store.AuditParams{
		Action: "data.query", TargetType: "connection", TargetID: t.Conn.ID,
		Detail: map[string]any{
			"name": t.Conn.Name, "table": dbx.TableRef{Namespace: req.Namespace, Name: req.Table}.String(),
			"rows": len(page.Rows), "offset": req.Offset,
			"filters": len(req.Filters), "search": req.Search != "",
		},
	})
	return c.JSON(fiber.Map{"page": page})
}

type dataMutateRequest struct {
	Namespace string         `json:"namespace"`
	Table     string         `json:"table"`
	Action    string         `json:"action"`
	Values    map[string]any `json:"values"`
	Key       map[string]any `json:"key"`
}

// handleDataMutate는 행 하나를 추가·수정·삭제한다.
func (s *Server) handleDataMutate(c *fiber.Ctx) error {
	t, err := s.dataTarget(c, model.CapDataWrite)
	if err != nil {
		return err
	}

	var req dataMutateRequest
	if err := c.BodyParser(&req); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "요청 형식이 올바르지 않습니다")
	}
	switch req.Action {
	case "insert", "update", "delete":
	default:
		return fail(c, fiber.StatusBadRequest, "bad_request", "알 수 없는 동작입니다")
	}
	if strings.TrimSpace(req.Table) == "" {
		return fail(c, fiber.StatusBadRequest, "bad_request", "대상을 지정하세요")
	}

	ctx, cancel := context.WithTimeout(c.Context(), dataTimeout)
	defer cancel()

	ref := dbx.TableRef{Namespace: req.Namespace, Name: req.Table}
	result, err := dbx.DoMutateRow(ctx, *t, dbx.RowMutation{
		Table: ref, Action: req.Action, Values: req.Values, Key: req.Key,
	})
	if err != nil {
		s.audit(c, store.AuditParams{
			Action: "data.mutate", TargetType: "connection", TargetID: t.Conn.ID,
			Result: "error",
			Detail: map[string]any{
				"name": t.Conn.Name, "table": ref.String(),
				"op": req.Action, "error": err.Error(),
			},
		})
		if errors.Is(err, dbx.ErrNotImplemented) {
			return fail(c, fiber.StatusBadRequest, "not_supported",
				"이 데이터베이스 종류는 데이터 수정을 지원하지 않습니다")
		}
		return failDetail(c, fiber.StatusBadGateway, "mutate_failed",
			"데이터를 수정하지 못했습니다", err.Error())
	}

	// 쓰기는 실행된 문장을 남긴다. 값은 파라미터로 분리되어 있으므로 자리표시자만
	// 남고, "어떤 테이블의 어떤 행을 언제 누가 고쳤는가"는 그대로 재구성된다.
	// 어느 행이었는지는 기본키로 특정되므로 키 값은 함께 남긴다.
	s.audit(c, store.AuditParams{
		Action: "data.mutate", TargetType: "connection", TargetID: t.Conn.ID,
		Detail: map[string]any{
			"name": t.Conn.Name, "table": ref.String(), "op": req.Action,
			"statement": result.Statement, "affected": result.Affected,
			"key":     req.Key,
			"columns": mapKeys(req.Values),
		},
	})

	if result.Affected == 0 && req.Action != "insert" {
		// 0건은 오류가 아니지만 사용자가 알아야 한다. 대개 다른 사람이 먼저
		// 지웠거나 값이 이미 같은 경우다.
		return c.JSON(fiber.Map{
			"result": result,
			"note":   "대상 행을 찾지 못했거나 변경할 내용이 없습니다",
		})
	}
	return c.JSON(fiber.Map{"result": result})
}

// maxBatchChanges는 한 번에 적용할 수 있는 변경 수다.
//
// 상한이 필요한 이유는 트랜잭션이다: 이 묶음은 전부 한 트랜잭션에서 돌고, 그동안
// 대상 행에 잠금이 잡힌다. 수백 건을 화면에서 모아 보내는 것은 이 화면이 할 일이
// 아니라 SQL 콘솔이 할 일이다.
const maxBatchChanges = 100

type batchChange struct {
	Action string         `json:"action"`
	Values map[string]any `json:"values,omitempty"`
	Key    map[string]any `json:"key,omitempty"`
}

type dataBatchRequest struct {
	Namespace string        `json:"namespace"`
	Table     string        `json:"table"`
	Changes   []batchChange `json:"changes"`
	// DryRun이면 실행하지 않고 문장만 돌려준다.
	DryRun bool `json:"dryRun"`
}

// handleDataBatch는 모아 둔 변경을 한 번에 적용하거나, 실행될 문장을 미리 보여준다.
//
// 화면이 수정을 모아 두었다가 한 번에 보내는 흐름을 위한 것이다. 그 흐름의 값어치는
// 두 가지인데, 둘 다 여기서 지켜진다.
//
//  1. **적용 전에 무엇이 실행될지 본다.** DryRun은 실제 적용과 **같은 코드**로
//     문장을 만든다(dbx.buildMutation). 두 경로로 만들면 미리보기가 거짓말이 된다.
//  2. **전부 되거나 전부 안 된다.** 한 트랜잭션으로 돈다. 절반만 반영되면
//     사용자는 무엇이 남았는지 알 수 없다.
func (s *Server) handleDataBatch(c *fiber.Ctx) error {
	t, err := s.dataTarget(c, model.CapDataWrite)
	if err != nil {
		return err
	}

	var req dataBatchRequest
	if err := c.BodyParser(&req); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "요청 형식이 올바르지 않습니다")
	}
	if strings.TrimSpace(req.Table) == "" {
		return fail(c, fiber.StatusBadRequest, "bad_request", "대상을 지정하세요")
	}
	if len(req.Changes) == 0 {
		return fail(c, fiber.StatusBadRequest, "bad_request", "적용할 변경이 없습니다")
	}
	if len(req.Changes) > maxBatchChanges {
		return fail(c, fiber.StatusBadRequest, "too_many",
			fmt.Sprintf("한 번에 적용할 수 있는 변경은 %d건까지입니다", maxBatchChanges))
	}

	ref := dbx.TableRef{Namespace: req.Namespace, Name: req.Table}
	muts := make([]dbx.RowMutation, 0, len(req.Changes))
	counts := map[string]int{}
	for i, ch := range req.Changes {
		switch ch.Action {
		case "insert", "update", "delete":
		default:
			return fail(c, fiber.StatusBadRequest, "bad_request",
				fmt.Sprintf("%d번째 변경의 동작을 알 수 없습니다", i+1))
		}
		counts[ch.Action]++
		muts = append(muts, dbx.RowMutation{
			Table: ref, Action: ch.Action, Values: ch.Values, Key: ch.Key, DryRun: req.DryRun,
		})
	}

	ctx, cancel := context.WithTimeout(c.Context(), dataTimeout)
	defer cancel()

	results, err := dbx.DoMutateRows(ctx, *t, muts)
	if err != nil {
		// 미리보기 실패는 아직 아무것도 바꾸지 않았다는 뜻이므로 감사 로그를 남기지
		// 않는다. 실제 적용의 실패는 남긴다 — 무엇을 시도했는지가 조사의 출발점이다.
		if !req.DryRun {
			s.audit(c, store.AuditParams{
				Action: "data.batch", TargetType: "connection", TargetID: t.Conn.ID,
				Result: "error",
				Detail: map[string]any{
					"name": t.Conn.Name, "table": ref.String(),
					"changes": len(muts), "ops": counts, "error": err.Error(),
				},
			})
		}
		if errors.Is(err, dbx.ErrNotImplemented) {
			return fail(c, fiber.StatusBadRequest, "not_supported",
				"이 데이터베이스 종류는 모아서 적용하기를 지원하지 않습니다")
		}
		return failDetail(c, fiber.StatusBadGateway, "batch_failed",
			"변경을 적용하지 못했습니다", err.Error())
	}

	if req.DryRun {
		return c.JSON(fiber.Map{"results": results, "dryRun": true})
	}

	var affected int64
	statements := make([]string, 0, len(results))
	for _, r := range results {
		affected += r.Affected
		statements = append(statements, r.Statement)
	}
	s.audit(c, store.AuditParams{
		Action: "data.batch", TargetType: "connection", TargetID: t.Conn.ID,
		Detail: map[string]any{
			"name": t.Conn.Name, "table": ref.String(),
			"changes": len(results), "ops": counts, "affected": affected,
			"statements": statements,
		},
	})
	return c.JSON(fiber.Map{"results": results, "affected": affected})
}

type statementRequest struct {
	Statement string `json:"statement"`
	MaxRows   int    `json:"maxRows"`
	ReadOnly  bool   `json:"readOnly"`
}

// handleRunStatement는 임의의 SQL(또는 Mongo/Redis 명령)을 실행한다.
func (s *Server) handleRunStatement(c *fiber.Ctx) error {
	t, err := s.dataTarget(c, model.CapSQLRun)
	if err != nil {
		return err
	}

	var req statementRequest
	if err := c.BodyParser(&req); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "요청 형식이 올바르지 않습니다")
	}
	if strings.TrimSpace(req.Statement) == "" {
		return fail(c, fiber.StatusBadRequest, "bad_request", "실행할 문장을 입력하세요")
	}

	ctx, cancel := context.WithTimeout(c.Context(), statementTimeout)
	defer cancel()

	start := time.Now()
	results, err := dbx.DoRunStatements(ctx, *t, dbx.StatementRequest{
		Statement: req.Statement, MaxRows: req.MaxRows, ReadOnly: req.ReadOnly,
	})
	if err != nil {
		s.audit(c, store.AuditParams{
			Action: "data.statement", TargetType: "connection", TargetID: t.Conn.ID,
			Result: "error",
			Detail: map[string]any{"name": t.Conn.Name, "error": err.Error()},
		})
		if errors.Is(err, dbx.ErrNotImplemented) {
			return fail(c, fiber.StatusBadRequest, "not_supported",
				"이 데이터베이스 종류는 문장 실행을 지원하지 않습니다")
		}
		return failDetail(c, fiber.StatusBadGateway, "statement_failed",
			"문장을 실행하지 못했습니다", err.Error())
	}

	// 실행한 문장은 통째로 남긴다. 여기서는 문장 자체가 감사의 대상이며,
	// 무엇을 실행했는지 모르면 사고 조사가 불가능하다.
	failed := ""
	for _, r := range results {
		if r.Error != "" {
			failed = r.Error
			break
		}
	}
	result := "ok"
	if failed != "" {
		result = "error"
	}
	s.audit(c, store.AuditParams{
		Action: "data.statement", TargetType: "connection", TargetID: t.Conn.ID,
		Result: result,
		Detail: map[string]any{
			"name": t.Conn.Name, "statement": req.Statement,
			"statements": len(results), "readOnly": req.ReadOnly,
			"elapsedMs": time.Since(start).Milliseconds(), "error": failed,
		},
	})
	return c.JSON(fiber.Map{"results": results})
}

// handleCheckStatement는 문장을 실행하지 않고 검사한다.
//
// 실행과 같은 권한(sql.run)을 요구한다. 검사는 값을 바꾸지 않지만, 어떤 테이블이
// 존재하는지를 오류 메시지로 알려주므로 조회 권한만으로 열어 두면 스키마를
// 더듬어 볼 수 있는 통로가 된다.
func (s *Server) handleCheckStatement(c *fiber.Ctx) error {
	t, err := s.dataTarget(c, model.CapSQLRun)
	if err != nil {
		return err
	}

	var req statementRequest
	if err := c.BodyParser(&req); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "요청 형식이 올바르지 않습니다")
	}
	if strings.TrimSpace(req.Statement) == "" {
		return fail(c, fiber.StatusBadRequest, "bad_request", "검사할 문장을 입력하세요")
	}

	ctx, cancel := context.WithTimeout(c.Context(), statementTimeout)
	defer cancel()

	checks, err := dbx.DoValidateStatements(ctx, *t, req.Statement)
	if err != nil {
		if errors.Is(err, dbx.ErrNotImplemented) {
			return fail(c, fiber.StatusBadRequest, "not_supported",
				"이 데이터베이스 종류는 구문 검사를 지원하지 않습니다")
		}
		return failDetail(c, fiber.StatusBadGateway, "check_failed",
			"구문을 검사하지 못했습니다", err.Error())
	}

	// 감사 로그에 문장을 남기지 않는다. 실행과 달리 아무것도 바꾸지 않으므로
	// 사고 조사의 대상이 아니고, 편집 중에 여러 번 눌리는 버튼이라 남기면
	// 정작 중요한 실행 기록이 검사 기록에 묻힌다.
	bad := 0
	for _, ck := range checks {
		if ck.Status == "error" {
			bad++
		}
	}
	return c.JSON(fiber.Map{"checks": checks, "errors": bad})
}

// mapKeys는 감사 로그에 남길 컬럼 이름 목록을 만든다.
// 정렬하는 이유: 맵 순회 순서는 매번 달라서, 정렬하지 않으면 같은 변경의 로그 두 줄을
// 눈으로 비교할 수 없다.
func mapKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
