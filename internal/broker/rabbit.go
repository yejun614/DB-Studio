package broker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"dbstudio/internal/opsapi"
)

// RabbitMQ 클라이언트.
//
// 관리 플러그인(rabbitmq_management)의 HTTP API를 쓴다. AMQP로 직접 붙지 않는 이유:
// AMQP에는 "큐 목록을 보여 달라"는 요청이 없다. 큐를 선언해 보면 존재 여부는 알 수 있지만
// 그것은 조회가 아니라 **변경**이다(없으면 만들어진다). 관리 API는 읽기 전용으로 전체를 준다.

// RabbitDefaultPort는 관리 API 포트다.
const RabbitDefaultPort = 15672

// Rabbit은 RabbitMQ 클러스터 클라이언트다.
type Rabbit struct {
	cfg    Config
	client *http.Client
}

func NewRabbit(cfg Config) *Rabbit {
	return &Rabbit{cfg: cfg, client: cfg.HTTPClient()}
}

func (r *Rabbit) Kind() string { return KindRabbitMQ }

func (r *Rabbit) get(ctx context.Context, path string, query url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		opsapi.JoinURL(r.cfg.BaseURL(), path, query), nil)
	if err != nil {
		return err
	}
	req.SetBasicAuth(r.cfg.User, r.cfg.Password)
	req.Header.Set("Accept", "application/json")
	return rabbitError(opsapi.DoJSON(ctx, r.client, req, out))
}

func (r *Rabbit) do(ctx context.Context, method, path string) error {
	req, err := http.NewRequestWithContext(ctx, method, r.cfg.BaseURL()+path, nil)
	if err != nil {
		return err
	}
	req.SetBasicAuth(r.cfg.User, r.cfg.Password)
	return rabbitError(opsapi.DoJSON(ctx, r.client, req, nil))
}

// overviewResponse는 /api/overview 의 필요한 부분이다.
type overviewResponse struct {
	RabbitMQVersion string `json:"rabbitmq_version"`
	ProductName     string `json:"product_name"`
	ClusterName     string `json:"cluster_name"`
	QueueTotals     struct {
		Messages               int64 `json:"messages"`
		MessagesReady          int64 `json:"messages_ready"`
		MessagesUnacknowledged int64 `json:"messages_unacknowledged"`
	} `json:"queue_totals"`
	ObjectTotals struct {
		Queues      int64 `json:"queues"`
		Exchanges   int64 `json:"exchanges"`
		Connections int64 `json:"connections"`
		Channels    int64 `json:"channels"`
		Consumers   int64 `json:"consumers"`
	} `json:"object_totals"`
	MessageStats struct {
		PublishDetails    rateDetail `json:"publish_details"`
		DeliverGetDetails rateDetail `json:"deliver_get_details"`
		AckDetails        rateDetail `json:"ack_details"`
		RedeliverDetails  rateDetail `json:"redeliver_details"`
	} `json:"message_stats"`
}

type rateDetail struct {
	Rate float64 `json:"rate"`
}

type rabbitNode struct {
	Name          string   `json:"name"`
	Running       bool     `json:"running"`
	MemUsed       int64    `json:"mem_used"`
	MemLimit      int64    `json:"mem_limit"`
	MemAlarm      bool     `json:"mem_alarm"`
	DiskFree      int64    `json:"disk_free"`
	DiskFreeLimit int64    `json:"disk_free_limit"`
	DiskFreeAlarm bool     `json:"disk_free_alarm"`
	FDUsed        int64    `json:"fd_used"`
	FDTotal       int64    `json:"fd_total"`
	SocketsUsed   int64    `json:"sockets_used"`
	Uptime        int64    `json:"uptime"`
	Partitions    []string `json:"partitions"`
	Type          string   `json:"type"`
}

type rabbitQueue struct {
	Name                   string  `json:"name"`
	VHost                  string  `json:"vhost"`
	State                  string  `json:"state"`
	Messages               int64   `json:"messages"`
	MessagesReady          int64   `json:"messages_ready"`
	MessagesUnacknowledged int64   `json:"messages_unacknowledged"`
	Consumers              int64   `json:"consumers"`
	Memory                 int64   `json:"memory"`
	Node                   string  `json:"node"`
	Durable                bool    `json:"durable"`
	IdleSince              string  `json:"idle_since"`
	MessageStats           struct {
		PublishDetails    rateDetail `json:"publish_details"`
		DeliverGetDetails rateDetail `json:"deliver_get_details"`
	} `json:"message_stats"`
}

func (q rabbitQueue) toQueue() Queue {
	out := Queue{
		Name: q.Name, VHost: q.VHost, State: q.State,
		Ready: q.MessagesReady, Unacked: q.MessagesUnacknowledged, Total: q.Messages,
		Consumers: q.Consumers, Memory: q.Memory, Node: q.Node, Durable: q.Durable,
		PublishRate: q.MessageStats.PublishDetails.Rate,
		DeliverRate: q.MessageStats.DeliverGetDetails.Rate,
		IdleSince:   q.IdleSince,
	}
	// 소비자가 없는데 메시지가 쌓여 있는 상태. 큐 목록에서 이것만 따로 보이게 한다.
	out.Starved = q.Consumers == 0 && q.MessagesReady > 0
	return out
}

// Ping은 접속과 인증을 확인하고 버전을 돌려준다.
func (r *Rabbit) Ping(ctx context.Context) (string, error) {
	var ov overviewResponse
	if err := r.get(ctx, "/api/overview", nil, &ov); err != nil {
		return "", err
	}
	version := ov.RabbitMQVersion
	if version != "" && ov.ProductName != "" {
		version = ov.ProductName + " " + version
	}
	return version, nil
}

// Overview는 클러스터 개요다.
func (r *Rabbit) Overview(ctx context.Context) (*Overview, error) {
	var ov overviewResponse
	if err := r.get(ctx, "/api/overview", nil, &ov); err != nil {
		return nil, err
	}
	out := &Overview{
		Kind: KindRabbitMQ, Version: ov.RabbitMQVersion,
		Backlog:   ov.QueueTotals.Messages,
		Consumers: ov.ObjectTotals.Consumers,
	}

	nodes, err := r.Nodes(ctx)
	if err != nil {
		out.Notes = append(out.Notes, "노드 상태를 읽지 못했습니다: "+err.Error())
	}
	queues, qErr := r.Queues(ctx, "", 0)
	if qErr != nil {
		out.Notes = append(out.Notes, "큐 목록을 읽지 못했습니다: "+qErr.Error())
	}

	var down, alarms int
	var split []string
	for _, n := range nodes {
		if !n.Running {
			down++
		}
		alarms += len(n.Alarms)
		if len(n.Partitions) > 0 {
			split = append(split, n.Name)
		}
	}
	starved := 0
	for _, q := range queues {
		if q.Starved {
			starved++
		}
	}

	out.Facts = []Fact{
		{Label: "큐", Value: fmt.Sprintf("%d개", ov.ObjectTotals.Queues)},
		{Label: "대기 메시지", Value: opsapi.HumanCount(ov.QueueTotals.MessagesReady),
			Level: opsapi.LevelIf(ov.QueueTotals.MessagesReady > 0 && ov.ObjectTotals.Consumers == 0, "warn")},
		{Label: "미확인 메시지", Value: opsapi.HumanCount(ov.QueueTotals.MessagesUnacknowledged)},
		{Label: "소비자", Value: fmt.Sprintf("%d개", ov.ObjectTotals.Consumers),
			Level: opsapi.LevelIf(ov.ObjectTotals.Consumers == 0 && ov.QueueTotals.Messages > 0, "warn")},
		{Label: "연결 · 채널", Value: fmt.Sprintf("%d · %d",
			ov.ObjectTotals.Connections, ov.ObjectTotals.Channels)},
		{Label: "초당 발행 · 소비", Value: fmt.Sprintf("%.1f · %.1f",
			ov.MessageStats.PublishDetails.Rate, ov.MessageStats.DeliverGetDetails.Rate)},
		{Label: "노드", Value: fmt.Sprintf("%d대 중 %d대 정상", len(nodes), len(nodes)-down),
			Level: opsapi.LevelIf(down > 0, "error")},
	}
	if starved > 0 {
		out.Facts = append(out.Facts, Fact{
			Label: "소비자 없는 큐", Value: fmt.Sprintf("%d개", starved), Level: "warn"})
	}

	// 상태 등급. 나쁜 순서대로 본다 — 분단은 데이터가 갈라지는 중이라는 뜻이므로 가장 위다.
	switch {
	case len(split) > 0:
		out.Health = Health{Level: HealthError,
			Summary: "네트워크 분단 (클러스터가 갈라졌습니다)", Checks: split}
	case down > 0:
		out.Health = Health{Level: HealthError,
			Summary: fmt.Sprintf("노드 %d대가 멈춰 있습니다", down)}
	case alarms > 0:
		out.Health = Health{Level: HealthError,
			Summary: "메모리·디스크 알람 (발행이 차단됩니다)", Checks: alarmList(nodes)}
	case starved > 0:
		out.Health = Health{Level: HealthWarn,
			Summary: fmt.Sprintf("소비자 없는 큐 %d개에 메시지가 쌓이고 있습니다", starved)}
	default:
		out.Health = Health{Level: HealthOK, Summary: "정상"}
	}
	return out, nil
}

func alarmList(nodes []NodeInfo) []string {
	var out []string
	for _, n := range nodes {
		for _, a := range n.Alarms {
			out = append(out, n.Name+": "+a)
		}
	}
	return out
}

// Nodes는 클러스터 노드 목록이다.
func (r *Rabbit) Nodes(ctx context.Context) ([]NodeInfo, error) {
	var raw []rabbitNode
	if err := r.get(ctx, "/api/nodes", nil, &raw); err != nil {
		return nil, err
	}
	out := make([]NodeInfo, 0, len(raw))
	for _, n := range raw {
		info := NodeInfo{
			Name: n.Name, Running: n.Running, MemUsed: n.MemUsed, MemLimit: n.MemLimit,
			DiskFree: n.DiskFree, DiskLimit: n.DiskFreeLimit,
			FDUsed: n.FDUsed, FDTotal: n.FDTotal, Uptime: n.Uptime, Partitions: n.Partitions,
		}
		// 알람은 "왜 발행이 막혔는가"의 답이다. 값(사용량)만 보여주면 임계선을 모르는
		// 사람은 그 숫자가 큰지 작은지 판단할 수 없다.
		if n.MemAlarm {
			info.Alarms = append(info.Alarms,
				fmt.Sprintf("메모리 알람 (%s / %s)",
					opsapi.HumanBytes(n.MemUsed), opsapi.HumanBytes(n.MemLimit)))
		}
		if n.DiskFreeAlarm {
			info.Alarms = append(info.Alarms,
				fmt.Sprintf("디스크 알람 (남은 %s, 한계 %s)",
					opsapi.HumanBytes(n.DiskFree), opsapi.HumanBytes(n.DiskFreeLimit)))
		}
		out = append(out, info)
	}
	return out, nil
}

// Queues는 큐 목록이다. vhost가 비어 있으면 전체를 본다.
//
// 정렬을 서버에 맡기지 않는 이유: 관리 API의 정렬 파라미터는 버전마다 지원 범위가 다르고,
// 목록이 짧을 때는 굳이 서버에 맡길 이유가 없다. 화면이 원하는 순서는 언제나 같다 —
// 문제가 있는 큐(소비자 없이 쌓인 것)가 위, 그다음이 많이 쌓인 순이다.
func (r *Rabbit) Queues(ctx context.Context, vhost string, limit int) ([]Queue, error) {
	path := "/api/queues"
	if v := strings.TrimSpace(vhost); v != "" {
		path += "/" + url.PathEscape(v)
	}
	var raw []rabbitQueue
	if err := r.get(ctx, path, nil, &raw); err != nil {
		return nil, err
	}
	out := make([]Queue, 0, len(raw))
	for _, q := range raw {
		out = append(out, q.toQueue())
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Starved != out[j].Starved {
			return out[i].Starved
		}
		return out[i].Total > out[j].Total
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// Exchanges는 익스체인지 목록이다.
func (r *Rabbit) Exchanges(ctx context.Context) ([]Exchange, error) {
	var raw []struct {
		Name         string `json:"name"`
		VHost        string `json:"vhost"`
		Type         string `json:"type"`
		Durable      bool   `json:"durable"`
		MessageStats struct {
			PublishInDetails  rateDetail `json:"publish_in_details"`
			PublishOutDetails rateDetail `json:"publish_out_details"`
		} `json:"message_stats"`
	}
	if err := r.get(ctx, "/api/exchanges", nil, &raw); err != nil {
		return nil, err
	}
	out := make([]Exchange, 0, len(raw))
	for _, e := range raw {
		name := e.Name
		if name == "" {
			// 기본 익스체인지는 이름이 빈 문자열이다. 그대로 두면 목록에 빈 줄로 보인다.
			name = "(기본)"
		}
		out = append(out, Exchange{
			Name: name, VHost: e.VHost, Type: e.Type, Durable: e.Durable,
			InRate: e.MessageStats.PublishInDetails.Rate,
			OutRate: e.MessageStats.PublishOutDetails.Rate,
		})
	}
	return out, nil
}

// Connections는 붙어 있는 클라이언트 목록이다.
func (r *Rabbit) Connections(ctx context.Context) ([]Conn, error) {
	var raw []struct {
		Name      string `json:"name"`
		User      string `json:"user"`
		VHost     string `json:"vhost"`
		State     string `json:"state"`
		Channels  int    `json:"channels"`
		PeerHost  string `json:"peer_host"`
		PeerPort  int    `json:"peer_port"`
		Protocol  string `json:"protocol"`
		ConnectedAt int64 `json:"connected_at"`
	}
	if err := r.get(ctx, "/api/connections", nil, &raw); err != nil {
		return nil, err
	}
	out := make([]Conn, 0, len(raw))
	for _, c := range raw {
		conn := Conn{
			Name: c.Name, User: c.User, VHost: c.VHost, State: c.State,
			Channels: c.Channels, Protocol: c.Protocol,
			Peer: fmt.Sprintf("%s:%d", c.PeerHost, c.PeerPort),
		}
		if c.ConnectedAt > 0 {
			conn.ConnectedAt = time.UnixMilli(c.ConnectedAt).UTC()
		}
		out = append(out, conn)
	}
	return out, nil
}

// PurgeQueue는 큐의 메시지를 모두 버린다.
//
// 되돌릴 수 없다. 그래서 화면은 지우기 전에 몇 개가 사라지는지 보여주고, 이 함수는
// 큐 이름을 정확히 받는다(패턴이나 "전체 비우기"는 만들지 않는다 — 한 번의 실수로
// 여러 큐를 비우는 길을 열지 않기 위해서다).
func (r *Rabbit) PurgeQueue(ctx context.Context, vhost, name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("큐 이름이 필요합니다")
	}
	return r.do(ctx, http.MethodDelete,
		fmt.Sprintf("/api/queues/%s/%s/contents", url.PathEscape(orDefault(vhost)), url.PathEscape(name)))
}

// DeleteQueue는 큐를 지운다.
func (r *Rabbit) DeleteQueue(ctx context.Context, vhost, name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("큐 이름이 필요합니다")
	}
	return r.do(ctx, http.MethodDelete,
		fmt.Sprintf("/api/queues/%s/%s", url.PathEscape(orDefault(vhost)), url.PathEscape(name)))
}

// CloseConnection은 클라이언트 연결을 끊는다.
//
// 필요한 이유: 폭주하는 발행자 하나가 브로커의 메모리 알람을 켜면 클러스터 전체의 발행이
// 막힌다. 그때 그 연결을 끊는 것이 가장 빠른 복구다.
func (r *Rabbit) CloseConnection(ctx context.Context, name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("연결 이름이 필요합니다")
	}
	return r.do(ctx, http.MethodDelete, "/api/connections/"+url.PathEscape(name))
}

func orDefault(vhost string) string {
	if strings.TrimSpace(vhost) == "" {
		return "/"
	}
	return vhost
}

// Collect는 지표 수집용 값이다.
func (r *Rabbit) Collect(ctx context.Context) (*Metrics, error) {
	ov, err := r.Overview(ctx)
	if err != nil {
		return nil, err
	}
	var raw overviewResponse
	if err := r.get(ctx, "/api/overview", nil, &raw); err != nil {
		return nil, err
	}
	nodes, _ := r.Nodes(ctx)
	queues, _ := r.Queues(ctx, "", 0)

	m := &Metrics{
		Health:      ov.Health,
		Backlog:     raw.QueueTotals.Messages,
		Unacked:     raw.QueueTotals.MessagesUnacknowledged,
		Consumers:   raw.ObjectTotals.Consumers,
		Queues:      raw.ObjectTotals.Queues,
		Nodes:       int64(len(nodes)),
		PublishRate: raw.MessageStats.PublishDetails.Rate,
		DeliverRate: raw.MessageStats.DeliverGetDetails.Rate,
	}
	for _, n := range nodes {
		if !n.Running {
			m.NodesDown++
		}
		m.Alarms += int64(len(n.Alarms))
	}
	for _, q := range queues {
		if q.Starved {
			m.Starved++
		}
	}
	return m, nil
}

// rabbitError는 관리 API의 오류를 사람이 읽을 말로 바꾼다.
func rabbitError(err error) error {
	var he *opsapi.HTTPError
	if err == nil || !asHTTPError(err, &he) {
		return err
	}
	var payload struct {
		Error  string `json:"error"`
		Reason string `json:"reason"`
	}
	_ = json.Unmarshal([]byte(he.Body), &payload)
	detail := strings.TrimSpace(payload.Reason)
	if detail == "" {
		detail = strings.TrimSpace(payload.Error)
	}
	if detail == "" {
		detail = opsapi.Snippet(he.Body)
	}
	switch he.Status {
	case http.StatusUnauthorized:
		return fmt.Errorf("인증이 거부됐습니다. 커넥션의 계정·비밀번호를 확인하세요: %s", detail)
	case http.StatusForbidden:
		return fmt.Errorf("이 계정에는 권한이 없습니다. 관리 권한(monitoring 이상)이 필요합니다: %s", detail)
	case http.StatusNotFound:
		// 가장 흔한 원인은 관리 플러그인이 꺼져 있는 것이다. 그 사실을 모르면
		// 포트나 주소를 계속 의심하게 된다.
		return fmt.Errorf("관리 API를 찾을 수 없습니다(%d). "+
			"rabbitmq_management 플러그인이 켜져 있는지, 포트가 관리 포트(기본 15672)인지 확인하세요: %s",
			he.Status, detail)
	}
	return fmt.Errorf("RabbitMQ 관리 API 오류(%d): %s", he.Status, detail)
}

// asHTTPError는 errors.As의 얇은 감싸기다(호출부를 짧게 유지한다).
func asHTTPError(err error, target **opsapi.HTTPError) bool {
	if err == nil {
		return false
	}
	return errors.As(err, target)
}
