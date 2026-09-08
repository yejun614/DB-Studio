package dbx

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"dbstudio/internal/dblog"
	"dbstudio/internal/metric"
	"dbstudio/internal/schema"
)

// ClickHouse.
//
// 다른 관계형 어댑터와 같은 자리(sqlAdapter)에 두는 이유: SQL 을 말하고 스키마가
// 있고 database/sql 드라이버가 있다. 열 지향이라는 것은 **성질**이지 다른 종류가
// 아니다 — 따로 두면 스키마·데이터·SQL 콘솔·백업·드라이런을 한 벌 더 만들어야 하고,
// 사용자는 같은 일을 화면마다 다시 배워야 한다.
//
// 갈리는 것은 셋이고 그것은 여기와 schema/clickhouse.go 에 모여 있다:
// 널 허용이 타입 안에 있다, 정렬 키가 곧 구조다, 외래키가 없다.

// clickhouseDSN: clickhouse://user:pass@host:port/db?params
func clickhouseDSN(t Target) (string, error) {
	c := t.Conn
	u := &url.URL{
		Scheme: "clickhouse",
		Host:   fmt.Sprintf("%s:%d", c.Host, port(c, 9000)),
		Path:   "/" + c.DatabaseName,
	}
	if user := t.Username(); user != "" {
		if pw := t.Password(); pw != "" {
			u.User = url.UserPassword(user, pw)
		} else {
			u.User = url.User(user)
		}
	}
	q := url.Values{}
	q.Set("dial_timeout", "10s")
	q.Set("read_timeout", "60s")
	// 압축을 켠다. ClickHouse 는 한 번에 돌려주는 양이 다른 DB 보다 훨씬 큰 일이
	// 흔해서(열 지향 집계), 네트워크가 병목이 되는 쪽이 먼저 온다.
	q.Set("compress", "lz4")
	if secure := strings.TrimSpace(t.Opt("secure", "")); secure != "" {
		if b, err := strconv.ParseBool(secure); err == nil && b {
			q.Set("secure", "true")
		}
	}
	if tz := strings.TrimSpace(t.Opt("timezone", "")); tz != "" {
		q.Set("session_timezone", tz)
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// introspectClickHouse는 system.tables·system.columns 에서 구조를 읽는다.
//
// information_schema 가 있기는 하지만 쓰지 않는다. 그쪽은 호환을 위한 뷰라서
// ClickHouse 고유의 것 — 엔진, 정렬 키, 파티션 키, 압축 후 크기 — 이 빠져 있고,
// 그 넷이 이 DB 에서 표를 이해하는 데 가장 중요한 값이다.
// clickhouseTableTTL은 CREATE 문에서 표의 TTL 절을 꺼낸다.
//
// 왜 이렇게까지 하는가: system.tables 에는 정렬 키도 파티션도 컬럼으로 있는데
// **TTL 만 없다.** 그래서 CREATE 문을 다시 읽는 수밖에 없다.
//
// 안 하면 조용히 사라진다. 보관 90일이 걸린 표를 ERD 로 가져와 다시 만들면
// TTL 없는 표가 되고, 그 사이 아무 경고도 나지 않는다. 데이터가 영원히
// 쌓이기 시작하는데 화면에는 "같은 표"라고 적혀 있는 셈이다.
//
// 줄 단위로 자르면 안 된다. SHOW CREATE 는 여러 줄로 보여 주지만 드라이버가
// 돌려주는 값은 **한 줄**이다 — 그 차이 때문에 한 번 틀렸다. 괄호 깊이와
// 따옴표를 세면서 맨 바깥 낱말만 본다.
func clickhouseTableTTL(createSQL string) string {
	heads := topLevelWords(createSQL)

	// 컬럼 목록 안(깊이 1)의 TTL 은 컬럼에 걸린 것이지 표의 것이 아니다.
	// 맨 바깥만 보므로 애초에 걸리지 않지만, ENGINE 뒤부터 찾아 한 겹 더 둔다.
	from := 0
	for i, h := range heads {
		if h.word == "ENGINE" {
			from = i + 1
			break
		}
	}
	for i := from; i < len(heads); i++ {
		if heads[i].word != "TTL" {
			continue
		}
		start := heads[i].end
		end := len(createSQL)
		for j := i + 1; j < len(heads); j++ {
			if clickhouseClauseHeads[heads[j].word] {
				end = heads[j].start
				break
			}
		}
		return strings.TrimSpace(createSQL[start:end])
	}
	return ""
}

// clickhouseClauseHeads는 TTL 절을 끝내는 낱말들이다.
//
// TO·DELETE·GROUP·WHERE 는 넣지 않는다. 그것들은 TTL 절 **안에** 올 수 있어서
// (TTL d + INTERVAL 1 DAY TO VOLUME 'cold'), 끝으로 보면 절이 잘린다.
var clickhouseClauseHeads = map[string]bool{
	"SETTINGS": true, "COMMENT": true, "SAMPLE": true,
	"PRIMARY": true, "ORDER": true, "PARTITION": true, "AS": true, "ENGINE": true,
}

// topLevelWord는 괄호·따옴표 밖에 있는 낱말 하나다.
type topLevelWord struct {
	word       string
	start, end int
}

// topLevelWords는 맨 바깥 깊이의 낱말만 훑는다.
//
// 괄호 안(컬럼 목록, 함수 인자)과 따옴표 안(주석 문구), 백틱 안(식별자)은 건너뛴다.
// 주석에 "SETTINGS" 같은 낱말이 들어 있어도 절이 끊기지 않게 하려는 것이다.
func topLevelWords(s string) []topLevelWord {
	isWordByte := func(c byte) bool {
		return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_'
	}
	var out []topLevelWord
	depth, i := 0, 0
	for i < len(s) {
		c := s[i]
		switch {
		case c == '(':
			depth++
			i++
		case c == ')':
			if depth > 0 {
				depth--
			}
			i++
		case c == '\'' || c == '`':
			quote := c
			i++
			for i < len(s) {
				if s[i] == '\\' && quote == '\'' {
					i += 2
					continue
				}
				if s[i] == quote {
					i++
					break
				}
				i++
			}
		case depth == 0 && isWordByte(c) && !(c >= '0' && c <= '9'):
			start := i
			for i < len(s) && isWordByte(s[i]) {
				i++
			}
			out = append(out, topLevelWord{
				word: strings.ToUpper(s[start:i]), start: start, end: i,
			})
		default:
			i++
		}
	}
	return out
}

func introspectClickHouse(ctx context.Context, db *sql.DB, t Target, s *schema.Schema) error {
	dbName := strings.TrimSpace(t.Conn.DatabaseName)
	if dbName == "" {
		if err := db.QueryRowContext(ctx, `SELECT currentDatabase()`).Scan(&dbName); err != nil {
			return fmt.Errorf("현재 데이터베이스를 확인할 수 없습니다: %w", err)
		}
		s.Name = dbName
	}

	tables := map[string]*schema.Table{}
	rows, err := db.QueryContext(ctx, `
		SELECT name, engine, sorting_key, partition_key, primary_key, comment, total_rows, total_bytes,
		       create_table_query
		FROM system.tables
		WHERE database = ? AND NOT is_temporary AND engine NOT LIKE '%View'`, dbName)
	if err != nil {
		return fmt.Errorf("테이블 목록 조회 실패: %w", err)
	}
	for rows.Next() {
		var name, engine, sortKey, partKey, pk, comment, createSQL string
		// total_rows·total_bytes 는 엔진에 따라 NULL 이다(Log·Memory 등).
		var totalRows, totalBytes sql.NullInt64
		if err := rows.Scan(&name, &engine, &sortKey, &partKey, &pk, &comment,
			&totalRows, &totalBytes, &createSQL); err != nil {
			rows.Close()
			return fmt.Errorf("테이블 정보 스캔 실패: %w", err)
		}
		tbl := &schema.Table{
			Name: name, Comment: comment,
			Columns: []*schema.Column{}, Indexes: []*schema.Index{},
			ForeignKeys: []*schema.ForeignKey{}, Checks: []*schema.Check{},
			Options:     map[string]string{},
			RowEstimate: totalRows.Int64, SizeBytes: totalBytes.Int64,
		}
		if engine != "" {
			tbl.Options["engine"] = engine
		}
		if sortKey != "" {
			tbl.Options["order_by"] = sortKey
		}
		if partKey != "" {
			tbl.Options["partition_by"] = partKey
		}
		if ttl := clickhouseTableTTL(createSQL); ttl != "" {
			tbl.Options["ttl"] = ttl
		}
		// 정렬 키를 기본키로도 둔다. ClickHouse 의 기본키는 유일성을 강제하지
		// 않지만, "이 표를 무엇으로 찾는가"라는 뜻은 다른 DB 의 기본키와 같다.
		// 이렇게 두면 ERD 가 그것을 열쇠로 그리고, 그 그림이 실제 쓰임에 맞는다.
		if key := firstNonEmpty(pk, sortKey); key != "" {
			cols := splitKeyExpr(key)
			if len(cols) > 0 {
				tbl.PrimaryKey = &schema.PrimaryKey{Columns: cols}
			}
		}
		tables[name] = tbl
		s.Tables = append(s.Tables, tbl)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	if err := clickhouseColumns(ctx, db, dbName, tables); err != nil {
		s.Notes = append(s.Notes, err.Error())
	}
	if err := clickhouseViews(ctx, db, dbName, s); err != nil {
		s.Notes = append(s.Notes, err.Error())
	}
	// 데이터 스킵 인덱스는 일반 인덱스와 뜻이 다르다(값을 찾는 것이 아니라 읽지
	// 않아도 될 블록을 건너뛴다). 그래도 목록에 보이는 편이 낫다 — 없으면
	// "인덱스가 하나도 없는 표"로 보이고, 그것은 사실이 아니다.
	if err := clickhouseSkipIndexes(ctx, db, dbName, tables); err != nil {
		s.Notes = append(s.Notes, err.Error())
	}
	return nil
}

func clickhouseColumns(ctx context.Context, db *sql.DB, dbName string, tables map[string]*schema.Table) error {
	rows, err := db.QueryContext(ctx, `
		SELECT table, name, type, position, default_kind, default_expression, comment
		FROM system.columns
		WHERE database = ?
		ORDER BY table, position`, dbName)
	if err != nil {
		return fmt.Errorf("컬럼 목록을 읽지 못했습니다: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var table, name, rawType, defaultKind, defaultExpr, comment string
		var position uint64
		if err := rows.Scan(&table, &name, &rawType, &position,
			&defaultKind, &defaultExpr, &comment); err != nil {
			return fmt.Errorf("컬럼 정보를 읽지 못했습니다: %w", err)
		}
		tbl := tables[table]
		if tbl == nil {
			continue
		}
		_, nullable := schema.UnwrapClickHouseType(rawType)
		col := &schema.Column{
			Name: name, Position: int(position), RawType: rawType,
			Type: schema.ParseType("clickhouse", rawType),
			// 널 허용은 타입이 말한다. Nullable 로 감싸지 않은 컬럼은 널을
			// 담을 수 없다 — 다른 DB 와 달리 이것이 기본이다.
			Nullable: nullable,
			Comment:  comment,
		}
		switch strings.ToUpper(defaultKind) {
		case "DEFAULT":
			col.HasDefault, col.Default = true, defaultExpr
		case "MATERIALIZED", "ALIAS":
			// 계산해서 얻는 컬럼이다. 기본값으로 두면 INSERT 에 그 식이 나가는데,
			// 그것은 서버가 거절한다.
			col.Generated = defaultExpr
		}
		tbl.Columns = append(tbl.Columns, col)
	}
	return rows.Err()
}

// clickhouseViewTarget은 구체화 뷰가 결과를 써 넣는 표(TO 절)를 꺼낸다.
//
// system.tables 에 이것을 담은 컬럼이 없어서 CREATE 문에서 읽는다. TO 가 없는
// 구체화 뷰는 결과를 자기 안에 담으므로 빈 문자열을 돌려준다.
func clickhouseViewTarget(createSQL string) string {
	words := topLevelWords(createSQL)
	seenView := false
	for i, w := range words {
		switch {
		case w.word == "VIEW":
			seenView = true
			continue
		case !seenView:
			continue
		case w.word == "AS":
			// TO 없이 AS 까지 왔다. 대상 표가 없다는 뜻이다.
			return ""
		case w.word != "TO":
			continue
		}
		rest := createSQL[w.end:]
		end := len(rest)
		// 표 이름 다음에는 컬럼 목록이나 AS 가 온다. 먼저 오는 쪽에서 끊는다.
		if p := strings.IndexByte(rest, '('); p >= 0 && p < end {
			end = p
		}
		for _, nx := range words[i+1:] {
			if nx.word == "AS" {
				if q := nx.start - w.end; q >= 0 && q < end {
					end = q
				}
				break
			}
		}
		return strings.ReplaceAll(strings.TrimSpace(rest[:end]), "`", "")
	}
	return ""
}

func clickhouseViews(ctx context.Context, db *sql.DB, dbName string, s *schema.Schema) error {
	rows, err := db.QueryContext(ctx, `
		SELECT name, as_select, engine, comment, create_table_query
		FROM system.tables
		WHERE database = ? AND engine LIKE '%View'
		ORDER BY name`, dbName)
	if err != nil {
		return fmt.Errorf("뷰 목록을 읽지 못했습니다: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name, definition, engine, comment, createSQL string
		if err := rows.Scan(&name, &definition, &engine, &comment, &createSQL); err != nil {
			return fmt.Errorf("뷰 정보를 읽지 못했습니다: %w", err)
		}
		materialized := strings.EqualFold(engine, "MaterializedView")
		if materialized && comment == "" {
			// 구체화 뷰는 실제로 표를 하나 더 만든다. 그 사실을 적어 두지 않으면
			// "뷰라서 공간을 안 쓴다"는 오해가 그대로 남는다.
			comment = "구체화 뷰 — 결과를 실제로 저장합니다"
		}
		v := &schema.View{
			Name: name, Definition: definition, Comment: comment,
			Materialized: materialized,
		}
		if materialized {
			// 안내 문구로만 적어 두면 DDL 을 만들 때는 사라진다. 그러면
			// 평범한 뷰가 만들어지고 대상 표에 행이 쌓이지 않기 시작한다.
			v.Target = clickhouseViewTarget(createSQL)
		}
		s.Views = append(s.Views, v)
	}
	return rows.Err()
}

func clickhouseSkipIndexes(ctx context.Context, db *sql.DB, dbName string, tables map[string]*schema.Table) error {
	rows, err := db.QueryContext(ctx, `
		SELECT table, name, expr, type, granularity
		FROM system.data_skipping_indices
		WHERE database = ?
		ORDER BY table, name`, dbName)
	if err != nil {
		// 오래된 서버에는 이 표가 없다. 없는 것이 오류는 아니다.
		return fmt.Errorf("데이터 스킵 인덱스를 읽지 못했습니다(오래된 서버일 수 있습니다): %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var table, name, expr, typ string
		var granularity uint64
		if err := rows.Scan(&table, &name, &expr, &typ, &granularity); err != nil {
			return err
		}
		tbl := tables[table]
		if tbl == nil {
			continue
		}
		parts := make([]schema.IndexPart, 0, 1)
		for _, e := range splitKeyExpr(expr) {
			parts = append(parts, schema.IndexPart{Expression: e})
		}
		tbl.Indexes = append(tbl.Indexes, &schema.Index{
			Name:    name,
			Type:    fmt.Sprintf("%s (스킵, GRANULARITY %d)", typ, granularity),
			Columns: parts,
		})
	}
	return rows.Err()
}

// splitKeyExpr는 정렬 키·인덱스 식에서 컬럼 이름을 뽑는다.
//
// 식일 수도 있다(toYYYYMM(created_at)). 그때는 식 전체를 하나로 둔다 — 안쪽의
// 컬럼만 꺼내면 화면이 "created_at 으로 정렬한다"고 말하게 되는데, 실제로는
// 달(month) 단위로 정렬한다. 다른 뜻이다.
func splitKeyExpr(expr string) []string {
	expr = strings.TrimSpace(expr)
	expr = strings.TrimPrefix(expr, "(")
	expr = strings.TrimSuffix(expr, ")")
	if expr == "" || expr == "tuple()" {
		return nil
	}
	out := []string{}
	depth, start := 0, 0
	for i, r := range expr {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				if v := strings.TrimSpace(expr[start:i]); v != "" {
					out = append(out, v)
				}
				start = i + 1
			}
		}
	}
	if v := strings.TrimSpace(expr[start:]); v != "" {
		out = append(out, v)
	}
	return out
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// metricsClickHouse는 상태·부하 지표를 모은다.
//
// system.metrics 는 지금 값, system.asynchronous_metrics 는 주기적으로 갱신되는
// 값이다. 둘을 섞어 읽되 **누적값(system.events)은 쓰지 않는다** — 시작 후 총합을
// 지금 값처럼 그리면 그래프가 영원히 오르기만 하고 아무 말도 하지 않는다.
func metricsClickHouse(ctx context.Context, db *sql.DB, t Target, set *metric.Set) {
	now := scanClickHouseMetrics(ctx, db, `SELECT metric, value FROM system.metrics`, set)
	async := scanClickHouseMetrics(ctx, db,
		`SELECT metric, value FROM system.asynchronous_metrics`, set)

	// 어느 표에 있든 찾는다.
	//
	// 두 표의 경계는 생각보다 흐리고 버전마다 옮겨 다닌다. 24.8 에서 재 보면
	// MemoryTracking 은 system.metrics 에, MaxPartCountForPartition 과
	// Uptime 은 system.asynchronous_metrics 에 있다 — 이름만 보고 짐작하면 틀린다.
	//
	// 실제로 틀렸었다. 파트 수를 system.metrics 에서 찾게 해 뒀는데 거기엔 없어서,
	// ClickHouse 의 대표 지표가 조용히 빠진 채로 그래프가 그려졌다. 없는 이름은
	// 그냥 없는 것으로 지나가므로 오류 하나 나지 않는다.
	find := func(name string) (float64, bool) {
		if v, ok := now[name]; ok {
			return v, true
		}
		v, ok := async[name]
		return v, ok
	}

	if v, ok := now["Query"]; ok {
		set.Gauge(metric.NameConnActive, v, metric.UnitCount)
	}
	// 접속 수는 프로토콜마다 따로 센다. 화면이 보는 것은 "몇 개가 붙어 있는가"라
	// 합쳐서 하나로 낸다.
	if _, ok := now["TCPConnection"]; ok {
		set.Gauge(metric.NameConnTotal,
			now["TCPConnection"]+now["HTTPConnection"]+now["MySQLConnection"], metric.UnitCount)
	}
	if v, ok := find("MaxPartCountForPartition"); ok {
		// 파티션 하나의 파트 수. 이 값이 300 근처로 오르면 병합이 쓰기를 따라오지
		// 못한다는 뜻이고, 그대로 두면 INSERT 가 거절되기 시작한다.
		set.Gauge(metric.NameClickHouseMaxParts, v, metric.UnitCount)
	}
	if v, ok := find("ReplicasMaxAbsoluteDelay"); ok {
		set.Gauge(metric.NameReplicaLag, v, metric.UnitSeconds)
	}
	if v, ok := find("Uptime"); ok {
		set.Gauge(metric.NameUptime, v, metric.UnitSeconds)
	}
	if v, ok := find("MemoryTracking"); ok {
		set.Gauge(metric.NameMemoryUsed, v, metric.UnitBytes)
	}

	var onDisk, uncompressed sql.NullFloat64
	if err := db.QueryRowContext(ctx, `
		SELECT sum(bytes_on_disk), sum(data_uncompressed_bytes)
		FROM system.parts WHERE active`).Scan(&onDisk, &uncompressed); err == nil {
		if onDisk.Valid {
			// 압축된 뒤의 크기다. 열 지향에서 이 값과 원본은 열 배 넘게 차이가
			// 나기도 하므로, 다른 DB 의 "데이터 크기"와 나란히 볼 때 주의가 필요하다.
			set.Gauge(metric.NameDataSize, onDisk.Float64, metric.UnitBytes)
		}
		if onDisk.Valid && uncompressed.Valid && onDisk.Float64 > 0 {
			set.Gauge(metric.NameClickHouseCompression,
				uncompressed.Float64/onDisk.Float64, metric.UnitCount)
		}
	}
}

func scanClickHouseMetrics(ctx context.Context, db *sql.DB, query string, set *metric.Set) map[string]float64 {
	out := map[string]float64{}
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		set.Notes = append(set.Notes, "지표를 읽지 못했습니다: "+err.Error())
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var v float64
		if rows.Scan(&name, &v) == nil {
			out[name] = v
		}
	}
	return out
}

// logsClickHouse는 system.query_log 에서 최근 쿼리와 통계를 읽는다.
//
// query_log 는 기본으로 켜져 있지만 끄는 설정이 있다. 없을 때 조용히 빈 화면을
// 보여주지 않고 "왜 볼 수 없는지"를 말하는 것은 다른 어댑터와 같은 규칙이다.
func logsClickHouse(ctx context.Context, db *sql.DB, t Target, f *dblog.Filter, res *dblog.Result) {
	if f.WantsSource(dblog.SourceSlowQuery) {
		clickhouseQueryLog(ctx, db, f, res)
	}
	if f.WantsSource(dblog.SourceStatements) {
		clickhouseQueryStats(ctx, db, f, res)
	}
	if f.WantsSource(dblog.SourceCurrent) {
		clickhouseRunningQueries(ctx, db, f, res)
	}
}

func clickhouseQueryLog(ctx context.Context, db *sql.DB, f *dblog.Filter, res *dblog.Result) {
	label := dblog.Label(dblog.SourceSlowQuery)
	rows, err := db.QueryContext(ctx, `
		SELECT event_time, type, query_duration_ms, query, user, current_database,
		       read_rows, result_rows, memory_usage, exception
		FROM system.query_log
		WHERE type != 'QueryStart'
		  AND event_time >= ? AND event_time <= ?
		  AND query_duration_ms >= ?
		ORDER BY event_time DESC
		LIMIT ?`, f.From, f.To, f.MinDurationMs, f.EffectiveLimit())
	if err != nil {
		res.MarkSource(dblog.SourceSlowQuery, label, false, 0,
			"system.query_log 를 읽지 못했습니다. 서버 설정에서 query_log 가 꺼져 있거나 "+
				"이 계정에 권한이 없습니다")
		res.AddNote("system.query_log 조회 실패: %v", err)
		return
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		var at time.Time
		var kind, query, user, database, exception string
		var durationMS, readRows, resultRows, memory uint64
		if err := rows.Scan(&at, &kind, &durationMS, &query, &user, &database,
			&readRows, &resultRows, &memory, &exception); err != nil {
			continue
		}
		entry := dblog.Entry{
			At: at, Severity: dblog.SeverityInfo, Source: dblog.SourceSlowQuery,
			Message: dblog.TruncateQuery(query, 400), Query: query,
			DurationMs: float64(durationMS), User: user, Database: database,
			RowsExamined: int64(readRows), RowsSent: int64(resultRows),
			Extra: map[string]string{
				"메모리": strconv.FormatUint(memory, 10),
				"종류":  kind,
			},
		}
		if exception != "" {
			// 실패한 쿼리는 로그에서 가장 먼저 찾는 것이다. 사유를 메시지에 함께
			// 넣어야 목록만 보고도 무엇이 깨졌는지 안다.
			entry.Severity = dblog.SeverityError
			entry.Message = dblog.TruncateQuery(query, 300) + " — " + exception
		}
		res.Entries = append(res.Entries, entry)
		count++
	}
	res.MarkSource(dblog.SourceSlowQuery, label, true, count, "")
}

func clickhouseQueryStats(ctx context.Context, db *sql.DB, f *dblog.Filter, res *dblog.Result) {
	label := dblog.Label(dblog.SourceStatements)
	// normalized_query_hash 는 리터럴을 지운 뒤의 해시다. ClickHouse 가 이미
	// 정규화해 주므로 우리가 다시 정규화하지 않는다.
	rows, err := db.QueryContext(ctx, `
		SELECT normalized_query_hash, any(query), count(),
		       sum(query_duration_ms), avg(query_duration_ms),
		       max(query_duration_ms), min(query_duration_ms),
		       sum(read_rows), min(event_time), max(event_time)
		FROM system.query_log
		WHERE type = 'QueryFinish' AND event_time >= ? AND event_time <= ?
		GROUP BY normalized_query_hash
		ORDER BY sum(query_duration_ms) DESC
		LIMIT 100`, f.From, f.To)
	if err != nil {
		res.MarkSource(dblog.SourceStatements, label, false, 0,
			"system.query_log 를 읽지 못해 쿼리 통계를 낼 수 없습니다")
		return
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		var hash uint64
		var sample string
		var calls, readRows uint64
		var total, mean, maxMS, minMS float64
		var first, last time.Time
		if err := rows.Scan(&hash, &sample, &calls, &total, &mean, &maxMS, &minMS,
			&readRows, &first, &last); err != nil {
			continue
		}
		firstAt, lastAt := first, last
		stat := dblog.QueryStat{
			Digest:     strconv.FormatUint(hash, 10),
			Normalized: dblog.TruncateQuery(sample, 400),
			Sample:     sample,
			Calls:      int64(calls),
			TotalMs:    total, MeanMs: mean, MaxMs: maxMS, MinMs: minMS,
			RowsTotal: int64(readRows),
			FirstSeen: &firstAt, LastSeen: &lastAt,
		}
		if calls > 0 {
			stat.RowsPerCall = float64(readRows) / float64(calls)
		}
		res.Stats = append(res.Stats, stat)
		count++
	}
	res.MarkSource(dblog.SourceStatements, label, true, count, "")
}

func clickhouseRunningQueries(ctx context.Context, db *sql.DB, f *dblog.Filter, res *dblog.Result) {
	label := dblog.Label(dblog.SourceCurrent)
	// system.processes 에는 시작 시각 컬럼이 없다(그것은 query_log 의 것이다).
	// 지금에서 흐른 시간을 빼서 만든다. 주소는 IPv6 타입이라 문자열로 바꿔 받는다 —
	// 드라이버가 그대로는 못 읽는다.
	rows, err := db.QueryContext(ctx, `
		SELECT now64(3) - toIntervalMillisecond(toUInt64(elapsed * 1000)) AS started,
		       elapsed * 1000 AS ms, query, user, toString(address) AS addr,
		       read_rows, memory_usage
		FROM system.processes
		ORDER BY elapsed DESC
		LIMIT 100`)
	if err != nil {
		// 사유를 지어내지 않는다. 예전에는 무조건 "권한 부족"이라고 적었는데,
		// 정작 이 자리에서 났던 것은 없는 컬럼을 물은 SQL 오류였다 — 그 안내를
		// 믿고 권한을 아무리 고쳐도 달라지지 않는다.
		res.MarkSource(dblog.SourceCurrent, label, false, 0,
			"system.processes 를 읽지 못했습니다: "+err.Error())
		return
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		var at time.Time
		var elapsedMS float64
		var query, user, addr string
		var readRows, memory uint64
		if err := rows.Scan(&at, &elapsedMS, &query, &user, &addr, &readRows, &memory); err != nil {
			continue
		}
		if elapsedMS < f.MinDurationMs {
			continue
		}
		res.Entries = append(res.Entries, dblog.Entry{
			At: at, Severity: dblog.SeverityInfo, Source: dblog.SourceCurrent,
			Message: dblog.TruncateQuery(query, 400), Query: query,
			DurationMs: elapsedMS, User: user, Client: addr,
			RowsExamined: int64(readRows),
			Extra:        map[string]string{"메모리": strconv.FormatUint(memory, 10)},
		})
		count++
	}
	res.MarkSource(dblog.SourceCurrent, label, true, count, "")
}
