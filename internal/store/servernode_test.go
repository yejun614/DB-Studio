package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"dbstudio/internal/crypto"
	"dbstudio/internal/model"
)

// 담당 노드가 서버 등급이 되면서 새로 생긴 규칙들을 여기서 확인한다.
//
// ── 왜 이 시험들이 필요한가 ─────────────────────────────────────────
// 담당 노드는 이제 서버에 있고(host·port와 같은 급의 접속 사실), connections.node_id는
// 그것을 덮는 **예외**다. 그 우선순위는 조회 한 곳에서만 합쳐진다(connections.go의
// connColumns). 그 한 줄이 틀리면 라우팅·지표 묶음 키·화면이 전부 조용히 어긋나고,
// 어느 쪽도 오류를 내지 않는다 — 증상은 "어떤 화면에서는 되고 어떤 화면에서는 안 된다"다.

// TestConnectionInheritsServerNode는 담당 노드가 서버 → DB로 내려오는지 본다.
//
// 이것이 이 기능의 핵심 약속이다: 서버에 한 번 지정하면 그 아래 DB가 전부 따라가고,
// DB에 값이 있으면 그쪽이 이긴다(예외). 둘 다 없으면 빈 값이다.
func TestConnectionInheritsServerNode(t *testing.T) {
	ctx, st := serverFixture(t)
	pj := testProject(t, ctx, st)

	srv, err := st.CreateServer(ctx, SaveServerParams{
		ProjectID: pj.ID, Name: "srv-inherit", Kind: model.KindPostgres,
		Host: "10.0.0.9", Port: 5432, DefaultEnvironment: model.EnvDev,
		Enabled: true, NodeID: "노드-A",
	})
	if err != nil {
		t.Fatalf("서버 생성: %v", err)
	}

	// DB가 담당을 비우면 서버 값을 받아야 한다.
	inherit, err := st.CreateConnection(ctx, SaveConnectionParams{
		ProjectID: pj.ID, ServerID: srv.ID, Name: "d-inherit",
		Environment: model.EnvDev, DatabaseName: "d1", Enabled: true,
	})
	if err != nil {
		t.Fatalf("커넥션 생성: %v", err)
	}
	if inherit.NodeID != "노드-A" {
		t.Errorf("상속된 담당 노드 = %q (기대 노드-A)", inherit.NodeID)
	}

	// DB에 값이 있으면 그 값이 이긴다 — 이 칸은 서버를 덮는 예외다.
	override, err := st.CreateConnection(ctx, SaveConnectionParams{
		ProjectID: pj.ID, ServerID: srv.ID, Name: "d-override",
		Environment: model.EnvDev, DatabaseName: "d2", Enabled: true, NodeID: "노드-B",
	})
	if err != nil {
		t.Fatalf("커넥션 생성: %v", err)
	}
	if override.NodeID != "노드-B" {
		t.Errorf("예외 담당 노드 = %q (기대 노드-B)", override.NodeID)
	}

	// 서버의 담당 노드를 나중에 바꾸면 **따르던 DB가 함께 움직여야** 한다.
	// 움직이지 않으면 그 DB는 조용히 예외로 굳은 것이고, 사람은 왜 안 따라오는지
	// 알 방법이 없다.
	if err := st.SetServerNode(ctx, srv.ID, "노드-C"); err != nil {
		t.Fatalf("서버 담당 노드 변경: %v", err)
	}
	moved, err := st.GetConnection(ctx, inherit.ID)
	if err != nil {
		t.Fatalf("커넥션 조회: %v", err)
	}
	if moved.NodeID != "노드-C" {
		t.Errorf("따르던 DB가 움직이지 않았습니다: %q (기대 노드-C) — "+
			"저장된 값이 그대로 남아 예외로 굳었습니다", moved.NodeID)
	}
	fixed, err := st.GetConnection(ctx, override.ID)
	if err != nil {
		t.Fatalf("커넥션 조회: %v", err)
	}
	if fixed.NodeID != "노드-B" {
		t.Errorf("예외 DB가 따라 움직였습니다: %q (기대 노드-B)", fixed.NodeID)
	}

	// 둘 다 비면 빈 값이어야 한다 — "요청을 받은 노드가 접속한다"는 정상 상태다.
	plain := mkServer(t, ctx, st, "srv-plain")
	plainConn := addDB(t, ctx, st, plain, "appdb")
	if plainConn.NodeID != "" {
		t.Errorf("담당이 없으면 빈 값이어야 합니다: %q", plainConn.NodeID)
	}
}

// hasColumn은 그 표에 그 컬럼이 있는지 본다.
//
// 이관 시험에서 필요한 이유: "옛 스키마로 시작했다"를 확인해야 시험이 의미를 갖는다.
// 확인 없이 돌리면 이미 최신인 DB에서 돌아 통과하지만 아무것도 검증하지 못한다.
func hasColumn(t *testing.T, st *Store, table, column string) bool {
	t.Helper()
	rows, err := st.db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		t.Fatalf("table_info(%s): %v", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan column: %v", err)
		}
		if name == column {
			return true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate columns: %v", err)
	}
	return false
}

// TestServerNodeImpactCounts는 영향 범위를 세는 값이 실제와 맞는지 본다.
//
// 화면의 확인 창이 이 숫자로 "그 서버의 DB 전부가 바뀐다"를 말한다. 숫자가 틀리면
// 사람은 잘못된 크기를 근거로 저장을 누른다.
func TestServerNodeImpactCounts(t *testing.T) {
	ctx, st := serverFixture(t)
	pj := testProject(t, ctx, st)

	srv, err := st.CreateServer(ctx, SaveServerParams{
		ProjectID: pj.ID, Name: "srv-impact", Kind: model.KindPostgres,
		Host: "10.0.0.9", Port: 5432, DefaultEnvironment: model.EnvDev, Enabled: true,
	})
	if err != nil {
		t.Fatalf("서버 생성: %v", err)
	}
	// DB 3개 중 하나만 예외로 지정한다.
	for i, node := range []string{"", "", "노드-X"} {
		if _, err := st.CreateConnection(ctx, SaveConnectionParams{
			ProjectID: pj.ID, ServerID: srv.ID, Name: "d" + string(rune('a'+i)),
			Environment: model.EnvDev, DatabaseName: "d" + string(rune('a'+i)),
			Enabled: true, NodeID: node,
		}); err != nil {
			t.Fatalf("커넥션 생성: %v", err)
		}
	}

	ch, err := st.DescribeServerNodeChange(ctx, srv.ID)
	if err != nil {
		t.Fatalf("영향 범위: %v", err)
	}
	if ch.Databases != 3 {
		t.Errorf("DB 수 = %d (기대 3)", ch.Databases)
	}
	if ch.Overridden != 1 {
		t.Errorf("예외 수 = %d (기대 1)", ch.Overridden)
	}
	if got := ch.Databases - ch.Overridden; got != 2 {
		t.Errorf("따라오는 DB 수 = %d (기대 2)", got)
	}

	// 노드를 내릴 때 화면이 쓰는 값.
	if err := st.SetServerNode(ctx, srv.ID, "노드-X"); err != nil {
		t.Fatalf("서버 담당 노드 변경: %v", err)
	}
	n, err := st.ServersOnNode(ctx, "노드-X")
	if err != nil {
		t.Fatalf("담당 서버 수: %v", err)
	}
	if n != 1 {
		t.Errorf("노드-X 담당 서버 = %d (기대 1)", n)
	}
	all, err := st.ServersOnNodeAll(ctx)
	if err != nil {
		t.Fatalf("담당 서버 수(전체): %v", err)
	}
	if all["노드-X"] != 1 {
		t.Errorf("한 번에 센 값 = %v (기대 노드-X:1)", all)
	}
	// 담당이 없는 서버는 어느 노드에도 세지 않는다 — 그 줄은 특정 노드가 빠져도
	// 영향받지 않는다("요청을 받은 노드가 접속").
	if _, ok := all[""]; ok {
		t.Errorf("담당 없는 서버가 노드로 세어졌습니다: %v", all)
	}
}

// TestServerNodeMigrationPromotesExisting는 기존 데이터가 서버로 올라가는지 본다.
//
// 0029까지는 담당 노드가 DB마다 있었다. 0044가 그것을 서버로 올리는데, 그 판단이
// 틀리면(아무것도 올리지 않거나, 예외를 잘못 올리면) 업그레이드한 사람의 사설망 DB가
// 조용히 마스터에서 접속을 시도하게 된다.
//
// 이관 마이그레이션은 **옛 상태에서 시작해야** 검증할 수 있다. 그래서 43번까지만 적용한
// DB를 만들고 거기에 옛 모양의 행을 넣은 뒤 나머지를 적용한다.
func TestServerNodeMigrationPromotesExisting(t *testing.T) {
	ctx := context.Background()
	box, err := crypto.NewSecretBox(make([]byte, 32))
	if err != nil {
		t.Fatalf("secret box: %v", err)
	}
	path := filepath.Join(t.TempDir(), "node-mig.db")
	db, err := sql.Open("sqlite", strings.ReplaceAll(path, "\\", "/")+
		"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	st := &Store{db: db, secret: box}
	t.Cleanup(func() { st.Close() })
	if err := st.migrateTo(ctx, 43); err != nil {
		t.Fatalf("migrate to 43: %v", err)
	}

	// 옛 상태를 만든다: 서버 하나에 DB 셋, 그중 둘이 같은 노드를 가리키고 하나는 다르다.
	//
	// 43번까지는 서버에 node_id 컬럼이 없으므로 **직접 SQL로 넣는다.** 정상 경로
	// (CreateServer)를 쓰면 "그런 컬럼이 없다"로 죽는데, 그것은 옛 스키마의 증거이긴
	// 하지만 시험이 검증하려는 것(0044의 승격)까지 가지 못한다.
	pj, err := st.CreateProject(ctx, SaveProjectParams{Name: "p"})
	if err != nil {
		t.Fatalf("프로젝트: %v", err)
	}
	now := nowString()
	if _, err := st.db.ExecContext(ctx, `INSERT INTO servers
		(id, project_id, name, name_lower, kind, host, port, options, default_environment,
		 tags, note, enabled, created_at, updated_at)
		VALUES ('srv-old','`+pj.ID+`','old-srv','old-srv','postgres','10.0.0.9',5432,
		        '{}','dev','','',1,?,?)`, now, now); err != nil {
		t.Fatalf("옛 서버 넣기: %v", err)
	}
	for i, node := range []string{"busan", "busan", "seoul"} {
		if _, err := st.db.ExecContext(ctx, `INSERT INTO connections
			(id, project_id, server_id, name, name_lower, environment, database_name,
			 node_id, tags, note, enabled, created_at, updated_at)
			VALUES (?, ?, 'srv-old', ?, ?, 'dev', ?, ?, '', '', 1, ?, ?)`,
			"c"+string(rune('a'+i)), pj.ID, "d"+string(rune('a'+i)),
			"d"+string(rune('a'+i)), "d"+string(rune('a'+i)), node, now, now); err != nil {
			t.Fatalf("옛 커넥션 넣기: %v", err)
		}
	}

	// 옛 상태임을 스스로 확인한다. 이 확인이 없으면 시험이 "이미 새 스키마"에서 돌아
	// 아무것도 검증하지 못한다 — 통과하지만 의미가 없는 시험이 된다.
	//
	// GetServer를 쓰지 않는 이유: 그 함수는 이미 s.node_id를 읽는다(새 스키마 기준).
	// 옛 스키마에서는 "그런 컬럼이 없다"로 죽으므로, 컬럼 목록을 직접 본다.
	if hasColumn(t, st, "servers", "node_id") {
		t.Fatal("43번까지 적용했는데 서버에 node_id가 있습니다 — 시험이 옛 상태를 만들지 못했습니다")
	}

	if err := st.migrate(ctx); err != nil {
		t.Fatalf("migrate rest: %v", err)
	}
	if !hasColumn(t, st, "servers", "node_id") {
		t.Fatal("0044가 적용되지 않았습니다")
	}

	// 다수결(busan 2, seoul 1)로 서버에 올라가야 한다.
	after, err := st.GetServer(ctx, "srv-old")
	if err != nil {
		t.Fatalf("서버 조회: %v", err)
	}
	if after.NodeID != "busan" {
		t.Errorf("서버 담당 노드 = %q (기대 busan) — 가장 많이 지정된 노드가 올라가야 합니다",
			after.NodeID)
	}

	// 그리고 **실효** 담당 노드는 그대로여야 한다. 올리면서 예외를 잃으면 그 DB들이
	// 의도하지 않은 노드에서 실행되고, 그 사실은 접속 실패로만 드러난다.
	want := map[string]string{"da": "busan", "db": "busan", "dc": "seoul"}
	conns, err := st.ListConnectionsByServer(ctx, "srv-old")
	if err != nil {
		t.Fatalf("커넥션 목록: %v", err)
	}
	if len(conns) != len(want) {
		t.Fatalf("커넥션 %d개 (기대 %d개)", len(conns), len(want))
	}
	for _, c := range conns {
		if c.NodeID != want[c.Name] {
			t.Errorf("%s 실효 담당 노드 = %q (기대 %q)", c.Name, c.NodeID, want[c.Name])
		}
	}
}
