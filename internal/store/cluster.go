package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// 클러스터 복제.
//
// 구조: 마스터의 메타 DB에 트리거를 달아 모든 행 변경을 repl_log에 적는다. 리플리카는
// 그 로그를 순서대로 받아 자기 DB에 그대로 적용한다. 로그가 잘려 따라잡을 수 없으면
// 스냅샷(파일 하나)을 받아 통째로 맞춘 뒤 그 지점부터 이어 간다.
//
// 왜 SQL 문장을 나르지 않는가: 문장을 재실행하면 now()나 random()이 노드마다 다른 값을
// 만들고, 그 순간 두 DB는 조용히 갈라진다. 결과(행)를 나르면 그런 여지가 없다.

// replSkip은 복제하지 않는 표다.
//
//   - repl_log: 로그 자체를 복제하면 무한히 커진다.
//   - repl_state: 노드마다 값이 달라야 한다("내가 어디까지 적용했는가").
//   - schema_migrations: 각 노드가 자기 바이너리로 자기 스키마를 만든다.
var replSkip = map[string]bool{
	"repl_log":          true,
	"repl_state":        true,
	"schema_migrations": true,
}

// ReplTables는 복제 대상 표 이름을 돌려준다.
func (s *Store) ReplTables(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT name, sql FROM sqlite_master
		WHERE type = 'table' AND name NOT LIKE 'sqlite_%'
		ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list tables: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var name string
		var ddl sql.NullString
		if err := rows.Scan(&name, &ddl); err != nil {
			return nil, err
		}
		if replSkip[name] {
			continue
		}
		// WITHOUT ROWID 표에는 rowid가 없어 이 방식으로 식별할 수 없다.
		// 지금은 하나도 없지만, 생기면 조용히 빠지는 대신 알 수 있어야 한다.
		if strings.Contains(strings.ToUpper(ddl.String), "WITHOUT ROWID") {
			continue
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// columnsOf는 표의 컬럼 이름과 BLOB 여부를 돌려준다.
func (s *Store) columnsOf(ctx context.Context, table string) ([]string, map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(`PRAGMA table_info(%q)`, table))
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	var cols []string
	blobs := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull int
		var dflt sql.NullString
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			return nil, nil, err
		}
		cols = append(cols, name)
		if strings.Contains(strings.ToUpper(typ), "BLOB") {
			blobs[name] = true
		}
	}
	return cols, blobs, rows.Err()
}

// InstallReplTriggers는 마스터에 변경 기록 트리거를 단다.
//
// 매번 지우고 다시 만드는 이유: 스키마 마이그레이션으로 컬럼이 늘어나면 옛 트리거는
// 새 컬럼을 기록하지 않는다. 그 사실은 아무 오류도 내지 않고, 리플리카에서 그 컬럼만
// 비어 있는 형태로 나타난다 — 찾기 가장 어려운 종류의 어긋남이다.
func (s *Store) InstallReplTriggers(ctx context.Context) (int, error) {
	tables, err := s.ReplTables(ctx)
	if err != nil {
		return 0, err
	}
	if err := s.DropReplTriggers(ctx); err != nil {
		return 0, err
	}

	for _, t := range tables {
		cols, blobs, err := s.columnsOf(ctx, t)
		if err != nil {
			return 0, fmt.Errorf("%s: %w", t, err)
		}
		if len(cols) == 0 {
			continue
		}
		for _, ev := range []string{"INSERT", "UPDATE"} {
			ddl := fmt.Sprintf(`CREATE TRIGGER %q AFTER %s ON %q BEGIN
	INSERT INTO repl_log (tbl, rid, op, row, at)
	VALUES (%s, NEW.rowid, 'upsert', %s, strftime('%%Y-%%m-%%dT%%H:%%M:%%fZ','now'));
END`,
				triggerName(t, ev), ev, t, quote(t), jsonObject("NEW", cols, blobs))
			if _, err := s.db.ExecContext(ctx, ddl); err != nil {
				return 0, fmt.Errorf("trigger %s %s: %w", t, ev, err)
			}
		}
		ddl := fmt.Sprintf(`CREATE TRIGGER %q AFTER DELETE ON %q BEGIN
	INSERT INTO repl_log (tbl, rid, op, row, at)
	VALUES (%s, OLD.rowid, 'delete', NULL, strftime('%%Y-%%m-%%dT%%H:%%M:%%fZ','now'));
END`, triggerName(t, "DELETE"), t, quote(t))
		if _, err := s.db.ExecContext(ctx, ddl); err != nil {
			return 0, fmt.Errorf("trigger %s DELETE: %w", t, err)
		}
	}
	return len(tables), nil
}

// DropReplTriggers는 복제 트리거를 모두 걷어낸다.
//
// 리플리카에서 반드시 불러야 한다. 남아 있으면 복제로 들어온 행이 다시 로그에 쌓이고,
// 그 노드가 나중에 마스터로 승격될 때 옛 변경이 되살아난다.
func (s *Store) DropReplTriggers(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `
		SELECT name FROM sqlite_master WHERE type = 'trigger' AND name LIKE 'repl__%' ESCAPE '_'`)
	if err != nil {
		return err
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return err
		}
		names = append(names, n)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, n := range names {
		if _, err := s.db.ExecContext(ctx, fmt.Sprintf(`DROP TRIGGER IF EXISTS %q`, n)); err != nil {
			return err
		}
	}
	return nil
}

func triggerName(table, event string) string {
	return "repl_" + table + "_" + strings.ToLower(event)
}

// jsonObject는 행 전체를 JSON으로 만드는 식이다.
//
// BLOB을 hex로 감싸는 이유: SQLite의 json_object는 BLOB을 담지 못하고 오류를 낸다.
// 그 오류는 트리거 안에서 나므로 **원래 쓰기까지 함께 실패한다** — 아바타 한 장 때문에
// 프로필 저장이 안 되는 식이다. 16진 문자열로 바꿔 싣고 받는 쪽에서 되돌린다.
func jsonObject(prefix string, cols []string, blobs map[string]bool) string {
	var b strings.Builder
	b.WriteString("json_object(")
	for i, c := range cols {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(quote(c))
		b.WriteString(", ")
		if blobs[c] {
			fmt.Fprintf(&b, "hex(%s.%q)", prefix, c)
		} else {
			fmt.Fprintf(&b, "%s.%q", prefix, c)
		}
	}
	b.WriteString(")")
	return b.String()
}

func quote(v string) string { return "'" + strings.ReplaceAll(v, "'", "''") + "'" }

// ReplChange는 복제 로그 한 줄이다.
type ReplChange struct {
	Seq   int64           `json:"seq"`
	Table string          `json:"tbl"`
	RowID int64           `json:"rid"`
	Op    string          `json:"op"`
	Row   json.RawMessage `json:"row,omitempty"`
	At    string          `json:"at"`
}

// ReplChanges는 since 이후의 변경을 순서대로 돌려준다.
func (s *Store) ReplChanges(ctx context.Context, since int64, limit int) ([]ReplChange, error) {
	if limit <= 0 || limit > 5000 {
		limit = 1000
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT seq, tbl, rid, op, row, at FROM repl_log
		WHERE seq > ? ORDER BY seq LIMIT ?`, since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []ReplChange{}
	for rows.Next() {
		var c ReplChange
		var row sql.NullString
		if err := rows.Scan(&c.Seq, &c.Table, &c.RowID, &c.Op, &row, &c.At); err != nil {
			return nil, err
		}
		if row.Valid {
			c.Row = json.RawMessage(row.String)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ReplBounds는 복제 로그의 처음과 끝 seq다.
//
// 처음이 필요한 이유: 리플리카가 요청한 지점이 이미 잘려 나갔다면 변경을 이어 붙일 수
// 없다. 그때는 "따라잡을 수 없다"고 알려 스냅샷을 받게 해야 한다 — 조용히 건너뛰면
// 그 사이의 변경이 영원히 빠진 채로 두 DB가 갈라진다.
func (s *Store) ReplBounds(ctx context.Context) (minSeq, maxSeq int64, err error) {
	var lo, hi sql.NullInt64
	err = s.db.QueryRowContext(ctx, `SELECT min(seq), max(seq) FROM repl_log`).Scan(&lo, &hi)
	if err != nil {
		return 0, 0, err
	}
	return lo.Int64, hi.Int64, nil
}

// PruneReplLog는 오래된 복제 로그를 지운다. 남길 기간과 최대 줄 수를 함께 본다.
func (s *Store) PruneReplLog(ctx context.Context, keep time.Duration, maxRows int) (int64, error) {
	var total int64
	if keep > 0 {
		cutoff := time.Now().UTC().Add(-keep).Format(time.RFC3339Nano)
		res, err := s.db.ExecContext(ctx, `DELETE FROM repl_log WHERE at < ?`, cutoff)
		if err != nil {
			return 0, err
		}
		n, _ := res.RowsAffected()
		total += n
	}
	if maxRows > 0 {
		res, err := s.db.ExecContext(ctx, `
			DELETE FROM repl_log WHERE seq <= (
				SELECT seq FROM repl_log ORDER BY seq DESC LIMIT 1 OFFSET ?
			)`, maxRows)
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected()
		total += n
	}
	return total, nil
}

// ReplApplied는 이 노드가 적용한 마지막 seq다.
func (s *Store) ReplApplied(ctx context.Context) (int64, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT v FROM repl_state WHERE k = 'applied_seq'`).Scan(&v)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	n, _ := strconv.ParseInt(v, 10, 64)
	return n, nil
}

// rowExecer는 *sql.Tx와 *sql.Conn을 함께 받는다.
type rowExecer interface {
	ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error)
}

func setApplied(ctx context.Context, ex rowExecer, seq int64) error {
	_, err := ex.ExecContext(ctx, `
		INSERT INTO repl_state (k, v) VALUES ('applied_seq', ?)
		ON CONFLICT (k) DO UPDATE SET v = excluded.v`, strconv.FormatInt(seq, 10))
	return err
}

// ApplyReplChanges는 받은 변경을 한 트랜잭션으로 적용한다.
//
// 한 트랜잭션인 이유: 절반만 적용된 상태에서 프로세스가 죽으면 applied_seq와 실제 내용이
// 어긋나고, 그 어긋남은 다음 복제에서 드러나지 않는다. 전부 적용하거나 아무것도 적용하지
// 않아야 "seq까지는 마스터와 같다"는 말이 참이 된다.
func (s *Store) ApplyReplChanges(ctx context.Context, changes []ReplChange) (int64, error) {
	if len(changes) == 0 {
		return 0, nil
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return 0, err
	}
	defer conn.Close()

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	// 외래키 검사를 커밋 시점으로 미룬다. 마스터가 커밋한 순서 그대로 적용하지만,
	// 한 트랜잭션 안에서는 참조 대상이 뒤에 올 수 있다(부모·자식이 같은 커밋에 있을 때).
	if _, err := tx.ExecContext(ctx, `PRAGMA defer_foreign_keys = ON`); err != nil {
		return 0, err
	}

	var last int64
	for _, ch := range changes {
		if replSkip[ch.Table] {
			last = ch.Seq
			continue
		}
		switch ch.Op {
		case "delete":
			if _, err := tx.ExecContext(ctx,
				fmt.Sprintf(`DELETE FROM %q WHERE rowid = ?`, ch.Table), ch.RowID); err != nil {
				return 0, fmt.Errorf("seq %d delete %s: %w", ch.Seq, ch.Table, err)
			}
		case "upsert":
			if err := s.applyUpsert(ctx, tx, ch); err != nil {
				return 0, err
			}
		default:
			return 0, fmt.Errorf("seq %d: 알 수 없는 변경 종류 %q", ch.Seq, ch.Op)
		}
		last = ch.Seq
	}

	if err := setApplied(ctx, tx, last); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return last, nil
}

func (s *Store) applyUpsert(ctx context.Context, tx *sql.Tx, ch ReplChange) error {
	var row map[string]any
	if err := json.Unmarshal(ch.Row, &row); err != nil {
		return fmt.Errorf("seq %d %s: 행을 읽지 못했습니다: %w", ch.Seq, ch.Table, err)
	}
	cols, blobs, err := s.columnsOf(ctx, ch.Table)
	if err != nil {
		return err
	}
	known := map[string]bool{}
	for _, c := range cols {
		known[c] = true
	}

	names := []string{"rowid"}
	args := []any{ch.RowID}
	// 키 순서를 고정한다. 순서가 흔들리면 같은 내용의 문장이 매번 달라져
	// SQLite의 문장 캐시가 무의미해진다.
	keys := make([]string, 0, len(row))
	for k := range row {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		// 마스터에 있고 여기에 없는 컬럼은 버전이 다르다는 뜻이다. 넣으려 하면
		// 그 행만이 아니라 복제 전체가 멈추므로, 건너뛰고 계속 간다.
		if !known[k] {
			continue
		}
		v := row[k]
		if blobs[k] {
			if str, ok := v.(string); ok {
				raw, err := hex.DecodeString(str)
				if err != nil {
					return fmt.Errorf("seq %d %s.%s: 16진 값을 되돌리지 못했습니다: %w", ch.Seq, ch.Table, k, err)
				}
				v = raw
			}
		}
		names = append(names, k)
		args = append(args, v)
	}

	// INSERT OR REPLACE를 쓰지 않는 이유가 있다.
	//
	// REPLACE는 충돌하는 행을 **지우고 다시 넣는다**. 그 삭제는 ON DELETE CASCADE를
	// 깨우므로, 부모 행 하나를 복제하는 것만으로 자식 행이 통째로 사라진다. 실제로
	// 겪은 증상은 이랬다: 마스터에서 로그인 → 세션이 리플리카로 복제됨 → 곧이어 그
	// 사용자 행이 복제되는 순간 세션이 사라짐 → 그 노드에서만 "로그인이 필요합니다".
	// 오류도 경고도 남지 않는다.
	//
	// 그래서 있으면 고치고 없으면 넣는다. 자식 행을 건드리지 않는 유일한 방법이다.
	sets := make([]string, 0, len(names))
	updateArgs := make([]any, 0, len(args))
	for i, n := range names {
		if n == "rowid" {
			continue
		}
		sets = append(sets, fmt.Sprintf("%q = ?", n))
		updateArgs = append(updateArgs, args[i])
	}
	if len(sets) > 0 {
		updateArgs = append(updateArgs, ch.RowID)
		res, err := tx.ExecContext(ctx,
			fmt.Sprintf(`UPDATE %q SET %s WHERE rowid = ?`, ch.Table, strings.Join(sets, ", ")),
			updateArgs...)
		if err != nil {
			return fmt.Errorf("seq %d update %s: %w", ch.Seq, ch.Table, err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			return nil
		}
	}

	quoted := make([]string, len(names))
	holes := make([]string, len(names))
	for i, n := range names {
		quoted[i] = fmt.Sprintf("%q", n)
		holes[i] = "?"
	}
	stmt := fmt.Sprintf(`INSERT INTO %q (%s) VALUES (%s)`,
		ch.Table, strings.Join(quoted, ", "), strings.Join(holes, ", "))
	if _, err := tx.ExecContext(ctx, stmt, args...); err != nil {
		return fmt.Errorf("seq %d insert %s: %w", ch.Seq, ch.Table, err)
	}
	return nil
}

// SnapshotTo는 지금 시점의 메타 DB를 파일 하나로 떠낸다.
//
// VACUUM INTO를 쓰는 이유: 실행 중인 DB를 파일 복사로 뜨면 WAL에만 있는 최신 변경이
// 빠지거나 반쯤 쓰인 페이지가 섞인다. VACUUM INTO는 일관된 시점의 완전한 DB를 만든다.
func (s *Store) SnapshotTo(ctx context.Context, path string) error {
	// 같은 이름의 파일이 있으면 SQLite가 거부한다.
	_ = os.Remove(path)
	safe := strings.ReplaceAll(path, "'", "''")
	if _, err := s.db.ExecContext(ctx, fmt.Sprintf(`VACUUM INTO '%s'`, safe)); err != nil {
		return fmt.Errorf("스냅샷 생성 실패: %w", err)
	}
	return nil
}

// LoadSnapshot은 받은 스냅샷 파일의 내용으로 이 노드의 메타 DB를 통째로 맞춘다.
//
// 파일을 바꿔치기하지 않는 이유: 지금 이 프로세스가 그 파일을 열고 있고, 열린 채로
// 교체하면 이미 열린 커넥션들이 사라진 파일을 계속 붙들게 된다. 대신 표 단위로
// 비우고 채운다 — 한 트랜잭션 안에서 일어나므로 중간 상태가 보이지 않는다.
func (s *Store) LoadSnapshot(ctx context.Context, path string) (int64, error) {
	tables, err := s.ReplTables(ctx)
	if err != nil {
		return 0, err
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return 0, err
	}
	defer conn.Close()

	safe := strings.ReplaceAll(path, "'", "''")
	if _, err := conn.ExecContext(ctx, fmt.Sprintf(`ATTACH DATABASE '%s' AS snap`, safe)); err != nil {
		return 0, fmt.Errorf("스냅샷을 열지 못했습니다: %w", err)
	}
	defer conn.ExecContext(context.WithoutCancel(ctx), `DETACH DATABASE snap`)

	var seq sql.NullInt64
	if err := conn.QueryRowContext(ctx, `SELECT max(seq) FROM snap.repl_log`).Scan(&seq); err != nil {
		return 0, fmt.Errorf("스냅샷의 복제 지점을 읽지 못했습니다: %w", err)
	}

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `PRAGMA defer_foreign_keys = ON`); err != nil {
		return 0, err
	}

	for _, t := range tables {
		// 스냅샷에 없는 표는 건너뛴다(버전 차이). 지우기만 하면 그 표가 통째로 비어
		// 버리는데, 그것은 복제가 아니라 파괴다.
		var exists int
		if err := tx.QueryRowContext(ctx,
			`SELECT count(*) FROM snap.sqlite_master WHERE type='table' AND name = ?`, t).Scan(&exists); err != nil {
			return 0, err
		}
		if exists == 0 {
			continue
		}
		cols, _, err := s.columnsOf(ctx, t)
		if err != nil {
			return 0, err
		}
		snapCols, err := snapshotColumns(ctx, tx, t)
		if err != nil {
			return 0, err
		}
		shared := []string{"rowid"}
		for _, c := range cols {
			if snapCols[c] {
				shared = append(shared, fmt.Sprintf("%q", c))
			}
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM %q`, t)); err != nil {
			return 0, fmt.Errorf("%s 비우기 실패: %w", t, err)
		}
		stmt := fmt.Sprintf(`INSERT INTO %q (%s) SELECT %s FROM snap.%q`,
			t, strings.Join(shared, ", "), strings.Join(shared, ", "), t)
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return 0, fmt.Errorf("%s 채우기 실패: %w", t, err)
		}
	}

	if err := setApplied(ctx, tx, seq.Int64); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return seq.Int64, nil
}

func snapshotColumns(ctx context.Context, tx *sql.Tx, table string) (map[string]bool, error) {
	rows, err := tx.QueryContext(ctx, fmt.Sprintf(`PRAGMA snap.table_info(%q)`, table))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull int
		var dflt sql.NullString
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			return nil, err
		}
		out[name] = true
	}
	return out, rows.Err()
}

// RemoteAudit은 감사 기록을 다른 노드(마스터)로 보내는 함수다.
type RemoteAudit func(ctx context.Context, p AuditParams) error

// SetReplicaMode는 이 노드가 메타 DB의 주인이 아님을 알린다.
//
// 무엇이 달라지는가: 이 노드에서 생기는 부수적인 쓰기(세션의 마지막 접속 시각 같은 것)를
// 하지 않고, 감사 기록은 마스터로 보낸다. 여기에 쓰면 다음 복제 때 사라지므로,
// 쓰지 않는 편이 "썼다가 조용히 없어지는" 것보다 정직하다.
func (s *Store) SetReplicaMode(audit RemoteAudit) {
	s.replicaMu.Lock()
	defer s.replicaMu.Unlock()
	s.replica = true
	s.remoteAudit = audit
}

// IsReplica는 이 노드가 복제본인지 여부다.
func (s *Store) IsReplica() bool {
	s.replicaMu.RLock()
	defer s.replicaMu.RUnlock()
	return s.replica
}

func (s *Store) auditForwarder() (bool, RemoteAudit) {
	s.replicaMu.RLock()
	defer s.replicaMu.RUnlock()
	return s.replica, s.remoteAudit
}
