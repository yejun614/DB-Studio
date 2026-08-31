package api

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"

	"dbstudio/internal/dbx"
	"dbstudio/internal/model"
	"dbstudio/internal/store"
)

// 연결 테스트 상한. 사용자가 무한정 기다리지 않도록 컨텍스트에 붙인다.
const connTestTimeout = 15 * time.Second

// handleListConnections는 접근 가능한 커넥션만 반환한다.
// 관리자는 전체를, 멤버는 정책이 허용한 것만 본다.
func (s *Server) handleListConnections(c *fiber.Ctx) error {
	u := currentUser(c)
	all, err := s.st.ListConnections(c.Context())
	if err != nil {
		return err
	}
	// 프로젝트로 먼저 좁힌다. ?project= 가 있으면 그 하나로, 없으면 볼 수 있는
	// 전체로. nil은 제한 없음(슈퍼 어드민)이다.
	scope, err := s.projectFilter(c)
	if err != nil {
		return err
	}

	effective, err := s.authz.EffectiveAccessList(c.Context(), u, all)
	if err != nil {
		return err
	}
	levels := make(map[string]model.Level, len(effective))
	caps := make(map[string][]model.Capability, len(effective))
	for _, e := range effective {
		if e.Accessible {
			levels[e.ConnectionID] = e.Level
			caps[e.ConnectionID] = e.Caps
		}
	}

	// 커넥션 관리자는 접근 등급이 없는 커넥션도 목록에서 볼 수 있어야 관리가 가능하다.
	// 대신 등급 정보를 함께 내려보내 UI가 기능 버튼을 제한한다.
	canManage := u.Role.CanManageConnections()
	items := make([]fiber.Map, 0, len(all))
	for _, conn := range all {
		// 관리자 예외도 프로젝트 밖으로는 나가지 않는다. 이 관문이 없으면 참여하지
		// 않은 프로젝트의 DB 이름이 관리자 화면에 그대로 뜬다 — 프로젝트를 나눈
		// 이유가 목록 한 곳에서 무너진다.
		if !inProjects(scope, conn.ProjectID) {
			continue
		}
		level, accessible := levels[conn.ID]
		if !accessible && !canManage {
			continue
		}
		list := caps[conn.ID]
		if list == nil {
			list = []model.Capability{}
		}
		items = append(items, fiber.Map{
			"connection": conn,
			"level":      level,
			"accessible": accessible,
			// 데이터 능력은 등급과 다른 축이므로 따로 내려보낸다.
			// 데이터·SQL 화면이 커넥션 선택 목록을 이 값으로 거른다.
			"caps":     list,
			"dataCaps": dbx.DataCapsFor(conn.Kind),
		})
	}
	return c.JSON(fiber.Map{"items": items, "canManage": canManage})
}

func (s *Server) handleGetConnection(c *fiber.Ctx) error {
	id := c.Params("id")
	u := currentUser(c)

	conn, err := s.st.GetConnection(c.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		return fail(c, fiber.StatusNotFound, "not_found", "커넥션을 찾을 수 없습니다")
	}
	if err != nil {
		return err
	}

	d, err := s.requireLevel(c, id, model.LevelMonitor)
	if err != nil {
		return err
	}
	if !d.Allowed && !u.Role.CanManageConnections() {
		return fail(c, fiber.StatusForbidden, "forbidden", d.Reason)
	}

	adapter, adapterErr := dbx.Get(conn.Kind)
	resp := fiber.Map{"connection": conn, "level": d.Level, "accessible": d.Allowed}
	if adapterErr == nil {
		resp["capabilities"] = adapter.Capabilities()
		resp["target"] = adapter.Redacted(dbx.Target{Conn: conn})
	}
	return c.JSON(resp)
}

// connectionRequest는 커넥션(관리 대상 DB) 생성/수정 입력이다.
//
// 접속 정보 필드(kind·host·port·options·자격증명)가 아직 남아 있는 이유:
// ServerID 없이 오면 "서버 + DB 하나"를 한 번에 만든다. 서버를 먼저 만들고 DB를 고르는
// 흐름이 정식이지만, DB 하나만 등록하려는 사람에게 두 단계를 강요할 이유는 없다.
type connectionRequest struct {
	// ProjectID는 이 DB가 속할 프로젝트다. 새로 만들 때는 반드시 있어야 한다.
	ProjectID    string            `json:"projectId"`
	ServerID     string            `json:"serverId"`
	Name         string            `json:"name"`
	Kind         model.DBKind      `json:"kind"`
	Environment  model.Environment `json:"environment"`
	Host         string            `json:"host"`
	Port         int               `json:"port"`
	DatabaseName string            `json:"databaseName"`
	Options      model.Options     `json:"options"`
	Tags         []string          `json:"tags"`
	Note         string            `json:"note"`
	Enabled      *bool             `json:"enabled"`
	Username     string            `json:"username"`
	Password     *string           `json:"password"`
	Extra        map[string]string `json:"extra"`
	// NodeID는 이 DB에 접속할 클러스터 노드다. 클러스터가 아니면 쓰이지 않는다.
	NodeID string `json:"nodeId"`
}

// dbParams는 DB(커넥션) 자체의 속성만 뽑는다.
func (r *connectionRequest) dbParams(actorID string) (store.SaveConnectionParams, error) {
	var p store.SaveConnectionParams
	name := strings.TrimSpace(r.Name)
	if name == "" {
		return p, errors.New("커넥션 이름을 입력하세요")
	}
	if !r.Environment.Valid() {
		return p, errors.New("환경은 dev 또는 prod여야 합니다")
	}
	if r.Tags == nil {
		r.Tags = []string{}
	}
	return store.SaveConnectionParams{
		ProjectID:    strings.TrimSpace(r.ProjectID),
		ServerID:     strings.TrimSpace(r.ServerID),
		Name:         name,
		Environment:  r.Environment,
		DatabaseName: strings.TrimSpace(r.DatabaseName),
		Tags:         r.Tags,
		Note:         r.Note,
		Enabled:      r.Enabled == nil || *r.Enabled,
		NodeID:       strings.TrimSpace(r.NodeID),
		ActorID:      actorID,
	}, nil
}

// serverPart는 같은 요청에서 서버 쪽 입력을 뽑는다.
func (r *connectionRequest) serverPart() serverRequest {
	return serverRequest{
		Name: r.Name, Kind: r.Kind, Host: r.Host, Port: r.Port,
		Options: r.Options, DefaultEnvironment: r.Environment,
		Tags: r.Tags, Enabled: r.Enabled,
		Username: r.Username, Password: r.Password, Extra: r.Extra,
	}
}

func (s *Server) handleCreateConnection(c *fiber.Ctx) error {
	var req connectionRequest
	if err := c.BodyParser(&req); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "요청 형식이 올바르지 않습니다")
	}
	actor := currentUser(c)
	params, err := req.dbParams(actor.ID)
	if err != nil {
		return fail(c, fiber.StatusBadRequest, "invalid_connection", err.Error())
	}
	// 프로젝트는 반드시 있고, 자기가 볼 수 있는 곳이어야 한다.
	//
	// 어디에도 속하지 않은 커넥션은 목록에도 권한 판정에도 나타나지 않는다 — 만들
	// 수는 있는데 아무도 볼 수 없는 유령이 된다. 참여하지 않은 프로젝트에 넣는 것도
	// 막는다: 그렇게 만든 DB는 만든 사람 자신에게도 보이지 않는다.
	if _, perr := s.requireProject(c, params.ProjectID); perr != nil {
		return perr
	}

	// 서버를 지정했으면 그 아래에 DB만 더한다. 접속 정보는 서버의 것을 쓴다.
	if params.ServerID != "" {
		srv, err := s.st.GetServer(c.Context(), params.ServerID)
		if errors.Is(err, store.ErrNotFound) {
			return fail(c, fiber.StatusNotFound, "not_found", "서버를 찾을 수 없습니다")
		}
		if err != nil {
			return err
		}
		conn, err := s.st.CreateConnection(c.Context(), params)
		if errors.Is(err, store.ErrDuplicateName) {
			return fail(c, fiber.StatusConflict, "duplicate", "같은 이름이나 같은 DB가 이미 등록되어 있습니다")
		}
		if err != nil {
			return err
		}
		s.audit(c, store.AuditParams{
			Action: store.ActionConnCreated, TargetType: "connection", TargetID: conn.ID,
			Detail: map[string]any{
				"name": conn.Name, "kind": conn.Kind,
				"environment": conn.Environment, "server": srv.Name,
			},
		})
		s.monitor.TriggerPoll(conn.ID)
		return c.Status(fiber.StatusCreated).JSON(fiber.Map{"connection": conn})
	}

	// 서버가 없으면 함께 만든다.
	sreq := req.serverPart()
	sp, adapter, err := sreq.toParams(actor.ID)
	if err != nil {
		return fail(c, fiber.StatusBadRequest, "invalid_connection", err.Error())
	}
	if err := validateServerTarget(adapter, sp, params.DatabaseName); err != nil {
		return fail(c, fiber.StatusBadRequest, "invalid_connection", err.Error())
	}

	srv, conn, err := s.st.CreateServerWithDatabase(c.Context(), sp, params)
	if errors.Is(err, store.ErrDuplicateName) {
		return fail(c, fiber.StatusConflict, "duplicate", "이미 같은 이름의 커넥션이 있습니다")
	}
	if err != nil {
		return err
	}
	s.audit(c, store.AuditParams{
		Action: store.ActionConnCreated, TargetType: "connection", TargetID: conn.ID,
		Detail: map[string]any{
			"name": conn.Name, "kind": conn.Kind,
			"environment": conn.Environment, "server": srv.Name, "serverCreated": true,
		},
	})
	s.monitor.TriggerPoll(conn.ID)
	return c.Status(fiber.StatusCreated).JSON(fiber.Map{"connection": conn, "server": srv})
}

// handleUpdateConnection은 DB의 속성을 바꾼다.
//
// 접속 정보(host·port·자격증명)는 여기서 바꾸지 않는다. 그것은 서버의 것이고,
// 이 DB의 형제들이 함께 쓰는 값이다. 다만 서버에 DB가 이 하나뿐이면 예전과 구분이
// 없으므로 그대로 받아 서버에 반영한다 — 그 경우 "이 커넥션을 고친다"와
// "이 서버를 고친다"는 실제로 같은 일이다.
func (s *Server) handleUpdateConnection(c *fiber.Ctx) error {
	id := c.Params("id")
	var req connectionRequest
	if err := c.BodyParser(&req); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "요청 형식이 올바르지 않습니다")
	}
	actor := currentUser(c)
	params, err := req.dbParams(actor.ID)
	if err != nil {
		return fail(c, fiber.StatusBadRequest, "invalid_connection", err.Error())
	}

	existing, err := s.st.GetConnection(c.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		return fail(c, fiber.StatusNotFound, "not_found", "커넥션을 찾을 수 없습니다")
	}
	if err != nil {
		return err
	}
	srv, err := s.st.GetServer(c.Context(), existing.ServerID)
	if err != nil {
		return err
	}

	if wantsServerChange(&req, existing) {
		if srv.DatabaseCount > 1 {
			return failDetail(c, fiber.StatusConflict, "server_shared",
				fmt.Sprintf("접속 정보는 서버 %s의 DB %d개가 함께 씁니다. 서버 설정에서 바꾸세요",
					srv.Name, srv.DatabaseCount), srv.ID)
		}
		sreq := req.serverPart()
		sreq.Name = srv.Name // 서버 이름까지 따라 바뀌면 목록에서 서버를 잃어버린다.
		sp, adapter, err := sreq.toParams(actor.ID)
		if err != nil {
			return fail(c, fiber.StatusBadRequest, "invalid_connection", err.Error())
		}
		if err := validateServerTarget(adapter, sp, params.DatabaseName); err != nil {
			return fail(c, fiber.StatusBadRequest, "invalid_connection", err.Error())
		}
		if _, err := s.st.UpdateServer(c.Context(), srv.ID, sp); err != nil {
			return err
		}
	}

	conn, err := s.st.UpdateConnection(c.Context(), id, params)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return fail(c, fiber.StatusNotFound, "not_found", "커넥션을 찾을 수 없습니다")
	case errors.Is(err, store.ErrDuplicateName):
		return fail(c, fiber.StatusConflict, "duplicate", "이미 같은 이름의 커넥션이 있습니다")
	case err != nil:
		return err
	}
	s.audit(c, store.AuditParams{
		Action: store.ActionConnUpdated, TargetType: "connection", TargetID: id,
		Detail: map[string]any{
			"name": conn.Name, "kind": conn.Kind, "environment": conn.Environment,
			"passwordChanged": req.Password != nil,
		},
	})
	// 접속 정보가 바뀌었으므로 즉시 재수집해 상태를 갱신한다.
	s.monitor.TriggerPoll(conn.ID)
	return c.JSON(fiber.Map{"connection": conn})
}

// wantsServerChange는 요청이 서버 쪽 값을 실제로 바꾸려 하는지 본다.
//
// **빈 값은 "바꾸지 않겠다"는 뜻으로 읽는다.** DB만 고치는 화면은 접속 정보를 아예
// 보내지 않으므로, 빈 문자열을 "호스트를 지우려 한다"로 해석하면 이름만 바꾸려던
// 요청이 서버 수정으로 번지고 종류가 비어 있어 400으로 거절된다.
// 접속 정보를 실제로 지우는 일은 없으므로(호스트 없는 서버는 접속할 수 없다)
// 이 해석으로 잃는 것이 없다.
func wantsServerChange(r *connectionRequest, existing *model.Connection) bool {
	if r.Password != nil {
		return true
	}
	if r.Kind != "" && r.Kind != existing.Kind {
		return true
	}
	if host := strings.TrimSpace(r.Host); host != "" && host != existing.Host {
		return true
	}
	if r.Port != 0 && r.Port != existing.Port {
		return true
	}
	if user := strings.TrimSpace(r.Username); user != "" && user != existing.Username {
		return true
	}
	if len(r.Options) > 0 && !maps.Equal(r.Options, existing.Options) {
		return true
	}
	return len(r.Extra) > 0
}

// impactLabels는 삭제 영향 항목을 사람이 읽는 말로 바꾼다.
// 화면이 아니라 여기서 붙이는 이유: 세는 대상이 늘면 store와 이 표를 함께 고치게 되고,
// 둘 중 하나만 고치면 컴파일이 아니라 "빈 이름"으로 조용히 새어 나간다.
var impactLabels = map[string]string{
	"erd":       "ERD 문서",
	"migration": "마이그레이션",
	"version":   "스키마 버전",
	"snapshot":  "스키마 스냅샷",
	"rule":      "감시 룰",
	"event":     "이벤트",
	"trigger":   "자동 실행 트리거",
	"vcs":       "Git 연동",
	"metric":    "수집된 지표",
	"access":    "사용자 권한 지정",
	"backup":    "백업 기록",
	"ai":        "AI 대화",
}

// handleConnectionImpact는 이 커넥션을 지웠을 때 무엇이 함께 사라지는지 알린다.
// 삭제 대화상자가 확인을 받기 전에 부른다.
func (s *Server) handleConnectionImpact(c *fiber.Ctx) error {
	id := c.Params("id")
	conn, err := s.st.GetConnection(c.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		return fail(c, fiber.StatusNotFound, "not_found", "커넥션을 찾을 수 없습니다")
	}
	if err != nil {
		return err
	}
	items, err := s.st.ConnectionImpact(c.Context(), id)
	if err != nil {
		return err
	}
	out := make([]fiber.Map, 0, len(items))
	for _, it := range items {
		label := impactLabels[it.Key]
		if label == "" {
			label = it.Key
		}
		out = append(out, fiber.Map{
			"key": it.Key, "label": label, "count": it.Count, "kept": it.Kept,
		})
	}
	return c.JSON(fiber.Map{
		"connection":  connSummary(conn),
		"items":       out,
		"confirmName": conn.Environment == model.EnvProd,
	})
}

func (s *Server) handleDeleteConnection(c *fiber.Ctx) error {
	id := c.Params("id")
	conn, err := s.st.GetConnection(c.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		return fail(c, fiber.StatusNotFound, "not_found", "커넥션을 찾을 수 없습니다")
	}
	if err != nil {
		return err
	}
	// 운영 DB 삭제는 이름 확인을 요구한다. 실수로 지우는 것을 막기 위한 안전장치다.
	if conn.Environment == model.EnvProd && c.Query("confirm") != conn.Name {
		return failDetail(c, fiber.StatusBadRequest, "confirm_required",
			"운영 DB를 삭제하려면 커넥션 이름을 확인 값으로 전달해야 합니다", conn.Name)
	}

	if err := s.st.DeleteConnection(c.Context(), id); err != nil {
		return err
	}
	// 룰 엔진이 들고 있던 위반 지속 상태를 정리한다.
	// 같은 ID가 재사용될 일은 없지만, 남겨두면 메모리가 계속 늘어난다.
	s.monitor.Engine().ForgetConnection(id)
	s.audit(c, store.AuditParams{
		Action: store.ActionConnDeleted, TargetType: "connection", TargetID: id,
		Detail: map[string]any{"name": conn.Name, "kind": conn.Kind, "environment": conn.Environment},
	})
	return c.JSON(fiber.Map{"ok": true})
}

// handleTestConnection은 저장된 커넥션으로 실제 접속을 시도한다.
// 모니터링 등급 이상이 필요하다.
func (s *Server) handleTestConnection(c *fiber.Ctx) error {
	id := c.Params("id")
	u := currentUser(c)

	conn, err := s.st.GetConnection(c.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		return fail(c, fiber.StatusNotFound, "not_found", "커넥션을 찾을 수 없습니다")
	}
	if err != nil {
		return err
	}
	d, err := s.requireLevel(c, id, model.LevelMonitor)
	if err != nil {
		return err
	}
	if !d.Allowed && !u.Role.CanManageConnections() {
		return fail(c, fiber.StatusForbidden, "forbidden", d.Reason)
	}

	secret, err := s.st.GetSecret(c.Context(), id)
	if err != nil {
		return err
	}
	adapter, err := dbx.Get(conn.Kind)
	if err != nil {
		return fail(c, fiber.StatusBadRequest, "unsupported_kind", err.Error())
	}

	ctx, cancel := context.WithTimeout(c.Context(), connTestTimeout)
	defer cancel()

	info, pingErr := adapter.Ping(ctx, dbx.Target{Conn: conn, Secret: secret})
	ok := pingErr == nil
	msg := ""
	if pingErr != nil {
		msg = pingErr.Error()
	}
	if err := s.st.RecordConnectionCheck(c.Context(), id, ok, msg); err != nil {
		return err
	}
	s.audit(c, store.AuditParams{
		Action: store.ActionConnTested, TargetType: "connection", TargetID: id,
		Result: resultOf(ok),
		Detail: map[string]any{"name": conn.Name, "ok": ok, "message": msg},
	})

	if !ok {
		return c.JSON(fiber.Map{"ok": false, "message": msg})
	}
	return c.JSON(fiber.Map{"ok": true, "server": info})
}

// handleTestAdhoc은 저장 전에 입력한 접속 정보를 검증한다. 커넥션 관리자만 사용할 수 있다.
func (s *Server) handleTestAdhoc(c *fiber.Ctx) error {
	var req connectionRequest
	if err := c.BodyParser(&req); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "요청 형식이 올바르지 않습니다")
	}
	// 이름은 임시 테스트에서 의미가 없으므로 비어 있어도 통과시킨다.
	if strings.TrimSpace(req.Name) == "" {
		req.Name = "(임시 테스트)"
	}
	if req.Environment == "" {
		req.Environment = model.EnvDev
	}
	actor := currentUser(c)
	sreq := req.serverPart()
	sreq.Name = req.Name
	params, adapter, err := sreq.toParams(actor.ID)
	if err != nil {
		return fail(c, fiber.StatusBadRequest, "invalid_connection", err.Error())
	}

	// 비밀번호를 비워둔 채 테스트하는 두 경우를 저장된 값으로 채운다:
	// 기존 커넥션 수정(id)과 기존 서버에 DB 추가(serverId).
	// 채우지 않으면 "비밀번호를 다시 입력해야만 테스트할 수 있다"가 되는데,
	// 그러면 테스트하려고 비밀번호를 다시 치다가 오타가 나는 쪽이 더 흔하다.
	password := ""
	switch {
	case params.Password != nil:
		password = *params.Password
	case strings.TrimSpace(c.Query("id")) != "":
		secret, err := s.st.GetSecret(c.Context(), strings.TrimSpace(c.Query("id")))
		if err != nil {
			return err
		}
		password = secret.Password
	case strings.TrimSpace(req.ServerID) != "":
		secret, err := s.st.GetServerSecret(c.Context(), strings.TrimSpace(req.ServerID))
		if err != nil {
			return err
		}
		password = secret.Password
		if params.Username == "" {
			params.Username = secret.Username
		}
	}

	probe := &model.Connection{
		Name: params.Name, Kind: params.Kind, Environment: req.Environment,
		Host: params.Host, Port: params.Port,
		DatabaseName: strings.TrimSpace(req.DatabaseName),
		Options:      params.Options,
	}
	target := dbx.Target{
		Conn:   probe,
		Secret: &model.Secret{Username: params.Username, Password: password, Extra: params.Extra},
	}

	ctx, cancel := context.WithTimeout(c.Context(), connTestTimeout)
	defer cancel()

	info, pingErr := adapter.Ping(ctx, target)
	ok := pingErr == nil
	msg := ""
	if pingErr != nil {
		msg = pingErr.Error()
	}
	s.audit(c, store.AuditParams{
		Action: store.ActionConnTested, TargetType: "connection", TargetID: "",
		Result: resultOf(ok),
		Detail: map[string]any{"adhoc": true, "target": adapter.Redacted(target), "ok": ok, "message": msg},
	})

	if !ok {
		return c.JSON(fiber.Map{"ok": false, "message": msg, "target": adapter.Redacted(target)})
	}
	return c.JSON(fiber.Map{"ok": true, "server": info, "target": adapter.Redacted(target)})
}

func resultOf(ok bool) string {
	if ok {
		return "ok"
	}
	return "error"
}
