-- P16: 구조 화면의 개인 배치.
--
-- 구조 화면은 "지금 이 DB가 어떻게 생겼는가"를 ERD로 보여준다. 스키마는 실제 DB에서
-- 읽으므로 저장할 것이 없다. 저장해야 하는 것은 **사람이 손으로 정리한 것**이다:
-- 카드를 어디에 놓았는지, 어디에 메모를 붙였는지, 무엇을 묶어 두었는지.
--
-- 왜 계정별인가: 이 배치는 읽는 사람의 관점이다. 결제를 보는 사람과 배송을 보는
-- 사람은 같은 스키마를 다르게 늘어놓아야 하고, 한 사람의 정리가 다른 사람의 정리를
-- 덮어쓰면 둘 다 쓸 수 없게 된다. 함께 그리는 그림은 ERD 설계 문서가 맡는다.
--
-- 왜 erd_documents를 재사용하지 않는가: 저 문서는 "만들고 싶은 것"이고 이것은
-- "있는 것"이다. 같은 표에 두면 목록에 초안 아닌 것이 섞이고, 마이그레이션 대상을
-- 고를 때 실제로는 적용할 수 없는 항목이 후보로 뜬다.
CREATE TABLE erd_structure_views (
    user_id       TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    connection_id TEXT NOT NULL REFERENCES connections (id) ON DELETE CASCADE,
    -- 좌표는 테이블 키 → {x, y, collapsed, color, icon} 맵이다.
    -- ERD 문서의 layout_json과 같은 형식이라 캔버스 코드가 그대로 쓰인다.
    layout_json   TEXT NOT NULL DEFAULT '{}',
    notes_json    TEXT NOT NULL DEFAULT '[]',
    groups_json   TEXT NOT NULL DEFAULT '[]',
    updated_at    TEXT NOT NULL,
    PRIMARY KEY (user_id, connection_id)
);
