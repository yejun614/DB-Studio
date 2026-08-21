package runstate

import "syscall"

// Windows 접근 권한 상수.
//
// SYNCHRONIZE만으로 WaitForSingleObject를 부를 수 있다. 다른 사용자의 프로세스에도
// 대개 허용되는 최소 권한이라 관리자 권한 없이 동작한다.
const (
	synchronize                    = 0x00100000
	processQueryLimitedInformation = 0x00001000
)

// processAlive는 PID가 살아 있는지 확인한다 (Windows).
//
// os.FindProcess를 쓰지 않는 이유: 최신 Go에서는 종료된 PID에도 성공을 돌려주므로
// 살아 있는지 판단할 수 없다(테스트가 이것을 잡아냈다 — 강제 종료된 프로세스를
// "실행 중"으로 오판했다).
//
// 대신 프로세스 핸들을 열고 대기 상태를 본다. 종료된 프로세스의 핸들은 시그널 상태가
// 되므로 WaitForSingleObject가 즉시 WAIT_OBJECT_0(0)을 돌려준다. GetExitCodeProcess의
// STILL_ACTIVE(259)를 보는 방법은 종료 코드가 실제로 259인 프로세스와 구분되지 않는다.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := syscall.OpenProcess(synchronize|processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		// 존재하지 않는 PID이거나 접근이 거부됐다. 접근 거부는 "남의 프로세스가
		// 살아 있다"는 뜻이지만, 우리가 남긴 표식의 주인은 같은 사용자이므로
		// 여기서는 없는 것으로 본다.
		return false
	}
	defer syscall.CloseHandle(h)

	const waitObject0 = 0 // 핸들이 시그널 상태 = 프로세스가 종료했다
	event, err := syscall.WaitForSingleObject(h, 0)
	if err != nil {
		return false
	}
	return event != waitObject0
}
