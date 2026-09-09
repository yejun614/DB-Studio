-- DB Studio 가 도커로 만든 DB 인스턴스.
--
-- ── 왜 우리가 따로 적어 두는가 ──────────────────────────────────────
-- 도커가 이미 컨테이너를 알고 있으므로, 목록은 라벨로 물어보면 된다. 그런데도
-- 우리 표를 두는 이유가 셋 있다.
--
--   1. **설정값**. 어떤 필드에 무엇을 넣어 만들었는지는 컨테이너에 남지 않는다.
--      환경변수는 남지만 그것은 결과이고, "사용자가 메모리 상한을 512 로
--      골랐다"는 우리만 안다. 다시 만들거나 compose.yml 로 내보낼 때 그 값이
--      있어야 한다.
--   2. **비밀번호**. 컨테이너의 환경변수에 평문으로 있지만, 그것을 읽으려면
--      도커에 물어야 하고 컨테이너를 지우면 사라진다. 커넥션으로 등록할 때와
--      다시 만들 때 필요하므로 봉해서 우리가 들고 있는다(server_secrets 와
--      같은 방식).
--   3. **사라진 것도 기록으로 남는다**. 컨테이너를 지우면 도커에서는 없어지지만
--      "누가 언제 무엇을 만들었다가 지웠다"는 남아야 한다.
--
-- ── 프로젝트에 매어 둔다 ────────────────────────────────────────────
-- 서버 컴퓨터·매크로와 달리 프로젝트 안에 둔다. 만든 DB 는 곧 커넥션이 되고,
-- 커넥션은 프로젝트 없이 존재할 수 없다(store.ErrNoProject). 인스턴스를
-- 프로젝트 밖에 두면 "어느 프로젝트의 커넥션으로 등록하는가"를 만들 때마다
-- 다시 물어야 하고, 권한 판정도 그 순간에만 있게 된다.

CREATE TABLE db_instances (
    id          TEXT PRIMARY KEY,
    project_id  TEXT NOT NULL REFERENCES projects (id) ON DELETE CASCADE,

    -- name 은 사람이 정한 이름이다. 컨테이너 이름과 볼륨 이름의 바탕이 되므로
    -- 소문자·숫자·붙임표만 받는다(provision.validName).
    name        TEXT NOT NULL,
    -- kind 는 이 앱이 아는 DB 종류다(postgres, mysql, …).
    kind        TEXT NOT NULL,
    image       TEXT NOT NULL,
    version     TEXT NOT NULL,

    -- container_id 는 도커가 준 id 다. 컨테이너를 지우면 비운다.
    container_id TEXT NOT NULL DEFAULT '',
    -- container_name·volume_name 은 우리가 정한 이름이다. 컨테이너가 없어도
    -- 남겨 둔다 — 다시 만들 때 같은 볼륨에 붙어야 데이터가 이어진다.
    container_name TEXT NOT NULL,
    volume_name    TEXT NOT NULL DEFAULT '',

    -- host_port 는 **도커가 실제로 잡은** 포트다. 0 은 아직 모른다는 뜻이다.
    --
    -- 요청한 값이 아니라 결과를 담는다. 사람이 0 을 주면 도커가 골라 주는데,
    -- 요청 쪽에는 그대로 0 이 남는다 — 그 값으로 커넥션을 등록하면 붙을 수
    -- 없는 커넥션이 만들어진다.
    host_port   INTEGER NOT NULL DEFAULT 0,
    -- port 는 컨테이너 안에서 DB 가 듣는 포트다.
    port        INTEGER NOT NULL,

    -- values 는 사람이 고른 설정이다({"database":"appdb","memoryMB":"512"}).
    -- 비밀 필드는 여기 담지 않는다(아래 db_instance_secrets 로 간다).
    values_json TEXT NOT NULL DEFAULT '{}',

    -- status 는 우리가 마지막으로 본 상태다.
    --
    -- 도커가 진짜를 들고 있으므로 이 값은 **캐시**다. 목록을 그릴 때마다
    -- 컨테이너 수만큼 도커에 물으면 느려지고, 도커가 꺼져 있을 때 목록이
    -- 통째로 비게 된다. 대신 열 때 새로 고친다.
    status      TEXT NOT NULL DEFAULT 'creating'
        CHECK (status IN ('creating', 'running', 'stopped', 'unhealthy', 'failed', 'removed')),
    -- health 는 컨테이너의 헬스체크 결과다(starting|healthy|unhealthy, 없으면 '').
    health      TEXT NOT NULL DEFAULT '',
    -- error 는 마지막 실패 이유다. 성공하면 비운다.
    error       TEXT NOT NULL DEFAULT '',
    -- progress 는 만드는 중 보여줄 한 줄이다("이미지 내려받는 중 42%").
    progress    TEXT NOT NULL DEFAULT '',

    -- connection_id 는 이 인스턴스로 등록한 커넥션이다.
    --
    -- ON DELETE SET NULL 인 이유: 커넥션을 지워도 컨테이너는 남는다. 그 둘은
    -- 다른 물건이고, 커넥션을 지운 것이 DB 를 지운 것이 되어서는 안 된다.
    connection_id TEXT REFERENCES connections (id) ON DELETE SET NULL,

    created_by  TEXT REFERENCES users (id) ON DELETE SET NULL,
    -- created_by_name 은 사람이 지워져도 남는 이름이다.
    created_by_name TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL
);

-- 컨테이너 이름은 도커에서 유일해야 하므로 우리 쪽에서도 유일하게 둔다.
-- 그러지 않으면 만들다가 도커의 409 로 실패하고, 그 오류는 우리 표에
-- 반쯤 만들어진 줄을 남긴다.
CREATE UNIQUE INDEX idx_db_instances_container ON db_instances (container_name);

-- 목록은 프로젝트별로 이름 순으로 읽는다.
CREATE INDEX idx_db_instances_project ON db_instances (project_id, name);
-- 커넥션 화면에서 "이 커넥션은 우리가 만든 것인가"를 되짚는다.
CREATE INDEX idx_db_instances_conn ON db_instances (connection_id);

-- 비밀번호는 따로 봉해 둔다.
--
-- 표를 나누는 이유는 server_secrets 와 같다. 인스턴스 목록을 읽는 질의가
-- 비밀을 함께 읽지 않아야 한다 — 같은 표에 두면 목록 한 번에 모든 비밀번호가
-- 메모리로 올라오고, 실수로 그것을 그대로 JSON 으로 내보내는 길이 생긴다.
CREATE TABLE db_instance_secrets (
    instance_id TEXT PRIMARY KEY REFERENCES db_instances (id) ON DELETE CASCADE,
    -- secrets_enc 는 {"password":"..."} 를 봉한 것이다.
    -- TEXT 인 이유: crypto.SecretBox.Seal 이 문자열을 돌려준다(server_secrets 와 같다).
    secrets_enc TEXT NOT NULL DEFAULT '',
    updated_at  TEXT NOT NULL
);
