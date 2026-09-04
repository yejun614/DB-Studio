package vector

import (
	"math"
	"strings"
	"testing"
)

// 비교의 값이 맞아야 한다. 손으로 계산할 수 있는 벡터로 확인한다 —
// 우리가 만든 값과 우리가 만든 기댓값을 견주면 아무것도 확인하지 못한다.
func TestCompareKnownVectors(t *testing.T) {
	// (3,4) 와 (4,3): 길이가 둘 다 5, 내적 24, 코사인 24/25 = 0.96,
	// 유클리드 sqrt(1+1) = sqrt(2).
	c, err := Compare([]float32{3, 4}, []float32{4, 3}, 5)
	if err != nil {
		t.Fatalf("비교 실패: %v", err)
	}
	near := func(name string, got, want float64) {
		t.Helper()
		if math.Abs(got-want) > 1e-6 {
			t.Errorf("%s = %v, 기댓값 %v", name, got, want)
		}
	}
	near("내적", c.Dot, 24)
	near("코사인", c.Cosine, 0.96)
	near("유클리드", c.Euclid, math.Sqrt2)
	near("길이 A", c.NormA, 5)
	near("길이 B", c.NormB, 5)
	if len(c.TopDeltas) != 2 {
		t.Fatalf("차이 목록이 %d개입니다", len(c.TopDeltas))
	}
}

// 차원이 다르면 견주지 않는다. 짧은 쪽에 맞춰 자르면 숫자는 나오지만 그 숫자는
// 서로 다른 모델의 임베딩을 비교한 값이라 아무 뜻이 없다.
func TestCompareRefusesDifferentDimensions(t *testing.T) {
	_, err := Compare([]float32{1, 2, 3}, []float32{1, 2}, 5)
	if err == nil {
		t.Fatal("차원이 다른데 비교했습니다")
	}
	if !strings.Contains(err.Error(), "차원") {
		t.Errorf("거절 사유가 이유를 말하지 않습니다: %v", err)
	}
}

// 영벡터에는 방향이 없어 코사인이 정의되지 않는다. 0을 조용히 돌려주면
// "직교한다"로 읽히는데, 그것은 사실이 아니라 계산할 수 없다는 뜻이다.
func TestCompareZeroVectorSaysSo(t *testing.T) {
	c, err := Compare([]float32{0, 0}, []float32{1, 1}, 5)
	if err != nil {
		t.Fatalf("비교 실패: %v", err)
	}
	if len(c.Notes) == 0 {
		t.Error("영벡터라는 사실을 말하지 않습니다")
	}
}

// 차이가 큰 차원이 앞에 와야 한다. "어디서 갈리는가"를 보는 유일한 창이다.
func TestCompareSortsByAbsoluteDifference(t *testing.T) {
	c, _ := Compare([]float32{0, 0, 0}, []float32{0.1, -5, 1}, 2)
	if len(c.TopDeltas) != 2 {
		t.Fatalf("상위 %d개", len(c.TopDeltas))
	}
	if c.TopDeltas[0].Index != 1 {
		t.Errorf("차이가 가장 큰 차원이 앞이 아닙니다: %+v", c.TopDeltas)
	}
	if c.TopDeltas[1].Index != 2 {
		t.Errorf("두 번째가 틀립니다: %+v", c.TopDeltas)
	}
}

// 거리 함수의 이름은 종류마다 다르지만 뜻은 같다. 한 어휘로 모으지 않으면
// 화면이 종류마다 다른 말을 한다.
func TestNormalizeMetric(t *testing.T) {
	cases := map[string]string{
		"Cosine": MetricCosine, "vector_cosine_ops": MetricCosine,
		"Euclid": MetricEuclid, "l2": MetricEuclid, "vector_l2_ops": MetricEuclid,
		"dotproduct": MetricDot, "vector_ip_ops": MetricDot,
	}
	for in, want := range cases {
		if got := NormalizeMetric(in); got != want {
			t.Errorf("NormalizeMetric(%q) = %q, 기댓값 %q", in, got, want)
		}
	}
}

// 유클리드 인덱스의 점수는 **거리**다. 유사도로 다루면 화면이 가장 먼 것을
// 가장 가까운 것으로 보여준다.
func TestPineconeScoreDirection(t *testing.T) {
	if got := PineconeScoreKind("euclidean"); got != ScoreDistance {
		t.Errorf("유클리드 점수를 %q 로 봤습니다", got)
	}
	for _, m := range []string{"cosine", "dotproduct"} {
		if got := PineconeScoreKind(m); got != ScoreSimilarity {
			t.Errorf("%s 점수를 %q 로 봤습니다", m, got)
		}
	}
}

// vector(1536) 에서 차원을 읽는다. 차원을 정하지 않은 컬럼은 0이다(색인을 만들 수 없다).
func TestDimensionsOf(t *testing.T) {
	cases := map[string]int{
		"vector(1536)": 1536, "halfvec(768)": 768, "vector": 0, "sparsevec(4)": 4,
	}
	for in, want := range cases {
		if got := dimensionsOf(in); got != want {
			t.Errorf("dimensionsOf(%q) = %d, 기댓값 %d", in, got, want)
		}
	}
}

// 색인을 만든 연산자 클래스와 **같은** 연산자로 찾아야 색인을 탄다.
func TestDistanceOperatorMatchesOpclass(t *testing.T) {
	cases := map[string]string{
		"vector_cosine_ops": "<=>", "vector_l2_ops": "<->", "vector_ip_ops": "<#>",
	}
	for opclass, want := range cases {
		if got, _ := distanceOperator(NormalizeMetric(opclass)); got != want {
			t.Errorf("%s → %q, 기댓값 %q", opclass, got, want)
		}
	}
}

// 컬렉션 이름은 왕복해야 한다. 갈라 읽지 못하면 조회가 엉뚱한 표를 가리킨다.
func TestCollectionNameRoundTrip(t *testing.T) {
	cases := [][3]string{
		{"public", "docs", "embedding"},
		{"rag", "chunks", "vec"},
	}
	for _, c := range cases {
		name := CollectionName(c[0], c[1], c[2])
		s, tb, col, err := splitCollection(name)
		if err != nil {
			t.Fatalf("%q 를 갈라 읽지 못했습니다: %v", name, err)
		}
		if s != c[0] || tb != c[1] || col != c[2] {
			t.Errorf("%q → (%q, %q, %q), 기댓값 %v", name, s, tb, col, c)
		}
	}
	if _, _, _, err := splitCollection("한덩어리"); err == nil {
		t.Error("이름이 하나뿐인데 받아들였습니다")
	}
}

// 벡터 리터럴은 왕복해야 한다. 여기서 정확도를 잃으면 검색 결과가 조용히 달라진다.
func TestVectorLiteralRoundTrip(t *testing.T) {
	in := []float32{0.1, -2.5, 3, 0}
	got, err := parseVectorLiteral(vectorLiteral(in))
	if err != nil {
		t.Fatalf("해석 실패: %v", err)
	}
	if len(got) != len(in) {
		t.Fatalf("길이 %d, 기댓값 %d", len(got), len(in))
	}
	for i := range in {
		if got[i] != in[i] {
			t.Errorf("%d 번째 = %v, 기댓값 %v", i, got[i], in[i])
		}
	}
	// 희소 벡터는 아직 읽지 않는다. 조용히 빈 벡터를 돌려주면 "값이 없다"로 보인다.
	if _, err := parseVectorLiteral("{1:0.5,3:0.2}/1536"); err == nil {
		t.Error("희소 벡터를 조용히 받아들였습니다")
	}
}

// 목록에 실어 보낼 때는 앞부분만 자른다. 1536 차원짜리 백 개는 그 자체로 몇 MB 다.
func TestTruncateMarksCut(t *testing.T) {
	v := make([]float32, 100)
	got, cut := Truncate(v, PreviewDims)
	if !cut || len(got) != PreviewDims {
		t.Errorf("자르지 않았습니다: len=%d cut=%v", len(got), cut)
	}
	if _, cut := Truncate([]float32{1, 2}, PreviewDims); cut {
		t.Error("짧은 벡터를 잘랐다고 표시했습니다")
	}
}
