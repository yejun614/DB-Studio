# 배포 이미지 (distroless) — 기본 변종.
#
# 빌드를 이미지 안에서 하지 않고 `scripts/release.sh`가 만든 바이너리를 그대로
# 복사한다. 이유는 두 가지다.
#
#   1. 빌드 플래그(ldflags로 심는 버전·커밋·시각)의 출처가 하나로 남는다.
#      Dockerfile에 go build를 다시 적으면 릴리스 자산과 이미지가 조용히 달라진다.
#   2. 이미지 안의 바이너리가 릴리스 자산과 **같은 바이트**가 된다 —
#      릴리스에 함께 올라가는 SHA256SUMS로 이미지 내용을 검증할 수 있다.
#
# 그래서 이 Dockerfile은 dist/ 가 채워져 있어야 빌드된다:
#   scripts/release.sh v1.0.0
#   docker build -f docker/Dockerfile --build-arg VERSION=v1.0.0 -t db-studio .
#
# 베이스를 distroless로 잡은 이유: 이 앱은 밖으로 HTTPS를 낸다(AI 제공자,
# Slack·Mattermost 웹훅, Git 호스트, Ceph·WebHDFS·RabbitMQ 관리 API, 노드 사이
# 통신). 서버 인증서를 검증하려면 **루트 CA 묶음**이 있어야 하므로 아무것도 없는
# scratch는 쓸 수 없다 — 모든 HTTPS 호출이
# "x509: certificate signed by unknown authority" 로 실패한다. distroless/static은
# 그 묶음(/etc/ssl/certs/ca-certificates.crt)과 nonroot 계정만 담은 700KB 바닥이다.
# 셸이 없으므로 매크로의 sh.run(-allow-shell)은 이 변종에서 동작하지 않는다 —
# 그것이 필요하면 alpine 변종을 쓴다(Dockerfile.alpine).

# 데이터 디렉터리를 미리 만들어 소유자를 맞춘다.
#
# 이 단계를 BUILDPLATFORM에 고정하는 이유: 빈 디렉터리는 아키텍처와 무관하므로
# arm64 이미지를 만들 때도 에뮬레이션 없이 끝난다.
FROM --platform=$BUILDPLATFORM alpine:3.22 AS prep
RUN mkdir -p /seed/data && chown -R 65532:65532 /seed/data

FROM gcr.io/distroless/static-debian12:nonroot

# TARGETARCH는 buildx가 채운다(amd64 · arm64). VERSION은 dist/ 파일 이름에 쓰인다.
ARG TARGETARCH
ARG VERSION=dev

COPY --from=prep --chown=65532:65532 /seed/data /data
COPY --chmod=755 dist/dbstudio_${VERSION}_linux_${TARGETARCH} /dbstudio

# nonroot = uid 65532. alpine 변종도 같은 uid를 쓰므로 두 변종이 같은 볼륨을 공유한다.
USER 65532:65532

# /data 에 메타 DB(dbstudio.db)와 마스터 암호화 키(master.key)가 생긴다.
# 볼륨으로 빼지 않으면 컨테이너를 새로 만들 때 키가 사라지고, 그러면 저장된 DB
# 자격증명을 복호화할 수 없다.
VOLUME ["/data"]
EXPOSE 8080

LABEL org.opencontainers.image.title="DB Studio" \
      org.opencontainers.image.description="여러 데이터베이스를 한 곳에서 관리하는 단일 바이너리 웹 앱" \
      org.opencontainers.image.source="https://github.com/yejun614/DB-Studio" \
      org.opencontainers.image.licenses="MIT"

ENTRYPOINT ["/dbstudio"]
CMD ["-addr", ":8080", "-data", "/data"]
