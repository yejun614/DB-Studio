-- P6: ERD 초안 문서, 편집 op-log, 문서별 채팅

-- ERD 문서. 스키마 구조는 schema.Schema(IR) JSON으로, 캔버스 좌표와 메모는
-- 별도 컬럼으로 담는다. 좌표를 IR에 넣지 않는 이유: IR의 지문이 드리프트 감지와
-- 버전 비교의 기준이므로, 테이블을 옮기기만 해도 "스키마가 바뀌었다"가 되면 안 된다.
CREATE TABLE erd_documents (
    id            TEXT PRIMARY KEY,
    name          TEXT NOT NULL,
    -- 대상 커넥션. 이 값으로 dialect가 정해지고 권한(ERD 등급)이 판정된다.
    -- 커넥션이 삭제되면 문서도 함께 지운다 — 대상이 없는 초안은 마이그레이션할 수 없다.
    connection_id TEXT NOT NULL REFERENCES connections (id) ON DELETE CASCADE,
    dialect       TEXT NOT NULL,
    status        TEXT NOT NULL DEFAULT 'draft'
                  CHECK (status IN ('draft', 'in_review', 'applied', 'archived')),
    -- 스냅샷과 그 시점의 seq. 재접속 시 스냅샷 + 이후 op만 보내면 되므로
    -- op-log가 길어져도 최초 로딩 비용이 늘지 않는다.
    snapshot_json TEXT NOT NULL,
    layout_json   TEXT NOT NULL DEFAULT '{}',
    notes_json    TEXT NOT NULL DEFAULT '[]',
    snapshot_seq  INTEGER NOT NULL DEFAULT 0,
    -- seq는 이 문서에 적용된 마지막 op 순번이다. op 저장과 함께 한 트랜잭션에서 올린다.
    seq           INTEGER NOT NULL DEFAULT 0,
    -- source_snapshot_id는 실제 DB 스키마에서 가져온 초안일 때 그 출처다.
    -- P7에서 "무엇을 기준으로 그린 초안인가"를 판단하는 데 쓴다.
    source_snapshot_id INTEGER REFERENCES schema_snapshots (id) ON DELETE SET NULL,
    note          TEXT NOT NULL DEFAULT '',
    created_by    TEXT REFERENCES users (id) ON DELETE SET NULL,
    created_at    TEXT NOT NULL,
    updated_at    TEXT NOT NULL
);

CREATE INDEX idx_erd_documents_conn ON erd_documents (connection_id, updated_at DESC);
CREATE INDEX idx_erd_documents_status ON erd_documents (status, updated_at DESC);

-- 편집 op-log. 문서 상태는 스냅샷에서 복원할 수 있으므로 op-log는 이력이지만,
-- 그 이상의 역할이 있다: 이름 변경 같은 의도를 담는다. diff는 두 스키마만 보고
-- rename을 추론하지 않으므로(잘못 짚으면 데이터가 사라진다), P7 마이그레이션은
-- 이 op-log에서 "삭제+생성"이 아니라 "이름 변경"이었음을 읽는다.
CREATE TABLE erd_ops (
    doc_id     TEXT NOT NULL REFERENCES erd_documents (id) ON DELETE CASCADE,
    seq        INTEGER NOT NULL,
    op_id      TEXT NOT NULL,          -- 클라이언트가 만든 식별자 (자기 op 식별 / 재전송 감지)
    kind       TEXT NOT NULL,
    payload    TEXT NOT NULL,          -- JSON
    actor_id   TEXT REFERENCES users (id) ON DELETE SET NULL,
    actor_name TEXT NOT NULL DEFAULT '',
    base_seq   INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL,
    PRIMARY KEY (doc_id, seq)
);

-- 같은 op_id 재전송을 막는다. 네트워크가 끊겼다 붙으면 클라이언트가 확인받지 못한
-- op를 다시 보내는데, 그때 두 번 적용되면 컬럼이 둘 생긴다.
CREATE UNIQUE INDEX idx_erd_ops_opid ON erd_ops (doc_id, op_id);

-- 문서별 채팅. 프레즌스(커서·선택)는 휘발성이라 메모리에만 두지만,
-- 채팅은 "왜 이렇게 설계했는가"의 기록이므로 영속한다.
CREATE TABLE erd_chat_messages (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    doc_id     TEXT NOT NULL REFERENCES erd_documents (id) ON DELETE CASCADE,
    user_id    TEXT REFERENCES users (id) ON DELETE SET NULL,
    user_name  TEXT NOT NULL DEFAULT '',
    body       TEXT NOT NULL,
    -- kind: message(사람이 쓴 글) | system(참여/이탈 등 자동 기록)
    kind       TEXT NOT NULL DEFAULT 'message' CHECK (kind IN ('message', 'system')),
    -- 대화가 특정 테이블에 대한 것이면 그 키를 남긴다. 캔버스에서 역참조할 수 있다.
    target_key TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL
);

CREATE INDEX idx_erd_chat_doc ON erd_chat_messages (doc_id, id);
