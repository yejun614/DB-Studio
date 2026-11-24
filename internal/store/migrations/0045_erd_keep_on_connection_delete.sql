-- +no-foreign-keys
--
-- 커넥션을 지워도 ERD 초안은 보존한다.
--
-- 지금까지 connection_id에는 ON DELETE CASCADE가 걸려 있어 대상 DB가 사라지면
-- 그 위에 그린 초안도 함께 사라졌다. 그 동작의 근거는 "대상이 없는 초안은 dialect와
-- 권한을 판정할 수 없다"였지만, ERD 초안은 이미 dialect와 프로젝트를 따로 들고 있는
-- 데다 대상이 사라진 뒤에도 읽기·설정·SQL 내보내기로 가치가 남는다. 커넥션만
-- 없어진 초안은 독립 초안과 같은 길로 이어지면 된다.
--
-- 이 파일이 -- +no-foreign-keys 로 시작하는 이유는 0021·0037과 같다: SQLite는
-- 외래키 정의를 바꿀 수 없어 테이블을 새로 만들어 옮기는데, 외래키 검사가 켜진
-- 채로 옛 테이블을 DROP 하면 op-log와 채팅이 CASCADE로 함께 지워진다.

CREATE TABLE erd_documents_new (
    id            TEXT PRIMARY KEY,
    name          TEXT NOT NULL,
    -- 대상 커넥션. 지워져도 초안은 보존되므로 ON DELETE SET NULL이다 — 지워지면
    -- 독립 초안처럼 동작한다.
    connection_id TEXT REFERENCES connections (id) ON DELETE SET NULL,
    dialect       TEXT NOT NULL,
    status        TEXT NOT NULL DEFAULT 'draft'
                  CHECK (status IN ('draft', 'in_review', 'applied', 'archived')),
    snapshot_json TEXT NOT NULL,
    layout_json   TEXT NOT NULL DEFAULT '{}',
    notes_json    TEXT NOT NULL DEFAULT '[]',
    groups_json   TEXT NOT NULL DEFAULT '[]',
    domains_json  TEXT NOT NULL DEFAULT '[]',
    snapshot_seq  INTEGER NOT NULL DEFAULT 0,
    seq           INTEGER NOT NULL DEFAULT 0,
    source_snapshot_id INTEGER REFERENCES schema_snapshots (id) ON DELETE SET NULL,
    note          TEXT NOT NULL DEFAULT '',
    created_by    TEXT REFERENCES users (id) ON DELETE SET NULL,
    created_at    TEXT NOT NULL,
    updated_at    TEXT NOT NULL,
    kind          TEXT NOT NULL DEFAULT 'draft',
    project_id    TEXT NOT NULL DEFAULT ''
);

INSERT INTO erd_documents_new
    (id, name, connection_id, dialect, status, snapshot_json, layout_json, notes_json,
     groups_json, domains_json, snapshot_seq, seq, source_snapshot_id, note, created_by,
     created_at, updated_at, kind, project_id)
SELECT
    id, name, connection_id, dialect, status, snapshot_json, layout_json, notes_json,
    groups_json, domains_json, snapshot_seq, seq, source_snapshot_id, note, created_by,
    created_at, updated_at, kind, project_id
FROM erd_documents;

DROP TABLE erd_documents;
ALTER TABLE erd_documents_new RENAME TO erd_documents;

CREATE INDEX idx_erd_documents_conn ON erd_documents (connection_id, updated_at DESC);
CREATE INDEX idx_erd_documents_status ON erd_documents (status, updated_at DESC);
CREATE INDEX idx_erd_documents_project ON erd_documents (project_id, updated_at DESC);
-- 커넥션당 구조 문서는 하나다. 둘이 되면 사람마다 다른 방에 들어가 서로의 메모가
-- 보이지 않는데, 그 사실은 화면에서 알 수 없다. 커넥션이 사라진 문서는 connection_id가
-- NULL이어서 이 제약에 걸리지 않는다(NULL은SQLite 고유 제약에서 중복 허용이다).
CREATE UNIQUE INDEX idx_erd_documents_structure
    ON erd_documents (connection_id) WHERE kind = 'structure';