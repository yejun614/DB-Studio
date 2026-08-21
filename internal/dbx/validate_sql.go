package dbx

import (
	"context"
	"fmt"
	"strings"

	"dbstudio/internal/model"
)

// 구문 검사.
//
// **실행하지 않고** 문장이 말이 되는지 확인한다. 방법은 드라이버의 Prepare다 —
// 모든 대상 DB에서 Prepare는 "파싱 + 이름 해석"까지 하고 실행은 하지 않는다.
// 그래서 문법 오류뿐 아니라 테이블·컬럼 이름의 오타까지 잡힌다. 사람이 직접
// EXPLAIN을 붙여 확인하는 것과 같은 일을, 문장을 건드리지 않고 한다.
//
// 왜 EXPLAIN을 쓰지 않는가: EXPLAIN은 DB마다 문법이 다르고(오라클은 INTO 절이
// 필요하다), DDL에는 붙일 수 없으며, 무엇보다 **문장 앞에 무언가를 덧붙이는 순간
// 사용자가 쓴 것과 다른 문장을 검사하게 된다.** Prepare는 문장을 그대로 보낸다.

// StatementCheck는 문장 하나의 검사 결과다.
type StatementCheck struct {
	Statement string `json:"statement"`
	// Index는 스크립트에서 몇 번째 문장인지다(0부터).
	Index int `json:"index"`
	// Status: ok | error | skipped
	//
	// skipped는 "틀렸다"가 아니라 "이 방법으로는 확인할 수 없다"이다. 둘을 같은
	// 색으로 보여주면 사용자는 멀쩡한 문장을 고치려 든다.
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// StatementValidator는 구문 검사를 지원하는 어댑터가 구현한다.
// MongoDB/Redis는 명령이 문자열 파싱 한 번으로 끝나 "검사"가 따로 성립하지 않는다.
type StatementValidator interface {
	ValidateStatements(ctx context.Context, t Target, script string) ([]StatementCheck, error)
}

// DoValidateStatements는 종류별 구현으로 넘긴다.
func DoValidateStatements(ctx context.Context, t Target, script string) ([]StatementCheck, error) {
	a, err := Get(t.Conn.Kind)
	if err != nil {
		return nil, err
	}
	v, ok := a.(StatementValidator)
	if !ok {
		return nil, fmt.Errorf("%w: %s 구문 검사", ErrNotImplemented, t.Conn.Kind)
	}
	return v.ValidateStatements(ctx, t, script)
}

// maxCheckStatements는 한 번에 검사할 문장 수 상한이다.
// 문장마다 왕복이 한 번이므로, 수천 줄짜리 덤프를 그대로 붙여 넣으면
// 검사 자체가 대상 DB에 대한 부하가 된다.
const maxCheckStatements = 200

func (a *sqlAdapter) ValidateStatements(ctx context.Context, t Target, script string) ([]StatementCheck, error) {
	stmts := splitSQL(a.kind, script)
	if len(stmts) == 0 {
		return nil, fmt.Errorf("검사할 문장이 없습니다")
	}
	if len(stmts) > maxCheckStatements {
		return nil, fmt.Errorf("한 번에 검사할 수 있는 문장은 %d개까지입니다 (%d개를 받았습니다)",
			maxCheckStatements, len(stmts))
	}

	db, err := a.open(t, 2)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	// 실행과 같은 이유로 커넥션 하나에 고정한다. `USE other_db;` 다음 문장은
	// 그 데이터베이스를 기준으로 검사되어야 사용자가 보는 것과 결과가 같다.
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("접속 실패: %w", err)
	}
	defer conn.Close()

	out := make([]StatementCheck, 0, len(stmts))
	for i, stmt := range stmts {
		res := StatementCheck{Statement: stmt, Index: i}

		// 세션 상태를 바꾸는 문장은 Prepare로 확인할 수 없다(준비만 하고 실행하지
		// 않으므로 상태가 바뀌지 않는다). 실행해 버리면 검사가 아니게 되므로,
		// 확인할 수 없다고 정직하게 알리고 넘어간다. 다만 이후 문장이 이 문장을
		// 전제로 쓰였을 수 있어, 그 사실도 함께 적는다.
		if reason := unverifiableReason(a.kind, stmt); reason != "" {
			res.Status = "skipped"
			res.Reason = reason
			out = append(out, res)
			continue
		}

		st, perr := conn.PrepareContext(ctx, stmt)
		if perr != nil {
			if reason := prepareUnsupported(a.kind, perr); reason != "" {
				res.Status = "skipped"
				res.Reason = reason
			} else {
				res.Status = "error"
				res.Error = perr.Error()
			}
			out = append(out, res)
			continue
		}
		_ = st.Close()
		res.Status = "ok"
		out = append(out, res)
	}
	return out, nil
}

// unverifiableReason은 Prepare로 확인할 수 없는 문장인지 앞 단어로 판단한다.
func unverifiableReason(kind model.DBKind, stmt string) string {
	switch firstWord(strings.ToUpper(strings.TrimSpace(stripLeadingComments(stmt)))) {
	case "USE":
		return "세션의 기본 데이터베이스를 바꾸는 문장이라 실행 없이 확인할 수 없습니다. " +
			"뒤따르는 문장은 현재 데이터베이스를 기준으로 검사했습니다"
	case "BEGIN", "START", "COMMIT", "ROLLBACK", "SAVEPOINT", "SET":
		return "트랜잭션·세션 제어 문장이라 실행 없이 확인할 수 없습니다"
	case "DELIMITER":
		return "클라이언트 지시어라 서버가 해석하지 않습니다"
	}
	if kind == model.KindPostgres && strings.HasPrefix(
		strings.ToUpper(strings.TrimSpace(stripLeadingComments(stmt))), "COPY") {
		return "COPY는 데이터 스트림이 필요해 실행 없이 확인할 수 없습니다"
	}
	return ""
}

// prepareUnsupported는 "문장이 틀렸다"가 아니라 "이 문장은 준비할 수 없다"는
// 드라이버 응답을 가려낸다.
//
// 이 구분이 없으면 멀쩡한 문장이 빨간 오류로 보인다. 예를 들어 MySQL은 일부 문장을
// 프리페어드 프로토콜에서 지원하지 않고(1295), 그때 돌려주는 것은 문법 오류가 아니다.
func prepareUnsupported(kind model.DBKind, err error) string {
	msg := strings.ToLower(err.Error())
	switch kind {
	case model.KindMySQL:
		if strings.Contains(msg, "1295") ||
			strings.Contains(msg, "not supported in the prepared statement protocol") {
			return "MySQL이 이 종류의 문장을 준비 단계에서 지원하지 않아 확인할 수 없습니다"
		}
	case model.KindPostgres:
		if strings.Contains(msg, "cannot insert multiple commands") {
			return "여러 명령이 한 문장에 들어 있어 확인할 수 없습니다"
		}
	}
	return ""
}
