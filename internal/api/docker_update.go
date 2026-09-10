package api

import (
	"context"
	"time"

	"github.com/gofiber/fiber/v2"

	"dbstudio/internal/provision"
	"dbstudio/internal/store"
)

// 만든 뒤에 설정 고치기.
//
// ── 왜 미리보기가 따로 있는가 ───────────────────────────────────────
// 고치기 전에 **무엇이 일어나는지** 보여 준다. 재시작인지, 컨테이너를 다시
// 만드는 것인지, 아니면 고쳐도 지금 DB 에는 적용되지 않는 것인지.
//
// 마지막 것이 이 기능의 핵심이다. PostgreSQL·MySQL·MariaDB·MongoDB·MS-SQL 은
// 계정과 비밀번호를 **첫 실행에서만** 쓴다 — 값을 고치고 컨테이너를 다시 만들어도
// 옛 비밀번호가 그대로다(살아 있는 컨테이너로 일곱 종류를 다 재 봤다).
// 그것을 말하지 않고 "저장했습니다"라고만 하면, 사람은 바뀐 줄 알고 새 비밀번호로
// 접속하다 막히고 무엇이 잘못됐는지 알 길이 없다.

// handleDBInstanceChanges는 무엇이 바뀌고 무엇이 필요한지만 답한다. 아무것도 바꾸지 않는다.
func (s *Server) handleDBInstanceChanges(c *fiber.Ctx) error {
	in, changes, _, _, err := s.planUpdate(c)
	if err != nil {
		return err
	}
	need := provision.Needed(changes)
	return c.JSON(fiber.Map{
		"instance":  in.Name,
		"changes":   changes,
		"needed":    string(need),
		"needsText": provision.DescribeNeed(need),
		// 고쳐도 소용없는 것들은 따로 뽑아 준다. 화면이 그것만 다르게 보여야 한다.
		"initOnly": provision.InitOnly(changes),
	})
}

// handleUpdateDBInstance는 설정을 고치고 필요한 만큼 컨테이너를 다룬다.
func (s *Server) handleUpdateDBInstance(c *fiber.Ctx) error {
	in, changes, plan, merged, err := s.planUpdate(c)
	if err != nil {
		return err
	}
	if len(changes) == 0 {
		return c.JSON(fiber.Map{"instance": in, "changes": []any{}, "needed": "none"})
	}
	if s.provisioner().Busy(in.ID) {
		return fail(c, fiber.StatusConflict, "busy", "아직 만들고 있습니다")
	}
	if in.Status == store.InstanceRemoved {
		return fail(c, fiber.StatusConflict, "removed",
			"지운 DB 는 고칠 수 없습니다. 같은 설정으로 새로 만드세요")
	}

	// 값을 먼저 저장한다.
	//
	// 순서가 중요하다: 컨테이너를 먼저 바꾸고 저장하다 실패하면, 도커에는 새
	// 설정이 들어갔는데 우리 표에는 옛 값이 남는다. 그 어긋남은 다음 재시작에
	// 옛 설정으로 되돌아가는 모양으로 나타난다.
	values, secrets := splitSecrets(plan.Recipe, merged)
	if err := s.st.UpdateDBInstance(c.Context(), in.ID, store.UpdateDBInstanceParams{
		Values: &values,
	}); err != nil {
		return err
	}
	if len(secrets) > 0 {
		if err := s.st.SaveDBInstanceSecrets(c.Context(), in.ID, secrets); err != nil {
			return err
		}
	}

	need := provision.Needed(changes)
	s.audit(c, store.AuditParams{
		Action: store.ActionDBInstanceUpdated, TargetType: "dbinstance", TargetID: in.ID,
		Detail: map[string]any{
			"name": in.Name, "needed": string(need), "changed": changedKeys(changes),
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if err := s.provisioner().Update(ctx, in, plan, need); err != nil {
		// 값은 이미 저장됐다. 그 사실을 함께 말한다 — "실패했습니다"만 보면
		// 사람은 아무 일도 없었다고 생각하고 같은 것을 또 고친다.
		return failDetail(c, fiber.StatusBadGateway, "apply_failed",
			"설정은 저장했지만 적용하지 못했습니다: "+err.Error(),
			"목록에서 다시 시작해 보세요. 그래도 안 되면 로그를 확인하세요")
	}

	fresh, err := s.st.GetDBInstance(c.Context(), in.ID)
	if err != nil {
		return err
	}
	// 다시 만들었으면 포트가 바뀌었을 수 있다.
	s.syncConnectionAddress(c.Context(), fresh)
	return c.JSON(fiber.Map{
		"instance": fresh, "changes": changes,
		"needed": string(need), "needsText": provision.DescribeNeed(need),
		"initOnly": provision.InitOnly(changes),
	})
}

// planUpdate는 요청을 읽고 차이와 새 계획을 만든다(아무것도 바꾸지 않는다).
func (s *Server) planUpdate(c *fiber.Ctx) (
	*store.DBInstance, []provision.Change, *provision.Plan, map[string]string, error,
) {
	in, err := s.requireDBInstance(c, c.Params("id"))
	if err != nil {
		return nil, nil, nil, nil, err
	}
	var req struct {
		Values map[string]string `json:"values"`
	}
	if err := c.BodyParser(&req); err != nil {
		return nil, nil, nil, nil, fail(c, fiber.StatusBadRequest, "bad_request",
			"요청 형식이 올바르지 않습니다")
	}
	recipe := provision.Find(in.Kind)
	if recipe == nil {
		return nil, nil, nil, nil, fail(c, fiber.StatusBadRequest, "unknown_kind",
			in.Kind+" 는 모르는 DB 종류입니다")
	}

	// 지금 값(비밀 포함)에 새 값을 덮어 계획을 세운다. 비밀을 합치지 않으면
	// "비밀번호를 입력하세요"로 막힌다 — 고치지 않은 칸인데도.
	before := s.mergedValues(c, in)
	merged := make(map[string]string, len(before)+len(req.Values))
	for k, v := range before {
		merged[k] = v
	}
	for k, v := range req.Values {
		merged[k] = v
	}
	plan, err := provision.Build(provision.Spec{
		Kind: in.Kind, Name: in.Name, Version: in.Version, Values: merged,
	})
	if err != nil {
		return nil, nil, nil, nil, fail(c, fiber.StatusBadRequest, "invalid_plan", err.Error())
	}

	changes := provision.Diff(recipe, before, req.Values)
	return in, changes, plan, merged, nil
}

// mergedValues는 저장된 값과 비밀을 합친 지금 설정이다.
func (s *Server) mergedValues(c *fiber.Ctx, in *store.DBInstance) map[string]string {
	out := make(map[string]string, len(in.Values)+2)
	for k, v := range in.Values {
		out[k] = v
	}
	if secrets, err := s.st.DBInstanceSecrets(c.Context(), in.ID); err == nil {
		for k, v := range secrets {
			out[k] = v
		}
	}
	return out
}

func changedKeys(changes []provision.Change) []string {
	out := make([]string, 0, len(changes))
	for _, c := range changes {
		out = append(out, c.Key)
	}
	return out
}
