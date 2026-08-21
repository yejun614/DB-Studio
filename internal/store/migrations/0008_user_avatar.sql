-- 프로필 아이콘. 값은 아이콘 키 문자열이며 빈 문자열은 "아이콘 없음"(이니셜 표시)이다.
-- 이미지를 저장하지 않는 이유는 internal/model/avatar.go에 적어 두었다.
ALTER TABLE users ADD COLUMN avatar TEXT NOT NULL DEFAULT '';
