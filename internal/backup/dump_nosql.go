package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"dbstudio/internal/dbx"
)

// MongoDB · Redis 덤프.
//
// SQL 덤프와 형식이 다른 이유는 단순하다: 두 DB에는 실행할 SQL이 없다. 대신 복구기가
// 그대로 되먹일 수 있는 형태로 쓴다 — Mongo는 줄 단위 확장 JSON, Redis는 줄 단위 명령.
//
// 두 형식 모두 **한 줄이 한 항목**이다. 그래야 큰 덤프를 통째로 메모리에 올리지 않고
// 복구할 수 있고, 사람이 파일을 열어 grep으로 뒤질 수 있다.

// mongoHeaderPrefix는 컬렉션 경계를 나타내는 줄이다.
// 주석이 아니라 JSON인 이유: 복구기가 줄마다 JSON 파서 하나만 쓰면 되고,
// 컬렉션 이름에 어떤 문자가 들어 있어도 파싱이 흔들리지 않는다.
const mongoHeaderPrefix = `{"$collection":`

func (s *Service) dumpMongo(ctx context.Context, w *writer, p StartBackupParams) (dumpStats, error) {
	var stats dumpStats
	conn := p.Target.Conn

	actor := ""
	if p.Actor != nil {
		actor = p.Actor.Username
	}
	if err := w.WriteString(header("//", conn, p.Options, actor)); err != nil {
		return stats, err
	}

	objects, err := dbx.DoListObjects(ctx, p.Target.dbx())
	if err != nil {
		return stats, fmt.Errorf("컬렉션 목록을 읽지 못했습니다: %w", err)
	}

	report := s.progressFor(ctx, p.jobID, &stats.Tables)
	for _, o := range objects {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		// 뷰는 원본 컬렉션에서 다시 계산되는 것이므로 덤프하지 않는다.
		if o.Kind != "collection" {
			continue
		}
		ref := dbx.TableRef{Namespace: o.Namespace, Name: o.Name}
		if !matchesTables(ref, p.Options.Tables) {
			continue
		}
		stats.Tables++

		head, err := json.Marshal(o.Name)
		if err != nil {
			return stats, err
		}
		if err := w.WriteString(mongoHeaderPrefix + string(head) + "}\n"); err != nil {
			return stats, err
		}

		// 구조만 덤프하는 경우 컬렉션 목록까지만 쓴다. Mongo에서 "구조"는
		// 컬렉션의 존재와 인덱스인데, 인덱스는 서버 명령이라 이 형식에 담기 어렵다.
		// 담을 수 없는 것을 담은 척하지 않는다.
		if p.Options.Scope == ScopeSchema {
			continue
		}

		var count int64
		offset := 0
		for {
			if err := ctx.Err(); err != nil {
				return stats, err
			}
			page, err := dbx.DoQueryRows(ctx, p.Target.dbx(), dbx.RowQuery{
				Table: ref, Limit: s.cfg.RowBatch, Offset: offset, Full: true,
			})
			if err != nil {
				return stats, fmt.Errorf("%s 조회 실패: %w", ref.Name, err)
			}
			// 원본 문서는 마지막 열($document)에 확장 JSON으로 실려 온다.
			// 표로 펼친 열들은 표시용이므로 덤프에는 쓰지 않는다 — 중첩 문서가
			// 문자열로 눌려 있어 그대로 복구하면 구조가 달라진다.
			raw := documentColumn(page)
			if raw < 0 {
				return stats, fmt.Errorf("%s: 원본 문서를 찾을 수 없습니다", ref.Name)
			}
			for _, row := range page.Rows {
				doc, _ := row[raw].(string)
				if strings.TrimSpace(doc) == "" {
					continue
				}
				if err := w.WriteString(doc + "\n"); err != nil {
					return stats, err
				}
				count++
				stats.Statements++
			}
			stats.Rows += int64(len(page.Rows))
			report(fmt.Sprintf("%s — %s개 문서", ref.Name, formatCount(stats.Rows)), stats.Rows)

			if !page.HasMore {
				break
			}
			offset += len(page.Rows)
			if len(page.Rows) == 0 {
				break
			}
		}
	}
	return stats, nil
}

func documentColumn(page *dbx.RowPage) int {
	for i, c := range page.Columns {
		if c.Name == "$document" {
			return i
		}
	}
	return -1
}

// dumpRedis는 키를 다시 만들 수 있는 명령 목록으로 쓴다.
//
// DUMP/RESTORE(바이너리 직렬화)를 쓰지 않는 이유: 그 형식은 Redis 버전에 묶여 있어
// 다른 버전으로 복구하면 거부된다. 백업은 "다른 곳에 다시 세우기 위한 것"이므로
// 이식성이 압축률보다 중요하다. 명령 형태는 사람이 읽을 수도 있다.
func (s *Service) dumpRedis(ctx context.Context, w *writer, p StartBackupParams) (dumpStats, error) {
	var stats dumpStats
	conn := p.Target.Conn

	actor := ""
	if p.Actor != nil {
		actor = p.Actor.Username
	}
	if err := w.WriteString(header("#", conn, p.Options, actor)); err != nil {
		return stats, err
	}

	report := s.progressFor(ctx, p.jobID, &stats.Tables)
	offset := 0
	pattern := "*"
	if len(p.Options.Tables) == 1 {
		// 대상이 하나면 그것을 SCAN 패턴으로 쓴다(데이터 화면의 키 그룹 이름과 같은 형태).
		pattern = p.Options.Tables[0]
	}

	for {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		page, err := dbx.DoQueryRows(ctx, p.Target.dbx(), dbx.RowQuery{
			Table: dbx.TableRef{Name: pattern}, Limit: s.cfg.RowBatch, Offset: offset, Full: true,
		})
		if err != nil {
			return stats, fmt.Errorf("키 조회 실패: %w", err)
		}
		idx := redisColumns(page)
		for _, row := range page.Rows {
			key, _ := row[idx.key].(string)
			kind, _ := row[idx.kind].(string)
			value, _ := row[idx.value].(string)
			if key == "" {
				continue
			}
			cmds, err := redisCommands(key, kind, value, row[idx.ttl])
			if err != nil {
				// 되살릴 수 없는 타입은 건너뛰되 파일에 사실을 남긴다.
				// 조용히 빠지면 복구 후 "왜 이 키가 없지"를 설명할 수 없다.
				if werr := w.WriteString(fmt.Sprintf("# 건너뜀: %s (%v)\n", key, err)); werr != nil {
					return stats, werr
				}
				continue
			}
			for _, cmd := range cmds {
				if err := w.WriteString(cmd + "\n"); err != nil {
					return stats, err
				}
				stats.Statements++
			}
			stats.Rows++
		}
		report(fmt.Sprintf("키 %s개", formatCount(stats.Rows)), stats.Rows)

		if !page.HasMore || len(page.Rows) == 0 {
			break
		}
		offset += len(page.Rows)
	}
	stats.Tables = 1
	return stats, nil
}

type redisIdx struct{ key, kind, ttl, value int }

func redisColumns(page *dbx.RowPage) redisIdx {
	idx := redisIdx{key: 0, kind: 1, ttl: 2, value: 4}
	for i, c := range page.Columns {
		switch c.Name {
		case "key":
			idx.key = i
		case "type":
			idx.kind = i
		case "ttl":
			idx.ttl = i
		case "value":
			idx.value = i
		}
	}
	return idx
}

// redisCommands는 키 하나를 되살리는 명령 목록을 만든다.
//
// 데이터 화면이 만든 표시용 값(쉼표로 이은 문자열)을 되돌려 쓰는 것이 한계다.
// 값 안에 쉼표가 있으면 원본과 다르게 복구된다. 그래서 문자열 키만 정확히 보장하고,
// 컬렉션 타입은 그 한계를 파일에 적어 둔다 — 틀릴 수 있다는 사실을 숨기지 않는다.
func redisCommands(key, kind, value string, ttl any) ([]string, error) {
	quote := func(s string) string {
		return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\r", `\r`).Replace(s) + `"`
	}
	var cmds []string

	switch kind {
	case "string":
		cmds = append(cmds, fmt.Sprintf("SET %s %s", quote(key), quote(value)))
	case "hash":
		parts := splitPreview(value)
		args := make([]string, 0, len(parts)*2)
		for _, p := range parts {
			f, v, ok := strings.Cut(p, "=")
			if !ok {
				continue
			}
			args = append(args, quote(f), quote(v))
		}
		if len(args) == 0 {
			return nil, fmt.Errorf("빈 해시")
		}
		cmds = append(cmds, "HSET "+quote(key)+" "+strings.Join(args, " "))
	case "list":
		parts := splitPreview(value)
		if len(parts) == 0 {
			return nil, fmt.Errorf("빈 리스트")
		}
		args := make([]string, len(parts))
		for i, p := range parts {
			args[i] = quote(p)
		}
		cmds = append(cmds, "RPUSH "+quote(key)+" "+strings.Join(args, " "))
	case "set":
		parts := splitPreview(value)
		if len(parts) == 0 {
			return nil, fmt.Errorf("빈 셋")
		}
		args := make([]string, len(parts))
		for i, p := range parts {
			args[i] = quote(p)
		}
		cmds = append(cmds, "SADD "+quote(key)+" "+strings.Join(args, " "))
	case "zset":
		parts := splitPreview(value)
		args := make([]string, 0, len(parts)*2)
		for _, p := range parts {
			member, score, ok := cutScore(p)
			if !ok {
				continue
			}
			args = append(args, score, quote(member))
		}
		if len(args) == 0 {
			return nil, fmt.Errorf("빈 정렬 셋")
		}
		cmds = append(cmds, "ZADD "+quote(key)+" "+strings.Join(args, " "))
	default:
		return nil, fmt.Errorf("%s 타입은 덤프할 수 없습니다", kind)
	}

	// TTL은 값을 넣은 뒤에 건다. 먼저 걸면 SET이 만료를 지운다.
	if ttl != nil {
		if secs := fmt.Sprint(ttl); secs != "" && secs != "0" && secs != "<nil>" {
			cmds = append(cmds, fmt.Sprintf("EXPIRE %s %s", quote(key), secs))
		}
	}
	return cmds, nil
}

func splitPreview(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ", ")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// cutScore는 "member(1.5)" 형태에서 점수를 떼어낸다.
func cutScore(s string) (string, string, bool) {
	open := strings.LastIndex(s, "(")
	if open < 0 || !strings.HasSuffix(s, ")") {
		return "", "", false
	}
	return s[:open], s[open+1 : len(s)-1], true
}
