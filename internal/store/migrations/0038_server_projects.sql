-- +no-foreign-keys
--
-- P38: 서버도 프로젝트 안으로.
--
-- 0037에서 서버는 프로젝트 밖에 두었다. 한 대의 물리 서버가 여러 팀의 DB를 담는
-- 경우를 생각했기 때문이다. 그러나 실제로는 프로젝트마다 서버를 **따로 등록한다** —
-- 접속 정보와 자격증명이 서버에 붙어 있어서, 같은 호스트라도 팀이 다르면 계정이
-- 다르고 그래서 등록도 따로 하게 된다.
--
-- 그 결과 프로젝트를 바꿔도 서버 목록은 그대로여서, DB 커넥션 화면에는 남의 팀
-- 서버가 호스트 이름까지 그대로 떠 있었다. 프로젝트를 나눈 이유가 그 화면에서
-- 무너진다.
--
-- 이제 계층은 셋이다: 프로젝트 → 서버 → DB.

-- 옮길 곳이 없는데 서버가 있으면 기본 프로젝트를 만든다.
--
-- 0037은 커넥션·ERD·용어가 하나라도 있을 때만 만들었다. 서버만 등록하고 DB는 아직
-- 하나도 만들지 않은 앱이 그 조건에서 빠진다 — 그런 앱에서 이 마이그레이션이 서버를
-- 갈 곳 없이 남기면 아무도 볼 수 없는 서버가 된다.
INSERT INTO projects (id, name, name_lower, note, created_by, created_at, updated_at)
SELECT 'default', '기본 프로젝트', '기본 프로젝트',
    '프로젝트가 생기기 전부터 있던 자원이 모두 여기에 들어왔습니다.',
    NULL, strftime('%Y-%m-%dT%H:%M:%SZ', 'now'), strftime('%Y-%m-%dT%H:%M:%SZ', 'now')
WHERE EXISTS (SELECT 1 FROM servers)
  AND NOT EXISTS (SELECT 1 FROM projects WHERE id = 'default');

INSERT OR IGNORE INTO project_members (project_id, user_id, added_at)
SELECT 'default', id, strftime('%Y-%m-%dT%H:%M:%SZ', 'now') FROM users
WHERE EXISTS (SELECT 1 FROM projects WHERE id = 'default');

-- ---------- 서버 ----------
--
-- 표를 새로 만들어 옮기는 이유는 0037의 커넥션과 같다: 이름의 유일성을 프로젝트
-- 안으로 좁혀야 한다. 앱 전체에서 유일하면, 다른 팀이 이미 "운영 MySQL"을 쓰고
-- 있을 때 보이지도 않는 이름 때문에 등록이 막힌다.
CREATE TABLE servers_new (
    id          TEXT PRIMARY KEY,
    -- 프로젝트는 반드시 있다. 어디에도 속하지 않은 서버는 목록에 뜨지 않으므로
    -- 만들 수는 있는데 아무도 볼 수 없는 유령이 된다.
    project_id  TEXT NOT NULL REFERENCES projects (id) ON DELETE RESTRICT,
    name        TEXT NOT NULL,
    name_lower  TEXT NOT NULL,
    kind        TEXT NOT NULL,
    host        TEXT NOT NULL DEFAULT '',
    port        INTEGER NOT NULL DEFAULT 0,
    options     TEXT NOT NULL DEFAULT '{}',
    default_environment TEXT NOT NULL DEFAULT 'dev' CHECK (default_environment IN ('dev', 'prod')),
    tags        TEXT NOT NULL DEFAULT '',
    note        TEXT NOT NULL DEFAULT '',
    enabled     INTEGER NOT NULL DEFAULT 1,
    created_by  TEXT REFERENCES users (id),
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL,
    UNIQUE (project_id, name_lower)
);

-- 소속은 그 아래 DB에서 읽는다. DB가 하나도 없는 서버는 기본 프로젝트로 간다.
--
-- 한 서버 아래 DB가 여러 프로젝트에 흩어져 있을 수는 없다(0037 직후에는 전부
-- 기본 프로젝트였고, 그 뒤에 만든 것은 만들 때 프로젝트를 골랐다). 그래도 MIN을
-- 쓰는 이유는, 어긋난 데이터가 있더라도 마이그레이션이 멈추지 않고 한 곳으로
-- 모으게 하기 위해서다 — 아래에서 커넥션을 서버 쪽으로 다시 맞춘다.
INSERT INTO servers_new
    (id, project_id, name, name_lower, kind, host, port, options,
     default_environment, tags, note, enabled, created_by, created_at, updated_at)
SELECT s.id,
    COALESCE((SELECT MIN(c.project_id) FROM connections c WHERE c.server_id = s.id), 'default'),
    s.name, s.name_lower, s.kind, s.host, s.port, s.options,
    s.default_environment, s.tags, s.note, s.enabled, s.created_by, s.created_at, s.updated_at
FROM servers s;

DROP TABLE servers;
ALTER TABLE servers_new RENAME TO servers;

CREATE INDEX idx_servers_kind ON servers (kind);
CREATE INDEX idx_servers_project ON servers (project_id, name_lower);

-- 커넥션의 프로젝트를 서버 쪽으로 맞춘다.
--
-- 이제 근거는 하나다: **DB의 프로젝트는 그 서버의 프로젝트다.** 두 값이 어긋날 수
-- 있는 상태를 남겨 두면 어느 쪽이 참인지 권한 판정이 답하지 못한다. 커넥션에서
-- 컬럼을 없애지 않는 이유는 판정과 목록이 그 값을 조인 없이 읽기 때문이고,
-- 대신 저장 계층이 쓸 때마다 서버 쪽 값을 넣는다(store/connections.go).
UPDATE connections
SET project_id = (SELECT s.project_id FROM servers s WHERE s.id = connections.server_id)
WHERE server_id IS NOT NULL
  AND project_id <> (SELECT s.project_id FROM servers s WHERE s.id = connections.server_id);
