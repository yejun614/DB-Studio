package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"dbstudio/internal/store"
)

// 알림의 값어치는 "필요한 것만, 읽을 수 있는 모양으로" 가는 데 있다.
// 그래서 이 시험들은 두 가지를 본다: 무엇을 거르는가, 무엇을 적어 보내는가.

func TestAllowsFilters(t *testing.T) {
	base := store.NotifySettings{Enabled: true, WebhookURL: "https://mm.example.com/hooks/x"}

	cases := []struct {
		name string
		cfg  store.NotifySettings
		kind string
		sev  store.Severity
		want bool
	}{
		{"기본은 경고 이상", base, store.EventThreshold, store.SeverityWarning, true},
		{"정보는 기본에서 제외", base, store.EventThreshold, store.SeverityInfo, false},
		{"심각도를 낮추면 정보도 간다",
			withMin(base, store.SeverityInfo), store.EventThreshold, store.SeverityInfo, true},
		{"종류를 고르면 그것만",
			withKinds(base, store.EventHost), store.EventThreshold, store.SeverityCritical, false},
		{"고른 종류는 간다",
			withKinds(base, store.EventHost), store.EventHost, store.SeverityCritical, true},
		{"꺼져 있으면 아무것도 안 간다",
			withEnabled(base, false), store.EventThreshold, store.SeverityCritical, false},
		{"주소가 없으면 켜 있어도 안 간다",
			store.NotifySettings{Enabled: true}, store.EventThreshold, store.SeverityCritical, false},
	}
	for _, tc := range cases {
		if got := tc.cfg.Allows(tc.kind, tc.sev); got != tc.want {
			t.Errorf("%s: Allows = %v, 기대값 %v", tc.name, got, tc.want)
		}
	}
}

func withMin(c store.NotifySettings, s store.Severity) store.NotifySettings {
	c.MinSeverity = s
	return c
}

func withKinds(c store.NotifySettings, kinds ...string) store.NotifySettings {
	c.Kinds = kinds
	return c
}

func withEnabled(c store.NotifySettings, v bool) store.NotifySettings {
	c.Enabled = v
	return c
}

func TestPayloadContent(t *testing.T) {
	value, threshold := 92.5, 85.0
	ev := &store.Event{
		Kind: store.EventThreshold, Severity: store.SeverityCritical,
		Metric: "connections.used_pct", Message: "커넥션 사용률이 높습니다",
		Value: &value, Threshold: &threshold, Occurrences: 3,
	}
	cfg := &store.NotifySettings{
		WebhookURL: "https://mm.example.com/hooks/x",
		AppURL:     "https://db.example.com/",
		Channel:    "db-alerts",
	}

	p := buildPayload(cfg, ev, "운영 결제 DB", false)
	if !strings.Contains(p.Text, "운영") && !strings.Contains(p.Text, ev.Message) {
		t.Errorf("본문에 이벤트 내용이 없습니다: %q", p.Text)
	}
	if p.Username != "DB Studio" {
		t.Errorf("보내는 이름 = %q", p.Username)
	}
	if p.Channel != "db-alerts" {
		t.Errorf("채널 = %q", p.Channel)
	}
	if len(p.Attachments) != 1 || p.Attachments[0].Color != colorCritical {
		t.Fatalf("심각 이벤트의 색이 다릅니다: %+v", p.Attachments)
	}
	// 값과 기준을 함께 보내야 "얼마나 넘었는가"를 채널에서 바로 안다.
	fields := map[string]string{}
	for _, f := range p.Attachments[0].Fields {
		fields[f.Title] = f.Value
	}
	if fields["대상"] != "운영 결제 DB" {
		t.Errorf("대상 = %q", fields["대상"])
	}
	if !strings.Contains(fields["값"], "92.50") || !strings.Contains(fields["값"], "85") {
		t.Errorf("값 = %q (값과 기준이 함께 있어야 합니다)", fields["값"])
	}
	if fields["반복"] != "3회" {
		t.Errorf("반복 = %q", fields["반복"])
	}
	if !strings.Contains(p.Attachments[0].Text, "https://db.example.com/events") {
		t.Errorf("이벤트 화면 링크가 없습니다: %q", p.Attachments[0].Text)
	}

	// 해소는 색과 말이 달라야 한다. 같으면 채널에서 발생과 구분되지 않는다.
	done := buildPayload(cfg, ev, "운영 결제 DB", true)
	if done.Attachments[0].Color != colorResolved {
		t.Errorf("해소 색 = %q", done.Attachments[0].Color)
	}
	if !strings.Contains(done.Text, "해소") {
		t.Errorf("해소 본문 = %q", done.Text)
	}
	// 커넥션이 없는 이벤트(호스트)는 "어디 이야기인지"를 그대로 적는다.
	host := buildPayload(cfg, &store.Event{Kind: store.EventHost, Severity: store.SeverityWarning,
		Message: "디스크가 찼습니다"}, "", false)
	if got := host.Attachments[0].Fields[0].Value; got != "서버 컴퓨터" {
		t.Errorf("대상 = %q", got)
	}
}

func TestPostSendsJSONAndReportsFailure(t *testing.T) {
	var mu sync.Mutex
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		_ = json.Unmarshal(body, &got)
		mu.Unlock()
		if r.Header.Get("Content-Type") != "application/json" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if strings.Contains(r.URL.Path, "broken") {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte("Couldn't find the channel"))
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	n := New(nil, nil)
	ctx := context.Background()
	p := payload{URL: srv.URL + "/hooks/abc", Username: "DB Studio", Text: "안녕",
		Attachments: []attachment{{Color: colorInfo, Title: "t"}}}
	if err := n.post(ctx, p); err != nil {
		t.Fatalf("전송 실패: %v", err)
	}
	// 값을 복사하고 곧바로 놓는다. 잠근 채로 다음 요청을 보내면 핸들러가 그 잠금을
	// 기다리다 클라이언트 타임아웃이 난다(시험이 스스로 만든 교착이다).
	mu.Lock()
	sent := got
	mu.Unlock()
	if sent["text"] != "안녕" || sent["username"] != "DB Studio" {
		t.Errorf("보낸 본문 = %v", sent)
	}
	if _, ok := sent["attachments"]; !ok {
		t.Error("첨부가 빠졌습니다")
	}
	if _, ok := sent["channel"]; ok {
		t.Error("채널을 비웠는데 본문에 들어갔습니다(웹훅 기본 채널을 덮어씁니다)")
	}

	// 거부당하면 이유를 그대로 전한다. 상태 코드만 남기면 무엇이 틀렸는지 알 수 없다.
	bad := payload{URL: srv.URL + "/hooks/broken", Text: "x"}
	err := n.post(ctx, bad)
	if err == nil {
		t.Fatal("거부를 성공으로 처리했습니다")
	}
	if !strings.Contains(err.Error(), "Couldn't find the channel") {
		t.Errorf("사유가 전달되지 않았습니다: %v", err)
	}
}

func TestPostRejectsBadTargets(t *testing.T) {
	n := New(nil, nil)
	ctx := context.Background()
	for _, tc := range []struct{ name, url, want string }{
		{"빈 주소", "", "올바르지 않"},
		{"http/https가 아님", "ftp://example.com/x", "http"},
		// 링크로컬은 클라우드 메타데이터 서비스가 사는 자리다. 잘못 적은 주소 하나가
		// 인스턴스 자격증명을 밖으로 내보내는 통로가 되어서는 안 된다.
		{"링크로컬", "http://169.254.169.254/latest/meta-data", "링크로컬"},
	} {
		err := n.post(ctx, payload{URL: tc.url, Text: "x"})
		if err == nil {
			t.Errorf("%s: 막지 못했습니다", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: 사유 = %v", tc.name, err)
		}
	}
}

func TestStatusRemembersLastResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	n := New(nil, nil)
	// 설정 화면이 "지금 잘 가고 있는가"에 답하려면 마지막 결과가 남아 있어야 한다.
	if err := n.Test(context.Background(), store.NotifySettings{WebhookURL: srv.URL + "/hooks/x"}); err == nil {
		t.Fatal("실패를 성공으로 처리했습니다")
	}
	st := n.Status()
	if st.OK || st.Detail == "" || st.At == nil {
		t.Fatalf("마지막 상태 = %+v", st)
	}
	if time.Since(*st.At) > time.Minute {
		t.Errorf("기록된 시각이 이상합니다: %v", *st.At)
	}
	// 한 번도 보내지 않았다면 시각은 비어 있어야 한다. 0001-01-01이 나가면
	// 화면은 그것을 "그때 실패했다"로 읽는다.
	if fresh := New(nil, nil).Status(); fresh.At != nil {
		t.Errorf("보낸 적 없는 전송기의 시각 = %v", fresh.At)
	}
}

// Slack과 Mattermost는 본문 구조가 같고 **글자 문법**만 다르다. 그 차이를 틀리면
// 채널에 별표와 괄호가 그대로 찍힌다 — 알림이 오긴 오는데 읽기 싫어진다.
func TestProviderMarkupDiffers(t *testing.T) {
	ev := &store.Event{
		Kind: store.EventConnectivity, Severity: store.SeverityCritical,
		Message: "접속할 수 없습니다",
	}
	mm := buildPayload(&store.NotifySettings{
		Provider: store.ProviderMattermost, AppURL: "https://db.example.com",
	}, ev, "운영 DB", false)
	sl := buildPayload(&store.NotifySettings{
		Provider: store.ProviderSlack, AppURL: "https://db.example.com",
	}, ev, "운영 DB", false)

	if !strings.Contains(mm.Text, "**[접속 발생]**") {
		t.Errorf("Mattermost 본문 = %q (표준 마크다운이어야 합니다)", mm.Text)
	}
	if !strings.Contains(sl.Text, "*[접속 발생]*") || strings.Contains(sl.Text, "**") {
		t.Errorf("Slack 본문 = %q (mrkdwn은 별표 하나입니다)", sl.Text)
	}
	if !strings.Contains(mm.Attachments[0].Text, "[이벤트 화면에서 보기](https://db.example.com/events)") {
		t.Errorf("Mattermost 링크 = %q", mm.Attachments[0].Text)
	}
	if !strings.Contains(sl.Attachments[0].Text, "<https://db.example.com/events|이벤트 화면에서 보기>") {
		t.Errorf("Slack 링크 = %q", sl.Attachments[0].Text)
	}
	// 색·필드처럼 구조에 해당하는 것은 두 메신저가 같다. 여기가 갈리면 한쪽만
	// 심각도를 색으로 보여주게 된다.
	if mm.Attachments[0].Color != sl.Attachments[0].Color {
		t.Errorf("색이 다릅니다: %q vs %q", mm.Attachments[0].Color, sl.Attachments[0].Color)
	}
	if len(mm.Attachments[0].Fields) != len(sl.Attachments[0].Fields) {
		t.Error("필드 구성이 다릅니다")
	}
}

// TestDiscordWire는 디스코드 본문이 그 서비스가 받는 모양인지 본다.
//
// 이 시험이 필요한 이유: 디스코드는 Slack 호환 본문을 받지 않는다. attachments 를
// 그대로 보내면 200이 아니라 400이 오거나, 색과 필드가 사라진 맨 줄만 남는다.
func TestDiscordWire(t *testing.T) {
	cfg := store.NotifySettings{
		Enabled: true, Provider: store.ProviderDiscord,
		WebhookURL: "https://discord.com/api/webhooks/1/abc",
		Channel:    "무시되는-채널", Username: "DB 감시",
		AppURL: "https://db.example.com",
	}
	value, threshold := 96.0, 95.0
	ev := &store.Event{
		Kind: store.EventThreshold, Severity: store.SeverityCritical,
		Message: "세션 사용률이 96%입니다", Metric: "conn.used_pct",
		Value: &value, Threshold: &threshold, Occurrences: 3,
	}
	wire := buildPayload(&cfg, ev, "shop", false).forWire()

	if _, ok := wire["text"]; ok {
		t.Error("디스코드에 text 를 보냈습니다(content 여야 합니다)")
	}
	if _, ok := wire["attachments"]; ok {
		t.Error("디스코드에 attachments 를 보냈습니다(embeds 여야 합니다)")
	}
	if _, ok := wire["channel"]; ok {
		t.Error("디스코드는 채널을 본문으로 바꿀 수 없으므로 싣지 않아야 합니다")
	}
	if got := wire["username"]; got != "DB 감시" {
		t.Errorf("username = %v", got)
	}
	content, _ := wire["content"].(string)
	if !strings.Contains(content, "세션 사용률") {
		t.Errorf("content = %q", content)
	}
	if !strings.Contains(content, "**") {
		t.Errorf("디스코드는 표준 마크다운을 쓴다: %q", content)
	}

	embeds, ok := wire["embeds"].([]map[string]any)
	if !ok || len(embeds) != 1 {
		t.Fatalf("embeds = %#v", wire["embeds"])
	}
	embed := embeds[0]
	// 색은 문자열이 아니라 정수다. "#e03e3e" 를 그대로 보내면 거부된다.
	if got := embed["color"]; got != int64(0xe03e3e) {
		t.Errorf("color = %#v (정수 0xe03e3e 여야 합니다)", got)
	}
	fields, ok := embed["fields"].([]map[string]any)
	if !ok || len(fields) == 0 {
		t.Fatalf("fields = %#v", embed["fields"])
	}
	// 필드 키 이름도 다르다: title/value/short → name/value/inline.
	if _, ok := fields[0]["name"]; !ok {
		t.Errorf("필드에 name 이 없습니다: %#v", fields[0])
	}
	if _, ok := fields[0]["inline"]; !ok {
		t.Errorf("필드에 inline 이 없습니다: %#v", fields[0])
	}
	desc, _ := embed["description"].(string)
	if !strings.Contains(desc, "https://db.example.com/events") {
		t.Errorf("이벤트 링크가 없습니다: %q", desc)
	}
}

// 텔레그램은 웹훅이 아니라 봇 API다. 본문 모양도, 보낼 주소를 만드는 방법도 다르다.
func TestTelegramWire(t *testing.T) {
	cfg := store.NotifySettings{
		Enabled: true, Provider: store.ProviderTelegram,
		WebhookURL: "123456789:AAH1234567890abcdefghij",
		Channel:    "-1001234567890", Username: "무시되는 이름",
		AppURL: "https://db.example.com",
	}
	value, threshold := 96.0, 95.0
	ev := &store.Event{
		Kind: store.EventThreshold, Severity: store.SeverityCritical,
		Message: "세션 사용률이 96%입니다", Metric: "conn.used_pct",
		Value: &value, Threshold: &threshold,
	}
	wire := buildPayload(&cfg, ev, "shop", false).forWire()

	if got := wire["chat_id"]; got != "-1001234567890" {
		t.Errorf("chat_id = %v (채팅 ID가 곧 보낼 곳이다)", got)
	}
	for _, key := range []string{"attachments", "embeds", "channel", "username"} {
		if _, ok := wire[key]; ok {
			t.Errorf("텔레그램에 %s 를 보냈습니다", key)
		}
	}
	text, _ := wire["text"].(string)
	// 첨부가 없으므로 대상·심각도·지표·값이 본문 줄로 들어가야 한다.
	for _, want := range []string{"세션 사용률", "shop", "심각", "conn.used_pct", "96", "기준 95"} {
		if !strings.Contains(text, want) {
			t.Errorf("본문에 %q 가 없습니다:\n%s", want, text)
		}
	}
	if !strings.Contains(text, "https://db.example.com/events") {
		t.Errorf("이벤트 링크가 없습니다:\n%s", text)
	}
	// 그림문자는 진짜 글자여야 한다. 텔레그램은 :rotating_light: 를 그대로 보여준다.
	if strings.Contains(text, ":rotating_light:") || !strings.Contains(text, "🚨") {
		t.Errorf("그림문자가 바뀌지 않았습니다:\n%s", text)
	}
	// 서식 표시가 글자로 남으면 안 된다(서식 없이 보낸다).
	if strings.Contains(text, "**") || strings.Contains(text, "](") {
		t.Errorf("서식 표시가 그대로 들어갔습니다:\n%s", text)
	}
	if _, ok := wire["parse_mode"]; ok {
		t.Error("parse_mode 를 보내면 <, &, _ 가 든 메시지가 400으로 거부된다")
	}
}

// 실제로 나가는 요청을 본다. 본문을 만드는 것과 보내는 것은 다른 일이고,
// 텔레그램은 그 둘이 모두 다른 유일한 메신저다(주소도 본문도).
func TestTelegramPost(t *testing.T) {
	var mu sync.Mutex
	var got map[string]any
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		path = r.URL.Path
		_ = json.Unmarshal(body, &got)
		mu.Unlock()
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	n := New(nil, nil)
	// 완성된 주소는 그대로 쓴다. 그래서 시험용 서버로 보낼 수 있다.
	p := payload{
		Provider: store.ProviderTelegram,
		URL:      srv.URL + "/bot123:abc/sendMessage",
		Channel:  "-100777",
		Text:     ":rotating_light: 무언가 일어났습니다",
		Attachments: []attachment{{
			Color:  colorCritical,
			Fields: []field{{Title: "대상", Value: "shop"}},
		}},
	}
	if err := n.post(context.Background(), p); err != nil {
		t.Fatalf("전송 실패: %v", err)
	}
	mu.Lock()
	sent, sentPath := got, path
	mu.Unlock()

	if sentPath != "/bot123:abc/sendMessage" {
		t.Errorf("주소 = %q", sentPath)
	}
	if sent["chat_id"] != "-100777" {
		t.Errorf("chat_id = %v", sent["chat_id"])
	}
	text, _ := sent["text"].(string)
	if !strings.Contains(text, "🚨") || !strings.Contains(text, "대상: shop") {
		t.Errorf("본문 = %q", text)
	}
	if sent["disable_web_page_preview"] != true {
		t.Errorf("링크 미리보기를 끄지 않았습니다: %v", sent["disable_web_page_preview"])
	}

	// 채팅 ID가 없으면 보내지 않는다(주소는 멀쩡해도 갈 곳이 없다).
	p.Channel = ""
	if err := n.post(context.Background(), p); err == nil {
		t.Error("채팅 ID 없이 보냈습니다")
	}
}

// 봇 토큰은 사람이 받는 그대로 넣는다. 주소를 만드는 것은 우리 몫이다.
func TestTelegramEndpoint(t *testing.T) {
	got := telegramAPI("123456789:AAH1234")
	want := "https://api.telegram.org/bot123456789:AAH1234/sendMessage"
	if got != want {
		t.Errorf("주소 = %q, 기대값 %q", got, want)
	}
	// 이미 완성된 주소를 넣은 사람도 있다. 그대로 쓴다.
	full := "https://api.telegram.org/bot1:2/sendMessage"
	if got := telegramAPI(full); got != full {
		t.Errorf("완성된 주소 = %q", got)
	}
}

// 채팅 ID가 없으면 보내지 않는다. 켜 두고도 아무 데도 가지 않는 상태가 가장 나쁘다 —
// 화면은 "켜짐"이라고 말하기 때문이다.
func TestTelegramNeedsChatID(t *testing.T) {
	cfg := store.NotifySettings{
		Enabled: true, Provider: store.ProviderTelegram,
		WebhookURL: "123456789:AAH1234567890abcdefghij",
	}
	if cfg.Active() {
		t.Error("채팅 ID가 없는데 보내는 상태입니다")
	}
	n := New(nil, nil)
	if err := n.Test(context.Background(), cfg); err == nil {
		t.Error("채팅 ID 없이 테스트 전송이 성공했습니다")
	}
}

// TestDiscordClampsContent는 상한(2000자)을 넘는 본문을 자르는지 본다.
// 넘겨 보내면 디스코드가 400으로 거부하고, 그러면 알림이 아예 오지 않는다.
func TestDiscordClampsContent(t *testing.T) {
	cfg := store.NotifySettings{
		Enabled: true, Provider: store.ProviderDiscord,
		WebhookURL: "https://discord.com/api/webhooks/1/abc",
	}
	ev := &store.Event{
		Kind: store.EventThreshold, Severity: store.SeverityWarning,
		Message: strings.Repeat("가", 3000),
	}
	wire := buildPayload(&cfg, ev, "shop", false).forWire()
	content, _ := wire["content"].(string)
	if n := len([]rune(content)); n != 2000 {
		t.Errorf("content 길이 = %d (2000이어야 합니다)", n)
	}
	if !strings.HasSuffix(content, "…") {
		t.Error("잘렸다는 표시가 없습니다")
	}
}

// TestDiscordPostSucceedsOn204는 디스코드의 성공 응답을 성공으로 보는지 본다.
//
// 디스코드는 웹후크가 성공하면 **204 No Content** 로 답하고 본문이 없다. 200과 본문을
// 기대하는 코드였다면 성공한 전송이 실패로 기록되고, 설정 화면의 전송 상태가 거짓이 된다.
func TestDiscordPostSucceedsOn204(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		// 디스코드는 content 도 embeds 도 없으면 400을 준다.
		if got["content"] == nil && got["embeds"] == nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	cfg := store.NotifySettings{
		Enabled: true, Provider: store.ProviderDiscord,
		WebhookURL: srv.URL + "/api/webhooks/1/abc", Username: "DB Studio",
	}
	ev := &store.Event{Kind: store.EventHost, Severity: store.SeverityWarning,
		Message: "메모리 사용률이 91%입니다"}
	n := New(nil, nil)
	if err := n.post(context.Background(), buildPayload(&cfg, ev, "", false)); err != nil {
		t.Fatalf("204를 실패로 보았습니다: %v", err)
	}
	if _, ok := got["embeds"]; !ok {
		t.Errorf("embeds 가 실려 나가지 않았습니다: %#v", got)
	}
}

func TestDiscordIsValidProvider(t *testing.T) {
	if !store.ValidProvider(store.ProviderDiscord) {
		t.Error("discord 가 아는 메신저 목록에 없습니다")
	}
}

func TestProviderDefaultsToMattermost(t *testing.T) {
	// 이 값이 생기기 전에 저장된 설정은 provider가 비어 있다. 그것을 알 수 없는
	// 메신저로 보면 이미 쓰던 알림이 조용히 멈춘다.
	var cfg store.NotifySettings
	if cfg.Kind() != store.ProviderMattermost {
		t.Errorf("기본 메신저 = %q", cfg.Kind())
	}
	cfg.Provider = "nosuch"
	if cfg.Kind() != store.ProviderMattermost {
		t.Errorf("모르는 값일 때 = %q", cfg.Kind())
	}
}
