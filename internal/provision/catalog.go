// Package provision은 "어떤 DB 를 어떻게 띄우는가"를 담는다.
//
// ── 왜 카탈로그가 따로 있는가 ───────────────────────────────────────
// 컨테이너를 띄우는 것과 **DB 를 띄우는 것**은 다른 일이다. PostgreSQL 은
// POSTGRES_PASSWORD 없이는 아예 뜨지 않고, MySQL 은 그 자리에 다른 이름을
// 쓰며, ClickHouse 는 사용자 이름을 주면 기본 계정을 지운다. 데이터가 어느
// 경로에 쌓이는지도 저마다 다르다.
//
// 그 지식을 화면에 두면 화면이 DB 마다 분기하게 되고, 서버에 흩어 두면 어느
// 값이 어디로 가는지 아무도 한눈에 못 본다. 그래서 **한 곳에 표로** 둔다 —
// DB 하나를 더하는 일이 이 파일에 항목 하나를 더하는 일이 되도록.
//
// ── 값이 어디로 가는가 ──────────────────────────────────────────────
// 설정 하나가 컨테이너에 닿는 길은 셋이다: 환경변수, 실행 인자, 설정 파일.
// 어느 길인지를 필드가 스스로 말한다(Target). 그래야 화면은 "이 DB 에 정할 수
// 있는 것들"만 알면 되고, 그것을 어떻게 넘기는지는 몰라도 된다.
package provision

import (
	"fmt"
	"sort"
	"strings"

	"dbstudio/internal/model"
)

// Target은 설정값이 컨테이너에 닿는 길이다.
type Target string

const (
	// TargetEnv는 환경변수로 넘긴다.
	TargetEnv Target = "env"
	// TargetArg는 실행 인자로 넘긴다(--max_connections=200 꼴).
	TargetArg Target = "arg"
	// TargetFile은 설정 파일에 넣는다.
	//
	// 파일은 컨테이너를 **만든 뒤 시작하기 전에** 넣는다. 시작한 뒤에 넣으면
	// 이미 읽고 지나간 뒤다. ClickHouse 의 메모리 설정이 이 길로 간다.
	TargetFile Target = "file"
	// TargetMeta는 컨테이너에 넘기지 않고 우리가 쓰는 값이다
	// (호스트 포트, 볼륨을 둘지, 자원 상한 …).
	TargetMeta Target = "meta"
)

// Apply는 만든 뒤에 이 값을 고쳤을 때 무엇이 필요한지다.
//
// ── 왜 필드마다 따로 적는가 ─────────────────────────────────────────
// 짐작할 수 없어서다. 살아 있는 컨테이너로 일곱 종류를 다 재 보니 **DB 마다
// 달랐다.** 비밀번호를 고치고 컨테이너를 다시 만들었을 때:
//
//	PostgreSQL · MySQL · MariaDB · MongoDB · MS-SQL  →  안 바뀐다(옛 것이 남는다)
//	ClickHouse · Redis                               →  바뀐다
//
// 앞의 다섯은 첫 실행에서만 계정을 만들고, 그 뒤로는 데이터에 든 것이 진짜다.
// 그것을 모르고 "고쳤습니다"라고 말하면 사람은 바뀐 줄 알고 새 비밀번호로
// 접속을 시도하다 막힌다 — 그리고 무엇이 잘못됐는지 알 길이 없다.
type Apply string

const (
	// ApplyLive는 컨테이너를 그대로 두고 바꾼다(도커가 해 준다).
	ApplyLive Apply = "live"
	// ApplyRestart는 설정 파일을 다시 넣고 재시작해야 먹는다.
	ApplyRestart Apply = "restart"
	// ApplyRecreate는 컨테이너를 다시 만들어야 먹는다(데이터는 남는다).
	ApplyRecreate Apply = "recreate"
	// ApplyInitOnly는 **처음 만들 때 한 번만** 쓰인다.
	//
	// 다시 만들어도 이미 있는 데이터에는 적용되지 않는다. 고칠 수 있게 두되,
	// 고쳐도 지금 DB 는 그대로라는 것을 화면이 말해야 한다.
	ApplyInitOnly Apply = "init"
)

// FieldKind는 화면이 무엇으로 그릴지다.
type FieldKind string

const (
	KindText     FieldKind = "text"
	KindPassword FieldKind = "password"
	KindNumber   FieldKind = "number"
	KindSelect   FieldKind = "select"
	KindBool     FieldKind = "bool"
)

// Field는 사람이 정할 수 있는 값 하나다.
type Field struct {
	Key   string    `json:"key"`
	Label string    `json:"label"`
	Kind  FieldKind `json:"kind"`
	// Help는 "이 값이 무엇인가"다. 뻔한 것에는 붙이지 않는다.
	Help        string   `json:"help,omitempty"`
	Placeholder string   `json:"placeholder,omitempty"`
	Default     string   `json:"default,omitempty"`
	Choices     []string `json:"choices,omitempty"`
	Required    bool     `json:"required,omitempty"`
	// Min·Max는 숫자 칸의 범위다(둘 다 0 이면 재지 않는다).
	Min int `json:"min,omitempty"`
	Max int `json:"max,omitempty"`

	// Target·Name은 이 값이 어디로 가는지다.
	//
	// 화면은 이 둘을 쓰지 않는다. 서버가 계획을 만들 때만 본다 — 화면이
	// 환경변수 이름을 알면 그 이름이 두 곳에 있게 되고, 이미지를 바꿀 때
	// 한쪽만 고치게 된다.
	Target Target `json:"-"`
	Name   string `json:"-"`

	// Apply는 만든 뒤에 이 값을 고쳤을 때 무엇이 필요한지다.
	//
	// 비워 두면 목적지에서 뽑는다(applyOf). 목적지만으로 정할 수 없는 것들은
	// (계정·비밀번호처럼 DB 마다 다른 것) 레시피가 직접 적는다.
	Apply Apply `json:"apply,omitempty"`
	// Secret이면 값을 로그·감사 기록에 남기지 않는다.
	Secret bool `json:"secret,omitempty"`
	// Hidden이면 화면에 그리지 않는다.
	//
	// 왜 필요한가: 같은 값을 두 길로 보내야 하는 경우가 있다(Redis 의 비밀번호는
	// 인자로 가고, 헬스체크의 redis-cli 를 위해 환경변수로도 간다). 칸을 둘
	// 그리면 사람이 같은 것을 두 번 적게 된다.
	Hidden bool `json:"hidden,omitempty"`
	// Advanced면 화면이 "자세한 설정" 아래로 접어 둔다.
	//
	// 접는 기준: 이것을 정하지 않아도 쓸 수 있는 DB 가 뜨는가. 뜨면 접는다 —
	// 처음 만드는 사람에게 스무 개의 칸을 한 번에 보여 주면 어느 것이 꼭
	// 필요한지 알 수 없다.
	Advanced bool `json:"advanced,omitempty"`
}

// Recipe는 DB 한 종류를 띄우는 방법이다.
type Recipe struct {
	// ID는 이 레시피의 신분이다. Kind 와 **다르다.**
	//
	// 왜 갈라야 하는가: MariaDB 는 MySQL 프로토콜을 말하므로 Kind 가 mysql 이다.
	// 그런데 이미지도 환경변수 이름도 다르다(MARIADB_ROOT_PASSWORD). Kind 로
	// 레시피를 찾으면 mysql 로 찾은 것이 MySQL 이 되고 MariaDB 는 영영 못 찾는다 —
	// 실제 데몬에 돌려 본 검사가 "mariadb 는 모르는 DB 종류입니다"로 잡았다.
	ID string `json:"id"`
	// Kind는 이 앱이 아는 DB 종류다. 만든 컨테이너를 커넥션으로 등록할 때 쓴다.
	Kind model.DBKind `json:"kind"`
	// Label은 화면에 보일 이름이다.
	Label string `json:"label"`
	// Blurb는 한 줄 설명이다("어떤 DB 인가"가 아니라 "언제 고르는가").
	Blurb string `json:"blurb"`
	// Image는 이미지 이름이다(태그 없이).
	Image string `json:"image"`
	// Versions는 고를 수 있는 태그다. 첫 번째가 기본값이다.
	Versions []string `json:"versions"`
	// Port는 컨테이너 안에서 DB 가 듣는 포트다.
	Port int `json:"port"`
	// DataPath는 데이터가 쌓이는 경로다. 볼륨을 여기 붙인다.
	//
	// 빈 문자열이면 붙일 자리가 없다는 뜻이다(Kafka 처럼 우리가 아직 다루지
	// 않는 것). 그때는 화면이 "데이터가 남지 않는다"고 말해야 한다.
	DataPath string `json:"dataPath"`
	// Fields는 이 DB 에 정할 수 있는 것들이다.
	Fields []Field `json:"fields"`
	// Health는 헬스체크 명령이다. %s 자리에 비밀번호가 들어간다면 Secret 을 쓴다.
	Health []string `json:"-"`

	// AdminUser·UserField는 만든 DB 에 붙을 계정이다.
	//
	// 뜬 컨테이너를 커넥션으로 등록할 때 쓴다. 둘로 나뉘는 이유: 사람이 정하는
	// 것(PostgreSQL·MongoDB)과 이미지가 정해 둔 것(MySQL 의 root, ClickHouse 의
	// default, MS-SQL 의 sa)이 있다. 하나로 합치면 어느 쪽인지를 등록하는 쪽이
	// 다시 판단해야 하고, 그 판단이 여기와 어긋나면 붙을 수 없는 커넥션이 된다.
	AdminUser string `json:"-"`
	UserField string `json:"-"`
	// Notes는 이 DB 를 띄울 때 사람이 알아야 하는 것이다.
	Notes []string `json:"notes,omitempty"`
}

// 공통 필드.
//
// 여기 모아 두는 이유: 이름·포트·볼륨·자원은 DB 마다 다르지 않다. 레시피마다
// 적어 두면 여덟 벌이 되고, 하나를 고칠 때 일곱을 빠뜨린다.
func commonFields(defaultPort int) []Field {
	return []Field{
		{
			Key: "hostPort", Label: "호스트 포트", Kind: KindNumber,
			Help: fmt.Sprintf("이 기계에서 접속할 포트입니다. 비워 두면 도커가 빈 포트를 골라 줍니다"+
				" (컨테이너 안에서는 늘 %d 입니다)", defaultPort),
			Min: 0, Max: 65535, Target: TargetMeta, Name: "hostPort",
		},
		{
			Key: "persist", Label: "데이터를 남긴다", Kind: KindBool, Default: "true",
			Help:   "이름 붙인 볼륨에 데이터를 둡니다. 끄면 컨테이너를 지울 때 데이터도 함께 사라집니다",
			Target: TargetMeta, Name: "persist",
		},
		{
			Key: "memoryMB", Label: "메모리 상한 (MB)", Kind: KindNumber,
			Help: "0 이면 제한하지 않습니다. 라즈베리파이처럼 작은 기계에서는 반드시 정하세요 — " +
				"DB 하나가 기계를 다 먹으면 DB Studio 자신도 함께 죽습니다",
			Min: 0, Max: 262144, Target: TargetMeta, Name: "memoryMB", Advanced: true,
		},
		{
			Key: "cpus", Label: "CPU 상한 (코어)", Kind: KindText, Placeholder: "1.5",
			Help: "0 이거나 비워 두면 제한하지 않습니다", Target: TargetMeta, Name: "cpus",
			Advanced: true,
		},
		{
			Key: "restart", Label: "다시 시작 정책", Kind: KindSelect,
			Choices: []string{"unless-stopped", "always", "on-failure", "no"},
			Default: "unless-stopped",
			Help:    "기계를 다시 켤 때 이 DB 도 함께 뜨게 하려면 unless-stopped 를 고르세요",
			Target:  TargetMeta, Name: "restart", Advanced: true,
		},
	}
}

// Catalog는 띄울 수 있는 DB 목록이다.
//
// 순서가 화면 순서다. 많이 쓰는 것을 앞에 둔다 — 알파벳 순으로 두면 처음
// 쓰는 사람이 목록 전체를 읽어야 한다.
func Catalog() []*Recipe {
	out := []*Recipe{postgres(), mysql(), mariadb(), mongo(), redis(), clickhouse(), mssql()}
	// 빈 Apply 를 여기서 채운다.
	//
	// 비워 두면 JSON 에서 빠지고(omitempty), 화면은 그것을 보고 스스로 짐작하게
	// 된다 — 그러면 같은 규칙이 서버와 화면 두 곳에 생기고, 실제로 어긋났다:
	// 메모리 상한이 화면에서 "다시 만들기 필요"로 보였다(진짜는 "바로 적용").
	// 규칙은 서버 한 곳에 두고, 나가는 값에는 언제나 답이 들어 있게 한다.
	for _, r := range out {
		for i := range r.Fields {
			r.Fields[i].Apply = ApplyOf(&r.Fields[i])
		}
	}
	return out
}

// Find는 레시피 ID 로 찾는다.
//
// Kind 로 찾지 않는 이유는 Recipe.ID 의 주석에 있다 — 한 Kind 에 레시피가
// 둘일 수 있다(MySQL·MariaDB).
func Find(id string) *Recipe {
	for _, r := range Catalog() {
		if strings.EqualFold(r.ID, id) {
			return r
		}
	}
	return nil
}

// Field는 열쇠로 필드를 찾는다.
func (r *Recipe) Field(key string) *Field {
	for i := range r.Fields {
		if r.Fields[i].Key == key {
			return &r.Fields[i]
		}
	}
	return nil
}

// ApplyOf는 이 필드를 고쳤을 때 무엇이 필요한지다.
//
// 레시피가 적어 둔 것이 있으면 그것을 쓰고, 없으면 목적지에서 뽑는다.
// 목적지로 정해지는 것들은 DB 와 무관하게 같기 때문이다 — 실행 인자는 만들 때
// 정해지므로 다시 만들어야 하고, 설정 파일은 넣고 재시작하면 읽힌다.
func ApplyOf(f *Field) Apply {
	if f.Apply != "" {
		return f.Apply
	}
	switch f.Target {
	case TargetFile:
		return ApplyRestart
	case TargetMeta:
		switch f.Key {
		case "memoryMB", "cpus", "restart":
			// 도커가 컨테이너를 그대로 두고 바꿔 준다.
			return ApplyLive
		default: // hostPort, persist
			return ApplyRecreate
		}
	default: // env, arg
		return ApplyRecreate
	}
}

// AdminAccount는 이 DB 에 붙을 계정 이름이다.
//
// 빈 문자열은 "계정이 없다"는 뜻이다(Redis 는 비밀번호만 받는다). 그때 아무
// 이름을 만들어 넣으면 접속이 실패하고, 실패 이유가 "계정이 틀렸다"로 보인다.
func (r *Recipe) AdminAccount(values map[string]string) string {
	if r.UserField != "" {
		if v := strings.TrimSpace(values[r.UserField]); v != "" {
			return v
		}
		// 사람이 비워 두면 필드의 기본값이 곧 이미지의 기본값이다.
		if f := r.Field(r.UserField); f != nil {
			return f.Default
		}
	}
	return r.AdminUser
}

// DefaultVersion은 기본 태그다.
func (r *Recipe) DefaultVersion() string {
	if len(r.Versions) == 0 {
		return "latest"
	}
	return r.Versions[0]
}

// ---------- 레시피 ----------

func postgres() *Recipe {
	return &Recipe{
		ID:        "postgres",
		UserField: "username",
		Kind:      model.KindPostgres, Label: "PostgreSQL",
		Blurb:    "표준에 가깝고 확장이 많습니다. 특별한 이유가 없으면 이것.",
		Image:    "postgres",
		Versions: []string{"17-alpine", "16-alpine", "15-alpine", "17", "16"},
		Port:     5432, DataPath: "/var/lib/postgresql/data",
		Health: []string{"CMD-SHELL", "pg_isready -U ${POSTGRES_USER} -d ${POSTGRES_DB}"},
		Fields: append([]Field{
			{
				Key: "database", Apply: ApplyInitOnly, Label: "데이터베이스 이름", Kind: KindText,
				Default: "appdb", Required: true,
				Target: TargetEnv, Name: "POSTGRES_DB",
			},
			{
				Key: "username", Apply: ApplyInitOnly, Label: "계정", Kind: KindText, Default: "postgres",
				Required: true, Target: TargetEnv, Name: "POSTGRES_USER",
			},
			{
				Key: "password", Apply: ApplyInitOnly, Label: "비밀번호", Kind: KindPassword, Required: true,
				Help:   "비워 두면 만들 수 없습니다. PostgreSQL 이 비밀번호 없이는 뜨지 않습니다",
				Secret: true, Target: TargetEnv, Name: "POSTGRES_PASSWORD",
			},
			{
				Key: "encoding", Apply: ApplyInitOnly, Label: "인코딩", Kind: KindSelect,
				Choices: []string{"UTF8", "SQL_ASCII", "LATIN1"}, Default: "UTF8",
				Target: TargetEnv, Name: "POSTGRES_INITDB_ARGS_ENCODING", Advanced: true,
				Help: "만들 때 한 번만 정해집니다. 뒤에 바꾸려면 데이터를 옮겨야 합니다",
			},
			{
				Key: "maxConnections", Label: "최대 접속 수", Kind: KindNumber,
				Default: "100", Min: 10, Max: 10000,
				Target: TargetArg, Name: "max_connections", Advanced: true,
			},
			{
				Key: "sharedBuffers", Label: "shared_buffers", Kind: KindText,
				Placeholder: "128MB",
				Help:        "보통 메모리의 1/4 입니다. 비워 두면 이미지 기본값(128MB)을 씁니다",
				Target:      TargetArg, Name: "shared_buffers", Advanced: true,
			},
		}, commonFields(5432)...),
		Notes: []string{
			"데이터베이스·계정·인코딩은 **처음 만들 때 한 번만** 정해집니다. " +
				"볼륨이 비어 있지 않으면 그 값들은 무시됩니다.",
		},
	}
}

func mysql() *Recipe {
	return &Recipe{
		ID:        "mysql",
		AdminUser: "root",
		Kind:      model.KindMySQL, Label: "MySQL",
		Blurb:    "가장 널리 쓰입니다. 기존 시스템과 맞춰야 할 때.",
		Image:    "mysql",
		Versions: []string{"8.4", "8.0", "8.4-oracle"},
		Port:     3306, DataPath: "/var/lib/mysql",
		Health: []string{"CMD-SHELL",
			"mysqladmin ping -h 127.0.0.1 -u root -p\"$MYSQL_ROOT_PASSWORD\" --silent"},
		Fields: append([]Field{
			{
				Key: "database", Apply: ApplyInitOnly, Label: "데이터베이스 이름", Kind: KindText,
				Default: "appdb", Required: true, Target: TargetEnv, Name: "MYSQL_DATABASE",
			},
			{
				Key: "password", Apply: ApplyInitOnly, Label: "root 비밀번호", Kind: KindPassword, Required: true,
				Secret: true, Target: TargetEnv, Name: "MYSQL_ROOT_PASSWORD",
			},
			{
				Key: "appUser", Apply: ApplyInitOnly, Label: "앱 계정 (선택)", Kind: KindText,
				Help:   "적어 두면 그 계정을 함께 만들고 위 데이터베이스의 권한을 줍니다",
				Target: TargetEnv, Name: "MYSQL_USER", Advanced: true,
			},
			{
				Key: "appPassword", Apply: ApplyInitOnly, Label: "앱 계정 비밀번호", Kind: KindPassword,
				Secret: true, Target: TargetEnv, Name: "MYSQL_PASSWORD", Advanced: true,
			},
			{
				Key: "charset", Apply: ApplyInitOnly, Label: "문자셋", Kind: KindSelect,
				Choices: []string{"utf8mb4", "utf8mb3", "latin1"}, Default: "utf8mb4",
				Target: TargetArg, Name: "character-set-server", Advanced: true,
			},
			{
				Key: "collation", Apply: ApplyInitOnly, Label: "정렬 규칙", Kind: KindText,
				Placeholder: "utf8mb4_0900_ai_ci",
				Target:      TargetArg, Name: "collation-server", Advanced: true,
			},
			{
				Key: "maxConnections", Label: "최대 접속 수", Kind: KindNumber,
				Default: "151", Min: 10, Max: 100000,
				Target: TargetArg, Name: "max_connections", Advanced: true,
			},
			{
				Key: "innodbBufferPoolMB", Label: "InnoDB 버퍼 풀 (MB)", Kind: KindNumber,
				Min: 0, Max: 262144,
				Help:   "0 이면 이미지 기본값(128MB). 보통 메모리의 절반 이상을 줍니다",
				Target: TargetArg, Name: "innodb-buffer-pool-size", Advanced: true,
			},
		}, commonFields(3306)...),
		Notes: []string{
			"MySQL 은 처음 뜰 때 데이터 디렉터리를 만들어 **한 번은 느립니다**(수십 초). " +
				"헬스체크가 통과할 때까지 기다리세요.",
		},
	}
}

func mariadb() *Recipe {
	return &Recipe{
		ID:        "mariadb",
		AdminUser: "root",
		Kind:      model.KindMySQL, Label: "MariaDB",
		Blurb:    "MySQL 과 프로토콜이 같습니다. 라즈베리파이처럼 작은 기계에서 더 가볍습니다.",
		Image:    "mariadb",
		Versions: []string{"11.4", "11.8", "10.11"},
		Port:     3306, DataPath: "/var/lib/mysql",
		Health: []string{"CMD-SHELL", "healthcheck.sh --connect --innodb_initialized"},
		Fields: append([]Field{
			{
				Key: "database", Apply: ApplyInitOnly, Label: "데이터베이스 이름", Kind: KindText,
				Default: "appdb", Required: true, Target: TargetEnv, Name: "MARIADB_DATABASE",
			},
			{
				Key: "password", Apply: ApplyInitOnly, Label: "root 비밀번호", Kind: KindPassword, Required: true,
				Secret: true, Target: TargetEnv, Name: "MARIADB_ROOT_PASSWORD",
			},
			{
				Key: "appUser", Apply: ApplyInitOnly, Label: "앱 계정 (선택)", Kind: KindText,
				Target: TargetEnv, Name: "MARIADB_USER", Advanced: true,
			},
			{
				Key: "appPassword", Apply: ApplyInitOnly, Label: "앱 계정 비밀번호", Kind: KindPassword,
				Secret: true, Target: TargetEnv, Name: "MARIADB_PASSWORD", Advanced: true,
			},
			{
				Key: "charset", Apply: ApplyInitOnly, Label: "문자셋", Kind: KindSelect,
				Choices: []string{"utf8mb4", "utf8mb3"}, Default: "utf8mb4",
				Target: TargetArg, Name: "character-set-server", Advanced: true,
			},
			{
				Key: "maxConnections", Label: "최대 접속 수", Kind: KindNumber,
				Default: "151", Min: 10, Max: 100000,
				Target: TargetArg, Name: "max_connections", Advanced: true,
			},
		}, commonFields(3306)...),
	}
}

func mongo() *Recipe {
	return &Recipe{
		ID:        "mongodb",
		UserField: "username",
		Kind:      model.KindMongoDB, Label: "MongoDB",
		Blurb:    "문서를 그대로 담습니다. 스키마가 자주 바뀌는 것.",
		Image:    "mongo",
		Versions: []string{"8", "7", "6"},
		Port:     27017, DataPath: "/data/db",
		Health: []string{"CMD-SHELL",
			"mongosh --quiet --eval 'db.adminCommand({ping:1}).ok' | grep -q 1"},
		Fields: append([]Field{
			{
				Key: "username", Apply: ApplyInitOnly, Label: "root 계정", Kind: KindText, Default: "root",
				Required: true, Target: TargetEnv, Name: "MONGO_INITDB_ROOT_USERNAME",
			},
			{
				Key: "password", Apply: ApplyInitOnly, Label: "root 비밀번호", Kind: KindPassword, Required: true,
				Secret: true, Target: TargetEnv, Name: "MONGO_INITDB_ROOT_PASSWORD",
			},
			{
				Key: "database", Apply: ApplyInitOnly, Label: "데이터베이스 이름", Kind: KindText, Default: "appdb",
				Help:   "이 이름으로 초기화 스크립트가 도는 자리를 정합니다",
				Target: TargetEnv, Name: "MONGO_INITDB_DATABASE",
			},
			{
				Key: "wiredTigerCacheGB", Label: "WiredTiger 캐시 (GB)", Kind: KindText,
				Placeholder: "0.5",
				Help: "비워 두면 (메모리의 절반 - 1GB)입니다. 작은 기계에서는 그 계산이 " +
					"음수가 되어 기본 최소값으로 떨어지므로 직접 정하는 편이 낫습니다",
				Target: TargetArg, Name: "wiredTigerCacheSizeGB", Advanced: true,
			},
		}, commonFields(27017)...),
		Notes: []string{
			"레플리카셋은 만들지 않습니다(단일 노드). 트랜잭션이 필요하면 " +
				"레플리카셋으로 띄워야 하는데, 그것은 노드가 여럿인 이야기라 여기서 다루지 않습니다.",
		},
	}
}

func redis() *Recipe {
	return &Recipe{
		ID:   "redis",
		Kind: model.KindRedis, Label: "Redis",
		Blurb:    "메모리에 두는 캐시·큐. 빠르고 단순합니다.",
		Image:    "redis",
		Versions: []string{"7-alpine", "8-alpine", "7"},
		Port:     6379, DataPath: "/data",
		// redis-cli 는 REDISCLI_AUTH 환경변수를 스스로 읽는다(아래 숨은 필드).
		//
		// 인증을 빼먹었다가 실제 데몬에서 잡혔다. requirepass 를 걸면
		// `redis-cli ping` 이 NOAUTH 를 돌려주므로 영원히 starting 이었다 —
		// DB 는 멀쩡히 돌고 있는데 화면에는 "뜨는 중"만 남는다.
		//
		// 비밀번호를 명령에 박지 않는 이유: 그러면 컨테이너 안에서 `ps` 만 해도
		// 보인다. 환경변수는 docker inspect 로만 보인다.
		Health: []string{"CMD-SHELL", "redis-cli ping | grep -q PONG"},
		Fields: append([]Field{
			{
				Key: "password", Apply: ApplyRecreate, Label: "비밀번호", Kind: KindPassword, Required: true,
				Help:   "Redis 는 계정이 없고 비밀번호만 있습니다",
				Secret: true, Target: TargetArg, Name: "requirepass",
			},
			{
				// 같은 값을 환경변수로도 보낸다. 헬스체크의 redis-cli 가 이것을
				// 읽는다(위 Health 주석). 화면에는 그리지 않는다.
				Key: "password", Kind: KindPassword,
				Secret: true, Target: TargetEnv, Name: "REDISCLI_AUTH", Hidden: true,
			},
			{
				Key: "maxmemoryMB", Label: "최대 메모리 (MB)", Kind: KindNumber,
				Min: 0, Max: 262144,
				Help: "0 이면 제한하지 않습니다. 캐시로 쓸 거라면 반드시 정하세요 — " +
					"제한이 없으면 기계의 메모리를 다 먹을 때까지 자랍니다",
				Target: TargetArg, Name: "maxmemory", Advanced: true,
			},
			{
				Key: "maxmemoryPolicy", Label: "메모리가 찼을 때", Kind: KindSelect,
				Choices: []string{"noeviction", "allkeys-lru", "volatile-lru", "allkeys-lfu"},
				Default: "noeviction",
				Help: "noeviction 은 쓰기를 거절합니다(큐로 쓸 때). 캐시로 쓸 거라면 " +
					"allkeys-lru 를 고르세요",
				Target: TargetArg, Name: "maxmemory-policy", Advanced: true,
			},
			{
				Key: "appendonly", Label: "AOF 로 기록", Kind: KindBool, Default: "true",
				Help:   "끄면 다시 시작할 때 데이터가 사라집니다(캐시로만 쓸 때)",
				Target: TargetArg, Name: "appendonly", Advanced: true,
			},
		}, commonFields(6379)...),
	}
}

func clickhouse() *Recipe {
	return &Recipe{
		ID:        "clickhouse",
		AdminUser: "default",
		Kind:      model.KindClickHouse, Label: "ClickHouse",
		Blurb:    "열 지향. 로그·이벤트를 쌓아 집계하는 것.",
		Image:    "clickhouse/clickhouse-server",
		Versions: []string{"24.8", "25.3", "latest"},
		Port:     9000, DataPath: "/var/lib/clickhouse",
		Health: []string{"CMD-SHELL", "clickhouse-client --password \"$CLICKHOUSE_PASSWORD\" --query 'SELECT 1'"},
		Fields: append([]Field{
			{
				Key: "database", Apply: ApplyInitOnly, Label: "데이터베이스 이름", Kind: KindText,
				Default: "appdb", Required: true, Target: TargetEnv, Name: "CLICKHOUSE_DB",
			},
			{
				Key: "password", Apply: ApplyRecreate, Label: "default 계정 비밀번호", Kind: KindPassword,
				Required: true, Secret: true,
				Target: TargetEnv, Name: "CLICKHOUSE_PASSWORD",
			},
			{
				Key: "accessManagement", Label: "SQL 로 권한 관리", Kind: KindBool,
				Default: "false",
				Help:    "켜면 SQL 로 사용자·권한을 만들 수 있습니다(CREATE USER)",
				Target:  TargetEnv, Name: "CLICKHOUSE_DEFAULT_ACCESS_MANAGEMENT", Advanced: true,
			},
			{
				Key: "markCacheMB", Label: "마크 캐시 (MB)", Kind: KindNumber,
				Min: 0, Max: 65536,
				Help: "기본값이 5GiB 입니다. 작은 기계에서는 전체 메모리보다 커서 " +
					"상한을 두지 않은 것과 같으므로 반드시 낮추세요",
				Target: TargetFile, Name: "mark_cache_size", Advanced: true,
			},
			{
				Key: "maxMemoryRatio", Label: "서버가 쓸 메모리 비율", Kind: KindText,
				Placeholder: "0.5",
				Help: "0.5 면 기계 메모리의 절반까지 씁니다. 같은 기계에 다른 것이 " +
					"돌고 있으면 낮추세요",
				Target: TargetFile, Name: "max_server_memory_usage_to_ram_ratio", Advanced: true,
			},
			{
				Key: "maxQueryMemoryMB", Label: "질의 하나의 메모리 상한 (MB)", Kind: KindNumber,
				Min: 0, Max: 262144,
				Help: "넘으면 그 질의만 실패하고 서버는 삽니다. 상한이 없으면 무거운 " +
					"조회 하나가 서버째로 죽습니다",
				Target: TargetFile, Name: "max_memory_usage", Advanced: true,
			},
		}, commonFields(9000)...),
		Notes: []string{
			"파일 핸들 상한을 올려 줍니다(nofile 262144). 기본값으로는 뜨다가 죽는 " +
				"호스트가 있고, 그 오류 메시지는 원인을 짐작하기 어렵습니다.",
			"**이미지가 ARMv8.2-A 를 요구합니다.** 라즈베리파이 4·3 에서는 " +
				"`Illegal instruction` 으로 죽습니다 — 그 기계에서는 호환 빌드가 필요합니다.",
		},
	}
}

func mssql() *Recipe {
	return &Recipe{
		ID:        "mssql",
		AdminUser: "sa",
		Kind:      model.KindMSSQL, Label: "MS-SQL Server",
		Blurb:    "윈도 쪽 시스템과 맞춰야 할 때.",
		Image:    "mcr.microsoft.com/mssql/server",
		Versions: []string{"2022-latest", "2019-latest"},
		Port:     1433, DataPath: "/var/opt/mssql",
		Health: []string{"CMD-SHELL",
			"/opt/mssql-tools18/bin/sqlcmd -S localhost -U sa -P \"$MSSQL_SA_PASSWORD\" -C -Q 'SELECT 1'"},
		Fields: append([]Field{
			{
				Key: "password", Apply: ApplyInitOnly, Label: "sa 비밀번호", Kind: KindPassword, Required: true,
				Help: "대문자·소문자·숫자·기호를 섞은 8자 이상이어야 합니다. " +
					"약하면 컨테이너가 뜨자마자 죽습니다",
				Secret: true, Target: TargetEnv, Name: "MSSQL_SA_PASSWORD",
			},
			{
				Key: "edition", Label: "에디션", Kind: KindSelect,
				Choices: []string{"Developer", "Express", "Standard", "Enterprise"},
				Default: "Developer",
				Help:    "Developer 와 Express 는 무료입니다. 나머지는 라이선스가 있어야 합니다",
				Target:  TargetEnv, Name: "MSSQL_PID",
			},
			{
				Key: "collation", Apply: ApplyInitOnly, Label: "정렬 규칙", Kind: KindText,
				Placeholder: "Korean_Wansung_CI_AS",
				Help:        "만들 때 한 번만 정해집니다",
				Target:      TargetEnv, Name: "MSSQL_COLLATION", Advanced: true,
			},
			{
				Key: "memoryLimitMB", Label: "SQL Server 메모리 상한 (MB)", Kind: KindNumber,
				Min: 0, Max: 262144,
				Help: "0 이면 정하지 않습니다. 컨테이너 메모리 상한과 따로이므로 " +
					"둘을 함께 정하는 편이 낫습니다",
				Target: TargetEnv, Name: "MSSQL_MEMORY_LIMIT_MB", Advanced: true,
			},
		}, commonFields(1433)...),
		Notes: []string{
			"**데이터베이스를 만들어 주지 않습니다.** 이미지가 그 기능을 제공하지 않아서, " +
				"뜬 뒤에 SQL 콘솔에서 `CREATE DATABASE` 를 해야 합니다.",
			"메모리를 2GB 이상 줘야 뜹니다. 라즈베리파이에서는 어렵습니다.",
		},
	}
}

// 카탈로그가 아는 종류.
//
// 여기 없는 것을 일부러 적어 둔다. "왜 없는가"를 사람이 묻기 전에 답하려는
// 것이고, 나중에 우리가 다시 판단할 때 그때의 이유가 남아 있어야 한다.
var notInCatalog = map[string]string{
	"oracle": "이미지를 자유롭게 내려받을 수 없습니다(계정과 라이선스 동의가 필요합니다). " +
		"직접 올린 이미지가 있다면 도커에서 띄우고 커넥션으로 등록하세요.",
	"sqlite": "서버가 없는 DB 입니다. 띄울 것이 없습니다.",
	"kafka": "브로커는 리스너 설정이 노드 구성에 딸려 있어(광고 주소), 단일 노드에서도 " +
		"고를 것이 DB 보다 많습니다. 뒤 단계에서 다룹니다.",
	"rabbitmq": "브로커입니다. 카프카와 같은 이유로 뒤 단계에서 다룹니다.",
}

// NotInCatalog는 왜 그 종류가 목록에 없는지다(있으면 빈 문자열).
func NotInCatalog(kind string) string {
	return notInCatalog[strings.ToLower(strings.TrimSpace(kind))]
}

// NotInCatalogAll은 못 만드는 것들과 그 이유다.
//
// 화면에 함께 내려보낸다. 목록에 없는 것을 사람이 찾다 못 찾으면 "지원하지
// 않는다"와 "아직 안 만들었다"를 구분할 수 없고, 그 구분이 없으면 같은 질문이
// 계속 들어온다.
func NotInCatalogAll() map[string]string {
	out := make(map[string]string, len(notInCatalog))
	for k, v := range notInCatalog {
		out[k] = v
	}
	return out
}

// CatalogKinds는 목록에 있는 종류를 정렬해 돌려준다(검사·문서용).
func CatalogKinds() []string {
	seen := map[string]bool{}
	out := []string{}
	for _, r := range Catalog() {
		k := string(r.Kind)
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
