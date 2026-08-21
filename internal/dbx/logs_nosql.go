package dbx

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readpref"

	"dbstudio/internal/dblog"
)

// ---------- MongoDB ----------

// Logs는 MongoDB의 프로파일러 기록과 서버 로그를 읽는다.
//
// system.profile은 프로파일링이 켜져 있어야 존재한다. 꺼져 있으면
// 활성화 방법을 안내한다 — 로그가 비어 있는 것과 기능이 꺼진 것은 다른 상황이다.
func (a *mongoAdapter) Logs(ctx context.Context, t Target, f *dblog.Filter) (*dblog.Result, error) {
	uri, err := a.uri(t)
	if err != nil {
		return nil, err
	}
	dbName := strings.TrimSpace(t.Conn.DatabaseName)
	if dbName == "" {
		return nil, fmt.Errorf("데이터베이스 이름이 필요합니다")
	}

	opts := options.Client().ApplyURI(uri).
		SetServerSelectionTimeout(10 * time.Second).
		SetAppName("dbstudio")
	client, err := mongo.Connect(opts)
	if err != nil {
		return nil, fmt.Errorf("접속 설정 오류: %w", err)
	}
	defer func() { _ = client.Disconnect(context.WithoutCancel(ctx)) }()

	if err := client.Ping(ctx, readpref.Primary()); err != nil {
		return nil, fmt.Errorf("접속 실패: %w", err)
	}

	res := dblog.NewResult()
	db := client.Database(dbName)

	if f.WantsSource(dblog.SourceProfiler) {
		mongoProfiler(ctx, db, f, res)
	}
	if f.WantsSource(dblog.SourceErrorLog) {
		mongoServerLog(ctx, client, f, res)
	}
	if f.WantsSource(dblog.SourceCurrent) {
		mongoCurrentOps(ctx, client, f, res)
	}

	res.SortEntries()
	dblog.SortStats(res.Stats, f.StatsOrderBy)
	return res, nil
}

func mongoProfiler(ctx context.Context, db *mongo.Database, f *dblog.Filter, res *dblog.Result) {
	label := dblog.Label(dblog.SourceProfiler)

	// 프로파일링 수준을 먼저 확인한다. 0이면 기록이 쌓이지 않는다.
	var status bson.M
	level := int32(-1)
	slowMs := int32(-1)
	if err := db.RunCommand(ctx, bson.D{{Key: "profile", Value: -1}}).Decode(&status); err == nil {
		if v, ok := bsonFloat(status["was"]); ok {
			level = int32(v)
		}
		if v, ok := bsonFloat(status["slowms"]); ok {
			slowMs = int32(v)
		}
	}
	if level == 0 {
		res.MarkSource(dblog.SourceProfiler, label, false, 0,
			fmt.Sprintf("프로파일링이 꺼져 있습니다. db.setProfilingLevel(1, { slowms: %d }) 로 "+
				"느린 작업만 기록하거나 2로 전체를 기록할 수 있습니다", maxInt32(slowMs, 100)))
		return
	}

	filter := bson.D{{Key: "ts", Value: bson.D{
		{Key: "$gte", Value: f.From},
		{Key: "$lte", Value: f.To},
	}}}
	if f.MinDurationMs > 0 {
		filter = append(filter, bson.E{Key: "millis", Value: bson.D{
			{Key: "$gte", Value: f.MinDurationMs},
		}})
	}

	cur, err := db.Collection("system.profile").Find(ctx, filter,
		options.Find().SetSort(bson.D{{Key: "ts", Value: -1}}).
			SetLimit(int64(f.EffectiveLimit())))
	if err != nil {
		res.MarkSource(dblog.SourceProfiler, label, false, 0,
			"system.profile을 읽을 수 없습니다 (프로파일링 미활성 또는 권한 부족)")
		res.AddNote("system.profile 조회 실패: %v", err)
		return
	}
	defer cur.Close(ctx)

	count := 0
	for cur.Next(ctx) {
		var doc bson.M
		if err := cur.Decode(&doc); err != nil {
			res.AddNote("프로파일러 문서 디코딩 실패: %v", err)
			break
		}
		entry := mongoProfileEntry(doc)
		res.Entries = append(res.Entries, entry)
		count++
	}
	if err := cur.Err(); err != nil {
		res.AddNote("프로파일러 순회 실패: %v", err)
	}
	hint := ""
	if count == 0 && level > 0 {
		hint = fmt.Sprintf("프로파일링은 켜져 있습니다(level=%d, slowms=%d). "+
			"해당 시간 범위에 기록된 작업이 없습니다", level, slowMs)
	}
	res.MarkSource(dblog.SourceProfiler, label, true, count, hint)
}

// mongoProfileEntry는 system.profile 문서를 로그 항목으로 변환한다.
func mongoProfileEntry(doc bson.M) dblog.Entry {
	entry := dblog.Entry{
		Severity: dblog.SeverityInfo, Source: dblog.SourceProfiler,
		Extra: map[string]string{},
	}

	if ts, ok := doc["ts"].(bson.DateTime); ok {
		entry.At = ts.Time().UTC()
	} else {
		entry.At = time.Now().UTC()
	}
	if v, ok := bsonFloat(doc["millis"]); ok {
		entry.DurationMs = v
		entry.Severity = severityForDuration(v)
	}

	op, _ := doc["op"].(string)
	ns, _ := doc["ns"].(string)
	entry.Database = ns

	// 작업 유형에 따라 실제 쿼리 문서가 담긴 필드가 다르다.
	// command 필드가 최신 형태이고, query/filter는 구버전 호환이다.
	var queryDoc any
	for _, key := range []string{"command", "query", "filter"} {
		if d := bsonDoc(doc[key]); d != nil {
			queryDoc = d
			break
		}
	}
	if queryDoc != nil {
		// 문서를 정규화된 JSON 문자열로 만든다. 값은 ?로 치환해
		// 같은 형태의 질의가 하나로 묶이게 한다.
		shape := mongoQueryShape(queryDoc, 0)
		entry.Query = dblog.TruncateQuery(shape.full, 4000)
		entry.Normalized = dblog.TruncateQuery(shape.normalized, 2000)
		entry.Digest = dblog.Digest(shape.normalized)
	}

	if v, ok := bsonFloat(doc["docsExamined"]); ok {
		entry.RowsExamined = int64(v)
	}
	if v, ok := bsonFloat(doc["nreturned"]); ok {
		entry.RowsSent = int64(v)
	}
	if plan, ok := doc["planSummary"].(string); ok && plan != "" {
		entry.Extra["planSummary"] = plan
		// COLLSCAN은 인덱스를 쓰지 않은 전체 스캔이므로 주목해야 한다.
		if strings.Contains(plan, "COLLSCAN") && entry.Severity.Rank() < dblog.SeverityWarning.Rank() {
			entry.Severity = dblog.SeverityWarning
		}
	}
	if user, ok := doc["user"].(string); ok {
		entry.User = user
	}
	if client, ok := doc["client"].(string); ok {
		entry.Client = client
	}

	msg := fmt.Sprintf("%s %s %.0fms", orDefault(op, "op"), ns, entry.DurationMs)
	if entry.RowsExamined > 0 {
		msg += fmt.Sprintf(" (검사 %d / 반환 %d)", entry.RowsExamined, entry.RowsSent)
	}
	if plan := entry.Extra["planSummary"]; plan != "" {
		msg += " " + plan
	}
	entry.Message = msg
	entry.Extra["op"] = op
	return entry
}

type mongoShape struct {
	full       string
	normalized string
}

// mongoQueryShape는 BSON 문서를 두 가지 문자열로 만든다:
// 값을 포함한 전체 형태와, 값을 ?로 치환한 정규화 형태.
//
// MongoDB는 SQL이 아니므로 SQL 정규화기를 쓸 수 없다. 대신 문서 구조(키 경로)를
// 보존하고 값만 지워 같은 형태의 질의를 묶는다.
func mongoQueryShape(v any, depth int) mongoShape {
	const maxDepth = 6
	if depth > maxDepth {
		return mongoShape{full: "…", normalized: "…"}
	}

	switch val := v.(type) {
	case bson.D:
		return mongoQueryShape(bsonDoc(val), depth)

	case map[string]any:
		// 키 순서를 고정해 같은 질의가 같은 문자열이 되게 한다.
		keys := sortedKeys(val)
		fullParts := make([]string, 0, len(keys))
		normParts := make([]string, 0, len(keys))
		for _, k := range keys {
			child := mongoQueryShape(val[k], depth+1)
			fullParts = append(fullParts, fmt.Sprintf("%s: %s", k, child.full))
			normParts = append(normParts, fmt.Sprintf("%s: %s", k, child.normalized))
		}
		return mongoShape{
			full:       "{" + strings.Join(fullParts, ", ") + "}",
			normalized: "{" + strings.Join(normParts, ", ") + "}",
		}

	case bson.A:
		if len(val) == 0 {
			return mongoShape{full: "[]", normalized: "[]"}
		}
		// 배열은 첫 원소의 형태만 대표로 남긴다. 길이가 다른 배열이
		// 다른 다이제스트를 갖지 않게 하기 위함이다.
		first := mongoQueryShape(val[0], depth+1)
		fulls := make([]string, 0, len(val))
		for _, item := range val {
			fulls = append(fulls, mongoQueryShape(item, depth+1).full)
		}
		return mongoShape{
			full:       "[" + strings.Join(fulls, ", ") + "]",
			normalized: "[" + first.normalized + "]",
		}

	case string:
		return mongoShape{full: fmt.Sprintf("%q", val), normalized: "?"}
	case nil:
		return mongoShape{full: "null", normalized: "?"}
	case bool:
		return mongoShape{full: fmt.Sprintf("%t", val), normalized: "?"}
	}

	if fv, ok := bsonFloat(v); ok {
		return mongoShape{full: trimFloat(fv), normalized: "?"}
	}
	return mongoShape{full: fmt.Sprintf("%v", v), normalized: "?"}
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// 짧은 목록이므로 단순 삽입 정렬로 충분하다.
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

func trimFloat(f float64) string {
	if f == float64(int64(f)) {
		return fmt.Sprintf("%d", int64(f))
	}
	return fmt.Sprintf("%g", f)
}

// mongoServerLog는 getLog 명령으로 최근 서버 로그를 읽는다.
// 링 버퍼라 최근 1024줄 정도만 보관한다.
func mongoServerLog(ctx context.Context, client *mongo.Client, f *dblog.Filter, res *dblog.Result) {
	label := dblog.Label(dblog.SourceErrorLog)

	var out bson.M
	if err := client.Database("admin").
		RunCommand(ctx, bson.D{{Key: "getLog", Value: "global"}}).Decode(&out); err != nil {
		res.MarkSource(dblog.SourceErrorLog, label, false, 0,
			"getLog 실행 권한이 필요합니다 (clusterMonitor 역할)")
		return
	}

	lines, _ := out["log"].(bson.A)
	limit := f.EffectiveLimit()
	count := 0

	// getLog는 오래된 것부터 반환하므로 뒤에서부터 읽어 최근 것을 우선 담는다.
	for i := len(lines) - 1; i >= 0 && count < limit; i-- {
		raw, ok := lines[i].(string)
		if !ok {
			continue
		}
		entry, ok := parseMongoLogLine(raw)
		if !ok {
			continue
		}
		if entry.At.Before(f.From) || entry.At.After(f.To) {
			continue
		}
		if f.MinSeverity != "" && entry.Severity.Rank() < f.MinSeverity.Rank() {
			continue
		}
		res.Entries = append(res.Entries, entry)
		count++
	}
	res.MarkSource(dblog.SourceErrorLog, label, true, count, "")
}

// parseMongoLogLine은 MongoDB 4.4+ 의 JSON 로그 한 줄을 파싱한다.
//
// 형식: {"t":{"$date":"..."},"s":"I","c":"NETWORK","id":123,"ctx":"...","msg":"..."}
// 구버전의 텍스트 형식도 최선으로 처리한다.
func parseMongoLogLine(raw string) (dblog.Entry, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return dblog.Entry{}, false
	}

	entry := dblog.Entry{
		Source: dblog.SourceErrorLog, Severity: dblog.SeverityInfo,
		Extra: map[string]string{},
	}

	if strings.HasPrefix(raw, "{") {
		var doc bson.M
		if err := bson.UnmarshalExtJSON([]byte(raw), false, &doc); err == nil {
			if tdoc := bsonDoc(doc["t"]); tdoc != nil {
				if ds, ok := tdoc["$date"].(string); ok {
					if ts, err := time.Parse(time.RFC3339Nano, ds); err == nil {
						entry.At = ts.UTC()
					}
				}
			}
			if dt, ok := doc["t"].(bson.DateTime); ok {
				entry.At = dt.Time().UTC()
			}
			if s, ok := doc["s"].(string); ok {
				entry.Severity = mongoSeverity(s)
			}
			component, _ := doc["c"].(string)
			ctxName, _ := doc["ctx"].(string)
			msg, _ := doc["msg"].(string)
			if attr := bsonDoc(doc["attr"]); attr != nil && len(attr) > 0 {
				msg += " " + mongoQueryShape(attr, 0).full
			}
			entry.Message = dblog.TruncateQuery(msg, 2000)
			if component != "" {
				entry.Extra["component"] = component
			}
			if ctxName != "" {
				entry.Extra["context"] = ctxName
			}
			if entry.At.IsZero() {
				entry.At = time.Now().UTC()
			}
			return entry, entry.Message != ""
		}
	}

	// 구버전 텍스트 형식: 2026-08-13T12:34:56.789+0900 I NETWORK  [ctx] message
	fields := strings.Fields(raw)
	if len(fields) >= 3 {
		if ts, err := time.Parse("2006-01-02T15:04:05.000-0700", fields[0]); err == nil {
			entry.At = ts.UTC()
			entry.Severity = mongoSeverity(fields[1])
			entry.Message = dblog.TruncateQuery(strings.Join(fields[2:], " "), 2000)
			return entry, true
		}
	}
	entry.At = time.Now().UTC()
	entry.Message = dblog.TruncateQuery(raw, 2000)
	return entry, true
}

// mongoSeverity는 MongoDB의 한 글자 심각도를 변환한다.
// F=Fatal, E=Error, W=Warning, I=Info, D=Debug
func mongoSeverity(s string) dblog.Severity {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "F":
		return dblog.SeverityFatal
	case "E":
		return dblog.SeverityError
	case "W":
		return dblog.SeverityWarning
	case "D", "D1", "D2", "D3", "D4", "D5":
		return dblog.SeverityDebug
	}
	return dblog.SeverityInfo
}

// mongoCurrentOps는 currentOp로 실행 중인 작업을 읽는다.
func mongoCurrentOps(ctx context.Context, client *mongo.Client, f *dblog.Filter, res *dblog.Result) {
	label := dblog.Label(dblog.SourceCurrent)

	var out bson.M
	err := client.Database("admin").RunCommand(ctx, bson.D{
		{Key: "currentOp", Value: 1},
		{Key: "active", Value: true},
	}).Decode(&out)
	if err != nil {
		res.MarkSource(dblog.SourceCurrent, label, false, 0,
			"currentOp 실행 권한이 필요합니다 (clusterMonitor 역할)")
		return
	}

	inprog, _ := out["inprog"].(bson.A)
	limit := f.EffectiveLimit()
	count := 0
	now := time.Now().UTC()

	for _, item := range inprog {
		if count >= limit {
			break
		}
		op := bsonDoc(item)
		if op == nil {
			continue
		}
		secs, _ := bsonFloat(op["secs_running"])
		durationMs := secs * 1000
		if f.MinDurationMs > 0 && durationMs < f.MinDurationMs {
			continue
		}
		opType, _ := op["op"].(string)
		ns, _ := op["ns"].(string)
		// 내부 작업은 노이즈이므로 제외한다.
		if strings.HasPrefix(ns, "admin.") || strings.HasPrefix(ns, "local.") || opType == "none" {
			continue
		}

		entry := dblog.Entry{
			At:         now.Add(-time.Duration(secs) * time.Second),
			Severity:   severityForDuration(durationMs),
			Source:     dblog.SourceCurrent,
			Message:    fmt.Sprintf("%s %s 실행 중 %.0fs", orDefault(opType, "op"), ns, secs),
			DurationMs: durationMs, Database: ns,
			Extra: map[string]string{},
		}
		if cmd := bsonDoc(op["command"]); cmd != nil {
			shape := mongoQueryShape(cmd, 0)
			entry.Query = dblog.TruncateQuery(shape.full, 4000)
			entry.Normalized = dblog.TruncateQuery(shape.normalized, 2000)
			entry.Digest = dblog.Digest(shape.normalized)
		}
		if v, ok := bsonFloat(op["opid"]); ok {
			entry.Extra["opid"] = trimFloat(v)
		}
		if wf, ok := op["waitingForLock"].(bool); ok && wf {
			entry.Message += " (락 대기)"
			entry.Severity = dblog.SeverityWarning
		}
		res.Entries = append(res.Entries, entry)
		count++
	}
	res.MarkSource(dblog.SourceCurrent, label, true, count, "")
}

func maxInt32(a, b int32) int32 {
	if a > b {
		return a
	}
	return b
}

// ---------- Redis ----------

// Logs는 Redis의 SLOWLOG를 읽는다.
//
// Redis에는 서버 로그를 조회하는 명령이 없다(logfile은 파일로만 기록된다).
// SLOWLOG가 유일하게 접근 가능한 로그이며, 그 사실을 사용자에게 알린다.
func (a *redisAdapter) Logs(ctx context.Context, t Target, f *dblog.Filter) (*dblog.Result, error) {
	client, err := a.client(t)
	if err != nil {
		return nil, err
	}
	defer client.Close()

	if err := client.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("접속 실패: %w", err)
	}

	res := dblog.NewResult()
	res.MarkSource(dblog.SourceErrorLog, dblog.Label(dblog.SourceErrorLog), false, 0,
		"Redis는 서버 로그를 명령으로 노출하지 않습니다 (logfile 설정 경로에서 직접 확인)")

	if !f.WantsSource(dblog.SourceSlowLog) {
		return res, nil
	}
	label := dblog.Label(dblog.SourceSlowLog)

	// 임계값을 함께 보여준다. 0이면 모든 명령이 기록되고, 음수면 비활성이다.
	thresholdUs := int64(-1)
	if vals, err := client.ConfigGet(ctx, "slowlog-log-slower-than").Result(); err == nil {
		if raw, ok := vals["slowlog-log-slower-than"]; ok {
			if v, err := parseInt64(raw); err == nil {
				thresholdUs = v
			}
		}
	}
	if thresholdUs < 0 {
		res.MarkSource(dblog.SourceSlowLog, label, false, 0,
			"SLOWLOG가 비활성입니다. CONFIG SET slowlog-log-slower-than 10000 (마이크로초) 으로 활성화하세요")
		return res, nil
	}

	entries, err := client.SlowLogGet(ctx, int64(f.EffectiveLimit())).Result()
	if err != nil {
		res.MarkSource(dblog.SourceSlowLog, label, false, 0,
			"SLOWLOG GET 실행 권한이 필요합니다")
		res.AddNote("SLOWLOG 조회 실패: %v", err)
		return res, nil
	}

	count := 0
	for _, e := range entries {
		at := e.Time.UTC()
		if at.Before(f.From) || at.After(f.To) {
			continue
		}
		durationMs := float64(e.Duration.Microseconds()) / 1000
		if f.MinDurationMs > 0 && durationMs < f.MinDurationMs {
			continue
		}

		// Redis 명령은 SQL이 아니므로 명령 이름은 남기고 인자만 지운다.
		full := strings.Join(e.Args, " ")
		normalized := redisCommandShape(e.Args)

		res.Entries = append(res.Entries, dblog.Entry{
			At: at, Severity: severityForDuration(durationMs), Source: dblog.SourceSlowLog,
			Message:    fmt.Sprintf("슬로우 명령 %.2fms", durationMs),
			Query:      dblog.TruncateQuery(full, 2000),
			Normalized: normalized,
			Digest:     dblog.Digest(normalized),
			DurationMs: durationMs,
			Client:     e.ClientAddr,
			Extra: map[string]string{
				"id":         fmt.Sprintf("%d", e.ID),
				"clientName": e.ClientName,
			},
		})
		count++
	}

	hint := ""
	if count == 0 {
		hint = fmt.Sprintf("임계값 %dµs 이상 걸린 명령이 없습니다", thresholdUs)
	}
	res.MarkSource(dblog.SourceSlowLog, label, true, count, hint)

	// SLOWLOG 항목을 명령별로 집계해 통계로도 제공한다.
	// Redis에는 누적 통계 소스가 없으므로 이것이 유일한 집계 축이다.
	if f.WantsSource(dblog.SourceStatements) {
		res.Stats = aggregateEntries(res.Entries)
		res.MarkSource(dblog.SourceStatements, dblog.Label(dblog.SourceStatements),
			true, len(res.Stats), "SLOWLOG 항목을 명령 형태별로 집계했습니다")
	}

	res.SortEntries()
	dblog.SortStats(res.Stats, f.StatsOrderBy)
	return res, nil
}

// redisCommandShape는 Redis 명령의 형태만 남긴다.
// `GET user:123` → `GET ?` 처럼 명령과 인자 개수만 보존해 같은 패턴을 묶는다.
func redisCommandShape(args []string) string {
	if len(args) == 0 {
		return ""
	}
	cmd := strings.ToUpper(args[0])
	if len(args) == 1 {
		return cmd
	}
	// 인자가 여러 개면 개수만 표시한다. 키 이름은 대개 고유해서
	// 그대로 두면 모든 명령이 서로 다른 형태가 된다.
	return fmt.Sprintf("%s ?×%d", cmd, len(args)-1)
}

// aggregateEntries는 로그 항목을 다이제스트별로 집계한다.
// DB가 누적 통계를 제공하지 않을 때 시간순 로그로부터 Top-N을 만든다.
func aggregateEntries(entries []dblog.Entry) []dblog.QueryStat {
	type acc struct {
		stat dblog.QueryStat
		sum  float64
	}
	groups := map[string]*acc{}

	for _, e := range entries {
		if e.Digest == "" {
			continue
		}
		g := groups[e.Digest]
		if g == nil {
			g = &acc{stat: dblog.QueryStat{
				Digest: e.Digest, Normalized: e.Normalized, Sample: e.Query,
				MinMs: e.DurationMs,
			}}
			groups[e.Digest] = g
		}
		g.stat.Calls++
		g.sum += e.DurationMs
		if e.DurationMs > g.stat.MaxMs {
			g.stat.MaxMs = e.DurationMs
		}
		if e.DurationMs < g.stat.MinMs {
			g.stat.MinMs = e.DurationMs
		}
		ts := e.At
		if g.stat.LastSeen == nil || ts.After(*g.stat.LastSeen) {
			g.stat.LastSeen = &ts
		}
		if g.stat.FirstSeen == nil || ts.Before(*g.stat.FirstSeen) {
			g.stat.FirstSeen = &ts
		}
	}

	out := make([]dblog.QueryStat, 0, len(groups))
	for _, g := range groups {
		g.stat.TotalMs = g.sum
		if g.stat.Calls > 0 {
			g.stat.MeanMs = g.sum / float64(g.stat.Calls)
		}
		out = append(out, g.stat)
	}
	return out
}

func parseInt64(s string) (int64, error) {
	var v int64
	_, err := fmt.Sscanf(strings.TrimSpace(s), "%d", &v)
	return v, err
}
