package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"

	"dbstudio/internal/auth"
	"dbstudio/internal/model"
	"dbstudio/internal/store"
)

// 토큰을 들고 들어오는 **프로그램 경로**가 함께 쓰는 관문.
//
// 지금 그 문은 둘이다 — MCP(`/mcp`, JSON-RPC)와 REST API(`/api/tools`, 평범한 HTTP).
// 프로토콜은 다르지만 물어야 할 것은 똑같다: 누구의 토큰인가, 그 범위로 이 툴을 쓸 수
// 있는가, 쓰기 툴이면 실행 전에 무엇을 검증하는가. 그래서 그 판정을 여기 한 벌만 둔다.
//
// 문마다 따로 쓰면 언젠가 한쪽에만 검사가 들어가고, 그 사실은 사고가 난 뒤에야 드러난다.
// 툴 레지스트리를 어시스턴트와 공유하는 이유(ai_tools.go)와 같은 이야기다.

// toolCallTimeout은 툴 하나의 실행 시간 상한이다.
// 클라이언트가 무한정 기다리지 않도록 툴 자체의 제한보다 약간 길게 잡는다.
const toolCallTimeout = 6 * time.Minute

// localToken은 Bearer 토큰으로 인증된 요청의 토큰을 담는 Locals 키다.
const localToken = "apiToken"

func currentToken(c *fiber.Ctx) *store.APIToken {
	t, _ := c.Locals(localToken).(*store.APIToken)
	return t
}

// bearerToken은 Authorization 헤더에서 토큰 원문을 꺼낸다.
func bearerToken(c *fiber.Ctx) string {
	header := c.Get(fiber.HeaderAuthorization)
	if len(header) > 7 && strings.EqualFold(header[:7], "bearer ") {
		return strings.TrimSpace(header[7:])
	}
	return ""
}

// authenticateBearer는 Bearer 토큰으로 사용자를 찾는다.
//
// **세션 쿠키를 받지 않는다.** 쿠키를 받으면 브라우저가 자동으로 실어 보내므로 다른
// 사이트가 이 엔드포인트를 부를 수 있게 된다(CSRF). 토큰은 클라이언트가 헤더에 직접
// 넣어야 하고, 그것이 이 경로들의 유일한 문이다.
//
// deniedAction은 실패를 남길 감사 로그의 이름이다. 문마다 다르게 남겨야 유출을
// 의심할 때 "어느 경로로 시도했는가"를 셀 수 있다.
func (s *Server) authenticateBearer(c *fiber.Ctx, deniedAction string) (*model.User, *store.APIToken, error) {
	raw := bearerToken(c)
	if raw == "" {
		return nil, nil, errors.New("Authorization: Bearer <토큰> 헤더가 필요합니다")
	}

	u, token, err := s.authn.AuthenticateToken(c.Context(), raw, clientIP(c))
	if err != nil {
		// 실패한 접근도 남긴다. 토큰이 유출되면 이 기록이 유일한 단서다.
		s.audit(c, store.AuditParams{
			Action: deniedAction, Result: "denied",
			Detail: map[string]any{"reason": err.Error(), "userAgent": c.Get(fiber.HeaderUserAgent)},
		})
		// 계정 상태 때문에 막힌 경우는 그대로 알려 준다. 토큰 자체는 유효하고
		// 그 사실은 토큰을 든 사람이 이미 아는 것이므로 숨겨서 얻는 것이 없다.
		// 반면 "왜 갑자기 안 되는가"를 모르면 유효한 토큰을 계속 재발급하게 된다.
		if errors.Is(err, auth.ErrAccountDisabled) || errors.Is(err, auth.ErrTOTPRequired) {
			return nil, nil, err
		}
		return nil, nil, auth.ErrInvalidToken
	}
	return u, token, nil
}

// tokenTools는 이 토큰으로 쓸 수 있는 툴 정의를 이름 순으로 만든다.
//
// 읽기 토큰에는 쓰기 툴을 아예 보여주지 않는다. 목록에 있는데 부르면 거부되는 툴은
// 클라이언트(와 모델)가 계속 시도하게 만든다.
func (s *Server) tokenTools(c *fiber.Ctx, u *model.User, token *store.APIToken) ([]*aiTool, map[string]*aiTool) {
	tools, registry := availableTools(u, s.toolHints(c, u))

	out := make([]*aiTool, 0, len(tools))
	for _, t := range tools {
		def := registry[t.Name]
		if def == nil {
			continue
		}
		if def.Mutating && token.Scope != store.TokenScopeWrite {
			continue
		}
		out = append(out, def)
	}
	return out, registry
}

// findTokenTool은 이름으로 툴을 찾고 이 토큰으로 쓸 수 있는지 함께 판정한다.
//
// 목록을 만들 때와 **같은 규칙으로 다시 계산하는 것**이 핵심이다. 목록은 편의이고
// 이것이 실제 관문이다 — 목록에서만 걸러 두면 이름을 직접 적어 부르는 클라이언트가
// 그대로 통과한다.
//
// def가 nil이면 그런 이름의 툴이 없다. reason이 비어 있지 않으면 거부이며,
// 그 문장은 그대로 호출자에게 보여도 되는 것이다.
func (s *Server) findTokenTool(c *fiber.Ctx, u *model.User, token *store.APIToken, name string) (def *aiTool, reason string) {
	tools, registry := s.tokenTools(c, u, token)
	def = registry[name]
	if def == nil {
		return nil, ""
	}
	for _, t := range tools {
		if t.Name == name {
			return def, ""
		}
	}
	if def.Mutating && token.Scope != store.TokenScopeWrite {
		return def, "이 토큰은 읽기 전용입니다. 변경 툴을 쓰려면 쓰기 범위의 토큰이 필요합니다"
	}
	return def, "이 툴을 쓸 권한이 없습니다"
}

// runTokenTool은 툴 하나를 실행한다.
//
// 쓰기 툴은 Propose를 먼저 부른다. 앱 화면에서는 그 결과를 사용자에게 보여주고
// 승인을 받지만, 여기서는 **검증 단계**로 쓴다 — Propose는 아무것도 바꾸지 않으면서
// 인자와 권한을 확인하므로, 그것을 건너뛰면 Apply 안에서만 잡히는 실수가 생긴다.
func (s *Server) runTokenTool(tc *toolContext, def *aiTool, args json.RawMessage) (string, error) {
	if !def.Mutating {
		if def.Run == nil {
			return "", fmt.Errorf("%s 툴에 구현이 없습니다", def.Name)
		}
		return def.Run(tc, args)
	}
	if def.Propose == nil || def.Apply == nil {
		return "", fmt.Errorf("%s 툴에 구현이 없습니다", def.Name)
	}
	if _, _, err := def.Propose(tc, args); err != nil {
		return "", err
	}
	return def.Apply(tc, args)
}

// auditToolDenied는 거부된 툴 호출을 남긴다.
func (s *Server) auditToolDenied(c *fiber.Ctx, action string, u *model.User, token *store.APIToken, name, reason string) {
	s.audit(c, store.AuditParams{
		ActorID: u.ID, ActorName: u.Username,
		Action: action, TargetType: "tool", TargetID: name, Result: "denied",
		Detail: map[string]any{"token": token.Name, "scope": token.Scope, "reason": reason},
	})
}

// auditToolCall은 실행된 툴 호출을 남긴다. err가 있으면 실패로 기록한다.
func (s *Server) auditToolCall(c *fiber.Ctx, action string, u *model.User, token *store.APIToken,
	def *aiTool, elapsed time.Duration, err error) {
	result := "ok"
	detail := map[string]any{
		"token": token.Name, "scope": token.Scope,
		"mutating": def.Mutating, "ms": elapsed.Milliseconds(),
	}
	if err != nil {
		result = "error"
		detail["error"] = err.Error()
	}
	s.audit(c, store.AuditParams{
		ActorID: u.ID, ActorName: u.Username,
		Action: action, TargetType: "tool", TargetID: def.Name,
		Result: result, Detail: detail,
	})
}
