-- ERD 초안의 도메인(재사용 타입) 정의.
--
-- 스키마 스냅샷(snapshot_json)이 아니라 별도 칼럼에 두는 이유는 메모·그룹과 같다:
-- 도메인은 설계의 어휘이지 대상 DB에 만들어지는 구조가 아니다. 스냅샷에 섞으면
-- 그 지문이 바뀌어 "도메인을 정리했을 뿐인데 대상 DB와 구조가 다르다"가 된다.
ALTER TABLE erd_documents ADD COLUMN domains_json TEXT NOT NULL DEFAULT '[]';
