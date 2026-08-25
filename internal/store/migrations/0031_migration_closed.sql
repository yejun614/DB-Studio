-- +no-foreign-keys
--
-- P28: 마이그레이션 닫기, 그리고 담당자 기본값.
--
-- 두 가지를 바꾼다.
--
-- 1) 상태에 'closed'를 더한다.
--
-- 지금까지 진행하지 않기로 한 계획은 삭제하는 수밖에 없었다. 그런데 삭제는
-- "이런 계획을 세웠다가 접었다"는 사실까지 지운다 — 나중에 같은 논의가 다시
-- 올라왔을 때 왜 접었는지 아무도 모른다. PR을 닫아 두는 것과 같은 자리다.
--
-- SQLite는 CHECK 제약을 바꿀 수 없어 표를 새로 만들어 옮긴다. 이 파일이
-- -- +no-foreign-keys 로 시작하는 이유가 그것이다: 외래키 검사가 켜진 상태로 옛
-- 표를 DROP 하면 migration_reviews·migration_reviewers가 CASCADE로 함께 지워진다.
--
-- 2) 담당자가 비어 있던 마이그레이션은 만든 사람을 담당자로 채운다.
--
-- 계획을 만든 사람이 그것을 끌고 가는 것이 기본값이다. 아무도 없는 상태로 두면
-- "누가 맡았나"의 답이 대부분 빈칸이 되고, 그러면 그 칸을 아무도 보지 않게 된다.

CREATE TABLE migrations_new (
    id            TEXT PRIMARY KEY,
    connection_id TEXT NOT NULL REFERENCES connections (id) ON DELETE CASCADE,
    doc_id        TEXT REFERENCES erd_documents (id) ON DELETE SET NULL,
    title         TEXT NOT NULL,
    from_version  INTEGER REFERENCES schema_versions (id) ON DELETE SET NULL,
    to_version    INTEGER REFERENCES schema_versions (id) ON DELETE SET NULL,
    rollback_to_version INTEGER REFERENCES schema_versions (id) ON DELETE SET NULL,
    base_fingerprint   TEXT NOT NULL,
    target_schema_json TEXT NOT NULL,
    up_sql        TEXT NOT NULL,
    down_sql      TEXT NOT NULL,
    plan_json     TEXT NOT NULL,
    diff_json     TEXT NOT NULL,
    destructive_count INTEGER NOT NULL DEFAULT 0,
    irreversible  TEXT NOT NULL DEFAULT '[]',
    -- 'closed'는 실행하지 않기로 한 계획이다. 실행된 것(applied/rolled_back)은
    -- 이력이므로 닫을 수 없다 — 그 구분은 코드의 전이 표가 지킨다.
    status        TEXT NOT NULL DEFAULT 'draft'
                  CHECK (status IN ('draft', 'in_review', 'approved', 'rejected',
                                    'applied', 'rolled_back', 'failed', 'closed')),
    applied_statements INTEGER NOT NULL DEFAULT 0,
    execution_log TEXT NOT NULL DEFAULT '[]',
    error         TEXT NOT NULL DEFAULT '',
    created_by    TEXT REFERENCES users (id) ON DELETE SET NULL,
    created_at    TEXT NOT NULL,
    updated_at    TEXT NOT NULL,
    applied_at    TEXT,
    applied_by    TEXT REFERENCES users (id) ON DELETE SET NULL,
    rolled_back_at TEXT,
    assignee_id   TEXT REFERENCES users (id) ON DELETE SET NULL
);

INSERT INTO migrations_new
    (id, connection_id, doc_id, title, from_version, to_version, rollback_to_version,
     base_fingerprint, target_schema_json, up_sql, down_sql, plan_json, diff_json,
     destructive_count, irreversible, status, applied_statements, execution_log, error,
     created_by, created_at, updated_at, applied_at, applied_by, rolled_back_at, assignee_id)
SELECT
    id, connection_id, doc_id, title, from_version, to_version, rollback_to_version,
    base_fingerprint, target_schema_json, up_sql, down_sql, plan_json, diff_json,
    destructive_count, irreversible, status, applied_statements, execution_log, error,
    created_by, created_at, updated_at, applied_at, applied_by, rolled_back_at,
    -- 담당자가 비어 있으면 만든 사람으로 채운다. 만든 사람도 없는 행(계정이 지워진
    -- 경우)은 그대로 비워 둔다.
    COALESCE(assignee_id, created_by)
FROM migrations;

DROP TABLE migrations;
ALTER TABLE migrations_new RENAME TO migrations;

CREATE INDEX idx_migrations_conn ON migrations (connection_id, created_at DESC);
CREATE INDEX idx_migrations_status ON migrations (status, updated_at DESC);
