package api

import (
	"strings"

	"github.com/gofiber/fiber/v2"

	"dbstudio/internal/model"
)

// AI 스킬.
//
// 스킬은 **미리 적어 둔 지시문**이다. 대화창에 사람이 매번 적던 것을 앱이 대신 적어
// 준다: "이 초안을 리뷰해 줘. 이름 규칙, 기본키, 외래키, 인덱스, 정규화를 순서대로
// 보고 심각한 것부터 알려 줘." 같은 문단은 쓸 때마다 조금씩 달라지고, 달라지면 답도
// 달라진다 — 그래서 결과가 사람마다 들쭉날쭉해진다.
//
// 실행 방식이 요점이다. 스킬은 **사용자의 말로** 대화에 들어간다. 새로운 실행 경로도,
// 새로운 권한도 없다: 모델은 지금까지와 같은 툴을 쓰고, 쓰기 작업은 여전히 승인을
// 거친다. 그래서 스킬을 더하는 일이 앱의 안전장치를 건드리지 않는다.
//
// 서버가 목록을 주는 이유(화면에 적어 두지 않고):
//   - 지시문이 툴 이름을 부른다. 툴은 서버가 정하므로, 툴 이름이 바뀌면 지시문도
//     같은 자리에서 바뀌어야 한다.
//   - 권한으로 거른다. 매크로를 만들 수 없는 사람에게 "매크로 만들기" 스킬을 보여
//     주면, 눌러서 승인 화면까지 간 뒤에 거부된다.

// skillArg는 스킬이 사람에게 물어보는 값 하나다.
type skillArg struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	// Type: connection | erd | text | number. 화면이 이 값으로 입력칸을 고른다.
	Type string `json:"type"`
	// Placeholder 는 자유 입력칸의 힌트다.
	Placeholder string `json:"placeholder,omitempty"`
	// Default 는 처음 값이다(숫자·글).
	Default string `json:"default,omitempty"`
	// Optional 이면 비워 둘 수 있다.
	Optional bool `json:"optional,omitempty"`
	// Multiline 이면 여러 줄 입력칸이다(SQL 처럼).
	Multiline bool `json:"multiline,omitempty"`
}

// aiSkill은 스킬 하나다.
type aiSkill struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Description string     `json:"description"`
	Icon        string     `json:"icon"`
	Args        []skillArg `json:"args,omitempty"`
	// Prompt 는 대화에 들어갈 지시문이다. {{key}} 자리에 사람이 고른 값이 들어간다.
	//
	// 치환은 **화면에서** 한다. 서버가 완성된 문장을 만들어 보내면 사람은 자기가
	// 무엇을 보내는지 모른 채 보내게 된다 — 스킬은 사용자의 말이므로, 보내기 전에
	// 그 말이 무엇인지 볼 수 있어야 한다.
	Prompt string `json:"prompt"`
	// RequiresPerm 이 있으면 그 권한을 가진 사람에게만 보인다.
	RequiresPerm model.Perm `json:"-"`
}

func aiSkills() []*aiSkill {
	return []*aiSkill{
		{
			ID:   "erd_review",
			Name: "ERD 설계 리뷰",
			Description: "초안을 읽고 이름 규칙·기본키·외래키·인덱스·정규화를 살펴 " +
				"고칠 곳을 심각한 것부터 알려줍니다.",
			Icon: "edit",
			Args: []skillArg{
				{Key: "document", Label: "리뷰할 초안", Type: "erd"},
				{Key: "focus", Label: "특히 볼 것", Type: "text", Optional: true,
					Placeholder: "예: 조회 성능, 이름 규칙 (비워 두면 전체)"},
			},
			Prompt: `{{document}} 초안을 설계 리뷰해 주세요.

먼저 erd_read_schema 로 전체 구조를 읽고(표가 많으면 offset 으로 이어 읽으세요),
아래 순서로 살펴본 뒤 **심각한 것부터** 정리해 주세요.

1. 기본키가 없거나 뜻이 불분명한 표
2. 외래키로 이어져야 하는데 이어지지 않은 컬럼(이름이 …_id 인데 관계가 없는 것)
3. 외래키 컬럼에 인덱스가 없는 것(대상 DB가 자동으로 만들지 않는 방언이면 조회가 느려집니다)
4. 이름 규칙이 어긋난 표·컬럼(단수/복수, 스네이크/카멜, 접두사)
5. 같은 뜻인데 표마다 타입이 다른 컬럼(도메인으로 묶을 후보)
6. 정규화 문제(반복 그룹, 한 컬럼에 여러 값, 갱신 이상)
7. NULL 허용이 뜻과 어긋나는 컬럼

각 항목은 **무엇이 / 왜 문제인지 / 어떻게 고칠지** 한 줄씩으로 적고, 마지막에 "지금
바로 고칠 것"과 "논의가 필요한 것"으로 나눠 주세요. 초안은 고치지 말고 보고만 해
주세요 — 무엇을 고칠지는 사람이 정합니다.
{{focus}}`,
		},
		{
			ID:   "sql_benchmark_macro",
			Name: "SQL 벤치마크 매크로 만들기",
			Description: "같은 쿼리를 여러 번 실행해 시간을 재는 매크로를 만듭니다. " +
				"만들기는 승인을 거칩니다.",
			Icon:         "activity",
			RequiresPerm: model.PermMacro,
			Args: []skillArg{
				{Key: "connection", Label: "대상 DB", Type: "connection"},
				{Key: "sql", Label: "잴 쿼리", Type: "text", Multiline: true,
					Placeholder: "SELECT ... 여러 개면 줄로 나눠 적으세요"},
				{Key: "runs", Label: "반복 횟수", Type: "number", Default: "20"},
			},
			Prompt: `아래 쿼리의 실행 시간을 재는 매크로를 만들어 주세요.

대상 DB: {{connection}}
반복 횟수: {{runs}}

쿼리:
{{sql}}

만들기 전에 describe_macro_nodes 로 쓸 수 있는 노드와 설정 칸을 확인하세요.
매크로는 이렇게 짜 주세요.

- 반복 횟수는 실행할 때 바꿀 수 있도록 **매개변수**로 둡니다(기본값 {{runs}}).
- 워밍업을 한 번 돌린 뒤 본 측정을 반복합니다 — 첫 실행은 캐시가 비어 있어 느립니다.
- 반복마다 걸린 시간을 남기고, 끝에 **횟수·평균·최소·최대**를 로그로 정리합니다.
- 쿼리는 읽기만 합니다. 값을 바꾸는 문장이면 그 사실을 먼저 알려 주고 멈추세요.

만든 뒤에는 무엇을 만들었는지(노드 흐름과 매개변수) 한 문단으로 설명해 주세요.
실행은 사람이 매크로 화면에서 합니다.`,
		},
		{
			ID:   "slow_query",
			Name: "느린 쿼리 진단",
			Description: "실행 계획과 스키마를 읽어 왜 느린지, 무엇을 고치면 되는지 " +
				"알려줍니다.",
			Icon: "search",
			Args: []skillArg{
				{Key: "connection", Label: "대상 DB", Type: "connection"},
				{Key: "sql", Label: "느린 쿼리", Type: "text", Multiline: true,
					Placeholder: "SELECT ..."},
			},
			Prompt: `{{connection}} 에서 아래 쿼리가 왜 느린지 봐 주세요.

{{sql}}

이 순서로 확인해 주세요.

1. run_sql 로 실행 계획을 뜹니다(EXPLAIN — PostgreSQL 이면 EXPLAIN (ANALYZE, BUFFERS),
   MySQL 이면 EXPLAIN ANALYZE, 방언에 맞는 것으로). **값을 바꾸는 문장에는 ANALYZE 를
   붙이지 마세요.**
2. introspect_schema 로 관련 표의 인덱스와 행 수를 봅니다.
3. 계획에서 비싼 단계를 짚고, 그 원인을 스키마와 이어서 설명합니다
   (풀 스캔, 잘못된 조인 순서, 인덱스 미사용, 형 변환으로 인덱스 무력화 등).

마지막에 **고칠 것을 순서대로** 적어 주세요. 인덱스를 제안할 때는 만들 DDL 을 그대로
적고, 그 인덱스가 어느 조회에 쓰이고 쓰기에는 얼마나 부담인지 한 줄로 덧붙여 주세요.
인덱스를 실제로 만들지는 마세요 — 제안까지만 해 주세요.`,
		},
		{
			ID:   "index_check",
			Name: "인덱스 점검",
			Description: "외래키·자주 쓰는 조건에 인덱스가 있는지, 겹치거나 쓰이지 않는 " +
				"인덱스가 있는지 봅니다.",
			Icon: "list",
			Args: []skillArg{
				{Key: "connection", Label: "대상 DB", Type: "connection"},
			},
			Prompt: `{{connection}} 의 인덱스를 점검해 주세요.

introspect_schema 로 표·인덱스·외래키를 읽고 아래를 봐 주세요.

1. **외래키 컬럼에 인덱스가 없는 것** — 부모 행을 지우거나 조인할 때 그 표를 통째로
   읽게 됩니다(대상 DB가 자동으로 만드는 방언인지 함께 밝혀 주세요).
2. **겹치는 인덱스** — (a) 와 (a, b) 가 함께 있으면 앞의 것은 대개 쓸모가 없습니다.
3. **기본키와 같은 컬럼의 유니크 인덱스**처럼 두 번 만든 것.
4. 컬럼이 너무 많은 인덱스, 선택도가 낮아 보이는 컬럼 하나짜리 인덱스.
5. 통계를 볼 수 있는 방언이면(PostgreSQL 의 pg_stat_user_indexes 등) run_sql 로
   **쓰이지 않는 인덱스**도 함께 찾아 주세요.

각 항목에 표·인덱스 이름과 근거를 적고, 지울 것과 만들 것을 DDL 로 정리해 주세요.
실제로 만들거나 지우지는 마세요.`,
		},
		{
			ID:          "logical_names",
			Name:        "논리명 채우기",
			Description: "초안의 표·컬럼에 한국어 논리명을 붙입니다. 반영은 바로 됩니다.",
			Icon:        "tag",
			Args: []skillArg{
				{Key: "document", Label: "대상 초안", Type: "erd"},
			},
			Prompt: `{{document}} 초안의 표와 컬럼에 한국어 논리명을 붙여 주세요.

erd_read_schema 로 이름·타입·주석을 읽고, 이름과 관계에서 뜻을 짐작해
erd_set_logical_names 로 한 번에 반영하세요.

규칙:
- 이미 논리명이 있는 것은 **건드리지 마세요**. 사람이 정한 말이 우선입니다.
- 뜻이 분명하지 않은 것(예: flag1, data2)은 붙이지 말고 목록으로 알려 주세요 —
  틀린 논리명은 없는 것보다 나쁩니다.
- 같은 뜻의 컬럼은 표가 달라도 같은 말을 쓰세요(created_at → "생성일시").
- 끝나면 무엇에 어떤 이름을 붙였는지, 그리고 건너뛴 것을 함께 정리해 주세요.`,
		},
	}
}

// handleListAISkills는 이 사용자가 쓸 수 있는 스킬 목록이다.
func (s *Server) handleListAISkills(c *fiber.Ctx) error {
	u := currentUser(c)
	out := make([]*aiSkill, 0, len(aiSkills()))
	for _, sk := range aiSkills() {
		if sk.RequiresPerm != "" && !u.HasPerm(sk.RequiresPerm) {
			continue
		}
		out = append(out, sk)
	}
	return c.JSON(fiber.Map{"items": out})
}

// skillPromptFor는 시험과 다른 서버 코드가 스킬 지시문을 찾을 때 쓴다.
func skillPromptFor(id string) string {
	for _, sk := range aiSkills() {
		if strings.EqualFold(sk.ID, id) {
			return sk.Prompt
		}
	}
	return ""
}
