package vcs

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ---------- 하네스 ----------

// recorded는 서버가 받은 요청 하나다.
type recorded struct {
	Method  string
	Path    string
	Query   string
	Headers http.Header
	Body    string
}

// fakeService는 Git 서비스를 흉내내는 서버다.
//
// 실제 GitHub/GitLab/Bitbucket에 붙여 테스트할 수 없으므로, 각 서비스의 API 계약을
// "내가 이해한 대로" 코드에 적고 그 이해가 구현과 일치하는지 확인한다. 이 테스트가
// 잡아내는 것은 경로·헤더·본문 형식·호출 순서의 실수이며, 실제 서비스와의 차이는
// 문서를 다시 읽어 이 파일을 고치는 방식으로 반영한다.
type fakeService struct {
	t        *testing.T
	server   *httptest.Server
	requests []recorded
	// routes는 "METHOD /path" → 응답 핸들러다.
	routes map[string]func(w http.ResponseWriter, r *http.Request)
	// missing은 404를 돌려줄 경로다 (브랜치 없음 등을 재현한다).
	missing map[string]bool
}

func newFake(t *testing.T) *fakeService {
	f := &fakeService{
		t:       t,
		routes:  map[string]func(http.ResponseWriter, *http.Request){},
		missing: map[string]bool{},
	}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		// EscapedPath를 쓰는 이유: GitLab은 프로젝트 경로를 %2F로 인코딩한 하나의
		// 경로 요소로 받는다. r.URL.Path는 디코딩된 값이므로 그것으로 라우팅하면
		// "group%2Fsub%2Fproj"와 "group/sub/proj"를 구분할 수 없고, 클라이언트가
		// 인코딩을 빠뜨려도 테스트가 통과해버린다.
		path := r.URL.EscapedPath()
		f.requests = append(f.requests, recorded{
			Method: r.Method, Path: path, Query: r.URL.RawQuery,
			Headers: r.Header.Clone(), Body: string(body),
		})
		key := r.Method + " " + path
		if f.missing[key] {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Not Found"}`))
			return
		}
		if h, ok := f.routes[key]; ok {
			// 핸들러가 본문을 다시 읽을 수 있게 되돌려준다.
			r.Body = io.NopCloser(strings.NewReader(string(body)))
			h(w, r)
			return
		}
		f.t.Errorf("예상하지 못한 요청: %s %s?%s", r.Method, path, r.URL.RawQuery)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeService) on(method, path string, status int, response string) {
	f.routes[method+" "+path] = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if response != "" {
			_, _ = w.Write([]byte(response))
		}
	}
}

func (f *fakeService) notFound(method, path string) {
	f.missing[method+" "+path] = true
}

// find는 기록된 요청 중 첫 번째 일치 항목을 반환한다.
func (f *fakeService) find(method, path string) *recorded {
	for i := range f.requests {
		if f.requests[i].Method == method && f.requests[i].Path == path {
			return &f.requests[i]
		}
	}
	f.t.Fatalf("요청을 찾을 수 없습니다: %s %s\n기록: %s", method, path, f.summary())
	return nil
}

func (f *fakeService) summary() string {
	out := []string{}
	for _, r := range f.requests {
		out = append(out, fmt.Sprintf("%s %s?%s", r.Method, r.Path, r.Query))
	}
	return strings.Join(out, "\n  ")
}

func (f *fakeService) sequence() []string {
	out := []string{}
	for _, r := range f.requests {
		out = append(out, r.Method+" "+r.Path)
	}
	return out
}

func decodeBody(t *testing.T, raw string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("본문을 해석할 수 없습니다: %v (%s)", err, raw)
	}
	return out
}

var testFiles = []File{
	{Path: "migrations/20260813_add_col.up.sql", Content: "ALTER TABLE t ADD COLUMN c INT;\n"},
	{Path: "migrations/20260813_add_col.down.sql", Content: "ALTER TABLE t DROP COLUMN c;\n"},
	{Path: "migrations/20260813_add_col.schema.json", Content: `{"dialect":"mysql"}`},
}

// ---------- GitHub ----------

func TestGitHubVerify(t *testing.T) {
	f := newFake(t)
	f.on("GET", "/api/v3/repos/acme/db", 200, `{
		"full_name":"acme/db","default_branch":"main","html_url":"https://git.example.com/acme/db",
		"private":true,"permissions":{"push":true}}`)

	cfg := Config{Kind: GitHub, BaseURL: f.server.URL, Repo: "acme/db", Token: "tok"}
	p, _ := Get(GitHub)
	info, err := p.Verify(context.Background(), cfg)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if info.FullName != "acme/db" || info.DefaultBranch != "main" || !info.Private {
		t.Errorf("info = %+v", info)
	}
	if info.CanWrite == nil || !*info.CanWrite {
		t.Error("쓰기 권한이 반영되지 않았습니다")
	}
	req := f.find("GET", "/api/v3/repos/acme/db")
	if got := req.Headers.Get("Authorization"); got != "Bearer tok" {
		t.Errorf("Authorization = %q", got)
	}
	if got := req.Headers.Get("X-GitHub-Api-Version"); got == "" {
		t.Error("API 버전 헤더가 없습니다")
	}
}

// GitHub은 여러 파일을 한 커밋으로 올리려면 트리→커밋→ref 순서를 직접 밟아야 한다.
// 이 순서와 base_tree 지정이 틀리면 나머지 파일이 삭제된 커밋이 만들어진다.
func TestGitHubPutFilesSingleCommit(t *testing.T) {
	f := newFake(t)
	root := "/api/v3/repos/acme/db"
	f.on("GET", root+"/git/ref/heads/schema/x", 200, `{"object":{"sha":"parent-sha"}}`)
	f.on("GET", root+"/git/commits/parent-sha", 200, `{"sha":"parent-sha","tree":{"sha":"base-tree"}}`)
	f.on("POST", root+"/git/trees", 201, `{"sha":"new-tree"}`)
	f.on("POST", root+"/git/commits", 201, `{"sha":"new-commit","html_url":"https://git/commit/new"}`)
	f.on("PATCH", root+"/git/refs/heads/schema/x", 200, `{"object":{"sha":"new-commit"}}`)

	cfg := Config{Kind: GitHub, BaseURL: f.server.URL, Repo: "acme/db", Token: "tok"}
	p, _ := Get(GitHub)
	commit, err := p.PutFiles(context.Background(), cfg, "schema/x", "커밋 메시지", testFiles)
	if err != nil {
		t.Fatalf("put files: %v", err)
	}
	if commit.SHA != "new-commit" {
		t.Errorf("커밋 SHA = %q", commit.SHA)
	}

	// 트리 요청이 base_tree와 모든 파일을 담아야 한다.
	tree := decodeBody(t, f.find("POST", root+"/git/trees").Body)
	if tree["base_tree"] != "base-tree" {
		t.Errorf("base_tree = %v (없으면 다른 파일이 모두 삭제된다)", tree["base_tree"])
	}
	entries, _ := tree["tree"].([]any)
	if len(entries) != len(testFiles) {
		t.Fatalf("트리 항목 수 = %d, 기대값 %d", len(entries), len(testFiles))
	}
	first, _ := entries[0].(map[string]any)
	if first["mode"] != "100644" || first["type"] != "blob" {
		t.Errorf("트리 항목 = %+v", first)
	}
	if first["content"] != testFiles[0].Content {
		t.Errorf("파일 내용이 다릅니다: %v", first["content"])
	}

	// 커밋은 부모를 가리켜야 한다.
	commitBody := decodeBody(t, f.find("POST", root+"/git/commits").Body)
	parents, _ := commitBody["parents"].([]any)
	if len(parents) != 1 || parents[0] != "parent-sha" {
		t.Errorf("parents = %v", parents)
	}

	// ref 갱신은 force를 쓰지 않아야 한다 (남의 커밋을 덮어쓰면 안 된다).
	refBody := decodeBody(t, f.find("PATCH", root+"/git/refs/heads/schema/x").Body)
	if refBody["force"] != false {
		t.Errorf("force = %v, false 여야 합니다", refBody["force"])
	}

	want := []string{
		"GET " + root + "/git/ref/heads/schema/x",
		"GET " + root + "/git/commits/parent-sha",
		"POST " + root + "/git/trees",
		"POST " + root + "/git/commits",
		"PATCH " + root + "/git/refs/heads/schema/x",
	}
	if got := f.sequence(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("호출 순서가 다릅니다:\n  실제: %v\n  기대: %v", got, want)
	}
}

func TestGitHubEnsureBranch(t *testing.T) {
	root := "/api/v3/repos/acme/db"

	t.Run("새로 만든다", func(t *testing.T) {
		f := newFake(t)
		f.notFound("GET", root+"/git/ref/heads/schema/new")
		f.on("GET", root+"/git/ref/heads/main", 200, `{"object":{"sha":"main-sha"}}`)
		f.on("POST", root+"/git/refs", 201, `{"ref":"refs/heads/schema/new"}`)

		cfg := Config{Kind: GitHub, BaseURL: f.server.URL, Repo: "acme/db", Token: "t"}
		p, _ := Get(GitHub)
		created, err := p.EnsureBranch(context.Background(), cfg, "schema/new", "main")
		if err != nil {
			t.Fatalf("ensure: %v", err)
		}
		if !created {
			t.Error("created = false")
		}
		body := decodeBody(t, f.find("POST", root+"/git/refs").Body)
		if body["ref"] != "refs/heads/schema/new" || body["sha"] != "main-sha" {
			t.Errorf("본문 = %+v", body)
		}
	})

	t.Run("이미 있으면 그대로 쓴다", func(t *testing.T) {
		f := newFake(t)
		f.on("GET", root+"/git/ref/heads/schema/dup", 200, `{"object":{"sha":"exists"}}`)

		cfg := Config{Kind: GitHub, BaseURL: f.server.URL, Repo: "acme/db", Token: "t"}
		p, _ := Get(GitHub)
		created, err := p.EnsureBranch(context.Background(), cfg, "schema/dup", "main")
		if err != nil {
			t.Fatalf("ensure: %v", err)
		}
		if created {
			t.Error("이미 있는 브랜치를 새로 만들었다고 보고했습니다")
		}
		if len(f.requests) != 1 {
			t.Errorf("요청 수 = %d, 존재 확인 한 번이면 충분합니다", len(f.requests))
		}
	})

	t.Run("기준 브랜치가 없으면 이유를 설명한다", func(t *testing.T) {
		f := newFake(t)
		f.notFound("GET", root+"/git/ref/heads/schema/x")
		f.notFound("GET", root+"/git/ref/heads/nope")

		cfg := Config{Kind: GitHub, BaseURL: f.server.URL, Repo: "acme/db", Token: "t"}
		p, _ := Get(GitHub)
		_, err := p.EnsureBranch(context.Background(), cfg, "schema/x", "nope")
		if err == nil {
			t.Fatal("없는 기준 브랜치로 성공했습니다")
		}
		if !strings.Contains(err.Error(), "기준 브랜치") {
			t.Errorf("오류 메시지가 원인을 설명하지 않습니다: %v", err)
		}
	})
}

// 같은 브랜치에 이미 열린 PR이 있으면 새로 만들지 않고 그것을 알려야 한다.
// 다시 만들려 하면 GitHub이 422로 거부하는데, 사용자에게는 오류가 아니다.
func TestGitHubOpenPRReusesExisting(t *testing.T) {
	f := newFake(t)
	root := "/api/v3/repos/acme/db"
	f.on("GET", root+"/pulls", 200,
		`[{"number":42,"html_url":"https://git/pr/42","state":"open"}]`)

	cfg := Config{Kind: GitHub, BaseURL: f.server.URL, Repo: "acme/db", Token: "t"}
	p, _ := Get(GitHub)
	pr, err := p.OpenPR(context.Background(), cfg, PRRequest{
		SourceBranch: "schema/x", TargetBranch: "main", Title: "제목",
	})
	if err != nil {
		t.Fatalf("open pr: %v", err)
	}
	if pr.Number != 42 || !pr.Existing {
		t.Errorf("pr = %+v", pr)
	}
	// GitHub의 head 필터는 "owner:branch" 형식이다. 콜론은 쿼리 값에서 인코딩할
	// 필요가 없고(RFC 3986), 브랜치 이름의 슬래시는 %2F로 인코딩된다.
	req := f.find("GET", root+"/pulls")
	if !strings.Contains(req.Query, "head=acme:schema%2Fx") {
		t.Errorf("head 필터가 없습니다: %s", req.Query)
	}
}

func TestGitHubOpenPRCreates(t *testing.T) {
	f := newFake(t)
	root := "/api/v3/repos/acme/db"
	f.on("GET", root+"/pulls", 200, `[]`)
	f.on("POST", root+"/pulls", 201, `{"number":7,"html_url":"https://git/pr/7","state":"open"}`)

	cfg := Config{Kind: GitHub, BaseURL: f.server.URL, Repo: "acme/db", Token: "t"}
	p, _ := Get(GitHub)
	pr, err := p.OpenPR(context.Background(), cfg, PRRequest{
		SourceBranch: "schema/x", TargetBranch: "main", Title: "제목", Body: "본문",
	})
	if err != nil {
		t.Fatalf("open pr: %v", err)
	}
	if pr.Number != 7 || pr.Existing {
		t.Errorf("pr = %+v", pr)
	}
	body := decodeBody(t, f.find("POST", root+"/pulls").Body)
	if body["head"] != "schema/x" || body["base"] != "main" || body["title"] != "제목" {
		t.Errorf("본문 = %+v", body)
	}
}

// GitHub의 검증 오류는 message만 보면 무엇이 잘못됐는지 알 수 없다.
// errors 배열까지 읽어 사용자에게 보여줘야 한다.
func TestGitHubErrorDetails(t *testing.T) {
	f := newFake(t)
	root := "/api/v3/repos/acme/db"
	f.on("GET", root+"/pulls", 200, `[]`)
	f.on("POST", root+"/pulls", 422, `{"message":"Validation Failed",
		"errors":[{"resource":"PullRequest","field":"head","code":"invalid",
		"message":"No commits between main and schema/x"}]}`)

	cfg := Config{Kind: GitHub, BaseURL: f.server.URL, Repo: "acme/db", Token: "t"}
	p, _ := Get(GitHub)
	_, err := p.OpenPR(context.Background(), cfg, PRRequest{
		SourceBranch: "schema/x", TargetBranch: "main", Title: "t",
	})
	if err == nil {
		t.Fatal("422가 성공으로 처리되었습니다")
	}
	if !strings.Contains(err.Error(), "No commits between") {
		t.Errorf("상세 오류가 전달되지 않았습니다: %v", err)
	}
}

// ---------- GitLab ----------

func TestGitLabVerify(t *testing.T) {
	f := newFake(t)
	f.on("GET", "/api/v4/projects/group%2Fsub%2Fproj", 200, `{
		"path_with_namespace":"group/sub/proj","default_branch":"trunk",
		"web_url":"https://gl/group/sub/proj","visibility":"private",
		"permissions":{"project_access":{"access_level":40}}}`)

	cfg := Config{Kind: GitLab, BaseURL: f.server.URL, Repo: "group/sub/proj", Token: "glpat"}
	p, _ := Get(GitLab)
	info, err := p.Verify(context.Background(), cfg)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if info.DefaultBranch != "trunk" || info.FullName != "group/sub/proj" {
		t.Errorf("info = %+v", info)
	}
	// 40 = Maintainer → 쓰기 가능
	if info.CanWrite == nil || !*info.CanWrite {
		t.Errorf("canWrite = %v (access_level 40은 쓰기 가능)", info.CanWrite)
	}
	req := f.find("GET", "/api/v4/projects/group%2Fsub%2Fproj")
	if got := req.Headers.Get("PRIVATE-TOKEN"); got != "glpat" {
		t.Errorf("PRIVATE-TOKEN = %q (GitLab은 이 헤더를 쓴다)", got)
	}
	if req.Headers.Get("Authorization") != "" {
		t.Error("GitLab에 Authorization 헤더를 보냈습니다")
	}
}

// 접근 수준 30(Developer) 미만은 브랜치를 만들 수 없다.
func TestGitLabAccessLevel(t *testing.T) {
	cases := []struct {
		level int
		write bool
	}{{10, false}, {20, false}, {30, true}, {40, true}, {50, true}}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("level%d", tc.level), func(t *testing.T) {
			f := newFake(t)
			f.on("GET", "/api/v4/projects/a%2Fb", 200, fmt.Sprintf(`{
				"path_with_namespace":"a/b","default_branch":"main","visibility":"private",
				"permissions":{"project_access":{"access_level":%d}}}`, tc.level))
			cfg := Config{Kind: GitLab, BaseURL: f.server.URL, Repo: "a/b", Token: "t"}
			p, _ := Get(GitLab)
			info, err := p.Verify(context.Background(), cfg)
			if err != nil {
				t.Fatalf("verify: %v", err)
			}
			if info.CanWrite == nil || *info.CanWrite != tc.write {
				t.Errorf("level %d → canWrite %v, 기대값 %t", tc.level, info.CanWrite, tc.write)
			}
		})
	}
}

// GitLab은 커밋 API 하나로 여러 파일을 원자적으로 올린다.
// 파일이 이미 있으면 action이 update여야 하고, 없으면 create여야 한다.
func TestGitLabPutFilesActions(t *testing.T) {
	f := newFake(t)
	root := "/api/v4/projects/acme%2Fdb/repository"
	// 첫 파일은 이미 있고, 나머지는 없다.
	f.on("HEAD", root+"/files/migrations%2F20260813_add_col.up.sql", 200, "")
	f.notFound("HEAD", root+"/files/migrations%2F20260813_add_col.down.sql")
	f.notFound("HEAD", root+"/files/migrations%2F20260813_add_col.schema.json")
	f.on("POST", root+"/commits", 201, `{"id":"abc123","web_url":"https://gl/commit/abc123"}`)

	cfg := Config{Kind: GitLab, BaseURL: f.server.URL, Repo: "acme/db", Token: "t"}
	p, _ := Get(GitLab)
	commit, err := p.PutFiles(context.Background(), cfg, "schema/x", "메시지", testFiles)
	if err != nil {
		t.Fatalf("put files: %v", err)
	}
	if commit.SHA != "abc123" {
		t.Errorf("SHA = %q", commit.SHA)
	}

	body := decodeBody(t, f.find("POST", root+"/commits").Body)
	if body["branch"] != "schema/x" || body["commit_message"] != "메시지" {
		t.Errorf("본문 = %+v", body)
	}
	actions, _ := body["actions"].([]any)
	if len(actions) != 3 {
		t.Fatalf("액션 수 = %d", len(actions))
	}
	first, _ := actions[0].(map[string]any)
	if first["action"] != "update" {
		t.Errorf("이미 있는 파일의 action = %v, update 여야 합니다", first["action"])
	}
	second, _ := actions[1].(map[string]any)
	if second["action"] != "create" {
		t.Errorf("없는 파일의 action = %v, create 여야 합니다", second["action"])
	}
}

func TestGitLabEnsureBranchQuery(t *testing.T) {
	f := newFake(t)
	root := "/api/v4/projects/acme%2Fdb/repository"
	// GitLab은 브랜치 이름의 슬래시도 %2F로 받는다.
	f.notFound("GET", root+"/branches/schema%2Fx")
	f.on("POST", root+"/branches", 201, `{"name":"schema/x"}`)

	cfg := Config{Kind: GitLab, BaseURL: f.server.URL, Repo: "acme/db", Token: "t"}
	p, _ := Get(GitLab)
	created, err := p.EnsureBranch(context.Background(), cfg, "schema/x", "main")
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if !created {
		t.Error("created = false")
	}
	req := f.find("POST", root+"/branches")
	if !strings.Contains(req.Query, "branch=schema%2Fx") || !strings.Contains(req.Query, "ref=main") {
		t.Errorf("쿼리 = %s", req.Query)
	}
}

func TestGitLabOpenMR(t *testing.T) {
	f := newFake(t)
	root := "/api/v4/projects/acme%2Fdb"
	f.on("GET", root+"/merge_requests", 200, `[]`)
	f.on("POST", root+"/merge_requests", 201,
		`{"iid":12,"web_url":"https://gl/mr/12","state":"opened"}`)

	cfg := Config{Kind: GitLab, BaseURL: f.server.URL, Repo: "acme/db", Token: "t"}
	p, _ := Get(GitLab)
	pr, err := p.OpenPR(context.Background(), cfg, PRRequest{
		SourceBranch: "schema/x", TargetBranch: "main", Title: "제목", Body: "설명",
	})
	if err != nil {
		t.Fatalf("open mr: %v", err)
	}
	// GitLab의 MR 번호는 iid(프로젝트 내 번호)다. id를 쓰면 URL이 맞지 않는다.
	if pr.Number != 12 {
		t.Errorf("번호 = %d, iid를 써야 합니다", pr.Number)
	}
	body := decodeBody(t, f.find("POST", root+"/merge_requests").Body)
	if body["source_branch"] != "schema/x" || body["target_branch"] != "main" {
		t.Errorf("본문 = %+v", body)
	}
	if body["description"] != "설명" {
		t.Errorf("description = %v (GitLab은 body가 아니라 description이다)", body["description"])
	}
}

// GitLab은 검증 오류를 {"message":{"base":["..."]}} 형태로 준다.
func TestGitLabNestedErrorMessage(t *testing.T) {
	f := newFake(t)
	root := "/api/v4/projects/acme%2Fdb"
	f.on("GET", root+"/merge_requests", 200, `[]`)
	f.on("POST", root+"/merge_requests", 409,
		`{"message":{"base":["Another open merge request already exists"]}}`)

	cfg := Config{Kind: GitLab, BaseURL: f.server.URL, Repo: "acme/db", Token: "t"}
	p, _ := Get(GitLab)
	_, err := p.OpenPR(context.Background(), cfg, PRRequest{SourceBranch: "s", TargetBranch: "main"})
	if err == nil {
		t.Fatal("409가 성공으로 처리되었습니다")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("중첩된 오류 메시지를 읽지 못했습니다: %v", err)
	}
}

// ---------- Bitbucket ----------

func TestBitbucketVerifyBasicAuth(t *testing.T) {
	f := newFake(t)
	f.on("GET", "/2.0/repositories/acme/db", 200, `{
		"full_name":"acme/db","is_private":true,
		"mainbranch":{"name":"develop"},
		"links":{"html":{"href":"https://bitbucket.org/acme/db"}}}`)

	cfg := Config{Kind: Bitbucket, BaseURL: f.server.URL, Repo: "acme/db",
		Username: "alice", Token: "app-password"}
	p, _ := Get(Bitbucket)
	info, err := p.Verify(context.Background(), cfg)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if info.DefaultBranch != "develop" {
		t.Errorf("기본 브랜치 = %q (mainbranch.name을 읽어야 한다)", info.DefaultBranch)
	}
	// 앱 비밀번호는 Basic 인증이다.
	got := f.find("GET", "/2.0/repositories/acme/db").Headers.Get("Authorization")
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("alice:app-password"))
	if got != want {
		t.Errorf("Authorization = %q, 기대값 %q", got, want)
	}
	// 권한 정보는 응답에 없으므로 "알 수 없음"이어야 한다.
	if info.CanWrite != nil {
		t.Errorf("canWrite = %v, Bitbucket 저장소 응답에는 권한 정보가 없으므로 nil이어야 합니다", *info.CanWrite)
	}
}

// 사용자 이름이 없으면 액세스 토큰(Bearer)으로 판단해야 한다.
func TestBitbucketBearerAuth(t *testing.T) {
	f := newFake(t)
	f.on("GET", "/2.0/repositories/acme/db", 200, `{"full_name":"acme/db","mainbranch":{"name":"main"}}`)

	cfg := Config{Kind: Bitbucket, BaseURL: f.server.URL, Repo: "acme/db", Token: "access-token"}
	p, _ := Get(Bitbucket)
	if _, err := p.Verify(context.Background(), cfg); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if got := f.find("GET", "/2.0/repositories/acme/db").Headers.Get("Authorization"); got != "Bearer access-token" {
		t.Errorf("Authorization = %q", got)
	}
}

// Bitbucket의 /src는 multipart/form-data이고 파일 경로가 폼 필드 이름이다.
func TestBitbucketPutFilesMultipart(t *testing.T) {
	f := newFake(t)
	root := "/2.0/repositories/acme/db"
	f.on("GET", root+"/refs/branches/schema/x", 200,
		`{"name":"schema/x","target":{"hash":"head-hash","links":{"html":{"href":"https://bb/commit/head"}}}}`)
	f.on("POST", root+"/src", 201, "")

	cfg := Config{Kind: Bitbucket, BaseURL: f.server.URL, Repo: "acme/db",
		Username: "u", Token: "p"}
	p, _ := Get(Bitbucket)
	if _, err := p.PutFiles(context.Background(), cfg, "schema/x", "메시지", testFiles); err != nil {
		t.Fatalf("put files: %v", err)
	}

	req := f.find("POST", root+"/src")
	ct := req.Headers.Get("Content-Type")
	if !strings.HasPrefix(ct, "multipart/form-data") {
		t.Fatalf("Content-Type = %q, multipart/form-data 여야 합니다", ct)
	}
	_, params, err := mime.ParseMediaType(ct)
	if err != nil {
		t.Fatalf("Content-Type 파싱: %v", err)
	}
	mr := multipart.NewReader(strings.NewReader(req.Body), params["boundary"])
	form, err := mr.ReadForm(1 << 20)
	if err != nil {
		t.Fatalf("multipart 파싱: %v", err)
	}
	if got := form.Value["message"]; len(got) != 1 || got[0] != "메시지" {
		t.Errorf("message = %v", got)
	}
	if got := form.Value["branch"]; len(got) != 1 || got[0] != "schema/x" {
		t.Errorf("branch = %v", got)
	}
	// parents가 있어야 그 커밋 위에 쌓인다 (없으면 브랜치를 덮어쓸 수 있다).
	if got := form.Value["parents"]; len(got) != 1 || got[0] != "head-hash" {
		t.Errorf("parents = %v", got)
	}
	for _, want := range testFiles {
		fh, ok := form.File[want.Path]
		if !ok || len(fh) == 0 {
			t.Errorf("파일 필드가 없습니다: %s (필드: %v)", want.Path, keysOf(form.File))
			continue
		}
		file, _ := fh[0].Open()
		content, _ := io.ReadAll(file)
		file.Close()
		if string(content) != want.Content {
			t.Errorf("%s 내용이 다릅니다: %q", want.Path, content)
		}
	}
}

func keysOf[V any](m map[string]V) []string {
	out := []string{}
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestBitbucketOpenPR(t *testing.T) {
	f := newFake(t)
	root := "/2.0/repositories/acme/db"
	f.on("GET", root+"/pullrequests", 200, `{"values":[]}`)
	f.on("POST", root+"/pullrequests", 201,
		`{"id":5,"state":"OPEN","links":{"html":{"href":"https://bb/pr/5"}}}`)

	cfg := Config{Kind: Bitbucket, BaseURL: f.server.URL, Repo: "acme/db", Token: "t"}
	p, _ := Get(Bitbucket)
	pr, err := p.OpenPR(context.Background(), cfg, PRRequest{
		SourceBranch: "schema/x", TargetBranch: "main", Title: "제목", Body: "설명",
	})
	if err != nil {
		t.Fatalf("open pr: %v", err)
	}
	if pr.Number != 5 || pr.State != "open" {
		t.Errorf("pr = %+v", pr)
	}
	body := decodeBody(t, f.find("POST", root+"/pullrequests").Body)
	src, _ := body["source"].(map[string]any)
	branch, _ := src["branch"].(map[string]any)
	if branch["name"] != "schema/x" {
		t.Errorf("source.branch.name = %v (Bitbucket은 중첩 구조를 쓴다)", branch["name"])
	}
}

// ---------- 공통 ----------

// 자체 호스팅 주소에 API 접두사를 중복해서 붙이면 404가 난다.
func TestAPIRootHandling(t *testing.T) {
	cases := []struct {
		kind Kind
		base string
		want string
	}{
		{GitHub, "", "https://api.github.com"},
		{GitHub, "https://git.corp.com", "https://git.corp.com/api/v3"},
		{GitHub, "https://git.corp.com/", "https://git.corp.com/api/v3"},
		{GitHub, "https://git.corp.com/api/v3", "https://git.corp.com/api/v3"},
		{GitLab, "", "https://gitlab.com/api/v4"},
		{GitLab, "https://gl.corp.com", "https://gl.corp.com/api/v4"},
		{GitLab, "https://gl.corp.com/api/v4", "https://gl.corp.com/api/v4"},
		{Bitbucket, "", "https://api.bitbucket.org/2.0"},
		{Bitbucket, "https://bb.corp.com", "https://bb.corp.com/2.0"},
		{Bitbucket, "https://bb.corp.com/2.0", "https://bb.corp.com/2.0"},
	}
	for _, tc := range cases {
		t.Run(string(tc.kind)+"/"+tc.base, func(t *testing.T) {
			if got := apiRoot(Config{Kind: tc.kind, BaseURL: tc.base}); got != tc.want {
				t.Errorf("apiRoot = %q, 기대값 %q", got, tc.want)
			}
		})
	}
}

func TestValidateBaseURL(t *testing.T) {
	cases := []struct {
		raw     string
		wantErr string
	}{
		{"", ""},
		{"https://git.corp.com", ""},
		{"http://localhost:3000", ""},
		{"http://127.0.0.1:8080", ""},
		{"http://192.168.1.10", ""},
		{"http://10.0.0.5/gitlab", ""},
		{"http://git.internal", ""},
		{"http://public.example.com", "사설망"},
		{"ftp://git.corp.com", "http 또는 https"},
		{"https://user:pw@git.corp.com", "자격증명"},
		{"https://", "호스트"},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			err := ValidateBaseURL(tc.raw)
			if tc.wantErr == "" {
				if err != nil {
					t.Errorf("허용해야 하는데 거부되었습니다: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("거부해야 하는데 통과했습니다")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("오류 = %v, %q 를 포함해야 합니다", err, tc.wantErr)
			}
		})
	}
}

func TestSlugify(t *testing.T) {
	cases := []struct{ in, want string }{
		{"주문 도메인 개편", "주문-도메인-개편"},
		{"Add display_name column", "add-display-name-column"},
		{"  spaces   everywhere  ", "spaces-everywhere"},
		{"UPPER Case", "upper-case"},
		{"weird!@#$%^&*()chars", "weird-chars"},
		{"", "change"},
		{"!!!", "change"},
		{"a/b/c", "a-b-c"},
		{"dots...and.dots", "dots-and-dots"},
		{"trailing-", "trailing"},
		{"~^:?*[", "change"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := Slugify(tc.in); got != tc.want {
				t.Errorf("Slugify(%q) = %q, 기대값 %q", tc.in, got, tc.want)
			}
		})
	}
	// Git 참조에 쓸 수 없는 문자가 남으면 브랜치 생성이 실패한다.
	for _, bad := range []string{" ", "~", "^", ":", "?", "*", "[", "\\", ".."} {
		got := Slugify("pre" + bad + "post")
		if strings.Contains(got, bad) {
			t.Errorf("Slugify가 %q 를 남겼습니다: %q", bad, got)
		}
	}
}

func TestExpandTemplate(t *testing.T) {
	v := TemplateVars{
		Date: "2026-08-13", Timestamp: "20260813T131500Z", Slug: "add-col",
		Connection: "dev-mysql", Env: "dev", Version: "3", MigrationID: "mig-1",
	}
	got := Expand("schema/{date}-{slug}", v)
	if got != "schema/2026-08-13-add-col" {
		t.Errorf("브랜치 템플릿 = %q", got)
	}
	got = Expand("migrations/{conn}/{ts}_{slug}", v)
	if got != "migrations/dev-mysql/20260813T131500Z_add-col" {
		t.Errorf("경로 템플릿 = %q", got)
	}
	// 알 수 없는 자리표시자는 그대로 남는다 — 조용히 지우면 경로가 뭉개진다.
	if got := Expand("x/{unknown}", v); got != "x/{unknown}" {
		t.Errorf("알 수 없는 자리표시자 = %q", got)
	}
}

func TestSplitRepo(t *testing.T) {
	cases := []struct {
		in         string
		owner, rep string
		wantErr    bool
	}{
		{"acme/db", "acme", "db", false},
		{"/acme/db/", "acme", "db", false},
		{"group/sub/proj", "group/sub", "proj", false},
		{"acme", "", "", true},
		{"", "", "", true},
		{"acme/", "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			owner, rep, err := splitRepo(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Error("오류를 기대했습니다")
				}
				return
			}
			if err != nil {
				t.Fatalf("splitRepo: %v", err)
			}
			if owner != tc.owner || rep != tc.rep {
				t.Errorf("= %q, %q; 기대값 %q, %q", owner, rep, tc.owner, tc.rep)
			}
		})
	}
}

// 리다이렉트를 따라가면 토큰이 다른 호스트로 전달된다.
func TestNoRedirectFollowing(t *testing.T) {
	var leaked string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked = r.Header.Get("Authorization")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"full_name":"x/y"}`))
	}))
	defer target.Close()

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/api/v3/repos/x/y", http.StatusMovedPermanently)
	}))
	defer origin.Close()

	cfg := Config{Kind: GitHub, BaseURL: origin.URL, Repo: "x/y", Token: "secret-token"}
	p, _ := Get(GitHub)
	if _, err := p.Verify(context.Background(), cfg); err == nil {
		t.Fatal("리다이렉트를 따라가 성공했습니다")
	}
	if leaked != "" {
		t.Errorf("토큰이 다른 호스트로 전달되었습니다: %q", leaked)
	}
}

func TestUnknownKind(t *testing.T) {
	if _, err := Get(Kind("svn")); err == nil {
		t.Error("알 수 없는 종류가 통과했습니다")
	}
	if Kind("svn").Valid() {
		t.Error("Valid()가 true를 반환했습니다")
	}
}
