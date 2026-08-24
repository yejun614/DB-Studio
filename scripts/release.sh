#!/usr/bin/env bash
# 크로스 컴파일 릴리스 빌드.
#
# 사용법: scripts/release.sh [버전]
#   버전을 주지 않으면 git describe, 그것도 없으면 dev 로 표기한다.
#
# CGO를 끄는 것이 이 프로젝트의 전제다(Pure Go 드라이버만 사용). 그래서
# 대상 플랫폼의 툴체인 없이 한 장비에서 모든 바이너리를 만들 수 있다.
set -euo pipefail

cd "$(dirname "$0")/.."

VERSION="${1:-}"
if [ -z "$VERSION" ]; then
  VERSION="$(git describe --tags --always --dirty 2>/dev/null || echo dev)"
fi
COMMIT="$(git rev-parse --short=12 HEAD 2>/dev/null || echo '')"
# 빌드 시각을 심으므로 같은 소스라도 빌드마다 바이너리 해시가 달라진다.
# 재현 가능한 빌드가 필요하면 SOURCE_DATE_EPOCH로 시각을 고정한다.
if [ -n "${SOURCE_DATE_EPOCH:-}" ]; then
  DATE="$(date -u -d "@$SOURCE_DATE_EPOCH" +%Y-%m-%dT%H:%M:%SZ)"
else
  DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
fi

PKG=dbstudio/internal/buildinfo
LDFLAGS="-s -w -X ${PKG}.Version=${VERSION} -X ${PKG}.Commit=${COMMIT} -X ${PKG}.Date=${DATE}"

OUT=dist
rm -rf "$OUT"
mkdir -p "$OUT"

# 대상 목록. 필요 없는 것은 지워도 되지만, 하나씩 빌드해도 1분 이내다.
#
# windows/arm64를 넣는 이유: Snapdragon X 계열 노트북과 ARM 윈도우 VM이
# 늘었다. x64 바이너리도 에뮬레이션으로 돌지만, 이 앱은 30초마다 호스트 지표를
# 읽고 폴러를 돌리는 상주 프로세스여서 에뮬레이션 비용을 계속 낸다.
TARGETS="
linux/amd64
linux/arm64
darwin/amd64
darwin/arm64
windows/amd64
windows/arm64
"

echo "dbstudio ${VERSION} (${COMMIT:-no-vcs}) 빌드"
for target in $TARGETS; do
  goos="${target%/*}"
  goarch="${target#*/}"
  name="dbstudio_${VERSION}_${goos}_${goarch}"
  bin="$OUT/$name"
  if [ "$goos" = "windows" ]; then
    bin="$bin.exe"
  fi
  echo "  → $goos/$goarch"
  CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" \
    go build -trimpath -ldflags "$LDFLAGS" -o "$bin" ./cmd/dbstudio
done

# 체크섬은 배포한 파일이 손상·교체되지 않았음을 받는 쪽이 확인할 수 있게 한다.
( cd "$OUT" && sha256sum ./* > SHA256SUMS )

echo
ls -lh "$OUT"
echo
echo "체크섬:"
cat "$OUT/SHA256SUMS"
