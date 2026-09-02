package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// 도메인을 툴로 만들고 컬럼에 붙일 수 있어야 한다.
//
// 이 테스트가 생긴 이유: 도메인 op 는 erd 패키지에 다 있었는데 AI 툴로 노출되지
// 않아, 모델이 "제가 쓸 수 있는 툴에는 도메인 기능이 없습니다"라고 답했다.
// 있는 기능을 못 쓰는 것과 없는 기능은 사람에게 똑같이 보인다.
func TestDomainTools(t *testing.T) {
	e := newTestEnv(t)
	c := login(t, e, "alice")
	docID := createStandalone(t, e, c, "도메인 초안", "postgres")

	registry := aiTools()
	tc := &toolContext{ctx: context.Background(), srv: e.srv, user: e.user}
	run := func(name, args string) (string, error) {
		tool := registry[name]
		if tool == nil {
			t.Fatalf("툴 %s 이(가) 상자에 없습니다", name)
		}
		return tool.Run(tc, json.RawMessage(args))
	}
	const doc = `"document":"도메인 초안"`

	if _, err := run("erd_add_domain",
		`{`+doc+`,"name":"money","type":"numeric(18,2)","comment":"금액"}`); err != nil {
		t.Fatalf("add_domain: %v", err)
	}
	if _, err := run("erd_add_table", `{`+doc+`,"name":"orders"}`); err != nil {
		t.Fatalf("add_table: %v", err)
	}
	// 타입 대신 도메인을 준다. 그것이 도메인을 두는 이유다.
	out, err := run("erd_add_column",
		`{`+doc+`,"table":"orders","name":"total","domain":"money","nullable":false}`)
	if err != nil {
		t.Fatalf("add_column with domain: %v", err)
	}
	if !strings.Contains(out, "numeric(18,2)") {
		t.Errorf("도메인을 거친 실제 타입이 결과에 없습니다: %s", out)
	}

	got, err := e.st.GetERDDocument(context.Background(), docID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	col := got.Schema.Tables[0].Column("total")
	if col == nil {
		t.Fatal("컬럼이 없습니다")
	}
	if col.Domain == "" {
		t.Error("컬럼에 도메인이 붙지 않았습니다")
	}
	if !strings.Contains(strings.ToLower(col.RawType), "numeric") {
		t.Errorf("타입 = %q, 도메인 정의를 따라야 합니다", col.RawType)
	}

	// 정의를 고치면 쓰는 컬럼이 함께 바뀐다. 안 바뀌면 도메인은 "이름을 붙인 메모"다.
	res, err := run("erd_update_domain", `{`+doc+`,"name":"money","type":"numeric(20,4)"}`)
	if err != nil {
		t.Fatalf("update_domain: %v", err)
	}
	if !strings.Contains(res, "orders.total") {
		t.Errorf("무엇이 함께 바뀌는지 알려주지 않았습니다: %s", res)
	}
	got, _ = e.st.GetERDDocument(context.Background(), docID)
	if rt := got.Schema.Tables[0].Column("total").RawType; !strings.Contains(rt, "20,4") {
		t.Errorf("도메인을 고쳤는데 컬럼 타입 = %q", rt)
	}

	// read_schema 가 도메인을 보여야 한다. 안 보이면 모델은 타입만 보고
	// "이미 맞다"고 판단해 도메인을 붙일 생각을 하지 않는다.
	schema, err := run("erd_read_schema", `{`+doc+`}`)
	if err != nil {
		t.Fatalf("read_schema: %v", err)
	}
	if !strings.Contains(schema, `"domains"`) || !strings.Contains(schema, "money") {
		t.Errorf("read_schema 에 도메인이 없습니다: %s", schema)
	}

	// 지우면 연결만 끊고 타입은 남는다. 타입까지 지우면 도메인 하나를 정리하려다
	// 여러 컬럼이 타입을 잃는다.
	if _, err := run("erd_detach_domain", `{`+doc+`,"name":"money"}`); err != nil {
		t.Fatalf("detach_domain: %v", err)
	}
	got, _ = e.st.GetERDDocument(context.Background(), docID)
	col = got.Schema.Tables[0].Column("total")
	if col.Domain != "" {
		t.Errorf("도메인 연결이 남았습니다: %q", col.Domain)
	}
	if !strings.Contains(col.RawType, "20,4") {
		t.Errorf("타입까지 사라졌습니다: %q", col.RawType)
	}
}

// 컬럼을 고치면서 도메인만 떼거나 붙일 수 있어야 한다.
func TestUpdateColumnDomain(t *testing.T) {
	e := newTestEnv(t)
	c := login(t, e, "alice")
	docID := createStandalone(t, e, c, "도메인 수정", "postgres")

	registry := aiTools()
	tc := &toolContext{ctx: context.Background(), srv: e.srv, user: e.user}
	run := func(name, args string) (string, error) {
		return registry[name].Run(tc, json.RawMessage(args))
	}
	const doc = `"document":"도메인 수정"`

	mustRun := func(name, args string) {
		t.Helper()
		if _, err := run(name, args); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	mustRun("erd_add_domain", `{`+doc+`,"name":"email","type":"varchar(320)"}`)
	mustRun("erd_add_table", `{`+doc+`,"name":"users"}`)
	mustRun("erd_add_column", `{`+doc+`,"table":"users","name":"mail","type":"text"}`)

	// 붙인다.
	mustRun("erd_update_column", `{`+doc+`,"table":"users","name":"mail","domain":"email"}`)
	got, _ := e.st.GetERDDocument(context.Background(), docID)
	col := got.Schema.Tables[0].Column("mail")
	if col.Domain == "" || !strings.Contains(col.RawType, "320") {
		t.Fatalf("도메인이 붙지 않았습니다: domain=%q type=%q", col.Domain, col.RawType)
	}

	// 이름만 바꿔도 도메인이 풀리면 안 된다. 보내지 않은 것은 그대로 둔다.
	mustRun("erd_update_column", `{`+doc+`,"table":"users","name":"mail","newName":"email_addr"}`)
	got, _ = e.st.GetERDDocument(context.Background(), docID)
	col = got.Schema.Tables[0].Column("email_addr")
	if col == nil {
		t.Fatal("이름이 안 바뀌었습니다")
	}
	if col.Domain == "" {
		t.Error("이름만 바꿨는데 도메인이 풀렸습니다")
	}

	// 빈 문자열은 "연결을 끊는다"이다.
	mustRun("erd_update_column", `{`+doc+`,"table":"users","name":"email_addr","domain":""}`)
	got, _ = e.st.GetERDDocument(context.Background(), docID)
	if d := got.Schema.Tables[0].Column("email_addr").Domain; d != "" {
		t.Errorf("연결이 안 끊겼습니다: %q", d)
	}
}
