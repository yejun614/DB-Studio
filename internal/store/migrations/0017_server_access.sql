-- P15: 서버 단위 권한 부여.
--
-- 서버 개념이 생기면서 권한 화면의 일이 늘었다. DB 10개짜리 서버 하나를 등록하면
-- 체크박스를 10번 눌러야 하고, DB를 추가할 때마다 모든 사용자의 권한을 다시 챙겨야 한다.
-- 그 부담은 결국 "그냥 모든 DB 접근 가능으로 두자"로 이어진다 — 권한 모델이 있으나 마나 해진다.
--
-- 그래서 **서버 단위 일괄 부여**를 얹되, 커넥션(DB) 단위 지정은 그대로 둔다.
-- 판정 순서는 좁은 것이 이긴다: 커넥션 오버라이드 > 서버 오버라이드 > 기본값.
-- "이 서버 전체에 모니터링, 단 billing DB만 접근 불가"가 표현되어야 하기 때문이다.

-- 접근 범위(allowlist/denylist)의 서버 항목.
-- 커넥션 항목(user_db_access_items)과 별도 테이블인 이유: 둘은 서로를 덮어쓰지 않는다.
-- allowlist에서는 "서버가 목록에 있거나 그 DB가 목록에 있으면" 범위 안이고,
-- denylist에서는 어느 한쪽이라도 목록에 있으면 차단이다.
CREATE TABLE user_server_access_items (
    user_id   TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    server_id TEXT NOT NULL REFERENCES servers (id) ON DELETE CASCADE,
    PRIMARY KEY (user_id, server_id)
);

-- 서버 단위 등급 오버라이드. 커넥션 오버라이드가 있으면 그쪽이 이긴다.
CREATE TABLE user_server_capability (
    user_id    TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    server_id  TEXT NOT NULL REFERENCES servers (id) ON DELETE CASCADE,
    level      TEXT NOT NULL CHECK (level IN ('none', 'monitor', 'erd', 'migrate')),
    updated_at TEXT NOT NULL,
    PRIMARY KEY (user_id, server_id)
);

-- 서버 단위 데이터 능력 오버라이드. 등급과 별도인 이유는 0009와 같다 —
-- 두 축은 독립적으로 지정되며, 한 테이블에 합치면 한쪽만 바꾸려다 다른 쪽이 조용히 넓어진다.
CREATE TABLE user_server_data_caps (
    user_id    TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    server_id  TEXT NOT NULL REFERENCES servers (id) ON DELETE CASCADE,
    caps       TEXT NOT NULL DEFAULT '',
    updated_at TEXT NOT NULL,
    PRIMARY KEY (user_id, server_id)
);

-- 기존 설정을 서버 쪽으로 복제하지 않는다.
-- 이관 시점에는 서버 하나에 DB가 하나뿐이므로 커넥션 설정만으로 결과가 같고,
-- 양쪽에 같은 값을 넣어 두면 나중에 커넥션 쪽을 지웠을 때 서버 값이 남아
-- "분명히 지웠는데 여전히 접근된다"가 된다.
