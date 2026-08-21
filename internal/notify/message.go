package notify

import (
	"fmt"
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

const (
	colorCritical = "#e03e3e"
	colorWarning  = "#e0a33e"
	colorInfo     = "#3e7fe0"
	colorResolved = "#2fa36b"
)

// payload는 Mattermost 들어오는 웹훅의 본문이다.
type payload struct {
	// URL은 보낼 곳이다. 본문으로 나가지 않는다(forWire에서 뺀다).
	URL         string
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

// bold는 메신저별 "굵게" 문법이다.
func bold(provider, text string) string {
	if provider == store.ProviderSlack {
		return "*" + text + "*"
	}
	return "**" + text + "**"
}

// link는 메신저별 링크 문법이다.
func link(provider, url, text string) string {
	if provider == store.ProviderSlack {
		return "<" + url + "|" + text + ">"
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
