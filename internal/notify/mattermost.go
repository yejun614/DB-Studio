// Package notify는 모니터링 이벤트를 메신저(Mattermost)로 보낸다.
//
// 왜 필요한가: 이벤트는 화면을 열어야 보인다. 디스크가 차거나 운영 DB가 응답을 멈춘
// 사실은 **보고 있지 않을 때** 일어나고, 그때 사람에게 닿지 않으면 이벤트 목록은
// 사후 기록일 뿐이다.
//
// 들어오는 웹훅(incoming webhook)을 쓰는 이유: 봇 토큰과 API를 쓰면 채널 목록 조회·
// 권한 관리가 따라오는데, 우리가 필요한 것은 "정해진 채널에 글 한 줄"이다. 웹훅은
// 주소 하나가 곧 그 권한이고, 그래서 이 앱은 그 주소를 커넥션 비밀번호와 같은 급으로
// 다룬다(암호화 저장, 화면에는 마스킹).
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"dbstudio/internal/store"
)

// queueSize는 보낼 메시지를 담아 두는 칸 수다.
//
// 큐를 두는 이유: 전송은 네트워크 왕복이고, 폴러는 그 시간을 기다려서는 안 된다.
// 넉넉하지만 무한하지 않게 잡는다 — 메신저가 죽어 있을 때 메모리가 늘어나는 것보다
// "그 사이의 알림 몇 건을 잃었다"고 기록에 남기는 편이 낫다.
const queueSize = 256

// Notifier는 이벤트를 웹훅으로 보낸다. monitor.EventSink를 구현한다.
type Notifier struct {
	st     *store.Store
	client *http.Client
	log    *slog.Logger
	queue  chan message

	mu   sync.Mutex
	last Status
}

// Status는 마지막 전송 결과다. 설정 화면이 "지금 잘 가고 있는가"에 답하려면 필요하다.
//
// At을 포인터로 둔 이유: 아직 한 번도 보내지 않은 상태와 "실패한 상태"는 다른 이야기다.
// time.Time은 비어 있어도 omitempty가 걸리지 않아 0001-01-01이 그대로 나가고,
// 화면은 그것을 마지막 전송 시각으로 읽어 "1-01-01에 실패"라고 말한다.
type Status struct {
	At      *time.Time `json:"at,omitempty"`
	OK      bool       `json:"ok"`
	Detail  string     `json:"detail,omitempty"`
	Dropped int64      `json:"dropped,omitempty"`
}

func New(st *store.Store, log *slog.Logger) *Notifier {
	if log == nil {
		log = slog.Default()
	}
	return &Notifier{
		st:  st,
		log: log,
		// 타임아웃을 짧게 잡는다. 알림이 늦게 가는 것보다 큐가 막히는 것이 나쁘다.
		client: &http.Client{Timeout: 10 * time.Second},
		queue:  make(chan message, queueSize),
	}
}

// message는 큐에 담기는 한 건이다.
type message struct {
	payload payload
	kind    string
	sev     store.Severity
}

// Run은 큐를 비우는 루프다. ctx가 끝나면 반환한다.
func (n *Notifier) Run(ctx context.Context) {
	n.log.Info("알림 전송기 시작")
	for {
		select {
		case <-ctx.Done():
			n.log.Info("알림 전송기 종료")
			return
		case msg := <-n.queue:
			n.deliver(ctx, msg)
		}
	}
}

// EventOpened는 새 이벤트를 큐에 넣는다(monitor.EventSink).
func (n *Notifier) EventOpened(ctx context.Context, ev *store.Event) {
	n.enqueue(ctx, ev, false)
}

// EventResolved는 해소된 이벤트를 큐에 넣는다(monitor.ResolveSink).
func (n *Notifier) EventResolved(ctx context.Context, ev *store.Event) {
	n.enqueue(ctx, ev, true)
}

func (n *Notifier) enqueue(ctx context.Context, ev *store.Event, resolved bool) {
	if ev == nil {
		return
	}
	cfg, err := n.st.NotifySettings(ctx)
	if err != nil {
		n.log.Error("알림 설정을 읽지 못했습니다", "err", err)
		return
	}
	if !cfg.Allows(ev.Kind, ev.Severity) {
		return
	}
	if resolved && !cfg.IncludeResolved {
		return
	}

	name := ""
	if ev.ConnectionID != "" {
		if conn, err := n.st.GetConnection(ctx, ev.ConnectionID); err == nil {
			name = conn.Name
		}
	}
	msg := message{payload: buildPayload(cfg, ev, name, resolved), kind: ev.Kind, sev: ev.Severity}

	select {
	case n.queue <- msg:
	default:
		// 큐가 가득 찼다. 여기서 기다리면 폴러가 멈추므로 버리되, 버렸다는 사실을 남긴다.
		n.mu.Lock()
		n.last.Dropped++
		n.mu.Unlock()
		n.log.Warn("알림 큐가 가득 차 메시지를 버렸습니다", "event", ev.ID, "kind", ev.Kind)
	}
}

// Test는 설정 화면의 "테스트 전송"이다. 큐를 거치지 않고 곧바로 보내 결과를 돌려준다.
//
// 곧바로 보내는 이유: 테스트의 목적은 "지금 이 설정으로 닿는가"를 확인하는 것이다.
// 큐에 넣고 성공을 돌려주면 실패해도 화면은 성공이라고 말한다.
func (n *Notifier) Test(ctx context.Context, cfg store.NotifySettings) error {
	if !cfg.HasWebhook() {
		return fmt.Errorf("웹훅 주소가 비어 있습니다")
	}
	body := payload{
		URL:      strings.TrimSpace(cfg.WebhookURL),
		Channel:  strings.TrimSpace(cfg.Channel),
		Username: displayName(cfg),
		Text: ":white_check_mark: " +
			bold(cfg.Kind(), "DB Studio") + " 알림 연결을 확인했습니다.",
		Attachments: []attachment{{
			Color: colorInfo,
			Title: "테스트 메시지",
			Text: "이 메시지가 보이면 이벤트가 생겼을 때 같은 채널로 알림이 옵니다.\n" +
				"보낼 기준: " + describeFilter(cfg),
		}},
	}
	err := n.post(ctx, body)
	n.record(err)
	return err
}

// Status는 마지막 전송 결과다.
func (n *Notifier) Status() Status {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.last
}

func (n *Notifier) deliver(ctx context.Context, msg message) {
	// 한 번 실패했다고 바로 버리지 않는다. 메신저 재시작이나 순간적인 네트워크 단절은
	// 몇 초 뒤면 지나가고, 그 사이에 버려진 알림은 아무도 다시 보내주지 않는다.
	var err error
	for attempt := range 3 {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Duration(attempt) * 2 * time.Second):
			}
		}
		if err = n.post(ctx, msg.payload); err == nil {
			break
		}
	}
	n.record(err)
	if err != nil {
		n.log.Error("알림 전송 실패", "kind", msg.kind, "severity", msg.sev, "err", err)
	}
}

func (n *Notifier) record(err error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	now := time.Now()
	n.last.At = &now
	n.last.OK = err == nil
	if err != nil {
		n.last.Detail = err.Error()
		return
	}
	n.last.Detail = ""
}

// post는 웹훅으로 한 건을 보낸다.
func (n *Notifier) post(ctx context.Context, body payload) error {
	target, err := url.Parse(strings.TrimSpace(body.URL))
	if err != nil || target.Host == "" {
		return fmt.Errorf("웹훅 주소가 올바르지 않습니다")
	}
	if target.Scheme != "http" && target.Scheme != "https" {
		return fmt.Errorf("웹훅 주소는 http 또는 https여야 합니다")
	}
	if err := checkTarget(ctx, target); err != nil {
		return err
	}

	raw, err := json.Marshal(body.forWire())
	if err != nil {
		return fmt.Errorf("메시지를 만들지 못했습니다: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	res, err := n.client.Do(req)
	if err != nil {
		return fmt.Errorf("보내지 못했습니다: %w", err)
	}
	defer res.Body.Close()
	// 본문을 조금 읽는 이유: Mattermost는 실패 사유를 본문에 적는다.
	// 상태 코드만 남기면 "400"만 보이고 무엇이 틀렸는지는 알 수 없다.
	snippet, _ := io.ReadAll(io.LimitReader(res.Body, 512))
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("메신저가 %d 로 거부했습니다: %s",
			res.StatusCode, strings.TrimSpace(string(snippet)))
	}
	return nil
}

// checkTarget은 부를 수 있는 주소인지 본다.
//
// 사설망을 막지 않는 이유: Mattermost는 사내에 두는 경우가 훨씬 많다. 대신 링크로컬은
// 막는다 — 클라우드 메타데이터 서비스(169.254.169.254)가 거기 있고, 그것을 부를 수
// 있으면 잘못 적은 주소 하나가 인스턴스 자격증명을 밖으로 내보내는 통로가 된다.
func checkTarget(ctx context.Context, target *url.URL) error {
	host := target.Hostname()
	if host == "" {
		return fmt.Errorf("웹훅 주소에 호스트가 없습니다")
	}
	var resolver net.Resolver
	addrs, err := resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return fmt.Errorf("호스트 이름을 찾을 수 없습니다: %s", host)
	}
	for _, addr := range addrs {
		if addr.IP.IsLinkLocalUnicast() || addr.IP.IsLinkLocalMulticast() {
			return fmt.Errorf("링크로컬 주소로는 보낼 수 없습니다 (%s)", addr.IP)
		}
		if addr.IP.IsUnspecified() {
			return fmt.Errorf("보낼 수 없는 주소입니다 (%s)", addr.IP)
		}
	}
	return nil
}
