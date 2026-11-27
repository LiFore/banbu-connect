// 下载页（S1.11）：后台服务以系统权限运行，不能替用户写文件，下载由外壳在用户会话里做。
// 后台服务把下载、打开文件、在文件夹中显示排队（状态里的 file_action），外壳认领后执行：
//   下载：从本机服务取文件内容（GET /api/v1/desktop/files/jobs/<id>/blob，本机服务再去管理服务或 OSS 下载），
//         写到「下载」目录的 <文件名>.part，每 0.5 秒报一次进度（回复里 cancel 为真时停下），完成后改名；
//   reveal / open：只处理「下载」目录里的文件（路径是外壳自己报上去的）。

use std::collections::HashSet;
use std::fs::File;
use std::io::{Read, Write};
use std::path::{Path, PathBuf};
use std::process::Command;
use std::sync::Mutex;
use std::time::{Duration, Instant};

use serde::Deserialize;
use serde_json::{json, Value};

use crate::{read_token, SERVICE};

const PROGRESS_EVERY: Duration = Duration::from_millis(500);

static STARTED: Mutex<Option<HashSet<String>>> = Mutex::new(None);

/// 每次读到状态时调用：有新的待执行操作就开一个线程去做（多个下载可以同时进行）。
pub fn tick(action: Option<String>) {
    let Some(id) = action else { return };
    let mut started = STARTED.lock().unwrap();
    let set = started.get_or_insert_with(HashSet::new);
    if !set.insert(id.clone()) {
        return;
    }
    if set.len() > 200 {
        set.clear();
        set.insert(id.clone());
    }
    drop(started);
    std::thread::spawn(move || run(&id));
}

#[derive(Deserialize, Default)]
struct Job {
    action: String,
    #[serde(default)]
    file_name: String,
    #[serde(default)]
    path: String,
}

fn agent() -> ureq::Agent {
    // 本机服务，不走系统代理；大文件下载不限总时长，只限连接和单次读取。
    ureq::AgentBuilder::new()
        .timeout_connect(Duration::from_secs(5))
        .timeout_read(Duration::from_secs(120))
        .build()
}

fn request(method: &str, path: &str, body: Value) -> Result<Value, String> {
    let token = read_token().ok_or("读不到访问令牌")?;
    let result = agent()
        .request(method, &format!("{SERVICE}/api/v1/desktop/files/jobs{path}"))
        .set("Authorization", &format!("Bearer {token}"))
        .set("Content-Type", "application/json")
        .send_string(&body.to_string());
    match result {
        Ok(resp) => resp.into_json().map_err(|e| e.to_string()),
        Err(ureq::Error::Status(_, resp)) => {
            let v: Value = resp.into_json().unwrap_or_default();
            Err(v["message"].as_str().unwrap_or("操作失败").to_string())
        }
        Err(e) => Err(e.to_string()),
    }
}

fn run(id: &str) {
    let job: Job = match request("POST", &format!("/{id}/claim"), json!({})) {
        Ok(v) => serde_json::from_value(v).unwrap_or_default(),
        Err(e) => {
            log!("认领文件操作失败：{e}");
            return;
        }
    };
    let result = match job.action.as_str() {
        "download" => download(id, &job.file_name).map(Some),
        "reveal" => reveal(&job.path).map(|_| None),
        "open" => open(&job.path).map(|_| None),
        _ => Err(Outcome::Failed("不支持的操作".into())),
    };
    let body = match result {
        Ok(path) => json!({ "ok": true, "path": path.map(|p| p.to_string_lossy().to_string()).unwrap_or_default() }),
        Err(Outcome::Canceled) => json!({ "canceled": true }),
        Err(Outcome::Failed(e)) => {
            log!("文件操作 {} 失败：{e}", job.action);
            json!({ "error": e })
        }
    };
    if let Err(e) = request("POST", &format!("/{id}/result"), body) {
        log!("回报文件操作结果失败：{e}");
    }
}

enum Outcome {
    Canceled,
    Failed(String),
}

impl From<String> for Outcome {
    fn from(s: String) -> Self {
        Outcome::Failed(s)
    }
}

// ---- 下载 ----

/// 用户的「下载」目录。
pub fn downloads_dir() -> Option<PathBuf> {
    let home = std::env::var_os(if cfg!(windows) { "USERPROFILE" } else { "HOME" })?;
    Some(PathBuf::from(home).join("Downloads"))
}

/// 清理文件名：去掉路径分隔符和 Windows 不允许的字符。
fn clean_name(name: &str) -> String {
    let s: String = name
        .chars()
        .map(|c| if c.is_control() || r#"<>:"/\|?*"#.contains(c) { '_' } else { c })
        .collect();
    let s = s.trim().trim_matches('.').to_string();
    if s.is_empty() {
        "download".into()
    } else {
        s
    }
}

/// 不覆盖已有文件：重名时加 (1)、(2)……
fn unique_path(dir: &Path, name: &str) -> PathBuf {
    let p = dir.join(name);
    if !p.exists() {
        return p;
    }
    let (stem, ext) = match name.rfind('.') {
        Some(i) if i > 0 => (&name[..i], &name[i..]),
        _ => (name, ""),
    };
    (1..1000)
        .map(|n| dir.join(format!("{stem} ({n}){ext}")))
        .find(|p| !p.exists())
        .unwrap_or_else(|| dir.join(format!("{stem} ({}){ext}", std::process::id())))
}

fn download(id: &str, file_name: &str) -> Result<PathBuf, Outcome> {
    let dir = downloads_dir().ok_or_else(|| "找不到「下载」目录".to_string())?;
    std::fs::create_dir_all(&dir).map_err(|e| format!("创建「下载」目录失败：{e}"))?;
    let name = clean_name(file_name);
    let part = unique_path(&dir, &format!("{name}.part"));

    let token = read_token().ok_or_else(|| "读不到访问令牌".to_string())?;
    let resp = agent()
        .get(&format!("{SERVICE}/api/v1/desktop/files/jobs/{id}/blob"))
        .set("Authorization", &format!("Bearer {token}"))
        .call()
        .map_err(|e| match e {
            ureq::Error::Status(_, r) => {
                let v: Value = r.into_json().unwrap_or_default();
                v["message"].as_str().unwrap_or("下载失败").to_string()
            }
            e => format!("下载失败：{e}"),
        })?;
    let total: u64 = resp.header("Content-Length").and_then(|v| v.parse().ok()).unwrap_or(0);
    let mut reader = resp.into_reader();
    let result = (|| -> Result<(), Outcome> {
        let mut out = File::create(&part).map_err(|e| format!("创建文件失败：{e}"))?;
        let mut buf = vec![0u8; 256 * 1024];
        let mut done: u64 = 0;
        let mut last = Instant::now();
        loop {
            let n = reader.read(&mut buf).map_err(|e| format!("下载中断：{e}"))?;
            if n == 0 {
                break;
            }
            out.write_all(&buf[..n]).map_err(|e| format!("写入文件失败：{e}"))?;
            done += n as u64;
            if last.elapsed() >= PROGRESS_EVERY {
                last = Instant::now();
                if report_progress(id, done, total) {
                    return Err(Outcome::Canceled);
                }
            }
        }
        out.flush().map_err(|e| format!("写入文件失败：{e}"))?;
        if total > 0 && done != total {
            return Err(Outcome::Failed(format!("下载不完整：{done} / {total} 字节")));
        }
        report_progress(id, done, total);
        Ok(())
    })();
    if let Err(e) = result {
        let _ = std::fs::remove_file(&part);
        return Err(e);
    }
    let target = unique_path(&dir, &name);
    std::fs::rename(&part, &target).map_err(|e| {
        let _ = std::fs::remove_file(&part);
        format!("保存文件失败：{e}")
    })?;
    Ok(target)
}

/// 报进度，返回是否要取消。报不上去时继续下载。
fn report_progress(id: &str, downloaded: u64, total: u64) -> bool {
    match request("PUT", &format!("/{id}/progress"), json!({ "downloaded": downloaded, "total": total })) {
        Ok(v) => v["cancel"].as_bool().unwrap_or(false),
        Err(_) => false,
    }
}

// ---- 打开 ----

/// 只处理「下载」目录里存在的文件。
fn checked(path: &str) -> Result<PathBuf, Outcome> {
    let p = PathBuf::from(path);
    let dir = downloads_dir().ok_or_else(|| "找不到「下载」目录".to_string())?;
    if !p.starts_with(&dir) || p.components().any(|c| matches!(c, std::path::Component::ParentDir)) {
        return Err(Outcome::Failed("只能打开「下载」目录里的文件".into()));
    }
    if !p.exists() {
        return Err(Outcome::Failed("文件已被移走或删除".into()));
    }
    Ok(p)
}

fn spawn(mut cmd: Command) -> Result<(), Outcome> {
    cmd.spawn().map(|_| ()).map_err(|e| Outcome::Failed(format!("打开失败：{e}")))
}

fn reveal(path: &str) -> Result<(), Outcome> {
    let p = checked(path)?;
    #[cfg(target_os = "macos")]
    {
        let mut c = Command::new("open");
        c.arg("-R").arg(&p);
        spawn(c)
    }
    #[cfg(windows)]
    {
        use std::os::windows::process::CommandExt;
        let mut c = Command::new("explorer");
        c.raw_arg(format!("/select,\"{}\"", p.display()));
        spawn(c)
    }
    #[cfg(not(any(target_os = "macos", windows)))]
    {
        let mut c = Command::new("xdg-open");
        c.arg(p.parent().unwrap_or(&p));
        spawn(c)
    }
}

fn open(path: &str) -> Result<(), Outcome> {
    let p = checked(path)?;
    #[cfg(target_os = "macos")]
    let c = {
        let mut c = Command::new("open");
        c.arg(&p);
        c
    };
    #[cfg(windows)]
    let c = {
        let mut c = Command::new("explorer");
        c.arg(&p);
        c
    };
    #[cfg(not(any(target_os = "macos", windows)))]
    let c = {
        let mut c = Command::new("xdg-open");
        c.arg(&p);
        c
    };
    spawn(c)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn cleans_names() {
        assert_eq!(clean_name("../a/b:c.zip"), "_a_b_c.zip");
        assert_eq!(clean_name("  安装 指南.md "), "安装 指南.md");
        assert_eq!(clean_name(".."), "download");
    }

    #[test]
    fn unique_names_do_not_overwrite() {
        let dir = std::env::temp_dir().join(format!("banbu-files-test-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        std::fs::write(dir.join("a.zip"), b"x").unwrap();
        assert_eq!(unique_path(&dir, "a.zip"), dir.join("a (1).zip"));
        assert_eq!(unique_path(&dir, "b"), dir.join("b"));
        std::fs::remove_dir_all(&dir).unwrap();
    }
}
