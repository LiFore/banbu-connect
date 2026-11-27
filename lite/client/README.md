# Banbu Connect Lite 桌面外壳

Tauri 外壳（`client/app`）：托盘、窗口，以及必须以用户身份在本机执行的操作（Claude Code 登录、Antigravity 凭据、ChatGPT 客户端登录 Codex、工作区多开、文件下载、OTA 更新）。整体说明见仓库根目录的 `README.md`。

## 后台进程

外壳启动时拉起随应用打包的 `boxctl desktop run -exit-on-stdin-close`（`resources/boxctl`），以当前用户身份运行，监听 `127.0.0.1:17940`。外壳持有它的标准输入，外壳退出（包括崩溃）时管道关闭，后台进程随之退出；后台进程意外退出时外壳重新拉起。端口上已经有一个在运行时不重复拉起。

令牌由后台进程写在数据目录的 `token`，外壳读同一个文件。

## 构建

```bash
./scripts/build-app.sh          # 沿用 tauri.conf.json 里的版本
./scripts/build-app.sh 0.3.0    # 先把版本号写进 tauri.conf.json 和 Cargo.toml，再打包
```

需要 Go ≥ 1.26、Node ≥ 20、Rust，在目标平台上运行（Tauri 不能交叉编译）。macOS 只有 ad-hoc 签名，第一次打开要在「系统设置 → 隐私与安全性」里点「仍要打开」，或运行 dmg 里的「修复无法打开」。

## Claude 账号登录

绑定管理服务后，「账号」页列出允许本机使用的账号，点「登录」在本机发起 Claude Code（CLI）的 OAuth 登录。我们只发起，不接管：PKCE、换 token、保存凭据都由 Claude Code 自己完成。

1. 页面通知本机服务，本机服务请管理服务开始等这个账号的登录邮件（`internal/dist/claude_client.go`）。
2. 托盘外壳从状态里看到待启动的登录，执行 `claude auth login --claudeai --email <邮箱>`（`client/app/src-tauri/src/claude_login.rs`）。由外壳执行，和用户自己在终端里登录一样。
3. 管理服务用 Gmail API 每 5 秒搜一次这个账号的 Gmail（ADR-025，管理服务代码在 `../server` 仓库）：`from:mail.anthropic.com after:<发起时间>`，取最新一封的原文，核对发件人后取出 `https://claude.ai/magic-link#…`。

**Gmail 授权：** 管理服务保存一个 Google OAuth 客户端（「桌面应用」类型，密钥加密保存），每个账号授权一次只读权限（`gmail.readonly`），服务端只保存 refresh token（加密），不保存 Gmail 密码。授权时浏览器会跳到打不开的 `http://127.0.0.1:53682/?…`，把这个地址粘贴回管理页面即可；这样管理服务没有域名、用自签名证书也能授权。OAuth 同意屏幕要发布为正式版，停在「测试」状态时授权 7 天后失效。授权失效或被撤销时，登录立即失败并提示重新授权。
4. 本机服务取到链接后先留在本机（阶段 `mail_ready`），页面自动打开这个账号的「邮件」。用户在登录邮件里点「在浏览器打开」后，外壳的长轮询才拿到链接，用默认浏览器打开；浏览器里完成授权后 Claude Code 收到回调并保存凭据。浏览器已登录同一账号时不会发邮件，Claude Code 直接完成。
5. 外壳用 `claude auth status --json` 核对登录的邮箱，回报给管理服务，「Claude 账号」页显示最近登录的时间和客户端。

- **查看邮件：** 每个账号旁的「邮件」列出这个 Gmail 最近 7 天 Anthropic（`anthropic.com` 及子域名）发来的邮件，最多 10 封（`internal/dist/claude_mail.go`），不要求先点「登录」：自己手动登录（在浏览器或 Claude Code 里输入邮箱）时也在这里取登录链接，可以复制，或点「在浏览器打开」由托盘外壳打开（几秒内）。其他发件人的邮件不会离开管理服务；正文只返回纯文本，客户端不渲染邮件的 HTML。
- **链接等于登录凭据：** 一键登录的链接管理服务只在内存里保留，只交给发起登录的那台客户端一次，10 分钟内没收到邮件就作废，不入库、不写日志。但「邮件」里能看到原文，所以被允许使用这个账号的客户端在 7 天内都能看到登录链接。外壳只打开 `https://claude.ai/magic-link#` 开头的链接，页面请求打开时本机服务和外壳各校验一次。
- **每台设备各自登录：** 不在设备之间复制凭据，避免一台刷新令牌后另一台失效。
- **找 claude：** 外壳先找 PATH，再找 `~/.local/bin`、`%APPDATA%\npm`、Homebrew 等常见位置。找不到时登录失败并提示安装 Claude Code。

## 应用更新（OTA）

- **签名：** 更新包用 Tauri updater 签名。私钥在构建机的 `~/.tauri/banbu-connect.key`（不进仓库，丢了只能换公钥重新发完整安装包），公钥在 `tauri.conf.json`。`scripts/build-app.sh` 找到私钥才生成更新包，结束时列出要上传的文件：macOS `Banbu Connect.app.tar.gz`，Windows `*-setup.exe`，各带一个 `.sig`。
- **发布：** 在管理服务「客户端更新」上传文件和 `.sig`，填版本号（与 `tauri.conf.json` 一致）和平台，打开「发布」。
- **检查：** 外壳启动时和每次打开主界面时各检查一次（一分钟内不重复），在后台时每小时一次，托盘菜单可以手动检查；有更新时托盘显示「安装更新 x.y.z」，点击后下载、安装、重启。
- **转发：** 外壳只访问本机服务 `http://127.0.0.1:17940/api/v1/desktop/update/…`，由服务带着客户端令牌转发到管理服务。所以外壳拿不到服务端令牌，也不用处理自签名证书。更新包由签名保证完整，回环地址上用 http 是安全的。
- **后台服务：** 应用更新后，打包在里面的后台服务是新的，但已安装的还是旧版本。外壳发现两者版本不同时打开安装页，提示「更新后台服务」，授权一次即可；设置和数据保留。


