#!/usr/bin/env bash
# 构建带内嵌前端的 linux/arm64 静态二进制（fork 私有脚本，上游没有）。
#
# 用法：
#   ours/build-linux-arm64.sh [输出路径]
#
# 默认输出：dist/sub2api_linux_arm64
# 结束时打印产物路径与 sha256。
set -euo pipefail

REPO_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
OUT="${1:-$REPO_DIR/dist/sub2api_linux_arm64}"

cd "$REPO_DIR"

command -v go >/dev/null 2>&1 || { echo "缺少 go，请先把 Go 1.27+ 放进 PATH" >&2; exit 1; }
command -v pnpm >/dev/null 2>&1 || { echo "缺少 pnpm（CI 用 pnpm 9），请先安装" >&2; exit 1; }

VERSION="$(backend/scripts/resolve-version.sh)"
COMMIT="$(git rev-parse --short HEAD)"
DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

echo "==> version=$VERSION commit=$COMMIT"

# 1) 前端：vite 的 outDir 直接指向 backend/internal/web/dist，构建完就位。
echo "==> 构建前端"
pnpm --dir frontend install --frozen-lockfile
pnpm --dir frontend run build

test -f backend/internal/web/dist/index.html || {
  echo "前端产物缺失：backend/internal/web/dist/index.html" >&2
  exit 1
}

# 2) 后端：-tags=embed 才会把 dist 编进二进制；CGO_ENABLED=0 出静态产物。
echo "==> 交叉编译 linux/arm64"
mkdir -p "$(dirname -- "$OUT")"
cd backend
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build \
  -tags=embed \
  -trimpath \
  -ldflags "-s -w -X main.Version=${VERSION} -X main.Commit=${COMMIT} -X main.Date=${DATE} -X main.BuildType=release" \
  -o "$OUT" \
  ./cmd/server
cd "$REPO_DIR"

echo
echo "==> 产物"
ls -lh "$OUT"
file "$OUT" 2>/dev/null || true
if command -v shasum >/dev/null 2>&1; then
  shasum -a 256 "$OUT"
else
  sha256sum "$OUT"
fi
