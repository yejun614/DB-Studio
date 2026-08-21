package schema

import (
	"fmt"
	"strings"
)

// ChangeKind는 스키마 변경의 종류다.
type ChangeKind string

const (
	CreateTable ChangeKind = "create_table"
	DropTable   ChangeKind = "drop_table"
	AlterTable  ChangeKind = "alter_table_comment"
	AddColumn   ChangeKind = "add_column"
	DropColumn  ChangeKind = "drop_column"
	AlterColumn ChangeKind = "alter_column"
	AddPrimary  ChangeKind = "add_primary_key"
	DropPrimary ChangeKind = "drop_primary_key"
	AddIndex    ChangeKind = "add_index"
	DropIndex   ChangeKind = "drop_index"
	AddForeign  ChangeKind = "add_foreign_key"
	DropForeign ChangeKind = "drop_foreign_key"
	AddCheck    ChangeKind = "add_check"
	DropCheck   ChangeKind = "drop_check"
	CreateEnum  ChangeKind = "create_enum"
	DropEnum    ChangeKind = "drop_enum"
	AlterEnum   ChangeKind = "alter_enum"
	CreateView  ChangeKind = "create_view"
	DropView    ChangeKind = "drop_view"
	ReplaceView ChangeKind = "replace_view"
)

// Change는 스키마 변경 한 건이다.
// From/To는 변경 대상의 이전/이후 상태이며, DDL 렌더러가 이 값으로 SQL을 만든다.
type Change struct {
	Kind    ChangeKind `json:"kind"`
	Table   string     `json:"table,omitempty"`  // 표시용 이름 (namespace 포함)
	Object  string     `json:"object,omitempty"` // 컬럼/인덱스/제약 이름
	Summary string     `json:"summary"`          // 사람이 읽는 한 줄 설명

	// Destructive는 데이터 손실이나 되돌릴 수 없는 결과를 낼 수 있는 변경을 뜻한다.
	// UI는 이 값으로 경고를 띄우고, 운영 DB에서는 추가 승인을 요구한다.
	Destructive bool `json:"destructive"`
	// LossyDetail은 파괴적인 이유를 설명한다.
	LossyDetail string `json:"lossyDetail,omitempty"`

	// 아래 필드는 DDL 렌더링에 필요한 구조체를 담는다. 종류에 따라 일부만 채워진다.
	TableRef   *Table      `json:"-"`
	OldTable   *Table      `json:"-"`
	Column     *Column     `json:"-"`
	OldColumn  *Column     `json:"-"`
	Index      *Index      `json:"-"`
	OldIndex   *Index      `json:"-"`
	ForeignKey *ForeignKey `json:"-"`
	OldFK      *ForeignKey `json:"-"`
	CheckRef   *Check      `json:"-"`
	OldCheck   *Check      `json:"-"`
	PrimaryKey *PrimaryKey `json:"-"`
	OldPrimary *PrimaryKey `json:"-"`
	EnumRef    *Enum       `json:"-"`
	OldEnum    *Enum       `json:"-"`
	ViewRef    *View       `json:"-"`

	// Attrs는 alter_column에서 무엇이 바뀌었는지 같은 세부 정보다.
	Attrs map[string]string `json:"attrs,omitempty"`
}

// DiffResult는 diff 전체 결과다.
type DiffResult struct {
	Changes []Change `json:"changes"`
	// Summary는 종류별 건수다.
	Counts map[ChangeKind]int `json:"counts"`
	// DestructiveCount는 파괴적 변경 건수다.
	DestructiveCount int `json:"destructiveCount"`
	// Unsupported는 diff로 표현할 수 없어 사람이 처리해야 하는 항목이다.
	Unsupported []string `json:"unsupported,omitempty"`
}

func (r *DiffResult) IsEmpty() bool { return len(r.Changes) == 0 }

// Diff는 from 스키마를 to 스키마로 만들기 위한 변경 목록을 계산한다.
//
// 매칭은 이름 기준이다. 이름 변경(rename)은 삭제 + 생성으로 나타난다 —
// 스키마 두 장만 보고 rename을 추론하면 잘못 짚었을 때 데이터가 사라지므로
// 의도적으로 추론하지 않는다. rename은 ERD 편집 이력(P6~P7)에서 op로 전달한다.
func Diff(from, to *Schema) *DiffResult {
	res := &DiffResult{Changes: []Change{}, Counts: map[ChangeKind]int{}}
	if from == nil || to == nil {
		return res
	}
	from.Sort()
	to.Sort()

	// 관계형이 아닌 스키마는 DDL 개념이 없으므로 비교 대상에서 제외한다.
	if from.Shape != ShapeRelational || to.Shape != ShapeRelational {
		res.Unsupported = append(res.Unsupported,
			fmt.Sprintf("%s ↔ %s 형태의 스키마는 DDL 비교를 지원하지 않습니다", from.Shape, to.Shape))
		return res
	}

	diffEnums(res, from, to)
	diffTables(res, from, to)
	diffViews(res, from, to)

	for _, c := range res.Changes {
		res.Counts[c.Kind]++
		if c.Destructive {
			res.DestructiveCount++
		}
	}
	return res
}

func (r *DiffResult) add(c Change) { r.Changes = append(r.Changes, c) }

func diffTables(res *DiffResult, from, to *Schema) {
	fromMap, toMap := from.TableMap(), to.TableMap()

	// 삭제: 데이터가 사라지므로 항상 파괴적이다.
	for _, ft := range from.Tables {
		if _, ok := toMap[ft.Key()]; !ok {
			res.add(Change{
				Kind: DropTable, Table: ft.Display(), OldTable: ft,
				Summary:     fmt.Sprintf("테이블 %s 삭제", ft.Display()),
				Destructive: true,
				LossyDetail: fmt.Sprintf("테이블의 모든 데이터가 삭제됩니다 (약 %d행)", ft.RowEstimate),
			})
		}
	}

	// 생성
	for _, tt := range to.Tables {
		if _, ok := fromMap[tt.Key()]; !ok {
			res.add(Change{
				Kind: CreateTable, Table: tt.Display(), TableRef: tt,
				Summary: fmt.Sprintf("테이블 %s 생성 (컬럼 %d개)", tt.Display(), len(tt.Columns)),
			})
			// 새 테이블의 FK는 참조 대상이 모두 생성된 뒤에 붙여야 하므로 별도 변경으로 낸다.
			for _, fk := range tt.ForeignKeys {
				res.add(Change{
					Kind: AddForeign, Table: tt.Display(), Object: fk.Name,
					TableRef: tt, ForeignKey: fk,
					Summary: fmt.Sprintf("%s.%s → %s 외래키 추가",
						tt.Display(), strings.Join(fk.Columns, ","), fk.RefTable),
				})
			}
			continue
		}
	}

	// 양쪽에 있는 테이블 비교
	for _, tt := range to.Tables {
		ft, ok := fromMap[tt.Key()]
		if !ok {
			continue
		}
		diffTable(res, ft, tt)
	}
}

func diffTable(res *DiffResult, ft, tt *Table) {
	if ft.Comment != tt.Comment {
		res.add(Change{
			Kind: AlterTable, Table: tt.Display(), TableRef: tt, OldTable: ft,
			Summary: fmt.Sprintf("테이블 %s 주석 변경", tt.Display()),
			Attrs:   map[string]string{"comment": tt.Comment, "oldComment": ft.Comment},
		})
	}

	fromCols := columnMap(ft)
	toCols := columnMap(tt)

	for _, fc := range ft.Columns {
		if _, ok := toCols[strings.ToLower(fc.Name)]; !ok {
			res.add(Change{
				Kind: DropColumn, Table: tt.Display(), Object: fc.Name,
				TableRef: tt, OldTable: ft, OldColumn: fc,
				Summary:     fmt.Sprintf("%s.%s 컬럼 삭제", tt.Display(), fc.Name),
				Destructive: true,
				LossyDetail: "컬럼의 모든 값이 삭제됩니다",
			})
		}
	}

	for _, tc := range tt.Columns {
		fc, ok := fromCols[strings.ToLower(tc.Name)]
		if !ok {
			// NOT NULL 컬럼을 기본값 없이 추가하면 기존 행이 있을 때 실패한다.
			destructive := !tc.Nullable && !tc.HasDefault && !tc.Identity
			detail := ""
			if destructive {
				detail = "기존 행이 있으면 NOT NULL 제약 위반으로 실패합니다. 기본값을 지정하거나 단계적으로 적용하세요"
			}
			res.add(Change{
				Kind: AddColumn, Table: tt.Display(), Object: tc.Name,
				TableRef: tt, Column: tc,
				Summary:     fmt.Sprintf("%s.%s 컬럼 추가 (%s)", tt.Display(), tc.Name, tc.Type.Canonical()),
				Destructive: destructive,
				LossyDetail: detail,
			})
			continue
		}
		diffColumn(res, tt, ft, fc, tc)
	}

	diffPrimaryKey(res, ft, tt)
	diffIndexes(res, ft, tt)
	diffForeignKeys(res, ft, tt)
	diffChecks(res, ft, tt)
}

func diffColumn(res *DiffResult, tt, ft *Table, fc, tc *Column) {
	attrs := map[string]string{}
	destructive := false
	var reasons []string

	if !fc.Type.Equal(tc.Type) {
		attrs["type"] = tc.Type.Canonical()
		attrs["oldType"] = fc.Type.Canonical()
		if !fc.Type.IsWidening(tc.Type) {
			destructive = true
			reasons = append(reasons, fmt.Sprintf("타입 축소/변환 (%s → %s)", fc.Type.Canonical(), tc.Type.Canonical()))
		}
	}
	if fc.Nullable != tc.Nullable {
		attrs["nullable"] = fmt.Sprint(tc.Nullable)
		attrs["oldNullable"] = fmt.Sprint(fc.Nullable)
		if !tc.Nullable {
			destructive = true
			reasons = append(reasons, "NULL 허용 → NOT NULL (기존 NULL 값이 있으면 실패)")
		}
	}
	// 기본값은 dialect 표기 차이를 흡수한 뒤 비교한다.
	fd := NormalizeDefault("", fc.Default)
	td := NormalizeDefault("", tc.Default)
	if fc.HasDefault != tc.HasDefault || fd != td {
		attrs["default"] = tc.Default
		attrs["oldDefault"] = fc.Default
		attrs["hasDefault"] = fmt.Sprint(tc.HasDefault)
	}
	if fc.Comment != tc.Comment {
		attrs["comment"] = tc.Comment
		attrs["oldComment"] = fc.Comment
	}
	if fc.Identity != tc.Identity {
		attrs["identity"] = fmt.Sprint(tc.Identity)
		attrs["oldIdentity"] = fmt.Sprint(fc.Identity)
	}
	if fc.Generated != tc.Generated {
		attrs["generated"] = tc.Generated
		attrs["oldGenerated"] = fc.Generated
	}
	if len(attrs) == 0 {
		return
	}

	res.add(Change{
		Kind: AlterColumn, Table: tt.Display(), Object: tc.Name,
		TableRef: tt, OldTable: ft, Column: tc, OldColumn: fc,
		Summary:     fmt.Sprintf("%s.%s 컬럼 변경 (%s)", tt.Display(), tc.Name, strings.Join(changedAttrLabels(attrs), ", ")),
		Destructive: destructive,
		LossyDetail: strings.Join(reasons, "; "),
		Attrs:       attrs,
	})
}

func changedAttrLabels(attrs map[string]string) []string {
	labels := map[string]string{
		"type": "타입", "nullable": "NULL 허용", "default": "기본값",
		"comment": "주석", "identity": "자동증가", "generated": "생성 컬럼",
	}
	// 표시 순서를 고정해 같은 변경이 매번 같은 문구로 보이게 한다.
	order := []string{"type", "nullable", "default", "comment", "identity", "generated"}
	out := []string{}
	for _, k := range order {
		if _, ok := attrs[k]; ok {
			out = append(out, labels[k])
		}
	}
	return out
}

func diffPrimaryKey(res *DiffResult, ft, tt *Table) {
	fp, tp := ft.PrimaryKey, tt.PrimaryKey
	same := (fp == nil && tp == nil) ||
		(fp != nil && tp != nil && equalFoldSlice(fp.Columns, tp.Columns))
	if same {
		return
	}
	if fp != nil {
		res.add(Change{
			Kind: DropPrimary, Table: tt.Display(), Object: fp.Name,
			TableRef: tt, OldPrimary: fp,
			Summary:     fmt.Sprintf("%s 기본키 삭제 (%s)", tt.Display(), strings.Join(fp.Columns, ",")),
			Destructive: true,
			LossyDetail: "기본키 삭제는 참조 무결성과 복제에 영향을 줄 수 있습니다",
		})
	}
	if tp != nil {
		res.add(Change{
			Kind: AddPrimary, Table: tt.Display(), Object: tp.Name,
			TableRef: tt, PrimaryKey: tp,
			Summary: fmt.Sprintf("%s 기본키 추가 (%s)", tt.Display(), strings.Join(tp.Columns, ",")),
		})
	}
}

func diffIndexes(res *DiffResult, ft, tt *Table) {
	fromIdx := map[string]*Index{}
	for _, i := range ft.Indexes {
		fromIdx[strings.ToLower(i.Name)] = i
	}
	toIdx := map[string]*Index{}
	for _, i := range tt.Indexes {
		toIdx[strings.ToLower(i.Name)] = i
	}

	for _, fi := range ft.Indexes {
		ti, ok := toIdx[strings.ToLower(fi.Name)]
		if !ok {
			res.add(Change{
				Kind: DropIndex, Table: tt.Display(), Object: fi.Name,
				TableRef: tt, OldIndex: fi,
				Summary: fmt.Sprintf("%s 인덱스 %s 삭제", tt.Display(), fi.Name),
			})
			continue
		}
		// 인덱스 정의 변경은 ALTER가 없으므로 삭제 후 재생성으로 표현한다.
		if !sameIndex(fi, ti) {
			res.add(Change{
				Kind: DropIndex, Table: tt.Display(), Object: fi.Name,
				TableRef: tt, OldIndex: fi,
				Summary: fmt.Sprintf("%s 인덱스 %s 재생성 (삭제)", tt.Display(), fi.Name),
			})
			res.add(Change{
				Kind: AddIndex, Table: tt.Display(), Object: ti.Name,
				TableRef: tt, Index: ti, OldIndex: fi,
				Summary: fmt.Sprintf("%s 인덱스 %s 재생성 (추가)", tt.Display(), ti.Name),
			})
		}
	}
	for _, ti := range tt.Indexes {
		if _, ok := fromIdx[strings.ToLower(ti.Name)]; !ok {
			res.add(Change{
				Kind: AddIndex, Table: tt.Display(), Object: ti.Name,
				TableRef: tt, Index: ti,
				Summary: fmt.Sprintf("%s 인덱스 %s 추가 (%s)", tt.Display(), ti.Name,
					strings.Join(ti.ColumnNames(), ",")),
			})
		}
	}
}

func sameIndex(a, b *Index) bool {
	if a.Unique != b.Unique || normalizeExpr(a.Where) != normalizeExpr(b.Where) {
		return false
	}
	if len(a.Columns) != len(b.Columns) {
		return false
	}
	for i := range a.Columns {
		if !strings.EqualFold(a.Columns[i].Column, b.Columns[i].Column) ||
			a.Columns[i].Descending != b.Columns[i].Descending ||
			normalizeExpr(a.Columns[i].Expression) != normalizeExpr(b.Columns[i].Expression) {
			return false
		}
	}
	// 인덱스 방식(btree/hash 등)은 DB가 기본값을 채워 보고하므로
	// 한쪽이 비어 있으면 같다고 본다.
	if a.Type != "" && b.Type != "" && !strings.EqualFold(a.Type, b.Type) {
		return false
	}
	return true
}

func diffForeignKeys(res *DiffResult, ft, tt *Table) {
	fromFK := map[string]*ForeignKey{}
	for _, f := range ft.ForeignKeys {
		fromFK[strings.ToLower(f.Name)] = f
	}
	toFK := map[string]*ForeignKey{}
	for _, f := range tt.ForeignKeys {
		toFK[strings.ToLower(f.Name)] = f
	}

	for _, ff := range ft.ForeignKeys {
		tf, ok := toFK[strings.ToLower(ff.Name)]
		if !ok || !sameFK(ff, tf) {
			res.add(Change{
				Kind: DropForeign, Table: tt.Display(), Object: ff.Name,
				TableRef: tt, OldFK: ff,
				Summary: fmt.Sprintf("%s 외래키 %s 삭제", tt.Display(), ff.Name),
			})
		}
	}
	for _, tf := range tt.ForeignKeys {
		ff, ok := fromFK[strings.ToLower(tf.Name)]
		if !ok || !sameFK(ff, tf) {
			res.add(Change{
				Kind: AddForeign, Table: tt.Display(), Object: tf.Name,
				TableRef: tt, ForeignKey: tf,
				Summary: fmt.Sprintf("%s 외래키 %s 추가 (%s → %s.%s)", tt.Display(), tf.Name,
					strings.Join(tf.Columns, ","), tf.RefTable, strings.Join(tf.RefColumns, ",")),
			})
		}
	}
}

func sameFK(a, b *ForeignKey) bool {
	return equalFoldSlice(a.Columns, b.Columns) &&
		a.RefKey() == b.RefKey() &&
		equalFoldSlice(a.RefColumns, b.RefColumns) &&
		normalizeAction(a.OnDelete) == normalizeAction(b.OnDelete) &&
		normalizeAction(a.OnUpdate) == normalizeAction(b.OnUpdate)
}

// normalizeAction은 참조 동작 표기를 통일한다.
// 빈 값과 NO ACTION, RESTRICT는 대부분의 DB에서 동일하게 동작한다.
func normalizeAction(s string) string {
	v := strings.ToUpper(strings.TrimSpace(s))
	v = strings.ReplaceAll(v, "_", " ")
	switch v {
	case "", "NO ACTION", "RESTRICT":
		return "NO ACTION"
	}
	return v
}

func diffChecks(res *DiffResult, ft, tt *Table) {
	fromCk := map[string]*Check{}
	for _, c := range ft.Checks {
		fromCk[strings.ToLower(c.Name)] = c
	}
	toCk := map[string]*Check{}
	for _, c := range tt.Checks {
		toCk[strings.ToLower(c.Name)] = c
	}

	for _, fc := range ft.Checks {
		tc, ok := toCk[strings.ToLower(fc.Name)]
		if !ok || normalizeExpr(fc.Expression) != normalizeExpr(tc.Expression) {
			res.add(Change{
				Kind: DropCheck, Table: tt.Display(), Object: fc.Name,
				TableRef: tt, OldCheck: fc,
				Summary: fmt.Sprintf("%s 체크 제약 %s 삭제", tt.Display(), fc.Name),
			})
		}
	}
	for _, tc := range tt.Checks {
		fc, ok := fromCk[strings.ToLower(tc.Name)]
		if !ok || normalizeExpr(fc.Expression) != normalizeExpr(tc.Expression) {
			res.add(Change{
				Kind: AddCheck, Table: tt.Display(), Object: tc.Name,
				TableRef: tt, CheckRef: tc,
				Summary:     fmt.Sprintf("%s 체크 제약 %s 추가", tt.Display(), tc.Name),
				Destructive: true,
				LossyDetail: "기존 데이터가 제약을 위반하면 적용이 실패합니다",
			})
		}
	}
}

func diffEnums(res *DiffResult, from, to *Schema) {
	fromEnums := map[string]*Enum{}
	for _, e := range from.Enums {
		fromEnums[e.Key()] = e
	}
	toEnums := map[string]*Enum{}
	for _, e := range to.Enums {
		toEnums[e.Key()] = e
	}

	for _, fe := range from.Enums {
		if _, ok := toEnums[fe.Key()]; !ok {
			res.add(Change{
				Kind: DropEnum, Object: fe.Name, OldEnum: fe,
				Summary:     fmt.Sprintf("enum 타입 %s 삭제", fe.Name),
				Destructive: true,
				LossyDetail: "이 타입을 쓰는 컬럼이 남아 있으면 실패합니다",
			})
		}
	}
	for _, te := range to.Enums {
		fe, ok := fromEnums[te.Key()]
		if !ok {
			res.add(Change{
				Kind: CreateEnum, Object: te.Name, EnumRef: te,
				Summary: fmt.Sprintf("enum 타입 %s 생성 (%s)", te.Name, strings.Join(te.Values, ", ")),
			})
			continue
		}
		if strings.Join(fe.Values, ",") == strings.Join(te.Values, ",") {
			continue
		}
		// 값 추가는 안전하지만 삭제/재정렬은 안전하지 않다.
		removed := missing(fe.Values, te.Values)
		res.add(Change{
			Kind: AlterEnum, Object: te.Name, EnumRef: te, OldEnum: fe,
			Summary: fmt.Sprintf("enum 타입 %s 값 변경 (%s → %s)", te.Name,
				strings.Join(fe.Values, ","), strings.Join(te.Values, ",")),
			Destructive: len(removed) > 0,
			LossyDetail: lossyEnumDetail(removed),
		})
	}
}

func lossyEnumDetail(removed []string) string {
	if len(removed) == 0 {
		return ""
	}
	return fmt.Sprintf("제거된 값(%s)을 쓰는 행이 있으면 실패합니다", strings.Join(removed, ", "))
}

func diffViews(res *DiffResult, from, to *Schema) {
	fromViews := map[string]*View{}
	for _, v := range from.Views {
		fromViews[v.Key()] = v
	}
	toViews := map[string]*View{}
	for _, v := range to.Views {
		toViews[v.Key()] = v
	}

	for _, fv := range from.Views {
		if _, ok := toViews[fv.Key()]; !ok {
			res.add(Change{
				Kind: DropView, Object: fv.Name, ViewRef: fv,
				Summary: fmt.Sprintf("뷰 %s 삭제", fv.Key()),
			})
		}
	}
	for _, tv := range to.Views {
		fv, ok := fromViews[tv.Key()]
		if !ok {
			res.add(Change{
				Kind: CreateView, Object: tv.Name, ViewRef: tv,
				Summary: fmt.Sprintf("뷰 %s 생성", tv.Key()),
			})
			continue
		}
		if normalizeExpr(fv.Definition) != normalizeExpr(tv.Definition) {
			res.add(Change{
				Kind: ReplaceView, Object: tv.Name, ViewRef: tv,
				Summary: fmt.Sprintf("뷰 %s 정의 변경", tv.Key()),
			})
		}
	}
}

// ---------- 유틸 ----------

func columnMap(t *Table) map[string]*Column {
	m := make(map[string]*Column, len(t.Columns))
	for _, c := range t.Columns {
		m[strings.ToLower(c.Name)] = c
	}
	return m
}

func equalFoldSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !strings.EqualFold(a[i], b[i]) {
			return false
		}
	}
	return true
}

// missing은 a에는 있고 b에는 없는 값을 반환한다.
func missing(a, b []string) []string {
	set := make(map[string]bool, len(b))
	for _, v := range b {
		set[v] = true
	}
	out := []string{}
	for _, v := range a {
		if !set[v] {
			out = append(out, v)
		}
	}
	return out
}
