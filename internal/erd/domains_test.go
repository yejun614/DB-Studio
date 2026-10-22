package erd

import (
	"strings"
	"testing"
)

// 도메인의 값어치는 "고치면 따라 바뀐다"에 있다. 그래서 이 시험들은 정의를 고친 뒤
// **컬럼이 어떻게 되었는가**를 본다 — 도메인 목록만 확인하면 이름 붙인 메모와
// 구분되지 않는다.

func domainDoc(t *testing.T) *Document {
	t.Helper()
	doc := twoTables(t)
	apply(t, doc, OpDomainAdd, `{"name":"email","type":"varchar(320)","comment":"메일 주소"}`)
	return doc
}

func TestDomainApplyToColumn(t *testing.T) {
	doc := domainDoc(t)
	apply(t, doc, OpColumnAdd, `{"table":"users","name":"login","domain":"email"}`)

	col := doc.findTable("users").Column("login")
	if col == nil {
		t.Fatal("컬럼이 만들어지지 않았습니다")
	}
	if col.RawType != "varchar(320)" {
		t.Errorf("타입 = %q, 도메인의 타입이어야 합니다", col.RawType)
	}
	if col.Domain != "email" {
		t.Errorf("도메인 연결 = %q", col.Domain)
	}
	// 도메인은 설계의 어휘이지 구조가 아니다. 지문에 들어가면 도메인으로 정리했을
	// 뿐인데 대상 DB와 구조가 다르다고 보고된다.
	before := doc.Schema.Fingerprint()
	col.Domain = ""
	if after := doc.Schema.Fingerprint(); after != before {
		t.Error("도메인 연결이 스키마 지문을 바꿉니다")
	}
}

func TestDomainUpdatePropagates(t *testing.T) {
	doc := domainDoc(t)
	apply(t, doc, OpColumnAdd, `{"table":"users","name":"login","domain":"email"}`)
	apply(t, doc, OpColumnAdd, `{"table":"orders","name":"buyer","domain":"email"}`)

	// 정의를 고치면 쓰는 컬럼이 함께 바뀐다.
	apply(t, doc, OpDomainUpdate, `{"name":"email","type":"varchar(254)"}`)
	for _, tc := range []struct{ table, col string }{{"users", "login"}, {"orders", "buyer"}} {
		got := doc.findTable(tc.table).Column(tc.col)
		if got.RawType != "varchar(254)" {
			t.Errorf("%s.%s 타입 = %q, 새 정의가 전파되지 않았습니다", tc.table, tc.col, got.RawType)
		}
	}

	// 이름을 바꾸면 연결도 따라간다. 따라가지 않으면 그 컬럼들은 도메인을 잃는다.
	apply(t, doc, OpDomainUpdate, `{"name":"email","newName":"email_addr"}`)
	if doc.findDomain("email") != nil {
		t.Error("옛 이름의 도메인이 남아 있습니다")
	}
	if got := doc.findTable("users").Column("login").Domain; got != "email_addr" {
		t.Errorf("연결 = %q, 새 이름이어야 합니다", got)
	}
}

func TestDomainNullableAndDefault(t *testing.T) {
	doc := twoTables(t)
	apply(t, doc, OpDomainAdd,
		`{"name":"created","type":"timestamp","nullable":false,"default":"CURRENT_TIMESTAMP"}`)
	apply(t, doc, OpColumnAdd, `{"table":"users","name":"created_at","domain":"created"}`)

	col := doc.findTable("users").Column("created_at")
	if col.Nullable {
		t.Error("도메인이 NOT NULL로 정했는데 컬럼이 NULL을 허용합니다")
	}
	if !col.HasDefault || col.Default != "CURRENT_TIMESTAMP" {
		t.Errorf("기본값 = %q(%t)", col.Default, col.HasDefault)
	}

	// NULL 여부를 정하지 않은 도메인은 컬럼의 값을 건드리지 않는다.
	// 같은 뜻의 컬럼이라도 표마다 필수/선택이 다를 수 있기 때문이다.
	apply(t, doc, OpDomainAdd, `{"name":"code","type":"varchar(20)"}`)
	apply(t, doc, OpColumnAdd, `{"table":"orders","name":"coupon","domain":"code","nullable":true}`)
	if !doc.findTable("orders").Column("coupon").Nullable {
		t.Error("도메인이 관여하지 않는 NULL 여부를 덮어썼습니다")
	}
}

func TestDomainDeleteKeepsType(t *testing.T) {
	doc := domainDoc(t)
	apply(t, doc, OpColumnAdd, `{"table":"users","name":"login","domain":"email"}`)
	apply(t, doc, OpDomainDelete, `{"name":"email"}`)

	col := doc.findTable("users").Column("login")
	if col.RawType != "varchar(320)" {
		t.Errorf("타입 = %q, 도메인을 지워도 타입은 남아야 합니다", col.RawType)
	}
	if col.Domain != "" {
		t.Errorf("연결 = %q, 끊겨야 합니다", col.Domain)
	}
}

func TestColumnTypeEditLeavesDomain(t *testing.T) {
	doc := domainDoc(t)
	apply(t, doc, OpColumnAdd, `{"table":"users","name":"login","domain":"email"}`)
	// 타입을 직접 고치면 도메인에서 벗어난다. 연결이 남아 있으면 다음에 도메인을
	// 고칠 때 이 컬럼의 값이 이유 없이 되돌아간다.
	apply(t, doc, OpColumnUpdate, `{"table":"users","name":"login","type":"text"}`)

	col := doc.findTable("users").Column("login")
	if col.Domain != "" {
		t.Errorf("연결 = %q, 직접 고친 컬럼은 도메인에서 벗어나야 합니다", col.Domain)
	}
	apply(t, doc, OpDomainUpdate, `{"name":"email","type":"varchar(100)"}`)
	if col.RawType != "text" {
		t.Errorf("타입 = %q, 벗어난 컬럼에 도메인이 다시 흘러들었습니다", col.RawType)
	}
}

func TestDomainValidation(t *testing.T) {
	doc := domainDoc(t)
	applyErr(t, doc, OpDomainAdd, `{"name":"email","type":"text"}`, "conflict", "이미 있습니다")
	applyErr(t, doc, OpDomainAdd, `{"name":"","type":"text"}`, "invalid", "비어 있습니다")
	applyErr(t, doc, OpDomainAdd, `{"name":"amount","type":""}`, "invalid", "타입이 비어 있습니다")
	applyErr(t, doc, OpDomainUpdate, `{"name":"nope","type":"text"}`, "not_found", "찾을 수 없습니다")
	applyErr(t, doc, OpColumnAdd, `{"table":"users","name":"x","domain":"nope"}`,
		"not_found", "찾을 수 없습니다")

	// 인용부호가 들어간 이름은 막는다(DDL 구조가 바뀌는 것을 막는 규칙과 같다).
	applyErr(t, doc, OpDomainAdd, `{"name":"ev\"il","type":"text"}`, "invalid", "쓸 수 없는 문자")
}

func TestDomainUndoRoundTrip(t *testing.T) {
	doc := domainDoc(t)
	apply(t, doc, OpColumnAdd, `{"table":"users","name":"login","domain":"email"}`)

	before := doc.Clone()
	apply(t, doc, OpDomainUpdate, `{"name":"email","type":"varchar(64)"}`)
	if got := doc.findTable("users").Column("login").RawType; got != "varchar(64)" {
		t.Fatalf("전파 실패: %q", got)
	}

	// 되돌리기는 도메인 정의와 그것이 바꾼 컬럼을 함께 되돌려야 한다.
	undo := Diff(doc, before)
	if undo == nil {
		t.Fatal("되돌릴 것이 없다고 판단했습니다")
	}
	if !strings.Contains(string(undo.Payload), "domains") {
		t.Errorf("복원 payload에 도메인이 없습니다: %s", undo.Payload)
	}
	if err := Apply(doc, undo); err != nil {
		t.Fatalf("되돌리기 실패: %v", err)
	}
	if got := doc.findDomain("email").Type; got != "varchar(320)" {
		t.Errorf("도메인 타입 = %q", got)
	}
	if got := doc.findTable("users").Column("login").RawType; got != "varchar(320)" {
		t.Errorf("컬럼 타입 = %q, 함께 되돌아가야 합니다", got)
	}
}

// 자동 갱신도 도메인이 정할 수 있다. "수정 시각" 도메인 하나로 표마다 같은 updated_at 을
// 만들려는 것이 이 기능의 쓰임이다.
func TestDomainOnUpdatePropagates(t *testing.T) {
	doc := twoTables(t)
	apply(t, doc, OpDomainAdd,
		`{"name":"modified","type":"timestamptz","default":"CURRENT_TIMESTAMP","onUpdate":"CURRENT_TIMESTAMP"}`)
	apply(t, doc, OpColumnAdd, `{"table":"users","name":"updated_at","domain":"modified"}`)
	apply(t, doc, OpColumnAdd, `{"table":"orders","name":"updated_at","type":"timestamptz"}`)
	apply(t, doc, OpColumnUpdate, `{"table":"orders","name":"updated_at","domain":"modified"}`)

	for _, tbl := range []string{"users", "orders"} {
		if got := doc.findTable(tbl).Column("updated_at").OnUpdate; got != "CURRENT_TIMESTAMP" {
			t.Errorf("%s.updated_at 자동 갱신 = %q, 도메인의 값이어야 합니다", tbl, got)
		}
	}

	// 고치면 따라 바뀐다.
	apply(t, doc, OpDomainUpdate, `{"name":"modified","onUpdate":"CURRENT_TIMESTAMP(3)"}`)
	if got := doc.findTable("users").Column("updated_at").OnUpdate; got != "CURRENT_TIMESTAMP(3)" {
		t.Errorf("자동 갱신 = %q, 새 정의가 전파되지 않았습니다", got)
	}

	// 관여하지 않는 도메인은 컬럼의 자동 갱신을 건드리지 않는다.
	apply(t, doc, OpDomainAdd, `{"name":"stamp","type":"timestamptz"}`)
	apply(t, doc, OpColumnAdd, `{"table":"orders","name":"seen_at","type":"timestamptz"}`)
	apply(t, doc, OpColumnUpdate, `{"table":"orders","name":"seen_at","onUpdate":"CURRENT_TIMESTAMP"}`)
	apply(t, doc, OpColumnUpdate, `{"table":"orders","name":"seen_at","domain":"stamp"}`)
	if got := doc.findTable("orders").Column("seen_at").OnUpdate; got != "CURRENT_TIMESTAMP" {
		t.Errorf("관여하지 않는 도메인이 자동 갱신을 지웠습니다: %q", got)
	}
}

// 도메인에서 자동 갱신을 끄면 도메인이 채워 준 컬럼에서도 꺼진다. 남겨 두면 도메인을
// 쓰는 컬럼의 타입 창에는 그 칸이 없어, 지우려면 도메인 연결부터 끊어야 했다.
func TestDomainOnUpdateClearFollows(t *testing.T) {
	doc := twoTables(t)
	apply(t, doc, OpDomainAdd, `{"name":"modified","type":"timestamptz","onUpdate":"CURRENT_TIMESTAMP"}`)
	apply(t, doc, OpColumnAdd, `{"table":"users","name":"updated_at","domain":"modified"}`)
	// 연결하기 전부터 다른 값을 가진 컬럼. 도메인이 준 것이 아니므로 남는다.
	apply(t, doc, OpColumnAdd, `{"table":"orders","name":"updated_at","type":"timestamptz"}`)
	apply(t, doc, OpColumnUpdate, `{"table":"orders","name":"updated_at","onUpdate":"CURRENT_TIMESTAMP(6)"}`)
	doc.findTable("orders").Column("updated_at").Domain = "modified"

	apply(t, doc, OpDomainUpdate, `{"name":"modified","newName":"changed","onUpdate":""}`)
	if got := doc.findDomain("changed").OnUpdate; got != "" {
		t.Errorf("도메인 자동 갱신 = %q", got)
	}
	if got := doc.findTable("users").Column("updated_at").OnUpdate; got != "" {
		t.Errorf("도메인이 준 자동 갱신이 남았습니다: %q", got)
	}
	if got := doc.findTable("orders").Column("updated_at").OnUpdate; got != "CURRENT_TIMESTAMP(6)" {
		t.Errorf("도메인과 다른 컬럼의 값을 지웠습니다: %q", got)
	}
	// 이름을 같이 바꿔도 연결은 따라간다(지우는 일은 옛 이름으로 찾는다).
	if got := doc.findTable("users").Column("updated_at").Domain; got != "changed" {
		t.Errorf("연결 = %q", got)
	}
}

// 시각 타입이 아닌 도메인에는 붙지 않는다. 어시스턴트·MCP 길은 아무 조합이나 보낼 수 있다.
func TestDomainOnUpdateValidation(t *testing.T) {
	doc := twoTables(t)
	applyErr(t, doc, OpDomainAdd, `{"name":"code","type":"varchar(20)","onUpdate":"CURRENT_TIMESTAMP"}`,
		"invalid", "날짜+시각")
	applyErr(t, doc, OpDomainAdd, `{"name":"stamp","type":"timestamptz","onUpdate":"my_func()"}`,
		"invalid", "CURRENT_TIMESTAMP")

	apply(t, doc, OpDomainAdd, `{"name":"stamp","type":"timestamptz","onUpdate":"CURRENT_TIMESTAMP"}`)
	// 타입만 바꾸면 거절한다. 자동 갱신을 함께 비우면 된다.
	applyErr(t, doc, OpDomainUpdate, `{"name":"stamp","type":"date"}`, "invalid", "함께 비우")
	apply(t, doc, OpDomainUpdate, `{"name":"stamp","type":"date","onUpdate":""}`)

	// MySQL 의 DATETIME 도 시각 타입이다.
	my := NewDocument("doc2", "테스트", "conn1", "mysql")
	apply(t, my, OpDomainAdd, `{"name":"modified","type":"datetime(3)","onUpdate":"CURRENT_TIMESTAMP(3)"}`)
}

// 되돌리기는 도메인의 자동 갱신과 그것이 바꾼 컬럼을 함께 되돌려야 한다. 새 칸이 깊은
// 복사에서 빠지면 사본까지 함께 바뀌어 "되돌릴 것이 없다"고 판단한다.
func TestDomainOnUpdateUndo(t *testing.T) {
	doc := twoTables(t)
	apply(t, doc, OpDomainAdd, `{"name":"modified","type":"timestamptz"}`)
	apply(t, doc, OpColumnAdd, `{"table":"users","name":"updated_at","domain":"modified"}`)
	before := doc.Clone()
	apply(t, doc, OpDomainUpdate, `{"name":"modified","onUpdate":"CURRENT_TIMESTAMP"}`)

	undo := Diff(doc, before)
	if undo == nil {
		t.Fatal("되돌릴 것이 없다고 판단했습니다")
	}
	if err := Apply(doc, undo); err != nil {
		t.Fatalf("되돌리기 실패: %v", err)
	}
	if got := doc.findDomain("modified").OnUpdate; got != "" {
		t.Errorf("도메인 자동 갱신 = %q", got)
	}
	if got := doc.findTable("users").Column("updated_at").OnUpdate; got != "" {
		t.Errorf("컬럼 자동 갱신 = %q, 함께 되돌아가야 합니다", got)
	}
}
