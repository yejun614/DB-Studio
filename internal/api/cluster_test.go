package api

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dbstudio/internal/auth"
	"dbstudio/internal/clock"
	"dbstudio/internal/cluster"
	"dbstudio/internal/config"
	"dbstudio/internal/crypto"
	"dbstudio/internal/model"
	"dbstudio/internal/monitor"
	"dbstudio/internal/store"
)

// 클러스터는 노드 사이의 실제 HTTP 호출로 성립한다. 그래서 이 시험은 마스터를 진짜
// 리스너에 띄우고, 리플리카가 그 주소로 붙게 한다 — 전달·복제·읽기 일관성은 한 프로세스
// 안의 함수 호출로는 확인할 수 없다.

const clusterSecret = "test-cluster-secret"

type clusterEnv struct {
	srv  *Server
	st   *store.Store
	node *cluster.Cluster
	url  string
}

// newClusterNode는 노드 하나를 만든다. addr가 비어 있지 않으면 리스너를 띄운다.
func newClusterNode(t *testing.T, role, masterURL string, listen bool) *clusterEnv {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()

	cfg, err := config.Load([]string{"-data", dir, "-monitor=false", "-log-file="})
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	box, err := crypto.NewSecretBox(make([]byte, 32))
	if err != nil {
		t.Fatalf("secret box: %v", err)
	}
	st, err := store.Open(ctx, filepath.Join(dir, "meta.db"), box)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	// 마스터에만 계정을 만든다. 리플리카의 계정은 복제로 들어와야 하고,
	// 그것이 이 시험이 확인하려는 것 중 하나다.
	if role == cluster.RoleMaster {
		hash, err := crypto.HashPassword(testPassword)
		if err != nil {
			t.Fatalf("hash: %v", err)
		}
		if _, err := st.CreateUser(ctx, store.CreateUserParams{
			Username: "alice", DisplayName: "Alice",
			Role: model.RoleSuperadmin, PasswordHash: hash,
		}); err != nil {
			t.Fatalf("create user: %v", err)
		}
	}

	authn := auth.NewService(st, time.Hour, clock.New(0))
	authz := auth.NewAuthorizer(st)
	mon := monitor.NewManager(st, monitor.DefaultConfig())
	srv := New(cfg, st, authn, authz, mon, os.DirFS(dir))

	ccfg := cluster.DefaultConfig()
	ccfg.Role = role
	ccfg.Secret = clusterSecret
	ccfg.MasterURL = masterURL
	ccfg.NodeName = role
	ccfg.SyncInterval = 50 * time.Millisecond
	ccfg.HeartbeatInterval = 200 * time.Millisecond

	// 리스너를 클러스터보다 먼저 띄운다. 주소를 알아야 -cluster-advertise 를 줄 수 있고,
	// **담당 노드를 시험하려면 그 주소가 있어야 한다** — 주소가 없으면 요청을 넘길 수
	// 없어("담당 노드 ... 의 주소를 알 수 없어") 담당 노드 기능 자체가 시험되지 않는다.
	// 실제 노드도 항상 자기 주소를 준다.
	var url string
	if listen {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		url = "http://" + ln.Addr().String()
		ccfg.Advertise = url
		// 노드 ID는 데이터 디렉터리 파일에서 온다(cluster.New). 리스너를 먼저 띄웠으므로
		// 실패하면 그대로 정리한다.
		t.Cleanup(func() { _ = ln.Close() })
		defer func() { go srv.App().Listener(ln) }()
	}

	node, err := cluster.New(ccfg, st, dir, nil)
	if err != nil {
		t.Fatalf("cluster: %v", err)
	}
	if node.IsReplica() {
		st.SetReplicaMode(node.SendAudit)
	}
	srv.SetCluster(node)

	env := &clusterEnv{srv: srv, st: st, node: node, url: url}
	if listen {
		t.Cleanup(func() { srv.App().Shutdown() })
	}
	return env
}

func (e *clusterEnv) client(t *testing.T) *client {
	return &client{t: t, srv: e.srv, cookies: map[string]string{}}
}

// startCluster는 마스터와 리플리카를 띄우고 복제가 붙을 때까지 기다린다.
func startCluster(t *testing.T) (master, replica *clusterEnv) {
	t.Helper()
	master = newClusterNode(t, cluster.RoleMaster, "", true)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go master.node.Run(ctx)

	replica = newClusterNode(t, cluster.RoleReplica, master.url, true)
	go replica.node.Run(ctx)

	// 마스터의 계정이 복제로 넘어오면 붙은 것이다.
	waitFor(t, 5*time.Second, "복제가 시작되지 않았습니다", func() bool {
		_, err := replica.st.GetUserByUsername(context.Background(), "alice")
		return err == nil
	})
	return master, replica
}

func waitFor(t *testing.T, limit time.Duration, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal(msg)
}

// TestClusterReplicationAndForwarding은 이 기능의 핵심 약속을 확인한다.
//
//   - 리플리카는 마스터의 데이터를 그대로 갖는다(로그인 계정까지).
//   - 리플리카에서 한 변경은 마스터에 저장된다.
//   - 저장 직후 리플리카에서 다시 읽어도 그 변경이 보인다(방금 한 일이 사라지지 않는다).
func TestClusterReplicationAndForwarding(t *testing.T) {
	master, replica := startCluster(t)

	// 로그인부터 리플리카에서 한다. 세션을 만드는 것은 쓰기이므로 마스터가 처리하고,
	// 그 쿠키가 리플리카를 거쳐 브라우저까지 돌아와야 한다.
	c := replica.client(t)
	status, body := c.do("POST", "/api/v1/auth/login",
		map[string]string{"username": "alice", "password": testPassword})
	if status != 200 {
		t.Fatalf("리플리카에서 로그인 = %d: %v", status, body)
	}
	if c.cookies[auth.SessionCookieName] == "" {
		t.Fatal("세션 쿠키가 리플리카를 통해 돌아오지 않았습니다")
	}

	// 서버도 프로젝트 안에 있으므로 먼저 하나 만든다. 이것도 리플리카를 통해
	// 마스터로 전달되어야 하는 쓰기다.
	status, body = c.do("POST", "/api/v1/projects/", map[string]any{"name": "테스트 프로젝트"})
	if status != 201 {
		t.Fatalf("리플리카에서 프로젝트 생성 = %d: %v", status, body)
	}
	madeProject, _ := body["project"].(map[string]any)
	projectID, _ := madeProject["id"].(string)

	// 리플리카에서 서버(=접속 대상)를 하나 만든다.
	status, body = c.do("POST", "/api/v1/servers", map[string]any{
		"projectId": projectID,
		"name":      "pg-1", "kind": "postgres", "host": "10.0.0.9", "port": 5432,
		"defaultEnvironment": "dev",
	})
	if status != 201 && status != 200 {
		t.Fatalf("리플리카에서 생성 = %d: %v", status, body)
	}

	// 마스터에 저장되었는가.
	ctx := context.Background()
	waitFor(t, 3*time.Second, "리플리카에서 만든 것이 마스터에 없습니다", func() bool {
		servers, err := master.st.ListServers(ctx, nil)
		return err == nil && len(servers) == 1
	})

	// 그리고 리플리카에서 곧바로 읽어도 보여야 한다. 쓰기 전달 뒤 복제 지점까지
	// 기다리므로, 화면이 저장 직후 목록을 다시 읽어도 방금 만든 것이 있다.
	status, body = c.do("GET", "/api/v1/servers", nil)
	if status != 200 {
		t.Fatalf("리플리카 조회 = %d: %v", status, body)
	}
	items, _ := body["items"].([]any)
	if len(items) != 1 {
		t.Errorf("리플리카 목록 %d개, 기대 1개 (읽기 일관성이 깨졌습니다)", len(items))
	}

	// 감사 기록은 마스터 한 곳에 모인다.
	var audits int
	if err := master.st.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM audit_logs WHERE action LIKE 'server%'`).Scan(&audits); err != nil {
		t.Fatalf("audit count: %v", err)
	}
	if audits == 0 {
		t.Error("마스터에 감사 기록이 남지 않았습니다")
	}
	var localAudits int
	if err := replica.st.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM audit_logs WHERE action LIKE 'server%'`).Scan(&localAudits); err != nil {
		t.Fatalf("replica audit count: %v", err)
	}
	// 리플리카에도 보인다 — 마스터에 남은 기록이 복제로 돌아온 것이다.
	if localAudits == 0 {
		t.Error("마스터의 감사 기록이 리플리카로 복제되지 않았습니다")
	}
}

// TestClusterStatusView는 화면이 보는 상태를 확인한다.
func TestClusterStatusView(t *testing.T) {
	master, replica := startCluster(t)

	// 리플리카가 하트비트를 보낼 때까지 기다린다(노드 목록에 두 대가 보여야 한다).
	waitFor(t, 5*time.Second, "리플리카가 노드 목록에 나타나지 않았습니다", func() bool {
		nodes, err := master.st.ListClusterNodes(context.Background())
		return err == nil && len(nodes) == 2
	})

	c := replica.client(t)
	if status, body := c.do("POST", "/api/v1/auth/login",
		map[string]string{"username": "alice", "password": testPassword}); status != 200 {
		t.Fatalf("로그인 = %d: %v", status, body)
	}
	status, body := c.do("GET", "/api/v1/cluster/", nil)
	if status != 200 {
		t.Fatalf("클러스터 상태 = %d: %v", status, body)
	}
	st, _ := body["status"].(map[string]any)
	if st["role"] != "replica" {
		t.Errorf("역할 %v, 기대 replica", st["role"])
	}
	nodes, _ := body["nodes"].([]any)
	if len(nodes) != 2 {
		t.Fatalf("노드 %d개, 기대 2개", len(nodes))
	}
	// 마스터가 먼저 오고, 이 노드에는 표시가 붙는다.
	first, _ := nodes[0].(map[string]any)
	if first["role"] != "master" {
		t.Errorf("첫 노드 %v, 기대 master", first["role"])
	}
}

// TestClusterInternalNeedsSecret은 노드 전용 경로가 비밀 없이 열리지 않는지 본다.
//
// 이 경로 하나가 열리면 클러스터의 메타 DB 전체를 스냅샷으로 받아 갈 수 있다.
func TestClusterInternalNeedsSecret(t *testing.T) {
	master, _ := startCluster(t)

	for _, path := range []string{"/api/v1/node/changes?since=0", "/api/v1/node/snapshot"} {
		req, _ := http.NewRequest("GET", master.url+path, nil)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: 비밀 없이 %d (기대 401)", path, res.StatusCode)
		}

		req, _ = http.NewRequest("GET", master.url+path, nil)
		req.Header.Set("Authorization", "Bearer "+clusterSecret)
		res, err = http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s(with secret): %v", path, err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Errorf("%s: 올바른 비밀로 %d (기대 200)", path, res.StatusCode)
		}
	}

	// 로그인한 사람도 이 경로는 쓸 수 없다. 사람의 권한과 노드의 자격은 다른 것이다.
	c := master.client(t)
	if status, _ := c.do("POST", "/api/v1/auth/login",
		map[string]string{"username": "alice", "password": testPassword}); status != 200 {
		t.Fatal("로그인 실패")
	}
	if status, _ := c.do("GET", "/api/v1/node/snapshot", nil); status != 401 {
		t.Errorf("세션만으로 스냅샷 = %d (기대 401)", status)
	}
}

// TestReplicaRejectsMasterOnlyCalls는 리플리카가 마스터인 척하지 않는지 본다.
func TestReplicaRejectsMasterOnlyCalls(t *testing.T) {
	_, replica := startCluster(t)

	req := httptest.NewRequest("GET", "/api/v1/node/changes?since=0", nil)
	req.Header.Set("Authorization", "Bearer "+clusterSecret)
	res, err := replica.srv.App().Test(req, -1)
	if err != nil {
		t.Fatalf("test: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusConflict {
		t.Errorf("리플리카의 changes = %d (기대 409)", res.StatusCode)
	}
	raw, _ := io.ReadAll(res.Body)
	if !strings.Contains(string(raw), "마스터가 아닙니다") {
		t.Errorf("거절 사유가 분명하지 않습니다: %s", raw)
	}
}

// TestMasterDownKeepsReadsWorking은 마스터가 멈춰도 조회가 계속되는지 본다.
//
// 이것이 복제본을 두는 이유다: 마스터가 없는 동안에도 화면이 열리고, 지금까지의 지표와
// 이력을 볼 수 있어야 한다. 변경만 분명한 이유와 함께 거절된다.
func TestMasterDownKeepsReadsWorking(t *testing.T) {
	master, replica := startCluster(t)

	c := replica.client(t)
	if status, _ := c.do("POST", "/api/v1/auth/login",
		map[string]string{"username": "alice", "password": testPassword}); status != 200 {
		t.Fatal("로그인 실패")
	}
	if err := master.srv.App().Shutdown(); err != nil {
		t.Fatalf("마스터 종료: %v", err)
	}

	if status, _ := c.do("GET", "/api/v1/servers", nil); status != 200 {
		t.Errorf("마스터가 없을 때 조회 = %d (기대 200)", status)
	}
	status, body := c.do("POST", "/api/v1/servers", map[string]any{
		"name": "x", "kind": "postgres", "host": "10.0.0.9", "port": 5432,
		"defaultEnvironment": "dev",
	})
	if status != 503 {
		t.Errorf("마스터가 없을 때 생성 = %d (기대 503)", status)
	}
	if msg, _ := body["message"].(string); !strings.Contains(msg, "마스터") {
		t.Errorf("거절 사유가 마스터 때문임을 알려주지 않습니다: %v", body)
	}
}

// TestNodeRoutingUnknownNode는 담당 노드를 찾을 수 없을 때의 답을 본다.
func TestNodeRoutingUnknownNode(t *testing.T) {
	master, _ := startCluster(t)
	ctx := context.Background()

	c := master.client(t)
	if status, _ := c.do("POST", "/api/v1/auth/login",
		map[string]string{"username": "alice", "password": testPassword}); status != 200 {
		t.Fatal("로그인 실패")
	}
	pj, err := master.st.CreateProject(ctx, store.SaveProjectParams{Name: "테스트 프로젝트"})
	if err != nil {
		t.Fatalf("프로젝트 생성: %v", err)
	}
	status, body := c.do("POST", "/api/v1/servers", map[string]any{
		"projectId": pj.ID,
		"name":      "pg-1", "kind": "postgres", "host": "10.0.0.9", "port": 5432,
		"defaultEnvironment": "dev",
	})
	if status != 200 && status != 201 {
		t.Fatalf("서버 생성 = %d: %v", status, body)
	}
	servers, err := master.st.ListServers(ctx, nil)
	if err != nil || len(servers) == 0 {
		t.Fatalf("서버 목록: %v", err)
	}
	conn, err := master.st.CreateConnection(ctx, store.SaveConnectionParams{
		ProjectID: pj.ID,
		ServerID:  servers[0].ID, Name: "db1", Environment: model.EnvDev,
		DatabaseName: "x.db", Enabled: true, NodeID: "없는-노드",
	})
	if err != nil {
		t.Fatalf("커넥션 생성: %v", err)
	}

	status, body = c.do("GET", "/api/v1/connections/"+conn.ID+"/schema", nil)
	if status != 502 {
		t.Errorf("담당 노드가 없을 때 = %d (기대 502): %v", status, body)
	}
	if code, _ := body["error"].(string); code != "unknown_node" {
		t.Errorf("오류 코드 %v, 기대 unknown_node", body["error"])
	}
}

// TestClusterConfigValidation은 잘못된 설정을 시작 시점에 막는지 본다.
func TestClusterConfigValidation(t *testing.T) {
	cases := []struct {
		name string
		cfg  cluster.Config
		ok   bool
	}{
		{"단일 서버", cluster.Config{Role: cluster.RoleStandalone}, true},
		{"비밀 없는 마스터", cluster.Config{Role: cluster.RoleMaster}, false},
		{"비밀 있는 마스터", cluster.Config{Role: cluster.RoleMaster, Secret: "s"}, true},
		{"주소 없는 리플리카", cluster.Config{Role: cluster.RoleReplica, Secret: "s"}, false},
		{"http가 아닌 주소", cluster.Config{
			Role: cluster.RoleReplica, Secret: "s", MasterURL: "master:8080"}, false},
		{"올바른 리플리카", cluster.Config{
			Role: cluster.RoleReplica, Secret: "s", MasterURL: "http://master:8080"}, true},
		{"알 수 없는 역할", cluster.Config{Role: "primary", Secret: "s"}, false},
	}
	for _, tc := range cases {
		err := tc.cfg.Validate()
		if tc.ok && err != nil {
			t.Errorf("%s: %v", tc.name, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("%s: 통과했습니다 (막았어야 합니다)", tc.name)
		}
	}
}

// TestClusterJoinRequiresMaster는 리플리카에 참여 요청이 가면 거절하는지 본다.
func TestClusterJoinRequiresMaster(t *testing.T) {
	_, replica := startCluster(t)

	raw, _ := json.Marshal(cluster.JoinRequest{NodeID: "n9", Name: "n9"})
	req := httptest.NewRequest("POST", "/api/v1/node/join", strings.NewReader(string(raw)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+clusterSecret)
	res, err := replica.srv.App().Test(req, -1)
	if err != nil {
		t.Fatalf("test: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusConflict {
		t.Errorf("리플리카에 참여 요청 = %d (기대 409)", res.StatusCode)
	}
}

// TestIsRoutableSuffix는 커넥션 하위 경로의 담당 노드 라우팅 판정을 고정한다.
//
// ── 이 시험이 필요한 이유 ─────────────────────────────────────────────
// routableSuffix/routablePrefix는 허용 목록이다. 새 경로가 생겼을 때 빠지면
// "접속 테스트는 성공하는데 해당 화면에서는 Access Denied"가 나는 조용한 실패가 된다.
// 이 시험은 "어떤 경로가 담당 노드로 가야 하고 어떤 경로는 가면 안 되는가"를 코드로 명시한다.
func TestIsRoutableSuffix(t *testing.T) {
	cases := []struct {
		suffix string
		want   bool
		why    string
	}{
		// ── 기존 경로 (회귀 보호) ──
		{"/data/objects", true, "데이터 조회: 대상 DB에 직접 접속"},
		{"/data/query", true, "SQL 조회: 대상 DB에 직접 접속"},
		{"/data/mutate", true, "데이터 변경: 대상 DB에 직접 접속"},
		{"/data/batch", true, "일괄 변경: 대상 DB에 직접 접속"},
		{"/statement", true, "SQL 실행: 대상 DB에 직접 접속"},
		{"/statement/check", true, "SQL 구문 검사: 대상 DB에 직접 접속"},
		{"/schema", true, "스키마 조회: introspect"},
		{"/schema/ddl", true, "DDL 생성: introspect"},
		{"/schema/diff", true, "스키마 비교: introspect"},
		{"/explore", true, "탐색 화면: 대상 DB에 직접 접속"},
		{"/logs", true, "로그 조회: 대상 DB에 직접 접속"},
		{"/logs/sources", true, "로그 소스 목록: 대상 DB에 직접 접속"},
		{"/test", true, "접속 테스트: Ping"},

		// ── 새로 추가된 경로 ──
		{"/structure", true, "구조 화면: introspectConnection을 통해 대상 DB에 접속"},
		{"/schema/comments", true, "설명 수정 계획 생성: introspectConnection 호출"},
		{"/storage", true, "스토리지 개요: resolveStorage → GetSecret"},
		{"/storage/browse", true, "스토리지 탐색: resolveStorage → GetSecret"},
		{"/storage/apps", true, "YARN 앱: resolveStorage → GetSecret"},
		{"/storage/pools", true, "Ceph 풀: resolveStorage → GetSecret"},
		{"/storage/osds", true, "Ceph OSD: resolveStorage → GetSecret"},
		{"/storage/buckets", true, "버킷 목록: resolveStorage → GetSecret"},
		{"/storage/objects", true, "오브젝트 목록: resolveStorage → GetSecret"},
		{"/storage/bucket-stat", true, "버킷 통계: resolveStorage → GetSecret"},
		{"/storage/mkdir", true, "디렉터리 생성: resolveStorage → GetSecret"},
		{"/storage/rename", true, "이름 변경: resolveStorage → GetSecret"},
		{"/storage/delete", true, "삭제: resolveStorage → GetSecret"},
		{"/vector", true, "벡터 개요: resolveVector → GetSecret"},
		{"/vector/scroll", true, "벡터 목록: resolveVector → GetSecret"},
		{"/vector/fetch", true, "벡터 조회: resolveVector → GetSecret"},
		{"/vector/search", true, "벡터 검색: resolveVector → GetSecret"},
		{"/vector/compare", true, "벡터 비교: resolveVector → GetSecret"},
		{"/broker", true, "브로커 개요: resolveBroker → GetSecret"},
		{"/broker/queues", true, "큐 목록: resolveBroker → GetSecret"},
		{"/broker/exchanges", true, "익스체인지: resolveBroker → GetSecret"},
		{"/broker/connections", true, "연결 목록: resolveBroker → GetSecret"},
		{"/broker/topics", true, "토픽 목록: resolveBroker → GetSecret"},
		{"/broker/topics/my-topic/config", true, "토픽 설정: routablePrefix로 매칭"},
		{"/broker/topics/complex-name-123/config", true, "동적 토픽 이름: routablePrefix로 매칭"},
		{"/broker/groups", true, "컨슈머 그룹: resolveBroker → GetSecret"},
		{"/broker/purge", true, "큐 비우기: resolveBroker → GetSecret"},
		{"/broker/delete-queue", true, "큐 삭제: resolveBroker → GetSecret"},
		{"/broker/close-connection", true, "연결 끊기: resolveBroker → GetSecret"},
		{"/drift/check", true, "드리프트 감지: monitor.CheckDriftByID → GetSecret"},

		// ── 메타 DB 작업 — 담당 노드로 넘기면 안 된다 ──
		{"/versions", false, "스키마 버전 목록은 메타 DB에서 읽는다"},
		{"/versions/diff", false, "버전 비교는 메타 DB 스냅샷 읽기"},
		{"/metrics", false, "지표는 로컬 시계열 DB에서 읽는다"},
		{"/metrics/available", false, "사용 가능한 지표 목록도 메타 DB"},
		{"/snapshots", false, "스냅샷은 메타 DB에서 읽는다"},
		{"/backups", false, "백업 시작은 메타 DB에 기록을 남긴다 — 마스터에서 처리"},
		{"/impact", false, "삭제 영향 확인은 메타 DB 쿼리만"},
		{"", false, "빈 suffix는 통과시키지 않는다"},
		{"/unknown-future-path", false, "등록되지 않은 경로는 기본적으로 넘기지 않는다"},
	}

	for _, tc := range cases {
		if got := isRoutableSuffix(tc.suffix); got != tc.want {
			t.Errorf("isRoutableSuffix(%q) = %v (기대 %v) — %s",
				tc.suffix, got, tc.want, tc.why)
		}
	}
}
