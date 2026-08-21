package backup

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"dbstudio/internal/dbx"
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

// openDump은 gzip 덤프를 읽기 위한 리더를 연다.
func (s *Service) openDump(b *store.Backup) (io.ReadCloser, func(), error) {
	path, err := s.FilePath(b.FileName)
	if err != nil {
		return nil, nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("백업 파일을 열 수 없습니다: %w", err)
	}
	gz, err := gzip.NewReader(f)
	if err != nil {
		f.Close()
		return nil, nil, fmt.Errorf("백업 파일이 손상되었습니다: %w", err)
	}
	return gz, func() { gz.Close(); f.Close() }, nil
}

// applyDump는 덤프를 대상에 적용한다.
// 반환값은 (실행한 문장 수, 전체 문장 수, 실패한 문장, 오류)다.
func (s *Service) applyDump(ctx context.Context, id string, p StartRestoreParams) (int, int, string, error) {
	switch p.Backup.Format {
	case FormatJSONL:
		return s.restoreMongo(ctx, id, p)
	case FormatRedis:
		return s.restoreRedis(ctx, id, p)
	default:
		return s.restoreSQL(ctx, id, p)
	}
}

// restoreSQL은 SQL 덤프를 실행한다.
//
// 전체를 한 트랜잭션으로 감싸지 않는다. 이유는 마이그레이션 실행기와 같다:
// MySQL·Oracle은 DDL이 암묵적 커밋이라 애초에 트랜잭션이 성립하지 않고, 그렇다고
// 수십만 문장을 메모리에 들고 있을 수도 없다. 대신 **어디까지 갔는지**를 남긴다.
func (s *Service) restoreSQL(ctx context.Context, id string, p StartRestoreParams) (int, int, string, error) {
	reader, closeFn, err := s.openDump(p.Backup)
	if err != nil {
		return 0, 0, "", err
	}
	defer closeFn()

	script, err := io.ReadAll(reader)
	if err != nil {
		return 0, 0, "", fmt.Errorf("백업 파일을 읽지 못했습니다: %w", err)
	}
	stmts := dbx.SplitStatements(p.Target.Conn.Kind, string(script))
	if len(stmts) == 0 {
		return 0, 0, "", fmt.Errorf("실행할 문장이 없습니다")
	}

	total := len(stmts)
	last := time.Now()
	done, failedStmt, execErr := dbx.ExecScript(ctx, p.Target.dbx(), stmts,
		func(i int, current string) bool {
			// 진행 상황은 1초에 한 번만 쓴다. 문장마다 쓰면 메타 DB 쓰기가
			// 복구 자체보다 오래 걸린다.
			if time.Since(last) < time.Second {
				return ctx.Err() == nil
			}
			last = time.Now()
			if uerr := s.st.UpdateRestoreProgress(context.WithoutCancel(ctx), id, i, total,
				fmt.Sprintf("%s / %s 문장", formatCount(int64(i)), formatCount(int64(total)))); uerr != nil {
				s.log.Debug("복구 진행 상황 갱신 실패", "id", id, "err", uerr)
			}
			return ctx.Err() == nil
		})
	return done, total, truncate(failedStmt, 2000), execErr
}

// restoreMongo는 줄 단위 확장 JSON을 되먹인다.
//
// InsertOne이 아니라 _id 기준 upsert(ReplaceOne)를 쓴다. 복구는 실패한 뒤 다시
// 실행되는 일이 잦은데, insert만 하면 두 번째 시도가 중복 키로 전부 실패한다.
func (s *Service) restoreMongo(ctx context.Context, id string, p StartRestoreParams) (int, int, string, error) {
	reader, closeFn, err := s.openDump(p.Backup)
	if err != nil {
		return 0, 0, "", err
	}
	defer closeFn()

	scanner := bufio.NewScanner(reader)
	// 문서 하나가 16MB까지 갈 수 있다(MongoDB의 상한). 기본 버퍼(64KB)로는 끊긴다.
	scanner.Buffer(make([]byte, 0, 1<<20), 17<<20)

	collection := ""
	done := 0
	total := 0
	last := time.Now()

	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return done, total, "", err
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "//") {
			continue
		}
		total++

		if strings.HasPrefix(line, mongoHeaderPrefix) {
			var head struct {
				Collection string `json:"$collection"`
			}
			if jerr := json.Unmarshal([]byte(line), &head); jerr != nil || head.Collection == "" {
				return done, total, line, fmt.Errorf("컬렉션 머리글을 읽지 못했습니다: %s", truncate(line, 200))
			}
			collection = head.Collection
			continue
		}
		if collection == "" {
			return done, total, line, fmt.Errorf("컬렉션이 정해지기 전에 문서가 나왔습니다")
		}

		if _, err := dbx.DoMutateRow(ctx, p.Target.dbx(), dbx.RowMutation{
			Table:  dbx.TableRef{Name: collection},
			Action: "restore",
			Values: map[string]any{"$document": line},
		}); err != nil {
			return done, total, truncate(line, 2000), fmt.Errorf("%s 복구 실패: %w", collection, err)
		}
		done++

		if time.Since(last) >= time.Second {
			last = time.Now()
			if uerr := s.st.UpdateRestoreProgress(context.WithoutCancel(ctx), id, done, 0,
				fmt.Sprintf("%s — %s개 문서", collection, formatCount(int64(done)))); uerr != nil {
				s.log.Debug("복구 진행 상황 갱신 실패", "id", id, "err", uerr)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return done, total, "", fmt.Errorf("백업 파일을 읽지 못했습니다: %w", err)
	}
	return done, total, "", nil
}

// restoreRedis는 줄 단위 명령을 실행한다.
func (s *Service) restoreRedis(ctx context.Context, id string, p StartRestoreParams) (int, int, string, error) {
	reader, closeFn, err := s.openDump(p.Backup)
	if err != nil {
		return 0, 0, "", err
	}
	defer closeFn()

	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 1<<20), 8<<20)

	done, total := 0, 0
	last := time.Now()
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return done, total, "", err
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		total++

		results, err := dbx.DoRunStatements(ctx, p.Target.dbx(), dbx.StatementRequest{
			Statement: line, MaxRows: 1,
		})
		if err != nil {
			return done, total, truncate(line, 2000), err
		}
		if len(results) > 0 && results[0].Error != "" {
			return done, total, truncate(line, 2000), fmt.Errorf("%s", results[0].Error)
		}
		done++

		if time.Since(last) >= time.Second {
			last = time.Now()
			if uerr := s.st.UpdateRestoreProgress(context.WithoutCancel(ctx), id, done, 0,
				fmt.Sprintf("%s개 명령", formatCount(int64(done)))); uerr != nil {
				s.log.Debug("복구 진행 상황 갱신 실패", "id", id, "err", uerr)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return done, total, "", fmt.Errorf("백업 파일을 읽지 못했습니다: %w", err)
	}
	return done, total, "", nil
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
