package api

import (
	"strconv"
	"strings"

	"github.com/gofiber/fiber/v2"

	"dbstudio/internal/dbx"
	"dbstudio/internal/model"
	"dbstudio/internal/vector"
)

// 벡터 DB(Qdrant·Pinecone·pgvector) API.
//
// 권한은 DB와 같은 규칙이다. 커넥션 등급이 이미 "누가 이 대상을 볼 수 있는가"를
// 정하고 있으므로 벡터라고 별도의 권한 체계를 만들 이유가 없다. 모든 조회는
// 모니터링 등급 이상이고, **쓰기는 없다** — 임베딩은 다른 파이프라인이 만들어
// 넣는 것이라, 이 화면에서 한 줄을 고치면 그 파이프라인이 다음에 덮어쓰거나
// 반대로 영영 어긋난 채 남는다.

// resolveVector는 커넥션을 벡터 저장소로 만든다.
//
// 호출자는 반드시 Close 해야 한다. pgvector 는 여기서 PostgreSQL 커넥션을 열기
// 때문이다 — 닫지 않으면 화면을 열 때마다 커넥션이 하나씩 샌다.
func (s *Server) resolveVector(c *fiber.Ctx) (*model.Connection, vector.Store, error) {
	id := c.Params("id")
	conn, err := s.st.GetConnection(c.Context(), id)
	if err != nil {
		return nil, nil, err
	}
	if !dbx.SupportsVector(conn.Kind) {
		return nil, nil, fiber.NewError(fiber.StatusBadRequest,
			"이 커넥션에서는 벡터를 볼 수 없습니다")
	}
	d, err := s.requireLevel(c, conn.ID, model.LevelMonitor)
	if err != nil {
		return nil, nil, err
	}
	if !d.Allowed {
		return nil, nil, fiber.NewError(fiber.StatusForbidden, d.Reason)
	}
	if !conn.Enabled {
		return nil, nil, fiber.NewError(fiber.StatusConflict, "비활성 상태인 커넥션입니다")
	}
	secret, err := s.st.GetSecret(c.Context(), conn.ID)
	if err != nil {
		return nil, nil, err
	}
	store, err := dbx.VectorStore(dbx.Target{Conn: conn, Secret: secret})
	if err != nil {
		return nil, nil, fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	return conn, store, nil
}

// handleVectorOverview는 컬렉션 목록과 상태다.
func (s *Server) handleVectorOverview(c *fiber.Ctx) error {
	conn, store, err := s.resolveVector(c)
	if err != nil {
		return err
	}
	defer store.Close()

	ov, err := store.Overview(c.Context())
	if err != nil {
		// pgvector 에서 확장이 없을 때가 여기로 온다. 그것은 서버 장애가 아니라
		// "여기에는 볼 것이 없다"이므로, 무엇을 하면 되는지까지 오류에 들어 있다.
		return failDetail(c, fiber.StatusBadGateway, "vector_unreachable",
			"벡터 정보를 읽지 못했습니다", err.Error())
	}
	return c.JSON(fiber.Map{
		"overview": ov,
		"kind":     conn.Kind,
		// 화면이 어떤 단추를 그릴지 정하는 근거다. 종류 이름으로 화면에서 분기하면
		// 종류가 늘 때마다 화면을 고쳐야 한다.
		"features": vectorFeatures(conn.Kind),
	})
}

// vectorFeatures는 이 종류에서 쓸 수 있는 기능이다.
func vectorFeatures(kind model.DBKind) fiber.Map {
	switch kind {
	case model.KindPinecone:
		// 목록 API 는 서버리스 인덱스에만 있다. 있다고 그려 놓고 실패하는 것보다
		// 미리 "이웃 찾기로 둘러보라"고 말하는 편이 낫다.
		return fiber.Map{"scroll": true, "scrollNote": "POD 인덱스에는 목록 API가 없습니다",
			"searchById": true, "filter": true}
	case model.KindQdrant:
		return fiber.Map{"scroll": true, "searchById": true, "filter": true}
	}
	// pgvector. 필터는 SQL WHERE 절이라 여기서 받지 않는다 — 임의의 SQL 을
	// 조각으로 받으면 그것이 곧 SQL 주입의 통로가 된다. 조건이 필요하면
	// 데이터 화면이나 SQL 콘솔에서 본다.
	return fiber.Map{"scroll": true, "searchById": true, "filter": false}
}

// handleVectorScroll은 컬렉션을 훑는다.
func (s *Server) handleVectorScroll(c *fiber.Ctx) error {
	_, store, err := s.resolveVector(c)
	if err != nil {
		return err
	}
	defer store.Close()

	collection := strings.TrimSpace(c.Query("collection"))
	if collection == "" {
		return fail(c, fiber.StatusBadRequest, "bad_request", "컬렉션을 고르세요")
	}
	limit, _ := strconv.Atoi(c.Query("limit", "50"))
	page, err := store.Scroll(c.Context(), collection, c.Query("cursor"), limit, false)
	if err != nil {
		return failDetail(c, fiber.StatusBadGateway, "scroll_failed",
			"벡터를 읽지 못했습니다", err.Error())
	}
	return c.JSON(fiber.Map{"page": page})
}

// vectorSearchBody는 이웃 찾기 입력이다.
type vectorSearchBody struct {
	Collection string    `json:"collection"`
	Vector     []float32 `json:"vector"`
	ID         string    `json:"id"`
	Limit      int       `json:"limit"`
	// WithVector가 참이면 결과에 벡터 전체를 싣는다. 비교 화면이 쓴다.
	WithVector bool `json:"withVector"`
	// Filter는 종류별 필터를 그대로 넘긴다.
	Filter map[string]any `json:"filter"`
}

// handleVectorSearch는 이웃을 찾는다.
func (s *Server) handleVectorSearch(c *fiber.Ctx) error {
	_, store, err := s.resolveVector(c)
	if err != nil {
		return err
	}
	defer store.Close()

	var body vectorSearchBody
	if err := c.BodyParser(&body); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "요청 본문을 해석할 수 없습니다")
	}
	if strings.TrimSpace(body.Collection) == "" {
		return fail(c, fiber.StatusBadRequest, "bad_request", "컬렉션을 고르세요")
	}
	if len(body.Vector) == 0 && strings.TrimSpace(body.ID) == "" {
		return fail(c, fiber.StatusBadRequest, "bad_request",
			"찾을 벡터를 넣거나 기준이 될 점의 id 를 고르세요")
	}
	res, err := store.Search(c.Context(), vector.SearchRequest{
		Collection: body.Collection, Vector: body.Vector, ID: strings.TrimSpace(body.ID),
		Limit: body.Limit, WithVector: body.WithVector, WithPayload: true,
		Filter: body.Filter,
	})
	if err != nil {
		return failDetail(c, fiber.StatusBadGateway, "search_failed",
			"이웃을 찾지 못했습니다", err.Error())
	}
	return c.JSON(fiber.Map{"result": res})
}

// vectorCompareBody는 비교 입력이다.
type vectorCompareBody struct {
	Collection string `json:"collection"`
	// A·B는 비교할 점의 id 다. 벡터를 직접 넣을 수도 있다.
	A       string    `json:"a"`
	B       string    `json:"b"`
	VectorA []float32 `json:"vectorA"`
	VectorB []float32 `json:"vectorB"`
}

// handleVectorCompare는 벡터 둘을 견준다.
//
// 계산을 서버에서 하는 이유: 1536 차원짜리 둘을 브라우저로 보내면 그것만으로도
// 수백 KB 이고, 차원마다의 차이를 정렬하는 일은 화면이 할 일이 아니다. 무엇보다
// **거리 계산이 두 곳에 있으면 언젠가 갈라진다** — 검색 결과의 점수와 비교 화면의
// 값이 서로 다른 수를 말하기 시작하면 어느 쪽이 맞는지 아무도 모른다.
func (s *Server) handleVectorCompare(c *fiber.Ctx) error {
	_, store, err := s.resolveVector(c)
	if err != nil {
		return err
	}
	defer store.Close()

	var body vectorCompareBody
	if err := c.BodyParser(&body); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "요청 본문을 해석할 수 없습니다")
	}
	a, b := body.VectorA, body.VectorB
	labels := [2]string{"", ""}

	// id 로 넘어온 쪽은 읽어 온다. 한 번에 읽는 이유: 두 번 왕복하면 그 사이에
	// 값이 바뀔 수 있고, 그때 화면은 서로 다른 시점의 두 벡터를 견준 값을 보게 된다.
	need := []string{}
	if len(a) == 0 && strings.TrimSpace(body.A) != "" {
		need = append(need, strings.TrimSpace(body.A))
	}
	if len(b) == 0 && strings.TrimSpace(body.B) != "" {
		need = append(need, strings.TrimSpace(body.B))
	}
	if len(need) > 0 {
		if strings.TrimSpace(body.Collection) == "" {
			return fail(c, fiber.StatusBadRequest, "bad_request", "컬렉션을 고르세요")
		}
		points, ferr := store.Fetch(c.Context(), body.Collection, need)
		if ferr != nil {
			return failDetail(c, fiber.StatusBadGateway, "fetch_failed",
				"벡터를 읽지 못했습니다", ferr.Error())
		}
		found := map[string][]float32{}
		for _, p := range points {
			found[p.ID] = p.Vector
		}
		if len(a) == 0 && body.A != "" {
			if v, ok := found[strings.TrimSpace(body.A)]; ok {
				a, labels[0] = v, body.A
			} else {
				return fail(c, fiber.StatusNotFound, "not_found", body.A+" 점을 찾지 못했습니다")
			}
		}
		if len(b) == 0 && body.B != "" {
			if v, ok := found[strings.TrimSpace(body.B)]; ok {
				b, labels[1] = v, body.B
			} else {
				return fail(c, fiber.StatusNotFound, "not_found", body.B+" 점을 찾지 못했습니다")
			}
		}
	}

	cmp, err := vector.Compare(a, b, 16)
	if err != nil {
		return fail(c, fiber.StatusBadRequest, "compare_failed", err.Error())
	}
	return c.JSON(fiber.Map{
		"comparison": cmp,
		"a":          labels[0], "b": labels[1],
	})
}

// handleVectorFetch는 id 로 점을 읽는다(비교 화면의 고르개가 쓴다).
func (s *Server) handleVectorFetch(c *fiber.Ctx) error {
	_, store, err := s.resolveVector(c)
	if err != nil {
		return err
	}
	defer store.Close()

	collection := strings.TrimSpace(c.Query("collection"))
	ids := []string{}
	for _, id := range strings.Split(c.Query("ids"), ",") {
		if id = strings.TrimSpace(id); id != "" {
			ids = append(ids, id)
		}
	}
	if collection == "" || len(ids) == 0 {
		return fail(c, fiber.StatusBadRequest, "bad_request", "컬렉션과 id 가 필요합니다")
	}
	if len(ids) > 20 {
		ids = ids[:20]
	}
	points, err := store.Fetch(c.Context(), collection, ids)
	if err != nil {
		return failDetail(c, fiber.StatusBadGateway, "fetch_failed",
			"벡터를 읽지 못했습니다", err.Error())
	}
	return c.JSON(fiber.Map{"points": points})
}
