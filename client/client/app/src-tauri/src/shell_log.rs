// 外壳日志：从「应用程序」启动时没有终端，stderr 的内容看不到，所以同时写到用户目录下的文件。
// macOS：~/Library/Application Support/Banbu Connect/shell.log
// Windows：%LOCALAPPDATA%\Banbu Connect\shell.log
// 超过 1 MB 时改名为 shell.log.1（只留一份旧的）。

use std::io::Write;
use std::path::PathBuf;
use std::sync::Mutex;
use std::time::{SystemTime, UNIX_EPOCH};

use crate::token_path;

const MAX_SIZE: u64 = 1 << 20;
static LOCK: Mutex<()> = Mutex::new(());

pub fn path() -> Option<PathBuf> {
    Some(token_path()?.parent()?.join("shell.log"))
}

/// 写一行日志（同时输出到 stderr）。失败时静默：日志不能影响正常功能。
pub fn write(msg: &str) {
    eprintln!("{msg}");
    let Some(path) = path() else { return };
    let _guard = LOCK.lock();
    if std::fs::metadata(&path).is_ok_and(|m| m.len() > MAX_SIZE) {
        let _ = std::fs::rename(&path, path.with_extension("log.1"));
    }
    if let Some(dir) = path.parent() {
        let _ = std::fs::create_dir_all(dir);
    }
    if let Ok(mut f) = std::fs::OpenOptions::new().create(true).append(true).open(&path) {
        let _ = writeln!(f, "{} [{}] {msg}", utc_now(), crate::APP_VERSION);
    }
}

/// 用默认程序打开日志文件（没有时先建一个空的）。
pub fn open() -> Result<(), String> {
    let path = path().ok_or("找不到日志目录")?;
    if !path.exists() {
        write("（打开日志）");
    }
    #[cfg(target_os = "macos")]
    let r = std::process::Command::new("/usr/bin/open").arg("-t").arg(&path).spawn();
    #[cfg(windows)]
    let r = std::process::Command::new("notepad.exe").arg(&path).spawn();
    #[cfg(not(any(windows, target_os = "macos")))]
    let r = std::process::Command::new("xdg-open").arg(&path).spawn();
    r.map(|_| ()).map_err(|e| format!("打开日志失败：{e}"))
}

/// 日志宏：用法同 eprintln!。
#[macro_export]
macro_rules! log {
    ($($arg:tt)*) => { $crate::shell_log::write(&format!($($arg)*)) };
}

/// UTC 时间 2026-11-16 09:30:00Z（不为此引入时间库）。
fn utc_now() -> String {
    let secs = SystemTime::now().duration_since(UNIX_EPOCH).map(|d| d.as_secs() as i64).unwrap_or(0);
    let (days, rem) = (secs.div_euclid(86400), secs.rem_euclid(86400));
    // Howard Hinnant 的 civil_from_days
    let z = days + 719_468;
    let era = z.div_euclid(146_097);
    let doe = z - era * 146_097;
    let yoe = (doe - doe / 1460 + doe / 36524 - doe / 146_096) / 365;
    let doy = doe - (365 * yoe + yoe / 4 - yoe / 100);
    let mp = (5 * doy + 2) / 153;
    let d = doy - (153 * mp + 2) / 5 + 1;
    let m = if mp < 10 { mp + 3 } else { mp - 9 };
    let y = yoe + era * 400 + i64::from(m <= 2);
    format!("{y:04}-{m:02}-{d:02} {:02}:{:02}:{:02}Z", rem / 3600, rem % 3600 / 60, rem % 60)
}

#[cfg(test)]
mod tests {
    #[test]
    fn utc_format_shape() {
        let s = super::utc_now();
        assert_eq!(s.len(), 20, "{s}");
        assert!(s.starts_with("20") && s.ends_with('Z'));
    }
}
