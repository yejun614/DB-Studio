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

// Pinecone 클라이언트.
//
// 다른 둘과 다른 점이 하나 있다: **주소가 둘이다.** 인덱스 목록은 제어 평면
// (api.pinecone.io)에 있고, 그 안의 벡터는 인덱스마다 다른 호스트에 있다.
// 커넥션에는 제어 평면과 API 키만 넣게 하고 데이터 평면 주소는 목록에서 읽어 온다 —
// 사람에게 인덱스마다 호스트를 적게 하면 인덱스를 새로 만들 때마다 커넥션을
// 고쳐야 하고, 고치지 않으면 조용히 옛 인덱스를 계속 보게 된다.
//
// **읽기 전용이다.**

const (
	// PineconeControlHost는 제어 평면 기본 주소다.
	PineconeControlHost = "api.pinecone.io"
	// PineconeDefaultPort는 HTTPS 다.
	PineconeDefaultPort = 443
	// pineconeAPIVersion은 헤더로 보내는 규약 버전이다. 적지 않으면 서버가
	// 최신을 고르는데, 그러면 어느 날 응답 모양이 바뀌어도 우리는 모른다.
	pineconeAPIVersion = "2025-04"
)

type Pinecone struct {
	cfg    opsapi.Config
	client *http.Client
	// hosts는 인덱스 이름 → 데이터 평면 호스트다. 목록을 읽을 때 채운다.
	hosts map[string]string
	// metrics는 인덱스 이름 → 거리 함수다.
	//
	// 점수의 **방향**을 알려면 이것이 있어야 한다. cosine·dotproduct 는 클수록
	// 가깝고 euclidean 은 작을수록 가까운데, 응답에는 그 구분이 없다. 모르면
	// 화면이 가장 먼 것을 가장 가까운 것으로 보여준다.
	metrics map[string]string
}

func NewPinecone(cfg opsapi.Config) *Pinecone {
	return &Pinecone{
		cfg: cfg, client: cfg.HTTPClient(),
		hosts: map[string]string{}, metrics: map[string]string{},
	}
}

func (p *Pinecone) Kind() string { return KindPinecone }
func (p *Pinecone) Close() error { return nil }
func (p *Pinecone) apiKey() string {
	// 키는 비밀번호 칸에 넣는다. 커넥션의 자격증명은 암호화되어 저장되지만
	// 옵션은 그렇지 않다 — API 키가 옵션 칸에 있으면 평문으로 남는다.
	if k := strings.TrimSpace(p.cfg.Password); k != "" {
		return k
	}
	return strings.TrimSpace(p.cfg.Extra["api_key"])
}

// namespace는 볼 네임스페이스다. 비어 있으면 기본 네임스페이스다.
func (p *Pinecone) namespace() string { return strings.TrimSpace(p.cfg.Extra["namespace"]) }

func (p *Pinecone) controlURL() string {
	host := strings.TrimSpace(p.cfg.Host)
	if host == "" {
		host = PineconeControlHost
	}
	return "https://" + host
}

func (p *Pinecone) call(ctx context.Context, method, base, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Api-Key", p.apiKey())
	req.Header.Set("X-Pinecone-API-Version", pineconeAPIVersion)

	res, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("%s 에 접속하지 못했습니다: %w", base, err)
	}
	defer res.Body.Close()

	payload, err := io.ReadAll(io.LimitReader(res.Body, 32<<20))
	if err != nil {
		return err
	}
	if res.StatusCode >= 400 {
		return pineconeError(res.StatusCode, payload)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(payload, out); err != nil {
		return fmt.Errorf("응답을 해석하지 못했습니다: %w", err)
	}
	return nil
}

func pineconeError(status int, body []byte) error {
	var payload struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(body, &payload)
	msg := payload.Error.Message
	if msg == "" {
		msg = payload.Message
	}
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("Pinecone 이 인증을 거절했습니다 — API 키를 확인하세요")
	case http.StatusNotFound:
		if msg != "" {
			return fmt.Errorf("%s", msg)
		}
		return fmt.Errorf("인덱스를 찾을 수 없습니다")
	}
	if msg != "" {
		return fmt.Errorf("%s (HTTP %d)", msg, status)
	}
	return fmt.Errorf("요청이 거절됐습니다 (HTTP %d)", status)
}

func (p *Pinecone) Ping(ctx context.Context) (string, error) {
	if p.apiKey() == "" {
		return "", fmt.Errorf("Pinecone API 키가 필요합니다 (비밀번호 칸에 넣으세요)")
	}
	var out pineconeIndexList
	if err := p.call(ctx, http.MethodGet, p.controlURL(), "/indexes", nil, &out); err != nil {
		return "", err
	}
	return fmt.Sprintf("Pinecone (인덱스 %d개)", len(out.Indexes)), nil
}

type pineconeIndexList struct {
	Indexes []struct {
		Name      string `json:"name"`
		Dimension int    `json:"dimension"`
		Metric    string `json:"metric"`
		Host      string `json:"host"`
		Status    struct {
			Ready bool   `json:"ready"`
			State string `json:"state"`
		} `json:"status"`
		Spec map[string]any `json:"spec"`
	} `json:"indexes"`
}

// dataURL은 인덱스의 데이터 평면 주소다.
func (p *Pinecone) dataURL(index string) (string, error) {
	// 사람이 직접 적어 둔 호스트가 있으면 그것을 우선한다(사설망·프록시 뒤).
	if h := strings.TrimSpace(p.cfg.Extra["index_host"]); h != "" {
		return withScheme(h), nil
	}
	if h, ok := p.hosts[index]; ok && h != "" {
		return withScheme(h), nil
	}
	return "", fmt.Errorf("%s 인덱스의 주소를 모릅니다 — 목록을 먼저 읽어야 합니다", index)
}

func withScheme(host string) string {
	if strings.HasPrefix(host, "http://") || strings.HasPrefix(host, "https://") {
		return strings.TrimSuffix(host, "/")
	}
	return "https://" + strings.TrimSuffix(host, "/")
}

func (p *Pinecone) Overview(ctx context.Context) (*Overview, error) {
	var list pineconeIndexList
	if err := p.call(ctx, http.MethodGet, p.controlURL(), "/indexes", nil, &list); err != nil {
		return nil, err
	}
	ov := &Overview{Kind: KindPinecone, Collections: []Collection{}}
	ov.Version = fmt.Sprintf("Pinecone (규약 %s)", pineconeAPIVersion)
	if ns := p.namespace(); ns != "" {
		ov.Facts = append(ov.Facts, Fact{Label: "네임스페이스", Value: ns})
	}

	for _, idx := range list.Indexes {
		p.hosts[idx.Name] = idx.Host
		p.metrics[idx.Name] = NormalizeMetric(idx.Metric)
		col := Collection{
			Name: idx.Name, Dimensions: idx.Dimension,
			Metric:    NormalizeMetric(idx.Metric),
			Points:    -1,
			Indexed:   -1, // Pinecone 은 색인 진행 상황을 따로 알려주지 않는다.
			Fullness:  -1,
			IndexType: pineconeIndexType(idx.Spec),
			Status:    "unknown",
		}
		if idx.Status.Ready {
			col.Status = "green"
		} else if idx.Status.State != "" {
			col.Status = "yellow"
			col.Note = "인덱스 상태: " + idx.Status.State
		}
		// 통계는 데이터 평면에 있다. 하나가 실패해도 나머지는 보여야 한다.
		if stats, err := p.stats(ctx, idx.Name); err != nil {
			ov.Notes = append(ov.Notes, fmt.Sprintf("%s 통계를 읽지 못했습니다: %v", idx.Name, err))
		} else {
			col.Points = stats.total
			col.Fullness = stats.fullness
			if stats.dimension > 0 {
				col.Dimensions = stats.dimension
			}
			for _, ns := range stats.namespaces {
				col.Facts = append(col.Facts, Fact{
					Label: "네임스페이스 " + ns.name,
					Value: strconv.FormatInt(ns.count, 10) + "개",
				})
			}
		}
		ov.Collections = append(ov.Collections, col)
	}
	return ov, nil
}

func pineconeIndexType(spec map[string]any) string {
	if spec == nil {
		return ""
	}
	if _, ok := spec["serverless"]; ok {
		return "serverless"
	}
	if _, ok := spec["pod"]; ok {
		return "pod"
	}
	return ""
}

type pineconeNamespaceCount struct {
	name  string
	count int64
}

type pineconeStats struct {
	total      int64
	dimension  int
	fullness   float64
	namespaces []pineconeNamespaceCount
}

func (p *Pinecone) stats(ctx context.Context, index string) (*pineconeStats, error) {
	base, err := p.dataURL(index)
	if err != nil {
		return nil, err
	}
	var out struct {
		Namespaces map[string]struct {
			VectorCount int64 `json:"vectorCount"`
		} `json:"namespaces"`
		Dimension        int     `json:"dimension"`
		IndexFullness    float64 `json:"indexFullness"`
		TotalVectorCount int64   `json:"totalVectorCount"`
	}
	if err := p.call(ctx, http.MethodPost, base, "/describe_index_stats",
		map[string]any{}, &out); err != nil {
		return nil, err
	}
	st := &pineconeStats{
		total: out.TotalVectorCount, dimension: out.Dimension,
		fullness: out.IndexFullness * 100,
	}
	for name, ns := range out.Namespaces {
		label := name
		if label == "" {
			label = "(기본)"
		}
		st.namespaces = append(st.namespaces, pineconeNamespaceCount{name: label, count: ns.VectorCount})
	}
	sort.Slice(st.namespaces, func(i, j int) bool {
		return st.namespaces[i].count > st.namespaces[j].count
	})
	return st, nil
}

// ensureHosts는 데이터 평면 주소를 아직 모를 때 목록을 한 번 읽는다.
func (p *Pinecone) ensureHosts(ctx context.Context, index string) error {
	if strings.TrimSpace(p.cfg.Extra["index_host"]) != "" {
		return nil
	}
	if _, ok := p.hosts[index]; ok {
		return nil
	}
	var list pineconeIndexList
	if err := p.call(ctx, http.MethodGet, p.controlURL(), "/indexes", nil, &list); err != nil {
		return err
	}
	for _, idx := range list.Indexes {
		p.hosts[idx.Name] = idx.Host
		p.metrics[idx.Name] = NormalizeMetric(idx.Metric)
	}
	return nil
}

// Scroll은 인덱스를 훑는다.
//
// Pinecone 에는 "전부 나열하기"가 서버리스 인덱스에만 있다(POD 인덱스에는 없다).
// 없을 때 빈 목록을 보여주면 "비어 있다"로 읽히므로, 왜 볼 수 없는지 말한다.
func (p *Pinecone) Scroll(ctx context.Context, collection, cursor string, limit int, withVector bool) (*Page, error) {
	if err := p.ensureHosts(ctx, collection); err != nil {
		return nil, err
	}
	base, err := p.dataURL(collection)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	q := url.Values{}
	q.Set("limit", strconv.Itoa(limit))
	if ns := p.namespace(); ns != "" {
		q.Set("namespace", ns)
	}
	if cursor != "" {
		q.Set("paginationToken", cursor)
	}
	var out struct {
		Vectors []struct {
			ID string `json:"id"`
		} `json:"vectors"`
		Pagination struct {
			Next string `json:"next"`
		} `json:"pagination"`
	}
	if err := p.call(ctx, http.MethodGet, base, "/vectors/list?"+q.Encode(), nil, &out); err != nil {
		return nil, fmt.Errorf("%w — POD 인덱스에는 목록 API 가 없습니다(서버리스만 지원). "+
			"그때는 이웃 찾기로 둘러보세요", err)
	}
	page := &Page{Collection: collection, Next: out.Pagination.Next}
	if len(out.Vectors) == 0 {
		return page, nil
	}
	// 목록은 id 만 준다. 값과 메타데이터는 따로 읽어야 한다.
	ids := make([]string, 0, len(out.Vectors))
	for _, v := range out.Vectors {
		ids = append(ids, v.ID)
	}
	points, err := p.Fetch(ctx, collection, ids)
	if err != nil {
		page.Notes = append(page.Notes, "값을 읽지 못해 id 만 보여줍니다: "+err.Error())
		for _, id := range ids {
			page.Points = append(page.Points, Point{ID: id})
		}
		return page, nil
	}
	for _, pt := range points {
		if !withVector && len(pt.Vector) > PreviewDims {
			pt.Vector, pt.Truncated = Truncate(pt.Vector, PreviewDims)
		}
		page.Points = append(page.Points, pt)
	}
	return page, nil
}

func (p *Pinecone) Fetch(ctx context.Context, collection string, ids []string) ([]Point, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	if err := p.ensureHosts(ctx, collection); err != nil {
		return nil, err
	}
	base, err := p.dataURL(collection)
	if err != nil {
		return nil, err
	}
	q := url.Values{}
	for _, id := range ids {
		q.Add("ids", id)
	}
	if ns := p.namespace(); ns != "" {
		q.Set("namespace", ns)
	}
	var out struct {
		Vectors map[string]struct {
			ID       string         `json:"id"`
			Values   []float32      `json:"values"`
			Metadata map[string]any `json:"metadata"`
		} `json:"vectors"`
	}
	if err := p.call(ctx, http.MethodGet, base, "/vectors/fetch?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	// 요청한 순서를 지킨다. 맵을 그대로 훑으면 순서가 매번 달라져서, 비교 화면이
	// "왼쪽/오른쪽"을 요청한 대로 채우지 못한다.
	points := make([]Point, 0, len(ids))
	for _, id := range ids {
		v, ok := out.Vectors[id]
		if !ok {
			continue
		}
		points = append(points, Point{
			ID: v.ID, Vector: v.Values, Dimensions: len(v.Values), Payload: v.Metadata,
		})
	}
	return points, nil
}

func (p *Pinecone) Search(ctx context.Context, req SearchRequest) (*Result, error) {
	if err := p.ensureHosts(ctx, req.Collection); err != nil {
		return nil, err
	}
	base, err := p.dataURL(req.Collection)
	if err != nil {
		return nil, err
	}
	limit := req.Limit
	if limit <= 0 || limit > 200 {
		limit = 20
	}
	body := map[string]any{
		"topK": limit, "includeMetadata": req.WithPayload, "includeValues": true,
	}
	if ns := p.namespace(); ns != "" {
		body["namespace"] = ns
	}
	if len(req.Filter) > 0 {
		body["filter"] = req.Filter
	}
	res := &Result{Collection: req.Collection, QueryID: req.ID, Metric: p.metrics[req.Collection]}
	scoreKind := PineconeScoreKind(res.Metric)
	switch {
	case len(req.Vector) > 0:
		body["vector"] = req.Vector
		res.Query, res.Dimensions = req.Vector, len(req.Vector)
	case req.ID != "":
		// Pinecone 은 id 로 찾는 것을 서버가 직접 지원한다. 벡터를 먼저 읽어 오는
		// 왕복을 아낄 수 있다.
		body["id"] = req.ID
	default:
		return nil, fmt.Errorf("찾을 벡터나 기준 점의 id 가 필요합니다")
	}

	var out struct {
		Matches []struct {
			ID       string         `json:"id"`
			Score    float64        `json:"score"`
			Values   []float32      `json:"values"`
			Metadata map[string]any `json:"metadata"`
		} `json:"matches"`
	}
	start := time.Now()
	if err := p.call(ctx, http.MethodPost, base, "/query", body, &out); err != nil {
		return nil, err
	}
	res.ElapsedMs = float64(time.Since(start).Microseconds()) / 1000
	for _, m := range out.Matches {
		// Pinecone 의 score 는 인덱스의 거리 함수를 그대로 쓴다. cosine·
		// dotproduct 는 클수록 가깝고, euclidean 은 **작을수록** 가깝다.
		// 이 구분을 화면에 넘기지 않으면 목록이 거꾸로 정렬된다.
		pt := Point{
			ID: m.ID, Payload: m.Metadata, Dimensions: len(m.Values),
			Score: m.Score, ScoreKind: scoreKind,
		}
		if req.WithVector {
			pt.Vector = m.Values
		} else if len(m.Values) > 0 {
			pt.Vector, pt.Truncated = Truncate(m.Values, PreviewDims)
		}
		res.Points = append(res.Points, pt)
		// id 로 찾았고 기준 벡터를 아직 모르면, 자기 자신이 결과에 들어 있다.
		if res.Query == nil && m.ID == req.ID {
			res.Query, res.Dimensions = m.Values, len(m.Values)
		}
	}
	return res, nil
}

// PineconeScoreKind는 거리 함수에 따른 점수의 방향이다.
//
// 화면이 "가까운 순"을 그리려면 이 방향을 알아야 한다. euclidean 인덱스에서
// 큰 점수를 위에 두면 **가장 먼 것을 가장 가까운 것으로** 보여주게 된다.
func PineconeScoreKind(metric string) string {
	if NormalizeMetric(metric) == MetricEuclid {
		return ScoreDistance
	}
	return ScoreSimilarity
}
