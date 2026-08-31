-- P35: 용어 사전.
--
-- 논리명과 물리명을 팀이 같은 규칙으로 쓰기 위한 표다. "회원 번호"를 누구는
-- member_no 로, 누구는 mbr_num 으로 적으면 같은 것이 두 이름을 갖는다. 그 뒤로는
-- 어느 쪽이 맞는지 아무도 확신하지 못하고, 조인 한 번에 두 이름을 다 외워야 한다.
--
-- 문서(ERD)마다 두지 않고 앱 전체에 하나만 두는 이유: 이것은 그림 하나의 사정이
-- 아니라 팀의 약속이다. 문서마다 사전이 따로 있으면 문서마다 다른 약속이 생기는데,
-- 그러면 사전이 있는 것이 없는 것보다 나쁘다.
--
-- 도메인(타입 재사용)과 다른 층이다. 도메인은 "이 컬럼은 어떤 타입인가"이고,
-- 용어는 "이것을 뭐라고 부르기로 했는가"다.

CREATE TABLE glossary_terms (
    id       TEXT PRIMARY KEY,
    -- term은 사람이 쓰는 말이다("회원", "주문 일시").
    term     TEXT NOT NULL,
    -- physical은 DB에 적을 이름이다("member", "order_dttm").
    physical TEXT NOT NULL,
    -- note는 언제 이 말을 쓰는지다. 뜻이 겹치는 말이 있을 때 그 자리를 가른다
    -- ("주문 일시는 결제 완료 시각이 아니다").
    note     TEXT NOT NULL DEFAULT '',

    created_by TEXT REFERENCES users (id) ON DELETE SET NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

-- 같은 말이 두 번 오르면 사전이 아니다. 대소문자는 구분하지 않는다 — "회원"과
-- "회원"은 같고, "Member"와 "member"도 사전에서는 같은 항목이어야 한다.
CREATE UNIQUE INDEX idx_glossary_term ON glossary_terms (term COLLATE NOCASE);

-- 물리명은 유일하지 않아도 된다. 뜻이 다른 두 말이 같은 약어를 쓰는 일은 실제로
-- 있고(그 자체가 팀이 정할 문제다), 여기서 막으면 사전에 적지 못한 채로 쓰게 된다.
-- 대신 화면이 겹친다는 사실을 보여 준다.
CREATE INDEX idx_glossary_physical ON glossary_terms (physical COLLATE NOCASE);
