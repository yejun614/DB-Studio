// Package vcs는 GitHub/GitLab/Bitbucket에 스키마 변경을 커밋하고 PR/MR을 만든다.
//
// 세 서비스의 API는 모양이 다르다. 특히 "여러 파일을 한 커밋으로 올리는" 방법이
// 제각각이다:
//
//	GitLab    — 커밋 API 하나가 여러 파일 액션을 받는다 (호출 1회)
//	Bitbucket — /src에 multipart로 파일들을 보낸다 (호출 1회)
//	GitHub    — Contents API는 파일당 커밋을 만든다. 한 커밋으로 묶으려면
//	            트리 → 커밋 → ref 갱신을 직접 해야 한다 (호출 5회)
//
// 그래도 한 커밋으로 묶는 이유: up.sql과 down.sql이 다른 커밋에 있으면 리뷰어가
// 반쪽만 보게 되고, 되돌릴 때도 커밋 하나로 처리할 수 없다.
package vcs

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Kind는 서비스 종류다.
type Kind string

const (
	GitHub    Kind = "github"
	GitLab    Kind = "gitlab"
	Bitbucket Kind = "bitbucket"
)

func (k Kind) Valid() bool {
	switch k {
	case GitHub, GitLab, Bitbucket:
		return true
	}
	return false
}

// Label은 화면 표시용 이름이다.
func (k Kind) Label() string {
	switch k {
	case GitHub:
		return "GitHub"
	case GitLab:
		return "GitLab"
	case Bitbucket:
		return "Bitbucket"
	}
	return string(k)
}

// Config는 한 저장소에 접근하기 위한 설정이다.
type Config struct {
	Kind Kind
	// BaseURL은 self-hosted 인스턴스의 주소다. 비어 있으면 공개 SaaS를 쓴다.
	BaseURL string
	// Repo는 서비스별 저장소 식별자다.
	//   GitHub    — owner/repo
	//   GitLab    — group/project (또는 숫자 ID)
	//   Bitbucket — workspace/repo
	Repo string
	// Username은 Bitbucket 앱 비밀번호 인증에 쓴다. 비어 있으면 Bearer 토큰으로 본다.
	Username string
	Token    string
}

// File은 커밋에 담을 파일 하나다.
type File struct {
	Path    string
	Content string
}

// RepoInfo는 저장소 확인 결과다.
type RepoInfo struct {
	FullName      string `json:"fullName"`
	DefaultBranch string `json:"defaultBranch"`
	WebURL        string `json:"webUrl,omitempty"`
	Private       bool   `json:"private"`
	// Permissions는 쓰기 권한 여부다. 확인할 수 없으면 nil이다 —
	// "권한이 없다"와 "알 수 없다"를 구분해야 사용자에게 정확히 말할 수 있다.
	CanWrite *bool `json:"canWrite,omitempty"`
}

// Commit은 커밋 결과다.
type Commit struct {
	SHA    string `json:"sha"`
	WebURL string `json:"webUrl,omitempty"`
}

// PRRequest는 PR/MR 생성 입력이다.
type PRRequest struct {
	SourceBranch string
	TargetBranch string
	Title        string
	Body         string
}

// PullRequest는 PR/MR 결과다.
type PullRequest struct {
	Number int    `json:"number"`
	WebURL string `json:"webUrl"`
	State  string `json:"state,omitempty"`
	// Existing이 true면 이미 있던 PR을 재사용했다.
	Existing bool `json:"existing"`
}

// Provider는 서비스별 구현이 만족해야 하는 인터페이스다.
type Provider interface {
	Kind() Kind

	// Verify는 토큰과 저장소 접근을 확인한다.
	Verify(ctx context.Context, cfg Config) (*RepoInfo, error)

	// EnsureBranch는 base에서 branch를 만든다. 이미 있으면 created=false다.
	EnsureBranch(ctx context.Context, cfg Config, branch, base string) (created bool, err error)

	// PutFiles는 파일들을 한 커밋으로 올린다.
	PutFiles(ctx context.Context, cfg Config, branch, message string, files []File) (*Commit, error)

	// OpenPR은 PR/MR을 만든다. 같은 브랜치의 열린 PR이 이미 있으면 그것을 반환한다.
	OpenPR(ctx context.Context, cfg Config, req PRRequest) (*PullRequest, error)
}

// Get은 종류에 맞는 프로바이더를 반환한다.
func Get(kind Kind) (Provider, error) {
	switch kind {
	case GitHub:
		return &githubProvider{}, nil
	case GitLab:
		return &gitlabProvider{}, nil
	case Bitbucket:
		return &bitbucketProvider{}, nil
	}
	return nil, fmt.Errorf("지원하지 않는 서비스입니다: %s", kind)
}

// ---------- 공통 HTTP ----------

// httpTimeout은 한 요청의 상한이다. Git 서비스가 느려도 앱 요청이 무한정 매달리면 안 된다.
const httpTimeout = 30 * time.Second

// client는 프로바이더 공용 HTTP 클라이언트다.
//
// 리다이렉트를 따르지 않는다: 토큰이 담긴 Authorization 헤더가 다른 호스트로
// 전달될 수 있고, 그것은 자격증명 유출이다. Git 서비스의 정상 API 경로는
// 리다이렉트를 요구하지 않는다.
var client = &http.Client{
	Timeout: httpTimeout,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return errors.New("리다이렉트를 따르지 않습니다 (토큰 유출 방지)")
	},
}

// APIError는 서비스가 돌려준 오류다.
type APIError struct {
	Status  int
	Message string
	Body    string
	URL     string
}

func (e *APIError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("HTTP %d: %s", e.Status, e.Message)
	}
	return fmt.Sprintf("HTTP %d: %s", e.Status, truncate(e.Body, 300))
}

// NotFound는 404를 구분한다. 브랜치 존재 확인처럼 "없음"이 정상인 경우가 있다.
func (e *APIError) NotFound() bool { return e.Status == http.StatusNotFound }

func asAPIError(err error) (*APIError, bool) {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae, true
	}
	return nil, false
}

// ---------- URL 검증 ----------

// ValidateBaseURL은 self-hosted 주소를 검증한다.
//
// http를 사설/루프백 주소에만 허용하는 이유: 토큰을 평문으로 보내는 것을 막아야 하지만,
// 사내망의 self-hosted 인스턴스나 로컬 테스트에서 TLS가 없는 경우는 현실에 존재한다.
// 그 둘을 구분하면 안전한 기본값과 실용성을 함께 얻는다.
//
// 이 함수는 SSRF를 완전히 막지 못한다(DNS는 나중에 바뀔 수 있다). 연동 설정은
// 어드민만 할 수 있고 그들은 이미 DB 자격증명을 다루므로, 이 지점의 신뢰 경계는
// "어드민을 신뢰한다"이며 docs/design/permissions.md에 그렇게 적어 둔다.
func ValidateBaseURL(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil // 공개 SaaS를 쓴다
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("주소를 해석할 수 없습니다: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("주소는 http 또는 https여야 합니다 (받은 값: %s)", u.Scheme)
	}
	if u.Host == "" {
		return errors.New("주소에 호스트가 없습니다")
	}
	if u.Scheme == "http" && !isPrivateHost(u.Hostname()) {
		return errors.New("http는 사설망 주소에만 허용됩니다. 토큰이 평문으로 전송되므로 https를 사용하세요")
	}
	if u.User != nil {
		return errors.New("주소에 자격증명을 넣지 마세요. 토큰은 별도 필드에 입력합니다")
	}
	return nil
}

// isPrivateHost는 사설망/루프백 주소인지 본다.
func isPrivateHost(host string) bool {
	host = strings.ToLower(host)
	if host == "localhost" || strings.HasSuffix(host, ".localhost") ||
		strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") {
		return true
	}
	ip := parseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
}

// ---------- 템플릿 ----------

// TemplateVars는 브랜치/경로 템플릿에 쓸 값이다.
type TemplateVars struct {
	Date        string // 2026-08-13
	Timestamp   string // 20260813T131500Z
	Slug        string // migration-title-slug
	Connection  string
	Env         string
	Version     string
	MigrationID string
}

// Expand는 템플릿의 {name} 자리를 채운다.
//
// text/template을 쓰지 않는 이유: 사용자가 입력하는 문자열이므로 함수 호출이나
// 반복 같은 표현력이 필요 없고, 오히려 파싱 오류로 실패할 여지만 늘어난다.
func Expand(tmpl string, v TemplateVars) string {
	repl := map[string]string{
		"{date}":    v.Date,
		"{ts}":      v.Timestamp,
		"{slug}":    v.Slug,
		"{conn}":    v.Connection,
		"{env}":     v.Env,
		"{version}": v.Version,
		"{id}":      v.MigrationID,
	}
	out := tmpl
	for k, val := range repl {
		out = strings.ReplaceAll(out, k, val)
	}
	return out
}

// Slugify는 제목을 브랜치·파일명에 쓸 수 있는 형태로 바꾼다.
//
// Git 참조 이름 규칙(공백·`~^:?*[`·연속 점·`.lock` 끝 금지)과 파일 경로 안전성을
// 동시에 만족해야 하므로, 허용 문자만 남기는 화이트리스트 방식을 쓴다.
func Slugify(s string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		case r >= 0xAC00 && r <= 0xD7A3, r >= 0x3131 && r <= 0x318E:
			// 한글은 그대로 둔다. Git 참조와 파일명 모두 UTF-8을 허용하며,
			// 한국어 제목을 전부 버리면 브랜치 이름이 의미를 잃는다.
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash && b.Len() > 0 {
				b.WriteRune('-')
				lastDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "change"
	}
	if len(out) > 60 {
		out = strings.Trim(out[:60], "-")
	}
	return out
}

func truncate(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
