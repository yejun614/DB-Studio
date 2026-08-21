package api

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
)

// 쓰기 전달.
//
// 리플리카는 자기 메타 DB에 쓰지 않는다. 쓰면 다음 복제 때 그 행이 사라지고 — 마스터에는
// 없는 행이므로 — 사용자는 "저장했는데 없어졌다"를 겪는다. 그래서 상태를 바꾸는 요청은
// 마스터로 넘기고, 그 답을 그대로 돌려준다.
//
// 세션 쿠키를 그대로 실어 보내는 이유: 세션도 복제되므로 마스터가 그 사람을 안다.
// 노드가 대신 로그인하거나 권한을 흉내 낼 필요가 없다 — 권한 판정은 언제나 마스터에서,
// 그 사람의 자격으로 이뤄진다.

// forwardTimeout은 마스터로 넘긴 요청을 기다리는 시간이다.
// 마이그레이션 적용처럼 오래 걸리는 작업이 있어 넉넉히 잡는다.
const forwardTimeout = 5 * time.Minute

// waitForSeq는 전달한 쓰기가 이 노드에 반영되기를 기다리는 시간이다.
//
// 기다리는 이유: 저장 직후 화면은 곧바로 목록을 다시 읽는다. 그 읽기가 아직 복제되지
// 않은 로컬 DB로 가면 방금 만든 것이 없다. 짧게 기다리면 그 어긋남이 사라진다.
const waitForSeq = 3 * time.Second

// clusterForward는 리플리카에서 상태 변경 요청을 마스터로 넘긴다.
func (s *Server) clusterForward(c *fiber.Ctx) error {
	if s.cluster == nil || !s.cluster.IsReplica() {
		return c.Next()
	}
	if !isStateChanging(c.Method()) || isClusterInternal(c.Path()) {
		return c.Next()
	}
	// 다른 노드가 "네가 실행하라"며 넘긴 요청과, 내가 담당인 DB에 대한 요청은 넘기지
	// 않는다. 넘기면 두 노드가 요청을 서로에게 던지는 고리가 된다.
	if c.Get(execHeader) != "" || c.Locals(localClusterExec) == true {
		return c.Next()
	}

	target := s.cluster.Config().MasterURL + c.OriginalURL()
	req, err := http.NewRequestWithContext(c.Context(), c.Method(), target, bytes.NewReader(c.Body()))
	if err != nil {
		return err
	}
	// 헤더를 골라 싣는다. 전부 옮기면 Host·Content-Length처럼 이 요청에만 맞는 값이
	// 따라가 마스터에서 다르게 해석된다.
	for _, h := range []string{
		fiber.HeaderContentType, fiber.HeaderCookie, fiber.HeaderAccept,
		"X-Requested-With", fiber.HeaderUserAgent,
	} {
		if v := c.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}
	// 마스터가 클라이언트 IP를 기록할 수 있게 한다(감사 로그의 IP가 리플리카 주소로
	// 통일되면 "누가 어디서 했는가"가 사라진다).
	req.Header.Set(fiber.HeaderXForwardedFor, clientIP(c))
	req.Header.Set("X-Cluster-Forwarded", s.cluster.NodeID())

	client := &http.Client{Timeout: forwardTimeout}
	res, err := client.Do(req)
	if err != nil {
		slog.Warn("마스터로 요청을 넘기지 못했습니다",
			"method", c.Method(), "path", c.Path(), "err", err)
		return fail(c, fiber.StatusServiceUnavailable, "master_unreachable",
			"마스터 노드에 닿지 못해 변경을 저장할 수 없습니다. 조회는 계속 가능합니다")
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return fail(c, fiber.StatusBadGateway, "master_read", "마스터의 응답을 읽지 못했습니다")
	}

	// 응답 헤더 중 의미가 있는 것만 옮긴다. Set-Cookie는 반드시 옮겨야 한다 —
	// 리플리카에서 로그인하면 세션 쿠키를 만드는 쪽이 마스터이기 때문이다.
	if ct := res.Header.Get(fiber.HeaderContentType); ct != "" {
		c.Set(fiber.HeaderContentType, ct)
	}
	for _, sc := range res.Header.Values(fiber.HeaderSetCookie) {
		c.Response().Header.Add(fiber.HeaderSetCookie, sc)
	}

	// 마스터가 알려 준 복제 지점까지 따라잡고 나서 답한다.
	if seq, _ := strconv.ParseInt(res.Header.Get(clusterSeqHeader), 10, 64); seq > 0 {
		if !s.cluster.WaitApplied(c.Context(), seq, waitForSeq) {
			// 늦어지는 것과 실패는 다르다. 변경은 이미 마스터에 저장되어 있으므로
			// 실패로 바꾸지 않고, 화면이 알 수 있게 표시만 남긴다.
			c.Set("X-Cluster-Stale", "1")
		}
	}
	return c.Status(res.StatusCode).Send(body)
}

// clusterSeqHeader는 마스터가 응답에 싣는 복제 지점이다.
const clusterSeqHeader = "X-Cluster-Seq"

// clusterSeqStamp는 마스터가 상태 변경 응답에 지금 복제 지점을 실어 보낸다.
//
// 응답에 싣는 이유: 리플리카가 "내 쓰기가 어디까지 반영되면 되는가"를 알 수 있는 유일한
// 시점이 이 응답이다. 나중에 물어보면 그 사이의 다른 쓰기까지 기다리게 된다.
func (s *Server) clusterSeqStamp(c *fiber.Ctx) error {
	err := c.Next()
	if s.cluster == nil || !s.cluster.IsMaster() || !isStateChanging(c.Method()) {
		return err
	}
	if _, max, e := s.st.ReplBounds(c.Context()); e == nil && max > 0 {
		c.Set(clusterSeqHeader, strconv.FormatInt(max, 10))
	}
	return err
}

// isClusterInternal은 노드끼리 부르는 경로인지 본다. 이 경로는 넘기지 않는다
// (넘기면 마스터가 다시 그 노드를 부르는 고리가 생긴다).
func isClusterInternal(path string) bool {
	return strings.HasPrefix(path, "/api/v1/node/")
}
