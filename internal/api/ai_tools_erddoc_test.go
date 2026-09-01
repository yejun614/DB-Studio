package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"dbstudio/internal/model"
	"dbstudio/internal/store"
)

// 일반 어시스턴트도 ERD 초안을 고칠 수 있다.
//
// ERD 화면 안의 대화와 **같은 구현**을 쓴다(document 인자만 더 받는다). 그래서 검증도,
// op-log 기록도, 열어 둔 사람들의 화면 갱신도 똑같다 — 여기에 두 번째 구현을 두면
// 그중 하나에서 검증이 빠지고, 빠진 쪽으로만 이상한 스키마가 들어온다.
func TestERDDocToolsEditDraftByName(t *testing.T) {
	e := newTestEnv(t)
	c := login(t, e, "alice")
	docID := createStandalone(t, e, c, "설계 초안", "postgres")

	registry := aiTools()
	tc := &toolContext{ctx: context.Background(), srv: e.srv, user: e.user}

	run := func(name, args string) (string, error) {
		tool := registry[name]
		if tool == nil {
			t.Fatalf("툴 %s 이(가) 상자에 없습니다", name)
		}
		return tool.Run(tc, json.RawMessage(args))
	}

	if _, err := run("erd_add_table", `{"document":"설계 초안","name":"users"}`); err != nil {
		t.Fatalf("erd_add_table: %v", err)
	}
	if _, err := run("erd_add_column",
		`{"document":"설계 초안","table":"users","name":"id","type":"bigint","nullable":false}`); err != nil {
		t.Fatalf("erd_add_column: %v", err)
	}
	if _, err := run("erd_set_primary_key",
		`{"document":"설계 초안","table":"users","columns":["id"]}`); err != nil {
		t.Fatalf("erd_set_primary_key: %v", err)
	}

	doc, err := e.st.GetERDDocument(context.Background(), docID)
	if err != nil {
		t.Fatalf("get document: %v", err)
	}
	if len(doc.Schema.Tables) != 1 || doc.Schema.Tables[0].Name != "users" {
		t.Fatalf("초안이 바뀌지 않았습니다: %+v", doc.Schema)
	}
	tbl := doc.Schema.Tables[0]
	if len(tbl.Columns) != 1 || tbl.Columns[0].Name != "id" {
		t.Errorf("컬럼 = %+v", tbl.Columns)
	}
	if tbl.PrimaryKey == nil || len(tbl.PrimaryKey.Columns) != 1 {
		t.Errorf("기본키 = %+v", tbl.PrimaryKey)
	}

	// ID로도 가리킬 수 있다. 이름이 겹칠 때 모델이 쓸 수 있어야 한다.
	if _, err := run("erd_read_schema", `{"document":"`+docID+`"}`); err != nil {
		t.Errorf("ID로 열기: %v", err)
	}
}

// document를 빠뜨리거나 없는 것을 가리키면 다음 걸음을 알려준다.
//
// 모델은 실패를 읽고 고쳐 부른다. "오류"만 돌려주면 같은 실패를 반복한다.
func TestERDDocToolsGuideWhenDocumentMissing(t *testing.T) {
	e := newTestEnv(t)
	tc := &toolContext{ctx: context.Background(), srv: e.srv, user: e.user}
	registry := aiTools()

	_, err := registry["erd_add_table"].Run(tc, json.RawMessage(`{"name":"users"}`))
	if err == nil || !strings.Contains(err.Error(), "list_erd_documents") {
		t.Errorf("document 없이 호출 = %v, 다음 걸음을 알려야 합니다", err)
	}
	_, err = registry["erd_read_schema"].Run(tc, json.RawMessage(`{"document":"없는 초안"}`))
	if err == nil || !strings.Contains(err.Error(), "찾을 수 없습니다") {
		t.Errorf("없는 초안 = %v", err)
	}
}

// 남의 초안은 있다는 사실도 알려 주지 않는다.
//
// 찾는 범위가 곧 접근 판정이다 — 목록은 볼 수 있는 커넥션과 참여한 프로젝트로 이미
// 좁혀져 있고, 거기 없는 문서는 이 사람에게 없는 것과 같다.
func TestERDDocToolsHideForeignDrafts(t *testing.T) {
	e := newTestEnv(t)
	alice := login(t, e, "alice")
	createStandalone(t, e, alice, "남의 초안", "postgres")

	// 다른 프로젝트에만 참여한 사람.
	dana := mkUserRole(t, e, "dana", model.RoleMember)
	ctx := context.Background()
	other, err := e.st.CreateProject(ctx, store.SaveProjectParams{Name: "물류"})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	if err := e.st.SetProjectMembers(ctx, other.ID, []string{dana.ID}); err != nil {
		t.Fatalf("참여자: %v", err)
	}

	tc := &toolContext{ctx: ctx, srv: e.srv, user: dana}
	_, err = aiTools()["erd_add_table"].Run(tc,
		json.RawMessage(`{"document":"남의 초안","name":"몰래"}`))
	if err == nil || !strings.Contains(err.Error(), "찾을 수 없습니다") {
		t.Errorf("남의 초안 = %v, 없는 것처럼 답해야 합니다", err)
	}
}

// 툴 목록에 document가 필수로 들어간다.
func TestERDDocToolsDeclareDocumentArg(t *testing.T) {
	tools, _ := availableTools(&model.User{Role: model.RoleSuperadmin}, toolHints{})
	var found bool
	for _, t2 := range tools {
		if t2.Name != "erd_add_column" {
			continue
		}
		found = true
		props, _ := t2.Schema["properties"].(map[string]any)
		if _, ok := props["document"]; !ok {
			t.Errorf("document 인자가 없습니다: %v", props)
		}
		// 원래 인자도 그대로 남아야 한다.
		if _, ok := props["table"]; !ok {
			t.Errorf("원래 인자가 사라졌습니다: %v", props)
		}
		req, _ := t2.Schema["required"].([]string)
		if len(req) == 0 || req[0] != "document" {
			t.Errorf("required = %v, document가 필수여야 합니다", req)
		}
	}
	if !found {
		t.Error("erd_add_column 이 툴 목록에 없습니다")
	}
}
