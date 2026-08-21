package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"dbstudio/internal/erd"
	"dbstudio/internal/migrate"
	"dbstudio/internal/model"
	"dbstudio/internal/schema"
	"dbstudio/internal/store"
	"dbstudio/internal/vcs"
)

// 이 파일은 쓰기 툴의 제안(Propose)과 승인 후 실행(Apply)을 담는다.
//
// 두 단계로 나누는 이유는 계획에서 정한 원칙이다: AI가 쓰기 동작을 직접 실행하지
// 못한다. Propose는 "무엇이 일어날지"를 사용자에게 보여주는 데 필요한 정보만 모으고
// 아무것도 바꾸지 않는다. Apply는 사용자가 승인 버튼을 누른 뒤에만 호출된다.
//
// Propose 단계에서도 권한을 검사하는 이유: 권한이 없는 동작을 제안으로 만들어
// 사용자에게 보여주면, 승인 버튼을 눌러도 실패하는 헛된 선택지를 주는 셈이다.
// 권한이 없으면 그 사실을 모델에게 바로 알려 다른 방법을 찾게 하는 것이 맞다.

// ---------- ERD 초안 생성 ----------

type createERDArgs struct {
	Name           string `json:"name"`
	Connection     string `json:"connection"`
	FromConnection *bool  `json:"fromConnection"`
}

func proposeCreateERD(tc *toolContext, args json.RawMessage) (string, any, error) {
	var in createERDArgs
	if err := parseArgs(args, &in); err != nil {
		return "", nil, err
	}
	if strings.TrimSpace(in.Name) == "" {
		return "", nil, errors.New("초안 이름(name)을 지정하세요")
	}
	conn, err := tc.resolveConn(in.Connection, model.LevelERD)
	if err != nil {
		return "", nil, err
	}
	fromConn := in.FromConnection == nil || *in.FromConnection

	preview := map[string]any{
		"connection":     conn.Name,
		"environment":    string(conn.Environment),
		"fromConnection": fromConn,
	}
	if fromConn {
		// 미리보기에 규모를 담아 사용자가 무엇을 가져오는지 알게 한다.
		if sc, ierr := tc.introspect(conn); ierr == nil {
			preview["willImport"] = sc.Stats()
		} else {
			preview["warning"] = "대상 DB의 스키마를 미리 읽지 못했습니다: " + ierr.Error()
		}
	}
	summary := fmt.Sprintf("%s 를 대상으로 ERD 초안 %q 를 만듭니다", conn.Name, in.Name)
	if fromConn {
		summary += " (현재 스키마를 가져옴)"
	}
	return summary, preview, nil
}

func applyCreateERD(tc *toolContext, args json.RawMessage) (string, error) {
	var in createERDArgs
	if err := parseArgs(args, &in); err != nil {
		return "", err
	}
	conn, err := tc.resolveConn(in.Connection, model.LevelERD)
	if err != nil {
		return "", err
	}
	fromConn := in.FromConnection == nil || *in.FromConnection

	docID := uuid.NewString()
	var doc *erd.Document
	var sourceSnapshot *int64
	if fromConn {
		sc, ierr := tc.introspect(conn)
		if ierr != nil {
			return "", fmt.Errorf("대상 DB의 스키마를 읽지 못했습니다: %w", ierr)
		}
		if snap, _, serr := tc.srv.st.SaveSchemaSnapshot(tc.ctx, conn.ID, sc,
			store.SnapshotSourceManual, nil); serr == nil && snap != nil {
			sourceSnapshot = &snap.ID
		}
		doc = erd.FromSchema(docID, in.Name, conn.ID, sc)
	} else {
		doc = erd.NewDocument(docID, in.Name, conn.ID, string(conn.Kind))
	}

	if err := tc.srv.st.CreateERDDocument(tc.ctx, doc, tc.user.ID,
		"AI 어시스턴트가 생성", sourceSnapshot); err != nil {
		return "", err
	}
	tc.audit("erd.create", "erd_document", docID, "", map[string]any{
		"name": in.Name, "connection": conn.Name, "via": "ai",
		"fromConnection": fromConn, "tables": len(doc.Schema.Tables),
	})
	return asJSON(map[string]any{
		"created": true, "documentId": docID, "name": in.Name,
		"connection": conn.Name, "tables": len(doc.Schema.Tables),
		"url": "/erd/" + docID,
	})
}

// ---------- 마이그레이션 계획 생성 ----------

type createMigrationArgs struct {
	DocumentID string `json:"documentId"`
	Title      string `json:"title"`
}

func proposeCreateMigration(tc *toolContext, args json.RawMessage) (string, any, error) {
	var in createMigrationArgs
	if err := parseArgs(args, &in); err != nil {
		return "", nil, err
	}
	// 계획을 만드는 것은 실행 대상을 정의하는 행위이므로 migrate 등급을 요구한다
	// (사용자 요청 경로와 같은 규칙).
	doc, conn, err := tc.resolveDoc(in.DocumentID, model.LevelMigrate)
	if err != nil {
		return "", nil, err
	}
	current, ierr := tc.introspect(conn)
	if ierr != nil {
		return "", nil, fmt.Errorf("대상 DB의 스키마를 읽지 못했습니다: %w", ierr)
	}
	diff := schema.Diff(current, doc.Schema)
	if diff.IsEmpty() {
		return "", nil, errors.New("초안과 대상 DB의 구조가 같아 만들 마이그레이션이 없습니다")
	}
	plan := schema.BuildPlan(string(conn.Kind), diff)
	if len(plan.Up) == 0 {
		return "", nil, fmt.Errorf("변경은 있지만 실행할 SQL을 만들 수 없습니다: %s",
			strings.Join(plan.Warnings, " / "))
	}

	title := strings.TrimSpace(in.Title)
	if title == "" {
		title = doc.Name
	}
	preview := map[string]any{
		"connection": conn.Name, "document": doc.Name, "title": title,
		"changes": changeList(diff), "destructive": diff.DestructiveCount,
		"statements": len(plan.Up), "upSql": plan.UpSQL(),
		"warnings": plan.Warnings, "irreversible": plan.Irreversible,
	}
	summary := fmt.Sprintf("%s 에 적용할 마이그레이션 계획을 만듭니다 — 변경 %d건",
		conn.Name, len(diff.Changes))
	if diff.DestructiveCount > 0 {
		summary += fmt.Sprintf(" (파괴적 %d건)", diff.DestructiveCount)
	}
	return summary, preview, nil
}

func applyCreateMigration(tc *toolContext, args json.RawMessage) (string, error) {
	var in createMigrationArgs
	if err := parseArgs(args, &in); err != nil {
		return "", err
	}
	doc, conn, err := tc.resolveDoc(in.DocumentID, model.LevelMigrate)
	if err != nil {
		return "", err
	}
	current, ierr := tc.introspect(conn)
	if ierr != nil {
		return "", fmt.Errorf("대상 DB의 스키마를 읽지 못했습니다: %w", ierr)
	}
	diff := schema.Diff(current, doc.Schema)
	if diff.IsEmpty() {
		return "", errors.New("초안과 대상 DB의 구조가 같습니다")
	}
	plan := schema.BuildPlan(string(conn.Kind), diff)

	// 기준선 버전이 없거나 어긋나면 지금 상태를 먼저 확정한다 (REST 경로와 동일).
	from, err := tc.srv.st.LatestSchemaVersion(tc.ctx, conn.ID, false)
	if err != nil {
		return "", err
	}
	if from == nil || from.Fingerprint != current.Fingerprint() {
		from, _, err = tc.srv.st.SaveSchemaVersion(tc.ctx, store.SaveVersionParams{
			ConnectionID: conn.ID, Schema: current,
			Source: sourceForBaseline(from), Note: "마이그레이션 기준선",
			AuthorID: tc.user.ID, AuthorName: displayName(tc.user),
		})
		if err != nil {
			return "", err
		}
	}

	title := strings.TrimSpace(in.Title)
	if title == "" {
		title = doc.Name
	}
	mig, err := tc.srv.st.CreateMigration(tc.ctx, store.CreateMigrationParams{
		ConnectionID: conn.ID, DocID: doc.ID, Title: title,
		FromVersion: &from.ID, BaseFinger: current.Fingerprint(),
		TargetSchema: doc.Schema, Plan: plan, Diff: diff, CreatedBy: tc.user.ID,
	})
	if err != nil {
		return "", err
	}
	tc.audit("migration.create", "migration", mig.ID, "", map[string]any{
		"connection": conn.Name, "title": title, "via": "ai",
		"changes": len(diff.Changes), "destructive": diff.DestructiveCount,
	})
	return asJSON(map[string]any{
		"created": true, "migrationId": mig.ID, "title": title,
		"status": mig.Status, "changes": len(diff.Changes),
		"destructive": diff.DestructiveCount, "statements": len(plan.Up),
		"url":  "/migrations/" + mig.ID,
		"note": "리뷰 요청과 승인을 거쳐야 실행할 수 있습니다",
	})
}

// ---------- 리뷰 요청 ----------

type migrationIDArgs struct {
	MigrationID string `json:"migrationId"`
}

func proposeRequestReview(tc *toolContext, args json.RawMessage) (string, any, error) {
	var in migrationIDArgs
	if err := parseArgs(args, &in); err != nil {
		return "", nil, err
	}
	mig, conn, err := tc.resolveMig(in.MigrationID, model.LevelMigrate)
	if err != nil {
		return "", nil, err
	}
	if !store.CanTransition(mig.Status, store.MigrationInReview) {
		return "", nil, fmt.Errorf("현재 상태(%s)에서는 리뷰를 요청할 수 없습니다", mig.Status)
	}
	required := migrate.RequiredApprovals(conn, mig.DestructiveCount)
	preview := map[string]any{
		"migration": mig.Title, "connection": conn.Name,
		"currentStatus": mig.Status, "requiredApprovals": required,
		"changes": len(mig.Diff.Changes), "destructive": mig.DestructiveCount,
	}
	return fmt.Sprintf("마이그레이션 %q 을 리뷰 요청 상태로 바꿉니다 (승인 %d명 필요)",
		mig.Title, required), preview, nil
}

func applyRequestReview(tc *toolContext, args json.RawMessage) (string, error) {
	var in migrationIDArgs
	if err := parseArgs(args, &in); err != nil {
		return "", err
	}
	mig, conn, err := tc.resolveMig(in.MigrationID, model.LevelMigrate)
	if err != nil {
		return "", err
	}
	if err := tc.srv.st.SetMigrationStatus(tc.ctx, mig.ID, store.MigrationInReview); err != nil {
		var ite *store.InvalidTransitionError
		if errors.As(err, &ite) {
			return "", errors.New(ite.Error())
		}
		return "", err
	}
	tc.audit("migration.status", "migration", mig.ID, "", map[string]any{
		"connection": conn.Name, "from": mig.Status, "to": store.MigrationInReview, "via": "ai",
	})
	return asJSON(map[string]any{
		"status": store.MigrationInReview,
		"note": fmt.Sprintf("검토자 %d명의 승인이 필요합니다. AI는 승인할 수 없습니다",
			migrate.RequiredApprovals(conn, mig.DestructiveCount)),
	})
}

// ---------- 마이그레이션 실행 ----------

func proposeApplyMigration(tc *toolContext, args json.RawMessage) (string, any, error) {
	var in migrationIDArgs
	if err := parseArgs(args, &in); err != nil {
		return "", nil, err
	}
	mig, conn, err := tc.resolveMig(in.MigrationID, model.LevelMigrate)
	if err != nil {
		return "", nil, err
	}
	secret, err := tc.srv.st.GetSecret(tc.ctx, conn.ID)
	if err != nil {
		return "", nil, err
	}
	// 사전 검사를 제안 단계에서 실행한다: 승인 버튼을 누르기 전에 무엇이 막고
	// 있는지 보여주는 것이 이 화면의 핵심 가치다.
	ctx, cancel := context.WithTimeout(tc.ctx, 2*time.Minute)
	defer cancel()
	pc, err := tc.srv.migrator.Check(ctx, conn, secret, mig)
	if err != nil {
		return "", nil, err
	}

	preview := map[string]any{
		"migration": mig.Title, "connection": conn.Name,
		"environment": string(conn.Environment), "status": mig.Status,
		"statements": len(mig.Plan.Up), "destructive": mig.DestructiveCount,
		"upSql": mig.UpSQL, "precheck": pc,
	}
	summary := fmt.Sprintf("마이그레이션 %q 을 %s(%s)에 실행합니다 — SQL %d문장",
		mig.Title, conn.Name, conn.Environment, len(mig.Plan.Up))
	if !pc.OK {
		summary += " — 현재 실행 조건을 만족하지 않습니다"
	}
	if conn.Environment == model.EnvProd {
		summary += " [운영 DB]"
	}
	return summary, preview, nil
}

func applyApplyMigration(tc *toolContext, args json.RawMessage) (string, error) {
	var in migrationIDArgs
	if err := parseArgs(args, &in); err != nil {
		return "", err
	}
	mig, conn, err := tc.resolveMig(in.MigrationID, model.LevelMigrate)
	if err != nil {
		return "", err
	}
	secret, err := tc.srv.st.GetSecret(tc.ctx, conn.ID)
	if err != nil {
		return "", err
	}
	// 사용자가 AI의 제안을 승인했더라도 앱의 기존 게이트는 그대로 적용된다:
	// 검토자 승인 수, 반려 여부, 사전 검사(지문 일치), 백업 훅.
	// AI 경로가 그 게이트를 우회할 수 있으면 P7의 안전장치가 무의미해진다.
	res, err := tc.srv.migrator.Apply(tc.ctx, migrate.ApplyParams{
		Conn: conn, Secret: secret, Mig: mig, Actor: tc.user,
	})
	if err != nil {
		var blocked *migrate.BlockedError
		if errors.As(err, &blocked) {
			tc.audit("migration.apply", "migration", mig.ID, "denied", map[string]any{
				"connection": conn.Name, "via": "ai", "blockers": blocked.Blockers,
			})
			return "", fmt.Errorf("실행 조건을 만족하지 않습니다: %s",
				strings.Join(blocked.Blockers, "; "))
		}
		return "", err
	}
	tc.audit("migration.apply", "migration", mig.ID, resultLabel(res.Status), map[string]any{
		"connection": conn.Name, "via": "ai", "status": res.Status,
		"applied": res.Report.Applied, "error": res.Error,
	})
	if res.Status == store.MigrationApplied {
		if _, _, derr := tc.srv.monitor.CheckDriftByID(tc.ctx, conn.ID); derr != nil {
			_ = derr // 기준선 갱신 실패가 실행 결과를 뒤집을 이유는 없다
		}
	}
	return asJSON(map[string]any{
		"status": res.Status, "applied": res.Report.Applied,
		"error": res.Error, "warnings": res.Warnings,
		"newVersion": versionNo(res.Version), "postDiff": res.PostDiff,
	})
}

func resultLabel(status string) string {
	if status == store.MigrationApplied || status == store.MigrationRolledBack {
		return ""
	}
	return "error"
}

func versionNo(v *store.SchemaVersion) any {
	if v == nil {
		return nil
	}
	return v.VersionNo
}

// ---------- 버전 확정 ----------

type captureVersionArgs struct {
	Connection string `json:"connection"`
	Note       string `json:"note"`
}

func proposeCaptureVersion(tc *toolContext, args json.RawMessage) (string, any, error) {
	var in captureVersionArgs
	if err := parseArgs(args, &in); err != nil {
		return "", nil, err
	}
	conn, err := tc.resolveConn(in.Connection, model.LevelERD)
	if err != nil {
		return "", nil, err
	}
	current, ierr := tc.introspect(conn)
	if ierr != nil {
		return "", nil, ierr
	}
	prev, err := tc.srv.st.LatestSchemaVersion(tc.ctx, conn.ID, true)
	if err != nil {
		return "", nil, err
	}
	preview := map[string]any{"connection": conn.Name, "stats": current.Stats()}
	if prev == nil {
		preview["source"] = store.VersionSourceImport
		preview["note"] = "첫 버전(기준선)으로 등록됩니다"
		return fmt.Sprintf("%s 의 현재 스키마를 첫 버전으로 확정합니다", conn.Name), preview, nil
	}
	preview["source"] = store.VersionSourceExternal
	preview["previousVersion"] = prev.VersionNo
	if prev.Schema != nil {
		diff := schema.Diff(prev.Schema, current)
		preview["changes"] = changeList(diff)
		if diff.IsEmpty() {
			return "", nil, fmt.Errorf("v%d 와 구조가 같아 새 버전을 만들 필요가 없습니다", prev.VersionNo)
		}
	}
	return fmt.Sprintf("%s 의 현재 스키마를 외부 편집 버전으로 확정합니다 (v%d 이후)",
		conn.Name, prev.VersionNo), preview, nil
}

func applyCaptureVersion(tc *toolContext, args json.RawMessage) (string, error) {
	var in captureVersionArgs
	if err := parseArgs(args, &in); err != nil {
		return "", err
	}
	conn, err := tc.resolveConn(in.Connection, model.LevelERD)
	if err != nil {
		return "", err
	}
	current, ierr := tc.introspect(conn)
	if ierr != nil {
		return "", ierr
	}
	prev, err := tc.srv.st.LatestSchemaVersion(tc.ctx, conn.ID, true)
	if err != nil {
		return "", err
	}
	source := store.VersionSourceImport
	summary := []string{}
	if prev != nil {
		source = store.VersionSourceExternal
		if prev.Schema != nil {
			for _, ch := range schema.Diff(prev.Schema, current).Changes {
				summary = append(summary, ch.Summary)
			}
		}
	}
	note := strings.TrimSpace(in.Note)
	if note == "" {
		note = "AI 어시스턴트가 등록"
	}
	version, created, err := tc.srv.st.SaveSchemaVersion(tc.ctx, store.SaveVersionParams{
		ConnectionID: conn.ID, Schema: current, Source: source, Note: note,
		ChangeSummary: summary, AuthorID: tc.user.ID, AuthorName: displayName(tc.user),
	})
	if err != nil {
		return "", err
	}
	tc.audit("version.capture", "connection", conn.ID, "", map[string]any{
		"name": conn.Name, "via": "ai", "source": source,
		"versionNo": version.VersionNo, "created": created,
	})
	return asJSON(map[string]any{
		"created": created, "versionNo": version.VersionNo,
		"source": version.Source, "changes": summary,
	})
}

// ---------- Git 푸시 ----------

type pushArgs struct {
	MigrationID string `json:"migrationId"`
	Integration string `json:"integration"`
}

// resolveIntegration은 이름 또는 ID로 Git 연동을 찾는다.
// 생략되면 이 커넥션에서 쓸 수 있는 첫 연동을 고른다.
//
// **대화 중인 사용자의 연동만 본다.** Git 계정은 개인의 것이므로, 어시스턴트가
// 남의 계정으로 PR을 여는 길이 있으면 안 된다 — 툴이 호출자의 권한으로 실행된다는
// 이 앱의 규칙이 여기서도 그대로 적용된다.
func (tc *toolContext) resolveIntegration(nameOrID, connID string) (*store.VCSIntegration, error) {
	items, err := tc.srv.st.ListVCSIntegrations(tc.ctx, tc.user.ID, connID)
	if err != nil {
		return nil, err
	}
	usable := []*store.VCSIntegration{}
	for _, i := range items {
		if i.Enabled && i.HasToken {
			usable = append(usable, i)
		}
	}
	if len(usable) == 0 {
		return nil, errors.New("사용할 수 있는 Git 연동이 없습니다. 먼저 Git 연동을 등록하세요")
	}
	nameOrID = strings.TrimSpace(nameOrID)
	if nameOrID == "" {
		return tc.srv.st.GetVCSIntegration(tc.ctx, usable[0].ID, tc.user.ID, true)
	}
	for _, i := range usable {
		if i.ID == nameOrID || strings.EqualFold(i.Name, nameOrID) {
			return tc.srv.st.GetVCSIntegration(tc.ctx, i.ID, tc.user.ID, true)
		}
	}
	names := []string{}
	for _, i := range usable {
		names = append(names, i.Name)
	}
	return nil, fmt.Errorf("Git 연동 %q 을(를) 찾을 수 없습니다. 사용 가능: %s",
		nameOrID, strings.Join(names, ", "))
}

func proposePushMigration(tc *toolContext, args json.RawMessage) (string, any, error) {
	var in pushArgs
	if err := parseArgs(args, &in); err != nil {
		return "", nil, err
	}
	mig, conn, err := tc.resolveMig(in.MigrationID, model.LevelMigrate)
	if err != nil {
		return "", nil, err
	}
	item, err := tc.resolveIntegration(in.Integration, conn.ID)
	if err != nil {
		return "", nil, err
	}
	now := time.Now().UTC()
	branch := vcs.Expand(item.BranchTemplate, vcs.TemplateVars{
		Date: now.Format("2006-01-02"), Timestamp: now.Format("20060102T150405Z"),
		Slug: vcs.Slugify(mig.Title), Connection: vcs.Slugify(conn.Name),
		Env: string(conn.Environment), MigrationID: mig.ID,
	})
	preview := map[string]any{
		"migration": mig.Title, "connection": conn.Name,
		"integration": item.Name, "provider": item.Provider, "repo": item.Repo,
		"branch": branch, "baseBranch": item.DefaultBranch,
		"willOpenPullRequest": true,
	}
	return fmt.Sprintf("마이그레이션 %q 을 %s(%s) 의 브랜치 %s 에 올리고 PR을 만듭니다",
		mig.Title, item.Repo, item.Provider, branch), preview, nil
}

func applyPushMigration(tc *toolContext, args json.RawMessage) (string, error) {
	var in pushArgs
	if err := parseArgs(args, &in); err != nil {
		return "", err
	}
	mig, conn, err := tc.resolveMig(in.MigrationID, model.LevelMigrate)
	if err != nil {
		return "", err
	}
	item, err := tc.resolveIntegration(in.Integration, conn.ID)
	if err != nil {
		return "", err
	}
	if item.ConnectionID != "" && item.ConnectionID != conn.ID {
		return "", errors.New("이 연동은 다른 커넥션 전용입니다")
	}

	res, err := tc.srv.pushMigrationTo(tc.ctx, item, mig, conn, pushOptions{
		Actor: tc.user, IP: tc.ip, OpenPR: true, Via: "ai",
	})
	if err != nil {
		return "", err
	}
	return asJSON(map[string]any{
		"branch": res.Branch, "commit": res.CommitSHA,
		"pullRequest": res.PRURL, "files": res.Files,
	})
}
