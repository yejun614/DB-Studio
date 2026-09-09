package api

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"

	"dbstudio/internal/model"
	"dbstudio/internal/provision"
	"dbstudio/internal/store"
)

// DB 컨테이너 만들기.
//
// ── 왜 미리보기를 따로 두는가 ───────────────────────────────────────
// 만들기 전에 "무엇이 만들어지는가"를 보여 준다. 이미지·포트·볼륨·경고까지.
// 이 화면이 없으면 사람은 만들어 보고서야 알게 되는데, 이 만들기는 되돌리는
// 값이 크다 — 컨테이너 하나와 볼륨 하나가 생기고, 그것이 잘못이면 지우는
// 것까지 해야 한다.
//
// ── 왜 만들자마자 커넥션이 되는가 ───────────────────────────────────
// 띄운 DB 를 DB Studio 에서 쓰려면 커넥션이어야 한다. 그것을 사람이 손으로
// 다시 등록하게 하면 호스트·포트·계정을 우리가 아는데도 사람이 옮겨 적게 되고,
// 옮겨 적는 자리마다 오타가 난다.
//
// 등록은 **뜬 뒤에** 한다. 포트는 뜨고 나서야 정해지고(도커가 골라 준다),
// 아직 안 뜬 DB 를 등록하면 접속 확인이 실패한다 — 그 실패를 사람은 자기가
// 설정을 잘못한 것으로 읽는다.

// instanceRequest는 DB 컨테이너 하나를 만드는 입력이다.
type instanceRequest struct {
	ProjectID string            `json:"projectId"`
	Kind      string            `json:"kind"`
	Name      string            `json:"name"`
	Version   string            `json:"version"`
	Values    map[string]string `json:"values"`

	// Register가 false면 커넥션으로 등록하지 않는다.
	//
	// 기본은 등록이다(nil 이면 참). 잠깐 띄워 보는 DB 도 있으니 끌 수는 있게
	// 두지만, 대개는 쓰려고 만드는 것이라 기본을 반대로 두면 사람이 매번
	// 켜야 한다.
	Register *bool `json:"register"`
	// Environment는 등록할 커넥션의 환경이다. 비우면 dev 다 —
	// 방금 도커로 띄운 DB 를 prod 로 잡아 두면 운영 화면의 경고가 무뎌진다.
	Environment model.Environment `json:"environment"`
}

// handleDockerCatalog는 띄울 수 있는 DB 목록이다.
func (s *Server) handleDockerCatalog(c *fiber.Ctx) error {
	return c.JSON(fiber.Map{
		"recipes": provision.Catalog(),
		// 못 만드는 것도 이유와 함께 내려보낸다. 목록에 없는 것을 사람이
		// 찾다 못 찾으면 "지원하지 않는다"와 "아직 안 만들었다"를 구분할 수 없다.
		"unsupported": provision.NotInCatalogAll(),
		"network":     provision.NetworkName,
	})
}

// handleDockerPlan은 만들기 전에 계획을 보여 준다. 아무것도 만들지 않는다.
func (s *Server) handleDockerPlan(c *fiber.Ctx) error {
	var req instanceRequest
	if err := c.BodyParser(&req); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "요청 형식이 올바르지 않습니다")
	}
	plan, err := provision.Build(provision.Spec{
		Kind: req.Kind, Name: strings.TrimSpace(req.Name),
		Version: req.Version, Values: req.Values,
	})
	if err != nil {
		return fail(c, fiber.StatusBadRequest, "invalid_plan", err.Error())
	}
	return c.JSON(fiber.Map{"plan": plan, "files": fileNames(plan)})
}

// handleCreateDBInstance는 컨테이너를 만든다. 만들기는 배경에서 돈다.
func (s *Server) handleCreateDBInstance(c *fiber.Ctx) error {
	var req instanceRequest
	if err := c.BodyParser(&req); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "요청 형식이 올바르지 않습니다")
	}
	// 프로젝트가 먼저다. 만든 DB 는 곧 커넥션이 되고, 커넥션은 프로젝트 없이
	// 존재할 수 없다 — 프로젝트 밖에서 만들면 만든 사람에게도 보이지 않는다.
	proj, perr := s.requireProject(c, req.ProjectID)
	if perr != nil {
		return perr
	}
	if req.Environment == "" {
		req.Environment = model.EnvDev
	}
	if !req.Environment.Valid() {
		return fail(c, fiber.StatusBadRequest, "bad_request", "환경은 dev 또는 prod여야 합니다")
	}

	plan, err := provision.Build(provision.Spec{
		Kind: req.Kind, Name: strings.TrimSpace(req.Name),
		Version: req.Version, Values: req.Values,
	})
	if err != nil {
		return fail(c, fiber.StatusBadRequest, "invalid_plan", err.Error())
	}

	// 비밀은 갈라서 담는다. values 에 남기면 목록 한 번에 모든 비밀번호가
	// 메모리로 올라오고, 그것을 JSON 으로 내보내는 길이 생긴다.
	values, secrets := splitSecrets(plan.Recipe, req.Values)

	in, err := s.st.CreateDBInstance(c.Context(), store.CreateDBInstanceParams{
		ProjectID: proj.ID,
		Name:      strings.TrimSpace(req.Name),
		Kind:      plan.Recipe.ID,
		Image:     plan.Image,
		Version:   versionOf(plan.Image),
		// 컨테이너·볼륨 이름은 계획이 정한다. 화면이 정하면 두 곳에 규칙이 있게 된다.
		ContainerName: plan.Container,
		VolumeName:    plan.Volume,
		Port:          plan.Port,
		HostPort:      0, // 실제로 잡힌 포트는 뜬 뒤에 적는다
		Values:        values,
		Secrets:       secrets,
		// 이름과 id 를 함께 남긴다. 이름만 남기면 사람을 지운 뒤 누구였는지
		// 알 수 없고, id 만 남기면 화면이 그것을 다시 사람 이름으로 풀어야 한다.
		ActorID: actorID(c),
		Actor:   actorName(c),
	})
	switch {
	case errors.Is(err, store.ErrInstanceNameTaken):
		return fail(c, fiber.StatusConflict, "duplicate",
			"같은 이름의 DB 컨테이너가 이미 있습니다. 다른 이름을 쓰세요")
	case errors.Is(err, store.ErrNoProject):
		return fail(c, fiber.StatusBadRequest, "no_project", "프로젝트를 고르세요")
	case err != nil:
		return err
	}

	s.audit(c, store.AuditParams{
		Action: store.ActionDBInstanceCreated, TargetType: "dbinstance", TargetID: in.ID,
		Detail: map[string]any{
			"name": in.Name, "kind": in.Kind, "image": in.Image,
			"project": proj.Name, "register": req.Register == nil || *req.Register,
		},
	})

	var onReady func(string)
	if req.Register == nil || *req.Register {
		// 클로저에 담는 것: 요청이 끝난 뒤에 불리므로 c 를 들고 갈 수 없다.
		// Fiber 의 컨텍스트는 요청이 끝나면 재사용되고, 그때 읽은 값은 다른
		// 요청의 것일 수 있다.
		actor, env := actorID(c), req.Environment
		onReady = func(id string) { s.registerInstance(id, actor, env) }
	}
	if err := s.provisioner().Create(in.ID, plan, onReady); err != nil {
		if errors.Is(err, provision.ErrBusy) {
			return fail(c, fiber.StatusConflict, "busy", err.Error())
		}
		return err
	}
	return c.Status(fiber.StatusAccepted).JSON(fiber.Map{
		"instance": in, "plan": plan, "warnings": plan.Warnings,
	})
}

// handleListDBInstances는 프로젝트의 DB 컨테이너 목록이다.
func (s *Server) handleListDBInstances(c *fiber.Ctx) error {
	scope, err := s.projectFilter(c)
	if err != nil {
		return err
	}
	all, err := s.st.ListDBInstances(c.Context(), strings.TrimSpace(c.Query("project")))
	if err != nil {
		return err
	}
	items := make([]*store.DBInstance, 0, len(all))
	for _, in := range all {
		// 프로젝트 밖의 것은 보이지 않는다. 이 관문이 없으면 참여하지 않은
		// 프로젝트의 DB 이름이 목록에 그대로 뜬다.
		if !inProjects(scope, in.ProjectID) {
			continue
		}
		items = append(items, in)
	}
	return c.JSON(fiber.Map{"items": items})
}

// handleGetDBInstance는 하나를 읽는다. 읽을 때 도커의 지금 상태로 맞춘다.
//
// 왜 읽을 때 맞추는가: 우리 status 는 캐시다. 사람이 도커에서 직접 멈췄거나
// 컨테이너가 죽었으면 우리 줄은 여전히 "도는 중"이고, 그 화면을 믿고 누른
// 버튼은 모두 실패한다.
func (s *Server) handleGetDBInstance(c *fiber.Ctx) error {
	in, err := s.requireDBInstance(c, c.Params("id"))
	if err != nil {
		return err
	}
	// 만드는 중에는 맞추지 않는다. 그 사이 상태는 러너가 쓰고 있고, 여기서
	// 도커를 보고 덮으면 진행률이 지워진다.
	if in.Status != store.InstanceCreating {
		ctx, cancel := context.WithTimeout(c.Context(), s.cfg.DockerTimeout)
		defer cancel()
		if err := s.provisioner().Refresh(ctx, in); err == nil {
			if fresh, err := s.st.GetDBInstance(c.Context(), in.ID); err == nil {
				in = fresh
			}
		}
	}
	return c.JSON(fiber.Map{
		"instance": in,
		"busy":     s.provisioner().Busy(in.ID),
		// 어떤 값으로 만들었는지를 화면이 다시 보여 줄 수 있어야 한다.
		// 레시피를 함께 내려보내는 이유: 열쇠만 있으면 라벨을 알 수 없다.
		"recipe": provision.Find(in.Kind),
	})
}

// requireDBInstance는 인스턴스를 읽고 프로젝트 접근을 확인한다.
func (s *Server) requireDBInstance(c *fiber.Ctx, id string) (*store.DBInstance, error) {
	in, err := s.st.GetDBInstance(c.Context(), strings.TrimSpace(id))
	if errors.Is(err, store.ErrNotFound) {
		return nil, fiber.NewError(fiber.StatusNotFound, "DB 컨테이너를 찾을 수 없습니다")
	}
	if err != nil {
		return nil, err
	}
	// 프로젝트를 못 보면 그 안의 것도 없는 것이다. 403 이 아니라 404 로
	// 답하는 이유는 requireProject 와 같다 — 있다는 사실 자체가 정보다.
	ok, err := s.canSeeProject(c, in.ProjectID)
	if err != nil {
		return nil, err
	}
	if !ok {
		s.auditDenied(c, "dbinstance.denied", in.ID)
		return nil, fiber.NewError(fiber.StatusNotFound, "DB 컨테이너를 찾을 수 없습니다")
	}
	return in, nil
}

// ---------- 커넥션으로 등록 ----------

// registerInstance는 뜬 DB 를 커넥션으로 등록한다.
//
// 러너의 배경 작업에서 불린다. 요청의 컨텍스트가 없으므로 시간 제한을 스스로
// 둔다.
func (s *Server) registerInstance(instanceID, actorID string, env model.Environment) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	in, err := s.st.GetDBInstance(ctx, instanceID)
	if err != nil {
		slog.Error("등록할 DB 컨테이너를 찾지 못했습니다", "instance", instanceID, "err", err)
		return
	}
	if in.ConnectionID != "" {
		return // 이미 등록되어 있다(다시 만든 경우)
	}
	recipe := provision.Find(in.Kind)
	if recipe == nil {
		return
	}
	secrets, err := s.st.DBInstanceSecrets(ctx, in.ID)
	if err != nil {
		slog.Error("DB 컨테이너의 비밀을 읽지 못했습니다", "instance", in.ID, "err", err)
		return
	}

	host, port, note := connectTarget(in, inContainer())
	pw := secrets["password"]
	sp := store.SaveServerParams{
		ProjectID: in.ProjectID,
		Name:      in.Name, Kind: recipe.Kind,
		Host: host, Port: port,
		Options: model.Options{}, DefaultEnvironment: env,
		Tags: []string{"docker"}, Note: note, Enabled: true,
		Username: recipe.AdminAccount(in.Values), Password: &pw,
		ActorID: actorID,
	}
	cp := store.SaveConnectionParams{
		ProjectID: in.ProjectID, Name: in.Name, Environment: env,
		DatabaseName: in.Values["database"], Tags: []string{"docker"},
		Enabled: true, ActorID: actorID,
	}

	_, conn, err := s.st.CreateServerWithDatabase(ctx, sp, cp)
	if err != nil {
		// 등록이 실패해도 DB 는 돌고 있다. 그 사실을 인스턴스에 적어 두어
		// 화면이 "직접 등록하세요"라고 말할 수 있게 한다.
		msg := "DB 는 떴지만 커넥션으로 등록하지 못했습니다: " + err.Error()
		_ = s.st.UpdateDBInstance(ctx, in.ID, store.UpdateDBInstanceParams{Error: &msg})
		slog.Error("DB 컨테이너를 커넥션으로 등록하지 못했습니다", "instance", in.ID, "err", err)
		return
	}
	if err := s.st.UpdateDBInstance(ctx, in.ID, store.UpdateDBInstanceParams{
		ConnectionID: &conn.ID,
	}); err != nil {
		slog.Error("커넥션 연결을 적지 못했습니다", "instance", in.ID, "err", err)
	}
	s.monitor.TriggerPoll(conn.ID)
}

// connectTarget은 이 앱에서 그 DB 에 붙을 주소를 정한다.
//
// ── 왜 두 갈래인가 ─────────────────────────────────────────────────
// 도커 소켓이 로컬이라는 것은 데몬이 같은 기계에 있다는 뜻일 뿐, **우리가
// 어디에 있는지**는 말해 주지 않는다.
//
//   - 우리가 호스트에서 돌면: 도커가 호스트에 열어 준 포트로 붙는다
//     (127.0.0.1:32768 꼴).
//   - 우리가 컨테이너 안에서 돌면: 127.0.0.1 은 **우리 컨테이너 자신**이다.
//     그 주소로 붙으면 아무것도 없고, 오류는 "connection refused" 뿐이라
//     원인을 짐작할 수 없다. 그래서 컨테이너 이름과 컨테이너 안 포트로 붙는다 —
//     같은 도커 네트워크에 있으면 이름이 곧 주소다.
//
// 두 번째 갈래는 우리 컨테이너가 dbstudio-net 에 붙어 있어야 한다. 그것을
// 여기서 확인할 수는 없으므로(우리 자신의 컨테이너 이름을 확실히 알 길이 없다)
// 커넥션의 메모에 적어 둔다. 접속이 안 될 때 볼 곳이 있어야 한다.
func connectTarget(in *store.DBInstance, inContainer bool) (host string, port int, note string) {
	if inContainer {
		return in.ContainerName, in.Port,
			"DB Studio 가 컨테이너 안에서 돌고 있어 컨테이너 이름으로 등록했습니다. " +
				"접속이 안 되면 DB Studio 컨테이너가 " + provision.NetworkName +
				" 네트워크에 붙어 있는지 확인하세요"
	}
	return "127.0.0.1", in.HostPort, "DB Studio 가 도커로 만든 DB 입니다"
}

// inContainer는 이 프로세스가 컨테이너 안인지 본다.
//
// /.dockerenv 는 도커가 만들어 두는 표식이다. 확실한 방법은 아니지만(다른
// 런타임은 만들지 않는다) 틀렸을 때의 결과가 "주소를 잘못 골랐다"이고 그것은
// 커넥션 화면에서 고칠 수 있다.
func inContainer() bool {
	_, err := os.Stat("/.dockerenv")
	return err == nil
}

// ---------- 유틸 ----------

// splitSecrets는 비밀 필드를 갈라낸다.
//
// 무엇이 비밀인지는 레시피가 안다. 화면이 정하면 새 필드를 더할 때 한쪽만
// 고치게 되고, 그때 비밀번호가 평문 칸으로 들어간다.
func splitSecrets(r *provision.Recipe, values map[string]string) (map[string]string, map[string]string) {
	plain, secret := map[string]string{}, map[string]string{}
	for k, v := range values {
		if f := r.Field(k); f != nil && f.Secret {
			secret[k] = v
			continue
		}
		plain[k] = v
	}
	return plain, secret
}

// fileNames는 계획이 넣을 설정 파일의 경로만 뽑는다.
//
// 내용까지 보여 주지 않는 이유: 미리보기 응답이 XML 로 뒤덮이면 정작 봐야 하는
// 이미지·포트·경고가 묻힌다. 내용이 필요하면 만든 뒤 컨테이너에서 볼 수 있다.
func fileNames(p *provision.Plan) []string {
	out := make([]string, 0, len(p.Files))
	for path := range p.Files {
		out = append(out, path)
	}
	sort.Strings(out)
	return out
}

// versionOf는 이미지 이름에서 태그만 뽑는다.
func versionOf(image string) string {
	if i := strings.LastIndex(image, ":"); i > 0 {
		return image[i+1:]
	}
	return "latest"
}

func actorName(c *fiber.Ctx) string {
	if u := currentUser(c); u != nil {
		if u.DisplayName != "" {
			return u.DisplayName
		}
		return u.Username
	}
	return ""
}

func actorID(c *fiber.Ctx) string {
	if u := currentUser(c); u != nil {
		return u.ID
	}
	return ""
}
