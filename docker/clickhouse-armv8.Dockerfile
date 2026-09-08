# ARMv8.0 CPU 용 ClickHouse. 바꾸는 것은 **바이너리 하나뿐**이다.
#
# 메모리 설정은 여기 없다. 그것은 기계가 작다는 이야기이고 CPU 와 상관이 없어서
# compose.test.small.yaml 로 갈라 두었다. 섞어 두면 파이 5 처럼 CPU 는 되지만
# 여전히 작은 기계로 옮길 때, 이 파일을 빼는 순간 아직 필요한 메모리 설정까지
# 함께 사라진다.
#
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
