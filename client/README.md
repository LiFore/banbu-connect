# Banbu Connect（完整版桌面客户端）

班布的桌面客户端（macOS / Windows）：本机代理内核、只保护特定服务的 Kill Switch、受信网络，加上 AI 账号管理。后台服务以管理员权限安装成系统服务，托盘外壳用 Tauri。

详细说明（架构、规则生成、DNS 接管、受信网络、Kill Switch、OTA、安全、验证状态）见 [client/README.md](client/README.md)。

## 同级仓库

这个仓库在 `banbu-connect/` 下，和另外三个仓库并列，各自独立提交：

| 目录 | 内容 |
|---|---|
| `../client` | 本仓库：完整版桌面客户端 |
| `../lite` | Banbu Connect Lite：只做 AI 账号管理，不做代理，不需要管理员权限 |
| `../server` | 管理服务（`boxctl server`）：下发配置、托管 AI 账号和客户端更新包 |
| `../box` | 路由盒（Banbu Box，iStoreOS）：Web 面板和 proxy-guard |

客户端和路由盒各有一份共用的代理代码（订阅、节点、代理组、配置生成、Kill Switch），改动时按需同步。

## 目录

| 目录 | 作用 |
|---|---|
| `boxctl/` | Go 后端（module boxctl），入口 `cmd/boxctl`：`desktop`、`mihomo`、`version` |
| `web/` | React + Ant Design 前端（Vite），构建后内嵌进 boxctl |
| `client/app/` | Tauri 外壳：托盘、窗口、安装页 |
| `client/guard/`、`client/clash-verge/` | 旧方案（脚本版 Kill Switch、Clash Verge 脚本） |
| `scripts/` | `check.sh`、`build-app.sh`、`upload-release.sh`、`force-stop-banbu.*`、`probe-antigravity.sh` |

## 开发

需要 Go ≥ 1.26、Node ≥ 20、Rust（打包时）。

```bash
cd web && npm install     # 首次
make dev-desktop          # 终端 1：后台服务（-fake，不改动系统），127.0.0.1:17933，数据在 .dev/desktop
make dev-web              # 终端 2：Vite 开发服务器，/api 代理到上面的后台服务
```

## 检查与打包

```bash
./scripts/check.sh              # gofmt、go vet、go test、前端 typecheck 与 lint
./scripts/build-app.sh [x.y.z]  # 前端 → 内嵌进 boxctl → 打包进 Tauri 应用（在目标平台上运行）
```

产物在 `client/app/src-tauri/target/release/bundle/`。打包后如果有 `.env` 和签名私钥，会自动上传更新包到管理服务（不发布）。

## 约定

- 新增表结构只追加迁移文件 `NNNN_xxx.sql`，不修改已发布的迁移。
- API 错误统一返回 `{code, message, detail}`。
- 仓库内任何样本数据不得包含真实凭据。
