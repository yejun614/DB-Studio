package dbx

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// Redis의 데이터 조회·수정.
//
// Redis에는 테이블도 컬렉션도 없다. 있는 것은 키 하나하나이고, 그 값의 모양이
// 타입마다 다르다(문자열, 해시, 리스트, 셋, 정렬된 셋, 스트림).
//
// 그래서 여기서는 **키를 행으로 본다.** 목록은 키 목록이고, 열은 키·타입·TTL·크기·값이다.
// 값 열은 타입에 따라 다르게 만든다 — 해시는 필드:값 쌍을, 리스트는 앞부분 원소를 보여준다.
// 이 방식이 아니면 타입마다 다른 화면이 필요하고, 그러면 화면이 여섯 개가 된다.
//
// 목록의 "네임스페이스"는 키 접두사(prefix)다. explore_redis.go가 이미 접두사로
// 키를 묶고 있고, 사람들은 실제로 그렇게 키를 설계한다(user:1:profile).

func (a *redisAdapter) DataCapabilities() DataCapabilities {
	return DataCapabilities{
		Browse: true, Filter: false, Sort: false, Mutate: true, Statement: true,
		StatementLabel: "명령",
		StatementHelp:  "Redis 명령을 한 줄에 하나씩 씁니다. 예: GET user:1",
	}
}

// redisScanCount는 SCAN 한 번에 요청하는 키 수다.
// COUNT는 힌트일 뿐이라 실제 반환 수는 다르며, 그래서 원하는 개수를 채울 때까지 돈다.
const redisScanCount = 500

// redisMaxScan은 한 요청에서 훑을 키 수의 상한이다.
// 패턴에 맞는 키가 거의 없는 큰 DB에서 SCAN이 끝나지 않는 것을 막는다.
const redisMaxScan = 100_000

// ListObjects는 키 접두사 그룹을 나열한다.
//
// 키를 전부 나열하지 않는 이유: 운영 Redis에는 키가 수백만 개 있고, 목록 화면이
// 그것을 받는 것은 의미가 없다. 접두사로 묶어 "어떤 종류의 키가 있는가"를 먼저
// 보여주고, 실제 키는 그 안에서 조회한다.
func (a *redisAdapter) ListObjects(ctx context.Context, t Target) ([]DataObject, error) {
	client, err := a.client(t)
	if err != nil {
		return nil, err
	}
	defer client.Close()

	counts := map[string]int64{}
	var cursor uint64
	scanned := 0
	for scanned < redisMaxScan {
		keys, next, err := client.Scan(ctx, cursor, "*", redisScanCount).Result()
		if err != nil {
			return nil, fmt.Errorf("키 스캔 실패: %w", err)
		}
		for _, k := range keys {
			counts[redisPrefix(k)]++
		}
		scanned += len(keys)
		cursor = next
		if cursor == 0 {
			break
		}
	}

	out := make([]DataObject, 0, len(counts))
	for prefix, n := range counts {
		out = append(out, DataObject{
			Namespace: "", Name: prefix, Kind: "keyspace", RowCount: n,
		})
	}
	// 키가 많은 그룹부터 보여준다. 이름순은 접두사가 수십 개일 때 아무 도움이 안 된다.
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j].RowCount > out[i].RowCount ||
				(out[j].RowCount == out[i].RowCount && out[j].Name < out[i].Name) {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out, nil
}

// redisPrefix는 키에서 그룹 이름을 뽑는다.
// 콜론 앞 두 마디까지를 그룹으로 본다(user:1:profile → user:*). 숫자·UUID 같은
// 식별자 마디는 * 로 축약한다 — explore_redis.go와 같은 규칙이다.
func redisPrefix(key string) string {
	parts := strings.Split(key, ":")
	if len(parts) == 1 {
		return key
	}
	out := []string{parts[0]}
	if len(parts) > 1 {
		out = append(out, "*")
	}
	return strings.Join(out, ":")
}

func (a *redisAdapter) QueryRows(ctx context.Context, t Target, q RowQuery) (*RowPage, error) {
	client, err := a.client(t)
	if err != nil {
		return nil, err
	}
	defer client.Close()

	// 패턴을 정한다. 그룹 이름(user:*)이 곧 SCAN 패턴이고, 검색어가 있으면 좁힌다.
	pattern := strings.TrimSpace(q.Table.Name)
	if pattern == "" {
		pattern = "*"
	}
	if s := strings.TrimSpace(q.Search); s != "" {
		pattern = "*" + redisGlobQuote(s) + "*"
	}

	limit := q.EffectiveLimit()
	start := time.Now()

	// SCAN은 커서 기반이라 임의 오프셋이 없다. 오프셋만큼 건너뛰며 읽는다.
	// 페이지가 깊어질수록 비용이 늘지만, Redis에서 깊은 페이지를 넘기는 것은
	// 원래 드문 동작이고 대안(전체를 메모리에 올리기)이 훨씬 나쁘다.
	keys := []string{}
	var cursor uint64
	skipped := 0
	scanned := 0
	for scanned < redisMaxScan {
		batch, next, err := client.Scan(ctx, cursor, pattern, redisScanCount).Result()
		if err != nil {
			return nil, fmt.Errorf("키 스캔 실패: %w", err)
		}
		scanned += len(batch)
		for _, k := range batch {
			if skipped < q.Offset {
				skipped++
				continue
			}
			keys = append(keys, k)
			if len(keys) > limit {
				break
			}
		}
		cursor = next
		if cursor == 0 || len(keys) > limit {
			break
		}
	}
	hasMore := false
	if len(keys) > limit {
		keys = keys[:limit]
		hasMore = true
	}

	cols := []DataColumn{
		{Name: "key", Type: "string", PK: true},
		{Name: "type", Type: "string", Nullable: false},
		{Name: "ttl", Type: "seconds", Nullable: true},
		{Name: "size", Type: "int", Nullable: true},
		{Name: "value", Type: "string", Nullable: true},
	}

	rows := make([][]any, 0, len(keys))
	truncated := [][2]int{}
	notes := []string{}
	for i, key := range keys {
		kind, err := client.Type(ctx, key).Result()
		if err != nil {
			// 스캔과 조회 사이에 키가 만료되는 것은 정상이다. 그 키만 건너뛴다.
			continue
		}
		ttl := redisTTL(ctx, client, key)
		value, size, err := redisValue(ctx, client, key, kind, q.Full)
		if err != nil {
			notes = append(notes, fmt.Sprintf("%s: %v", key, err))
			continue
		}
		v, cut := normalizeValue(value, !q.Full)
		if cut {
			truncated = append(truncated, [2]int{i, 4})
		}
		rows = append(rows, []any{key, kind, ttl, size, v})
	}

	return &RowPage{
		Columns: cols, Rows: rows, PrimaryKey: []string{"key"}, Truncated: truncated,
		Total: -1, Offset: q.Offset, Limit: limit, HasMore: hasMore,
		ElapsedMs: float64(time.Since(start).Microseconds()) / 1000,
		Editable:  true, Notes: notes,
	}, nil
}

// redisTTL은 남은 수명을 초로 반환한다. 만료가 없으면 nil이다.
func redisTTL(ctx context.Context, client redis.UniversalClient, key string) any {
	d, err := client.TTL(ctx, key).Result()
	if err != nil || d < 0 {
		// -1은 만료 없음, -2는 키 없음. 둘 다 "표시할 TTL이 없다"로 본다.
		return nil
	}
	return int64(d / time.Second)
}

// redisValue는 타입별로 값을 읽어 표시용 문자열과 크기를 만든다.
//
// 컬렉션 타입은 앞부분만 읽는다. 원소가 백만 개인 리스트를 전부 가져오면
// 서버와 앱 양쪽이 멈춘다. 편집은 문자열 타입에서만 지원하므로(아래 MutateRow)
// 잘라 읽어도 데이터를 잃지 않는다.
func redisValue(ctx context.Context, client redis.UniversalClient, key, kind string, full bool) (string, any, error) {
	const preview = 20

	switch kind {
	case "string":
		v, err := client.Get(ctx, key).Result()
		if err != nil {
			return "", nil, err
		}
		return v, len(v), nil

	case "hash":
		n, _ := client.HLen(ctx, key).Result()
		if full {
			all, err := client.HGetAll(ctx, key).Result()
			if err != nil {
				return "", nil, err
			}
			parts := make([]string, 0, len(all))
			for f, v := range all {
				parts = append(parts, f+"="+v)
			}
			return strings.Join(parts, ", "), n, nil
		}
		fields, _, err := client.HScan(ctx, key, 0, "*", preview*2).Result()
		if err != nil {
			return "", nil, err
		}
		parts := []string{}
		for i := 0; i+1 < len(fields); i += 2 {
			parts = append(parts, fields[i]+"="+fields[i+1])
		}
		return strings.Join(parts, ", "), n, nil

	case "list":
		n, _ := client.LLen(ctx, key).Result()
		stop := int64(preview - 1)
		if full {
			stop = -1
		}
		items, err := client.LRange(ctx, key, 0, stop).Result()
		if err != nil {
			return "", nil, err
		}
		return strings.Join(items, ", "), n, nil

	case "set":
		n, _ := client.SCard(ctx, key).Result()
		count := int64(preview)
		if full {
			count = n
		}
		items, err := client.SRandMemberN(ctx, key, count).Result()
		if err != nil {
			return "", nil, err
		}
		return strings.Join(items, ", "), n, nil

	case "zset":
		n, _ := client.ZCard(ctx, key).Result()
		stop := int64(preview - 1)
		if full {
			stop = -1
		}
		items, err := client.ZRangeWithScores(ctx, key, 0, stop).Result()
		if err != nil {
			return "", nil, err
		}
		parts := make([]string, 0, len(items))
		for _, z := range items {
			parts = append(parts, fmt.Sprintf("%v(%g)", z.Member, z.Score))
		}
		return strings.Join(parts, ", "), n, nil

	case "stream":
		n, _ := client.XLen(ctx, key).Result()
		items, err := client.XRangeN(ctx, key, "-", "+", preview).Result()
		if err != nil {
			return "", nil, err
		}
		parts := make([]string, 0, len(items))
		for _, m := range items {
			parts = append(parts, fmt.Sprintf("%s%v", m.ID, m.Values))
		}
		return strings.Join(parts, ", "), n, nil

	default:
		return "(" + kind + ")", nil, nil
	}
}

// redisGlobQuote는 검색어의 글롭 메타문자를 무력화한다.
func redisGlobQuote(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `*`, `\*`, `?`, `\?`, `[`, `\[`, `]`, `\]`)
	return r.Replace(s)
}

// MutateRow는 키를 추가·수정·삭제한다.
//
// 문자열 타입만 수정할 수 있다. 해시의 필드 하나, 리스트의 n번째 원소를 고치는 것은
// 각각 다른 명령이고 화면도 달라야 하는데, 표 한 칸을 고치는 UI로 그것을 표현하면
// 사용자가 무엇을 바꾸는지 알 수 없게 된다. 컬렉션 타입은 명령 콘솔에서 다룬다.
func (a *redisAdapter) MutateRow(ctx context.Context, t Target, m RowMutation) (*MutationResult, error) {
	client, err := a.client(t)
	if err != nil {
		return nil, err
	}
	defer client.Close()

	key, _ := m.Key["key"].(string)
	if m.Action == "insert" {
		if v, ok := m.Values["key"].(string); ok {
			key = v
		}
	}
	if strings.TrimSpace(key) == "" {
		return nil, fmt.Errorf("키 이름이 필요합니다")
	}

	switch m.Action {
	case "delete":
		n, err := client.Del(ctx, key).Result()
		if err != nil {
			return nil, fmt.Errorf("삭제 실패: %w", err)
		}
		return &MutationResult{Affected: n, Statement: "DEL " + key}, nil

	case "insert", "update":
		if m.Action == "update" {
			kind, err := client.Type(ctx, key).Result()
			if err != nil {
				return nil, fmt.Errorf("키 타입을 확인하지 못했습니다: %w", err)
			}
			if kind != "string" && kind != "none" {
				return nil, fmt.Errorf("%s 타입은 표 편집으로 수정할 수 없습니다. 명령 콘솔을 사용하세요", kind)
			}
		}
		raw, ok := m.Values["value"]
		if !ok {
			return nil, fmt.Errorf("저장할 값이 필요합니다")
		}
		value := fmt.Sprint(raw)
		if raw == nil {
			value = ""
		}

		// TTL을 함께 보냈으면 유지한다. 값을 고쳤다는 이유로 만료가 사라지면
		// 캐시가 영원히 남게 되는데, 그것은 화면에서 보이지 않는 사고다.
		expire := time.Duration(0)
		if ttl, ok := m.Values["ttl"]; ok && ttl != nil {
			if secs, err := strconv.ParseInt(fmt.Sprint(ttl), 10, 64); err == nil && secs > 0 {
				expire = time.Duration(secs) * time.Second
			}
		}
		if err := client.Set(ctx, key, value, expire).Err(); err != nil {
			return nil, fmt.Errorf("저장 실패: %w", err)
		}
		stmt := "SET " + key + " …"
		if expire > 0 {
			stmt += fmt.Sprintf(" EX %d", int64(expire/time.Second))
		}
		return &MutationResult{Affected: 1, Statement: stmt}, nil

	default:
		return nil, fmt.Errorf("알 수 없는 동작입니다: %s", m.Action)
	}
}

// RunStatements는 Redis 명령을 한 줄에 하나씩 실행한다.
func (a *redisAdapter) RunStatements(ctx context.Context, t Target, r StatementRequest) ([]StatementResult, error) {
	lines := []string{}
	for line := range strings.SplitSeq(r.Statement, "\n") {
		if s := strings.TrimSpace(line); s != "" && !strings.HasPrefix(s, "#") {
			lines = append(lines, s)
		}
	}
	if len(lines) == 0 {
		return nil, fmt.Errorf("실행할 명령이 없습니다")
	}

	client, err := a.client(t)
	if err != nil {
		return nil, err
	}
	defer client.Close()

	out := make([]StatementResult, 0, len(lines))
	for _, line := range lines {
		res := StatementResult{Statement: line}
		args, err := redisSplitArgs(line)
		if err != nil {
			res.Error = err.Error()
			out = append(out, res)
			break
		}
		if r.ReadOnly && !redisReadOnlyCommand(args[0]) {
			res.Error = "읽기 전용 모드에서는 조회 명령만 실행할 수 있습니다"
			out = append(out, res)
			break
		}
		if redisBlockedCommand(args[0]) {
			// FLUSHALL 같은 명령은 실행 취소가 없고 되돌릴 방법도 없다.
			// 권한과 별개로, 콘솔에 한 줄 잘못 쳐서 일어나기에는 결과가 너무 크다.
			res.Error = fmt.Sprintf("%s 명령은 이 콘솔에서 실행할 수 없습니다", strings.ToUpper(args[0]))
			out = append(out, res)
			break
		}

		start := time.Now()
		anyArgs := make([]any, len(args))
		for i, a := range args {
			anyArgs[i] = a
		}
		value, err := client.Do(ctx, anyArgs...).Result()
		res.ElapsedMs = float64(time.Since(start).Microseconds()) / 1000
		if err != nil && err != redis.Nil {
			res.Error = err.Error()
			out = append(out, res)
			break
		}
		res.Kind = "rows"
		res.Columns = []DataColumn{{Name: "result", Type: "value"}}
		for _, row := range redisFlatten(value) {
			v, _ := normalizeValue(row, true)
			res.Rows = append(res.Rows, []any{v})
		}
		res.Affected = int64(len(res.Rows))
		out = append(out, res)
	}
	return out, nil
}

// redisFlatten은 응답을 행 목록으로 편다.
// 배열 응답(LRANGE, HGETALL)을 한 칸에 넣으면 읽을 수 없다.
func redisFlatten(v any) []any {
	switch val := v.(type) {
	case nil:
		return []any{nil}
	case []any:
		out := make([]any, 0, len(val))
		for _, item := range val {
			if nested, ok := item.([]any); ok {
				out = append(out, fmt.Sprint(nested))
				continue
			}
			out = append(out, item)
		}
		return out
	default:
		return []any{val}
	}
}

// redisSplitArgs는 명령 한 줄을 인자로 나눈다. 따옴표로 공백을 포함할 수 있다.
func redisSplitArgs(line string) ([]string, error) {
	args := []string{}
	var cur strings.Builder
	quote := rune(0)
	started := false

	for _, r := range line {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
				continue
			}
			cur.WriteRune(r)
		case r == '\'' || r == '"':
			quote = r
			started = true
		case r == ' ' || r == '\t':
			if started {
				args = append(args, cur.String())
				cur.Reset()
				started = false
			}
		default:
			cur.WriteRune(r)
			started = true
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("따옴표가 닫히지 않았습니다")
	}
	if started {
		args = append(args, cur.String())
	}
	if len(args) == 0 {
		return nil, fmt.Errorf("명령이 비어 있습니다")
	}
	return args, nil
}

func redisReadOnlyCommand(cmd string) bool {
	switch strings.ToUpper(cmd) {
	case "GET", "MGET", "STRLEN", "EXISTS", "TTL", "PTTL", "TYPE", "KEYS", "SCAN",
		"HGET", "HGETALL", "HKEYS", "HVALS", "HLEN", "HSCAN", "HMGET", "HEXISTS",
		"LRANGE", "LLEN", "LINDEX",
		"SMEMBERS", "SCARD", "SISMEMBER", "SSCAN", "SRANDMEMBER",
		"ZRANGE", "ZREVRANGE", "ZCARD", "ZSCORE", "ZCOUNT", "ZSCAN", "ZRANGEBYSCORE",
		"XRANGE", "XLEN", "XINFO",
		"INFO", "DBSIZE", "MEMORY", "OBJECT", "COMMAND", "CONFIG", "CLIENT", "SLOWLOG",
		"LOLWUT", "PING", "TIME", "RANDOMKEY", "GETRANGE", "BITCOUNT", "LPOS", "SINTER":
		return true
	}
	return false
}

// redisBlockedCommand는 콘솔에서 막는 명령이다.
func redisBlockedCommand(cmd string) bool {
	switch strings.ToUpper(cmd) {
	case "FLUSHALL", "FLUSHDB", "SHUTDOWN", "DEBUG", "REPLICAOF", "SLAVEOF",
		"MIGRATE", "SWAPDB", "SCRIPT", "EVAL", "EVALSHA", "FUNCTION", "ACL", "MODULE":
		return true
	}
	return false
}
