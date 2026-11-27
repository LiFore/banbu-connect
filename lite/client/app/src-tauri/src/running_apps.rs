// 退出前检查还在运行的 AI 应用（精简页「退出」确认框用），提醒用户先关掉。
// 每次调用现查一次进程列表（工作区扫描 30 秒一次，太旧）。只看进程名和路径，不读别的。
// macOS：ps 的命令行里认 claude / codex 命令和 Claude、ChatGPT（含旧名 Codex）、Antigravity 应用；
// Windows：tasklist 只有映像名，claude.exe 分不清是 Claude Code 还是 Claude Desktop，统称 Claude。
// TODO(verify:V-quit-running-apps) Windows 映像名、Antigravity / Claude Desktop 的 macOS 路径未在真机确认。

use std::collections::BTreeSet;

/// 现在在运行的 AI 应用名（去重、排好序）。查不到进程列表时返回空。
pub fn list() -> Vec<String> {
    #[cfg(target_os = "macos")]
    {
        let Ok(out) = std::process::Command::new("/bin/ps").args(["-axww", "-o", "command="]).output() else { return Vec::new() };
        from_ps(&String::from_utf8_lossy(&out.stdout))
    }
    #[cfg(windows)]
    {
        let Ok(out) = crate::workspaces::hidden("tasklist.exe").args(["/FO", "CSV", "/NH"]).output() else { return Vec::new() };
        from_tasklist(&String::from_utf8_lossy(&out.stdout))
    }
    #[cfg(not(any(windows, target_os = "macos")))]
    {
        Vec::new()
    }
}

#[cfg_attr(not(target_os = "macos"), allow(dead_code))]
fn from_ps(text: &str) -> Vec<String> {
    let mut set = BTreeSet::new();
    for line in text.lines() {
        let app = |name: &str| line.contains(&format!("/{name}.app/Contents/"));
        if app("Claude") {
            set.insert("Claude Desktop");
        } else if app("ChatGPT") || app("Codex") {
            set.insert("ChatGPT");
        } else if app("Antigravity") {
            set.insert("Antigravity");
        } else {
            // 命令行工具：前两个参数里有 claude / codex（claude 可能以 node 运行）。
            for t in line.split_whitespace().take(2) {
                match t.rsplit('/').next().unwrap_or(t) {
                    "claude" => {
                        set.insert("Claude Code");
                    }
                    "codex" => {
                        set.insert("Codex");
                    }
                    _ => {}
                }
            }
        }
    }
    set.into_iter().map(String::from).collect()
}

#[cfg_attr(not(windows), allow(dead_code))]
fn from_tasklist(text: &str) -> Vec<String> {
    let mut set = BTreeSet::new();
    for line in text.lines() {
        let name = line.split(',').next().unwrap_or_default().trim_matches('"').to_lowercase();
        let app = match name.as_str() {
            "claude.exe" => "Claude",
            "codex.exe" => "Codex",
            "chatgpt.exe" => "ChatGPT",
            "antigravity.exe" => "Antigravity",
            _ => continue,
        };
        set.insert(app);
    }
    set.into_iter().map(String::from).collect()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn picks_ai_apps_from_ps() {
        let ps = "/Applications/Claude.app/Contents/MacOS/Claude\n\
                  /Applications/Claude.app/Contents/Frameworks/Claude Helper.app/Contents/MacOS/Claude Helper --type=renderer\n\
                  /Applications/ChatGPT.app/Contents/Resources/codex-cli/codex app-server\n\
                  node /Users/a/.npm-global/bin/claude --resume\n\
                  /opt/homebrew/bin/codex\n\
                  /Applications/Antigravity.app/Contents/MacOS/Electron\n\
                  /usr/bin/vim claude.md\n";
        assert_eq!(from_ps(ps), ["Antigravity", "ChatGPT", "Claude Code", "Claude Desktop", "Codex"]);
        assert!(from_ps("/usr/bin/vim notes-about-claude\n").is_empty());
    }

    #[test]
    fn picks_ai_apps_from_tasklist() {
        let t = "\"claude.exe\",\"1\",\"Console\",\"1\",\"10 K\"\r\n\"Antigravity.exe\",\"2\",\"Console\",\"1\",\"10 K\"\r\n\"explorer.exe\",\"3\",\"Console\",\"1\",\"10 K\"\r\n";
        assert_eq!(from_tasklist(t), ["Antigravity", "Claude"]);
    }
}
