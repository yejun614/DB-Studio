package api

import (
	"errors"
	"strings"

	"github.com/gofiber/fiber/v2"

	"dbstudio/internal/model"
	"dbstudio/internal/provision"
	"dbstudio/internal/store"
)

// ClickHouse 클러스터 만들기.
//
// ── 왜 만들기 경로를 따로 두는가 ────────────────────────────────────
// 지금까지의 만들기는 **컨테이너 하나 = 줄 하나**였다. 클러스터는 줄이 여럿이고
// 순서가 있다(조정자 먼저). 같은 핸들러에 분기를 넣으면 그 함수가 두 가지 일을
// 하게 되고, 어느 쪽이 실패했는지도 섞인다.
//
// ── 왜 줄을 노드마다 두는가 ─────────────────────────────────────────
// 그래야 지금 있는 것이 그대로 쓰인다 — 목록도, 시작·중단도, 로그도, 지우기도.
// 묶음이라는 사실은 cluster 칸이 말한다.

// clusterRequest는 클러스터 구성이다.
type clusterRequest struct {
	Shards   int `json:"shards"`
	Replicas int `json:"replicas"`
}

// createCluster는 클러스터 하나를 만든다(handleCreateDBInstance 에서 갈라져 온다).
func (s *Server) createCluster(c *fiber.Ctx, req *instanceRequest, proj *store.Project) error {
	if !strings.EqualFold(req.Kind, "clickhouse") {
		return fail(c, fiber.StatusBadRequest, "cluster_unsupported",
			"클러스터로 만들 수 있는 것은 지금 ClickHouse 뿐입니다")
	}
	plan, err := provision.BuildCluster(provision.ClusterSpec{
		Name: strings.TrimSpace(req.Name), Version: req.Version,
		Shards: req.Cluster.Shards, Replicas: req.Cluster.Replicas,
		Values: req.Values,
	})
	if err != nil {
		return fail(c, fiber.StatusBadRequest, "invalid_plan", err.Error())
	}

	// 줄을 먼저 다 적는다.
	//
	// 하나라도 이름이 겹치면 **아무것도 만들지 않고** 멈춘다. 절반만 적어 두면
	// 그 절반이 "만드는 중"으로 남고, 사람은 그것을 하나씩 지워야 한다.
	rows := make([]*store.DBInstance, 0, len(plan.Nodes))
	creates := make([]provision.ClusterCreate, 0, len(plan.Nodes))
	for _, node := range plan.Nodes {
		values, secrets := splitSecrets(node.Plan.Recipe, req.Values)
		in, err := s.st.CreateDBInstance(c.Context(), store.CreateDBInstanceParams{
			ProjectID: proj.ID,
			Name:      strings.TrimPrefix(node.Plan.Container, "dbstudio-"),
			Kind:      "clickhouse",
			Image:     node.Plan.Image,
			Version:   versionOf(node.Plan.Image),

			ContainerName: node.Plan.Container,
			VolumeName:    node.Plan.Volume,
			Port:          node.Plan.Port,
			Values:        values,
			Secrets:       secrets,

			Cluster: plan.Name, Role: node.Role,
			Shard: node.Shard, Replica: node.Replica,

			ActorID: actorID(c), Actor: actorName(c),
		})
		if err != nil {
			// 앞서 적은 줄을 되돌린다. 도커에는 아직 아무것도 만들지 않았다.
			for _, made := range rows {
				_ = s.st.DeleteDBInstance(c.Context(), made.ID)
			}
			if errors.Is(err, store.ErrInstanceNameTaken) {
				return fail(c, fiber.StatusConflict, "duplicate",
					nameTakenMessage(c, s, strings.TrimPrefix(node.Plan.Container, "dbstudio-")))
			}
			return err
		}
		rows = append(rows, in)
		creates = append(creates, provision.ClusterCreate{
			InstanceID: in.ID, Plan: node.Plan, Role: node.Role,
		})
	}

	s.audit(c, store.AuditParams{
		Action: store.ActionDBInstanceCreated, TargetType: "dbinstance", TargetID: rows[0].ID,
		Detail: map[string]any{
			"cluster": plan.Name, "shards": req.Cluster.Shards,
			"replicas": req.Cluster.Replicas, "nodes": len(rows), "project": proj.Name,
		},
	})

	var onReady func(string)
	if req.Register == nil || *req.Register {
		actor, env := actorID(c), req.Environment
		onReady = func(id string) { s.registerClusterNode(id, actor, env) }
	}
	if err := s.provisioner().CreateCluster(creates, onReady); err != nil {
		return err
	}
	return c.Status(fiber.StatusAccepted).JSON(fiber.Map{
		"cluster":   plan.Name,
		"instances": rows,
		"warnings":  plan.Warnings,
		"nodes":     len(rows),
	})
}

// registerClusterNode는 클러스터의 **첫 노드만** 커넥션으로 등록한다.
//
// ── 왜 하나만인가 ───────────────────────────────────────────────────
// 노드 넷을 다 등록하면 커넥션 목록에 같은 데이터가 넷으로 보인다. 그런데
// 그것들은 서로 다른 DB 가 아니라 한 클러스터의 입구들이다 — 어디로 물어도
// 같은 답이 온다(Distributed 표를 쓰면).
//
// 조정자는 등록하지 않는다. DB 가 아니라 복제를 맞추는 것이고, 붙어도 할 수
// 있는 일이 없다.
func (s *Server) registerClusterNode(instanceID, actorID string, env model.Environment) {
	ctx, cancel := backgroundCtx()
	defer cancel()

	in, err := s.st.GetDBInstance(ctx, instanceID)
	if err != nil {
		return
	}
	if in.Role == "keeper" {
		return
	}
	// 첫 샤드의 첫 레플리카만. 나머지는 같은 클러스터의 다른 입구다.
	if in.Shard != 1 || in.Replica != 1 {
		return
	}
	s.registerInstanceWith(instanceID, actorID, env, model.Options{
		// 이 값이 있으면 DDL 에 ON CLUSTER 가 붙는다. 클러스터로 만들어 놓고
		// 이것을 비워 두면, 화면에서 만든 표가 노드 한 대에만 생긴다.
		"cluster": provision.ClusterName,
	})
}
