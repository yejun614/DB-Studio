package broker

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/sasl"
	"github.com/twmb/franz-go/pkg/sasl/plain"
	"github.com/twmb/franz-go/pkg/sasl/scram"

	"dbstudio/internal/opsapi"
)

// Kafka 클라이언트.
//
// 브로커에는 HTTP 관리 API가 없다. 카프카 프로토콜을 직접 말하는 순수 Go 클라이언트
// (franz-go)로 메타데이터·컨슈머 랙을 읽는다. REST 프록시를 요구하면 그것을 따로
// 세워야 하는 배포에서는 이 기능을 아예 쓸 수 없기 때문이다.
//
// 읽는 것:
//   - 메타데이터: 브로커 목록·토픽·파티션·리더·ISR. 클러스터의 뼈대가 여기 있다.
//   - 컨슈머 그룹과 랙: "쌓이고 있나"의 답. 브로커 운영에서 가장 중요한 숫자다.
//   - 토픽 설정: 보관 기간·정리 정책 등 눈에 띄어야 하는 것만 뽑는다.

// KafkaDefaultPort는 카프카 브로커 포트다.
const KafkaDefaultPort = 9092

// Kafka는 카프카 클러스터 클라이언트다.
type Kafka struct {
	cfg    Config
	client *kgo.Client
	admin  *kadm.Client
}

// NewKafka는 접속 설정으로 클라이언트를 만든다.
//
// 시드 브로커는 Host:Port 하나로 충분하다 — 카프카는 메타데이터를 받으면 나머지
// 브로커를 스스로 찾는다. Extra["brokers"]에 콤마로 구분한 추가 시드를 넣으면
// 첫 브로커가 죽어 있어도 다른 시드로 시작할 수 있다.
func NewKafka(cfg Config) (*Kafka, error) {
	seeds := []string{fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)}
	if extra := strings.TrimSpace(cfg.Opt("brokers", "")); extra != "" {
		for _, s := range strings.Split(extra, ",") {
			if s = strings.TrimSpace(s); s != "" {
				seeds = append(seeds, s)
			}
		}
	}

	opts := []kgo.Opt{
		kgo.SeedBrokers(seeds...),
		// 관리 화면이 물어보는 것은 전부 메타데이터·오프셋뿐이다. 프로듀서·컨슈머
		// 기능을 켜면 불필요한 연결과 재조정이 생긴다.
		kgo.DisableIdempotentWrite(),
		kgo.ConnIdleTimeout(30 * time.Second),
	}

	if tlsOn(cfg) {
		opts = append(opts, kgo.DialTLSConfig(&tls.Config{
			MinVersion:         tls.VersionTLS12,
			InsecureSkipVerify: cfg.Insecure,
		}))
	}
	if mech := saslMechanism(cfg); mech != nil {
		opts = append(opts, kgo.SASL(mech))
	}
	cl, err := kgo.NewClient(opts...)
	if err != nil {
		return nil, fmt.Errorf("카프카 클라이언트를 만들지 못했습니다: %w", err)
	}
	return &Kafka{cfg: cfg, client: cl, admin: kadm.NewClient(cl)}, nil
}

// tlsOn은 TLS 사용 여부다. 옵션 tls=true 또는 scheme=https일 때 켠다.
func tlsOn(cfg Config) bool {
	if b, err := strconv.ParseBool(cfg.Opt("tls", "")); err == nil {
		return b
	}
	return strings.EqualFold(cfg.Scheme, "https")
}

// saslMechanism은 옵션 sasl이 지정한 인증 방식을 만든다. 비어 있으면 nil(익명).
func saslMechanism(cfg Config) sasl.Mechanism {
	user, pass := cfg.User, cfg.Password
	switch strings.ToLower(strings.TrimSpace(cfg.Opt("sasl", ""))) {
	case "plain":
		return plain.Plain(func(context.Context) (plain.Auth, error) {
			return plain.Auth{User: user, Pass: pass}, nil
		})
	case "scram-sha-256":
		return scram.Sha256(func(context.Context) (scram.Auth, error) {
			return scram.Auth{User: user, Pass: pass}, nil
		})
	case "scram-sha-512":
		return scram.Sha512(func(context.Context) (scram.Auth, error) {
			return scram.Auth{User: user, Pass: pass}, nil
		})
	}
	return nil
}

// Close는 클라이언트 연결을 닫는다.
func (k *Kafka) Close() { k.client.Close() }

func (k *Kafka) Kind() string { return KindKafka }

// kafkaError는 카프카 프로토콜 오류를 사람이 읽을 말로 바꾼다.
//
// kerr의 오류는 코드만 담고 있어 그대로 보여주면 "SASL_AUTHENTICATION_FAILED"처럼
// 영어 코드가 화면에 나온다. 인증 실패는 설정 문제가 가장 흔하므로 그 안내를 함께 적는다.
func kafkaError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, kerr.SaslAuthenticationFailed) {
		return fmt.Errorf("인증이 거부됐습니다. 커넥션의 계정·비밀번호와 SASL 방식을 확인하세요: %w", err)
	}
	if errors.Is(err, kerr.UnsupportedSaslMechanism) {
		return fmt.Errorf("브로커가 이 SASL 방식을 지원하지 않습니다. plain/scram-sha-256/scram-sha-512 중 브로커가 쓰는 것을 고르세요: %w", err)
	}
	if errors.Is(err, kerr.TopicAuthorizationFailed) {
		return fmt.Errorf("이 계정에는 토픽을 볼 권한이 없습니다: %w", err)
	}
	if errors.Is(err, kerr.GroupAuthorizationFailed) {
		return fmt.Errorf("이 계정에는 컨슈머 그룹을 볼 권한이 없습니다: %w", err)
	}
	if errors.Is(err, kerr.ClusterAuthorizationFailed) {
		return fmt.Errorf("이 계정에는 클러스터 관리 권한이 없습니다: %w", err)
	}
	return err
}

// Ping은 접속·인증을 확인하고 버전을 돌려준다.
//
// 카프카는 버전을 직접 알려주지 않는다. 대신 ApiVersions 응답의 지원 범위로
// 추정한다(kversion). 정확한 패치 버전은 알 수 없지만, 운영 화면에 필요한 것은
// "몇 년대의 브로커인가"다.
func (k *Kafka) Ping(ctx context.Context) (string, error) {
	vs, err := k.admin.ApiVersions(ctx)
	if err != nil {
		return "", kafkaError(err)
	}
	for _, b := range vs.Sorted() {
		if b.Err == nil {
			return "Kafka " + b.VersionGuess(), nil
		}
	}
	return "", kafkaError(vs[0].Err)
}

// Overview는 클러스터 개요다.
func (k *Kafka) Overview(ctx context.Context) (*Overview, error) {
	version, err := k.Ping(ctx)
	if err != nil {
		return nil, err
	}
	out := &Overview{Kind: KindKafka, Version: version,
		Health: Health{Level: HealthUnknown, Summary: "확인하지 못했습니다"}}

	topics, err := k.admin.ListTopicsWithInternal(ctx)
	if err != nil {
		return nil, kafkaError(err)
	}
	groups, err := k.admin.ListGroups(ctx)
	if err != nil {
		return nil, kafkaError(err)
	}
	brokers, err := k.admin.ListBrokers(ctx)
	if err != nil {
		return nil, kafkaError(err)
	}

	// 랙은 그룹이 없으면 계산할 것이 없다. 그룹이 있을 때만 물어본다.
	var lags kadm.DescribedGroupLags
	if len(groups) > 0 {
		lags, err = k.admin.Lag(ctx, groups.Groups()...)
		if err != nil {
			out.Notes = append(out.Notes, "컨슈머 랙을 읽지 못했습니다: "+kafkaError(err).Error())
		}
	}

	var (
		partitions, underRep, offline int
		internalTopics                int
	)
	for _, t := range topics {
		if t.IsInternal {
			internalTopics++
		}
		for _, p := range t.Partitions {
			partitions++
			if p.Leader < 0 {
				offline++
			}
			if len(p.ISR) < len(p.Replicas) {
				underRep++
			}
		}
	}

	var totalLag, members, starved int64
	var maxLag int64 = -1
	for _, g := range lags {
		totalLag += g.Lag.Total()
		members += int64(len(g.Members))
		if g.Lag.Total() > maxLag {
			maxLag = g.Lag.Total()
		}
		// 메시지는 쌓이는데 멤버가 없는 그룹. RabbitMQ의 "소비자 없는 큐"와 같은 뜻이다.
		if g.Lag.Total() > 0 && len(g.Members) == 0 {
			starved++
		}
	}

	out.Backlog = totalLag
	out.Consumers = members
	out.Facts = []Fact{
		{Label: "브로커", Value: fmt.Sprintf("%d대", len(brokers))},
		{Label: "토픽", Value: fmt.Sprintf("%d개 (내부 %d개)", len(topics), internalTopics)},
		{Label: "파티션", Value: fmt.Sprintf("%d개", partitions)},
		{Label: "컨슈머 그룹", Value: fmt.Sprintf("%d개", len(groups))},
		{Label: "그룹 멤버", Value: fmt.Sprintf("%d명", members)},
		{Label: "총 랙", Value: opsapi.HumanCount(totalLag),
			Level: opsapi.LevelIf(totalLag > 0 && members == 0, "warn")},
		{Label: "최대 랙", Value: opsapi.HumanCount(maxLag)},
	}
	if underRep > 0 {
		out.Facts = append(out.Facts, Fact{Label: "복제본 부족 파티션",
			Value: fmt.Sprintf("%d개", underRep), Level: "warn"})
	}
	if offline > 0 {
		out.Facts = append(out.Facts, Fact{Label: "리더 없는 파티션",
			Value: fmt.Sprintf("%d개", offline), Level: "error"})
	}
	if starved > 0 {
		out.Facts = append(out.Facts, Fact{Label: "소비자 없는 그룹",
			Value: fmt.Sprintf("%d개", starved), Level: "warn"})
	}

	// 상태 등급. 리더 없는 파티션은 읽지도 쓰지도 못하므로 가장 위다.
	switch {
	case offline > 0:
		out.Health = Health{Level: HealthError,
			Summary: fmt.Sprintf("리더 없는 파티션 %d개 (읽기·쓰기가 막힙니다)", offline)}
	case underRep > 0:
		out.Health = Health{Level: HealthWarn,
			Summary: fmt.Sprintf("복제본이 부족한 파티션 %d개", underRep)}
	case starved > 0:
		out.Health = Health{Level: HealthWarn,
			Summary: fmt.Sprintf("소비자 없는 그룹 %d개에 메시지가 쌓이고 있습니다", starved)}
	default:
		out.Health = Health{Level: HealthOK, Summary: "정상"}
	}
	return out, nil
}

// Nodes는 브로커 목록이다.
func (k *Kafka) Nodes(ctx context.Context) ([]NodeInfo, error) {
	brokers, err := k.admin.ListBrokers(ctx)
	if err != nil {
		return nil, kafkaError(err)
	}
	// 컨트롤러는 메타데이터에서 안다. ListBrokers에는 없으므로 따로 읽는다.
	meta, err := k.admin.Metadata(ctx)
	if err != nil {
		return nil, kafkaError(err)
	}
	out := make([]NodeInfo, 0, len(brokers))
	for _, b := range brokers {
		out = append(out, NodeInfo{
			Name:       fmt.Sprintf("%s:%d", b.Host, b.Port),
			Running:    true,
			Host:       b.Host,
			Port:       int(b.Port),
			Controller: b.NodeID == meta.Controller,
		})
	}
	return out, nil
}

// Topics는 토픽 목록이다.
//
// 메시지 수는 (끝 오프셋 - 시작 오프셋)의 합이다. 정확한 개수가 아니라 보관 중인
// 양의 근사다 — 압축과 보관 기간 삭제 때문이다. 토픽이 많으면 오프셋 조회가
// 비싸지므로, 화면이 요청한 만큼만 계산한다.
func (k *Kafka) Topics(ctx context.Context, limit int) ([]Topic, error) {
	topics, err := k.admin.ListTopicsWithInternal(ctx)
	if err != nil {
		return nil, kafkaError(err)
	}
	names := topics.Names()
	sort.Strings(names)

	out := make([]Topic, 0, len(names))
	for _, name := range names {
		t := topics[name]
		topic := Topic{
			Name: name, Partitions: len(t.Partitions),
			Internal: t.IsInternal,
		}
		// 복제 수는 파티션마다 다를 수 있지만 보통 같다. 첫 파티션의 값을 쓴다.
		if len(t.Partitions) > 0 {
			topic.ReplicationFactor = len(t.Partitions[0].Replicas)
		}
		for _, p := range t.Partitions {
			if p.Leader < 0 {
				topic.Offline++
			}
			if len(p.ISR) < len(p.Replicas) {
				topic.UnderReplicated++
			}
		}
		out = append(out, topic)
	}

	// 메시지 수는 요청한 만큼만 계산한다. 목록이 길면 오프셋 조회가 지표 수집처럼
	// 비싸지므로, 화면이 보여줄 만큼만 값을 채운다.
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	if err := k.fillMessageCounts(ctx, out); err != nil {
		// 실패해도 목록을 막지 않는다. 권한이 없는 토픽이 섞여 있으면
		// 오프셋 조회가 부분 실패하기 때문이다.
		for i := range out {
			if out[i].Messages == 0 {
				out[i].Messages = -1
			}
		}
	}
	return out, nil
}

// fillMessageCounts는 토픽들의 보관 중인 메시지 수를 채운다.
func (k *Kafka) fillMessageCounts(ctx context.Context, topics []Topic) error {
	names := make([]string, 0, len(topics))
	for _, t := range topics {
		names = append(names, t.Name)
	}
	ends, err := k.admin.ListEndOffsets(ctx, names...)
	if err != nil {
		return kafkaError(err)
	}
	starts, err := k.admin.ListStartOffsets(ctx, names...)
	if err != nil {
		return kafkaError(err)
	}
	for i := range topics {
		var sum int64
		// ListedOffsets는 map[topic]map[partition]ListedOffset이다.
		endMap := ends[topics[i].Name]
		startMap := starts[topics[i].Name]
		for part, end := range endMap {
			start, ok := startMap[part]
			if !ok || end.Err != nil || start.Err != nil {
				continue
			}
			if end.Offset > start.Offset {
				sum += end.Offset - start.Offset
			}
		}
		topics[i].Messages = sum
	}
	return nil
}

// TopicConfig는 토픽 하나의 설정이다.
func (k *Kafka) TopicConfig(ctx context.Context, name string) (map[string]string, error) {
	rcs, err := k.admin.DescribeTopicConfigs(ctx, name)
	if err != nil {
		return nil, kafkaError(err)
	}
	rc, err := rcs.On(name, nil)
	if err != nil {
		return nil, kafkaError(err)
	}
	out := map[string]string{}
	for _, c := range rc.Configs {
		if c.Sensitive {
			continue
		}
		out[c.Key] = c.MaybeValue()
	}
	return out, nil
}

// Groups는 컨슈머 그룹 목록이다. 각 그룹의 랙을 함께 계산한다.
func (k *Kafka) Groups(ctx context.Context) ([]Group, error) {
	listed, err := k.admin.ListGroups(ctx)
	if err != nil {
		return nil, kafkaError(err)
	}
	names := listed.Groups()
	if len(names) == 0 {
		return []Group{}, nil
	}
	lags, err := k.admin.Lag(ctx, names...)
	if err != nil {
		return nil, kafkaError(err)
	}
	out := make([]Group, 0, len(lags))
	for _, g := range lags.Sorted() {
		group := Group{
			Name: g.Group, State: g.State,
			Members: len(g.Members), Protocol: g.Protocol,
			Lag: g.Lag.Total(),
		}
		// 토픽별 랙은 랙이 큰 순으로 정렬해 담는다. 화면은 이 순서를 그대로 보여준다.
		for _, tl := range g.Lag.TotalByTopic().Sorted() {
			group.Topics = append(group.Topics, GroupLag{Topic: tl.Topic, Lag: tl.Lag})
		}
		out = append(out, group)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Lag != out[j].Lag {
			return out[i].Lag > out[j].Lag
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// Collect는 지표 수집용 값이다.
func (k *Kafka) Collect(ctx context.Context) (*Metrics, error) {
	ov, err := k.Overview(ctx)
	if err != nil {
		return nil, err
	}
	topics, err := k.admin.ListTopicsWithInternal(ctx)
	if err != nil {
		return nil, kafkaError(err)
	}
	brokers, err := k.admin.ListBrokers(ctx)
	if err != nil {
		return nil, kafkaError(err)
	}
	groups, err := k.admin.ListGroups(ctx)
	if err != nil {
		return nil, kafkaError(err)
	}

	m := &Metrics{
		Health: ov.Health,
		Backlog: ov.Backlog,
		Consumers: ov.Consumers,
		Topics: int64(len(topics)),
		Groups: int64(len(groups)),
		Nodes: int64(len(brokers)),
		MaxLag: -1,
	}
	for _, t := range topics {
		for _, p := range t.Partitions {
			if p.Leader < 0 {
				m.Offline++
			}
			if len(p.ISR) < len(p.Replicas) {
				m.UnderReplicated++
			}
		}
	}
	if len(groups) > 0 {
		lags, err := k.admin.Lag(ctx, groups.Groups()...)
		if err == nil {
			for _, g := range lags {
				if g.Lag.Total() > m.MaxLag {
					m.MaxLag = g.Lag.Total()
				}
				if g.Lag.Total() > 0 && len(g.Members) == 0 {
					m.Starved++
				}
			}
		}
	}
	return m, nil
}
