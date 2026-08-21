package dbx

import (
	"context"
	"fmt"
	"strings"
	"time"

	"dbstudio/internal/broker"
	"dbstudio/internal/dblog"
	"dbstudio/internal/metric"
	"dbstudio/internal/model"
	"dbstudio/internal/schema"
)

// RabbitMQ·Kafka 어댑터.
//
// 스토리지 어댑터와 같은 이유로 여기에 등록한다: 커넥션 등록·자격증명 보관·접근 권한·
// 지표 수집·임계값·이벤트·알림은 이미 커넥션 단위로 돌아가고 있고, 브로커에도 그대로
// 필요하다. 스키마·SQL·마이그레이션 능력은 전부 꺼 둔다(Capabilities).
//
// 스토리지와 다른 점은 Capabilities.Broker가 참이라는 것뿐이다. 화면은 이 값을 보고
// DB 메뉴 대신 브로커 화면을 연다.

func init() {
	register(&rabbitAdapter{})
	register(&kafkaAdapter{})
}

// ---------- RabbitMQ ----------

type rabbitAdapter struct{}

func (a *rabbitAdapter) Kind() model.DBKind { return model.KindRabbitMQ }

func (a *rabbitAdapter) Capabilities() Capabilities {
	return Capabilities{Monitor: true, Broker: true}
}

func (a *rabbitAdapter) DefaultPort() int { return broker.RabbitDefaultPort }

func (a *rabbitAdapter) Validate(t Target) error {
	if t.Conn == nil {
		return fmt.Errorf("커넥션 정보가 없습니다")
	}
	cfg := broker.ConfigFrom(t.Conn, t.Secret, a.DefaultPort())
	if err := cfg.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(cfg.User) == "" {
		// 여기서 막는 이유: RabbitMQ 관리 API는 익명 접근이 없다. 계정 없이 저장하면
		// 등록은 되지만 모든 화면이 401로 끝나고, 그 원인이 설정에 있다는 것이
		// 오류 메시지에는 드러나지 않는다.
		return fmt.Errorf("RabbitMQ 관리 계정을 입력하세요")
	}
	return nil
}

func (a *rabbitAdapter) client(t Target) *broker.Rabbit {
	return broker.NewRabbit(broker.ConfigFrom(t.Conn, t.Secret, a.DefaultPort()))
}

func (a *rabbitAdapter) Ping(ctx context.Context, t Target) (*ServerInfo, error) {
	start := time.Now()
	version, err := a.client(t).Ping(ctx)
	if err != nil {
		return nil, err
	}
	info := &ServerInfo{Version: version, Latency: time.Since(start)}
	info.LatencyM = float64(info.Latency.Microseconds()) / 1000
	return info, nil
}

func (a *rabbitAdapter) Introspect(context.Context, Target) (*schema.Schema, error) {
	return nil, fmt.Errorf("%w: RabbitMQ에는 스키마가 없습니다", ErrNotImplemented)
}

func (a *rabbitAdapter) Logs(context.Context, Target, *dblog.Filter) (*dblog.Result, error) {
	return nil, fmt.Errorf("%w: RabbitMQ 로그 조회는 지원하지 않습니다", ErrNotImplemented)
}

func (a *rabbitAdapter) ExecDDL(context.Context, Target, []string, ExecOptions) (*ExecReport, error) {
	return nil, fmt.Errorf("%w: RabbitMQ에는 DDL이 없습니다", ErrNotImplemented)
}

func (a *rabbitAdapter) Redacted(t Target) string {
	return broker.ConfigFrom(t.Conn, t.Secret, a.DefaultPort()).BaseURL()
}

// Metrics는 RabbitMQ 지표를 수집한다.
//
// 접속 실패를 에러가 아니라 up=0으로 돌려주는 규칙은 다른 어댑터와 같다. 폴러는
// 그것을 근거로 접속 이벤트를 만들고, 에러로 올리면 "물어보지도 못했다"와 구분이
// 사라진다.
func (a *rabbitAdapter) Metrics(ctx context.Context, t Target) (*metric.Set, error) {
	set := metric.NewSet()
	start := time.Now()
	m, err := a.client(t).Collect(ctx)
	set.LatencyMs = float64(time.Since(start).Microseconds()) / 1000
	if err != nil {
		set.Gauge(metric.NameUp, 0, metric.UnitCount)
		set.Notes = append(set.Notes, err.Error())
		return set, nil
	}
	set.Gauge(metric.NameUp, 1, metric.UnitCount)
	set.Gauge(metric.NameLatency, set.LatencyMs, metric.UnitMillis)
	set.Gauge(metric.NameBrokerHealth, m.Health.Score(), metric.UnitCount)
	set.Gauge(metric.NameBrokerBacklog, float64(m.Backlog), metric.UnitCount)
	set.Gauge(metric.NameBrokerUnacked, float64(m.Unacked), metric.UnitCount)
	set.Gauge(metric.NameBrokerConsumers, float64(m.Consumers), metric.UnitCount)
	set.Gauge(metric.NameBrokerStarved, float64(m.Starved), metric.UnitCount)
	set.Gauge(metric.NameBrokerQueues, float64(m.Queues), metric.UnitCount)
	set.Gauge(metric.NameBrokerNodes, float64(m.Nodes), metric.UnitCount)
	set.Gauge(metric.NameBrokerNodesDown, float64(m.NodesDown), metric.UnitCount)
	set.Gauge(metric.NameBrokerPublish, m.PublishRate, metric.UnitPerSec)
	set.Gauge(metric.NameBrokerDeliver, m.DeliverRate, metric.UnitPerSec)
	set.Gauge(metric.NameBrokerAlarms, float64(m.Alarms), metric.UnitCount)
	return set, nil
}

// ---------- Kafka ----------

type kafkaAdapter struct{}

func (a *kafkaAdapter) Kind() model.DBKind { return model.KindKafka }

func (a *kafkaAdapter) Capabilities() Capabilities {
	return Capabilities{Monitor: true, Broker: true}
}

func (a *kafkaAdapter) DefaultPort() int { return broker.KafkaDefaultPort }

func (a *kafkaAdapter) Validate(t Target) error {
	if t.Conn == nil {
		return fmt.Errorf("커넥션 정보가 없습니다")
	}
	cfg := broker.ConfigFrom(t.Conn, t.Secret, a.DefaultPort())
	if err := cfg.Validate(); err != nil {
		return err
	}
	// SASL을 켰는데 계정이 없으면 인증이 반드시 실패한다. 여기서 막아
	// "왜 401이 나는지"를 등록 시점에 알려준다.
	if s := strings.ToLower(strings.TrimSpace(cfg.Opt("sasl", ""))); s != "" && s != "none" {
		if strings.TrimSpace(cfg.User) == "" {
			return fmt.Errorf("SASL 인증을 켰다면 계정을 입력하세요")
		}
	}
	return nil
}

func (a *kafkaAdapter) client(t Target) (*broker.Kafka, error) {
	return broker.NewKafka(broker.ConfigFrom(t.Conn, t.Secret, a.DefaultPort()))
}

func (a *kafkaAdapter) Ping(ctx context.Context, t Target) (*ServerInfo, error) {
	cl, err := a.client(t)
	if err != nil {
		return nil, err
	}
	defer cl.Close()
	start := time.Now()
	version, err := cl.Ping(ctx)
	if err != nil {
		return nil, err
	}
	info := &ServerInfo{Version: version, Latency: time.Since(start)}
	info.LatencyM = float64(info.Latency.Microseconds()) / 1000
	return info, nil
}

func (a *kafkaAdapter) Introspect(context.Context, Target) (*schema.Schema, error) {
	return nil, fmt.Errorf("%w: Kafka에는 스키마가 없습니다", ErrNotImplemented)
}

func (a *kafkaAdapter) Logs(context.Context, Target, *dblog.Filter) (*dblog.Result, error) {
	return nil, fmt.Errorf("%w: Kafka 로그 조회는 지원하지 않습니다", ErrNotImplemented)
}

func (a *kafkaAdapter) ExecDDL(context.Context, Target, []string, ExecOptions) (*ExecReport, error) {
	return nil, fmt.Errorf("%w: Kafka에는 DDL이 없습니다", ErrNotImplemented)
}

func (a *kafkaAdapter) Redacted(t Target) string {
	cfg := broker.ConfigFrom(t.Conn, t.Secret, a.DefaultPort())
	return fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
}

// Metrics는 Kafka 지표를 수집한다.
func (a *kafkaAdapter) Metrics(ctx context.Context, t Target) (*metric.Set, error) {
	cl, err := a.client(t)
	if err != nil {
		set := metric.NewSet()
		set.Gauge(metric.NameUp, 0, metric.UnitCount)
		set.Notes = append(set.Notes, err.Error())
		return set, nil
	}
	defer cl.Close()

	set := metric.NewSet()
	start := time.Now()
	m, err := cl.Collect(ctx)
	set.LatencyMs = float64(time.Since(start).Microseconds()) / 1000
	if err != nil {
		set.Gauge(metric.NameUp, 0, metric.UnitCount)
		set.Notes = append(set.Notes, err.Error())
		return set, nil
	}
	set.Gauge(metric.NameUp, 1, metric.UnitCount)
	set.Gauge(metric.NameLatency, set.LatencyMs, metric.UnitMillis)
	set.Gauge(metric.NameBrokerHealth, m.Health.Score(), metric.UnitCount)
	set.Gauge(metric.NameBrokerBacklog, float64(m.Backlog), metric.UnitCount)
	set.Gauge(metric.NameBrokerConsumers, float64(m.Consumers), metric.UnitCount)
	set.Gauge(metric.NameBrokerStarved, float64(m.Starved), metric.UnitCount)
	set.Gauge(metric.NameBrokerTopics, float64(m.Topics), metric.UnitCount)
	set.Gauge(metric.NameBrokerGroups, float64(m.Groups), metric.UnitCount)
	set.Gauge(metric.NameBrokerNodes, float64(m.Nodes), metric.UnitCount)
	set.Gauge(metric.NameBrokerMaxLag, float64(m.MaxLag), metric.UnitCount)
	set.Gauge(metric.NameBrokerOffline, float64(m.Offline), metric.UnitCount)
	set.Gauge(metric.NameBrokerUnderRep, float64(m.UnderReplicated), metric.UnitCount)
	return set, nil
}
