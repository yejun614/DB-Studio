package dbx

import (
	"strings"
	"testing"

	"dbstudio/internal/schema"
)

// renderedOnUpdateBody는 DDL 생성기가 만든 자동 갱신 함수의 본문이다. 읽는 쪽을 손으로
// 적은 문자열로 시험하면 만드는 쪽이 바뀌어도 시험은 통과한다 — 둘이 같은 모양을
// 오가는지가 시험할 것이다.
func renderedOnUpdateBody(t *testing.T, cols ...*schema.Column) string {
	t.Helper()
	sc := &schema.Schema{Dialect: "postgres", Shape: schema.ShapeRelational, Tables: []*schema.Table{{
		Name: "posts", Columns: cols,
	}}}
	up := schema.BuildPlan("postgres", schema.Diff(&schema.Schema{Dialect: "postgres", Shape: schema.ShapeRelational}, sc)).UpSQL()
	_, rest, ok := strings.Cut(up, "RETURNS trigger AS $$")
	body, _, ok2 := strings.Cut(rest, "$$ LANGUAGE plpgsql")
	if !ok || !ok2 {
		t.Fatalf("함수 본문을 찾지 못했습니다:\n%s", up)
	}
	return body
}

func TestOnUpdateTriggerRoundTrip(t *testing.T) {
	body := renderedOnUpdateBody(t,
		&schema.Column{Name: "id", Position: 1, Type: schema.LogicalType{Base: schema.TypeBigInt}},
		&schema.Column{Name: "updated_at", Position: 2, Type: schema.LogicalType{Base: schema.TypeTimestamp},
			OnUpdate: "now(3)"},
		// 따옴표와 공백이 든 이름도 같은 이름으로 돌아와야 한다.
		&schema.Column{Name: `touched "at"`, Position: 3, Type: schema.LogicalType{Base: schema.TypeTimestamp},
			OnUpdate: "CURRENT_TIMESTAMP"},
	)
	got, ok := pgOnUpdateFromTrigger(body)
	if !ok {
		t.Fatalf("우리가 만든 본문을 알아보지 못했습니다:\n%s", body)
	}
	want := map[string]string{"updated_at": "CURRENT_TIMESTAMP(3)", `touched "at"`: "CURRENT_TIMESTAMP"}
	if len(got) != len(want) {
		t.Fatalf("읽은 컬럼 %v, 기댓값 %v", got, want)
	}
	for name, expr := range want {
		if got[name] != expr {
			t.Errorf("%s: %q, 기댓값 %q", name, got[name], expr)
		}
	}
}

// 사람이 고친 함수는 컬럼의 성질로 읽지 않는다. 읽으면 다음 마이그레이션이 그것을
// 우리 본문으로 덮어쓴다.
func TestOnUpdateTriggerIgnoresEditedBody(t *testing.T) {
	body := renderedOnUpdateBody(t,
		&schema.Column{Name: "updated_at", Position: 1, Type: schema.LogicalType{Base: schema.TypeTimestamp},
			OnUpdate: "CURRENT_TIMESTAMP"})
	line := `    IF NEW."updated_at" IS NOT DISTINCT FROM OLD."updated_at" THEN NEW."updated_at" := CURRENT_TIMESTAMP; END IF;`
	if !strings.Contains(body, line) {
		t.Fatalf("본문 모양이 바뀌었습니다. 이 시험의 고친 본문도 맞춰야 합니다:\n%s", body)
	}
	edited := map[string]string{
		"다른 식":    strings.Replace(body, ":= CURRENT_TIMESTAMP;", ":= clock_timestamp();", 1),
		"다른 컬럼":   strings.Replace(body, `NEW."updated_at" :=`, `NEW."other" :=`, 1),
		"덧붙인 줄":   strings.Replace(body, "  RETURN NEW;", "  PERFORM pg_notify('x', 'y');\n  RETURN NEW;", 1),
		"조건 없이":   strings.Replace(body, "  IF NEW IS DISTINCT FROM OLD THEN\n", "", 1),
		"컬럼이 하나도": strings.Replace(body, line+"\n", "", 1),
	}
	for name, src := range edited {
		if src == body {
			t.Fatalf("%s: 고치지 못했습니다", name)
		}
		if got, ok := pgOnUpdateFromTrigger(src); ok {
			t.Errorf("%s: 고친 본문을 우리 것으로 읽었습니다: %v", name, got)
		}
	}
}

// PostgreSQL 은 63바이트를 넘는 식별자를 글자 경계에서 자른다. 긴 표 이름의 트리거는
// 잘린 이름으로 저장되므로, 이름을 견줄 때도 같게 잘라야 한다.
func TestPgIdentifierClipsAtCharBoundary(t *testing.T) {
	if got := pgIdentifier("posts_on_update"); got != "posts_on_update" {
		t.Errorf("짧은 이름을 바꿨습니다: %q", got)
	}
	long := strings.Repeat("a", 60) + "_on_update"
	if got := pgIdentifier(long); got != long[:63] {
		t.Errorf("%q", got)
	}
	// 한글은 3바이트다. 21자 = 63바이트, 그 뒤는 잘린다.
	korean := strings.Repeat("가", 22) + "_on_update"
	if got := pgIdentifier(korean); got != strings.Repeat("가", 21) {
		t.Errorf("글자 가운데에서 잘랐습니다: %q", got)
	}
}
