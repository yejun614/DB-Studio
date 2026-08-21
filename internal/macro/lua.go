package macro

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	lua "github.com/yuin/gopher-lua"

	"dbstudio/internal/dbx"
	"dbstudio/internal/model"
)

// Lua 샌드박스.
//
// 샌드박스의 규칙은 **기본은 아무것도 없고, 필요한 것만 준다**이다. 표준 라이브러리를
// 전부 열고 위험한 것을 지우는 방식(블랙리스트)은 언젠가 하나를 빠뜨리고, 빠뜨렸다는
// 사실은 사고가 난 뒤에 알게 된다.
//
// 그래서 열어 주는 것은 base·table·string·math 네 개뿐이다. io·os·package·debug는
// 열지 않는다. 파일을 읽거나 명령을 실행하는 방법은 오직 호스트 함수(sh.run)뿐이고,
// 그 함수는 실행자의 권한과 서버 설정을 확인한다.
//
// 스크립트가 dofile/loadfile/load로 새 코드를 불러오는 길도 막는다 — 그것이 열려 있으면
// 위 목록이 의미를 잃는다.

// newState는 샌드박스된 Lua 상태에 호스트 API를 심어 돌려준다.
func (r *runner) newState(ctx context.Context) *lua.LState {
	L := newSandbox(ctx)
	r.installAPI(L)
	return L
}

// newSandbox는 실행 문맥 없이 샌드박스만 만든다.
// 호스트 API와 분리해 둔 이유는 "무엇이 열려 있는가"를 그 자체로 시험할 수 있어야
// 하기 때문이다 — 샌드박스가 새는지 확인하는 데 스토어나 커넥션이 필요해서는 안 된다.
func newSandbox(ctx context.Context) *lua.LState {
	L := lua.NewState(lua.Options{SkipOpenLibs: true})
	// SkipOpenLibs로 아무것도 열지 않은 뒤 필요한 것만 연다.
	for _, lib := range []struct {
		name string
		open lua.LGFunction
	}{
		{lua.BaseLibName, lua.OpenBase},
		{lua.TabLibName, lua.OpenTable},
		{lua.StringLibName, lua.OpenString},
		{lua.MathLibName, lua.OpenMath},
	} {
		L.Push(L.NewFunction(lib.open))
		L.Push(lua.LString(lib.name))
		L.Call(1, 0)
	}

	// base 라이브러리에 딸려 온 코드 로딩 함수를 없앤다.
	//
	// require가 여기 있는 이유: package 라이브러리를 열지 않았으므로 호출해도
	// 실패하지만, 남겨 두면 "이건 되겠지" 하고 시도하게 되고 그 실패는
	// 샌드박스 정책이 아니라 버그처럼 보인다. 없는 것이 정직하다.
	for _, name := range []string{
		"dofile", "loadfile", "load", "loadstring", "require", "collectgarbage",
	} {
		L.SetGlobal(name, lua.LNil)
	}

	// 컨텍스트를 붙이면 취소·타임아웃 시 VM이 멈춘다. 무한 루프에 대한 방어는 이것이다.
	L.SetContext(ctx)
	return L
}

// installAPI는 호스트 함수를 전역에 심는다.
func (r *runner) installAPI(L *lua.LState) {
	// vars: 실행 문맥의 변수 가방.
	L.SetGlobal("vars", goToLua(L, r.vars))

	// log
	logTable := L.NewTable()
	for _, level := range []string{"debug", "info", "warn", "error"} {
		lv := level
		L.SetField(logTable, lv, L.NewFunction(func(L *lua.LState) int {
			r.log(lv, r.currentNode, luaJoin(L), nil)
			return 0
		}))
	}
	L.SetGlobal("log", logTable)

	// json
	jsonTable := L.NewTable()
	L.SetField(jsonTable, "encode", L.NewFunction(func(L *lua.LState) int {
		b, err := json.Marshal(luaToGo(L.CheckAny(1)))
		if err != nil {
			L.RaiseError("json.encode: %v", err)
			return 0
		}
		L.Push(lua.LString(b))
		return 1
	}))
	L.SetField(jsonTable, "decode", L.NewFunction(func(L *lua.LState) int {
		var v any
		if err := json.Unmarshal([]byte(L.CheckString(1)), &v); err != nil {
			L.RaiseError("json.decode: %v", err)
			return 0
		}
		L.Push(goToLua(L, normalizeJSON(v)))
		return 1
	}))
	L.SetGlobal("json", jsonTable)

	// db
	dbTable := L.NewTable()
	L.SetField(dbTable, "query", L.NewFunction(r.luaDBQuery))
	L.SetField(dbTable, "exec", L.NewFunction(r.luaDBExec))
	L.SetField(dbTable, "rows", L.NewFunction(r.luaDBRows))
	L.SetField(dbTable, "mutate", L.NewFunction(r.luaDBMutate))
	L.SetGlobal("db", dbTable)

	// sh
	shTable := L.NewTable()
	L.SetField(shTable, "run", L.NewFunction(r.luaShellRun))
	L.SetGlobal("sh", shTable)

	// http
	httpTable := L.NewTable()
	L.SetField(httpTable, "request", L.NewFunction(r.luaHTTPRequest))
	L.SetField(httpTable, "get", L.NewFunction(r.luaHTTPGet))
	L.SetField(httpTable, "post", L.NewFunction(r.luaHTTPPost))
	L.SetGlobal("http", httpTable)

	// macro
	macroTable := L.NewTable()
	L.SetField(macroTable, "run", L.NewFunction(r.luaMacroRun))
	L.SetGlobal("macro", macroTable)

	// fail: 스크립트가 명시적으로 매크로를 실패시킨다.
	L.SetGlobal("fail", L.NewFunction(func(L *lua.LState) int {
		L.RaiseError("%s", luaJoin(L))
		return 0
	}))
}

// luaJoin은 가변 인자를 공백으로 이은 문자열로 만든다(print와 같은 사용감).
func luaJoin(L *lua.LState) string {
	parts := make([]string, 0, L.GetTop())
	for i := 1; i <= L.GetTop(); i++ {
		parts = append(parts, L.ToStringMeta(L.Get(i)).String())
	}
	return strings.Join(parts, " ")
}

// ---------- 호스트 함수 ----------

func (r *runner) luaDBQuery(L *lua.LState) int {
	connID := L.CheckString(1)
	sql := L.CheckString(2)
	maxRows := L.OptInt(3, 500)

	conn, secret, err := r.connection(connID, model.CapSQLRun)
	if err != nil {
		L.RaiseError("db.query: %v", err)
		return 0
	}
	results, err := dbx.DoRunStatements(r.ctx, dbx.Target{Conn: conn, Secret: secret},
		dbx.StatementRequest{Statement: sql, MaxRows: maxRows, ReadOnly: true})
	if err != nil {
		L.RaiseError("db.query: %v", err)
		return 0
	}
	last := results[len(results)-1]
	if last.Error != "" {
		L.RaiseError("db.query: %s", last.Error)
		return 0
	}
	L.Push(goToLua(L, rowsToMaps(last)))
	return 1
}

func (r *runner) luaDBExec(L *lua.LState) int {
	connID := L.CheckString(1)
	sql := L.CheckString(2)

	conn, secret, err := r.connection(connID, model.CapSQLRun)
	if err != nil {
		L.RaiseError("db.exec: %v", err)
		return 0
	}
	results, err := dbx.DoRunStatements(r.ctx, dbx.Target{Conn: conn, Secret: secret},
		dbx.StatementRequest{Statement: sql, MaxRows: 1})
	if err != nil {
		L.RaiseError("db.exec: %v", err)
		return 0
	}
	total := int64(0)
	for _, res := range results {
		if res.Error != "" {
			L.RaiseError("db.exec: %s", res.Error)
			return 0
		}
		if res.Affected > 0 {
			total += res.Affected
		}
	}
	L.Push(goToLua(L, map[string]any{"affected": float64(total)}))
	return 1
}

func (r *runner) luaDBRows(L *lua.LState) int {
	connID := L.CheckString(1)
	table := L.CheckString(2)
	opts := map[string]any{}
	if L.GetTop() >= 3 {
		if t, ok := luaToGo(L.Get(3)).(map[string]any); ok {
			opts = t
		}
	}

	conn, secret, err := r.connection(connID, model.CapDataRead)
	if err != nil {
		L.RaiseError("db.rows: %v", err)
		return 0
	}
	limit := 100
	if f, err := toFloat(opts["limit"]); err == nil && f > 0 {
		limit = int(f)
	}
	page, err := dbx.DoQueryRows(r.ctx, dbx.Target{Conn: conn, Secret: secret}, dbx.RowQuery{
		Table:   dbx.TableRef{Namespace: stringOr(opts["namespace"]), Name: table},
		Limit:   limit,
		Offset:  int(orZero(opts["offset"])),
		OrderBy: stringOr(opts["orderBy"]),
		Desc:    toBool(opts["desc"]),
		Search:  stringOr(opts["search"]),
		Full:    true,
	})
	if err != nil {
		L.RaiseError("db.rows: %v", err)
		return 0
	}
	L.Push(goToLua(L, pageToMaps(page)))
	return 1
}

func (r *runner) luaDBMutate(L *lua.LState) int {
	connID := L.CheckString(1)
	table := L.CheckString(2)
	action := L.CheckString(3)
	values, _ := luaToGo(L.Get(4)).(map[string]any)
	key, _ := luaToGo(L.Get(5)).(map[string]any)

	conn, secret, err := r.connection(connID, model.CapDataWrite)
	if err != nil {
		L.RaiseError("db.mutate: %v", err)
		return 0
	}
	res, err := dbx.DoMutateRow(r.ctx, dbx.Target{Conn: conn, Secret: secret}, dbx.RowMutation{
		Table:  dbx.TableRef{Name: table},
		Action: action, Values: values, Key: key,
	})
	if err != nil {
		L.RaiseError("db.mutate: %v", err)
		return 0
	}
	r.log("info", r.currentNode, fmt.Sprintf("%s: %s %d건", conn.Name, action, res.Affected), nil)
	L.Push(goToLua(L, map[string]any{"affected": float64(res.Affected)}))
	return 1
}

func (r *runner) luaShellRun(L *lua.LState) int {
	shell := L.CheckString(1)
	script := L.CheckString(2)
	opts := map[string]any{}
	if L.GetTop() >= 3 {
		if t, ok := luaToGo(L.Get(3)).(map[string]any); ok {
			opts = t
		}
	}

	res, err := r.runShell(shell, script, stringOr(opts["dir"]))
	if err != nil {
		L.RaiseError("sh.run: %v", err)
		return 0
	}
	L.Push(goToLua(L, map[string]any{
		"code": float64(res.Code), "stdout": res.Stdout, "stderr": res.Stderr,
	}))
	return 1
}

// luaHTTPRequest는 http.request{...} 형태의 호출을 처리한다.
//
// 표 하나로 받는 이유: 인자가 다섯 개(메서드·주소·헤더·본문·기대 상태)이고 대부분
// 생략되는데, 위치 인자로 받으면 `http.request(nil, url, nil, body)` 같은 호출이 된다.
func (r *runner) luaHTTPRequest(L *lua.LState) int {
	opts, ok := luaToGo(L.CheckAny(1)).(map[string]any)
	if !ok {
		L.RaiseError("http.request: 표(table)를 넘기세요 — {url=..., method=...}")
		return 0
	}
	return r.pushHTTPResult(L, HTTPRequest{
		Method:  stringOr(opts["method"]),
		URL:     stringOr(opts["url"]),
		Headers: headerMap(opts["headers"]),
		Body:    httpBody(opts["body"]),
	})
}

func (r *runner) luaHTTPGet(L *lua.LState) int {
	req := HTTPRequest{Method: "GET", URL: L.CheckString(1)}
	if L.GetTop() >= 2 {
		if opts, ok := luaToGo(L.Get(2)).(map[string]any); ok {
			req.Headers = headerMap(opts["headers"])
		}
	}
	return r.pushHTTPResult(L, req)
}

func (r *runner) luaHTTPPost(L *lua.LState) int {
	req := HTTPRequest{Method: "POST", URL: L.CheckString(1)}
	if L.GetTop() >= 2 {
		req.Body = httpBody(luaToGo(L.Get(2)))
	}
	if L.GetTop() >= 3 {
		if opts, ok := luaToGo(L.Get(3)).(map[string]any); ok {
			req.Headers = headerMap(opts["headers"])
		}
	}
	return r.pushHTTPResult(L, req)
}

func (r *runner) pushHTTPResult(L *lua.LState, req HTTPRequest) int {
	res, err := r.callHTTP(req)
	if err != nil {
		L.RaiseError("http: %v", err)
		return 0
	}
	// 호출 사실을 실행 로그에 남긴다. 데이터가 밖으로 나가는 동작이므로
	// 스크립트 안에서 일어났더라도 기록이 있어야 한다.
	level := "info"
	if res.Status >= 400 {
		level = "warn"
	}
	method := req.Method
	if method == "" {
		method = "GET"
	}
	r.log(level, r.currentNode, fmt.Sprintf("%s %s → %d (%.0fms)",
		method, req.URL, res.Status, res.ElapsedMs),
		map[string]any{"bytes": len(res.Body), "truncated": res.Truncated})

	L.Push(goToLua(L, httpResultValue(res)))
	return 1
}

// headerMap은 Lua 표에서 헤더를 뽑는다.
func headerMap(v any) map[string]string {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, val := range m {
		out[k] = stringify(val)
	}
	return out
}

// httpBody는 본문 인자를 문자열로 만든다.
// 표를 넘기면 JSON으로 직렬화한다 — API 호출의 대부분이 그 형태이고,
// 매번 json.encode를 부르게 하는 것은 군더더기다.
func httpBody(v any) string {
	switch val := v.(type) {
	case nil:
		return ""
	case string:
		return val
	case map[string]any, []any:
		if b, err := json.Marshal(val); err == nil {
			return string(b)
		}
		return stringify(val)
	default:
		return stringify(val)
	}
}

func (r *runner) luaMacroRun(L *lua.LState) int {
	macroID := L.CheckString(1)
	params := map[string]any{}
	if L.GetTop() >= 2 {
		if t, ok := luaToGo(L.Get(2)).(map[string]any); ok {
			params = t
		}
	}
	if !r.actor.HasPerm(model.PermMacro) {
		L.RaiseError("macro.run: 매크로 실행 권한이 없습니다")
		return 0
	}
	if macroID == r.macro.ID {
		L.RaiseError("macro.run: 자기 자신을 호출할 수 없습니다")
		return 0
	}
	run, err := r.engine.RunNested(r.ctx, RunRequest{
		MacroID: macroID, Actor: r.actor, ActorIP: r.actorIP, Params: params,
		Trigger: "macro", ParentRunID: r.runID, depth: r.depth + 1,
	})
	if err != nil {
		L.RaiseError("macro.run: %v", err)
		return 0
	}
	L.Push(goToLua(L, map[string]any{
		"runId": run.ID, "status": run.Status, "error": run.Error,
		"durationMs": float64(run.DurationMs),
	}))
	return 1
}

// ---------- 노드 실행 ----------

// execLua는 Lua 스크립트 노드를 실행한다.
func execLua(r *runner, n *Node) (any, string, error) {
	script := r.rawStr(n, "script")
	if strings.TrimSpace(script) == "" {
		return nil, "", fmt.Errorf("스크립트가 비어 있습니다")
	}
	value, port, err := r.runLua(n, script, nil)
	if err != nil {
		return nil, "", err
	}
	return value, port, nil
}

// runCustom은 사용자가 등록한 노드를 실행한다.
//
// 등록된 노드도 결국 Lua다. 다른 점은 설정 입력칸(fields)을 정의할 수 있고,
// 그 값이 스크립트에 params 표로 전달된다는 것뿐이다. 이 설계 덕분에 사용자 노드는
// 내장 노드와 같은 권한 규칙 아래 있다 — 호스트 함수를 거치지 않고는 아무것도 못 한다.
func (r *runner) runCustom(n *Node) (string, error) {
	def, ok := r.nodeDefs[n.NodeRef]
	if !ok {
		return "", fmt.Errorf("등록된 노드를 찾을 수 없습니다: %s", n.NodeRef)
	}
	// 설정값도 ${변수} 치환을 거친다. 내장 노드와 다르게 동작하면 사용자가
	// 두 가지 규칙을 기억해야 한다.
	params := map[string]any{}
	for k, v := range n.Params {
		if s, ok := v.(string); ok {
			params[k] = r.resolve(s)
			continue
		}
		params[k] = v
	}

	value, port, err := r.runLua(n, def.Script, params)
	if err != nil {
		return "", fmt.Errorf("%s: %w", def.Name, err)
	}
	if value != nil {
		r.vars[r.outputName(n)] = value
	}
	if port == "" {
		port = PortOut
	}
	return port, nil
}

// runLua는 스크립트를 실행하고 (반환값, 포트)를 돌려준다.
//
// 스크립트는 두 값을 반환할 수 있다: `return result, "포트이름"`.
// 포트를 생략하면 out이다. 사용자 노드가 분기를 만들 수 있어야 내장 노드로 표현할 수
// 없는 판단(예: 외부 시스템 응답에 따른 분기)을 담을 수 있다.
func (r *runner) runLua(n *Node, script string, params map[string]any) (any, string, error) {
	timeout := r.engine.cfg.LuaTimeout
	if timeout <= 0 {
		timeout = time.Minute
	}
	ctx, cancel := context.WithTimeout(r.ctx, timeout)
	defer cancel()

	prev := r.currentNode
	r.currentNode = n
	defer func() { r.currentNode = prev }()

	L := r.newState(ctx)
	defer L.Close()

	if params != nil {
		L.SetGlobal("params", goToLua(L, params))
	}

	// 스크립트를 함수 본문으로 감싼다. 그러지 않으면 최상위 return을 쓸 수 없고,
	// 사용자는 결과를 돌려줄 방법을 잃는다.
	fn, err := L.LoadString("return (function()\n" + script + "\nend)()")
	if err != nil {
		return nil, "", fmt.Errorf("스크립트 오류: %w", err)
	}
	L.Push(fn)
	if err := L.PCall(0, 2, nil); err != nil {
		if ctx.Err() != nil && r.ctx.Err() == nil {
			return nil, "", fmt.Errorf("스크립트가 시간 제한(%s)을 넘었습니다", timeout)
		}
		return nil, "", fmt.Errorf("스크립트 실행 실패: %w", err)
	}

	// 반환값을 꺼내기 전에 vars의 변경을 되가져온다.
	// 스크립트가 vars.x = 1 로 값을 넣는 것이 가장 자연스러운 사용법이다.
	if tbl, ok := L.GetGlobal("vars").(*lua.LTable); ok {
		if merged, ok := luaToGo(tbl).(map[string]any); ok {
			for k, v := range merged {
				r.vars[k] = v
			}
		}
	}

	port := ""
	if s, ok := L.Get(-1).(lua.LString); ok {
		port = string(s)
	}
	value := luaToGo(L.Get(-2))
	L.Pop(2)
	return value, port, nil
}

// evalLua는 식 하나를 계산한다(분기 조건, 변수 설정).
func (r *runner) evalLua(expr string) (any, error) {
	if strings.TrimSpace(expr) == "" {
		return nil, fmt.Errorf("식이 비어 있습니다")
	}
	value, _, err := r.runLua(r.currentNode, "return "+expr, nil)
	if err != nil {
		// 식이 아니라 여러 줄 스크립트를 쓴 경우를 위해 그대로 한 번 더 시도한다.
		// "return x" 로 감싸면 문법 오류가 나지만 본문으로는 유효한 입력이 흔하다.
		value, _, err2 := r.runLua(r.currentNode, expr, nil)
		if err2 != nil {
			return nil, err
		}
		return value, nil
	}
	return value, nil
}

// EvalParamExprs는 트리거 파라미터 식을 계산한다.
//
// 매크로 실행기와 완전히 분리된 샌드박스에서 돈다: DB·셸·HTTP 함수를 심지 않는다.
// 이 식은 "무엇을 넘길지" 를 정하는 자리이지 일을 하는 자리가 아니고, 아직 실행
// 권한 검사도 지나지 않았다. 여기서 부수효과를 허용하면 트리거가 매크로를 거치지
// 않고도 DB를 건드릴 수 있게 된다.
//
// vars에는 trigger·event 같은 문맥이 들어온다. 한 식이 실패해도 나머지는 계산하고,
// 실패한 것만 오류로 모아 돌려준다 — 자동 실행은 아무도 보고 있지 않으므로
// 무엇이 왜 비었는지 기록에 남아야 한다.
func EvalParamExprs(ctx context.Context, exprs map[string]string, vars map[string]any) (map[string]any, []string) {
	out := map[string]any{}
	problems := []string{}
	if len(exprs) == 0 {
		return out, problems
	}

	evalCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	for key, expr := range exprs {
		if strings.TrimSpace(expr) == "" {
			continue
		}
		value, err := evalStandalone(evalCtx, expr, vars)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", key, err))
			continue
		}
		out[key] = value
	}
	return out, problems
}

func evalStandalone(ctx context.Context, expr string, vars map[string]any) (any, error) {
	L := newSandbox(ctx)
	defer L.Close()

	for k, v := range vars {
		L.SetGlobal(k, goToLua(L, v))
	}

	// 한 줄짜리 식이 가장 흔하므로 return을 붙여 먼저 시도한다.
	// 실패하면 여러 줄 스크립트로 보고 그대로 실행한다(사용자가 return을 직접 쓴 경우).
	for _, script := range []string{"return " + expr, expr} {
		fn, err := L.LoadString("return (function()\n" + script + "\nend)()")
		if err != nil {
			continue
		}
		L.Push(fn)
		if err := L.PCall(0, 1, nil); err != nil {
			if ctx.Err() != nil {
				return nil, fmt.Errorf("시간 제한을 넘었습니다")
			}
			return nil, fmt.Errorf("실행 실패: %w", err)
		}
		value := luaToGo(L.Get(-1))
		L.Pop(1)
		return value, nil
	}
	return nil, fmt.Errorf("식을 해석하지 못했습니다")
}

// ---------- 값 변환 ----------

// goToLua는 Go 값을 Lua 값으로 바꾼다.
func goToLua(L *lua.LState, v any) lua.LValue {
	switch val := v.(type) {
	case nil:
		return lua.LNil
	case bool:
		return lua.LBool(val)
	case string:
		return lua.LString(val)
	case float64:
		return lua.LNumber(val)
	case float32:
		return lua.LNumber(val)
	case int:
		return lua.LNumber(val)
	case int64:
		return lua.LNumber(val)
	case []any:
		tbl := L.NewTable()
		for _, item := range val {
			tbl.Append(goToLua(L, item))
		}
		return tbl
	case map[string]any:
		tbl := L.NewTable()
		for k, item := range val {
			L.SetField(tbl, k, goToLua(L, item))
		}
		return tbl
	default:
		return lua.LString(fmt.Sprint(val))
	}
}

// luaToGo는 Lua 값을 Go 값으로 바꾼다.
//
// 표는 배열인지 맵인지 판단해야 한다. Lua에는 구분이 없지만 JSON과 템플릿에는 있다.
// 1부터 빈틈없이 이어지는 정수 키만 있으면 배열로 본다 — Lua의 관습과 같다.
func luaToGo(v lua.LValue) any {
	switch val := v.(type) {
	case *lua.LNilType:
		return nil
	case lua.LBool:
		return bool(val)
	case lua.LString:
		return string(val)
	case lua.LNumber:
		return float64(val)
	case *lua.LTable:
		return tableToGo(val)
	default:
		return nil
	}
}

func tableToGo(tbl *lua.LTable) any {
	// 배열 여부 판정: 길이가 0보다 크고, 그 길이만큼의 정수 키가 전부 있고,
	// 다른 키가 없어야 한다.
	n := tbl.Len()
	if n > 0 {
		isArray := true
		count := 0
		tbl.ForEach(func(k, _ lua.LValue) {
			count++
			num, ok := k.(lua.LNumber)
			if !ok || float64(num) != float64(int(num)) || int(num) < 1 || int(num) > n {
				isArray = false
			}
		})
		if isArray && count == n {
			out := make([]any, 0, n)
			for i := 1; i <= n; i++ {
				out = append(out, luaToGo(tbl.RawGetInt(i)))
			}
			return out
		}
	}
	out := map[string]any{}
	tbl.ForEach(func(k, v lua.LValue) {
		out[k.String()] = luaToGo(v)
	})
	return out
}

// normalizeJSON은 encoding/json이 만든 값을 내부 표현으로 맞춘다.
// json은 이미 float64/map[string]any/[]any를 쓰므로 대부분 그대로다.
func normalizeJSON(v any) any {
	switch val := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(val))
		for k, item := range val {
			out[k] = normalizeJSON(item)
		}
		return out
	case []any:
		out := make([]any, len(val))
		for i, item := range val {
			out[i] = normalizeJSON(item)
		}
		return out
	default:
		return v
	}
}

func parseJSONObject(s string) (map[string]any, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, fmt.Errorf("JSON 형식이 아닙니다: %w", err)
	}
	return out, nil
}

func stringOr(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return stringify(v)
}

func orZero(v any) float64 {
	f, err := toFloat(v)
	if err != nil {
		return 0
	}
	return f
}
