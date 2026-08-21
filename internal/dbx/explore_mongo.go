package dbx

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"dbstudio/internal/schema"
)

// ExploreDocument는 컬렉션별 저장 통계·인덱스 상세·필드 분포를 모은다.
//
// Introspect(P3)와 겹치는 부분(필드 추론)은 같은 헬퍼를 재사용한다. 겹치지 않는 것,
// 즉 저장 크기·인덱스 크기·인덱스 사용 횟수는 스키마 IR에 담을 자리가 없어서
// 이 경로에서만 얻을 수 있다.
func (a *mongoAdapter) ExploreDocument(ctx context.Context, t Target) (*DocumentExplore, []string, error) {
	uri, err := a.uri(t)
	if err != nil {
		return nil, nil, err
	}
	dbName := strings.TrimSpace(t.Conn.DatabaseName)
	if dbName == "" {
		return nil, nil, fmt.Errorf("데이터베이스 이름이 필요합니다")
	}

	opts := options.Client().ApplyURI(uri).
		SetServerSelectionTimeout(10 * time.Second).
		SetAppName("dbstudio")
	client, err := mongo.Connect(opts)
	if err != nil {
		return nil, nil, fmt.Errorf("접속 설정 오류: %w", err)
	}
	defer func() { _ = client.Disconnect(context.WithoutCancel(ctx)) }()

	var notes []string
	note := func(format string, args ...any) {
		notes = append(notes, fmt.Sprintf(format, args...))
	}

	out := &DocumentExplore{
		Database:    dbName,
		SampleSize:  mongoSampleSize,
		Server:      map[string]string{},
		Collections: []*DocumentCollection{},
	}
	db := client.Database(dbName)

	// 서버 정보: 버전과 스토리지 엔진은 화면에서 판단의 전제가 된다
	// (예: 압축 여부, 트랜잭션 지원).
	var buildInfo bson.M
	if err := client.Database("admin").
		RunCommand(ctx, bson.D{{Key: "buildInfo", Value: 1}}).Decode(&buildInfo); err == nil {
		if v, ok := buildInfo["version"].(string); ok {
			out.Server["version"] = v
		}
	} else {
		note("buildInfo를 읽지 못했습니다: %v", err)
	}
	var serverStatus bson.M
	if err := client.Database("admin").
		RunCommand(ctx, bson.D{{Key: "serverStatus", Value: 1}}).Decode(&serverStatus); err == nil {
		if se := bsonDoc(serverStatus["storageEngine"]); se != nil {
			if v, ok := se["name"].(string); ok {
				out.Server["storageEngine"] = v
			}
		}
		if v, ok := serverStatus["host"].(string); ok {
			out.Server["host"] = v
		}
		if repl := bsonDoc(serverStatus["repl"]); repl != nil {
			if v, ok := repl["setName"].(string); ok {
				out.Server["replicaSet"] = v
			}
			if v, ok := repl["ismaster"].(bool); ok {
				out.Server["primary"] = strconv.FormatBool(v)
			}
		}
		if v, ok := bsonFloat(serverStatus["uptime"]); ok {
			out.Server["uptimeSeconds"] = strconv.FormatInt(int64(v), 10)
		}
	} else {
		// serverStatus는 clusterMonitor 역할을 요구한다. 없어도 나머지는 읽을 수 있다.
		note("serverStatus를 읽지 못했습니다 (clusterMonitor 역할 필요): %v", err)
	}

	var dbStats bson.M
	if err := db.RunCommand(ctx, bson.D{{Key: "dbStats", Value: 1}}).Decode(&dbStats); err == nil {
		out.Stats = DocumentDBStats{
			Collections: int(bsonInt(dbStats["collections"])),
			Views:       int(bsonInt(dbStats["views"])),
			Objects:     bsonInt(dbStats["objects"]),
			DataSize:    bsonInt(dbStats["dataSize"]),
			StorageSize: bsonInt(dbStats["storageSize"]),
			IndexSize:   bsonInt(dbStats["indexSize"]),
			AvgObjSize:  bsonInt(dbStats["avgObjSize"]),
			Indexes:     int(bsonInt(dbStats["indexes"])),
		}
	} else {
		note("dbStats를 읽지 못했습니다: %v", err)
	}

	// listCollections는 뷰·시계열 컬렉션을 구분할 수 있는 유일한 경로다.
	kinds, viewOn, err := mongoCollectionKinds(ctx, db)
	if err != nil {
		return nil, notes, fmt.Errorf("컬렉션 목록 조회 실패: %w", err)
	}
	all := make([]string, 0, len(kinds))
	for name := range kinds {
		all = append(all, name)
	}
	// 스키마 화면과 같은 기준으로 내부 컬렉션을 제외한다 —
	// 두 화면이 다른 컬렉션 목록을 보여주면 어느 쪽을 믿어야 할지 알 수 없다.
	names, skipped := filterSystemCollections(all)
	if skipped > 0 {
		note("MongoDB 내부 컬렉션 %d개(system.*)는 제외했습니다", skipped)
	}
	sort.Strings(names)

	for _, name := range names {
		item := &DocumentCollection{
			Name:    name,
			Type:    kinds[name],
			ViewOn:  viewOn[name],
			Fields:  []*DocumentField{},
			Indexes: []*DocumentIndex{},
		}
		out.Collections = append(out.Collections, item)

		// 뷰는 자체 저장 공간과 인덱스가 없다. collStats를 호출하면 오류가 나므로
		// 원본 컬렉션만 알려주고 넘어간다.
		if item.Type == "view" {
			item.Note = "뷰입니다. 저장 공간과 인덱스는 원본 컬렉션에 있습니다"
			continue
		}

		coll := db.Collection(name)
		indexSizes, err := mongoCollStats(ctx, db, name, item)
		if err != nil {
			note("%s collStats 실패: %v", name, err)
		}
		if item.Documents == 0 {
			// collStats를 읽지 못한 경우의 대체 경로.
			if n, err := coll.EstimatedDocumentCount(ctx); err == nil {
				item.Documents = n
			}
		}
		if err := mongoExploreFields(ctx, coll, item); err != nil {
			note("%s 필드 샘플링 실패: %v", name, err)
		}
		if err := mongoExploreIndexes(ctx, coll, item, indexSizes); err != nil {
			note("%s 인덱스 조회 실패: %v", name, err)
		}
	}

	if len(out.Collections) == 0 {
		note("컬렉션이 없습니다")
	}
	return out, notes, nil
}

// mongoCollectionKinds는 이름 → 종류(collection/view/timeseries)를 반환한다.
func mongoCollectionKinds(ctx context.Context, db *mongo.Database) (map[string]string, map[string]string, error) {
	cur, err := db.ListCollections(ctx, bson.D{})
	if err != nil {
		return nil, nil, err
	}
	defer cur.Close(ctx)

	kinds := map[string]string{}
	viewOn := map[string]string{}
	for cur.Next(ctx) {
		var spec bson.M
		if err := cur.Decode(&spec); err != nil {
			return nil, nil, err
		}
		name, _ := spec["name"].(string)
		if name == "" {
			continue
		}
		kind, _ := spec["type"].(string)
		if kind == "" {
			kind = "collection"
		}
		kinds[name] = kind
		if opts := bsonDoc(spec["options"]); opts != nil {
			if v, ok := opts["viewOn"].(string); ok {
				viewOn[name] = v
			}
		}
	}
	if err := cur.Err(); err != nil {
		return nil, nil, err
	}
	return kinds, viewOn, nil
}

// mongoCollStats는 컬렉션 저장 통계를 채우고, 인덱스별 크기 맵을 반환한다.
//
// 인덱스 크기를 반환값으로 넘기는 이유: collStats와 listIndexes는 별개 호출이고,
// 둘을 한 함수로 합치면 실패 지점이 섞여 "크기를 못 읽었다"와 "인덱스를 못 읽었다"를
// 구분할 수 없다. 공용 저장소에 두면 동시 요청끼리 섞인다.
func mongoCollStats(ctx context.Context, db *mongo.Database, name string, item *DocumentCollection) (map[string]any, error) {
	var stats bson.M
	if err := db.RunCommand(ctx, bson.D{{Key: "collStats", Value: name}}).Decode(&stats); err != nil {
		return nil, err
	}
	item.Documents = bsonInt(stats["count"])
	item.DataSize = bsonInt(stats["size"])
	item.StorageSize = bsonInt(stats["storageSize"])
	item.AvgObjSize = bsonInt(stats["avgObjSize"])
	item.IndexSize = bsonInt(stats["totalIndexSize"])
	if v, ok := stats["capped"].(bool); ok {
		item.Capped = v
	}
	// 인덱스별 크기는 이 응답에만 있다(listIndexes는 크기를 주지 않는다).
	return bsonDoc(stats["indexSizes"]), nil
}

func mongoExploreFields(ctx context.Context, coll *mongo.Collection, item *DocumentCollection) error {
	// Introspect와 같은 추론 로직을 쓴다 — 두 화면이 다른 답을 내면 안 된다.
	tbl := &schema.Table{Name: item.Name, Columns: []*schema.Column{}, Options: map[string]string{}}
	tmp := &schema.Schema{}
	if err := mongoInferFields(ctx, coll, tbl, tmp); err != nil {
		return err
	}
	if v, err := strconv.Atoi(tbl.Options["sampled"]); err == nil {
		item.Sampled = v
	}
	for _, col := range tbl.Columns {
		f := &DocumentField{
			Path:     col.Name,
			Type:     col.RawType,
			Presence: col.Presence,
		}
		// Introspect는 혼합 타입을 Comment에 "혼합 타입: ..." 으로 남긴다.
		if rest, found := strings.CutPrefix(col.Comment, "혼합 타입: "); found {
			f.Mixed = true
			f.Types = rest
		}
		item.Fields = append(item.Fields, f)
	}
	if item.Sampled == 0 && len(tmp.Notes) > 0 {
		item.Note = tmp.Notes[0]
	}
	return nil
}

func mongoExploreIndexes(ctx context.Context, coll *mongo.Collection, item *DocumentCollection, sizes map[string]any) error {
	cur, err := coll.Indexes().List(ctx)
	if err != nil {
		return err
	}
	defer cur.Close(ctx)

	for cur.Next(ctx) {
		var spec bson.M
		if err := cur.Decode(&spec); err != nil {
			return err
		}
		name, _ := spec["name"].(string)
		if name == "" {
			continue
		}
		idx := &DocumentIndex{Name: name, Keys: mongoIndexKeys(spec["key"])}
		if v, ok := spec["unique"].(bool); ok {
			idx.Unique = v
		}
		if v, ok := spec["sparse"].(bool); ok {
			idx.Sparse = v
		}
		if _, ok := spec["partialFilterExpression"]; ok {
			idx.Partial = true
		}
		if v, ok := bsonFloat(spec["expireAfterSeconds"]); ok {
			ttl := int64(v)
			idx.TTLSecond = &ttl
		}
		if sizes != nil {
			idx.SizeBytes = bsonInt(sizes[name])
		}
		item.Indexes = append(item.Indexes, idx)
	}
	if err := cur.Err(); err != nil {
		return err
	}

	// $indexStats로 사용 횟수를 채운다. 권한이 없으면 조용히 넘어간다 —
	// Ops가 nil이면 화면이 "확인 불가"로 표시하므로 0과 혼동되지 않는다.
	usage, err := mongoIndexUsage(ctx, coll)
	if err == nil {
		for _, idx := range item.Indexes {
			if u, ok := usage[idx.Name]; ok {
				ops := u.ops
				idx.Ops = &ops
				if !u.since.IsZero() {
					since := u.since
					idx.Since = &since
				}
			}
		}
	}
	sort.Slice(item.Indexes, func(i, j int) bool { return item.Indexes[i].Name < item.Indexes[j].Name })
	return nil
}

type indexUsage struct {
	ops   int64
	since time.Time
}

func mongoIndexUsage(ctx context.Context, coll *mongo.Collection) (map[string]indexUsage, error) {
	cur, err := coll.Aggregate(ctx, mongo.Pipeline{
		{{Key: "$indexStats", Value: bson.D{}}},
	})
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)

	out := map[string]indexUsage{}
	for cur.Next(ctx) {
		var row bson.M
		if err := cur.Decode(&row); err != nil {
			return nil, err
		}
		name, _ := row["name"].(string)
		if name == "" {
			continue
		}
		u := indexUsage{}
		if accesses := bsonDoc(row["accesses"]); accesses != nil {
			u.ops = bsonInt(accesses["ops"])
			if dt, ok := accesses["since"].(bson.DateTime); ok {
				u.since = dt.Time().UTC()
			}
		}
		out[name] = u
	}
	return out, cur.Err()
}

// mongoIndexKeys는 인덱스 키를 "field ASC, other DESC" 문자열로 만든다.
// 순서가 의미를 가지므로 bson.D를 우선 처리한다.
func mongoIndexKeys(v any) string {
	parts := []string{}
	if keys, ok := v.(bson.D); ok {
		for _, e := range keys {
			parts = append(parts, mongoKeyPart(e.Key, e.Value))
		}
		return strings.Join(parts, ", ")
	}
	if keys := bsonDoc(v); keys != nil {
		names := make([]string, 0, len(keys))
		for k := range keys {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			parts = append(parts, mongoKeyPart(k, keys[k]))
		}
	}
	return strings.Join(parts, ", ")
}

func mongoKeyPart(field string, dir any) string {
	// 방향이 숫자가 아닌 인덱스도 있다(text, 2dsphere, hashed).
	// 그 경우 값 자체가 종류이므로 그대로 보여주는 것이 정확하다.
	if s, ok := dir.(string); ok {
		return field + " " + s
	}
	if bsonNegative(dir) {
		return field + " DESC"
	}
	return field + " ASC"
}

// bsonInt는 BSON 숫자(int32/int64/double)를 int64로 읽는다.
func bsonInt(v any) int64 {
	if f, ok := bsonFloat(v); ok {
		return int64(f)
	}
	return 0
}
