//go:build !windows

package runstate

import (
	"errors"
	"os"
	"syscall"
)

// processAlive는 PID가 살아 있는지 확인한다 (Unix).
//
// Unix에서 FindProcess는 항상 성공하므로 신호 0을 보내 확인해야 한다.
// 권한 오류는 "남의 프로세스지만 살아 있다"는 뜻이므로 살아 있는 것으로 본다.
func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = p.Signal(syscall.Signal(0))
	if err == nil {
		return true
	}
	return errors.Is(err, os.ErrPermission)
}
