package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// 용어 사전을 툴로 찾고 쓸 수 있어야 한다.
//
// 이 테스트가 생긴 이유: 사전은 "이 팀에서 이 말은 이 물리명으로 쓴다"는 약속인데,
// ERD를 짜는 동안 그 약속이 정해진다. 그런데 사전에 적는 일만 화면을 따로 열어야
// 했다. 그래서 약속과 사전이 어긋나고, 어긋난 사전은 아무도 안 본다.
func TestGlossaryTools(t *testing.T) {
	e := newTestEnv(t)
	login(t, e, "alice")

	registry := aiTools()
	tc := &toolContext{ctx: context.Background(), srv: e.srv, user: e.user}
	run := func(name, args string) (string, error) {
		tool := registry[name]
		if tool == nil {
			t.Fatalf("툴 %s 이(가) 상자에 없습니다", name)
		}
		return tool.Run(tc, json.RawMessage(args))
	}

	// 비어 있는 사전을 찾아도 오류가 아니다.
	out, err := run("search_glossary", `{}`)
	if err != nil {
		t.Fatalf("search_glossary: %v", err)
	}
	if !strings.Contains(out, `"count": 0`) {
		t.Errorf("빈 사전 = %s", out)
	}

	if _, err := run("add_glossary_term",
		`{"term":"주문","physical":"order","note":"고객이 넣은 주문","cat1":"판매"}`); err != nil {
		t.Fatalf("add_glossary_term: %v", err)
	}

	// 같은 말을 또 넣으면 거절해야 한다. 두 줄이면 사전이 답을 두 개 준다.
	if _, err := run("add_glossary_term", `{"term":"주문","physical":"orders"}`); err == nil {
		t.Error("중복 용어가 들어갔습니다")
	} else if !strings.Contains(err.Error(), "order") {
		t.Errorf("이미 있는 물리명을 알려주지 않았습니다: %v", err)
	}

	// 찾으면 나와야 하고, 쓰이는 분류도 함께 와야 한다 — 안 보이면 매번 새 분류를
	// 만들어 사전이 흩어진다.
	out, err = run("search_glossary", `{"q":"주문"}`)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if !strings.Contains(out, "order") || !strings.Contains(out, "판매") {
		t.Errorf("찾은 결과 = %s", out)
	}
	if !strings.Contains(out, `"categories"`) {
		t.Errorf("분류 목록이 없습니다: %s", out)
	}

	// 보낸 것만 바뀐다. 안 보낸 칸을 빈 값으로 덮으면 남이 적어 둔 설명이 사라진다.
	if _, err := run("update_glossary_term",
		`{"term":"주문","physical":"purchase_order"}`); err != nil {
		t.Fatalf("update: %v", err)
	}
	out, _ = run("search_glossary", `{"q":"주문"}`)
	if !strings.Contains(out, "purchase_order") {
		t.Errorf("물리명이 안 바뀌었습니다: %s", out)
	}
	if !strings.Contains(out, "고객이 넣은 주문") {
		t.Errorf("보내지 않은 설명이 사라졌습니다: %s", out)
	}
	if !strings.Contains(out, "판매") {
		t.Errorf("보내지 않은 분류가 사라졌습니다: %s", out)
	}

	// 없는 말을 고치라고 하면 그렇게 말해야 한다.
	if _, err := run("update_glossary_term", `{"term":"없는말","physical":"x"}`); err == nil {
		t.Error("없는 용어를 고쳤습니다")
	}

	// 지우는 툴은 두지 않았다. 되돌리기가 닿지 않는 자리라 사람이 화면에서 한다.
	for _, name := range []string{"delete_glossary_term", "remove_glossary_term"} {
		if registry[name] != nil {
			t.Errorf("지우는 툴 %s 이(가) 생겼습니다", name)
		}
	}
}

// 용어와 물리명은 둘 다 있어야 한다.
func TestGlossaryToolRequiresBothNames(t *testing.T) {
	e := newTestEnv(t)
	login(t, e, "alice")
	tc := &toolContext{ctx: context.Background(), srv: e.srv, user: e.user}
	run := func(args string) error {
		_, err := aiTools()["add_glossary_term"].Run(tc, json.RawMessage(args))
		return err
	}
	if err := run(`{"term":"주문","physical":"  "}`); err == nil {
		t.Error("물리명 없이 들어갔습니다")
	}
	if err := run(`{"term":"","physical":"order"}`); err == nil {
		t.Error("용어 없이 들어갔습니다")
	}
}
