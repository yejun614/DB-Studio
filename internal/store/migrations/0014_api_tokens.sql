-- P13: API 토큰. MCP 클라이언트를 비롯한 프로그램 접근용 자격증명이다.
--
-- 세션과 나누는 이유: 세션은 브라우저의 것이고 12시간 뒤 사라진다. MCP 클라이언트는
-- 브라우저가 아니고 쿠키를 쓰지 않으며, 몇 달 동안 같은 자격증명으로 붙는다.
-- 세션 TTL을 늘려 그 용도를 겸하게 하면 브라우저 세션까지 함께 길어진다.
--
-- 저장하는 것은 **해시뿐이다**(세션과 같은 규칙). 원문은 발급 순간 한 번만 보여주고
-- 어디에도 남기지 않는다. DB가 유출되어도 토큰으로 로그인할 수 없어야 한다.
CREATE TABLE api_tokens (
    id           TEXT PRIMARY KEY,
    user_id      TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name         TEXT NOT NULL,
    -- token_hash는 원문의 SHA-256이다.
    token_hash   TEXT NOT NULL UNIQUE,
    -- prefix는 목록에서 어느 토큰인지 알아보기 위한 앞부분이다(예: dbs_7Kq2…).
    -- 원문을 복원할 수 없을 만큼 짧게 자른다.
    prefix       TEXT NOT NULL DEFAULT '',
    -- scope: read | write
    --
    -- 기본은 read다. 쓰기를 켜는 것은 의식적인 선택이어야 한다 — 이 토큰을 들고 있는
    -- 프로그램은 사람이 화면에서 누르는 확인 단계를 거치지 않는다.
    scope        TEXT NOT NULL DEFAULT 'read' CHECK (scope IN ('read', 'write')),
    created_at   TEXT NOT NULL,
    expires_at   TEXT,
    last_used_at TEXT,
    -- last_used_ip는 "이 토큰이 어디서 쓰이고 있는가"를 알려준다.
    -- 쓰지 않는 토큰을 지울 때, 그리고 유출을 의심할 때 첫 단서가 된다.
    last_used_ip TEXT NOT NULL DEFAULT '',
    revoked_at   TEXT
);

CREATE INDEX idx_api_tokens_user ON api_tokens (user_id, created_at DESC);
