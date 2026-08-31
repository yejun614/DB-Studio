package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"dbstudio/internal/dbx"
	"dbstudio/internal/macro"
	"dbstudio/internal/model"
	"dbstudio/internal/store"
)

// P14·P15 기능의 AI/MCP 툴: DB 서버(서버 1 : DB N)와 매크로 자동 실행.
//
// 여기 없는 것들이 있고, 빠진 이유가 있다.
//
//   - **서버 등록·수정**: 자격증명을 인자로 받아야 하는데, 그러면 비밀번호가 대화 기록에
//     남고 AI 프로바이더로 전송된다. 툴 하나 편하자고 감수할 일이 아니다. 서버를 만들고
//     비밀번호를 바꾸는 것은 화면에서 한다.
//   - **서버 삭제·병합**: 삭제는 DB 여러 개와 그 지표·이벤트·버전·ERD를 함께 지우고,
//     병합은 "두 자격증명 중 무엇이 맞는가"라는 사람의 판단을 요구한다. 되돌릴 수 없는
//     쪽으로 크게 움직이는 동작은 승인 절차가 아니라 화면에 둔다.
//
// 반면 **DB 등록(register_databases)은 넣었다.** 이미 등록된 서버의 자격증명을 그대로
// 쓰므로 새 비밀을 받지 않고, 잘못 넣어도 관리 목록에서 빼면 그만이다.

func infraTools() []*aiTool {
	return []*aiTool{
		// ---------- 서버 ----------
		{
			Name: "list_servers",
			Description: "등록된 DB 서버와 각 서버에 딸린 관리 대상 DB 목록을 반환한다. " +
				"접속 정보와 자격증명은 서버가 갖고 그 아래 DB들이 함께 쓴다. " +
				"\"어떤 서버에 무엇이 붙어 있는지\", \"같은 서버를 중복 등록했는지\"를 볼 때 쓴다.",
			Schema: objectSchema(map[string]any{
				"kind": str("DB 종류로 걸러낸다: mysql, postgres, mssql, oracle, sqlite, mongodb, redis"),
			}),
			Run: toolListServers,
		},
		{
			Name: "list_server_databases",
			Description: "서버에 **실제로 접속해** 그 안에 있는 DB 목록을 읽어온다. " +
				"이미 등록된 것은 registered로 표시된다. 아직 관리하지 않는 DB를 찾을 때 쓴다. " +
				"Oracle과 SQLite는 지원하지 않는다(대상이 곧 접속 정보의 일부라 나열할 수 없다).",
			Schema: objectSchema(map[string]any{
				"server": str("서버 이름 또는 ID (list_servers로 확인)"),
			}, "server"),
			ConnManagerOnly: true,
			Run:             toolListServerDatabases,
		},
		{
			Name: "register_databases",
			Description: "서버에 있는 DB를 관리 대상으로 등록한다. 이미 등록된 서버의 자격증명을 쓰므로 " +
				"비밀번호를 받지 않는다. 실제 데이터베이스를 만들지는 않는다 — 관리 목록에 추가할 뿐이다. " +
				"사용자 승인이 필요하다.",
			Schema: objectSchema(map[string]any{
				"server":      str("서버 이름 또는 ID"),
				"databases":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "등록할 DB 이름 목록"},
				"environment": str("dev 또는 prod (생략하면 서버의 기본값)"),
			}, "server", "databases"),
			Mutating:        true,
			ConnManagerOnly: true,
			Propose:         proposeRegisterDatabases,
			Apply:           applyRegisterDatabases,
		},

		// ---------- 매크로 자동 실행 ----------
		{
			Name: "list_macro_triggers",
			Description: "매크로 자동 실행 설정(정기 실행·조건 실행) 목록과 마지막 발화 결과를 반환한다. " +
				"트리거는 만든 사람의 권한으로 실행되므로, 실패했다면 소유자의 권한을 먼저 확인한다.",
			Schema: objectSchema(map[string]any{
				"macro": str("매크로 이름 또는 ID로 걸러낸다 (생략하면 전체)"),
			}),
			RequiresPerm: model.PermMacro,
			Run:          toolListMacroTriggers,
		},
		{
			Name: "create_macro_trigger",
			Description: "매크로를 자동으로 실행할 트리거를 만든다. 정기 실행(kind=schedule, 5필드 cron)과 " +
				"조건 실행(kind=event, 모니터링 이벤트가 새로 열릴 때)이 있다. " +
				"**만든 사람의 권한으로 실행된다** — 요청한 사용자가 그 매크로를 실행할 수 없으면 트리거도 실패한다. " +
				"사용자 승인이 필요하다.",
			Schema: objectSchema(map[string]any{
				"macro":         str("매크로 이름 또는 ID"),
				"name":          str("트리거 이름"),
				"kind":          str("schedule 또는 event"),
				"cron":          str("schedule일 때: 분 시 일 월 요일 (예: 0 3 * * * = 매일 새벽 3시)"),
				"timezone":      str("schedule일 때: IANA 시간대 (예: Asia/Seoul). 비우면 서버 시간"),
				"eventKind":     str("event일 때: threshold, connectivity, drift, collect_error (비우면 전체)"),
				"eventSeverity": str("event일 때: info, warning, critical 중 이 값 이상만 (비우면 전체)"),
				"eventMetric":   str("event일 때: 특정 지표로 좁힌다"),
				"connection":    str("event일 때: 특정 커넥션의 이벤트로 좁힌다"),
				"params": map[string]any{
					"type": "object", "description": "매크로 실행 파라미터",
				},
			}, "macro", "name", "kind"),
			Mutating:     true,
			RequiresPerm: model.PermMacro,
			Propose:      proposeCreateTrigger,
			Apply:        applyCreateTrigger,
		},
		{
			Name: "set_macro_trigger_enabled",
			Description: "자동 실행 트리거를 켜거나 끈다. 정기 실행을 켤 때는 다음 실행 시각을 다시 잡으므로 " +
				"켜자마자 밀린 실행이 돌지 않는다. 사용자 승인이 필요하다.",
			Schema: objectSchema(map[string]any{
				"trigger": str("트리거 이름 또는 ID (list_macro_triggers로 확인)"),
				"enabled": boolp("true면 켜고 false면 끈다"),
			}, "trigger", "enabled"),
			Mutating:     true,
			RequiresPerm: model.PermMacro,
			Propose:      proposeToggleTrigger,
			Apply:        applyToggleTrigger,
		},
		{
			Name: "delete_macro_trigger",
			Description: "자동 실행 트리거를 삭제한다. 매크로 자체와 실행 이력은 남는다. " +
				"잠시 멈추려는 것이라면 삭제 대신 set_macro_trigger_enabled로 끈다. 사용자 승인이 필요하다.",
			Schema: objectSchema(map[string]any{
				"trigger": str("트리거 이름 또는 ID"),
			}, "trigger"),
			Mutating:     true,
			RequiresPerm: model.PermMacro,
			Propose:      proposeDeleteTrigger,
			Apply:        applyDeleteTrigger,
		},
	}
}

// ---------- 서버 ----------

// resolveServer는 이름 또는 ID로 서버를 찾는다.
func (tc *toolContext) resolveServer(nameOrID string) (*model.Server, error) {
	nameOrID = strings.TrimSpace(nameOrID)
	if nameOrID == "" {
		return nil, errors.New("서버를 지정하세요")
	}
	servers, err := tc.srv.st.ListServers(tc.ctx, tc.projectScope())
	if err != nil {
		return nil, err
	}
	for _, s := range servers {
		if s.ID == nameOrID || strings.EqualFold(s.Name, nameOrID) {
			return s, nil
		}
	}
	return nil, fmt.Errorf("서버 %q 을(를) 찾을 수 없습니다. list_servers로 목록을 확인하세요", nameOrID)
}

func toolListServers(tc *toolContext, args json.RawMessage) (string, error) {
	var in struct {
		Kind string `json:"kind"`
	}
	if err := parseArgs(args, &in); err != nil {
		return "", err
	}
	servers, err := tc.srv.st.ListServers(tc.ctx, tc.projectScope())
	if err != nil {
		return "", err
	}
	// 접근 가능한 DB만 보여준다. 툴도 화면과 같은 판정을 쓴다 —
	// 여기서만 전체를 돌려주면 AI가 권한 우회 통로가 된다.
	conns, levels, err := tc.accessibleConns(model.LevelMonitor)
	if err != nil {
		return "", err
	}
	byServer := map[string][]map[string]any{}
	for _, c := range conns {
		byServer[c.ServerID] = append(byServer[c.ServerID], map[string]any{
			"id": c.ID, "name": c.Name, "database": c.DatabaseName,
			"environment": string(c.Environment), "level": string(levels[c.ID]),
			"enabled": c.Enabled,
		})
	}

	canManage := tc.user.Role.CanManageConnections()
	out := []map[string]any{}
	for _, s := range servers {
		if in.Kind != "" && !strings.EqualFold(string(s.Kind), in.Kind) {
			continue
		}
		dbs := byServer[s.ID]
		// 볼 수 있는 DB가 하나도 없는 서버는 관리자에게만 보인다.
		// 아니면 접근할 수 없는 서버의 이름과 호스트가 그대로 드러난다.
		if len(dbs) == 0 && !canManage {
			continue
		}
		if dbs == nil {
			dbs = []map[string]any{}
		}
		out = append(out, map[string]any{
			"id": s.ID, "name": s.Name, "kind": string(s.Kind),
			"host": s.Host, "port": s.Port, "enabled": s.Enabled,
			"defaultEnvironment": string(s.DefaultEnvironment),
			"databaseCount":      s.DatabaseCount,
			"databases":          dbs,
			"canListDatabases":   dbx.CanListDatabases(s.Kind),
		})
	}
	return asJSON(map[string]any{
		"servers": out,
		"note": "접속 정보와 자격증명은 서버가 갖고 그 아래 DB들이 함께 쓴다. " +
			"databases는 이 사용자가 접근할 수 있는 것만이며, databaseCount는 실제 전체 개수다.",
	})
}

func toolListServerDatabases(tc *toolContext, args json.RawMessage) (string, error) {
	var in struct {
		Server string `json:"server"`
	}
	if err := parseArgs(args, &in); err != nil {
		return "", err
	}
	srv, err := tc.resolveServer(in.Server)
	if err != nil {
		return "", err
	}
	sec, err := tc.srv.st.GetServerSecret(tc.ctx, srv.ID)
	if err != nil {
		return "", err
	}

	ctx, cancel := context.WithTimeout(tc.ctx, connTestTimeout)
	defer cancel()

	list, err := dbx.ListDatabases(ctx, srv, sec)
	if err != nil {
		return "", err
	}
	existing, err := tc.srv.st.ListConnectionsByServer(tc.ctx, srv.ID)
	if err != nil {
		return "", err
	}
	registered := make(map[string]bool, len(existing))
	for _, c := range existing {
		registered[c.DatabaseName] = true
	}
	for i := range list {
		list[i].Registered = registered[list[i].Name]
	}
	dbx.SortDatabases(list)

	return asJSON(map[string]any{
		"server":    srv.Name,
		"databases": list,
		"note": "system은 엔진이 스스로 쓰는 DB다. registered는 이미 관리 중이라 " +
			"register_databases로 다시 등록할 수 없다.",
	})
}

// registerInput은 DB 일괄 등록 인자다.
type registerInput struct {
	Server      string   `json:"server"`
	Databases   []string `json:"databases"`
	Environment string   `json:"environment"`
}

// planRegister는 등록 계획을 세운다. Propose와 Apply가 같은 판단을 쓰게 하기 위해 나눴다.
func (tc *toolContext) planRegister(in registerInput) (*model.Server, []string, model.Environment, error) {
	srv, err := tc.resolveServer(in.Server)
	if err != nil {
		return nil, nil, "", err
	}
	env := model.Environment(strings.TrimSpace(in.Environment))
	if env == "" {
		env = srv.DefaultEnvironment
	}
	if !env.Valid() {
		return nil, nil, "", errors.New("환경은 dev 또는 prod여야 합니다")
	}

	existing, err := tc.srv.st.ListConnectionsByServer(tc.ctx, srv.ID)
	if err != nil {
		return nil, nil, "", err
	}
	taken := make(map[string]bool, len(existing))
	for _, c := range existing {
		taken[c.DatabaseName] = true
	}

	names := []string{}
	for _, raw := range in.Databases {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		if taken[name] {
			return nil, nil, "", fmt.Errorf("%s 는 이미 등록되어 있습니다", name)
		}
		taken[name] = true
		names = append(names, name)
	}
	if len(names) == 0 {
		return nil, nil, "", errors.New("등록할 DB를 하나 이상 지정하세요")
	}
	return srv, names, env, nil
}

func proposeRegisterDatabases(tc *toolContext, args json.RawMessage) (string, any, error) {
	var in registerInput
	if err := parseArgs(args, &in); err != nil {
		return "", nil, err
	}
	srv, names, env, err := tc.planRegister(in)
	if err != nil {
		return "", nil, err
	}
	envLabel := "개발"
	if env == model.EnvProd {
		envLabel = "운영"
	}
	return fmt.Sprintf("서버 %s에 DB %d개를 %s 환경으로 등록합니다: %s",
			srv.Name, len(names), envLabel, strings.Join(names, ", ")),
		map[string]any{
			"server": srv.Name, "databases": names, "environment": string(env),
			"note": "실제 데이터베이스를 만들지는 않는다. 관리 목록에 추가할 뿐이다.",
		}, nil
}

func applyRegisterDatabases(tc *toolContext, args json.RawMessage) (string, error) {
	var in registerInput
	if err := parseArgs(args, &in); err != nil {
		return "", err
	}
	srv, names, env, err := tc.planRegister(in)
	if err != nil {
		return "", err
	}

	created := []string{}
	failed := []map[string]string{}
	for _, name := range names {
		conn, err := tc.srv.st.CreateConnection(tc.ctx, store.SaveConnectionParams{
			ServerID: srv.ID, Name: srv.Name + " / " + name, Environment: env,
			DatabaseName: name, Enabled: true, ActorID: tc.user.ID,
		})
		if err != nil {
			reason := "등록에 실패했습니다"
			if errors.Is(err, store.ErrDuplicateName) {
				reason = "같은 이름이나 같은 DB가 이미 등록되어 있습니다"
			}
			failed = append(failed, map[string]string{"database": name, "reason": reason})
			continue
		}
		created = append(created, conn.Name)
		tc.audit(store.ActionConnCreated, "connection", conn.ID, "ok", map[string]any{
			"name": conn.Name, "kind": conn.Kind, "environment": conn.Environment,
			"server": srv.Name, "via": "ai_tool",
		})
		tc.srv.monitor.TriggerPoll(conn.ID)
	}
	return asJSON(map[string]any{
		"created": created, "failed": failed,
		"note": "등록된 DB는 서버의 자격증명을 그대로 쓴다.",
	})
}

// ---------- 매크로 자동 실행 ----------

// resolveTrigger는 이름 또는 ID로 트리거를 찾는다.
//
// 이 함수를 쓰는 툴은 전부 트리거를 바꾸는 것(켜기·끄기·삭제)이므로 **관리 권한까지
// 여기서 확인한다.** 찾기와 판정을 나누면 툴이 하나 늘 때마다 판정을 다시 붙여야 하고,
// 그 하나를 빠뜨리는 것으로 규칙이 무너진다.
//
// 이름이 여러 개 걸리면 고르지 않고 되돌린다. 자동 실행을 잘못 끄거나 지우는 것은
// 조용히 아무 일도 일어나지 않게 만드는 종류의 실수라, 짐작으로 정하면 안 된다.
func (tc *toolContext) resolveTrigger(nameOrID string) (*store.MacroTrigger, error) {
	t, err := tc.findTrigger(nameOrID)
	if err != nil {
		return nil, err
	}
	m, err := tc.srv.st.GetMacro(tc.ctx, t.MacroID, store.MacroViewer{User: tc.user})
	if err != nil {
		return nil, err
	}
	if !m.Access.CanManage() {
		return nil, fmt.Errorf("매크로 %q 의 자동 실행을 바꿀 권한이 없습니다 "+
			"(만든 사람과 협업자만 가능합니다)", m.Name)
	}
	return t, nil
}

func (tc *toolContext) findTrigger(nameOrID string) (*store.MacroTrigger, error) {
	nameOrID = strings.TrimSpace(nameOrID)
	if nameOrID == "" {
		return nil, errors.New("트리거를 지정하세요")
	}
	// 볼 수 있는 매크로의 트리거만 후보다. 이름으로 찾는 경로이므로 거르지 않으면
	// 남의 비공개 매크로에 걸린 자동 실행을 이름만으로 만질 수 있게 된다.
	list, err := tc.srv.st.ListVisibleTriggers(tc.ctx, "", store.MacroViewer{User: tc.user})
	if err != nil {
		return nil, err
	}
	var matches []*store.MacroTrigger
	for _, t := range list {
		if t.ID == nameOrID {
			return t, nil
		}
		if strings.EqualFold(t.Name, nameOrID) {
			matches = append(matches, t)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return nil, fmt.Errorf("트리거 %q 을(를) 찾을 수 없습니다. list_macro_triggers로 목록을 확인하세요", nameOrID)
	default:
		names := make([]string, 0, len(matches))
		for _, t := range matches {
			names = append(names, fmt.Sprintf("%s (%s, id=%s)", t.Name, t.MacroName, t.ID))
		}
		return nil, fmt.Errorf("같은 이름의 트리거가 여러 개입니다. ID로 지정하세요: %s",
			strings.Join(names, " / "))
	}
}

func triggerSummary(t *store.MacroTrigger) map[string]any {
	out := map[string]any{
		"id": t.ID, "name": t.Name, "macro": t.MacroName, "kind": t.Kind,
		"enabled": t.Enabled, "owner": t.OwnerName,
		"lastStatus": t.LastStatus, "failCount": t.FailCount,
	}
	if t.Kind == store.TriggerSchedule {
		out["cron"] = t.Cron
		out["timezone"] = t.Timezone
		out["describe"] = macro.Describe(t.Cron)
		if t.NextRunAt != nil {
			out["nextRunAt"] = t.NextRunAt.Format(time.RFC3339)
		}
	} else {
		out["eventKind"] = orAll(t.EventKind)
		out["eventSeverity"] = orAll(t.EventSeverity)
		out["eventMetric"] = orAll(t.EventMetric)
		out["minIntervalSec"] = t.MinIntervalSec
	}
	if t.LastFiredAt != nil {
		out["lastFiredAt"] = t.LastFiredAt.Format(time.RFC3339)
	}
	if t.LastError != "" {
		out["lastError"] = t.LastError
	}
	return out
}

func orAll(s string) string {
	if s == "" {
		return "(전체)"
	}
	return s
}

func toolListMacroTriggers(tc *toolContext, args json.RawMessage) (string, error) {
	var in struct {
		Macro string `json:"macro"`
	}
	if err := parseArgs(args, &in); err != nil {
		return "", err
	}
	macroID := ""
	if strings.TrimSpace(in.Macro) != "" {
		m, err := tc.resolveMacro(in.Macro)
		if err != nil {
			return "", err
		}
		macroID = m.ID
	}
	list, err := tc.srv.st.ListVisibleTriggers(tc.ctx, macroID, store.MacroViewer{User: tc.user})
	if err != nil {
		return "", err
	}
	out := make([]map[string]any, 0, len(list))
	for _, t := range list {
		out = append(out, triggerSummary(t))
	}
	return asJSON(map[string]any{
		"triggers": out,
		"note": "트리거는 소유자(owner)의 권한으로 실행된다. " +
			"lastStatus가 disabled면 소유자의 권한이 회수되었거나 연속 실패로 스스로 꺼진 것이다.",
	})
}

// triggerInput은 트리거 생성 인자다.
type triggerInput struct {
	Macro         string         `json:"macro"`
	Name          string         `json:"name"`
	Kind          string         `json:"kind"`
	Cron          string         `json:"cron"`
	Timezone      string         `json:"timezone"`
	EventKind     string         `json:"eventKind"`
	EventSeverity string         `json:"eventSeverity"`
	EventMetric   string         `json:"eventMetric"`
	Connection    string         `json:"connection"`
	Params        map[string]any `json:"params"`
}

// planTrigger는 인자를 검증해 저장 파라미터로 바꾼다.
//
// 화면과 같은 검증(api.triggerParams)을 쓴다. 규칙이 두 벌이 되면 툴로 만든 트리거만
// 화면에서 열리지 않는 식의 어긋남이 생긴다.
func (tc *toolContext) planTrigger(in triggerInput) (store.SaveTriggerParams, *store.Macro, error) {
	var zero store.SaveTriggerParams
	m, err := tc.resolveMacro(in.Macro)
	if err != nil {
		return zero, nil, err
	}
	// 자동 실행은 관리 권한이다. 공개+수정으로 열린 매크로라도 아무나 걸 수는 없다.
	if !m.Access.CanManage() {
		return zero, nil, fmt.Errorf(
			"매크로 %q 에 자동 실행을 걸 권한이 없습니다 (만든 사람과 협업자만 가능합니다)", m.Name)
	}
	// 실행할 수 없는 매크로에 자동 실행을 걸 수는 없다.
	// 트리거는 만든 사람의 권한으로 돌기 때문에, 지금 막히면 나중에도 막힌다.
	_, _, blockers, err := tc.macroRunPlan(in.Macro)
	if err != nil {
		return zero, nil, err
	}
	if len(blockers) > 0 {
		reasons := make([]string, 0, len(blockers))
		for _, b := range blockers {
			reasons = append(reasons, b.Node+": "+b.Reason)
		}
		return zero, nil, fmt.Errorf(
			"이 매크로를 실행할 권한이 없어 트리거를 만들 수 없습니다 — %s", strings.Join(reasons, " / "))
	}

	connID := ""
	if strings.TrimSpace(in.Connection) != "" {
		conn, err := tc.resolveConn(in.Connection, model.LevelMonitor)
		if err != nil {
			return zero, nil, err
		}
		connID = conn.ID
	}

	req := triggerRequest{
		Name: in.Name, Kind: strings.TrimSpace(in.Kind), Params: in.Params,
		Cron: in.Cron, Timezone: in.Timezone,
		EventKind: in.EventKind, EventSeverity: in.EventSeverity,
		EventMetric: in.EventMetric, ConnectionID: connID,
	}
	p, code, message := triggerParams(req, m.ID)
	if code != "" {
		return zero, nil, errors.New(message)
	}
	p.OwnerID = tc.user.ID
	p.OwnerName = displayName(tc.user)
	return p, m, nil
}

func proposeCreateTrigger(tc *toolContext, args json.RawMessage) (string, any, error) {
	var in triggerInput
	if err := parseArgs(args, &in); err != nil {
		return "", nil, err
	}
	p, m, err := tc.planTrigger(in)
	if err != nil {
		return "", nil, err
	}

	preview := map[string]any{
		"macro": m.Name, "trigger": p.Name, "kind": p.Kind,
		"owner": p.OwnerName,
		"note":  "이 트리거는 " + p.OwnerName + " 의 권한으로 실행된다.",
	}
	var when string
	if p.Kind == store.TriggerSchedule {
		when = macro.Describe(p.Cron)
		preview["cron"] = p.Cron
		preview["describe"] = when
		// 다음 실행 시각을 함께 보여준다. cron 식은 눈으로 검산하기 어렵고,
		// 틀렸다는 것은 대개 실행되지 않은 다음 날 아침에야 알게 된다.
		if next := nextRuns(p.Cron, p.Timezone, 3); len(next) > 0 {
			preview["nextRuns"] = next
			when += " (다음: " + strings.Join(next, ", ") + ")"
		}
	} else {
		when = fmt.Sprintf("%s 이벤트", orAll(p.EventKind))
		if p.EventSeverity != "" {
			when += " " + p.EventSeverity + " 이상"
		}
		preview["eventKind"] = orAll(p.EventKind)
		preview["eventSeverity"] = orAll(p.EventSeverity)
		preview["minIntervalSec"] = p.MinIntervalSec
	}
	return fmt.Sprintf("매크로 %q 를 자동 실행합니다 — %s", m.Name, when), preview, nil
}

// nextRuns는 다음 실행 시각 몇 개를 사람이 읽는 문자열로 만든다.
func nextRuns(spec, timezone string, count int) []string {
	schedule, err := macro.ParseSchedule(spec)
	if err != nil {
		return nil
	}
	loc, err := macro.LoadLocation(timezone)
	if err != nil {
		return nil
	}
	out := make([]string, 0, count)
	cursor := time.Now().In(loc)
	for range count {
		next, ok := schedule.Next(cursor)
		if !ok {
			break
		}
		out = append(out, next.Format("2006-01-02 15:04 MST"))
		cursor = next
	}
	return out
}

func applyCreateTrigger(tc *toolContext, args json.RawMessage) (string, error) {
	var in triggerInput
	if err := parseArgs(args, &in); err != nil {
		return "", err
	}
	p, m, err := tc.planTrigger(in)
	if err != nil {
		return "", err
	}
	t, err := tc.srv.st.CreateTrigger(tc.ctx, p)
	if err != nil {
		return "", err
	}
	tc.audit("macro.trigger.created", "macro", m.ID, "ok", map[string]any{
		"macro": m.Name, "trigger": t.Name, "kind": t.Kind,
		"cron": t.Cron, "eventKind": t.EventKind, "via": "ai_tool",
	})
	return asJSON(map[string]any{
		"trigger": triggerSummary(t),
		"note":    "이 트리거는 " + t.OwnerName + " 의 권한으로 실행된다.",
	})
}

func proposeToggleTrigger(tc *toolContext, args json.RawMessage) (string, any, error) {
	var in struct {
		Trigger string `json:"trigger"`
		Enabled *bool  `json:"enabled"`
	}
	if err := parseArgs(args, &in); err != nil {
		return "", nil, err
	}
	if in.Enabled == nil {
		return "", nil, errors.New("enabled를 true 또는 false로 지정하세요")
	}
	t, err := tc.resolveTrigger(in.Trigger)
	if err != nil {
		return "", nil, err
	}
	if t.Enabled == *in.Enabled {
		state := "꺼져"
		if t.Enabled {
			state = "켜져"
		}
		return "", nil, fmt.Errorf("트리거 %q 는 이미 %s 있습니다", t.Name, state)
	}
	verb := "끕니다"
	if *in.Enabled {
		verb = "켭니다"
	}
	return fmt.Sprintf("매크로 %q 의 자동 실행 %q 를 %s", t.MacroName, t.Name, verb),
		triggerSummary(t), nil
}

func applyToggleTrigger(tc *toolContext, args json.RawMessage) (string, error) {
	var in struct {
		Trigger string `json:"trigger"`
		Enabled *bool  `json:"enabled"`
	}
	if err := parseArgs(args, &in); err != nil {
		return "", err
	}
	if in.Enabled == nil {
		return "", errors.New("enabled를 true 또는 false로 지정하세요")
	}
	t, err := tc.resolveTrigger(in.Trigger)
	if err != nil {
		return "", err
	}

	// 켤 때는 다음 실행 시각을 다시 잡는다. 꺼져 있는 동안 예정 시각이 과거가 되었을
	// 텐데 그대로 켜면 켜자마자 한 번 실행된다 — 대개 그것은 의도가 아니다.
	if *in.Enabled && t.Kind == store.TriggerSchedule {
		schedule, perr := macro.ParseSchedule(t.Cron)
		if perr != nil {
			return "", perr
		}
		loc, lerr := macro.LoadLocation(t.Timezone)
		if lerr != nil {
			return "", lerr
		}
		if next, ok := schedule.Next(time.Now().In(loc)); ok {
			utc := next.UTC()
			if err := tc.srv.st.SetTriggerNextRun(tc.ctx, t.ID, &utc); err != nil {
				return "", err
			}
		}
	}
	if err := tc.srv.st.SetTriggerEnabled(tc.ctx, t.ID, *in.Enabled); err != nil {
		return "", err
	}
	tc.audit("macro.trigger.toggled", "macro", t.MacroID, "ok", map[string]any{
		"trigger": t.Name, "enabled": *in.Enabled, "via": "ai_tool",
	})
	updated, err := tc.srv.st.GetTrigger(tc.ctx, t.ID)
	if err != nil {
		return "", err
	}
	return asJSON(map[string]any{"trigger": triggerSummary(updated)})
}

func proposeDeleteTrigger(tc *toolContext, args json.RawMessage) (string, any, error) {
	var in struct {
		Trigger string `json:"trigger"`
	}
	if err := parseArgs(args, &in); err != nil {
		return "", nil, err
	}
	t, err := tc.resolveTrigger(in.Trigger)
	if err != nil {
		return "", nil, err
	}
	return fmt.Sprintf("매크로 %q 의 자동 실행 %q 를 삭제합니다. 이 매크로는 더 이상 저절로 실행되지 않습니다",
		t.MacroName, t.Name), triggerSummary(t), nil
}

func applyDeleteTrigger(tc *toolContext, args json.RawMessage) (string, error) {
	var in struct {
		Trigger string `json:"trigger"`
	}
	if err := parseArgs(args, &in); err != nil {
		return "", err
	}
	t, err := tc.resolveTrigger(in.Trigger)
	if err != nil {
		return "", err
	}
	if err := tc.srv.st.DeleteTrigger(tc.ctx, t.ID); err != nil {
		return "", err
	}
	tc.audit("macro.trigger.deleted", "macro", t.MacroID, "ok", map[string]any{
		"trigger": t.Name, "kind": t.Kind, "via": "ai_tool",
	})
	return asJSON(map[string]any{
		"deleted": t.Name,
		"macro":   t.MacroName,
		"note":    "매크로와 실행 이력은 남는다.",
	})
}
