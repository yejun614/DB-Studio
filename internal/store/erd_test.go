package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"dbstudio/internal/crypto"
	"dbstudio/internal/erd"
	"dbstudio/internal/model"
)

// erdFixture는 메타 DB와 커넥션 하나, 빈 ERD 문서를 준비한다.
func erdFixture(t *testing.T) (context.Context, *Store, string) {
	t.Helper()
	ctx := context.Background()
	box, err := crypto.NewSecretBox(make([]byte, 32))
	if err != nil {
		t.Fatalf("secret box: %v", err)
	}
	st, err := Open(ctx, filepath.Join(t.TempDir(), "erd.db"), box)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	pj, err := st.CreateProject(ctx, SaveProjectParams{Name: "테스트 프로젝트"})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	pw := "pw"
	_, conn, err := st.CreateServerWithDatabase(ctx,
		SaveServerParams{
			Name: "c", Kind: model.KindPostgres, DefaultEnvironment: model.EnvDev,
			Host: "h", Port: 1, Options: model.Options{}, Tags: []string{},
			Enabled: true, Password: &pw,
		},
		SaveConnectionParams{
			ProjectID: pj.ID,
			Name:      "c", Environment: model.EnvDev, DatabaseName: "d",
			Tags: []string{}, Enabled: true,
		})
	if err != nil {
		t.Fatalf("create connection: %v", err)
	}
	return ctx, st, conn.ID
}

func mkOp(id string, kind erd.Kind, payload string) *erd.Op {
	return &erd.Op{ID: id, Kind: kind, Payload: json.RawMessage(payload)}
}

// 저장된 스냅샷 + op-log를 재생하면 원래 상태가 그대로 복원되어야 한다.
// 이것이 깨지면 새로고침만으로 문서가 달라진다.
func TestERDDocumentReplay(t *testing.T) {
	ctx, st, connID := erdFixture(t)

	doc := erd.NewDocument("doc1", "초안", connID, "postgres")
	if err := st.CreateERDDocument(ctx, doc, "", "메모", nil); err != nil {
		t.Fatalf("create: %v", err)
	}

	ops := []*erd.Op{
		mkOp("o1", erd.OpTableAdd, `{"name":"users","withId":true}`),
		mkOp("o2", erd.OpColumnAdd, `{"table":"users","name":"email","type":"varchar(255)","nullable":false}`),
		mkOp("o3", erd.OpIndexAdd, `{"table":"users","name":"ux_email","columns":["email"],"unique":true}`),
		mkOp("o4", erd.OpTableMove, `{"key":"users","x":123,"y":456}`),
		mkOp("o5", erd.OpNoteAdd, `{"id":"n1","text":"검토 필요","x":10,"y":10}`),
	}
	for _, op := range ops {
		if err := erd.Apply(doc, op); err != nil {
			t.Fatalf("apply %s: %v", op.Kind, err)
		}
		if err := st.AppendERDOp(ctx, doc, op); err != nil {
			t.Fatalf("append %s: %v", op.Kind, err)
		}
	}

	loaded, err := st.GetERDDocument(ctx, "doc1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if loaded.Seq != 5 {
		t.Errorf("seq = %d, 기대값 5", loaded.Seq)
	}
	if got, want := loaded.Schema.Fingerprint(), doc.Schema.Fingerprint(); got != want {
		t.Errorf("재생된 스키마가 다릅니다:\n  저장 전 %s\n  재생 후 %s", want, got)
	}
	if box := loaded.Layout["users"]; box == nil || box.X != 123 || box.Y != 456 {
		t.Errorf("레이아웃이 복원되지 않았습니다: %+v", loaded.Layout)
	}
	if len(loaded.Notes) != 1 || loaded.Notes[0].Text != "검토 필요" {
		t.Errorf("메모가 복원되지 않았습니다: %+v", loaded.Notes)
	}
	if loaded.Dialect != "postgres" || loaded.ConnectionID != connID {
		t.Errorf("메타데이터가 어긋났습니다: %+v", loaded)
	}
}

// seq는 문서별로 1부터 빈틈없이 증가해야 한다. 재생 순서의 유일성이 여기에 달려 있다.
func TestERDOpSeqIsMonotonic(t *testing.T) {
	ctx, st, connID := erdFixture(t)
	docs := map[string]*erd.Document{}
	for _, id := range []string{"d1", "d2"} {
		doc := erd.NewDocument(id, id, connID, "postgres")
		if err := st.CreateERDDocument(ctx, doc, "", "", nil); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
		docs[id] = doc
	}
	for i := 1; i <= 3; i++ {
		for _, id := range []string{"d1", "d2"} {
			op := mkOp(id+"-op"+string(rune('0'+i)), erd.OpNoteAdd,
				`{"id":"n`+string(rune('0'+i))+`","text":"x"}`)
			if err := st.AppendERDOp(ctx, docs[id], op); err != nil {
				t.Fatalf("append: %v", err)
			}
			if op.Seq != int64(i) {
				t.Errorf("%s 의 %d번째 op seq = %d", id, i, op.Seq)
			}
			// 문서의 seq도 함께 올라가야 한다. 이것을 호출자가 하도록 두면
			// 잊었을 때의 실패(압축이 조용히 건너뛰어짐)를 찾기 어렵다.
			if docs[id].Seq != op.Seq {
				t.Errorf("%s 문서 seq = %d, op seq = %d", id, docs[id].Seq, op.Seq)
			}
		}
	}
}

// 재접속한 클라이언트가 확인받지 못한 op를 다시 보내면 두 번 적용되어서는 안 된다.
// 두 번 적용되면 컬럼이 둘 생긴다.
func TestERDOpResendIsRejected(t *testing.T) {
	ctx, st, connID := erdFixture(t)
	doc := erd.NewDocument("d1", "d", connID, "postgres")
	if err := st.CreateERDDocument(ctx, doc, "", "", nil); err != nil {
		t.Fatalf("create: %v", err)
	}
	op := mkOp("same-id", erd.OpTableAdd, `{"name":"users"}`)
	if err := st.AppendERDOp(ctx, doc, op); err != nil {
		t.Fatalf("first append: %v", err)
	}
	err := st.AppendERDOp(ctx, doc, mkOp("same-id", erd.OpTableAdd, `{"name":"users"}`))
	if !errors.Is(err, ErrOpConflict) {
		t.Fatalf("재전송된 op 오류 = %v, 기대값 ErrOpConflict", err)
	}
	if doc.Seq != 1 {
		t.Errorf("거부된 재전송이 문서 seq를 올렸습니다: %d", doc.Seq)
	}
	// seq가 헛되게 올라가서는 안 된다.
	loaded, err := st.GetERDDocument(ctx, "d1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if loaded.Seq != 1 {
		t.Errorf("거부된 재전송 후 seq = %d, 기대값 1", loaded.Seq)
	}
}

// 압축은 상태를 바꾸지 않으면서 재생해야 할 op만 줄인다.
func TestERDCompaction(t *testing.T) {
	ctx, st, connID := erdFixture(t)
	doc := erd.NewDocument("d1", "d", connID, "postgres")
	if err := st.CreateERDDocument(ctx, doc, "", "", nil); err != nil {
		t.Fatalf("create: %v", err)
	}
	for i := 0; i < 12; i++ {
		name := "t" + string(rune('a'+i))
		op := mkOp("op"+name, erd.OpTableAdd, `{"name":"`+name+`"}`)
		if err := erd.Apply(doc, op); err != nil {
			t.Fatalf("apply: %v", err)
		}
		if err := st.AppendERDOp(ctx, doc, op); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	before := doc.Schema.Fingerprint()

	compacted, err := st.CompactERDDocument(ctx, doc, 2)
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if !compacted {
		t.Fatal("압축이 건너뛰어졌습니다")
	}

	// 남은 op는 최근 2개뿐이어야 한다.
	remaining, err := st.ListERDOps(ctx, "d1", 0)
	if err != nil {
		t.Fatalf("list ops: %v", err)
	}
	if len(remaining) != 2 {
		t.Errorf("압축 후 남은 op 수 = %d, 기대값 2", len(remaining))
	}

	loaded, err := st.GetERDDocument(ctx, "d1")
	if err != nil {
		t.Fatalf("get after compact: %v", err)
	}
	if got := loaded.Schema.Fingerprint(); got != before {
		t.Errorf("압축이 상태를 바꿨습니다:\n  전 %s\n  후 %s", before, got)
	}
	if loaded.Seq != 12 {
		t.Errorf("압축 후 seq = %d, 기대값 12", loaded.Seq)
	}
}

// 압축하려는 시점과 문서의 현재 seq가 다르면(그 사이 새 op가 들어왔으면)
// 스냅샷이 그 op를 포함하는지 알 수 없으므로 건너뛰어야 한다.
func TestERDCompactionSkipsWhenStale(t *testing.T) {
	ctx, st, connID := erdFixture(t)
	doc := erd.NewDocument("d1", "d", connID, "postgres")
	if err := st.CreateERDDocument(ctx, doc, "", "", nil); err != nil {
		t.Fatalf("create: %v", err)
	}
	op := mkOp("o1", erd.OpTableAdd, `{"name":"users"}`)
	if err := erd.Apply(doc, op); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if err := st.AppendERDOp(ctx, doc, op); err != nil {
		t.Fatalf("append: %v", err)
	}

	// 다른 참여자의 op가 들어온 상황을 만든다 (문서 seq는 2, 우리 doc.Seq는 1).
	// 그 사람은 자기 문서 사본을 들고 있으므로 여기서도 별도 사본으로 append한다.
	otherView := doc.Clone()
	other := mkOp("o2", erd.OpTableAdd, `{"name":"orders"}`)
	if err := st.AppendERDOp(ctx, otherView, other); err != nil {
		t.Fatalf("append other: %v", err)
	}

	compacted, err := st.CompactERDDocument(ctx, doc, 0)
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if compacted {
		t.Error("뒤처진 상태로 압축이 실행되었습니다")
	}
	// 압축을 건너뛰었으므로 두 op가 모두 남아 있고, 로딩 결과에 orders가 있어야 한다.
	loaded, err := st.GetERDDocument(ctx, "d1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if loaded.Seq != 2 {
		t.Errorf("seq = %d, 기대값 2", loaded.Seq)
	}
	names := []string{}
	for _, tbl := range loaded.Schema.Tables {
		names = append(names, tbl.Name)
	}
	if got := strings.Join(names, ","); got != "orders,users" {
		t.Errorf("테이블 = %s (뒤늦은 압축이 op를 잃었습니다)", got)
	}
}

func TestERDDocumentList(t *testing.T) {
	ctx, st, connID := erdFixture(t)
	doc := erd.NewDocument("d1", "첫 초안", connID, "postgres")
	if err := erd.Apply(doc, mkOp("o1", erd.OpTableAdd, `{"name":"users"}`)); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if err := erd.Apply(doc, mkOp("o2", erd.OpTableAdd, `{"name":"orders"}`)); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if err := st.CreateERDDocument(ctx, doc, "", "설명", nil); err != nil {
		t.Fatalf("create: %v", err)
	}

	list, err := st.ListERDDocuments(ctx, nil, nil, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("문서 수 = %d", len(list))
	}
	if list[0].TableCount != 2 {
		t.Errorf("테이블 수 = %d, 기대값 2", list[0].TableCount)
	}
	if list[0].Name != "첫 초안" || list[0].Status != erd.StatusDraft {
		t.Errorf("메타 = %+v", list[0])
	}

	// 접근 가능한 커넥션으로 필터하면 다른 커넥션의 문서는 보이지 않아야 한다.
	other, err := st.ListERDDocuments(ctx, []string{"other-conn"}, nil, 0)
	if err != nil {
		t.Fatalf("list filtered: %v", err)
	}
	if len(other) != 0 {
		t.Errorf("접근 범위 밖 문서가 %d건 노출되었습니다", len(other))
	}

	// 빈 슬라이스는 "접근 가능한 커넥션이 없음"이다. 이것을 nil(제한 없음)과 같이
	// 취급하면 권한 없는 사용자에게 모든 문서가 노출된다.
	none, err := st.ListERDDocuments(ctx, []string{}, nil, 0)
	if err != nil {
		t.Fatalf("list with empty scope: %v", err)
	}
	if len(none) != 0 {
		t.Errorf("빈 접근 범위에서 문서 %d건이 노출되었습니다 (권한 누출)", len(none))
	}

	// nil은 제한 없음이다. 관리 목적의 전체 조회에 쓴다.
	all, err := st.ListERDDocuments(ctx, nil, nil, 0)
	if err != nil {
		t.Fatalf("list unrestricted: %v", err)
	}
	if len(all) != 1 {
		t.Errorf("nil 범위 조회 결과 = %d건, 기대값 1건", len(all))
	}
}

// 커넥션을 지우면 그 커넥션의 초안도 함께 사라져야 한다.
// 대상이 없는 초안은 dialect도 권한도 판정할 수 없다.
func TestERDDocumentCascadesWithConnection(t *testing.T) {
	ctx, st, connID := erdFixture(t)
	doc := erd.NewDocument("d1", "d", connID, "postgres")
	if err := st.CreateERDDocument(ctx, doc, "", "", nil); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := st.DeleteConnection(ctx, connID); err != nil {
		t.Fatalf("delete connection: %v", err)
	}
	if _, err := st.GetERDDocument(ctx, "d1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("커넥션 삭제 후 문서 조회 = %v, 기대값 ErrNotFound", err)
	}
}

func TestERDChat(t *testing.T) {
	ctx, st, connID := erdFixture(t)
	doc := erd.NewDocument("d1", "d", connID, "postgres")
	if err := st.CreateERDDocument(ctx, doc, "", "", nil); err != nil {
		t.Fatalf("create: %v", err)
	}
	for _, body := range []string{"첫 메시지", "두 번째", "세 번째"} {
		m := &ERDChatMessage{DocID: "d1", UserName: "홍길동", Body: body}
		if err := st.AddERDChatMessage(ctx, m); err != nil {
			t.Fatalf("add chat: %v", err)
		}
		if m.ID == 0 || m.CreatedAt.IsZero() {
			t.Errorf("저장 후 메시지에 ID/시각이 없습니다: %+v", m)
		}
	}
	msgs, err := st.ListERDChatMessages(ctx, "d1", 0)
	if err != nil {
		t.Fatalf("list chat: %v", err)
	}
	// 화면은 위에서 아래로 시간순으로 읽는다.
	if len(msgs) != 3 || msgs[0].Body != "첫 메시지" || msgs[2].Body != "세 번째" {
		t.Fatalf("메시지 순서가 잘못되었습니다: %+v", msgs)
	}

	// limit은 최신 것을 남겨야 한다 — 오래된 대화를 보여주고 최근 것을 버리면 쓸모없다.
	recent, err := st.ListERDChatMessages(ctx, "d1", 2)
	if err != nil {
		t.Fatalf("list recent: %v", err)
	}
	if len(recent) != 2 || recent[0].Body != "두 번째" || recent[1].Body != "세 번째" {
		t.Errorf("최근 메시지 = %+v", recent)
	}
}

// 그룹·아이콘은 op 로그에만 있으면 압축되는 순간 사라진다.
// 사용자는 자기가 그린 것이 왜 없어졌는지 알 수 없으므로 스냅샷에 함께 담아야 한다.
func TestERDCompactionKeepsGroupsAndIcons(t *testing.T) {
	ctx, st, connID := erdFixture(t)
	doc := erd.NewDocument("d1", "d", connID, "postgres")
	if err := st.CreateERDDocument(ctx, doc, "", "", nil); err != nil {
		t.Fatalf("create: %v", err)
	}

	ops := []struct {
		id      string
		kind    erd.Kind
		payload string
	}{
		{"o1", erd.OpTableAdd, `{"name":"users"}`},
		{"o2", erd.OpTableMove, `{"key":"users","x":10,"y":20,"icon":"users","color":"#22c55e"}`},
		{"o3", erd.OpGroupAdd, `{"id":"g1","label":"사용자 도메인","x":0,"y":0,"w":300,"h":220,"color":"#3b82f6"}`},
		{"o4", erd.OpNoteAdd, `{"id":"n1","text":"메모"}`},
	}
	for _, o := range ops {
		op := mkOp(o.id, o.kind, o.payload)
		if err := erd.Apply(doc, op); err != nil {
			t.Fatalf("apply %s: %v", o.kind, err)
		}
		if err := st.AppendERDOp(ctx, doc, op); err != nil {
			t.Fatalf("append %s: %v", o.kind, err)
		}
	}

	// keepOps=0 이면 op 로그가 통째로 잘린다. 스냅샷만 남는 상황이다.
	if _, err := st.CompactERDDocument(ctx, doc, 0); err != nil {
		t.Fatalf("compact: %v", err)
	}

	loaded, err := st.GetERDDocument(ctx, "d1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(loaded.Groups) != 1 || loaded.Groups[0].Label != "사용자 도메인" {
		t.Errorf("그룹이 사라졌습니다: %+v", loaded.Groups)
	}
	if loaded.Groups[0].W != 300 || loaded.Groups[0].Color != "#3b82f6" {
		t.Errorf("그룹 속성이 보존되지 않았습니다: %+v", loaded.Groups[0])
	}
	if box := loaded.Layout["users"]; box == nil || box.Icon != "users" || box.Color != "#22c55e" {
		t.Errorf("아이콘/색이 보존되지 않았습니다: %+v", box)
	}
	if len(loaded.Notes) != 1 {
		t.Errorf("메모가 사라졌습니다: %+v", loaded.Notes)
	}
}

// 0021 마이그레이션은 구조를 바꾸려고 erd_documents를 새로 만들어 옮긴다.
// 외래키 검사가 켜진 채로 옛 테이블을 지우면 op 로그와 대화가 CASCADE로 함께
// 사라진다 — 그 사고가 다시 일어나지 않도록 실제 데이터를 넣고 확인한다.
func TestStandaloneMigrationKeepsOpsAndChat(t *testing.T) {
	ctx := context.Background()
	box, err := crypto.NewSecretBox(make([]byte, 32))
	if err != nil {
		t.Fatalf("secret box: %v", err)
	}
	path := filepath.Join(t.TempDir(), "mig21.db")
	db, err := sql.Open("sqlite", strings.ReplaceAll(path, "\\", "/")+
		"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	st := &Store{db: db, secret: box}
	t.Cleanup(func() { st.Close() })
	if err := st.migrateTo(ctx, 20); err != nil {
		t.Fatalf("migrate to 20: %v", err)
	}

	now := nowString()
	if _, err := db.ExecContext(ctx, `INSERT INTO servers
		(id, name, name_lower, kind, host, port, options, default_environment,
		 tags, note, enabled, created_at, updated_at)
		VALUES ('s1','pg','pg','postgres','h',5432,'{}','dev','','',1,?,?)`,
		now, now); err != nil {
		t.Fatalf("insert server: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO connections
		(id, name, name_lower, environment, database_name, tags, note, enabled,
		 server_id, created_at, updated_at)
		VALUES ('c1','pg','pg','dev','d','','',1,'s1',?,?)`,
		now, now); err != nil {
		t.Fatalf("insert connection: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO erd_documents
		(id, name, connection_id, dialect, status, snapshot_json, layout_json, notes_json,
		 groups_json, snapshot_seq, seq, note, created_at, updated_at)
		VALUES ('d1','초안','c1','postgres','draft','{"tables":[]}','{}','[]','[]',0,1,'',?,?)`,
		now, now); err != nil {
		t.Fatalf("insert document: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO erd_ops
		(doc_id, seq, op_id, kind, payload, actor_name, base_seq, created_at)
		VALUES ('d1',1,'op-1','table.add','{"name":"users"}','나',0,?)`, now); err != nil {
		t.Fatalf("insert op: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO erd_chat_messages
		(doc_id, user_name, body, kind, target_key, created_at)
		VALUES ('d1','나','왜 이렇게 했는지','message','',?)`, now); err != nil {
		t.Fatalf("insert chat: %v", err)
	}

	if err := st.migrate(ctx); err != nil {
		t.Fatalf("migrate rest: %v", err)
	}

	var ops, chats, docs int
	if err := db.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM erd_ops),
		(SELECT COUNT(*) FROM erd_chat_messages),
		(SELECT COUNT(*) FROM erd_documents)`).Scan(&ops, &chats, &docs); err != nil {
		t.Fatalf("count: %v", err)
	}
	if ops != 1 || chats != 1 || docs != 1 {
		t.Fatalf("마이그레이션이 데이터를 잃었습니다: ops=%d chats=%d docs=%d", ops, chats, docs)
	}

	// 이제 커넥션 없는 초안을 넣을 수 있어야 한다.
	if _, err := db.ExecContext(ctx, `INSERT INTO erd_documents
		(id, name, connection_id, dialect, status, snapshot_json, layout_json, notes_json,
		 groups_json, snapshot_seq, seq, note, created_at, updated_at)
		VALUES ('d2','독립 초안',NULL,'mysql','draft','{"tables":[]}','{}','[]','[]',0,0,'',?,?)`,
		now, now); err != nil {
		t.Fatalf("독립 초안을 넣지 못했습니다: %v", err)
	}

	// 커넥션을 지우면 그 커넥션의 초안만 사라지고 독립 초안은 남아야 한다.
	if _, err := db.ExecContext(ctx, `DELETE FROM connections WHERE id = 'c1'`); err != nil {
		t.Fatalf("delete connection: %v", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM erd_documents`).Scan(&docs); err != nil {
		t.Fatalf("count after delete: %v", err)
	}
	if docs != 1 {
		t.Fatalf("커넥션 삭제 후 남은 문서 수가 %d 입니다 (독립 초안 1개여야 합니다)", docs)
	}
}

// 도메인은 문서의 일부다. 저장·복원에서 빠지면 새로고침만으로 설계의 어휘가 사라진다.
func TestERDDomainsSurviveReload(t *testing.T) {
	ctx, st, connID := erdFixture(t)

	doc := erd.NewDocument("doc-dom", "초안", connID, "postgres")
	if err := st.CreateERDDocument(ctx, doc, "", "", nil); err != nil {
		t.Fatalf("create: %v", err)
	}
	ops := []*erd.Op{
		mkOp("o1", erd.OpDomainAdd, `{"name":"email","type":"varchar(320)","comment":"메일"}`),
		mkOp("o2", erd.OpTableAdd, `{"name":"users","withId":true}`),
		mkOp("o3", erd.OpColumnAdd, `{"table":"users","name":"login","domain":"email"}`),
	}
	for _, op := range ops {
		if err := erd.Apply(doc, op); err != nil {
			t.Fatalf("apply %s: %v", op.Kind, err)
		}
		if err := st.AppendERDOp(ctx, doc, op); err != nil {
			t.Fatalf("append %s: %v", op.Kind, err)
		}
	}

	// op 재생 경로
	got, err := st.GetERDDocument(ctx, "doc-dom")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.Domains) != 1 || got.Domains[0].Type != "varchar(320)" {
		t.Fatalf("도메인 = %+v", got.Domains)
	}
	if col := got.Schema.Tables[0].Column("login"); col == nil || col.Domain != "email" {
		t.Errorf("컬럼의 도메인 연결이 사라졌습니다: %+v", col)
	}

	// 압축(스냅샷) 경로. 여기서 빠지면 op가 정리된 뒤에야 도메인이 사라진다 —
	// 한참 뒤에 드러나는 만큼 더 나쁜 실패다.
	if ok, err := st.CompactERDDocument(ctx, got, 0); err != nil || !ok {
		t.Fatalf("compact: %v (ok=%t)", err, ok)
	}
	again, err := st.GetERDDocument(ctx, "doc-dom")
	if err != nil {
		t.Fatalf("get after compact: %v", err)
	}
	if len(again.Domains) != 1 || again.Domains[0].Name != "email" {
		t.Errorf("압축 뒤 도메인 = %+v", again.Domains)
	}
}
