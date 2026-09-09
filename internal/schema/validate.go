package schema

import (
	"fmt"
	"sort"
	"strings"
)

// 문제의 무게. 둘로만 나눈다.
//
//	error   이대로는 만들 수 없거나, 만들어져도 뜻이 깨진 것
//	warning 만들어지고 돌아가지만 나중에 아플 것
//
// 셋 이상으로 나누지 않는 이유: "정보" 같은 칸을 두면 거기에 온갖 것이 쌓이고,
// 그러면 목록 전체가 읽히지 않는다. 고쳐야 하는가, 알고만 있으면 되는가 —
// 사람이 하는 판단은 그 둘뿐이다.
const (
	SeverityError   = "error"
	SeverityWarning = "warning"
)

// Issue는 설계에서 찾은 문제 하나다.
//
// 어디의 문제인지를 키로 담는다. 화면은 이 값으로 카드·컬럼·관계선을 강조하므로,
// 사람이 읽는 문장(Message)과 기계가 찾는 자리(Table·Column·FK)를 따로 둔다 —
// 문장에서 표 이름을 다시 뽑아내게 하면 이름에 점이 든 표에서 어긋난다.
type Issue struct {
	Severity string `json:"severity"`
	// Rule은 규칙의 이름이다. 화면이 같은 규칙을 묶어 보여줄 때 쓴다.
	Rule    string `json:"rule"`
	Message string `json:"message"`
	// Hint는 "그래서 어떻게 하는가"다. 없으면 비운다 — 뻔한 것에 굳이 붙이면
	// 목록이 두 배로 길어지고, 길어진 목록은 읽히지 않는다.
	Hint string `json:"hint,omitempty"`

	Table  string `json:"table,omitempty"`  // 표 키(소문자, namespace.name)
	Column string `json:"column,omitempty"` // 컬럼 이름(소문자)
	FK     string `json:"fk,omitempty"`     // 외래키 이름
	Index  string `json:"index,omitempty"`
	View   string `json:"view,omitempty"` // 뷰 키(소문자)
}

// Validate는 초안에서 찾을 수 있는 문제를 모은다.
//
// 왜 마이그레이션 계획과 따로 두는가: 계획은 "지금 DB를 이렇게 바꾼다"이고 대상
// DB가 있어야 만들어진다. 여기서 잡는 것들은 대상이 없어도, 첫 표를 그리는
// 순간부터 이미 틀린 것들이다 — 없는 표를 가리키는 외래키는 어느 DB에 붙여도
// 틀렸다. 설계하는 동안 보여줘야 하는 것은 이쪽이다.
//
// 순서는 정해 둔다(무게 → 표 → 규칙). 같은 초안에서 목록의 순서가 매번 바뀌면
// 무엇이 새로 생긴 문제인지 알 수 없다.
func Validate(sc *Schema) []Issue {
	if sc == nil {
		return nil
	}
	v := &validator{sc: sc, dialect: strings.ToLower(strings.TrimSpace(sc.Dialect))}
	v.byKey = sc.TableMap()

	v.checkDuplicateTables()
	for _, t := range sc.Tables {
		v.checkTableShape(t)
		v.checkPrimaryKey(t)
		v.checkIndexes(t)
		v.checkForeignKeys(t)
		v.checkColumns(t)
	}
	v.checkViews()
	v.checkCycles()

	sort.SliceStable(v.out, func(i, j int) bool {
		a, b := v.out[i], v.out[j]
		if a.Severity != b.Severity {
			return a.Severity == SeverityError // error 가 먼저다
		}
		if a.Table != b.Table {
			return a.Table < b.Table
		}
		if a.Rule != b.Rule {
			return a.Rule < b.Rule
		}
		return a.Column < b.Column
	})
	return v.out
}

type validator struct {
	sc      *Schema
	dialect string
	byKey   map[string]*Table
	out     []Issue
}

func (v *validator) err(is Issue) {
	is.Severity = SeverityError
	v.out = append(v.out, is)
}

func (v *validator) warn(is Issue) {
	is.Severity = SeverityWarning
	v.out = append(v.out, is)
}

// ---------- 방언이 지원하지 않는 것 ----------

// hasForeignKeys는 이 방언에 외래키가 있는지다.
//
// 여기서 "지원하지 않는다"고 잡아 두는 이유: 계획을 만들 때도 걸러지고 경고가
// 남지만(ddl.go), 그때는 이미 관계를 다 그려 놓은 뒤다. 그리는 동안 알려줘야
// 참조 무결성을 어디서 지킬지 함께 정할 수 있다.
func (v *validator) hasForeignKeys() bool { return v.dialect != "clickhouse" }
func (v *validator) hasIndexes() bool     { return v.dialect != "clickhouse" }

// ---------- 표의 모양 ----------

func (v *validator) checkDuplicateTables() {
	seen := map[string][]string{}
	for _, t := range v.sc.Tables {
		seen[t.Key()] = append(seen[t.Key()], t.Display())
	}
	for key, names := range seen {
		if len(names) < 2 {
			continue
		}
		v.err(Issue{
			Rule: "duplicate-table", Table: key,
			Message: fmt.Sprintf("표 이름 %s 이 %d번 쓰였습니다", names[0], len(names)),
			Hint:    "같은 이름의 표는 하나만 만들어집니다. 하나를 지우거나 이름을 바꾸세요",
		})
	}
}

func (v *validator) checkTableShape(t *Table) {
	if len(t.Columns) == 0 {
		v.err(Issue{
			Rule: "table-no-columns", Table: t.Key(),
			Message: fmt.Sprintf("%s 에 컬럼이 없습니다", t.Display()),
			Hint:    "컬럼이 없는 표는 만들 수 없습니다",
		})
		return
	}

	seen := map[string]int{}
	for _, c := range t.Columns {
		seen[strings.ToLower(c.Name)]++
	}
	for name, n := range seen {
		if n < 2 {
			continue
		}
		v.err(Issue{
			Rule: "duplicate-column", Table: t.Key(), Column: name,
			Message: fmt.Sprintf("%s 에 컬럼 %s 이 %d번 있습니다", t.Display(), name, n),
			Hint:    "대소문자만 다른 이름도 같은 이름으로 봅니다(대부분의 DB가 그렇습니다)",
		})
	}
}

func (v *validator) checkColumns(t *Table) {
	for _, c := range t.Columns {
		if strings.TrimSpace(c.Name) == "" {
			v.err(Issue{
				Rule: "column-no-name", Table: t.Key(),
				Message: fmt.Sprintf("%s 에 이름 없는 컬럼이 있습니다", t.Display()),
			})
			continue
		}
		// 제로값("")과 TypeUnknown 을 함께 본다. BaseType 은 문자열이라 아무것도
		// 정하지 않은 컬럼은 "unknown" 이 아니라 빈 문자열이다 — 하나만 보면
		// 타입이 아예 없는 컬럼이 그대로 빠져나간다.
		if (c.Type.Base == "" || c.Type.Base == TypeUnknown) && strings.TrimSpace(c.RawType) == "" {
			v.err(Issue{
				Rule: "column-no-type", Table: t.Key(), Column: strings.ToLower(c.Name),
				Message: fmt.Sprintf("%s.%s 에 타입이 없습니다", t.Display(), c.Name),
			})
		}
		if c.Type.Base == TypeEnum && len(c.Type.Values) == 0 && c.Type.EnumName == "" {
			v.err(Issue{
				Rule: "enum-no-values", Table: t.Key(), Column: strings.ToLower(c.Name),
				Message: fmt.Sprintf("%s.%s 은 열거형인데 값이 하나도 없습니다", t.Display(), c.Name),
			})
		}
	}
}

// ---------- 기본키 ----------

func (v *validator) checkPrimaryKey(t *Table) {
	pk := t.PrimaryKey
	if pk == nil || len(pk.Columns) == 0 {
		if len(t.Columns) == 0 {
			return
		}
		// ClickHouse 는 정렬 키가 그 자리라 따로 본다.
		if v.dialect == "clickhouse" {
			if strings.TrimSpace(t.Options["order_by"]) == "" {
				v.warn(Issue{
					Rule: "clickhouse-no-sort-key", Table: t.Key(),
					Message: fmt.Sprintf("%s 에 기본키도 정렬 키도 없습니다", t.Display()),
					Hint:    "ORDER BY tuple() 로 만들어지고, 그 표는 조회가 언제나 전수 조사입니다",
				})
			}
			return
		}
		v.warn(Issue{
			Rule: "no-primary-key", Table: t.Key(),
			Message: fmt.Sprintf("%s 에 기본키가 없습니다", t.Display()),
			Hint:    "한 행을 집어낼 방법이 없어 데이터 화면에서 수정할 수 없고, 다른 표가 참조할 수도 없습니다",
		})
		return
	}

	for _, name := range pk.Columns {
		col := t.Column(name)
		if col == nil {
			v.err(Issue{
				Rule: "pk-missing-column", Table: t.Key(), Column: strings.ToLower(name),
				Message: fmt.Sprintf("%s 의 기본키가 없는 컬럼 %s 을 가리킵니다", t.Display(), name),
			})
			continue
		}
		if col.Nullable {
			v.err(Issue{
				Rule: "pk-nullable-column", Table: t.Key(), Column: strings.ToLower(col.Name),
				Message: fmt.Sprintf("%s.%s 은 기본키인데 NULL 을 허용합니다", t.Display(), col.Name),
				Hint:    "기본키 컬럼은 NULL 일 수 없습니다. NOT NULL 로 바꾸거나 기본키에서 빼세요",
			})
		}
	}

	dup := map[string]bool{}
	for _, name := range pk.Columns {
		low := strings.ToLower(name)
		if dup[low] {
			v.err(Issue{
				Rule: "pk-duplicate-column", Table: t.Key(), Column: low,
				Message: fmt.Sprintf("%s 의 기본키에 컬럼 %s 이 두 번 들어 있습니다", t.Display(), name),
			})
		}
		dup[low] = true
	}
}

// ---------- 인덱스 ----------

func (v *validator) checkIndexes(t *Table) {
	if len(t.Indexes) == 0 {
		return
	}
	if !v.hasIndexes() {
		v.warn(Issue{
			Rule: "index-unsupported", Table: t.Key(),
			Message: fmt.Sprintf("%s 의 인덱스는 ClickHouse 에서 만들어지지 않습니다", t.Display()),
			Hint:    "ClickHouse 에서 읽기 성능을 정하는 것은 정렬 키(ORDER BY)입니다",
		})
	}

	byCols := map[string][]string{}
	for _, ix := range t.Indexes {
		for _, part := range ix.Columns {
			if part.Expression != "" {
				continue // 식 기반 인덱스는 컬럼을 견줄 수 없다
			}
			if t.Column(part.Column) == nil {
				v.err(Issue{
					Rule: "index-missing-column", Table: t.Key(),
					Column: strings.ToLower(part.Column), Index: ix.Name,
					Message: fmt.Sprintf("%s 의 인덱스 %s 가 없는 컬럼 %s 을 가리킵니다",
						t.Display(), ix.Name, part.Column),
				})
			}
		}
		key := strings.ToLower(strings.Join(ix.ColumnNames(), ", "))
		if key == "" {
			continue
		}
		byCols[key] = append(byCols[key], ix.Name)
	}
	for cols, names := range byCols {
		if len(names) < 2 {
			continue
		}
		sort.Strings(names)
		v.warn(Issue{
			Rule: "duplicate-index", Table: t.Key(), Index: names[0],
			Message: fmt.Sprintf("%s 에 같은 컬럼(%s)을 덮는 인덱스가 %d개 있습니다: %s",
				t.Display(), cols, len(names), strings.Join(names, ", ")),
			Hint: "인덱스는 쓰기를 느리게 하고 자리를 차지합니다. 하나만 남기세요",
		})
	}
}

// ---------- 외래키 ----------

func (v *validator) checkForeignKeys(t *Table) {
	if len(t.ForeignKeys) == 0 {
		return
	}
	if !v.hasForeignKeys() {
		v.warn(Issue{
			Rule: "fk-unsupported", Table: t.Key(),
			Message: fmt.Sprintf("%s 의 관계는 ClickHouse 에서 만들어지지 않습니다", t.Display()),
			Hint:    "ClickHouse 에는 외래키가 없습니다. 참조 무결성은 넣는 쪽에서 지켜야 합니다",
		})
	}
	for _, fk := range t.ForeignKeys {
		v.checkForeignKey(t, fk)
	}
}

func (v *validator) checkForeignKey(t *Table, fk *ForeignKey) {
	at := Issue{Table: t.Key(), FK: fk.Name}
	if len(fk.Columns) > 0 {
		at.Column = strings.ToLower(fk.Columns[0])
	}

	// 1) 이 표에 그 컬럼이 있는가.
	var locals []*Column
	for _, name := range fk.Columns {
		col := t.Column(name)
		if col == nil {
			is := at
			is.Column = strings.ToLower(name)
			is.Rule = "fk-missing-local-column"
			is.Message = fmt.Sprintf("%s 의 관계 %s 가 없는 컬럼 %s 에서 나갑니다",
				t.Display(), fkLabel(fk), name)
			v.err(is)
			return
		}
		locals = append(locals, col)
	}
	if len(locals) == 0 {
		is := at
		is.Rule = "fk-no-columns"
		is.Message = fmt.Sprintf("%s 의 관계 %s 에 컬럼이 없습니다", t.Display(), fkLabel(fk))
		v.err(is)
		return
	}

	// 2) 가리키는 표가 있는가.
	ref := v.byKey[fk.RefKey()]
	if ref == nil {
		is := at
		is.Rule = "fk-missing-table"
		is.Message = fmt.Sprintf("%s 의 관계 %s 가 없는 표 %s 를 가리킵니다",
			t.Display(), fkLabel(fk), refDisplay(fk))
		is.Hint = "그 표를 만들거나, 이름이 바뀐 것이라면 관계를 고쳐야 합니다"
		v.err(is)
		return
	}

	// 3) 컬럼 수가 맞는가.
	if len(fk.RefColumns) != len(fk.Columns) {
		is := at
		is.Rule = "fk-column-count"
		is.Message = fmt.Sprintf("%s 의 관계 %s 는 컬럼 %d개에서 나가는데 %d개를 가리킵니다",
			t.Display(), fkLabel(fk), len(fk.Columns), len(fk.RefColumns))
		v.err(is)
		return
	}

	// 4) 가리키는 컬럼이 있는가, 타입이 같은가.
	for i, name := range fk.RefColumns {
		col := ref.Column(name)
		if col == nil {
			is := at
			is.Rule = "fk-missing-ref-column"
			is.Message = fmt.Sprintf("%s 의 관계 %s 가 %s 에 없는 컬럼 %s 을 가리킵니다",
				t.Display(), fkLabel(fk), ref.Display(), name)
			v.err(is)
			return
		}
		// 타입이 다르면 대부분의 DB가 외래키 자체를 거절한다. 받아 주는 DB에서도
		// 견주는 값이 조용히 변환되어, 있는 행을 못 찾는 일이 생긴다.
		if a, b := locals[i].Type.Canonical(), col.Type.Canonical(); a != b {
			is := at
			is.Column = strings.ToLower(locals[i].Name)
			is.Rule = "fk-type-mismatch"
			is.Message = fmt.Sprintf("%s.%s(%s) 과 %s.%s(%s) 의 타입이 다릅니다",
				t.Display(), locals[i].Name,
				RenderType(v.dialect, locals[i].Type, locals[i].RawType),
				ref.Display(), col.Name,
				RenderType(v.dialect, col.Type, col.RawType))
			is.Hint = "관계로 묶는 두 컬럼은 타입이 같아야 합니다"
			v.err(is)
		}
	}

	// 5) 가리키는 쪽이 유일한가. 유일하지 않은 값을 가리키는 외래키는 만들 수 없다.
	if v.hasForeignKeys() && !v.isUnique(ref, fk.RefColumns) {
		is := at
		is.Rule = "fk-target-not-unique"
		is.Message = fmt.Sprintf("%s 의 관계 %s 가 가리키는 %s(%s) 이 유일하지 않습니다",
			t.Display(), fkLabel(fk), ref.Display(), strings.Join(fk.RefColumns, ", "))
		is.Hint = "가리키는 쪽은 기본키이거나 유니크 인덱스가 있어야 합니다"
		v.err(is)
	}

	// 6) SET NULL 인데 컬럼이 NOT NULL 이면 지우는 순간 실패한다.
	if isSetNull(fk.OnDelete) || isSetNull(fk.OnUpdate) {
		for _, col := range locals {
			if col.Nullable {
				continue
			}
			is := at
			is.Column = strings.ToLower(col.Name)
			is.Rule = "fk-set-null-not-nullable"
			is.Message = fmt.Sprintf("%s.%s 은 NOT NULL 인데 관계 %s 가 SET NULL 로 되어 있습니다",
				t.Display(), col.Name, fkLabel(fk))
			is.Hint = "부모 행을 지우는 순간 실패합니다. 컬럼을 NULL 허용으로 바꾸거나 다른 동작을 고르세요"
			v.err(is)
		}
	}

	// 7) 나가는 쪽에 인덱스가 없으면 부모를 지우거나 고칠 때 자식 표를 전수 조사한다.
	if v.hasIndexes() && !v.hasLeadingIndex(t, fk.Columns) {
		is := at
		is.Rule = "fk-no-index"
		is.Message = fmt.Sprintf("%s(%s) 에 인덱스가 없습니다 (관계 %s)",
			t.Display(), strings.Join(fk.Columns, ", "), fkLabel(fk))
		is.Hint = "부모 행을 지우거나 고칠 때마다 이 표를 전수 조사합니다. 표가 커지면 그때 아픕니다"
		v.warn(is)
	}

	// 8) 자기 자신을 가리키는데 NOT NULL 이면 첫 행을 넣을 수 없다.
	if ref.Key() == t.Key() {
		allNotNull := true
		for _, col := range locals {
			if col.Nullable {
				allNotNull = false
				break
			}
		}
		if allNotNull {
			is := at
			is.Rule = "fk-self-not-nullable"
			is.Message = fmt.Sprintf("%s 가 자기 자신을 NOT NULL 로 가리킵니다 (관계 %s)",
				t.Display(), fkLabel(fk))
			is.Hint = "가리킬 행이 아직 없으므로 첫 행을 넣을 수 없습니다. NULL 허용으로 두세요"
			v.err(is)
		}
	}
}

// isUnique는 그 컬럼 묶음이 유일함을 보장받는지다(기본키이거나 유니크 인덱스).
//
// 순서는 보지 않는다. (a, b) 에 유니크 인덱스가 있으면 (b, a) 를 가리키는 외래키도
// 만들 수 있다 — 유일한 것은 묶음이지 순서가 아니다.
func (v *validator) isUnique(t *Table, cols []string) bool {
	want := lowerSet(cols)
	if pk := t.PrimaryKey; pk != nil && sameSet(lowerSet(pk.Columns), want) {
		return true
	}
	for _, ix := range t.Indexes {
		if ix.Unique && sameSet(lowerSet(ix.ColumnNames()), want) {
			return true
		}
	}
	return false
}

// hasLeadingIndex는 그 컬럼들로 **시작하는** 인덱스가 있는지다.
//
// 앞부분만 맞아도 쓸 수 있는 것이 B-tree 인덱스의 성질이다. (a, b, c) 인덱스는
// (a) 로 찾는 질의에도 쓰인다. 기본키도 인덱스로 센다.
func (v *validator) hasLeadingIndex(t *Table, cols []string) bool {
	want := lowerList(cols)
	if len(want) == 0 {
		return true
	}
	if pk := t.PrimaryKey; pk != nil && coversPrefix(lowerList(pk.Columns), want) {
		return true
	}
	for _, ix := range t.Indexes {
		if coversPrefix(lowerList(ix.ColumnNames()), want) {
			return true
		}
	}
	return false
}

// ---------- 뷰 ----------

func (v *validator) checkViews() {
	seen := map[string]int{}
	for _, view := range v.sc.Views {
		seen[view.Key()]++
		if strings.TrimSpace(view.Definition) == "" {
			v.err(Issue{
				Rule: "view-no-definition", View: view.Key(),
				Message: fmt.Sprintf("뷰 %s 에 정의(SELECT)가 없습니다", view.Name),
				Hint:    "정의가 없는 뷰는 만들 수 없습니다",
			})
		}
		if view.Materialized && strings.TrimSpace(view.Target) == "" {
			v.warn(Issue{
				Rule: "matview-no-target", View: view.Key(),
				Message: fmt.Sprintf("구체화 뷰 %s 에 결과를 써 넣을 표가 없습니다", view.Name),
				Hint:    "결과를 뷰가 자기 안에 담습니다. 대상 표를 따로 두려면 TO 절이 필요합니다",
			})
		}
		if v.byKey[view.Key()] != nil {
			v.err(Issue{
				Rule: "view-name-taken", View: view.Key(), Table: view.Key(),
				Message: fmt.Sprintf("%s 이 표와 뷰에 같은 이름으로 있습니다", view.Key()),
			})
		}
	}
	for key, n := range seen {
		if n < 2 {
			continue
		}
		v.err(Issue{
			Rule: "duplicate-view", View: key,
			Message: fmt.Sprintf("뷰 이름 %s 이 %d번 쓰였습니다", key, n),
		})
	}
}

// ---------- 순환 ----------

// checkCycles는 NOT NULL 로만 이어진 참조의 고리를 찾는다.
//
// 왜 이것만 보는가: 참조의 고리 자체는 흔하고 문제도 아니다(부서와 부서장).
// 고리를 이루는 모든 컬럼이 NOT NULL 일 때만 아프다 — 어느 쪽을 먼저 넣어도
// 가리킬 행이 없어서, 그 표들에는 **첫 행을 넣을 수 없다.** 오류 없이 만들어지고
// 데이터를 넣으려는 순간에야 드러난다.
func (v *validator) checkCycles() {
	if !v.hasForeignKeys() {
		return
	}
	// 굳은 간선만 남긴 그래프. 자기 참조는 따로 보므로 넣지 않는다.
	next := map[string][]string{}
	for _, t := range v.sc.Tables {
		for _, fk := range t.ForeignKeys {
			ref := v.byKey[fk.RefKey()]
			if ref == nil || ref.Key() == t.Key() || len(fk.Columns) == 0 {
				continue
			}
			hard := true
			for _, name := range fk.Columns {
				col := t.Column(name)
				if col == nil || col.Nullable {
					hard = false
					break
				}
			}
			if hard {
				next[t.Key()] = append(next[t.Key()], ref.Key())
			}
		}
	}

	const (
		white = 0
		grey  = 1
		black = 2
	)
	color := map[string]int{}
	var stack []string
	var found [][]string

	var walk func(key string)
	walk = func(key string) {
		color[key] = grey
		stack = append(stack, key)
		for _, to := range next[key] {
			switch color[to] {
			case white:
				walk(to)
			case grey:
				// stack 에서 to 부터 잘라 낸 것이 고리다.
				for i, k := range stack {
					if k == to {
						found = append(found, append([]string{}, stack[i:]...))
						break
					}
				}
			}
		}
		stack = stack[:len(stack)-1]
		color[key] = black
	}
	keys := make([]string, 0, len(v.sc.Tables))
	for _, t := range v.sc.Tables {
		keys = append(keys, t.Key())
	}
	sort.Strings(keys)
	for _, key := range keys {
		if color[key] == white {
			walk(key)
		}
	}

	// 같은 고리를 여러 번 적지 않는다.
	reported := map[string]bool{}
	for _, cycle := range found {
		sorted := append([]string{}, cycle...)
		sort.Strings(sorted)
		id := strings.Join(sorted, ">")
		if reported[id] {
			continue
		}
		reported[id] = true
		path := strings.Join(cycle, " → ") + " → " + cycle[0]
		for _, key := range cycle {
			v.err(Issue{
				Rule: "fk-hard-cycle", Table: key,
				Message: fmt.Sprintf("%s 이 NOT NULL 관계의 고리에 있습니다: %s", key, path),
				Hint:    "어느 쪽을 먼저 넣어도 가리킬 행이 없어 첫 행을 넣을 수 없습니다. 고리의 한 곳을 NULL 허용으로 두세요",
			})
		}
	}
}

// ---------- 유틸 ----------

func fkLabel(fk *ForeignKey) string {
	if fk == nil || strings.TrimSpace(fk.Name) == "" {
		return "(이름 없음)"
	}
	return fk.Name
}

func refDisplay(fk *ForeignKey) string {
	if fk.RefNamespace == "" {
		return fk.RefTable
	}
	return fk.RefNamespace + "." + fk.RefTable
}

func isSetNull(action string) bool {
	return strings.EqualFold(strings.Join(strings.Fields(action), " "), "SET NULL")
}

func lowerSet(in []string) map[string]bool {
	out := make(map[string]bool, len(in))
	for _, s := range in {
		out[strings.ToLower(s)] = true
	}
	return out
}

func lowerList(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		out = append(out, strings.ToLower(s))
	}
	return out
}

func sameSet(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

// coversPrefix는 have 의 앞부분이 want 를 모두 담는지다.
func coversPrefix(have, want []string) bool {
	if len(want) > len(have) {
		return false
	}
	head := lowerSet(have[:len(want)])
	for _, w := range want {
		if !head[w] {
			return false
		}
	}
	return true
}
