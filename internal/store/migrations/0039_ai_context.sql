-- +no-foreign-keys
--
-- 프로바이더별 컨텍스트 크기, 그리고 Ollama 네이티브 종류.
--
-- 두 가지를 함께 하는 이유는 둘 다 ai_providers 를 다시 만들어야 하기 때문이다.
-- provider 열에 CHECK 제약이 걸려 있는데 SQLite는 제약만 바꿀 수 없다.
--
-- ## 컨텍스트 크기
--
-- 지금까지 대화 이력은 프로바이더와 무관하게 12만 자에서 잘렸다. 그 숫자를 정할
-- 때의 근거는 "넘치면 프로바이더가 오류로 알려준다"였는데, **Ollama에서는 그 가정이
-- 틀렸다.** Ollama는 컨텍스트를 넘는 프롬프트를 오류 없이 앞에서 잘라낸다.
-- 시스템 프롬프트와 처음의 지시가 먼저 사라지고, 모델은 자기가 무엇을 하던 중인지
-- 모르는 채로 답한다. 사람 눈에는 "갑자기 엉뚱한 소리를 한다"로 보이고, 어디에도
-- 그 이유가 적히지 않는다.
--
-- 반대쪽도 문제다. Claude는 20만 토큰을 받는데 12만 자(대략 3~4만 토큰)에서 자르면
-- 멀쩡한 이력을 이유 없이 버린다.
--
-- 단위는 **토큰**이다 — 모델 카드에 적힌 값이 토큰이고, 사람이 옮겨 적을 수 있어야
-- 한다. 문자 수로의 환산은 코드가 한다(api.historyBudget).
--
-- 0은 "모름"이고, 그때는 예전과 같은 기본값을 쓴다. 이미 돌고 있는 설치가 이 열이
-- 생겼다는 이유로 동작이 달라지면 안 된다.
--
-- ## ollama
--
-- OpenAI 호환 어댑터로도 Ollama에 붙을 수 있는데 왜 종류를 더하는가: 호환
-- 엔드포인트(/v1/chat/completions)는 options.num_ctx 를 받지 않는다. 위에 적은
-- "말없이 잘라내는" 문제를 막을 방법이 그 길에는 없다. 네이티브 API(/api/chat)에는
-- 있다. 로컬과 Ollama Cloud가 같은 규약을 쓰므로 종류 하나로 둘 다 다룬다.
--
-- 기존 행은 건드리지 않는다. openai 로 등록해 둔 Ollama가 있어도 그대로 돈다 —
-- 컨텍스트를 정할 수 없을 뿐이고, 바꾸고 싶으면 화면에서 종류만 고치면 된다.
CREATE TABLE ai_providers_new (
    id            TEXT PRIMARY KEY,
    name          TEXT NOT NULL,
    -- provider: anthropic(네이티브 Messages API)
    --         | openai(호환 chat/completions)
    --         | ollama(네이티브 /api/chat — 컨텍스트 크기를 정할 수 있다)
    provider      TEXT NOT NULL CHECK (provider IN ('anthropic', 'openai', 'ollama')),
    base_url      TEXT NOT NULL DEFAULT '',
    api_key_enc   TEXT NOT NULL,
    default_model TEXT NOT NULL DEFAULT '',
    allowed_models TEXT NOT NULL DEFAULT '[]',
    -- context_tokens: 이 프로바이더가 한 번에 받는 토큰 수. 0은 모름.
    context_tokens INTEGER NOT NULL DEFAULT 0,
    enabled       INTEGER NOT NULL DEFAULT 1,
    is_default    INTEGER NOT NULL DEFAULT 0,
    last_check_at TEXT,
    last_check_ok INTEGER,
    last_check_msg TEXT NOT NULL DEFAULT '',
    created_by    TEXT REFERENCES users (id) ON DELETE SET NULL,
    created_at    TEXT NOT NULL,
    updated_at    TEXT NOT NULL,
    UNIQUE (name)
);

INSERT INTO ai_providers_new
    (id, name, provider, base_url, api_key_enc, default_model, allowed_models,
     context_tokens, enabled, is_default, last_check_at, last_check_ok,
     last_check_msg, created_by, created_at, updated_at)
SELECT id, name, provider, base_url, api_key_enc, default_model, allowed_models,
       0, enabled, is_default, last_check_at, last_check_ok,
       last_check_msg, created_by, created_at, updated_at
FROM ai_providers;

DROP TABLE ai_providers;
ALTER TABLE ai_providers_new RENAME TO ai_providers;
