// Package model은 앱 전역에서 공유되는 엔티티와 열거형을 정의한다.
package model

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
)

// ---------- 시스템 역할 ----------

type Role string

const (
	RoleSuperadmin Role = "superadmin" // 사용자 관리 + 모든 설정
	RoleAdmin      Role = "admin"      // 커넥션 등록/승인, 사용자 관리 불가
	RoleMember     Role = "member"     // 부여된 범위만
)

func (r Role) Valid() bool {
	switch r {
	case RoleSuperadmin, RoleAdmin, RoleMember:
		return true
	}
	return false
}

// CanManageUsers는 사용자 계정을 생성/수정/삭제할 수 있는지 반환한다.
func (r Role) CanManageUsers() bool { return r == RoleSuperadmin }

// CanManageConnections는 DB 커넥션을 등록/수정할 수 있는지 반환한다.
func (r Role) CanManageConnections() bool { return r == RoleSuperadmin || r == RoleAdmin }

type UserStatus string

const (
	UserActive   UserStatus = "active"
	UserDisabled UserStatus = "disabled"
)

// ---------- 접근 범위 / 능력 등급 ----------

// AccessMode는 사용자가 어떤 커넥션에 접근 가능한지 결정하는 방식이다.
type AccessMode string

const (
	AccessAll       AccessMode = "all"       // 모든 DB 접근 가능
	AccessAllowlist AccessMode = "allowlist" // 선택한 DB에만 접근 가능
	AccessDenylist  AccessMode = "denylist"  // 특정 DB만 접근 불가
)

func (m AccessMode) Valid() bool {
	switch m {
	case AccessAll, AccessAllowlist, AccessDenylist:
		return true
	}
	return false
}

// Level은 접근 가능한 커넥션에 대해 무엇까지 할 수 있는지를 나타낸다.
// none < monitor < erd < migrate 순으로 포함 관계를 가진다.
type Level string

const (
	LevelNone    Level = "none"
	LevelMonitor Level = "monitor" // 상태/부하/로그 조회
	LevelERD     Level = "erd"     // ERD 설계, 초안 저장, 리뷰 요청
	LevelMigrate Level = "migrate" // 승인된 마이그레이션 실행/롤백
)

var levelRank = map[Level]int{
	LevelNone:    0,
	LevelMonitor: 1,
	LevelERD:     2,
	LevelMigrate: 3,
}

func (l Level) Valid() bool { _, ok := levelRank[l]; return ok }

// Rank는 비교용 정수를 반환한다. 알 수 없는 값은 0(none)으로 취급한다.
func (l Level) Rank() int { return levelRank[l] }

// Includes는 이 등급이 need 등급을 포함하는지 반환한다.
func (l Level) Includes(need Level) bool { return l.Rank() >= need.Rank() }

// ---------- 커넥션 ----------

type DBKind string

const (
	KindMySQL    DBKind = "mysql"
	KindPostgres DBKind = "postgres"
	KindMSSQL    DBKind = "mssql"
	KindOracle   DBKind = "oracle"
	KindSQLite   DBKind = "sqlite"
	KindMongoDB  DBKind = "mongodb"
	KindRedis    DBKind = "redis"

	// 분산 스토리지. DB는 아니지만 커넥션으로 등록해 관리·감시한다.
	//
	// 같은 표에 두는 이유: 접근 권한·자격증명 보관·지표 수집·이벤트·알림이 모두 커넥션
	// 단위로 돌아간다. 스토리지만 따로 두면 그 여섯 가지를 한 벌 더 만들어야 하고,
	// 사용자는 "권한을 어디서 주는가"를 대상마다 다시 배워야 한다.
	KindHadoop DBKind = "hadoop"
	KindCeph   DBKind = "ceph"

	// 메시지 브로커. DB도 스토리지도 아니지만 커넥션으로 등록해 관리·감시한다.
	// 스토리지와 같은 이유로 커넥션 표에 둔다 — 권한·자격증명·지표·이벤트·알림이
	// 모두 커넥션 단위로 돌아가기 때문이다.
	KindRabbitMQ DBKind = "rabbitmq"
	KindKafka    DBKind = "kafka"
)

// SupportedKinds는 실제로 접속 가능한 DB 종류 목록이다.
func SupportedKinds() []DBKind {
	return []DBKind{KindMySQL, KindPostgres, KindMSSQL, KindOracle, KindSQLite, KindMongoDB, KindRedis,
		KindHadoop, KindCeph, KindRabbitMQ, KindKafka}
}

// IsStorage는 이 종류가 분산 스토리지 클러스터인지 여부다.
//
// 화면과 핸들러가 "DB가 아닌 것"을 판별하는 한 곳이다. 종류 이름을 여기저기서 비교하면
// 종류가 하나 늘 때마다 그 비교를 전부 찾아 고쳐야 한다.
func (k DBKind) IsStorage() bool {
	return k == KindHadoop || k == KindCeph
}

// IsBroker는 이 종류가 메시지 브로커인지 여부다.
//
// 스토리지와 같은 이유로 한 곳에 둔다: 화면은 이 값을 보고 DB 메뉴 대신
// 브로커 화면을 열고, 핸들러는 이 값을 보고 브로커 클라이언트를 고른다.
func (k DBKind) IsBroker() bool {
	return k == KindRabbitMQ || k == KindKafka
}

func (k DBKind) Valid() bool { return slices.Contains(SupportedKinds(), k) }

type Environment string

const (
	EnvDev  Environment = "dev"
	EnvProd Environment = "prod"
)

func (e Environment) Valid() bool { return e == EnvDev || e == EnvProd }

// ---------- 엔티티 ----------

type User struct {
	ID                 string     `json:"id"`
	Username           string     `json:"username"`
	Email              string     `json:"email"`
	DisplayName        string     `json:"displayName"`
	Role               Role       `json:"role"`
	Status             UserStatus `json:"status"`
	MustChangePassword bool       `json:"mustChangePassword"`
	LastLoginAt        *time.Time `json:"lastLoginAt"`
	// LastLoginIP는 마지막 로그인 시점의 접속 IP다.
	// 세션은 만료되면 사라지므로 그 시점의 IP를 사용자 행에 따로 남긴다.
	LastLoginIP string `json:"lastLoginIp,omitempty"`
	// Avatar는 프로필 아이콘 키다. 빈 값이면 화면이 이니셜을 그린다.
	// 특수값 AvatarUpload("upload")는 업로드한 이미지를 쓴다는 표시이며,
	// 실제 바이트는 user_avatars 테이블에 있다.
	Avatar string `json:"avatar,omitempty"`
	// AvatarVersion은 업로드 이미지가 바뀔 때마다 올라간다.
	// 이미지 URL에 붙여 브라우저 캐시를 무효화하는 용도다 — 경로가 그대로면
	// 사진을 바꿔도 예전 것이 계속 보인다.
	AvatarVersion int `json:"avatarVersion,omitempty"`
	// Perms는 커넥션에 매이지 않는 전역 권한이다(매크로 사용, 셸 실행).
	Perms []Perm `json:"perms"`
	// TOTPEnabled는 2단계 인증 등록을 **마쳤는지**다(시작만 한 상태는 거짓).
	//
	// 사용자 행에 두지 않고 조회 때마다 채우는 이유: 실제 값은 user_totp 테이블에 있고,
	// 두 곳에 같은 사실을 적으면 언젠가 한쪽만 갱신된다. 그 어긋남은 "2FA가 켜져 있다고
	// 표시되는데 로그인에서는 묻지 않는" 형태로 나타나며, 그것은 최악의 실패다.
	TOTPEnabled bool `json:"totpEnabled"`

	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
	CreatedBy string    `json:"createdBy,omitempty"`

	// PasswordHash는 절대 JSON으로 나가지 않는다.
	PasswordHash string `json:"-"`
}

type Session struct {
	ID         string    `json:"id"`
	UserID     string    `json:"userId"`
	CreatedAt  time.Time `json:"createdAt"`
	ExpiresAt  time.Time `json:"expiresAt"`
	LastSeenAt time.Time `json:"lastSeenAt"`
	IP         string    `json:"ip"`
	UserAgent  string    `json:"userAgent"`
}

// Server는 DB 서버 하나다. 접속 정보와 자격증명을 갖고, 그 아래에 관리 대상 DB
// (Connection)가 여러 개 달린다.
//
// 커넥션에서 이것을 뽑아낸 이유: 같은 서버의 DB 세 개를 관리하려면 같은 자격증명을
// 세 번 등록해야 했고, 비밀번호를 바꾸면 세 곳을 고쳐야 했다. 자격증명은 한 곳에 있어야
// 한다 — 여러 벌이면 언젠가 한 벌만 갱신되고, 그 사실은 접속 실패로만 드러난다.
type Server struct {
	ID string `json:"id"`
	// ProjectID는 이 서버가 속한 프로젝트다.
	//
	// 프로젝트마다 서버를 따로 등록한다. 접속 정보와 자격증명이 서버에 붙어 있어서,
	// 같은 호스트라도 팀이 다르면 계정이 다르고 그래서 등록도 따로 하게 된다.
	// 그 아래 DB의 프로젝트도 여기서 나온다 — 근거는 하나여야 한다.
	ProjectID   string  `json:"projectId"`
	ProjectName string  `json:"projectName,omitempty"`
	Name        string  `json:"name"`
	Kind        DBKind  `json:"kind"`
	Host        string  `json:"host"`
	Port        int     `json:"port"`
	Options     Options `json:"options"`
	// DefaultEnvironment는 이 서버에 DB를 추가할 때의 기본값이다.
	// 환경 자체는 DB(Connection)마다 정한다 — 이유는 0016 마이그레이션에 적어 두었다.
	DefaultEnvironment Environment `json:"defaultEnvironment"`
	Tags               []string    `json:"tags"`
	Note               string      `json:"note"`
	Enabled            bool        `json:"enabled"`

	Username  string    `json:"username"` // 표시용. 비밀번호는 포함하지 않는다.
	CreatedBy string    `json:"createdBy,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`

	// DatabaseCount는 목록 화면이 쓰는 조인 결과다.
	DatabaseCount int `json:"databaseCount"`
}

// Connection은 관리 대상 DB 하나다. 접속 정보(Kind·Host·Port·Options·Username)는
// 소속 서버에서 채워진다 — 이 구조체를 쓰는 쪽은 서버가 생긴 것을 몰라도 된다.
type Connection struct {
	ID           string      `json:"id"`
	Name         string      `json:"name"`
	Kind         DBKind      `json:"kind"`
	Environment  Environment `json:"environment"`
	Host         string      `json:"host"`
	Port         int         `json:"port"`
	DatabaseName string      `json:"databaseName"`
	Options      Options     `json:"options"`
	Tags         []string    `json:"tags"`
	Note         string      `json:"note"`
	Enabled      bool        `json:"enabled"`

	LastCheckAt  *time.Time `json:"lastCheckAt"`
	LastCheckOK  *bool      `json:"lastCheckOk"`
	LastCheckMsg string     `json:"lastCheckMsg"`

	Username  string    `json:"username"` // 표시용. 비밀번호는 포함하지 않는다.
	CreatedBy string    `json:"createdBy,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`

	// ProjectID/ProjectName은 이 DB가 속한 프로젝트다.
	//
	// 서버(물리적 위치)와는 다른 축이다. 한 서버에 여러 프로젝트의 DB가 함께 있을
	// 수 있고, 한 프로젝트의 DB가 여러 서버에 흩어져 있을 수도 있다.
	ProjectID   string `json:"projectId"`
	ProjectName string `json:"projectName,omitempty"`

	// ServerID/ServerName은 이 DB가 속한 서버다.
	ServerID   string `json:"serverId"`
	ServerName string `json:"serverName"`
	// SelfEnabled/ServerEnabled는 Enabled를 이루는 두 스위치다.
	//
	// Enabled 자체는 **실효값**(둘 다 켜져야 참)이다. 이렇게 둔 이유는 이미 앱 곳곳에
	// `if !conn.Enabled { 거부 }` 형태의 관문이 있기 때문이다 — 실효값을 넣어야 서버를
	// 끄는 순간 그 관문들이 전부 옳게 동작한다. 수정 화면만 두 값을 따로 본다.
	SelfEnabled   bool `json:"selfEnabled"`
	ServerEnabled bool `json:"serverEnabled"`

	// NodeID는 이 DB에 접속할 클러스터 노드다. 비어 있으면 요청을 받은 노드가 직접
	// 접속한다. 사설망 안에 있어 특정 서버에서만 닿는 DB를 위해 있다.
	NodeID string `json:"nodeId,omitempty"`
}

// Secret은 복호화된 자격증명이다. API 응답에 절대 포함하지 않는다.
type Secret struct {
	Username string
	Password string
	Extra    map[string]string
}

// Options는 DB별 부가 접속 옵션이다. 스키마를 고정하지 않고 JSON으로 저장한다.
// 예: sslmode, service_name, tls, auth_source, db_index, params
type Options map[string]string

func (o Options) Get(key string) string { return o[key] }

func (o Options) GetOr(key, def string) string {
	if v, ok := o[key]; ok && v != "" {
		return v
	}
	return def
}

func (o Options) MarshalDB() (string, error) {
	if o == nil {
		return "{}", nil
	}
	b, err := json.Marshal(o)
	if err != nil {
		return "", fmt.Errorf("marshal options: %w", err)
	}
	return string(b), nil
}

func UnmarshalOptions(s string) Options {
	if s == "" {
		return Options{}
	}
	var o Options
	if err := json.Unmarshal([]byte(s), &o); err != nil || o == nil {
		return Options{}
	}
	return o
}

// AccessPolicy는 한 사용자의 접근 범위와 능력 등급 설정 전체다.
type AccessPolicy struct {
	UserID       string           `json:"userId"`
	Mode         AccessMode       `json:"mode"`
	DefaultLevel Level            `json:"defaultLevel"`
	Items        []string         `json:"items"`        // allowlist/denylist 대상 connection ID
	Capabilities map[string]Level `json:"capabilities"` // connection ID → 등급 오버라이드

	// DefaultCaps는 오버라이드가 없는 커넥션에 적용되는 데이터 능력이다.
	DefaultCaps []Capability `json:"defaultCaps"`
	// CapOverrides는 커넥션별 데이터 능력 오버라이드다.
	//
	// Level 오버라이드와 별도의 맵인 이유: 두 축은 독립적으로 지정된다.
	// "이 DB만 데이터 수정 허용"과 "이 DB만 마이그레이션 허용"은 서로를 함의하지
	// 않으므로, 한쪽을 지정하려고 다른 쪽까지 적어야 하면 실수로 권한이 넓어진다.
	CapOverrides map[string][]Capability `json:"capOverrides"`

	// ---- 서버 단위 ----
	//
	// 서버 하나에 DB가 열 개면 커넥션마다 체크박스를 열 번 눌러야 하고, DB를 추가할
	// 때마다 모든 사용자의 권한을 다시 챙겨야 한다. 그 부담은 결국 "그냥 전체 허용"으로
	// 이어져 권한 모델을 무력화한다. 그래서 일괄 부여를 얹되, 아래 DB 단위 지정은
	// 그대로 살려 둔다 — **좁은 쪽이 이긴다**(커넥션 > 서버 > 기본값).
	ServerItems        []string                `json:"serverItems"`
	ServerCapabilities map[string]Level        `json:"serverCapabilities"`
	ServerCapOverrides map[string][]Capability `json:"serverCapOverrides"`

	// ---- 프로젝트 ----
	//
	// 위의 두 층보다 앞에 오는 **관문**이다. 참여하지 않은 프로젝트의 DB는 등급이
	// 무엇으로 적혀 있든 보이지 않는다.
	//
	// 등급과 합치지 않고 목록 하나로 둔 이유: 프로젝트는 "무엇을 할 수 있는가"가
	// 아니라 "무엇이 내 일인가"다. 여기에 등급을 얹으면 같은 것을 두 곳에서 정하게
	// 되고, 두 곳이 어긋나면 어느 쪽이 참인지 화면이 답하지 못한다.
	Projects []string `json:"projects"`

	UpdatedAt time.Time `json:"updatedAt"`
}

// Scope는 판정에 필요한 대상 정보다.
//
// 커넥션 ID만으로는 서버 단위 설정을 찾을 수 없어 소속 서버까지 받는다.
// model.Connection을 통째로 받지 않는 이유: 판정에 쓰이는 것은 이 두 값뿐이고,
// 그래야 판정 함수가 순수 함수로 남아 시험하기 쉽다.
type Scope struct {
	ConnectionID string
	ServerID     string
	// ProjectID는 판정의 첫 관문이다. 비어 있으면 어느 프로젝트에 속하는지 알 수
	// 없다는 뜻이고, 그때는 참여 여부를 확인할 방법이 없으므로 막는다.
	ProjectID string
}

func (c *Connection) Scope() Scope {
	if c == nil {
		return Scope{}
	}
	return Scope{ConnectionID: c.ID, ServerID: c.ServerID, ProjectID: c.ProjectID}
}

// EffectiveAccess는 특정 커넥션에 대한 판정 결과다.
type EffectiveAccess struct {
	ConnectionID string       `json:"connectionId"`
	Accessible   bool         `json:"accessible"`
	Level        Level        `json:"level"`
	Caps         []Capability `json:"caps"`
	Reason       string       `json:"reason"`
}

// AuditEntry는 감사 로그 한 줄이다.
type AuditEntry struct {
	ID         int64          `json:"id"`
	At         time.Time      `json:"at"`
	ActorID    string         `json:"actorId"`
	ActorName  string         `json:"actorName"`
	Action     string         `json:"action"`
	TargetType string         `json:"targetType"`
	TargetID   string         `json:"targetId"`
	Detail     map[string]any `json:"detail"`
	IP         string         `json:"ip"`
	Result     string         `json:"result"`
}

// TagsToString / TagsFromString은 콤마 구분 저장 형식과 슬라이스를 변환한다.
func TagsToString(tags []string) string {
	cleaned := make([]string, 0, len(tags))
	for _, t := range tags {
		t = strings.TrimSpace(t)
		if t != "" {
			cleaned = append(cleaned, t)
		}
	}
	return strings.Join(cleaned, ",")
}

func TagsFromString(s string) []string {
	if strings.TrimSpace(s) == "" {
		return []string{}
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
