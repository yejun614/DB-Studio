package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gofiber/fiber/v2"

	"dbstudio/internal/ai"
	"dbstudio/internal/dblog"
	"dbstudio/internal/dbx"
	"dbstudio/internal/erd"
	"dbstudio/internal/migrate"
	"dbstudio/internal/model"
	"dbstudio/internal/schema"
	"dbstudio/internal/store"
)

// 이 파일은 앱의 기능을 AI 툴로 노출한다.
//
// 설계의 핵심은 하나다: **툴은 호출자의 권한으로 실행된다.** 모든 툴이 사용자 요청과
// 똑같은 권한 판정(auth.Authorizer.Can)을 통과하므로, 모델이 "운영 DB를 보여줘"라고
// 결정해도 그 사용자가 접근할 수 없으면 볼 수 없다. AI에게 별도의 서비스 계정을 주면
// 그 순간 권한 모델이 무의미해진다.
//
// 두 번째 원칙: **쓰기·파괴적 툴은 모델이 실행할 수 없다.** 그런 툴은 실행 대신
// 제안(pending action)을 만들고, 사용자가 화면에서 승인해야 실행된다. 승인 후에도
// 기존 게이트(마이그레이션 승인 수, 프리체크, 확인 문구)는 그대로 적용된다.

// toolContext는 툴 실행에 필요한 호출자 정보다.
//
// *fiber.Ctx를 담지 않는 이유가 중요하다: SSE 스트리밍은 핸들러가 반환한 뒤에
// 본문을 쓰므로 그때 fiber 컨텍스트를 만지면 이미 해제된 메모리를 읽는다.
// 그래서 필요한 값(사용자, IP)을 미리 복사해 둔다.
type toolContext struct {
	ctx     context.Context
	srv     *Server
	user    *model.User
	ip      string
	session *store.AISession
}

// aiTool은 하나의 툴 정의다.
type aiTool struct {
	Name        string
	Description string
	Schema      map[string]any

	// Mutating이면 모델이 직접 실행할 수 없고 제안만 만든다.
	Mutating bool
	// SuperadminOnly면 슈퍼 어드민만 쓸 수 있다.
	SuperadminOnly bool
	// ConnManagerOnly면 커넥션 관리자(어드민 이상)만 쓸 수 있다.
	//
	// SuperadminOnly와 따로 둔 이유: 서버·DB 등록은 어드민의 일이고 사용자 관리는
	// 슈퍼 어드민의 일이다. 하나로 합치면 어드민이 자기 일을 툴로 할 수 없게 된다.
	ConnManagerOnly bool
	// RequiresPerm이 지정되면 그 전역 권한이 있는 사용자에게만 노출된다.
	RequiresPerm model.Perm
	// RequiresCap이 지정되면 그 데이터 능력을 **어느 커넥션에서든** 가진 사용자에게만
	// 노출된다. 실제 판정은 커넥션별이므로 여기서는 목록에서 감출지만 정한다 —
	// 어차피 못 쓰는 툴을 보여주면 모델이 그것을 시도하는 데 토큰을 쓴다.
	RequiresCap model.Capability

	// Run은 읽기 툴의 실행이다.
	Run func(tc *toolContext, args json.RawMessage) (string, error)
	// Propose는 쓰기 툴의 제안을 만든다 (요약 + 미리보기).
	Propose func(tc *toolContext, args json.RawMessage) (summary string, preview any, err error)
	// Apply는 사용자 승인 후의 실제 실행이다.
	Apply func(tc *toolContext, args json.RawMessage) (string, error)
}

// objectSchema는 JSON Schema 객체를 짧게 만든다.
func objectSchema(props map[string]any, required ...string) map[string]any {
	if props == nil {
		props = map[string]any{}
	}
	s := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func str(desc string) map[string]any   { return map[string]any{"type": "string", "description": desc} }
func num(desc string) map[string]any   { return map[string]any{"type": "integer", "description": desc} }
func boolp(desc string) map[string]any { return map[string]any{"type": "boolean", "description": desc} }

// aiTools는 툴 레지스트리다. 이름 → 정의.
//
// 메서드가 아니라 함수로 두고 Server를 인자로 받지 않는 이유: 툴 구현은 toolContext를
// 통해서만 앱에 접근하고, 그 안에 권한 판정이 들어 있다. 서버를 직접 캡처하면
// 권한 검사를 우회하는 구현을 쓰기 쉬워진다.
func aiTools() map[string]*aiTool {
	list := []*aiTool{
		// ---------- 읽기 ----------
		{
			Name: "list_connections",
			Description: "이 사용자가 접근할 수 있는 DB 커넥션 목록과 각 커넥션에 대한 권한 등급을 반환한다. " +
				"다른 툴에 넘길 커넥션 이름을 찾을 때 먼저 호출한다.",
			Schema: objectSchema(map[string]any{
				"environment": str("dev 또는 prod로 걸러낸다 (생략하면 전체)"),
			}),
			Run: toolListConnections,
		},
		{
			Name:        "get_connection_status",
			Description: "커넥션의 최신 상태(접속 여부, 응답 시간, 최근 지표)와 열린 이벤트 수를 반환한다.",
			Schema: objectSchema(map[string]any{
				"connection": str("커넥션 이름 또는 ID"),
			}, "connection"),
			Run: toolConnectionStatus,
		},
		{
			Name:        "get_metrics",
			Description: "커넥션의 지표 시계열을 반환한다. 성능 문제를 조사할 때 쓴다.",
			Schema: objectSchema(map[string]any{
				"connection": str("커넥션 이름 또는 ID"),
				"metric":     str("지표 이름 (생략하면 사용 가능한 지표 목록을 반환한다)"),
				"range":      str("조회 기간: 1h, 6h, 24h, 7d (기본 1h)"),
			}, "connection"),
			Run: toolGetMetrics,
		},
		{
			Name:        "list_events",
			Description: "모니터링 이벤트(임계치 위반, 접속 실패, 스키마 외부 변경)를 반환한다.",
			Schema: objectSchema(map[string]any{
				"state":    str("open 또는 resolved (기본 open)"),
				"severity": str("info, warning, critical"),
				"limit":    num("최대 개수 (기본 20)"),
			}),
			Run: toolListEvents,
		},
		{
			Name: "search_logs",
			Description: "DB 로그와 쿼리 통계를 조회한다. 느린 쿼리를 찾거나 오류 원인을 조사할 때 쓴다. " +
				"소스는 DB 설정에 의존하므로 사용할 수 없는 소스가 있으면 그 이유가 함께 반환된다.",
			Schema: objectSchema(map[string]any{
				"connection":  str("커넥션 이름 또는 ID"),
				"q":           str("메시지·쿼리 부분 문자열 검색"),
				"minDuration": num("이보다 느린 쿼리만 (ms)"),
				"range":       str("조회 기간 (기본 1h)"),
				"limit":       num("최대 개수 (기본 20)"),
			}, "connection"),
			Run: toolSearchLogs,
		},
		{
			Name: "introspect_schema",
			Description: "커넥션의 현재 스키마를 읽는다. table을 지정하면 그 테이블의 컬럼·인덱스·제약을 " +
				"상세히, 생략하면 테이블 목록과 규모를 반환한다.",
			Schema: objectSchema(map[string]any{
				"connection": str("커넥션 이름 또는 ID"),
				"table":      str("테이블 이름 (생략하면 목록)"),
			}, "connection"),
			Run: toolIntrospectSchema,
		},
		{
			Name: "explore_nosql",
			Description: "MongoDB/Redis 전용 조회. MongoDB는 컬렉션별 저장 크기·인덱스 사용 횟수·필드 분포를, " +
				"Redis는 메모리 정책·지속성 상태·키 접두사 분포·큰 키·명령 통계를 반환한다. " +
				"관계형 DB에는 쓸 수 없다(introspect_schema를 쓴다).",
			Schema: objectSchema(map[string]any{
				"connection": str("커넥션 이름 또는 ID (MongoDB 또는 Redis)"),
			}, "connection"),
			Run: toolExploreNoSQL,
		},
		{
			Name:        "diff_schema",
			Description: "두 커넥션의 스키마를 비교해 변경 목록과 마이그레이션 SQL을 반환한다.",
			Schema: objectSchema(map[string]any{
				"from": str("현재 상태 커넥션"),
				"to":   str("목표 상태 커넥션"),
			}, "from", "to"),
			Run: toolDiffSchema,
		},
		{
			Name:        "list_erd_documents",
			Description: "ERD 초안 목록을 반환한다.",
			Schema:      objectSchema(nil),
			Run:         toolListERDDocuments,
		},
		{
			Name: "search_glossary",
			Description: "프로젝트의 용어 사전을 찾는다. 팀이 정해 둔 '이 말은 이 물리명으로 쓴다'는 " +
				"약속이다. 테이블·컬럼 이름을 지을 때 먼저 부른다 — 이미 정해 둔 이름이 있으면 " +
				"그것을 써야 하고, 없으면 새로 짓고 사전에도 올려야 한다. " +
				"쓰이고 있는 분류 목록도 함께 반환한다.",
			Schema: objectSchema(map[string]any{
				"project": str("프로젝트 이름 (생략하면 참여 중인 프로젝트가 하나일 때만 그것)"),
				"q":       str("용어·물리명·설명에서 찾을 말 (생략하면 전체)"),
				"limit":   num("최대 개수 (기본 50)"),
			}),
			Run: toolSearchGlossary,
		},
		{
			Name: "add_glossary_term",
			Description: "용어 사전에 한 줄을 더한다. 설계하면서 새로 정한 이름을 사전에 남길 때 쓴다. " +
				"이미 있는 용어면 거절하고 알려준다 — 같은 말이 두 줄이면 사전이 답을 두 개 준다. " +
				"분류(대/중/소)는 선택이고, 있는 분류를 쓰려면 search_glossary 로 먼저 확인한다.",
			Schema: objectSchema(map[string]any{
				"project":  str("프로젝트 이름 (생략하면 참여 중인 프로젝트가 하나일 때만 그것)"),
				"term":     str("용어 (예: 주문)"),
				"physical": str("물리명 (예: order)"),
				"note":     str("설명 (선택)"),
				"cat1":     str("대분류 (선택)"),
				"cat2":     str("중분류 (선택)"),
				"cat3":     str("소분류 (선택)"),
			}, "term", "physical"),
			Run: toolAddGlossaryTerm,
		},
		{
			Name: "update_glossary_term",
			Description: "사전의 한 줄을 고친다. 보낸 것만 바뀐다 — 안 보낸 칸은 그대로 둔다. " +
				"지우는 툴은 없다: 사전은 여러 사람이 함께 쓰는 것이라 지우는 일은 화면에서 사람이 한다.",
			Schema: objectSchema(map[string]any{
				"project":  str("프로젝트 이름 (생략하면 참여 중인 프로젝트가 하나일 때만 그것)"),
				"term":     str("고칠 용어 (id 를 주면 생략 가능)"),
				"id":       str("고칠 용어의 id (선택)"),
				"newTerm":  str("새 용어 (선택)"),
				"physical": str("새 물리명 (선택)"),
				"note":     str("새 설명 (선택)"),
				"cat1":     str("대분류 (선택)"),
				"cat2":     str("중분류 (선택)"),
				"cat3":     str("소분류 (선택)"),
			}),
			Run: toolUpdateGlossaryTerm,
		},
		{
			Name:        "get_erd_document",
			Description: "ERD 초안의 스키마(테이블·컬럼·관계)와 대상 DB와의 차이를 반환한다.",
			Schema: objectSchema(map[string]any{
				"documentId": str("초안 ID"),
			}, "documentId"),
			Run: toolGetERDDocument,
		},
		{
			Name:        "list_migrations",
			Description: "마이그레이션 목록을 상태와 함께 반환한다.",
			Schema: objectSchema(map[string]any{
				"status": str("draft, in_review, approved, applied, rolled_back, failed"),
				"limit":  num("최대 개수 (기본 20)"),
			}),
			Run: toolListMigrations,
		},
		{
			Name:        "get_migration",
			Description: "마이그레이션의 변경 목록, up/down SQL, 리뷰 상태, 실행 기록을 반환한다.",
			Schema: objectSchema(map[string]any{
				"migrationId": str("마이그레이션 ID"),
			}, "migrationId"),
			Run: toolGetMigration,
		},
		{
			Name:        "list_versions",
			Description: "커넥션의 확정된 스키마 버전 이력을 반환한다 (외부 편집 포함).",
			Schema: objectSchema(map[string]any{
				"connection": str("커넥션 이름 또는 ID"),
				"limit":      num("최대 개수 (기본 20)"),
			}, "connection"),
			Run: toolListVersions,
		},

		// ---------- 쓰기 (제안 → 사용자 승인 후 실행) ----------
		{
			Name: "create_erd_document",
			Description: "새 ERD 초안을 만든다. fromConnection이 true면 대상 DB의 현재 스키마를 가져와 " +
				"시작한다. 사용자 승인이 필요하다.",
			Schema: objectSchema(map[string]any{
				"name":           str("초안 이름"),
				"connection":     str("대상 커넥션 이름 또는 ID"),
				"fromConnection": boolp("현재 스키마를 가져와 시작 (기본 true)"),
			}, "name", "connection"),
			Mutating: true,
			Propose:  proposeCreateERD,
			Apply:    applyCreateERD,
		},
		{
			Name: "create_migration",
			Description: "ERD 초안과 대상 DB의 차이로 마이그레이션 계획을 만든다. 실행하지는 않는다. " +
				"사용자 승인이 필요하다.",
			Schema: objectSchema(map[string]any{
				"documentId": str("초안 ID"),
				"title":      str("마이그레이션 제목"),
			}, "documentId"),
			Mutating: true,
			Propose:  proposeCreateMigration,
			Apply:    applyCreateMigration,
		},
		{
			Name:        "request_review",
			Description: "마이그레이션을 리뷰 요청 상태로 바꾼다. 사용자 승인이 필요하다.",
			Schema: objectSchema(map[string]any{
				"migrationId": str("마이그레이션 ID"),
			}, "migrationId"),
			Mutating: true,
			Propose:  proposeRequestReview,
			Apply:    applyRequestReview,
		},
		{
			Name: "apply_migration",
			Description: "승인된 마이그레이션을 대상 DB에 실행한다. 사용자 승인이 필요하며, " +
				"승인 후에도 앱의 기존 안전장치(검토자 승인 수, 사전 검사, 운영 DB 확인)가 모두 적용된다.",
			Schema: objectSchema(map[string]any{
				"migrationId": str("마이그레이션 ID"),
			}, "migrationId"),
			Mutating: true,
			Propose:  proposeApplyMigration,
			Apply:    applyApplyMigration,
		},
		{
			Name:        "capture_version",
			Description: "커넥션의 현재 스키마를 버전으로 확정한다 (외부 편집 기록). 사용자 승인이 필요하다.",
			Schema: objectSchema(map[string]any{
				"connection": str("커넥션 이름 또는 ID"),
				"note":       str("메모"),
			}, "connection"),
			Mutating: true,
			Propose:  proposeCaptureVersion,
			Apply:    applyCaptureVersion,
		},
		{
			Name: "push_migration",
			Description: "마이그레이션을 Git 저장소의 브랜치에 커밋하고 PR/MR을 만든다. " +
				"사용자 승인이 필요하다.",
			Schema: objectSchema(map[string]any{
				"migrationId": str("마이그레이션 ID"),
				"integration": str("Git 연동 이름 또는 ID (생략하면 사용 가능한 첫 연동)"),
			}, "migrationId"),
			Mutating: true,
			Propose:  proposePushMigration,
			Apply:    applyPushMigration,
		},
	}

	// P11~P12에서 더한 기능의 툴은 별도 파일에 있다. 목록이 한 함수 안에서
	// 계속 길어지면 무엇이 있는지 훑는 것 자체가 일이 된다.
	list = append(list, dataTools()...)
	list = append(list, infraTools()...)
	// ERD 초안을 고치는 툴. ERD 화면 안의 대화가 쓰는 것과 **같은 구현**이고,
	// 여기서는 "어느 초안인가"(document)를 인자로 받는다(ai_tools_erddoc.go).
	list = append(list, erdDocTools()...)

	out := make(map[string]*aiTool, len(list))
	for _, t := range list {
		out[t.Name] = t
	}
	return out
}

// availableTools는 이 사용자에게 노출할 툴 목록을 만든다.
//
// 권한이 없는 툴을 아예 보여주지 않는 이유: 모델이 호출했다가 거부되는 것보다
// 처음부터 없는 것이 대화가 깔끔하고 토큰도 덜 쓴다. 물론 실행 시점에도 다시
// 검사한다 — 노출 여부는 편의이고, 실제 방어는 실행 시점 판정이다.
// toolHints는 목록을 거르는 데 필요한 사용자 요약이다.
//
// 능력은 커넥션마다 다르므로 "어디에서든 가지고 있는가"만 본다. 정확한 판정은
// 툴을 실제로 부를 때 커넥션별로 한다 — 이 값은 목록을 다듬는 편의일 뿐이고,
// 조작해도 권한이 늘어나지 않는다.
type toolHints struct {
	Caps map[model.Capability]bool
}

func (s *Server) toolHints(c *fiber.Ctx, u *model.User) toolHints {
	hints := toolHints{Caps: map[model.Capability]bool{}}
	policy, err := s.st.GetAccessPolicy(c.Context(), u.ID)
	if err != nil {
		return hints
	}
	for _, cap := range model.AllCapabilities() {
		hints.Caps[cap] = hasAnyCap(u, policy, cap)
	}
	return hints
}

func availableTools(u *model.User, hints toolHints) ([]ai.Tool, map[string]*aiTool) {
	registry := aiTools()
	names := make([]string, 0, len(registry))
	for name := range registry {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]ai.Tool, 0, len(names))
	for _, name := range names {
		t := registry[name]
		if t.SuperadminOnly && (u == nil || !u.Role.CanManageUsers()) {
			continue
		}
		if t.ConnManagerOnly && (u == nil || !u.Role.CanManageConnections()) {
			continue
		}
		if t.RequiresPerm != "" && !u.HasPerm(t.RequiresPerm) {
			continue
		}
		if t.RequiresCap != "" && !hints.Caps[t.RequiresCap] {
			continue
		}
		out = append(out, ai.Tool{
			Name: t.Name, Description: t.Description, Schema: t.Schema,
		})
	}
	return out, registry
}

// resolveConnCap은 커넥션을 찾고 **데이터 능력**을 확인한다.
// resolveConn(등급)과 짝을 이루며, 두 축 어느 쪽이든 판정은 authz를 통과한다.
func (tc *toolContext) resolveConnCap(nameOrID string, need model.Capability) (*model.Connection, error) {
	conn, err := tc.findConn(nameOrID)
	if err != nil {
		return nil, err
	}
	d, err := tc.srv.authz.CanCap(tc.ctx, tc.user, conn.ID, need)
	if err != nil {
		return nil, err
	}
	if !d.Allowed {
		tc.audit("ai.tool.denied", "connection", conn.ID, "denied", map[string]any{
			"connection": conn.Name, "need": need, "reason": d.Reason,
		})
		return nil, fmt.Errorf("%s 에 대한 %s 권한이 없습니다: %s", conn.Name, need.Label(), d.Reason)
	}
	if !conn.Enabled {
		return nil, fmt.Errorf("%s 은(는) 비활성화된 커넥션입니다", conn.Name)
	}
	return conn, nil
}

// findConn은 권한 검사 없이 이름/ID로 커넥션을 찾는다.
// 권한은 부르는 쪽(resolveConn / resolveConnCap)이 반드시 확인한다.
func (tc *toolContext) findConn(nameOrID string) (*model.Connection, error) {
	nameOrID = strings.TrimSpace(nameOrID)
	if nameOrID == "" {
		return nil, errors.New("커넥션을 지정하세요")
	}
	conns, err := tc.srv.st.ListConnections(tc.ctx)
	if err != nil {
		return nil, err
	}
	for _, c := range conns {
		if c.ID == nameOrID || strings.EqualFold(c.Name, nameOrID) {
			return c, nil
		}
	}
	return nil, fmt.Errorf("커넥션 %q 을(를) 찾을 수 없습니다. list_connections로 목록을 확인하세요", nameOrID)
}

// target은 툴이 대상 DB에 접속하는 데 필요한 값을 만든다.
func (tc *toolContext) target(conn *model.Connection) (dbx.Target, error) {
	secret, err := tc.srv.st.GetSecret(tc.ctx, conn.ID)
	if err != nil {
		return dbx.Target{}, err
	}
	return dbx.Target{Conn: conn, Secret: secret}, nil
}

// ---------- 공용 헬퍼 ----------

// resolveConn은 이름 또는 ID로 커넥션을 찾고 권한을 확인한다.
//
// 이름으로도 찾을 수 있게 하는 이유: 모델은 사용자의 말("운영 DB")을 그대로 넘기는
// 경향이 있고, ID를 요구하면 list_connections를 반드시 먼저 부르게 만들어 왕복이 늘어난다.
func (tc *toolContext) resolveConn(nameOrID string, need model.Level) (*model.Connection, error) {
	nameOrID = strings.TrimSpace(nameOrID)
	if nameOrID == "" {
		return nil, errors.New("커넥션을 지정하세요")
	}
	conns, err := tc.srv.st.ListConnections(tc.ctx)
	if err != nil {
		return nil, err
	}
	var found *model.Connection
	for _, c := range conns {
		if c.ID == nameOrID || strings.EqualFold(c.Name, nameOrID) {
			found = c
			break
		}
	}
	if found == nil {
		return nil, fmt.Errorf("커넥션 %q 을(를) 찾을 수 없습니다. list_connections로 목록을 확인하세요", nameOrID)
	}

	// 권한 판정: 사용자 요청과 완전히 같은 경로를 쓴다.
	d, err := tc.srv.authz.Can(tc.ctx, tc.user, found.ID, need)
	if err != nil {
		return nil, err
	}
	if !d.Allowed {
		// 감사 로그에 남긴다. AI를 통한 접근 시도도 사람의 시도와 같이 추적되어야 한다.
		tc.audit("ai.tool.denied", "connection", found.ID, "denied", map[string]any{
			"connection": found.Name, "need": need, "reason": d.Reason,
		})
		return nil, fmt.Errorf("%s 에 대한 권한이 없습니다: %s", found.Name, d.Reason)
	}
	return found, nil
}

// audit은 fiber 컨텍스트 없이 감사 로그를 남긴다.
func (tc *toolContext) audit(action, targetType, targetID, result string, detail map[string]any) {
	if detail == nil {
		detail = map[string]any{}
	}
	if tc.session != nil {
		detail["aiSession"] = tc.session.ID
	}
	p := store.AuditParams{
		Action: action, TargetType: targetType, TargetID: targetID,
		Result: result, Detail: detail, IP: tc.ip,
	}
	if tc.user != nil {
		p.ActorID, p.ActorName = tc.user.ID, tc.user.Username
	}
	if err := tc.srv.st.Audit(tc.ctx, p); err != nil {
		// 감사 실패가 툴 실행을 막을 이유는 없다.
		_ = err
	}
}

// accessibleConns는 사용자가 need 등급 이상으로 볼 수 있는 커넥션을 반환한다.
func (tc *toolContext) accessibleConns(need model.Level) ([]*model.Connection, map[string]model.Level, error) {
	all, err := tc.srv.st.ListConnections(tc.ctx)
	if err != nil {
		return nil, nil, err
	}
	return tc.srv.authz.FilterAccessible(tc.ctx, tc.user, all, need)
}

// parseArgs는 툴 인자를 구조체로 디코딩한다.
//
// 알 수 없는 필드를 거부하지 않는 이유(erd 패키지와 반대): 모델은 스키마에 없는
// 필드를 덧붙이는 일이 흔하고, 그때 툴 전체를 실패시키면 대화가 막힌다.
// 대신 필요한 필드가 없으면 그것을 명확히 알려 모델이 고쳐 부를 수 있게 한다.
func parseArgs(args json.RawMessage, dst any) error {
	if len(strings.TrimSpace(string(args))) == 0 {
		args = json.RawMessage("{}")
	}
	if err := json.Unmarshal(args, dst); err != nil {
		return fmt.Errorf("인자를 해석할 수 없습니다: %v", err)
	}
	return nil
}

// asJSON은 툴 결과를 JSON 문자열로 만든다.
//
// 결과를 JSON으로 주는 이유: 모델이 구조를 정확히 읽고 후속 툴 호출의 인자로
// 옮기기 쉽다. 사람이 읽을 문장은 모델이 이 데이터를 보고 직접 만든다.
func asJSON(v any) (string, error) {
	data, err := json.MarshalIndent(v, "", " ")
	if err != nil {
		return "", fmt.Errorf("결과를 직렬화할 수 없습니다: %w", err)
	}
	// 툴 결과가 지나치게 크면 컨텍스트를 다 먹는다. 상한을 두고 잘렸음을 알린다.
	//
	// 여기서 잘린 결과는 **JSON으로서 깨져 있다**. 그러니 이 자리에 오는 것 자체가
	// 마지막 방어선이지 정상 경로가 아니다 — 툴은 인자로 범위를 좁힐 수 있어야 하고
	// (read_schema의 offset처럼), 스스로 먼저 멈춰 온전한 JSON을 주어야 한다.
	const maxResult = 24_000
	if len(data) > maxResult {
		// 글자 중간에서 자르지 않는다. UTF-8 한 글자가 반 토막 나면 그 바이트는
		// U+FFFD로 바뀌어 나가고, 한국어 주석이 있는 스키마에서는 매번 그렇게 된다.
		cut := maxResult
		for cut > 0 && !utf8.RuneStart(data[cut]) {
			cut--
		}
		return string(data[:cut]) + fmt.Sprintf(
			"\n… (전체 %d바이트 중 %d바이트에서 잘렸습니다. 여기까지는 JSON이 완결되지 "+
				"않았으니 그대로 믿지 마세요. 범위를 좁히는 인자(table·offset·limit 등)가 "+
				"있으면 나눠서 다시 부르세요)", len(data), cut), nil
	}
	return string(data), nil
}

// ---------- 읽기 툴 구현 ----------

func toolListConnections(tc *toolContext, args json.RawMessage) (string, error) {
	var in struct {
		Environment string `json:"environment"`
	}
	if err := parseArgs(args, &in); err != nil {
		return "", err
	}
	conns, levels, err := tc.accessibleConns(model.LevelMonitor)
	if err != nil {
		return "", err
	}
	type item struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		Kind        string `json:"kind"`
		Environment string `json:"environment"`
		Level       string `json:"level"`
		Enabled     bool   `json:"enabled"`
	}
	out := []item{}
	for _, c := range conns {
		if in.Environment != "" && !strings.EqualFold(string(c.Environment), in.Environment) {
			continue
		}
		out = append(out, item{
			ID: c.ID, Name: c.Name, Kind: string(c.Kind),
			Environment: string(c.Environment), Level: string(levels[c.ID]),
			Enabled: c.Enabled,
		})
	}
	return asJSON(map[string]any{
		"connections": out,
		"note":        "level은 이 사용자의 권한 등급이다: monitor < erd < migrate",
	})
}

func toolConnectionStatus(tc *toolContext, args json.RawMessage) (string, error) {
	var in struct {
		Connection string `json:"connection"`
	}
	if err := parseArgs(args, &in); err != nil {
		return "", err
	}
	conn, err := tc.resolveConn(in.Connection, model.LevelMonitor)
	if err != nil {
		return "", err
	}
	state, err := tc.srv.st.GetConnectionState(tc.ctx, conn.ID)
	if err != nil {
		return "", err
	}
	events, _, err := tc.srv.st.ListEvents(tc.ctx, store.EventFilter{
		ConnectionIDs: []string{conn.ID}, State: "open", Limit: 20,
	})
	if err != nil {
		return "", err
	}
	openBySeverity := map[string]int{}
	for _, e := range events {
		openBySeverity[string(e.Severity)]++
	}
	return asJSON(map[string]any{
		"connection":   connSummaryMap(conn),
		"state":        state,
		"openEvents":   openBySeverity,
		"recentIssues": eventSummaries(events, 5),
	})
}

func toolGetMetrics(tc *toolContext, args json.RawMessage) (string, error) {
	var in struct {
		Connection string `json:"connection"`
		Metric     string `json:"metric"`
		Range      string `json:"range"`
	}
	if err := parseArgs(args, &in); err != nil {
		return "", err
	}
	conn, err := tc.resolveConn(in.Connection, model.LevelMonitor)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(in.Metric) == "" {
		names, err := tc.srv.st.ListMetricNames(tc.ctx, conn.ID)
		if err != nil {
			return "", err
		}
		return asJSON(map[string]any{
			"connection":       conn.Name,
			"availableMetrics": names,
			"note":             "metric 인자에 이름을 넣어 다시 호출하면 시계열을 반환한다",
		})
	}

	dur := parseRangeDuration(in.Range, time.Hour)
	to := time.Now().UTC()
	from := to.Add(-dur)
	series, err := tc.srv.st.QuerySeries(tc.ctx, store.SeriesQuery{
		ConnectionID: conn.ID, Metrics: []string{in.Metric},
		From: from, To: to, MaxPoints: 120,
	})
	if err != nil {
		return "", err
	}
	// 시계열 전체를 넘기면 컨텍스트를 다 먹는다. 요약과 최근 값만 준다.
	return asJSON(map[string]any{
		"connection": conn.Name,
		"metric":     in.Metric,
		"range":      dur.String(),
		"summary":    summarizeSeries(series),
	})
}

func toolListEvents(tc *toolContext, args json.RawMessage) (string, error) {
	var in struct {
		State    string `json:"state"`
		Severity string `json:"severity"`
		Limit    int    `json:"limit"`
	}
	if err := parseArgs(args, &in); err != nil {
		return "", err
	}
	conns, _, err := tc.accessibleConns(model.LevelMonitor)
	if err != nil {
		return "", err
	}
	// 빈 슬라이스는 "접근 가능한 커넥션 없음"이다. nil을 넘기면 전체가 노출된다.
	ids := make([]string, 0, len(conns))
	names := map[string]string{}
	for _, c := range conns {
		ids = append(ids, c.ID)
		names[c.ID] = c.Name
	}
	state := in.State
	if state == "" {
		state = "open"
	}
	limit := in.Limit
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	events, total, err := tc.srv.st.ListEvents(tc.ctx, store.EventFilter{
		ConnectionIDs: ids, State: state,
		Severity: store.Severity(in.Severity), Limit: limit,
	})
	if err != nil {
		return "", err
	}
	type item struct {
		ID          int64  `json:"id"`
		Connection  string `json:"connection"`
		Severity    string `json:"severity"`
		Kind        string `json:"kind"`
		Message     string `json:"message"`
		StartedAt   string `json:"startedAt"`
		Occurrences int    `json:"occurrences"`
	}
	out := []item{}
	for _, e := range events {
		out = append(out, item{
			ID: e.ID, Connection: names[e.ConnectionID], Severity: string(e.Severity),
			Kind: e.Kind, Message: e.Message,
			StartedAt: e.StartedAt.Format(time.RFC3339), Occurrences: e.Occurrences,
		})
	}
	return asJSON(map[string]any{"events": out, "total": total})
}

func toolSearchLogs(tc *toolContext, args json.RawMessage) (string, error) {
	var in struct {
		Connection  string  `json:"connection"`
		Q           string  `json:"q"`
		MinDuration float64 `json:"minDuration"`
		Range       string  `json:"range"`
		Limit       int     `json:"limit"`
	}
	if err := parseArgs(args, &in); err != nil {
		return "", err
	}
	conn, err := tc.resolveConn(in.Connection, model.LevelMonitor)
	if err != nil {
		return "", err
	}
	adapter, err := dbx.Get(conn.Kind)
	if err != nil {
		return "", err
	}
	if !adapter.Capabilities().Logs {
		return "", fmt.Errorf("%s 는 로그 조회를 지원하지 않습니다", conn.Kind)
	}
	secret, err := tc.srv.st.GetSecret(tc.ctx, conn.ID)
	if err != nil {
		return "", err
	}
	limit := in.Limit
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	dur := parseRangeDuration(in.Range, time.Hour)
	to := time.Now().UTC()
	f := &dblog.Filter{
		From: to.Add(-dur), To: to, Search: in.Q,
		MinDurationMs: in.MinDuration, Limit: limit,
	}
	f.Normalize()

	ctx, cancel := context.WithTimeout(tc.ctx, logQueryTimeout)
	defer cancel()
	res, err := adapter.Logs(ctx, dbx.Target{Conn: conn, Secret: secret}, f)
	if err != nil {
		return "", err
	}

	type entry struct {
		Time     string  `json:"time"`
		Severity string  `json:"severity"`
		Source   string  `json:"source"`
		Message  string  `json:"message"`
		Duration float64 `json:"durationMs,omitempty"`
	}
	entries := []entry{}
	for i, e := range res.Entries {
		if i >= limit {
			break
		}
		entries = append(entries, entry{
			Time: e.At.Format(time.RFC3339), Severity: string(e.Severity),
			Source: string(e.Source), Message: dblog.TruncateQuery(e.Message, 500),
			Duration: e.DurationMs,
		})
	}
	type stat struct {
		Query   string  `json:"query"`
		Calls   int64   `json:"calls"`
		TotalMs float64 `json:"totalMs"`
		MeanMs  float64 `json:"meanMs"`
	}
	stats := []stat{}
	for i, s := range res.Stats {
		if i >= 10 {
			break
		}
		stats = append(stats, stat{
			Query: dblog.TruncateQuery(s.Normalized, 400),
			Calls: s.Calls, TotalMs: s.TotalMs, MeanMs: s.MeanMs,
		})
	}
	unavailable := []map[string]string{}
	for _, s := range res.Sources {
		if !s.Available {
			unavailable = append(unavailable, map[string]string{
				"source": string(s.Kind), "reason": s.Hint,
			})
		}
	}
	return asJSON(map[string]any{
		"connection":         conn.Name,
		"entries":            entries,
		"topQueries":         stats,
		"unavailableSources": unavailable,
		"notes":              res.Notes,
	})
}

func toolIntrospectSchema(tc *toolContext, args json.RawMessage) (string, error) {
	var in struct {
		Connection string `json:"connection"`
		Table      string `json:"table"`
	}
	if err := parseArgs(args, &in); err != nil {
		return "", err
	}
	conn, err := tc.resolveConn(in.Connection, model.LevelMonitor)
	if err != nil {
		return "", err
	}
	sc, err := tc.introspect(conn)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(in.Table) != "" {
		tbl := findTableByName(sc, in.Table)
		if tbl == nil {
			return "", fmt.Errorf("테이블 %q 을(를) 찾을 수 없습니다", in.Table)
		}
		return asJSON(map[string]any{
			"connection": conn.Name, "dialect": sc.Dialect, "table": tbl,
		})
	}
	return asJSON(map[string]any{
		"connection": conn.Name, "dialect": sc.Dialect, "shape": sc.Shape,
		"stats": sc.Stats(), "tables": tableSummaries(sc),
		"views": sc.Views, "enums": sc.Enums, "notes": sc.Notes,
	})
}

// introspect는 대상 DB의 스키마를 읽는다.
func (tc *toolContext) introspect(conn *model.Connection) (*schema.Schema, error) {
	adapter, err := dbx.Get(conn.Kind)
	if err != nil {
		return nil, err
	}
	if !adapter.Capabilities().Introspect {
		return nil, fmt.Errorf("%s 는 스키마 조회를 지원하지 않습니다", conn.Kind)
	}
	secret, err := tc.srv.st.GetSecret(tc.ctx, conn.ID)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(tc.ctx, introspectTimeout)
	defer cancel()
	return adapter.Introspect(ctx, dbx.Target{Conn: conn, Secret: secret})
}

// toolExploreNoSQL은 Mongo/Redis 특화 조회 결과를 요약해 돌려준다.
//
// 전체 결과를 그대로 주면 컬렉션이 많은 DB에서 응답 상한(asJSON)에 걸려 뒤가 잘린다.
// 모델이 판단에 쓰는 것은 규모와 이상 신호이므로, 필드 목록처럼 긴 부분은 접어서 보낸다.
func toolExploreNoSQL(tc *toolContext, args json.RawMessage) (string, error) {
	var in struct {
		Connection string `json:"connection"`
	}
	if err := parseArgs(args, &in); err != nil {
		return "", err
	}
	conn, err := tc.resolveConn(in.Connection, model.LevelMonitor)
	if err != nil {
		return "", err
	}
	adapter, err := dbx.Get(conn.Kind)
	if err != nil {
		return "", err
	}
	if !adapter.Capabilities().Explore {
		return "", fmt.Errorf("%s 는 전용 탐색을 지원하지 않습니다. introspect_schema를 쓰세요", conn.Kind)
	}
	secret, err := tc.srv.st.GetSecret(tc.ctx, conn.ID)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(tc.ctx, exploreTimeout)
	defer cancel()

	res, err := dbx.DoExplore(ctx, dbx.Target{Conn: conn, Secret: secret})
	if err != nil {
		return "", err
	}
	out := map[string]any{
		"connection": conn.Name, "kind": conn.Kind,
		"shape": res.Shape, "notes": res.Notes,
	}
	if d := res.Document; d != nil {
		colls := make([]map[string]any, 0, len(d.Collections))
		for _, c := range d.Collections {
			unused := []string{}
			for _, idx := range c.Indexes {
				// 사용 횟수를 읽을 수 있었고 0인 인덱스만 "쓰이지 않는다"고 말할 수 있다.
				if idx.Ops != nil && *idx.Ops == 0 && idx.Name != "_id_" {
					unused = append(unused, idx.Name)
				}
			}
			colls = append(colls, map[string]any{
				"name": c.Name, "type": c.Type, "documents": c.Documents,
				"dataSize": c.DataSize, "storageSize": c.StorageSize, "indexSize": c.IndexSize,
				"indexes": len(c.Indexes), "unusedIndexes": unused,
				"fields": len(c.Fields), "sampled": c.Sampled,
				"optionalFields": optionalFieldPaths(c.Fields),
			})
		}
		out["database"] = d.Database
		out["server"] = d.Server
		out["stats"] = d.Stats
		out["collections"] = colls
	}
	if k := res.Keyspace; k != nil {
		out["server"] = k.Server
		out["memory"] = k.Memory
		out["persistence"] = k.Persistence
		out["stats"] = k.Stats
		out["databases"] = k.Databases
		out["scannedKeys"] = k.Scanned
		out["truncated"] = k.Truncated
		groups := make([]map[string]any, 0, len(k.Groups))
		for i, g := range k.Groups {
			if i >= 30 {
				break
			}
			groups = append(groups, map[string]any{
				"prefix": g.Prefix, "keys": g.Keys, "types": g.Types,
				"withTtl": g.WithTTL, "bytes": g.Bytes,
			})
		}
		out["groups"] = groups
		out["bigKeys"] = k.BigKeys
		out["commands"] = k.Commands
	}
	return asJSON(out)
}

// optionalFieldPaths는 일부 문서에만 존재하는 필드를 알려준다.
// 스키마가 강제되지 않는 DB에서 이것이 버그의 단서가 되는 경우가 많다.
func optionalFieldPaths(fields []*dbx.DocumentField) []string {
	out := []string{}
	for _, f := range fields {
		if f.Presence < 1 || f.Mixed {
			out = append(out, fmt.Sprintf("%s (%.0f%%%s)", f.Path, f.Presence*100,
				map[bool]string{true: ", 혼합 타입", false: ""}[f.Mixed]))
		}
		if len(out) >= 20 {
			break
		}
	}
	return out
}

func toolDiffSchema(tc *toolContext, args json.RawMessage) (string, error) {
	var in struct {
		From string `json:"from"`
		To   string `json:"to"`
	}
	if err := parseArgs(args, &in); err != nil {
		return "", err
	}
	fromConn, err := tc.resolveConn(in.From, model.LevelMonitor)
	if err != nil {
		return "", err
	}
	toConn, err := tc.resolveConn(in.To, model.LevelMonitor)
	if err != nil {
		return "", err
	}
	fromSchema, err := tc.introspect(fromConn)
	if err != nil {
		return "", err
	}
	toSchema, err := tc.introspect(toConn)
	if err != nil {
		return "", err
	}
	diff := schema.Diff(fromSchema, toSchema)
	plan := schema.BuildPlan(fromSchema.Dialect, diff)
	return asJSON(map[string]any{
		"from": fromConn.Name, "to": toConn.Name,
		"changes":     changeList(diff),
		"destructive": diff.DestructiveCount,
		"upSql":       plan.UpSQL(),
		"warnings":    plan.Warnings,
	})
}

// projectScope는 이 사람이 볼 수 있는 프로젝트 아이디를 준다(슈퍼 어드민은 nil).
//
// 툴도 화면과 같은 관문을 지나야 한다. 여기서만 전체를 돌려주면 AI가 권한 우회
// 통로가 된다 — 그것이 이 파일 전체의 규칙이다.
func (tc *toolContext) projectScope() []string {
	if tc.user == nil {
		return []string{}
	}
	if tc.user.Role == model.RoleSuperadmin {
		return nil
	}
	ids, err := tc.srv.st.ProjectIDsForUser(tc.ctx, tc.user.ID)
	if err != nil || ids == nil {
		// 읽지 못했으면 아무것도 보여주지 않는다. 실패가 권한을 넓히면 안 된다.
		return []string{}
	}
	return ids
}

func toolListERDDocuments(tc *toolContext, args json.RawMessage) (string, error) {
	conns, _, err := tc.accessibleConns(model.LevelMonitor)
	if err != nil {
		return "", err
	}
	ids := make([]string, 0, len(conns))
	names := map[string]string{}
	for _, c := range conns {
		ids = append(ids, c.ID)
		names[c.ID] = c.Name
	}
	// 독립 초안은 커넥션으로 걸러지지 않으므로 프로젝트로 좁힌다.
	docs, err := tc.srv.st.ListERDDocuments(tc.ctx, ids, tc.projectScope(), 50)
	if err != nil {
		return "", err
	}
	type item struct {
		ID         string `json:"id"`
		Name       string `json:"name"`
		Connection string `json:"connection"`
		Status     string `json:"status"`
		Tables     int    `json:"tables"`
		UpdatedAt  string `json:"updatedAt"`
	}
	out := []item{}
	for _, d := range docs {
		out = append(out, item{
			ID: d.ID, Name: d.Name, Connection: names[d.ConnectionID],
			Status: d.Status, Tables: d.TableCount,
			UpdatedAt: d.UpdatedAt.Format(time.RFC3339),
		})
	}
	return asJSON(map[string]any{"documents": out})
}

func toolGetERDDocument(tc *toolContext, args json.RawMessage) (string, error) {
	var in struct {
		DocumentID string `json:"documentId"`
	}
	if err := parseArgs(args, &in); err != nil {
		return "", err
	}
	doc, conn, err := tc.resolveDoc(in.DocumentID, model.LevelMonitor)
	if err != nil {
		return "", err
	}
	current, ierr := tc.introspect(conn)
	result := map[string]any{
		"document": map[string]any{
			"id": doc.ID, "name": doc.Name, "status": doc.Status,
			"connection": conn.Name, "dialect": doc.Dialect, "seq": doc.Seq,
		},
		"schema": map[string]any{
			"stats": doc.Schema.Stats(), "tables": tableSummaries(doc.Schema),
		},
	}
	if ierr == nil {
		diff := schema.Diff(current, doc.Schema)
		result["diffAgainstLiveDb"] = map[string]any{
			"changes": changeList(diff), "destructive": diff.DestructiveCount,
		}
	} else {
		result["diffAgainstLiveDb"] = map[string]any{"error": ierr.Error()}
	}
	return asJSON(result)
}

// resolveDoc은 ERD 초안을 찾고 대상 커넥션 권한을 확인한다.
func (tc *toolContext) resolveDoc(docID string, need model.Level) (*erd.Document, *model.Connection, error) {
	docID = strings.TrimSpace(docID)
	if docID == "" {
		return nil, nil, errors.New("초안 ID를 지정하세요")
	}
	doc, err := tc.srv.st.GetERDDocument(tc.ctx, docID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil, fmt.Errorf("초안 %q 을(를) 찾을 수 없습니다", docID)
	}
	if err != nil {
		return nil, nil, err
	}
	conn, err := tc.resolveConn(doc.ConnectionID, need)
	if err != nil {
		return nil, nil, err
	}
	return doc, conn, nil
}

func toolListMigrations(tc *toolContext, args json.RawMessage) (string, error) {
	var in struct {
		Status string `json:"status"`
		Limit  int    `json:"limit"`
	}
	if err := parseArgs(args, &in); err != nil {
		return "", err
	}
	conns, _, err := tc.accessibleConns(model.LevelMonitor)
	if err != nil {
		return "", err
	}
	ids := make([]string, 0, len(conns))
	names := map[string]string{}
	for _, c := range conns {
		ids = append(ids, c.ID)
		names[c.ID] = c.Name
	}
	limit := in.Limit
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	migs, err := tc.srv.st.ListMigrations(tc.ctx, ids, in.Status, limit)
	if err != nil {
		return "", err
	}
	type item struct {
		ID          string `json:"id"`
		Title       string `json:"title"`
		Connection  string `json:"connection"`
		Status      string `json:"status"`
		Changes     int    `json:"changes"`
		Destructive int    `json:"destructive"`
		UpdatedAt   string `json:"updatedAt"`
	}
	out := []item{}
	for _, m := range migs {
		changes := 0
		if m.Diff != nil {
			changes = len(m.Diff.Changes)
		}
		out = append(out, item{
			ID: m.ID, Title: m.Title, Connection: names[m.ConnectionID],
			Status: m.Status, Changes: changes, Destructive: m.DestructiveCount,
			UpdatedAt: m.UpdatedAt.Format(time.RFC3339),
		})
	}
	return asJSON(map[string]any{"migrations": out})
}

func toolGetMigration(tc *toolContext, args json.RawMessage) (string, error) {
	var in struct {
		MigrationID string `json:"migrationId"`
	}
	if err := parseArgs(args, &in); err != nil {
		return "", err
	}
	mig, conn, err := tc.resolveMig(in.MigrationID, model.LevelMonitor)
	if err != nil {
		return "", err
	}
	reviews := []map[string]string{}
	for _, r := range mig.Reviews {
		reviews = append(reviews, map[string]string{
			"reviewer": r.ReviewerName, "decision": r.Decision, "comment": r.Comment,
		})
	}
	return asJSON(map[string]any{
		"id": mig.ID, "title": mig.Title, "connection": conn.Name,
		"status": mig.Status, "fromVersion": mig.FromVersionNo, "toVersion": mig.ToVersionNo,
		"changes":           changeList(mig.Diff),
		"destructive":       mig.DestructiveCount,
		"irreversible":      mig.Irreversible,
		"upSql":             mig.UpSQL,
		"downSql":           mig.DownSQL,
		"reviews":           reviews,
		"requiredApprovals": migrate.RequiredApprovals(conn, mig.DestructiveCount),
		"approvals":         store.ApprovalCount(mig.Reviews),
		"appliedStatements": mig.AppliedStatements,
		"error":             mig.Error,
	})
}

// resolveMig은 마이그레이션을 찾고 대상 커넥션 권한을 확인한다.
func (tc *toolContext) resolveMig(migID string, need model.Level) (*store.Migration, *model.Connection, error) {
	migID = strings.TrimSpace(migID)
	if migID == "" {
		return nil, nil, errors.New("마이그레이션 ID를 지정하세요")
	}
	mig, err := tc.srv.st.GetMigration(tc.ctx, migID, true)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil, fmt.Errorf("마이그레이션 %q 을(를) 찾을 수 없습니다", migID)
	}
	if err != nil {
		return nil, nil, err
	}
	conn, err := tc.resolveConn(mig.ConnectionID, need)
	if err != nil {
		return nil, nil, err
	}
	return mig, conn, nil
}

func toolListVersions(tc *toolContext, args json.RawMessage) (string, error) {
	var in struct {
		Connection string `json:"connection"`
		Limit      int    `json:"limit"`
	}
	if err := parseArgs(args, &in); err != nil {
		return "", err
	}
	conn, err := tc.resolveConn(in.Connection, model.LevelMonitor)
	if err != nil {
		return "", err
	}
	limit := in.Limit
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	versions, err := tc.srv.st.ListSchemaVersions(tc.ctx, conn.ID, limit)
	if err != nil {
		return "", err
	}
	type item struct {
		VersionNo int      `json:"versionNo"`
		Source    string   `json:"source"`
		Note      string   `json:"note,omitempty"`
		Author    string   `json:"author,omitempty"`
		CreatedAt string   `json:"createdAt"`
		Changes   []string `json:"changes,omitempty"`
	}
	out := []item{}
	for _, v := range versions {
		changes := v.ChangeSummary
		if len(changes) > 10 {
			changes = changes[:10]
		}
		out = append(out, item{
			VersionNo: v.VersionNo, Source: v.Source, Note: v.Note,
			Author: v.AuthorName, CreatedAt: v.CreatedAt.Format(time.RFC3339),
			Changes: changes,
		})
	}
	return asJSON(map[string]any{"connection": conn.Name, "versions": out})
}

// ---------- 표시 헬퍼 ----------

func connSummaryMap(conn *model.Connection) map[string]any {
	return map[string]any{
		"id": conn.ID, "name": conn.Name,
		"kind": conn.Kind, "environment": conn.Environment,
	}
}

func eventSummaries(events []*store.Event, max int) []string {
	out := []string{}
	for i, e := range events {
		if i >= max {
			break
		}
		out = append(out, fmt.Sprintf("[%s] %s", e.Severity, e.Message))
	}
	return out
}

func changeList(diff *schema.DiffResult) []map[string]any {
	if diff == nil {
		return []map[string]any{}
	}
	out := []map[string]any{}
	for i, c := range diff.Changes {
		if i >= 60 {
			out = append(out, map[string]any{
				"summary": fmt.Sprintf("… 그 외 %d건", len(diff.Changes)-60),
			})
			break
		}
		item := map[string]any{"kind": string(c.Kind), "summary": c.Summary}
		if c.Destructive {
			item["destructive"] = true
			if c.LossyDetail != "" {
				item["lossyDetail"] = c.LossyDetail
			}
		}
		out = append(out, item)
	}
	return out
}

func parseRangeDuration(raw string, def time.Duration) time.Duration {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "15m":
		return 15 * time.Minute
	case "1h":
		return time.Hour
	case "6h":
		return 6 * time.Hour
	case "24h", "1d":
		return 24 * time.Hour
	case "7d":
		return 7 * 24 * time.Hour
	case "30d":
		return 30 * 24 * time.Hour
	}
	return def
}

// summarizeSeries는 시계열을 요약한다.
//
// 전체 점을 모델에 넘기지 않는 이유: 컨텍스트를 다 먹으면서도 모델은 수백 개의
// 숫자에서 추세를 잘 읽지 못한다. 최신값·최소·최대·평균이 판단에 더 유용하다.
func summarizeSeries(series []store.Series) []map[string]any {
	out := []map[string]any{}
	for _, s := range series {
		if len(s.Points) == 0 {
			out = append(out, map[string]any{"metric": s.Metric, "points": 0})
			continue
		}
		first := s.Points[0]
		last := s.Points[len(s.Points)-1]
		minV, maxV, sum := first.Min, first.Max, 0.0
		for _, p := range s.Points {
			if p.Min < minV {
				minV = p.Min
			}
			if p.Max > maxV {
				maxV = p.Max
			}
			sum += p.Avg
		}
		out = append(out, map[string]any{
			"metric": s.Metric, "unit": s.Unit, "points": len(s.Points),
			"latest": last.Avg, "latestAt": last.Ts.Format(time.RFC3339),
			"min": minV, "max": maxV, "avg": sum / float64(len(s.Points)),
			"firstAt": first.Ts.Format(time.RFC3339), "source": s.Source,
		})
	}
	return out
}
