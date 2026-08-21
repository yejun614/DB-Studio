package api

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"

	"dbstudio/internal/buildinfo"
	"dbstudio/internal/store"
)

// REST API — MCP와 같은 툴을 평범한 HTTP로 연다.
//
// MCP가 있는데 이것을 따로 두는 이유는 하나다: **MCP 클라이언트가 아닌 것들이 있다.**
// 셸 스크립트, CI 잡, 사내 대시보드, 다른 언어의 서비스는 JSON-RPC 봉투를 만들고
// initialize를 협상할 이유가 없다. 그들에게 필요한 것은 `curl -H "Authorization: Bearer …"`
// 한 줄이다.
//
// 세 가지가 이 파일의 설계다.
//
//  1. **툴 레지스트리와 관문을 MCP와 공유한다**(tokenapi.go). 기능을 REST로 다시 구현하면
//     어느 한쪽에만 있는 기능이 생기고, 더 나쁘게는 권한 검사가 한쪽에만 들어간다.
//     여기서 새로 만드는 것은 **표현**뿐이다 — 경로, 상태 코드, 응답 형태.
//  2. **오류는 HTTP 상태 코드로 답한다.** MCP는 툴 오류를 `isError` 텍스트로 돌려준다
//     (그래야 모델이 읽고 고쳐 다시 부른다). REST 클라이언트는 모델이 아니라 프로그램이고,
//     `if res.ok` 로 분기한다. 200에 오류를 숨겨 두면 그 분기가 조용히 틀린다.
//  3. **결과는 파싱된 JSON으로 준다.** 툴 결과는 대부분 JSON 문자열이므로 문자열째로
//     넘기면 클라이언트가 한 번 더 파싱해야 한다.

// restBasePath는 툴 엔드포인트의 뿌리다. 토큰 화면이 이 값을 그대로 보여준다.
const restBasePath = "/api/tools"

// requireToken은 Bearer 토큰을 검증하고 Locals에 사용자와 토큰을 심는다.
//
// requireAuth(세션 쿠키)와 짝을 이루지만 서로를 대신하지 않는다. 이 경로는 쿠키를
// 받지 않고, 저 경로는 토큰을 받지 않는다 — 두 자격증명의 수명과 위협 모형이 다르다.
func (s *Server) requireToken(c *fiber.Ctx) error {
	u, token, err := s.authenticateBearer(c, "api.auth.denied")
	if err != nil {
		// 401에는 WWW-Authenticate를 붙인다. 클라이언트가 "자격증명 문제"와
		// "요청 내용 문제"를 구분할 수 있어야 한다.
		c.Set(fiber.HeaderWWWAuthenticate, `Bearer realm="dbstudio"`)
		return fail(c, fiber.StatusUnauthorized, "unauthorized", err.Error())
	}
	c.Locals(localUser, u)
	c.Locals(localToken, token)
	return c.Next()
}

// restTool은 목록·상세에 실리는 툴 하나다.
type restTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	// Mutating이면 무언가를 바꾼다. 클라이언트가 확인 단계를 넣을 근거이며,
	// MCP의 destructiveHint와 같은 사실을 REST 어휘로 옮긴 것이다.
	Mutating bool `json:"mutating"`
	// Path는 이 툴을 부를 주소다. 이름을 문자열로 이어 붙이게 하지 않는다 —
	// 경로 규칙이 바뀌어도 클라이언트가 따라올 수 있어야 한다.
	Path string `json:"path"`
}

func toRESTTool(def *aiTool) restTool {
	return restTool{
		Name:        def.Name,
		Description: def.Description,
		InputSchema: def.Schema,
		Mutating:    def.Mutating,
		Path:        restBasePath + "/" + def.Name,
	}
}

// handleRESTIdentity는 토큰의 주인과 범위를 알려준다.
//
// 클라이언트가 처음 붙었을 때 "이 토큰이 살아 있는가, 무엇을 할 수 있는가"를
// 툴을 부르지 않고 확인할 수 있어야 한다. MCP의 initialize가 하던 일이다.
func (s *Server) handleRESTIdentity(c *fiber.Ctx) error {
	u, token := currentUser(c), currentToken(c)
	tools, _ := s.tokenTools(c, u, token)

	return c.JSON(fiber.Map{
		"user": fiber.Map{
			"id": u.ID, "username": u.Username,
			"displayName": u.DisplayName, "role": u.Role,
		},
		"token": fiber.Map{
			"name": token.Name, "scope": token.Scope, "expiresAt": token.ExpiresAt,
		},
		"toolCount": len(tools),
		"version":   buildinfo.Get().Version,
		// 경로를 함께 준다. 주소 하나만 설정에 적고 나머지는 여기서 찾게 한다.
		"paths": fiber.Map{"tools": restBasePath, "mcp": "/mcp"},
	})
}

// handleRESTToolList는 이 토큰으로 쓸 수 있는 툴을 나열한다.
func (s *Server) handleRESTToolList(c *fiber.Ctx) error {
	u, token := currentUser(c), currentToken(c)
	defs, _ := s.tokenTools(c, u, token)

	out := make([]restTool, 0, len(defs))
	for _, def := range defs {
		out = append(out, toRESTTool(def))
	}
	return c.JSON(fiber.Map{"tools": out, "count": len(out), "scope": token.Scope})
}

// handleRESTToolGet은 툴 하나의 스키마를 돌려준다.
//
// 목록에 이미 스키마가 있는데 따로 두는 이유: 인자 하나를 확인하려고 38종의 스키마를
// 통째로 받는 것은 낭비이고, 스크립트가 "이 툴이 아직 있는가"를 묻는 가장 싼 방법이다.
func (s *Server) handleRESTToolGet(c *fiber.Ctx) error {
	u, token := currentUser(c), currentToken(c)
	name := strings.TrimSpace(c.Params("name"))

	def, reason := s.findTokenTool(c, u, token, name)
	if def == nil {
		return s.restToolNotFound(c, name)
	}
	if reason != "" {
		// 조회 단계에서도 거부는 남긴다. 못 쓰는 툴의 스키마만 반복해서 읽는
		// 클라이언트는 대개 잘못된 범위의 토큰을 들고 있다.
		s.auditToolDenied(c, "api.tool.denied", u, token, name, reason)
		return fail(c, fiber.StatusForbidden, "forbidden", reason)
	}
	return c.JSON(toRESTTool(def))
}

// restCallResponse는 툴 실행 결과다.
type restCallResponse struct {
	Tool string `json:"tool"`
	// Result는 툴 결과를 파싱한 JSON이다.
	Result json.RawMessage `json:"result,omitempty"`
	// Text는 결과가 JSON이 아닐 때의 원문이다. 둘 중 하나만 채워진다.
	Text     string `json:"text,omitempty"`
	Mutating bool   `json:"mutating"`
	MS       int64  `json:"ms"`
}

// handleRESTToolCall은 툴을 실행한다. 본문이 곧 인자 객체다.
func (s *Server) handleRESTToolCall(c *fiber.Ctx) error {
	u, token := currentUser(c), currentToken(c)
	name := strings.TrimSpace(c.Params("name"))

	def, reason := s.findTokenTool(c, u, token, name)
	if def == nil {
		return s.restToolNotFound(c, name)
	}
	if reason != "" {
		s.auditToolDenied(c, "api.tool.denied", u, token, name, reason)
		code := "forbidden"
		if def.Mutating && token.Scope != store.TokenScopeWrite {
			// 범위 때문에 막힌 것은 권한 부족과 다른 문제다. 코드가 같으면
			// 클라이언트는 "토큰을 다시 발급하면 된다"는 사실을 알 수 없다.
			code = "read_only_token"
		}
		return fail(c, fiber.StatusForbidden, code, reason)
	}

	args, err := restArguments(c.Body())
	if err != nil {
		return failDetail(c, fiber.StatusBadRequest, "bad_request",
			"본문은 인자를 담은 JSON 객체여야 합니다", err.Error())
	}

	// 툴 실행 컨텍스트. 요청 컨텍스트에서 파생하되 시간 제한을 건다.
	// 클라이언트가 끊으면 툴도 함께 멈추는 것이 옳다 — 아무도 기다리지 않는
	// 조회를 대상 DB에 계속 걸어 둘 이유가 없다.
	ctx, cancel := context.WithTimeout(c.Context(), toolCallTimeout)
	defer cancel()

	tc := &toolContext{ctx: ctx, srv: s, user: u, ip: clientIP(c)}
	started := time.Now()
	output, runErr := s.runTokenTool(tc, def, args)
	elapsed := time.Since(started)

	s.auditToolCall(c, "api.tool.call", u, token, def, elapsed, runErr)

	if runErr != nil {
		// 422를 쓰는 이유: 요청 자체는 형식이 맞았고(400이 아니다) 서버가 고장 난 것도
		// 아니다(500이 아니다). 툴이 "그 커넥션에는 접근할 수 없다", "그런 테이블이 없다"
		// 같은 판정을 내린 것이고, 그것은 요청 내용의 문제다.
		return failDetail(c, fiber.StatusUnprocessableEntity, "tool_failed",
			runErr.Error(), name)
	}

	res := restCallResponse{Tool: name, Mutating: def.Mutating, MS: elapsed.Milliseconds()}
	res.Result, res.Text = restResult(output)
	return c.JSON(res)
}

// restToolNotFound는 없는 툴에 대한 응답이다.
//
// 목록 경로를 함께 알려준다. 이름을 잘못 적은 클라이언트가 다음에 무엇을 해야 하는지
// 오류 문구만 보고 알 수 있어야 한다.
func (s *Server) restToolNotFound(c *fiber.Ctx, name string) error {
	if name == "" {
		return fail(c, fiber.StatusNotFound, "unknown_tool", "툴 이름이 필요합니다")
	}
	return failDetail(c, fiber.StatusNotFound, "unknown_tool",
		name+" 라는 툴이 없습니다", "사용 가능한 툴은 GET "+restBasePath+" 에서 확인하세요")
}

// restArguments는 요청 본문을 툴 인자로 읽는다.
//
// 빈 본문을 허용하는 이유: 인자가 없는 툴(list_erd_documents 같은)을 부르는 데
// `-d '{}'` 를 요구할 이유가 없다. 반대로 배열이나 문자열은 거부한다 — 툴 스키마는
// 전부 객체이고, 그것을 여기서 걸러야 오류가 "인자를 해석할 수 없습니다"라는
// 툴 안쪽 문구가 아니라 400으로 나온다.
func restArguments(body []byte) (json.RawMessage, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return json.RawMessage("{}"), nil
	}
	var probe map[string]any
	if err := json.Unmarshal(trimmed, &probe); err != nil {
		return nil, err
	}
	// null은 "인자 없음"으로 읽는다. JSON으로는 유효하지만 객체가 아니므로
	// 그대로 넘기면 툴마다 다르게 깨진다.
	if probe == nil {
		return json.RawMessage("{}"), nil
	}
	return json.RawMessage(trimmed), nil
}

// restResult는 툴이 돌려준 문자열을 응답에 담을 형태로 바꾼다.
//
// 툴 결과는 대부분 JSON이다(asJSON). 유효한 JSON이면 그대로 본문에 심어 클라이언트가
// 두 번 파싱하지 않게 한다.
//
// 유효하지 않은 경우가 실제로 있다: 결과가 상한을 넘으면 asJSON이 잘라내고 안내 문장을
// 덧붙이므로 더 이상 JSON이 아니다. 그때는 원문을 text로 준다 — 잘린 JSON을 억지로
// 고쳐 넘기면 클라이언트는 그것이 결과 전부라고 믿는다.
func restResult(out string) (json.RawMessage, string) {
	trimmed := strings.TrimSpace(out)
	if trimmed != "" && json.Valid([]byte(trimmed)) {
		return json.RawMessage(trimmed), ""
	}
	return nil, out
}
