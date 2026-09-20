package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"dbstudio/internal/model"
)

// 담당 노드에서 덤프를 만들어 스트림으로 흘려보내는 자리 (P33-1).
//
// ── 누가 덤프를 만드는가 ───────────────────────────────────────────
// 담당 노드가 지정된 DB는 **마스터가 닿지 못할 수 있다** — 그것이 담당 노드를 지정한
// 이유다. 그런데 백업 요청(`POST /backups/...`)은 쓰기라서 리플리카에서 오면 마스터로
// 넘어가고, 그러면 **닿지도 못하는 마스터가 덤프를 시도**한다. 실제로 그 자리에서
// 1045가 났다(버전 캡처와 같은 원인).
//
// 그래서 마스터가 그 일을 담당 노드에 맡긴다. 결과만 받는 것이 아니라 **파일 전체**를
// 받는다. 백업은 값이 아니라 파일이 남는 일이라 지표·스키마와 같은 방식이 통하지 않는다.
//
// ── 방향: 마스터가 부탁하고 노드가 흘려보낸다 ───────────────────────
//	마스터 : 이름을 정해 "이 커넥션을 떠라" → 담당 노드 (POST /node/dump)
//	노드   : 자기 DB에 붙어 덤프 → 응답 본문으로 흘려보낸다
//	마스터 : 받아서 그 이름으로 보관하고 기록을 남긴다
//
// 마스터→노드 HTTP 는 된다(클러스터 라우팅이 쓰는 그 길이다). 막히는 것은 **노드 뒤의
// DB** 이고, 그것이 담당 노드를 지정한 이유다. 그래서 마스터가 노드를 부르는 것은
// 문제가 되지 않는다.
//
// 노드가 파일로 남기지 않고 곧바로 흘려보내는 이유: 그 노드에 파일을 남기면 그 노드가
// 사라질 때 백업도 사라진다 — 백업의 목적 자체가 무너진다.
//
// ── 파일 이름은 경계를 넘지 않는다 ─────────────────────────────────
// 처음에는 노드가 만든 이름을 그대로 마스터에 보내려 했다. 그것이 경로 조작
// (`../../etc/x`)을 막는 검사를 한 곳 더 만들게 했고, 그 검사를 빠뜨리면 디렉터리
// 탈출이 된다. 지금은 **마스터가 이름을 정해 내려보내고** 노드는 그 이름을 그대로 쓴다.
// 노드가 이름을 만들어 올릴 일이 없으므로 검사할 것도 없다 — 경계를 넘는 값이 없다.
// 그 검사(FilePath)는 그대로 남아 있고, 모든 파일 경로가 여전히 그곳을 지난다.

// removeQuietly는 파일을 지운다. 이미 없으면 성공으로 본다.
//
// "없음"을 실패로 만들면 정리 실패로 기록되고, 그 경고가 진짜 문제를 가린다.
// 없어진 파일을 지우려 한 것은 결과적으로 원하는 상태다.
func removeQuietly(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// WriteDumpTo는 이 노드에서 그 커넥션의 덤프를 만들어 w로 흘려보낸다.
//
// 담당 노드가 자기 DB에 붙어 덤프하는 자리다. 마스터의 요청(`/node/dump`)이 부른다.
//
// 파일을 거치지 않고 곧바로 스트림에 쓰는 이유: 노드에 임시 파일을 만들면 그것을
// 지우는 일이 하나 더 생기고, 지우지 못한 임시 파일이 노드의 디스크를 조용히 채운다.
// 받는 쪽(마스터)이 잘린 것을 구분할 수 있으므로 중간 파일이 없어도 안전하다
// (마스터는 임시 이름에 받고 완성된 뒤에 옮긴다).
func (s *Service) WriteDumpTo(ctx context.Context, w io.Writer, p StreamParams) error {
	pw := newStreamWriter(w, s.cfg.MaxBytes)

	format := FormatFor(p.Target.Conn.Kind)
	if format == FormatRedis && p.Options.Scope == ScopeSchema {
		return fmt.Errorf("Redis는 구조만 덤프할 수 없습니다. 데이터 또는 전체를 고르세요")
	}
	// 헤더는 여기서 쓰지 않는다 — dumpSQL/dumpMongo/dumpRedis 가 각자 첫 줄에 쓴다
	// (파일만 보고 무엇인지 알 수 있어야 한다는 규칙은 그쪽에 있다). 여기서 또 쓰면
	// 실측에서 헤더가 두 번 찍히는 것을 확인했다.
	// 통계는 이 경로에서 쓰지 않는다 — 기록은 마스터가 만들고, 노드는 바이트만 흘려보낸다.
	// 그래도 반환값을 받는 이유는 시그니처를 공유하기 때문이다(덤프 코드는 어디로
	// 가는지 모른 채 그대로 돈다).
	sp := StartBackupParams{Target: p.Target, Options: p.Options, Actor: p.Actor, Trigger: p.Trigger}
	var err error
	switch format {
	case FormatJSONL:
		_, err = s.dumpMongo(ctx, pw, sp)
	case FormatRedis:
		_, err = s.dumpRedis(ctx, pw, sp)
	default:
		_, err = s.dumpSQL(ctx, pw, sp)
	}
	if err != nil {
		return err
	}
	return pw.Close()
}

// StreamParams는 이 노드에서 만들 덤프의 조건이다.
type StreamParams struct {
	Target  Target
	Options Options
	Actor   *model.User
	Trigger string
}
