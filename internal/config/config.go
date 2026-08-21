// Package config는 플래그와 환경변수에서 앱 설정을 읽어온다.
package config

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Addr       string        // 리스닝 주소 (예: ":8080")
	DataDir    string        // 메타 DB, 마스터 키 저장 위치
	DevMode    bool          // true면 web/을 디스크에서 서빙 (핫 리로드)
	WebDir     string        // DevMode에서 사용할 프론트엔드 경로
	MasterKey  string        // base64 마스터 키. 비어있으면 DataDir/master.key 사용
	SessionTTL time.Duration // 세션 유효기간
	// TrustProxy는 X-Forwarded-For를 클라이언트 IP로 쓸지 여부다.
	// TrustedProxies에 있는 상대가 보낸 헤더만 신뢰한다 — 그러지 않으면
	// 앱에 직접 닿을 수 있는 누구나 자기 IP를 위조할 수 있다.
	TrustProxy     bool
	TrustedProxies []string
	SecureCookie   bool // Secure 쿠키 플래그 (HTTPS 배포 시)

	// 모니터링
	MonitorEnabled      bool          // false면 폴러를 시작하지 않는다
	MonitorInterval     time.Duration // 지표 수집 주기
	SchemaCheckInterval time.Duration // 스키마 드리프트 확인 주기
	MetricRetention     time.Duration // 원본 지표 보존기간

	// 호스트(=이 앱이 도는 컴퓨터) 감시
	//
	// 임계값을 여기에 두지 않은 이유: "디스크 몇 %에서 알릴 것인가"는 운영 중에
	// 바뀌는 값이라 설정 화면에서 고칠 수 있어야 한다(app_settings에 저장한다).
	// 여기 있는 것은 프로세스를 띄울 때 정해지는 것뿐이다.
	HostMonitorEnabled bool          // false면 호스트 지표를 수집하지 않는다
	HostInterval       time.Duration // 호스트 지표 수집 주기
	HostSyslog         string        // 읽을 시스템 로그 (리눅스: 파일 경로, 윈도우: 이벤트 로그 채널)

	// 로그
	//
	// 파일 로그가 기본으로 켜져 있는 이유: 터미널을 닫거나 서비스로 띄운 뒤에는
	// stderr가 사라지고, 그러면 "왜 멈췄는지"를 확인할 방법이 없어진다.
	LogLevel  string // debug | info | warn | error
	LogFormat string // text | json
	LogFile   string // 빈 문자열이면 파일 로그를 쓰지 않는다. "-"면 기본 경로
	LogMaxMB  int    // 이 크기를 넘으면 .1로 밀어내고 새로 시작한다

	// BackupCmd는 운영 DB 마이그레이션 전에 실행할 외부 명령이다.
	//
	// 플래그로만 지정할 수 있게 한 것은 의도적이다. API로 지정하게 하면 이 앱이
	// 임의 명령 실행 통로가 되므로, 운영자가 프로세스를 띄울 때 정하는 값이어야 한다.
	// 인자에는 {name} {kind} {host} {port} {database} {env} {id} 를 쓸 수 있다.
	BackupCmd string

	// 매크로
	//
	// AllowShell은 매크로의 셸 노드를 사용할 수 있게 한다.
	//
	// 사용자 권한(script.run)과 별개로 이 플래그를 둔 이유: 권한 설정은 화면에서
	// 몇 번의 클릭으로 바뀌지만, 이 기능이 켜지는 순간 앱은 원격 셸이 된다.
	// 그런 성격의 변경은 프로세스를 띄우는 사람이 의식적으로 정해야 한다
	// (-backup-cmd를 플래그로만 받는 것과 같은 이유다).
	AllowShell bool
	// ShellTimeout은 셸 노드 하나의 실행 시간 상한이다.
	ShellTimeout time.Duration
	// MacroTimeout은 매크로 실행 전체의 시간 상한이다.
	MacroTimeout time.Duration
	// HTTPTimeout은 매크로의 외부 HTTP 호출 하나의 시간 상한이다.
	HTTPTimeout time.Duration
	// HTTPMaxBodyKB는 응답 본문을 읽어들이는 상한이다.
	// 없으면 큰 파일 하나가 매크로 실행 전체의 메모리를 먹는다.
	HTTPMaxBodyKB int
	// HTTPAllow는 호출을 허용할 호스트/CIDR 목록이다.
	// 비어 있으면 링크로컬(클라우드 메타데이터)을 뺀 모든 주소를 허용한다.
	HTTPAllow []string

	// LuaTimeout은 Lua 노드 하나의 실행 시간 상한이다.
	//
	// 명령 수가 아니라 시간으로 재는 이유: GopherLua에는 명령 수를 세는 훅이 없고,
	// 있더라도 "몇 개의 명령이 정상인가"는 아무도 답할 수 없다. 반면 "한 노드가
	// 1분 넘게 돌면 무언가 잘못됐다"는 판단은 할 수 있다. 무한 루프는 이 상한에서 멈춘다.
	LuaTimeout time.Duration

	// 백업
	//
	// BackupDir는 논리 덤프 파일을 두는 곳이다. 비어 있으면 <data>/backups.
	//
	// 파일로 두는 이유: 덤프는 GB 단위가 될 수 있고 그것을 메타 DB(SQLite)에 넣으면
	// 백업 한 번이 앱 전체를 느리게 만든다. 대신 파일 하나에 행 하나가 대응하도록
	// 하고, 행이 사라지면 파일도 지운다.
	BackupDir string
	// BackupMaxMB는 덤프 하나의 크기 상한이다. 넘으면 **실패로 끝낸다** —
	// 잘린 백업은 없는 백업보다 위험하다(복구할 수 있다고 믿게 만든다).
	BackupMaxMB int
	// BackupRetention은 이 기간이 지난 백업을 자동으로 지운다. 0이면 지우지 않는다.
	BackupRetention time.Duration

	// 클러스터
	//
	// 분산 배치에서 여러 서버의 DB Studio를 하나처럼 다루기 위한 설정이다.
	// 역할과 비밀은 프로세스를 띄울 때 정해진다 — 화면에서 바꿀 수 있게 하면
	// 누군가 실수로 마스터를 둘로 만들 수 있고, 그 순간 진실이 두 개가 된다.
	ClusterRole      string // standalone | master | replica
	ClusterMaster    string // 리플리카가 바라볼 마스터 주소
	ClusterSecret    string // 노드 사이 공용 비밀 (환경변수 권장)
	ClusterNodeName  string // 화면에 보일 이 노드의 이름
	ClusterAdvertise string // 다른 노드가 이 노드를 부를 주소
	ClusterSync      time.Duration
	ClusterHeartbeat time.Duration
	ClusterLogKeep   time.Duration
	ClusterLogMax    int

	// AvatarMaxKB는 업로드 가능한 프로필 이미지의 최대 크기(KB)다.
	AvatarMaxKB int
	// AvatarAllowPrivateURI는 사설망 주소에서도 프로필 이미지를 내려받을지 여부다.
	// 기본값 false — 아바타 가져오기가 내부망 포트 스캐너로 쓰이면 안 된다.
	AvatarAllowPrivateURI bool
}

func (c *Config) MetaDBPath() string  { return filepath.Join(c.DataDir, "dbstudio.db") }
func (c *Config) KeyFilePath() string { return filepath.Join(c.DataDir, "master.key") }

// BackupPath는 덤프 파일을 둘 디렉터리다.
func (c *Config) BackupPath() string {
	if c.BackupDir != "" {
		return c.BackupDir
	}
	return filepath.Join(c.DataDir, "backups")
}

// LogFilePath는 실제로 쓸 로그 파일 경로다. 빈 문자열이면 파일 로그를 쓰지 않는다.
func (c *Config) LogFilePath() string {
	switch c.LogFile {
	case "":
		return ""
	case "-":
		return filepath.Join(c.DataDir, "dbstudio.log")
	default:
		return c.LogFile
	}
}

// Load는 플래그 → 환경변수 → 기본값 순으로 설정을 확정한다.
func Load(args []string) (*Config, error) {
	c := &Config{}
	fs := flag.NewFlagSet("dbstudio", flag.ContinueOnError)
	fs.StringVar(&c.Addr, "addr", env("DBSTUDIO_ADDR", ":8080"), "listen address")
	fs.StringVar(&c.DataDir, "data", env("DBSTUDIO_DATA", "./data"), "data directory")
	fs.BoolVar(&c.DevMode, "dev", envBool("DBSTUDIO_DEV", false), "serve frontend from disk instead of embedded FS")
	fs.StringVar(&c.WebDir, "web", env("DBSTUDIO_WEB", "./web"), "frontend directory (dev mode only)")
	fs.DurationVar(&c.SessionTTL, "session-ttl", envDur("DBSTUDIO_SESSION_TTL", 12*time.Hour), "session lifetime")
	fs.BoolVar(&c.TrustProxy, "trust-proxy", envBool("DBSTUDIO_TRUST_PROXY", false),
		"use X-Forwarded-For as the client IP (only from -trusted-proxies)")
	var trusted string
	fs.StringVar(&trusted, "trusted-proxies", env("DBSTUDIO_TRUSTED_PROXIES", ""),
		"comma-separated IPs/CIDRs allowed to set X-Forwarded-For (default: loopback + private ranges)")
	fs.BoolVar(&c.SecureCookie, "secure-cookie", envBool("DBSTUDIO_SECURE_COOKIE", false), "set Secure flag on session cookie")
	fs.BoolVar(&c.MonitorEnabled, "monitor", envBool("DBSTUDIO_MONITOR", true), "enable metric polling")
	fs.DurationVar(&c.MonitorInterval, "monitor-interval", envDur("DBSTUDIO_MONITOR_INTERVAL", 30*time.Second), "metric collection interval")
	fs.DurationVar(&c.SchemaCheckInterval, "schema-check-interval", envDur("DBSTUDIO_SCHEMA_CHECK_INTERVAL", 15*time.Minute), "schema drift check interval")
	fs.DurationVar(&c.MetricRetention, "metric-retention", envDur("DBSTUDIO_METRIC_RETENTION", 48*time.Hour), "raw metric sample retention")
	fs.BoolVar(&c.HostMonitorEnabled, "host-monitor", envBool("DBSTUDIO_HOST_MONITOR", true),
		"monitor the machine running DB Studio (CPU, memory, disk, network)")
	fs.DurationVar(&c.HostInterval, "host-interval", envDur("DBSTUDIO_HOST_INTERVAL", 30*time.Second),
		"host metric collection interval")
	fs.StringVar(&c.HostSyslog, "host-syslog", env("DBSTUDIO_HOST_SYSLOG", ""),
		"system log to watch for OS errors (linux: file path, windows: event log channel; empty = auto)")
	fs.StringVar(&c.LogLevel, "log-level", env("DBSTUDIO_LOG_LEVEL", "info"), "log level: debug, info, warn, error")
	fs.StringVar(&c.LogFormat, "log-format", env("DBSTUDIO_LOG_FORMAT", "text"), "log format: text or json")
	fs.StringVar(&c.LogFile, "log-file", env("DBSTUDIO_LOG_FILE", "-"),
		"log file path (\"-\" = <data>/dbstudio.log, empty = stderr only)")
	fs.IntVar(&c.LogMaxMB, "log-max-mb", envInt("DBSTUDIO_LOG_MAX_MB", 20), "rotate the log file when it exceeds this size")
	fs.StringVar(&c.BackupCmd, "backup-cmd", env("DBSTUDIO_BACKUP_CMD", ""),
		"command to run before production migrations (placeholders: {name} {kind} {host} {port} {database} {env} {id})")
	fs.BoolVar(&c.AllowShell, "allow-shell", envBool("DBSTUDIO_ALLOW_SHELL", false),
		"allow macros to run bash/powershell scripts (users still need the script.run permission)")
	fs.DurationVar(&c.ShellTimeout, "shell-timeout", envDur("DBSTUDIO_SHELL_TIMEOUT", 2*time.Minute),
		"time limit for a single shell node")
	fs.DurationVar(&c.MacroTimeout, "macro-timeout", envDur("DBSTUDIO_MACRO_TIMEOUT", 15*time.Minute),
		"time limit for one macro run")
	fs.DurationVar(&c.LuaTimeout, "macro-lua-timeout", envDur("DBSTUDIO_MACRO_LUA_TIMEOUT", time.Minute),
		"time limit for a single Lua node (guards against infinite loops)")
	fs.DurationVar(&c.HTTPTimeout, "macro-http-timeout", envDur("DBSTUDIO_MACRO_HTTP_TIMEOUT", 30*time.Second),
		"time limit for one outbound HTTP call from a macro")
	fs.IntVar(&c.HTTPMaxBodyKB, "macro-http-max-kb", envInt("DBSTUDIO_MACRO_HTTP_MAX_KB", 1024),
		"max response body a macro HTTP call will read, in KB")
	var httpAllow string
	fs.StringVar(&httpAllow, "macro-http-allow", env("DBSTUDIO_MACRO_HTTP_ALLOW", ""),
		"comma-separated hosts/CIDRs macros may call (empty = anything except link-local)")
	fs.StringVar(&c.BackupDir, "backup-dir", env("DBSTUDIO_BACKUP_DIR", ""),
		"directory for logical dump files (default: <data>/backups)")
	fs.IntVar(&c.BackupMaxMB, "backup-max-mb", envInt("DBSTUDIO_BACKUP_MAX_MB", 2048),
		"fail a dump that would exceed this size")
	fs.DurationVar(&c.BackupRetention, "backup-retention", envDur("DBSTUDIO_BACKUP_RETENTION", 720*time.Hour),
		"delete backups older than this (0 = keep forever)")
	fs.StringVar(&c.ClusterRole, "cluster-role", env("DBSTUDIO_CLUSTER_ROLE", "standalone"),
		"cluster role: standalone, master, or replica")
	fs.StringVar(&c.ClusterMaster, "cluster-master", env("DBSTUDIO_CLUSTER_MASTER", ""),
		"master URL for a replica (http://host:port)")
	fs.StringVar(&c.ClusterNodeName, "cluster-node-name", env("DBSTUDIO_CLUSTER_NODE_NAME", ""),
		"name of this node in the cluster (default: hostname)")
	fs.StringVar(&c.ClusterAdvertise, "cluster-advertise", env("DBSTUDIO_CLUSTER_ADVERTISE", ""),
		"URL other nodes can reach this node at")
	fs.DurationVar(&c.ClusterSync, "cluster-sync-interval", envDur("DBSTUDIO_CLUSTER_SYNC_INTERVAL", 2*time.Second),
		"how often a replica pulls changes from the master")
	fs.DurationVar(&c.ClusterHeartbeat, "cluster-heartbeat", envDur("DBSTUDIO_CLUSTER_HEARTBEAT", 10*time.Second),
		"how often a node reports that it is alive")
	fs.DurationVar(&c.ClusterLogKeep, "cluster-log-keep", envDur("DBSTUDIO_CLUSTER_LOG_KEEP", 24*time.Hour),
		"how long the master keeps replication log entries")
	fs.IntVar(&c.ClusterLogMax, "cluster-log-max", envInt("DBSTUDIO_CLUSTER_LOG_MAX", 200000),
		"max replication log entries the master keeps")
	fs.IntVar(&c.AvatarMaxKB, "avatar-max-kb", envInt("DBSTUDIO_AVATAR_MAX_KB", 512),
		"max profile image size in KB")
	fs.BoolVar(&c.AvatarAllowPrivateURI, "avatar-allow-private-uri", envBool("DBSTUDIO_AVATAR_ALLOW_PRIVATE_URI", false),
		"allow importing profile images from private/loopback addresses")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	c.MasterKey = os.Getenv("DBSTUDIO_MASTER_KEY")
	// 클러스터 비밀은 플래그로 받지 않는다. 명령줄은 프로세스 목록(ps)에 그대로 보이고,
	// 이 값 하나면 클러스터의 모든 데이터를 받아 갈 수 있다.
	c.ClusterSecret = os.Getenv("DBSTUDIO_CLUSTER_SECRET")
	c.HTTPAllow = splitList(httpAllow)

	// 리버스 프록시는 거의 항상 루프백이나 사설망에 있다. 그것을 기본값으로 두면
	// 흔한 배치에서 추가 설정 없이 동작하고, 공개 IP에서 온 위조 헤더는 무시된다.
	c.TrustedProxies = splitList(trusted)
	if len(c.TrustedProxies) == 0 {
		c.TrustedProxies = defaultTrustedProxies()
	}

	abs, err := filepath.Abs(c.DataDir)
	if err != nil {
		return nil, fmt.Errorf("resolve data dir: %w", err)
	}
	c.DataDir = abs
	if err := os.MkdirAll(c.DataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	return c, nil
}

// defaultTrustedProxies는 루프백과 사설망 대역이다(RFC1918 + 링크로컬 + IPv6 로컬).
func defaultTrustedProxies() []string {
	return []string{
		"127.0.0.0/8", "::1/128",
		"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16",
		"169.254.0.0/16", "fc00::/7", "fe80::/10",
	}
}

func splitList(v string) []string {
	out := []string{}
	for _, part := range strings.Split(v, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envBool(k string, def bool) bool {
	if v := os.Getenv(k); v != "" {
		b, err := strconv.ParseBool(v)
		if err == nil {
			return b
		}
	}
	return def
}

func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		n, err := strconv.Atoi(v)
		if err == nil {
			return n
		}
	}
	return def
}

func envDur(k string, def time.Duration) time.Duration {
	if v := os.Getenv(k); v != "" {
		d, err := time.ParseDuration(v)
		if err == nil {
			return d
		}
	}
	return def
}
