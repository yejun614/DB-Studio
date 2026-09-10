package provision

import (
	"fmt"
	"strings"
	"testing"
)

func clusterFor(t *testing.T, shards, replicas int) *ClusterPlan {
	t.Helper()
	p, err := BuildCluster(ClusterSpec{
		Name: "chc", Version: "24.8", Shards: shards, Replicas: replicas,
		Values: map[string]string{"password": "Pw1234!aB", "database": "appdb"},
	})
	if err != nil {
		t.Fatalf("%dx%d: %v", shards, replicas, err)
	}
	return p
}

// 노드 수는 샤드 × 레플리카이고, 복제가 있으면 조정자가 하나 더 붙는다.
func TestClusterNodeCount(t *testing.T) {
	cases := []struct{ shards, replicas, nodes, keepers int }{
		{1, 1, 1, 0}, // 조정자 없음
		{2, 1, 2, 0},
		{1, 2, 2, 1},
		{2, 2, 4, 1},
		{3, 2, 6, 1},
	}
	for _, tc := range cases {
		p := clusterFor(t, tc.shards, tc.replicas)
		nodes, keepers := 0, 0
		for _, n := range p.Nodes {
			if n.Role == "keeper" {
				keepers++
			} else {
				nodes++
			}
		}
		if nodes != tc.nodes || keepers != tc.keepers {
			t.Errorf("%dx%d → 노드 %d·조정자 %d (기대 %d·%d)",
				tc.shards, tc.replicas, nodes, keepers, tc.nodes, tc.keepers)
		}
	}
}

// macros 는 **노드마다 달라야** 한다.
//
// ReplicatedMergeTree 의 경로에 {shard} 와 {replica} 가 들어간다. 값이 같으면
// 서로 다른 노드가 같은 복제본이라고 우기게 되고, 그러면 데이터가 조용히 섞인다.
func TestClusterMacrosAreUniquePerNode(t *testing.T) {
	p := clusterFor(t, 2, 2)
	seen := map[string]string{}
	for _, n := range p.Nodes {
		if n.Role == "keeper" {
			continue
		}
		macros := n.Plan.Files[macrosConfigPath]
		if macros == "" {
			t.Fatalf("%s 에 macros 가 없습니다", n.Plan.Container)
		}
		if prev, dup := seen[macros]; dup {
			t.Errorf("%s 와 %s 의 macros 가 같습니다", prev, n.Plan.Container)
		}
		seen[macros] = n.Plan.Container

		want := fmt.Sprintf("<shard>%d</shard>", n.Shard)
		if !strings.Contains(macros, want) {
			t.Errorf("%s: %s 가 없습니다:\n%s", n.Plan.Container, want, macros)
		}
	}
}

// remote_servers 는 **모든 노드가 같은 것**을 봐야 한다.
//
// 하나라도 다르면 그 노드만 다른 클러스터를 보게 되고, 그것은 질의가 절반만
// 도는 모양으로 나타난다.
func TestClusterTopologyIsTheSameEverywhere(t *testing.T) {
	p := clusterFor(t, 2, 2)
	var first string
	for _, n := range p.Nodes {
		if n.Role == "keeper" {
			continue
		}
		got := n.Plan.Files[clusterConfigPath]
		if got == "" {
			t.Fatalf("%s 에 클러스터 설정이 없습니다", n.Plan.Container)
		}
		if first == "" {
			first = got
			continue
		}
		if got != first {
			t.Errorf("%s 가 다른 클러스터를 봅니다", n.Plan.Container)
		}
	}
	// 네 노드가 모두 이름으로 적혀 있어야 한다.
	for _, want := range []string{
		"dbstudio-chc-s1r1", "dbstudio-chc-s1r2", "dbstudio-chc-s2r1", "dbstudio-chc-s2r2",
	} {
		if !strings.Contains(first, want) {
			t.Errorf("%s 가 클러스터 설정에 없습니다", want)
		}
	}
}

// 노드끼리 붙을 계정이 들어 있어야 한다.
//
// ── 이 검사가 지키는 것 ─────────────────────────────────────────────
// 계정을 적지 않으면 노드끼리 `default` 로 비밀번호 없이 붙으려 하고, 비밀번호를
// 건 클러스터에서는 실패한다. **그 실패가 조용하다**: 분산 표에 INSERT 하면
// 성공으로 돌아오고, 데이터는 다른 샤드로 가지 못한 채 큐에 쌓인다. 살아 있는
// 클러스터에서 실제로 그렇게 됐다 — 500 줄이 사라지고 세어 보고서야 알았다.
func TestClusterNodesCarryCredentials(t *testing.T) {
	p := clusterFor(t, 2, 2)
	for _, n := range p.Nodes {
		if n.Role == "keeper" {
			continue
		}
		cfg := n.Plan.Files[clusterConfigPath]
		if !strings.Contains(cfg, "<user>default</user>") {
			t.Errorf("%s: 노드끼리 쓸 계정이 없습니다", n.Plan.Container)
		}
		if !strings.Contains(cfg, "<password>Pw1234!aB</password>") {
			t.Errorf("%s: 노드끼리 쓸 비밀번호가 없습니다", n.Plan.Container)
		}
		break
	}
}

// XML 에 넣을 수 없는 글자는 바꿔야 한다.
//
// 비밀번호에 & 나 < 가 들어가면 설정 파일이 깨지고, ClickHouse 는 그 파일을
// 통째로 무시한다 — 클러스터 설정이 조용히 사라진다.
func TestClusterEscapesXML(t *testing.T) {
	p, err := BuildCluster(ClusterSpec{
		Name: "x", Version: "24.8", Shards: 1, Replicas: 2,
		Values: map[string]string{"password": `a&b<c>"d'`},
	})
	if err != nil {
		t.Fatalf("%v", err)
	}
	for _, n := range p.Nodes {
		if n.Role == "keeper" {
			continue
		}
		cfg := n.Plan.Files[clusterConfigPath]
		if strings.Contains(cfg, "a&b<c>") {
			t.Errorf("XML 을 깨뜨리는 값이 그대로 들어갔습니다:\n%s", cfg)
		}
		if !strings.Contains(cfg, "a&amp;b&lt;c&gt;") {
			t.Errorf("바꿔 적히지 않았습니다:\n%s", cfg)
		}
		break
	}
}

// 조정자는 DB 가 아니다.
//
// 레시피의 헬스체크(clickhouse-client)를 그대로 쓰면 잘 도는 조정자가 영원히
// "뜨는 중"으로 남는다. 그리고 `keeper` 만 적으면 이미지가 그것을 실행 파일로
// 찾다 죽는다("exec: keeper: not found") — 실제로 그렇게 죽었다.
func TestKeeperRunsAsKeeper(t *testing.T) {
	p := clusterFor(t, 1, 2)
	var keeper *ClusterNode
	for i := range p.Nodes {
		if p.Nodes[i].Role == "keeper" {
			keeper = &p.Nodes[i]
		}
	}
	if keeper == nil {
		t.Fatal("조정자가 없습니다")
	}
	if len(keeper.Plan.Args) == 0 || keeper.Plan.Args[0] != "clickhouse-keeper" {
		t.Errorf("실행 명령 = %v (기대 clickhouse-keeper)", keeper.Plan.Args)
	}
	if len(keeper.Plan.Health) == 0 || !strings.Contains(strings.Join(keeper.Plan.Health, " "), "ruok") {
		t.Errorf("헬스체크 = %v (조정자는 clickhouse-client 로 물어볼 수 없다)", keeper.Plan.Health)
	}
	// 밖으로 포트를 열지 않는다. 열어 두면 아무나 클러스터의 메타데이터를 만진다.
	if keeper.Plan.HostPort != 0 {
		t.Errorf("조정자가 호스트 포트를 엽니다: %d", keeper.Plan.HostPort)
	}
	// 노드는 조정자를 찾을 수 있어야 한다.
	for _, n := range p.Nodes {
		if n.Role == "keeper" {
			continue
		}
		if !strings.Contains(n.Plan.Files[keeperClientPath], "dbstudio-chc-keeper") {
			t.Errorf("%s 가 조정자를 찾지 못합니다", n.Plan.Container)
		}
	}
}

// 레플리카가 하나면 조정자를 띄우지 않고, 그 사실을 말한다.
func TestClusterWarnsAboutWhatItIsNot(t *testing.T) {
	p := clusterFor(t, 2, 1)
	joined := strings.Join(p.Warnings, " ")
	if !strings.Contains(joined, "복제가 없습니다") {
		t.Errorf("복제가 없다는 안내가 없습니다: %v", p.Warnings)
	}
	// 한 기계에 다 뜬다는 것은 언제나 말해야 한다.
	if !strings.Contains(joined, "한 대에 뜹니다") {
		t.Errorf("한 기계에 다 뜬다는 안내가 없습니다: %v", p.Warnings)
	}
	for _, n := range p.Nodes {
		if n.Role == "keeper" {
			t.Error("레플리카가 1 인데 조정자를 띄웁니다")
		}
		if _, has := n.Plan.Files[keeperClientPath]; has {
			t.Error("조정자가 없는데 조정자 설정을 넣었습니다")
		}
	}
}

// 잘못된 구성은 만들기 전에 막는다.
func TestBuildClusterRejectsBadShapes(t *testing.T) {
	bad := []ClusterSpec{
		{Name: "x", Shards: 0, Replicas: 1},
		{Name: "x", Shards: 1, Replicas: 0},
		{Name: "x", Shards: 99, Replicas: 1},
		{Name: "x", Shards: 1, Replicas: 99},
		{Name: "../탈출", Shards: 1, Replicas: 1},
		{Name: strings.Repeat("a", 30), Shards: 1, Replicas: 1},
	}
	for _, spec := range bad {
		spec.Version = "24.8"
		spec.Values = map[string]string{"password": "pw"}
		if _, err := BuildCluster(spec); err == nil {
			t.Errorf("%+v 를 받아들였습니다", spec)
		}
	}
}
