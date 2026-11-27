// 用户空闲时间（S1.10 静默更新：装好后等用户空闲再重启应用，ADR-057）。
// 读不到时返回 None，调用方不重启（等下次退出或重启生效）。

use std::time::Duration;

#[cfg(target_os = "macos")]
pub fn idle() -> Option<Duration> {
    // ioreg 的 HIDIdleTime 是距最近一次键盘鼠标输入的纳秒数。
    let out = std::process::Command::new("/usr/sbin/ioreg").args(["-c", "IOHIDSystem", "-d", "4"]).output().ok()?;
    let text = String::from_utf8_lossy(&out.stdout);
    let line = text.lines().find(|l| l.contains("\"HIDIdleTime\""))?;
    let ns: u64 = line.rsplit('=').next()?.trim().parse().ok()?;
    Some(Duration::from_nanos(ns))
}

#[cfg(windows)]
pub fn idle() -> Option<Duration> {
    use windows_sys::Win32::System::SystemInformation::GetTickCount;
    use windows_sys::Win32::UI::Input::KeyboardAndMouse::{GetLastInputInfo, LASTINPUTINFO};
    let mut info = LASTINPUTINFO { cbSize: std::mem::size_of::<LASTINPUTINFO>() as u32, dwTime: 0 };
    // SAFETY: info 是合法的 LASTINPUTINFO，cbSize 已填。
    let ok = unsafe { GetLastInputInfo(&mut info) };
    if ok == 0 {
        return None;
    }
    // SAFETY: 无参数。两者都是开机以来的毫秒数（32 位，约 49 天回绕，用 wrapping_sub 处理）。
    let now = unsafe { GetTickCount() };
    Some(Duration::from_millis(now.wrapping_sub(info.dwTime) as u64))
}

#[cfg(not(any(windows, target_os = "macos")))]
pub fn idle() -> Option<Duration> {
    None
}
