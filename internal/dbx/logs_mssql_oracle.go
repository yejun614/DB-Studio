package dbx

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"dbstudio/internal/dblog"
)

// ---------- MS-SQL ----------

// logsMSSQL은 MS-SQL의 쿼리 통계, 실행 중인 쿼리, 에러 로그를 읽는다.
func logsMSSQL(ctx context.Context, db *sql.DB, t Target, f *dblog.Filter, res *dblog.Result) {
	if f.WantsSource(dblog.SourceStatements) {
		mssqlQueryStats(ctx, db, f, res)
	}
	if f.WantsSource(dblog.SourceCurrent) {
		mssqlCurrentQueries(ctx, db, f, res)
	}
	if f.WantsSource(dblog.SourceErrorLog) {
		mssqlErrorLog(ctx, db, f, res)
	}
}

func mssqlQueryStats(ctx context.Context, db *sql.DB, f *dblog.Filter, res *dblog.Result) {
	label := dblog.Label(dblog.SourceStatements)

	// dm_exec_query_stats의 시간 단위는 마이크로초다.
	// query_hash를 다이제스트로 쓰면 리터럴만 다른 쿼리가 하나로 묶인다.
	rows, err := db.QueryContext(ctx, fmt.Sprintf(`
		SELECT TOP (%d)
		    CONVERT(VARCHAR(34), qs.query_hash, 1) AS digest,
		    SUBSTRING(st.text,
		        (qs.statement_start_offset / 2) + 1,
		        ((CASE qs.statement_end_offset WHEN -1 THEN DATALENGTH(st.text)
		          ELSE qs.statement_end_offset END - qs.statement_start_offset) / 2) + 1) AS query_text,
		    qs.execution_count,
		    qs.total_elapsed_time / 1000.0 AS total_ms,
		    (qs.total_elapsed_time / qs.execution_count) / 1000.0 AS mean_ms,
		    qs.max_elapsed_time / 1000.0 AS max_ms,
		    qs.min_elapsed_time / 1000.0 AS min_ms,
		    qs.total_rows,
		    qs.total_logical_reads, qs.total_physical_reads,
		    qs.creation_time, qs.last_execution_time,
		    ISNULL(DB_NAME(st.dbid), '') AS db_name
		FROM sys.dm_exec_query_stats qs
		CROSS APPLY sys.dm_exec_sql_text(qs.sql_handle) st
		WHERE qs.execution_count > 0 AND st.text IS NOT NULL
		ORDER BY qs.total_elapsed_time DESC`, f.EffectiveLimit()))
	if err != nil {
		res.MarkSource(dblog.SourceStatements, label, false, 0,
			"VIEW SERVER STATE 권한이 필요합니다")
		res.AddNote("dm_exec_query_stats 조회 실패: %v", err)
		return
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		var digest, queryText, dbName string
		var calls, totalRows, logicalReads, physicalReads int64
		var totalMs, meanMs, maxMs, minMs float64
		var creationTime, lastExec sql.NullTime

		if err := rows.Scan(&digest, &queryText, &calls, &totalMs, &meanMs, &maxMs, &minMs,
			&totalRows, &logicalReads, &physicalReads, &creationTime, &lastExec, &dbName); err != nil {
			res.AddNote("쿼리 통계 행 스캔 실패: %v", err)
			break
		}
		if f.MinDurationMs > 0 && meanMs < f.MinDurationMs {
			continue
		}

		// MS-SQL은 정규화된 텍스트를 주지 않으므로 직접 정규화한다.
		normalized, _ := dblog.NormalizeAndDigest(queryText)
		stat := dblog.QueryStat{
			Digest: digest, Normalized: dblog.TruncateQuery(normalized, 4000),
			Sample: dblog.TruncateQuery(queryText, 2000),
			Calls:  calls, TotalMs: totalMs, MeanMs: meanMs, MaxMs: maxMs, MinMs: minMs,
			RowsTotal: totalRows, Database: dbName,
			Extra: map[string]string{},
		}
		if calls > 0 {
			stat.RowsPerCall = float64(totalRows) / float64(calls)
		}
		if logicalReads > 0 {
			// 논리 읽기 중 물리 읽기(디스크) 비율의 역이 버퍼 적중률이다.
			stat.CacheHitPct = float64(logicalReads-physicalReads) / float64(logicalReads) * 100
			stat.Extra["logicalReads"] = fmt.Sprintf("%d", logicalReads)
		}
		if creationTime.Valid {
			ts := creationTime.Time.UTC()
			stat.FirstSeen = &ts
		}
		if lastExec.Valid {
			ts := lastExec.Time.UTC()
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

func mssqlCurrentQueries(ctx context.Context, db *sql.DB, f *dblog.Filter, res *dblog.Result) {
	label := dblog.Label(dblog.SourceCurrent)

	rows, err := db.QueryContext(ctx, fmt.Sprintf(`
		SELECT TOP (%d)
		    r.session_id, ISNULL(s.login_name, ''), ISNULL(s.host_name, ''),
		    ISNULL(DB_NAME(r.database_id), ''), r.status,
		    r.total_elapsed_time / 1000.0 AS elapsed_ms,
		    ISNULL(st.text, ''), ISNULL(r.wait_type, ''), r.blocking_session_id,
		    r.start_time
		FROM sys.dm_exec_requests r
		JOIN sys.dm_exec_sessions s ON s.session_id = r.session_id
		OUTER APPLY sys.dm_exec_sql_text(r.sql_handle) st
		WHERE s.is_user_process = 1 AND r.session_id <> @@SPID
		ORDER BY r.total_elapsed_time DESC`, f.EffectiveLimit()))
	if err != nil {
		res.MarkSource(dblog.SourceCurrent, label, false, 0, "VIEW SERVER STATE 권한이 필요합니다")
		return
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		var sessionID, blockingSession int64
		var login, host, dbName, status, queryText, waitType string
		var elapsedMs float64
		var startTime sql.NullTime

		if err := rows.Scan(&sessionID, &login, &host, &dbName, &status,
			&elapsedMs, &queryText, &waitType, &blockingSession, &startTime); err != nil {
			res.AddNote("실행 중인 쿼리 스캔 실패: %v", err)
			break
		}
		if f.MinDurationMs > 0 && elapsedMs < f.MinDurationMs {
			continue
		}
		normalized, digest := dblog.NormalizeAndDigest(queryText)

		at := time.Now().UTC()
		if startTime.Valid {
			at = startTime.Time.UTC()
		}
		msg := fmt.Sprintf("%s %.0fms", status, elapsedMs)
		extra := map[string]string{
			"sessionId": fmt.Sprintf("%d", sessionID), "status": status,
		}
		severity := severityForDuration(elapsedMs)
		if blockingSession != 0 {
			// 다른 세션에 막혀 있으면 원인 세션을 함께 보여준다.
			msg += fmt.Sprintf(" (세션 %d에 의해 차단)", blockingSession)
			extra["blockingSessionId"] = fmt.Sprintf("%d", blockingSession)
			severity = dblog.SeverityWarning
		}
		if waitType != "" {
			msg += " 대기: " + waitType
			extra["waitType"] = waitType
		}

		res.Entries = append(res.Entries, dblog.Entry{
			At: at, Severity: severity, Source: dblog.SourceCurrent, Message: msg,
			Query: dblog.TruncateQuery(queryText, 4000), Normalized: normalized, Digest: digest,
			DurationMs: elapsedMs, User: login, Database: dbName, Client: host,
			Extra: extra,
		})
		count++
	}
	if err := rows.Err(); err != nil {
		res.AddNote("실행 중인 쿼리 순회 실패: %v", err)
	}
	res.MarkSource(dblog.SourceCurrent, label, true, count, "")
}

// mssqlErrorLog는 sp_readerrorlog로 서버 에러 로그를 읽는다.
// 이 프로시저는 sysadmin 또는 securityadmin 권한이 필요하다.
func mssqlErrorLog(ctx context.Context, db *sql.DB, f *dblog.Filter, res *dblog.Result) {
	label := dblog.Label(dblog.SourceErrorLog)

	// 임시 테이블에 받아 시간 범위로 걸러낸다.
	// sp_readerrorlog의 파라미터: 로그번호, 로그종류(1=에러로그), 검색어1, 검색어2
	rows, err := db.QueryContext(ctx, `
		DECLARE @log TABLE (LogDate DATETIME, ProcessInfo NVARCHAR(64), Text NVARCHAR(MAX));
		INSERT INTO @log EXEC sp_readerrorlog 0, 1;
		SELECT LogDate, ISNULL(ProcessInfo, ''), ISNULL(Text, '')
		FROM @log
		WHERE LogDate >= @p1 AND LogDate <= @p2
		ORDER BY LogDate DESC;`, f.From, f.To)
	if err != nil {
		res.MarkSource(dblog.SourceErrorLog, label, false, 0,
			"sp_readerrorlog 실행 권한이 필요합니다 (sysadmin 또는 securityadmin)")
		return
	}
	defer rows.Close()

	limit := f.EffectiveLimit()
	count := 0
	for rows.Next() && count < limit {
		var logDate time.Time
		var processInfo, text string
		if err := rows.Scan(&logDate, &processInfo, &text); err != nil {
			res.AddNote("에러 로그 스캔 실패: %v", err)
			break
		}
		severity := mssqlLogSeverity(text)
		if f.MinSeverity != "" && severity.Rank() < f.MinSeverity.Rank() {
			continue
		}
		res.Entries = append(res.Entries, dblog.Entry{
			At: logDate.UTC(), Severity: severity, Source: dblog.SourceErrorLog,
			Message: dblog.TruncateQuery(strings.TrimSpace(text), 2000),
			Extra:   map[string]string{"processInfo": processInfo},
		})
		count++
	}
	if err := rows.Err(); err != nil {
		res.AddNote("에러 로그 순회 실패: %v", err)
	}
	res.MarkSource(dblog.SourceErrorLog, label, true, count, "")
}

// mssqlLogSeverity는 로그 본문에서 심각도를 추정한다.
// MS-SQL 에러 로그는 레벨 컬럼을 주지 않으므로 본문의 키워드로 판단한다.
func mssqlLogSeverity(text string) dblog.Severity {
	upper := strings.ToUpper(text)
	switch {
	case strings.Contains(upper, "SEVERITY: 2") || strings.Contains(upper, "FATAL"):
		return dblog.SeverityFatal
	case strings.Contains(upper, "ERROR") || strings.Contains(upper, "FAILED") ||
		strings.Contains(upper, "CANNOT") || strings.Contains(upper, "DEADLOCK"):
		return dblog.SeverityError
	case strings.Contains(upper, "WARNING") || strings.Contains(upper, "TIMEOUT"):
		return dblog.SeverityWarning
	}
	return dblog.SeverityInfo
}

// ---------- Oracle ----------

// logsOracle은 Oracle의 쿼리 통계와 alert 로그를 읽는다.
func logsOracle(ctx context.Context, db *sql.DB, t Target, f *dblog.Filter, res *dblog.Result) {
	if f.WantsSource(dblog.SourceStatements) {
		oracleQueryStats(ctx, db, f, res)
	}
	if f.WantsSource(dblog.SourceCurrent) {
		oracleCurrentQueries(ctx, db, f, res)
	}
	if f.WantsSource(dblog.SourceErrorLog) {
		oracleAlertLog(ctx, db, f, res)
	}
}

func oracleQueryStats(ctx context.Context, db *sql.DB, f *dblog.Filter, res *dblog.Result) {
	label := dblog.Label(dblog.SourceStatements)

	// v$sql의 시간 단위는 마이크로초다. sql_id를 다이제스트로 쓴다.
	// sql_fulltext는 CLOB이라 조회가 무거우므로 앞부분만 가져온다.
	rows, err := db.QueryContext(ctx, `
		SELECT * FROM (
		  SELECT sql_id,
		         DBMS_LOB.SUBSTR(sql_fulltext, 3900, 1) AS query_text,
		         executions,
		         elapsed_time / 1000 AS total_ms,
		         (elapsed_time / GREATEST(executions, 1)) / 1000 AS mean_ms,
		         rows_processed,
		         buffer_gets, disk_reads,
		         first_load_time, last_active_time,
		         parsing_schema_name
		  FROM v$sql
		  WHERE executions > 0 AND sql_fulltext IS NOT NULL
		  ORDER BY elapsed_time DESC
		) WHERE rownum <= :1`, f.EffectiveLimit())
	if err != nil {
		res.MarkSource(dblog.SourceStatements, label, false, 0,
			"v$sql 조회 권한이 필요합니다 (SELECT_CATALOG_ROLE)")
		res.AddNote("v$sql 조회 실패: %v", err)
		return
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		var sqlID, schemaName string
		var queryText sql.NullString
		var calls, rowsProcessed, bufferGets, diskReads int64
		var totalMs, meanMs float64
		var firstLoad sql.NullString
		var lastActive sql.NullTime

		if err := rows.Scan(&sqlID, &queryText, &calls, &totalMs, &meanMs,
			&rowsProcessed, &bufferGets, &diskReads, &firstLoad, &lastActive,
			&schemaName); err != nil {
			res.AddNote("쿼리 통계 행 스캔 실패: %v", err)
			break
		}
		if f.MinDurationMs > 0 && meanMs < f.MinDurationMs {
			continue
		}
		normalized, _ := dblog.NormalizeAndDigest(queryText.String)

		stat := dblog.QueryStat{
			Digest: sqlID, Normalized: dblog.TruncateQuery(normalized, 4000),
			Sample: dblog.TruncateQuery(queryText.String, 2000),
			Calls:  calls, TotalMs: totalMs, MeanMs: meanMs,
			RowsTotal: rowsProcessed, User: schemaName,
			Extra: map[string]string{},
		}
		if calls > 0 {
			stat.RowsPerCall = float64(rowsProcessed) / float64(calls)
		}
		if bufferGets > 0 {
			stat.CacheHitPct = float64(bufferGets-diskReads) / float64(bufferGets) * 100
			stat.Extra["bufferGets"] = fmt.Sprintf("%d", bufferGets)
		}
		if lastActive.Valid {
			ts := lastActive.Time.UTC()
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

func oracleCurrentQueries(ctx context.Context, db *sql.DB, f *dblog.Filter, res *dblog.Result) {
	label := dblog.Label(dblog.SourceCurrent)

	rows, err := db.QueryContext(ctx, `
		SELECT * FROM (
		  SELECT s.sid, NVL(s.username, ''), NVL(s.machine, ''), NVL(s.status, ''),
		         s.last_call_et, NVL(DBMS_LOB.SUBSTR(q.sql_fulltext, 3900, 1), ''),
		         NVL(s.event, ''), NVL(TO_CHAR(s.blocking_session), '')
		  FROM v$session s
		  LEFT JOIN v$sql q ON q.sql_id = s.sql_id
		  WHERE s.type = 'USER' AND s.status = 'ACTIVE'
		    AND s.sid <> SYS_CONTEXT('USERENV', 'SID')
		  ORDER BY s.last_call_et DESC
		) WHERE rownum <= :1`, f.EffectiveLimit())
	if err != nil {
		res.MarkSource(dblog.SourceCurrent, label, false, 0, "v$session 조회 권한이 필요합니다")
		return
	}
	defer rows.Close()

	now := time.Now().UTC()
	count := 0
	for rows.Next() {
		var sid int64
		var username, machine, status, queryText, event, blocking string
		var lastCallEt int64
		if err := rows.Scan(&sid, &username, &machine, &status,
			&lastCallEt, &queryText, &event, &blocking); err != nil {
			res.AddNote("실행 중인 쿼리 스캔 실패: %v", err)
			break
		}
		durationMs := float64(lastCallEt) * 1000
		if f.MinDurationMs > 0 && durationMs < f.MinDurationMs {
			continue
		}
		normalized, digest := dblog.NormalizeAndDigest(queryText)

		msg := fmt.Sprintf("%s %ds", status, lastCallEt)
		extra := map[string]string{"sid": fmt.Sprintf("%d", sid)}
		severity := severityForDuration(durationMs)
		if blocking != "" {
			msg += fmt.Sprintf(" (세션 %s에 의해 차단)", blocking)
			extra["blockingSession"] = blocking
			severity = dblog.SeverityWarning
		}
		if event != "" {
			msg += " 대기: " + event
			extra["waitEvent"] = event
		}

		res.Entries = append(res.Entries, dblog.Entry{
			At:       now.Add(-time.Duration(lastCallEt) * time.Second),
			Severity: severity, Source: dblog.SourceCurrent, Message: msg,
			Query: dblog.TruncateQuery(queryText, 4000), Normalized: normalized, Digest: digest,
			DurationMs: durationMs, User: username, Client: machine, Extra: extra,
		})
		count++
	}
	if err := rows.Err(); err != nil {
		res.AddNote("실행 중인 쿼리 순회 실패: %v", err)
	}
	res.MarkSource(dblog.SourceCurrent, label, true, count, "")
}

// oracleAlertLog는 v$diag_alert_ext에서 alert 로그를 읽는다.
func oracleAlertLog(ctx context.Context, db *sql.DB, f *dblog.Filter, res *dblog.Result) {
	label := dblog.Label(dblog.SourceErrorLog)

	rows, err := db.QueryContext(ctx, `
		SELECT * FROM (
		  SELECT originating_timestamp, message_type, message_level, message_text
		  FROM v$diag_alert_ext
		  WHERE originating_timestamp >= :1 AND originating_timestamp <= :2
		  ORDER BY originating_timestamp DESC
		) WHERE rownum <= :3`, f.From, f.To, f.EffectiveLimit())
	if err != nil {
		res.MarkSource(dblog.SourceErrorLog, label, false, 0,
			"v$diag_alert_ext 조회 권한이 필요합니다 (SELECT_CATALOG_ROLE)")
		return
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		var ts time.Time
		var msgType, msgLevel int64
		var text sql.NullString
		if err := rows.Scan(&ts, &msgType, &msgLevel, &text); err != nil {
			res.AddNote("alert 로그 스캔 실패: %v", err)
			break
		}
		severity := oracleAlertSeverity(msgType, msgLevel)
		if f.MinSeverity != "" && severity.Rank() < f.MinSeverity.Rank() {
			continue
		}
		res.Entries = append(res.Entries, dblog.Entry{
			At: ts.UTC(), Severity: severity, Source: dblog.SourceErrorLog,
			Message: dblog.TruncateQuery(strings.TrimSpace(text.String), 2000),
			Extra: map[string]string{
				"messageType":  fmt.Sprintf("%d", msgType),
				"messageLevel": fmt.Sprintf("%d", msgLevel),
			},
		})
		count++
	}
	if err := rows.Err(); err != nil {
		res.AddNote("alert 로그 순회 실패: %v", err)
	}
	res.MarkSource(dblog.SourceErrorLog, label, true, count, "")
}

// oracleAlertSeverity는 Oracle의 message_type/level을 심각도로 변환한다.
//
// message_type: 1=UNKNOWN, 2=INCIDENT_ERROR, 3=ERROR, 4=WARNING, 5=NOTIFICATION, 6=TRACE
// message_level: 1이 가장 심각하고 숫자가 커지면 완화된다.
func oracleAlertSeverity(msgType, msgLevel int64) dblog.Severity {
	switch msgType {
	case 2:
		return dblog.SeverityFatal
	case 3:
		return dblog.SeverityError
	case 4:
		return dblog.SeverityWarning
	case 5:
		return dblog.SeverityInfo
	case 6:
		return dblog.SeverityDebug
	}
	if msgLevel <= 1 {
		return dblog.SeverityError
	}
	return dblog.SeverityInfo
}
