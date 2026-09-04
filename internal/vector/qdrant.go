package vector

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"dbstudio/internal/opsapi"
)

// Qdrant 클라이언트.
//
// REST 를 쓴다(gRPC 가 아니라). 이 화면이 하는 일은 목록·훑어보기·이웃 찾기이고
// 그 셋은 초당 수천 번 부르는 일이 아니다 — gRPC 가 주는 이득은 없고 의존성만 는다.
//
// **읽기 전용이다.** 컬렉션 삭제나 점 갱신은 되돌릴 수 없고, 임베딩은 대개 다른
// 파이프라인이 만들어 넣는다.

// QdrantDefaultPort는 REST 포트다(gRPC 는 6334).
const QdrantDefaultPort = 6333

type Qdrant struct {
	cfg    opsapi.Config
	client *http.Client
}

func NewQdrant(cfg opsapi.Config) *Qdrant {
	return &Qdrant{cfg: cfg, client: cfg.HTTPClient()}
}

func (q *Qdrant) Kind() string   { return KindQdrant }
func (q *Qdrant) Close() error   { return nil }
func (q *Qdrant) apiKey() string { return strings.TrimSpace(q.cfg.Extra["api_key"]) }

func (q *Qdrant) call(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, q.cfg.BaseURL()+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// 자체 호스팅에서 인증을 켜지 않았으면 키가 없다. 빈 헤더를 보내면 일부
	// 버전이 "잘못된 키"로 거절하므로 아예 붙이지 않는다.
	if key := q.apiKey(); key != "" {
		req.Header.Set("api-key", key)
	}
	res, err := q.client.Do(req)
	if err != nil {
		return fmt.Errorf("%s 에 접속하지 못했습니다: %w", q.cfg.BaseURL(), err)
	}
	defer res.Body.Close()

	payload, err := io.ReadAll(io.LimitReader(res.Body, 32<<20))
	if err != nil {
		return err
	}
	if res.StatusCode >= 400 {
		return qdrantError(res.StatusCode, payload)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(payload, out); err != nil {
		return fmt.Errorf("응답을 해석하지 못했습니다: %w", err)
	}
	return nil
}

// qdrantError는 오류 응답을 사람 말로 바꾼다.
func qdrantError(status int, body []byte) error {
	var payload struct {
		Status any `json:"status"`
	}
	_ = json.Unmarshal(body, &payload)
	detail := ""
	switch v := payload.Status.(type) {
	case map[string]any:
		if e, ok := v["error"].(string); ok {
			detail = e
		}
	case string:
		detail = v
	}
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("Qdrant 가 인증을 거절했습니다 — API 키를 확인하세요")
	case http.StatusNotFound:
		if detail != "" {
			return fmt.Errorf("%s", detail)
		}
		return fmt.Errorf("컬렉션을 찾을 수 없습니다")
	}
	if detail != "" {
		return fmt.Errorf("%s (HTTP %d)", detail, status)
	}
	return fmt.Errorf("요청이 거절됐습니다 (HTTP %d)", status)
}

func (q *Qdrant) Ping(ctx context.Context) (string, error) {
	var out struct {
		Title   string `json:"title"`
		Version string `json:"version"`
	}
	if err := q.call(ctx, http.MethodGet, "/", nil, &out); err != nil {
		return "", err
	}
	if out.Version == "" {
		return "Qdrant", nil
	}
	return "Qdrant " + out.Version, nil
}

type qdrantCollectionInfo struct {
	Result struct {
		Status              string `json:"status"`
		OptimizerStatus     any    `json:"optimizer_status"`
		PointsCount         *int64 `json:"points_count"`
		IndexedVectorsCount *int64 `json:"indexed_vectors_count"`
		SegmentsCount       int    `json:"segments_count"`
		PayloadSchema       map[string]struct {
			DataType string `json:"data_type"`
		} `json:"payload_schema"`
		Config struct {
			Params struct {
				// vectors 는 단일 설정이거나 이름별 설정 묶음이다.
				Vectors     json.RawMessage `json:"vectors"`
				ShardNumber int             `json:"shard_number"`
			} `json:"params"`
			HnswConfig struct {
				M           int `json:"m"`
				EfConstruct int `json:"ef_construct"`
			} `json:"hnsw_config"`
		} `json:"config"`
	} `json:"result"`
}

type qdrantVectorParams struct {
	Size     int    `json:"size"`
	Distance string `json:"distance"`
}

// parseVectors는 단일 벡터 설정과 이름 붙은 벡터 묶음을 모두 읽는다.
//
// 이름 붙은 벡터(named vectors)는 한 점이 여러 임베딩을 갖는 기능이다. 흔하지는
// 않지만, 이것을 모르는 채로 읽으면 그런 컬렉션에서 차원 수가 0으로 나오고
// 화면은 "빈 컬렉션"처럼 보이게 된다.
func parseQdrantVectors(raw json.RawMessage) (qdrantVectorParams, []string) {
	var single qdrantVectorParams
	if err := json.Unmarshal(raw, &single); err == nil && single.Size > 0 {
		return single, nil
	}
	var named map[string]qdrantVectorParams
	if err := json.Unmarshal(raw, &named); err == nil && len(named) > 0 {
		names := make([]string, 0, len(named))
		for name := range named {
			names = append(names, name)
		}
		sort.Strings(names)
		return named[names[0]], names
	}
	return qdrantVectorParams{}, nil
}

func (q *Qdrant) Overview(ctx context.Context) (*Overview, error) {
	var list struct {
		Result struct {
			Collections []struct {
				Name string `json:"name"`
			} `json:"collections"`
		} `json:"result"`
	}
	if err := q.call(ctx, http.MethodGet, "/collections", nil, &list); err != nil {
		return nil, err
	}
	ov := &Overview{Kind: KindQdrant, Collections: []Collection{}}
	ov.Version, _ = q.Ping(ctx)

	for _, item := range list.Result.Collections {
		var info qdrantCollectionInfo
		if err := q.call(ctx, http.MethodGet,
			"/collections/"+url.PathEscape(item.Name), nil, &info); err != nil {
			// 하나를 못 읽었다고 목록 전체를 버리지 않는다. 권한이 컬렉션마다
			// 다를 수 있고, 그때 다른 컬렉션은 멀쩡히 보여야 한다.
			ov.Notes = append(ov.Notes, fmt.Sprintf("%s 을(를) 읽지 못했습니다: %v", item.Name, err))
			ov.Collections = append(ov.Collections, Collection{
				Name: item.Name, Points: -1, Indexed: -1, Fullness: -1, Status: "unknown",
			})
			continue
		}
		r := info.Result
		params, named := parseQdrantVectors(r.Config.Params.Vectors)
		col := Collection{
			Name:       item.Name,
			Dimensions: params.Size,
			Metric:     NormalizeMetric(params.Distance),
			Points:     valueOr(r.PointsCount, -1),
			Indexed:    valueOr(r.IndexedVectorsCount, -1),
			Status:     strings.ToLower(r.Status),
			IndexType:  "hnsw",
			Fullness:   -1,
		}
		for key := range r.PayloadSchema {
			col.PayloadKeys = append(col.PayloadKeys, key)
		}
		sort.Strings(col.PayloadKeys)
		col.Facts = []Fact{
			{Label: "세그먼트", Value: strconv.Itoa(r.SegmentsCount),
				Help: "저장 단위입니다. 색인은 세그먼트마다 따로 만들어집니다"},
		}
		if r.Config.HnswConfig.M > 0 {
			col.Facts = append(col.Facts, Fact{
				Label: "HNSW m", Value: strconv.Itoa(r.Config.HnswConfig.M),
				Help: "이웃 연결 수입니다. 크면 정확하고 느리며 메모리를 더 씁니다",
			})
		}
		if len(named) > 0 {
			col.Note = "이름 붙은 벡터를 씁니다(" + strings.Join(named, ", ") +
				"). 여기서는 첫 번째 것을 봅니다"
		}
		if col.Indexed >= 0 && col.Points > 0 && col.Indexed < col.Points {
			col.Note = strings.TrimSpace(col.Note + " 색인이 아직 따라오지 못한 벡터가 있습니다 — " +
				"그 벡터들도 검색되지만 전수 조사로 떨어져 느립니다")
		}
		ov.Collections = append(ov.Collections, col)
	}
	return ov, nil
}

func valueOr(v *int64, def int64) int64 {
	if v == nil {
		return def
	}
	return *v
}

type qdrantPoint struct {
	ID      any             `json:"id"`
	Vector  json.RawMessage `json:"vector"`
	Payload map[string]any  `json:"payload"`
	Score   *float64        `json:"score"`
}

// vectorOf는 단일 벡터와 이름 붙은 벡터 묶음을 모두 읽는다.
func (p qdrantPoint) vectorOf() []float32 {
	if len(p.Vector) == 0 {
		return nil
	}
	var flat []float32
	if err := json.Unmarshal(p.Vector, &flat); err == nil {
		return flat
	}
	var named map[string][]float32
	if err := json.Unmarshal(p.Vector, &named); err == nil && len(named) > 0 {
		names := make([]string, 0, len(named))
		for k := range named {
			names = append(names, k)
		}
		sort.Strings(names)
		return named[names[0]]
	}
	return nil
}

func (p qdrantPoint) idString() string {
	switch v := p.ID.(type) {
	case string:
		return v
	case float64:
		// Qdrant 의 수치 id 는 정수다. %v 로 찍으면 1e+06 같은 표기가 나와서
		// 그대로 다시 조회하면 찾지 못한다.
		return strconv.FormatInt(int64(v), 10)
	}
	return fmt.Sprintf("%v", p.ID)
}

func toPoint(p qdrantPoint, withVector bool, scoreKind string) Point {
	out := Point{ID: p.idString(), Payload: p.Payload}
	vec := p.vectorOf()
	out.Dimensions = len(vec)
	if withVector {
		out.Vector = vec
	} else if len(vec) > 0 {
		out.Vector, out.Truncated = Truncate(vec, PreviewDims)
	}
	if p.Score != nil {
		out.Score, out.ScoreKind = *p.Score, scoreKind
	}
	return out
}

func (q *Qdrant) Scroll(ctx context.Context, collection, cursor string, limit int, withVector bool) (*Page, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	body := map[string]any{
		"limit": limit, "with_payload": true, "with_vector": true,
	}
	if cursor != "" {
		// 커서는 서버가 준 값을 그대로 돌려준다. 수치 id 인 컬렉션에서는 숫자다.
		if n, err := strconv.ParseInt(cursor, 10, 64); err == nil {
			body["offset"] = n
		} else {
			body["offset"] = cursor
		}
	}
	var out struct {
		Result struct {
			Points         []qdrantPoint `json:"points"`
			NextPageOffset any           `json:"next_page_offset"`
		} `json:"result"`
	}
	if err := q.call(ctx, http.MethodPost,
		"/collections/"+url.PathEscape(collection)+"/points/scroll", body, &out); err != nil {
		return nil, err
	}
	page := &Page{Collection: collection, Points: make([]Point, 0, len(out.Result.Points))}
	for _, p := range out.Result.Points {
		page.Points = append(page.Points, toPoint(p, withVector, ""))
	}
	if out.Result.NextPageOffset != nil {
		page.Next = fmt.Sprintf("%v", out.Result.NextPageOffset)
		if f, ok := out.Result.NextPageOffset.(float64); ok {
			page.Next = strconv.FormatInt(int64(f), 10)
		}
	}
	return page, nil
}

func (q *Qdrant) Fetch(ctx context.Context, collection string, ids []string) ([]Point, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	// id 는 문자열(UUID)이거나 정수다. 종류를 섞어 보내면 서버가 거절하므로
	// 숫자로 읽히는 것은 숫자로 바꿔 보낸다.
	raw := make([]any, 0, len(ids))
	for _, id := range ids {
		if n, err := strconv.ParseInt(id, 10, 64); err == nil {
			raw = append(raw, n)
			continue
		}
		raw = append(raw, id)
	}
	var out struct {
		Result []qdrantPoint `json:"result"`
	}
	if err := q.call(ctx, http.MethodPost,
		"/collections/"+url.PathEscape(collection)+"/points",
		map[string]any{"ids": raw, "with_payload": true, "with_vector": true}, &out); err != nil {
		return nil, err
	}
	points := make([]Point, 0, len(out.Result))
	for _, p := range out.Result {
		points = append(points, toPoint(p, true, ""))
	}
	return points, nil
}

func (q *Qdrant) Search(ctx context.Context, req SearchRequest) (*Result, error) {
	res := &Result{Collection: req.Collection, QueryID: req.ID}
	query := req.Vector
	if len(query) == 0 && req.ID != "" {
		found, err := q.Fetch(ctx, req.Collection, []string{req.ID})
		if err != nil {
			return nil, err
		}
		if len(found) == 0 || len(found[0].Vector) == 0 {
			return nil, fmt.Errorf("%s 점을 찾지 못했거나 벡터가 없습니다", req.ID)
		}
		query = found[0].Vector
	}
	if len(query) == 0 {
		return nil, fmt.Errorf("찾을 벡터나 기준 점의 id 가 필요합니다")
	}
	limit := req.Limit
	if limit <= 0 || limit > 200 {
		limit = 20
	}
	body := map[string]any{
		"vector": query, "limit": limit,
		"with_payload": req.WithPayload, "with_vector": true,
	}
	if len(req.Filter) > 0 {
		body["filter"] = req.Filter
	}
	var out struct {
		Result []qdrantPoint `json:"result"`
	}
	start := time.Now()
	if err := q.call(ctx, http.MethodPost,
		"/collections/"+url.PathEscape(req.Collection)+"/points/search", body, &out); err != nil {
		return nil, err
	}
	res.ElapsedMs = float64(time.Since(start).Microseconds()) / 1000
	res.Query, res.Dimensions = query, len(query)
	// Qdrant 의 score 는 언제나 **유사도**다(코사인이든 유클리드든 클수록 가깝게
	// 뒤집어 준다). 이것을 거리로 오해하면 목록이 거꾸로 정렬된다.
	for _, p := range out.Result {
		res.Points = append(res.Points, toPoint(p, req.WithVector, ScoreSimilarity))
	}
	return res, nil
}
