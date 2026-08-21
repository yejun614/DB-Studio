// Package broker는 메시지 브로커(RabbitMQ·Kafka)를 다룬다.
//
// 스토리지와 같은 자리에 두지 않은 이유: 물어보는 것이 다르다. 스토리지에서는 "얼마나 찼나"가
// 첫 질문이지만, 브로커에서는 **"쌓이고 있나"** 가 첫 질문이다. 큐 깊이는 그 자체로는 좋지도
// 나쁘지도 않다 — 소비자가 따라오고 있으면 정상이고, 따라오지 못하면 장애의 전조다.
// 그래서 이 패키지는 언제나 "쌓인 양"과 "그것을 줄이는 쪽"을 함께 본다.
//
// 두 브로커의 접근 방법이 다르다.
//   - RabbitMQ: 관리 플러그인의 HTTP API(기본 15672)를 쓴다.
//   - Kafka: 브로커에 HTTP API가 없다. 카프카 프로토콜을 직접 말하는 순수 Go 클라이언트
//     (franz-go)로 메타데이터·컨슈머 랙을 읽는다. REST 프록시를 요구하면 그것을 따로 세워야
//     하는 배포에서는 이 기능을 아예 쓸 수 없다.
package broker

import (
	"context"
	"time"

	"dbstudio/internal/opsapi"
)

// 이 패키지가 다루는 종류.
const (
	KindRabbitMQ = "rabbitmq"
	KindKafka    = "kafka"
)

// 공통 어휘는 opsapi에 있다(storage와 같은 이유로 별칭을 둔다).
type (
	Health = opsapi.Health
	Fact   = opsapi.Fact
	Config = opsapi.Config
)

const (
	HealthOK      = opsapi.HealthOK
	HealthWarn    = opsapi.HealthWarn
	HealthError   = opsapi.HealthError
	HealthUnknown = opsapi.HealthUnknown
)

// ConfigFrom은 커넥션과 자격증명에서 접속 설정을 만든다.
var ConfigFrom = opsapi.ConfigFrom

// Overview는 개요 화면 한 장이다.
type Overview struct {
	Kind    string `json:"kind"`
	Version string `json:"version,omitempty"`
	Health  Health `json:"health"`
	// Backlog는 지금 브로커에 남아 있는 메시지 수다(RabbitMQ는 큐의 합, Kafka는 총 랙).
	//
	// 두 브로커의 서로 다른 개념을 같은 칸에 담는 이유: 운영자가 화면을 열고 처음 보는
	// 숫자는 "밀린 게 얼마나 되나"이고, 그 답은 브로커 종류와 무관하게 같은 뜻이다.
	Backlog int64 `json:"backlog"`
	// Consumers는 그 적체를 줄이고 있는 쪽의 수다(소비자 또는 컨슈머 그룹 멤버).
	Consumers int64    `json:"consumers"`
	Facts     []Fact   `json:"facts"`
	Notes     []string `json:"notes,omitempty"`
}

// Queue는 RabbitMQ 큐 하나다.
type Queue struct {
	Name  string `json:"name"`
	VHost string `json:"vhost"`
	State string `json:"state"`
	// Ready는 소비자에게 아직 나가지 않은 메시지, Unacked는 나갔지만 확인되지 않은 메시지다.
	Ready       int64   `json:"ready"`
	Unacked     int64   `json:"unacked"`
	Total       int64   `json:"total"`
	Consumers   int64   `json:"consumers"`
	Memory      int64   `json:"memory"`
	Node        string  `json:"node,omitempty"`
	Durable     bool    `json:"durable"`
	PublishRate float64 `json:"publishRate"`
	DeliverRate float64 `json:"deliverRate"`
	IdleSince   string  `json:"idleSince,omitempty"`
	// Starved는 "메시지는 쌓여 있는데 소비자가 없다"는 뜻이다. 브로커에서 가장 흔한
	// 사고 유형이고, 큐 목록에서 눈에 먼저 들어와야 한다.
	Starved bool `json:"starved"`
}

// Exchange는 RabbitMQ 익스체인지 하나다.
type Exchange struct {
	Name    string `json:"name"`
	VHost   string `json:"vhost"`
	Type    string `json:"type"`
	Durable bool   `json:"durable"`
	// InRate/OutRate는 들어오고 나가는 초당 메시지다. 라우팅이 끊긴 익스체인지는
	// 들어오는 것만 있고 나가는 것이 없다.
	InRate  float64 `json:"inRate"`
	OutRate float64 `json:"outRate"`
}

// NodeInfo는 브로커 노드 하나다(RabbitMQ 노드 또는 Kafka 브로커).
type NodeInfo struct {
	Name    string `json:"name"`
	Running bool   `json:"running"`
	// Host/Port는 Kafka 브로커에 쓴다.
	Host string `json:"host,omitempty"`
	Port int    `json:"port,omitempty"`
	// Controller/Leader 표시(Kafka).
	Controller bool `json:"controller,omitempty"`

	MemUsed   int64    `json:"memUsed,omitempty"`
	MemLimit  int64    `json:"memLimit,omitempty"`
	DiskFree  int64    `json:"diskFree,omitempty"`
	DiskLimit int64    `json:"diskLimit,omitempty"`
	FDUsed    int64    `json:"fdUsed,omitempty"`
	FDTotal   int64    `json:"fdTotal,omitempty"`
	Alarms    []string `json:"alarms,omitempty"`
	// Partitions는 네트워크 분단으로 갈라져 보이는 다른 노드들이다(RabbitMQ).
	// 비어 있지 않으면 클러스터가 쪼개진 상태다.
	Partitions []string `json:"partitions,omitempty"`
	Uptime     int64    `json:"uptime,omitempty"`
}

// Topic은 Kafka 토픽 하나다.
type Topic struct {
	Name              string `json:"name"`
	Partitions        int    `json:"partitions"`
	ReplicationFactor int    `json:"replicationFactor"`
	// UnderReplicated는 ISR이 복제 수를 못 채운 파티션 수다.
	UnderReplicated int `json:"underReplicated"`
	// Offline은 리더가 없는 파티션 수다. 그 파티션은 읽지도 쓰지도 못한다.
	Offline int `json:"offline"`
	// Messages는 (끝 오프셋 - 시작 오프셋)의 합이다. 정확한 메시지 수가 아니라
	// **보관 중인 양의 근사**다 — 압축(compaction)과 보관 기간 삭제 때문이다.
	Messages int64 `json:"messages"`
	Internal bool  `json:"internal"`
	// Config는 눈에 띄어야 하는 설정만 담는다(보관 기간, 정리 정책 등).
	Config map[string]string `json:"config,omitempty"`
}

// Group은 컨슈머 그룹 하나다(Kafka).
type Group struct {
	Name     string `json:"name"`
	State    string `json:"state"`
	Members  int    `json:"members"`
	Protocol string `json:"protocol,omitempty"`
	// Lag은 이 그룹이 아직 읽지 못한 메시지 수의 합이다. 브로커 운영에서 가장 중요한 숫자다.
	Lag    int64      `json:"lag"`
	Topics []GroupLag `json:"topics,omitempty"`
}

// GroupLag는 그룹이 구독 중인 토픽 하나의 랙이다.
type GroupLag struct {
	Topic string `json:"topic"`
	Lag   int64  `json:"lag"`
}

// Conn은 브로커에 붙어 있는 클라이언트 연결 하나다(RabbitMQ).
type Conn struct {
	Name        string    `json:"name"`
	User        string    `json:"user"`
	VHost       string    `json:"vhost"`
	State       string    `json:"state"`
	Channels    int       `json:"channels"`
	Peer        string    `json:"peer"`
	Protocol    string    `json:"protocol,omitempty"`
	ConnectedAt time.Time `json:"connectedAt,omitempty"`
}

// Client는 두 브로커가 공통으로 답하는 것이다.
type Client interface {
	Kind() string
	Overview(ctx context.Context) (*Overview, error)
}

// Metrics는 폴러가 쓰는 값이다.
//
// 두 브로커의 지표를 한 구조로 모으는 이유: 임계값 룰은 종류를 모르고도 걸려야 한다.
// "밀린 메시지"와 "그것을 줄이는 쪽의 수"는 어느 브로커에나 있다.
type Metrics struct {
	Health Health
	// Backlog는 밀린 메시지(RabbitMQ: ready+unacked, Kafka: 총 랙).
	Backlog int64
	// Unacked는 나갔지만 확인되지 않은 메시지다(RabbitMQ).
	Unacked int64
	// Consumers는 소비자 수(RabbitMQ) 또는 그룹 멤버 수(Kafka).
	Consumers int64
	// Starved는 "메시지는 있는데 소비자가 없는" 큐·그룹의 수다.
	Starved int64
	// Queues/Topics/Groups/Nodes는 규모다.
	Queues    int64
	Topics    int64
	Groups    int64
	Nodes     int64
	NodesDown int64
	// PublishRate/DeliverRate는 초당 메시지다(RabbitMQ).
	PublishRate float64
	DeliverRate float64
	// MaxLag는 그룹 하나가 가진 가장 큰 랙이다(Kafka).
	MaxLag int64
	// Offline/UnderReplicated는 파티션 상태다(Kafka).
	Offline         int64
	UnderReplicated int64
	// Alarms는 메모리·디스크 알람 수다(RabbitMQ).
	Alarms int64
}
