package api

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"

	"dbstudio/internal/ai"
	"dbstudio/internal/erd"
	"dbstudio/internal/model"
	"dbstudio/internal/schema"
)

// ERD 초안을 고치는 툴.
//
// 앱 전체의 툴 레지스트리(ai_tools*.go)와 **다른 한 벌**인 이유가 있다. 저쪽은
// "어느 DB를 볼까"부터 정해야 하는 넓은 도구 상자이고, 여기는 지금 열어 둔 문서
// 하나가 대상이다. 넓은 상자를 그대로 주면 모델이 커넥션을 찾는 데 왕복을 쓰고,
// 무엇보다 이 대화의 맥락(이 초안)을 벗어난 일을 하게 된다.
//
// **승인 단계를 두지 않는다.** 앱의 다른 쓰기 툴은 제안을 만들고 사람이 승인해야
// 실행되는데, 여기서는 셋이 다르기 때문이다.
//
//  1. 대상이 초안이다. 진짜 DB를 건드리는 단계(마이그레이션 실행)는 지금까지의
//     관문을 그대로 지난다 — 확인 문구, 검토자 승인, 사전 검사.
//  2. 모든 변경이 op-log에 남고 편집 이력에서 누가 무엇을 했는지 보인다.
//     같은 문서를 연 사람들의 화면에도 그 자리에서 나타난다.
//  3. 컬럼 다섯 개를 더하는 데 승인 다섯 번을 요구하면 아무도 쓰지 않는다.
//
// **지우는 툴은 컬럼 하나뿐이고, 그것만 승인을 거친다**(ai_tools_erd_delete.go).
// 더하기는 잘못돼도 그 자리에 남는 군더더기지만, 지우기는 그 컬럼에 딸린 것들(기본키
// 자리·인덱스·외래키)을 함께 데려간다. 표를 지우는 툴은 여전히 없다 — 표 하나가
// 사라지는 것은 그 표를 가리키는 모든 관계가 사라지는 일이고, 그 결정은 도면을 보면서
// 하는 편이 낫다.

// erdToolContext는 ERD 툴이 실행되는 문맥이다.
type erdToolContext struct {
	tc    *toolContext
	docID string
	// connID는 이 초안의 대상 커넥션이다(독립 초안이면 빈 문자열).
	// 편집 권한 판정에 쓴다.
	connID string
}

// erdCanEdit은 이 사용자가 그 초안을 고칠 수 있는지 본다.
//
// 대화를 시작할 때 한 번 확인했더라도 실행 시점에 다시 본다. 대화는 며칠 뒤에
// 이어질 수 있고, 그 사이에 권한이 회수될 수 있다 — 그때도 예전 대화창에서
// 툴이 도는 것은 곤란하다. 화면·소켓과 같은 판정(authz.Can)을 지난다.
func (s *Server) erdCanEdit(ec *erdToolContext, u *model.User) bool {
	// 독립 초안은 대상이 없으므로 등급을 물을 곳도 없다(문서 조회와 같은 규칙).
	if ec.connID == "" {
		return true
	}
	d, err := s.authz.Can(ec.tc.ctx, u, ec.connID, model.LevelERD)
	if err != nil {
		return false
	}
	return d.Allowed
}

// erdTool은 ERD 툴 하나다.
//
// 대개 Run 하나뿐이다(승인 없이 바로 반영된다). Mutating 인 툴만 승인을 거치며,
// 그때 Propose 가 "무엇이 사라지는지"를 만들고 Run 이 승인 뒤의 실행이 된다 —
// 실행 함수를 따로 두지 않는 이유는 승인 여부에 따라 하는 일이 달라져서는 안 되기
// 때문이다. 승인 화면에 적힌 그대로가 실행돼야 한다.
type erdTool struct {
	Name        string
	Description string
	Schema      map[string]any
	// Mutating이면 사용자 승인 뒤에 실행된다(지금은 컬럼 삭제 하나뿐이다).
	Mutating bool
	Propose  func(ec *erdToolContext, args json.RawMessage) (summary string, preview any, err error)
	Run      func(ec *erdToolContext, args json.RawMessage) (string, error)
}

func erdTools() map[string]*erdTool {
	list := []*erdTool{
		{
			Name: "read_schema",
			Description: "이 초안의 현재 스키마를 읽는다. 테이블·컬럼·기본키·외래키·인덱스와 " +
				"논리명을 반환한다. 무언가를 바꾸기 전에 먼저 부른다 — 지금 무엇이 있는지 " +
				"모르고 고치면 이름이 겹치거나 없는 것을 참조한다. " +
				"결과가 한 번에 다 들어가지 않으면 nextOffset을 알려주므로 offset을 바꿔 이어서 부른다.",
			Schema: objectSchema(map[string]any{
				"table":  str("이 테이블 하나만 자세히 본다 (생략하면 전체)"),
				"offset": num("몇 번째 테이블부터 읽을지 (기본 0). 이어 읽을 때 쓴다"),
				"limit":  num("최대 몇 개를 읽을지 (생략하면 들어가는 만큼)"),
			}),
			Run: erdReadSchema,
		},
		{
			Name: "list_domains",
			Description: "이 초안의 도메인(재사용 타입) 목록을 반환한다. " +
				"컬럼에 도메인을 붙이기 전에 무엇이 있는지 먼저 본다.",
			Schema: objectSchema(nil),
			Run:    erdListDomains,
		},
		{
			Name: "add_domain",
			Description: "도메인을 만든다. 도메인은 이름을 붙인 재사용 타입이다" +
				"(\"이메일은 VARCHAR(320)\", \"금액은 DECIMAL(18,2)\"). " +
				"같은 뜻의 컬럼이 표마다 다른 타입으로 만들어지는 것을 막는 장치이고, " +
				"정의를 고치면 그 도메인을 쓰는 컬럼이 함께 바뀐다. " +
				"DB에는 도메인이 만들어지지 않는다 — 컬럼에는 언제나 구체 타입이 함께 남는다.",
			Schema: objectSchema(map[string]any{
				"name":     str("도메인 이름 (예: email, money)"),
				"type":     str("DB 타입 (예: VARCHAR(320), DECIMAL(18,2))"),
				"nullable": boolp("이 도메인을 쓰는 컬럼의 NULL 허용 (생략하면 컬럼마다 다르게 둔다)"),
				"default":  str("기본값 식 (선택)"),
				"comment":  str("설명 (선택)"),
			}, "name", "type"),
			Run: erdAddDomain,
		},
		{
			Name: "update_domain",
			Description: "도메인의 이름·타입·NULL 허용·기본값·설명을 바꾼다. 바꿀 것만 넘긴다. " +
				"**이 도메인을 쓰는 컬럼들도 함께 바뀐다** — 그것이 도메인을 두는 이유다.",
			Schema: objectSchema(map[string]any{
				"name":     str("지금 도메인 이름"),
				"newName":  str("새 이름 (선택)"),
				"type":     str("새 타입 (선택)"),
				"nullable": boolp("NULL 허용 (선택)"),
				"clearNullable": boolp(
					"true면 NULL 허용을 도메인이 정하지 않게 되돌린다 (컬럼마다 다르게)"),
				"default": str("기본값 식 (선택)"),
				"comment": str("설명 (선택)"),
			}, "name"),
			Run: erdUpdateDomain,
		},
		{
			Name: "detach_domain",
			Description: "도메인을 지운다. 그 도메인을 쓰던 컬럼의 **타입은 그대로 두고 연결만 끊는다** — " +
				"타입까지 지우면 도메인 하나를 정리하려다 여러 컬럼이 타입을 잃는다.",
			Schema: objectSchema(map[string]any{
				"name": str("지울 도메인 이름"),
			}, "name"),
			Run: erdDeleteDomain,
		},
		{
			Name: "set_logical_names",
			Description: "테이블과 컬럼에 논리명(설계용 이름, 보통 한국어)을 붙인다. " +
				"여러 테이블을 한 번에 보낼 수 있다 — 표마다 따로 부르면 툴 왕복 횟수를 금방 넘긴다. " +
				"논리명은 DB에 만들어지지 않는 설계 메모여서 마이그레이션과 구조 지문에는 들어가지 않는다. " +
				"빈 문자열을 주면 그 논리명을 지운다.",
			Schema: objectSchema(map[string]any{
				"tables": map[string]any{
					"type":        "array",
					"description": "논리명을 붙일 테이블 목록",
					"items": objectSchema(map[string]any{
						"table":   str("테이블 이름"),
						"logical": str("테이블 논리명 (선택)"),
						"columns": map[string]any{
							"type": "object",
							"description": "컬럼 이름 → 논리명. 보낸 컬럼만 바뀐다 " +
								"(예: {\"user_id\": \"회원 번호\"})",
							"additionalProperties": map[string]any{"type": "string"},
						},
					}, "table"),
				},
			}, "tables"),
			Run: erdSetLogicalNames,
		},
		{
			Name: "add_table",
			Description: "새 테이블을 만든다. columns를 함께 주면 컬럼까지 한 번에 만든다. " +
				"기본키로 쓸 컬럼은 primaryKey에 이름을 적는다.",
			Schema: objectSchema(map[string]any{
				"name":    str("테이블 이름"),
				"comment": str("테이블 설명 (선택)"),
				"columns": map[string]any{
					"type":        "array",
					"description": "컬럼 목록. 각 항목은 {name, type, nullable, comment}",
					"items": objectSchema(map[string]any{
						"name":     str("컬럼 이름"),
						"type":     str("DB 타입 (예: BIGINT, VARCHAR(255))"),
						"nullable": boolp("NULL 허용 (기본 true)"),
						"comment":  str("컬럼 설명 (선택)"),
					}, "name", "type"),
				},
				"primaryKey": map[string]any{
					"type":        "array",
					"description": "기본키 컬럼 이름 목록",
					"items":       map[string]any{"type": "string"},
				},
			}, "name"),
			Run: erdAddTable,
		},
		{
			Name:        "update_table",
			Description: "테이블의 이름이나 설명을 바꾼다.",
			Schema: objectSchema(map[string]any{
				"table":   str("지금 테이블 이름"),
				"name":    str("새 이름 (선택)"),
				"comment": str("새 설명 (선택)"),
			}, "table"),
			Run: erdUpdateTable,
		},
		{
			Name:        "add_column",
			Description: "테이블에 컬럼을 더한다.",
			Schema: objectSchema(map[string]any{
				"table":    str("테이블 이름"),
				"name":     str("컬럼 이름"),
				"type":     str("DB 타입. domain을 주면 생략할 수 있다"),
				"domain":   str("쓸 도메인 이름 (선택). 주면 타입·NULL·기본값이 그 정의를 따른다"),
				"nullable": boolp("NULL 허용 (기본 true)"),
				"comment":  str("설명 (선택)"),
				"default":  str("기본값 (선택)"),
			}, "table", "name"),
			Run: erdAddColumn,
		},
		{
			Name:        "update_column",
			Description: "컬럼의 이름·타입·도메인·NULL 허용·기본값·설명을 바꾼다. 바꿀 것만 넘긴다.",
			Schema: objectSchema(map[string]any{
				"table":    str("테이블 이름"),
				"name":     str("지금 컬럼 이름"),
				"newName":  str("새 이름 (선택)"),
				"type":     str("새 타입 (선택)"),
				"domain":   str("쓸 도메인 이름 (선택). 빈 문자열을 주면 도메인 연결을 끊는다"),
				"nullable": boolp("NULL 허용 (선택)"),
				"default":  str("기본값 (선택)"),
				"comment":  str("설명 (선택)"),
			}, "table", "name"),
			Run: erdUpdateColumn,
		},
		{
			Name:        "set_primary_key",
			Description: "테이블의 기본키를 지정한다. 컬럼 순서가 곧 키 순서다.",
			Schema: objectSchema(map[string]any{
				"table": str("테이블 이름"),
				"columns": map[string]any{
					"type": "array", "description": "기본키 컬럼 이름 목록",
					"items": map[string]any{"type": "string"},
				},
			}, "table", "columns"),
			Run: erdSetPK,
		},
		{
			Name:        "add_foreign_key",
			Description: "외래키를 만든다. 참조할 테이블은 이 초안 안에 있어야 한다.",
			Schema: objectSchema(map[string]any{
				"table": str("외래키를 가질 테이블"),
				"name":  str("외래키 이름"),
				"columns": map[string]any{
					"type": "array", "description": "이 테이블의 컬럼", "items": map[string]any{"type": "string"},
				},
				"refTable": str("참조할 테이블"),
				"refColumns": map[string]any{
					"type": "array", "description": "참조할 컬럼", "items": map[string]any{"type": "string"},
				},
				"onDelete": str("참조 행 삭제 시: NO ACTION, RESTRICT, CASCADE, SET NULL, SET DEFAULT"),
			}, "table", "name", "columns", "refTable", "refColumns"),
			Run: erdAddFK,
		},
		{
			Name:        "add_index",
			Description: "인덱스를 만든다.",
			Schema: objectSchema(map[string]any{
				"table": str("테이블 이름"),
				"name":  str("인덱스 이름"),
				"columns": map[string]any{
					"type": "array", "description": "인덱스 컬럼", "items": map[string]any{"type": "string"},
				},
				"unique": boolp("고유 인덱스"),
			}, "table", "name", "columns"),
			Run: erdAddIndex,
		},
		{
			Name: "delete_column",
			Description: "테이블에서 컬럼 하나를 지운다. **사용자 승인이 필요하다** — " +
				"초안 편집 툴 가운데 이것만 그렇다. 지우면 그 컬럼을 쓰는 기본키 자리·인덱스·" +
				"외래키가 함께 영향받으므로, 제안에 무엇이 함께 사라지는지 담아 사용자가 " +
				"보고 결정한다. 부르고 나서 \"지웠습니다\"가 아니라 \"승인을 요청했습니다\"라고 말한다.",
			Schema: objectSchema(map[string]any{
				"table":  str("테이블 이름"),
				"column": str("지울 컬럼 이름"),
			}, "table", "column"),
			Mutating: true,
			Propose:  erdProposeDeleteColumn,
			Run:      erdDeleteColumn,
		},
		{
			Name: "duplicate_table",
			Description: "테이블을 통째로 베껴 새 테이블을 만든다. 컬럼·기본키·인덱스·체크 제약과 " +
				"이 테이블에서 나가는 외래키를 함께 베낀다(이 테이블을 가리키는 외래키는 베끼지 않는다). " +
				"\"같은 구조로 하나 더\" 같은 요청에 add_table로 컬럼을 하나씩 다시 만들지 말고 이것을 쓴다.",
			Schema: objectSchema(map[string]any{
				"table":           str("베낄 테이블 이름"),
				"name":            str("새 테이블 이름 (생략하면 원본이름_copy)"),
				"withIndexes":     boolp("인덱스도 베낀다 (기본 true)"),
				"withForeignKeys": boolp("외래키도 베낀다 (기본 true)"),
				"withChecks":      boolp("체크 제약도 베낀다 (기본 true)"),
			}, "table"),
			Run: erdDuplicateTable,
		},
	}

	out := make(map[string]*erdTool, len(list))
	for _, t := range list {
		out[t.Name] = t
	}
	return out
}

// erdAITools는 모델에게 노출할 툴 정의를 만든다.
func erdAITools() ([]ai.Tool, map[string]*erdTool) {
	registry := erdTools()
	names := make([]string, 0, len(registry))
	for name := range registry {
		names = append(names, name)
	}
	slices.Sort(names)

	out := make([]ai.Tool, 0, len(names))
	for _, name := range names {
		t := registry[name]
		out = append(out, ai.Tool{Name: t.Name, Description: t.Description, Schema: t.Schema})
	}
	return out, registry
}

// submit은 op 하나를 문서에 적용한다.
//
// 사람이 편집할 때와 **같은 경로**(Hub.SubmitOp)를 쓴다. 그래서 검증도, op-log 기록도,
// 열어 둔 사람들에게 브로드캐스트되는 것도 똑같다 — AI 전용 경로를 따로 만들면
// 그중 하나가 빠지고, 빠진 것은 대개 검증이다.
func (ec *erdToolContext) submit(kind erd.Kind, payload any) (*erd.Document, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("요청을 만들 수 없습니다: %w", err)
	}
	op := &erd.Op{
		ID: uuid.NewString(), Kind: kind, Payload: raw,
		Actor: ec.tc.user.ID, ActorName: displayName(ec.tc.user),
	}
	doc, err := ec.tc.srv.erdHub.SubmitOp(ec.tc.ctx, ec.docID, op)
	if err != nil {
		// erd.Error는 사람이 읽을 수 있는 거절 사유를 담고 있다.
		// 모델이 그것을 읽고 고쳐 다시 부를 수 있도록 그대로 넘긴다.
		return nil, err
	}
	return doc, nil
}

func (ec *erdToolContext) document() (*erd.Document, error) {
	return ec.tc.srv.st.GetERDDocument(ec.tc.ctx, ec.docID)
}

// findTable은 이름으로 테이블을 찾는다. 네임스페이스를 붙인 키도 받는다.
func findERDTable(doc *erd.Document, name string) *schema.Table {
	needle := strings.ToLower(strings.TrimSpace(name))
	for _, t := range doc.Schema.Tables {
		if strings.ToLower(t.Name) == needle || strings.ToLower(t.Key()) == needle {
			return t
		}
	}
	return nil
}

// erdSchemaBudget은 read_schema 한 번에 담을 대략의 바이트 수다.
//
// asJSON의 상한(maxResult)보다 넉넉히 낮게 잡는다. 상한에 걸려 잘리면 JSON이
// 문장 중간에서 끊겨 모델이 아무것도 못 읽지만, 여기서 미리 멈추면 온전한 JSON에
// "여기까지 담았고 다음은 몇 번부터"를 함께 줄 수 있다.
const erdSchemaBudget = 16_000

type erdColOut struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Nullable bool   `json:"nullable"`
	Comment  string `json:"comment,omitempty"`
	Logical  string `json:"logical,omitempty"`
	// Domain은 이 컬럼이 쓰는 재사용 타입의 이름이다.
	//
	// 함께 싣는 이유: 이것이 없으면 모델은 타입만 보고 "이미 맞다"고 판단해
	// 도메인을 붙일 생각을 하지 않는다. 그러면 정의를 고쳐도 그 컬럼만 안 따라온다.
	Domain string `json:"domain,omitempty"`
}

type erdTblOut struct {
	Name       string      `json:"name"`
	Comment    string      `json:"comment,omitempty"`
	Logical    string      `json:"logical,omitempty"`
	Columns    []erdColOut `json:"columns"`
	PrimaryKey []string    `json:"primaryKey,omitempty"`
	ForeignKey []string    `json:"foreignKeys,omitempty"`
	Indexes    []string    `json:"indexes,omitempty"`
}

// erdTableOut은 테이블 하나를 툴 결과 모양으로 만든다.
//
// 논리명을 함께 싣는 이유: 논리명은 스키마가 아니라 배치(Box)에 담겨 있어서,
// 스키마만 읽으면 이미 붙어 있는 이름이 보이지 않는다. 그러면 모델은 다 붙었는지
// 알 수 없어 매번 전부 다시 붙인다.
func erdTableOut(doc *erd.Document, t *schema.Table) erdTblOut {
	out := erdTblOut{Name: t.Name, Comment: t.Comment}
	box := doc.Layout[t.Key()]
	if box != nil {
		out.Logical = box.Logical
	}
	for _, c := range t.Columns {
		col := erdColOut{
			Name: c.Name, Type: c.RawType, Nullable: c.Nullable,
			Comment: c.Comment, Domain: c.Domain,
		}
		if box != nil {
			col.Logical = box.ColumnLogical[strings.ToLower(c.Name)]
		}
		out.Columns = append(out.Columns, col)
	}
	if t.PrimaryKey != nil {
		out.PrimaryKey = t.PrimaryKey.Columns
	}
	for _, fk := range t.ForeignKeys {
		out.ForeignKey = append(out.ForeignKey, fmt.Sprintf("%s: %s → %s(%s)",
			fk.Name, strings.Join(fk.Columns, ","), fk.RefTable, strings.Join(fk.RefColumns, ",")))
	}
	for _, idx := range t.Indexes {
		cols := make([]string, 0, len(idx.Columns))
		for _, c := range idx.Columns {
			cols = append(cols, c.Column)
		}
		label := idx.Name
		if idx.Unique {
			label = "UNIQUE " + label
		}
		out.Indexes = append(out.Indexes, fmt.Sprintf("%s (%s)", label, strings.Join(cols, ",")))
	}
	return out
}

func erdReadSchema(ec *erdToolContext, args json.RawMessage) (string, error) {
	var in struct {
		Table  string `json:"table"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
	}
	if err := parseArgs(args, &in); err != nil {
		return "", err
	}
	doc, err := ec.document()
	if err != nil {
		return "", err
	}
	all := doc.Schema.Tables

	// 테이블 하나만 달라고 하면 그것만 준다. 컬럼이 아주 많은 표 하나 때문에
	// 나머지를 못 읽는 일이 없어야 한다.
	if name := strings.TrimSpace(in.Table); name != "" {
		t := findERDTable(doc, name)
		if t == nil {
			return "", fmt.Errorf("테이블 %q을(를) 찾을 수 없습니다", name)
		}
		one := map[string]any{
			"document": doc.Name, "dialect": doc.Dialect,
			"tables": []erdTblOut{erdTableOut(doc, t)},
		}
		if len(doc.Domains) > 0 {
			one["domains"] = domainsOut(doc)
		}
		return asJSON(one)
	}

	start := in.Offset
	if start < 0 {
		start = 0
	}
	if start > len(all) {
		start = len(all)
	}

	// 들어가는 만큼 담는다. 개수로 자르면 컬럼이 많은 표에서는 그래도 넘치고,
	// 적은 표에서는 쓸데없이 여러 번 부르게 된다.
	tables := make([]erdTblOut, 0, len(all)-start)
	size := 0
	next := 0
	for i := start; i < len(all); i++ {
		if in.Limit > 0 && len(tables) >= in.Limit {
			next = i
			break
		}
		out := erdTableOut(doc, all[i])
		n := erdOutSize(out)
		// 첫 표는 아무리 커도 담는다. 안 그러면 큰 표 하나에서 영영 못 나아간다.
		if len(tables) > 0 && size+n > erdSchemaBudget {
			next = i
			break
		}
		tables = append(tables, out)
		size += n
	}

	res := map[string]any{
		"document": doc.Name, "dialect": doc.Dialect,
		"tableCount": len(all), "offset": start, "tables": tables,
	}
	// 도메인 목록도 함께 준다. 작고, 컬럼 타입을 정할 때마다 필요한 값이다 —
	// 따로 물어보게 하면 툴 왕복이 한 번 더 늘고, 안 물어보면 도메인을 무시한다.
	if len(doc.Domains) > 0 {
		res["domains"] = domainsOut(doc)
	}
	if next > 0 {
		res["nextOffset"] = next
		res["note"] = fmt.Sprintf(
			"테이블 %d개 중 %d~%d번째만 담았습니다. 나머지는 offset=%d 으로 이어서 부르세요.",
			len(all), start, next-1, next)
	}
	return asJSON(res)
}

// erdOutSize는 이 테이블이 결과에서 차지할 대략의 바이트 수다.
func erdOutSize(t erdTblOut) int {
	data, err := json.Marshal(t)
	if err != nil {
		return 0
	}
	// MarshalIndent로 다시 찍히므로 들여쓰기만큼 늘어난다. 넉넉히 잡는다.
	return len(data) * 3 / 2
}

// erdSetLogicalNames는 테이블·컬럼에 논리명을 붙인다.
//
// 논리명이 table.move 로 가는 이유: 논리명은 스키마가 아니라 배치에 담긴 설계
// 메모다(색·아이콘과 같은 자리). 그래서 좌표를 함께 보내야 하고, 지금 좌표를
// 읽어 그대로 되돌려 준다 — 안 그러면 이름을 붙이는 동안 카드가 (0,0)으로 모인다.
func erdSetLogicalNames(ec *erdToolContext, args json.RawMessage) (string, error) {
	var in struct {
		Tables []struct {
			Table   string            `json:"table"`
			Logical *string           `json:"logical"`
			Columns map[string]string `json:"columns"`
		} `json:"tables"`
	}
	if err := parseArgs(args, &in); err != nil {
		return "", err
	}
	if len(in.Tables) == 0 {
		return "", fmt.Errorf("tables가 비어 있습니다. 논리명을 붙일 테이블을 적어주세요")
	}

	doc, err := ec.document()
	if err != nil {
		return "", err
	}

	type done struct {
		Table   string `json:"table"`
		Logical string `json:"logical,omitempty"`
		Columns int    `json:"columns"`
	}
	out := make([]done, 0, len(in.Tables))
	for _, item := range in.Tables {
		t := findERDTable(doc, item.Table)
		if t == nil {
			return "", fmt.Errorf("테이블 %q을(를) 찾을 수 없습니다", item.Table)
		}
		key := t.Key()

		// 없는 컬럼에 이름을 붙이면 화면 어디에도 나타나지 않는다. 조용히 넘기면
		// 모델은 붙였다고 믿으므로, 어느 컬럼인지 짚어 거절한다.
		cols := map[string]string{}
		for name, label := range item.Columns {
			col := t.Column(name)
			if col == nil {
				return "", fmt.Errorf("%s 테이블에 %q 컬럼이 없습니다", t.Name, name)
			}
			cols[col.Name] = label
		}

		box := doc.Layout[key]
		payload := map[string]any{"key": key}
		if box != nil {
			payload["x"], payload["y"] = box.X, box.Y
		}
		if item.Logical != nil {
			payload["logical"] = strings.TrimSpace(*item.Logical)
		}
		if len(cols) > 0 {
			payload["columnLogical"] = cols
		}
		if item.Logical == nil && len(cols) == 0 {
			continue
		}
		next, err := ec.submit(erd.OpTableMove, payload)
		if err != nil {
			return "", err
		}
		doc = next

		rec := done{Table: t.Name, Columns: len(cols)}
		if item.Logical != nil {
			rec.Logical = strings.TrimSpace(*item.Logical)
		}
		out = append(out, rec)
	}

	return asJSON(map[string]any{
		"ok": true, "updated": out,
		"note": "논리명은 설계 메모입니다. 도구 줄의 논리명/둘 다 보기에서 보이고, " +
			"마이그레이션 SQL에는 들어가지 않습니다.",
	})
}

func erdAddTable(ec *erdToolContext, args json.RawMessage) (string, error) {
	var in struct {
		Name       string   `json:"name"`
		Comment    string   `json:"comment"`
		PrimaryKey []string `json:"primaryKey"`
		Columns    []struct {
			Name     string `json:"name"`
			Type     string `json:"type"`
			Nullable *bool  `json:"nullable"`
			Comment  string `json:"comment"`
		} `json:"columns"`
	}
	if err := parseArgs(args, &in); err != nil {
		return "", err
	}
	if strings.TrimSpace(in.Name) == "" {
		return "", fmt.Errorf("테이블 이름이 필요합니다")
	}
	if _, err := ec.submit(erd.OpTableAdd, map[string]any{
		"name": in.Name, "comment": in.Comment,
	}); err != nil {
		return "", err
	}

	added := 0
	for _, c := range in.Columns {
		nullable := true
		if c.Nullable != nil {
			nullable = *c.Nullable
		}
		if _, err := ec.submit(erd.OpColumnAdd, map[string]any{
			"table": in.Name, "name": c.Name, "type": c.Type,
			"nullable": nullable, "comment": c.Comment,
		}); err != nil {
			// 테이블은 이미 만들어졌다. 그 사실을 알려야 모델이 처음부터 다시
			// 만들려 하지 않는다.
			return "", fmt.Errorf("테이블 %s 은(는) 만들었지만 컬럼 %s 에서 실패했습니다: %w",
				in.Name, c.Name, err)
		}
		added++
	}
	if len(in.PrimaryKey) > 0 {
		if _, err := ec.submit(erd.OpPKSet, map[string]any{
			"table": in.Name, "columns": in.PrimaryKey,
		}); err != nil {
			return "", fmt.Errorf("테이블과 컬럼은 만들었지만 기본키 지정에서 실패했습니다: %w", err)
		}
	}
	return asJSON(map[string]any{
		"created": in.Name, "columns": added, "primaryKey": in.PrimaryKey,
	})
}

func erdUpdateTable(ec *erdToolContext, args json.RawMessage) (string, error) {
	var in struct {
		Table   string  `json:"table"`
		Name    *string `json:"name"`
		Comment *string `json:"comment"`
	}
	if err := parseArgs(args, &in); err != nil {
		return "", err
	}
	doc, err := ec.document()
	if err != nil {
		return "", err
	}
	tbl := findERDTable(doc, in.Table)
	if tbl == nil {
		return "", fmt.Errorf("테이블 %q 을(를) 찾을 수 없습니다. read_schema로 목록을 확인하세요", in.Table)
	}
	payload := map[string]any{"key": tbl.Key()}
	if in.Name != nil {
		payload["name"] = *in.Name
	}
	if in.Comment != nil {
		payload["comment"] = *in.Comment
	}
	if _, err := ec.submit(erd.OpTableUpdate, payload); err != nil {
		return "", err
	}
	return asJSON(map[string]any{"updated": tbl.Name})
}

func erdDuplicateTable(ec *erdToolContext, args json.RawMessage) (string, error) {
	var in struct {
		Table           string `json:"table"`
		Name            string `json:"name"`
		WithIndexes     *bool  `json:"withIndexes"`
		WithForeignKeys *bool  `json:"withForeignKeys"`
		WithChecks      *bool  `json:"withChecks"`
	}
	if err := parseArgs(args, &in); err != nil {
		return "", err
	}
	doc, err := ec.document()
	if err != nil {
		return "", err
	}
	tbl := findERDTable(doc, in.Table)
	if tbl == nil {
		return "", fmt.Errorf("테이블 %q 을(를) 찾을 수 없습니다. read_schema로 목록을 확인하세요", in.Table)
	}
	payload := map[string]any{"key": tbl.Key()}
	if strings.TrimSpace(in.Name) != "" {
		payload["name"] = in.Name
	}
	if in.WithIndexes != nil {
		payload["withIndexes"] = *in.WithIndexes
	}
	if in.WithForeignKeys != nil {
		payload["withForeignKeys"] = *in.WithForeignKeys
	}
	if in.WithChecks != nil {
		payload["withChecks"] = *in.WithChecks
	}
	next, err := ec.submit(erd.OpTableDuplicate, payload)
	if err != nil {
		return "", err
	}
	// 만들어진 이름을 돌려준다. 이름을 생략했으면 서버가 정하므로, 그것을 알려주지
	// 않으면 모델이 다음 호출에서 없는 이름을 쓴다.
	made := in.Name
	if made == "" {
		for _, t := range next.Schema.Tables {
			if findERDTable(doc, t.Key()) == nil {
				made = t.Name
				break
			}
		}
	}
	return asJSON(map[string]any{"duplicated": tbl.Name, "created": made})
}

func erdAddColumn(ec *erdToolContext, args json.RawMessage) (string, error) {
	var in struct {
		Table    string `json:"table"`
		Name     string `json:"name"`
		Type     string `json:"type"`
		Domain   string `json:"domain"`
		Nullable *bool  `json:"nullable"`
		Comment  string `json:"comment"`
		Default  string `json:"default"`
	}
	if err := parseArgs(args, &in); err != nil {
		return "", err
	}
	tbl, err := ec.mustTable(in.Table)
	if err != nil {
		return "", err
	}
	nullable := true
	if in.Nullable != nil {
		nullable = *in.Nullable
	}
	payload := map[string]any{
		"table": tbl.Key(), "name": in.Name, "type": in.Type,
		"nullable": nullable, "comment": in.Comment,
	}
	// 도메인을 주면 타입은 그 정의에서 온다. 타입을 함께 줬더라도 도메인이 이긴다 —
	// 둘이 다를 때 도메인을 무시하면 "도메인을 붙였는데 타입이 다르다"가 된다.
	if d := strings.TrimSpace(in.Domain); d != "" {
		payload["domain"] = d
	}
	if in.Default != "" {
		payload["default"] = in.Default
	}
	if _, err := ec.submit(erd.OpColumnAdd, payload); err != nil {
		return "", err
	}
	// 도메인을 거쳤으면 실제로 무슨 타입이 됐는지 돌려준다. 모델이 그것을 모르면
	// 다음 판단(인덱스 길이, 비교 대상)이 어긋난다.
	out := map[string]any{"table": tbl.Name, "added": in.Name, "type": in.Type}
	if in.Domain != "" {
		out["domain"] = in.Domain
		if doc, derr := ec.document(); derr == nil {
			if t := findERDTable(doc, tbl.Key()); t != nil {
				if c := t.Column(in.Name); c != nil {
					out["type"] = c.RawType
				}
			}
		}
	}
	return asJSON(out)
}

func erdUpdateColumn(ec *erdToolContext, args json.RawMessage) (string, error) {
	var in struct {
		Table    string  `json:"table"`
		Name     string  `json:"name"`
		NewName  *string `json:"newName"`
		Type     *string `json:"type"`
		Domain   *string `json:"domain"`
		Nullable *bool   `json:"nullable"`
		Default  *string `json:"default"`
		Comment  *string `json:"comment"`
	}
	if err := parseArgs(args, &in); err != nil {
		return "", err
	}
	tbl, err := ec.mustTable(in.Table)
	if err != nil {
		return "", err
	}
	payload := map[string]any{"table": tbl.Key(), "name": in.Name}
	if in.NewName != nil {
		payload["newName"] = *in.NewName
	}
	if in.Type != nil {
		payload["type"] = *in.Type
	}
	// 빈 문자열은 "연결을 끊는다"이고, 값이 있으면 그 도메인을 쓴다.
	// 보내지 않으면 지금 것을 그대로 둔다 — 이름만 바꾸려다 도메인이 풀리면 안 된다.
	if in.Domain != nil {
		payload["domain"] = strings.TrimSpace(*in.Domain)
	}
	if in.Nullable != nil {
		payload["nullable"] = *in.Nullable
	}
	if in.Default != nil {
		payload["default"] = *in.Default
	}
	if in.Comment != nil {
		payload["comment"] = *in.Comment
	}
	if _, err := ec.submit(erd.OpColumnUpdate, payload); err != nil {
		return "", err
	}
	out := map[string]any{"table": tbl.Name, "updated": in.Name}
	if doc, derr := ec.document(); derr == nil {
		if t := findERDTable(doc, tbl.Key()); t != nil {
			name := in.Name
			if in.NewName != nil {
				name = *in.NewName
			}
			if c := t.Column(name); c != nil {
				out["type"] = c.RawType
				if c.Domain != "" {
					out["domain"] = c.Domain
				}
			}
		}
	}
	return asJSON(out)
}

func erdSetPK(ec *erdToolContext, args json.RawMessage) (string, error) {
	var in struct {
		Table   string   `json:"table"`
		Columns []string `json:"columns"`
	}
	if err := parseArgs(args, &in); err != nil {
		return "", err
	}
	tbl, err := ec.mustTable(in.Table)
	if err != nil {
		return "", err
	}
	if _, err := ec.submit(erd.OpPKSet, map[string]any{
		"table": tbl.Key(), "columns": in.Columns,
	}); err != nil {
		return "", err
	}
	return asJSON(map[string]any{"table": tbl.Name, "primaryKey": in.Columns})
}

func erdAddFK(ec *erdToolContext, args json.RawMessage) (string, error) {
	var in struct {
		Table      string   `json:"table"`
		Name       string   `json:"name"`
		Columns    []string `json:"columns"`
		RefTable   string   `json:"refTable"`
		RefColumns []string `json:"refColumns"`
		OnDelete   string   `json:"onDelete"`
	}
	if err := parseArgs(args, &in); err != nil {
		return "", err
	}
	tbl, err := ec.mustTable(in.Table)
	if err != nil {
		return "", err
	}
	ref, err := ec.mustTable(in.RefTable)
	if err != nil {
		return "", err
	}
	payload := map[string]any{
		"table": tbl.Key(), "name": in.Name, "columns": in.Columns,
		"refTable": ref.Name, "refColumns": in.RefColumns,
	}
	if in.OnDelete != "" {
		payload["onDelete"] = strings.ToUpper(in.OnDelete)
	}
	if _, err := ec.submit(erd.OpFKAdd, payload); err != nil {
		return "", err
	}
	return asJSON(map[string]any{
		"table": tbl.Name, "foreignKey": in.Name, "references": ref.Name,
	})
}

func erdAddIndex(ec *erdToolContext, args json.RawMessage) (string, error) {
	var in struct {
		Table   string   `json:"table"`
		Name    string   `json:"name"`
		Columns []string `json:"columns"`
		Unique  bool     `json:"unique"`
	}
	if err := parseArgs(args, &in); err != nil {
		return "", err
	}
	tbl, err := ec.mustTable(in.Table)
	if err != nil {
		return "", err
	}
	cols := make([]map[string]any, 0, len(in.Columns))
	for _, c := range in.Columns {
		cols = append(cols, map[string]any{"column": c})
	}
	if _, err := ec.submit(erd.OpIndexAdd, map[string]any{
		"table": tbl.Key(), "name": in.Name, "columns": cols, "unique": in.Unique,
	}); err != nil {
		return "", err
	}
	return asJSON(map[string]any{"table": tbl.Name, "index": in.Name, "unique": in.Unique})
}

// mustTable은 이름으로 테이블을 찾고, 없으면 모델이 고칠 수 있는 오류를 준다.
func (ec *erdToolContext) mustTable(name string) (*schema.Table, error) {
	doc, err := ec.document()
	if err != nil {
		return nil, err
	}
	tbl := findERDTable(doc, name)
	if tbl == nil {
		names := make([]string, 0, len(doc.Schema.Tables))
		for _, t := range doc.Schema.Tables {
			names = append(names, t.Name)
		}
		return nil, fmt.Errorf("테이블 %q 을(를) 찾을 수 없습니다. 이 초안의 테이블: %s",
			name, strings.Join(names, ", "))
	}
	return tbl, nil
}

// ---------- 도메인 (재사용 타입) ----------

type erdDomainOut struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Nullable *bool  `json:"nullable,omitempty"`
	Default  string `json:"default,omitempty"`
	Comment  string `json:"comment,omitempty"`
	// UsedBy는 이 도메인을 쓰는 컬럼들이다("orders.total" 꼴).
	//
	// 함께 세는 이유: 도메인을 고치면 이 컬럼들이 함께 바뀐다. 몇 개가 딸려
	// 움직이는지 모르고 고치는 것과 알고 고치는 것은 다른 일이다.
	UsedBy []string `json:"usedBy,omitempty"`
}

func domainsOut(doc *erd.Document) []erdDomainOut {
	out := make([]erdDomainOut, 0, len(doc.Domains))
	for _, d := range doc.Domains {
		item := erdDomainOut{
			Name: d.Name, Type: d.Type, Default: d.Default, Comment: d.Comment,
		}
		if d.Nullable != nil {
			v := *d.Nullable
			item.Nullable = &v
		}
		for _, t := range doc.Schema.Tables {
			for _, c := range t.Columns {
				if strings.EqualFold(c.Domain, d.Name) || strings.EqualFold(c.Domain, d.Key()) {
					item.UsedBy = append(item.UsedBy, t.Name+"."+c.Name)
				}
			}
		}
		out = append(out, item)
	}
	return out
}

func erdListDomains(ec *erdToolContext, _ json.RawMessage) (string, error) {
	doc, err := ec.document()
	if err != nil {
		return "", err
	}
	return asJSON(map[string]any{
		"document": doc.Name, "dialect": doc.Dialect, "domains": domainsOut(doc),
	})
}

func erdAddDomain(ec *erdToolContext, args json.RawMessage) (string, error) {
	var in struct {
		Name     string  `json:"name"`
		Type     string  `json:"type"`
		Nullable *bool   `json:"nullable"`
		Default  *string `json:"default"`
		Comment  *string `json:"comment"`
	}
	if err := parseArgs(args, &in); err != nil {
		return "", err
	}
	payload := map[string]any{"name": in.Name, "type": in.Type}
	if in.Nullable != nil {
		payload["nullable"] = *in.Nullable
	}
	if in.Default != nil {
		payload["default"] = *in.Default
	}
	if in.Comment != nil {
		payload["comment"] = *in.Comment
	}
	if _, err := ec.submit(erd.OpDomainAdd, payload); err != nil {
		return "", err
	}
	return asJSON(map[string]any{
		"ok": true, "added": in.Name, "type": in.Type,
		"note": "컬럼에 붙이려면 add_column/update_column 의 domain 인자에 이 이름을 주세요. " +
			"도메인은 DB에 만들어지지 않고, 컬럼에는 구체 타입이 함께 남습니다.",
	})
}

func erdUpdateDomain(ec *erdToolContext, args json.RawMessage) (string, error) {
	var in struct {
		Name          string  `json:"name"`
		NewName       *string `json:"newName"`
		Type          *string `json:"type"`
		Nullable      *bool   `json:"nullable"`
		ClearNullable *bool   `json:"clearNullable"`
		Default       *string `json:"default"`
		Comment       *string `json:"comment"`
	}
	if err := parseArgs(args, &in); err != nil {
		return "", err
	}
	// 무엇이 함께 바뀌는지 먼저 세어 둔다. 바꾼 뒤에는 옛 이름으로 찾을 수 없다.
	before, err := ec.document()
	if err != nil {
		return "", err
	}
	affected := []string{}
	for _, d := range domainsOut(before) {
		if strings.EqualFold(d.Name, in.Name) {
			affected = d.UsedBy
		}
	}

	payload := map[string]any{"name": in.Name}
	if in.NewName != nil {
		payload["newName"] = *in.NewName
	}
	if in.Type != nil {
		payload["type"] = *in.Type
	}
	if in.ClearNullable != nil && *in.ClearNullable {
		payload["clearNullable"] = true
	} else if in.Nullable != nil {
		payload["nullable"] = *in.Nullable
	}
	if in.Default != nil {
		payload["default"] = *in.Default
	}
	if in.Comment != nil {
		payload["comment"] = *in.Comment
	}
	if _, err := ec.submit(erd.OpDomainUpdate, payload); err != nil {
		return "", err
	}
	return asJSON(map[string]any{
		"ok": true, "domain": in.Name, "alsoChanged": affected,
	})
}

func erdDeleteDomain(ec *erdToolContext, args json.RawMessage) (string, error) {
	var in struct {
		Name string `json:"name"`
	}
	if err := parseArgs(args, &in); err != nil {
		return "", err
	}
	before, err := ec.document()
	if err != nil {
		return "", err
	}
	detached := []string{}
	for _, d := range domainsOut(before) {
		if strings.EqualFold(d.Name, in.Name) {
			detached = d.UsedBy
		}
	}
	if _, err := ec.submit(erd.OpDomainDelete, map[string]any{"name": in.Name}); err != nil {
		return "", err
	}
	return asJSON(map[string]any{
		"ok": true, "deleted": in.Name, "detached": detached,
		"note": "이 컬럼들의 타입은 그대로 두고 도메인 연결만 끊었습니다.",
	})
}
