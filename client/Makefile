.PHONY: check app dev-desktop dev-web

check:
	./scripts/check.sh

# 打包 Banbu Connect（在目标平台上运行）
app:
	./scripts/build-app.sh

# 本地开发：两个终端分别运行。-fake 时 Kill Switch 用假后端，不改动系统；要在设置里关掉 TUN。
# -fake-ssid 把当前网络当作这个 Wi-Fi，用来预览受信网络。
dev-desktop:
	cd boxctl && go run ./cmd/boxctl desktop run -fake -fake-ssid Home -addr 127.0.0.1:17933 -data ../.dev/desktop

dev-web:
	cd web && npm run dev
