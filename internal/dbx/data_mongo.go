package dbx

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// MongoDB의 데이터 조회·수정.
//
// 관계형과 달리 문서에는 고정된 컬럼이 없다. 그래서 표를 그리려면 두 가지 중 하나를
// 골라야 한다: (1) 표본에서 필드를 모아 열을 만들거나, (2) 문서를 통째로 한 칸에 넣거나.
//
// 여기서는 **둘 다 한다.** 화면이 표로 볼 때는 표본에서 모은 필드가 열이 되고,
// 각 행에는 원본 문서 JSON도 함께 실려 온다(_raw 열). 표만 주면 표본에 없던 필드가
// 조용히 사라져서 "저장했더니 필드가 없어졌다"가 되고, JSON만 주면 100개 문서를
// 눈으로 훑어야 한다.
//
// 수정은 언제나 _id로만 한다. 관계형에서 기본키를 요구하는 것과 같은 이유다.

// rawField는 원본 문서 JSON이 담기는 가상 컬럼 이름이다.
// 실제 필드 이름과 부딪히지 않도록 몽고가 금지하는 접두사($)를 쓴다.
const rawField = "$document"

func (a *mongoAdapter) DataCapabilities() DataCapabilities {
	return DataCapabilities{
		Browse: true, Filter: true, Sort: true, Mutate: true, Statement: true,
		StatementLabel: "명령",
		StatementHelp: "MongoDB 명령을 JSON으로 씁니다. 예: {\"find\": \"users\", \"limit\": 10} " +
			"또는 {\"aggregate\": \"orders\", \"pipeline\": [], \"cursor\": {}}",
	}
}

// mongoSampleFields는 열을 만들기 위해 훑는 문서 수다.
// introspect(200개)보다 적게 잡는다 — 여기서는 정확한 스키마 추론이 아니라
// "이 컬렉션을 표로 보면 어떤 열이 보여야 하는가"만 정하면 되고, 화면을 여는
// 동작이므로 빨라야 한다.
const mongoSampleFields = 50

func (a *mongoAdapter) connect(ctx context.Context, t Target) (*mongo.Client, string, error) {
	uri, err := a.uri(t)
	if err != nil {
		return nil, "", err
	}
	dbName := strings.TrimSpace(t.Conn.DatabaseName)
	if dbName == "" {
		return nil, "", fmt.Errorf("데이터베이스 이름이 필요합니다")
	}
	opts := options.Client().ApplyURI(uri).
		SetServerSelectionTimeout(10 * time.Second).
		SetAppName("dbstudio")
	client, err := mongo.Connect(opts)
	if err != nil {
		return nil, "", fmt.Errorf("접속 설정 오류: %w", err)
	}
	return client, dbName, nil
}

func (a *mongoAdapter) ListObjects(ctx context.Context, t Target) ([]DataObject, error) {
	client, dbName, err := a.connect(ctx, t)
	if err != nil {
		return nil, err
	}
	defer func() { _ = client.Disconnect(context.WithoutCancel(ctx)) }()

	db := client.Database(dbName)
	cursor, err := db.ListCollections(ctx, bson.D{})
	if err != nil {
		return nil, fmt.Errorf("컬렉션 목록 조회 실패: %w", err)
	}
	defer cursor.Close(ctx)

	out := []DataObject{}
	for cursor.Next(ctx) {
		var spec bson.M
		if err := cursor.Decode(&spec); err != nil {
			continue
		}
		name, _ := spec["name"].(string)
		if name == "" {
			continue
		}
		kind := "collection"
		if v, _ := spec["type"].(string); v == "view" {
			kind = "view"
		}
		obj := DataObject{Namespace: dbName, Name: name, Kind: kind, RowCount: -1}
		// 추정 개수는 메타데이터에서 읽으므로 컬렉션을 훑지 않는다.
		if n, err := db.Collection(name).EstimatedDocumentCount(ctx); err == nil {
			obj.RowCount = n
		}
		out = append(out, obj)
	}
	if err := cursor.Err(); err != nil {
		return nil, fmt.Errorf("컬렉션 목록 순회 실패: %w", err)
	}
	return out, nil
}

func (a *mongoAdapter) QueryRows(ctx context.Context, t Target, q RowQuery) (*RowPage, error) {
	if q.Table.Empty() {
		return nil, fmt.Errorf("대상 컬렉션을 지정하세요")
	}
	client, dbName, err := a.connect(ctx, t)
	if err != nil {
		return nil, err
	}
	defer func() { _ = client.Disconnect(context.WithoutCancel(ctx)) }()

	coll := client.Database(dbName).Collection(q.Table.Name)
	limit := q.EffectiveLimit()

	filter, err := mongoFilter(q.Filters, q.Search)
	if err != nil {
		return nil, err
	}

	findOpts := options.Find().
		SetLimit(int64(limit) + 1).
		SetSkip(int64(q.Offset))
	if q.OrderBy != "" {
		dir := 1
		if q.Desc {
			dir = -1
		}
		findOpts.SetSort(bson.D{{Key: q.OrderBy, Value: dir}})
	} else {
		// _id 순서는 삽입 순서와 대략 같고 항상 존재하므로 기본 정렬로 안전하다.
		findOpts.SetSort(bson.D{{Key: "_id", Value: 1}})
	}

	start := time.Now()
	cursor, err := coll.Find(ctx, filter, findOpts)
	if err != nil {
		return nil, fmt.Errorf("조회 실패: %w", err)
	}
	defer cursor.Close(ctx)

	docs := []bson.M{}
	for cursor.Next(ctx) {
		var doc bson.M
		if err := cursor.Decode(&doc); err != nil {
			return nil, fmt.Errorf("문서 디코딩 실패: %w", err)
		}
		docs = append(docs, doc)
	}
	if err := cursor.Err(); err != nil {
		return nil, fmt.Errorf("문서 순회 실패: %w", err)
	}
	hasMore := false
	if len(docs) > limit {
		docs = docs[:limit]
		hasMore = true
	}
	elapsed := float64(time.Since(start).Microseconds()) / 1000

	// 열 구성: 이 페이지의 문서에서 필드를 모은다. 표본을 따로 뜨지 않는 이유는
	// 사용자가 보는 것이 이 페이지이기 때문이다 — 다른 표본에서 온 열이 이 페이지에서
	// 전부 비어 있으면 오히려 혼란스럽다.
	fields := mongoFieldOrder(docs)
	cols := make([]DataColumn, 0, len(fields)+1)
	for _, f := range fields {
		cols = append(cols, DataColumn{Name: f, Type: "document field", Nullable: true, PK: f == "_id"})
	}
	cols = append(cols, DataColumn{Name: rawField, Type: "json", Nullable: false})

	rows := make([][]any, 0, len(docs))
	truncated := [][2]int{}
	for i, doc := range docs {
		row := make([]any, len(cols))
		for j, f := range fields {
			v, cut := normalizeValue(mongoScalar(doc[f]), !q.Full)
			row[j] = v
			if cut {
				truncated = append(truncated, [2]int{i, j})
			}
		}
		raw, err := mongoJSON(doc)
		if err != nil {
			raw = fmt.Sprintf("(직렬화 실패: %v)", err)
		}
		v, cut := normalizeValue(raw, !q.Full)
		row[len(cols)-1] = v
		if cut {
			truncated = append(truncated, [2]int{i, len(cols) - 1})
		}
		rows = append(rows, row)
	}

	page := &RowPage{
		Columns: cols, Rows: rows, PrimaryKey: []string{"_id"}, Truncated: truncated,
		Total: -1, Offset: q.Offset, Limit: limit, HasMore: hasMore,
		ElapsedMs: elapsed, Editable: true,
	}
	if q.WithTotal {
		if n, err := coll.CountDocuments(ctx, filter); err == nil {
			page.Total = n
		} else {
			page.Notes = append(page.Notes, "전체 문서 수를 세지 못했습니다: "+err.Error())
		}
	}
	return page, nil
}

// mongoFieldOrder는 표의 열 순서를 정한다.
// _id를 맨 앞에 두고, 나머지는 처음 나타난 순서를 지킨다 — 알파벳 순으로 정렬하면
// 문서의 원래 구조(중요한 필드가 앞에 오는 관습)가 사라진다.
func mongoFieldOrder(docs []bson.M) []string {
	seen := map[string]bool{"_id": true}
	order := []string{"_id"}
	for _, doc := range docs {
		for k := range doc {
			if !seen[k] {
				seen[k] = true
				order = append(order, k)
			}
		}
	}
	return order
}

// mongoScalar는 문서 필드 하나를 표 칸에 넣을 값으로 바꾼다.
// 중첩 문서와 배열은 JSON 문자열로 만든다 — 표 안에 표를 그릴 수는 없다.
func mongoScalar(v any) any {
	switch val := v.(type) {
	case nil:
		return nil
	case bson.M, bson.D, bson.A, []any, map[string]any:
		s, err := mongoJSON(val)
		if err != nil {
			return fmt.Sprint(val)
		}
		return s
	case bson.ObjectID:
		return val.Hex()
	case bson.DateTime:
		return val.Time().Format(time.RFC3339Nano)
	default:
		return v
	}
}

// mongoJSON은 BSON 값을 확장 JSON(relaxed)으로 만든다.
//
// 표준 encoding/json을 쓰지 않는 이유: ObjectID·Decimal128·Timestamp 같은 BSON
// 전용 타입이 의미를 잃는다. relaxed 확장 JSON은 사람이 읽을 수 있으면서
// 타입 정보를 유지하고, 무엇보다 그대로 다시 파싱해 저장할 수 있다.
func mongoJSON(v any) (string, error) {
	b, err := bson.MarshalExtJSON(v, false, false)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// mongoFilter는 필터와 검색어를 몽고 질의 문서로 만든다.
func mongoFilter(filters []Filter, search string) (bson.D, error) {
	conds := []bson.D{}
	for _, f := range filters {
		if !f.Op.Valid() {
			return nil, fmt.Errorf("알 수 없는 조건입니다: %s", f.Op)
		}
		if strings.TrimSpace(f.Column) == "" {
			return nil, fmt.Errorf("조건의 필드 이름이 비어 있습니다")
		}
		value := mongoValue(f.Column, f.Value)
		switch f.Op {
		case OpEq:
			conds = append(conds, bson.D{{Key: f.Column, Value: value}})
		case OpNe:
			conds = append(conds, bson.D{{Key: f.Column, Value: bson.D{{Key: "$ne", Value: value}}}})
		case OpLt:
			conds = append(conds, bson.D{{Key: f.Column, Value: bson.D{{Key: "$lt", Value: value}}}})
		case OpLte:
			conds = append(conds, bson.D{{Key: f.Column, Value: bson.D{{Key: "$lte", Value: value}}}})
		case OpGt:
			conds = append(conds, bson.D{{Key: f.Column, Value: bson.D{{Key: "$gt", Value: value}}}})
		case OpGte:
			conds = append(conds, bson.D{{Key: f.Column, Value: bson.D{{Key: "$gte", Value: value}}}})
		case OpContains:
			conds = append(conds, bson.D{{Key: f.Column, Value: bson.D{
				{Key: "$regex", Value: regexpQuote(f.Value)}, {Key: "$options", Value: "i"},
			}}})
		case OpPrefix:
			conds = append(conds, bson.D{{Key: f.Column, Value: bson.D{
				{Key: "$regex", Value: "^" + regexpQuote(f.Value)}, {Key: "$options", Value: "i"},
			}}})
		case OpIsNull:
			conds = append(conds, bson.D{{Key: f.Column, Value: bson.D{{Key: "$eq", Value: nil}}}})
		case OpNotNull:
			conds = append(conds, bson.D{{Key: f.Column, Value: bson.D{{Key: "$ne", Value: nil}}}})
		}
	}

	if s := strings.TrimSpace(search); s != "" {
		// 몽고에는 "모든 필드 검색"이 없다. $text는 텍스트 인덱스를 요구하고,
		// 인덱스가 없는 컬렉션에서는 그냥 실패한다. 그래서 $where 대신
		// 문서 전체를 문자열로 만들어 비교하는 표현식을 쓴다 — 느리지만
		// 인덱스 유무에 관계없이 동작하고, 사용자가 기대하는 결과를 준다.
		conds = append(conds, bson.D{{Key: "$expr", Value: bson.D{{Key: "$regexMatch", Value: bson.D{
			{Key: "input", Value: bson.D{{Key: "$toString", Value: "$$ROOT"}}},
			{Key: "regex", Value: regexpQuote(s)},
			{Key: "options", Value: "i"},
		}}}}})
	}

	switch len(conds) {
	case 0:
		return bson.D{}, nil
	case 1:
		return conds[0], nil
	default:
		return bson.D{{Key: "$and", Value: conds}}, nil
	}
}

// mongoValue는 문자열로 온 값을 필드에 맞는 BSON 값으로 바꾼다.
// _id는 ObjectID인 경우가 압도적이므로 24자리 16진 문자열이면 변환한다 —
// 그러지 않으면 목록에서 복사한 _id로 검색해도 아무것도 나오지 않는다.
func mongoValue(field, value string) any {
	if field == "_id" || strings.HasSuffix(field, "._id") {
		if id, err := bson.ObjectIDFromHex(value); err == nil {
			return id
		}
	}
	return value
}

// regexpQuote는 검색어를 정규식 리터럴로 만든다.
// 사용자가 친 "a.b"가 "a<아무거나>b"로 해석되면 검색 결과를 설명할 수 없다.
func regexpQuote(s string) string {
	var b strings.Builder
	for _, r := range s {
		if strings.ContainsRune(`\.+*?()|[]{}^$`, r) {
			b.WriteRune('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

func (a *mongoAdapter) MutateRow(ctx context.Context, t Target, m RowMutation) (*MutationResult, error) {
	if m.Table.Empty() {
		return nil, fmt.Errorf("대상 컬렉션을 지정하세요")
	}
	client, dbName, err := a.connect(ctx, t)
	if err != nil {
		return nil, err
	}
	defer func() { _ = client.Disconnect(context.WithoutCancel(ctx)) }()

	coll := client.Database(dbName).Collection(m.Table.Name)

	switch m.Action {
	case "insert":
		doc, err := mongoDocFromValues(m.Values)
		if err != nil {
			return nil, err
		}
		res, err := coll.InsertOne(ctx, doc)
		if err != nil {
			return nil, fmt.Errorf("추가 실패: %w", err)
		}
		return &MutationResult{
			Affected: 1,
			Statement: fmt.Sprintf("db.%s.insertOne(…) → _id=%v",
				m.Table.Name, mongoScalar(res.InsertedID)),
		}, nil

	case "update":
		id, err := mongoID(m.Key)
		if err != nil {
			return nil, err
		}
		doc, err := mongoDocFromValues(m.Values)
		if err != nil {
			return nil, err
		}
		// _id는 바꿀 수 없다. 요청에 들어 있으면 조용히 버리는 대신 빼고 진행한다 —
		// 문서 전체를 편집하는 화면에서는 _id가 항상 함께 오기 때문이다.
		delete(doc, "_id")
		// ReplaceOne이 아니라 $set을 쓴다. 화면이 보여준 필드만 보낸 경우
		// ReplaceOne은 나머지 필드를 전부 지운다.
		res, err := coll.UpdateOne(ctx, bson.D{{Key: "_id", Value: id}},
			bson.D{{Key: "$set", Value: doc}})
		if err != nil {
			return nil, fmt.Errorf("수정 실패: %w", err)
		}
		return &MutationResult{
			Affected:  res.ModifiedCount,
			Statement: fmt.Sprintf("db.%s.updateOne({_id: %v}, {$set: …})", m.Table.Name, mongoScalar(id)),
		}, nil

	case "delete":
		id, err := mongoID(m.Key)
		if err != nil {
			return nil, err
		}
		res, err := coll.DeleteOne(ctx, bson.D{{Key: "_id", Value: id}})
		if err != nil {
			return nil, fmt.Errorf("삭제 실패: %w", err)
		}
		return &MutationResult{
			Affected:  res.DeletedCount,
			Statement: fmt.Sprintf("db.%s.deleteOne({_id: %v})", m.Table.Name, mongoScalar(id)),
		}, nil

	case "restore":
		// 백업 복구 전용. 문서를 통째로 바꾸고, 없으면 만든다.
		//
		// insert가 아니라 upsert인 이유: 복구는 실패한 뒤 다시 실행되는 일이 잦은데,
		// insert만 하면 두 번째 시도가 중복 키로 전부 실패한다. 또 $set이 아니라
		// 전체 교체인 이유: 덤프의 문서가 그 시점의 전부이며, $set으로 넣으면
		// 대상에 남아 있던 필드가 섞여 원본과 다른 문서가 된다.
		doc, err := mongoDocFromValues(m.Values)
		if err != nil {
			return nil, err
		}
		id, ok := doc["_id"]
		if !ok {
			res, err := coll.InsertOne(ctx, doc)
			if err != nil {
				return nil, fmt.Errorf("복구 실패: %w", err)
			}
			return &MutationResult{Affected: 1,
				Statement: fmt.Sprintf("db.%s.insertOne(…) → _id=%v",
					m.Table.Name, mongoScalar(res.InsertedID))}, nil
		}
		res, err := coll.ReplaceOne(ctx, bson.D{{Key: "_id", Value: id}}, doc,
			options.Replace().SetUpsert(true))
		if err != nil {
			return nil, fmt.Errorf("복구 실패: %w", err)
		}
		return &MutationResult{
			Affected:  res.ModifiedCount + res.UpsertedCount,
			Statement: fmt.Sprintf("db.%s.replaceOne({_id: %v}, …, {upsert: true})", m.Table.Name, mongoScalar(id)),
		}, nil

	default:
		return nil, fmt.Errorf("알 수 없는 동작입니다: %s", m.Action)
	}
}

// mongoDocFromValues는 편집 화면이 보낸 값을 문서로 만든다.
//
// 값 하나가 rawField(=$document)면 그것을 문서 전체로 본다. 화면이 JSON 편집기를
// 쓰는 경우이며, 그때 다른 키는 무시한다 — 두 표현이 섞이면 어느 쪽이 이겨야
// 하는지 정할 수 없다.
func mongoDocFromValues(values map[string]any) (bson.M, error) {
	if raw, ok := values[rawField]; ok {
		s, ok := raw.(string)
		if !ok {
			return nil, fmt.Errorf("문서는 JSON 문자열이어야 합니다")
		}
		var doc bson.M
		if err := bson.UnmarshalExtJSON([]byte(s), false, &doc); err != nil {
			return nil, fmt.Errorf("문서 JSON을 해석할 수 없습니다: %w", err)
		}
		return doc, nil
	}
	doc := bson.M{}
	for k, v := range values {
		if k == rawField {
			continue
		}
		doc[k] = mongoParseCell(v)
	}
	if len(doc) == 0 {
		return nil, fmt.Errorf("저장할 값이 없습니다")
	}
	return doc, nil
}

// mongoParseCell은 표 칸에서 온 값을 되돌린다.
// 중첩 문서를 JSON 문자열로 보여줬으므로, 그 형태로 돌아온 값은 다시 문서로 만든다.
func mongoParseCell(v any) any {
	s, ok := v.(string)
	if !ok {
		return v
	}
	trimmed := strings.TrimSpace(s)
	if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		var parsed any
		if err := json.Unmarshal([]byte(trimmed), &parsed); err == nil {
			return parsed
		}
	}
	return v
}

func mongoID(key map[string]any) (any, error) {
	raw, ok := key["_id"]
	if !ok || raw == nil {
		return nil, fmt.Errorf("대상 문서의 _id가 필요합니다")
	}
	s, ok := raw.(string)
	if !ok {
		return raw, nil
	}
	if id, err := bson.ObjectIDFromHex(s); err == nil {
		return id, nil
	}
	return s, nil
}

// RunStatements는 MongoDB 명령을 실행한다.
//
// 명령을 JSON으로 받는 이유: 몽고 셸 문법(db.users.find({...}))을 지원하려면
// JavaScript 파서가 필요하고, 그것은 이 앱이 짊어질 것이 아니다. runCommand는
// 몽고가 실제로 받는 형식이며, 드라이버 문서와 서버 문서가 모두 이 형태로 쓰여 있다.
func (a *mongoAdapter) RunStatements(ctx context.Context, t Target, r StatementRequest) ([]StatementResult, error) {
	script := strings.TrimSpace(r.Statement)
	if script == "" {
		return nil, fmt.Errorf("실행할 명령이 없습니다")
	}
	if r.ReadOnly && !mongoReadOnlyCommand(script) {
		return []StatementResult{{
			Statement: script,
			Error:     "읽기 전용 모드에서는 find·aggregate·count·distinct만 실행할 수 있습니다",
		}}, nil
	}

	var cmd bson.D
	if err := bson.UnmarshalExtJSON([]byte(script), false, &cmd); err != nil {
		return nil, fmt.Errorf("명령 JSON을 해석할 수 없습니다: %w", err)
	}

	client, dbName, err := a.connect(ctx, t)
	if err != nil {
		return nil, err
	}
	defer func() { _ = client.Disconnect(context.WithoutCancel(ctx)) }()

	start := time.Now()
	var raw bson.M
	err = client.Database(dbName).RunCommand(ctx, cmd).Decode(&raw)
	elapsed := float64(time.Since(start).Microseconds()) / 1000
	res := StatementResult{Statement: script, ElapsedMs: elapsed, Kind: "rows"}
	if err != nil {
		res.Error = err.Error()
		return []StatementResult{res}, nil
	}

	// 커서를 돌려주는 명령(find/aggregate)은 첫 배치를 표로 펼친다.
	// 그러지 않으면 결과가 {cursor:{firstBatch:[…]}} 한 덩어리로 보여 쓸모가 없다.
	if docs, ok := mongoCursorBatch(raw); ok {
		limit := r.MaxRows
		if limit <= 0 || limit > MaxRowLimit {
			limit = MaxRowLimit
		}
		if len(docs) > limit {
			docs = docs[:limit]
			res.Truncated = true
		}
		fields := mongoFieldOrder(docs)
		for _, f := range fields {
			res.Columns = append(res.Columns, DataColumn{Name: f, Type: "document field", Nullable: true})
		}
		res.Columns = append(res.Columns, DataColumn{Name: rawField, Type: "json"})
		for _, doc := range docs {
			row := make([]any, 0, len(fields)+1)
			for _, f := range fields {
				v, _ := normalizeValue(mongoScalar(doc[f]), true)
				row = append(row, v)
			}
			j, _ := mongoJSON(doc)
			v, _ := normalizeValue(j, true)
			row = append(row, v)
			res.Rows = append(res.Rows, row)
		}
		res.Affected = int64(len(res.Rows))
		return []StatementResult{res}, nil
	}

	// 커서가 아니면 응답 문서 자체를 한 행으로 보여준다.
	j, _ := mongoJSON(raw)
	value, _ := normalizeValue(j, true)
	res.Columns = []DataColumn{{Name: "result", Type: "json"}}
	res.Rows = [][]any{{value}}
	res.Affected = 1
	return []StatementResult{res}, nil
}

// mongoCursorBatch는 응답에서 커서 배치를 꺼낸다.
func mongoCursorBatch(raw bson.M) ([]bson.M, bool) {
	cursor, ok := raw["cursor"]
	if !ok {
		return nil, false
	}
	var batch any
	switch c := cursor.(type) {
	case bson.M:
		batch = c["firstBatch"]
	case bson.D:
		for _, e := range c {
			if e.Key == "firstBatch" {
				batch = e.Value
			}
		}
	default:
		return nil, false
	}
	items, ok := batch.(bson.A)
	if !ok {
		return nil, false
	}
	out := make([]bson.M, 0, len(items))
	for _, item := range items {
		switch doc := item.(type) {
		case bson.M:
			out = append(out, doc)
		case bson.D:
			m := bson.M{}
			for _, e := range doc {
				m[e.Key] = e.Value
			}
			out = append(out, m)
		}
	}
	return out, true
}

// mongoReadOnlyCommand는 첫 키가 조회 명령인지 본다.
func mongoReadOnlyCommand(script string) bool {
	var cmd bson.D
	if err := bson.UnmarshalExtJSON([]byte(script), false, &cmd); err != nil {
		return false
	}
	if len(cmd) == 0 {
		return false
	}
	switch strings.ToLower(cmd[0].Key) {
	case "find", "aggregate", "count", "distinct", "listcollections", "listindexes",
		"dbstats", "collstats", "explain":
		// aggregate에 $out·$merge가 있으면 쓰기다. 파이프라인 전체를 문자열로 훑어
		// 확인한다 — 단계별로 파고들기보다 확실하고, 오탐이 나도 실행을 막을 뿐이다.
		return !strings.Contains(script, "$out") && !strings.Contains(script, "$merge")
	}
	return false
}
