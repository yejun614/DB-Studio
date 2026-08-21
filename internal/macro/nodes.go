package macro

import (
	"context"
	"fmt"
	"strings"
	"time"

	"dbstudio/internal/backup"
	"dbstudio/internal/dbx"
	"dbstudio/internal/model"
	"dbstudio/internal/schema"
	"dbstudio/internal/store"
)

// 내장 노드.
//
// 무엇을 내장하고 무엇을 Lua에 맡길지 정하는 기준은 하나다: **권한이 걸린 동작은
// 내장으로 만든다.** DB 접속, 셸 실행, 다른 매크로 호출은 실행자의 권한을 확인해야
// 하고, 그 확인이 Lua 스크립트 안에서 이루어지면 스크립트를 고쳐 우회할 수 있게
// 보일 수 있다(실제로는 호스트 함수가 막지만, 안전이 눈에 보이는 편이 낫다).
//
// 반대로 값을 만지고 모양을 바꾸는 일은 전부 Lua다. 그런 것을 노드로 만들기 시작하면
// "문자열 자르기 노드", "숫자 더하기 노드"가 끝없이 생기고, 결국 그림으로 프로그램을
// 쓰게 된다 — 그건 텍스트로 쓰는 편이 훨씬 낫다.

const (
	TypeStart      = "start"
	TypeLog        = "log"
	TypeSetVar     = "setvar"
	TypeBranch     = "branch"
	TypeForEach    = "foreach"
	TypeDelay      = "delay"
	TypeFail       = "fail"
	TypeSQLQuery   = "sql.query"
	TypeSQLExec    = "sql.exec"
	TypeDataQuery  = "data.query"
	TypeDataMutate = "data.mutate"
	TypeIntrospect = "schema.introspect"
	TypeCapture    = "schema.capture"
	TypeDrift      = "drift.check"
	TypeConnTest   = "connection.test"
	TypeBackup     = "backup.create"
	TypeShell      = "shell"
	TypeHTTP       = "http.request"
	TypeLua        = "lua"
	TypeMacroCall  = "macro.call"
	TypeCustom     = "custom"
)

// 포트 이름. 대부분의 노드는 out 하나만 쓴다.
const (
	PortOut   = "out"
	PortTrue  = "true"
	PortFalse = "false"
	PortBody  = "body"
	PortDone  = "done"
)

// FieldSpec은 노드 설정 입력칸 하나다. 화면이 이것을 보고 폼을 그린다.
type FieldSpec struct {
	Key         string   `json:"key"`
	Label       string   `json:"label"`
	Type        string   `json:"type"` // text | textarea | code | number | boolean | select | connection | macro
	Placeholder string   `json:"placeholder,omitempty"`
	Help        string   `json:"help,omitempty"`
	Options     []string `json:"options,omitempty"`
	Required    bool     `json:"required,omitempty"`
	// Language는 code 필드의 문법 강조 힌트다(sql | lua | shell | json).
	Language string `json:"language,omitempty"`
	// AllowAll은 connection 칸에 "전체 DB"(ConnectionAll)를 고를 수 있게 한다.
	// 그 값을 받아 처리할 줄 아는 노드에만 켠다 — 화면에 보여 놓고 실행이 거부되면
	// 사용자는 권한 문제라고 오해한다.
	AllowAll bool `json:"allowAll,omitempty"`
}

// NodeSpec은 팔레트 항목이자 노드 종류의 명세다.
type NodeSpec struct {
	Type        string      `json:"type"`
	Label       string      `json:"label"`
	Group       string      `json:"group"` // flow | db | studio | script
	Description string      `json:"description"`
	Ports       []string    `json:"ports"`
	Fields      []FieldSpec `json:"fields"`
	// NeedsCap는 이 노드가 대상 커넥션에 대해 요구하는 데이터 능력이다.
	// 화면이 실행 전에 "권한 부족" 경고를 그리는 근거이며, 실행 시에도 다시 확인한다.
	NeedsCap model.Capability `json:"needsCap,omitempty"`
	// NeedsLevel은 등급이 필요한 노드의 요구 등급이다.
	NeedsLevel model.Level `json:"needsLevel,omitempty"`
	// NeedsPerm은 전역 권한 요구다(셸, 매크로 호출).
	NeedsPerm model.Perm `json:"needsPerm,omitempty"`
	// NeedsShell이면 서버가 -allow-shell로 켜져 있어야 한다.
	NeedsShell bool `json:"needsShell,omitempty"`
}

var connectionField = FieldSpec{
	Key: "connection", Label: "커넥션", Type: "connection", Required: true,
}

// ConnectionAll은 "접근할 수 있는 모든 DB"를 뜻하는 커넥션 값이다.
//
// 이름을 쓸 수 없는 값(`*`)으로 둔 이유: 커넥션 이름과 겹치면 안 되고, 저장된
// 매크로를 눈으로 읽을 때도 특별한 값임이 드러나야 한다.
const ConnectionAll = "*"

// connectionAllField는 전체 DB를 고를 수 있는 커넥션 칸이다.
//
// 백업에만 이 칸을 주는 이유가 있다. "매일 새벽에 전부 백업"은 흔한 요구이고,
// DB를 하나 늘릴 때마다 매크로에 노드를 하나 더 붙이게 하면 언젠가 빠뜨린다 —
// 그리고 빠뜨린 사실은 복구가 필요한 날에야 드러난다.
//
// 반대로 SQL 실행이나 데이터 수정에 이 칸을 주지 않는 이유도 같은 무게다:
// 문장 하나가 모든 DB에 도는 것은 되돌릴 수 없는 쪽으로 너무 크게 움직인다.
var connectionAllField = FieldSpec{
	Key: "connection", Label: "커넥션", Type: "connection", Required: true,
	AllowAll: true,
	Help:     "전체 DB를 고르면 접근할 수 있는 모든 DB를 하나씩 백업합니다",
}

// Specs는 팔레트에 나오는 내장 노드 목록이다. 순서가 곧 표시 순서다.
func Specs() []NodeSpec { return specs }

var specs = []NodeSpec{
	{
		Type: TypeStart, Label: "시작", Group: "flow",
		Description: "매크로가 여기서 시작합니다. 실행 파라미터는 매크로 설정에서 정의합니다.",
		Ports:       []string{PortOut},
	},
	{
		Type: TypeLog, Label: "로그", Group: "flow",
		Description: "실행 로그에 한 줄을 남깁니다. ${변수} 를 쓸 수 있습니다.",
		Ports:       []string{PortOut},
		Fields: []FieldSpec{
			{Key: "message", Label: "메시지", Type: "textarea", Required: true,
				Placeholder: "조회 결과 ${rows.count}건"},
			{Key: "level", Label: "수준", Type: "select", Options: []string{"info", "warn", "error", "debug"}},
		},
	},
	{
		Type: TypeSetVar, Label: "변수 설정", Group: "flow",
		Description: "Lua 식을 계산해 변수에 담습니다. 예: rows.count > 0",
		Ports:       []string{PortOut},
		Fields: []FieldSpec{
			{Key: "name", Label: "변수 이름", Type: "text", Required: true},
			{Key: "expr", Label: "식", Type: "code", Language: "lua", Required: true,
				Placeholder: "#rows"},
		},
	},
	{
		Type: TypeBranch, Label: "분기", Group: "flow",
		Description: "Lua 식이 참이면 true, 거짓이면 false 포트로 갑니다.",
		Ports:       []string{PortTrue, PortFalse},
		Fields: []FieldSpec{
			{Key: "expr", Label: "조건식", Type: "code", Language: "lua", Required: true,
				Placeholder: "#rows > 0"},
		},
	},
	{
		Type: TypeForEach, Label: "반복", Group: "flow",
		Description: "목록 변수의 각 항목마다 body 포트를 실행합니다. 끝나면 done으로 갑니다.",
		Ports:       []string{PortBody, PortDone},
		Fields: []FieldSpec{
			{Key: "list", Label: "목록 변수", Type: "text", Required: true, Placeholder: "rows"},
			{Key: "item", Label: "항목 변수 이름", Type: "text", Placeholder: "item"},
			{Key: "limit", Label: "최대 반복 횟수", Type: "number", Placeholder: "1000",
				Help: "비워두면 1000회에서 멈춥니다"},
		},
	},
	{
		Type: TypeDelay, Label: "대기", Group: "flow",
		Description: "지정한 시간만큼 기다립니다.",
		Ports:       []string{PortOut},
		Fields: []FieldSpec{
			{Key: "seconds", Label: "초", Type: "number", Required: true, Placeholder: "5"},
		},
	},
	{
		Type: TypeFail, Label: "중단", Group: "flow",
		Description: "매크로를 실패로 끝냅니다.",
		Ports:       []string{},
		Fields: []FieldSpec{
			{Key: "message", Label: "사유", Type: "text", Required: true},
		},
	},

	{
		Type: TypeSQLQuery, Label: "SQL 조회", Group: "db",
		Description: "SELECT 문을 실행하고 결과 행을 변수에 담습니다.",
		Ports:       []string{PortOut}, NeedsCap: model.CapSQLRun,
		Fields: []FieldSpec{
			connectionField,
			{Key: "sql", Label: "SQL", Type: "code", Language: "sql", Required: true},
			{Key: "maxRows", Label: "최대 행 수", Type: "number", Placeholder: "500"},
		},
	},
	{
		Type: TypeSQLExec, Label: "SQL 실행", Group: "db",
		Description: "임의의 SQL을 실행합니다(여러 문장은 세미콜론으로 구분).",
		Ports:       []string{PortOut}, NeedsCap: model.CapSQLRun,
		Fields: []FieldSpec{
			connectionField,
			{Key: "sql", Label: "SQL", Type: "code", Language: "sql", Required: true},
		},
	},
	{
		Type: TypeDataQuery, Label: "데이터 조회", Group: "db",
		Description: "테이블/컬렉션에서 조건에 맞는 행을 읽습니다. SQL을 쓰지 않는 안전한 조회입니다.",
		Ports:       []string{PortOut}, NeedsCap: model.CapDataRead,
		Fields: []FieldSpec{
			connectionField,
			{Key: "table", Label: "테이블", Type: "text", Required: true},
			{Key: "namespace", Label: "스키마", Type: "text"},
			{Key: "search", Label: "검색어", Type: "text"},
			{Key: "orderBy", Label: "정렬 컬럼", Type: "text"},
			{Key: "desc", Label: "내림차순", Type: "boolean"},
			{Key: "limit", Label: "행 수", Type: "number", Placeholder: "100"},
		},
	},
	{
		Type: TypeDataMutate, Label: "데이터 수정", Group: "db",
		Description: "행 하나를 추가·수정·삭제합니다. 값은 JSON으로 씁니다.",
		Ports:       []string{PortOut}, NeedsCap: model.CapDataWrite,
		Fields: []FieldSpec{
			connectionField,
			{Key: "table", Label: "테이블", Type: "text", Required: true},
			{Key: "namespace", Label: "스키마", Type: "text"},
			{Key: "action", Label: "동작", Type: "select", Options: []string{"insert", "update", "delete"}, Required: true},
			{Key: "values", Label: "값 (JSON)", Type: "code", Language: "json",
				Placeholder: `{"name": "${item.name}"}`},
			{Key: "key", Label: "기본키 (JSON)", Type: "code", Language: "json",
				Placeholder: `{"id": "${item.id}"}`, Help: "수정·삭제에 필요합니다"},
		},
	},

	{
		Type: TypeIntrospect, Label: "스키마 읽기", Group: "studio",
		Description: "대상 DB의 구조를 읽어 변수에 담습니다.",
		Ports:       []string{PortOut}, NeedsLevel: model.LevelMonitor,
		Fields: []FieldSpec{connectionField},
	},
	{
		Type: TypeCapture, Label: "버전 확정", Group: "studio",
		Description: "현재 스키마를 새 버전으로 등록합니다. 구조가 같으면 만들지 않습니다.",
		Ports:       []string{PortOut}, NeedsLevel: model.LevelERD,
		Fields: []FieldSpec{
			connectionField,
			{Key: "note", Label: "메모", Type: "text"},
		},
	},
	{
		Type: TypeDrift, Label: "드리프트 확인", Group: "studio",
		Description: "앱 외부에서 스키마가 바뀌었는지 확인합니다.",
		Ports:       []string{PortOut}, NeedsLevel: model.LevelMonitor,
		Fields: []FieldSpec{connectionField},
	},
	{
		Type: TypeConnTest, Label: "연결 확인", Group: "studio",
		Description: "대상 DB에 접속되는지 확인하고 서버 정보를 담습니다.",
		Ports:       []string{PortOut}, NeedsLevel: model.LevelMonitor,
		Fields: []FieldSpec{connectionField},
	},
	{
		Type: TypeBackup, Label: "백업 만들기", Group: "studio",
		Description: "논리 덤프를 만듭니다. 마이그레이션 전에 백업을 남기는 매크로에 씁니다.",
		Ports:       []string{PortOut}, NeedsCap: model.CapDataRead,
		Fields: []FieldSpec{
			connectionAllField,
			{Key: "scope", Label: "범위", Type: "select",
				Options: []string{"full", "schema", "data"}, Required: true},
			{Key: "dropIfExists", Label: "DROP 문 포함", Type: "boolean",
				Help: "복구할 때 기존 테이블을 지우고 다시 만듭니다"},
			{Key: "note", Label: "메모", Type: "text"},
			{Key: "wait", Label: "완료까지 기다리기", Type: "boolean",
				Help: "켜면 백업이 끝난 뒤 다음 노드로 갑니다. 마이그레이션 앞에 둘 때 필요합니다"},
		},
	},
	{
		Type: TypeMacroCall, Label: "매크로 호출", Group: "studio",
		Description: "다른 매크로를 실행하고 끝날 때까지 기다립니다.",
		Ports:       []string{PortOut}, NeedsPerm: model.PermMacro,
		Fields: []FieldSpec{
			{Key: "macro", Label: "매크로", Type: "macro", Required: true},
			{Key: "params", Label: "파라미터 (JSON)", Type: "code", Language: "json"},
		},
	},

	{
		Type: TypeShell, Label: "셸 스크립트", Group: "script",
		Description: "bash 또는 powershell 스크립트를 실행합니다.",
		Ports:       []string{PortOut}, NeedsPerm: model.PermScriptRun, NeedsShell: true,
		Fields: []FieldSpec{
			{Key: "shell", Label: "셸", Type: "select", Options: []string{"bash", "powershell"}, Required: true},
			{Key: "script", Label: "스크립트", Type: "code", Language: "shell", Required: true},
			{Key: "dir", Label: "작업 디렉터리", Type: "text"},
			{Key: "failOnExit", Label: "0이 아닌 종료 코드를 실패로 처리", Type: "boolean"},
		},
	},
	{
		Type: TypeHTTP, Label: "HTTP 호출", Group: "script",
		Description: "외부 API를 호출합니다. 응답이 JSON이면 파싱된 값도 함께 담깁니다. " +
			"Lua 스크립트에서는 http.get/post/request 로도 부를 수 있습니다.",
		Ports: []string{PortOut}, NeedsPerm: model.PermHTTPCall,
		Fields: []FieldSpec{
			{Key: "method", Label: "메서드", Type: "select",
				Options: []string{"GET", "POST", "PUT", "PATCH", "DELETE"}},
			{Key: "url", Label: "주소", Type: "text", Required: true,
				Placeholder: "https://hooks.example.com/notify"},
			{Key: "headers", Label: "헤더 (JSON)", Type: "code", Language: "json",
				Placeholder: `{"Authorization": "Bearer ..."}`},
			{Key: "body", Label: "본문", Type: "code", Language: "json",
				Placeholder: `{"text": "${message}"}`},
			{Key: "failOnError", Label: "4xx·5xx를 실패로 처리", Type: "boolean"},
		},
	},
	{
		Type: TypeLua, Label: "Lua 스크립트", Group: "script",
		Description: "Lua로 임의의 처리를 합니다. vars 표로 변수를 읽고 쓰며, return 값이 이 노드의 결과입니다.",
		Ports:       []string{PortOut},
		Fields: []FieldSpec{
			{Key: "script", Label: "스크립트", Type: "code", Language: "lua", Required: true,
				Placeholder: "local total = 0\nfor _, row in ipairs(vars.rows) do\n  total = total + row.amount\nend\nreturn total"},
		},
	},
}

var specByType = func() map[string]NodeSpec {
	m := make(map[string]NodeSpec, len(specs))
	for _, s := range specs {
		m[s.Type] = s
	}
	return m
}()

// KnownTypes는 검증용 종류 집합이다.
func KnownTypes() map[string]bool {
	out := make(map[string]bool, len(specs)+1)
	for _, s := range specs {
		out[s.Type] = true
	}
	out[TypeCustom] = true
	return out
}

type execFunc func(r *runner, n *Node) (any, string, error)

var executors map[string]execFunc

// init에서 채우는 이유: 일부 노드(반복)가 walk를 다시 부르고, walk는 executors를
// 참조한다. 패키지 수준 변수의 초기화 순환을 컴파일러가 거부하므로 여기서 끊는다.
func init() {
	executors = map[string]execFunc{
		TypeStart:      execStart,
		TypeLog:        execLog,
		TypeSetVar:     execSetVar,
		TypeBranch:     execBranch,
		TypeForEach:    execForEach,
		TypeDelay:      execDelay,
		TypeFail:       execFail,
		TypeSQLQuery:   execSQLQuery,
		TypeSQLExec:    execSQLExec,
		TypeDataQuery:  execDataQuery,
		TypeDataMutate: execDataMutate,
		TypeIntrospect: execIntrospect,
		TypeCapture:    execCapture,
		TypeDrift:      execDrift,
		TypeConnTest:   execConnTest,
		TypeBackup:     execBackup,
		TypeShell:      execShell,
		TypeHTTP:       execHTTP,
		TypeLua:        execLua,
		TypeMacroCall:  execMacroCall,
	}
}

// ---------- 흐름 노드 ----------

func execStart(r *runner, n *Node) (any, string, error) {
	return nil, PortOut, nil
}

func execLog(r *runner, n *Node) (any, string, error) {
	level := r.rawStr(n, "level")
	if level == "" {
		level = "info"
	}
	r.log(level, n, r.str(n, "message"), nil)
	return nil, PortOut, nil
}

func execSetVar(r *runner, n *Node) (any, string, error) {
	name := r.rawStr(n, "name")
	if !validVarName(name) {
		return nil, "", fmt.Errorf("변수 이름이 올바르지 않습니다: %s", name)
	}
	value, err := r.evalLua(r.rawStr(n, "expr"))
	if err != nil {
		return nil, "", err
	}
	r.vars[name] = value
	r.log("debug", n, fmt.Sprintf("%s = %s", name, truncate(stringify(value), 200)), nil)
	return nil, PortOut, nil
}

func execBranch(r *runner, n *Node) (any, string, error) {
	value, err := r.evalLua(r.rawStr(n, "expr"))
	if err != nil {
		return nil, "", err
	}
	taken := truthy(value)
	r.log("debug", n, fmt.Sprintf("조건 결과: %v", taken), nil)
	if taken {
		return value, PortTrue, nil
	}
	return value, PortFalse, nil
}

// execForEach는 목록의 각 항목마다 body 갈래를 끝까지 실행한다.
//
// 반복을 그래프의 되돌아오는 간선(loop back)으로 표현하지 않은 이유: 되돌아오는
// 간선은 그림에서 흐름을 읽기 어렵게 만들고, 종료 조건이 어디에 있는지 보이지 않는다.
// 반복 노드가 몸통을 소유하면 "여기서 반복하고 끝나면 저기로 간다"가 한눈에 보인다.
func execForEach(r *runner, n *Node) (any, string, error) {
	listName := r.rawStr(n, "list")
	raw, ok := r.lookup(listName)
	if !ok {
		return nil, "", fmt.Errorf("목록 변수를 찾을 수 없습니다: %s", listName)
	}
	items, ok := raw.([]any)
	if !ok {
		return nil, "", fmt.Errorf("%s 은(는) 목록이 아닙니다", listName)
	}

	itemName := r.rawStr(n, "item")
	if itemName == "" {
		itemName = "item"
	}
	if !validVarName(itemName) {
		return nil, "", fmt.Errorf("항목 변수 이름이 올바르지 않습니다: %s", itemName)
	}
	limit := int(r.num(n, "limit", 1000))
	if limit <= 0 {
		limit = 1000
	}

	body := r.graph.Next(n.ID, PortBody)
	if len(body) == 0 {
		r.log("warn", n, "반복할 내용(body 연결)이 없습니다", nil)
		return len(items), PortDone, nil
	}

	done := 0
	for i, item := range items {
		if err := r.ctx.Err(); err != nil {
			return nil, "", err
		}
		if i >= limit {
			r.log("warn", n, fmt.Sprintf("반복 상한(%d회)에 도달해 남은 %d개를 건너뜁니다",
				limit, len(items)-i), nil)
			break
		}
		r.vars[itemName] = item
		r.vars[itemName+"_index"] = float64(i + 1)
		for _, id := range body {
			if err := r.walk(id); err != nil {
				return nil, "", fmt.Errorf("%d번째 항목에서 실패: %w", i+1, err)
			}
			if r.stopped {
				return done, PortDone, nil
			}
		}
		done++
	}
	r.log("info", n, fmt.Sprintf("%d개 항목을 처리했습니다", done), nil)
	return done, PortDone, nil
}

func execDelay(r *runner, n *Node) (any, string, error) {
	seconds := r.num(n, "seconds", 0)
	if seconds <= 0 {
		return nil, PortOut, nil
	}
	d := time.Duration(seconds * float64(time.Second))
	r.log("debug", n, fmt.Sprintf("%s 대기", d.Round(time.Millisecond)), nil)
	select {
	case <-time.After(d):
		return nil, PortOut, nil
	case <-r.ctx.Done():
		return nil, "", r.ctx.Err()
	}
}

func execFail(r *runner, n *Node) (any, string, error) {
	message := r.str(n, "message")
	if message == "" {
		message = "매크로가 중단되었습니다"
	}
	return nil, "", fmt.Errorf("%s", message)
}

// ---------- DB 노드 ----------

func execSQLQuery(r *runner, n *Node) (any, string, error) {
	conn, secret, err := r.connection(r.str(n, "connection"), model.CapSQLRun)
	if err != nil {
		return nil, "", err
	}
	sql := r.str(n, "sql")
	if strings.TrimSpace(sql) == "" {
		return nil, "", fmt.Errorf("SQL이 비어 있습니다")
	}
	maxRows := int(r.num(n, "maxRows", 500))

	results, err := dbx.DoRunStatements(r.ctx, dbx.Target{Conn: conn, Secret: secret},
		dbx.StatementRequest{Statement: sql, MaxRows: maxRows, ReadOnly: true})
	if err != nil {
		return nil, "", err
	}
	last := results[len(results)-1]
	if last.Error != "" {
		return nil, "", fmt.Errorf("%s", last.Error)
	}
	rows := rowsToMaps(last)
	r.log("info", n, fmt.Sprintf("%s: %d행 (%.1fms)", conn.Name, len(rows), last.ElapsedMs),
		map[string]any{"statement": last.Statement})
	return rows, PortOut, nil
}

func execSQLExec(r *runner, n *Node) (any, string, error) {
	conn, secret, err := r.connection(r.str(n, "connection"), model.CapSQLRun)
	if err != nil {
		return nil, "", err
	}
	sql := r.str(n, "sql")
	if strings.TrimSpace(sql) == "" {
		return nil, "", fmt.Errorf("SQL이 비어 있습니다")
	}

	results, err := dbx.DoRunStatements(r.ctx, dbx.Target{Conn: conn, Secret: secret},
		dbx.StatementRequest{Statement: sql, MaxRows: 100})
	if err != nil {
		return nil, "", err
	}
	total := int64(0)
	for _, res := range results {
		if res.Error != "" {
			return nil, "", fmt.Errorf("%s", res.Error)
		}
		if res.Affected > 0 {
			total += res.Affected
		}
		r.log("info", n, fmt.Sprintf("%s: %s (%.1fms)", conn.Name,
			truncate(res.Statement, 160), res.ElapsedMs), nil)
	}
	return map[string]any{"affected": float64(total), "statements": float64(len(results))}, PortOut, nil
}

func execDataQuery(r *runner, n *Node) (any, string, error) {
	conn, secret, err := r.connection(r.str(n, "connection"), model.CapDataRead)
	if err != nil {
		return nil, "", err
	}
	page, err := dbx.DoQueryRows(r.ctx, dbx.Target{Conn: conn, Secret: secret}, dbx.RowQuery{
		Table:   dbx.TableRef{Namespace: r.str(n, "namespace"), Name: r.str(n, "table")},
		Limit:   int(r.num(n, "limit", 100)),
		OrderBy: r.str(n, "orderBy"),
		Desc:    r.flag(n, "desc"),
		Search:  r.str(n, "search"),
		Full:    true,
	})
	if err != nil {
		return nil, "", err
	}
	rows := pageToMaps(page)
	r.log("info", n, fmt.Sprintf("%s.%s: %d행", conn.Name, r.str(n, "table"), len(rows)), nil)
	return rows, PortOut, nil
}

func execDataMutate(r *runner, n *Node) (any, string, error) {
	conn, secret, err := r.connection(r.str(n, "connection"), model.CapDataWrite)
	if err != nil {
		return nil, "", err
	}
	action := r.rawStr(n, "action")
	values, err := parseJSONObject(r.str(n, "values"))
	if err != nil {
		return nil, "", fmt.Errorf("값 JSON: %w", err)
	}
	key, err := parseJSONObject(r.str(n, "key"))
	if err != nil {
		return nil, "", fmt.Errorf("기본키 JSON: %w", err)
	}

	res, err := dbx.DoMutateRow(r.ctx, dbx.Target{Conn: conn, Secret: secret}, dbx.RowMutation{
		Table:  dbx.TableRef{Namespace: r.str(n, "namespace"), Name: r.str(n, "table")},
		Action: action, Values: values, Key: key,
	})
	if err != nil {
		return nil, "", err
	}
	r.log("info", n, fmt.Sprintf("%s: %s %d건 — %s", conn.Name, action, res.Affected,
		truncate(res.Statement, 200)), nil)
	return map[string]any{"affected": float64(res.Affected)}, PortOut, nil
}

// ---------- DB Studio 조작 노드 ----------

func execIntrospect(r *runner, n *Node) (any, string, error) {
	conn, secret, err := r.levelConnection(r.str(n, "connection"), model.LevelMonitor)
	if err != nil {
		return nil, "", err
	}
	current, err := r.introspect(conn, secret)
	if err != nil {
		return nil, "", err
	}
	stats := current.Stats()
	r.log("info", n, fmt.Sprintf("%s: 테이블 %d개, 컬럼 %d개", conn.Name, stats.Tables, stats.Columns), nil)
	return map[string]any{
		"tables":  float64(stats.Tables),
		"columns": float64(stats.Columns),
		"views":   float64(stats.Views),
		"names":   tableNames(current),
	}, PortOut, nil
}

func execCapture(r *runner, n *Node) (any, string, error) {
	conn, secret, err := r.levelConnection(r.str(n, "connection"), model.LevelERD)
	if err != nil {
		return nil, "", err
	}
	current, err := r.introspect(conn, secret)
	if err != nil {
		return nil, "", err
	}

	prev, err := r.engine.st.LatestSchemaVersion(r.ctx, conn.ID, true)
	if err != nil {
		return nil, "", err
	}
	source := store.VersionSourceImport
	summary := []string{}
	if prev != nil {
		source = store.VersionSourceExternal
		if prev.Schema != nil {
			for _, ch := range schema.Diff(prev.Schema, current).Changes {
				summary = append(summary, ch.Summary)
			}
			if len(summary) == 0 {
				r.log("info", n, fmt.Sprintf("%s: 이전 버전과 구조가 같아 새 버전을 만들지 않았습니다", conn.Name), nil)
				return map[string]any{"created": false, "version": float64(prev.VersionNo)}, PortOut, nil
			}
		}
	}

	version, created, err := r.engine.st.SaveSchemaVersion(r.ctx, store.SaveVersionParams{
		ConnectionID: conn.ID, Schema: current, Source: source,
		Note: r.str(n, "note"), ChangeSummary: summary,
		AuthorID: r.actor.ID, AuthorName: r.actor.Username,
	})
	if err != nil {
		return nil, "", err
	}
	r.log("info", n, fmt.Sprintf("%s: 버전 v%d %s", conn.Name, version.VersionNo,
		map[bool]string{true: "등록", false: "재사용"}[created]),
		map[string]any{"changes": summary})
	return map[string]any{
		"created": created, "version": float64(version.VersionNo),
		"changes": toAnySlice(summary),
	}, PortOut, nil
}

func execDrift(r *runner, n *Node) (any, string, error) {
	conn, _, err := r.levelConnection(r.str(n, "connection"), model.LevelMonitor)
	if err != nil {
		return nil, "", err
	}
	if r.engine.drift == nil {
		return nil, "", fmt.Errorf("이 서버에는 드리프트 확인 기능이 연결되어 있지 않습니다")
	}
	snapshot, changed, err := r.engine.drift.CheckDriftByID(r.ctx, conn.ID)
	if err != nil {
		return nil, "", err
	}
	result := map[string]any{"changed": changed}
	if snapshot != nil {
		result["fingerprint"] = snapshot.Fingerprint
	}
	if changed {
		r.log("warn", n, fmt.Sprintf("%s: 외부에서 스키마가 변경되었습니다", conn.Name), nil)
	} else {
		r.log("info", n, fmt.Sprintf("%s: 변경 없음", conn.Name), nil)
	}
	return result, PortOut, nil
}

func execConnTest(r *runner, n *Node) (any, string, error) {
	conn, secret, err := r.levelConnection(r.str(n, "connection"), model.LevelMonitor)
	if err != nil {
		return nil, "", err
	}
	adapter, err := dbx.Get(conn.Kind)
	if err != nil {
		return nil, "", err
	}
	ctx, cancel := context.WithTimeout(r.ctx, 30*time.Second)
	defer cancel()

	info, err := adapter.Ping(ctx, dbx.Target{Conn: conn, Secret: secret})
	if err != nil {
		return nil, "", fmt.Errorf("%s 접속 실패: %w", conn.Name, err)
	}
	r.log("info", n, fmt.Sprintf("%s: 접속 성공 (%s, %.1fms)", conn.Name, info.Version, info.LatencyM), nil)
	return map[string]any{"version": info.Version, "latencyMs": info.LatencyM}, PortOut, nil
}

// execBackup은 논리 덤프를 만든다.
//
// "마이그레이션 전에 백업" 같은 순서를 매크로로 표현할 수 있게 하는 것이 목적이므로
// 기다리기 옵션이 중요하다. 기다리지 않으면 백업이 끝나기 전에 다음 노드가 DB를
// 바꾸기 시작하고, 그러면 백업이 어느 시점의 것인지 알 수 없게 된다.
func execBackup(r *runner, n *Node) (any, string, error) {
	if r.engine.backups == nil {
		return nil, "", fmt.Errorf("이 서버에는 백업 기능이 연결되어 있지 않습니다")
	}
	if strings.TrimSpace(r.str(n, "connection")) == ConnectionAll {
		return execBackupAll(r, n)
	}
	conn, secret, err := r.connection(r.str(n, "connection"), model.CapDataRead)
	if err != nil {
		return nil, "", err
	}
	scope := r.rawStr(n, "scope")
	if scope == "" {
		scope = backup.ScopeFull
	}

	id, err := r.engine.backups.StartBackup(r.ctx, backup.StartBackupParams{
		Target: backup.Target{Conn: conn, Secret: secret},
		Options: backup.Options{
			Scope: scope, DropIfExists: r.flag(n, "dropIfExists"), Note: r.str(n, "note"),
		},
		Actor: r.actor, Trigger: "macro",
	})
	if err != nil {
		return nil, "", err
	}
	r.log("info", n, fmt.Sprintf("%s 백업 시작 (범위 %s)", conn.Name, scope),
		map[string]any{"backupId": id})

	if !r.flag(n, "wait") {
		return map[string]any{"backupId": id, "waited": false}, PortOut, nil
	}

	b, err := r.engine.backups.WaitFor(r.ctx, id)
	if err != nil {
		return nil, "", err
	}
	if b.Status != "success" {
		return nil, "", fmt.Errorf("백업이 %s했습니다: %s", statusLabel(b.Status), b.Error)
	}
	r.log("info", n, fmt.Sprintf("%s 백업 완료 — 테이블 %d개, %d행, %s",
		conn.Name, b.TableCount, b.RowCount, formatBytes(b.SizeBytes)),
		map[string]any{"backupId": id})
	return map[string]any{
		"backupId": id, "waited": true, "rows": float64(b.RowCount),
		"tables": float64(b.TableCount), "bytes": float64(b.SizeBytes),
	}, PortOut, nil
}

// execBackupAll은 실행자가 접근할 수 있는 모든 DB를 하나씩 백업한다.
//
// 세 가지를 정해 두었다.
//
//  1. **권한은 커넥션마다 확인한다.** "전체"라고 해서 판정을 건너뛰면 이 노드가
//     권한 상승 통로가 된다. 능력이 없는 DB는 조용히 건너뛰되 기록은 남긴다 —
//     실행자가 볼 수 없는 DB가 있다는 사실 자체는 그 사람이 알 필요가 없다.
//  2. **하나가 실패해도 나머지는 진행한다.** 여기서 멈추면 알파벳 순으로 뒤에 있는
//     DB들이 통째로 백업되지 않는다. 실패는 모아서 마지막에 알린다.
//  3. **차례로 돈다.** 한꺼번에 시작하면 덤프 여러 개가 동시에 디스크와 대상 DB를
//     때린다. 백업은 급한 일이 아니고, 새벽에 도는 것이 보통이다.
func execBackupAll(r *runner, n *Node) (any, string, error) {
	conns, err := r.engine.st.ListConnections(r.ctx)
	if err != nil {
		return nil, "", err
	}
	scope := r.rawStr(n, "scope")
	if scope == "" {
		scope = backup.ScopeFull
	}
	wait := r.flag(n, "wait")

	started := []any{}
	skipped := 0
	failures := []string{}

	for _, c := range conns {
		if !c.Enabled {
			continue
		}
		conn, secret, cerr := r.connection(c.ID, model.CapDataRead)
		if cerr != nil {
			skipped++
			continue
		}
		id, serr := r.engine.backups.StartBackup(r.ctx, backup.StartBackupParams{
			Target: backup.Target{Conn: conn, Secret: secret},
			Options: backup.Options{
				Scope: scope, DropIfExists: r.flag(n, "dropIfExists"), Note: r.str(n, "note"),
			},
			Actor: r.actor, Trigger: "macro",
		})
		if serr != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", conn.Name, serr))
			continue
		}
		r.log("info", n, fmt.Sprintf("%s 백업 시작 (범위 %s)", conn.Name, scope),
			map[string]any{"backupId": id})

		entry := map[string]any{"connection": conn.Name, "backupId": id}
		if wait {
			b, werr := r.engine.backups.WaitFor(r.ctx, id)
			if werr != nil {
				failures = append(failures, fmt.Sprintf("%s: %v", conn.Name, werr))
				continue
			}
			if b.Status != "success" {
				failures = append(failures, fmt.Sprintf("%s: %s (%s)",
					conn.Name, statusLabel(b.Status), b.Error))
				continue
			}
			entry["rows"] = float64(b.RowCount)
			entry["tables"] = float64(b.TableCount)
			entry["bytes"] = float64(b.SizeBytes)
			r.log("info", n, fmt.Sprintf("%s 백업 완료 — 테이블 %d개, %d행, %s",
				conn.Name, b.TableCount, b.RowCount, formatBytes(b.SizeBytes)),
				map[string]any{"backupId": id})
		}
		started = append(started, entry)
	}

	if skipped > 0 {
		r.log("info", n, fmt.Sprintf("권한이 없어 건너뛴 DB %d개", skipped), nil)
	}
	if len(started) == 0 && len(failures) == 0 {
		return nil, "", fmt.Errorf("백업할 수 있는 DB가 없습니다 (접근 권한을 확인하세요)")
	}
	if len(failures) > 0 {
		// 일부라도 실패하면 노드는 실패다. 성공한 것이 있다는 사실은 로그와
		// 백업 목록에 남으므로, 여기서 "부분 성공"을 성공으로 보고할 이유가 없다.
		return nil, "", fmt.Errorf("DB %d개 백업 실패: %s",
			len(failures), strings.Join(failures, "; "))
	}
	return map[string]any{
		"backups": started, "count": float64(len(started)), "waited": wait,
	}, PortOut, nil
}

func formatBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1fGB", float64(n)/float64(int64(1)<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1fMB", float64(n)/float64(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1fKB", float64(n)/float64(1<<10))
	default:
		return fmt.Sprintf("%dB", n)
	}
}

func execMacroCall(r *runner, n *Node) (any, string, error) {
	if !r.actor.HasPerm(model.PermMacro) {
		return nil, "", fmt.Errorf("매크로 실행 권한이 없습니다")
	}
	targetID := r.str(n, "macro")
	if targetID == r.macro.ID {
		return nil, "", fmt.Errorf("자기 자신을 호출할 수 없습니다")
	}
	params, err := parseJSONObject(r.str(n, "params"))
	if err != nil {
		return nil, "", fmt.Errorf("파라미터 JSON: %w", err)
	}

	run, err := r.engine.RunNested(r.ctx, RunRequest{
		MacroID: targetID, Actor: r.actor, ActorIP: r.actorIP,
		Params: params, Trigger: "macro", ParentRunID: r.runID, depth: r.depth + 1,
	})
	if err != nil {
		return nil, "", err
	}
	r.log("info", n, fmt.Sprintf("%s 실행 %s (%dms)", run.MacroName,
		statusLabel(run.Status), run.DurationMs),
		map[string]any{"runId": run.ID, "status": run.Status})
	if run.Status != "success" {
		return nil, "", fmt.Errorf("호출한 매크로 %s 이(가) %s했습니다: %s",
			run.MacroName, statusLabel(run.Status), run.Error)
	}
	return map[string]any{
		"runId": run.ID, "status": run.Status, "durationMs": float64(run.DurationMs),
	}, PortOut, nil
}

// ---------- 헬퍼 ----------

func (r *runner) introspect(conn *model.Connection, secret *model.Secret) (*schema.Schema, error) {
	adapter, err := dbx.Get(conn.Kind)
	if err != nil {
		return nil, err
	}
	if !adapter.Capabilities().Introspect {
		return nil, fmt.Errorf("%s 는 스키마 읽기를 지원하지 않습니다", conn.Kind)
	}
	ctx, cancel := context.WithTimeout(r.ctx, 90*time.Second)
	defer cancel()
	return adapter.Introspect(ctx, dbx.Target{Conn: conn, Secret: secret})
}

// rowsToMaps는 문장 실행 결과를 Lua/템플릿에서 쓰기 좋은 표 목록으로 바꾼다.
func rowsToMaps(res dbx.StatementResult) []any {
	out := make([]any, 0, len(res.Rows))
	for _, row := range res.Rows {
		m := make(map[string]any, len(res.Columns))
		for i, col := range res.Columns {
			if i < len(row) {
				m[col.Name] = row[i]
			}
		}
		out = append(out, m)
	}
	return out
}

func pageToMaps(page *dbx.RowPage) []any {
	out := make([]any, 0, len(page.Rows))
	for _, row := range page.Rows {
		m := make(map[string]any, len(page.Columns))
		for i, col := range page.Columns {
			if i < len(row) {
				m[col.Name] = row[i]
			}
		}
		out = append(out, m)
	}
	return out
}

func tableNames(s *schema.Schema) []any {
	out := make([]any, 0, len(s.Tables))
	for _, t := range s.Tables {
		out = append(out, t.Display())
	}
	return out
}

func toAnySlice(in []string) []any {
	out := make([]any, len(in))
	for i, s := range in {
		out[i] = s
	}
	return out
}

func truthy(v any) bool {
	switch val := v.(type) {
	case nil:
		return false
	case bool:
		return val
	case float64:
		return val != 0
	case string:
		return val != "" && val != "false" && val != "0"
	case []any:
		return len(val) > 0
	case map[string]any:
		return len(val) > 0
	default:
		return true
	}
}

func truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "…"
}
