-- P4: 지표 시계열, 롤업, 임계치 룰, 이벤트, 스키마 스냅샷

-- 원본 샘플. 폴링 간격(기본 30초)마다 지표별로 한 행씩 쌓인다.
-- 보존기간이 짧으므로(기본 48시간) 세밀한 조사에만 쓰고,
-- 장기 조회는 metric_hourly를 본다.
CREATE TABLE metric_samples (
    connection_id TEXT NOT NULL REFERENCES connections (id) ON DELETE CASCADE,
    metric        TEXT NOT NULL,
    ts            TEXT NOT NULL,
    value         REAL NOT NULL,
    unit          TEXT NOT NULL DEFAULT 'count'
);

-- 조회는 항상 (커넥션, 지표, 시간범위)로 들어오므로 이 순서의 복합 인덱스가 필요하다.
CREATE INDEX idx_metric_samples_lookup ON metric_samples (connection_id, metric, ts);
-- 보존기간 정리는 ts만으로 스캔한다.
CREATE INDEX idx_metric_samples_ts ON metric_samples (ts);

-- 시간 단위 롤업. 완결된 시간대만 집계해 넣는다.
CREATE TABLE metric_hourly (
    connection_id TEXT NOT NULL REFERENCES connections (id) ON DELETE CASCADE,
    metric        TEXT NOT NULL,
    bucket        TEXT NOT NULL,           -- 정시로 절삭된 시각
    unit          TEXT NOT NULL DEFAULT 'count',
    samples       INTEGER NOT NULL,
    avg_value     REAL NOT NULL,
    min_value     REAL NOT NULL,
    max_value     REAL NOT NULL,
    PRIMARY KEY (connection_id, metric, bucket)
);

CREATE INDEX idx_metric_hourly_bucket ON metric_hourly (bucket);

-- 커넥션의 최신 상태. 매 폴링마다 덮어써서 목록 화면이 시계열을 훑지 않게 한다.
CREATE TABLE connection_state (
    connection_id   TEXT PRIMARY KEY REFERENCES connections (id) ON DELETE CASCADE,
    up              INTEGER NOT NULL DEFAULT 0,
    last_polled_at  TEXT,
    last_ok_at      TEXT,
    last_error      TEXT NOT NULL DEFAULT '',
    latency_ms      REAL NOT NULL DEFAULT 0,
    notes           TEXT NOT NULL DEFAULT '[]',   -- JSON 배열: 부분 수집 실패 사유
    metrics_json    TEXT NOT NULL DEFAULT '{}',   -- JSON: 최신 지표 스냅샷
    consecutive_fails INTEGER NOT NULL DEFAULT 0
);

-- 임계치 룰. connection_id가 NULL이면 모든 커넥션에 적용된다.
CREATE TABLE monitor_rules (
    id            TEXT PRIMARY KEY,
    name          TEXT NOT NULL,
    connection_id TEXT REFERENCES connections (id) ON DELETE CASCADE,
    -- environment가 지정되면 해당 환경의 커넥션에만 적용한다 (운영만 엄격하게 등).
    environment   TEXT CHECK (environment IN ('dev', 'prod')),
    kind          TEXT NOT NULL DEFAULT 'threshold' CHECK (kind IN ('threshold', 'connectivity', 'drift')),
    metric        TEXT NOT NULL DEFAULT '',
    -- 빈 문자열을 허용한다: connectivity/drift 룰은 비교 연산자가 없다
    -- (감지 로직이 고정되어 있어 지표와 임계치를 쓰지 않는다).
    op            TEXT NOT NULL DEFAULT '>' CHECK (op IN ('', '>', '>=', '<', '<=', '==', '!=')),
    threshold     REAL NOT NULL DEFAULT 0,
    -- duration_sec 동안 연속으로 조건을 만족해야 이벤트를 만든다.
    -- 순간적인 스파이크로 알림이 쏟아지는 것을 막기 위한 장치다.
    duration_sec  INTEGER NOT NULL DEFAULT 0,
    severity      TEXT NOT NULL DEFAULT 'warning' CHECK (severity IN ('info', 'warning', 'critical')),
    enabled       INTEGER NOT NULL DEFAULT 1,
    description   TEXT NOT NULL DEFAULT '',
    -- builtin 룰은 부트스트랩이 만든 기본 룰이다. 사용자가 수정/삭제할 수 있다.
    builtin       INTEGER NOT NULL DEFAULT 0,
    created_at    TEXT NOT NULL,
    updated_at    TEXT NOT NULL
);

CREATE INDEX idx_monitor_rules_conn ON monitor_rules (connection_id);
CREATE INDEX idx_monitor_rules_metric ON monitor_rules (metric);

-- 이벤트. 룰 위반, 연결 실패, 스키마 드리프트가 모두 여기로 모인다.
CREATE TABLE events (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    connection_id TEXT REFERENCES connections (id) ON DELETE CASCADE,
    rule_id       TEXT REFERENCES monitor_rules (id) ON DELETE SET NULL,
    kind          TEXT NOT NULL,        -- threshold | connectivity | drift | collect_error
    severity      TEXT NOT NULL CHECK (severity IN ('info', 'warning', 'critical')),
    state         TEXT NOT NULL DEFAULT 'open' CHECK (state IN ('open', 'resolved')),
    metric        TEXT NOT NULL DEFAULT '',
    message       TEXT NOT NULL,
    value         REAL,
    threshold     REAL,
    detail        TEXT NOT NULL DEFAULT '{}',   -- JSON
    started_at    TEXT NOT NULL,
    updated_at    TEXT NOT NULL,
    resolved_at   TEXT,
    acked_at      TEXT,
    acked_by      TEXT REFERENCES users (id) ON DELETE SET NULL,
    -- 같은 원인의 반복 발생 횟수. 새 이벤트를 만들지 않고 이 값을 올린다.
    occurrences   INTEGER NOT NULL DEFAULT 1
);

CREATE INDEX idx_events_conn_started ON events (connection_id, started_at DESC);
CREATE INDEX idx_events_state ON events (state, severity, started_at DESC);
-- 열린 이벤트를 원인별로 찾는 인덱스. 중복 생성을 막는 조회에 쓴다.
CREATE INDEX idx_events_open_key ON events (connection_id, kind, metric, state);

-- 스키마 스냅샷. 외부 편집(드리프트) 감지와 P7 버전 등록의 기반이다.
CREATE TABLE schema_snapshots (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    connection_id TEXT NOT NULL REFERENCES connections (id) ON DELETE CASCADE,
    fingerprint   TEXT NOT NULL,
    captured_at   TEXT NOT NULL,
    schema_json   TEXT NOT NULL,
    -- source: monitor(폴러가 자동 수집) | manual(사용자 요청) | migration(마이그레이션 후)
    source        TEXT NOT NULL DEFAULT 'monitor',
    -- 이전 스냅샷과의 변경 요약. 드리프트 이벤트에 표시한다.
    change_summary TEXT NOT NULL DEFAULT '[]'
);

CREATE INDEX idx_schema_snapshots_conn ON schema_snapshots (connection_id, captured_at DESC);
