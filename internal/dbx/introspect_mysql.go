package dbx

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"dbstudio/internal/schema"
)

// introspectMySQL은 information_schema에서 스키마를 읽는다.
//
// 대상 스키마는 커넥션의 database_name이다. 비어 있으면 DATABASE()를 사용한다.
// 권한 부족으로 일부 조회가 실패해도 나머지는 계속 읽고 Notes에 기록한다 —
// "아무것도 못 읽음"과 "일부만 읽음"을 사용자가 구분할 수 있어야 한다.
func introspectMySQL(ctx context.Context, db *sql.DB, t Target, s *schema.Schema) error {
	dbName := strings.TrimSpace(t.Conn.DatabaseName)
	if dbName == "" {
		if err := db.QueryRowContext(ctx, `SELECT DATABASE()`).Scan(&dbName); err != nil {
			return fmt.Errorf("현재 데이터베이스를 확인할 수 없습니다: %w", err)
		}
		if dbName == "" {
			return fmt.Errorf("데이터베이스가 지정되지 않았습니다")
		}
		s.Name = dbName
	}

	tables := map[string]*schema.Table{}

	// 테이블 목록 + 주석 + 통계
	rows, err := db.QueryContext(ctx, `
		SELECT TABLE_NAME, IFNULL(TABLE_COMMENT, ''), IFNULL(ENGINE, ''),
		       IFNULL(TABLE_COLLATION, ''), IFNULL(TABLE_ROWS, 0),
		       IFNULL(DATA_LENGTH, 0) + IFNULL(INDEX_LENGTH, 0)
		FROM information_schema.TABLES
		WHERE TABLE_SCHEMA = ? AND TABLE_TYPE = 'BASE TABLE'`, dbName)
	if err != nil {
		return fmt.Errorf("테이블 목록 조회 실패: %w", err)
	}
	for rows.Next() {
		var name, comment, engine, collation string
		var rowCount, size int64
		if err := rows.Scan(&name, &comment, &engine, &collation, &rowCount, &size); err != nil {
			rows.Close()
			return fmt.Errorf("테이블 정보 스캔 실패: %w", err)
		}
		tbl := &schema.Table{
			Name: name, Comment: comment,
			Columns: []*schema.Column{}, Indexes: []*schema.Index{},
			ForeignKeys: []*schema.ForeignKey{}, Checks: []*schema.Check{},
			Options:     map[string]string{},
			RowEstimate: rowCount, SizeBytes: size,
		}
		if engine != "" {
			tbl.Options["engine"] = engine
		}
		if collation != "" {
			tbl.Options["collation"] = collation
			// charset은 collation의 접두사다 (utf8mb4_general_ci → utf8mb4).
			if i := strings.Index(collation, "_"); i > 0 {
				tbl.Options["charset"] = collation[:i]
			}
		}
		tables[strings.ToLower(name)] = tbl
		s.Tables = append(s.Tables, tbl)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("테이블 목록 순회 실패: %w", err)
	}
	if len(s.Tables) == 0 {
		return nil
	}

	// 컬럼
	rows, err = db.QueryContext(ctx, `
		SELECT TABLE_NAME, COLUMN_NAME, ORDINAL_POSITION, COLUMN_TYPE, IS_NULLABLE,
		       COLUMN_DEFAULT, IFNULL(EXTRA, ''), IFNULL(COLUMN_COMMENT, ''),
		       IFNULL(COLLATION_NAME, ''), IFNULL(GENERATION_EXPRESSION, '')
		FROM information_schema.COLUMNS
		WHERE TABLE_SCHEMA = ?
		ORDER BY TABLE_NAME, ORDINAL_POSITION`, dbName)
	if err != nil {
		return fmt.Errorf("컬럼 조회 실패: %w", err)
	}
	for rows.Next() {
		var tblName, colName, colType, nullable, extra, comment, collation, genExpr string
		var pos int
		var def sql.NullString
		if err := rows.Scan(&tblName, &colName, &pos, &colType, &nullable, &def,
			&extra, &comment, &collation, &genExpr); err != nil {
			rows.Close()
			return fmt.Errorf("컬럼 정보 스캔 실패: %w", err)
		}
		tbl := tables[strings.ToLower(tblName)]
		if tbl == nil {
			continue // 뷰의 컬럼
		}
		col := &schema.Column{
			Name: colName, Position: pos,
			Type: schema.ParseType("mysql", colType), RawType: colType,
			Nullable: nullable == "YES",
			Identity: strings.Contains(strings.ToLower(extra), "auto_increment"),
			Comment:  comment, Collation: collation,
		}
		if genExpr != "" {
			col.Generated = genExpr
		}
		if def.Valid {
			col.HasDefault = true
			col.Default = mysqlDefaultExpr(def.String, col.Type)
		}
		tbl.Columns = append(tbl.Columns, col)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("컬럼 순회 실패: %w", err)
	}

	// 인덱스 + 기본키. MySQL은 PRIMARY라는 이름의 인덱스로 기본키를 표현한다.
	rows, err = db.QueryContext(ctx, `
		SELECT TABLE_NAME, INDEX_NAME, NON_UNIQUE, SEQ_IN_INDEX,
		       IFNULL(COLUMN_NAME, ''), IFNULL(COLLATION, 'A'),
		       IFNULL(INDEX_TYPE, ''), IFNULL(EXPRESSION, '')
		FROM information_schema.STATISTICS
		WHERE TABLE_SCHEMA = ?
		ORDER BY TABLE_NAME, INDEX_NAME, SEQ_IN_INDEX`, dbName)
	if err != nil {
		return fmt.Errorf("인덱스 조회 실패: %w", err)
	}
	type idxAcc struct {
		idx *schema.Index
		pk  *schema.PrimaryKey
	}
	acc := map[string]*idxAcc{}
	for rows.Next() {
		var tblName, idxName, collation, idxType, expr string
		var nonUnique, seq int
		var colName string
		if err := rows.Scan(&tblName, &idxName, &nonUnique, &seq, &colName,
			&collation, &idxType, &expr); err != nil {
			rows.Close()
			return fmt.Errorf("인덱스 정보 스캔 실패: %w", err)
		}
		tbl := tables[strings.ToLower(tblName)]
		if tbl == nil {
			continue
		}
		key := strings.ToLower(tblName) + "\x00" + idxName
		a := acc[key]
		if a == nil {
			a = &idxAcc{}
			if idxName == "PRIMARY" {
				a.pk = &schema.PrimaryKey{Name: "PRIMARY", Columns: []string{}}
				tbl.PrimaryKey = a.pk
			} else {
				a.idx = &schema.Index{
					Name: idxName, Unique: nonUnique == 0,
					Type: strings.ToLower(idxType), Columns: []schema.IndexPart{},
				}
				tbl.Indexes = append(tbl.Indexes, a.idx)
			}
			acc[key] = a
		}
		if a.pk != nil {
			a.pk.Columns = append(a.pk.Columns, colName)
			continue
		}
		a.idx.Columns = append(a.idx.Columns, schema.IndexPart{
			Column:     colName,
			Descending: collation == "D",
			Expression: expr,
		})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("인덱스 순회 실패: %w", err)
	}

	// 외래키
	rows, err = db.QueryContext(ctx, `
		SELECT k.TABLE_NAME, k.CONSTRAINT_NAME, k.COLUMN_NAME, k.ORDINAL_POSITION,
		       k.REFERENCED_TABLE_NAME, k.REFERENCED_COLUMN_NAME,
		       IFNULL(r.DELETE_RULE, ''), IFNULL(r.UPDATE_RULE, '')
		FROM information_schema.KEY_COLUMN_USAGE k
		JOIN information_schema.REFERENTIAL_CONSTRAINTS r
		  ON r.CONSTRAINT_SCHEMA = k.CONSTRAINT_SCHEMA
		 AND r.CONSTRAINT_NAME = k.CONSTRAINT_NAME
		WHERE k.TABLE_SCHEMA = ? AND k.REFERENCED_TABLE_NAME IS NOT NULL
		ORDER BY k.TABLE_NAME, k.CONSTRAINT_NAME, k.ORDINAL_POSITION`, dbName)
	if err != nil {
		return fmt.Errorf("외래키 조회 실패: %w", err)
	}
	fkAcc := map[string]*schema.ForeignKey{}
	for rows.Next() {
		var tblName, cname, colName, refTable, refCol, delRule, updRule string
		var pos int
		if err := rows.Scan(&tblName, &cname, &colName, &pos, &refTable, &refCol,
			&delRule, &updRule); err != nil {
			rows.Close()
			return fmt.Errorf("외래키 정보 스캔 실패: %w", err)
		}
		tbl := tables[strings.ToLower(tblName)]
		if tbl == nil {
			continue
		}
		key := strings.ToLower(tblName) + "\x00" + cname
		fk := fkAcc[key]
		if fk == nil {
			fk = &schema.ForeignKey{
				Name: cname, RefTable: refTable,
				Columns: []string{}, RefColumns: []string{},
				OnDelete: delRule, OnUpdate: updRule,
			}
			fkAcc[key] = fk
			tbl.ForeignKeys = append(tbl.ForeignKeys, fk)
		}
		fk.Columns = append(fk.Columns, colName)
		fk.RefColumns = append(fk.RefColumns, refCol)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("외래키 순회 실패: %w", err)
	}

	// 체크 제약: MySQL 8.0.16+ / MariaDB 10.2+ 에만 존재한다.
	// CHECK_CONSTRAINTS에는 TABLE_NAME이 없으므로 TABLE_CONSTRAINTS와 조인해야 한다.
	crows, err := db.QueryContext(ctx, `
		SELECT tc.TABLE_NAME, cc.CONSTRAINT_NAME, cc.CHECK_CLAUSE
		FROM information_schema.CHECK_CONSTRAINTS cc
		JOIN information_schema.TABLE_CONSTRAINTS tc
		  ON tc.CONSTRAINT_SCHEMA = cc.CONSTRAINT_SCHEMA
		 AND tc.CONSTRAINT_NAME = cc.CONSTRAINT_NAME
		WHERE cc.CONSTRAINT_SCHEMA = ? AND tc.CONSTRAINT_TYPE = 'CHECK'`, dbName)
	if err != nil {
		s.AddNote("체크 제약을 읽지 못했습니다 (MySQL 8.0.16 이전 버전이거나 권한 부족): %v", err)
	} else {
		for crows.Next() {
			var tblName, cname, clause string
			if err := crows.Scan(&tblName, &cname, &clause); err != nil {
				crows.Close()
				return fmt.Errorf("체크 제약 스캔 실패: %w", err)
			}
			if tbl := tables[strings.ToLower(tblName)]; tbl != nil {
				tbl.Checks = append(tbl.Checks, &schema.Check{Name: cname, Expression: clause})
			}
		}
		crows.Close()
		if err := crows.Err(); err != nil {
			return fmt.Errorf("체크 제약 순회 실패: %w", err)
		}
	}

	// 뷰
	vrows, err := db.QueryContext(ctx, `
		SELECT TABLE_NAME, IFNULL(VIEW_DEFINITION, '')
		FROM information_schema.VIEWS WHERE TABLE_SCHEMA = ?`, dbName)
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
		s.Views = append(s.Views, &schema.View{Name: name, Definition: def})
	}
	if err := vrows.Err(); err != nil {
		return fmt.Errorf("뷰 순회 실패: %w", err)
	}
	return nil
}

// mysqlDefaultExpr는 information_schema가 돌려주는 기본값을 DDL에 쓸 수 있는 표현식으로 만든다.
// MySQL은 문자열 기본값을 따옴표 없이 돌려주므로 직접 인용해야 하고,
// 함수 기본값(CURRENT_TIMESTAMP 등)은 인용하면 안 된다.
func mysqlDefaultExpr(raw string, t schema.LogicalType) string {
	upper := strings.ToUpper(strings.TrimSpace(raw))
	switch {
	case upper == "CURRENT_TIMESTAMP" || strings.HasPrefix(upper, "CURRENT_TIMESTAMP("):
		return raw
	case upper == "NULL":
		return "NULL"
	case strings.HasPrefix(raw, "(") && strings.HasSuffix(raw, ")"):
		// MySQL 8은 식 기본값을 괄호로 감싸 돌려준다.
		return raw
	}
	switch t.Base {
	case schema.TypeChar, schema.TypeVarchar, schema.TypeText, schema.TypeEnum,
		schema.TypeJSON, schema.TypeDate, schema.TypeTime,
		schema.TypeTimestamp, schema.TypeTimestampTZ, schema.TypeUUID:
		return "'" + strings.ReplaceAll(raw, "'", "''") + "'"
	}
	return raw
}
