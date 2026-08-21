package dbx

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/redis/go-redis/v9"

	"dbstudio/internal/model"
)

// 통합 테스트: docker/compose.test.yaml 의 MongoDB/Redis 인스턴스를 사용한다.
// 컨테이너가 없으면 스킵한다.
func skipUnlessIntegration(t *testing.T) {
	t.Helper()
	if !*runIntegration && os.Getenv("DBSTUDIO_INTEGRATION") == "" {
		t.Skip("통합 테스트: -integration 플래그 또는 DBSTUDIO_INTEGRATION 환경변수 필요")
	}
}

func mongoTarget() Target {
	return Target{
		Conn: &model.Connection{
			Kind: model.KindMongoDB, Host: "127.0.0.1", Port: 27018,
			DatabaseName: "explore_test", Options: model.Options{"auth_source": "admin"},
		},
		Secret: &model.Secret{Username: "root", Password: "rootpw123"},
	}
}

func redisTarget() Target {
	return Target{
		Conn: &model.Connection{
			Kind: model.KindRedis, Host: "127.0.0.1", Port: 16379,
			// 다른 테스트와 키가 섞이지 않도록 전용 DB 인덱스를 쓴다.
			DatabaseName: "3", Options: model.Options{},
		},
		Secret: &model.Secret{Password: "rootpw123"},
	}
}

// TestExploreMongo는 컬렉션 통계·인덱스 상세·필드 분포를 검증한다.
//
// 검증하는 것:
//  1. 컬렉션별 저장 통계를 읽는가 (스키마 IR에는 없는 정보)
//  2. 인덱스의 unique/sparse/TTL 속성과 크기를 읽는가
//  3. 뷰를 컬렉션과 구분하는가
//  4. 필드 존재 비율이 실제 데이터와 일치하는가
func TestExploreMongo(t *testing.T) {
	skipUnlessIntegration(t)

	target := mongoTarget()
	adapter, err := Get(model.KindMongoDB)
	if err != nil {
		t.Fatalf("어댑터 없음: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if _, err := adapter.Ping(ctx, target); err != nil {
		t.Skipf("접속 불가 (컨테이너 미실행?): %v", err)
	}

	client := mongoTestClient(t, target)
	db := client.Database(target.Conn.DatabaseName)
	t.Cleanup(func() {
		_ = db.Drop(context.WithoutCancel(ctx))
		_ = client.Disconnect(context.WithoutCancel(ctx))
	})
	_ = db.Drop(ctx)

	// 문서 10개 중 6개만 optionalField를 가진다 → 존재 비율 60%.
	docs := make([]any, 0, 10)
	for i := range 10 {
		doc := bson.D{
			{Key: "userId", Value: i},
			{Key: "name", Value: "user"},
			{Key: "createdAt", Value: time.Now().UTC()},
		}
		if i < 6 {
			doc = append(doc, bson.E{Key: "optionalField", Value: "present"})
		}
		docs = append(docs, doc)
	}
	if _, err := db.Collection("profiles").InsertMany(ctx, docs); err != nil {
		t.Fatalf("문서 삽입 실패: %v", err)
	}
	// 인덱스 3종: 일반, unique, TTL.
	ttl := int32(3600)
	if _, err := db.Collection("profiles").Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "userId", Value: 1}}, Options: options.Index().SetUnique(true).SetName("uniq_user")},
		{Keys: bson.D{{Key: "name", Value: -1}}, Options: options.Index().SetName("by_name")},
		{Keys: bson.D{{Key: "createdAt", Value: 1}}, Options: options.Index().
			SetExpireAfterSeconds(ttl).SetName("ttl_created")},
	}); err != nil {
		t.Fatalf("인덱스 생성 실패: %v", err)
	}
	if err := db.CreateView(ctx, "profiles_view", "profiles", mongo.Pipeline{}); err != nil {
		t.Fatalf("뷰 생성 실패: %v", err)
	}

	res, err := DoExplore(ctx, target)
	if err != nil {
		t.Fatalf("DoExplore 실패: %v", err)
	}
	if res.Document == nil {
		t.Fatalf("document 결과가 비어 있습니다")
	}
	d := res.Document

	if d.Stats.Objects < 10 {
		t.Errorf("dbStats.objects = %d, 10 이상이어야 합니다", d.Stats.Objects)
	}
	if d.Stats.StorageSize == 0 {
		t.Error("dbStats.storageSize를 읽지 못했습니다")
	}

	var profiles, view *DocumentCollection
	for _, c := range d.Collections {
		switch c.Name {
		case "profiles":
			profiles = c
		case "profiles_view":
			view = c
		}
	}
	if profiles == nil || view == nil {
		t.Fatalf("컬렉션을 찾지 못했습니다: %+v", collectionNames(d))
	}

	if view.Type != "view" {
		t.Errorf("뷰의 type = %q, view여야 합니다", view.Type)
	}
	if view.ViewOn != "profiles" {
		t.Errorf("뷰의 viewOn = %q, profiles여야 합니다", view.ViewOn)
	}
	if len(view.Indexes) != 0 {
		t.Errorf("뷰에 인덱스가 %d개 있습니다", len(view.Indexes))
	}

	if profiles.Documents != 10 {
		t.Errorf("문서 수 = %d, 10이어야 합니다", profiles.Documents)
	}
	if profiles.StorageSize == 0 || profiles.IndexSize == 0 {
		t.Errorf("저장 통계가 비었습니다: storage=%d index=%d",
			profiles.StorageSize, profiles.IndexSize)
	}
	if profiles.Sampled != 10 {
		t.Errorf("샘플 수 = %d, 10이어야 합니다", profiles.Sampled)
	}

	// 필드 존재 비율: 6/10 = 0.6
	fields := map[string]*DocumentField{}
	for _, f := range profiles.Fields {
		fields[f.Path] = f
	}
	if got := fields["optionalField"]; got == nil {
		t.Error("optionalField를 찾지 못했습니다")
	} else if got.Presence < 0.55 || got.Presence > 0.65 {
		t.Errorf("optionalField 존재 비율 = %.2f, 0.6이어야 합니다", got.Presence)
	}
	if got := fields["userId"]; got == nil || got.Presence != 1 {
		t.Errorf("userId 존재 비율 = %v, 1이어야 합니다", got)
	}

	indexes := map[string]*DocumentIndex{}
	for _, idx := range profiles.Indexes {
		indexes[idx.Name] = idx
	}
	if idx := indexes["uniq_user"]; idx == nil {
		t.Error("uniq_user 인덱스를 찾지 못했습니다")
	} else {
		if !idx.Unique {
			t.Error("uniq_user의 unique가 false입니다")
		}
		if idx.Keys != "userId ASC" {
			t.Errorf("uniq_user 키 = %q", idx.Keys)
		}
		if idx.SizeBytes == 0 {
			t.Error("uniq_user 크기를 읽지 못했습니다 (collStats.indexSizes)")
		}
	}
	if idx := indexes["by_name"]; idx == nil {
		t.Error("by_name 인덱스를 찾지 못했습니다")
	} else if idx.Keys != "name DESC" {
		// 방향은 인덱스가 어떤 정렬에 쓰일 수 있는지를 결정하므로 정확해야 한다.
		t.Errorf("by_name 키 = %q, 'name DESC'여야 합니다", idx.Keys)
	}
	if idx := indexes["ttl_created"]; idx == nil {
		t.Error("ttl_created 인덱스를 찾지 못했습니다")
	} else if idx.TTLSecond == nil || *idx.TTLSecond != int64(ttl) {
		t.Errorf("ttl_created TTL = %v, %d이어야 합니다", idx.TTLSecond, ttl)
	}
	// _id_ 인덱스는 사용자가 만들지 않았어도 존재한다. 탐색 화면에서는
	// 크기를 차지하는 실물이므로 숨기지 않는다.
	if _, ok := indexes["_id_"]; !ok {
		t.Error("_id_ 인덱스가 목록에 없습니다")
	}
}

func collectionNames(d *DocumentExplore) []string {
	out := make([]string, 0, len(d.Collections))
	for _, c := range d.Collections {
		out = append(out, c.Name)
	}
	return out
}

func mongoTestClient(t *testing.T, target Target) *mongo.Client {
	t.Helper()
	a := &mongoAdapter{}
	uri, err := a.uri(target)
	if err != nil {
		t.Fatalf("uri: %v", err)
	}
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatalf("연결 실패: %v", err)
	}
	return client
}

// TestExploreRedis는 INFO 파싱과 키 표본 분석을 검증한다.
//
// 검증하는 것:
//  1. 메모리·지속성·통계 섹션을 읽는가
//  2. 접두사 그룹이 타입·TTL과 함께 집계되는가
//  3. 큰 키가 크기 순으로 나오는가
//  4. keyspace 섹션(db별 키 수)을 해석하는가
func TestExploreRedis(t *testing.T) {
	skipUnlessIntegration(t)

	target := redisTarget()
	adapter, err := Get(model.KindRedis)
	if err != nil {
		t.Fatalf("어댑터 없음: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := adapter.Ping(ctx, target); err != nil {
		t.Skipf("접속 불가 (컨테이너 미실행?): %v", err)
	}

	ra := &redisAdapter{}
	client, err := ra.client(target)
	if err != nil {
		t.Fatalf("클라이언트 생성 실패: %v", err)
	}
	defer client.Close()
	// 전용 DB 인덱스이므로 비워도 다른 테스트에 영향이 없다.
	if err := client.FlushDB(ctx).Err(); err != nil {
		t.Fatalf("FLUSHDB 실패: %v", err)
	}
	t.Cleanup(func() { _ = client.FlushDB(context.WithoutCancel(ctx)).Err() })

	// 세 그룹: TTL 있는 문자열, TTL 없는 문자열, 해시.
	// 키 접두사 그룹은 숫자·UUID 세그먼트를 *로 바꿔 묶으므로 ID를 붙인다.
	for i := range 5 {
		if err := client.Set(ctx, fmt.Sprintf("session:%d", i), "v", time.Hour).Err(); err != nil {
			t.Fatalf("SET 실패: %v", err)
		}
	}
	for i := range 3 {
		if err := client.Set(ctx, fmt.Sprintf("cache:%d", i), "v", 0).Err(); err != nil {
			t.Fatalf("SET 실패: %v", err)
		}
	}
	if err := client.HSet(ctx, "user:1", "name", "kim", "age", "30").Err(); err != nil {
		t.Fatalf("HSET 실패: %v", err)
	}
	// 큰 키: 다른 키보다 확실히 큰 값
	big := make([]byte, 40000)
	for i := range big {
		big[i] = 'x'
	}
	if err := client.Set(ctx, "blob:big", string(big), 0).Err(); err != nil {
		t.Fatalf("큰 키 SET 실패: %v", err)
	}

	res, err := DoExplore(ctx, target)
	if err != nil {
		t.Fatalf("DoExplore 실패: %v", err)
	}
	if res.Keyspace == nil {
		t.Fatalf("keyspace 결과가 비어 있습니다")
	}
	k := res.Keyspace

	if k.SelectedDB != 3 {
		t.Errorf("selectedDb = %d, 3이어야 합니다", k.SelectedDB)
	}
	if k.Server["redis_version"] == "" {
		t.Error("서버 버전을 읽지 못했습니다")
	}
	if k.Memory.Used == 0 {
		t.Error("사용 메모리를 읽지 못했습니다")
	}
	if k.Memory.Policy == "" {
		t.Error("메모리 정책을 읽지 못했습니다")
	}
	if k.Stats.TotalCommands == 0 {
		t.Error("총 명령 수를 읽지 못했습니다")
	}
	if k.Scanned != 10 {
		t.Errorf("스캔한 키 = %d, 10이어야 합니다", k.Scanned)
	}
	if k.Truncated {
		t.Error("10개 키에서 truncated가 true입니다")
	}

	// db3의 키 수가 keyspace 섹션에 나타나야 한다.
	var db3 *KeyspaceDB
	for i := range k.Databases {
		if k.Databases[i].Index == 3 {
			db3 = &k.Databases[i]
		}
	}
	if db3 == nil {
		t.Fatalf("db3이 keyspace에 없습니다: %+v", k.Databases)
	}
	if db3.Keys != 10 {
		t.Errorf("db3 키 수 = %d, 10이어야 합니다", db3.Keys)
	}
	if db3.Expires != 5 {
		t.Errorf("db3 만료 설정 키 = %d, 5여야 합니다", db3.Expires)
	}

	groups := map[string]*KeyGroup{}
	for _, g := range k.Groups {
		groups[g.Prefix] = g
	}
	if g := groups["session:*"]; g == nil {
		t.Errorf("session:* 그룹이 없습니다: %+v", groupPrefixes(k))
	} else {
		if g.Keys != 5 {
			t.Errorf("session:* 그룹 키 = %d, 5여야 합니다", g.Keys)
		}
		if g.WithTTL != 5 {
			t.Errorf("session:* 그룹 TTL 있는 키 = %d, 5여야 합니다", g.WithTTL)
		}
		if g.Types["string"] != 5 {
			t.Errorf("session:* 그룹 타입 = %v", g.Types)
		}
	}
	if g := groups["cache:*"]; g == nil {
		t.Errorf("cache:* 그룹이 없습니다: %+v", groupPrefixes(k))
	} else if g.WithTTL != 0 {
		// TTL 없는 캐시 키는 메모리가 계속 늘어나는 원인이므로 정확히 세어야 한다.
		t.Errorf("cache:* 그룹 TTL 있는 키 = %d, 0이어야 합니다", g.WithTTL)
	}
	if g := groups["user:*"]; g == nil {
		t.Errorf("user:* 그룹이 없습니다: %+v", groupPrefixes(k))
	} else if g.Types["hash"] != 1 {
		t.Errorf("user:* 그룹 타입 = %v, hash 1개여야 합니다", g.Types)
	}

	if len(k.BigKeys) == 0 {
		t.Fatal("큰 키 목록이 비어 있습니다")
	}
	if k.BigKeys[0].Key != "blob:big" {
		t.Errorf("가장 큰 키 = %q, blob:big이어야 합니다", k.BigKeys[0].Key)
	}
	if k.BigKeys[0].Bytes < 40000 {
		t.Errorf("blob:big 크기 = %d, 40000 이상이어야 합니다", k.BigKeys[0].Bytes)
	}
	if k.BigKeys[0].TTL != -1 {
		t.Errorf("blob:big TTL = %d, -1(만료 없음)이어야 합니다", k.BigKeys[0].TTL)
	}
	// 크기 내림차순이어야 한다.
	for i := 1; i < len(k.BigKeys); i++ {
		if k.BigKeys[i-1].Bytes < k.BigKeys[i].Bytes {
			t.Errorf("큰 키 정렬이 깨졌습니다: %d < %d",
				k.BigKeys[i-1].Bytes, k.BigKeys[i].Bytes)
		}
	}
}

func groupPrefixes(k *KeyspaceExplore) []string {
	out := make([]string, 0, len(k.Groups))
	for _, g := range k.Groups {
		out = append(out, g.Prefix)
	}
	return out
}

// TestExploreUnsupported는 관계형 DB에서 특화 탐색이 거부되는지 확인한다.
// 빈 결과를 돌려주면 화면이 "읽을 것이 없다"로 오해하게 된다.
func TestExploreUnsupported(t *testing.T) {
	target := Target{
		Conn: &model.Connection{
			Kind: model.KindSQLite, DatabaseName: "x.db", Options: model.Options{},
		},
		Secret: &model.Secret{},
	}
	if _, err := DoExplore(context.Background(), target); err == nil {
		t.Fatal("SQLite에서 DoExplore가 성공했습니다")
	}
}

// TestParseRedisKeyspace는 INFO keyspace 문자열 해석을 검증한다.
// 실제 서버 없이 형식만 확인하는 단위 테스트다.
func TestParseRedisKeyspace(t *testing.T) {
	info := map[string]string{
		"db0":           "keys=1,expires=0,avg_ttl=0",
		"db3":           "keys=120,expires=45,avg_ttl=6000",
		"db_bogus":      "keys=1",
		"redis_version": "7.2.4",
		"used_memory":   "1024",
		"nonsense":      "keys=1",
	}
	dbs := parseRedisKeyspace(info)
	if len(dbs) != 2 {
		t.Fatalf("파싱된 DB = %d개 (%+v), 2개여야 합니다", len(dbs), dbs)
	}
	if dbs[0].Index != 0 || dbs[1].Index != 3 {
		t.Errorf("정렬이 인덱스 순이 아닙니다: %+v", dbs)
	}
	if dbs[1].Keys != 120 || dbs[1].Expires != 45 || dbs[1].AvgTTLMs != 6000 {
		t.Errorf("db3 파싱 결과 = %+v", dbs[1])
	}
}

// TestParseRedisCommandStats는 commandstats 형식과 정렬을 검증한다.
func TestParseRedisCommandStats(t *testing.T) {
	raw := "# Commandstats\r\n" +
		"cmdstat_get:calls=100,usec=250,usec_per_call=2.50,rejected_calls=0,failed_calls=1\r\n" +
		"cmdstat_set:calls=500,usec=2000,usec_per_call=4.00,rejected_calls=3,failed_calls=0\r\n" +
		"cmdstat_info:calls=5,usec=50,usec_per_call=10.00\r\n"
	stats := parseRedisCommandStats(raw)
	if len(stats) != 3 {
		t.Fatalf("파싱된 명령 = %d개, 3개여야 합니다", len(stats))
	}
	if stats[0].Name != "set" {
		t.Errorf("첫 항목 = %q, 호출 수가 가장 많은 set이어야 합니다", stats[0].Name)
	}
	if stats[0].Calls != 500 || stats[0].UsecPerCall != 4 || stats[0].Rejected != 3 {
		t.Errorf("set 파싱 결과 = %+v", stats[0])
	}
	if stats[1].Name != "get" || stats[1].Failed != 1 {
		t.Errorf("get 파싱 결과 = %+v", stats[1])
	}
}

// TestRedisPartialError는 MEMORY USAGE가 막힌 서버에서도 결과를 살리는지 확인한다.
func TestRedisPartialError(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{redis.Nil, true},
		{errString("ERR unknown command 'MEMORY'"), true},
		{errString("NOPERM this user has no permissions to run the 'memory|usage' command"), true},
		{errString("dial tcp: connection refused"), false},
	}
	for _, tc := range cases {
		if got := isRedisPartial(tc.err); got != tc.want {
			t.Errorf("isRedisPartial(%v) = %v, %v를 기대했습니다", tc.err, got, tc.want)
		}
	}
}

type errString string

func (e errString) Error() string { return string(e) }
