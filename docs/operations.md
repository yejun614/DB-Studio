# 운영

릴리스를 만들고, 로그를 읽고, 키와 백업을 관리하는 방법.

> [문서 색인](README.md) · [프로젝트 README](../README.md)

## 차례

- [릴리스 빌드](#릴리스-빌드)
- [CI와 릴리스 자동화 (GitHub Actions)](#ci와-릴리스-자동화-github-actions)
- [도커 이미지](#도커-이미지)
- [로그 — 서버가 멈췄을 때 먼저 볼 곳](#로그--서버가-멈췄을-때-먼저-볼-곳)
- [백업 훅](#백업-훅)
- [마스터 암호화 키](#마스터-암호화-키)

## 릴리스 빌드

```bash
scripts/release.sh v1.0.0        # 버전을 생략하면 git describe → dev
```

```powershell
.\scripts\release.ps1 -Version v1.0.0
```

`dist/`에 6개 플랫폼(linux amd64·arm64, darwin amd64·arm64, windows amd64·arm64) 바이너리와
`SHA256SUMS`가 만들어진다. `-trimpath -ldflags "-s -w"`로 빌드해 개발 경로가 남지 않고
크기도 줄어든다(약 50~53MB). 버전은 바이너리에 심어지므로 실행 없이도 확인할 수 있다.

빌드 시각을 심기 때문에 같은 소스라도 실행마다 해시가 달라진다. 해시를 고정해야 한다면
`SOURCE_DATE_EPOCH`로 시각을 지정한다(`SOURCE_DATE_EPOCH=1700000000 scripts/release.sh v1.0.0`).

```bash
./dbstudio -version                       # dbstudio v1.0.0 linux/amd64 go1.26.5
curl -s localhost:8080/api/v1/health      # 로그인 없이 빌드 정보 확인 (배포 검증용)
```

## CI와 릴리스 자동화 (GitHub Actions)

`.github/workflows/`에 둘이 있다.

| 워크플로 | 언제 | 하는 일 |
|---|---|---|
| `ci.yml` | main 푸시 · PR | gofmt 확인, `go vet`, `go test ./...`(우분투·윈도우), 6개 플랫폼 크로스 빌드 |
| `release.yml` | `v*` 태그 푸시 | 시험 → `scripts/release.sh` 빌드 → 릴리스 노트 작성 → GitHub 릴리스 생성 |

```bash
git tag v1.0.0 && git push origin v1.0.0
```

- **빌드 명령은 `scripts/release.sh` 하나뿐이다.** 워크플로가 `go build`를 다시 적으면
  ldflags가 두 곳에 생기고, 손으로 만든 빌드와 CI 빌드가 조용히 달라진다.
- 릴리스 빌드는 태그가 가리키는 커밋 시각을 `SOURCE_DATE_EPOCH`로 심는다. 같은 태그를
  다시 빌드하면 바이너리 해시가 같다.
- 하이픈이 있는 태그(`v1.0.0-rc.1`)는 사전 배포로 올라간다.
- **시험이 실패하면 릴리스를 만들지 않는다.** 태그를 되돌리는 것보다 거기서 멈추는 편이 싸다.
- CI가 윈도우에서도 시험을 돌리는 이유: 호스트 감시와 OS 로그 읽기는 플랫폼마다 구현이
  다르다(`internal/hostmon/*_windows.go`). 한쪽에서만 돌리면 다른 쪽 코드는 아무도 실행하지 않는다.
- 크로스 빌드를 따로 두는 이유: OS별로 갈라지는 파일이 있어 리눅스 시험이 통과해도 macOS
  빌드가 깨질 수 있고, 그 사실은 릴리스를 만드는 순간에야 드러난다.
- 통합 시험(실제 DB 접속)은 `-integration` 플래그나 `DBSTUDIO_INTEGRATION`이 있을 때만 도므로
  CI에서는 자동으로 건너뛴다. 돌리려면 `docker/compose.test.yaml`을 먼저 띄워야 한다.

## 도커 이미지

태그를 밀면 릴리스 워크플로가 두 변종을 GHCR에 올린다. 둘 다 linux/amd64 · arm64다.

| 변종 | 태그 | 바닥 | 언제 |
|---|---|---|---|
| distroless (기본) | `:v1.0.0` · `:1.0.0` · `:1.0` · `:latest` | `distroless/static` | 대부분의 경우 |
| alpine | `:v1.0.0-alpine` · `:1.0.0-alpine` · `:1.0-alpine` · `:alpine` | `alpine:3.22` | 셸이 필요할 때 |

```bash
docker run -d --name dbstudio -p 8080:8080 \
  -v dbstudio-data:/data \
  -e DBSTUDIO_MASTER_KEY="$(openssl rand -base64 32)" \
  ghcr.io/yejun614/db-studio:latest
docker logs dbstudio | head -20   # 슈퍼 어드민 계정과 임시 비밀번호가 한 번 나온다
```

**`/data`는 반드시 볼륨으로 빼야 한다.** 메타 DB(`dbstudio.db`)와 마스터 암호화 키
(`master.key`)가 거기 생긴다. 컨테이너를 새로 만들 때 볼륨을 놓치면 키가 사라지고,
그러면 저장된 DB 자격증명을 복호화할 수 없다. 위 예시처럼 `DBSTUDIO_MASTER_KEY`로
키를 주입하면 키를 이미지·볼륨 밖(시크릿 관리)에 둘 수 있고, 클러스터의 모든 노드가
같은 키를 쓰는 것도 그 방법으로 한다.

두 변종은 **같은 uid(65532)** 로 돌므로 볼륨을 그대로 주고받을 수 있다. 바인드 마운트를
쓸 때는 호스트 디렉터리를 `chown 65532` 해야 한다.

**어떤 변종을 쓰나.** distroless에는 셸이 없다 — 공격 면이 좁은 대신 매크로의
Lua `sh.run`(`-allow-shell`)과 `docker exec ... sh`를 쓸 수 없다. 그 둘이 필요하면
`-alpine` 태그를 쓴다.

**이미지 속 바이너리는 릴리스 자산과 같은 바이트다.** Dockerfile이 이미지 안에서 다시
빌드하지 않고 `scripts/release.sh`가 만든 파일을 그대로 복사하므로, 릴리스에 함께 올라간
`SHA256SUMS`로 이미지 내용을 검증할 수 있다.

사내 CA로 발급한 GitLab·Ceph·RabbitMQ를 쓴다면 그 인증서를 신뢰시켜야 한다
(공개 CA 묶음만으로는 검증되지 않는다). alpine 변종에 도구가 들어 있다.

```bash
docker run -v ./our-ca.crt:/usr/local/share/ca-certificates/our-ca.crt:ro \
  --user 0 --entrypoint sh ghcr.io/yejun614/db-studio:alpine \
  -c 'update-ca-certificates && exec /dbstudio -data /data'
```

**설정은 전부 환경변수로 준다.** 플래그 40개에 모두 `DBSTUDIO_*` 대응이 있고
(목록은 [시작하기의 옵션 표](getting-started.md#옵션)), 이미지는 플래그를 인자로 넘기지
않으므로 환경변수가 그대로 먹는다.

예시 파일이 저장소에 있다 — [`docker/compose.yaml`](../docker/compose.yaml).

```bash
echo "DBSTUDIO_MASTER_KEY=$(openssl rand -base64 32)" > docker/.env
docker compose -f docker/compose.yaml up -d
docker compose -f docker/compose.yaml logs | head -20   # 첫 계정과 임시 비밀번호
```

키를 환경변수로 주면 볼륨에 `master.key` 파일을 만들지 않는다 — 키와 데이터가 한곳에
있지 않게 된다. 그 예시 파일은 `read_only`·`cap_drop: ALL`·`no-new-privileges` 까지
켜 둔 상태로 동작을 확인한 것이다(쓰는 곳은 `/data` 와 tmpfs `/tmp` 뿐이다).

예시는 `dbstudio` 라는 이름의 브리지 네트워크를 만든다. 리버스 프록시를 앞에 둘 때는
`ports` 를 지우고 프록시를 같은 네트워크에 붙이면 호스트 포트를 열지 않고 `dbstudio:8080`
으로 닿는다(예시 파일에 주석으로 켜는 자리를 만들어 두었다). **네트워크를
`internal: true` 로 두면 안 된다** — 밖으로 내는 HTTPS(AI 제공자·웹훅·Git 호스트·
클러스터의 다른 노드)가 막히는데, DB 접속은 멀쩡해서 원인을 찾기 어렵다.

우선순위는 `인자 > 환경변수 > 기본값`이다. 이미지에는 `DBSTUDIO_ADDR=:8080`·
`DBSTUDIO_DATA=/data` 두 개만 기본값으로 심어 두었고, 인자를 주면 그것이 이긴다.

```bash
docker run ... ghcr.io/yejun614/db-studio:latest -addr :9000   # 인자로도 된다
```

손으로 만들 때는 `dist/`를 먼저 채워야 한다 — 이미지가 그 파일을 복사한다.

```bash
scripts/release.sh v1.0.0
docker build -f docker/Dockerfile --build-arg VERSION=v1.0.0 -t db-studio .
```

## 로그 — 서버가 멈췄을 때 먼저 볼 곳

로그는 stderr와 **파일에 동시에** 기록된다. 터미널을 닫거나 서비스로 띄우면 stderr는
사라지므로, 파일이 없으면 "왜 멈췄는지"를 확인할 방법이 없어진다.

| 파일 | 내용 |
|---|---|
| `<data>/dbstudio.log` | 앱 로그. 20MB를 넘으면 `.1`로 밀려나고 새로 시작한다 |
| `<data>/dbstudio.log.1` | 직전 로그 한 개만 보관한다 |
| `<data>/dbstudio.log.crash` | Go 런타임 크래시 리포트. 여기 내용이 있으면 패닉으로 죽은 것이다 |
| `<data>/dbstudio.running` | 실행 중 표식. 1분마다 갱신하고 정상 종료 시 지운다 |

**종료 원인 읽는 법.** 정상 종료는 세 줄을 남긴다.

```
level=INFO msg="종료 신호 수신" signal=interrupt note="터미널을 닫거나 Ctrl+C를 누르면 이 신호가 옵니다"
level=INFO msg="정리 시작" trigger=interrupt timeout=10s
level=INFO msg="정리 완료"
level=INFO msg="프로세스 종료" pid=49420 uptime=3h12m0s
```

| 로그의 마지막 모습 | 의미 |
|---|---|
| `종료 신호 수신` → `프로세스 종료` | 정상 종료. **터미널을 닫거나 Ctrl+C를 눌렀을 때도 이 경로다** |
| `서버가 오류로 종료합니다` + `hint=...` | 시작 실패(포트 충돌, 키 문제, 데이터 디렉터리 권한). hint가 다음 할 일을 알려준다 |
| `프로세스 종료` 줄이 아예 없음 | 강제 종료(`kill -9`, 작업 관리자, 다른 프로세스의 종료 명령, 전원 차단) 또는 런타임 크래시 |
| `.crash` 파일에 스택 | 패닉으로 죽음. 스택의 첫 줄이 원인이다 |
| `패닉을 복구했습니다` | 백그라운드 작업 하나만 중단됐고 서버는 살아 있다 |

**강제 종료는 다음 시작 때 스스로 알려준다.** 종료 기록을 남길 기회조차 없었던 실행은
`dbstudio.running` 표식이 남으므로, 다음 시작이 그것을 읽어 보고한다.

```
level=WARN msg="이전 실행이 종료 기록을 남기지 않았습니다"
  이전실행="pid=58548 version=v1.0.0 addr=:18008 시작=…T02:05:36+09:00 마지막 생존=…T02:24:12+09:00"
  원인="강제 종료로 보입니다 (작업 관리자·kill -9·다른 프로세스의 종료 명령·전원 차단). 크래시 리포트는 비어 있습니다"
```

표식을 1분마다 갱신하기 때문에 **"몇 시까지 살아 있었는지"** 를 말할 수 있다. 이것이 없으면
DB 파일의 수정 시각 같은 간접 증거를 뒤져야 한다. 표식의 주인이 아직 살아 있으면
(같은 데이터 디렉터리를 두 인스턴스가 쓰는 경우) 크래시로 오해하지 않고 그 사실을 알려주며,
살아 있는 쪽의 표식을 건드리지 않는다.

같은 로그 파일에 여러 인스턴스가 쓸 수 있으므로 모든 줄에 `pid`가 있는 시작·종료 줄을
기준으로 구분한다. 포트 충돌로 죽은 인스턴스가 있으면 그 사실이 그대로 남는다.

**요청 로그.** 기본값에서는 문제 있는 요청만 남긴다 — 5xx 응답(`요청 실패`)과
5초를 넘긴 요청(`느린 요청`). 정적 자산과 폴링까지 남기면 정작 필요한 줄을 찾을 수 없다.
전부 보려면 `-log-level debug`로 올린다.

```bash
./dbstudio -log-level debug              # 모든 API 요청 기록
./dbstudio -log-format json              # 수집기(Loki, CloudWatch 등)에 넣을 때
./dbstudio -log-file /var/log/dbstudio.log
./dbstudio -log-file ""                  # 파일 없이 stderr만 (컨테이너에서 유용)
```

## 백업 훅

운영 DB에 마이그레이션을 적용하기 전에 실행할 외부 명령을 지정한다. 실패하면 마이그레이션을
중단한다.

```bash
./bin/dbstudio -backup-cmd "/opt/scripts/backup.sh {kind} {host} {port} {database} {id}"
```

인자에 쓸 수 있는 치환값: `{name}` `{kind}` `{host}` `{port}` `{database}` `{env}` `{id}`.

앱이 DB별 백업을 직접 구현하지 않는 이유는 도구와 정책이 조직마다 다르고(pg_dump, mysqldump,
볼륨 스냅샷) 잘못 만든 백업은 없는 것보다 위험하기 때문이다. 명령은 **실행 플래그로만**
지정할 수 있다 — API로 지정하게 하면 이 앱이 임의 명령 실행 통로가 된다. 셸을 거치지 않고
실행하므로 커넥션 이름 같은 사용자 지정 값이 명령으로 해석되지 않는다. 자격증명은 넘기지
않으며, 백업 도구의 인증은 환경변수나 설정 파일로 준비해야 한다.

## 마스터 암호화 키

DB 접속 비밀번호는 AES-256-GCM으로 암호화되어 저장된다. 키는 `DBSTUDIO_MASTER_KEY`(base64 32바이트)를
쓰거나, 없으면 `<data>/master.key`에 자동 생성된다.

**이 키를 잃으면 저장된 모든 DB 자격증명을 복호화할 수 없다.** 운영 환경에서는 환경변수로 주입하고
시크릿 관리 시스템에 보관할 것.
