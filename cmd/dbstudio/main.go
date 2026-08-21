// Command dbstudio는 DB 관리용 웹 앱을 단일 바이너리로 실행한다.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	dbstudio "dbstudio"
	"dbstudio/internal/api"
	"dbstudio/internal/applog"
	"dbstudio/internal/auth"
	"dbstudio/internal/bootstrap"
	"dbstudio/internal/buildinfo"
	"dbstudio/internal/clock"
	"dbstudio/internal/cluster"
	"dbstudio/internal/config"
	"dbstudio/internal/crypto"
	"dbstudio/internal/monitor"
	"dbstudio/internal/notify"
	"dbstudio/internal/runstate"
	"dbstudio/internal/store"
)

func main() {
	if err := run(); err != nil {
		if errors.Is(err, flagHelp) {
			return
		}
		// 이 지점에서는 로그 파일이 이미 닫혀 있다(run의 defer). 그래서 종료 이유는
		// run 안에서 기록하고, 여기서는 종료 코드만 정한다. 파일에 남지 않은 이유는
		// 조사에 쓸 수 없다 — 터미널을 닫으면 사라지기 때문이다.
		if !errors.Is(err, errLogged) {
			slog.Error("종료", "err", err)
		}
		os.Exit(1)
	}
}

// errLogged는 "이미 기록했다"는 표시다. main이 같은 내용을 두 번 남기지 않게 한다.
var errLogged = errors.New("already logged")

// startupHint는 흔한 시작 실패에 대해 다음에 할 일을 알려준다.
// 원인을 아는 것과 무엇을 해야 하는지 아는 것은 다르다.
func startupHint(err error) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "bind") || strings.Contains(msg, "in use") ||
		strings.Contains(msg, "각 소켓 주소"):
		return "같은 주소에 이미 다른 인스턴스가 떠 있습니다. -addr 로 포트를 바꾸거나 기존 프로세스를 종료하세요"
	case strings.Contains(msg, "master key"):
		return "DBSTUDIO_MASTER_KEY 값이 올바른 base64 32바이트인지, master.key 파일 권한이 있는지 확인하세요"
	case strings.Contains(msg, "open meta db") || strings.Contains(msg, "database is locked"):
		return "데이터 디렉터리에 쓸 수 있는지, 같은 데이터 디렉터리를 다른 인스턴스가 쓰고 있지 않은지 확인하세요"
	default:
		return ""
	}
}

var flagHelp = errors.New("help requested")

func run() error {
	// 설정을 읽기 전에도 로그는 나가야 한다(설정 자체가 실패할 수 있다).
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))

	// -version은 설정을 읽기 전에 처리한다. 데이터 디렉터리를 만들지 않고
	// 버전만 확인하고 싶은 경우(컨테이너 이미지 검증 등)가 있다.
	for _, arg := range os.Args[1:] {
		if arg == "-version" || arg == "--version" {
			fmt.Println("dbstudio " + buildinfo.Get().String())
			return flagHelp
		}
	}

	cfg, err := config.Load(os.Args[1:])
	if err != nil {
		// flag 패키지가 -h를 처리하면 이미 사용법을 출력했으므로 조용히 종료한다.
		if errors.Is(err, flag.ErrHelp) {
			return flagHelp
		}
		return err
	}

	// 설정이 확정된 뒤 파일 로그로 전환한다. 이 시점부터의 기록은 터미널을 닫아도 남는다.
	logging := applog.Setup(applog.Options{
		Level: cfg.LogLevel, Format: cfg.LogFormat,
		File: cfg.LogFilePath(), MaxMB: cfg.LogMaxMB,
	})
	defer logging.Close()

	started := time.Now()
	build := buildinfo.Get()
	// 시작 줄에 pid를 넣는 이유: 같은 로그 파일에 여러 인스턴스가 쓸 수 있고,
	// "누가 언제 죽었는가"를 구분하는 유일한 단서가 된다.
	slog.Info("프로세스 시작",
		"version", build.Version, "commit", build.Commit, "platform", build.Platform,
		"pid", os.Getpid(), "args", strings.Join(os.Args[1:], " "),
		"logFile", orNone(logging.Path), "crashFile", orNone(logging.CrashPath),
		"logLevel", logging.Level.String())

	// 종료는 반드시 한 줄을 남긴다. 이 줄이 없으면 강제 종료(kill -9, 전원 차단,
	// 런타임 크래시)라는 뜻이므로, 로그 부재 자체가 정보가 된다.
	defer func() {
		slog.Info("프로세스 종료", "pid", os.Getpid(), "uptime", time.Since(started).Round(time.Second))
	}()

	// 이전 실행이 종료 기록을 남기지 않았는지 확인한다.
	//
	// 강제 종료는 프로세스에 아무 기회도 주지 않으므로 그 실행의 로그는 "시작했다"에서
	// 끊긴다. 그 사실을 다음 시작 때 말해 주지 않으면 사용자는 로그를 보고도
	// "이유 없이 꺼졌다"고 판단할 수밖에 없다.
	var run *runstate.Run
	takeOver, startupNote := reportPreviousRun(cfg, logging.CrashPath)
	if takeOver {
		var err error
		if run, err = runstate.Begin(runstate.Path(cfg.DataDir), build.Version, cfg.Addr); err != nil {
			// 표식을 못 쓰는 것이 서버 시작을 막을 이유는 아니다.
			slog.Warn("실행 표식을 기록하지 못했습니다 (비정상 종료 감지가 동작하지 않습니다)", "err", err)
		}
		defer run.End()
	}

	if err := serve(cfg, run, startupNote); err != nil {
		// 로그 파일이 아직 열려 있는 지점에서 기록한다.
		slog.Error("서버가 오류로 종료합니다", "err", err, "hint", startupHint(err))
		return fmt.Errorf("%w: %w", errLogged, err)
	}
	return nil
}

// serve는 의존성을 구성하고 리스닝을 시작한다. 오류는 호출자가 기록한다.
//
// startupNote는 이전 실행이 종료 기록을 남기지 않았을 때의 설명이다. 호스트 모니터가
// 그것을 이벤트로 남긴다 — 로그는 서버에 접근할 수 있는 사람만 보기 때문이다.
func serve(cfg *config.Config, run *runstate.Run, startupNote string) error {
	// 어떤 신호를 받았는지 남기기 위해 NotifyContext 대신 직접 받는다.
	// "종료 신호를 받았다"와 "무슨 신호를 받았다"는 조사에서 전혀 다른 정보다.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	var gotSignal atomic.Value
	go func() {
		sig, ok := <-sigCh
		if !ok {
			return
		}
		gotSignal.Store(sig.String())
		slog.Info("종료 신호 수신", "signal", sig.String(),
			"note", "터미널을 닫거나 Ctrl+C를 누르면 이 신호가 옵니다")
		cancel()
	}()

	// 1. 마스터 암호화 키 확보
	key, generated, err := crypto.LoadOrCreateMasterKey(cfg.MasterKey, cfg.KeyFilePath())
	if err != nil {
		return fmt.Errorf("master key: %w", err)
	}
	if generated {
		slog.Warn("마스터 암호화 키를 새로 생성했습니다. 이 파일을 잃으면 저장된 DB 자격증명을 복호화할 수 없습니다",
			"path", cfg.KeyFilePath())
	}
	secret, err := crypto.NewSecretBox(key)
	if err != nil {
		return fmt.Errorf("secret box: %w", err)
	}

	// 2. 메타 DB 열기 + 스키마 마이그레이션
	st, err := store.Open(ctx, cfg.MetaDBPath(), secret)
	if err != nil {
		return fmt.Errorf("open meta db: %w", err)
	}
	defer st.Close()
	slog.Info("메타 데이터베이스 준비 완료", "path", cfg.MetaDBPath())

	// 3. 사용자가 없으면 슈퍼 어드민 부트스트랩
	boot, err := bootstrap.EnsureSuperadmin(ctx, st)
	if err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}
	bootstrap.PrintCredentials(boot)

	// 4. 기본 모니터링 룰 시딩 (룰이 하나도 없을 때만)
	if n, err := st.SeedBuiltinRules(ctx); err != nil {
		return fmt.Errorf("seed monitor rules: %w", err)
	} else if n > 0 {
		slog.Info("기본 모니터링 룰 생성", "count", n)
	}

	// 5. 서비스 구성
	//
	// 내부 시계를 여기서 만든다. 지금까지 학습한 보정값을 읽어 이어 가고, 앞으로
	// 갱신될 값을 저장할 길을 열어 준다. 재시작마다 처음부터 배우면 그 사이의
	// 2단계 인증이 실패한다.
	offset, err := st.ClockOffset(ctx)
	if err != nil {
		return fmt.Errorf("read clock offset: %w", err)
	}
	appClock := clock.New(offset)
	appClock.SetPersister(func(d time.Duration) {
		// 컨텍스트를 새로 만드는 이유: 이 호출은 인증 요청 도중에 일어나고,
		// 그 요청이 끝나면 컨텍스트가 취소된다. 학습한 값을 못 남기면 다음 재시작
		// 때 다시 배워야 하므로 요청의 수명과 분리한다.
		saveCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := st.SaveClockOffset(saveCtx, d); err != nil {
			slog.Warn("내부 시계 보정값을 저장하지 못했습니다", "err", err)
		}
	})
	if offset != 0 {
		slog.Info("내부 시계 보정값을 이어받았습니다",
			"보정", offset.Round(time.Second),
			"내부시각", appClock.Now().Format(time.RFC3339))
	}

	authn := auth.NewService(st, cfg.SessionTTL, appClock)
	authz := auth.NewAuthorizer(st)

	monCfg := monitor.DefaultConfig()
	monCfg.Interval = cfg.MonitorInterval
	monCfg.SchemaInterval = cfg.SchemaCheckInterval
	monCfg.RawRetention = cfg.MetricRetention
	mon := monitor.NewManager(st, monCfg)

	hostCfg := monitor.DefaultHostConfig()
	hostCfg.Enabled = cfg.HostMonitorEnabled
	hostCfg.Interval = cfg.HostInterval
	hostCfg.Retention = cfg.MetricRetention
	hostCfg.OSLogPath = cfg.HostSyslog
	hostCfg.StartupNote = startupNote
	hostCfg.Version = buildinfo.Get().Version
	hostMon := monitor.NewHostMonitor(st, hostCfg)

	var web fs.FS
	if !cfg.DevMode {
		web, err = dbstudio.WebFS()
		if err != nil {
			return fmt.Errorf("embedded web assets: %w", err)
		}
	}

	// 알림 전송기. 이벤트를 메신저로 내보낸다.
	notifier := notify.New(st, slog.Default())

	// 클러스터 참여자. standalone이면 아무것도 하지 않는다.
	//
	// 서버보다 먼저 만드는 이유: 리플리카는 자기 메타 DB에 쓰지 않아야 하고, 그 사실을
	// store가 알아야 한다(감사 기록은 마스터로 보내고, 부수적인 쓰기는 하지 않는다).
	clusterCfg := cluster.DefaultConfig()
	clusterCfg.Role = cfg.ClusterRole
	clusterCfg.MasterURL = cfg.ClusterMaster
	clusterCfg.Secret = cfg.ClusterSecret
	clusterCfg.NodeName = cfg.ClusterNodeName
	clusterCfg.Advertise = cfg.ClusterAdvertise
	clusterCfg.SyncInterval = cfg.ClusterSync
	clusterCfg.HeartbeatInterval = cfg.ClusterHeartbeat
	clusterCfg.LogKeep = cfg.ClusterLogKeep
	clusterCfg.LogMaxRows = cfg.ClusterLogMax
	node, err := cluster.New(clusterCfg, st, cfg.DataDir, slog.Default())
	if err != nil {
		return fmt.Errorf("cluster: %w", err)
	}
	if node.IsReplica() {
		st.SetReplicaMode(node.SendAudit)
	}
	// 하트비트에 이 컴퓨터의 상태를 실어 보낸다. 노드마다 따로 접속하러 다니지 않고도
	// 각 서버의 CPU·메모리·디스크를 한 화면에서 볼 수 있다.
	node.SetHostSnapshot(func() any {
		if s := hostMon.Latest(); s != nil {
			return s
		}
		return nil
	})

	srv := api.New(cfg, st, authn, authz, mon, web)
	srv.SetHostMonitor(hostMon)
	srv.SetNotifier(notifier)
	srv.SetCluster(node)

	// 앱이 매크로 실행 도중 죽으면 그 기록은 'running'으로 남는다. 화면은 그것을
	// "지금 돌고 있음"으로 보여주고 사용자는 끝나기를 기다린다. 우리는 그 실행이
	// 이어지지 않았음을 아는 유일한 시점에 있으므로 여기서 정리한다.
	if n, err := st.MarkStaleRunsFailed(ctx); err != nil {
		slog.Error("중단된 매크로 실행 기록 정리 실패", "err", err)
	} else if n > 0 {
		slog.Warn("이전 실행에서 중단된 매크로를 실패로 표시했습니다", "count", n)
	}
	if n, err := st.MarkStaleBackupsFailed(ctx); err != nil {
		slog.Error("중단된 백업·복구 기록 정리 실패", "err", err)
	} else if n > 0 {
		slog.Warn("이전 실행에서 중단된 백업·복구를 실패로 표시했습니다", "count", n)
	}
	// 보존 기간이 지난 백업은 부팅할 때 정리한다. 백업이 만들어질 때마다도 정리하지만,
	// 백업을 한동안 만들지 않으면 오래된 파일이 계속 디스크를 차지한다.
	if n, err := srv.Backups().Purge(ctx); err != nil {
		slog.Warn("만료된 백업 정리 실패", "err", err)
	} else if n > 0 {
		slog.Info("만료된 백업을 정리했습니다", "count", n)
	}

	// 실행 표식 갱신. 마지막 갱신 시각이 "몇 시까지 살아 있었는가"의 근거가 된다.
	if run != nil {
		go func() {
			defer applog.Recover("runstate.heartbeat")
			run.Heartbeat(ctx)
		}()
	}

	// 6. 백그라운드 루틴: 세션 정리, 지표 폴링
	//
	// 각 goroutine을 Recover로 감싸는 이유: 여기서 패닉이 나면 프로세스 전체가 죽는다.
	// 서버가 "이유 없이" 꺼지는 대표적인 경로이며, 복구하면 해당 작업만 멈추고
	// 그 사실이 로그에 남는다.
	go func() {
		defer applog.Recover("purgeSessions")
		purgeSessions(ctx, st)
	}()
	// 리플리카에서는 폴러를 돌리지 않는다.
	//
	// 이유: 폴러는 지표와 이벤트를 메타 DB에 쓴다. 리플리카가 쓰면 그 행은 다음 복제
	// 때 사라지고(마스터에는 없으므로), 같은 DB를 두 노드가 동시에 폴링하면 지표가
	// 두 배로 들어간 것처럼 보인다. 수집은 마스터 한 곳에서 한다.
	switch {
	case !cfg.MonitorEnabled:
		slog.Warn("모니터링이 비활성화되어 지표를 수집하지 않습니다 (-monitor=false)")
	case node.IsReplica():
		slog.Info("리플리카에서는 지표를 수집하지 않습니다 (마스터가 수집합니다)")
	default:
		go func() {
			defer applog.Recover("monitor.Run")
			mon.Run(ctx)
		}()
	}
	// 호스트 감시는 커넥션 폴러와 독립적으로 돈다. DB에 하나도 접속하지 못하는
	// 상황이야말로 "이 컴퓨터가 어떤 상태인가"를 가장 알고 싶은 때다.
	if cfg.HostMonitorEnabled {
		// 리플리카의 호스트 감시는 **기록하지 않고 관측만** 한다. 그 컴퓨터의 상태는
		// 하트비트에 실려 마스터의 노드 목록에 남는다 — 각 서버의 상태는 보이면서도
		// 리플리카가 자기 DB에 쓰는 일은 없다.
		hostMon.SetObserveOnly(node.IsReplica())
		go func() {
			defer applog.Recover("hostMonitor.Run")
			hostMon.Run(ctx)
		}()
	}

	// 매크로 자동 실행. 스케줄러는 시각을 보고, 이벤트 수신자는 모니터가 부른다.
	//
	// 모니터링이 꺼져 있으면 이벤트가 생기지 않으므로 조건 트리거도 동작하지 않는다.
	// 정기 실행은 모니터링과 무관하게 돈다 — 둘을 묶으면 "매일 3시 백업"을 쓰려고
	// 지표 수집을 켜야 하는 이상한 의존이 생긴다.
	// 이벤트를 받는 곳이 둘이다: 매크로 자동 실행과 메신저 알림.
	// 둘은 서로를 몰라야 하고(한쪽이 느려도 다른 쪽이 막히면 안 된다), 그 조합을
	// 아는 것은 여기, 부팅 절차의 몫이다.
	scheduler := srv.Macros().Scheduler()
	sinks := monitor.FanOut{scheduler, notifier}
	mon.SetEventSink(sinks)
	hostMon.SetEventSink(sinks)
	go func() {
		defer applog.Recover("notify.Run")
		notifier.Run(ctx)
	}()
	// 클러스터 루프. 마스터는 로그를 정리하고 노드를 지켜보며, 리플리카는 변경을 받아 적는다.
	if node.Enabled() {
		go func() {
			defer applog.Recover("cluster.Run")
			node.Run(ctx)
		}()
	}
	// 매크로 자동 실행도 마스터에서만 돈다. 노드마다 돌면 "매일 3시 백업"이 노드 수만큼
	// 실행되고, 그 기록은 리플리카에서 사라진다.
	if node.IsReplica() {
		slog.Info("리플리카에서는 매크로 자동 실행을 하지 않습니다 (마스터가 실행합니다)")
	} else {
		go func() {
			defer applog.Recover("macro.Scheduler")
			scheduler.Run(ctx)
		}()
	}

	// 7. 리스닝 + graceful shutdown
	errCh := make(chan error, 1)
	go func() {
		defer applog.Recover("listen")
		mode := "embedded"
		if cfg.DevMode {
			mode = "dev (" + cfg.WebDir + ")"
		}
		trust := "off (원격 주소를 그대로 사용)"
		if cfg.TrustProxy {
			trust = "X-Forwarded-For (" + strings.Join(cfg.TrustedProxies, ", ") + " 에서 온 경우)"
		}
		slog.Info("HTTP 리스닝 시작",
			"addr", cfg.Addr, "frontend", mode, "data", cfg.DataDir,
			"monitor", cfg.MonitorEnabled, "devMode", cfg.DevMode, "clientIP", trust)
		errCh <- srv.Listen()
	}()

	select {
	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("listen: %w", err)
		}
		// 신호도 오류도 없이 리스너가 끝나는 경우다(Shutdown 호출 등).
		slog.Warn("리스너가 스스로 종료했습니다", "reason", "listener returned without error")
		return nil
	case <-ctx.Done():
		sig, _ := gotSignal.Load().(string)
		if sig == "" {
			sig = "(신호 없음: 내부에서 종료를 요청했습니다)"
		}
		slog.Info("정리 시작", "trigger", sig, "timeout", shutdownTimeout)
		shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancelShutdown()
		if err := srv.App().ShutdownWithContext(shutdownCtx); err != nil {
			// 정리 실패도 기록해야 한다. 여기서 조용히 나가면 커넥션이 끊긴 이유를 알 수 없다.
			slog.Error("정리 중 오류", "err", err, "timeout", shutdownTimeout)
			return fmt.Errorf("shutdown: %w", err)
		}
		slog.Info("정리 완료")
		return nil
	}
}

// reportPreviousRun은 이전 실행 표식을 확인해 알리고, 표식을 우리가 가져도 되는지 답한다.
//
// 표식이 남아 있는 경우는 두 가지이고, 사용자가 해야 할 일이 서로 다르다.
//  1. 그 프로세스가 아직 살아 있다 → 같은 데이터 디렉터리를 두 인스턴스가 쓰고 있다(설정 오류).
//     이때 표식을 덮어쓰면 안 된다. 우리가 종료할 때 표식을 지워, 살아 있는 쪽의
//     비정상 종료를 나중에 감지할 수 없게 만든다.
//  2. 이미 없다 → 그 실행은 종료 처리를 할 기회를 얻지 못했다(강제 종료·전원 차단·크래시).
//     크래시 파일로 크래시 여부를 구분해 알려 준다.
//
// 두 번째 반환값은 "이전 실행이 비정상으로 끝났다"는 설명이다. 정상 종료였으면 비어 있다.
func reportPreviousRun(cfg *config.Config, crashPath string) (takeOver bool, note string) {
	prev, err := runstate.Read(runstate.Path(cfg.DataDir))
	if err != nil {
		slog.Warn("이전 실행 표식을 읽지 못했습니다", "err", err)
		return true, ""
	}
	if prev == nil {
		return true, "" // 정상 종료했다
	}

	if prev.LooksLive() {
		slog.Warn("같은 데이터 디렉터리를 다른 인스턴스가 쓰고 있는 것 같습니다",
			"실행중", runstate.Describe(prev),
			"data", cfg.DataDir,
			"hint", "인스턴스마다 -data 를 따로 주세요. 두 프로세스가 같은 메타 DB를 쓰면 "+
				"세션·이벤트가 서로 덮어씁니다")
		return false, ""
	}

	cause := "강제 종료로 보입니다 (작업 관리자·kill -9·다른 프로세스의 종료 명령·전원 차단). " +
		"크래시 리포트는 비어 있습니다"
	attrs := []any{"이전실행", runstate.Describe(prev)}
	if crash := crashReport(crashPath); crash != "" {
		cause = "런타임 크래시로 보입니다"
		attrs = append(attrs, "원인", cause, "crashFile", crashPath, "crash", crash)
	} else {
		attrs = append(attrs, "원인", cause)
	}
	slog.Warn("이전 실행이 종료 기록을 남기지 않았습니다", attrs...)
	return true, cause + " / " + runstate.Describe(prev)
}

// crashReport는 크래시 파일의 첫 줄들을 돌려준다. 비어 있으면 빈 문자열이다.
func crashReport(path string) string {
	if path == "" {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return ""
	}
	// 전체 스택은 파일에 있다. 로그에는 원인을 알 만큼만 넣는다.
	text := strings.TrimSpace(string(data))
	if len(text) > 400 {
		text = text[:400] + "…"
	}
	return strings.ReplaceAll(text, "\n", " / ")
}

// orNone은 빈 문자열을 로그에서 읽히는 값으로 바꾼다.
func orNone(s string) string {
	if s == "" {
		return "(없음: stderr만 사용)"
	}
	return s
}

// shutdownTimeout은 진행 중인 요청을 기다려 줄 시간이다.
// 이 시간을 넘기면 남은 커넥션을 끊고 종료한다.
const shutdownTimeout = 10 * time.Second

// purgeSessions는 만료된 세션과 2단계 인증 챌린지를 주기적으로 정리한다.
func purgeSessions(ctx context.Context, st *store.Store) {
	ticker := time.NewTicker(30 * time.Minute)
	defer ticker.Stop()
	for {
		// 시작 직후 한 번, 이후 주기적으로 실행한다.
		if n, err := st.PurgeExpiredSessions(ctx); err != nil {
			slog.Warn("만료 세션 정리 실패", "err", err)
		} else if n > 0 {
			slog.Info("만료 세션 정리", "count", n)
		}
		// 챌린지는 5분이면 죽지만 행은 남는다. 로그인 시도마다 하나씩 쌓이므로
		// 같은 주기로 함께 치운다.
		if n, err := st.PurgeExpiredTOTPChallenges(ctx); err != nil {
			slog.Warn("만료된 2단계 인증 챌린지 정리 실패", "err", err)
		} else if n > 0 {
			slog.Debug("만료된 2단계 인증 챌린지 정리", "count", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
