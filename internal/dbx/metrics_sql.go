package dbx

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	"dbstudio/internal/metric"
)

// Metrics는 대상 DB의 상태·부하 지표를 수집한다.
//
// 개별 쿼리 실패는 전체 실패로 만들지 않는다. 모니터링 계정에 일부 시스템 뷰
// 권한이 없는 경우가 흔하고, 그때 "아무것도 못 봄"이 되면 모니터링이 무용해진다.
// 실패한 항목은 Notes에 남겨 사용자가 권한을 보완할 수 있게 한다.
func (a *sqlAdapter) Metrics(ctx context.Context, t Target) (*metric.Set, error) {
	if a.metrics == nil {
		return nil, fmt.Errorf("%w: %s 지표 수집", ErrNotImplemented, a.kind)
	}
	db, err := a.open(t, 2)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	set := metric.NewSet()
	start := time.Now()
	if err := db.PingContext(ctx); err != nil {
		// 접속 자체가 실패하면 up=0만 남기고 반환한다. 이 값이 연결 이벤트의 근거가 된다.
		set.Gauge(metric.NameUp, 0, metric.UnitCount)
		set.AddNote("접속 실패: %v", err)
		set.LatencyMs = float64(time.Since(start).Microseconds()) / 1000
		return set, nil
	}
	set.Gauge(metric.NameUp, 1, metric.UnitCount)
	set.LatencyMs = float64(time.Since(start).Microseconds()) / 1000
	set.Gauge(metric.NameLatency, set.LatencyMs, metric.UnitMillis)

	a.metrics(ctx, db, t, set)
	set.Sort()
	return set, nil
}

// scanKeyValue는 "이름, 값" 두 컬럼을 반환하는 쿼리를 맵으로 만든다.
// SHOW GLOBAL STATUS 처럼 키-값 쌍을 돌려주는 명령에 쓴다.
func scanKeyValue(ctx context.Context, db *sql.DB, query string, args ...any) (map[string]string, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]string{}
	for rows.Next() {
		var k string
		var v sql.NullString
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[strings.ToLower(k)] = v.String
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// numFrom은 맵에서 숫자를 꺼낸다. 없거나 숫자가 아니면 두 번째 값이 false다.
func numFrom(m map[string]string, key string) (float64, bool) {
	v, ok := m[strings.ToLower(key)]
	if !ok {
		return 0, false
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil {
		return 0, false
	}
	return f, true
}

// gaugeFrom은 맵의 값을 게이지로 추가한다. 키가 없으면 아무것도 하지 않는다.
func gaugeFrom(set *metric.Set, m map[string]string, key, name string, unit metric.Unit) bool {
	if v, ok := numFrom(m, key); ok {
		set.Gauge(name, v, unit)
		return true
	}
	return false
}

// counterFrom은 맵의 값을 누적 카운터로 추가한다.
func counterFrom(set *metric.Set, m map[string]string, key, name string) bool {
	if v, ok := numFrom(m, key); ok {
		set.Counter(name, v)
		return true
	}
	return false
}

// queryOneFloat는 단일 숫자를 반환하는 쿼리를 실행한다.
// NULL은 0으로 처리한다 (예: 실행 중인 쿼리가 없을 때의 MAX(duration)).
func queryOneFloat(ctx context.Context, db *sql.DB, query string, args ...any) (float64, error) {
	var v sql.NullFloat64
	if err := db.QueryRowContext(ctx, query, args...).Scan(&v); err != nil {
		return 0, err
	}
	if !v.Valid {
		return 0, nil
	}
	return v.Float64, nil
}

// collectOne은 단일 값 쿼리를 실행해 게이지로 추가하고, 실패는 Notes에 남긴다.
func collectOne(ctx context.Context, db *sql.DB, set *metric.Set, name string, unit metric.Unit, label, query string, args ...any) {
	v, err := queryOneFloat(ctx, db, query, args...)
	if err != nil {
		set.AddNote("%s를 읽지 못했습니다: %v", label, err)
		return
	}
	set.Gauge(name, v, unit)
}

// ---------- MySQL ----------

func metricsMySQL(ctx context.Context, db *sql.DB, t Target, set *metric.Set) {
	status, err := scanKeyValue(ctx, db, `SHOW GLOBAL STATUS`)
	if err != nil {
		set.AddNote("SHOW GLOBAL STATUS 실패 (PROCESS 권한 필요): %v", err)
	} else {
		gaugeFrom(set, status, "Threads_connected", metric.NameConnTotal, metric.UnitCount)
		gaugeFrom(set, status, "Threads_running", metric.NameConnActive, metric.UnitCount)
		gaugeFrom(set, status, "Uptime", metric.NameUptime, metric.UnitSeconds)
		counterFrom(set, status, "Queries", metric.NameQueryRate)
		counterFrom(set, status, "Slow_queries", metric.NameSlowQueryRate)
		counterFrom(set, status, "Com_commit", metric.NameTxnCommitRate)
		counterFrom(set, status, "Com_rollback", metric.NameTxnRollbkRate)
		counterFrom(set, status, "Aborted_connects", metric.NameAbortedConnRate)
		gaugeFrom(set, status, "Innodb_row_lock_current_waits", metric.NameLockWaits, metric.UnitCount)
		gaugeFrom(set, status, "Innodb_row_lock_time_avg", metric.NameLockWaitTime, metric.UnitMillis)

		// InnoDB 버퍼풀 적중률: (읽기요청 - 디스크읽기) / 읽기요청
		reqs, okR := numFrom(status, "Innodb_buffer_pool_read_requests")
		reads, okD := numFrom(status, "Innodb_buffer_pool_reads")
		if okR && okD && reqs > 0 {
			set.Gauge(metric.NameCacheHitRatio, (reqs-reads)/reqs*100, metric.UnitPercent)
		}
	}

	vars, err := scanKeyValue(ctx, db, `SHOW GLOBAL VARIABLES LIKE 'max_connections'`)
	if err != nil {
		set.AddNote("max_connections를 읽지 못했습니다: %v", err)
	} else if maxConn, ok := numFrom(vars, "max_connections"); ok && maxConn > 0 {
		set.Gauge(metric.NameConnMax, maxConn, metric.UnitCount)
		if total, ok := set.Get(metric.NameConnTotal); ok {
			set.Gauge(metric.NameConnUsedPct, total.Value/maxConn*100, metric.UnitPercent)
		}
	}

	// 최장 실행 쿼리. information_schema.PROCESSLIST는 PROCESS 권한이 필요하다.
	collectOne(ctx, db, set, metric.NameLongestQuery, metric.UnitSeconds, "최장 실행 쿼리",
		`SELECT COALESCE(MAX(TIME), 0) FROM information_schema.PROCESSLIST
		 WHERE COMMAND NOT IN ('Sleep', 'Daemon', 'Binlog Dump') AND ID <> CONNECTION_ID()`)

	// 데이터/인덱스 크기
	dbName := strings.TrimSpace(t.Conn.DatabaseName)
	if dbName != "" {
		collectOne(ctx, db, set, metric.NameDataSize, metric.UnitBytes, "데이터 크기",
			`SELECT COALESCE(SUM(DATA_LENGTH), 0) FROM information_schema.TABLES WHERE TABLE_SCHEMA = ?`, dbName)
		collectOne(ctx, db, set, metric.NameIndexSize, metric.UnitBytes, "인덱스 크기",
			`SELECT COALESCE(SUM(INDEX_LENGTH), 0) FROM information_schema.TABLES WHERE TABLE_SCHEMA = ?`, dbName)
	}

	// 복제 지연. 복제 구성이 아니면 행이 없으므로 조용히 넘긴다.
	collectReplicaLagMySQL(ctx, db, set)
}

// collectReplicaLagMySQL은 복제 지연을 읽는다.
// MySQL 8.0.22+는 SHOW REPLICA STATUS, 이전은 SHOW SLAVE STATUS다.
func collectReplicaLagMySQL(ctx context.Context, db *sql.DB, set *metric.Set) {
	for _, q := range []string{`SHOW REPLICA STATUS`, `SHOW SLAVE STATUS`} {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			continue
		}
		cols, err := rows.Columns()
		if err != nil {
			rows.Close()
			continue
		}
		if !rows.Next() {
			// 복제 대상이 아니다. 정상 상황이므로 노트를 남기지 않는다.
			rows.Close()
			return
		}
		values := make([]any, len(cols))
		holders := make([]sql.NullString, len(cols))
		for i := range values {
			values[i] = &holders[i]
		}
		if err := rows.Scan(values...); err != nil {
			rows.Close()
			continue
		}
		rows.Close()

		for i, name := range cols {
			if strings.EqualFold(name, "Seconds_Behind_Master") || strings.EqualFold(name, "Seconds_Behind_Source") {
				if holders[i].Valid {
					if v, err := strconv.ParseFloat(holders[i].String, 64); err == nil {
						set.Gauge(metric.NameReplicaLag, v, metric.UnitSeconds)
					}
				}
				return
			}
		}
		return
	}
}

// ---------- PostgreSQL ----------

func metricsPostgres(ctx context.Context, db *sql.DB, t Target, set *metric.Set) {
	dbName := strings.TrimSpace(t.Conn.DatabaseName)

	// 세션 상태별 집계. pg_stat_activity는 일반 사용자도 자기 세션만 보이므로
	// 전체를 보려면 pg_monitor 역할이 필요하다.
	//
	// FILTER 절은 집계 함수 호출 뒤에 와야 한다: MAX(expr) FILTER (WHERE ...).
	// MAX(expr FILTER (...)) 처럼 인자 안에 넣으면 문법 오류다.
	row := db.QueryRowContext(ctx, `
		SELECT count(*),
		       count(*) FILTER (WHERE state = 'active'),
		       count(*) FILTER (WHERE state = 'idle in transaction'),
		       count(*) FILTER (WHERE wait_event_type = 'Lock'),
		       COALESCE(
		         MAX(EXTRACT(EPOCH FROM (now() - query_start)))
		           FILTER (WHERE state = 'active' AND pid <> pg_backend_pid()),
		         0)
		FROM pg_stat_activity WHERE datname = $1`, dbName)
	var total, active, idleTx, lockWaits, longest float64
	if err := row.Scan(&total, &active, &idleTx, &lockWaits, &longest); err != nil {
		set.AddNote("pg_stat_activity를 읽지 못했습니다 (pg_monitor 역할 필요): %v", err)
	} else {
		set.Gauge(metric.NameConnTotal, total, metric.UnitCount)
		set.Gauge(metric.NameConnActive, active, metric.UnitCount)
		set.Gauge("connections.idle_in_txn", idleTx, metric.UnitCount)
		set.Gauge(metric.NameLockWaits, lockWaits, metric.UnitCount)
		set.Gauge(metric.NameLongestQuery, longest, metric.UnitSeconds)
	}

	if maxConn, err := queryOneFloat(ctx, db, `SELECT setting::float FROM pg_settings WHERE name = 'max_connections'`); err == nil && maxConn > 0 {
		set.Gauge(metric.NameConnMax, maxConn, metric.UnitCount)
		if tot, ok := set.Get(metric.NameConnTotal); ok {
			set.Gauge(metric.NameConnUsedPct, tot.Value/maxConn*100, metric.UnitPercent)
		}
	}

	// 데이터베이스 누적 통계
	drow := db.QueryRowContext(ctx, `
		SELECT COALESCE(xact_commit, 0), COALESCE(xact_rollback, 0),
		       COALESCE(blks_hit, 0), COALESCE(blks_read, 0),
		       COALESCE(deadlocks, 0), COALESCE(tup_returned, 0) + COALESCE(tup_fetched, 0)
		FROM pg_stat_database WHERE datname = $1`, dbName)
	var commits, rollbacks, blksHit, blksRead, deadlocks, tuples float64
	if err := drow.Scan(&commits, &rollbacks, &blksHit, &blksRead, &deadlocks, &tuples); err != nil {
		set.AddNote("pg_stat_database를 읽지 못했습니다: %v", err)
	} else {
		set.Counter(metric.NameTxnCommitRate, commits)
		set.Counter(metric.NameTxnRollbkRate, rollbacks)
		set.Counter(metric.NameDeadlocks, deadlocks)
		// 커밋+롤백을 쿼리 처리량의 대리 지표로 쓴다.
		// PostgreSQL은 pg_stat_statements 없이는 총 쿼리 수를 노출하지 않는다.
		set.Counter(metric.NameQueryRate, commits+rollbacks)
		if blksHit+blksRead > 0 {
			set.Gauge(metric.NameCacheHitRatio, blksHit/(blksHit+blksRead)*100, metric.UnitPercent)
		}
	}

	collectOne(ctx, db, set, metric.NameDataSize, metric.UnitBytes, "데이터베이스 크기",
		`SELECT pg_database_size($1)::float`, dbName)
	collectOne(ctx, db, set, metric.NameUptime, metric.UnitSeconds, "가동 시간",
		`SELECT EXTRACT(EPOCH FROM (now() - pg_postmaster_start_time()))`)

	// 복제 지연: 스탠바이일 때만 의미가 있다.
	var isReplica bool
	if err := db.QueryRowContext(ctx, `SELECT pg_is_in_recovery()`).Scan(&isReplica); err == nil && isReplica {
		collectOne(ctx, db, set, metric.NameReplicaLag, metric.UnitSeconds, "복제 지연",
			`SELECT COALESCE(EXTRACT(EPOCH FROM (now() - pg_last_xact_replay_timestamp())), 0)`)
	}
}

// ---------- MS-SQL ----------

func metricsMSSQL(ctx context.Context, db *sql.DB, t Target, set *metric.Set) {
	// 세션. dm_exec_sessions는 VIEW SERVER STATE 권한이 필요하다.
	row := db.QueryRowContext(ctx, `
		SELECT COUNT(*), SUM(CASE WHEN status = 'running' THEN 1 ELSE 0 END)
		FROM sys.dm_exec_sessions WHERE is_user_process = 1`)
	var total, active sql.NullFloat64
	if err := row.Scan(&total, &active); err != nil {
		set.AddNote("dm_exec_sessions를 읽지 못했습니다 (VIEW SERVER STATE 권한 필요): %v", err)
	} else {
		set.Gauge(metric.NameConnTotal, total.Float64, metric.UnitCount)
		set.Gauge(metric.NameConnActive, active.Float64, metric.UnitCount)
	}

	// 사용자 요청만 센다. session_id > 50 필터는 최신 SQL Server에서 불충분하다 —
	// 시스템 태스크도 50을 넘는 세션 ID를 가질 수 있고, 그것들은 수 시간씩 실행되므로
	// 그대로 두면 "최장 실행 쿼리 1.9시간" 같은 오탐이 계속 발생한다.
	// dm_exec_sessions.is_user_process가 정확한 판별 기준이다.
	collectOne(ctx, db, set, metric.NameLongestQuery, metric.UnitSeconds, "최장 실행 쿼리",
		`SELECT COALESCE(MAX(r.total_elapsed_time) / 1000.0, 0)
		 FROM sys.dm_exec_requests r
		 JOIN sys.dm_exec_sessions s ON s.session_id = r.session_id
		 WHERE s.is_user_process = 1 AND r.session_id <> @@SPID`)
	collectOne(ctx, db, set, metric.NameLockWaits, metric.UnitCount, "락 대기",
		`SELECT COUNT(*) FROM sys.dm_exec_requests WHERE blocking_session_id <> 0`)
	collectOne(ctx, db, set, metric.NameUptime, metric.UnitSeconds, "가동 시간",
		`SELECT DATEDIFF(SECOND, sqlserver_start_time, GETDATE()) FROM sys.dm_os_sys_info`)

	// 성능 카운터. cntr_type 272696576은 누적 카운터, 65792는 순간값이다.
	counters, err := scanKeyValue(ctx, db, `
		SELECT RTRIM(counter_name), CAST(cntr_value AS VARCHAR(32))
		FROM sys.dm_os_performance_counters
		WHERE RTRIM(counter_name) IN (
			'Batch Requests/sec', 'Transactions/sec', 'Buffer cache hit ratio',
			'Buffer cache hit ratio base', 'Page life expectancy',
			'Number of Deadlocks/sec', 'Lock Waits/sec')
		  AND (instance_name = '' OR instance_name = '_Total')`)
	if err != nil {
		set.AddNote("성능 카운터를 읽지 못했습니다: %v", err)
	} else {
		counterFrom(set, counters, "Batch Requests/sec", metric.NameQueryRate)
		counterFrom(set, counters, "Transactions/sec", metric.NameTxnCommitRate)
		counterFrom(set, counters, "Number of Deadlocks/sec", metric.NameDeadlocks)
		gaugeFrom(set, counters, "Page life expectancy", "buffer.page_life_expectancy", metric.UnitSeconds)

		// 버퍼 캐시 적중률은 값과 base의 비율로 계산해야 한다.
		hit, okH := numFrom(counters, "Buffer cache hit ratio")
		base, okB := numFrom(counters, "Buffer cache hit ratio base")
		if okH && okB && base > 0 {
			set.Gauge(metric.NameCacheHitRatio, hit/base*100, metric.UnitPercent)
		}
	}

	if dbName := strings.TrimSpace(t.Conn.DatabaseName); dbName != "" {
		collectOne(ctx, db, set, metric.NameDataSize, metric.UnitBytes, "데이터 크기",
			`SELECT COALESCE(SUM(CAST(size AS BIGINT)) * 8192, 0)
			 FROM sys.master_files WHERE database_id = DB_ID(@p1) AND type_desc = 'ROWS'`, dbName)
	}
}

// ---------- SQLite ----------

// metricsSQLite는 파일 기반 DB에 맞는 지표를 수집한다.
// SQLite에는 서버가 없으므로 세션/처리량 개념이 없고, 대신 파일 크기와
// 단편화(freelist), WAL 상태가 실질적인 운영 지표다.
func metricsSQLite(ctx context.Context, db *sql.DB, t Target, set *metric.Set) {
	pageCount, errPC := queryOneFloat(ctx, db, `PRAGMA page_count`)
	pageSize, errPS := queryOneFloat(ctx, db, `PRAGMA page_size`)
	if errPC == nil && errPS == nil {
		set.Gauge(metric.NameDataSize, pageCount*pageSize, metric.UnitBytes)
		set.Gauge("sqlite.page_count", pageCount, metric.UnitCount)
	} else {
		set.AddNote("페이지 정보를 읽지 못했습니다")
	}

	if freelist, err := queryOneFloat(ctx, db, `PRAGMA freelist_count`); err == nil {
		set.Gauge("sqlite.freelist_pages", freelist, metric.UnitCount)
		// 미사용 페이지 비율이 높으면 VACUUM이 필요하다.
		if pageCount > 0 {
			set.Gauge("sqlite.fragmentation_pct", freelist/pageCount*100, metric.UnitPercent)
		}
	}

	collectOne(ctx, db, set, "tables.count", metric.UnitCount, "테이블 수",
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`)
	collectOne(ctx, db, set, "indexes.count", metric.UnitCount, "인덱스 수",
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index'`)

	// WAL 모드에서만 wal_checkpoint가 의미를 가진다.
	var journalMode string
	if err := db.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&journalMode); err == nil {
		if strings.EqualFold(journalMode, "wal") {
			set.Gauge("sqlite.wal_mode", 1, metric.UnitCount)
		} else {
			set.Gauge("sqlite.wal_mode", 0, metric.UnitCount)
		}
	}
}

// ---------- Oracle ----------

func metricsOracle(ctx context.Context, db *sql.DB, t Target, set *metric.Set) {
	// v$ 뷰는 SELECT_CATALOG_ROLE 또는 개별 권한이 필요하다.
	collectOne(ctx, db, set, metric.NameConnTotal, metric.UnitCount, "세션 수",
		`SELECT COUNT(*) FROM v$session WHERE type = 'USER'`)
	collectOne(ctx, db, set, metric.NameConnActive, metric.UnitCount, "활성 세션",
		`SELECT COUNT(*) FROM v$session WHERE type = 'USER' AND status = 'ACTIVE'`)
	collectOne(ctx, db, set, metric.NameConnMax, metric.UnitCount, "세션 상한",
		`SELECT TO_NUMBER(value) FROM v$parameter WHERE name = 'sessions'`)
	collectOne(ctx, db, set, metric.NameLongestQuery, metric.UnitSeconds, "최장 실행 쿼리",
		`SELECT COALESCE(MAX(last_call_et), 0) FROM v$session
		 WHERE type = 'USER' AND status = 'ACTIVE' AND sid <> SYS_CONTEXT('USERENV', 'SID')`)
	collectOne(ctx, db, set, metric.NameLockWaits, metric.UnitCount, "락 대기",
		`SELECT COUNT(*) FROM v$session WHERE blocking_session IS NOT NULL`)
	collectOne(ctx, db, set, metric.NameUptime, metric.UnitSeconds, "가동 시간",
		`SELECT (SYSDATE - startup_time) * 86400 FROM v$instance`)

	if total, okT := set.Get(metric.NameConnTotal); okT {
		if maxConn, okM := set.Get(metric.NameConnMax); okM && maxConn.Value > 0 {
			set.Gauge(metric.NameConnUsedPct, total.Value/maxConn.Value*100, metric.UnitPercent)
		}
	}

	// 누적 통계
	stats, err := scanKeyValue(ctx, db, `
		SELECT name, TO_CHAR(value) FROM v$sysstat
		WHERE name IN ('execute count', 'user commits', 'user rollbacks', 'parse count (total)')`)
	if err != nil {
		set.AddNote("v$sysstat를 읽지 못했습니다 (SELECT_CATALOG_ROLE 필요): %v", err)
	} else {
		counterFrom(set, stats, "execute count", metric.NameQueryRate)
		counterFrom(set, stats, "user commits", metric.NameTxnCommitRate)
		counterFrom(set, stats, "user rollbacks", metric.NameTxnRollbkRate)
	}

	// 버퍼 캐시 적중률은 v$sysmetric이 직접 제공한다.
	collectOne(ctx, db, set, metric.NameCacheHitRatio, metric.UnitPercent, "버퍼 캐시 적중률",
		`SELECT COALESCE(value, 0) FROM v$sysmetric
		 WHERE metric_name = 'Buffer Cache Hit Ratio' AND group_id = 2 AND rownum = 1`)

	// 사용자 세그먼트 크기
	collectOne(ctx, db, set, metric.NameDataSize, metric.UnitBytes, "세그먼트 크기",
		`SELECT COALESCE(SUM(bytes), 0) FROM user_segments`)
}
