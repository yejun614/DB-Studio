// Package applog은 앱 전역 로거를 구성한다.
//
// 이 패키지가 존재하는 이유는 하나다: **프로세스가 왜 멈췄는지 나중에 확인할 수 있어야 한다.**
// stderr만 쓰면 터미널을 닫거나 서비스로 띄운 순간 그 기록이 사라지고, 사용자에게는
// "이유 없이 꺼졌다"로만 보인다. 그래서 기본적으로 파일에도 함께 쓰고,
// Go 런타임이 패닉 시 출력하는 크래시 리포트는 별도 파일로 받아 둔다.
package applog

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
)

// Options는 로거 구성 입력이다.
type Options struct {
	Level  string // debug | info | warn | error
	Format string // text | json
	File   string // 빈 문자열이면 stderr만 쓴다
	MaxMB  int    // 파일이 이 크기를 넘으면 .1로 밀어낸다 (0이면 무제한)
}

// Logging은 구성 결과다. 종료 시 Close를 불러야 파일 핸들이 정리된다.
type Logging struct {
	// Path는 실제로 쓰는 로그 파일이다. 파일 로그를 쓰지 않으면 빈 문자열이다.
	Path string
	// CrashPath는 런타임 크래시 리포트가 쌓이는 파일이다.
	CrashPath string
	Level     slog.Level

	rot   *rotator
	crash *os.File
}

func (l *Logging) Close() error {
	if l == nil {
		return nil
	}
	var err error
	if l.rot != nil {
		err = l.rot.Close()
	}
	if l.crash != nil {
		// 런타임은 SetCrashOutput에 준 파일을 복제해 따로 들고 있다. 우리 쪽 핸들만
		// 닫으면 런타임의 복제본이 남아 파일을 지우거나 이동할 수 없다.
		_ = debug.SetCrashOutput(nil, debug.CrashOptions{})
		_ = l.crash.Close()
		l.crash = nil
	}
	return err
}

// Setup은 slog 기본 로거를 설정한다.
//
// 파일을 열지 못해도 실패로 만들지 않는다: 로그를 파일에 못 쓴다고 서버가 뜨지 않으면
// 그 자체가 더 큰 장애다. 대신 stderr에 경고를 남기고 stderr 전용으로 진행한다.
func Setup(opts Options) *Logging {
	level := parseLevel(opts.Level)
	out := io.Writer(os.Stderr)
	result := &Logging{Level: level}

	if opts.File != "" {
		maxBytes := int64(opts.MaxMB) * 1024 * 1024
		rot, err := newRotator(opts.File, maxBytes)
		if err != nil {
			// 아직 로거가 없으므로 직접 쓴다.
			fmt.Fprintf(os.Stderr, "경고: 로그 파일을 열지 못했습니다 (%s): %v\n", opts.File, err)
		} else {
			result.Path = opts.File
			result.rot = rot
			// stderr에도 계속 쓴다. 터미널에서 띄운 사람은 그대로 보고,
			// 서비스로 띄운 경우에는 파일에 남는다.
			out = io.MultiWriter(os.Stderr, rot)
			result.enableCrashOutput(opts.File)
		}
	}

	handlerOpts := &slog.HandlerOptions{Level: level}
	var handler slog.Handler
	if strings.EqualFold(opts.Format, "json") {
		handler = slog.NewJSONHandler(out, handlerOpts)
	} else {
		handler = slog.NewTextHandler(out, handlerOpts)
	}
	slog.SetDefault(slog.New(handler))
	return result
}

// enableCrashOutput은 런타임 크래시 리포트를 별도 파일로 받는다.
//
// 로그 파일과 분리하는 이유가 두 가지다.
//  1. 런타임이 그 파일의 핸들을 계속 쥐고 있어서, 같은 파일을 로테이션하면
//     이름을 바꿀 수 없다(Windows에서 실패한다).
//  2. 크래시 리포트는 흔치 않고 결정적인 증거다. 평상시 로그에 밀려
//     로테이션으로 사라지면 안 된다.
func (l *Logging) enableCrashOutput(logPath string) {
	path := logPath + ".crash"
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		fmt.Fprintf(os.Stderr, "경고: 크래시 로그 파일을 열지 못했습니다 (%s): %v\n", path, err)
		return
	}
	if err := debug.SetCrashOutput(f, debug.CrashOptions{}); err != nil {
		fmt.Fprintf(os.Stderr, "경고: 크래시 출력을 설정하지 못했습니다: %v\n", err)
		_ = f.Close()
		return
	}
	l.CrashPath = path
	l.crash = f
}

func parseLevel(name string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// rotator는 크기 상한을 넘으면 파일을 .1로 밀어내고 새 파일에 이어 쓴다.
//
// 외부 로그 로테이터(logrotate 등)에 의존하지 않는 이유: 이 앱은 단일 바이너리로
// 아무 환경에나 놓이는 것을 전제로 한다. 무한히 커지는 로그 파일은 결국 디스크를 채운다.
// 보관본을 하나만 두는 것은 의도적이다 — 장애 조사에 필요한 것은 최근 기록이다.
type rotator struct {
	mu       sync.Mutex
	file     *os.File
	path     string
	maxBytes int64
	size     int64
}

func newRotator(path string, maxBytes int64) (*rotator, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	f, err := openAppend(path)
	if err != nil {
		return nil, err
	}
	size := fileSize(f)
	// 이미 상한을 넘긴 파일이면 시작 시점에 한 번 밀어낸다.
	// 그러지 않으면 재시작할 때마다 상한을 넘긴 파일에 계속 덧붙는다.
	if maxBytes > 0 && size > maxBytes {
		_ = f.Close()
		if err := rotateFile(path); err != nil {
			return nil, err
		}
		if f, err = openAppend(path); err != nil {
			return nil, err
		}
		size = 0
	}
	return &rotator{file: f, path: path, maxBytes: maxBytes, size: size}, nil
}

func (r *rotator) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	n, err := r.file.Write(p)
	r.size += int64(n)
	if err != nil {
		return n, err
	}
	if r.maxBytes <= 0 || r.size < r.maxBytes {
		return n, nil
	}

	// 로테이션 실패는 삼킨다. 로그를 남기려다 로그를 못 남기게 되는 것이 최악이다.
	_ = r.file.Close()
	if rerr := rotateFile(r.path); rerr != nil {
		fmt.Fprintf(os.Stderr, "경고: 로그 로테이션 실패: %v\n", rerr)
	}
	f, ferr := openAppend(r.path)
	if ferr != nil {
		fmt.Fprintf(os.Stderr, "경고: 새 로그 파일을 열지 못했습니다: %v\n", ferr)
		return n, nil
	}
	r.file = f
	r.size = fileSize(f)
	return n, nil
}

func (r *rotator) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.file == nil {
		return nil
	}
	err := r.file.Close()
	r.file = nil
	return err
}

func openAppend(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
}

func fileSize(f *os.File) int64 {
	st, err := f.Stat()
	if err != nil {
		return 0
	}
	return st.Size()
}

// rotateFile은 현재 파일을 .1로 밀어낸다.
//
// 호출 전에 파일을 닫아야 한다: Windows는 이 프로세스가 열고 있는 파일의 이름을
// 바꾸지 못한다(Go는 FILE_SHARE_DELETE 없이 파일을 연다).
func rotateFile(path string) error {
	backup := path + ".1"
	_ = os.Remove(backup)
	return os.Rename(path, backup)
}

// Recover는 백그라운드 goroutine의 패닉을 잡아 기록한다.
//
// HTTP 핸들러는 Fiber의 recover 미들웨어가 감싸지만, 그 밖의 goroutine
// (모니터 폴러, WebSocket 펌프, SSE 스트림 라이터)에서 패닉이 나면
// **프로세스 전체가 죽는다.** 사용자가 보는 "이유 없이 꺼짐"의 대표적인 원인이다.
//
//	defer applog.Recover("monitor.poll")
func Recover(where string) {
	if v := recover(); v != nil {
		slog.Error("패닉을 복구했습니다 (해당 작업만 중단됩니다)",
			"where", where, "panic", fmt.Sprint(v), "stack", string(debug.Stack()))
	}
}
