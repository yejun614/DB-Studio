package docker

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// 컨테이너 안에서 명령 하나 돌리기.
//
// ── 무엇에 쓰는가 ───────────────────────────────────────────────────
// 만든 DB 에 "정말 이렇게 떴는가"를 물어보는 데 쓴다. 설정 파일을 넣는 것과
// DB 가 그것을 읽은 것은 다른 일이라(경로가 하나 틀리면 파일은 잘 들어가고 DB 는
// 기본값으로 뜬다), 확인하려면 DB 자신에게 물어야 한다.
//
// ── 임의 명령의 통로가 아니다 ───────────────────────────────────────
// 이 함수는 **우리가 적어 둔 명령**만 돌린다(레시피의 헬스체크, 확인 질의).
// 사용자가 준 문자열을 여기 넘기는 길은 만들지 않는다 — 도커 소켓에 닿는
// 이 앱에서 그것은 곧 호스트의 셸이고, 그 문은 -allow-shell 하나로 충분하다.
//
// argv 를 배열로 받는 것도 그래서다. 문자열 하나로 받으면 셸을 거치게 되고,
// 그러면 값에 든 세미콜론이 명령이 된다.

// Exec은 컨테이너 안에서 명령을 돌리고 출력을 돌려준다.
//
// stdout 과 stderr 를 합쳐 준다. 여기 쓰는 명령들은 실패 이유를 stderr 로
// 내는 것이 흔하고, 그 줄이 사라지면 왜 실패했는지 알 수 없다.
func (c *Client) Exec(ctx context.Context, id string, argv []string) (string, error) {
	if len(argv) == 0 {
		return "", fmt.Errorf("명령 실행: 명령이 비어 있습니다")
	}

	// 1) exec 를 만든다.
	var created struct {
		ID string `json:"Id"`
	}
	err := c.postJSON(ctx, "/"+APIVersion+"/containers/"+url.PathEscape(id)+"/exec",
		map[string]any{
			"AttachStdout": true,
			"AttachStderr": true,
			"Cmd":          argv,
		}, &created, "명령 준비")
	if err != nil {
		return "", err
	}

	// 2) 시작하고 출력을 읽는다.
	//
	// Detach 를 false 로 두면 이 응답의 본문이 곧 출력이다. Tty 는 켜지 않는다 —
	// 켜면 프레임 머리말이 사라져서 stdout 과 stderr 를 가를 수 없다.
	res, err := c.do(ctx, http.MethodPost,
		"/"+APIVersion+"/exec/"+url.PathEscape(created.ID)+"/start",
		map[string]any{"Detach": false, "Tty": false}, "명령 실행")
	if err != nil {
		return "", err
	}
	defer res.Body.Close()

	// 출력도 로그와 같은 프레임 형식이다.
	stream := &LogStream{body: res.Body, tty: false}
	var b strings.Builder
	for line := range stream.Lines() {
		if b.Len() > 64<<10 {
			break // 확인 질의의 출력이 64KB 를 넘는 일은 없다
		}
		b.WriteString(line.Text)
		b.WriteString("\n")
	}
	out := strings.TrimRight(b.String(), "\n")

	// 3) 종료 코드를 본다.
	//
	// 출력만 보면 실패를 놓친다. clickhouse-client 는 오류를 stderr 로 내고
	// 빈 stdout 을 남기는데, 그것을 "빈 결과"로 읽으면 설정이 안 먹은 것과
	// 명령이 틀린 것이 구별되지 않는다.
	var info struct {
		ExitCode int  `json:"ExitCode"`
		Running  bool `json:"Running"`
	}
	if err := c.getJSON(ctx, "/"+APIVersion+"/exec/"+url.PathEscape(created.ID)+"/json",
		&info, "명령 결과 확인"); err != nil {
		return out, err
	}
	if info.ExitCode != 0 {
		return out, fmt.Errorf("명령이 실패했습니다 (종료 코드 %d): %s", info.ExitCode, clip(out, 500))
	}
	return out, nil
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
