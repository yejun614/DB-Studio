package backup

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dbstudio/internal/model"
)

// 백업 중계의 받는 쪽(마스터)을 시험한다.
//
// ── 방향이 두 번 뒤집혔다 (기록해 둔다) ────────────────────────────
// ① 처음: 노드가 자기 이름을 만들어 올린다 → 마스터가 그 이름을 검사해야 했다
//    (`../../etc/x`). 검사를 한 곳 더 만들면 빠뜨릴 수 있고, 빠뜨린 날이 디렉터리 탈출이다.
// ② 다음: "마스터가 모든 노드에 닿아야 하니 마스터가 받아 오면 안 된다"고 판단해
//    노드가 밀어 올리는 구조로 뒤집었다. **그 판단이 틀렸다** — 마스터→노드 HTTP 는
//    원래 잘 되고(클러스터 라우팅이 쓰는 그 길이다), 막히는 것은 **노드 뒤의 DB** 다.
//    그것이 담당 노드를 지정한 이유다.
// ③ 지금: 마스터가 이름을 정해 `POST /node/dump` 로 부탁하고, 노드가 응답 본문으로
//    덤프를 흘려보내고, 마스터가 받아 보관한다. 이름이 경계를 넘지 않으므로 경로 조작
//    검사를 새로 만들 필요가 없고, 노드에 파일도 남지 않는다.
//
// 그래서 여기서 보는 것은 셋이다: 이름을 마스터가 만드는가(확장자가 복구 방법을 정한다),
// 받은 파일이 안전한 곳에 놓이는가, 실패했을 때 잘린 파일이 남지 않는가.
//
// ── 왜 이 시험이 필요한가 ───────────────────────────────────────────
// 백업은 다른 읽기와 사정이 다르다. 파일이 남는 일이라 값 중계로는 해결되지 않는다.
// 그리고 실패했을 때 **어느 쪽에 파일이 남는가**가 안전을 결정한다:
//
//	올리기 실패 → 파일을 남긴다 (지우면 백업이 하나도 없는 상태가 된다)
//	올리기 성공 → 로컬 사본을 지운다 (남기면 진짜가 둘이 된다)
//
// 뒤집히면 조용히 백업을 잃거나, 조용히 디스크를 먹거나, 복구 시점에 "파일이 없다"를
// 알게 된다. 그래서 두 방향을 다 고정한다.

// newStreamTestService는 백업 디렉터리 하나를 가진 서비스를 만든다.
//
// 실제 디렉터리를 쓰는 이유: 이 시험들이 보는 것은 "파일이 어디에 놓이는가"이고,
// 그것은 진짜 파일 시스템에서만 확인할 수 있다(경로 조작도 그렇다).
func newStreamTestService(t *testing.T) (*Service, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "backups")
	s := New(nil, Config{Dir: dir}, slog.Default())
	if err := s.EnsureDir(); err != nil {
		t.Fatalf("ensure dir: %v", err)
	}
	return s, dir
}

// TestAllocateDumpNameKeepsFormat은 확장자를 보존하고 경로가 안전한지 본다.
//
// 확장자가 **복구기가 읽는 방법**을 정하므로 잃으면 안 된다. 그리고 이름을 마스터가
// 만들지만 그래도 FilePath를 지나는지 확인한다 — 모든 파일 경로가 그곳을 지난다는
// 성질이 다음 사람의 안전장치다.
func TestAllocateDumpNameKeepsFormat(t *testing.T) {
	s, dir := newStreamTestService(t)

	cases := []struct{ kind, wantExt string }{
		{string(model.KindMySQL), ".sql.gz"},
		{string(model.KindPostgres), ".sql.gz"},
		{string(model.KindMongoDB), ".jsonl.gz"},
		{string(model.KindRedis), ".redis.gz"},
	}
	for _, tc := range cases {
		name, err := s.AllocateDumpName(tc.kind)
		if err != nil {
			t.Errorf("%s: %v", tc.kind, err)
			continue
		}
		if !strings.HasSuffix(name, tc.wantExt) {
			t.Errorf("%s → %q (확장자 %s 를 기대)", tc.kind, name, tc.wantExt)
		}
		// 경로가 백업 디렉터리 안이어야 한다.
		full, err := s.FilePath(name)
		if err != nil {
			t.Errorf("%s → FilePath: %v", tc.kind, err)
			continue
		}
		if !strings.HasPrefix(full, dir) {
			t.Errorf("%s → 디렉터리 밖: %s", tc.kind, full)
		}
	}
}

// TestAllocateDumpNameRejectsUnknownKind는 모르는 종류를 거절하는지 본다.
//
// 이 값으로 파일을 만들고, 나중에 복구기가 그 값으로 읽는 방법을 정한다. 모르는 것을
// 받아 두면 "파일은 있는데 열 수 없다"가 복구 시점에 드러난다.
func TestAllocateDumpNameRejectsUnknownKind(t *testing.T) {
	s, _ := newStreamTestService(t)
	if _, err := s.AllocateDumpName("nosuchdb"); err == nil {
		t.Error("모르는 종류를 받아들였다 — 나중에 무엇으로 열지 아무도 모른다")
	}
}

// TestReceiveDumpUsesGivenName은 마스터가 준 이름 그대로 보관하는지 본다.
//
// 이름이 두 곳에서 각각 만들어지면 화면의 목록과 실제 파일이 어긋난다. 그 어긋남은
// **복구 시점에** 드러난다 — 가장 늦게 발견되는 실패다.
func TestReceiveDumpUsesGivenName(t *testing.T) {
	s, dir := newStreamTestService(t)
	want, err := s.AllocateDumpName(string(model.KindMySQL))
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	// 실제 덤프와 같은 모양(gzip)으로 보낸다 — 이 함수는 온전한 gzip 만 받는다.
	content := []byte("DUMP-FROM-NODE")
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(content); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	got, err := s.ReceiveDump(want, bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ReceiveDump: %v", err)
	}
	if got != want {
		t.Errorf("이름 = %q (기대 %q) — 기록과 파일이 어긋난다", got, want)
	}
	// 저장된 파일을 다시 풀어 내용이 그대로인지 본다(압축/해제 왕복이 온전한지).
	saved, err := os.ReadFile(filepath.Join(dir, want))
	if err != nil {
		t.Fatalf("저장된 파일을 읽지 못했다: %v", err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(saved))
	if err != nil {
		t.Fatalf("저장된 파일이 gzip 이 아니다: %v", err)
	}
	got2, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("저장된 파일을 풀지 못했다: %v", err)
	}
	if !bytes.Equal(got2, content) {
		t.Errorf("내용이 다르다: %q", got2)
	}
}

// TestReceiveDumpStaysInsideBackupDir은 경로가 섞인 이름도 디렉터리 밖으로 못 나가는지 본다.
//
// 이름은 마스터가 만들지만 이 함수는 그 규칙에 기대지 않는다. 규칙이 지켜지지 않는 날이
// 곧 디렉터리 탈출이 되는 날이고, 그날은 아무도 이 시험을 보지 않는다.
func TestReceiveDumpStaysInsideBackupDir(t *testing.T) {
	s, dir := newStreamTestService(t)

	for _, bad := range []string{
		"../../../etc/passwd.sql.gz",
		"..\\..\\windows\\evil.sql.gz",
		"/etc/shadow.sql.gz",
	} {
		name, err := s.ReceiveDump(bad, bytes.NewReader([]byte("x")))
		if err != nil {
			t.Logf("%q → 거절됨(%v)", bad, err)
			continue
		}
		full := filepath.Join(dir, name)
		if !strings.HasPrefix(full, dir+string(os.PathSeparator)) {
			t.Errorf("%q → 디렉터리 밖에 썼다: %s", bad, full)
		}
	}
}

// TestReceiveDumpRejectsTruncatedDump는 잘린 덤프를 **성공으로 기록하지 않는지** 본다.
//
// ── 이 시험이 실제 버그를 잡았다 ───────────────────────────────────
// 노드는 응답을 흘려보내기 시작한 뒤에 덤프가 실패하면 상태 코드를 바꿀 수 없다. 그래서
// 마스터는 "200 OK + 잘린 본문"을 받는다. gzip 검사가 없으면 그 잘린 파일이 **success 로
// 기록**되고, 쓸 수 없다는 사실은 복구가 필요할 때 드러난다 — 이 저장소가 가장 나쁘다고
// 부르는 실패다. 실제로 2노드 실측에서 0바이트 파일이 success 로 남는 것을 확인했다.
//
// 잘린 것을 "온전하다"로 통과시키면 이 시험이 실패한다.
func TestReceiveDumpRejectsTruncatedDump(t *testing.T) {
	s, dir := newStreamTestService(t)
	name, err := s.AllocateDumpName(string(model.KindMySQL))
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}

	// 온전한 gzip 을 만들어 앞부분만 자른다(전송이 끊긴 상황).
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte("CREATE TABLE orders (id INT);\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	full := buf.Bytes()
	truncated := full[:len(full)/2]

	if _, err := s.ReceiveDump(name, bytes.NewReader(truncated)); err == nil {
		t.Error("잘린 덤프를 받아들였다 — 복구 시점에 쓸 수 없다는 것을 알게 된다")
	}
	// 잘린 파일이 남아 있으면 목록에 있고 이름도 멀쩡하다. 그러면 언젠가 그것으로
	// 복구를 시도하게 된다.
	if _, err := os.Stat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
		t.Error("잘린 파일이 남았다 — 목록에서 멀쩡해 보인다")
	}
}

// TestReceiveDumpAcceptsIntactDump는 온전한 덤프는 통과시키는지 본다.
//
// 위 시험이 "무조건 거절"로 통과하지 않게 하는 짝이다. 이게 없으면 검사가 너무 엄격해져
// 정상 백업이 전부 실패해도 알 수 없다.
func TestReceiveDumpAcceptsIntactDump(t *testing.T) {
	s, _ := newStreamTestService(t)
	name, err := s.AllocateDumpName(string(model.KindMySQL))
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}

	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte("CREATE TABLE orders (id INT);\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	got, err := s.ReceiveDump(name, bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("온전한 덤프를 거절했다: %v", err)
	}
	if got != name {
		t.Errorf("이름 = %q (기대 %q)", got, name)
	}
}

// TestReceiveDumpRejectsNonGzip은 gzip 이 아닌 것을 거절하는지 본다.
//
// 이 파일은 나중에 복구기가 열어야 한다. 형식이 아니면 그 사실을 지금 알아야 한다.
func TestReceiveDumpRejectsNonGzip(t *testing.T) {
	s, _ := newStreamTestService(t)
	name, err := s.AllocateDumpName(string(model.KindMySQL))
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if _, err := s.ReceiveDump(name, bytes.NewReader([]byte("not gzip at all"))); err == nil {
		t.Error("gzip 이 아닌 것을 받아들였다 — 복구기가 열 수 없다")
	}
}

// TestReceiveDumpLeavesNoPartialFile은 중간에 끊긴 전송이 파일을 남기지 않는지 본다.
//
// 잘린 덤프가 목록에 있으면 언젠가 그것으로 복구를 시도하게 되고, 그때는 이미 늦다.
// 임시 이름에 받고 완성된 뒤에 옮기는 이유가 그것이다.
func TestReceiveDumpLeavesNoPartialFile(t *testing.T) {
	s, dir := newStreamTestService(t)
	name, err := s.AllocateDumpName(string(model.KindMySQL))
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}

	if _, err := s.ReceiveDump(name, &failingReader{after: 4}); err == nil {
		t.Fatal("끊긴 전송이 성공으로 처리됐다")
	}
	entries, rerr := os.ReadDir(dir)
	if rerr != nil {
		t.Fatalf("readdir: %v", rerr)
	}
	for _, e := range entries {
		t.Errorf("실패했는데 파일이 남았다: %s", e.Name())
	}
}

// failingReader는 몇 바이트 뒤에 끊기는 리더다.
type failingReader struct{ after int }

func (r *failingReader) Read(p []byte) (int, error) {
	if r.after <= 0 {
		return 0, errors.New("연결이 끊겼습니다")
	}
	n := len(p)
	if n > r.after {
		n = r.after
	}
	for i := 0; i < n; i++ {
		p[i] = 'x'
	}
	r.after -= n
	return n, nil
}
