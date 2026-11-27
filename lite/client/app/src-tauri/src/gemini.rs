// Gemini（Antigravity）账号（S1.7）。外壳运行在用户会话里，做后台服务（SYSTEM/root）做不了的事：
// - 用每个账号固定的浏览器档案打开 Google 页面（BXC-F0-019、BXC-F0-020，ADR-047）：
//   系统 Chrome / Edge 加 --user-data-dir，经严格组的本机入口（--proxy-server）出去，出口固定；不做指纹伪装。
// - 把本机当前账号的凭据写进 agy CLI 和 Antigravity 应用的存储，读回它们自己刷新后的凭据（BXC-F0-021）。

use std::collections::hash_map::DefaultHasher;
use std::hash::{Hash, Hasher};
use std::path::PathBuf;
use std::process::Command;
use std::sync::Mutex;
use std::time::{Duration, Instant};

use serde::{Deserialize, Serialize};
use serde_json::Value;

use tauri::AppHandle;
use tauri_plugin_notification::NotificationExt;

use crate::{gemini_store, read_token, token_path, SERVICE};

#[derive(Deserialize, Clone, PartialEq, Default, Debug)]
pub struct Brief {
    /// "账号:版本"：变化时取凭据写入；"0:0" 删除本机凭据；空表示后台服务还没同步过，什么都不做。
    #[serde(default)]
    pub apply: String,
    /// 待打开的浏览器档案。
    #[serde(default)]
    pub browser: Option<PendingBrowser>,
    /// 一键启动：等凭据写到这个账号后启动 Antigravity 应用。
    #[serde(default)]
    pub launch: Option<LaunchReq>,
    /// 延后的切换（自动轮换、管理员下发）：Antigravity 在运行时，发一次通知，到 until（Unix 秒）前不关应用、不写入。
    #[serde(default)]
    pub defer: Option<DeferReq>,
}

#[derive(Deserialize, Clone, PartialEq, Default, Debug)]
pub struct DeferReq {
    pub until: i64,
    #[serde(default)]
    pub reason: String,
}

#[derive(Deserialize, Clone, PartialEq, Default, Debug)]
pub struct LaunchReq {
    pub id: String,
    pub account_id: i64,
}

#[derive(Deserialize, Clone, PartialEq, Default, Debug)]
pub struct PendingBrowser {
    pub id: String,
}

#[derive(Deserialize, Default)]
struct Launch {
    profile: String,
    url: String,
    #[serde(default)]
    proxy: String,
}

fn agent(timeout: Duration) -> ureq::Agent {
    ureq::AgentBuilder::new().timeout(timeout).build()
}

pub(crate) fn request(method: &str, path: &str, body: Option<serde_json::Value>, timeout: Duration) -> Result<serde_json::Value, String> {
    let token = read_token().ok_or("读不到访问令牌")?;
    let req = agent(timeout)
        .request(method, &format!("{SERVICE}/api/v1/desktop/gemini{path}"))
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

// ---- 浏览器档案（BXC-F0-019）----

/// 只打开 Google 账号的页面：地址来自后台服务，不能让它借外壳打开任意网址。
fn allowed_url(u: &str) -> bool {
    u.starts_with("https://accounts.google.com/") || u.starts_with("https://myaccount.google.com/")
}

/// 只接受本机入口作代理。
fn allowed_proxy(p: &str) -> bool {
    p.is_empty() || p.starts_with("http://127.0.0.1:")
}

/// 认领并打开后台服务请求的浏览器档案。
pub fn open_browser(p: PendingBrowser) {
    std::thread::spawn(move || {
        let v = match request("POST", &format!("/browser/{}/claim", p.id), Some(serde_json::json!({})), Duration::from_secs(10)) {
            Ok(v) => v,
            Err(e) => {
                log!("认领浏览器档案请求失败：{e}");
                return;
            }
        };
        let l: Launch = serde_json::from_value(v).unwrap_or_default();
        if let Err(e) = launch(&l) {
            log!("打开浏览器档案失败：{e}");
        }
    });
}

fn launch(l: &Launch) -> Result<(), String> {
    if !allowed_url(&l.url) || !allowed_proxy(&l.proxy) || l.profile.is_empty() {
        return Err("后台服务给的地址或代理不对，已忽略".into());
    }
    let browser = find_browser().ok_or("找不到 Chrome 或 Edge，请先安装其中一个")?;
    let dir = profile_dir(&l.profile).ok_or("找不到浏览器档案目录")?;
    std::fs::create_dir_all(&dir).map_err(|e| format!("创建浏览器档案目录失败：{e}"))?;
    let mut cmd = Command::new(&browser);
    cmd.arg(format!("--user-data-dir={}", dir.display())).arg("--no-first-run").arg("--no-default-browser-check");
    // 后台进程给了本机代理入口时才加（Lite 版不做代理，一般为空，直连）。
    if !l.proxy.is_empty() {
        cmd.arg(format!("--proxy-server={}", l.proxy));
    }
    cmd.arg(&l.url);
    cmd.spawn().map(|_| ()).map_err(|e| format!("启动浏览器失败：{e}"))
}

/// 每个账号一个档案目录：与令牌同目录下的 gemini-profiles/<账号>。
fn profile_dir(profile: &str) -> Option<PathBuf> {
    Some(token_path()?.parent()?.join("gemini-profiles").join(profile_name(profile)))
}

/// 把账号邮箱转成目录名：只留小写字母、数字和 . _ -。
pub(crate) fn profile_name(email: &str) -> String {
    email
        .to_lowercase()
        .replace('@', "_at_")
        .chars()
        .map(|c| if c.is_ascii_alphanumeric() || c == '.' || c == '-' || c == '_' { c } else { '_' })
        .collect()
}

/// 找系统里的 Chrome，没有时找 Edge。
fn find_browser() -> Option<PathBuf> {
    let mut candidates: Vec<PathBuf> = Vec::new();
    #[cfg(target_os = "macos")]
    {
        let apps = [
            "Google Chrome.app/Contents/MacOS/Google Chrome",
            "Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
        ];
        for app in apps {
            candidates.push(PathBuf::from("/Applications").join(app));
            if let Some(h) = std::env::var_os("HOME") {
                candidates.push(PathBuf::from(h).join("Applications").join(app));
            }
        }
    }
    #[cfg(windows)]
    {
        let exes = [r"Google\Chrome\Application\chrome.exe", r"Microsoft\Edge\Application\msedge.exe"];
        for exe in exes {
            for var in ["PROGRAMFILES", "PROGRAMFILES(X86)", "LOCALAPPDATA"] {
                if let Some(base) = std::env::var_os(var) {
                    candidates.push(PathBuf::from(base).join(exe));
                }
            }
        }
    }
    candidates.into_iter().find(|p| p.is_file())
}

// ---- 写入与读回本机凭据（BXC-F0-021、BXC-F0-018）----
//
// 状态里的 apply（"账号:版本"）变了，或者每分钟一次：
// 1. 本机凭据是上次写入的那个账号、但内容变了（agy / 应用自己刷新过）→ 交给后台服务上传（按版本号确认）。
// 2. 后台服务给的凭据和本机一样 → 只记下版本；本机的更新（同一 refresh token、更晚过期）→ 不覆盖。
// 3. 否则退出 Antigravity 应用 → 写入三处 → 重新启动应用（换了账号或 refresh token 时才重启）。
// 4. apply 为 0:0（取消分配、解绑）：本机凭据还是上次写入的账号时删除；用户自己登录的别的账号不动。
// 结果回报给后台服务，页面显示。写入记录（账号、版本、指纹）放在 gemini-applied.json，不含凭据。

const RECHECK: Duration = Duration::from_secs(60);

struct SyncState {
    running: bool,
    done: String,
    last: Option<Instant>,
}

static SYNC: Mutex<SyncState> = Mutex::new(SyncState { running: false, done: String::new(), last: None });

#[derive(Serialize, Deserialize, Default, Clone, PartialEq, Debug)]
struct Record {
    account_id: i64,
    email: String,
    version: i64,
    fingerprint: String,
}

#[derive(Deserialize, Default)]
struct Apply {
    #[serde(default)]
    account_id: i64,
    #[serde(default)]
    email: String,
    #[serde(default)]
    version: i64,
    #[serde(default)]
    credential: Option<Value>,
}

/// 已经通知过的延后切换（until），同一次只通知一次。
static DEFER_NOTIFIED: Mutex<i64> = Mutex::new(0);

/// 延后的切换还在宽限期内、且 Antigravity 正在运行：发一次系统通知，返回 true 表示这次先不核对。
/// 应用没在运行时不打扰任何人，直接切换。
fn deferred(app: &AppHandle, b: &Brief) -> bool {
    let Some(d) = &b.defer else { return false };
    let now = std::time::SystemTime::now().duration_since(std::time::UNIX_EPOCH).map(|t| t.as_secs() as i64).unwrap_or(0);
    let left = d.until - now;
    if left <= 0 || !gemini_store::app_running() {
        return false;
    }
    let mut notified = DEFER_NOTIFIED.lock().unwrap();
    if *notified != d.until {
        *notified = d.until;
        log!("Gemini 账号将在 {left} 秒后切换：{}", d.reason);
        let body = format!("{}。Antigravity 将在约 {} 秒后关闭并重新打开，请保存进行中的工作；也可以在 Banbu Connect Lite 里点「立即切换」。", d.reason, left);
        if let Err(e) = app.notification().builder().title("Gemini 账号即将切换").body(body).show() {
            log!("系统通知失败：{e}");
        }
    }
    true
}

/// 每次读到状态时调用：需要时在后台线程里核对一次。
pub fn tick(app: &AppHandle, b: &Brief) {
    if b.apply.is_empty() {
        return;
    }
    tick_launch(b);
    if deferred(app, b) {
        return;
    }
    let mut st = SYNC.lock().unwrap();
    let due = st.done != b.apply || st.last.is_none_or(|t| t.elapsed() > RECHECK);
    if st.running || !due {
        return;
    }
    st.running = true;
    let rev = b.apply.clone();
    std::thread::spawn(move || {
        let ok = match sync_once() {
            Ok(()) => true,
            Err(e) => {
                log!("Gemini 凭据核对失败：{e}");
                false
            }
        };
        let mut st = SYNC.lock().unwrap();
        st.running = false;
        st.last = Some(Instant::now());
        if ok {
            st.done = rev;
        }
    });
}

fn record_path() -> Option<PathBuf> {
    Some(token_path()?.parent()?.join("gemini-applied.json"))
}

fn load_record() -> Record {
    record_path()
        .and_then(|p| std::fs::read(p).ok())
        .and_then(|b| serde_json::from_slice(&b).ok())
        .unwrap_or_default()
}

fn save_record(r: &Record) {
    if let Some(p) = record_path() {
        let _ = std::fs::write(p, serde_json::to_vec(r).unwrap_or_default());
    }
}

fn token_field<'a>(v: &'a Value, k: &str) -> &'a str {
    v["token"][k].as_str().unwrap_or_default()
}

/// 凭据指纹：refresh token + access token 的哈希，只用来判断有没有变。
fn fingerprint(v: &Value) -> String {
    let mut h = DefaultHasher::new();
    token_field(v, "refresh_token").hash(&mut h);
    token_field(v, "access_token").hash(&mut h);
    format!("{:016x}", h.finish())
}

/// id_token 里的邮箱（不校验签名，只用来认账号）。
fn id_email(v: &Value) -> String {
    use base64::Engine;
    let tok = v["id_token"].as_str().unwrap_or_default();
    let Some(payload) = tok.split('.').nth(1) else { return String::new() };
    base64::engine::general_purpose::URL_SAFE_NO_PAD
        .decode(payload.trim_end_matches('='))
        .ok()
        .and_then(|b| serde_json::from_slice::<Value>(&b).ok())
        .and_then(|c| c["email"].as_str().map(str::to_lowercase))
        .unwrap_or_default()
}

/// 过期时间：都是同一格式的 UTC RFC 3339，按字典序比较即可；没有时为空串，当作最旧。
fn expiry(v: &Value) -> String {
    token_field(v, "expiry").to_string()
}

/// 本机凭据是不是记录里的那个账号：有邮箱时比邮箱，没有时比 refresh token。
fn same_account(local: &Value, rec: &Record, written_refresh: &str) -> bool {
    let e = id_email(local);
    if !e.is_empty() {
        return e == rec.email;
    }
    !written_refresh.is_empty() && token_field(local, "refresh_token") == written_refresh
}

/// 选本机最新的一份（过期时间最晚）。
fn newest_local() -> Option<Value> {
    gemini_store::read_all().into_iter().map(|(_, v)| v).max_by_key(expiry)
}

#[derive(Serialize)]
struct Applied<'a> {
    account_id: i64,
    version: i64,
    error: &'a str,
}

fn report(account_id: i64, version: i64, error: &str) {
    let body = serde_json::to_value(Applied { account_id, version, error }).unwrap_or_default();
    if let Err(e) = request("POST", "/applied", Some(body), Duration::from_secs(10)) {
        log!("回报 Gemini 凭据写入结果失败：{e}");
    }
}

// ---- 一键启动（参考 cockpit-tools 的「切换并启动」）----

static LAUNCHED: Mutex<String> = Mutex::new(String::new());
/// 最近一次因写入凭据而重启应用的时间：刚重启过的，「启动」时不再重启一次。
static RESTARTED: Mutex<Option<Instant>> = Mutex::new(None);

fn mark_restarted() {
    *RESTARTED.lock().unwrap() = Some(Instant::now());
}

/// 页面点了「启动」：后台服务已把当前账号切过去；等这一版凭据写进本机（核对完成）后启动 Antigravity 应用。
/// 已在运行时退出再启动，让它重新读凭据（应用只在启动时读；之前写入失败、或别处改过凭据时它会一直显示未登录）；
/// 30 秒内 sync_once 刚重启过的不再重启。结果回报给后台服务。
fn tick_launch(b: &Brief) {
    let Some(l) = &b.launch else { return };
    if !b.apply.starts_with(&format!("{}:", l.account_id)) {
        return;
    }
    {
        let st = SYNC.lock().unwrap();
        if st.running || st.done != b.apply {
            return; // 凭据还没写好，下次再看
        }
    }
    let mut done = LAUNCHED.lock().unwrap();
    if *done == l.id {
        return;
    }
    *done = l.id.clone();
    let id = l.id.clone();
    std::thread::spawn(move || {
        let fresh = RESTARTED.lock().unwrap().is_some_and(|t| t.elapsed() < Duration::from_secs(30));
        let err = if !fresh && gemini_store::app_running() {
            match gemini_store::quit_app() {
                Ok(Some(h)) => {
                    let hubs = gemini_store::stop_hubs();
                    gemini_store::restart_hubs(hubs);
                    gemini_store::start_app(&h);
                    mark_restarted();
                    String::new()
                }
                Ok(None) => gemini_store::launch_app().err().unwrap_or_default(),
                Err(e) => e,
            }
        } else {
            gemini_store::launch_app().err().unwrap_or_default()
        };
        if !err.is_empty() {
            log!("启动 Antigravity 失败：{err}");
        }
        if let Err(e) = request("POST", &format!("/launch/{id}/result"), Some(serde_json::json!({ "error": err })), Duration::from_secs(10)) {
            log!("回报 Antigravity 启动结果失败：{e}");
        }
    });
}

fn sync_once() -> Result<(), String> {
    let v = request("GET", "/apply", None, Duration::from_secs(10))?;
    let a: Apply = serde_json::from_value(v).map_err(|e| e.to_string())?;
    let mut rec = load_record();
    let local = newest_local();

    let Some(cred) = a.credential.filter(|_| a.account_id > 0) else {
        // 不再持有任何账号：只删除自己写入的那个账号的凭据。
        if rec.account_id != 0 {
            if local.as_ref().is_some_and(|l| same_account(l, &rec, "") || fingerprint(l) == rec.fingerprint) {
                let handle = match gemini_store::quit_app() {
                    Ok(h) => h,
                    Err(e) => {
                        report(0, 0, &e);
                        return Err(e);
                    }
                };
                let hubs = gemini_store::stop_hubs();
                let r = gemini_store::remove_all();
                gemini_store::restart_hubs(hubs);
                if let Some(h) = handle {
                    gemini_store::start_app(&h);
                    mark_restarted();
                }
                if let Err(e) = r {
                    report(0, 0, &e);
                    return Err(e);
                }
                log!("已删除本机的 Gemini 凭据（{}）", rec.email);
            }
            save_record(&Record::default());
        }
        report(0, 0, "");
        return Ok(());
    };

    let want_refresh = token_field(&cred, "refresh_token").to_string();
    if let Some(l) = &local {
        // 1. 本机刷新过：交给后台服务上传。
        if rec.account_id == a.account_id && fingerprint(l) != rec.fingerprint && same_account(l, &rec, &want_refresh) {
            let body = serde_json::json!({ "account_id": a.account_id, "credential": l });
            match request("POST", "/local", Some(body), Duration::from_secs(30)) {
                Ok(r) if r["uploaded"].as_bool() == Some(true) => log!("已上传本机刷新后的 Gemini 凭据（{}）", a.email),
                Ok(_) => {}
                Err(e) => log!("上传本机 Gemini 凭据失败：{e}"),
            }
            rec.fingerprint = fingerprint(l);
            save_record(&rec);
        }
        // 2. 和本机一样，或本机更新：不写。
        let same = fingerprint(l) == fingerprint(&cred);
        let newer = rec.account_id == a.account_id && token_field(l, "refresh_token") == want_refresh && expiry(l) > expiry(&cred);
        if same || newer {
            rec = Record { account_id: a.account_id, email: a.email.clone(), version: a.version, fingerprint: fingerprint(l) };
            save_record(&rec);
            report(a.account_id, a.version, "");
            return Ok(());
        }
    }

    // 3. 写入；换了账号或 refresh token 时重启应用，让它读到新凭据。
    let restart = local.as_ref().is_none_or(|l| token_field(l, "refresh_token") != want_refresh);
    // 换账号时连同 App Hub 一起重启：它在内存里持有登录信息（macOS，参考 cockpit-tools）。
    // 关不掉应用时不写：写了应用也还是旧账号（K029）。
    let handle = if restart {
        match gemini_store::quit_app() {
            Ok(h) => h,
            Err(e) => {
                report(a.account_id, a.version, &e);
                return Err(e);
            }
        }
    } else {
        None
    };
    let hubs = if restart { gemini_store::stop_hubs() } else { Vec::new() };
    let result = gemini_store::write_all(&cred);
    gemini_store::restart_hubs(hubs);
    if let Some(h) = handle {
        gemini_store::start_app(&h);
        mark_restarted();
    }
    if let Err(e) = result {
        report(a.account_id, a.version, &e);
        return Err(e);
    }
    if rec.account_id != a.account_id {
        log!("本机 Gemini 账号已切换到 {}", a.email);
    }
    save_record(&Record { account_id: a.account_id, email: a.email, version: a.version, fingerprint: fingerprint(&cred) });
    report(a.account_id, a.version, "");
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn profile_names_are_safe() {
        assert_eq!(profile_name("A.B+x@Gmail.com"), "a.b_x_at_gmail.com");
        assert_eq!(profile_name("../x@y"), ".._x_at_y");
        assert!(!profile_name("../x@y").contains('/'));
    }

    fn cred(email: &str, refresh: &str, access: &str, expiry: &str) -> Value {
        use base64::Engine;
        let enc = |s: &str| base64::engine::general_purpose::URL_SAFE_NO_PAD.encode(s);
        let id = if email.is_empty() { String::new() } else { format!("{}.{}.sig", enc("{}"), enc(&format!("{{\"email\":\"{email}\"}}"))) };
        serde_json::json!({"token": {"access_token": access, "refresh_token": refresh, "expiry": expiry}, "id_token": id})
    }

    #[test]
    fn recognizes_the_written_account() {
        let rec = Record { account_id: 1, email: "a@gmail.com".into(), version: 1, fingerprint: String::new() };
        assert_eq!(id_email(&cred("A@Gmail.com", "r", "x", "")), "a@gmail.com");
        assert!(same_account(&cred("a@gmail.com", "r2", "x", ""), &rec, "r"));
        assert!(!same_account(&cred("b@gmail.com", "r", "x", ""), &rec, "r"));
        // 没有 id_token：只认 refresh token 没变的。
        assert!(same_account(&cred("", "r", "x", ""), &rec, "r"));
        assert!(!same_account(&cred("", "other", "x", ""), &rec, "r"));
        // 刷新后指纹变化、过期时间更晚。
        let old = cred("a@gmail.com", "r", "x1", "2026-11-23T10:00:00.000000Z");
        let new = cred("a@gmail.com", "r", "x2", "2026-11-23T11:00:00.000000Z");
        assert_ne!(fingerprint(&old), fingerprint(&new));
        assert!(expiry(&new) > expiry(&old));
    }

    #[test]
    fn only_google_urls_and_local_proxy() {
        assert!(allowed_url("https://accounts.google.com/o/oauth2/v2/auth?x=1"));
        assert!(!allowed_url("https://evil.example/https://accounts.google.com/"));
        assert!(allowed_proxy("http://127.0.0.1:19190"));
        assert!(!allowed_proxy("http://10.0.0.1:8080"));
    }
}
