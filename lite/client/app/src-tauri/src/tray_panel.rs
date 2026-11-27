// 托盘弹出面板：左键点托盘图标弹出的小窗口（ui/tray.html），显示后台进程和管理服务的状态、
// 打开主界面和退出。右键仍是系统菜单（不常用的操作和退出）。
// 面板是本地页面，经下面的命令向外壳要数据、发操作；外壳再带本机令牌请求后台服务，页面拿不到令牌。

use std::sync::Mutex;
use std::time::{Duration, Instant};

use tauri::{AppHandle, Manager, PhysicalPosition, Rect, WebviewUrl, WebviewWindow, WebviewWindowBuilder, WindowEvent};

pub const LABEL: &str = "tray";
/// 面板的逻辑尺寸（与 ui/tray.html 的布局对应）。
const WIDTH: f64 = 300.0;
const HEIGHT: f64 = 220.0;
/// 面板和托盘图标、屏幕边缘的间距（逻辑像素）。
const GAP: f64 = 6.0;
const MARGIN: f64 = 8.0;

/// 面板因失去焦点而隐藏的时间：点托盘图标时面板先失焦隐藏、再收到点击，
/// 不能紧接着又显示出来（用户本意是关掉）。
static LAST_HIDE: Mutex<Option<Instant>> = Mutex::new(None);
const REOPEN_GUARD: Duration = Duration::from_millis(300);

fn panel(app: &AppHandle) -> Option<WebviewWindow> {
    app.get_webview_window(LABEL)
}

/// 启动时建好面板窗口（隐藏），点击时只需定位和显示，弹出没有延迟。
pub fn create(app: &AppHandle) -> tauri::Result<()> {
    let window = WebviewWindowBuilder::new(app, LABEL, WebviewUrl::App("tray.html".into()))
        .title(crate::PRODUCT)
        .inner_size(WIDTH, HEIGHT)
        .resizable(false)
        .maximizable(false)
        .minimizable(false)
        .decorations(false)
        .always_on_top(true)
        .skip_taskbar(true)
        .visible(false)
        .focused(false)
        .shadow(true)
        .build()?;
    let handle = app.clone();
    window.on_window_event(move |event| {
        if let WindowEvent::Focused(false) = event {
            hide(&handle);
        }
    });
    Ok(())
}

pub fn hide(app: &AppHandle) {
    let Some(w) = panel(app) else { return };
    if w.is_visible().unwrap_or(false) {
        *LAST_HIDE.lock().unwrap() = Some(Instant::now());
        let _ = w.hide();
        let _ = w.eval("window.dispatchEvent(new Event('tray-hidden'))");
    }
}

/// 点托盘图标：显示时隐藏，隐藏时在图标旁边弹出。
pub fn toggle(app: &AppHandle, icon: Rect) {
    let Some(w) = panel(app) else { return };
    if w.is_visible().unwrap_or(false) {
        hide(app);
        return;
    }
    if LAST_HIDE.lock().unwrap().is_some_and(|t| t.elapsed() < REOPEN_GUARD) {
        return;
    }
    place(app, &w, icon);
    let _ = w.show();
    let _ = w.set_focus();
    let _ = w.eval("window.dispatchEvent(new Event('tray-shown'))");
}

/// 面板放在图标旁边：图标在屏幕上半部分（macOS 菜单栏、顶部任务栏）时放在下方，否则放在上方（Windows 底部任务栏）；
/// 水平方向以图标为中心，不超出屏幕可用区域。
fn place(app: &AppHandle, w: &WebviewWindow, icon: Rect) {
    let monitor = app
        .monitor_from_point(icon_center(&icon, 1.0).0, icon_center(&icon, 1.0).1)
        .ok()
        .flatten()
        .or_else(|| app.primary_monitor().ok().flatten());
    let Some(monitor) = monitor else { return };
    let scale = monitor.scale_factor();
    let area = monitor.work_area();
    let (ax, ay) = (area.position.x as f64, area.position.y as f64);
    let (aw, ah) = (area.size.width as f64, area.size.height as f64);
    let pos = icon.position.to_physical::<f64>(scale);
    let size = icon.size.to_physical::<f64>(scale);
    let (pw, ph) = (WIDTH * scale, HEIGHT * scale);
    let (gap, margin) = (GAP * scale, MARGIN * scale);

    let center_x = pos.x + size.width / 2.0;
    let mut x = center_x - pw / 2.0;
    x = x.min(ax + aw - pw - margin).max(ax + margin);
    let below = pos.y + size.height / 2.0 < ay + ah / 2.0;
    let mut y = if below { pos.y + size.height + gap } else { pos.y - ph - gap };
    y = y.min(ay + ah - ph - margin).max(ay + margin);
    let _ = w.set_position(PhysicalPosition::new(x.round() as i32, y.round() as i32));
}

fn icon_center(icon: &Rect, scale: f64) -> (f64, f64) {
    let pos = icon.position.to_physical::<f64>(scale);
    let size = icon.size.to_physical::<f64>(scale);
    (pos.x + size.width / 2.0, pos.y + size.height / 2.0)
}
