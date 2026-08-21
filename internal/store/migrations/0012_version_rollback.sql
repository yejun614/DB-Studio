-- P12: 스키마 버전 롤백.
--
-- "v3으로 되돌린다"는 요청은 결국 **현재 구조에서 v3 구조로 가는 마이그레이션**이다.
-- 그래서 새 실행 경로를 만들지 않고 기존 마이그레이션 워크플로(리뷰·승인·프리체크·
-- 문장별 기록·롤백)를 그대로 탄다. 별도 경로를 만들면 "승인 없이 구조를 바꾸는 길"이
-- 하나 더 생기고, 그것이 이 앱이 막으려는 바로 그 상황이다.
--
-- 이 열은 그 마이그레이션이 롤백이라는 표시다. 적용이 끝나면 등록되는 스키마 버전의
-- source가 'migration'이 아니라 'rollback'이 된다 — 이력에서 "되돌린 지점"이
-- 보여야 나중에 무슨 일이 있었는지 읽을 수 있다.
ALTER TABLE migrations ADD COLUMN rollback_to_version INTEGER
    REFERENCES schema_versions (id) ON DELETE SET NULL;
