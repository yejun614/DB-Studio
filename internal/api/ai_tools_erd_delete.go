package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"dbstudio/internal/erd"
	"dbstudio/internal/schema"
)

// 초안에서 컬럼을 지우는 툴. **이것만 승인이 필요하다.**
//
// 나머지 초안 편집 툴에 승인을 두지 않은 이유는 ai_tools_erd.go 에 적어 두었다:
// 대상이 초안이고, 모든 변경이 op-log 에 남고, 컬럼 다섯 개를 더하는 데 승인 다섯
// 번을 요구하면 아무도 쓰지 않는다.
//
// 지우기는 그 셋 가운데 첫째가 다르다. 더하기는 잘못돼도 그 자리에 남는 군더더기지만,
// 컬럼을 지우면 그 컬럼에 딸린 것들이 함께 사라진다 — 기본키의 한 자리, 그 컬럼을
// 쓰는 인덱스, 그 컬럼을 건드리는 외래키, 논리명과 주석. 되돌리기(op-log)로 돌아오긴
// 하지만, "돌릴 수 있다"는 것과 "무엇이 사라지는지 보고 결정했다"는 다른 일이다.
//
// 그래서 제안(Propose)에서 **함께 사라지는 것들을 미리 세어** 보여준다. 승인 화면이
// 하는 일은 그것이다: 사람이 결정을 내릴 근거를 화면에 두는 것.

// erdProposeDeleteColumn은 지우기 제안을 만든다. 아무것도 바꾸지 않는다.
func erdProposeDeleteColumn(ec *erdToolContext, args json.RawMessage) (string, any, error) {
	in, tbl, col, err := erdDeleteTarget(ec, args)
	if err != nil {
		return "", nil, err
	}
	doc, err := ec.document()
	if err != nil {
		return "", nil, err
	}

	preview := map[string]any{
		"document": doc.Name,
		"table":    tbl.Name,
		"column":   col.Name,
		"type":     col.RawType,
		"nullable": col.Nullable,
	}
	if c := strings.TrimSpace(col.Comment); c != "" {
		preview["comment"] = c
	}
	if d := strings.TrimSpace(col.Domain); d != "" {
		preview["domain"] = d
	}

	// 함께 사라지는 것들. 이름을 그대로 담는다 — "인덱스 2개"보다 "idx_orders_memo,
	// uq_orders_memo"가 결정에 필요한 정보다.
	drops := erdColumnDependents(doc, tbl, col.Name)
	if len(drops) > 0 {
		preview["alsoAffected"] = drops
	}
	summary := fmt.Sprintf("초안 %q 의 %s.%s 컬럼을 지웁니다", doc.Name, tbl.Name, col.Name)
	if len(drops) > 0 {
		summary += fmt.Sprintf(" (%s 도 함께 영향받습니다)", strings.Join(drops, ", "))
	}
	_ = in
	return summary, preview, nil
}

// erdDeleteColumn은 승인 뒤의 실행이다.
func erdDeleteColumn(ec *erdToolContext, args json.RawMessage) (string, error) {
	_, tbl, col, err := erdDeleteTarget(ec, args)
	if err != nil {
		return "", err
	}
	doc, err := ec.document()
	if err != nil {
		return "", err
	}
	drops := erdColumnDependents(doc, tbl, col.Name)

	// 사람이 인스펙터에서 지울 때와 같은 op 를 같은 경로로 보낸다(submit 주석).
	// 딸린 것들을 여기서 손보지 않는 이유: 무엇이 함께 정리되는지는 서버의 op 적용
	// 규칙이 정한다. 여기서 흉내 내면 두 벌의 규칙이 생기고, 언젠가 어긋난다.
	if _, err := ec.submit(erd.OpColumnDelete, map[string]any{
		"table": tbl.Key(), "name": col.Name,
	}); err != nil {
		return "", err
	}
	out := map[string]any{
		"document": doc.Name, "table": tbl.Name, "deleted": col.Name,
	}
	if len(drops) > 0 {
		out["alsoAffected"] = drops
	}
	return asJSON(out)
}

// erdDeleteTarget은 인자가 가리키는 테이블과 컬럼을 찾는다.
//
// 제안과 실행이 같은 함수를 쓰는 이유: 승인은 며칠 뒤에 눌릴 수 있고, 그 사이에
// 그 컬럼이 사라졌을 수 있다. 실행 쪽에서 다시 찾지 않으면 없는 컬럼을 지우라는
// op 가 나가고, 그 거절 사유는 사람이 승인한 것과 아무 상관이 없는 말이 된다.
func erdDeleteTarget(ec *erdToolContext, args json.RawMessage) (
	struct {
		Table  string `json:"table"`
		Column string `json:"column"`
	}, *schema.Table, *schema.Column, error,
) {
	var in struct {
		Table  string `json:"table"`
		Column string `json:"column"`
	}
	if err := parseArgs(args, &in); err != nil {
		return in, nil, nil, err
	}
	if strings.TrimSpace(in.Column) == "" {
		return in, nil, nil, errors.New("지울 컬럼 이름(column)을 지정하세요")
	}
	tbl, err := ec.mustTable(in.Table)
	if err != nil {
		return in, nil, nil, err
	}
	col := tbl.Column(in.Column)
	if col == nil {
		names := make([]string, 0, len(tbl.Columns))
		for _, c := range tbl.Columns {
			names = append(names, c.Name)
		}
		return in, nil, nil, fmt.Errorf("%s 에 컬럼 %q 이(가) 없습니다. 이 테이블의 컬럼: %s",
			tbl.Name, in.Column, strings.Join(names, ", "))
	}
	return in, tbl, col, nil
}

// erdColumnDependents는 이 컬럼을 지우면 함께 영향받는 것들의 이름이다.
//
// 이 표 안의 것만 세지 않는다. 다른 표가 이 컬럼을 **가리키고** 있으면 그 외래키가
// 갈 곳을 잃는데, 그것은 지우는 표가 아니라 저쪽 표에서 드러난다 — 화면에서 지울
// 때보다 여기서 더 중요한 정보다. 대화에서는 그 표가 눈앞에 없기 때문이다.
func erdColumnDependents(doc *erd.Document, tbl *schema.Table, column string) []string {
	name := strings.ToLower(strings.TrimSpace(column))
	uses := func(cols []string) bool {
		for _, c := range cols {
			if strings.ToLower(c) == name {
				return true
			}
		}
		return false
	}
	out := []string{}

	if pk := tbl.PrimaryKey; pk != nil && uses(pk.Columns) {
		if len(pk.Columns) > 1 {
			out = append(out, fmt.Sprintf("기본키 (%s)", strings.Join(pk.Columns, ", ")))
		} else {
			out = append(out, "기본키")
		}
	}
	for _, idx := range tbl.Indexes {
		cols := make([]string, 0, len(idx.Columns))
		for _, c := range idx.Columns {
			cols = append(cols, c.Column)
		}
		if uses(cols) {
			out = append(out, "인덱스 "+idx.Name)
		}
	}
	for _, fk := range tbl.ForeignKeys {
		if uses(fk.Columns) {
			out = append(out, "외래키 "+fk.Name)
		}
	}
	// 이 컬럼을 가리키는 다른 표의 외래키.
	for _, other := range doc.Schema.Tables {
		if other.Key() == tbl.Key() {
			continue
		}
		for _, fk := range other.ForeignKeys {
			if !strings.EqualFold(fk.RefTable, tbl.Name) {
				continue
			}
			if uses(fk.RefColumns) {
				out = append(out, fmt.Sprintf("%s 의 외래키 %s (이 컬럼을 참조)", other.Name, fk.Name))
			}
		}
	}
	return out
}
