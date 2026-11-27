.PHONY: check build dev-server dev-web

check:
	./scripts/check.sh

# 管理服务：前端内嵌进 boxctl，交叉编译成 Linux 二进制（dist/boxctl-linux-<arch>）
build:
	./scripts/build-server.sh

# 本地开发：两个终端分别运行。自签名证书，客户端绑定时填「客户端」页面显示的指纹。
dev-server:
	cd boxctl && go run ./cmd/boxctl server -listen 127.0.0.1:18443 -data-dir ../.dev/server

dev-web:
	cd web && npm run dev
