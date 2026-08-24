package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// 알림 설정(메신저로 이벤트를 보내는 규칙).
//
// 커넥션 자격증명과 같은 자리(app_settings)에 두되 **웹훅 주소만 암호화**한다.
// 그 주소는 그것 하나로 아무나 그 채널에 글을 쓸 수 있는 비밀이기 때문이다.
// 나머지(채널 이름·심각도 기준)는 비밀이 아니므로 그대로 둔다 — 전부를 암호화하면
// 설정을 눈으로 확인할 방법이 사라지고, 값이 깨졌을 때 원인을 찾을 수 없다.

// SettingNotifyMattermost는 알림 설정(JSON)이다.
//
// 키 이름에 mattermost가 남아 있는 이유: 이미 저장된 설정이 이 키에 있고, 키를 바꾸면
// 그 설정이 조용히 사라진다. 이름보다 "쓰던 설정이 그대로 있는 것"이 중요하다.
const SettingNotifyMattermost = "notify.mattermost"

// 보낼 수 있는 메신저.
//
// 셋을 하나의 설정으로 다루는 이유: 필요한 것은 어느 쪽이든 "주소 하나와 무엇을
// 보낼지"이고, 그 규칙(최소 심각도·종류·해소 포함)은 메신저와 무관하다. 그래서 저장
// 구조와 화면은 한 벌이다.
//
// 다른 점은 **본문 모양**뿐이다. Slack과 Mattermost는 같다(Mattermost가 Slack
// 호환으로 만들었다) — 글자 문법만 다르다. 디스코드는 본문 구조가 다르다:
// attachments 대신 embeds 이고 색이 문자열이 아니라 정수다. 그 차이는 notify 패키지의
// forWire 한 곳에 가둬 둔다.
const (
	ProviderMattermost = "mattermost"
	ProviderSlack      = "slack"
	ProviderDiscord    = "discord"
)

// ValidProvider는 아는 메신저인지 본다.
func ValidProvider(p string) bool {
	return p == ProviderMattermost || p == ProviderSlack || p == ProviderDiscord
}

// NotifySettings는 이벤트를 메신저로 보낼 규칙이다.
type NotifySettings struct {
	Enabled bool `json:"enabled"`
	// Provider는 보낼 메신저다(mattermost | slack | discord). 비어 있으면 mattermost로 본다 —
	// 이 값이 생기기 전에 저장된 설정은 모두 Mattermost였다.
	Provider string `json:"provider,omitempty"`
	// WebhookURL은 Mattermost의 들어오는 웹훅 주소다. 저장할 때 암호화한다.
	WebhookURL string `json:"webhookUrl,omitempty"`
	// Channel은 웹훅에 설정된 기본 채널 대신 보낼 채널이다(비우면 웹훅의 기본값).
	Channel string `json:"channel,omitempty"`
	// Username은 메시지에 표시할 이름이다. 비우면 "DB Studio".
	Username string `json:"username,omitempty"`
	// MinSeverity 이상만 보낸다(info < warning < critical).
	MinSeverity Severity `json:"minSeverity,omitempty"`
	// Kinds가 비어 있지 않으면 그 종류의 이벤트만 보낸다(threshold, connectivity, drift, host …).
	Kinds []string `json:"kinds,omitempty"`
	// IncludeResolved면 이벤트가 해소될 때도 알린다.
	//
	// 기본으로 켜 두는 이유: "문제가 생겼다"만 오는 채널은 시간이 지나면 아무도 믿지
	// 않는다. 끝났다는 소식이 함께 와야 지금 열려 있는 것이 무엇인지 채널만 보고 안다.
	IncludeResolved bool `json:"includeResolved"`
	// AppURL은 알림에 붙일 이 앱의 주소다(https://db.example.com). 비우면 링크를 넣지 않는다.
	AppURL string `json:"appUrl,omitempty"`

	UpdatedAt *time.Time `json:"updatedAt,omitempty"`
	UpdatedBy string     `json:"updatedBy,omitempty"`
}

// Kind는 보낼 메신저다. 저장된 값이 비어 있으면 mattermost다.
func (n NotifySettings) Kind() string {
	if ValidProvider(n.Provider) {
		return n.Provider
	}
	return ProviderMattermost
}

// HasWebhook은 보낼 곳이 정해져 있는지다.
func (n NotifySettings) HasWebhook() bool { return strings.TrimSpace(n.WebhookURL) != "" }

// Active는 실제로 보내는 상태인지다.
func (n NotifySettings) Active() bool { return n.Enabled && n.HasWebhook() }

// Allows는 이 이벤트를 보낼지 판단한다.
func (n NotifySettings) Allows(kind string, sev Severity) bool {
	if !n.Active() {
		return false
	}
	min := n.MinSeverity
	if !min.Valid() {
		min = SeverityWarning
	}
	if sev.Rank() < min.Rank() {
		return false
	}
	if len(n.Kinds) == 0 {
		return true
	}
	for _, k := range n.Kinds {
		if strings.EqualFold(strings.TrimSpace(k), kind) {
			return true
		}
	}
	return false
}

// Masked는 화면에 돌려줄 사본이다. 웹훅 주소는 앞뒤만 남긴다.
//
// 저장된 주소를 그대로 돌려주지 않는 이유: 설정 화면을 열 수 있는 사람과 그 주소로
// 채널에 글을 쓸 수 있는 사람은 같지 않다(화면은 세션, 주소는 영구 비밀이다).
// 그렇다고 아예 감추면 "무엇이 저장되어 있는가"를 확인할 길이 없으므로 형태만 남긴다.
func (n NotifySettings) Masked() NotifySettings {
	out := n
	out.WebhookURL = maskSecretURL(n.WebhookURL)
	return out
}

func maskSecretURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	// 마지막 경로 조각(토큰)만 가린다. 호스트는 남겨야 "어디로 가는지"를 확인할 수 있다.
	at := strings.LastIndex(raw, "/")
	if at < 0 || at == len(raw)-1 {
		return "••••"
	}
	token := raw[at+1:]
	head := raw[:at+1]
	if len(token) <= 4 {
		return head + "••••"
	}
	return head + token[:2] + "••••" + token[len(token)-2:]
}

// NotifySettings는 저장된 알림 설정을 읽는다. 없으면 기본값이다.
func (s *Store) NotifySettings(ctx context.Context) (*NotifySettings, error) {
	out := &NotifySettings{MinSeverity: SeverityWarning, IncludeResolved: true}

	row := s.db.QueryRowContext(ctx,
		`SELECT value, updated_at, updated_by FROM app_settings WHERE key = ?`,
		SettingNotifyMattermost)
	var raw, updatedAt, updatedBy string
	if err := row.Scan(&raw, &updatedAt, &updatedBy); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return out, nil
		}
		return nil, fmt.Errorf("read notify settings: %w", err)
	}

	stored := struct {
		NotifySettings
		// WebhookEnc는 암호화된 웹훅 주소다. 평문 필드와 이름을 달리해,
		// 옛 값이나 손으로 넣은 값이 실수로 평문 취급되지 않게 한다.
		WebhookEnc string `json:"webhookEnc,omitempty"`
	}{}
	if err := json.Unmarshal([]byte(raw), &stored); err != nil {
		// 값이 깨졌다고 앱이 멈출 이유는 없다. 알림만 꺼진 상태로 본다.
		return out, nil
	}
	*out = stored.NotifySettings
	if stored.WebhookEnc != "" {
		url, err := s.secret.Open(stored.WebhookEnc)
		if err != nil {
			// 마스터 키가 바뀌면 여기 온다. 알림을 조용히 멈추지 않고 이유를 남긴다.
			return nil, fmt.Errorf("decrypt notify webhook: %w", err)
		}
		out.WebhookURL = url
	}
	if !out.MinSeverity.Valid() {
		out.MinSeverity = SeverityWarning
	}
	if t := parseTime(updatedAt); !t.IsZero() {
		out.UpdatedAt = &t
	}
	out.UpdatedBy = updatedBy
	return out, nil
}

// SaveNotifySettings는 알림 설정을 저장한다. 웹훅 주소는 암호화해 넣는다.
func (s *Store) SaveNotifySettings(ctx context.Context, in NotifySettings, actorID string) error {
	sealed := ""
	if url := strings.TrimSpace(in.WebhookURL); url != "" {
		enc, err := s.secret.Seal(url)
		if err != nil {
			return fmt.Errorf("encrypt notify webhook: %w", err)
		}
		sealed = enc
	}
	payload := struct {
		NotifySettings
		WebhookEnc string `json:"webhookEnc,omitempty"`
	}{NotifySettings: in, WebhookEnc: sealed}
	// 평문 주소는 저장하지 않는다. 같은 구조체를 재사용하되 그 칸만 비운다.
	payload.WebhookURL = ""
	payload.UpdatedAt, payload.UpdatedBy = nil, ""

	b, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal notify settings: %w", err)
	}
	return s.SetSetting(ctx, SettingNotifyMattermost, string(b), actorID)
}
