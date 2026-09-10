#!/usr/bin/env bash
# 构建 fork 发布产物（fork 私有脚本，上游没有）。形态与上游 goreleaser 的 linux 包一致：
#
#   dist/sub2api_linux_arm64                    未压缩二进制（部署时直接上传这个）
#   dist/sub2api_linux_amd64
#   dist/sub2api_<版本>_linux_arm64.tar.gz      根目录：sub2api、LICENSE、README*、PATCH-NOTES.md、deploy/
#   dist/sub2api_<版本>_linux_amd64.tar.gz
#   dist/checksums.txt                          两个 tar.gz 的 sha256（与上游同名同格式）
#
# 二进制本体约 107MB，和上游一样；GitHub 上看到的 30 多 MB 是 tar.gz 压缩后的大小。
#
# 用法：ours/build-release.sh [输出目录]
set -euo pipefail

REPO_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
OUT_DIR="${1:-$REPO_DIR/dist}"
ARCHES=(arm64 amd64)

cd "$REPO_DIR"
command -v go >/dev/null 2>&1 || { echo "缺少 go（需要 1.27+）" >&2; exit 1; }
command -v pnpm >/dev/null 2>&1 || { echo "缺少 pnpm（CI 用 pnpm 9）" >&2; exit 1; }

VERSION="$(backend/scripts/resolve-version.sh)"
COMMIT="$(git rev-parse --short HEAD)"
DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
echo "==> version=$VERSION commit=$COMMIT"

# 前端：vite 的 outDir 直接指向 backend/internal/web/dist，-tags=embed 时编进二进制。
echo "==> 构建前端"
pnpm --dir frontend install --frozen-lockfile
pnpm --dir frontend run build
test -f backend/internal/web/dist/index.html || { echo "前端产物缺失" >&2; exit 1; }

mkdir -p "$OUT_DIR"
rm -f "$OUT_DIR"/sub2api_*_linux_*.tar.gz "$OUT_DIR/checksums.txt"

# macOS 的 bsdtar 默认会把扩展属性打进包里，Linux 解包会出现 ._ 文件，关掉。
TAR_EXTRA=()
if tar --help 2>&1 | grep -q -- '--no-mac-metadata'; then
  TAR_EXTRA+=(--no-mac-metadata)
fi
export COPYFILE_DISABLE=1

for arch in "${ARCHES[@]}"; do
  echo "==> 交叉编译 linux/$arch"
  bin="$OUT_DIR/sub2api_linux_$arch"
  (
    cd backend
    CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build \
      -tags=embed -trimpath \
      -ldflags "-s -w -X main.Version=${VERSION} -X main.Commit=${COMMIT} -X main.Date=${DATE} -X main.BuildType=release" \
      -o "$bin" ./cmd/server
  )

  stage="$(mktemp -d)"
  cp "$bin" "$stage/sub2api"
  cp LICENSE* README* PATCH-NOTES.md "$stage/" 2>/dev/null || true
  cp -R deploy "$stage/deploy"
  tarball="$OUT_DIR/sub2api_${VERSION}_linux_${arch}.tar.gz"
  tar "${TAR_EXTRA[@]}" -czf "$tarball" -C "$stage" .
  rm -rf "$stage"
done

(
  cd "$OUT_DIR"
  if command -v shasum >/dev/null 2>&1; then
    shasum -a 256 sub2api_*_linux_*.tar.gz > checksums.txt
  else
    sha256sum sub2api_*_linux_*.tar.gz > checksums.txt
  fi
)

echo
echo "==> 产物"
ls -lh "$OUT_DIR"/sub2api_linux_* "$OUT_DIR"/sub2api_*_linux_*.tar.gz "$OUT_DIR/checksums.txt"
echo
echo "==> 未压缩二进制 sha256"
for arch in "${ARCHES[@]}"; do
  if command -v shasum >/dev/null 2>&1; then shasum -a 256 "$OUT_DIR/sub2api_linux_$arch"; else sha256sum "$OUT_DIR/sub2api_linux_$arch"; fi
done
echo "==> checksums.txt"
cat "$OUT_DIR/checksums.txt"
