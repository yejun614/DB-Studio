package erd

import (
	"strings"
	"testing"

	"dbstudio/internal/schema"
)

// ENUM 도메인은 같은 이름의 ENUM 타입을 소유한다. 이 시험들은 도메인을 고친 뒤
// **타입과 컬럼이 어떻게 되었는가**, 그리고 그것이 PostgreSQL 에서 무엇으로
// 내보내지는가를 본다.

func enumDomainDoc(t *testing.T) *Document {
	t.Helper()
	doc := twoTables(t)
	apply(t, doc, OpDomainAdd, `{"name":"order_status","type":"ENUM('new','paid')"}`)
	apply(t, doc, OpColumnAdd, `{"table":"orders","name":"status","domain":"order_status"}`)
	apply(t, doc, OpColumnAdd, `{"table":"users","name":"last_status","domain":"order_status"}`)
	return doc
}

func pgExport(doc *Document) string {
	empty := &schema.Schema{Dialect: "postgres", Shape: schema.ShapeRelational}
	return schema.BuildPlan("postgres", schema.Diff(empty, doc.Schema)).UpSQL()
}

func TestEnumDomainOwnsType(t *testing.T) {
	doc := enumDomainDoc(t)

	e := findSchemaEnum(doc.Schema, "order_status")
	if e == nil || strings.Join(e.Values, ",") != "new,paid" {
		t.Fatalf("도메인이 타입을 만들지 않았습니다: %+v", doc.Schema.Enums)
	}
	col := doc.findTable("orders").Column("status")
	if col.Type.EnumName != "order_status" || len(col.Type.Values) != 2 {
		t.Errorf("컬럼이 타입을 가리키지 않습니다: %+v", col.Type)
	}

	up := pgExport(doc)
	if strings.Count(up, `CREATE TYPE "order_status" AS ENUM ('new', 'paid')`) != 1 {
		t.Errorf("타입이 한 번 만들어지지 않았습니다:\n%s", up)
	}
	if strings.Count(up, `"order_status"`) < 3 {
		t.Errorf("두 컬럼이 타입을 쓰지 않습니다:\n%s", up)
	}
}

func TestEnumDomainUpdateValuesAndRename(t *testing.T) {
	doc := enumDomainDoc(t)

	apply(t, doc, OpDomainUpdate, `{"name":"order_status","type":"ENUM('new','paid','shipped')"}`)
	if e := findSchemaEnum(doc.Schema, "order_status"); strings.Join(e.Values, ",") != "new,paid,shipped" {
		t.Errorf("타입의 값이 따라오지 않았습니다: %+v", e.Values)
	}
	if v := doc.findTable("users").Column("last_status").Type.Values; len(v) != 3 {
		t.Errorf("컬럼의 값이 따라오지 않았습니다: %+v", v)
	}

	apply(t, doc, OpDomainUpdate, `{"name":"order_status","newName":"order_state"}`)
	if findSchemaEnum(doc.Schema, "order_status") != nil || findSchemaEnum(doc.Schema, "order_state") == nil {
		t.Errorf("타입 이름이 따라오지 않았습니다: %+v", doc.Schema.Enums)
	}
	if got := doc.findTable("orders").Column("status").Type.EnumName; got != "order_state" {
		t.Errorf("컬럼이 옛 이름을 가리킵니다: %q", got)
	}
}

// ENUM 이 아니게 되면 아무도 쓰지 않는 옛 타입을 지운다. 남기면 주인 없는 타입이
// 마이그레이션마다 만들어진다.
func TestEnumDomainBecomesPlainType(t *testing.T) {
	doc := enumDomainDoc(t)
	apply(t, doc, OpDomainUpdate, `{"name":"order_status","type":"varchar(20)"}`)
	if len(doc.Schema.Enums) != 0 {
		t.Errorf("옛 타입이 남았습니다: %+v", doc.Schema.Enums)
	}
	if col := doc.findTable("orders").Column("status"); col.Type.Base != schema.TypeVarchar {
		t.Errorf("컬럼 타입 = %+v", col.Type)
	}
}

// 도메인을 지워도 컬럼은 타입을 그대로 쓴다. 그 타입도 남아야 한다 — 지우면 없는
// 타입을 가리키는 컬럼이 생긴다.
func TestEnumDomainDeleteKeepsUsedType(t *testing.T) {
	doc := enumDomainDoc(t)
	apply(t, doc, OpDomainDelete, `{"name":"order_status"}`)
	if findSchemaEnum(doc.Schema, "order_status") == nil {
		t.Fatal("쓰는 컬럼이 있는 타입을 지웠습니다")
	}
	if col := doc.findTable("orders").Column("status"); col.Domain != "" || col.Type.EnumName != "order_status" {
		t.Errorf("컬럼 = %+v / %q", col.Type, col.Domain)
	}

	// 쓰는 곳이 없는 ENUM 도메인을 지우면 타입도 함께 사라진다.
	apply(t, doc, OpDomainAdd, `{"name":"mood","type":"ENUM('ok','sad')"}`)
	apply(t, doc, OpDomainDelete, `{"name":"mood"}`)
	if findSchemaEnum(doc.Schema, "mood") != nil {
		t.Error("쓰는 곳 없는 타입이 남았습니다")
	}
}

// 다른 컬럼이 쓰는 ENUM 타입의 이름으로 바꾸면 거절한다. 한 이름을 두 값 목록이 나눠
// 쓸 수는 없다.
func TestEnumDomainRenameConflict(t *testing.T) {
	doc := enumDomainDoc(t)
	doc.Schema.Enums = append(doc.Schema.Enums, &schema.Enum{Name: "legacy", Values: []string{"x"}})
	doc.findTable("users").Columns = append(doc.findTable("users").Columns, &schema.Column{
		Name: "legacy", Position: 9, RawType: "legacy",
		Type: schema.LogicalType{Base: schema.TypeEnum, EnumName: "legacy", Values: []string{"x"}},
	})
	applyErr(t, doc, OpDomainUpdate, `{"name":"order_status","newName":"legacy"}`, "conflict", "legacy")
}

// 실제 DB 에서 만든 초안: ENUM 타입은 도메인으로 보이고 그 타입을 쓰는 컬럼이 묶인다.
// 내보내면 처음과 같은 이름의 타입이다.
func TestFromSchemaAdoptsEnums(t *testing.T) {
	sc := &schema.Schema{Dialect: "postgres", Shape: schema.ShapeRelational,
		Enums: []*schema.Enum{
			{Namespace: "public", Name: "order_status", Values: []string{"new", "paid"}},
			{Namespace: "public", Name: "quoted", Values: []string{"it's"}},
		},
		Tables: []*schema.Table{{Namespace: "public", Name: "orders", Columns: []*schema.Column{{
			Name: "status", Position: 1, RawType: "order_status",
			Type: schema.LogicalType{Base: schema.TypeEnum, EnumName: "order_status"},
		}}}},
	}
	before := schema.Diff(sc.Clone(), sc.Clone())
	doc := FromSchema("d", "초안", "c", sc)

	d := doc.findDomain("order_status")
	if d == nil || d.Type != "ENUM('new', 'paid')" {
		t.Fatalf("도메인이 생기지 않았습니다: %+v", doc.Domains)
	}
	if doc.findDomain("quoted") != nil {
		t.Error("따옴표가 든 값의 타입을 도메인으로 맡았습니다(ENUM('…') 표기로 되살릴 수 없습니다)")
	}
	if col := doc.Schema.Tables[0].Columns[0]; col.Domain != "order_status" || col.Type.EnumName != "order_status" {
		t.Errorf("컬럼이 묶이지 않았습니다: %+v / %q", col.Type, col.Domain)
	}

	// 도메인으로 맡았을 뿐 구조는 그대로다. 원래 DB 와 견주면 같아야 한다.
	db := &schema.Schema{Dialect: "postgres", Shape: schema.ShapeRelational,
		Enums: []*schema.Enum{
			{Namespace: "public", Name: "order_status", Values: []string{"new", "paid"}},
			{Namespace: "public", Name: "quoted", Values: []string{"it's"}},
		},
		Tables: []*schema.Table{{Namespace: "public", Name: "orders", Columns: []*schema.Column{{
			Name: "status", Position: 1, RawType: "order_status",
			Type: schema.LogicalType{Base: schema.TypeEnum, EnumName: "order_status"},
		}}}},
	}
	if diff := schema.Diff(db, doc.Schema); len(diff.Changes) != len(before.Changes) {
		t.Errorf("도메인으로 맡은 초안이 원래 DB 와 다르다고 나옵니다: %+v", diff.Changes)
	}
}

// SQL 불러오기: CREATE TYPE 은 도메인이 되고, 그 타입을 쓰는 컬럼이 묶인다. 앞서 불러온
// 타입의 이름만 적은 표를 나중에 불러와도 묶인다.
func TestImportEnumBecomesDomain(t *testing.T) {
	doc := newDoc(t)
	enumOnly := `{"enums":[{"name":"mood","values":["ok","sad"]}]}`
	apply(t, doc, OpSchemaImport, enumOnly)
	if d := doc.findDomain("mood"); d == nil || d.Type != "ENUM('ok', 'sad')" {
		t.Fatalf("도메인이 생기지 않았습니다: %+v", doc.Domains)
	}

	apply(t, doc, OpSchemaImport, `{"tables":[{"name":"diary","columns":[
		{"name":"feeling","position":1,"rawType":"mood","type":{"base":"unknown"},"nullable":true}]}]}`)
	col := doc.findTable("diary").Column("feeling")
	if col.Domain != "mood" || col.Type.EnumName != "mood" || len(col.Type.Values) != 2 {
		t.Errorf("컬럼이 ENUM 으로 묶이지 않았습니다: %+v / %q", col.Type, col.Domain)
	}
	if up := pgExport(doc); !strings.Contains(up, `CREATE TYPE "mood"`) || !strings.Contains(up, `"feeling" "mood"`) {
		t.Errorf("내보내기에 타입이 없습니다:\n%s", up)
	}

	// 같은 이름을 다시 불러오면 값을 덮어쓴다(불러오기의 규칙).
	apply(t, doc, OpSchemaImport, `{"enums":[{"name":"mood","values":["ok","sad","glad"]}]}`)
	if v := doc.findTable("diary").Column("feeling").Type.Values; len(v) != 3 {
		t.Errorf("다시 불러온 값이 컬럼에 오지 않았습니다: %+v", v)
	}
}

// 되돌리기는 도메인만이 아니라 도메인이 만든 타입과 컬럼까지 되돌려야 한다. 도메인만
// 사라지고 타입이 남으면, 아무도 만든 적 없는 CREATE TYPE 이 마이그레이션에 나온다.
func TestEnumDomainUndoRestoresType(t *testing.T) {
	doc := twoTables(t)
	apply(t, doc, OpColumnAdd, `{"table":"orders","name":"status","type":"varchar(10)"}`)
	prev := doc.Clone()

	apply(t, doc, OpDomainAdd, `{"name":"order_status","type":"ENUM('new','paid')"}`)
	apply(t, doc, OpColumnUpdate, `{"table":"orders","name":"status","domain":"order_status"}`)

	undo := Diff(doc, prev)
	if undo == nil {
		t.Fatal("되돌릴 op 가 없습니다")
	}
	if err := Apply(doc, undo); err != nil {
		t.Fatalf("되돌리기 실패: %v", err)
	}
	if len(doc.Schema.Enums) != 0 || len(doc.Domains) != 0 {
		t.Errorf("타입·도메인이 남았습니다: %+v / %+v", doc.Schema.Enums, doc.Domains)
	}
	if col := doc.findTable("orders").Column("status"); col.Type.Base != schema.TypeVarchar || col.Domain != "" {
		t.Errorf("컬럼이 되돌아가지 않았습니다: %+v / %q", col.Type, col.Domain)
	}
}
