-- P7: 스키마 버전 관리와 마이그레이션 워크플로

-- 스키마 버전. "이 커넥션의 스키마가 이 시점에 이러했다"는 확정된 기록이다.
--
-- schema_snapshots(P4)와 나누는 이유: 스냅샷은 폴러가 자동으로 수집하는 관측치이고
-- 버전은 사람이 확정한 이력이다. 스냅샷은 개수 기준으로 정리되지만 버전은 지우지 않는다.
CREATE TABLE schema_versions (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    connection_id TEXT NOT NULL REFERENCES connections (id) ON DELETE CASCADE,
    -- version_no는 커넥션별로 1부터 증가한다. 화면과 대화에서 쓰는 번호다.
    version_no    INTEGER NOT NULL,
    fingerprint   TEXT NOT NULL,
    schema_json   TEXT NOT NULL,
    -- source: 이 버전이 어떻게 생겼는가
    --   initial_import — 커넥션 등록 후 현재 상태를 기준선으로 등록
    --   migration      — 이 앱이 실행한 마이그레이션 결과
    --   external_edit  — 앱 밖에서 바뀐 것을 드리프트 감지로 발견해 등록
    --   rollback       — 롤백 실행 결과
    source        TEXT NOT NULL
                  CHECK (source IN ('initial_import', 'migration', 'external_edit', 'rollback')),
    note          TEXT NOT NULL DEFAULT '',
    -- 이전 버전과의 변경 요약(JSON 배열). 타임라인에 그대로 보여준다.
    change_summary TEXT NOT NULL DEFAULT '[]',
    author_id     TEXT REFERENCES users (id) ON DELETE SET NULL,
    author_name   TEXT NOT NULL DEFAULT '',
    created_at    TEXT NOT NULL,
    UNIQUE (connection_id, version_no)
);

CREATE INDEX idx_schema_versions_conn ON schema_versions (connection_id, version_no DESC);

-- 마이그레이션. 초안(ERD) 또는 두 버전의 차이에서 만들어지는 실행 단위다.
CREATE TABLE migrations (
    id            TEXT PRIMARY KEY,
    connection_id TEXT NOT NULL REFERENCES connections (id) ON DELETE CASCADE,
    -- 출처 초안. 초안이 지워져도 실행 이력은 남아야 하므로 SET NULL이다.
    doc_id        TEXT REFERENCES erd_documents (id) ON DELETE SET NULL,
    title         TEXT NOT NULL,
    -- from_version은 이 마이그레이션이 전제하는 시작 상태다.
    from_version  INTEGER REFERENCES schema_versions (id) ON DELETE SET NULL,
    -- to_version은 실행 성공 후 확정된 결과 버전이다 (실행 전에는 NULL).
    to_version    INTEGER REFERENCES schema_versions (id) ON DELETE SET NULL,
    -- base_fingerprint는 계획을 만든 시점의 대상 DB 구조 지문이다.
    -- 실행 직전 프리체크가 이 값과 실제 DB를 비교해, 그 사이 스키마가 바뀌었으면 막는다.
    -- 이것이 없으면 오래된 계획을 그대로 실행해 남의 변경을 덮어쓸 수 있다.
    base_fingerprint   TEXT NOT NULL,
    -- target_schema_json은 목표 상태(초안)다. 실행 후 결과 검증과 버전 등록에 쓴다.
    target_schema_json TEXT NOT NULL,
    up_sql        TEXT NOT NULL,
    down_sql      TEXT NOT NULL,
    plan_json     TEXT NOT NULL,
    diff_json     TEXT NOT NULL,
    destructive_count INTEGER NOT NULL DEFAULT 0,
    -- 되돌릴 수 없는 변경 목록(JSON 배열). 롤백으로 데이터가 복구되지 않음을 알린다.
    irreversible  TEXT NOT NULL DEFAULT '[]',
    status        TEXT NOT NULL DEFAULT 'draft'
                  CHECK (status IN ('draft', 'in_review', 'approved', 'rejected',
                                    'applied', 'rolled_back', 'failed')),
    -- 실행 기록. 부분 적용이 가능한 DB(MySQL, Oracle은 DDL이 암묵적 커밋)에서
    -- "어디까지 적용됐는가"는 복구의 출발점이므로 반드시 남긴다.
    applied_statements INTEGER NOT NULL DEFAULT 0,
    execution_log TEXT NOT NULL DEFAULT '[]',
    error         TEXT NOT NULL DEFAULT '',
    created_by    TEXT REFERENCES users (id) ON DELETE SET NULL,
    created_at    TEXT NOT NULL,
    updated_at    TEXT NOT NULL,
    applied_at    TEXT,
    applied_by    TEXT REFERENCES users (id) ON DELETE SET NULL,
    rolled_back_at TEXT
);

CREATE INDEX idx_migrations_conn ON migrations (connection_id, created_at DESC);
CREATE INDEX idx_migrations_status ON migrations (status, updated_at DESC);

-- 리뷰 기록. 승인은 사람 단위로 세므로 같은 사람의 재승인은 덮어쓴다.
CREATE TABLE migration_reviews (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    migration_id  TEXT NOT NULL REFERENCES migrations (id) ON DELETE CASCADE,
    reviewer_id   TEXT REFERENCES users (id) ON DELETE SET NULL,
    reviewer_name TEXT NOT NULL DEFAULT '',
    decision      TEXT NOT NULL CHECK (decision IN ('approved', 'rejected', 'comment')),
    comment       TEXT NOT NULL DEFAULT '',
    created_at    TEXT NOT NULL
);

CREATE INDEX idx_migration_reviews_mig ON migration_reviews (migration_id, id);
