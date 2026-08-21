package macro

import (
	"context"
	"net"
	"net/url"
	"sync"
	"testing"
	"time"

	"dbstudio/internal/store"
)

func TestMatchesEvent(t *testing.T) {
	// 빈 조건은 "전부"다. 이 규칙이 뒤집히면 아무 조건도 걸지 않은 트리거가
	// 아무 이벤트에도 반응하지 않게 되고, 사용자는 "만들었는데 안 돈다"만 본다.
	ev := &store.Event{
		Kind:     store.EventThreshold,
		Severity: store.SeverityWarning,
		Metric:   "connections.total",
	}

	cases := []struct {
		name    string
		trigger store.MacroTrigger
		want    bool
	}{
		{"조건 없음", store.MacroTrigger{}, true},
		{"종류 일치", store.MacroTrigger{EventKind: store.EventThreshold}, true},
		{"종류 불일치", store.MacroTrigger{EventKind: store.EventDrift}, false},
		{"지표 일치", store.MacroTrigger{EventMetric: "connections.total"}, true},
		{"지표 불일치", store.MacroTrigger{EventMetric: "disk.free"}, false},
		{"심각도 이하 — 통과", store.MacroTrigger{EventSeverity: string(store.SeverityInfo)}, true},
		{"심각도 같음 — 통과", store.MacroTrigger{EventSeverity: string(store.SeverityWarning)}, true},
		{"심각도 초과 — 차단", store.MacroTrigger{EventSeverity: string(store.SeverityCritical)}, false},
		{"모두 일치", store.MacroTrigger{
			EventKind: store.EventThreshold, EventMetric: "connections.total",
			EventSeverity: string(store.SeverityWarning),
		}, true},
		{"하나만 어긋나도 차단", store.MacroTrigger{
			EventKind: store.EventThreshold, EventMetric: "disk.free",
		}, false},
	}

	for _, tc := range cases {
		if got := matchesEvent(&tc.trigger, ev); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestAllowEventFireDebounces(t *testing.T) {
	s := &Scheduler{lastEventFire: map[string]time.Time{}}
	now := time.Date(2024, 3, 15, 10, 0, 0, 0, time.UTC)

	if !s.allowEventFire("t1", 300, now) {
		t.Fatal("첫 발화는 통과해야 한다")
	}
	if s.allowEventFire("t1", 300, now.Add(time.Minute)) {
		t.Error("최소 간격 안인데 통과했다")
	}
	if !s.allowEventFire("t1", 300, now.Add(6*time.Minute)) {
		t.Error("최소 간격이 지났는데 막혔다")
	}
	// 디바운스는 트리거별이다. 하나가 억제되었다고 다른 트리거까지 멈추면
	// 서로 무관한 자동화가 조용히 묶인다.
	if !s.allowEventFire("t2", 300, now.Add(6*time.Minute)) {
		t.Error("다른 트리거가 함께 억제되었다")
	}
}

func TestAllowEventFireIsRaceFree(t *testing.T) {
	// 모니터는 이벤트마다 고루틴으로 알린다. 확인과 기록이 한 잠금 안에 없으면
	// 같은 순간 들어온 이벤트들이 모두 "마지막 발화 없음"을 보고 통과한다.
	s := &Scheduler{lastEventFire: map[string]time.Time{}}
	now := time.Now()

	var wg sync.WaitGroup
	var mu sync.Mutex
	passed := 0
	for range 50 {
		wg.Go(func() {
			if s.allowEventFire("t1", 300, now) {
				mu.Lock()
				passed++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if passed != 1 {
		t.Errorf("동시 발화 중 %d개가 통과했다 (1이어야 한다)", passed)
	}
}

func TestTriggerContext(t *testing.T) {
	// 이벤트로 시작한 매크로는 "무엇 때문에 시작했는지" 알아야 조치를 취할 수 있다.
	// 이 값들이 Lua의 trigger 변수로 들어간다.
	tr := &store.MacroTrigger{ID: "tr1", Name: "야간 정리", Kind: store.TriggerSchedule}
	ctxVals := triggerContext(tr, nil)
	if ctxVals["kind"] != store.TriggerSchedule || ctxVals["name"] != "야간 정리" {
		t.Fatalf("스케줄 문맥이 다르다: %v", ctxVals)
	}
	if _, ok := ctxVals["event"]; ok {
		t.Error("스케줄 트리거에 event가 들어 있다")
	}

	ev := &store.Event{
		ID: 42, Kind: store.EventThreshold, Severity: store.SeverityCritical,
		Metric: "connections.total", ConnectionID: "conn1", Message: "커넥션 과다",
	}
	tr.Kind = store.TriggerEvent
	ctxVals = triggerContext(tr, ev)
	evMap, ok := ctxVals["event"].(map[string]any)
	if !ok {
		t.Fatalf("event 문맥이 없다: %v", ctxVals)
	}
	if evMap["metric"] != "connections.total" || evMap["severity"] != string(store.SeverityCritical) {
		t.Errorf("이벤트 문맥이 다르다: %v", evMap)
	}
	if evMap["connectionId"] != "conn1" {
		t.Errorf("커넥션 ID가 빠졌다: %v", evMap)
	}
}

// ---------- HTTP 대상 검사 ----------

func TestMatchHTTPRule(t *testing.T) {
	addrs := []net.IPAddr{
		{IP: net.ParseIP("203.0.113.10")},
		{IP: net.ParseIP("10.1.2.3")},
	}

	cases := []struct {
		rule string
		host string
		want bool
		note string
	}{
		{"api.slack.com", "api.slack.com", true, "이름 그대로"},
		{"API.SLACK.COM", "api.slack.com", true, "대소문자 무시"},
		{"api.slack.com", "evil.com", false, "다른 이름"},
		{".slack.com", "hooks.slack.com", true, "앞점은 하위 도메인 전체"},
		{".slack.com", "notslack.com", false, "하위 도메인이 아니다"},
		{"203.0.113.10", "any.host", true, "해석된 IP와 일치"},
		{"203.0.113.11", "any.host", false, "IP 불일치"},
		{"10.0.0.0/8", "any.host", true, "CIDR 안"},
		{"192.168.0.0/16", "any.host", false, "CIDR 밖"},
		{"", "any.host", false, "빈 규칙은 아무것도 허용하지 않는다"},
		{"   ", "any.host", false, "공백만 있는 규칙"},
	}

	for _, tc := range cases {
		if got := matchHTTPRule(tc.rule, tc.host, addrs); got != tc.want {
			t.Errorf("matchHTTPRule(%q, %q) = %v, want %v (%s)",
				tc.rule, tc.host, got, tc.want, tc.note)
		}
	}
}

func TestCheckHTTPTargetBlocksLinkLocal(t *testing.T) {
	// 169.254.169.254는 클라우드 메타데이터 엔드포인트다. 부를 수 있으면
	// 인스턴스 자격증명이 그대로 나간다 — 허용 목록과 무관하게 항상 막아야 한다.
	u := mustURL(t, "http://169.254.169.254/latest/meta-data/")
	err := checkHTTPTarget(context.Background(), u, HTTPConfig{})
	if err == nil {
		t.Fatal("링크로컬 주소가 통과했다")
	}

	// 허용 목록에 명시해도 막혀야 한다.
	err = checkHTTPTarget(context.Background(), u,
		HTTPConfig{Allow: []string{"169.254.169.254"}})
	if err == nil {
		t.Fatal("허용 목록에 넣었더니 링크로컬이 통과했다")
	}
}

func TestCheckHTTPTargetAllowList(t *testing.T) {
	// 127.0.0.1은 DNS 없이 해석되므로 허용 목록 판정만 시험할 수 있다.
	u := mustURL(t, "http://127.0.0.1:9999/hook")

	if err := checkHTTPTarget(context.Background(), u, HTTPConfig{}); err != nil {
		t.Errorf("목록이 비면 통과해야 한다: %v", err)
	}
	if err := checkHTTPTarget(context.Background(), u,
		HTTPConfig{Allow: []string{"127.0.0.1"}}); err != nil {
		t.Errorf("허용 목록에 있는데 막혔다: %v", err)
	}
	if err := checkHTTPTarget(context.Background(), u,
		HTTPConfig{Allow: []string{"127.0.0.0/8"}}); err != nil {
		t.Errorf("CIDR 허용인데 막혔다: %v", err)
	}
	if err := checkHTTPTarget(context.Background(), u,
		HTTPConfig{Allow: []string{"api.slack.com"}}); err == nil {
		t.Error("허용 목록 밖인데 통과했다")
	}
}

func TestCheckHTTPTargetNeedsHost(t *testing.T) {
	if err := checkHTTPTarget(context.Background(), mustURL(t, "http:///path"),
		HTTPConfig{}); err == nil {
		t.Error("호스트가 없는데 통과했다")
	}
}

func TestHTTPResultValueParsesJSON(t *testing.T) {
	res := &HTTPResult{Status: 200, Body: `{"ok":true,"count":3}`}
	out := httpResultValue(res)
	if out["ok"] != true {
		t.Errorf("2xx인데 ok가 아니다: %v", out["ok"])
	}
	parsed, isMap := out["json"].(map[string]any)
	if !isMap {
		t.Fatalf("JSON 본문이 파싱되지 않았다: %v", out["json"])
	}
	if parsed["count"] != float64(3) {
		t.Errorf("파싱 결과가 다르다: %v", parsed)
	}

	// 잘린 본문은 파싱하지 않는다. 반쪽짜리 JSON에서 나온 값은 조용히 틀린 값이다.
	truncatedJSON := &HTTPResult{Status: 200, Body: `{"ok":true`, Truncated: true}
	if _, has := httpResultValue(truncatedJSON)["json"]; has {
		t.Error("잘린 본문을 파싱했다")
	}

	// 4xx/5xx는 ok가 아니다 — Lua 쪽에서 status를 일일이 비교하지 않아도 되게.
	if httpResultValue(&HTTPResult{Status: 500})["ok"] != false {
		t.Error("500인데 ok가 참이다")
	}
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("URL 파싱 실패 %q: %v", raw, err)
	}
	return u
}
