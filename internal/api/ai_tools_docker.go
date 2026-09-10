package api

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"dbstudio/internal/docker"
	"dbstudio/internal/model"
	"dbstudio/internal/provision"
	"dbstudio/internal/store"
)

// 도커로 DB 를 세우는 툴.
//
// ── 왜 카탈로그를 설명하는 툴이 함께 있는가 ─────────────────────────
// 매크로와 같은 이유다. 어떤 DB 에 어떤 칸이 있는지는 서버가 아는 것이고,
// 그것을 모르면 모델은 그럴듯한 이름의 칸을 지어낸다 — `max_connections` 를
// `maxConn` 으로 적은 요청은 조용히 무시되고, 사람은 정한 값이 안 먹었다는 것을
// 한참 뒤에 안다. 그래서 만들기 전에 반드시 카탈로그를 먼저 부르게 한다.
//
// ── 무엇을 승인 뒤로 미루는가 ───────────────────────────────────────
// 만들기·지우기·실행·중단은 모두 승인을 거친다. 읽기만 바로 돈다.
//
// 실행·중단까지 승인을 요구하는 이유: 여기서 멈추는 것은 **누군가 쓰고 있는
// DB** 일 수 있다. 모델이 "정리하겠습니다"라고 판단해 멈추는 일이 일어나서는
// 안 된다 — 그 판단의 근거는 모델이 볼 수 없는 곳(누가 붙어 있는가)에 있다.
//
// ── 왜 이중 문을 그대로 지키는가 ────────────────────────────────────
// 도커 소켓에 닿는다는 것은 그 기계에서 무엇이든 할 수 있다는 뜻이다. 화면에서
// 이중 문(스위치 × 권한)을 두고 툴에서 풀어 주면, 문을 옆으로 돌아가는 길을
// 우리가 만든 셈이 된다. RequiresDocker 와 RequiresPerm 을 함께 붙이고,
// 실행 시점에도 다시 본다.

func dockerTools() []*aiTool {
	return []*aiTool{
		{
			Name: "describe_db_catalog",
			Description: "도커로 띄울 수 있는 DB 종류와 각 종류에 정할 수 있는 설정 칸을 반환한다. " +
				"create_db_container 로 DB 를 만들기 전에 반드시 먼저 부른다 — 칸 이름을 " +
				"지어내면 그 값은 조용히 무시된다.",
			Schema: objectSchema(map[string]any{
				"kind": str("한 종류만 자세히 보려면 그 id (postgres, mysql, mariadb, mongodb, redis, clickhouse, mssql)"),
			}),
			RequiresDocker: true,
			RequiresPerm:   model.PermDockerManage,
			Run:            toolDescribeDBCatalog,
		},
		{
			Name:        "list_db_containers",
			Description: "DB Studio 가 도커로 만든 DB 목록을 반환한다. 상태·포트·커넥션 등록 여부를 함께 준다.",
			Schema: objectSchema(map[string]any{
				"project": str("프로젝트 이름 또는 ID (생략하면 볼 수 있는 전체)"),
			}),
			RequiresDocker: true,
			RequiresPerm:   model.PermDockerManage,
			Run:            toolListDBContainers,
		},
		{
			Name: "get_db_container_logs",
			Description: "만든 DB 컨테이너의 최근 로그를 반환한다. 뜨지 않거나 죽는 이유를 찾을 때 쓴다 — " +
				"컨테이너로 띄운 DB 는 그 이유를 로그에만 남긴다.",
			Schema: objectSchema(map[string]any{
				"name": str("DB 컨테이너 이름 또는 ID"),
				"tail": num("가져올 줄 수 (기본 100, 최대 500)"),
			}, "name"),
			RequiresDocker: true,
			RequiresPerm:   model.PermDockerManage,
			Run:            toolDBContainerLogs,
		},
		{
			Name:        "export_db_compose",
			Description: "만든 DB 를 compose.yml 로 내보낸다. 비밀번호는 파일에 들어가지 않고 ${VAR} 자리만 적힌다.",
			Schema: objectSchema(map[string]any{
				"name":    str("DB 컨테이너 이름 또는 ID (생략하면 프로젝트 전체)"),
				"project": str("프로젝트 이름 또는 ID (name 을 생략했을 때)"),
			}),
			RequiresDocker: true,
			RequiresPerm:   model.PermDockerManage,
			Run:            toolExportDBCompose,
		},
		{
			Name: "create_db_container",
			Description: "도커로 새 DB 를 띄운다. values 의 칸 이름은 describe_db_catalog 로 먼저 확인한다. " +
				"사용자 승인이 필요하며, 승인 뒤 만들기는 배경에서 돈다 — " +
				"진행 상황은 list_db_containers 로 확인한다.",
			Schema: objectSchema(map[string]any{
				"kind":    str("DB 종류 id (describe_db_catalog 의 id)"),
				"name":    str("이 DB 의 이름. 소문자로 시작하고 소문자·숫자·붙임표만"),
				"version": str("이미지 태그 (생략하면 기본값)"),
				"project": str("프로젝트 이름 또는 ID"),
				"values": map[string]any{
					"type": "object",
					"description": "설정 칸 열쇠 → 값. describe_db_catalog 가 준 key 를 그대로 쓴다. " +
						"required 인 칸(대개 password)은 반드시 넣는다.",
				},
				"register": flag("뜬 뒤 DB 커넥션으로 등록할지 (기본 true)"),
			}, "kind", "name", "project"),
			Mutating:       true,
			RequiresDocker: true,
			RequiresPerm:   model.PermDockerManage,
			Propose:        proposeCreateDBContainer,
			Apply:          applyCreateDBContainer,
		},
		{
			Name:        "control_db_container",
			Description: "만든 DB 를 시작·중단·다시 시작한다. 사용자 승인이 필요하다.",
			Schema: objectSchema(map[string]any{
				"name":   str("DB 컨테이너 이름 또는 ID"),
				"action": str("start, stop, restart"),
			}, "name", "action"),
			Mutating:       true,
			RequiresDocker: true,
			RequiresPerm:   model.PermDockerManage,
			Propose:        proposeControlDBContainer,
			Apply:          applyControlDBContainer,
		},
		{
			Name: "remove_db_container",
			Description: "만든 DB 컨테이너를 지운다. 데이터(볼륨)는 dropData 가 true 일 때만 함께 지운다. " +
				"사용자 승인이 필요하다.",
			Schema: objectSchema(map[string]any{
				"name":     str("DB 컨테이너 이름 또는 ID"),
				"dropData": flag("데이터 볼륨까지 지울지 (기본 false — 되돌릴 수 없다)"),
			}, "name"),
			Mutating:       true,
			RequiresDocker: true,
			RequiresPerm:   model.PermDockerManage,
			Propose:        proposeRemoveDBContainer,
			Apply:          applyRemoveDBContainer,
		},
	}
}

// ---------- 공통 ----------

// requireDockerTool은 툴 실행 시점의 이중 문이다.
//
// 노출 여부(RequiresDocker·RequiresPerm)와 따로 두는 이유: 노출은 편의이고
// 실제 방어는 실행 시점이다. MCP 와 REST 는 툴 목록을 거치지 않고 이름으로
// 바로 부를 수 있다 — 그 길에서 문이 없으면 문이 없는 것이다.
func (tc *toolContext) requireDockerTool() error {
	if !tc.srv.cfg.AllowDocker {
		return fmt.Errorf("이 서버는 도커 기능이 꺼져 있습니다 (-allow-docker 로 켭니다)")
	}
	if tc.user == nil || !tc.user.HasPerm(model.PermDockerManage) {
		return fmt.Errorf("%s 권한이 없습니다", model.PermDockerManage.Label())
	}
	return nil
}

// canSeeProject는 이 사람이 그 프로젝트를 볼 수 있는지다.
//
// 화면 쪽 canSeeProject 와 같은 판정이되 fiber.Ctx 대신 툴 컨텍스트를 쓴다.
// 규칙을 두 곳에 적는 셈이라 마음에 들지 않지만, 여기서 느슨하게 두면 화면에서
// 막은 것을 어시스턴트로 우회하게 된다 — 그쪽이 훨씬 나쁘다.
func (tc *toolContext) canSeeProject(projectID string) (bool, error) {
	if tc.user == nil {
		return false, nil
	}
	if tc.user.Role == model.RoleSuperadmin {
		return true, nil
	}
	return tc.srv.st.IsProjectMember(tc.ctx, projectID, tc.user.ID)
}

// findDBInstance는 이름이나 id 로 인스턴스를 찾고 프로젝트 접근을 확인한다.
func (tc *toolContext) findDBInstance(nameOrID string) (*store.DBInstance, error) {
	want := strings.TrimSpace(nameOrID)
	if want == "" {
		return nil, fmt.Errorf("DB 컨테이너 이름을 지정하세요")
	}
	all, err := tc.srv.st.ListDBInstances(tc.ctx, "")
	if err != nil {
		return nil, err
	}
	var found *store.DBInstance
	for _, in := range all {
		if in.ID == want || strings.EqualFold(in.Name, want) || in.ContainerName == want {
			found = in
			break
		}
	}
	if found == nil {
		return nil, fmt.Errorf("%s 라는 DB 컨테이너를 찾을 수 없습니다", nameOrID)
	}
	// 프로젝트를 못 보면 그 안의 것도 없는 것이다. 툴에서도 같은 규칙을 쓴다 —
	// 여기서만 느슨하면 화면에서 막은 것을 어시스턴트로 우회할 수 있다.
	ok, err := tc.canSeeProject(found.ProjectID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("%s 라는 DB 컨테이너를 찾을 수 없습니다", nameOrID)
	}
	return found, nil
}

// findProjectID는 이름이나 id 로 프로젝트를 찾는다.
func (tc *toolContext) findProjectID(nameOrID string) (string, error) {
	want := strings.TrimSpace(nameOrID)
	if want == "" {
		return "", fmt.Errorf("프로젝트를 지정하세요")
	}
	list, err := tc.srv.st.ListProjects(tc.ctx, "")
	if err != nil {
		return "", err
	}
	for _, p := range list {
		if p.ID != want && !strings.EqualFold(p.Name, want) {
			continue
		}
		ok, err := tc.canSeeProject(p.ID)
		if err != nil {
			return "", err
		}
		if !ok {
			break
		}
		return p.ID, nil
	}
	return "", fmt.Errorf("%s 라는 프로젝트를 찾을 수 없습니다", nameOrID)
}

// ---------- 읽기 ----------

func toolDescribeDBCatalog(tc *toolContext, args json.RawMessage) (string, error) {
	if err := tc.requireDockerTool(); err != nil {
		return "", err
	}
	var a struct {
		Kind string `json:"kind"`
	}
	_ = json.Unmarshal(args, &a)

	type field struct {
		Key      string   `json:"key"`
		Label    string   `json:"label"`
		Kind     string   `json:"kind"`
		Default  string   `json:"default,omitempty"`
		Choices  []string `json:"choices,omitempty"`
		Required bool     `json:"required,omitempty"`
		Secret   bool     `json:"secret,omitempty"`
		Help     string   `json:"help,omitempty"`
	}
	type recipe struct {
		ID       string   `json:"id"`
		Label    string   `json:"label"`
		Blurb    string   `json:"blurb"`
		Image    string   `json:"image"`
		Versions []string `json:"versions"`
		Port     int      `json:"port"`
		Fields   []field  `json:"fields"`
		Notes    []string `json:"notes,omitempty"`
	}

	out := []recipe{}
	for _, r := range provision.Catalog() {
		if a.Kind != "" && !strings.EqualFold(r.ID, a.Kind) {
			continue
		}
		item := recipe{
			ID: r.ID, Label: r.Label, Blurb: r.Blurb, Image: r.Image,
			Versions: r.Versions, Port: r.Port, Notes: r.Notes,
		}
		for _, f := range r.Fields {
			// 숨긴 칸은 내보내지 않는다. 사람이 정하는 것이 아니라 우리가 두 길로
			// 보내는 값이라(Redis 의 헬스체크용 환경변수), 모델이 그것을 정하려
			// 들면 값이 엇갈린다.
			if f.Hidden {
				continue
			}
			item.Fields = append(item.Fields, field{
				Key: f.Key, Label: f.Label, Kind: string(f.Kind), Default: f.Default,
				Choices: f.Choices, Required: f.Required, Secret: f.Secret, Help: f.Help,
			})
		}
		out = append(out, item)
	}
	if len(out) == 0 {
		if why := provision.NotInCatalog(a.Kind); why != "" {
			return "", fmt.Errorf("%s 는 아직 만들 수 없습니다: %s", a.Kind, why)
		}
		return "", fmt.Errorf("%s 는 모르는 DB 종류입니다", a.Kind)
	}
	return asJSON(map[string]any{
		"recipes":     out,
		"unsupported": provision.NotInCatalogAll(),
	})
}

func toolListDBContainers(tc *toolContext, args json.RawMessage) (string, error) {
	if err := tc.requireDockerTool(); err != nil {
		return "", err
	}
	var a struct {
		Project string `json:"project"`
	}
	_ = json.Unmarshal(args, &a)

	projectID := ""
	if a.Project != "" {
		id, err := tc.findProjectID(a.Project)
		if err != nil {
			return "", err
		}
		projectID = id
	}
	all, err := tc.srv.st.ListDBInstances(tc.ctx, projectID)
	if err != nil {
		return "", err
	}

	type row struct {
		Name       string `json:"name"`
		Kind       string `json:"kind"`
		Version    string `json:"version"`
		Status     string `json:"status"`
		Health     string `json:"health,omitempty"`
		HostPort   int    `json:"hostPort,omitempty"`
		Progress   string `json:"progress,omitempty"`
		Error      string `json:"error,omitempty"`
		Connection bool   `json:"registeredAsConnection"`
	}
	out := []row{}
	for _, in := range all {
		ok, err := tc.canSeeProject(in.ProjectID)
		if err != nil {
			return "", err
		}
		if !ok {
			continue
		}
		out = append(out, row{
			Name: in.Name, Kind: in.Kind, Version: in.Version,
			Status: in.Status, Health: in.Health, HostPort: in.HostPort,
			Progress: in.Progress, Error: in.Error,
			Connection: in.ConnectionID != "",
		})
	}
	return asJSON(map[string]any{"instances": out})
}

func toolDBContainerLogs(tc *toolContext, args json.RawMessage) (string, error) {
	if err := tc.requireDockerTool(); err != nil {
		return "", err
	}
	var a struct {
		Name string `json:"name"`
		Tail int    `json:"tail"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", err
	}
	in, err := tc.findDBInstance(a.Name)
	if err != nil {
		return "", err
	}
	if in.ContainerID == "" {
		return "", fmt.Errorf("%s 에는 컨테이너가 없어 로그를 읽을 수 없습니다 (상태: %s)",
			in.Name, in.Status)
	}
	if a.Tail <= 0 {
		a.Tail = 100
	}
	if a.Tail > 500 {
		a.Tail = 500
	}

	// 따라가지 않는다. 툴은 한 번 답하고 끝나는 것이라, 따라가면 영영 돌아오지 않는다.
	ctx, cancel := context.WithTimeout(tc.ctx, 30*time.Second)
	defer cancel()
	stream, err := tc.srv.provisioner().Logs(ctx, in, docker.LogOptions{
		Tail: a.Tail, Follow: false, Timestamps: true,
	})
	if err != nil {
		return "", err
	}
	defer stream.Close()

	var b strings.Builder
	lines := 0
	for line := range stream.Lines() {
		// 모델에게 주는 것이라 상한을 둔다. 로그 한 줄이 아주 긴 DB 가 있고
		// (질의를 통째로 찍는다), 그것이 대화의 자리를 다 먹으면 정작 물어본
		// 것에 답할 자리가 없다.
		if b.Len() > 24<<10 {
			b.WriteString("… (너무 길어 잘랐습니다)\n")
			break
		}
		b.WriteString(line.Text)
		b.WriteByte('\n')
		lines++
	}
	if lines == 0 {
		return asJSON(map[string]any{
			"name": in.Name, "lines": 0,
			"note": "로그가 비어 있습니다. 컨테이너가 아직 아무것도 남기지 않았습니다",
		})
	}
	return asJSON(map[string]any{"name": in.Name, "lines": lines, "log": b.String()})
}

func toolExportDBCompose(tc *toolContext, args json.RawMessage) (string, error) {
	if err := tc.requireDockerTool(); err != nil {
		return "", err
	}
	var a struct {
		Name    string `json:"name"`
		Project string `json:"project"`
	}
	_ = json.Unmarshal(args, &a)

	var items []provision.ComposeItem
	if a.Name != "" {
		in, err := tc.findDBInstance(a.Name)
		if err != nil {
			return "", err
		}
		item, err := tc.composeItem(in)
		if err != nil {
			return "", err
		}
		items = append(items, item)
	} else {
		projectID := ""
		if a.Project != "" {
			id, err := tc.findProjectID(a.Project)
			if err != nil {
				return "", err
			}
			projectID = id
		}
		all, err := tc.srv.st.ListDBInstances(tc.ctx, projectID)
		if err != nil {
			return "", err
		}
		for _, in := range all {
			if in.Status == store.InstanceRemoved {
				continue
			}
			ok, err := tc.canSeeProject(in.ProjectID)
			if err != nil || !ok {
				continue
			}
			item, err := tc.composeItem(in)
			if err != nil {
				continue
			}
			items = append(items, item)
		}
	}
	if len(items) == 0 {
		return "", fmt.Errorf("내보낼 DB 가 없습니다")
	}
	f := provision.Compose(items)
	return asJSON(map[string]any{
		"yaml": f.YAML, "envExample": f.EnvExample,
		"files": f.Files, "notes": f.Notes, "services": len(items),
	})
}

// composeItem은 툴 쪽의 계획 다시 세우기다(핸들러의 것과 같은 일).
func (tc *toolContext) composeItem(in *store.DBInstance) (provision.ComposeItem, error) {
	values := make(map[string]string, len(in.Values)+2)
	for k, v := range in.Values {
		values[k] = v
	}
	secrets, err := tc.srv.st.DBInstanceSecrets(tc.ctx, in.ID)
	if err != nil {
		return provision.ComposeItem{}, err
	}
	for k, v := range secrets {
		values[k] = v
	}
	plan, err := provision.Build(provision.Spec{
		Kind: in.Kind, Name: in.Name, Version: in.Version, Values: values,
	})
	if err != nil {
		return provision.ComposeItem{}, err
	}
	return provision.ComposeItem{Service: in.Name, Plan: plan, HostPort: in.HostPort}, nil
}

// ---------- 만들기 ----------

type createDBArgs struct {
	Kind     string            `json:"kind"`
	Name     string            `json:"name"`
	Version  string            `json:"version"`
	Project  string            `json:"project"`
	Values   map[string]string `json:"values"`
	Register *bool             `json:"register"`
}

func proposeCreateDBContainer(tc *toolContext, args json.RawMessage) (string, any, error) {
	if err := tc.requireDockerTool(); err != nil {
		return "", nil, err
	}
	a, plan, projectID, err := parseCreateDB(tc, args)
	if err != nil {
		return "", nil, err
	}
	_ = projectID

	summary := fmt.Sprintf("%s(%s)을 %s 로 띄웁니다", a.Name, a.Kind, plan.Image)
	preview := map[string]any{
		"container": plan.Container,
		"image":     plan.Image,
		"volume":    plan.Volume,
		"port":      plan.Port,
		"hostPort":  plan.HostPort,
		"memoryMB":  plan.MemoryMB,
		"cpus":      plan.CPUs,
		"restart":   plan.Restart,
		"warnings":  plan.Warnings,
		"files":     fileNames(plan),
		// 값은 보여주되 비밀은 가린다. 승인하는 사람이 무엇을 만드는지 봐야
		// 하지만, 그 화면과 대화 기록에 비밀번호가 남아서는 안 된다.
		"values": redactValues(plan.Recipe, a.Values),
	}
	return summary, preview, nil
}

func applyCreateDBContainer(tc *toolContext, args json.RawMessage) (string, error) {
	if err := tc.requireDockerTool(); err != nil {
		return "", err
	}
	a, plan, projectID, err := parseCreateDB(tc, args)
	if err != nil {
		return "", err
	}

	values, secrets := splitSecrets(plan.Recipe, a.Values)
	in, err := tc.srv.st.CreateDBInstance(tc.ctx, store.CreateDBInstanceParams{
		ProjectID: projectID, Name: a.Name, Kind: plan.Recipe.ID,
		Image: plan.Image, Version: versionOf(plan.Image),
		ContainerName: plan.Container, VolumeName: plan.Volume,
		Port: plan.Port, Values: values, Secrets: secrets,
		ActorID: tc.user.ID, Actor: tc.user.DisplayName,
	})
	if err != nil {
		return "", err
	}
	tc.audit("dbinstance.created", "dbinstance", in.ID, "ok", map[string]any{
		"name": in.Name, "kind": in.Kind, "image": in.Image, "via": "ai",
	})

	var onReady func(string)
	if a.Register == nil || *a.Register {
		actor := tc.user.ID
		onReady = func(id string) { tc.srv.registerInstance(id, actor, model.EnvDev) }
	}
	if err := tc.srv.provisioner().Create(in.ID, plan, onReady); err != nil {
		return "", err
	}
	// 만들기는 배경에서 돈다. "만들었습니다"라고 답하면 모델이 바로 접속을
	// 시도하고, 아직 뜨지 않은 DB 에서 실패한다.
	return fmt.Sprintf("%s 를 만들기 시작했습니다. 이미지를 내려받는 데 몇 분이 걸릴 수 있습니다 — "+
		"list_db_containers 로 진행 상황을 확인하세요.", in.Name), nil
}

// parseCreateDB는 인자를 읽고 계획까지 세운다(제안과 실행이 같은 것을 보게).
func parseCreateDB(tc *toolContext, args json.RawMessage) (*createDBArgs, *provision.Plan, string, error) {
	var a createDBArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, nil, "", err
	}
	projectID, err := tc.findProjectID(a.Project)
	if err != nil {
		return nil, nil, "", err
	}
	plan, err := provision.Build(provision.Spec{
		Kind: a.Kind, Name: strings.TrimSpace(a.Name),
		Version: a.Version, Values: a.Values,
	})
	if err != nil {
		// 칸 이름을 지어낸 경우가 대부분이다. 어디를 봐야 하는지 알려 준다.
		return nil, nil, "", fmt.Errorf("%w (describe_db_catalog 로 칸 이름을 확인하세요)", err)
	}
	return &a, plan, projectID, nil
}

// redactValues는 비밀 칸을 가린 사본을 만든다.
func redactValues(r *provision.Recipe, values map[string]string) map[string]string {
	out := make(map[string]string, len(values))
	for k, v := range values {
		if f := r.Field(k); f != nil && f.Secret {
			out[k] = "(가림)"
			continue
		}
		out[k] = v
	}
	return out
}

// ---------- 실행·중단 ----------

func proposeControlDBContainer(tc *toolContext, args json.RawMessage) (string, any, error) {
	if err := tc.requireDockerTool(); err != nil {
		return "", nil, err
	}
	in, action, err := parseControlDB(tc, args)
	if err != nil {
		return "", nil, err
	}
	verb := map[string]string{"start": "시작", "stop": "중단", "restart": "다시 시작"}[action]
	summary := fmt.Sprintf("%s 를 %s합니다", in.Name, verb)
	return summary, map[string]any{
		"name": in.Name, "container": in.ContainerName,
		"status": in.Status, "action": action,
		// 붙어 있는 커넥션이 있으면 말한다. 멈추는 것이 무엇을 끊는지 사람이
		// 알아야 승인할 수 있다.
		"registeredAsConnection": in.ConnectionID != "",
	}, nil
}

func applyControlDBContainer(tc *toolContext, args json.RawMessage) (string, error) {
	if err := tc.requireDockerTool(); err != nil {
		return "", err
	}
	in, action, err := parseControlDB(tc, args)
	if err != nil {
		return "", err
	}
	if tc.srv.provisioner().Busy(in.ID) {
		return "", fmt.Errorf("%s 는 아직 만들고 있습니다", in.Name)
	}

	ctx, cancel := context.WithTimeout(tc.ctx, tc.srv.cfg.DockerTimeout+30*time.Second)
	defer cancel()
	switch action {
	case "start":
		err = tc.srv.provisioner().Start(ctx, in)
	case "stop":
		err = tc.srv.provisioner().Stop(ctx, in)
	case "restart":
		err = tc.srv.provisioner().Restart(ctx, in)
	}
	if err != nil {
		return "", err
	}
	tc.audit("dbinstance."+action+"ed", "dbinstance", in.ID, "ok",
		map[string]any{"name": in.Name, "via": "ai"})

	fresh, err := tc.srv.st.GetDBInstance(tc.ctx, in.ID)
	if err != nil {
		return "", err
	}
	// 시작·다시 시작하면 도커가 호스트 포트를 새로 고른다. 화면과 같은 자리에서
	// 커넥션 주소를 맞춘다 — 여기서 빠뜨리면 어시스턴트로 다시 시작한 DB 만
	// 붙지 않게 된다.
	tc.srv.syncConnectionAddress(tc.ctx, fresh)
	return fmt.Sprintf("%s: %s (포트 %s)", fresh.Name, fresh.Status, orNone(fresh.HostPort)), nil
}

func parseControlDB(tc *toolContext, args json.RawMessage) (*store.DBInstance, string, error) {
	var a struct {
		Name   string `json:"name"`
		Action string `json:"action"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, "", err
	}
	action := strings.ToLower(strings.TrimSpace(a.Action))
	switch action {
	case "start", "stop", "restart":
	default:
		return nil, "", fmt.Errorf("action 은 start, stop, restart 중 하나여야 합니다")
	}
	in, err := tc.findDBInstance(a.Name)
	if err != nil {
		return nil, "", err
	}
	return in, action, nil
}

// ---------- 지우기 ----------

func proposeRemoveDBContainer(tc *toolContext, args json.RawMessage) (string, any, error) {
	if err := tc.requireDockerTool(); err != nil {
		return "", nil, err
	}
	in, dropData, err := parseRemoveDB(tc, args)
	if err != nil {
		return "", nil, err
	}
	summary := fmt.Sprintf("%s 컨테이너를 지웁니다", in.Name)
	if dropData && in.VolumeName != "" {
		summary += " (데이터도 함께 지웁니다 — 되돌릴 수 없습니다)"
	}
	return summary, map[string]any{
		"name": in.Name, "container": in.ContainerName,
		"volume": in.VolumeName, "dropData": dropData,
		"status":                 in.Status,
		"registeredAsConnection": in.ConnectionID != "",
		"note":                   "등록된 커넥션은 함께 지워지지 않습니다",
	}, nil
}

func applyRemoveDBContainer(tc *toolContext, args json.RawMessage) (string, error) {
	if err := tc.requireDockerTool(); err != nil {
		return "", err
	}
	in, dropData, err := parseRemoveDB(tc, args)
	if err != nil {
		return "", err
	}
	if tc.srv.provisioner().Busy(in.ID) {
		return "", fmt.Errorf("%s 는 아직 만들고 있습니다", in.Name)
	}

	ctx, cancel := context.WithTimeout(tc.ctx, tc.srv.cfg.DockerTimeout+30*time.Second)
	defer cancel()
	if err := tc.srv.provisioner().Remove(ctx, in, !dropData); err != nil {
		return "", err
	}
	tc.audit("dbinstance.removed", "dbinstance", in.ID, "ok", map[string]any{
		"name": in.Name, "dataKept": !dropData, "via": "ai",
	})
	if dropData {
		return fmt.Sprintf("%s 와 그 데이터를 지웠습니다", in.Name), nil
	}
	return fmt.Sprintf("%s 를 지웠습니다. 데이터 볼륨(%s)은 남겼습니다",
		in.Name, orDash(in.VolumeName)), nil
}

func parseRemoveDB(tc *toolContext, args json.RawMessage) (*store.DBInstance, bool, error) {
	var a struct {
		Name     string `json:"name"`
		DropData bool   `json:"dropData"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, false, err
	}
	in, err := tc.findDBInstance(a.Name)
	if err != nil {
		return nil, false, err
	}
	return in, a.DropData, nil
}

func orNone(port int) string {
	if port <= 0 {
		return "없음"
	}
	return fmt.Sprintf("%d", port)
}

func orDash(v string) string {
	if v == "" {
		return "없음"
	}
	return v
}
