package vcs

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// maxBody는 응답을 읽을 상한이다. 잘못된 주소를 가리켰을 때 거대한 응답으로
// 메모리를 소모하지 않게 한다.
const maxBody = 4 << 20

// request는 JSON API 호출을 수행한다.
//
// out이 nil이면 응답 본문을 버린다. 오류 응답은 *APIError로 반환하며, 서비스별
// 오류 메시지 필드를 찾아 사람이 읽을 수 있는 문장을 담는다 — "HTTP 422"만으로는
// 사용자가 무엇을 고쳐야 할지 알 수 없다.
func request(ctx context.Context, cfg Config, method, endpoint string, body, out any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal request: %w", err)
		}
		reader = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	applyAuth(req, cfg)

	return doRequest(req, cfg.Kind, endpoint, out)
}

// multipartRequest는 Bitbucket의 /src처럼 multipart/form-data를 요구하는 호출을 수행한다.
func multipartRequest(ctx context.Context, cfg Config, endpoint string, fields map[string]string, files []File, out any) error {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for k, v := range fields {
		if err := w.WriteField(k, v); err != nil {
			return fmt.Errorf("write field %s: %w", k, err)
		}
	}
	for _, f := range files {
		// Bitbucket은 파일 경로를 필드 이름으로 쓴다 (파일 이름이 아니라).
		part, err := w.CreateFormFile(f.Path, f.Path)
		if err != nil {
			return fmt.Errorf("create part %s: %w", f.Path, err)
		}
		if _, err := part.Write([]byte(f.Content)); err != nil {
			return fmt.Errorf("write part %s: %w", f.Path, err)
		}
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("close multipart: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, &buf)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Accept", "application/json")
	applyAuth(req, cfg)

	return doRequest(req, cfg.Kind, endpoint, out)
}

func doRequest(req *http.Request, kind Kind, endpoint string, out any) error {
	res, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("요청 실패: %w", err)
	}
	defer res.Body.Close()

	data, err := io.ReadAll(io.LimitReader(res.Body, maxBody))
	if err != nil {
		return fmt.Errorf("응답을 읽지 못했습니다: %w", err)
	}

	if res.StatusCode < 200 || res.StatusCode >= 300 {
		message := extractMessage(data)
		return &APIError{
			Status:  res.StatusCode,
			Message: message,
			Body:    string(data),
			URL:     endpoint,
			Hint:    hintFor(kind, res.StatusCode, message),
		}
	}
	if out == nil || len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("응답을 해석하지 못했습니다: %w (본문: %s)", err, truncate(string(data), 200))
	}
	return nil
}

// applyAuth는 서비스별 인증 헤더를 붙인다.
func applyAuth(req *http.Request, cfg Config) {
	switch cfg.Kind {
	case GitLab:
		// GitLab은 개인 액세스 토큰을 전용 헤더로 받는다. Bearer도 되지만
		// 그것은 OAuth 토큰용이고, 사용자가 등록하는 것은 보통 PAT다.
		req.Header.Set("PRIVATE-TOKEN", cfg.Token)
	case Bitbucket:
		// 앱 비밀번호는 Basic 인증이고, 액세스 토큰은 Bearer다.
		// 사용자 이름이 있으면 앱 비밀번호로 판단한다.
		if cfg.Username != "" {
			req.Header.Set("Authorization", "Basic "+basicAuth(cfg.Username, cfg.Token))
			return
		}
		req.Header.Set("Authorization", "Bearer "+cfg.Token)
	default:
		req.Header.Set("Authorization", "Bearer "+cfg.Token)
		// GitHub은 API 버전을 헤더로 고정할 수 있다. 고정하지 않으면 기본 버전이
		// 바뀔 때 응답 형태가 달라질 수 있다.
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	}
}

func basicAuth(user, pass string) string {
	return base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
}

// extractMessage는 서비스별 오류 메시지 필드를 찾는다.
//
// GitHub: {"message": "...", "errors": [...]}
// GitLab: {"message": "..."} 또는 {"error": "..."} 또는 {"message": {"base": ["..."]}}
// Bitbucket: {"error": {"message": "..."}}
func extractMessage(data []byte) string {
	var probe map[string]any
	if err := json.Unmarshal(data, &probe); err != nil {
		return ""
	}
	if s, ok := probe["message"].(string); ok && s != "" {
		return withDetails(s, probe)
	}
	if s, ok := probe["error"].(string); ok && s != "" {
		return s
	}
	if m, ok := probe["error"].(map[string]any); ok {
		if s, ok := m["message"].(string); ok {
			return s
		}
	}
	// GitLab은 검증 오류를 {"message": {"base": ["..."]}} 형태로 준다.
	if m, ok := probe["message"].(map[string]any); ok {
		parts := []string{}
		for k, v := range m {
			switch vv := v.(type) {
			case string:
				parts = append(parts, k+": "+vv)
			case []any:
				for _, item := range vv {
					if s, ok := item.(string); ok {
						parts = append(parts, s)
					}
				}
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, "; ")
		}
	}
	return ""
}

// withDetails는 GitHub의 errors 배열을 메시지에 덧붙인다.
// "Validation Failed"만 보여주면 무엇이 잘못됐는지 알 수 없다.
func withDetails(msg string, probe map[string]any) string {
	raw, ok := probe["errors"].([]any)
	if !ok || len(raw) == 0 {
		return msg
	}
	parts := []string{}
	for _, item := range raw {
		switch v := item.(type) {
		case string:
			parts = append(parts, v)
		case map[string]any:
			if s, ok := v["message"].(string); ok && s != "" {
				parts = append(parts, s)
				continue
			}
			field, _ := v["field"].(string)
			code, _ := v["code"].(string)
			if field != "" || code != "" {
				parts = append(parts, strings.TrimSpace(field+" "+code))
			}
		}
	}
	if len(parts) == 0 {
		return msg
	}
	return msg + " (" + strings.Join(parts, "; ") + ")"
}

// apiRoot는 서비스별 API 기준 주소를 만든다.
func apiRoot(cfg Config) string {
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	switch cfg.Kind {
	case GitHub:
		if base == "" {
			return "https://api.github.com"
		}
		// GitHub Enterprise Server의 API는 /api/v3 아래에 있다.
		// 사용자가 이미 그 경로를 넣었으면 중복해서 붙이지 않는다.
		if strings.HasSuffix(base, "/api/v3") {
			return base
		}
		return base + "/api/v3"
	case GitLab:
		if base == "" {
			return "https://gitlab.com/api/v4"
		}
		if strings.HasSuffix(base, "/api/v4") {
			return base
		}
		return base + "/api/v4"
	case Bitbucket:
		if base == "" {
			return "https://api.bitbucket.org/2.0"
		}
		if strings.HasSuffix(base, "/2.0") {
			return base
		}
		return base + "/2.0"
	}
	return base
}

// splitRepo는 "owner/repo" 형태를 나눈다.
func splitRepo(repo string) (string, string, error) {
	repo = strings.Trim(strings.TrimSpace(repo), "/")
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" {
		return "", "", fmt.Errorf("저장소는 owner/repo 형식이어야 합니다 (받은 값: %q)", repo)
	}
	// GitLab의 중첩 그룹(group/subgroup/project)은 마지막 요소가 프로젝트다.
	if strings.Contains(name, "/") {
		idx := strings.LastIndex(name, "/")
		owner = owner + "/" + name[:idx]
		name = name[idx+1:]
	}
	return owner, name, nil
}

func parseIP(host string) net.IP { return net.ParseIP(host) }

// pathEscape는 경로 요소 하나를 인코딩한다 (슬래시도 %2F로).
func pathEscape(s string) string { return url.PathEscape(s) }

// pathKeepSlash는 경로에 쓸 수 없는 문자만 인코딩하고 슬래시는 남긴다.
//
// 브랜치 이름을 URL에 넣는 방식이 서비스마다 다르다:
//
//	GitLab    — 브랜치 이름을 하나의 경로 요소로 보므로 슬래시를 %2F로 인코딩해야 한다
//	GitHub    — /git/ref/ 뒤의 나머지 전체를 ref로 해석한다. 슬래시를 그대로 둬야 하며,
//	            %2F로 보내면 매칭에 실패하거나 프록시가 거부할 수 있다
//	Bitbucket — refs/branches/{name} 에서 슬래시를 그대로 받는다
//
// 그래서 GitLab만 pathEscape를 쓰고 나머지는 이 함수를 쓴다.
func pathKeepSlash(s string) string {
	u := &url.URL{Path: s}
	return u.EscapedPath()
}

// queryEscape는 쿼리 값을 인코딩한다.
func queryEscape(s string) string { return url.QueryEscape(s) }
