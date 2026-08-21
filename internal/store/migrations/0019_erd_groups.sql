-- ERD 캔버스 그룹(테이블 묶음을 감싸는 반투명 사각형).
--
-- 메모(notes_json)와 나란히 두는 이유: 둘 다 구조가 아니라 설명이고, 스냅샷을 압축할
-- 때 함께 저장되어야 한다. 칸을 만들지 않으면 op 로그가 잘리는 순간(압축) 그룹이
-- 통째로 사라지고, 그때 사용자는 자기가 그린 것이 왜 없어졌는지 알 수 없다.
ALTER TABLE erd_documents ADD COLUMN groups_json TEXT NOT NULL DEFAULT '[]';
