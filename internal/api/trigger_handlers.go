package api

import (
	"errors"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"

	"dbstudio/internal/macro"
	"dbstudio/internal/model"
	"dbstudio/internal/store"
)

// 매크로 자동 실행 트리거 API.
//
// 접근 규칙은 대상 매크로를 따른다. 보는 것은 매크로를 볼 수 있으면 되고,
// **만들고 고치고 지우는 것은 관리 권한(만든 사람·협업자·슈퍼어드민)이 필요하다.**
// 수정 권한으로 열지 않은 이유: 자동 실행은 아무도 보고 있지 않은 시각에 소유자의
// 권한으로 도는 것이고, 공개+수정으로 열어 둔 매크로에서 아무나 그것을 걸 수 있으면
// 공개의 의미가 "내 권한을 아무 때나 빌려 줌"이 된다.
//
// **소유자는 만든 사람으로 고정된다.** 트리거는 소유자의 권한으로 실행되므로,
// 소유자를 바꿀 수 있으면 남의 권한으로 도는 자동 실행을 만들 수 있게 된다.
// 다른 사람의 권한으로 돌려야 한다면 그 사람이 직접 만들어야 한다.

const (
	maxTriggerNameLen = 60
	// minTriggerInterval은 이벤트 트리거의 최소 간격 하한이다.
	// 0을 허용하면 이벤트가 몰릴 때 매크로가 그만큼 동시에 시작된다.
	minTriggerInterval = 10
)

type triggerRequest struct {
	Name    string         `json:"name"`
	Kind    string         `json:"kind"`
	Enabled *bool          `json:"enabled"`
	Params  map[string]any `json:"params"`
	// ParamExprs는 실행 시점에 계산할 파라미터다(키 → Lua 식).
	ParamExprs map[string]string `json:"paramExprs"`

	Cron     string `json:"cron"`
	Timezone string `json:"timezone"`

	EventKind      string `json:"eventKind"`
	EventSeverity  string `json:"eventSeverity"`
	EventMetric    string `json:"eventMetric"`
	ConnectionID   string `json:"connectionId"`
	MinIntervalSec int    `json:"minIntervalSec"`

	SkipIfRunning *bool `json:"skipIfRunning"`
}

// triggerParams는 요청을 검증해 저장 파라미터로 바꾼다.
// 다음 실행 시각도 여기서 계산한다 — 저장하는 순간 "언제 도는지"가 정해져야 한다.
func triggerParams(req triggerRequest, macroID string) (store.SaveTriggerParams, string, string) {
	p := store.SaveTriggerParams{
		MacroID:        macroID,
		Name:           strings.TrimSpace(req.Name),
		Kind:           req.Kind,
		Enabled:        req.Enabled == nil || *req.Enabled,
		Params:         req.Params,
		ParamExprs:     req.ParamExprs,
		SkipIfRunning:  req.SkipIfRunning == nil || *req.SkipIfRunning,
		MinIntervalSec: req.MinIntervalSec,
	}
	if p.Name == "" {
		return p, "invalid_name", "트리거 이름을 입력하세요"
	}
	if len([]rune(p.Name)) > maxTriggerNameLen {
		return p, "invalid_name", "트리거 이름은 60자 이내로 입력하세요"
	}
	if !store.ValidTriggerKind(p.Kind) {
		return p, "invalid_kind", "트리거 종류는 schedule 또는 event 여야 합니다"
	}

	switch p.Kind {
	case store.TriggerSchedule:
		p.Cron = strings.TrimSpace(req.Cron)
		p.Timezone = strings.TrimSpace(req.Timezone)

		schedule, err := macro.ParseSchedule(p.Cron)
		if err != nil {
			return p, "invalid_cron", err.Error()
		}
		loc, err := macro.LoadLocation(p.Timezone)
		if err != nil {
			return p, "invalid_timezone", err.Error()
		}
		next, ok := schedule.Next(time.Now().In(loc))
		if !ok {
			return p, "invalid_cron", "이 cron 식은 실행될 시각이 없습니다"
		}
		utc := next.UTC()
		p.NextRunAt = &utc

	case store.TriggerEvent:
		p.EventKind = strings.TrimSpace(req.EventKind)
		p.EventSeverity = strings.TrimSpace(req.EventSeverity)
		p.EventMetric = strings.TrimSpace(req.EventMetric)
		p.ConnectionID = strings.TrimSpace(req.ConnectionID)

		switch p.EventKind {
		case "", store.EventThreshold, store.EventConnectivity, store.EventDrift, store.EventCollectError:
		default:
			return p, "invalid_event_kind", "알 수 없는 이벤트 종류입니다"
		}
		if p.EventSeverity != "" && !store.Severity(p.EventSeverity).Valid() {
			return p, "invalid_severity", "알 수 없는 심각도입니다"
		}
		if p.MinIntervalSec < minTriggerInterval {
			p.MinIntervalSec = minTriggerInterval
		}
	}
	return p, "", ""
}

// resolveTriggerMacro는 매크로를 읽고 트리거를 만질 수 있는지 확인한다.
func (s *Server) resolveTriggerMacro(c *fiber.Ctx, macroID string) (*store.Macro, error) {
	return s.requireMacro(c, macroID, model.MacroAccessManage)
}

func (s *Server) handleListTriggers(c *fiber.Ctx) error {
	// 목록은 볼 수 있는 매크로의 것만 나온다. macro= 로 좁힐 때도 마찬가지이므로
	// 존재하지 않는 것과 볼 수 없는 것이 똑같이 빈 목록으로 보인다.
	triggers, err := s.st.ListVisibleTriggers(c.Context(), c.Query("macro"), viewer(c))
	if err != nil {
		return err
	}
	// 트리거를 만질 수 있는지는 매크로마다 다르다. 화면이 항목마다 매크로를 다시
	// 물어보게 두면 목록 하나에 요청이 수십 번 나간다 — 한 번에 계산해 함께 보낸다.
	manageable := map[string]bool{}
	macros, err := s.st.ListMacros(c.Context(), viewer(c))
	if err != nil {
		return err
	}
	for _, m := range macros {
		manageable[m.ID] = m.CanManage
	}

	// cron 식을 사람이 읽는 문장으로 함께 내려보낸다.
	// "0 3 * * 1"이 무슨 뜻인지 아는 사람만 쓸 수 있게 두면 기능이 반만 있는 것이다.
	items := make([]fiber.Map, 0, len(triggers))
	for _, t := range triggers {
		item := fiber.Map{"trigger": t, "canManage": manageable[t.MacroID]}
		if t.Kind == store.TriggerSchedule {
			item["describe"] = macro.Describe(t.Cron)
		}
		items = append(items, item)
	}
	return c.JSON(fiber.Map{"items": items})
}

func (s *Server) handleCreateTrigger(c *fiber.Ctx) error {
	u := currentUser(c)
	m, err := s.resolveTriggerMacro(c, c.Params("id"))
	if err != nil {
		return err
	}

	var req triggerRequest
	if err := c.BodyParser(&req); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "요청 형식이 올바르지 않습니다")
	}
	p, code, message := triggerParams(req, m.ID)
	if code != "" {
		return fail(c, fiber.StatusBadRequest, code, message)
	}
	if p.ConnectionID != "" {
		// 접근할 수 없는 커넥션의 이벤트로 트리거를 걸 수는 없다.
		// 그러면 볼 수 없는 DB의 상태 변화를 감지하는 통로가 된다.
		d, err := s.requireLevel(c, p.ConnectionID, model.LevelMonitor)
		if err != nil {
			return err
		}
		if !d.Allowed {
			return fiber.NewError(fiber.StatusForbidden, d.Reason)
		}
	}
	p.OwnerID = u.ID
	p.OwnerName = displayName(u)

	t, err := s.st.CreateTrigger(c.Context(), p)
	if err != nil {
		return err
	}
	s.audit(c, store.AuditParams{
		Action: "macro.trigger.created", TargetType: "macro", TargetID: m.ID,
		Detail: map[string]any{
			"macro": m.Name, "trigger": t.Name, "kind": t.Kind,
			"cron": t.Cron, "timezone": t.Timezone,
			"eventKind": t.EventKind, "eventSeverity": t.EventSeverity,
		},
	})
	return c.Status(fiber.StatusCreated).JSON(fiber.Map{
		"trigger": t, "describe": macro.Describe(t.Cron),
	})
}

// requireTrigger는 트리거를 읽고 그 매크로에 관리 권한이 있는지 확인한다.
//
// 트리거 ID만으로 접근을 허용하면 매크로 쪽 규칙이 무의미해진다 — 비공개 매크로의
// 자동 실행을 남이 끄거나 지울 수 있게 된다.
func (s *Server) requireTrigger(c *fiber.Ctx) (*store.MacroTrigger, error) {
	t, err := s.st.GetTrigger(c.Context(), c.Params("triggerId"))
	if errors.Is(err, store.ErrNotFound) {
		return nil, fiber.NewError(fiber.StatusNotFound, "트리거를 찾을 수 없습니다")
	}
	if err != nil {
		return nil, err
	}
	if _, err := s.requireMacro(c, t.MacroID, model.MacroAccessManage); err != nil {
		return nil, err
	}
	return t, nil
}

func (s *Server) handleUpdateTrigger(c *fiber.Ctx) error {
	existing, err := s.requireTrigger(c)
	if err != nil {
		return err
	}

	var req triggerRequest
	if err := c.BodyParser(&req); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "요청 형식이 올바르지 않습니다")
	}
	// 종류는 바꿀 수 없다. 스케줄과 이벤트는 조건 필드가 전혀 달라서, 바꾸는 것은
	// 사실상 새 트리거를 만드는 일이고 그때 옛 조건이 남아 있으면 헷갈린다.
	req.Kind = existing.Kind

	p, code, message := triggerParams(req, existing.MacroID)
	if code != "" {
		return fail(c, fiber.StatusBadRequest, code, message)
	}
	if p.ConnectionID != "" && p.ConnectionID != existing.ConnectionID {
		d, err := s.requireLevel(c, p.ConnectionID, model.LevelMonitor)
		if err != nil {
			return err
		}
		if !d.Allowed {
			return fiber.NewError(fiber.StatusForbidden, d.Reason)
		}
	}

	t, err := s.st.UpdateTrigger(c.Context(), existing.ID, p)
	if err != nil {
		return err
	}
	s.audit(c, store.AuditParams{
		Action: "macro.trigger.updated", TargetType: "macro", TargetID: t.MacroID,
		Detail: map[string]any{
			"trigger": t.Name, "kind": t.Kind, "enabled": t.Enabled, "cron": t.Cron,
		},
	})
	return c.JSON(fiber.Map{"trigger": t, "describe": macro.Describe(t.Cron)})
}

func (s *Server) handleDeleteTrigger(c *fiber.Ctx) error {
	t, err := s.requireTrigger(c)
	if err != nil {
		return err
	}
	if err := s.st.DeleteTrigger(c.Context(), t.ID); err != nil {
		return err
	}
	s.audit(c, store.AuditParams{
		Action: "macro.trigger.deleted", TargetType: "macro", TargetID: t.MacroID,
		Detail: map[string]any{"trigger": t.Name, "kind": t.Kind},
	})
	return c.JSON(fiber.Map{"ok": true})
}

// handleToggleTrigger는 트리거를 켜고 끈다.
//
// 켤 때 다음 실행 시각을 다시 잡는 이유: 꺼져 있는 동안 예정 시각이 과거가 되었을
// 텐데, 그대로 켜면 켜자마자 한 번 실행된다. 대개 그것은 의도가 아니다.
func (s *Server) handleToggleTrigger(c *fiber.Ctx) error {
	t, err := s.requireTrigger(c)
	if err != nil {
		return err
	}

	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := c.BodyParser(&body); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "요청 형식이 올바르지 않습니다")
	}

	if body.Enabled && t.Kind == store.TriggerSchedule {
		schedule, perr := macro.ParseSchedule(t.Cron)
		if perr != nil {
			return fail(c, fiber.StatusBadRequest, "invalid_cron", perr.Error())
		}
		loc, lerr := macro.LoadLocation(t.Timezone)
		if lerr != nil {
			return fail(c, fiber.StatusBadRequest, "invalid_timezone", lerr.Error())
		}
		next, ok := schedule.Next(time.Now().In(loc))
		if !ok {
			return fail(c, fiber.StatusBadRequest, "invalid_cron", "실행될 시각이 없습니다")
		}
		utc := next.UTC()
		if err := s.st.SetTriggerNextRun(c.Context(), t.ID, &utc); err != nil {
			return err
		}
	}
	if err := s.st.SetTriggerEnabled(c.Context(), t.ID, body.Enabled); err != nil {
		return err
	}

	s.audit(c, store.AuditParams{
		Action: "macro.trigger.toggled", TargetType: "macro", TargetID: t.MacroID,
		Detail: map[string]any{"trigger": t.Name, "enabled": body.Enabled},
	})
	updated, err := s.st.GetTrigger(c.Context(), t.ID)
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"trigger": updated})
}

// handlePreviewSchedule은 cron 식의 다음 실행 시각들을 미리 보여준다.
//
// 저장하기 전에 확인할 수 있어야 한다. cron 식은 눈으로 검산하기 어렵고,
// 틀렸다는 것은 대개 실행되지 않은 다음 날 아침에야 알게 된다.
func (s *Server) handlePreviewSchedule(c *fiber.Ctx) error {
	var body struct {
		Cron     string `json:"cron"`
		Timezone string `json:"timezone"`
		Count    int    `json:"count"`
	}
	if err := c.BodyParser(&body); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "요청 형식이 올바르지 않습니다")
	}
	schedule, err := macro.ParseSchedule(body.Cron)
	if err != nil {
		return fail(c, fiber.StatusBadRequest, "invalid_cron", err.Error())
	}
	loc, err := macro.LoadLocation(body.Timezone)
	if err != nil {
		return fail(c, fiber.StatusBadRequest, "invalid_timezone", err.Error())
	}

	count := body.Count
	if count <= 0 || count > 10 {
		count = 5
	}
	times := make([]time.Time, 0, count)
	cursor := time.Now().In(loc)
	for range count {
		next, ok := schedule.Next(cursor)
		if !ok {
			break
		}
		times = append(times, next)
		cursor = next
	}
	if len(times) == 0 {
		return fail(c, fiber.StatusBadRequest, "invalid_cron", "실행될 시각이 없습니다")
	}
	return c.JSON(fiber.Map{
		"describe": macro.Describe(body.Cron),
		"next":     times,
		"timezone": loc.String(),
	})
}
