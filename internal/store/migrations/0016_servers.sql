-- P15: 서버 개념 도입 — 커넥션 하나가 아니라 "서버 하나에 DB 여러 개".
--
-- 그전에는 커넥션 하나가 접속 정보(host·port·자격증명)와 대상 DB를 함께 들고 있었다.
-- 그래서 같은 서버의 DB 세 개를 관리하려면 같은 자격증명을 세 번 등록해야 했고,
-- 비밀번호를 바꾸면 세 곳을 고쳐야 했다.
--
-- **커넥션을 쪼개지 않고 서버를 위로 뽑았다.** connection_id는 이 앱에서 18개 테이블의
-- 기준 단위다(권한·지표·이벤트·드리프트·ERD 문서·스키마 버전·마이그레이션·백업·트리거).
-- 커넥션 하나가 DB 여러 개를 품는 쪽으로 갔다면 그 18개 전부에 database 차원을 더해야 하고,
-- 커넥션 단위인 권한이 "한 DB를 허용하면 나머지도 전부 열린다"로 조용히 넓어진다.
-- 서버를 위에 두면 다운스트림은 그대로고, 권한은 DB 단위 정밀도를 유지한 채
-- 서버 단위 일괄 부여만 얹으면 된다(0017 참고).

CREATE TABLE servers (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL,
    name_lower  TEXT NOT NULL UNIQUE,
    kind        TEXT NOT NULL,             -- mysql | postgres | mssql | oracle | sqlite | mongodb | redis
    host        TEXT NOT NULL DEFAULT '',
    port        INTEGER NOT NULL DEFAULT 0,
    -- 접속 옵션(sslmode, service_name, tls, auth_source 등)은 서버 속성이다.
    -- 같은 서버의 DB마다 TLS 설정이 다를 수는 없다.
    options     TEXT NOT NULL DEFAULT '{}',
    -- default_environment는 이 서버에 DB를 추가할 때의 기본값일 뿐이다.
    --
    -- 환경을 커넥션(DB)에 남긴 이유: 이 값은 "이 서버가 무엇인가"가 아니라 "이 대상을
    -- 얼마나 조심히 다룰 것인가"를 뜻한다(운영이면 승인 수와 확인 문구가 붙는다).
    -- 한 서버에 실운영 DB와 임시 DB가 함께 사는 일은 흔하고, 그때 둘을 같게 취급하면
    -- 안전장치가 필요 없는 쪽에 붙거나 필요한 쪽에서 빠진다.
    default_environment TEXT NOT NULL DEFAULT 'dev' CHECK (default_environment IN ('dev', 'prod')),
    tags        TEXT NOT NULL DEFAULT '',
    note        TEXT NOT NULL DEFAULT '',
    -- enabled가 꺼지면 소속 DB 전부가 꺼진다. 서버 점검처럼 한 번에 멈춰야 하는 경우가 있다.
    enabled     INTEGER NOT NULL DEFAULT 1,
    created_by  TEXT REFERENCES users (id),
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL
);

CREATE INDEX idx_servers_kind ON servers (kind);

-- 자격증명은 서버에 하나뿐이다. 이것이 이 변경의 요점이다 —
-- 한 번 고치면 그 서버의 모든 DB에 반영된다.
CREATE TABLE server_secrets (
    server_id    TEXT PRIMARY KEY REFERENCES servers (id) ON DELETE CASCADE,
    username     TEXT NOT NULL DEFAULT '',
    password_enc TEXT NOT NULL DEFAULT '',
    extra_enc    TEXT NOT NULL DEFAULT '',
    updated_at   TEXT NOT NULL
);

-- ---- 기존 데이터 이관 ----
--
-- **커넥션들을 자동으로 묶지 않는다.** 커넥션 하나당 서버 하나를 만든다.
-- (kind, host, port)가 같으면 같은 서버처럼 보이지만 자격증명이 다를 수 있고,
-- 비밀번호는 매번 다른 nonce로 봉인되어 있어 SQL로는 같은지 비교할 수조차 없다.
-- 잘못 묶으면 한쪽 자격증명이 조용히 사라지고, 그 사실은 다음 접속 실패로만 드러난다.
-- 합치는 것은 값을 확인할 수 있는 사람이 화면에서 명시적으로 한다(서버 병합 기능).
INSERT INTO servers (id, name, name_lower, kind, host, port, options,
                     default_environment, tags, note, enabled,
                     created_by, created_at, updated_at)
SELECT 'srv_' || c.id, c.name, c.name_lower, c.kind, c.host, c.port, c.options,
       c.environment, c.tags, '', c.enabled,
       c.created_by, c.created_at, c.updated_at
FROM connections c;

INSERT INTO server_secrets (server_id, username, password_enc, extra_enc, updated_at)
SELECT 'srv_' || s.connection_id, s.username, s.password_enc, s.extra_enc, s.updated_at
FROM connection_secrets s;

ALTER TABLE connections ADD COLUMN server_id TEXT REFERENCES servers (id) ON DELETE CASCADE;
UPDATE connections SET server_id = 'srv_' || id;

-- 서버로 옮긴 컬럼을 지운다. SQLite는 인덱스가 걸린 컬럼을 지우지 못하므로 인덱스를 먼저 지운다.
-- environment는 위에 적은 이유로 커넥션에 남으므로 idx_connections_env도 남는다.
DROP INDEX idx_connections_kind;
ALTER TABLE connections DROP COLUMN kind;
ALTER TABLE connections DROP COLUMN host;
ALTER TABLE connections DROP COLUMN port;
ALTER TABLE connections DROP COLUMN options;

DROP TABLE connection_secrets;

CREATE INDEX idx_connections_server ON connections (server_id);

-- 한 서버에 같은 DB를 두 번 등록할 수는 없다.
-- 지금까지는 커넥션이 곧 대상이라 이런 중복이 "다른 이름의 같은 것"으로 조용히 생겼고,
-- 지표와 이벤트가 두 벌씩 쌓였다.
CREATE UNIQUE INDEX idx_connections_server_db ON connections (server_id, database_name);
