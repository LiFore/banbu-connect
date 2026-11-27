// 用 ChatGPT 桌面客户端登录 Codex 账号（做法参考 cockpit-tools 的「官方客户端登录」，只参考思路不复制代码）。
//
// 新版 ChatGPT 桌面客户端（macOS /Applications/ChatGPT.app，bundle id com.openai.codex；Windows 为 Store/MSIX 包）
// 内置 Codex，登录由内置的 codex app-server 完成，凭据写在 CODEX_HOME/auth.json。所以：
//   1. 给客户端单独起一个实例：CODEX_HOME 指向账号的工作区，--user-data-dir 指向一次性的 Electron 档案
//      （workspaces/chatgpt-app/<账号>），不碰用户自己正在用的 ChatGPT；
//   2. 用户在客户端里按账号的注册方式登录（Google / 微软 / Apple / 邮箱密码都可以）；
//   3. 工作区的 auth.json 出现 refresh token 后关闭这个实例、删掉 Electron 档案（里面有 chatgpt.com 的会话 Cookie）；
//   4. 之后照常由扫描上报，按 ADR-061 的「本机登录」上传管理服务。
// 登录回调和 codex login 一样走 localhost:1455，所以和 codex login 共用一把锁，一次只登录一个。
// 已登录的工作区重新登录时先把 auth.json 挪开，否则客户端会直接用旧登录；登录没完成就挪回来。

use std::path::{Path, PathBuf};
use std::process::Command;
use std::time::{Duration, Instant};

use crate::workspaces::{self, CODEX};

const CLOSE_WAIT: Duration = Duration::from_secs(20);
/// 启动后这么久还找不到客户端进程，就认为它没起来或被用户关了。
const START_GRACE: Duration = Duration::from_secs(20);

fn app_data_dir(email: &str) -> Option<PathBuf> {
    Some(workspaces::root()?.join("chatgpt-app").join(workspaces::dir_name(email)))
}

pub fn login(email: &str) -> Result<(), String> {
    let _guard = workspaces::CODEX_LOGIN.try_lock().map_err(|_| "另一个 Codex 账号正在登录，请等它完成")?;
    let dir = workspaces::ensure(CODEX, email)?;
    if workspaces::is_default(&dir) {
        return Err("这个账号已设为默认，请直接打开 ChatGPT 客户端登录".into());
    }
    let n = workspaces::running(CODEX, &dir);
    if n > 0 {
        return Err(format!("这个账号有 {n} 个 codex 进程在运行，请先退出它们再登录"));
    }
    let app = find_app().ok_or("找不到 ChatGPT 桌面客户端，请先安装（需要内置 Codex 的新版）")?;
    let data = app_data_dir(email).ok_or("找不到数据目录")?;
    if !running_pids(&data).is_empty() {
        return Err("这个账号的 ChatGPT 客户端登录窗口已经开着，请在里面完成登录或先关掉它".into());
    }
    let _ = std::fs::remove_dir_all(&data);
    std::fs::create_dir_all(&data).map_err(|e| format!("创建客户端档案目录失败：{e}"))?;

    let auth = dir.join("auth.json");
    let bak = dir.join("auth.json.banbu-relogin");
    if auth.exists() {
        std::fs::rename(&auth, &bak).map_err(|e| format!("挪开旧登录失败：{e}"))?;
    }
    let result = launch(&app, &dir, &data).and_then(|_| wait_login(&dir, &data));
    let closed = close(&data);
    if let Err(e) = &closed {
        log!("关闭 {email} 的 ChatGPT 登录窗口失败：{e}");
    } else {
        let _ = std::fs::remove_dir_all(&data);
    }
    match &result {
        Ok(_) => {
            let _ = std::fs::remove_file(&bak);
        }
        Err(_) if bak.exists() && !workspaces::login_state(CODEX, &dir).0 => {
            let _ = std::fs::rename(&bak, &auth);
        }
        Err(_) => {}
    }
    let got = result?;
    if !got.is_empty() && !got.eq_ignore_ascii_case(email) {
        return Err(format!("登录的是 {got}，不是 {email}"));
    }
    log!("已用 ChatGPT 客户端登录 {email}");
    Ok(())
}

/// 等工作区里出现 refresh token；返回登录的邮箱。
fn wait_login(dir: &Path, data: &Path) -> Result<String, String> {
    let start = Instant::now();
    let mut seen = false;
    loop {
        let (logged, got) = workspaces::login_state(CODEX, dir);
        if logged {
            // 给客户端一点时间把 auth.json 写完整（id_token、account_id 同一次写入，这里只是保险）。
            std::thread::sleep(Duration::from_secs(1));
            let (_, got2) = workspaces::login_state(CODEX, dir);
            return Ok(if got2.is_empty() { got } else { got2 });
        }
        if start.elapsed() > workspaces::LOGIN_WAIT {
            return Err("10 分钟内没有完成登录".into());
        }
        let alive = !running_pids(data).is_empty();
        seen |= alive;
        if !alive && (seen || start.elapsed() > START_GRACE) {
            return Err(if seen { "ChatGPT 客户端已关闭，登录没有完成".into() } else { "ChatGPT 客户端没有启动起来".into() });
        }
        std::thread::sleep(Duration::from_secs(1));
    }
}

// ---- 找客户端 ----

#[cfg(target_os = "macos")]
fn find_app() -> Option<PathBuf> {
    // 内置 Codex 的才行：老版 ChatGPT 客户端没有 codex-cli；Codex.app 是改名前的旧路径。
    let has_codex = |p: &Path| p.join("Contents/Resources/codex-cli").exists() || p.join("Contents/Resources/codex").exists();
    let mut cands: Vec<PathBuf> = Vec::new();
    for base in [Some(PathBuf::from("/Applications")), workspaces::home().map(|h| h.join("Applications"))].into_iter().flatten() {
        cands.push(base.join("ChatGPT.app"));
        cands.push(base.join("Codex.app"));
    }
    if let Ok(out) = Command::new("/usr/bin/mdfind").arg("kMDItemCFBundleIdentifier == 'com.openai.codex'").output() {
        cands.extend(String::from_utf8_lossy(&out.stdout).lines().filter(|l| l.ends_with(".app")).map(PathBuf::from));
    }
    cands.into_iter().find(|p| has_codex(p))
}

/// Windows 返回 包族名|AppId|可执行文件（Store/MSIX 包只能在包上下文里带环境变量启动，K025）。
#[cfg(windows)]
fn find_app() -> Option<PathBuf> {
    let script = r#"$ErrorActionPreference = 'Stop'
foreach ($pkg in @(Get-AppxPackage | Where-Object { $_.Name -like '*OpenAI*' -and $_.InstallLocation } | Sort-Object Version -Descending)) {
  $m = Get-AppxPackageManifest -Package $pkg
  foreach ($a in $m.Package.Applications.Application) {
    $exe = [string]$a.Executable
    if ($exe -and ([IO.Path]::GetFileName($exe) -in @('ChatGPT.exe', 'Codex.exe'))) {
      Write-Output ($pkg.PackageFamilyName + '|' + $a.Id + '|' + (Join-Path $pkg.InstallLocation $exe))
      exit 0
    }
  }
}"#;
    let out = powershell(script).ok()?;
    let line = out.lines().map(str::trim).find(|l| l.split('|').count() == 3)?;
    Some(PathBuf::from(line))
}

#[cfg(not(any(windows, target_os = "macos")))]
fn find_app() -> Option<PathBuf> {
    None
}

// ---- 启动 ----

#[cfg(target_os = "macos")]
fn launch(app: &Path, codex_home: &Path, data: &Path) -> Result<(), String> {
    // 经 LaunchServices（open -n）起新实例：直接执行主程序会被系统当成本程序的子进程。open --env 把 CODEX_HOME 带给新实例。
    let status = Command::new("/usr/bin/open")
        .arg("-n")
        .arg("-a")
        .arg(app)
        .arg("--env")
        .arg(format!("CODEX_HOME={}", codex_home.display()))
        .arg("--args")
        .arg(format!("--user-data-dir={}", data.display()))
        .status()
        .map_err(|e| format!("启动 ChatGPT 客户端失败：{e}"))?;
    if !status.success() {
        return Err(format!("启动 ChatGPT 客户端失败（退出码 {:?}）", status.code()));
    }
    Ok(())
}

#[cfg(windows)]
fn launch(app: &Path, codex_home: &Path, data: &Path) -> Result<(), String> {
    // TODO(verify:V-chatgpt-app-login-win) 包上下文里用 cmd 设环境变量再 start 客户端，未在真机确认 CODEX_HOME 能带进去。
    let app = app.to_string_lossy();
    let mut it = app.split('|');
    let (family, app_id, exe) = (it.next().unwrap_or_default(), it.next().unwrap_or_default(), it.next().unwrap_or_default());
    let q = |s: &str| s.replace('\'', "''");
    let cmd_args = format!(
        "/c set \"CODEX_HOME={}\" && start \"\" \"{}\" --user-data-dir=\"{}\"",
        codex_home.display(),
        exe,
        data.display()
    );
    let script = format!(
        "$ErrorActionPreference = 'Stop'\nInvoke-CommandInDesktopPackage -PackageFamilyName '{}' -AppId '{}' -PreventBreakaway -Command 'cmd.exe' -Args '{}'",
        q(family),
        q(app_id),
        q(&cmd_args)
    );
    powershell(&script).map(|_| ())
}

#[cfg(not(any(windows, target_os = "macos")))]
fn launch(_app: &Path, _codex_home: &Path, _data: &Path) -> Result<(), String> {
    Err("这个系统不支持".into())
}

// ---- 找进程、关闭 ----

/// 用这个档案目录启动的客户端主进程（Electron 子进程带 --type=，不算）。
fn main_pids(lines: &str, data: &Path) -> Vec<u32> {
    let flag = format!("--user-data-dir={}", data.display());
    let quoted = format!("--user-data-dir=\"{}\"", data.display());
    lines
        .lines()
        .filter(|l| (has_arg(l, &flag) || l.contains(&quoted)) && !l.contains("--type="))
        .filter_map(|l| l.split_whitespace().next()?.parse().ok())
        .collect()
}

/// 命令行里有这个参数（后面紧跟空白或行尾，避免 …/a 匹配到 …/ab；路径本身可能带空格，不能按空白切）。
fn has_arg(line: &str, arg: &str) -> bool {
    line.match_indices(arg).any(|(i, _)| line[i + arg.len()..].chars().next().is_none_or(char::is_whitespace))
}

#[cfg(target_os = "macos")]
fn running_pids(data: &Path) -> Vec<u32> {
    let Ok(out) = Command::new("/bin/ps").args(["-axww", "-o", "pid=,command="]).output() else { return Vec::new() };
    main_pids(&String::from_utf8_lossy(&out.stdout), data)
}

#[cfg(windows)]
fn running_pids(data: &Path) -> Vec<u32> {
    let script = "Get-CimInstance Win32_Process -Filter \"Name='ChatGPT.exe' OR Name='Codex.exe'\" | ForEach-Object { [string]$_.ProcessId + ' ' + $_.CommandLine }";
    powershell(script).map(|out| main_pids(&out, data)).unwrap_or_default()
}

#[cfg(not(any(windows, target_os = "macos")))]
fn running_pids(_data: &Path) -> Vec<u32> {
    Vec::new()
}

/// 先礼后兵：正常退出（macOS SIGTERM，Windows 发关闭消息并带上子进程），等不到再强杀。
fn close(data: &Path) -> Result<(), String> {
    let pids = running_pids(data);
    if pids.is_empty() {
        return Ok(());
    }
    signal(&pids, false);
    let start = Instant::now();
    while start.elapsed() < CLOSE_WAIT {
        std::thread::sleep(Duration::from_millis(500));
        if running_pids(data).is_empty() {
            return Ok(());
        }
    }
    signal(&running_pids(data), true);
    std::thread::sleep(Duration::from_secs(1));
    if running_pids(data).is_empty() {
        Ok(())
    } else {
        Err("ChatGPT 客户端没有退出，请手动关掉这个登录窗口".into())
    }
}

#[cfg(unix)]
fn signal(pids: &[u32], force: bool) {
    for pid in pids {
        let _ = Command::new("/bin/kill").arg(if force { "-KILL" } else { "-TERM" }).arg(pid.to_string()).status();
    }
}

#[cfg(windows)]
fn signal(pids: &[u32], force: bool) {
    for pid in pids {
        let mut c = workspaces::hidden("taskkill.exe");
        c.args(["/PID", &pid.to_string(), "/T"]);
        if force {
            c.arg("/F");
        }
        let _ = c.status();
    }
}

#[cfg(windows)]
fn powershell(script: &str) -> Result<String, String> {
    let out = workspaces::hidden("powershell.exe")
        .args(["-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", script])
        .output()
        .map_err(|e| format!("运行 PowerShell 失败：{e}"))?;
    if !out.status.success() {
        let err = String::from_utf8_lossy(&out.stderr);
        return Err(format!("PowerShell 执行失败：{}", err.lines().next().unwrap_or_default()));
    }
    Ok(String::from_utf8_lossy(&out.stdout).into_owned())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn picks_main_process_only() {
        let data = Path::new("/w/chatgpt-app/a");
        let ps = "\
 101 /Applications/ChatGPT.app/Contents/MacOS/ChatGPT --user-data-dir=/w/chatgpt-app/a
 102 /Applications/ChatGPT.app/Contents/Frameworks/x/Codex (Aperitif) --type=renderer --user-data-dir=/w/chatgpt-app/a
 103 /Applications/ChatGPT.app/Contents/MacOS/ChatGPT
 104 /Applications/ChatGPT.app/Contents/MacOS/ChatGPT --user-data-dir=/w/chatgpt-app/b
 106 /Applications/ChatGPT.app/Contents/MacOS/ChatGPT --user-data-dir=/w/chatgpt-app/ab
 105 C:\\Apps\\ChatGPT.exe --user-data-dir=\"/w/chatgpt-app/a\"";
        assert_eq!(main_pids(ps, data), vec![101, 105]);
    }
}
