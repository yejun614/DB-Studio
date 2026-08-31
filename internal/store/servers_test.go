package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"dbstudio/internal/crypto"
	"dbstudio/internal/model"
)

// 이관(0016)은 무손실이어야 한다.
//
// 15번까지만 적용한 옛 DB를 만들고, 거기에 옛 모양의 커넥션을 넣은 뒤 나머지를
// 적용한다. 이렇게 하지 않으면 "옛 상태에서 시작한다"는 조건을 만들 수 없고,
// 그러면 이 마이그레이션은 실제로 검증되지 않은 채 남의 운영 DB에서 처음 실행된다.
func TestServerMigrationIsLossless(t *testing.T) {
	ctx := context.Background()
	box, err := crypto.NewSecretBox(make([]byte, 32))
	if err != nil {
		t.Fatalf("secret box: %v", err)
	}
	path := filepath.Join(t.TempDir(), "mig.db")

	db, err := sql.Open("sqlite", strings.ReplaceAll(path, "\\", "/")+
		"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	st := &Store{db: db, secret: box}
	if err := st.migrateTo(ctx, 15); err != nil {
		t.Fatalf("migrate to 15: %v", err)
	}

	// 서버 개념이 생기기 전의 커넥션. 접속 정보와 자격증명이 커넥션에 붙어 있다.
	pwEnc, err := box.Seal("s3cret")
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	now := nowString()
	if _, err := db.ExecContext(ctx, `INSERT INTO connections
		(id, name, name_lower, kind, environment, host, port, database_name, options, tags,
		 note, enabled, created_at, updated_at)
		VALUES ('c1','prod-pg','prod-pg','postgres','prod','10.0.1.5',5432,'appdb',
		        '{"sslmode":"require"}','core','메모',1,?,?)`, now, now); err != nil {
		t.Fatalf("insert legacy connection: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO connection_secrets
		(connection_id, username, password_enc, extra_enc, updated_at)
		VALUES ('c1','app',?,'',?)`, pwEnc, now); err != nil {
		t.Fatalf("insert legacy secret: %v", err)
	}

	if err := st.migrate(ctx); err != nil {
		t.Fatalf("migrate rest: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	conn, err := st.GetConnection(ctx, "c1")
	if err != nil {
		t.Fatalf("get after migrate: %v", err)
	}
	if conn.Host != "10.0.1.5" || conn.Port != 5432 || conn.Kind != model.KindPostgres {
		t.Errorf("접속 정보가 사라졌다: %+v", conn)
	}
	if conn.DatabaseName != "appdb" || conn.Environment != model.EnvProd {
		t.Errorf("대상 정보가 달라졌다: %+v", conn)
	}
	if conn.Options.GetOr("sslmode", "") != "require" {
		t.Errorf("옵션이 사라졌다: %v", conn.Options)
	}
	if conn.Note != "메모" || len(conn.Tags) != 1 || conn.Tags[0] != "core" {
		t.Errorf("메모/태그가 사라졌다: note=%q tags=%v", conn.Note, conn.Tags)
	}
	if !conn.Enabled {
		t.Error("활성 상태가 뒤집혔다")
	}

	// 자격증명은 복호화까지 되어야 한다. 봉인 값을 그대로 옮기지 못했다면 여기서 깨진다.
	sec, err := st.GetSecret(ctx, "c1")
	if err != nil || sec.Password != "s3cret" || sec.Username != "app" {
		t.Fatalf("자격증명이 깨졌다: %v %+v", err, sec)
	}

	// 커넥션마다 서버 하나가 생겨야 한다. 자동으로 묶지 않는 것이 의도다 —
	// 봉인된 비밀번호는 같은지 비교할 수조차 없어서, 묶으면 한쪽이 조용히 사라진다.
	servers, err := st.ListServers(ctx, nil)
	if err != nil {
		t.Fatalf("list servers: %v", err)
	}
	if len(servers) != 1 {
		t.Fatalf("서버 수 = %d, 기대 1", len(servers))
	}
	if servers[0].ID != conn.ServerID || servers[0].DatabaseCount != 1 {
		t.Errorf("서버가 커넥션과 이어지지 않았다: %+v", servers[0])
	}
	if servers[0].DefaultEnvironment != model.EnvProd {
		t.Errorf("기본 환경이 옛 값을 따르지 않는다: %s", servers[0].DefaultEnvironment)
	}
}

func serverFixture(t *testing.T) (context.Context, *Store) {
	t.Helper()
	ctx := context.Background()
	box, err := crypto.NewSecretBox(make([]byte, 32))
	if err != nil {
		t.Fatalf("secret box: %v", err)
	}
	st, err := Open(ctx, filepath.Join(t.TempDir(), "srv.db"), box)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return ctx, st
}

func mkServer(t *testing.T, ctx context.Context, st *Store, name string) *model.Server {
	t.Helper()
	pw := "pw"
	srv, err := st.CreateServer(ctx, SaveServerParams{ProjectID: testProject(t, ctx, st).ID,
		Name: name, Kind: model.KindPostgres, Host: "10.0.0.1", Port: 5432,
		DefaultEnvironment: model.EnvDev, Enabled: true, Username: "app", Password: &pw,
	})
	if err != nil {
		t.Fatalf("create server %s: %v", name, err)
	}
	return srv
}

// testProject는 이 시험 DB의 프로젝트를 하나 만들어 두고 그것을 계속 쓴다.
//
// 자원은 모두 프로젝트 안에 있으므로(0037) 커넥션을 만들려면 먼저 하나가 있어야
// 한다. 시험마다 새로 만들면 "같은 서버, 다른 프로젝트"가 되어 이름 중복 규칙이
// 시험하려던 것과 달라진다.
func testProject(t *testing.T, ctx context.Context, st *Store) *Project {
	t.Helper()
	list, err := st.ListProjects(ctx, "")
	if err != nil {
		t.Fatalf("list projects: %v", err)
	}
	if len(list) > 0 {
		return list[0]
	}
	p, err := st.CreateProject(ctx, SaveProjectParams{Name: "테스트 프로젝트"})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	return p
}

func addDB(t *testing.T, ctx context.Context, st *Store, srv *model.Server, db string) *model.Connection {
	t.Helper()
	conn, err := st.CreateConnection(ctx, SaveConnectionParams{
		ProjectID: testProject(t, ctx, st).ID,
		ServerID:  srv.ID, Name: srv.Name + " / " + db, Environment: model.EnvDev,
		DatabaseName: db, Enabled: true,
	})
	if err != nil {
		t.Fatalf("add db %s: %v", db, err)
	}
	return conn
}

// 자격증명이 서버에 한 벌만 있다는 것이 이 구조의 요점이다.
// 한 번 고치면 그 서버의 모든 DB가 새 값을 쓴다 — 여러 벌이면 언젠가 한 벌만 갱신된다.
func TestServerSecretIsSharedByAllDatabases(t *testing.T) {
	ctx, st := serverFixture(t)
	srv := mkServer(t, ctx, st, "prod-pg")
	a := addDB(t, ctx, st, srv, "appdb")
	b := addDB(t, ctx, st, srv, "analytics")

	next := "rotated"
	if _, err := st.UpdateServer(ctx, srv.ID, SaveServerParams{
		Name: srv.Name, Kind: srv.Kind, Host: srv.Host, Port: srv.Port,
		DefaultEnvironment: srv.DefaultEnvironment, Enabled: true,
		Username: "app2", Password: &next,
	}); err != nil {
		t.Fatalf("update server: %v", err)
	}

	for _, conn := range []*model.Connection{a, b} {
		sec, err := st.GetSecret(ctx, conn.ID)
		if err != nil {
			t.Fatalf("get secret: %v", err)
		}
		if sec.Password != next || sec.Username != "app2" {
			t.Errorf("%s 가 옛 자격증명을 쓴다: %+v", conn.Name, sec)
		}
	}
}

// 접속 정보는 서버가 갖지만, 커넥션을 쓰는 쪽(권한·지표·ERD·백업)은 그것을 모른 채
// 예전처럼 conn.Host를 읽는다. 조인이 빠지면 그 코드가 전부 빈 값을 보게 된다.
func TestConnectionInheritsServerTarget(t *testing.T) {
	ctx, st := serverFixture(t)
	srv := mkServer(t, ctx, st, "prod-pg")
	conn := addDB(t, ctx, st, srv, "appdb")

	if conn.Host != "10.0.0.1" || conn.Port != 5432 || conn.Kind != model.KindPostgres {
		t.Errorf("접속 정보가 채워지지 않았다: %+v", conn)
	}
	if conn.ServerID != srv.ID || conn.ServerName != "prod-pg" {
		t.Errorf("서버 정보가 비었다: %+v", conn)
	}
	if conn.Username != "app" {
		t.Errorf("사용자 이름이 비었다: %q", conn.Username)
	}
}

// 서버를 끄면 그 아래 DB가 전부 꺼져야 한다. 앱 곳곳의 `if !conn.Enabled` 관문이
// 실효값을 보기 때문에, 여기가 틀리면 꺼진 서버의 DB에 계속 접속을 시도한다.
func TestDisablingServerDisablesDatabases(t *testing.T) {
	ctx, st := serverFixture(t)
	srv := mkServer(t, ctx, st, "prod-pg")
	addDB(t, ctx, st, srv, "appdb")

	if _, err := st.UpdateServer(ctx, srv.ID, SaveServerParams{
		Name: srv.Name, Kind: srv.Kind, Host: srv.Host, Port: srv.Port,
		DefaultEnvironment: srv.DefaultEnvironment, Enabled: false,
	}); err != nil {
		t.Fatalf("disable server: %v", err)
	}
	conns, _ := st.ListConnectionsByServer(ctx, srv.ID)
	if len(conns) != 1 {
		t.Fatalf("DB 수 = %d", len(conns))
	}
	if conns[0].Enabled {
		t.Error("서버를 껐는데 DB가 켜져 있다")
	}
	if !conns[0].SelfEnabled {
		t.Error("DB 자체 스위치까지 꺼졌다 — 서버를 다시 켜면 되돌아와야 한다")
	}
}

// 같은 DB를 두 번 등록하면 지표와 이벤트가 두 벌씩 쌓인다.
func TestDuplicateDatabaseInSameServerIsRejected(t *testing.T) {
	ctx, st := serverFixture(t)
	srv := mkServer(t, ctx, st, "prod-pg")
	addDB(t, ctx, st, srv, "appdb")

	_, err := st.CreateConnection(ctx, SaveConnectionParams{
		ProjectID: testProject(t, ctx, st).ID,
		ServerID:  srv.ID, Name: "다른 이름", Environment: model.EnvDev,
		DatabaseName: "appdb", Enabled: true,
	})
	if !errors.Is(err, ErrDuplicateName) {
		t.Fatalf("중복 등록이 통과했다: %v", err)
	}
	// 다른 서버의 같은 이름 DB는 막지 않는다 — 서로 다른 대상이다.
	other := mkServer(t, ctx, st, "dev-pg")
	if _, err := st.CreateConnection(ctx, SaveConnectionParams{
		ProjectID: testProject(t, ctx, st).ID,
		ServerID:  other.ID, Name: "dev-pg / appdb", Environment: model.EnvDev,
		DatabaseName: "appdb", Enabled: true,
	}); err != nil {
		t.Fatalf("다른 서버의 같은 이름 DB가 막혔다: %v", err)
	}
}

func TestMergeMovesDatabasesAndClearsEmptyServers(t *testing.T) {
	ctx, st := serverFixture(t)
	target := mkServer(t, ctx, st, "prod-pg")
	addDB(t, ctx, st, target, "appdb")
	src := mkServer(t, ctx, st, "prod-pg-2")
	moved := addDB(t, ctx, st, src, "analytics")

	if err := st.MoveConnectionsToServer(ctx, target.ID, []string{moved.ID}); err != nil {
		t.Fatalf("merge: %v", err)
	}
	got, _ := st.GetConnection(ctx, moved.ID)
	if got.ServerID != target.ID {
		t.Errorf("옮겨지지 않았다: %s", got.ServerID)
	}
	// 옮겨진 DB는 대상 서버의 자격증명을 쓴다.
	if got.Host != target.Host {
		t.Errorf("접속 정보가 대상 서버를 따르지 않는다: %s", got.Host)
	}

	n, err := st.DeleteEmptyServers(ctx)
	if err != nil {
		t.Fatalf("delete empty: %v", err)
	}
	if n != 1 {
		t.Errorf("빈 서버 정리 = %d개, 기대 1개", n)
	}
	if _, err := st.GetServer(ctx, target.ID); err != nil {
		t.Errorf("대상 서버까지 지워졌다: %v", err)
	}
}

// 서버를 지우면 그 아래 DB도 함께 사라진다(CASCADE).
func TestDeletingServerRemovesDatabases(t *testing.T) {
	ctx, st := serverFixture(t)
	srv := mkServer(t, ctx, st, "prod-pg")
	addDB(t, ctx, st, srv, "appdb")
	addDB(t, ctx, st, srv, "analytics")

	if err := st.DeleteServer(ctx, srv.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	left, err := st.ListConnections(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(left) != 0 {
		t.Errorf("DB가 %d개 남았다", len(left))
	}
}
