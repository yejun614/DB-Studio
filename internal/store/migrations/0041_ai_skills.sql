-- P41: 사용자가 만드는 AI 스킬.
--
-- 스킬은 미리 적어 둔 지시문이다. 앱이 들고 있는 다섯 개(ai_skills.go)로 시작했는데,
-- 실제로 반복되는 물음은 팀마다 다르다 — "이 표에 감사 컬럼 4종 붙여 줘", "이번 주
-- 마이그레이션 계획을 리뷰해 줘"처럼. 그 문단을 매번 다시 적으면 조금씩 달라지고,
-- 달라지면 답도 달라진다. 그래서 사람이 자기 스킬을 만들 수 있어야 한다.
--
-- 앱이 들고 있는 스킬은 이 표에 넣지 않는다. 그것들은 툴 이름을 부르는 글이라 툴이
-- 바뀌면 함께 바뀌어야 하고, DB에 복사해 두면 업그레이드한 설치에서 옛 글이 남는다.
-- 화면은 둘을 합쳐 보여주되 **앱의 것은 고칠 수 없다**(builtin 표시).
--
-- shared는 "이 스킬을 남도 쓸 수 있는가"다. 스킬은 팀의 일하는 방식이기도 해서,
-- 잘 만든 것 하나가 팀 전체의 물음을 고르게 만든다. 고치고 지우는 것은 만든 사람
-- (과 사용자 관리자)만 할 수 있다 — 남이 쓰고 있는 글을 아무나 바꿀 수 있으면
-- "어제 쓰던 스킬이 오늘 다른 말을 한다"가 된다.
--
-- args는 JSON 배열이다. 칸마다 열을 만들지 않는 이유: 이것은 화면이 입력칸을 그리는
-- 데만 쓰는 값이고, 종류가 늘어날 때마다 마이그레이션을 하나씩 더할 이유가 없다.
-- 검증은 서버가 저장 전에 한다(빈 열쇠, 모르는 종류, 지시문과 어긋나는 자리).

CREATE TABLE ai_skills (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    -- icon은 화면의 아이콘 이름이다(sparkles, edit, list …). 비면 기본값을 쓴다.
    icon        TEXT NOT NULL DEFAULT '',
    -- prompt는 대화에 들어갈 지시문이다. {{열쇠}} 자리에 사람이 고른 값이 들어간다.
    prompt      TEXT NOT NULL,
    -- args는 [{key,label,type,placeholder,default,optional,multiline}] 다.
    args        TEXT NOT NULL DEFAULT '[]',
    -- shared면 모두가 목록에서 본다. 고치고 지우는 것은 여전히 주인만 한다.
    shared      INTEGER NOT NULL DEFAULT 0,

    created_by  TEXT REFERENCES users (id) ON DELETE SET NULL,
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL
);

-- 목록은 "내 것 + 공유된 것"을 이름 순으로 읽는다.
CREATE INDEX idx_ai_skills_owner ON ai_skills (created_by);
CREATE INDEX idx_ai_skills_shared ON ai_skills (shared);
