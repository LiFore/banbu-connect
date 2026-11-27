# Banbu Connect Lite

Banbu Connect 的精简版桌面客户端（macOS / Windows），**只做 AI 账号管理，不做代理**。账号由管理服务托管，客户端负责在本机使用这些账号：

- **账号：** 管理服务分配给本机的 Claude / Codex（ChatGPT）/ Gemini（Antigravity）账号，一键登录、切换、多开工作区，查看额度。
- **托管凭据：** Claude / Codex 的 OAuth 令牌由管理服务集中保存和续期，本机写进工作区、读回上传（ADR-061、ADR-071）。
- **登录邮件：** 查看账号邮箱里 Anthropic / OpenAI 发来的登录链接和验证码，托盘在浏览器打开。
- **Claude Code 用量：** 本机 OTel 接收器，自动配置 Claude Code，上报到管理服务。
- **下载：** 管理服务文件存储里的文件。
- **应用更新：** 经管理服务 OTA 更新。

和完整版的区别：没有代理内核、TUN、Kill Switch、受信网络、路由盒，所以**不安装系统服务、不需要管理员权限**。后台进程以当前用户身份运行，由托盘外壳拉起，外壳退出它就退出。两者数据目录、端口、更新通道都分开，可以同时安装。

本仓库是 `banbu-connect/` 下四个独立仓库之一：

| 目录 | 内容 |
|---|---|
| `../client` | 完整版 Banbu Connect 桌面客户端（代理 + Kill Switch + 账号） |
| `../lite` | 本仓库：Banbu Connect Lite，只做账号管理 |
| `../server` | 管理服务（`boxctl server`），Lite 和完整版都连它 |
| `../box` | 路由盒 Banbu Box（面板 + proxy-guard） |

各仓库的 Go 代码是各自独立的一份（module 名都叫 `boxctl`）；改管理服务的接口时，要同时检查本仓库 `boxctl/internal/dist` 里的客户端代码。

## 架构

```
client/app（Tauri）              托盘、窗口；拉起并看护后台进程
   │ HTTP + 令牌（127.0.0.1:17940）
boxctl desktop run（用户进程）    数据在用户目录，标准输入关闭时退出
   ├─ internal/desktop           本机 API：账号、工作区、额度、下载、应用更新、事件与通知
   ├─ internal/dist              管理服务的客户端：绑定与接入申请、AI 账号同步、上报
   ├─ internal/otel              Claude Code 遥测接收器
   └─ internal/webui             内嵌的前端（web/ 构建产物）
```

| 目录 | 作用 |
|---|---|
| `boxctl/` | Go 后端（module boxctl），入口 `cmd/boxctl`：`desktop run`、`version` |
| `web/` | React + Ant Design 前端（Vite），构建后内嵌进 boxctl |
| `client/app/` | Tauri 外壳：托盘、窗口、Claude Code / Antigravity / ChatGPT 的本机操作 |
| `scripts/` | `check.sh`（检查）、`build-app.sh`（打包）、`upload-release.sh`（上传更新包） |

**数据目录：** macOS `~/Library/Application Support/Banbu Connect Lite`，Windows `%LOCALAPPDATA%\Banbu Connect Lite`。里面有本机 API 令牌（`token`）、管理服务绑定、日志（`boxctl.log`）和遥测数据库。

## 管理服务

Lite 版用 `../server` 的管理服务（默认 `https://connect.duallee.cn`）：在「设置」里申请接入，管理员授权后点「刷新授权」自动绑定；也可以手动填令牌。管理服务下发的代理配置（节点、代理组、规则）Lite 版忽略，只用账号相关的接口。

**更新通道：** Lite 版查更新时平台带 `lite-` 前缀（`lite-darwin`、`lite-windows`），和完整版的更新包分开。管理服务的「客户端更新」需要接受这些平台名，上传时选对应的平台（`scripts/upload-release.sh` 已按此填写）。

## 开发

需要 Go ≥ 1.26、Node ≥ 20、Rust（打包时）。

```bash
cd web && npm install     # 首次
make dev-desktop          # 终端 1：后台进程，数据在 .dev/lite，地址 127.0.0.1:17940
make dev-web              # 终端 2：Vite 开发服务器
```

浏览器打开 `http://127.0.0.1:17940/#token=<.dev/lite/token 的内容>`。

## 检查与打包

```bash
./scripts/check.sh              # gofmt、go vet、go test、前端 typecheck 与 lint
./scripts/build-app.sh [x.y.z]  # 前端 → 内嵌进 boxctl → 打包进 Tauri 应用（在目标平台上运行）
```

产物在 `client/app/src-tauri/target/release/bundle/`。更新包签名的私钥在构建机的 `~/.tauri/banbu-connect.key`（与完整版共用一对密钥，公钥在 `tauri.conf.json`）。

## 安全

- 后台进程只监听回环地址并校验 Host，防止 DNS 重绑定；写请求校验来源。
- API 只认 Bearer 令牌；令牌保存在只有本人可读的数据目录。外壳打开窗口时把令牌放在 URL 片段里，页面读取后立即从地址栏移除。
- 管理服务的设备令牌和托管凭据保存在数据目录（权限 `0600`）。

## 约定

- API 错误统一返回 `{code, message}`。
- 仓库内任何样本数据不得包含真实凭据。
