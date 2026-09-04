package schema

import (
	"strings"
	"testing"
)

func tableWith(name string, opts map[string]string) *Table {
	return &Table{
		Name: name, Options: opts,
		Columns: []*Column{{
			Name: "id", Position: 1,
			Type: LogicalType{Base: TypeBigInt}, RawType: "bigint",
		}},
		PrimaryKey: &PrimaryKey{Columns: []string{"id"}},
	}
}

func schemaWith(dialect string, tables ...*Table) *Schema {
	return &Schema{Dialect: dialect, Shape: ShapeRelational, Tables: tables}
}

// CREATE TABLE 에 표 설정이 실려야 한다. 화면에서 골라 두고 DDL 에는 안 나가면
// 그 어긋남은 마이그레이션을 실행한 뒤에야 드러난다.
func TestCreateTableCarriesOptions(t *testing.T) {
	sc := schemaWith("mysql", tableWith("users", map[string]string{
		"engine": "InnoDB", "charset": "utf8mb4", "collation": "utf8mb4_0900_ai_ci",
	}))
	plan := BuildPlan("mysql", Diff(schemaWith("mysql"), sc))
	sql := plan.UpSQL()
	for _, want := range []string{"ENGINE=InnoDB", "DEFAULT CHARSET=utf8mb4", "COLLATE=utf8mb4_0900_ai_ci"} {
		if !strings.Contains(sql, want) {
			t.Errorf("%s 가 DDL 에 없습니다:\n%s", want, sql)
		}
	}

	pg := schemaWith("postgres", tableWith("users", map[string]string{"tablespace": "fast"}))
	sql = BuildPlan("postgres", Diff(schemaWith("postgres"), pg)).UpSQL()
	if !strings.Contains(sql, "TABLESPACE fast") {
		t.Errorf("테이블스페이스가 DDL 에 없습니다:\n%s", sql)
	}
}

// 초안에서 비워 둔 칸은 "정하지 않았다"이지 "기본값으로 바꿔라"가 아니다.
// 이것을 구분하지 못하면 아무것도 고르지 않은 초안이 표마다 변경을 만들어 낸다.
func TestEmptyDraftOptionsAreNotDrift(t *testing.T) {
	// 실제 DB 에서 읽은 쪽에는 언제나 엔진과 문자셋이 있다.
	from := schemaWith("mysql", tableWith("users", map[string]string{
		"engine": "InnoDB", "charset": "utf8mb4", "rows": "1200",
	}))
	to := schemaWith("mysql", tableWith("users", nil))

	if diff := Diff(from, to); len(diff.Changes) != 0 {
		t.Errorf("비운 초안이 변경을 만들었습니다: %+v", diff.Changes)
	}
}

// 값을 정해 두었고 실제와 다르면 변경이 잡히고, 그 변경이 DDL 로 나온다.
func TestTableOptionDrift(t *testing.T) {
	from := schemaWith("mysql", tableWith("users", map[string]string{
		"engine": "MyISAM", "charset": "latin1",
	}))
	to := schemaWith("mysql", tableWith("users", map[string]string{
		"engine": "InnoDB", "charset": "utf8mb4",
	}))
	diff := Diff(from, to)
	if len(diff.Changes) != 1 || diff.Changes[0].Kind != AlterTableOptions {
		t.Fatalf("표 설정 변경이 잡히지 않았습니다: %+v", diff.Changes)
	}
	if !diff.Changes[0].Destructive {
		t.Error("엔진·문자셋 변경은 표를 통째로 다시 씁니다 — 파괴적으로 표시해야 합니다")
	}
	plan := BuildPlan("mysql", diff)
	up, down := plan.UpSQL(), plan.DownSQL()
	if !strings.Contains(up, "ENGINE=InnoDB") ||
		!strings.Contains(up, "CONVERT TO CHARACTER SET utf8mb4") {
		t.Errorf("up SQL 이 설정을 바꾸지 않습니다:\n%s", up)
	}
	if !strings.Contains(down, "ENGINE=MyISAM") ||
		!strings.Contains(down, "CONVERT TO CHARACTER SET latin1") {
		t.Errorf("down SQL 이 예전 설정으로 되돌리지 않습니다:\n%s", down)
	}
}

// 예전 값이 없으면 되돌릴 문장을 만들 수 없다. 조용히 지금 값을 남겨 두면
// 되돌린 뒤에도 바뀐 설정이 그대로인 채로 "되돌렸다"고 보고하게 된다.
func TestTableOptionWithoutOldValueIsIrreversible(t *testing.T) {
	from := schemaWith("mysql", tableWith("users", nil))
	to := schemaWith("mysql", tableWith("users", map[string]string{"engine": "InnoDB"}))
	plan := BuildPlan("mysql", Diff(from, to))
	if len(plan.Irreversible) == 0 {
		t.Errorf("되돌릴 수 없다는 표시가 없습니다: %+v", plan)
	}
	if strings.Contains(plan.DownSQL(), "ENGINE") {
		t.Errorf("되돌릴 수 없는데 down SQL 이 생겼습니다:\n%s", plan.DownSQL())
	}
}

// 우리가 모르는 열쇠(통계 등)는 견주지 않는다.
func TestUnknownOptionKeysAreIgnored(t *testing.T) {
	from := schemaWith("mysql", tableWith("users", map[string]string{"avg_row_length": "40"}))
	to := schemaWith("mysql", tableWith("users", map[string]string{"avg_row_length": "80"}))
	if diff := Diff(from, to); len(diff.Changes) != 0 {
		t.Errorf("모르는 열쇠로 변경이 잡혔습니다: %+v", diff.Changes)
	}
}

// 방언마다 정할 수 있는 것이 다르다. SQLite 에는 표 설정이 없다.
func TestOptionSpecsPerDialect(t *testing.T) {
	if len(TableOptionSpecs("sqlite")) != 0 {
		t.Error("SQLite 에 표 설정 칸이 생겼습니다")
	}
	if len(TableOptionSpecs("mysql")) == 0 || len(DatabaseOptionSpecs("postgres")) == 0 {
		t.Error("있어야 할 설정 목록이 비었습니다")
	}
	if OptionLabel("mysql", "engine") != "엔진" {
		t.Errorf("사람 말 이름이 없습니다: %q", OptionLabel("mysql", "engine"))
	}
	if OptionLabel("mysql", "no_such_key") != "no_such_key" {
		t.Error("모르는 열쇠는 그대로 돌려줘야 합니다")
	}
}

// 문자셋과 정렬 규칙은 한 절로 나가야 한다. 따로 내면 CONVERT 가 컬럼을 그 문자셋의
// 기본 정렬로 바꾼 뒤 표의 기본 정렬만 따로 바뀌어, 컬럼과 표가 다른 규칙을 갖는다.
func TestCharsetAndCollationConvertTogether(t *testing.T) {
	from := schemaWith("mysql", tableWith("users", map[string]string{
		"charset": "latin1", "collation": "latin1_swedish_ci",
	}))
	to := schemaWith("mysql", tableWith("users", map[string]string{
		"charset": "utf8mb4", "collation": "utf8mb4_0900_ai_ci",
	}))
	plan := BuildPlan("mysql", Diff(from, to))
	up := plan.UpSQL()
	if !strings.Contains(up, "CONVERT TO CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci") {
		t.Errorf("문자셋과 정렬 규칙이 한 절로 나가지 않습니다:\n%s", up)
	}
	if strings.Contains(up, "COLLATE=") {
		t.Errorf("표 기본 정렬만 따로 바꾸는 절이 함께 나갔습니다:\n%s", up)
	}
	down := plan.DownSQL()
	if !strings.Contains(down, "CONVERT TO CHARACTER SET latin1 COLLATE latin1_swedish_ci") {
		t.Errorf("되돌리기가 예전 문자셋으로 돌아가지 않습니다:\n%s", down)
	}
}

// 정렬 규칙만 바꿔도 문자셋을 함께 적어야 한다 — CONVERT 에 문자셋이 없으면
// MySQL 이 무엇으로 바꿀지 알 수 없다.
func TestCollationOnlyKeepsCharset(t *testing.T) {
	from := schemaWith("mysql", tableWith("users", map[string]string{
		"charset": "utf8mb4", "collation": "utf8mb4_general_ci",
	}))
	to := schemaWith("mysql", tableWith("users", map[string]string{
		"charset": "utf8mb4", "collation": "utf8mb4_0900_ai_ci",
	}))
	up := BuildPlan("mysql", Diff(from, to)).UpSQL()
	if !strings.Contains(up, "CONVERT TO CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci") {
		t.Errorf("문자셋이 빠졌습니다:\n%s", up)
	}
}
