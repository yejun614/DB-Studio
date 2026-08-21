package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"dbstudio/internal/backup"
	"dbstudio/internal/dbx"
	"dbstudio/internal/macro"
	"dbstudio/internal/model"
	"dbstudio/internal/schema"
	"dbstudio/internal/store"
)

// P11~P12에서 더한 기능(데이터 조회/수정, SQL 실행, 매크로, 백업·복구, 버전 롤백)의 툴.
//
// 앞의 툴들과 같은 두 규칙을 따른다.
//
//  1. **호출자의 권한으로 실행한다.** 데이터 툴은 등급이 아니라 능력(data.read /
//     data.write / sql.run)으로 판정하며, 그 판정도 화면과 똑같은 경로(authz.CanCap)를 지난다.
//  2. **쓰기는 제안한다.** 앱 안의 AI 어시스턴트에서는 사용자가 화면에서 승인해야 실행된다.
//     MCP에서는 클라이언트가 그 자리를 대신하지만(도구 호출 승인), 서버 쪽 게이트
//     (승인 수·확인 문구·능력 판정)는 어느 경로에서든 그대로 적용된다.

// dataTools는 이 파일이 제공하는 툴 목록이다. aiTools()가 이어 붙인다.
func dataTools() []*aiTool {
	return []*aiTool{
		// ---------- 데이터 읽기 ----------
		{
			Name: "list_data_objects",
			Description: "조회할 수 있는 테이블·뷰(관계형), 컬렉션(MongoDB), 키 접두사 그룹(Redis)을 나열한다. " +
				"query_data에 넘길 이름을 찾을 때 먼저 호출한다.",
			Schema: objectSchema(map[string]any{
				"connection": str("커넥션 이름 또는 ID"),
			}, "connection"),
			RequiresCap: model.CapDataRead,
			Run:         toolListDataObjects,
		},
		{
			Name: "query_data",
			Description: "테이블·컬렉션의 실제 행을 조회한다. SQL을 쓰지 않는 안전한 조회이며 " +
				"조건·검색어·정렬·페이지를 지정할 수 있다. 구조가 아니라 값을 봐야 할 때 쓴다.",
			Schema: objectSchema(map[string]any{
				"connection": str("커넥션 이름 또는 ID"),
				"table":      str("테이블/컬렉션 이름 (Redis는 키 패턴)"),
				"namespace":  str("스키마 이름 (관계형에서 선택)"),
				"search":     str("모든 컬럼에 대한 부분 일치 검색어"),
				"filters": map[string]any{
					"type":        "array",
					"description": "조건 목록. 각 항목은 {column, op, value}이며 op는 eq/ne/lt/lte/gt/gte/contains/prefix/isnull/notnull",
					"items": objectSchema(map[string]any{
						"column": str("컬럼 이름"),
						"op":     str("비교 연산"),
						"value":  str("비교 값"),
					}, "column", "op"),
				},
				"orderBy": str("정렬 기준 컬럼"),
				"desc":    boolp("내림차순"),
				"limit":   num("최대 행 수 (기본 50, 최대 500)"),
				"offset":  num("건너뛸 행 수"),
			}, "connection", "table"),
			RequiresCap: model.CapDataRead,
			Run:         toolQueryData,
		},
		{
			Name: "run_sql",
			Description: "SELECT 같은 조회 SQL을 실행하고 결과를 반환한다. **읽기 전용으로 강제된다** — " +
				"데이터를 바꾸는 문장은 거부된다. 변경이 필요하면 execute_sql을 쓴다. " +
				"MongoDB는 runCommand JSON, Redis는 명령 한 줄이다.",
			Schema: objectSchema(map[string]any{
				"connection": str("커넥션 이름 또는 ID"),
				"statement":  str("실행할 문장"),
				"maxRows":    num("최대 행 수 (기본 100, 최대 500)"),
			}, "connection", "statement"),
			RequiresCap: model.CapSQLRun,
			Run:         toolRunSQL,
		},

		// ---------- 데이터 쓰기 ----------
		{
			Name: "mutate_data",
			Description: "행 하나를 추가·수정·삭제한다. 수정·삭제는 **기본키로만** 대상을 지정한다. " +
				"사용자 승인이 필요하다.",
			Schema: objectSchema(map[string]any{
				"connection": str("커넥션 이름 또는 ID"),
				"table":      str("테이블/컬렉션 이름"),
				"namespace":  str("스키마 이름 (선택)"),
				"action":     str("insert, update, delete"),
				"values":     map[string]any{"type": "object", "description": "설정할 값 (insert/update)"},
				"key":        map[string]any{"type": "object", "description": "대상 행의 기본키 (update/delete)"},
			}, "connection", "table", "action"),
			Mutating:    true,
			RequiresCap: model.CapDataWrite,
			Propose:     proposeMutateData,
			Apply:       applyMutateData,
		},
		{
			Name: "execute_sql",
			Description: "임의의 SQL을 실행한다(데이터·구조를 바꿀 수 있다). 여러 문장은 세미콜론으로 구분한다. " +
				"사용자 승인이 필요하다. 스키마를 바꾸는 일이라면 마이그레이션 워크플로(create_migration)가 " +
				"이력과 롤백을 남기므로 그쪽이 낫다.",
			Schema: objectSchema(map[string]any{
				"connection": str("커넥션 이름 또는 ID"),
				"statement":  str("실행할 SQL"),
			}, "connection", "statement"),
			Mutating:    true,
			RequiresCap: model.CapSQLRun,
			Propose:     proposeExecuteSQL,
			Apply:       applyExecuteSQL,
		},

		// ---------- 백업 ----------
		{
			Name: "list_backups",
			Description: "논리 덤프(백업) 목록과 최근 복구 이력을 반환한다. 진행 중인 작업의 상태를 " +
				"확인할 때도 이 툴을 쓴다.",
			Schema: objectSchema(map[string]any{
				"connection": str("커넥션 이름 또는 ID (생략하면 전체)"),
				"limit":      num("최대 개수 (기본 20)"),
			}),
			Run: toolListBackups,
		},
		{
			Name: "create_backup",
			Description: "논리 덤프를 만든다. 범위는 full(구조+데이터), schema, data 중 하나다. " +
				"비동기로 시작되며 진행 상황은 list_backups로 확인한다. 사용자 승인이 필요하다.",
			Schema: objectSchema(map[string]any{
				"connection":   str("커넥션 이름 또는 ID"),
				"scope":        str("full, schema, data (기본 full)"),
				"tables":       map[string]any{"type": "array", "items": str("테이블 이름"), "description": "일부만 담을 때"},
				"dropIfExists": boolp("복구 시 기존 테이블을 지우는 DROP 문 포함"),
				"note":         str("메모"),
			}, "connection"),
			Mutating: true,
			Propose:  proposeCreateBackup,
			Apply:    applyCreateBackup,
		},
		{
			Name: "restore_backup",
			Description: "백업을 대상 커넥션에 되돌린다. **되돌릴 수 없는 동작이다.** " +
				"운영 DB로의 복구는 확인 문구가 필요해 이 경로로는 실행되지 않는다(웹 화면에서 해야 한다). " +
				"사용자 승인이 필요하다.",
			Schema: objectSchema(map[string]any{
				"backupId":   str("백업 ID (list_backups로 확인)"),
				"connection": str("복구할 커넥션 이름 또는 ID (생략하면 원래 대상)"),
			}, "backupId"),
			Mutating: true,
			Propose:  proposeRestoreBackup,
			Apply:    applyRestoreBackup,
		},

		// ---------- 매크로 ----------
		{
			Name:         "list_macros",
			Description:  "매크로 목록과 각 매크로의 현재 버전·마지막 실행 상태를 반환한다.",
			Schema:       objectSchema(nil),
			RequiresPerm: model.PermMacro,
			Run:          toolListMacros,
		},
		{
			Name: "get_macro_run",
			Description: "매크로 실행 하나의 상태와 로그를 반환한다. run_macro로 시작한 실행이 " +
				"끝났는지 확인할 때 쓴다.",
			Schema: objectSchema(map[string]any{
				"runId": str("실행 ID"),
			}, "runId"),
			RequiresPerm: model.PermMacro,
			Run:          toolGetMacroRun,
		},
		{
			Name: "run_macro",
			Description: "저장된 매크로를 실행한다. 비동기로 시작되며 상태는 get_macro_run으로 확인한다. " +
				"매크로 안의 각 노드는 실행자의 권한으로 판정된다. 사용자 승인이 필요하다.",
			Schema: objectSchema(map[string]any{
				"macro":  str("매크로 이름 또는 ID"),
				"params": map[string]any{"type": "object", "description": "실행 파라미터"},
			}, "macro"),
			Mutating:     true,
			RequiresPerm: model.PermMacro,
			Propose:      proposeRunMacro,
			Apply:        applyRunMacro,
		},

		// ---------- 버전 롤백 ----------
		{
			Name: "rollback_version",
			Description: "대상 DB의 구조를 지정한 스키마 버전으로 되돌리는 **마이그레이션 계획을 만든다**. " +
				"바로 실행하지 않는다 — 만들어진 계획은 기존 흐름(리뷰 → 승인 → 실행)을 거친다. " +
				"되돌리기는 안전하지 않다: 그 버전 이후에 만들어진 테이블은 삭제된다. 사용자 승인이 필요하다.",
			Schema: objectSchema(map[string]any{
				"connection": str("커넥션 이름 또는 ID"),
				"versionNo":  num("되돌릴 버전 번호 (list_versions로 확인)"),
				"note":       str("메모"),
			}, "connection", "versionNo"),
			Mutating: true,
			Propose:  proposeRollbackVersion,
			Apply:    applyRollbackVersion,
		},
	}
}

// ---------- 데이터 읽기 ----------

func toolListDataObjects(tc *toolContext, args json.RawMessage) (string, error) {
	var in struct {
		Connection string `json:"connection"`
	}
	if err := parseArgs(args, &in); err != nil {
		return "", err
	}
	conn, err := tc.resolveConnCap(in.Connection, model.CapDataRead)
	if err != nil {
		return "", err
	}
	target, err := tc.target(conn)
	if err != nil {
		return "", err
	}

	ctx, cancel := contextWithTimeout(tc, dataTimeout)
	defer cancel()

	objects, err := dbx.DoListObjects(ctx, target)
	if err != nil {
		return "", err
	}
	tc.audit("ai.data.objects", "connection", conn.ID, "ok", map[string]any{
		"connection": conn.Name, "count": len(objects),
	})
	return asJSON(map[string]any{
		"connection": conn.Name,
		"kind":       string(conn.Kind),
		"objects":    objects,
		"support":    dbx.DataCapsFor(conn.Kind),
	})
}

type queryDataArgs struct {
	Connection string       `json:"connection"`
	Table      string       `json:"table"`
	Namespace  string       `json:"namespace"`
	Search     string       `json:"search"`
	Filters    []dbx.Filter `json:"filters"`
	OrderBy    string       `json:"orderBy"`
	Desc       bool         `json:"desc"`
	Limit      int          `json:"limit"`
	Offset     int          `json:"offset"`
}

func toolQueryData(tc *toolContext, args json.RawMessage) (string, error) {
	var in queryDataArgs
	if err := parseArgs(args, &in); err != nil {
		return "", err
	}
	if strings.TrimSpace(in.Table) == "" {
		return "", errors.New("조회할 테이블(table)을 지정하세요")
	}
	conn, err := tc.resolveConnCap(in.Connection, model.CapDataRead)
	if err != nil {
		return "", err
	}
	target, err := tc.target(conn)
	if err != nil {
		return "", err
	}
	if in.Limit <= 0 {
		in.Limit = 50
	}

	ctx, cancel := contextWithTimeout(tc, dataTimeout)
	defer cancel()

	page, err := dbx.DoQueryRows(ctx, target, dbx.RowQuery{
		Table:     dbx.TableRef{Namespace: in.Namespace, Name: in.Table},
		Limit:     in.Limit,
		Offset:    in.Offset,
		OrderBy:   in.OrderBy,
		Desc:      in.Desc,
		Search:    in.Search,
		Filters:   in.Filters,
		WithTotal: true,
	})
	if err != nil {
		return "", err
	}

	// 감사 로그에는 무엇을 조회했는지만 남기고 값은 남기지 않는다.
	// 화면에서 조회할 때와 같은 규칙이다 — 감사 로그가 유출 경로가 되면 안 된다.
	tc.audit("ai.data.query", "connection", conn.ID, "ok", map[string]any{
		"connection": conn.Name, "table": in.Table,
		"rows": len(page.Rows), "filters": len(in.Filters), "search": in.Search != "",
	})

	return asJSON(map[string]any{
		"connection": conn.Name,
		"table":      dbx.TableRef{Namespace: in.Namespace, Name: in.Table}.String(),
		"columns":    page.Columns,
		"rows":       page.Rows,
		"primaryKey": page.PrimaryKey,
		"total":      page.Total,
		"hasMore":    page.HasMore,
		"editable":   page.Editable,
		"notes":      page.Notes,
	})
}

func toolRunSQL(tc *toolContext, args json.RawMessage) (string, error) {
	var in struct {
		Connection string `json:"connection"`
		Statement  string `json:"statement"`
		MaxRows    int    `json:"maxRows"`
	}
	if err := parseArgs(args, &in); err != nil {
		return "", err
	}
	if strings.TrimSpace(in.Statement) == "" {
		return "", errors.New("실행할 문장(statement)을 지정하세요")
	}
	conn, err := tc.resolveConnCap(in.Connection, model.CapSQLRun)
	if err != nil {
		return "", err
	}
	target, err := tc.target(conn)
	if err != nil {
		return "", err
	}
	if in.MaxRows <= 0 {
		in.MaxRows = 100
	}

	ctx, cancel := contextWithTimeout(tc, dataTimeout)
	defer cancel()

	// 읽기 전용은 여기서 강제한다. 모델이 인자로 끌 수 있게 두면
	// "조회만 하는 툴"이라는 설명이 거짓이 된다.
	results, err := dbx.DoRunStatements(ctx, target, dbx.StatementRequest{
		Statement: in.Statement, MaxRows: in.MaxRows, ReadOnly: true,
	})
	if err != nil {
		return "", err
	}
	tc.audit("ai.data.statement", "connection", conn.ID, "ok", map[string]any{
		"connection": conn.Name, "statement": in.Statement, "readOnly": true,
	})
	return asJSON(map[string]any{"connection": conn.Name, "results": results})
}

// ---------- 데이터 쓰기 ----------

type mutateDataArgs struct {
	Connection string         `json:"connection"`
	Table      string         `json:"table"`
	Namespace  string         `json:"namespace"`
	Action     string         `json:"action"`
	Values     map[string]any `json:"values"`
	Key        map[string]any `json:"key"`
}

func (in mutateDataArgs) validate() error {
	if strings.TrimSpace(in.Table) == "" {
		return errors.New("대상 테이블(table)을 지정하세요")
	}
	switch in.Action {
	case "insert", "update", "delete":
	default:
		return errors.New("action은 insert, update, delete 중 하나여야 합니다")
	}
	if in.Action != "insert" && len(in.Key) == 0 {
		return errors.New("수정·삭제에는 대상 행의 기본키(key)가 필요합니다")
	}
	if in.Action != "delete" && len(in.Values) == 0 {
		return errors.New("추가·수정에는 값(values)이 필요합니다")
	}
	return nil
}

func proposeMutateData(tc *toolContext, args json.RawMessage) (string, any, error) {
	var in mutateDataArgs
	if err := parseArgs(args, &in); err != nil {
		return "", nil, err
	}
	if err := in.validate(); err != nil {
		return "", nil, err
	}
	conn, err := tc.resolveConnCap(in.Connection, model.CapDataWrite)
	if err != nil {
		return "", nil, err
	}

	ref := dbx.TableRef{Namespace: in.Namespace, Name: in.Table}
	verb := map[string]string{"insert": "추가", "update": "수정", "delete": "삭제"}[in.Action]
	preview := map[string]any{
		"connection":  conn.Name,
		"environment": string(conn.Environment),
		"table":       ref.String(),
		"action":      in.Action,
		"key":         in.Key,
		"values":      in.Values,
	}
	summary := fmt.Sprintf("%s 의 %s 에서 행 1건을 %s합니다", conn.Name, ref, verb)
	if conn.Environment == model.EnvProd {
		summary = "[운영 DB] " + summary
	}
	return summary, preview, nil
}

func applyMutateData(tc *toolContext, args json.RawMessage) (string, error) {
	var in mutateDataArgs
	if err := parseArgs(args, &in); err != nil {
		return "", err
	}
	if err := in.validate(); err != nil {
		return "", err
	}
	conn, err := tc.resolveConnCap(in.Connection, model.CapDataWrite)
	if err != nil {
		return "", err
	}
	target, err := tc.target(conn)
	if err != nil {
		return "", err
	}

	ctx, cancel := contextWithTimeout(tc, dataTimeout)
	defer cancel()

	ref := dbx.TableRef{Namespace: in.Namespace, Name: in.Table}
	res, err := dbx.DoMutateRow(ctx, target, dbx.RowMutation{
		Table: ref, Action: in.Action, Values: in.Values, Key: in.Key,
	})
	if err != nil {
		return "", err
	}
	tc.audit("ai.data.mutate", "connection", conn.ID, "ok", map[string]any{
		"connection": conn.Name, "table": ref.String(), "op": in.Action,
		"statement": res.Statement, "affected": res.Affected, "key": in.Key,
	})
	return asJSON(map[string]any{
		"connection": conn.Name, "table": ref.String(), "action": in.Action,
		"affected": res.Affected, "statement": res.Statement,
	})
}

func proposeExecuteSQL(tc *toolContext, args json.RawMessage) (string, any, error) {
	var in struct {
		Connection string `json:"connection"`
		Statement  string `json:"statement"`
	}
	if err := parseArgs(args, &in); err != nil {
		return "", nil, err
	}
	if strings.TrimSpace(in.Statement) == "" {
		return "", nil, errors.New("실행할 SQL(statement)을 지정하세요")
	}
	conn, err := tc.resolveConnCap(in.Connection, model.CapSQLRun)
	if err != nil {
		return "", nil, err
	}

	stmts := dbx.SplitStatements(conn.Kind, in.Statement)
	summary := fmt.Sprintf("%s 에서 SQL %d문장을 실행합니다", conn.Name, len(stmts))
	if conn.Environment == model.EnvProd {
		summary = "[운영 DB] " + summary
	}
	return summary, map[string]any{
		"connection":  conn.Name,
		"environment": string(conn.Environment),
		"statements":  stmts,
	}, nil
}

func applyExecuteSQL(tc *toolContext, args json.RawMessage) (string, error) {
	var in struct {
		Connection string `json:"connection"`
		Statement  string `json:"statement"`
	}
	if err := parseArgs(args, &in); err != nil {
		return "", err
	}
	conn, err := tc.resolveConnCap(in.Connection, model.CapSQLRun)
	if err != nil {
		return "", err
	}
	target, err := tc.target(conn)
	if err != nil {
		return "", err
	}

	ctx, cancel := contextWithTimeout(tc, statementTimeout)
	defer cancel()

	results, err := dbx.DoRunStatements(ctx, target, dbx.StatementRequest{
		Statement: in.Statement, MaxRows: 100,
	})
	if err != nil {
		return "", err
	}
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
	tc.audit("ai.data.statement", "connection", conn.ID, result, map[string]any{
		"connection": conn.Name, "statement": in.Statement,
		"statements": len(results), "readOnly": false, "error": failed,
	})
	return asJSON(map[string]any{"connection": conn.Name, "results": results})
}

// ---------- 백업 ----------

func toolListBackups(tc *toolContext, args json.RawMessage) (string, error) {
	var in struct {
		Connection string `json:"connection"`
		Limit      int    `json:"limit"`
	}
	if err := parseArgs(args, &in); err != nil {
		return "", err
	}
	if in.Limit <= 0 {
		in.Limit = 20
	}

	// 접근 가능한 커넥션의 백업만 보여준다. 화면과 같은 규칙이다.
	conns, levels, err := tc.accessibleConns(model.LevelMonitor)
	if err != nil {
		return "", err
	}
	_ = levels
	allowed := make(map[string]bool, len(conns))
	for _, c := range conns {
		allowed[c.ID] = true
	}

	filter := ""
	if strings.TrimSpace(in.Connection) != "" {
		conn, err := tc.resolveConn(in.Connection, model.LevelMonitor)
		if err != nil {
			return "", err
		}
		filter = conn.ID
	}

	backups, err := tc.srv.st.ListBackups(tc.ctx, filter, in.Limit)
	if err != nil {
		return "", err
	}
	items := make([]map[string]any, 0, len(backups))
	for _, b := range backups {
		if b.ConnectionID != "" && !allowed[b.ConnectionID] {
			continue
		}
		items = append(items, map[string]any{
			"id": b.ID, "connection": b.ConnectionName, "scope": b.Scope,
			"status": b.Status, "startedAt": b.StartedAt, "rows": b.RowCount,
			"tables": b.TableCount, "bytes": b.SizeBytes, "note": b.Note,
			"progress": b.Progress, "error": b.Error,
			"fileMissing": b.Status == "success" && !tc.srv.backups.FileExists(b),
		})
	}

	restores, err := tc.srv.st.ListRestores(tc.ctx, filter, 20)
	if err != nil {
		return "", err
	}
	restoreItems := make([]map[string]any, 0, len(restores))
	for _, r := range restores {
		if r.ConnectionID != "" && !allowed[r.ConnectionID] {
			continue
		}
		restoreItems = append(restoreItems, map[string]any{
			"id": r.ID, "connection": r.ConnectionName, "backup": r.BackupLabel,
			"status": r.Status, "done": r.StatementsDone, "total": r.StatementsTotal,
			"error": r.Error, "progress": r.Progress, "startedAt": r.StartedAt,
		})
	}

	return asJSON(map[string]any{"backups": items, "restores": restoreItems})
}

type createBackupArgs struct {
	Connection   string   `json:"connection"`
	Scope        string   `json:"scope"`
	Tables       []string `json:"tables"`
	DropIfExists bool     `json:"dropIfExists"`
	Note         string   `json:"note"`
}

// backupAccess는 범위에 맞는 권한을 확인한다. 핸들러(requireDumpAccess)와 같은 규칙이다:
// 구조를 담으면 monitor 등급, 데이터를 담으면 data.read.
func (tc *toolContext) backupAccess(nameOrID, scope string) (*model.Connection, error) {
	conn, err := tc.findConn(nameOrID)
	if err != nil {
		return nil, err
	}
	if scope == backup.ScopeFull || scope == backup.ScopeSchema {
		if _, err := tc.resolveConn(conn.ID, model.LevelMonitor); err != nil {
			return nil, err
		}
	}
	if scope == backup.ScopeFull || scope == backup.ScopeData {
		if _, err := tc.resolveConnCap(conn.ID, model.CapDataRead); err != nil {
			return nil, err
		}
	}
	if !conn.Enabled {
		return nil, fmt.Errorf("%s 은(는) 비활성화된 커넥션입니다", conn.Name)
	}
	return conn, nil
}

func proposeCreateBackup(tc *toolContext, args json.RawMessage) (string, any, error) {
	var in createBackupArgs
	if err := parseArgs(args, &in); err != nil {
		return "", nil, err
	}
	if in.Scope == "" {
		in.Scope = backup.ScopeFull
	}
	if !backup.ValidScope(in.Scope) {
		return "", nil, errors.New("scope는 full, schema, data 중 하나여야 합니다")
	}
	conn, err := tc.backupAccess(in.Connection, in.Scope)
	if err != nil {
		return "", nil, err
	}

	label := map[string]string{"full": "구조+데이터", "schema": "구조만", "data": "데이터만"}[in.Scope]
	summary := fmt.Sprintf("%s 의 논리 덤프를 만듭니다 (%s)", conn.Name, label)
	return summary, map[string]any{
		"connection": conn.Name, "environment": string(conn.Environment),
		"scope": in.Scope, "tables": in.Tables, "dropIfExists": in.DropIfExists,
	}, nil
}

func applyCreateBackup(tc *toolContext, args json.RawMessage) (string, error) {
	var in createBackupArgs
	if err := parseArgs(args, &in); err != nil {
		return "", err
	}
	if in.Scope == "" {
		in.Scope = backup.ScopeFull
	}
	conn, err := tc.backupAccess(in.Connection, in.Scope)
	if err != nil {
		return "", err
	}
	secret, err := tc.srv.st.GetSecret(tc.ctx, conn.ID)
	if err != nil {
		return "", err
	}

	id, err := tc.srv.backups.StartBackup(tc.ctx, backup.StartBackupParams{
		Target: backup.Target{Conn: conn, Secret: secret},
		Options: backup.Options{
			Scope: in.Scope, Tables: in.Tables,
			DropIfExists: in.DropIfExists, Note: strings.TrimSpace(in.Note),
		},
		Actor: tc.user, Trigger: "manual",
	})
	if err != nil {
		return "", err
	}
	tc.audit("ai.backup.start", "connection", conn.ID, "ok", map[string]any{
		"connection": conn.Name, "backupId": id, "scope": in.Scope,
	})
	return asJSON(map[string]any{
		"backupId": id, "status": "running",
		"note": "백업이 시작되었습니다. list_backups로 진행 상황을 확인하세요.",
	})
}

// restoreTarget은 복구 대상과 권한을 확인한다.
// 핸들러와 같은 규칙: migrate 등급 + data.write 둘 다.
func (tc *toolContext) restoreTarget(backupID, connArg string) (*store.Backup, *model.Connection, error) {
	b, err := tc.srv.st.GetBackup(tc.ctx, strings.TrimSpace(backupID))
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil, fmt.Errorf("백업 %q 을(를) 찾을 수 없습니다. list_backups로 확인하세요", backupID)
	}
	if err != nil {
		return nil, nil, err
	}
	if b.Status != "success" {
		return nil, nil, fmt.Errorf("성공한 백업만 복구할 수 있습니다 (현재 %s)", b.Status)
	}

	targetRef := strings.TrimSpace(connArg)
	if targetRef == "" {
		targetRef = b.ConnectionID
	}
	if targetRef == "" {
		return nil, nil, errors.New("복구할 커넥션을 지정하세요 (원래 대상이 삭제되었습니다)")
	}
	conn, err := tc.resolveConn(targetRef, model.LevelMigrate)
	if err != nil {
		return nil, nil, err
	}
	if _, err := tc.resolveConnCap(conn.ID, model.CapDataWrite); err != nil {
		return nil, nil, err
	}
	// 운영 DB 복구는 확인 문구를 요구한다. 그 문구는 사람이 직접 타이핑하게 만드는
	// 장치이므로 프로그램 경로에서 통과시키면 존재 이유가 사라진다.
	if conn.Environment == model.EnvProd {
		return nil, nil, fmt.Errorf(
			"%s 은(는) 운영 DB입니다. 운영 DB 복구는 확인 문구가 필요해 웹 화면에서만 할 수 있습니다", conn.Name)
	}
	return b, conn, nil
}

func proposeRestoreBackup(tc *toolContext, args json.RawMessage) (string, any, error) {
	var in struct {
		BackupID   string `json:"backupId"`
		Connection string `json:"connection"`
	}
	if err := parseArgs(args, &in); err != nil {
		return "", nil, err
	}
	b, conn, err := tc.restoreTarget(in.BackupID, in.Connection)
	if err != nil {
		return "", nil, err
	}
	drop, _ := b.Options["dropIfExists"].(bool)

	summary := fmt.Sprintf("%s 의 %s 백업을 %s 에 복구합니다 (되돌릴 수 없음)",
		b.ConnectionName, b.StartedAt.Format("2006-01-02 15:04"), conn.Name)
	return summary, map[string]any{
		"backupId": b.ID, "source": b.ConnectionName, "target": conn.Name,
		"scope": b.Scope, "rows": b.RowCount, "dropIfExists": drop,
		"crossConnection": b.ConnectionID != conn.ID,
	}, nil
}

func applyRestoreBackup(tc *toolContext, args json.RawMessage) (string, error) {
	var in struct {
		BackupID   string `json:"backupId"`
		Connection string `json:"connection"`
	}
	if err := parseArgs(args, &in); err != nil {
		return "", err
	}
	b, conn, err := tc.restoreTarget(in.BackupID, in.Connection)
	if err != nil {
		return "", err
	}
	secret, err := tc.srv.st.GetSecret(tc.ctx, conn.ID)
	if err != nil {
		return "", err
	}

	id, err := tc.srv.backups.StartRestore(tc.ctx, backup.StartRestoreParams{
		Backup: b,
		Target: backup.Target{Conn: conn, Secret: secret},
		Actor:  tc.user,
	})
	if err != nil {
		return "", err
	}
	tc.audit("ai.backup.restore", "connection", conn.ID, "ok", map[string]any{
		"connection": conn.Name, "backupId": b.ID, "restoreId": id,
		"sourceConnection": b.ConnectionName,
	})
	return asJSON(map[string]any{
		"restoreId": id, "status": "running",
		"note": "복구가 시작되었습니다. list_backups로 진행 상황을 확인하세요.",
	})
}

// ---------- 매크로 ----------

// toolListMacros는 대화 상대가 볼 수 있는 매크로만 보여준다.
//
// AI 툴은 언제나 호출자의 권한으로 동작한다는 앱 전체의 규칙이 여기서도 적용된다 —
// 어시스턴트에게 물어보면 남의 비공개 매크로가 나오는 뒷문이 생기면 안 된다.
func toolListMacros(tc *toolContext, args json.RawMessage) (string, error) {
	macros, err := tc.srv.st.ListMacros(tc.ctx, store.MacroViewer{User: tc.user})
	if err != nil {
		return "", err
	}
	items := make([]map[string]any, 0, len(macros))
	for _, m := range macros {
		items = append(items, map[string]any{
			"id": m.ID, "name": m.Name, "description": m.Description,
			"version": m.CurrentVersion, "versions": m.VersionCount,
			"lastRunAt": m.LastRunAt, "lastRunStatus": m.LastRunStatus,
			"visibility": m.Visibility, "access": m.Access,
		})
	}
	return asJSON(map[string]any{"macros": items})
}

func toolGetMacroRun(tc *toolContext, args json.RawMessage) (string, error) {
	var in struct {
		RunID string `json:"runId"`
	}
	if err := parseArgs(args, &in); err != nil {
		return "", err
	}
	run, err := tc.srv.st.GetMacroRun(tc.ctx, strings.TrimSpace(in.RunID))
	if errors.Is(err, store.ErrNotFound) {
		return "", fmt.Errorf("실행 기록 %q 을(를) 찾을 수 없습니다", in.RunID)
	}
	if err != nil {
		return "", err
	}
	logs, err := tc.srv.st.ListRunLogs(tc.ctx, run.ID, 0)
	if err != nil {
		return "", err
	}
	// 로그가 길면 뒤쪽(실패 원인이 있는 곳)을 남긴다.
	const maxLogs = 120
	if len(logs) > maxLogs {
		logs = logs[len(logs)-maxLogs:]
	}
	return asJSON(map[string]any{
		"run": map[string]any{
			"id": run.ID, "macro": run.MacroName, "version": run.Version,
			"status": run.Status, "actor": run.ActorName, "error": run.Error,
			"startedAt": run.StartedAt, "durationMs": run.DurationMs, "nodes": run.NodeCount,
		},
		"logs": logs,
		"live": tc.srv.macros.IsRunning(run.ID),
	})
}

// resolveMacro는 이름 또는 ID로 매크로를 찾는다.
func (tc *toolContext) resolveMacro(nameOrID string) (*store.Macro, error) {
	nameOrID = strings.TrimSpace(nameOrID)
	if nameOrID == "" {
		return nil, errors.New("매크로를 지정하세요")
	}
	macros, err := tc.srv.st.ListMacros(tc.ctx, store.MacroViewer{User: tc.user})
	if err != nil {
		return nil, err
	}
	for _, m := range macros {
		if m.ID == nameOrID || strings.EqualFold(m.Name, nameOrID) {
			return m, nil
		}
	}
	return nil, fmt.Errorf("매크로 %q 을(를) 찾을 수 없습니다. list_macros로 확인하세요", nameOrID)
}

// macroRunPlan은 실행 전에 권한을 판정하고 그래프를 돌려준다.
// 판정은 엔진의 공용 함수를 쓴다 — 화면·자동 실행과 같은 규칙이어야 한다.
func (tc *toolContext) macroRunPlan(nameOrID string) (*store.Macro, *store.MacroVersion, []macro.Blocker, error) {
	if !tc.user.HasPerm(model.PermMacro) {
		return nil, nil, nil, errors.New("매크로 사용 권한이 없습니다")
	}
	m, err := tc.resolveMacro(nameOrID)
	if err != nil {
		return nil, nil, nil, err
	}
	ver, err := tc.srv.st.GetMacroVersion(tc.ctx, m.ID, 0)
	if err != nil {
		return nil, nil, nil, err
	}
	graph, err := macro.ParseGraph(ver.Graph)
	if err != nil {
		return nil, nil, nil, err
	}
	blockers, err := tc.srv.macros.Blockers(tc.ctx, tc.user, graph)
	if err != nil {
		return nil, nil, nil, err
	}
	return m, ver, blockers, nil
}

func proposeRunMacro(tc *toolContext, args json.RawMessage) (string, any, error) {
	var in struct {
		Macro  string         `json:"macro"`
		Params map[string]any `json:"params"`
	}
	if err := parseArgs(args, &in); err != nil {
		return "", nil, err
	}
	m, ver, blockers, err := tc.macroRunPlan(in.Macro)
	if err != nil {
		return "", nil, err
	}
	if len(blockers) > 0 {
		reasons := make([]string, 0, len(blockers))
		for _, b := range blockers {
			reasons = append(reasons, b.Node+": "+b.Reason)
		}
		return "", nil, fmt.Errorf("이 매크로를 실행할 권한이 없습니다 — %s", strings.Join(reasons, " / "))
	}
	return fmt.Sprintf("매크로 %q (v%d) 를 실행합니다", m.Name, ver.Version),
		map[string]any{"macro": m.Name, "version": ver.Version, "params": in.Params}, nil
}

func applyRunMacro(tc *toolContext, args json.RawMessage) (string, error) {
	var in struct {
		Macro  string         `json:"macro"`
		Params map[string]any `json:"params"`
	}
	if err := parseArgs(args, &in); err != nil {
		return "", err
	}
	m, ver, blockers, err := tc.macroRunPlan(in.Macro)
	if err != nil {
		return "", err
	}
	if len(blockers) > 0 {
		return "", errors.New("이 매크로를 실행할 권한이 없습니다")
	}

	runID, err := tc.srv.macros.Start(tc.ctx, macro.RunRequest{
		MacroID: m.ID, Version: ver.Version, Actor: tc.user, ActorIP: tc.ip,
		Params: in.Params, Trigger: "manual",
	})
	if err != nil {
		return "", err
	}
	tc.audit("ai.macro.run", "macro", m.ID, "ok", map[string]any{
		"name": m.Name, "version": ver.Version, "runId": runID, "params": in.Params,
	})
	return asJSON(map[string]any{
		"runId": runID, "macro": m.Name, "version": ver.Version, "status": "running",
		"note": "매크로가 시작되었습니다. get_macro_run으로 진행 상황을 확인하세요.",
	})
}

// ---------- 버전 롤백 ----------

type rollbackArgs struct {
	Connection string `json:"connection"`
	VersionNo  int    `json:"versionNo"`
	Note       string `json:"note"`
}

// rollbackPlan은 되돌리기 계획을 계산한다. 제안과 실행이 같은 계산을 쓴다.
func (tc *toolContext) rollbackPlan(in rollbackArgs) (*model.Connection, *store.SchemaVersion,
	*schema.Schema, *schema.DiffResult, *schema.Plan, error) {
	conn, err := tc.resolveConn(in.Connection, model.LevelMigrate)
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}
	if in.VersionNo <= 0 {
		return nil, nil, nil, nil, nil, errors.New("되돌릴 버전 번호(versionNo)를 지정하세요")
	}

	versions, err := tc.srv.st.ListSchemaVersions(tc.ctx, conn.ID, 500)
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}
	var targetID int64 = -1
	for _, v := range versions {
		if v.VersionNo == in.VersionNo {
			targetID = v.ID
			break
		}
	}
	if targetID < 0 {
		return nil, nil, nil, nil, nil, fmt.Errorf("v%d 를 찾을 수 없습니다. list_versions로 확인하세요", in.VersionNo)
	}
	target, err := tc.srv.st.GetSchemaVersion(tc.ctx, targetID, true)
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}
	if target.Schema == nil {
		return nil, nil, nil, nil, nil, errors.New("이 버전에는 스키마 본문이 없습니다")
	}

	current, err := tc.introspect(conn)
	if err != nil {
		return nil, nil, nil, nil, nil, fmt.Errorf("현재 스키마를 읽지 못했습니다: %w", err)
	}
	diff := schema.Diff(current, target.Schema)
	if diff.IsEmpty() {
		return nil, nil, nil, nil, nil, fmt.Errorf("현재 구조가 이미 v%d 와 같습니다", in.VersionNo)
	}
	plan := schema.BuildPlan(string(conn.Kind), diff)
	if len(plan.Up) == 0 {
		return nil, nil, nil, nil, nil, fmt.Errorf(
			"되돌릴 변경은 있지만 실행할 SQL을 만들 수 없습니다: %s", strings.Join(plan.Warnings, " / "))
	}
	return conn, target, current, diff, plan, nil
}

func proposeRollbackVersion(tc *toolContext, args json.RawMessage) (string, any, error) {
	var in rollbackArgs
	if err := parseArgs(args, &in); err != nil {
		return "", nil, err
	}
	conn, target, _, diff, plan, err := tc.rollbackPlan(in)
	if err != nil {
		return "", nil, err
	}

	summary := fmt.Sprintf("%s 를 v%d 로 되돌리는 마이그레이션 계획을 만듭니다 (변경 %d건, 파괴적 %d건)",
		conn.Name, target.VersionNo, len(diff.Changes), diff.DestructiveCount)
	return summary, map[string]any{
		"connection": conn.Name, "toVersion": target.VersionNo,
		"changes": changeList(diff), "destructive": diff.DestructiveCount,
		"statements": len(plan.Up), "warnings": plan.Warnings,
		"note": "계획을 만들 뿐 실행하지 않습니다. 검토·승인 후 apply_migration으로 실행합니다.",
	}, nil
}

func applyRollbackVersion(tc *toolContext, args json.RawMessage) (string, error) {
	var in rollbackArgs
	if err := parseArgs(args, &in); err != nil {
		return "", err
	}
	conn, target, current, diff, plan, err := tc.rollbackPlan(in)
	if err != nil {
		return "", err
	}

	// 지금 상태가 버전으로 남아 있지 않으면 먼저 확정한다.
	// 그러지 않으면 되돌린 뒤 "무엇으로부터 되돌렸는가"를 말할 수 없다.
	from, err := tc.srv.st.LatestSchemaVersion(tc.ctx, conn.ID, false)
	if err != nil {
		return "", err
	}
	if from == nil || from.Fingerprint != current.Fingerprint() {
		from, _, err = tc.srv.st.SaveSchemaVersion(tc.ctx, store.SaveVersionParams{
			ConnectionID: conn.ID, Schema: current,
			Source: sourceForBaseline(from), Note: "롤백 기준선",
			AuthorID: tc.user.ID, AuthorName: tc.user.Username,
		})
		if err != nil {
			return "", err
		}
	}

	title := fmt.Sprintf("v%d 으로 롤백", target.VersionNo)
	if note := strings.TrimSpace(in.Note); note != "" {
		title += " — " + note
	}
	mig, err := tc.srv.st.CreateMigration(tc.ctx, store.CreateMigrationParams{
		ConnectionID: conn.ID, Title: title,
		FromVersion: &from.ID, RollbackTo: &target.ID,
		BaseFinger:   current.Fingerprint(),
		TargetSchema: target.Schema, Plan: plan, Diff: diff, CreatedBy: tc.user.ID,
	})
	if err != nil {
		return "", err
	}
	tc.audit("ai.version.rollback.plan", "migration", mig.ID, "ok", map[string]any{
		"connection": conn.Name, "toVersion": target.VersionNo,
		"changes": len(diff.Changes), "destructive": diff.DestructiveCount,
	})
	return asJSON(map[string]any{
		"migrationId": mig.ID, "title": mig.Title, "status": mig.Status,
		"changes": len(diff.Changes), "destructive": diff.DestructiveCount,
		"note": "계획을 만들었습니다. request_review → 승인 → apply_migration 순서로 진행합니다.",
	})
}

// contextWithTimeout은 툴 실행 컨텍스트에 시간 제한을 건다.
//
// 툴마다 직접 context.WithTimeout을 부르지 않고 여기로 모은 이유: 어떤 툴이
// 제한 없이 도는 상태를 만들지 않기 위해서다. 대상 DB가 응답하지 않으면
// AI 대화나 MCP 요청 하나가 영원히 매달린다.
func contextWithTimeout(tc *toolContext, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(tc.ctx, d)
}
