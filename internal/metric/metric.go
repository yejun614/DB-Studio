// Package metric은 모니터링 지표의 타입만 정의한다.
//
// 타입만 분리한 이유: dbx(수집기)와 monitor(폴러/룰 엔진)가 모두 이 타입을 쓰는데,
// monitor는 dbx를 임포트하므로 타입을 어느 한쪽에 두면 순환 의존이 생긴다.
package metric

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Kind는 지표의 성질이다.
type Kind string

const (
	// Gauge는 그 시점의 값이다 (활성 커넥션 수, 메모리 사용량).
	Gauge Kind = "gauge"
	// Counter는 서버 시작 후 누적된 값이다 (총 쿼리 수).
	// 절대값은 서버 재시작 시 초기화되어 의미가 약하므로,
	// 폴러가 이전 값과의 차이로 초당 변화율을 계산해 저장한다.
	Counter Kind = "counter"
)

// Unit은 값의 단위다. UI가 축 라벨과 포맷을 결정하는 데 쓴다.
type Unit string

const (
	UnitCount   Unit = "count"
	UnitPerSec  Unit = "per_sec"
	UnitBytes   Unit = "bytes"
	UnitMillis  Unit = "ms"
	UnitSeconds Unit = "s"
	UnitPercent Unit = "percent"
	UnitRatio   Unit = "ratio"
)

// Sample은 수집된 지표 한 건이다.
type Sample struct {
	Name  string  `json:"name"`
	Value float64 `json:"value"`
	Kind  Kind    `json:"kind"`
	Unit  Unit    `json:"unit"`
}

// Set은 한 번의 수집 결과 전체다.
type Set struct {
	Samples []Sample `json:"samples"`
	// Notes는 권한 부족 등으로 일부 지표를 수집하지 못한 사유다.
	// 전체 실패와 부분 실패를 구분해 사용자에게 알리기 위해 존재한다.
	Notes []string `json:"notes,omitempty"`
	// CollectedAt은 수집 시각이다.
	CollectedAt time.Time `json:"collectedAt"`
	// Latency는 수집에 걸린 시간이다. 그 자체로 DB 응답성 지표가 된다.
	LatencyMs float64 `json:"latencyMs"`
}

func NewSet() *Set {
	return &Set{Samples: []Sample{}, CollectedAt: time.Now().UTC()}
}

// Clone은 샘플과 사유를 복사한 새 Set을 만든다.
//
// 폴러가 같은 대상을 가리키는 커넥션 여러 개에 한 번의 수집 결과를 나눠 줄 때
// 쓴다. 그대로 나눠 주면 한쪽에서 변화율을 계산하며 Samples를 갈아 끼운 것이
// 다른 쪽에도 보이고, 두 번째부터는 이미 변환된 값을 다시 변환하게 된다.
func (s *Set) Clone() *Set {
	if s == nil {
		return nil
	}
	out := &Set{
		Samples:     make([]Sample, len(s.Samples)),
		CollectedAt: s.CollectedAt,
		LatencyMs:   s.LatencyMs,
	}
	copy(out.Samples, s.Samples)
	if s.Notes != nil {
		out.Notes = make([]string, len(s.Notes))
		copy(out.Notes, s.Notes)
	}
	return out
}

// Gauge는 게이지 지표를 추가한다.
func (s *Set) Gauge(name string, value float64, unit Unit) {
	s.Samples = append(s.Samples, Sample{Name: name, Value: value, Kind: Gauge, Unit: unit})
}

// Counter는 누적 카운터를 추가한다. 폴러가 변화율로 변환한다.
func (s *Set) Counter(name string, value float64) {
	s.Samples = append(s.Samples, Sample{Name: name, Value: value, Kind: Counter, Unit: UnitPerSec})
}

// AddNote는 부분 실패를 기록한다. 같은 메시지는 중복하지 않는다.
func (s *Set) AddNote(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	for _, n := range s.Notes {
		if n == msg {
			return
		}
	}
	s.Notes = append(s.Notes, msg)
}

// Get은 이름으로 샘플을 찾는다.
func (s *Set) Get(name string) (Sample, bool) {
	for _, sm := range s.Samples {
		if sm.Name == name {
			return sm, true
		}
	}
	return Sample{}, false
}

// Sort는 이름순으로 정렬해 출력을 결정적으로 만든다.
func (s *Set) Sort() {
	sort.SliceStable(s.Samples, func(i, j int) bool { return s.Samples[i].Name < s.Samples[j].Name })
}

// ---------- 지표 카탈로그 ----------

// 표준 지표 이름. DB 종류가 달라도 같은 의미면 같은 이름을 쓴다.
// 그래야 여러 DB를 한 대시보드에서 나란히 볼 수 있다.
const (
	// 공통
	NameUp              = "up"                    // 1=정상, 0=응답 없음
	NameLatency         = "response_time"         // 수집 왕복 시간
	NameConnActive      = "connections.active"    // 활성 세션
	NameConnTotal       = "connections.total"     // 전체 세션
	NameConnMax         = "connections.max"       // 상한
	NameConnUsedPct     = "connections.used_pct"  // 상한 대비 사용률
	NameUptime          = "uptime"                // 서버 가동 시간
	NameDataSize        = "size.data"             // 데이터 크기
	NameIndexSize       = "size.index"            // 인덱스 크기
	NameMemoryUsed      = "memory.used"           // 메모리 사용량
	NameCacheHitRatio   = "cache.hit_ratio"       // 캐시 적중률
	NameQueryRate       = "query.rate"            // 초당 쿼리
	NameSlowQueryRate   = "query.slow_rate"       // 초당 슬로우 쿼리
	NameTxnCommitRate   = "txn.commit_rate"       // 초당 커밋
	NameTxnRollbkRate   = "txn.rollback_rate"     // 초당 롤백
	NameLockWaits       = "lock.waits"            // 락 대기
	NameLockWaitTime    = "lock.wait_time"        // 평균 락 대기 시간
	NameDeadlocks       = "lock.deadlock_rate"    // 초당 데드락
	NameLongestQuery    = "query.longest_running" // 최장 실행 쿼리 시간
	NameReplicaLag      = "replication.lag"       // 복제 지연
	NameBlockedClients  = "clients.blocked"       // 대기 중인 클라이언트
	NameEvictedRate     = "keys.evicted_rate"     // 초당 축출 키 (Redis)
	NameAbortedConnRate = "connections.aborted_rate"

	// 분산 스토리지 (하둡·Ceph)
	//
	// 종류가 달라도 뜻이 같으면 이름을 공유한다: 하둡의 데이터노드와 Ceph의 OSD는
	// 둘 다 "데이터를 들고 있는 구성원"이므로 storage.nodes.* 하나로 본다. 그래야
	// 임계값 룰을 종류마다 따로 만들지 않아도 된다.
	NameStorageUsedPct   = "storage.used_pct"   // 클러스터 용량 사용률
	NameStorageTotal     = "storage.total"      // 전체 용량
	NameStorageUsed      = "storage.used"       // 사용 용량
	NameStorageHealth    = "storage.health"     // 0=정상 1=주의 2=위험 (-1=모름)
	NameStorageNodesUp   = "storage.nodes.up"   // 살아 있는 데이터노드/OSD
	NameStorageNodesDown = "storage.nodes.down" // 죽은 데이터노드/OSD

	NameHDFSMissingBlocks   = "hdfs.blocks.missing"          // 복제본이 하나도 없는 블록
	NameHDFSUnderReplicated = "hdfs.blocks.under_replicated" // 복제본이 모자란 블록
	NameHDFSCorruptBlocks   = "hdfs.blocks.corrupt"
	NameYARNAppsRunning     = "yarn.apps.running"
	NameYARNAppsPending     = "yarn.apps.pending"
	NameYARNMemUsedPct      = "yarn.memory.used_pct"

	NameCephOSDsIn     = "ceph.osd.in"     // 데이터 배치에 참여 중인 OSD
	NameCephPGsUnclean = "ceph.pg.unclean" // active+clean이 아닌 PG
	NameCephPools      = "ceph.pools"

	// 오브젝트 스토리지(S3 규약). 용량 지표가 없는 것이 특징이다 —
	// 오브젝트 스토리지에는 "총 용량"이 없고 쓰는 만큼 늘어난다. 있는 척 0을
	// 채우면 사용률 막대가 늘 0%로 보이고, 그것은 아무 말도 하지 않는다.
	NameS3Buckets = "s3.buckets" // 버킷 수

	// ClickHouse. 열 지향 DB 에서 먼저 무너지는 곳은 다른 DB 와 다르다 —
	// 접속 수도 락도 아니고 **병합이 쓰기를 따라오는가**다.
	NameClickHouseMaxParts    = "clickhouse.parts.max" // 파티션 하나의 파트 수
	NameClickHouseCompression = "clickhouse.compression_ratio"

	// 벡터 DB. 첫 질문은 "몇 개가 들어 있고 색인이 준비됐나"다.
	// 색인이 준비되지 않은 컬렉션은 검색이 되기는 하지만 전수 조사로 떨어져서,
	// 느려진 이유가 데이터가 늘어서인지 색인이 아직 안 끝나서인지 알 수 없다.
	NameVectorCollections = "vector.collections" // 컬렉션(인덱스) 수
	NameVectorPoints      = "vector.points"      // 담긴 벡터 수
	NameVectorIndexed     = "vector.indexed"     // 색인이 끝난 벡터 수
	NameVectorNotReady    = "vector.not_ready"   // 준비되지 않은 컬렉션 수
	NameVectorFullness    = "vector.fullness"    // 인덱스 사용률(Pinecone)

	// 메시지 브로커(RabbitMQ·Kafka). 브로커의 첫 질문은 "쌓이고 있나"다.
	// 큐 깊이·랙은 그 자체로 좋지도 나쁘지도 않다 — 소비자가 따라오고 있으면
	// 정상이고, 따라오지 못하면 장애의 전조다. 그래서 항상 "쌓인 양"과
	// "그것을 줄이는 쪽"을 함께 본다.
	NameBrokerHealth    = "broker.health"           // 0=정상 1=주의 2=위험 (-1=모름)
	NameBrokerBacklog   = "broker.backlog"          // 밀린 메시지 (RabbitMQ: 큐 합, Kafka: 총 랙)
	NameBrokerUnacked   = "broker.unacked"          // 나갔지만 확인되지 않은 메시지 (RabbitMQ)
	NameBrokerConsumers = "broker.consumers"        // 소비자 수 (RabbitMQ) / 그룹 멤버 수 (Kafka)
	NameBrokerStarved   = "broker.starved"          // 메시지는 있는데 소비자가 없는 큐·그룹 수
	NameBrokerQueues    = "broker.queues"           // 큐 수 (RabbitMQ)
	NameBrokerTopics    = "broker.topics"           // 토픽 수 (Kafka)
	NameBrokerGroups    = "broker.groups"           // 컨슈머 그룹 수 (Kafka)
	NameBrokerNodes     = "broker.nodes"            // 노드 수
	NameBrokerNodesDown = "broker.nodes.down"       // 죽은 노드 수
	NameBrokerPublish   = "broker.publish_rate"     // 초당 발행 (RabbitMQ)
	NameBrokerDeliver   = "broker.deliver_rate"     // 초당 소비 (RabbitMQ)
	NameBrokerMaxLag    = "broker.max_lag"          // 그룹 하나의 최대 랙 (Kafka)
	NameBrokerOffline   = "broker.offline"          // 리더 없는 파티션 수 (Kafka)
	NameBrokerUnderRep  = "broker.under_replicated" // 복제본 부족 파티션 수 (Kafka)
	NameBrokerAlarms    = "broker.alarms"           // 메모리·디스크 알람 수 (RabbitMQ)
)

// Meta는 지표의 표시 정보다.
type Meta struct {
	Name  string `json:"name"`
	Label string `json:"label"`
	Unit  Unit   `json:"unit"`
	// Help는 지표의 의미와 해석 방법이다.
	Help string `json:"help"`
	// HigherIsBetter가 true면 값이 클수록 좋다 (캐시 적중률).
	// UI가 색상 방향을 결정하는 데 쓴다.
	HigherIsBetter bool `json:"higherIsBetter"`
	// Primary가 true면 대시보드 요약 카드에 표시한다.
	Primary bool `json:"primary"`
}

var catalog = map[string]Meta{
	NameUp:              {Label: "응답", Unit: UnitCount, Help: "1이면 정상 응답, 0이면 접속 실패", HigherIsBetter: true, Primary: true},
	NameLatency:         {Label: "응답 시간", Unit: UnitMillis, Help: "지표 수집 왕복 시간. DB 응답성의 직접적인 신호입니다", Primary: true},
	NameConnActive:      {Label: "활성 세션", Unit: UnitCount, Help: "현재 쿼리를 실행 중인 세션 수", Primary: true},
	NameConnTotal:       {Label: "전체 세션", Unit: UnitCount, Help: "유휴 세션을 포함한 전체 접속 수", Primary: true},
	NameConnMax:         {Label: "세션 상한", Unit: UnitCount, Help: "서버 설정상 최대 동시 접속 수"},
	NameConnUsedPct:     {Label: "세션 사용률", Unit: UnitPercent, Help: "상한 대비 사용 비율. 100%에 가까우면 신규 접속이 거부됩니다", Primary: true},
	NameUptime:          {Label: "가동 시간", Unit: UnitSeconds, Help: "서버 시작 후 경과 시간. 갑자기 줄면 재시작이 있었다는 뜻입니다"},
	NameDataSize:        {Label: "데이터 크기", Unit: UnitBytes, Help: "테이블 데이터가 차지하는 용량"},
	NameIndexSize:       {Label: "인덱스 크기", Unit: UnitBytes, Help: "인덱스가 차지하는 용량"},
	NameMemoryUsed:      {Label: "메모리 사용량", Unit: UnitBytes, Help: "DB 프로세스가 사용 중인 메모리"},
	NameCacheHitRatio:   {Label: "캐시 적중률", Unit: UnitPercent, Help: "버퍼/캐시에서 읽은 비율. 낮으면 디스크 I/O가 늘어납니다", HigherIsBetter: true, Primary: true},
	NameQueryRate:       {Label: "초당 쿼리", Unit: UnitPerSec, Help: "초당 실행된 쿼리 수", Primary: true},
	NameSlowQueryRate:   {Label: "초당 슬로우 쿼리", Unit: UnitPerSec, Help: "설정된 임계 시간을 넘긴 쿼리 발생률", Primary: true},
	NameTxnCommitRate:   {Label: "초당 커밋", Unit: UnitPerSec, Help: "초당 커밋된 트랜잭션 수"},
	NameTxnRollbkRate:   {Label: "초당 롤백", Unit: UnitPerSec, Help: "초당 롤백된 트랜잭션 수. 급증은 오류나 경합을 뜻합니다"},
	NameLockWaits:       {Label: "락 대기", Unit: UnitCount, Help: "락을 기다리는 세션 수"},
	NameLockWaitTime:    {Label: "평균 락 대기", Unit: UnitMillis, Help: "락 획득까지 걸린 평균 시간"},
	NameDeadlocks:       {Label: "초당 데드락", Unit: UnitPerSec, Help: "데드락 발생률. 0이 아니면 조사가 필요합니다"},
	NameLongestQuery:    {Label: "최장 실행 쿼리", Unit: UnitSeconds, Help: "현재 실행 중인 쿼리 중 가장 오래된 것의 경과 시간", Primary: true},
	NameReplicaLag:      {Label: "복제 지연", Unit: UnitSeconds, Help: "주 서버 대비 지연 시간", Primary: true},
	NameBlockedClients:  {Label: "대기 클라이언트", Unit: UnitCount, Help: "블로킹 명령에서 대기 중인 클라이언트 수"},
	NameEvictedRate:     {Label: "초당 키 축출", Unit: UnitPerSec, Help: "메모리 한계로 삭제된 키 발생률. 0이 아니면 메모리가 부족합니다"},
	NameAbortedConnRate: {Label: "초당 접속 실패", Unit: UnitPerSec, Help: "거부되거나 중단된 접속 시도 발생률"},

	NameStorageUsedPct: {Label: "클러스터 사용률", Unit: UnitPercent, Primary: true,
		Help: "전체 용량 대비 사용률. HDFS는 복제본을 포함한 물리 사용량입니다"},
	NameStorageTotal: {Label: "전체 용량", Unit: UnitBytes,
		Help: "클러스터가 가진 원시(raw) 용량. HDFS는 데이터노드 디스크의 합, Ceph는 OSD 디스크의 합입니다"},
	NameStorageUsed: {Label: "사용 용량", Unit: UnitBytes, Primary: true,
		Help: "복제본을 포함해 실제로 차지하고 있는 용량입니다"},
	NameStorageHealth: {Label: "클러스터 상태", Unit: UnitCount, Primary: true,
		Help: "0=정상, 1=주의, 2=위험. Ceph의 HEALTH_*와 HDFS의 손실 블록·세이프모드를 같은 눈금으로 본 값입니다"},
	NameStorageNodesUp: {Label: "정상 노드", Unit: UnitCount, Primary: true,
		Help: "살아 있는 데이터노드(HDFS) 또는 up 상태 OSD(Ceph)"},
	NameStorageNodesDown: {Label: "비정상 노드", Unit: UnitCount, Primary: true,
		Help: "죽은 데이터노드(HDFS) 또는 down 상태 OSD(Ceph). 0이 아니면 복제본이 줄어든 상태입니다"},

	NameHDFSMissingBlocks: {Label: "손실 블록", Unit: UnitCount, Primary: true,
		Help: "복제본이 하나도 남지 않은 블록. 0이 아니면 이미 데이터가 유실된 상태입니다"},
	NameHDFSUnderReplicated: {Label: "복제 부족 블록", Unit: UnitCount,
		Help: "설정된 복제 수를 채우지 못한 블록. 노드가 빠지면 늘고, 복구되면 줄어듭니다"},
	NameHDFSCorruptBlocks: {Label: "손상 블록", Unit: UnitCount,
		Help: "체크섬이 맞지 않는 복제본이 있는 블록. 디스크 오류의 신호입니다"},
	NameYARNAppsRunning: {Label: "실행 중 앱", Unit: UnitCount,
		Help: "지금 자원을 받아 돌고 있는 YARN 애플리케이션 수"},
	NameYARNAppsPending: {Label: "대기 중 앱", Unit: UnitCount, Help: "자원을 기다리는 애플리케이션 수"},
	NameYARNMemUsedPct: {Label: "YARN 메모리 사용률", Unit: UnitPercent,
		Help: "클러스터 메모리 중 할당된 비율. 100%에 붙어 있으면 대기 앱이 쌓입니다"},

	NameClickHouseMaxParts: {Label: "최대 파트 수", Unit: UnitCount, Primary: true,
		Help: "한 파티션이 가진 파트 수 중 가장 큰 값입니다. 300 근처로 오르면 병합이 쓰기를 따라오지 못하는 것이고, 그대로 두면 INSERT가 거절되기 시작합니다"},
	NameClickHouseCompression: {Label: "압축비", Unit: UnitCount, HigherIsBetter: true,
		Help: "원본 크기 ÷ 디스크 크기입니다. 갑자기 낮아지면 압축이 잘 되지 않는 데이터가 들어온 것입니다"},

	NameS3Buckets: {Label: "버킷", Unit: UnitCount, Primary: true,
		Help: "이 자격증명으로 보이는 버킷 수입니다. 객체 수와 크기는 버킷을 나열해야 알 수 있어 수집 주기마다 세지 않습니다"},

	NameVectorCollections: {Label: "컬렉션", Unit: UnitCount, Primary: true,
		Help: "이 서버의 컬렉션(Pinecone은 인덱스) 수입니다"},
	NameVectorPoints: {Label: "벡터", Unit: UnitCount, Primary: true,
		Help: "모든 컬렉션에 담긴 벡터 수의 합입니다"},
	NameVectorIndexed: {Label: "색인된 벡터", Unit: UnitCount,
		Help: "색인에 올라간 벡터 수. 담긴 수보다 한참 적으면 색인이 아직 따라오지 못한 것입니다"},
	NameVectorNotReady: {Label: "준비 안 된 컬렉션", Unit: UnitCount, Primary: true,
		Help: "green(준비됨)이 아닌 컬렉션 수입니다. 0이 아니면 그 컬렉션의 검색이 전수 조사로 떨어져 느려집니다"},
	NameVectorFullness: {Label: "인덱스 사용률", Unit: UnitPercent,
		Help: "Pinecone이 알려주는 인덱스 사용률입니다. 100%에 가까우면 더 넣을 수 없습니다"},

	NameCephOSDsIn:     {Label: "in 상태 OSD", Unit: UnitCount, Help: "데이터 배치에 참여 중인 OSD"},
	NameCephPGsUnclean: {Label: "비정상 PG", Unit: UnitCount, Help: "active+clean이 아닌 배치 그룹 수"},
	NameCephPools: {Label: "풀", Unit: UnitCount,
		Help: "클러스터의 풀 개수. 갑자기 줄면 누군가 풀을 지운 것입니다"},

	NameBrokerBacklog: {Label: "밀린 메시지", Unit: UnitCount, Primary: true,
		Help: "아직 소비되지 않은 메시지 수. RabbitMQ는 큐의 합, Kafka는 컨슈머 그룹의 총 랙입니다. 소비자가 따라오고 있으면 정상입니다"},
	NameBrokerHealth: {Label: "브로커 상태", Unit: UnitCount, Primary: true,
		Help: "0=정상, 1=주의, 2=위험. RabbitMQ의 알람·분단과 Kafka의 리더 없는 파티션을 같은 눈금으로 본 값입니다"},
	NameBrokerUnacked: {Label: "미확인 메시지", Unit: UnitCount,
		Help: "소비자에게 나갔지만 확인(ack)되지 않은 메시지 수 (RabbitMQ). 소비자가 죽으면 이 값이 계속 쌓입니다"},
	NameBrokerConsumers: {Label: "소비자", Unit: UnitCount, Primary: true,
		Help: "적체를 줄이고 있는 쪽의 수. RabbitMQ는 소비자 수, Kafka는 컨슈머 그룹 멤버 수입니다"},
	NameBrokerStarved: {Label: "소비자 없는 큐·그룹", Unit: UnitCount, Primary: true,
		Help: "메시지는 쌓이는데 소비자가 없는 큐(RabbitMQ) 또는 그룹(Kafka) 수. 브로커에서 가장 흔한 사고 유형입니다"},
	NameBrokerQueues: {Label: "큐", Unit: UnitCount, Help: "RabbitMQ 큐 개수"},
	NameBrokerTopics: {Label: "토픽", Unit: UnitCount, Help: "Kafka 토픽 개수"},
	NameBrokerGroups: {Label: "컨슈머 그룹", Unit: UnitCount, Help: "Kafka 컨슈머 그룹 개수"},
	NameBrokerNodes:  {Label: "노드", Unit: UnitCount, Help: "클러스터를 이루는 노드 수"},
	NameBrokerNodesDown: {Label: "비정상 노드", Unit: UnitCount, Primary: true,
		Help: "멈춰 있는 노드 수. 0이 아니면 복제본이 줄어든 상태입니다"},
	NameBrokerPublish: {Label: "초당 발행", Unit: UnitPerSec,
		Help: "초당 들어오는 메시지 수 (RabbitMQ)"},
	NameBrokerDeliver: {Label: "초당 소비", Unit: UnitPerSec,
		Help: "초당 소비자에게 나가는 메시지 수 (RabbitMQ). 발행보다 오래 낮으면 적체가 자랍니다"},
	NameBrokerMaxLag: {Label: "최대 랙", Unit: UnitCount, Primary: true,
		Help: "컨슈머 그룹 하나가 가진 가장 큰 랙 (Kafka). 한 그룹이 크게 뒤처지면 그 서비스만 지연됩니다"},
	NameBrokerOffline: {Label: "리더 없는 파티션", Unit: UnitCount, Primary: true,
		Help: "리더가 없는 파티션 수 (Kafka). 그 파티션은 읽지도 쓰지도 못합니다"},
	NameBrokerUnderRep: {Label: "복제 부족 파티션", Unit: UnitCount,
		Help: "ISR이 복제 수를 채우지 못한 파티션 수 (Kafka). 노드가 빠지면 늘고, 복구되면 줄어듭니다"},
	NameBrokerAlarms: {Label: "알람", Unit: UnitCount,
		Help: "메모리·디스크 알람 수 (RabbitMQ). 0이 아니면 발행이 차단됩니다"},
}

// Lookup은 지표의 표시 정보를 반환한다. 카탈로그에 없으면 이름을 그대로 라벨로 쓴다.
func Lookup(name string) Meta {
	if m, ok := catalog[name]; ok {
		m.Name = name
		return m
	}
	return Meta{Name: name, Label: name, Unit: UnitCount}
}

// Catalog는 전체 카탈로그를 이름순으로 반환한다. UI가 룰 설정 폼을 그리는 데 쓴다.
func Catalog() []Meta {
	out := make([]Meta, 0, len(catalog))
	for name, m := range catalog {
		m.Name = name
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ParseInfoLines는 "key:value" 형식의 텍스트(Redis INFO 등)를 맵으로 만든다.
func ParseInfoLines(raw string) map[string]string {
	out := map[string]string{}
	for line := range strings.SplitSeq(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		out[strings.TrimSpace(key)] = strings.TrimSpace(value)
	}
	return out
}
