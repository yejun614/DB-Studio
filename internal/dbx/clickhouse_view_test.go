package dbx

import "testing"

// 구체화 뷰의 대상 표(TO 절)는 system.tables 에 없어서 CREATE 문에서 읽는다.
// 놓치면 평범한 뷰가 만들어지고, 대상 표에 행이 쌓이지 않기 시작한다.
func TestClickHouseViewTarget(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		want string
	}{
		{
			name: "TO 가 있는 구체화 뷰 (드라이버가 준 실제 문장)",
			sql: "CREATE MATERIALIZED VIEW appdb.events_mv TO appdb.events " +
				"(`id` UInt64, `kind` String) AS SELECT id, kind FROM appdb.events_queue",
			want: "appdb.events",
		},
		{
			name: "TO 가 없는 구체화 뷰",
			sql: "CREATE MATERIALIZED VIEW appdb.mv (`id` UInt64) ENGINE = MergeTree " +
				"ORDER BY (id) AS SELECT id FROM appdb.src",
			want: "",
		},
		{
			name: "평범한 뷰",
			sql:  "CREATE VIEW appdb.v AS SELECT 1",
			want: "",
		},
		{
			name: "역따옴표가 붙은 이름",
			sql: "CREATE MATERIALIZED VIEW `appdb`.`mv` TO `appdb`.`dst` " +
				"AS SELECT 1 FROM appdb.src",
			want: "appdb.dst",
		},
		{
			// SELECT 안에 TO 라는 낱말이 있어도 대상 표로 읽으면 안 된다.
			name: "본문에 TO 가 나오는 뷰",
			sql: "CREATE MATERIALIZED VIEW appdb.mv TO appdb.dst " +
				"AS SELECT toString(id) AS id FROM appdb.src",
			want: "appdb.dst",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := clickhouseViewTarget(c.sql); got != c.want {
				t.Errorf("대상 표를 %q 로 읽었습니다. 기대: %q", got, c.want)
			}
		})
	}
}
