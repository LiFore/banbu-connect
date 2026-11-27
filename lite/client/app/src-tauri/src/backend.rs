// 后台进程：随应用打包的 boxctl（resources/boxctl），以当前用户身份由外壳拉起，不需要管理员权限。
//
//   boxctl desktop run -exit-on-stdin-close
//
// 标准输入接一根管道，外壳一直拿着写端：外壳退出（包括崩溃、OTA 后重启）时管道关闭，后台进程随之退出。
// 端口上已经有一个在应答（比如上一个外壳刚退出、它的后台进程还没退完）时不再拉起，等它退出后再接手。
// 后台进程意外退出时按退避时间重新拉起；它的 stderr 写到数据目录的 backend.log，起不来时页面显示最后几行。

use std::path::{Path, PathBuf};
use std::process::{Child, ChildStdin, Command, Stdio};
use std::sync::Mutex;
use std::time::{Duration, Instant};

use tauri::{AppHandle, Manager};

use crate::{agent, data_dir, read_token, SERVICE};

struct Backend {
    child: Option<Child>,
    // 拿着写端，后台进程才不会因为标准输入关闭而退出。和 child 一起丢掉。
    stdin: Option<ChildStdin>,
    started: Option<Instant>,
    error: Option<String>,
    // 下一次拉起前要等的时间（退避）。
    backoff: Duration,
    // stop() 之后不再自动拉起，直到 restart()。
    paused: bool,
}

static STATE: Mutex<Backend> =
    Mutex::new(Backend { child: None, stdin: None, started: None, error: None, backoff: MIN_BACKOFF, paused: false });

const MIN_BACKOFF: Duration = Duration::from_secs(1);
const MAX_BACKOFF: Duration = Duration::from_secs(30);
/// 运行超过这么久再退出的，不算起不来：下次立即拉起。
const HEALTHY_AFTER: Duration = Duration::from_secs(60);
const CHECK_EVERY: Duration = Duration::from_millis(1500);

fn binary(app: &AppHandle) -> Result<PathBuf, String> {
    let name = if cfg!(windows) { "boxctl.exe" } else { "boxctl" };
    app.path()
        .resolve(format!("resources/{name}"), tauri::path::BaseDirectory::Resource)
        .map_err(|e| e.to_string())
}

fn stderr_path() -> Option<PathBuf> {
    Some(data_dir()?.join("backend.log"))
}

/// 端口上有后台进程在应答（不管是不是这个外壳拉起的）。
pub fn answering() -> bool {
    ureq::AgentBuilder::new()
        .timeout(Duration::from_secs(1))
        .build()
        .get(&format!("{SERVICE}/api/v1/auth/session"))
        .call()
        .is_ok()
}

/// 最近一次拉起失败的原因；正常运行时为 None。
pub fn error() -> Option<String> {
    STATE.lock().unwrap().error.clone()
}

/// 启动看护线程：拉起后台进程，退出后按退避时间重新拉起。
pub fn supervise(app: &AppHandle) {
    let app = app.clone();
    std::thread::spawn(move || loop {
        tick(&app);
        std::thread::sleep(CHECK_EVERY);
    });
}

fn tick(app: &AppHandle) {
    let mut st = STATE.lock().unwrap();
    if let Some(child) = st.child.as_mut() {
        match child.try_wait() {
            Ok(None) => return, // 还在运行
            Ok(Some(status)) => {
                let ran = st.started.map(|t| t.elapsed()).unwrap_or_default();
                let tail = stderr_path().map(|p| tail(&p, 6)).unwrap_or_default();
                let msg = if tail.is_empty() { format!("后台进程已退出（{status}）") } else { format!("后台进程已退出（{status}）：\n{tail}") };
                log!("{msg}");
                st.child = None;
                st.stdin = None;
                if ran >= HEALTHY_AFTER {
                    st.backoff = MIN_BACKOFF;
                } else {
                    st.error = Some(msg);
                }
            }
            Err(e) => log!("查询后台进程状态失败：{e}"),
        }
        return;
    }
    if st.paused {
        return;
    }
    // 别的后台进程在应答（上一个外壳留下的、正在退出的，或开发时手动运行的）：先用它。
    drop(st);
    if answering() {
        STATE.lock().unwrap().error = None;
        return;
    }
    let mut st = STATE.lock().unwrap();
    if let Some(t) = st.started {
        if t.elapsed() < st.backoff {
            return;
        }
    }
    match spawn(app) {
        Ok((child, stdin)) => {
            log!("已拉起后台进程（pid {}）", child.id());
            if st.started.is_some_and(|t| t.elapsed() < HEALTHY_AFTER) {
                st.backoff = (st.backoff * 2).min(MAX_BACKOFF);
            }
            st.child = Some(child);
            st.stdin = Some(stdin);
            st.started = Some(Instant::now());
        }
        Err(e) => {
            log!("拉起后台进程失败：{e}");
            st.error = Some(format!("拉起后台进程失败：{e}"));
            st.started = Some(Instant::now());
            st.backoff = (st.backoff * 2).min(MAX_BACKOFF);
        }
    }
}

fn spawn(app: &AppHandle) -> Result<(Child, ChildStdin), String> {
    let bin = binary(app)?;
    let mut cmd = Command::new(&bin);
    cmd.args(["desktop", "run", "-exit-on-stdin-close"]).stdin(Stdio::piped()).stdout(Stdio::null());
    match stderr_path().and_then(|p| {
        let _ = std::fs::create_dir_all(p.parent()?);
        std::fs::File::create(p).ok()
    }) {
        Some(f) => cmd.stderr(Stdio::from(f)),
        None => cmd.stderr(Stdio::null()),
    };
    #[cfg(windows)]
    {
        use std::os::windows::process::CommandExt;
        const CREATE_NO_WINDOW: u32 = 0x0800_0000;
        cmd.creation_flags(CREATE_NO_WINDOW);
    }
    let mut child = cmd.spawn().map_err(|e| format!("{}：{e}", bin.display()))?;
    let stdin = child.stdin.take().ok_or("拿不到后台进程的标准输入")?;
    Ok((child, stdin))
}

/// 停止后台进程（OTA 安装前、退出时）：先请它自己退出，等一会儿还在就结束它。之后不再自动拉起，直到 restart。
pub fn stop() {
    let mut st = STATE.lock().unwrap();
    st.paused = true;
    let child = st.child.take();
    st.stdin = None; // 关掉标准输入，后台进程也会退出
    drop(st);
    if let Some(token) = read_token() {
        let _ = agent()
            .post(&format!("{SERVICE}/api/v1/desktop/shutdown"))
            .set("Authorization", &format!("Bearer {token}"))
            .send_string("{}");
    }
    if let Some(mut child) = child {
        for _ in 0..20 {
            if matches!(child.try_wait(), Ok(Some(_))) {
                return;
            }
            std::thread::sleep(Duration::from_millis(100));
        }
        let _ = child.kill();
        let _ = child.wait();
    }
}

/// 重启后台进程（版本比应用旧时、安装页点「重试」时）。
pub fn restart() {
    stop();
    // 等端口空出来，否则看护线程会以为别的后台进程在应答。
    for _ in 0..30 {
        if !answering() {
            break;
        }
        std::thread::sleep(Duration::from_millis(200));
    }
    let mut st = STATE.lock().unwrap();
    st.paused = false;
    st.error = None;
    st.started = None;
    st.backoff = MIN_BACKOFF;
}

/// 文件最后 n 行（只读最后 8 KB）。
fn tail(path: &Path, n: usize) -> String {
    use std::io::{Read, Seek, SeekFrom};
    let Ok(mut f) = std::fs::File::open(path) else { return String::new() };
    if let Ok(m) = f.metadata() {
        if m.len() > 8 << 10 {
            let _ = f.seek(SeekFrom::End(-(8 << 10)));
        }
    }
    let mut buf = Vec::new();
    let _ = f.read_to_end(&mut buf);
    let text = String::from_utf8_lossy(&buf);
    let lines: Vec<&str> = text.trim().lines().collect();
    lines[lines.len().saturating_sub(n)..].join("\n")
}
