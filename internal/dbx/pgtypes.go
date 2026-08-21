package dbx

import (
	"fmt"
	"strings"
)

// pgArray는 문자열 슬라이스를 PostgreSQL 배열 리터럴로 만든다.
// 쿼리에서는 `= ANY($1::text[])` 처럼 명시적 캐스트와 함께 쓴다.
// 드라이버의 배열 변환 동작에 의존하지 않기 위해 직접 만든다.
func pgArray(values []string) string {
	parts := make([]string, len(values))
	for i, v := range values {
		// 배열 리터럴 안에서는 백슬래시와 큰따옴표를 이스케이프해야 한다.
		escaped := strings.ReplaceAll(v, `\`, `\\`)
		escaped = strings.ReplaceAll(escaped, `"`, `\"`)
		parts[i] = `"` + escaped + `"`
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// pgTextArray는 text[] 컬럼을 스캔한다.
// database/sql은 배열 타입을 모르므로 리터럴 문자열을 직접 파싱한다.
type pgTextArray struct {
	Values []string
}

func (a *pgTextArray) Scan(src any) error {
	a.Values = []string{}
	if src == nil {
		return nil
	}
	var raw string
	switch v := src.(type) {
	case string:
		raw = v
	case []byte:
		raw = string(v)
	default:
		return fmt.Errorf("pgTextArray: 지원하지 않는 소스 타입 %T", src)
	}
	a.Values = parsePGArray(raw)
	return nil
}

// parsePGArray는 `{a,b,"c,d"}` 형태의 배열 리터럴을 슬라이스로 만든다.
func parsePGArray(raw string) []string {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "{")
	raw = strings.TrimSuffix(raw, "}")
	if raw == "" {
		return []string{}
	}

	out := []string{}
	var cur strings.Builder
	inQuote := false
	escaped := false
	for _, r := range raw {
		switch {
		case escaped:
			cur.WriteRune(r)
			escaped = false
		case r == '\\':
			escaped = true
		case r == '"':
			inQuote = !inQuote
		case r == ',' && !inQuote:
			out = append(out, cur.String())
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	out = append(out, cur.String())
	return out
}
