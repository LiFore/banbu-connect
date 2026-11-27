.PHONY: check build dev-panel dev-guard dev-web

check:
	./scripts/check.sh

build:
	./scripts/build.sh

# 本地开发：三个终端分别运行
dev-guard:
	cd boxctl && go run ./cmd/boxctl guard -sock ../.dev/guard.sock -data-dir ../.dev -fake

dev-panel:
	cd boxctl && go run ./cmd/boxctl panel -listen 127.0.0.1:8443 -data-dir ../.dev -guard-sock ../.dev/guard.sock -status-sock ../.dev/panel.sock

dev-web:
	cd web && npm run dev
