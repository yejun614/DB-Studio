package api

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"

	"dbstudio/internal/buildinfo"
	"dbstudio/internal/mcp"
	"dbstudio/internal/model"
	"dbstudio/internal/store"
)

// MCP 엔드포인트.
//
// 세 가지가 이 파일의 설계다.
//
//  1. **툴 레지스트리를 앱 안의 AI 어시스턴트와 공유한다.** 두 벌을 만들면 어느 한쪽에만
//     있는 툴이 생기고, 더 나쁘게는 권한 검사가 한쪽에만 들어간다.
//  2. **인증은 API 토큰뿐이다.** 세션 쿠키를 받지 않는다 — 쿠키를 받으면 브라우저가
//     자동으로 실어 보내므로 다른 사이트가 이 엔드포인트를 부를 수 있게 된다(CSRF).
//     토큰은 클라이언트가 헤더에 직접 넣어야 하고, 그것이 이 경로의 유일한 관문이다.
//  3. **쓰기 툴은 토큰 범위로 가른다.** 앱 안에서는 사용자가 화면에서 승인 버튼을 누르지만
//     MCP에는 그 화면이 없다. 대신 토큰을 발급할 때 쓰기를 허용할지 고르게 하고,
//     허용해도 서버 쪽 게이트(승인 수·확인 문구·능력 판정)는 그대로 적용된다.
//
// 인증·툴 노출 판정·실행은 REST API(rest_handlers.go)와 같은 함수를 쓴다(tokenapi.go).
// 이 파일에 남는 것은 **프로토콜을 입히는 일**뿐이다 — 그것이 두 문의 유일한 차이다.

// handleMCPGet은 서버→클라이언트 스트림 요청에 답한다.
//
// 우리는 서버가 먼저 말을 거는 일이 없으므로(알림·샘플링을 쓰지 않는다) 스트림을
// 열어 줄 이유가 없다. 규약은 이 경우 405를 돌려주라고 한다.
func (s *Server) handleMCPGet(c *fiber.Ctx) error {
	c.Set("Allow", "POST")
	return c.Status(fiber.StatusMethodNotAllowed).JSON(fiber.Map{
		"error": "이 서버는 서버 발신 스트림을 쓰지 않습니다. POST로 요청하세요",
	})
}

// handleMCP는 JSON-RPC 메시지를 처리한다.
func (s *Server) handleMCP(c *fiber.Ctx) error {
	u, token, err := s.mcpAuthenticate(c)
	if err != nil {
		// 인증 실패는 JSON-RPC 오류가 아니라 HTTP 401이다. 클라이언트가
		// "자격증명 문제"와 "요청 내용 문제"를 구분할 수 있어야 한다.
		c.Set("WWW-Authenticate", `Bearer realm="dbstudio"`)
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
			"error": "unauthorized", "message": err.Error(),
		})
	}

	messages, isBatch, perr := mcp.ParseMessages(c.Body())
	if perr != nil {
		return c.Status(fiber.StatusBadRequest).JSON(mcp.Response{
			JSONRPC: "2.0",
			Error:   &mcp.RPCError{Code: mcp.CodeParseError, Message: "JSON을 해석할 수 없습니다"},
		})
	}

	responses := make([]*mcp.Response, 0, len(messages))
	for _, msg := range messages {
		if res := s.mcpDispatch(c, u, token, msg); res != nil {
			responses = append(responses, res)
		}
	}

	// 알림만 들어왔으면 돌려줄 것이 없다. 규약은 202를 쓰라고 한다.
	if len(responses) == 0 {
		return c.SendStatus(fiber.StatusAccepted)
	}
	c.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
	if isBatch {
		return c.JSON(responses)
	}
	return c.JSON(responses[0])
}

// mcpAuthenticate는 Bearer 토큰으로 사용자를 찾는다.
func (s *Server) mcpAuthenticate(c *fiber.Ctx) (*model.User, *store.APIToken, error) {
	return s.authenticateBearer(c, "mcp.auth.denied")
}

// mcpDispatch는 메시지 하나를 처리한다. 알림이면 nil을 반환한다.
func (s *Server) mcpDispatch(c *fiber.Ctx, u *model.User, token *store.APIToken, msg *mcp.Request) *mcp.Response {
	if msg.JSONRPC != "" && msg.JSONRPC != "2.0" {
		return mcp.NewError(msg.ID, mcp.CodeInvalidRequest, "jsonrpc는 2.0이어야 합니다", nil)
	}

	switch msg.Method {
	case "initialize":
		return s.mcpInitialize(u, token, msg)

	case "notifications/initialized", "notifications/cancelled":
		// 알림에는 응답하지 않는다.
		return nil

	case "ping":
		return mcp.NewResult(msg.ID, map[string]any{})

	case "tools/list":
		return s.mcpToolsList(c, u, token, msg)

	case "tools/call":
		return s.mcpToolsCall(c, u, token, msg)

	case "resources/list", "resources/templates/list":
		// 리소스는 제공하지 않는다. 빈 목록을 돌려주는 이유: 메서드를 모른다고
		// 답하면 일부 클라이언트가 연결 자체를 실패로 처리한다.
		return mcp.NewResult(msg.ID, map[string]any{"resources": []any{}})

	case "prompts/list":
		return mcp.NewResult(msg.ID, map[string]any{"prompts": []any{}})

	default:
		if msg.IsNotificationMethod() {
			return nil
		}
		return mcp.NewError(msg.ID, mcp.CodeMethodNotFound,
			fmt.Sprintf("지원하지 않는 메서드입니다: %s", msg.Method), nil)
	}
}

func (s *Server) mcpInitialize(u *model.User, token *store.APIToken, msg *mcp.Request) *mcp.Response {
	var params struct {
		ProtocolVersion string `json:"protocolVersion"`
		ClientInfo      struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"clientInfo"`
	}
	_ = json.Unmarshal(msg.Params, &params)

	return mcp.NewResult(msg.ID, mcp.InitializeResult{
		ProtocolVersion: mcp.NegotiateVersion(params.ProtocolVersion),
		// listChanged는 쓰지 않는다. 툴 목록은 사용자의 권한이 바뀔 때만 달라지고,
		// 그때는 어차피 클라이언트가 다시 연결한다.
		Capabilities: map[string]any{"tools": map[string]any{}},
		ServerInfo: mcp.ServerInfo{
			Name:    "dbstudio",
			Title:   "DB Studio",
			Version: buildinfo.Get().Version,
		},
		Instructions: mcp.Instructions(u.Username, token.Scope),
	})
}

// mcpTools는 이 토큰으로 쓸 수 있는 툴 목록을 MCP 형식으로 만든다.
//
// 무엇을 보여줄지는 tokenTools가 정한다. 여기서 하는 일은 주석(annotations)을 붙이는
// 것뿐이며, 그것이 MCP에만 있는 개념이다.
func (s *Server) mcpTools(c *fiber.Ctx, u *model.User, token *store.APIToken) []mcp.Tool {
	defs, _ := s.tokenTools(c, u, token)

	out := make([]mcp.Tool, 0, len(defs))
	for _, def := range defs {
		out = append(out, mcp.Tool{
			Name: def.Name, Description: def.Description, InputSchema: def.Schema,
			Annotations: &mcp.ToolAnnotations{
				ReadOnlyHint: !def.Mutating,
				// 클라이언트가 사람에게 확인을 받게 만드는 힌트다. 앱 화면의
				// 승인 버튼이 하던 일을 MCP에서는 클라이언트가 한다.
				DestructiveHint: def.Mutating,
			},
		})
	}
	return out
}

func (s *Server) mcpToolsList(c *fiber.Ctx, u *model.User, token *store.APIToken, msg *mcp.Request) *mcp.Response {
	return mcp.NewResult(msg.ID, map[string]any{"tools": s.mcpTools(c, u, token)})
}

func (s *Server) mcpToolsCall(c *fiber.Ctx, u *model.User, token *store.APIToken, msg *mcp.Request) *mcp.Response {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(msg.Params, &params); err != nil {
		return mcp.NewError(msg.ID, mcp.CodeInvalidParams, "params를 해석할 수 없습니다", nil)
	}
	if strings.TrimSpace(params.Name) == "" {
		return mcp.NewError(msg.ID, mcp.CodeInvalidParams, "툴 이름(name)이 필요합니다", nil)
	}

	// 목록을 만들 때와 같은 규칙으로 접근 가능 여부를 다시 판정한다.
	// 목록은 편의이고 이것이 실제 관문이다.
	def, reason := s.findTokenTool(c, u, token, params.Name)
	if def == nil {
		return mcp.NewResult(msg.ID, mcp.TextResult(
			fmt.Sprintf("%s 라는 툴이 없습니다. tools/list로 사용 가능한 툴을 확인하세요", params.Name), true))
	}
	if reason != "" {
		s.auditToolDenied(c, "mcp.tool.denied", u, token, params.Name, reason)
		return mcp.NewResult(msg.ID, mcp.TextResult(reason, true))
	}

	// 툴 실행 컨텍스트. 요청 컨텍스트에서 파생하되 시간 제한을 건다.
	//
	// 응답을 이 핸들러가 직접 쓰므로(스트리밍이 아니다) 요청 컨텍스트를 그대로 써도
	// 안전하다. 클라이언트가 끊으면 툴도 함께 멈추는 것이 옳다 — 아무도 기다리지 않는
	// 조회를 대상 DB에 계속 걸어 둘 이유가 없다.
	ctx, cancel := context.WithTimeout(c.Context(), toolCallTimeout)
	defer cancel()

	tc := &toolContext{ctx: ctx, srv: s, user: u, ip: clientIP(c)}
	started := time.Now()

	output, err := s.runTokenTool(tc, def, params.Arguments)
	elapsed := time.Since(started)

	s.auditToolCall(c, "mcp.tool.call", u, token, def, elapsed, err)

	if err != nil {
		// 툴 오류는 JSON-RPC 오류가 아니라 isError로 돌려준다.
		// 그래야 모델이 오류를 읽고 고쳐서 다시 부를 수 있다.
		return mcp.NewResult(msg.ID, mcp.TextResult(err.Error(), true))
	}
	return mcp.NewResult(msg.ID, mcp.TextResult(output, false))
}
