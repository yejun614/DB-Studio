package api

import (
	"context"
	"encoding/json"
	"testing"
)

// 어시스턴트가 화면의 "문제" 탭과 같은 결과를 읽고, 적힌 툴로 고쳐 문제를 없앤다.
//
// 읽기만 되고 고칠 툴이 없으면 어시스턴트는 문제를 보고도 사람에게 넘긴다. 그래서 이
// 시험은 "읽었다"가 아니라 **고친 뒤 다시 읽으면 사라졌다**를 본다.
func TestERDToolsReadAndResolveIssues(t *testing.T) {
	e := newTestEnv(t)
	c := login(t, e, "alice")
	createStandalone(t, e, c, "설계 초안", "postgres")

	registry := aiTools()
	tc := &toolContext{ctx: context.Background(), srv: e.srv, user: e.user}
	run := func(name, args string) string {
		t.Helper()
		tool := registry[name]
		if tool == nil {
			t.Fatalf("툴 %s 이(가) 상자에 없습니다", name)
		}
		out, err := tool.Run(tc, json.RawMessage(args))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return out
	}
	type issue struct {
		Severity, Rule, Table, FK, Resolve string
		Fix                                *struct {
			Tool      string          `json:"tool"`
			Arguments json.RawMessage `json:"arguments"`
		}
	}
	read := func() []issue {
		t.Helper()
		var res struct {
			Errors, Warnings int
			Issues           []issue
		}
		if err := json.Unmarshal([]byte(run("erd_read_issues", `{"document":"설계 초안"}`)), &res); err != nil {
			t.Fatal(err)
		}
		return res.Issues
	}
	has := func(list []issue, rule string) *issue {
		for i := range list {
			if list[i].Rule == rule {
				return &list[i]
			}
		}
		return nil
	}

	run("erd_add_table", `{"document":"설계 초안","name":"users"}`)
	run("erd_add_column", `{"document":"설계 초안","table":"users","name":"id","type":"bigint","nullable":false}`)
	run("erd_set_primary_key", `{"document":"설계 초안","table":"users","columns":["id"]}`)
	run("erd_add_table", `{"document":"설계 초안","name":"orders"}`)
	run("erd_add_column", `{"document":"설계 초안","table":"orders","name":"id","type":"bigint","nullable":false}`)
	run("erd_set_primary_key", `{"document":"설계 초안","table":"orders","columns":["id"]}`)
	run("erd_add_column", `{"document":"설계 초안","table":"orders","name":"user_id","type":"bigint","nullable":false}`)
	// NOT NULL 컬럼에 SET NULL — 부모를 지우는 순간 실패하는 오류다.
	run("erd_add_foreign_key", `{"document":"설계 초안","table":"orders","name":"fk_orders_user",
		"columns":["user_id"],"refTable":"users","refColumns":["id"],"onDelete":"set null"}`)
	run("erd_add_index", `{"document":"설계 초안","table":"users","name":"ix_a","columns":["id"]}`)
	run("erd_add_index", `{"document":"설계 초안","table":"users","name":"ix_b","columns":["id"]}`)

	issues := read()
	setNull := has(issues, "fk-set-null-not-nullable")
	noIndex := has(issues, "fk-no-index")
	dup := has(issues, "duplicate-index")
	if setNull == nil || noIndex == nil || dup == nil {
		t.Fatalf("문제를 다 읽지 못했습니다: %+v", issues)
	}
	if setNull.Severity != "error" || setNull.Table != "orders" || setNull.FK != "fk_orders_user" || setNull.Resolve == "" {
		t.Errorf("오류의 자리·고칠 방법이 없습니다: %+v", setNull)
	}
	if noIndex.Fix == nil || noIndex.Fix.Tool != "add_index" {
		t.Fatalf("그대로 부를 fix 가 없습니다: %+v", noIndex)
	}

	// 적힌 대로 고친다. fix 는 그대로 부른다(document 만 더한다).
	var fixArgs map[string]any
	_ = json.Unmarshal(noIndex.Fix.Arguments, &fixArgs)
	fixArgs["document"] = "설계 초안"
	raw, _ := json.Marshal(fixArgs)
	run("erd_"+noIndex.Fix.Tool, string(raw))
	run("erd_update_foreign_key", `{"document":"설계 초안","table":"orders","name":"fk_orders_user","onDelete":"cascade"}`)
	run("erd_delete_index", `{"document":"설계 초안","table":"users","name":"ix_b"}`)

	after := read()
	for _, rule := range []string{"fk-set-null-not-nullable", "fk-no-index", "duplicate-index"} {
		if is := has(after, rule); is != nil {
			t.Errorf("고쳤는데 %s 가 남았습니다: %+v", rule, is)
		}
	}

	// 관계를 지울 수도 있다. 컬럼은 남는다.
	type schemaOut struct {
		Tables []struct {
			Columns     []struct{ Name string }
			ForeignKeys []any `json:"foreignKeys"`
		}
	}
	readOrders := func() (schemaOut, string) {
		out := run("erd_read_schema", `{"document":"설계 초안","table":"orders"}`)
		var so schemaOut
		_ = json.Unmarshal([]byte(out), &so)
		return so, out
	}
	if before, out := readOrders(); len(before.Tables) != 1 || len(before.Tables[0].ForeignKeys) != 1 {
		t.Fatalf("지우기 전에 관계가 보이지 않습니다: %s", out)
	}
	run("erd_delete_foreign_key", `{"document":"설계 초안","table":"orders","name":"fk_orders_user"}`)
	if so, out := readOrders(); len(so.Tables) != 1 || len(so.Tables[0].ForeignKeys) != 0 || len(so.Tables[0].Columns) != 2 {
		t.Errorf("관계 삭제 결과 = %s", out)
	}
}

// 가리키는 곳을 바꾸면서 컬럼만 넘기면 op 가 조용히 무시한다. 그것을 성공으로 돌려주면
// 모델은 고쳤다고 믿는다.
func TestERDUpdateForeignKeyNeedsWholeTarget(t *testing.T) {
	e := newTestEnv(t)
	c := login(t, e, "alice")
	createStandalone(t, e, c, "설계 초안", "postgres")
	registry := aiTools()
	tc := &toolContext{ctx: context.Background(), srv: e.srv, user: e.user}

	for _, args := range []string{
		`{"document":"설계 초안","name":"users"}`,
	} {
		if _, err := registry["erd_add_table"].Run(tc, json.RawMessage(args)); err != nil {
			t.Fatal(err)
		}
	}
	_, err := registry["erd_update_foreign_key"].Run(tc, json.RawMessage(
		`{"document":"설계 초안","table":"users","name":"fk","columns":["a"]}`))
	if err == nil {
		t.Fatal("refTable·refColumns 없이 컬럼만 바꾸는 요청이 통과했습니다")
	}
}
