-- P9: AI 어시스턴트 (프로바이더 키, 세션, 대화, 보류 중인 쓰기 제안)

-- AI 프로바이더 설정. API 키는 DB 비밀번호와 같은 방식으로 봉인해 저장한다.
CREATE TABLE ai_providers (
    id            TEXT PRIMARY KEY,
    name          TEXT NOT NULL,
    -- provider: anthropic(네이티브 Messages API) | openai(호환 chat/completions)
    -- 두 종류만 두는 이유는 base_url을 사용자가 지정할 수 있어 OpenAI 호환 어댑터
    -- 하나로 OpenAI 본체·로컬 LLM·게이트웨이를 모두 커버할 수 있기 때문이다.
    provider      TEXT NOT NULL CHECK (provider IN ('anthropic', 'openai')),
    base_url      TEXT NOT NULL DEFAULT '',
    api_key_enc   TEXT NOT NULL,
    default_model TEXT NOT NULL DEFAULT '',
    enabled       INTEGER NOT NULL DEFAULT 1,
    -- is_default가 1인 프로바이더가 새 세션의 기본값이 된다.
    is_default    INTEGER NOT NULL DEFAULT 0,
    last_check_at TEXT,
    last_check_ok INTEGER,
    last_check_msg TEXT NOT NULL DEFAULT '',
    created_by    TEXT REFERENCES users (id) ON DELETE SET NULL,
    created_at    TEXT NOT NULL,
    updated_at    TEXT NOT NULL,
    UNIQUE (name)
);

-- 대화 세션. 사용자별로 분리된다 — 다른 사람의 대화는 볼 수 없다.
--
-- 세션이 사용자에 묶이는 이유는 권한이다: 툴은 호출자의 권한으로 실행되므로,
-- 대화 이력에는 그 사람만 볼 수 있는 데이터(지표·로그·스키마)가 들어 있다.
CREATE TABLE ai_sessions (
    id            TEXT PRIMARY KEY,
    user_id       TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    title         TEXT NOT NULL DEFAULT '',
    provider_id   TEXT REFERENCES ai_providers (id) ON DELETE SET NULL,
    model         TEXT NOT NULL DEFAULT '',
    -- connection_id가 있으면 그 커넥션을 대화의 기본 대상으로 삼는다.
    connection_id TEXT REFERENCES connections (id) ON DELETE SET NULL,
    -- 누적 토큰 사용량. 비용 추적과 컨텍스트 관리에 쓴다.
    input_tokens  INTEGER NOT NULL DEFAULT 0,
    output_tokens INTEGER NOT NULL DEFAULT 0,
    archived      INTEGER NOT NULL DEFAULT 0,
    created_at    TEXT NOT NULL,
    updated_at    TEXT NOT NULL
);

CREATE INDEX idx_ai_sessions_user ON ai_sessions (user_id, updated_at DESC);

-- 대화 메시지. 툴 호출과 결과도 메시지로 남긴다.
--
-- 툴 호출/결과를 별도 테이블로 나누지 않는 이유: 프로바이더에 다시 보낼 때
-- 순서가 의미를 가지므로 한 시퀀스로 두는 것이 정확하다.
CREATE TABLE ai_messages (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id    TEXT NOT NULL REFERENCES ai_sessions (id) ON DELETE CASCADE,
    -- role: user | assistant | tool
    role          TEXT NOT NULL CHECK (role IN ('user', 'assistant', 'tool')),
    text          TEXT NOT NULL DEFAULT '',
    -- tool_calls / tool_results는 JSON 배열이다.
    tool_calls    TEXT NOT NULL DEFAULT '[]',
    tool_results  TEXT NOT NULL DEFAULT '[]',
    input_tokens  INTEGER NOT NULL DEFAULT 0,
    output_tokens INTEGER NOT NULL DEFAULT 0,
    -- error가 비어 있지 않으면 이 턴이 실패했다는 뜻이다.
    error         TEXT NOT NULL DEFAULT '',
    created_at    TEXT NOT NULL
);

CREATE INDEX idx_ai_messages_session ON ai_messages (session_id, id);

-- 보류 중인 쓰기 제안.
--
-- 계획에서 정한 원칙: AI가 쓰기·파괴적 동작을 직접 실행하지 못한다. 모델이 그런 툴을
-- 호출하면 실행 대신 이 표에 제안을 남기고, 사용자가 화면에서 승인 버튼을 눌러야
-- 실행된다. 그래서 "모델이 잘못 판단해서 운영 DB를 바꿨다"가 구조적으로 불가능하다.
CREATE TABLE ai_pending_actions (
    id            TEXT PRIMARY KEY,
    session_id    TEXT NOT NULL REFERENCES ai_sessions (id) ON DELETE CASCADE,
    -- message_id는 이 제안을 만든 assistant 메시지다.
    message_id    INTEGER REFERENCES ai_messages (id) ON DELETE CASCADE,
    -- tool_call_id는 프로바이더가 부여한 호출 ID다. 승인/거부 결과를 그 호출에
    -- 대한 응답으로 되돌려야 모델이 대화를 이어갈 수 있다.
    tool_call_id  TEXT NOT NULL,
    tool_name     TEXT NOT NULL,
    arguments     TEXT NOT NULL DEFAULT '{}',
    -- summary는 사용자에게 보여줄 한국어 설명이다.
    summary       TEXT NOT NULL DEFAULT '',
    -- preview는 실행 전에 보여줄 상세(예: 생성될 SQL). JSON.
    preview       TEXT NOT NULL DEFAULT '{}',
    status        TEXT NOT NULL DEFAULT 'pending'
                  CHECK (status IN ('pending', 'approved', 'rejected', 'failed', 'expired')),
    result        TEXT NOT NULL DEFAULT '',
    error         TEXT NOT NULL DEFAULT '',
    decided_by    TEXT REFERENCES users (id) ON DELETE SET NULL,
    decided_at    TEXT,
    created_at    TEXT NOT NULL
);

CREATE INDEX idx_ai_pending_session ON ai_pending_actions (session_id, id);
CREATE INDEX idx_ai_pending_status ON ai_pending_actions (status, created_at DESC);
