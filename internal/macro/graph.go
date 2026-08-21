// Package macro는 노드 그래프로 작성한 매크로를 저장·검증·실행한다.
//
// 실행 모델은 **제어 흐름 + 변수 가방**이다. 노드끼리 잇는 선은 "다음에 무엇을
// 실행하는가"만 나타내고, 데이터는 실행 문맥의 변수 가방을 통해 오간다.
//
// 데이터 포트를 선으로 잇는 방식(진짜 데이터플로우)을 쓰지 않은 이유가 있다.
// DB 자동화에서 노드 하나가 만드는 것은 대개 "행 목록" 하나인데, 그것을 쓰는 쪽은
// 세 노드 뒤의 조건식이거나 Lua 스크립트 안이다. 데이터 선으로 표현하려면 화면이
// 선으로 뒤덮이고, 정작 사람이 알고 싶은 "무엇이 어떤 순서로 실행되는가"가 묻힌다.
// 변수 가방은 그 순서를 선으로 보여주고, 데이터 참조는 이름으로 한다.
package macro

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// Graph는 매크로 한 버전의 내용 전부다.
type Graph struct {
	Nodes []*Node `json:"nodes"`
	Edges []*Edge `json:"edges"`
	// Params는 실행할 때 사용자가 입력하는 값의 정의다.
	Params []*ParamDef `json:"params"`
	// View는 캔버스 상태(팬/줌)다. 실행에 영향을 주지 않지만 버전과 함께 저장해야
	// 이력에서 되돌렸을 때 보던 자리가 유지된다.
	View map[string]float64 `json:"view,omitempty"`
	// Notes와 Groups는 캔버스 주석이다. 실행에 전혀 관여하지 않는다.
	//
	// 그런데도 그래프에 함께 저장하는 이유: 노드가 스무 개를 넘어가면 "이 묶음이
	// 무엇을 하는 부분인가"를 그림만 보고 알 수 없다. 그 설명이 매크로 밖에 있으면
	// 버전을 되돌렸을 때 설명만 남아 어긋난다. 같은 버전에 함께 담아야 짝이 맞는다.
	Notes  []*Note  `json:"notes,omitempty"`
	Groups []*Group `json:"groups,omitempty"`
}

// Note는 캔버스에 붙이는 메모다.
type Note struct {
	ID   string  `json:"id"`
	Text string  `json:"text"`
	X    float64 `json:"x"`
	Y    float64 `json:"y"`
	W    float64 `json:"w,omitempty"`
	H    float64 `json:"h,omitempty"`
	// Color는 팔레트 키다(yellow | blue | green | pink | gray). 빈 값이면 기본색.
	Color string `json:"color,omitempty"`
}

// Group은 노드 여러 개를 감싸는 반투명 사각형이다.
// 노드를 실제로 담지는 않는다 — 겹쳐 놓는 것만으로 묶여 보이면 충분하고,
// 소속을 데이터로 관리하면 노드를 옮길 때마다 소속이 바뀌어 오히려 성가시다.
type Group struct {
	ID    string  `json:"id"`
	Label string  `json:"label"`
	X     float64 `json:"x"`
	Y     float64 `json:"y"`
	W     float64 `json:"w"`
	H     float64 `json:"h"`
	Color string  `json:"color,omitempty"`
}

// Node는 그래프의 노드 하나다.
type Node struct {
	ID    string  `json:"id"`
	Type  string  `json:"type"`
	Label string  `json:"label"`
	X     float64 `json:"x"`
	Y     float64 `json:"y"`
	// Params는 노드 종류별 설정이다. 값에 ${변수} 를 쓰면 실행 시점에 치환된다.
	Params map[string]any `json:"params"`
	// NodeRef는 Type이 "custom"일 때 참조하는 노드 정의의 ID다.
	NodeRef string `json:"nodeRef,omitempty"`
	// Output은 이 노드의 결과를 담을 변수 이름이다. 비어 있으면 노드 ID를 쓴다.
	Output string `json:"output,omitempty"`
	// Disabled면 실행 시 건너뛴다. 지우지 않고 잠시 빼두는 일이 잦다 —
	// 지웠다가 다시 그리면 연결도 다시 이어야 한다.
	Disabled bool `json:"disabled,omitempty"`
	// ContinueOnError면 이 노드가 실패해도 다음으로 넘어간다.
	// 정리 작업처럼 "없으면 없는 대로 괜찮은" 단계에 쓴다.
	ContinueOnError bool `json:"continueOnError,omitempty"`
}

// Edge는 제어 흐름 연결이다.
// FromPort는 출력 포트 이름이며, 대부분의 노드는 "out" 하나만 가진다.
type Edge struct {
	ID       string `json:"id"`
	From     string `json:"from"`
	FromPort string `json:"fromPort"`
	To       string `json:"to"`
}

// ParamDef는 실행 파라미터 정의다.
type ParamDef struct {
	Name     string `json:"name"`
	Label    string `json:"label"`
	Type     string `json:"type"` // string | number | boolean | connection | text
	Default  any    `json:"default,omitempty"`
	Required bool   `json:"required,omitempty"`
	Help     string `json:"help,omitempty"`
}

// ParseGraph는 저장된 JSON을 그래프로 되돌린다.
func ParseGraph(raw string) (*Graph, error) {
	g := &Graph{}
	if strings.TrimSpace(raw) == "" {
		return &Graph{Nodes: []*Node{}, Edges: []*Edge{}, Params: []*ParamDef{}}, nil
	}
	if err := json.Unmarshal([]byte(raw), g); err != nil {
		return nil, fmt.Errorf("그래프 형식이 올바르지 않습니다: %w", err)
	}
	if g.Nodes == nil {
		g.Nodes = []*Node{}
	}
	if g.Edges == nil {
		g.Edges = []*Edge{}
	}
	if g.Params == nil {
		g.Params = []*ParamDef{}
	}
	return g, nil
}

func (g *Graph) JSON() (string, error) {
	b, err := json.Marshal(g)
	if err != nil {
		return "", fmt.Errorf("그래프 직렬화 실패: %w", err)
	}
	return string(b), nil
}

// Node는 ID로 노드를 찾는다.
func (g *Graph) Node(id string) *Node {
	for _, n := range g.Nodes {
		if n.ID == id {
			return n
		}
	}
	return nil
}

// Next는 한 포트에서 나가는 대상 노드들을 반환한다.
//
// 하나가 아니라 여럿을 허용하는 이유: 한 노드 뒤에 서로 무관한 작업 둘을 붙이는
// 것은 흔한 구성이다. 순서는 간선이 그려진 순서를 따른다(안정적이어야 실행 결과가
// 재현된다).
func (g *Graph) Next(nodeID, port string) []string {
	out := []string{}
	for _, e := range g.Edges {
		if e.From == nodeID && e.FromPort == port {
			out = append(out, e.To)
		}
	}
	return out
}

// Start는 시작 노드를 찾는다.
func (g *Graph) Start() *Node {
	for _, n := range g.Nodes {
		if n.Type == TypeStart {
			return n
		}
	}
	return nil
}

// ValidationIssue는 검증에서 발견한 문제 하나다.
// 오류(Fatal)는 저장을 막고, 경고는 저장은 되지만 화면에 표시된다.
type ValidationIssue struct {
	NodeID  string `json:"nodeId,omitempty"`
	Fatal   bool   `json:"fatal"`
	Message string `json:"message"`
}

// Validate는 그래프를 검사한다.
//
// 저장 시점에 검사하는 이유: 매크로는 저장된 뒤에야 실행할 수 있고(요구사항),
// 실행은 다른 사람이 누른다. 깨진 그래프를 저장하게 두면 그 사람이 실행 버튼을
// 눌러서야 문제를 알게 된다.
func (g *Graph) Validate(knownNodeTypes map[string]bool) []ValidationIssue {
	issues := []ValidationIssue{}
	add := func(nodeID string, fatal bool, format string, args ...any) {
		issues = append(issues, ValidationIssue{
			NodeID: nodeID, Fatal: fatal, Message: fmt.Sprintf(format, args...),
		})
	}

	if len(g.Nodes) == 0 {
		add("", true, "노드가 하나도 없습니다")
		return issues
	}

	seen := map[string]bool{}
	starts := 0
	for _, n := range g.Nodes {
		if strings.TrimSpace(n.ID) == "" {
			add("", true, "ID가 없는 노드가 있습니다")
			continue
		}
		if seen[n.ID] {
			add(n.ID, true, "노드 ID가 중복됩니다: %s", n.ID)
		}
		seen[n.ID] = true

		if n.Type == TypeStart {
			starts++
		}
		if n.Type == TypeCustom {
			if strings.TrimSpace(n.NodeRef) == "" {
				add(n.ID, true, "사용자 노드가 어떤 정의를 쓰는지 지정되지 않았습니다")
			}
		} else if !knownNodeTypes[n.Type] {
			add(n.ID, true, "알 수 없는 노드 종류입니다: %s", n.Type)
		}
		if n.Output != "" && !validVarName(n.Output) {
			add(n.ID, true, "출력 변수 이름은 영문자로 시작하는 영문/숫자/밑줄이어야 합니다: %s", n.Output)
		}
	}

	switch {
	case starts == 0:
		add("", true, "시작 노드가 없습니다")
	case starts > 1:
		add("", true, "시작 노드는 하나여야 합니다")
	}

	for _, e := range g.Edges {
		if !seen[e.From] {
			add("", true, "존재하지 않는 노드에서 나가는 연결이 있습니다: %s", e.From)
		}
		if !seen[e.To] {
			add("", true, "존재하지 않는 노드로 가는 연결이 있습니다: %s", e.To)
		}
	}

	// 시작 노드에서 닿지 않는 노드는 오류가 아니라 경고다. 만들다 만 가지를
	// 남겨 두고 다른 부분을 손보는 것은 정상적인 작업 방식이다.
	reachable := g.reachable()
	for _, n := range g.Nodes {
		if !reachable[n.ID] && n.Type != TypeStart {
			add(n.ID, false, "시작 노드에서 연결되지 않아 실행되지 않습니다")
		}
	}

	names := map[string]bool{}
	for _, p := range g.Params {
		if !validVarName(p.Name) {
			add("", true, "파라미터 이름이 올바르지 않습니다: %s", p.Name)
		}
		if names[p.Name] {
			add("", true, "파라미터 이름이 중복됩니다: %s", p.Name)
		}
		names[p.Name] = true
	}
	return issues
}

func (g *Graph) reachable() map[string]bool {
	start := g.Start()
	seen := map[string]bool{}
	if start == nil {
		return seen
	}
	queue := []string{start.ID}
	seen[start.ID] = true
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		for _, e := range g.Edges {
			if e.From == id && !seen[e.To] {
				seen[e.To] = true
				queue = append(queue, e.To)
			}
		}
	}
	return seen
}

// HasFatal은 저장을 막아야 하는 문제가 있는지 반환한다.
func HasFatal(issues []ValidationIssue) bool {
	return slices.ContainsFunc(issues, func(i ValidationIssue) bool { return i.Fatal })
}

func validVarName(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r == '_':
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}
