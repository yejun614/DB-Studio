package cluster

import (
	"context"
	"encoding/json"
	"os"
	"runtime"
	"time"

	"dbstudio/internal/buildinfo"
	"dbstudio/internal/store"
)

// 노드의 배경 루프.
//
// 리플리카: 참여 → 변경 받기 → 하트비트. 마스터: 로그 정리 → 소식 없는 노드 감지.

// Run은 역할에 맞는 루프를 돈다. ctx가 끝나면 반환한다.
func (c *Cluster) Run(ctx context.Context) {
	switch {
	case c.IsMaster():
		c.runMaster(ctx)
	case c.IsReplica():
		c.runReplica(ctx)
	}
}

// RegisterSelf는 마스터가 자기 자신을 노드 목록에 올린다.
func (c *Cluster) RegisterSelf(ctx context.Context) error {
	b := buildinfo.Get()
	return c.st.UpsertClusterNode(ctx, store.ClusterNode{
		ID: c.id, Name: c.cfg.NodeName, Role: store.NodeRoleMaster,
		Address: c.cfg.Advertise, Version: b.Version, Platform: platform(),
	})
}

func platform() string { return runtime.GOOS + "/" + runtime.GOARCH }

func (c *Cluster) runMaster(ctx context.Context) {
	c.log.Info("클러스터 마스터로 동작합니다", "node", c.cfg.NodeName, "id", c.id)

	// 트리거는 마스터에만 단다. 이것이 있어야 복제 로그가 쌓인다.
	if n, err := c.st.InstallReplTriggers(ctx); err != nil {
		c.log.Error("복제 트리거를 달지 못했습니다. 리플리카가 변경을 받지 못합니다", "err", err)
		c.setErr(err)
	} else {
		c.log.Info("복제 트리거 준비", "tables", n)
	}
	if err := c.RegisterSelf(ctx); err != nil {
		c.log.Error("노드 목록에 자신을 올리지 못했습니다", "err", err)
	}

	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.maintain(ctx)
		}
	}
}

// maintain은 마스터의 정기 정리다.
func (c *Cluster) maintain(ctx context.Context) {
	if n, err := c.st.PruneReplLog(ctx, c.cfg.LogKeep, c.cfg.LogMaxRows); err != nil {
		c.log.Warn("복제 로그 정리 실패", "err", err)
	} else if n > 0 {
		c.log.Debug("복제 로그 정리", "removed", n)
	}
	// 마스터 자신의 하트비트. 이 값이 멈춰 있으면 화면에서 마스터가 죽은 것처럼 보인다.
	var snap any
	if c.hostSnap != nil {
		snap = c.hostSnap()
	}
	_ = c.TouchSelf(ctx, snap)
	c.checkStale(ctx)
}

// TouchSelf는 마스터가 자기 하트비트를 기록한다.
func (c *Cluster) TouchSelf(ctx context.Context, hostSnapshot any) error {
	_, max, err := c.st.ReplBounds(ctx)
	if err != nil {
		return err
	}
	return c.st.TouchClusterNode(ctx, c.id, max, encodeSnapshot(hostSnapshot))
}

// checkStale은 소식이 끊긴 노드를 이벤트로 알린다.
//
// 이벤트로 내보내는 이유: 노드가 조용히 빠지면 그 노드가 담당하던 커넥션의 지표만
// 멈추고, 화면에서는 "그 DB가 조용하다"로 보인다. 원인과 증상이 멀어지는 대표적인 경우다.
func (c *Cluster) checkStale(ctx context.Context) {
	limit := c.cfg.HeartbeatInterval * 4
	if limit < time.Minute {
		limit = time.Minute
	}
	nodes, err := c.st.StaleClusterNodes(ctx, limit)
	if err != nil {
		c.log.Warn("노드 상태 확인 실패", "err", err)
		return
	}
	stale := map[string]bool{}
	for _, n := range nodes {
		if n.ID == c.id {
			continue // 자기 자신은 방금 갱신했다
		}
		stale[n.ID] = true
		// 지표 이름에 노드를 넣는 이유: 이벤트는 (커넥션, 종류, 지표)로 중복을 합친다.
		// 이름이 같으면 세 노드가 끊겨도 이벤트는 하나로 보이고, 하나가 돌아오면
		// 나머지 둘의 이벤트까지 함께 닫힌다.
		_, _, err := c.st.OpenEvent(ctx, store.OpenEventParams{
			Kind:     store.EventCluster,
			Severity: store.SeverityWarning,
			Metric:   nodeMetric(n.ID),
			Message:  "노드 \"" + n.Name + "\" 에서 소식이 끊겼습니다",
			Detail: map[string]any{
				"nodeId": n.ID, "nodeName": n.Name, "role": n.Role,
				"lastSeenAt": n.LastSeenAt, "address": n.Address,
			},
		})
		if err != nil {
			c.log.Warn("노드 이벤트를 남기지 못했습니다", "node", n.Name, "err", err)
		}
	}

	// 돌아온 노드의 이벤트는 닫는다.
	all, err := c.st.ListClusterNodes(ctx)
	if err != nil {
		return
	}
	for _, n := range all {
		if stale[n.ID] || n.Status != "active" {
			continue
		}
		if _, err := c.st.ResolveEvents(ctx, "", store.EventCluster, nodeMetric(n.ID), ""); err != nil {
			c.log.Warn("노드 이벤트를 닫지 못했습니다", "err", err)
		}
	}
}

func nodeMetric(nodeID string) string { return "cluster.node:" + nodeID }

// encodeSnapshot은 하트비트로 받은 호스트 상태를 저장할 JSON 문자열로 만든다.
func encodeSnapshot(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(raw)
}

func (c *Cluster) runReplica(ctx context.Context) {
	c.log.Info("클러스터 리플리카로 동작합니다",
		"node", c.cfg.NodeName, "id", c.id, "master", c.cfg.MasterURL)

	// 리플리카에 트리거가 남아 있으면 복제로 들어온 행이 다시 로그가 된다.
	// (마스터였다가 역할이 바뀐 노드에서 실제로 일어난다.)
	if err := c.st.DropReplTriggers(ctx); err != nil {
		c.log.Warn("복제 트리거를 걷어내지 못했습니다", "err", err)
	}
	if applied, err := c.st.ReplApplied(ctx); err == nil {
		c.setApplied(applied)
	}

	sync := time.NewTicker(c.cfg.SyncInterval)
	defer sync.Stop()
	beat := time.NewTicker(c.cfg.HeartbeatInterval)
	defer beat.Stop()

	c.tryJoin(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-sync.C:
			c.syncOnce(ctx)
		case <-c.kick:
			c.syncOnce(ctx)
		case <-beat.C:
			if !c.joined {
				c.tryJoin(ctx)
				continue
			}
			if _, err := c.Heartbeat(ctx); err != nil {
				c.setErr(err)
				c.log.Warn("하트비트 실패", "err", err)
			}
		}
	}
}

func (c *Cluster) tryJoin(ctx context.Context) {
	b := buildinfo.Get()
	res, err := c.Join(ctx, JoinRequest{
		NodeID: c.id, Name: c.cfg.NodeName, Address: c.cfg.Advertise,
		Version: b.Version, Platform: platform(),
	})
	if err != nil {
		c.setErr(err)
		c.log.Warn("클러스터 참여 실패. 계속 시도합니다", "master", c.cfg.MasterURL, "err", err)
		return
	}
	c.joined = true
	c.setErr(nil)
	c.mu.Lock()
	c.masterSeq = res.MasterSeq
	c.mu.Unlock()
	c.log.Info("클러스터에 참여했습니다",
		"master", res.MasterName, "masterSeq", res.MasterSeq, "applied", c.Applied())
	c.Kick()
}

// syncOnce는 변경을 한 번 받아 적용한다.
func (c *Cluster) syncOnce(ctx context.Context) {
	if !c.joined {
		return
	}
	// 아직 한 번도 맞춰 본 적이 없으면 스냅샷부터 받는다.
	//
	// 변경 로그로 시작할 수 없는 이유: 로그에는 **트리거를 단 뒤의 변경**만 있다.
	// 이미 쓰고 있던 마스터에 새 노드가 붙으면 그 전의 데이터(계정, 커넥션, 이력 전부)는
	// 로그 어디에도 없다. 그 상태로 변경만 따라가면 리플리카는 영원히 반쯤 빈 채로
	// 남고, 그 사실은 "왜 이 노드에서는 로그인이 안 되지"로 나타난다.
	if c.Applied() == 0 {
		if err := c.bootstrap(ctx); err != nil {
			c.setErr(err)
			c.log.Error("첫 스냅샷 복제 실패. 다시 시도합니다", "err", err)
			return
		}
	}
	for {
		res, err := c.FetchChanges(ctx, c.Applied(), 500)
		if err != nil {
			c.setErr(err)
			// 한 줄은 남긴다. 조용히 멈춘 복제는 겉으로는 "조금 한가한 서버"와 같아 보인다.
			c.log.Warn("복제 변경을 받지 못했습니다", "err", err)
			return
		}
		c.mu.Lock()
		c.masterSeq = res.MaxSeq
		c.mu.Unlock()

		// 마스터의 로그가 내 적용 지점보다 뒤에 있다 = 마스터의 데이터가 새로 만들어졌다
		// (DB를 갈아 끼웠거나 다른 클러스터를 가리키고 있다). 이어 붙이면 두 DB가
		// 섞이므로 처음부터 다시 맞춘다.
		if res.MaxSeq < c.Applied() {
			c.log.Warn("마스터의 복제 지점이 이 노드보다 뒤에 있습니다. 스냅샷으로 다시 맞춥니다",
				"applied", c.Applied(), "masterSeq", res.MaxSeq)
			if err := c.bootstrap(ctx); err != nil {
				c.setErr(err)
				c.log.Error("스냅샷 복제 실패", "err", err)
			}
			return
		}

		if res.NeedSnapshot {
			c.log.Warn("복제 로그가 잘려 따라잡을 수 없습니다. 스냅샷을 받습니다",
				"applied", c.Applied(), "masterMin", res.MinSeq)
			if err := c.bootstrap(ctx); err != nil {
				c.setErr(err)
				c.log.Error("스냅샷 복제 실패", "err", err)
				return
			}
			continue
		}
		if len(res.Changes) == 0 {
			c.setErr(nil)
			c.markSynced()
			return
		}
		last, err := c.st.ApplyReplChanges(ctx, res.Changes)
		if err != nil {
			c.setErr(err)
			c.log.Error("복제 적용 실패", "err", err)
			return
		}
		c.setApplied(last)
		c.setErr(nil)
		c.markSynced()
		c.log.Debug("복제 적용", "count", len(res.Changes), "applied", last)
		if len(res.Changes) < 500 {
			return
		}
	}
}

func (c *Cluster) markSynced() {
	c.mu.Lock()
	c.lastSync = time.Now()
	c.mu.Unlock()
}

// bootstrap은 스냅샷을 받아 통째로 맞춘다.
func (c *Cluster) bootstrap(ctx context.Context) error {
	dir := os.TempDir()
	if c.snapshotDir != "" {
		dir = c.snapshotDir
	}
	path, err := c.FetchSnapshot(ctx, dir)
	if err != nil {
		return err
	}
	defer os.Remove(path)

	seq, err := c.st.LoadSnapshot(ctx, path)
	if err != nil {
		return err
	}
	c.setApplied(seq)
	c.log.Info("스냅샷으로 메타 DB를 맞췄습니다", "seq", seq)
	return nil
}
