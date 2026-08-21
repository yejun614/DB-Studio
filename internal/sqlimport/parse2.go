package sqlimport

import (
	"fmt"
	"strings"

	"dbstudio/internal/schema"
)

// ---------- 이름과 식 읽기 ----------

// qualifiedName은 [db.][schema.]name 을 읽고 (namespace, name)을 돌려준다.
//
// 세 토막(db.schema.table)이면 가운데를 네임스페이스로 본다. 이 앱의 IR은
// 데이터베이스를 커넥션이 이미 정하고 있으므로 담을 자리가 없고, 버리는 편이
// 잘못 담는 것보다 낫다.
func (p *parser) qualifiedName() (string, string, bool) {
	if !p.peek().isName() {
		return "", "", false
	}
	parts := []string{p.next().val}
	for p.acceptPunct(".") {
		if !p.peek().isName() {
			break
		}
		parts = append(parts, p.next().val)
	}
	switch len(parts) {
	case 1:
		return "", parts[0], true
	case 2:
		return parts[0], parts[1], true
	default:
		return parts[len(parts)-2], parts[len(parts)-1], true
	}
}

// typeName은 컬럼 타입을 원문 그대로 읽는다.
//
// 파싱해서 다시 조립하지 않는 이유: RawType은 "DB가 말한 그대로"여야 한다.
// `DOUBLE PRECISION`, `TIMESTAMP WITH TIME ZONE`, `NUMERIC(10,2)`, `INT UNSIGNED`
// 처럼 여러 낱말과 괄호가 섞이고, 조립하다 한 조각을 흘리면 타입이 조용히 바뀐다.
func (p *parser) typeName() string {
	if p.peek().kind != tWord && p.peek().kind != tIdent {
		return ""
	}
	var b strings.Builder
	b.WriteString(p.next().text)

	// 괄호가 붙는 길이/정밀도
	if p.peek().isPunct("(") {
		b.WriteString(p.balanced())
	}

	// 여러 낱말로 된 타입의 뒷부분. 여기 없는 낱말이 나오면 타입이 끝난 것이다.
	for !p.done() {
		t := p.peek()
		if t.kind != tWord {
			break
		}
		switch t.upper {
		case "PRECISION", "VARYING", "UNSIGNED", "SIGNED", "ZEROFILL":
			b.WriteString(" " + p.next().text)
			if p.peek().isPunct("(") {
				b.WriteString(p.balanced())
			}
		case "CHARACTER", "NATIONAL", "BINARY", "TIME", "TIMESTAMP", "DAY", "YEAR", "SECOND":
			// CHARACTER VARYING / WITH TIME ZONE / INTERVAL DAY TO SECOND 등의 꼬리.
			// 앞에 이미 낱말이 있을 때만 이어 붙인다.
			b.WriteString(" " + p.next().text)
		case "WITH", "WITHOUT":
			// WITH TIME ZONE / WITHOUT TIME ZONE
			if p.peekAt(1).isWord("TIME") && p.peekAt(2).isWord("ZONE") {
				b.WriteString(" " + p.next().text + " " + p.next().text + " " + p.next().text)
				continue
			}
			return b.String()
		case "ARRAY":
			b.WriteString(" " + p.next().text)
		case "TO":
			// INTERVAL DAY TO SECOND
			b.WriteString(" " + p.next().text)
		default:
			return b.String()
		}
	}
	// PostgreSQL 배열 표기 int[]
	for p.peek().isPunct("[") {
		b.WriteString(p.balanced())
	}
	return b.String()
}

// columnList는 (a, b DESC, lower(c)) 형태의 목록을 읽어 이름만 돌려준다.
func (p *parser) columnList() []string {
	if !p.acceptPunct("(") {
		return nil
	}
	out := []string{}
	depth := 0
	cur := []token{}
	flush := func() {
		if len(cur) == 0 {
			return
		}
		// 정렬 방향과 길이 지정(MySQL의 col(10))은 컬럼 이름이 아니다.
		name := cur[0].val
		out = append(out, name)
		cur = nil
	}
	for !p.done() {
		t := p.next()
		if t.isPunct("(") {
			depth++
			cur = append(cur, t)
			continue
		}
		if t.isPunct(")") {
			if depth == 0 {
				flush()
				return out
			}
			depth--
			cur = append(cur, t)
			continue
		}
		if t.isPunct(",") && depth == 0 {
			flush()
			continue
		}
		cur = append(cur, t)
	}
	flush()
	return out
}

// balanced는 현재 위치의 괄호 한 쌍을 원문 그대로 돌려준다.
func (p *parser) balanced() string {
	open := p.peek()
	if !open.isPunct("(") && !(open.kind == tPunct && open.text == "[") {
		return ""
	}
	var b strings.Builder
	depth := 0
	for !p.done() {
		t := p.next()
		if t.isPunct("(") {
			depth++
		} else if t.isPunct(")") {
			depth--
		}
		if b.Len() > 0 && needsSpace(t) {
			b.WriteString(" ")
		}
		b.WriteString(t.text)
		if depth == 0 {
			break
		}
	}
	return b.String()
}

// parenExpr는 괄호 안의 식을 원문에 가깝게 돌려준다(바깥 괄호 제외).
func (p *parser) parenExpr() string {
	raw := p.balanced()
	if len(raw) < 2 {
		return ""
	}
	return strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(raw, "("), ")"))
}

// expr는 DEFAULT 뒤에 오는 값을 읽는다.
//
// 완전한 식 파서가 아니다. 리터럴 하나 또는 함수 호출 하나를 읽고 멈춘다 —
// DEFAULT 뒤에 오는 것은 실무에서 거의 전부 그 둘이고, 더 복잡한 식은
// RawType처럼 원문을 그대로 옮겨 담는 것이 안전하다.
func (p *parser) expr() string {
	t := p.peek()
	if t.kind == tPunct && t.text == "(" {
		return p.balanced()
	}
	if t.kind == tString || t.kind == tNumber {
		p.i++
		return t.text
	}
	if t.kind == tOperator && (t.text == "-" || t.text == "+") {
		p.i++
		return t.text + p.expr()
	}
	if t.kind == tWord || t.kind == tIdent {
		p.i++
		out := t.text
		// CURRENT_TIMESTAMP(6), now(), nextval('seq')
		if p.peek().isPunct("(") {
			out += p.balanced()
			return out
		}
		// CURRENT_TIMESTAMP ON UPDATE … 의 ON 은 기본값이 아니다.
		for p.peek().kind == tWord {
			switch p.peek().upper {
			case "NULL", "TRUE", "FALSE":
				out += " " + p.next().text
			default:
				return out
			}
		}
		return out
	}
	return ""
}

// acceptParenTail은 IDENTITY(1,1) 처럼 뒤에 붙는 괄호를 버린다.
func (p *parser) acceptParenTail() {
	if p.peek().isPunct("(") {
		p.balanced()
	}
}

func needsSpace(t token) bool {
	return t.kind == tWord || t.kind == tIdent || t.kind == tString
}

// ---------- ALTER / DROP / COMMENT ----------

func (p *parser) alterStatement(start int) {
	if !p.accept("TABLE") {
		p.note(start, "")
		p.skipToStatementEnd()
		return
	}
	p.accept("IF", "EXISTS")
	p.accept("ONLY")
	ns, name, ok := p.qualifiedName()
	if !ok {
		p.note(start, "테이블 이름을 읽지 못했습니다")
		p.skipToStatementEnd()
		return
	}
	tbl := p.byKey[key(ns, name)]
	if tbl == nil {
		// 스크립트 안에 정의가 없는 테이블을 고치는 문장. 무엇을 고치는지는
		// 알지만 원래 모습을 모르므로 반영할 수 없다.
		//
		// 여기서 빈 테이블을 만들어 붙이면 초안의 기존 테이블이 컬럼 하나짜리로
		// 덮어써진다 — 불러오기가 데이터를 지우는 셈이다. 그래서 알린 뒤 넘긴다.
		p.note(start, fmt.Sprintf("이 스크립트에 %s 의 정의가 없어 반영하지 못했습니다", display(ns, name)))
		p.skipToStatementEnd()
		return
	}

	for !p.done() {
		if p.acceptPunct(";") {
			return
		}
		if p.acceptPunct(",") {
			continue
		}
		if !p.alterAction(start, tbl) {
			p.skipElement()
			if p.peek().isPunct(";") {
				p.i++
				return
			}
		}
	}
}

func (p *parser) alterAction(start int, tbl *schema.Table) bool {
	switch {
	case p.accept("ADD"):
		p.accept("COLUMN")
		p.accept("IF", "NOT", "EXISTS")
		if p.peek().isWord("CONSTRAINT") || isConstraintWord(p.peek()) {
			return p.tableElement(tbl)
		}
		return p.columnDef(tbl)

	case p.accept("DROP", "COLUMN"), p.accept("DROP"):
		p.accept("IF", "EXISTS")
		if p.accept("CONSTRAINT") || p.accept("INDEX") || p.accept("KEY") {
			if p.peek().isName() {
				dropConstraint(tbl, p.next().val)
			}
			p.accept("CASCADE")
			p.accept("RESTRICT")
			return true
		}
		if p.accept("PRIMARY", "KEY") {
			tbl.PrimaryKey = nil
			return true
		}
		if p.peek().isName() {
			dropColumn(tbl, p.next().val)
			p.accept("CASCADE")
			p.accept("RESTRICT")
			return true
		}
		return false

	case p.accept("RENAME", "TO"), p.accept("RENAME"):
		p.accept("TO")
		if _, name, ok := p.qualifiedName(); ok {
			delete(p.byKey, tbl.Key())
			tbl.Name = name
			p.byKey[tbl.Key()] = tbl
			return true
		}
		return false

	case p.accept("ALTER", "COLUMN"), p.accept("MODIFY", "COLUMN"), p.accept("MODIFY"):
		return p.alterColumn(tbl)

	case p.accept("COMMENT"):
		p.acceptEq()
		if p.peek().kind == tString {
			tbl.Comment = p.next().val
			return true
		}
		return false
	}
	p.note(start, "ALTER TABLE 의 일부를 해석하지 못했습니다")
	return false
}

// alterColumn은 기존 컬럼의 타입·NULL 여부·기본값을 고친다.
func (p *parser) alterColumn(tbl *schema.Table) bool {
	if !p.peek().isName() {
		return false
	}
	name := p.next().val
	col := tbl.Column(name)
	if col == nil {
		return false
	}
	switch {
	case p.accept("SET", "NOT", "NULL"):
		col.Nullable = false
	case p.accept("DROP", "NOT", "NULL"):
		col.Nullable = true
	case p.accept("SET", "DEFAULT"):
		col.Default = p.expr()
		col.HasDefault = col.Default != ""
	case p.accept("DROP", "DEFAULT"):
		col.Default, col.HasDefault = "", false
	case p.accept("TYPE"), p.accept("SET", "DATA", "TYPE"):
		if raw := p.typeName(); raw != "" {
			col.RawType = raw
			col.Type = schema.ParseType(p.dialect, raw)
		}
		p.acceptUsingTail()
	default:
		// MySQL의 MODIFY col <타입> <수식어…> 형태.
		if raw := p.typeName(); raw != "" {
			col.RawType = raw
			col.Type = schema.ParseType(p.dialect, raw)
			// NOT NULL / DEFAULT 등은 이어지는 토큰이 알려준다.
			for !p.done() {
				t := p.peek()
				if t.isPunct(",") || t.isPunct(";") {
					break
				}
				switch {
				case p.accept("NOT", "NULL"):
					col.Nullable = false
				case p.accept("NULL"):
					col.Nullable = true
				case p.accept("DEFAULT"):
					col.Default = p.expr()
					col.HasDefault = col.Default != ""
				case p.accept("COMMENT"):
					if p.peek().kind == tString {
						col.Comment = p.next().val
					}
				case p.accept("AUTO_INCREMENT"):
					col.Identity = true
				default:
					p.i++
				}
			}
			return true
		}
		return false
	}
	return true
}

func (p *parser) acceptUsingTail() {
	if p.accept("USING") {
		for !p.done() && !p.peek().isPunct(",") && !p.peek().isPunct(";") {
			p.i++
		}
	}
}

func (p *parser) acceptEq() {
	if p.peek().kind == tOperator && p.peek().text == "=" {
		p.i++
	}
}

func (p *parser) dropStatement(start int) {
	if !p.accept("TABLE") {
		// DROP INDEX / VIEW / SEQUENCE 등. ERD에 담기지 않거나(뷰) 정보가
		// 부족해(인덱스 소속 테이블) 반영하지 않는다.
		p.note(start, "")
		p.skipToStatementEnd()
		return
	}
	p.accept("IF", "EXISTS")
	for !p.done() {
		ns, name, ok := p.qualifiedName()
		if !ok {
			break
		}
		k := key(ns, name)
		p.res.Drops = append(p.res.Drops, k)
		// 같은 스크립트 안에서 만들었다가 지운 테이블은 만들지 않은 것과 같다.
		if _, ok := p.byKey[k]; ok {
			delete(p.byKey, k)
			p.removeTable(k)
		}
		if !p.acceptPunct(",") {
			break
		}
	}
	p.skipToStatementEnd()
}

// commentOn은 PostgreSQL의 COMMENT ON TABLE/COLUMN 을 읽는다.
func (p *parser) commentOn(start int) {
	switch {
	case p.accept("TABLE"):
		ns, name, ok := p.qualifiedName()
		if !ok {
			break
		}
		if tbl := p.byKey[key(ns, name)]; tbl != nil && p.accept("IS") && p.peek().kind == tString {
			tbl.Comment = p.next().val
		}
	case p.accept("COLUMN"):
		// schema.table.column — 마지막이 컬럼이다.
		parts := []string{}
		for p.peek().isName() {
			parts = append(parts, p.next().val)
			if !p.acceptPunct(".") {
				break
			}
		}
		if len(parts) >= 2 && p.accept("IS") && p.peek().kind == tString {
			body := p.next().val
			colName := parts[len(parts)-1]
			ns := ""
			if len(parts) >= 3 {
				ns = parts[len(parts)-3]
			}
			if tbl := p.byKey[key(ns, parts[len(parts)-2])]; tbl != nil {
				if col := tbl.Column(colName); col != nil {
					col.Comment = body
				}
			}
		}
	default:
		p.note(start, "")
	}
	p.skipToStatementEnd()
}

// createIndex는 CREATE INDEX 문을 읽어 대상 테이블에 붙인다.
func (p *parser) createIndex(start int, unique bool) {
	p.accept("IF", "NOT", "EXISTS")
	idxName := ""
	if p.peek().isName() && !p.peek().isWord("ON") {
		idxName = p.next().val
	}
	if !p.accept("ON") {
		p.note(start, "인덱스의 대상 테이블을 읽지 못했습니다")
		p.skipToStatementEnd()
		return
	}
	ns, name, ok := p.qualifiedName()
	if !ok {
		p.note(start, "인덱스의 대상 테이블을 읽지 못했습니다")
		p.skipToStatementEnd()
		return
	}
	tbl := p.byKey[key(ns, name)]
	if tbl == nil {
		p.note(start, fmt.Sprintf("이 스크립트에 %s 의 정의가 없어 인덱스를 반영하지 못했습니다",
			display(ns, name)))
		p.skipToStatementEnd()
		return
	}
	p.acceptIndexHints()
	cols := p.columnList()
	if len(cols) == 0 {
		p.skipToStatementEnd()
		return
	}
	idx := &schema.Index{
		Name: indexName(idxName, tbl, cols, unique), Columns: indexParts(cols), Unique: unique,
	}
	// 부분 인덱스 조건은 diff에서 같고 다름을 가르므로 반드시 담아야 한다.
	if p.accept("WHERE") {
		var b strings.Builder
		for !p.done() && !p.peek().isPunct(";") {
			t := p.next()
			if b.Len() > 0 && needsSpace(t) {
				b.WriteString(" ")
			}
			b.WriteString(t.text)
		}
		idx.Where = strings.TrimSpace(b.String())
	}
	replaceIndex(tbl, idx)
	p.skipToStatementEnd()
}

func isConstraintWord(t token) bool {
	if t.kind != tWord {
		return false
	}
	switch t.upper {
	case "PRIMARY", "FOREIGN", "UNIQUE", "CHECK", "KEY", "INDEX", "FULLTEXT", "SPATIAL":
		return true
	}
	return false
}
