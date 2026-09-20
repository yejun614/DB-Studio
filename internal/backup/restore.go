package backup

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"dbstudio/internal/model"
	"dbstudio/internal/store"
)

// 복구.
//
// **이 앱이 만든 백업만 복구한다.** 외부에서 받은 .sql 파일을 올려 실행하는 경로는
// 두지 않았다. 그것은 결국 "임의의 SQL을 파일로 실행하는 기능"이고, 그런 통로는 이미
// SQL 콘솔에 있으며 거기에는 `sql.run` 권한이 붙어 있다. 파일 업로드로 같은 일을
// 하게 만들면 권한 두 벌이 같은 능력을 가리키게 되고, 어느 쪽이 진짜인지 흐려진다.
// 백업을 내려받아 보관하는 것은 가능하므로, 외부 파일을 되돌릴 방법이 없는 것도 아니다.
//
// 복구는 되돌릴 수 없다. 그래서 이 파일의 모든 결정은 **무엇이 일어났는지 정확히
// 남기는 쪽**으로 기운다 — 어디까지 실행했는지, 어느 문장에서 멈췄는지.

// StartRestoreParams는 복구 요청이다.
type StartRestoreParams struct {
	Backup *store.Backup
	Target Target
	Actor  *model.User
}

// StartRestore는 복구를 시작하고 기록 ID를 즉시 반환한다.
func (s *Service) StartRestore(ctx context.Context, p StartRestoreParams) (string, error) {
	if p.Backup.Status != "success" {
		return "", fmt.Errorf("성공한 백업만 복구할 수 있습니다 (현재 %s)", p.Backup.Status)
	}
	if !s.FileExists(p.Backup) {
		return "", fmt.Errorf("백업 파일이 없습니다. 보존 기간이 지나 삭제되었을 수 있습니다")
	}
	// 형식이 맞지 않으면 실행할 방법이 없다. Mongo 덤프를 PostgreSQL에 부으면
	// 첫 줄에서 실패하겠지만, 그 전에 막는 편이 낫다.
	if want := FormatFor(p.Target.Conn.Kind); want != p.Backup.Format {
		return "", fmt.Errorf("이 백업(%s)은 %s 커넥션에 복구할 수 없습니다",
			p.Backup.Format, p.Target.Conn.Kind)
	}

	actorID, actorName := "", ""
	if p.Actor != nil {
		actorID, actorName = p.Actor.ID, p.Actor.Username
	}
	label := fmt.Sprintf("%s · %s", p.Backup.ConnectionName,
		p.Backup.StartedAt.Local().Format("2006-01-02 15:04"))

	id, err := s.st.CreateRestore(ctx, store.CreateRestoreParams{
		BackupID: p.Backup.ID, BackupLabel: label,
		ConnectionID: p.Target.Conn.ID, ConnectionName: p.Target.Conn.Name,
		ActorID: actorID, ActorName: actorName,
	})
	if err != nil {
		return "", err
	}

	jobCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cfg.ScopeLimit)
	s.track(id, cancel)

	go func() {
		defer cancel()
		defer s.untrack(id)
		s.runRestore(jobCtx, id, p)
	}()
	return id, nil
}

// applyDump는 덤프를 대상에 적용한다.
// 반환값은 (실행한 문장 수, 전체 문장 수, 실패한 문장, 오류)다.
//
// 마스터가 로컬에서 복구하는 경로다(담당 노드가 없거나 이 노드가 담당). 담당 노드가 따로
// 있으면 그 노드가 같은 적용 로직을 돌린다 — 그래서 적용 코드는 restore_stream.go 한 곳에
// 있고, 여기서는 진행률을 **복구 행에 적는** 함수만 넘긴다.
func (s *Service) applyDump(ctx context.Context, id string, p StartRestoreParams) (int, int, string, error) {
	return s.RestoreFromBackup(ctx, p.Backup,
		RestoreParams{Kind: string(p.Target.Conn.Kind), Target: p.Target},
		s.progressWriter(id))
}

// progressWriter는 진행 상황을 복구 행에 적는 함수를 만든다.
//
// 로컬 복구에서만 쓴다. 담당 노드에는 복구 행이 없으므로(기록은 마스터의 일이다) 그쪽은
// nil을 넘긴다 — 그 차이가 이 함수의 존재 이유다.
func (s *Service) progressWriter(id string) ProgressFunc {
	return func(done, total int, message string) {
		if uerr := s.st.UpdateRestoreProgress(
			context.WithoutCancel(context.Background()), id, done, total, message); uerr != nil {
			s.log.Debug("복구 진행 상황 갱신 실패", "id", id, "err", uerr)
		}
	}
}

func (s *Service) runRestore(ctx context.Context, id string, p StartRestoreParams) {
	start := time.Now()

	done, total, failed, err := s.applyDump(ctx, id, p)

	status, msg := statusFor(err, ctx.Err() == context.Canceled)
	if ferr := s.st.FinishRestore(context.WithoutCancel(ctx), id, store.FinishRestoreParams{
		Status: status, Error: msg, FailedStatement: failed,
		StatementsDone: done, StatementsTotal: total,
		DurationMs: time.Since(start).Milliseconds(),
	}); ferr != nil {
		s.log.Error("복구 기록 갱신 실패", "id", id, "err", ferr)
	}
}

// openDump은 이 노드의 디스크에 있는 백업 파일을 열어 푼다.
//
// 경로 조립과 gzip 열기는 OpenDumpFile 한 곳에 있다 — 마스터가 담당 노드로 밀어 넣을 때도
// 같은 함수를 쓰므로, 경로 검사(FilePath)가 두 경로 모두에 적용된다.
func (s *Service) openDump(b *store.Backup) (io.ReadCloser, func(), error) {
	return s.OpenDumpFile(b.FileName)
}

// Preview는 백업 파일의 앞부분을 돌려준다.
//
// 복구 전에 "이 파일이 무엇인가"를 눈으로 확인할 수 있어야 한다. 머리글에 커넥션·
// 시각·범위가 적혀 있으므로 앞의 몇십 줄이면 대개 충분하다.
func (s *Service) Preview(b *store.Backup, maxBytes int) (string, error) {
	reader, closeFn, err := s.openDump(b)
	if err != nil {
		return "", err
	}
	defer closeFn()

	if maxBytes <= 0 || maxBytes > 64*1024 {
		maxBytes = 16 * 1024
	}
	buf := make([]byte, maxBytes)
	n, err := io.ReadFull(reader, buf)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return "", fmt.Errorf("백업 파일을 읽지 못했습니다: %w", err)
	}
	out := string(buf[:n])
	// 마지막 줄이 잘려 있으면 버린다. 반쯤 잘린 SQL 문장을 보여주면
	// 그것이 파일의 문제인지 미리보기의 문제인지 알 수 없다.
	if n == maxBytes {
		if idx := strings.LastIndexByte(out, '\n'); idx > 0 {
			out = out[:idx] + "\n… (미리보기는 앞부분만 보여줍니다)"
		}
	}
	return out, nil
}

// OpenForDownload는 다운로드용 파일 핸들을 연다. 압축된 그대로 내보낸다.
func (s *Service) OpenForDownload(b *store.Backup) (*os.File, int64, error) {
	path, err := s.FilePath(b.FileName)
	if err != nil {
		return nil, 0, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, fmt.Errorf("백업 파일을 열 수 없습니다: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, err
	}
	return f, info.Size(), nil
}

func truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "…"
}
