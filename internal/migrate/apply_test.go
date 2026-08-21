package migrate

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dbstudio/internal/crypto"
	"dbstudio/internal/dbx"
	"dbstudio/internal/model"
	"dbstudio/internal/schema"
	"dbstudio/internal/store"
)

var runIntegration = flag.Bool("integration", false, "실제 DB 컨테이너를 대상으로 실행")

// ---------- 하네스 ----------

type harness struct {
	ctx     context.Context
	st      *store.Store
	runner  *Runner
	conn    *model.Connection
	secret  *model.Secret
	adapter dbx.Adapter
	author  *model.User
	// reviewer는 계획을 만들지 않은 다른 사람이다. 파괴적 변경은 본인 승인을 막으므로
	// 별도 검토자가 필요하다.
	reviewer *model.User
}

func skipUnlessIntegration(t *testing.T) {
	t.Helper()
	if !*runIntegration && os.Getenv("DBSTUDIO_INTEGRATION") == "" {
		t.Skip("통합 테스트: -integration 플래그 또는 DBSTUDIO_INTEGRATION 환경변수 필요")
	}
}

// setup은 메타 DB와 대상 커넥션을 준비한다.
//
// dbName은 이 패키지 전용 데이터베이스여야 한다. `go test ./...`는 패키지를 병렬로
// 실행하므로, dbx 통합 테스트와 같은 데이터베이스를 쓰면 상대의 테이블이 우리
// introspect 결과에 섞여 들어와 "적용 후 목표와 다르다"는 거짓 실패가 난다.
// (P4에서 dbx↔monitor 사이에 같은 문제를 겪었다.)
func setup(t *testing.T, kind model.DBKind, host string, port int, dbName, user, pass string, opts model.Options) *harness {
	t.Helper()
	ctx := context.Background()

	box, err := crypto.NewSecretBox(make([]byte, 32))
	if err != nil {
		t.Fatalf("secret box: %v", err)
	}
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "meta.db"), box)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	author, err := st.CreateUser(ctx, store.CreateUserParams{
		Username: "author", DisplayName: "작성자", Role: model.RoleAdmin, PasswordHash: "x",
	})
	if err != nil {
		t.Fatalf("create author: %v", err)
	}
	reviewer, err := st.CreateUser(ctx, store.CreateUserParams{
		Username: "reviewer", DisplayName: "검토자", Role: model.RoleAdmin, PasswordHash: "x",
	})
	if err != nil {
		t.Fatalf("create reviewer: %v", err)
	}

	pw := pass
	_, conn, err := st.CreateServerWithDatabase(ctx,
		store.SaveServerParams{
			Name: "target-" + string(kind), Kind: kind, DefaultEnvironment: model.EnvDev,
			Host: host, Port: port, Username: user,
			Options: opts, Tags: []string{}, Enabled: true, Password: &pw,
		},
		store.SaveConnectionParams{
			Name: "target-" + string(kind), Environment: model.EnvDev,
			DatabaseName: dbName, Tags: []string{}, Enabled: true,
		})
	if err != nil {
		t.Fatalf("create connection: %v", err)
	}
	secret, err := st.GetSecret(ctx, conn.ID)
	if err != nil {
		t.Fatalf("get secret: %v", err)
	}
	adapter, err := dbx.Get(kind)
	if err != nil {
		t.Fatalf("adapter: %v", err)
	}
	if _, err := adapter.Ping(ctx, dbx.Target{Conn: conn, Secret: secret}); err != nil {
		t.Skipf("접속 불가 (컨테이너 미실행?): %v", err)
	}

	return &harness{
		ctx: ctx, st: st,
		runner: New(st, "", slog.New(slog.NewTextHandler(io.Discard, nil))),
		conn:   conn, secret: secret, adapter: adapter,
		author: author, reviewer: reviewer,
	}
}

func (h *harness) target() dbx.Target {
	return dbx.Target{Conn: h.conn, Secret: h.secret}
}

// exec은 대상 DB에 직접 DDL을 실행한다 (테스트 준비와 외부 변경 재현용).
func (h *harness) exec(t *testing.T, stmts ...string) {
	t.Helper()
	report, err := h.adapter.ExecDDL(h.ctx, h.target(), stmts, dbx.ExecOptions{})
	if err != nil {
		t.Fatalf("exec 실패: %v", err)
	}
	if report.Error != "" {
		t.Fatalf("exec 문장 실패 (%d번째): %s\nSQL: %s",
			report.FailedIndex, report.Error, stmts[min(report.FailedIndex, len(stmts)-1)])
	}
}

// execIgnore은 실패를 무시한다 (정리용).
func (h *harness) execIgnore(stmts ...string) {
	_, _ = h.adapter.ExecDDL(h.ctx, h.target(), stmts, dbx.ExecOptions{ContinueOnError: true})
}

func (h *harness) introspect(t *testing.T) *schema.Schema {
	t.Helper()
	sc, err := h.adapter.Introspect(h.ctx, h.target())
	if err != nil {
		t.Fatalf("introspect: %v", err)
	}
	return sc
}

// createMigration은 현재 상태 → target 스키마의 마이그레이션을 만든다.
func (h *harness) createMigration(t *testing.T, title string, target *schema.Schema) *store.Migration {
	t.Helper()
	current := h.introspect(t)

	from, _, err := h.st.SaveSchemaVersion(h.ctx, store.SaveVersionParams{
		ConnectionID: h.conn.ID, Schema: current, Source: store.VersionSourceImport,
		Note: "기준선", AuthorID: h.author.ID, AuthorName: h.author.DisplayName,
	})
	if err != nil {
		t.Fatalf("save baseline version: %v", err)
	}

	diff := schema.Diff(current, target)
	if diff.IsEmpty() {
		t.Fatalf("변경이 없습니다 — 테스트 설정을 확인하세요")
	}
	plan := schema.BuildPlan(string(h.conn.Kind), diff)
	if len(plan.Up) == 0 {
		t.Fatalf("실행할 SQL이 없습니다: %v", plan.Warnings)
	}

	mig, err := h.st.CreateMigration(h.ctx, store.CreateMigrationParams{
		ConnectionID: h.conn.ID, Title: title, FromVersion: &from.ID,
		BaseFinger: current.Fingerprint(), TargetSchema: target,
		Plan: plan, Diff: diff, CreatedBy: h.author.ID,
	})
	if err != nil {
		t.Fatalf("create migration: %v", err)
	}
	return mig
}

// approve는 필요한 수만큼 승인해 실행 가능한 상태로 만든다.
func (h *harness) approve(t *testing.T, mig *store.Migration) *store.Migration {
	t.Helper()
	if err := h.st.SetMigrationStatus(h.ctx, mig.ID, store.MigrationInReview); err != nil {
		t.Fatalf("to in_review: %v", err)
	}
	need := RequiredApprovals(h.conn, mig.DestructiveCount)
	approvers := []*model.User{h.reviewer, h.author}
	for i := 0; i < need; i++ {
		u := approvers[i%len(approvers)]
		if err := h.st.AddMigrationReview(h.ctx, &store.MigrationReview{
			MigrationID: mig.ID, ReviewerID: u.ID, ReviewerName: u.DisplayName,
			Decision: store.ReviewApproved,
		}); err != nil {
			t.Fatalf("approve: %v", err)
		}
	}
	if err := h.st.SetMigrationStatus(h.ctx, mig.ID, store.MigrationApproved); err != nil {
		t.Fatalf("to approved: %v", err)
	}
	reloaded, err := h.st.GetMigration(h.ctx, mig.ID, true)
	if err != nil {
		t.Fatalf("reload migration: %v", err)
	}
	return reloaded
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ---------- dialect별 케이스 ----------

type dialectCase struct {
	name    string
	kind    model.DBKind
	host    string
	port    int
	dbName  string
	user    string
	pass    string
	options model.Options
	// bootstrap은 대상 데이터베이스 자체를 만드는 DDL이다.
	// adminDB에 접속해 실행한다 (아직 dbName이 없을 수 있으므로).
	adminDB   string
	bootstrap []string
	// setup은 기준 스키마를 만드는 DDL이다.
	setup []string
	clean []string
	// badSQL은 반드시 실패하는 문장이다 (실패 경로 검증용).
	badSQL string
}

func dialectCases(t *testing.T) []dialectCase {
	sqlitePath := strings.ReplaceAll(t.TempDir(), "\\", "/") + "/mig.db"
	return []dialectCase{
		{
			// 이 패키지 전용 데이터베이스를 쓴다. appdb는 dbx 통합 테스트가 쓰므로
			// 공유하면 병렬 실행에서 서로의 테이블이 섞인다.
			name: "mysql", kind: model.KindMySQL,
			host: "127.0.0.1", port: 13306, dbName: "dbstudio_migrate",
			user: "root", pass: "rootpw123",
			options:   model.Options{},
			adminDB:   "appdb",
			bootstrap: []string{"CREATE DATABASE IF NOT EXISTS dbstudio_migrate DEFAULT CHARSET=utf8mb4"},
			setup: []string{
				`DROP TABLE IF EXISTS mg_orders`,
				`DROP TABLE IF EXISTS mg_users`,
				"CREATE TABLE mg_users (id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY, " +
					"email VARCHAR(255) NOT NULL, nickname VARCHAR(60) NULL) ENGINE=InnoDB",
			},
			clean:  []string{`DROP TABLE IF EXISTS mg_orders`, `DROP TABLE IF EXISTS mg_users`},
			badSQL: "ALTER TABLE mg_users ADD COLUMN broken NOTATYPE(1)",
		},
		{
			name: "postgres", kind: model.KindPostgres,
			host: "127.0.0.1", port: 15432, dbName: "appdb", user: "postgres", pass: "rootpw123",
			options: model.Options{"sslmode": "disable", "search_path": "mig_test"},
			setup: []string{
				`DROP SCHEMA IF EXISTS mig_test CASCADE`,
				`CREATE SCHEMA mig_test`,
				`CREATE TABLE mig_test.mg_users (id BIGSERIAL PRIMARY KEY, ` +
					`email VARCHAR(255) NOT NULL, nickname VARCHAR(60))`,
			},
			clean:  []string{`DROP SCHEMA IF EXISTS mig_test CASCADE`},
			badSQL: `ALTER TABLE mig_test.mg_users ADD COLUMN broken NOTATYPE`,
		},
		{
			name: "sqlite", kind: model.KindSQLite,
			dbName: sqlitePath, options: model.Options{},
			setup: []string{
				`DROP TABLE IF EXISTS mg_users`,
				`CREATE TABLE mg_users (id INTEGER PRIMARY KEY AUTOINCREMENT, ` +
					`email TEXT NOT NULL, nickname TEXT)`,
			},
			clean:  []string{`DROP TABLE IF EXISTS mg_users`},
			badSQL: `ALTER TABLE mg_users ADD COLUMN broken NOTATYPE CHECK (`,
		},
	}
}

func (c dialectCase) newHarness(t *testing.T) *harness {
	// 전용 데이터베이스가 필요하면 관리용 DB에 붙어 먼저 만든다.
	if len(c.bootstrap) > 0 {
		admin := setup(t, c.kind, c.host, c.port, c.adminDB, c.user, c.pass, c.options)
		admin.exec(t, c.bootstrap...)
	}
	h := setup(t, c.kind, c.host, c.port, c.dbName, c.user, c.pass, c.options)
	h.execIgnore(c.clean...)
	h.exec(t, c.setup...)
	t.Cleanup(func() { h.execIgnore(c.clean...) })
	return h
}

// addColumnTarget은 컬럼과 인덱스를 추가한 목표 스키마를 만든다.
//
// 주석은 SQLite에서 제외한다. SQLite에는 주석을 저장할 곳이 없어 절대 수렴하지
// 않으므로, 왕복 검증에 섞으면 이 테스트가 항상 실패한다. 그 성질 자체는
// TestUnsupportedChangeIsReported가 따로 검증한다.
func addColumnTarget(t *testing.T, h *harness) *schema.Schema {
	t.Helper()
	target := h.introspect(t)
	users := findTable(target, "mg_users")
	if users == nil {
		t.Fatalf("mg_users 를 찾을 수 없습니다: %v", tableNames(target))
	}
	users.Columns = append(users.Columns, &schema.Column{
		Name: "created_at", Position: len(users.Columns) + 1,
		Type: schema.LogicalType{Base: schema.TypeTimestamp}, RawType: "timestamp",
		Nullable: true,
	})
	users.Indexes = append(users.Indexes, &schema.Index{
		Name: "ux_mg_users_email", Unique: true,
		Columns: []schema.IndexPart{{Column: "email"}},
	})
	if h.conn.Kind != model.KindSQLite {
		users.Comment = "마이그레이션 테스트"
	}
	return target
}

func findTable(sc *schema.Schema, name string) *schema.Table {
	for _, t := range sc.Tables {
		if strings.EqualFold(t.Name, name) {
			return t
		}
	}
	return nil
}

func tableNames(sc *schema.Schema) []string {
	out := []string{}
	for _, t := range sc.Tables {
		out = append(out, t.Display())
	}
	return out
}

// ---------- 테스트 ----------

// TestApplyAndRollback은 마이그레이션의 왕복을 실제 DB에서 검증한다.
//
// 이것이 P7의 핵심 성질이다: 적용하면 목표 구조가 되고, 롤백하면 원래 구조로
// 정확히 돌아와야 한다. 지문으로 비교하므로 "비슷하게"가 아니라 "같게"를 요구한다.
func TestApplyAndRollback(t *testing.T) {
	skipUnlessIntegration(t)

	for _, tc := range dialectCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			h := tc.newHarness(t)

			before := h.introspect(t)
			beforeFinger := before.Fingerprint()

			mig := h.createMigration(t, "컬럼과 인덱스 추가", addColumnTarget(t, h))
			mig = h.approve(t, mig)

			res, err := h.runner.Apply(h.ctx, ApplyParams{
				Conn: h.conn, Secret: h.secret, Mig: mig, Actor: h.author,
			})
			if err != nil {
				t.Fatalf("apply: %v", err)
			}
			if res.Status != store.MigrationApplied {
				t.Fatalf("상태 = %s, 오류 = %s", res.Status, res.Error)
			}
			if len(res.PostDiff) > 0 {
				t.Errorf("적용 후 목표와 %d건 다릅니다: %v", len(res.PostDiff), res.PostDiff)
			}
			if res.Version == nil || res.Version.Source != store.VersionSourceMigrated {
				t.Errorf("버전이 등록되지 않았습니다: %+v", res.Version)
			}

			// 실제 DB에 반영되었는지 직접 확인한다.
			applied := h.introspect(t)
			users := findTable(applied, "mg_users")
			if users == nil || users.Column("created_at") == nil {
				t.Fatalf("컬럼이 추가되지 않았습니다: %v", tableNames(applied))
			}
			hasIndex := false
			for _, idx := range users.Indexes {
				if strings.EqualFold(idx.Name, "ux_mg_users_email") && idx.Unique {
					hasIndex = true
				}
			}
			if !hasIndex {
				t.Error("고유 인덱스가 추가되지 않았습니다")
			}

			// 저장된 실행 기록 확인
			stored, err := h.st.GetMigration(h.ctx, mig.ID, true)
			if err != nil {
				t.Fatalf("reload: %v", err)
			}
			if stored.Status != store.MigrationApplied {
				t.Errorf("저장된 상태 = %s", stored.Status)
			}
			if stored.AppliedStatements != len(mig.Plan.Up) {
				t.Errorf("적용 문장 수 = %d, 계획 = %d", stored.AppliedStatements, len(mig.Plan.Up))
			}
			if len(stored.ExecutionLog) == 0 {
				t.Error("실행 기록이 비어 있습니다")
			}
			if stored.ToVersion == nil {
				t.Error("결과 버전이 연결되지 않았습니다")
			}

			// 롤백
			rollbackRes, err := h.runner.Rollback(h.ctx, ApplyParams{
				Conn: h.conn, Secret: h.secret, Mig: stored, Actor: h.author,
			}, false)
			if err != nil {
				t.Fatalf("rollback: %v", err)
			}
			if rollbackRes.Status != store.MigrationRolledBack {
				t.Fatalf("롤백 상태 = %s, 오류 = %s", rollbackRes.Status, rollbackRes.Error)
			}

			after := h.introspect(t)
			if got := after.Fingerprint(); got != beforeFinger {
				t.Errorf("롤백 후 구조가 원래와 다릅니다:\n  전 %s\n  후 %s\n  차이: %v",
					beforeFinger, got, summaries(schema.Diff(before, after)))
			}
			if rollbackRes.Version == nil || rollbackRes.Version.Source != store.VersionSourceRollback {
				t.Errorf("롤백 버전이 등록되지 않았습니다: %+v", rollbackRes.Version)
			}
		})
	}
}

func summaries(diff *schema.DiffResult) []string {
	out := []string{}
	for _, c := range diff.Changes {
		out = append(out, c.Summary)
	}
	return out
}

// 계획을 만든 뒤 대상 DB가 바뀌면 실행을 막아야 한다.
// 이 검사가 없으면 오래된 계획이 남의 변경을 덮어쓴다.
func TestPrecheckBlocksOnDrift(t *testing.T) {
	skipUnlessIntegration(t)

	for _, tc := range dialectCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			h := tc.newHarness(t)
			mig := h.createMigration(t, "드리프트 검증", addColumnTarget(t, h))
			mig = h.approve(t, mig)

			// 앱 밖에서 스키마를 바꾼다.
			h.exec(t, externalChange(tc))

			pc, err := h.runner.Check(h.ctx, h.conn, h.secret, mig)
			if err != nil {
				t.Fatalf("check: %v", err)
			}
			if !pc.Drifted {
				t.Errorf("외부 변경을 감지하지 못했습니다 (기대 %s, 실제 %s)",
					pc.ExpectedFingerprint, pc.ActualFingerprint)
			}
			if pc.OK {
				t.Error("드리프트가 있는데 실행 가능으로 판정했습니다")
			}
			if len(pc.DriftChanges) == 0 {
				t.Error("무엇이 달라졌는지 설명이 없습니다")
			}

			// 실제 실행도 막혀야 한다.
			_, err = h.runner.Apply(h.ctx, ApplyParams{
				Conn: h.conn, Secret: h.secret, Mig: mig, Actor: h.author,
			})
			var blocked *BlockedError
			if err == nil {
				t.Fatal("드리프트 상태에서 실행이 성공했습니다")
			}
			if !asBlocked(err, &blocked) {
				t.Fatalf("오류 종류 = %T (%v), BlockedError 를 기대했습니다", err, err)
			}
		})
	}
}

func externalChange(tc dialectCase) string {
	switch tc.kind {
	case model.KindPostgres:
		return `ALTER TABLE mig_test.mg_users ADD COLUMN external_col INT`
	default:
		return `ALTER TABLE mg_users ADD COLUMN external_col INT`
	}
}

func asBlocked(err error, target **BlockedError) bool {
	b, ok := err.(*BlockedError)
	if ok {
		*target = b
	}
	return ok
}

// 승인 없이 실행할 수 없어야 한다.
func TestApprovalGate(t *testing.T) {
	skipUnlessIntegration(t)

	tc := dialectCases(t)[2] // sqlite: 가장 빠르다
	h := tc.newHarness(t)
	mig := h.createMigration(t, "승인 게이트", addColumnTarget(t, h))

	// 초안 상태로 실행 시도
	_, err := h.runner.Apply(h.ctx, ApplyParams{
		Conn: h.conn, Secret: h.secret, Mig: mig, Actor: h.author,
	})
	var blocked *BlockedError
	if err == nil || !asBlocked(err, &blocked) {
		t.Fatalf("초안 상태에서 실행이 막히지 않았습니다: %v", err)
	}
	joined := strings.Join(blocked.Blockers, " ")
	if !strings.Contains(joined, "승인") {
		t.Errorf("차단 사유에 승인 관련 설명이 없습니다: %v", blocked.Blockers)
	}

	// 리뷰 중이지만 승인 수 부족
	if err := h.st.SetMigrationStatus(h.ctx, mig.ID, store.MigrationInReview); err != nil {
		t.Fatalf("to in_review: %v", err)
	}
	reloaded, _ := h.st.GetMigration(h.ctx, mig.ID, true)
	if _, err := h.runner.Apply(h.ctx, ApplyParams{
		Conn: h.conn, Secret: h.secret, Mig: reloaded, Actor: h.author,
	}); err == nil {
		t.Error("리뷰 중 상태에서 실행이 성공했습니다")
	}

	// 반려가 남아 있으면 막혀야 한다.
	if err := h.st.AddMigrationReview(h.ctx, &store.MigrationReview{
		MigrationID: mig.ID, ReviewerID: h.reviewer.ID, ReviewerName: "검토자",
		Decision: store.ReviewRejected, Comment: "이 변경은 위험합니다",
	}); err != nil {
		t.Fatalf("reject: %v", err)
	}
	reloaded, _ = h.st.GetMigration(h.ctx, mig.ID, true)
	pc, err := h.runner.Check(h.ctx, h.conn, h.secret, reloaded)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if pc.OK {
		t.Error("반려가 남아 있는데 실행 가능으로 판정했습니다")
	}
}

// 파괴적 변경은 승인 2명을 요구해야 한다.
func TestDestructiveRequiresTwoApprovals(t *testing.T) {
	skipUnlessIntegration(t)

	tc := dialectCases(t)[2] // sqlite
	h := tc.newHarness(t)

	// nickname 컬럼을 지우는 목표 (파괴적)
	target := h.introspect(t)
	users := findTable(target, "mg_users")
	kept := []*schema.Column{}
	for _, c := range users.Columns {
		if strings.EqualFold(c.Name, "nickname") {
			continue
		}
		kept = append(kept, c)
	}
	users.Columns = kept

	mig := h.createMigration(t, "컬럼 삭제", target)
	if mig.DestructiveCount == 0 {
		t.Fatalf("컬럼 삭제가 파괴적으로 분류되지 않았습니다")
	}
	if got := RequiredApprovals(h.conn, mig.DestructiveCount); got != 2 {
		t.Errorf("필요 승인 수 = %d, 기대값 2", got)
	}

	if err := h.st.SetMigrationStatus(h.ctx, mig.ID, store.MigrationInReview); err != nil {
		t.Fatalf("to in_review: %v", err)
	}
	// 승인 1명
	if err := h.st.AddMigrationReview(h.ctx, &store.MigrationReview{
		MigrationID: mig.ID, ReviewerID: h.reviewer.ID, ReviewerName: "검토자",
		Decision: store.ReviewApproved,
	}); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if err := h.st.SetMigrationStatus(h.ctx, mig.ID, store.MigrationApproved); err != nil {
		t.Fatalf("to approved: %v", err)
	}
	reloaded, _ := h.st.GetMigration(h.ctx, mig.ID, true)
	pc, err := h.runner.Check(h.ctx, h.conn, h.secret, reloaded)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if pc.OK {
		t.Errorf("승인 1명으로 실행 가능해졌습니다 (필요 %d, 현재 %d)",
			pc.RequiredApprovals, pc.Approvals)
	}
	// 같은 사람이 두 번 승인해도 1명이어야 한다.
	if err := h.st.AddMigrationReview(h.ctx, &store.MigrationReview{
		MigrationID: mig.ID, ReviewerID: h.reviewer.ID, ReviewerName: "검토자",
		Decision: store.ReviewApproved,
	}); err != nil {
		t.Fatalf("approve again: %v", err)
	}
	reloaded, _ = h.st.GetMigration(h.ctx, mig.ID, true)
	if got := store.ApprovalCount(reloaded.Reviews); got != 1 {
		t.Errorf("같은 사람의 반복 승인이 %d명으로 계산되었습니다", got)
	}

	// 다른 사람이 승인하면 통과한다.
	if err := h.st.AddMigrationReview(h.ctx, &store.MigrationReview{
		MigrationID: mig.ID, ReviewerID: h.author.ID, ReviewerName: "작성자",
		Decision: store.ReviewApproved,
	}); err != nil {
		t.Fatalf("second approve: %v", err)
	}
	reloaded, _ = h.st.GetMigration(h.ctx, mig.ID, true)
	pc, err = h.runner.Check(h.ctx, h.conn, h.secret, reloaded)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if !pc.OK {
		t.Errorf("승인 2명인데 막혔습니다: %v", pc.Blockers)
	}
	// 파괴적 변경 경고는 반드시 있어야 한다.
	if !containsSubstring(pc.Warnings, "데이터 손실") {
		t.Errorf("파괴적 변경 경고가 없습니다: %v", pc.Warnings)
	}
}

func containsSubstring(list []string, want string) bool {
	for _, s := range list {
		if strings.Contains(s, want) {
			return true
		}
	}
	return false
}

// 실패 시 동작은 DB 종류에 따라 달라야 한다.
// 트랜잭션 DDL을 지원하는 DB는 전부 되돌아가고, 그렇지 않은 DB는 어디까지
// 적용됐는지 기록되어야 한다.
func TestFailureBehaviorPerDialect(t *testing.T) {
	skipUnlessIntegration(t)

	for _, tc := range dialectCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			h := tc.newHarness(t)
			beforeFinger := h.introspect(t).Fingerprint()

			mig := h.createMigration(t, "실패 경로", addColumnTarget(t, h))
			mig = h.approve(t, mig)

			// 계획 마지막에 반드시 실패하는 문장을 끼워 넣는다.
			mig.Plan.Up = append(mig.Plan.Up, schema.Statement{
				SQL: tc.badSQL, Kind: schema.AddColumn, Table: "mg_users",
			})

			res, err := h.runner.Apply(h.ctx, ApplyParams{
				Conn: h.conn, Secret: h.secret, Mig: mig, Actor: h.author,
			})
			if err != nil {
				t.Fatalf("apply: %v", err)
			}
			if res.Status != store.MigrationFailed {
				t.Fatalf("상태 = %s (실패를 기대)", res.Status)
			}
			if res.Error == "" {
				t.Error("실패 사유가 비어 있습니다")
			}

			transactional := dbx.TransactionalDDL(string(tc.kind))
			if res.Report.TransactionUsed != transactional {
				t.Errorf("트랜잭션 사용 = %t, 기대값 %t", res.Report.TransactionUsed, transactional)
			}

			after := h.introspect(t)
			if transactional {
				// 전부 되돌아가야 한다.
				if got := after.Fingerprint(); got != beforeFinger {
					t.Errorf("트랜잭션 DDL인데 부분 적용이 남았습니다: %v",
						summaries(schema.Diff(h.introspect(t), after)))
				}
				if res.Report.Applied != 0 {
					t.Errorf("되돌린 뒤 적용 수 = %d, 기대값 0", res.Report.Applied)
				}
			} else {
				// 부분 적용이 남고, 그 사실이 기록되어야 한다.
				if res.Report.Applied == 0 {
					t.Error("실패 전 문장이 하나도 적용되지 않았습니다 (설정 확인)")
				}
				stored, err := h.st.GetMigration(h.ctx, mig.ID, true)
				if err != nil {
					t.Fatalf("reload: %v", err)
				}
				if stored.AppliedStatements != res.Report.Applied {
					t.Errorf("기록된 적용 수 = %d, 실제 = %d",
						stored.AppliedStatements, res.Report.Applied)
				}
				if !containsSubstring(res.Warnings, "적용된 상태로 남아") {
					t.Errorf("부분 적용 경고가 없습니다: %v", res.Warnings)
				}
				// 부분 적용은 오류 무시 롤백으로 정리할 수 있어야 한다.
				stored.Plan.Down = mig.Plan.Down
				rb, err := h.runner.Rollback(h.ctx, ApplyParams{
					Conn: h.conn, Secret: h.secret, Mig: stored, Actor: h.author,
				}, true)
				if err != nil {
					t.Fatalf("rollback: %v", err)
				}
				if rb.Status != store.MigrationRolledBack {
					t.Errorf("정리 롤백 상태 = %s", rb.Status)
				}
				if got := h.introspect(t).Fingerprint(); got != beforeFinger {
					t.Errorf("정리 후에도 구조가 원래와 다릅니다: %v",
						summaries(schema.Diff(after, h.introspect(t))))
				}
			}
		})
	}
}

// 대상 DB가 표현할 수 없는 변경은 조용히 사라지지 않고 알려야 한다.
//
// SQLite에는 테이블 주석을 저장할 곳이 없다. 그래서 ERD에서 주석을 달면 그 차이는
// 영원히 수렴하지 않는데, 아무 설명이 없으면 사용자는 앱이 고장난 줄 알고 같은
// 마이그레이션을 반복해서 만든다.
func TestUnsupportedChangeIsReported(t *testing.T) {
	skipUnlessIntegration(t)

	tc := dialectCases(t)[2] // sqlite
	h := tc.newHarness(t)

	target := h.introspect(t)
	findTable(target, "mg_users").Comment = "SQLite가 저장할 수 없는 주석"

	current := h.introspect(t)
	diff := schema.Diff(current, target)
	if diff.IsEmpty() {
		t.Fatal("주석 변경이 차이로 잡히지 않았습니다")
	}
	plan := schema.BuildPlan("sqlite", diff)
	if !containsSubstring(plan.Warnings, "주석") {
		t.Errorf("계획에 주석 미지원 경고가 없습니다: %v", plan.Warnings)
	}
	// 실행할 SQL이 없으므로 마이그레이션 자체를 만들 수 없어야 한다 —
	// API가 이 조건을 400으로 막는다.
	if len(plan.Up) != 0 {
		t.Errorf("SQLite 주석 변경에 SQL이 생성되었습니다: %v", plan.Up)
	}
}

// 실행 기록의 문장별 로그가 실제 SQL과 순서를 담고 있어야 한다.
func TestExecutionLogDetail(t *testing.T) {
	skipUnlessIntegration(t)

	tc := dialectCases(t)[2] // sqlite
	h := tc.newHarness(t)
	mig := h.createMigration(t, "실행 기록", addColumnTarget(t, h))
	mig = h.approve(t, mig)

	res, err := h.runner.Apply(h.ctx, ApplyParams{
		Conn: h.conn, Secret: h.secret, Mig: mig, Actor: h.author,
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.Status != store.MigrationApplied {
		t.Fatalf("상태 = %s: %s", res.Status, res.Error)
	}

	stored, err := h.st.GetMigration(h.ctx, mig.ID, true)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	for i, step := range stored.ExecutionLog {
		if step.Index != i {
			t.Errorf("%d번째 기록의 index = %d", i, step.Index)
		}
		if strings.TrimSpace(step.SQL) == "" {
			t.Errorf("%d번째 기록에 SQL이 없습니다", i)
		}
		if step.Error != "" {
			t.Errorf("%d번째 기록에 오류가 있습니다: %s", i, step.Error)
		}
	}
	if len(stored.ExecutionLog) != len(mig.Plan.Up) {
		t.Errorf("기록 수 = %d, 계획 문장 수 = %d", len(stored.ExecutionLog), len(mig.Plan.Up))
	}
}

// 버전 번호는 커넥션별로 1부터 증가하고, 같은 구조는 새 버전을 만들지 않아야 한다.
func TestVersionNumbering(t *testing.T) {
	skipUnlessIntegration(t)

	tc := dialectCases(t)[2] // sqlite
	h := tc.newHarness(t)
	current := h.introspect(t)

	v1, created, err := h.st.SaveSchemaVersion(h.ctx, store.SaveVersionParams{
		ConnectionID: h.conn.ID, Schema: current, Source: store.VersionSourceImport,
	})
	if err != nil || !created || v1.VersionNo != 1 {
		t.Fatalf("첫 버전: v=%+v created=%t err=%v", v1, created, err)
	}
	// 같은 구조 재등록
	same, created, err := h.st.SaveSchemaVersion(h.ctx, store.SaveVersionParams{
		ConnectionID: h.conn.ID, Schema: current, Source: store.VersionSourceExternal,
	})
	if err != nil {
		t.Fatalf("재등록: %v", err)
	}
	if created {
		t.Error("같은 구조인데 새 버전을 만들었습니다")
	}
	if same.VersionNo != 1 {
		t.Errorf("버전 번호 = %d", same.VersionNo)
	}

	// 구조를 바꾸고 등록
	h.exec(t, `ALTER TABLE mg_users ADD COLUMN extra TEXT`)
	changed := h.introspect(t)
	v2, created, err := h.st.SaveSchemaVersion(h.ctx, store.SaveVersionParams{
		ConnectionID: h.conn.ID, Schema: changed, Source: store.VersionSourceExternal,
		ChangeSummary: summaries(schema.Diff(current, changed)),
	})
	if err != nil || !created {
		t.Fatalf("두 번째 버전: created=%t err=%v", created, err)
	}
	if v2.VersionNo != 2 {
		t.Errorf("버전 번호 = %d, 기대값 2", v2.VersionNo)
	}
	if len(v2.ChangeSummary) == 0 {
		t.Error("변경 요약이 비어 있습니다")
	}

	list, err := h.st.ListSchemaVersions(h.ctx, h.conn.ID, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 || list[0].VersionNo != 2 {
		t.Errorf("목록 = %d건, 첫 항목 = %d", len(list), list[0].VersionNo)
	}
	// 목록은 본문 대신 통계를 담아야 한다.
	if list[0].Schema != nil {
		t.Error("목록에 스키마 본문이 포함되어 있습니다")
	}
	if list[0].Stats == nil || list[0].Stats.Tables == 0 {
		t.Errorf("통계가 없습니다: %+v", list[0].Stats)
	}
}

func TestStatusTransitions(t *testing.T) {
	cases := []struct {
		from, to string
		want     bool
	}{
		{store.MigrationDraft, store.MigrationInReview, true},
		{store.MigrationDraft, store.MigrationApproved, false},
		{store.MigrationDraft, store.MigrationApplied, false},
		{store.MigrationInReview, store.MigrationApproved, true},
		{store.MigrationInReview, store.MigrationRejected, true},
		{store.MigrationInReview, store.MigrationApplied, false},
		{store.MigrationApproved, store.MigrationApplied, true},
		{store.MigrationApproved, store.MigrationRolledBack, false},
		{store.MigrationApplied, store.MigrationRolledBack, true},
		{store.MigrationApplied, store.MigrationDraft, false},
		{store.MigrationRolledBack, store.MigrationApplied, false},
		{store.MigrationRejected, store.MigrationDraft, true},
		{store.MigrationRejected, store.MigrationApproved, false},
		{store.MigrationFailed, store.MigrationDraft, true},
		{store.MigrationFailed, store.MigrationApplied, false},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%s→%s", tc.from, tc.to), func(t *testing.T) {
			if got := store.CanTransition(tc.from, tc.to); got != tc.want {
				t.Errorf("CanTransition(%s, %s) = %t, 기대값 %t", tc.from, tc.to, got, tc.want)
			}
		})
	}
}

func TestRequiredApprovalsRules(t *testing.T) {
	dev := &model.Connection{Environment: model.EnvDev}
	prod := &model.Connection{Environment: model.EnvProd}

	if got := RequiredApprovals(dev, 0); got != 1 {
		t.Errorf("개발 + 비파괴 = %d, 기대값 1", got)
	}
	if got := RequiredApprovals(dev, 2); got != 2 {
		t.Errorf("개발 + 파괴적 = %d, 기대값 2", got)
	}
	if got := RequiredApprovals(prod, 0); got != 2 {
		t.Errorf("운영 = %d, 기대값 2", got)
	}
	if got := RequiredApprovals(prod, 5); got != 2 {
		t.Errorf("운영 + 파괴적 = %d, 기대값 2", got)
	}
}
