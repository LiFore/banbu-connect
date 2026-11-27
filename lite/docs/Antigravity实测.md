# Antigravity 实测清单（BXC-F0-016）

S1.7「Gemini（Antigravity）联网轮换」的第 0 期，决策见 ADR-046、ADR-047。后续条目（F0-017～023）已按下文 cockpit-tools 的做法先实现（ADR-059，2026-11-23），依赖实测的地方标了 TODO(verify:V-agy-*)，实测结论出来后再修正。

在 Mac 上执行。检测脚本 `scripts/probe-antigravity.sh` 是只读的，不输出 token 原文，凭据只给指纹，快照目录 `~/boxctl-probe/` 可以整个发回来分析。

## 准备

- 一个**测试用**个人 Google 账号（不要用主力账号），记为账号 A。最好再准备一个账号 B，用来验证切换。
- 装好 agy CLI 和 Antigravity 应用（各自官方安装方式），先不要登录。
- 第二台机器（另一台 Mac 或 Windows），用于 V-agy-portable。
- 记录本机的 Banbu Connect 严格组出口，登录和后续使用都走同一出口。

## 参考实现：cockpit-tools（2026-11-22 调研）

[jlcodes99/cockpit-tools](https://github.com/jlcodes99/cockpit-tools) v1.4.2（commit ccac863，Tauri + Rust），多平台 AI 账号切换器，其中 Antigravity 部分已经实现「Google OAuth 拿 refresh token → 存账号 → 写入应用/agy → 查额度 → 切号」。下面是从代码读出的做法，**未在本机验证**，作为各项实测的预期：实测只需确认或否定，不必从零摸索。

许可证是 CC BY-NC-SA 4.0（相同方式共享），**只参考做法，不复制代码**。

**登录（不经过 agy / 应用）**：自己跑一遍 Google OAuth，用的是 Antigravity 的 installed-app client（client_id `1071006060591-tmhssin2h21lcre235vtolojh4g403ep.apps.googleusercontent.com`，client_secret 也是写死在客户端里的公开值）。

- 授权地址 `https://accounts.google.com/o/oauth2/v2/auth`，`response_type=code`、`access_type=offline`、`prompt=consent`、`include_granted_scopes=true`，带 `state`
- scope：`openid`、`cloud-platform`、`userinfo.email`、`userinfo.profile`、`cclog`、`experimentsandconfigs`、`aicode`（均为 `https://www.googleapis.com/auth/` 前缀）
- 回跳：本机临时监听随机端口，`http://localhost:<端口>/oauth-callback`（只有 IPv4 时用 `127.0.0.1`）；另外支持把浏览器地址栏里的回跳 URL 手动粘贴回来
- 授权链接由程序生成后用系统浏览器打开，所以**完全可以改成交给指定的浏览器档案打开**（ADR-047），不依赖 agy 有没有 `--print-url`
- 换 token：POST `https://oauth2.googleapis.com/token`（`grant_type=authorization_code`），UA 伪装成 `vscode/1.X.X (Antigravity/4.3.0)`；之后用 `https://www.googleapis.com/oauth2/v2/userinfo` 取邮箱作为账号主键
- 没拿到 refresh token 时提示去 myaccount.google.com/permissions 撤销授权后重试（`prompt=consent` 下一般不会发生）
- 也支持直接粘贴 refresh token 添加账号：先刷新一次拿 access token，再查 userinfo

**刷新**：标准 `grant_type=refresh_token`，同一个 client_id/secret；提前 15 分钟刷新；返回里没有新 refresh token 就沿用旧的；`invalid_grant` 先等 500ms 重试一次再判失效。

**凭据写到哪里（切号就是覆盖这些位置）**，三处写同一份 JSON：

```json
{"token":{"access_token":"…","token_type":"Bearer","refresh_token":"…","expiry":"RFC3339 微秒 UTC"},"auth_method":"consumer","id_token":"…"}
```

| 位置 | macOS | Windows | Linux |
|---|---|---|---|
| 系统凭据（应用读取） | Keychain 通用密码，服务名 `gemini`、账号名 `antigravity`，值为 `go-keyring-base64:` + base64(JSON)；用 `security add-generic-password -U` 原地更新，不带 `-A/-T`，避免每次切号弹授权 | 凭据管理器 Generic，target `gemini:antigravity`、user `antigravity`，上限 2560 字节 | `secret-tool`，service `gemini`、username `antigravity` |
| 应用回退文件（Keyring 不可用时） | `~/.gemini/jetski-standalone-oauth-token` | 同左 | 同左 |
| agy CLI | `~/.gemini/antigravity-cli/antigravity-oauth-token` | 同左 | 同左 |

- 回退文件必须和系统凭据同时写，否则 Keyring 超时时应用会读到旧账号
- agy 目录不存在时，「切应用」不写 agy 文件，「切 CLI」才强制写
- 旧版 Antigravity IDE（VS Code 分支）另存在 `~/Library/Application Support/Antigravity IDE/User/globalStorage/state.vscdb`，`ItemTable` 键 `antigravityUnifiedStateSync.oauthToken`，值是 base64 的 protobuf（字段 1 access、2 类型、3 refresh、4 过期时间）；切号时还要处理 `…userStatus`、`…enterprisePreferences`。新的独立应用（Antigravity 2.x）走上表的 Keychain + 回退文件
- 切号流程：先关闭正在运行的应用（macOS 上还会停掉后台 hub 进程）→ 写凭据 → 重新启动

**额度**：Cloud Code 内部接口，基址 `https://cloudcode-pa.googleapis.com`（另有 `daily-cloudcode-pa`、`autopush-cloudcode-pa.sandbox` 备用），Bearer 为 access token。

1. `POST /v1internal:loadCodeAssist` 取 project id 和账号等级；没有 project 时 `POST /v1internal:onboardUser`，返回长任务时轮询
2. `POST /v1internal:fetchAvailableModels`，返回 `models` 里每个模型的 `quotaInfo.remainingFraction`（剩余比例）和 `resetTime`
3. UA 伪装为 `antigravity/2.25.0 (aidev_client; os_type=…; arch=…; cl=1003475615)`，loadCodeAssist 用 `antigravity/2.25.0 <os>/<arch> google-api-nodejs-client/10.3.0`
4. 额度是**按模型**的剩余比例 + 重置时间，没有 Claude 那种 5 小时/每周窗口

**对 S1.7 的影响（待实测确认后再改条目）**：

- F0-020 一键登录可以由客户端自己跑 OAuth，在固定档案里打开授权链接，拿到 refresh token 直接上传，不需要驱动 agy/应用的登录界面
- 如果 V-agy-refresh 确认 refresh token 不轮换，管理服务只托管 refresh token，各客户端各自刷新 access token，不需要 clauth 那种「单主机刷新 + 版本号确认」的凭据链
- F0-021 写入凭据 = 覆盖上表三处 + 重启应用；F0-022 额度采集可由管理服务直接调 fetchAvailableModels，不必在客户端抓
- 新风险：借用 Antigravity 的 client_id 和伪装 UA，属于非官方客户端行为，Google 可能据此封禁或改 client；版本号（4.3.0、2.25.0、cl）需随官方更新

## V-agy-store：凭据存在哪里，CLI 和应用是否共用

1. `scripts/probe-antigravity.sh snapshot 0-clean`
2. 只用 **agy CLI** 登录账号 A，完成后执行 `snapshot 1-agy-login`
3. 打开 **Antigravity 应用**，看它是否已经是登录状态（是 → 两者共用凭据）。如果没有登录，就在应用里登录账号 A，然后执行 `snapshot 2-app-login`
4. `scripts/probe-antigravity.sh diff 0-clean 1-agy-login`，以及 `diff 1-agy-login 2-app-login`

cockpit 预期：Keychain `gemini`/`antigravity`（`go-keyring-base64:` 前缀）+ `~/.gemini/jetski-standalone-oauth-token`（应用）+ `~/.gemini/antigravity-cli/antigravity-oauth-token`（agy），三处 JSON 相同。重点确认 agy 是否也读 Keychain，以及应用和 agy 是否共用登录。

需要的结论：
- 凭据文件路径，或 Keychain 条目（服务名 `svce`、账号名 `acct`），见 `files.txt` 和 `keychain.txt`
- 应用是否额外用了 Electron 的 safeStorage：Keychain 里是否有 `Antigravity Safe Storage`，`Library/Application Support/Antigravity/User/globalStorage/state.vscdb` 是否变化
- 文件格式和字段名：`files.txt` 里的结构，例如 `access_token`、`refresh_token`、`expiry_date`、`email`

## V-agy-browser：登录时浏览器怎么打开

1. 看 `agy-help.txt` 里有没有 `--no-browser`、`--print-url`、指定配置目录之类的选项
2. 把下面的脚本存为 `/tmp/log-url.sh` 并 `chmod +x`：
   ```bash
   #!/bin/bash
   echo "$@" >> /tmp/agy-url.log
   ```
   退出登录后执行 `BROWSER=/tmp/log-url.sh agy`，看 `/tmp/agy-url.log` 里是否出现授权链接
3. 如果 BROWSER 不起作用：看 agy 是否在终端里打印了链接；授权回跳地址是 `http://127.0.0.1:<端口>` 还是要粘贴授权码
4. 应用的登录是否也走系统默认浏览器，能否改用其他浏览器打开

cockpit 预期：不需要 agy 配合，客户端自己生成授权链接（见上文「登录」）。本项降级为：确认用 Antigravity client_id 自己登录拿到的 refresh token，写进上述位置后 agy 和应用都能直接用。

需要的结论：能不能拿到授权链接，交给 `--user-data-dir` 指定的浏览器档案去打开（ADR-047）。

## V-agy-portable：凭据换机器能不能用（决定联网轮换能否进行）

1. 在第一台机器上，把 V-agy-store 找到的凭据原样复制到第二台机器的对应位置。文件直接复制；Keychain 条目用 `security find-generic-password -s <svce> -w` 取出，在目标机器上 `security add-generic-password` 写入
2. 第二台机器**不登录**，直接执行一次 agy 对话；再打开应用试一次
3. 两台机器各自使用一段时间（交替使用，跨过 1 小时，确保 access token 至少刷新过一次），观察有没有被要求重新登录、有没有报错、Google 账号是否收到「新设备登录」安全提醒

cockpit 预期：能用（其核心功能就是粘贴 refresh token 在任意机器上添加并切换账号）。仍需确认是否触发 Google 安全提醒。

需要的结论：能用 / 不能用 / 能用但会触发安全提醒。不能用的话，ADR-046 退回为「各机器各自登录，只同步分配」。

## V-agy-refresh：刷新后 refresh token 会不会变

1. 登录后立刻 `snapshot r0`
2. 隔 1 小时以上再用一次 agy，然后 `snapshot r1`
3. `diff r0 r1`：看 `refresh_token` 的指纹有没有变，`access_token` 的指纹和过期时间是否更新

cockpit 预期：Google 刷新通常不返回新 refresh token，cockpit 沿用旧值。

需要的结论：refresh token 是否保持不变。变的话，服务端要改成 clauth 那种「一台主机负责刷新 + 按版本号确认」的凭据链。

## V-agy-quota：额度从哪个接口查

1. 应用和 agy 里有没有显示额度的地方（剩余百分比、5 小时和每周窗口、重置时间）？截图
2. 抓包，任选一种：
   - mitmproxy：`HTTPS_PROXY=http://127.0.0.1:8080 agy`；如果 agy 是 Node 程序，还要加 `NODE_EXTRA_CA_CERTS=<mitm 证书>`
   - 打开应用的开发者工具（Help → Toggle Developer Tools → Network），翻到显示额度的页面
3. 记下：接口地址、请求方法、请求头（鉴权方式，token 原文打码）、返回 JSON 的结构

cockpit 预期：`cloudcode-pa.googleapis.com/v1internal:fetchAvailableModels`，按模型返回 `quotaInfo.remainingFraction` 和 `resetTime`（前置 `loadCodeAssist` 取 project id）。抓包时重点对照这几个接口和 UA。

需要的结论：有没有可以直接调用的额度接口；窗口有哪些（5 小时、每周、按模型）；用量和重置时间的字段。

## 回报

把 `~/boxctl-probe/` 打包，连同上面每一项的结论发回来。我写入 F0-016 的 testNote，并据此调整 F0-017～023。

已实现的代码还依赖下面几点，实测时一并确认：

- Antigravity OAuth 的 client_secret：代码里不写死，在管理服务「Gemini 账号 → Antigravity OAuth 客户端」里填写
- 授权回跳用 `http://127.0.0.1:<随机端口>/oauth-callback`（不是 localhost），并带 PKCE（S256）：Google 是否接受
- 应用名 / 进程名按 `Antigravity` 处理（切换账号时退出再启动）；macOS 上的后台 hub 进程还没有处理
- macOS 经 `security -i` 的标准输入写钥匙串，值为 `go-keyring-base64:` + base64(JSON)
- 额度请求的 UA 和 `loadCodeAssist` 的 metadata（`ideType: ANTIGRAVITY`）

涉及的验证编号：TODO(verify:V-agy-store)、TODO(verify:V-agy-browser)、TODO(verify:V-agy-portable)、TODO(verify:V-agy-refresh)、TODO(verify:V-agy-quota)。
