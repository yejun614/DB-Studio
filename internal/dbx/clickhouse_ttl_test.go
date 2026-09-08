package dbx

import "testing"

// system.tables 에는 TTL 컬럼이 없다. CREATE 문에서 꺼내지 않으면 보관 기간이
// ERD 를 한 번 거치는 것만으로 조용히 사라진다.
//
// 아래 문장들은 드라이버가 실제로 돌려준 모양 그대로다. SHOW CREATE 는 줄을
// 나눠 보여 주지만 create_table_query 는 **한 줄**로 온다 — 처음에 그 차이를
// 모르고 줄 단위로 잘랐다가, 손으로 지어낸 입력에서만 통과하는 검사를 만들었다.
func TestClickHouseTableTTL(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		want string
	}{
		{
			name: "TTL 이 있는 표 (드라이버가 준 실제 문장)",
			sql: "CREATE TABLE appdb.page_views (`viewed_at` DateTime64(3), " +
				"`path` LowCardinality(String), `member_id` Nullable(UInt64), `ms` UInt32) " +
				"ENGINE = MergeTree PARTITION BY toYYYYMM(viewed_at) ORDER BY (path, viewed_at) " +
				"TTL toDateTime(viewed_at) + toIntervalDay(90) " +
				"SETTINGS index_granularity = 8192 COMMENT '페이지 조회 로그 (보관 90일)'",
			want: "toDateTime(viewed_at) + toIntervalDay(90)",
		},
		{
			name: "TTL 이 없는 표",
			sql: "CREATE TABLE appdb.events (`id` UInt64) ENGINE = MergeTree " +
				"ORDER BY (id) SETTINGS index_granularity = 8192",
			want: "",
		},
		{
			// 컬럼에 걸린 TTL 은 표의 TTL 이 아니다. 이것을 표 TTL 로 읽으면
			// 표 전체가 그 주기로 지워지는 DDL 이 만들어진다.
			name: "컬럼 TTL 만 있는 표",
			sql: "CREATE TABLE appdb.logs (`at` DateTime, `body` String TTL at + toIntervalDay(7)) " +
				"ENGINE = MergeTree ORDER BY (at) SETTINGS index_granularity = 8192",
			want: "",
		},
		{
			name: "TTL 뒤에 아무 절도 없는 표",
			sql: "CREATE TABLE appdb.t (`at` DateTime) ENGINE = MergeTree ORDER BY (at) " +
				"TTL at + toIntervalMonth(1)",
			want: "at + toIntervalMonth(1)",
		},
		{
			// TO VOLUME 은 TTL 절의 일부다. 이것을 다음 절로 보면 절이 잘린다.
			name: "옮기는 TTL",
			sql: "CREATE TABLE appdb.t (`at` DateTime) ENGINE = MergeTree ORDER BY (at) " +
				"TTL at + toIntervalDay(7) TO VOLUME 'cold', at + toIntervalDay(30) DELETE " +
				"SETTINGS index_granularity = 8192",
			want: "at + toIntervalDay(7) TO VOLUME 'cold', at + toIntervalDay(30) DELETE",
		},
		{
			// 주석 안의 낱말이 절을 끊으면 안 된다.
			name: "주석에 SETTINGS 가 들어 있는 표",
			sql: "CREATE TABLE appdb.t (`at` DateTime) ENGINE = MergeTree ORDER BY (at) " +
				"TTL at + toIntervalDay(7) COMMENT 'SETTINGS 를 조심하세요'",
			want: "at + toIntervalDay(7)",
		},
		{
			// SHOW CREATE 모양(여러 줄)으로 들어와도 같은 답이어야 한다.
			name: "여러 줄로 온 문장",
			sql: "CREATE TABLE appdb.page_views\n(\n    `viewed_at` DateTime64(3)\n)\n" +
				"ENGINE = MergeTree\nORDER BY (viewed_at)\n" +
				"TTL toDateTime(viewed_at) + toIntervalDay(90)\n" +
				"SETTINGS index_granularity = 8192",
			want: "toDateTime(viewed_at) + toIntervalDay(90)",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := clickhouseTableTTL(c.sql); got != c.want {
				t.Errorf("TTL 을 %q 로 읽었습니다. 기대: %q", got, c.want)
			}
		})
	}
}
