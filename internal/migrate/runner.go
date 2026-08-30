// Package migrate는 마이그레이션의 사전 검사와 실행을 담당한다.
//
// 이 패키지의 존재 이유는 "안전하게 실행하는 것"이다. DDL 생성은 schema 패키지가,
// 저장은 store가 이미 한다. 여기서 다루는 것은 그 사이의 위험한 순간이다:
//
//   - 계획을 세운 뒤 대상 DB가 바뀌었는가 (프리체크)
//   - 실패하면 어디까지 적용됐는가 (DB 종류별 트랜잭션 차이)
//   - 실행 결과가 의도와 같은가 (사후 검증)
//   - 운영 DB에 손실이 생기는 변경을 막을 장치가 있는가 (승인·백업 훅)
package migrate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"time"

	"dbstudio/internal/dbx"
	"dbstudio/internal/model"
	"dbstudio/internal/schema"
	"dbstudio/internal/store"
)

// 실행 타임아웃. 큰 테이블의 ALTER는 오래 걸릴 수 있어 넉넉하게 둔다.
const (
	introspectTimeout = 120 * time.Second
	statementTimeout  = 30 * time.Minute
	backupTimeout     = 30 * time.Minute
)

// Runner는 마이그레이션 실행기다.
type Runner struct {
	st  *store.Store
	log *slog.Logger
	// backupCmd는 운영 DB 마이그레이션 전에 실행할 외부 명령이다.
	//
	// 앱이 DB별 백업을 직접 구현하지 않는 이유: 도구와 정책이 조직마다 다르고
	// (pg_dump, mysqldump, 스냅샷, 볼륨 백업) 잘못 만든 백업은 없는 것보다 위험하다.
	// 명령은 운영자가 실행 플래그로만 지정할 수 있다 — API 사용자가 지정하게 하면
	// 앱이 임의 명령 실행 통로가 된다.
	backupCmd string
}

func New(st *store.Store, backupCmd string, log *slog.Logger) *Runner {
	if log == nil {
		log = slog.Default()
	}
	return &Runner{st: st, log: log, backupCmd: strings.TrimSpace(backupCmd)}
}

// BackupConfigured는 백업 훅이 설정되어 있는지 알려준다. UI가 상태를 표시한다.
func (r *Runner) BackupConfigured() bool { return r.backupCmd != "" }

// Precheck은 실행 전 안전 조건을 확인한다.
type Precheck struct {
	// OK가 false면 실행을 막아야 한다.
	OK bool `json:"ok"`
	// Blockers는 실행을 막는 사유다.
	Blockers []string `json:"blockers"`
	// Warnings는 막지는 않지만 사용자가 알아야 하는 것이다.
	Warnings []string `json:"warnings"`

	// ExpectedFingerprint는 계획이 전제한 대상 DB 구조다.
	ExpectedFingerprint string `json:"expectedFingerprint"`
	// ActualFingerprint는 지금 대상 DB의 구조다.
	ActualFingerprint string `json:"actualFingerprint"`
	// Drifted가 true면 계획을 세운 뒤 대상 DB가 바뀌었다.
	Drifted bool `json:"drifted"`
	// DriftChanges는 무엇이 달라졌는지다.
	DriftChanges []string `json:"driftChanges,omitempty"`

	// TransactionalDDL이 false면 실패 시 부분 적용이 남을 수 있다.
	TransactionalDDL bool `json:"transactionalDdl"`
	// RequiredApprovals / Approvals는 승인 요건과 현재 승인 수다.
	RequiredApprovals int `json:"requiredApprovals"`
	Approvals         int `json:"approvals"`
	// BackupConfigured는 백업 훅 설정 여부다.
	BackupConfigured bool `json:"backupConfigured"`

	// Current는 프리체크에서 읽은 현재 스키마다. 실행 경로가 재사용한다.
	Current *schema.Schema `json:"-"`
}

// RequiredApprovals는 이 커넥션에 필요한 승인 수를 정한다. 언제나 1명이다.
//
// 예전에는 운영 DB와 파괴적 변경에 2명을 요구했다. 두 사람이 보는 것이 더 안전한
// 것은 맞지만, 두 번째 승인자를 구하지 못해 계획이 며칠씩 멈추면 사람들은 이
// 흐름을 우회하는 다른 길(콘솔에서 직접 실행)을 찾는다. 그러면 검토는 한 명도
// 거치지 않은 것이 된다 — 지키지 못할 규칙은 규칙을 통째로 잃게 만든다.
//
// 한 명이 남기는 승인은 여전히 "담당자가 아닌 다른 사람이 봤다"는 뜻이다. 담당자는
// 리뷰어가 될 수 없고(handleSetMigrationAssignment) 자기 계획을 승인할 수도 없기
// 때문이다. 파괴적 변경은 승인 수 대신 경고와 사전 검사로 막는다.
//
// 인자를 남겨 두는 이유: 커넥션·파괴적 변경 수에 따라 다시 갈라야 할 때 부르는 쪽을
// 고치지 않아도 되게 하기 위해서다. 규칙은 여기 한 곳에만 있다.
func RequiredApprovals(conn *model.Connection, destructive int) int {
	return 1
}

// Check은 마이그레이션 실행 전 검사를 수행한다.
func (r *Runner) Check(ctx context.Context, conn *model.Connection, secret *model.Secret,
	mig *store.Migration) (*Precheck, error) {

	adapter, err := dbx.Get(conn.Kind)
	if err != nil {
		return nil, err
	}
	if !adapter.Capabilities().Migrate {
		return nil, fmt.Errorf("%s는 마이그레이션을 지원하지 않습니다", conn.Kind)
	}

	pc := &Precheck{
		Blockers: []string{}, Warnings: []string{},
		ExpectedFingerprint: mig.BaseFinger,
		TransactionalDDL:    dbx.TransactionalDDL(string(conn.Kind)),
		Approvals:           store.ApprovalCount(mig.Reviews),
		RequiredApprovals:   RequiredApprovals(conn, mig.DestructiveCount),
		BackupConfigured:    r.BackupConfigured(),
	}

	ictx, cancel := context.WithTimeout(ctx, introspectTimeout)
	defer cancel()
	current, err := adapter.Introspect(ictx, dbx.Target{Conn: conn, Secret: secret})
	if err != nil {
		pc.Blockers = append(pc.Blockers, "대상 DB의 현재 스키마를 읽지 못했습니다: "+err.Error())
		return pc, nil
	}
	pc.Current = current
	pc.ActualFingerprint = current.Fingerprint()

	// 프리체크의 핵심: 계획이 전제한 상태와 실제가 같은가.
	// 다르면 그 사이 누군가 스키마를 바꾼 것이고, 오래된 계획을 실행하면
	// 그 변경을 덮어쓰거나 예측하지 못한 결과가 난다.
	if pc.ExpectedFingerprint != "" && pc.ActualFingerprint != pc.ExpectedFingerprint {
		pc.Drifted = true
		pc.Blockers = append(pc.Blockers,
			"계획을 만든 뒤 대상 DB의 스키마가 바뀌었습니다. 계획을 다시 만들어야 합니다")
		if mig.TargetSchema != nil {
			// 무엇이 달라졌는지 보여준다. "바뀌었다"만으로는 무엇을 해야 할지 알 수 없다.
			diff := schema.Diff(current, mig.TargetSchema)
			for _, c := range diff.Changes {
				pc.DriftChanges = append(pc.DriftChanges, c.Summary)
				if len(pc.DriftChanges) >= 20 {
					break
				}
			}
		}
	}

	if mig.Status != store.MigrationApproved {
		pc.Blockers = append(pc.Blockers,
			fmt.Sprintf("승인된 마이그레이션만 실행할 수 있습니다 (현재 상태: %s)", mig.Status))
	}
	if store.HasRejection(mig.Reviews) {
		pc.Blockers = append(pc.Blockers, "반려 의견이 남아 있습니다")
	}
	if pc.Approvals < pc.RequiredApprovals {
		pc.Blockers = append(pc.Blockers,
			fmt.Sprintf("승인 %d명이 필요합니다 (현재 %d명)", pc.RequiredApprovals, pc.Approvals))
	}
	if len(mig.Plan.Up) == 0 {
		pc.Blockers = append(pc.Blockers, "실행할 SQL이 없습니다")
	}

	if !pc.TransactionalDDL {
		pc.Warnings = append(pc.Warnings,
			fmt.Sprintf("%s는 DDL을 트랜잭션으로 되돌릴 수 없습니다. "+
				"중간에 실패하면 앞의 문장은 적용된 상태로 남습니다", conn.Kind))
	}
	if conn.Environment == model.EnvProd && !r.BackupConfigured() {
		pc.Warnings = append(pc.Warnings,
			"운영 DB인데 백업 훅(-backup-cmd)이 설정되지 않았습니다")
	}
	if mig.DestructiveCount > 0 {
		pc.Warnings = append(pc.Warnings,
			fmt.Sprintf("데이터 손실이 발생할 수 있는 변경 %d건이 포함되어 있습니다", mig.DestructiveCount))
	}
	if len(mig.Irreversible) > 0 {
		pc.Warnings = append(pc.Warnings,
			fmt.Sprintf("되돌릴 수 없는 변경 %d건: 롤백해도 데이터는 복구되지 않습니다",
				len(mig.Irreversible)))
	}
	for _, w := range mig.Plan.Warnings {
		pc.Warnings = append(pc.Warnings, w)
	}

	pc.OK = len(pc.Blockers) == 0
	return pc, nil
}

// ApplyParams는 실행 입력이다.
type ApplyParams struct {
	Conn   *model.Connection
	Secret *model.Secret
	Mig    *store.Migration
	Actor  *model.User
	// SkipBackup은 백업 훅을 건너뛴다. 운영 DB에서 명시적으로 요청한 경우만 쓴다.
	SkipBackup bool
}

// Result는 실행 결과다.
type Result struct {
	Status  string               `json:"status"`
	Report  *dbx.ExecReport      `json:"report"`
	Version *store.SchemaVersion `json:"version,omitempty"`
	// PostDiff는 실행 후 실제 스키마와 목표의 차이다. 비어 있어야 정상이다.
	PostDiff []string `json:"postDiff,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
	Error    string   `json:"error,omitempty"`
	// BackupOutput은 백업 훅의 출력이다 (성공 시에도 남긴다).
	BackupOutput string `json:"backupOutput,omitempty"`
}

// Apply는 마이그레이션을 실행하고 결과를 기록한다.
func (r *Runner) Apply(ctx context.Context, p ApplyParams) (*Result, error) {
	pc, err := r.Check(ctx, p.Conn, p.Secret, p.Mig)
	if err != nil {
		return nil, err
	}
	if !pc.OK {
		return nil, &BlockedError{Blockers: pc.Blockers}
	}

	res := &Result{Warnings: append([]string{}, pc.Warnings...)}

	// 백업 훅은 운영 DB에서만 강제한다. 개발 DB에서 매번 백업을 요구하면
	// 실질적으로 아무도 마이그레이션을 쓰지 않게 된다.
	if p.Conn.Environment == model.EnvProd && r.BackupConfigured() && !p.SkipBackup {
		out, berr := r.runBackup(ctx, p.Conn, p.Mig)
		res.BackupOutput = out
		if berr != nil {
			// 백업 실패는 곧 중단이다. 백업 없이 운영 DB를 바꾸는 것은
			// 이 워크플로가 막으려는 바로 그 상황이다.
			_ = r.st.RecordMigrationRun(ctx, p.Mig.ID, store.RunResult{
				Status: store.MigrationFailed, Steps: nil, Applied: 0,
				Error: "백업 실패로 중단: " + berr.Error(), ActorID: actorID(p.Actor),
			})
			return nil, fmt.Errorf("백업이 실패해 마이그레이션을 중단했습니다: %w", berr)
		}
	}

	adapter, err := dbx.Get(p.Conn.Kind)
	if err != nil {
		return nil, err
	}
	stmts := statementList(p.Mig.Plan.Up)

	report, err := adapter.ExecDDL(ctx, dbx.Target{Conn: p.Conn, Secret: p.Secret}, stmts,
		dbx.ExecOptions{StatementTimeout: statementTimeout})
	if err != nil {
		// 실행을 시작하지 못했다 (접속 실패 등). 적용된 것이 없으므로 상태만 남긴다.
		_ = r.st.RecordMigrationRun(ctx, p.Mig.ID, store.RunResult{
			Status: store.MigrationFailed, Applied: 0,
			Error: err.Error(), ActorID: actorID(p.Actor),
		})
		return nil, err
	}
	res.Report = report

	steps := toStoreSteps(report.Steps)
	if report.Error != "" {
		res.Status = store.MigrationFailed
		res.Error = report.Error
		if !report.TransactionUsed && report.Applied > 0 {
			res.Warnings = append(res.Warnings, fmt.Sprintf(
				"%d번째 문장에서 실패했고 앞의 %d개 문장은 적용된 상태로 남아 있습니다. "+
					"실행 기록을 확인한 뒤 롤백하거나 직접 정리해야 합니다",
				report.FailedIndex+1, report.Applied))
		}
		if err := r.st.RecordMigrationRun(ctx, p.Mig.ID, store.RunResult{
			Status: store.MigrationFailed, Steps: steps, Applied: report.Applied,
			Error: report.Error, ActorID: actorID(p.Actor),
		}); err != nil {
			return nil, err
		}
		return res, nil
	}

	// 사후 검증: 실제로 읽은 스키마를 목표와 비교한다.
	// 여기서 버전으로 등록하는 것은 "관측된 실제 스키마"다 — 의도(목표)를 등록하면
	// 생성기의 미묘한 차이가 이력에 영원히 숨는다.
	ictx, cancel := context.WithTimeout(ctx, introspectTimeout)
	defer cancel()
	actual, ierr := adapter.Introspect(ictx, dbx.Target{Conn: p.Conn, Secret: p.Secret})
	if ierr != nil {
		res.Warnings = append(res.Warnings,
			"적용은 성공했지만 결과 스키마를 다시 읽지 못했습니다: "+ierr.Error())
		actual = p.Mig.TargetSchema
	} else if p.Mig.TargetSchema != nil {
		post := schema.Diff(actual, p.Mig.TargetSchema)
		for _, c := range post.Changes {
			res.PostDiff = append(res.PostDiff, c.Summary)
		}
		if len(res.PostDiff) > 0 {
			// 계획 단계에서 이미 "이 dialect로는 표현할 수 없다"고 경고한 항목이 있으면
			// 남은 차이는 대개 그것이다. 같은 사실을 두 번 다르게 설명하면 사용자는
			// 새로운 문제가 생겼다고 오해한다.
			if len(p.Mig.Plan.Warnings) > 0 {
				res.Warnings = append(res.Warnings, fmt.Sprintf(
					"적용 후에도 목표와 %d건 다릅니다. 계획 단계의 경고(%s)에 해당하는 "+
						"항목일 수 있습니다 — 이 DB 종류가 표현할 수 없는 변경입니다",
					len(res.PostDiff), strings.Join(p.Mig.Plan.Warnings, " / ")))
			} else {
				res.Warnings = append(res.Warnings, fmt.Sprintf(
					"적용 후 스키마가 목표와 %d건 다릅니다. 생성된 DDL이 의도를 완전히 재현하지 "+
						"못했을 수 있습니다", len(res.PostDiff)))
			}
		}
	}

	// 버전으로 되돌리는 마이그레이션이면 그 사실을 이력에 남긴다.
	// 같은 실행 경로를 쓰지만 이력에서는 구분되어야 한다 — "왜 구조가 예전으로
	// 돌아갔는가"에 답하려면 그 지점이 보여야 한다.
	source := store.VersionSourceMigrated
	if p.Mig.RollbackTo != nil {
		source = store.VersionSourceRollback
	}
	summary := changeSummaries(p.Mig.Diff)
	version, _, verr := r.st.SaveSchemaVersion(ctx, store.SaveVersionParams{
		ConnectionID: p.Conn.ID, Schema: actual,
		Source: source,
		Note:   p.Mig.Title, ChangeSummary: summary,
		AuthorID: actorID(p.Actor), AuthorName: actorName(p.Actor),
	})
	if verr != nil {
		return nil, verr
	}
	res.Version = version
	res.Status = store.MigrationApplied

	var toVersion *int64
	if version != nil {
		toVersion = &version.ID
	}
	if err := r.st.RecordMigrationRun(ctx, p.Mig.ID, store.RunResult{
		Status: store.MigrationApplied, Steps: steps, Applied: report.Applied,
		ToVersion: toVersion, ActorID: actorID(p.Actor),
	}); err != nil {
		return nil, err
	}
	return res, nil
}

// Rollback은 down SQL을 실행해 이전 상태로 되돌린다.
//
// continueOnError는 부분 적용된(실패한) 마이그레이션을 정리할 때만 켠다.
// 그때는 "만들어지지 않은 것을 지우려는" 문장이 섞여 있어 첫 실패에서 멈추면
// 나머지를 정리할 수 없다.
func (r *Runner) Rollback(ctx context.Context, p ApplyParams, continueOnError bool) (*Result, error) {
	if p.Mig.Status != store.MigrationApplied && p.Mig.Status != store.MigrationFailed {
		return nil, &BlockedError{Blockers: []string{
			fmt.Sprintf("적용됨 또는 실패 상태만 롤백할 수 있습니다 (현재: %s)", p.Mig.Status),
		}}
	}
	if len(p.Mig.Plan.Down) == 0 {
		return nil, &BlockedError{Blockers: []string{
			"이 마이그레이션에는 롤백 SQL이 없습니다. 직접 되돌려야 합니다",
		}}
	}

	adapter, err := dbx.Get(p.Conn.Kind)
	if err != nil {
		return nil, err
	}
	res := &Result{}
	if len(p.Mig.Irreversible) > 0 {
		res.Warnings = append(res.Warnings,
			"되돌릴 수 없는 변경이 포함되어 있어 구조는 복구되지만 데이터는 복구되지 않습니다")
	}

	stmts := statementList(p.Mig.Plan.Down)
	report, err := adapter.ExecDDL(ctx, dbx.Target{Conn: p.Conn, Secret: p.Secret}, stmts,
		dbx.ExecOptions{StatementTimeout: statementTimeout, ContinueOnError: continueOnError})
	if err != nil {
		return nil, err
	}
	res.Report = report
	steps := toStoreSteps(report.Steps)

	if report.Error != "" && !continueOnError {
		res.Status = store.MigrationFailed
		res.Error = report.Error
		if err := r.st.RecordMigrationRun(ctx, p.Mig.ID, store.RunResult{
			Status: store.MigrationFailed, Steps: steps, Applied: report.Applied,
			Error: "롤백 실패: " + report.Error, ActorID: actorID(p.Actor),
		}); err != nil {
			return nil, err
		}
		return res, nil
	}
	if report.Error != "" {
		res.Warnings = append(res.Warnings, fmt.Sprintf(
			"오류를 무시하고 계속 실행했습니다. 실패한 문장이 있으므로 결과를 확인하세요 (%s)",
			report.Error))
	}

	// 롤백 후의 실제 스키마를 새 버전으로 등록한다. 되돌린 것도 이력이다 —
	// 버전을 지우면 "언제 무엇이 되돌아갔는가"를 알 수 없다.
	ictx, cancel := context.WithTimeout(ctx, introspectTimeout)
	defer cancel()
	actual, ierr := adapter.Introspect(ictx, dbx.Target{Conn: p.Conn, Secret: p.Secret})
	if ierr != nil {
		res.Warnings = append(res.Warnings, "롤백 후 스키마를 다시 읽지 못했습니다: "+ierr.Error())
	} else {
		version, _, verr := r.st.SaveSchemaVersion(ctx, store.SaveVersionParams{
			ConnectionID: p.Conn.ID, Schema: actual,
			Source:   store.VersionSourceRollback,
			Note:     "롤백: " + p.Mig.Title,
			AuthorID: actorID(p.Actor), AuthorName: actorName(p.Actor),
		})
		if verr != nil {
			return nil, verr
		}
		res.Version = version
	}

	res.Status = store.MigrationRolledBack
	if err := r.st.RecordMigrationRun(ctx, p.Mig.ID, store.RunResult{
		Status: store.MigrationRolledBack, Steps: steps, Applied: report.Applied,
		ActorID: actorID(p.Actor), RolledBack: true,
	}); err != nil {
		return nil, err
	}
	return res, nil
}

// runBackup은 백업 훅을 실행한다.
//
// 명령은 셸을 거치지 않고 실행한다. 셸을 쓰면 커넥션 이름 같은 값이 명령으로
// 해석될 수 있고, 그 값은 사용자가 정한다.
func (r *Runner) runBackup(ctx context.Context, conn *model.Connection, mig *store.Migration) (string, error) {
	parts := strings.Fields(r.backupCmd)
	if len(parts) == 0 {
		return "", errors.New("백업 명령이 비어 있습니다")
	}
	// 인자에 값을 치환한다. 자격증명은 넘기지 않는다 — 백업 도구의 인증은
	// 운영자가 환경변수나 설정 파일로 준비하는 것이 맞다.
	repl := map[string]string{
		"{name}":     conn.Name,
		"{kind}":     string(conn.Kind),
		"{host}":     conn.Host,
		"{port}":     fmt.Sprintf("%d", conn.Port),
		"{database}": conn.DatabaseName,
		"{env}":      string(conn.Environment),
		"{id}":       mig.ID,
	}
	args := make([]string, 0, len(parts)-1)
	for _, a := range parts[1:] {
		for k, v := range repl {
			a = strings.ReplaceAll(a, k, v)
		}
		args = append(args, a)
	}

	bctx, cancel := context.WithTimeout(ctx, backupTimeout)
	defer cancel()
	cmd := exec.CommandContext(bctx, parts[0], args...)
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if len(text) > 4000 {
		text = text[:4000] + "…"
	}
	if err != nil {
		r.log.Error("백업 훅 실패", "conn", conn.Name, "error", err, "output", text)
		if text != "" {
			return text, fmt.Errorf("%w: %s", err, text)
		}
		return text, err
	}
	r.log.Info("백업 훅 성공", "conn", conn.Name, "migration", mig.ID)
	return text, nil
}

// BlockedError는 안전 조건 때문에 실행을 막았음을 뜻한다.
type BlockedError struct {
	Blockers []string
}

func (e *BlockedError) Error() string {
	return "실행 조건을 만족하지 않습니다: " + strings.Join(e.Blockers, "; ")
}

func statementList(stmts []schema.Statement) []string {
	out := make([]string, 0, len(stmts))
	for _, s := range stmts {
		out = append(out, s.SQL)
	}
	return out
}

func toStoreSteps(steps []dbx.ExecStep) []store.ExecutionStep {
	out := make([]store.ExecutionStep, 0, len(steps))
	for _, s := range steps {
		out = append(out, store.ExecutionStep{
			Index: s.Index, SQL: s.SQL, DurationMs: s.DurationMs,
			RowsAffected: s.RowsAffected, Error: s.Error,
		})
	}
	return out
}

func changeSummaries(diff *schema.DiffResult) []string {
	if diff == nil {
		return []string{}
	}
	out := make([]string, 0, len(diff.Changes))
	for _, c := range diff.Changes {
		out = append(out, c.Summary)
	}
	return out
}

func actorID(u *model.User) string {
	if u == nil {
		return ""
	}
	return u.ID
}

func actorName(u *model.User) string {
	if u == nil {
		return ""
	}
	if strings.TrimSpace(u.DisplayName) != "" {
		return u.DisplayName
	}
	return u.Username
}
