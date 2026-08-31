package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"dbstudio/internal/model"
	"dbstudio/internal/store"
)

// 스토리지 화면은 커넥션의 권한 규칙 위에 얹혀 있다. 그 규칙(조회는 모니터링 등급,
// 변경은 data.write)이 실제로 걸리는지, 그리고 Ceph의 조회 전용 원칙이 API에서
// 지켜지는지 HTTP로 확인한다.

// fakeHDFS는 이 시험에 필요한 최소한의 하둡 응답을 돌려준다.
func fakeHDFS(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/jmx", func(w http.ResponseWriter, r *http.Request) {
		qry := r.URL.Query().Get("qry")
		bean := map[string]any{}
		switch {
		case strings.Contains(qry, "NameNodeInfo"):
			bean = map[string]any{"Version": "3.3.6", "Total": 1000.0, "Used": 400.0, "Free": 600.0, "Safemode": ""}
		case strings.Contains(qry, "FSNamesystemState"):
			bean = map[string]any{"NumLiveDataNodes": 2.0, "NumDeadDataNodes": 0.0}
		case strings.Contains(qry, "name=FSNamesystem"):
			bean = map[string]any{"MissingBlocks": 0.0, "UnderReplicatedBlocks": 0.0, "BlocksTotal": 10.0}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"beans": []any{bean}})
	})
	mux.HandleFunc("/webhdfs/v1/", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("op") {
		case "LISTSTATUS":
			_ = json.NewEncoder(w).Encode(map[string]any{"FileStatuses": map[string]any{
				"FileStatus": []any{map[string]any{
					"pathSuffix": "tmp", "type": "DIRECTORY", "owner": "hdfs",
					"group": "hadoop", "permission": "777",
				}},
			}})
		case "GETCONTENTSUMMARY":
			_ = json.NewEncoder(w).Encode(map[string]any{"ContentSummary": map[string]any{
				"directoryCount": 2, "fileCount": 7, "length": 4096,
				"quota": -1, "spaceConsumed": 12288, "spaceQuota": -1,
			}})
		case "MKDIRS", "DELETE":
			_ = json.NewEncoder(w).Encode(map[string]any{"boolean": true})
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// storageFixture는 스토리지 커넥션을 등록한다.
func storageFixture(t *testing.T, e *testEnv, name string, kind model.DBKind, target string) *model.Connection {
	t.Helper()
	host, port := "127.0.0.1", 9870
	if target != "" {
		u, err := url.Parse(target)
		if err != nil {
			t.Fatalf("target: %v", err)
		}
		host = u.Hostname()
		port, _ = strconv.Atoi(u.Port())
	}
	pw := "secret"
	_, conn, err := e.st.CreateServerWithDatabase(context.Background(),
		store.SaveServerParams{ProjectID: e.project.ID,
			Name: name + "-srv", Kind: kind, DefaultEnvironment: model.EnvDev,
			Host: host, Port: port, Options: model.Options{"scheme": "http"},
			Tags: []string{}, Enabled: true, Username: "hdfs", Password: &pw,
		},
		store.SaveConnectionParams{
			ProjectID: e.project.ID,
			Name:      name, Environment: model.EnvDev, Tags: []string{}, Enabled: true,
		})
	if err != nil {
		t.Fatalf("create connection: %v", err)
	}
	return conn
}

func TestStorageOverviewAndBrowse(t *testing.T) {
	e := newTestEnv(t)
	c := login(t, e, "alice")
	hdfs := fakeHDFS(t)
	conn := storageFixture(t, e, "hdfs-prod", model.KindHadoop, hdfs.URL)

	status, body := c.do("GET", "/api/v1/connections/"+conn.ID+"/storage", nil)
	if status != 200 {
		t.Fatalf("개요 = %d: %v", status, body)
	}
	ov, _ := body["overview"].(map[string]any)
	if ov["kind"] != "hadoop" {
		t.Errorf("종류 %v", ov["kind"])
	}
	health, _ := ov["health"].(map[string]any)
	if health["level"] != "ok" {
		t.Errorf("상태 %v", health)
	}
	feats, _ := body["features"].(map[string]any)
	if feats["browse"] != true || feats["pools"] != false {
		t.Errorf("기능 표시가 종류와 맞지 않습니다: %v", feats)
	}

	status, body = c.do("GET", "/api/v1/connections/"+conn.ID+"/storage/browse?path=/user", nil)
	if status != 200 {
		t.Fatalf("탐색 = %d: %v", status, body)
	}
	entries, _ := body["entries"].([]any)
	if len(entries) != 1 {
		t.Fatalf("항목 %d개: %v", len(entries), body)
	}
	if body["summary"] == nil {
		t.Error("경로 요약이 없습니다")
	}
}

// TestStorageWriteNeedsCapability는 디렉터리 조작에 쓰기 능력이 필요한지 본다.
func TestStorageWriteNeedsCapability(t *testing.T) {
	e := newTestEnv(t)
	hdfs := fakeHDFS(t)
	conn := storageFixture(t, e, "hdfs-prod", model.KindHadoop, hdfs.URL)

	// 조회만 가능한 사용자를 만든다.
	addUserWithRole(t, e, "bob", model.RoleMember)
	ctx := context.Background()
	bob, err := e.st.GetUserByUsername(ctx, "bob")
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	if err := e.st.SetAccessPolicy(ctx, &model.AccessPolicy{
		UserID: bob.ID, Mode: model.AccessAllowlist, DefaultLevel: model.LevelNone,
		Items:        []string{conn.ID},
		Capabilities: map[string]model.Level{conn.ID: model.LevelMonitor},
		// 프로젝트 참여가 등급보다 앞선 관문이다.
		Projects: []string{e.project.ID},
	}); err != nil {
		t.Fatalf("save access: %v", err)
	}

	viewer := login(t, e, "bob")
	if status, _ := viewer.do("GET", "/api/v1/connections/"+conn.ID+"/storage", nil); status != 200 {
		t.Errorf("모니터링 등급이 개요를 못 봅니다: %d", status)
	}
	status, body := viewer.do("POST", "/api/v1/connections/"+conn.ID+"/storage/mkdir",
		map[string]any{"path": "/tmp/x"})
	if status != 403 {
		t.Errorf("쓰기 능력 없이 디렉터리를 만들 수 있습니다: %d %v", status, body)
	}

	// 슈퍼 어드민은 만들 수 있어야 한다.
	admin := login(t, e, "alice")
	if status, body := admin.do("POST", "/api/v1/connections/"+conn.ID+"/storage/mkdir",
		map[string]any{"path": "/tmp/x"}); status != 200 {
		t.Errorf("어드민 mkdir = %d: %v", status, body)
	}
	// 감사 로그에 남아야 한다. 파일시스템을 바꾸는 조작은 누가 했는지가 남아야 한다.
	var n int
	if err := e.st.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM audit_logs WHERE action = 'storage.mkdir'`).Scan(&n); err != nil {
		t.Fatalf("audit: %v", err)
	}
	if n != 1 {
		t.Errorf("감사 기록 %d건, 기대 1건", n)
	}
}

// TestStorageDeleteDryRun은 지우기 전에 무엇이 사라지는지 세어 주는지 본다.
func TestStorageDeleteDryRun(t *testing.T) {
	e := newTestEnv(t)
	c := login(t, e, "alice")
	hdfs := fakeHDFS(t)
	conn := storageFixture(t, e, "hdfs-prod", model.KindHadoop, hdfs.URL)

	status, body := c.do("POST",
		"/api/v1/connections/"+conn.ID+"/storage/delete?dryRun=1",
		map[string]any{"path": "/user/tmp", "recursive": true})
	if status != 200 {
		t.Fatalf("미리보기 = %d: %v", status, body)
	}
	if body["files"] == nil || body["directories"] == nil {
		t.Errorf("삭제 영향이 비어 있습니다: %v", body)
	}
	// 미리보기는 아무것도 지우지 않으므로 감사 기록도 없어야 한다.
	var n int
	_ = e.st.DB().QueryRowContext(context.Background(),
		`SELECT count(*) FROM audit_logs WHERE action = 'storage.delete'`).Scan(&n)
	if n != 0 {
		t.Errorf("미리보기가 삭제로 기록됐습니다 (%d건)", n)
	}

	if status, body = c.do("POST", "/api/v1/connections/"+conn.ID+"/storage/delete",
		map[string]any{"path": "/user/tmp", "recursive": true}); status != 200 {
		t.Fatalf("삭제 = %d: %v", status, body)
	}
	_ = e.st.DB().QueryRowContext(context.Background(),
		`SELECT count(*) FROM audit_logs WHERE action = 'storage.delete'`).Scan(&n)
	if n != 1 {
		t.Errorf("삭제 감사 기록 %d건", n)
	}

	// 루트는 어떤 경우에도 막는다.
	if status, _ := c.do("POST", "/api/v1/connections/"+conn.ID+"/storage/delete",
		map[string]any{"path": "/", "recursive": true}); status != 400 {
		t.Errorf("루트 삭제 = %d (기대 400)", status)
	}
}

// TestCephIsReadOnly는 Ceph에 쓰기 경로가 열려 있지 않은지 본다.
func TestCephIsReadOnly(t *testing.T) {
	e := newTestEnv(t)
	c := login(t, e, "alice")
	conn := storageFixture(t, e, "ceph-prod", model.KindCeph, "http://127.0.0.1:8443")

	status, body := c.do("POST", "/api/v1/connections/"+conn.ID+"/storage/mkdir",
		map[string]any{"path": "/x"})
	if status != 400 {
		t.Fatalf("Ceph mkdir = %d (기대 400): %v", status, body)
	}
	if msg, _ := body["message"].(string); !strings.Contains(msg, "되돌릴 수 없") {
		t.Errorf("이유가 설명되지 않습니다: %v", body["message"])
	}
}

// TestStorageRejectsNonStorage는 DB 커넥션에 스토리지 API를 부르면 막는지 본다.
func TestStorageRejectsNonStorage(t *testing.T) {
	e := newTestEnv(t)
	c := login(t, e, "alice")
	conn := storageFixture(t, e, "pg", model.KindPostgres, "http://127.0.0.1:5432")

	status, body := c.do("GET", "/api/v1/connections/"+conn.ID+"/storage", nil)
	if status != 400 {
		t.Fatalf("DB 커넥션의 스토리지 조회 = %d (기대 400): %v", status, body)
	}
}

// TestStorageKindsAreRegistered는 등록 화면이 두 종류를 볼 수 있는지 본다.
func TestStorageKindsAreRegistered(t *testing.T) {
	e := newTestEnv(t)
	c := login(t, e, "alice")

	status, body := c.do("GET", "/api/v1/meta", nil)
	if status != 200 {
		t.Fatalf("meta = %d", status)
	}
	kinds, _ := body["dbKinds"].([]any)
	found := map[string]map[string]any{}
	for _, raw := range kinds {
		k, _ := raw.(map[string]any)
		name, _ := k["kind"].(string)
		found[name] = k
	}
	for _, want := range []string{"hadoop", "ceph"} {
		k, ok := found[want]
		if !ok {
			t.Fatalf("%s 종류가 등록 목록에 없습니다", want)
		}
		caps, _ := k["capabilities"].(map[string]any)
		if caps["storage"] != true {
			t.Errorf("%s: storage 능력이 꺼져 있습니다: %v", want, caps)
		}
		// 스키마·마이그레이션은 없어야 한다. 켜져 있으면 화면이 빈 스키마 메뉴를 그린다.
		if caps["introspect"] == true || caps["migrate"] == true || caps["erd"] == true {
			t.Errorf("%s: DB 전용 능력이 켜져 있습니다: %v", want, caps)
		}
		if k["needsDb"] == true {
			t.Errorf("%s: 데이터베이스 이름을 요구하고 있습니다", want)
		}
	}
}
