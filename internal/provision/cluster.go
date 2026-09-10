package provision

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"dbstudio/internal/applog"
)

// ClickHouse 클러스터 — 샤딩과 복제.
//
// ── 무엇이 하나가 아닌가 ────────────────────────────────────────────
// 지금까지 만든 것은 **컨테이너 하나 = DB 하나**였다. 클러스터는 그렇지 않다.
// 샤드 2 × 레플리카 2 면 노드가 넷이고, 그 넷이 서로를 알아야 하며, 복제를
// 맞추는 조정자(Keeper)가 하나 더 필요하다. 그래서 계획도 하나가 아니라 묶음이다.
//
// ── 노드가 서로를 아는 방법 ─────────────────────────────────────────
// 세 가지 설정이 각 노드에 들어간다.
//
//	remote_servers  이 클러스터에 어떤 샤드와 레플리카가 있는가(전 노드가 같다)
//	macros          나는 몇 번 샤드의 몇 번 레플리카인가(노드마다 다르다)
//	zookeeper       복제를 맞출 조정자는 어디 있는가(레플리카가 둘 이상일 때)
//
// macros 가 노드마다 달라야 하는 것이 핵심이다. ReplicatedMergeTree 의 경로에
// `{shard}` 와 `{replica}` 가 들어가는데, 그 값이 같으면 서로 다른 노드가 같은
// 복제본이라고 우기게 된다 — 그러면 데이터가 조용히 섞인다.
//
// ── 왜 Keeper 를 따로 띄우는가 ──────────────────────────────────────
// 같은 이미지로 뜨지만 하는 일이 다르다(clickhouse-keeper). 서버 안에 얹을 수도
// 있지만, 그러면 그 서버를 멈출 때 클러스터 전체의 복제가 멈춘다. 하나로 띄우는
// 것은 단일 장애점이지만, 그 사실이 눈에 보이는 편이 서버 하나에 숨어 있는
// 것보다 낫다.

const (
	// KeeperPort는 노드들이 Keeper 에 붙는 포트다.
	KeeperPort = 9181
	// KeeperRaftPort는 Keeper 끼리 쓰는 포트다(하나뿐이어도 열어 둔다).
	KeeperRaftPort = 9234
	// ClusterName은 remote_servers 에 적히는 이름이다.
	//
	// 고정해 두는 이유: 이 이름이 DDL 의 `ON CLUSTER` 에 들어가고 커넥션 옵션에도
	// 들어간다. 사람이 정하게 하면 그 둘이 어긋날 자리가 생긴다.
	ClusterName = "dbstudio"
)

// ClusterSpec은 클러스터 하나를 만드는 입력이다.
type ClusterSpec struct {
	// Name은 클러스터의 이름이다. 노드 이름의 바탕이 된다.
	Name string
	// Version은 이미지 태그다.
	Version string
	// Shards는 데이터를 나눌 조각 수다.
	Shards int
	// Replicas는 조각마다 둘 복제본 수다.
	Replicas int
	// Values는 노드마다 같게 적용할 설정이다(비밀번호·메모리 …).
	Values map[string]string
}

// ClusterNode는 묶음 안의 한 대다.
type ClusterNode struct {
	// Role은 keeper 또는 node 다.
	Role string
	// Shard·Replica는 1부터다. Keeper 는 0 이다.
	Shard   int
	Replica int
	Plan    *Plan
}

// ClusterPlan은 클러스터 하나를 만드는 계획 묶음이다.
type ClusterPlan struct {
	Name  string
	Nodes []ClusterNode
	// Warnings는 만들 수는 있지만 알아야 하는 것이다.
	Warnings []string
}

// NodeName은 샤드·레플리카 번호로 노드 이름을 만든다.
//
// 번호를 이름에 넣는 이유: `docker ps` 에서 어느 것이 무엇인지 바로 보여야
// 한다. s1r2 는 1번 샤드의 2번 레플리카다.
func NodeName(cluster string, shard, replica int) string {
	return fmt.Sprintf("%s-s%dr%d", cluster, shard, replica)
}

// KeeperName은 조정자의 이름이다.
func KeeperName(cluster string) string { return cluster + "-keeper" }

// BuildCluster는 클러스터 하나의 계획 묶음을 만든다.
func BuildCluster(spec ClusterSpec) (*ClusterPlan, error) {
	name := strings.TrimSpace(spec.Name)
	if err := validName(name); err != nil {
		return nil, err
	}
	// 이름이 길면 노드 이름이 더 길어진다(-s1r1 이 붙는다). 컨테이너 이름은
	// 도커가 받아 주지만, 사람이 목록에서 읽지 못하면 소용이 없다.
	if len(name) > 24 {
		return nil, fmt.Errorf("클러스터 이름은 24자 이내로 지으세요 (노드마다 -s1r1 이 붙습니다)")
	}
	if spec.Shards < 1 || spec.Shards > 8 {
		return nil, fmt.Errorf("샤드는 1~8 개여야 합니다")
	}
	if spec.Replicas < 1 || spec.Replicas > 4 {
		return nil, fmt.Errorf("레플리카는 1~4 개여야 합니다")
	}

	out := &ClusterPlan{Name: name}
	needKeeper := spec.Replicas > 1

	// remote_servers 는 모든 노드가 같은 것을 봐야 한다. 하나라도 다르면 그
	// 노드만 다른 클러스터를 보게 되고, 그 사실은 질의가 절반만 도는 모양으로
	// 나타난다.
	remote := renderRemoteServers(name, spec.Shards, spec.Replicas,
		Find("clickhouse").AdminAccount(spec.Values), spec.Values["password"])

	if needKeeper {
		kp, err := buildKeeper(name, spec)
		if err != nil {
			return nil, err
		}
		out.Nodes = append(out.Nodes, ClusterNode{Role: "keeper", Plan: kp})
	}

	for shard := 1; shard <= spec.Shards; shard++ {
		for replica := 1; replica <= spec.Replicas; replica++ {
			p, err := buildClusterNode(name, shard, replica, spec, remote, needKeeper)
			if err != nil {
				return nil, err
			}
			out.Nodes = append(out.Nodes, ClusterNode{
				Role: "node", Shard: shard, Replica: replica, Plan: p,
			})
		}
	}

	if spec.Replicas == 1 {
		out.Warnings = append(out.Warnings,
			"레플리카가 1 이라 복제가 없습니다. 노드가 하나 죽으면 그 샤드의 데이터를 "+
				"읽을 수 없습니다 (Keeper 도 띄우지 않습니다)")
	}
	if spec.Shards == 1 && spec.Replicas == 1 {
		out.Warnings = append(out.Warnings,
			"샤드도 레플리카도 1 이면 클러스터로 만들 이유가 거의 없습니다. "+
				"단일 노드로 만드는 편이 간단합니다")
	}
	if needKeeper {
		out.Warnings = append(out.Warnings,
			"Keeper 를 하나만 띄웁니다. 그것이 죽으면 복제가 멈춥니다(읽기·쓰기는 "+
				"계속됩니다). 운영에서는 3대를 두는 것이 보통입니다")
	}
	// 모든 노드가 한 기계에 뜬다. 그 사실을 말하지 않으면 사람은 이것으로
	// 장애를 견딜 수 있다고 생각하게 된다.
	out.Warnings = append(out.Warnings,
		"노드가 모두 이 기계 한 대에 뜹니다. 기계가 죽으면 클러스터도 함께 죽습니다 — "+
			"이 구성은 개발과 시험을 위한 것입니다")
	return out, nil
}

// buildClusterNode는 노드 한 대의 계획이다.
func buildClusterNode(cluster string, shard, replica int, spec ClusterSpec,
	remote string, needKeeper bool) (*Plan, error) {
	vals := map[string]string{}
	for k, v := range spec.Values {
		vals[k] = v
	}
	// 포트는 노드마다 달라야 한다. 사람이 하나를 정해 두면 두 번째 노드가
	// "포트가 이미 쓰이고 있다"로 뜨지 않는다.
	vals["hostPort"] = "0"

	p, err := Build(Spec{
		Kind:    "clickhouse",
		Name:    NodeName(cluster, shard, replica),
		Version: spec.Version,
		Values:  vals,
	})
	if err != nil {
		return nil, err
	}

	p.Files[clusterConfigPath] = remote
	p.Files[macrosConfigPath] = renderMacros(cluster, shard, replica)
	if needKeeper {
		p.Files[keeperClientPath] = renderKeeperClient(cluster)
	}
	return p, nil
}

// buildKeeper는 조정자 한 대의 계획이다.
//
// 같은 이미지를 쓰되 하는 일이 다르다. 서버 설정을 그대로 두면 9000 을 열고
// DB 처럼 굴려고 하므로, keeper 전용 설정만 넣고 entrypoint 를 바꾼다.
func buildKeeper(cluster string, spec ClusterSpec) (*Plan, error) {
	version := strings.TrimSpace(spec.Version)
	r := Find("clickhouse")
	if version == "" {
		version = r.DefaultVersion()
	}
	if !contains(r.Versions, version) {
		return nil, fmt.Errorf("%s 는 고를 수 없는 판본입니다", version)
	}

	name := KeeperName(cluster)
	p := &Plan{
		Recipe:    r,
		Container: ContainerName(name),
		Image:     r.Image + ":" + version,
		Port:      KeeperPort,
		// Keeper 는 밖에서 붙을 일이 없다. 호스트 포트를 열지 않는다 —
		// 열어 두면 아무나 클러스터의 메타데이터를 만질 수 있다.
		HostPort: 0,
		Env:      map[string]string{},
		Files:    map[string]string{},
		Restart:  "unless-stopped",
		// 데이터를 남긴다. Keeper 의 로그를 잃으면 복제 상태를 잃는다.
		Volume: VolumeName(name),
	}
	// `keeper` 만 적으면 이미지의 엔트리포인트가 그것을 실행 파일로 찾다 죽는다
	// ("exec: keeper: not found"). 이미지에는 clickhouse-keeper 가 있다.
	p.Args = []string{"clickhouse-keeper", "--config-file=" + keeperConfigPath}
	// 조정자는 DB 가 아니라서 clickhouse-client 로 물어볼 수 없다. 레시피의
	// 헬스체크를 그대로 쓰면 잘 도는 조정자가 영원히 "뜨는 중"으로 남는다.
	p.Health = []string{"CMD-SHELL", "clickhouse-keeper-client -q ruok | grep -q imok"}
	p.Files[keeperConfigPath] = renderKeeperConfig(cluster)
	if mb := atoiOr(spec.Values["memoryMB"], 0); mb > 0 {
		// 조정자는 서버만큼 먹지 않는다. 같은 값을 주면 기계가 두 배로 잡힌다.
		p.MemoryMB = maxInt(mb/4, 256)
	}
	return p, nil
}

// ---------- 설정 파일 ----------

const (
	clusterConfigPath = "/etc/clickhouse-server/config.d/dbstudio-cluster.xml"
	macrosConfigPath  = "/etc/clickhouse-server/config.d/dbstudio-macros.xml"
	keeperClientPath  = "/etc/clickhouse-server/config.d/dbstudio-keeper.xml"
	keeperConfigPath  = "/etc/clickhouse-keeper/keeper_config.xml"
)

// renderRemoteServers는 클러스터의 생김새다. 모든 노드가 같은 것을 본다.
//
// ── 계정을 적어야 하는 이유 ─────────────────────────────────────────
// 노드끼리 붙을 때도 인증을 한다. 적지 않으면 `default` 로 **비밀번호 없이**
// 붙으려 하고, 비밀번호를 건 클러스터에서는 그것이 실패한다.
//
// 그 실패가 조용하다는 것이 문제다. 살아 있는 클러스터에서 봤다: 분산 표에
// INSERT 를 넣으면 **성공으로 돌아오고**, 데이터는 다른 샤드로 가지 못한 채
// 큐에 쌓인다(system.distribution_queue 에 AUTHENTICATION_FAILED 가 남는다).
// 넣은 사람은 들어간 줄 알고, 세어 보면 절반만 있다.
func renderRemoteServers(cluster string, shards, replicas int, user, password string) string {
	var b strings.Builder
	b.WriteString("<!-- DB Studio 가 만든 클러스터 설정입니다. -->\n<clickhouse>\n")
	b.WriteString("    <remote_servers>\n        <" + ClusterName + ">\n")
	for shard := 1; shard <= shards; shard++ {
		b.WriteString("            <shard>\n")
		// 샤드 안의 복제본은 내용이 같다. 그중 하나에만 쓰면 나머지가
		// 따라가야 하므로, 쓰기를 받은 노드가 복제를 맡는다고 알려 준다.
		b.WriteString("                <internal_replication>true</internal_replication>\n")
		for replica := 1; replica <= replicas; replica++ {
			b.WriteString("                <replica>\n")
			fmt.Fprintf(&b, "                    <host>%s</host>\n",
				ContainerName(NodeName(cluster, shard, replica)))
			fmt.Fprintf(&b, "                    <port>%d</port>\n", 9000)
			// 계정을 적지 않으면 노드끼리 `default` 로 비밀번호 없이 붙으려 하고,
			// 비밀번호를 건 클러스터에서는 그것이 실패한다.
			if user != "" {
				fmt.Fprintf(&b, "                    <user>%s</user>\n", xmlText(user))
			}
			if password != "" {
				fmt.Fprintf(&b, "                    <password>%s</password>\n", xmlText(password))
			}
			b.WriteString("                </replica>\n")
		}
		b.WriteString("            </shard>\n")
	}
	b.WriteString("        </" + ClusterName + ">\n    </remote_servers>\n</clickhouse>\n")
	return b.String()
}

// renderMacros는 "나는 누구인가"다. 노드마다 다르다.
//
// 이것이 노드마다 달라야 하는 이유: ReplicatedMergeTree 의 경로에 {shard} 와
// {replica} 가 들어간다. 값이 같으면 서로 다른 노드가 같은 복제본이라고 우기게
// 되고, 그러면 데이터가 조용히 섞인다.
func renderMacros(cluster string, shard, replica int) string {
	var b strings.Builder
	b.WriteString("<!-- DB Studio 가 만든 노드 표시입니다. -->\n<clickhouse>\n    <macros>\n")
	fmt.Fprintf(&b, "        <cluster>%s</cluster>\n", ClusterName)
	fmt.Fprintf(&b, "        <shard>%d</shard>\n", shard)
	fmt.Fprintf(&b, "        <replica>%s</replica>\n", NodeName(cluster, shard, replica))
	b.WriteString("    </macros>\n</clickhouse>\n")
	return b.String()
}

// renderKeeperClient는 노드가 조정자를 찾는 길이다.
func renderKeeperClient(cluster string) string {
	var b strings.Builder
	b.WriteString("<!-- DB Studio 가 만든 조정자 설정입니다. -->\n<clickhouse>\n")
	b.WriteString("    <zookeeper>\n        <node>\n")
	fmt.Fprintf(&b, "            <host>%s</host>\n", ContainerName(KeeperName(cluster)))
	fmt.Fprintf(&b, "            <port>%d</port>\n", KeeperPort)
	b.WriteString("        </node>\n    </zookeeper>\n</clickhouse>\n")
	return b.String()
}

// renderKeeperConfig는 조정자 자신의 설정이다.
func renderKeeperConfig(cluster string) string {
	host := ContainerName(KeeperName(cluster))
	var b strings.Builder
	b.WriteString("<!-- DB Studio 가 만든 Keeper 설정입니다. -->\n<clickhouse>\n")
	// 밖에서 붙을 일은 없지만 같은 네트워크의 노드들은 붙어야 한다.
	b.WriteString("    <listen_host>0.0.0.0</listen_host>\n")
	b.WriteString("    <logger><level>information</level><console>1</console></logger>\n")
	b.WriteString("    <keeper_server>\n")
	fmt.Fprintf(&b, "        <tcp_port>%d</tcp_port>\n", KeeperPort)
	b.WriteString("        <server_id>1</server_id>\n")
	b.WriteString("        <log_storage_path>/var/lib/clickhouse/coordination/log</log_storage_path>\n")
	b.WriteString("        <snapshot_storage_path>/var/lib/clickhouse/coordination/snapshots</snapshot_storage_path>\n")
	b.WriteString("        <coordination_settings>\n")
	b.WriteString("            <operation_timeout_ms>10000</operation_timeout_ms>\n")
	b.WriteString("            <session_timeout_ms>30000</session_timeout_ms>\n")
	b.WriteString("        </coordination_settings>\n")
	b.WriteString("        <raft_configuration>\n            <server>\n")
	b.WriteString("                <id>1</id>\n")
	fmt.Fprintf(&b, "                <hostname>%s</hostname>\n", host)
	fmt.Fprintf(&b, "                <port>%d</port>\n", KeeperRaftPort)
	b.WriteString("            </server>\n        </raft_configuration>\n")
	b.WriteString("    </keeper_server>\n</clickhouse>\n")
	return b.String()
}

// clusterContext는 묶음 만들기의 시간 상한이다.
//
// 단일 노드보다 길게 둔다. 노드가 넷이면 이미지 내려받기는 한 번이지만 뜨기를
// 기다리는 것은 넷이고, 그 하나하나가 몇 분씩 걸릴 수 있다.
func clusterContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 45*time.Minute)
}

// xmlText는 값 하나를 XML 안에 넣을 수 있게 만든다.
//
// 비밀번호에 &, <, > 가 들어갈 수 있다. 그대로 적으면 설정 파일이 깨지고,
// ClickHouse 는 그 파일을 통째로 무시한다 — 클러스터 설정이 조용히 사라진다.
func xmlText(v string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;",
		"\"", "&quot;", "'", "&apos;")
	return r.Replace(v)
}

func atoiOr(s string, def int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
		return n
	}
	return def
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// ---------- 만들기 ----------

// ClusterCreate는 클러스터 한 대의 만들기 지시다.
type ClusterCreate struct {
	InstanceID string
	Plan       *Plan
	// Role은 keeper 또는 node 다. 조정자를 먼저 띄우기 위해 본다.
	Role string
}

// CreateCluster는 묶음을 순서대로 만든다. **바로 돌아온다.**
//
// ── 왜 순서가 있는가 ────────────────────────────────────────────────
// 조정자(Keeper)가 먼저 떠 있어야 노드가 복제를 시작할 수 있다. 노드를 먼저
// 띄우면 조정자를 찾지 못해 재시도를 돌고, 그 사이 로그가 오류로 가득 찬다 —
// 결국 뜨기는 하지만, 그 로그를 본 사람은 무언가 잘못됐다고 생각한다.
//
// ── 왜 고루틴 하나인가 ──────────────────────────────────────────────
// 노드마다 Create 를 부르면 순서를 지킬 수 없다. 하나의 흐름으로 두면 "조정자가
// 뜨고 나서 노드"가 코드의 모양 그대로가 된다. 한 대가 실패하면 거기서 멈춘다 —
// 반쯤 뜬 클러스터를 만들어 두면 사람이 그것을 고치는 편이 다시 만드는 것보다
// 어렵다.
func (r *Runner) CreateCluster(nodes []ClusterCreate, onReady func(string)) error {
	if len(nodes) == 0 {
		return fmt.Errorf("만들 노드가 없습니다")
	}
	r.mu.Lock()
	for _, n := range nodes {
		if _, busy := r.running[n.InstanceID]; busy {
			r.mu.Unlock()
			return ErrBusy
		}
	}
	ctx, cancel := clusterContext()
	for _, n := range nodes {
		r.running[n.InstanceID] = cancel
	}
	r.mu.Unlock()

	go func() {
		defer applog.Recover("provision.cluster.create")
		defer func() {
			cancel()
			r.mu.Lock()
			for _, n := range nodes {
				delete(r.running, n.InstanceID)
			}
			r.mu.Unlock()
		}()

		// 조정자 먼저. 순서를 여기서 정하므로 부르는 쪽은 정렬을 몰라도 된다.
		ordered := make([]ClusterCreate, 0, len(nodes))
		for _, n := range nodes {
			if n.Role == "keeper" {
				ordered = append(ordered, n)
			}
		}
		for _, n := range nodes {
			if n.Role != "keeper" {
				ordered = append(ordered, n)
			}
		}

		for i, n := range ordered {
			if err := r.create(ctx, n.InstanceID, n.Plan); err != nil {
				r.fail(n.InstanceID, err)
				// 뒤에 남은 것들은 시작도 못 했다는 것을 적어 준다. 그냥 두면
				// "만드는 중"으로 영원히 남고, 사람은 기다리다 새로 고친다.
				for _, rest := range ordered[i+1:] {
					r.fail(rest.InstanceID,
						fmt.Errorf("앞 노드(%s)가 실패해 시작하지 않았습니다", n.Plan.Container))
				}
				return
			}
			if onReady != nil {
				onReady(n.InstanceID)
			}
		}
	}()
	return nil
}
