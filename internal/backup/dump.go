package backup

import (
	"bufio"
	"compress/gzip"
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"dbstudio/internal/dbx"
	"dbstudio/internal/model"
	"dbstudio/internal/schema"
	"dbstudio/internal/store"
)

// 덤프 파일은 항상 gzip으로 저장한다.
//
// 덤프는 반복이 많은 텍스트라 5~10배로 줄고, 백업에서 제약이 되는 것은 CPU가 아니라
// 디스크다. 다운로드도 .gz 그대로 준다 — 서버에서 풀어 보내면 전송량이 그만큼 늘고,
// 받는 쪽에서는 `gunzip`(또는 `zcat … | psql`) 한 번이면 끝난다.
const fileExt = ".gz"

// StartBackupParams는 덤프 작업 요청이다.
type StartBackupParams struct {
	Target  Target
	Options Options
	Actor   *model.User
	Trigger string
	// jobID는 기록 행의 ID다. StartBackup이 채운다 — 호출부가 정하는 값이 아니다.
	jobID string
}

// StartBackup은 덤프를 시작하고 기록 ID를 즉시 반환한다.
func (s *Service) StartBackup(ctx context.Context, p StartBackupParams) (string, error) {
	if !ValidScope(p.Options.Scope) {
		return "", fmt.Errorf("알 수 없는 덤프 범위입니다: %s", p.Options.Scope)
	}
	if err := s.EnsureDir(); err != nil {
		return "", err
	}
	format := FormatFor(p.Target.Conn.Kind)
	if format == FormatRedis && p.Options.Scope == ScopeSchema {
		// Redis에는 구조라고 부를 것이 없다. "구조만 백업"은 빈 파일을 만들 뿐이다.
		return "", fmt.Errorf("Redis는 구조만 덤프할 수 없습니다. 데이터 또는 전체를 고르세요")
	}

	actorID, actorName := "", ""
	if p.Actor != nil {
		actorID, actorName = p.Actor.ID, p.Actor.Username
	}

	id, err := s.st.CreateBackup(ctx, store.CreateBackupParams{
		ConnectionID: p.Target.Conn.ID, ConnectionName: p.Target.Conn.Name,
		ConnectionKind: string(p.Target.Conn.Kind),
		Scope:          p.Options.Scope, Format: format,
		Options: map[string]any{
			"dropIfExists": p.Options.DropIfExists,
			"tables":       p.Options.Tables,
		},
		Note: p.Options.Note, ActorID: actorID, ActorName: actorName, Trigger: p.Trigger,
	})
	if err != nil {
		return "", err
	}

	p.jobID = id

	// 요청 컨텍스트에서 파생하지 않는다. HTTP 응답이 끝나면 그것은 취소되고,
	// 그러면 덤프가 시작하자마자 죽는다(매크로 실행과 같은 이유).
	jobCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cfg.ScopeLimit)
	s.track(id, cancel)

	go func() {
		defer cancel()
		defer s.untrack(id)
		s.runBackup(jobCtx, id, p, format)
	}()
	return id, nil
}

func (s *Service) runBackup(ctx context.Context, id string, p StartBackupParams, format string) {
	start := time.Now()
	fileName := id + dumpExt(format) + fileExt
	path, err := s.FilePath(fileName)
	if err != nil {
		s.finishBackup(ctx, id, "", err, start, dumpStats{})
		return
	}

	stats, err := s.writeDump(ctx, path, p, format)
	if err != nil {
		// 실패한 덤프 파일은 남기지 않는다. 잘린 덤프가 목록에 있으면 언젠가
		// 그것으로 복구를 시도하게 되고, 그때는 이미 늦다.
		if rmErr := os.Remove(path); rmErr != nil && !os.IsNotExist(rmErr) {
			s.log.Warn("실패한 덤프 파일을 지우지 못했습니다", "file", path, "err", rmErr)
		}
		s.finishBackup(ctx, id, "", err, start, stats)
		return
	}
	if info, serr := os.Stat(path); serr == nil {
		stats.Size = info.Size()
	}
	s.finishBackup(ctx, id, fileName, nil, start, stats)

	// 성공한 뒤에만 오래된 것을 지운다. 실패 직후에 정리를 돌리면 방금 필요해진
	// 백업(마지막으로 성공한 것)을 지울 수 있다.
	if n, perr := s.Purge(context.WithoutCancel(ctx)); perr != nil {
		s.log.Warn("만료된 백업 정리 실패", "err", perr)
	} else if n > 0 {
		s.log.Info("만료된 백업을 정리했습니다", "count", n)
	}
}

func (s *Service) finishBackup(ctx context.Context, id, fileName string, err error, start time.Time, st dumpStats) {
	status, msg := statusFor(err, ctx.Err() == context.Canceled)
	if ferr := s.st.FinishBackup(context.WithoutCancel(ctx), id, store.FinishBackupParams{
		Status: status, Error: msg, FileName: fileName, SizeBytes: st.Size,
		TableCount: st.Tables, RowCount: st.Rows, StatementCount: st.Statements,
		DurationMs: time.Since(start).Milliseconds(),
	}); ferr != nil {
		s.log.Error("백업 기록 갱신 실패", "id", id, "err", ferr)
	}
}

func dumpExt(format string) string {
	switch format {
	case FormatJSONL:
		return ".jsonl"
	case FormatRedis:
		return ".redis"
	default:
		return ".sql"
	}
}

type dumpStats struct {
	Tables     int
	Rows       int64
	Statements int
	Size       int64
}

// writer는 크기 상한을 지키며 gzip으로 쓴다.
type writer struct {
	file  *os.File
	gz    *gzip.Writer
	buf   *bufio.Writer
	max   int64
	wrote int64
}

func newWriter(path string, max int64) (*writer, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("덤프 파일을 만들 수 없습니다: %w", err)
	}
	gz := gzip.NewWriter(f)
	return &writer{file: f, gz: gz, buf: bufio.NewWriterSize(gz, 64*1024), max: max}, nil
}

func (w *writer) WriteString(s string) error {
	// 상한은 **압축 전 크기**로 잰다. 압축률은 데이터에 따라 10배씩 달라지므로
	// 압축 후 크기로 재면 어떤 DB는 상한에 영영 닿지 않고 어떤 DB는 금방 걸린다.
	// 사용자가 예측할 수 있는 쪽은 "덤프 텍스트가 몇 MB인가"다.
	w.wrote += int64(len(s))
	if w.max > 0 && w.wrote > w.max {
		return fmt.Errorf("덤프가 상한(%d MB)을 넘었습니다. 대상을 좁히거나 -backup-max-mb를 올리세요",
			w.max>>20)
	}
	_, err := w.buf.WriteString(s)
	return err
}

func (w *writer) Close() error {
	if err := w.buf.Flush(); err != nil {
		w.gz.Close()
		w.file.Close()
		return err
	}
	if err := w.gz.Close(); err != nil {
		w.file.Close()
		return err
	}
	return w.file.Close()
}

func (s *Service) writeDump(ctx context.Context, path string, p StartBackupParams, format string) (dumpStats, error) {
	w, err := newWriter(path, s.cfg.MaxBytes)
	if err != nil {
		return dumpStats{}, err
	}
	// 실패 경로에서도 반드시 닫는다. 닫지 않으면 파일 핸들이 남아 Windows에서는
	// 삭제조차 되지 않는다.
	defer w.Close()

	var stats dumpStats
	switch format {
	case FormatJSONL:
		stats, err = s.dumpMongo(ctx, w, p)
	case FormatRedis:
		stats, err = s.dumpRedis(ctx, w, p)
	default:
		stats, err = s.dumpSQL(ctx, w, p)
	}
	if err != nil {
		return stats, err
	}
	return stats, w.Close()
}

// header는 모든 덤프의 첫 줄들이다.
//
// 파일만 보고도 무엇인지 알 수 있어야 한다. 백업은 몇 달 뒤에 열리고, 그때는
// 이 앱이 없을 수도 있다.
func header(comment string, conn *model.Connection, opts Options, actor string) string {
	var b strings.Builder
	line := func(format string, args ...any) {
		b.WriteString(comment + " " + fmt.Sprintf(format, args...) + "\n")
	}
	line("DB Studio 논리 덤프")
	line("커넥션: %s (%s, %s)", conn.Name, conn.Kind, conn.Environment)
	line("데이터베이스: %s", conn.DatabaseName)
	line("범위: %s", opts.Scope)
	if len(opts.Tables) > 0 {
		line("대상: %s", strings.Join(opts.Tables, ", "))
	}
	line("DROP 포함: %v", opts.DropIfExists)
	line("만든 시각: %s", time.Now().UTC().Format(time.RFC3339))
	if actor != "" {
		line("만든 사람: %s", actor)
	}
	b.WriteString("\n")
	return b.String()
}

// dumpSQL은 관계형 DB를 SQL 스크립트로 덤프한다.
func (s *Service) dumpSQL(ctx context.Context, w *writer, p StartBackupParams) (dumpStats, error) {
	var stats dumpStats
	conn := p.Target.Conn
	kind := conn.Kind

	actor := ""
	if p.Actor != nil {
		actor = p.Actor.Username
	}
	if err := w.WriteString(header("--", conn, p.Options, actor)); err != nil {
		return stats, err
	}

	adapter, err := dbx.Get(kind)
	if err != nil {
		return stats, err
	}

	// 구조는 스키마 IR에서 생성한다. 빈 스키마와의 차이가 곧 "전부 만들기"이므로
	// 마이그레이션이 쓰는 것과 같은 DDL 생성기를 그대로 쓴다 — 덤프용 생성기를
	// 따로 만들면 두 곳이 갈라지고, 그 차이는 복구할 때에야 드러난다.
	var current *schema.Schema
	if p.Options.Scope != ScopeData {
		introCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		current, err = adapter.Introspect(introCtx, p.Target.dbx())
		cancel()
		if err != nil {
			return stats, fmt.Errorf("스키마를 읽지 못했습니다: %w", err)
		}
		current = filterSchema(current, p.Options.Tables)

		empty := &schema.Schema{Dialect: current.Dialect, Shape: current.Shape, Name: current.Name}
		plan := schema.BuildPlan(string(kind), schema.Diff(empty, current))

		if p.Options.DropIfExists {
			// 역순으로 지운다. 외래키가 있는 경우 참조하는 쪽을 먼저 지워야 한다.
			if err := w.WriteString("-- 기존 객체 제거\n"); err != nil {
				return stats, err
			}
			tables := slices.Clone(current.Tables)
			slices.Reverse(tables)
			for _, t := range tables {
				stmt := fmt.Sprintf("DROP TABLE IF EXISTS %s;\n",
					dbx.QualifyTable(kind, t.Namespace, t.Name))
				if kind == model.KindOracle {
					// Oracle에는 IF EXISTS가 없다. 없는 테이블을 지우려 하면 실패하므로
					// 예외를 삼키는 블록으로 감싼다.
					stmt = fmt.Sprintf(
						"BEGIN EXECUTE IMMEDIATE 'DROP TABLE %s CASCADE CONSTRAINTS'; "+
							"EXCEPTION WHEN OTHERS THEN NULL; END;\n/\n",
						dbx.QualifyTable(kind, t.Namespace, t.Name))
				}
				if err := w.WriteString(stmt); err != nil {
					return stats, err
				}
				stats.Statements++
			}
			if err := w.WriteString("\n"); err != nil {
				return stats, err
			}
		}

		if err := w.WriteString("-- 구조\n"); err != nil {
			return stats, err
		}
		for _, stmt := range plan.Up {
			if err := w.WriteString(stmt.SQL + ";\n"); err != nil {
				return stats, err
			}
			stats.Statements++
		}
		for _, warn := range plan.Warnings {
			if err := w.WriteString("-- 경고: " + warn + "\n"); err != nil {
				return stats, err
			}
		}
		if err := w.WriteString("\n"); err != nil {
			return stats, err
		}
	}

	if p.Options.Scope == ScopeSchema {
		if current != nil {
			stats.Tables = len(current.Tables)
		}
		return stats, nil
	}

	// 데이터. 대상 목록은 구조를 읽었으면 거기서, 아니면 카탈로그에서 가져온다.
	targets, err := s.dataTargets(ctx, p, current)
	if err != nil {
		return stats, err
	}
	if err := w.WriteString("-- 데이터\n"); err != nil {
		return stats, err
	}

	report := s.progressFor(ctx, p.jobID, &stats.Tables)
	for _, ref := range targets {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		stats.Tables++
		rows, stmts, err := s.dumpTable(ctx, w, p, ref, stats.Rows, report)
		if err != nil {
			return stats, err
		}
		stats.Rows += rows
		stats.Statements += stmts
	}
	return stats, nil
}

// dumpTable은 테이블 하나의 데이터를 INSERT 문으로 쓴다.
func (s *Service) dumpTable(ctx context.Context, w *writer, p StartBackupParams,
	ref dbx.TableRef, rowsSoFar int64, report progressFn) (int64, int, error) {
	kind := p.Target.Conn.Kind
	adapter, err := dbx.Get(kind)
	if err != nil {
		return 0, 0, err
	}
	streamer, ok := adapter.(dbx.RowStreamer)
	if !ok {
		return 0, 0, fmt.Errorf("%s는 데이터 덤프를 지원하지 않습니다", kind)
	}

	if err := w.WriteString(fmt.Sprintf("\n-- %s\n", ref)); err != nil {
		return 0, 0, err
	}

	var rows int64
	var stmts int
	target := dbx.QualifyTable(kind, ref.Namespace, ref.Name)

	err = streamer.StreamRows(ctx, p.Target.dbx(), ref, s.cfg.RowBatch,
		func(cols []dbx.DataColumn, batch [][]any) error {
			if len(batch) == 0 {
				return nil
			}
			names := make([]string, len(cols))
			for i, c := range cols {
				names[i] = dbx.QuoteIdent(kind, c.Name)
			}
			prefix := fmt.Sprintf("INSERT INTO %s (%s) VALUES\n", target, strings.Join(names, ", "))

			var b strings.Builder
			b.WriteString(prefix)
			for i, row := range batch {
				values := make([]string, len(row))
				for j, v := range row {
					values[j] = dbx.SQLLiteral(kind, v)
				}
				b.WriteString("  (" + strings.Join(values, ", ") + ")")
				if i < len(batch)-1 {
					b.WriteString(",\n")
				}
			}
			b.WriteString(";\n")

			// 여러 행을 한 INSERT에 묶는다. 행마다 문장을 만들면 파일이 두 배가 되고
			// 복구가 몇 배 느려진다(문장 하나가 왕복 하나다).
			//
			// Oracle은 이 문법(다중 VALUES)을 지원하지 않으므로 행마다 나눈다.
			if kind == model.KindOracle {
				b.Reset()
				for _, row := range batch {
					values := make([]string, len(row))
					for j, v := range row {
						values[j] = dbx.SQLLiteral(kind, v)
					}
					b.WriteString(fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s);\n",
						target, strings.Join(names, ", "), strings.Join(values, ", ")))
					stmts++
				}
			} else {
				stmts++
			}

			if err := w.WriteString(b.String()); err != nil {
				return err
			}
			rows += int64(len(batch))

			// 진행 상황은 배치마다 갱신한다. 매 행마다 쓰면 메타 DB가 덤프보다
			// 바빠지고, 테이블마다 한 번이면 큰 테이블에서 몇 분간 멈춘 것처럼 보인다.
			report(fmt.Sprintf("%s — %s행", ref, formatCount(rowsSoFar+rows)), rowsSoFar+rows)
			return nil
		})
	if err != nil {
		return rows, stmts, err
	}
	return rows, stmts, nil
}

// dataTargets는 데이터를 덤프할 테이블 목록을 정한다.
func (s *Service) dataTargets(ctx context.Context, p StartBackupParams, current *schema.Schema) ([]dbx.TableRef, error) {
	if current != nil {
		out := make([]dbx.TableRef, 0, len(current.Tables))
		for _, t := range current.Tables {
			out = append(out, dbx.TableRef{Namespace: t.Namespace, Name: t.Name})
		}
		return out, nil
	}
	objects, err := dbx.DoListObjects(ctx, p.Target.dbx())
	if err != nil {
		return nil, fmt.Errorf("테이블 목록을 읽지 못했습니다: %w", err)
	}
	out := []dbx.TableRef{}
	for _, o := range objects {
		// 뷰는 데이터를 덤프하지 않는다. 뷰의 내용은 원본 테이블에서 다시 계산되며,
		// 뷰에 INSERT하는 복구 스크립트는 대부분 실패한다.
		if o.Kind != "table" {
			continue
		}
		ref := dbx.TableRef{Namespace: o.Namespace, Name: o.Name}
		if !matchesTables(ref, p.Options.Tables) {
			continue
		}
		out = append(out, ref)
	}
	return out, nil
}

// filterSchema는 대상 목록에 없는 테이블을 스키마에서 뺀다.
func filterSchema(s *schema.Schema, tables []string) *schema.Schema {
	if len(tables) == 0 {
		return s
	}
	out := *s
	out.Tables = nil
	for _, t := range s.Tables {
		if matchesTables(dbx.TableRef{Namespace: t.Namespace, Name: t.Name}, tables) {
			out.Tables = append(out.Tables, t)
		}
	}
	// 뷰는 테이블을 골랐을 때 함께 담지 않는다. 고르지 않은 테이블을 참조하는 뷰는
	// 복구할 수 없고, 그 사실이 복구 시점에야 드러나면 곤란하다.
	out.Views = nil
	return &out
}

func matchesTables(ref dbx.TableRef, tables []string) bool {
	if len(tables) == 0 {
		return true
	}
	full := ref.String()
	return slices.Contains(tables, full) || slices.Contains(tables, ref.Name)
}

func formatCount(n int64) string {
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	return b.String()
}

// progressFn은 "지금 무엇을 하고 있는가"를 기록 행에 반영한다.
//
// 클로저로 넘기는 이유: 갱신에는 작업 ID와 지금까지의 테이블 수가 필요한데,
// 그것을 스트리밍 콜백까지 인자로 끌고 내려가면 시그니처가 지저분해진다.
// 실패는 삼킨다 — 진행 표시를 못 썼다고 덤프를 멈출 이유가 없다.
type progressFn func(message string, rows int64)

func (s *Service) progressFor(ctx context.Context, id string, tables *int) progressFn {
	return func(message string, rows int64) {
		if err := s.st.UpdateBackupProgress(context.WithoutCancel(ctx), id, message, rows, *tables); err != nil {
			s.log.Debug("백업 진행 상황 갱신 실패", "id", id, "err", err)
		}
	}
}
