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

// 담당 노드에서 복구를 실행하기 위한 스트림 기반 복구 (P33-2).
//
// ── 왜 필요한가 ────────────────────────────────────────────────────
// 복구는 백업의 역방향이고, 백업과 같은 문제를 갖는다:
//
//	· 요청이 **쓰기**라서 리플리카에서 오면 마스터로 넘어온다
//	· 담당 노드가 지정된 DB는 **마스터가 닿지 못할 수 있다** — 담당 노드를 지정한 이유다
//
// 그래서 마스터가 복구를 시도하면 1045 로 죽는다(백업·버전 캡처와 같은 원인).
//
// ── 방향은 백업의 반대다 ──────────────────────────────────────────
// 백업은 노드가 만들어 마스터로 **올렸다**. 복구는 반대로 마스터가 가진 파일을 노드로
// **밀어 넣는다** — 파일이 마스터에 있으므로(백업이 그렇게 보관한다) 그 방향이 자연스럽다.
//
//	마스터 : 파일을 열어 담당 노드의 /node/restore 로 본문에 실어 보낸다
//	노드   : 자기 DB에 그 덤프를 적용하고 (문장 수, 실패 문장, 오류)를 JSON 으로 돌려준다
//	마스터 : 그 답으로 복구 기록을 남긴다 (기록은 언제나 마스터의 일이다)
//
// ── 진행 상황은 노드가 남기지 않는다 ──────────────────────────────
// 백업과 같은 이유다: 노드가 자기 메타 DB 에 복구 행을 만들면 다음 복제에서 사라진다.
// 그래서 노드는 진행 상황을 적지 않고 **끝난 결과만** 돌려준다.

// RestoreParams 는 복구 대상과 자격증명이다.
//
// StartRestoreParams 와 나뉘어 있는 이유: 저쪽은 **기록을 만드는** 요청이고(마스터의 일),
// 이쪽은 **DB 에 적용하는** 조건이다(노드의 일). 한 구조체로 묶으면 노드가 기록을 만드는
// 것처럼 보인다.
type RestoreParams struct {
	// Kind 는 대상 DB 종류다. 덤프를 실행하는 방법(SQL/JSONL/Redis)을 정한다.
	Kind string
	// Target 은 접속 대상이다. 노드는 자기 복제본에서 채운다.
	Target Target
}

// ProgressFunc 는 복구 진행 상황을 받는 자리다.
//
// 노드에서는 nil 로 부른다(남길 곳이 없다 — 복구 행이 거기 없다). 마스터가 로컬로
// 복구할 때는 그 행에 진행률을 적는다. 그 차이가 이 인자의 이유다.
type ProgressFunc func(done, total int, message string)

// RestoreResult 는 복구 적용의 결과다. 노드가 마스터로 돌려준다.
type RestoreResult struct {
	// Done 은 실행한 문장(또는 문서·명령) 수다.
	Done int `json:"done"`
	// Total 은 전체 수다. 알 수 없으면 0.
	Total int `json:"total"`
	// FailedStatement 는 실패한 문장이다(잘라서 담는다).
	FailedStatement string `json:"failedStatement,omitempty"`
	// Error 는 실패 이유다. 실패하지 않았으면 빈 값이다.
	Error string `json:"error,omitempty"`
}

// ApplyStream 은 리더에서 읽은 덤프를 대상 DB 에 적용한다.
//
// 파일을 열지 않고 리더를 받는 이유: 클러스터에서 그 리더는 **노드로 가는 요청 본문**이고
// (마스터 쪽), 노드에서는 **마스터가 보낸 본문**이다. 어느 쪽이든 파일이 아니다.
// 기존 파일 경로(restoreSQL 등)도 이 함수를 지나므로 적용 로직은 한 곳뿐이다 —
// 두 곳이 갈라지면 그 차이는 복구할 때에 드러난다.
func (s *Service) ApplyStream(ctx context.Context, r io.Reader, p RestoreParams, progress ProgressFunc) (int, int, string, error) {
	if p.Target.Conn == nil {
		return 0, 0, "", fmt.Errorf("복구 대상이 없습니다")
	}
	// 형식은 **대상 DB 종류**로 정한다. 덤프 기록의 Format 이 아니라 대상이 기준인 이유:
	// 노드는 그 DB 에 문장을 실행하므로 실행 방법을 아는 것은 대상 쪽이다.
	// (기록의 Format 과 어긋나면 마스터가 StartRestore 에서 이미 막는다.)
	switch FormatFor(model.DBKind(p.Kind)) {
	case FormatJSONL:
		return s.restoreMongoStream(ctx, r, p, progress)
	case FormatRedis:
		return s.restoreRedisStream(ctx, r, p, progress)
	default:
		return s.restoreSQLStream(ctx, r, p, progress)
	}
}

// RestoreFromBackup 은 이 노드의 디스크에 있는 백업 파일을 대상에 적용한다.
//
// 마스터가 자기 파일로 로컬 복구할 때의 경로다(담당 노드가 없거나 이 노드가 담당일 때).
func (s *Service) RestoreFromBackup(ctx context.Context, b *store.Backup, p RestoreParams, progress ProgressFunc) (int, int, string, error) {
	reader, closeFn, err := s.openDump(b)
	if err != nil {
		return 0, 0, "", err
	}
	defer closeFn()
	return s.ApplyStream(ctx, reader, p, progress)
}

// OpenDumpFile 은 이름으로 백업 파일을 열어 푼다.
//
// 이름을 받는 이유: 노드로 밀어 넣을 때 마스터는 이름만 알고 있다. 경로 조립은
// FilePath 를 지나므로 경로 검사도 그대로 적용된다.
func (s *Service) OpenDumpFile(fileName string) (io.ReadCloser, func(), error) {
	path, err := s.FilePath(fileName)
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

// restoreSQLStream 은 SQL 덤프를 실행한다.
//
// 전체를 한 트랜잭션으로 감싸지 않는다. 이유는 마이그레이션 실행기와 같다:
// MySQL·Oracle은 DDL이 암묵적 커밋이라 애초에 트랜잭션이 성립하지 않고, 그렇다고
// 수십만 문장을 메모리에 들고 있을 수도 없다. 대신 **어디까지 갔는지**를 남긴다.
//
// 문장을 나누려면 전체를 읽어야 한다(SplitStatements 가 문자열을 받는다). 그래서
// SQL 경로만 메모리에 올린다 — 다른 경로는 줄 단위로 흘려보낸다.
func (s *Service) restoreSQLStream(ctx context.Context, r io.Reader, p RestoreParams, progress ProgressFunc) (int, int, string, error) {
	script, err := io.ReadAll(r)
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
			// 진행 상황은 1초에 한 번만 알린다. 문장마다 알리면 메타 DB 쓰기가
			// 복구 자체보다 오래 걸린다.
			if progress == nil || time.Since(last) < time.Second {
				return ctx.Err() == nil
			}
			last = time.Now()
			progress(i, total, fmt.Sprintf("%s / %s 문장",
				formatCount(int64(i)), formatCount(int64(total))))
			return ctx.Err() == nil
		})
	return done, total, truncate(failedStmt, 2000), execErr
}

// restoreMongoStream 은 줄 단위 확장 JSON을 되먹인다.
//
// InsertOne이 아니라 _id 기준 upsert(ReplaceOne)를 쓴다. 복구는 실패한 뒤 다시
// 실행되는 일이 잦은데, insert만 하면 두 번째 시도가 중복 키로 전부 실패한다.
func (s *Service) restoreMongoStream(ctx context.Context, r io.Reader, p RestoreParams, progress ProgressFunc) (int, int, string, error) {
	scanner := bufio.NewScanner(r)
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

		if progress != nil && time.Since(last) >= time.Second {
			last = time.Now()
			progress(done, 0, fmt.Sprintf("%s — %s개 문서", collection, formatCount(int64(done))))
		}
	}
	if err := scanner.Err(); err != nil {
		return done, total, "", fmt.Errorf("백업 파일을 읽지 못했습니다: %w", err)
	}
	return done, total, "", nil
}

// restoreRedisStream 은 줄 단위 명령을 실행한다.
func (s *Service) restoreRedisStream(ctx context.Context, r io.Reader, p RestoreParams, progress ProgressFunc) (int, int, string, error) {
	scanner := bufio.NewScanner(r)
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

		if progress != nil && time.Since(last) >= time.Second {
			last = time.Now()
			progress(done, 0, fmt.Sprintf("%s개 명령", formatCount(int64(done))))
		}
	}
	if err := scanner.Err(); err != nil {
		return done, total, "", fmt.Errorf("백업 파일을 읽지 못했습니다: %w", err)
	}
	return done, total, "", nil
}
