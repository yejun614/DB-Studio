package vcs

import (
	"context"
	"fmt"
	"net/http"
)

// githubProvider는 GitHub(및 GitHub Enterprise Server) 구현이다.
type githubProvider struct{}

func (p *githubProvider) Kind() Kind { return GitHub }

func (p *githubProvider) Verify(ctx context.Context, cfg Config) (*RepoInfo, error) {
	owner, name, err := splitRepo(cfg.Repo)
	if err != nil {
		return nil, err
	}
	var out struct {
		FullName      string `json:"full_name"`
		DefaultBranch string `json:"default_branch"`
		HTMLURL       string `json:"html_url"`
		Private       bool   `json:"private"`
		Permissions   struct {
			Push bool `json:"push"`
		} `json:"permissions"`
	}
	root := fmt.Sprintf("%s/repos/%s/%s", apiRoot(cfg), pathEscape(owner), pathEscape(name))
	if err := request(ctx, cfg, http.MethodGet, root, nil, &out); err != nil {
		if ae, ok := asAPIError(err); ok && ae.NotFound() {
			return nil, fmt.Errorf("저장소 %q 를 찾을 수 없습니다. 이름이 틀렸거나 토큰에 "+
				"접근 권한이 없습니다(클래식 토큰은 repo 스코프가 필요합니다)", cfg.Repo)
		}
		return nil, err
	}

	info := &RepoInfo{
		FullName: out.FullName, DefaultBranch: out.DefaultBranch,
		WebURL: out.HTMLURL, Private: out.Private,
	}

	// 저장소 내용을 읽을 수 있는지 **실제로** 확인한다.
	//
	// 위의 저장소 조회는 Metadata 권한만으로 통과한다. 브랜치를 만들려면 그보다
	// 넓은 Contents 권한이 필요하고, 그 차이는 지금까지 첫 푸시에서야 드러났다.
	// 참조 하나를 읽어 보면 그 자리에서 알 수 있다.
	// 기준 브랜치는 저장소가 알려 준 기본 브랜치를 쓴다. 연동에 설정된 기준 브랜치와
	// 다를 수 있지만, 여기서 확인하려는 것은 브랜치가 아니라 "내용을 읽을 수 있는가"다.
	base := out.DefaultBranch
	if base != "" {
		err := request(ctx, cfg, http.MethodGet,
			root+"/git/ref/heads/"+pathKeepSlash(base), nil, nil)
		ae, isAPI := asAPIError(err)
		switch {
		case err == nil:
			// 읽을 수 있다. 쓰기는 여전히 첫 푸시에서 드러난다.
		case isAPI && ae.NotFound():
			// 저장소가 비었거나 기준 브랜치 이름이 다르다. 권한 문제가 아니므로
			// 확인을 실패시키지 않고 알려만 준다.
			info.Notes = append(info.Notes,
				fmt.Sprintf("기준 브랜치 %q 를 찾을 수 없습니다. 빈 저장소이거나 이름이 다릅니다", base))
		default:
			// 403이면 hintFor가 붙인 안내가 그대로 따라온다.
			return nil, fmt.Errorf("저장소 내용을 읽을 수 없습니다: %w", err)
		}
	}

	// permissions는 인증된 사용자에게만 포함된다. 없으면 "알 수 없음"으로 남긴다.
	canWrite := out.Permissions.Push
	info.CanWrite = &canWrite
	if canWrite {
		// 이 값은 **계정** 기준이다. 세분화 토큰은 그보다 좁을 수 있고, 그때
		// 통과한 확인이 오히려 사람을 안심시킨다.
		info.Notes = append(info.Notes,
			"쓰기 권한은 계정 기준으로 보고된 값입니다. 세분화(fine-grained) 토큰이면 "+
				"Contents 가 Read and write 인지 토큰 설정에서 확인하세요")
	}
	return info, nil
}

func (p *githubProvider) EnsureBranch(ctx context.Context, cfg Config, branch, base string) (bool, error) {
	owner, name, err := splitRepo(cfg.Repo)
	if err != nil {
		return false, err
	}
	root := fmt.Sprintf("%s/repos/%s/%s", apiRoot(cfg), pathEscape(owner), pathEscape(name))

	// 이미 있으면 그대로 쓴다. 같은 마이그레이션을 다시 올리는 경우가 정상적으로 있다.
	var existing struct {
		Object struct{ SHA string } `json:"object"`
	}
	err = request(ctx, cfg, http.MethodGet, root+"/git/ref/heads/"+pathKeepSlash(branch), nil, &existing)
	if err == nil && existing.Object.SHA != "" {
		return false, nil
	}
	if ae, ok := asAPIError(err); ok && !ae.NotFound() {
		return false, err
	}

	baseSHA, err := p.headSHA(ctx, cfg, root, base)
	if err != nil {
		return false, err
	}
	body := map[string]string{"ref": "refs/heads/" + branch, "sha": baseSHA}
	if err := request(ctx, cfg, http.MethodPost, root+"/git/refs", body, nil); err != nil {
		return false, err
	}
	return true, nil
}

func (p *githubProvider) headSHA(ctx context.Context, cfg Config, root, branch string) (string, error) {
	var out struct {
		Object struct{ SHA string } `json:"object"`
	}
	err := request(ctx, cfg, http.MethodGet, root+"/git/ref/heads/"+pathKeepSlash(branch), nil, &out)
	if err != nil {
		if ae, ok := asAPIError(err); ok && ae.NotFound() {
			return "", fmt.Errorf("기준 브랜치 %q 를 찾을 수 없습니다", branch)
		}
		return "", err
	}
	if out.Object.SHA == "" {
		return "", fmt.Errorf("기준 브랜치 %q 의 커밋을 확인할 수 없습니다", branch)
	}
	return out.Object.SHA, nil
}

// PutFiles는 트리 → 커밋 → ref 갱신으로 여러 파일을 한 커밋에 담는다.
//
// Contents API(PUT /contents/{path})를 쓰지 않는 이유: 파일당 커밋이 하나씩 생겨
// up.sql과 down.sql이 다른 커밋에 흩어진다. 리뷰와 되돌리기가 모두 불편해진다.
func (p *githubProvider) PutFiles(ctx context.Context, cfg Config, branch, message string, files []File) (*Commit, error) {
	if len(files) == 0 {
		return nil, fmt.Errorf("커밋할 파일이 없습니다")
	}
	owner, name, err := splitRepo(cfg.Repo)
	if err != nil {
		return nil, err
	}
	root := fmt.Sprintf("%s/repos/%s/%s", apiRoot(cfg), pathEscape(owner), pathEscape(name))

	parentSHA, err := p.headSHA(ctx, cfg, root, branch)
	if err != nil {
		return nil, err
	}

	// 부모 커밋의 트리를 기준으로 새 트리를 만든다. base_tree를 주지 않으면
	// 나머지 파일이 전부 삭제된 트리가 된다.
	var parent struct {
		Tree struct{ SHA string } `json:"tree"`
	}
	if err := request(ctx, cfg, http.MethodGet, root+"/git/commits/"+pathKeepSlash(parentSHA), nil, &parent); err != nil {
		return nil, err
	}

	type treeEntry struct {
		Path    string `json:"path"`
		Mode    string `json:"mode"`
		Type    string `json:"type"`
		Content string `json:"content"`
	}
	entries := make([]treeEntry, 0, len(files))
	for _, f := range files {
		entries = append(entries, treeEntry{
			Path: f.Path, Mode: "100644", Type: "blob", Content: f.Content,
		})
	}
	var tree struct{ SHA string }
	if err := request(ctx, cfg, http.MethodPost, root+"/git/trees",
		map[string]any{"base_tree": parent.Tree.SHA, "tree": entries}, &tree); err != nil {
		return nil, err
	}

	var commit struct {
		SHA     string `json:"sha"`
		HTMLURL string `json:"html_url"`
	}
	if err := request(ctx, cfg, http.MethodPost, root+"/git/commits",
		map[string]any{"message": message, "tree": tree.SHA, "parents": []string{parentSHA}},
		&commit); err != nil {
		return nil, err
	}

	// force는 쓰지 않는다. 그 사이 누군가 브랜치에 커밋했다면 실패해야 하며,
	// 강제로 덮어쓰면 남의 커밋이 사라진다.
	if err := request(ctx, cfg, http.MethodPatch, root+"/git/refs/heads/"+pathKeepSlash(branch),
		map[string]any{"sha": commit.SHA, "force": false}, nil); err != nil {
		return nil, err
	}
	return &Commit{SHA: commit.SHA, WebURL: commit.HTMLURL}, nil
}

func (p *githubProvider) OpenPR(ctx context.Context, cfg Config, req PRRequest) (*PullRequest, error) {
	owner, name, err := splitRepo(cfg.Repo)
	if err != nil {
		return nil, err
	}
	root := fmt.Sprintf("%s/repos/%s/%s", apiRoot(cfg), pathEscape(owner), pathEscape(name))

	// 같은 브랜치의 열린 PR이 이미 있으면 그것을 쓴다. 다시 만들려 하면
	// GitHub이 422로 거부하는데, 사용자에게는 "이미 열려 있다"가 정답이다.
	var open []struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
		State   string `json:"state"`
	}
	listURL := fmt.Sprintf("%s/pulls?state=open&head=%s:%s",
		root, queryEscape(owner), queryEscape(req.SourceBranch))
	if err := request(ctx, cfg, http.MethodGet, listURL, nil, &open); err == nil && len(open) > 0 {
		return &PullRequest{
			Number: open[0].Number, WebURL: open[0].HTMLURL,
			State: open[0].State, Existing: true,
		}, nil
	}

	var out struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
		State   string `json:"state"`
	}
	body := map[string]any{
		"title": req.Title, "body": req.Body,
		"head": req.SourceBranch, "base": req.TargetBranch,
	}
	if err := request(ctx, cfg, http.MethodPost, root+"/pulls", body, &out); err != nil {
		return nil, err
	}
	return &PullRequest{Number: out.Number, WebURL: out.HTMLURL, State: out.State}, nil
}
