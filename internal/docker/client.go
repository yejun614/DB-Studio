// Package docker는 도커 엔진 API 를 부르는 최소한의 클라이언트다.
//
// SDK 를 쓰지 않는 이유는 이 앱이 SigV4 를 손으로 쓴 것과 같다. 엔진 API 는
// 유닉스 소켓 위의 평범한 HTTP + JSON 이고, 우리가 쓸 것은 그중 스무 개쯤이다.
// github.com/docker/docker 를 넣으면 의존성 나무가 백 개 단위로 늘어나는데,
// 그 대가로 얻는 것이 net/http 로 열 줄이면 되는 일이다. 단일 바이너리로
// 빌드된다는 전제도 함께 지킨다.
//
// ── 이 패키지가 하지 않는 것 ────────────────────────────────────────
// 권한 판단을 하지 않는다. "이 사람이 도커를 만질 수 있는가"는 API 계층의
// 일이고(docker.manage 권한 + 스위치), 여기까지 온 호출은 이미 허락된 것이다.
// 그 두 가지를 한곳에 섞으면 "권한 검사를 어디서 하는가"가 흐려진다.
package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"time"
)

// APIVersion은 우리가 말하는 엔진 API 버전이다.
//
// 낮게 고정한다. 엔진 API 는 뒤로 호환되므로 새 데몬은 옛 버전 요청을 그대로
// 받아 주지만, 반대는 아니다 — 최신 버전을 적어 두면 조금 옛 데몬에서 통째로
// 실패한다. 1.43 은 도커 24.0(2023-05)의 것이라 지금 쓰이는 데몬은 거의 다
// 받는다. 더 새 기능이 필요해지면 그때 그 호출만 버전을 올린다.
const APIVersion = "v1.43"

// DefaultSocket은 리눅스에서 도커 데몬이 듣는 자리다.
const DefaultSocket = "/var/run/docker.sock"

// Client는 도커 데몬 하나를 가리킨다.
type Client struct {
	socket string
	http   *http.Client
}

// New는 유닉스 소켓에 붙는 클라이언트를 만든다.
//
// 붙어 보지는 않는다. 만드는 것과 닿는 것을 갈라 두면 앱이 뜰 때 도커가 없어도
// 뜨고(도커는 곁다리 기능이다), 화면이 "왜 안 닿는가"를 물어볼 자리가 생긴다.
func New(socket string) *Client {
	if strings.TrimSpace(socket) == "" {
		socket = DefaultSocket
	}
	return &Client{
		socket: socket,
		http: &http.Client{
			// 여기 시간 제한을 두지 않는다. 로그 따라가기처럼 끝나지 않는 호출이
			// 있고, 그것까지 잘리면 로그 화면이 20초마다 끊긴다. 제한은 부르는
			// 쪽이 ctx 로 준다.
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", socket)
				},
				// 소켓 하나에 붙는 것이라 연결을 아껄 이유가 없지만, 로그를
				// 여럿 따라가는 동안 제어 호출이 막히지 않을 만큼은 열어 둔다.
				MaxIdleConns:          8,
				IdleConnTimeout:       30 * time.Second,
				ResponseHeaderTimeout: 0,
			},
		},
	}
}

// Socket은 이 클라이언트가 보는 소켓 경로다.
func (c *Client) Socket() string { return c.socket }

// ---------- 오류 ----------

// Error는 데몬이 돌려준 오류다.
//
// 상태 코드를 함께 들고 있는 이유: 404(없음)와 409(이미 그 상태다)와 500(데몬
// 안에서 터졌다)은 화면이 다르게 다뤄야 한다. 문장만 보면 그 구분을 매번
// 문자열로 다시 해야 한다.
type Error struct {
	Status  int
	Message string
	// Op는 무엇을 하다 났는지다(컨테이너 만들기, 로그 읽기 …).
	Op string
}

func (e *Error) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("%s 실패 (HTTP %d)", e.Op, e.Status)
	}
	return fmt.Sprintf("%s 실패: %s", e.Op, e.Message)
}

// NotFound는 없는 것을 가리켰는지다.
func (e *Error) NotFound() bool { return e.Status == http.StatusNotFound }

// Conflict는 이미 그 상태인지다(돌고 있는 것을 또 시작 등).
func (e *Error) Conflict() bool { return e.Status == http.StatusConflict }

// IsNotFound는 오류가 "없다"인지 본다.
func IsNotFound(err error) bool {
	var de *Error
	return errors.As(err, &de) && de.NotFound()
}

// IsConflict는 오류가 "이미 그 상태다"인지 본다.
func IsConflict(err error) bool {
	var de *Error
	return errors.As(err, &de) && de.Conflict()
}

// UnreachableError는 데몬에 닿지 못한 것이다(오류를 돌려받은 것이 아니다).
//
// 갈라 두는 이유: 이 둘은 사람이 할 일이 완전히 다르다. 데몬이 돌려준 오류는
// 요청을 고쳐야 하고, 닿지 못한 것은 배치를 고쳐야 한다(소켓을 마운트했는가,
// 권한이 있는가). 같은 오류로 뭉치면 화면이 그 둘에 같은 말을 하게 된다.
type UnreachableError struct {
	Socket string
	Reason string
	Err    error
}

func (e *UnreachableError) Error() string {
	return fmt.Sprintf("도커에 닿지 못했습니다 (%s): %s", e.Socket, e.Reason)
}

func (e *UnreachableError) Unwrap() error { return e.Err }

// IsUnreachable은 오류가 "닿지 못했다"인지 본다.
func IsUnreachable(err error) bool {
	var ue *UnreachableError
	return errors.As(err, &ue)
}

// unreachable은 붙지 못한 이유를 사람 말로 바꾼다.
//
// 무엇을 고쳐야 하는지까지 적는다. "connect: permission denied" 만 보고
// 무엇을 해야 하는지 아는 사람은 이미 아는 사람이고, 그렇지 않은 사람에게는
// 그 문장이 막다른 길이다.
func (c *Client) unreachable(err error) error {
	reason := err.Error()
	switch {
	case errors.Is(err, os.ErrNotExist), strings.Contains(reason, "no such file"):
		reason = "소켓 파일이 없습니다. 컨테이너로 돌고 있다면 " +
			"-v /var/run/docker.sock:/var/run/docker.sock 로 소켓을 넣어 주세요"
	case errors.Is(err, os.ErrPermission), strings.Contains(reason, "permission denied"):
		reason = "소켓을 열 권한이 없습니다. 이 앱은 nonroot(65532)로 도는데 " +
			"소켓은 보통 root:docker 라, 컨테이너에 도커 그룹의 gid 를 주거나 " +
			"user 를 바꿔야 합니다"
	case strings.Contains(reason, "connection refused"):
		reason = "소켓은 있지만 데몬이 받지 않습니다. 도커가 돌고 있는지 확인하세요"
	case runtime.GOOS == "windows":
		// 개발 기계에서 흔히 만난다. 윈도의 도커 데스크톱은 유닉스 소켓이 아니라
		// 이름 있는 파이프(npipe)를 쓴다 — 이 앱의 배포 대상은 리눅스다.
		reason = "윈도의 도커 데스크톱은 유닉스 소켓이 아니라 이름 있는 파이프를 씁니다. " +
			"이 기능은 리눅스 호스트에서 씁니다"
	}
	return &UnreachableError{Socket: c.socket, Reason: reason, Err: err}
}

// ---------- 호출 ----------

// do는 요청 하나를 보내고 응답을 그대로 돌려준다(본문을 닫는 것은 부르는 쪽 몫).
//
// 스트리밍(로그)에 쓰려면 본문을 여기서 읽어 버릴 수 없다. 그래서 JSON 을
// 받아 푸는 편의 함수(getJSON)를 따로 둔다.
func (c *Client) do(ctx context.Context, method, path string, body any, op string) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("%s: 요청을 만들지 못했습니다: %w", op, err)
		}
		reader = bytes.NewReader(raw)
	}

	// 호스트 이름은 아무것이나 된다(유닉스 소켓으로 가므로 쓰이지 않는다).
	// 그래도 적어 두어야 net/http 가 요청을 만든다.
	req, err := http.NewRequestWithContext(ctx, method, "http://docker"+path, reader)
	if err != nil {
		return nil, fmt.Errorf("%s: 요청을 만들지 못했습니다: %w", op, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	res, err := c.http.Do(req)
	if err != nil {
		// ctx 가 끝난 것은 배치 문제가 아니다 — 그대로 올린다.
		if ctx.Err() != nil {
			return nil, fmt.Errorf("%s: %w", op, ctx.Err())
		}
		return nil, c.unreachable(err)
	}
	if res.StatusCode >= 400 {
		defer res.Body.Close()
		return nil, decodeError(res, op)
	}
	return res, nil
}

// decodeError는 데몬의 오류 본문을 읽는다.
func decodeError(res *http.Response, op string) error {
	// 본문을 넉넉히 자른다. 오류 문장이 몇 킬로바이트를 넘는 일은 없고,
	// 넘는다면 그것은 오류 본문이 아니다.
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 8<<10))
	var payload struct {
		Message string `json:"message"`
	}
	msg := ""
	if json.Unmarshal(raw, &payload) == nil {
		msg = strings.TrimSpace(payload.Message)
	}
	if msg == "" {
		msg = strings.TrimSpace(string(raw))
	}
	return &Error{Status: res.StatusCode, Message: msg, Op: op}
}

// getJSON은 GET 해서 JSON 을 푼다.
func (c *Client) getJSON(ctx context.Context, path string, out any, op string) error {
	res, err := c.do(ctx, http.MethodGet, path, nil, op)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if out == nil {
		_, _ = io.Copy(io.Discard, res.Body)
		return nil
	}
	if err := json.NewDecoder(res.Body).Decode(out); err != nil {
		return fmt.Errorf("%s: 응답을 해석하지 못했습니다: %w", op, err)
	}
	return nil
}

// postJSON은 POST 하고 JSON 을 푼다(out 이 nil 이면 본문을 버린다).
func (c *Client) postJSON(ctx context.Context, path string, body, out any, op string) error {
	res, err := c.do(ctx, http.MethodPost, path, body, op)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if out == nil {
		_, _ = io.Copy(io.Discard, res.Body)
		return nil
	}
	if err := json.NewDecoder(res.Body).Decode(out); err != nil {
		return fmt.Errorf("%s: 응답을 해석하지 못했습니다: %w", op, err)
	}
	return nil
}

// q는 질의 문자열을 만든다(빈 값은 넣지 않는다).
func q(pairs map[string]string) string {
	if len(pairs) == 0 {
		return ""
	}
	v := url.Values{}
	for k, val := range pairs {
		if val == "" {
			continue
		}
		v.Set(k, val)
	}
	if len(v) == 0 {
		return ""
	}
	return "?" + v.Encode()
}
