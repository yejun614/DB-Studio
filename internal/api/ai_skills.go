package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"regexp"
	"strings"

	"github.com/gofiber/fiber/v2"

	"dbstudio/internal/model"
	"dbstudio/internal/store"
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
	ID string `json:"id"`
	// Builtin이면 앱이 들고 있는 스킬이다(고칠 수 없다).
	//
	// 화면이 이 값으로 연필·휴지통을 감춘다. 서버도 같은 판정을 다시 한다 —
	// 화면에서 감추는 것은 안내이지 규칙이 아니다.
	Builtin bool `json:"builtin"`
	// Mine이면 내가 만든 것이다(고치고 지울 수 있다).
	Mine        bool       `json:"mine,omitempty"`
	Shared      bool       `json:"shared,omitempty"`
	Owner       string     `json:"owner,omitempty"`
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
//
// 앱의 것을 앞에, 사람이 만든 것을 뒤에 둔다. 새로 만든 스킬을 목록 맨 아래에서
// 찾게 되는 것이 이상해 보일 수 있지만, 앱의 것은 자리가 늘 같아야 손이 기억한다.
func (s *Server) handleListAISkills(c *fiber.Ctx) error {
	u := currentUser(c)
	out := []*aiSkill{}
	for _, sk := range aiSkills() {
		if sk.RequiresPerm != "" && !u.HasPerm(sk.RequiresPerm) {
			continue
		}
		copy := *sk
		copy.Builtin = true
		out = append(out, &copy)
	}

	mine, err := s.st.ListAISkills(c.Context(), u.ID)
	if err != nil {
		return err
	}
	for _, row := range mine {
		sk, cerr := skillFromRow(row, u)
		if cerr != nil {
			// 한 줄이 깨졌다고 목록 전체를 못 쓰게 하지 않는다. 그 스킬만 빠진다.
			slog.Error("스킬을 읽지 못했습니다", "id", row.ID, "error", cerr)
			continue
		}
		out = append(out, sk)
	}
	return c.JSON(fiber.Map{"items": out})
}

// skillFromRow는 저장된 줄을 화면이 쓰는 모양으로 바꾼다.
func skillFromRow(row *store.AISkill, u *model.User) (*aiSkill, error) {
	args := []skillArg{}
	if strings.TrimSpace(row.Args) != "" {
		if err := json.Unmarshal([]byte(row.Args), &args); err != nil {
			return nil, err
		}
	}
	owner := row.CreatedName
	if owner == "" {
		owner = "(알 수 없음)"
	}
	return &aiSkill{
		ID: row.ID, Name: row.Name, Description: row.Description,
		Icon: row.Icon, Args: args, Prompt: row.Prompt,
		Mine:   row.CreatedBy == u.ID || u.Role.CanManageUsers(),
		Shared: row.Shared,
		Owner:  owner,
	}, nil
}

// skillSlotRe는 지시문의 {{열쇠}} 자리다.
var skillSlotRe = regexp.MustCompile(`\{\{(\w+)\}\}`)

type saveSkillBody struct {
	Name        string     `json:"name"`
	Description string     `json:"description"`
	Icon        string     `json:"icon"`
	Prompt      string     `json:"prompt"`
	Args        []skillArg `json:"args"`
	Shared      bool       `json:"shared"`
}

// validate는 저장 전에 스킬을 확인한다.
//
// 지시문과 인자가 서로를 가리키는지 보는 것이 요점이다. 어긋나면 두 가지가 조용히
// 일어난다: 채울 인자가 없는 자리는 "{{focus}}" 그대로 남아 모델이 그 글자를 지시로
// 읽고, 쓰이지 않는 인자는 사람에게 묻고 나서 아무 데도 안 쓰인다. 둘 다 화면에서는
// "답이 좀 이상하다"로만 보인다 — 그래서 만들 때 막는다.
func (b *saveSkillBody) validate() (store.SaveAISkillParams, error) {
	var out store.SaveAISkillParams
	name := strings.TrimSpace(b.Name)
	prompt := strings.TrimSpace(b.Prompt)
	if name == "" {
		return out, errors.New("스킬 이름을 적으세요")
	}
	if len([]rune(name)) > 60 {
		return out, errors.New("스킬 이름이 너무 깁니다 (60자 제한)")
	}
	if prompt == "" {
		return out, errors.New("지시문을 적으세요")
	}
	if len([]rune(prompt)) > 20000 {
		return out, errors.New("지시문이 너무 깁니다 (20000자 제한)")
	}

	keys := map[string]bool{}
	for i := range b.Args {
		a := &b.Args[i]
		a.Key = strings.TrimSpace(a.Key)
		a.Label = strings.TrimSpace(a.Label)
		if a.Key == "" {
			return out, errors.New("입력값의 열쇠(key)를 적으세요")
		}
		if !regexp.MustCompile(`^\w+$`).MatchString(a.Key) {
			return out, errors.New("입력값의 열쇠는 영문·숫자·밑줄만 쓸 수 있습니다: " + a.Key)
		}
		if keys[a.Key] {
			return out, errors.New("입력값의 열쇠가 겹칩니다: " + a.Key)
		}
		keys[a.Key] = true
		if a.Label == "" {
			a.Label = a.Key
		}
		switch a.Type {
		case "connection", "erd", "text", "number":
		default:
			return out, errors.New("입력값 종류가 올바르지 않습니다: " + a.Type)
		}
	}

	used := map[string]bool{}
	for _, m := range skillSlotRe.FindAllStringSubmatch(prompt, -1) {
		used[m[1]] = true
		if !keys[m[1]] {
			return out, errors.New("지시문의 {{" + m[1] + "}} 를 채울 입력값이 없습니다")
		}
	}
	for key := range keys {
		if !used[key] {
			return out, errors.New("입력값 " + key + " 를 지시문에서 쓰지 않습니다 " +
				"({{" + key + "}} 를 넣으세요)")
		}
	}

	raw, err := json.Marshal(b.Args)
	if err != nil {
		return out, err
	}
	return store.SaveAISkillParams{
		Name: name, Description: strings.TrimSpace(b.Description),
		Icon: strings.TrimSpace(b.Icon), Prompt: prompt,
		Args: string(raw), Shared: b.Shared,
	}, nil
}

func (s *Server) handleCreateAISkill(c *fiber.Ctx) error {
	var body saveSkillBody
	if err := c.BodyParser(&body); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "요청 본문을 해석할 수 없습니다")
	}
	params, err := body.validate()
	if err != nil {
		return fail(c, fiber.StatusBadRequest, "invalid_skill", err.Error())
	}
	u := currentUser(c)
	row, err := s.st.CreateAISkill(c.Context(), params, u.ID)
	if err != nil {
		return err
	}
	s.audit(c, store.AuditParams{
		Action: "ai.skill.create", TargetType: "ai_skill", TargetID: row.ID,
		Detail: map[string]any{"name": row.Name, "shared": row.Shared},
	})
	sk, cerr := skillFromRow(row, u)
	if cerr != nil {
		return cerr
	}
	return c.Status(fiber.StatusCreated).JSON(fiber.Map{"skill": sk})
}

// requireOwnSkill은 고치거나 지울 수 있는 스킬인지 본다.
//
// 공유된 스킬은 누구나 **쓸** 수 있지만 고치는 것은 주인(과 사용자 관리자)뿐이다.
// 남이 쓰고 있는 글을 아무나 바꿀 수 있으면 "어제 쓰던 스킬이 오늘 다른 말을 한다"가
// 된다 — 그때 답이 달라진 이유를 아무도 찾지 못한다.
func (s *Server) requireOwnSkill(c *fiber.Ctx) (*store.AISkill, error) {
	row, err := s.st.GetAISkill(c.Context(), c.Params("id"))
	if errors.Is(err, store.ErrNotFound) {
		return nil, fiber.NewError(fiber.StatusNotFound, "스킬을 찾을 수 없습니다")
	}
	if err != nil {
		return nil, err
	}
	u := currentUser(c)
	if row.CreatedBy != u.ID && !u.Role.CanManageUsers() {
		return nil, fiber.NewError(fiber.StatusForbidden, "내가 만든 스킬만 고칠 수 있습니다")
	}
	return row, nil
}

func (s *Server) handleUpdateAISkill(c *fiber.Ctx) error {
	row, err := s.requireOwnSkill(c)
	if err != nil {
		return err
	}
	var body saveSkillBody
	if perr := c.BodyParser(&body); perr != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "요청 본문을 해석할 수 없습니다")
	}
	params, verr := body.validate()
	if verr != nil {
		return fail(c, fiber.StatusBadRequest, "invalid_skill", verr.Error())
	}
	next, uerr := s.st.UpdateAISkill(c.Context(), row.ID, params)
	if uerr != nil {
		return uerr
	}
	s.audit(c, store.AuditParams{
		Action: "ai.skill.update", TargetType: "ai_skill", TargetID: row.ID,
		Detail: map[string]any{"name": next.Name, "shared": next.Shared},
	})
	sk, cerr := skillFromRow(next, currentUser(c))
	if cerr != nil {
		return cerr
	}
	return c.JSON(fiber.Map{"skill": sk})
}

func (s *Server) handleDeleteAISkill(c *fiber.Ctx) error {
	row, err := s.requireOwnSkill(c)
	if err != nil {
		return err
	}
	if derr := s.st.DeleteAISkill(c.Context(), row.ID); derr != nil {
		return derr
	}
	s.audit(c, store.AuditParams{
		Action: "ai.skill.delete", TargetType: "ai_skill", TargetID: row.ID,
		Detail: map[string]any{"name": row.Name},
	})
	return c.SendStatus(fiber.StatusNoContent)
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
