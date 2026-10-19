package schema

import (
	"strings"
	"testing"
)

// MySQL 의 ENUM('a','b') 은 다른 DB 에서 문자열 타입으로 그려진다. 거기서 멈추면
// 아무 문자열이나 들어가는 컬럼이 되고 오류도 나지 않으므로, 허용값을 CHECK 로
// 옮겨야 한다.
func TestInlineEnumBecomesCheck(t *testing.T) {
	status := func() *Schema {
		return &Schema{Dialect: "mysql", Shape: ShapeRelational, Tables: []*Table{{
			Name: "app",
			Columns: []*Column{{
				Name: "status", Position: 1, RawType: "enum('DEVELOPING','RUNNING')",
				Type:       ParseType("mysql", "enum('DEVELOPING','RUNNING')"),
				HasDefault: true, Default: "'DEVELOPING'",
			}},
		}}}
	}
	want := map[string]string{
		"postgres": `"status" text NOT NULL DEFAULT 'DEVELOPING' CHECK ("status" IN ('DEVELOPING', 'RUNNING'))`,
		"mssql":    `[status] nvarchar(255) NOT NULL DEFAULT 'DEVELOPING' CHECK ([status] IN ('DEVELOPING', 'RUNNING'))`,
		"oracle":   `"status" VARCHAR2(255) DEFAULT 'DEVELOPING' NOT NULL CHECK ("status" IN ('DEVELOPING', 'RUNNING'))`,
	}
	for dialect, def := range want {
		up := BuildPlan(dialect, Diff(&Schema{Dialect: dialect, Shape: ShapeRelational}, status())).UpSQL()
		if !strings.Contains(up, def) {
			t.Errorf("%s: %q 가 없습니다:\n%s", dialect, def, up)
		}
	}

	// MySQL 은 타입 자체가 허용값을 지킨다. CHECK 를 덧붙이면 같은 규칙이 두 군데 산다.
	up := BuildPlan("mysql", Diff(&Schema{Dialect: "mysql", Shape: ShapeRelational}, status())).UpSQL()
	if strings.Contains(up, "CHECK") {
		t.Errorf("MySQL 에 CHECK 가 붙었습니다:\n%s", up)
	}
}

// SET 은 'a,b' 처럼 여러 값을 품는다. IN 으로 묶으면 멀쩡한 값이 거부된다.
func TestInlineSetIsNotChecked(t *testing.T) {
	sc := &Schema{Dialect: "postgres", Shape: ShapeRelational, Tables: []*Table{{
		Name: "app",
		Columns: []*Column{{
			Name: "flags", Position: 1, RawType: "set('a','b')",
			Type: ParseType("mysql", "set('a','b')"), Nullable: true,
		}},
	}}}
	up := BuildPlan("postgres", Diff(&Schema{Dialect: "postgres", Shape: ShapeRelational}, sc)).UpSQL()
	if strings.Contains(up, "CHECK") {
		t.Errorf("SET 에 CHECK 가 붙었습니다:\n%s", up)
	}
}
