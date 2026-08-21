package dbx

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readpref"

	"dbstudio/internal/metric"
)

// ---------- MongoDB ----------

// Metrics는 serverStatus와 dbStats에서 지표를 수집한다.
// serverStatus는 clusterMonitor 역할이 필요하며, 없으면 dbStats만으로 진행한다.
func (a *mongoAdapter) Metrics(ctx context.Context, t Target) (*metric.Set, error) {
	uri, err := a.uri(t)
	if err != nil {
		return nil, err
	}
	opts := options.Client().ApplyURI(uri).
		SetServerSelectionTimeout(10 * time.Second).
		SetAppName("dbstudio")
	client, err := mongo.Connect(opts)
	if err != nil {
		return nil, fmt.Errorf("접속 설정 오류: %w", err)
	}
	defer func() { _ = client.Disconnect(context.WithoutCancel(ctx)) }()

	set := metric.NewSet()
	start := time.Now()
	if err := client.Ping(ctx, readpref.Primary()); err != nil {
		set.Gauge(metric.NameUp, 0, metric.UnitCount)
		set.AddNote("접속 실패: %v", err)
		set.LatencyMs = float64(time.Since(start).Microseconds()) / 1000
		return set, nil
	}
	set.Gauge(metric.NameUp, 1, metric.UnitCount)
	set.LatencyMs = float64(time.Since(start).Microseconds()) / 1000
	set.Gauge(metric.NameLatency, set.LatencyMs, metric.UnitMillis)

	var status bson.M
	if err := client.Database("admin").
		RunCommand(ctx, bson.D{{Key: "serverStatus", Value: 1}}).Decode(&status); err != nil {
		set.AddNote("serverStatus를 읽지 못했습니다 (clusterMonitor 역할 필요): %v", err)
	} else {
		if conns := bsonDoc(status["connections"]); conns != nil {
			if v, ok := bsonFloat(conns["current"]); ok {
				set.Gauge(metric.NameConnTotal, v, metric.UnitCount)
			}
			if v, ok := bsonFloat(conns["active"]); ok {
				set.Gauge(metric.NameConnActive, v, metric.UnitCount)
			}
			// available + current = 상한
			cur, okC := bsonFloat(conns["current"])
			avail, okA := bsonFloat(conns["available"])
			if okC && okA && cur+avail > 0 {
				set.Gauge(metric.NameConnMax, cur+avail, metric.UnitCount)
				set.Gauge(metric.NameConnUsedPct, cur/(cur+avail)*100, metric.UnitPercent)
			}
		} else {
			set.AddNote("serverStatus.connections를 해석하지 못했습니다")
		}

		if ops := bsonDoc(status["opcounters"]); ops != nil {
			total := 0.0
			for _, key := range []string{"insert", "query", "update", "delete", "getmore", "command"} {
				if v, ok := bsonFloat(ops[key]); ok {
					total += v
				}
			}
			set.Counter(metric.NameQueryRate, total)
		}
		if v, ok := bsonFloat(status["uptime"]); ok {
			set.Gauge(metric.NameUptime, v, metric.UnitSeconds)
		}
		if mem := bsonDoc(status["mem"]); mem != nil {
			if v, ok := bsonFloat(mem["resident"]); ok {
				// mem.resident는 MiB 단위다.
				set.Gauge(metric.NameMemoryUsed, v*1024*1024, metric.UnitBytes)
			}
		}
		if gl := bsonDoc(status["globalLock"]); gl != nil {
			if cq := bsonDoc(gl["currentQueue"]); cq != nil {
				if v, ok := bsonFloat(cq["total"]); ok {
					set.Gauge(metric.NameLockWaits, v, metric.UnitCount)
				}
			}
		}
		// WiredTiger 캐시 적중률
		if wt := bsonDoc(status["wiredTiger"]); wt != nil {
			if cache := bsonDoc(wt["cache"]); cache != nil {
				readInto, ok1 := bsonFloat(cache["pages read into cache"])
				reqs, ok2 := bsonFloat(cache["pages requested from the cache"])
				if ok1 && ok2 && reqs > 0 {
					set.Gauge(metric.NameCacheHitRatio, (reqs-readInto)/reqs*100, metric.UnitPercent)
				}
			}
		}
	}

	if dbName := t.Conn.DatabaseName; dbName != "" {
		var stats bson.M
		if err := client.Database(dbName).
			RunCommand(ctx, bson.D{{Key: "dbStats", Value: 1}}).Decode(&stats); err != nil {
			set.AddNote("dbStats를 읽지 못했습니다: %v", err)
		} else {
			if v, ok := bsonFloat(stats["dataSize"]); ok {
				set.Gauge(metric.NameDataSize, v, metric.UnitBytes)
			}
			if v, ok := bsonFloat(stats["indexSize"]); ok {
				set.Gauge(metric.NameIndexSize, v, metric.UnitBytes)
			}
			if v, ok := bsonFloat(stats["collections"]); ok {
				set.Gauge("collections.count", v, metric.UnitCount)
			}
			if v, ok := bsonFloat(stats["objects"]); ok {
				set.Gauge("documents.count", v, metric.UnitCount)
			}
		}
	}

	set.Sort()
	return set, nil
}

// bsonDoc는 중첩 BSON 문서를 맵으로 정규화한다.
//
// 드라이버는 interface{} 자리의 중첩 문서를 기본적으로 bson.D(순서 보존)로
// 디코딩한다. bson.M으로 타입 단정하면 조용히 실패해 지표가 통째로 빠지므로
// 두 형태를 모두 받아 맵으로 바꾼다. 문서가 아니면 nil을 반환한다.
func bsonDoc(v any) map[string]any {
	switch d := v.(type) {
	case bson.M:
		return d
	case map[string]any:
		return d
	case bson.D:
		out := make(map[string]any, len(d))
		for _, e := range d {
			out[e.Key] = e.Value
		}
		return out
	}
	return nil
}

// bsonNegative는 인덱스 키 방향(-1 = 내림차순)을 판별한다.
func bsonNegative(v any) bool {
	f, ok := bsonFloat(v)
	return ok && f < 0
}

// bsonFloat는 BSON이 돌려주는 여러 수치 타입을 float64로 통일한다.
func bsonFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case float64:
		return n, true
	case bson.Decimal128:
		if f, err := strconv.ParseFloat(n.String(), 64); err == nil {
			return f, true
		}
	}
	return 0, false
}

// ---------- Redis ----------

// Metrics는 INFO 명령의 여러 섹션에서 지표를 수집한다.
func (a *redisAdapter) Metrics(ctx context.Context, t Target) (*metric.Set, error) {
	client, err := a.client(t)
	if err != nil {
		return nil, err
	}
	defer client.Close()

	set := metric.NewSet()
	start := time.Now()
	if err := client.Ping(ctx).Err(); err != nil {
		set.Gauge(metric.NameUp, 0, metric.UnitCount)
		set.AddNote("접속 실패: %v", err)
		set.LatencyMs = float64(time.Since(start).Microseconds()) / 1000
		return set, nil
	}
	set.Gauge(metric.NameUp, 1, metric.UnitCount)
	set.LatencyMs = float64(time.Since(start).Microseconds()) / 1000
	set.Gauge(metric.NameLatency, set.LatencyMs, metric.UnitMillis)

	raw, err := client.Info(ctx, "clients", "memory", "stats", "server", "keyspace", "replication").Result()
	if err != nil {
		set.AddNote("INFO 명령을 실행하지 못했습니다: %v", err)
		set.Sort()
		return set, nil
	}
	info := metric.ParseInfoLines(raw)

	infoGauge(set, info, "connected_clients", metric.NameConnTotal, metric.UnitCount)
	infoGauge(set, info, "blocked_clients", metric.NameBlockedClients, metric.UnitCount)
	infoGauge(set, info, "used_memory", metric.NameMemoryUsed, metric.UnitBytes)
	infoGauge(set, info, "maxmemory", "memory.max", metric.UnitBytes)
	infoGauge(set, info, "mem_fragmentation_ratio", "memory.fragmentation", metric.UnitRatio)
	infoGauge(set, info, "uptime_in_seconds", metric.NameUptime, metric.UnitSeconds)
	// instantaneous_ops_per_sec는 Redis가 직접 계산한 순간 처리량이므로
	// 카운터 변환 없이 게이지로 쓴다.
	infoGauge(set, info, "instantaneous_ops_per_sec", metric.NameQueryRate, metric.UnitPerSec)

	infoCounter(set, info, "evicted_keys", metric.NameEvictedRate)
	infoCounter(set, info, "rejected_connections", metric.NameAbortedConnRate)

	// 키스페이스 적중률
	hits, okH := infoFloat(info, "keyspace_hits")
	misses, okM := infoFloat(info, "keyspace_misses")
	if okH && okM && hits+misses > 0 {
		set.Gauge(metric.NameCacheHitRatio, hits/(hits+misses)*100, metric.UnitPercent)
	}

	// 메모리 사용률: maxmemory가 0이면 무제한이라 비율을 계산하지 않는다.
	used, okU := infoFloat(info, "used_memory")
	maxMem, okX := infoFloat(info, "maxmemory")
	if okU && okX && maxMem > 0 {
		set.Gauge("memory.used_pct", used/maxMem*100, metric.UnitPercent)
	}

	// 복제 지연: 슬레이브일 때만 의미가 있다.
	if info["role"] == "slave" {
		if v, ok := infoFloat(info, "master_last_io_seconds_ago"); ok {
			set.Gauge(metric.NameReplicaLag, v, metric.UnitSeconds)
		}
	}

	// 선택한 DB의 키 수
	if dbIdx := t.Conn.DatabaseName; dbIdx != "" {
		if n, err := client.DBSize(ctx).Result(); err == nil {
			set.Gauge("keys.count", float64(n), metric.UnitCount)
		}
	}

	set.Sort()
	return set, nil
}

func infoFloat(info map[string]string, key string) (float64, bool) {
	v, ok := info[key]
	if !ok {
		return 0, false
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0, false
	}
	return f, true
}

func infoGauge(set *metric.Set, info map[string]string, key, name string, unit metric.Unit) {
	if v, ok := infoFloat(info, key); ok {
		set.Gauge(name, v, unit)
	}
}

func infoCounter(set *metric.Set, info map[string]string, key, name string) {
	if v, ok := infoFloat(info, key); ok {
		set.Counter(name, v)
	}
}
