#!/usr/bin/env bash
# 统一检查（M13-01）：任一项失败即返回非 0。
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"

echo "== go: gofmt / vet / test"
cd "$root/boxctl"
unformatted="$(gofmt -l .)"
if [ -n "$unformatted" ]; then
	echo "gofmt 未格式化：" >&2
	echo "$unformatted" >&2
	exit 1
fi
go vet ./...
go test ./...

echo "== web: typecheck / lint"
cd "$root/web"
npm run --silent typecheck
npm run --silent lint

echo "OK"
