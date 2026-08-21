package dbx

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"dbstudio/internal/schema"
)

// introspectOracle은 all_* 데이터 딕셔너리 뷰에서 스키마를 읽는다.
//
// user_* 대신 all_*을 쓰는 이유: 접속 계정이 다른 스키마의 객체를 관리하는 경우가
// 흔하고, owner를 명시할 수 있어야 한다. 기본 owner는 접속 계정이다.
func introspectOracle(ctx context.Context, db *sql.DB, t Target, s *schema.Schema) error {
	owner := strings.ToUpper(strings.TrimSpace(t.Opt("owner", "")))
	if owner == "" {
		if err := db.QueryRowContext(ctx, `SELECT USER FROM DUAL`).Scan(&owner); err != nil {
			return fmt.Errorf("접속 계정을 확인할 수 없습니다: %w", err)
		}
	}
	s.Name = owner

	tables := map[string]*schema.Table{}

	// 테이블 + 주석 + 통계 (num_rows는 마지막 통계 수집 시점의 값이다)
	rows, err := db.QueryContext(ctx, `
		SELECT tt.table_name,
		       NVL(tc.comments, ' '),
		       NVL(tt.num_rows, 0),
		       NVL(tt.blocks, 0) * 8192
		FROM all_tables tt
		LEFT JOIN all_tab_comments tc
		       ON tc.owner = tt.owner AND tc.table_name = tt.table_name
		WHERE tt.owner = :1`, owner)
	if err != nil {
		return fmt.Errorf("테이블 목록 조회 실패: %w", err)
	}
	for rows.Next() {
		var name, comment string
		var rowCount, size int64
		if err := rows.Scan(&name, &comment, &rowCount, &size); err != nil {
			rows.Close()
			return fmt.Errorf("테이블 정보 스캔 실패: %w", err)
		}
		tbl := &schema.Table{
			Namespace: owner, Name: name, Comment: strings.TrimSpace(comment),
			Columns: []*schema.Column{}, Indexes: []*schema.Index{},
			ForeignKeys: []*schema.ForeignKey{}, Checks: []*schema.Check{},
			RowEstimate: rowCount, SizeBytes: size,
		}
		tables[strings.ToLower(owner+"."+name)] = tbl
		s.Tables = append(s.Tables, tbl)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("테이블 목록 순회 실패: %w", err)
	}
	if len(s.Tables) == 0 {
		s.AddNote("소유자 %s 에서 테이블을 찾지 못했습니다", owner)
		return nil
	}

	// 컬럼. data_default는 LONG 타입이라 NVL/함수를 적용할 수 없다(ORA-00932).
	// 그대로 선택해 NullString으로 받는다.
	rows, err = db.QueryContext(ctx, `
		SELECT c.table_name, c.column_name, c.column_id, c.data_type,
		       NVL(c.data_length, 0), NVL(c.data_precision, 0), NVL(c.data_scale, 0),
		       c.nullable, c.data_default,
		       NVL(c.identity_column, 'NO'), NVL(c.virtual_column, 'NO'),
		       NVL(cc.comments, ' '), NVL(c.char_length, 0)
		FROM all_tab_cols c
		LEFT JOIN all_col_comments cc
		       ON cc.owner = c.owner AND cc.table_name = c.table_name
		      AND cc.column_name = c.column_name
		WHERE c.owner = :1 AND c.hidden_column = 'NO'
		ORDER BY c.table_name, c.column_id`, owner)
	if err != nil {
		return fmt.Errorf("컬럼 조회 실패: %w", err)
	}
	for rows.Next() {
		var tblName, colName, dataType, nullable, identity, virtual, comment string
		var def sql.NullString
		var colID, dataLength, precision, scale, charLength int
		if err := rows.Scan(&tblName, &colName, &colID, &dataType,
			&dataLength, &precision, &scale, &nullable, &def,
			&identity, &virtual, &comment, &charLength); err != nil {
			rows.Close()
			return fmt.Errorf("컬럼 정보 스캔 실패: %w", err)
		}
		tbl := tables[strings.ToLower(owner+"."+tblName)]
		if tbl == nil {
			continue
		}
		raw := oracleRawType(dataType, dataLength, charLength, precision, scale)
		col := &schema.Column{
			Name: colName, Position: colID,
			Type: schema.ParseType("oracle", raw), RawType: raw,
			Nullable: nullable == "Y",
			Identity: identity == "YES",
			Comment:  strings.TrimSpace(comment),
		}
		trimmedDef := strings.TrimSpace(def.String)
		if virtual == "YES" {
			col.Generated = trimmedDef
		} else if trimmedDef != "" && !strings.EqualFold(trimmedDef, "NULL") {
			col.HasDefault = true
			col.Default = trimmedDef
		}
		tbl.Columns = append(tbl.Columns, col)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("컬럼 순회 실패: %w", err)
	}

	// 제약: P(기본키), R(외래키), C(체크/NOT NULL)
	rows, err = db.QueryContext(ctx, `
		SELECT c.table_name, c.constraint_name, c.constraint_type,
		       NVL(cc.column_name, ' '), NVL(cc.position, 0),
		       NVL(c.r_owner, ' '), NVL(rc.table_name, ' '), NVL(rc.column_name, ' '),
		       NVL(c.delete_rule, ' '), NVL(c.search_condition_vc, ' ')
		FROM all_constraints c
		LEFT JOIN all_cons_columns cc
		       ON cc.owner = c.owner AND cc.constraint_name = c.constraint_name
		LEFT JOIN all_cons_columns rc
		       ON rc.owner = c.r_owner AND rc.constraint_name = c.r_constraint_name
		      AND rc.position = cc.position
		WHERE c.owner = :1 AND c.constraint_type IN ('P', 'R', 'C')
		ORDER BY c.table_name, c.constraint_name, cc.position`, owner)
	if err != nil {
		return fmt.Errorf("제약 조회 실패: %w", err)
	}
	pkSeen := map[string]*schema.PrimaryKey{}
	fkSeen := map[string]*schema.ForeignKey{}
	ckSeen := map[string]bool{}
	for rows.Next() {
		var tblName, cname, ctype, colName, rOwner, refTable, refCol, delRule, condition string
		var position int
		if err := rows.Scan(&tblName, &cname, &ctype, &colName, &position,
			&rOwner, &refTable, &refCol, &delRule, &condition); err != nil {
			rows.Close()
			return fmt.Errorf("제약 정보 스캔 실패: %w", err)
		}
		tbl := tables[strings.ToLower(owner+"."+tblName)]
		if tbl == nil {
			continue
		}
		key := strings.ToLower(tblName) + "\x00" + cname
		colName = strings.TrimSpace(colName)

		switch ctype {
		case "P":
			pk := pkSeen[key]
			if pk == nil {
				pk = &schema.PrimaryKey{Name: cname, Columns: []string{}}
				pkSeen[key] = pk
				tbl.PrimaryKey = pk
			}
			if colName != "" {
				pk.Columns = append(pk.Columns, colName)
			}
		case "R":
			fk := fkSeen[key]
			if fk == nil {
				ns := strings.TrimSpace(rOwner)
				if strings.EqualFold(ns, owner) {
					ns = ""
				}
				fk = &schema.ForeignKey{
					Name: cname, RefNamespace: ns, RefTable: strings.TrimSpace(refTable),
					Columns: []string{}, RefColumns: []string{},
					OnDelete: strings.TrimSpace(delRule),
				}
				fkSeen[key] = fk
				tbl.ForeignKeys = append(tbl.ForeignKeys, fk)
			}
			if colName != "" {
				fk.Columns = append(fk.Columns, colName)
			}
			if rc := strings.TrimSpace(refCol); rc != "" {
				fk.RefColumns = append(fk.RefColumns, rc)
			}
		case "C":
			cond := strings.TrimSpace(condition)
			// Oracle은 NOT NULL도 체크 제약으로 표현한다. 컬럼의 nullable로 이미
			// 표현되므로 중복 기록하면 diff에서 계속 변경으로 잡힌다.
			if cond == "" || isOracleNotNullCheck(cond) || ckSeen[key] {
				continue
			}
			ckSeen[key] = true
			tbl.Checks = append(tbl.Checks, &schema.Check{Name: cname, Expression: cond})
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("제약 순회 실패: %w", err)
	}

	// 인덱스. 제약이 만든 인덱스는 제외한다.
	rows, err = db.QueryContext(ctx, `
		SELECT i.table_name, i.index_name, i.uniqueness, i.index_type,
		       ic.column_name, ic.column_position, NVL(ic.descend, 'ASC')
		FROM all_indexes i
		JOIN all_ind_columns ic
		  ON ic.index_owner = i.owner AND ic.index_name = i.index_name
		WHERE i.owner = :1
		  AND NOT EXISTS (
		        SELECT 1 FROM all_constraints c
		        WHERE c.owner = i.owner AND c.index_name = i.index_name
		          AND c.constraint_type IN ('P', 'U'))
		ORDER BY i.table_name, i.index_name, ic.column_position`, owner)
	if err != nil {
		s.AddNote("인덱스를 읽지 못했습니다: %v", err)
	} else {
		idxSeen := map[string]*schema.Index{}
		for rows.Next() {
			var tblName, idxName, uniqueness, idxType, colName, descend string
			var position int
			if err := rows.Scan(&tblName, &idxName, &uniqueness, &idxType,
				&colName, &position, &descend); err != nil {
				rows.Close()
				return fmt.Errorf("인덱스 정보 스캔 실패: %w", err)
			}
			tbl := tables[strings.ToLower(owner+"."+tblName)]
			if tbl == nil {
				continue
			}
			key := strings.ToLower(tblName) + "\x00" + idxName
			idx := idxSeen[key]
			if idx == nil {
				idx = &schema.Index{
					Name: idxName, Unique: uniqueness == "UNIQUE",
					Type: strings.ToLower(idxType), Columns: []schema.IndexPart{},
				}
				idxSeen[key] = idx
				tbl.Indexes = append(tbl.Indexes, idx)
			}
			idx.Columns = append(idx.Columns, schema.IndexPart{
				Column: colName, Descending: strings.EqualFold(descend, "DESC"),
			})
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("인덱스 순회 실패: %w", err)
		}
	}

	// 뷰. text 컬럼은 LONG이라 조회가 실패할 수 있어 실패를 허용한다.
	vrows, err := db.QueryContext(ctx, `
		SELECT view_name, NVL(text_vc, ' ') FROM all_views WHERE owner = :1`, owner)
	if err != nil {
		s.AddNote("뷰 목록을 읽지 못했습니다: %v", err)
		return nil
	}
	defer vrows.Close()
	for vrows.Next() {
		var name, def string
		if err := vrows.Scan(&name, &def); err != nil {
			return fmt.Errorf("뷰 정보 스캔 실패: %w", err)
		}
		s.Views = append(s.Views, &schema.View{
			Namespace: owner, Name: name, Definition: strings.TrimSpace(def),
		})
	}
	if err := vrows.Err(); err != nil {
		return fmt.Errorf("뷰 순회 실패: %w", err)
	}
	return nil
}

// oracleRawType은 딕셔너리 수치 정보를 타입 문자열로 재구성한다.
func oracleRawType(dataType string, dataLength, charLength, precision, scale int) string {
	upper := strings.ToUpper(dataType)
	switch {
	case strings.HasPrefix(upper, "VARCHAR2"), strings.HasPrefix(upper, "NVARCHAR2"),
		upper == "CHAR", upper == "NCHAR":
		n := charLength
		if n == 0 {
			n = dataLength
		}
		return fmt.Sprintf("%s(%d)", dataType, n)
	case upper == "NUMBER":
		if precision == 0 {
			return "NUMBER"
		}
		return fmt.Sprintf("NUMBER(%d,%d)", precision, scale)
	case upper == "RAW":
		return fmt.Sprintf("RAW(%d)", dataLength)
	case strings.HasPrefix(upper, "TIMESTAMP"):
		// "TIMESTAMP(6) WITH TIME ZONE" 형태로 이미 완성되어 온다.
		return dataType
	}
	return dataType
}

// isOracleNotNullCheck은 "COL" IS NOT NULL 형태의 자동 생성 제약인지 판별한다.
func isOracleNotNullCheck(cond string) bool {
	normalized := strings.ToUpper(strings.Join(strings.Fields(cond), " "))
	return strings.HasSuffix(normalized, "IS NOT NULL") &&
		!strings.Contains(normalized, " AND ") &&
		!strings.Contains(normalized, " OR ")
}
