-- ClickHouse 클러스터 DDL. 실제 쓰이는 모양을 그대로 담았다.
--
-- 여기 있는 것들이 하나라도 안 읽히면 스크립트가 통째로 안 읽힌다. 클러스터
-- DDL 은 거의 모든 문장에 ON CLUSTER 를 달고 있고, 표는 로컬/분산 둘씩 다닌다.
CREATE TABLE IF NOT EXISTS events_local ON CLUSTER c1
(
    event_id     UUID,
    event_type   LowCardinality(String),
    user_id      UInt64,
    occurred_at  DateTime64(3, 'Asia/Seoul'),
    ingested_at  DateTime64(3, 'Asia/Seoul') DEFAULT now64(3),
    ext          Map(String, String)
)
ENGINE = ReplicatedReplacingMergeTree('/clickhouse/tables/{shard}/events', '{replica}', ingested_at)
PARTITION BY toYYYYMM(occurred_at)
ORDER BY (user_id, occurred_at, event_id)
TTL toDateTime(occurred_at) + INTERVAL 18 MONTH;

-- 컬럼을 적지 않고 위 표의 것을 그대로 쓴다. 응용이 실제로 읽고 쓰는 쪽이다.
CREATE TABLE IF NOT EXISTS events ON CLUSTER c1
AS events_local
ENGINE = Distributed(c1, currentDatabase(), events_local, cityHash64(user_id));

CREATE OR REPLACE VIEW v_recent ON CLUSTER c1 AS
SELECT user_id, occurred_at FROM events_local WHERE event_type = 'PLAY';

-- 사전은 표도 뷰도 아니다. 건너뛰되 나머지는 계속 읽어야 한다.
CREATE DICTIONARY IF NOT EXISTS dict_users ON CLUSTER c1
(
    user_id UInt64,
    name    String
)
PRIMARY KEY user_id
SOURCE(CLICKHOUSE(TABLE 'v_recent'))
LAYOUT(FLAT())
LIFETIME(MIN 300 MAX 600);

CREATE TABLE IF NOT EXISTS signals_local ON CLUSTER c1
(
    user_id     UInt64,
    signal      LowCardinality(String),
    occurred_at DateTime64(3, 'Asia/Seoul'),
    weight      Float32
)
ENGINE = ReplicatedMergeTree('/clickhouse/tables/{shard}/signals', '{replica}')
ORDER BY (user_id, occurred_at);

CREATE MATERIALIZED VIEW IF NOT EXISTS mv_signals ON CLUSTER c1
TO signals_local AS
SELECT user_id, event_type AS signal, occurred_at, 1.0 AS weight
FROM events_local
WHERE event_type IN ('LIKE', 'DISLIKE');

CREATE TABLE IF NOT EXISTS queue ON CLUSTER c1
(
    event_id   String,
    event_type String
)
ENGINE = Kafka
SETTINGS
    kafka_broker_list = 'kafka1:9092,kafka2:9092',
    kafka_topic_list = 'events',
    kafka_group_name = 'ch_events',
    kafka_format = 'JSONEachRow',
    kafka_num_consumers = 3;
