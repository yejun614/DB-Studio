package api

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/gofiber/fiber/v2"

	"dbstudio/internal/dbx"
	"dbstudio/internal/model"
	"dbstudio/internal/store"
)

// DB 서버 API.
//
// 서버는 접속 정보와 자격증명을 갖고, 그 아래에 관리 대상 DB(커넥션)가 달린다.
// 커넥션 API는 그대로 살아 있으며(다운스트림이 전부 커넥션을 가리킨다), 여기서는
// "서버를 만들고 → DB 목록을 읽어 → 여러 개를 한 번에 등록한다"를 담당한다.

// serverRequest는 서버 생성/수정 입력이다.
// Password가 nil이면(수정 시) 기존 비밀번호를 유지한다.
type serverRequest struct {
	// ProjectID는 이 서버가 속할 프로젝트다. 만들 때 반드시 있어야 한다.
	ProjectID          string            `json:"projectId"`
	Name               string            `json:"name"`
	Kind               model.DBKind      `json:"kind"`
	Host               string            `json:"host"`
	Port               int               `json:"port"`
	Options            model.Options     `json:"options"`
	DefaultEnvironment model.Environment `json:"defaultEnvironment"`
	Tags               []string          `json:"tags"`
	Note               string            `json:"note"`
	Enabled            *bool             `json:"enabled"`
	Username           string            `json:"username"`
	Password           *string           `json:"password"`
	Extra              map[string]string `json:"extra"`
}

func (r *serverRequest) toParams(actorID string) (store.SaveServerParams, dbx.Adapter, error) {
	var p store.SaveServerParams

	r.Name = strings.TrimSpace(r.Name)
	if r.Name == "" {
		return p, nil, errors.New("서버 이름을 입력하세요")
	}
	if !r.Kind.Valid() {
		return p, nil, errors.New("알 수 없는 데이터베이스 종류입니다")
	}
	if r.DefaultEnvironment == "" {
		r.DefaultEnvironment = model.EnvDev
	}
	if !r.DefaultEnvironment.Valid() {
		return p, nil, errors.New("환경은 dev 또는 prod여야 합니다")
	}
	adapter, err := dbx.Get(r.Kind)
	if err != nil {
		return p, nil, err
	}

	port := r.Port
	if port == 0 {
		port = adapter.DefaultPort()
	}
	if r.Options == nil {
		r.Options = model.Options{}
	}
	if r.Tags == nil {
		r.Tags = []string{}
	}
	p = store.SaveServerParams{
		ProjectID: strings.TrimSpace(r.ProjectID),
		Name:      r.Name, Kind: r.Kind,
		Host: strings.TrimSpace(r.Host), Port: port,
		Options: r.Options, DefaultEnvironment: r.DefaultEnvironment,
		Tags: r.Tags, Note: r.Note,
		Enabled:  r.Enabled == nil || *r.Enabled,
		Username: strings.TrimSpace(r.Username), Password: r.Password, Extra: r.Extra,
		ActorID: actorID,
	}
	return p, adapter, nil
}

// validateServerTarget은 형식 검증만 한다. 네트워크 접근은 하지 않는다.
func validateServerTarget(adapter dbx.Adapter, p store.SaveServerParams, database string) error {
	pw := ""
	if p.Password != nil {
		pw = *p.Password
	}
	probe := &model.Connection{
		Kind: p.Kind, Host: p.Host, Port: p.Port,
		DatabaseName: database, Options: p.Options,
	}
	return adapter.Validate(dbx.Target{
		Conn:   probe,
		Secret: &model.Secret{Username: p.Username, Password: pw},
	})
}

// requireServer는 서버를 읽고 그 프로젝트를 볼 수 있는 사람인지 확인한다.
//
// "없음"과 "볼 수 없음"에 같은 404를 주는 이유는 프로젝트와 같다: 403은 그 자리에
// 무언가 있다는 사실을 알려 주는데, 서버 이름과 호스트는 그 자체로 알려서는 안 되는
// 것일 수 있다.
func (s *Server) requireServer(c *fiber.Ctx, id string) (*model.Server, error) {
	srv, err := s.st.GetServer(c.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, fiber.NewError(fiber.StatusNotFound, "서버를 찾을 수 없습니다")
	}
	if err != nil {
		return nil, err
	}
	ok, err := s.canSeeProject(c, srv.ProjectID)
	if err != nil {
		return nil, err
	}
	if !ok {
		s.auditDenied(c, "server.denied", srv.ID)
		return nil, fiber.NewError(fiber.StatusNotFound, "서버를 찾을 수 없습니다")
	}
	return srv, nil
}

// handleListServers는 서버와 그 아래 DB를 함께 반환한다.
//
// 두 번 부르게 하지 않는 이유: 화면이 항상 둘을 같이 그린다(서버 줄을 펴면 DB 목록).
// 나눠 두면 목록 화면이 N+1 요청을 하게 된다.
func (s *Server) handleListServers(c *fiber.Ctx) error {
	u := currentUser(c)
	// 서버 목록도 프로젝트로 좁힌다. 예전에는 서버가 프로젝트 밖이라 프로젝트를
	// 바꿔도 남의 팀 서버가 호스트 이름까지 그대로 떠 있었다.
	scope, err := s.projectFilter(c)
	if err != nil {
		return err
	}
	servers, err := s.st.ListServers(c.Context(), scope)
	if err != nil {
		return err
	}
	conns, err := s.st.ListConnections(c.Context())
	if err != nil {
		return err
	}
	effective, err := s.authz.EffectiveAccessList(c.Context(), u, conns)
	if err != nil {
		return err
	}
	access := make(map[string]model.EffectiveAccess, len(effective))
	for _, e := range effective {
		access[e.ConnectionID] = e
	}

	canManage := u.Role.CanManageConnections()
	byServer := map[string][]fiber.Map{}
	for _, conn := range conns {
		if !inProjects(scope, conn.ProjectID) {
			continue
		}
		e := access[conn.ID]
		if !e.Accessible && !canManage {
			continue
		}
		caps := e.Caps
		if caps == nil {
			caps = []model.Capability{}
		}
		byServer[conn.ServerID] = append(byServer[conn.ServerID], fiber.Map{
			"connection": conn, "level": e.Level, "accessible": e.Accessible,
			"caps": caps, "dataCaps": dbx.DataCapsFor(conn.Kind),
		})
	}

	items := make([]fiber.Map, 0, len(servers))
	for _, srv := range servers {
		dbs := byServer[srv.ID]
		// 접근 가능한 DB가 하나도 없는 서버는 관리자에게만 보인다.
		// 볼 수 없는 DB만 있는 서버를 보여주면 이름과 호스트가 그대로 노출된다.
		if len(dbs) == 0 && !canManage {
			continue
		}
		if dbs == nil {
			dbs = []fiber.Map{}
		}
		item := fiber.Map{"server": srv, "databases": dbs}
		if a, err := dbx.Get(srv.Kind); err == nil {
			item["capabilities"] = a.Capabilities()
			item["canListDatabases"] = dbx.CanListDatabases(srv.Kind)
		}
		items = append(items, item)
	}
	return c.JSON(fiber.Map{"items": items, "canManage": canManage})
}

func (s *Server) handleGetServer(c *fiber.Ctx) error {
	srv, err := s.requireServer(c, c.Params("id"))
	if err != nil {
		return err
	}
	conns, err := s.st.ListConnectionsByServer(c.Context(), srv.ID)
	if err != nil {
		return err
	}
	resp := fiber.Map{"server": srv, "databases": conns,
		"canListDatabases": dbx.CanListDatabases(srv.Kind)}
	if a, err := dbx.Get(srv.Kind); err == nil {
		resp["capabilities"] = a.Capabilities()
	}
	return c.JSON(resp)
}

func (s *Server) handleCreateServer(c *fiber.Ctx) error {
	var req serverRequest
	if err := c.BodyParser(&req); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "요청 형식이 올바르지 않습니다")
	}
	actor := currentUser(c)
	params, adapter, err := req.toParams(actor.ID)
	if err != nil {
		return fail(c, fiber.StatusBadRequest, "invalid_server", err.Error())
	}
	// 목록을 읽을 수 없는 종류(Oracle·SQLite)는 대상 없이 서버만 만들 수 없다.
	// Oracle의 서비스명과 SQLite의 파일 경로는 접속 정보의 일부이자 대상 그 자체라서,
	// 그것 없이는 검증할 것도 나중에 고를 것도 없다. 드라이버의 "서비스명을 입력하세요"를
	// 그대로 보여주면 어느 칸을 채워야 하는지 알 수 없으므로 여기서 되돌린다.
	if !dbx.CanListDatabases(params.Kind) {
		return fail(c, fiber.StatusBadRequest, "needs_first_database",
			kindNeedsFirstDatabase(params.Kind))
	}
	// 프로젝트는 반드시 있고, 자기가 볼 수 있는 곳이어야 한다. 어디에도 속하지 않은
	// 서버는 목록에 뜨지 않으므로 만든 사람조차 찾지 못한다.
	if _, perr := s.requireProject(c, params.ProjectID); perr != nil {
		return perr
	}
	// 대상 DB 없이도 접속 정보 자체는 검증할 수 있어야 한다.
	// 종류별 부트스트랩 DB를 넣어 형식만 본다.
	if err := validateServerTarget(adapter, params, dbx.BootstrapDatabase(params.Kind)); err != nil {
		return fail(c, fiber.StatusBadRequest, "invalid_server", err.Error())
	}

	srv, err := s.st.CreateServer(c.Context(), params)
	if errors.Is(err, store.ErrDuplicateName) {
		return fail(c, fiber.StatusConflict, "duplicate", "이미 같은 이름의 서버가 있습니다")
	}
	if err != nil {
		return err
	}
	s.audit(c, store.AuditParams{
		Action: store.ActionServerCreated, TargetType: "server", TargetID: srv.ID,
		Detail: map[string]any{"name": srv.Name, "kind": srv.Kind, "host": srv.Host, "port": srv.Port},
	})
	return c.Status(fiber.StatusCreated).JSON(fiber.Map{"server": srv})
}

// handleUpdateServer는 서버 설정을 바꾼다.
//
// 여기서 비밀번호를 한 번 고치면 그 서버의 모든 DB에 반영된다. 반대로 말하면
// **여기서의 실수도 모든 DB에 반영된다** — 그래서 응답에 영향 범위(DB 수)를 함께 준다.
func (s *Server) handleUpdateServer(c *fiber.Ctx) error {
	id := c.Params("id")
	// 프로젝트를 볼 수 있는 사람만 그 서버를 고칠 수 있다. 자격증명이 여기 붙어
	// 있으므로 이 관문이 없으면 남의 팀 DB의 접속 계정을 바꿀 수 있다.
	before, perr := s.requireServer(c, id)
	if perr != nil {
		return perr
	}
	var req serverRequest
	if err := c.BodyParser(&req); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "요청 형식이 올바르지 않습니다")
	}
	actor := currentUser(c)
	params, adapter, err := req.toParams(actor.ID)
	if err != nil {
		return fail(c, fiber.StatusBadRequest, "invalid_server", err.Error())
	}
	// 프로젝트는 옮기지 않는다. 옮기면 그 서버의 DB 전부가 함께 옮겨지고, 원래
	// 프로젝트의 사람들이 그것을 소리 없이 잃는다.
	params.ProjectID = before.ProjectID
	// 수정에서는 이미 등록된 DB 하나를 빌려 검증한다.
	// Oracle·SQLite처럼 대상이 곧 접속 정보의 일부인 종류는 그래야 형식을 볼 수 있다.
	probeDB := dbx.BootstrapDatabase(params.Kind)
	if existing := s.connectionsOf(c, id); len(existing) > 0 {
		probeDB = existing[0].DatabaseName
	}
	if err := validateServerTarget(adapter, params, probeDB); err != nil {
		return fail(c, fiber.StatusBadRequest, "invalid_server", err.Error())
	}

	srv, err := s.st.UpdateServer(c.Context(), id, params)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return fail(c, fiber.StatusNotFound, "not_found", "서버를 찾을 수 없습니다")
	case errors.Is(err, store.ErrDuplicateName):
		return fail(c, fiber.StatusConflict, "duplicate", "이미 같은 이름의 서버가 있습니다")
	case err != nil:
		return err
	}
	s.audit(c, store.AuditParams{
		Action: store.ActionServerUpdated, TargetType: "server", TargetID: srv.ID,
		Detail: map[string]any{
			"name": srv.Name, "host": srv.Host, "port": srv.Port,
			"passwordChanged": req.Password != nil, "databases": srv.DatabaseCount,
		},
	})
	// 접속 정보가 바뀌었으므로 소속 DB의 상태를 다시 확인하게 한다.
	for _, conn := range s.connectionsOf(c, srv.ID) {
		s.monitor.TriggerPoll(conn.ID)
	}
	return c.JSON(fiber.Map{"server": srv})
}

func (s *Server) connectionsOf(c *fiber.Ctx, serverID string) []*model.Connection {
	conns, err := s.st.ListConnectionsByServer(c.Context(), serverID)
	if err != nil {
		return nil
	}
	return conns
}

// handleDeleteServer는 서버와 그 아래 DB를 전부 지운다.
//
// 확인 문구를 요구하는 이유: 서버 하나를 지우는 것은 DB 여러 개와 그 지표·이벤트·
// 스키마 버전·ERD 문서를 함께 지우는 일이다. 커넥션 하나를 지우는 것과 무게가 다르다.
func (s *Server) handleDeleteServer(c *fiber.Ctx) error {
	id := c.Params("id")
	srv, err := s.requireServer(c, id)
	if err != nil {
		return err
	}
	if srv.DatabaseCount > 0 && c.Query("confirm") != srv.Name {
		return failDetail(c, fiber.StatusBadRequest, "confirm_required",
			fmt.Sprintf("DB %d개가 함께 삭제됩니다. 확인을 위해 서버 이름을 입력하세요", srv.DatabaseCount),
			srv.Name)
	}
	if err := s.st.DeleteServer(c.Context(), id); err != nil {
		return err
	}
	s.audit(c, store.AuditParams{
		Action: store.ActionServerDeleted, TargetType: "server", TargetID: id,
		Detail: map[string]any{"name": srv.Name, "databases": srv.DatabaseCount},
	})
	return c.JSON(fiber.Map{"ok": true})
}

// handleListServerDatabases는 서버에 실제로 붙어 DB 목록을 읽어온다.
//
// 이미 등록된 것은 registered로 표시해 내려보낸다 — 목록에서 빼면 "왜 안 보이지"가 되고,
// 표시 없이 두면 같은 DB를 두 번 등록하려다 실패한다.
func (s *Server) handleListServerDatabases(c *fiber.Ctx) error {
	srv, err := s.requireServer(c, c.Params("id"))
	if err != nil {
		return err
	}
	sec, err := s.st.GetServerSecret(c.Context(), srv.ID)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(c.Context(), connTestTimeout)
	defer cancel()

	list, err := dbx.ListDatabases(ctx, srv, sec)
	if err != nil {
		if errors.Is(err, dbx.ErrNotImplemented) {
			return failDetail(c, fiber.StatusBadRequest, "not_supported", err.Error(), string(srv.Kind))
		}
		return failDetail(c, fiber.StatusBadGateway, "list_failed",
			"DB 목록을 읽지 못했습니다: "+err.Error(), string(srv.Kind))
	}

	existing, err := s.st.ListConnectionsByServer(c.Context(), srv.ID)
	if err != nil {
		return err
	}
	registered := make(map[string]bool, len(existing))
	for _, conn := range existing {
		registered[conn.DatabaseName] = true
	}
	for i := range list {
		list[i].Registered = registered[list[i].Name]
	}
	dbx.SortDatabases(list)
	return c.JSON(fiber.Map{"databases": list})
}

// addDatabasesRequest는 서버에 DB 여러 개를 한 번에 등록하는 요청이다.
type addDatabasesRequest struct {
	// 프로젝트는 받지 않는다. 서버가 이미 한 프로젝트의 것이고, DB는 그 서버의
	// 프로젝트에 들어간다 — 근거는 하나여야 한다.
	Databases   []string          `json:"databases"`
	Environment model.Environment `json:"environment"`
	// NamePrefix가 비면 "<서버명> / <DB명>"으로 짓는다.
	NamePrefix string   `json:"namePrefix"`
	Tags       []string `json:"tags"`
}

// handleAddServerDatabases는 고른 DB들을 한 번에 등록한다.
//
// 부분 성공을 허용한다. 열 개 중 하나가 이름 충돌로 실패했다고 아홉 개를 되돌리면
// 사용자는 무엇이 문제였는지 모른 채 처음부터 다시 골라야 한다. 대신 어느 것이
// 왜 실패했는지 함께 돌려준다.
func (s *Server) handleAddServerDatabases(c *fiber.Ctx) error {
	srv, err := s.requireServer(c, c.Params("id"))
	if err != nil {
		return err
	}

	var req addDatabasesRequest
	if err := c.BodyParser(&req); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "요청 형식이 올바르지 않습니다")
	}
	if len(req.Databases) == 0 {
		return fail(c, fiber.StatusBadRequest, "no_databases", "등록할 DB를 하나 이상 고르세요")
	}
	env := req.Environment
	if env == "" {
		env = srv.DefaultEnvironment
	}
	if !env.Valid() {
		return fail(c, fiber.StatusBadRequest, "invalid_environment", "환경은 dev 또는 prod여야 합니다")
	}
	if req.Tags == nil {
		req.Tags = []string{}
	}

	prefix := strings.TrimSpace(req.NamePrefix)
	if prefix == "" {
		prefix = srv.Name
	}
	actor := currentUser(c)

	created := []*model.Connection{}
	failed := []fiber.Map{}
	for _, raw := range req.Databases {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		// 프로젝트는 서버에서 나온다. 요청이 들고 온 값을 쓰지 않는 이유: 서버가
		// 이미 한 프로젝트의 것이므로, 다른 프로젝트를 적어 넣을 수 있게 두면
		// 근거가 둘이 되고 그중 하나는 언젠가 어긋난다.
		conn, err := s.st.CreateConnection(c.Context(), store.SaveConnectionParams{
			ProjectID: srv.ProjectID,
			ServerID:  srv.ID, Name: prefix + " / " + name, Environment: env,
			DatabaseName: name, Tags: req.Tags, Enabled: true, ActorID: actor.ID,
		})
		if err != nil {
			reason := "등록에 실패했습니다"
			if errors.Is(err, store.ErrDuplicateName) {
				reason = "같은 이름이나 같은 DB가 이미 등록되어 있습니다"
			}
			failed = append(failed, fiber.Map{"database": name, "reason": reason})
			continue
		}
		created = append(created, conn)
		s.audit(c, store.AuditParams{
			Action: store.ActionConnCreated, TargetType: "connection", TargetID: conn.ID,
			Detail: map[string]any{
				"name": conn.Name, "kind": conn.Kind, "environment": conn.Environment,
				"server": srv.Name, "bulk": true,
			},
		})
		s.monitor.TriggerPoll(conn.ID)
	}

	status := fiber.StatusCreated
	if len(created) == 0 {
		status = fiber.StatusConflict
	}
	return c.Status(status).JSON(fiber.Map{"created": created, "failed": failed})
}

// mergeRequest는 다른 서버의 DB를 이 서버로 옮기는 요청이다.
type mergeRequest struct {
	// SourceServerIDs의 DB 전부를 옮긴다.
	SourceServerIDs []string `json:"sourceServerIds"`
	// DropEmpty가 참이면 비게 된 원본 서버를 지운다.
	DropEmpty bool `json:"dropEmpty"`
}

// handleMergeServers는 이관 후에 남은 중복 서버를 합친다.
//
// 이관 마이그레이션은 기존 커넥션을 자동으로 묶지 않았다 — 자격증명이 다를 수 있고
// 봉인된 값은 비교할 수조차 없기 때문이다. 합치는 판단은 값을 아는 사람이 해야 하고,
// 그 사람이 여기를 부른다. **옮겨진 DB는 대상 서버의 자격증명을 쓰게 된다.**
func (s *Server) handleMergeServers(c *fiber.Ctx) error {
	target, err := s.requireServer(c, c.Params("id"))
	if err != nil {
		return err
	}

	var req mergeRequest
	if err := c.BodyParser(&req); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "요청 형식이 올바르지 않습니다")
	}
	if len(req.SourceServerIDs) == 0 {
		return fail(c, fiber.StatusBadRequest, "no_sources", "합칠 서버를 고르세요")
	}
	if slices.Contains(req.SourceServerIDs, target.ID) {
		return fail(c, fiber.StatusBadRequest, "self_merge", "대상 서버 자신은 합칠 수 없습니다")
	}

	// 옮기기 전에 겹치는 DB를 전부 찾아 한 번에 알린다.
	// 첫 충돌에서 멈추면 사용자는 고치고 다시 눌러 다음 충돌을 만나기를 반복하게 된다.
	targetDBs, err := s.st.ListConnectionsByServer(c.Context(), target.ID)
	if err != nil {
		return err
	}
	taken := make(map[string]bool, len(targetDBs))
	for _, conn := range targetDBs {
		taken[conn.DatabaseName] = true
	}

	moved := []string{}
	names := []string{}
	conflicts := []string{}
	for _, srcID := range req.SourceServerIDs {
		src, serr := s.requireServer(c, srcID)
		if serr != nil {
			return serr
		}
		// 프로젝트를 넘어 합치지 않는다. 옮겨진 DB는 대상 서버의 프로젝트로
		// 딸려 가는데, 그러면 원래 프로젝트의 사람들이 그 DB를 소리 없이 잃는다.
		if src.ProjectID != target.ProjectID {
			return fail(c, fiber.StatusBadRequest, "project_mismatch",
				src.Name+"은(는) 다른 프로젝트의 서버라 합칠 수 없습니다")
		}
		// 종류가 다르면 접속 자체가 불가능하다. 합치는 순간 전부 죽는다.
		if src.Kind != target.Kind {
			return fail(c, fiber.StatusBadRequest, "kind_mismatch",
				fmt.Sprintf("%s는 종류가 달라(%s) %s(%s)와 합칠 수 없습니다",
					src.Name, src.Kind, target.Name, target.Kind))
		}
		conns, err := s.st.ListConnectionsByServer(c.Context(), srcID)
		if err != nil {
			return err
		}
		ids := make([]string, 0, len(conns))
		for _, conn := range conns {
			if taken[conn.DatabaseName] {
				conflicts = append(conflicts, src.Name+" / "+conn.DatabaseName)
				continue
			}
			taken[conn.DatabaseName] = true
			ids = append(ids, conn.ID)
		}
		if len(conflicts) > 0 {
			// 하나라도 겹치면 아무것도 옮기지 않는다. 절반만 옮겨진 상태는
			// 어느 서버에 무엇이 있는지 사람이 다시 세어 봐야 하는 상태다.
			return failDetail(c, fiber.StatusConflict, "duplicate",
				fmt.Sprintf("대상 서버 %s에 같은 DB가 이미 있어 옮길 수 없습니다: %s. "+
					"먼저 한쪽을 삭제하거나 이름을 정리하세요",
					target.Name, strings.Join(conflicts, ", ")),
				strings.Join(conflicts, ","))
		}
		if err := s.st.MoveConnectionsToServer(c.Context(), target.ID, ids); err != nil {
			if errors.Is(err, store.ErrDuplicateName) {
				return fail(c, fiber.StatusConflict, "duplicate",
					"대상 서버에 같은 DB가 이미 등록되어 있습니다")
			}
			return err
		}
		moved = append(moved, ids...)
		names = append(names, src.Name)
	}

	dropped := 0
	if req.DropEmpty {
		if dropped, err = s.st.DeleteEmptyServers(c.Context()); err != nil {
			return err
		}
	}
	s.audit(c, store.AuditParams{
		Action: store.ActionServerMerged, TargetType: "server", TargetID: target.ID,
		Detail: map[string]any{
			"target": target.Name, "sources": names,
			"movedDatabases": len(moved), "droppedServers": dropped,
		},
	})
	updated, err := s.st.GetServer(c.Context(), target.ID)
	if err != nil {
		return err
	}
	return c.JSON(fiber.Map{"server": updated, "moved": len(moved), "droppedServers": dropped})
}

// kindNeedsFirstDatabase는 첫 대상과 함께 등록해야 하는 이유를 설명한다.
func kindNeedsFirstDatabase(kind model.DBKind) string {
	switch kind {
	case model.KindOracle:
		return "Oracle은 서비스명(또는 SID)이 접속 정보의 일부입니다. " +
			"커넥션 등록에서 서비스명과 함께 만드세요"
	case model.KindSQLite:
		return "SQLite는 파일 하나가 곧 데이터베이스입니다. " +
			"커넥션 등록에서 파일 경로와 함께 만드세요"
	default:
		return "이 종류는 대상 데이터베이스와 함께 등록해야 합니다"
	}
}
