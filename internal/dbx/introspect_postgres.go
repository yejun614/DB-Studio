package dbx

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"dbstudio/internal/schema"
)

// introspectPostgres는 pg_catalog에서 스키마를 읽는다.
//
// information_schema보다 pg_catalog를 쓰는 이유: identity/generated 컬럼,
// 부분 인덱스 조건, 인덱스 방식(btree/gin), enum 타입을 information_schema로는
// 얻을 수 없거나 부정확하다.
//
// 대상 네임스페이스는 options.search_path 또는 기본 'public'이다.
func introspectPostgres(ctx context.Context, db *sql.DB, t Target, s *schema.Schema) error {
	namespaces := postgresNamespaces(t)

	noteExtensionObjects(ctx, db, namespaces, s)

	tables := map[string]*schema.Table{} // "ns.table" (소문자) → Table

	// 테이블 + 주석 + 통계
	rows, err := db.QueryContext(ctx, `
		SELECT n.nspname, c.relname,
		       COALESCE(obj_description(c.oid, 'pg_class'), ''),
		       COALESCE(c.reltuples, 0)::bigint,
		       COALESCE(pg_total_relation_size(c.oid), 0)
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE c.relkind = 'r' AND n.nspname = ANY($1::text[])
		  AND `+pgNotExtensionOwned("pg_class", "c.oid"), pgArray(namespaces))
	if err != nil {
		return fmt.Errorf("테이블 목록 조회 실패: %w", err)
	}
	for rows.Next() {
		var ns, name, comment string
		var rowCount, size int64
		if err := rows.Scan(&ns, &name, &comment, &rowCount, &size); err != nil {
			rows.Close()
			return fmt.Errorf("테이블 정보 스캔 실패: %w", err)
		}
		if rowCount < 0 {
			rowCount = 0 // ANALYZE 전에는 -1이 나온다
		}
		tbl := &schema.Table{
			Namespace: ns, Name: name, Comment: comment,
			Columns: []*schema.Column{}, Indexes: []*schema.Index{},
			ForeignKeys: []*schema.ForeignKey{}, Checks: []*schema.Check{},
			RowEstimate: rowCount, SizeBytes: size,
		}
		tables[strings.ToLower(ns+"."+name)] = tbl
		s.Tables = append(s.Tables, tbl)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("테이블 목록 순회 실패: %w", err)
	}
	if len(s.Tables) == 0 {
		s.AddNote("네임스페이스 %s 에서 테이블을 찾지 못했습니다", strings.Join(namespaces, ", "))
		return nil
	}

	// 컬럼
	rows, err = db.QueryContext(ctx, `
		SELECT n.nspname, c.relname, a.attname, a.attnum,
		       format_type(a.atttypid, a.atttypmod),
		       NOT a.attnotnull,
		       COALESCE(pg_get_expr(d.adbin, d.adrelid), ''),
		       a.atthasdef,
		       a.attidentity <> '' OR pg_get_serial_sequence(quote_ident(n.nspname) || '.' || quote_ident(c.relname), a.attname) IS NOT NULL,
		       COALESCE(a.attgenerated, '') <> '',
		       COALESCE(col_description(c.oid, a.attnum), ''),
		       COALESCE(co.collname, ''),
		       COALESCE(et.typname, '')
		FROM pg_attribute a
		JOIN pg_class c ON c.oid = a.attrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
		LEFT JOIN pg_collation co ON co.oid = a.attcollation AND co.collname <> 'default'
		LEFT JOIN pg_type pt ON pt.oid = a.atttypid
		LEFT JOIN pg_type et ON et.oid = pt.oid AND pt.typtype = 'e'
		WHERE c.relkind = 'r' AND n.nspname = ANY($1::text[]) AND a.attnum > 0 AND NOT a.attisdropped
		ORDER BY n.nspname, c.relname, a.attnum`, pgArray(namespaces))
	if err != nil {
		return fmt.Errorf("컬럼 조회 실패: %w", err)
	}
	for rows.Next() {
		var ns, tblName, colName, rawType, def, comment, collation, enumName string
		var pos int
		var nullable, hasDefault, identity, generated bool
		if err := rows.Scan(&ns, &tblName, &colName, &pos, &rawType, &nullable,
			&def, &hasDefault, &identity, &generated, &comment, &collation, &enumName); err != nil {
			rows.Close()
			return fmt.Errorf("컬럼 정보 스캔 실패: %w", err)
		}
		tbl := tables[strings.ToLower(ns+"."+tblName)]
		if tbl == nil {
			continue
		}
		lt := schema.ParseType("postgres", rawType)
		if enumName != "" {
			lt = schema.LogicalType{Base: schema.TypeEnum, EnumName: enumName}
		}
		col := &schema.Column{
			Name: colName, Position: pos, Type: lt, RawType: rawType,
			Nullable: nullable, Identity: identity,
			Comment: comment, Collation: collation,
		}
		if generated {
			col.Generated = def
		} else if hasDefault && def != "" {
			// serial 컬럼의 기본값은 nextval(...)이며 identity로 표현되므로 중복 기록하지 않는다.
			if strings.HasPrefix(def, "nextval(") {
				col.Identity = true
			} else {
				col.HasDefault = true
				col.Default = def
			}
		}
		tbl.Columns = append(tbl.Columns, col)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("컬럼 순회 실패: %w", err)
	}

	// 기본키
	rows, err = db.QueryContext(ctx, `
		SELECT n.nspname, c.relname, con.conname,
		       ARRAY(SELECT a.attname FROM unnest(con.conkey) WITH ORDINALITY AS k(attnum, ord)
		             JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum = k.attnum
		             ORDER BY k.ord)
		FROM pg_constraint con
		JOIN pg_class c ON c.oid = con.conrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE con.contype = 'p' AND n.nspname = ANY($1::text[])`, pgArray(namespaces))
	if err != nil {
		return fmt.Errorf("기본키 조회 실패: %w", err)
	}
	for rows.Next() {
		var ns, tblName, cname string
		var cols pgTextArray
		if err := rows.Scan(&ns, &tblName, &cname, &cols); err != nil {
			rows.Close()
			return fmt.Errorf("기본키 정보 스캔 실패: %w", err)
		}
		if tbl := tables[strings.ToLower(ns+"."+tblName)]; tbl != nil {
			tbl.PrimaryKey = &schema.PrimaryKey{Name: cname, Columns: cols.Values}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("기본키 순회 실패: %w", err)
	}

	// 인덱스. 기본키를 뒷받침하는 인덱스와 제약이 만든 인덱스는 제외한다 —
	// 그것들은 제약으로 이미 표현되므로 중복 DDL을 만들면 충돌한다.
	rows, err = db.QueryContext(ctx, `
		SELECT n.nspname, c.relname, i.relname, x.indisunique,
		       am.amname,
		       COALESCE(pg_get_expr(x.indpred, x.indrelid), ''),
		       pg_get_indexdef(x.indexrelid)
		FROM pg_index x
		JOIN pg_class c ON c.oid = x.indrelid
		JOIN pg_class i ON i.oid = x.indexrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		JOIN pg_am am ON am.oid = i.relam
		WHERE n.nspname = ANY($1::text[]) AND NOT x.indisprimary
		  AND NOT EXISTS (SELECT 1 FROM pg_constraint con WHERE con.conindid = x.indexrelid)
		ORDER BY n.nspname, c.relname, i.relname`, pgArray(namespaces))
	if err != nil {
		return fmt.Errorf("인덱스 조회 실패: %w", err)
	}
	for rows.Next() {
		var ns, tblName, idxName, method, where, indexDef string
		var unique bool
		if err := rows.Scan(&ns, &tblName, &idxName, &unique, &method, &where, &indexDef); err != nil {
			rows.Close()
			return fmt.Errorf("인덱스 정보 스캔 실패: %w", err)
		}
		tbl := tables[strings.ToLower(ns+"."+tblName)]
		if tbl == nil {
			continue
		}
		tbl.Indexes = append(tbl.Indexes, &schema.Index{
			Name: idxName, Unique: unique, Type: method, Where: where,
			Columns: parseIndexDefColumns(indexDef),
		})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("인덱스 순회 실패: %w", err)
	}

	// 외래키 + 체크 제약
	rows, err = db.QueryContext(ctx, `
		SELECT n.nspname, c.relname, con.conname, con.contype,
		       COALESCE(fn.nspname, ''), COALESCE(fc.relname, ''),
		       ARRAY(SELECT a.attname FROM unnest(con.conkey) WITH ORDINALITY AS k(attnum, ord)
		             JOIN pg_attribute a ON a.attrelid = con.conrelid AND a.attnum = k.attnum
		             ORDER BY k.ord),
		       ARRAY(SELECT a.attname FROM unnest(con.confkey) WITH ORDINALITY AS k(attnum, ord)
		             JOIN pg_attribute a ON a.attrelid = con.confrelid AND a.attnum = k.attnum
		             ORDER BY k.ord),
		       con.confdeltype, con.confupdtype, con.condeferrable,
		       COALESCE(pg_get_constraintdef(con.oid), '')
		FROM pg_constraint con
		JOIN pg_class c ON c.oid = con.conrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		LEFT JOIN pg_class fc ON fc.oid = con.confrelid
		LEFT JOIN pg_namespace fn ON fn.oid = fc.relnamespace
		WHERE con.contype IN ('f', 'c') AND n.nspname = ANY($1::text[])
		ORDER BY n.nspname, c.relname, con.conname`, pgArray(namespaces))
	if err != nil {
		return fmt.Errorf("제약 조회 실패: %w", err)
	}
	for rows.Next() {
		var ns, tblName, cname, ctype, refNS, refTable, delType, updType, def string
		var cols, refCols pgTextArray
		var deferrable bool
		if err := rows.Scan(&ns, &tblName, &cname, &ctype, &refNS, &refTable,
			&cols, &refCols, &delType, &updType, &deferrable, &def); err != nil {
			rows.Close()
			return fmt.Errorf("제약 정보 스캔 실패: %w", err)
		}
		tbl := tables[strings.ToLower(ns+"."+tblName)]
		if tbl == nil {
			continue
		}
		if ctype == "f" {
			// 참조 네임스페이스가 대상과 같으면 생략해 dialect 간 이식성을 높인다.
			if strings.EqualFold(refNS, ns) {
				refNS = ""
			}
			tbl.ForeignKeys = append(tbl.ForeignKeys, &schema.ForeignKey{
				Name: cname, Columns: cols.Values,
				RefNamespace: refNS, RefTable: refTable, RefColumns: refCols.Values,
				OnDelete: pgFKAction(delType), OnUpdate: pgFKAction(updType),
				Deferrable: deferrable,
			})
			continue
		}
		// contype='c': pg_get_constraintdef는 "CHECK ((expr))" 형태로 준다.
		expr := strings.TrimPrefix(def, "CHECK ")
		tbl.Checks = append(tbl.Checks, &schema.Check{Name: cname, Expression: expr})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("제약 순회 실패: %w", err)
	}

	// enum 타입
	erows, err := db.QueryContext(ctx, `
		SELECT n.nspname, t.typname,
		       ARRAY(SELECT e.enumlabel FROM pg_enum e WHERE e.enumtypid = t.oid ORDER BY e.enumsortorder)
		FROM pg_type t
		JOIN pg_namespace n ON n.oid = t.typnamespace
		WHERE t.typtype = 'e' AND n.nspname = ANY($1::text[])
		  AND `+pgNotExtensionOwned("pg_type", "t.oid"), pgArray(namespaces))
	if err != nil {
		s.AddNote("enum 타입을 읽지 못했습니다: %v", err)
	} else {
		for erows.Next() {
			var ns, name string
			var values pgTextArray
			if err := erows.Scan(&ns, &name, &values); err != nil {
				erows.Close()
				return fmt.Errorf("enum 정보 스캔 실패: %w", err)
			}
			s.Enums = append(s.Enums, &schema.Enum{Namespace: ns, Name: name, Values: values.Values})
		}
		erows.Close()
		if err := erows.Err(); err != nil {
			return fmt.Errorf("enum 순회 실패: %w", err)
		}
	}

	// 뷰
	vrows, err := db.QueryContext(ctx, `
		SELECT n.nspname, c.relname, COALESCE(pg_get_viewdef(c.oid, true), ''),
		       COALESCE(obj_description(c.oid, 'pg_class'), '')
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE c.relkind IN ('v', 'm') AND n.nspname = ANY($1::text[])
		  AND `+pgNotExtensionOwned("pg_class", "c.oid"), pgArray(namespaces))
	if err != nil {
		s.AddNote("뷰 목록을 읽지 못했습니다: %v", err)
		return nil
	}
	defer vrows.Close()
	for vrows.Next() {
		var ns, name, def, comment string
		if err := vrows.Scan(&ns, &name, &def, &comment); err != nil {
			return fmt.Errorf("뷰 정보 스캔 실패: %w", err)
		}
		s.Views = append(s.Views, &schema.View{
			Namespace: ns, Name: name,
			Definition: strings.TrimSuffix(strings.TrimSpace(def), ";"),
			Comment:    comment,
		})
	}
	if err := vrows.Err(); err != nil {
		return fmt.Errorf("뷰 순회 실패: %w", err)
	}
	return nil
}

// pgNotExtensionOwned는 확장(extension)이 소유한 객체를 제외하는 조건을 만든다.
//
// pg_stat_statements 같은 확장을 설치하면 그 뷰와 타입이 public 스키마에 들어온다.
// 그것들을 사용자 스키마로 취급하면 두 가지가 망가진다: ERD가 사용자와 무관한
// 객체로 어지러워지고, 생성한 DDL이 확장 전용 C 함수를 참조해 다른 곳에서
// 실행되지 않는다. MS-SQL에서 is_ms_shipped = 0 으로 걸러내는 것과 같은 이유다.
//
// pg_depend.deptype = 'e' 가 "이 객체는 확장에 속한다"는 표시다.
func pgNotExtensionOwned(catalog, oidExpr string) string {
	return `NOT EXISTS (
			SELECT 1 FROM pg_depend dep
			WHERE dep.classid = '` + catalog + `'::regclass
			  AND dep.objid = ` + oidExpr + `
			  AND dep.deptype = 'e')`
}

// noteExtensionObjects는 확장 소유로 제외한 객체가 있으면 그 사실을 알린다.
// 조용히 빼면 "내가 만든 뷰가 왜 안 보이지"라는 오해를 부른다.
func noteExtensionObjects(ctx context.Context, db *sql.DB, namespaces []string, s *schema.Schema) {
	var count int
	var names string
	err := db.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(string_agg(DISTINCT e.extname, ', '), '')
		FROM pg_depend dep
		JOIN pg_extension e ON e.oid = dep.refobjid
		JOIN pg_class c ON c.oid = dep.objid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE dep.classid = 'pg_class'::regclass AND dep.deptype = 'e'
		  AND c.relkind IN ('r', 'v', 'm')
		  AND n.nspname = ANY($1::text[])`, pgArray(namespaces)).Scan(&count, &names)
	if err != nil || count == 0 {
		return
	}
	s.AddNote("확장(%s)이 소유한 객체 %d개는 스키마에서 제외했습니다", names, count)
}

// postgresNamespaces는 읽을 스키마 목록을 결정한다.
func postgresNamespaces(t Target) []string {
	raw := t.Opt("search_path", "")
	if strings.TrimSpace(raw) == "" {
		return []string{"public"}
	}
	out := []string{}
	for _, part := range strings.Split(raw, ",") {
		p := strings.TrimSpace(part)
		// $user 같은 동적 항목은 여기서 해석할 수 없으므로 건너뛴다.
		if p == "" || strings.HasPrefix(p, "$") {
			continue
		}
		out = append(out, p)
	}
	if len(out) == 0 {
		return []string{"public"}
	}
	return out
}

func pgFKAction(code string) string {
	switch code {
	case "a":
		return "NO ACTION"
	case "r":
		return "RESTRICT"
	case "c":
		return "CASCADE"
	case "n":
		return "SET NULL"
	case "d":
		return "SET DEFAULT"
	}
	return ""
}

// parseIndexDefColumns는 pg_get_indexdef 결과에서 컬럼 목록을 추출한다.
// pg_index.indkey로 컬럼을 얻으면 식 기반 인덱스와 DESC/NULLS 순서를 잃기 때문에
// 정의 문자열을 파싱한다.
func parseIndexDefColumns(indexDef string) []schema.IndexPart {
	open := strings.Index(indexDef, "(")
	if open < 0 {
		return nil
	}
	// WHERE 절 앞까지의 마지막 닫는 괄호를 찾는다.
	body := indexDef[open+1:]
	if w := strings.LastIndex(strings.ToUpper(body), ") WHERE "); w >= 0 {
		body = body[:w]
	} else if c := strings.LastIndex(body, ")"); c >= 0 {
		body = body[:c]
	}

	parts := []schema.IndexPart{}
	for _, raw := range splitBalanced(body) {
		item := strings.TrimSpace(raw)
		if item == "" {
			continue
		}
		p := schema.IndexPart{}
		upper := strings.ToUpper(item)
		if strings.HasSuffix(upper, " DESC") {
			p.Descending = true
			item = strings.TrimSpace(item[:len(item)-5])
		} else if strings.HasSuffix(upper, " ASC") {
			item = strings.TrimSpace(item[:len(item)-4])
		}
		// NULLS FIRST/LAST와 연산자 클래스는 비교 대상에서 제외한다.
		for _, suffix := range []string{" NULLS FIRST", " NULLS LAST"} {
			if strings.HasSuffix(strings.ToUpper(item), suffix) {
				item = strings.TrimSpace(item[:len(item)-len(suffix)])
			}
		}
		if strings.HasPrefix(item, "(") {
			p.Expression = item
		} else {
			// 첫 토큰만 컬럼명이다 (뒤에 opclass가 올 수 있다).
			p.Column = strings.Trim(strings.Fields(item)[0], `"`)
		}
		parts = append(parts, p)
	}
	return parts
}

// splitBalanced는 괄호 깊이를 고려해 콤마로 분리한다.
func splitBalanced(s string) []string {
	out := []string{}
	depth := 0
	var cur strings.Builder
	var quote rune
	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			}
			cur.WriteRune(r)
		case r == '\'' || r == '"':
			quote = r
			cur.WriteRune(r)
		case r == '(':
			depth++
			cur.WriteRune(r)
		case r == ')':
			depth--
			cur.WriteRune(r)
		case r == ',' && depth == 0:
			out = append(out, cur.String())
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	if strings.TrimSpace(cur.String()) != "" {
		out = append(out, cur.String())
	}
	return out
}
