#!/usr/bin/env bash
# 构建前端 → 内嵌进 Go → 交叉编译 linux/amd64 静态二进制（含 Mihomo 内核，ADR-030），
# 并复制到 OpenWrt 包目录 openwrt/boxctl/files/boxctl。
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"

version="${VERSION:-$(git -C "$root" describe --tags --always --dirty 2>/dev/null || echo dev)}"
commit="$(git -C "$root" rev-parse --short HEAD 2>/dev/null || echo unknown)"
date="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

cd "$root/web"
npm ci
npm run build

cd "$root/boxctl"
mkdir -p "$root/build"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
	-ldflags "-s -w \
		-X boxctl/internal/buildinfo.Version=$version \
		-X boxctl/internal/buildinfo.Commit=$commit \
		-X boxctl/internal/buildinfo.Date=$date" \
	-o "$root/build/boxctl-linux-amd64" ./cmd/boxctl

cp "$root/build/boxctl-linux-amd64" "$root/openwrt/boxctl/files/boxctl"
echo "built $root/build/boxctl-linux-amd64 ($version)"
