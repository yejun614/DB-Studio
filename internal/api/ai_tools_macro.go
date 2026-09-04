package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"dbstudio/internal/macro"
	"dbstudio/internal/model"
	"dbstudio/internal/store"
)

// 매크로를 만드는 툴.
//
// 왜 필요한가: 어시스턴트에게 "이 쿼리 벤치마크 매크로 만들어 줘"라고 하면, 지금까지는
// 노드 그래프를 글로 설명하는 답만 받았다. 그 글을 사람이 매크로 편집기에서 노드
// 스무 개로 다시 그리는 일이 남고, 그 옮겨 적기가 이 요청의 대부분이었다.
//
// **승인을 거친다.** 매크로는 나중에 사람이 실행하는 물건이고, 그 안에는 SQL 과 셸이
// 들어갈 수 있다. 만들어진 그래프를 사람이 보고 결정해야 한다 — 제안 미리보기에
// 노드 흐름을 담는 이유가 그것이다.
//
// 모델이 그래프를 지어내지 않게 describe_macro_nodes 를 함께 둔다. 노드마다 어떤
// 설정 칸이 있는지는 서버가 아는 것이고, 그것을 모르면 모델은 그럴듯한 이름의 칸을
// 지어낸다 — 그런 매크로는 저장은 되고 실행에서 실패한다.

func macroTools() []*aiTool {
	return []*aiTool{
		{
			Name: "describe_macro_nodes",
			Description: "매크로에 쓸 수 있는 노드 종류와 각 노드의 설정 칸을 반환한다. " +
				"create_macro 로 매크로를 만들기 전에 반드시 먼저 부른다 — 칸 이름을 " +
				"지어내면 저장은 되고 실행에서 실패한다.",
			Schema:       objectSchema(nil),
			RequiresPerm: model.PermMacro,
			Run:          toolDescribeMacroNodes,
		},
		{
			Name: "create_macro",
			Description: "노드 그래프로 새 매크로를 만든다. graph 는 {nodes, edges, params} 객체다. " +
				"노드 종류와 설정 칸은 describe_macro_nodes 로 먼저 확인한다. " +
				"사용자 승인이 필요하다.",
			Schema: objectSchema(map[string]any{
				"name":        str("매크로 이름"),
				"description": str("이 매크로가 하는 일 (한 줄)"),
				"graph": map[string]any{
					"type": "object",
					"description": "매크로 그래프. nodes[{id,type,label,x,y,params,output}], " +
						"edges[{id,from,fromPort,to}], params[{name,label,type,default}]. " +
						"start 노드가 하나 있어야 하고 모든 노드가 그 뒤로 이어져야 한다.",
				},
			}, "name", "graph"),
			Mutating:     true,
			RequiresPerm: model.PermMacro,
			Propose:      proposeCreateMacro,
			Apply:        applyCreateMacro,
		},
	}
}

func toolDescribeMacroNodes(tc *toolContext, args json.RawMessage) (string, error) {
	specs := macro.Specs()
	items := make([]map[string]any, 0, len(specs))
	for _, sp := range specs {
		fields := make([]map[string]any, 0, len(sp.Fields))
		for _, f := range sp.Fields {
			field := map[string]any{"key": f.Key, "label": f.Label, "type": f.Type}
			if f.Required {
				field["required"] = true
			}
			if f.Help != "" {
				field["help"] = f.Help
			}
			fields = append(fields, field)
		}
		item := map[string]any{
			"type": sp.Type, "label": sp.Label, "group": sp.Group,
			"description": sp.Description, "ports": sp.Ports, "fields": fields,
		}
		if sp.NeedsShell {
			item["needsShell"] = true
		}
		items = append(items, item)
	}
	return asJSON(map[string]any{
		"nodes": items,
		"note": "값에 ${변수} 를 쓰면 실행 시점에 치환된다. 노드의 output 에 이름을 주면 " +
			"그 결과를 뒤 노드에서 그 이름으로 쓴다. 흐름은 edges 로 잇고, 대부분의 노드는 " +
			"out 포트 하나를 쓴다(branch 는 true/false, foreach 는 body/done).",
	})
}

type createMacroArgs struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Graph       json.RawMessage `json:"graph"`
}

// parseMacroGraph는 인자의 그래프를 읽고 검증한다.
//
// 제안과 실행이 같은 함수를 쓴다. 승인은 며칠 뒤에 눌릴 수 있고, 그때도 저장되는 것은
// 사람이 승인 화면에서 본 그래프여야 한다.
func parseMacroGraph(in createMacroArgs) (*macro.Graph, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return nil, errors.New("매크로 이름(name)을 지정하세요")
	}
	if len(in.Graph) == 0 {
		return nil, errors.New("graph 를 지정하세요 (nodes/edges/params)")
	}
	var g macro.Graph
	if err := json.Unmarshal(in.Graph, &g); err != nil {
		return nil, fmt.Errorf("graph 를 해석할 수 없습니다: %w", err)
	}
	// 화면에서 만든 매크로와 같은 검증을 지난다. 여기서 느슨하게 받으면 저장은 되고
	// 실행에서 실패하는 매크로가 남는다 — 그 실패는 만든 사람이 아니라 며칠 뒤에
	// 실행 버튼을 누른 사람이 본다.
	issues := g.Validate(macro.KnownTypes())
	fatal := []string{}
	for _, is := range issues {
		if is.Fatal {
			fatal = append(fatal, is.Message)
		}
	}
	if len(fatal) > 0 {
		return nil, fmt.Errorf("그래프가 올바르지 않습니다: %s", strings.Join(fatal, " / "))
	}
	return &g, nil
}

func proposeCreateMacro(tc *toolContext, args json.RawMessage) (string, any, error) {
	var in createMacroArgs
	if err := parseArgs(args, &in); err != nil {
		return "", nil, err
	}
	g, err := parseMacroGraph(in)
	if err != nil {
		return "", nil, err
	}

	// 미리보기에는 **흐름**을 담는다. 그래프 JSON 을 그대로 보여 주면 사람이 읽어야
	// 하는 것은 백 줄짜리 좌표 목록이고, 정작 판단에 필요한 것은 "무엇을 어느 DB에
	// 대고 어떤 순서로 하는가"다.
	steps := make([]string, 0, len(g.Nodes))
	conns := map[string]bool{}
	warnings := []string{}
	for _, n := range g.Nodes {
		label := n.Label
		if label == "" {
			label = n.Type
		}
		steps = append(steps, fmt.Sprintf("%s (%s)", label, n.Type))
		if c, ok := n.Params["connection"].(string); ok && strings.TrimSpace(c) != "" {
			conns[c] = true
		}
		// 값을 바꾸는 노드와 셸은 따로 세어 둔다. 승인하는 사람이 가장 먼저 알아야
		// 하는 것이고, 노드 목록 스무 줄 안에 섞여 있으면 눈에 띄지 않는다.
		switch n.Type {
		case macro.TypeSQLExec, macro.TypeDataMutate:
			warnings = append(warnings, fmt.Sprintf("%s 노드가 값을 바꿉니다", label))
		case macro.TypeShell:
			warnings = append(warnings, fmt.Sprintf("%s 노드가 서버에서 셸 명령을 실행합니다", label))
		}
	}
	targets := make([]string, 0, len(conns))
	for c := range conns {
		targets = append(targets, c)
	}

	preview := map[string]any{
		"name":  strings.TrimSpace(in.Name),
		"nodes": len(g.Nodes),
		"steps": steps,
	}
	if d := strings.TrimSpace(in.Description); d != "" {
		preview["description"] = d
	}
	if len(targets) > 0 {
		preview["connections"] = targets
	}
	if len(g.Params) > 0 {
		keys := make([]string, 0, len(g.Params))
		for _, p := range g.Params {
			keys = append(keys, p.Name)
		}
		preview["params"] = keys
	}
	if len(warnings) > 0 {
		preview["warnings"] = warnings
	}

	summary := fmt.Sprintf("매크로 %q 를 만듭니다 (노드 %d개)", strings.TrimSpace(in.Name), len(g.Nodes))
	if len(targets) > 0 {
		summary += " — 대상: " + strings.Join(targets, ", ")
	}
	return summary, preview, nil
}

func applyCreateMacro(tc *toolContext, args json.RawMessage) (string, error) {
	var in createMacroArgs
	if err := parseArgs(args, &in); err != nil {
		return "", err
	}
	g, err := parseMacroGraph(in)
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(g)
	if err != nil {
		return "", err
	}
	m, err := tc.srv.st.CreateMacro(tc.ctx, strings.TrimSpace(in.Name),
		strings.TrimSpace(in.Description), string(raw), tc.user, displayName(tc.user))
	if errors.Is(err, store.ErrDuplicateName) {
		return "", errors.New("같은 이름의 매크로가 이미 있습니다")
	}
	if err != nil {
		return "", err
	}
	tc.audit("macro.create", "macro", m.ID, "", map[string]any{
		"name": m.Name, "nodes": len(g.Nodes), "via": "ai",
	})
	return asJSON(map[string]any{
		"id": m.ID, "name": m.Name, "version": m.CurrentVersion,
		"note": "매크로를 만들었습니다. 실행은 사람이 매크로 화면에서 합니다.",
	})
}
