package cloud

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Cloudflare D1 REST 클라이언트.
//
// Workers 바인딩이 아니라 REST(api.cloudflare.com)를 쓰는 이유: 이 앱은 Cloudflare 밖에서
// 도는 단일 바이너리다. REST는 토큰 하나로 어디서나 부를 수 있고, 별도 Worker를 배포해
// 두라고 요구하지 않는다.
//
// 쓰는 것은 /query 하나다. 문장 여러 개는 batch로 한 번에 보낸다 — 변경 백 줄을 백 번의
// 요청으로 보내면 Cloudflare API 한도(5분에 1,200번)를 동기화 하나가 다 쓴다.

// d1BaseURL은 Cloudflare API 주소다. 시험에서 바꿔 끼운다.
var d1BaseURL = "https://api.cloudflare.com/client/v4"

// Stmt는 D1에 보낼 문장 하나다.
//
// 매개변수가 모두 문자열인 이유: REST API가 문자열 배열로 받는다. 정수 컬럼에 "5"를
// 넣어도 SQLite의 컬럼 친화성이 정수로 바꿔 담으므로 뜻은 같다.
type Stmt struct {
	SQL    string   `json:"sql"`
	Params []string `json:"params,omitempty"`
}

// size는 요청 본문에서 이 문장이 차지할 대략의 바이트다(묶음 크기를 정할 때 쓴다).
func (s Stmt) size() int {
	n := len(s.SQL) + 16
	for _, p := range s.Params {
		n += len(p) + 4
	}
	return n
}

// D1Result는 문장 하나의 결과다.
type D1Result struct {
	Results []map[string]any `json:"results"`
	Success bool             `json:"success"`
}

// D1 은 D1 데이터베이스 하나에 대한 클라이언트다.
type D1 struct {
	account  string
	database string
	token    string
	http     *http.Client
}

// NewD1은 클라이언트를 만든다.
func NewD1(account, database, token string) *D1 {
	return &D1{
		account:  strings.TrimSpace(account),
		database: strings.TrimSpace(database),
		token:    strings.TrimSpace(token),
		// 묶음 하나가 수백 줄이어도 D1은 몇 초 안에 끝낸다. 그보다 오래 걸리면 망이
		// 막힌 것이고, 다음 주기에 다시 하면 된다(보내는 것은 모두 다시 보내도 안전하다).
		http: &http.Client{Timeout: 60 * time.Second},
	}
}

// Exec는 문장들을 한 요청으로 보낸다. 결과는 문장 순서와 같다.
func (d *D1) Exec(ctx context.Context, stmts []Stmt) ([]D1Result, error) {
	if len(stmts) == 0 {
		return nil, nil
	}
	var body any = map[string]any{"batch": stmts}
	if len(stmts) == 1 {
		body = stmts[0]
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	endpoint := fmt.Sprintf("%s/accounts/%s/d1/database/%s/query",
		d1BaseURL, url.PathEscape(d.account), url.PathEscape(d.database))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+d.token)
	req.Header.Set("Content-Type", "application/json")

	res, err := d.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Cloudflare API 에 닿지 못했습니다: %w", err)
	}
	defer res.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(res.Body, 64<<20))
	if err != nil {
		return nil, err
	}

	var out struct {
		Result  []D1Result `json:"result"`
		Success bool       `json:"success"`
		Errors  []struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	dec := json.NewDecoder(bytes.NewReader(payload))
	// 정수를 float64로 받으면 큰 값의 끝자리가 바뀐다(rowid·seq).
	dec.UseNumber()
	decodeErr := dec.Decode(&out)

	if res.StatusCode >= 400 || !out.Success {
		msg := ""
		for _, e := range out.Errors {
			if msg != "" {
				msg += "; "
			}
			msg += e.Message
		}
		return nil, d1Error(res.StatusCode, msg)
	}
	if decodeErr != nil {
		return nil, fmt.Errorf("D1 응답을 해석하지 못했습니다: %w", decodeErr)
	}
	return out.Result, nil
}

// d1Error는 오류 응답을 사람 말로 바꾼다. 상태 코드만으로는 "토큰이 틀렸다"와
// "그 DB가 없다"를 구분하기 어렵다.
func d1Error(status int, msg string) error {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("Cloudflare 가 거절했습니다 — API 토큰과 그 토큰의 D1 편집 권한을 확인하세요 (%s)", orStatus(msg, status))
	case http.StatusNotFound:
		return fmt.Errorf("D1 데이터베이스를 찾지 못했습니다 — 계정 ID와 데이터베이스 ID를 확인하세요 (%s)", orStatus(msg, status))
	case http.StatusTooManyRequests:
		return fmt.Errorf("Cloudflare API 요청 한도에 걸렸습니다. 잠시 뒤 다시 보냅니다")
	}
	return fmt.Errorf("D1 요청이 실패했습니다: %s", orStatus(msg, status))
}

func orStatus(msg string, status int) string {
	if strings.TrimSpace(msg) != "" {
		return msg
	}
	return fmt.Sprintf("HTTP %d", status)
}

// isNoSuchTable은 원격에 아직 표가 없다는 오류인지 본다(처음 연결한 D1).
func isNoSuchTable(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no such table")
}

// str은 결과 칸 하나를 문자열로 읽는다.
func str(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case json.Number:
		return x.String()
	default:
		return fmt.Sprint(x)
	}
}

// num은 결과 칸 하나를 정수로 읽는다.
func num(v any) int64 {
	switch x := v.(type) {
	case json.Number:
		n, err := x.Int64()
		if err != nil {
			f, _ := x.Float64()
			return int64(f)
		}
		return n
	case string:
		var n int64
		fmt.Sscan(x, &n)
		return n
	case float64:
		return int64(x)
	case int64:
		return x
	}
	return 0
}
