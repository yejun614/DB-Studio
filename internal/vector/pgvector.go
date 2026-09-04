package vector

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// pgvector.
//
// **종류가 아니라 성질이다.** pgvector 는 PostgreSQL 의 확장이고, 그것을 별도
// 커넥션 종류로 만들면 같은 데이터베이스를 두 번 등록해야 한다 — 한쪽에서는
// 스키마·데이터·SQL 이 보이고 다른 쪽에서는 벡터만 보이는, 반쪽짜리 커넥션이 둘.
//
// 그래서 여기서는 이미 열려 있는 *sql.DB 를 받아 쓴다. 컬렉션 하나는 **표의 벡터
// 컬럼 하나**다(스키마.표.컬럼). 한 표에 임베딩 컬럼이 둘일 수 있고(제목용·본문용),
// 그 둘은 차원도 색인도 다르므로 하나로 묶으면 둘 다 틀리게 말하게 된다.

// colKey는 벡터 컬럼 하나를 가리킨다(스키마·표·컬럼).
type colKey struct{ schema, table, column string }

// PgVector는 pgvector 확장을 쓰는 PostgreSQL 데이터베이스다.
type PgVector struct {
	db *sql.DB
	// owned가 참이면 Close 에서 커넥션을 닫는다. 남이 준 것을 닫으면 그 뒤의
	// 모든 조회가 "이미 닫힌 커넥션"으로 죽는다.
	owned bool
}

func NewPgVector(db *sql.DB, owned bool) *PgVector {
	return &PgVector{db: db, owned: owned}
}

func (p *PgVector) Kind() string { return KindPgVector }

func (p *PgVector) Close() error {
	if p.owned && p.db != nil {
		return p.db.Close()
	}
	return nil
}

// Ping은 확장이 깔려 있는지 확인한다.
//
// 접속만 확인하지 않는 이유: 이 화면에서 "붙었다"는 답은 쓸모가 없다. 사람이
// 알아야 하는 것은 **이 데이터베이스에서 벡터를 볼 수 있는가**이고, 그 답은
// 확장이 있는가다. 없으면 무엇을 해야 하는지 함께 말한다.
func (p *PgVector) Ping(ctx context.Context) (string, error) {
	var version sql.NullString
	err := p.db.QueryRowContext(ctx,
		`SELECT extversion FROM pg_extension WHERE extname = 'vector'`).Scan(&version)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("이 데이터베이스에 vector 확장이 없습니다. " +
			"관리자 계정으로 CREATE EXTENSION vector; 를 실행하면 볼 수 있습니다")
	}
	if err != nil {
		return "", fmt.Errorf("확장 목록을 읽지 못했습니다: %w", err)
	}
	return "pgvector " + version.String, nil
}

func (p *PgVector) Overview(ctx context.Context) (*Overview, error) {
	version, err := p.Ping(ctx)
	if err != nil {
		return nil, err
	}
	ov := &Overview{Kind: KindPgVector, Version: version, Collections: []Collection{}}

	// 벡터 컬럼을 찾는다. format_type 이 vector(1536) 처럼 차원까지 알려주는데,
	// information_schema 로는 그 차원을 알 수 없다(거기서는 그냥 USER-DEFINED 다).
	rows, err := p.db.QueryContext(ctx, `
		SELECT n.nspname, c.relname, a.attname,
		       format_type(a.atttypid, a.atttypmod),
		       COALESCE(cl.reltuples, -1)::bigint
		FROM pg_attribute a
		JOIN pg_class c ON c.oid = a.attrelid
		JOIN pg_class cl ON cl.oid = c.oid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		JOIN pg_type t ON t.oid = a.atttypid
		WHERE t.typname IN ('vector', 'halfvec', 'sparsevec')
		  AND a.attnum > 0 AND NOT a.attisdropped
		  AND c.relkind IN ('r', 'p', 'm')
		  AND n.nspname NOT IN ('pg_catalog', 'information_schema')
		ORDER BY n.nspname, c.relname, a.attnum`)
	if err != nil {
		return nil, fmt.Errorf("벡터 컬럼을 찾지 못했습니다: %w", err)
	}
	defer rows.Close()

	found := []colKey{}
	cols := map[colKey]*Collection{}
	for rows.Next() {
		var schemaName, table, column, rawType string
		var estimate int64
		if err := rows.Scan(&schemaName, &table, &column, &rawType, &estimate); err != nil {
			return nil, err
		}
		k := colKey{schemaName, table, column}
		found = append(found, k)
		col := &Collection{
			Name:       CollectionName(schemaName, table, column),
			Points:     estimate,
			Indexed:    -1,
			Fullness:   -1,
			Status:     "unknown",
			Dimensions: dimensionsOf(rawType),
			Facts: []Fact{
				{Label: "타입", Value: rawType},
			},
		}
		if estimate < 0 {
			// reltuples 는 ANALYZE 시점의 추정치다. 한 번도 돌지 않았으면 -1 이다.
			col.Points = -1
			col.Note = "행 수를 모릅니다 — 이 표에 ANALYZE 가 아직 돌지 않았습니다"
		}
		cols[k] = col
		ov.Collections = append(ov.Collections, Collection{})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// 색인. 어떤 연산자 클래스로 만들었는지가 곧 거리 함수다 —
	// vector_cosine_ops 로 만든 색인은 코사인 검색에만 쓰인다. 다른 연산자로
	// 검색하면 색인을 타지 않고 전수 조사가 되는데, **결과는 맞고 느리기만 하다**.
	// 그래서 이것을 보여주지 않으면 느린 이유를 찾을 자리가 없다.
	if err := p.loadIndexes(ctx, cols); err != nil {
		ov.Notes = append(ov.Notes, err.Error())
	}

	ov.Collections = ov.Collections[:0]
	for _, k := range found {
		col := cols[k]
		if col.Metric == "" {
			col.Note = strings.TrimSpace(col.Note + " 이 컬럼에는 벡터 색인이 없습니다 — " +
				"검색이 언제나 전수 조사가 됩니다(결과는 맞지만 표가 커지면 느려집니다)")
		}
		ov.Collections = append(ov.Collections, *col)
	}
	return ov, nil
}

// loadIndexes는 벡터 색인을 찾아 컬렉션에 붙인다.
func (p *PgVector) loadIndexes(ctx context.Context, cols map[colKey]*Collection) error {
	rows, err := p.db.QueryContext(ctx, `
		SELECT n.nspname, t.relname, a.attname, am.amname, opc.opcname
		FROM pg_index i
		JOIN pg_class ix ON ix.oid = i.indexrelid
		JOIN pg_class t ON t.oid = i.indrelid
		JOIN pg_namespace n ON n.oid = t.relnamespace
		JOIN pg_am am ON am.oid = ix.relam
		JOIN pg_attribute a ON a.attrelid = t.oid AND a.attnum = i.indkey[0]
		JOIN pg_opclass opc ON opc.oid = i.indclass[0]
		WHERE am.amname IN ('ivfflat', 'hnsw')`)
	if err != nil {
		return fmt.Errorf("벡터 색인을 읽지 못했습니다: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var schemaName, table, column, method, opclass string
		if err := rows.Scan(&schemaName, &table, &column, &method, &opclass); err != nil {
			return err
		}
		col, ok := cols[colKey{schemaName, table, column}]
		if !ok {
			continue
		}
		col.IndexType = method
		col.Metric = NormalizeMetric(opclass)
		col.Status = "green"
		col.Facts = append(col.Facts,
			Fact{Label: "색인", Value: method + " / " + opclass,
				Help: "이 연산자 클래스와 같은 거리 함수로 찾을 때만 색인을 탑니다"})
	}
	return rows.Err()
}

// dimensionsOf는 vector(1536) 같은 타입 문자열에서 차원을 읽는다.
func dimensionsOf(rawType string) int {
	open := strings.Index(rawType, "(")
	if open < 0 || !strings.HasSuffix(rawType, ")") {
		// 차원을 정하지 않은 컬럼이다(vector). 값마다 길이가 다를 수 있어서
		// 색인을 만들 수 없다.
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(rawType[open+1 : len(rawType)-1]))
	if err != nil {
		return 0
	}
	return n
}

// CollectionName은 스키마·표·컬럼을 한 이름으로 묶는다.
func CollectionName(schemaName, table, column string) string {
	if schemaName == "" || schemaName == "public" {
		return table + "." + column
	}
	return schemaName + "." + table + "." + column
}

// splitCollection은 컬렉션 이름을 되돌린다.
func splitCollection(name string) (schemaName, table, column string, err error) {
	parts := strings.Split(name, ".")
	switch len(parts) {
	case 2:
		return "public", parts[0], parts[1], nil
	case 3:
		return parts[0], parts[1], parts[2], nil
	}
	return "", "", "", fmt.Errorf("컬렉션 이름이 표.컬럼 또는 스키마.표.컬럼 이어야 합니다: %s", name)
}

// quoteIdent는 식별자를 인용한다.
//
// 이름은 사용자가 고른 것이 아니라 카탈로그에서 읽어 온 것이지만, 그래도 인용한다:
// 인용하지 않으면 "order" 라는 이름의 표를 열 수 없고, 무엇보다 이름이 SQL 로
// 해석될 여지가 남는다.
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// pkColumn은 이 표의 단일 기본키 컬럼이다. 없으면 빈 문자열.
//
// 벡터를 id 로 집으려면 무엇이 id 인지 알아야 한다. 복합키는 다루지 않는다 —
// 한 문자열로 합쳐 보여줄 수는 있지만 그것을 다시 갈라 조회하는 규칙을 사람이
// 알 수 없고, 임베딩 표가 복합키를 쓰는 일은 드물다.
func (p *PgVector) pkColumn(ctx context.Context, schemaName, table string) (string, error) {
	rows, err := p.db.QueryContext(ctx, `
		SELECT a.attname FROM pg_index i
		JOIN pg_class c ON c.oid = i.indrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum = ANY(i.indkey)
		WHERE i.indisprimary AND c.relname = $1 AND n.nspname = $2`, table, schemaName)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	names := []string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return "", err
		}
		names = append(names, name)
	}
	if len(names) != 1 {
		return "", nil
	}
	return names[0], nil
}

// payloadColumns는 벡터가 아닌 나머지 컬럼이다(메타데이터로 보여준다).
func (p *PgVector) payloadColumns(ctx context.Context, schemaName, table, vectorCol string) ([]string, error) {
	rows, err := p.db.QueryContext(ctx, `
		SELECT a.attname FROM pg_attribute a
		JOIN pg_class c ON c.oid = a.attrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		JOIN pg_type t ON t.oid = a.atttypid
		WHERE c.relname = $1 AND n.nspname = $2
		  AND a.attnum > 0 AND NOT a.attisdropped
		  AND t.typname NOT IN ('vector', 'halfvec', 'sparsevec')
		ORDER BY a.attnum
		LIMIT 24`, table, schemaName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// scanPoints는 (id, vector, payload...) 순서의 결과를 읽는다.
func scanPoints(rows *sql.Rows, payloadCols []string, withVector bool, withScore bool) ([]Point, error) {
	points := []Point{}
	for rows.Next() {
		width := 2 + len(payloadCols)
		if withScore {
			width++
		}
		cells := make([]any, width)
		holders := make([]any, width)
		for i := range cells {
			holders[i] = &cells[i]
		}
		if err := rows.Scan(holders...); err != nil {
			return nil, err
		}
		pt := Point{ID: asString(cells[0])}
		vec, err := parseVectorLiteral(asString(cells[1]))
		if err != nil {
			return nil, err
		}
		pt.Dimensions = len(vec)
		if withVector {
			pt.Vector = vec
		} else {
			pt.Vector, pt.Truncated = Truncate(vec, PreviewDims)
		}
		next := 2
		if withScore {
			pt.Score = asFloat(cells[next])
			// pgvector 의 연산자는 **거리**를 돌려준다(작을수록 가깝다).
			// 유사도로 착각하면 목록이 거꾸로 정렬된다.
			pt.ScoreKind = ScoreDistance
			next++
		}
		if len(payloadCols) > 0 {
			pt.Payload = map[string]any{}
			for i, name := range payloadCols {
				pt.Payload[name] = normalizeCell(cells[next+i])
			}
		}
		points = append(points, pt)
	}
	return points, rows.Err()
}

func asString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case []byte:
		return string(t)
	}
	return fmt.Sprintf("%v", v)
}

func asFloat(v any) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case float32:
		return float64(t)
	case int64:
		return float64(t)
	}
	f, _ := strconv.ParseFloat(asString(v), 64)
	return f
}

// normalizeCell은 JSON 으로 나갈 수 있는 값으로 바꾼다.
func normalizeCell(v any) any {
	switch t := v.(type) {
	case nil:
		return nil
	case []byte:
		return string(t)
	case time.Time:
		return t.Format(time.RFC3339)
	}
	return v
}

// parseVectorLiteral은 pgvector 의 텍스트 표현("[1,2,3]")을 읽는다.
//
// 드라이버가 vector 타입을 모르므로 문자열로 온다. 바이너리 표현을 다루지 않는
// 이유: 텍스트가 정확도를 잃지 않고(float4 를 그대로 찍는다) 규약이 안정적이다.
func parseVectorLiteral(s string) ([]float32, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	// sparsevec 은 {1:0.5,3:0.2}/1536 형태다. 조밀 벡터로 펴는 것은 차원이 크면
	// 메모리를 많이 먹으므로 여기서는 다루지 않고 그 사실을 알린다.
	if strings.HasPrefix(s, "{") {
		return nil, fmt.Errorf("희소 벡터(sparsevec)는 아직 이 화면에서 읽지 않습니다")
	}
	var out []float32
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, fmt.Errorf("벡터 값을 해석하지 못했습니다: %w", err)
	}
	return out, nil
}

func (p *PgVector) Scroll(ctx context.Context, collection, cursor string, limit int, withVector bool) (*Page, error) {
	schemaName, table, column, err := splitCollection(collection)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	pk, err := p.pkColumn(ctx, schemaName, table)
	if err != nil {
		return nil, err
	}
	page := &Page{Collection: collection}
	if pk == "" {
		// 기본키가 없으면 점을 id 로 집을 수 없다. 훑어보기는 되지만 비교와
		// "이것과 비슷한 것 찾기"는 할 수 없으므로 그 사실을 미리 말한다.
		page.Notes = append(page.Notes,
			"이 표에 단일 기본키가 없어 점을 id 로 집을 수 없습니다. 훑어보기만 됩니다")
	}
	payload, err := p.payloadColumns(ctx, schemaName, table, column)
	if err != nil {
		return nil, err
	}

	offset := 0
	if cursor != "" {
		offset, _ = strconv.Atoi(cursor)
	}
	idExpr := "''"
	if pk != "" {
		idExpr = quoteIdent(pk) + "::text"
	}
	selects := []string{idExpr, quoteIdent(column) + "::text"}
	for _, name := range payload {
		selects = append(selects, quoteIdent(name))
	}
	order := ""
	if pk != "" {
		// 정렬이 없으면 페이지를 넘길 때 같은 행이 다시 나오거나 건너뛰어진다.
		order = " ORDER BY " + quoteIdent(pk)
	} else {
		page.Notes = append(page.Notes,
			"정렬 기준이 없어 페이지 사이의 행 순서가 보장되지 않습니다")
	}
	query := fmt.Sprintf("SELECT %s FROM %s.%s WHERE %s IS NOT NULL%s LIMIT $1 OFFSET $2",
		strings.Join(selects, ", "), quoteIdent(schemaName), quoteIdent(table),
		quoteIdent(column), order)

	rows, err := p.db.QueryContext(ctx, query, limit+1, offset)
	if err != nil {
		return nil, fmt.Errorf("조회 실패: %w", err)
	}
	defer rows.Close()
	points, err := scanPoints(rows, payload, withVector, false)
	if err != nil {
		return nil, err
	}
	if len(points) > limit {
		points = points[:limit]
		page.Next = strconv.Itoa(offset + limit)
	}
	page.Points = points
	return page, nil
}

func (p *PgVector) Fetch(ctx context.Context, collection string, ids []string) ([]Point, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	schemaName, table, column, err := splitCollection(collection)
	if err != nil {
		return nil, err
	}
	pk, err := p.pkColumn(ctx, schemaName, table)
	if err != nil {
		return nil, err
	}
	if pk == "" {
		return nil, fmt.Errorf("이 표에 단일 기본키가 없어 id 로 점을 집을 수 없습니다")
	}
	payload, err := p.payloadColumns(ctx, schemaName, table, column)
	if err != nil {
		return nil, err
	}
	selects := []string{quoteIdent(pk) + "::text", quoteIdent(column) + "::text"}
	for _, name := range payload {
		selects = append(selects, quoteIdent(name))
	}
	// 기본키의 타입은 정수일 수도 uuid 일 수도 있다. 문자열로 견주면 어느 쪽이든
	// 통하고, 값은 파라미터로만 들어가므로 SQL 로 해석될 여지가 없다.
	query := fmt.Sprintf("SELECT %s FROM %s.%s WHERE %s::text = ANY($1)",
		strings.Join(selects, ", "), quoteIdent(schemaName), quoteIdent(table), quoteIdent(pk))

	rows, err := p.db.QueryContext(ctx, query, pgTextArray(ids))
	if err != nil {
		return nil, fmt.Errorf("조회 실패: %w", err)
	}
	defer rows.Close()
	found, err := scanPoints(rows, payload, true, false)
	if err != nil {
		return nil, err
	}
	// 요청한 순서를 지킨다. 비교 화면이 "왼쪽/오른쪽"을 요청한 대로 채운다.
	byID := map[string]Point{}
	for _, pt := range found {
		byID[pt.ID] = pt
	}
	out := make([]Point, 0, len(ids))
	for _, id := range ids {
		if pt, ok := byID[id]; ok {
			out = append(out, pt)
		}
	}
	return out, nil
}

// pgTextArray는 문자열 목록을 PostgreSQL 배열 리터럴로 만든다.
func pgTextArray(values []string) string {
	parts := make([]string, 0, len(values))
	for _, v := range values {
		parts = append(parts, `"`+strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(v)+`"`)
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// distanceOperator는 거리 함수에 맞는 연산자다.
//
// 색인을 만든 연산자 클래스와 **같은** 연산자로 찾아야 색인을 탄다. 다른 것을 쓰면
// 결과는 맞지만 전수 조사가 되어 느려지는데, 그 사실은 아무 데도 드러나지 않는다.
func distanceOperator(metric string) (string, string) {
	switch NormalizeMetric(metric) {
	case MetricEuclid:
		return "<->", "유클리드 거리"
	case MetricDot:
		return "<#>", "내적(음수)"
	default:
		return "<=>", "코사인 거리"
	}
}

func (p *PgVector) Search(ctx context.Context, req SearchRequest) (*Result, error) {
	schemaName, table, column, err := splitCollection(req.Collection)
	if err != nil {
		return nil, err
	}
	limit := req.Limit
	if limit <= 0 || limit > 200 {
		limit = 20
	}
	res := &Result{Collection: req.Collection, QueryID: req.ID}

	// 이 컬럼의 색인이 무엇으로 만들어졌는지 본다. 색인과 다른 연산자로 찾으면
	// 전수 조사가 된다.
	cols := map[colKey]*Collection{{schemaName, table, column}: {}}
	if err := p.loadIndexes(ctx, cols); err != nil {
		res.Notes = append(res.Notes, err.Error())
	}
	metric := cols[colKey{schemaName, table, column}].Metric
	if metric == "" {
		metric = MetricCosine
		res.Notes = append(res.Notes,
			"이 컬럼에 벡터 색인이 없어 코사인 거리로 전수 조사합니다. 표가 커지면 느려집니다")
	}
	res.Metric = metric
	op, opLabel := distanceOperator(metric)

	query := req.Vector
	if len(query) == 0 && req.ID != "" {
		found, err := p.Fetch(ctx, req.Collection, []string{req.ID})
		if err != nil {
			return nil, err
		}
		if len(found) == 0 || len(found[0].Vector) == 0 {
			return nil, fmt.Errorf("%s 행을 찾지 못했거나 벡터가 비어 있습니다", req.ID)
		}
		query = found[0].Vector
	}
	if len(query) == 0 {
		return nil, fmt.Errorf("찾을 벡터나 기준 행의 id 가 필요합니다")
	}
	res.Query, res.Dimensions = query, len(query)

	pk, err := p.pkColumn(ctx, schemaName, table)
	if err != nil {
		return nil, err
	}
	idExpr := "''"
	if pk != "" {
		idExpr = quoteIdent(pk) + "::text"
	}
	payload := []string{}
	if req.WithPayload {
		if payload, err = p.payloadColumns(ctx, schemaName, table, column); err != nil {
			return nil, err
		}
	}
	selects := []string{
		idExpr, quoteIdent(column) + "::text",
		fmt.Sprintf("(%s %s $1::vector)::float8", quoteIdent(column), op),
	}
	for _, name := range payload {
		selects = append(selects, quoteIdent(name))
	}
	sql := fmt.Sprintf(
		"SELECT %s FROM %s.%s WHERE %s IS NOT NULL ORDER BY %s %s $1::vector LIMIT $2",
		strings.Join(selects, ", "), quoteIdent(schemaName), quoteIdent(table),
		quoteIdent(column), quoteIdent(column), op)

	start := time.Now()
	rows, err := p.db.QueryContext(ctx, sql, vectorLiteral(query), limit)
	if err != nil {
		return nil, fmt.Errorf("검색 실패: %w", err)
	}
	defer rows.Close()
	points, err := scanPoints(rows, payload, req.WithVector, true)
	if err != nil {
		return nil, err
	}
	res.ElapsedMs = float64(time.Since(start).Microseconds()) / 1000
	res.Points = points
	res.Notes = append(res.Notes, "점수는 "+opLabel+"입니다 — 작을수록 가깝습니다")
	return res, nil
}

// vectorLiteral은 벡터를 pgvector 가 읽는 텍스트로 만든다.
func vectorLiteral(v []float32) string {
	parts := make([]string, 0, len(v))
	for _, f := range v {
		parts = append(parts, strconv.FormatFloat(float64(f), 'g', -1, 32))
	}
	return "[" + strings.Join(parts, ",") + "]"
}
