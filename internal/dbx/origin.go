package dbx

import (
	"fmt"
	"regexp"
	"strings"
)

// 접속 실패가 **어디서** 났는지를 문구에 남기는 도구.
//
// ── 왜 필요한가 ────────────────────────────────────────────────────
// 클러스터에서 DB에 접속하는 노드는 하나가 아니다. 담당 노드가 따로 있는 DB는 그 노드가
// 접속하고, 담당이 없는 DB는 요청을 받은 노드가 접속한다. 그래서 같은 커넥션·같은 계정·
// 같은 비밀번호인데도 **어느 노드가 실행하느냐에 따라 결과가 갈린다.**
//
// 이유는 MySQL이 사용자를 `이름` + `출발 호스트` 두 개의 조합으로 보기 때문이다.
// `appuser@172.24.0.4` 와 `appuser@172.24.0.1` 은 이름이 같아도 **다른 계정**이고,
// 한쪽만 있으면 다른 쪽에서 온 접속은 "그런 사용자 없다"로 거절된다. 사설망 VPN
// (Tailscale 등)이 SNAT하므로 노드마다 출발 호스트가 다르고, 설정 파일 어디에도 이 값은
// 적혀 있지 않다 — 계정을 만들 때 넣은 값일 뿐이라 눈으로 확인할 곳이 없다.
//
// 그래서 실패 문구가 유일한 단서인데, MySQL의 원문에는 그것이 이렇게 들어 있다:
//
//	Error 1045 (28000): Access denied for user 'appuser'@'172.24.0.4' (using password: YES)
//
// 호스트는 있지만 **어느 노드가** 그 접속을 열었는지는 없다. 사람은 그 한 줄을 붙잡고
// 노드 목록과 계정 목록을 눈으로 대조해야 한다. 이 파일은 그 두 가지를 문구에 붙인다.
//
// 여기서 하는 일은 **문구 가공뿐**이다(네트워크·저장소 접근 없음). 실행 노드 이름을
// 아는 것은 부르는 쪽의 몫이다 — 그 이름의 출처가 계층마다 다르기 때문이다(프로세스
// 자신의 이름, 또는 담당 노드의 이름).

// originMarker는 "이미 누가 붙였는가"를 판정하는 표식이다.
//
// 두 번 붙이지 않기 위해 필요하다. 담당 노드가 실패 문구를 만들어 자기 이름을 붙여
// 돌려주면, 그것을 받은 마스터가 다시 자기 차례라고 붙일 수 있다. 그렇게 되면 문구가
// "... — 실행 노드: replica-b — 실행 노드: master" 처럼 자라면서 정작 읽는 사람은
// **어느 노드가 시도했는지**를 알 수 없게 된다. 표식이 있으면 손대지 않는다.
const originMarker = "실행 노드:"

// deniedRe는 MySQL/MariaDB의 1045 원문에서 사용자와 출발 호스트를 뽑는다.
//
//	Access denied for user 'appuser'@'172.24.0.4' (using password: YES)
//
// 종류(MySQL·MariaDB)와 버전에 따라 앞부분(Error 1045 (28000):)이 달라지므로
// 그 뒤의 문장만 본다.
var deniedRe = regexp.MustCompile(`Access denied for user '([^']*)'@'([^']*)'`)

// Origin은 접속 실패 하나가 어디서 났는지다.
type Origin struct {
	// Node는 시도를 실행한 노드 이름이다. 비면 이 프로세스에서 실행했다는 뜻이다
	// (독립 실행 모드이거나, 클러스터지만 담당 노드가 이 노드인 경우).
	Node string
	// Host는 **MySQL이 연결에서 관찰한** 출발 주소다. 계정을 만들 때 넣은 값이 아니라
	// 그 순간 보인 값이라, 설정 파일에는 없는 값이다. 못 뽑으면 빈 값이다.
	Host string
	// User는 거절당한 계정 이름이다.
	User string
}

// ParseOrigin은 실패 문구에서 사용자·출발 호스트를 뽑는다.
//
// 1045가 아닌 문구(타임아웃, 호스트 미해석 등)에서는 빈 Origin을 돌려준다 — 그 경우
// 출발 호스트라는 개념 자체가 없다. 인증까지 갔는지가 이 구분의 기준이다.
func ParseOrigin(msg string) Origin {
	m := deniedRe.FindStringSubmatch(msg)
	if m == nil {
		return Origin{}
	}
	return Origin{User: m[1], Host: m[2]}
}

// WithNode는 출발 호스트를 뽑은 결과에 실행 노드 이름을 채운다.
func (o Origin) WithNode(name string) Origin {
	o.Node = strings.TrimSpace(name)
	return o
}

// Annotate는 실패 문구에 실행 노드와 출발 주소를 덧붙인다.
//
// 붙일 것이 없으면 원문을 그대로 돌려준다 — 문구를 바꾸는 것 자체가 목적이 아니라,
// 다음에 같은 실패를 만난 사람이 **어느 노드의 어느 주소를 계정에 넣어야 하는지**를
// 화면에서 바로 읽게 하는 것이 목적이다. 그래서 계정에 넣을 형태(`'user'@'host'`)까지
// 적어 준다.
//
// 이미 표식이 있으면 손대지 않는다(위 originMarker의 이유).
func (o Origin) Annotate(msg string) string {
	if strings.Contains(msg, originMarker) {
		return msg
	}
	parts := make([]string, 0, 2)
	if o.Node != "" {
		parts = append(parts, "실행 노드: "+o.Node)
	}
	if o.Host != "" {
		if o.User != "" {
			// 그대로 복사해 계정을 대조할 수 있게 원문 형태로 적는다.
			parts = append(parts, fmt.Sprintf(
				"MySQL이 본 접속 주소: %s (계정 '%s'@'%s' 가 있는지 확인)", o.Host, o.User, o.Host))
		} else {
			parts = append(parts, "MySQL이 본 접속 주소: "+o.Host)
		}
	}
	if len(parts) == 0 {
		return msg
	}
	return msg + " — " + strings.Join(parts, " / ")
}

// AnnotateError는 에러 문구를 곧바로 가공하는 지름길이다(부르는 쪽이 에러만 가진 경우).
func AnnotateError(msg, nodeName string) string {
	return ParseOrigin(msg).WithNode(nodeName).Annotate(msg)
}
