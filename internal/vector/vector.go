// Package vector는 벡터 DB(Qdrant·Pinecone·pgvector)를 다룬다.
//
// 왜 dbx 가 아니라 별도 패키지인가: 벡터 DB 의 질문이 다르다. 표와 행으로 읽으면
// "몇 행인가"까지는 말할 수 있지만, 정작 필요한 것 — 차원 수, 거리 함수, 색인이
// 준비됐는가, 이 벡터와 가까운 것이 무엇인가 — 은 그 모델에 자리가 없다.
// dbx.Adapter 에 억지로 끼우면 절반이 "미지원"인 어댑터가 되고, 화면은 그 절반을
// 숨기는 조건문으로 채워진다(하둡·Ceph 를 storage 로 뺀 것과 같은 판단이다).
//
// 대신 접점은 둘로 나눈다.
//   - 접속 확인과 **지표 수집**은 dbx 어댑터로 붙인다. 그래야 임계값·이벤트·알림이
//     이미 하던 일을 그대로 한다.
//   - 컬렉션 목록·훑어보기·이웃 찾기·비교는 이 패키지가 맡고, 전용 화면이 부른다.
//
// **읽기 전용이다.** 벡터를 지우거나 고치는 것은 되돌릴 수 없고, 임베딩은 대개
// 다른 파이프라인이 만들어 넣는다. 이 화면에서 한 줄을 고치면 그 파이프라인이
// 다음에 덮어쓰거나, 반대로 영영 어긋난 채 남는다.
package vector

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
)

// Kind는 이 패키지가 다루는 종류다.
const (
	KindQdrant   = "qdrant"
	KindPinecone = "pinecone"
	KindPgVector = "pgvector"
)

// 거리 함수. 종류마다 이름이 다르지만(Qdrant 는 Cosine, Pinecone 은 cosine,
// pgvector 는 연산자) 뜻은 같으므로 한 어휘로 모은다 — 그래야 화면이 종류마다
// 다른 말을 하지 않는다.
const (
	MetricCosine = "cosine"
	MetricEuclid = "euclid"
	MetricDot    = "dot"
)

// MetricLabel은 거리 함수의 사람 말 이름이다.
func MetricLabel(m string) string {
	switch strings.ToLower(strings.TrimSpace(m)) {
	case MetricCosine:
		return "코사인 거리"
	case MetricEuclid:
		return "유클리드 거리"
	case MetricDot:
		return "내적"
	case "":
		return "알 수 없음"
	}
	return m
}

// NormalizeMetric은 종류마다 다른 이름을 한 어휘로 모은다.
func NormalizeMetric(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "cosine", "cos", "vector_cosine_ops":
		return MetricCosine
	case "euclid", "euclidean", "l2", "l2_norm", "vector_l2_ops":
		return MetricEuclid
	case "dot", "dotproduct", "ip", "inner_product", "vector_ip_ops":
		return MetricDot
	}
	return strings.ToLower(strings.TrimSpace(raw))
}

// Fact는 종류마다 다른 요약값이다(라벨 → 값). 화면은 순서를 지켜 그대로 보여준다.
type Fact struct {
	Label string `json:"label"`
	Value string `json:"value"`
	Help  string `json:"help,omitempty"`
}

// Collection은 벡터가 담긴 곳 하나다.
//
// 종류마다 부르는 이름이 다르다: Qdrant 는 컬렉션, Pinecone 은 인덱스,
// pgvector 는 표의 벡터 컬럼 하나다. 화면이 세 이름을 다 알아야 할 이유가 없으므로
// 한 낱말로 모은다.
type Collection struct {
	Name string `json:"name"`
	// Dimensions는 벡터의 길이다. 0이면 알 수 없다(비어 있는 컬렉션 등).
	Dimensions int    `json:"dimensions"`
	Metric     string `json:"metric"`
	// Points는 담긴 벡터 수다. -1이면 모른다.
	Points int64 `json:"points"`
	// Indexed는 색인에 올라간 벡터 수다. -1이면 이 종류가 알려주지 않는다.
	//
	// Points 와 따로 두는 이유가 중요하다: 색인이 따라오지 못한 벡터도 검색은
	// 된다(전수 조사로 떨어진다). 즉 **틀린 답이 아니라 느린 답**이 나오므로,
	// 느려진 이유를 이 두 수의 차이 말고는 알 방법이 없다.
	Indexed int64 `json:"indexed"`
	// Status는 green | yellow | red | unknown 이다.
	Status string `json:"status"`
	// IndexType은 색인 방식이다(hnsw, ivfflat, ...). 없으면 빈 문자열.
	IndexType string `json:"indexType,omitempty"`
	// Fullness는 인덱스 사용률이다(Pinecone). -1이면 모른다.
	Fullness float64 `json:"fullness"`
	// PayloadKeys는 이 컬렉션의 메타데이터 열쇠다(표본에서 모은 것이라 전부가 아니다).
	PayloadKeys []string `json:"payloadKeys,omitempty"`
	Facts       []Fact   `json:"facts,omitempty"`
	// Note는 이 컬렉션에 대해 사람이 알아야 할 사실이다.
	Note string `json:"note,omitempty"`
}

// Point는 벡터 하나다.
type Point struct {
	ID     string    `json:"id"`
	Vector []float32 `json:"vector,omitempty"`
	// Truncated가 참이면 Vector 는 앞부분만이다.
	//
	// 자르는 이유: 1536 차원짜리를 백 개 보내면 그 자체로 몇 MB 다. 화면이 실제로
	// 그리는 것은 앞머리 몇 개와 요약이므로, 전부가 필요한 자리(비교)에서만 따로 읽는다.
	Truncated bool `json:"truncated,omitempty"`
	// Dimensions는 자르기 전의 길이다.
	Dimensions int            `json:"dimensions,omitempty"`
	Payload    map[string]any `json:"payload,omitempty"`
	// Score는 검색 결과일 때의 점수다. 종류마다 뜻이 다르므로 ScoreKind 를 함께 본다.
	Score float64 `json:"score,omitempty"`
	// ScoreKind는 similarity(클수록 가깝다) 또는 distance(작을수록 가깝다)다.
	//
	// 이것을 함께 보내지 않으면 화면이 정렬 방향을 짐작해야 하고, 짐작이 틀리면
	// **가장 먼 것을 가장 가까운 것으로** 보여주게 된다. 조용히 뒤집히는 종류의 오류다.
	ScoreKind string `json:"scoreKind,omitempty"`
}

const (
	ScoreSimilarity = "similarity"
	ScoreDistance   = "distance"
)

// Page는 훑어보기 한 장이다.
type Page struct {
	Collection string  `json:"collection"`
	Points     []Point `json:"points"`
	// Next는 다음 장을 요청할 때 그대로 돌려보내는 값이다. 비어 있으면 끝이다.
	Next  string   `json:"next,omitempty"`
	Notes []string `json:"notes,omitempty"`
}

// SearchRequest는 이웃 찾기 요청이다.
type SearchRequest struct {
	Collection string
	// Vector가 있으면 그것으로 찾는다.
	Vector []float32
	// ID가 있으면 그 벡터를 먼저 읽어 그것으로 찾는다("이것과 비슷한 것").
	//
	// 두 길을 모두 두는 이유: 사람이 손으로 1536개의 수를 적는 일은 없다. 실제
	// 쓰임은 "이 문서와 비슷한 것을 찾아 줘"이고, 그때 기준은 이미 들어 있는 점이다.
	ID          string
	Limit       int
	WithVector  bool
	WithPayload bool
	// Filter는 종류별 필터다(Qdrant 의 filter, Pinecone 의 metadata filter).
	// 해석하지 않고 그대로 넘긴다 — 우리가 만든 어휘로 옮기면 두 종류의 표현력
	// 차이가 조용히 사라진다.
	Filter map[string]any
}

// Result는 이웃 찾기 결과다.
type Result struct {
	Collection string `json:"collection"`
	// Query는 실제로 검색에 쓴 벡터다(ID 로 찾았을 때 그 점의 벡터).
	Query      []float32 `json:"query,omitempty"`
	QueryID    string    `json:"queryId,omitempty"`
	Dimensions int       `json:"dimensions"`
	Metric     string    `json:"metric"`
	Points     []Point   `json:"points"`
	ElapsedMs  float64   `json:"elapsedMs"`
	Notes      []string  `json:"notes,omitempty"`
}

// Overview는 한 서버(또는 데이터베이스)의 벡터 개요다.
type Overview struct {
	Kind        string       `json:"kind"`
	Version     string       `json:"version,omitempty"`
	Collections []Collection `json:"collections"`
	Facts       []Fact       `json:"facts,omitempty"`
	Notes       []string     `json:"notes,omitempty"`
}

// Store는 벡터 화면이 쓰는 통로다.
type Store interface {
	Kind() string
	Ping(ctx context.Context) (string, error)
	Overview(ctx context.Context) (*Overview, error)
	// Scroll은 컬렉션을 훑는다. cursor 는 앞선 Page.Next 다.
	Scroll(ctx context.Context, collection, cursor string, limit int, withVector bool) (*Page, error)
	// Fetch는 id 로 점을 읽는다. 비교 화면이 쓴다.
	Fetch(ctx context.Context, collection string, ids []string) ([]Point, error)
	Search(ctx context.Context, req SearchRequest) (*Result, error)
	Close() error
}

// ---------- 비교 ----------

// Comparison은 벡터 둘을 견준 결과다.
//
// 세 가지 거리를 **모두** 낸다. 컬렉션이 코사인으로 색인돼 있어도 사람이 확인하려는
// 것은 대개 "왜 이 둘이 가깝다고 나오지"이고, 그 답이 코사인에는 안 보이고
// 유클리드에는 보이는 일이 흔하다(길이가 다른데 방향이 같은 경우가 그렇다).
type Comparison struct {
	Dimensions int `json:"dimensions"`
	// Cosine은 코사인 **유사도**다(-1..1, 클수록 가깝다). 거리가 아니다.
	Cosine float64 `json:"cosine"`
	// Euclid는 유클리드 거리다(0 이상, 작을수록 가깝다).
	Euclid float64 `json:"euclid"`
	// Dot은 내적이다.
	Dot float64 `json:"dot"`
	// NormA·NormB는 각 벡터의 길이다. 코사인만 보면 사라지는 정보라 함께 낸다.
	NormA float64 `json:"normA"`
	NormB float64 `json:"normB"`
	// TopDeltas는 차이가 큰 차원들이다. "어디서 갈리는가"를 보는 유일한 창이다.
	TopDeltas []Delta  `json:"topDeltas"`
	Notes     []string `json:"notes,omitempty"`
}

// Delta는 한 차원에서의 차이다.
type Delta struct {
	Index int     `json:"index"`
	A     float64 `json:"a"`
	B     float64 `json:"b"`
	Diff  float64 `json:"diff"`
}

// Compare는 벡터 둘을 견준다.
//
// 길이가 다르면 견주지 않는다. 짧은 쪽에 맞춰 자르면 숫자는 나오지만 그 숫자는
// 아무 뜻이 없다 — 서로 다른 모델이 만든 임베딩을 비교한 값이기 때문이다.
func Compare(a, b []float32, topN int) (*Comparison, error) {
	if len(a) == 0 || len(b) == 0 {
		return nil, fmt.Errorf("비교할 벡터가 비어 있습니다")
	}
	if len(a) != len(b) {
		return nil, fmt.Errorf("차원이 다릅니다 (%d vs %d) — 다른 모델이 만든 벡터는 견줄 수 없습니다",
			len(a), len(b))
	}
	if topN <= 0 {
		topN = 12
	}
	c := &Comparison{Dimensions: len(a)}
	var dot, sumA, sumB, sumSq float64
	deltas := make([]Delta, 0, len(a))
	for i := range a {
		av, bv := float64(a[i]), float64(b[i])
		dot += av * bv
		sumA += av * av
		sumB += bv * bv
		d := av - bv
		sumSq += d * d
		deltas = append(deltas, Delta{Index: i, A: av, B: bv, Diff: d})
	}
	c.Dot = dot
	c.NormA, c.NormB = math.Sqrt(sumA), math.Sqrt(sumB)
	c.Euclid = math.Sqrt(sumSq)
	if c.NormA == 0 || c.NormB == 0 {
		// 영벡터에는 방향이 없어서 코사인이 정의되지 않는다. 0을 돌려주면
		// "직교한다"로 읽히는데, 그것은 사실이 아니라 계산할 수 없다는 뜻이다.
		c.Notes = append(c.Notes,
			"한쪽이 영벡터라 코사인 유사도를 계산할 수 없습니다(방향이 없습니다)")
	} else {
		c.Cosine = dot / (c.NormA * c.NormB)
	}

	sort.Slice(deltas, func(i, j int) bool {
		return math.Abs(deltas[i].Diff) > math.Abs(deltas[j].Diff)
	})
	if len(deltas) > topN {
		deltas = deltas[:topN]
	}
	c.TopDeltas = deltas
	return c, nil
}

// Truncate는 화면에 보낼 만큼만 남기고 자른 사본을 만든다.
func Truncate(v []float32, max int) ([]float32, bool) {
	if max <= 0 || len(v) <= max {
		return v, false
	}
	out := make([]float32, max)
	copy(out, v[:max])
	return out, true
}

// PreviewDims는 목록에 실어 보내는 성분 수다.
const PreviewDims = 16
