package runstate

import (
	"os/exec"
	"runtime"
	"testing"
)

// deadProcessPID는 이미 종료한 프로세스의 PID를 돌려준다.
//
// 임의의 큰 숫자를 쓰지 않는 이유: 그 PID가 우연히 살아 있는 프로세스일 수 있어
// 테스트가 간헐적으로 실패한다. 실제로 실행하고 끝난 프로세스의 PID가 확실하다.
func deadProcessPID(t *testing.T) int {
	t.Helper()

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("cmd", "/c", "exit", "0")
	} else {
		cmd = exec.Command("true")
	}
	if err := cmd.Start(); err != nil {
		t.Skipf("보조 프로세스를 실행할 수 없어 건너뜁니다: %v", err)
	}
	pid := cmd.Process.Pid
	if err := cmd.Wait(); err != nil {
		t.Skipf("보조 프로세스가 실패했습니다: %v", err)
	}
	return pid
}
