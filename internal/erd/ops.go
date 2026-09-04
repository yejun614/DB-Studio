package erd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"dbstudio/internal/schema"
)

// Kind는 편집 연산의 종류다.
type Kind string

const (
	OpTableAdd    Kind = "table.add"
	OpTableUpdate Kind = "table.update" // 이름·주석·네임스페이스 패치
	OpTableMove   Kind = "table.move"   // 레이아웃 전용 (구조 변경 아님)
	OpTableDelete Kind = "table.delete"
	// OpTableDuplicate는 테이블을 통째로 베낀다.
	//
	// 컬럼을 하나씩 다시 만들게 하지 않고 op 하나로 두는 이유가 셋 있다.
	// 첫째, 스무 컬럼짜리 테이블을 베끼는 일이 스무 번의 편집으로 남으면 편집
	// 이력과 되돌리기가 그 하나의 동작을 스무 조각으로 보여준다. 둘째, 중간에
	// 하나가 거부되면 반쪽만 만들어진 테이블이 남는다. 셋째, 제약 이름을 새 이름에
	// 맞춰 바꾸는 규칙이 클라이언트마다 따로 구현되면 언젠가 어긋난다.
	OpTableDuplicate Kind = "table.duplicate"

	OpColumnAdd    Kind = "column.add"
	OpColumnUpdate Kind = "column.update"
	OpColumnDelete Kind = "column.delete"
	// OpColumnMove는 테이블 안에서 컬럼 순서를 바꾼다.
	//
	// 순서는 구조다. 생성될 DDL의 컬럼 순서가 바뀌고, 복합 인덱스나 기본키를 만들 때
	// 사람이 보는 순서가 곧 기준이 된다. 그래서 레이아웃이 아니라 구조 op다.
	OpColumnMove Kind = "column.move"

	OpPKSet Kind = "pk.set"

	OpIndexAdd    Kind = "index.add"
	OpIndexUpdate Kind = "index.update"
	OpIndexDelete Kind = "index.delete"

	OpFKAdd    Kind = "fk.add"
	OpFKUpdate Kind = "fk.update"
	OpFKDelete Kind = "fk.delete"

	OpCheckAdd    Kind = "check.add"
	OpCheckDelete Kind = "check.delete"

	OpEnumAdd    Kind = "enum.add"
	OpEnumUpdate Kind = "enum.update"
	OpEnumDelete Kind = "enum.delete"

	// 도메인은 재사용하는 타입 정의다. 구조 op로 두는 이유: 도메인을 고치면 그것을
	// 쓰는 컬럼의 타입이 함께 바뀌고, 그 변화는 생성될 DDL에 그대로 나타난다.
	OpDomainAdd    Kind = "domain.add"
	OpDomainUpdate Kind = "domain.update"
	OpDomainDelete Kind = "domain.delete"

	// 뷰는 표와 함께 도면에 놓인다. view.move 는 좌표만 바꾸는 op 다(구조가 아니다).
	OpViewAdd    Kind = "view.add"
	OpViewUpdate Kind = "view.update"
	OpViewDelete Kind = "view.delete"
	OpViewMove   Kind = "view.move"

	OpNoteAdd    Kind = "note.add"
	OpNoteUpdate Kind = "note.update"
	OpNoteDelete Kind = "note.delete"

	OpGroupAdd    Kind = "group.add"
	OpGroupUpdate Kind = "group.update"
	OpGroupDelete Kind = "group.delete"

	// OpSchemaImport는 SQL 스크립트를 읽어 초안에 얹는다.
	// 여러 테이블을 한 번에 바꾸므로 op 하나여야 한다 — 자세한 이유는 import.go.
	// OpDocOptions는 문서 수준 설정이다: 표 기본 저장 설정, 만들 데이터베이스.
	OpDocOptions Kind = "doc.options"

	OpSchemaImport Kind = "schema.import"
)

// Op는 하나의 편집 연산이다.
type Op struct {
	// ID는 클라이언트가 만든 식별자다. 브로드캐스트된 op가 자기 것인지 알아보고
	// 낙관적 적용과 정합할 때 쓴다.
	ID   string `json:"id"`
	Kind Kind   `json:"kind"`
	// Payload는 op 종류별 인자다. 갱신 계열은 패치(없는 필드 = 변경 없음)다.
	Payload json.RawMessage `json:"payload"`

	// 이하는 서버가 채운다.
	Seq       int64     `json:"seq,omitempty"`
	Actor     string    `json:"actor,omitempty"`
	ActorName string    `json:"actorName,omitempty"`
	At        time.Time `json:"at,omitempty"`
	// BaseSeq는 클라이언트가 이 op를 만들 때 본 마지막 seq다. 서버 권위 모델에서
	// 적용 순서를 바꾸지는 않지만, 충돌 원인을 사후에 추적할 때 필요하다.
	BaseSeq int64 `json:"baseSeq,omitempty"`
	// Batch는 "한 동작에서 나온 편집"을 묶는다. 되돌리기가 함께 되돌린다.
	//
	// 필요한 이유: 여러 개를 골라 한 번에 끌면 대상마다 op가 하나씩 생긴다. 묶지
	// 않으면 Ctrl+Z가 카드를 하나씩 되돌려, 한 번 옮긴 것을 열 번 되돌려야 한다 —
	// 사람이 보기에 그것은 되돌리기가 아니라 고장이다.
	//
	// 적용 순서에는 관여하지 않는다. op는 여전히 하나씩 검증되고 하나씩 적용된다.
	Batch string `json:"batch,omitempty"`
}

// StructuralKinds는 실제 스키마 구조를 바꾸는 op다.
// 레이아웃·메모 op는 마이그레이션과 무관하므로 스냅샷 지문에 영향을 주지 않는다.
func (k Kind) Structural() bool {
	switch k {
	case OpTableMove, OpViewMove, OpNoteAdd, OpNoteUpdate, OpNoteDelete,
		OpGroupAdd, OpGroupUpdate, OpGroupDelete:
		return false
	}
	return true
}

// Error는 op 거부 사유다.
type Error struct {
	// Code는 클라이언트가 대응을 결정하는 기계용 값이다.
	//   invalid   — 입력이 잘못됐다. 사용자에게 고치라고 알린다.
	//   conflict  — 다른 사람의 편집과 어긋났다. 최신 상태로 재동기화해야 한다.
	//   not_found — 대상이 이미 사라졌다. 역시 재동기화 대상이다.
	Code   string
	Reason string
}

func (e *Error) Error() string { return e.Reason }

func invalid(format string, args ...any) *Error {
	return &Error{Code: "invalid", Reason: fmt.Sprintf(format, args...)}
}

func conflict(format string, args ...any) *Error {
	return &Error{Code: "conflict", Reason: fmt.Sprintf(format, args...)}
}

func notFound(format string, args ...any) *Error {
	return &Error{Code: "not_found", Reason: fmt.Sprintf(format, args...)}
}

// Apply는 op를 문서에 적용한다.
//
// 호출자는 사본에 적용해야 한다. 검증은 적용 도중에도 실패할 수 있고, 그때 원본이
// 부분 변경된 상태로 남으면 이후 모든 op가 잘못된 기준 위에서 동작한다.
func Apply(doc *Document, op *Op) error {
	if doc == nil || doc.Schema == nil {
		return invalid("문서가 초기화되지 않았습니다")
	}
	switch op.Kind {
	case OpTableAdd:
		return applyTableAdd(doc, op)
	case OpTableUpdate:
		return applyTableUpdate(doc, op)
	case OpTableMove:
		return applyTableMove(doc, op)
	case OpTableDelete:
		return applyTableDelete(doc, op)
	case OpTableDuplicate:
		return applyTableDuplicate(doc, op)
	case OpColumnAdd:
		return applyColumnAdd(doc, op)
	case OpColumnUpdate:
		return applyColumnUpdate(doc, op)
	case OpColumnDelete:
		return applyColumnDelete(doc, op)
	case OpColumnMove:
		return applyColumnMove(doc, op)
	case OpPKSet:
		return applyPKSet(doc, op)
	case OpIndexAdd, OpIndexUpdate:
		return applyIndexUpsert(doc, op)
	case OpIndexDelete:
		return applyIndexDelete(doc, op)
	case OpFKAdd, OpFKUpdate:
		return applyFKUpsert(doc, op)
	case OpFKDelete:
		return applyFKDelete(doc, op)
	case OpCheckAdd:
		return applyCheckAdd(doc, op)
	case OpCheckDelete:
		return applyCheckDelete(doc, op)
	case OpDomainAdd:
		return applyDomainAdd(doc, op)
	case OpDomainUpdate:
		return applyDomainUpdate(doc, op)
	case OpDomainDelete:
		return applyDomainDelete(doc, op)
	case OpEnumAdd, OpEnumUpdate:
		return applyEnumUpsert(doc, op)
	case OpEnumDelete:
		return applyEnumDelete(doc, op)
	case OpViewAdd:
		return applyViewAdd(doc, op)
	case OpViewUpdate:
		return applyViewUpdate(doc, op)
	case OpViewDelete:
		return applyViewDelete(doc, op)
	case OpViewMove:
		return applyViewMove(doc, op)
	case OpNoteAdd:
		return applyNoteAdd(doc, op)
	case OpNoteUpdate:
		return applyNoteUpdate(doc, op)
	case OpNoteDelete:
		return applyNoteDelete(doc, op)
	case OpGroupAdd:
		return applyGroupAdd(doc, op)
	case OpGroupUpdate:
		return applyGroupUpdate(doc, op)
	case OpGroupDelete:
		return applyGroupDelete(doc, op)
	case OpDocOptions:
		return applyDocOptions(doc, op)
	case OpSchemaImport:
		return applySchemaImport(doc, op)
	case OpRestore:
		return applyRestore(doc, op)
	}
	return invalid("알 수 없는 연산입니다: %s", op.Kind)
}

// decode는 payload를 엄격하게 디코딩한다.
//
// DisallowUnknownFields를 쓰는 이유: 패치 op에서 필드 이름을 틀리면(nullible 등)
// 관대한 디코더는 그 필드를 조용히 버리고 "변경 없음"으로 처리한다. 그러면 사용자
// 입장에서는 편집이 아무 오류 없이 사라진다. 프론트엔드가 바이너리에 함께 담겨
// 배포되므로 버전 불일치가 생길 수 없어, 엄격한 쪽이 손해가 없다.
func decode(op *Op, dst any) error {
	if len(bytes.TrimSpace(op.Payload)) == 0 {
		return invalid("%s 연산에 인자가 없습니다", op.Kind)
	}
	dec := json.NewDecoder(bytes.NewReader(op.Payload))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return invalid("%s 연산의 인자가 올바르지 않습니다: %v", op.Kind, err)
	}
	return nil
}

// ---------- 식별자 검증 ----------

// maxIdentLen은 대상 DB들의 식별자 길이 제한 중 가장 짧은 값(Oracle 12.2+ 128)에 맞춘다.
const maxIdentLen = 128

// badIdentChars는 식별자에 허용하지 않는 문자다.
//
// 인용 문자를 막는 것은 보안 문제다. DDL 생성기는 dialect별 인용부호로 식별자를
// 감싸는데, 이름 안에 그 인용부호가 들어 있으면 생성된 SQL의 구조가 바뀐다
// (`users"; DROP TABLE x; --` 같은 이름). ERD를 그리는 사람과 마이그레이션을
// 실행하는 사람이 다를 수 있으므로 여기서 막는 것이 맞다.
const badIdentChars = "\"`'[]\x00\n\r\t;"

func validateIdent(what, name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", invalid("%s 이름이 비어 있습니다", what)
	}
	if len([]rune(name)) > maxIdentLen {
		return "", invalid("%s 이름이 너무 깁니다 (%d자 제한)", what, maxIdentLen)
	}
	if i := strings.IndexAny(name, badIdentChars); i >= 0 {
		// badIdentChars는 전부 ASCII이므로 바이트 하나가 곧 그 문자다.
		return "", invalid("%s 이름에 쓸 수 없는 문자가 있습니다: %q", what, string(name[i]))
	}
	return name, nil
}

// ---------- 테이블 ----------

type tableAddPayload struct {
	Name      string   `json:"name"`
	Namespace string   `json:"namespace,omitempty"`
	Comment   string   `json:"comment,omitempty"`
	X         *float64 `json:"x,omitempty"`
	Y         *float64 `json:"y,omitempty"`
	// WithID는 "id 기본키 컬럼을 함께 만든다"는 편의 플래그다.
	// 빈 테이블을 만들고 컬럼을 따로 추가하는 왕복을 줄인다.
	WithID bool `json:"withId,omitempty"`
}

func applyTableAdd(doc *Document, op *Op) error {
	var p tableAddPayload
	if err := decode(op, &p); err != nil {
		return err
	}
	name, err := validateIdent("테이블", p.Name)
	if err != nil {
		return err
	}
	ns := strings.TrimSpace(p.Namespace)
	if ns != "" {
		if ns, err = validateIdent("네임스페이스", ns); err != nil {
			return err
		}
	}

	tbl := &schema.Table{
		Namespace: ns, Name: name, Comment: strings.TrimSpace(p.Comment),
		Columns: []*schema.Column{}, Indexes: []*schema.Index{},
		ForeignKeys: []*schema.ForeignKey{}, Checks: []*schema.Check{},
	}
	if doc.findTable(tbl.Key()) != nil {
		return conflict("테이블 %s 이(가) 이미 있습니다", tbl.Display())
	}
	if p.WithID {
		tbl.Columns = append(tbl.Columns, &schema.Column{
			Name: "id", Position: 1, Identity: true,
			Type: schema.LogicalType{Base: schema.TypeBigInt}, RawType: "bigint",
		})
		tbl.PrimaryKey = &schema.PrimaryKey{Columns: []string{"id"}}
	}

	applyTableDefaults(doc, tbl)
	doc.Schema.Tables = append(doc.Schema.Tables, tbl)
	x, y := doc.nextFreeSlot()
	if p.X != nil {
		x = *p.X
	}
	if p.Y != nil {
		y = *p.Y
	}
	doc.Layout[tbl.Key()] = &Box{X: x, Y: y}
	return nil
}

type tableDuplicatePayload struct {
	// Key는 베낄 원본이다.
	Key string `json:"key"`
	// Name이 비어 있으면 "원본이름_copy" 로 정한다(겹치면 뒤에 번호).
	Name      string   `json:"name"`
	Namespace *string  `json:"namespace,omitempty"`
	X         *float64 `json:"x,omitempty"`
	Y         *float64 `json:"y,omitempty"`
	// 함께 베낄 것들. 지정하지 않으면 모두 베낀다 — "복제"라는 말에 가장 가깝다.
	WithIndexes *bool `json:"withIndexes,omitempty"`
	WithFKs     *bool `json:"withForeignKeys,omitempty"`
	WithChecks  *bool `json:"withChecks,omitempty"`
}

// applyTableDuplicate는 테이블을 베껴 새 테이블을 만든다.
//
// 베끼지 않는 것: 이 테이블을 **가리키는** 외래키(다른 테이블에 있다). 그것을 함께
// 베끼면 남의 테이블을 고치는 일이 되고, 사본과 원본 중 어느 쪽을 가리켜야 하는지도
// 사람만 안다. 통계값(행 수·크기)도 베끼지 않는다 — 사본에는 아직 행이 없다.
func applyTableDuplicate(doc *Document, op *Op) error {
	var p tableDuplicatePayload
	if err := decode(op, &p); err != nil {
		return err
	}
	src := doc.findTable(p.Key)
	if src == nil {
		return notFound("테이블 %s 을(를) 찾을 수 없습니다", p.Key)
	}

	ns := src.Namespace
	if p.Namespace != nil {
		ns = strings.TrimSpace(*p.Namespace)
		if ns != "" {
			var err error
			if ns, err = validateIdent("네임스페이스", ns); err != nil {
				return err
			}
		}
	}

	name := strings.TrimSpace(p.Name)
	if name == "" {
		name = freeTableName(doc, src.Name, ns)
	}
	name, err := validateIdent("테이블", name)
	if err != nil {
		return err
	}

	dst := &schema.Table{
		Namespace: ns, Name: name, Comment: src.Comment,
		Columns: make([]*schema.Column, 0, len(src.Columns)),
		// 아래에서 채운다. nil 로 두면 화면과 DDL 쪽에서 매번 nil 검사를 해야 한다.
		Indexes: []*schema.Index{}, ForeignKeys: []*schema.ForeignKey{}, Checks: []*schema.Check{},
	}
	if doc.findTable(dst.Key()) != nil {
		return conflict("테이블 %s 이(가) 이미 있습니다", dst.Display())
	}
	if len(src.Options) > 0 {
		dst.Options = make(map[string]string, len(src.Options))
		for k, v := range src.Options {
			dst.Options[k] = v
		}
	}

	for _, c := range src.Columns {
		copied := *c
		dst.Columns = append(dst.Columns, &copied)
	}
	renumber(dst)

	if src.PrimaryKey != nil {
		pk := &schema.PrimaryKey{
			// 이름은 새 테이블에 맞춰 바꾼다. PostgreSQL·MS-SQL에서 제약 이름은
			// 스키마 안에서 유일해야 하므로, 그대로 베끼면 실행 시점에 실패한다.
			Name:    copyConstraintName(doc, src.PrimaryKey.Name, src.Name, name),
			Columns: append([]string(nil), src.PrimaryKey.Columns...),
		}
		dst.PrimaryKey = pk
	}
	if p.WithIndexes == nil || *p.WithIndexes {
		for _, idx := range src.Indexes {
			copied := &schema.Index{
				Name:    copyConstraintName(doc, idx.Name, src.Name, name),
				Unique:  idx.Unique,
				Type:    idx.Type,
				Where:   idx.Where,
				Columns: append([]schema.IndexPart(nil), idx.Columns...),
			}
			dst.Indexes = append(dst.Indexes, copied)
		}
	}
	if p.WithChecks == nil || *p.WithChecks {
		for _, ck := range src.Checks {
			dst.Checks = append(dst.Checks, &schema.Check{
				Name:       copyConstraintName(doc, ck.Name, src.Name, name),
				Expression: ck.Expression,
			})
		}
	}
	if p.WithFKs == nil || *p.WithFKs {
		for _, fk := range src.ForeignKeys {
			dst.ForeignKeys = append(dst.ForeignKeys, &schema.ForeignKey{
				Name:         copyConstraintName(doc, fk.Name, src.Name, name),
				Columns:      append([]string(nil), fk.Columns...),
				RefNamespace: fk.RefNamespace,
				RefTable:     fk.RefTable,
				RefColumns:   append([]string(nil), fk.RefColumns...),
				OnDelete:     fk.OnDelete,
				OnUpdate:     fk.OnUpdate,
			})
		}
	}

	doc.Schema.Tables = append(doc.Schema.Tables, dst)

	// 자리: 원본 옆에 조금 비껴 놓는다. 정확히 겹치면 사본이 만들어졌는지 화면만
	// 보고 알 수 없고, 끌어서 옮기려 해도 어느 쪽을 잡았는지 알 수 없다.
	box := doc.Layout[src.Key()]
	x, y := doc.nextFreeSlot()
	if box != nil {
		x, y = box.X+40, box.Y+40
	}
	if p.X != nil {
		x = *p.X
	}
	if p.Y != nil {
		y = *p.Y
	}
	next := &Box{X: x, Y: y}
	if box != nil {
		// 표시 정보(색·아이콘·논리명)는 함께 베낀다. 사본이 원본과 나란히 있을 때
		// 같은 묶음으로 보이는 편이 맞다.
		next.Collapsed, next.Color, next.Icon = box.Collapsed, box.Color, box.Icon
		// 테이블 논리명은 베끼지 않는다. 물리명이 users_copy 가 된 사본에 "회원"이
		// 그대로 붙어 있으면 화면에 같은 이름이 둘 생긴다 — 사본을 만든 사람이
		// 무엇을 만들었는지 알 수 없게 된다. 컬럼 논리명은 그대로 쓸모가 있다.
		if len(box.ColumnIcons) > 0 {
			next.ColumnIcons = make(map[string]string, len(box.ColumnIcons))
			for k, v := range box.ColumnIcons {
				next.ColumnIcons[k] = v
			}
		}
		if len(box.ColumnLogical) > 0 {
			next.ColumnLogical = make(map[string]string, len(box.ColumnLogical))
			for k, v := range box.ColumnLogical {
				next.ColumnLogical[k] = v
			}
		}
	}
	doc.Layout[dst.Key()] = next
	return nil
}

// freeTableName은 "이름_copy" 꼴로 비어 있는 이름을 찾는다.
func freeTableName(doc *Document, base, ns string) string {
	key := func(name string) string {
		if ns == "" {
			return strings.ToLower(name)
		}
		return strings.ToLower(ns + "." + name)
	}
	name := base + "_copy"
	for i := 2; doc.findTable(key(name)) != nil; i++ {
		name = fmt.Sprintf("%s_copy%d", base, i)
	}
	return name
}

// copyConstraintName은 제약·인덱스 이름을 사본의 이름에 맞춘다.
//
// 이름에 원본 테이블 이름이 들어 있으면 그 자리만 바꾼다(ix_users_email →
// ix_users_copy_email). 없으면 앞에 새 테이블 이름을 붙인다. 어느 쪽이든 문서
// 안에서 이미 쓰이는 이름이면 뒤에 번호를 붙인다 — 인덱스 이름은 MySQL에서는
// 테이블마다, PostgreSQL에서는 스키마 안에서 유일해야 하므로 넓은 쪽을 기준으로 본다.
func copyConstraintName(doc *Document, old, srcTable, dstTable string) string {
	if strings.TrimSpace(old) == "" {
		return ""
	}
	// PRIMARY 는 MySQL이 기본키에 붙이는 암묵 이름이다. 테이블마다 하나뿐이라
	// 겹칠 수 없고, MySQL은 이 이름을 바꿀 수도 없다. 그대로 두지 않으면 사본의
	// DDL에 ic_users_copy_PRIMARY 같은 아무도 쓰지 않는 이름이 남는다.
	if strings.EqualFold(old, "PRIMARY") {
		return old
	}
	lower := strings.ToLower(old)
	at := strings.Index(lower, strings.ToLower(srcTable))
	candidate := dstTable + "_" + old
	if at >= 0 {
		candidate = old[:at] + dstTable + old[at+len(srcTable):]
	}
	name := candidate
	for i := 2; constraintNameTaken(doc, name); i++ {
		name = fmt.Sprintf("%s_%d", candidate, i)
	}
	return name
}

// constraintNameTaken은 문서 전체에서 그 이름이 이미 쓰이는지 본다.
func constraintNameTaken(doc *Document, name string) bool {
	for _, t := range doc.Schema.Tables {
		if t.PrimaryKey != nil && strings.EqualFold(t.PrimaryKey.Name, name) {
			return true
		}
		for _, idx := range t.Indexes {
			if strings.EqualFold(idx.Name, name) {
				return true
			}
		}
		for _, fk := range t.ForeignKeys {
			if strings.EqualFold(fk.Name, name) {
				return true
			}
		}
		for _, ck := range t.Checks {
			if strings.EqualFold(ck.Name, name) {
				return true
			}
		}
	}
	return false
}

type tableUpdatePayload struct {
	Key       string   `json:"key"`
	Name      *string  `json:"name,omitempty"`
	Namespace *string  `json:"namespace,omitempty"`
	Comment   *string  `json:"comment,omitempty"`
	Collapsed *bool    `json:"collapsed,omitempty"`
	Color     *string  `json:"color,omitempty"`
	Options   *KVPatch `json:"options,omitempty"`
}

// KVPatch는 문자열 맵의 부분 갱신이다. 값이 빈 문자열이면 그 키를 지운다.
type KVPatch map[string]string

func applyTableUpdate(doc *Document, op *Op) error {
	var p tableUpdatePayload
	if err := decode(op, &p); err != nil {
		return err
	}
	tbl := doc.findTable(p.Key)
	if tbl == nil {
		return notFound("테이블 %s 을(를) 찾을 수 없습니다 (다른 사용자가 지웠을 수 있습니다)", p.Key)
	}
	oldKey := tbl.Key()

	if p.Name != nil {
		name, err := validateIdent("테이블", *p.Name)
		if err != nil {
			return err
		}
		tbl.Name = name
	}
	if p.Namespace != nil {
		ns := strings.TrimSpace(*p.Namespace)
		if ns != "" {
			var err error
			if ns, err = validateIdent("네임스페이스", ns); err != nil {
				return err
			}
		}
		tbl.Namespace = ns
	}
	if p.Comment != nil {
		tbl.Comment = strings.TrimSpace(*p.Comment)
	}
	if p.Options != nil {
		if tbl.Options == nil {
			tbl.Options = map[string]string{}
		}
		for k, v := range *p.Options {
			if v == "" {
				delete(tbl.Options, k)
				continue
			}
			tbl.Options[k] = v
		}
	}

	newKey := tbl.Key()
	if newKey != oldKey {
		// 이름이 바뀌면 다른 테이블과 충돌하는지 확인한다. 원상 복구는 호출자가
		// 사본에 적용하기 때문에 필요 없다.
		for _, other := range doc.Schema.Tables {
			if other != tbl && other.Key() == newKey {
				return conflict("테이블 이름 %s 이(가) 이미 쓰이고 있습니다", tbl.Display())
			}
		}
		// 레이아웃 키와 이 테이블을 참조하는 외래키를 함께 옮긴다.
		// 참조를 방치하면 ERD의 관계선이 끊기고, 생성된 DDL이 없는 테이블을 가리킨다.
		if box, ok := doc.Layout[oldKey]; ok {
			delete(doc.Layout, oldKey)
			doc.Layout[newKey] = box
		}
		renameReferences(doc.Schema, oldKey, tbl)
	}

	// 레이아웃 속성은 구조가 아니지만 테이블 카드의 성질이므로 같은 op로 받는다.
	if p.Collapsed != nil || p.Color != nil {
		box := doc.Layout[tbl.Key()]
		if box == nil {
			box = &Box{}
			doc.Layout[tbl.Key()] = box
		}
		if p.Collapsed != nil {
			box.Collapsed = *p.Collapsed
		}
		if p.Color != nil {
			box.Color = strings.TrimSpace(*p.Color)
		}
	}
	return nil
}

// renameReferences는 테이블 이름이 바뀔 때 이를 참조하는 외래키를 갱신한다.
func renameReferences(sc *schema.Schema, oldKey string, renamed *schema.Table) {
	for _, t := range sc.Tables {
		for _, fk := range t.ForeignKeys {
			refKey := fk.RefKey()
			if refKey != oldKey {
				continue
			}
			fk.RefTable = renamed.Name
			// 같은 네임스페이스면 생략하는 것이 introspect의 규칙이라 그대로 따른다.
			if strings.EqualFold(renamed.Namespace, t.Namespace) {
				fk.RefNamespace = ""
				continue
			}
			fk.RefNamespace = renamed.Namespace
		}
	}
}

type tableMovePayload struct {
	Key       string   `json:"key"`
	X         float64  `json:"x"`
	Y         float64  `json:"y"`
	Collapsed *bool    `json:"collapsed,omitempty"`
	Color     *string  `json:"color,omitempty"`
	Width     *float64 `json:"width,omitempty"`
	// Icon은 카드 제목 옆 표식이다. 좌표와 함께 오는 이유는 이것들이 모두
	// "구조가 아닌 표시 정보"이고 같은 Box에 담기기 때문이다.
	Icon *string `json:"icon,omitempty"`
	// ColumnIcons는 컬럼 이름 → 아이콘의 **부분** 지도다. 보낸 것만 바뀐다.
	ColumnIcons map[string]string `json:"columnIcons,omitempty"`
	// Logical은 테이블 논리명이다. 아이콘과 같은 이유로 여기에 함께 온다.
	Logical *string `json:"logical,omitempty"`
	// ColumnLogical은 컬럼 이름 → 논리명의 **부분** 지도다. 보낸 것만 바뀐다.
	ColumnLogical map[string]string `json:"columnLogical,omitempty"`
}

func applyTableMove(doc *Document, op *Op) error {
	var p tableMovePayload
	if err := decode(op, &p); err != nil {
		return err
	}
	tbl := doc.findTable(p.Key)
	if tbl == nil {
		return notFound("테이블 %s 을(를) 찾을 수 없습니다", p.Key)
	}
	box := doc.Layout[tbl.Key()]
	if box == nil {
		box = &Box{}
		doc.Layout[tbl.Key()] = box
	}
	box.X, box.Y = p.X, p.Y
	// 폭은 보낸 경우에만 바꾼다. 옮기기만 하는 op가 폭을 0으로 되돌리면, 넓혀 둔
	// 카드가 남이 한 번 끌 때마다 기본 폭으로 돌아간다.
	if p.Width != nil {
		box.W = clampCardWidth(*p.Width)
	}
	if p.Collapsed != nil {
		box.Collapsed = *p.Collapsed
	}
	if p.Color != nil {
		box.Color = strings.TrimSpace(*p.Color)
	}
	if p.Icon != nil {
		box.Icon = strings.TrimSpace(*p.Icon)
	}
	// 컬럼 아이콘은 **부분 갱신**이다. 통째로 받으면 같은 표를 함께 보는 두 사람이
	// 서로의 아이콘을 지우게 된다 — 내가 보낸 지도에 없는 컬럼은 사라지기 때문이다.
	// 빈 값은 "자동으로 되돌리기"라 지운다.
	for name, ic := range p.ColumnIcons {
		key := strings.ToLower(strings.TrimSpace(name))
		if key == "" {
			continue
		}
		ic = strings.TrimSpace(ic)
		if ic == "" {
			delete(box.ColumnIcons, key)
			continue
		}
		if box.ColumnIcons == nil {
			box.ColumnIcons = map[string]string{}
		}
		box.ColumnIcons[key] = ic
	}
	if p.Logical != nil {
		box.Logical = strings.TrimSpace(*p.Logical)
	}
	// 컬럼 논리명도 부분 갱신이다(아이콘과 같은 이유). 빈 값은 지우는 것이다.
	for name, label := range p.ColumnLogical {
		key := strings.ToLower(strings.TrimSpace(name))
		if key == "" {
			continue
		}
		label = strings.TrimSpace(label)
		if label == "" {
			delete(box.ColumnLogical, key)
			continue
		}
		if box.ColumnLogical == nil {
			box.ColumnLogical = map[string]string{}
		}
		box.ColumnLogical[key] = label
	}
	return nil
}

type tableDeletePayload struct {
	Key string `json:"key"`
	// Cascade면 이 테이블을 참조하는 외래키도 함께 지운다.
	// 기본은 거부다 — 참조를 남기면 생성된 DDL이 실행되지 않고, 조용히 지우면
	// 사용자가 의도하지 않은 관계 삭제가 일어난다.
	Cascade bool `json:"cascade,omitempty"`
}

func applyTableDelete(doc *Document, op *Op) error {
	var p tableDeletePayload
	if err := decode(op, &p); err != nil {
		return err
	}
	tbl := doc.findTable(p.Key)
	if tbl == nil {
		return notFound("테이블 %s 을(를) 찾을 수 없습니다 (이미 지워졌을 수 있습니다)", p.Key)
	}
	key := tbl.Key()

	referrers := []string{}
	for _, t := range doc.Schema.Tables {
		if t == tbl {
			continue
		}
		for _, fk := range t.ForeignKeys {
			if fk.RefKey() == key {
				referrers = append(referrers, t.Display()+"."+fk.Name)
			}
		}
	}
	if len(referrers) > 0 && !p.Cascade {
		return invalid("%s 을(를) 참조하는 외래키가 있습니다: %s",
			tbl.Display(), strings.Join(referrers, ", "))
	}
	if p.Cascade {
		for _, t := range doc.Schema.Tables {
			if t == tbl {
				continue
			}
			kept := t.ForeignKeys[:0]
			for _, fk := range t.ForeignKeys {
				if fk.RefKey() == key {
					continue
				}
				kept = append(kept, fk)
			}
			t.ForeignKeys = kept
		}
	}

	kept := doc.Schema.Tables[:0]
	for _, t := range doc.Schema.Tables {
		if t == tbl {
			continue
		}
		kept = append(kept, t)
	}
	doc.Schema.Tables = kept
	delete(doc.Layout, key)
	return nil
}

// ---------- 컬럼 ----------

type columnAddPayload struct {
	Table string `json:"table"`
	Name  string `json:"name"`
	Type  string `json:"type"`
	// Domain은 재사용 타입의 이름이다. 주면 Type 대신 도메인의 정의를 쓴다.
	Domain   *string `json:"domain,omitempty"`
	Nullable *bool   `json:"nullable,omitempty"`
	Default  *string `json:"default,omitempty"`
	Identity *bool   `json:"identity,omitempty"`
	Comment  *string `json:"comment,omitempty"`
	// Position은 1부터 시작하는 삽입 위치다. 생략하면 맨 뒤에 붙인다.
	Position *int `json:"position,omitempty"`
}

func applyColumnAdd(doc *Document, op *Op) error {
	var p columnAddPayload
	if err := decode(op, &p); err != nil {
		return err
	}
	tbl := doc.findTable(p.Table)
	if tbl == nil {
		return notFound("테이블 %s 을(를) 찾을 수 없습니다", p.Table)
	}
	name, err := validateIdent("컬럼", p.Name)
	if err != nil {
		return err
	}
	if tbl.Column(name) != nil {
		return conflict("%s 에 컬럼 %s 이(가) 이미 있습니다", tbl.Display(), name)
	}
	// 도메인을 주면 타입은 도메인이 정한다. 둘 다 오면 도메인이 이긴다 —
	// 도메인을 고르는 것은 "이 컬럼의 타입을 저 정의에 맡긴다"는 뜻이기 때문이다.
	var dom *Domain
	if p.Domain != nil && strings.TrimSpace(*p.Domain) != "" {
		dom = doc.findDomain(*p.Domain)
		if dom == nil {
			return notFound("도메인 %s 을(를) 찾을 수 없습니다", *p.Domain)
		}
	}
	typeText := p.Type
	if dom != nil {
		typeText = dom.Type
	}
	lt, raw, err := parseColumnType(doc, typeText)
	if err != nil {
		return err
	}

	col := &schema.Column{Name: name, Type: lt, RawType: raw, Nullable: true}
	if dom != nil {
		if err := setColumnDomain(doc, col, dom); err != nil {
			return err
		}
	}
	if p.Nullable != nil {
		col.Nullable = *p.Nullable
	}
	if p.Default != nil && strings.TrimSpace(*p.Default) != "" {
		col.HasDefault = true
		col.Default = strings.TrimSpace(*p.Default)
	}
	if p.Identity != nil {
		col.Identity = *p.Identity
	}
	if p.Comment != nil {
		col.Comment = strings.TrimSpace(*p.Comment)
	}

	at := len(tbl.Columns)
	if p.Position != nil {
		at = *p.Position - 1
		if at < 0 {
			at = 0
		}
		if at > len(tbl.Columns) {
			at = len(tbl.Columns)
		}
	}
	tbl.Columns = append(tbl.Columns, nil)
	copy(tbl.Columns[at+1:], tbl.Columns[at:])
	tbl.Columns[at] = col
	renumber(tbl)
	return nil
}

type columnUpdatePayload struct {
	Table   string  `json:"table"`
	Name    string  `json:"name"`
	NewName *string `json:"newName,omitempty"`
	Type    *string `json:"type,omitempty"`
	// Domain은 재사용 타입의 이름이다. 빈 문자열이면 연결을 끊고 타입은 그대로 둔다.
	Domain   *string `json:"domain,omitempty"`
	Nullable *bool   `json:"nullable,omitempty"`
	// Default는 빈 문자열이면 기본값을 제거한다. null(필드 생략)은 "변경 없음"이다.
	// 이 둘을 구분하지 못하면 기본값 삭제를 표현할 방법이 없다.
	Default   *string `json:"default,omitempty"`
	Identity  *bool   `json:"identity,omitempty"`
	Comment   *string `json:"comment,omitempty"`
	Generated *string `json:"generated,omitempty"`
	Position  *int    `json:"position,omitempty"`
}

func applyColumnUpdate(doc *Document, op *Op) error {
	var p columnUpdatePayload
	if err := decode(op, &p); err != nil {
		return err
	}
	tbl := doc.findTable(p.Table)
	if tbl == nil {
		return notFound("테이블 %s 을(를) 찾을 수 없습니다", p.Table)
	}
	col := tbl.Column(p.Name)
	if col == nil {
		return notFound("%s 에 컬럼 %s 이(가) 없습니다 (다른 사용자가 지웠을 수 있습니다)",
			tbl.Display(), p.Name)
	}
	oldName := col.Name

	if p.NewName != nil {
		name, err := validateIdent("컬럼", *p.NewName)
		if err != nil {
			return err
		}
		if !strings.EqualFold(name, oldName) && tbl.Column(name) != nil {
			return conflict("%s 에 컬럼 %s 이(가) 이미 있습니다", tbl.Display(), name)
		}
		col.Name = name
	}
	if p.Domain != nil {
		name := strings.TrimSpace(*p.Domain)
		if name == "" {
			// 연결만 끊는다. 타입은 마지막으로 입혀진 값 그대로 남는다.
			col.Domain = ""
		} else {
			dom := doc.findDomain(name)
			if dom == nil {
				return notFound("도메인 %s 을(를) 찾을 수 없습니다", name)
			}
			if err := setColumnDomain(doc, col, dom); err != nil {
				return err
			}
		}
	}
	if p.Type != nil {
		lt, raw, err := parseColumnType(doc, *p.Type)
		if err != nil {
			return err
		}
		col.Type, col.RawType = lt, raw
		// 타입을 직접 고치면 도메인에서 벗어난 것이다. 연결을 남겨 두면 도메인을
		// 다음에 고칠 때 이 컬럼의 값이 이유 없이 되돌아간다.
		if p.Domain == nil {
			col.Domain = ""
		}
	}
	if p.Nullable != nil {
		col.Nullable = *p.Nullable
	}
	if p.Default != nil {
		def := strings.TrimSpace(*p.Default)
		col.HasDefault = def != ""
		col.Default = def
	}
	if p.Identity != nil {
		col.Identity = *p.Identity
	}
	if p.Comment != nil {
		col.Comment = strings.TrimSpace(*p.Comment)
	}
	if p.Generated != nil {
		col.Generated = strings.TrimSpace(*p.Generated)
	}
	if p.Position != nil {
		moveColumn(tbl, col, *p.Position)
	}
	if col.Name != oldName {
		renameColumnReferences(doc.Schema, tbl, oldName, col.Name)
		renameColumnIcon(doc.Layout[tbl.Key()], oldName, col.Name)
	}
	renumber(tbl)
	return nil
}

// renameColumnIcon은 컬럼의 표시 정보(아이콘·논리명)를 새 이름으로 옮긴다.
//
// 레이아웃은 이름을 열쇠로 쓰므로 이름이 바뀌면 연결이 끊긴다. 아이콘이나 논리명이
// 사라지는 것은 조용한 손실이라 — 아무도 지우지 않았는데 없어진다 — 여기서 따라가게
// 한다. 물리명을 고치는 일은 흔하고(오타, 규칙 통일), 그때마다 논리명을 다시 적게
// 하면 아무도 논리명을 적지 않게 된다.
func renameColumnIcon(box *Box, oldName, newName string) {
	if box == nil {
		return
	}
	from, to := strings.ToLower(oldName), strings.ToLower(newName)
	if from == to {
		return
	}
	if ic, ok := box.ColumnIcons[from]; ok {
		delete(box.ColumnIcons, from)
		box.ColumnIcons[to] = ic
	}
	if label, ok := box.ColumnLogical[from]; ok {
		delete(box.ColumnLogical, from)
		box.ColumnLogical[to] = label
	}
}

// renameColumnReferences는 컬럼 이름 변경을 PK·인덱스·외래키·참조하는 외래키에 전파한다.
// 이것을 빼먹으면 이름만 바뀌고 제약이 없는 컬럼을 가리켜, ERD 상으로는 정상인데
// 생성된 DDL이 실행되지 않는다.
func renameColumnReferences(sc *schema.Schema, tbl *schema.Table, oldName, newName string) {
	replace := func(list []string) {
		for i, c := range list {
			if strings.EqualFold(c, oldName) {
				list[i] = newName
			}
		}
	}
	if tbl.PrimaryKey != nil {
		replace(tbl.PrimaryKey.Columns)
	}
	for _, idx := range tbl.Indexes {
		for i := range idx.Columns {
			if strings.EqualFold(idx.Columns[i].Column, oldName) {
				idx.Columns[i].Column = newName
			}
		}
	}
	for _, fk := range tbl.ForeignKeys {
		replace(fk.Columns)
	}
	key := tbl.Key()
	for _, other := range sc.Tables {
		for _, fk := range other.ForeignKeys {
			if fk.RefKey() != key {
				continue
			}
			replace(fk.RefColumns)
		}
	}
}

type columnDeletePayload struct {
	Table string `json:"table"`
	Name  string `json:"name"`
}

func applyColumnDelete(doc *Document, op *Op) error {
	var p columnDeletePayload
	if err := decode(op, &p); err != nil {
		return err
	}
	tbl := doc.findTable(p.Table)
	if tbl == nil {
		return notFound("테이블 %s 을(를) 찾을 수 없습니다", p.Table)
	}
	col := tbl.Column(p.Name)
	if col == nil {
		return notFound("%s 에 컬럼 %s 이(가) 없습니다 (이미 지워졌을 수 있습니다)", tbl.Display(), p.Name)
	}

	// 이 컬럼을 쓰는 제약을 함께 정리한다. 남겨두면 존재하지 않는 컬럼을 가리킨다.
	name := col.Name
	kept := tbl.Columns[:0]
	for _, c := range tbl.Columns {
		if c == col {
			continue
		}
		kept = append(kept, c)
	}
	tbl.Columns = kept
	renumber(tbl)
	// 아이콘은 컬럼 이름으로 매달려 있다. 두고 가면 나중에 같은 이름의 컬럼을
	// 만들었을 때 예전 아이콘이 되살아난다 — 고른 적 없는 표식이 붙는다.
	if box := doc.Layout[tbl.Key()]; box != nil {
		delete(box.ColumnIcons, strings.ToLower(name))
	}

	if tbl.PrimaryKey != nil {
		tbl.PrimaryKey.Columns = withoutString(tbl.PrimaryKey.Columns, name)
		if len(tbl.PrimaryKey.Columns) == 0 {
			tbl.PrimaryKey = nil
		}
	}
	idxKept := tbl.Indexes[:0]
	for _, idx := range tbl.Indexes {
		parts := idx.Columns[:0]
		for _, part := range idx.Columns {
			if strings.EqualFold(part.Column, name) {
				continue
			}
			parts = append(parts, part)
		}
		idx.Columns = parts
		if len(idx.Columns) == 0 {
			continue // 컬럼이 하나도 남지 않은 인덱스는 의미가 없다
		}
		idxKept = append(idxKept, idx)
	}
	tbl.Indexes = idxKept

	fkKept := tbl.ForeignKeys[:0]
	for _, fk := range tbl.ForeignKeys {
		if containsFold(fk.Columns, name) {
			continue // 복합 외래키에서 한 컬럼만 빼면 의미가 달라지므로 제약 전체를 지운다
		}
		fkKept = append(fkKept, fk)
	}
	tbl.ForeignKeys = fkKept

	key := tbl.Key()
	for _, other := range doc.Schema.Tables {
		if other == tbl {
			continue
		}
		kept := other.ForeignKeys[:0]
		for _, fk := range other.ForeignKeys {
			if fk.RefKey() == key && containsFold(fk.RefColumns, name) {
				continue
			}
			kept = append(kept, fk)
		}
		other.ForeignKeys = kept
	}
	return nil
}

// renumber는 컬럼 Position을 1부터 촘촘하게 다시 매긴다.
// Position은 정렬 기준이면서 DDL의 컬럼 순서이므로 구멍이 생기면 안 된다.
func renumber(tbl *schema.Table) {
	for i, c := range tbl.Columns {
		c.Position = i + 1
	}
}

func moveColumn(tbl *schema.Table, col *schema.Column, position int) {
	from := -1
	for i, c := range tbl.Columns {
		if c == col {
			from = i
			break
		}
	}
	if from < 0 {
		return
	}
	to := position - 1
	if to < 0 {
		to = 0
	}
	if to >= len(tbl.Columns) {
		to = len(tbl.Columns) - 1
	}
	if to == from {
		return
	}
	rest := append(tbl.Columns[:from:from], tbl.Columns[from+1:]...)
	rest = append(rest, nil)
	copy(rest[to+1:], rest[to:])
	rest[to] = col
	tbl.Columns = rest
}

// parseColumnType은 사용자가 입력한 타입 문자열을 논리 타입으로 정규화한다.
//
// 정규화에 실패한 타입(벤더 전용 타입 등)도 거부하지 않고 원본을 RawType에 보존한다.
// DDL 생성기가 같은 dialect에서는 원본을 그대로 쓰기 때문에 동작에 문제가 없고,
// 다른 dialect로 변환할 때만 근사가 불가능하다 — 그 상황은 diff/계획 단계에서 드러난다.
func parseColumnType(doc *Document, raw string) (schema.LogicalType, string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return schema.LogicalType{}, "", invalid("컬럼 타입이 비어 있습니다")
	}
	if len([]rune(raw)) > 200 {
		return schema.LogicalType{}, "", invalid("컬럼 타입 문자열이 너무 깁니다")
	}
	if strings.ContainsAny(raw, ";\x00\n\r") {
		return schema.LogicalType{}, "", invalid("컬럼 타입에 쓸 수 없는 문자가 있습니다")
	}
	// 문서에 정의된 enum 이름을 쓰면 enum 타입으로 해석한다.
	// introspect가 PostgreSQL의 이름 붙은 enum을 이렇게 표현하므로, 여기서 맞추지 않으면
	// ERD로 그린 enum 컬럼이 실제 DB의 같은 컬럼과 영원히 다르게 보인다.
	for _, e := range doc.Schema.Enums {
		if strings.EqualFold(e.Name, raw) {
			return schema.LogicalType{Base: schema.TypeEnum, EnumName: e.Name}, raw, nil
		}
	}
	return schema.ParseType(doc.Dialect, raw), raw, nil
}

// ---------- 기본키 ----------

type pkSetPayload struct {
	Table   string   `json:"table"`
	Name    string   `json:"name,omitempty"`
	Columns []string `json:"columns"`
}

func applyPKSet(doc *Document, op *Op) error {
	var p pkSetPayload
	if err := decode(op, &p); err != nil {
		return err
	}
	tbl := doc.findTable(p.Table)
	if tbl == nil {
		return notFound("테이블 %s 을(를) 찾을 수 없습니다", p.Table)
	}
	if len(p.Columns) == 0 {
		tbl.PrimaryKey = nil
		return nil
	}
	cols, err := resolveColumns(tbl, p.Columns, "기본키")
	if err != nil {
		return err
	}
	// 기본키 컬럼은 NULL일 수 없다. 사용자가 따로 지정하게 하면 대상 DB에서
	// 거부되는 DDL이 만들어지므로 여기서 맞춘다.
	for _, name := range cols {
		if c := tbl.Column(name); c != nil {
			c.Nullable = false
		}
	}
	name := strings.TrimSpace(p.Name)
	if name != "" {
		if name, err = validateIdent("기본키", name); err != nil {
			return err
		}
	}
	tbl.PrimaryKey = &schema.PrimaryKey{Name: name, Columns: cols}
	return nil
}

// resolveColumns는 컬럼 이름 목록을 검증하고 실제 컬럼의 표기(대소문자)로 맞춘다.
func resolveColumns(tbl *schema.Table, names []string, what string) ([]string, error) {
	out := make([]string, 0, len(names))
	seen := map[string]bool{}
	for _, raw := range names {
		name := strings.TrimSpace(raw)
		if name == "" {
			return nil, invalid("%s 컬럼 이름이 비어 있습니다", what)
		}
		col := tbl.Column(name)
		if col == nil {
			return nil, invalid("%s 에 컬럼 %s 이(가) 없습니다", tbl.Display(), name)
		}
		lower := strings.ToLower(col.Name)
		if seen[lower] {
			return nil, invalid("%s 컬럼 목록에 %s 이(가) 중복됩니다", what, col.Name)
		}
		seen[lower] = true
		out = append(out, col.Name)
	}
	return out, nil
}

// ---------- 인덱스 ----------

type indexPayload struct {
	Table   string   `json:"table"`
	Name    string   `json:"name"`
	Columns []string `json:"columns"`
	Unique  *bool    `json:"unique,omitempty"`
	Type    *string  `json:"type,omitempty"`
	Where   *string  `json:"where,omitempty"`
	// NewName은 이름 바꾸기다. Name이 찾는 열쇠이므로 새 이름은 따로 받는다.
	//
	// 지웠다 다시 만들지 않는 이유: 그 사이에 인덱스가 없는 순간이 생기고, 그
	// 두 op는 되돌리기에서도 따로 논다.
	NewName *string `json:"newName,omitempty"`
	// Descending은 내림차순으로 둘 컬럼 이름이다.
	//
	// 컬럼 목록과 나란한 배열이 아니라 이름의 집합인 이유: 나란한 배열은 두 목록의
	// 길이가 어긋나는 순간 어느 컬럼이 내림차순인지 알 수 없게 된다.
	Descending []string `json:"descending,omitempty"`
}

func applyIndexUpsert(doc *Document, op *Op) error {
	var p indexPayload
	if err := decode(op, &p); err != nil {
		return err
	}
	tbl := doc.findTable(p.Table)
	if tbl == nil {
		return notFound("테이블 %s 을(를) 찾을 수 없습니다", p.Table)
	}
	name, err := validateIdent("인덱스", p.Name)
	if err != nil {
		return err
	}

	var idx *schema.Index
	for _, existing := range tbl.Indexes {
		if strings.EqualFold(existing.Name, name) {
			idx = existing
			break
		}
	}
	if op.Kind == OpIndexAdd && idx != nil {
		return conflict("인덱스 %s 이(가) 이미 있습니다", name)
	}
	if op.Kind == OpIndexUpdate && idx == nil {
		return notFound("인덱스 %s 을(를) 찾을 수 없습니다", name)
	}
	if idx == nil {
		idx = &schema.Index{Name: name}
		tbl.Indexes = append(tbl.Indexes, idx)
	}

	if p.NewName != nil {
		next, err := validateIdent("인덱스", *p.NewName)
		if err != nil {
			return err
		}
		for _, other := range tbl.Indexes {
			if other != idx && strings.EqualFold(other.Name, next) {
				return conflict("인덱스 %s 이(가) 이미 있습니다", next)
			}
		}
		idx.Name = next
	}

	if len(p.Columns) > 0 {
		cols, err := resolveColumns(tbl, p.Columns, "인덱스")
		if err != nil {
			return err
		}
		desc := map[string]bool{}
		for _, name := range p.Descending {
			desc[strings.ToLower(strings.TrimSpace(name))] = true
		}
		parts := make([]schema.IndexPart, 0, len(cols))
		for _, c := range cols {
			parts = append(parts, schema.IndexPart{Column: c, Descending: desc[strings.ToLower(c)]})
		}
		idx.Columns = parts
	} else if op.Kind == OpIndexAdd {
		return invalid("인덱스 컬럼을 하나 이상 지정해야 합니다")
	}
	if p.Unique != nil {
		idx.Unique = *p.Unique
	}
	if p.Type != nil {
		idx.Type = strings.TrimSpace(*p.Type)
	}
	if p.Where != nil {
		idx.Where = strings.TrimSpace(*p.Where)
	}
	return nil
}

type namedTargetPayload struct {
	Table string `json:"table"`
	Name  string `json:"name"`
}

func applyIndexDelete(doc *Document, op *Op) error {
	var p namedTargetPayload
	if err := decode(op, &p); err != nil {
		return err
	}
	tbl := doc.findTable(p.Table)
	if tbl == nil {
		return notFound("테이블 %s 을(를) 찾을 수 없습니다", p.Table)
	}
	// 존재 확인을 걸러내기 전에 한다. 걸러내기는 슬라이스를 제자리에서 덮어쓰므로,
	// 실패를 나중에 알면 오류를 반환하면서도 상태를 이미 건드린 상태가 된다.
	if !hasIndex(tbl, p.Name) {
		return notFound("인덱스 %s 을(를) 찾을 수 없습니다", p.Name)
	}
	kept := tbl.Indexes[:0]
	for _, idx := range tbl.Indexes {
		if strings.EqualFold(idx.Name, p.Name) {
			continue
		}
		kept = append(kept, idx)
	}
	tbl.Indexes = kept
	return nil
}

func hasIndex(tbl *schema.Table, name string) bool {
	for _, idx := range tbl.Indexes {
		if strings.EqualFold(idx.Name, name) {
			return true
		}
	}
	return false
}

// ---------- 외래키 ----------

type fkPayload struct {
	Table        string   `json:"table"`
	Name         string   `json:"name"`
	Columns      []string `json:"columns"`
	RefTable     string   `json:"refTable"`
	RefNamespace *string  `json:"refNamespace,omitempty"`
	RefColumns   []string `json:"refColumns"`
	OnDelete     *string  `json:"onDelete,omitempty"`
	OnUpdate     *string  `json:"onUpdate,omitempty"`
	// NewName은 이름 바꾸기다. Name은 대상을 찾는 열쇠이므로 새 이름은 따로 받는다.
	//
	// 지웠다 새로 만드는 방식과 다른 이유: 제약 이름은 대상 DB에도 그대로 나가는
	// 이름이고, 지우고 만드는 두 op는 되돌리기에서도 따로 논다.
	NewName *string `json:"newName,omitempty"`
}

// 허용하는 참조 동작. 임의 문자열을 그대로 DDL에 넣으면 안 된다.
var fkActions = map[string]string{
	"":            "",
	"NO ACTION":   "NO ACTION",
	"RESTRICT":    "RESTRICT",
	"CASCADE":     "CASCADE",
	"SET NULL":    "SET NULL",
	"SET DEFAULT": "SET DEFAULT",
}

func applyFKUpsert(doc *Document, op *Op) error {
	var p fkPayload
	if err := decode(op, &p); err != nil {
		return err
	}
	tbl := doc.findTable(p.Table)
	if tbl == nil {
		return notFound("테이블 %s 을(를) 찾을 수 없습니다", p.Table)
	}
	name, err := validateIdent("외래키", p.Name)
	if err != nil {
		return err
	}

	var fk *schema.ForeignKey
	for _, existing := range tbl.ForeignKeys {
		if strings.EqualFold(existing.Name, name) {
			fk = existing
			break
		}
	}
	if op.Kind == OpFKAdd && fk != nil {
		return conflict("외래키 %s 이(가) 이미 있습니다", name)
	}
	if op.Kind == OpFKUpdate && fk == nil {
		return notFound("외래키 %s 을(를) 찾을 수 없습니다", name)
	}
	isNew := fk == nil
	if isNew {
		fk = &schema.ForeignKey{Name: name}
	}

	if p.NewName != nil {
		next, err := validateIdent("외래키", *p.NewName)
		if err != nil {
			return err
		}
		for _, other := range tbl.ForeignKeys {
			if other != fk && strings.EqualFold(other.Name, next) {
				return conflict("외래키 %s 이(가) 이미 있습니다", next)
			}
		}
		fk.Name = next
	}

	if p.RefTable != "" || isNew {
		refTable, err := validateIdent("참조 테이블", p.RefTable)
		if err != nil {
			return err
		}
		refNS := tbl.Namespace
		if p.RefNamespace != nil {
			refNS = strings.TrimSpace(*p.RefNamespace)
		}
		refKey := strings.ToLower(refTable)
		if refNS != "" {
			refKey = strings.ToLower(refNS + "." + refTable)
		}
		target := doc.findTable(refKey)
		if target == nil {
			return invalid("참조할 테이블 %s 이(가) 문서에 없습니다", refKey)
		}
		fk.RefTable = target.Name
		if strings.EqualFold(target.Namespace, tbl.Namespace) {
			fk.RefNamespace = ""
		} else {
			fk.RefNamespace = target.Namespace
		}

		if len(p.Columns) == 0 || len(p.RefColumns) == 0 {
			return invalid("외래키의 컬럼과 참조 컬럼을 지정해야 합니다")
		}
		if len(p.Columns) != len(p.RefColumns) {
			return invalid("외래키 컬럼 수(%d)와 참조 컬럼 수(%d)가 다릅니다",
				len(p.Columns), len(p.RefColumns))
		}
		cols, err := resolveColumns(tbl, p.Columns, "외래키")
		if err != nil {
			return err
		}
		refCols, err := resolveColumns(target, p.RefColumns, "참조")
		if err != nil {
			return err
		}
		// 참조 대상은 고유해야 한다. 그렇지 않은 컬럼을 참조하는 FK는 실제 DB가 거부한다.
		// ERD 단계에서 막아야 마이그레이션 실행 시점에 실패하지 않는다.
		if !isUniqueTarget(target, refCols) {
			return invalid("%s(%s) 은(는) 기본키나 고유 인덱스가 아니어서 참조할 수 없습니다",
				target.Display(), strings.Join(refCols, ", "))
		}
		fk.Columns, fk.RefColumns = cols, refCols
	}

	if p.OnDelete != nil {
		action, ok := fkActions[strings.ToUpper(strings.TrimSpace(*p.OnDelete))]
		if !ok {
			return invalid("지원하지 않는 ON DELETE 동작입니다: %s", *p.OnDelete)
		}
		fk.OnDelete = action
	}
	if p.OnUpdate != nil {
		action, ok := fkActions[strings.ToUpper(strings.TrimSpace(*p.OnUpdate))]
		if !ok {
			return invalid("지원하지 않는 ON UPDATE 동작입니다: %s", *p.OnUpdate)
		}
		fk.OnUpdate = action
	}
	if isNew {
		tbl.ForeignKeys = append(tbl.ForeignKeys, fk)
	}
	return nil
}

// isUniqueTarget은 참조 컬럼 조합이 기본키 또는 고유 인덱스와 일치하는지 본다.
// 순서는 무관하다 — 제약의 고유성은 컬럼 집합으로 결정된다.
func isUniqueTarget(tbl *schema.Table, cols []string) bool {
	if tbl.PrimaryKey != nil && sameSet(tbl.PrimaryKey.Columns, cols) {
		return true
	}
	for _, idx := range tbl.Indexes {
		if idx.Unique && sameSet(idx.ColumnNames(), cols) {
			return true
		}
	}
	return false
}

func applyFKDelete(doc *Document, op *Op) error {
	var p namedTargetPayload
	if err := decode(op, &p); err != nil {
		return err
	}
	tbl := doc.findTable(p.Table)
	if tbl == nil {
		return notFound("테이블 %s 을(를) 찾을 수 없습니다", p.Table)
	}
	found := false
	for _, fk := range tbl.ForeignKeys {
		if strings.EqualFold(fk.Name, p.Name) {
			found = true
			break
		}
	}
	if !found {
		return notFound("외래키 %s 을(를) 찾을 수 없습니다", p.Name)
	}
	kept := tbl.ForeignKeys[:0]
	for _, fk := range tbl.ForeignKeys {
		if strings.EqualFold(fk.Name, p.Name) {
			continue
		}
		kept = append(kept, fk)
	}
	tbl.ForeignKeys = kept
	return nil
}

// ---------- 체크 제약 ----------

type checkPayload struct {
	Table      string `json:"table"`
	Name       string `json:"name"`
	Expression string `json:"expression"`
}

func applyCheckAdd(doc *Document, op *Op) error {
	var p checkPayload
	if err := decode(op, &p); err != nil {
		return err
	}
	tbl := doc.findTable(p.Table)
	if tbl == nil {
		return notFound("테이블 %s 을(를) 찾을 수 없습니다", p.Table)
	}
	name, err := validateIdent("체크 제약", p.Name)
	if err != nil {
		return err
	}
	expr := strings.TrimSpace(p.Expression)
	if expr == "" {
		return invalid("체크 제약식이 비어 있습니다")
	}
	// 체크식은 SQL 식이므로 식별자만큼 좁게 검증할 수 없다. 문장 종결자만 막아
	// 하나의 식이라는 성질을 지킨다.
	if strings.Contains(expr, ";") {
		return invalid("체크 제약식에 세미콜론을 쓸 수 없습니다")
	}
	for _, existing := range tbl.Checks {
		if strings.EqualFold(existing.Name, name) {
			existing.Expression = expr
			return nil
		}
	}
	tbl.Checks = append(tbl.Checks, &schema.Check{Name: name, Expression: expr})
	return nil
}

func applyCheckDelete(doc *Document, op *Op) error {
	var p namedTargetPayload
	if err := decode(op, &p); err != nil {
		return err
	}
	tbl := doc.findTable(p.Table)
	if tbl == nil {
		return notFound("테이블 %s 을(를) 찾을 수 없습니다", p.Table)
	}
	found := false
	for _, ck := range tbl.Checks {
		if strings.EqualFold(ck.Name, p.Name) {
			found = true
			break
		}
	}
	if !found {
		return notFound("체크 제약 %s 을(를) 찾을 수 없습니다", p.Name)
	}
	kept := tbl.Checks[:0]
	for _, ck := range tbl.Checks {
		if strings.EqualFold(ck.Name, p.Name) {
			continue
		}
		kept = append(kept, ck)
	}
	tbl.Checks = kept
	return nil
}

// ---------- enum ----------

type enumPayload struct {
	Name      string   `json:"name"`
	Namespace *string  `json:"namespace,omitempty"`
	Values    []string `json:"values"`
}

func applyEnumUpsert(doc *Document, op *Op) error {
	var p enumPayload
	if err := decode(op, &p); err != nil {
		return err
	}
	name, err := validateIdent("enum", p.Name)
	if err != nil {
		return err
	}
	ns := ""
	if p.Namespace != nil {
		ns = strings.TrimSpace(*p.Namespace)
	}
	key := strings.ToLower(name)
	if ns != "" {
		key = strings.ToLower(ns + "." + name)
	}

	var target *schema.Enum
	for _, e := range doc.Schema.Enums {
		if e.Key() == key {
			target = e
			break
		}
	}
	if op.Kind == OpEnumAdd && target != nil {
		return conflict("enum %s 이(가) 이미 있습니다", key)
	}
	if op.Kind == OpEnumUpdate && target == nil {
		return notFound("enum %s 을(를) 찾을 수 없습니다", key)
	}
	if len(p.Values) == 0 {
		return invalid("enum 값을 하나 이상 지정해야 합니다")
	}
	values := make([]string, 0, len(p.Values))
	seen := map[string]bool{}
	for _, v := range p.Values {
		v = strings.TrimSpace(v)
		if v == "" {
			return invalid("enum 값이 비어 있습니다")
		}
		if strings.ContainsAny(v, "'\x00\n\r") {
			return invalid("enum 값에 쓸 수 없는 문자가 있습니다: %s", v)
		}
		if seen[v] {
			return invalid("enum 값 %s 이(가) 중복됩니다", v)
		}
		seen[v] = true
		values = append(values, v)
	}
	if target == nil {
		doc.Schema.Enums = append(doc.Schema.Enums, &schema.Enum{Namespace: ns, Name: name, Values: values})
		return nil
	}
	target.Values = values
	return nil
}

func applyEnumDelete(doc *Document, op *Op) error {
	var p struct {
		Key string `json:"key"`
	}
	if err := decode(op, &p); err != nil {
		return err
	}
	key := strings.ToLower(strings.TrimSpace(p.Key))
	var target *schema.Enum
	for _, e := range doc.Schema.Enums {
		if e.Key() == key {
			target = e
			break
		}
	}
	if target == nil {
		return notFound("enum %s 을(를) 찾을 수 없습니다", p.Key)
	}

	// 이 enum을 쓰는 컬럼이 있으면 지우지 않는다. 지우면 존재하지 않는 타입을
	// 가리키는 컬럼이 남고, 생성된 DDL이 실행되지 않는다.
	// 컬럼의 EnumName은 네임스페이스 없는 타입 이름이므로 Name과 비교한다.
	users := []string{}
	for _, t := range doc.Schema.Tables {
		for _, c := range t.Columns {
			if c.Type.EnumName != "" && strings.EqualFold(c.Type.EnumName, target.Name) {
				users = append(users, t.Display()+"."+c.Name)
			}
		}
	}
	if len(users) > 0 {
		return invalid("이 enum을 사용하는 컬럼이 있습니다: %s", strings.Join(users, ", "))
	}

	kept := doc.Schema.Enums[:0]
	for _, e := range doc.Schema.Enums {
		if e == target {
			continue
		}
		kept = append(kept, e)
	}
	doc.Schema.Enums = kept
	return nil
}

// ---------- 메모 ----------

type notePayload struct {
	ID    string   `json:"id"`
	Text  *string  `json:"text,omitempty"`
	X     *float64 `json:"x,omitempty"`
	Y     *float64 `json:"y,omitempty"`
	Color *string  `json:"color,omitempty"`
	W     *float64 `json:"w,omitempty"`
	H     *float64 `json:"h,omitempty"`
}

// applyNotePatch는 add·update가 공유하는 필드 반영이다.
// 두 곳에 같은 목록을 두면 한쪽만 늘어난다(크기를 추가할 때 실제로 그럴 뻔했다).
func applyNotePatch(n *Note, p *notePayload) error {
	if p.Text != nil {
		if len([]rune(*p.Text)) > maxNoteLen {
			return invalid("메모가 너무 깁니다 (%d자 제한)", maxNoteLen)
		}
		n.Text = *p.Text
	}
	if p.X != nil {
		n.X = *p.X
	}
	if p.Y != nil {
		n.Y = *p.Y
	}
	if p.Color != nil {
		n.Color = strings.TrimSpace(*p.Color)
	}
	// 너무 작으면 글자가 한 자도 보이지 않고, 너무 크면 캔버스를 덮는다.
	if p.W != nil {
		n.W = clampSize(*p.W, 120, 900)
	}
	if p.H != nil {
		n.H = clampSize(*p.H, 60, 900)
	}
	return nil
}

func clampSize(v, min, max float64) float64 {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

const maxNoteLen = 4000

// 메모·묶음 수 상한.
//
// 상한을 두는 이유는 저장 비용이 아니라 화면이다. 캔버스에 수천 개가 얹히면 그 문서는
// 여는 것부터 느려지고, 지우려 해도 무엇을 지워야 할지 보이지 않는다. 구조 화면이
// 개인 저장이던 시절 서버가 걸어 두었던 값을 그대로 op 경로로 옮겼다(0032).
const (
	maxNotes  = 200
	maxGroups = 100
)

func applyNoteAdd(doc *Document, op *Op) error {
	var p notePayload
	if err := decode(op, &p); err != nil {
		return err
	}
	id := strings.TrimSpace(p.ID)
	if id == "" {
		return invalid("메모 ID가 없습니다")
	}
	if doc.Note(id) != nil {
		// 같은 op가 재전송된 경우다. 중복 생성 대신 무시하는 것이 안전하다.
		return nil
	}
	if len(doc.Notes) >= maxNotes {
		return invalid("메모는 %d개까지 붙일 수 있습니다", maxNotes)
	}
	n := &Note{ID: id}
	if err := applyNotePatch(n, &p); err != nil {
		return err
	}
	doc.Notes = append(doc.Notes, n)
	return nil
}

func applyNoteUpdate(doc *Document, op *Op) error {
	var p notePayload
	if err := decode(op, &p); err != nil {
		return err
	}
	n := doc.Note(strings.TrimSpace(p.ID))
	if n == nil {
		return notFound("메모를 찾을 수 없습니다")
	}
	return applyNotePatch(n, &p)
}

func applyNoteDelete(doc *Document, op *Op) error {
	var p struct {
		ID string `json:"id"`
	}
	if err := decode(op, &p); err != nil {
		return err
	}
	if doc.Note(p.ID) == nil {
		return notFound("메모를 찾을 수 없습니다")
	}
	kept := doc.Notes[:0]
	for _, n := range doc.Notes {
		if n.ID == p.ID {
			continue
		}
		kept = append(kept, n)
	}
	doc.Notes = kept
	return nil
}

// ---------- 작은 유틸 ----------

func withoutString(list []string, name string) []string {
	out := list[:0]
	for _, v := range list {
		if strings.EqualFold(v, name) {
			continue
		}
		out = append(out, v)
	}
	return out
}

func containsFold(list []string, name string) bool {
	for _, v := range list {
		if strings.EqualFold(v, name) {
			return true
		}
	}
	return false
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]int, len(a))
	for _, v := range a {
		seen[strings.ToLower(v)]++
	}
	for _, v := range b {
		key := strings.ToLower(v)
		seen[key]--
		if seen[key] < 0 {
			return false
		}
	}
	return true
}

// ---------- 그룹 ----------

type groupPayload struct {
	ID    string   `json:"id"`
	Label *string  `json:"label,omitempty"`
	X     *float64 `json:"x,omitempty"`
	Y     *float64 `json:"y,omitempty"`
	W     *float64 `json:"w,omitempty"`
	H     *float64 `json:"h,omitempty"`
	Color *string  `json:"color,omitempty"`
}

// Group은 id로 그룹을 찾는다.
func (d *Document) Group(id string) *Group {
	for _, g := range d.Groups {
		if g.ID == id {
			return g
		}
	}
	return nil
}

func applyGroupAdd(doc *Document, op *Op) error {
	var p groupPayload
	if err := decode(op, &p); err != nil {
		return err
	}
	id := strings.TrimSpace(p.ID)
	if id == "" {
		return invalid("그룹 ID가 없습니다")
	}
	if doc.Group(id) != nil {
		// 같은 op가 재전송된 경우다. 메모와 같은 판단으로 무시한다.
		return nil
	}
	if len(doc.Groups) >= maxGroups {
		return invalid("묶음은 %d개까지 만들 수 있습니다", maxGroups)
	}
	g := &Group{ID: id, W: 320, H: 240}
	applyGroupPatch(g, &p)
	doc.Groups = append(doc.Groups, g)
	return nil
}

func applyGroupUpdate(doc *Document, op *Op) error {
	var p groupPayload
	if err := decode(op, &p); err != nil {
		return err
	}
	g := doc.Group(strings.TrimSpace(p.ID))
	if g == nil {
		return notFound("그룹을 찾을 수 없습니다")
	}
	applyGroupPatch(g, &p)
	return nil
}

// applyGroupPatch는 없는 필드를 "변경 없음"으로 둔다(문서 전체의 패치 규칙).
func applyGroupPatch(g *Group, p *groupPayload) {
	if p.Label != nil {
		g.Label = strings.TrimSpace(*p.Label)
	}
	if p.X != nil {
		g.X = *p.X
	}
	if p.Y != nil {
		g.Y = *p.Y
	}
	// 최소 크기를 두는 이유: 0에 가까운 사각형은 화면에서 보이지 않는데 문서에는
	// 남아 있어, 지울 수도 고를 수도 없는 유령이 된다.
	if p.W != nil {
		g.W = max(*p.W, 80)
	}
	if p.H != nil {
		g.H = max(*p.H, 60)
	}
	if p.Color != nil {
		g.Color = strings.TrimSpace(*p.Color)
	}
}

func applyGroupDelete(doc *Document, op *Op) error {
	var p struct {
		ID string `json:"id"`
	}
	if err := decode(op, &p); err != nil {
		return err
	}
	if doc.Group(p.ID) == nil {
		return notFound("그룹을 찾을 수 없습니다")
	}
	kept := doc.Groups[:0]
	for _, g := range doc.Groups {
		if g.ID == p.ID {
			continue
		}
		kept = append(kept, g)
	}
	doc.Groups = kept
	return nil
}

// ---------- 컬럼 순서 ----------

type columnMovePayload struct {
	Table string `json:"table"`
	Name  string `json:"name"`
	// To는 옮길 자리다(1부터). 범위를 벗어나면 양 끝으로 붙인다 —
	// 화면의 ↑↓ 버튼이 끝에서 한 번 더 눌리는 일은 흔하고, 그때 오류를 내면
	// 사용자는 자기가 뭘 잘못했는지 알 수 없다.
	To int `json:"to"`
}

func applyColumnMove(doc *Document, op *Op) error {
	var p columnMovePayload
	if err := decode(op, &p); err != nil {
		return err
	}
	tbl := doc.findTable(p.Table)
	if tbl == nil {
		return notFound("테이블 %s 을(를) 찾을 수 없습니다", p.Table)
	}
	from := -1
	for i, c := range tbl.Columns {
		if strings.EqualFold(c.Name, p.Name) {
			from = i
			break
		}
	}
	if from < 0 {
		return notFound("컬럼 %s 을(를) 찾을 수 없습니다", p.Name)
	}

	to := p.To - 1
	if to < 0 {
		to = 0
	}
	if to >= len(tbl.Columns) {
		to = len(tbl.Columns) - 1
	}
	if to == from {
		return nil
	}

	col := tbl.Columns[from]
	rest := append(tbl.Columns[:from:from], tbl.Columns[from+1:]...)
	moved := make([]*schema.Column, 0, len(tbl.Columns))
	moved = append(moved, rest[:to]...)
	moved = append(moved, col)
	moved = append(moved, rest[to:]...)
	tbl.Columns = moved

	// Position은 1부터 매기는 표시 순서다. 옮긴 뒤 다시 매기지 않으면
	// 생성된 DDL과 화면의 순서가 어긋난다.
	for i, c := range tbl.Columns {
		c.Position = i + 1
	}
	return nil
}
