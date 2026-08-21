package vcs

import (
	"context"
	"fmt"
	"net/http"
	"strings"
)

// bitbucketProvider는 Bitbucket Cloud 구현이다.
type bitbucketProvider struct{}

func (p *bitbucketProvider) Kind() Kind { return Bitbucket }

func (p *bitbucketProvider) Verify(ctx context.Context, cfg Config) (*RepoInfo, error) {
	workspace, name, err := splitRepo(cfg.Repo)
	if err != nil {
		return nil, err
	}
	var out struct {
		FullName   string `json:"full_name"`
		IsPrivate  bool   `json:"is_private"`
		MainBranch struct {
			Name string `json:"name"`
		} `json:"mainbranch"`
		Links struct {
			HTML struct {
				Href string `json:"href"`
			} `json:"html"`
		} `json:"links"`
	}
	endpoint := fmt.Sprintf("%s/repositories/%s/%s",
		apiRoot(cfg), pathEscape(workspace), pathEscape(name))
	if err := request(ctx, cfg, http.MethodGet, endpoint, nil, &out); err != nil {
		return nil, err
	}
	// Bitbucket의 저장소 응답에는 권한 정보가 없다. 별도 엔드포인트(/permissions)는
	// 추가 스코프를 요구하므로, "알 수 없음"으로 남기고 실제 실패 시점에 알린다.
	return &RepoInfo{
		FullName: out.FullName, DefaultBranch: out.MainBranch.Name,
		WebURL: out.Links.HTML.Href, Private: out.IsPrivate,
	}, nil
}

func (p *bitbucketProvider) EnsureBranch(ctx context.Context, cfg Config, branch, base string) (bool, error) {
	workspace, name, err := splitRepo(cfg.Repo)
	if err != nil {
		return false, err
	}
	root := fmt.Sprintf("%s/repositories/%s/%s",
		apiRoot(cfg), pathEscape(workspace), pathEscape(name))

	var existing struct {
		Name string `json:"name"`
	}
	err = request(ctx, cfg, http.MethodGet, root+"/refs/branches/"+pathKeepSlash(branch), nil, &existing)
	if err == nil && existing.Name != "" {
		return false, nil
	}
	if ae, ok := asAPIError(err); ok && !ae.NotFound() {
		return false, err
	}

	// 기준 브랜치의 커밋 해시를 찾아 그 지점에서 분기한다.
	var baseRef struct {
		Target struct {
			Hash string `json:"hash"`
		} `json:"target"`
	}
	if err := request(ctx, cfg, http.MethodGet,
		root+"/refs/branches/"+pathKeepSlash(base), nil, &baseRef); err != nil {
		if ae, ok := asAPIError(err); ok && ae.NotFound() {
			return false, fmt.Errorf("기준 브랜치 %q 를 찾을 수 없습니다", base)
		}
		return false, err
	}

	body := map[string]any{
		"name":   branch,
		"target": map[string]string{"hash": baseRef.Target.Hash},
	}
	if err := request(ctx, cfg, http.MethodPost, root+"/refs/branches", body, nil); err != nil {
		return false, err
	}
	return true, nil
}

// PutFiles는 /src에 multipart로 파일을 올린다.
//
// Bitbucket은 JSON 대신 form 인코딩을 쓴다. 파일 경로가 폼 필드 이름이 되고,
// message/branch/parents는 일반 필드다. parents를 주면 그 커밋 위에 쌓이므로
// 경쟁 상황에서 남의 커밋을 덮어쓰지 않는다.
func (p *bitbucketProvider) PutFiles(ctx context.Context, cfg Config, branch, message string, files []File) (*Commit, error) {
	if len(files) == 0 {
		return nil, fmt.Errorf("커밋할 파일이 없습니다")
	}
	workspace, name, err := splitRepo(cfg.Repo)
	if err != nil {
		return nil, err
	}
	root := fmt.Sprintf("%s/repositories/%s/%s",
		apiRoot(cfg), pathEscape(workspace), pathEscape(name))

	var head struct {
		Target struct {
			Hash string `json:"hash"`
		} `json:"target"`
	}
	if err := request(ctx, cfg, http.MethodGet,
		root+"/refs/branches/"+pathKeepSlash(branch), nil, &head); err != nil {
		return nil, err
	}

	fields := map[string]string{
		"message": message,
		"branch":  branch,
	}
	if head.Target.Hash != "" {
		fields["parents"] = head.Target.Hash
	}
	// /src는 성공 시 본문 없이 201을 주고, 커밋 위치를 Location 헤더로 알려준다.
	// 해시를 알기 위해 커밋 후 브랜치 head를 다시 읽는다.
	if err := multipartRequest(ctx, cfg, root+"/src", fields, files, nil); err != nil {
		return nil, err
	}

	var after struct {
		Target struct {
			Hash  string `json:"hash"`
			Links struct {
				HTML struct {
					Href string `json:"href"`
				} `json:"html"`
			} `json:"links"`
		} `json:"target"`
	}
	if err := request(ctx, cfg, http.MethodGet,
		root+"/refs/branches/"+pathKeepSlash(branch), nil, &after); err != nil {
		// 커밋은 성공했으므로 해시를 몰라도 실패로 만들지 않는다.
		return &Commit{}, nil
	}
	return &Commit{SHA: after.Target.Hash, WebURL: after.Target.Links.HTML.Href}, nil
}

func (p *bitbucketProvider) OpenPR(ctx context.Context, cfg Config, req PRRequest) (*PullRequest, error) {
	workspace, name, err := splitRepo(cfg.Repo)
	if err != nil {
		return nil, err
	}
	root := fmt.Sprintf("%s/repositories/%s/%s",
		apiRoot(cfg), pathEscape(workspace), pathEscape(name))

	// Bitbucket은 q 파라미터로 필터한다.
	var open struct {
		Values []struct {
			ID    int    `json:"id"`
			State string `json:"state"`
			Links struct {
				HTML struct {
					Href string `json:"href"`
				} `json:"html"`
			} `json:"links"`
		} `json:"values"`
	}
	query := fmt.Sprintf(`source.branch.name="%s" AND state="OPEN"`, req.SourceBranch)
	listURL := fmt.Sprintf("%s/pullrequests?q=%s", root, queryEscape(query))
	if err := request(ctx, cfg, http.MethodGet, listURL, nil, &open); err == nil && len(open.Values) > 0 {
		v := open.Values[0]
		return &PullRequest{
			Number: v.ID, WebURL: v.Links.HTML.Href,
			State: strings.ToLower(v.State), Existing: true,
		}, nil
	}

	var out struct {
		ID    int    `json:"id"`
		State string `json:"state"`
		Links struct {
			HTML struct {
				Href string `json:"href"`
			} `json:"html"`
		} `json:"links"`
	}
	body := map[string]any{
		"title":       req.Title,
		"description": req.Body,
		"source":      map[string]any{"branch": map[string]string{"name": req.SourceBranch}},
		"destination": map[string]any{"branch": map[string]string{"name": req.TargetBranch}},
	}
	if err := request(ctx, cfg, http.MethodPost, root+"/pullrequests", body, &out); err != nil {
		return nil, err
	}
	return &PullRequest{
		Number: out.ID, WebURL: out.Links.HTML.Href, State: strings.ToLower(out.State),
	}, nil
}
