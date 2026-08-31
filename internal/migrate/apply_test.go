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

// 파괴적 변경도 승인 1명이면 실행할 수 있고, 대신 경고가 반드시 남아야 한다.
//
// 승인 수를 안전장치로 쓰지 않기로 했으므로(RequiredApprovals) 남는 장치는 경고와
// 사전 검사다. 그것마저 없으면 컬럼을 지우는 계획이 아무 표시 없이 지나간다.
func TestDestructiveNeedsOneApprovalAndWarns(t *testing.T) {
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
	if got := RequiredApprovals(h.conn, mig.DestructiveCount); got != 1 {
		t.Errorf("필요 승인 수 = %d, 기대값 1", got)
	}

	if err := h.st.SetMigrationStatus(h.ctx, mig.ID, store.MigrationInReview); err != nil {
		t.Fatalf("to in_review: %v", err)
	}
	// 아직 아무도 승인하지 않았다 — 파괴적이든 아니든 승인 없이는 막힌다.
	reloaded, _ := h.st.GetMigration(h.ctx, mig.ID, true)
	pc, err := h.runner.Check(h.ctx, h.conn, h.secret, reloaded)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if pc.OK {
		t.Errorf("승인 0명인데 실행 가능으로 판정했습니다 (필요 %d)", pc.RequiredApprovals)
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
	reloaded, _ = h.st.GetMigration(h.ctx, mig.ID, true)
	pc, err = h.runner.Check(h.ctx, h.conn, h.secret, reloaded)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if !pc.OK {
		t.Errorf("승인 1명인데 막혔습니다: %v", pc.Blockers)
	}
	// 같은 사람이 두 번 승인해도 1명이다. 사람 단위로 세는 규칙은 그대로다.
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

// 롤백한 계획은 승인을 지닌 채 다시 실행할 수 있어야 한다.
//
// 롤백은 "이 변경을 물린다"이지 "이 계획을 버린다"가 아니다. 실행 중 문제가 생겨
// 일단 되돌렸다가 원인을 고친 뒤 같은 변경을 다시 넣는 일이 흔하다. 그때마다 계획을
// 새로 만들고 승인을 다시 받아야 한다면 사람들은 롤백을 누르기를 망설이게 된다.
//
// 여기서 실제로 확인하는 것은 두 가지다: 다시 실행한 뒤 DB가 목표 구조가 되는가,
// 그리고 그 사이 승인 기록이 살아 있는가.
func TestRerunAfterRollbackAppliesAgain(t *testing.T) {
	skipUnlessIntegration(t)

	for _, tc := range dialectCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			h := tc.newHarness(t)
			beforeFinger := h.introspect(t).Fingerprint()

			mig := h.createMigration(t, "재실행", addColumnTarget(t, h))
			mig = h.approve(t, mig)
			targetFinger := ""

			if _, err := h.runner.Apply(h.ctx, ApplyParams{
				Conn: h.conn, Secret: h.secret, Mig: mig, Actor: h.author,
			}); err != nil {
				t.Fatalf("첫 실행: %v", err)
			}
			targetFinger = h.introspect(t).Fingerprint()
			if targetFinger == beforeFinger {
				t.Fatalf("실행했는데 구조가 그대로입니다 (설정 확인)")
			}

			applied, err := h.st.GetMigration(h.ctx, mig.ID, true)
			if err != nil {
				t.Fatalf("reload: %v", err)
			}
			if _, err := h.runner.Rollback(h.ctx, ApplyParams{
				Conn: h.conn, Secret: h.secret, Mig: applied, Actor: h.author,
			}, false); err != nil {
				t.Fatalf("롤백: %v", err)
			}
			if got := h.introspect(t).Fingerprint(); got != beforeFinger {
				t.Fatalf("롤백 뒤 구조가 원래와 다릅니다")
			}

			// 여기가 이번 변경이다: 롤백된 계획을 다시 실행 대기로 되돌린다.
			if err := h.st.SetMigrationStatus(h.ctx, mig.ID, store.MigrationApproved); err != nil {
				t.Fatalf("다시 실행 대기로: %v", err)
			}
			again, err := h.st.GetMigration(h.ctx, mig.ID, true)
			if err != nil {
				t.Fatalf("reload: %v", err)
			}
			// 승인 기록이 살아 있어야 한다. 닫기 후 다시 열기와 다른 점이다.
			if store.ApprovalCount(again.Reviews) == 0 {
				t.Fatalf("다시 실행 대기로 되돌렸는데 승인이 사라졌습니다: %+v", again.Reviews)
			}

			// 사전 검사가 통과해야 한다. 롤백으로 DB가 계획의 기준 구조로 돌아왔으므로
			// 기준 지문이 다시 맞는다 — 이것이 재실행이 안전한 까닭이다.
			pc, err := h.runner.Check(h.ctx, h.conn, h.secret, again)
			if err != nil {
				t.Fatalf("사전 검사: %v", err)
			}
			if !pc.OK {
				t.Fatalf("다시 실행할 수 없습니다: %v", pc.Blockers)
			}

			res, err := h.runner.Apply(h.ctx, ApplyParams{
				Conn: h.conn, Secret: h.secret, Mig: again, Actor: h.author,
			})
			if err != nil {
				t.Fatalf("재실행: %v", err)
			}
			if res.Status != store.MigrationApplied {
				t.Fatalf("재실행 상태 = %s, 오류 = %s", res.Status, res.Error)
			}
			if got := h.introspect(t).Fingerprint(); got != targetFinger {
				t.Errorf("재실행 뒤 구조가 첫 실행 때와 다릅니다:\n  첫 %s\n  다시 %s",
					targetFinger, got)
			}
		})
	}
}

// 미리 실행해 보기: 깨진 SQL을 잡고, 대상 DB는 건드리지 않고, 뒷정리까지 한다.
//
// 계획을 만들고 나서야 SQL이 깨진 것을 알면 한 바퀴가 통째로 낭비된다(만들고, 리뷰
// 받고, 실행하고, 실패를 보고, 고쳐서 다시 처음부터). 그 한 바퀴를 없애는 장치이므로
// 세 가지가 모두 참이어야 값어치가 있다: 진짜로 잡는가, 대상 DB에 자국을 남기지
// 않는가, 검사용 DB를 남기지 않는가.
func TestDryRunCatchesBadSQL(t *testing.T) {
	skipUnlessIntegration(t)

	for _, tc := range dialectCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			h := tc.newHarness(t)
			adapter, err := dbx.Get(h.conn.Kind)
			if err != nil {
				t.Fatalf("adapter: %v", err)
			}
			target := dbx.Target{Conn: h.conn, Secret: h.secret}
			before := h.introspect(t)
			seed := dryRunSeed(t, string(h.conn.Kind), before)

			// 첫 문장은 되고, 둘째 문장은 반드시 실패한다.
			good := "CREATE TABLE mg_dryrun_probe (id INT)"
			if h.conn.Kind == model.KindPostgres {
				good = "CREATE TABLE mig_test.mg_dryrun_probe (id INT)"
			}
			rep, err := dbx.DryRunDDL(h.ctx, adapter, target, seed,
				[]string{good, tc.badSQL}, dbx.ExecOptions{})
			if err != nil {
				t.Fatalf("미리 실행: %v", err)
			}
			if rep.Skipped != "" {
				t.Fatalf("검사하지 못했습니다: %s", rep.Skipped)
			}
			if rep.OK {
				t.Fatalf("깨진 SQL을 통과시켰습니다: %+v", rep.Steps)
			}
			if rep.FailedIndex != 1 {
				t.Errorf("막힌 자리 = %d, 기대 1 (첫 문장은 되어야 합니다)", rep.FailedIndex)
			}
			if rep.Error == "" {
				t.Error("막힌 사유가 비어 있습니다")
			}

			// 대상 DB에는 아무 자국도 없어야 한다. 이것이 깨지면 "미리 검사"는
			// 검사가 아니라 실행이다.
			after := h.introspect(t)
			if got := after.Fingerprint(); got != before.Fingerprint() {
				t.Errorf("대상 DB가 바뀌었습니다: %v", summaries(schema.Diff(before, after)))
			}
			if findTableOrNil(after, "mg_dryrun_probe") != nil {
				t.Error("검사용 테이블이 대상 DB에 남았습니다")
			}

			// 그림자 DB도 남지 않아야 한다. 남으면 서버에 정체 모를 DB가 쌓인다.
			if leftovers := dryRunLeftovers(t, h); len(leftovers) > 0 {
				t.Errorf("그림자 DB가 남았습니다: %v", leftovers)
			}
		})
	}
}

// 멀쩡한 계획은 통과해야 한다. 무엇이든 막아 세우는 검사는 곧 꺼진다.
func TestDryRunPassesGoodPlan(t *testing.T) {
	skipUnlessIntegration(t)

	for _, tc := range dialectCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			h := tc.newHarness(t)
			adapter, err := dbx.Get(h.conn.Kind)
			if err != nil {
				t.Fatalf("adapter: %v", err)
			}
			before := h.introspect(t)
			target := h.introspect(t)
			findTable(target, "mg_users").Columns = append(
				findTable(target, "mg_users").Columns,
				&schema.Column{
					Name: "memo", Type: schema.LogicalType{Base: schema.TypeText}, Nullable: true,
				})
			plan := schema.BuildPlan(string(h.conn.Kind), schema.Diff(before, target))
			if len(plan.Up) == 0 {
				t.Fatalf("계획이 비었습니다: %v", plan.Warnings)
			}

			rep, err := dbx.DryRunDDL(h.ctx, adapter, dbx.Target{Conn: h.conn, Secret: h.secret},
				dryRunSeed(t, string(h.conn.Kind), before), statementList(plan.Up),
				dbx.ExecOptions{})
			if err != nil {
				t.Fatalf("미리 실행: %v", err)
			}
			if rep.Skipped != "" {
				t.Fatalf("검사하지 못했습니다: %s", rep.Skipped)
			}
			if !rep.OK {
				t.Fatalf("멀쩡한 계획이 막혔습니다: %s (%d번째)", rep.Error, rep.FailedIndex+1)
			}
			if got := h.introspect(t).Fingerprint(); got != before.Fingerprint() {
				t.Error("검사가 대상 DB를 바꿨습니다")
			}
		})
	}
}

// dryRunSeed는 그림자 DB에 기준 구조를 세우는 문장이다(API의 seedStatements와 같은 일).
func dryRunSeed(t *testing.T, dialect string, from *schema.Schema) []string {
	t.Helper()
	out := []string{}
	seen := map[string]bool{}
	for _, tb := range from.Tables {
		ns := strings.TrimSpace(tb.Namespace)
		if ns == "" || seen[ns] {
			continue
		}
		seen[ns] = true
		if dialect == string(model.KindPostgres) {
			out = append(out, `CREATE SCHEMA IF NOT EXISTS "`+ns+`"`)
		}
	}
	empty := &schema.Schema{Dialect: dialect, Shape: schema.ShapeRelational}
	return append(out, statementList(schema.BuildPlan(dialect, schema.Diff(empty, from)).Up)...)
}

// dryRunLeftovers는 서버에 남은 그림자 DB를 찾는다.
func dryRunLeftovers(t *testing.T, h *harness) []string {
	t.Helper()
	if h.conn.Kind == model.KindSQLite {
		// SQLite의 그림자는 임시 파일이라 서버에 남을 것이 없다.
		return nil
	}
	// 커넥션이 아는 것으로 서버를 빚는다. 목록 조회는 서버 단위라서 필요하다.
	srv := &model.Server{
		ID: h.conn.ServerID, Name: h.conn.ServerName, Kind: h.conn.Kind,
		Host: h.conn.Host, Port: h.conn.Port, Options: h.conn.Options,
		DefaultEnvironment: h.conn.Environment, Enabled: true,
	}
	names, err := dbx.ListDatabases(h.ctx, srv, h.secret)
	if err != nil {
		t.Logf("DB 목록을 읽지 못해 뒷정리 확인을 건너뜁니다: %v", err)
		return nil
	}
	out := []string{}
	for _, n := range names {
		if strings.HasPrefix(n.Name, "dbstudio_dryrun_") {
			out = append(out, n.Name)
		}
	}
	return out
}

// findTableOrNil은 없으면 nil을 준다(findTable은 없으면 시험을 끝낸다).
func findTableOrNil(sc *schema.Schema, name string) *schema.Table {
	for _, t := range sc.Tables {
		if strings.EqualFold(t.Name, name) {
			return t
		}
	}
	return nil
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
				// 트랜잭션이 없으므로 앞 문장은 일단 적용된다. 그 뒤 앱이 되돌려
				// 실행 전 상태로 돌려놓아야 한다 — 사람이 손댈 것이 남으면 안 된다.
				if res.Report.Applied == 0 {
					t.Error("실패 전 문장이 하나도 적용되지 않았습니다 (설정 확인)")
				}
				if res.Undo == nil {
					t.Fatalf("되돌리지 않았습니다 (까닭: %q, 경고: %v)", res.UndoSkipped, res.Warnings)
				}
				if res.Undo.Error != "" {
					t.Fatalf("되돌리기가 실패했습니다: %s", res.Undo.Error)
				}
				if got := h.introspect(t).Fingerprint(); got != beforeFinger {
					t.Errorf("되돌렸는데 구조가 원래와 다릅니다: %v",
						summaries(schema.Diff(h.introspect(t), after)))
				}
				stored, err := h.st.GetMigration(h.ctx, mig.ID, true)
				if err != nil {
					t.Fatalf("reload: %v", err)
				}
				// 되돌렸으므로 "적용된 문장 수"는 0이다. 이 숫자가 남아 있으면
				// 다음 사람이 DB에 무언가 남아 있다고 읽는다.
				if stored.AppliedStatements != 0 {
					t.Errorf("되돌린 뒤 기록된 적용 수 = %d, 기대값 0", stored.AppliedStatements)
				}
				if !containsSubstring(res.Warnings, "되돌렸습니다") {
					t.Errorf("되돌렸다는 안내가 없습니다: %v", res.Warnings)
				}
				// 되돌리기 문장도 기록에 남아야 한다. 무엇을 되돌렸는지 모르면
				// 기록은 "실패했다"까지만 말한다.
				undoSteps := 0
				for _, st := range stored.ExecutionLog {
					if st.Undo {
						undoSteps++
					}
				}
				if undoSteps == 0 {
					t.Errorf("되돌리기 문장이 기록에 없습니다: %+v", stored.ExecutionLog)
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
		// 반려를 되돌릴 수 있어야 한다. 리뷰어가 마음을 바꾸면 상태는 남아 있는
		// 결정에서 다시 계산되므로(핸들러), 승인 수가 이미 찼다면 곧바로 승인됨이다.
		{store.MigrationRejected, store.MigrationApproved, true},
		{store.MigrationRejected, store.MigrationInReview, true},
		// 실행 전이라면 승인도 거둘 수 있다. "이미 승인됐으니 이제 못 막는다"가
		// 되어서는 안 된다.
		{store.MigrationApproved, store.MigrationInReview, true},
		{store.MigrationApproved, store.MigrationRejected, true},
		// 실행된 뒤에는 결정을 바꿀 자리가 아니다.
		{store.MigrationApplied, store.MigrationRejected, false},
		{store.MigrationRolledBack, store.MigrationInReview, false},
		// 롤백된 계획은 다시 실행할 수 있다. 승인 기록은 그대로 남아 있으므로
		// 승인됨으로 곧장 돌아간다 — 되돌리기가 비싸면 아무도 되돌리지 않는다.
		{store.MigrationRolledBack, store.MigrationApproved, true},
		// 되돌린 뒤 "하지 않기로 했다"고 접을 수도 있어야 한다.
		{store.MigrationRolledBack, store.MigrationClosed, true},
		// 적용된 계획도 닫을 수 있다. 목록에 영원히 남으면 "지금 볼 것"과 "끝난 것"이
		// 섞이기 때문이다. 다시 열면 적용됨으로 돌아간다(store가 닫기 전 상태를 본다).
		{store.MigrationApplied, store.MigrationClosed, true},
		{store.MigrationClosed, store.MigrationApplied, true},
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

// 실패했을 때 되돌릴 문장을 고르는 규칙.
//
// 트랜잭션이 없는 DB(MySQL·Oracle)에서 이것이 유일한 안전장치다. 여기서 잘못 고르면
// 되돌린다면서 남의 것을 지우게 되므로, 짐작이 섞이는 모든 경우에 손을 떼야 한다.
func TestUndoPlanPicksAppliedInverses(t *testing.T) {
	// 변경 3개짜리 계획. down은 up의 역순이다(BuildPlan이 그렇게 만든다).
	plan := &schema.Plan{
		Up: []schema.Statement{
			{SQL: "CREATE TABLE a", Seq: 1},
			{SQL: "CREATE TABLE b", Seq: 2},
			{SQL: "CREATE TABLE c", Seq: 3},
		},
		Down: []schema.Statement{
			{SQL: "DROP TABLE c", Seq: 3},
			{SQL: "DROP TABLE b", Seq: 2},
			{SQL: "DROP TABLE a", Seq: 1},
		},
	}
	// 1·2번은 성공, 3번에서 실패했다.
	steps := []dbx.ExecStep{
		{Index: 0, SQL: "CREATE TABLE a"},
		{Index: 1, SQL: "CREATE TABLE b"},
		{Index: 2, SQL: "CREATE TABLE c", Error: "boom"},
	}
	got, skip := undoPlan(plan, steps, 2)
	if skip != "" {
		t.Fatalf("되돌리기를 건너뛰었습니다: %s", skip)
	}
	// 되돌리기끼리도 순서가 있다. 나중에 만든 것을 먼저 지운다.
	want := []string{"DROP TABLE b", "DROP TABLE a"}
	if len(got) != len(want) {
		t.Fatalf("되돌릴 문장 = %v, 기대 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%d번째 = %q, 기대 %q", i, got[i], want[i])
		}
	}
}

// 한 변경이 문장 여러 개를 낳아도 짝이 맞아야 한다(SQLite의 테이블 재작성 같은 것).
func TestUndoPlanHandlesMultiStatementChange(t *testing.T) {
	plan := &schema.Plan{
		Up: []schema.Statement{
			{SQL: "CREATE TABLE a_new", Seq: 1},
			{SQL: "INSERT INTO a_new SELECT * FROM a", Seq: 1},
			{SQL: "CREATE TABLE b", Seq: 2},
		},
		Down: []schema.Statement{
			{SQL: "DROP TABLE b", Seq: 2},
			{SQL: "DROP TABLE a_new", Seq: 1},
		},
	}
	steps := []dbx.ExecStep{
		{Index: 0}, {Index: 1}, {Index: 2, Error: "boom"},
	}
	got, skip := undoPlan(plan, steps, 2)
	if skip != "" {
		t.Fatalf("건너뜀: %s", skip)
	}
	if len(got) != 1 || got[0] != "DROP TABLE a_new" {
		t.Errorf("되돌릴 문장 = %v, 기대 [DROP TABLE a_new]", got)
	}
}

// 이미 적용된 문장이 파괴적이면 손대지 않는다.
//
// 구조는 되살릴 수 있어도 데이터는 아니다. 자동으로 되돌리면 "복구됐다"고 보이는 빈
// 컬럼이 남는데, 그것은 사람이 알고 골라야 하는 일이다.
func TestUndoPlanRefusesAfterDestructive(t *testing.T) {
	plan := &schema.Plan{
		Up: []schema.Statement{
			{SQL: "ALTER TABLE users DROP COLUMN memo", Seq: 1, Destructive: true,
				Table: "users", Object: "memo"},
			{SQL: "CREATE TABLE b", Seq: 2},
		},
		Down: []schema.Statement{
			{SQL: "DROP TABLE b", Seq: 2},
			{SQL: "ALTER TABLE users ADD COLUMN memo TEXT", Seq: 1},
		},
	}
	steps := []dbx.ExecStep{{Index: 0}, {Index: 1, Error: "boom"}}
	got, skip := undoPlan(plan, steps, 1)
	if got != nil {
		t.Errorf("파괴적 변경을 자동으로 되돌렸습니다: %v", got)
	}
	if !strings.Contains(skip, "users.memo") {
		t.Errorf("까닭에 무엇이 남았는지가 없습니다: %q", skip)
	}
}

// 되돌릴 SQL이 없는 변경이 섞여 있으면 손대지 않는다.
// 반쪽만 되돌리면 상태가 더 헝클어진다.
func TestUndoPlanRefusesWhenInverseMissing(t *testing.T) {
	plan := &schema.Plan{
		Up: []schema.Statement{
			{SQL: "CREATE TABLE a", Seq: 1},
			{SQL: "CREATE VIEW v AS SELECT 1", Seq: 2},
			{SQL: "CREATE TABLE c", Seq: 3},
		},
		// 2번 변경의 되돌리기가 없다.
		Down: []schema.Statement{
			{SQL: "DROP TABLE c", Seq: 3},
			{SQL: "DROP TABLE a", Seq: 1},
		},
	}
	steps := []dbx.ExecStep{{Index: 0}, {Index: 1}, {Index: 2, Error: "boom"}}
	if got, skip := undoPlan(plan, steps, 2); got != nil || skip == "" {
		t.Errorf("되돌릴 SQL이 없는데 되돌렸습니다: %v (%q)", got, skip)
	}
}

// Seq가 없는 예전 계획은 짝을 지을 수 없으므로 손대지 않는다.
func TestUndoPlanRefusesLegacyPlan(t *testing.T) {
	plan := &schema.Plan{
		Up:   []schema.Statement{{SQL: "CREATE TABLE a"}, {SQL: "CREATE TABLE b"}},
		Down: []schema.Statement{{SQL: "DROP TABLE b"}, {SQL: "DROP TABLE a"}},
	}
	steps := []dbx.ExecStep{{Index: 0}, {Index: 1, Error: "boom"}}
	if got, skip := undoPlan(plan, steps, 1); got != nil || skip == "" {
		t.Errorf("짝지을 수 없는 계획을 되돌렸습니다: %v (%q)", got, skip)
	}
}

// BuildPlan은 up과 down에 같은 번호를 찍어야 한다. 이 끈이 끊기면 위의 모든 규칙이
// 조용히 "되돌릴 수 없음"으로 떨어진다.
func TestBuildPlanPairsUpAndDownBySeq(t *testing.T) {
	before := &schema.Schema{Dialect: "mysql", Shape: schema.ShapeRelational, Tables: []*schema.Table{{
		Name:    "keep",
		Columns: []*schema.Column{{Name: "id", Type: schema.LogicalType{Base: schema.TypeInt}}},
	}}}
	after := &schema.Schema{Dialect: "mysql", Shape: schema.ShapeRelational, Tables: []*schema.Table{
		before.Tables[0],
		{
			Name: "added",
			Columns: []*schema.Column{
				{Name: "id", Type: schema.LogicalType{Base: schema.TypeInt}},
			},
		},
	}}
	plan := schema.BuildPlan("mysql", schema.Diff(before, after))
	if len(plan.Up) == 0 {
		t.Fatalf("계획이 비었습니다")
	}
	seqs := map[int]bool{}
	for _, up := range plan.Up {
		if up.Seq == 0 {
			t.Fatalf("up 문장에 번호가 없습니다: %q", up.SQL)
		}
		seqs[up.Seq] = true
	}
	for _, down := range plan.Down {
		if down.Seq == 0 {
			t.Fatalf("down 문장에 번호가 없습니다: %q", down.SQL)
		}
		if !seqs[down.Seq] {
			t.Errorf("down 문장의 번호 %d 가 up 에 없습니다: %q", down.Seq, down.SQL)
		}
	}
}

// 승인은 어디서나 1명이면 된다.
//
// 두 번째 승인자를 구하지 못해 계획이 멈추면 사람들은 이 흐름을 우회한다. 그러면
// 검토는 한 명도 거치지 않은 것이 된다 — 지키지 못할 규칙은 규칙을 통째로 잃게
// 만든다. 파괴적 변경은 승인 수 대신 경고와 사전 검사로 막는다.
func TestRequiredApprovalsRules(t *testing.T) {
	dev := &model.Connection{Environment: model.EnvDev}
	prod := &model.Connection{Environment: model.EnvProd}

	for _, tc := range []struct {
		name string
		conn *model.Connection
		dest int
	}{
		{"개발 + 비파괴", dev, 0},
		{"개발 + 파괴적", dev, 2},
		{"운영", prod, 0},
		{"운영 + 파괴적", prod, 5},
		{"커넥션 없음", nil, 0},
	} {
		if got := RequiredApprovals(tc.conn, tc.dest); got != 1 {
			t.Errorf("%s = %d, 기대값 1", tc.name, got)
		}
	}
}
