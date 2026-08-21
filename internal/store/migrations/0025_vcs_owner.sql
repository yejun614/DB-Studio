-- P19: Git 연동은 개인의 것이다.
--
-- 지금까지 vcs_integrations는 커넥션 관리자가 등록하고 모두가 함께 쓰는 설정이었다.
-- 그런데 이 설정에 담기는 것은 **개인의 Git 계정 토큰**이다. 그것을 공유하면 두 가지가
-- 무너진다.
--
--  1. 원격 저장소에서 "누가 올렸는가"가 사라진다. 모든 PR이 토큰 주인의 이름으로 열리므로,
--     스키마 변경의 책임자를 Git 쪽 기록만으로는 알 수 없다. 이 앱은 감사 로그에서
--     "누구의 권한으로 실행되는가"를 일관되게 지켜 왔는데 이 경로만 예외였다.
--  2. 남의 계정 자격증명을 쥔 사람이 생긴다. 토큰은 이 앱 밖(저장소, 이슈, CI)에서도
--     쓸 수 있으므로, 그 범위는 DB Studio의 권한 모델이 감당하는 범위를 넘는다.
--
-- 그래서 소유자를 붙이고 조회·수정·삭제를 소유자로 좁힌다. **슈퍼 어드민도 예외가 아니다** —
-- 남의 Git 계정을 볼 수 있어야 할 이유가 없고, 볼 수 있다면 그것은 곧 그 사람 명의로
-- 무언가를 할 수 있다는 뜻이다. API 토큰을 남이 발급할 수 없게 한 것과 같은 규칙이다.
--
-- 기존 연동은 지운다(사용자 결정). 공유 연동에는 "이것이 누구의 계정인가"가 기록되어 있지
-- 않다 — created_by는 등록한 사람일 뿐 토큰 주인이라는 보장이 없다. 잘못된 주인에게
-- 넘기느니 각자 자기 계정으로 다시 등록하는 편이 안전하다.
--
-- 푸시 이력은 연동에 딸려 있으므로 함께 사라진다. 이미 원격에 올라간 커밋과 PR은
-- 그대로 남아 있고, 이 앱의 기록만 없어진다.
DELETE FROM vcs_pushes;
DELETE FROM vcs_integrations;

-- SQLite는 열의 제약(NOT NULL, 외래키, UNIQUE)을 나중에 바꿀 수 없다.
-- 행이 없는 지금이 표를 다시 만들 수 있는 유일한 시점이다.
DROP TABLE vcs_integrations;

CREATE TABLE vcs_integrations (
    id            TEXT PRIMARY KEY,
    -- owner_id는 이 연동의 주인이다. 계정이 사라지면 함께 사라진다 —
    -- 주인 없는 토큰이 DB에 남아 있을 이유가 없다.
    owner_id      TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name          TEXT NOT NULL,
    provider      TEXT NOT NULL CHECK (provider IN ('github', 'gitlab', 'bitbucket')),
    base_url      TEXT NOT NULL DEFAULT '',
    repo          TEXT NOT NULL,
    default_branch TEXT NOT NULL DEFAULT 'main',
    branch_template TEXT NOT NULL DEFAULT 'schema/{date}-{slug}',
    path_template   TEXT NOT NULL DEFAULT 'migrations/{ts}_{slug}',
    username      TEXT NOT NULL DEFAULT '',
    token_enc     TEXT NOT NULL,
    connection_id TEXT REFERENCES connections (id) ON DELETE CASCADE,
    enabled       INTEGER NOT NULL DEFAULT 1,
    last_check_at TEXT,
    last_check_ok INTEGER,
    last_check_msg TEXT NOT NULL DEFAULT '',
    created_at    TEXT NOT NULL,
    updated_at    TEXT NOT NULL,
    -- 이름은 사람마다 따로 센다. "회사 저장소"라는 이름을 나만 쓸 수 있게 하면
    -- 다른 사람은 자기 계정을 등록하면서 남의 이름을 피해 가야 한다.
    UNIQUE (owner_id, name)
);

CREATE INDEX idx_vcs_integrations_owner ON vcs_integrations (owner_id, name);
CREATE INDEX idx_vcs_integrations_conn ON vcs_integrations (connection_id);
