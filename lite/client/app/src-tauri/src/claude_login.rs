// 一键发起 Claude Code 登录（BXC-F1-085）。外壳运行在用户会话里，由它执行 claude，
// 凭据才会写到当前用户的目录；后台服务以 SYSTEM/root 运行，不能代劳。
//
// 流程：状态里出现待启动的登录 → 认领 → claude auth login --claudeai --email <邮箱>
// → 经本机服务长轮询 magic link：登录邮件到了、用户在主界面的邮件里点「在浏览器打开」后才拿到，
// 用默认浏览器打开 → Claude Code 自己完成授权和保存凭据 → claude auth status --json 核对邮箱 → 回报。
// 外壳不碰 PKCE、token，也不读写凭据文件。
//
// 另外，不走一键登录、自己手动登录时，主界面也能请求打开邮件里的登录链接：状态里出现 open_link，
// 外壳认领后打开（open_pending）。两种情况都只打开 Claude 的 magic link。

use std::path::PathBuf;
use std::process::{Child, Command, Stdio};
use std::time::{Duration, Instant};

use serde::Deserialize;

use crate::{read_token, workspaces, SERVICE};

/// 只打开这个前缀的链接：链接来自管理服务，不能让它借外壳打开任意网址。
const LINK_PREFIX: &str = "https://claude.ai/magic-link#";
/// 打开链接后等 Claude Code 完成授权的时间。
const AUTHORIZE_WAIT: Duration = Duration::from_secs(10 * 60);

#[derive(Deserialize, Clone, PartialEq, Default, Debug)]
pub struct PendingClaude {
    pub id: String,
    pub email: String,
}

#[derive(Deserialize, Clone, PartialEq, Default, Debug)]
pub struct PendingOpen {
    pub id: String,
}

/// 认领并打开主界面请求的登录链接。
pub fn open_pending(p: PendingOpen) {
    std::thread::spawn(move || {
        let v = match request("POST", &format!("/open-link/{}/claim", p.id), Some(serde_json::json!({})), Duration::from_secs(10)) {
            Ok(v) => v,
            Err(e) => {
                log!("认领待打开的链接失败：{e}");
                return;
            }
        };
        let url = v["url"].as_str().unwrap_or_default();
        if !url.starts_with(LINK_PREFIX) {
            log!("本机服务返回的不是 Claude 登录链接，已忽略");
            return;
        }
        if let Err(e) = open_url(url) {
            log!("{e}");
        }
    });
}

#[derive(Deserialize, Default)]
struct LinkResponse {
    #[serde(default)]
    status: String,
    #[serde(default)]
    link: String,
    #[serde(default)]
    error: String,
}

#[derive(Deserialize, Default)]
struct AuthStatus {
    #[serde(default, rename = "loggedIn")]
    logged_in: bool,
    #[serde(default)]
    email: String,
}

fn agent(timeout: Duration) -> ureq::Agent {
    ureq::AgentBuilder::new().timeout(timeout).build()
}

fn request(method: &str, path: &str, body: Option<serde_json::Value>, timeout: Duration) -> Result<serde_json::Value, String> {
    let token = read_token().ok_or("读不到访问令牌")?;
    let req = agent(timeout)
        .request(method, &format!("{SERVICE}/api/v1/desktop/claude{path}"))
        .set("Authorization", &format!("Bearer {token}"));
    let result = match body {
        Some(b) => req.set("Content-Type", "application/json").send_string(&b.to_string()),
        None => req.call(),
    };
    match result {
        Ok(resp) => resp.into_json().map_err(|e| e.to_string()),
        Err(ureq::Error::Status(_, resp)) => {
            let v: serde_json::Value = resp.into_json().unwrap_or_default();
            Err(v["message"].as_str().unwrap_or("操作失败").to_string())
        }
        Err(e) => Err(e.to_string()),
    }
}

fn report(id: &str, ok: bool, email: &str, error: &str) {
    let body = serde_json::json!({ "ok": ok, "email": email, "error": error });
    if let Err(e) = request("POST", &format!("/login/{id}/result"), Some(body), Duration::from_secs(30)) {
        log!("回报 Claude 登录结果失败：{e}");
    }
}

/// 在后台线程里执行一次登录；认领失败（已被别的外壳实例处理）时直接返回。
pub fn run(p: PendingClaude) {
    std::thread::spawn(move || {
        if let Err(e) = request("POST", &format!("/login/{}/claim", p.id), Some(serde_json::json!({})), Duration::from_secs(10)) {
            log!("认领 Claude 登录失败：{e}");
            return;
        }
        match login(&p) {
            Ok(email) => report(&p.id, true, &email, ""),
            Err(e) => report(&p.id, false, "", &e),
        }
    });
}

// TODO(verify:V-claude-login) 未在真实环境验证：claude auth login 在没有终端（stdin 为空、无窗口）时
// 是否直接打开浏览器并等待回调，而不是等待按键或因没有 TTY 退出。
fn login(p: &PendingClaude) -> Result<String, String> {
    let claude = find_claude().ok_or("找不到 claude 命令，请先安装 Claude Code")?;
    // 登录到这个账号的工作区（S1.9 BXC-F0-034）；账号已设为默认时登录到默认目录。默认的 ~/.claude 不动。
    let dir = workspaces::ensure(workspaces::CLAUDE, &p.email)?;
    let env = (!dir.join(".banbu-default").exists()).then_some(dir);
    let mut cmd = command(&claude);
    if let Some(d) = &env {
        cmd.env("CLAUDE_CONFIG_DIR", d);
    } else {
        cmd.env_remove("CLAUDE_CONFIG_DIR");
    }
    let mut child = cmd
        .args(["auth", "login", "--claudeai", "--email", &p.email])
        .stdin(Stdio::null())
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .spawn()
        .map_err(|e| format!("启动 claude 失败：{e}"))?;

    // 浏览器里已登录同一账号时不会发邮件，Claude Code 直接完成，不必等链接。
    let mut opened = false;
    loop {
        if let Some(status) = child.try_wait().map_err(|e| e.to_string())? {
            if !status.success() {
                return Err(format!("claude auth login 没有完成（退出码 {:?}）", status.code()));
            }
            break;
        }
        if opened {
            wait_exit(&mut child)?;
            break;
        }
        let v = request("GET", &format!("/login/{}/link?wait=20", p.id), None, Duration::from_secs(45));
        let r: LinkResponse = match v {
            Ok(v) => serde_json::from_value(v).unwrap_or_default(),
            Err(e) => {
                kill(&mut child);
                return Err(format!("取登录链接失败：{e}"));
            }
        };
        if !r.link.is_empty() {
            if !r.link.starts_with(LINK_PREFIX) {
                kill(&mut child);
                return Err("管理服务返回的不是 Claude 登录链接，已忽略".into());
            }
            open_url(&r.link)?;
            opened = true;
            continue;
        }
        if r.status == "failed" || r.status == "expired" {
            kill(&mut child);
            return Err(if r.error.is_empty() { "没有取到登录链接".into() } else { r.error });
        }
    }
    verify(&claude, &p.email, env.as_deref())
}

fn wait_exit(child: &mut Child) -> Result<(), String> {
    let start = Instant::now();
    loop {
        if let Some(status) = child.try_wait().map_err(|e| e.to_string())? {
            return if status.success() {
                Ok(())
            } else {
                Err(format!("claude auth login 没有完成（退出码 {:?}）", status.code()))
            };
        }
        if start.elapsed() > AUTHORIZE_WAIT {
            kill(child);
            return Err("10 分钟内没有完成授权".into());
        }
        std::thread::sleep(Duration::from_secs(1));
    }
}

fn kill(child: &mut Child) {
    let _ = child.kill();
    let _ = child.wait();
}

/// 用 claude auth status 核对登录的邮箱（在同一个工作区里）。
fn verify(claude: &PathBuf, want: &str, dir: Option<&std::path::Path>) -> Result<String, String> {
    let mut cmd = command(claude);
    match dir {
        Some(d) => cmd.env("CLAUDE_CONFIG_DIR", d),
        None => cmd.env_remove("CLAUDE_CONFIG_DIR"),
    };
    let out = cmd
        .args(["auth", "status", "--json"])
        .stdin(Stdio::null())
        .stderr(Stdio::null())
        .output()
        .map_err(|e| format!("读取登录状态失败：{e}"))?;
    let s: AuthStatus = serde_json::from_slice(&out.stdout).map_err(|_| "读取登录状态失败".to_string())?;
    if !s.logged_in {
        return Err("Claude Code 没有登录".into());
    }
    if !s.email.eq_ignore_ascii_case(want) {
        return Err(format!("Claude Code 登录的是 {}，不是 {want}", s.email));
    }
    Ok(s.email)
}

#[cfg(windows)]
fn command(path: &PathBuf) -> Command {
    use std::os::windows::process::CommandExt;
    const CREATE_NO_WINDOW: u32 = 0x0800_0000;
    let mut c = Command::new(path);
    c.creation_flags(CREATE_NO_WINDOW);
    c
}

#[cfg(not(windows))]
fn command(path: &PathBuf) -> Command {
    Command::new(path)
}

/// 找 claude：先找 PATH，再找常见的安装位置（从 Dock、开始菜单启动的应用 PATH 不完整）。
fn find_claude() -> Option<PathBuf> {
    if let Some(p) = workspaces::find_command("claude") {
        return Some(p);
    }
    let home = std::env::var_os(if cfg!(windows) { "USERPROFILE" } else { "HOME" }).map(PathBuf::from);
    let names: &[&str] = if cfg!(windows) { &["claude.exe", "claude.cmd"] } else { &["claude"] };
    let mut dirs: Vec<PathBuf> = std::env::var_os("PATH").map(|p| std::env::split_paths(&p).collect()).unwrap_or_default();
    if let Some(h) = &home {
        dirs.push(h.join(".local").join("bin"));
        dirs.push(h.join(".claude").join("local"));
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

fn open_url(url: &str) -> Result<(), String> {
    #[cfg(windows)]
    let r = command(&PathBuf::from("rundll32.exe")).args(["url.dll,FileProtocolHandler", url]).spawn();
    #[cfg(target_os = "macos")]
    let r = Command::new("/usr/bin/open").arg(url).spawn();
    #[cfg(not(any(windows, target_os = "macos")))]
    let r = Command::new("xdg-open").arg(url).spawn();
    r.map(|_| ()).map_err(|e| format!("打开浏览器失败：{e}"))
}
