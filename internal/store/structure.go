package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"dbstudio/internal/erd"
)

// StructureView는 구조 화면에서 한 사람이 정리해 둔 배치다.
//
// 스키마는 담지 않는다. 스키마는 실제 DB(또는 저장된 버전)에서 읽으며, 여기에
// 복사해 두면 두 벌이 되어 어느 쪽이 맞는지 알 수 없게 된다. 여기 있는 것은
// 그 스키마 위에 사람이 얹은 것뿐이다.
type StructureView struct {
	Layout map[string]*erd.Box `json:"layout"`
	Notes  []*erd.Note         `json:"notes"`
	Groups []*erd.Group        `json:"groups"`
}

func emptyStructureView() *StructureView {
	return &StructureView{
		Layout: map[string]*erd.Box{},
		Notes:  []*erd.Note{},
		Groups: []*erd.Group{},
	}
}

// 0032부터 구조 화면의 정리는 공유 문서(erd_documents.kind='structure')에 있다.
// 이 표는 그 전에 쌓인 개인 정리이며, 구조 문서를 처음 만들 때 씨앗으로 한 번 읽는다.
// 새로 쓰는 경로는 없다 — 두 곳에 쓰면 어느 쪽이 화면에 보이는지 알 수 없게 된다.

// GetStructureView는 한 사람의 배치를 읽는다.
// 저장한 적이 없으면 빈 배치를 돌려준다 — 없는 것과 비어 있는 것을 화면이 구분할
// 이유가 없고, 호출부마다 nil을 확인하게 하면 언젠가 빠뜨린다.
func (s *Store) GetStructureView(ctx context.Context, userID, connectionID string) (*StructureView, error) {
	var layoutJSON, notesJSON, groupsJSON string
	err := s.db.QueryRowContext(ctx, `SELECT layout_json, notes_json, groups_json
		FROM erd_structure_views WHERE user_id = ? AND connection_id = ?`,
		userID, connectionID).Scan(&layoutJSON, &notesJSON, &groupsJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return emptyStructureView(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("scan structure view: %w", err)
	}

	out := emptyStructureView()
	if err := json.Unmarshal([]byte(layoutJSON), &out.Layout); err != nil {
		return nil, fmt.Errorf("unmarshal structure layout: %w", err)
	}
	if err := json.Unmarshal([]byte(notesJSON), &out.Notes); err != nil {
		return nil, fmt.Errorf("unmarshal structure notes: %w", err)
	}
	if err := json.Unmarshal([]byte(groupsJSON), &out.Groups); err != nil {
		return nil, fmt.Errorf("unmarshal structure groups: %w", err)
	}
	if out.Layout == nil {
		out.Layout = map[string]*erd.Box{}
	}
	if out.Notes == nil {
		out.Notes = []*erd.Note{}
	}
	if out.Groups == nil {
		out.Groups = []*erd.Group{}
	}
	return out, nil
}

// SaveStructureView는 한 사람의 배치를 통째로 덮어쓴다.
//
// 부분 갱신(카드 하나만)을 지원하지 않는 이유: 이 값은 한 사람만 쓰므로 동시 편집이
// 없고, 통째로 쓰면 "무엇이 지워졌는가"를 서버가 계산할 필요가 없다. 화면이 가진
// 것이 곧 정답이다.
func (s *Store) SaveStructureView(ctx context.Context, userID, connectionID string, v *StructureView) error {
	if v == nil {
		v = emptyStructureView()
	}
	if v.Layout == nil {
		v.Layout = map[string]*erd.Box{}
	}
	if v.Notes == nil {
		v.Notes = []*erd.Note{}
	}
	if v.Groups == nil {
		v.Groups = []*erd.Group{}
	}
	layoutJSON, err := json.Marshal(v.Layout)
	if err != nil {
		return fmt.Errorf("marshal structure layout: %w", err)
	}
	notesJSON, err := json.Marshal(v.Notes)
	if err != nil {
		return fmt.Errorf("marshal structure notes: %w", err)
	}
	groupsJSON, err := json.Marshal(v.Groups)
	if err != nil {
		return fmt.Errorf("marshal structure groups: %w", err)
	}

	_, err = s.db.ExecContext(ctx, `INSERT INTO erd_structure_views
		(user_id, connection_id, layout_json, notes_json, groups_json, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (user_id, connection_id) DO UPDATE SET
			layout_json = excluded.layout_json,
			notes_json  = excluded.notes_json,
			groups_json = excluded.groups_json,
			updated_at  = excluded.updated_at`,
		userID, connectionID, string(layoutJSON), string(notesJSON), string(groupsJSON), nowString())
	if err != nil {
		return fmt.Errorf("save structure view: %w", err)
	}
	return nil
}
