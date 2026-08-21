// Package opsapi는 운영 대상(스토리지 클러스터·메시지 브로커)의 관리 API를 부를 때
// 공통으로 쓰는 어휘와 HTTP 도구다.
//
// 왜 따로 두는가: 하둡·Ceph·RabbitMQ·Kafka는 서로 다른 시스템이지만, 화면과 이벤트가
// 물어보는 것은 같다 — "지금 괜찮은가(Health)", "무엇을 알아야 하는가(Fact)". 그 답을
// 종류마다 다른 모양으로 만들면 화면이 종류 수만큼의 렌더러를 갖게 되고, 임계값 판정도
// 종류마다 달라진다. 번역은 각 클라이언트가 한 번 하고, 그 뒤로는 모두 같은 말을 쓴다.
package opsapi

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"dbstudio/internal/model"
)

// 상태 등급.
const (
	HealthOK      = "ok"
	HealthWarn    = "warn"
	HealthError   = "error"
	HealthUnknown = "unknown"
)

// Health는 대상의 상태 한 줄이다.
//
// 문자열이 아니라 등급으로 두는 이유: "HEALTH_WARN"과 "메모리 알람"과 "세이프모드"는 서로
// 다른 말이지만 화면과 이벤트에는 같은 뜻(주의)으로 보여야 한다.
type Health struct {
	Level   string   `json:"level"` // ok | warn | error | unknown
	Summary string   `json:"summary"`
	Checks  []string `json:"checks,omitempty"`
}

// Score는 이벤트 임계값 비교에 쓰는 숫자다(0 정상 → 2 심각, -1 모름).
func (h Health) Score() float64 {
	switch h.Level {
	case HealthOK:
		return 0
	case HealthWarn:
		return 1
	case HealthError:
		return 2
	}
	return -1
}

// Fact는 개요에 한 줄로 들어가는 이름·값 쌍이다. Level이 있으면 화면이 강조한다.
type Fact struct {
	Label string `json:"label"`
	Value string `json:"value"`
	Level string `json:"level,omitempty"` // "" | warn | error
}

// LevelIf는 조건이 참일 때만 강조 등급을 붙인다.
func LevelIf(cond bool, level string) string {
	if cond {
		return level
	}
	return ""
}

// Config는 관리 API 접속 설정이다. 커넥션과 자격증명에서 만든다.
type Config struct {
	Scheme   string // http | https
	Host     string
	Port     int
	User     string
	Password string
	// Insecure는 자체 서명 인증서를 허용한다. 사내 클러스터에서 흔하다.
	Insecure bool
	// Extra는 종류별 부가 설정이다(예: 하둡의 yarn_url, Kafka의 sasl).
	Extra map[string]string
	// Timeout은 한 번의 호출 상한이다.
	Timeout time.Duration
}

// ConfigFrom은 커넥션과 자격증명에서 설정을 만든다.
func ConfigFrom(conn *model.Connection, secret *model.Secret, defaultPort int) Config {
	cfg := Config{
		Scheme:  "http",
		Host:    strings.TrimSpace(conn.Host),
		Port:    conn.Port,
		Extra:   map[string]string{},
		Timeout: 20 * time.Second,
	}
	if cfg.Port == 0 {
		cfg.Port = defaultPort
	}
	for k, v := range conn.Options {
		cfg.Extra[k] = v
	}
	if s := strings.ToLower(strings.TrimSpace(cfg.Extra["scheme"])); s == "https" || s == "http" {
		cfg.Scheme = s
	}
	if b, err := strconv.ParseBool(cfg.Extra["insecure"]); err == nil {
		cfg.Insecure = b
	}
	if secret != nil {
		cfg.User = secret.Username
		cfg.Password = secret.Password
	}
	return cfg
}

// BaseURL은 관리 API 주소다.
func (c Config) BaseURL() string {
	return fmt.Sprintf("%s://%s:%d", c.Scheme, c.Host, c.Port)
}

// Opt는 부가 설정 하나를 읽는다.
func (c Config) Opt(key, def string) string {
	if v, ok := c.Extra[key]; ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return def
}

// HTTPClient는 설정에 맞는 클라이언트를 만든다.
func (c Config) HTTPClient() *http.Client {
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			// 사내 시스템은 자체 서명 인증서를 쓰는 경우가 많다. 기본값은 검증이고,
			// 끄는 것은 사용자가 옵션으로 명시할 때뿐이다.
			InsecureSkipVerify: c.Insecure,
		},
		IdleConnTimeout: 30 * time.Second,
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	return &http.Client{Transport: tr, Timeout: timeout}
}

// Validate는 접속 정보의 형식을 본다. 네트워크는 건드리지 않는다.
func (c Config) Validate() error {
	if strings.TrimSpace(c.Host) == "" {
		return fmt.Errorf("호스트를 입력하세요")
	}
	if c.Port <= 0 || c.Port > 65535 {
		return fmt.Errorf("포트가 올바르지 않습니다: %d", c.Port)
	}
	return nil
}

// HTTPError는 관리 API가 거절한 결과다.
type HTTPError struct {
	Status int
	Body   string
	URL    string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("%d 응답: %s", e.Status, Snippet(e.Body))
}

// DoJSON은 JSON 응답을 받아 온다.
//
// 오류에 상태 코드와 본문 앞부분을 함께 담는 이유: 이 API들은 실패 사유를 본문에 적는다
// (하둡은 RemoteException, Ceph는 detail, RabbitMQ는 reason). 상태 코드만 남기면
// "403"만 보이고 권한 문제인지 인증 문제인지 알 수 없다.
func DoJSON(ctx context.Context, client *http.Client, req *http.Request, out any) error {
	res, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%s 호출 실패: %w", req.URL.Host, err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("응답을 읽지 못했습니다: %w", err)
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return &HTTPError{Status: res.StatusCode, Body: string(body), URL: req.URL.String()}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("응답이 JSON이 아닙니다 (%s): %s", req.URL.Path, Snippet(string(body)))
	}
	return nil
}

// JoinURL은 base에 경로와 질의를 붙인다.
func JoinURL(base, path string, query url.Values) string {
	u := strings.TrimRight(base, "/") + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	return u
}

// Snippet은 오류 본문을 한 줄로 줄인다.
func Snippet(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) > 300 {
		return s[:300] + "…"
	}
	if s == "" {
		return "(본문 없음)"
	}
	return s
}

// HumanBytes는 바이트를 사람이 읽는 단위로 만든다.
func HumanBytes(v int64) string {
	const unit = 1024
	if v < unit {
		return fmt.Sprintf("%d B", v)
	}
	div, exp := int64(unit), 0
	for n := v / unit; n >= unit && exp < 5; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(v)/float64(div), "KMGTPE"[exp])
}

// HumanCount는 개수를 사람이 읽는 단위로 만든다(1.2만, 3.4억).
//
// 메시지 수와 랙은 자릿수가 커서 그대로 쓰면 읽는 데 시간이 걸린다. 화면에서 필요한 것은
// "정확히 몇 개인가"가 아니라 "자릿수가 몇인가"다.
func HumanCount(v int64) string {
	switch {
	case v >= 100_000_000:
		return fmt.Sprintf("%.1f억", float64(v)/100_000_000)
	case v >= 10_000:
		return fmt.Sprintf("%.1f만", float64(v)/10_000)
	default:
		return strconv.FormatInt(v, 10)
	}
}
