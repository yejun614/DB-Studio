package api

import (
	"context"
	"strconv"
	"testing"

	"dbstudio/internal/model"
	"dbstudio/internal/schema"
	"dbstudio/internal/store"
)

// 구조 화면은 스키마를 읽고 배치를 저장한다. 두 가지가 서로 다른 곳에서 오므로
// (스키마는 DB/버전, 배치는 계정) 그 조합이 실제로 맞물리는지 HTTP로 확인한다.

func structureFixture(t *testing.T, e *testEnv, name string) (*model.Connection, int64) {
	t.Helper()
	ctx := context.Background()
	pw := "pw"
	_, conn, err := e.st.CreateServerWithDatabase(ctx,
		store.SaveServerParams{
			Name: name + "-srv", Kind: model.KindPostgres, DefaultEnvironment: model.EnvDev,
			Host: "h", Port: 5432, Options: model.Options{}, Tags: []string{},
			Enabled: true, Password: &pw,
		},
		store.SaveConnectionParams{
			Name: name, Environment: model.EnvDev, DatabaseName: name,
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

func TestStructureVersionAndPersonalView(t *testing.T) {
	e := newTestEnv(t)
	c := login(t, e, "alice")
	conn, versionID := structureFixture(t, e, "appdb")

	base := "/api/v1/connections/" + conn.ID + "/structure"

	// 버전을 고르면 그 시점의 스키마가 온다. 실제 DB에 접속하지 않는다.
	status, body := c.do("GET", base+"?version="+strconv.FormatInt(versionID, 10), nil)
	if status != 200 {
		t.Fatalf("구조 조회 = %d: %v", status, body)
	}
	src, _ := body["source"].(map[string]any)
	if src["kind"] != "version" {
		t.Errorf("출처가 버전이 아닙니다: %v", src)
	}
	sc, _ := body["schema"].(map[string]any)
	tables, _ := sc["tables"].([]any)
	if len(tables) != 2 {
		t.Fatalf("테이블 %d개가 왔습니다: %v", len(tables), sc)
	}

	// 좌표가 없던 테이블은 자동으로 자리를 받는다. 그러지 않으면 전부 겹쳐 쌓인다.
	if body["placed"] != float64(2) {
		t.Errorf("자동 배치 수: %v", body["placed"])
	}
	layout, _ := body["layout"].(map[string]any)
	if len(layout) != 2 {
		t.Fatalf("좌표가 %d개입니다: %v", len(layout), layout)
	}
	users, _ := layout["users"].(map[string]any)
	orders, _ := layout["orders"].(map[string]any)
	if users["x"] == orders["x"] && users["y"] == orders["y"] {
		t.Error("두 테이블이 같은 자리에 놓였습니다")
	}

	// 배치를 저장하면 다음 조회에 그대로 나와야 한다.
	status, body = c.do("PUT", base+"/view", map[string]any{
		"layout": map[string]any{"users": map[string]any{"x": 1234, "y": 567, "color": "#22c55e"}},
		"notes":  []any{map[string]any{"id": "n1", "text": "정산이 여기서 시작", "x": 10, "y": 20}},
		"groups": []any{map[string]any{"id": "g1", "label": "계정", "x": 0, "y": 0, "w": 400, "h": 300}},
	})
	if status != 200 {
		t.Fatalf("배치 저장 = %d: %v", status, body)
	}

	status, body = c.do("GET", base+"?version="+strconv.FormatInt(versionID, 10), nil)
	if status != 200 {
		t.Fatalf("재조회 = %d: %v", status, body)
	}
	layout, _ = body["layout"].(map[string]any)
	saved, _ := layout["users"].(map[string]any)
	if saved["x"] != float64(1234) || saved["color"] != "#22c55e" {
		t.Errorf("저장한 배치가 돌아오지 않았습니다: %v", saved)
	}
	// 저장하지 않은 테이블은 다시 자동 배치된다. 그렇지 않으면 새 테이블이
	// 원점에 겹쳐 쌓인다.
	if body["placed"] != float64(1) {
		t.Errorf("자동 배치 수: %v", body["placed"])
	}
	notes, _ := body["notes"].([]any)
	groups, _ := body["groups"].([]any)
	if len(notes) != 1 || len(groups) != 1 {
		t.Errorf("메모/그룹이 남지 않았습니다: %v %v", notes, groups)
	}
}

// 배치는 계정별이다. 한 사람의 정리가 다른 사람에게 보이면 둘 다 못 쓴다.
func TestStructureViewIsPerUser(t *testing.T) {
	e := newTestEnv(t)
	alice := login(t, e, "alice")
	conn, versionID := structureFixture(t, e, "appdb")

	// 두 번째 사람에게도 이 커넥션이 보여야 비교가 성립한다.
	bob := addMember(t, e, "bob")
	if err := e.st.SetAccessPolicy(context.Background(), &model.AccessPolicy{
		UserID: bob.ID, Mode: model.AccessAll, DefaultLevel: model.LevelMonitor,
		Items: []string{}, Capabilities: map[string]model.Level{},
	}); err != nil {
		t.Fatalf("set access policy: %v", err)
	}
	bobClient := login(t, e, "bob")

	base := "/api/v1/connections/" + conn.ID + "/structure"
	if status, body := alice.do("PUT", base+"/view", map[string]any{
		"layout": map[string]any{"users": map[string]any{"x": 999, "y": 999}},
	}); status != 200 {
		t.Fatalf("alice 저장 = %d: %v", status, body)
	}

	status, body := bobClient.do("GET", base+"?version="+strconv.FormatInt(versionID, 10), nil)
	if status != 200 {
		t.Fatalf("bob 조회 = %d: %v", status, body)
	}
	layout, _ := body["layout"].(map[string]any)
	users, _ := layout["users"].(map[string]any)
	if users["x"] == float64(999) {
		t.Error("다른 사람의 배치가 보입니다")
	}
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
