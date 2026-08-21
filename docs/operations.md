# 운영

릴리스를 만들고, 로그를 읽고, 키와 백업을 관리하는 방법.

> [문서 색인](README.md) · [프로젝트 README](../README.md)

## 차례

- [릴리스 빌드](#릴리스-빌드)
- [CI와 릴리스 자동화 (GitHub Actions)](#ci와-릴리스-자동화-github-actions)
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

`dist/`에 5개 플랫폼(linux amd64·arm64, darwin amd64·arm64, windows amd64) 바이너리와
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
| `ci.yml` | main 푸시 · PR | gofmt 확인, `go vet`, `go test ./...`(우분투·윈도우), 5개 플랫폼 크로스 빌드 |
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
