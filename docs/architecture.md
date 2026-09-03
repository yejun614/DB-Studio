# 구조

디렉터리 배치와 각 패키지의 역할.

> [문서 색인](README.md) · [프로젝트 README](../README.md)

```
cmd/dbstudio/        엔트리포인트, 부트스트랩, graceful shutdown
embed.go             web/ 정적 자산 embed (루트에 있어야 web/ 참조 가능)
internal/
  config/            플래그·환경변수 설정
  crypto/            argon2id 비밀번호 해싱, AES-GCM 시크릿 봉인, 랜덤 생성
  store/             메타 SQLite: 마이그레이션 러너 + 리포지토리
                     projects.go = 자원의 울타리(프로젝트 → 서버 → DB, ERD·용어도),
                     servers.go = DB 서버(접속 정보·자격증명), connections.go = 그 아래 DB
    migrations/      embed된 스키마 SQL
  model/             엔티티와 열거형 (역할, 접근 모드, 능력 등급, DB 종류)
                     perm.go = 데이터 능력·전역 권한 (등급과 독립적인 두 번째 축)
  auth/              세션 서비스 + RBAC 판정(Authorizer.Can / CanCap)
                     rbac.go의 resolveWithPolicy가 유일한 관문이다: 프로젝트 참여 →
                     접근 범위 → 등급·능력 순으로 좁힌다(HTTP·WS·AI 툴이 모두 통과)
                     totp.go = 2단계 인증(로그인 2단계·등록·복구 코드·시계 재동기화)
  totp/              RFC 4226/6238 구현과 QR 인코더 (외부 의존성 없음)
  clock/             앱이 스스로 관리하는 시계. 단조 시계에 고정하고 인증 앱에게서 보정값을 배운다
  api/               Fiber 라우팅, 미들웨어, 핸들러
                     ai_tools*.go = 툴 레지스트리(어시스턴트·MCP·REST API가 함께 쓴다),
                     tokenapi.go = 토큰 경로의 공통 관문(인증·툴 노출 판정·실행),
                     mcp_handlers.go / rest_handlers.go = 그 위에 프로토콜만 입힌 두 문
  schema/            스키마 IR(ir.go) + 타입 정규화(types.go) + diff(diff.go) + DDL 생성(ddl.go)
  metric/            지표 타입 전용 (dbx와 monitor의 순환 의존을 끊기 위해 분리)
  dblog/             로그 타입 + SQL 정규화/다이제스트 (같은 이유로 dbx와 분리)
  dbx/               대상 DB 어댑터 (DSN 생성, Ping, introspect_*.go, metrics_*.go, logs_*.go,
                     explore_*.go = Mongo/Redis 특화 조회,
                     data*.go = 값 조회/수정/문장 실행 + 방언 차이(data_dialect.go),
                     listdb.go = 서버의 DB 목록 조회(일괄 등록용))
  macro/             매크로 엔진: 그래프(graph.go), 실행기(engine.go), 내장 노드(nodes.go),
                     Lua 샌드박스와 호스트 API(lua.go), 셸 실행(shell.go), 외부 HTTP(http.go),
                     cron 해석(cron.go), 자동 실행 스케줄러와 권한 판정(trigger.go)
  backup/            논리 덤프 생성(dump*.go)과 복구(restore.go), 작업 관리(backup.go)
  monitor/           폴러(poller.go) + 룰 엔진(rules.go) + 드리프트 감지(drift.go)
  erd/               ERD 문서 모델 + 편집 op 정의/적용/검증 (전송·저장 계층을 모른다)
  erdhub/            문서별 실시간 편집 중개: seq 부여, 브로드캐스트, 프레즌스, 채팅
  migrate/           마이그레이션 사전 검사와 실행 (프리체크·백업 훅·문장별 기록·롤백)
  vcs/               GitHub/GitLab/Bitbucket 공통 인터페이스와 3사 REST 구현
  ai/                LLM 프로바이더 어댑터 (Anthropic 네이티브 / OpenAI 호환) + SSE 파서
  mcp/               MCP 프로토콜 타입과 판 협상 (앱을 모른다. 툴 실행은 api가 한다)
  applog/            로거 구성 (파일 기록·로테이션·크래시 리포트·패닉 복구)
  runstate/          실행 중 표식 (강제 종료를 다음 시작 때 감지)
  buildinfo/         빌드 시점에 주입되는 버전 정보
  bootstrap/         최초 슈퍼 어드민 생성
web/                 프론트엔드 (ES Module, 프레임워크 없음)
  favicon.svg        파비콘 (다크 모드 반전) · favicon.ico · apple-touch-icon.png
  fonts/pretendard-jp/  본문 폰트 Pretendard JP (OFL 1.1). CDN을 쓸 수 없어 함께 담는다
                     (CSP가 font-src 'self'). dynamic subset이라 브라우저는 쓰는
                     조각만 받는다 — 한국어 화면 기준 약 175KB (통짜 파일은 5.3MB)
  fonts/d2coding/    코드·ERD 도면 고정폭 D2Coding (OFL 1.1, NAVER). 같은 이유로 함께 담는다.
                     한글이 라틴 두 칸 폭이라 한글 섞인 코드·컬럼명에서도 세로줄이 맞는다 —
                     라틴 전용 고정폭에서는 한글만 대체 글꼴로 빠져 줄이 어긋난다.
                     subset(한글 2,350자 + ASCII) 두 굵기로 약 750KB (통짜는 3MB)
  js/theme-init.js   저장된 테마를 첫 페인트 전에 적용 (CSP 때문에 인라인 불가)
  js/core/           api, dom, ui, store, router, chart, dblogo, erdsocket, theme, avatars, highlight,
                     screen(보고 있는 화면 → AI 어시스턴트의 시스템 프롬프트)
  js/pages/          login, totp, security, dashboard, users, access, connections, schema, monitor,
                     rules, events, logs, erd, erdeditor, migrations, vcs, nosql,
                     data, sqlconsole, backups, macros, macroeditor, triggers, assistant, audit
scripts/             릴리스 빌드 (release.sh / release.ps1), 파비콘 생성 (gen-favicon)
.github/workflows/   CI (ci.yml) 와 릴리스 (release.yml)
docker/              테스트용 DB 컨테이너 정의
docs/PLAN.md         전체 설계 및 단계 계획
```
