-- P0/P1/P2: 사용자, 세션, 감사로그, DB 커넥션, 접근 권한

CREATE TABLE users (
    id                   TEXT PRIMARY KEY,
    username             TEXT NOT NULL,
    username_lower       TEXT NOT NULL UNIQUE,
    email                TEXT NOT NULL DEFAULT '',
    display_name         TEXT NOT NULL DEFAULT '',
    role                 TEXT NOT NULL CHECK (role IN ('superadmin', 'admin', 'member')),
    password_hash        TEXT NOT NULL,
    must_change_password INTEGER NOT NULL DEFAULT 0,
    status               TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
    last_login_at        TEXT,
    created_at           TEXT NOT NULL,
    updated_at           TEXT NOT NULL,
    created_by           TEXT
);

CREATE TABLE sessions (
    id           TEXT PRIMARY KEY,          -- 토큰의 SHA-256 해시 (원문은 쿠키에만 존재)
    user_id      TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_at   TEXT NOT NULL,
    expires_at   TEXT NOT NULL,
    last_seen_at TEXT NOT NULL,
    ip           TEXT NOT NULL DEFAULT '',
    user_agent   TEXT NOT NULL DEFAULT ''
);

CREATE INDEX idx_sessions_user ON sessions (user_id);
CREATE INDEX idx_sessions_expires ON sessions (expires_at);

CREATE TABLE audit_logs (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    at          TEXT NOT NULL,
    actor_id    TEXT,
    actor_name  TEXT NOT NULL DEFAULT '',
    action      TEXT NOT NULL,
    target_type TEXT NOT NULL DEFAULT '',
    target_id   TEXT NOT NULL DEFAULT '',
    detail      TEXT NOT NULL DEFAULT '{}',  -- JSON
    ip          TEXT NOT NULL DEFAULT '',
    result      TEXT NOT NULL DEFAULT 'ok'
);

CREATE INDEX idx_audit_at ON audit_logs (at DESC);
CREATE INDEX idx_audit_actor ON audit_logs (actor_id, at DESC);
CREATE INDEX idx_audit_target ON audit_logs (target_type, target_id, at DESC);

CREATE TABLE connections (
    id            TEXT PRIMARY KEY,
    name          TEXT NOT NULL,
    name_lower    TEXT NOT NULL UNIQUE,
    kind          TEXT NOT NULL,             -- mysql | postgres | mssql | oracle | sqlite | mongodb | redis
    environment   TEXT NOT NULL CHECK (environment IN ('dev', 'prod')),
    host          TEXT NOT NULL DEFAULT '',
    port          INTEGER NOT NULL DEFAULT 0,
    database_name TEXT NOT NULL DEFAULT '',
    options       TEXT NOT NULL DEFAULT '{}',  -- JSON: sslmode, service_name, tls, params 등
    tags          TEXT NOT NULL DEFAULT '',    -- 콤마 구분
    note          TEXT NOT NULL DEFAULT '',
    enabled       INTEGER NOT NULL DEFAULT 1,
    last_check_at TEXT,
    last_check_ok INTEGER,
    last_check_msg TEXT NOT NULL DEFAULT '',
    created_by    TEXT REFERENCES users (id),
    created_at    TEXT NOT NULL,
    updated_at    TEXT NOT NULL
);

CREATE INDEX idx_connections_env ON connections (environment);
CREATE INDEX idx_connections_kind ON connections (kind);

CREATE TABLE connection_secrets (
    connection_id TEXT PRIMARY KEY REFERENCES connections (id) ON DELETE CASCADE,
    username      TEXT NOT NULL DEFAULT '',
    password_enc  TEXT NOT NULL DEFAULT '',
    extra_enc     TEXT NOT NULL DEFAULT '',  -- 암호화된 JSON (인증서, 키파일 내용 등)
    updated_at    TEXT NOT NULL
);

-- 사용자별 접근 범위. mode에 따라 user_db_access_items의 의미가 달라진다.
--   all       : 모든 커넥션 접근 가능 (items 무시)
--   allowlist : items에 있는 커넥션만 접근 가능
--   denylist  : items에 있는 커넥션만 접근 불가, 나머지 허용
CREATE TABLE user_db_access (
    user_id       TEXT PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    mode          TEXT NOT NULL DEFAULT 'allowlist' CHECK (mode IN ('all', 'allowlist', 'denylist')),
    default_level TEXT NOT NULL DEFAULT 'monitor' CHECK (default_level IN ('none', 'monitor', 'erd', 'migrate')),
    updated_at    TEXT NOT NULL
);

CREATE TABLE user_db_access_items (
    user_id       TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    connection_id TEXT NOT NULL REFERENCES connections (id) ON DELETE CASCADE,
    PRIMARY KEY (user_id, connection_id)
);

-- 커넥션별 능력 등급 오버라이드. 없으면 user_db_access.default_level을 적용한다.
CREATE TABLE user_db_capability (
    user_id       TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    connection_id TEXT NOT NULL REFERENCES connections (id) ON DELETE CASCADE,
    level         TEXT NOT NULL CHECK (level IN ('none', 'monitor', 'erd', 'migrate')),
    updated_at    TEXT NOT NULL,
    PRIMARY KEY (user_id, connection_id)
);
