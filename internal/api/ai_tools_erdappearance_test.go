package api

import (
	"encoding/json"
	"testing"

	"dbstudio/internal/erd"
)

// 색·아이콘 툴이 지키는 것: 고른 값이 문서에 남고, 좌표는 그대로이며, 화면 고르개에
// 없는 값은 하나도 반영하지 않고 거절한다.

func TestERDSetAppearance(t *testing.T) {
	e, ec, impls := layoutFixture(t)
	if _, err := impls["arrange_layout"].Run(ec, json.RawMessage(
		`{"groups":[{"new":true,"label":"회원","x":0,"y":0,"tables":["users"]}],
		  "tables":[{"table":"orders","x":700,"y":40}]}`)); err != nil {
		t.Fatalf("arrange_layout: %v", err)
	}
	if _, err := ec.submit(erd.OpNoteAdd, map[string]any{"id": "n1", "text": "메모", "x": 900, "y": 900}); err != nil {
		t.Fatalf("note.add: %v", err)
	}
	before := layoutDoc(t, e, ec)

	out, err := impls["set_appearance"].Run(ec, json.RawMessage(`{
		"tables":[
			{"table":"users","color":"파랑","icon":"users","columnIcons":{"email":"mail","id":"none"}},
			{"table":"orders","color":"#F97316","icon":"cart"}],
		"groups":[{"group":"회원","color":"blue"}],
		"notes":[{"note":"n1","color":"yellow"}]
	}`))
	if err != nil {
		t.Fatalf("set_appearance: %v", err)
	}
	doc := layoutDoc(t, e, ec)
	u, o := doc.Layout["users"], doc.Layout["orders"]
	if u.Color != "#3b82f6" || u.Icon != "users" {
		t.Errorf("users = 색 %q 아이콘 %q", u.Color, u.Icon)
	}
	if u.ColumnIcons["email"] != "mail" || u.ColumnIcons["id"] != "none" {
		t.Errorf("users 컬럼 아이콘 = %v", u.ColumnIcons)
	}
	if o.Color != "#f97316" || o.Icon != "cart" {
		t.Errorf("orders = 색 %q 아이콘 %q (팔레트 값은 대소문자를 가리지 않아야 한다)", o.Color, o.Icon)
	}
	if doc.Groups[0].Color != "#3b82f6" {
		t.Errorf("그룹 색 = %q", doc.Groups[0].Color)
	}
	if doc.Note("n1").Color != "#eab308" {
		t.Errorf("메모 색 = %q", doc.Note("n1").Color)
	}
	// 색을 바꾸는 동안 카드가 움직이면 안 된다.
	for _, k := range []string{"users", "orders"} {
		if doc.Layout[k].X != before.Layout[k].X || doc.Layout[k].Y != before.Layout[k].Y {
			t.Errorf("%s 가 움직였습니다: %+v → %+v", k, before.Layout[k], doc.Layout[k])
		}
	}
	if doc.Seq-before.Seq != 4 {
		t.Errorf("op 수 = %d, 표 둘·그룹 하나·메모 하나여야 합니다 (%s)", doc.Seq-before.Seq, out)
	}

	// "default" 는 색을 지우고, "auto" 는 컬럼 아이콘을 자동으로 되돌린다.
	if _, err := impls["set_appearance"].Run(ec, json.RawMessage(
		`{"tables":[{"table":"users","color":"default","icon":"none","columnIcons":{"email":"auto"}}]}`)); err != nil {
		t.Fatalf("set_appearance: %v", err)
	}
	u = layoutDoc(t, e, ec).Layout["users"]
	if u.Color != "" || u.Icon != "" {
		t.Errorf("기본으로 되돌리지 않았습니다: 색 %q 아이콘 %q", u.Color, u.Icon)
	}
	if _, ok := u.ColumnIcons["email"]; ok {
		t.Errorf("email 아이콘이 자동으로 돌아가지 않았습니다: %v", u.ColumnIcons)
	}

	// read_layout 이 지금 색·아이콘을 돌려준다.
	if _, err := impls["set_appearance"].Run(ec, json.RawMessage(`{"tables":[{"table":"items","color":"green","icon":"box"}]}`)); err != nil {
		t.Fatalf("set_appearance: %v", err)
	}
	read, err := impls["read_layout"].Run(ec, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("read_layout: %v", err)
	}
	var res struct {
		Cards []struct {
			Name, Color, Icon string
		} `json:"cards"`
		Groups []struct{ Color string } `json:"groups"`
	}
	if err := json.Unmarshal([]byte(read), &res); err != nil {
		t.Fatalf("결과가 JSON 이 아닙니다: %v", err)
	}
	found := false
	for _, c := range res.Cards {
		if c.Name == "items" {
			found = c.Color == "#22c55e" && c.Icon == "box"
		}
	}
	if !found || res.Groups[0].Color != "#3b82f6" {
		t.Errorf("read_layout 이 색·아이콘을 돌려주지 않습니다: %s", read)
	}
}

func TestERDSetAppearanceRejectsUnknownValues(t *testing.T) {
	e, ec, impls := layoutFixture(t)
	before := layoutDoc(t, e, ec)
	for name, body := range map[string]string{
		"팔레트에 없는 색": `{"tables":[{"table":"users","color":"green"},{"table":"orders","color":"#123456"}]}`,
		"없는 표 아이콘":  `{"tables":[{"table":"users","icon":"rocket"}]}`,
		"없는 컬럼 아이콘": `{"tables":[{"table":"users","columnIcons":{"email":"rocket"}}]}`,
		"없는 컬럼":     `{"tables":[{"table":"users","columnIcons":{"ghost":"mail"}}]}`,
		"없는 표":      `{"tables":[{"table":"ghost","color":"red"}]}`,
		"바꿀 것 없는 표": `{"tables":[{"table":"users"}]}`,
		"없는 그룹":     `{"groups":[{"group":"없음","color":"red"}]}`,
		"없는 메모":     `{"notes":[{"note":"none","color":"red"}]}`,
		"빈 요청":      `{}`,
		"같은 표 두 번":  `{"tables":[{"table":"users","color":"red"},{"table":"users","icon":"users"}]}`,
	} {
		if _, err := impls["set_appearance"].Run(ec, json.RawMessage(body)); err == nil {
			t.Errorf("%s: 통과했습니다", name)
		}
	}
	// 앞의 항목이 맞았어도 뒤의 항목이 틀리면 아무것도 반영하지 않는다.
	if after := layoutDoc(t, e, ec); after.Seq != before.Seq {
		t.Errorf("거절된 요청이 문서를 바꿨습니다 (seq %d → %d)", before.Seq, after.Seq)
	}
}
