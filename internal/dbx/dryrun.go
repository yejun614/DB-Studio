package dbx

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"dbstudio/internal/model"
)

// 미리 실행해 보기(dry run).
//
// 계획을 만들고 나서야 SQL이 깨진 것을 아는 흐름은 비싸다: 마이그레이션을 만들고,
// 리뷰를 받고, 실행하고, 실패를 보고, 초안을 고치고, 다시 처음부터 — 한 바퀴가
// 사람 여럿의 시간이다. 그런데 "이 문장이 이 DB에서 실행되는가"는 계획을 만들기
// 전에 물어볼 수 있는 질문이다.
//
// 진짜로 실행해 본다. 문법 검사기를 따로 만들지 않는 이유: DB가 거절하는 것은
// 문법만이 아니다. "AUTO_INCREMENT 컬럼은 키여야 한다", "이 타입에는 길이를 줄 수
// 없다", "이름이 너무 길다" 같은 것은 그 엔진의 그 버전만이 안다. 흉내 낸 검사기는
// 언제나 진짜와 조금씩 다르고, 그 차이가 바로 사람이 걸려 넘어지는 자리다.
//
// 대신 **그림자 DB**에서 실행한다. 빈 DB를 새로 만들고, 기준 구조를 세우고, 계획을
// 실행해 본 뒤, 통째로 지운다. 대상 DB는 손대지 않는다.

// dropTimeout은 그림자 DB를 지우는 데 주는 시간이다. 요청이 취소된 뒤에도
// 지우기는 끝까지 해야 하므로 새 시간표로 돈다.
const dropTimeout = 30 * time.Second

// DryRunReport는 미리 실행해 본 결과다.
type DryRunReport struct {
	// OK는 모든 문장이 실행됐는지다.
	OK bool `json:"ok"`
	// Steps는 문장별 결과다. 실패한 문장에는 Error가 채워진다.
	Steps []ExecStep `json:"steps"`
	// FailedIndex는 처음 실패한 문장의 위치다. 실패가 없으면 -1이다.
	FailedIndex int `json:"failedIndex"`
	// Error는 첫 실패 사유다.
	Error string `json:"error,omitempty"`
	// Skipped는 검사를 하지 못한 까닭이다. 비어 있지 않으면 OK는 뜻이 없다.
	//
	// "실패"와 "검사하지 못함"을 가르는 것이 이 항목의 일이다. 둘을 섞으면 권한이
	// 없어 검사를 못 한 것을 계획이 잘못된 것으로 읽게 된다.
	Skipped string `json:"skipped,omitempty"`
	// Where는 어디서 실행했는지다(그림자 DB 이름). 화면에 그대로 보여준다.
	Where string `json:"where,omitempty"`
	// SeedFailed는 기준 구조를 세우다 실패했음을 뜻한다. 이때 계획은 아직
	// 시험대에 오르지도 못했다.
	SeedFailed bool `json:"seedFailed,omitempty"`
}

// DryRunDDL은 그림자 DB에서 문장들을 실행해 본다.
//
// seed는 기준 구조를 세우는 문장이고(대개 "빈 DB → 기준 스키마"의 CREATE 문),
// stmts가 검사할 계획이다. 둘을 나누는 이유: seed가 실패하면 그것은 계획의 잘못이
// 아니라 우리가 시험대를 못 세운 것이므로, 계획을 나무라서는 안 된다.
func DryRunDDL(ctx context.Context, a Adapter, t Target, seed, stmts []string,
	opts ExecOptions) (*DryRunReport, error) {

	sa, ok := a.(*sqlAdapter)
	if !ok || !sa.caps.Migrate {
		return &DryRunReport{
			FailedIndex: -1,
			Skipped:     fmt.Sprintf("%s 는 미리 실행해 볼 수 없습니다", t.Conn.Kind),
		}, nil
	}
	if t.Conn.Kind == model.KindOracle {
		// Oracle에서 "새 DB"는 스키마(사용자)를 만드는 일이고, 그러려면 DBA 권한과
		// 테이블스페이스가 필요하다. 검사 한 번을 위해 요구할 것이 아니다.
		return &DryRunReport{
			FailedIndex: -1,
			Skipped:     "Oracle은 그림자 스키마를 만들려면 DBA 권한이 필요해 미리 실행해 보지 않습니다",
		}, nil
	}

	sandbox, cleanup, err := openSandbox(ctx, sa, t)
	if err != nil {
		return &DryRunReport{FailedIndex: -1, Skipped: err.Error()}, nil
	}
	defer cleanup()

	// 기준 구조를 세운다. 여기서 실패하면 계획을 시험할 수 없다.
	//
	// 오류를 무시하고 계속 가지 않는다. 절반만 선 기준 위에서 계획을 돌리면 "없는
	// 테이블을 고치려 한다"는 엉뚱한 실패가 나오고, 사람은 초안을 의심하게 된다.
	if len(seed) > 0 {
		rep, serr := sa.ExecDDL(ctx, sandbox.target, seed, opts)
		if serr != nil {
			return &DryRunReport{FailedIndex: -1, SeedFailed: true,
				Skipped: "그림자 DB에 기준 구조를 세우지 못했습니다: " + serr.Error()}, nil
		}
		if rep.Error != "" {
			return &DryRunReport{
				FailedIndex: -1, SeedFailed: true, Where: sandbox.label,
				Skipped: fmt.Sprintf("그림자 DB에 기준 구조를 세우다 %d번째 문장에서 막혔습니다: %s",
					rep.FailedIndex+1, rep.Error),
			}, nil
		}
	}

	rep, err := sa.ExecDDL(ctx, sandbox.target, stmts, opts)
	if err != nil {
		return &DryRunReport{FailedIndex: -1, Where: sandbox.label,
			Skipped: "그림자 DB에서 실행하지 못했습니다: " + err.Error()}, nil
	}
	return &DryRunReport{
		OK:          rep.Error == "",
		Steps:       rep.Steps,
		FailedIndex: rep.FailedIndex,
		Error:       rep.Error,
		Where:       sandbox.label,
	}, nil
}

// sandbox는 실행해 보고 버릴 DB다.
type sandbox struct {
	target Target
	label  string
}

// openSandbox는 그림자 DB를 만들고, 지우는 함수를 함께 돌려준다.
func openSandbox(ctx context.Context, sa *sqlAdapter, t Target) (*sandbox, func(), error) {
	name := "dbstudio_dryrun_" + randomSuffix()

	if t.Conn.Kind == model.KindSQLite {
		// SQLite는 파일 하나가 DB 하나다. 임시 파일을 만들고 끝나면 지운다.
		dir, err := os.MkdirTemp("", "dbstudio-dryrun-")
		if err != nil {
			return nil, nil, fmt.Errorf("임시 파일을 만들지 못했습니다: %w", err)
		}
		path := filepath.Join(dir, name+".db")
		shadow := shadowTarget(t, path)
		return &sandbox{target: shadow, label: "임시 파일"}, func() { _ = os.RemoveAll(dir) }, nil
	}

	// 서버형 DB는 관리용 DB에 붙어 새 DB를 만든다.
	admin := shadowTarget(t, bootstrapDatabase(t.Conn.Kind))
	db, err := sa.open(admin, 1)
	if err != nil {
		return nil, nil, fmt.Errorf("그림자 DB를 만들 서버에 붙지 못했습니다: %w", err)
	}
	if _, err := db.ExecContext(ctx, "CREATE DATABASE "+quoteDBName(t.Conn.Kind, name)); err != nil {
		db.Close()
		// 권한이 없는 것이 가장 흔하다. 사유를 그대로 전한다 — "검사할 수 없다"와
		// "계획이 틀렸다"는 사람이 할 일이 전혀 다르다.
		return nil, nil, fmt.Errorf("그림자 DB(%s)를 만들지 못했습니다: %w", name, err)
	}

	cleanup := func() {
		// 지우기는 반드시 한다. 남으면 서버에 쓰레기 DB가 쌓이고, 다음 사람은
		// 그것이 무엇인지 알 수 없다.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), dropTimeout)
		defer cancel()
		_, _ = db.ExecContext(ctx, "DROP DATABASE "+quoteDBName(t.Conn.Kind, name))
		db.Close()
	}
	return &sandbox{target: shadowTarget(t, name), label: name}, cleanup, nil
}

// shadowTarget은 같은 서버의 다른 DB를 가리키는 접속 대상을 만든다.
func shadowTarget(t Target, database string) Target {
	conn := *t.Conn
	conn.DatabaseName = database
	// search_path는 원래 DB의 스키마를 가리킨다. 그림자 DB에는 그 스키마가 없으므로
	// 지운다 — 세울 스키마는 seed가 만든다.
	if len(conn.Options) > 0 {
		opts := model.Options{}
		for k, v := range conn.Options {
			if strings.EqualFold(k, "search_path") {
				continue
			}
			opts[k] = v
		}
		conn.Options = opts
	}
	return Target{Conn: &conn, Secret: t.Secret}
}

// quoteDBName은 DB 이름을 종류에 맞게 감싼다.
//
// 이름은 우리가 만든 것(dbstudio_dryrun_<16진수>)이라 위험한 글자가 없지만,
// 감싸는 규칙은 종류마다 다르므로 한곳에 적어 둔다.
func quoteDBName(kind model.DBKind, name string) string {
	switch kind {
	case model.KindPostgres:
		return `"` + name + `"`
	case model.KindMSSQL:
		return "[" + name + "]"
	default:
		return "`" + name + "`"
	}
}

func randomSuffix() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "0000"
	}
	return hex.EncodeToString(b[:])
}
