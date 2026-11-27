// 打开 Claude Desktop（ADR-088）：折中版，用 Claude Desktop 自己的登录，不按账号隔离。
// 后台服务排队前已做过保护检查（和启动 claude 一样），这里只负责找到应用并打开；已在运行时调到前面。
// macOS bundle id 是 com.anthropic.claudefordesktop；Windows 是 %LOCALAPPDATA%\AnthropicClaude\claude.exe（Squirrel 安装），
// 找不到时按开始菜单里名为 Claude 的应用打开（MSIX 安装）。
// TODO(verify:V-claude-desktop-open) 本机没装 Claude Desktop，bundle id、Windows 路径都未实测。

#[cfg(target_os = "macos")]
use std::process::Command;

#[cfg(target_os = "macos")]
const BUNDLE_ID: &str = "com.anthropic.claudefordesktop";

#[cfg(target_os = "macos")]
pub fn open() -> Result<(), String> {
    // 先按 bundle id 交给 LaunchServices（装在哪都能找到）；不行再试常见路径。
    if Command::new("/usr/bin/open").args(["-b", BUNDLE_ID]).status().map(|s| s.success()).unwrap_or(false) {
        return Ok(());
    }
    let cands = [Some(std::path::PathBuf::from("/Applications")), crate::workspaces::home().map(|h| h.join("Applications"))];
    let app = cands.into_iter().flatten().map(|b| b.join("Claude.app")).find(|p| p.exists()).ok_or("没有找到 Claude Desktop，请先安装（claude.ai/download）")?;
    let status = Command::new("/usr/bin/open").arg("-a").arg(&app).status().map_err(|e| format!("打开 Claude Desktop 失败：{e}"))?;
    if !status.success() {
        return Err(format!("打开 Claude Desktop 失败（退出码 {:?}）", status.code()));
    }
    Ok(())
}

#[cfg(windows)]
pub fn open() -> Result<(), String> {
    use crate::workspaces::hidden;
    if let Some(l) = std::env::var_os("LOCALAPPDATA") {
        let exe = std::path::PathBuf::from(l).join("AnthropicClaude").join("claude.exe");
        if exe.is_file() {
            return std::process::Command::new(&exe).spawn().map(|_| ()).map_err(|e| format!("打开 Claude Desktop 失败：{e}"));
        }
    }
    // MSIX 安装：从开始菜单的应用列表里找 AppID，经 shell:AppsFolder 打开。
    let out = hidden("powershell.exe")
        .args(["-NoProfile", "-NonInteractive", "-Command", "(Get-StartApps | Where-Object { $_.Name -eq 'Claude' } | Select-Object -First 1).AppID"])
        .output()
        .map_err(|e| format!("运行 PowerShell 失败：{e}"))?;
    let id = String::from_utf8_lossy(&out.stdout).trim().to_string();
    if id.is_empty() {
        return Err("没有找到 Claude Desktop，请先安装（claude.ai/download）".into());
    }
    hidden("explorer.exe").arg(format!("shell:AppsFolder\\{id}")).spawn().map(|_| ()).map_err(|e| format!("打开 Claude Desktop 失败：{e}"))
}

#[cfg(not(any(windows, target_os = "macos")))]
pub fn open() -> Result<(), String> {
    Err("这个系统没有 Claude Desktop".into())
}
