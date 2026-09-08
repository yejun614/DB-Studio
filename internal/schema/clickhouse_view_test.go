package schema

import (
	"strings"
	"testing"
)

// 구체화 뷰를 평범한 뷰로 만들면 문장은 통과하지만 하는 일이 달라진다.
// INSERT 때 대상 표에 써 넣던 것이 읽을 때 계산하는 것으로 바뀌므로, 그때부터
// 대상 표에 행이 쌓이지 않는다 — 오류가 없어서 알아채기까지 한참 걸린다.
func TestClickHouseMaterializedViewKeepsItsKind(t *testing.T) {
	to := &Schema{Dialect: "clickhouse", Shape: ShapeRelational, Views: []*View{{
		Name: "events_mv", Materialized: true, Target: "appdb.events",
		Definition: "SELECT id, kind FROM appdb.events_queue",
	}}}
	up := BuildPlan("clickhouse", Diff(
		&Schema{Dialect: "clickhouse", Shape: ShapeRelational}, to)).UpSQL()

	if !strings.Contains(up, "CREATE MATERIALIZED VIEW") {
		t.Errorf("구체화 뷰가 평범한 뷰가 됐습니다:\n%s", up)
	}
	if !strings.Contains(up, "TO appdb.events") {
		t.Errorf("결과를 써 넣을 대상 표가 빠졌습니다:\n%s", up)
	}
	// ClickHouse 는 구체화 뷰에 OR REPLACE 를 허용하지 않는다.
	if strings.Contains(up, "OR REPLACE") {
		t.Errorf("ClickHouse 가 거절하는 문법이 나갔습니다:\n%s", up)
	}
}

// 평범한 뷰는 그대로 평범한 뷰여야 한다.
func TestClickHousePlainViewUnchanged(t *testing.T) {
	to := &Schema{Dialect: "clickhouse", Shape: ShapeRelational, Views: []*View{{
		Name: "v", Definition: "SELECT 1",
	}}}
	up := BuildPlan("clickhouse", Diff(
		&Schema{Dialect: "clickhouse", Shape: ShapeRelational}, to)).UpSQL()
	if strings.Contains(up, "MATERIALIZED") {
		t.Errorf("평범한 뷰가 구체화 뷰가 됐습니다:\n%s", up)
	}
}

// 구체화 뷰를 바꿀 때는 먼저 지워야 한다. IF NOT EXISTS 만 내보내면 이미 있는
// 뷰에는 아무 일도 일어나지 않는데 화면에는 "바꿨다"고 남는다.
func TestClickHouseReplaceMaterializedViewDropsFirst(t *testing.T) {
	view := func(def string) *Schema {
		return &Schema{Dialect: "clickhouse", Shape: ShapeRelational, Views: []*View{{
			Name: "events_mv", Materialized: true, Target: "appdb.events", Definition: def,
		}}}
	}
	up := BuildPlan("clickhouse", Diff(
		view("SELECT id FROM appdb.src"),
		view("SELECT id, kind FROM appdb.src"))).UpSQL()

	if !strings.Contains(up, "DROP VIEW") {
		t.Errorf("바꾸기 전에 지우지 않습니다:\n%s", up)
	}
	if strings.Index(up, "DROP VIEW") > strings.Index(up, "CREATE MATERIALIZED VIEW") {
		t.Errorf("지우는 문장이 만드는 문장보다 뒤에 있습니다:\n%s", up)
	}
}

// 종류가 바뀌면 정의가 같아도 알아채야 한다. 읽을 때 계산하던 것이 INSERT 때
// 써 넣는 것으로 바뀌는 것이라, 같은 SELECT 라도 하는 일이 다르다.
func TestClickHouseViewKindChangeIsSeen(t *testing.T) {
	base := &Schema{Dialect: "clickhouse", Shape: ShapeRelational, Views: []*View{{
		Name: "v", Definition: "SELECT 1",
	}}}
	mat := &Schema{Dialect: "clickhouse", Shape: ShapeRelational, Views: []*View{{
		Name: "v", Definition: "SELECT 1", Materialized: true, Target: "appdb.t",
	}}}
	if n := len(Diff(base, mat).Changes); n != 1 {
		t.Errorf("종류가 바뀐 것을 알아채지 못했습니다 (변경 %d건)", n)
	}
	// 대상 표만 바뀐 경우도 마찬가지다.
	other := &Schema{Dialect: "clickhouse", Shape: ShapeRelational, Views: []*View{{
		Name: "v", Definition: "SELECT 1", Materialized: true, Target: "appdb.other",
	}}}
	if n := len(Diff(mat, other).Changes); n != 1 {
		t.Errorf("대상 표가 바뀐 것을 알아채지 못했습니다 (변경 %d건)", n)
	}
}

// 정렬 키를 받지 않는 엔진에 ORDER BY 를 붙이면 서버가 문장을 거절한다.
// Kafka 표에는 브로커 주소와 토픽 이름(SETTINGS)이 표를 표이게 하는 전부다.
func TestClickHouseNonMergeTreeEngine(t *testing.T) {
	sc := &Schema{Dialect: "clickhouse", Shape: ShapeRelational, Tables: []*Table{{
		Name: "events_queue",
		Columns: []*Column{
			{Name: "id", Type: LogicalType{Base: TypeBigInt, Unsigned: true}, RawType: "UInt64"},
		},
		Options: map[string]string{
			"engine": "Kafka",
			"settings": "kafka_broker_list = 'kafka:9092', kafka_topic_list = 'events', " +
				"kafka_group_name = 'clickhouse-events', kafka_format = 'JSONEachRow'",
		},
	}}}
	up := BuildPlan("clickhouse", Diff(
		&Schema{Dialect: "clickhouse", Shape: ShapeRelational}, sc)).UpSQL()

	if strings.Contains(up, "ORDER BY") {
		t.Errorf("Kafka 엔진에 정렬 키가 붙었습니다 — 서버가 거절합니다:\n%s", up)
	}
	if !strings.Contains(up, "kafka_broker_list") {
		t.Errorf("Kafka 설정이 빠졌습니다 — 이대로는 표가 만들어지지 않습니다:\n%s", up)
	}
}

// MergeTree 는 그대로 정렬 키를 받는다. 위의 수정이 이쪽을 건드리면 안 된다.
func TestClickHouseMergeTreeStillOrders(t *testing.T) {
	sc := &Schema{Dialect: "clickhouse", Shape: ShapeRelational, Tables: []*Table{{
		Name:       "t",
		Columns:    []*Column{{Name: "id", Type: LogicalType{Base: TypeBigInt}, RawType: "Int64"}},
		Options:    map[string]string{"engine": "ReplacingMergeTree", "order_by": "id"},
		PrimaryKey: &PrimaryKey{Columns: []string{"id"}},
	}}}
	up := BuildPlan("clickhouse", Diff(
		&Schema{Dialect: "clickhouse", Shape: ShapeRelational}, sc)).UpSQL()
	if !strings.Contains(up, "ORDER BY (id)") {
		t.Errorf("MergeTree 계열에서 정렬 키가 사라졌습니다:\n%s", up)
	}
}
