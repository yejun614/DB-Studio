package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/valyala/fasthttp"

	"dbstudio/internal/ai"
	"dbstudio/internal/applog"
	"dbstudio/internal/model"
	"dbstudio/internal/store"
)

// ---------- 프로바이더 설정 ----------

func (s *Server) handleListAIProviders(c *fiber.Ctx) error {
	items, err := s.st.ListAIProviders(c.Context())
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{
		"items": items,
		"kinds": []fiber.Map{
			{
				"value": ai.Anthropic, "label": "Anthropic",
				"baseHint":  "비우면 api.anthropic.com",
				"keyHint":   "x-api-key 로 전송되는 API 키",
				"modelHint": "예: claude-sonnet-4-5",
			},
			{
				"value": ai.OpenAICompatible, "label": "OpenAI 호환",
				"baseHint":  "OpenAI 본체는 비우고, 로컬 LLM은 http://localhost:11434/v1 처럼 지정",
				"keyHint":   "Authorization: Bearer 로 전송되는 키 (로컬 LLM은 임의 값)",
				"modelHint": "예: gpt-4o-mini, llama3.1",
			},
			{
				"value": ai.Ollama, "label": "Ollama",
				"baseHint": "로컬은 " + ai.OllamaLocalBaseURL + ", 클라우드는 " + ai.OllamaCloudBaseURL,
				// 로컬 Ollama는 키를 쓰지 않지만, 프로바이더 저장은 키를 요구한다.
				// 없는 것을 있는 것처럼 적어 두면 저장 단계에서 막히고 이유를 알 수 없다.
				"keyHint": "Ollama Cloud의 API 키. 로컬은 아무 값이나 넣으면 됩니다(쓰이지 않습니다)",
				// 컨텍스트 크기를 정할 수 있는 것이 OpenAI 호환과의 차이다.
				"modelHint": "예: glm-5.3, gpt-oss:120b, qwen3.5:397b",
			},
		},
		// 주소를 외우게 하지 않는다. 특히 Google의 호환 엔드포인트는 경로가
		// /v1beta/openai 라서 손으로 적으면 틀리기 쉽다(그리고 틀리면 404만 보인다).
		// 모델 이름은 넣지 않는다 — 자주 바뀌므로 키를 넣고 목록을 불러오는 것이 맞다.
		"presets": []fiber.Map{
			{"label": "OpenAI", "provider": ai.OpenAICompatible, "baseUrl": ""},
			{"label": "Google Gemini", "provider": ai.OpenAICompatible, "baseUrl": ai.GoogleCompatBaseURL},
			{"label": "Ollama Cloud", "provider": ai.Ollama, "baseUrl": ai.OllamaCloudBaseURL},
			{"label": "Ollama (로컬)", "provider": ai.Ollama, "baseUrl": ai.OllamaLocalBaseURL},
			{"label": "Anthropic", "provider": ai.Anthropic, "baseUrl": ""},
		},
	})
}

// maxAllowedModels는 허용 목록의 크기 상한이다.
// 프로바이더가 돌려주는 목록은 수백 개가 되기도 하는데, 그것을 그대로 저장하면
// "고른 것"이 아니라 "전부"가 되어 목록의 의미가 사라진다.
const maxAllowedModels = 50

// normalizeModels는 허용 모델 목록을 정리한다 (공백 제거, 빈 값·중복 제거).
//
// 순서를 유지하는 이유: 화면에서 고른 순서가 곧 목록의 순서이고, 첫 항목이 기본
// 모델의 후보가 된다. 정렬해 버리면 사용자가 의도한 우선순위가 사라진다.
func normalizeModels(in []string) ([]string, error) {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, m := range in {
		m = strings.TrimSpace(m)
		if m == "" || seen[m] {
			continue
		}
		if len([]rune(m)) > 120 {
			return nil, errors.New("모델 이름이 너무 깁니다 (120자 제한)")
		}
		seen[m] = true
		out = append(out, m)
	}
	if len(out) > maxAllowedModels {
		return nil, fmt.Errorf("허용 모델은 %d개까지 지정할 수 있습니다", maxAllowedModels)
	}
	return out, nil
}

// modelAllowed는 이 프로바이더로 그 모델을 쓸 수 있는지 본다.
//
// 목록이 비어 있으면 제한이 없다. 이 규칙이 이 기능의 전부이며, 저장·세션 선택·
// 실제 호출 세 곳이 모두 이 함수를 지난다 — 판정이 여러 벌이면 화면에서 고를 수
// 없는 모델이 API로는 통하는 식으로 어긋난다.
func modelAllowed(p *store.AIProvider, name string) bool {
	if p == nil || len(p.Models) == 0 {
		return true
	}
	return slices.Contains(p.Models, strings.TrimSpace(name))
}

type aiProviderRequest struct {
	Name         string `json:"name"`
	Provider     string `json:"provider"`
	BaseURL      string `json:"baseUrl"`
	DefaultModel string `json:"defaultModel"`
	// ContextTokens는 이 프로바이더가 한 번에 받는 토큰 수다. 0이면 모른다.
	ContextTokens int `json:"contextTokens"`
	// Models가 비어 있으면 모델 제한이 없다.
	Models    []string `json:"models"`
	APIKey    *string  `json:"apiKey"`
	Enabled   *bool    `json:"enabled"`
	IsDefault *bool    `json:"isDefault"`
}

func (r *aiProviderRequest) validate() error {
	r.Name = strings.TrimSpace(r.Name)
	if r.Name == "" {
		return errors.New("이름을 입력하세요")
	}
	if len([]rune(r.Name)) > 80 {
		return errors.New("이름이 너무 깁니다 (80자 제한)")
	}
	if !ai.Kind(r.Provider).Valid() {
		return errors.New("종류는 anthropic, openai, ollama 중 하나여야 합니다")
	}
	// 컨텍스트 크기는 사람이 손으로 적는 값이라 오타가 난다. 음수와 터무니없이 큰
	// 값을 여기서 막는다 — 틀린 값으로 예산을 계산하면 이력이 통째로 사라지거나
	// 아무것도 걸러지지 않고, 둘 다 조용히 일어난다.
	if r.ContextTokens < 0 || r.ContextTokens > 10_000_000 {
		return errors.New("컨텍스트 크기가 올바르지 않습니다 (0은 '모름', 최대 10,000,000)")
	}
	r.BaseURL = strings.TrimSpace(r.BaseURL)
	if err := ai.ValidateBaseURL(r.BaseURL); err != nil {
		return err
	}
	r.DefaultModel = strings.TrimSpace(r.DefaultModel)

	models, err := normalizeModels(r.Models)
	if err != nil {
		return err
	}
	r.Models = models
	if len(models) > 0 {
		// 기본 모델은 반드시 목록 안에 있어야 한다. 밖에 있으면 아무도 고르지 않은
		// 모델이 모든 새 대화에서 쓰이게 되고, 그것은 목록을 정한 의미를 없앤다.
		if r.DefaultModel == "" {
			r.DefaultModel = models[0]
		} else if !slices.Contains(models, r.DefaultModel) {
			return fmt.Errorf("기본 모델 %q 가 허용 목록에 없습니다", r.DefaultModel)
		}
	}
	return nil
}

func (s *Server) handleCreateAIProvider(c *fiber.Ctx) error {
	var req aiProviderRequest
	if err := c.BodyParser(&req); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "요청 본문을 해석할 수 없습니다")
	}
	if err := req.validate(); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", err.Error())
	}
	if req.APIKey == nil || strings.TrimSpace(*req.APIKey) == "" {
		return fail(c, fiber.StatusBadRequest, "bad_request", "API 키를 입력하세요")
	}
	u := currentUser(c)
	item, err := s.st.CreateAIProvider(c.Context(), store.SaveAIProviderParams{
		Name: req.Name, Provider: req.Provider, BaseURL: req.BaseURL,
		DefaultModel: req.DefaultModel, ContextTokens: req.ContextTokens,
		Models: req.Models, APIKey: req.APIKey,
		Enabled: boolOr(req.Enabled, true), IsDefault: boolOr(req.IsDefault, false),
		CreatedBy: u.ID,
	})
	if errors.Is(err, store.ErrDuplicateName) {
		return fail(c, fiber.StatusConflict, "duplicate", "이미 같은 이름의 프로바이더가 있습니다")
	}
	if err != nil {
		return err
	}
	s.audit(c, store.AuditParams{
		Action: "ai.provider.create", TargetType: "ai_provider", TargetID: item.ID,
		Detail: map[string]any{
			"name": item.Name, "provider": item.Provider,
			"baseUrl": item.BaseURL, "model": item.DefaultModel,
		},
	})
	return c.Status(fiber.StatusCreated).JSON(fiber.Map{"provider": item})
}

func (s *Server) handleUpdateAIProvider(c *fiber.Ctx) error {
	id := c.Params("id")
	if _, err := s.st.GetAIProvider(c.Context(), id, false); errors.Is(err, store.ErrNotFound) {
		return fiber.NewError(fiber.StatusNotFound, "프로바이더를 찾을 수 없습니다")
	} else if err != nil {
		return err
	}
	var req aiProviderRequest
	if err := c.BodyParser(&req); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "요청 본문을 해석할 수 없습니다")
	}
	if err := req.validate(); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", err.Error())
	}
	// 키를 보내지 않았으면 기존 것을 유지한다.
	if req.APIKey != nil && strings.TrimSpace(*req.APIKey) == "" {
		req.APIKey = nil
	}
	item, err := s.st.UpdateAIProvider(c.Context(), store.SaveAIProviderParams{
		ID: id, Name: req.Name, Provider: req.Provider, BaseURL: req.BaseURL,
		DefaultModel: req.DefaultModel, ContextTokens: req.ContextTokens,
		Models: req.Models, APIKey: req.APIKey,
		Enabled: boolOr(req.Enabled, true), IsDefault: boolOr(req.IsDefault, false),
	})
	if errors.Is(err, store.ErrDuplicateName) {
		return fail(c, fiber.StatusConflict, "duplicate", "이미 같은 이름의 프로바이더가 있습니다")
	}
	if errors.Is(err, store.ErrNotFound) {
		return fiber.NewError(fiber.StatusNotFound, "프로바이더를 찾을 수 없습니다")
	}
	if err != nil {
		return err
	}
	s.audit(c, store.AuditParams{
		Action: "ai.provider.update", TargetType: "ai_provider", TargetID: id,
		Detail: map[string]any{
			"name": item.Name, "keyChanged": req.APIKey != nil,
			"model": item.DefaultModel, "models": item.Models,
		},
	})
	return c.JSON(fiber.Map{"provider": item})
}

func (s *Server) handleDeleteAIProvider(c *fiber.Ctx) error {
	id := c.Params("id")
	item, err := s.st.GetAIProvider(c.Context(), id, false)
	if errors.Is(err, store.ErrNotFound) {
		return fiber.NewError(fiber.StatusNotFound, "프로바이더를 찾을 수 없습니다")
	}
	if err != nil {
		return err
	}
	if err := s.st.DeleteAIProvider(c.Context(), id); err != nil {
		return err
	}
	s.audit(c, store.AuditParams{
		Action: "ai.provider.delete", TargetType: "ai_provider", TargetID: id,
		Detail: map[string]any{"name": item.Name},
	})
	return c.JSON(fiber.Map{"deleted": true})
}

// handleDiscoverAIModels는 저장 전에 모델 목록을 조회한다.
//
// 저장된 프로바이더에만 목록 조회를 붙일 수 없는 이유: 허용 모델을 고르는 일은
// **키를 등록하는 화면 안에서** 일어난다. 먼저 저장하고 다시 열어 고르게 하면
// 그 사이에 기본 모델이 비어 있는 프로바이더가 존재하게 되고, 사용자는 두 단계를
// 오간다. 그래서 아직 저장되지 않은 입력값으로도 조회할 수 있어야 한다.
//
// 기존 프로바이더를 수정하는 중이라면 id만 보내면 된다 — 키는 다시 입력받지 않는다
// (원문을 화면에 돌려주지 않으므로 다시 받을 수도 없다).
func (s *Server) handleDiscoverAIModels(c *fiber.Ctx) error {
	var body struct {
		ID       string `json:"id"`
		Provider string `json:"provider"`
		BaseURL  string `json:"baseUrl"`
		APIKey   string `json:"apiKey"`
	}
	if err := c.BodyParser(&body); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "요청 본문을 해석할 수 없습니다")
	}
	body.Provider = strings.TrimSpace(body.Provider)
	body.BaseURL = strings.TrimSpace(body.BaseURL)
	body.APIKey = strings.TrimSpace(body.APIKey)

	if id := strings.TrimSpace(body.ID); id != "" {
		stored, err := s.st.GetAIProvider(c.Context(), id, true)
		if errors.Is(err, store.ErrNotFound) {
			return fiber.NewError(fiber.StatusNotFound, "프로바이더를 찾을 수 없습니다")
		}
		if err != nil {
			return err
		}
		// 화면에서 바꾼 값이 있으면 그것을 우선한다. 주소를 고치고 목록을 다시
		// 부르는 것이 이 버튼의 주된 쓰임이다.
		if body.Provider == "" {
			body.Provider = stored.Provider
		}
		if body.BaseURL == "" {
			body.BaseURL = stored.BaseURL
		}
		if body.APIKey == "" {
			body.APIKey = stored.APIKey
		}
	}

	if !ai.Kind(body.Provider).Valid() {
		return fail(c, fiber.StatusBadRequest, "bad_request",
			"종류는 anthropic, openai, ollama 중 하나여야 합니다")
	}
	if err := ai.ValidateBaseURL(body.BaseURL); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", err.Error())
	}
	// 로컬 Ollama는 키가 없다. 키를 요구하면 목록을 못 불러오고, 목록이 없으면
	// 모델 이름을 손으로 적어야 한다 — 이름을 틀리면 그때 나오는 것은 404뿐이다.
	if body.APIKey == "" && ai.Kind(body.Provider) != ai.Ollama {
		return fail(c, fiber.StatusBadRequest, "bad_request", "API 키를 입력하세요")
	}

	provider, err := ai.Get(ai.Kind(body.Provider))
	if err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", err.Error())
	}
	ctx, cancel := context.WithTimeout(c.Context(), 30*time.Second)
	defer cancel()

	models, err := provider.Models(ctx, ai.Config{
		Kind: ai.Kind(body.Provider), BaseURL: body.BaseURL, APIKey: body.APIKey,
	})
	if errors.Is(err, ai.ErrNotSupported) {
		// 목록을 주지 않는 엔드포인트가 있다(로컬 LLM 일부). 그때는 실패가 아니라
		// "직접 입력하라"는 안내다 — 오류로 만들면 사용자는 키가 틀렸다고 생각한다.
		return c.JSON(fiber.Map{"models": []string{}, "supported": false})
	}
	if err != nil {
		return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{
			"error": "verify_failed", "message": "모델 목록을 가져오지 못했습니다",
			"detail": err.Error(),
		})
	}
	// Ollama는 모델마다 컨텍스트 크기를 알려준다. 화면이 그것으로 칸을 채운다 —
	// 사람이 모델 카드를 찾아 손으로 옮겨 적으면 틀리고, 틀린 값은 조용히 이력을
	// 지우거나 조용히 넘치게 한다. (클라우드는 주지 않으므로 빈 지도가 온다.)
	res := fiber.Map{"models": models, "supported": true}
	if ai.Kind(body.Provider) == ai.Ollama {
		if sizes, cerr := ai.OllamaModelContext(ctx, ai.Config{
			Kind: ai.Ollama, BaseURL: body.BaseURL, APIKey: body.APIKey,
		}); cerr == nil && len(sizes) > 0 {
			res["contextTokens"] = sizes
		}
	}
	return c.JSON(res)
}

// handleTestAIProvider는 키가 유효한지 확인하고 모델 목록을 가져온다.
func (s *Server) handleTestAIProvider(c *fiber.Ctx) error {
	id := c.Params("id")
	item, err := s.st.GetAIProvider(c.Context(), id, true)
	if errors.Is(err, store.ErrNotFound) {
		return fiber.NewError(fiber.StatusNotFound, "프로바이더를 찾을 수 없습니다")
	}
	if err != nil {
		return err
	}
	provider, err := ai.Get(ai.Kind(item.Provider))
	if err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", err.Error())
	}
	ctx, cancel := context.WithTimeout(c.Context(), 30*time.Second)
	defer cancel()

	models, merr := provider.Models(ctx, ai.Config{
		Kind: ai.Kind(item.Provider), BaseURL: item.BaseURL,
		APIKey: item.APIKey, Model: item.DefaultModel,
	})
	if merr != nil {
		_ = s.st.RecordAIProviderCheck(c.Context(), id, false, merr.Error())
		s.audit(c, store.AuditParams{
			Action: "ai.provider.test", TargetType: "ai_provider", TargetID: id,
			Result: "error", Detail: map[string]any{"name": item.Name, "error": merr.Error()},
		})
		return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{
			"ok": false, "error": "verify_failed",
			"message": "프로바이더에 접근할 수 없습니다", "detail": merr.Error(),
		})
	}
	msg := fmt.Sprintf("모델 %d개 확인", len(models))
	_ = s.st.RecordAIProviderCheck(c.Context(), id, true, msg)
	s.audit(c, store.AuditParams{
		Action: "ai.provider.test", TargetType: "ai_provider", TargetID: id,
		Detail: map[string]any{"name": item.Name, "models": len(models)},
	})

	warnings := []string{}
	if item.DefaultModel != "" && len(models) > 0 && !slices.Contains(models, item.DefaultModel) {
		warnings = append(warnings, fmt.Sprintf(
			"기본 모델 %q 이(가) 목록에 없습니다. 이름을 확인하세요", item.DefaultModel))
	}
	// 허용 목록에 있는데 프로바이더가 모르는 모델은 고를 수는 있지만 부르면 실패한다.
	// 그 실패는 대화 도중에 나타나므로 여기서 미리 알린다.
	if len(models) > 0 {
		missing := []string{}
		for _, m := range item.Models {
			if !slices.Contains(models, m) {
				missing = append(missing, m)
			}
		}
		if len(missing) > 0 {
			warnings = append(warnings, fmt.Sprintf(
				"허용 목록의 모델 중 프로바이더에 없는 것: %s", strings.Join(missing, ", ")))
		}
	}
	if len(models) > 60 {
		models = models[:60]
	}
	return c.JSON(fiber.Map{"ok": true, "models": models, "warnings": warnings})
}

func boolOr(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

// ---------- 세션 ----------

func (s *Server) handleListAISessions(c *fiber.Ctx) error {
	u := currentUser(c)
	sessions, err := s.st.ListAISessions(c.Context(), u.ID,
		c.Query("archived") == "1", c.QueryInt("limit", 50))
	if err != nil {
		return err
	}
	providers, err := s.st.ListAIProviders(c.Context())
	if err != nil {
		return err
	}
	enabled := []*store.AIProvider{}
	for _, p := range providers {
		if p.Enabled && p.HasKey {
			enabled = append(enabled, p)
		}
	}
	tools, _ := availableTools(u, s.toolHints(c, u))
	toolInfo := make([]fiber.Map, 0, len(tools))
	registry := aiTools()
	for _, t := range tools {
		toolInfo = append(toolInfo, fiber.Map{
			"name": t.Name, "description": t.Description,
			"mutating": registry[t.Name].Mutating,
		})
	}
	return c.JSON(fiber.Map{
		"sessions": sessions, "providers": enabled, "tools": toolInfo,
	})
}

func (s *Server) handleCreateAISession(c *fiber.Ctx) error {
	var body struct {
		Title        string `json:"title"`
		ProviderID   string `json:"providerId"`
		Model        string `json:"model"`
		ConnectionID string `json:"connectionId"`
	}
	_ = c.BodyParser(&body)

	u := currentUser(c)
	// 대상 커넥션을 지정했으면 접근 권한을 확인한다. 권한 없는 커넥션을 세션에
	// 붙여두면 이후 툴 호출마다 실패해 사용자가 이유를 알기 어렵다.
	if strings.TrimSpace(body.ConnectionID) != "" {
		d, err := s.requireLevel(c, body.ConnectionID, model.LevelMonitor)
		if err != nil {
			return err
		}
		if !d.Allowed {
			return fiber.NewError(fiber.StatusForbidden, d.Reason)
		}
	}
	if err := s.checkSessionModel(c.Context(),
		strings.TrimSpace(body.ProviderID), strings.TrimSpace(body.Model)); err != nil {
		return fail(c, fiber.StatusBadRequest, "model_not_allowed", err.Error())
	}
	title := strings.TrimSpace(body.Title)
	if title == "" {
		title = "새 대화"
	}
	sess, err := s.st.CreateAISession(c.Context(), store.CreateAISessionParams{
		UserID: u.ID, Title: title,
		ProviderID:   strings.TrimSpace(body.ProviderID),
		Model:        strings.TrimSpace(body.Model),
		ConnectionID: strings.TrimSpace(body.ConnectionID),
	})
	if err != nil {
		return err
	}
	s.audit(c, store.AuditParams{
		Action: "ai.session.create", TargetType: "ai_session", TargetID: sess.ID,
		Detail: map[string]any{"title": title},
	})
	return c.Status(fiber.StatusCreated).JSON(fiber.Map{"session": sess})
}

// checkSessionModel은 세션이 고른 모델이 그 프로바이더에서 허용된 것인지 확인한다.
//
// 모델을 비워 두는 것은 정상이다 — 그러면 프로바이더의 기본 모델을 쓴다.
func (s *Server) checkSessionModel(ctx context.Context, providerID, modelName string) error {
	if modelName == "" {
		return nil
	}
	var p *store.AIProvider
	var err error
	if providerID != "" {
		p, err = s.st.GetAIProvider(ctx, providerID, false)
		if errors.Is(err, store.ErrNotFound) {
			return errors.New("프로바이더를 찾을 수 없습니다")
		}
	} else {
		p, err = s.st.DefaultAIProvider(ctx, false)
	}
	if err != nil {
		return err
	}
	if !modelAllowed(p, modelName) {
		return fmt.Errorf("%s 에서는 모델 %q 를 쓸 수 없습니다. 허용된 모델: %s",
			p.Name, modelName, strings.Join(p.Models, ", "))
	}
	return nil
}

// resolveSession은 세션을 읽고 소유자인지 확인한다.
//
// 소유자만 접근할 수 있게 하는 이유: 대화에는 그 사람의 권한으로 조회한 데이터가
// 들어 있다. 슈퍼 어드민도 남의 대화를 읽을 수 없다 — 그렇게 하려면 감사 로그를
// 보는 것이 맞고, 대화 내용은 조회 결과를 포함하므로 권한 우회 통로가 된다.
func (s *Server) resolveAISession(c *fiber.Ctx, id string) (*store.AISession, error) {
	sess, err := s.st.GetAISession(c.Context(), strings.TrimSpace(id))
	if errors.Is(err, store.ErrNotFound) {
		return nil, fiber.NewError(fiber.StatusNotFound, "세션을 찾을 수 없습니다")
	}
	if err != nil {
		return nil, err
	}
	u := currentUser(c)
	if sess.UserID != u.ID {
		// 존재 여부까지 숨긴다. 404가 "남의 세션이 있다"는 정보를 주지 않는다.
		return nil, fiber.NewError(fiber.StatusNotFound, "세션을 찾을 수 없습니다")
	}
	return sess, nil
}

func (s *Server) handleGetAISession(c *fiber.Ctx) error {
	sess, err := s.resolveAISession(c, c.Params("id"))
	if err != nil {
		return err
	}
	messages, err := s.st.ListAIMessages(c.Context(), sess.ID, 0)
	if err != nil {
		return err
	}
	pending, err := s.st.ListAIPendingActions(c.Context(), sess.ID)
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{
		"session": sess, "messages": messages, "pendingActions": pending,
	})
}

func (s *Server) handleUpdateAISession(c *fiber.Ctx) error {
	sess, err := s.resolveAISession(c, c.Params("id"))
	if err != nil {
		return err
	}
	var body struct {
		Title        *string `json:"title"`
		ProviderID   *string `json:"providerId"`
		Model        *string `json:"model"`
		ConnectionID *string `json:"connectionId"`
		Archived     *bool   `json:"archived"`
	}
	if err := c.BodyParser(&body); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "요청 본문을 해석할 수 없습니다")
	}
	title, providerID, modelName := sess.Title, sess.ProviderID, sess.Model
	connID, archived := sess.ConnectionID, sess.Archived
	if body.Title != nil {
		title = strings.TrimSpace(*body.Title)
		if title == "" {
			return fail(c, fiber.StatusBadRequest, "bad_request", "제목을 입력하세요")
		}
	}
	if body.ProviderID != nil {
		providerID = strings.TrimSpace(*body.ProviderID)
	}
	if body.Model != nil {
		modelName = strings.TrimSpace(*body.Model)
	}
	if body.ConnectionID != nil {
		connID = strings.TrimSpace(*body.ConnectionID)
		if connID != "" {
			d, err := s.requireLevel(c, connID, model.LevelMonitor)
			if err != nil {
				return err
			}
			if !d.Allowed {
				return fiber.NewError(fiber.StatusForbidden, d.Reason)
			}
		}
	}
	if body.Archived != nil {
		archived = *body.Archived
	}
	// 프로바이더와 모델은 함께 바뀔 수 있으므로 최종 조합으로 한 번만 판정한다.
	if err := s.checkSessionModel(c.Context(), providerID, modelName); err != nil {
		return fail(c, fiber.StatusBadRequest, "model_not_allowed", err.Error())
	}
	if err := s.st.UpdateAISessionMeta(c.Context(), sess.ID,
		title, providerID, modelName, connID, archived); err != nil {
		return err
	}
	updated, err := s.st.GetAISession(c.Context(), sess.ID)
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"session": updated})
}

func (s *Server) handleDeleteAISession(c *fiber.Ctx) error {
	sess, err := s.resolveAISession(c, c.Params("id"))
	if err != nil {
		return err
	}
	if err := s.st.DeleteAISession(c.Context(), sess.ID); err != nil {
		return err
	}
	s.audit(c, store.AuditParams{
		Action: "ai.session.delete", TargetType: "ai_session", TargetID: sess.ID,
		Detail: map[string]any{"title": sess.Title},
	})
	return c.JSON(fiber.Map{"deleted": true})
}

// ---------- 대화 (SSE 스트리밍) ----------

// handleAIChat는 사용자 메시지를 받아 응답을 SSE로 스트리밍한다.
//
// POST + SSE 조합을 쓰는 이유: EventSource는 GET만 지원하므로 메시지를 URL에 넣어야
// 하는데, 질문이 길면 URL 길이 제한에 걸리고 서버 로그에 대화 내용이 남는다.
// fetch + ReadableStream으로 읽으면 POST 본문을 쓸 수 있다.
func (s *Server) handleAIChat(c *fiber.Ctx) error {
	sess, err := s.resolveAISession(c, c.Params("id"))
	if err != nil {
		return err
	}
	var body struct {
		Message string `json:"message"`
		// ReplaceFrom이 있으면 그 메시지와 그 뒤를 지우고 그 자리에서 다시 시작한다.
		// 사람이 자기 말을 고쳐 다시 보내는 것이 그 뜻이다.
		ReplaceFrom int64 `json:"replaceFrom"`
	}
	if err := c.BodyParser(&body); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "요청 본문을 해석할 수 없습니다")
	}
	text := strings.TrimSpace(body.Message)
	if text == "" {
		return fail(c, fiber.StatusBadRequest, "bad_request", "메시지를 입력하세요")
	}
	if len([]rune(text)) > 20_000 {
		return fail(c, fiber.StatusBadRequest, "bad_request", "메시지가 너무 깁니다 (20000자 제한)")
	}

	cfg, provider, err := s.providerConfig(c.Context(), sess)
	if err != nil {
		return failDetail(c, fiber.StatusBadRequest, "no_provider",
			"AI 프로바이더를 사용할 수 없습니다", err.Error())
	}

	// 고친 말이면 그 자리부터 지운다.
	//
	// 새 말을 뒤에 붙이지 않는 이유: 고치기는 "다시 묻는 것"이 아니라 "그때 다르게
	// 물었다면"이다. 옛 문답이 뒤에 남으면 대화는 있지도 않았던 흐름이 되고, 다음
	// 요청의 문맥으로 그 옛 답이 그대로 모델에게 간다.
	//
	// 지우기 전에 그 메시지가 **이 대화의 사용자 말인지** 확인한다. 아이디는 앱
	// 전체에서 증가하는 숫자라 남의 대화를 가리키기 쉽고, 모델의 답을 가리키면
	// "내 말을 고친다"가 아니라 남의 답을 지우는 일이 된다.
	if body.ReplaceFrom > 0 {
		target, terr := s.st.GetAIMessage(c.Context(), sess.ID, body.ReplaceFrom)
		if errors.Is(terr, store.ErrNotFound) {
			return fail(c, fiber.StatusNotFound, "not_found", "고칠 메시지를 찾을 수 없습니다")
		}
		if terr != nil {
			return terr
		}
		if target.Role != string(ai.RoleUser) {
			return fail(c, fiber.StatusBadRequest, "not_user_message",
				"고칠 수 있는 것은 내가 보낸 말입니다")
		}
		removed, left, derr := s.st.TruncateAIMessagesFrom(c.Context(), sess.ID, body.ReplaceFrom)
		if derr != nil {
			return derr
		}
		// 첫 말을 고쳤으면 제목도 그 말에서 뽑은 것이다. 비워 두면 아래에서 새 말로
		// 다시 정해진다 — 그러지 않으면 목록의 제목과 대화의 첫 줄이 어긋난다.
		if left == 0 {
			sess.Title = ""
		}
		s.audit(c, store.AuditParams{
			Action: "ai.message.replaced", TargetType: "ai_session", TargetID: sess.ID,
			Detail: map[string]any{"fromId": body.ReplaceFrom, "removed": removed},
		})
	}

	// 새 사용자 메시지가 오면 이전 제안은 문맥을 잃는다. 그대로 두면 한참 뒤에
	// 승인 버튼을 눌러 예상하지 못한 시점에 실행될 수 있다.
	if n, err := s.st.ExpireAIPendingActions(c.Context(), sess.ID); err == nil && n > 0 {
		s.audit(c, store.AuditParams{
			Action: "ai.pending.expired", TargetType: "ai_session", TargetID: sess.ID,
			Detail: map[string]any{"count": n},
		})
	}

	if err := s.st.AddAIMessage(c.Context(), &store.AIMessage{
		SessionID: sess.ID, Role: string(ai.RoleUser), Text: text,
	}); err != nil {
		return err
	}
	// 첫 사용자 메시지로 제목을 정한다. "새 대화"가 목록에 여러 개 쌓이면 구분할 수 없다.
	if sess.Title == "" || sess.Title == "새 대화" {
		title := text
		if len([]rune(title)) > 40 {
			title = string([]rune(title)[:40]) + "…"
		}
		_ = s.st.UpdateAISessionMeta(c.Context(), sess.ID, title,
			sess.ProviderID, sess.Model, sess.ConnectionID, sess.Archived)
		sess.Title = title
	}

	stored, err := s.st.ListAIMessages(c.Context(), sess.ID, 0)
	if err != nil {
		return err
	}
	// 이력을 얼마나 남길지는 프로바이더의 컨텍스트 크기가 정한다.
	//
	// 하나의 상수로 두면 어느 쪽으로든 틀린다: Claude(20만 토큰)에서는 멀쩡한
	// 이력을 이유 없이 버리고, 로컬 Ollama(기본 4~8천 토큰)에서는 넘치는 줄도
	// 모르고 보내 Ollama가 말없이 앞을 잘라낸다.
	history := buildHistory(stored, historyBudget(cfg.ContextTokens))

	// 대상 DB를 시스템 프롬프트에 담는다. 이름을 읽을 수 없으면(지워졌거나 권한이
	// 사라졌으면) 그냥 붙이지 않는다 — 여기서 실패시키면 대화 전체가 막힌다.
	var target *model.Connection
	if sess.ConnectionID != "" {
		if conn, cerr := s.st.GetConnection(c.Context(), sess.ConnectionID); cerr == nil {
			target = conn
		}
	}

	u := currentUser(c)
	ip := clientIP(c)

	// ERD 초안에 매인 대화는 툴 상자가 다르다.
	//
	// 앱 전체의 툴(38종)을 주면 모델이 "어느 DB를 볼까"부터 정하려 들고, 이 대화의
	// 맥락(이 초안 하나)을 벗어난 일을 하게 된다. 여기서는 문서를 고치는 툴만 준다.
	var tools []ai.Tool
	var registry map[string]*aiTool
	var erdTools map[string]*erdTool
	var erdDocID, erdConnID, erdDocName, erdDialect string
	if sess.ERDDocumentID != "" {
		meta, merr := s.st.GetERDDocumentMeta(c.Context(), sess.ERDDocumentID)
		if merr != nil {
			return failDetail(c, fiber.StatusBadRequest, "no_document",
				"이 대화의 ERD 초안을 찾을 수 없습니다", merr.Error())
		}
		erdDocID, erdConnID = sess.ERDDocumentID, meta.ConnectionID
		erdDocName, erdDialect = meta.Name, meta.Dialect
		tools, erdTools = erdAITools()
	} else {
		tools, registry = availableTools(u, s.toolHints(c, u))
	}

	s.audit(c, store.AuditParams{
		Action: "ai.chat", TargetType: "ai_session", TargetID: sess.ID,
		Detail: map[string]any{
			"provider": provider.Name, "model": cfg.Model,
			"messageChars": len(text), "tools": len(tools),
		},
	})

	c.Set(fiber.HeaderContentType, "text/event-stream")
	c.Set("Cache-Control", "no-cache")
	c.Set("Connection", "keep-alive")
	// 프록시가 SSE를 버퍼링하면 스트리밍이 아니게 된다. nginx는 이 헤더를 인식한다.
	c.Set("X-Accel-Buffering", "no")

	// 여기서부터는 *fiber.Ctx를 만지면 안 된다: 스트림 라이터는 핸들러가 반환한 뒤
	// 실행되고, 그때 요청 컨텍스트는 이미 해제되어 있다. 필요한 값은 위에서 복사했다.
	srv := s
	c.Context().SetBodyStreamWriter(fasthttp.StreamWriter(func(w *bufio.Writer) {
		// 이 함수는 핸들러가 반환한 뒤 실행되므로 Fiber의 recover 미들웨어가 감싸지 않는다.
		// 여기서 패닉이 나면 프로세스 전체가 죽는다.
		defer applog.Recover("ai.chat.stream")

		// 컨텍스트는 요청과 무관하게 새로 만든다. 클라이언트 이탈은 쓰기 실패로 알 수 있다.
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()

		out := &sseWriter{w: w}
		_ = out.send("start", map[string]any{
			"sessionId": sess.ID, "provider": provider.Name, "model": cfg.Model,
		})

		tc := srv.newToolContext(ctx, u, ip, sess)
		run := &agentRun{
			srv: srv, tc: tc, session: sess,
			cfg: cfg, system: sessionPrompt(target), tools: tools, registry: registry, out: out,
		}
		if erdDocID != "" {
			run.erd = &erdToolContext{tc: tc, docID: erdDocID, connID: erdConnID}
			run.erdTools = erdTools
			run.system = erdSystemPrompt(erdDocName, erdDialect)
		}
		run.run(ctx, history)
	}))
	return nil
}

// ---------- 제안 승인/거부 ----------

// handleDecidePendingAction은 쓰기 제안을 승인하거나 거부한다.
//
// 승인 시 실제 실행이 여기서 일어난다. AI는 이 경로를 호출할 수 없다 —
// 사용자의 명시적 요청만이 도달한다.
func (s *Server) handleDecidePendingAction(c *fiber.Ctx) error {
	sess, err := s.resolveAISession(c, c.Params("id"))
	if err != nil {
		return err
	}
	action, err := s.st.GetAIPendingAction(c.Context(), c.Params("actionId"))
	if errors.Is(err, store.ErrNotFound) {
		return fiber.NewError(fiber.StatusNotFound, "제안을 찾을 수 없습니다")
	}
	if err != nil {
		return err
	}
	if action.SessionID != sess.ID {
		return fiber.NewError(fiber.StatusNotFound, "이 세션의 제안이 아닙니다")
	}
	if action.Status != store.PendingStatusPending {
		return fail(c, fiber.StatusConflict, "already_decided",
			fmt.Sprintf("이미 처리된 제안입니다 (%s)", action.Status))
	}

	var body struct {
		Decision string `json:"decision"`
	}
	if err := c.BodyParser(&body); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "요청 본문을 해석할 수 없습니다")
	}
	decision := strings.TrimSpace(body.Decision)
	if decision != "approve" && decision != "reject" {
		return fail(c, fiber.StatusBadRequest, "bad_request",
			"decision은 approve 또는 reject여야 합니다")
	}

	u := currentUser(c)
	if decision == "reject" {
		if err := s.st.DecideAIPendingAction(c.Context(), action.ID,
			store.PendingStatusRejected, "사용자가 거부했습니다", "", u.ID); err != nil {
			if errors.Is(err, store.ErrAlreadyDecided) {
				return fail(c, fiber.StatusConflict, "already_decided", "이미 처리된 제안입니다")
			}
			return err
		}
		s.audit(c, store.AuditParams{
			Action: "ai.pending.reject", TargetType: "ai_session", TargetID: sess.ID,
			Detail: map[string]any{"tool": action.ToolName, "actionId": action.ID},
		})
		// 모델에게 결과를 알려 대화가 이어지게 한다.
		if err := s.appendToolResult(c.Context(), sess.ID, action.ToolCallID,
			"사용자가 이 작업을 거부했습니다. 실행되지 않았습니다.", false); err != nil {
			return err
		}
		return c.JSON(fiber.Map{"status": store.PendingStatusRejected})
	}

	// 승인: 툴의 Apply를 실행한다.
	registry := aiTools()
	tool := registry[action.ToolName]
	if tool == nil || tool.Apply == nil {
		return fail(c, fiber.StatusBadRequest, "unknown_tool",
			fmt.Sprintf("%q 툴을 실행할 수 없습니다", action.ToolName))
	}
	if tool.SuperadminOnly && !u.Role.CanManageUsers() {
		return fiber.NewError(fiber.StatusForbidden, "이 작업은 슈퍼 어드민만 실행할 수 있습니다")
	}

	// 실행 전에 상태를 선점한다: 두 요청이 동시에 승인하면 툴이 두 번 실행된다.
	// DecideAIPendingAction은 pending 상태만 갱신하므로 한쪽만 통과한다.
	if err := s.st.DecideAIPendingAction(c.Context(), action.ID,
		store.PendingStatusApproved, "", "", u.ID); err != nil {
		if errors.Is(err, store.ErrAlreadyDecided) {
			return fail(c, fiber.StatusConflict, "already_decided", "이미 처리된 제안입니다")
		}
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	tc := s.newToolContext(ctx, u, clientIP(c), sess)

	result, runErr := tool.Apply(tc, action.Arguments)
	if runErr != nil {
		// 실패를 기록한다. 상태는 이미 approved이므로 별도 컬럼에 오류를 남긴다.
		_, _ = s.st.DB().ExecContext(c.Context(),
			`UPDATE ai_pending_actions SET status = ?, error = ? WHERE id = ?`,
			store.PendingStatusFailed, runErr.Error(), action.ID)
		s.audit(c, store.AuditParams{
			Action: "ai.pending.approve", TargetType: "ai_session", TargetID: sess.ID,
			Result: "error",
			Detail: map[string]any{
				"tool": action.ToolName, "actionId": action.ID, "error": runErr.Error(),
			},
		})
		if err := s.appendToolResult(c.Context(), sess.ID, action.ToolCallID,
			"사용자가 승인했지만 실행이 실패했습니다: "+runErr.Error(), true); err != nil {
			return err
		}
		return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{
			"error": "apply_failed", "message": "승인한 작업을 실행하지 못했습니다",
			"detail": runErr.Error(), "status": store.PendingStatusFailed,
		})
	}

	_, _ = s.st.DB().ExecContext(c.Context(),
		`UPDATE ai_pending_actions SET result = ? WHERE id = ?`,
		truncateForUI(result, 8000), action.ID)
	s.audit(c, store.AuditParams{
		Action: "ai.pending.approve", TargetType: "ai_session", TargetID: sess.ID,
		Detail: map[string]any{"tool": action.ToolName, "actionId": action.ID},
	})
	if err := s.appendToolResult(c.Context(), sess.ID, action.ToolCallID,
		"사용자가 승인했고 실행이 완료되었습니다. 결과: "+result, false); err != nil {
		return err
	}
	return c.JSON(fiber.Map{
		"status": store.PendingStatusApproved, "result": json.RawMessage(result),
	})
}

// appendToolResult는 승인/거부 결과를 대화에 툴 결과로 남긴다.
//
// 이것이 없으면 모델은 자기가 요청한 툴의 결과를 영원히 받지 못하고, 다음 요청에서
// 프로바이더가 "tool_use에 대응하는 tool_result가 없다"며 거부한다.
func (s *Server) appendToolResult(ctx context.Context, sessionID, callID, content string, isError bool) error {
	return s.st.AddAIMessage(ctx, &store.AIMessage{
		SessionID: sessionID, Role: string(ai.RoleTool),
		ToolResults: []ai.ToolResult{{CallID: callID, Content: content, IsError: isError}},
	})
}
