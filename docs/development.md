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
docker compose -f docker/compose.test.yaml up -d               # MySQL, PostgreSQL, MS-SQL, Mongo, Redis, ClickHouse, RabbitMQ, Kafka
docker compose -f docker/compose.test.yaml --profile oracle up -d oracle
docker compose -f docker/compose.test.yaml down -v             # 정리
```

| 대상 | 포트 | 계정 |
|---|---|---|
| MySQL | 13306 | root / rootpw123 |
| PostgreSQL | 15432 | postgres / rootpw123 |
| MS-SQL | 11433 | sa / RootPw123! |
| Oracle (프로파일) | 11521 | appuser / RootPw123 |
| MongoDB | 27018 | root / rootpw123 |
| Redis | 16379 | (비밀번호만) rootpw123 |
| ClickHouse | 19000 (네이티브) · 18123 (HTTP) | default / rootpw123 |
| RabbitMQ | 15673 (관리 API) · 15672 (AMQP) | admin / rootpw123 |
| Kafka | 19092 | (인증 없음) |

#### ClickHouse + Kafka 를 이어서 보기

둘은 서로 **이어져 있다**. 카프카의 `events` 토픽을 ClickHouse 가 소비해 표에 쌓는다.

```bash
docker compose -f docker/compose.test.yaml up -d clickhouse kafka kafka-seed
```

`kafka-seed` 는 토픽을 만들고 메시지 200개를 넣은 뒤 **끝나서 사라진다**(`ps` 에
Exited 로 보이는 것이 정상이다). 그 뒤에 보이는 것:

- **스키마 화면** — `events_queue`(Kafka 엔진), `events`(MergeTree), `events_mv`(구체화 뷰).
  ClickHouse 에서 큐를 읽는 방법이 표 하나가 아니라 이 셋이라는 것이 그대로 보인다.
- **데이터 화면** — `events` 200행, `page_views` 500행(카프카와 무관한 표도 하나 둔다.
  브로커를 안 띄웠을 때 빈 화면과 "못 읽었다"를 구분할 수 있어야 한다).
- **브로커 화면** — `events` 토픽과 `clickhouse-events` 컨슈머 그룹의 랙.

같은 흐름을 두 화면에서 보는 것이 요점이다. 메시지를 더 넣으면 두 화면의 숫자가
함께 움직인다:

```bash
docker compose -f docker/compose.test.yaml run --rm kafka-seed
```

정의는 `docker/clickhouse-init/01-kafka-pipeline.sql` 에 있고, 컨테이너가 **처음**
뜰 때만 실행된다. 고친 뒤에는 `down -v` 로 볼륨을 지워야 다시 돈다.

#### ClickHouse 가 뜨지 않을 때 — CPU 명령어

로그가 이렇게 끝나면 설정 문제가 아니다.

```
/entrypoint.sh: line 51: 23 Illegal instruction (core dumped) clickhouse
  extract-from-config --config-file ... --key='storage_configuration.disks.*.path'
```

`extract-from-config` 는 entrypoint 가 **가장 먼저** 부르는 것이다. 설정을 읽기도
전에 죽었다는 것은 **그 CPU 에 바이너리가 쓰는 명령어가 없다**는 뜻이다. 메시지에
설정 파일 이야기가 나와서 설정을 의심하게 되는데, 그쪽에는 아무 문제가 없다.

공식 빌드가 요구하는 것:

| 아키텍처 | 요구 | 못 미치는 예 |
|---|---|---|
| x86_64 | SSE 4.2 | 오래된 CPU, 그리고 **가상 머신의 CPU 모델**(Proxmox·QEMU 의 기본값 `kvm64`·`qemu64` 는 SSE4.2 를 노출하지 않는다) |
| aarch64 | ARMv8.2-A (LSE) | 라즈베리파이 4(Cortex-A72)·3(Cortex-A53) |

어느 쪽인지는 이렇게 본다.

```bash
uname -m
lscpu | grep -o -E 'sse4_2|popcnt' | sort -u   # x86_64 에서 비면 SSE4.2 가 없다
```

**가상 머신이면 CPU 모델을 고치는 것이 가장 깔끔하다** — Proxmox 는 프로세서
종류를 `host`(또는 `x86-64-v2`)로, libvirt 는 `<cpu mode='host-passthrough'/>` 로.
CPU 를 못 바꾸는 기계(라즈베리파이)에는 덮어쓰기 파일을 둔다.

```bash
docker compose -f docker/compose.test.yaml \
               -f docker/compose.test.armv8.yaml up -d --build clickhouse
```

ClickHouse 가 그런 CPU 용 호환 빌드를 따로 내므로, 공식 이미지에 그 바이너리를
갈아 끼운 이미지를 그 자리에서 만든다(`docker/clickhouse-armv8.Dockerfile`).
이 파일이 바꾸는 것은 **바이너리 하나뿐**이다 — 메모리 설정은 CPU 와 상관없는
이야기라 아래의 다른 파일로 갈라 두었다.

빌드할 때 `ClickHouse 가 이 CPU 에서 돕니다: …` 가 찍히면 그 지점을 넘긴 것이다.
안 찍히면 빌드가 그 자리에서 실패하므로, 컨테이너를 띄워 보고 나서 아는 일은 없다.

호환 빌드에는 **버전이 고정되지 않는다**는 제약이 있다. ClickHouse 가 버전별로
내지 않고 master 빌드 하나만 두기 때문에, 이미지의 24.8 과 정확히 같은 버전이
아니다. 시험용으로는 충분하지만 이 조합을 운영에 쓸 것이라면 알고 써야 한다.
x86_64 인데 SSE4.2 가 없는 기계라면 Dockerfile 의 URL 에서 `aarch64v80compat` 을
`amd64compat` 으로 바꾸면 같은 방법이 그대로 통한다.

#### 메모리가 적은 기계 — 그리고 설정을 손볼 때 걸리는 것 셋

ClickHouse 의 기본값은 서버를 통째로 쓰는 것을 전제한다. 같은 기계에서
DB Studio 도 돌면 둘 중 하나가 OOM 으로 죽는다 — `mark_cache_size` 의 기본값이
5GiB 로 파이의 전체 메모리보다 크고, 전체보다 큰 값을 상한으로 두는 것은 상한을
두지 않은 것과 같다.

```bash
docker compose -f docker/compose.test.yaml \
               -f docker/compose.test.small.yaml up -d clickhouse
```

**CPU 와 따로 둔 파일이다.** 파이 4 라면 위의 `armv8` 파일과 겹쳐 쓰고, 파이 5 나
작은 VM 이라면 이것만 쓴다. 섞어 두면 CPU 는 되지만 여전히 작은 기계로 옮길 때
`armv8` 을 빼는 순간 아직 필요한 메모리 설정까지 함께 사라진다.

**1. 디렉터리로 얹지 말 것 — 파일 하나씩 얹는다.** 두 곳 다 이미지가 넣어 둔
파일이 있고, 가리면 잃는 것이 있다. 둘 다 오류가 원인을 가리키지 않는다.

| 가려지는 파일 | 하는 일 | 가리면 |
|---|---|---|
| `config.d/docker_related_config.xml` | `listen_host` 를 `0.0.0.0`·`::` 로 (다른 컨테이너·호스트에서 붙게) | 서버가 자기 안에서만 듣는다. 증상은 `connection refused` 하나뿐이라 "설정을 얹은 것"과 이어지지 않는다 |
| `users.d/default-user.xml` | entrypoint 가 `CLICKHOUSE_PASSWORD` 로 만든다 — 계정·비밀번호·허용 대역 | **비밀번호가 사라진다** |

**2. 파일이 둘인 것도 같은 종류의 함정이다.** 서버 설정(캐시 크기, 전체 메모리
비율, 동시 질의 수)은 `config.d`, 프로필 설정(`max_memory_usage`·`max_threads` —
질의 하나의 상한)은 `users.d` 에서 읽는다. 프로필을 `config.d` 에 적으면 오류도
나지 않고 그냥 안 먹는다 — 재 보면 `max_memory_usage` 가 0(무제한)인 채다.

**3. 뒷일 스레드 수(`background_pool_size`)는 건드리지 않는다.** 낮추면 서버가 시작을
거부한다 — 병합 대기열에서 비워 둘 자리 수(`number_of_free_entries_in_pool_to_*`)
기본값이 풀 크기를 넘기기 때문이고, 그 값이 셋이라 하나를 맞추면 다음 것이 걸린다.
맞춰 봐도 얻는 것이 없다(스레드 풀은 할 일이 없으면 논다).

먹었는지는 두 표를 따로 봐야 한다.

```bash
CH="docker compose -f docker/compose.test.yaml exec -T clickhouse clickhouse-client"
$CH --password rootpw123 --query "SELECT name, value FROM system.server_settings WHERE name LIKE '%cache_size%'"
$CH --password rootpw123 --query "SELECT name, value FROM system.settings WHERE name IN ('max_memory_usage','max_threads')"
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
