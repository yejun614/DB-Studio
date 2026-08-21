package dbx

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"dbstudio/internal/metric"
)

// 큰 키를 찾을 때 MEMORY USAGE를 호출할 최대 키 수.
//
// 표본 전체(최대 20000개)에 대해 호출하면 왕복이 폭발하고 서버에도 부담이다.
// 파이프라인으로 묶어도 응답 크기가 커지므로 상한을 둔다.
const redisMemoryProbeLimit = 2000

// 화면에 보여줄 큰 키 개수. 목록이 길어지면 판단에 도움이 되지 않는다.
const redisBigKeyCount = 15

// ExploreKeyspace는 INFO 섹션과 키 표본에서 Redis 운영 정보를 모은다.
//
// 스키마 IR(P3)은 접두사 그룹만 담을 수 있다. 실제로 Redis를 운영할 때 필요한
// 메모리 정책·지속성 상태·TTL 없는 키·큰 키·명령 분포는 여기서만 얻는다.
func (a *redisAdapter) ExploreKeyspace(ctx context.Context, t Target) (*KeyspaceExplore, []string, error) {
	client, err := a.client(t)
	if err != nil {
		return nil, nil, err
	}
	defer client.Close()

	var notes []string
	note := func(format string, args ...any) {
		notes = append(notes, fmt.Sprintf(format, args...))
	}

	out := &KeyspaceExplore{
		Server:    map[string]string{},
		Databases: []KeyspaceDB{},
		Groups:    []*KeyGroup{},
		BigKeys:   []*KeyEntry{},
	}
	if n, err := strconv.Atoi(strings.TrimSpace(t.Conn.DatabaseName)); err == nil {
		out.SelectedDB = n
	}

	raw, err := client.Info(ctx, "server", "clients", "memory", "persistence",
		"stats", "replication", "keyspace").Result()
	if err != nil {
		// INFO를 못 읽으면 남는 정보가 거의 없지만, 키 스캔은 여전히 가능하다.
		note("INFO 명령을 실행하지 못했습니다: %v", err)
	} else {
		info := metric.ParseInfoLines(raw)
		fillRedisServer(out, info)
		fillRedisMemory(out, info)
		fillRedisPersistence(out, info)
		fillRedisStats(out, info)
		out.Databases = parseRedisKeyspace(info)
	}

	// maxmemory-policy는 INFO memory에도 있지만 CONFIG GET이 더 정확하다.
	// 관리형 서비스는 CONFIG를 막는 경우가 있어 실패를 정상 경로로 취급한다.
	if vals, err := client.ConfigGet(ctx, "maxmemory-policy").Result(); err == nil {
		if v, ok := vals["maxmemory-policy"]; ok && v != "" {
			out.Memory.Policy = v
		}
	} else if out.Memory.Policy == "" {
		note("CONFIG GET이 허용되지 않아 메모리 정책을 확인하지 못했습니다")
	}

	if n, err := client.SlowLogGet(ctx, -1).Result(); err == nil {
		out.Stats.SlowlogLen = int64(len(n))
	}

	// 명령 통계는 어떤 명령이 부하를 만드는지 보여준다.
	// commandstats는 INFO의 별도 섹션이라 위 호출에 포함되지 않는다.
	if cmdRaw, err := client.Info(ctx, "commandstats").Result(); err == nil {
		out.Commands = parseRedisCommandStats(cmdRaw)
	} else {
		note("commandstats를 읽지 못했습니다: %v", err)
	}

	if err := redisScanGroups(ctx, client, t, out, note); err != nil {
		return out, notes, err
	}
	return out, notes, nil
}

func fillRedisServer(out *KeyspaceExplore, info map[string]string) {
	for _, key := range []string{"redis_version", "redis_mode", "os", "arch_bits",
		"process_id", "run_id", "tcp_port", "role", "connected_slaves", "master_link_status"} {
		if v, ok := info[key]; ok && v != "" {
			out.Server[key] = v
		}
	}
}

func fillRedisMemory(out *KeyspaceExplore, info map[string]string) {
	out.Memory = KeyspaceMemory{
		Used:          infoI64(info, "used_memory"),
		Peak:          infoI64(info, "used_memory_peak"),
		RSS:           infoI64(info, "used_memory_rss"),
		Dataset:       infoI64(info, "used_memory_dataset"),
		MaxMemory:     infoI64(info, "maxmemory"),
		Fragmentation: infoF64(info, "mem_fragmentation_ratio"),
		Policy:        info["maxmemory_policy"],
	}
}

func fillRedisPersistence(out *KeyspaceExplore, info map[string]string) {
	out.Persistence = KeyspacePersistence{
		AOFEnabled:      info["aof_enabled"] == "1",
		RDBChangesSince: infoI64(info, "rdb_changes_since_last_save"),
		LastSaveStatus:  info["rdb_last_bgsave_status"],
		Loading:         info["loading"] == "1",
	}
	// rdb_last_save_time은 유닉스 초다. 0이면 저장 이력이 없다는 뜻이므로
	// 1970년으로 표시하지 않도록 구분한다.
	if ts := infoI64(info, "rdb_last_save_time"); ts > 0 {
		at := time.Unix(ts, 0).UTC()
		out.Persistence.RDBLastSaveAt = &at
	}
}

func fillRedisStats(out *KeyspaceExplore, info map[string]string) {
	hits := infoI64(info, "keyspace_hits")
	misses := infoI64(info, "keyspace_misses")
	out.Stats = KeyspaceStats{
		Uptime:           infoI64(info, "uptime_in_seconds"),
		ConnectedClients: infoI64(info, "connected_clients"),
		BlockedClients:   infoI64(info, "blocked_clients"),
		OpsPerSec:        infoI64(info, "instantaneous_ops_per_sec"),
		TotalCommands:    infoI64(info, "total_commands_processed"),
		KeyspaceHits:     hits,
		KeyspaceMisses:   misses,
		ExpiredKeys:      infoI64(info, "expired_keys"),
		EvictedKeys:      infoI64(info, "evicted_keys"),
	}
	if hits+misses > 0 {
		out.Stats.HitRatio = float64(hits) / float64(hits+misses) * 100
	}
}

// parseRedisKeyspace는 "db0:keys=12,expires=3,avg_ttl=0" 형태를 해석한다.
func parseRedisKeyspace(info map[string]string) []KeyspaceDB {
	dbs := []KeyspaceDB{}
	for key, value := range info {
		if !strings.HasPrefix(key, "db") {
			continue
		}
		idx, err := strconv.Atoi(strings.TrimPrefix(key, "db"))
		if err != nil {
			continue
		}
		entry := KeyspaceDB{Index: idx}
		for _, part := range strings.Split(value, ",") {
			name, num, found := strings.Cut(part, "=")
			if !found {
				continue
			}
			n, err := strconv.ParseInt(strings.TrimSpace(num), 10, 64)
			if err != nil {
				continue
			}
			switch strings.TrimSpace(name) {
			case "keys":
				entry.Keys = n
			case "expires":
				entry.Expires = n
			case "avg_ttl":
				entry.AvgTTLMs = n
			}
		}
		dbs = append(dbs, entry)
	}
	sort.Slice(dbs, func(i, j int) bool { return dbs[i].Index < dbs[j].Index })
	return dbs
}

// parseRedisCommandStats는 "cmdstat_get:calls=42,usec=100,usec_per_call=2.38" 을 해석한다.
func parseRedisCommandStats(raw string) []CommandStat {
	info := metric.ParseInfoLines(raw)
	stats := []CommandStat{}
	for key, value := range info {
		name, found := strings.CutPrefix(key, "cmdstat_")
		if !found {
			continue
		}
		st := CommandStat{Name: name}
		for _, part := range strings.Split(value, ",") {
			field, num, ok := strings.Cut(part, "=")
			if !ok {
				continue
			}
			switch field {
			case "calls":
				st.Calls, _ = strconv.ParseInt(num, 10, 64)
			case "usec_per_call":
				st.UsecPerCall, _ = strconv.ParseFloat(num, 64)
			case "rejected_calls":
				st.Rejected, _ = strconv.ParseInt(num, 10, 64)
			case "failed_calls":
				st.Failed, _ = strconv.ParseInt(num, 10, 64)
			}
		}
		stats = append(stats, st)
	}
	// 호출 수가 많은 순. 같으면 이름순으로 정렬해 결과를 결정적으로 만든다.
	sort.Slice(stats, func(i, j int) bool {
		if stats[i].Calls != stats[j].Calls {
			return stats[i].Calls > stats[j].Calls
		}
		return stats[i].Name < stats[j].Name
	})
	if len(stats) > 25 {
		stats = stats[:25]
	}
	return stats
}

// redisScanGroups는 키를 표본 스캔해 접두사 그룹과 큰 키를 만든다.
//
// SCAN을 쓰는 이유는 KEYS가 서버를 멈추기 때문이다. 표본이므로 결과는 근사값이며,
// Scanned/Truncated로 그 사실을 함께 전달한다.
func redisScanGroups(ctx context.Context, client redis.UniversalClient, t Target,
	out *KeyspaceExplore, note func(string, ...any)) error {
	delimiter := t.Opt("key_delimiter", ":")

	groups := map[string]*KeyGroup{}
	// 크기·TTL을 조사할 대상. 표본 전체가 아니라 앞쪽 일부만 본다.
	probe := make([]string, 0, redisMemoryProbeLimit)

	var cursor uint64
	for {
		keys, next, err := client.Scan(ctx, cursor, "*", redisScanBatch).Result()
		if err != nil {
			return fmt.Errorf("키 스캔 실패: %w", err)
		}
		for _, key := range keys {
			out.Scanned++
			prefix := redisKeyPrefix(key, delimiter)
			g := groups[prefix]
			if g == nil {
				g = &KeyGroup{Prefix: prefix, Types: map[string]int{}, SampleKeys: []string{}}
				groups[prefix] = g
			}
			g.Keys++
			if len(g.SampleKeys) < 3 {
				g.SampleKeys = append(g.SampleKeys, key)
			}
			if len(probe) < redisMemoryProbeLimit {
				probe = append(probe, key)
			}
		}
		cursor = next
		if cursor == 0 {
			break
		}
		if out.Scanned >= redisScanLimit {
			out.Truncated = true
			break
		}
	}

	details, err := redisProbeKeys(ctx, client, probe)
	if err != nil {
		note("키 상세 조회에 실패했습니다: %v", err)
	}
	for _, d := range details {
		g := groups[redisKeyPrefix(d.Key, delimiter)]
		if g == nil {
			continue
		}
		g.Types[d.Type]++
		g.Bytes += d.Bytes
		if d.TTL >= 0 {
			g.WithTTL++
		}
	}
	if len(details) > 0 && len(probe) < out.Scanned {
		note("타입·TTL·크기는 표본 %d개 중 앞쪽 %d개만 조사했습니다", out.Scanned, len(probe))
	}

	out.Groups = make([]*KeyGroup, 0, len(groups))
	for _, g := range groups {
		out.Groups = append(out.Groups, g)
	}
	sort.Slice(out.Groups, func(i, j int) bool {
		if out.Groups[i].Keys != out.Groups[j].Keys {
			return out.Groups[i].Keys > out.Groups[j].Keys
		}
		return out.Groups[i].Prefix < out.Groups[j].Prefix
	})
	if len(out.Groups) > redisMaxGroups {
		note("접두사 그룹이 %d개라 상위 %d개만 표시합니다", len(out.Groups), redisMaxGroups)
		out.Groups = out.Groups[:redisMaxGroups]
	}

	// 큰 키: 바이트 기준 상위 N개. 메모리 문제의 원인은 대개 소수의 키다.
	sort.Slice(details, func(i, j int) bool {
		if details[i].Bytes != details[j].Bytes {
			return details[i].Bytes > details[j].Bytes
		}
		return details[i].Key < details[j].Key
	})
	for i := range details {
		if i >= redisBigKeyCount {
			break
		}
		if details[i].Bytes == 0 {
			break
		}
		out.BigKeys = append(out.BigKeys, details[i])
	}
	if out.Scanned == 0 {
		note("키가 없습니다")
	}
	return nil
}

// redisProbeKeys는 키별 타입·TTL·메모리 사용량을 파이프라인으로 조회한다.
//
// 키마다 3번씩 왕복하면 1000개 키에 3000번이 되므로 파이프라인이 필수다.
// MEMORY USAGE는 관리형 서비스에서 막혀 있을 수 있어, 실패해도 타입·TTL은 살린다.
func redisProbeKeys(ctx context.Context, client redis.UniversalClient, keys []string) ([]*KeyEntry, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	pipe := client.Pipeline()
	types := make([]*redis.StatusCmd, len(keys))
	ttls := make([]*redis.DurationCmd, len(keys))
	mems := make([]*redis.IntCmd, len(keys))
	for i, key := range keys {
		types[i] = pipe.Type(ctx, key)
		ttls[i] = pipe.TTL(ctx, key)
		mems[i] = pipe.MemoryUsage(ctx, key)
	}
	// 개별 명령 실패는 각 Cmd에 담긴다. 여기의 err는 전송 자체의 실패다.
	if _, err := pipe.Exec(ctx); err != nil && !isRedisPartial(err) {
		return nil, err
	}

	out := make([]*KeyEntry, 0, len(keys))
	for i, key := range keys {
		entry := &KeyEntry{Key: key, TTL: -1}
		if v, err := types[i].Result(); err == nil {
			entry.Type = v
		}
		if v, err := ttls[i].Result(); err == nil {
			// Redis의 TTL은 만료 없음을 -1, 키 없음을 -2로 답한다. go-redis는 양수만
			// 초 단위로 변환하고 이 두 센티널은 Duration에 그대로 담아 돌려주므로
			// (-1ns, -2ns) 두 표기를 모두 받아야 한다. 이것을 놓치면 TTL이 없는 키가
			// 전부 "사라진 키"로 분류되어 분석에서 빠진다.
			switch v {
			case -1, -1 * time.Second:
				entry.TTL = -1
			case -2, -2 * time.Second:
				continue // 스캔 이후 사라진 키
			default:
				if v < 0 {
					continue
				}
				entry.TTL = int64(v.Seconds())
			}
		}
		if v, err := mems[i].Result(); err == nil {
			entry.Bytes = v
		}
		out = append(out, entry)
	}
	return out, nil
}

// isRedisPartial은 파이프라인 일부 명령만 실패한 경우를 판별한다.
// redis.Nil은 "키가 없다"이며 파이프라인 전체의 실패가 아니다.
func isRedisPartial(err error) bool {
	if errors.Is(err, redis.Nil) {
		return true
	}
	// MEMORY USAGE가 비활성화된 서버는 명령 오류를 돌려준다. 그것 때문에
	// 타입·TTL 결과까지 버리면 화면이 텅 비므로 부분 실패로 취급한다.
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unknown command") ||
		strings.Contains(msg, "not allowed") ||
		strings.Contains(msg, "no permissions")
}

func infoI64(info map[string]string, key string) int64 {
	v, err := strconv.ParseInt(strings.TrimSpace(info[key]), 10, 64)
	if err != nil {
		return 0
	}
	return v
}

func infoF64(info map[string]string, key string) float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(info[key]), 64)
	if err != nil {
		return 0
	}
	return v
}
