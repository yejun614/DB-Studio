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
// 대신 **지우는 툴은 주지 않았다.** 되돌리기가 아직 없어서, 잘못 지운 테이블을
// 복구하려면 사람이 다시 만들어야 한다. 지우는 일은 인스펙터에서 한 번 누르면 되고,
// 그 한 번은 사람이 하는 편이 낫다.

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

// erdTool은 ERD 툴 하나다. 앱 툴과 달리 승인 경로가 없으므로 Run 하나뿐이다.
type erdTool struct {
	Name        string
	Description string
	Schema      map[string]any
	Run         func(ec *erdToolContext, args json.RawMessage) (string, error)
}

func erdTools() map[string]*erdTool {
	list := []*erdTool{
		{
			Name: "read_schema",
			Description: "이 초안의 현재 스키마를 읽는다. 테이블·컬럼·기본키·외래키·인덱스를 반환한다. " +
				"무언가를 바꾸기 전에 먼저 부른다 — 지금 무엇이 있는지 모르고 고치면 이름이 겹치거나 없는 것을 참조한다.",
			Schema: objectSchema(nil),
			Run:    erdReadSchema,
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
				"type":     str("DB 타입"),
				"nullable": boolp("NULL 허용 (기본 true)"),
				"comment":  str("설명 (선택)"),
				"default":  str("기본값 (선택)"),
			}, "table", "name", "type"),
			Run: erdAddColumn,
		},
		{
			Name:        "update_column",
			Description: "컬럼의 이름·타입·NULL 허용·기본값·설명을 바꾼다. 바꿀 것만 넘긴다.",
			Schema: objectSchema(map[string]any{
				"table":    str("테이블 이름"),
				"name":     str("지금 컬럼 이름"),
				"newName":  str("새 이름 (선택)"),
				"type":     str("새 타입 (선택)"),
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

func erdReadSchema(ec *erdToolContext, _ json.RawMessage) (string, error) {
	doc, err := ec.document()
	if err != nil {
		return "", err
	}
	type colOut struct {
		Name     string `json:"name"`
		Type     string `json:"type"`
		Nullable bool   `json:"nullable"`
		Comment  string `json:"comment,omitempty"`
	}
	type tblOut struct {
		Name       string   `json:"name"`
		Comment    string   `json:"comment,omitempty"`
		Columns    []colOut `json:"columns"`
		PrimaryKey []string `json:"primaryKey,omitempty"`
		ForeignKey []string `json:"foreignKeys,omitempty"`
		Indexes    []string `json:"indexes,omitempty"`
	}
	tables := make([]tblOut, 0, len(doc.Schema.Tables))
	for _, t := range doc.Schema.Tables {
		out := tblOut{Name: t.Name, Comment: t.Comment}
		for _, c := range t.Columns {
			out.Columns = append(out.Columns, colOut{
				Name: c.Name, Type: c.RawType, Nullable: c.Nullable, Comment: c.Comment,
			})
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
		tables = append(tables, out)
	}
	return asJSON(map[string]any{
		"document": doc.Name, "dialect": doc.Dialect, "tables": tables,
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
	if in.Default != "" {
		payload["default"] = in.Default
	}
	if _, err := ec.submit(erd.OpColumnAdd, payload); err != nil {
		return "", err
	}
	return asJSON(map[string]any{"table": tbl.Name, "added": in.Name, "type": in.Type})
}

func erdUpdateColumn(ec *erdToolContext, args json.RawMessage) (string, error) {
	var in struct {
		Table    string  `json:"table"`
		Name     string  `json:"name"`
		NewName  *string `json:"newName"`
		Type     *string `json:"type"`
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
	return asJSON(map[string]any{"table": tbl.Name, "updated": in.Name})
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
