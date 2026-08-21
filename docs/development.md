# 개발

개발 환경, 시험, 자산 생성.

> [문서 색인](README.md) · [프로젝트 README](../README.md)

프론트엔드는 번들러나 빌드 스텝이 없다. 네이티브 ES Module과 직접 작성한 CSS를 `go:embed`로 포함한다.

```bash
go run ./cmd/dbstudio -dev          # web/ 을 디스크에서 서빙
go test ./...                       # 단위 테스트 (DB 컨테이너 불필요)
```

단위 테스트는 `store`(시간 형식·이벤트 중복 억제·롤업 멱등성·ERD 스냅샷 재생),
`monitor`(룰 판정·지속 시간 게이트·카운터 변화율), `dblog`(SQL 정규화·다이제스트 안정성),
`erd`(op 패치 의미·식별자 검증·참조 무결성), `erdhub`(동시 편집의 seq 유일성·거부 격리·
재전송 멱등성·프레즌스), `migrate`(상태 전이 규칙·승인 수 규칙),
`vcs`(3사 API의 경로·헤더·본문 형식과 호출 순서)를 다룬다.

`erdhub` 테스트는 WebSocket 없이 허브를 직접 구동하므로 동시성 문제를 브라우저 없이
재현할 수 있다. `vcs` 테스트는 `httptest`로 각 서비스를 흉내내 "내가 이해한 API 계약"을
코드로 고정한다 — 실제 서비스와의 차이는 문서를 다시 읽어 그 테스트를 고치는 방식으로
반영한다.

### 파비콘 재생성

아이콘은 생성물이다. 모양을 바꿀 때는 `scripts/gen-favicon`의 기하 상수를 고치고 다시 만든다.

```bash
go run ./scripts/gen-favicon     # web/favicon.svg, favicon.ico, apple-touch-icon.png
```

SVG와 래스터를 각각 손으로 만들면 도형이 어긋나고 한쪽만 고치는 일이 반드시 생기므로,
기하 정의 한 곳에서 두 형식을 함께 뽑는다. 외부 도구(ImageMagick 등)는 쓰지 않는다 —
표준 라이브러리만으로 빌드되는 전제를 아이콘 하나 때문에 깨뜨릴 이유가 없다.

### 테스트용 DB 컨테이너

```bash
docker compose -f docker/compose.test.yaml up -d               # MySQL, PostgreSQL, MS-SQL, Mongo, Redis
docker compose -f docker/compose.test.yaml --profile oracle up -d oracle
docker compose -f docker/compose.test.yaml down -v             # 정리
```

### 통합 테스트

컨테이너가 떠 있으면 introspect 정확성과 DDL 왕복을 실제 DB에 대해 검증한다.
컨테이너가 없으면 각 케이스는 스킵된다.

```bash
go test ./internal/dbx/ -integration -v
DBSTUDIO_INTEGRATION=1 go test ./...    # 전체 (환경변수로도 켤 수 있다)
```

검증 내용:
- **구조 정확성** — PK/FK/인덱스/체크제약/주석/타입 정규화를 알려진 스키마와 대조
- **읽기 안정성** — 같은 DB를 두 번 읽은 결과의 diff가 빈 집합이어야 한다.
  이게 깨지면 변경이 없는데도 마이그레이션이 생성되어 앱을 신뢰할 수 없다.
- **변경 감지** — 인위적 변경을 넣고 의도한 변경만 정확히 잡히는지
- **DDL 왕복** — 생성한 DDL을 빈 스키마에 실제로 실행하고 다시 읽어 원본과 비교.
  diff가 비어야 생성기가 원본 구조를 재현했다고 말할 수 있다.
- **드리프트 감지** — 드라이버로 직접 DDL을 실행해 앱 외부 변경을 재현하고,
  기준선 저장 → 무변경 무시 → 추가 변경(경고) → 파괴적 변경(심각 승격)을 확인
- **마이그레이션 왕복** — 실제 DB에 적용 → 구조 확인 → 롤백 → 지문이 원본과 일치.
  더불어 프리체크의 드리프트 차단, 승인 게이트, dialect별 실패 동작(트랜잭션 DB는 전부
  되돌아가고 비트랜잭션 DB는 부분 적용이 기록됨)을 검증한다.
- **MongoDB·Redis 특화 조회** — 컬렉션 저장 통계, 인덱스 속성(unique/sparse/TTL)과 크기,
  복합 인덱스의 키 순서·방향, 뷰 구분, 필드 존재 비율, Redis INFO 파싱과 접두사 그룹의
  타입·TTL 집계, 큰 키 정렬

각 테스트는 전용 데이터베이스를 만들어 쓴다. 스키마 지문은 DB 내 모든 테이블을
포함하므로, DB를 공유하면 한 테스트의 테이블 생성이 다른 테스트에게 "외부 변경"으로
보이고, `go test`가 패키지를 병렬 실행하기 때문에 결과가 실행 조합에 따라 달라진다.

| DB | 포트 | 계정 |
|---|---|---|
| MySQL 8.4 | 13306 | `root` / `rootpw123`, db `appdb` |
| PostgreSQL 17 | 15432 | `postgres` / `rootpw123`, db `appdb` (sslmode=disable) |
| MS-SQL 2022 | 11433 | `sa` / `RootPw123!`, db `master` |
| Oracle Free 23 | 11521 | `appuser` / `RootPw123`, service `FREEPDB1` |
| MongoDB 8 | 27018 | `root` / `rootpw123`, db `appdb` (auth_source=admin) |
| Redis 7 | 16379 | 비밀번호 `rootpw123`, db `0` |
