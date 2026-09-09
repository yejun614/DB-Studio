package schema

import (
	"strings"
	"testing"
)

// ---------- 검사용 도우미 ----------

func col(name string, base BaseType, nullable bool) *Column {
	return &Column{Name: name, Type: LogicalType{Base: base}, Nullable: nullable}
}

func tbl(name string, cols ...*Column) *Table {
	return &Table{Name: name, Columns: cols, Options: map[string]string{}}
}

func withPK(t *Table, cols ...string) *Table {
	t.PrimaryKey = &PrimaryKey{Columns: cols}
	return t
}

func withFK(t *Table, fk *ForeignKey) *Table {
	t.ForeignKeys = append(t.ForeignKeys, fk)
	return t
}

func withIndex(t *Table, name string, unique bool, cols ...string) *Table {
	parts := make([]IndexPart, 0, len(cols))
	for _, c := range cols {
		parts = append(parts, IndexPart{Column: c})
	}
	t.Indexes = append(t.Indexes, &Index{Name: name, Columns: parts, Unique: unique})
	return t
}

func sc(dialect string, tables ...*Table) *Schema {
	return &Schema{Dialect: dialect, Shape: ShapeRelational, Tables: tables}
}

// rules는 나온 문제들을 규칙 이름으로 모은다.
func rules(issues []Issue) map[string]Issue {
	out := map[string]Issue{}
	for _, is := range issues {
		if _, seen := out[is.Rule]; !seen {
			out[is.Rule] = is
		}
	}
	return out
}

func wantRule(t *testing.T, issues []Issue, rule, severity string) Issue {
	t.Helper()
	is, ok := rules(issues)[rule]
	if !ok {
		got := []string{}
		for _, x := range issues {
			got = append(got, x.Rule)
		}
		t.Fatalf("%s 를 찾지 못했습니다. 나온 것: %v", rule, got)
	}
	if is.Severity != severity {
		t.Errorf("%s 의 무게가 %s 입니다(기대 %s): %s", rule, is.Severity, severity, is.Message)
	}
	if strings.TrimSpace(is.Message) == "" {
		t.Errorf("%s 에 설명이 없습니다", rule)
	}
	return is
}

func noRule(t *testing.T, issues []Issue, rule string) {
	t.Helper()
	if is, ok := rules(issues)[rule]; ok {
		t.Errorf("%s 가 나오지 않아야 하는데 나왔습니다: %s", rule, is.Message)
	}
}

// ---------- 아무 문제 없는 설계 ----------

// 제대로 된 설계에서는 아무것도 나오지 않아야 한다.
//
// 이 검사가 가장 중요하다. 잘못 잡는 규칙이 하나라도 있으면 목록에 늘 뭔가가
// 떠 있고, 그러면 사람이 목록 자체를 보지 않게 된다 — 그때부터 진짜 문제도
// 함께 안 보인다.
func TestValidateCleanSchemaIsSilent(t *testing.T) {
	members := withPK(tbl("members",
		col("id", TypeBigInt, false),
		col("email", TypeVarchar, false),
	), "id")
	orders := withIndex(withFK(withPK(tbl("orders",
		col("id", TypeBigInt, false),
		col("member_id", TypeBigInt, false),
		col("amount", TypeDecimal, false),
	), "id"), &ForeignKey{
		Name: "fk_orders_member", Columns: []string{"member_id"},
		RefTable: "members", RefColumns: []string{"id"},
	}), "ix_orders_member", false, "member_id")

	if issues := Validate(sc("mysql", members, orders)); len(issues) != 0 {
		for _, is := range issues {
			t.Errorf("잘못 잡았습니다 [%s/%s] %s", is.Severity, is.Rule, is.Message)
		}
	}
}

// ---------- 외래키 ----------

func TestValidateForeignKeyErrors(t *testing.T) {
	t.Run("없는 표를 가리킨다", func(t *testing.T) {
		orders := withFK(withPK(tbl("orders",
			col("id", TypeBigInt, false), col("member_id", TypeBigInt, false),
		), "id"), &ForeignKey{
			Name: "fk1", Columns: []string{"member_id"},
			RefTable: "members", RefColumns: []string{"id"},
		})
		is := wantRule(t, Validate(sc("mysql", orders)), "fk-missing-table", SeverityError)
		if is.Table != "orders" || is.FK != "fk1" {
			t.Errorf("자리가 어긋납니다: %+v", is)
		}
	})

	t.Run("없는 컬럼에서 나간다", func(t *testing.T) {
		members := withPK(tbl("members", col("id", TypeBigInt, false)), "id")
		orders := withFK(withPK(tbl("orders", col("id", TypeBigInt, false)), "id"),
			&ForeignKey{
				Name: "fk1", Columns: []string{"member_id"},
				RefTable: "members", RefColumns: []string{"id"},
			})
		is := wantRule(t, Validate(sc("mysql", members, orders)),
			"fk-missing-local-column", SeverityError)
		if is.Column != "member_id" {
			t.Errorf("컬럼이 %q 입니다", is.Column)
		}
	})

	t.Run("없는 컬럼을 가리킨다", func(t *testing.T) {
		members := withPK(tbl("members", col("id", TypeBigInt, false)), "id")
		orders := withFK(withPK(tbl("orders",
			col("id", TypeBigInt, false), col("member_id", TypeBigInt, false),
		), "id"), &ForeignKey{
			Name: "fk1", Columns: []string{"member_id"},
			RefTable: "members", RefColumns: []string{"uid"},
		})
		wantRule(t, Validate(sc("mysql", members, orders)),
			"fk-missing-ref-column", SeverityError)
	})

	t.Run("타입이 다르다", func(t *testing.T) {
		members := withPK(tbl("members", col("id", TypeBigInt, false)), "id")
		orders := withIndex(withFK(withPK(tbl("orders",
			col("id", TypeBigInt, false), col("member_id", TypeVarchar, false),
		), "id"), &ForeignKey{
			Name: "fk1", Columns: []string{"member_id"},
			RefTable: "members", RefColumns: []string{"id"},
		}), "ix", false, "member_id")
		is := wantRule(t, Validate(sc("mysql", members, orders)),
			"fk-type-mismatch", SeverityError)
		if is.Column != "member_id" {
			t.Errorf("컬럼이 %q 입니다", is.Column)
		}
	})

	t.Run("컬럼 수가 다르다", func(t *testing.T) {
		members := withPK(tbl("members",
			col("id", TypeBigInt, false), col("tenant", TypeVarchar, false),
		), "tenant", "id")
		orders := withFK(withPK(tbl("orders",
			col("id", TypeBigInt, false), col("member_id", TypeBigInt, false),
		), "id"), &ForeignKey{
			Name: "fk1", Columns: []string{"member_id"},
			RefTable: "members", RefColumns: []string{"tenant", "id"},
		})
		wantRule(t, Validate(sc("mysql", members, orders)),
			"fk-column-count", SeverityError)
	})

	t.Run("가리키는 쪽이 유일하지 않다", func(t *testing.T) {
		members := withPK(tbl("members",
			col("id", TypeBigInt, false), col("email", TypeVarchar, false),
		), "id")
		orders := withIndex(withFK(withPK(tbl("orders",
			col("id", TypeBigInt, false), col("email", TypeVarchar, false),
		), "id"), &ForeignKey{
			Name: "fk1", Columns: []string{"email"},
			RefTable: "members", RefColumns: []string{"email"},
		}), "ix", false, "email")
		wantRule(t, Validate(sc("mysql", members, orders)),
			"fk-target-not-unique", SeverityError)
	})

	// 유니크 인덱스가 있으면 기본키가 아니어도 가리킬 수 있다.
	t.Run("유니크 인덱스를 가리키는 것은 문제가 아니다", func(t *testing.T) {
		members := withIndex(withPK(tbl("members",
			col("id", TypeBigInt, false), col("email", TypeVarchar, false),
		), "id"), "uq_email", true, "email")
		orders := withIndex(withFK(withPK(tbl("orders",
			col("id", TypeBigInt, false), col("email", TypeVarchar, false),
		), "id"), &ForeignKey{
			Name: "fk1", Columns: []string{"email"},
			RefTable: "members", RefColumns: []string{"email"},
		}), "ix", false, "email")
		noRule(t, Validate(sc("mysql", members, orders)), "fk-target-not-unique")
	})

	t.Run("SET NULL 인데 NOT NULL 이다", func(t *testing.T) {
		members := withPK(tbl("members", col("id", TypeBigInt, false)), "id")
		orders := withIndex(withFK(withPK(tbl("orders",
			col("id", TypeBigInt, false), col("member_id", TypeBigInt, false),
		), "id"), &ForeignKey{
			Name: "fk1", Columns: []string{"member_id"},
			RefTable: "members", RefColumns: []string{"id"},
			OnDelete: "SET NULL",
		}), "ix", false, "member_id")
		wantRule(t, Validate(sc("mysql", members, orders)),
			"fk-set-null-not-nullable", SeverityError)
	})

	t.Run("자기 자신을 NOT NULL 로 가리킨다", func(t *testing.T) {
		nodes := withIndex(withFK(withPK(tbl("nodes",
			col("id", TypeBigInt, false), col("parent_id", TypeBigInt, false),
		), "id"), &ForeignKey{
			Name: "fk_parent", Columns: []string{"parent_id"},
			RefTable: "nodes", RefColumns: []string{"id"},
		}), "ix", false, "parent_id")
		wantRule(t, Validate(sc("mysql", nodes)), "fk-self-not-nullable", SeverityError)
	})

	t.Run("NULL 허용 자기 참조는 문제가 아니다", func(t *testing.T) {
		nodes := withIndex(withFK(withPK(tbl("nodes",
			col("id", TypeBigInt, false), col("parent_id", TypeBigInt, true),
		), "id"), &ForeignKey{
			Name: "fk_parent", Columns: []string{"parent_id"},
			RefTable: "nodes", RefColumns: []string{"id"},
		}), "ix", false, "parent_id")
		noRule(t, Validate(sc("mysql", nodes)), "fk-self-not-nullable")
	})
}

// 관계 컬럼에 인덱스가 없으면 경고다. 만들어지고 돌아가지만 표가 커지면 아프다.
func TestValidateForeignKeyIndexWarning(t *testing.T) {
	members := withPK(tbl("members", col("id", TypeBigInt, false)), "id")
	orders := withFK(withPK(tbl("orders",
		col("id", TypeBigInt, false), col("member_id", TypeBigInt, false),
	), "id"), &ForeignKey{
		Name: "fk1", Columns: []string{"member_id"},
		RefTable: "members", RefColumns: []string{"id"},
	})
	wantRule(t, Validate(sc("mysql", members, orders)), "fk-no-index", SeverityWarning)

	// 여러 컬럼 인덱스의 **앞부분**이면 쓸 수 있다. 이것을 모르면 잘 만든 설계에
	// 경고가 뜬다.
	orders2 := withIndex(withFK(withPK(tbl("orders",
		col("id", TypeBigInt, false),
		col("member_id", TypeBigInt, false),
		col("at", TypeTimestamp, false),
	), "id"), &ForeignKey{
		Name: "fk1", Columns: []string{"member_id"},
		RefTable: "members", RefColumns: []string{"id"},
	}), "ix", false, "member_id", "at")
	noRule(t, Validate(sc("mysql", members, orders2)), "fk-no-index")

	// 기본키의 앞부분이어도 마찬가지다.
	orders3 := withFK(withPK(tbl("orders",
		col("member_id", TypeBigInt, false), col("seq", TypeInt, false),
	), "member_id", "seq"), &ForeignKey{
		Name: "fk1", Columns: []string{"member_id"},
		RefTable: "members", RefColumns: []string{"id"},
	})
	noRule(t, Validate(sc("mysql", members, orders3)), "fk-no-index")
}

// ---------- 기본키 ----------

func TestValidatePrimaryKey(t *testing.T) {
	t.Run("기본키가 없으면 경고", func(t *testing.T) {
		wantRule(t, Validate(sc("mysql", tbl("logs", col("body", TypeText, true)))),
			"no-primary-key", SeverityWarning)
	})

	t.Run("기본키 컬럼이 NULL 허용이면 오류", func(t *testing.T) {
		wantRule(t, Validate(sc("mysql",
			withPK(tbl("t", col("id", TypeBigInt, true)), "id"))),
			"pk-nullable-column", SeverityError)
	})

	t.Run("기본키가 없는 컬럼을 가리키면 오류", func(t *testing.T) {
		wantRule(t, Validate(sc("mysql",
			withPK(tbl("t", col("id", TypeBigInt, false)), "uid"))),
			"pk-missing-column", SeverityError)
	})
}

// ---------- 표·컬럼의 모양 ----------

func TestValidateTableShape(t *testing.T) {
	t.Run("컬럼이 없으면 오류", func(t *testing.T) {
		wantRule(t, Validate(sc("mysql", tbl("empty"))), "table-no-columns", SeverityError)
	})

	t.Run("같은 컬럼 이름이 둘이면 오류", func(t *testing.T) {
		// 대소문자만 다른 것도 같은 이름으로 본다.
		got := Validate(sc("mysql", withPK(tbl("t",
			col("id", TypeBigInt, false), col("ID", TypeBigInt, false),
		), "id")))
		wantRule(t, got, "duplicate-column", SeverityError)
	})

	t.Run("같은 표 이름이 둘이면 오류", func(t *testing.T) {
		a := withPK(tbl("t", col("id", TypeBigInt, false)), "id")
		b := withPK(tbl("t", col("id", TypeBigInt, false)), "id")
		wantRule(t, Validate(sc("mysql", a, b)), "duplicate-table", SeverityError)
	})

	t.Run("타입 없는 컬럼은 오류", func(t *testing.T) {
		bad := &Table{Name: "t", Columns: []*Column{{Name: "x"}}}
		wantRule(t, Validate(sc("mysql", bad)), "column-no-type", SeverityError)
	})
}

// ---------- 인덱스 ----------

func TestValidateIndexes(t *testing.T) {
	t.Run("없는 컬럼을 가리키면 오류", func(t *testing.T) {
		got := Validate(sc("mysql", withIndex(withPK(tbl("t",
			col("id", TypeBigInt, false)), "id"), "ix", false, "nope")))
		wantRule(t, got, "index-missing-column", SeverityError)
	})

	t.Run("같은 컬럼을 덮는 인덱스가 둘이면 경고", func(t *testing.T) {
		x := withIndex(withIndex(withPK(tbl("t",
			col("id", TypeBigInt, false), col("email", TypeVarchar, false),
		), "id"), "ix1", false, "email"), "ix2", false, "email")
		wantRule(t, Validate(sc("mysql", x)), "duplicate-index", SeverityWarning)
	})
}

// ---------- 뷰 ----------

func TestValidateViews(t *testing.T) {
	t.Run("정의가 없으면 오류", func(t *testing.T) {
		s := sc("mysql", withPK(tbl("t", col("id", TypeBigInt, false)), "id"))
		s.Views = []*View{{Name: "v"}}
		wantRule(t, Validate(s), "view-no-definition", SeverityError)
	})

	t.Run("표와 이름이 겹치면 오류", func(t *testing.T) {
		s := sc("mysql", withPK(tbl("t", col("id", TypeBigInt, false)), "id"))
		s.Views = []*View{{Name: "t", Definition: "SELECT 1"}}
		wantRule(t, Validate(s), "view-name-taken", SeverityError)
	})

	t.Run("대상 표 없는 구체화 뷰는 경고", func(t *testing.T) {
		s := sc("clickhouse", withPK(tbl("t", col("id", TypeBigInt, false)), "id"))
		s.Views = []*View{{Name: "mv", Definition: "SELECT 1", Materialized: true}}
		wantRule(t, Validate(s), "matview-no-target", SeverityWarning)
	})
}

// ---------- 순환 ----------

// NOT NULL 로만 이어진 고리는 첫 행을 넣을 수 없다. 오류 없이 만들어지고
// 데이터를 넣으려는 순간에야 드러난다.
func TestValidateHardCycle(t *testing.T) {
	a := withIndex(withFK(withPK(tbl("a",
		col("id", TypeBigInt, false), col("b_id", TypeBigInt, false),
	), "id"), &ForeignKey{
		Name: "fk_a_b", Columns: []string{"b_id"},
		RefTable: "b", RefColumns: []string{"id"},
	}), "ix_a", false, "b_id")
	b := withIndex(withFK(withPK(tbl("b",
		col("id", TypeBigInt, false), col("a_id", TypeBigInt, false),
	), "id"), &ForeignKey{
		Name: "fk_b_a", Columns: []string{"a_id"},
		RefTable: "a", RefColumns: []string{"id"},
	}), "ix_b", false, "a_id")

	got := Validate(sc("mysql", a, b))
	is := wantRule(t, got, "fk-hard-cycle", SeverityError)
	if !strings.Contains(is.Message, "→") {
		t.Errorf("고리의 경로를 적지 않았습니다: %s", is.Message)
	}
	// 고리에 든 표 둘 다 짚어야 한다. 한쪽만 짚으면 화면에서 한쪽만 강조된다.
	seen := map[string]bool{}
	for _, x := range got {
		if x.Rule == "fk-hard-cycle" {
			seen[x.Table] = true
		}
	}
	if !seen["a"] || !seen["b"] {
		t.Errorf("고리에 든 표를 다 짚지 않았습니다: %v", seen)
	}
}

// 한쪽이 NULL 을 허용하면 고리가 아니다. 부서와 부서장처럼 흔한 모양이라,
// 여기서 잘못 잡으면 정상 설계에 오류가 뜬다.
func TestValidateSoftCycleIsFine(t *testing.T) {
	a := withIndex(withFK(withPK(tbl("a",
		col("id", TypeBigInt, false), col("b_id", TypeBigInt, true),
	), "id"), &ForeignKey{
		Name: "fk_a_b", Columns: []string{"b_id"},
		RefTable: "b", RefColumns: []string{"id"},
	}), "ix_a", false, "b_id")
	b := withIndex(withFK(withPK(tbl("b",
		col("id", TypeBigInt, false), col("a_id", TypeBigInt, false),
	), "id"), &ForeignKey{
		Name: "fk_b_a", Columns: []string{"a_id"},
		RefTable: "a", RefColumns: []string{"id"},
	}), "ix_b", false, "a_id")
	noRule(t, Validate(sc("mysql", a, b)), "fk-hard-cycle")
}

// ---------- 방언 ----------

// ClickHouse 에는 외래키·인덱스가 없다. 그린 관계는 만들어지지 않으므로
// 그리는 동안 알려줘야 한다 — 계획을 만들 때 알면 이미 다 그린 뒤다.
func TestValidateClickHouseUnsupported(t *testing.T) {
	events := withIndex(withFK(&Table{
		Name: "events",
		Columns: []*Column{
			col("id", TypeBigInt, false), col("user_id", TypeBigInt, false),
		},
		Options:    map[string]string{"order_by": "id"},
		PrimaryKey: &PrimaryKey{Columns: []string{"id"}},
	}, &ForeignKey{
		Name: "fk1", Columns: []string{"user_id"},
		RefTable: "users", RefColumns: []string{"id"},
	}), "ix", false, "user_id")
	users := &Table{
		Name:       "users",
		Columns:    []*Column{col("id", TypeBigInt, false)},
		Options:    map[string]string{"order_by": "id"},
		PrimaryKey: &PrimaryKey{Columns: []string{"id"}},
	}

	got := Validate(sc("clickhouse", events, users))
	wantRule(t, got, "fk-unsupported", SeverityWarning)
	wantRule(t, got, "index-unsupported", SeverityWarning)
	// 없는 기능에 대해 "유일하지 않다"거나 "인덱스가 없다"고 더 말하지 않는다.
	noRule(t, got, "fk-target-not-unique")
	noRule(t, got, "fk-no-index")
}

// 정렬 키도 기본키도 없는 ClickHouse 표는 조회가 언제나 전수 조사다.
func TestValidateClickHouseNoSortKey(t *testing.T) {
	got := Validate(sc("clickhouse", tbl("t", col("id", TypeBigInt, false))))
	wantRule(t, got, "clickhouse-no-sort-key", SeverityWarning)
	// 다른 방언의 "기본키 없음" 경고와 겹쳐 두 번 말하지 않는다.
	noRule(t, got, "no-primary-key")
}

// ---------- 순서 ----------

// 목록의 순서가 매번 바뀌면 무엇이 새로 생긴 문제인지 알 수 없다.
func TestValidateOrderIsStable(t *testing.T) {
	s := sc("mysql",
		tbl("zzz", col("a", TypeText, true)),
		tbl("aaa", col("b", TypeText, true)),
		withPK(tbl("mmm", col("id", TypeBigInt, true)), "id"),
	)
	first := Validate(s)
	for range 5 {
		got := Validate(s)
		if len(got) != len(first) {
			t.Fatalf("건수가 달라집니다: %d ≠ %d", len(got), len(first))
		}
		for j := range got {
			if got[j] != first[j] {
				t.Fatalf("%d번째가 달라집니다: %+v ≠ %+v", j, got[j], first[j])
			}
		}
	}
	// 오류가 경고보다 먼저 온다.
	seenWarn := false
	for _, is := range first {
		if is.Severity == SeverityWarning {
			seenWarn = true
		} else if seenWarn {
			t.Errorf("경고 뒤에 오류가 왔습니다: %+v", is)
		}
	}
}

func TestValidateNilSchema(t *testing.T) {
	if got := Validate(nil); got != nil {
		t.Errorf("nil 스키마에서 %d건이 나왔습니다", len(got))
	}
}
