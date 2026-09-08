# ARMv8.0 라즈베리파이용 ClickHouse. 공식 이미지에서 두 가지를 바꾼다.
#
# ── 1. 바이너리 ──────────────────────────────────────────────────────
# 공식 ARM 빌드는 ARMv8.2-A(LSE 원자 명령)를 요구한다. 파이 4 의 Cortex-A72 와
# 파이 3 의 Cortex-A53 은 ARMv8.0 이라 그 명령어가 없어서, 설정을 읽는 첫 줄에서
# SIGILL 로 죽는다:
#
#   /entrypoint.sh: line 51: 23 Illegal instruction (core dumped) clickhouse
#     extract-from-config --config-file ... --key='storage_configuration.disks.*.path'
#
# 이 메시지로는 원인을 짐작할 수 없다. 설정 파일 이야기가 나오니 설정을 의심하게
# 되는데, 정작 못 도는 것은 바이너리 자체다. ClickHouse 가 그런 CPU 용 호환
# 빌드를 따로 내므로 바이너리만 갈아 끼운다.
#
# 알아 둘 것: 호환 빌드는 **버전이 고정되지 않은 master 빌드**다. 아래 FROM 의
# 24.8 과 정확히 같은 버전이 아니고, 그것을 고를 방법도 없다(ClickHouse 가
# 버전별 호환 빌드를 내지 않는다). 시험용으로는 충분하지만 운영에 쓸 것이라면
# 그 사실을 알고 써야 한다.
#
# x86_64 인데 SSE4.2 가 없는 기계(가상 머신의 기본 CPU 모델이 흔하다)라면
# 아래 URL 의 aarch64v80compat 을 amd64compat 으로 바꾸면 그대로 통한다.
#
# ── 2. 메모리 설정 ───────────────────────────────────────────────────
# 파이에서는 DB Studio 가 같은 기계에서 돌지만 ClickHouse 의 기본값은 서버를
# 통째로 쓰는 것을 전제한다. 설정 파일은 **이미지 안에 넣는다** — compose 에서
# 디렉터리로 마운트하면 이미지가 config.d 에 넣어 둔
# docker_related_config.xml(모든 주소에서 듣기)이 가려져 밖에서 안 보인다.
# COPY 는 디렉터리를 합치므로 그 문제가 생기지 않는다.
FROM clickhouse/clickhouse-server:24.8

ADD --chmod=755 https://builds.clickhouse.com/master/aarch64v80compat/clickhouse \
    /usr/bin/clickhouse

# 이미지가 심볼릭 링크로 부르는 이름들을 새 바이너리로 다시 잇고, 이 CPU 에서
# 실제로 도는지 **빌드할 때** 확인한다. 컨테이너를 띄워 보고 아는 것보다 낫다.
RUN set -eux; \
    for name in clickhouse-server clickhouse-client clickhouse-local \
                clickhouse-extract-from-config clickhouse-benchmark; do \
      ln -sf /usr/bin/clickhouse "/usr/bin/$name"; \
    done; \
    clickhouse local --query "SELECT 'ClickHouse 가 이 CPU 에서 돕니다: ' || version()"

# 서버 설정은 config.d, 프로필 설정은 users.d 로 간다(각 파일 주석 참고).
COPY clickhouse-armv8/config.d/ /etc/clickhouse-server/config.d/
COPY clickhouse-armv8/users.d/  /etc/clickhouse-server/users.d/
