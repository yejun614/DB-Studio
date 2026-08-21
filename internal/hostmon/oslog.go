package hostmon

import (
	"strings"
	"time"
)

// 시스템 로그(OS가 남기는 오류) 읽기.
//
// 왜 이것까지 보는가: 디스크가 죽어 가거나 커널이 프로세스를 죽였을 때(OOM) DB는
// "느리다"거나 "연결이 끊겼다"로만 보인다. 원인은 OS 로그에만 있고, 그 사실을 아는
// 사람은 서버에 로그인해 본 사람뿐이다.
//
// **새 줄만** 본다. 처음 볼 때는 지금 위치를 기억만 하고 아무것도 만들지 않는다 —
// 기능을 켠 순간 지난 몇 달의 오류가 이벤트로 쏟아지면 그 목록은 아무도 보지 않는다.

// LogEntry는 시스템 로그에서 뽑아낸 한 줄이다.
type LogEntry struct {
	At      time.Time `json:"at"`
	Source  string    `json:"source,omitempty"`
	Message string    `json:"message"`
}

// OSLog은 "어디까지 읽었는지"를 들고 이어 읽는다.
//
// 위치를 파일이 아니라 DB(host_state)에 두는 이유: 앱을 재시작할 때마다 처음부터
// 읽으면 같은 오류가 다시 이벤트가 되고, 끝에서 다시 시작하면 꺼져 있던 동안의
// 오류를 영원히 놓친다.
type OSLog struct {
	path   string
	offset int64
	// primed는 "이 위치는 이미 확인된 자리"라는 뜻이다. 거짓이면 첫 읽기이므로
	// 내용을 만들지 않고 위치만 잡는다.
	primed bool
}

// NewOSLog는 저장된 위치에서 이어 읽는 리더를 만든다.
//
// path가 저장된 것과 다르면(설정을 바꿨거나 처음이면) 위치를 버리고 끝에서 시작한다.
func NewOSLog(wantPath, savedPath string, savedOffset int64) *OSLog {
	l := &OSLog{path: strings.TrimSpace(wantPath)}
	if l.path == "" {
		l.path = defaultLogPath()
	}
	if savedPath == l.path && savedOffset >= 0 {
		l.offset, l.primed = savedOffset, true
	}
	return l
}

func (l *OSLog) Path() string  { return l.path }
func (l *OSLog) Offset() int64 { return l.offset }

// Available은 읽을 대상이 있는지다. 없으면 이유는 Note에 실린다.
func (l *OSLog) Available() bool { return l.path != "" }

// maxEntries는 한 번에 이벤트로 만들 줄 수 상한이다.
//
// 상한을 두는 이유: 장애 중에는 같은 오류가 초당 수백 줄씩 쌓인다. 그것을 다 이벤트로
// 만들면 이벤트 표가 로그 사본이 된다. 이벤트는 "무슨 일이 있었나"를 알리는 것이고
// 자세한 내용은 로그 파일이 갖고 있다.
const maxEntries = 20

// errorPatterns는 "이건 사람이 봐야 한다"고 볼 만한 표시다.
//
// 소문자로 비교한다. 목록을 짧게 유지하는 이유: 'error'만으로 걸러도 정상 로그가
// 잔뜩 걸린다(설정 파일 이름에 error가 들어가는 것만으로도). 확실한 것만 남긴다.
var errorPatterns = []string{
	"out of memory",
	"oom-killer",
	"i/o error",
	"kernel panic",
	"segfault",
	"filesystem error",
	"ext4-fs error",
	"xfs error",
	"medium error",
	"critical",
	"read-only file system",
	"failed to start",
	"disk quota exceeded",
	"no space left",
}

// looksLikeError는 한 줄이 보고할 만한 오류인지 본다.
func looksLikeError(line string) bool {
	low := strings.ToLower(line)
	for _, p := range errorPatterns {
		// 정규식을 쓰지 않는다. 여기 필요한 것은 부분 문자열 일치이고,
		// 패턴을 늘릴 때 정규식이면 실수 하나로 모든 줄이 걸린다.
		if strings.Contains(low, p) {
			return true
		}
	}
	return false
}
