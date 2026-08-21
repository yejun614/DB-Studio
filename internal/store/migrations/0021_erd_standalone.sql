-- +no-foreign-keys
--
-- 커넥션 없는 ERD 초안.
--
-- 지금까지 모든 초안은 대상 DB를 하나 정해야 만들 수 있었다. 그 제약에는 이유가
-- 있었다 — dialect와 편집 권한이 커넥션에서 나온다. 하지만 설계는 DB를 만들기 전에
-- 시작되는 일이 더 많고, "어디에 적용할지"는 나중에 정해도 되는 것이었다.
--
-- 그래서 connection_id를 NULL 허용으로 바꾼다. NULL이면 독립 초안이며,
-- dialect는 만들 때 사람이 고르고, 권한은 대상이 없으므로 로그인 여부와 작성자
-- (created_by)로 판정한다. 커넥션이 붙은 초안의 동작은 그대로다.
--
-- SQLite는 컬럼의 NOT NULL을 없앨 수 없어 테이블을 새로 만들어 옮긴다.
-- 이 파일이 -- +no-foreign-keys 로 시작하는 이유가 그것이다: 외래키 검사가 켜진
-- 상태로 옛 테이블을 DROP 하면 erd_ops·erd_chat_messages가 CASCADE로 함께 지워진다.

CREATE TABLE erd_documents_new (
    id            TEXT PRIMARY KEY,
    name          TEXT NOT NULL,
    -- NULL이면 대상 DB가 없는 독립 초안이다.
    -- 값이 있으면 지금까지와 같이 그 커넥션이 dialect와 권한의 근거가 되고,
    -- 커넥션이 지워지면 초안도 함께 지워진다.
    connection_id TEXT REFERENCES connections (id) ON DELETE CASCADE,
    dialect       TEXT NOT NULL,
    status        TEXT NOT NULL DEFAULT 'draft'
                  CHECK (status IN ('draft', 'in_review', 'applied', 'archived')),
    snapshot_json TEXT NOT NULL,
    layout_json   TEXT NOT NULL DEFAULT '{}',
    notes_json    TEXT NOT NULL DEFAULT '[]',
    groups_json   TEXT NOT NULL DEFAULT '[]',
    snapshot_seq  INTEGER NOT NULL DEFAULT 0,
    seq           INTEGER NOT NULL DEFAULT 0,
    source_snapshot_id INTEGER REFERENCES schema_snapshots (id) ON DELETE SET NULL,
    note          TEXT NOT NULL DEFAULT '',
    created_by    TEXT REFERENCES users (id) ON DELETE SET NULL,
    created_at    TEXT NOT NULL,
    updated_at    TEXT NOT NULL
);

INSERT INTO erd_documents_new
    (id, name, connection_id, dialect, status, snapshot_json, layout_json, notes_json,
     groups_json, snapshot_seq, seq, source_snapshot_id, note, created_by, created_at, updated_at)
SELECT
    id, name, connection_id, dialect, status, snapshot_json, layout_json, notes_json,
    groups_json, snapshot_seq, seq, source_snapshot_id, note, created_by, created_at, updated_at
FROM erd_documents;

DROP TABLE erd_documents;
ALTER TABLE erd_documents_new RENAME TO erd_documents;

CREATE INDEX idx_erd_documents_conn ON erd_documents (connection_id, updated_at DESC);
CREATE INDEX idx_erd_documents_status ON erd_documents (status, updated_at DESC);
-- 독립 초안은 커넥션으로 좁힐 수 없으므로 작성자로 찾는다(삭제·설정 권한 판정).
CREATE INDEX idx_erd_documents_author ON erd_documents (created_by, updated_at DESC);
