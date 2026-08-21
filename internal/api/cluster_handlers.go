package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"

	"dbstudio/internal/buildinfo"
	"dbstudio/internal/cluster"
	"dbstudio/internal/store"
)

// 클러스터 API.
//
// 두 종류가 있다.
//   - 노드끼리 부르는 것(join·heartbeat·changes·snapshot·audit): 사람의 세션이 아니라
//     공용 비밀로 인증한다. 노드는 사람이 아니고, 사람 계정으로 붙이면 그 계정이 사라질 때
//     클러스터가 멈춘다.
//   - 사람이 보는 것(status·노드 목록·내리기): 슈퍼 어드민 전용이다. 노드 주소와 복제 지연은
//     인프라 정보이고, 노드를 내리는 것은 그 노드가 담당하던 일이 멈춘다는 뜻이다.

// requireClusterSecret은 노드 사이 호출을 인증한다.
func (s *Server) requireClusterSecret(c *fiber.Ctx) error {
	if s.cluster == nil || !s.cluster.Enabled() {
		return fail(c, fiber.StatusNotFound, "cluster_off", "이 서버는 클러스터로 동작하지 않습니다")
	}
	token := strings.TrimSpace(strings.TrimPrefix(c.Get(fiber.HeaderAuthorization), "Bearer "))
	if !s.cluster.SecretOK(token) {
		return fail(c, fiber.StatusUnauthorized, "cluster_auth", "클러스터 비밀이 맞지 않습니다")
	}
	return c.Next()
}

// requireMaster는 마스터만 처리할 수 있는 호출을 막는다.
//
// 리플리카가 조용히 처리하면 안 되는 이유: 그 답은 자기 복사본을 근거로 만들어지고,
// 받는 쪽은 그것을 마스터의 답으로 믿는다. 두 개의 진실이 생기는 첫걸음이다.
func (s *Server) requireMaster(c *fiber.Ctx) error {
	if s.cluster == nil || !s.cluster.IsMaster() {
		return fail(c, fiber.StatusConflict, "not_master", "이 노드는 마스터가 아닙니다")
	}
	return c.Next()
}

func (s *Server) handleClusterJoin(c *fiber.Ctx) error {
	var req cluster.JoinRequest
	if err := c.BodyParser(&req); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "요청 형식이 올바르지 않습니다")
	}
	req.NodeID = strings.TrimSpace(req.NodeID)
	if req.NodeID == "" {
		return fail(c, fiber.StatusBadRequest, "bad_request", "노드 ID가 필요합니다")
	}
	if strings.TrimSpace(req.Name) == "" {
		req.Name = req.NodeID
	}
	if err := s.st.UpsertClusterNode(c.Context(), store.ClusterNode{
		ID: req.NodeID, Name: req.Name, Role: store.NodeRoleReplica,
		Address: strings.TrimRight(req.Address, "/"), Version: req.Version, Platform: req.Platform,
	}); err != nil {
		return err
	}
	minSeq, maxSeq, err := s.st.ReplBounds(c.Context())
	if err != nil {
		return err
	}
	schema, err := s.st.SchemaVersion(c.Context())
	if err != nil {
		return err
	}
	s.audit(c, store.AuditParams{
		ActorName: "cluster", Action: "cluster.join", TargetType: "node", TargetID: req.NodeID,
		Detail: map[string]any{"name": req.Name, "address": req.Address, "version": req.Version},
	})
	return c.JSON(cluster.JoinResponse{
		MasterID: s.cluster.NodeID(), MasterName: s.cluster.Name(),
		MasterSeq: maxSeq, MinSeq: minSeq, SchemaVersion: schema,
	})
}

func (s *Server) handleClusterHeartbeat(c *fiber.Ctx) error {
	var req cluster.HeartbeatRequest
	if err := c.BodyParser(&req); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "요청 형식이 올바르지 않습니다")
	}
	snapshot := ""
	if req.HostSnapshot != nil {
		if raw, err := c.App().Config().JSONEncoder(req.HostSnapshot); err == nil {
			snapshot = string(raw)
		}
	}
	if err := s.st.TouchClusterNode(c.Context(), req.NodeID, req.AppliedSeq, snapshot); err != nil {
		if err == store.ErrNotFound {
			// 마스터가 이 노드를 모른다(목록에서 내려갔거나 마스터가 새 DB로 시작했다).
			// 409로 답해 리플리카가 다시 참여하게 한다 — 조용히 성공을 돌려주면
			// 그 노드는 목록에 없는 채로 계속 돈다.
			return fail(c, fiber.StatusConflict, "unknown_node", "등록되지 않은 노드입니다. 다시 참여하세요")
		}
		return err
	}
	minSeq, maxSeq, err := s.st.ReplBounds(c.Context())
	if err != nil {
		return err
	}
	return c.JSON(cluster.HeartbeatResponse{MasterSeq: maxSeq, MinSeq: minSeq})
}

func (s *Server) handleClusterChanges(c *fiber.Ctx) error {
	since, _ := strconv.ParseInt(c.Query("since", "0"), 10, 64)
	limit, _ := strconv.Atoi(c.Query("limit", "500"))
	minSeq, maxSeq, err := s.st.ReplBounds(c.Context())
	if err != nil {
		return err
	}
	out := cluster.ChangesResponse{MinSeq: minSeq, MaxSeq: maxSeq}

	// 요청 지점 바로 다음이 로그에 없으면 그 사이의 변경은 이미 잘려 나갔다.
	// (로그가 비어 있는 경우는 예외다 — 아직 아무 변경도 없었다는 뜻이다.)
	if minSeq > 0 && since+1 < minSeq {
		out.NeedSnapshot = true
		return c.JSON(out)
	}
	changes, err := s.st.ReplChanges(c.Context(), since, limit)
	if err != nil {
		return err
	}
	out.Changes = changes
	return c.JSON(out)
}

// handleClusterSnapshot은 메타 DB 스냅샷을 내보낸다.
//
// 파일로 떠서 흘려보내는 이유: 메타 DB는 수백 MB가 될 수 있고, 메모리에 담으면 리플리카
// 하나가 붙을 때마다 마스터의 메모리를 그만큼 먹는다.
func (s *Server) handleClusterSnapshot(c *fiber.Ctx) error {
	path := filepath.Join(s.cfg.DataDir, "snapshot-out.db")
	// 지난 번 것을 여기서 지운다. 응답을 다 보낸 시점에는 이 핸들러가 이미 끝나 있어
	// 그때 지울 방법이 없다(본문은 핸들러 반환 뒤에 흘러 나간다).
	_ = os.Remove(path)
	if err := s.st.SnapshotTo(c.Context(), path); err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	c.Set(fiber.HeaderContentType, "application/octet-stream")
	c.Response().SetBodyStream(f, int(info.Size()))
	return nil
}

// handleClusterAudit은 다른 노드가 실행한 작업의 감사 기록을 받는다.
func (s *Server) handleClusterAudit(c *fiber.Ctx) error {
	var p store.AuditParams
	if err := c.BodyParser(&p); err != nil {
		return fail(c, fiber.StatusBadRequest, "bad_request", "요청 형식이 올바르지 않습니다")
	}
	if strings.TrimSpace(p.Action) == "" {
		return fail(c, fiber.StatusBadRequest, "bad_request", "action이 필요합니다")
	}
	if err := s.st.Audit(c.Context(), p); err != nil {
		return err
	}
	return c.JSON(fiber.Map{"ok": true})
}

// handleClusterStatus는 화면이 보는 클러스터 상태다.
func (s *Server) handleClusterStatus(c *fiber.Ctx) error {
	if s.cluster == nil {
		return c.JSON(fiber.Map{"status": cluster.Status{Role: cluster.RoleStandalone}, "nodes": []any{}})
	}
	status := s.cluster.Status(c.Context())
	nodes := []*nodeView{}
	if s.cluster.Enabled() {
		list, err := s.st.ListClusterNodes(c.Context())
		if err != nil {
			return err
		}
		for _, n := range list {
			nodes = append(nodes, newNodeView(n, status, s.cluster.NodeID()))
		}
	}
	return c.JSON(fiber.Map{
		"status":  status,
		"nodes":   nodes,
		"version": buildinfo.Get().Version,
	})
}

// staleAfter는 이 시간 동안 하트비트가 없으면 "소식 끊김"으로 본다.
// cluster.checkStale이 이벤트를 여는 기준(하트비트 주기 × 4)과 맞춘다 — 화면과 이벤트가
// 다른 기준을 쓰면 "목록은 정상인데 이벤트는 끊겼다고 한다"가 된다.
const staleAfter = 45 * time.Second

// nodeView는 노드 한 줄이다. 호스트 스냅샷은 원문 JSON 그대로 싣는다.
type nodeView struct {
	*store.ClusterNode
	Host  any   `json:"host,omitempty"`
	Lag   int64 `json:"lag"`
	IsMe  bool  `json:"isMe"`
	Stale bool  `json:"stale"`
}

func newNodeView(n *store.ClusterNode, status cluster.Status, myID string) *nodeView {
	v := &nodeView{ClusterNode: n, IsMe: n.ID == myID}
	switch {
	case v.IsMe:
		// 지금 보고 있는 노드의 지연은 **살아 있는 값**으로 보여준다. 목록에 적힌 값은
		// 마지막 하트비트 때의 것이라 몇 초 뒤처지는데, 화면 위쪽의 "동기화됨"과 나란히
		// 놓이면 같은 노드가 두 가지를 말하는 것처럼 보인다.
		v.Lag = status.Lag
	case n.Role == store.NodeRoleReplica:
		v.Lag = status.MasterSeq - n.AppliedSeq
		if v.Lag < 0 {
			v.Lag = 0
		}
	}
	if n.HostSnapshot != "" && n.HostSnapshot != "{}" {
		v.Host = json.RawMessage(n.HostSnapshot)
	}
	// 소식이 끊긴 기준은 하트비트 주기의 네 배다(cluster.checkStale과 같은 기준).
	v.Stale = n.Status == "active" && time.Since(n.LastSeenAt) > staleAfter
	return v
}

// handleRemoveClusterNode는 노드를 목록에서 내린다.
func (s *Server) handleRemoveClusterNode(c *fiber.Ctx) error {
	id := c.Params("id")
	if s.cluster != nil && id == s.cluster.NodeID() {
		return fail(c, fiber.StatusBadRequest, "self", "지금 보고 있는 노드는 내릴 수 없습니다")
	}
	node, err := s.st.GetClusterNode(c.Context(), id)
	if err != nil {
		return err
	}
	if err := s.st.RemoveClusterNode(c.Context(), id); err != nil {
		return err
	}
	s.audit(c, store.AuditParams{
		Action: "cluster.node.removed", TargetType: "node", TargetID: id,
		Detail: map[string]any{"name": node.Name, "role": node.Role},
	})
	return c.JSON(fiber.Map{"ok": true})
}
