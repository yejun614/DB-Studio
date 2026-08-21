//go:build linux

package hostmon

import (
	"bufio"
	"os"
	"strings"
	"time"
)

// 리눅스에서는 평문 syslog 파일을 이어 읽는다.
//
// journalctl을 부르지 않는 이유: 외부 명령을 실행하면 이 앱이 도는 환경(컨테이너,
// 최소 이미지)에 그 명령이 있어야 하고, 없을 때의 실패가 조용하다. 파일은 있으면 읽히고
// 없으면 없다고 말할 수 있다. journald만 쓰는 시스템에서는 -host-syslog 로 경로를
// 지정하거나(예: rsyslog 설정), 이 기능이 꺼진 것으로 본다.

// defaultLogPath는 흔한 경로 중 실제로 있는 것을 고른다.
func defaultLogPath() string {
	for _, p := range []string{"/var/log/syslog", "/var/log/messages"} {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return ""
}

// Read는 지난번 위치 이후의 오류 줄을 돌려준다.
//
// 두 번째 반환값은 사람이 읽을 사유다(못 읽었을 때). 오류로 만들지 않는 이유:
// 로그를 못 읽는 것은 흔한 일이고(권한, 컨테이너), 그때 지표 수집까지 멈출 이유는 없다.
func (l *OSLog) Read() ([]LogEntry, string) {
	if l.path == "" {
		return nil, "시스템 로그 파일을 찾지 못했습니다 (-host-syslog 로 경로를 지정할 수 있습니다)"
	}
	f, err := os.Open(l.path)
	if err != nil {
		return nil, "시스템 로그를 열지 못했습니다: " + err.Error()
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return nil, "시스템 로그 크기를 알 수 없습니다: " + err.Error()
	}
	size := st.Size()
	// 파일이 작아졌으면 로테이션된 것이다. 그 자리에서 이어 읽으면 새 파일의
	// 한가운데를 줄 중간부터 읽게 된다.
	if size < l.offset {
		l.offset = 0
	}
	if !l.primed {
		// 첫 읽기: 지금 끝을 기억만 한다.
		l.offset, l.primed = size, true
		return nil, ""
	}
	if size == l.offset {
		return nil, ""
	}

	if _, err := f.Seek(l.offset, 0); err != nil {
		return nil, "시스템 로그 위치를 옮기지 못했습니다: " + err.Error()
	}

	out := []LogEntry{}
	sc := bufio.NewScanner(f)
	// 한 줄이 아주 길 수 있다(스택 덤프 등). 기본 상한(64KB)에서 멈추면
	// 그 줄 이후를 통째로 놓치므로 넉넉히 잡는다.
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	read := l.offset
	for sc.Scan() {
		line := sc.Text()
		read += int64(len(line)) + 1
		if !looksLikeError(line) {
			continue
		}
		if len(out) < maxEntries {
			out = append(out, LogEntry{At: time.Now(), Source: l.path, Message: trimLine(line)})
		}
	}
	// 스캔 오류(마지막 줄이 아직 다 쓰이지 않은 경우 등)에도 읽은 만큼은 인정한다.
	l.offset = read
	if l.offset > size {
		l.offset = size
	}
	return out, ""
}

// trimLine은 이벤트 메시지에 넣을 만큼만 남긴다.
func trimLine(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 300 {
		return s[:300] + "…"
	}
	return s
}
