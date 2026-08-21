package dbx

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strings"

	"dbstudio/internal/schema"
)

// introspectSQLite는 sqlite_master와 PRAGMA로 스키마를 읽는다.
//
// SQLite에는 information_schema가 없다. PRAGMA는 테이블명을 파라미터로 받지 못하므로
// 식별자를 직접 인용해 넣어야 한다 — 그래서 sqlite_master에서 얻은 이름만 사용하고
// 인용부호를 이스케이프한다(외부 입력이 아니지만 이름에 따옴표가 들어갈 수 있다).
func introspectSQLite(ctx context.Context, db *sql.DB, t Target, s *schema.Schema) error {
	s.Name = t.Conn.DatabaseName

	// 테이블 목록. sqlite_ 접두사 객체는 내부용이므로 제외한다.
	rows, err := db.QueryContext(ctx, `
		SELECT name, sql FROM sqlite_master
		WHERE type = 'table' AND name NOT LIKE 'sqlite_%'
		ORDER BY name`)
	if err != nil {
		return fmt.Errorf("테이블 목록 조회 실패: %w", err)
	}
	type tableDef struct {
		name string
		ddl  string
	}
	defs := []tableDef{}
	for rows.Next() {
		var name string
		var ddl sql.NullString
		if err := rows.Scan(&name, &ddl); err != nil {
			rows.Close()
			return fmt.Errorf("테이블 정보 스캔 실패: %w", err)
		}
		defs = append(defs, tableDef{name: name, ddl: ddl.String})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("테이블 목록 순회 실패: %w", err)
	}

	for i, d := range defs {
		tbl := &schema.Table{
			Name:    d.name,
			Columns: []*schema.Column{}, Indexes: []*schema.Index{},
			ForeignKeys: []*schema.ForeignKey{}, Checks: []*schema.Check{},
		}
		s.Tables = append(s.Tables, tbl)

		if err := sqliteColumns(ctx, db, tbl); err != nil {
			return err
		}
		if err := sqliteIndexes(ctx, db, tbl); err != nil {
			return err
		}
		if err := sqliteForeignKeys(ctx, db, tbl); err != nil {
			return err
		}
		// 체크 제약과 AUTOINCREMENT는 PRAGMA로 얻을 수 없어 원본 DDL에서 추출한다.
		tbl.Checks = append(tbl.Checks, parseSQLiteChecks(d.ddl)...)
		applySQLiteAutoincrement(tbl, d.ddl)

		// 행 수는 통계 테이블이 없어 직접 세야 한다. 큰 테이블에서는 비싸므로
		// 테이블 수가 많을 때는 생략하고 그 사실을 알린다.
		if len(defs) <= 50 {
			var n int64
			q := fmt.Sprintf(`SELECT count(*) FROM %s`, sqliteQuote(d.name))
			if err := db.QueryRowContext(ctx, q).Scan(&n); err == nil {
				tbl.RowEstimate = n
			}
		} else if i == 0 {
			s.AddNote("테이블이 %d개라 행 수 집계를 생략했습니다 (SQLite는 통계 테이블이 없어 전체 스캔이 필요합니다)", len(defs))
		}
	}

	// 뷰
	vrows, err := db.QueryContext(ctx, `
		SELECT name, sql FROM sqlite_master WHERE type = 'view' ORDER BY name`)
	if err != nil {
		s.AddNote("뷰 목록을 읽지 못했습니다: %v", err)
		return nil
	}
	defer vrows.Close()
	for vrows.Next() {
		var name string
		var ddl sql.NullString
		if err := vrows.Scan(&name, &ddl); err != nil {
			return fmt.Errorf("뷰 정보 스캔 실패: %w", err)
		}
		s.Views = append(s.Views, &schema.View{
			Name: name, Definition: extractViewBody(ddl.String),
		})
	}
	if err := vrows.Err(); err != nil {
		return fmt.Errorf("뷰 순회 실패: %w", err)
	}
	return nil
}

func sqliteColumns(ctx context.Context, db *sql.DB, tbl *schema.Table) error {
	// table_xinfo는 생성 컬럼(hidden)까지 포함한다.
	rows, err := db.QueryContext(ctx, fmt.Sprintf(`PRAGMA table_xinfo(%s)`, sqliteQuote(tbl.Name)))
	if err != nil {
		return fmt.Errorf("%s 컬럼 조회 실패: %w", tbl.Name, err)
	}
	defer rows.Close()

	pkCols := map[int]string{} // pk 순서 → 컬럼명
	for rows.Next() {
		var cid, notNull, pk, hidden int
		var name, declType string
		var def sql.NullString
		if err := rows.Scan(&cid, &name, &declType, &notNull, &def, &pk, &hidden); err != nil {
			return fmt.Errorf("%s 컬럼 정보 스캔 실패: %w", tbl.Name, err)
		}
		col := &schema.Column{
			Name: name, Position: cid + 1,
			Type: schema.ParseType("sqlite", declType), RawType: declType,
			Nullable: notNull == 0,
		}
		if def.Valid {
			col.HasDefault = true
			col.Default = def.String
		}
		// hidden=2는 VIRTUAL, 3은 STORED 생성 컬럼이다.
		if hidden == 2 || hidden == 3 {
			col.Generated = def.String
			col.HasDefault = false
			col.Default = ""
		}
		if pk > 0 {
			pkCols[pk] = name
		}
		tbl.Columns = append(tbl.Columns, col)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("%s 컬럼 순회 실패: %w", tbl.Name, err)
	}

	if len(pkCols) > 0 {
		cols := make([]string, 0, len(pkCols))
		for i := 1; i <= len(pkCols); i++ {
			if name, ok := pkCols[i]; ok {
				cols = append(cols, name)
			}
		}
		tbl.PrimaryKey = &schema.PrimaryKey{Columns: cols}
	}
	return nil
}

func sqliteIndexes(ctx context.Context, db *sql.DB, tbl *schema.Table) error {
	rows, err := db.QueryContext(ctx, fmt.Sprintf(`PRAGMA index_list(%s)`, sqliteQuote(tbl.Name)))
	if err != nil {
		return fmt.Errorf("%s 인덱스 목록 조회 실패: %w", tbl.Name, err)
	}
	type idxRow struct {
		name    string
		unique  bool
		origin  string
		partial bool
	}
	list := []idxRow{}
	for rows.Next() {
		var seq, unique, partial int
		var name, origin string
		if err := rows.Scan(&seq, &name, &unique, &origin, &partial); err != nil {
			rows.Close()
			return fmt.Errorf("%s 인덱스 정보 스캔 실패: %w", tbl.Name, err)
		}
		list = append(list, idxRow{name: name, unique: unique == 1, origin: origin, partial: partial == 1})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("%s 인덱스 목록 순회 실패: %w", tbl.Name, err)
	}

	for _, ir := range list {
		// origin: "c"=CREATE INDEX, "u"=UNIQUE 제약, "pk"=기본키.
		// 제약이 만든 인덱스는 제약으로 이미 표현되므로 제외한다.
		if ir.origin == "pk" {
			continue
		}
		idx := &schema.Index{Name: ir.name, Unique: ir.unique, Columns: []schema.IndexPart{}}

		irows, err := db.QueryContext(ctx, fmt.Sprintf(`PRAGMA index_xinfo(%s)`, sqliteQuote(ir.name)))
		if err != nil {
			return fmt.Errorf("%s 인덱스 컬럼 조회 실패: %w", ir.name, err)
		}
		for irows.Next() {
			var seqno, cid, desc, coll, key int
			var name sql.NullString
			var collName sql.NullString
			if err := irows.Scan(&seqno, &cid, &name, &desc, &collName, &key); err != nil {
				irows.Close()
				return fmt.Errorf("%s 인덱스 컬럼 스캔 실패: %w", ir.name, err)
			}
			_ = coll
			// key=0은 인덱스 뒤에 자동으로 붙는 rowid 컬럼이므로 제외한다.
			if key == 0 || !name.Valid {
				continue
			}
			idx.Columns = append(idx.Columns, schema.IndexPart{
				Column: name.String, Descending: desc == 1,
			})
		}
		irows.Close()
		if err := irows.Err(); err != nil {
			return fmt.Errorf("%s 인덱스 컬럼 순회 실패: %w", ir.name, err)
		}
		if len(idx.Columns) > 0 {
			tbl.Indexes = append(tbl.Indexes, idx)
		}
	}
	return nil
}

func sqliteForeignKeys(ctx context.Context, db *sql.DB, tbl *schema.Table) error {
	rows, err := db.QueryContext(ctx, fmt.Sprintf(`PRAGMA foreign_key_list(%s)`, sqliteQuote(tbl.Name)))
	if err != nil {
		return fmt.Errorf("%s 외래키 조회 실패: %w", tbl.Name, err)
	}
	defer rows.Close()

	acc := map[int]*schema.ForeignKey{}
	for rows.Next() {
		var id, seq int
		var refTable, from, onUpdate, onDelete, match string
		var to sql.NullString
		if err := rows.Scan(&id, &seq, &refTable, &from, &to, &onUpdate, &onDelete, &match); err != nil {
			return fmt.Errorf("%s 외래키 정보 스캔 실패: %w", tbl.Name, err)
		}
		fk := acc[id]
		if fk == nil {
			// SQLite는 외래키에 이름을 부여하지 않는다. diff가 안정적으로 매칭되려면
			// 이름이 필요하므로 구조에서 결정적인 이름을 만든다.
			fk = &schema.ForeignKey{
				RefTable: refTable, Columns: []string{}, RefColumns: []string{},
				OnDelete: onDelete, OnUpdate: onUpdate,
			}
			acc[id] = fk
			tbl.ForeignKeys = append(tbl.ForeignKeys, fk)
		}
		fk.Columns = append(fk.Columns, from)
		if to.Valid && to.String != "" {
			fk.RefColumns = append(fk.RefColumns, to.String)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("%s 외래키 순회 실패: %w", tbl.Name, err)
	}

	for _, fk := range tbl.ForeignKeys {
		if fk.Name == "" {
			fk.Name = fmt.Sprintf("fk_%s_%s", strings.ToLower(tbl.Name),
				strings.ToLower(strings.Join(fk.Columns, "_")))
		}
		// to가 비어 있으면 참조 대상 테이블의 기본키를 가리킨다.
		if len(fk.RefColumns) == 0 {
			fk.RefColumns = nil
		}
	}
	return nil
}

var sqliteCheckRE = regexp.MustCompile(`(?is)CONSTRAINT\s+["'\[]?([\w]+)["'\]]?\s+CHECK\s*\(`)

// parseSQLiteChecks는 CREATE TABLE 문에서 이름 있는 체크 제약을 추출한다.
// PRAGMA로는 체크 제약을 얻을 수 없어 DDL 파싱이 유일한 방법이다.
// 이름 없는 체크 제약은 diff에서 안정적으로 매칭할 수 없어 건너뛴다.
func parseSQLiteChecks(ddl string) []*schema.Check {
	out := []*schema.Check{}
	for _, m := range sqliteCheckRE.FindAllStringSubmatchIndex(ddl, -1) {
		name := ddl[m[2]:m[3]]
		// CHECK( 다음부터 괄호 균형이 맞는 지점까지가 표현식이다.
		start := m[1] // 여는 괄호 다음
		depth := 1
		end := start
		for end < len(ddl) && depth > 0 {
			switch ddl[end] {
			case '(':
				depth++
			case ')':
				depth--
			}
			if depth == 0 {
				break
			}
			end++
		}
		if depth != 0 {
			continue
		}
		out = append(out, &schema.Check{
			Name:       name,
			Expression: strings.TrimSpace(ddl[start:end]),
		})
	}
	return out
}

var sqliteAutoincRE = regexp.MustCompile(`(?is)["'\[]?(\w+)["'\]]?\s+INTEGER\s+PRIMARY\s+KEY\s+AUTOINCREMENT`)

// applySQLiteAutoincrement는 AUTOINCREMENT 컬럼에 Identity 플래그를 세운다.
// PRAGMA는 이 정보를 노출하지 않는다.
func applySQLiteAutoincrement(tbl *schema.Table, ddl string) {
	m := sqliteAutoincRE.FindStringSubmatch(ddl)
	if m == nil {
		return
	}
	if col := tbl.Column(m[1]); col != nil {
		col.Identity = true
	}
}

// sqliteQuote는 PRAGMA 인자로 넣을 식별자를 인용한다.
func sqliteQuote(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}
