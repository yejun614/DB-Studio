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

// 문서 스키마 추론에 사용할 샘플 수. 크면 정확하지만 비싸다.
const mongoSampleSize = 200

// 중첩 문서를 펼칠 최대 깊이. 너무 깊게 들어가면 필드가 폭발한다.
const mongoMaxDepth = 3

// Introspect는 컬렉션별로 문서를 샘플링해 필드 구조를 추론한다.
//
// MongoDB는 스키마가 강제되지 않으므로 이 결과는 "관찰된 구조"이며 정의가 아니다.
// Column.Presence(샘플 중 해당 필드가 존재한 비율)로 그 불확실성을 함께 전달한다.
func (a *mongoAdapter) Introspect(ctx context.Context, t Target) (*schema.Schema, error) {
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

	s := &schema.Schema{
		Dialect:    string(a.Kind()),
		Shape:      schema.ShapeDocument,
		Name:       dbName,
		CapturedAt: time.Now().UTC(),
		Tables:     []*schema.Table{},
		Views:      []*schema.View{},
	}
	s.AddNote("MongoDB는 스키마가 강제되지 않습니다. 아래 구조는 컬렉션별 최대 %d개 문서를 샘플링해 추론한 결과입니다", mongoSampleSize)

	db := client.Database(dbName)
	all, err := db.ListCollectionNames(ctx, bson.D{})
	if err != nil {
		return nil, fmt.Errorf("컬렉션 목록 조회 실패: %w", err)
	}
	names, skipped := filterSystemCollections(all)
	if skipped > 0 {
		s.AddNote("MongoDB 내부 컬렉션 %d개(system.*)는 제외했습니다", skipped)
	}
	sort.Strings(names)

	for _, name := range names {
		coll := db.Collection(name)
		tbl := &schema.Table{
			Name:    name,
			Columns: []*schema.Column{}, Indexes: []*schema.Index{},
			ForeignKeys: []*schema.ForeignKey{}, Checks: []*schema.Check{},
			Options: map[string]string{},
		}
		s.Tables = append(s.Tables, tbl)

		if n, err := coll.EstimatedDocumentCount(ctx); err == nil {
			tbl.RowEstimate = n
		}
		if err := mongoInferFields(ctx, coll, tbl, s); err != nil {
			s.AddNote("%s 컬렉션 샘플링 실패: %v", name, err)
		}
		if err := mongoCollectionIndexes(ctx, coll, tbl); err != nil {
			s.AddNote("%s 컬렉션 인덱스 조회 실패: %v", name, err)
		}
	}

	s.Sort()
	return s, nil
}

// filterSystemCollections는 MongoDB 내부 컬렉션을 걸러낸다.
//
// system.profile(프로파일러 기록), system.views(뷰 정의) 같은 것은 사용자 데이터가
// 아니다. 목록에 섞이면 컬렉션 수와 크기가 실제와 달라 보이고, system.profile은
// 문서마다 형태가 달라 필드가 수백 개로 추론되어 화면을 덮는다. 접근 권한도 없는
// 경우가 많아 조회 실패 메시지만 남는다.
func filterSystemCollections(names []string) ([]string, int) {
	out := make([]string, 0, len(names))
	skipped := 0
	for _, name := range names {
		if strings.HasPrefix(name, "system.") {
			skipped++
			continue
		}
		out = append(out, name)
	}
	return out, skipped
}

// mongoInferFields는 샘플 문서에서 필드 경로와 타입을 모은다.
func mongoInferFields(ctx context.Context, coll *mongo.Collection, tbl *schema.Table, s *schema.Schema) error {
	cur, err := coll.Find(ctx, bson.D{}, options.Find().SetLimit(mongoSampleSize))
	if err != nil {
		return fmt.Errorf("find: %w", err)
	}
	defer cur.Close(ctx)

	// 필드 경로 → 관찰된 타입 집합과 등장 횟수
	type fieldInfo struct {
		types map[string]int
		count int
		order int // 처음 관찰된 순서. 컬럼 위치를 안정적으로 만든다.
	}
	fields := map[string]*fieldInfo{}
	sampled := 0
	nextOrder := 0

	for cur.Next(ctx) {
		var doc bson.D
		if err := cur.Decode(&doc); err != nil {
			return fmt.Errorf("decode: %w", err)
		}
		sampled++
		observed := map[string]string{}
		collectMongoFields("", doc, 1, observed)
		for path, typeName := range observed {
			fi := fields[path]
			if fi == nil {
				fi = &fieldInfo{types: map[string]int{}, order: nextOrder}
				nextOrder++
				fields[path] = fi
			}
			fi.count++
			fi.types[typeName]++
		}
	}
	if err := cur.Err(); err != nil {
		return fmt.Errorf("cursor: %w", err)
	}
	if sampled == 0 {
		s.AddNote("%s 컬렉션이 비어 있어 필드를 추론할 수 없습니다", tbl.Name)
		return nil
	}
	tbl.Options["sampled"] = strconv.Itoa(sampled)

	paths := make([]string, 0, len(fields))
	for p := range fields {
		paths = append(paths, p)
	}
	// 경로 사전순으로 정렬해 중첩 필드가 부모 바로 아래 오도록 한다.
	sort.Strings(paths)

	for i, path := range paths {
		fi := fields[path]
		typeName, mixed := dominantType(fi.types)
		lt := schema.ParseType("mongodb", typeName)
		if mixed {
			// 타입이 섞인 필드는 마이그레이션 대상으로 삼기 위험하므로 명시한다.
			lt = schema.LogicalType{Base: schema.TypeUnknown}
		}
		col := &schema.Column{
			Name: path, Position: i + 1,
			Type: lt, RawType: typeName,
			// 모든 샘플에 존재하지 않으면 사실상 nullable이다.
			Nullable: fi.count < sampled,
			Presence: float64(fi.count) / float64(sampled),
		}
		if mixed {
			col.Comment = "혼합 타입: " + typeSummary(fi.types)
		}
		tbl.Columns = append(tbl.Columns, col)

		// _id는 항상 존재하는 기본키다.
		if path == "_id" {
			tbl.PrimaryKey = &schema.PrimaryKey{Columns: []string{"_id"}}
		}
	}
	return nil
}

// collectMongoFields는 문서를 재귀적으로 훑어 "a.b.c" 형태의 경로와 타입을 모은다.
func collectMongoFields(prefix string, doc bson.D, depth int, out map[string]string) {
	for _, elem := range doc {
		path := elem.Key
		if prefix != "" {
			path = prefix + "." + elem.Key
		}
		typeName := mongoTypeName(elem.Value)
		out[path] = typeName

		if depth >= mongoMaxDepth {
			continue
		}
		switch v := elem.Value.(type) {
		case bson.D:
			collectMongoFields(path, v, depth+1, out)
		case bson.A:
			// 배열 원소가 문서면 첫 원소의 구조만 대표로 본다.
			// 원소마다 다른 구조를 전부 펼치면 필드 수가 폭발한다.
			if len(v) > 0 {
				if inner, ok := v[0].(bson.D); ok {
					collectMongoFields(path+"[]", inner, depth+1, out)
				}
			}
		}
	}
}

func mongoTypeName(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case int32:
		return "int"
	case int64:
		return "long"
	case float64:
		return "double"
	case string:
		return "string"
	case bson.ObjectID:
		return "objectid"
	case bson.DateTime:
		return "timestamp"
	case bson.Decimal128:
		return "decimal128"
	case bson.Binary:
		return "binary"
	case bson.D:
		return "document"
	case bson.A:
		return "array"
	case bson.Regex:
		return "regex"
	}
	return "mixed"
}

// dominantType은 가장 많이 관찰된 타입과, 타입이 섞였는지를 반환한다.
// null만 다른 경우는 "섞였다"고 보지 않는다 — 값이 없는 것은 타입 충돌이 아니다.
func dominantType(types map[string]int) (string, bool) {
	best, bestCount := "", 0
	nonNull := 0
	for name, count := range types {
		if name != "null" {
			nonNull++
		}
		if count > bestCount || (count == bestCount && name < best) {
			best, bestCount = name, count
		}
	}
	if best == "null" && nonNull > 0 {
		// null이 최다여도 실제 타입이 있으면 그것을 대표로 쓴다.
		for name := range types {
			if name != "null" {
				best = name
				break
			}
		}
	}
	return best, nonNull > 1
}

func typeSummary(types map[string]int) string {
	names := make([]string, 0, len(types))
	for n := range types {
		names = append(names, n)
	}
	sort.Strings(names)
	parts := make([]string, len(names))
	for i, n := range names {
		parts[i] = fmt.Sprintf("%s×%d", n, types[n])
	}
	return strings.Join(parts, ", ")
}

func mongoCollectionIndexes(ctx context.Context, coll *mongo.Collection, tbl *schema.Table) error {
	cur, err := coll.Indexes().List(ctx)
	if err != nil {
		return fmt.Errorf("list indexes: %w", err)
	}
	defer cur.Close(ctx)

	for cur.Next(ctx) {
		var spec bson.M
		if err := cur.Decode(&spec); err != nil {
			return fmt.Errorf("decode index: %w", err)
		}
		name, _ := spec["name"].(string)
		if name == "" {
			continue
		}
		idx := &schema.Index{Name: name, Columns: []schema.IndexPart{}}
		if u, ok := spec["unique"].(bool); ok {
			idx.Unique = u
		}
		// 인덱스 키는 순서가 의미를 가지므로 bson.D를 그대로 쓰는 것이 정확하다.
		// bson.M으로만 단정하면 드라이버가 bson.D를 돌려줄 때 조용히 실패한다.
		if keys, ok := spec["key"].(bson.D); ok {
			for _, e := range keys {
				idx.Columns = append(idx.Columns, schema.IndexPart{
					Column: e.Key, Descending: bsonNegative(e.Value),
				})
			}
		} else if keys := bsonDoc(spec["key"]); keys != nil {
			// 맵으로 온 경우에는 순서를 잃으므로 이름순으로 정렬해 결정적으로 만든다.
			names := make([]string, 0, len(keys))
			for k := range keys {
				names = append(names, k)
			}
			sort.Strings(names)
			for _, k := range names {
				idx.Columns = append(idx.Columns, schema.IndexPart{
					Column: k, Descending: bsonNegative(keys[k]),
				})
			}
		}
		// _id_ 인덱스는 기본키로 이미 표현된다.
		if name == "_id_" {
			continue
		}
		tbl.Indexes = append(tbl.Indexes, idx)
	}
	if err := cur.Err(); err != nil {
		return fmt.Errorf("index cursor: %w", err)
	}
	return nil
}
