-- P21: DB Studio가 도는 컴퓨터 자신의 지표.
--
-- metric_samples를 재사용하지 않는 이유: 그 표의 connection_id는 NOT NULL이고
-- connections를 참조한다. 호스트는 커넥션이 아니므로 가짜 행을 만들어 끼워 넣어야
-- 하는데, 그러면 커넥션 목록·권한 판정·삭제 CASCADE가 전부 그 가짜를 신경 써야 한다.
-- 표를 따로 두는 편이 각 표의 뜻이 하나로 유지된다.

CREATE TABLE host_samples (
    metric TEXT NOT NULL,
    ts     TEXT NOT NULL,
    value  REAL NOT NULL,
    unit   TEXT NOT NULL DEFAULT 'count'
);

CREATE INDEX idx_host_samples_lookup ON host_samples (metric, ts);
CREATE INDEX idx_host_samples_ts ON host_samples (ts);

-- 최신 상태 한 줄. 목록·대시보드가 시계열을 훑지 않도록 스냅샷을 통째로 담는다.
CREATE TABLE host_state (
    id           INTEGER PRIMARY KEY CHECK (id = 1),
    at           TEXT NOT NULL,
    snapshot     TEXT NOT NULL DEFAULT '{}',   -- JSON: hostmon.Snapshot
    -- boot_at은 재부팅 감지에 쓴다. 값이 바뀌면 그 사이에 서버가 다시 켜진 것이다.
    boot_at      TEXT NOT NULL DEFAULT '',
    -- os_log_offset은 시스템 로그 파일을 어디까지 읽었는지다.
    -- 파일이 잘리면(로테이션) 크기가 이 값보다 작아지므로 그때 처음부터 다시 읽는다.
    os_log_path   TEXT NOT NULL DEFAULT '',
    os_log_offset INTEGER NOT NULL DEFAULT 0
);
