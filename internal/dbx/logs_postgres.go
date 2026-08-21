package dbx

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"dbstudio/internal/dblog"
)

// logsPostgres는 PostgreSQL의 쿼리 통계와 현재 실행 쿼리를 읽는다.
//
// PostgreSQL의 서버 로그는 파일로만 기록되며 SQL로 읽으려면 pg_read_file
// (슈퍼유저 또는 pg_read_server_files 역할)이 필요하다. 관리형 서비스에서는
// 대개 불가능하므로, 시도해보고 실패하면 그 사실을 안내한다.
func logsPostgres(ctx context.Context, db *sql.DB, t Target, f *dblog.Filter, res *dblog.Result) {
	if f.WantsSource(dblog.SourceStatements) {
		pgStatStatements(ctx, db, t, f, res)
	}
	if f.WantsSource(dblog.SourceCurrent) {
		pgCurrentQueries(ctx, db, t, f, res)
	}
	if f.WantsSource(dblog.SourceErrorLog) {
		pgServerLog(ctx, db, f, res)
	}
}

func pgStatStatements(ctx context.Context, db *sql.DB, t Target, f *dblog.Filter, res *dblog.Result) {
	label := dblog.Label(dblog.SourceStatements)

	// 확장 설치 여부를 먼저 확인해 명확한 안내를 준다.
	var installed bool
	if err := db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'pg_stat_statements')`).
		Scan(&installed); err != nil {
		res.MarkSource(dblog.SourceStatements, label, false, 0, "확장 설치 여부를 확인할 수 없습니다")
		res.AddNote("pg_extension 조회 실패: %v", err)
		return
	}
	if !installed {
		res.MarkSource(dblog.SourceStatements, label, false, 0,
			"CREATE EXTENSION pg_stat_statements; 가 필요하고, "+
				"postgresql.conf의 shared_preload_libraries에 추가한 뒤 재시작해야 합니다")
		return
	}

	// pg_stat_statements의 컬럼 이름은 버전에 따라 다르다:
	//   PostgreSQL 13 미만: total_time, mean_time
	//   13 이상: total_exec_time, mean_exec_time
	// 버전을 확인해 맞는 컬럼을 쓴다. 틀린 컬럼을 쓰면 전체 조회가 실패한다.
	totalCol, meanCol, stddevCol := "total_exec_time", "mean_exec_time", "stddev_exec_time"
	var hasExecTime bool
	if err := db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.columns
		 WHERE table_name = 'pg_stat_statements' AND column_name = 'total_exec_time')`).
		Scan(&hasExecTime); err == nil && !hasExecTime {
		totalCol, meanCol, stddevCol = "total_time", "mean_time", "stddev_time"
	}

	dbName := strings.TrimSpace(t.Conn.DatabaseName)
	query := fmt.Sprintf(`
		SELECT s.queryid::text, s.query, s.calls,
		       s.%s, s.%s, s.max_exec_time_or_zero, s.min_exec_time_or_zero, s.%s,
		       s.rows, s.shared_blks_hit, s.shared_blks_read,
		       COALESCE(d.datname, ''), COALESCE(r.rolname, '')
		FROM (
		    SELECT queryid, query, calls, %s, %s, %s, rows,
		           shared_blks_hit, shared_blks_read, dbid, userid,
		           COALESCE(max_exec_time, 0) AS max_exec_time_or_zero,
		           COALESCE(min_exec_time, 0) AS min_exec_time_or_zero
		    FROM pg_stat_statements
		) s
		LEFT JOIN pg_database d ON d.oid = s.dbid
		LEFT JOIN pg_roles r ON r.oid = s.userid
		WHERE s.calls > 0`,
		totalCol, meanCol, stddevCol, totalCol, meanCol, stddevCol)

	args := []any{}
	if dbName != "" {
		query += ` AND (d.datname = $1 OR d.datname IS NULL)`
		args = append(args, dbName)
	}
	query += fmt.Sprintf(` ORDER BY s.%s DESC LIMIT %d`, totalCol, f.EffectiveLimit())

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		// max_exec_time은 PostgreSQL 14+에만 있다. 없으면 단순한 쿼리로 재시도한다.
		rows, err = pgStatStatementsFallback(ctx, db, t, f, totalCol, meanCol)
		if err != nil {
			res.MarkSource(dblog.SourceStatements, label, false, 0,
				"pg_stat_statements를 읽을 수 없습니다 (pg_read_all_stats 역할 필요)")
			res.AddNote("pg_stat_statements 조회 실패: %v", err)
			return
		}
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		var queryID, queryText, datname, rolname string
		var calls, nrows, blksHit, blksRead int64
		var totalMs, meanMs, maxMs, minMs, stddevMs float64

		if err := rows.Scan(&queryID, &queryText, &calls, &totalMs, &meanMs,
			&maxMs, &minMs, &stddevMs, &nrows, &blksHit, &blksRead,
			&datname, &rolname); err != nil {
			res.AddNote("쿼리 통계 행 스캔 실패: %v", err)
			break
		}
		if f.MinDurationMs > 0 && meanMs < f.MinDurationMs {
			continue
		}

		// pg_stat_statements의 query는 이미 $1, $2 형태로 정규화되어 있다.
		// 다만 다이제스트를 우리 방식으로 다시 계산하지 않고 queryid를 쓴다 —
		// 같은 DB 안에서는 그것이 더 정확한 식별자다.
		stat := dblog.QueryStat{
			Digest:     queryID,
			Normalized: dblog.TruncateQuery(strings.TrimSpace(queryText), 4000),
			Calls:      calls, TotalMs: totalMs, MeanMs: meanMs,
			MaxMs: maxMs, MinMs: minMs, StddevMs: stddevMs,
			RowsTotal: nrows, Database: datname, User: rolname,
			Extra: map[string]string{},
		}
		if calls > 0 {
			stat.RowsPerCall = float64(nrows) / float64(calls)
		}
		if blksHit+blksRead > 0 {
			stat.CacheHitPct = float64(blksHit) / float64(blksHit+blksRead) * 100
		}
		res.Stats = append(res.Stats, stat)
		count++
	}
	if err := rows.Err(); err != nil {
		res.AddNote("쿼리 통계 순회 실패: %v", err)
	}
	res.MarkSource(dblog.SourceStatements, label, true, count, "")
}

// pgStatStatementsFallback은 버전별 선택 컬럼 없이 최소 집합만 읽는다.
// max/min/stddev가 없는 구버전에서도 핵심 정보는 보여주기 위한 경로다.
func pgStatStatementsFallback(ctx context.Context, db *sql.DB, t Target, f *dblog.Filter, totalCol, meanCol string) (*sql.Rows, error) {
	dbName := strings.TrimSpace(t.Conn.DatabaseName)
	query := fmt.Sprintf(`
		SELECT s.queryid::text, s.query, s.calls,
		       s.%s, s.%s, 0::float8, 0::float8, 0::float8,
		       s.rows, s.shared_blks_hit, s.shared_blks_read,
		       COALESCE(d.datname, ''), COALESCE(r.rolname, '')
		FROM pg_stat_statements s
		LEFT JOIN pg_database d ON d.oid = s.dbid
		LEFT JOIN pg_roles r ON r.oid = s.userid
		WHERE s.calls > 0`, totalCol, meanCol)
	args := []any{}
	if dbName != "" {
		query += ` AND (d.datname = $1 OR d.datname IS NULL)`
		args = append(args, dbName)
	}
	query += fmt.Sprintf(` ORDER BY s.%s DESC LIMIT %d`, totalCol, f.EffectiveLimit())
	return db.QueryContext(ctx, query, args...)
}

func pgCurrentQueries(ctx context.Context, db *sql.DB, t Target, f *dblog.Filter, res *dblog.Result) {
	label := dblog.Label(dblog.SourceCurrent)
	dbName := strings.TrimSpace(t.Conn.DatabaseName)

	rows, err := db.QueryContext(ctx, `
		SELECT pid, COALESCE(usename, ''), COALESCE(client_addr::text, 'local'),
		       COALESCE(datname, ''), state,
		       COALESCE(EXTRACT(EPOCH FROM (now() - query_start)) * 1000, 0),
		       COALESCE(query, ''), COALESCE(wait_event_type, ''), COALESCE(wait_event, ''),
		       query_start
		FROM pg_stat_activity
		WHERE pid <> pg_backend_pid()
		  AND state IS NOT NULL AND state <> 'idle'
		  AND ($1 = '' OR datname = $1)
		ORDER BY query_start NULLS LAST
		LIMIT $2`, dbName, f.EffectiveLimit())
	if err != nil {
		res.MarkSource(dblog.SourceCurrent, label, false, 0, "pg_monitor 역할이 필요합니다")
		res.AddNote("pg_stat_activity 조회 실패: %v", err)
		return
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		var pid int64
		var user, client, datname, state, query, waitType, waitEvent string
		var durationMs float64
		var queryStart sql.NullTime

		if err := rows.Scan(&pid, &user, &client, &datname, &state,
			&durationMs, &query, &waitType, &waitEvent, &queryStart); err != nil {
			res.AddNote("실행 중인 쿼리 스캔 실패: %v", err)
			break
		}
		if f.MinDurationMs > 0 && durationMs < f.MinDurationMs {
			continue
		}
		normalized, digest := dblog.NormalizeAndDigest(query)

		at := time.Now().UTC()
		if queryStart.Valid {
			at = queryStart.Time.UTC()
		}
		msg := fmt.Sprintf("%s %.0fms", state, durationMs)
		if waitType != "" {
			msg += fmt.Sprintf(" (대기: %s/%s)", waitType, waitEvent)
		}

		extra := map[string]string{"pid": fmt.Sprintf("%d", pid), "state": state}
		if waitType != "" {
			extra["waitEventType"] = waitType
			extra["waitEvent"] = waitEvent
		}
		// 락 대기는 그 자체로 조사 대상이므로 심각도를 올린다.
		severity := severityForDuration(durationMs)
		if waitType == "Lock" && severity.Rank() < dblog.SeverityWarning.Rank() {
			severity = dblog.SeverityWarning
		}

		res.Entries = append(res.Entries, dblog.Entry{
			At: at, Severity: severity, Source: dblog.SourceCurrent, Message: msg,
			Query: dblog.TruncateQuery(query, 4000), Normalized: normalized, Digest: digest,
			DurationMs: durationMs, User: user, Database: datname, Client: client,
			Extra: extra,
		})
		count++
	}
	if err := rows.Err(); err != nil {
		res.AddNote("실행 중인 쿼리 순회 실패: %v", err)
	}
	res.MarkSource(dblog.SourceCurrent, label, true, count, "")
}

// pgServerLog는 서버 로그 파일을 읽으려 시도한다.
//
// 대부분의 환경에서 실패하는 것이 정상이다. 시도하는 이유는 자체 호스팅 환경에서는
// 성공하며, 그때 에러 로그를 함께 볼 수 있는 가치가 크기 때문이다.
// 실패하면 왜 못 읽는지와 대안을 안내한다.
func pgServerLog(ctx context.Context, db *sql.DB, f *dblog.Filter, res *dblog.Result) {
	label := dblog.Label(dblog.SourceErrorLog)

	// CSV 형식 로그가 활성화되어 있으면 구조화된 파싱이 가능하다.
	settings, err := scanKeyValue(ctx, db,
		`SELECT name, setting FROM pg_settings
		 WHERE name IN ('log_destination', 'logging_collector', 'log_directory', 'log_min_duration_statement')`)
	if err != nil {
		res.MarkSource(dblog.SourceErrorLog, label, false, 0,
			"로그 설정을 읽을 수 없습니다")
		return
	}

	if !strings.EqualFold(settings["logging_collector"], "on") {
		res.MarkSource(dblog.SourceErrorLog, label, false, 0,
			"logging_collector가 off입니다. 서버 로그는 stderr로만 나가며 SQL로 읽을 수 없습니다")
		return
	}

	// pg_current_logfile()은 슈퍼유저 또는 pg_monitor에게 허용된다.
	var logFile sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT pg_current_logfile()`).Scan(&logFile); err != nil {
		res.MarkSource(dblog.SourceErrorLog, label, false, 0,
			"현재 로그 파일 경로를 확인할 수 없습니다 (pg_monitor 역할 필요)")
		return
	}
	if !logFile.Valid || logFile.String == "" {
		res.MarkSource(dblog.SourceErrorLog, label, false, 0, "활성 로그 파일이 없습니다")
		return
	}

	// pg_read_file은 슈퍼유저 또는 pg_read_server_files 역할이 필요하다.
	// 마지막 부분만 읽어 큰 파일에서도 응답 시간을 지킨다.
	const tailBytes = 256 * 1024
	var content sql.NullString
	err = db.QueryRowContext(ctx,
		`SELECT pg_read_file($1, GREATEST(0, (pg_stat_file($1)).size - $2), $2)`,
		logFile.String, tailBytes).Scan(&content)
	if err != nil {
		res.MarkSource(dblog.SourceErrorLog, label, false, 0,
			"로그 파일 읽기 권한이 없습니다 (GRANT pg_read_server_files 또는 슈퍼유저 필요). "+
				"파일 경로: "+logFile.String)
		return
	}

	entries := parsePostgresLog(content.String, f)
	res.Entries = append(res.Entries, entries...)
	res.MarkSource(dblog.SourceErrorLog, label, true, len(entries), "")
}

// parsePostgresLog는 stderr 형식 로그를 파싱한다.
//
// 형식 예: 2026-08-13 12:34:56.789 UTC [1234] ERROR:  relation "x" does not exist
// 형식이 log_line_prefix 설정에 따라 달라지므로, 타임스탬프와 레벨만 최선으로 뽑고
// 나머지는 메시지로 둔다. 파싱에 실패한 줄은 이전 항목의 연속으로 붙인다.
func parsePostgresLog(content string, f *dblog.Filter) []dblog.Entry {
	entries := []dblog.Entry{}
	levels := map[string]dblog.Severity{
		"DEBUG": dblog.SeverityDebug, "INFO": dblog.SeverityInfo,
		"NOTICE": dblog.SeverityInfo, "LOG": dblog.SeverityInfo,
		"WARNING": dblog.SeverityWarning, "ERROR": dblog.SeverityError,
		"FATAL": dblog.SeverityFatal, "PANIC": dblog.SeverityFatal,
	}

	limit := f.EffectiveLimit()
	for line := range strings.SplitSeq(content, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}

		// 레벨 표기를 찾는다.
		var severity dblog.Severity
		var levelIdx = -1
		for name, sev := range levels {
			if i := strings.Index(line, name+":"); i >= 0 {
				if levelIdx < 0 || i < levelIdx {
					levelIdx = i
					severity = sev
				}
			}
		}
		if levelIdx < 0 {
			// 이전 항목의 연속 줄(쿼리 본문 등)로 취급한다.
			if len(entries) > 0 {
				last := &entries[len(entries)-1]
				last.Message = strings.TrimSpace(last.Message + " " + strings.TrimSpace(line))
			}
			continue
		}

		at := parsePostgresLogTime(line)
		message := strings.TrimSpace(line[levelIdx:])

		if !at.IsZero() && (at.Before(f.From) || at.After(f.To)) {
			continue
		}
		if severity != "" && f.MinSeverity != "" && severity.Rank() < f.MinSeverity.Rank() {
			continue
		}
		if at.IsZero() {
			at = time.Now().UTC()
		}

		entries = append(entries, dblog.Entry{
			At: at, Severity: severity, Source: dblog.SourceErrorLog,
			Message: dblog.TruncateQuery(message, 2000),
		})
		if len(entries) >= limit {
			break
		}
	}
	return entries
}

// parsePostgresLogTime은 줄 앞부분의 타임스탬프를 파싱한다.
func parsePostgresLogTime(line string) time.Time {
	if len(line) < 19 {
		return time.Time{}
	}
	layouts := []string{
		"2006-01-02 15:04:05.000 MST",
		"2006-01-02 15:04:05 MST",
		"2006-01-02 15:04:05.000",
		"2006-01-02 15:04:05",
	}
	for _, layout := range layouts {
		if len(line) >= len(layout) {
			if t, err := time.Parse(layout, line[:len(layout)]); err == nil {
				return t.UTC()
			}
		}
	}
	return time.Time{}
}
