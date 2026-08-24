<div align="center">

# DB Studio

**여러 데이터베이스를 한 곳에서.**
접근 권한을 통제하고, 상태를 지켜보고, 구조를 설계해 바꾸고, 그 과정을 기록으로 남깁니다.

[![CI](https://github.com/yejun614/DB-Studio/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/yejun614/DB-Studio/actions/workflows/ci.yml)
[![Release](https://github.com/yejun614/DB-Studio/actions/workflows/release.yml/badge.svg)](https://github.com/yejun614/DB-Studio/actions/workflows/release.yml)
[![Latest release](https://img.shields.io/github/v/release/yejun614/DB-Studio?display_name=tag&sort=semver&label=release)](https://github.com/yejun614/DB-Studio/releases/latest)

[![Go](https://img.shields.io/badge/Go-1.26-00ADD8?logo=go&logoColor=white)](go.mod)
[![License](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Single binary](https://img.shields.io/badge/배포-단일%20바이너리-success)](docs/operations.md)
[![Platforms](https://img.shields.io/badge/platform-linux%20%7C%20macOS%20%7C%20windows-lightgrey)](docs/operations.md)
[![No CGO](https://img.shields.io/badge/CGO-없음-informational)](docs/architecture.md)

</div>

---

DB Studio는 **프론트엔드까지 담은 하나의 실행 파일**입니다. 런타임 의존성도, 별도의 DB 서버도,
빌드 단계도 없습니다. 파일 하나를 옮겨 실행하면 그 자리에서 웹 앱이 뜹니다.

```bash
./dbstudio -addr :8080 -data ./data
```

<div align="center">
  <img src="docs/images/dashboard.png" alt="대시보드 — 등록된 DB의 상태와 열린 이벤트" width="100%">
  <sub>대시보드는 "지금 문제가 있는가"에 먼저 답합니다 · <a href="docs/screenshots.md">화면 더 보기</a></sub>
</div>

## 바탕에 깔린 원칙 네 가지

이 넷을 알면 나머지 동작은 예측할 수 있습니다.

| # | 원칙 | 뜻 |
|---|---|---|
| 1 | **권한은 커넥션 단위로 판정한다** | "이 사람이 어드민인가"가 아니라 "이 사람이 이 DB에 무엇까지 할 수 있는가"를 묻습니다. 화면·REST·MCP·매크로·AI가 모두 같은 판정을 지납니다 |
| 2 | **되돌릴 수 없는 일에는 단계를 둔다** | 마이그레이션은 계획 → 리뷰 → 승인 → 실행. 데이터 수정은 모아 두었다가 실행될 SQL을 보고 적용. AI의 변경 제안은 사람의 승인 뒤에 실행 |
| 3 | **한 일은 남는다** | 누가·언제·무엇을·어떤 결과로 했는지 감사 로그에 남고, 지울 수 없습니다 |
| 4 | **초안과 실제는 다르다** | ERD에서 그린 것은 설계도이고, 실제 DB는 마이그레이션을 실행할 때만 바뀝니다 |

## 할 수 있는 일

| 영역 | 내용 |
|---|---|
| 🔐 **권한** | 역할(슈퍼 어드민·어드민·멤버) × 접근 범위 × 능력 등급, 데이터 능력은 별도 축. 서버 단위로 주고 DB 단위로 예외 — 좁은 쪽이 이깁니다 |
| 📊 **관측** | 지표 수집, 임계값 룰, 이벤트, 슬로우 쿼리, DB 서버 로그 탐색, **앱이 도는 컴퓨터**의 CPU·메모리·디스크·OS 로그 |
| 🗂 **데이터** | 표 브라우저(외래키 따라가기), SQL 콘솔(구문 검사·포맷·실행 계획), MongoDB·Redis 전용 화면 |
| 🧩 **설계** | ERD 동시 편집(커서·대화·되돌리기), 타입 고르개, 도메인, SQL 불러오기·내보내기 |
| 🚦 **변경** | 마이그레이션 계획·리뷰·승인·사전 검사·실행, 버전 이력과 롤백, 드리프트 감지 |
| 💾 **백업** | 논리 덤프(구조·데이터 선택, gzip), 다른 커넥션으로 복구, 보존 기간 정리 |
| ⚙️ **자동화** | 노드 그래프 매크로 + Lua 샌드박스, cron·이벤트 트리거, 소유자 권한으로 실행 |
| 🤖 **AI** | 어시스턴트(조회는 즉시, 변경은 승인 후), **MCP 서버**, **REST API** — 같은 툴·같은 권한 |
| 🔔 **알림** | 이벤트를 Mattermost·Slack으로. 해소 알림, 심각도별 색, 전송 상태 기록 |
| 🌐 **분산** | 여러 서버를 하나처럼(마스터-리플리카), 특정 노드에서만 닿는 DB에는 담당 노드 지정 |
| 📖 **설명서** | 앱 안에 들어 있습니다(`/manual`). 바이너리에 임베드되므로 화면과 버전이 갈리지 않습니다 |

## 화면

<table>
<tr>
<td width="50%"><a href="docs/images/erd.png"><img src="docs/images/erd.png" alt="ERD 설계"></a><br>
<b>ERD 설계</b> — 여러 명이 같은 초안을 동시에 고칩니다. 커서·대화·되돌리기가 있고,
그린 것은 마이그레이션으로만 실제 DB에 반영됩니다.</td>
<td width="50%"><a href="docs/images/cluster.png"><img src="docs/images/cluster.png" alt="클러스터"></a><br>
<b>클러스터</b> — 여러 서버를 하나처럼. 노드마다 복제 지연과 그 컴퓨터의
CPU·메모리·디스크를 함께 봅니다.</td>
</tr>
<tr>
<td width="50%"><a href="docs/images/data.png"><img src="docs/images/data.png" alt="데이터 브라우저"></a><br>
<b>데이터</b> — 외래키를 누르면 대상 행이 오른쪽에 펼쳐집니다. 수정은 모아 두었다가
실행될 SQL을 보고 적용합니다.</td>
<td width="50%"><a href="docs/images/sql-console.png"><img src="docs/images/sql-console.png" alt="SQL 콘솔"></a><br>
<b>SQL 콘솔</b> — 구문 검사(실행하지 않고 확인), 포맷팅, 실행 계획. 실행한 문장은
감사 로그에 남습니다.</td>
</tr>
</table>

<div align="center"><a href="docs/screenshots.md">모니터링 · 브로커 · 서버 컴퓨터 · 설명서 화면 보기 →</a></div>

## 다룰 수 있는 대상

**데이터베이스** — PostgreSQL · MySQL · MS-SQL · Oracle · SQLite · MongoDB · Redis
**분산 스토리지** — Hadoop(HDFS·YARN) · Ceph
**메시지 브로커** — RabbitMQ · Kafka

드라이버는 모두 순수 Go입니다. CGO를 쓰면 단일 바이너리 배포가 깨지기 때문입니다.

## 빠른 시작

<details open>
<summary><b>내려받아 실행</b></summary>

[릴리스](https://github.com/yejun614/DB-Studio/releases/latest)에서 플랫폼에 맞는 파일 하나를 받습니다(Linux · macOS · Windows / x86-64 · ARM64).

```bash
chmod +x dbstudio_*_linux_amd64
./dbstudio_*_linux_amd64 -addr :8080 -data ./data
```

</details>

<details>
<summary><b>도커로 실행</b></summary>

```bash
docker run -d -p 8080:8080 -v dbstudio-data:/data ghcr.io/yejun614/db-studio:latest
```

셸이 필요하면(매크로의 `sh.run`) `:alpine` 태그를 쓰세요. `/data`에 메타 DB와 마스터
암호화 키가 생기므로 볼륨을 반드시 빼야 합니다 →
[운영 문서](docs/operations.md#도커-이미지)

</details>

<details>
<summary><b>소스에서 빌드</b></summary>

```bash
git clone https://github.com/yejun614/DB-Studio.git && cd DB-Studio
CGO_ENABLED=0 go build -o bin/dbstudio ./cmd/dbstudio
./bin/dbstudio -addr :8080 -data ./data
```

Windows PowerShell:

```powershell
$env:CGO_ENABLED="0"; go build -o bin/dbstudio.exe ./cmd/dbstudio
.\bin\dbstudio.exe -addr :8080 -data .\data
```

</details>

첫 실행 때 **슈퍼 어드민 계정과 임시 비밀번호가 터미널에 한 번** 표시됩니다. 다시 표시되지
않으므로 그 자리에서 보관하세요. `http://localhost:8080` 으로 접속해 로그인하면 비밀번호
변경을 요구합니다.

> 데이터 디렉터리에 메타 DB(`dbstudio.db`)와 **마스터 암호화 키**(`master.key`)가 생깁니다.
> 키를 잃으면 저장된 DB 자격증명을 복호화할 수 없습니다 → [운영 문서](docs/operations.md#마스터-암호화-키)

## 문서

| 문서 | 내용 |
|---|---|
| [문서 색인](docs/README.md) | 아래 전부의 지도 |
| [화면](docs/screenshots.md) | 실제로 띄운 서버의 화면 12장 |
| [기능 목록](docs/features.md) | 구현되어 있는 것 전체 |
| [시작하기](docs/getting-started.md) | 빌드·실행, 옵션, 첫 로그인, 2단계 인증 |
| [운영](docs/operations.md) | 릴리스, CI, 로그 읽기, 백업 훅, 암호화 키 |
| [구조](docs/architecture.md) | 디렉터리와 패키지의 역할 |
| [설계 노트](docs/README.md#설계-노트) | **왜 그렇게 만들었는가** — 권한·스키마·화면·연동·자동화·분산 |
| [알려진 제약](docs/limitations.md) | 지원하지 않는 것과 그 이유 |
| [PLAN.md](docs/PLAN.md) | 단계별(P0~P25) 결정 기록 |

## 개발

```bash
go test ./...                                    # 통합 시험은 자동으로 건너뜁니다
docker compose -f docker/compose.test.yaml up -d # 실제 DB로 시험하려면
go test ./internal/dbx -run TestIntrospect -integration
```

- 프론트엔드는 **빌드 단계가 없습니다.** 브라우저가 그대로 읽는 ES 모듈이라 번들러도,
  `node_modules`도, 트랜스파일도 없습니다. 고치고 새로고침하면 끝입니다.
- 대신 컴파일러가 잡아 주던 것을 시험이 잡습니다(`assets_test.go` — 없는 함수 호출,
  쓰이지 않는 CSS 변수).
- `main` 푸시·PR에서 gofmt·vet·시험(우분투·윈도우)과 6개 플랫폼 크로스 빌드가 돕니다.
  `v*` 태그를 밀면 릴리스가 만들어집니다 → [운영 문서](docs/operations.md#ci와-릴리스-자동화-github-actions)

## 스택

| 영역 | 쓰는 것 |
|---|---|
| 백엔드 | Go 1.26 · [Fiber](https://gofiber.io) v2 · 순수 Go 드라이버만 |
| 메타 저장소 | SQLite(단일 파일) · 번호가 붙은 마이그레이션 |
| 프론트엔드 | 프레임워크 없음 — ES 모듈 · CSS 변수 테마 |
| 스크립트 | [GopherLua](https://github.com/yuin/gopher-lua) 샌드박스 |
| 배포 | 자산·글꼴까지 임베드한 단일 실행 파일 |

## 라이선스

이 프로젝트는 [MIT 라이선스](LICENSE)입니다.

함께 담긴 본문 글꼴 **Pretendard JP**는 SIL Open Font License 1.1을 따릅니다
(`web/fonts/pretendard-jp/OFL.txt`). CDN을 쓸 수 없는 사설망 배치를 위해 저장소에 포함했습니다.
