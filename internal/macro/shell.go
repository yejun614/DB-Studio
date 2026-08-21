package macro

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"dbstudio/internal/model"
)

// 셸 스크립트 실행.
//
// 이 기능은 **이중 게이트** 뒤에 있다.
//
//  1. 서버가 -allow-shell 로 켜져 있어야 한다. 이 플래그가 없으면 사용자에게 어떤
//     권한을 줘도 실행되지 않는다. 권한 설정은 화면에서 몇 번의 클릭으로 바뀌지만,
//     이 기능이 켜지는 순간 앱은 원격 셸이 된다 — 그런 성격의 변경은 프로세스를
//     띄우는 사람이 정해야 한다(-backup-cmd를 플래그로만 받는 것과 같은 판단이다).
//  2. 실행자가 script.run 권한을 가져야 한다.
//
// 그 둘을 통과한 뒤에는 임의의 스크립트를 실행한다. 명령을 걸러 내려는 시도는 하지
// 않는다 — 셸에서 무언가를 금지하는 것은 원리적으로 불가능하고(파이프, 인코딩, 인터프리터
// 호출), 반쯤 막힌 것을 안전하다고 부르는 편이 아무것도 막지 않는 것보다 위험하다.
// 여기서 우리가 할 수 있는 정직한 일은 **실행되는 것을 전부 기록하는 것**이다.

// ShellResult는 실행 결과다.
type ShellResult struct {
	Code   int
	Stdout string
	Stderr string
}

// maxShellOutput은 로그와 변수에 담는 출력의 상한이다.
// 스크립트가 수십 MB를 뱉는 일은 드물지 않고, 그것이 메타 DB로 들어가면
// 로그 화면이 열리지 않는다.
const maxShellOutput = 64 * 1024

func execShell(r *runner, n *Node) (any, string, error) {
	shell := r.rawStr(n, "shell")
	script := r.str(n, "script")
	dir := r.str(n, "dir")

	res, err := r.runShell(shell, script, dir)
	if err != nil {
		return nil, "", err
	}

	level := "info"
	if res.Code != 0 {
		level = "warn"
	}
	r.log(level, n, fmt.Sprintf("%s 종료 코드 %d", shell, res.Code), map[string]any{
		"stdout": truncate(res.Stdout, 4000),
		"stderr": truncate(res.Stderr, 4000),
	})

	if res.Code != 0 && r.flag(n, "failOnExit") {
		return nil, "", fmt.Errorf("스크립트가 종료 코드 %d로 끝났습니다: %s",
			res.Code, truncate(strings.TrimSpace(res.Stderr), 300))
	}
	return map[string]any{
		"code": float64(res.Code), "stdout": res.Stdout, "stderr": res.Stderr,
	}, PortOut, nil
}

// runShell은 게이트를 확인하고 스크립트를 실행한다.
func (r *runner) runShell(shell, script, dir string) (*ShellResult, error) {
	if !r.engine.cfg.AllowShell {
		return nil, fmt.Errorf("서버가 셸 실행을 허용하지 않습니다(-allow-shell 없이 실행 중)")
	}
	if !r.actor.HasPerm(model.PermScriptRun) {
		return nil, fmt.Errorf("셸 스크립트 실행 권한이 없습니다")
	}
	if strings.TrimSpace(script) == "" {
		return nil, fmt.Errorf("스크립트가 비어 있습니다")
	}

	name, args, err := shellCommand(shell)
	if err != nil {
		return nil, err
	}

	timeout := r.engine.cfg.ShellTimeout
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	ctx, cancel := context.WithTimeout(r.ctx, timeout)
	defer cancel()

	// 스크립트를 인자가 아니라 파일로 넘긴다.
	//
	// -c "긴 스크립트"는 OS마다 다른 인자 길이 제한에 걸리고, 따옴표 처리가
	// 플랫폼마다 달라 조용히 다른 스크립트가 실행되기도 한다. 파일은 그런 변수가 없다.
	file, err := os.CreateTemp("", "dbstudio-macro-*"+shellExt(shell))
	if err != nil {
		return nil, fmt.Errorf("임시 파일을 만들지 못했습니다: %w", err)
	}
	defer os.Remove(file.Name())
	if _, err := file.WriteString(script); err != nil {
		file.Close()
		return nil, fmt.Errorf("스크립트를 쓰지 못했습니다: %w", err)
	}
	file.Close()

	args = append(args, file.Name())
	cmd := exec.CommandContext(ctx, name, args...)
	if dir != "" {
		abs, err := filepath.Abs(dir)
		if err != nil {
			return nil, fmt.Errorf("작업 디렉터리 경로가 올바르지 않습니다: %w", err)
		}
		info, err := os.Stat(abs)
		if err != nil || !info.IsDir() {
			return nil, fmt.Errorf("작업 디렉터리를 찾을 수 없습니다: %s", dir)
		}
		cmd.Dir = abs
	}

	// 실행 문맥을 환경변수로 알려준다. 스크립트가 "누가 무엇을 실행 중인지"를
	// 알아야 하는 경우(외부 시스템에 남길 태그 등)가 흔하다.
	cmd.Env = append(os.Environ(),
		"DBSTUDIO_RUN_ID="+r.runID,
		"DBSTUDIO_MACRO="+r.macro.Name,
		"DBSTUDIO_ACTOR="+r.actor.Username,
	)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()
	result := &ShellResult{
		Code:   cmd.ProcessState.ExitCode(),
		Stdout: clipOutput(stdout.String()),
		Stderr: clipOutput(stderr.String()),
	}
	if ctx.Err() == context.DeadlineExceeded {
		return result, fmt.Errorf("스크립트가 시간 제한(%s)을 넘었습니다", timeout)
	}
	// 종료 코드가 0이 아닌 것은 오류가 아니다. 스크립트가 상태를 코드로 알려주는
	// 것은 정상적인 사용법이고, 실패로 볼지는 노드 설정(failOnExit)이 정한다.
	var exitErr *exec.ExitError
	if runErr != nil && !asExitError(runErr, &exitErr) {
		return result, fmt.Errorf("스크립트를 실행하지 못했습니다: %w", runErr)
	}
	return result, nil
}

func asExitError(err error, target **exec.ExitError) bool {
	e, ok := err.(*exec.ExitError)
	if ok {
		*target = e
	}
	return ok
}

// shellCommand는 셸 이름을 실제 실행 파일과 인자로 바꾼다.
func shellCommand(shell string) (string, []string, error) {
	switch strings.ToLower(strings.TrimSpace(shell)) {
	case "bash", "":
		return "bash", []string{}, nil
	case "sh":
		return "sh", []string{}, nil
	case "powershell", "pwsh":
		// -File은 스크립트 파일을 실행한다. -NoProfile은 사용자 프로필이 결과를
		// 바꾸는 것을 막는다(프로필이 있는 기계와 없는 기계에서 다르게 동작하면
		// 매크로가 "어느 서버에서 돌았는가"에 따라 달라진다).
		exe := "powershell"
		if runtime.GOOS != "windows" {
			exe = "pwsh"
		}
		return exe, []string{"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File"}, nil
	default:
		return "", nil, fmt.Errorf("알 수 없는 셸입니다: %s (bash 또는 powershell)", shell)
	}
}

func shellExt(shell string) string {
	switch strings.ToLower(strings.TrimSpace(shell)) {
	case "powershell", "pwsh":
		return ".ps1"
	default:
		return ".sh"
	}
}

func clipOutput(s string) string {
	if len(s) <= maxShellOutput {
		return s
	}
	return s[:maxShellOutput] + fmt.Sprintf("\n… (출력이 %d바이트를 넘어 잘렸습니다)", maxShellOutput)
}
