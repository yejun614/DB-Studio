//go:build !linux && !windows

package hostmon

// 리눅스·윈도우가 아닌 곳에서는 시스템 로그를 읽지 않는다.
//
// macOS의 통합 로그는 `log show`(외부 명령)로만 접근할 수 있고, 그 출력 형식은
// 버전마다 바뀐다. 못 읽는 것을 못 읽는다고 말하는 편이, 어느 날 조용히 빈 결과를
// 돌려주기 시작하는 것보다 낫다.

func defaultLogPath() string { return "" }

func (l *OSLog) Read() ([]LogEntry, string) {
	return nil, "이 운영체제에서는 시스템 로그를 읽지 않습니다"
}
