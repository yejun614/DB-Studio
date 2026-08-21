package sqlimport

import (
	"fmt"
	"strings"

	"dbstudio/internal/schema"
)

// key는 IR과 같은 규칙으로 테이블 식별자를 만든다.
// 두 곳의 규칙이 갈리면 "덮어쓰기"가 "새 테이블 추가"로 조용히 바뀐다.
func key(namespace, name string) string {
	if namespace == "" {
		return strings.ToLower(name)
	}
	return strings.ToLower(namespace + "." + name)
}

func display(namespace, name string) string {
	if namespace == "" {
		return name
	}
	return namespace + "." + name
}

// upsert는 읽어낸 테이블을 결과에 넣는다. 같은 이름이 이미 있으면 교체한다 —
// 스크립트 안에서 같은 테이블을 두 번 정의하면 뒤의 것이 이긴다.
func (p *parser) upsert(tbl *schema.Table) {
	k := tbl.Key()
	if old, ok := p.byKey[k]; ok {
		for i, t := range p.res.Tables {
			if t == old {
				p.res.Tables[i] = tbl
				p.byKey[k] = tbl
				return
			}
		}
	}
	p.res.Tables = append(p.res.Tables, tbl)
	p.byKey[k] = tbl
	// 같은 스크립트에서 만들었다가 지웠다가 다시 만드는 경우, 앞의 DROP은 무효다.
	p.res.Drops = removeString(p.res.Drops, k)
}

func (p *parser) removeTable(k string) {
	for i, t := range p.res.Tables {
		if t.Key() == k {
			p.res.Tables = append(p.res.Tables[:i], p.res.Tables[i+1:]...)
			return
		}
	}
}

// finishTable은 읽기를 마친 테이블을 정리한다.
func (p *parser) finishTable(tbl *schema.Table) {
	// 기본키 컬럼은 NULL일 수 없다. 문장에 NOT NULL이 없어도 DB가 그렇게 만든다.
	// 이것을 맞추지 않으면 불러온 직후의 초안이 실제 DB와 다르다고 나온다.
	if tbl.PrimaryKey != nil {
		for _, name := range tbl.PrimaryKey.Columns {
			if col := tbl.Column(name); col != nil {
				col.Nullable = false
			}
		}
	}
	for i, c := range tbl.Columns {
		c.Position = i + 1
	}
	// 기본키를 그대로 베낀 UNIQUE 인덱스는 introspect가 걸러내는 것과 같은 이유로
	// 여기서도 뺀다. 남겨 두면 불러오기 직후에 없던 차이가 생긴다.
	if tbl.PrimaryKey != nil {
		kept := tbl.Indexes[:0]
		for _, idx := range tbl.Indexes {
			if idx.Unique && sameCols(idx.ColumnNames(), tbl.PrimaryKey.Columns) {
				continue
			}
			kept = append(kept, idx)
		}
		tbl.Indexes = kept
	}
}

// indexName은 이름 없는 인덱스에 이름을 지어 준다.
// diff와 DDL 생성이 이름으로 인덱스를 짝지으므로 비워 둘 수 없다.
func indexName(name string, tbl *schema.Table, cols []string, unique bool) string {
	if strings.TrimSpace(name) != "" {
		return name
	}
	prefix := "ix"
	if unique {
		prefix = "uq"
	}
	return fmt.Sprintf("%s_%s_%s", prefix, tbl.Name, strings.Join(cols, "_"))
}

// checkName은 이름 없는 체크 제약에 이름을 지어 준다.
// 번호는 그 테이블 안에서만 세므로 스크립트를 다시 읽어도 같은 이름이 나온다.
func checkName(name string, tbl *schema.Table) string {
	if strings.TrimSpace(name) != "" {
		return name
	}
	return fmt.Sprintf("ck_%s_%d", tbl.Name, len(tbl.Checks)+1)
}

func indexParts(cols []string) []schema.IndexPart {
	out := make([]schema.IndexPart, 0, len(cols))
	for _, c := range cols {
		out = append(out, schema.IndexPart{Column: c})
	}
	return out
}

// replaceIndex는 같은 이름의 인덱스를 갈아 끼운다.
func replaceIndex(tbl *schema.Table, idx *schema.Index) {
	for i, old := range tbl.Indexes {
		if strings.EqualFold(old.Name, idx.Name) {
			tbl.Indexes[i] = idx
			return
		}
	}
	tbl.Indexes = append(tbl.Indexes, idx)
}

// dropColumn은 컬럼과 그 컬럼을 쓰던 제약을 함께 지운다.
// 컬럼만 지우면 없는 컬럼을 가리키는 인덱스가 남아, 나중에 만들 수 없는 DDL이 된다.
func dropColumn(tbl *schema.Table, name string) {
	kept := tbl.Columns[:0]
	for _, c := range tbl.Columns {
		if !strings.EqualFold(c.Name, name) {
			kept = append(kept, c)
		}
	}
	tbl.Columns = kept
	for i, c := range tbl.Columns {
		c.Position = i + 1
	}

	if tbl.PrimaryKey != nil {
		tbl.PrimaryKey.Columns = removeFold(tbl.PrimaryKey.Columns, name)
		if len(tbl.PrimaryKey.Columns) == 0 {
			tbl.PrimaryKey = nil
		}
	}
	idx := tbl.Indexes[:0]
	for _, i := range tbl.Indexes {
		if !containsFold(i.ColumnNames(), name) {
			idx = append(idx, i)
		}
	}
	tbl.Indexes = idx

	fks := tbl.ForeignKeys[:0]
	for _, f := range tbl.ForeignKeys {
		if !containsFold(f.Columns, name) {
			fks = append(fks, f)
		}
	}
	tbl.ForeignKeys = fks
}

// dropConstraint는 이름으로 제약을 지운다. 어느 종류인지는 문장에 없으므로
// 이름이 맞는 것을 모두 본다.
func dropConstraint(tbl *schema.Table, name string) {
	if tbl.PrimaryKey != nil && strings.EqualFold(tbl.PrimaryKey.Name, name) {
		tbl.PrimaryKey = nil
	}
	idx := tbl.Indexes[:0]
	for _, i := range tbl.Indexes {
		if !strings.EqualFold(i.Name, name) {
			idx = append(idx, i)
		}
	}
	tbl.Indexes = idx

	fks := tbl.ForeignKeys[:0]
	for _, f := range tbl.ForeignKeys {
		if !strings.EqualFold(f.Name, name) {
			fks = append(fks, f)
		}
	}
	tbl.ForeignKeys = fks

	cks := tbl.Checks[:0]
	for _, c := range tbl.Checks {
		if !strings.EqualFold(c.Name, name) {
			cks = append(cks, c)
		}
	}
	tbl.Checks = cks
}

func containsFold(list []string, name string) bool {
	for _, v := range list {
		if strings.EqualFold(v, name) {
			return true
		}
	}
	return false
}

func removeFold(list []string, name string) []string {
	out := list[:0]
	for _, v := range list {
		if !strings.EqualFold(v, name) {
			out = append(out, v)
		}
	}
	return out
}

func removeString(list []string, name string) []string {
	out := list[:0]
	for _, v := range list {
		if v != name {
			out = append(out, v)
		}
	}
	return out
}

func sameCols(a, b []string) bool {
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
