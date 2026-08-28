package notify

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"dbstudio/internal/store"
)

// 메시지 만들기.
//
// 첨부(attachment)를 쓰는 이유: 심각도를 **색**으로 먼저 알리기 위해서다. 채널에
// 여러 알림이 쌓였을 때 사람이 먼저 하는 일은 읽기가 아니라 훑기이고, 그때 색만큼
// 빠른 단서가 없다. 본문 한 줄만 보내면 심각과 정보가 같은 모양으로 쌓인다.
//
// Slack과 Mattermost는 본문 구조가 같다(Mattermost가 Slack 호환으로 만들었다).
// 다른 것은 **글자 문법**이다: Slack의 mrkdwn은 굵게가 *한 개* 별표이고 링크가
// <주소|글자>인 반면, Mattermost는 표준 마크다운을 쓴다. 그 차이를 여기 두 함수
// (bold·link)에 가둬 두면 나머지 코드는 어느 메신저인지 몰라도 된다.
//
// 디스코드는 본문 구조 자체가 다르다(embeds, 정수 색). 그 차이는 discordWire 하나에
// 가둬 두고, 무엇을 보낼지 정하는 코드는 셋을 구분하지 않는다.

const (
	colorCritical = "#e03e3e"
	colorWarning  = "#e0a33e"
	colorInfo     = "#3e7fe0"
	colorResolved = "#2fa36b"
)

// payload는 메신저에 보낼 내용이다. 메신저별 JSON 모양은 forWire가 만든다.
type payload struct {
	// URL은 보낼 곳이다. 본문으로 나가지 않는다(forWire에서 뺀다).
	URL string
	// Provider는 어느 메신저의 모양으로 만들지 정한다.
	Provider    string
	Channel     string
	Username    string
	Text        string
	Attachments []attachment
}

type attachment struct {
	Color  string  `json:"color,omitempty"`
	Title  string  `json:"title,omitempty"`
	Text   string  `json:"text,omitempty"`
	Fields []field `json:"fields,omitempty"`
}

type field struct {
	Title string `json:"title"`
	Value string `json:"value"`
	Short bool   `json:"short"`
}

// forWire는 실제로 보낼 JSON 구조다.
func (p payload) forWire() map[string]any {
	if p.Provider == store.ProviderDiscord {
		return p.discordWire()
	}
	if p.Provider == store.ProviderTelegram {
		return p.telegramWire()
	}
	out := map[string]any{"text": p.Text}
	if p.Channel != "" {
		out["channel"] = p.Channel
	}
	if p.Username != "" {
		out["username"] = p.Username
	}
	if len(p.Attachments) > 0 {
		out["attachments"] = p.Attachments
	}
	return out
}

// discordWire는 디스코드 웹훅 본문이다.
//
// 왜 따로 만드는가: 디스코드는 Slack 호환 본문을 그대로 받지 않는다. 첨부(attachments)
// 대신 embeds 를 쓰고, 색은 "#e03e3e" 같은 문자열이 아니라 정수(0xe03e3e)다. 주소 끝에
// /slack 을 붙이면 Slack 본문도 받아 주지만 그 경로에서는 색과 필드가 제대로 살지 않아,
// 심각도를 색으로 먼저 알린다는 이 기능의 목적이 사라진다.
//
// 채널을 싣지 않는 이유: 디스코드 웹훅은 만들 때 채널이 정해지고 본문으로 바꿀 수 없다.
// 보내는 이름(username)은 반영된다.
func (p payload) discordWire() map[string]any {
	embed := map[string]any{}
	if len(p.Attachments) > 0 {
		a := p.Attachments[0]
		if c := discordColor(a.Color); c > 0 {
			embed["color"] = c
		}
		if a.Text != "" {
			embed["description"] = a.Text
		}
		if len(a.Fields) > 0 {
			fields := make([]map[string]any, 0, len(a.Fields))
			for _, f := range a.Fields {
				fields = append(fields, map[string]any{
					"name": f.Title, "value": f.Value, "inline": f.Short,
				})
			}
			embed["fields"] = fields
		}
	}

	out := map[string]any{
		// 본문 상한이 2000자다. 넘으면 400으로 거부되므로 여기서 자른다 —
		// 잘린 알림이 오는 것과 아무 알림도 오지 않는 것은 다른 이야기다.
		"content": clamp(p.Text, 2000),
	}
	if p.Username != "" {
		out["username"] = p.Username
	}
	if len(embed) > 0 {
		out["embeds"] = []map[string]any{embed}
	}
	return out
}

// telegramWire는 텔레그램 sendMessage 본문이다.
//
// 색도 첨부도 없다. 그래서 첨부에 담았던 것(대상·심각도·지표·값)을 줄로 펼쳐 적는다 —
// 채널에서 훑을 때 색 대신 맨 앞의 그림문자가 그 일을 한다.
//
// 서식(parse_mode)을 쓰지 않는 이유: 텔레그램의 HTML·마크다운 모드는 본문에 <, &, _
// 같은 글자가 들어 있으면 400으로 거부한다. 이벤트 메시지에는 DB가 준 문자열이 그대로
// 들어오므로(테이블 이름, 오류 문구) 언젠가 반드시 그런 글자가 온다. 그때 알림이
// 통째로 사라지는 것보다, 굵은 글씨 없이 확실히 도착하는 편이 낫다.
func (p payload) telegramWire() map[string]any {
	lines := []string{plainEmoji(p.Text)}
	if len(p.Attachments) > 0 {
		a := p.Attachments[0]
		if a.Title != "" {
			lines = append(lines, a.Title)
		}
		for _, f := range a.Fields {
			lines = append(lines, fmt.Sprintf("%s: %s", f.Title, f.Value))
		}
		if a.Text != "" {
			lines = append(lines, plainEmoji(a.Text))
		}
	}
	return map[string]any{
		"chat_id": p.Channel,
		// 4096자가 상한이다. 넘으면 거부되므로 여기서 자른다.
		"text": clamp(strings.Join(lines, "\n"), 4096),
		// 링크 미리보기를 끈다. 알림 한 건마다 앱 화면의 미리보기 카드가 붙으면
		// 채널이 그림으로 덮인다.
		"disable_web_page_preview": true,
	}
}

// plainEmoji는 :shortcode: 를 진짜 그림문자로 바꾼다.
//
// Slack·Mattermost·Discord는 :rotating_light: 를 알아보지만 텔레그램은 그대로 글자로
// 보여준다. 심각도를 한눈에 알리는 것이 맨 앞 글자의 역할이라, 그것이 ":rotating_light:"
// 라는 글자로 보이면 아무 역할도 하지 못한다.
func plainEmoji(s string) string {
	rep := strings.NewReplacer(
		":rotating_light:", "🚨",
		":warning:", "⚠️",
		":information_source:", "ℹ️",
		":white_check_mark:", "✅",
	)
	return rep.Replace(s)
}

// discordColor는 "#rrggbb"를 디스코드가 받는 정수로 바꾼다.
func discordColor(hex string) int64 {
	s := strings.TrimPrefix(strings.TrimSpace(hex), "#")
	if len(s) != 6 {
		return 0
	}
	n, err := strconv.ParseInt(s, 16, 64)
	if err != nil {
		return 0
	}
	return n
}

func clamp(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}

// bold는 메신저별 "굵게" 문법이다.
//
// 텔레그램은 서식 없이 보내므로(telegramWire 참고) 표시를 붙이지 않는다. 붙이면
// 별표가 그대로 보인다.
func bold(provider, text string) string {
	switch provider {
	case store.ProviderSlack:
		return "*" + text + "*"
	case store.ProviderTelegram:
		return text
	}
	return "**" + text + "**"
}

// link는 메신저별 링크 문법이다.
func link(provider, url, text string) string {
	switch provider {
	case store.ProviderSlack:
		return "<" + url + "|" + text + ">"
	case store.ProviderTelegram:
		// 평문이라 주소를 그대로 적는다. 텔레그램은 주소를 알아서 누를 수 있게 만든다.
		return text + ": " + url
	}
	return "[" + text + "](" + url + ")"
}

func displayName(cfg store.NotifySettings) string {
	if name := strings.TrimSpace(cfg.Username); name != "" {
		return name
	}
	return "DB Studio"
}

// buildPayload는 이벤트 한 건을 메시지로 만든다.
func buildPayload(cfg *store.NotifySettings, ev *store.Event, connName string, resolved bool) payload {
	head := ":rotating_light:"
	color := colorInfo
	state := "발생"
	switch {
	case resolved:
		head, color, state = ":white_check_mark:", colorResolved, "해소"
	case ev.Severity == store.SeverityCritical:
		head, color = ":rotating_light:", colorCritical
	case ev.Severity == store.SeverityWarning:
		head, color = ":warning:", colorWarning
	default:
		head, color = ":information_source:", colorInfo
	}

	where := connName
	if where == "" {
		// 커넥션에 속하지 않는 이벤트(호스트 상태 등)다. 빈칸으로 두면 어디 이야기인지
		// 알 수 없으므로 그 사실을 그대로 적는다.
		where = "서버 컴퓨터"
	}

	fields := []field{
		{Title: "대상", Value: where, Short: true},
		{Title: "심각도", Value: severityLabel(ev.Severity), Short: true},
	}
	if ev.Metric != "" {
		fields = append(fields, field{Title: "지표", Value: ev.Metric, Short: true})
	}
	if ev.Value != nil {
		value := formatNumber(*ev.Value)
		if ev.Threshold != nil {
			value += fmt.Sprintf(" (기준 %s)", formatNumber(*ev.Threshold))
		}
		// 해소 알림의 값은 **지금 값이 아니라 문제일 때의 값**이다. 그냥 "값"이라고
		// 적으면 이미 정상으로 돌아온 지표를 아직 넘은 것처럼 읽는다.
		title := "값"
		if resolved {
			title = "발생 당시 값"
		}
		fields = append(fields, field{Title: title, Value: value, Short: true})
	}
	if resolved {
		// 얼마나 오래 열려 있었는지는 해소 알림에서 가장 쓸모 있는 숫자다
		// ("5분 만에 끝났다"와 "이틀 동안 열려 있었다"는 전혀 다른 이야기다).
		if d := eventDuration(ev); d != "" {
			fields = append(fields, field{Title: "지속", Value: d, Short: true})
		}
	} else if ev.Occurrences > 1 {
		fields = append(fields, field{Title: "반복", Value: fmt.Sprintf("%d회", ev.Occurrences), Short: true})
	}

	provider := cfg.Kind()
	body := ""
	if url := eventLink(cfg.AppURL); url != "" {
		body = link(provider, url, "이벤트 화면에서 보기")
	}

	return payload{
		URL:      strings.TrimSpace(cfg.WebhookURL),
		Provider: provider,
		Channel:  strings.TrimSpace(cfg.Channel),
		Username: displayName(*cfg),
		Text: fmt.Sprintf("%s %s %s", head,
			bold(provider, fmt.Sprintf("[%s %s]", kindLabel(ev.Kind), state)), ev.Message),
		Attachments: []attachment{{
			Color:  color,
			Fields: fields,
			Text:   body,
		}},
	}
}

// eventLink는 이벤트 목록 주소다. 앱 주소를 모르면 빈 문자열이다.
//
// 이벤트 하나로 바로 가지 않는 이유: 이 앱의 이벤트 화면은 목록이고 개별 주소가 없다.
// 없는 주소를 만들어 보내면 눌렀을 때 빈 화면이 나온다.
func eventLink(appURL string) string {
	base := strings.TrimRight(strings.TrimSpace(appURL), "/")
	if base == "" {
		return ""
	}
	return base + "/events"
}

// eventDuration은 이벤트가 열려 있던 시간을 사람이 읽는 말로 만든다.
func eventDuration(ev *store.Event) string {
	if ev.StartedAt.IsZero() || ev.ResolvedAt == nil {
		return ""
	}
	d := ev.ResolvedAt.Sub(ev.StartedAt)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d초", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d분", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d시간 %d분", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%d일 %d시간", int(d.Hours())/24, int(d.Hours())%24)
	}
}

func severityLabel(s store.Severity) string {
	switch s {
	case store.SeverityCritical:
		return "심각"
	case store.SeverityWarning:
		return "경고"
	case store.SeverityInfo:
		return "정보"
	}
	return string(s)
}

func kindLabel(kind string) string {
	switch kind {
	case store.EventThreshold:
		return "임계치"
	case store.EventConnectivity:
		return "접속"
	case store.EventDrift:
		return "스키마 변경"
	case store.EventCollectError:
		return "수집 오류"
	case store.EventHost:
		return "서버 컴퓨터"
	}
	return kind
}

// describeFilter는 "무엇을 보내는 설정인가"를 한 줄로 적는다. 테스트 메시지에 넣어,
// 채널을 보는 사람이 앞으로 무엇이 올지 미리 알 수 있게 한다.
func describeFilter(cfg store.NotifySettings) string {
	parts := []string{severityLabel(cfg.MinSeverity) + " 이상"}
	if len(cfg.Kinds) > 0 {
		labels := make([]string, 0, len(cfg.Kinds))
		for _, k := range cfg.Kinds {
			labels = append(labels, kindLabel(strings.TrimSpace(k)))
		}
		parts = append(parts, strings.Join(labels, "·"))
	} else {
		parts = append(parts, "모든 종류")
	}
	if cfg.IncludeResolved {
		parts = append(parts, "해소 알림 포함")
	}
	return strings.Join(parts, " · ")
}

// formatNumber는 사람이 읽을 숫자다. 소수점이 필요 없으면 붙이지 않는다.
func formatNumber(v float64) string {
	if v == float64(int64(v)) {
		return fmt.Sprintf("%d", int64(v))
	}
	return fmt.Sprintf("%.2f", v)
}
