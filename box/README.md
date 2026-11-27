# Banbu Box（boxctl）

端侧软路由盒子（Banbu Box，底座 iStoreOS，ADR-029）的管理程序：一个 Go 二进制，两个进程角色。
界面和功能与桌面客户端 Banbu Connect 对齐：概览、线路、Kill Switch、高级（管理服务、订阅、节点、代理组、规则与设置）。

| 角色 | 命令 | 职责 |
|---|---|---|
| panel | `boxctl panel` | Web 管理面板 + 托管编进二进制的 Mihomo 内核（TUN auto-redirect 透明代理局域网，ADR-030）、严格线路出口检查、绑定管理服务 |
| guard | `boxctl guard` | proxy-guard：独立 nft 表 `inet proxy_guard` 常驻拦截受保护服务的直连，dnsmasq nftset 填充地址，不依赖 panel 和内核存活（ADR-032） |

两者通过 unix socket（默认 `/var/run/boxctl/guard.sock`）通信。另有 `boxctl status`（本机状态，LuCI 用）、`boxctl reset-password`、`boxctl mihomo`（内核子进程）、`boxctl version`。

## 相关仓库

本仓库在 `banbu-connect/box`，同级的几个独立仓库：

| 目录 | 内容 |
|---|---|
| `../client` | 桌面客户端 Banbu Connect（本机代理 + Kill Switch） |
| `../lite` | Banbu Connect Lite（只做 AI 账号管理） |
| `../server` | 管理服务（`boxctl server`）：下发配置、托管账号、客户端更新。路由盒在「高级 → 管理服务」绑定它 |

和桌面客户端共用的代码（`internal/core`、`gen`、`groups`、`nodes`、`dist` 的客户端部分等）是各自独立的一份，改动时注意同步。

## 文档

- `端侧软路由盒子_技术方案与实施路线.docx` — 原始方案
- `管理面板_开发评估.md` — 开发评估（架构、工作量、里程碑）
- `docs/功能清单与验收.md` — **功能点与验收标准**，开发与提交以其编号（如 `M2-08`）为准

## 目录

```
boxctl/                 Go 后端（module boxctl）
  cmd/boxctl/           入口：panel / guard / status / reset-password / mihomo / version
  internal/panel/       HTTP API + 内嵌前端
  internal/core/        内核托管（生成、校验、热重载、线路）
  internal/box/         路由盒运行时：把 core、严格线路检查、管理服务、guard 策略接进 panel
  internal/guard/       proxy-guard 守护进程（nft 规则、dnsmasq 片段、自检恢复）
  internal/ipc/         panel ↔ guard unix socket 协议
  internal/dist/        管理服务的客户端：绑定、同步下发的配置
  internal/store/       SQLite 与版本化迁移（migrations/*.sql）
  internal/webui/dist/  前端构建产物（go:embed，勿手改）
web/                    React + Ant Design 前端（Vite）
openwrt/boxctl/         iStoreOS（OpenWrt 24.10）包定义与 procd init 脚本
openwrt/luci-app-boxctl/  LuCI「服务 → Banbu Box」入口
scripts/                check.sh / build.sh / build-istore.sh / fault-inject.sh / fault-traffic.sh
```

## 开发环境

需要 Go ≥ 1.26（modernc.org/sqlite 要求）、Node ≥ 20。

```bash
cd web && npm install           # 首次
make dev-guard                  # 终端 1（-fake：不改动防火墙）
make dev-panel                  # 终端 2，HTTPS API 在 127.0.0.1:8443（自签证书）
make dev-web                    # 终端 3，Vite 开发服务器 http://localhost:5173，/api 代理到 panel
```

首次启动会在数据目录（开发时为 `.dev/`）生成：`boxctl.db`、`device.key`（凭据加密密钥）、`tls/`（自签证书）、`initial-password`（初始管理员密码，首次登录强制修改后删除）。忘记密码：`boxctl reset-password -data-dir <目录>`。

Windows 下没有 make 时直接执行 Makefile 里对应的命令；`scripts/*.sh` 可在 Git Bash 中运行。

## 检查与构建

```bash
./scripts/check.sh    # gofmt、go vet、go test、前端 typecheck 与 lint
./scripts/build.sh    # 前端构建 → 内嵌 → linux/amd64 静态二进制 → build/ 与 openwrt/boxctl/files/
./scripts/build-istore.sh   # 再打成 build/istore/ 下的 ipk（opkg）、apk（apk-tools 3）和对应的 .run 安装包（不需要 SDK；打 apk 需要 Docker）
```

安装：按系统版本选 .run——OpenWrt / iStoreOS 24.10（opkg）用 `banbu-box_*_openwrt24.10_x86_64.run`，25.12（apk）用 `banbu-box_*_openwrt25.12_x86_64.run`（装错会提示）。iStore →「手动安装」上传，或 SSH 里 `sh banbu-box_*.run`。只打一种时设 `TARGETS=openwrt24.10` 或 `TARGETS=openwrt25.12`。装好后在 LuCI「服务 → Banbu Box」打开面板（`https://<LAN 地址>:8443`），初始密码在 `/etc/boxctl/initial-password`。
用 SDK 打包：把 `openwrt/boxctl`、`openwrt/luci-app-boxctl` 复制到 SDK 的 `package/` 下编译（24.10 SDK 出 ipk，25.12 SDK 出 apk）。

## 约定

- 新增表结构只追加迁移文件 `NNNN_xxx.sql`，不修改已发布的迁移。
- API 错误统一返回 `{code, message, detail}`。
- 仓库内任何样本数据不得包含真实凭据（M13-04）。
