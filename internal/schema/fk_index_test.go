package schema

import (
	"strings"
	"testing"
)

func fkIndexSchema(dialect string, indexes ...*Index) *Schema {
	return &Schema{Dialect: dialect, Shape: ShapeRelational, Tables: []*Table{
		{
			Name:       "users",
			Columns:    []*Column{{Name: "id", Position: 1, Type: LogicalType{Base: TypeBigInt}}},
			PrimaryKey: &PrimaryKey{Columns: []string{"id"}},
		},
		{
			Name: "orders",
			Columns: []*Column{
				{Name: "id", Position: 1, Type: LogicalType{Base: TypeBigInt}},
				{Name: "user_id", Position: 2, Type: LogicalType{Base: TypeBigInt}},
				{Name: "created_at", Position: 3, Type: LogicalType{Base: TypeTimestamp}},
			},
			PrimaryKey: &PrimaryKey{Columns: []string{"id"}},
			Indexes:    indexes,
			ForeignKeys: []*ForeignKey{{
				Name: "fk_orders_user", Columns: []string{"user_id"},
				RefTable: "users", RefColumns: []string{"id"},
			}},
		},
	}}
}

func fkNoIndex(issues []Issue) []Issue {
	var out []Issue
	for _, is := range issues {
		if is.Rule == "fk-no-index" {
			out = append(out, is)
		}
	}
	return out
}

// PostgreSQL 은 외래키 컬럼에 인덱스를 만들지 않는다. 없으면 알리고, 고칠 인덱스를
// 함께 내놓는다.
func TestFKWithoutIndexWarns(t *testing.T) {
	got := fkNoIndex(Validate(fkIndexSchema("postgres")))
	if len(got) != 1 {
		t.Fatalf("경고 %d건: %+v", len(got), got)
	}
	is := got[0]
	if is.Severity != SeverityWarning || is.Table != "orders" || is.FK != "fk_orders_user" {
		t.Errorf("경고 = %+v", is)
	}
	if !strings.Contains(is.Hint, "PostgreSQL") {
		t.Errorf("어느 DB 이야기인지 없습니다: %s", is.Hint)
	}
	fix := is.FixIndex
	if fix == nil || fix.Name != "ix_orders_user_id" || len(fix.Columns) != 1 ||
		fix.Columns[0].Column != "user_id" || fix.Unique {
		t.Errorf("고칠 인덱스 = %+v", fix)
	}

	// 고칠 인덱스를 넣으면 경고가 사라진다.
	if got := fkNoIndex(Validate(fkIndexSchema("postgres", fix))); len(got) != 0 {
		t.Errorf("제안한 인덱스를 넣었는데 경고가 남았습니다: %+v", got)
	}
}

// 외래키 컬럼이 인덱스의 앞부분이면 충분하다. 뒷부분이면 그렇지 않다.
// (규칙 자체는 validate_test 가 고정한다. 여기서는 제안한 인덱스가 그 규칙을 통과하는지 본다.)
func TestFKIndexMustLead(t *testing.T) {
	leading := &Index{Name: "ix_user_time", Columns: []IndexPart{{Column: "user_id"}, {Column: "created_at"}}}
	if got := fkNoIndex(Validate(fkIndexSchema("postgres", leading))); len(got) != 0 {
		t.Errorf("앞부분 인덱스를 인정하지 않습니다: %+v", got)
	}
	trailing := &Index{Name: "ix_time_user", Columns: []IndexPart{{Column: "created_at"}, {Column: "user_id"}}}
	if got := fkNoIndex(Validate(fkIndexSchema("postgres", trailing))); len(got) != 1 {
		t.Errorf("뒷부분 인덱스를 받쳐 준다고 봤습니다: %+v", got)
	}
}

// 고칠 인덱스는 어느 DB 에서나 내놓는다(MySQL 도 초안에 적어 두어야 실제 DB 와 같다).
// "DB 가 만들어 주지 않는다"는 말은 만들어 주지 않는 DB 에서만 한다.
func TestFKIndexHintPerDialect(t *testing.T) {
	for d, says := range map[string]bool{"mysql": false, "mssql": true, "oracle": true, "sqlite": true} {
		got := fkNoIndex(Validate(fkIndexSchema(d)))
		if len(got) != 1 || got[0].FixIndex == nil {
			t.Errorf("%s: 경고·제안 = %+v", d, got)
			continue
		}
		if has := strings.Contains(got[0].Hint, "만들어 주지 않습니다"); has != says {
			t.Errorf("%s: 힌트 = %s", d, got[0].Hint)
		}
	}
	if got := fkNoIndex(Validate(fkIndexSchema("clickhouse"))); len(got) != 0 {
		t.Errorf("ClickHouse 에서 경고했습니다: %+v", got)
	}
}

// PostgreSQL 의 인덱스 이름은 스키마 안에서 유일하다. 다른 표가 이미 쓰는 이름이면
// 피한다.
func TestFKIndexNameAvoidsTaken(t *testing.T) {
	sc := fkIndexSchema("postgres")
	sc.Tables[0].Indexes = []*Index{{Name: "ix_orders_user_id", Columns: []IndexPart{{Column: "id"}}}}
	got := fkNoIndex(Validate(sc))
	if len(got) != 1 || got[0].FixIndex.Name != "ix_orders_user_id_2" {
		t.Errorf("이름이 겹칩니다: %+v", got)
	}
}
