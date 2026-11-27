#!/usr/bin/env bash
# 构建管理服务（boxctl server）：前端内嵌进 boxctl，交叉编译成 Linux 二进制。
# 用法：./scripts/build-server.sh [amd64|arm64]，产物在 dist/boxctl-linux-<arch>
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
arch="${1:-amd64}"

(cd "$root/web" && npm ci && npm run build)
mkdir -p "$root/dist"
version="$(git -C "$root" describe --tags --always --dirty 2>/dev/null || echo dev)"
(cd "$root/boxctl" && CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath \
	-ldflags "-s -w -X boxctl/internal/buildinfo.Version=$version" -o "$root/dist/boxctl-linux-$arch" ./cmd/boxctl)
echo "产物：dist/boxctl-linux-$arch"
