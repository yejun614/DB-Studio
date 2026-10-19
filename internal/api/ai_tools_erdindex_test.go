package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"dbstudio/internal/schema"
)

// 어시스턴트가 외래키와 그 컬럼의 인덱스를 만든다.
//
// add_index 는 컬럼을 {"column": …} 모양으로 보내고 있었고 index.add 는 이름의 배열을
// 받으므로, 이 툴은 처음부터 한 번도 성공하지 못했다. PostgreSQL 은 외래키 컬럼에 인덱스를
// 만들어 주지 않아(MySQL InnoDB 는 만든다) 어시스턴트가 가장 자주 부르는 툴 가운데 하나다.
func TestERDToolsAddForeignKeyAndIndex(t *testing.T) {
	e := newTestEnv(t)
	c := login(t, e, "alice")
	docID := createStandalone(t, e, c, "설계 초안", "postgres")

	registry := aiTools()
	tc := &toolContext{ctx: context.Background(), srv: e.srv, user: e.user}
	run := func(name, args string) {
		t.Helper()
		tool := registry[name]
		if tool == nil {
			t.Fatalf("툴 %s 이(가) 상자에 없습니다", name)
		}
		if _, err := tool.Run(tc, json.RawMessage(args)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}

	run("erd_add_table", `{"document":"설계 초안","name":"users"}`)
	run("erd_add_column", `{"document":"설계 초안","table":"users","name":"id","type":"bigint","nullable":false}`)
	run("erd_set_primary_key", `{"document":"설계 초안","table":"users","columns":["id"]}`)
	run("erd_add_table", `{"document":"설계 초안","name":"orders"}`)
	run("erd_add_column", `{"document":"설계 초안","table":"orders","name":"id","type":"bigint","nullable":false}`)
	run("erd_add_column", `{"document":"설계 초안","table":"orders","name":"user_id","type":"bigint","nullable":false}`)
	run("erd_add_foreign_key", `{"document":"설계 초안","table":"orders","name":"fk_orders_user",
		"columns":["user_id"],"refTable":"users","refColumns":["id"],"onDelete":"cascade"}`)
	run("erd_add_index", `{"document":"설계 초안","table":"orders","name":"ix_orders_user_id","columns":["user_id"]}`)

	doc, err := e.st.GetERDDocument(context.Background(), docID)
	if err != nil {
		t.Fatal(err)
	}
	var orders *schema.Table
	for _, tbl := range doc.Schema.Tables {
		if tbl.Name == "orders" {
			orders = tbl
		}
	}
	if orders == nil || len(orders.ForeignKeys) != 1 || len(orders.Indexes) != 1 {
		t.Fatalf("외래키·인덱스가 들어가지 않았습니다: %+v", orders)
	}
	if idx := orders.Indexes[0]; len(idx.Columns) != 1 || idx.Columns[0].Column != "user_id" || idx.Unique {
		t.Errorf("인덱스 = %+v", idx)
	}

	up := schema.BuildPlan("postgres", schema.Diff(
		&schema.Schema{Dialect: "postgres", Shape: schema.ShapeRelational}, doc.Schema)).UpSQL()
	for _, want := range []string{
		`CREATE INDEX IF NOT EXISTS "ix_orders_user_id" ON "orders" ("user_id")`,
		`FOREIGN KEY ("user_id") REFERENCES "users" ("id") ON DELETE CASCADE`,
	} {
		if !strings.Contains(up, want) {
			t.Errorf("%q 가 없습니다:\n%s", want, up)
		}
	}
}
