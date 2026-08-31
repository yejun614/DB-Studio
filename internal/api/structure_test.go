package api

import (
	"context"
	"strconv"
	"testing"

	"dbstudio/internal/erd"
	"dbstudio/internal/model"
	"dbstudio/internal/schema"
	"dbstudio/internal/store"
)

// 구조 화면은 스키마를 읽고 정리(배치·메모·묶음)를 얹는다. 스키마는 실제 DB나 버전에서,
// 정리는 커넥션마다 하나인 공유 구조 문서에서 온다(0032). 그 조합이 실제로 맞물리는지
// HTTP로 확인한다.

func structureFixture(t *testing.T, e *testEnv, name string) (*model.Connection, int64) {
	t.Helper()
	ctx := context.Background()
	pw := "pw"
	_, conn, err := e.st.CreateServerWithDatabase(ctx,
		store.SaveServerParams{ProjectID: e.project.ID,
			Name: name + "-srv", Kind: model.KindPostgres, DefaultEnvironment: model.EnvDev,
			Host: "h", Port: 5432, Options: model.Options{}, Tags: []string{},
			Enabled: true, Password: &pw,
		},
		store.SaveConnectionParams{
			ProjectID: e.project.ID,
			Name:      name, Environment: model.EnvDev, DatabaseName: name,
			Tags: []string{}, Enabled: true,
		})
	if err != nil {
		t.Fatalf("create connection: %v", err)
	}

	sc := &schema.Schema{
		Dialect: "postgres", Shape: schema.ShapeRelational, Name: name,
		Tables: []*schema.Table{
			{Name: "users", Columns: []*schema.Column{
				{Name: "id", Position: 1, RawType: "bigint"},
				{Name: "email", Position: 2, RawType: "text"},
			}, PrimaryKey: &schema.PrimaryKey{Columns: []string{"id"}}},
			{Name: "orders", Columns: []*schema.Column{
				{Name: "id", Position: 1, RawType: "bigint"},
			}},
		},
	}
	v, _, err := e.st.SaveSchemaVersion(ctx, store.SaveVersionParams{
		ConnectionID: conn.ID, Schema: sc, Source: store.VersionSourceImport,
		Note: "기준선", AuthorID: e.user.ID, AuthorName: "Alice",
	})
	if err != nil {
		t.Fatalf("save version: %v", err)
	}
	return conn, v.ID
}

// 다른 커넥션의 버전을 이 경로로 읽으면 권한 검사를 우회하게 된다.
func TestStructureRejectsForeignVersion(t *testing.T) {
	e := newTestEnv(t)
	c := login(t, e, "alice")
	connA, _ := structureFixture(t, e, "one")
	_, versionB := structureFixture(t, e, "two")

	status, body := c.do("GET",
		"/api/v1/connections/"+connA.ID+"/structure?version="+strconv.FormatInt(versionB, 10), nil)
	if status != 404 {
		t.Errorf("남의 커넥션 버전이 통과했습니다: %d %v", status, body)
	}
}

// 구조 화면의 정리는 이제 커넥션마다 하나인 공유 문서에 있다.
//
// 확인할 것이 셋이다: 같은 커넥션을 보는 두 사람이 **같은 문서**를 받는가(공유),
// 과거 버전을 볼 때는 실시간 방을 열지 않는가, 그리고 옛 개인 정리가 씨앗으로
// 옮겨지는가(바뀌었다고 그동안 적은 것이 사라지면 사람은 잃은 것으로 받아들인다).
func TestStructureDocumentIsSharedPerConnection(t *testing.T) {
	e := newTestEnv(t)
	alice := login(t, e, "alice")
	conn, versionID := structureFixture(t, e, "appdb")

	// 옛 개인 정리를 하나 남겨 둔다(0032 이전에 쌓였을 데이터).
	if err := e.st.SaveStructureView(context.Background(), e.user.ID, conn.ID, &store.StructureView{
		Layout: map[string]*erd.Box{"users": {X: 1234, Y: 567, Color: "#22c55e"}},
		Notes:  []*erd.Note{{ID: "n1", Text: "정산이 여기서 시작", X: 10, Y: 20}},
		Groups: []*erd.Group{{ID: "g1", Label: "계정", W: 400, H: 300}},
	}); err != nil {
		t.Fatalf("옛 정리 저장: %v", err)
	}

	base := "/api/v1/connections/" + conn.ID + "/structure"
	status, body := alice.do("GET", base+"?version="+strconv.FormatInt(versionID, 10), nil)
	if status != 200 {
		t.Fatalf("구조 조회 = %d: %v", status, body)
	}
	// 방은 DB 기준이라 과거 시점을 보는 중에도 같은 방에 있다(누가 접속해 있는지와
	// 대화는 시점과 무관한 사실이다). 다만 편집은 현재 시점에서만 한다.
	if body["documentId"] == "" {
		t.Error("과거 시점 보기에 방 열쇠가 오지 않았습니다")
	}
	if body["canEdit"] != false {
		t.Errorf("과거 버전 보기가 편집 가능으로 왔습니다: %v", body["canEdit"])
	}
	// 씨앗이 옮겨졌는가.
	layout, _ := body["layout"].(map[string]any)
	users, _ := layout["users"].(map[string]any)
	if users["x"] != float64(1234) || users["color"] != "#22c55e" {
		t.Errorf("옛 배치가 옮겨지지 않았습니다: %v", users)
	}
	notes, _ := body["notes"].([]any)
	groups, _ := body["groups"].([]any)
	if len(notes) != 1 || len(groups) != 1 {
		t.Errorf("옛 메모/묶음이 옮겨지지 않았습니다: %v %v", notes, groups)
	}
	// 좌표가 없던 테이블은 자동으로 자리를 받는다(전부 겹쳐 쌓이지 않게).
	if len(layout) != 2 {
		t.Errorf("좌표가 %d개입니다: %v", len(layout), layout)
	}

	// 다른 사람이 열어도 같은 문서다.
	bob := addMember(t, e, "bob")
	if err := e.st.SetAccessPolicy(context.Background(), &model.AccessPolicy{
		UserID: bob.ID, Mode: model.AccessAll, DefaultLevel: model.LevelERD,
		Items: []string{}, Capabilities: map[string]model.Level{},
		// 전체 허용이어도 프로젝트에 참여하지 않으면 아무것도 보이지 않는다.
		Projects: []string{e.project.ID},
	}); err != nil {
		t.Fatalf("set access policy: %v", err)
	}
	bobClient := login(t, e, "bob")
	status, bobBody := bobClient.do("GET", base+"?version="+strconv.FormatInt(versionID, 10), nil)
	if status != 200 {
		t.Fatalf("bob 조회 = %d: %v", status, bobBody)
	}
	bobLayout, _ := bobBody["layout"].(map[string]any)
	bobUsers, _ := bobLayout["users"].(map[string]any)
	if bobUsers["x"] != float64(1234) {
		t.Errorf("공유 정리가 다른 사람에게 보이지 않습니다: %v", bobUsers)
	}
	bobNotes, _ := bobBody["notes"].([]any)
	if len(bobNotes) != 1 {
		t.Errorf("공유 메모가 다른 사람에게 보이지 않습니다: %v", bobNotes)
	}

	// 문서는 커넥션당 하나여야 한다. 둘이 되면 사람마다 다른 방에 들어가고,
	// 서로의 메모가 보이지 않는데 그 사실은 화면에서 알 수 없다.
	docID, err := e.st.GetStructureDocumentID(context.Background(), conn.ID)
	if err != nil {
		t.Fatalf("구조 문서: %v", err)
	}
	doc, err := e.st.GetERDDocument(context.Background(), docID)
	if err != nil {
		t.Fatalf("문서 읽기: %v", err)
	}
	if doc.Kind != store.DocKindStructure {
		t.Errorf("문서 종류 = %q, 기대 structure", doc.Kind)
	}
	// 초안 목록에는 나오지 않아야 한다(0022가 걱정한 오염).
	metas, err := e.st.ListERDDocuments(context.Background(), nil, nil, 100)
	if err != nil {
		t.Fatalf("문서 목록: %v", err)
	}
	for _, m := range metas {
		if m.ID == docID {
			t.Error("구조 문서가 초안 목록에 섞였습니다")
		}
	}
}

// 구조 문서로는 마이그레이션을 만들 수 없다 — 지금 DB의 사본이라 차이가 없다.
func TestStructureDocumentRejectsMigration(t *testing.T) {
	e := newTestEnv(t)
	c := login(t, e, "alice")
	conn, versionID := structureFixture(t, e, "appdb")

	base := "/api/v1/connections/" + conn.ID + "/structure"
	if status, body := c.do("GET", base+"?version="+strconv.FormatInt(versionID, 10), nil); status != 200 {
		t.Fatalf("구조 조회 = %d: %v", status, body)
	}
	docID, err := e.st.GetStructureDocumentID(context.Background(), conn.ID)
	if err != nil {
		t.Fatalf("구조 문서: %v", err)
	}

	status, body := c.do("POST", "/api/v1/migrations/", map[string]any{"docId": docID})
	if status != 400 {
		t.Errorf("구조 문서로 마이그레이션 = %d, 기대 400 (%v)", status, body)
	}
	status, body = c.do("DELETE", "/api/v1/erd/documents/"+docID, nil)
	if status != 400 {
		t.Errorf("구조 문서 삭제 = %d, 기대 400 (%v)", status, body)
	}
}
