package api

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"dbstudio/internal/model"
	"dbstudio/internal/store"
)

// 일반 어시스턴트에서 ERD 초안을 고치는 툴.
//
// ERD 화면 안의 대화는 문서 하나에 매여 있어 그 상자만 쓴다(ai_tools_erd.go).
// 일반 어시스턴트는 "무엇을 고칠까"부터 정해야 하므로, 같은 툴에 **document 인자
// 하나만 더해 감싼다.**
//
// 구현을 베끼지 않는 것이 요점이다. 초안을 고치는 규칙(입력 검증, op-log 기록,
// 같은 문서를 열어 둔 사람들의 화면 갱신)은 한 곳에만 있어야 한다 — 두 벌이 되면
// 그중 하나에서 검증이 빠지고, 빠진 쪽으로만 이상한 스키마가 들어온다.
//
// **승인 단계를 두지 않는 이유도 그대로다**(지우는 툴만 예외다). 대상이 초안이라 진짜 DB에 닿지 않고,
// 모든 변경이 op-log에 남으며, 컬럼 다섯 개를 더하는 데 승인 다섯 번을 요구하면
// 아무도 쓰지 않는다. 그 초안을 실제 DB에 반영하는 단계(마이그레이션)는 지금까지의
// 관문을 그대로 지난다 — 검토자 승인, 사전 검사, 운영 DB 확인 문구.
//
// 이름 앞에 erd_ 를 붙인다. 앱 툴 상자에는 이미 read_schema 같은 이름이 있어서,
// 그대로 넣으면 모델이 "지금 이 read_schema 가 DB의 것인가 초안의 것인가"를
// 헷갈린다.

const erdToolPrefix = "erd_"

// erdDocTools는 ERD 툴을 일반 어시스턴트용으로 감싼다.
func erdDocTools() []*aiTool {
	inner := erdTools()
	names := make([]string, 0, len(inner))
	for name := range inner {
		names = append(names, name)
	}
	slices.Sort(names)

	out := make([]*aiTool, 0, len(names))
	for _, name := range names {
		t := inner[name]
		wrapped := &aiTool{
			Name:        erdToolPrefix + t.Name,
			Description: t.Description + " 어느 초안인지 document 로 알려준다(이름 또는 ID).",
			Schema:      withDocumentArg(t.Schema),
		}
		if t.Mutating {
			// 승인이 필요한 툴(컬럼 삭제)은 제안과 실행으로 갈라 잇는다. 두 단계
			// 모두 문서를 다시 찾는다 — 승인은 며칠 뒤에 눌릴 수 있고, 그 사이에
			// 권한이 회수되거나 문서가 사라질 수 있다.
			wrapped.Mutating = true
			wrapped.Propose = func(tc *toolContext, args json.RawMessage) (string, any, error) {
				ec, err := tc.openERDDoc(args)
				if err != nil {
					return "", nil, err
				}
				return t.Propose(ec, args)
			}
			wrapped.Apply = func(tc *toolContext, args json.RawMessage) (string, error) {
				ec, err := tc.openERDDoc(args)
				if err != nil {
					return "", err
				}
				return t.Run(ec, args)
			}
		} else {
			// 나머지는 승인 없이 바로 반영된다(위 주석 참고). 초안은 실제 DB가 아니다.
			wrapped.Run = func(tc *toolContext, args json.RawMessage) (string, error) {
				ec, err := tc.openERDDoc(args)
				if err != nil {
					return "", err
				}
				// 안쪽 툴은 자기 인자만 읽고 모르는 필드(document)는 지나친다.
				return t.Run(ec, args)
			}
		}
		out = append(out, wrapped)
	}
	return out
}

// withDocumentArg는 툴 스키마에 document를 필수로 끼워 넣는다.
//
// 원본을 고치지 않고 복사하는 이유: 같은 map이 ERD 화면 쪽 툴 정의에도 쓰인다.
// 거기서는 문서가 이미 정해져 있으므로 document를 물으면 안 된다.
func withDocumentArg(schema map[string]any) map[string]any {
	props := map[string]any{
		"document": str("고칠 ERD 초안의 이름 또는 ID. " +
			"모르면 list_erd_documents 로 먼저 목록을 본다."),
	}
	required := []string{"document"}

	if schema != nil {
		if old, ok := schema["properties"].(map[string]any); ok {
			for k, v := range old {
				props[k] = v
			}
		}
		if old, ok := schema["required"].([]string); ok {
			required = append(required, old...)
		}
	}
	return map[string]any{"type": "object", "properties": props, "required": required}
}

// openERDDoc은 document 인자가 가리키는 초안을 찾고 고칠 수 있는지 본다.
//
// 찾는 범위 자체가 접근 판정이다. 목록은 **볼 수 있는 커넥션과 참여한 프로젝트**로
// 이미 좁혀져 있으므로, 여기서 나오지 않는 문서는 이 사람에게 없는 것과 같다.
// 남의 문서에 "찾을 수 없습니다"로 답하는 것은 그 자체로 옳다 — 있다는 사실도
// 알려 주지 않는다.
func (tc *toolContext) openERDDoc(args json.RawMessage) (*erdToolContext, error) {
	var in struct {
		Document string `json:"document"`
	}
	if err := parseArgs(args, &in); err != nil {
		return nil, err
	}
	want := strings.TrimSpace(in.Document)
	if want == "" {
		return nil, fmt.Errorf("어느 초안을 고칠지 document 로 알려주세요(이름 또는 ID). " +
			"list_erd_documents 로 목록을 볼 수 있습니다")
	}

	conns, _, err := tc.accessibleConns(model.LevelMonitor)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(conns))
	for _, c := range conns {
		ids = append(ids, c.ID)
	}
	docs, err := tc.srv.st.ListERDDocuments(tc.ctx, ids, tc.projectScope(), 500)
	if err != nil {
		return nil, err
	}

	var found *store.ERDDocumentMeta
	matches := []string{}
	for _, d := range docs {
		if d.ID == want {
			found = d
			matches = []string{d.Name}
			break
		}
		if strings.EqualFold(d.Name, want) {
			found = d
			matches = append(matches, d.Name)
		}
	}
	if found == nil {
		return nil, fmt.Errorf("초안 %q 을(를) 찾을 수 없습니다. "+
			"list_erd_documents 로 목록을 확인하세요", want)
	}
	if len(matches) > 1 {
		return nil, fmt.Errorf("이름이 %q 인 초안이 %d개입니다. ID로 지정하세요 — "+
			"list_erd_documents 가 ID를 함께 돌려줍니다", want, len(matches))
	}

	ec := &erdToolContext{tc: tc, docID: found.ID, connID: found.ConnectionID}
	if !tc.srv.erdCanEdit(ec, tc.user) {
		return nil, fmt.Errorf("초안 %q 을(를) 고칠 권한이 없습니다(대상 DB의 ERD 등급 필요)", found.Name)
	}
	return ec, nil
}
