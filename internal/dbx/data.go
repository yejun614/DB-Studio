package dbx

import (
	"context"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"dbstudio/internal/model"
)

// 데이터 접근 계층.
//
// 지금까지 이 앱이 대상 DB에서 읽던 것은 **구조와 지표**였다(스키마, 통계, 로그).
// 여기서부터는 **값**을 읽고 쓴다. 그 차이가 이 파일의 설계를 결정한다:
//
//   - 값은 크다. 스키마는 통째로 읽어도 되지만 테이블은 그럴 수 없다. 모든 조회는
//     페이지 단위이고, 셀 하나도 표시용으로 잘라서 보낸다(전체 값은 편집할 때만 따로 읽는다).
//   - 값은 임의의 타입이다. 드라이버마다 같은 컬럼을 다른 Go 타입으로 준다.
//     그래서 JSON으로 나가기 전에 한 곳(normalizeValue)에서 정규화한다.
//   - 값을 고치는 것은 되돌릴 수 없다. 수정·삭제는 반드시 기본키로만 지정한다.
//     WHERE 절을 사용자가 쓰게 하면 한 줄을 고치려다 테이블을 비우는 사고가 난다.
//
// 어댑터 인터페이스(Adapter)에 메서드를 더하지 않고 별도의 선택적 인터페이스로 둔 것은
// explore.go의 DocumentExplorer와 같은 이유다 — 데이터 접근이 불가능한 종류에
// "지원 안 함"만 반환하는 메서드를 억지로 만들게 하지 않는다.

// TableRef는 조회 대상을 가리킨다.
// Namespace는 관계형에서 스키마(또는 MySQL의 데이터베이스), MongoDB에서는 DB 이름이다.
type TableRef struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

func (r TableRef) String() string {
	if r.Namespace == "" {
		return r.Name
	}
	return r.Namespace + "." + r.Name
}

func (r TableRef) Empty() bool { return strings.TrimSpace(r.Name) == "" }

// DataObject는 데이터 화면의 좌측 목록에 나오는 항목 하나다.
type DataObject struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	// Kind는 table | view | collection | keyspace 중 하나다.
	// 뷰는 대개 수정할 수 없으므로 화면이 편집 버튼을 감추는 데 쓴다.
	Kind string `json:"kind"`
	// RowCount는 추정치다. 정확한 개수를 위해 매번 count(*)를 돌리면
	// 큰 테이블에서 목록을 여는 것만으로 DB가 흔들린다. -1은 "모름"이다.
	RowCount int64  `json:"rowCount"`
	Comment  string `json:"comment,omitempty"`
}

// DataColumn은 한 컬럼의 표시·편집에 필요한 최소 정보다.
type DataColumn struct {
	Name string `json:"name"`
	Type string `json:"type"`
	// Nullable은 드라이버가 알려주지 않으면 true다(모르면 넓게 잡는다).
	Nullable bool `json:"nullable"`
	PK       bool `json:"pk"`
	// Numeric은 필터 값을 숫자로 바꿔 보낼지 판단하는 데 쓴다.
	// PostgreSQL은 정수 컬럼에 문자열 파라미터를 주면 그냥 실패한다.
	Numeric bool `json:"numeric"`
	// FK는 이 컬럼이 가리키는 곳이다. 없으면 nil.
	//
	// 데이터 화면이 "이 값이 무엇인가"를 따라갈 수 있으려면 값 옆에 대상이
	// 있어야 한다. 숫자 하나(예: user_id = 481)만 보고 그것이 누구인지 알려면
	// 지금까지는 표를 옮겨 다니며 직접 조회해야 했다.
	FK *ColumnRef `json:"fk,omitempty"`
}

// ColumnRef는 외래키가 가리키는 자리다.
type ColumnRef struct {
	Namespace string `json:"namespace,omitempty"`
	Table     string `json:"table"`
	Column    string `json:"column"`
}

// FilterOp는 지원하는 비교 연산이다.
type FilterOp string

const (
	OpEq       FilterOp = "eq"
	OpNe       FilterOp = "ne"
	OpLt       FilterOp = "lt"
	OpLte      FilterOp = "lte"
	OpGt       FilterOp = "gt"
	OpGte      FilterOp = "gte"
	OpContains FilterOp = "contains"
	OpPrefix   FilterOp = "prefix"
	OpIsNull   FilterOp = "isnull"
	OpNotNull  FilterOp = "notnull"
)

func (o FilterOp) Valid() bool {
	switch o {
	case OpEq, OpNe, OpLt, OpLte, OpGt, OpGte, OpContains, OpPrefix, OpIsNull, OpNotNull:
		return true
	}
	return false
}

// NeedsValue는 값 입력이 필요한 연산인지 반환한다.
func (o FilterOp) NeedsValue() bool { return o != OpIsNull && o != OpNotNull }

// Filter는 컬럼 하나에 대한 조건이다.
type Filter struct {
	Column string   `json:"column"`
	Op     FilterOp `json:"op"`
	Value  string   `json:"value"`
}

// RowQuery는 한 페이지 조회 요청이다.
type RowQuery struct {
	Table   TableRef
	Limit   int
	Offset  int
	OrderBy string
	Desc    bool
	Filters []Filter
	// Search는 모든 문자열 컬럼에 대한 부분 일치다. 사용자는 어느 컬럼에 있는지
	// 모르는 채로 찾기 시작하므로, 컬럼을 고르라고 요구하기 전에 통짜 검색이 있어야 한다.
	Search string
	// WithTotal이 true면 count(*)를 함께 실행한다. 기본은 false다 —
	// 페이지를 넘길 때마다 전체를 세는 것은 큰 테이블에서 가장 비싼 동작이다.
	WithTotal bool
	// Full이 true면 셀 값을 자르지 않는다. 편집 화면에서 한 행을 읽을 때만 쓴다.
	Full bool
}

const (
	// DefaultRowLimit은 한 페이지 기본 행 수다.
	DefaultRowLimit = 50
	// MaxRowLimit은 한 번에 가져올 수 있는 최대 행 수다.
	MaxRowLimit = 500
	// maxCellLen은 목록에서 셀 하나가 가질 수 있는 최대 문자 수다.
	// 넘으면 잘라 보내고 잘렸다고 표시한다. 편집은 전체 값을 다시 읽어서 한다.
	maxCellLen = 4000
)

func (q RowQuery) EffectiveLimit() int {
	if q.Limit <= 0 {
		return DefaultRowLimit
	}
	if q.Limit > MaxRowLimit {
		return MaxRowLimit
	}
	return q.Limit
}

// RowPage는 조회 결과 한 페이지다.
type RowPage struct {
	Columns []DataColumn `json:"columns"`
	Rows    [][]any      `json:"rows"`
	// PrimaryKey가 비어 있으면 이 결과는 수정할 수 없다.
	// 화면이 편집 버튼을 감추는 근거이며, 서버도 같은 이유로 수정을 거부한다.
	PrimaryKey []string `json:"primaryKey"`
	// Truncated는 (행 인덱스, 열 인덱스) 쌍의 목록으로 잘린 셀을 알려준다.
	Truncated [][2]int `json:"truncated,omitempty"`
	Total     int64    `json:"total"`
	Offset    int      `json:"offset"`
	Limit     int      `json:"limit"`
	// HasMore는 다음 페이지가 있는지다. Total을 세지 않아도 알 수 있도록
	// 한 행을 더 읽어서 판단한다.
	HasMore   bool     `json:"hasMore"`
	ElapsedMs float64  `json:"elapsedMs"`
	Notes     []string `json:"notes,omitempty"`
	// Editable이 false면 수정 UI를 감춘다(뷰, 기본키 없는 테이블 등).
	Editable bool   `json:"editable"`
	Reason   string `json:"reason,omitempty"`
}

// RowMutation은 행 하나에 대한 변경이다.
type RowMutation struct {
	Table  TableRef
	Action string // insert | update | delete
	// Values는 insert/update에서 설정할 값이다. nil 값은 NULL을 뜻한다.
	Values map[string]any
	// Key는 update/delete가 대상을 지정하는 기본키 값이다.
	Key map[string]any
	// DryRun이면 문장만 만들고 실행하지 않는다.
	//
	// 이 값을 어댑터가 지키지 않으면 "미리보기"가 곧 실행이 된다. 그래서
	// DataCapabilities.Preview가 false인 어댑터에는 아예 보내지 않는다(DoMutateRow).
	DryRun bool
}

// MutationResult는 변경 결과다.
type MutationResult struct {
	Affected int64 `json:"affected"`
	// Statement는 실행된 문장이다(값은 파라미터로 분리되어 있으므로 자리표시자가 남는다).
	// 감사 로그와 화면에 무엇이 실행됐는지 그대로 보여주기 위해 반환한다.
	Statement string `json:"statement"`
	Params    []any  `json:"params,omitempty"`
}

// StatementRequest는 SQL(또는 Mongo/Redis 명령) 실행 요청이다.
type StatementRequest struct {
	Statement string
	MaxRows   int
	// ReadOnly가 true면 결과를 반환하지 않는 문장을 거부한다.
	// 사용자가 스스로 거는 안전장치이며, 권한을 대신하지 않는다.
	ReadOnly bool
}

// StatementResult는 실행 결과 하나다. 여러 문장을 실행하면 여러 개가 나온다.
type StatementResult struct {
	Statement string       `json:"statement"`
	Kind      string       `json:"kind"` // rows | affected | ok
	Columns   []DataColumn `json:"columns,omitempty"`
	Rows      [][]any      `json:"rows,omitempty"`
	Truncated bool         `json:"truncated,omitempty"`
	Affected  int64        `json:"affected"`
	ElapsedMs float64      `json:"elapsedMs"`
	Error     string       `json:"error,omitempty"`
}

// DataCapabilities는 이 종류가 데이터 화면에서 무엇을 할 수 있는지 알려준다.
// 화면이 지원하지 않는 컨트롤을 그리지 않도록 한다.
type DataCapabilities struct {
	Browse    bool `json:"browse"`
	Filter    bool `json:"filter"`
	Sort      bool `json:"sort"`
	Mutate    bool `json:"mutate"`
	Statement bool `json:"statement"`
	// StatementCheck는 실행 없이 문장을 검사할 수 있는지다.
	// 화면은 이 값이 false면 "구문 검사" 버튼을 그리지 않는다 — 눌러도 안 되는
	// 버튼을 보여주는 것은 없는 것보다 나쁘다.
	StatementCheck bool `json:"statementCheck"`
	// StatementLabel은 실행 화면의 이름이다. Mongo/Redis는 SQL이 아니다.
	StatementLabel string `json:"statementLabel"`
	// StatementHelp는 입력 형식 안내다.
	StatementHelp string `json:"statementHelp"`
	// Explain은 실행 계획을 보기 위해 문장 앞에 붙일 구절이다.
	//
	// 접두사로 표현하는 이유: DB마다 이름이 다르고(EXPLAIN ANALYZE / EXPLAIN QUERY
	// PLAN), 어떤 DB는 접두사만으로는 안 된다(Oracle은 EXPLAIN PLAN FOR 뒤에
	// 계획 표를 따로 조회해야 하고, MS-SQL은 세션 설정을 바꿔야 한다). 접두사가
	// 비어 있으면 화면은 버튼을 그리지 않는다 — 눌러도 안 되는 버튼은 없는 것보다 나쁘다.
	Explain string `json:"explain"`
	// Preview는 **실행하지 않고** 무엇이 실행될지 보여줄 수 있는지다.
	//
	// 화면은 이 값으로 두 가지 흐름을 가른다: true면 수정을 모아 두었다가
	// 실행될 문장을 확인하고 한 번에 적용하고, false면 지금처럼 바로 반영한다.
	// Mongo·Redis가 false인 이유는 그쪽 어댑터가 실행 결과(생성된 _id 등)를 담아
	// 명령 문자열을 만들기 때문이다 — 실행 전에는 보여줄 문장이 없다.
	Preview bool `json:"preview"`
}

// DataBrowser는 값 읽기/쓰기를 지원하는 어댑터가 구현한다.
type DataBrowser interface {
	DataCapabilities() DataCapabilities
	ListObjects(ctx context.Context, t Target) ([]DataObject, error)
	QueryRows(ctx context.Context, t Target, q RowQuery) (*RowPage, error)
	MutateRow(ctx context.Context, t Target, m RowMutation) (*MutationResult, error)
	RunStatements(ctx context.Context, t Target, r StatementRequest) ([]StatementResult, error)
}

// browserFor는 대상의 데이터 브라우저를 찾는다.
func browserFor(t Target) (DataBrowser, error) {
	if t.Conn == nil {
		return nil, fmt.Errorf("커넥션 정보가 없습니다")
	}
	a, err := Get(t.Conn.Kind)
	if err != nil {
		return nil, err
	}
	b, ok := a.(DataBrowser)
	if !ok {
		return nil, fmt.Errorf("%w: %s 데이터 조회", ErrNotImplemented, t.Conn.Kind)
	}
	return b, nil
}

// DataCapsFor는 DB 종류의 데이터 기능을 반환한다. 미지원 종류는 zero value다.
// /meta가 이것을 함께 내려보내 화면이 종류별로 다른 컨트롤을 그린다.
func DataCapsFor(kind model.DBKind) DataCapabilities {
	a, err := Get(kind)
	if err != nil {
		return DataCapabilities{}
	}
	if b, ok := a.(DataBrowser); ok {
		return b.DataCapabilities()
	}
	return DataCapabilities{}
}

// ---------- 디스패처 ----------
//
// 핸들러는 이 함수들만 호출한다. 종류별 분기는 여기서 끝난다.

func DoListObjects(ctx context.Context, t Target) ([]DataObject, error) {
	b, err := browserFor(t)
	if err != nil {
		return nil, err
	}
	return b.ListObjects(ctx, t)
}

func DoQueryRows(ctx context.Context, t Target, q RowQuery) (*RowPage, error) {
	b, err := browserFor(t)
	if err != nil {
		return nil, err
	}
	return b.QueryRows(ctx, t, q)
}

func DoMutateRow(ctx context.Context, t Target, m RowMutation) (*MutationResult, error) {
	b, err := browserFor(t)
	if err != nil {
		return nil, err
	}
	caps := b.DataCapabilities()
	if !caps.Mutate {
		return nil, fmt.Errorf("%w: %s 데이터 수정", ErrNotImplemented, t.Conn.Kind)
	}
	// 미리보기를 지원하지 않는 어댑터에 DryRun을 넘기면 그대로 실행된다.
	// 여기서 막는다 — 이 실수는 "미리보기를 눌렀는데 데이터가 바뀌는" 형태로 나타난다.
	if m.DryRun && !caps.Preview {
		return nil, fmt.Errorf("%w: %s 미리보기", ErrNotImplemented, t.Conn.Kind)
	}
	return b.MutateRow(ctx, t, m)
}

// rowBatcher는 여러 변경을 **한 트랜잭션으로** 처리할 수 있는 어댑터가 구현한다.
//
// DataBrowser에 넣지 않고 선택 인터페이스로 둔 이유: Mongo·Redis에는 이 화면이
// 기대하는 의미의 트랜잭션이 없다. 인터페이스에 넣으면 그쪽에도 "한 번에 적용"처럼
// 보이는 구현을 만들어야 하는데, 중간에 실패하면 앞의 것만 반영된 상태로 남는다.
type rowBatcher interface {
	MutateRows(ctx context.Context, t Target, ms []RowMutation) ([]MutationResult, error)
}

// DoMutateRows는 변경 묶음을 적용한다(또는 DryRun이면 문장만 만든다).
//
// 트랜잭션을 지원하는 어댑터에서는 전부 성공하거나 전부 취소된다. 데이터 화면이
// 여러 행을 모아 두었다가 한 번에 적용하는 흐름은 그 보장 위에서만 안전하다 —
// 절반만 반영되면 사용자는 무엇이 적용되고 무엇이 남았는지 알 수 없다.
func DoMutateRows(ctx context.Context, t Target, ms []RowMutation) ([]MutationResult, error) {
	if len(ms) == 0 {
		return nil, fmt.Errorf("적용할 변경이 없습니다")
	}
	b, err := browserFor(t)
	if err != nil {
		return nil, err
	}
	caps := b.DataCapabilities()
	if !caps.Mutate {
		return nil, fmt.Errorf("%w: %s 데이터 수정", ErrNotImplemented, t.Conn.Kind)
	}
	for _, m := range ms {
		if m.DryRun && !caps.Preview {
			return nil, fmt.Errorf("%w: %s 미리보기", ErrNotImplemented, t.Conn.Kind)
		}
	}
	batcher, ok := b.(rowBatcher)
	if !ok {
		return nil, fmt.Errorf("%w: %s 일괄 적용", ErrNotImplemented, t.Conn.Kind)
	}
	return batcher.MutateRows(ctx, t, ms)
}

func DoRunStatements(ctx context.Context, t Target, r StatementRequest) ([]StatementResult, error) {
	b, err := browserFor(t)
	if err != nil {
		return nil, err
	}
	if !b.DataCapabilities().Statement {
		return nil, fmt.Errorf("%w: %s 문장 실행", ErrNotImplemented, t.Conn.Kind)
	}
	return b.RunStatements(ctx, t, r)
}

// ---------- 값 정규화 ----------

// normalizeValue는 드라이버가 준 값을 JSON으로 안전하게 나갈 수 있는 형태로 바꾼다.
//
// 드라이버마다 같은 컬럼을 다른 Go 타입으로 준다(MySQL은 대부분 []byte, pgx는
// 타입별 Go 값, Oracle은 또 다르다). 이 차이가 화면까지 새어 나가면 같은 데이터가
// DB 종류에 따라 다르게 보이므로 한 곳에서 정리한다.
//
// truncate가 true면 긴 문자열을 잘라 (값, true)를 반환한다.
func normalizeValue(v any, truncate bool) (any, bool) {
	switch val := v.(type) {
	case nil:
		return nil, false
	case bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64,
		float32, float64:
		return val, false
	case time.Time:
		// 고정 형식으로 보낸다. 로케일에 따라 달라지는 표현을 서버가 정하면
		// 화면이 다시 파싱할 수 없다.
		return val.Format(time.RFC3339Nano), false
	case []byte:
		if utf8.Valid(val) {
			return clip(string(val), truncate)
		}
		// 바이너리는 표시용 16진 문자열로 바꾼다. 편집은 지원하지 않으므로
		// 원본으로 되돌릴 필요가 없고, 그대로 보내면 JSON이 깨진다.
		const shown = 32
		if len(val) > shown {
			return fmt.Sprintf("0x%s… (%d bytes)", hex.EncodeToString(val[:shown]), len(val)), true
		}
		return "0x" + hex.EncodeToString(val), false
	case string:
		return clip(val, truncate)
	default:
		return clip(fmt.Sprint(val), truncate)
	}
}

func clip(s string, truncate bool) (any, bool) {
	if !truncate || len(s) <= maxCellLen {
		return s, false
	}
	// 문자 경계에서 자른다. 바이트로 자르면 UTF-8이 깨져 JSON 인코딩이 실패한다.
	runes := []rune(s)
	if len(runes) <= maxCellLen {
		return s, false
	}
	return string(runes[:maxCellLen]), true
}
