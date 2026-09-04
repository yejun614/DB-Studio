package erd

import (
	"fmt"
	"strings"

	"dbstudio/internal/schema"
)

// SQL 불러오기.
//
// 이것은 op 하나다. 파싱 결과를 table.add / column.add … 수십 개로 쪼개 보내지
// 않는다. 이유가 둘이다:
//
//  1. 중간 상태가 없어야 한다. 쪼개서 보내면 열 번째 op가 거부되는 순간 초안은
//     "절반만 불러온" 상태로 남고, 그것은 되돌릴 방법이 없다.
//  2. 이력이 읽혀야 한다. 편집 이력에 "테이블 추가" 40줄이 쌓이는 것보다
//     "SQL 불러오기 — 테이블 12개" 한 줄이 나중에 훨씬 쓸모 있다.
//
// payload에는 **파싱된 결과**를 담는다. 원본 SQL을 담고 재생할 때 다시 파싱하면,
// 파서가 나중에 바뀌었을 때 같은 op가 다른 결과를 낳는다. 그러면 op 로그 재생으로
// 복원한 문서가 사람들이 보던 것과 달라진다 — 이 동기화 방식의 전제가 무너진다.

type importPayload struct {
	// Tables는 새로 정의된 테이블이다. 같은 키가 이미 있으면 통째로 교체한다.
	Tables []*schema.Table `json:"tables"`
	Enums  []*schema.Enum  `json:"enums,omitempty"`
	// Views는 스크립트가 정의한 뷰다. 같은 키가 있으면 정의를 덮어쓴다.
	Views []*schema.View `json:"views,omitempty"`
	// Drops는 지울 테이블 키다.
	Drops []string `json:"drops,omitempty"`
	// ViewDrops는 지울 뷰 키다.
	ViewDrops []string `json:"viewDrops,omitempty"`
	// Label은 이력에 남길 설명이다(파일 이름 등).
	Label string `json:"label,omitempty"`
}

// maxImportTables는 한 번에 불러올 수 있는 테이블 수다.
// 문서 하나가 브라우저에서 그려지는 한계이기도 하다.
const maxImportTables = 500

// applySchemaImport는 파싱된 스키마를 초안에 얹는다.
//
// 병합 규칙은 사용자가 이해할 수 있는 가장 단순한 것이다:
//   - 초안에만 있는 테이블은 그대로 둔다.
//   - 같은 이름이 있으면 **불러온 쪽으로 덮어쓴다** (좌표와 색은 유지한다).
//   - DROP 된 테이블은 초안에서도 지운다.
//
// 좌표를 유지하는 이유: 덮어쓰기는 대개 "같은 테이블의 새 버전"이고, 그때 카드가
// 화면 다른 곳으로 튀면 사용자는 자기가 정리해 둔 배치를 처음부터 다시 만들어야 한다.
func applySchemaImport(doc *Document, op *Op) error {
	var p importPayload
	if err := decode(op, &p); err != nil {
		return err
	}
	if len(p.Tables) == 0 && len(p.Drops) == 0 && len(p.Views) == 0 && len(p.ViewDrops) == 0 {
		return invalid("불러올 내용이 없습니다")
	}
	if len(p.Tables) > maxImportTables {
		return invalid("한 번에 불러올 수 있는 테이블은 %d개까지입니다", maxImportTables)
	}

	// 사람이 정리해 둔 배치가 있는지 먼저 적어 둔다. 불러오기가 끝난 뒤 관계에
	// 따라 다시 놓을지를 이것으로 판단한다.
	placedBefore := make(map[string]bool, len(doc.Layout))
	for key := range doc.Layout {
		placedBefore[key] = true
	}

	// 먼저 지운다. 같은 스크립트에서 DROP 후 CREATE 하는 흐름이 흔하고,
	// 순서가 반대면 방금 만든 테이블을 지우게 된다.
	for _, raw := range p.Drops {
		key := strings.ToLower(strings.TrimSpace(raw))
		if key == "" {
			continue
		}
		dropTableByKey(doc, key)
	}

	for _, tbl := range p.Tables {
		if tbl == nil {
			continue
		}
		if _, err := validateIdent("테이블", tbl.Name); err != nil {
			return err
		}
		for _, col := range tbl.Columns {
			if _, err := validateIdent("컬럼", col.Name); err != nil {
				return err
			}
		}
		mergeTable(doc, tbl)
	}
	// 가져온 표에도 문서 기본값을 채운다. 스크립트에 엔진이 적혀 있으면 그것이
	// 이기고, 비어 있는 칸만 문서 기본값으로 메운다.
	applyTableDefaults(doc, doc.Schema.Tables...)

	for _, e := range p.Enums {
		if e == nil {
			continue
		}
		replaceEnum(doc.Schema, e)
	}

	for _, raw := range p.ViewDrops {
		key := strings.ToLower(strings.TrimSpace(raw))
		if key == "" {
			continue
		}
		dropViewByKey(doc, key)
	}
	for _, v := range p.Views {
		if v == nil {
			continue
		}
		if _, err := validateIdent("뷰", v.Name); err != nil {
			return err
		}
		if err := mergeView(doc, v); err != nil {
			return err
		}
	}

	doc.Schema.Sort()

	// 불러온 것이 전부 새 표라면 관계에 따라 다시 놓는다.
	//
	// 왜 이 조건인가: mergeTable 은 새 표를 격자의 빈칸에 하나씩 놓는다. 빈 초안에
	// 스키마를 통째로 불러오면 그 격자가 그대로 도면이 되어, 관계선이 도면을
	// 가로지른다 — 회원과 주문이 대각선으로 마주 보는 그림에서 무엇이 무엇을
	// 가리키는지는 선을 눈으로 따라가야 알 수 있다.
	//
	// 반대로 사람이 카드를 옮겨 둔 문서에서는 아무것도 건드리지 않는다. 표 하나를
	// 더 불러왔다고 남이 정리한 배치를 흐트러뜨리는 것은, 불러오기가 할 일이 아니다.
	fresh := len(doc.Schema.Tables) > 0
	for _, t := range doc.Schema.Tables {
		if placedBefore[t.Key()] {
			fresh = false
			break
		}
	}
	if fresh {
		doc.Layout = AutoLayout(doc.Schema)
	}
	return nil
}

// mergeTable은 테이블 하나를 초안에 넣는다. 같은 키가 있으면 교체한다.
func mergeTable(doc *Document, tbl *schema.Table) {
	key := tbl.Key()
	for i, t := range doc.Schema.Tables {
		if t.Key() == key {
			doc.Schema.Tables[i] = tbl
			// 레이아웃은 건드리지 않는다. 사람이 정리해 둔 배치가 유지된다.
			return
		}
	}
	doc.Schema.Tables = append(doc.Schema.Tables, tbl)
	if _, ok := doc.Layout[key]; !ok {
		x, y := doc.nextFreeSlot()
		doc.Layout[key] = &Box{X: x, Y: y}
	}
}

// dropTableByKey는 테이블과 그 테이블을 참조하던 외래키를 함께 지운다.
//
// 참조를 남겨 두면 만들 수 없는 DDL이 된다. table.delete가 cascade 없이는 거부하는
// 것과 다르게 여기서는 항상 지우는데, 불러오기에서 DROP은 사용자가 스크립트에
// 명시적으로 적은 것이고 되물을 자리가 없기 때문이다. 무엇이 함께 지워졌는지는
// 화면이 미리보기에서 알려준다.
func dropTableByKey(doc *Document, key string) {
	var target *schema.Table
	for _, t := range doc.Schema.Tables {
		if t.Key() == key {
			target = t
			break
		}
	}
	if target == nil {
		return
	}
	kept := doc.Schema.Tables[:0]
	for _, t := range doc.Schema.Tables {
		if t == target {
			continue
		}
		fks := t.ForeignKeys[:0]
		for _, fk := range t.ForeignKeys {
			if fk.RefKey() == key {
				continue
			}
			fks = append(fks, fk)
		}
		t.ForeignKeys = fks
		kept = append(kept, t)
	}
	doc.Schema.Tables = kept
	delete(doc.Layout, key)
}

func replaceEnum(sc *schema.Schema, e *schema.Enum) {
	for i, old := range sc.Enums {
		if old.Key() == e.Key() {
			sc.Enums[i] = e
			return
		}
	}
	sc.Enums = append(sc.Enums, e)
}

// ImportSummary는 불러오기가 초안에 무엇을 할지 미리 설명한다.
//
// 적용과 같은 규칙으로 계산해야 한다. 미리보기와 결과가 갈리면 미리보기는
// 확인이 아니라 거짓말이 된다.
type ImportSummary struct {
	Added   []string `json:"added"`
	Updated []string `json:"updated"`
	Dropped []string `json:"dropped"`
	// DroppedRefs는 테이블이 지워지면서 함께 사라지는 외래키다.
	DroppedRefs []string `json:"droppedRefs,omitempty"`
	// MissingRefs는 참조 대상이 초안에도 스크립트에도 없는 외래키다.
	// 막지는 않는다 — 나머지 테이블을 나중에 불러오는 흐름이 정상이기 때문이다.
	MissingRefs []string `json:"missingRefs,omitempty"`
	// 뷰도 표와 같은 규칙으로 센다. 미리보기가 표만 말하면 "뷰 열 개가 함께 들어온다"는
	// 사실이 적용한 뒤에야 보인다.
	ViewsAdded   []string `json:"viewsAdded,omitempty"`
	ViewsUpdated []string `json:"viewsUpdated,omitempty"`
	ViewsDropped []string `json:"viewsDropped,omitempty"`
}

// SummarizeImport는 불러오기 결과를 미리 계산한다.
func SummarizeImport(doc *Document, tables []*schema.Table, drops []string) *ImportSummary {
	return SummarizeImportAll(doc, tables, drops, nil, nil)
}

// SummarizeImportAll은 뷰까지 함께 센다.
//
// SummarizeImport 를 남겨 두는 이유: 이 함수는 시험과 다른 곳에서도 쓰인다. 인자
// 둘을 더해 모든 호출자를 고치는 것보다, 뷰를 세지 않는 옛 이름을 그대로 두는 편이
// "이 호출은 뷰를 세지 않는다"를 읽는 사람에게 분명히 남긴다.
func SummarizeImportAll(doc *Document, tables []*schema.Table, drops []string,
	views []*schema.View, viewDrops []string,
) *ImportSummary {
	out := &ImportSummary{Added: []string{}, Updated: []string{}, Dropped: []string{}}
	summarizeViews(doc, views, viewDrops, out)

	existing := map[string]*schema.Table{}
	for _, t := range doc.Schema.Tables {
		existing[t.Key()] = t
	}

	dropped := map[string]bool{}
	for _, raw := range drops {
		key := strings.ToLower(strings.TrimSpace(raw))
		t := existing[key]
		if t == nil {
			continue
		}
		dropped[key] = true
		out.Dropped = append(out.Dropped, t.Display())
	}

	incoming := map[string]bool{}
	for _, t := range tables {
		incoming[t.Key()] = true
	}
	for _, t := range tables {
		if old := existing[t.Key()]; old != nil && !dropped[t.Key()] {
			out.Updated = append(out.Updated, t.Display())
			continue
		}
		out.Added = append(out.Added, t.Display())
	}

	// 지워지는 테이블을 가리키던 외래키. 그 참조도 함께 사라진다.
	for _, t := range doc.Schema.Tables {
		if dropped[t.Key()] || incoming[t.Key()] {
			continue
		}
		for _, fk := range t.ForeignKeys {
			if dropped[fk.RefKey()] {
				out.DroppedRefs = append(out.DroppedRefs,
					fmt.Sprintf("%s.%s → %s", t.Display(), fk.Name, fk.RefTable))
			}
		}
	}

	// 참조 대상이 어디에도 없는 외래키.
	for _, t := range tables {
		for _, fk := range t.ForeignKeys {
			ref := fk.RefKey()
			if incoming[ref] || (existing[ref] != nil && !dropped[ref]) {
				continue
			}
			out.MissingRefs = append(out.MissingRefs,
				fmt.Sprintf("%s.%s → %s", t.Display(), fk.Name, fk.RefTable))
		}
	}
	return out
}

// mergeView는 불러온 뷰를 초안에 얹는다.
//
// 같은 이름이 있으면 정의를 덮어쓰고 **자리는 그대로 둔다**(표와 같은 규칙이다).
// 덮어쓰기는 대개 "같은 뷰의 새 버전"이고, 그때 카드가 화면 다른 곳으로 튀면
// 사람이 정리해 둔 배치를 처음부터 다시 만들어야 한다.
func mergeView(doc *Document, in *schema.View) error {
	key := in.Key()
	// 표와 뷰는 한 스키마에서 이름을 나눠 쓴다. 겹친 채로 담으면 그 초안이 만드는
	// DDL 은 대상 DB 가 거부한다 — 그 사실은 실행하는 순간에야 드러난다.
	if doc.findTable(key) != nil {
		return conflict("%s 은(는) 테이블 이름으로 이미 쓰이고 있어 뷰로 담을 수 없습니다", key)
	}
	for _, v := range doc.Schema.Views {
		if v.Key() != key {
			continue
		}
		v.Definition = in.Definition
		if in.Comment != "" {
			v.Comment = in.Comment
		}
		return nil
	}
	doc.Schema.Views = append(doc.Schema.Views, in)
	if _, ok := doc.Layout[key]; !ok {
		x, y := doc.nextFreeSlot()
		doc.Layout[key] = &Box{X: x, Y: y}
	}
	return nil
}

func dropViewByKey(doc *Document, key string) {
	for i, v := range doc.Schema.Views {
		if v.Key() == key {
			doc.Schema.Views = append(doc.Schema.Views[:i], doc.Schema.Views[i+1:]...)
			delete(doc.Layout, key)
			return
		}
	}
}

// summarizeViews는 뷰의 추가·갱신·삭제를 센다.
func summarizeViews(doc *Document, views []*schema.View, drops []string, out *ImportSummary) {
	existing := map[string]*schema.View{}
	for _, v := range doc.Schema.Views {
		existing[v.Key()] = v
	}
	dropped := map[string]bool{}
	for _, raw := range drops {
		key := strings.ToLower(strings.TrimSpace(raw))
		v := existing[key]
		if v == nil {
			continue
		}
		dropped[key] = true
		out.ViewsDropped = append(out.ViewsDropped, viewDisplay(v))
	}
	for _, v := range views {
		if v == nil {
			continue
		}
		if old := existing[v.Key()]; old != nil && !dropped[v.Key()] {
			out.ViewsUpdated = append(out.ViewsUpdated, viewDisplay(v))
			continue
		}
		out.ViewsAdded = append(out.ViewsAdded, viewDisplay(v))
	}
}

func viewDisplay(v *schema.View) string {
	if v.Namespace == "" {
		return v.Name
	}
	return v.Namespace + "." + v.Name
}
