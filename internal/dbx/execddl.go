package dbx

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// ExecOptions는 DDL 실행 방식을 정한다.
type ExecOptions struct {
	// ContinueOnError는 실패한 문장을 건너뛰고 계속 실행한다.
	//
	// 부분 적용된 마이그레이션을 되돌릴 때만 쓴다: 그때는 "이미 없는 것을 지우려는"
	// 문장이 섞여 있어 첫 실패에서 멈추면 나머지를 정리할 수 없다.
	// 정상적인 적용 경로에서 켜면 안 된다 — 앞 문장이 실패한 채로 뒤 문장을
	// 실행하면 스키마가 어느 상태인지 알 수 없게 된다.
	ContinueOnError bool
	// StatementTimeout은 문장 하나의 상한이다. 0이면 상한 없음(컨텍스트만 적용).
	// 큰 테이블의 ALTER는 몇 분이 걸릴 수 있으므로 넉넉해야 한다.
	StatementTimeout time.Duration
	// ForceNoTransaction은 트랜잭션을 쓸 수 있어도 쓰지 않는다.
	// 트랜잭션 안에서 실행할 수 없는 문장(예: PostgreSQL의 CREATE INDEX CONCURRENTLY)이
	// 섞인 계획을 위한 탈출구다.
	ForceNoTransaction bool
}

// ExecStep은 문장 하나의 실행 결과다.
type ExecStep struct {
	Index        int    `json:"index"`
	SQL          string `json:"sql"`
	DurationMs   int64  `json:"durationMs"`
	RowsAffected int64  `json:"rowsAffected,omitempty"`
	Error        string `json:"error,omitempty"`
}

// ExecReport는 DDL 실행 전체 결과다.
type ExecReport struct {
	Steps []ExecStep `json:"steps"`
	// Applied는 성공한 문장 수다.
	Applied int `json:"applied"`
	// TransactionUsed가 true면 실패 시 전부 되돌아갔다(부분 적용 없음).
	TransactionUsed bool `json:"transactionUsed"`
	// RolledBack은 트랜잭션을 되돌렸는지 여부다.
	RolledBack bool `json:"rolledBack"`
	// FailedIndex는 실패한 문장의 위치다. 실패가 없으면 -1이다.
	FailedIndex int `json:"failedIndex"`
	// Error는 첫 실패 사유다.
	Error string `json:"error,omitempty"`
}

// TransactionalDDL은 이 종류의 DB가 DDL을 트랜잭션으로 되돌릴 수 있는지 알려준다.
//
// 이 차이가 마이그레이션 안전성의 핵심이다:
//   - PostgreSQL / MS-SQL / SQLite — DDL이 트랜잭션에 참여한다. 실패하면 전부 되돌아가
//     "절반만 적용된 스키마"가 생기지 않는다.
//   - MySQL / Oracle — DDL이 암묵적 커밋을 일으킨다. 트랜잭션으로 감싸도 의미가 없고,
//     중간에 실패하면 앞의 문장은 그대로 남는다. 그래서 어디까지 적용됐는지를
//     문장 단위로 기록하는 것이 유일한 복구 근거다.
func TransactionalDDL(kind string) bool {
	switch kind {
	case "postgres", "mssql", "sqlite":
		return true
	}
	return false
}

// ExecDDL은 DDL 문장들을 순서대로 실행한다.
func (a *sqlAdapter) ExecDDL(ctx context.Context, t Target, stmts []string, opts ExecOptions) (*ExecReport, error) {
	if !a.caps.Migrate {
		return nil, fmt.Errorf("%w: %s DDL 실행", ErrNotImplemented, a.kind)
	}
	// 동시 실행은 하지 않는다. DDL은 순서가 의미를 가지므로 커넥션 하나로 직렬 실행한다.
	db, err := a.open(t, 1)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	if err := db.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("접속 실패: %w", err)
	}

	report := &ExecReport{Steps: []ExecStep{}, FailedIndex: -1}
	useTx := TransactionalDDL(string(a.kind)) && !opts.ContinueOnError && !opts.ForceNoTransaction
	report.TransactionUsed = useTx

	// executor는 트랜잭션 또는 DB 핸들 중 하나다.
	type execer interface {
		ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	}
	var runner execer = db
	var tx *sql.Tx
	if useTx {
		tx, err = db.BeginTx(ctx, nil)
		if err != nil {
			return nil, fmt.Errorf("트랜잭션 시작 실패: %w", err)
		}
		runner = tx
		defer func() {
			if tx != nil {
				_ = tx.Rollback()
			}
		}()
	}

	for i, raw := range stmts {
		stmt := strings.TrimSpace(raw)
		if stmt == "" {
			continue
		}
		stepCtx := ctx
		var cancel context.CancelFunc
		if opts.StatementTimeout > 0 {
			stepCtx, cancel = context.WithTimeout(ctx, opts.StatementTimeout)
		}
		start := time.Now()
		res, execErr := runner.ExecContext(stepCtx, stmt)
		elapsed := time.Since(start)
		if cancel != nil {
			cancel()
		}

		step := ExecStep{Index: i, SQL: stmt, DurationMs: elapsed.Milliseconds()}
		if execErr != nil {
			step.Error = execErr.Error()
			report.Steps = append(report.Steps, step)
			if report.FailedIndex < 0 {
				report.FailedIndex = i
				report.Error = execErr.Error()
			}
			if opts.ContinueOnError {
				continue
			}
			if useTx {
				_ = tx.Rollback()
				tx = nil
				report.RolledBack = true
				report.Applied = 0
			}
			return report, nil
		}
		if res != nil {
			if n, aerr := res.RowsAffected(); aerr == nil {
				step.RowsAffected = n
			}
		}
		report.Steps = append(report.Steps, step)
		report.Applied++
	}

	if useTx && tx != nil {
		if err := tx.Commit(); err != nil {
			tx = nil
			report.RolledBack = true
			report.Applied = 0
			report.Error = fmt.Sprintf("커밋 실패: %v", err)
			report.FailedIndex = len(stmts)
			return report, nil
		}
		tx = nil
	}
	return report, nil
}

// ExecDDL은 문서/키-값 DB에서는 지원하지 않는다.
func (a *mongoAdapter) ExecDDL(context.Context, Target, []string, ExecOptions) (*ExecReport, error) {
	return nil, fmt.Errorf("%w: MongoDB는 DDL 마이그레이션 대상이 아닙니다", ErrNotImplemented)
}

func (a *redisAdapter) ExecDDL(context.Context, Target, []string, ExecOptions) (*ExecReport, error) {
	return nil, fmt.Errorf("%w: Redis는 DDL 마이그레이션 대상이 아닙니다", ErrNotImplemented)
}
