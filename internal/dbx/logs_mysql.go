package dbx

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"dbstudio/internal/dblog"
)

// logsMySQL은 MySQL의 로그와 쿼리 통계를 읽는다.
//
// 소스:
//   - mysql.slow_log 테이블: log_output에 TABLE이 포함되어야 읽을 수 있다.
//     파일로만 기록하면 SQL로 접근할 방법이 없으므로 그 사실을 안내한다.
//   - performance_schema.events_statements_summary_by_digest: 누적 쿼리 통계.
//     MySQL이 이미 정규화한 DIGEST_TEXT를 제공하므로 그것을 그대로 쓴다.
//   - information_schema.PROCESSLIST: 현재 실행 중인 쿼리.
func logsMySQL(ctx context.Context, db *sql.DB, t Target, f *dblog.Filter, res *dblog.Result) {
	if f.WantsSource(dblog.SourceSlowQuery) {
		mysqlSlowLog(ctx, db, f, res)
	}
	if f.WantsSource(dblog.SourceStatements) {
		mysqlStatementDigest(ctx, db, t, f, res)
	}
	if f.WantsSource(dblog.SourceCurrent) {
		mysqlCurrentQueries(ctx, db, f, res)
	}
}

func mysqlSlowLog(ctx context.Context, db *sql.DB, f *dblog.Filter, res *dblog.Result) {
	label := dblog.Label(dblog.SourceSlowQuery)

	// 먼저 슬로우 로그 설정을 확인해 "왜 비어 있는지"를 설명할 수 있게 한다.
	settings, err := scanKeyValue(ctx, db,
		`SHOW GLOBAL VARIABLES WHERE Variable_name IN
		 ('slow_query_log', 'log_output', 'long_query_time')`)
	if err != nil {
		res.MarkSource(dblog.SourceSlowQuery, label, false, 0,
			"슬로우 로그 설정을 읽을 수 없습니다 (권한 부족)")
		res.AddNote("슬로우 로그 설정 조회 실패: %v", err)
		return
	}

	enabled := strings.EqualFold(settings["slow_query_log"], "ON")
	toTable := strings.Contains(strings.ToUpper(settings["log_output"]), "TABLE")

	if !enabled {
		res.MarkSource(dblog.SourceSlowQuery, label, false, 0,
			"SET GLOBAL slow_query_log = ON; 으로 활성화하세요 "+
				"(현재 long_query_time = "+settings["long_query_time"]+"초)")
		return
	}
	if !toTable {
		res.MarkSource(dblog.SourceSlowQuery, label, false, 0,
			"log_output이 '"+settings["log_output"]+"'입니다. SQL로 읽으려면 "+
				"SET GLOBAL log_output = 'TABLE'; 이 필요합니다 (파일 로그는 서버에서 직접 확인)")
		return
	}

	// slow_log의 query_time은 TIME 타입이므로 초 단위 실수로 변환한다.
	rows, err := db.QueryContext(ctx, `
		SELECT start_time,
		       TIME_TO_SEC(query_time) + MICROSECOND(query_time) / 1000000 AS secs,
		       rows_examined, rows_sent, user_host, db, CONVERT(sql_text USING utf8mb4)
		FROM mysql.slow_log
		WHERE start_time >= ? AND start_time <= ?
		ORDER BY start_time DESC
		LIMIT ?`,
		f.From, f.To, f.EffectiveLimit())
	if err != nil {
		res.MarkSource(dblog.SourceSlowQuery, label, false, 0,
			"mysql.slow_log 조회 권한이 필요합니다")
		res.AddNote("mysql.slow_log 조회 실패: %v", err)
		return
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		var startTime time.Time
		var secs float64
		var rowsExamined, rowsSent int64
		var userHost, dbName, sqlText sql.NullString
		if err := rows.Scan(&startTime, &secs, &rowsExamined, &rowsSent,
			&userHost, &dbName, &sqlText); err != nil {
			res.AddNote("슬로우 로그 행 스캔 실패: %v", err)
			break
		}

		durationMs := secs * 1000
		if f.MinDurationMs > 0 && durationMs < f.MinDurationMs {
			continue
		}
		query := sqlText.String
		normalized, digest := dblog.NormalizeAndDigest(query)
		user, client := splitUserHost(userHost.String)

		res.Entries = append(res.Entries, dblog.Entry{
			At: startTime.UTC(), Severity: dblog.SeverityWarning,
			Source: dblog.SourceSlowQuery,
			Message: fmt.Sprintf("슬로우 쿼리 %.0fms (검사 %d행 / 반환 %d행)",
				durationMs, rowsExamined, rowsSent),
			Query: dblog.TruncateQuery(query, 4000), Normalized: normalized, Digest: digest,
			DurationMs: durationMs, RowsExamined: rowsExamined, RowsSent: rowsSent,
			User: user, Database: dbName.String, Client: client,
		})
		count++
	}
	if err := rows.Err(); err != nil {
		res.AddNote("슬로우 로그 순회 실패: %v", err)
	}
	res.MarkSource(dblog.SourceSlowQuery, label, true, count, "")
}

func mysqlStatementDigest(ctx context.Context, db *sql.DB, t Target, f *dblog.Filter, res *dblog.Result) {
	label := dblog.Label(dblog.SourceStatements)
	dbName := strings.TrimSpace(t.Conn.DatabaseName)

	// performance_schema의 단위는 피코초(picosecond)다. 1e9로 나누면 밀리초가 된다.
	// 대상 DB로 범위를 좁히되, SCHEMA_NAME이 NULL인 항목(준비된 문장 등)도 포함한다.
	query := `
		SELECT DIGEST, DIGEST_TEXT, COUNT_STAR,
		       SUM_TIMER_WAIT / 1000000000 AS total_ms,
		       AVG_TIMER_WAIT / 1000000000 AS mean_ms,
		       MAX_TIMER_WAIT / 1000000000 AS max_ms,
		       MIN_TIMER_WAIT / 1000000000 AS min_ms,
		       SUM_ROWS_SENT, SUM_ROWS_EXAMINED, SUM_NO_INDEX_USED,
		       FIRST_SEEN, LAST_SEEN, IFNULL(SCHEMA_NAME, '')
		FROM performance_schema.events_statements_summary_by_digest
		WHERE DIGEST_TEXT IS NOT NULL`
	args := []any{}
	if dbName != "" {
		query += ` AND (SCHEMA_NAME = ? OR SCHEMA_NAME IS NULL)`
		args = append(args, dbName)
	}
	query += ` ORDER BY SUM_TIMER_WAIT DESC LIMIT ?`
	args = append(args, f.EffectiveLimit())

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		res.MarkSource(dblog.SourceStatements, label, false, 0,
			"performance_schema가 비활성이거나 권한이 없습니다 "+
				"(my.cnf에 performance_schema = ON)")
		res.AddNote("performance_schema 통계 조회 실패: %v", err)
		return
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		var digest, digestText, schemaName sql.NullString
		var calls, rowsSent, rowsExamined, noIndexUsed int64
		var totalMs, meanMs, maxMs, minMs float64
		var firstSeen, lastSeen sql.NullTime

		if err := rows.Scan(&digest, &digestText, &calls, &totalMs, &meanMs, &maxMs, &minMs,
			&rowsSent, &rowsExamined, &noIndexUsed, &firstSeen, &lastSeen, &schemaName); err != nil {
			res.AddNote("쿼리 통계 행 스캔 실패: %v", err)
			break
		}
		if calls == 0 {
			continue
		}
		if f.MinDurationMs > 0 && meanMs < f.MinDurationMs {
			continue
		}

		// MySQL의 DIGEST_TEXT는 이미 정규화된 형태(`SELECT * FROM t WHERE id = ?`)다.
		// 우리 정규화기를 다시 돌리지 않고 그대로 쓰되, 다이제스트는 소스가 준 값을 쓴다.
		normalized := strings.TrimSpace(digestText.String)
		stat := dblog.QueryStat{
			Digest: digest.String, Normalized: dblog.TruncateQuery(normalized, 4000),
			Calls: calls, TotalMs: totalMs, MeanMs: meanMs, MaxMs: maxMs, MinMs: minMs,
			RowsTotal: rowsSent, Database: schemaName.String,
			Extra: map[string]string{},
		}
		if calls > 0 {
			stat.RowsPerCall = float64(rowsSent) / float64(calls)
		}
		if rowsExamined > 0 && rowsSent >= 0 {
			// 검사한 행 대비 반환한 행의 비율. 낮으면 인덱스가 부족하다는 신호다.
			stat.Extra["rowsExamined"] = fmt.Sprintf("%d", rowsExamined)
			if rowsSent > 0 {
				stat.Extra["examinedPerSent"] = fmt.Sprintf("%.1f", float64(rowsExamined)/float64(rowsSent))
			}
		}
		if noIndexUsed > 0 {
			stat.Extra["noIndexUsed"] = fmt.Sprintf("%d", noIndexUsed)
		}
		if firstSeen.Valid {
			ts := firstSeen.Time.UTC()
			stat.FirstSeen = &ts
		}
		if lastSeen.Valid {
			ts := lastSeen.Time.UTC()
			stat.LastSeen = &ts
		}
		res.Stats = append(res.Stats, stat)
		count++
	}
	if err := rows.Err(); err != nil {
		res.AddNote("쿼리 통계 순회 실패: %v", err)
	}
	res.MarkSource(dblog.SourceStatements, label, true, count, "")
}

func mysqlCurrentQueries(ctx context.Context, db *sql.DB, f *dblog.Filter, res *dblog.Result) {
	label := dblog.Label(dblog.SourceCurrent)

	rows, err := db.QueryContext(ctx, `
		SELECT ID, USER, HOST, IFNULL(DB, ''), COMMAND, TIME, STATE, IFNULL(INFO, '')
		FROM information_schema.PROCESSLIST
		WHERE COMMAND NOT IN ('Sleep', 'Daemon', 'Binlog Dump')
		  AND ID <> CONNECTION_ID()
		ORDER BY TIME DESC
		LIMIT ?`, f.EffectiveLimit())
	if err != nil {
		res.MarkSource(dblog.SourceCurrent, label, false, 0, "PROCESS 권한이 필요합니다")
		return
	}
	defer rows.Close()

	now := time.Now().UTC()
	count := 0
	for rows.Next() {
		var id int64
		var user, host, dbName, command, state string
		var elapsed int64
		var info string
		if err := rows.Scan(&id, &user, &host, &dbName, &command, &elapsed, &state, &info); err != nil {
			res.AddNote("실행 중인 쿼리 스캔 실패: %v", err)
			break
		}
		durationMs := float64(elapsed) * 1000
		if f.MinDurationMs > 0 && durationMs < f.MinDurationMs {
			continue
		}
		normalized, digest := dblog.NormalizeAndDigest(info)

		// 시작 시각을 경과 시간으로 역산한다. PROCESSLIST는 시작 시각을 주지 않는다.
		res.Entries = append(res.Entries, dblog.Entry{
			At:       now.Add(-time.Duration(elapsed) * time.Second),
			Severity: severityForDuration(durationMs),
			Source:   dblog.SourceCurrent,
			Message:  fmt.Sprintf("실행 중 %ds (%s / %s)", elapsed, command, orDefault(state, "-")),
			Query:    dblog.TruncateQuery(info, 4000), Normalized: normalized, Digest: digest,
			DurationMs: durationMs, User: user, Database: dbName, Client: host,
			Extra: map[string]string{"threadId": fmt.Sprintf("%d", id), "command": command},
		})
		count++
	}
	if err := rows.Err(); err != nil {
		res.AddNote("실행 중인 쿼리 순회 실패: %v", err)
	}
	res.MarkSource(dblog.SourceCurrent, label, true, count, "")
}

// splitUserHost는 MySQL의 "user[user] @ host [ip]" 형식을 분리한다.
func splitUserHost(userHost string) (user, host string) {
	if userHost == "" {
		return "", ""
	}
	parts := strings.SplitN(userHost, "[", 2)
	user = strings.TrimSpace(parts[0])
	if i := strings.Index(userHost, "@"); i >= 0 {
		host = strings.TrimSpace(userHost[i+1:])
		host = strings.Trim(host, " []")
	}
	return user, host
}

// severityForDuration은 실행 시간에 따라 심각도를 정한다.
// 실행 중인 쿼리는 그 자체로 오류가 아니지만, 오래 걸릴수록 주목해야 한다.
func severityForDuration(ms float64) dblog.Severity {
	switch {
	case ms >= 60000:
		return dblog.SeverityError
	case ms >= 5000:
		return dblog.SeverityWarning
	}
	return dblog.SeverityInfo
}

func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}
