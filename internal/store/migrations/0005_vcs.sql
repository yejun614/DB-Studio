-- P8: Git 저장소 연동 (GitHub / GitLab / Bitbucket)

-- 연동 설정. 토큰은 커넥션 비밀번호와 같은 방식(AES-256-GCM)으로 봉인해 저장한다.
CREATE TABLE vcs_integrations (
    id            TEXT PRIMARY KEY,
    name          TEXT NOT NULL,
    provider      TEXT NOT NULL CHECK (provider IN ('github', 'gitlab', 'bitbucket')),
    -- base_url이 비어 있으면 공개 SaaS를 쓴다. self-hosted 인스턴스는 여기에 주소를 넣는다.
    base_url      TEXT NOT NULL DEFAULT '',
    -- repo 형식: GitHub owner/repo · GitLab group/project · Bitbucket workspace/repo
    repo          TEXT NOT NULL,
    default_branch TEXT NOT NULL DEFAULT 'main',
    -- 브랜치/경로 템플릿. {date} {ts} {slug} {conn} {env} {version} {id} 를 쓸 수 있다.
    branch_template TEXT NOT NULL DEFAULT 'schema/{date}-{slug}',
    path_template   TEXT NOT NULL DEFAULT 'migrations/{ts}_{slug}',
    -- username은 Bitbucket 앱 비밀번호 인증에만 쓴다.
    username      TEXT NOT NULL DEFAULT '',
    token_enc     TEXT NOT NULL,
    -- connection_id가 있으면 그 커넥션 전용 연동이다. NULL이면 모든 커넥션에서 고를 수 있다.
    -- 스키마 저장소를 DB별로 나누는 조직과 하나로 합치는 조직이 모두 있다.
    connection_id TEXT REFERENCES connections (id) ON DELETE CASCADE,
    enabled       INTEGER NOT NULL DEFAULT 1,
    -- 마지막 확인 결과. 목록에서 "이 연동이 지금 동작하는가"를 보여준다.
    last_check_at TEXT,
    last_check_ok INTEGER,
    last_check_msg TEXT NOT NULL DEFAULT '',
    created_by    TEXT REFERENCES users (id) ON DELETE SET NULL,
    created_at    TEXT NOT NULL,
    updated_at    TEXT NOT NULL,
    UNIQUE (name)
);

CREATE INDEX idx_vcs_integrations_conn ON vcs_integrations (connection_id);

-- 푸시 이력. 어떤 마이그레이션이 어느 브랜치/커밋/PR로 나갔는지 기록한다.
--
-- 실패도 남기는 이유: 토큰 만료나 권한 부족은 반복해서 발생하며, 그때 "무엇이
-- 언제부터 실패하고 있었나"를 알 수 있어야 원인을 좁힐 수 있다.
CREATE TABLE vcs_pushes (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    integration_id TEXT NOT NULL REFERENCES vcs_integrations (id) ON DELETE CASCADE,
    -- 마이그레이션이 지워져도 푸시 이력은 남는다 (이미 원격에 올라간 사실은 사라지지 않는다).
    migration_id   TEXT REFERENCES migrations (id) ON DELETE SET NULL,
    migration_title TEXT NOT NULL DEFAULT '',
    branch         TEXT NOT NULL,
    branch_created INTEGER NOT NULL DEFAULT 0,
    commit_sha     TEXT NOT NULL DEFAULT '',
    commit_url     TEXT NOT NULL DEFAULT '',
    pr_number      INTEGER,
    pr_url         TEXT NOT NULL DEFAULT '',
    pr_existing    INTEGER NOT NULL DEFAULT 0,
    files          TEXT NOT NULL DEFAULT '[]',   -- JSON 배열: 올린 파일 경로
    status         TEXT NOT NULL CHECK (status IN ('ok', 'failed')),
    error          TEXT NOT NULL DEFAULT '',
    actor_id       TEXT REFERENCES users (id) ON DELETE SET NULL,
    actor_name     TEXT NOT NULL DEFAULT '',
    created_at     TEXT NOT NULL
);

CREATE INDEX idx_vcs_pushes_integration ON vcs_pushes (integration_id, id DESC);
CREATE INDEX idx_vcs_pushes_migration ON vcs_pushes (migration_id, id DESC);
