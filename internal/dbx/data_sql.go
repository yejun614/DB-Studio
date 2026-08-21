package dbx

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"dbstudio/internal/model"
)

// 관계형 DB의 데이터 조회·수정·SQL 실행.
//
// 이 파일의 규칙 하나: **식별자는 절대로 사용자 입력에서 오지 않는다.** 테이블 이름,
// 컬럼 이름, 정렬 기준은 모두 요청에 담겨 오지만, 쿼리에 넣기 전에 DB에서 읽은 실제
// 목록과 대조해 그 목록에 있는 문자열로 치환한다. 값은 파라미터로만 보낸다.
// 이 두 가지를 지키면 SQL 주입은 구조적으로 불가능해진다 — 인용만으로 막으려 하면
// 언젠가 인용을 빠뜨린 경로가 생긴다.

func (a *sqlAdapter) DataCapabilities() DataCapabilities {
	return DataCapabilities{
		Browse: true, Filter: true, Sort: true, Mutate: true, Statement: true,
		Preview:        true,
		StatementCheck: true,
		Explain:        explainPrefix(a.kind),
		StatementLabel: "SQL",
		StatementHelp:  "여러 문장을 세미콜론으로 구분해 실행할 수 있습니다",
	}
}

// explainPrefix는 실행 계획을 보기 위해 문장 앞에 붙일 구절이다.
//
// PostgreSQL·MySQL은 ANALYZE를 붙이면 **문장을 실제로 실행한 뒤** 걸린 시간을
// 알려준다. 그래서 조회 문장에만 의미가 있고, 읽기 전용 판정도 그 사실을 알아야
// 한다(isReadOnlyStatement 참고) — EXPLAIN이 붙었다고 안전한 것이 아니다.
//
// SQLite에는 실측 개념이 없어 계획만 보여준다. Oracle·MS-SQL은 접두사만으로
// 되지 않으므로 빈 문자열이고, 화면은 버튼을 그리지 않는다.
func explainPrefix(kind model.DBKind) string {
	switch kind {
	case model.KindPostgres:
		return "EXPLAIN (ANALYZE, BUFFERS) "
	case model.KindMySQL:
		return "EXPLAIN ANALYZE "
	case model.KindSQLite:
		return "EXPLAIN QUERY PLAN "
	}
	return ""
}

// ListObjects는 조회 가능한 테이블과 뷰를 나열한다.
func (a *sqlAdapter) ListObjects(ctx context.Context, t Target) ([]DataObject, error) {
	db, err := a.open(t, 2)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	// owner 옵션은 introspect(구조 화면)와 같은 값을 써야 한다. 한쪽만 존중하면
	// 같은 커넥션인데 구조 화면과 데이터 화면의 테이블 목록이 달라진다.
	query, args := listObjectsSQL(a.kind, t.Conn.DatabaseName, strings.TrimSpace(t.Opt("owner", "")))
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("목록 조회 실패: %w", err)
	}
	defer rows.Close()

	out := []DataObject{}
	for rows.Next() {
		var o DataObject
		var ns, comment sql.NullString
		var count sql.NullInt64
		if err := rows.Scan(&ns, &o.Name, &o.Kind, &count, &comment); err != nil {
			return nil, fmt.Errorf("목록 스캔 실패: %w", err)
		}
		o.Namespace = ns.String
		o.Comment = comment.String
		o.RowCount = -1
		if count.Valid {
			o.RowCount = count.Int64
		}
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("목록 순회 실패: %w", err)
	}
	return out, nil
}

// describeColumns는 컬럼 메타데이터를 읽는다.
//
// information_schema를 다시 조회하는 대신 `SELECT * FROM t WHERE 1=0`의 결과
// 메타데이터를 쓴다. 방언마다 다른 카탈로그 쿼리를 다섯 벌 쓰지 않아도 되고,
// 무엇보다 **실제로 그 쿼리가 돌려줄 컬럼**을 그대로 알려준다는 점이 중요하다.
// 카탈로그와 결과가 어긋나는 경우(뷰, 권한에 따른 컬럼 숨김)에도 틀리지 않는다.
func (a *sqlAdapter) describeColumns(ctx context.Context, db *sql.DB, ref TableRef) ([]DataColumn, error) {
	probe := "SELECT * FROM " + qualify(a.kind, ref) + " WHERE 1 = 0"
	rows, err := db.QueryContext(ctx, probe)
	if err != nil {
		return nil, fmt.Errorf("컬럼 정보를 읽지 못했습니다: %w", err)
	}
	defer rows.Close()

	types, err := rows.ColumnTypes()
	if err != nil {
		return nil, fmt.Errorf("컬럼 타입을 읽지 못했습니다: %w", err)
	}
	cols := make([]DataColumn, 0, len(types))
	for _, ct := range types {
		nullable, ok := ct.Nullable()
		if !ok {
			nullable = true // 모르면 넓게 잡는다. 좁게 잡으면 멀쩡한 입력을 거부한다.
		}
		typeName := ct.DatabaseTypeName()
		cols = append(cols, DataColumn{
			Name:     ct.Name(),
			Type:     typeName,
			Nullable: nullable,
			Numeric:  isNumericType(typeName),
		})
	}
	return cols, nil
}

// primaryKey는 기본키 컬럼 이름을 순서대로 반환한다.
// 읽지 못하면 빈 슬라이스다 — 그 경우 편집이 막힐 뿐 조회는 계속 된다.
func (a *sqlAdapter) primaryKey(ctx context.Context, db *sql.DB, ref TableRef) []string {
	query, args := primaryKeySQL(a.kind, ref)
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()

	out := []string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil
		}
		out = append(out, name)
	}
	if rows.Err() != nil {
		return nil
	}
	return out
}

// foreignKeys는 컬럼 이름 → 대상 자리를 읽는다.
//
// 읽지 못하면 빈 맵이다. 외래키를 못 읽는 것은 조회를 막을 이유가 되지 않는다 —
// 화면에서 "따라가기"가 없을 뿐, 값은 그대로 보인다.
func (a *sqlAdapter) foreignKeys(ctx context.Context, db *sql.DB, ref TableRef) map[string]ColumnRef {
	out := map[string]ColumnRef{}
	query, args := foreignKeySQL(a.kind, ref)
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return out
	}
	defer rows.Close()

	for rows.Next() {
		var col, ns, table, refCol string
		if err := rows.Scan(&col, &ns, &table, &refCol); err != nil {
			return out
		}
		if col == "" || table == "" {
			continue
		}
		// 같은 컬럼에 외래키가 둘 이상 걸린 경우(드물다)는 먼저 읽은 것을 쓴다.
		// 어느 쪽으로 따라가도 틀리지 않고, 고르게 하는 것은 화면을 복잡하게만 한다.
		if _, ok := out[strings.ToLower(col)]; ok {
			continue
		}
		out[strings.ToLower(col)] = ColumnRef{Namespace: ns, Table: table, Column: refCol}
	}
	return out
}

// QueryRows는 한 페이지를 조회한다.
func (a *sqlAdapter) QueryRows(ctx context.Context, t Target, q RowQuery) (*RowPage, error) {
	if q.Table.Empty() {
		return nil, fmt.Errorf("대상 테이블을 지정하세요")
	}
	db, err := a.open(t, 3)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	cols, err := a.describeColumns(ctx, db, q.Table)
	if err != nil {
		return nil, err
	}
	pk := a.primaryKey(ctx, db, q.Table)
	fks := a.foreignKeys(ctx, db, q.Table)
	byName := make(map[string]*DataColumn, len(cols))
	for i := range cols {
		if slices.Contains(pk, cols[i].Name) {
			cols[i].PK = true
		}
		if ref, ok := fks[strings.ToLower(cols[i].Name)]; ok {
			r := ref
			cols[i].FK = &r
		}
		byName[cols[i].Name] = &cols[i]
	}

	params := newParams(a.kind)
	where, err := a.buildWhere(params, byName, cols, q.Filters, q.Search)
	if err != nil {
		return nil, err
	}

	// 정렬 기준을 정한다. 사용자가 고르지 않았고 기본키가 있으면 기본키로 정렬한다.
	// 정렬이 없으면 DB는 순서를 보장하지 않으므로 페이지를 넘길 때 같은 행이
	// 다시 나오거나 아예 건너뛰어진다 — 목록으로서 신뢰할 수 없게 된다.
	orderCols := []string{}
	if q.OrderBy != "" {
		col, ok := byName[q.OrderBy]
		if !ok {
			return nil, fmt.Errorf("존재하지 않는 컬럼으로 정렬할 수 없습니다: %s", q.OrderBy)
		}
		dir := " ASC"
		if q.Desc {
			dir = " DESC"
		}
		orderCols = append(orderCols, quoteIdent(a.kind, col.Name)+dir)
	} else {
		for _, name := range pk {
			orderCols = append(orderCols, quoteIdent(a.kind, name))
		}
	}
	orderClause := ""
	if len(orderCols) > 0 {
		orderClause = " ORDER BY " + strings.Join(orderCols, ", ")
	}

	limit := q.EffectiveLimit()
	// 한 행을 더 읽어 다음 페이지 존재 여부를 판단한다.
	// count(*)를 매번 돌리는 것보다 훨씬 싸고, 사용자가 실제로 알고 싶은 것은
	// "다음이 있는가"이지 "전부 몇 개인가"가 아닌 경우가 많다.
	page := pageClause(a.kind, params, limit+1, q.Offset, orderClause != "")

	query := "SELECT * FROM " + qualify(a.kind, q.Table)
	if where != "" {
		query += " WHERE " + where
	}
	query += orderClause + page

	start := time.Now()
	rows, err := db.QueryContext(ctx, query, params.Values()...)
	if err != nil {
		return nil, fmt.Errorf("조회 실패: %w", err)
	}
	defer rows.Close()

	data, truncated, err := scanValueRows(rows, len(cols), limit, q.Full)
	if err != nil {
		return nil, err
	}
	hasMore := false
	if len(data) > limit {
		data = data[:limit]
		hasMore = true
	}
	elapsed := float64(time.Since(start).Microseconds()) / 1000

	result := &RowPage{
		Columns: cols, Rows: data, PrimaryKey: pk, Truncated: truncated,
		Total: -1, Offset: q.Offset, Limit: limit, HasMore: hasMore,
		ElapsedMs: elapsed, Editable: len(pk) > 0,
	}
	if len(pk) == 0 {
		result.Reason = "기본키가 없어 개별 행을 지정할 수 없습니다. 조회만 가능합니다"
	}
	// 정렬할 것이 없었다면(뷰, 기본키 없는 테이블) 페이지 경계가 흔들릴 수 있다.
	// 첫 페이지만 보는 경우에는 아무 문제가 없으므로, 막지 않고 알리기만 한다.
	if orderClause == "" && (hasMore || q.Offset > 0) {
		result.Notes = append(result.Notes, unstableOrderNote)
	}

	if q.WithTotal {
		countParams := newParams(a.kind)
		countWhere, err := a.buildWhere(countParams, byName, cols, q.Filters, q.Search)
		if err != nil {
			return nil, err
		}
		var total int64
		if err := db.QueryRowContext(ctx, countSQL(a.kind, q.Table, countWhere),
			countParams.Values()...).Scan(&total); err != nil {
			// 개수를 못 세는 것은 조회 실패가 아니다. 큰 테이블에서 타임아웃이
			// 나기도 하는데, 그 때문에 이미 읽은 페이지를 버리면 손해다.
			result.Notes = append(result.Notes, "전체 행 수를 세지 못했습니다: "+err.Error())
		} else {
			result.Total = total
		}
	}
	return result, nil
}

// buildWhere는 필터와 검색어를 WHERE 절로 만든다.
// 컬럼 이름은 반드시 byName에서 찾은 실제 이름으로 치환한다.
func (a *sqlAdapter) buildWhere(p *paramBuilder, byName map[string]*DataColumn, cols []DataColumn, filters []Filter, search string) (string, error) {
	parts := []string{}
	for _, f := range filters {
		col, ok := byName[f.Column]
		if !ok {
			return "", fmt.Errorf("존재하지 않는 컬럼입니다: %s", f.Column)
		}
		if !f.Op.Valid() {
			return "", fmt.Errorf("알 수 없는 조건입니다: %s", f.Op)
		}
		quoted := quoteIdent(a.kind, col.Name)
		switch f.Op {
		case OpIsNull:
			parts = append(parts, quoted+" IS NULL")
		case OpNotNull:
			parts = append(parts, quoted+" IS NOT NULL")
		case OpContains:
			parts = append(parts, containsExpr(a.kind, quoted, p, f.Value))
		case OpPrefix:
			parts = append(parts, prefixExpr(a.kind, quoted, p, f.Value))
		default:
			op := map[FilterOp]string{
				OpEq: "=", OpNe: "<>", OpLt: "<", OpLte: "<=", OpGt: ">", OpGte: ">=",
			}[f.Op]
			parts = append(parts, quoted+" "+op+" "+p.Add(coerce(col, f.Value)))
		}
	}

	if strings.TrimSpace(search) != "" {
		// 검색은 모든 컬럼을 대상으로 한다. 컬럼별로 캐스트하므로 타입에 상관없이
		// 동작하고, 사용자는 값이 어느 컬럼에 있는지 몰라도 된다.
		ors := make([]string, 0, len(cols))
		for _, col := range cols {
			ors = append(ors, containsExpr(a.kind, quoteIdent(a.kind, col.Name), p, search))
		}
		if len(ors) > 0 {
			parts = append(parts, "("+strings.Join(ors, " OR ")+")")
		}
	}
	return strings.Join(parts, " AND "), nil
}

// coerce는 문자열로 온 필터 값을 컬럼 타입에 맞는 Go 값으로 바꾼다.
//
// 필요한 이유: PostgreSQL은 정수 컬럼을 문자열 파라미터와 비교하면
// "operator does not exist: integer = text"로 그냥 실패한다. 화면은 모든 입력을
// 문자열로 보내므로 여기서 되돌려 놓아야 한다. 숫자로 읽히지 않으면 문자열
// 그대로 보낸다 — 그러면 DB가 자기 규칙대로 판단하고, 그 오류 메시지가
// 우리가 지어낸 것보다 정확하다.
func coerce(col *DataColumn, value string) any {
	if !col.Numeric {
		return value
	}
	if n, err := strconv.ParseInt(value, 10, 64); err == nil {
		return n
	}
	if f, err := strconv.ParseFloat(value, 64); err == nil {
		return f
	}
	return value
}

// scanValueRows는 결과 집합을 [][]any로 읽는다.
func scanValueRows(rows *sql.Rows, width, limit int, full bool) ([][]any, [][2]int, error) {
	out := [][]any{}
	truncated := [][2]int{}
	for rows.Next() {
		if len(out) > limit {
			break
		}
		holders := make([]any, width)
		pointers := make([]any, width)
		for i := range holders {
			pointers[i] = &holders[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			return nil, nil, fmt.Errorf("행 스캔 실패: %w", err)
		}
		row := make([]any, width)
		for i, raw := range holders {
			v, cut := normalizeValue(raw, !full)
			row[i] = v
			if cut {
				truncated = append(truncated, [2]int{len(out), i})
			}
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("행 순회 실패: %w", err)
	}
	return out, truncated, nil
}

// MutateRow는 행 하나를 추가·수정·삭제한다.
func (a *sqlAdapter) MutateRow(ctx context.Context, t Target, m RowMutation) (*MutationResult, error) {
	if m.Table.Empty() {
		return nil, fmt.Errorf("대상 테이블을 지정하세요")
	}
	db, err := a.open(t, 2)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	stmt, values, err := a.buildMutation(ctx, db, m, map[string]*tableShape{})
	if err != nil {
		return nil, err
	}
	if m.DryRun {
		// 실행하지 않는다. Affected를 -1로 두어 "아직 아무 일도 일어나지 않았다"를
		// 호출부가 0(변경된 행 없음)과 구분할 수 있게 한다.
		return &MutationResult{Affected: -1, Statement: stmt, Params: values}, nil
	}

	res, err := db.ExecContext(ctx, stmt, values...)
	if err != nil {
		return nil, fmt.Errorf("실행 실패: %w", err)
	}
	affected, _ := res.RowsAffected()
	return &MutationResult{Affected: affected, Statement: stmt, Params: values}, nil
}

// MutateRows는 변경 묶음을 한 트랜잭션으로 적용한다.
//
// DryRun이면 문장만 만들어 돌려준다 — 이때는 연결만 열고 트랜잭션을 시작하지 않는다.
// 미리보기가 잠금을 잡고 있으면, 사람이 화면을 보고 판단하는 몇 초 동안 다른 세션이
// 막힌다.
func (a *sqlAdapter) MutateRows(ctx context.Context, t Target, ms []RowMutation) ([]MutationResult, error) {
	db, err := a.open(t, 2)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	// 같은 표에 대한 변경이 이어지는 것이 보통이다. 컬럼과 기본키를 변경마다 다시
	// 조회하면 20건을 적용할 때 introspection이 40번 돈다.
	shapes := map[string]*tableShape{}

	type built struct {
		stmt   string
		params []any
	}
	plan := make([]built, 0, len(ms))
	for i, m := range ms {
		if m.Table.Empty() {
			return nil, fmt.Errorf("%d번째 변경에 대상 테이블이 없습니다", i+1)
		}
		stmt, values, berr := a.buildMutation(ctx, db, m, shapes)
		if berr != nil {
			return nil, fmt.Errorf("%d번째 변경: %w", i+1, berr)
		}
		plan = append(plan, built{stmt: stmt, params: values})
	}

	out := make([]MutationResult, 0, len(plan))
	if ms[0].DryRun {
		for _, p := range plan {
			out = append(out, MutationResult{Affected: -1, Statement: p.stmt, Params: p.params})
		}
		return out, nil
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("트랜잭션을 시작하지 못했습니다: %w", err)
	}
	defer tx.Rollback()

	for i, p := range plan {
		res, xerr := tx.ExecContext(ctx, p.stmt, p.params...)
		if xerr != nil {
			// 하나라도 실패하면 전부 되돌린다. 몇 번째에서 멈췄는지 알려야
			// 사용자가 그 항목만 고쳐 다시 시도할 수 있다.
			return nil, fmt.Errorf("%d번째 변경 실패 (전체 취소됨): %w", i+1, xerr)
		}
		affected, _ := res.RowsAffected()
		out = append(out, MutationResult{Affected: affected, Statement: p.stmt, Params: p.params})
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("커밋하지 못했습니다: %w", err)
	}
	return out, nil
}

// tableShape는 한 표의 컬럼과 기본키다. 묶음 적용에서 재조회를 피하려고 캐시한다.
type tableShape struct {
	cols   []DataColumn
	byName map[string]*DataColumn
	pk     []string
}

func (a *sqlAdapter) shapeOf(ctx context.Context, db *sql.DB, ref TableRef, cache map[string]*tableShape) (*tableShape, error) {
	if s, ok := cache[ref.String()]; ok {
		return s, nil
	}
	cols, err := a.describeColumns(ctx, db, ref)
	if err != nil {
		return nil, err
	}
	byName := make(map[string]*DataColumn, len(cols))
	for i := range cols {
		byName[cols[i].Name] = &cols[i]
	}
	s := &tableShape{cols: cols, byName: byName, pk: a.primaryKey(ctx, db, ref)}
	cache[ref.String()] = s
	return s, nil
}

// buildMutation은 실행할 문장과 파라미터를 만든다. **실행하지 않는다.**
//
// 만들기와 실행을 나눈 이유가 이 화면의 요구다: 사용자는 적용 전에 무엇이 실행될지
// 봐야 하고, 그 문장은 실제로 실행될 것과 같아야 한다. 두 경로로 나누어 만들면
// 미리보기와 실제가 갈라지고, 그러면 미리보기가 거짓말이 된다.
func (a *sqlAdapter) buildMutation(ctx context.Context, db *sql.DB, m RowMutation, cache map[string]*tableShape) (string, []any, error) {
	shape, err := a.shapeOf(ctx, db, m.Table, cache)
	if err != nil {
		return "", nil, err
	}
	cols, byName, pk := shape.cols, shape.byName, shape.pk

	var stmt string
	params := newParams(a.kind)

	switch m.Action {
	case "insert":
		if len(m.Values) == 0 {
			return "", nil, fmt.Errorf("추가할 값이 없습니다")
		}
		names := []string{}
		holders := []string{}
		// 컬럼 순서를 실제 테이블 순서로 고정한다. 맵 순회 순서는 매번 달라지는데,
		// 그러면 감사 로그에 남는 문장이 실행할 때마다 달라 비교할 수 없다.
		for _, col := range cols {
			v, ok := m.Values[col.Name]
			if !ok {
				continue
			}
			names = append(names, quoteIdent(a.kind, col.Name))
			holders = append(holders, params.Add(v))
		}
		if err := unknownColumns(m.Values, byName); err != nil {
			return "", nil, err
		}
		if len(names) == 0 {
			return "", nil, fmt.Errorf("추가할 값이 없습니다")
		}
		stmt = fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)",
			qualify(a.kind, m.Table), strings.Join(names, ", "), strings.Join(holders, ", "))

	case "update":
		if len(pk) == 0 {
			return "", nil, fmt.Errorf("기본키가 없는 테이블은 수정할 수 없습니다")
		}
		if len(m.Values) == 0 {
			return "", nil, fmt.Errorf("변경할 값이 없습니다")
		}
		if err := unknownColumns(m.Values, byName); err != nil {
			return "", nil, err
		}
		sets := []string{}
		for _, col := range cols {
			v, ok := m.Values[col.Name]
			if !ok {
				continue
			}
			sets = append(sets, quoteIdent(a.kind, col.Name)+" = "+params.Add(v))
		}
		if len(sets) == 0 {
			return "", nil, fmt.Errorf("변경할 값이 없습니다")
		}
		where, err := a.pkWhere(params, byName, pk, m.Key)
		if err != nil {
			return "", nil, err
		}
		stmt = fmt.Sprintf("UPDATE %s SET %s WHERE %s",
			qualify(a.kind, m.Table), strings.Join(sets, ", "), where)

	case "delete":
		if len(pk) == 0 {
			return "", nil, fmt.Errorf("기본키가 없는 테이블은 삭제할 수 없습니다")
		}
		where, err := a.pkWhere(params, byName, pk, m.Key)
		if err != nil {
			return "", nil, err
		}
		stmt = fmt.Sprintf("DELETE FROM %s WHERE %s", qualify(a.kind, m.Table), where)

	default:
		return "", nil, fmt.Errorf("알 수 없는 동작입니다: %s", m.Action)
	}

	return stmt, params.Values(), nil
}

// pkWhere는 기본키 전체를 요구하는 WHERE 절을 만든다.
//
// 기본키의 일부만 받으면 여러 행이 걸린다. 복합키에서 흔한 실수이고, 결과는
// "한 행을 고치려다 여러 행을 고침"이므로 부족한 경우 아예 거부한다.
func (a *sqlAdapter) pkWhere(p *paramBuilder, byName map[string]*DataColumn, pk []string, key map[string]any) (string, error) {
	if len(key) == 0 {
		return "", fmt.Errorf("대상 행의 기본키가 필요합니다")
	}
	parts := make([]string, 0, len(pk))
	for _, name := range pk {
		col, ok := byName[name]
		if !ok {
			return "", fmt.Errorf("기본키 컬럼을 찾을 수 없습니다: %s", name)
		}
		v, ok := key[name]
		if !ok {
			return "", fmt.Errorf("기본키 값이 빠졌습니다: %s", name)
		}
		if v == nil {
			// 기본키는 NULL일 수 없으므로 여기 오면 요청이 잘못된 것이다.
			return "", fmt.Errorf("기본키 값이 비어 있습니다: %s", name)
		}
		if s, ok := v.(string); ok {
			parts = append(parts, quoteIdent(a.kind, name)+" = "+p.Add(coerce(col, s)))
			continue
		}
		parts = append(parts, quoteIdent(a.kind, name)+" = "+p.Add(v))
	}
	return strings.Join(parts, " AND "), nil
}

// unknownColumns는 테이블에 없는 컬럼이 요청에 섞였는지 확인한다.
// 조용히 무시하면 사용자는 저장이 됐다고 생각하는데 값은 반영되지 않는다.
func unknownColumns(values map[string]any, byName map[string]*DataColumn) error {
	unknown := []string{}
	for name := range values {
		if _, ok := byName[name]; !ok {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) > 0 {
		slices.Sort(unknown)
		return fmt.Errorf("존재하지 않는 컬럼입니다: %s", strings.Join(unknown, ", "))
	}
	return nil
}

// RunStatements는 임의의 SQL을 실행한다.
//
// 문장마다 결과를 따로 담고, 실패해도 뒤 문장을 계속 실행하지 않는다.
// 마이그레이션 실행기(ExecDDL)와 같은 판단이다: 앞이 실패한 상태에서 뒤를 계속하면
// 어디까지가 의도한 결과인지 알 수 없게 된다.
func (a *sqlAdapter) RunStatements(ctx context.Context, t Target, r StatementRequest) ([]StatementResult, error) {
	stmts := splitSQL(a.kind, r.Statement)
	if len(stmts) == 0 {
		return nil, fmt.Errorf("실행할 문장이 없습니다")
	}

	db, err := a.open(t, 2)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	// 한 묶음은 커넥션 하나에 고정한다.
	//
	// 풀에 맡기면 문장마다 다른 커넥션으로 나갈 수 있다. 그러면 `USE appdb;`
	// 다음 줄의 SELECT가 기본 데이터베이스가 바뀌지 않은 커넥션에서 실행되어,
	// 방금 옮겨 놓은 것이 무시된 채 "그런 테이블 없음"으로 끝난다.
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("접속 실패: %w", err)
	}
	defer conn.Close()

	maxRows := r.MaxRows
	if maxRows <= 0 || maxRows > MaxRowLimit {
		maxRows = MaxRowLimit
	}

	out := make([]StatementResult, 0, len(stmts))
	for _, stmt := range stmts {
		res := a.runOne(ctx, conn, stmt, maxRows, r.ReadOnly)
		out = append(out, res)
		if res.Error != "" {
			break
		}
	}
	return out, nil
}

// querier는 *sql.DB와 *sql.Conn을 함께 받기 위한 최소 인터페이스다.
// 문장 실행은 커넥션 하나에 고정해야 세션 상태(USE 등)가 이어진다.
type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func (a *sqlAdapter) runOne(ctx context.Context, db querier, stmt string, maxRows int, readOnly bool) StatementResult {
	res := StatementResult{Statement: stmt}
	start := time.Now()
	defer func() { res.ElapsedMs = float64(time.Since(start).Microseconds()) / 1000 }()

	// 읽기 전용은 사용자가 스스로 거는 안전장치다. 권한(sql.run)을 대신하지 않으며,
	// 문장 종류를 앞 단어로 판단한다 — 완벽한 파서가 아니어도, 실수로 UPDATE를
	// 실행하는 가장 흔한 사고는 이것으로 막힌다.
	if readOnly && !isReadOnlyStatement(stmt) {
		res.Error = "읽기 전용 모드에서는 조회 문장만 실행할 수 있습니다"
		return res
	}

	// 결과 집합이 있는지 미리 알 수 없으므로 항상 Query로 실행한다.
	// Exec로 실행하면 SELECT의 결과를 버리게 되고, Query로 실행한 INSERT는
	// 결과가 없는 정상 응답을 돌려준다(드라이버가 모두 이 동작을 지원한다).
	rows, err := db.QueryContext(ctx, stmt)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	defer rows.Close()

	cts, err := rows.ColumnTypes()
	if err != nil {
		res.Error = err.Error()
		return res
	}
	if len(cts) == 0 {
		// 결과 집합이 없는 문장. RowsAffected를 알 수 없으므로 실행 성공만 알린다.
		res.Kind = "ok"
		res.Affected = -1
		return res
	}

	cols := make([]DataColumn, 0, len(cts))
	for _, ct := range cts {
		nullable, ok := ct.Nullable()
		if !ok {
			nullable = true
		}
		cols = append(cols, DataColumn{
			Name: ct.Name(), Type: ct.DatabaseTypeName(), Nullable: nullable,
			Numeric: isNumericType(ct.DatabaseTypeName()),
		})
	}
	data, _, err := scanValueRows(rows, len(cols), maxRows, false)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	if len(data) > maxRows {
		data = data[:maxRows]
		res.Truncated = true
	}
	res.Kind = "rows"
	res.Columns = cols
	res.Rows = data
	res.Affected = int64(len(data))
	return res
}

// isReadOnlyStatement는 조회 문장인지 앞 단어로 판단한다.
func isReadOnlyStatement(stmt string) bool {
	s := strings.ToUpper(strings.TrimSpace(stripLeadingComments(stmt)))
	switch firstWord(s) {
	case "SELECT", "SHOW", "DESCRIBE", "DESC", "PRAGMA":
		return true
	case "EXPLAIN":
		// EXPLAIN은 보통 계획만 보여주지만, ANALYZE가 붙으면 **문장을 실제로
		// 실행한다**(PostgreSQL·MySQL). `EXPLAIN ANALYZE DELETE …`는 행을 지운다.
		// 여기서 걸러내지 않으면 읽기 전용이라는 표시가 거짓말이 된다.
		return isReadOnlyExplain(s[len("EXPLAIN"):])
	// USE는 세션의 기본 데이터베이스를 바꿀 뿐 데이터를 건드리지 않는다.
	// 읽기 전용은 "실수로 값을 바꾸는 것"을 막으려는 장치이므로, DB를 옮겨 다니며
	// 조회하는 흐름까지 막을 이유가 없다.
	case "USE":
		return true
	case "WITH":
		// WITH ... INSERT/UPDATE/DELETE는 조회가 아니다(PostgreSQL의 CTE 쓰기).
		return !writesInCTE(s)
	}
	return false
}

// reAnalyzeOpt는 EXPLAIN 옵션 자리의 ANALYZE를 찾는다(영/미 철자 모두).
var reAnalyzeOpt = regexp.MustCompile(`\bANALY[SZ]E\b`)

// reInnerStatement는 EXPLAIN 뒤에 오는 실제 문장이 시작되는 자리를 찾는다.
var reInnerStatement = regexp.MustCompile(
	`\b(SELECT|INSERT|UPDATE|DELETE|MERGE|REPLACE|WITH|CREATE|DROP|ALTER|TRUNCATE|CALL)\b`)

// isReadOnlyExplain은 EXPLAIN 뒤쪽을 보고 판정한다.
//
// 안쪽 문장을 찾지 못하면 읽기로 본다 — 알 수 없는 형태를 쓰기로 단정하면
// DB마다 다른 EXPLAIN 문법이 이유 없이 막힌다.
func isReadOnlyExplain(rest string) bool {
	loc := reInnerStatement.FindStringIndex(rest)
	if loc == nil {
		return true
	}
	if !reAnalyzeOpt.MatchString(rest[:loc[0]]) {
		return true // 계획만 본다. 실행하지 않으므로 안전하다.
	}
	return isReadOnlyStatement(rest[loc[0]:])
}

// firstWord는 맨 앞 낱말 하나를 떼어 낸다.
//
// 접두사 비교만 하면 "USER"가 "USE"로, "DESCEND"가 "DESC"로 읽힌다.
// 낱말 경계까지 봐야 판정이 문장 종류를 따라간다.
func firstWord(upper string) string {
	i := strings.IndexFunc(upper, func(r rune) bool { return r < 'A' || r > 'Z' })
	if i < 0 {
		return upper
	}
	return upper[:i]
}

// cteWriteKeywords는 CTE 안의 쓰기 문장을 찾는다.
//
// 단순 부분 문자열 검색으로는 안 된다. "WITH d AS (DELETE …"처럼 여는 괄호가
// 바로 앞에 오는 것이 오히려 흔한 형태이고, 반대로 `deleted_at` 같은 컬럼 이름이
// 걸리면 멀쩡한 SELECT가 쓰기로 오판된다. 단어 경계가 필요하다.
var cteWriteKeywords = regexp.MustCompile(`\b(INSERT|UPDATE|DELETE|MERGE)\b`)

func writesInCTE(upper string) bool {
	return cteWriteKeywords.MatchString(upper)
}

func stripLeadingComments(s string) string {
	for {
		s = strings.TrimSpace(s)
		switch {
		case strings.HasPrefix(s, "--"):
			if i := strings.IndexAny(s, "\r\n"); i >= 0 {
				s = s[i:]
				continue
			}
			return ""
		case strings.HasPrefix(s, "/*"):
			if i := strings.Index(s[2:], "*/"); i >= 0 {
				s = s[i+4:]
				continue
			}
			return ""
		default:
			return s
		}
	}
}

// isNumericType은 DB가 알려준 타입 이름이 수치형인지 판단한다.
//
// 이름 기반 판단은 정확하지 않지만, 이 값의 용도는 필터 값을 숫자로 바꿀지
// 정하는 것뿐이고 틀려도 문자열로 보내 DB가 판단하게 된다(coerce 참조).
func isNumericType(name string) bool {
	n := strings.ToUpper(name)
	for _, needle := range []string{
		"INT", "SERIAL", "DEC", "NUMERIC", "NUMBER", "FLOAT", "DOUBLE", "REAL", "MONEY",
	} {
		if strings.Contains(n, needle) {
			return true
		}
	}
	return false
}

// splitSQL은 스크립트를 문장 단위로 나눈다.
//
// strings.Split(s, ";")로는 안 된다. 세미콜론은 문자열 리터럴 안에도, 주석 안에도,
// PostgreSQL의 달러 인용 함수 본문 안에도 들어 있다. 그것을 경계로 삼으면 문장이
// 조각나 문법 오류가 나고, 사용자는 자기가 쓴 SQL이 틀렸다고 오해한다.
func splitSQL(kind model.DBKind, script string) []string {
	out := []string{}
	var cur strings.Builder
	runes := []rune(script)

	flush := func() {
		if s := strings.TrimSpace(cur.String()); s != "" {
			out = append(out, s)
		}
		cur.Reset()
	}

	for i := 0; i < len(runes); i++ {
		c := runes[i]
		switch {
		case c == '-' && i+1 < len(runes) && runes[i+1] == '-':
			for i < len(runes) && runes[i] != '\n' {
				cur.WriteRune(runes[i])
				i++
			}
			if i < len(runes) {
				cur.WriteRune('\n')
			}

		case c == '#' && kind == model.KindMySQL:
			for i < len(runes) && runes[i] != '\n' {
				cur.WriteRune(runes[i])
				i++
			}
			if i < len(runes) {
				cur.WriteRune('\n')
			}

		case c == '/' && i+1 < len(runes) && runes[i+1] == '*':
			cur.WriteString("/*")
			i += 2
			for i < len(runes) && !(runes[i] == '*' && i+1 < len(runes) && runes[i+1] == '/') {
				cur.WriteRune(runes[i])
				i++
			}
			if i < len(runes) {
				cur.WriteString("*/")
				i++
			}

		case c == '\'' || c == '"' || c == '`':
			quote := c
			cur.WriteRune(c)
			i++
			for i < len(runes) {
				// MySQL은 문자열 안에서 백슬래시 이스케이프를 쓴다.
				if runes[i] == '\\' && kind == model.KindMySQL && quote == '\'' && i+1 < len(runes) {
					cur.WriteRune(runes[i])
					cur.WriteRune(runes[i+1])
					i += 2
					continue
				}
				cur.WriteRune(runes[i])
				if runes[i] == quote {
					// 같은 인용부호가 두 번이면 이스케이프된 인용부호다.
					if i+1 < len(runes) && runes[i+1] == quote {
						cur.WriteRune(runes[i+1])
						i += 2
						continue
					}
					break
				}
				i++
			}

		case c == '$' && kind == model.KindPostgres:
			// 달러 인용: $$ 또는 $tag$ ... 같은 태그로 닫힌다.
			if tag, size := dollarTag(runes[i:]); size > 0 {
				cur.WriteString(tag)
				i += size
				for i < len(runes) {
					if strings.HasPrefix(string(runes[i:]), tag) {
						cur.WriteString(tag)
						i += len([]rune(tag)) - 1
						break
					}
					cur.WriteRune(runes[i])
					i++
				}
			} else {
				cur.WriteRune(c)
			}

		case c == ';':
			flush()

		default:
			cur.WriteRune(c)
		}
	}
	flush()
	return out
}

// dollarTag는 달러 인용의 여는 태그를 읽는다. 태그가 아니면 크기 0을 반환한다.
func dollarTag(runes []rune) (string, int) {
	if len(runes) == 0 || runes[0] != '$' {
		return "", 0
	}
	for i := 1; i < len(runes) && i < 64; i++ {
		if runes[i] == '$' {
			return string(runes[:i+1]), i + 1
		}
		// 태그는 식별자 문자만 쓸 수 있다. 그 밖의 문자가 나오면 그냥 달러 기호다.
		r := runes[i]
		if !(r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
			return "", 0
		}
	}
	return "", 0
}
