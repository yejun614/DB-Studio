package dbx

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strings"
	"time"

	"dbstudio/internal/dblog"
	"dbstudio/internal/metric"
	"dbstudio/internal/model"
	"dbstudio/internal/schema"
)

// database/sql 기반 어댑터는 DSN 생성 방식만 다르므로 공통 구현을 공유한다.
type sqlAdapter struct {
	kind        model.DBKind
	driver      string
	defaultPort int
	caps        Capabilities

	// dsn은 접속 문자열을 만든다. 자격증명이 포함되므로 로그에 남기지 않는다.
	dsn func(t Target) (string, error)
	// versionQuery는 서버 버전 조회 쿼리다. 권한 부족 등으로 실패해도 Ping은 성공 처리한다.
	versionQuery string
	// needsDatabase가 true면 database_name이 비어 있을 때 Validate가 실패한다.
	needsDatabase bool
	// needsHost가 true면 host가 비어 있을 때 Validate가 실패한다.
	needsHost bool
	// introspect는 종류별 스키마 읽기 구현이다. nil이면 ErrNotImplemented를 반환한다.
	introspect func(ctx context.Context, db *sql.DB, t Target, s *schema.Schema) error
	// metrics는 종류별 지표 수집 구현이다. nil이면 ErrNotImplemented를 반환한다.
	metrics func(ctx context.Context, db *sql.DB, t Target, set *metric.Set)
	// logs는 종류별 로그 조회 구현이다. nil이면 ErrNotImplemented를 반환한다.
	logs func(ctx context.Context, db *sql.DB, t Target, f *dblog.Filter, res *dblog.Result)
}

// Logs는 대상 DB의 로그와 쿼리 통계를 조회한다.
func (a *sqlAdapter) Logs(ctx context.Context, t Target, f *dblog.Filter) (*dblog.Result, error) {
	if a.logs == nil {
		return nil, fmt.Errorf("%w: %s 로그 조회", ErrNotImplemented, a.kind)
	}
	// 여러 소스를 순차 조회하므로 커넥션을 몇 개 열어둔다.
	db, err := a.open(t, 3)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	if err := db.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("접속 실패: %w", err)
	}

	res := dblog.NewResult()
	a.logs(ctx, db, t, f, res)

	// 검색 조건은 소스별 SQL에 넣기 어렵고 소스마다 컬럼이 달라
	// 조회 후 공통 단계에서 적용한다.
	filterEntries(res, f)
	res.SortEntries()
	dblog.SortStats(res.Stats, f.StatsOrderBy)

	if len(res.Entries) > f.EffectiveLimit() {
		res.Entries = res.Entries[:f.EffectiveLimit()]
		res.Truncated = true
	}
	return res, nil
}

// filterEntries는 텍스트 검색과 심각도 조건을 적용한다.
func filterEntries(res *dblog.Result, f *dblog.Filter) {
	if f.Search == "" && f.MinSeverity == "" {
		return
	}

	var re *regexp.Regexp
	if f.Search != "" && f.Regex {
		compiled, err := regexp.Compile("(?i)" + f.Search)
		if err != nil {
			res.AddNote("정규식이 올바르지 않아 부분 문자열 검색으로 처리했습니다: %v", err)
		} else {
			re = compiled
		}
	}
	needle := strings.ToLower(f.Search)

	kept := make([]dblog.Entry, 0, len(res.Entries))
	for _, e := range res.Entries {
		if f.MinSeverity != "" && e.Severity.Rank() < f.MinSeverity.Rank() {
			continue
		}
		if f.Search != "" {
			// 메시지와 쿼리를 함께 검색한다. 사용자는 둘을 구분해 찾지 않는다.
			haystack := e.Message + "\n" + e.Query + "\n" + e.Normalized
			if re != nil {
				if !re.MatchString(haystack) {
					continue
				}
			} else if !strings.Contains(strings.ToLower(haystack), needle) {
				continue
			}
		}
		kept = append(kept, e)
	}
	res.Entries = kept

	// 통계도 검색어로 걸러 화면이 일관되게 보이도록 한다.
	if f.Search != "" {
		keptStats := make([]dblog.QueryStat, 0, len(res.Stats))
		for _, s := range res.Stats {
			haystack := s.Normalized + "\n" + s.Sample
			if re != nil {
				if !re.MatchString(haystack) {
					continue
				}
			} else if !strings.Contains(strings.ToLower(haystack), needle) {
				continue
			}
			keptStats = append(keptStats, s)
		}
		res.Stats = keptStats
	}
}

// Introspect는 대상 DB의 스키마를 읽어 IR로 반환한다.
func (a *sqlAdapter) Introspect(ctx context.Context, t Target) (*schema.Schema, error) {
	if a.introspect == nil {
		return nil, fmt.Errorf("%w: %s introspect", ErrNotImplemented, a.kind)
	}
	db, err := a.open(t, 4)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	s := &schema.Schema{
		Dialect:    string(a.kind),
		Shape:      schema.ShapeRelational,
		Name:       t.Conn.DatabaseName,
		CapturedAt: time.Now().UTC(),
		Tables:     []*schema.Table{},
		Views:      []*schema.View{},
	}
	if err := a.introspect(ctx, db, t, s); err != nil {
		return nil, err
	}
	s.Sort()
	return s, nil
}

func (a *sqlAdapter) Kind() model.DBKind         { return a.kind }
func (a *sqlAdapter) Capabilities() Capabilities { return a.caps }
func (a *sqlAdapter) DefaultPort() int           { return a.defaultPort }

func (a *sqlAdapter) Validate(t Target) error {
	if t.Conn == nil {
		return fmt.Errorf("커넥션 정보가 없습니다")
	}
	if a.needsHost && strings.TrimSpace(t.Conn.Host) == "" {
		return fmt.Errorf("호스트를 입력하세요")
	}
	if a.needsDatabase && strings.TrimSpace(t.Conn.DatabaseName) == "" {
		return fmt.Errorf("%s를 입력하세요", dbLabels[a.kind])
	}
	if _, err := a.dsn(t); err != nil {
		return err
	}
	return nil
}

// open은 대상 DB에 대한 핸들을 만든다. 호출부가 Close해야 한다.
// maxConns는 introspect처럼 여러 쿼리를 순차 실행하는 경우를 위해 조절한다.
func (a *sqlAdapter) open(t Target, maxConns int) (*sql.DB, error) {
	dsn, err := a.dsn(t)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open(a.driver, dsn)
	if err != nil {
		return nil, fmt.Errorf("접속 설정 오류: %w", err)
	}
	db.SetMaxOpenConns(maxConns)
	db.SetMaxIdleConns(maxConns)
	return db, nil
}

// ExecRaw는 대상 DB에 임의의 SQL을 실행한다.
//
// 마이그레이션 실행(P7)과 테스트에서 쓰기 위한 저수준 통로다. 호출부가 권한을
// 확인한 뒤에만 호출해야 하며, 여기서는 어떤 검증도 하지 않는다.
func ExecRaw(ctx context.Context, a Adapter, t Target, sql string) error {
	sa, ok := a.(*sqlAdapter)
	if !ok {
		return fmt.Errorf("%w: %s는 SQL 실행을 지원하지 않습니다", ErrNotImplemented, a.Kind())
	}
	db, err := sa.open(t, 1)
	if err != nil {
		return err
	}
	defer db.Close()

	if _, err := db.ExecContext(ctx, sql); err != nil {
		return fmt.Errorf("실행 실패: %w", err)
	}
	return nil
}

// Ping은 접속 → 응답 확인 → 버전 조회 순으로 진행한다.
func (a *sqlAdapter) Ping(ctx context.Context, t Target) (*ServerInfo, error) {
	db, err := a.open(t, 1)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	start := time.Now()
	if err := db.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("접속 실패: %w", err)
	}
	latency := time.Since(start)

	info := &ServerInfo{
		Version:  "unknown",
		Latency:  latency,
		LatencyM: float64(latency.Microseconds()) / 1000,
		Extra:    map[string]string{},
	}
	if a.versionQuery != "" {
		var version sql.NullString
		// 버전 조회는 권한에 따라 실패할 수 있다. 접속 자체는 성공했으므로 에러를 삼킨다.
		if err := db.QueryRowContext(ctx, a.versionQuery).Scan(&version); err == nil && version.Valid {
			info.Version = strings.TrimSpace(firstLine(version.String))
		}
	}
	return info, nil
}

func (a *sqlAdapter) Redacted(t Target) string {
	if t.Conn == nil {
		return string(a.kind)
	}
	c := t.Conn
	user := t.Username()
	if user == "" {
		user = "-"
	}
	if a.kind == model.KindSQLite {
		return fmt.Sprintf("sqlite:%s", c.DatabaseName)
	}
	return fmt.Sprintf("%s://%s:***@%s:%d/%s", a.kind, user, c.Host, port(c, a.defaultPort), c.DatabaseName)
}

func port(c *model.Connection, def int) int {
	if c.Port > 0 {
		return c.Port
	}
	return def
}

func firstLine(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		return s[:i]
	}
	return s
}
