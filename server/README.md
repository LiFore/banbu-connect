# Banbu Connect 管理服务

`boxctl server`：部署在公网服务器上的管理服务。一个 Go 二进制，前端已内嵌，数据在 SQLite。

- **配置下发：** 在网页上维护订阅、节点、代理组、线路和规则，发布给绑定的 Banbu Connect 客户端和路由盒。
- **客户端：** 接入申请与授权、设备令牌、吊销、设备访问控制，查看版本、同步状态、诊断数据和连接统计。
- **AI 账号：** 托管 Claude / Codex / Gemini（Antigravity）账号和 OAuth 凭据，集中续期，按客户端授权；读取账号 Gmail 里的登录邮件；额度、在线时长、到期提醒。
- **客户端更新：** 托管 Tauri 更新包，发布、灰度、更新方式（普通、推送、静默、强制）。
- **其他：** 文件存储（本地或阿里云 OSS）、通知与告警、Claude Code 遥测汇总。

## 相关仓库

都在同一个目录 `banbu-connect/` 下，各自独立：

| 目录 | 内容 |
|---|---|
| `../client` | Banbu Connect 完整版桌面客户端（本机代理 + Kill Switch + 账号） |
| `../lite` | Banbu Connect Lite：只做 AI 账号管理的桌面客户端，不做代理 |
| `../box` | Banbu Box 路由盒（iStoreOS 面板 + proxy-guard） |
| `server`（本仓库） | 管理服务 |

客户端和路由盒都通过 `/api/v1/client/...` 接口连接本服务；接口有改动时要和它们一起改。

## 部署

```bash
./scripts/build-server.sh amd64    # arm64 服务器用 arm64，产物 dist/boxctl-linux-<arch>
```

把二进制复制到服务器的 `/usr/local/bin/boxctl`，用 `deploy/banbu-connect.service` 交给 systemd 运行（数据在 `/var/lib/banbu-connect`）。完整步骤见 [docs/管理服务部署.md](docs/管理服务部署.md)。

```bash
# 自签名证书（客户端绑定时填证书指纹）
boxctl server -listen 0.0.0.0:8443 -data-dir /var/lib/banbu-connect
# 已有证书
boxctl server -listen 0.0.0.0:443 -tls-cert fullchain.pem -tls-key privkey.pem
# 放在反向代理（Caddy、Nginx）后面，由代理负责 HTTPS，并设置 X-Real-IP
boxctl server -listen 127.0.0.1:8080 -plain-http
```

初始管理员密码在数据目录的 `initial-password`，首次登录要修改；忘记时运行 `boxctl reset-password -data-dir …`。管理页面在 `/admin`。

## 客户端接入

客户端默认连接 `https://connect.duallee.cn`，可以改。

1. 用户在客户端填设备名称（默认取电脑名称）后点「申请接入」，客户端提交设备名称、平台（如 `darwin/arm64`）、系统版本、应用版本。
2. 管理员在「客户端」页的「接入申请」里授权（可以改名）或拒绝。
3. 用户点「刷新授权」，客户端用申请时拿到的密钥领取令牌并自动绑定。

- 令牌在领取时才生成，服务端只存哈希，只能领取一次。
- 同一台电脑重复申请时合并成一条（按客户端安装时生成的随机 ID），旧密钥失效。重新授权已吊销的客户端会解除吊销。
- 申请接口不需要登录：每个 IP 最多 5 条、全部最多 200 条待授权的申请，超出返回 429。
- 也可以在「客户端」页新建令牌（只显示一次），在客户端手动绑定。

## AI 账号

- **登记：** 在「AI 账号」登记账号邮箱和类型（Claude / Codex），指定哪些客户端可以用。
- **Gmail 授权：** 保存一个 Google OAuth 客户端（「桌面应用」类型，密钥加密保存），每个账号授权一次只读权限（`gmail.readonly`），只保存 refresh token（加密）。授权时浏览器跳到打不开的 `http://127.0.0.1:53682/?…`，把这个地址粘贴回管理页面即可。OAuth 同意屏幕要发布为正式版，停在「测试」状态时授权 7 天后失效。
- **一键登录：** 客户端发起登录后，服务每 5 秒搜一次账号的 Gmail，取出 Claude 的 magic link（或 OpenAI 的 6 位验证码），只交给发起登录的那台客户端一次；10 分钟内没收到邮件就作废，不入库、不写日志。
- **查看邮件：** 客户端可以看账号最近 7 天 Anthropic / OpenAI 发来的邮件（最多 10 封，纯文本）。其他发件人的邮件不会离开管理服务。
- **托管凭据与集中续期：** Claude / Codex 的 OAuth 凭据保存在服务端（ADR-061），访问令牌快过期时由服务端安排一台在线客户端续期（ADR-071），其他客户端只拿访问令牌。
- **Gemini（Antigravity）：** 账号池、额度采样、按额度轮换。

## 客户端更新

- 在「客户端更新」上传更新包和 `.sig` 签名，填版本号和平台，打开「发布」后客户端才会收到。
- 平台：完整版 `darwin-aarch64`、`darwin-x86_64`、`windows-x86_64`；Lite 版带 `lite-` 前缀（`lite-darwin-aarch64` 等），两者的更新包互不干扰。
- 「灰度」指定客户端后只有它们收到这个版本，其他客户端收到不在灰度中的最高版本，清空即全量。
- 客户端经自己的本机服务转发更新请求，带着设备令牌，所以外壳不用处理自签名证书。更新包由 Tauri updater 签名保证完整。

## 预置配置

首次启动前把文件放进数据目录的 `seed/`，服务在还没有任何节点和代理组时导入一次：`*.yaml` 是 Mihomo 配置（导入 `proxies`、`proxy-groups`、`x-strict`、`x-selections`），`*.txt` 每行一条规则。`boxctl server export-seed -data-dir … -out …` 把一台管理服务当前的配置导出成预置文件。文件含节点凭据，不要放进仓库。

## 开发

需要 Go ≥ 1.26、Node ≥ 20。

```bash
cd web && npm install     # 首次
make dev-server           # 终端 1：https://127.0.0.1:18443（自签名证书），数据在 .dev/server
make dev-web              # 终端 2：Vite 开发服务器，/api 代理到 18443
```

```bash
./scripts/check.sh        # gofmt、go vet、go test、前端 typecheck 与 lint
```

## 约定

- 新增表结构只追加迁移文件 `boxctl/internal/store/migrations/NNNN_xxx.sql`，不修改已发布的迁移。
- API 错误统一返回 `{code, message, detail}`。
- 仓库内任何样本数据不得包含真实凭据。
