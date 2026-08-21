-- P24: 클러스터 (마스터-리플리카)
--
-- 여러 서버에서 DB Studio를 띄우고 하나처럼 다루기 위한 표들이다. 마스터가 메타 DB의
-- 유일한 주인이고, 리플리카는 그 변경을 그대로 따라 적는다.
--
-- 왜 논리 복제(행 단위)인가: SQLite 파일을 통째로 주고받으면 리플리카가 열어 둔 DB를
-- 실행 중에 바꿔치기해야 하고, WAL 프레임을 나르면 두 노드의 페이지 배치가 완전히
-- 같아야 한다. 행 단위로 나르면 노드가 각자 자기 파일을 관리하면서도 내용이 같아진다.

-- cluster_nodes는 이 클러스터에 참여한 노드 목록이다.
--
-- 마스터의 메타 DB에 있고 복제되므로, 어느 노드에 접속해도 같은 목록이 보인다.
CREATE TABLE cluster_nodes (
    id       TEXT PRIMARY KEY,
    name     TEXT NOT NULL,
    role     TEXT NOT NULL CHECK (role IN ('master', 'replica')),
    -- address는 다른 노드가 이 노드를 부를 수 있는 주소다(http://host:port).
    -- 비어 있으면 이 노드로는 요청을 넘길 수 없다.
    address  TEXT NOT NULL DEFAULT '',
    version  TEXT NOT NULL DEFAULT '',
    platform TEXT NOT NULL DEFAULT '',

    joined_at    TEXT NOT NULL,
    last_seen_at TEXT NOT NULL,
    -- applied_seq는 그 노드가 복제 로그를 어디까지 적용했는지다.
    -- 마스터의 최신 seq와의 차이가 곧 복제 지연이다.
    applied_seq INTEGER NOT NULL DEFAULT 0,

    -- host_snapshot은 그 노드가 도는 컴퓨터의 최신 상태(JSON: hostmon.Snapshot)다.
    -- 하트비트에 실려 온다 — 각 서버의 CPU·메모리·디스크를 한 화면에서 보기 위해서다.
    host_snapshot TEXT NOT NULL DEFAULT '{}',
    host_at       TEXT NOT NULL DEFAULT '',

    -- status는 운영자가 노드를 목록에서 내렸는지 여부다. 지우지 않고 남기는 이유는
    -- 감사 로그와 이벤트가 node_id를 참조하기 때문이다.
    status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'left'))
);

-- repl_log는 마스터가 만드는 변경 기록이다. 트리거가 채운다(store/cluster.go).
--
-- rowid로 행을 식별하는 이유: 이 앱의 표는 기본키가 제각각(TEXT id, 복합키, 없음)인데
-- rowid는 모든 표에 있다. 리플리카가 같은 rowid로 넣으면 표마다 다른 규칙을 알 필요가 없다.
CREATE TABLE repl_log (
    seq INTEGER PRIMARY KEY AUTOINCREMENT,
    tbl TEXT NOT NULL,
    rid INTEGER NOT NULL,
    op  TEXT NOT NULL CHECK (op IN ('upsert', 'delete')),
    -- row는 JSON 한 줄이다. delete면 NULL.
    row TEXT,
    at  TEXT NOT NULL
);

CREATE INDEX idx_repl_log_at ON repl_log (at);

-- repl_state는 **이 노드가** 어디까지 적용했는지다. 복제되지 않는다
-- (노드마다 값이 달라야 하는 유일한 표다).
CREATE TABLE repl_state (
    k TEXT PRIMARY KEY,
    v TEXT NOT NULL
);

-- node_id는 이 커넥션에 접속하는 노드다. 비어 있으면 요청을 받은 노드가 직접 접속한다.
--
-- 필요한 이유: 분산 환경에서는 어떤 DB가 특정 서버에서만 닿는다(사설망, 방화벽).
-- 그 커넥션을 다루는 요청은 그 노드에서 실행되어야 한다.
ALTER TABLE connections ADD COLUMN node_id TEXT NOT NULL DEFAULT '';

-- 이벤트와 호스트 지표에 노드를 붙인다. 빈 값은 "마스터(또는 단일 서버)"다.
ALTER TABLE events ADD COLUMN node_id TEXT NOT NULL DEFAULT '';
ALTER TABLE host_samples ADD COLUMN node_id TEXT NOT NULL DEFAULT '';
