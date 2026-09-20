package backup

import (
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"dbstudio/internal/model"
)

// 마스터 쪽: 담당 노드가 올린 덤프를 받아 보관한다.
//
// ── 이름은 마스터가 정한다 ────────────────────────────────────────
// 노드가 이름을 만들어 올리면 마스터가 그것을 검사해야 한다(`../../etc/x` 같은 값).
// 검사를 한 곳 더 만들면 빠뜨릴 수 있고, 빠뜨린 날이 곧 디렉터리 탈출이다. 그래서
// **마스터가 이름을 정해 노드에 준다**(`AllocateDumpName`). 노드는 받은 이름을 그대로
// 쓰고, 마스터는 자기가 만든 이름만 받으므로 경계를 넘는 값이 없다.
//
// `FilePath`의 경로 검사는 그대로 남아 있다 — 모든 파일 경로가 여전히 그곳을 지난다.

// AllocateDumpName은 이 노드가 보관할 덤프 파일 이름을 만든다.
//
// 확장자를 받는 이유: 그 값이 **복구기가 읽는 방법**을 정한다(.sql / .jsonl / .redis).
// 마스터가 DB 종류를 알고 있으므로(커넥션 기록에 있다) 거기서 정해 내려보낸다.
func (s *Service) AllocateDumpName(kind string) (string, error) {
	ext, err := DumpExtFor(kind)
	if err != nil {
		return "", err
	}
	name := fmt.Sprintf("node-%s%s", time.Now().UTC().Format("20060102T150405.000"), ext)
	// 자기가 만든 이름이지만 여기서 한 번 확인한다. 이 규칙이 지켜지지 않는 날이 곧
	// 디렉터리 탈출이 되는 날이라는 것이 FilePath 주석의 판단이다.
	if _, err := s.FilePath(name); err != nil {
		return "", err
	}
	return name, nil
}

// DumpExtFor는 DB 종류에 맞는 덤프 파일 확장자를 준다.
//
// FormatFor(덤프 내용의 형식)와 함께 두는 이유: 둘은 짝이고, 한쪽만 알면 나중에
// "무엇으로 열어야 하는가"를 알 수 없다.
//
// ── 왜 default가 없는가 ────────────────────────────────────────────
// 처음에는 FormatFor처럼 default로 SQL을 돌려주게 만들려 했다. 그런데 이 값으로
// **파일을 만들고**, 나중에 복구기가 그 값으로 읽는 방법을 정한다. 모르는 종류에
// SQL을 배정하면 그 파일은 있어도 열 수 없고, 그 사실은 복구 시점에 드러난다.
//
// 게다가 FormatFor의 default는 이 함수가 아니라 **덤프 경로**에서도 쓰인다 — 거기서는
// "관계형으로 취급한다"가 맞다(스토리지·브로커는 백업 대상이 아니다). 파일 이름을
// 정하는 곳에서는 그 관대함이 위험하므로, 여기서는 아는 종류만 받는다.
func DumpExtFor(kind string) (string, error) {
	switch FormatFor(model.DBKind(kind)) {
	case FormatJSONL:
		return ".jsonl.gz", nil
	case FormatRedis:
		return ".redis.gz", nil
	}
	// 관계형으로 취급되는 종류인지 확인한다. FormatFor의 default를 그대로 믿으면
	// 스토리지·브로커·벡터까지 ".sql.gz"가 되어, 그 파일을 복구할 수 없게 된다.
	switch model.DBKind(kind) {
	case model.KindMySQL, model.KindPostgres, model.KindMSSQL, model.KindOracle,
		model.KindSQLite, model.KindClickHouse:
		return ".sql.gz", nil
	}
	return "", fmt.Errorf("이 DB 종류는 백업할 수 없습니다: %s", kind)
}

// ReceiveDump는 올라온 덤프를 이름 그대로 이 노드의 백업 디렉터리에 저장한다.
//
// 이름은 마스터가 만든 것만 온다(위 주석). 그래도 FilePath를 지나는 이유는 그 함수가
// 모든 경로의 유일한 관문이기 때문이다 — 여기서 우회하면 "모든 경로가 그곳을 지난다"는
// 성질이 깨지고, 그 성질이 다음 사람의 안전장치다.
func (s *Service) ReceiveDump(fileName string, r io.Reader) (string, error) {
	if err := s.EnsureDir(); err != nil {
		return "", err
	}
	name := filepath.Base(filepath.FromSlash(strings.TrimSpace(fileName)))
	path, err := s.FilePath(name)
	if err != nil {
		return "", err
	}

	// ── 왜 임시 파일에 받고 옮기는가 ────────────────────────────────
	// 전송이 중간에 끊기면 잘린 파일이 남는다. 그 파일은 목록에 있고 이름도 멀쩡해서,
	// **복구 시점에야** 쓸 수 없다는 것을 알게 된다 — 가장 늦게 발견되는 실패다.
	// 완성된 뒤에 이름을 바꾸면 그 창이 없다.
	tmp := path + ".incoming"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return "", fmt.Errorf("덤프를 저장할 수 없습니다: %w", err)
	}

	// ── 크기 상한을 건다 ───────────────────────────────────────────
	// 담당 노드는 자기 상한(-backup-max-mb)을 지키지만, 그 값을 이 노드는 모른다.
	// 상한 없이 받으면 잘못된 노드 하나가 마스터의 디스크를 채울 수 있다. 보내는 쪽의
	// 상한은 **압축 전** 텍스트 기준이라 압축 후 파일은 그보다 작으므로, 넉넉히 두 배로
	// 잡아도 실제로 걸리지 않는다.
	limit := s.cfg.MaxBytes * 2
	written, cerr := io.Copy(f, io.LimitReader(r, limit))
	if cerr != nil {
		f.Close()
		removeQuietly(tmp)
		return "", fmt.Errorf("덤프를 받는 중에 끊겼습니다: %w", cerr)
	}
	if err := f.Close(); err != nil {
		removeQuietly(tmp)
		return "", fmt.Errorf("덤프를 저장하지 못했습니다: %w", err)
	}
	// LimitReader가 상한에 정확히 닿으면 더 있을 수 있다 — 잘린 파일이므로 버린다.
	// 잘린 것을 성공으로 적으면 복구가 필요할 때 없다는 것을 안다.
	if written >= limit {
		removeQuietly(tmp)
		return "", fmt.Errorf("덤프가 상한(%d MB)을 넘습니다. 대상을 좁히거나 -backup-max-mb를 올리세요",
			s.cfg.MaxBytes>>20)
	}

	if err := os.Rename(tmp, path); err != nil {
		removeQuietly(tmp)
		return "", fmt.Errorf("덤프를 제자리에 두지 못했습니다: %w", err)
	}
	// ── 받은 것이 온전한 덤프인지 확인한다 ──────────────────────────
	//
	// ── 왜 이것이 반드시 필요한가 (실제로 겪었다) ──────────────────
	// 노드는 응답을 **흘려보내기 시작한 뒤에** 덤프가 실패하면 상태 코드를 바꿀 수 없다.
	// 그래서 덤프가 중간에 죽으면 마스터는 "200 OK + 잘린 본문"을 받는다.
	//
	// 검사 없이 두면 그 잘린 파일이 **성공으로 기록**되고, 쓸 수 없다는 사실은 복구가
	// 필요할 때 드러난다 — 이 저장소가 가장 나쁘다고 부르는 실패다. 실제로 이 검사를
	// 넣기 전에 0바이트 파일이 success 로 남는 것을 실측에서 확인했다.
	//
	// gzip 은 끝에 CRC32 와 원본 크기를 적어 두므로, 잘린 파일은 열어서 끝까지 읽는
	// 순간 반드시 실패한다. 그래서 "완전한가"를 따로 표시할 필요가 없다(트레일러 헤더를
	// 붙이는 방법도 있지만, 이쪽은 손상 일반도 함께 잡는다).
	if err := verifyGzipDump(path); err != nil {
		removeQuietly(path)
		return "", fmt.Errorf("받은 덤프가 온전하지 않습니다: %w", err)
	}
	return name, nil
}

// verifyGzipDump는 gzip 파일이 끝까지 온전한지 확인한다.
//
// 전부 읽는 비용이 든다(압축 해제). 그 대가로 "복구 시점에야 알게 되는" 실패를 막는다 —
// 백업에서 그 교환은 남는 장사다.
func verifyGzipDump(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("gzip 형식이 아닙니다: %w", err)
	}
	defer zr.Close()
	// 끝까지 읽어야 CRC32 와 크기 검사가 이뤄진다.
	if _, err := io.Copy(io.Discard, zr); err != nil {
		return fmt.Errorf("덤프가 중간에 끊겼습니다: %w", err)
	}
	return nil
}
