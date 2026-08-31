package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"dbstudio/internal/erd"
	"dbstudio/internal/schema"
)

// ErrOpConflict은 같은 op_id가 이미 저장되어 있을 때 반환된다.
// 재전송된 op를 두 번 적용하면 컬럼이 둘 생기므로, 호출자는 이를 성공으로 취급하고
// 기존 결과를 다시 알려주어야 한다.
var ErrOpConflict = errors.New("op already applied")

// ERDDocumentMeta는 목록 화면용 요약이다. 스냅샷 본문을 읽지 않는다.
type ERDDocumentMeta struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// ProjectID는 이 문서가 속한 프로젝트다. 독립 초안(커넥션 없음)의 권한 판정은
	// 이 값 하나에 달려 있다.
	ProjectID string `json:"projectId"`
	// ConnectionID가 비어 있으면 대상 DB가 없는 독립 초안이다.
	ConnectionID string    `json:"connectionId"`
	Dialect      string    `json:"dialect"`
	Status       string    `json:"status"`
	Seq          int64     `json:"seq"`
	Note         string    `json:"note,omitempty"`
	CreatedBy    string    `json:"createdBy,omitempty"`
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
	// TableCount는 목록에서 규모를 보여주기 위한 값이다.
	TableCount int `json:"tableCount"`
}

// CreateERDDocument는 새 문서를 저장한다.
func (s *Store) CreateERDDocument(ctx context.Context, doc *erd.Document, createdBy, note string, sourceSnapshotID *int64) error {
	j, err := marshalDocument(doc)
	if err != nil {
		return err
	}
	// 대상 커넥션이 있으면 프로젝트는 그 커넥션의 것이다.
	//
	// 두 곳에 적힌 값이 어긋나면 어느 쪽이 참인지 판정이 답할 수 없게 된다. 부르는
	// 쪽이 무엇을 넣었든 커넥션 쪽으로 맞추는 이유가 그것이다 — 커넥션은 실제
	// 대상이고, 문서는 그것을 향한 그림이다.
	if doc.ConnectionID != "" {
		conn, cerr := s.GetConnection(ctx, doc.ConnectionID)
		if cerr != nil {
			return fmt.Errorf("resolve document project: %w", cerr)
		}
		doc.ProjectID = conn.ProjectID
	}
	if doc.ProjectID == "" {
		return fmt.Errorf("insert erd document: 프로젝트가 없습니다")
	}
	now := nowString()
	_, err = s.db.ExecContext(ctx, `INSERT INTO erd_documents
		(id, name, project_id, connection_id, dialect, status, kind, snapshot_json, layout_json,
		 notes_json, groups_json, domains_json, snapshot_seq, seq, source_snapshot_id, note,
		 created_by, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		// 대상 커넥션이 없으면 NULL이다. 빈 문자열로 넣으면 외래키가 걸려 들어가지
		// 않고, 들어간다 해도 "존재하지 않는 커넥션을 가리키는 문서"가 된다.
		doc.ID, doc.Name, doc.ProjectID, nullString(doc.ConnectionID), doc.Dialect, doc.Status,
		docKind(doc.Kind), j.schema, j.layout, j.notes, j.groups, j.domains, doc.Seq, doc.Seq,
		sourceSnapshotID, note, nullString(createdBy), now, now)
	if err != nil {
		return fmt.Errorf("insert erd document: %w", err)
	}
	return nil
}

// GetERDDocument는 문서를 스냅샷에서 복원하고 그 이후의 op를 적용해 현재 상태를 만든다.
//
// 스냅샷 + 잔여 op 방식을 쓰는 이유: 문서 상태를 매 op마다 통째로 다시 쓰면
// 큰 스키마에서 쓰기 비용이 op 수에 비례해 커진다. 반대로 op만 쌓으면 로딩이
// 무한히 느려진다. 주기적 압축이 두 비용을 모두 묶는다.
func (s *Store) GetERDDocument(ctx context.Context, id string) (*erd.Document, error) {
	row := s.db.QueryRowContext(ctx, `SELECT
		id, name, project_id, COALESCE(connection_id, ''), dialect, status, kind,
		snapshot_json, layout_json, notes_json,
		groups_json, domains_json, snapshot_seq, seq
		FROM erd_documents WHERE id = ?`, id)

	var doc erd.Document
	var j docJSON
	var snapshotSeq int64
	if err := row.Scan(&doc.ID, &doc.Name, &doc.ProjectID, &doc.ConnectionID, &doc.Dialect, &doc.Status, &doc.Kind,
		&j.schema, &j.layout, &j.notes, &j.groups, &j.domains, &snapshotSeq, &doc.Seq); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("scan erd document: %w", err)
	}

	if err := unmarshalDocument(&doc, j); err != nil {
		return nil, err
	}

	// 스냅샷 이후의 op를 순서대로 적용한다.
	if snapshotSeq < doc.Seq {
		ops, err := s.ListERDOps(ctx, id, snapshotSeq)
		if err != nil {
			return nil, err
		}
		applied := snapshotSeq
		for _, op := range ops {
			if err := erd.Apply(&doc, op); err != nil {
				// 저장된 op가 재생되지 않는 것은 데이터 손상이다. 조용히 넘기면
				// 사용자마다 다른 상태를 보게 되므로 여기서 멈추고 알린다.
				return nil, fmt.Errorf("문서 %s의 op %d(%s) 재생 실패: %w", id, op.Seq, op.Kind, err)
			}
			applied = op.Seq
		}
		doc.Seq = applied
	}
	doc.Schema.Sort()
	return &doc, nil
}

// ListERDDocuments는 문서 요약 목록을 반환한다.
//
// connectionIDs의 nil과 빈 슬라이스는 뜻이 다르다:
//   - nil       — 제한 없음 (관리 목적의 전체 조회)
//   - 빈 슬라이스 — 접근 가능한 커넥션이 하나도 없음 → 커넥션에 매인 문서는 없어야 한다
//
// 이 구분을 len(x) > 0 으로만 판단하면 빈 슬라이스가 "제한 없음"으로 처리되어
// 권한 없는 사용자에게 모든 문서가 노출된다. ListEvents도 같은 이유로 같은 형태다.
//
// 대상 커넥션이 없는 독립 초안(connection_id IS NULL)은 이 필터의 바깥이다.
// 걸러낼 근거가 되는 커넥션이 없고, 그 초안은 실제 DB를 건드리지 않으므로
// 로그인한 사람이면 누구나 본다. 삭제와 설정 변경만 작성자·어드민으로 좁힌다.
// projectIDs가 nil이면 프로젝트로 좁히지 않는다(슈퍼 어드민). 빈 슬라이스는
// "볼 수 있는 프로젝트가 하나도 없다"는 뜻이라 결과도 비어야 한다.
func (s *Store) ListERDDocuments(ctx context.Context, connectionIDs, projectIDs []string, limit int) ([]*ERDDocumentMeta, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	query := `SELECT id, name, project_id, COALESCE(connection_id, ''), dialect, status, seq, note,
		COALESCE(created_by, ''), created_at, updated_at,
		-- 테이블 수는 스냅샷 JSON 전체를 디코딩하지 않고 세는 것이 목적이지만
		-- SQLite에서 JSON 배열 길이는 json_array_length로 싸게 얻을 수 있다.
		COALESCE(json_array_length(snapshot_json, '$.tables'), 0)
		FROM erd_documents`
	// 구조 문서는 초안이 아니다. 목록에 섞이면 마이그레이션 대상 후보에도 뜨는데,
	// 그것은 실제 DB의 사본이라 적용할 것이 언제나 없다.
	args := []any{}
	query += ` WHERE kind <> 'structure'`
	if connectionIDs != nil {
		where := `connection_id IS NULL`
		if len(connectionIDs) > 0 {
			marks := make([]string, len(connectionIDs))
			for i, id := range connectionIDs {
				marks[i] = "?"
				args = append(args, id)
			}
			where += ` OR connection_id IN (` + strings.Join(marks, ",") + `)`
		}
		query += ` AND (` + where + `)`
	}
	if projectIDs != nil {
		if len(projectIDs) == 0 {
			// IN () 는 SQLite에서 문법 오류다. 아무것도 맞지 않는 조건으로 바꾼다.
			query += ` AND 0`
		} else {
			marks := make([]string, len(projectIDs))
			for i, id := range projectIDs {
				marks[i] = "?"
				args = append(args, id)
			}
			query += ` AND project_id IN (` + strings.Join(marks, ",") + `)`
		}
	}
	query += ` ORDER BY updated_at DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list erd documents: %w", err)
	}
	defer rows.Close()

	out := []*ERDDocumentMeta{}
	for rows.Next() {
		var m ERDDocumentMeta
		var createdAt, updatedAt string
		if err := rows.Scan(&m.ID, &m.Name, &m.ProjectID, &m.ConnectionID, &m.Dialect, &m.Status,
			&m.Seq, &m.Note, &m.CreatedBy, &createdAt, &updatedAt, &m.TableCount); err != nil {
			return nil, fmt.Errorf("scan erd document meta: %w", err)
		}
		m.CreatedAt, m.UpdatedAt = parseTime(createdAt), parseTime(updatedAt)
		out = append(out, &m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate erd documents: %w", err)
	}
	return out, nil
}

// GetERDDocumentMeta는 문서 하나의 요약을 읽는다.
//
// 목록 조회를 재사용하지 않는 이유: 목록은 상한(기본 200건)이 있어, 문서가 그보다
// 많아지면 오래된 문서의 메타데이터를 "찾을 수 없다"고 답하게 된다. 그 실패는
// 이름 변경이 안 되는 형태로만 드러나 원인을 짚기 어렵다.
func (s *Store) GetERDDocumentMeta(ctx context.Context, id string) (*ERDDocumentMeta, error) {
	var m ERDDocumentMeta
	var createdAt, updatedAt string
	err := s.db.QueryRowContext(ctx, `SELECT id, name, project_id, COALESCE(connection_id, ''),
		dialect, status, seq, note, COALESCE(created_by, ''), created_at, updated_at,
		COALESCE(json_array_length(snapshot_json, '$.tables'), 0)
		FROM erd_documents WHERE id = ?`, id).
		Scan(&m.ID, &m.Name, &m.ProjectID, &m.ConnectionID, &m.Dialect, &m.Status, &m.Seq, &m.Note,
			&m.CreatedBy, &createdAt, &updatedAt, &m.TableCount)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan erd document meta: %w", err)
	}
	m.CreatedAt, m.UpdatedAt = parseTime(createdAt), parseTime(updatedAt)
	return &m, nil
}

// UpdateERDDocumentMeta는 이름·상태·메모만 바꾼다. 구조 변경은 op로만 이뤄진다.
func (s *Store) UpdateERDDocumentMeta(ctx context.Context, id, name, status, note string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE erd_documents
		SET name = ?, status = ?, note = ?, updated_at = ? WHERE id = ?`,
		name, status, note, nowString(), id)
	if err != nil {
		return fmt.Errorf("update erd document: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) DeleteERDDocument(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM erd_documents WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete erd document: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// AppendERDOp은 op를 저장하고 문서의 seq를 올린다.
//
// seq 부여와 저장을 한 트랜잭션에 묶는 이유: 두 사람의 op가 같은 seq를 받으면
// 재생 순서가 모호해지고, 그러면 스냅샷 복원 결과가 참여자마다 달라질 수 있다.
// op_id 유니크 제약 위반은 ErrOpConflict으로 구분해 재전송을 안전하게 처리한다.
//
// docID가 아니라 문서를 받는 이유: 성공 시 doc.Seq를 함께 올려준다. 호출자가
// 이 갱신을 잊으면 이후의 압축이 "내가 가진 상태가 최신인지" 판단을 틀리게 하고,
// 그 실패는 조용하다. 갱신 주체를 한 곳으로 모아 그 실수를 불가능하게 만든다.
func (s *Store) AppendERDOp(ctx context.Context, doc *erd.Document, op *erd.Op) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin append op: %w", err)
	}
	defer tx.Rollback()

	var seq int64
	if err := tx.QueryRowContext(ctx,
		`SELECT seq FROM erd_documents WHERE id = ?`, doc.ID).Scan(&seq); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("read doc seq: %w", err)
	}
	next := seq + 1

	now := nowString()
	if _, err := tx.ExecContext(ctx, `INSERT INTO erd_ops
		(doc_id, seq, op_id, kind, payload, actor_id, actor_name, base_seq, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		doc.ID, next, op.ID, string(op.Kind), string(op.Payload),
		nullString(op.Actor), op.ActorName, op.BaseSeq, now); err != nil {
		if isUniqueViolation(err) {
			return ErrOpConflict
		}
		return fmt.Errorf("insert erd op: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE erd_documents SET seq = ?, updated_at = ? WHERE id = ?`,
		next, now, doc.ID); err != nil {
		return fmt.Errorf("bump doc seq: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit append op: %w", err)
	}
	op.Seq = next
	op.At = parseTime(now)
	doc.Seq = next
	return nil
}

// ListERDOps는 afterSeq보다 큰 op를 순서대로 반환한다.
func (s *Store) ListERDOps(ctx context.Context, docID string, afterSeq int64) ([]*erd.Op, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT
		seq, op_id, kind, payload, COALESCE(actor_id, ''), actor_name, base_seq, created_at
		FROM erd_ops WHERE doc_id = ? AND seq > ? ORDER BY seq`, docID, afterSeq)
	if err != nil {
		return nil, fmt.Errorf("list erd ops: %w", err)
	}
	defer rows.Close()

	out := []*erd.Op{}
	for rows.Next() {
		var op erd.Op
		var kind, payload, createdAt string
		if err := rows.Scan(&op.Seq, &op.ID, &kind, &payload,
			&op.Actor, &op.ActorName, &op.BaseSeq, &createdAt); err != nil {
			return nil, fmt.Errorf("scan erd op: %w", err)
		}
		op.Kind = erd.Kind(kind)
		op.Payload = json.RawMessage(payload)
		op.At = parseTime(createdAt)
		out = append(out, &op)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate erd ops: %w", err)
	}
	return out, nil
}

// RecentERDOpIDs는 최근 op의 클라이언트 ID를 반환한다.
//
// 허브가 재전송을 걸러내는 데 쓴다. 문서 상태 복원에는 필요 없지만, 방이 새로
// 만들어진 직후 재접속한 클라이언트가 보내는 재전송을 검증 이전에 알아내야 한다.
func (s *Store) RecentERDOpIDs(ctx context.Context, docID string, limit int) ([]string, error) {
	if limit <= 0 || limit > 5000 {
		limit = 1000
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT op_id FROM erd_ops WHERE doc_id = ? ORDER BY seq DESC LIMIT ?`, docID, limit)
	if err != nil {
		return nil, fmt.Errorf("list recent op ids: %w", err)
	}
	defer rows.Close()

	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan op id: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate op ids: %w", err)
	}
	return out, nil
}

// CompactERDDocument는 현재 상태를 스냅샷으로 굳히고 그 이전 op를 지운다.
//
// op를 지우는 것은 이력을 버리는 일이므로 기본적으로는 남긴다. 이 함수는
// 명시적으로 호출될 때만 정리하며, keepOps 개수만큼은 항상 남겨 최근 변경 이력을
// 화면에서 볼 수 있게 한다.
//
// 반환값 compacted가 false면 그 사이 새 op가 들어와 건너뛴 것이다. 조용히 nil만
// 돌려주면 호출자는 압축이 된 줄 알고 다음 압축 시점을 뒤로 미루게 된다.
func (s *Store) CompactERDDocument(ctx context.Context, doc *erd.Document, keepOps int64) (bool, error) {
	j, err := marshalDocument(doc)
	if err != nil {
		return false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin compact: %w", err)
	}
	defer tx.Rollback()

	// 문서의 현재 seq와 압축 대상 seq가 같은지 확인한다. 압축 도중 새 op가 들어오면
	// 스냅샷이 그 op를 이미 포함하는지 알 수 없으므로 압축을 건너뛴다.
	var current int64
	if err := tx.QueryRowContext(ctx, `SELECT seq FROM erd_documents WHERE id = ?`, doc.ID).Scan(&current); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, ErrNotFound
		}
		return false, fmt.Errorf("read doc seq: %w", err)
	}
	if current != doc.Seq {
		return false, nil
	}

	if _, err := tx.ExecContext(ctx, `UPDATE erd_documents
		SET snapshot_json = ?, layout_json = ?, notes_json = ?, groups_json = ?,
		    domains_json = ?, snapshot_seq = ?, updated_at = ?
		WHERE id = ?`,
		j.schema, j.layout, j.notes, j.groups, j.domains, doc.Seq, nowString(), doc.ID); err != nil {
		return false, fmt.Errorf("write snapshot: %w", err)
	}
	if keepOps >= 0 {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM erd_ops WHERE doc_id = ? AND seq <= ?`,
			doc.ID, doc.Seq-keepOps); err != nil {
			return false, fmt.Errorf("prune erd ops: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit compact: %w", err)
	}
	return true, nil
}

// ---------- 채팅 ----------

// ERDChatMessage는 문서 채팅의 한 줄이다.
type ERDChatMessage struct {
	ID        int64     `json:"id"`
	DocID     string    `json:"docId"`
	UserID    string    `json:"userId,omitempty"`
	UserName  string    `json:"userName"`
	Body      string    `json:"body"`
	Kind      string    `json:"kind"`
	TargetKey string    `json:"targetKey,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
}

const (
	ChatKindMessage = "message"
	ChatKindSystem  = "system"
)

func (s *Store) AddERDChatMessage(ctx context.Context, m *ERDChatMessage) error {
	if m.Kind == "" {
		m.Kind = ChatKindMessage
	}
	now := nowString()
	res, err := s.db.ExecContext(ctx, `INSERT INTO erd_chat_messages
		(doc_id, user_id, user_name, body, kind, target_key, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		m.DocID, nullString(m.UserID), m.UserName, m.Body, m.Kind, m.TargetKey, now)
	if err != nil {
		return fmt.Errorf("insert chat message: %w", err)
	}
	m.ID, _ = res.LastInsertId()
	m.CreatedAt = parseTime(now)
	return nil
}

// ListERDChatMessages는 최근 메시지를 오래된 것부터 반환한다.
// 화면은 위에서 아래로 시간순으로 읽으므로, 최신 N개를 뒤집어 돌려준다.
func (s *Store) ListERDChatMessages(ctx context.Context, docID string, limit int) ([]*ERDChatMessage, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := s.db.QueryContext(ctx, `SELECT
		id, doc_id, COALESCE(user_id, ''), user_name, body, kind, target_key, created_at
		FROM erd_chat_messages WHERE doc_id = ? ORDER BY id DESC LIMIT ?`, docID, limit)
	if err != nil {
		return nil, fmt.Errorf("list chat messages: %w", err)
	}
	defer rows.Close()

	out := []*ERDChatMessage{}
	for rows.Next() {
		var m ERDChatMessage
		var createdAt string
		if err := rows.Scan(&m.ID, &m.DocID, &m.UserID, &m.UserName,
			&m.Body, &m.Kind, &m.TargetKey, &createdAt); err != nil {
			return nil, fmt.Errorf("scan chat message: %w", err)
		}
		m.CreatedAt = parseTime(createdAt)
		out = append(out, &m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate chat messages: %w", err)
	}
	// 시간 오름차순으로 뒤집는다.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// ---------- 직렬화 ----------

// docJSON은 문서에서 칼럼으로 나가는 JSON 조각들이다.
//
// 문자열 네댓 개를 순서대로 주고받지 않는 이유: 같은 타입이 늘어설수록 호출부에서
// 자리를 바꿔 넣어도 컴파일러가 잡지 못한다(레이아웃이 메모 칸에 들어가는 식).
type docJSON struct {
	schema  string
	layout  string
	notes   string
	groups  string
	domains string
}

func marshalDocument(doc *erd.Document) (docJSON, error) {
	if doc.Schema == nil {
		doc.Schema = &schema.Schema{Dialect: doc.Dialect, Shape: schema.ShapeRelational}
	}
	if doc.Layout == nil {
		doc.Layout = map[string]*erd.Box{}
	}
	if doc.Notes == nil {
		doc.Notes = []*erd.Note{}
	}
	if doc.Groups == nil {
		doc.Groups = []*erd.Group{}
	}
	if doc.Domains == nil {
		doc.Domains = []*erd.Domain{}
	}
	out := docJSON{}
	for _, part := range []struct {
		what string
		src  any
		dst  *string
	}{
		{"schema", doc.Schema, &out.schema},
		{"layout", doc.Layout, &out.layout},
		{"notes", doc.Notes, &out.notes},
		{"groups", doc.Groups, &out.groups},
		{"domains", doc.Domains, &out.domains},
	} {
		raw, err := json.Marshal(part.src)
		if err != nil {
			return docJSON{}, fmt.Errorf("marshal erd %s: %w", part.what, err)
		}
		*part.dst = string(raw)
	}
	return out, nil
}

func unmarshalDocument(doc *erd.Document, j docJSON) error {
	var sc schema.Schema
	if err := json.Unmarshal([]byte(j.schema), &sc); err != nil {
		return fmt.Errorf("unmarshal erd schema: %w", err)
	}
	if sc.Tables == nil {
		sc.Tables = []*schema.Table{}
	}
	if sc.Views == nil {
		sc.Views = []*schema.View{}
	}
	doc.Schema = &sc

	doc.Layout = map[string]*erd.Box{}
	if j.layout != "" {
		if err := json.Unmarshal([]byte(j.layout), &doc.Layout); err != nil {
			return fmt.Errorf("unmarshal erd layout: %w", err)
		}
	}
	doc.Notes = []*erd.Note{}
	if j.notes != "" {
		if err := json.Unmarshal([]byte(j.notes), &doc.Notes); err != nil {
			return fmt.Errorf("unmarshal erd notes: %w", err)
		}
	}
	doc.Groups = []*erd.Group{}
	if j.groups != "" {
		if err := json.Unmarshal([]byte(j.groups), &doc.Groups); err != nil {
			return fmt.Errorf("unmarshal erd groups: %w", err)
		}
	}
	doc.Domains = []*erd.Domain{}
	if j.domains != "" {
		if err := json.Unmarshal([]byte(j.domains), &doc.Domains); err != nil {
			return fmt.Errorf("unmarshal erd domains: %w", err)
		}
	}
	return nil
}

// ---------- 구조 문서 ----------

// DocKindDraft / DocKindStructure는 erd_documents.kind 값이다.
//
// 구조 문서는 커넥션마다 하나이고, "지금 이 DB가 이렇게 생겼다"를 함께 보는 자리다.
// 초안과 같은 캔버스·같은 실시간 방을 쓰지만 스키마 레이어는 읽기 전용이다.
const (
	DocKindDraft     = "draft"
	DocKindStructure = "structure"
)

// docKind는 빈 값을 초안으로 본다. 기존 문서는 모두 초안이다.
func docKind(kind string) string {
	if kind == DocKindStructure {
		return DocKindStructure
	}
	return DocKindDraft
}

// GetStructureDocumentID는 커넥션의 구조 문서 id를 찾는다. 없으면 ErrNotFound다.
//
// 문서 전체가 아니라 id만 돌려주는 이유: 호출부는 대개 그 id로 GetERDDocument를
// 부르거나(스냅샷+op 재생) 소켓 경로로 넘긴다. 여기서 통째로 읽으면 그 일을 두 번 한다.
func (s *Store) GetStructureDocumentID(ctx context.Context, connectionID string) (string, error) {
	var id string
	err := s.db.QueryRowContext(ctx,
		`SELECT id FROM erd_documents WHERE connection_id = ? AND kind = ?`,
		connectionID, DocKindStructure).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("find structure document: %w", err)
	}
	return id, nil
}

// ReplaceERDSnapshot은 문서의 스키마 레이어만 통째로 바꾼다.
//
// 구조 문서를 위한 것이다: 실제 DB를 다시 읽었더니 테이블이 달라졌을 때, 그 사실을
// op로 흘려보내지 않고 스냅샷을 갈아 끼운다. op로 만들면 "사람이 한 편집"과 "DB가
// 그렇게 생겼다"가 같은 이력에 섞이고, 되돌리기가 남의 DB 상태를 되돌리려 든다.
//
// seq는 건드리지 않는다. 열려 있는 클라이언트가 자기 op 순서를 그대로 이어 간다.
func (s *Store) ReplaceERDSnapshot(ctx context.Context, docID string, sc *schema.Schema, layout map[string]*erd.Box) error {
	scJSON, err := json.Marshal(sc)
	if err != nil {
		return fmt.Errorf("marshal schema: %w", err)
	}
	layoutJSON, err := json.Marshal(layout)
	if err != nil {
		return fmt.Errorf("marshal layout: %w", err)
	}
	// snapshot_seq를 현재 seq로 올린다. 그러지 않으면 다음 로딩이 옛 op를 새 스냅샷
	// 위에 다시 적용해, 방금 사라진 테이블을 되살리거나 재생 실패로 문서를 못 연다.
	res, err := s.db.ExecContext(ctx,
		`UPDATE erd_documents
		 SET snapshot_json = ?, layout_json = ?, snapshot_seq = seq, updated_at = ?
		 WHERE id = ?`,
		string(scJSON), string(layoutJSON), nowString(), docID)
	if err != nil {
		return fmt.Errorf("replace snapshot: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
