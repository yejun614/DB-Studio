package dbx

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"dbstudio/internal/model"
)

// 데이터 화면의 "모아 두었다가 한 번에 적용"이 지켜야 하는 두 가지를 고정한다.
//
//  1. 미리보기는 **아무것도 바꾸지 않는다.** 이것이 깨지면 확인 버튼이 곧 실행이 된다.
//  2. 적용은 **전부 되거나 전부 안 된다.** 절반만 반영되면 사용자는 무엇이 남았는지
//     알 수 없고, 화면이 보여주는 것과 DB가 다른 상태가 된다.
//
// SQLite로 시험하는 이유: 컨테이너가 필요 없어 일반 `go test`에 얹을 수 있다.
// 트랜잭션과 문장 조립은 어댑터가 종류와 무관하게 같은 경로를 쓴다.
func sqliteTarget(t *testing.T) Target {
	t.Helper()
	path := filepath.Join(t.TempDir(), "batch.db")

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sqlite 열기: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE items (
		id INTEGER PRIMARY KEY,
		name TEXT NOT NULL,
		note TEXT
	)`); err != nil {
		t.Fatalf("표 만들기: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO items (id, name, note) VALUES (1, 'a', NULL), (2, 'b', NULL)`); err != nil {
		t.Fatalf("초기 데이터: %v", err)
	}

	return Target{
		Conn:   &model.Connection{Kind: model.KindSQLite, DatabaseName: path, Options: model.Options{}},
		Secret: &model.Secret{},
	}
}

func rowCount(t *testing.T, target Target, where string) int {
	t.Helper()
	db, err := sql.Open("sqlite", target.Conn.DatabaseName)
	if err != nil {
		t.Fatalf("확인용 연결: %v", err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow("SELECT count(*) FROM items WHERE " + where).Scan(&n); err != nil {
		t.Fatalf("세기(%s): %v", where, err)
	}
	return n
}

func TestBatchDryRunChangesNothing(t *testing.T) {
	target := sqliteTarget(t)
	ref := TableRef{Name: "items"}
	ctx := context.Background()

	results, err := DoMutateRows(ctx, target, []RowMutation{
		{Table: ref, Action: "update", Values: map[string]any{"name": "고침"}, Key: map[string]any{"id": "1"}, DryRun: true},
		{Table: ref, Action: "delete", Key: map[string]any{"id": "2"}, DryRun: true},
	})
	if err != nil {
		t.Fatalf("미리보기: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("결과 수 = %d", len(results))
	}
	if !strings.HasPrefix(results[0].Statement, "UPDATE") || !strings.HasPrefix(results[1].Statement, "DELETE") {
		t.Errorf("문장이 예상과 다르다: %q / %q", results[0].Statement, results[1].Statement)
	}
	// 아직 실행하지 않았다는 표시. 0(변경된 행 없음)과 구분되어야 한다.
	if results[0].Affected != -1 {
		t.Errorf("미리보기 Affected = %d, want -1", results[0].Affected)
	}
	if n := rowCount(t, target, "name = '고침'"); n != 0 {
		t.Errorf("미리보기가 데이터를 바꿨다 (%d행)", n)
	}
	if n := rowCount(t, target, "id = 2"); n != 1 {
		t.Errorf("미리보기가 행을 지웠다")
	}
}

func TestBatchAppliesAll(t *testing.T) {
	target := sqliteTarget(t)
	ref := TableRef{Name: "items"}

	results, err := DoMutateRows(context.Background(), target, []RowMutation{
		{Table: ref, Action: "update", Values: map[string]any{"note": "메모"}, Key: map[string]any{"id": "1"}},
		{Table: ref, Action: "delete", Key: map[string]any{"id": "2"}},
		{Table: ref, Action: "insert", Values: map[string]any{"id": "3", "name": "c"}},
	})
	if err != nil {
		t.Fatalf("적용: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("결과 수 = %d", len(results))
	}
	if n := rowCount(t, target, "id = 1 AND note = '메모'"); n != 1 {
		t.Error("수정이 반영되지 않았다")
	}
	if n := rowCount(t, target, "id = 2"); n != 0 {
		t.Error("삭제가 반영되지 않았다")
	}
	if n := rowCount(t, target, "id = 3 AND name = 'c'"); n != 1 {
		t.Error("추가가 반영되지 않았다")
	}
}

// 하나가 실패하면 앞의 것도 남지 않아야 한다.
// 이 보장이 없으면 "적용하기"는 절반만 반영된 상태를 만들 수 있고,
// 그때 화면은 무엇이 적용됐는지 알려줄 방법이 없다.
func TestBatchRollsBackOnFailure(t *testing.T) {
	target := sqliteTarget(t)
	ref := TableRef{Name: "items"}

	_, err := DoMutateRows(context.Background(), target, []RowMutation{
		{Table: ref, Action: "update", Values: map[string]any{"note": "먼저"}, Key: map[string]any{"id": "1"}},
		// NOT NULL 위반 — 두 번째에서 실패한다.
		{Table: ref, Action: "update", Values: map[string]any{"name": nil}, Key: map[string]any{"id": "2"}},
	})
	if err == nil {
		t.Fatal("실패해야 하는 묶음이 성공했다")
	}
	if !strings.Contains(err.Error(), "2번째") {
		t.Errorf("몇 번째에서 멈췄는지 알려주지 않는다: %v", err)
	}
	if n := rowCount(t, target, "note = '먼저'"); n != 0 {
		t.Errorf("실패했는데 앞의 변경이 남았다 (%d행) — 트랜잭션이 아니다", n)
	}
}

// 미리보기를 지원하지 않는 어댑터에 DryRun이 새어 들어가면 그대로 실행된다.
// 그 경로를 막아 두었는지 확인한다.
func TestDryRunRejectedWhenUnsupported(t *testing.T) {
	target := Target{
		Conn:   &model.Connection{Kind: model.KindRedis, Host: "127.0.0.1", Port: 6379, Options: model.Options{}},
		Secret: &model.Secret{},
	}
	_, err := DoMutateRow(context.Background(), target, RowMutation{
		Table: TableRef{Name: "k"}, Action: "delete", Key: map[string]any{"key": "k"}, DryRun: true,
	})
	if err == nil {
		t.Fatal("미리보기를 지원하지 않는 종류에서 DryRun이 통과했다")
	}
	if !strings.Contains(err.Error(), "미리보기") {
		t.Errorf("거절 이유가 미리보기와 무관하다: %v", err)
	}
}
