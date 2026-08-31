# 시작하기

빌드하고 띄우고 처음 로그인하기까지.

> [문서 색인](README.md) · [프로젝트 README](../README.md)

## 차례

- [빌드 & 실행](#빌드--실행)
- [옵션](#옵션)
- [로그인](#로그인)
- [2단계 인증 (TOTP)](#2단계-인증-totp)

## 빌드 & 실행

CGO가 필요 없으므로 크로스 컴파일이 그대로 동작한다.

```bash
CGO_ENABLED=0 go build -o bin/dbstudio ./cmd/dbstudio
./bin/dbstudio -addr :8080 -data ./data
```

Windows:

```powershell
$env:CGO_ENABLED="0"; go build -o bin/dbstudio.exe ./cmd/dbstudio
.\bin\dbstudio.exe -addr :8080 -data .\data
```

최초 실행 시 슈퍼 어드민 계정과 랜덤 비밀번호가 터미널에 출력된다. 이 값은 다시 표시되지 않는다.

> 참고: Windows 콘솔이 UTF-8이 아니면 한글 출력이 깨질 수 있다. `chcp 65001` 후 실행하거나
> Windows Terminal을 사용하면 정상 표시된다.

## 옵션

| 플래그 | 환경변수 | 기본값 | 설명 |
|---|---|---|---|
| `-addr` | `DBSTUDIO_ADDR` | `:8080` | 리스닝 주소 |
| `-data` | `DBSTUDIO_DATA` | `./data` | 메타 DB와 마스터 키 저장 경로 |
| `-dev` | `DBSTUDIO_DEV` | `false` | 프론트엔드를 디스크에서 서빙 (새로고침만으로 반영, 캐시 안 함) |
| `-web` | `DBSTUDIO_WEB` | `./web` | dev 모드에서 사용할 프론트엔드 경로 |
| `-session-ttl` | `DBSTUDIO_SESSION_TTL` | `12h` | 세션 유효기간 |
| `-secure-cookie` | `DBSTUDIO_SECURE_COOKIE` | `false` | HTTPS 배포 시 Secure 쿠키 |
| `-trust-proxy` | `DBSTUDIO_TRUST_PROXY` | `false` | `X-Forwarded-For`를 클라이언트 IP로 사용 |
| `-trusted-proxies` | `DBSTUDIO_TRUSTED_PROXIES` | 루프백 + 사설망 | 위 헤더를 신뢰할 상대(IP/CIDR, 콤마 구분) |
| `-monitor` | `DBSTUDIO_MONITOR` | `true` | 지표 폴링 활성화 |
| `-monitor-interval` | `DBSTUDIO_MONITOR_INTERVAL` | `30s` | 지표 수집 주기 |
| `-schema-check-interval` | `DBSTUDIO_SCHEMA_CHECK_INTERVAL` | `15m` | 스키마 드리프트 확인 주기 |
| `-metric-retention` | `DBSTUDIO_METRIC_RETENTION` | `48h` | 원본 지표 보존기간 |
| `-host-monitor` | `DBSTUDIO_HOST_MONITOR` | `true` | DB Studio가 도는 컴퓨터 자신의 감시 (CPU·메모리·디스크·네트워크) |
| `-host-interval` | `DBSTUDIO_HOST_INTERVAL` | `30s` | 호스트 지표 수집 주기 |
| `-host-syslog` | `DBSTUDIO_HOST_SYSLOG` | 자동 | 감시할 시스템 로그 (리눅스: 파일 경로, 윈도우: 이벤트 로그 채널) |
| `-log-level` | `DBSTUDIO_LOG_LEVEL` | `info` | `debug`로 올리면 모든 API 요청을 기록 |
| `-log-format` | `DBSTUDIO_LOG_FORMAT` | `text` | `json`으로 바꾸면 수집기에 넣기 쉬움 |
| `-log-file` | `DBSTUDIO_LOG_FILE` | `-` | `-`는 `<data>/dbstudio.log`, 빈 값이면 stderr만 |
| `-log-max-mb` | `DBSTUDIO_LOG_MAX_MB` | `20` | 이 크기를 넘으면 `.1`로 밀어내고 새로 시작 |
| `-backup-cmd` | `DBSTUDIO_BACKUP_CMD` | (없음) | 운영 DB 마이그레이션 전 실행할 외부 명령 |
| `-allow-shell` | `DBSTUDIO_ALLOW_SHELL` | `false` | 매크로의 셸 노드 활성화 (사용자에게 `script.run` 권한도 있어야 한다) |
| `-shell-timeout` | `DBSTUDIO_SHELL_TIMEOUT` | `2m` | 셸 노드 하나의 실행 시간 상한 |
| `-macro-timeout` | `DBSTUDIO_MACRO_TIMEOUT` | `15m` | 매크로 한 번의 실행 시간 상한 |
| `-macro-lua-timeout` | `DBSTUDIO_MACRO_LUA_TIMEOUT` | `1m` | Lua 노드 하나의 실행 시간 상한 (무한 루프 방어) |
| `-macro-http-timeout` | `DBSTUDIO_MACRO_HTTP_TIMEOUT` | `30s` | 매크로가 부르는 외부 HTTP 요청 하나의 상한 |
| `-macro-http-max-kb` | `DBSTUDIO_MACRO_HTTP_MAX_KB` | `1024` | 외부 응답 본문의 상한. 넘으면 자르고 `truncated`로 알린다 |
| `-macro-http-allow` | `DBSTUDIO_MACRO_HTTP_ALLOW` | (없음) | 비우면 링크로컬 외 전부 허용. 지정하면 **그 목록만** 허용 (호스트 이름·`.하위도메인`·IP·CIDR, 쉼표 구분) |
| `-backup-dir` | `DBSTUDIO_BACKUP_DIR` | `<data>/backups` | 논리 덤프 파일을 두는 디렉터리 |
| `-backup-max-mb` | `DBSTUDIO_BACKUP_MAX_MB` | `2048` | 이 크기를 넘는 덤프는 **실패로 끝낸다** (압축 전 기준) |
| `-backup-retention` | `DBSTUDIO_BACKUP_RETENTION` | `720h` (30일) | 이보다 오래된 백업을 자동 삭제. `0`이면 보관 |
| `-avatar-max-kb` | `DBSTUDIO_AVATAR_MAX_KB` | `512` | 프로필 이미지 최대 크기 |
| `-avatar-allow-private-uri` | `DBSTUDIO_AVATAR_ALLOW_PRIVATE_URI` | `false` | 사설망·루프백 주소에서 프로필 이미지 가져오기 허용 |
| `-cluster-role` | `DBSTUDIO_CLUSTER_ROLE` | `standalone` | `standalone` / `master` / `replica` |
| `-cluster-master` | `DBSTUDIO_CLUSTER_MASTER` | — | 리플리카가 부를 마스터 주소 |
| `-cluster-node-name` | `DBSTUDIO_CLUSTER_NODE_NAME` | 호스트 이름 | 화면에 보일 노드 이름 |
| `-cluster-advertise` | `DBSTUDIO_CLUSTER_ADVERTISE` | — | 다른 노드가 이 노드를 부를 주소 |
| `-cluster-sync-interval` | `DBSTUDIO_CLUSTER_SYNC_INTERVAL` | `2s` | 변경을 받아 오는 주기 |
| `-cluster-heartbeat` | `DBSTUDIO_CLUSTER_HEARTBEAT` | `10s` | 살아 있음을 알리는 주기 |
| `-cluster-log-keep` | `DBSTUDIO_CLUSTER_LOG_KEEP` | `24h` | 마스터가 복제 로그를 남기는 기간 |
| `-cluster-log-max` | `DBSTUDIO_CLUSTER_LOG_MAX` | `200000` | 복제 로그 최대 줄 수 |
| — | `DBSTUDIO_CLUSTER_SECRET` | — | 노드 사이 공용 비밀. **환경변수로만** 받는다 |
| — | `DBSTUDIO_MASTER_KEY` | 없으면 파일 생성 | 자격증명 암호화 키(base64 32바이트). 클러스터는 모든 노드가 같은 값을 써야 한다 |

**플래그와 환경변수는 같은 것을 가리킨다.** 환경변수는 그 플래그의 *기본값*이 되고,
인자로 준 플래그가 그것을 덮는다: `인자 > 환경변수 > 기본값`. 그래서 컨테이너·systemd
배치에서는 환경변수만으로 전부 설정할 수 있다(→ [운영 문서의 도커 이미지](operations.md#도커-이미지)).

비밀 두 개(`DBSTUDIO_CLUSTER_SECRET`·`DBSTUDIO_MASTER_KEY`)는 플래그가 없다. 명령줄
인자는 프로세스 목록에 그대로 보이고, 그 값 하나로 클러스터의 모든 데이터를 받아 가거나
저장된 자격증명을 복호화할 수 있기 때문이다.
| — | `DBSTUDIO_MASTER_KEY` | (자동 생성) | base64 32바이트 암호화 키 |

> **`-allow-shell`은 의식적으로 켜는 스위치다.** 이 플래그가 없으면 사용자에게 어떤 권한을 줘도
> 셸 노드는 실행되지 않는다. 권한 설정은 화면에서 몇 번의 클릭으로 바뀌지만, 이 기능이 켜지는
> 순간 앱은 원격 셸이 된다 — 그런 성격의 변경은 프로세스를 띄우는 사람이 정해야 한다.
>
> 도커로 띄운다면 **`-alpine` 태그**를 써야 한다. 기본 이미지(distroless)에는 셸이 아예 없어서
> 플래그만 켜도 실행 시점에 실패한다. alpine 변종에는 `bash` 가 함께 들어 있으므로
> `DBSTUDIO_ALLOW_SHELL=true` 하나만 주면 된다.

## 로그인

- **아이디 저장**: 체크해 두면 다음 로그인 때 아이디가 채워지고 커서가 비밀번호 칸에서
  시작한다. **비밀번호는 저장하지 않는다** — 그 일은 브라우저의 비밀번호 관리자가
  안전하게 하도록 만들어진 물건이고, 앱이 흉내 내면 저장소를 읽을 수 있는 누구에게나
  자격증명을 넘겨주는 셈이 된다. 저장 위치는 이 브라우저(localStorage)이며,
  체크를 끄고 로그인하면 지워진다
- 저장은 **로그인에 성공한 뒤**에 한다. 오타를 기억해 두면 다음에도 그 오타로 시작한다

## 2단계 인증 (TOTP)

비밀번호 하나가 새면 이 앱이 열어 주는 모든 것 — 운영 DB의 데이터, 마이그레이션 실행,
백업 복구 — 이 함께 샌다. 그래서 두 번째 자물쇠를 붙였다. SMS나 이메일이 아니라 TOTP인
이유는 이 앱이 **외부망 없이 도는 곳에도 설치되기 때문**이다. 나가는 통신 없이 동작하는
두 번째 요소는 사실상 이것뿐이다.

### 쓰는 법

- **개인**: 프로필 화면 → `2단계 인증 켜기` → QR을 인증 앱으로 찍고 코드 6자리 입력.
  등록을 마치면 **복구 코드 10개**가 한 번만 표시된다.
  Google Authenticator, Microsoft Authenticator, 1Password, Authy 등 표준 TOTP 앱이면 모두 동작한다.
- **슈퍼 어드민**: 보안 설정 화면 → `모든 사용자에게 의무화`.
  기본값은 **자율**이며, 켜면 미등록 사용자는 로그인 후 등록 화면에서 멈춘다(API 토큰도 함께 막힌다).
  의무화하려면 **본인이 먼저 켜 두어야 한다** — 되돌릴 수 있는 사람이 스스로 잠기는 상태를 막기 위해서다.
- **인증 앱을 잃었을 때**: 로그인 2단계 화면에서 `복구 코드 사용`.
  복구 코드도 없으면 슈퍼 어드민이 사용자 화면에서 초기화해 준다(감사 로그에 남는다).

### 설계

- **1단계에서 세션을 만들지 않는다.** 비밀번호가 맞으면 세션 대신 5분짜리 챌린지를 발급하고
  HttpOnly 쿠키로 내려보낸다. "비밀번호는 통과했다"는 상태를 세션으로 표현하면 세션을 읽는
  모든 코드가 그 예외를 알아야 하고, 하나라도 모르면 그것이 우회로가 된다.
- **공유 비밀은 봉인해서 저장한다**(DB 자격증명과 같은 AES-GCM). 코드 검증에 원문이 필요하므로
  해시할 수는 없지만, 메타 DB만 새어 나가서는 남의 코드를 만들 수 없어야 한다.
- **한 코드는 한 번만 쓰인다.** 마지막으로 성공한 시간 스텝을 기록해 같거나 이른 코드를 거부한다.
- 실패가 누적되면 계정 단위로 5분 잠긴다. 챌린지를 새로 만들어 시도 횟수를 초기화하는 우회도 막힌다.
- 복구 코드는 해시만 저장하고, 소비는 `UPDATE … WHERE used_at IS NULL` 한 문장으로 처리한다
  (조회 후 갱신으로 나누면 같은 코드로 두 세션을 얻을 수 있다).

### 서버 시계가 틀려도 동작한다

TOTP는 서버와 인증 앱이 같은 시각을 본다는 가정 위에 있는데, 이 앱이 놓이는 자리(사내망 서버,
노트북, 컨테이너)의 시계는 자주 틀린다. NTP가 막혀 있거나, VM이 정지·재개되며 몇 분씩 밀리거나,
누군가 시각을 손으로 맞춰 놓는다. 그 상태에서 TOTP를 붙이면 증상은 "인증 앱이 고장 났다"로
나타나고 사용자는 원인을 알 방법이 없다. 그래서 **DB Studio는 자체 시계를 관리한다**
(`internal/clock`).

1. **단조 시계에 고정한다.** 시작할 때 벽시계를 한 번 읽어 기준으로 삼고, 그 뒤로는 경과 시간을
   단조 시계로 잰다. 실행 중에 시스템 시각이 튀어도(NTP 도약, 수동 변경, VM 재개) 내부 시각은
   그만큼 튀지 않는다. `time.Now()`를 그대로 쓰면 그 순간 모든 사용자의 코드가 한꺼번에 틀린다.
2. **인증 앱에게서 배운다.** 시각이 맞는 장치와 대조하는 것 말고는 우리 시계가 틀렸음을 알 방법이
   없고, 사용자의 휴대폰이 바로 그 장치다. 등록할 때는 ±15분을 훑어 몇 칸 어긋났는지를 재고,
   그 값을 그 사용자의 보정값으로 확정한다. 외부 시각 서버에 나가지 않고 시계를 맞추는 셈이다.
3. **전역 보정값을 학습한다.** 관측을 지수 평활로 조금씩 반영해, 다음 사용자는 처음부터 맞은
   자리에서 등록을 시작한다. 한 사람의 틀린 휴대폰이 전체를 끌고 가지 않도록 가중치로 눌린다.
   이 값은 재시작 후에도 이어진다.
4. **이미 등록한 사람은 흔들리지 않는다.** 사용자별 보정값은 전역 보정값이 아니라 움직이지 않는
   기준 시각에 매달려 있다. 남이 로그인했다는 이유로 내 코드가 틀리게 되면 안 된다.
5. **시계가 크게 어긋나면 스스로 재동기화한다.** 로그인 검증은 ±30초만 받아들이지만, 거기서
   실패한 코드를 ±15분 범위로 한 번 더 훑어본다. 거기서 맞으면 **로그인은 거절하되 보정값만
   고치고** "다음 코드를 입력하세요"라고 안내한다. 그 코드로 통과시켜 주면 인증에 쓰이는 창이
   몇십 분으로 넓어져, 어깨너머로 본 코드가 그동안 살아 있게 되기 때문이다.

보안 설정 화면에서 내부 시각·시스템 시각·학습된 보정값·이 브라우저와의 차이를 한 번에 볼 수 있다.
"왜 코드가 안 맞는가"에 답하는 화면이다.
