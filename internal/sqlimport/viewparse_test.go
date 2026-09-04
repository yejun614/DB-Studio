package sqlimport

import "testing"

func TestParseCreateView(t *testing.T) {
	res, err := Parse("postgres", `
CREATE TABLE orders (id bigint primary key, total numeric(12,2));
CREATE OR REPLACE VIEW public.daily_sales AS
  SELECT date_trunc('day', o.created_at) AS day, sum(o.total) AS amount
  FROM orders o
  GROUP BY 1;
DROP VIEW old_sales;
`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(res.Views) != 1 {
		t.Fatalf("뷰 수: %d", len(res.Views))
	}
	v := res.Views[0]
	if v.Name != "daily_sales" || v.Namespace != "public" {
		t.Errorf("이름: %q / %q", v.Namespace, v.Name)
	}
	if got := v.Definition; got == "" || got[:6] != "SELECT" {
		t.Errorf("정의: %q", got)
	}
	if len(res.ViewDrops) != 1 || res.ViewDrops[0] != "old_sales" {
		t.Errorf("드롭: %v", res.ViewDrops)
	}
}
