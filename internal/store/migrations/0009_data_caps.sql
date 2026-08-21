-- P11: 권한의 두 번째 축 — 데이터 능력(커넥션별)과 전역 권한(사용자별).
--
-- 기존 level(none<monitor<erd<migrate)은 설계·운영 작업의 사다리다. 데이터 조회/수정은
-- 그 사다리의 어느 칸도 아니다(자세한 이유는 internal/model/perm.go). 그래서 열을 더하지
-- 않고 별도의 축으로 저장한다.

-- 오버라이드가 없는 커넥션에 적용되는 기본 능력. 빈 문자열 = 아무 능력 없음.
-- 기본값을 빈 문자열로 두는 것이 중요하다: 이 마이그레이션이 적용되는 순간
-- 기존 사용자 누구에게도 데이터 접근 권한이 생기지 않아야 한다.
ALTER TABLE user_db_access ADD COLUMN default_caps TEXT NOT NULL DEFAULT '';

-- 커넥션별 능력 오버라이드. user_db_capability(등급)와 별도 테이블인 이유:
-- 두 축은 독립적으로 지정된다. 한 테이블에 합치면 능력만 바꾸려 해도 등급 값을
-- 함께 적어야 하고, 그 과정에서 등급이 조용히 넓어지는 사고가 난다.
CREATE TABLE user_db_data_caps (
    user_id       TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    connection_id TEXT NOT NULL REFERENCES connections (id) ON DELETE CASCADE,
    caps          TEXT NOT NULL DEFAULT '',  -- 콤마 구분: data.read,data.write,sql.run
    updated_at    TEXT NOT NULL,
    PRIMARY KEY (user_id, connection_id)
);

-- 전역 권한. 콤마 구분: macro,script.run
ALTER TABLE users ADD COLUMN perms TEXT NOT NULL DEFAULT '';
