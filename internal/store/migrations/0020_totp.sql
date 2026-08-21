-- P20: 2단계 인증(TOTP)과 전역 보안 설정.
--
-- 왜 TOTP인가: 비밀번호 하나로 지키기에는 이 앱이 열어 주는 것이 너무 많다.
-- 운영 DB의 데이터 조회·수정, 마이그레이션 실행, 백업 복구가 한 계정 뒤에 있다.
-- SMS나 이메일이 아니라 TOTP를 쓰는 이유는 이 앱이 외부망 없이 도는 곳에도
-- 설치되기 때문이다 — 나가는 통신 없이 동작하는 두 번째 요소는 사실상 이것뿐이다.

-- app_settings는 앱 전역 설정의 키·값 저장소다.
--
-- 설정마다 컬럼을 늘리지 않는 이유: 지금 필요한 것은 두 개(2FA 의무화 여부,
-- 학습된 시계 보정값)뿐이고, 둘의 성격이 전혀 다르다. 하나는 사람이 정하는 정책이고
-- 다른 하나는 앱이 스스로 갱신하는 관측값이다. 이런 것들을 위해 매번 마이그레이션을
-- 추가하면 스키마가 설정 화면을 따라다니게 된다.
CREATE TABLE app_settings (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    -- updated_by는 정책을 바꾼 사람이다. 앱이 스스로 갱신하는 값(시계 보정)은 비어 있다.
    updated_by TEXT
);

-- user_totp는 한 사용자의 2단계 인증 등록 상태다.
CREATE TABLE user_totp (
    user_id TEXT PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    -- secret은 봉인된(AES-GCM) base32 공유 비밀이다.
    --
    -- DB 접속 자격증명과 같은 방식으로 다룬다. 비밀번호처럼 해시할 수는 없다 —
    -- 코드를 검증하려면 원문이 필요하기 때문이다. 그래서 최소한 마스터 키 없이는
    -- 쓸 수 없게 만든다: 메타 DB만 새어 나가도 남의 코드를 만들어 낼 수 있으면 안 된다.
    secret       TEXT NOT NULL,
    digits       INTEGER NOT NULL DEFAULT 6,
    period       INTEGER NOT NULL DEFAULT 30,
    -- skew_seconds는 이 사용자에게만 적용되는 시각 보정값이다.
    --
    -- 전역 보정값(app_settings의 clock.offset_seconds)으로 메우지 못한 나머지가 여기 남는다.
    -- 사용자마다 따로 두는 이유: 휴대폰 하나의 시각이 어긋난 것과 서버 시계가 어긋난
    -- 것은 다른 문제이고, 전자를 전역에 반영하면 나머지 사람들의 로그인이 깨진다.
    skew_seconds INTEGER NOT NULL DEFAULT 0,
    -- last_step은 마지막으로 인증에 성공한 시간 스텝이다.
    --
    -- 이 값이 재사용을 막는다. TOTP 코드는 30초 동안 유효하므로, 어깨너머로 보거나
    -- 중간에서 가로챈 코드를 그 안에 다시 쓸 수 있다. 같거나 더 이른 스텝을 거부하면
    -- 한 코드는 한 번만 쓰인다.
    last_step    INTEGER NOT NULL DEFAULT 0,
    created_at   TEXT NOT NULL,
    -- confirmed_at이 NULL이면 아직 등록을 마치지 않은 상태다(비밀만 만들어 둔 상태).
    -- 이 구분이 없으면 QR을 띄워만 놓고 앱에 등록하지 않은 사람이 로그인하지 못한다.
    confirmed_at TEXT,
    last_used_at TEXT,
    -- failures/locked_until은 코드 무차별 대입을 막는다. 6자리는 100만 가지뿐이라
    -- 시도 횟수를 제한하지 않으면 창을 열어 두는 것과 같다.
    failures     INTEGER NOT NULL DEFAULT 0,
    locked_until TEXT
);

-- user_totp_recovery는 인증 앱을 잃었을 때 쓰는 일회용 복구 코드다.
--
-- 왜 필요한가: 슈퍼 어드민이 2FA를 의무화한 상태에서 휴대폰을 잃으면 그 계정은
-- 잠긴다. 다른 슈퍼 어드민이 초기화해 줄 수 있지만, 슈퍼 어드민이 한 명뿐인 설치에서는
-- 그 길도 없다. 복구 코드는 그 막다른 길을 없애기 위한 것이다.
--
-- 저장하는 것은 해시뿐이다(세션·API 토큰과 같은 규칙).
CREATE TABLE user_totp_recovery (
    id         TEXT PRIMARY KEY,
    user_id    TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    code_hash  TEXT NOT NULL,
    created_at TEXT NOT NULL,
    used_at    TEXT,
    used_ip    TEXT NOT NULL DEFAULT ''
);

CREATE INDEX idx_totp_recovery_user ON user_totp_recovery (user_id);
CREATE UNIQUE INDEX idx_totp_recovery_hash ON user_totp_recovery (code_hash);

-- totp_challenges는 "비밀번호는 맞았지만 아직 코드를 안 낸" 중간 상태다.
--
-- 이 상태를 서버에 두는 것이 핵심이다. 클라이언트에게 "이 사용자는 1단계를 통과했다"는
-- 표시를 들려 보내면, 그 표시가 곧 우회 수단이 된다. 세션 테이블을 재활용하지 않는
-- 이유도 같다 — 세션 행이 하나라도 "아직 인증 전"인 상태로 존재하면, 세션을 읽는
-- 모든 코드가 그 예외를 알고 있어야 한다. 하나라도 모르면 그것이 구멍이다.
CREATE TABLE totp_challenges (
    -- id는 챌린지 토큰의 SHA-256이다. 원문은 쿠키로만 오간다.
    id         TEXT PRIMARY KEY,
    user_id    TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_at TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    attempts   INTEGER NOT NULL DEFAULT 0,
    ip         TEXT NOT NULL DEFAULT '',
    user_agent TEXT NOT NULL DEFAULT ''
);

CREATE INDEX idx_totp_challenges_expires ON totp_challenges (expires_at);
