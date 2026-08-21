-- P14: 매크로 자동 실행 — 정기 실행(스케줄)과 조건 실행(이벤트).
--
-- 두 가지를 한 테이블에 두는 이유: 둘 다 "무엇이 매크로를 자동으로 시작하는가"라는
-- 같은 질문의 답이고, 목록·활성화·소유자·실행 이력이 모두 같다. 나누면 화면도 API도
-- 두 벌이 되는데 정작 다른 것은 조건 필드 몇 개뿐이다.
--
-- **소유자(owner_id)의 권한으로 실행된다.** 이것이 이 기능의 핵심 결정이다.
-- 서비스 계정을 만들어 그 권한으로 돌리면 "툴은 호출자의 권한으로 실행된다"는
-- 앱 전체의 규칙이 무너진다. 대신 소유자의 계정이 비활성화되거나 권한이 회수되면
-- 그 트리거는 실행 시점에 실패하고 자동으로 꺼진다.
CREATE TABLE macro_triggers (
    id          TEXT PRIMARY KEY,
    macro_id    TEXT NOT NULL REFERENCES macros (id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    -- kind: schedule(정기) | event(모니터링 이벤트)
    kind        TEXT NOT NULL CHECK (kind IN ('schedule', 'event')),
    enabled     INTEGER NOT NULL DEFAULT 1,

    -- 실행 파라미터. 매크로가 정의한 파라미터에 넘길 값이다.
    params      TEXT NOT NULL DEFAULT '{}',

    -- ---- kind='schedule' ----
    -- cron 식(분 시 일 월 요일)과 시간대. 시간대가 비면 서버 지역 시간을 쓴다.
    cron        TEXT NOT NULL DEFAULT '',
    timezone    TEXT NOT NULL DEFAULT '',
    -- next_run_at은 다음 예정 시각이다. 저장해 두는 이유는 두 가지다:
    -- 재시작해도 자리를 잃지 않고, 화면이 "다음 실행"을 보여줄 수 있다.
    next_run_at TEXT,

    -- ---- kind='event' ----
    -- 비어 있는 조건은 "전부"를 뜻한다.
    event_kind      TEXT NOT NULL DEFAULT '',  -- threshold | connectivity | drift | collect_error
    event_severity  TEXT NOT NULL DEFAULT '',  -- 이 심각도 이상만 (info < warning < critical)
    event_metric    TEXT NOT NULL DEFAULT '',
    connection_id   TEXT REFERENCES connections (id) ON DELETE CASCADE,
    -- min_interval_sec는 같은 트리거가 연달아 터지는 것을 막는다.
    -- 지표가 임계치 근처에서 흔들리면 이벤트가 반복 생성되는데, 그때마다 매크로를
    -- 시작하면 매크로가 문제를 키운다.
    min_interval_sec INTEGER NOT NULL DEFAULT 300,

    -- ---- 공통 ----
    -- skip_if_running이면 같은 매크로가 실행 중일 때 건너뛴다.
    -- 기본값이 켜짐인 이유: 5분마다 도는 매크로가 6분 걸리기 시작하면 실행이 겹쳐
    -- 쌓이고, 대개 그것이 장애의 시작이다.
    skip_if_running INTEGER NOT NULL DEFAULT 1,

    owner_id    TEXT REFERENCES users (id) ON DELETE CASCADE,
    owner_name  TEXT NOT NULL DEFAULT '',

    -- 마지막 결과. 목록에서 "이 트리거가 잘 돌고 있는가"를 바로 보여준다.
    last_fired_at TEXT,
    last_run_id   TEXT,
    last_status   TEXT NOT NULL DEFAULT '',
    last_error    TEXT NOT NULL DEFAULT '',
    -- fail_count는 연속 실패 횟수다. 일정 횟수를 넘으면 스스로 꺼진다 —
    -- 영원히 실패하는 트리거는 로그만 채우고 아무도 보지 않는다.
    fail_count    INTEGER NOT NULL DEFAULT 0,

    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL
);

CREATE INDEX idx_triggers_macro ON macro_triggers (macro_id);
CREATE INDEX idx_triggers_due ON macro_triggers (kind, enabled, next_run_at);
CREATE INDEX idx_triggers_event ON macro_triggers (kind, enabled, connection_id);
