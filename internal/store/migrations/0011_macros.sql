-- P11: 매크로 — 노드 그래프 + 버전 관리 + 실행 이력.
--
-- 세 가지 규칙이 이 스키마를 결정한다.
--
-- 1. **모든 매크로는 실행 전에 저장된다.** 그래서 실행 이력은 macro_id가 아니라
--    특정 버전을 가리킨다. "그때 무엇이 실행됐는가"를 나중에 정확히 재구성할 수
--    있어야 하고, 그러려면 실행 시점의 그래프가 남아 있어야 한다.
-- 2. **버전은 추가만 된다.** 롤백도 새 버전을 만든다(옛 그래프를 복사한다).
--    되돌린 것도 이력이며, 버전을 지우면 그 버전으로 실행된 기록이 미아가 된다.
-- 3. **매크로는 권한자끼리 공유한다.** 작성자는 기록으로만 남고 접근 제어에는
--    쓰이지 않는다 — 요구사항이며, 버전 관리가 있어서 성립한다.

CREATE TABLE macros (
    id              TEXT PRIMARY KEY,
    name            TEXT NOT NULL,
    name_lower      TEXT NOT NULL UNIQUE,
    description     TEXT NOT NULL DEFAULT '',
    -- current_version은 실행·편집의 기준이 되는 버전이다.
    current_version INTEGER NOT NULL DEFAULT 0,
    created_by      TEXT REFERENCES users (id) ON DELETE SET NULL,
    created_by_name TEXT NOT NULL DEFAULT '',
    created_at      TEXT NOT NULL,
    updated_at      TEXT NOT NULL,
    updated_by_name TEXT NOT NULL DEFAULT ''
);

CREATE TABLE macro_versions (
    macro_id    TEXT NOT NULL REFERENCES macros (id) ON DELETE CASCADE,
    version     INTEGER NOT NULL,
    graph       TEXT NOT NULL,             -- JSON: nodes/edges/params
    note        TEXT NOT NULL DEFAULT '',
    author_id   TEXT REFERENCES users (id) ON DELETE SET NULL,
    author_name TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL,
    PRIMARY KEY (macro_id, version)
);

-- 사용자가 Lua로 작성해 등록하는 노드.
--
-- scope='global'이면 모든 매크로에서 쓸 수 있고, scope='macro'면 macro_id의
-- 매크로에서만 보인다. 둘 다 필요한 이유: 여러 매크로가 공유하는 유틸리티가 있는가
-- 하면, 한 매크로에서만 의미 있는 단계를 전역 목록에 올려 남의 팔레트를 어지럽히고
-- 싶지 않은 경우도 있다.
CREATE TABLE macro_node_defs (
    id              TEXT PRIMARY KEY,
    name            TEXT NOT NULL,
    scope           TEXT NOT NULL DEFAULT 'global' CHECK (scope IN ('global', 'macro')),
    macro_id        TEXT REFERENCES macros (id) ON DELETE CASCADE,
    description     TEXT NOT NULL DEFAULT '',
    -- fields는 노드 설정 입력칸 정의다(JSON 배열). 화면이 이것을 보고 폼을 그린다.
    fields          TEXT NOT NULL DEFAULT '[]',
    -- ports는 출력 포트 이름 목록이다(JSON 배열). 비어 있으면 ["out"]으로 본다.
    ports           TEXT NOT NULL DEFAULT '[]',
    script          TEXT NOT NULL DEFAULT '',
    current_version INTEGER NOT NULL DEFAULT 1,
    created_by      TEXT REFERENCES users (id) ON DELETE SET NULL,
    created_by_name TEXT NOT NULL DEFAULT '',
    created_at      TEXT NOT NULL,
    updated_at      TEXT NOT NULL
);

CREATE INDEX idx_node_defs_scope ON macro_node_defs (scope, macro_id);

-- 노드 정의도 버전 관리한다. 전역 노드는 여러 매크로가 함께 쓰므로, 누군가 고쳐서
-- 깨졌을 때 되돌릴 방법이 없으면 그 노드를 쓰는 매크로가 전부 멈춘다.
CREATE TABLE macro_node_def_versions (
    def_id      TEXT NOT NULL REFERENCES macro_node_defs (id) ON DELETE CASCADE,
    version     INTEGER NOT NULL,
    script      TEXT NOT NULL DEFAULT '',
    fields      TEXT NOT NULL DEFAULT '[]',
    ports       TEXT NOT NULL DEFAULT '[]',
    note        TEXT NOT NULL DEFAULT '',
    author_name TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL,
    PRIMARY KEY (def_id, version)
);

-- 실행 이력.
--
-- macro_id에 ON DELETE SET NULL을 걸고 이름을 따로 저장한다. 매크로를 지웠다고
-- 실행 기록까지 사라지면, "누가 무엇을 실행했는가"를 지우는 방법이 매크로 삭제가
-- 되어 버린다.
CREATE TABLE macro_runs (
    id            TEXT PRIMARY KEY,
    macro_id      TEXT REFERENCES macros (id) ON DELETE SET NULL,
    macro_name    TEXT NOT NULL DEFAULT '',
    version       INTEGER NOT NULL DEFAULT 0,
    status        TEXT NOT NULL DEFAULT 'running'
                  CHECK (status IN ('running', 'success', 'failed', 'canceled')),
    actor_id      TEXT REFERENCES users (id) ON DELETE SET NULL,
    actor_name    TEXT NOT NULL DEFAULT '',
    actor_ip      TEXT NOT NULL DEFAULT '',
    params        TEXT NOT NULL DEFAULT '{}',
    -- trigger는 어떻게 시작됐는지다: manual | macro (다른 매크로가 호출)
    trigger       TEXT NOT NULL DEFAULT 'manual',
    parent_run_id TEXT REFERENCES macro_runs (id) ON DELETE SET NULL,
    started_at    TEXT NOT NULL,
    finished_at   TEXT,
    duration_ms   INTEGER NOT NULL DEFAULT 0,
    node_count    INTEGER NOT NULL DEFAULT 0,
    error         TEXT NOT NULL DEFAULT ''
);

CREATE INDEX idx_macro_runs_macro ON macro_runs (macro_id, started_at DESC);
CREATE INDEX idx_macro_runs_started ON macro_runs (started_at DESC);
CREATE INDEX idx_macro_runs_status ON macro_runs (status, started_at DESC);

-- 실행 로그. 노드 단위로 남긴다.
--
-- seq를 따로 두는 이유: 같은 밀리초에 여러 줄이 남으면 시각만으로는 순서를 복원할
-- 수 없다. 로그는 순서가 전부인 자료다.
CREATE TABLE macro_run_logs (
    run_id  TEXT NOT NULL REFERENCES macro_runs (id) ON DELETE CASCADE,
    seq     INTEGER NOT NULL,
    at      TEXT NOT NULL,
    level   TEXT NOT NULL DEFAULT 'info',   -- debug | info | warn | error
    node_id TEXT NOT NULL DEFAULT '',
    node    TEXT NOT NULL DEFAULT '',       -- 표시용 노드 이름
    message TEXT NOT NULL DEFAULT '',
    detail  TEXT NOT NULL DEFAULT '',       -- JSON
    PRIMARY KEY (run_id, seq)
);
