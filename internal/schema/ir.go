// Package schema는 데이터베이스 스키마의 중간 표현(IR)과 그에 대한 연산을 제공한다.
//
// 이 IR은 앱 전체의 중심 자료구조다. introspect(실제 DB 읽기), ERD 편집, diff,
// 마이그레이션 SQL 생성, 버전 저장이 모두 동일한 표현을 공유하므로
// "ERD로 그린 것"과 "DB에 있는 것"을 같은 방식으로 비교할 수 있다.
package schema

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Shape은 스키마의 성격이다. 관계형이 아닌 DB는 테이블/컬럼 개념을 근사해 담는다.
type Shape string

const (
	ShapeRelational Shape = "relational" // MySQL, PostgreSQL, MS-SQL, Oracle, SQLite
	ShapeDocument   Shape = "document"   // MongoDB — 문서 샘플링으로 필드 추론
	ShapeKeyspace   Shape = "keyspace"   // Redis — 키 접두사 그룹 요약
)

// Schema는 한 데이터베이스의 구조 전체다.
type Schema struct {
	Dialect    string    `json:"dialect"`
	Shape      Shape     `json:"shape"`
	Name       string    `json:"name"`
	CapturedAt time.Time `json:"capturedAt"`

	Tables    []*Table    `json:"tables"`
	Views     []*View     `json:"views"`
	Enums     []*Enum     `json:"enums,omitempty"`
	Sequences []*Sequence `json:"sequences,omitempty"`

	// Notes는 introspect 중 발생한 부분적 실패나 근사 처리를 사용자에게 알리는 경고다.
	// 스키마를 못 읽은 것과 "권한이 없어 일부만 읽었다"를 구분하기 위해 존재한다.
	Notes []string `json:"notes,omitempty"`
}

// Table은 테이블(또는 컬렉션/키 그룹) 하나다.
type Table struct {
	// Namespace는 PostgreSQL schema, MS-SQL schema, Oracle owner에 해당한다.
	// MySQL/SQLite처럼 개념이 없으면 빈 문자열이다.
	Namespace string    `json:"namespace"`
	Name      string    `json:"name"`
	Comment   string    `json:"comment,omitempty"`
	Columns   []*Column `json:"columns"`

	PrimaryKey  *PrimaryKey       `json:"primaryKey"`
	Indexes     []*Index          `json:"indexes"`
	ForeignKeys []*ForeignKey     `json:"foreignKeys"`
	Checks      []*Check          `json:"checks"`
	Options     map[string]string `json:"options,omitempty"` // engine, charset, tablespace 등

	// 통계값. introspect 시점의 추정치이며 정확한 값이 아니다.
	RowEstimate int64 `json:"rowEstimate"`
	SizeBytes   int64 `json:"sizeBytes"`
}

// Key는 네임스페이스를 포함한 정규화된 테이블 식별자다. diff의 매칭 기준이다.
func (t *Table) Key() string {
	if t.Namespace == "" {
		return strings.ToLower(t.Name)
	}
	return strings.ToLower(t.Namespace + "." + t.Name)
}

// Display는 사용자에게 보여줄 이름이다.
func (t *Table) Display() string {
	if t.Namespace == "" {
		return t.Name
	}
	return t.Namespace + "." + t.Name
}

func (t *Table) Column(name string) *Column {
	for _, c := range t.Columns {
		if strings.EqualFold(c.Name, name) {
			return c
		}
	}
	return nil
}

// Column은 컬럼(또는 문서 필드) 하나다.
type Column struct {
	Name     string      `json:"name"`
	Position int         `json:"position"`
	Type     LogicalType `json:"type"`
	// RawType은 DB가 보고한 원본 타입 문자열이다. 같은 dialect로 되돌릴 때 손실을 막는다.
	RawType    string `json:"rawType"`
	Nullable   bool   `json:"nullable"`
	HasDefault bool   `json:"hasDefault"`
	Default    string `json:"default,omitempty"` // 원본 표현식 문자열
	Identity   bool   `json:"identity"`          // auto_increment / IDENTITY / serial
	Generated  string `json:"generated,omitempty"`
	// Domain은 이 컬럼의 타입이 어느 도메인(재사용 타입)에서 왔는지다.
	//
	// 설계 단계에서만 쓰는 값이라 introspect는 채우지 않고 지문(Fingerprint)에도
	// 넣지 않는다 — 넣으면 "도메인으로 정리했을 뿐인데 대상 DB와 구조가 다르다"가 된다.
	// 컬럼에 함께 두는 이유는 이름 변경·복사에 자연히 따라오기 때문이다.
	Domain    string `json:"domain,omitempty"`
	Collation string `json:"collation,omitempty"`
	Comment   string `json:"comment,omitempty"`

	// Presence는 문서 DB에서만 쓰인다. 샘플 문서 중 이 필드를 가진 비율(0~1)이다.
	Presence float64 `json:"presence,omitempty"`
}

// PrimaryKey는 기본키 제약이다.
type PrimaryKey struct {
	Name    string   `json:"name,omitempty"`
	Columns []string `json:"columns"`
}

// Index는 인덱스다. 기본키를 뒷받침하는 인덱스는 introspect에서 제외한다.
type Index struct {
	Name    string      `json:"name"`
	Columns []IndexPart `json:"columns"`
	Unique  bool        `json:"unique"`
	Type    string      `json:"type,omitempty"`  // btree, hash, gin, fulltext 등
	Where   string      `json:"where,omitempty"` // 부분 인덱스 조건
}

type IndexPart struct {
	Column     string `json:"column"`
	Descending bool   `json:"descending,omitempty"`
	// Expression은 컬럼이 아닌 식 기반 인덱스일 때 채워진다.
	Expression string `json:"expression,omitempty"`
}

func (i *Index) ColumnNames() []string {
	out := make([]string, 0, len(i.Columns))
	for _, p := range i.Columns {
		if p.Expression != "" {
			out = append(out, p.Expression)
			continue
		}
		out = append(out, p.Column)
	}
	return out
}

// ForeignKey는 외래키 제약이다. ERD의 관계선이 이 값에서 나온다.
type ForeignKey struct {
	Name         string   `json:"name"`
	Columns      []string `json:"columns"`
	RefNamespace string   `json:"refNamespace,omitempty"`
	RefTable     string   `json:"refTable"`
	RefColumns   []string `json:"refColumns"`
	OnDelete     string   `json:"onDelete,omitempty"` // CASCADE, SET NULL, RESTRICT, NO ACTION
	OnUpdate     string   `json:"onUpdate,omitempty"`
	Deferrable   bool     `json:"deferrable,omitempty"`
}

// RefKey는 참조 대상 테이블의 정규화된 식별자다.
func (f *ForeignKey) RefKey() string {
	if f.RefNamespace == "" {
		return strings.ToLower(f.RefTable)
	}
	return strings.ToLower(f.RefNamespace + "." + f.RefTable)
}

// Check는 체크 제약이다.
type Check struct {
	Name       string `json:"name"`
	Expression string `json:"expression"`
}

// View는 뷰다. 정의문 전체를 문자열로 보관한다.
type View struct {
	Namespace  string `json:"namespace,omitempty"`
	Name       string `json:"name"`
	Definition string `json:"definition,omitempty"`
	Comment    string `json:"comment,omitempty"`
}

func (v *View) Key() string {
	if v.Namespace == "" {
		return strings.ToLower(v.Name)
	}
	return strings.ToLower(v.Namespace + "." + v.Name)
}

// Enum은 PostgreSQL의 enum 타입처럼 이름 붙은 열거형이다.
type Enum struct {
	Namespace string   `json:"namespace,omitempty"`
	Name      string   `json:"name"`
	Values    []string `json:"values"`
}

func (e *Enum) Key() string {
	if e.Namespace == "" {
		return strings.ToLower(e.Name)
	}
	return strings.ToLower(e.Namespace + "." + e.Name)
}

// Sequence는 시퀀스다.
type Sequence struct {
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
	Start     int64  `json:"start"`
	Increment int64  `json:"increment"`
}

func (s *Sequence) Key() string {
	if s.Namespace == "" {
		return strings.ToLower(s.Name)
	}
	return strings.ToLower(s.Namespace + "." + s.Name)
}

// ---------- 조회 헬퍼 ----------

// Table은 정규화된 키로 테이블을 찾는다.
func (s *Schema) Table(key string) *Table {
	for _, t := range s.Tables {
		if t.Key() == strings.ToLower(key) {
			return t
		}
	}
	return nil
}

// TableMap은 키 → 테이블 맵을 만든다. diff에서 O(1) 조회를 위해 사용한다.
func (s *Schema) TableMap() map[string]*Table {
	m := make(map[string]*Table, len(s.Tables))
	for _, t := range s.Tables {
		m[t.Key()] = t
	}
	return m
}

// Sort는 스키마 내 모든 목록을 결정적 순서로 정렬한다.
// diff 결과와 저장된 JSON이 introspect 순서에 따라 흔들리지 않게 하려면 필수다.
func (s *Schema) Sort() {
	sort.SliceStable(s.Tables, func(i, j int) bool { return s.Tables[i].Key() < s.Tables[j].Key() })
	for _, t := range s.Tables {
		// 컬럼은 DB상의 물리 순서가 의미를 가지므로 Position 기준으로만 정렬한다.
		sort.SliceStable(t.Columns, func(i, j int) bool { return t.Columns[i].Position < t.Columns[j].Position })
		sort.SliceStable(t.Indexes, func(i, j int) bool { return strings.ToLower(t.Indexes[i].Name) < strings.ToLower(t.Indexes[j].Name) })
		sort.SliceStable(t.ForeignKeys, func(i, j int) bool {
			return strings.ToLower(t.ForeignKeys[i].Name) < strings.ToLower(t.ForeignKeys[j].Name)
		})
		sort.SliceStable(t.Checks, func(i, j int) bool { return strings.ToLower(t.Checks[i].Name) < strings.ToLower(t.Checks[j].Name) })
	}
	sort.SliceStable(s.Views, func(i, j int) bool { return s.Views[i].Key() < s.Views[j].Key() })
	sort.SliceStable(s.Enums, func(i, j int) bool { return s.Enums[i].Key() < s.Enums[j].Key() })
	sort.SliceStable(s.Sequences, func(i, j int) bool { return s.Sequences[i].Key() < s.Sequences[j].Key() })
}

// Stats는 스키마 규모 요약이다. 목록 화면과 diff 헤더에 쓴다.
type Stats struct {
	Tables      int   `json:"tables"`
	Columns     int   `json:"columns"`
	Indexes     int   `json:"indexes"`
	ForeignKeys int   `json:"foreignKeys"`
	Views       int   `json:"views"`
	RowEstimate int64 `json:"rowEstimate"`
	SizeBytes   int64 `json:"sizeBytes"`
}

func (s *Schema) Stats() Stats {
	st := Stats{Tables: len(s.Tables), Views: len(s.Views)}
	for _, t := range s.Tables {
		st.Columns += len(t.Columns)
		st.Indexes += len(t.Indexes)
		st.ForeignKeys += len(t.ForeignKeys)
		st.RowEstimate += t.RowEstimate
		st.SizeBytes += t.SizeBytes
	}
	return st
}

// AddNote는 introspect 중 부분 실패를 기록한다. 같은 메시지는 중복 기록하지 않는다.
func (s *Schema) AddNote(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	for _, n := range s.Notes {
		if n == msg {
			return
		}
	}
	s.Notes = append(s.Notes, msg)
}

// Clone은 스키마를 깊게 복사한다.
//
// 얕게 두면 사본을 고칠 때 원본까지 바뀐다. 이 구조체를 다루는 코드는 대부분
// "사본에 먼저 적용하고, 잘못되면 버린다"는 규칙으로 쓰여 있어서(편집 op, 설명 수정),
// 얕은 복사는 그 규칙을 조용히 무너뜨린다 — diff가 "바뀐 것이 없다"고 말하게 된다.
func (s *Schema) Clone() *Schema {
	if s == nil {
		return nil
	}
	out := *s
	out.Tables = make([]*Table, 0, len(s.Tables))
	for _, t := range s.Tables {
		out.Tables = append(out.Tables, t.Clone())
	}
	out.Views = append([]*View(nil), s.Views...)
	out.Enums = make([]*Enum, 0, len(s.Enums))
	for _, e := range s.Enums {
		c := *e
		c.Values = append([]string(nil), e.Values...)
		out.Enums = append(out.Enums, &c)
	}
	out.Sequences = append([]*Sequence(nil), s.Sequences...)
	out.Notes = append([]string(nil), s.Notes...)
	return &out
}

// Clone은 테이블을 깊게 복사한다.
func (t *Table) Clone() *Table {
	if t == nil {
		return nil
	}
	out := *t
	out.Columns = make([]*Column, 0, len(t.Columns))
	for _, c := range t.Columns {
		cc := *c
		if c.Type.Values != nil {
			cc.Type.Values = append([]string(nil), c.Type.Values...)
		}
		out.Columns = append(out.Columns, &cc)
	}
	if t.PrimaryKey != nil {
		pk := *t.PrimaryKey
		pk.Columns = append([]string(nil), t.PrimaryKey.Columns...)
		out.PrimaryKey = &pk
	}
	out.Indexes = make([]*Index, 0, len(t.Indexes))
	for _, i := range t.Indexes {
		ic := *i
		ic.Columns = append([]IndexPart(nil), i.Columns...)
		out.Indexes = append(out.Indexes, &ic)
	}
	out.ForeignKeys = make([]*ForeignKey, 0, len(t.ForeignKeys))
	for _, f := range t.ForeignKeys {
		fc := *f
		fc.Columns = append([]string(nil), f.Columns...)
		fc.RefColumns = append([]string(nil), f.RefColumns...)
		out.ForeignKeys = append(out.ForeignKeys, &fc)
	}
	out.Checks = make([]*Check, 0, len(t.Checks))
	for _, ck := range t.Checks {
		cc := *ck
		out.Checks = append(out.Checks, &cc)
	}
	if t.Options != nil {
		out.Options = make(map[string]string, len(t.Options))
		for k, v := range t.Options {
			out.Options[k] = v
		}
	}
	return &out
}

// Fingerprint는 스키마 구조의 해시 문자열을 만든다.
// 외부 편집 감지(드리프트)에서 "구조가 바뀌었는지"를 싸게 판별하기 위해 사용한다.
// 통계값(행 수, 크기)은 구조가 아니므로 제외한다.
func (s *Schema) Fingerprint() string {
	var b strings.Builder
	s.Sort()
	for _, t := range s.Tables {
		fmt.Fprintf(&b, "T:%s|%s\n", t.Key(), t.Comment)
		for _, c := range t.Columns {
			fmt.Fprintf(&b, " C:%s|%s|%t|%t|%s|%t|%s\n",
				strings.ToLower(c.Name), c.Type.Canonical(), c.Nullable, c.HasDefault, c.Default, c.Identity, c.Generated)
		}
		if t.PrimaryKey != nil {
			fmt.Fprintf(&b, " P:%s\n", strings.ToLower(strings.Join(t.PrimaryKey.Columns, ",")))
		}
		for _, i := range t.Indexes {
			fmt.Fprintf(&b, " I:%s|%t|%s|%s\n",
				strings.ToLower(i.Name), i.Unique, strings.ToLower(strings.Join(i.ColumnNames(), ",")), i.Where)
		}
		for _, f := range t.ForeignKeys {
			fmt.Fprintf(&b, " F:%s|%s|%s|%s|%s|%s\n",
				strings.ToLower(f.Name), strings.ToLower(strings.Join(f.Columns, ",")),
				f.RefKey(), strings.ToLower(strings.Join(f.RefColumns, ",")), f.OnDelete, f.OnUpdate)
		}
		for _, ck := range t.Checks {
			fmt.Fprintf(&b, " K:%s|%s\n", strings.ToLower(ck.Name), normalizeExpr(ck.Expression))
		}
	}
	for _, v := range s.Views {
		fmt.Fprintf(&b, "V:%s\n", v.Key())
	}
	for _, e := range s.Enums {
		fmt.Fprintf(&b, "E:%s|%s\n", e.Key(), strings.Join(e.Values, ","))
	}
	return hashString(b.String())
}
