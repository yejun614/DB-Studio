package sqlimport

import (
	"fmt"
	"strings"

	"dbstudio/internal/schema"
)

// Result는 스크립트를 읽어낸 결과다.
type Result struct {
	// Tables는 스크립트가 정의한 테이블이다. 같은 이름이 초안에 이미 있으면 덮어쓴다.
	Tables []*schema.Table `json:"tables"`
	Enums  []*schema.Enum  `json:"enums,omitempty"`
	// Views는 스크립트가 정의한 뷰다.
	//
	// 정의문(AS 뒤)을 **원문 그대로** 담는다. 뷰의 본문은 SELECT 한 덩어리이고,
	// 그것을 토큰으로 되살려 적으면 사람이 적어 둔 줄바꿈과 주석이 사라진다 —
	// 뷰에서 읽는 사람이 보는 것은 그 SQL 자체다.
	Views []*schema.View `json:"views,omitempty"`
	// Drops는 DROP TABLE로 지워진 테이블 키다.
	Drops []string `json:"drops,omitempty"`
	// ViewDrops는 DROP VIEW로 지워진 뷰 키다.
	ViewDrops []string `json:"viewDrops,omitempty"`
	// Notes는 해석하지 못했거나 무시한 것들이다.
	//
	// 이 목록이 이 패키지에서 가장 중요한 반환값이다. 파서가 완전하지 않다는 것을
	// 사용자가 알아야 결과를 검토할 수 있고, 무엇을 놓쳤는지 모르면 검토할 방법이 없다.
	Notes []string `json:"notes,omitempty"`
	// Statements는 읽은 문장 수다(주석과 빈 문장 제외).
	Statements int `json:"statements"`
}

// Parse는 DDL 스크립트를 읽는다.
//
// 오류를 반환하는 것은 "아무것도 읽어내지 못한 경우"뿐이다. 문장 하나를 이해하지
// 못했다고 전체를 실패시키면, 프로시저 정의 하나 때문에 테이블 서른 개를 못 읽는다.
func Parse(dialect, script string) (*Result, error) {
	p := &parser{
		toks:    lex(script),
		src:     script,
		dialect: dialect,
		res:     &Result{Tables: []*schema.Table{}},
		// 같은 스크립트 안에서 ALTER가 앞의 CREATE를 고칠 수 있어야 한다.
		byKey: map[string]*schema.Table{},
	}
	p.run()
	// 뷰만 담긴 스크립트도 읽어낸 것이 있는 스크립트다. 여기서 실패시키면
	// "뷰 정의만 따로 불러오기"가 아예 되지 않는다.
	empty := len(p.res.Tables) == 0 && len(p.res.Drops) == 0 &&
		len(p.res.Views) == 0 && len(p.res.ViewDrops) == 0
	if empty {
		if len(p.res.Notes) > 0 {
			return nil, fmt.Errorf("테이블·뷰 정의를 찾지 못했습니다: %s", strings.Join(p.res.Notes, " / "))
		}
		return nil, fmt.Errorf("테이블·뷰 정의를 찾지 못했습니다")
	}
	return p.res, nil
}

type parser struct {
	toks []token
	// src는 원문이다. 뷰 정의처럼 **글자 그대로** 담아야 하는 조각을 잘라낼 때 쓴다.
	src     string
	i       int
	dialect string
	res     *Result
	byKey   map[string]*schema.Table
}

// ---------- 토큰 이동 ----------

func (p *parser) done() bool { return p.i >= len(p.toks) }

func (p *parser) peek() token {
	if p.done() {
		return token{}
	}
	return p.toks[p.i]
}

func (p *parser) peekAt(n int) token {
	if p.i+n >= len(p.toks) {
		return token{}
	}
	return p.toks[p.i+n]
}

func (p *parser) next() token {
	t := p.peek()
	p.i++
	return t
}

// accept는 다음 토큰이 그 예약어면 소비한다.
func (p *parser) accept(words ...string) bool {
	for n, w := range words {
		if !p.peekAt(n).isWord(w) {
			return false
		}
	}
	p.i += len(words)
	return true
}

func (p *parser) acceptPunct(s string) bool {
	if p.peek().isPunct(s) {
		p.i++
		return true
	}
	return false
}

// skipToStatementEnd는 세미콜론까지 건너뛴다. 괄호 안의 세미콜론은 세지 않는다.
func (p *parser) skipToStatementEnd() {
	p.skipToStatementEndAt()
}

// skipToStatementEndAt은 문장 끝까지 건너뛰고 **끝 자리**를 돌려준다(세미콜론 앞,
// 없으면 원문의 끝). 원문을 그대로 잘라내야 하는 곳에서 쓴다.
func (p *parser) skipToStatementEndAt() int {
	depth := 0
	for !p.done() {
		t := p.next()
		if t.isPunct("(") {
			depth++
		} else if t.isPunct(")") {
			depth--
		} else if t.isPunct(";") && depth <= 0 {
			return t.pos
		}
	}
	return len(p.src)
}

// ---------- 문장 분기 ----------

func (p *parser) run() {
	for !p.done() {
		if p.acceptPunct(";") {
			continue
		}
		start := p.i
		p.res.Statements++
		switch {
		case p.accept("CREATE"):
			p.createStatement(start)
		case p.accept("ALTER"):
			p.alterStatement(start)
		case p.accept("DROP"):
			p.dropStatement(start)
		case p.accept("COMMENT", "ON"):
			p.commentOn(start)
		default:
			p.note(start, "")
			p.skipToStatementEnd()
		}
	}
}

// note는 해석하지 못한 문장을 기록한다. 앞 몇 낱말만 적어 무엇이었는지 알아볼 수
// 있게 하고, 문장 전체를 적지는 않는다(덤프 전체가 화면에 쏟아진다).
func (p *parser) note(start int, reason string) {
	head := []string{}
	for n := 0; n < 5 && start+n < len(p.toks); n++ {
		t := p.toks[start+n]
		if t.isPunct(";") || t.isPunct("(") {
			break
		}
		head = append(head, t.text)
	}
	label := strings.Join(head, " ")
	if label == "" {
		return
	}
	if reason == "" {
		reason = "ERD에 담을 수 없는 문장이라 건너뛰었습니다"
	}
	msg := fmt.Sprintf("%s… — %s", label, reason)
	for _, n := range p.res.Notes {
		if n == msg {
			return
		}
	}
	p.res.Notes = append(p.res.Notes, msg)
}

func (p *parser) createStatement(start int) {
	// CREATE [OR REPLACE] [GLOBAL|LOCAL] [TEMP|TEMPORARY|UNLOGGED] TABLE ...
	p.accept("OR", "REPLACE")
	p.accept("GLOBAL")
	p.accept("LOCAL")
	p.accept("TEMPORARY")
	p.accept("TEMP")
	p.accept("UNLOGGED")

	unique := p.accept("UNIQUE")
	// MATERIALIZED VIEW 도 뷰로 읽는다. 우리 IR 에는 그 구분이 없으므로 담을 때는
	// 같은 것이 되지만, 건너뛰는 것보다는 도면에 나타나는 편이 낫다.
	p.accept("MATERIALIZED")
	switch {
	case p.accept("TABLE"):
		p.createTable(start)
	case p.accept("INDEX"), p.accept("CLUSTERED", "INDEX"), p.accept("NONCLUSTERED", "INDEX"):
		p.createIndex(start, unique)
	case p.accept("VIEW"):
		p.createView(start)
	default:
		p.note(start, "")
		p.skipToStatementEnd()
	}
}

func (p *parser) createTable(start int) {
	p.accept("IF", "NOT", "EXISTS")
	ns, name, ok := p.qualifiedName()
	if !ok {
		p.note(start, "테이블 이름을 읽지 못했습니다")
		p.skipToStatementEnd()
		return
	}

	// CREATE TABLE x AS SELECT … 는 구조를 문장에서 알 수 없다.
	if p.peek().isWord("AS") {
		p.note(start, "CREATE TABLE … AS SELECT 는 결과 구조를 알 수 없어 건너뛰었습니다")
		p.skipToStatementEnd()
		return
	}
	if !p.acceptPunct("(") {
		// CREATE TABLE x LIKE y / PARTITION OF …
		p.note(start, "컬럼 정의가 없어 건너뛰었습니다")
		p.skipToStatementEnd()
		return
	}

	tbl := &schema.Table{
		Namespace: ns, Name: name,
		Columns: []*schema.Column{}, Indexes: []*schema.Index{},
		ForeignKeys: []*schema.ForeignKey{}, Checks: []*schema.Check{},
	}

	for !p.done() {
		if p.acceptPunct(")") {
			break
		}
		if p.acceptPunct(",") {
			continue
		}
		if !p.tableElement(tbl) {
			// 이해하지 못한 요소는 다음 쉼표까지 버린다. 컬럼 하나를 놓쳐도
			// 나머지 컬럼은 살아야 한다.
			p.skipElement()
		}
	}

	p.finishTable(tbl)
	p.upsert(tbl)
	// 테이블 옵션(ENGINE=, COMMENT=, PARTITION BY …)은 이 문장의 꼬리에 붙는다.
	p.tableTail(tbl)
	p.skipToStatementEnd()
}

// skipElement는 같은 괄호 깊이의 다음 쉼표(또는 닫는 괄호) 앞까지 건너뛴다.
func (p *parser) skipElement() {
	depth := 0
	for !p.done() {
		t := p.peek()
		if t.isPunct("(") {
			depth++
		} else if t.isPunct(")") {
			if depth == 0 {
				return
			}
			depth--
		} else if t.isPunct(",") && depth == 0 {
			return
		} else if t.isPunct(";") && depth == 0 {
			return
		}
		p.i++
	}
}

// tableElement는 컬럼 정의 또는 테이블 제약 하나를 읽는다.
func (p *parser) tableElement(tbl *schema.Table) bool {
	t := p.peek()

	// 이름 붙은 제약: CONSTRAINT x PRIMARY KEY (...)
	if t.isWord("CONSTRAINT") {
		p.i++
		cname := ""
		if p.peek().isName() {
			cname = p.next().val
		}
		return p.tableConstraint(tbl, cname)
	}
	if t.kind == tWord {
		switch t.upper {
		case "PRIMARY", "FOREIGN", "UNIQUE", "CHECK", "KEY", "INDEX", "FULLTEXT", "SPATIAL":
			return p.tableConstraint(tbl, "")
		case "LIKE", "EXCLUDE", "PERIOD":
			return false
		}
	}
	return p.columnDef(tbl)
}

func (p *parser) tableConstraint(tbl *schema.Table, name string) bool {
	switch {
	case p.accept("PRIMARY", "KEY"):
		// MySQL은 인덱스 종류를 붙일 수 있다: PRIMARY KEY USING BTREE (...)
		p.acceptIndexHints()
		cols := p.columnList()
		if len(cols) == 0 {
			return false
		}
		tbl.PrimaryKey = &schema.PrimaryKey{Name: name, Columns: cols}
		return true

	case p.accept("FOREIGN", "KEY"):
		// MySQL은 인덱스 이름을 여기에 넣을 수 있다: FOREIGN KEY idx_name (cols)
		if p.peek().isName() && !p.peek().isPunct("(") {
			if name == "" {
				name = p.peek().val
			}
			p.i++
		}
		cols := p.columnList()
		return p.foreignKeyTail(tbl, name, cols)

	case p.accept("UNIQUE"):
		p.accept("KEY")
		p.accept("INDEX")
		if p.peek().isName() {
			if name == "" {
				name = p.peek().val
			}
			p.i++
		}
		p.acceptIndexHints()
		cols := p.columnList()
		if len(cols) == 0 {
			return false
		}
		tbl.Indexes = append(tbl.Indexes, &schema.Index{
			Name: indexName(name, tbl, cols, true), Columns: indexParts(cols), Unique: true,
		})
		return true

	case p.accept("CHECK"):
		expr := p.parenExpr()
		if expr == "" {
			return false
		}
		// PostgreSQL의 NOT VALID 등 꼬리는 버린다.
		tbl.Checks = append(tbl.Checks, &schema.Check{
			Name: checkName(name, tbl), Expression: expr,
		})
		return true

	case p.accept("KEY"), p.accept("INDEX"), p.accept("FULLTEXT"), p.accept("SPATIAL"):
		p.accept("KEY")
		p.accept("INDEX")
		if p.peek().isName() {
			if name == "" {
				name = p.peek().val
			}
			p.i++
		}
		p.acceptIndexHints()
		cols := p.columnList()
		if len(cols) == 0 {
			return false
		}
		tbl.Indexes = append(tbl.Indexes, &schema.Index{
			Name: indexName(name, tbl, cols, false), Columns: indexParts(cols),
		})
		return true
	}
	return false
}

// acceptIndexHints는 컬럼 목록 앞에 끼어드는 인덱스 종류 지정을 소비한다.
// MySQL의 USING BTREE, MS-SQL의 CLUSTERED 가 여기에 해당한다. 소비하지 않으면
// 바로 뒤의 컬럼 목록을 읽지 못해 제약이 통째로 사라진다.
func (p *parser) acceptIndexHints() {
	for {
		switch {
		case p.accept("USING"):
			if p.peek().kind == tWord {
				p.i++
			}
		case p.accept("CLUSTERED"), p.accept("NONCLUSTERED"):
			// 물리 저장 방식이라 IR에 담지 않는다.
		default:
			return
		}
	}
}

func (p *parser) columnDef(tbl *schema.Table) bool {
	nameTok := p.peek()
	if !nameTok.isName() {
		return false
	}
	p.i++
	col := &schema.Column{Name: nameTok.val, Nullable: true, Position: len(tbl.Columns) + 1}

	raw := p.typeName()
	if raw == "" {
		return false
	}
	col.RawType = raw
	col.Type = schema.ParseType(p.dialect, raw)

	// 컬럼 뒤에 붙는 수식어들. 순서는 DB마다 다르므로 순서를 가정하지 않고
	// 알아보는 것이 없을 때까지 계속 읽는다.
	for !p.done() {
		t := p.peek()
		if t.isPunct(",") || t.isPunct(")") || t.isPunct(";") {
			break
		}
		switch {
		case p.accept("NOT", "NULL"):
			col.Nullable = false
		case p.accept("NULL"):
			col.Nullable = true
		case p.accept("PRIMARY", "KEY"):
			// 컬럼에 붙은 기본키. 복합키는 테이블 제약으로만 쓸 수 있으므로 단일 컬럼이다.
			tbl.PrimaryKey = &schema.PrimaryKey{Columns: []string{col.Name}}
			col.Nullable = false
		case p.accept("UNIQUE"):
			p.accept("KEY")
			tbl.Indexes = append(tbl.Indexes, &schema.Index{
				Name:    indexName("", tbl, []string{col.Name}, true),
				Columns: indexParts([]string{col.Name}), Unique: true,
			})
		case p.accept("DEFAULT"):
			col.Default = p.expr()
			col.HasDefault = col.Default != ""
		case p.accept("AUTO_INCREMENT"), p.accept("AUTOINCREMENT"):
			col.Identity = true
		case p.accept("IDENTITY"):
			col.Identity = true
			p.acceptParenTail()
		case p.accept("GENERATED"):
			// GENERATED { ALWAYS | BY DEFAULT } AS { IDENTITY [(…)] | (expr) STORED }
			p.accept("ALWAYS")
			p.accept("BY", "DEFAULT")
			p.accept("AS")
			if p.accept("IDENTITY") {
				col.Identity = true
				p.acceptParenTail()
			} else if e := p.parenExpr(); e != "" {
				col.Generated = e
				p.accept("STORED")
				p.accept("VIRTUAL")
			}
		case p.accept("COMMENT"):
			if p.peek().kind == tString {
				col.Comment = p.next().val
			}
		case p.accept("COLLATE"):
			if p.peek().isName() {
				col.Collation = p.next().val
			}
		case p.accept("CHECK"):
			if e := p.parenExpr(); e != "" {
				tbl.Checks = append(tbl.Checks, &schema.Check{
					Name: checkName("", tbl), Expression: e,
				})
			}
		case p.accept("REFERENCES"):
			// 컬럼에 인라인으로 붙은 외래키.
			p.inlineReference(tbl, col.Name)
		case p.accept("CONSTRAINT"):
			// 컬럼 뒤에 이름 붙은 제약이 이어지는 형태.
			cname := ""
			if p.peek().isName() {
				cname = p.next().val
			}
			if p.accept("CHECK") {
				if e := p.parenExpr(); e != "" {
					tbl.Checks = append(tbl.Checks, &schema.Check{Name: checkName(cname, tbl), Expression: e})
				}
			} else if p.accept("REFERENCES") {
				p.inlineReferenceNamed(tbl, cname, col.Name)
			} else if p.accept("PRIMARY", "KEY") {
				tbl.PrimaryKey = &schema.PrimaryKey{Name: cname, Columns: []string{col.Name}}
				col.Nullable = false
			} else if p.accept("UNIQUE") {
				tbl.Indexes = append(tbl.Indexes, &schema.Index{
					Name:    indexName(cname, tbl, []string{col.Name}, true),
					Columns: indexParts([]string{col.Name}), Unique: true,
				})
			}
		default:
			// 알아보지 못한 수식어(UNSIGNED는 타입에서 이미 먹었고, ZEROFILL·
			// CHARACTER SET·ON UPDATE 등이 남는다). 한 토큰씩 흘려보낸다.
			p.i++
		}
	}

	tbl.Columns = append(tbl.Columns, col)
	return true
}

// inlineReference는 컬럼 뒤 REFERENCES 절을 외래키로 만든다.
func (p *parser) inlineReference(tbl *schema.Table, colName string) {
	p.inlineReferenceNamed(tbl, "", colName)
}

func (p *parser) inlineReferenceNamed(tbl *schema.Table, name, colName string) {
	ns, refTable, ok := p.qualifiedName()
	if !ok {
		return
	}
	refCols := p.columnList()
	fk := &schema.ForeignKey{
		Name: name, Columns: []string{colName},
		RefNamespace: ns, RefTable: refTable, RefColumns: refCols,
	}
	p.referenceActions(fk)
	if fk.Name == "" {
		fk.Name = fmt.Sprintf("fk_%s_%s", tbl.Name, colName)
	}
	tbl.ForeignKeys = append(tbl.ForeignKeys, fk)
}

// foreignKeyTail은 FOREIGN KEY (cols) 다음의 REFERENCES 절을 읽는다.
func (p *parser) foreignKeyTail(tbl *schema.Table, name string, cols []string) bool {
	if len(cols) == 0 || !p.accept("REFERENCES") {
		return false
	}
	ns, refTable, ok := p.qualifiedName()
	if !ok {
		return false
	}
	refCols := p.columnList()
	fk := &schema.ForeignKey{
		Name: name, Columns: cols,
		RefNamespace: ns, RefTable: refTable, RefColumns: refCols,
	}
	p.referenceActions(fk)
	if fk.Name == "" {
		fk.Name = fmt.Sprintf("fk_%s_%s", tbl.Name, strings.Join(cols, "_"))
	}
	tbl.ForeignKeys = append(tbl.ForeignKeys, fk)
	return true
}

// referenceActions는 ON DELETE / ON UPDATE / DEFERRABLE 을 읽는다.
func (p *parser) referenceActions(fk *schema.ForeignKey) {
	for !p.done() {
		switch {
		case p.accept("ON", "DELETE"):
			fk.OnDelete = p.refAction()
		case p.accept("ON", "UPDATE"):
			fk.OnUpdate = p.refAction()
		case p.accept("DEFERRABLE"):
			fk.Deferrable = true
		case p.accept("NOT", "DEFERRABLE"):
			fk.Deferrable = false
		case p.accept("INITIALLY"):
			p.i++ // DEFERRED | IMMEDIATE
		case p.accept("MATCH"):
			p.i++ // FULL | PARTIAL | SIMPLE
		default:
			return
		}
	}
}

func (p *parser) refAction() string {
	switch {
	case p.accept("CASCADE"):
		return "CASCADE"
	case p.accept("SET", "NULL"):
		return "SET NULL"
	case p.accept("SET", "DEFAULT"):
		return "SET DEFAULT"
	case p.accept("RESTRICT"):
		return "RESTRICT"
	case p.accept("NO", "ACTION"):
		return "NO ACTION"
	}
	return ""
}

// tableTail은 컬럼 정의 괄호 뒤의 테이블 옵션에서 주석만 건진다.
// 나머지(ENGINE, CHARSET, TABLESPACE)는 ERD가 표현하지 않는다.
func (p *parser) tableTail(tbl *schema.Table) {
	depth := 0
	for !p.done() {
		t := p.peek()
		if t.isPunct("(") {
			depth++
		} else if t.isPunct(")") {
			depth--
		} else if t.isPunct(";") && depth <= 0 {
			return
		} else if depth == 0 && t.isWord("COMMENT") {
			p.i++
			p.acceptPunct("=")
			if p.peek().kind == tOperator && p.peek().text == "=" {
				p.i++
			}
			if p.peek().kind == tString {
				tbl.Comment = p.next().val
				continue
			}
			continue
		}
		p.i++
	}
}
