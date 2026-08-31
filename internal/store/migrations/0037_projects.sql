-- +no-foreign-keys
--
-- P37: 프로젝트.
--
-- 지금까지 이 앱에는 층이 하나뿐이었다 — 커넥션이 뿌리고, ERD·마이그레이션·버전이
-- 그 아래 달렸다. 그 구조는 관리 대상이 한 팀의 것일 때만 맞는다. 두 팀이 같은
-- 앱을 쓰기 시작하면 목록 화면은 남의 DB로 채워지고, 권한은 커넥션 하나하나에
-- 손으로 채워 넣어야 한다.
--
-- 프로젝트는 그 위의 층이다. 커넥션과 독립 ERD 초안과 용어 사전이 프로젝트에
-- 속하고, 나머지(마이그레이션·버전·스냅샷·백업·구조 문서)는 커넥션을 따라 저절로
-- 딸려 간다. 새 컬럼을 스무 개 늘리지 않고 두 개만 늘리는 이유가 그것이다.
--
-- 서버 컴퓨터·매크로·클러스터·사용자·알림은 프로젝트 밖에 둔다. 한 대의 서버가
-- 여러 프로젝트의 DB를 담고, 매크로 하나가 두 프로젝트를 오갈 수 있다. 그런 것을
-- 프로젝트에 매면 "어느 프로젝트의 권한으로 도는가"라는 답 없는 물음이 생긴다.

CREATE TABLE projects (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL,
    name_lower TEXT NOT NULL UNIQUE,
    note       TEXT NOT NULL DEFAULT '',
    created_by TEXT REFERENCES users (id) ON DELETE SET NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

-- 참여자. 프로젝트를 볼 수 있는가를 정한다.
--
-- 기존의 커넥션별 등급·능력(user_db_access)을 대신하지 않는다. 그 위에 있는
-- **관문**이다: 프로젝트에 참여하지 않으면 그 안의 DB는 등급이 무엇이든 보이지
-- 않고, 참여하더라도 DB별 등급은 지금까지처럼 따로 정한다. 둘을 하나로 합치면
-- "프로젝트에는 있지만 운영 DB만 못 본다"를 적을 수 없게 된다.
CREATE TABLE project_members (
    project_id TEXT NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    user_id    TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    added_at   TEXT NOT NULL,
    PRIMARY KEY (project_id, user_id)
);

CREATE INDEX idx_project_members_user ON project_members (user_id);

-- 옮겨 갈 곳이 필요한 데이터가 있으면 기본 프로젝트를 만든다.
--
-- 빈 앱에는 만들지 않는다. 처음 켠 사람은 프로젝트를 손수 만들면서 이 층이
-- 있다는 것을 알게 되고, 이미 쓰던 앱은 하나도 잃지 않는다. 아이디를 'default'로
-- 고정한 이유: 이 줄은 "프로젝트가 생기기 전부터 있던 것들"이라는 뜻이고, 그것을
-- DB만 열어 봐도 알 수 있어야 한다.
INSERT INTO projects (id, name, name_lower, note, created_by, created_at, updated_at)
SELECT 'default', '기본 프로젝트', '기본 프로젝트',
    '프로젝트가 생기기 전부터 있던 자원이 모두 여기에 들어왔습니다.',
    NULL, strftime('%Y-%m-%dT%H:%M:%SZ', 'now'), strftime('%Y-%m-%dT%H:%M:%SZ', 'now')
WHERE EXISTS (SELECT 1 FROM connections)
   OR EXISTS (SELECT 1 FROM erd_documents)
   OR EXISTS (SELECT 1 FROM glossary_terms);

-- 있던 사람은 모두 참여자로 넣는다. 이것이 없으면 앱을 올리는 순간 슈퍼 어드민을
-- 뺀 전원이 아무것도 못 보게 된다 — 기능을 더하면서 남의 권한을 조용히 빼앗는
-- 셈이고, 그 사실은 "화면이 비어 있다"로만 드러난다.
INSERT INTO project_members (project_id, user_id, added_at)
SELECT 'default', id, strftime('%Y-%m-%dT%H:%M:%SZ', 'now') FROM users
WHERE EXISTS (SELECT 1 FROM projects WHERE id = 'default');

-- ---------- 커넥션 ----------
--
-- 테이블을 새로 만들어 옮긴다. 컬럼 하나를 더하는 것이면 ALTER TABLE로 충분하지만,
-- 이름의 유일성을 **프로젝트 안으로** 좁혀야 하기 때문이다. 지금은 name_lower가
-- 앱 전체에서 유일해서, 다른 팀이 이미 "운영 DB"를 쓰고 있으면 보이지도 않는
-- 이름 때문에 등록이 막힌다. 그 실패는 원인을 짚을 방법이 없다.
--
-- 이 파일이 -- +no-foreign-keys 로 시작하는 이유는 0021과 같다: 외래키 검사가 켜진
-- 채로 옛 connections를 DROP 하면 그 아래 달린 것이 전부 CASCADE로 함께 지워진다.
CREATE TABLE connections_new (
    id            TEXT PRIMARY KEY,
    -- 프로젝트는 반드시 있다. 어디에도 속하지 않은 커넥션은 목록에도 권한 판정에도
    -- 나타날 수 없어서, 만들 수는 있는데 아무도 볼 수 없는 유령이 된다.
    project_id    TEXT NOT NULL REFERENCES projects (id) ON DELETE RESTRICT,
    -- server_id는 지금 그대로 NULL을 허용한다. 0016에서 ALTER로 붙은 컬럼이라
    -- 옛 DB에 빈 값이 남아 있을 수 있고, 여기서 NOT NULL로 조이면 그런 DB는 앱이
    -- 아예 뜨지 않는다. 이 마이그레이션이 고치려는 문제와도 상관이 없다.
    server_id     TEXT REFERENCES servers (id) ON DELETE CASCADE,
    name          TEXT NOT NULL,
    name_lower    TEXT NOT NULL,
    environment   TEXT NOT NULL CHECK (environment IN ('dev', 'prod')),
    database_name TEXT NOT NULL DEFAULT '',
    node_id       TEXT NOT NULL DEFAULT '',
    tags          TEXT NOT NULL DEFAULT '',
    note          TEXT NOT NULL DEFAULT '',
    enabled       INTEGER NOT NULL DEFAULT 1,
    last_check_at TEXT,
    last_check_ok INTEGER,
    last_check_msg TEXT NOT NULL DEFAULT '',
    created_by    TEXT REFERENCES users (id),
    created_at    TEXT NOT NULL,
    updated_at    TEXT NOT NULL,
    UNIQUE (project_id, name_lower)
);

INSERT INTO connections_new
    (id, project_id, server_id, name, name_lower, environment, database_name, node_id,
     tags, note, enabled, last_check_at, last_check_ok, last_check_msg,
     created_by, created_at, updated_at)
SELECT id, 'default', server_id, name, name_lower, environment, database_name, node_id,
     tags, note, enabled, last_check_at, last_check_ok, last_check_msg,
     created_by, created_at, updated_at
FROM connections;

DROP TABLE connections;
ALTER TABLE connections_new RENAME TO connections;

CREATE INDEX idx_connections_env ON connections (environment);
CREATE INDEX idx_connections_server ON connections (server_id);
CREATE INDEX idx_connections_project ON connections (project_id, name_lower);
-- 한 서버에 같은 DB를 두 번 등록할 수는 없다(0016). 프로젝트가 달라도 마찬가지다 —
-- 같은 DB를 두 프로젝트가 각자 등록하면 지표와 이벤트가 두 벌씩 쌓인다.
CREATE UNIQUE INDEX idx_connections_server_db ON connections (server_id, database_name);

-- ---------- ERD 문서 ----------
--
-- 커넥션이 붙은 문서는 커넥션에서 프로젝트를 알 수 있지만, 독립 초안(connection_id
-- NULL)에는 그 근거가 없다. 설계는 DB를 만들기 전에 시작되는 일이 더 많아서 독립
-- 초안이야말로 프로젝트가 필요한 쪽이다.
--
-- 컬럼을 따로 두면 커넥션이 붙은 문서에서 근거가 둘이 된다. 그래서 저장 계층이
-- 커넥션을 붙일 때 두 값이 같은지 확인한다(store/erd.go).
ALTER TABLE erd_documents ADD COLUMN project_id TEXT NOT NULL DEFAULT '';
UPDATE erd_documents SET project_id = 'default';

CREATE INDEX idx_erd_documents_project ON erd_documents (project_id, updated_at DESC);

-- ---------- 용어 사전 ----------
--
-- 사전은 팀의 약속이고, 팀이 다르면 약속도 다르다. "주문"이 한쪽에서는 결제 전
-- 장바구니이고 다른 쪽에서는 배송 지시서인 일은 실제로 있다. 앱 하나에 사전이
-- 하나뿐이면 그 둘 중 하나는 사전에 적지 못한 채로 쓰게 된다.
ALTER TABLE glossary_terms ADD COLUMN project_id TEXT NOT NULL DEFAULT '';
UPDATE glossary_terms SET project_id = 'default';

-- 같은 말은 한 프로젝트 안에서만 한 번이다. 옛 유일 인덱스는 앱 전체를 걸고
-- 있었으므로 갈아 끼운다.
DROP INDEX IF EXISTS idx_glossary_term;
CREATE UNIQUE INDEX idx_glossary_term ON glossary_terms (project_id, term COLLATE NOCASE);
CREATE INDEX idx_glossary_project ON glossary_terms (project_id, cat1, cat2, cat3);
