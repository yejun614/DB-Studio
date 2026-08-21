-- P12: 논리 덤프(백업)와 복구.
--
-- 덤프 **내용**은 파일에 두고 여기에는 그 파일에 대한 기록만 둔다. 덤프는 GB 단위가
-- 될 수 있고, 그것을 메타 DB에 넣으면 백업 한 번이 앱 전체를 느리게 만든다.
-- (프로필 이미지를 DB에 넣은 것과 반대의 판단인데, 근거는 같다 — 크기.)
--
-- connection_id에 ON DELETE SET NULL을 걸고 이름을 따로 저장한다. 커넥션을 지웠다고
-- 백업 기록이 사라지면, 정작 그 백업이 필요한 상황(커넥션을 잘못 지웠다)에서
-- 무엇이 남아 있는지 알 수 없다.
CREATE TABLE backups (
    id              TEXT PRIMARY KEY,
    connection_id   TEXT REFERENCES connections (id) ON DELETE SET NULL,
    connection_name TEXT NOT NULL DEFAULT '',
    connection_kind TEXT NOT NULL DEFAULT '',
    -- scope: full(스키마+데이터) | schema | data
    scope           TEXT NOT NULL DEFAULT 'full' CHECK (scope IN ('full', 'schema', 'data')),
    -- format: sql | jsonl | redis — 복구기가 파일을 어떻게 읽을지 정한다
    format          TEXT NOT NULL DEFAULT 'sql',
    status          TEXT NOT NULL DEFAULT 'running'
                    CHECK (status IN ('running', 'success', 'failed', 'canceled')),
    -- path는 파일 이름만 담는다. 절대 경로를 저장하면 데이터 디렉터리를 옮겼을 때
    -- 모든 행이 한꺼번에 죽는다.
    file_name       TEXT NOT NULL DEFAULT '',
    size_bytes      INTEGER NOT NULL DEFAULT 0,
    table_count     INTEGER NOT NULL DEFAULT 0,
    row_count       INTEGER NOT NULL DEFAULT 0,
    statement_count INTEGER NOT NULL DEFAULT 0,
    -- options는 덤프를 만들 때 고른 것들이다(DROP 포함 여부, 대상 목록).
    -- 복구 화면이 "이 백업이 무엇을 담고 있는가"를 설명하는 데 쓴다.
    options         TEXT NOT NULL DEFAULT '{}',
    note            TEXT NOT NULL DEFAULT '',
    error           TEXT NOT NULL DEFAULT '',
    -- progress는 진행 중 표시용 한 줄이다(예: "public.orders 32,000행").
    progress        TEXT NOT NULL DEFAULT '',
    actor_id        TEXT REFERENCES users (id) ON DELETE SET NULL,
    actor_name      TEXT NOT NULL DEFAULT '',
    trigger         TEXT NOT NULL DEFAULT 'manual',  -- manual | macro
    started_at      TEXT NOT NULL,
    finished_at     TEXT,
    duration_ms     INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX idx_backups_conn ON backups (connection_id, started_at DESC);
CREATE INDEX idx_backups_started ON backups (started_at DESC);

-- 복구 기록.
--
-- 백업 하나를 여러 번, 여러 대상에 복구할 수 있으므로 별도 테이블이다.
-- backup_id에 ON DELETE SET NULL을 거는 이유도 위와 같다 — 복구했다는 사실은
-- 백업 파일을 지운 뒤에도 남아야 한다.
CREATE TABLE backup_restores (
    id               TEXT PRIMARY KEY,
    backup_id        TEXT REFERENCES backups (id) ON DELETE SET NULL,
    backup_label     TEXT NOT NULL DEFAULT '',
    connection_id    TEXT REFERENCES connections (id) ON DELETE SET NULL,
    connection_name  TEXT NOT NULL DEFAULT '',
    status           TEXT NOT NULL DEFAULT 'running'
                     CHECK (status IN ('running', 'success', 'failed', 'canceled')),
    statements_total INTEGER NOT NULL DEFAULT 0,
    statements_done  INTEGER NOT NULL DEFAULT 0,
    -- failed_statement는 멈춘 지점의 문장이다. 부분 적용된 복구를 사람이 정리하려면
    -- "어디까지 갔는가"가 유일한 출발점이다(마이그레이션 실행 기록과 같은 이유).
    failed_statement TEXT NOT NULL DEFAULT '',
    error            TEXT NOT NULL DEFAULT '',
    progress         TEXT NOT NULL DEFAULT '',
    actor_id         TEXT REFERENCES users (id) ON DELETE SET NULL,
    actor_name       TEXT NOT NULL DEFAULT '',
    started_at       TEXT NOT NULL,
    finished_at      TEXT,
    duration_ms      INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX idx_restores_started ON backup_restores (started_at DESC);
CREATE INDEX idx_restores_conn ON backup_restores (connection_id, started_at DESC);
