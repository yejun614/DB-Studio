package backup

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"dbstudio/internal/dbx"
	"dbstudio/internal/model"
)

func TestValidScope(t *testing.T) {
	for _, s := range []string{ScopeFull, ScopeSchema, ScopeData} {
		if !ValidScope(s) {
			t.Errorf("%s 는 유효해야 한다", s)
		}
	}
	if ValidScope("everything") {
		t.Error("모르는 범위는 거부해야 한다")
	}
}

func TestFormatForKind(t *testing.T) {
	if got := FormatFor(model.KindMongoDB); got != FormatJSONL {
		t.Errorf("MongoDB = %s", got)
	}
	if got := FormatFor(model.KindRedis); got != FormatRedis {
		t.Errorf("Redis = %s", got)
	}
	for _, kind := range []model.DBKind{
		model.KindMySQL, model.KindPostgres, model.KindMSSQL, model.KindOracle, model.KindSQLite,
	} {
		if got := FormatFor(kind); got != FormatSQL {
			t.Errorf("%s = %s", kind, got)
		}
	}
}

// 파일 이름은 앱이 만들지만, 경로를 조립하는 곳에서는 언제나 확인한다.
// 그 규칙이 지켜지지 않는 날이 오면 그때가 디렉터리 탈출이 되는 날이다.
func TestFilePathRejectsTraversal(t *testing.T) {
	s := New(nil, Config{Dir: filepath.Join(t.TempDir(), "backups")}, slog.Default())

	for _, bad := range []string{
		"", "../secret", "..\\secret", "sub/dir.sql", `sub\dir.sql`, "/etc/passwd",
	} {
		if _, err := s.FilePath(bad); err == nil {
			t.Errorf("%q 는 거부해야 한다", bad)
		}
	}
	good := "6f1c-4a2e.sql.gz"
	path, err := s.FilePath(good)
	if err != nil {
		t.Fatalf("정상 이름이 거부되었다: %v", err)
	}
	if filepath.Base(path) != good {
		t.Errorf("경로 = %q", path)
	}
}

func TestMatchesTables(t *testing.T) {
	ref := dbx.TableRef{Namespace: "public", Name: "orders"}

	// 비어 있으면 전부.
	if !matchesTables(ref, nil) {
		t.Error("목록이 비면 모두 통과해야 한다")
	}
	// 전체 이름과 짧은 이름 둘 다 받는다. 사용자는 스키마를 붙이기도 하고 안 붙이기도 한다.
	if !matchesTables(ref, []string{"public.orders"}) {
		t.Error("전체 이름이 맞아야 한다")
	}
	if !matchesTables(ref, []string{"orders"}) {
		t.Error("짧은 이름도 맞아야 한다")
	}
	if matchesTables(ref, []string{"users"}) {
		t.Error("다른 이름은 걸러야 한다")
	}
	// 접두사가 같다고 통과하면 안 된다.
	if matchesTables(ref, []string{"order"}) {
		t.Error("부분 일치로 통과하면 안 된다")
	}
}

// 취소는 실패가 아니다. 사용자가 의도한 결과이므로 실패 목록에 섞이면
// "백업이 자꾸 실패한다"는 잘못된 인상을 준다.
func TestStatusFor(t *testing.T) {
	status, msg := statusFor(nil, false)
	if status != "success" || msg != "" {
		t.Errorf("성공 = %s %q", status, msg)
	}

	status, msg = statusFor(errors.New("디스크 가득 참"), false)
	if status != "failed" || msg != "디스크 가득 참" {
		t.Errorf("실패 = %s %q", status, msg)
	}

	status, _ = statusFor(errors.New("무엇이든"), true)
	if status != "canceled" {
		t.Errorf("취소 플래그 = %s", status)
	}
	// 컨텍스트 취소가 오류로 감싸여 올라와도 취소로 본다.
	status, _ = statusFor(context.Canceled, false)
	if status != "canceled" {
		t.Errorf("context.Canceled = %s", status)
	}
}

func TestFormatCount(t *testing.T) {
	cases := map[int64]string{0: "0", 999: "999", 1000: "1,000", 1234567: "1,234,567"}
	for in, want := range cases {
		if got := formatCount(in); got != want {
			t.Errorf("formatCount(%d) = %q, want %q", in, got, want)
		}
	}
}

// Redis 덤프는 표시용 값을 되돌려 명령으로 만든다. 그 변환이 틀리면 복구가
// 조용히 다른 데이터를 만든다.
func TestRedisCommands(t *testing.T) {
	cmds, err := redisCommands("user:1", "string", "hello", nil)
	if err != nil {
		t.Fatalf("string: %v", err)
	}
	if len(cmds) != 1 || !strings.HasPrefix(cmds[0], `SET "user:1"`) {
		t.Errorf("string 명령 = %v", cmds)
	}

	// TTL은 값을 넣은 뒤에 걸어야 한다. 먼저 걸면 SET이 만료를 지운다.
	cmds, err = redisCommands("k", "string", "v", int64(60))
	if err != nil {
		t.Fatalf("ttl: %v", err)
	}
	if len(cmds) != 2 || !strings.HasPrefix(cmds[0], "SET ") || !strings.HasPrefix(cmds[1], "EXPIRE ") {
		t.Errorf("TTL 순서 = %v", cmds)
	}

	cmds, err = redisCommands("h", "hash", "a=1, b=2", nil)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if !strings.Contains(cmds[0], `HSET "h" "a" "1" "b" "2"`) {
		t.Errorf("hash 명령 = %v", cmds)
	}

	cmds, err = redisCommands("z", "zset", "alice(1.5), bob(2)", nil)
	if err != nil {
		t.Fatalf("zset: %v", err)
	}
	if !strings.Contains(cmds[0], `ZADD "z" 1.5 "alice" 2 "bob"`) {
		t.Errorf("zset 명령 = %v", cmds)
	}

	// 되살릴 수 없는 타입은 조용히 빠지지 않고 오류로 알린다.
	if _, err := redisCommands("s", "stream", "x", nil); err == nil {
		t.Error("stream은 거부해야 한다")
	}
}

// 명령 인자에 따옴표와 줄바꿈이 있으면 복구 시 파싱이 깨진다.
func TestRedisCommandsQuoting(t *testing.T) {
	cmds, err := redisCommands("k", "string", `say "hi"`+"\nnext", nil)
	if err != nil {
		t.Fatalf("redisCommands: %v", err)
	}
	if strings.Contains(cmds[0], "\n") {
		t.Errorf("줄바꿈이 그대로 남았다: %q", cmds[0])
	}
	if !strings.Contains(cmds[0], `\"hi\"`) {
		t.Errorf("따옴표가 이스케이프되지 않았다: %q", cmds[0])
	}
}

func TestCutScore(t *testing.T) {
	member, score, ok := cutScore("alice(1.5)")
	if !ok || member != "alice" || score != "1.5" {
		t.Errorf("cutScore = %q %q %v", member, score, ok)
	}
	// 괄호가 이름 안에 있는 경우 마지막 것을 기준으로 잘라야 한다.
	member, score, ok = cutScore("a(b)(2)")
	if !ok || member != "a(b)" || score != "2" {
		t.Errorf("중첩 괄호 = %q %q %v", member, score, ok)
	}
	if _, _, ok := cutScore("점수없음"); ok {
		t.Error("점수가 없으면 실패해야 한다")
	}
}

func TestSplitPreview(t *testing.T) {
	got := splitPreview("a, b, c")
	if len(got) != 3 || got[0] != "a" || got[2] != "c" {
		t.Errorf("splitPreview = %v", got)
	}
	if len(splitPreview("")) != 0 {
		t.Error("빈 문자열은 빈 목록이어야 한다")
	}
}

// 상한을 넘긴 덤프는 잘라서 저장하지 않고 실패해야 한다.
// 잘린 백업은 복구할 수 있다고 믿게 만들므로 없는 백업보다 위험하다.
func TestWriterFailsPastLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.sql.gz")
	w, err := newWriter(path, 32)
	if err != nil {
		t.Fatalf("newWriter: %v", err)
	}
	defer w.Close()

	if err := w.WriteString(strings.Repeat("a", 20)); err != nil {
		t.Fatalf("상한 안에서 실패했다: %v", err)
	}
	if err := w.WriteString(strings.Repeat("b", 20)); err == nil {
		t.Fatal("상한을 넘겼는데 통과했다")
	}
}
