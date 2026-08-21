package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"dbstudio/internal/store"
)

// ERD 초안 대화는 앱 전체 어시스턴트와 같은 엔드포인트를 쓰되 툴 상자가 다르다.
// 그 경계가 무너지면 두 방향 모두 나쁘다: 초안 대화에서 실제 DB를 건드리거나,
// 반대로 초안을 고칠 수 없게 된다.

func TestERDAISessionIsScopedToDocument(t *testing.T) {
	e := newTestEnv(t)
	c := login(t, e, "alice")
	docID := createStandalone(t, c, "AI 초안", "postgres")

	key := "sk-test"
	if _, err := e.st.CreateAIProvider(context.Background(), store.SaveAIProviderParams{
		Name: "p", Provider: "openai", DefaultModel: "gpt-4o",
		APIKey: &key, Enabled: true, IsDefault: true,
	}); err != nil {
		t.Fatalf("create provider: %v", err)
	}

	status, body := c.do("POST", "/api/v1/erd/documents/"+docID+"/ai/sessions",
		map[string]any{"title": "설계 도우미"})
	if status != http.StatusCreated {
		t.Fatalf("create = %d: %v", status, body)
	}
	sess, _ := body["session"].(map[string]any)
	if sess["erdDocumentId"] != docID {
		t.Fatalf("세션이 문서에 매이지 않았습니다: %v", sess)
	}

	// 목록은 이 문서의 내 대화만 보여준다.
	status, body = c.do("GET", "/api/v1/erd/documents/"+docID+"/ai/sessions", nil)
	if status != http.StatusOK {
		t.Fatalf("list = %d: %v", status, body)
	}
	list, _ := body["sessions"].([]any)
	if len(list) != 1 {
		t.Fatalf("세션 개수 = %d, 1개여야 합니다: %v", len(list), body)
	}

	// 남의 대화는 보이지 않는다. AI 세션은 방 채팅과 달리 개인의 시행착오가
	// 그대로 남는 곳이므로, 같은 문서를 열었다고 서로 읽히면 안 된다.
	addMember(t, e, "bob")
	other := login(t, e, "bob")
	status, body = other.do("GET", "/api/v1/erd/documents/"+docID+"/ai/sessions", nil)
	if status != http.StatusOK {
		t.Fatalf("bob list = %d: %v", status, body)
	}
	if got, _ := body["sessions"].([]any); len(got) != 0 {
		t.Errorf("남의 AI 세션이 보입니다: %v", got)
	}

	// 앱 전체 대화 목록에는 초안 대화가 섞이지 않는다. 섞이면 어시스턴트 화면에서
	// 열었을 때 문서 편집 툴이 붙은 대화를 DB 대화인 줄 알고 이어가게 된다.
	status, body = c.do("GET", "/api/v1/ai/sessions", nil)
	if status != http.StatusOK {
		t.Fatalf("ai sessions = %d: %v", status, body)
	}
	for _, it := range asList(body["sessions"]) {
		if it["erdDocumentId"] != "" && it["erdDocumentId"] != nil {
			t.Errorf("어시스턴트 목록에 초안 대화가 섞였습니다: %v", it)
		}
	}
}

func asList(v any) []map[string]any {
	arr, _ := v.([]any)
	out := make([]map[string]any, 0, len(arr))
	for _, it := range arr {
		if m, ok := it.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// ERD 툴은 실제 DB를 건드리는 툴과 겹치지 않아야 한다.
// 이름이 하나라도 겹치면 어느 상자에서 꺼냈는지에 따라 결과가 달라진다.
func TestERDToolsAreSeparateFromAppTools(t *testing.T) {
	tools, impls := erdAITools()
	if len(tools) != len(impls) {
		t.Fatalf("스키마 %d개, 구현 %d개 — 짝이 맞지 않습니다", len(tools), len(impls))
	}
	for _, tl := range tools {
		if impls[tl.Name] == nil {
			t.Errorf("%s 의 구현이 없습니다", tl.Name)
		}
		// 스키마가 없으면 모델이 인자를 지어낸다.
		if tl.Schema == nil {
			t.Errorf("%s 에 입력 스키마가 없습니다", tl.Name)
		}
	}
	if impls["read_schema"] == nil {
		t.Error("현재 상태를 읽는 툴이 없으면 모델이 없는 테이블을 참조한다")
	}
	// 지우는 툴은 일부러 넣지 않았다 — 되돌리기가 생기기 전까지는
	// 잘못 지운 것을 복구할 방법이 없다.
	for name := range impls {
		if name == "drop_table" || name == "drop_column" {
			t.Errorf("삭제 툴 %s 이 노출됐습니다", name)
		}
	}
}

// 툴이 만든 변경은 초안 문서에 남아야 한다. 이것이 이 기능의 전부다 —
// 응답만 그럴듯하고 문서가 그대로면 아무것도 한 것이 아니다.
func TestERDToolAppliesToDocument(t *testing.T) {
	e := newTestEnv(t)
	c := login(t, e, "alice")
	docID := createStandalone(t, c, "툴 적용", "postgres")

	_, impls := erdAITools()
	ec := &erdToolContext{
		tc:    &toolContext{ctx: context.Background(), srv: e.srv, user: e.user},
		docID: docID,
	}

	out, err := impls["add_table"].Run(ec, json.RawMessage(`{"name":"users"}`))
	if err != nil {
		t.Fatalf("add_table: %v", err)
	}
	if out == "" {
		t.Error("툴이 빈 결과를 돌려주면 모델은 실패인지 성공인지 모른다")
	}
	if _, err := impls["add_column"].Run(ec, json.RawMessage(
		`{"table":"users","name":"id","type":"bigint","nullable":false}`)); err != nil {
		t.Fatalf("add_column: %v", err)
	}
	if _, err := impls["set_primary_key"].Run(ec, json.RawMessage(
		`{"table":"users","columns":["id"]}`)); err != nil {
		t.Fatalf("set_primary_key: %v", err)
	}

	doc, err := e.st.GetERDDocument(context.Background(), docID)
	if err != nil {
		t.Fatalf("get document: %v", err)
	}
	if len(doc.Schema.Tables) != 1 {
		t.Fatalf("테이블 수 = %d: %+v", len(doc.Schema.Tables), doc.Schema)
	}
	tbl := doc.Schema.Tables[0]
	if tbl.Name != "users" || len(tbl.Columns) != 1 || tbl.Columns[0].Name != "id" {
		t.Fatalf("적용 결과가 다릅니다: %+v", tbl)
	}
	if tbl.PrimaryKey == nil || len(tbl.PrimaryKey.Columns) != 1 || tbl.PrimaryKey.Columns[0] != "id" {
		t.Errorf("기본키 = %+v", tbl.PrimaryKey)
	}

	// 같은 이름을 또 만들면 거절해야 한다. 조용히 두 번 만들면 화면에는
	// 같은 카드가 겹쳐 보이고 마이그레이션에서야 터진다.
	if _, err := impls["add_table"].Run(ec, json.RawMessage(`{"name":"users"}`)); err == nil {
		t.Error("이름이 겹치는 테이블이 통과했습니다")
	}
	// 없는 테이블에 컬럼을 붙이는 것도 마찬가지다.
	if _, err := impls["add_column"].Run(ec, json.RawMessage(
		`{"table":"ghost","name":"x","type":"text"}`)); err == nil {
		t.Error("없는 테이블에 컬럼이 추가됐습니다")
	}
}

// AI가 만든 변경도 되돌릴 수 있어야 한다.
//
// 툴에는 삭제가 없지만 "만들어 달라"는 요청 자체를 취소하고 싶은 일은 흔하다.
// 되돌리기가 소켓으로 들어온 편집에만 붙어 있으면 AI가 만든 것만 손으로 지워야 한다.
func TestERDAIEditIsUndoable(t *testing.T) {
	e := newTestEnv(t)
	c := login(t, e, "alice")
	docID := createStandalone(t, c, "되돌리기", "postgres")

	_, impls := erdAITools()
	ec := &erdToolContext{
		tc:    &toolContext{ctx: context.Background(), srv: e.srv, user: e.user},
		docID: docID,
	}
	if _, err := impls["add_table"].Run(ec, json.RawMessage(`{"name":"orders"}`)); err != nil {
		t.Fatalf("add_table: %v", err)
	}

	canUndo, _ := e.srv.erdHub.UndoState(docID, e.user.ID)
	if !canUndo {
		t.Fatal("AI가 만든 편집이 되돌리기 스택에 쌓이지 않았습니다")
	}
}
