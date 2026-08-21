package sqlimport

import (
	"strings"
	"testing"

	"dbstudio/internal/schema"
)

func mustParse(t *testing.T, dialect, script string) *Result {
	t.Helper()
	res, err := Parse(dialect, script)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return res
}

func TestCreateTableMySQL(t *testing.T) {
	res := mustParse(t, "mysql", "CREATE TABLE `orders` (\n"+
		"  `id` BIGINT NOT NULL AUTO_INCREMENT,\n"+
		"  `user_id` BIGINT NOT NULL,\n"+
		"  `amount` DECIMAL(10,2) NOT NULL DEFAULT 0.00 COMMENT '금액',\n"+
		"  `status` VARCHAR(20) DEFAULT 'new',\n"+
		"  `memo` TEXT,\n"+
		"  PRIMARY KEY (`id`),\n"+
		"  UNIQUE KEY `uq_orders_user` (`user_id`,`status`),\n"+
		"  KEY `ix_orders_status` (`status`),\n"+
		"  CONSTRAINT `fk_orders_user` FOREIGN KEY (`user_id`) REFERENCES `users` (`id`) ON DELETE CASCADE,\n"+
		"  CONSTRAINT `ck_amount` CHECK (`amount` >= 0)\n"+
		") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='주문';")

	if len(res.Tables) != 1 {
		t.Fatalf("테이블 수: %d", len(res.Tables))
	}
	tbl := res.Tables[0]
	if tbl.Name != "orders" || tbl.Comment != "주문" {
		t.Errorf("이름/주석: %q %q", tbl.Name, tbl.Comment)
	}
	if len(tbl.Columns) != 5 {
		t.Fatalf("컬럼 수: %d (%+v)", len(tbl.Columns), columnNames(tbl))
	}
	id := tbl.Column("id")
	if !id.Identity || id.Nullable || !strings.EqualFold(id.RawType, "BIGINT") {
		t.Errorf("id: %+v", id)
	}
	amount := tbl.Column("amount")
	if amount.RawType != "DECIMAL(10, 2)" && amount.RawType != "DECIMAL(10,2)" {
		t.Errorf("amount 타입: %q", amount.RawType)
	}
	if amount.Default != "0.00" || !amount.HasDefault {
		t.Errorf("amount 기본값: %q", amount.Default)
	}
	if amount.Comment != "금액" {
		t.Errorf("amount 주석: %q", amount.Comment)
	}
	if got := tbl.Column("status").Default; got != "'new'" {
		t.Errorf("status 기본값: %q", got)
	}
	if !tbl.Column("memo").Nullable {
		t.Error("memo 는 NULL 가능이어야 합니다")
	}
	if tbl.PrimaryKey == nil || len(tbl.PrimaryKey.Columns) != 1 || tbl.PrimaryKey.Columns[0] != "id" {
		t.Errorf("기본키: %+v", tbl.PrimaryKey)
	}
	if len(tbl.Indexes) != 2 {
		t.Fatalf("인덱스 수: %d %+v", len(tbl.Indexes), tbl.Indexes)
	}
	if !tbl.Indexes[0].Unique || len(tbl.Indexes[0].Columns) != 2 {
		t.Errorf("유니크 인덱스: %+v", tbl.Indexes[0])
	}
	if len(tbl.ForeignKeys) != 1 {
		t.Fatalf("외래키 수: %d", len(tbl.ForeignKeys))
	}
	fk := tbl.ForeignKeys[0]
	if fk.Name != "fk_orders_user" || fk.RefTable != "users" || fk.OnDelete != "CASCADE" {
		t.Errorf("외래키: %+v", fk)
	}
	if len(tbl.Checks) != 1 || !strings.Contains(tbl.Checks[0].Expression, ">=") {
		t.Errorf("체크: %+v", tbl.Checks)
	}
}

func TestCreateTablePostgres(t *testing.T) {
	res := mustParse(t, "postgres", `
CREATE TABLE IF NOT EXISTS app.users (
    id          bigserial PRIMARY KEY,
    email       text NOT NULL UNIQUE,
    name        character varying(120),
    created_at  timestamp with time zone NOT NULL DEFAULT now(),
    settings    jsonb DEFAULT '{}'::jsonb,
    manager_id  bigint REFERENCES app.users (id) ON DELETE SET NULL
);
CREATE UNIQUE INDEX ix_users_email_lower ON app.users (email) WHERE email IS NOT NULL;
COMMENT ON TABLE app.users IS '사용자';
COMMENT ON COLUMN app.users.email IS '로그인 아이디';
`)
	if len(res.Tables) != 1 {
		t.Fatalf("테이블 수: %d", len(res.Tables))
	}
	tbl := res.Tables[0]
	if tbl.Namespace != "app" || tbl.Name != "users" {
		t.Errorf("이름: %q.%q", tbl.Namespace, tbl.Name)
	}
	if tbl.Comment != "사용자" {
		t.Errorf("테이블 주석: %q", tbl.Comment)
	}
	if got := tbl.Column("email").Comment; got != "로그인 아이디" {
		t.Errorf("컬럼 주석: %q", got)
	}
	if got := tbl.Column("name").RawType; got != "character varying(120)" {
		t.Errorf("varchar 타입: %q", got)
	}
	if got := tbl.Column("created_at").RawType; got != "timestamp with time zone" {
		t.Errorf("timestamptz 타입: %q", got)
	}
	if got := tbl.Column("created_at").Default; got != "now()" {
		t.Errorf("now() 기본값: %q", got)
	}
	if tbl.PrimaryKey == nil || tbl.PrimaryKey.Columns[0] != "id" {
		t.Errorf("기본키: %+v", tbl.PrimaryKey)
	}
	if len(tbl.ForeignKeys) != 1 || tbl.ForeignKeys[0].OnDelete != "SET NULL" {
		t.Errorf("자기 참조 외래키: %+v", tbl.ForeignKeys)
	}
	// email UNIQUE(인라인) + 부분 유니크 인덱스 = 2개
	if len(tbl.Indexes) != 2 {
		t.Fatalf("인덱스 수: %d %+v", len(tbl.Indexes), tbl.Indexes)
	}
	var partial = tbl.Indexes[len(tbl.Indexes)-1]
	if partial.Where == "" {
		t.Errorf("부분 인덱스 조건이 사라졌습니다: %+v", partial)
	}
}

func TestAlterAndDrop(t *testing.T) {
	res := mustParse(t, "postgres", `
CREATE TABLE t (a int, b text);
ALTER TABLE t ADD COLUMN c boolean NOT NULL DEFAULT false;
ALTER TABLE t DROP COLUMN b;
ALTER TABLE t ADD CONSTRAINT pk_t PRIMARY KEY (a);
ALTER TABLE t ALTER COLUMN a TYPE bigint;
DROP TABLE old_one, other.two;
`)
	if len(res.Tables) != 1 {
		t.Fatalf("테이블 수: %d", len(res.Tables))
	}
	tbl := res.Tables[0]
	if tbl.Column("b") != nil {
		t.Error("b 컬럼이 남아 있습니다")
	}
	c := tbl.Column("c")
	if c == nil || c.Nullable || c.Default != "false" {
		t.Errorf("c 컬럼: %+v", c)
	}
	if tbl.PrimaryKey == nil || tbl.PrimaryKey.Name != "pk_t" {
		t.Errorf("기본키: %+v", tbl.PrimaryKey)
	}
	if got := tbl.Column("a").RawType; got != "bigint" {
		t.Errorf("타입 변경이 반영되지 않았습니다: %q", got)
	}
	if len(res.Drops) != 2 || res.Drops[0] != "old_one" || res.Drops[1] != "other.two" {
		t.Errorf("DROP 목록: %+v", res.Drops)
	}
}

// 스크립트에 정의가 없는 테이블을 ALTER 하면 조용히 빈 테이블을 만들어서는 안 된다.
// 그렇게 하면 초안의 기존 테이블이 컬럼 하나짜리로 덮어써진다.
func TestAlterWithoutDefinitionIsReported(t *testing.T) {
	res := mustParse(t, "mysql", `
CREATE TABLE a (id int);
ALTER TABLE elsewhere ADD COLUMN x int;
`)
	if len(res.Tables) != 1 || res.Tables[0].Name != "a" {
		t.Fatalf("테이블: %+v", res.Tables)
	}
	if len(res.Notes) == 0 || !strings.Contains(strings.Join(res.Notes, " "), "elsewhere") {
		t.Errorf("알림이 없습니다: %+v", res.Notes)
	}
}

// 해석하지 못한 문장은 반드시 알려야 한다. 조용히 넘기면 사용자는 불러오기가 끝난
// 뒤에야 빠진 것을 발견한다.
func TestUnknownStatementsAreNoted(t *testing.T) {
	res := mustParse(t, "postgres", `
CREATE TABLE a (id int);
CREATE VIEW v AS SELECT * FROM a;
CREATE FUNCTION f() RETURNS int AS $$ BEGIN RETURN 1; END $$ LANGUAGE plpgsql;
INSERT INTO a VALUES (1);
`)
	if len(res.Tables) != 1 {
		t.Fatalf("테이블 수: %d", len(res.Tables))
	}
	joined := strings.Join(res.Notes, " | ")
	for _, want := range []string{"VIEW", "FUNCTION", "INSERT"} {
		if !strings.Contains(joined, want) {
			t.Errorf("%s 문장이 알림에 없습니다: %s", want, joined)
		}
	}
}

// 달러 인용 안의 세미콜론이 문장을 끊으면 뒤의 테이블을 통째로 잃는다.
func TestDollarQuotedBodyDoesNotSplitStatements(t *testing.T) {
	res := mustParse(t, "postgres", `
CREATE FUNCTION f() RETURNS trigger AS $$ BEGIN; RETURN NEW; END; $$ LANGUAGE plpgsql;
CREATE TABLE after_fn (id int PRIMARY KEY);
`)
	if len(res.Tables) != 1 || res.Tables[0].Name != "after_fn" {
		t.Fatalf("함수 뒤의 테이블을 놓쳤습니다: %+v", res.Tables)
	}
}

func TestMSSQLBrackets(t *testing.T) {
	res := mustParse(t, "mssql", `
CREATE TABLE [dbo].[Product] (
  [Id] INT IDENTITY(1,1) NOT NULL,
  [Name] NVARCHAR(200) NOT NULL,
  [Price] DECIMAL(18, 4) NULL,
  CONSTRAINT [PK_Product] PRIMARY KEY CLUSTERED ([Id] ASC)
);
`)
	tbl := res.Tables[0]
	if tbl.Namespace != "dbo" || tbl.Name != "Product" {
		t.Errorf("이름: %q.%q", tbl.Namespace, tbl.Name)
	}
	if !tbl.Column("Id").Identity {
		t.Error("IDENTITY 를 읽지 못했습니다")
	}
	if tbl.PrimaryKey == nil || tbl.PrimaryKey.Columns[0] != "Id" {
		t.Errorf("기본키: %+v", tbl.PrimaryKey)
	}
}

func TestNoTablesIsAnError(t *testing.T) {
	if _, err := Parse("mysql", "SELECT 1;"); err == nil {
		t.Fatal("테이블이 없는 스크립트는 오류여야 합니다")
	}
}

func columnNames(t *schema.Table) []string {
	out := []string{}
	for _, c := range t.Columns {
		out = append(out, c.Name)
	}
	return out
}
