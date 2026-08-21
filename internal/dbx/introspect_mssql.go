package dbx

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strings"

	"dbstudio/internal/schema"
)

// introspectMSSQL은 sys.* 카탈로그 뷰에서 스키마를 읽는다.
//
// information_schema 대신 sys.*를 쓰는 이유: IDENTITY 여부, 확장 속성(주석),
// 필터 인덱스 조건, 인덱스 컬럼 순서를 information_schema로는 얻을 수 없다.
func introspectMSSQL(ctx context.Context, db *sql.DB, t Target, s *schema.Schema) error {
	// 대상 스키마를 제한하지 않으면 sys 내부 객체까지 끌려온다.
	// 사용자가 지정하지 않으면 시스템 스키마만 제외한다.
	// is_ms_shipped 필터가 중요하다: master 같은 시스템 DB의 dbo 스키마에는
	// spt_values 처럼 Microsoft가 배포한 객체가 들어 있어, 이를 사용자 객체로
	// 취급하면 마이그레이션 계획에 시스템 뷰가 섞인다.
	nsFilter := strings.TrimSpace(t.Opt("schema", ""))
	nsClause := "s.name NOT IN ('sys', 'INFORMATION_SCHEMA')"
	args := []any{}
	if nsFilter != "" {
		nsClause = "s.name = @p1"
		args = append(args, nsFilter)
	}
	tableClause := nsClause + " AND tb.is_ms_shipped = 0"
	viewClause := nsClause + " AND v.is_ms_shipped = 0"

	tables := map[string]*schema.Table{}

	rows, err := db.QueryContext(ctx, `
		SELECT s.name, tb.name,
		       ISNULL(CAST(ep.value AS NVARCHAR(MAX)), ''),
		       ISNULL(p.rows, 0),
		       ISNULL(SUM(au.total_pages) * 8192, 0)
		FROM sys.tables tb
		JOIN sys.schemas s ON s.schema_id = tb.schema_id
		LEFT JOIN sys.extended_properties ep
		       ON ep.major_id = tb.object_id AND ep.minor_id = 0 AND ep.name = 'MS_Description'
		LEFT JOIN sys.partitions p
		       ON p.object_id = tb.object_id AND p.index_id IN (0, 1)
		LEFT JOIN sys.allocation_units au ON au.container_id = p.partition_id
		WHERE `+tableClause+`
		GROUP BY s.name, tb.name, CAST(ep.value AS NVARCHAR(MAX)), p.rows`, args...)
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
		return nil
	}

	// 컬럼. 타입 길이는 sys.types의 max_length가 바이트 단위이므로
	// nchar/nvarchar는 2로 나누어 문자 수로 환산한다.
	rows, err = db.QueryContext(ctx, `
		SELECT s.name, tb.name, c.name, c.column_id, ty.name,
		       c.max_length, c.precision, c.scale, c.is_nullable, c.is_identity,
		       ISNULL(dc.definition, ''), CASE WHEN dc.definition IS NULL THEN 0 ELSE 1 END,
		       ISNULL(cc.definition, ''),
		       ISNULL(CAST(ep.value AS NVARCHAR(MAX)), ''),
		       ISNULL(c.collation_name, '')
		FROM sys.columns c
		JOIN sys.tables tb ON tb.object_id = c.object_id
		JOIN sys.schemas s ON s.schema_id = tb.schema_id
		JOIN sys.types ty ON ty.user_type_id = c.user_type_id
		LEFT JOIN sys.default_constraints dc ON dc.object_id = c.default_object_id
		LEFT JOIN sys.computed_columns cc ON cc.object_id = c.object_id AND cc.column_id = c.column_id
		LEFT JOIN sys.extended_properties ep
		       ON ep.major_id = c.object_id AND ep.minor_id = c.column_id AND ep.name = 'MS_Description'
		WHERE `+tableClause+`
		ORDER BY s.name, tb.name, c.column_id`, args...)
	if err != nil {
		return fmt.Errorf("컬럼 조회 실패: %w", err)
	}
	for rows.Next() {
		var ns, tblName, colName, typeName, def, computed, comment, collation string
		var colID, precision, scale int
		var maxLength int
		var nullable, isIdentity, hasDefault bool
		if err := rows.Scan(&ns, &tblName, &colName, &colID, &typeName,
			&maxLength, &precision, &scale, &nullable, &isIdentity,
			&def, &hasDefault, &computed, &comment, &collation); err != nil {
			rows.Close()
			return fmt.Errorf("컬럼 정보 스캔 실패: %w", err)
		}
		tbl := tables[strings.ToLower(ns+"."+tblName)]
		if tbl == nil {
			continue
		}
		raw := mssqlRawType(typeName, maxLength, precision, scale)
		col := &schema.Column{
			Name: colName, Position: colID,
			Type: schema.ParseType("mssql", raw), RawType: raw,
			Nullable: nullable, Identity: isIdentity,
			Comment: comment, Collation: collation,
		}
		if computed != "" {
			col.Generated = strings.Trim(computed, "()")
		} else if hasDefault {
			col.HasDefault = true
			// sys.default_constraints.definition은 "((0))" 처럼 이중 괄호로 온다.
			col.Default = trimOuterParens(def)
		}
		tbl.Columns = append(tbl.Columns, col)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("컬럼 순회 실패: %w", err)
	}

	// 인덱스 + 기본키. is_primary_key로 둘을 구분한다.
	rows, err = db.QueryContext(ctx, `
		SELECT s.name, tb.name, i.name, i.is_unique, i.is_primary_key,
		       i.type_desc, ISNULL(i.filter_definition, ''),
		       c.name, ic.key_ordinal, ic.is_descending_key
		FROM sys.indexes i
		JOIN sys.tables tb ON tb.object_id = i.object_id
		JOIN sys.schemas s ON s.schema_id = tb.schema_id
		JOIN sys.index_columns ic ON ic.object_id = i.object_id AND ic.index_id = i.index_id
		JOIN sys.columns c ON c.object_id = ic.object_id AND c.column_id = ic.column_id
		WHERE `+tableClause+` AND i.name IS NOT NULL AND ic.is_included_column = 0
		ORDER BY s.name, tb.name, i.name, ic.key_ordinal`, args...)
	if err != nil {
		return fmt.Errorf("인덱스 조회 실패: %w", err)
	}
	type acc struct {
		idx *schema.Index
		pk  *schema.PrimaryKey
	}
	seen := map[string]*acc{}
	for rows.Next() {
		var ns, tblName, idxName, typeDesc, filter, colName string
		var unique, isPK, descending bool
		var ordinal int
		if err := rows.Scan(&ns, &tblName, &idxName, &unique, &isPK,
			&typeDesc, &filter, &colName, &ordinal, &descending); err != nil {
			rows.Close()
			return fmt.Errorf("인덱스 정보 스캔 실패: %w", err)
		}
		tbl := tables[strings.ToLower(ns+"."+tblName)]
		if tbl == nil {
			continue
		}
		key := strings.ToLower(ns+"."+tblName) + "\x00" + idxName
		a := seen[key]
		if a == nil {
			a = &acc{}
			if isPK {
				a.pk = &schema.PrimaryKey{Name: idxName, Columns: []string{}}
				tbl.PrimaryKey = a.pk
			} else {
				a.idx = &schema.Index{
					Name: idxName, Unique: unique, Where: filter,
					Type: strings.ToLower(typeDesc), Columns: []schema.IndexPart{},
				}
				tbl.Indexes = append(tbl.Indexes, a.idx)
			}
			seen[key] = a
		}
		if a.pk != nil {
			a.pk.Columns = append(a.pk.Columns, colName)
			continue
		}
		a.idx.Columns = append(a.idx.Columns, schema.IndexPart{Column: colName, Descending: descending})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("인덱스 순회 실패: %w", err)
	}

	// 외래키
	rows, err = db.QueryContext(ctx, `
		SELECT s.name, tb.name, fk.name,
		       rs.name, rt.name,
		       pc.name, rc.name, fkc.constraint_column_id,
		       fk.delete_referential_action_desc, fk.update_referential_action_desc
		FROM sys.foreign_keys fk
		JOIN sys.tables tb ON tb.object_id = fk.parent_object_id
		JOIN sys.schemas s ON s.schema_id = tb.schema_id
		JOIN sys.tables rt ON rt.object_id = fk.referenced_object_id
		JOIN sys.schemas rs ON rs.schema_id = rt.schema_id
		JOIN sys.foreign_key_columns fkc ON fkc.constraint_object_id = fk.object_id
		JOIN sys.columns pc ON pc.object_id = fkc.parent_object_id AND pc.column_id = fkc.parent_column_id
		JOIN sys.columns rc ON rc.object_id = fkc.referenced_object_id AND rc.column_id = fkc.referenced_column_id
		WHERE `+tableClause+`
		ORDER BY s.name, tb.name, fk.name, fkc.constraint_column_id`, args...)
	if err != nil {
		return fmt.Errorf("외래키 조회 실패: %w", err)
	}
	fkSeen := map[string]*schema.ForeignKey{}
	for rows.Next() {
		var ns, tblName, fkName, refNS, refTable, col, refCol, delAction, updAction string
		var ordinal int
		if err := rows.Scan(&ns, &tblName, &fkName, &refNS, &refTable,
			&col, &refCol, &ordinal, &delAction, &updAction); err != nil {
			rows.Close()
			return fmt.Errorf("외래키 정보 스캔 실패: %w", err)
		}
		tbl := tables[strings.ToLower(ns+"."+tblName)]
		if tbl == nil {
			continue
		}
		key := strings.ToLower(ns+"."+tblName) + "\x00" + fkName
		fk := fkSeen[key]
		if fk == nil {
			if strings.EqualFold(refNS, ns) {
				refNS = ""
			}
			fk = &schema.ForeignKey{
				Name: fkName, RefNamespace: refNS, RefTable: refTable,
				Columns: []string{}, RefColumns: []string{},
				OnDelete: delAction, OnUpdate: updAction,
			}
			fkSeen[key] = fk
			tbl.ForeignKeys = append(tbl.ForeignKeys, fk)
		}
		fk.Columns = append(fk.Columns, col)
		fk.RefColumns = append(fk.RefColumns, refCol)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("외래키 순회 실패: %w", err)
	}

	// 체크 제약
	crows, err := db.QueryContext(ctx, `
		SELECT s.name, tb.name, cc.name, ISNULL(cc.definition, '')
		FROM sys.check_constraints cc
		JOIN sys.tables tb ON tb.object_id = cc.parent_object_id
		JOIN sys.schemas s ON s.schema_id = tb.schema_id
		WHERE `+tableClause, args...)
	if err != nil {
		s.AddNote("체크 제약을 읽지 못했습니다: %v", err)
	} else {
		for crows.Next() {
			var ns, tblName, name, def string
			if err := crows.Scan(&ns, &tblName, &name, &def); err != nil {
				crows.Close()
				return fmt.Errorf("체크 제약 스캔 실패: %w", err)
			}
			if tbl := tables[strings.ToLower(ns+"."+tblName)]; tbl != nil {
				tbl.Checks = append(tbl.Checks, &schema.Check{Name: name, Expression: trimOuterParens(def)})
			}
		}
		crows.Close()
		if err := crows.Err(); err != nil {
			return fmt.Errorf("체크 제약 순회 실패: %w", err)
		}
	}

	// 뷰
	vrows, err := db.QueryContext(ctx, `
		SELECT s.name, v.name, ISNULL(m.definition, '')
		FROM sys.views v
		JOIN sys.schemas s ON s.schema_id = v.schema_id
		LEFT JOIN sys.sql_modules m ON m.object_id = v.object_id
		WHERE `+viewClause, args...)
	if err != nil {
		s.AddNote("뷰 목록을 읽지 못했습니다: %v", err)
		return nil
	}
	defer vrows.Close()
	for vrows.Next() {
		var ns, name, def string
		if err := vrows.Scan(&ns, &name, &def); err != nil {
			return fmt.Errorf("뷰 정보 스캔 실패: %w", err)
		}
		s.Views = append(s.Views, &schema.View{
			Namespace: ns, Name: name, Definition: extractViewBody(def),
		})
	}
	if err := vrows.Err(); err != nil {
		return fmt.Errorf("뷰 순회 실패: %w", err)
	}
	return nil
}

// mssqlRawType은 sys.columns의 수치 정보를 타입 문자열로 재구성한다.
func mssqlRawType(typeName string, maxLength, precision, scale int) string {
	switch strings.ToLower(typeName) {
	case "nvarchar", "nchar":
		if maxLength == -1 {
			return typeName + "(max)"
		}
		// nchar/nvarchar의 max_length는 바이트 수라 문자 수로 환산한다.
		return fmt.Sprintf("%s(%d)", typeName, maxLength/2)
	case "varchar", "char", "varbinary", "binary":
		if maxLength == -1 {
			return typeName + "(max)"
		}
		return fmt.Sprintf("%s(%d)", typeName, maxLength)
	case "decimal", "numeric":
		return fmt.Sprintf("%s(%d,%d)", typeName, precision, scale)
	case "datetime2", "time", "datetimeoffset":
		return fmt.Sprintf("%s(%d)", typeName, scale)
	}
	return typeName
}

// trimOuterParens는 MS-SQL이 정의문에 붙이는 중복 괄호를 제거한다.
func trimOuterParens(s string) string {
	out := strings.TrimSpace(s)
	for strings.HasPrefix(out, "(") && strings.HasSuffix(out, ")") && balanced(out[1:len(out)-1]) {
		out = strings.TrimSpace(out[1 : len(out)-1])
	}
	return out
}

// balanced는 문자열의 괄호가 균형을 이루는지 확인한다.
// "(a) AND (b)" 처럼 바깥 괄호를 벗기면 안 되는 경우를 구분하기 위해 필요하다.
func balanced(s string) bool {
	depth := 0
	for _, r := range s {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
			if depth < 0 {
				return false
			}
		}
	}
	return depth == 0
}

// createViewRE는 CREATE VIEW 헤더와 본문을 분리한다.
//
// " AS " 문자열을 찾는 단순한 방법은 두 가지로 깨진다:
// 헤더의 AS가 개행으로 끝나는 경우(공백 매칭 실패)와, 본문의
// "col COLLATE x AS alias" 같은 별칭 AS를 헤더로 오인하는 경우다.
// 비탐욕 매칭 + 단어 경계로 CREATE VIEW 직후의 첫 AS 토큰만 잡는다.
var createViewRE = regexp.MustCompile(`(?is)\bCREATE\s+(?:OR\s+(?:ALTER|REPLACE)\s+)?VIEW\b.*?\bAS\b\s*(.*)`)

// extractViewBody는 "CREATE VIEW x AS SELECT ..." 에서 본문(SELECT 이후)만 남긴다.
// IR은 뷰 본문만 담고, DDL 생성 시 CREATE 구문을 dialect에 맞게 다시 붙인다.
func extractViewBody(def string) string {
	if m := createViewRE.FindStringSubmatch(def); m != nil {
		return strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(m[1]), ";"))
	}
	return strings.TrimSpace(def)
}
