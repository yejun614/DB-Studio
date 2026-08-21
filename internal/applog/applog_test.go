package applog

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// LF는 테스트에서 줄바꿈을 명시적으로 쓰기 위한 상수다.
const LF = "\n"

// TestSetupWritesToFile은 로그가 파일에 남는지 확인한다.
// 이것이 이 패키지의 존재 이유다 — 터미널이 사라져도 기록이 남아야 한다.
func TestSetupWritesToFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")

	logging := Setup(Options{Level: "info", Format: "text", File: path, MaxMB: 10})
	defer logging.Close()

	if logging.Path != path {
		t.Errorf("Path = %q, %q를 기대했습니다", logging.Path, path)
	}
	slog.Info("테스트 줄", "key", "value")

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("로그 파일을 읽지 못했습니다: %v", err)
	}
	if !strings.Contains(string(data), "테스트 줄") {
		t.Errorf("로그 내용에 메시지가 없습니다: %q", data)
	}
	if !strings.Contains(string(data), "key=value") {
		t.Errorf("구조화 필드가 없습니다: %q", data)
	}
}

func TestSetupJSONFormat(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	logging := Setup(Options{Level: "info", Format: "json", File: path, MaxMB: 10})
	defer logging.Close()

	slog.Info("json 줄", "count", 3)
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), `"msg":"json 줄"`) || !strings.Contains(string(data), `"count":3`) {
		t.Errorf("JSON 형식이 아닙니다: %q", data)
	}
}

// TestLevelFilter는 레벨 아래의 기록이 파일에 남지 않는지 확인한다.
// debug를 항상 남기면 파일이 폴링 로그로 가득 차 조사에 쓸 수 없다.
func TestLevelFilter(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	logging := Setup(Options{Level: "warn", Format: "text", File: path, MaxMB: 10})
	defer logging.Close()

	slog.Info("보이면 안 되는 줄")
	slog.Warn("보여야 하는 줄")

	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "보이면 안 되는 줄") {
		t.Error("레벨 아래의 기록이 남았습니다")
	}
	if !strings.Contains(string(data), "보여야 하는 줄") {
		t.Error("레벨 이상의 기록이 없습니다")
	}
}

// TestRotatorRotatesAtLimit은 상한을 넘으면 .1로 밀려나고 새 파일에 이어 쓰는지 확인한다.
// 로테이션이 없으면 오래 도는 서버의 로그가 디스크를 채운다.
func TestRotatorRotatesAtLimit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")

	rot, err := newRotator(path, 200) // 200바이트 상한
	if err != nil {
		t.Fatal(err)
	}
	defer rot.Close()

	if _, err := rot.Write([]byte(strings.Repeat("a", 150) + LF)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".1"); err == nil {
		t.Fatal("상한 이전에 로테이션이 일어났습니다")
	}
	if _, err := rot.Write([]byte(strings.Repeat("b", 100) + LF)); err != nil {
		t.Fatal(err)
	}

	backup, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatalf("백업 파일이 없습니다: %v", err)
	}
	if !strings.Contains(string(backup), "aaa") || !strings.Contains(string(backup), "bbb") {
		t.Error("백업에 이전 내용이 없습니다")
	}

	// 로테이션 이후에도 계속 쓸 수 있어야 한다.
	if _, err := rot.Write([]byte("after" + LF)); err != nil {
		t.Fatalf("로테이션 이후 쓰기 실패: %v", err)
	}
	current, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(current) != "after"+LF {
		t.Errorf("새 파일 내용 = %q, after 한 줄을 기대했습니다", current)
	}
}

// TestRotatorCloseReleasesCurrentFile은 로테이션 후에도 Close가 살아 있는 핸들을
// 닫는지 확인한다. 닫지 않으면 파일 핸들이 새고, Windows에서는 그 파일을 지울 수 없다.
func TestRotatorCloseReleasesCurrentFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	rot, err := newRotator(path, 50)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rot.Write([]byte(strings.Repeat("x", 80))); err != nil {
		t.Fatal(err)
	}
	if err := rot.Close(); err != nil {
		t.Fatalf("Close 실패: %v", err)
	}
	// 핸들이 남아 있으면 Windows에서 삭제가 실패한다.
	if err := os.Remove(path); err != nil {
		t.Errorf("로테이션 후 현재 파일을 지울 수 없습니다(핸들 누수): %v", err)
	}
}

// TestOversizedFileRotatedOnStart는 이미 큰 파일을 시작 시점에 밀어내는지 확인한다.
func TestOversizedFileRotatedOnStart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	if err := os.WriteFile(path, []byte(strings.Repeat("y", 2*1024*1024)), 0o600); err != nil {
		t.Fatal(err)
	}

	logging := Setup(Options{Level: "info", Format: "text", File: path, MaxMB: 1})
	defer logging.Close()
	slog.Info("새 시작")

	if _, err := os.Stat(path + ".1"); err != nil {
		t.Errorf("시작 시점에 밀어내지 않았습니다: %v", err)
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "yyyy") {
		t.Error("이전 내용이 현재 파일에 남아 있습니다")
	}
	if !strings.Contains(string(data), "새 시작") {
		t.Error("새 파일에 기록이 없습니다")
	}
}

// TestSetupSurvivesUnwritableFile은 파일을 열 수 없어도 로거가 동작하는지 확인한다.
// 로그를 파일에 못 쓴다고 서버가 뜨지 않으면 그것이 더 큰 장애다.
func TestSetupSurvivesUnwritableFile(t *testing.T) {
	dir := t.TempDir()
	// 디렉터리를 로그 파일 경로로 준다 — 열 수 없다.
	logging := Setup(Options{Level: "info", Format: "text", File: dir, MaxMB: 10})
	defer logging.Close()

	if logging.Path != "" {
		t.Errorf("열지 못한 파일을 경로로 보고했습니다: %q", logging.Path)
	}
	// 패닉 없이 기록이 되어야 한다(stderr로).
	slog.Info("stderr 전용 모드")
}

func TestRecoverLogsPanic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	logging := Setup(Options{Level: "info", Format: "text", File: path, MaxMB: 10})
	defer logging.Close()

	func() {
		defer Recover("테스트지점")
		panic("일부러 낸 패닉")
	}()

	data, _ := os.ReadFile(path)
	text := string(data)
	if !strings.Contains(text, "테스트지점") {
		t.Errorf("패닉 위치가 기록되지 않았습니다: %q", text)
	}
	if !strings.Contains(text, "일부러 낸 패닉") {
		t.Errorf("패닉 값이 기록되지 않았습니다: %q", text)
	}
	if !strings.Contains(text, "applog_test.go") {
		t.Errorf("스택이 기록되지 않았습니다: %q", text)
	}
}

// TestCrashOutputIsSeparateFile은 크래시 리포트가 별도 파일로 가는지 확인한다.
// 같은 파일이면 (1) 런타임이 핸들을 쥐고 있어 로테이션이 실패하고
// (2) 결정적인 증거가 평상시 로그에 밀려 사라진다.
func TestCrashOutputIsSeparateFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	logging := Setup(Options{Level: "info", Format: "text", File: path, MaxMB: 1})
	defer logging.Close()

	if logging.CrashPath != path+".crash" {
		t.Errorf("CrashPath = %q, %q를 기대했습니다", logging.CrashPath, path+".crash")
	}
	if _, err := os.Stat(logging.CrashPath); err != nil {
		t.Errorf("크래시 파일이 만들어지지 않았습니다: %v", err)
	}
	// 크래시 출력이 켜진 상태에서도 로그 파일 로테이션이 되어야 한다.
	// (같은 파일을 쓰면 런타임이 핸들을 쥐고 있어 이름을 바꿀 수 없다)
	logging.rot.maxBytes = 100
	if _, err := logging.rot.Write([]byte(strings.Repeat("z", 200))); err != nil {
		t.Fatalf("쓰기 실패: %v", err)
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Errorf("크래시 출력이 켜진 상태에서 로테이션이 실패했습니다: %v", err)
	}
}

func TestParseLevel(t *testing.T) {
	cases := map[string]slog.Level{
		"debug": slog.LevelDebug, "DEBUG": slog.LevelDebug,
		"info": slog.LevelInfo, "": slog.LevelInfo, "nonsense": slog.LevelInfo,
		"warn": slog.LevelWarn, "warning": slog.LevelWarn,
		"error": slog.LevelError,
	}
	for in, want := range cases {
		if got := parseLevel(in); got != want {
			t.Errorf("parseLevel(%q) = %v, %v를 기대했습니다", in, got, want)
		}
	}
}
