package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"

	"dbstudio/internal/model"
	"dbstudio/internal/store"
	"dbstudio/internal/vcs"
)

// vcsTimeout은 Git 서비스 호출 전체의 상한이다.
// 브랜치 생성 → 커밋 → PR 생성까지 여러 번 왕복하므로 개별 요청 상한보다 길다.
const vcsTimeout = 90 * time.Second

// ---------- 연동 설정 ----------

// handleListVCSIntegrations는 연동 목록을 반환한다 (토큰 제외).
func (s *Server) handleListVCSIntegrations(c *fiber.Ctx) error {
	// 내 연동만 나온다. Git 계정은 개인의 것이므로 남의 것을 볼 이유가 없고,
	// 목록에 보이면 그것을 고르는 화면이 생긴다.
	items, err := s.st.ListVCSIntegrations(c.Context(),
		currentUser(c).ID, strings.TrimSpace(c.Query("connection")))
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{
		"items": items,
		"providers": []fiber.Map{
			{
				"value": vcs.GitHub, "label": "GitHub",
				"repoHint":  "owner/repo",
				"tokenHint": "Personal Access Token (repo 권한 또는 fine-grained: Contents 쓰기 + Pull requests 쓰기)",
				"baseHint":  "GitHub Enterprise Server 주소 (비우면 github.com)",
			},
			{
				"value": vcs.GitLab, "label": "GitLab",
				"repoHint":  "group/project (중첩 그룹 가능)",
				"tokenHint": "Personal/Project Access Token (api 스코프, Developer 이상)",
				"baseHint":  "self-hosted GitLab 주소 (비우면 gitlab.com)",
			},
			{
				"value": vcs.Bitbucket, "label": "Bitbucket",
				"repoHint":  "workspace/repo",
				"tokenHint": "앱 비밀번호(사용자 이름 함께 입력) 또는 액세스 토큰",
				"baseHint":  "비워 두세요 (Bitbucket Cloud만 지원)",
			},
		},
		"templateVars": []fiber.Map{
			{"name": "{date}", "help": "2026-08-13"},
			{"name": "{ts}", "help": "20260813T131500Z"},
			{"name": "{slug}", "help": "마이그레이션 제목을 안전한 문자로 변환"},
			{"name": "{conn}", "help": "커넥션 이름"},
			{"name": "{env}", "help": "dev 또는 prod"},
			{"name": "{version}", "help": "기준 버전 번호"},
			{"name": "{id}", "help": "마이그레이션 ID"},
		},
	})
}

type vcsRequest struct {
	Name           string  `json:"name"`
	Provider       string  `json:"provider"`
	BaseURL        string  `json:"baseUrl"`
	Repo           string  `json:"repo"`
	DefaultBranch  string  `json:"defaultBranch"`
	BranchTemplate string  `json:"branchTemplate"`
	PathTemplate   string  `json:"pathTemplate"`
	Username       string  `json:"username"`
	Token          *string `json:"token"`
	ConnectionID   string  `json:"connectionId"`
	Enabled        *bool   `json:"enabled"`
}

// validate는 입력을 검사하고 기본값을 채운다.
func (r *vcsRequest) validate() error {
	r.Name = strings.TrimSpace(r.Name)
	if r.Name == "" {
		return errors.New("연동 이름을 입력하세요")
	}
	if len([]rune(r.Name)) > 80 {
		return errors.New("연동 이름이 너무 깁니다 (80자 제한)")
	}
	if !vcs.Kind(r.Provider).Valid() {
		return errors.New("서비스는 github, gitlab, bitbucket 중 하나여야 합니다")
	}
	r.BaseURL = strings.TrimSpace(r.BaseURL)
	if err := vcs.ValidateBaseURL(r.BaseURL); err != nil {
		return err
	}
	r.Repo = strings.Trim(strings.TrimSpace(r.Repo), "/")
	if r.Repo == "" {
		return errors.New("저장소를 입력하세요")
	}
	if !strings.Contains(r.Repo, "/") {
		return errors.New("저장소는 owner/repo 형식이어야 합니다")
	}
	r.DefaultBranch = strings.TrimSpace(r.DefaultBranch)
	if r.DefaultBranch == "" {
		r.DefaultBranch = "main"
	}
	r.BranchTemplate = strings.TrimSpace(r.BranchTemplate)
	if r.BranchTemplate == "" {
		r.BranchTemplate = "schema/{date}-{slug}"
	}
	r.PathTemplate = strings.TrimSpace(r.PathTemplate)
	if r.PathTemplate == "" {
		r.PathTemplate = "migrations/{ts}_{slug}"
	}
	// 경로 템플릿이 저장소 밖을 가리키면 안 된다. 서비스가 거부하겠지만
	// 여기서 막는 것이 오류 메시지를 이해하기 쉽다.
	if strings.HasPrefix(r.PathTemplate, "/") || strings.Contains(r.PathTemplate, "..") {
		return errors.New("경로 템플릿은 상대 경로여야 하며 .. 를 쓸 수 없습니다")
	}
	if strings.HasPrefix(r.BranchTemplate, "/") || strings.Contains(r.BranchTemplate, "..") {
		return errors.New("브랜치 템플릿에 / 로 시작하거나 .. 를 쓸 수 없습니다")
	}
	r.Username = strings.TrimSpace(r.Username)
	return nil
}

func (s *Server) handleCreateVCSIntegration(c *fiber.Ctx) error {
	var req vcsRequest
	if err := c.BodyParser(&req); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "요청 본문을 해석할 수 없습니다")
	}
	if err := req.validate(); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", err.Error())
	}
	if req.Token == nil || strings.TrimSpace(*req.Token) == "" {
		return fail(c, fiber.StatusBadRequest, "bad_request", "토큰을 입력하세요")
	}
	if err := s.checkVCSConnectionScope(c, req.ConnectionID); err != nil {
		return err
	}

	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	u := currentUser(c)
	item, err := s.st.CreateVCSIntegration(c.Context(), store.SaveVCSParams{
		Name: req.Name, Provider: req.Provider, BaseURL: req.BaseURL, Repo: req.Repo,
		DefaultBranch: req.DefaultBranch, BranchTemplate: req.BranchTemplate,
		PathTemplate: req.PathTemplate, Username: req.Username, Token: req.Token,
		ConnectionID: req.ConnectionID, Enabled: enabled, OwnerID: u.ID,
	})
	if errors.Is(err, store.ErrDuplicateName) {
		return fail(c, fiber.StatusConflict, "duplicate", "이미 같은 이름의 연동이 있습니다")
	}
	if err != nil {
		return err
	}
	s.audit(c, store.AuditParams{
		Action: "vcs.create", TargetType: "vcs_integration", TargetID: item.ID,
		Detail: map[string]any{
			"name": item.Name, "provider": item.Provider, "repo": item.Repo,
			"baseUrl": item.BaseURL,
		},
	})
	return c.Status(fiber.StatusCreated).JSON(fiber.Map{"integration": item})
}

func (s *Server) handleUpdateVCSIntegration(c *fiber.Ctx) error {
	id := c.Params("id")
	if _, err := s.st.GetVCSIntegration(c.Context(), id, currentUser(c).ID, false); errors.Is(err, store.ErrNotFound) {
		return fiber.NewError(fiber.StatusNotFound, "연동을 찾을 수 없습니다")
	} else if err != nil {
		return err
	}

	var req vcsRequest
	if err := c.BodyParser(&req); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "요청 본문을 해석할 수 없습니다")
	}
	if err := req.validate(); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", err.Error())
	}
	if err := s.checkVCSConnectionScope(c, req.ConnectionID); err != nil {
		return err
	}
	// 토큰을 보내지 않았으면 기존 것을 유지한다.
	if req.Token != nil && strings.TrimSpace(*req.Token) == "" {
		req.Token = nil
	}

	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	item, err := s.st.UpdateVCSIntegration(c.Context(), store.SaveVCSParams{
		ID: id, Name: req.Name, Provider: req.Provider, BaseURL: req.BaseURL, Repo: req.Repo,
		DefaultBranch: req.DefaultBranch, BranchTemplate: req.BranchTemplate,
		PathTemplate: req.PathTemplate, Username: req.Username, Token: req.Token,
		ConnectionID: req.ConnectionID, Enabled: enabled, OwnerID: currentUser(c).ID,
	})
	if errors.Is(err, store.ErrDuplicateName) {
		return fail(c, fiber.StatusConflict, "duplicate", "이미 같은 이름의 연동이 있습니다")
	}
	if errors.Is(err, store.ErrNotFound) {
		return fiber.NewError(fiber.StatusNotFound, "연동을 찾을 수 없습니다")
	}
	if err != nil {
		return err
	}
	s.audit(c, store.AuditParams{
		Action: "vcs.update", TargetType: "vcs_integration", TargetID: id,
		Detail: map[string]any{
			"name": item.Name, "repo": item.Repo, "tokenChanged": req.Token != nil,
		},
	})
	return c.JSON(fiber.Map{"integration": item})
}

func (s *Server) handleDeleteVCSIntegration(c *fiber.Ctx) error {
	id := c.Params("id")
	item, err := s.st.GetVCSIntegration(c.Context(), id, currentUser(c).ID, false)
	if errors.Is(err, store.ErrNotFound) {
		return fiber.NewError(fiber.StatusNotFound, "연동을 찾을 수 없습니다")
	}
	if err != nil {
		return err
	}
	if err := s.st.DeleteVCSIntegration(c.Context(), id, currentUser(c).ID); err != nil {
		return err
	}
	s.audit(c, store.AuditParams{
		Action: "vcs.delete", TargetType: "vcs_integration", TargetID: id,
		Detail: map[string]any{"name": item.Name, "repo": item.Repo},
	})
	return c.JSON(fiber.Map{"deleted": true})
}

// handleTestVCSIntegration은 토큰과 저장소 접근을 확인한다.
func (s *Server) handleTestVCSIntegration(c *fiber.Ctx) error {
	id := c.Params("id")
	item, err := s.st.GetVCSIntegration(c.Context(), id, currentUser(c).ID, true)
	if errors.Is(err, store.ErrNotFound) {
		return fiber.NewError(fiber.StatusNotFound, "연동을 찾을 수 없습니다")
	}
	if err != nil {
		return err
	}

	provider, err := vcs.Get(vcs.Kind(item.Provider))
	if err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", err.Error())
	}
	ctx, cancel := context.WithTimeout(c.Context(), vcsTimeout)
	defer cancel()

	info, verr := provider.Verify(ctx, vcsConfig(item))
	if verr != nil {
		_ = s.st.RecordVCSCheck(c.Context(), id, false, verr.Error())
		s.audit(c, store.AuditParams{
			Action: "vcs.test", TargetType: "vcs_integration", TargetID: id,
			Result: "error",
			Detail: map[string]any{"name": item.Name, "error": verr.Error()},
		})
		return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{
			"ok": false, "error": "verify_failed",
			"message": "저장소에 접근할 수 없습니다", "detail": verr.Error(),
		})
	}

	msg := fmt.Sprintf("%s (기본 브랜치 %s)", info.FullName, info.DefaultBranch)
	_ = s.st.RecordVCSCheck(c.Context(), id, true, msg)
	s.audit(c, store.AuditParams{
		Action: "vcs.test", TargetType: "vcs_integration", TargetID: id,
		Detail: map[string]any{"name": item.Name, "repo": info.FullName},
	})

	warnings := []string{}
	if info.CanWrite != nil && !*info.CanWrite {
		warnings = append(warnings,
			"이 토큰에는 쓰기 권한이 없습니다. 브랜치 생성과 커밋이 실패합니다")
	}
	if info.CanWrite == nil {
		warnings = append(warnings,
			"이 서비스는 권한 정보를 알려주지 않습니다. 실제 푸시에서 권한 부족이 드러날 수 있습니다")
	}
	if item.DefaultBranch != "" && info.DefaultBranch != "" && item.DefaultBranch != info.DefaultBranch {
		warnings = append(warnings, fmt.Sprintf(
			"설정된 기준 브랜치는 %s 인데 저장소의 기본 브랜치는 %s 입니다",
			item.DefaultBranch, info.DefaultBranch))
	}
	return c.JSON(fiber.Map{"ok": true, "repo": info, "warnings": warnings})
}

// checkVCSConnectionScope는 커넥션 전용 연동을 만들 때 그 커넥션 권한을 확인한다.
// 커넥션을 지정하지 않은 공용 연동은 어드민 권한만으로 충분하다.
func (s *Server) checkVCSConnectionScope(c *fiber.Ctx, connectionID string) error {
	connectionID = strings.TrimSpace(connectionID)
	if connectionID == "" {
		return nil
	}
	if _, err := s.st.GetConnection(c.Context(), connectionID); errors.Is(err, store.ErrNotFound) {
		return fiber.NewError(fiber.StatusNotFound, "커넥션을 찾을 수 없습니다")
	} else if err != nil {
		return err
	}
	d, err := s.requireLevel(c, connectionID, model.LevelMigrate)
	if err != nil {
		return err
	}
	if !d.Allowed {
		return fiber.NewError(fiber.StatusForbidden, d.Reason)
	}
	return nil
}

func vcsConfig(item *store.VCSIntegration) vcs.Config {
	return vcs.Config{
		Kind: vcs.Kind(item.Provider), BaseURL: item.BaseURL, Repo: item.Repo,
		Username: item.Username, Token: item.Token,
	}
}

// ---------- 푸시 ----------

// handlePushMigration은 마이그레이션을 브랜치에 커밋하고 PR/MR을 만든다.
//
// 순서: 브랜치 확보 → 파일 커밋 → PR 생성. 각 단계의 결과를 기록하므로
// 중간에 실패해도 "어디까지 갔는지" 알 수 있다.
func (s *Server) handlePushMigration(c *fiber.Ctx) error {
	mig, conn, err := s.resolveMigration(c, c.Params("migId"), model.LevelMigrate)
	if err != nil {
		return err
	}
	var body struct {
		IntegrationID string `json:"integrationId"`
		Branch        string `json:"branch"`
		BaseBranch    string `json:"baseBranch"`
		Title         string `json:"title"`
		OpenPR        *bool  `json:"openPr"`
	}
	if err := c.BodyParser(&body); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "요청 본문을 해석할 수 없습니다")
	}

	// 푸시도 **내 연동으로만** 한다. 남의 계정으로 PR을 여는 길이 있으면
	// 원격 저장소의 "누가 올렸는가"가 이 앱의 감사 기록과 어긋난다.
	item, err := s.st.GetVCSIntegration(c.Context(),
		strings.TrimSpace(body.IntegrationID), currentUser(c).ID, true)
	if errors.Is(err, store.ErrNotFound) {
		return fiber.NewError(fiber.StatusNotFound, "연동을 찾을 수 없습니다")
	}
	if err != nil {
		return err
	}
	if !item.Enabled {
		return fail(c, fiber.StatusBadRequest, "disabled", "이 연동은 비활성 상태입니다")
	}
	// 커넥션 전용 연동은 그 커넥션의 마이그레이션에만 쓸 수 있다.
	// 그러지 않으면 개발 DB용 저장소에 운영 변경이 올라갈 수 있다.
	if item.ConnectionID != "" && item.ConnectionID != conn.ID {
		return fail(c, fiber.StatusBadRequest, "wrong_connection",
			"이 연동은 다른 커넥션 전용입니다")
	}

	provider, err := vcs.Get(vcs.Kind(item.Provider))
	if err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", err.Error())
	}

	now := time.Now().UTC()
	vars := vcs.TemplateVars{
		Date:        now.Format("2006-01-02"),
		Timestamp:   now.Format("20060102T150405Z"),
		Slug:        vcs.Slugify(mig.Title),
		Connection:  vcs.Slugify(conn.Name),
		Env:         string(conn.Environment),
		MigrationID: mig.ID,
	}
	if mig.FromVersionNo != nil {
		vars.Version = fmt.Sprintf("%d", *mig.FromVersionNo)
	}

	branch := strings.TrimSpace(body.Branch)
	if branch == "" {
		branch = vcs.Expand(item.BranchTemplate, vars)
	}
	base := strings.TrimSpace(body.BaseBranch)
	if base == "" {
		base = item.DefaultBranch
	}
	title := strings.TrimSpace(body.Title)
	if title == "" {
		title = fmt.Sprintf("[%s] %s", conn.Name, mig.Title)
	}
	openPR := true
	if body.OpenPR != nil {
		openPR = *body.OpenPR
	}

	files := migrationFiles(item.PathTemplate, vars, mig, conn)
	paths := make([]string, 0, len(files))
	for _, f := range files {
		paths = append(paths, f.Path)
	}

	u := currentUser(c)
	res, err := s.pushMigrationTo(c.Context(), item, mig, conn, pushOptions{
		Actor: u, IP: clientIP(c), Branch: branch, BaseBranch: base,
		Title: title, OpenPR: openPR, Via: "ui",
	})
	if err != nil {
		var pf *pushFailure
		if errors.As(err, &pf) {
			return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{
				"error": "push_failed", "message": "Git 저장소에 올리지 못했습니다",
				"stage": pf.Stage, "detail": pf.Err.Error(),
			})
		}
		return err
	}
	_ = provider // provider는 pushMigrationTo 안에서 다시 얻는다
	return c.JSON(fiber.Map{
		"push": res.Record, "commit": res.Commit, "pullRequest": res.PR,
	})
}

// pushOptions는 푸시 입력이다. 비어 있는 값은 연동 설정의 기본값으로 채운다.
type pushOptions struct {
	Actor      *model.User
	IP         string
	Branch     string
	BaseBranch string
	Title      string
	OpenPR     bool
	// Via는 감사 로그에 남길 경로다 (ui 또는 ai).
	Via string
}

// pushResult는 푸시 결과다.
type pushResult struct {
	Record    *store.VCSPush
	Commit    *vcs.Commit
	PR        *vcs.PullRequest
	Branch    string
	CommitSHA string
	PRURL     string
	Files     []string
}

// pushFailure는 어느 단계에서 실패했는지 담는다.
//
// 단계를 구분하는 이유: "커밋까지는 갔는데 PR 생성이 실패했다"와 "브랜치도 못 만들었다"는
// 사용자가 해야 할 일이 완전히 다르다.
type pushFailure struct {
	Stage string
	Err   error
}

func (e *pushFailure) Error() string { return e.Stage + ": " + e.Err.Error() }
func (e *pushFailure) Unwrap() error { return e.Err }

// pushMigrationTo는 브랜치 확보 → 파일 커밋 → PR 생성을 수행한다.
//
// REST 핸들러와 AI 툴이 같은 함수를 쓰는 이유: 두 경로가 각자 구현하면 한쪽에만
// 기록이 남거나 한쪽만 PR 재사용 규칙을 따르는 식으로 어긋난다.
func (s *Server) pushMigrationTo(ctx context.Context, item *store.VCSIntegration,
	mig *store.Migration, conn *model.Connection, opts pushOptions) (*pushResult, error) {

	provider, err := vcs.Get(vcs.Kind(item.Provider))
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	vars := vcs.TemplateVars{
		Date: now.Format("2006-01-02"), Timestamp: now.Format("20060102T150405Z"),
		Slug: vcs.Slugify(mig.Title), Connection: vcs.Slugify(conn.Name),
		Env: string(conn.Environment), MigrationID: mig.ID,
	}
	if mig.FromVersionNo != nil {
		vars.Version = fmt.Sprintf("%d", *mig.FromVersionNo)
	}

	branch := strings.TrimSpace(opts.Branch)
	if branch == "" {
		branch = vcs.Expand(item.BranchTemplate, vars)
	}
	base := strings.TrimSpace(opts.BaseBranch)
	if base == "" {
		base = item.DefaultBranch
	}
	title := strings.TrimSpace(opts.Title)
	if title == "" {
		title = fmt.Sprintf("[%s] %s", conn.Name, mig.Title)
	}

	files := migrationFiles(item.PathTemplate, vars, mig, conn)
	paths := make([]string, 0, len(files))
	for _, f := range files {
		paths = append(paths, f.Path)
	}

	callCtx, cancel := context.WithTimeout(ctx, vcsTimeout)
	defer cancel()

	actorID, actorName := "", ""
	if opts.Actor != nil {
		actorID, actorName = opts.Actor.ID, displayName(opts.Actor)
	}
	record := &store.VCSPush{
		IntegrationID: item.ID, MigrationID: mig.ID, MigrationTitle: mig.Title,
		Branch: branch, Files: paths, ActorID: actorID, ActorName: actorName,
	}

	auditDetail := func(extra map[string]any) map[string]any {
		d := map[string]any{
			"integration": item.Name, "repo": item.Repo, "branch": branch,
			"via": opts.Via,
		}
		for k, v := range extra {
			d[k] = v
		}
		return d
	}
	failPush := func(stage string, pushErr error) (*pushResult, error) {
		record.Status = "failed"
		record.Error = stage + ": " + pushErr.Error()
		_ = s.st.RecordVCSPush(ctx, record)
		_ = s.st.RecordVCSCheck(ctx, item.ID, false, record.Error)
		s.auditDirect(ctx, actorID, actorName, opts.IP, store.AuditParams{
			Action: "vcs.push", TargetType: "migration", TargetID: mig.ID,
			Result: "error",
			Detail: auditDetail(map[string]any{"stage": stage, "error": pushErr.Error()}),
		})
		return nil, &pushFailure{Stage: stage, Err: pushErr}
	}

	created, err := provider.EnsureBranch(callCtx, vcsConfig(item), branch, base)
	if err != nil {
		return failPush("브랜치 생성", err)
	}
	record.BranchCreated = created

	message := commitMessage(mig, conn, title)
	commit, err := provider.PutFiles(callCtx, vcsConfig(item), branch, message, files)
	if err != nil {
		return failPush("파일 커밋", err)
	}
	record.CommitSHA, record.CommitURL = commit.SHA, commit.WebURL

	var pr *vcs.PullRequest
	if opts.OpenPR {
		pr, err = provider.OpenPR(callCtx, vcsConfig(item), vcs.PRRequest{
			SourceBranch: branch, TargetBranch: base,
			Title: title, Body: prBody(mig, conn, paths),
		})
		if err != nil {
			// 커밋은 성공했으므로 그 사실을 기록에 남긴 뒤 실패를 알린다.
			// 커밋까지 갔다는 정보가 없으면 사용자가 처음부터 다시 시도하게 된다.
			return failPush("PR 생성", err)
		}
		record.PRNumber, record.PRURL = &pr.Number, pr.WebURL
		record.PRExisting = pr.Existing
	}

	record.Status = "ok"
	if err := s.st.RecordVCSPush(ctx, record); err != nil {
		return nil, err
	}
	_ = s.st.RecordVCSCheck(ctx, item.ID, true, "푸시 성공: "+branch)
	s.auditDirect(ctx, actorID, actorName, opts.IP, store.AuditParams{
		Action: "vcs.push", TargetType: "migration", TargetID: mig.ID,
		Detail: auditDetail(map[string]any{
			"branchCreated": created, "commit": commit.SHA, "files": len(files),
			"pr": record.PRURL, "prExisting": record.PRExisting,
		}),
	})
	return &pushResult{
		Record: record, Commit: commit, PR: pr,
		Branch: branch, CommitSHA: commit.SHA, PRURL: record.PRURL, Files: paths,
	}, nil
}

// handleListVCSPushes는 푸시 이력을 반환한다.
func (s *Server) handleListVCSPushes(c *fiber.Ctx) error {
	migID := strings.TrimSpace(c.Query("migration"))
	if migID != "" {
		// 마이그레이션별 조회는 그 커넥션 권한을 확인한다.
		if _, _, err := s.resolveMigration(c, migID, model.LevelMonitor); err != nil {
			return err
		}
	}
	pushes, err := s.st.ListVCSPushes(c.Context(), migID, c.QueryInt("limit", 100))
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"pushes": pushes})
}

// ---------- 커밋 내용 만들기 ----------

// migrationFiles는 커밋할 파일 목록을 만든다.
//
// 세 파일을 함께 올리는 이유:
//   - up.sql / down.sql — 리뷰어가 실제로 실행될 SQL을 본다
//   - schema.json       — 목표 스키마 전체(IR). 나중에 이 시점의 설계를 복원할 수 있고,
//     SQL만으로는 읽기 어려운 구조를 도구가 다시 해석할 수 있다
func migrationFiles(pathTemplate string, vars vcs.TemplateVars, mig *store.Migration, conn *model.Connection) []vcs.File {
	base := vcs.Expand(pathTemplate, vars)
	files := []vcs.File{
		{Path: base + ".up.sql", Content: sqlFileContent(mig, conn, "up")},
		{Path: base + ".down.sql", Content: sqlFileContent(mig, conn, "down")},
	}
	if mig.TargetSchema != nil {
		if data, err := json.MarshalIndent(mig.TargetSchema, "", "  "); err == nil {
			files = append(files, vcs.File{Path: base + ".schema.json", Content: string(data) + "\n"})
		}
	}
	return files
}

// sqlFileContent는 SQL 파일 본문을 만든다.
// 머리말에 출처를 적는 이유: 저장소만 보는 사람이 이 파일이 어디서 왔는지,
// 어떤 DB를 대상으로 하는지 알 수 있어야 한다.
func sqlFileContent(mig *store.Migration, conn *model.Connection, dir string) string {
	var b strings.Builder
	label := "적용 (up)"
	sql := mig.UpSQL
	if dir == "down" {
		label = "롤백 (down)"
		sql = mig.DownSQL
	}
	fmt.Fprintf(&b, "-- %s: %s\n", label, mig.Title)
	fmt.Fprintf(&b, "-- 대상: %s (%s, %s)\n", conn.Name, conn.Kind, conn.Environment)
	if mig.FromVersionNo != nil {
		fmt.Fprintf(&b, "-- 기준 버전: v%d\n", *mig.FromVersionNo)
	}
	fmt.Fprintf(&b, "-- 마이그레이션 ID: %s\n", mig.ID)
	if mig.DestructiveCount > 0 {
		fmt.Fprintf(&b, "-- 주의: 데이터 손실이 발생할 수 있는 변경 %d건 포함\n", mig.DestructiveCount)
	}
	if dir == "down" && len(mig.Irreversible) > 0 {
		b.WriteString("-- 주의: 되돌릴 수 없는 변경이 있어 구조만 복구되고 데이터는 복구되지 않습니다\n")
	}
	b.WriteString("\n")
	if strings.TrimSpace(sql) == "" {
		b.WriteString("-- (생성된 SQL이 없습니다)\n")
		return b.String()
	}
	b.WriteString(sql)
	if !strings.HasSuffix(sql, "\n") {
		b.WriteString("\n")
	}
	return b.String()
}

func commitMessage(mig *store.Migration, conn *model.Connection, title string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\n", title)
	fmt.Fprintf(&b, "대상: %s (%s / %s)\n", conn.Name, conn.Kind, conn.Environment)
	if mig.Diff != nil {
		fmt.Fprintf(&b, "변경 %d건", len(mig.Diff.Changes))
		if mig.DestructiveCount > 0 {
			fmt.Fprintf(&b, " (파괴적 %d건)", mig.DestructiveCount)
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "마이그레이션 ID: %s\n", mig.ID)
	return b.String()
}

// prBody는 PR/MR 설명을 만든다.
//
// 여기에 변경 요약과 위험 신호를 담는 이유: 저장소의 리뷰어는 이 앱을 보지 않는다.
// PR 화면만 보고 판단할 수 있어야 리뷰가 실제로 이뤄진다.
func prBody(mig *store.Migration, conn *model.Connection, files []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## 대상\n\n- 커넥션: `%s` (%s / %s)\n", conn.Name, conn.Kind, conn.Environment)
	if mig.FromVersionNo != nil {
		fmt.Fprintf(&b, "- 기준 버전: v%d\n", *mig.FromVersionNo)
	}
	fmt.Fprintf(&b, "- 마이그레이션 ID: `%s`\n", mig.ID)
	fmt.Fprintf(&b, "- 상태: %s\n", mig.Status)

	// 파일 목록을 적는 이유: PR에 다른 커밋이 섞여 있으면 어느 파일이 이 변경인지
	// diff만 보고는 바로 알기 어렵다.
	if len(files) > 0 {
		b.WriteString("\n## 파일\n\n")
		for _, f := range files {
			fmt.Fprintf(&b, "- `%s`\n", f)
		}
	}

	if mig.Diff != nil && len(mig.Diff.Changes) > 0 {
		fmt.Fprintf(&b, "\n## 변경 %d건", len(mig.Diff.Changes))
		if mig.DestructiveCount > 0 {
			fmt.Fprintf(&b, " (파괴적 %d건)", mig.DestructiveCount)
		}
		b.WriteString("\n\n")
		for i, ch := range mig.Diff.Changes {
			if i >= 50 {
				fmt.Fprintf(&b, "- … 그 외 %d건\n", len(mig.Diff.Changes)-50)
				break
			}
			mark := ""
			if ch.Destructive {
				mark = " ⚠️"
			}
			fmt.Fprintf(&b, "- %s%s\n", ch.Summary, mark)
		}
	}

	if mig.DestructiveCount > 0 {
		b.WriteString("\n## ⚠️ 데이터 손실 위험\n\n")
		for _, ch := range mig.Diff.Changes {
			if ch.Destructive && ch.LossyDetail != "" {
				fmt.Fprintf(&b, "- **%s**: %s\n", ch.Summary, ch.LossyDetail)
			}
		}
	}
	if len(mig.Irreversible) > 0 {
		b.WriteString("\n## 되돌릴 수 없는 변경\n\n")
		b.WriteString("롤백하면 구조는 복구되지만 데이터는 복구되지 않습니다.\n\n")
		for _, w := range mig.Irreversible {
			fmt.Fprintf(&b, "- %s\n", w)
		}
	}
	if mig.Plan != nil && len(mig.Plan.Warnings) > 0 {
		b.WriteString("\n## 계획 경고\n\n")
		for _, w := range mig.Plan.Warnings {
			fmt.Fprintf(&b, "- %s\n", w)
		}
	}

	if len(mig.Reviews) > 0 {
		b.WriteString("\n## 앱 내 검토\n\n")
		for _, r := range mig.Reviews {
			label := map[string]string{
				store.ReviewApproved: "승인", store.ReviewRejected: "반려", store.ReviewComment: "의견",
			}[r.Decision]
			fmt.Fprintf(&b, "- %s — **%s**", r.ReviewerName, label)
			if r.Comment != "" {
				fmt.Fprintf(&b, ": %s", r.Comment)
			}
			b.WriteString("\n")
		}
	}

	b.WriteString("\n---\n\n_DB Studio가 자동으로 생성했습니다._\n")
	return b.String()
}
