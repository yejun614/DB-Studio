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
	// notedCluster는 ON CLUSTER 안내를 이미 적었는지다(스크립트당 한 번).
	notedCluster bool
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
	// 구체화 뷰라는 사실을 넘긴다. 평범한 뷰로 담으면 문장은 통과하고 **다른 것**이
	// 만들어진다 — 구체화 뷰는 INSERT 때 대상 표에 써 넣고 평범한 뷰는 읽을 때
	// 계산하므로, 그때부터 대상 표에 행이 쌓이지 않는다.
	materialized := p.accept("MATERIALIZED")
	switch {
	case p.accept("TABLE"):
		p.createTable(start)
	case p.accept("INDEX"), p.accept("CLUSTERED", "INDEX"), p.accept("NONCLUSTERED", "INDEX"):
		p.createIndex(start, unique)
	case p.accept("VIEW"):
		p.createView(start, materialized)
	case p.accept("DICTIONARY"):
		// 사전은 표도 뷰도 아니다(외부 원본을 메모리에 얹어 dictGet 으로 찾는 것).
		// ERD 에 담을 자리가 없으므로 건너뛰되, 무엇을 건너뛰었는지는 적는다.
		p.note(start, "사전(DICTIONARY)은 표나 뷰가 아니라 ERD 에 담지 않습니다. "+
			"이 사전을 쓰는 뷰의 정의는 그대로 남습니다")
		p.skipToStatementEnd()
	default:
		p.note(start, "")
		p.skipToStatementEnd()
	}
}

// tableFromLikeAs는 `CREATE TABLE b AS a` 형태에서 원본 표의 컬럼을 베낀다.
//
// ClickHouse 클러스터에서는 표가 둘씩 다닌다 — 데이터를 담는 로컬 표와, 그 앞에
// 서서 샤드로 흩뿌리는 Distributed 표. 뒤쪽은 컬럼을 적지 않고 앞쪽의 것을
// 그대로 쓴다.
//
//	CREATE TABLE listen_events AS listen_events_local
//	ENGINE = Distributed(mupid, currentDatabase(), listen_events_local, cityHash64(user_id));
//
// 이것을 못 읽으면 도면에서 **표의 절반이 사라진다.** 그리고 사라진 쪽이 응용이
// 실제로 읽고 쓰는 표다.
//
// 베끼는 것은 컬럼뿐이다. 엔진·정렬 키·파티션은 두 표가 서로 다르고(그쪽은 뒤에
// 오는 절에서 읽는다), 기본키도 옮기지 않는다 — Distributed 표에는 정렬 키가 없다.
//
// AS 뒤가 SELECT 나 괄호면 이 형태가 아니다(그때는 nil 을 돌려준다).
func (p *parser) tableFromLikeAs(start int, ns, name string) *schema.Table {
	save := p.i
	p.i++ // AS

	if p.peek().isWord("SELECT") || p.peek().isWord("WITH") || p.peek().isPunct("(") {
		p.i = save
		return nil
	}
	srcNS, srcName, ok := p.qualifiedName()
	if !ok {
		p.i = save
		return nil
	}

	tbl := &schema.Table{
		Namespace: ns, Name: name,
		Columns: []*schema.Column{}, Indexes: []*schema.Index{},
		ForeignKeys: []*schema.ForeignKey{}, Checks: []*schema.Check{},
	}
	src := p.byKey[key(srcNS, srcName)]
	if src == nil && srcNS == "" {
		src = p.byKey[key(ns, srcName)]
	}
	if src == nil {
		// 원본이 이 스크립트에 없다. 컬럼 없이라도 도면에 두는 편이 낫다 —
		// 없는 표로 두면 그 표를 가리키는 다른 정의들이 허공을 가리킨다.
		p.note(start, "원본 표 "+srcName+" 을 이 스크립트에서 찾지 못해 컬럼 없이 만들었습니다. "+
			"원본을 함께 불러오면 컬럼이 채워집니다")
		return tbl
	}
	for i, c := range src.Columns {
		cp := *c
		cp.Position = i + 1
		tbl.Columns = append(tbl.Columns, &cp)
	}
	return tbl
}

// acceptOnCluster는 ClickHouse 의 ON CLUSTER 절을 지나간다.
//
// 이름 뒤에 붙어서, 이것을 모르면 그 뒤의 괄호도 AS 도 못 찾는다. 클러스터 DDL 은
// 거의 모든 문장에 이 절을 달고 있으므로, 모르면 스크립트가 통째로 안 읽힌다.
//
// 클러스터 이름은 표마다 다른 것이 아니라 **어디에 적용하는가**라서, 표 설정으로
// 담지 않는다. 이 앱에서는 커넥션 옵션 `cluster` 가 그 자리이고, 거기 적어 두면
// 내보내는 DDL 에 ON CLUSTER 가 붙는다.
func (p *parser) acceptOnCluster(start int) {
	if !p.accept("ON", "CLUSTER") {
		return
	}
	name := ""
	if p.peek().isName() {
		name = p.next().val
	}
	// 스크립트마다 한 번만 적는다. 문장마다 적으면 같은 줄이 열다섯 개 쌓인다.
	if !p.notedCluster {
		p.notedCluster = true
		p.note(start, "ON CLUSTER "+name+" 는 지나갑니다. 클러스터 이름은 표가 아니라 "+
			"커넥션의 설정이므로, 커넥션 옵션 '클러스터 이름'에 적어 두면 "+
			"내보내는 DDL 에 다시 붙습니다")
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

	p.acceptOnCluster(start)

	// CREATE TABLE x AS … 는 두 가지다.
	//
	//   AS SELECT …   결과 구조를 문장에서 알 수 없다.
	//   AS 다른표      그 표의 컬럼을 그대로 쓴다. Distributed 표를 만들 때 쓰는
	//                  형태로, ClickHouse 클러스터에서는 표 절반이 이 모양이다.
	if p.peek().isWord("AS") {
		if tbl := p.tableFromLikeAs(start, ns, name); tbl != nil {
			p.finishTable(tbl)
			p.upsert(tbl)
			p.tableTail(tbl)
			p.skipToStatementEnd()
			return
		}
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
		case p.accept("ON", "UPDATE"):
			// 컬럼에 붙은 ON UPDATE(MySQL 의 자동 갱신)다. 외래키의 ON UPDATE 와
			// 글자가 같지만 있는 자리가 다르다 — 저쪽은 REFERENCES 뒤에 온다.
			//
			// 읽지 않고 지나가면 안 되는 이유: 덤프를 불러올 때 이 값만 조용히
			// 사라진다. 그러면 초안과 실제 DB 가 매번 다르다고 보고되고, 그 차이를
			// 없애려고 만든 마이그레이션이 자동 갱신을 지운다.
			col.OnUpdate = p.expr()
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

	// ClickHouse 는 널 허용이 타입 안에 있다. Nullable(...) 로 감싸지 않은 컬럼은
	// NOT NULL 이 없어도 NULL 을 받지 않는다.
	//
	// 다른 방언의 규칙("NOT NULL 이 없으면 널 허용")을 그대로 쓰면 모든 컬럼이
	// 널 허용으로 들어온다. 그러면 DDL 을 쓸 때 전부 Nullable 로 감싸지는데,
	// ClickHouse 에서 그것은 값마다 널 표시를 따로 저장하는 다른 표다.
	//
	// 수식어를 다 읽은 뒤에 정한다. 타입과 NOT NULL 이 어긋나면(Nullable(Date)
	// NOT NULL 같은 것) 타입이 이긴다 — 그것이 서버가 보는 방식이다.
	if p.dialect == "clickhouse" {
		_, nullable := schema.UnwrapClickHouseType(col.RawType)
		col.Nullable = nullable
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

// clickHouseTailClauses는 ClickHouse 표 정의의 꼬리에서 알아볼 절들이다.
//
// 낱말 수가 다르므로 함께 적어 둔다. ORDER BY 는 두 낱말, TTL 은 한 낱말이다.
var clickHouseTailClauses = []struct {
	words []string
	key   string
}{
	{[]string{"ENGINE"}, "engine"},
	{[]string{"ORDER", "BY"}, "order_by"},
	{[]string{"PARTITION", "BY"}, "partition_by"},
	{[]string{"PRIMARY", "KEY"}, "ch_primary_key"},
	{[]string{"SAMPLE", "BY"}, "sample_by"},
	{[]string{"TTL"}, "ttl"},
	{[]string{"SETTINGS"}, "settings"},
}

// clickhouseTail은 컬럼 괄호 뒤의 절들을 표 설정으로 담는다.
//
// 왜 따로 두는가: 다른 DB 에서 꼬리에 붙는 것들(ENGINE=InnoDB, CHARSET)은
// ERD 가 표현하지 않아도 표가 달라지지 않는다. ClickHouse 는 반대다. 엔진과
// 정렬 키가 **표의 구조 그 자체**이고, 파티션과 TTL 이 데이터의 수명을 정한다.
//
// 담지 않으면 이렇게 된다. SummingMergeTree ORDER BY (tenant, metric, day)
// PARTITION BY toYYYYMM(day) TTL day + 365일 로 적어 넣은 표가, 초안에서는
// MergeTree ORDER BY tuple() 짜리 표가 된다 — 집계도 정렬 키도 보관 기간도
// 없는 다른 물건인데, 경고 한 줄 나오지 않는다.
//
// 값은 원문을 그대로 잘라 담는다. 토큰을 다시 이어 붙이면 toYYYYMM(day) 같은
// 식에서 공백이 어긋난다.
// clickhouseMark는 꼬리에서 찾은 절 하나의 자리다.
// from: 값이 시작하는 원문 위치, at: 절 이름이 시작하는 위치.
type clickhouseMark struct {
	key      string
	from, at int
}

func (p *parser) clickhouseTail(tbl *schema.Table) {
	var marks []clickhouseMark
	depth := 0
	end := len(p.src)
	// 주석은 절이 아니라 경계다. 절 하나의 값이 어디서 끝나는지를 정할 때
	// 다음 절의 자리와 함께 본다.
	commentAt := -1

	for !p.done() {
		t := p.peek()
		switch {
		case t.isPunct("("):
			depth++
		case t.isPunct(")"):
			depth--
		case t.isPunct(";") && depth <= 0:
			// 문장이 끝났다. **위치를 옮기지 않고** 멈춘다.
			//
			// 처음에 여기서 p.i 를 토큰 끝으로 밀어 버렸다. 이 절만 보면 맞는
			// 것처럼 보이는데(더 읽을 꼬리가 없으니), 그러면 **그 뒤의 문장이
			// 전부 사라진다.** 한 문장짜리 스크립트로만 시험해서 오래 몰랐다.
			// 세미콜론은 부르는 쪽의 skipToStatementEnd 가 치운다.
			end = t.pos
			p.finishClickhouseTail(tbl, marks, commentAt, end)
			return
		}
		if depth == 0 && t.kind == tWord {
			matched := false
			for _, c := range clickHouseTailClauses {
				if !p.matchWords(c.words) {
					continue
				}
				at := t.pos
				p.i += len(c.words)
				p.acceptPunct("=")
				if p.peek().kind == tOperator && p.peek().text == "=" {
					p.i++
				}
				from := len(p.src)
				if !p.done() {
					from = p.peek().pos
				}
				marks = append(marks, clickhouseMark{key: c.key, from: from, at: at})
				matched = true
				break
			}
			if matched {
				continue
			}
			// COMMENT 는 문자열 하나라 원문을 자를 필요가 없다.
			if t.isWord("COMMENT") {
				p.i++
				p.acceptPunct("=")
				if p.peek().kind == tOperator && p.peek().text == "=" {
					p.i++
				}
				if p.peek().kind == tString {
					tbl.Comment = p.next().val
					if commentAt < 0 {
						commentAt = t.pos
					}
					continue
				}
				continue
			}
		}
		p.i++
	}

	p.finishClickhouseTail(tbl, marks, commentAt, end)
}

// finishClickhouseTail은 표시해 둔 경계로 원문을 잘라 표 설정에 담는다.
func (p *parser) finishClickhouseTail(tbl *schema.Table, marks []clickhouseMark, commentAt, end int) {
	if tbl.Options == nil {
		tbl.Options = map[string]string{}
	}
	for i, m := range marks {
		stop := end
		if i+1 < len(marks) {
			stop = marks[i+1].at
		}
		// COMMENT 가 중간에 끼면 그 앞에서 끊긴다. 절 하나가 다음 절까지
		// 삼키지 않게 하려면 두 경계를 따로 두어야 한다 — 처음에 주석 자리를
		// 앞 절의 경계 칸에 덮어썼더니, 그 칸이 곧 그 앞 절의 끝이라서
		// 파티션 절이 TTL 절을 통째로 삼켰다.
		if commentAt > m.at && commentAt < stop {
			stop = commentAt
		}
		if m.at < stop && m.from < stop {
			if v := cleanClickHouseClause(p.src[m.from:stop]); v != "" {
				tbl.Options[m.key] = v
			}
		}
	}
	// ClickHouse 의 PRIMARY KEY 는 정렬 키의 앞부분일 뿐 유일성을 뜻하지 않는다.
	// 표 설정으로만 남기고, ERD 의 기본키는 정렬 키에서 만든다.
	if pk := tbl.Options["ch_primary_key"]; pk != "" {
		delete(tbl.Options, "ch_primary_key")
		if tbl.PrimaryKey == nil {
			tbl.PrimaryKey = &schema.PrimaryKey{Columns: splitClickHouseKey(pk)}
		}
	}
	if tbl.PrimaryKey == nil {
		if cols := splitClickHouseKey(tbl.Options["order_by"]); len(cols) > 0 {
			tbl.PrimaryKey = &schema.PrimaryKey{Columns: cols}
		}
	}
}

// cleanClickHouseClause는 잘라 온 절에서 군더더기를 뗀다.
func cleanClickHouseClause(s string) string {
	v := strings.TrimSpace(s)
	v = strings.TrimSuffix(v, ";")
	v = strings.TrimSpace(v)
	// 괄호 한 겹은 벗긴다. 정렬 키를 (a, b) 로 담아 두면 DDL 을 쓸 때 다시
	// 감싸게 되고, 안쪽 괄호가 표현식인지 목록인지 구분이 흐려진다.
	if strings.HasPrefix(v, "(") && strings.HasSuffix(v, ")") && balancedOnce(v) {
		return strings.TrimSpace(v[1 : len(v)-1])
	}
	return v
}

// balancedOnce는 맨 앞 괄호가 맨 뒤 괄호와 짝인지 본다.
// toYYYYMM(day) 처럼 짝이 아닌 것을 벗기면 식이 깨진다.
func balancedOnce(s string) bool {
	depth := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i == len(s)-1
			}
		}
	}
	return false
}

// splitClickHouseKey는 정렬 키 목록을 컬럼 이름으로 가른다.
// 괄호 안의 쉼표는 함수 인자이므로 세지 않는다.
func splitClickHouseKey(expr string) []string {
	var out []string
	depth, start := 0, 0
	push := func(s string) {
		v := strings.Trim(strings.TrimSpace(s), "`\"")
		// 식은 컬럼 이름이 아니다. toYYYYMM(day) 같은 것은 기본키로 두지 않는다.
		if v != "" && !strings.ContainsAny(v, "() +-*/") {
			out = append(out, v)
		}
	}
	for i := 0; i < len(expr); i++ {
		switch expr[i] {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				push(expr[start:i])
				start = i + 1
			}
		}
	}
	push(expr[start:])
	return out
}

// matchWords는 지금 위치부터 낱말들이 순서대로 오는지 본다(위치는 옮기지 않는다).
func (p *parser) matchWords(words []string) bool {
	for i, w := range words {
		if !p.peekAt(i).isWord(w) {
			return false
		}
	}
	return true
}

// tableTail은 컬럼 정의 괄호 뒤의 테이블 옵션에서 주석만 건진다.
// 나머지(ENGINE, CHARSET, TABLESPACE)는 ERD가 표현하지 않는다.
//
// ClickHouse 는 예외다. 그쪽은 꼬리에 붙는 것이 표의 구조 그 자체라 따로 읽는다.
func (p *parser) tableTail(tbl *schema.Table) {
	if p.dialect == "clickhouse" {
		p.clickhouseTail(tbl)
		return
	}
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
