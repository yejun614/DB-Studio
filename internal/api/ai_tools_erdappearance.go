package api

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"dbstudio/internal/erd"
)

// 도면의 겉모습(색·아이콘)을 고치는 툴.
//
// 배치 툴과 같은 이유로 승인 없이 바로 반영되고(구조가 아니다), 한 번 부른 것 전체가 한
// 묶음이라 Ctrl+Z 한 번으로 되돌아간다. 값은 화면 고르개와 **같은 목록**에서만 받는다
// (erd/appearance.go) — 아무 색이나 받으면 카드는 칠해지지만 고르개에 불이 들어오지 않는다.
//
// 표의 색·아이콘은 table.move 로 간다. 화면도 그렇게 보낸다 — 좌표·색·아이콘·논리명은
// 모두 "구조가 아닌 표시 정보"로 같은 Box 에 담기기 때문이다. 그래서 지금 좌표를 그대로
// 되돌려 보낸다. 안 그러면 색을 바꾸는 동안 카드가 (0, 0)으로 모인다.

func tintChoices() string {
	parts := make([]string, 0, len(erd.Tints))
	for _, t := range erd.Tints {
		parts = append(parts, fmt.Sprintf("%s(%s %s)", t.Name, t.Label, t.Value))
	}
	return strings.Join(parts, ", ")
}

func nonEmpty(list []string) []string {
	out := make([]string, 0, len(list))
	for _, v := range list {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

func erdAppearanceTools() []*erdTool {
	colorDesc := "색. 팔레트 이름(영어 또는 한국어)이나 그 값만 받는다: " + tintChoices() +
		`. "default" 또는 빈 문자열이면 색을 지운다(기본색)`
	return []*erdTool{{
		Name: "set_appearance",
		Description: "표 카드·그룹·메모의 색과 아이콘을 바꾼다. 여러 개를 한 번에 보낸다 — 한 번 부른 것 전체가 " +
			"한 묶음이라 사용자가 Ctrl+Z 한 번으로 되돌릴 수 있다. 구조(컬럼·키)는 바뀌지 않고 " +
			"마이그레이션에도 들어가지 않는다. 지금 색·아이콘은 read_layout 이 함께 돌려준다.\n" +
			"- 색: " + tintChoices() + ". 표 카드는 머리띠가, 그룹·메모는 바탕이 그 색이 된다.\n" +
			"- 표 아이콘: " + strings.Join(nonEmpty(erd.TableIcons), ", ") + ` ("none" 이면 없음).` + "\n" +
			"- 컬럼 아이콘: " + strings.Join(erd.ColumnIcons, ", ") +
			` ("auto" 면 타입·키를 보고 자동, "none" 이면 표시 안 함). 보낸 컬럼만 바뀐다.` + "\n" +
			"잘못된 값이 하나라도 있으면 아무것도 바꾸지 않는다.",
		Schema: objectSchema(map[string]any{
			"tables": map[string]any{
				"type":        "array",
				"description": "바꿀 표 목록. 바꿀 항목만 적는다",
				"items": objectSchema(map[string]any{
					"table": str("테이블 이름"),
					"color": str(colorDesc),
					"icon":  str(`표 아이콘 ("none" 이면 없음)`),
					"columnIcons": map[string]any{
						"type":                 "object",
						"description":          `컬럼 이름 → 아이콘 (예: {"email": "mail", "id": "auto"})`,
						"additionalProperties": map[string]any{"type": "string"},
					},
				}, "table"),
			},
			"groups": map[string]any{
				"type":        "array",
				"description": "바꿀 그룹 목록",
				"items": objectSchema(map[string]any{
					"group": str("그룹 ID 또는 이름"),
					"color": str(colorDesc),
				}, "group", "color"),
			},
			"notes": map[string]any{
				"type":        "array",
				"description": "바꿀 메모 목록 (ID 는 read_layout 의 notes 에 있다)",
				"items": objectSchema(map[string]any{
					"note":  str("메모 ID"),
					"color": str(colorDesc),
				}, "note", "color"),
			},
		}),
		Run: erdSetAppearance,
	}}
}

func erdSetAppearance(ec *erdToolContext, args json.RawMessage) (string, error) {
	var in struct {
		Tables []struct {
			Table       string            `json:"table"`
			Color       *string           `json:"color"`
			Icon        *string           `json:"icon"`
			ColumnIcons map[string]string `json:"columnIcons"`
		} `json:"tables"`
		Groups []struct {
			Group string `json:"group"`
			Color string `json:"color"`
		} `json:"groups"`
		Notes []struct {
			Note  string `json:"note"`
			Color string `json:"color"`
		} `json:"notes"`
	}
	if err := parseArgs(args, &in); err != nil {
		return "", err
	}
	if len(in.Tables)+len(in.Groups)+len(in.Notes) == 0 {
		return "", fmt.Errorf("바꿀 것이 없습니다. tables·groups·notes 중 하나 이상을 적어주세요")
	}
	doc, err := ec.document()
	if err != nil {
		return "", err
	}
	color := func(where, v string) (string, error) {
		c, ok := erd.ResolveTint(v)
		if !ok {
			return "", fmt.Errorf("%s: %q 은(는) 팔레트에 없는 색입니다. 고를 수 있는 색: %s", where, v, tintChoices())
		}
		return c, nil
	}

	// 먼저 전부 확인하고 마지막에 한꺼번에 보낸다. 절반만 칠해진 도면은 처음보다 어지럽다.
	type op struct {
		kind    erd.Kind
		payload map[string]any
	}
	ops := []op{}
	seen := map[string]bool{}
	changed := []string{}

	for _, t := range in.Tables {
		tbl := findERDTable(doc, t.Table)
		if tbl == nil {
			return "", fmt.Errorf("테이블 %q을(를) 찾을 수 없습니다", t.Table)
		}
		key := tbl.Key()
		if seen["t:"+key] {
			return "", fmt.Errorf("%s 을(를) 두 번 적었습니다. 한 번에 모아 적으세요", tbl.Name)
		}
		seen["t:"+key] = true
		// 좌표는 지금 그대로. 좌표가 없는 카드는 화면이 (80, 80)에 그리고 있으므로 그 자리다.
		x, y := 80.0, 80.0
		if box := doc.Layout[key]; box != nil {
			x, y = box.X, box.Y
		}
		p := map[string]any{"key": key, "x": x, "y": y}
		what := []string{}
		if t.Color != nil {
			c, err := color(tbl.Name, *t.Color)
			if err != nil {
				return "", err
			}
			p["color"] = c
			what = append(what, "색")
		}
		if t.Icon != nil {
			ic, ok := erd.ValidTableIcon(*t.Icon)
			if !ok {
				return "", fmt.Errorf("%s: %q 은(는) 표 아이콘에 없습니다. 고를 수 있는 것: %s",
					tbl.Name, *t.Icon, strings.Join(nonEmpty(erd.TableIcons), ", "))
			}
			p["icon"] = ic
			what = append(what, "아이콘")
		}
		if len(t.ColumnIcons) > 0 {
			cols := map[string]string{}
			for name, v := range t.ColumnIcons {
				col := tbl.Column(name)
				if col == nil {
					return "", fmt.Errorf("%s 테이블에 %q 컬럼이 없습니다", tbl.Name, name)
				}
				ic, ok := erd.ValidColumnIcon(v)
				if !ok {
					return "", fmt.Errorf("%s.%s: %q 은(는) 컬럼 아이콘에 없습니다. 고를 수 있는 것: auto, none, %s",
						tbl.Name, col.Name, v, strings.Join(erd.ColumnIcons, ", "))
				}
				// table.move 에서 빈 값은 "자동으로 되돌리기"(지우기)다.
				cols[col.Name] = ic
			}
			p["columnIcons"] = cols
			what = append(what, fmt.Sprintf("컬럼 아이콘 %d개", len(cols)))
		}
		if len(what) == 0 {
			return "", fmt.Errorf("%s: 바꿀 것(color·icon·columnIcons)이 없습니다", tbl.Name)
		}
		ops = append(ops, op{erd.OpTableMove, p})
		changed = append(changed, tbl.Name+" ("+strings.Join(what, "·")+")")
	}
	for _, g := range in.Groups {
		grp, err := findGroup(doc, g.Group)
		if err != nil {
			return "", err
		}
		if seen["g:"+grp.ID] {
			return "", fmt.Errorf("그룹 %s 을(를) 두 번 적었습니다", groupName(grp))
		}
		seen["g:"+grp.ID] = true
		c, err := color("그룹 "+groupName(grp), g.Color)
		if err != nil {
			return "", err
		}
		ops = append(ops, op{erd.OpGroupUpdate, map[string]any{"id": grp.ID, "color": c}})
		changed = append(changed, "그룹 "+groupName(grp))
	}
	for _, n := range in.Notes {
		note := doc.Note(strings.TrimSpace(n.Note))
		if note == nil {
			return "", fmt.Errorf("메모 %q 를 찾을 수 없습니다. read_layout 의 notes 에서 ID 를 확인하세요", n.Note)
		}
		if seen["n:"+note.ID] {
			return "", fmt.Errorf("메모 %s 를 두 번 적었습니다", note.ID)
		}
		seen["n:"+note.ID] = true
		c, err := color("메모 "+note.ID, n.Color)
		if err != nil {
			return "", err
		}
		ops = append(ops, op{erd.OpNoteUpdate, map[string]any{"id": note.ID, "color": c}})
		changed = append(changed, "메모 "+note.ID)
	}
	if len(ops) > arrangeMaxOps {
		return "", fmt.Errorf("한 번에 바꿀 수 있는 것은 %d개까지입니다(요청 %d개). 나눠서 부르세요", arrangeMaxOps, len(ops))
	}

	batch := uuid.NewString()
	for _, o := range ops {
		if _, err := ec.submitBatch(o.kind, o.payload, batch); err != nil {
			return "", fmt.Errorf("반영하지 못했습니다: %w", err)
		}
	}
	return asJSON(map[string]any{
		"ok": true, "changed": capList(changed),
		"note": "한 묶음으로 반영했습니다. 사용자가 도면에서 Ctrl+Z 한 번으로 통째로 되돌릴 수 있습니다.",
	})
}
