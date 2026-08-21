package hostmon

import (
	"errors"
	"os"
	"runtime"
)

// errParse는 형식이 예상과 다를 때의 사유다. 원인을 더 쪼개지 않는 이유:
// 화면에 나가는 것은 "읽지 못했다"이고, 그 이상은 이 앱이 할 수 있는 일이 없다.
var errParse = errors.New("형식을 해석할 수 없습니다")

func hostname() string {
	name, err := os.Hostname()
	if err != nil {
		return ""
	}
	return name
}

// procRSS는 이 프로세스가 쓰는 메모리다.
//
// runtime.MemStats를 쓰는 이유: OS가 보고하는 RSS를 플랫폼마다 따로 읽는 것보다
// 정확하지는 않지만(런타임이 OS에 돌려준 메모리를 세지 않는다), 여기서 알고 싶은
// 것은 "이 앱이 서버를 무겁게 하는가"이고 그 판단에는 충분하다. 무엇보다 이 값은
// 어느 OS에서도 같은 뜻이다.
func procRSS() uint64 {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.Sys
}
