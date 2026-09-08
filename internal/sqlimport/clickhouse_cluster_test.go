package sqlimport

import (
	"os"
	"strings"
	"testing"
)

// 클러스터 DDL 을 통째로 불러온다.
//
// 이 검사가 세는 것은 "몇 개를 읽었나"다. 하나가 안 읽히는 것보다 그 뒤가 전부
// 사라지는 것이 훨씬 흔하고, 그때는 화면에 표 하나만 나오고 오류는 없다.
func TestClickHouseClusterScript(t *testing.T) {
	raw, err := os.ReadFile("testdata/clickhouse_cluster.sql")
	if err != nil {
		t.Fatal(err)
	}
	res, err := Parse("clickhouse", string(raw))
	if err != nil {
		t.Fatalf("불러오지 못했습니다: %v", err)
	}

	wantTables := []string{"events_local", "events", "signals_local", "queue"}
	if len(res.Tables) != len(wantTables) {
		got := []string{}
		for _, tb := range res.Tables {
			got = append(got, tb.Name)
		}
		t.Fatalf("표 %d개를 읽었습니다(기대 %d개): %v",
			len(res.Tables), len(wantTables), got)
	}
	for _, name := range wantTables {
		if findTable(res, name) == nil {
			t.Errorf("%s 를 읽지 못했습니다", name)
		}
	}
	if len(res.Views) != 2 {
		t.Errorf("뷰 %d개를 읽었습니다(기대 2개)", len(res.Views))
	}

	// ON CLUSTER 가 있어도 꼬리 절은 그대로 읽혀야 한다.
	local := findTable(res, "events_local")
	want := map[string]string{
		"engine":       "ReplicatedReplacingMergeTree('/clickhouse/tables/{shard}/events', '{replica}', ingested_at)",
		"order_by":     "user_id, occurred_at, event_id",
		"partition_by": "toYYYYMM(occurred_at)",
		"ttl":          "toDateTime(occurred_at) + INTERVAL 18 MONTH",
	}
	for k, v := range want {
		if got := local.Options[k]; got != v {
			t.Errorf("events_local.%s = %q, 기대: %q", k, got, v)
		}
	}
	// 시간대가 붙은 타입과 Map 을 원문 그대로 지켜야 한다.
	if got := local.Column("occurred_at").RawType; got != "DateTime64(3, 'Asia/Seoul')" {
		t.Errorf("occurred_at 타입 = %q", got)
	}
	if got := local.Column("ext").RawType; got != "Map(String, String)" {
		t.Errorf("ext 타입 = %q", got)
	}

	// 사전은 건너뛰지만 왜 건너뛰었는지 적혀야 한다.
	notes := strings.Join(res.Notes, " ")
	for _, frag := range []string{"ON CLUSTER c1", "사전(DICTIONARY)"} {
		if !strings.Contains(notes, frag) {
			t.Errorf("%q 안내가 없습니다: %v", frag, res.Notes)
		}
	}
	// 같은 안내를 문장마다 쌓지 않는다.
	if n := strings.Count(notes, "ON CLUSTER"); n != 1 {
		t.Errorf("ON CLUSTER 안내가 %d번 나옵니다(한 번이어야 합니다)", n)
	}
}

// `CREATE TABLE b AS a` 는 a 의 컬럼을 그대로 쓴다.
//
// 못 읽으면 도면에서 표의 절반이 사라지고, 사라진 쪽이 응용이 실제로 읽고 쓰는
// 분산 표다.
func TestClickHouseDistributedClonesColumns(t *testing.T) {
	raw, _ := os.ReadFile("testdata/clickhouse_cluster.sql")
	res, _ := Parse("clickhouse", string(raw))

	local, dist := findTable(res, "events_local"), findTable(res, "events")
	if local == nil || dist == nil {
		t.Fatal("표을 읽지 못했습니다")
	}
	if len(dist.Columns) != len(local.Columns) {
		t.Fatalf("분산 표의 컬럼이 %d개입니다(원본 %d개)",
			len(dist.Columns), len(local.Columns))
	}
	for i, c := range local.Columns {
		d := dist.Columns[i]
		if d.Name != c.Name || d.RawType != c.RawType {
			t.Errorf("%d번 컬럼이 다릅니다: %s %s ≠ %s %s",
				i, d.Name, d.RawType, c.Name, c.RawType)
		}
	}
	// 엔진은 베끼지 않는다. 두 표는 서로 다른 물건이다.
	if !strings.HasPrefix(dist.Options["engine"], "Distributed(") {
		t.Errorf("분산 표의 엔진 = %q", dist.Options["engine"])
	}
	// 분산 표에는 정렬 키가 없다. 원본의 것을 베끼면 서버가 문장을 거절한다.
	if dist.Options["order_by"] != "" {
		t.Errorf("분산 표에 정렬 키가 붙었습니다: %q", dist.Options["order_by"])
	}
}

// 구체화 뷰는 종류와 대상 표를 함께 읽어야 한다. 평범한 뷰로 담으면 문장은
// 통과하고 대상 표에 행이 쌓이지 않는다.
func TestClickHouseMaterializedViewFromScript(t *testing.T) {
	raw, _ := os.ReadFile("testdata/clickhouse_cluster.sql")
	res, _ := Parse("clickhouse", string(raw))

	var mv, plain bool
	for _, v := range res.Views {
		switch v.Name {
		case "mv_signals":
			mv = true
			if !v.Materialized {
				t.Error("mv_signals 를 평범한 뷰로 읽었습니다")
			}
			if v.Target != "signals_local" {
				t.Errorf("mv_signals 의 대상 표 = %q, 기대: signals_local", v.Target)
			}
			if !strings.Contains(v.Definition, "FROM events_local") {
				t.Errorf("정의가 잘렸습니다: %q", v.Definition)
			}
		case "v_recent":
			plain = true
			if v.Materialized {
				t.Error("v_recent 를 구체화 뷰로 읽었습니다")
			}
		}
	}
	if !mv || !plain {
		t.Errorf("뷰를 찾지 못했습니다 (구체화=%v 평범=%v)", mv, plain)
	}
}

// 문장 하나를 읽은 뒤 그 뒤를 계속 읽어야 한다.
//
// 이것을 따로 세는 이유: ClickHouse 꼬리 절을 읽는 코드가 세미콜론을 만났을 때
// 토큰 자리를 끝으로 밀어 버린 적이 있다. 그러면 **첫 표만 남고 나머지가 전부
// 사라지는데** 오류는 나지 않는다. 한 문장짜리 검사로만 보면 통과한다.
func TestClickHouseTailDoesNotEatNextStatements(t *testing.T) {
	res, err := Parse("clickhouse", `
CREATE TABLE a (x UInt64) ENGINE = MergeTree ORDER BY (x) SETTINGS index_granularity = 8192;
CREATE TABLE b (y UInt64) ENGINE = MergeTree ORDER BY (y) TTL toDateTime(y) + INTERVAL 1 DAY;
CREATE TABLE c (z UInt64) ENGINE = MergeTree ORDER BY (z) COMMENT '셋째';
CREATE TABLE d (w UInt64) ENGINE = MergeTree ORDER BY (w);`)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Tables) != 4 {
		got := []string{}
		for _, tb := range res.Tables {
			got = append(got, tb.Name)
		}
		t.Fatalf("표 %d개만 읽었습니다(기대 4개): %v — 꼬리 절이 뒤 문장을 먹었습니다",
			len(res.Tables), got)
	}
	if got := findTable(res, "c").Comment; got != "셋째" {
		t.Errorf("주석 = %q", got)
	}
}
