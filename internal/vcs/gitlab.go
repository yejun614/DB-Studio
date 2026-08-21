package vcs

import (
	"context"
	"fmt"
	"net/http"
	"strings"
)

// gitlabProvider는 GitLab(및 self-hosted) 구현이다.
type gitlabProvider struct{}

func (p *gitlabProvider) Kind() Kind { return GitLab }

// projectPath는 GitLab의 프로젝트 식별자를 URL에 넣을 형태로 만든다.
//
// GitLab은 "group/subgroup/project" 전체 경로를 하나의 경로 요소로 받으므로
// 슬래시까지 인코딩해야 한다(%2F). 이것을 빠뜨리면 404가 나는데 원인을 찾기 어렵다.
func projectPath(repo string) string {
	return queryEscape(strings.Trim(strings.TrimSpace(repo), "/"))
}

func (p *gitlabProvider) Verify(ctx context.Context, cfg Config) (*RepoInfo, error) {
	var out struct {
		PathWithNamespace string `json:"path_with_namespace"`
		DefaultBranch     string `json:"default_branch"`
		WebURL            string `json:"web_url"`
		Visibility        string `json:"visibility"`
		Permissions       struct {
			ProjectAccess *struct {
				AccessLevel int `json:"access_level"`
			} `json:"project_access"`
			GroupAccess *struct {
				AccessLevel int `json:"access_level"`
			} `json:"group_access"`
		} `json:"permissions"`
	}
	endpoint := fmt.Sprintf("%s/projects/%s", apiRoot(cfg), projectPath(cfg.Repo))
	if err := request(ctx, cfg, http.MethodGet, endpoint, nil, &out); err != nil {
		return nil, err
	}

	// GitLab 접근 수준: 30=Developer 이상이면 브랜치를 만들고 푸시할 수 있다.
	var canWrite *bool
	level := 0
	if a := out.Permissions.ProjectAccess; a != nil && a.AccessLevel > level {
		level = a.AccessLevel
	}
	if a := out.Permissions.GroupAccess; a != nil && a.AccessLevel > level {
		level = a.AccessLevel
	}
	if level > 0 {
		w := level >= 30
		canWrite = &w
	}
	return &RepoInfo{
		FullName: out.PathWithNamespace, DefaultBranch: out.DefaultBranch,
		WebURL: out.WebURL, Private: out.Visibility != "public", CanWrite: canWrite,
	}, nil
}

func (p *gitlabProvider) EnsureBranch(ctx context.Context, cfg Config, branch, base string) (bool, error) {
	root := fmt.Sprintf("%s/projects/%s/repository", apiRoot(cfg), projectPath(cfg.Repo))

	var existing struct {
		Name string `json:"name"`
	}
	// GitLab은 브랜치 이름을 하나의 경로 요소로 보므로 슬래시까지 %2F로 인코딩해야 한다.
	// (GitHub/Bitbucket은 반대로 슬래시를 그대로 받는다 — pathKeepSlash 주석 참고)
	err := request(ctx, cfg, http.MethodGet, root+"/branches/"+pathEscape(branch), nil, &existing)
	if err == nil && existing.Name != "" {
		return false, nil
	}
	if ae, ok := asAPIError(err); ok && !ae.NotFound() {
		return false, err
	}

	// GitLab은 브랜치 생성 인자를 쿼리 문자열로 받는다 (본문도 허용하지만
	// 쿼리가 문서화된 방식이다).
	endpoint := fmt.Sprintf("%s/branches?branch=%s&ref=%s",
		root, queryEscape(branch), queryEscape(base))
	if err := request(ctx, cfg, http.MethodPost, endpoint, nil, nil); err != nil {
		return false, err
	}
	return true, nil
}

// PutFiles는 커밋 API 하나로 여러 파일을 원자적으로 올린다.
//
// action은 파일 존재 여부에 따라 create/update가 갈린다. 잘못 고르면 GitLab이
// 400을 반환하므로, 존재를 확인하는 대신 create로 시도하고 실패 시 update로
// 바꾸는 방법도 있지만 호출이 두 배가 된다. 대신 파일별로 존재를 한 번 확인한다 —
// 파일 수가 적고(3~4개) 실패 후 재시도보다 예측 가능하다.
func (p *gitlabProvider) PutFiles(ctx context.Context, cfg Config, branch, message string, files []File) (*Commit, error) {
	if len(files) == 0 {
		return nil, fmt.Errorf("커밋할 파일이 없습니다")
	}
	root := fmt.Sprintf("%s/projects/%s/repository", apiRoot(cfg), projectPath(cfg.Repo))

	type action struct {
		Action   string `json:"action"`
		FilePath string `json:"file_path"`
		Content  string `json:"content"`
	}
	actions := make([]action, 0, len(files))
	for _, f := range files {
		verb := "create"
		if p.fileExists(ctx, cfg, root, branch, f.Path) {
			verb = "update"
		}
		actions = append(actions, action{Action: verb, FilePath: f.Path, Content: f.Content})
	}

	var out struct {
		ID     string `json:"id"`
		WebURL string `json:"web_url"`
	}
	body := map[string]any{
		"branch": branch, "commit_message": message, "actions": actions,
	}
	if err := request(ctx, cfg, http.MethodPost, root+"/commits", body, &out); err != nil {
		return nil, err
	}
	return &Commit{SHA: out.ID, WebURL: out.WebURL}, nil
}

func (p *gitlabProvider) fileExists(ctx context.Context, cfg Config, root, branch, path string) bool {
	endpoint := fmt.Sprintf("%s/files/%s?ref=%s", root, queryEscape(path), queryEscape(branch))
	err := request(ctx, cfg, http.MethodHead, endpoint, nil, nil)
	return err == nil
}

func (p *gitlabProvider) OpenPR(ctx context.Context, cfg Config, req PRRequest) (*PullRequest, error) {
	root := fmt.Sprintf("%s/projects/%s", apiRoot(cfg), projectPath(cfg.Repo))

	var open []struct {
		IID    int    `json:"iid"`
		WebURL string `json:"web_url"`
		State  string `json:"state"`
	}
	listURL := fmt.Sprintf("%s/merge_requests?state=opened&source_branch=%s",
		root, queryEscape(req.SourceBranch))
	if err := request(ctx, cfg, http.MethodGet, listURL, nil, &open); err == nil && len(open) > 0 {
		return &PullRequest{
			Number: open[0].IID, WebURL: open[0].WebURL,
			State: open[0].State, Existing: true,
		}, nil
	}

	var out struct {
		IID    int    `json:"iid"`
		WebURL string `json:"web_url"`
		State  string `json:"state"`
	}
	body := map[string]any{
		"source_branch": req.SourceBranch,
		"target_branch": req.TargetBranch,
		"title":         req.Title,
		"description":   req.Body,
	}
	if err := request(ctx, cfg, http.MethodPost, root+"/merge_requests", body, &out); err != nil {
		return nil, err
	}
	return &PullRequest{Number: out.IID, WebURL: out.WebURL, State: out.State}, nil
}
