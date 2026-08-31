package api

import (
	"context"
	"strings"
	"testing"

	"dbstudio/internal/crypto"
	"dbstudio/internal/model"
	"dbstudio/internal/store"
)

// 대상 DB 없는 초안은 권한 판정의 근거가 커넥션이 아니라 계정이다.
// 그 규칙은 라우팅·미들웨어·핸들러가 모두 맞아야 성립하므로 HTTP로 확인한다.

func login(t *testing.T, e *testEnv, username string) *client {
	t.Helper()
	c := e.client(t)
	status, body := c.do("POST", "/api/v1/auth/login",
		map[string]string{"username": username, "password": testPassword})
	if status != 200 {
		t.Fatalf("login(%s) = %d: %v", username, status, body)
	}
	return c
}

// addMember는 멤버 계정을 하나 더 만들고 같은 프로젝트에 넣는다.
//
// "남의 초안"을 만들려면 두 사람이 필요하고, 두 사람이 같은 것을 보려면 같은
// 프로젝트에 있어야 한다 — 프로젝트가 등급보다 앞선 관문이다(0037).
func addMember(t *testing.T, e *testEnv, username string) *model.User {
	t.Helper()
	hash, err := crypto.HashPassword(testPassword)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	u, err := e.st.CreateUser(context.Background(), store.CreateUserParams{
		Username: username, DisplayName: username, Role: model.RoleMember, PasswordHash: hash,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	e.join(t, u.ID)
	return u
}

// 독립 초안에는 대상 DB가 없어 프로젝트가 유일한 울타리다. 그래서 만들 때 반드시
// 프로젝트를 골라야 한다.
func createStandalone(t *testing.T, e *testEnv, c *client, name, dialect string) string {
	t.Helper()
	status, body := c.do("POST", "/api/v1/erd/documents/", map[string]any{
		"name": name, "dialect": dialect, "projectId": e.project.ID,
	})
	if status != 201 {
		t.Fatalf("create = %d: %v", status, body)
	}
	doc, _ := body["document"].(map[string]any)
	id, _ := doc["id"].(string)
	if id == "" {
		t.Fatalf("문서 ID가 없습니다: %v", body)
	}
	if doc["connectionId"] != "" {
		t.Errorf("독립 초안인데 커넥션이 붙어 있습니다: %v", doc["connectionId"])
	}
	return id
}

func TestStandaloneERDNeedsDialect(t *testing.T) {
	e := newTestEnv(t)
	c := login(t, e, "alice")

	status, body := c.do("POST", "/api/v1/erd/documents/",
		map[string]any{"name": "초안", "projectId": e.project.ID})
	if status != 400 {
		t.Fatalf("dialect 없이 만들 수 있으면 안 됩니다: %d %v", status, body)
	}
	status, body = c.do("POST", "/api/v1/erd/documents/",
		map[string]any{"name": "초안", "dialect": "mongodb", "projectId": e.project.ID})
	if status != 400 {
		t.Fatalf("관계형이 아닌 종류가 통과했습니다: %d %v", status, body)
	}
}

// 만든 사람이 아니어도 보고 편집할 수 있어야 한다 — 설계는 함께 하는 일이다.
// 반대로 삭제와 설정 변경은 만든 사람과 어드민만 할 수 있다.
func TestStandaloneERDPermissions(t *testing.T) {
	e := newTestEnv(t)
	owner := login(t, e, "alice") // superadmin
	addMember(t, e, "bob")
	other := login(t, e, "bob")

	docID := createStandalone(t, e, owner, "공용 초안", "postgres")

	// 다른 사람도 열 수 있고 편집 권한을 받는다.
	status, body := other.do("GET", "/api/v1/erd/documents/"+docID, nil)
	if status != 200 {
		t.Fatalf("남의 독립 초안을 열지 못했습니다: %d %v", status, body)
	}
	if body["canEdit"] != true {
		t.Errorf("편집 권한이 없습니다: %v", body["canEdit"])
	}
	if body["canManage"] != false {
		t.Errorf("만든 사람이 아닌데 관리 권한이 있습니다: %v", body["canManage"])
	}

	// 목록에도 보인다. 커넥션 권한이 하나도 없는 멤버여도 마찬가지다.
	status, body = other.do("GET", "/api/v1/erd/documents/", nil)
	if status != 200 {
		t.Fatalf("목록 = %d: %v", status, body)
	}
	items, _ := body["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("목록에 %d개가 보입니다 (1개여야 합니다)", len(items))
	}

	// 설정 변경과 삭제는 거부되어야 한다.
	if status, body := other.do("PATCH", "/api/v1/erd/documents/"+docID,
		map[string]any{"name": "가로채기"}); status != 403 {
		t.Errorf("남의 초안 이름을 바꿀 수 있습니다: %d %v", status, body)
	}
	if status, body := other.do("DELETE", "/api/v1/erd/documents/"+docID, nil); status != 403 {
		t.Errorf("남의 초안을 지울 수 있습니다: %d %v", status, body)
	}

	// 만든 사람은 할 수 있다.
	if status, body := owner.do("DELETE", "/api/v1/erd/documents/"+docID, nil); status != 200 {
		t.Errorf("만든 사람이 지우지 못했습니다: %d %v", status, body)
	}
}

// 대상 DB가 없으면 마이그레이션과 변경 비교는 성립하지 않는다.
// 그 사실을 500이 아니라 이유가 담긴 400으로 알려야 한다.
func TestStandaloneERDHasNoMigrationTarget(t *testing.T) {
	e := newTestEnv(t)
	c := login(t, e, "alice")
	docID := createStandalone(t, e, c, "초안", "mysql")

	status, body := c.do("POST", "/api/v1/erd/documents/"+docID+"/diff", nil)
	if status != 400 {
		t.Errorf("변경 비교 = %d: %v", status, body)
	}
	status, body = c.do("POST", "/api/v1/migrations/", map[string]any{"docId": docID})
	if status != 400 {
		t.Errorf("마이그레이션 생성 = %d: %v", status, body)
	}
}

// SQL 불러오기: 미리보기가 실제 적용과 같은 결과를 말해야 하고,
// 적용 뒤에는 문서가 그대로 반영되어야 한다.
func TestERDImportSQL(t *testing.T) {
	e := newTestEnv(t)
	c := login(t, e, "alice")
	docID := createStandalone(t, e, c, "초안", "postgres")

	script := `
CREATE TABLE users (
  id bigint PRIMARY KEY,
  email text NOT NULL UNIQUE
);
CREATE TABLE orders (
  id bigint PRIMARY KEY,
  user_id bigint NOT NULL REFERENCES users (id) ON DELETE CASCADE
);
`
	// 미리보기는 문서를 바꾸지 않는다.
	status, body := c.do("POST", "/api/v1/erd/documents/"+docID+"/import",
		map[string]any{"sql": script, "dryRun": true})
	if status != 200 {
		t.Fatalf("미리보기 = %d: %v", status, body)
	}
	if body["applied"] != false {
		t.Errorf("미리보기인데 적용됐습니다: %v", body["applied"])
	}
	summary, _ := body["summary"].(map[string]any)
	added, _ := summary["added"].([]any)
	if len(added) != 2 {
		t.Fatalf("추가 예정이 %d개입니다 (2개여야 합니다): %v", len(added), summary)
	}
	doc, err := e.st.GetERDDocument(context.Background(), docID)
	if err != nil {
		t.Fatalf("get doc: %v", err)
	}
	if len(doc.Schema.Tables) != 0 {
		t.Fatalf("미리보기가 문서를 바꿨습니다: %d개", len(doc.Schema.Tables))
	}

	// 적용.
	status, body = c.do("POST", "/api/v1/erd/documents/"+docID+"/import",
		map[string]any{"sql": script})
	if status != 200 {
		t.Fatalf("적용 = %d: %v", status, body)
	}
	if body["applied"] != true {
		t.Errorf("적용 표시가 없습니다: %v", body["applied"])
	}
	doc, err = e.st.GetERDDocument(context.Background(), docID)
	if err != nil {
		t.Fatalf("get doc: %v", err)
	}
	if len(doc.Schema.Tables) != 2 {
		t.Fatalf("테이블 %d개가 들어갔습니다: %+v", len(doc.Schema.Tables), doc.Schema.Tables)
	}
	// 새 테이블은 서로 다른 자리에 놓여야 한다. 겹쳐 쌓이면 화면이 망가진다.
	if len(doc.Layout) != 2 {
		t.Fatalf("좌표가 %d개입니다: %+v", len(doc.Layout), doc.Layout)
	}
	if doc.Layout["users"].X == doc.Layout["orders"].X &&
		doc.Layout["users"].Y == doc.Layout["orders"].Y {
		t.Error("두 테이블이 같은 자리에 놓였습니다")
	}

	// 같은 이름을 다시 불러오면 덮어쓰기다. 테이블 수가 늘어나면 안 된다.
	status, body = c.do("POST", "/api/v1/erd/documents/"+docID+"/import",
		map[string]any{"sql": "CREATE TABLE users (id bigint PRIMARY KEY, name text);"})
	if status != 200 {
		t.Fatalf("덮어쓰기 = %d: %v", status, body)
	}
	doc, _ = e.st.GetERDDocument(context.Background(), docID)
	if len(doc.Schema.Tables) != 2 {
		t.Fatalf("덮어쓰기가 아니라 추가되었습니다: %d개", len(doc.Schema.Tables))
	}
	if tbl := doc.Schema.Table("users"); tbl == nil || len(tbl.Columns) != 2 {
		t.Errorf("users 가 새 정의로 바뀌지 않았습니다: %+v", tbl)
	}

	// DROP은 테이블과 그것을 가리키던 외래키를 함께 지운다.
	status, body = c.do("POST", "/api/v1/erd/documents/"+docID+"/import",
		map[string]any{"sql": "DROP TABLE users; CREATE TABLE audit (id bigint PRIMARY KEY);"})
	if status != 200 {
		t.Fatalf("DROP = %d: %v", status, body)
	}
	doc, _ = e.st.GetERDDocument(context.Background(), docID)
	if doc.Schema.Table("users") != nil {
		t.Error("users 가 남아 있습니다")
	}
	if orders := doc.Schema.Table("orders"); orders != nil && len(orders.ForeignKeys) != 0 {
		t.Errorf("끊어진 외래키가 남았습니다: %+v", orders.ForeignKeys)
	}
}

// SQL 내보내기는 초안을 처음부터 만드는 스크립트를 돌려준다.
func TestERDExportDDL(t *testing.T) {
	e := newTestEnv(t)
	c := login(t, e, "alice")
	docID := createStandalone(t, e, c, "초안", "postgres")

	if status, body := c.do("POST", "/api/v1/erd/documents/"+docID+"/import",
		map[string]any{"sql": "CREATE TABLE t (id bigint PRIMARY KEY, name text NOT NULL);"}); status != 200 {
		t.Fatalf("import = %d: %v", status, body)
	}

	status, body := c.do("GET", "/api/v1/erd/documents/"+docID+"/ddl", nil)
	if status != 200 {
		t.Fatalf("ddl = %d: %v", status, body)
	}
	sql, _ := body["upSql"].(string)
	if sql == "" {
		t.Fatalf("SQL이 비어 있습니다: %v", body)
	}
	for _, want := range []string{"CREATE TABLE", "id", "name"} {
		if !strings.Contains(sql, want) {
			t.Errorf("SQL에 %q 가 없습니다:\n%s", want, sql)
		}
	}

	// 다른 방언으로도 만들 수 있어야 한다.
	status, body = c.do("GET", "/api/v1/erd/documents/"+docID+"/ddl?dialect=mysql", nil)
	if status != 200 {
		t.Fatalf("mysql ddl = %d: %v", status, body)
	}
	if body["dialect"] != "mysql" {
		t.Errorf("방언이 반영되지 않았습니다: %v", body["dialect"])
	}
	status, _ = c.do("GET", "/api/v1/erd/documents/"+docID+"/ddl?dialect=nonsense", nil)
	if status != 400 {
		t.Errorf("알 수 없는 방언이 통과했습니다: %d", status)
	}
}
