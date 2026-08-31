-- P36: 용어의 분류(대·중·소).
--
-- 사전이 커지면 "회원 쪽 용어만 보고 싶다"가 된다. 그때 이름으로만 찾게 하면 이미
-- 무슨 말이 있는지 아는 사람만 찾을 수 있는데, 사전이 필요한 사람은 대개 그것을
-- 모르는 사람이다.
--
-- 셋 다 비워 둘 수 있다. 처음부터 분류 체계를 세우고 시작하는 팀은 없다 — 쓰다
-- 보면 덩어리가 보이고, 그때 붙인다. 필수로 만들면 아무 말이나 넣게 되고, 그렇게
-- 들어간 분류는 없느니만 못하다.
--
-- 분류를 따로 표로 두지 않는 이유: 분류는 이름 그 자체이고 다른 속성이 없다. 표로
-- 두면 "쓰이지 않는 분류"를 누가 치울 것인가가 새 문제로 생기는데, 그 문제는 지금
-- 있지도 않은 문제다. 목록은 실제로 쓰인 값에서 모은다.

ALTER TABLE glossary_terms ADD COLUMN cat1 TEXT NOT NULL DEFAULT '';
ALTER TABLE glossary_terms ADD COLUMN cat2 TEXT NOT NULL DEFAULT '';
ALTER TABLE glossary_terms ADD COLUMN cat3 TEXT NOT NULL DEFAULT '';

CREATE INDEX idx_glossary_cat ON glossary_terms (cat1, cat2, cat3);
