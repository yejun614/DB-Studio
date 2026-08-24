package api

import (
	"strings"

	"github.com/gofiber/fiber/v2"

	"dbstudio/internal/store"
)

// 알림 설정 화면의 API.
//
// 슈퍼 어드민만 다룬다: 웹훅 주소는 그것 하나로 그 채널에 아무나 글을 쓸 수 있는
// 비밀이고, 알림 대상 채널을 정하는 것은 "누가 무엇을 알게 되는가"를 정하는 일이다.

// handleGetNotify는 알림 설정과 마지막 전송 결과를 돌려준다.
func (s *Server) handleGetNotify(c *fiber.Ctx) error {
	cfg, err := s.st.NotifySettings(c.Context())
	if err != nil {
		return err
	}
	masked := cfg.Masked()
	// 저장된 적이 없으면 Provider가 비어 있다. 화면이 고르개의 초기값을 정할 수 있게
	// 기본값(mattermost)을 채워 보낸다.
	masked.Provider = cfg.Kind()
	body := fiber.Map{
		// 저장된 주소는 마스킹해서 내보낸다(store.NotifySettings.Masked 주석 참고).
		"settings":  masked,
		"kinds":     notifyKinds(),
		"providers": notifyProviders(),
	}
	if s.notifier != nil {
		body["status"] = s.notifier.Status()
	}
	return c.JSON(body)
}

// notifyPayload는 설정 화면이 보내는 값이다.
//
// WebhookURL을 포인터로 받는 이유: 화면은 마스킹된 주소를 보여주므로, 주소를 바꾸지
// 않은 저장에서는 그 칸이 아예 오지 않는다. 빈 문자열과 "안 보냄"을 구분하지 못하면
// 저장 버튼을 누를 때마다 웹훅이 지워진다.
type notifyPayload struct {
	Enabled         bool     `json:"enabled"`
	Provider        string   `json:"provider"`
	WebhookURL      *string  `json:"webhookUrl"`
	Channel         string   `json:"channel"`
	Username        string   `json:"username"`
	MinSeverity     string   `json:"minSeverity"`
	Kinds           []string `json:"kinds"`
	IncludeResolved bool     `json:"includeResolved"`
	AppURL          string   `json:"appUrl"`
}

func (s *Server) handlePutNotify(c *fiber.Ctx) error {
	var in notifyPayload
	if err := c.BodyParser(&in); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "요청 형식이 올바르지 않습니다")
	}
	cur, err := s.st.NotifySettings(c.Context())
	if err != nil {
		return err
	}
	next, ferr := mergeNotify(*cur, in)
	if ferr != "" {
		return fail(c, fiber.StatusBadRequest, "invalid_settings", ferr)
	}

	u := currentUser(c)
	if err := s.st.SaveNotifySettings(c.Context(), next, u.ID); err != nil {
		return err
	}
	s.audit(c, store.AuditParams{
		Action: "notify.updated", TargetType: "settings", TargetID: "notify.mattermost",
		// 주소 자체는 감사 로그에도 남기지 않는다. 남는 것은 "켰는가/무엇을 보내는가"다.
		Detail: map[string]any{
			"provider": next.Kind(),
			"enabled":  next.Enabled, "minSeverity": next.MinSeverity,
			"kinds": next.Kinds, "includeResolved": next.IncludeResolved,
			"hasWebhook": next.HasWebhook(),
		},
	})

	saved, err := s.st.NotifySettings(c.Context())
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"settings": saved.Masked()})
}

// handleTestNotify는 지금 설정으로 한 건을 보내 본다.
//
// 저장된 설정으로 보내는 이유: 화면의 값으로 보내면 "테스트는 됐는데 실제로는 안 온다"가
// 가능해진다(저장 전 값과 저장된 값이 다를 수 있다). 그래서 저장 → 테스트 순서다.
func (s *Server) handleTestNotify(c *fiber.Ctx) error {
	if s.notifier == nil {
		return fail(c, fiber.StatusServiceUnavailable, "notifier_off", "알림 전송기가 꺼져 있습니다")
	}
	cfg, err := s.st.NotifySettings(c.Context())
	if err != nil {
		return err
	}
	if !cfg.HasWebhook() {
		return fail(c, fiber.StatusBadRequest, "no_webhook",
			"웹훅 주소를 먼저 저장하세요")
	}
	if err := s.notifier.Test(c.Context(), *cfg); err != nil {
		return fail(c, fiber.StatusBadGateway, "send_failed", err.Error())
	}
	s.audit(c, store.AuditParams{
		Action: "notify.tested", TargetType: "settings", TargetID: "notify.mattermost",
	})
	return c.JSON(fiber.Map{"ok": true, "status": s.notifier.Status()})
}

// mergeNotify는 들어온 값을 지금 설정 위에 얹는다. 두 번째 반환값은 거절 사유다.
func mergeNotify(cur store.NotifySettings, in notifyPayload) (store.NotifySettings, string) {
	out := cur
	out.Enabled = in.Enabled
	provider := strings.TrimSpace(in.Provider)
	if provider == "" {
		provider = cur.Kind()
	}
	if !store.ValidProvider(provider) {
		return out, "알 수 없는 메신저입니다"
	}
	out.Provider = provider
	out.Channel = strings.TrimSpace(in.Channel)
	out.Username = strings.TrimSpace(in.Username)
	out.IncludeResolved = in.IncludeResolved
	out.AppURL = strings.TrimRight(strings.TrimSpace(in.AppURL), "/")

	if in.WebhookURL != nil {
		url := strings.TrimSpace(*in.WebhookURL)
		if url != "" && !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
			return out, "웹훅 주소는 http:// 또는 https:// 로 시작해야 합니다"
		}
		out.WebhookURL = url
	}

	sev := store.Severity(strings.TrimSpace(in.MinSeverity))
	if sev == "" {
		sev = store.SeverityWarning
	}
	if !sev.Valid() {
		return out, "알 수 없는 심각도입니다"
	}
	out.MinSeverity = sev

	known := map[string]bool{}
	for _, k := range notifyKinds() {
		known[k.Value] = true
	}
	kinds := []string{}
	for _, k := range in.Kinds {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		if !known[k] {
			return out, "알 수 없는 이벤트 종류입니다: " + k
		}
		kinds = append(kinds, k)
	}
	out.Kinds = kinds

	if out.Enabled && !out.HasWebhook() {
		return out, "웹훅 주소가 있어야 알림을 켤 수 있습니다"
	}
	return out, ""
}

// notifyProviders는 고를 수 있는 메신저다. 각각의 안내가 다르므로 함께 보낸다 —
// Slack의 앱 웹훅은 채널·보내는 이름 지정을 무시하는데, 그 사실을 모르면
// "채널을 적었는데 다른 곳으로 간다"로 보인다.
func notifyProviders() []struct {
	Value string `json:"value"`
	Label string `json:"label"`
	Note  string `json:"note"`
} {
	return []struct {
		Value string `json:"value"`
		Label string `json:"label"`
		Note  string `json:"note"`
	}{
		{store.ProviderMattermost, "Mattermost",
			"통합 → 들어오는 웹훅에서 주소를 만듭니다. 채널과 보내는 이름을 여기서 덮어쓸 수 있습니다."},
		{store.ProviderSlack, "Slack",
			"앱 설정 → Incoming Webhooks에서 주소를 만듭니다. 최신 Slack 앱 웹훅은 채널·보내는 이름 지정을 " +
				"무시하고 웹훅을 만들 때 고른 채널로 보냅니다."},
		{store.ProviderDiscord, "Discord",
			"채널 설정 → 연동 → 웹후크에서 주소를 만듭니다. 채널은 웹후크를 만들 때 정해져 여기서 바꿀 수 " +
				"없고, 보내는 이름은 반영됩니다."},
	}
}

// notifyKinds는 고를 수 있는 이벤트 종류다. 화면이 목록을 따로 들고 있지 않도록 함께 준다.
func notifyKinds() []struct {
	Value string `json:"value"`
	Label string `json:"label"`
} {
	return []struct {
		Value string `json:"value"`
		Label string `json:"label"`
	}{
		{store.EventThreshold, "임계치"},
		{store.EventConnectivity, "접속"},
		{store.EventDrift, "스키마 변경"},
		{store.EventCollectError, "수집 오류"},
		{store.EventHost, "서버 컴퓨터"},
	}
}
