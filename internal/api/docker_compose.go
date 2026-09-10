package api

import (
	"strings"

	"github.com/gofiber/fiber/v2"

	"dbstudio/internal/provision"
	"dbstudio/internal/store"
)

// compose.yml 내보내기.
//
// ── 왜 필요한가 ─────────────────────────────────────────────────────
// 화면에서 정한 것을 **가져갈 수 있어야** 한다. 이 앱 없이도 같은 DB 를 띄울 수
// 있어야 하고, 그 파일을 저장소에 넣어 검토할 수 있어야 한다. 그렇지 않으면
// 여기서 만든 DB 는 이 앱 안에서만 존재하는 것이 되고, 그것은 사람을 묶어 두는
// 종류의 편리함이다.
//
// ── 왜 계획을 다시 세우는가 ─────────────────────────────────────────
// 저장해 둔 것은 사람이 고른 **값**이지 계획이 아니다. 계획을 통째로 저장하면
// 레시피를 고친 뒤에도 옛 계획이 남아, 화면이 말하는 것과 내보낸 파일이 갈린다.
// 값에서 매번 다시 세우면 그 둘이 갈릴 자리가 없다.

// handleInstanceCompose는 인스턴스 하나를 compose.yml 로 내보낸다.
func (s *Server) handleInstanceCompose(c *fiber.Ctx) error {
	in, err := s.requireDBInstance(c, c.Params("id"))
	if err != nil {
		return err
	}
	item, err := s.composeItem(c, in)
	if err != nil {
		return fail(c, fiber.StatusBadRequest, "invalid_plan", err.Error())
	}
	f := composeFormat(c)
	res := composeResponse(renderFor(f, []provision.ComposeItem{item}), 1, f)
	if in.Cluster != "" {
		res["notes"] = append(toStrings(res["notes"]),
			"이 노드는 클러스터의 일부입니다. 내보낸 파일에는 클러스터 설정"+
				"(remote_servers·macros·Keeper)이 **들어 있지 않습니다** — 이 파일로 띄우면 "+
				"클러스터가 아니라 혼자 도는 ClickHouse 가 뜹니다.")
	}
	return c.JSON(res)
}

// handleProjectCompose는 프로젝트의 DB 들을 한 파일로 내보낸다.
//
// 하나씩 내보낸 파일을 사람이 손으로 합치게 두지 않는 이유: 합치는 자리에
// 볼륨·네트워크 선언이 겹치고, 그것을 정리하다 보면 파일이 원본과 달라진다.
func (s *Server) handleProjectCompose(c *fiber.Ctx) error {
	scope, err := s.projectFilter(c)
	if err != nil {
		return err
	}
	all, err := s.st.ListDBInstances(c.Context(), strings.TrimSpace(c.Query("project")))
	if err != nil {
		return err
	}

	items := make([]provision.ComposeItem, 0, len(all))
	skipped := []string{}
	clustered := false
	for _, in := range all {
		if !inProjects(scope, in.ProjectID) {
			continue
		}
		// 지운 것은 넣지 않는다. 그 줄은 기록이지 띄울 것이 아니다.
		if in.Status == store.InstanceRemoved {
			continue
		}
		item, err := s.composeItem(c, in)
		if err != nil {
			// 하나가 막혀도 나머지는 내보낸다. 대신 무엇이 빠졌는지 말한다 —
			// 조용히 빠지면 그 DB 가 없는 파일을 받아 들고 왜 안 뜨는지 찾게 된다.
			skipped = append(skipped, in.Name+": "+err.Error())
			continue
		}
		if in.Cluster != "" {
			clustered = true
		}
		items = append(items, item)
	}
	if len(items) == 0 {
		return fail(c, fiber.StatusNotFound, "empty",
			"내보낼 DB 가 없습니다")
	}
	f := composeFormat(c)
	res := composeResponse(renderFor(f, items), len(items), f)
	if len(skipped) > 0 {
		res["skipped"] = skipped
	}
	if clustered {
		// 클러스터 노드는 값에서 계획을 다시 세우는 길로는 온전히 나오지
		// 않는다. remote_servers·macros·조정자 설정이 빠지고, 그렇게 나온
		// 파일로 띄우면 **클러스터가 아닌 노드 여럿**이 뜬다 — 겉보기에는
		// 멀쩡하다. 그 사실을 말하지 않으면 받아 간 사람이 그것으로 클러스터를
		// 세웠다고 믿는다.
		res["notes"] = append(toStrings(res["notes"]),
			"클러스터로 만든 노드가 들어 있습니다. 내보낸 파일에는 클러스터 설정"+
				"(remote_servers·macros·Keeper)이 **들어 있지 않습니다** — 이 파일로 띄우면 "+
				"서로 모르는 ClickHouse 여럿이 뜹니다.")
	}
	return c.JSON(res)
}

// composeItem은 저장해 둔 값에서 계획을 다시 세운다.
//
// 비밀을 다시 합치는 이유: 비밀번호는 값 칸이 아니라 따로 저장된다. 그것 없이
// 계획을 세우면 "비밀번호를 입력하세요"로 막힌다 — 비밀번호가 없어서가 아니라
// 우리가 갈라 두었기 때문인데, 화면에는 값을 안 넣은 것처럼 보인다.
//
// 합쳐도 파일로는 나가지 않는다. 내보내는 쪽이 값을 보고 `${VAR}` 자리로
// 바꾸기 때문이고(masker), 그것이 값으로 찾는 방식이어야 하는 이유이기도 하다.
func (s *Server) composeItem(c *fiber.Ctx, in *store.DBInstance) (provision.ComposeItem, error) {
	values := make(map[string]string, len(in.Values)+2)
	for k, v := range in.Values {
		values[k] = v
	}
	secrets, err := s.st.DBInstanceSecrets(c.Context(), in.ID)
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
	return provision.ComposeItem{
		Service: in.Name,
		Plan:    plan,
		// 실제로 잡힌 포트를 쓴다. 계획의 값(대개 0)을 그대로 두면 내보낸 파일로
		// 띄웠을 때 포트가 또 달라지고, 그러면 지금 붙어 있는 주소와 어긋난다.
		HostPort: in.HostPort,
	}, nil
}

// composeFormat은 무엇을 위한 파일인지 읽는다.
//
// 기본이 compose 인 이유: 스웜을 쓰지 않는 사람이 훨씬 많고, 스웜용 파일은
// 스웜이 아닌 곳에서 조용히 다르게 동작한다(restart 가 무시되고 네트워크가
// 오버레이라 만들어지지 않는다).
func composeFormat(c *fiber.Ctx) provision.Format {
	if strings.EqualFold(strings.TrimSpace(c.Query("format")), string(provision.ForStack)) {
		return provision.ForStack
	}
	return provision.ForCompose
}

func renderFor(f provision.Format, items []provision.ComposeItem) *provision.ComposeFile {
	if f == provision.ForStack {
		return provision.Stack(items)
	}
	return provision.Compose(items)
}

// toStrings는 fiber.Map 에 들어 있던 안내 목록을 되꺼낸다.
func toStrings(v any) []string {
	if out, ok := v.([]string); ok {
		return out
	}
	return nil
}

func composeResponse(f *provision.ComposeFile, count int, format provision.Format) fiber.Map {
	name := "compose.yaml"
	if format == provision.ForStack {
		name = "stack.yaml"
	}
	return fiber.Map{
		"yaml":       f.YAML,
		"envExample": f.EnvExample,
		"files":      f.Files,
		"notes":      f.Notes,
		"services":   count,
		"format":     string(format),
		"filename":   name,
	}
}
