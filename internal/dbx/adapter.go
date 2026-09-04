// Package dbx는 관리 대상 데이터베이스에 대한 접속/조회를 종류별로 추상화한다.
//
// 모든 어댑터는 Pure Go 드라이버만 사용하므로 CGO 없이 단일 바이너리로 빌드된다.
package dbx

import (
	"context"
	"errors"
	"fmt"
	"time"

	"dbstudio/internal/dblog"
	"dbstudio/internal/metric"
	"dbstudio/internal/model"
	"dbstudio/internal/schema"
)

var (
	ErrUnsupportedKind = errors.New("지원하지 않는 데이터베이스 종류")
	ErrNotImplemented  = errors.New("이 DB 종류에서는 지원되지 않는 기능")
)

// Target은 어댑터가 접속에 필요한 모든 정보를 담은 값이다.
// 자격증명이 포함되므로 로그에 그대로 출력해서는 안 된다.
type Target struct {
	Conn   *model.Connection
	Secret *model.Secret
}

func (t Target) Username() string {
	if t.Secret != nil && t.Secret.Username != "" {
		return t.Secret.Username
	}
	return ""
}

func (t Target) Password() string {
	if t.Secret != nil {
		return t.Secret.Password
	}
	return ""
}

func (t Target) Opt(key, def string) string {
	if t.Conn == nil {
		return def
	}
	return t.Conn.Options.GetOr(key, def)
}

// Capabilities는 어댑터가 지원하는 기능을 알려준다.
// UI는 미지원 기능을 비활성화하고, AI 툴은 호출 전에 이 값을 확인한다.
type Capabilities struct {
	Introspect bool `json:"introspect"` // 스키마 읽기 (P3)
	Monitor    bool `json:"monitor"`    // 상태/부하 지표 (P4)
	Logs       bool `json:"logs"`       // 로그 조회 (P5)
	Migrate    bool `json:"migrate"`    // DDL 마이그레이션 (P7)
	ERD        bool `json:"erd"`        // ERD 설계 대상 (P6)
	Explore    bool `json:"explore"`    // 종류별 특화 탐색 화면 (P10, Mongo/Redis)
	// Storage는 분산 스토리지 관리 화면 대상이다(P25, 하둡/Ceph).
	// 이 값이 참이면 스키마·데이터·SQL 메뉴 대신 스토리지 화면이 열린다.
	Storage bool `json:"storage"`
	// Broker는 메시지 브로커 관리 화면 대상이다(RabbitMQ/Kafka).
	// Storage와 같은 이유로 따로 둔다 — 화면은 이 값을 보고 브로커 화면을 연다.
	Broker bool `json:"broker"`
	// Vector는 벡터 화면 대상이다(Qdrant/Pinecone).
	//
	// PostgreSQL 은 여기서 참이 아니다 — pgvector 는 **확장**이라 깔려 있는지는
	// 붙어 봐야 안다. "이 종류에서 벡터 화면을 열어 볼 수 있는가"는
	// SupportsVector 가 답하고, "볼 것이 있는가"는 실제로 붙어서 답한다.
	Vector bool `json:"vector"`
}

// ServerInfo는 연결 테스트 시 수집하는 대상 서버 기본 정보다.
type ServerInfo struct {
	Version  string            `json:"version"`
	Extra    map[string]string `json:"extra,omitempty"`
	Latency  time.Duration     `json:"-"`
	LatencyM float64           `json:"latencyMs"`
}

// Adapter는 DB 종류별 구현이 만족해야 하는 인터페이스다.
// 이후 단계에서 Metrics/Logs가 추가된다.
type Adapter interface {
	Kind() model.DBKind
	Capabilities() Capabilities

	// DefaultPort는 사용자가 포트를 비워둔 경우의 기본값이다. 0이면 포트 개념이 없다.
	DefaultPort() int

	// Validate는 접속 정보의 형식을 검사한다. 네트워크 접근은 하지 않는다.
	Validate(t Target) error

	// Ping은 실제로 접속해 응답을 확인하고 서버 정보를 반환한다.
	Ping(ctx context.Context, t Target) (*ServerInfo, error)

	// Introspect는 대상 DB의 구조를 읽어 스키마 IR로 반환한다.
	// 미지원 종류는 ErrNotImplemented를 반환한다.
	Introspect(ctx context.Context, t Target) (*schema.Schema, error)

	// Metrics는 상태·부하 지표를 수집한다.
	//
	// 접속 실패는 에러가 아니라 up=0 샘플로 반환한다 — 폴러가 그것을 근거로
	// 연결 이벤트를 만들어야 하므로, "못 물어봤다"와 "물어봤는데 죽어 있다"를
	// 구분할 수 있어야 한다. 에러는 설정 오류처럼 폴링 자체가 불가능한 경우에만 쓴다.
	Metrics(ctx context.Context, t Target) (*metric.Set, error)

	// Logs는 로그 항목과 쿼리 통계를 조회한다.
	//
	// 개별 소스의 실패는 에러가 아니라 Result.Sources의 Available=false와 Hint로
	// 표현한다. 로그는 DB 설정에 크게 의존하므로(슬로우 로그 비활성, 확장 미설치,
	// 권한 부족) "왜 볼 수 없는지"를 알려주는 것이 결과만큼 중요하다.
	// 에러는 접속조차 불가능한 경우에만 반환한다.
	Logs(ctx context.Context, t Target, f *dblog.Filter) (*dblog.Result, error)

	// ExecDDL은 마이그레이션 DDL을 순서대로 실행한다.
	//
	// 반환된 error는 "실행을 시작조차 못한 경우"(접속 실패, 미지원)만을 뜻한다.
	// 문장 실행 실패는 error가 아니라 ExecReport에 담긴다 — 어디까지 적용됐는지가
	// 실패 자체보다 중요한 정보이기 때문이다.
	ExecDDL(ctx context.Context, t Target, stmts []string, opts ExecOptions) (*ExecReport, error)

	// Redacted는 자격증명을 가린 접속 문자열을 반환한다. 로그/UI 표시용이다.
	Redacted(t Target) string
}

// registry는 종류별 어댑터를 보관한다. 각 어댑터 파일의 init()에서 등록한다.
var registry = map[model.DBKind]Adapter{}

func register(a Adapter) { registry[a.Kind()] = a }

// Get은 DB 종류에 맞는 어댑터를 반환한다.
func Get(kind model.DBKind) (Adapter, error) {
	a, ok := registry[kind]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedKind, kind)
	}
	return a, nil
}

// KindInfo는 프론트엔드가 커넥션 등록 폼을 그리는 데 필요한 메타데이터다.
type KindInfo struct {
	Kind         model.DBKind `json:"kind"`
	Label        string       `json:"label"`
	DefaultPort  int          `json:"defaultPort"`
	Capabilities Capabilities `json:"capabilities"`
	NeedsHost    bool         `json:"needsHost"`
	NeedsDB      bool         `json:"needsDb"`
	DBLabel      string       `json:"dbLabel"`
	OptionHints  []OptionHint `json:"optionHints"`

	// TableOptions·DatabaseOptions는 이 DB에서 정할 수 있는 저장 설정이다
	// (표의 엔진·문자셋, 데이터베이스의 인코딩 등).
	//
	// 커넥션 폼의 OptionHints와 다른 것이다: 저쪽은 "이 서버에 어떻게 접속하는가"고
	// 이쪽은 "이 서버에 무엇을 만드는가"다. 한 목록으로 합치면 커넥션 폼에 엔진
	// 칸이 생긴다.
	TableOptions    []schema.OptionSpec `json:"tableOptions,omitempty"`
	DatabaseOptions []schema.OptionSpec `json:"databaseOptions,omitempty"`
}

// OptionHint는 DB별 부가 옵션 입력 필드를 설명한다.
//
// Choices가 있으면 화면은 자유 입력 대신 선택 목록을 그린다. 값의 집합이 정해져 있는
// 옵션(예: MongoDB의 인증 방식)은 오타가 접속 시점에야 드러나므로, 고를 수 있는 것을
// 처음부터 보여주는 편이 낫다. 빈 값(=서버 기본값)은 화면이 항상 첫 항목으로 넣는다.
type OptionHint struct {
	Key         string   `json:"key"`
	Label       string   `json:"label"`
	Placeholder string   `json:"placeholder,omitempty"`
	Help        string   `json:"help,omitempty"`
	Choices     []string `json:"choices,omitempty"`
}

// Kinds는 UI에 노출할 DB 종류 목록을 반환한다.
func Kinds() []KindInfo {
	out := []KindInfo{}
	for _, k := range model.SupportedKinds() {
		a, err := Get(k)
		if err != nil {
			continue
		}
		info := KindInfo{
			Kind:         k,
			Label:        kindLabels[k],
			DefaultPort:  a.DefaultPort(),
			Capabilities: a.Capabilities(),
			NeedsHost:    k != model.KindSQLite,
			// 스토리지·브로커에는 "데이터베이스"에 해당하는 것이 없다.
			// 클러스터 하나가 대상 전부다.
			NeedsDB:     !k.IsStorage() && !k.IsBroker(),
			DBLabel:     dbLabels[k],
			OptionHints: optionHints[k],
			// 방언 이름은 DB 종류와 같은 문자열이다(mysql, postgres, ...).
			TableOptions:    schema.TableOptionSpecs(string(k)),
			DatabaseOptions: schema.DatabaseOptionSpecs(string(k)),
		}
		out = append(out, info)
	}
	return out
}

var kindLabels = map[model.DBKind]string{
	model.KindMySQL:      "MySQL / MariaDB",
	model.KindClickHouse: "ClickHouse",
	model.KindQdrant:     "Qdrant (벡터)",
	model.KindPinecone:   "Pinecone (벡터)",
	model.KindS3:         "S3 호환 오브젝트 스토리지",
	model.KindPostgres:   "PostgreSQL",
	model.KindMSSQL:      "Microsoft SQL Server",
	model.KindOracle:     "Oracle",
	model.KindSQLite:     "SQLite",
	model.KindMongoDB:    "MongoDB",
	model.KindRedis:      "Redis",
	model.KindHadoop:     "Apache Hadoop (HDFS)",
	model.KindCeph:       "Ceph",
	model.KindRabbitMQ:   "RabbitMQ",
	model.KindKafka:      "Apache Kafka",
}

var dbLabels = map[model.DBKind]string{
	model.KindMySQL:      "데이터베이스",
	model.KindPostgres:   "데이터베이스",
	model.KindMSSQL:      "데이터베이스",
	model.KindOracle:     "서비스명 / SID",
	model.KindSQLite:     "파일 경로",
	model.KindClickHouse: "데이터베이스",
	// Qdrant 는 서버 하나가 대상 전부다(컬렉션이 그 아래에 있고, 화면이 고른다).
	model.KindQdrant: "",
	// Pinecone 은 인덱스가 곧 대상이다 — 주소도 인덱스마다 다르다.
	model.KindPinecone: "인덱스",
	model.KindMongoDB:  "데이터베이스",
	model.KindRedis:    "DB 인덱스 (0-15)",
	// 스토리지에는 "데이터베이스"에 해당하는 것이 없다. 클러스터 하나가 대상 전부다.
	model.KindHadoop: "",
	model.KindCeph:   "",
	model.KindS3:     "",
	// 브로커도 마찬가지다. 클러스터 하나가 대상 전부다.
	model.KindRabbitMQ: "",
	model.KindKafka:    "",
}

var optionHints = map[model.DBKind][]OptionHint{
	model.KindMySQL: {
		{Key: "tls", Label: "TLS", Placeholder: "false | true | skip-verify | preferred"},
		{
			Key: "timezone", Label: "세션 시간대", Placeholder: "Asia/Seoul 또는 +09:00",
			Help: "이 커넥션으로 여는 세션의 time_zone입니다. 비우면 서버 기본값을 씁니다",
		},
		{
			Key: "charset", Label: "세션 문자셋", Placeholder: "utf8mb4",
			Help: "값을 주고받을 때 쓰는 문자셋입니다. 표를 만들 때의 문자셋과는 다른 것입니다",
		},
		{Key: "params", Label: "추가 파라미터", Placeholder: "charset=utf8mb4&parseTime=true"},
	},
	model.KindPostgres: {
		{Key: "sslmode", Label: "SSL 모드", Placeholder: "disable | require | verify-full"},
		{Key: "search_path", Label: "search_path", Placeholder: "public"},
		{
			Key: "timezone", Label: "세션 시간대", Placeholder: "Asia/Seoul",
			Help: "이 커넥션으로 여는 세션의 TimeZone입니다. 비우면 서버 기본값을 씁니다",
		},
		{
			Key: "client_encoding", Label: "클라이언트 인코딩", Placeholder: "UTF8",
			Help: "값을 주고받을 때 쓰는 인코딩입니다. 데이터베이스의 인코딩과는 다른 것입니다",
		},
	},
	model.KindClickHouse: {
		{Key: "secure", Label: "TLS", Choices: []string{"false", "true"},
			Help: "ClickHouse Cloud 는 켜야 합니다(기본 포트 9440)"},
		{Key: "cluster", Label: "클러스터 이름", Placeholder: "default",
			Help: "적어 두면 DDL 에 ON CLUSTER 가 붙습니다. 단일 노드면 비워 두세요"},
		{Key: "timezone", Label: "세션 시간대", Placeholder: "Asia/Seoul"},
	},
	model.KindQdrant: {
		{Key: "scheme", Label: "프로토콜", Choices: []string{"http", "https"}},
		{
			Key: "api_key", Label: "API 키", Help: "Qdrant Cloud 는 필수입니다. 자체 호스팅에서 인증을 켜지 않았다면 비워 두세요",
		},
		{
			Key: "insecure", Label: "인증서 검증 생략", Choices: []string{"false", "true"},
			Help: "자체 서명 인증서를 쓰는 사내 서버에서만 켜세요",
		},
	},
	model.KindPinecone: {
		{
			Key: "index_host", Label: "인덱스 호스트",
			Placeholder: "my-index-abc123.svc.us-east-1-aws.pinecone.io",
			Help:        "인덱스마다 주소가 다릅니다. 콘솔의 Index 화면에 적힌 host 를 그대로 넣으세요",
		},
		{Key: "namespace", Label: "네임스페이스", Help: "비우면 기본 네임스페이스를 봅니다"},
	},
	model.KindS3: {
		{Key: "region", Label: "리전", Placeholder: "ap-northeast-2",
			Help: "AWS 는 필수입니다. MinIO 처럼 리전이 없는 서버는 비워 두세요"},
		{
			Key: "addressing", Label: "주소 방식", Choices: []string{"path", "virtual"},
			Help: "path 는 host/버킷/키, virtual 은 버킷.host/키 입니다. MinIO·자체 호스팅은 path 를 쓰세요",
		},
		{Key: "scheme", Label: "프로토콜", Choices: []string{"https", "http"}},
		{Key: "session_token", Label: "세션 토큰", Help: "STS 임시 자격증명을 쓸 때만 넣습니다"},
		{
			Key: "insecure", Label: "인증서 검증 생략", Choices: []string{"false", "true"},
			Help: "자체 서명 인증서를 쓰는 사내 서버에서만 켜세요",
		},
	},
	model.KindMSSQL: {
		{Key: "encrypt", Label: "암호화", Placeholder: "disable | true | false"},
		{Key: "trust_server_certificate", Label: "서버 인증서 신뢰", Placeholder: "true | false"},
		{Key: "instance", Label: "인스턴스명", Placeholder: "SQLEXPRESS"},
	},
	model.KindOracle: {
		{Key: "connect_type", Label: "접속 방식", Placeholder: "service | sid", Help: "기본값 service"},
		{
			Key: "owner", Label: "스키마 소유자", Placeholder: "접속 계정",
			Help: "다른 계정이 소유한 테이블을 볼 때 지정합니다. 비우면 접속 계정의 스키마를 봅니다",
		},
		{Key: "ssl", Label: "SSL", Placeholder: "true | false"},
	},
	model.KindHadoop: {
		{
			Key: "yarn_url", Label: "YARN 주소", Placeholder: "http://resourcemanager:8088",
			Help: "비우면 HDFS만 봅니다. 넣으면 실행 중인 애플리케이션과 자원 사용률도 함께 봅니다",
		},
		{Key: "scheme", Label: "프로토콜", Choices: []string{"http", "https"}},
		{
			Key: "insecure", Label: "인증서 검증 생략", Choices: []string{"false", "true"},
			Help: "자체 서명 인증서를 쓰는 사내 클러스터에서만 켜세요",
		},
	},
	model.KindCeph: {
		{Key: "scheme", Label: "프로토콜", Choices: []string{"https", "http"},
			Help: "대시보드는 기본이 HTTPS입니다"},
		{
			Key: "insecure", Label: "인증서 검증 생략", Choices: []string{"false", "true"},
			Help: "자체 서명 인증서를 쓰는 사내 클러스터에서만 켜세요",
		},
	},
	model.KindRabbitMQ: {
		{Key: "scheme", Label: "프로토콜", Choices: []string{"http", "https"},
			Help: "관리 플러그인은 기본이 HTTP입니다"},
		{
			Key: "insecure", Label: "인증서 검증 생략", Choices: []string{"false", "true"},
			Help: "자체 서명 인증서를 쓰는 사내 클러스터에서만 켜세요",
		},
	},
	model.KindKafka: {
		{
			Key: "brokers", Label: "추가 시드 브로커", Placeholder: "broker2:9092,broker3:9092",
			Help: "콤마로 구분한 추가 브로커 주소. 첫 브로커가 죽어 있어도 다른 시드로 시작합니다",
		},
		{
			Key: "sasl", Label: "SASL 인증", Choices: []string{"", "plain", "scram-sha-256", "scram-sha-512"},
			Help: "비우면 익명 접속. 브로커가 요구하는 방식을 고르세요",
		},
		{Key: "tls", Label: "TLS", Choices: []string{"false", "true"},
			Help: "브로커가 TLS 리스너를 쓸 때 켜세요"},
		{
			Key: "insecure", Label: "인증서 검증 생략", Choices: []string{"false", "true"},
			Help: "자체 서명 인증서를 쓰는 사내 클러스터에서만 켜세요",
		},
	},
	model.KindMongoDB: {
		{Key: "uri", Label: "전체 URI", Placeholder: "mongodb+srv://...", Help: "입력 시 호스트/포트 대신 이 값을 사용합니다"},
		{Key: "auth_source", Label: "인증 DB", Placeholder: "admin"},
		{
			Key: "auth_mechanism", Label: "인증 방식", Choices: MongoAuthMechanisms,
			Help: "비워두면 서버가 정한 기본값(대개 SCRAM-SHA-256)을 씁니다",
		},
		{
			Key: "auth_mechanism_properties", Label: "인증 방식 속성",
			Placeholder: "SERVICE_NAME:mongodb,CANONICALIZE_HOST_NAME:true",
			Help:        "GSSAPI·MONGODB-AWS·OIDC에서 쓰는 추가 속성. 콤마로 구분한 KEY:VALUE",
		},
		{Key: "replica_set", Label: "레플리카셋", Placeholder: "rs0"},
		{Key: "tls", Label: "TLS", Placeholder: "true | false"},
	},
	model.KindRedis: {
		{Key: "tls", Label: "TLS", Placeholder: "true | false"},
		{Key: "cluster", Label: "클러스터 모드", Placeholder: "true | false"},
	},
	model.KindSQLite: {
		{Key: "readonly", Label: "읽기 전용", Placeholder: "true | false"},
	},
}
