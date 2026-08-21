// Package store는 메타 데이터베이스(SQLite) 접근과 스키마 마이그레이션을 담당한다.
package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"math"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite 드라이버 (CGO 불필요)

	"dbstudio/internal/crypto"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// Store는 메타 DB 핸들과 시크릿 봉인기를 함께 들고 있는다.
// 저장 계층 전체가 이 타입의 메서드로 노출된다.
type Store struct {
	db     *sql.DB
	secret *crypto.SecretBox

	// 전역 보안 정책 캐시. 인증 미들웨어가 매 요청마다 읽으므로 DB까지 가지 않는다.
	// nil이면 아직 읽지 않았거나 무효화된 상태다(settings.go 참고).
	policyMu    sync.RWMutex
	policyCache *SecurityPolicy

	// 클러스터 리플리카 모드. 이 노드가 메타 DB의 주인이 아닐 때 켜진다(cluster.go).
	replicaMu   sync.RWMutex
	replica     bool
	remoteAudit RemoteAudit
}

// Open은 메타 DB를 열고 마이그레이션을 적용한다.
func Open(ctx context.Context, dbPath string, secret *crypto.SecretBox) (*Store, error) {
	// busy_timeout과 foreign_keys는 커넥션마다 적용되어야 하므로 DSN에 담는다.
	// (아래 db.ExecContext로 켠 PRAGMA는 풀의 한 커넥션에만 적용될 수 있다.)
	dsn := strings.ReplaceAll(dbPath, "\\", "/") +
		"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// SQLite는 쓰기 단일화가 필요하다. modernc 드라이버는 스레드 세이프하지만
	// 커넥션 수를 제한해 잠금 경합을 줄인다.
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(8)
	db.SetConnMaxLifetime(time.Hour)

	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}

	s := &Store{db: db, secret: secret}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

// DB는 아직 리포지토리 메서드가 없는 조회를 위해 원시 핸들을 노출한다.
func (s *Store) DB() *sql.DB { return s.db }

// Secret은 시크릿 봉인기를 노출한다.
func (s *Store) Secret() *crypto.SecretBox { return s.secret }

// migrate는 embed된 migrations/*.sql을 버전 순서대로 한 번씩 적용한다.
func (s *Store) migrate(ctx context.Context) error {
	return s.migrateTo(ctx, math.MaxInt)
}

// migrateTo는 maxVersion까지만 적용한다.
//
// 상한을 둘 수 있게 한 이유는 시험 때문이다. 이관 마이그레이션(데이터를 옮기는 것)은
// "옛 상태에서 시작해야" 검증할 수 있는데, 항상 최신까지 적용해 버리면 옛 상태를
// 만들 방법이 없다. 운영 경로는 언제나 상한 없이 부른다.
func (s *Store) migrateTo(ctx context.Context, maxVersion int) error {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER PRIMARY KEY,
		name       TEXT NOT NULL,
		applied_at TEXT NOT NULL
	)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	applied := map[int]bool{}
	rows, err := s.db.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return fmt.Errorf("read schema_migrations: %w", err)
	}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return fmt.Errorf("scan version: %w", err)
		}
		applied[v] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate schema_migrations: %w", err)
	}

	files, err := fs.Glob(migrationFS, "migrations/*.sql")
	if err != nil {
		return fmt.Errorf("glob migrations: %w", err)
	}
	sort.Strings(files)

	for _, f := range files {
		base := path.Base(f)
		version, name, err := parseMigrationName(base)
		if err != nil {
			return err
		}
		if applied[version] || version > maxVersion {
			continue
		}
		body, err := migrationFS.ReadFile(f)
		if err != nil {
			return fmt.Errorf("read %s: %w", f, err)
		}
		if err := s.applyMigration(ctx, base, version, name, string(body)); err != nil {
			return err
		}
	}
	return nil
}

// noFKMarker는 외래키 검사를 끈 채로 적용해야 하는 마이그레이션의 표시다.
//
// SQLite는 컬럼의 NOT NULL이나 외래키 정의를 바꿀 수 없어, 그런 변경은 테이블을
// 새로 만들어 옮기고 옛 테이블을 지우는 방법밖에 없다. 그런데 외래키 검사가 켜져
// 있으면 DROP TABLE이 자식 행을 CASCADE로 함께 지운다 — 구조를 고치려다 데이터가
// 사라진다. PRAGMA foreign_keys는 트랜잭션 **밖에서만** 바뀌므로, 이런 파일은
// 커넥션을 하나 붙잡아 두고 그 위에서 처리한다.
const noFKMarker = "-- +no-foreign-keys"

// applyMigration은 파일 하나를 적용하고 적용 기록을 남긴다.
func (s *Store) applyMigration(ctx context.Context, base string, version int, name, body string) error {
	needsNoFK := strings.Contains(body, noFKMarker)

	// 일반 경로: 풀에서 아무 커넥션이나 써도 된다.
	if !needsNoFK {
		return s.runMigrationTx(ctx, s.db, base, version, name, body)
	}

	// 외래키를 끄려면 PRAGMA를 실행한 그 커넥션에서 계속 작업해야 한다.
	// 풀에 돌려주면 다음 문장이 다른 커넥션으로 나가 PRAGMA가 무의미해진다.
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("pin connection for %s: %w", base, err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		return fmt.Errorf("disable foreign keys for %s: %w", base, err)
	}
	runErr := s.runMigrationTx(ctx, conn, base, version, name, body)
	// 되돌리기는 성공/실패와 무관하게 반드시 한다. 이 커넥션은 곧 풀로 돌아가고,
	// 검사가 꺼진 채 돌아가면 이후의 모든 쓰기가 무결성 검사를 건너뛴다.
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = ON`); err != nil && runErr == nil {
		return fmt.Errorf("re-enable foreign keys after %s: %w", base, err)
	}
	if runErr != nil {
		return runErr
	}

	// 옮겨 담은 결과가 실제로 온전한지 확인한다. 검사를 껐으므로 이 확인이
	// 유일한 안전망이다.
	var brokenTable, brokenParent sql.NullString
	row := conn.QueryRowContext(ctx, `PRAGMA foreign_key_check`)
	if err := row.Scan(&brokenTable, new(any), &brokenParent, new(any)); err == nil {
		return fmt.Errorf("migration %s left dangling references: %s → %s",
			base, brokenTable.String, brokenParent.String)
	}
	return nil
}

// execer는 *sql.DB와 *sql.Conn을 함께 받는다.
type execer interface {
	BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error)
}

func (s *Store) runMigrationTx(ctx context.Context, e execer, base string, version int, name, body string) error {
	tx, err := e.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration %s: %w", base, err)
	}
	if _, err := tx.ExecContext(ctx, body); err != nil {
		tx.Rollback()
		return fmt.Errorf("apply migration %s: %w", base, err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)`,
		version, name, nowString(),
	); err != nil {
		tx.Rollback()
		return fmt.Errorf("record migration %s: %w", base, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration %s: %w", base, err)
	}
	return nil
}

func parseMigrationName(base string) (int, string, error) {
	numPart, rest, ok := strings.Cut(strings.TrimSuffix(base, ".sql"), "_")
	if !ok {
		return 0, "", fmt.Errorf("migration %q: expected NNNN_name.sql", base)
	}
	v, err := strconv.Atoi(numPart)
	if err != nil {
		return 0, "", fmt.Errorf("migration %q: bad version prefix: %w", base, err)
	}
	return v, rest, nil
}

// timeLayout은 시간 저장 형식이다.
//
// time.RFC3339Nano를 쓰지 않는 이유가 중요하다: RFC3339Nano는 소수점 뒤 0을
// 제거하므로 자리수가 가변이고, 그러면 문자열 정렬이 시간 정렬과 달라진다.
// 예를 들어 정확히 정초인 "…:40Z"와 "…:40.5Z"를 비교하면 'Z'(0x5A) > '.'(0x2E)라서
// 사전순으로는 앞의 값이 더 크지만 실제로는 더 이르다. SQLite는 시간을 문자열로
// 비교하므로 이 차이가 BETWEEN/`<`/ORDER BY를 조용히 틀리게 만든다.
// 소수점 9자리를 항상 채워 고정 폭으로 만들면 문자열 순서 = 시간 순서가 보장된다.
const timeLayout = "2006-01-02T15:04:05.000000000Z"

// formatTime은 시간을 저장용 고정 폭 문자열로 만든다.
func formatTime(t time.Time) string { return t.UTC().Format(timeLayout) }

func nowString() string { return formatTime(time.Now()) }

func timePtrString(t *time.Time) any {
	if t == nil {
		return nil
	}
	return formatTime(*t)
}

// parseTime은 저장된 시간 문자열을 파싱한다.
// 고정 폭 형식을 우선 시도하고, 실패하면 RFC3339 계열로 넘어간다 —
// 형식을 바꾸기 전에 기록된 값도 계속 읽을 수 있어야 한다.
func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(timeLayout, s); err == nil {
		return t
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t
	}
	return time.Time{}
}

func parseTimePtr(s sql.NullString) *time.Time {
	if !s.Valid || s.String == "" {
		return nil
	}
	t := parseTime(s.String)
	if t.IsZero() {
		return nil
	}
	return &t
}

// SchemaVersion은 적용된 마지막 마이그레이션 번호다.
//
// 클러스터가 쓴다: 노드 사이에 스키마가 다르면 복제된 행에 없는 컬럼이 생기거나
// 반대로 남는다. 그 사실을 참여 시점에 알 수 있어야 한다.
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	var v sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT max(version) FROM schema_migrations`).Scan(&v); err != nil {
		return 0, err
	}
	return int(v.Int64), nil
}
