package sqlimport

import (
	"testing"

	"dbstudio/internal/schema"
)

// CREATE TYPE … AS ENUM 을 읽고, 그 타입을 쓰는 컬럼이 이름과 값을 함께 든다.
// 타입 정의가 표보다 뒤에 와도 맞춘다.
func TestParseCreateTypeEnum(t *testing.T) {
	res, err := Parse("postgres", `
CREATE TABLE orders (
  id bigint PRIMARY KEY,
  status "public"."order_status" NOT NULL DEFAULT 'new',
  kind order_kind
);
CREATE TYPE public.order_status AS ENUM ('new', 'paid');
CREATE TYPE order_kind AS ENUM ('a''b', 'c');
ALTER TYPE public.order_status ADD VALUE IF NOT EXISTS 'shipped';
ALTER TYPE public.order_status ADD VALUE 'draft' BEFORE 'new';
CREATE TYPE pair AS (x int, y int);
`)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Enums) != 2 {
		t.Fatalf("ENUM %d개를 읽었습니다: %+v", len(res.Enums), res.Enums)
	}
	st := res.Enums[0]
	if st.Namespace != "public" || st.Name != "order_status" ||
		!eq(st.Values, []string{"draft", "new", "paid", "shipped"}) {
		t.Errorf("order_status = %+v", st)
	}
	if !eq(res.Enums[1].Values, []string{"a'b", "c"}) {
		t.Errorf("따옴표가 든 값을 잘못 읽었습니다: %+v", res.Enums[1].Values)
	}

	cols := res.Tables[0].Columns
	if got := cols[1].Type; got.Base != schema.TypeEnum || got.EnumName != "order_status" || len(got.Values) != 4 {
		t.Errorf("status 컬럼 타입 = %+v", got)
	}
	if got := cols[2].Type; got.EnumName != "order_kind" {
		t.Errorf("kind 컬럼 타입 = %+v", got)
	}
	// 복합 타입은 건너뛰되 그 사실을 알린다.
	if len(res.Notes) == 0 {
		t.Error("복합 타입을 건너뛴 것을 알리지 않았습니다")
	}
}

// 타입 정의만 담긴 스크립트도 읽어낸 것이 있는 스크립트다.
func TestParseEnumOnlyScript(t *testing.T) {
	res, err := Parse("postgres", `CREATE TYPE mood AS ENUM ('sad', 'ok');`)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Enums) != 1 {
		t.Fatalf("ENUM 을 읽지 못했습니다: %+v", res)
	}
}

func eq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
