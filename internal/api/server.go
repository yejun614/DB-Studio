// Package api는 HTTP 라우팅과 핸들러를 제공한다.
package api

import (
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"runtime/debug"
	"strings"
	"time"

	"github.com/gofiber/contrib/websocket"
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/compress"
	"github.com/gofiber/fiber/v2/middleware/etag"
	"github.com/gofiber/fiber/v2/middleware/filesystem"
	"github.com/gofiber/fiber/v2/middleware/recover"

	"dbstudio/internal/auth"
	"dbstudio/internal/backup"
	"dbstudio/internal/buildinfo"
	"dbstudio/internal/cluster"
	"dbstudio/internal/config"
	"dbstudio/internal/dbx"
	"dbstudio/internal/erdhub"
	"dbstudio/internal/macro"
	"dbstudio/internal/migrate"
	"dbstudio/internal/model"
	"dbstudio/internal/monitor"
	"dbstudio/internal/notify"
	"dbstudio/internal/store"
)

// Server는 의존성을 묶어 라우터를 구성한다.
type Server struct {
	cfg     *config.Config
	st      *store.Store
	authn   *auth.Service
	authz   *auth.Authorizer
	monitor *monitor.Manager
	// notifier는 이벤트를 메신저로 보내는 전송기다. nil일 수 있다.
	notifier *notify.Notifier
	// hostMon은 이 컴퓨터 자신의 감시자다. nil일 수 있다(호스트 감시를 끈 경우).
	hostMon *monitor.HostMonitor
	// cluster는 이 노드의 클러스터 참여 상태다. nil이면 단일 서버로 동작한다.
	cluster  *cluster.Cluster
	erdHub   *erdhub.Hub
	migrator *migrate.Runner
	macros   *macro.Engine
	backups  *backup.Service
	web      fs.FS
	app      *fiber.App
}

func New(cfg *config.Config, st *store.Store, authn *auth.Service, authz *auth.Authorizer, mon *monitor.Manager, web fs.FS) *Server {
	backups := backup.New(st, backup.Config{
		Dir:       cfg.BackupPath(),
		MaxBytes:  int64(cfg.BackupMaxMB) << 20,
		Retention: cfg.BackupRetention,
	}, slog.Default())

	s := &Server{
		cfg: cfg, st: st, authn: authn, authz: authz, monitor: mon, web: web,
		erdHub:   erdhub.New(st, slog.Default()),
		migrator: migrate.New(st, cfg.BackupCmd, slog.Default()),
		backups:  backups,
		macros: macro.New(st, authz, macro.Config{
			AllowShell:   cfg.AllowShell,
			ShellTimeout: cfg.ShellTimeout,
			RunTimeout:   cfg.MacroTimeout,
			LuaTimeout:   cfg.LuaTimeout,
			HTTP: macro.HTTPConfig{
				Timeout: cfg.HTTPTimeout,
				MaxBody: int64(cfg.HTTPMaxBodyKB) << 10,
				Allow:   cfg.HTTPAllow,
			},
		}, mon, backups, slog.Default()),
	}

	// 호스트 감시자는 나중에 붙인다(SetHostMonitor). 생성자 인자를 늘리지 않은 이유는
	// 이 서버를 만드는 시험이 여럿이고, 그 시험들에 호스트 감시는 아무 상관이 없기 때문이다.

	s.app = fiber.New(fiberConfig(cfg))

	// 패닉을 잡되 **반드시 기록한다.** 기본 설정은 조용히 500을 돌려주므로
	// 서버는 살아 있지만 무엇이 깨졌는지 아무도 모르게 된다.
	s.app.Use(recover.New(recover.Config{
		EnableStackTrace: true,
		StackTraceHandler: func(c *fiber.Ctx, e any) {
			slog.Error("요청 처리 중 패닉",
				"method", c.Method(), "path", c.Path(), "ip", clientIP(c),
				"panic", fmt.Sprint(e), "stack", string(debug.Stack()))
		},
	}))
	s.app.Use(compress.New())
	s.app.Use(securityHeaders)
	s.app.Use(requestLog)

	// 백업 디렉터리는 여기서 만든다. 첫 백업을 누른 뒤에야 "디렉터리를 만들 수 없다"를
	// 알게 되면 늦다 — 그때는 이미 백업이 필요한 상황일 수 있다.
	if err := s.backups.EnsureDir(); err != nil {
		slog.Error("백업 디렉터리를 준비하지 못했습니다. 백업 기능이 실패합니다", "err", err)
	}

	s.routes()
	return s
}

// Backups는 부팅 절차가 정리 작업을 부를 수 있도록 백업 서비스를 노출한다.
func (s *Server) Backups() *backup.Service { return s.backups }

// Macros는 부팅 절차가 자동 실행 스케줄러를 띄울 수 있도록 매크로 엔진을 노출한다.
func (s *Server) Macros() *macro.Engine { return s.macros }

// SetNotifier는 알림 전송기를 붙인다. 붙이지 않으면 설정 화면은 열리되
// "전송기가 꺼져 있습니다"로 답한다 — 설정만 저장되고 아무것도 가지 않는 상태를
// 화면이 성공으로 보여주는 것보다 낫다.
func (s *Server) SetNotifier(n *notify.Notifier) { s.notifier = n }

// SetCluster는 클러스터 참여 상태를 붙인다. 붙이지 않으면 단일 서버로 동작한다
// (쓰기 전달도, 노드 목록도 없다).
func (s *Server) SetCluster(cl *cluster.Cluster) { s.cluster = cl }

// SetHostMonitor는 호스트 감시자를 붙인다. 붙이지 않으면 호스트 화면은
// "수집하고 있지 않음"으로 응답한다 — 빈 그래프를 보여주는 것보다 낫다.
func (s *Server) SetHostMonitor(h *monitor.HostMonitor) { s.hostMon = h }

// fiberConfig는 Fiber 설정을 만든다.
//
// 별도 함수로 둔 이유: 클라이언트 IP 판정은 로그인 IP·감사 로그·세션 기록에 모두
// 쓰이므로 테스트로 고정해야 하고, 테스트가 실제 설정과 다른 값을 쓰면 의미가 없다.
func fiberConfig(cfg *config.Config) fiber.Config {
	return fiber.Config{
		AppName:               "DB Studio",
		DisableStartupMessage: true,
		ErrorHandler:          errorHandler,
		ReadTimeout:           30 * time.Second,
		WriteTimeout:          60 * time.Second,
		BodyLimit:             8 * 1024 * 1024,

		// 프록시 뒤에 있을 때만 X-Forwarded-For를 클라이언트 IP로 쓴다.
		//
		// TrustedProxies를 함께 주지 않으면 Fiber는 아무도 신뢰하지 않아 ProxyHeader가
		// 무시된다 — 플래그를 켜도 프록시의 IP만 기록되는 상태가 된다(조용히 틀린다).
		EnableTrustedProxyCheck: cfg.TrustProxy,
		TrustedProxies:          cfg.TrustedProxies,
		// 없으면 c.IP()가 "client, proxy1" 같은 헤더 값을 그대로 돌려준다.
		// 그 문자열이 IP 칸에 저장되면 기록이 쓸모없어진다.
		EnableIPValidation: true,
		ProxyHeader: func() string {
			if cfg.TrustProxy {
				return fiber.HeaderXForwardedFor
			}
			return ""
		}(),
	}
}

func (s *Server) App() *fiber.App { return s.app }

func (s *Server) Listen() error { return s.app.Listen(s.cfg.Addr) }

func (s *Server) routes() {
	v1 := s.app.Group("/api/v1")

	// 클러스터 미들웨어는 인증보다 **앞**에 온다.
	//
	// 앞에 두는 이유: 리플리카가 넘기는 요청의 권한 판정은 마스터에서 이뤄져야 한다.
	// 여기서 먼저 인증하면 두 노드가 각자 판정하게 되고, 세션이 아직 복제되지 않은
	// 찰나에 "리플리카는 통과시켰는데 마스터는 거부"하는 어긋남이 생긴다.
	v1.Use(s.clusterSeqStamp)
	// 담당 노드 라우팅이 쓰기 전달보다 앞이다. 그 DB에 닿을 수 있는 노드가 먼저
	// 정해져야 하고, 그 노드가 자기 몫의 쓰기를 어떻게 처리할지는 그다음 문제다.
	v1.Use(s.clusterRoute)
	v1.Use(s.clusterForward)

	// 노드끼리 부르는 경로. 사람의 세션이 아니라 공용 비밀로 인증한다.
	//
	// 사람이 보는 /cluster 와 접두어를 나눈 이유: Fiber의 그룹 미들웨어는 그 접두어로
	// 시작하는 **모든** 경로에 걸린다. 같은 접두어를 쓰면 화면이 부르는 /cluster/ 까지
	// 공용 비밀을 요구하게 되고, 그 증상은 "슈퍼 어드민인데 클러스터 화면이 401"이다.
	nodes := v1.Group("/node", s.requireClusterSecret)
	nodes.Post("/join", s.requireMaster, s.handleClusterJoin)
	nodes.Post("/heartbeat", s.requireMaster, s.handleClusterHeartbeat)
	nodes.Get("/changes", s.requireMaster, s.handleClusterChanges)
	nodes.Get("/snapshot", s.requireMaster, s.handleClusterSnapshot)
	nodes.Post("/audit", s.requireMaster, s.handleClusterAudit)

	// 인증 불필요
	v1.Get("/health", s.handleHealth)
	v1.Post("/auth/login", s.handleLogin)
	// 로그인 2단계. 인증 전이므로 여기에 둔다 — 통과의 근거는 세션이 아니라
	// 1단계가 발급한 챌린지 쿠키다.
	v1.Post("/auth/login/totp", s.handleLoginTOTP)
	v1.Post("/auth/logout", s.handleLogout)

	// 인증 필요
	authed := v1.Group("", s.requireAuth)
	authed.Get("/auth/me", s.handleMe)
	authed.Post("/auth/password", s.handleChangeOwnPassword)
	// 자기 프로필은 역할과 무관하게 누구나 수정한다. 대상이 항상 자신이므로 별도 인가가 없다.
	authed.Patch("/auth/profile", s.handleUpdateProfile)
	// 2단계 인증: 자기 계정에 대해서만 켜고 끈다.
	// (남의 것을 초기화하는 경로는 아래 사용자 관리 그룹에 있다.)
	authed.Get("/auth/totp", s.handleTOTPStatus)
	authed.Post("/auth/totp/setup", s.handleTOTPSetup)
	authed.Post("/auth/totp/confirm", s.handleTOTPConfirm)
	authed.Post("/auth/totp/disable", s.handleTOTPDisable)
	authed.Post("/auth/totp/recovery", s.handleTOTPRecoveryCodes)

	// API 토큰: 자기 것만 만들고 지운다.
	authed.Get("/auth/tokens", s.handleListTokens)
	authed.Post("/auth/tokens", s.handleCreateToken)
	// 값만 다시 발급한다(이름·범위·만료는 그대로). 값이 샜을 때 할 일은 대개
	// 토큰을 버리는 것이 아니라 값을 바꾸는 것이다.
	authed.Post("/auth/tokens/:tokenId/rotate", s.handleRotateToken)
	authed.Delete("/auth/tokens/:tokenId", s.handleDeleteToken)

	authed.Post("/auth/avatar", s.handleUploadAvatar)
	authed.Post("/auth/avatar/uri", s.handleImportAvatarURI)
	authed.Delete("/auth/avatar", s.handleDeleteAvatar)
	// 이미지 조회는 사용자 관리 권한과 무관하다 — 사이드바와 목록이 남의 아바타를
	// 그려야 하므로 로그인한 사람이면 누구나 읽을 수 있어야 한다.
	authed.Get("/users/:id/avatar", s.handleGetAvatar)
	authed.Get("/meta", s.handleMeta)

	// 사용자 관리: 슈퍼 어드민 전용
	users := authed.Group("/users", s.requireRole(model.RoleSuperadmin))
	users.Get("/", s.handleListUsers)
	users.Post("/", s.handleCreateUser)
	// 여러 명을 같은 권한으로 한 번에. 팀이 들어오는 일은 한 명씩 오지 않는다.
	users.Post("/bulk", s.handleBulkCreateUsers)
	users.Get("/:id", s.handleGetUser)
	users.Patch("/:id", s.handleUpdateUser)
	users.Delete("/:id", s.handleDeleteUser)
	users.Post("/:id/password", s.handleResetPassword)
	users.Get("/:id/access", s.handleGetAccess)
	users.Put("/:id/access", s.handlePutAccess)
	// 인증 앱을 잃은 사람의 마지막 경로. 감사 로그에 반드시 남는다.
	users.Post("/:id/totp/reset", s.handleResetUserTOTP)

	// 전역 보안 설정과 내부 시계 상태.
	security := authed.Group("/security", s.requireRole(model.RoleSuperadmin))
	security.Get("/", s.handleGetSecurity)
	security.Put("/", s.handlePutSecurity)

	// 알림(메신저) 설정. 슈퍼 어드민만 다루는 이유는 notify_handlers.go의 주석에 있다.
	notifyGroup := authed.Group("/notify", s.requireRole(model.RoleSuperadmin))
	notifyGroup.Get("/", s.handleGetNotify)
	notifyGroup.Put("/", s.handlePutNotify)
	notifyGroup.Post("/test", s.handleTestNotify)

	// 클러스터: 노드 목록과 복제 상태. 인프라 정보이므로 슈퍼 어드민 전용이다.
	clusterGroup := authed.Group("/cluster", s.requireRole(model.RoleSuperadmin))
	clusterGroup.Get("/", s.handleClusterStatus)
	clusterGroup.Delete("/nodes/:id", s.handleRemoveClusterNode)

	// 프로젝트: 자원의 울타리.
	//
	// 조회는 참여한 것만(슈퍼 어드민은 전부), 고치는 것은 커넥션 관리자다.
	// 프로젝트를 만드는 일은 곧 그 안에 DB를 등록하겠다는 뜻이다.
	projects := authed.Group("/projects")
	projects.Get("/", s.handleListProjects)
	projects.Post("/", s.requireConnManager, s.handleCreateProject)
	projects.Get("/:projectId", s.handleGetProject)
	projects.Put("/:projectId", s.requireConnManager, s.handleUpdateProject)
	projects.Delete("/:projectId", s.requireConnManager, s.handleDeleteProject)
	// 참여자 편집만 슈퍼 어드민이다. 명단을 고치려면 사용자 목록을 볼 수 있어야
	// 하는데, 그것이 슈퍼 어드민의 일이기 때문이다(/users).
	projects.Put("/:projectId/members", s.requireRole(model.RoleSuperadmin), s.handleSetProjectMembers)

	// 커넥션: 조회는 접근 권한 기준으로 필터, 변경은 admin 이상
	conns := authed.Group("/connections")
	conns.Get("/", s.handleListConnections)
	conns.Post("/", s.requireConnManager, s.handleCreateConnection)
	conns.Post("/test", s.requireConnManager, s.handleTestAdhoc)
	conns.Get("/:id", s.handleGetConnection)
	conns.Put("/:id", s.requireConnManager, s.handleUpdateConnection)
	conns.Delete("/:id", s.requireConnManager, s.handleDeleteConnection)
	// 삭제 전 영향 확인. 지우는 사람만 물어보면 되므로 같은 권한을 요구한다.
	conns.Get("/:id/impact", s.requireConnManager, s.handleConnectionImpact)
	conns.Post("/:id/test", s.handleTestConnection)

	// 서버: 접속 정보와 자격증명의 주인. 그 아래에 DB(커넥션)가 달린다.
	// 목록은 접근 권한으로 걸러 누구나 볼 수 있고, 변경은 커넥션 관리자만 한다.
	servers := authed.Group("/servers")
	servers.Get("/", s.handleListServers)
	servers.Post("/", s.requireConnManager, s.handleCreateServer)
	servers.Get("/:id", s.handleGetServer)
	servers.Put("/:id", s.requireConnManager, s.handleUpdateServer)
	servers.Delete("/:id", s.requireConnManager, s.handleDeleteServer)
	// DB 목록 조회는 서버에 실제로 접속한다. 등록 전 단계이므로 관리자만 부를 수 있다.
	servers.Get("/:id/databases", s.requireConnManager, s.handleListServerDatabases)
	servers.Post("/:id/databases", s.requireConnManager, s.handleAddServerDatabases)
	servers.Post("/:id/merge", s.requireConnManager, s.handleMergeServers)

	// 백업(논리 덤프)과 복구.
	//
	// 정적 경로를 :backupId 보다 먼저 등록한다(Fiber는 등록 순서로 매칭한다).
	backups := authed.Group("/backups")
	backups.Get("/", s.handleListBackups)
	backups.Get("/restores/:restoreId", s.handleGetRestore)
	backups.Post("/restores/:restoreId/cancel", s.handleCancelRestore)
	backups.Get("/:backupId", s.handleGetBackup)
	backups.Get("/:backupId/preview", s.handleBackupPreview)
	backups.Get("/:backupId/download", s.handleDownloadBackup)
	backups.Post("/:backupId/restore", s.handleRestoreBackup)
	backups.Post("/:backupId/cancel", s.handleCancelBackup)
	backups.Delete("/:backupId", s.handleDeleteBackup)
	conns.Post("/:id/backups", s.handleCreateBackup)

	// 데이터: 등급이 아니라 능력(data.read / data.write / sql.run)으로 판정한다.
	// 각 핸들러가 requireCap을 직접 호출하므로 여기에는 미들웨어를 걸지 않는다 —
	// 세 경로가 서로 다른 능력을 요구하기 때문이다.
	conns.Get("/:id/data/objects", s.handleDataObjects)
	conns.Post("/:id/data/query", s.handleDataQuery)
	conns.Post("/:id/data/mutate", s.handleDataMutate)
	// 모아 둔 변경을 한 번에 적용한다(또는 dryRun으로 실행될 문장만 본다).
	conns.Post("/:id/data/batch", s.handleDataBatch)
	conns.Post("/:id/statement", s.handleRunStatement)
	// 구문 검사. 실행 경로 바로 옆에 두는 이유: 같은 권한을 요구하고, 같은 입력을
	// 받으며, 화면에서도 같은 버튼 줄에 놓인다.
	conns.Post("/:id/statement/check", s.handleCheckStatement)

	// 스키마: 모니터링 등급 이상이면 조회 가능
	conns.Get("/:id/schema", s.handleGetSchema)
	// 특화 탐색: MongoDB/Redis 전용 (관계형은 스키마 화면이 같은 역할을 한다)
	conns.Get("/:id/explore", s.handleExplore)
	conns.Post("/:id/schema/diff", s.handleSchemaDiff)
	conns.Get("/:id/schema/ddl", s.handleSchemaDDL)
	// 설명(주석) 고치기. 실제 DB를 바꾸는 일이므로 계획만 만들고, 리뷰·승인·실행은
	// 여느 마이그레이션과 같은 길을 탄다.
	conns.Post("/:id/schema/comments", s.handleSchemaComments)
	// 구조 화면: 현재(또는 특정 버전) 스키마를 ERD로 본다.
	// 배치·메모·그룹은 계정별로 저장되므로 저장 경로가 같은 커넥션 아래에 있다.
	conns.Get("/:id/structure", s.handleGetStructure)

	// 분산 스토리지(하둡·Ceph). 커넥션 아래에 두는 이유는 권한과 자격증명이
	// 커넥션의 것이기 때문이다(storage_handlers.go 참고).
	conns.Get("/:id/storage", s.handleStorageOverview)
	conns.Get("/:id/storage/browse", s.handleStorageBrowse)
	conns.Get("/:id/storage/apps", s.handleStorageApps)
	conns.Get("/:id/storage/pools", s.handleStoragePools)
	conns.Get("/:id/storage/osds", s.handleStorageOSDs)
	conns.Get("/:id/storage/buckets", s.handleStorageBuckets)
	conns.Post("/:id/storage/mkdir", s.handleStorageMkdir)
	conns.Post("/:id/storage/rename", s.handleStorageRename)
	conns.Post("/:id/storage/delete", s.handleStorageDelete)

	// 메시지 브로커(RabbitMQ·Kafka). 스토리지와 같은 이유로 커넥션 아래에 둔다 —
	// 권한과 자격증명이 커넥션의 것이기 때문이다(broker_handlers.go 참고).
	conns.Get("/:id/broker", s.handleBrokerOverview)
	conns.Get("/:id/broker/queues", s.handleBrokerQueues)
	conns.Get("/:id/broker/exchanges", s.handleBrokerExchanges)
	conns.Get("/:id/broker/connections", s.handleBrokerConnections)
	conns.Get("/:id/broker/topics", s.handleBrokerTopics)
	conns.Get("/:id/broker/topics/:topic/config", s.handleBrokerTopicConfig)
	conns.Get("/:id/broker/groups", s.handleBrokerGroups)
	conns.Post("/:id/broker/purge", s.handleBrokerPurgeQueue)
	conns.Post("/:id/broker/delete-queue", s.handleBrokerDeleteQueue)
	conns.Post("/:id/broker/close-connection", s.handleBrokerCloseConnection)

	// 모니터링: 지표 시계열과 스키마 스냅샷은 커넥션에 종속된다
	conns.Get("/:id/metrics", s.handleMonitorMetrics)
	conns.Get("/:id/metrics/available", s.handleMonitorAvailableMetrics)
	conns.Post("/:id/drift/check", s.handleCheckDrift)
	conns.Get("/:id/snapshots", s.handleListSnapshots)
	conns.Get("/:id/snapshots/:snapshotId", s.handleGetSnapshot)

	// 로그: 모니터링 등급 이상이면 조회 가능
	conns.Get("/:id/logs", s.handleGetLogs)
	conns.Get("/:id/logs/sources", s.handleLogSources)

	// 모니터링 개요 / 이벤트 / 룰
	authed.Get("/logs/meta", s.handleLogMeta)
	mon := authed.Group("/monitor")
	mon.Get("/overview", s.handleMonitorOverview)
	mon.Get("/storage", s.requireRole(model.RoleSuperadmin), s.handleStorageStats)
	mon.Get("/events", s.handleListEvents)
	mon.Post("/events/:id/ack", s.handleAckEvent)
	mon.Post("/events/:id/resolve", s.handleResolveEvent)
	// 호스트 감시. 커넥션 관리 권한을 요구하는 이유는 host_handlers.go의 주석에 있다.
	host := mon.Group("/host", s.requireRole(model.RoleSuperadmin, model.RoleAdmin))
	host.Get("/", s.handleHostOverview)
	host.Get("/series", s.handleHostSeries)
	host.Get("/metrics", s.handleHostMetricNames)
	host.Put("/thresholds", s.handleSaveHostThresholds)

	mon.Get("/rules", s.handleListRules)
	mon.Post("/rules", s.handleCreateRule)
	mon.Put("/rules/:id", s.handleUpdateRule)
	mon.Delete("/rules/:id", s.handleDeleteRule)

	// 타입 카탈로그: "이 DB에서 고를 수 있는 타입". 문서가 아니라 dialect에 달린
	// 정보이므로 문서 경로 밖에 둔다(초안을 만들기 전에도 필요하다).
	authed.Get("/erd/types", s.handleERDTypeCatalog)

	// ERD 문서. 권한은 문서가 아니라 대상 커넥션에 붙어 있다.
	docs := authed.Group("/erd/documents")
	docs.Get("/", s.handleListERDDocuments)
	docs.Post("/", s.handleCreateERDDocument)
	docs.Get("/:docId", s.handleGetERDDocument)
	docs.Patch("/:docId", s.handleUpdateERDDocument)
	docs.Delete("/:docId", s.handleDeleteERDDocument)
	docs.Post("/:docId/duplicate", s.handleDuplicateERDDocument)
	docs.Get("/:docId/ops", s.handleERDOps)
	docs.Get("/:docId/chat", s.handleERDChat)
	// 문서에 매인 AI 대화. 대화 자체는 기존 /ai/sessions/:id/chat 으로 이어지고
	// (프로바이더·스트리밍·메시지 저장이 모두 같다), 여기서는 목록과 생성만 맡는다.
	docs.Get("/:docId/ai/sessions", s.handleListERDAISessions)
	docs.Post("/:docId/ai/sessions", s.handleCreateERDAISession)
	docs.Post("/:docId/diff", s.handleERDDiff)
	// 초안을 SQL로 받는다. 대상 DB가 없는 초안에서는 이것이 유일한 산출물이다.
	docs.Get("/:docId/ddl", s.handleERDDDL)
	// 계획을 만들기 전에 SQL이 실제로 실행되는지 확인한다. 그림자 DB를 만들어
	// 거기서 돌려 보므로 대상 DB는 손대지 않는다.
	docs.Post("/:docId/dryrun", s.handleERDDryRun)
	// SQL을 읽어 초안에 반영한다. 미리보기(dryRun)와 적용이 같은 경로다 —
	// 무엇이 바뀔지 보여준 것과 실제로 적용되는 것이 갈리면 미리보기가 거짓말이 된다.
	docs.Post("/:docId/import", s.handleERDImportSQL)

	// 실시간 편집 소켓. 업그레이드 전에 인증·권한·Origin을 확인한다.
	authed.Get("/erd/documents/:docId/socket", s.erdWSUpgrade, websocket.New(s.handleERDSocket))

	// 스키마 버전: 커넥션에 종속된다.
	conns.Get("/:id/versions", s.handleListVersions)
	conns.Post("/:id/versions", s.handleCaptureVersion)
	conns.Get("/:id/versions/diff", s.handleVersionDiff)
	conns.Get("/:id/versions/:versionId", s.handleGetVersion)
	// 버전 롤백: 전용 실행 경로가 아니라 마이그레이션 계획을 만든다.
	// GET은 미리보기, POST는 계획 생성이다.
	conns.Get("/:id/versions/:versionId/rollback", s.handleVersionRollbackPreview)
	conns.Post("/:id/versions/:versionId/rollback", s.handleVersionRollback)

	// 용어 사전. 읽기는 누구나, 고치기는 커넥션 관리자만이다 — 팀의 약속이라
	// 아무나 바꾸면 약속이 아니게 되지만, 아무나 볼 수 없으면 지킬 수도 없다.
	// 사전은 **참여자가 함께 쓴다.** 관문은 프로젝트 참여뿐이고, 각 핸들러가
	// requireProject 로 확인한다(내려받은 프로젝트가 아니라 그 용어가 든 프로젝트로).
	//
	// 커넥션 관리자만 쓸 수 있게 두었던 것이 잘못이었다. 사전에 말을 올리는 사람은
	// 설계하는 사람이고, 그때마다 관리자를 찾아야 하면 사전은 쓰이지 않는다 —
	// 그러면 사전 밖의 약속이 생기고, 그것이 사전이 있는 것보다 나쁘다.
	//
	// 지우기만 좁다(만든 사람과 관리자). 되돌릴 수 없는 동작과 함께 하는 동작을 같은
	// 문턱에 두면, 함께 쓰게 열어 준 대가로 사고가 따라온다 — 독립 ERD 초안에 쓴
	// 것과 같은 규칙이다.
	glossary := authed.Group("/glossary")
	glossary.Get("/", s.handleListGlossary)
	glossary.Post("/", s.handleCreateGlossaryTerm)
	glossary.Post("/bulk", s.handleBulkGlossary)
	glossary.Put("/:termId", s.handleUpdateGlossaryTerm)
	glossary.Delete("/:termId", s.handleDeleteGlossaryTerm)

	// 마이그레이션: 여러 커넥션에 걸친 목록이 필요하므로 별도 그룹이다.
	migs := authed.Group("/migrations")
	migs.Get("/", s.handleListMigrations)
	migs.Post("/", s.handleCreateMigration)
	migs.Get("/:migId", s.handleGetMigration)
	migs.Post("/:migId/status", s.handleMigrationStatus)
	migs.Post("/:migId/review", s.handleReviewMigration)
	// 리뷰 한 건 고치기·지우기. 오타를 고치거나 빈 의견을 치우는 길이 없으면
	// 리뷰 칸에는 지울 수 없는 부스러기가 쌓이고, 결국 읽히지 않는 칸이 된다.
	migs.Patch("/:migId/review/:reviewId", s.handleUpdateMigrationReview)
	migs.Delete("/:migId/review/:reviewId", s.handleDeleteMigrationReview)
	// 담당자·리뷰어 지정. 후보 목록은 대상 커넥션을 만질 수 있는 사람만 담는다.
	migs.Get("/:migId/people", s.handleListMigrationPeople)
	// 이 계획 하나의 이력. 감사 로그 전체(슈퍼 어드민 전용)와 달리 계획을 볼 수
	// 있는 사람이면 볼 수 있다 — 누가 승인했는지 모르는 리뷰는 절차가 아니다.
	migs.Get("/:migId/activity", s.handleMigrationActivity)
	migs.Put("/:migId/assignment", s.handleSetMigrationAssignment)
	migs.Post("/:migId/precheck", s.handlePrecheckMigration)
	// 미리 검사: 조건이 아니라 SQL 자체를 본다. 그림자 DB에서 돌려 보므로
	// 대상 DB는 손대지 않는다.
	migs.Post("/:migId/dryrun", s.handleMigrationDryRun)
	migs.Post("/:migId/apply", s.handleApplyMigration)
	migs.Post("/:migId/rollback", s.handleRollbackMigration)
	migs.Post("/:migId/push", s.handlePushMigration)
	migs.Delete("/:migId", s.handleDeleteMigration)

	// Git 저장소 연동.
	//
	// 권한 미들웨어가 없다. 이것은 **개인의 Git 계정**이고, 등록하는 사람과 쓰는 사람이
	// 언제나 같기 때문이다(모든 질의가 owner_id로 좁혀진다). 커넥션 관리 권한을 요구하던
	// 예전 모델은 "관리자가 등록한 토큰을 모두가 함께 쓴다"는 뜻이었는데, 그러면 원격
	// 저장소에서 누가 올렸는지가 사라진다.
	//
	// 남의 것을 볼 수 없다는 규칙에는 슈퍼 어드민도 포함된다. API 토큰과 같은 이유다 —
	// 남의 자격증명을 볼 수 있다는 것은 곧 그 사람 명의로 행동할 수 있다는 뜻이다.
	git := authed.Group("/vcs")
	git.Get("/integrations", s.handleListVCSIntegrations)
	git.Post("/integrations", s.handleCreateVCSIntegration)
	git.Put("/integrations/:id", s.handleUpdateVCSIntegration)
	git.Delete("/integrations/:id", s.handleDeleteVCSIntegration)
	git.Post("/integrations/:id/test", s.handleTestVCSIntegration)
	git.Get("/pushes", s.handleListVCSPushes)

	// AI 어시스턴트.
	//
	// 프로바이더(API 키) 설정은 커넥션 관리 권한 이상을 요구한다 — 키를 다루는 설정이다.
	// 대화는 누구나 할 수 있지만, 툴은 그 사람의 권한으로만 실행된다.
	assistant := authed.Group("/ai")
	assistant.Get("/providers", s.handleListAIProviders)
	// 모델 목록 조회. 저장 전에도 불러야 하므로 :id 아래가 아니다
	// (정적 경로를 :id 보다 먼저 등록한다 — Fiber는 등록 순서로 매칭한다).
	assistant.Post("/providers/models", s.requireConnManager, s.handleDiscoverAIModels)
	assistant.Post("/providers", s.requireConnManager, s.handleCreateAIProvider)
	assistant.Put("/providers/:id", s.requireConnManager, s.handleUpdateAIProvider)
	assistant.Delete("/providers/:id", s.requireConnManager, s.handleDeleteAIProvider)
	assistant.Post("/providers/:id/test", s.requireConnManager, s.handleTestAIProvider)

	// 스킬은 미리 적어 둔 지시문이다(ai_skills.go). 목록만 준다 — 실행은 사용자가
	// 그 글을 자기 말로 보내는 것이고, 그러면 지금까지의 툴·승인 규칙이 그대로 적용된다.
	assistant.Get("/skills", s.handleListAISkills)
	// 사람이 만든 스킬. 고치고 지우는 것은 주인(과 사용자 관리자)만 한다 —
	// 공유된 스킬을 아무나 바꾸면 "어제 쓰던 스킬이 오늘 다른 말을 한다"가 된다.
	assistant.Post("/skills", s.handleCreateAISkill)
	assistant.Put("/skills/:id", s.handleUpdateAISkill)
	assistant.Delete("/skills/:id", s.handleDeleteAISkill)

	assistant.Get("/sessions", s.handleListAISessions)
	assistant.Post("/sessions", s.handleCreateAISession)
	assistant.Get("/sessions/:id", s.handleGetAISession)
	assistant.Patch("/sessions/:id", s.handleUpdateAISession)
	assistant.Delete("/sessions/:id", s.handleDeleteAISession)
	assistant.Post("/sessions/:id/chat", s.handleAIChat)
	assistant.Post("/sessions/:id/actions/:actionId", s.handleDecidePendingAction)

	// 매크로. 메뉴 접근 자체가 권한이므로 그룹 전체에 미들웨어를 건다.
	//
	// 정적 경로(runs, nodes, meta)를 :id 보다 먼저 등록한다. Fiber는 등록 순서대로
	// 매칭하므로 순서가 뒤바뀌면 /macros/runs 가 id="runs" 인 매크로 조회로 잡힌다.
	macros := authed.Group("/macros", s.requirePerm(model.PermMacro))
	macros.Get("/meta", s.handleMacroMeta)
	macros.Get("/runs", s.handleListMacroRuns)
	macros.Get("/runs/:runId", s.handleGetMacroRun)
	macros.Get("/runs/:runId/stream", s.handleStreamMacroRun)
	macros.Post("/runs/:runId/cancel", s.handleCancelMacroRun)
	// 자동 실행 트리거. 목록은 매크로에 매이지 않은 전체 조회도 지원한다
	// (?macro= 로 좁힌다) — "지금 자동으로 도는 것이 무엇인가"는 한 화면에서 봐야 한다.
	macros.Get("/triggers", s.handleListTriggers)
	macros.Post("/triggers/preview", s.handlePreviewSchedule)
	macros.Put("/triggers/:triggerId", s.handleUpdateTrigger)
	macros.Post("/triggers/:triggerId/toggle", s.handleToggleTrigger)
	macros.Delete("/triggers/:triggerId", s.handleDeleteTrigger)

	macros.Get("/nodes", s.handleListNodeDefs)
	macros.Post("/nodes", s.handleCreateNodeDef)
	macros.Put("/nodes/:defId", s.handleUpdateNodeDef)
	macros.Delete("/nodes/:defId", s.handleDeleteNodeDef)
	macros.Get("/nodes/:defId/versions", s.handleListNodeDefVersions)
	// 전역 노드의 공유 설정. 매크로 전용 노드는 소속 매크로를 따르므로 여기에 없다.
	macros.Put("/nodes/:defId/access", s.handleUpdateNodeDefAccess)
	macros.Get("/nodes/:defId/collaborators", s.handleListNodeDefCollaborators)
	macros.Post("/nodes/:defId/collaborators", s.handleAddNodeDefCollaborator)
	macros.Delete("/nodes/:defId/collaborators/:userId", s.handleRemoveNodeDefCollaborator)

	// 협업자 후보 목록. /users 는 슈퍼어드민 전용이므로 이름만 주는 좁은 창구를 둔다.
	macros.Get("/people", s.handleListMacroPeople)

	macros.Get("/", s.handleListMacros)
	macros.Post("/", s.handleCreateMacro)
	macros.Get("/:id", s.handleGetMacro)
	macros.Patch("/:id", s.handleUpdateMacro)
	macros.Delete("/:id", s.handleDeleteMacro)
	macros.Get("/:id/versions", s.handleListMacroVersions)
	macros.Post("/:id/versions", s.handleSaveMacroVersion)
	macros.Get("/:id/versions/:version", s.handleGetMacroVersion)
	macros.Post("/:id/versions/:version/restore", s.handleRestoreMacroVersion)
	macros.Post("/:id/run", s.handleRunMacro)
	macros.Post("/:id/triggers", s.handleCreateTrigger)
	// 공유: 공개 범위와 협업자. 만든 사람과 협업자(와 슈퍼어드민)만 바꾼다.
	macros.Put("/:id/access", s.handleUpdateMacroAccess)
	macros.Get("/:id/collaborators", s.handleListMacroCollaborators)
	macros.Post("/:id/collaborators", s.handleAddMacroCollaborator)
	macros.Delete("/:id/collaborators/:userId", s.handleRemoveMacroCollaborator)

	// 감사 로그: 슈퍼 어드민 전용
	authed.Get("/audit", s.requireRole(model.RoleSuperadmin), s.handleListAudit)

	// MCP 엔드포인트.
	//
	// /api/v1 밑이 아닌 이유: MCP 클라이언트는 서버 주소 하나를 설정에 적는다.
	// 짧고 관례적인 경로가 낫고, 버전은 프로토콜 자체가 initialize에서 협상한다.
	//
	// 세션 미들웨어(requireAuth)를 거치지 않는다 — 이 경로는 쿠키를 받지 않고
	// Bearer 토큰만 받는다(자세한 이유는 mcp_handlers.go).
	s.app.Post("/mcp", s.handleMCP)
	s.app.Get("/mcp", s.handleMCPGet)

	// REST API 엔드포인트. MCP와 같은 툴을 평범한 HTTP로 연다.
	//
	// /api/v1 밑이 아닌 이유는 MCP와 같다: 그 아래에는 세션 쿠키 미들웨어(requireAuth)가
	// 걸려 있고 이 경로는 쿠키를 받지 않는다. 토큰 경로가 쿠키도 받으면 브라우저가
	// 자동으로 자격증명을 실어 보내게 되어 CSRF 방어가 무너진다.
	//
	// 그룹이 아니라 경로마다 requireToken을 붙이는 이유: /api 전체에 미들웨어를 걸면
	// 그 뒤에 등록되는 /api/* 폴백까지 토큰을 요구하게 되고, 오타 난 프론트엔드 요청이
	// 404 대신 401을 받아 로그아웃으로 이어진다.
	s.app.Get("/api/me", s.requireToken, s.handleRESTIdentity)
	s.app.Get(restBasePath, s.requireToken, s.handleRESTToolList)
	s.app.Get(restBasePath+"/:name", s.requireToken, s.handleRESTToolGet)
	s.app.Post(restBasePath+"/:name", s.requireToken, s.handleRESTToolCall)

	// 정의되지 않은 API 경로는 SPA fallback으로 새지 않도록 여기서 404 처리한다.
	s.app.All("/api/*", func(c *fiber.Ctx) error {
		return fail(c, fiber.StatusNotFound, "not_found", "존재하지 않는 API 경로입니다")
	})

	s.mountFrontend()
}

// mountFrontend는 프론트엔드를 서빙한다.
// dev 모드에서는 디스크에서 읽어 새로고침만으로 변경이 반영된다.
// assetExtensions는 "정적 자산"으로 볼 확장자다.
// 여기에 없는 확장자 없는 경로(=SPA 라우트)만 index.html로 응답한다.
var assetExtensions = map[string]bool{
	".js": true, ".mjs": true, ".css": true, ".map": true,
	".svg": true, ".png": true, ".jpg": true, ".jpeg": true, ".gif": true,
	".webp": true, ".ico": true, ".avif": true,
	".woff": true, ".woff2": true, ".ttf": true, ".otf": true,
	".json": true, ".webmanifest": true, ".txt": true, ".xml": true,
}

// codeExtensions는 "옛것과 새것이 섞이면 오작동하는" 자산이다.
//
// 모듈 하나만 옛것이어도 화면은 새것처럼 보이면서 다르게 움직인다. 그런 것은
// 캐시에서 꺼내기 전에 반드시 서버에 물어야 한다.
var codeExtensions = map[string]bool{
	".js": true, ".mjs": true, ".css": true, ".html": true,
	".map": true, ".json": true, ".webmanifest": true,
}

func isCodePath(path string) bool {
	dot := strings.LastIndexByte(path, '.')
	if dot < 0 {
		return false
	}
	if slash := strings.LastIndexByte(path, '/'); slash > dot {
		return false
	}
	return codeExtensions[strings.ToLower(path[dot:])]
}

func isAssetPath(path string) bool {
	dot := strings.LastIndexByte(path, '.')
	if dot < 0 {
		return false
	}
	// 마지막 경로 요소에 있는 점만 확장자로 본다 (/v1.2/users 같은 경로 보호).
	if slash := strings.LastIndexByte(path, '/'); slash > dot {
		return false
	}
	return assetExtensions[strings.ToLower(path[dot:])]
}

func (s *Server) mountFrontend() {
	var handler fiber.Handler
	if s.cfg.DevMode {
		fsHandler := filesystem.New(filesystem.Config{
			Root:   http.Dir(s.cfg.WebDir),
			Index:  "index.html",
			Browse: false,
		})
		// 개발 모드에서는 캐시를 아예 쓰지 않게 한다.
		//
		// Cache-Control이 없으면 브라우저는 Last-Modified만 보고 **스스로 유효기간을
		// 정한다**(휴리스틱 캐싱). 그러면 파일을 고쳐도 새로고침에 옛 모듈이 그려지고,
		// 화면으로 확인하는 사람은 자기 수정이 틀렸다고 판단한다 — 이 저장소에서
		// 실제로 두 번 겪은 함정이다. dev의 목적은 "새로고침만으로 반영"이므로
		// 여기서는 속도보다 정확함을 고른다.
		handler = func(c *fiber.Ctx) error {
			c.Set(fiber.HeaderCacheControl, "no-store")
			return fsHandler(c)
		}
	} else {
		// 코드(js·css·html)는 **캐시하되 반드시 되물어야** 한다.
		//
		// 예전에는 모든 자산에 max-age=3600 을 걸었다. 그런데 embed.FS 는 수정 시각이
		// 0이라 Last-Modified 도 ETag 도 함께 나가지 않았고, 그래서 브라우저는 한
		// 시간 동안 **되묻지 않았다**. 새 버전을 올리고 새로고침해도 옛 모듈이 그대로
		// 그려진다는 뜻이다 — 셸(index.html)만 no-cache 로 두었지만 모듈 주소는
		// 그대로여서 아무 소용이 없었다.
		//
		// 그 상태는 "새 기능이 보이는데 동작이 이상하다"로 나타난다. 고친 사람도 쓰는
		// 사람도 무엇이 틀렸는지 알 수 없다 — 실제로 그런 신고를 받았다.
		//
		// no-cache 는 "쓰지 말라"가 아니라 "쓰기 전에 물어보라"다. 내용이 그대로면
		// ETag 가 같아 304 한 줄로 끝나고, 값은 캐시에서 나온다.
		//
		// 글꼴·그림은 하루 캐시한다. 그것들은 바뀌어도 화면이 조금 다를 뿐, 코드처럼
		// 옛것과 새것이 섞여 오작동하지 않는다.
		tagged := etag.New()
		s.app.Use("/", func(c *fiber.Ctx) error {
			if isCodePath(c.Path()) {
				c.Set(fiber.HeaderCacheControl, "no-cache")
				return tagged(c)
			}
			c.Set(fiber.HeaderCacheControl, "public, max-age=86400")
			return c.Next()
		})
		handler = filesystem.New(filesystem.Config{
			Root:   http.FS(s.web),
			Index:  "index.html",
			Browse: false,
		})
	}
	s.app.Use("/", handler)

	// SPA 라우팅: 정적 파일이 없으면 index.html을 돌려준다.
	s.app.Use(func(c *fiber.Ctx) error {
		if strings.HasPrefix(c.Path(), "/api/") {
			return fiber.ErrNotFound
		}
		// 자산처럼 보이는 경로는 셸을 돌려주지 않고 404로 답한다.
		//
		// 없는 .js를 index.html로 응답하면 브라우저는 "HTML에서 문법 오류"라고 보고해
		// 원인을 찾기 어려워지고, 없는 아이콘을 HTML로 응답하면 브라우저가 그것을
		// 아이콘으로 해석하려 한다(/favicon.ico는 링크가 없어도 자동 요청된다).
		if isAssetPath(c.Path()) {
			return fiber.ErrNotFound
		}
		index, err := s.readIndex()
		if err != nil {
			return fiber.NewError(fiber.StatusInternalServerError, "index.html을 읽을 수 없습니다")
		}
		c.Set(fiber.HeaderContentType, fiber.MIMETextHTMLCharsetUTF8)
		// 셸(index.html)은 캐시하지 않는다. 이 문서가 모듈 목록을 들고 있으므로,
		// 새 버전을 배포해도 셸이 캐시에서 나오면 옛 모듈을 계속 불러온다.
		if s.cfg.DevMode {
			c.Set(fiber.HeaderCacheControl, "no-store")
		} else {
			c.Set(fiber.HeaderCacheControl, "no-cache")
		}
		// filesystem 미들웨어가 파일을 못 찾으면 404를 세팅한 뒤 Next를 호출한다.
		// SPA 셸은 정상 응답이므로 200으로 되돌려야 브라우저 히스토리/캐시가 올바르게 동작한다.
		return c.Status(fiber.StatusOK).Send(index)
	})
}

func (s *Server) readIndex() ([]byte, error) {
	if s.cfg.DevMode {
		b, err := os.ReadFile(s.cfg.WebDir + "/index.html")
		if err != nil {
			return nil, fmt.Errorf("read index: %w", err)
		}
		return b, nil
	}
	b, err := fs.ReadFile(s.web, "index.html")
	if err != nil {
		return nil, fmt.Errorf("read embedded index: %w", err)
	}
	return b, nil
}

func securityHeaders(c *fiber.Ctx) error {
	c.Set("X-Content-Type-Options", "nosniff")
	c.Set("X-Frame-Options", "DENY")
	c.Set("Referrer-Policy", "same-origin")
	// 프론트엔드는 외부 리소스를 전혀 쓰지 않으므로 self로 고정한다.
	c.Set("Content-Security-Policy",
		"default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; "+
			"script-src 'self'; connect-src 'self'; font-src 'self'; frame-ancestors 'none'")
	return c.Next()
}

func (s *Server) handleHealth(c *fiber.Ctx) error {
	// 버전을 여기서 노출하는 이유: 배포 후 "무엇이 돌고 있는가"를 로그인 없이
	// 확인할 수 있어야 한다. 인증 뒤에 두면 배포 검증 스크립트가 쓸 수 없다.
	return c.JSON(fiber.Map{
		"status": "ok", "time": time.Now().UTC(), "build": buildinfo.Get(),
	})
}

// handleMeta는 프론트엔드가 폼과 권한 UI를 그리는 데 필요한 정적 메타데이터를 제공한다.
func (s *Server) handleMeta(c *fiber.Ctx) error {
	return c.JSON(fiber.Map{
		// 빌드 정보를 함께 싣는 이유: 화면이 버전을 늘 보여주려면 셸을 그릴 때
		// 그 값이 있어야 한다. /health 로 따로 물으면 화면을 열 때마다 요청이
		// 하나 늘고, 늦게 오면 버전만 뒤늦게 나타난다.
		"build":   buildinfo.Get(),
		"dbKinds": dbx.Kinds(),
		"roles": []fiber.Map{
			{"value": model.RoleSuperadmin, "label": "슈퍼 어드민", "help": "사용자 관리 및 모든 설정"},
			{"value": model.RoleAdmin, "label": "어드민", "help": "DB 커넥션 등록/수정 (사용자 관리 불가)"},
			{"value": model.RoleMember, "label": "멤버", "help": "부여된 DB 범위만 사용"},
		},
		"accessModes": []fiber.Map{
			{"value": model.AccessAll, "label": "모든 DB 접근 가능"},
			{"value": model.AccessAllowlist, "label": "선택한 DB에만 접근 가능"},
			{"value": model.AccessDenylist, "label": "특정 DB 접근 불가"},
		},
		"levels": []fiber.Map{
			{"value": model.LevelNone, "label": "없음", "help": "접근 불가"},
			{"value": model.LevelMonitor, "label": "모니터링", "help": "상태·부하·로그 조회"},
			{"value": model.LevelERD, "label": "ERD 설계", "help": "모니터링 + ERD 설계 및 리뷰 요청"},
			{"value": model.LevelMigrate, "label": "마이그레이션", "help": "ERD + 승인된 마이그레이션 실행/롤백"},
		},
		"environments": []fiber.Map{
			{"value": model.EnvDev, "label": "개발"},
			{"value": model.EnvProd, "label": "운영"},
		},
		// 데이터 능력은 등급과 다른 축이므로 별도 목록으로 내려보낸다.
		// 화면이 이 둘을 한 목록으로 합치면 "monitor이면서 data.write"처럼
		// 실제로 가능한 조합을 표현할 수 없게 된다.
		"capabilities": []fiber.Map{
			{"value": model.CapDataRead, "label": model.CapDataRead.Label(),
				"help": "테이블·문서·키의 값을 조회하고 검색합니다"},
			{"value": model.CapDataWrite, "label": model.CapDataWrite.Label(),
				"help": "행·문서·키 값을 추가·수정·삭제합니다 (조회 권한이 함께 필요합니다)"},
			{"value": model.CapSQLRun, "label": model.CapSQLRun.Label(),
				"help": "임의의 SQL 또는 Mongo/Redis 명령을 실행합니다"},
		},
		"perms": []fiber.Map{
			{"value": model.PermMacro, "label": model.PermMacro.Label(),
				"help": "매크로를 보고 만들고 수정합니다. 매크로는 권한자끼리 공유됩니다"},
			{"value": model.PermScriptRun, "label": model.PermScriptRun.Label(),
				"help": "매크로에서 bash/powershell 스크립트를 실행합니다 (서버가 -allow-shell로 켜져 있어야 합니다)"},
			{"value": model.PermHTTPCall, "label": model.PermHTTPCall.Label(),
				"help": "매크로에서 외부 HTTP API를 호출합니다. DB에서 읽은 값을 외부로 보낼 수 있습니다"},
		},
		"shellEnabled": s.cfg.AllowShell,
		// 모니터링이 꺼져 있으면 이벤트가 생기지 않는다 — 즉 조건 트리거는 영원히
		// 발화하지 않는다. 화면이 그 사실을 미리 알려야 "만들었는데 안 돈다"를 막는다.
		"monitorEnabled": s.cfg.MonitorEnabled,
		"avatarMaxKB":    s.cfg.AvatarMaxKB,
		// 아바타 목록은 서버가 갖는다. 화면은 여기 있는 키만 그리고, API는 여기 있는
		// 키만 저장한다 — 목록이 두 곳에 있으면 언젠가 갈라진다.
		"avatarGroups": model.AvatarGroups(),
		"avatars":      model.Avatars(),
		"avatarMimes":  model.AvatarMimes(),
	})
}
