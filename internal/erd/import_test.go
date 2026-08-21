package erd

import (
	"encoding/json"
	"testing"

	"dbstudio/internal/schema"
)

func importDoc(t *testing.T) *Document {
	t.Helper()
	doc := NewDocument("d1", "초안", "c1", "postgres")
	doc.Schema.Tables = []*schema.Table{
		{Name: "users", Columns: []*schema.Column{{Name: "id", Position: 1}}},
		{Name: "orders", Columns: []*schema.Column{{Name: "id", Position: 1}},
			ForeignKeys: []*schema.ForeignKey{
				{Name: "fk_orders_user", Columns: []string{"user_id"}, RefTable: "users", RefColumns: []string{"id"}},
			}},
		{Name: "keep_me", Columns: []*schema.Column{{Name: "id", Position: 1}}},
	}
	doc.Layout = map[string]*Box{
		"users":   {X: 500, Y: 500, Color: "#22c55e"},
		"orders":  {X: 900, Y: 500},
		"keep_me": {X: 80, Y: 80},
	}
	return doc
}

func importOp(t *testing.T, payload map[string]any) *Op {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return &Op{ID: "op-1", Kind: OpSchemaImport, Payload: raw}
}

// 같은 이름의 테이블은 덮어쓰되 좌표와 색은 그대로여야 한다.
// 카드가 화면 다른 곳으로 튀면 사용자가 정리해 둔 배치가 통째로 날아간다.
func TestImportOverwritesButKeepsLayout(t *testing.T) {
	doc := importDoc(t)
	op := importOp(t, map[string]any{
		"tables": []*schema.Table{
			{Name: "users", Columns: []*schema.Column{
				{Name: "id", Position: 1}, {Name: "email", Position: 2},
			}},
		},
	})
	if err := Apply(doc, op); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(doc.Schema.Tables) != 3 {
		t.Fatalf("테이블 수: %d", len(doc.Schema.Tables))
	}
	users := doc.Schema.Table("users")
	if len(users.Columns) != 2 {
		t.Errorf("덮어쓰기가 되지 않았습니다: %+v", users.Columns)
	}
	box := doc.Layout["users"]
	if box == nil || box.X != 500 || box.Color != "#22c55e" {
		t.Errorf("좌표/색이 유지되지 않았습니다: %+v", box)
	}
}

// 새 테이블은 빈 자리에 놓인다. 좌표가 없으면 캔버스 원점에 겹쳐 쌓인다.
func TestImportPlacesNewTables(t *testing.T) {
	doc := importDoc(t)
	op := importOp(t, map[string]any{
		"tables": []*schema.Table{{Name: "brand_new", Columns: []*schema.Column{{Name: "id", Position: 1}}}},
	})
	if err := Apply(doc, op); err != nil {
		t.Fatalf("apply: %v", err)
	}
	box := doc.Layout["brand_new"]
	if box == nil {
		t.Fatal("새 테이블에 좌표가 없습니다")
	}
	for key, other := range doc.Layout {
		if key == "brand_new" {
			continue
		}
		if other.X == box.X && other.Y == box.Y {
			t.Errorf("새 테이블이 %s 와 같은 자리에 놓였습니다", key)
		}
	}
}

// DROP은 테이블과 그 테이블을 가리키던 외래키를 함께 지운다.
// 참조가 남으면 만들 수 없는 DDL이 된다.
func TestImportDropRemovesReferences(t *testing.T) {
	doc := importDoc(t)
	op := importOp(t, map[string]any{"drops": []string{"users"}})
	if err := Apply(doc, op); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if doc.Schema.Table("users") != nil {
		t.Error("users 가 남아 있습니다")
	}
	if doc.Layout["users"] != nil {
		t.Error("users 의 좌표가 남아 있습니다")
	}
	orders := doc.Schema.Table("orders")
	if len(orders.ForeignKeys) != 0 {
		t.Errorf("끊어진 외래키가 남았습니다: %+v", orders.ForeignKeys)
	}
}

// 같은 스크립트에서 DROP 뒤에 CREATE 하는 흐름이 흔하다.
// 순서가 반대로 적용되면 방금 만든 테이블이 사라진다.
func TestImportDropRunsBeforeCreate(t *testing.T) {
	doc := importDoc(t)
	op := importOp(t, map[string]any{
		"drops": []string{"users"},
		"tables": []*schema.Table{
			{Name: "users", Columns: []*schema.Column{{Name: "uuid", Position: 1}}},
		},
	})
	if err := Apply(doc, op); err != nil {
		t.Fatalf("apply: %v", err)
	}
	users := doc.Schema.Table("users")
	if users == nil {
		t.Fatal("다시 만든 users 가 없습니다")
	}
	if len(users.Columns) != 1 || users.Columns[0].Name != "uuid" {
		t.Errorf("새 정의가 아닙니다: %+v", users.Columns)
	}
}

// 식별자 검증은 불러오기에도 적용되어야 한다. 인용부호가 든 이름이 통과하면
// 생성되는 DDL의 구조가 바뀐다.
func TestImportRejectsBadIdentifiers(t *testing.T) {
	doc := importDoc(t)
	op := importOp(t, map[string]any{
		"tables": []*schema.Table{{Name: `x"; DROP TABLE y; --`}},
	})
	if err := Apply(doc, op); err == nil {
		t.Fatal("인용부호가 든 테이블 이름이 통과했습니다")
	}
	if len(doc.Schema.Tables) != 3 {
		t.Errorf("거부된 op가 문서를 바꿨습니다: %d개", len(doc.Schema.Tables))
	}
}

func TestSummarizeImport(t *testing.T) {
	doc := importDoc(t)
	sum := SummarizeImport(doc,
		[]*schema.Table{
			{Name: "users", Columns: []*schema.Column{{Name: "id"}}},
			{Name: "invoices", ForeignKeys: []*schema.ForeignKey{
				{Name: "fk_inv_missing", RefTable: "nowhere"},
			}},
		},
		[]string{"keep_me"})

	if len(sum.Updated) != 1 || sum.Updated[0] != "users" {
		t.Errorf("갱신 목록: %+v", sum.Updated)
	}
	if len(sum.Added) != 1 || sum.Added[0] != "invoices" {
		t.Errorf("추가 목록: %+v", sum.Added)
	}
	if len(sum.Dropped) != 1 || sum.Dropped[0] != "keep_me" {
		t.Errorf("삭제 목록: %+v", sum.Dropped)
	}
	if len(sum.MissingRefs) != 1 {
		t.Errorf("없는 참조를 알리지 않았습니다: %+v", sum.MissingRefs)
	}
}
