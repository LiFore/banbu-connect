.PHONY: check app dev-desktop dev-web

check:
	./scripts/check.sh

# 打包 Banbu Connect Lite（在目标平台上运行）
app:
	./scripts/build-app.sh

# 本地开发：两个终端分别运行。后台进程用开发数据目录，不影响已安装的应用。
dev-desktop:
	cd boxctl && go run ./cmd/boxctl desktop run -data ../.dev/lite

dev-web:
	cd web && npm run dev
