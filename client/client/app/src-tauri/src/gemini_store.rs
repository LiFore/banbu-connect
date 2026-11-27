// agy CLI 与 Antigravity 应用的凭据存储（BXC-F0-021）。三处写同一份凭据 JSON（cockpit-tools 读出的做法，
// TODO(verify:V-agy-store) 未在真机确认）：
// - 系统凭据（应用读取）：macOS 钥匙串通用密码 服务 gemini / 账号 antigravity，值为 "go-keyring-base64:" + base64(JSON)；
//   Windows 凭据管理器 Generic，目标 gemini:antigravity、用户 antigravity，值为 JSON 原文（上限 2560 字节）。
// - 应用的回退文件 ~/.gemini/jetski-standalone-oauth-token（钥匙串超时时应用读它，必须和钥匙串一起写）。
// - agy CLI：~/.gemini/antigravity-cli/antigravity-oauth-token。
// 切换账号时先退出正在运行的 Antigravity 应用，写完再启动。

use std::path::PathBuf;
use std::process::{Command, Stdio};
use std::time::{Duration, Instant};

use serde_json::Value;

const KEYRING_SERVICE: &str = "gemini";
const KEYRING_USER: &str = "antigravity";
#[cfg(target_os = "macos")]
const KEYRING_PREFIX: &str = "go-keyring-base64:";
/// TODO(verify:V-agy-store) 应用名与进程名。
#[cfg(target_os = "macos")]
const APP_NAME: &str = "Antigravity";
#[cfg(windows)]
const APP_PROCESS: &str = "Antigravity";

fn home() -> Option<PathBuf> {
    std::env::var_os(if cfg!(windows) { "USERPROFILE" } else { "HOME" }).map(PathBuf::from)
}

fn files() -> Vec<(&'static str, PathBuf)> {
    let Some(h) = home() else { return Vec::new() };
    let g = h.join(".gemini");
    vec![
        ("agy", g.join("antigravity-cli").join("antigravity-oauth-token")),
        ("app-file", g.join("jetski-standalone-oauth-token")),
    ]
}

/// agy 用过（目录已存在）时才写它的凭据文件；没装 agy 时不替它建目录（和 cockpit-tools 的「切应用」一致）。
fn agy_initialized() -> bool {
    home().is_some_and(|h| h.join(".gemini").join("antigravity-cli").is_dir())
}

/// 读出本机各处的凭据（解析失败、不存在的跳过），返回（来源, JSON）。
pub fn read_all() -> Vec<(&'static str, Value)> {
    let mut out = Vec::new();
    for (name, path) in files() {
        if let Some(v) = std::fs::read(&path).ok().and_then(|b| serde_json::from_slice::<Value>(&b).ok()) {
            out.push((name, v));
        }
    }
    if let Some(v) = keyring_read().and_then(|s| serde_json::from_str::<Value>(&s).ok()) {
        out.push(("keyring", v));
    }
    out
}

/// 写入：先写系统凭据（应用优先读它），再写回退文件，agy 用过时再写 agy 的文件。
/// 任何一处失败都返回错误，但其余的照常写。
pub fn write_all(cred: &Value) -> Result<(), String> {
    let text = cred.to_string();
    let mut errs = Vec::new();
    if let Err(e) = keyring_write(&text) {
        errs.push(format!("系统凭据：{e}"));
    }
    for (name, path) in files() {
        if name == "agy" && !agy_initialized() {
            continue;
        }
        if let Err(e) = write_file(&path, text.as_bytes()) {
            errs.push(format!("{name}：{e}"));
        }
    }
    if errs.is_empty() {
        Ok(())
    } else {
        Err(errs.join("；"))
    }
}

/// 删除三处。
pub fn remove_all() -> Result<(), String> {
    let mut errs = Vec::new();
    for (name, path) in files() {
        if let Err(e) = std::fs::remove_file(&path) {
            if e.kind() != std::io::ErrorKind::NotFound {
                errs.push(format!("{name}：{e}"));
            }
        }
    }
    if let Err(e) = keyring_delete() {
        errs.push(format!("系统凭据：{e}"));
    }
    if errs.is_empty() {
        Ok(())
    } else {
        Err(errs.join("；"))
    }
}

/// 先写临时文件再改名，避免 agy / 应用读到一半；只有当前用户可读。
fn write_file(path: &PathBuf, body: &[u8]) -> Result<(), String> {
    let dir = path.parent().ok_or("路径不对")?;
    std::fs::create_dir_all(dir).map_err(|e| e.to_string())?;
    let tmp = path.with_extension("boxctl-tmp");
    std::fs::write(&tmp, body).map_err(|e| e.to_string())?;
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        let _ = std::fs::set_permissions(&tmp, std::fs::Permissions::from_mode(0o600));
    }
    std::fs::rename(&tmp, path).map_err(|e| e.to_string())
}

// ---- 系统凭据 ----

#[cfg(target_os = "macos")]
fn keyring_read() -> Option<String> {
    // 应用用 go-keyring 写入，它经 /usr/bin/security 读写，这里同样用 security，不会弹出授权提示。
    let out = Command::new("/usr/bin/security")
        .args(["find-generic-password", "-s", KEYRING_SERVICE, "-a", KEYRING_USER, "-w"])
        .stderr(Stdio::null())
        .output()
        .ok()?;
    if !out.status.success() {
        return None;
    }
    let s = String::from_utf8(out.stdout).ok()?.trim().to_string();
    decode_keyring(&s)
}

#[cfg(target_os = "macos")]
fn decode_keyring(s: &str) -> Option<String> {
    use base64::Engine;
    match s.strip_prefix(KEYRING_PREFIX) {
        Some(b) => String::from_utf8(base64::engine::general_purpose::STANDARD.decode(b).ok()?).ok(),
        None => Some(s.to_string()),
    }
}

#[cfg(target_os = "macos")]
fn keyring_write(text: &str) -> Result<(), String> {
    use base64::Engine;
    let value = format!("{KEYRING_PREFIX}{}", base64::engine::general_purpose::STANDARD.encode(text));
    // 和应用里的 go-keyring 一样把值放在参数里：之前经 `security -i` 的标准输入传值，它每行只读约 4 KB，
    // 超长时把截断的值写进钥匙串，应用读到坏凭据就显示未登录（2026-11-23 实测）。
    // -U 原地更新，不带 -A/-T，避免每次切换弹授权。进程参数只有本用户和 root 看得到，且只存在几毫秒。
    let out = Command::new("/usr/bin/security")
        .args(["add-generic-password", "-U", "-s", KEYRING_SERVICE, "-a", KEYRING_USER, "-w", &value])
        .stdout(Stdio::null())
        .stderr(Stdio::piped())
        .output()
        .map_err(|e| e.to_string())?;
    if !out.status.success() {
        return Err(format!("写入钥匙串失败：{}", String::from_utf8_lossy(&out.stderr).trim()));
    }
    // 读回核对：值不一致时宁可报错，也不让应用读到半截凭据。
    if keyring_read().as_deref() != Some(text) {
        return Err("写入钥匙串后读回的内容不一致".into());
    }
    Ok(())
}

#[cfg(target_os = "macos")]
fn keyring_delete() -> Result<(), String> {
    let out = Command::new("/usr/bin/security")
        .args(["delete-generic-password", "-s", KEYRING_SERVICE, "-a", KEYRING_USER])
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .status()
        .map_err(|e| e.to_string())?;
    // 退出码 44：本来就没有。
    if out.success() || out.code() == Some(44) {
        Ok(())
    } else {
        Err(format!("删除钥匙串条目失败（退出码 {:?}）", out.code()))
    }
}

#[cfg(windows)]
mod wincred {
    use windows_sys::Win32::Security::Credentials::{
        CredDeleteW, CredFree, CredReadW, CredWriteW, CREDENTIALW, CRED_PERSIST_LOCAL_MACHINE, CRED_TYPE_GENERIC,
    };

    /// go-keyring（Windows）的目标名是 "服务:用户"。
    fn target() -> Vec<u16> {
        wide(&format!("{}:{}", super::KEYRING_SERVICE, super::KEYRING_USER))
    }

    fn wide(s: &str) -> Vec<u16> {
        s.encode_utf16().chain(std::iter::once(0)).collect()
    }

    pub fn read() -> Option<String> {
        let t = target();
        let mut p: *mut CREDENTIALW = std::ptr::null_mut();
        // SAFETY: p 由 CredReadW 分配，用完 CredFree。
        unsafe {
            if CredReadW(t.as_ptr(), CRED_TYPE_GENERIC, 0, &mut p) == 0 || p.is_null() {
                return None;
            }
            let c = &*p;
            let blob = std::slice::from_raw_parts(c.CredentialBlob, c.CredentialBlobSize as usize).to_vec();
            CredFree(p as *const _);
            String::from_utf8(blob).ok()
        }
    }

    pub fn write(text: &str) -> Result<(), String> {
        // go-keyring 的上限。
        if text.len() > 2560 {
            return Err(format!("凭据有 {} 字节，超过 Windows 凭据管理器的上限 2560", text.len()));
        }
        let mut t = target();
        let mut user = wide(super::KEYRING_USER);
        let mut blob = text.as_bytes().to_vec();
        // SAFETY: 结构体里的指针在调用期间有效。
        unsafe {
            let mut c: CREDENTIALW = std::mem::zeroed();
            c.Type = CRED_TYPE_GENERIC;
            c.TargetName = t.as_mut_ptr();
            c.UserName = user.as_mut_ptr();
            c.CredentialBlobSize = blob.len() as u32;
            c.CredentialBlob = blob.as_mut_ptr();
            c.Persist = CRED_PERSIST_LOCAL_MACHINE;
            if CredWriteW(&c, 0) == 0 {
                return Err(format!("写入凭据管理器失败：{}", std::io::Error::last_os_error()));
            }
        }
        Ok(())
    }

    pub fn delete() -> Result<(), String> {
        let t = target();
        // SAFETY: 只传入以 0 结尾的字符串。
        unsafe {
            if CredDeleteW(t.as_ptr(), CRED_TYPE_GENERIC, 0) == 0 {
                let e = std::io::Error::last_os_error();
                // ERROR_NOT_FOUND：本来就没有。
                if e.raw_os_error() != Some(1168) {
                    return Err(format!("删除凭据管理器条目失败：{e}"));
                }
            }
        }
        Ok(())
    }
}

#[cfg(windows)]
fn keyring_read() -> Option<String> {
    wincred::read()
}
#[cfg(windows)]
fn keyring_write(text: &str) -> Result<(), String> {
    wincred::write(text)
}
#[cfg(windows)]
fn keyring_delete() -> Result<(), String> {
    wincred::delete()
}

#[cfg(not(any(windows, target_os = "macos")))]
fn keyring_read() -> Option<String> {
    None
}
#[cfg(not(any(windows, target_os = "macos")))]
fn keyring_write(_: &str) -> Result<(), String> {
    Ok(())
}
#[cfg(not(any(windows, target_os = "macos")))]
fn keyring_delete() -> Result<(), String> {
    Ok(())
}

// ---- Antigravity 应用：切换账号时退出再启动 ----
//
// 先礼后兵（K029）：先让应用正常退出（外部退出不弹确认框，应用自己有限时清理），等 QUIT_GRACE；
// 还有进程（应用卡住、或子进程没退）就对应用目录下的全部进程（主程序、Helper、language_server）发 TERM，
// 再等 TERM_GRACE 后 KILL。确认全部退出才返回；关不掉时返回错误，调用方不写凭据——
// 否则应用还是旧账号，再启动时新实例拿不到单实例锁会立刻退出。

const QUIT_GRACE: Duration = Duration::from_secs(10);
const TERM_GRACE: Duration = Duration::from_secs(3);

/// 正在运行时关闭应用，返回重新启动用的信息；没在运行返回 Ok(None)；关不掉返回错误。
pub fn quit_app() -> Result<Option<String>, String> {
    #[cfg(target_os = "macos")]
    {
        let main_running = app_running();
        if !main_running && app_pids().is_empty() {
            return Ok(None);
        }
        if main_running {
            // AppleEvent 默认超时约 2 分钟，应用卡住时 osascript 会一直等：脚本里限时，外面再兜一层。
            let script = format!("with timeout of {} seconds\ntell application \"{APP_NAME}\" to quit\nend timeout", QUIT_GRACE.as_secs());
            if let Ok(child) = Command::new("/usr/bin/osascript").args(["-e", &script]).stdout(Stdio::null()).stderr(Stdio::null()).spawn() {
                wait_child(child, QUIT_GRACE + Duration::from_secs(2));
            }
            wait_for(QUIT_GRACE, || !app_running() && app_pids().is_empty());
        }
        let left = app_pids();
        if !left.is_empty() {
            log!("Antigravity 没有在 {} 秒内退出，强制结束 {} 个进程", QUIT_GRACE.as_secs(), left.len());
            signal(&left, "-TERM");
            wait_for(TERM_GRACE, || app_pids().is_empty());
            let left = app_pids();
            if !left.is_empty() {
                signal(&left, "-KILL");
                wait_for(Duration::from_secs(2), || app_pids().is_empty());
            }
        }
        let left = app_pids();
        if !left.is_empty() {
            return Err(format!("Antigravity 没能关闭（还有 {} 个进程），没有切换账号", left.len()));
        }
        Ok(main_running.then(|| APP_NAME.to_string()))
    }
    #[cfg(windows)]
    {
        let path = powershell(&format!(
            "(Get-Process -Name {APP_PROCESS} -ErrorAction SilentlyContinue | Select-Object -First 1).Path"
        ));
        if path.is_empty() {
            return Ok(None);
        }
        // 应用目录下的全部进程（含 resources\bin 下的 language_server）。
        let dir = std::path::Path::new(&path).parent().map(|d| d.to_string_lossy().replace('\'', "''")).unwrap_or_default();
        let under = format!("Get-Process -ErrorAction SilentlyContinue | Where-Object {{ $_.Path -and $_.Path.StartsWith('{dir}\\', 'OrdinalIgnoreCase') }}");
        let count = || powershell(&format!("@({under}).Count"));
        let _ = powershell(&format!("Get-Process -Name {APP_PROCESS} -ErrorAction SilentlyContinue | ForEach-Object {{ $_.CloseMainWindow() | Out-Null }}"));
        wait_for(QUIT_GRACE, || count() == "0");
        if count() != "0" {
            log!("Antigravity 没有在 {} 秒内退出，强制结束", QUIT_GRACE.as_secs());
            let _ = powershell(&format!("{under} | Stop-Process -Force -ErrorAction SilentlyContinue"));
            wait_for(TERM_GRACE, || count() == "0");
        }
        if count() != "0" {
            return Err("Antigravity 没能关闭，没有切换账号".into());
        }
        Ok(Some(path))
    }
    #[cfg(not(any(windows, target_os = "macos")))]
    Ok(None)
}

/// 本用户在 Antigravity.app 里运行的全部进程：主程序、各 Helper、应用自带的 language_server。
/// 不含 ~/.gemini/bin/agy 的 App Hub（它由 stop_hubs 单独处理，写完凭据要原样重启）。
#[cfg(target_os = "macos")]
fn app_pids() -> Vec<u32> {
    let Ok(out) = Command::new("/bin/ps").args(["-x", "-o", "pid=,args="]).output() else { return Vec::new() };
    String::from_utf8_lossy(&out.stdout)
        .lines()
        .filter_map(|l| {
            let (pid, args) = l.trim_start().split_once(' ')?;
            args.contains("/Antigravity.app/Contents/").then(|| pid.parse().ok())?
        })
        .collect()
}

#[cfg(target_os = "macos")]
fn signal(pids: &[u32], sig: &str) {
    let mut args = vec![sig.to_string()];
    args.extend(pids.iter().map(|p| p.to_string()));
    let _ = Command::new("/bin/kill").args(&args).stderr(Stdio::null()).status();
}

/// 等子进程结束，超时就结束它（osascript 在应用无响应时可能一直等）。
#[cfg(target_os = "macos")]
fn wait_child(mut child: std::process::Child, limit: Duration) {
    let start = Instant::now();
    while start.elapsed() < limit {
        if let Ok(Some(_)) = child.try_wait() {
            return;
        }
        std::thread::sleep(Duration::from_millis(200));
    }
    let _ = child.kill();
    let _ = child.wait();
}

// ---- macOS 的 App Hub：agy 装好后常驻的 `~/.gemini/bin/agy --hub --app_data_dir antigravity` ----
// 它独立于应用运行、在内存里持有登录信息；应用重启后会重新连上它，不重启 Hub 就还是旧账号或未登录。
// 切换时先停掉（参考 cockpit-tools），写完凭据再用原来的参数启动。

/// 列出参数满足条件、且带 `--app_data_dir antigravity` 的本用户进程。
#[cfg(target_os = "macos")]
fn hub_pids(filter: impl Fn(&str) -> bool) -> Vec<u32> {
    hub_processes(filter).into_iter().map(|(pid, _)| pid).collect()
}

#[cfg(target_os = "macos")]
fn hub_processes(filter: impl Fn(&str) -> bool) -> Vec<(u32, String)> {
    // ps -x 只列本用户的进程（含没有终端的）。
    let Ok(out) = Command::new("/bin/ps").args(["-x", "-o", "pid=,args="]).output() else { return Vec::new() };
    String::from_utf8_lossy(&out.stdout)
        .lines()
        .filter_map(|l| {
            let l = l.trim_start();
            let (pid, args) = l.split_once(' ')?;
            let data_dir = args.contains("--app_data_dir antigravity") || args.contains("--app_data_dir=antigravity");
            if !(data_dir && filter(args)) {
                return None;
            }
            Some((pid.parse().ok()?, args.trim().to_string()))
        })
        .collect()
}

/// 停掉独立运行的 App Hub，返回它们的命令行，写完凭据后用 restart_hubs 原样启动。
pub fn stop_hubs() -> Vec<String> {
    #[cfg(target_os = "macos")]
    {
        let Some(agy) = home().map(|h| h.join(".gemini").join("bin").join("agy")) else { return Vec::new() };
        let agy = agy.to_string_lossy().to_string();
        let is_hub = |args: &str| args.starts_with(&format!("{agy} ")) && args.split_whitespace().any(|a| a == "--hub");
        let hubs = hub_processes(is_hub);
        for (pid, _) in &hubs {
            let _ = Command::new("/bin/kill").args(["-TERM", &pid.to_string()]).status();
        }
        wait_until(|| hub_pids(is_hub).is_empty());
        hubs.into_iter().map(|(_, args)| args).collect()
    }
    #[cfg(not(target_os = "macos"))]
    Vec::new()
}

/// 按原来的命令行重新启动 App Hub。参数按空格切分：agy 的路径和参数里没有空格（主目录带空格时跳过）。
pub fn restart_hubs(hubs: Vec<String>) {
    for line in hubs {
        let mut parts = line.split_whitespace();
        let Some(exe) = parts.next() else { continue };
        if !std::path::Path::new(exe).is_file() {
            log!("App Hub 的命令行无法还原，没有重启：{exe}");
            continue;
        }
        let r = Command::new(exe).args(parts).stdin(Stdio::null()).stdout(Stdio::null()).stderr(Stdio::null()).spawn();
        if let Err(e) = r {
            log!("重新启动 Antigravity App Hub 失败：{e}");
        }
    }
}

/// 应用是否在运行。
pub fn app_running() -> bool {
    #[cfg(target_os = "macos")]
    {
        Command::new("/usr/bin/osascript")
            .args(["-e", &format!("application \"{APP_NAME}\" is running")])
            .output()
            .map(|o| String::from_utf8_lossy(&o.stdout).trim() == "true")
            .unwrap_or(false)
    }
    #[cfg(windows)]
    {
        !powershell(&format!("(Get-Process -Name {APP_PROCESS} -ErrorAction SilentlyContinue | Select-Object -First 1).Path")).is_empty()
    }
    #[cfg(not(any(windows, target_os = "macos")))]
    false
}

/// 重新启动 quit_app 退出的应用。
pub fn start_app(handle: &str) {
    #[cfg(target_os = "macos")]
    let r = Command::new("/usr/bin/open").args(["-a", handle]).status();
    #[cfg(windows)]
    let r = {
        use std::os::windows::process::CommandExt;
        Command::new(handle).creation_flags(0x0000_0008 /* DETACHED_PROCESS */).spawn().map(|_| ())
    };
    #[cfg(not(any(windows, target_os = "macos")))]
    let r: Result<(), String> = Ok(());
    if let Err(e) = r.map(|_| ()) {
        log!("重新启动 Antigravity 失败：{e}");
    }
}

/// 一键启动：启动 Antigravity 应用，已在运行时切到前台。TODO(verify:V-agy-store) Windows 的安装位置。
pub fn launch_app() -> Result<(), String> {
    #[cfg(target_os = "macos")]
    {
        let ok = Command::new("/usr/bin/open").args(["-a", APP_NAME]).status().map(|s| s.success()).unwrap_or(false);
        if ok {
            Ok(())
        } else {
            Err("找不到 Antigravity 应用，请先安装".into())
        }
    }
    #[cfg(windows)]
    {
        use std::os::windows::process::CommandExt;
        let running = powershell(&format!(
            "(Get-Process -Name {APP_PROCESS} -ErrorAction SilentlyContinue | Select-Object -First 1).Path"
        ));
        if !running.is_empty() {
            return Ok(());
        }
        let mut candidates: Vec<PathBuf> = Vec::new();
        if let Some(l) = std::env::var_os("LOCALAPPDATA") {
            candidates.push(PathBuf::from(l).join("Programs").join("Antigravity").join("Antigravity.exe"));
        }
        for v in ["ProgramFiles", "ProgramFiles(x86)"] {
            if let Some(p) = std::env::var_os(v) {
                candidates.push(PathBuf::from(p).join("Antigravity").join("Antigravity.exe"));
            }
        }
        let exe = candidates.into_iter().find(|p| p.is_file()).ok_or("找不到 Antigravity 应用，请先安装")?;
        Command::new(&exe)
            .creation_flags(0x0000_0008 /* DETACHED_PROCESS */)
            .spawn()
            .map(|_| ())
            .map_err(|e| format!("启动 Antigravity 失败：{e}"))
    }
    #[cfg(not(any(windows, target_os = "macos")))]
    Err("这个系统不支持".into())
}

#[allow(dead_code)]
fn wait_until(done: impl Fn() -> bool) {
    wait_for(Duration::from_secs(15), done)
}

#[allow(dead_code)]
fn wait_for(limit: Duration, done: impl Fn() -> bool) {
    let start = Instant::now();
    while !done() && start.elapsed() < limit {
        std::thread::sleep(Duration::from_millis(500));
    }
}

#[cfg(windows)]
fn powershell(script: &str) -> String {
    use std::os::windows::process::CommandExt;
    Command::new("powershell.exe")
        .args(["-NoProfile", "-NonInteractive", "-Command", script])
        .creation_flags(0x0800_0000) // CREATE_NO_WINDOW
        .stderr(Stdio::null())
        .output()
        .map(|o| String::from_utf8_lossy(&o.stdout).trim().to_string())
        .unwrap_or_default()
}
