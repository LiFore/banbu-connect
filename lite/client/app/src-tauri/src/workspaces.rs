// 多开：本机工作区（S1.9，ADR-052）。外壳运行在用户会话里，管理每个账号的隔离目录：
//   workspaces/claude-cli/<账号>：CLAUDE_CONFIG_DIR；workspaces/codex/<账号>：CODEX_HOME。
// 工作区目录一经创建不再改名：Claude 在 macOS 钥匙串里的凭据条目按目录字符串的哈希命名，写法变了就是另一个登录。
//
// - 定期扫描（30 秒，操作后立即）：登录与否、实际登录的账号、是否为默认账号、运行中的进程数、额度用的访问令牌，
//   报给后台服务（PUT /api/v1/desktop/workspaces/report）；
// - 执行后台服务排队的操作：Codex 登录（codex login，或用 ChatGPT 桌面客户端登录 app_login）、打开隔离终端、设为默认、删除、写入管理服务托管的凭据（import，ADR-061）；
// - 生成命令行入口 banbu-claude / banbu-codex（BXC-F0-038）。
// 访问令牌只读不刷新，刷新交给 claude / codex 自己，避免抢 refresh token。读出的 refresh token 交给后台服务，
// 和管理服务托管的凭据对比后上传（ADR-061）。
//
// 设为默认（BXC-F0-046，ADR-055）是「移动」：把账号的凭据移到默认目录（~/.claude + 钥匙串默认条目，或 ~/.codex），
// 工作区里只留标记 .banbu-default，同一份 refresh token 只在一处；切到别的账号时先移回原工作区。
// 默认目录原来是用户自己登录的账号时，先备份到 workspaces/backup/。

use std::path::{Path, PathBuf};
use std::process::{Command, Stdio};
use std::sync::Mutex;
use std::time::{Duration, Instant};

use serde::{Deserialize, Serialize};
use serde_json::{json, Value};
use sha2::{Digest, Sha256};

use crate::{read_token, token_path, SERVICE};

const SCAN_EVERY: Duration = Duration::from_secs(30);
pub(crate) const LOGIN_WAIT: Duration = Duration::from_secs(10 * 60);
const MARKER: &str = ".banbu-workspace.json";
const DEFAULT_MARKER: &str = ".banbu-default";

pub const CLAUDE: &str = "claude";
pub const CODEX: &str = "codex";

// ---- 路径 ----

pub fn root() -> Option<PathBuf> {
    Some(token_path()?.parent()?.join("workspaces"))
}

fn kind_dir(kind: &str) -> &'static str {
    if kind == CODEX {
        "codex"
    } else {
        "claude-cli"
    }
}

pub(crate) fn home() -> Option<PathBuf> {
    std::env::var_os(if cfg!(windows) { "USERPROFILE" } else { "HOME" }).map(PathBuf::from)
}

fn env_name(kind: &str) -> &'static str {
    if kind == CODEX {
        "CODEX_HOME"
    } else {
        "CLAUDE_CONFIG_DIR"
    }
}

/// 账号邮箱转目录名（和浏览器档案同一规则）。
pub fn dir_name(email: &str) -> String {
    crate::gemini::profile_name(email)
}

pub fn workspace_dir(kind: &str, email: &str) -> Option<PathBuf> {
    Some(root()?.join(kind_dir(kind)).join(dir_name(email)))
}

#[derive(Serialize, Deserialize, Default)]
struct Marker {
    email: String,
}

/// 确保工作区存在：建目录、写标记；Codex 另写 config.toml（凭据存文件，不用钥匙串/secrets 后端，便于隔离）。
pub fn ensure(kind: &str, email: &str) -> Result<PathBuf, String> {
    let dir = workspace_dir(kind, email).ok_or("找不到数据目录")?;
    std::fs::create_dir_all(&dir).map_err(|e| format!("创建工作区失败：{e}"))?;
    let marker = dir.join(MARKER);
    if !marker.exists() {
        std::fs::write(&marker, serde_json::to_vec(&Marker { email: email.to_string() }).unwrap_or_default())
            .map_err(|e| format!("写工作区标记失败：{e}"))?;
    }
    if kind == CODEX {
        let cfg = dir.join("config.toml");
        if !cfg.exists() {
            // TODO(verify:V-multi-codex-store) Windows 上 file 存储时凭据只在 auth.json。
            std::fs::write(&cfg, "cli_auth_credentials_store = \"file\"\n").map_err(|e| format!("写 config.toml 失败：{e}"))?;
        }
    }
    Ok(dir)
}

/// 列出某类工作区（账号, 目录）。
fn list(kind: &str) -> Vec<(String, PathBuf)> {
    let Some(base) = root().map(|r| r.join(kind_dir(kind))) else { return Vec::new() };
    let Ok(rd) = std::fs::read_dir(&base) else { return Vec::new() };
    let mut out = Vec::new();
    for e in rd.flatten() {
        let dir = e.path();
        let m: Option<Marker> = std::fs::read(dir.join(MARKER)).ok().and_then(|b| serde_json::from_slice(&b).ok());
        if let Some(m) = m.filter(|m| !m.email.is_empty()) {
            out.push((m.email, dir));
        }
    }
    out.sort();
    out
}

/// Claude 工作区目录（写 OTel 配置用，BXC-F0-035）。
pub fn claude_dirs() -> Vec<PathBuf> {
    list(CLAUDE).into_iter().map(|(_, d)| d).collect()
}

pub(crate) fn is_default(dir: &Path) -> bool {
    dir.join(DEFAULT_MARKER).exists()
}

fn default_owner(kind: &str) -> Option<(String, PathBuf)> {
    list(kind).into_iter().find(|(_, d)| is_default(d))
}

// ---- 凭据读写 ----

fn read_json(p: &Path) -> Option<Value> {
    std::fs::read(p).ok().and_then(|b| serde_json::from_slice(&b).ok())
}

fn write_private(p: &Path, body: &[u8]) -> Result<(), String> {
    if let Some(d) = p.parent() {
        std::fs::create_dir_all(d).map_err(|e| e.to_string())?;
    }
    let tmp = p.with_extension("banbu-tmp");
    std::fs::write(&tmp, body).map_err(|e| e.to_string())?;
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        let _ = std::fs::set_permissions(&tmp, std::fs::Permissions::from_mode(0o600));
    }
    std::fs::rename(&tmp, p).map_err(|e| e.to_string())
}

/// JWT 里的邮箱（不校验签名）。
fn jwt_email(tok: &str) -> String {
    use base64::Engine;
    let Some(p) = tok.split('.').nth(1) else { return String::new() };
    let Some(c) = base64::engine::general_purpose::URL_SAFE_NO_PAD
        .decode(p.trim_end_matches('='))
        .ok()
        .and_then(|b| serde_json::from_slice::<Value>(&b).ok())
    else {
        return String::new();
    };
    c["email"]
        .as_str()
        .or_else(|| c["https://api.openai.com/profile"]["email"].as_str())
        .unwrap_or_default()
        .to_lowercase()
}

/// Claude 钥匙串条目名：默认目录没有后缀，其他目录加目录字符串 sha256 的前 8 位。
/// TODO(verify:V-multi-claude-keychain) 命名规则来自 cockpit-tools，官方没有文档。
#[cfg_attr(not(target_os = "macos"), allow(dead_code))]
fn claude_keychain_service(dir: Option<&Path>) -> String {
    match dir {
        None => "Claude Code-credentials".to_string(),
        Some(d) => {
            let sum = Sha256::digest(d.display().to_string().as_bytes());
            let hex: String = sum.iter().take(4).map(|b| format!("{b:02x}")).collect();
            format!("Claude Code-credentials-{hex}")
        }
    }
}

/// 读 Claude 凭据（{"claudeAiOauth": {...}}）。dir 为空表示默认目录。
fn claude_cred_read(dir: Option<&Path>) -> Option<String> {
    #[cfg(target_os = "macos")]
    {
        let out = Command::new("/usr/bin/security")
            .args(["find-generic-password", "-s", &claude_keychain_service(dir), "-w"])
            .stderr(Stdio::null())
            .output()
            .ok()?;
        if out.status.success() {
            return Some(String::from_utf8_lossy(&out.stdout).trim().to_string());
        }
    }
    // Windows/Linux 在 <目录>/.credentials.json；macOS 钥匙串写失败时也会回退到这里。
    let d = match dir {
        Some(d) => d.to_path_buf(),
        None => home()?.join(".claude"),
    };
    std::fs::read_to_string(d.join(".credentials.json")).ok()
}

fn claude_cred_write(dir: Option<&Path>, text: &str) -> Result<(), String> {
    #[cfg(target_os = "macos")]
    {
        use std::io::Write;
        // 十六进制传值（-X）：JSON 里的引号不用转义，也不出现在进程参数里。
        let hex: String = text.bytes().map(|b| format!("{b:02x}")).collect();
        let user = std::env::var("USER").unwrap_or_default();
        let mut child = Command::new("/usr/bin/security")
            .arg("-i")
            .stdin(Stdio::piped())
            .stdout(Stdio::null())
            .stderr(Stdio::piped())
            .spawn()
            .map_err(|e| e.to_string())?;
        let line = format!("add-generic-password -U -a \"{user}\" -s \"{}\" -X {hex}\n", claude_keychain_service(dir));
        child.stdin.take().ok_or("security 没有标准输入")?.write_all(line.as_bytes()).map_err(|e| e.to_string())?;
        let out = child.wait_with_output().map_err(|e| e.to_string())?;
        let err = String::from_utf8_lossy(&out.stderr);
        if !out.status.success() || err.contains("rror") {
            return Err(format!("写入钥匙串失败：{}", err.trim()));
        }
        Ok(())
    }
    #[cfg(not(target_os = "macos"))]
    {
        let d = match dir {
            Some(d) => d.to_path_buf(),
            None => home().ok_or("找不到用户目录")?.join(".claude"),
        };
        write_private(&d.join(".credentials.json"), text.as_bytes())
    }
}

fn claude_cred_delete(dir: Option<&Path>) {
    #[cfg(target_os = "macos")]
    {
        let _ = Command::new("/usr/bin/security")
            .args(["delete-generic-password", "-s", &claude_keychain_service(dir)])
            .stdout(Stdio::null())
            .stderr(Stdio::null())
            .status();
    }
    let d = match dir {
        Some(d) => Some(d.to_path_buf()),
        None => home().map(|h| h.join(".claude")),
    };
    if let Some(d) = d {
        let _ = std::fs::remove_file(d.join(".credentials.json"));
    }
}

/// Claude 的 .claude.json：工作区在 <目录>/.claude.json，默认在 ~/.claude.json。
fn claude_state_path(dir: Option<&Path>) -> Option<PathBuf> {
    match dir {
        Some(d) => Some(d.join(".claude.json")),
        None => Some(home()?.join(".claude.json")),
    }
}

fn claude_email(dir: Option<&Path>) -> String {
    claude_state_path(dir)
        .and_then(|p| read_json(&p))
        .and_then(|v| v["oauthAccount"]["emailAddress"].as_str().map(str::to_lowercase))
        .unwrap_or_default()
}

fn codex_auth_path(dir: Option<&Path>) -> Option<PathBuf> {
    match dir {
        Some(d) => Some(d.join("auth.json")),
        None => Some(home()?.join(".codex").join("auth.json")),
    }
}

// ---- 扫描 ----

#[derive(Serialize, Default)]
struct WsState {
    kind: String,
    email: String,
    dir: String,
    logged_in: bool,
    #[serde(skip_serializing_if = "String::is_empty")]
    login_email: String,
    default: bool,
    running: i64,
    #[serde(skip_serializing_if = "String::is_empty")]
    error: String,
}

#[derive(Serialize, Default)]
struct DefaultState {
    #[serde(skip_serializing_if = "String::is_empty")]
    email: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    owner: String,
    running: i64,
    logged_in: bool,
}

#[derive(Serialize)]
struct Token {
    kind: String,
    email: String,
    token: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    account_id: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    refresh_token: String,
    #[serde(skip_serializing_if = "is_zero")]
    expires_at: i64,
    #[serde(skip_serializing_if = "String::is_empty")]
    id_token: String,
}

fn is_zero(v: &i64) -> bool {
    *v == 0
}

/// 工作区里读出的凭据。
#[derive(Default)]
struct Cred {
    access: String,
    refresh: String,
    expires_at: i64,
    id_token: String,
    account_id: String,
}

impl Cred {
    fn token(self, kind: &str, email: &str) -> Token {
        Token {
            kind: kind.into(),
            email: email.into(),
            token: self.access,
            account_id: self.account_id,
            refresh_token: self.refresh,
            expires_at: self.expires_at,
            id_token: self.id_token,
        }
    }
}

/// 正在运行的 claude / codex 进程：(可执行文件名, 环境里的工作区目录)。
/// macOS 用 ps -E 读同一用户进程的环境变量；Windows 读不到别的进程的环境，只能数进程。
/// TODO(verify:V-multi-claude-concurrent) claude 可能以 node 运行，按前两个参数里有没有 claude 判断。
struct Procs {
    known: bool,
    list: Vec<(String, String)>,
}

fn procs() -> Procs {
    #[cfg(target_os = "macos")]
    {
        let Ok(out) = Command::new("/bin/ps").args(["-Eww", "-axo", "command="]).output() else {
            return Procs { known: false, list: Vec::new() };
        };
        let mut list = Vec::new();
        for line in String::from_utf8_lossy(&out.stdout).lines() {
            let tokens: Vec<&str> = line.split_whitespace().collect();
            let exe = tokens
                .iter()
                .take(2)
                .map(|t| t.rsplit('/').next().unwrap_or(t))
                .find(|n| *n == CLAUDE || *n == CODEX)
                .map(str::to_string);
            let Some(exe) = exe else { continue };
            let var = format!("{}=", env_name(&exe));
            let dir = tokens.iter().find_map(|t| t.strip_prefix(var.as_str())).unwrap_or_default().to_string();
            list.push((exe, dir));
        }
        Procs { known: true, list }
    }
    #[cfg(not(target_os = "macos"))]
    {
        let mut list = Vec::new();
        #[cfg(windows)]
        if let Ok(out) = hidden("tasklist.exe").args(["/FO", "CSV", "/NH"]).output() {
            for line in String::from_utf8_lossy(&out.stdout).lines() {
                let name = line.split(',').next().unwrap_or_default().trim_matches('"').to_lowercase();
                if name == "claude.exe" || name == "codex.exe" {
                    list.push((name.trim_end_matches(".exe").to_string(), String::new()));
                }
            }
        }
        Procs { known: false, list }
    }
}

impl Procs {
    fn count(&self, kind: &str, dir: Option<&Path>) -> i64 {
        if !self.known {
            // 区分不了工作区：默认目录按全部进程算（宁可多提示），工作区显示不知道。
            return if dir.is_none() { self.list.iter().filter(|(k, _)| k == kind).count() as i64 } else { -1 };
        }
        let want = dir.map(|d| d.display().to_string()).unwrap_or_default();
        self.list.iter().filter(|(k, d)| k == kind && *d == want).count() as i64
    }
}

fn scan() -> Value {
    let p = procs();
    let mut workspaces = Vec::new();
    let mut tokens = Vec::new();
    let mut defaults = serde_json::Map::new();
    for kind in [CLAUDE, CODEX] {
        let owner = default_owner(kind).map(|(e, _)| e).unwrap_or_default();
        for (email, dir) in list(kind) {
            let mut s = WsState { kind: kind.into(), email: email.clone(), dir: dir.display().to_string(), ..Default::default() };
            s.default = is_default(&dir);
            // 设为默认时凭据在默认目录，状态看默认目录。
            let src = if s.default { None } else { Some(dir.as_path()) };
            let (logged, login_email, tok) = read_login(kind, src);
            (s.logged_in, s.login_email) = (logged, login_email);
            s.running = if s.default { p.count(kind, None) } else { p.count(kind, Some(&dir)) };
            if let Some(c) = tok {
                tokens.push(c.token(kind, &email));
            }
            workspaces.push(s);
        }
        let (logged, email, tok) = read_login(kind, None);
        if owner.is_empty() {
            if let Some(c) = tok.filter(|c| !c.access.is_empty()) {
                tokens.push(c.token(kind, ""));
            }
        }
        let d = DefaultState { email, owner, running: p.count(kind, None), logged_in: logged };
        defaults.insert(kind.into(), serde_json::to_value(d).unwrap_or_default());
    }
    json!({
        "workspaces": workspaces,
        "defaults": defaults,
        "cli_dir": cli_dir().map(|d| d.display().to_string()).unwrap_or_default(),
        "tokens": tokens,
    })
}

/// 读登录状态：(是否登录, 登录的账号, 凭据)。dir 为空表示默认目录。
fn read_login(kind: &str, dir: Option<&Path>) -> (bool, String, Option<Cred>) {
    let s = |v: &Value| v.as_str().unwrap_or_default().to_string();
    let c = if kind == CLAUDE {
        let cred = claude_cred_read(dir).and_then(|s| serde_json::from_str::<Value>(&s).ok());
        let o = cred.as_ref().map(|c| c["claudeAiOauth"].clone()).unwrap_or_default();
        Cred { access: s(&o["accessToken"]), refresh: s(&o["refreshToken"]), expires_at: o["expiresAt"].as_i64().unwrap_or(0), ..Default::default() }
    } else {
        let auth = codex_auth_path(dir).and_then(|p| read_json(&p)).unwrap_or_default();
        let t = &auth["tokens"];
        Cred {
            access: s(&t["access_token"]),
            refresh: s(&t["refresh_token"]),
            id_token: s(&t["id_token"]),
            account_id: s(&t["account_id"]),
            expires_at: 0,
        }
    };
    // 只拿 Access Token 的客户端（ADR-071）工作区里没有 refresh token，有访问令牌也算登录了。
    let logged = !c.refresh.is_empty() || !c.access.is_empty();
    let email = if kind == CLAUDE { claude_email(dir) } else { jwt_email(&c.id_token) };
    let tok = (!c.access.is_empty() || !c.refresh.is_empty()).then_some(c);
    (logged, email, tok)
}

// ---- 与后台服务通信 ----

fn agent(timeout: Duration) -> ureq::Agent {
    ureq::AgentBuilder::new().timeout(timeout).build()
}

fn request(method: &str, path: &str, body: Option<Value>) -> Result<Value, String> {
    let token = read_token().ok_or("读不到访问令牌")?;
    let req = agent(Duration::from_secs(15))
        .request(method, &format!("{SERVICE}/api/v1/desktop/workspaces{path}"))
        .set("Authorization", &format!("Bearer {token}"));
    let result = match body {
        Some(b) => req.set("Content-Type", "application/json").send_string(&b.to_string()),
        None => req.call(),
    };
    match result {
        Ok(resp) => resp.into_json().map_err(|e| e.to_string()),
        Err(ureq::Error::Status(_, resp)) => {
            let v: Value = resp.into_json().unwrap_or_default();
            Err(v["message"].as_str().unwrap_or("操作失败").to_string())
        }
        Err(e) => Err(e.to_string()),
    }
}

fn report_now() {
    if let Err(e) = request("PUT", "/report", Some(scan())) {
        log!("回报工作区状态失败：{e}");
    }
}

struct TickState {
    last_scan: Option<Instant>,
    handled: String,
    busy: bool,
}

static STATE: Mutex<TickState> = Mutex::new(TickState { last_scan: None, handled: String::new(), busy: false });

/// 每次读到状态时调用：有新操作就执行，否则定期扫描。都在后台线程里做，一次只做一件。
pub fn tick(action: Option<String>) {
    let mut st = STATE.lock().unwrap();
    if st.busy {
        return;
    }
    let id = action.filter(|id| *id != st.handled);
    let due = st.last_scan.is_none_or(|t| t.elapsed() > SCAN_EVERY);
    if id.is_none() && !due {
        return;
    }
    if let Some(id) = &id {
        st.handled = id.clone();
    }
    st.busy = true;
    drop(st);
    std::thread::spawn(move || {
        if let Some(id) = id {
            run_action(&id);
        }
        install_cli();
        report_now();
        let mut st = STATE.lock().unwrap();
        st.busy = false;
        st.last_scan = Some(Instant::now());
    });
}

#[derive(Deserialize, Default)]
struct Action {
    action: String,
    kind: String,
    #[serde(default)]
    email: String,
    #[serde(default)]
    cwd: String,
    #[serde(default)]
    credential: Value,
}

fn run_action(id: &str) {
    let a: Action = match request("POST", &format!("/actions/{id}/claim"), Some(json!({}))) {
        Ok(v) => serde_json::from_value(v).unwrap_or_default(),
        Err(e) => {
            log!("认领工作区操作失败：{e}");
            return;
        }
    };
    if a.kind != CLAUDE && a.kind != CODEX {
        return;
    }
    let result = match a.action.as_str() {
        "login" if a.kind == CODEX => codex_login(&a.email),
        "app_login" if a.kind == CODEX => crate::chatgpt_app::login(&a.email),
        "desktop_app" if a.kind == CLAUDE => crate::claude_app::open(),
        "terminal" => open_terminal(&a.kind, &a.email, &a.cwd, false),
        "launch" => open_terminal(&a.kind, &a.email, &a.cwd, true),
        "set_default" => set_default(&a.kind, &a.email),
        "adopt_default" => adopt_default(&a.kind, &a.email),
        "delete" => delete(&a.kind, &a.email),
        "import" => import_cred(&a.kind, &a.email, &a.credential),
        _ => Err("不支持的操作".into()),
    };
    if let Err(e) = &result {
        log!("工作区操作 {} {} {} 失败：{e}", a.action, a.kind, a.email);
    }
    let body = json!({ "ok": result.is_ok(), "error": result.err().unwrap_or_default() });
    if let Err(e) = request("POST", &format!("/actions/{id}/result"), Some(body)) {
        log!("回报工作区操作结果失败：{e}");
    }
}

// ---- Codex 登录（BXC-F0-036）----

/// 找命令：先找 PATH，再找常见的安装位置（从 Dock、开始菜单启动的应用 PATH 不完整）。
pub fn find_command(name: &str) -> Option<PathBuf> {
    let home = home();
    let names: Vec<String> = if cfg!(windows) { vec![format!("{name}.exe"), format!("{name}.cmd")] } else { vec![name.to_string()] };
    let mut dirs: Vec<PathBuf> = std::env::var_os("PATH").map(|p| std::env::split_paths(&p).collect()).unwrap_or_default();
    if let Some(h) = &home {
        dirs.push(h.join(".local").join("bin"));
        dirs.push(h.join(".claude").join("local"));
        dirs.push(h.join(".npm-global").join("bin"));
    }
    if cfg!(windows) {
        if let Some(a) = std::env::var_os("APPDATA") {
            dirs.push(PathBuf::from(a).join("npm"));
        }
        if let Some(l) = std::env::var_os("LOCALAPPDATA") {
            dirs.push(PathBuf::from(l).join("Microsoft").join("WinGet").join("Links"));
        }
    } else {
        dirs.push(PathBuf::from("/opt/homebrew/bin"));
        dirs.push(PathBuf::from("/usr/local/bin"));
    }
    dirs.iter().flat_map(|d| names.iter().map(move |n| d.join(n))).find(|p| p.is_file())
}

pub(crate) static CODEX_LOGIN: Mutex<()> = Mutex::new(());

/// 在工作区里执行 codex login：codex 自己打开浏览器，本地回调固定 127.0.0.1:1455，所以一次只登录一个。
/// OpenAI 发到邮箱的验证码在账号页的「邮件」里看（BXC-F0-045）。
fn codex_login(email: &str) -> Result<(), String> {
    let _guard = CODEX_LOGIN.try_lock().map_err(|_| "另一个 Codex 账号正在登录，请等它完成")?;
    let codex = find_command(CODEX).ok_or("找不到 codex 命令，请先安装 Codex CLI")?;
    let dir = ensure(CODEX, email)?;
    if is_default(&dir) {
        return Err("这个账号已设为默认，请在普通终端里运行 codex login".into());
    }
    let mut child = hidden_path(&codex)
        .arg("login")
        .env("CODEX_HOME", &dir)
        .stdin(Stdio::null())
        .stdout(Stdio::null())
        .stderr(Stdio::piped())
        .spawn()
        .map_err(|e| format!("启动 codex 失败：{e}"))?;
    let start = Instant::now();
    let status = loop {
        if let Some(s) = child.try_wait().map_err(|e| e.to_string())? {
            break s;
        }
        if start.elapsed() > LOGIN_WAIT {
            let _ = child.kill();
            let _ = child.wait();
            return Err("10 分钟内没有完成登录".into());
        }
        std::thread::sleep(Duration::from_secs(1));
    };
    if !status.success() {
        let mut err = String::new();
        if let Some(mut e) = child.stderr.take() {
            use std::io::Read;
            let _ = e.read_to_string(&mut err);
        }
        // TODO(verify:V-multi-codex-login) Windows 上 1455 落在保留端口段时报 10013。
        if err.contains("10013") || err.contains("1455") {
            return Err("本机登录回调端口 1455 不可用（可能被占用或在系统保留端口段），请关闭其他正在登录的 codex，或在终端里用 codex login --device-auth".into());
        }
        return Err(format!("codex login 没有完成（退出码 {:?}）", status.code()));
    }
    let (logged, got, _) = read_login(CODEX, Some(&dir));
    if !logged {
        return Err("登录后没有找到凭据".into());
    }
    if !got.is_empty() && !got.eq_ignore_ascii_case(email) {
        return Err(format!("登录的是 {got}，不是 {email}"));
    }
    Ok(())
}

// ---- 打开隔离终端（BXC-F0-037）----

#[cfg_attr(windows, allow(dead_code))]
fn sh_quote(s: &str) -> String {
    format!("'{}'", s.replace('\'', "'\\''"))
}

/// 打开系统终端：注入工作区的 CLAUDE_CONFIG_DIR / CODEX_HOME（设为默认的账号和 email 为空时不注入），只影响这个终端。
/// run 为 true 时是一键启动：终端打开后直接运行 claude / codex（退出后终端留着）。
fn open_terminal(kind: &str, email: &str, cwd: &str, run: bool) -> Result<(), String> {
    let label = if kind == CODEX { "Codex" } else { "Claude" };
    let cli = if run {
        let name = if kind == CODEX { CODEX } else { CLAUDE };
        Some(find_command(name).ok_or_else(|| format!("找不到 {name} 命令，请先安装"))?)
    } else {
        None
    };
    let mut env: Option<PathBuf> = None;
    let title = if email.is_empty() {
        format!("{label} · 默认账号")
    } else {
        let dir = ensure(kind, email)?;
        if !is_default(&dir) {
            env = Some(dir);
        }
        format!("{label} · {email}")
    };
    let cwd = if cwd.is_empty() { home().map(|h| h.display().to_string()).unwrap_or_default() } else { cwd.to_string() };
    #[cfg(target_os = "macos")]
    {
        let mut script = String::new();
        if let Some(d) = &env {
            script.push_str(&format!("export {}={}; ", env_name(kind), sh_quote(&d.display().to_string())));
        } else {
            script.push_str(&format!("unset {}; ", env_name(kind)));
        }
        script.push_str(&format!("cd {} 2>/dev/null; printf '\\033]0;%s\\007' {}; clear", sh_quote(&cwd), sh_quote(&title)));
        if let Some(c) = &cli {
            script.push_str(&format!("; {}", sh_quote(&c.display().to_string())));
        }
        let apple = script.replace('\\', "\\\\").replace('"', "\\\"");
        let status = Command::new("/usr/bin/osascript")
            .args(["-e", &format!("tell application \"Terminal\" to do script \"{apple}\""), "-e", "tell application \"Terminal\" to activate"])
            .status()
            .map_err(|e| format!("打开终端失败：{e}"))?;
        if !status.success() {
            return Err("打开终端失败：请在「系统设置 → 隐私与安全性 → 自动化」里允许 Banbu Connect Lite 控制「终端」".into());
        }
        Ok(())
    }
    #[cfg(windows)]
    {
        use std::os::windows::process::CommandExt;
        const CREATE_NEW_CONSOLE: u32 = 0x0000_0010;
        let ps_title = title.replace('\'', "''");
        let mut ps = format!("$Host.UI.RawUI.WindowTitle = '{ps_title}'");
        if let Some(c) = &cli {
            ps.push_str(&format!("; & '{}'", c.display().to_string().replace('\'', "''")));
        }
        let mut c = Command::new("powershell.exe");
        c.args(["-NoExit", "-NoLogo", "-Command", &ps])
            .current_dir(&cwd)
            .creation_flags(CREATE_NEW_CONSOLE);
        match &env {
            Some(d) => c.env(env_name(kind), d),
            None => c.env_remove(env_name(kind)),
        };
        c.spawn().map(|_| ()).map_err(|e| format!("打开终端失败：{e}"))
    }
    #[cfg(not(any(windows, target_os = "macos")))]
    {
        let _ = (env, title, cwd, cli, sh_quote(""));
        Err("这个系统不支持".into())
    }
}

/// 工作区的登录状态：(是否登录, 登录的账号)。
pub(crate) fn login_state(kind: &str, dir: &Path) -> (bool, String) {
    let (logged, email, _) = read_login(kind, Some(dir));
    (logged, email)
}

/// 某个工作区正在运行的进程数（区分不了工作区的系统上返回 -1）。
pub(crate) fn running(kind: &str, dir: &Path) -> i64 {
    procs().count(kind, Some(dir))
}

// ---- 设为默认（BXC-F0-046）----

fn running_default(kind: &str) -> Result<(), String> {
    let n = procs().count(kind, None);
    if n > 0 {
        let name = if kind == CODEX { "codex（含 Codex 桌面应用的 app-server）" } else { "claude" };
        return Err(format!("有 {n} 个 {name} 进程在使用默认账号，请先退出它们再切换，否则会和新账号抢刷新登录"));
    }
    Ok(())
}

fn backup_dir() -> Option<PathBuf> {
    Some(root()?.join("backup"))
}

/// 把 Claude 凭据和 .claude.json 的 oauthAccount 从 from 移到 to（None 表示默认目录）。
fn claude_move(from: Option<&Path>, to: Option<&Path>) -> Result<(), String> {
    let cred = claude_cred_read(from).ok_or("没有找到要移动的 Claude 凭据")?;
    let account = claude_state_path(from).and_then(|p| read_json(&p)).map(|v| v["oauthAccount"].clone()).unwrap_or(Value::Null);
    claude_cred_write(to, &cred)?;
    if let Some(dst) = claude_state_path(to) {
        let mut state = read_json(&dst).unwrap_or_else(|| json!({}));
        if dst.exists() {
            let _ = std::fs::copy(&dst, dst.with_extension("json.banbu-bak"));
        }
        if let Some(m) = state.as_object_mut() {
            if account.is_null() {
                m.remove("oauthAccount");
            } else {
                m.insert("oauthAccount".into(), account);
            }
        }
        write_private(&dst, serde_json::to_string_pretty(&state).unwrap_or_default().as_bytes())?;
    }
    claude_cred_delete(from);
    if let Some(src) = claude_state_path(from).and_then(|p| read_json(&p).map(|v| (p, v))) {
        let (p, mut v) = src;
        if let Some(m) = v.as_object_mut() {
            m.remove("oauthAccount");
        }
        let _ = write_private(&p, serde_json::to_string_pretty(&v).unwrap_or_default().as_bytes());
    }
    Ok(())
}

fn codex_move(from: Option<&Path>, to: Option<&Path>) -> Result<(), String> {
    let src = codex_auth_path(from).ok_or("找不到目录")?;
    let dst = codex_auth_path(to).ok_or("找不到目录")?;
    let body = std::fs::read(&src).map_err(|_| "没有找到要移动的 Codex 凭据".to_string())?;
    write_private(&dst, &body)?;
    std::fs::remove_file(&src).map_err(|e| e.to_string())
}

fn move_cred(kind: &str, from: Option<&Path>, to: Option<&Path>) -> Result<(), String> {
    if kind == CODEX {
        codex_move(from, to)
    } else {
        claude_move(from, to)
    }
}

fn set_default(kind: &str, email: &str) -> Result<(), String> {
    let dir = workspace_dir(kind, email).filter(|d| d.join(MARKER).exists()).ok_or("这个账号在本机还没有登录过")?;
    if is_default(&dir) {
        return Ok(());
    }
    if !read_login(kind, Some(&dir)).0 {
        return Err("这个账号在本机还没有登录，先登录再设为默认".into());
    }
    running_default(kind)?;
    // 1. 默认目录里原来的凭据：属于另一个工作区的移回去；是用户自己的，备份。
    if let Some((_, old)) = default_owner(kind) {
        move_cred(kind, None, Some(&old))?;
        let _ = std::fs::remove_file(old.join(DEFAULT_MARKER));
    } else if read_login(kind, None).0 {
        let bak = backup_dir().ok_or("找不到数据目录")?.join(format!("{kind}-default"));
        std::fs::create_dir_all(&bak).map_err(|e| e.to_string())?;
        move_cred(kind, None, Some(&bak)).map_err(|e| format!("备份默认账号失败：{e}"))?;
        log!("默认目录原来的 {kind} 账号已备份到 {}", bak.display());
    }
    // 2. 选定账号移到默认目录。
    move_cred(kind, Some(&dir), None)?;
    std::fs::write(dir.join(DEFAULT_MARKER), b"").map_err(|e| e.to_string())?;
    log!("已把 {kind} 账号 {email} 设为默认");
    Ok(())
}

/// 用本机已登录的：默认目录里登录的正是这个账号时（用户自己用 claude / codex 登录过，注册方式不限），
/// 把默认目录交给这个账号的工作区——建工作区、打上默认标记，凭据留在默认目录不动。之后扫描把默认目录的登录
/// 算作这个账号，后台服务按「本机登录」上报管理服务（ADR-061）。不复制凭据：refresh token 是一次性的，
/// 两处各拿一份会互相顶掉。
fn adopt_default(kind: &str, email: &str) -> Result<(), String> {
    let (logged, login_email, _) = read_login(kind, None);
    if !logged {
        return Err(format!("本机默认目录里没有登录 {kind}，先在普通终端里运行 {kind} 登录"));
    }
    if login_email.is_empty() {
        return Err("读不到本机默认目录里登录的邮箱，无法确认是不是这个账号".into());
    }
    if !login_email.eq_ignore_ascii_case(email) {
        return Err(format!("本机默认目录里登录的是 {login_email}，不是 {email}"));
    }
    match default_owner(kind) {
        Some((owner, _)) if owner.eq_ignore_ascii_case(email) => return Ok(()),
        Some((owner, _)) => return Err(format!("默认目录已经交给了 {owner} 的工作区")),
        None => {}
    }
    if let Some(dir) = workspace_dir(kind, email).filter(|d| d.join(MARKER).exists()) {
        if read_login(kind, Some(&dir)).0 {
            return Err("这个账号的工作区已经登录过，不需要再用本机默认目录的登录".into());
        }
    }
    let dir = ensure(kind, email)?;
    std::fs::write(dir.join(DEFAULT_MARKER), b"").map_err(|e| e.to_string())?;
    log!("已把本机默认目录的 {kind} 登录交给 {email} 的工作区");
    Ok(())
}

// ---- 写入管理服务托管的凭据（ADR-061）----

/// 把托管的凭据写进账号的工作区；工作区设为默认时写进默认目录。
/// Claude：钥匙串（macOS）或 .credentials.json 里的 claudeAiOauth，保留原有的其他字段；.claude.json 的
/// oauthAccount 换成这个账号（邮箱不同时整个替换，claude 启动后会自己补全资料）。
/// Codex：auth.json，auth_mode 必须是 chatgpt，不能带 OPENAI_API_KEY。
/// 只拿 Access Token 时（ADR-071）没有 refresh token：Claude 不写 refreshToken，到期前由管理服务续期后重新写入；
/// Codex 的 refresh_token 写空串、last_refresh 写当前时间，codex 8 天内不会自己去刷新，管理服务在这之前续期。
/// TODO(verify:V-ai-cred-import) 格式参照 cockpit-tools 读出的做法，未在真机确认 claude / codex 能直接使用。
/// TODO(verify:V-ai-token-only) 未在真机确认 claude / codex 只有访问令牌时能正常使用、续期写入后正在运行的进程能接上。
fn import_cred(kind: &str, email: &str, cred: &Value) -> Result<(), String> {
    let s = |k: &str| cred[k].as_str().unwrap_or_default().to_string();
    let (access, refresh) = (s("access_token"), s("refresh_token"));
    if access.is_empty() {
        return Err("凭据不完整：缺少访问令牌".into());
    }
    if kind == CODEX && s("id_token").is_empty() {
        return Err("凭据不完整：缺少 id_token".into());
    }
    let dir = ensure(kind, email)?;
    let target = if is_default(&dir) { None } else { Some(dir.as_path()) };
    if kind == CODEX {
        let path = codex_auth_path(target).ok_or("找不到目录")?;
        let body = json!({
            "OPENAI_API_KEY": Value::Null,
            "auth_mode": "chatgpt",
            "tokens": {
                "id_token": s("id_token"),
                "access_token": access,
                "refresh_token": refresh,
                "account_id": s("account_id"),
            },
            "last_refresh": utc_now(),
        });
        write_private(&path, serde_json::to_string_pretty(&body).unwrap_or_default().as_bytes())?;
        log!("已把托管的 Codex 凭据写进 {email} 的工作区");
        return Ok(());
    }
    let mut full = claude_cred_read(target).and_then(|s| serde_json::from_str::<Value>(&s).ok()).unwrap_or_else(|| json!({}));
    if !full.is_object() {
        full = json!({});
    }
    let mut o = full["claudeAiOauth"].clone();
    if !o.is_object() {
        o = json!({ "scopes": ["user:inference", "user:profile", "user:sessions:claude_code"], "subscriptionType": Value::Null });
    }
    o["accessToken"] = json!(access);
    if refresh.is_empty() {
        if let Some(m) = o.as_object_mut() {
            m.remove("refreshToken");
        }
    } else {
        o["refreshToken"] = json!(refresh);
    }
    o["expiresAt"] = json!(cred["expires_at"].as_i64().unwrap_or(0));
    full["claudeAiOauth"] = o;
    claude_cred_write(target, &full.to_string())?;
    if let Some(p) = claude_state_path(target) {
        let mut state = read_json(&p).unwrap_or_else(|| json!({}));
        let same = state["oauthAccount"]["emailAddress"].as_str().is_some_and(|e| e.eq_ignore_ascii_case(email));
        if !same {
            if let Some(m) = state.as_object_mut() {
                m.insert("oauthAccount".into(), json!({ "emailAddress": email }));
            }
            write_private(&p, serde_json::to_string_pretty(&state).unwrap_or_default().as_bytes())?;
        }
    }
    log!("已把托管的 Claude 凭据写进 {email} 的工作区");
    Ok(())
}

/// 当前时间（RFC 3339，UTC），不为此引入日期库。
fn utc_now() -> String {
    let secs = std::time::SystemTime::now().duration_since(std::time::UNIX_EPOCH).map(|d| d.as_secs()).unwrap_or(0) as i64;
    rfc3339(secs)
}

fn rfc3339(secs: i64) -> String {
    let (days, rem) = (secs.div_euclid(86400), secs.rem_euclid(86400));
    // 公历换算（Howard Hinnant 的 civil_from_days）。
    let z = days + 719468;
    let era = z.div_euclid(146097);
    let doe = z - era * 146097;
    let yoe = (doe - doe / 1460 + doe / 36524 - doe / 146096) / 365;
    let doy = doe - (365 * yoe + yoe / 4 - yoe / 100);
    let mp = (5 * doy + 2) / 153;
    let d = doy - (153 * mp + 2) / 5 + 1;
    let m = if mp < 10 { mp + 3 } else { mp - 9 };
    let y = yoe + era * 400 + if m <= 2 { 1 } else { 0 };
    format!("{y:04}-{m:02}-{d:02}T{:02}:{:02}:{:02}Z", rem / 3600, rem % 3600 / 60, rem % 60)
}

// ---- 删除工作区 ----

fn delete(kind: &str, email: &str) -> Result<(), String> {
    let Some(dir) = workspace_dir(kind, email).filter(|d| d.exists()) else { return Ok(()) };
    if is_default(&dir) {
        return Err("这个账号是默认账号，先把别的账号设为默认再删除".into());
    }
    let n = procs().count(kind, Some(&dir));
    if n > 0 {
        return Err(format!("这个工作区有 {n} 个进程在运行，先退出它们"));
    }
    if kind == CLAUDE {
        claude_cred_delete(Some(&dir));
    }
    std::fs::remove_dir_all(&dir).map_err(|e| format!("删除工作区失败：{e}"))
}

// ---- 命令行入口（BXC-F0-038）----

fn cli_dir() -> Option<PathBuf> {
    Some(token_path()?.parent()?.join("bin"))
}

const SH_SCRIPT: &str = r#"#!/bin/sh
# Banbu Connect 生成：按账号工作区运行 __CMD__。用法：banbu-__CMD__ <账号邮箱或开头几个字> [__CMD__ 的参数…]
root="__ROOT__"
if [ -z "$1" ]; then
  echo "用法：banbu-__CMD__ <账号> [参数…]，可用的账号："
  for d in "$root"/*/; do sed -n 's/.*"email":"\([^"]*\)".*/  \1/p' "$d.banbu-workspace.json" 2>/dev/null; done
  exit 1
fi
want="$1"; shift
for d in "$root"/*/; do
  email=$(sed -n 's/.*"email":"\([^"]*\)".*/\1/p' "$d.banbu-workspace.json" 2>/dev/null)
  case "$email" in "$want"*)
    if [ -e "$d.banbu-default" ]; then unset __ENV__; else export __ENV__="${d%/}"; fi
    exec __CMD__ "$@" ;;
  esac
done
echo "没有找到账号：$want" >&2
exit 1
"#;

const PS_SCRIPT: &str = r#"# Banbu Connect 生成：按账号工作区运行 __CMD__。用法：banbu-__CMD__ <账号邮箱或开头几个字> [__CMD__ 的参数…]
$root = '__ROOT__'
$ws = Get-ChildItem -Directory $root -ErrorAction SilentlyContinue | ForEach-Object {
  $m = Join-Path $_.FullName '.banbu-workspace.json'
  if (Test-Path $m) { [pscustomobject]@{ Email = (Get-Content $m -Raw | ConvertFrom-Json).email; Dir = $_.FullName } }
}
if ($args.Count -eq 0) { "用法：banbu-__CMD__ <账号> [参数…]，可用的账号："; $ws | ForEach-Object { '  ' + $_.Email }; exit 1 }
$w = $ws | Where-Object { $_.Email -like ($args[0] + '*') } | Select-Object -First 1
if (-not $w) { Write-Error ('没有找到账号：' + $args[0]); exit 1 }
if (Test-Path (Join-Path $w.Dir '.banbu-default')) { Remove-Item Env:__ENV__ -ErrorAction SilentlyContinue } else { $env:__ENV__ = $w.Dir }
$rest = @($args | Select-Object -Skip 1)
& __CMD__ @rest
exit $LASTEXITCODE
"#;

/// 生成 banbu-claude / banbu-codex（内容没变时不重写）。
fn install_cli() {
    let (Some(dir), Some(root)) = (cli_dir(), root()) else { return };
    let _ = std::fs::create_dir_all(&dir);
    for kind in [CLAUDE, CODEX] {
        let ws_root = root.join(kind_dir(kind)).display().to_string();
        let fill = |t: &str| t.replace("__CMD__", kind).replace("__ENV__", env_name(kind)).replace("__ROOT__", &ws_root);
        let mut files: Vec<(PathBuf, String)> = Vec::new();
        if cfg!(windows) {
            files.push((dir.join(format!("banbu-{kind}.ps1")), fill(PS_SCRIPT)));
            files.push((
                dir.join(format!("banbu-{kind}.cmd")),
                format!("@powershell -NoProfile -ExecutionPolicy Bypass -File \"%~dp0banbu-{kind}.ps1\" %*\r\n"),
            ));
        } else {
            files.push((dir.join(format!("banbu-{kind}")), fill(SH_SCRIPT)));
        }
        for (path, body) in files {
            if std::fs::read_to_string(&path).is_ok_and(|old| old == body) {
                continue;
            }
            if std::fs::write(&path, &body).is_ok() {
                #[cfg(unix)]
                {
                    use std::os::unix::fs::PermissionsExt;
                    let _ = std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o755));
                }
            }
        }
    }
}

// ---- 工具 ----

#[cfg(windows)]
pub(crate) fn hidden(program: &str) -> Command {
    use std::os::windows::process::CommandExt;
    let mut c = Command::new(program);
    c.creation_flags(0x0800_0000); // CREATE_NO_WINDOW
    c
}

fn hidden_path(path: &Path) -> Command {
    #[cfg(windows)]
    {
        use std::os::windows::process::CommandExt;
        let mut c = Command::new(path);
        c.creation_flags(0x0800_0000);
        c
    }
    #[cfg(not(windows))]
    Command::new(path)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn keychain_names() {
        assert_eq!(claude_keychain_service(None), "Claude Code-credentials");
        // sha256("/Users/a/ws") 的前 8 位十六进制。
        let sum = Sha256::digest(b"/Users/a/ws");
        let want = format!("Claude Code-credentials-{:02x}{:02x}{:02x}{:02x}", sum[0], sum[1], sum[2], sum[3]);
        assert_eq!(claude_keychain_service(Some(Path::new("/Users/a/ws"))), want);
    }

    #[test]
    fn codex_email_from_id_token() {
        use base64::Engine;
        let enc = |s: &str| base64::engine::general_purpose::URL_SAFE_NO_PAD.encode(s);
        let tok = format!("{}.{}.x", enc("{}"), enc(r#"{"https://api.openai.com/profile":{"email":"A@x.com"}}"#));
        assert_eq!(jwt_email(&tok), "a@x.com");
        assert_eq!(jwt_email("bad"), "");
    }

    #[test]
    fn rfc3339_format() {
        assert_eq!(rfc3339(0), "1970-01-01T00:00:00Z");
        assert_eq!(rfc3339(1_795_000_000), "2026-11-18T11:06:40Z");
        assert_eq!(rfc3339(951_782_400), "2000-02-29T00:00:00Z");
    }

    #[test]
    fn shell_quoting() {
        assert_eq!(sh_quote("it's"), "'it'\\''s'");
    }

    #[test]
    fn process_counts() {
        let p = Procs { known: true, list: vec![("claude".into(), "/w/a".into()), ("claude".into(), String::new()), ("codex".into(), String::new())] };
        assert_eq!(p.count(CLAUDE, Some(Path::new("/w/a"))), 1);
        assert_eq!(p.count(CLAUDE, None), 1);
        assert_eq!(p.count(CODEX, None), 1);
        let unknown = Procs { known: false, list: vec![("claude".into(), String::new()), ("claude".into(), String::new())] };
        assert_eq!(unknown.count(CLAUDE, None), 2);
        assert_eq!(unknown.count(CLAUDE, Some(Path::new("/w/a"))), -1);
    }
}
