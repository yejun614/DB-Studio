package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"dbstudio/internal/bootstrap"
	"dbstudio/internal/cluster"
	"dbstudio/internal/config"
	"dbstudio/internal/crypto"
	"dbstudio/internal/runstate"
	"dbstudio/internal/store"
)

// 서버를 띄우지 않고 하는 일들.
//
// ── 왜 하위 명령인가 ────────────────────────────────────────────────
// 플래그로 두면(`-reset-password`) 서버를 띄우는 명령줄에 섞여 들어갈 수 있고,
// 그 조합이 무엇을 뜻하는지 아무도 모른다. 하위 명령은 **그 일만 하고 끝난다**는
// 것을 이름으로 말한다.
//
// 데이터 디렉터리 플래그(-data)와 환경변수(DBSTUDIO_DATA·DBSTUDIO_MASTER_KEY)는
// 서버와 같은 것을 쓴다. 여기서 따로 정의하면 서버는 환경변수로 돌고 있는데
// 이 명령은 엉뚱한 디렉터리를 열게 된다.

const commandUsage = `사용법:
  dbstudio                                   서버를 띄운다
  dbstudio reset-password [플래그] [아이디]   비밀번호를 새 랜덤 값으로 바꾼다
                                             (아이디를 비우면 superadmin)

플래그는 서버와 같습니다. 데이터 디렉터리는 -data 또는 DBSTUDIO_DATA,
마스터 키는 master.key 파일 또는 DBSTUDIO_MASTER_KEY 입니다.

예:
  dbstudio reset-password
  dbstudio reset-password -data /srv/dbstudio/data
  dbstudio reset-password -data ./data dev1
`

func runCommand(name string, args []string) error {
	err := dispatch(name, args)
	if err == nil || errors.Is(err, flagHelp) {
		return err
	}
	// 하위 명령의 오류는 사람이 지금 읽는다. 구조화 로그로 감싸면 줄바꿈이
	// `\n` 으로 보이고 경로의 역슬래시가 두 개로 늘어난다 — 안내가 가장
	// 필요한 순간에 가장 읽기 어려워진다.
	fmt.Fprintln(os.Stderr, "오류: "+err.Error())
	return fmt.Errorf("%w: %w", errLogged, err)
}

func dispatch(name string, args []string) error {
	switch name {
	case "reset-password":
		return resetPassword(args)
	case "help":
		fmt.Print(commandUsage)
		return flagHelp
	default:
		fmt.Fprint(os.Stderr, commandUsage)
		return fmt.Errorf("모르는 명령입니다: %s", name)
	}
}

// resetPassword는 한 계정의 비밀번호를 새 랜덤 값으로 바꾼다.
//
// ── 왜 서버가 아니라 명령줄인가 ─────────────────────────────────────
// 이것이 필요한 순간은 **아무도 로그인할 수 없는** 순간이다. 화면에 두면 그
// 화면에 들어가기 위해 로그인이 필요하고, 그러면 이 기능은 필요한 때에 쓸 수
// 없다. 대신 데이터 디렉터리와 master.key 를 읽을 수 있어야 한다 —
// 그 둘을 가진 사람은 이미 이 앱의 모든 것을 가진 사람이다.
func resetPassword(args []string) error {
	for _, a := range args {
		if a == "-h" || a == "--help" || a == "help" {
			fmt.Print(commandUsage)
			return flagHelp
		}
	}

	cfg, err := config.Load(args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return flagHelp
		}
		return err
	}
	username := ""
	if len(cfg.Args) > 0 {
		username = cfg.Args[0]
	}
	if len(cfg.Args) > 1 {
		return fmt.Errorf("아이디는 하나만 줄 수 있습니다: %s", strings.Join(cfg.Args, " "))
	}

	// 메타 DB 가 없으면 멈춘다.
	//
	// 없는 것을 열면 store 가 **새 DB 를 만들고** 마이그레이션까지 돌린다.
	// 그러면 "그런 계정이 없습니다"라는 답을 받게 되는데, 진짜 원인은 -data 를
	// 잘못 짚은 것이다 — 그 사이에 엉뚱한 곳에 빈 데이터 디렉터리가 하나 생긴다.
	if _, err := os.Stat(cfg.MetaDBPath()); os.IsNotExist(err) {
		return fmt.Errorf("메타 데이터베이스가 없습니다: %s\n"+
			"-data 로 데이터 디렉터리를 가리키세요 (지금 본 곳: %s)",
			cfg.MetaDBPath(), cfg.DataDir)
	}

	// 돌고 있는 서버가 있어도 막지 않는다.
	//
	// 이 명령이 필요한 때는 대개 서버가 잘 돌고 있는데 들어갈 수 없는 때다.
	// 비밀번호는 어디에도 캐시되지 않으므로 다시 띄우지 않아도 곧 듣는다.
	// 그래도 알려는 준다 — 세션을 지우므로 그 계정으로 열려 있던 창은 로그아웃된다.
	if m, _ := runstate.Read(runstate.Path(cfg.DataDir)); m != nil && m.LooksLive() {
		fmt.Fprintf(os.Stderr,
			"참고: 서버가 돌고 있는 것 같습니다 (pid %d, %s). 다시 띄울 필요는 없지만, "+
				"그 계정으로 열려 있던 창은 로그아웃됩니다.\n", m.PID, m.Addr)
	}
	// 리플리카에서 바꾸면 마스터가 덮어쓴다. 조용히 되돌려지는 것이 최악이라
	// 미리 멈춘다 — 여기서 바꿔도 다음 동기화에 사라진다.
	if cfg.ClusterRole == cluster.RoleReplica {
		return errors.New("이 노드는 리플리카입니다. 사용자는 마스터가 들고 있으므로 " +
			"마스터 노드에서 실행하세요 (여기서 바꿔도 다음 동기화에 되돌려집니다)")
	}

	key, generated, err := crypto.LoadOrCreateMasterKey(cfg.MasterKey, cfg.KeyFilePath())
	if err != nil {
		return fmt.Errorf("master key: %w", err)
	}
	if generated {
		// 키가 새로 생겼다는 것은 이 데이터 디렉터리의 키가 아니라는 뜻이다.
		// 그 키로 DB 를 열면 저장된 자격증명을 하나도 풀지 못한다.
		return fmt.Errorf("마스터 키가 없어 새로 만들었습니다: %s\n"+
			"이 데이터 디렉터리의 것이 아닙니다. 원래 키를 두거나 DBSTUDIO_MASTER_KEY 로 주세요",
			cfg.KeyFilePath())
	}
	secret, err := crypto.NewSecretBox(key)
	if err != nil {
		return fmt.Errorf("secret box: %w", err)
	}

	// 이 명령의 로그는 사람이 지금 보고 있다. 파일로 보내지 않는다.
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	st, err := store.Open(ctx, cfg.MetaDBPath(), secret)
	if err != nil {
		return fmt.Errorf("open meta db: %w", err)
	}
	defer st.Close()

	res, err := bootstrap.ResetPassword(ctx, st, username)
	if res != nil && err != nil {
		// 비밀번호는 바뀌었고 부수적인 것만 실패했다. 값을 먼저 보여 준다 —
		// 여기서 오류만 내면 사람은 바뀐 비밀번호를 모른 채 다시 돌린다.
		bootstrap.PrintResetCredentials(res)
		fmt.Fprintf(os.Stderr, "경고: %v\n", err)
		return nil
	}
	if err != nil {
		return err
	}
	bootstrap.PrintResetCredentials(res)
	fmt.Fprintf(os.Stdout, "  데이터 디렉터리: %s\n\n", filepath.Clean(cfg.DataDir))
	return nil
}
