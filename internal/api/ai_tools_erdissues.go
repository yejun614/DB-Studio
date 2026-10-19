package api

import (
	"encoding/json"
	"fmt"
	"strings"

	"dbstudio/internal/erd"
	"dbstudio/internal/schema"
)

// 설계 문제를 읽고 고치는 툴.
//
// 화면의 "문제" 탭이 보여 주는 오류·경고를 어시스턴트도 읽는다. 같은 검증(schema.Validate)
// 을 쓴다 — 두 벌이 되면 화면에는 경고가 있는데 어시스턴트는 "문제없다"고 답한다.
//
// 읽기만으로는 고칠 수 없는 문제가 있었다. 외래키를 고치거나 지우는 툴, 인덱스를 지우는
// 툴이 없어서 "없는 표를 가리키는 관계", "겹치는 인덱스" 같은 것은 어시스턴트가 보고도
// 사람에게 넘겨야 했다. 서버에는 이미 그 op 가 있으므로 툴만 더한다. 지우는 것도 승인을
// 거치지 않는다 — 관계·인덱스는 컬럼과 달리 지워도 다른 것이 함께 사라지지 않고, 모두
// 편집 이력에 남아 되돌릴 수 있다.

// erdIssueTools는 문제 읽기와 그것을 고치는 데 필요한 툴이다.
func erdIssueTools() []*erdTool {
	return []*erdTool{
		{
			Name: "read_issues",
			Description: "화면의 \"문제\" 탭과 같은 설계 검사 결과(오류·경고)를 읽는다. " +
				"오류는 이대로는 만들 수 없거나 뜻이 깨진 것, 경고는 돌아가지만 나중에 아플 것이다. " +
				"문제마다 어디의 문제인지(table·column·fk·index), 고치는 방법(resolve)이 있고, " +
				"그대로 부르면 되는 경우는 fix 에 툴 이름과 인자가 들어 있다. " +
				"설계를 고친 뒤에 다시 불러 문제가 사라졌는지 확인한다. " +
				"결과가 많으면 nextOffset 으로 이어 읽는다.",
			Schema: objectSchema(map[string]any{
				"severity": map[string]any{
					"type": "string", "enum": []string{"error", "warning"},
					"description": "이 무게만 본다 (생략하면 둘 다)",
				},
				"table":  str("이 테이블의 문제만 본다 (생략하면 전체)"),
				"offset": num("몇 번째 문제부터 읽을지 (기본 0)"),
				"limit":  num("최대 몇 개를 읽을지 (기본 60)"),
			}),
			Run: erdReadIssues,
		},
		{
			Name: "update_foreign_key",
			Description: "외래키(관계)를 고친다. 바꿀 것만 넘긴다. 가리키는 표·컬럼을 바꾸려면 " +
				"columns·refTable·refColumns 를 함께 넘긴다(가리키는 쪽은 기본키나 고유 인덱스여야 한다).",
			Schema: objectSchema(map[string]any{
				"table":   str("외래키가 있는 테이블"),
				"name":    str("지금 외래키 이름"),
				"newName": str("새 이름 (선택)"),
				"columns": map[string]any{
					"type": "array", "description": "외래키 컬럼 (선택)", "items": map[string]any{"type": "string"},
				},
				"refTable": str("가리킬 테이블 (선택)"),
				"refColumns": map[string]any{
					"type": "array", "description": "가리킬 컬럼 (선택)", "items": map[string]any{"type": "string"},
				},
				"onDelete": str("참조 행 삭제 시 (선택): NO ACTION, RESTRICT, CASCADE, SET NULL, SET DEFAULT"),
				"onUpdate": str("참조 키 변경 시 (선택): NO ACTION, RESTRICT, CASCADE, SET NULL, SET DEFAULT"),
			}, "table", "name"),
			Run: erdUpdateFK,
		},
		{
			Name: "delete_foreign_key",
			Description: "외래키(관계) 하나를 지운다. 컬럼은 그대로 남는다. 승인 없이 바로 반영되며 " +
				"편집 이력에서 되돌릴 수 있다.",
			Schema: objectSchema(map[string]any{
				"table": str("외래키가 있는 테이블"),
				"name":  str("외래키 이름"),
			}, "table", "name"),
			Run: erdDeleteFK,
		},
		{
			Name: "delete_index",
			Description: "인덱스 하나를 지운다. 컬럼은 그대로 남는다. 승인 없이 바로 반영되며 " +
				"편집 이력에서 되돌릴 수 있다.",
			Schema: objectSchema(map[string]any{
				"table": str("인덱스가 있는 테이블"),
				"name":  str("인덱스 이름"),
			}, "table", "name"),
			Run: erdDeleteIndex,
		},
	}
}

// issueResolve는 규칙마다 "어떤 툴로 어떻게 고치는가"다.
//
// 검증기의 Hint 는 사람에게 하는 말이라 화면의 단추나 동작으로 설명한다. 모델에게는 부를
// 툴을 알려 줘야 같은 문제를 두 번 묻지 않는다. 여기 없는 규칙은 툴로 고칠 수 없는 것이다
// (뷰 정의 등) — 그때는 화면에서 고치도록 안내하게 한다.
var issueResolve = map[string]string{
	"fk-no-index":              "fix 의 add_index 를 그대로 부른다(외래키 컬럼으로 시작하는 인덱스).",
	"no-primary-key":           "set_primary_key 로 지정한다. 알맞은 컬럼이 없으면 add_column 으로 식별 컬럼을 먼저 더한다.",
	"fk-missing-table":         "update_foreign_key 로 있는 표를 가리키게 하거나, 필요 없는 관계면 delete_foreign_key.",
	"fk-missing-ref-column":    "update_foreign_key 로 있는 컬럼을 가리키게 하거나 delete_foreign_key.",
	"fk-missing-local-column":  "update_foreign_key 로 있는 컬럼에서 나가게 하거나, add_column 으로 그 컬럼을 더한다.",
	"fk-no-columns":            "update_foreign_key 로 columns·refTable·refColumns 를 채우거나 delete_foreign_key.",
	"fk-column-count":          "update_foreign_key 로 columns 와 refColumns 의 수를 맞춘다.",
	"fk-type-mismatch":         "update_column 으로 한쪽 타입을 다른 쪽에 맞춘다(보통 외래키 쪽 컬럼을 고친다).",
	"fk-target-not-unique":     "가리키는 컬럼에 add_index(unique=true)를 만들거나, update_foreign_key 로 기본키를 가리키게 한다.",
	"fk-set-null-not-nullable": "update_column 으로 그 컬럼을 nullable=true 로 바꾸거나, update_foreign_key 로 onDelete 를 다른 동작으로 바꾼다.",
	"fk-self-not-nullable":     "update_column 으로 자기 참조 컬럼을 nullable=true 로 바꾼다.",
	"fk-hard-cycle":            "고리 안의 외래키 컬럼 하나를 update_column 으로 nullable=true 로 바꾼다.",
	"fk-unsupported":           "이 DB 에는 외래키가 없다. 관계를 남겨 둘지 사용자에게 묻고, 지우려면 delete_foreign_key.",
	"pk-nullable-column":       "update_column 으로 그 컬럼을 nullable=false 로 바꾼다.",
	"pk-missing-column":        "set_primary_key 로 있는 컬럼만 지정한다.",
	"pk-duplicate-column":      "set_primary_key 로 컬럼을 한 번씩만 지정한다.",
	"index-missing-column":     "delete_index 로 지우고, 필요하면 있는 컬럼으로 add_index 를 다시 만든다.",
	"duplicate-index":          "겹치는 인덱스 가운데 하나를 delete_index 로 지운다(고유 인덱스·더 긴 인덱스를 남긴다).",
	"index-unsupported":        "이 DB 에는 일반 인덱스가 없다. 지우려면 delete_index.",
	"duplicate-column":         "update_column 의 newName 으로 하나의 이름을 바꾼다.",
	"column-no-type":           "update_column 으로 타입을 정한다.",
	"enum-no-values":           "update_column 으로 값 목록이 있는 타입(ENUM('a','b'))을 준다.",
	"table-no-columns":         "add_column 으로 컬럼을 더한다.",
	"duplicate-table":          "update_table 의 name 으로 하나의 이름을 바꾼다.",
}

type erdIssueOut struct {
	Severity string `json:"severity"`
	Rule     string `json:"rule"`
	Message  string `json:"message"`
	Hint     string `json:"hint,omitempty"`
	Table    string `json:"table,omitempty"`
	Column   string `json:"column,omitempty"`
	FK       string `json:"fk,omitempty"`
	Index    string `json:"index,omitempty"`
	View     string `json:"view,omitempty"`
	Resolve  string `json:"resolve"`
	Fix      any    `json:"fix,omitempty"`
}

func erdReadIssues(ec *erdToolContext, args json.RawMessage) (string, error) {
	var in struct {
		Severity string `json:"severity"`
		Table    string `json:"table"`
		Offset   int    `json:"offset"`
		Limit    int    `json:"limit"`
	}
	if err := parseArgs(args, &in); err != nil {
		return "", err
	}
	doc, err := ec.document()
	if err != nil {
		return "", err
	}
	tableKey := ""
	if name := strings.TrimSpace(in.Table); name != "" {
		t := findERDTable(doc, name)
		if t == nil {
			return "", fmt.Errorf("테이블 %q을(를) 찾을 수 없습니다", name)
		}
		tableKey = t.Key()
	}

	all := schema.Validate(doc.Schema)
	errors, warnings := 0, 0
	picked := []schema.Issue{}
	for _, is := range all {
		if is.Severity == schema.SeverityError {
			errors++
		} else {
			warnings++
		}
		if in.Severity != "" && is.Severity != in.Severity {
			continue
		}
		if tableKey != "" && is.Table != tableKey {
			continue
		}
		picked = append(picked, is)
	}

	limit := in.Limit
	if limit <= 0 || limit > 200 {
		limit = 60
	}
	offset := min(max(in.Offset, 0), len(picked))
	end := min(offset+limit, len(picked))

	out := make([]erdIssueOut, 0, end-offset)
	for _, is := range picked[offset:end] {
		o := erdIssueOut{
			Severity: is.Severity, Rule: is.Rule, Message: is.Message, Hint: is.Hint,
			Table: is.Table, Column: is.Column, FK: is.FK, Index: is.Index, View: is.View,
			Resolve: issueResolve[is.Rule],
		}
		if o.Resolve == "" {
			o.Resolve = "툴로 고칠 수 없는 문제다. 사용자에게 화면에서 고치도록 안내한다."
		}
		// 검증기가 고칠 인덱스까지 정해 준 경우는 그대로 부를 수 있게 툴 인자로 바꿔 준다.
		// 이름을 모델이 다시 지으면 화면의 "인덱스 만들기" 단추와 다른 이름이 생긴다.
		if fix := is.FixIndex; fix != nil {
			o.Fix = map[string]any{
				"tool": "add_index",
				"arguments": map[string]any{
					"table": is.Table, "name": fix.Name, "columns": fix.ColumnNames(), "unique": fix.Unique,
				},
			}
		}
		out = append(out, o)
	}

	res := map[string]any{
		"document": doc.Name, "dialect": doc.Dialect,
		"errors": errors, "warnings": warnings,
		"matched": len(picked), "issues": out,
	}
	if end < len(picked) {
		res["nextOffset"] = end
	}
	if len(all) == 0 {
		res["note"] = "찾은 문제가 없습니다."
	}
	return asJSON(res)
}

func erdUpdateFK(ec *erdToolContext, args json.RawMessage) (string, error) {
	var in struct {
		Table      string   `json:"table"`
		Name       string   `json:"name"`
		NewName    *string  `json:"newName"`
		Columns    []string `json:"columns"`
		RefTable   string   `json:"refTable"`
		RefColumns []string `json:"refColumns"`
		OnDelete   *string  `json:"onDelete"`
		OnUpdate   *string  `json:"onUpdate"`
	}
	if err := parseArgs(args, &in); err != nil {
		return "", err
	}
	tbl, err := ec.mustTable(in.Table)
	if err != nil {
		return "", err
	}
	// 가리키는 곳을 바꾸려면 세 가지가 함께 와야 한다. op 는 refTable 이 있을 때만 컬럼을
	// 다시 읽으므로, 컬럼만 넘기면 조용히 무시된다 — 그것을 성공으로 돌려주면 안 된다.
	retarget := in.RefTable != "" || len(in.Columns) > 0 || len(in.RefColumns) > 0
	payload := map[string]any{"table": tbl.Key(), "name": in.Name}
	if retarget {
		if in.RefTable == "" || len(in.Columns) == 0 || len(in.RefColumns) == 0 {
			return "", fmt.Errorf("가리키는 곳을 바꾸려면 columns·refTable·refColumns 를 함께 넘겨야 합니다")
		}
		ref, err := ec.mustTable(in.RefTable)
		if err != nil {
			return "", err
		}
		payload["columns"], payload["refTable"], payload["refColumns"] = in.Columns, ref.Name, in.RefColumns
		if ref.Namespace != tbl.Namespace {
			payload["refNamespace"] = ref.Namespace
		}
	}
	if in.NewName != nil {
		payload["newName"] = *in.NewName
	}
	if in.OnDelete != nil {
		payload["onDelete"] = strings.ToUpper(strings.TrimSpace(*in.OnDelete))
	}
	if in.OnUpdate != nil {
		payload["onUpdate"] = strings.ToUpper(strings.TrimSpace(*in.OnUpdate))
	}
	if _, err := ec.submit(erd.OpFKUpdate, payload); err != nil {
		return "", err
	}
	name := in.Name
	if in.NewName != nil {
		name = *in.NewName
	}
	return asJSON(map[string]any{"table": tbl.Name, "foreignKey": name, "updated": true})
}

func erdDeleteFK(ec *erdToolContext, args json.RawMessage) (string, error) {
	var in struct {
		Table string `json:"table"`
		Name  string `json:"name"`
	}
	if err := parseArgs(args, &in); err != nil {
		return "", err
	}
	tbl, err := ec.mustTable(in.Table)
	if err != nil {
		return "", err
	}
	if _, err := ec.submit(erd.OpFKDelete, map[string]any{"table": tbl.Key(), "name": in.Name}); err != nil {
		return "", err
	}
	return asJSON(map[string]any{"table": tbl.Name, "foreignKey": in.Name, "deleted": true})
}

func erdDeleteIndex(ec *erdToolContext, args json.RawMessage) (string, error) {
	var in struct {
		Table string `json:"table"`
		Name  string `json:"name"`
	}
	if err := parseArgs(args, &in); err != nil {
		return "", err
	}
	tbl, err := ec.mustTable(in.Table)
	if err != nil {
		return "", err
	}
	if _, err := ec.submit(erd.OpIndexDelete, map[string]any{"table": tbl.Key(), "name": in.Name}); err != nil {
		return "", err
	}
	return asJSON(map[string]any{"table": tbl.Name, "index": in.Name, "deleted": true})
}
