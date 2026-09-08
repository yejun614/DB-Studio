-- ClickHouse ↔ Kafka 파이프라인. 컨테이너가 처음 뜰 때 한 번 실행된다.
--
-- 왜 세 덩어리인가: ClickHouse 에서 Kafka 를 읽는 방법은 표 하나로 끝나지 않는다.
--   1) Kafka 엔진 표(events_queue)는 **큐 그 자체**다. SELECT 하면 메시지를 소비해
--      버리므로 사람이 직접 조회할 것이 못 된다.
--   2) MergeTree 표(events)는 실제로 저장되는 곳이다.
--   3) 구체화 뷰(events_mv)가 1에서 2로 옮긴다. 이것이 없으면 큐는 돌지만
--      아무것도 남지 않는다.
--
-- 이 앱에서 볼 것: 스키마 화면에 세 가지가 각각 다른 엔진으로 나오고, 데이터
-- 화면에서 events 의 행이 늘어나며, 브로커 화면에서 같은 이름의 컨슈머 그룹이
-- 보인다. 즉 한 흐름을 두 화면에서 확인할 수 있다.

CREATE DATABASE IF NOT EXISTS appdb;

-- 1) 큐. 브로커 주소는 **컨테이너 이름**이다.
--
-- localhost:19092 가 아닌 이유: 그 주소는 호스트에서 붙을 때의 것이다. 이 표는
-- ClickHouse 컨테이너 안에서 붙으므로 localhost 는 자기 자신을 가리킨다.
CREATE TABLE IF NOT EXISTS appdb.events_queue
(
    `id` UInt64,
    `kind` String,
    `at` DateTime64(3),
    `payload` String
)
ENGINE = Kafka
SETTINGS
    kafka_broker_list = 'kafka:9092',
    kafka_topic_list = 'events',
    kafka_group_name = 'clickhouse-events',
    kafka_format = 'JSONEachRow',
    -- 형식이 어긋난 메시지 하나가 소비 전체를 멈추지 않게 한다. 시험용 큐에는
    -- 손으로 넣은 메시지가 섞이기 마련이다.
    kafka_skip_broken_messages = 100,
    kafka_num_consumers = 1;

-- 2) 실제로 남는 곳.
CREATE TABLE IF NOT EXISTS appdb.events
(
    `id` UInt64,
    `kind` LowCardinality(String),
    `at` DateTime64(3),
    `payload` String,
    -- 언제 들어왔는지. 메시지의 시각(at)과 다를 수 있고, 그 차이가 곧 지연이다.
    `ingested_at` DateTime64(3) DEFAULT now64(3)
)
ENGINE = MergeTree
PARTITION BY toYYYYMM(at)
ORDER BY (kind, at, id)
COMMENT '카프카에서 받아 쌓는 이벤트';

-- 3) 큐에서 표로 옮기는 다리.
CREATE MATERIALIZED VIEW IF NOT EXISTS appdb.events_mv TO appdb.events AS
SELECT id, kind, at, payload
FROM appdb.events_queue;

-- 카프카와 무관한 표도 하나 둔다.
--
-- 브로커를 안 띄웠거나 큐가 비어 있어도 스키마·데이터 화면에 볼 것이 있어야 한다 —
-- 빈 화면은 "이 DB 를 못 읽었다"와 구분되지 않는다.
CREATE TABLE IF NOT EXISTS appdb.page_views
(
    `viewed_at` DateTime64(3),
    `path` LowCardinality(String),
    `member_id` Nullable(UInt64),
    `ms` UInt32
)
ENGINE = MergeTree
PARTITION BY toYYYYMM(viewed_at)
ORDER BY (path, viewed_at)
TTL toDateTime(viewed_at) + INTERVAL 90 DAY
COMMENT '페이지 조회 로그 (보관 90일)';

INSERT INTO appdb.page_views (viewed_at, path, member_id, ms)
SELECT
    now64(3) - toIntervalSecond(number * 37),
    ['/', '/login', '/erd', '/schema', '/vector'][(number % 5) + 1],
    if(number % 7 = 0, NULL, number % 120),
    20 + (number % 400)
FROM numbers(500);
