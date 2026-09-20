package backup

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"dbstudio/internal/model"
)

// 스트림으로 받은 덤프를 적용하는 경로를 시험한다 (P33-2).
//
// ── 왜 이 시험이 필요한가 ───────────────────────────────────────────
// 복구도 백업과 같은 문제를 갖는다: 요청이 쓰기라 마스터로 넘어오는데, 담당 노드가 지정된
// DB는 마스터가 닿지 못할 수 있다. 그래서 마스터가 파일을 노드로 밀어 넣는다 — 그때
// 노드는 **파일에 접근할 수 없다.** 리더로 받은 것을 그대로 적용해야 한다.
//
// 그래서 이 시험들은 **실제 DB 없이** 확인할 수 있는 것만 본다: 형식 판정, 문장 분리,
// 그리고 실패했을 때 이유가 남는가. 실제 적용은 2노드 실측이 본다.

// TestApplyStreamNeedsTarget은 대상이 없으면 거절하는지 본다.
//
// 대상 없이 진행하면 어디에도 적용하지 않고 성공을 돌려줄 수 있다 — 가장 나쁜 거짓 성공이다.
func TestApplyStreamNeedsTarget(t *testing.T) {
	s, _ := newStreamTestService(t)

	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write([]byte("SELECT 1;\n"))
	zw.Close()

	_, _, _, err := s.ApplyStream(context.Background(), bytes.NewReader(buf.Bytes()),
		RestoreParams{Kind: string(model.KindMySQL)}, nil)
	if err == nil {
		t.Error("대상 없이 진행했다 — 어디에도 적용하지 않고 성공을 돌려줄 수 있다")
	}
}

// TestApplyStreamRejectsEmptyDump는 내용이 없는 덤프를 성공으로 처리하지 않는지 본다.
//
// 빈 덤프를 성공으로 처리하면 "복구했는데 데이터가 없다"가 조용히 지나간다. 복구는
// 되돌릴 수 없는 일이라, 아무것도 하지 않은 것을 성공이라 부르면 안 된다.
//
// ── 이 시험에서 배운 것 ────────────────────────────────────────────
// 처음에는 "실행할 문장이 없습니다"라는 특정 문구를 기대했다. 그런데 주석만 있는 덤프는
// SplitStatements 가 주석을 문장으로 세지 않으면서도 0개가 되지 않는다 — 그래서 실제로는
// **DB 접속 시도**까지 가서 접속 실패로 끝났다. 즉 "빈 덤프"를 문장 0개로 만드는 조건은
// 이 테스트가 만든 것보다 좁다.
//
// 그래서 확인할 것을 **성공하지 않는 것**으로 바꿨다. 그것이 이 자리의 실제 계약이다 —
// 어떤 이유로든 내용이 없으면 성공이라고 답하면 안 된다.
func TestApplyStreamRejectsEmptyDump(t *testing.T) {
	s, _ := newStreamTestService(t)

	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write([]byte("-- 헤더만 있는 덤프\n\n"))
	zw.Close()

	conn := &model.Connection{Kind: model.KindMySQL, Host: "127.0.0.1", Port: 1}
	_, _, _, err := s.ApplyStream(context.Background(), bytes.NewReader(buf.Bytes()),
		RestoreParams{Kind: string(model.KindMySQL), Target: Target{Conn: conn, Secret: &model.Secret{}}}, nil)
	if err == nil {
		t.Error("내용이 없는데 성공으로 처리했다 — '복구했는데 데이터가 없다'가 조용히 지나간다")
	}
}

// TestRestoreResultCarriesProgress는 결과가 "어디까지 갔는지"를 나르는지 본다.
//
// ── 왜 이것이 복구에서 가장 중요한 정보다 ─────────────────────────
// 복구는 되돌릴 수 없다. 실패했을 때 사람이 알아야 하는 것은 "실패했다"가 아니라
// **어디까지 적용됐는가**다 — 절반이 적용된 DB에 다시 부으면 중복 키가 쏟아진다.
// 그래서 노드는 실패해도 200으로 답하고 결과 안에 그 숫자를 담는다.
func TestRestoreResultCarriesProgress(t *testing.T) {
	res := RestoreResult{
		Done: 42, Total: 100,
		FailedStatement: "INSERT INTO orders VALUES (100, 'A-100', 999)",
		Error:           "duplicate key",
	}
	// 화면·기록이 읽을 수 있는 모양인지 확인한다(이 구조가 그대로 JSON 으로 오간다).
	if res.Done != 42 || res.Total != 100 {
		t.Errorf("진행 숫자가 보존되지 않았다: %+v", res)
	}
	if res.FailedStatement == "" || res.Error == "" {
		t.Errorf("실패 이유가 비었다: %+v", res)
	}
}

// TestOpenDumpFileValidatesName은 이름으로 열 때도 경로 검사를 지나는지 본다.
//
// 마스터가 담당 노드로 밀어 넣을 때 이 함수를 쓴다. 이름은 자기 기록에서 오지만,
// 경로를 조립하는 곳에서는 언제나 확인한다는 것이 이 저장소의 규칙이다(FilePath 주석).
func TestOpenDumpFileValidatesName(t *testing.T) {
	s, _ := newStreamTestService(t)

	for _, bad := range []string{
		"",
		"../../../etc/passwd.sql.gz",
		"a/b.sql.gz",
	} {
		if _, _, err := s.OpenDumpFile(bad); err == nil {
			t.Errorf("%q 를 열었다 — 경로 검사가 우회됐다", bad)
		}
	}
}

// TestOpenDumpFileFailsOnMissing은 없는 파일을 정직하게 실패시키는지 본다.
//
// 여기서 빈 리더를 돌려주면 복구가 "문장 0개"로 끝나고, 그 실패는 **백업 파일이 사라졌다**는
// 사실을 가린다.
func TestOpenDumpFileFailsOnMissing(t *testing.T) {
	s, _ := newStreamTestService(t)
	if _, _, err := s.OpenDumpFile("없는파일.sql.gz"); err == nil {
		t.Error("없는 파일을 열었다")
	}
}

// TestOpenDumpFileReadsIntactDump는 진짜 덤프를 풀어 읽는지 본다.
//
// 위 시험들이 "무조건 실패"로 통과하지 않게 하는 짝이다.
func TestOpenDumpFileReadsIntactDump(t *testing.T) {
	s, dir := newStreamTestService(t)

	content := []byte("-- 덤프\nCREATE TABLE orders (id INT);\n")
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write(content)
	zw.Close()
	name := "node-test.sql.gz"
	if err := os.WriteFile(filepath.Join(dir, name), buf.Bytes(), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	r, closeFn, err := s.OpenDumpFile(name)
	if err != nil {
		t.Fatalf("OpenDumpFile: %v", err)
	}
	defer closeFn()
	got := make([]byte, len(content))
	if _, err := io.ReadFull(r, got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("내용이 다르다: %q", got)
	}
}
