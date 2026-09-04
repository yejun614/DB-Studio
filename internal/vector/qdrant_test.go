package vector

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"dbstudio/internal/opsapi"
)

// 가짜 Qdrant 로 응답 해석을 확인한다.
//
// 실제 서버 없이 확인하려는 것은 **응답을 우리가 제대로 읽는가**다. 그 지점이
// 조용히 틀리기 쉬운 곳이다 — 이름 붙은 벡터, 수치 id, 점수의 방향은 모두
// 틀려도 오류가 나지 않고 화면에 엉뚱한 값이 그려질 뿐이다.
func fakeQdrant(t *testing.T, routes map[string]string) *Qdrant {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := routes[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"status":{"error":"없는 경로"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("가짜 서버 주소: %v", err)
	}
	port, _ := strconv.Atoi(u.Port())
	return NewQdrant(opsapi.Config{
		Scheme: "http", Host: u.Hostname(), Port: port, Extra: map[string]string{},
	})
}

// 이름 붙은 벡터를 모르는 채로 읽으면 차원 수가 0으로 나오고, 화면은 그 컬렉션을
// "비어 있다"처럼 보여준다.
func TestQdrantNamedVectors(t *testing.T) {
	q := fakeQdrant(t, map[string]string{
		"/":            `{"title":"qdrant","version":"1.12.0"}`,
		"/collections": `{"result":{"collections":[{"name":"docs"},{"name":"imgs"}]}}`,
		"/collections/docs": `{"result":{"status":"green","points_count":120,
			"indexed_vectors_count":100,"segments_count":3,
			"payload_schema":{"tag":{"data_type":"keyword"}},
			"config":{"params":{"vectors":{"size":768,"distance":"Cosine"}},
			          "hnsw_config":{"m":16,"ef_construct":100}}}}`,
		"/collections/imgs": `{"result":{"status":"yellow","points_count":5,
			"indexed_vectors_count":0,"segments_count":1,
			"config":{"params":{"vectors":{"clip":{"size":512,"distance":"Dot"}}}}}}`,
	})
	ov, err := q.Overview(context.Background())
	if err != nil {
		t.Fatalf("개요 실패: %v", err)
	}
	if len(ov.Collections) != 2 {
		t.Fatalf("컬렉션 %d개", len(ov.Collections))
	}
	docs, imgs := ov.Collections[0], ov.Collections[1]
	if docs.Dimensions != 768 || docs.Metric != MetricCosine {
		t.Errorf("단일 벡터 설정을 잘못 읽었습니다: %+v", docs)
	}
	if docs.Points != 120 || docs.Indexed != 100 {
		t.Errorf("개수를 잘못 읽었습니다: %+v", docs)
	}
	if !strings.Contains(docs.Note, "색인이 아직") {
		t.Errorf("색인이 따라오지 못한 사실을 말하지 않습니다: %q", docs.Note)
	}
	if imgs.Dimensions != 512 || imgs.Metric != MetricDot {
		t.Errorf("이름 붙은 벡터를 읽지 못했습니다: %+v", imgs)
	}
	if !strings.Contains(imgs.Note, "이름 붙은 벡터") {
		t.Errorf("이름 붙은 벡터라는 사실을 말하지 않습니다: %q", imgs.Note)
	}
	if len(docs.PayloadKeys) != 1 || docs.PayloadKeys[0] != "tag" {
		t.Errorf("메타데이터 열쇠를 읽지 못했습니다: %v", docs.PayloadKeys)
	}
}

// 하나를 못 읽었다고 목록 전체를 버리면, 권한이 컬렉션마다 다른 서버에서
// 아무것도 못 보게 된다.
func TestQdrantKeepsOtherCollectionsOnError(t *testing.T) {
	q := fakeQdrant(t, map[string]string{
		"/":            `{"version":"1.12.0"}`,
		"/collections": `{"result":{"collections":[{"name":"ok"},{"name":"denied"}]}}`,
		"/collections/ok": `{"result":{"status":"green","points_count":1,
			"config":{"params":{"vectors":{"size":4,"distance":"Cosine"}}}}}`,
	})
	ov, err := q.Overview(context.Background())
	if err != nil {
		t.Fatalf("개요 실패: %v", err)
	}
	if len(ov.Collections) != 2 {
		t.Fatalf("컬렉션 %d개 — 실패한 것도 자리를 지켜야 합니다", len(ov.Collections))
	}
	if len(ov.Notes) == 0 {
		t.Error("읽지 못한 이유를 적지 않았습니다")
	}
}

// 수치 id 를 %v 로 찍으면 1e+06 같은 표기가 나오고, 그 문자열로 다시 조회하면
// 찾지 못한다. 조용히 "없는 점"이 되는 종류의 오류다.
func TestQdrantNumericIDs(t *testing.T) {
	q := fakeQdrant(t, map[string]string{
		"/collections/docs/points/scroll": `{"result":{"points":[
			{"id":1000000,"vector":[0.1,0.2],"payload":{"t":"a"}},
			{"id":"9c1f-uuid","vector":[0.3,0.4]}],"next_page_offset":1000001}}`,
	})
	page, err := q.Scroll(context.Background(), "docs", "", 10, true)
	if err != nil {
		t.Fatalf("훑기 실패: %v", err)
	}
	if page.Points[0].ID != "1000000" {
		t.Errorf("수치 id = %q, 기댓값 \"1000000\"", page.Points[0].ID)
	}
	if page.Points[1].ID != "9c1f-uuid" {
		t.Errorf("문자열 id = %q", page.Points[1].ID)
	}
	if page.Next != "1000001" {
		t.Errorf("다음 장 커서 = %q", page.Next)
	}
}

// Qdrant 의 score 는 언제나 유사도다(클수록 가깝다). 거리로 다루면 목록이
// 거꾸로 정렬된다.
func TestQdrantSearchScoreIsSimilarity(t *testing.T) {
	q := fakeQdrant(t, map[string]string{
		"/collections/docs/points/search": `{"result":[
			{"id":1,"score":0.98,"vector":[1,0],"payload":{"t":"가깝다"}},
			{"id":2,"score":0.11,"vector":[0,1]}]}`,
	})
	res, err := q.Search(context.Background(), SearchRequest{
		Collection: "docs", Vector: []float32{1, 0}, Limit: 5, WithPayload: true,
	})
	if err != nil {
		t.Fatalf("검색 실패: %v", err)
	}
	if len(res.Points) != 2 {
		t.Fatalf("결과 %d개", len(res.Points))
	}
	if res.Points[0].ScoreKind != ScoreSimilarity {
		t.Errorf("점수의 방향을 %q 로 봤습니다", res.Points[0].ScoreKind)
	}
	if res.Dimensions != 2 {
		t.Errorf("기준 벡터의 차원 = %d", res.Dimensions)
	}
}

// 목록에 실을 때는 벡터를 자른다. 1536 차원짜리 백 개는 그 자체로 몇 MB 다.
func TestQdrantScrollTruncatesVectors(t *testing.T) {
	long := make([]float32, 100)
	buf, _ := json.Marshal(long)
	q := fakeQdrant(t, map[string]string{
		"/collections/docs/points/scroll": `{"result":{"points":[
			{"id":1,"vector":` + string(buf) + `}]}}`,
	})
	page, err := q.Scroll(context.Background(), "docs", "", 10, false)
	if err != nil {
		t.Fatalf("훑기 실패: %v", err)
	}
	p := page.Points[0]
	if !p.Truncated || len(p.Vector) != PreviewDims {
		t.Errorf("자르지 않았습니다: len=%d truncated=%v", len(p.Vector), p.Truncated)
	}
	if p.Dimensions != 100 {
		t.Errorf("자르기 전의 길이를 잃었습니다: %d", p.Dimensions)
	}
}

// 인증 실패는 "요청이 거절됐습니다"가 아니라 무엇을 고쳐야 하는지 말해야 한다.
func TestQdrantAuthErrorSaysWhat(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())
	q := NewQdrant(opsapi.Config{Scheme: "http", Host: u.Hostname(), Port: port,
		Extra: map[string]string{}})

	_, err := q.Overview(context.Background())
	if err == nil || !strings.Contains(err.Error(), "API 키") {
		t.Errorf("인증 실패의 사유를 말하지 않습니다: %v", err)
	}
}
