package macro

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"dbstudio/internal/model"
)

// 매크로에서 외부 API를 호출한다.
//
// 왜 별도 권한(http.call)인가: 셸보다 약해 보이지만, 이것이 있으면 **DB에서 읽은 값을
// 임의의 주소로 보낼 수 있다.** 조회 권한과 결합하면 데이터 반출 통로가 되므로
// "매크로를 쓸 수 있다"와 "외부로 내보낼 수 있다"는 따로 판단해야 한다.
//
// 셸처럼 서버 플래그까지 요구하지는 않는다. 셸은 켜지는 순간 앱이 원격 셸이 되지만,
// HTTP는 이 앱이 이미 하고 있는 일(Git 연동, AI 프로바이더, 아바타 가져오기)과 같은
// 종류의 동작이다. 게이트를 하나 더 두면 실제로 막는 것 없이 설정만 늘어난다.
//
// 대신 **링크로컬 주소는 항상 막는다.** 169.254.169.254 같은 클라우드 메타데이터
// 엔드포인트는 인스턴스 자격증명을 그대로 내주며, 정상적인 매크로가 그곳을 부를 이유가
// 없다. 그 밖의 사설망은 기본 허용이다 — 사내 웹훅을 부르는 것이 이 기능의 주 용도인데
// 막아 두면 쓸 수 없다. 더 좁혀야 하면 -macro-http-allow로 목록을 정한다.

// HTTPConfig는 외부 호출의 한계값이다.
type HTTPConfig struct {
	Timeout time.Duration
	MaxBody int64
	// Allow가 비어 있지 않으면 그 목록에 맞는 대상만 호출할 수 있다.
	// 항목은 호스트 이름, IP, 또는 CIDR이다.
	Allow []string
}

// maxRedirects는 따라갈 리다이렉트 수다.
// 무제한이면 루프에 걸리고, 아예 막으면 흔한 API 주소 대부분이 실패한다.
const maxRedirects = 5

// HTTPResult는 호출 결과다.
type HTTPResult struct {
	Status  int
	Headers map[string]string
	Body    string
	// Truncated는 본문이 상한에서 잘렸음을 뜻한다.
	// 잘린 JSON을 파싱하면 조용히 다른 값이 나오므로 호출부가 알아야 한다.
	Truncated bool
	ElapsedMs float64
}

// HTTPRequest는 호출 요청이다.
type HTTPRequest struct {
	Method  string
	URL     string
	Headers map[string]string
	Body    string
}

// callHTTP는 외부 요청을 보낸다. 권한과 주소 검사가 여기 모여 있다.
func (r *runner) callHTTP(req HTTPRequest) (*HTTPResult, error) {
	if !r.actor.HasPerm(model.PermHTTPCall) {
		return nil, fmt.Errorf("외부 API 호출 권한이 없습니다")
	}
	cfg := r.engine.cfg.HTTP
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	if cfg.MaxBody <= 0 {
		cfg.MaxBody = 1 << 20
	}

	target, err := url.Parse(strings.TrimSpace(req.URL))
	if err != nil || (target.Scheme != "http" && target.Scheme != "https") {
		return nil, fmt.Errorf("http 또는 https 주소만 호출할 수 있습니다: %s", req.URL)
	}

	method := strings.ToUpper(strings.TrimSpace(req.Method))
	if method == "" {
		method = http.MethodGet
	}
	switch method {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch,
		http.MethodDelete, http.MethodHead, http.MethodOptions:
	default:
		return nil, fmt.Errorf("지원하지 않는 메서드입니다: %s", method)
	}

	ctx, cancel := context.WithTimeout(r.ctx, cfg.Timeout)
	defer cancel()

	if err := checkHTTPTarget(ctx, target, cfg); err != nil {
		return nil, err
	}

	var body io.Reader
	if req.Body != "" {
		body = strings.NewReader(req.Body)
	}
	httpReq, err := http.NewRequestWithContext(ctx, method, target.String(), body)
	if err != nil {
		return nil, err
	}
	// 기본 헤더. 받는 쪽 로그에서 무엇이 불렀는지 알 수 있어야 한다.
	httpReq.Header.Set("User-Agent", "dbstudio-macro")
	for k, v := range req.Headers {
		httpReq.Header.Set(k, v)
	}
	if req.Body != "" && httpReq.Header.Get("Content-Type") == "" {
		// 본문이 JSON처럼 생겼으면 그렇게 알린다. 대부분의 API가 이것을 요구하고,
		// 빠뜨렸을 때의 오류(415)는 원인을 짐작하기 어렵다.
		if looksLikeJSON(req.Body) {
			httpReq.Header.Set("Content-Type", "application/json")
		} else {
			httpReq.Header.Set("Content-Type", "text/plain; charset=utf-8")
		}
	}

	client := &http.Client{
		Timeout: cfg.Timeout,
		CheckRedirect: func(rr *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return fmt.Errorf("리다이렉트가 %d회를 넘었습니다", maxRedirects)
			}
			// 리다이렉트 대상도 검사한다. 공개 주소로 시작해 내부로 보내는 것이
			// SSRF의 전형적인 수법이다(아바타 가져오기와 같은 판단).
			return checkHTTPTarget(rr.Context(), rr.URL, cfg)
		},
	}

	start := time.Now()
	res, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("호출 실패: %w", err)
	}
	defer res.Body.Close()

	// 상한보다 1바이트 더 읽어 잘렸는지 판단한다.
	data, err := io.ReadAll(io.LimitReader(res.Body, cfg.MaxBody+1))
	if err != nil {
		return nil, fmt.Errorf("응답을 읽지 못했습니다: %w", err)
	}
	truncated := false
	if int64(len(data)) > cfg.MaxBody {
		data = data[:cfg.MaxBody]
		truncated = true
	}

	headers := make(map[string]string, len(res.Header))
	for k := range res.Header {
		headers[strings.ToLower(k)] = res.Header.Get(k)
	}

	return &HTTPResult{
		Status: res.StatusCode, Headers: headers, Body: string(data),
		Truncated: truncated,
		ElapsedMs: float64(time.Since(start).Microseconds()) / 1000,
	}, nil
}

// checkHTTPTarget은 부를 수 있는 주소인지 확인한다.
func checkHTTPTarget(ctx context.Context, target *url.URL, cfg HTTPConfig) error {
	host := target.Hostname()
	if host == "" {
		return fmt.Errorf("주소에 호스트가 없습니다")
	}

	var resolver net.Resolver
	addrs, err := resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return fmt.Errorf("호스트 이름을 찾을 수 없습니다: %s", host)
	}

	for _, addr := range addrs {
		ip := addr.IP
		// 링크로컬은 언제나 막는다. 클라우드 메타데이터 서비스(169.254.169.254)가
		// 여기 살고, 그것을 부를 수 있으면 인스턴스 자격증명이 그대로 나간다.
		if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
			return fmt.Errorf("링크로컬 주소는 호출할 수 없습니다 (%s)", ip)
		}
		if ip.IsUnspecified() {
			return fmt.Errorf("호출할 수 없는 주소입니다 (%s)", ip)
		}
	}

	if len(cfg.Allow) == 0 {
		return nil
	}
	// 허용 목록이 있으면 호스트 이름이나 해석된 IP 중 하나가 맞아야 한다.
	// 이름으로도 맞춰 보는 이유: 운영자는 대개 "api.slack.com"처럼 이름으로 적는다.
	for _, rule := range cfg.Allow {
		if matchHTTPRule(rule, host, addrs) {
			return nil
		}
	}
	return fmt.Errorf("허용 목록에 없는 대상입니다: %s (-macro-http-allow 확인)", host)
}

func matchHTTPRule(rule, host string, addrs []net.IPAddr) bool {
	rule = strings.TrimSpace(rule)
	if rule == "" {
		return false
	}
	if strings.EqualFold(rule, host) {
		return true
	}
	// 앞에 점이 있으면 하위 도메인 전체를 뜻한다(.example.com).
	if strings.HasPrefix(rule, ".") && strings.HasSuffix(strings.ToLower(host), strings.ToLower(rule)) {
		return true
	}
	if _, network, err := net.ParseCIDR(rule); err == nil {
		for _, addr := range addrs {
			if network.Contains(addr.IP) {
				return true
			}
		}
		return false
	}
	if ip := net.ParseIP(rule); ip != nil {
		for _, addr := range addrs {
			if addr.IP.Equal(ip) {
				return true
			}
		}
	}
	return false
}

func looksLikeJSON(s string) bool {
	t := strings.TrimSpace(s)
	return strings.HasPrefix(t, "{") || strings.HasPrefix(t, "[")
}

// ---------- 노드 ----------

func execHTTP(r *runner, n *Node) (any, string, error) {
	headers, err := parseJSONObject(r.str(n, "headers"))
	if err != nil {
		return nil, "", fmt.Errorf("헤더 JSON: %w", err)
	}
	headerMap := make(map[string]string, len(headers))
	for k, v := range headers {
		headerMap[k] = stringify(v)
	}

	res, err := r.callHTTP(HTTPRequest{
		Method:  r.rawStr(n, "method"),
		URL:     r.str(n, "url"),
		Headers: headerMap,
		Body:    r.str(n, "body"),
	})
	if err != nil {
		return nil, "", err
	}

	level := "info"
	if res.Status >= 400 {
		level = "warn"
	}
	// 호출한 주소와 결과를 실행 로그에 남긴다. 데이터가 밖으로 나가는 동작이므로
	// "언제 어디로 무엇을 보냈는가"가 기록에 있어야 한다.
	r.log(level, n, fmt.Sprintf("%s %s → %d (%.0fms)",
		orDefault(r.rawStr(n, "method"), "GET"), r.str(n, "url"), res.Status, res.ElapsedMs),
		map[string]any{"bytes": len(res.Body), "truncated": res.Truncated})

	if res.Status >= 400 && r.flag(n, "failOnError") {
		return nil, "", fmt.Errorf("HTTP %d: %s", res.Status, truncate(res.Body, 300))
	}
	return httpResultValue(res), PortOut, nil
}

// httpResultValue는 결과를 매크로 변수로 쓸 수 있는 형태로 바꾼다.
func httpResultValue(res *HTTPResult) map[string]any {
	out := map[string]any{
		"status":    float64(res.Status),
		"ok":        res.Status >= 200 && res.Status < 300,
		"body":      res.Body,
		"headers":   toAnyMap(res.Headers),
		"truncated": res.Truncated,
		"elapsedMs": res.ElapsedMs,
	}
	// 본문이 JSON이면 파싱해서 함께 준다. 대부분의 API가 JSON을 돌려주고,
	// 그때마다 json.decode를 부르게 하는 것은 군더더기다.
	// 잘린 본문은 파싱하지 않는다 — 반쪽짜리 JSON에서 나온 값은 틀린 값이다.
	if !res.Truncated {
		var parsed any
		if err := json.Unmarshal([]byte(res.Body), &parsed); err == nil {
			out["json"] = normalizeJSON(parsed)
		}
	}
	return out
}

func toAnyMap(in map[string]string) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}
