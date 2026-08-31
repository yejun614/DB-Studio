package erd

import (
	"encoding/json"
	"strings"
	"testing"

	"dbstudio/internal/schema"
)

// op는 테스트에서 op를 짧게 만드는 헬퍼다.
func op(kind Kind, payload string) *Op {
	return &Op{ID: "op-" + string(kind), Kind: kind, Payload: json.RawMessage(payload)}
}

// apply는 사본이 아닌 문서에 직접 적용한다(테스트 편의).
func apply(t *testing.T, doc *Document, kind Kind, payload string) {
	t.Helper()
	if err := Apply(doc, op(kind, payload)); err != nil {
		t.Fatalf("%s 적용 실패: %v\npayload=%s", kind, err, payload)
	}
}

// applyErr은 실패를 기대한다. 코드와 메시지를 함께 확인한다 —
// "오류가 났다"만 보면 잘못된 이유로 실패해도 통과한다.
func applyErr(t *testing.T, doc *Document, kind Kind, payload, wantCode, wantContains string) {
	t.Helper()
	err := Apply(doc, op(kind, payload))
	if err == nil {
		t.Fatalf("%s 는 실패해야 합니다\npayload=%s", kind, payload)
	}
	e, ok := err.(*Error)
	if !ok {
		t.Fatalf("%s: *erd.Error 를 기대했으나 %T", kind, err)
	}
	if e.Code != wantCode {
		t.Errorf("%s: code = %q, 기대값 %q (사유: %s)", kind, e.Code, wantCode, e.Reason)
	}
	if wantContains != "" && !strings.Contains(e.Reason, wantContains) {
		t.Errorf("%s: 사유에 %q 가 없습니다: %s", kind, wantContains, e.Reason)
	}
}

func newDoc(t *testing.T) *Document {
	t.Helper()
	return NewDocument("doc1", "테스트", "conn1", "postgres")
}

// twoTables는 users(id PK) / orders(id PK) 두 테이블이 있는 문서를 만든다.
func twoTables(t *testing.T) *Document {
	t.Helper()
	doc := newDoc(t)
	apply(t, doc, OpTableAdd, `{"name":"users","withId":true}`)
	apply(t, doc, OpTableAdd, `{"name":"orders","withId":true}`)
	return doc
}

func TestTableAdd(t *testing.T) {
	doc := newDoc(t)
	apply(t, doc, OpTableAdd, `{"name":"users","comment":"회원"}`)

	if len(doc.Schema.Tables) != 1 {
		t.Fatalf("테이블 수 = %d", len(doc.Schema.Tables))
	}
	tbl := doc.Schema.Tables[0]
	if tbl.Name != "users" || tbl.Comment != "회원" {
		t.Errorf("테이블 = %+v", tbl)
	}
	if box := doc.Layout["users"]; box == nil {
		t.Error("레이아웃이 만들어지지 않았습니다")
	}

	// 같은 이름은 대소문자를 달리해도 충돌이다 — 대상 DB 대부분이 그렇게 취급한다.
	applyErr(t, doc, OpTableAdd, `{"name":"USERS"}`, "conflict", "이미 있습니다")
}

func TestTableAddWithID(t *testing.T) {
	doc := newDoc(t)
	apply(t, doc, OpTableAdd, `{"name":"users","withId":true}`)
	tbl := doc.Schema.Tables[0]
	if len(tbl.Columns) != 1 || tbl.Columns[0].Name != "id" {
		t.Fatalf("컬럼 = %+v", tbl.Columns)
	}
	if !tbl.Columns[0].Identity {
		t.Error("id 컬럼이 identity가 아닙니다")
	}
	if tbl.PrimaryKey == nil || tbl.PrimaryKey.Columns[0] != "id" {
		t.Errorf("기본키 = %+v", tbl.PrimaryKey)
	}
}

// 새 테이블은 이미 쓰인 격자점을 피해야 한다. 여러 사람이 동시에 추가할 때
// 같은 자리에 겹쳐 놓이면 하나가 보이지 않는다.
func TestTableAddAvoidsOverlap(t *testing.T) {
	doc := newDoc(t)
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		apply(t, doc, OpTableAdd, `{"name":"`+name+`"}`)
	}
	seen := map[[2]float64]string{}
	for key, box := range doc.Layout {
		pos := [2]float64{box.X, box.Y}
		if other, dup := seen[pos]; dup {
			t.Errorf("%s 와 %s 가 같은 위치 %v 에 놓였습니다", key, other, pos)
		}
		seen[pos] = key
	}
}

func TestIdentifierValidation(t *testing.T) {
	doc := newDoc(t)
	cases := []struct {
		name    string
		payload string
		want    string
	}{
		{"빈 이름", `{"name":""}`, "비어 있습니다"},
		{"공백만", `{"name":"   "}`, "비어 있습니다"},
		{"인용부호", `{"name":"us\"ers"}`, "쓸 수 없는 문자"},
		{"백틱", "{\"name\":\"us`ers\"}", "쓸 수 없는 문자"},
		{"세미콜론", `{"name":"users; DROP TABLE x"}`, "쓸 수 없는 문자"},
		{"줄바꿈", `{"name":"users\nmore"}`, "쓸 수 없는 문자"},
		{"대괄호", `{"name":"[users]"}`, "쓸 수 없는 문자"},
		{"너무 긴 이름", `{"name":"` + strings.Repeat("a", 129) + `"}`, "너무 깁니다"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			applyErr(t, doc, OpTableAdd, tc.payload, "invalid", tc.want)
		})
	}

	// 한글·유니코드 식별자는 허용해야 한다. 실제로 쓰는 팀이 있다.
	apply(t, doc, OpTableAdd, `{"name":"회원_테이블"}`)
	if doc.findTable("회원_테이블") == nil {
		t.Error("유니코드 식별자가 거부되었습니다")
	}
}

// 오타 난 필드 이름을 조용히 무시하면 사용자의 편집이 아무 오류 없이 사라진다.
func TestUnknownFieldRejected(t *testing.T) {
	doc := twoTables(t)
	applyErr(t, doc, OpColumnUpdate,
		`{"table":"users","name":"id","nullible":true}`, "invalid", "올바르지 않습니다")

	// 정상 필드로는 통과한다.
	apply(t, doc, OpColumnUpdate, `{"table":"users","name":"id","nullable":true}`)
}

// 패치 의미: 없는 필드는 변경하지 않는다. 이것이 필드 단위 LWW의 전제다.
func TestColumnUpdateIsPatch(t *testing.T) {
	doc := twoTables(t)
	apply(t, doc, OpColumnAdd,
		`{"table":"users","name":"email","type":"varchar(255)","nullable":false,"comment":"이메일"}`)

	// A는 타입만, B는 주석만 바꾼다. 전체 교체였다면 나중 op가 앞의 편집을 되돌린다.
	apply(t, doc, OpColumnUpdate, `{"table":"users","name":"email","type":"varchar(320)"}`)
	apply(t, doc, OpColumnUpdate, `{"table":"users","name":"email","comment":"로그인 이메일"}`)

	col := doc.findTable("users").Column("email")
	if col.RawType != "varchar(320)" {
		t.Errorf("타입 = %q, 기대값 varchar(320)", col.RawType)
	}
	if col.Comment != "로그인 이메일" {
		t.Errorf("주석 = %q", col.Comment)
	}
	if col.Nullable {
		t.Error("nullable이 패치와 무관하게 바뀌었습니다")
	}
}

// 기본값 제거를 표현할 수 있어야 한다. 빈 문자열 = 제거, 필드 생략 = 변경 없음.
func TestColumnDefaultRemoval(t *testing.T) {
	doc := twoTables(t)
	apply(t, doc, OpColumnAdd, `{"table":"users","name":"state","type":"varchar(20)","default":"'new'"}`)
	col := doc.findTable("users").Column("state")
	if !col.HasDefault || col.Default != "'new'" {
		t.Fatalf("기본값 = %q (has=%t)", col.Default, col.HasDefault)
	}

	apply(t, doc, OpColumnUpdate, `{"table":"users","name":"state","comment":"상태"}`)
	if !col.HasDefault {
		t.Error("필드를 생략했는데 기본값이 사라졌습니다")
	}

	apply(t, doc, OpColumnUpdate, `{"table":"users","name":"state","default":""}`)
	if col.HasDefault || col.Default != "" {
		t.Errorf("기본값이 제거되지 않았습니다: %q (has=%t)", col.Default, col.HasDefault)
	}
}

func TestColumnTypeNormalization(t *testing.T) {
	doc := twoTables(t)
	apply(t, doc, OpColumnAdd, `{"table":"users","name":"amount","type":"numeric(10,2)"}`)
	col := doc.findTable("users").Column("amount")
	if col.Type.Base != schema.TypeDecimal || col.Type.Precision != 10 || col.Type.Scale != 2 {
		t.Errorf("논리 타입 = %+v", col.Type)
	}
	if col.RawType != "numeric(10,2)" {
		t.Errorf("원본 타입이 보존되지 않았습니다: %q", col.RawType)
	}

	// 벤더 전용 타입은 거부하지 않고 원본을 보존한다.
	apply(t, doc, OpColumnAdd, `{"table":"users","name":"loc","type":"hstore"}`)
	loc := doc.findTable("users").Column("loc")
	if loc.RawType != "hstore" {
		t.Errorf("벤더 타입이 보존되지 않았습니다: %q", loc.RawType)
	}

	applyErr(t, doc, OpColumnAdd, `{"table":"users","name":"bad","type":""}`, "invalid", "비어 있습니다")
	applyErr(t, doc, OpColumnAdd,
		`{"table":"users","name":"bad","type":"int; DROP TABLE x"}`, "invalid", "쓸 수 없는 문자")
}

func TestColumnPositionAndReorder(t *testing.T) {
	doc := twoTables(t)
	apply(t, doc, OpColumnAdd, `{"table":"users","name":"b","type":"int"}`)
	apply(t, doc, OpColumnAdd, `{"table":"users","name":"c","type":"int"}`)
	// 맨 앞(1번)에 삽입
	apply(t, doc, OpColumnAdd, `{"table":"users","name":"a","type":"int","position":1}`)

	names := columnNames(doc.findTable("users"))
	if got := strings.Join(names, ","); got != "a,id,b,c" {
		t.Fatalf("컬럼 순서 = %s", got)
	}
	for i, c := range doc.findTable("users").Columns {
		if c.Position != i+1 {
			t.Errorf("%s 의 position = %d, 기대값 %d", c.Name, c.Position, i+1)
		}
	}

	// 이동
	apply(t, doc, OpColumnUpdate, `{"table":"users","name":"a","position":3}`)
	if got := strings.Join(columnNames(doc.findTable("users")), ","); got != "id,b,a,c" {
		t.Fatalf("이동 후 순서 = %s", got)
	}
}

func columnNames(tbl *schema.Table) []string {
	out := make([]string, 0, len(tbl.Columns))
	for _, c := range tbl.Columns {
		out = append(out, c.Name)
	}
	return out
}

// 테이블 이름을 바꾸면 이를 참조하는 외래키와 레이아웃 키가 함께 따라가야 한다.
func TestTableRenamePropagates(t *testing.T) {
	doc := twoTables(t)
	apply(t, doc, OpColumnAdd, `{"table":"orders","name":"user_id","type":"bigint"}`)
	apply(t, doc, OpFKAdd,
		`{"table":"orders","name":"fk_orders_user","columns":["user_id"],"refTable":"users","refColumns":["id"]}`)

	// 이름을 바꾸기 전 좌표를 기억해 둔다(아래에서 그대로인지 본다).
	before := doc.Layout["users"]
	if before == nil {
		t.Fatal("이름을 바꾸기 전 레이아웃이 없습니다")
	}
	wantX, wantY := before.X, before.Y

	apply(t, doc, OpTableUpdate, `{"key":"users","name":"members"}`)

	if doc.findTable("users") != nil {
		t.Error("옛 키로 테이블이 아직 조회됩니다")
	}
	if doc.findTable("members") == nil {
		t.Fatal("새 키로 테이블을 찾을 수 없습니다")
	}
	// 좌표까지 그대로 따라와야 한다. 키만 옮기고 값이 새로 만들어지면 카드가
	// 기본 자리로 튀고, 사용자에게는 "이름을 바꾸니 다이어그램이 헝클어졌다"가 된다.
	moved, ok := doc.Layout["members"]
	if !ok {
		t.Fatal("레이아웃 키가 따라오지 않았습니다")
	}
	if moved.X != wantX || moved.Y != wantY {
		t.Errorf("좌표가 바뀌었습니다: (%v,%v) → (%v,%v)", wantX, wantY, moved.X, moved.Y)
	}
	if _, ok := doc.Layout["users"]; ok {
		t.Error("옛 레이아웃 키가 남아 있습니다")
	}
	fk := doc.findTable("orders").ForeignKeys[0]
	if fk.RefTable != "members" {
		t.Errorf("외래키 참조가 갱신되지 않았습니다: %s", fk.RefTable)
	}
}

func TestTableRenameConflict(t *testing.T) {
	doc := twoTables(t)
	applyErr(t, doc, OpTableUpdate, `{"key":"users","name":"orders"}`, "conflict", "이미 쓰이고")
}

// 컬럼 이름을 바꾸면 기본키·인덱스·외래키·참조하는 외래키가 모두 따라가야 한다.
func TestColumnRenamePropagates(t *testing.T) {
	doc := twoTables(t)
	apply(t, doc, OpColumnAdd, `{"table":"orders","name":"user_id","type":"bigint"}`)
	apply(t, doc, OpIndexAdd, `{"table":"orders","name":"ix_user","columns":["user_id"]}`)
	apply(t, doc, OpFKAdd,
		`{"table":"orders","name":"fk_o_u","columns":["user_id"],"refTable":"users","refColumns":["id"]}`)

	// 참조 대상(users.id)의 이름을 바꾼다.
	apply(t, doc, OpColumnUpdate, `{"table":"users","name":"id","newName":"user_id"}`)
	if got := doc.findTable("users").PrimaryKey.Columns[0]; got != "user_id" {
		t.Errorf("기본키 컬럼 = %s", got)
	}
	if got := doc.findTable("orders").ForeignKeys[0].RefColumns[0]; got != "user_id" {
		t.Errorf("참조 컬럼 = %s", got)
	}

	// 참조하는 쪽(orders.user_id)의 이름을 바꾼다.
	apply(t, doc, OpColumnUpdate, `{"table":"orders","name":"user_id","newName":"owner_id"}`)
	orders := doc.findTable("orders")
	if got := orders.ForeignKeys[0].Columns[0]; got != "owner_id" {
		t.Errorf("외래키 컬럼 = %s", got)
	}
	if got := orders.Indexes[0].Columns[0].Column; got != "owner_id" {
		t.Errorf("인덱스 컬럼 = %s", got)
	}
}

// 외래키도 만든 뒤에 이름과 컬럼 짝을 고칠 수 있어야 한다.
//
// 제약 이름은 대상 DB에도 그대로 나가는 이름이다. 고치는 길이 "지우고 새로 만들기"뿐이면
// 관계선이 사라졌다 나타나고, 그 두 op는 되돌리기에서도 따로 논다.
func TestFKUpdateNameAndColumns(t *testing.T) {
	doc := twoTables(t)
	apply(t, doc, OpColumnAdd, `{"table":"users","name":"email","type":"varchar(255)"}`)
	apply(t, doc, OpColumnAdd, `{"table":"users","name":"nickname","type":"varchar(40)"}`)
	apply(t, doc, OpIndexAdd, `{"table":"users","name":"ux_email","columns":["email"],"unique":true}`)
	apply(t, doc, OpColumnAdd, `{"table":"orders","name":"user_id","type":"int"}`)
	apply(t, doc, OpColumnAdd, `{"table":"orders","name":"user_email","type":"varchar(255)"}`)
	apply(t, doc, OpFKAdd,
		`{"table":"orders","name":"fk_a","columns":["user_id"],"refTable":"users","refColumns":["id"]}`)

	// 이름과 컬럼 짝을 함께 바꾼다(id 참조 → email 참조).
	apply(t, doc, OpFKUpdate, `{"table":"orders","name":"fk_a","newName":"fk_orders_user_email",`+
		`"columns":["user_email"],"refTable":"users","refColumns":["email"],"onDelete":"SET NULL"}`)

	orders := doc.findTable("orders")
	if len(orders.ForeignKeys) != 1 {
		t.Fatalf("외래키 수 = %d (이름을 바꾸면서 하나 더 만들었습니다)", len(orders.ForeignKeys))
	}
	fk := orders.ForeignKeys[0]
	if fk.Name != "fk_orders_user_email" {
		t.Errorf("이름 = %q", fk.Name)
	}
	if len(fk.Columns) != 1 || fk.Columns[0] != "user_email" {
		t.Errorf("컬럼 = %v", fk.Columns)
	}
	if len(fk.RefColumns) != 1 || fk.RefColumns[0] != "email" {
		t.Errorf("참조 컬럼 = %v", fk.RefColumns)
	}
	if fk.OnDelete != "SET NULL" {
		t.Errorf("ON DELETE = %q", fk.OnDelete)
	}

	// 이미 있는 이름으로는 바꿀 수 없다.
	apply(t, doc, OpFKAdd,
		`{"table":"orders","name":"fk_b","columns":["user_id"],"refTable":"users","refColumns":["id"]}`)
	applyErr(t, doc, OpFKUpdate,
		`{"table":"orders","name":"fk_b","newName":"fk_orders_user_email"}`, "conflict", "이미 있습니다")
	if doc.findTable("orders").ForeignKeys[1].Name != "fk_b" {
		t.Error("거부된 이름 변경이 적용되었습니다")
	}

	// 고유하지 않은 컬럼은 여전히 참조할 수 없다(이름만 바꾸는 길로도 뚫리지 않아야 한다).
	applyErr(t, doc, OpFKUpdate,
		`{"table":"orders","name":"fk_b","columns":["user_email"],"refTable":"users","refColumns":["nickname"]}`,
		"invalid", "참조할 수 없습니다")
}

// 인덱스는 만든 뒤에도 고칠 수 있어야 한다: 이름, 컬럼 순서, 정렬 방향.
//
// 이름을 지웠다 다시 만드는 방식으로 바꾸면 그 사이에 인덱스가 없는 순간이 생기고,
// 두 op는 되돌리기에서도 따로 논다. 컬럼 순서와 정렬 방향은 어떤 조회가 이 인덱스를
// 탈 수 있는지를 정하므로, 고칠 수 없으면 만들 때 한 번에 맞히는 수밖에 없다.
func TestIndexUpdateDetails(t *testing.T) {
	doc := twoTables(t)
	apply(t, doc, OpColumnAdd, `{"table":"users","name":"email","type":"varchar(255)"}`)
	apply(t, doc, OpColumnAdd, `{"table":"users","name":"created_at","type":"timestamp"}`)
	apply(t, doc, OpIndexAdd, `{"table":"users","name":"ix_email","columns":["email"]}`)

	apply(t, doc, OpIndexUpdate, `{"table":"users","name":"ix_email","newName":"ix_users_email",`+
		`"columns":["created_at","email"],"descending":["created_at"],"unique":true}`)

	users := doc.findTable("users")
	if len(users.Indexes) != 1 {
		t.Fatalf("인덱스 수 = %d (이름을 바꾸면서 하나 더 만들었습니다)", len(users.Indexes))
	}
	idx := users.Indexes[0]
	if idx.Name != "ix_users_email" {
		t.Errorf("이름 = %q", idx.Name)
	}
	if !idx.Unique {
		t.Error("UNIQUE가 켜지지 않았습니다")
	}
	if len(idx.Columns) != 2 || idx.Columns[0].Column != "created_at" || idx.Columns[1].Column != "email" {
		t.Errorf("컬럼 = %+v (보낸 순서 그대로여야 합니다)", idx.Columns)
	}
	if !idx.Columns[0].Descending || idx.Columns[1].Descending {
		t.Errorf("정렬 방향 = %v, %v (created_at만 내림차순)",
			idx.Columns[0].Descending, idx.Columns[1].Descending)
	}

	// 이미 있는 이름으로는 바꿀 수 없다. 허용하면 이름이 열쇠인 다른 op들이
	// 어느 인덱스를 가리키는지 알 수 없게 된다.
	apply(t, doc, OpIndexAdd, `{"table":"users","name":"ix_other","columns":["email"]}`)
	applyErr(t, doc, OpIndexUpdate,
		`{"table":"users","name":"ix_other","newName":"ix_users_email"}`, "conflict", "이미 있습니다")
	if doc.findTable("users").Indexes[1].Name != "ix_other" {
		t.Error("거부된 이름 변경이 적용되었습니다")
	}
}

// 테이블 복제는 컬럼·기본키·인덱스·체크·나가는 외래키를 함께 베낀다.
//
// 제약 이름은 사본의 이름에 맞춰 바꿔야 한다. 그대로 베끼면 PostgreSQL·MS-SQL에서
// 실행 시점에 "이미 있는 제약 이름"으로 실패하고, 그 실패는 마이그레이션을 만들어
// 실행할 때까지 드러나지 않는다.
func TestTableDuplicate(t *testing.T) {
	doc := twoTables(t)
	apply(t, doc, OpPKSet, `{"table":"users","name":"users_pkey","columns":["id"]}`)
	apply(t, doc, OpColumnAdd, `{"table":"users","name":"email","type":"varchar(255)","nullable":false}`)
	apply(t, doc, OpColumnUpdate, `{"table":"users","name":"email","comment":"로그인 아이디"}`)
	apply(t, doc, OpIndexAdd, `{"table":"users","name":"ux_users_email","columns":["email"],"unique":true}`)
	apply(t, doc, OpCheckAdd, `{"table":"users","name":"ck_users_email","expression":"email <> ''"}`)
	apply(t, doc, OpColumnAdd, `{"table":"users","name":"team_id","type":"int"}`)
	apply(t, doc, OpFKAdd, `{"table":"users","name":"fk_users_team","columns":["team_id"],`+
		`"refTable":"orders","refColumns":["id"],"onDelete":"CASCADE"}`)
	// orders → users 로 들어오는 외래키. 이것은 베끼지 않아야 한다.
	apply(t, doc, OpColumnAdd, `{"table":"orders","name":"owner_id","type":"bigint"}`)
	apply(t, doc, OpFKAdd, `{"table":"orders","name":"fk_orders_owner","columns":["owner_id"],`+
		`"refTable":"users","refColumns":["id"]}`)
	apply(t, doc, OpTableMove, `{"key":"users","x":100,"y":200,"color":"#eab308","icon":"users",`+
		`"columnIcons":{"email":"mail"}}`)

	apply(t, doc, OpTableDuplicate, `{"key":"users"}`)

	dup := doc.findTable("users_copy")
	if dup == nil {
		t.Fatalf("사본이 없습니다: %v", tableNamesOf(doc))
	}
	src := doc.findTable("users")

	// 컬럼은 값까지 그대로. 원본과 같은 포인터를 공유하면 한쪽을 고칠 때 다른 쪽이
	// 함께 바뀐다 — 화면에서는 이유를 알 수 없는 동작이 된다.
	if len(dup.Columns) != len(src.Columns) {
		t.Fatalf("컬럼 수 = %d, 원본 %d", len(dup.Columns), len(src.Columns))
	}
	for i := range dup.Columns {
		if dup.Columns[i] == src.Columns[i] {
			t.Fatalf("%s 컬럼이 원본과 같은 객체입니다", dup.Columns[i].Name)
		}
		if dup.Columns[i].Name != src.Columns[i].Name {
			t.Errorf("컬럼 순서/이름이 다릅니다: %q vs %q", dup.Columns[i].Name, src.Columns[i].Name)
		}
	}
	if c := dup.Column("email"); c == nil || c.Comment != "로그인 아이디" || c.Nullable {
		t.Errorf("컬럼 속성이 베껴지지 않았습니다: %+v", c)
	}

	// 제약 이름은 사본 이름으로 바뀐다.
	if dup.PrimaryKey == nil || len(dup.PrimaryKey.Columns) != 1 {
		t.Errorf("기본키 = %+v", dup.PrimaryKey)
	}
	if dup.PrimaryKey.Name != "users_copy_pkey" {
		t.Errorf("기본키 이름 = %q (사본 이름으로 바뀌어야 합니다)", dup.PrimaryKey.Name)
	}
	if len(dup.Indexes) != 1 || dup.Indexes[0].Name != "ux_users_copy_email" {
		t.Errorf("인덱스 = %+v", dup.Indexes)
	}
	if len(dup.Checks) != 1 || dup.Checks[0].Name != "ck_users_copy_email" {
		t.Errorf("체크 = %+v", dup.Checks)
	}
	if len(dup.ForeignKeys) != 1 || dup.ForeignKeys[0].Name != "fk_users_copy_team" {
		t.Fatalf("외래키 = %+v", dup.ForeignKeys)
	}
	if dup.ForeignKeys[0].OnDelete != "CASCADE" {
		t.Errorf("ON DELETE 가 베껴지지 않았습니다: %+v", dup.ForeignKeys[0])
	}
	// 들어오는 외래키는 그대로 원본만 가리킨다.
	orders := doc.findTable("orders")
	for _, fk := range orders.ForeignKeys {
		if strings.EqualFold(fk.RefTable, "users_copy") {
			t.Error("이 테이블을 가리키던 외래키가 사본까지 가리킵니다")
		}
	}

	// 표시 정보는 함께, 자리는 비껴서.
	box := doc.Layout["users_copy"]
	if box == nil || box.X != 140 || box.Y != 240 {
		t.Errorf("자리 = %+v (원본 옆으로 비껴 놓아야 합니다)", box)
	}
	if box.Color != "#eab308" || box.Icon != "users" || box.ColumnIcons["email"] != "mail" {
		t.Errorf("표시 정보가 베껴지지 않았습니다: %+v", box)
	}
	// 컬럼 아이콘 지도를 공유하면 사본에서 아이콘을 바꿀 때 원본도 바뀐다.
	if same := doc.Layout["users"].ColumnIcons; same != nil {
		box.ColumnIcons["email"] = "lock"
		if same["email"] != "mail" {
			t.Error("컬럼 아이콘 지도를 원본과 공유합니다")
		}
		box.ColumnIcons["email"] = "mail"
	}

	// MySQL의 암묵 이름은 그대로 둔다. 바꿀 수 없는 이름이라, 바꿔 적으면 사본의
	// DDL에 아무도 쓰지 않는 제약 이름이 남는다.
	apply(t, doc, OpPKSet, `{"table":"orders","name":"PRIMARY","columns":["id"]}`)
	apply(t, doc, OpTableDuplicate, `{"key":"orders","name":"orders_copy"}`)
	if got := doc.findTable("orders_copy").PrimaryKey.Name; got != "PRIMARY" {
		t.Errorf("사본의 기본키 이름 = %q (PRIMARY 그대로여야 합니다)", got)
	}

	// 이름을 직접 줄 수 있고, 겹치면 거부한다.
	apply(t, doc, OpTableDuplicate, `{"key":"users","name":"users_archive"}`)
	if doc.findTable("users_archive") == nil {
		t.Error("이름을 지정한 복제가 되지 않았습니다")
	}
	applyErr(t, doc, OpTableDuplicate, `{"key":"users","name":"orders"}`, "conflict", "이미 있습니다")
	applyErr(t, doc, OpTableDuplicate, `{"key":"nope"}`, "not_found", "")

	// 이름을 비우면 번호가 붙는다(users_copy 는 이미 있다).
	apply(t, doc, OpTableDuplicate, `{"key":"users"}`)
	if doc.findTable("users_copy2") == nil {
		t.Errorf("두 번째 사본 이름이 users_copy2 가 아닙니다: %v", tableNamesOf(doc))
	}

	// 함께 베낄 것을 고를 수 있다.
	apply(t, doc, OpTableDuplicate,
		`{"key":"users","name":"users_bare","withIndexes":false,"withForeignKeys":false,"withChecks":false}`)
	bare := doc.findTable("users_bare")
	if len(bare.Indexes) != 0 || len(bare.ForeignKeys) != 0 || len(bare.Checks) != 0 {
		t.Errorf("빼기로 지정한 것이 베껴졌습니다: %+v", bare)
	}
	if len(bare.Columns) != len(src.Columns) || bare.PrimaryKey == nil {
		t.Error("컬럼과 기본키는 빼기 대상이 아닙니다")
	}
}

func tableNamesOf(doc *Document) []string {
	out := []string{}
	for _, t := range doc.Schema.Tables {
		out = append(out, t.Name)
	}
	return out
}

// 컬럼을 지우면 그 컬럼을 쓰는 제약도 정리되어야 한다.
// 남겨두면 ERD는 정상으로 보이는데 생성한 DDL이 없는 컬럼을 가리킨다.
func TestColumnDeleteCleansConstraints(t *testing.T) {
	doc := twoTables(t)
	apply(t, doc, OpColumnAdd, `{"table":"users","name":"email","type":"varchar(255)"}`)
	apply(t, doc, OpColumnAdd, `{"table":"users","name":"tenant","type":"int"}`)
	apply(t, doc, OpIndexAdd, `{"table":"users","name":"ux_email","columns":["email"],"unique":true}`)
	apply(t, doc, OpIndexAdd, `{"table":"users","name":"ix_multi","columns":["email","tenant"]}`)
	apply(t, doc, OpColumnAdd, `{"table":"orders","name":"user_email","type":"varchar(255)"}`)
	apply(t, doc, OpFKAdd,
		`{"table":"orders","name":"fk_email","columns":["user_email"],"refTable":"users","refColumns":["email"]}`)

	apply(t, doc, OpColumnDelete, `{"table":"users","name":"email"}`)

	users := doc.findTable("users")
	if users.Column("email") != nil {
		t.Fatal("컬럼이 지워지지 않았습니다")
	}
	for _, idx := range users.Indexes {
		if idx.Name == "ux_email" {
			t.Error("컬럼이 하나도 남지 않은 인덱스가 유지되었습니다")
		}
		for _, part := range idx.Columns {
			if part.Column == "email" {
				t.Errorf("인덱스 %s 가 지워진 컬럼을 가리킵니다", idx.Name)
			}
		}
	}
	if len(doc.findTable("orders").ForeignKeys) != 0 {
		t.Error("지워진 컬럼을 참조하는 외래키가 남았습니다")
	}
	// position은 촘촘하게 재배치되어야 한다.
	for i, c := range users.Columns {
		if c.Position != i+1 {
			t.Errorf("%s position = %d, 기대값 %d", c.Name, c.Position, i+1)
		}
	}
}

func TestPrimaryKeyForcesNotNull(t *testing.T) {
	doc := newDoc(t)
	apply(t, doc, OpTableAdd, `{"name":"t"}`)
	apply(t, doc, OpColumnAdd, `{"table":"t","name":"a","type":"int","nullable":true}`)
	apply(t, doc, OpColumnAdd, `{"table":"t","name":"b","type":"int","nullable":true}`)
	apply(t, doc, OpPKSet, `{"table":"t","columns":["a","b"]}`)

	for _, name := range []string{"a", "b"} {
		if doc.findTable("t").Column(name).Nullable {
			t.Errorf("기본키 컬럼 %s 가 NULL 허용입니다", name)
		}
	}
	applyErr(t, doc, OpPKSet, `{"table":"t","columns":["a","a"]}`, "invalid", "중복")
	applyErr(t, doc, OpPKSet, `{"table":"t","columns":["nope"]}`, "invalid", "없습니다")

	// 빈 목록은 기본키 제거다.
	apply(t, doc, OpPKSet, `{"table":"t","columns":[]}`)
	if doc.findTable("t").PrimaryKey != nil {
		t.Error("기본키가 제거되지 않았습니다")
	}
}

// 고유하지 않은 컬럼을 참조하는 외래키는 실제 DB가 거부한다.
// ERD 단계에서 막지 않으면 마이그레이션 실행 시점에 실패한다.
func TestForeignKeyRequiresUniqueTarget(t *testing.T) {
	doc := twoTables(t)
	apply(t, doc, OpColumnAdd, `{"table":"users","name":"email","type":"varchar(255)"}`)
	apply(t, doc, OpColumnAdd, `{"table":"orders","name":"user_email","type":"varchar(255)"}`)

	applyErr(t, doc, OpFKAdd,
		`{"table":"orders","name":"fk1","columns":["user_email"],"refTable":"users","refColumns":["email"]}`,
		"invalid", "고유 인덱스가 아니어서")

	// 고유 인덱스를 만들면 통과한다.
	apply(t, doc, OpIndexAdd, `{"table":"users","name":"ux_email","columns":["email"],"unique":true}`)
	apply(t, doc, OpFKAdd,
		`{"table":"orders","name":"fk1","columns":["user_email"],"refTable":"users","refColumns":["email"]}`)
}

func TestForeignKeyValidation(t *testing.T) {
	doc := twoTables(t)
	apply(t, doc, OpColumnAdd, `{"table":"orders","name":"user_id","type":"bigint"}`)

	applyErr(t, doc, OpFKAdd,
		`{"table":"orders","name":"fk","columns":["user_id"],"refTable":"nope","refColumns":["id"]}`,
		"invalid", "문서에 없습니다")
	applyErr(t, doc, OpFKAdd,
		`{"table":"orders","name":"fk","columns":["user_id"],"refTable":"users","refColumns":["id","x"]}`,
		"invalid", "컬럼 수")
	applyErr(t, doc, OpFKAdd,
		`{"table":"orders","name":"fk","columns":["user_id"],"refTable":"users","refColumns":["id"],"onDelete":"DROP DATABASE"}`,
		"invalid", "ON DELETE")

	apply(t, doc, OpFKAdd,
		`{"table":"orders","name":"fk","columns":["user_id"],"refTable":"users","refColumns":["id"],"onDelete":"cascade"}`)
	if got := doc.findTable("orders").ForeignKeys[0].OnDelete; got != "CASCADE" {
		t.Errorf("ON DELETE = %q", got)
	}
	applyErr(t, doc, OpFKAdd,
		`{"table":"orders","name":"fk","columns":["user_id"],"refTable":"users","refColumns":["id"]}`,
		"conflict", "이미 있습니다")
}

// 참조되는 테이블을 그냥 지우면 끊긴 참조가 남는다. 기본은 거부, cascade는 명시해야 한다.
func TestTableDeleteGuardsReferences(t *testing.T) {
	doc := twoTables(t)
	apply(t, doc, OpColumnAdd, `{"table":"orders","name":"user_id","type":"bigint"}`)
	apply(t, doc, OpFKAdd,
		`{"table":"orders","name":"fk_o_u","columns":["user_id"],"refTable":"users","refColumns":["id"]}`)

	applyErr(t, doc, OpTableDelete, `{"key":"users"}`, "invalid", "참조하는 외래키가 있습니다")
	if doc.findTable("users") == nil {
		t.Fatal("거부된 삭제가 테이블을 지웠습니다")
	}

	apply(t, doc, OpTableDelete, `{"key":"users","cascade":true}`)
	if doc.findTable("users") != nil {
		t.Error("cascade 삭제가 동작하지 않았습니다")
	}
	if len(doc.findTable("orders").ForeignKeys) != 0 {
		t.Error("cascade가 참조 외래키를 지우지 않았습니다")
	}
	if _, ok := doc.Layout["users"]; ok {
		t.Error("레이아웃 항목이 남았습니다")
	}
}

func TestEnumOps(t *testing.T) {
	doc := newDoc(t)
	apply(t, doc, OpEnumAdd, `{"name":"order_status","values":["new","paid"]}`)
	applyErr(t, doc, OpEnumAdd, `{"name":"order_status","values":["x"]}`, "conflict", "이미 있습니다")
	applyErr(t, doc, OpEnumUpdate, `{"name":"order_status","values":["a","a"]}`, "invalid", "중복")
	applyErr(t, doc, OpEnumUpdate, `{"name":"order_status","values":["it's"]}`, "invalid", "쓸 수 없는 문자")
	apply(t, doc, OpEnumUpdate, `{"name":"order_status","values":["new","paid","shipped"]}`)
	if got := len(doc.Schema.Enums[0].Values); got != 3 {
		t.Errorf("enum 값 수 = %d", got)
	}

	// 문서에 정의된 enum 이름을 타입으로 쓰면 enum 타입으로 해석되어야 한다.
	// introspect가 같은 표현을 쓰므로, 여기서 어긋나면 diff가 영원히 차이를 보고한다.
	apply(t, doc, OpTableAdd, `{"name":"orders"}`)
	apply(t, doc, OpColumnAdd, `{"table":"orders","name":"status","type":"order_status"}`)
	col := doc.findTable("orders").Column("status")
	if col.Type.Base != schema.TypeEnum || col.Type.EnumName != "order_status" {
		t.Errorf("enum 컬럼 타입 = %+v", col.Type)
	}

	// 사용 중인 enum은 지울 수 없다.
	applyErr(t, doc, OpEnumDelete, `{"key":"order_status"}`, "invalid", "사용하는 컬럼")

	apply(t, doc, OpColumnDelete, `{"table":"orders","name":"status"}`)
	apply(t, doc, OpEnumDelete, `{"key":"order_status"}`)
	if len(doc.Schema.Enums) != 0 {
		t.Error("enum이 지워지지 않았습니다")
	}
}

func TestNoteOps(t *testing.T) {
	doc := newDoc(t)
	apply(t, doc, OpNoteAdd, `{"id":"n1","text":"검토 필요","x":10,"y":20}`)
	if n := doc.Note("n1"); n == nil || n.Text != "검토 필요" {
		t.Fatalf("메모 = %+v", doc.Notes)
	}
	// 같은 op 재전송은 중복 생성 대신 무시한다.
	apply(t, doc, OpNoteAdd, `{"id":"n1","text":"다시"}`)
	if len(doc.Notes) != 1 {
		t.Errorf("메모 수 = %d (재전송으로 중복 생성됨)", len(doc.Notes))
	}
	apply(t, doc, OpNoteUpdate, `{"id":"n1","x":99}`)
	if doc.Note("n1").X != 99 || doc.Note("n1").Text != "검토 필요" {
		t.Errorf("패치가 다른 필드를 건드렸습니다: %+v", doc.Note("n1"))
	}
	applyErr(t, doc, OpNoteUpdate, `{"id":"nope","x":1}`, "not_found", "")
	apply(t, doc, OpNoteDelete, `{"id":"n1"}`)
	if len(doc.Notes) != 0 {
		t.Error("메모가 지워지지 않았습니다")
	}
}

// 이동 op는 구조를 바꾸지 않는다. 지문이 바뀌면 테이블을 옮기기만 해도
// 스키마가 변경된 것으로 처리되어 드리프트·버전 비교가 망가진다.
func TestMoveDoesNotChangeFingerprint(t *testing.T) {
	doc := twoTables(t)
	before := doc.Schema.Fingerprint()

	apply(t, doc, OpTableMove, `{"key":"users","x":999,"y":888}`)
	apply(t, doc, OpNoteAdd, `{"id":"n1","text":"메모"}`)

	if after := doc.Schema.Fingerprint(); after != before {
		t.Errorf("레이아웃 변경이 지문을 바꿨습니다: %s → %s", before, after)
	}
	if box := doc.Layout["users"]; box.X != 999 || box.Y != 888 {
		t.Errorf("이동이 반영되지 않았습니다: %+v", box)
	}
	if OpTableMove.Structural() {
		t.Error("table.move가 구조 변경으로 분류되었습니다")
	}
	if !OpColumnAdd.Structural() {
		t.Error("column.add가 구조 변경으로 분류되지 않았습니다")
	}
}

// 컬럼 아이콘은 부분 갱신이고, 컬럼을 따라다녀야 한다.
//
// 통째로 덮어쓰면 같은 표를 함께 보는 두 사람이 서로의 아이콘을 지운다. 이름이
// 바뀌거나 컬럼이 사라졌을 때 뒤처리를 안 하면 아무도 고른 적 없는 아이콘이
// 나중에 되살아난다 — 둘 다 화면에서만 보이는 조용한 어긋남이라 여기서 잡는다.
func TestColumnIcons(t *testing.T) {
	doc := twoTables(t)
	before := doc.Schema.Fingerprint()

	apply(t, doc, OpColumnAdd, `{"table":"users","name":"email","type":"varchar(200)"}`)
	apply(t, doc, OpTableMove,
		`{"key":"users","x":10,"y":20,"columnIcons":{"id":"key","email":"mail"}}`)
	// 다른 컬럼만 담은 두 번째 패치는 앞의 것을 지우지 않아야 한다.
	apply(t, doc, OpTableMove, `{"key":"users","x":10,"y":20,"columnIcons":{"email":"lock"}}`)

	box := doc.Layout["users"]
	if got := box.ColumnIcons["id"]; got != "key" {
		t.Errorf("id 아이콘 = %q (부분 갱신이 앞의 값을 지웠습니다)", got)
	}
	if got := box.ColumnIcons["email"]; got != "lock" {
		t.Errorf("email 아이콘 = %q", got)
	}

	// 빈 값은 "자동으로 되돌리기"다.
	apply(t, doc, OpTableMove, `{"key":"users","x":10,"y":20,"columnIcons":{"id":""}}`)
	if _, ok := box.ColumnIcons["id"]; ok {
		t.Error("빈 값을 보냈는데 아이콘이 남아 있습니다")
	}

	// 이름을 고치면 따라간다.
	apply(t, doc, OpColumnUpdate, `{"table":"users","name":"email","newName":"mail_addr"}`)
	if got := box.ColumnIcons["mail_addr"]; got != "lock" {
		t.Errorf("이름 변경 후 아이콘 = %q (연결이 끊겼습니다)", got)
	}
	if _, ok := box.ColumnIcons["email"]; ok {
		t.Error("옛 이름의 아이콘이 남아 있습니다")
	}

	// 지우면 함께 지워진다.
	apply(t, doc, OpColumnDelete, `{"table":"users","name":"mail_addr"}`)
	if _, ok := box.ColumnIcons["mail_addr"]; ok {
		t.Error("컬럼을 지웠는데 아이콘이 남아 있습니다")
	}

	// 아이콘은 표시 정보다. 지문에 들어가면 아이콘만 바꿔도 드리프트로 잡힌다.
	apply(t, doc, OpTableMove, `{"key":"orders","x":0,"y":0,"columnIcons":{"id":"cart"}}`)
	if after := doc.Schema.Fingerprint(); after != before {
		t.Errorf("아이콘이 지문을 바꿨습니다: %s → %s", before, after)
	}
}

// 실패한 op는 문서를 건드리지 않아야 한다. 호출자가 사본에 적용하는 규약을 검증한다.
func TestFailedOpLeavesCloneUnused(t *testing.T) {
	doc := twoTables(t)
	before := doc.Schema.Fingerprint()

	clone := doc.Clone()
	if err := Apply(clone, op(OpColumnAdd, `{"table":"nope","name":"x","type":"int"}`)); err == nil {
		t.Fatal("없는 테이블에 컬럼 추가가 성공했습니다")
	}
	if after := doc.Schema.Fingerprint(); after != before {
		t.Errorf("원본이 변경되었습니다: %s → %s", before, after)
	}
}

func TestCloneIsDeep(t *testing.T) {
	doc := twoTables(t)
	apply(t, doc, OpColumnAdd, `{"table":"users","name":"email","type":"varchar(255)"}`)
	apply(t, doc, OpIndexAdd, `{"table":"users","name":"ux","columns":["email"],"unique":true}`)
	apply(t, doc, OpNoteAdd, `{"id":"n1","text":"메모"}`)

	clone := doc.Clone()
	apply(t, clone, OpColumnUpdate, `{"table":"users","name":"email","type":"text"}`)
	apply(t, clone, OpTableMove, `{"key":"users","x":500,"y":500}`)
	apply(t, clone, OpNoteUpdate, `{"id":"n1","text":"바뀜"}`)
	apply(t, clone, OpIndexUpdate, `{"table":"users","name":"ux","unique":false}`)

	if got := doc.findTable("users").Column("email").RawType; got != "varchar(255)" {
		t.Errorf("원본 컬럼 타입이 오염되었습니다: %q", got)
	}
	if got := doc.Layout["users"].X; got == 500 {
		t.Error("원본 레이아웃이 오염되었습니다")
	}
	if got := doc.Note("n1").Text; got != "메모" {
		t.Errorf("원본 메모가 오염되었습니다: %q", got)
	}
	if !doc.findTable("users").Indexes[0].Unique {
		t.Error("원본 인덱스가 오염되었습니다")
	}
}

// 두 사용자가 서로 다른 테이블을 동시에 편집하면 순서에 관계없이 같은 결과여야 한다.
func TestIndependentEditsCommute(t *testing.T) {
	build := func(order []int) string {
		doc := twoTables(t)
		ops := []*Op{
			op(OpColumnAdd, `{"table":"users","name":"email","type":"varchar(255)"}`),
			op(OpColumnAdd, `{"table":"orders","name":"total","type":"numeric(10,2)"}`),
			op(OpTableUpdate, `{"key":"users","comment":"회원"}`),
		}
		for _, i := range order {
			if err := Apply(doc, ops[i]); err != nil {
				t.Fatalf("적용 실패: %v", err)
			}
		}
		return doc.Schema.Fingerprint()
	}
	a := build([]int{0, 1, 2})
	b := build([]int{2, 1, 0})
	if a != b {
		t.Errorf("독립 편집의 순서가 결과를 바꿨습니다: %s vs %s", a, b)
	}
}

// 같은 필드를 동시에 고치면 나중 op가 이긴다 (LWW). 서버가 순서를 정하므로
// 모든 참여자가 같은 결론에 도달한다.
func TestSameFieldLastWriteWins(t *testing.T) {
	doc := twoTables(t)
	apply(t, doc, OpColumnAdd, `{"table":"users","name":"email","type":"varchar(100)"}`)
	apply(t, doc, OpColumnUpdate, `{"table":"users","name":"email","type":"varchar(200)"}`)
	apply(t, doc, OpColumnUpdate, `{"table":"users","name":"email","type":"varchar(300)"}`)

	if got := doc.findTable("users").Column("email").RawType; got != "varchar(300)" {
		t.Errorf("마지막 쓰기가 이기지 않았습니다: %q", got)
	}
}

func TestFromSchemaAutoLayout(t *testing.T) {
	sc := &schema.Schema{Dialect: "mysql", Shape: schema.ShapeRelational}
	for _, name := range []string{"a", "b", "c", "d", "e", "f"} {
		sc.Tables = append(sc.Tables, &schema.Table{Name: name})
	}
	doc := FromSchema("d1", "가져온 스키마", "conn1", sc)

	if len(doc.Layout) != 6 {
		t.Fatalf("레이아웃 항목 수 = %d", len(doc.Layout))
	}
	seen := map[[2]float64]bool{}
	for _, box := range doc.Layout {
		pos := [2]float64{box.X, box.Y}
		if seen[pos] {
			t.Errorf("자동 배치가 겹칩니다: %v", pos)
		}
		seen[pos] = true
	}
	if doc.Dialect != "mysql" {
		t.Errorf("dialect = %q", doc.Dialect)
	}
}

// 잘린 payload나 타입이 맞지 않는 payload로 패닉이 나면 한 사람의 잘못된 op가
// 전체 편집 세션을 끊는다.
func TestMalformedPayloadNoPanic(t *testing.T) {
	doc := twoTables(t)
	payloads := []string{
		``, `   `, `{`, `null`, `[]`, `"string"`, `123`,
		`{"table":123}`, `{"key":null}`, `{"columns":"not-an-array"}`,
		`{"table":"users","name":"x","type":[]}`,
	}
	kinds := []Kind{
		OpTableAdd, OpTableUpdate, OpTableMove, OpTableDelete, OpTableDuplicate,
		OpColumnAdd, OpColumnUpdate, OpColumnDelete, OpPKSet,
		OpIndexAdd, OpIndexUpdate, OpIndexDelete,
		OpFKAdd, OpFKUpdate, OpFKDelete,
		OpCheckAdd, OpCheckDelete,
		OpEnumAdd, OpEnumUpdate, OpEnumDelete,
		OpNoteAdd, OpNoteUpdate, OpNoteDelete,
		Kind("nonsense.kind"),
	}
	for _, kind := range kinds {
		for _, p := range payloads {
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Errorf("%s / %q 에서 패닉: %v", kind, p, r)
					}
				}()
				_ = Apply(doc.Clone(), op(kind, p))
			}()
		}
	}
}

func TestCheckConstraint(t *testing.T) {
	doc := twoTables(t)
	apply(t, doc, OpCheckAdd, `{"table":"users","name":"ck_id","expression":"id > 0"}`)
	if got := doc.findTable("users").Checks[0].Expression; got != "id > 0" {
		t.Errorf("체크식 = %q", got)
	}
	// 같은 이름으로 다시 추가하면 갱신이다 (편집 화면에서 수정 = 같은 이름 재전송).
	apply(t, doc, OpCheckAdd, `{"table":"users","name":"ck_id","expression":"id >= 1"}`)
	if len(doc.findTable("users").Checks) != 1 {
		t.Error("같은 이름의 체크 제약이 중복 생성되었습니다")
	}
	applyErr(t, doc, OpCheckAdd,
		`{"table":"users","name":"ck2","expression":"1=1; DROP TABLE users"}`, "invalid", "세미콜론")
	applyErr(t, doc, OpCheckAdd, `{"table":"users","name":"ck3","expression":"  "}`, "invalid", "비어 있습니다")
	apply(t, doc, OpCheckDelete, `{"table":"users","name":"ck_id"}`)
	if len(doc.findTable("users").Checks) != 0 {
		t.Error("체크 제약이 지워지지 않았습니다")
	}
}

// 컬럼 순서는 생성될 DDL의 순서이고, 복합키를 만들 때 사람이 보는 기준이다.
func TestColumnMove(t *testing.T) {
	doc := newDoc(t)
	apply(t, doc, OpTableAdd, `{"name":"users","withId":true}`)
	apply(t, doc, OpColumnAdd, `{"table":"users","name":"email","type":"text"}`)
	apply(t, doc, OpColumnAdd, `{"table":"users","name":"name","type":"text"}`)

	names := func() []string {
		out := []string{}
		for _, c := range doc.Schema.Tables[0].Columns {
			out = append(out, c.Name)
		}
		return out
	}
	if got := strings.Join(names(), ","); got != "id,email,name" {
		t.Fatalf("초기 순서 = %s", got)
	}

	// name을 맨 앞으로
	apply(t, doc, OpColumnMove, `{"table":"users","name":"name","to":1}`)
	if got := strings.Join(names(), ","); got != "name,id,email" {
		t.Errorf("옮긴 뒤 = %s", got)
	}
	// Position은 표시 순서다. 어긋나면 DDL과 화면이 달라진다.
	for i, c := range doc.Schema.Tables[0].Columns {
		if c.Position != i+1 {
			t.Errorf("%s 의 Position = %d, want %d", c.Name, c.Position, i+1)
		}
	}

	// 범위를 벗어난 자리는 양 끝으로 붙는다(끝에서 화살표를 한 번 더 누르는 일은 흔하다).
	apply(t, doc, OpColumnMove, `{"table":"users","name":"name","to":99}`)
	if got := strings.Join(names(), ","); got != "id,email,name" {
		t.Errorf("맨 뒤로 = %s", got)
	}
	applyErr(t, doc, OpColumnMove, `{"table":"users","name":"nope","to":1}`, "not_found", "컬럼")
}

// 그룹·아이콘은 표시 정보다. 구조 지문에 영향을 주면 아이콘만 바꿔도 드리프트가 된다.
func TestGroupsAndIconAreNotStructural(t *testing.T) {
	for _, k := range []Kind{OpGroupAdd, OpGroupUpdate, OpGroupDelete} {
		if k.Structural() {
			t.Errorf("%s 는 구조 op가 아니어야 합니다", k)
		}
	}
	if !OpColumnMove.Structural() {
		t.Error("컬럼 순서는 구조 op여야 합니다")
	}
}

func TestGroupLifecycle(t *testing.T) {
	doc := newDoc(t)
	apply(t, doc, OpGroupAdd, `{"id":"g1","label":"주문","x":10,"y":20,"w":300,"h":200,"color":"blue"}`)
	if len(doc.Groups) != 1 {
		t.Fatalf("그룹 수 = %d", len(doc.Groups))
	}
	// 같은 op가 두 번 와도 하나여야 한다(재전송).
	apply(t, doc, OpGroupAdd, `{"id":"g1","label":"주문"}`)
	if len(doc.Groups) != 1 {
		t.Fatalf("재전송 후 그룹 수 = %d", len(doc.Groups))
	}

	// 패치: 없는 필드는 그대로 둔다.
	apply(t, doc, OpGroupUpdate, `{"id":"g1","label":"주문 도메인"}`)
	g := doc.Group("g1")
	if g.Label != "주문 도메인" || g.X != 10 || g.W != 300 || g.Color != "blue" {
		t.Errorf("패치 결과 = %+v", g)
	}

	// 너무 작은 크기는 보이지 않는 유령이 된다.
	apply(t, doc, OpGroupUpdate, `{"id":"g1","w":1,"h":1}`)
	if g.W < 80 || g.H < 60 {
		t.Errorf("최소 크기가 지켜지지 않았습니다: %+v", g)
	}

	apply(t, doc, OpGroupDelete, `{"id":"g1"}`)
	if len(doc.Groups) != 0 {
		t.Errorf("삭제 후 그룹 수 = %d", len(doc.Groups))
	}
	applyErr(t, doc, OpGroupDelete, `{"id":"g1"}`, "not_found", "그룹")
}

func TestTableIconPatch(t *testing.T) {
	doc := newDoc(t)
	apply(t, doc, OpTableAdd, `{"name":"users"}`)
	apply(t, doc, OpTableMove, `{"key":"users","x":5,"y":6,"icon":"users","color":"#7c3aed"}`)
	box := doc.Layout["users"]
	if box.Icon != "users" || box.Color != "#7c3aed" || box.X != 5 {
		t.Errorf("레이아웃 = %+v", box)
	}
	// 아이콘을 지우는 것도 패치로 표현된다(빈 문자열).
	apply(t, doc, OpTableMove, `{"key":"users","x":5,"y":6,"icon":""}`)
	if doc.Layout["users"].Icon != "" {
		t.Errorf("아이콘이 지워지지 않았습니다: %q", doc.Layout["users"].Icon)
	}
}

// 논리명은 레이아웃에 담기고, 구조 지문을 건드리지 않는다.
//
// 지문에 들어가면 이름을 한국어로 적는 순간 대상 DB와 다르다고(드리프트) 잡힌다.
// 논리명은 DB에 만들어지는 무엇이 아니라 설계용 이름이므로, 그것이 구조를 바꾼
// 것으로 읽혀서는 안 된다.
func TestLogicalNamesStayOutOfFingerprint(t *testing.T) {
	doc := newDoc(t)
	apply(t, doc, OpTableAdd, `{"name":"users","withId":true}`)
	before := doc.Schema.Fingerprint()

	apply(t, doc, OpTableMove,
		`{"key":"users","x":0,"y":0,"logical":"회원","columnLogical":{"id":"회원 번호"}}`)
	if got := doc.Schema.Fingerprint(); got != before {
		t.Errorf("논리명이 구조 지문을 바꿨습니다: %s → %s", before, got)
	}
	box := doc.Layout["users"]
	if box == nil || box.Logical != "회원" {
		t.Fatalf("테이블 논리명이 저장되지 않았습니다: %+v", box)
	}
	if box.ColumnLogical["id"] != "회원 번호" {
		t.Errorf("컬럼 논리명 = %q", box.ColumnLogical["id"])
	}

	// 부분 갱신이어야 한다. 통째로 받으면 같은 표를 함께 보는 두 사람이 서로의
	// 논리명을 지우게 된다.
	apply(t, doc, OpColumnAdd, `{"table":"users","name":"email","type":"varchar(255)"}`)
	apply(t, doc, OpTableMove, `{"key":"users","x":0,"y":0,"columnLogical":{"email":"이메일"}}`)
	if doc.Layout["users"].ColumnLogical["id"] != "회원 번호" {
		t.Error("다른 컬럼의 논리명이 사라졌습니다")
	}

	// 빈 값은 지우는 것이다.
	apply(t, doc, OpTableMove, `{"key":"users","x":0,"y":0,"columnLogical":{"email":""}}`)
	if _, ok := doc.Layout["users"].ColumnLogical["email"]; ok {
		t.Error("빈 값을 보냈는데 논리명이 남았습니다")
	}
}

// 컬럼 이름을 바꾸면 논리명이 따라간다.
//
// 레이아웃은 이름을 열쇠로 쓰므로, 따라가지 않으면 물리명을 고치는 순간 논리명이
// 조용히 사라진다 — 아무도 지우지 않았는데 없어지는 것이 가장 나쁘다.
func TestColumnRenameKeepsLogicalName(t *testing.T) {
	doc := newDoc(t)
	apply(t, doc, OpTableAdd, `{"name":"users"}`)
	apply(t, doc, OpColumnAdd, `{"table":"users","name":"usr_id","type":"bigint"}`)
	apply(t, doc, OpTableMove, `{"key":"users","x":0,"y":0,"columnLogical":{"usr_id":"회원 번호"}}`)

	apply(t, doc, OpColumnUpdate, `{"table":"users","name":"usr_id","newName":"user_id"}`)

	box := doc.Layout["users"]
	if box.ColumnLogical["user_id"] != "회원 번호" {
		t.Errorf("이름을 바꾸자 논리명이 사라졌습니다: %+v", box.ColumnLogical)
	}
	if _, ok := box.ColumnLogical["usr_id"]; ok {
		t.Error("옛 이름의 논리명이 남았습니다")
	}
}

// 테이블을 복제하면 컬럼 논리명은 따라오고 테이블 논리명은 오지 않는다.
//
// 물리명이 users_copy 가 된 사본에 "회원"이 그대로 붙어 있으면 화면에 같은 이름이
// 둘 생긴다 — 사본을 만든 사람이 무엇을 만들었는지 알 수 없게 된다.
func TestTableDuplicateLogicalNames(t *testing.T) {
	doc := newDoc(t)
	apply(t, doc, OpTableAdd, `{"name":"users","withId":true}`)
	apply(t, doc, OpTableMove,
		`{"key":"users","x":0,"y":0,"logical":"회원","columnLogical":{"id":"회원 번호"}}`)

	apply(t, doc, OpTableDuplicate, `{"key":"users","name":"users_copy"}`)

	box := doc.Layout["users_copy"]
	if box == nil {
		t.Fatal("사본 레이아웃이 없습니다")
	}
	if box.Logical != "" {
		t.Errorf("사본에 테이블 논리명이 따라왔습니다: %q", box.Logical)
	}
	if box.ColumnLogical["id"] != "회원 번호" {
		t.Errorf("사본에 컬럼 논리명이 오지 않았습니다: %+v", box.ColumnLogical)
	}
}
