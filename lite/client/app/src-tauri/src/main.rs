// 托盘和窗口外壳（Banbu Connect Lite）。账号管理的逻辑都在后台进程 boxctl desktop 里，
// 这里拉起后台进程（backend.rs）、显示状态、转发操作。Lite 版不做代理，不需要管理员权限。
#![cfg_attr(not(debug_assertions), windows_subsystem = "windows")]

#[macro_use]
mod shell_log;
mod backend;
mod chatgpt_app;
mod claude_app;
mod running_apps;
mod claude_login;
mod claude_otel;
mod gemini;
mod gemini_store;
mod idle;
mod workspaces;
mod files;
mod tray_icon;
mod tray_panel;

use std::path::PathBuf;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::Mutex;
use std::time::{Duration, Instant};

use serde::Deserialize;
use tauri::image::Image;
use tauri::menu::{Menu, MenuItem, PredefinedMenuItem};
use tauri::tray::{MouseButton, MouseButtonState, TrayIconBuilder, TrayIconEvent};
use tauri::{AppHandle, Manager, Url, WebviewUrl, WebviewWindow, WebviewWindowBuilder, Wry};
use tauri_plugin_notification::NotificationExt;
use tauri_plugin_updater::{Update, UpdaterExt};

/// 后台进程的地址，和完整版 Banbu Connect（17930）分开，两者可以同时运行。
const SERVICE: &str = "http://127.0.0.1:17940";
const PRODUCT: &str = "Banbu Connect Lite";
const POLL: Duration = Duration::from_secs(3);
/// 静默更新装好后，用户空闲这么久就重启应用。
const IDLE_RESTART: Duration = Duration::from_secs(10 * 60);
const UPDATE_EVERY: Duration = Duration::from_secs(3600); // 在后台时每小时检查一次；启动和打开主界面时另外各查一次
/// 应用版本，取自 tauri.conf.json（见 build.rs），与 updater 上报的、后台进程的版本同源。
const APP_VERSION: &str = env!("BANBU_APP_VERSION");
const SHELL: &str = if cfg!(target_os = "macos") { "mac" } else { "win" };
const SHELL_SCRIPT: &str = if cfg!(target_os = "macos") {
    "document.documentElement.dataset.shell='mac';try{sessionStorage.setItem('boxctl_shell','mac')}catch(e){};window.dispatchEvent(new Event('banbu-shell'))"
} else {
    "document.documentElement.dataset.shell='win';try{sessionStorage.setItem('boxctl_shell','win')}catch(e){};window.dispatchEvent(new Event('banbu-shell'))"
};
const TEMPLATE_ICON: bool = cfg!(target_os = "macos");
const ICON_SIZE: usize = if TEMPLATE_ICON { 36 } else { 32 }; // macOS 菜单栏图标高 18pt，Retina 下为 36 像素

/// 管理服务的绑定状态（GET /api/v1/desktop/server 同一份）。
#[derive(Deserialize, Clone, PartialEq, Default)]
struct Server {
    #[serde(default)]
    bound: bool,
    #[serde(default)]
    url: String,
    #[serde(default)]
    last_error: String,
}

#[derive(Deserialize, Clone, PartialEq, Default)]
struct Notice {
    /// 单调递增（纳秒时间戳），后台进程重启后也不回退。
    id: i64,
    title: String,
    body: String,
}

#[derive(Deserialize, Clone, PartialEq, Default)]
struct Summary {
    #[serde(default)]
    version: String,
    #[serde(default)]
    server: Server,
    /// 后台进程排好的系统通知（续期提醒、更新），按时间先后。
    #[serde(default, deserialize_with = "null_default")]
    notices: Vec<Notice>,
    /// 待外壳启动的 Claude Code 登录（页面上点了「登录」）。
    #[serde(default)]
    claude_login: Option<claude_login::PendingClaude>,
    /// 主界面请求在浏览器打开的登录链接。
    #[serde(default)]
    open_link: Option<claude_login::PendingOpen>,
    /// Claude Code 遥测：外壳据此自动配置用户的 settings.json。
    #[serde(default)]
    otel: Option<claude_otel::OTelBrief>,
    /// 首页点了「更新」或「立即重启」，待外壳处理。
    #[serde(default)]
    app_update: Option<AppUpdateRequest>,
    /// Gemini（Antigravity）账号：待写入的凭据、待打开的浏览器档案。
    #[serde(default)]
    gemini: Option<gemini::Brief>,
    /// 待执行的多开工作区操作（S1.9）。
    #[serde(default)]
    workspace_action: Option<String>,
    /// 待执行的下载、打开文件、在文件夹中显示（S1.11）。
    #[serde(default)]
    file_action: Option<String>,
}

#[derive(Deserialize, Clone, PartialEq, Default)]
struct AppUpdateRequest {
    id: String,
    action: String, // install / restart
}

/// 右键菜单里会变的项。状态在左键弹出的面板里（tray_panel.rs、ui/tray.html）。
struct TrayItems {
    status: MenuItem<Wry>,
    update: MenuItem<Wry>,
}

#[derive(Default)]
struct AppState {
    items: Mutex<Option<TrayItems>>,
    last: Mutex<Option<Summary>>,
    // 已下载信息、等待用户确认安装的更新。
    pending: Mutex<Option<Update>>,
    // 已启动过的 Claude 登录，避免下一次轮询重复启动。
    claude_started: Mutex<Option<String>>,
    // 已处理过的待打开链接。
    open_handled: Mutex<Option<String>>,
    // 已处理过的浏览器档案请求。
    browser_handled: Mutex<Option<String>>,
    // Claude Code 遥测配置的核对节奏。
    otel: Mutex<claude_otel::Syncer>,
    // 正在检查更新：启动、打开主界面和定时检查可能同时触发，只跑一个。
    checking: AtomicBool,
    // 上次自动检查完成的时间：一分钟内不重复检查（比如启动时就弹出主界面）。
    last_check: Mutex<Option<Instant>>,
    // 已弹出的最后一条通知；None 表示还没连上过后台进程。
    last_notice: Mutex<Option<i64>>,
    // 已处理过的首页更新请求。
    app_update_handled: Mutex<Option<String>>,
    // 已读到过状态：第一次读到时，后台进程里留着的重启请求是给上一个外壳的，见 claim_app_update。
    status_seen: AtomicBool,
    // 已为哪个后台进程版本重启过：同一版本只重启一次，避免反复重启。
    outdated_restarted: Mutex<Option<String>>,
    // 本地启动页的地址（启动时窗口的初始地址），后台进程不可用时从管理界面切回来。
    start_page: Mutex<Option<Url>>,
    // 更新已安装、等待重启：托盘菜单的「更新」项变成重启。
    restart_ready: Mutex<bool>,
    // 「更新」项当前的文字（检查结果、下载进度），托盘面板也显示它。
    update_text: Mutex<Option<String>>,
}

// ---- 令牌和后台进程 ----

/// 数据目录，与后台进程的一致（boxctl/internal/desktop/platform_*.go）：令牌、外壳日志都在这里。
fn data_dir() -> Option<PathBuf> {
    #[cfg(windows)]
    let base = PathBuf::from(std::env::var_os("LOCALAPPDATA")?);
    #[cfg(not(windows))]
    let base = PathBuf::from(std::env::var_os("HOME")?)
        .join("Library")
        .join("Application Support");
    Some(base.join(PRODUCT))
}

/// 令牌由后台进程启动时写入。
fn token_path() -> Option<PathBuf> {
    Some(data_dir()?.join("token"))
}

fn read_token() -> Option<String> {
    let token = std::fs::read_to_string(token_path()?).ok()?;
    let token = token.trim();
    (!token.is_empty()).then(|| token.to_string())
}

fn agent() -> ureq::Agent {
    // 后台进程在本机，不能走系统代理。ureq 默认不读系统代理设置。
    ureq::AgentBuilder::new()
        .timeout(Duration::from_secs(5))
        .build()
}

/// 读后台进程状态。连不上、令牌不对、内容解析不了都返回 None（托盘显示「后台进程未连接」），
/// 但原因写进日志。
fn fetch_status(token: &str) -> Option<Summary> {
    let result = agent()
        .get(&format!("{SERVICE}/api/v1/desktop/status"))
        .set("Authorization", &format!("Bearer {token}"))
        .call()
        .map_err(|e| format!("读后台进程状态失败：{e}"))
        .and_then(|r| r.into_string().map_err(|e| format!("读后台进程状态失败：{e}")))
        .and_then(|body| parse_status(&body));
    match result {
        Ok(s) => {
            *STATUS_ERROR.lock().unwrap() = None;
            Some(s)
        }
        Err(e) => {
            // 同样的错误只记一次，轮询每 3 秒一次，不能刷屏。
            let mut last = STATUS_ERROR.lock().unwrap();
            if last.as_deref() != Some(e.as_str()) {
                log!("{e}");
                *last = Some(e);
            }
            None
        }
    }
}

static STATUS_ERROR: Mutex<Option<String>> = Mutex::new(None);

fn parse_status(body: &str) -> Result<Summary, String> {
    serde_json::from_str(body).map_err(|e| format!("后台进程状态解析失败（应用和后台进程版本可能不匹配）：{e}"))
}

/// 服务把空列表写成 null 时按空处理（serde 的 default 只管字段缺失，不管 null）。
pub(crate) fn null_default<'de, D, T>(d: D) -> Result<T, D::Error>
where
    D: serde::Deserializer<'de>,
    T: Default + Deserialize<'de>,
{
    Ok(Option::<T>::deserialize(d)?.unwrap_or_default())
}

/// 调用后台服务；错误信息来自服务返回的 message。
fn call(method: &str, path: &str, body: serde_json::Value) -> Result<(), String> {
    let token = read_token().ok_or("读不到访问令牌")?;
    let result = agent()
        .request(method, &format!("{SERVICE}/api/v1{path}"))
        .set("Authorization", &format!("Bearer {token}"))
        .set("Content-Type", "application/json")
        .send_string(&body.to_string());
    match result {
        Ok(_) => Ok(()),
        Err(ureq::Error::Status(_, resp)) => {
            let v: serde_json::Value = resp.into_json().unwrap_or_default();
            Err(v["message"].as_str().unwrap_or("操作失败").to_string())
        }
        Err(e) => Err(e.to_string()),
    }
}

// ---- 启动页调用的命令 ----

/// 后台进程比应用旧时返回它的版本：OTA 更新后，上一个外壳拉起的旧进程可能还在应答，要重启它。
/// 早期版本不报告版本（空），同样算旧版；开发构建的版本是 dev，不比较。
fn outdated(s: &Summary) -> Option<String> {
    match s.version.as_str() {
        "" => Some("旧版本".to_string()),
        "dev" => None,
        v if older(v, APP_VERSION) => Some(v.to_string()),
        _ => None,
    }
}

/// a 是否比 b 旧（x.y.z，预发布后缀低于正式版；与服务端 dist.CompareVersions 一致）。
fn older(a: &str, b: &str) -> bool {
    fn parts(v: &str) -> ([u64; 3], Option<&str>) {
        let (core, pre) = match v.trim_start_matches('v').split_once('-') {
            Some((c, p)) => (c, Some(p)),
            None => (v.trim_start_matches('v'), None),
        };
        let mut n = [0u64; 3];
        for (i, x) in core.split('.').take(3).enumerate() {
            n[i] = x.parse().unwrap_or(0);
        }
        (n, pre)
    }
    let ((na, pa), (nb, pb)) = (parts(a), parts(b));
    if na != nb {
        return na < nb;
    }
    match (pa, pb) {
        (Some(x), Some(y)) => x < y,
        (Some(_), None) => true,
        _ => false,
    }
}

/// 后台进程可用时返回带令牌的地址（令牌放在 URL 片段里，不会发给服务器）。
fn current_service_url() -> Option<String> {
    let token = read_token()?;
    let s = fetch_status(&token)?;
    if outdated(&s).is_some() {
        return None; // 旧进程，等它重启
    }
    // shell 告诉页面系统标题栏已去掉，由页面自绘（tauri.macos.conf.json、tauri.windows.conf.json）。
    // 主窗口是精简页；完整界面在单独的窗口（open_full）。
    Some(format!("{SERVICE}{COMPACT_PATH}#token={token}&shell={SHELL}"))
}

/// 启动页用：后台进程就绪时返回地址。同步命令跑在主线程上，查状态最多等 5 秒，所以放到后台线程。
#[tauri::command]
async fn service_url() -> Option<String> {
    tauri::async_runtime::spawn_blocking(current_service_url).await.ok().flatten()
}

/// 启动页用：后台进程起不来的原因（拉起失败、启动后马上退出），正常时为 None。
#[tauri::command]
fn backend_error() -> Option<String> {
    backend::error()
}

/// 启动页的「重试」：重启后台进程。
#[tauri::command]
async fn restart_backend() {
    let _ = tauri::async_runtime::spawn_blocking(backend::restart).await;
}

// ---- 窗口 ----

fn main_window(app: &AppHandle) -> Option<WebviewWindow> {
    app.get_webview_window("main")
}

/// 用轮询缓存的状态（每 3 秒一次）判断服务是否就绪，不发请求：show_window 在主线程上调用，
/// 不能为等服务响应卡住窗口。
fn cached_service_url(app: &AppHandle) -> Option<String> {
    cached_service_page(app, COMPACT_PATH)
}

/// 管理界面某个页面的地址（带令牌）。主窗口是精简页 COMPACT_PATH，完整界面在 FULL_LABEL 窗口里打开 "/"。
fn cached_service_page(app: &AppHandle, path: &str) -> Option<String> {
    let token = read_token()?;
    let last = app.state::<AppState>().last.lock().unwrap().clone()?;
    if outdated(&last).is_some() {
        return None;
    }
    Some(format!("{SERVICE}{path}#token={token}&shell={SHELL}"))
}

/// 主窗口显示的精简页（web/src/pages/desktop/Compact.tsx）；原来的主界面放到二级窗口。
const COMPACT_PATH: &str = "/compact";
/// 完整界面窗口：精简页上点「完整界面」打开，关闭即销毁（不像主窗口那样隐藏到托盘）。
const FULL_LABEL: &str = "full";

/// 打开（或调到前面）完整界面窗口。窗口样式与主窗口一致：macOS 叠加红绿灯，Windows 自绘标题栏。
fn open_full(app: &AppHandle, path: &str) {
    if let Some(w) = app.get_webview_window(FULL_LABEL) {
        if path != "/" {
            if let Some(url) = cached_service_page(app, path).and_then(|u| Url::parse(&u).ok()) {
                let _ = w.navigate(url);
            }
        }
        let _ = w.show();
        let _ = w.unminimize();
        let _ = w.set_focus();
        return;
    }
    let Some(url) = cached_service_page(app, path).and_then(|u| Url::parse(&u).ok()) else {
        show_window(app); // 后台进程没就绪：回到主窗口（启动页）
        return;
    };
    let builder = WebviewWindowBuilder::new(app, FULL_LABEL, WebviewUrl::External(url))
        .title(PRODUCT)
        .inner_size(960.0, 820.0)
        .min_inner_size(720.0, 560.0);
    #[cfg(target_os = "macos")]
    let builder = builder
        .title_bar_style(tauri::TitleBarStyle::Overlay)
        .hidden_title(true)
        .traffic_light_position(tauri::LogicalPosition::new(16.0, 24.0));
    #[cfg(not(target_os = "macos"))]
    let builder = builder.decorations(false).shadow(true);
    match builder.build() {
        Ok(w) => {
            let _ = w.set_focus();
        }
        Err(e) => log!("打开完整界面失败：{e}"),
    }
}

/// 精简页的「完整界面」按钮；path 是要打开的完整界面页面（如账号提示里的「去处理」），
/// 只接受站内路径（以 / 开头，只含路径字符），其他一律打开首页。
#[tauri::command]
fn shell_open_full(app: AppHandle, path: Option<String>) {
    let path = path
        .filter(|p| p.starts_with('/') && !p.starts_with("//") && p.len() <= 200 && p.chars().all(|c| c.is_ascii_alphanumeric() || "/-_.".contains(c)))
        .unwrap_or_else(|| "/".to_string());
    let handle = app.clone();
    let _ = app.run_on_main_thread(move || open_full(&handle, &path));
}

/// 完整界面左上角（Windows）/ 右上角（macOS）的「精简界面」按钮：回到主窗口的精简页，关掉完整界面窗口。
#[tauri::command]
fn shell_open_compact(app: AppHandle) {
    let handle = app.clone();
    let _ = app.run_on_main_thread(move || {
        // 主窗口停在完整界面（旧版本或旧地址载入的 "/"）时换回精简页；show_window 只在不在服务页时才跳转。
        if let (Some(window), Some(url)) = (main_window(&handle), cached_service_url(&handle)) {
            let on_compact = window.url().map(|u| u.as_str().starts_with(SERVICE) && u.path().starts_with(COMPACT_PATH)).unwrap_or(false);
            if !on_compact {
                if let Ok(parsed) = Url::parse(&url) {
                    let _ = window.navigate(parsed);
                }
            }
        }
        show_window(&handle);
        if let Some(w) = handle.get_webview_window(FULL_LABEL) {
            let _ = w.close();
        }
    });
}

/// 精简页「退出」确认框：现在还在运行的 AI 应用（Claude、Codex、ChatGPT、Antigravity），提醒用户先关掉。
#[tauri::command]
async fn shell_running_apps() -> Vec<String> {
    tauri::async_runtime::spawn_blocking(running_apps::list).await.unwrap_or_default()
}

/// 精简页的「退出」：停止后台进程，退出应用。
#[tauri::command]
async fn shell_quit(app: AppHandle) {
    let _ = tauri::async_runtime::spawn_blocking(backend::stop).await;
    app.exit(0);
}

/// 托盘的「退出」：停止后台进程，退出应用。
fn quit_app(app: &AppHandle) {
    let app = app.clone();
    std::thread::spawn(move || {
        backend::stop();
        app.exit(0);
    });
}

/// 显示窗口。后台进程已就绪时直接进入管理界面，否则留在本地启动页。
fn show_window(app: &AppHandle) {
    let Some(window) = main_window(app) else { return };
    let on_service = window.url().map(|u| u.as_str().starts_with(SERVICE)).unwrap_or(false);
    let state = app.state::<AppState>();
    if let Some(url) = cached_service_url(app) {
        if !on_service {
            if let Ok(parsed) = Url::parse(&url) {
                let _ = window.navigate(parsed);
            }
        }
    } else if on_service && state.last.lock().unwrap().is_none() {
        // 后台进程不可用：从管理界面切回启动页，那里显示原因和「重试」。
        if let Some(page) = state.start_page.lock().unwrap().clone() {
            let _ = window.navigate(page);
        }
    }
    let _ = window.show();
    let _ = window.unminimize();
    let _ = window.set_focus();
    // 告诉管理界面「应用被打开了」：有强制更新时页面据此全屏拦住。
    if on_service {
        let _ = window.eval("window.dispatchEvent(new Event('banbu-shown'))");
    }
    auto_check_update(app);
}

// ---- 托盘 ----

fn tray_image(status: tray_icon::Status) -> Image<'static> {
    let rgba = if TEMPLATE_ICON {
        tray_icon::template_rgba(status, ICON_SIZE)
    } else {
        tray_icon::color_rgba(status, ICON_SIZE)
    };
    Image::new_owned(rgba, ICON_SIZE as u32, ICON_SIZE as u32)
}

fn tray_status(s: Option<&Summary>) -> tray_icon::Status {
    use tray_icon::Status;
    match s {
        None => Status::Off,
        Some(s) if !s.server.bound => Status::Warn,
        Some(s) if !s.server.last_error.is_empty() => Status::Error,
        Some(_) => Status::On,
    }
}

fn status_text(s: Option<&Summary>) -> String {
    match s {
        None => "后台进程未连接".into(),
        Some(s) if !s.server.bound => "未绑定管理服务".into(),
        Some(s) if !s.server.last_error.is_empty() => "管理服务连接失败".into(),
        Some(s) => format!("已连接 {}", host_of(&s.server.url)),
    }
}

fn host_of(url: &str) -> &str {
    let rest = url.split_once("://").map_or(url, |(_, r)| r);
    rest.split('/').next().unwrap_or(rest)
}

fn apply_status(app: &AppHandle, summary: Option<Summary>) {
    let state = app.state::<AppState>();
    {
        let mut last = state.last.lock().unwrap();
        if *last == summary {
            return;
        }
        last.clone_from(&summary);
    }
    let guard = state.items.lock().unwrap();
    let Some(items) = guard.as_ref() else { return };
    let text = status_text(summary.as_ref());
    let _ = items.status.set_text(&text);
    if let Some(tray) = app.tray_by_id("main") {
        // 更换图标时同时声明模板属性，否则 macOS 上可能不再随系统明暗变色。
        let _ = tray.set_icon_with_as_template(Some(tray_image(tray_status(summary.as_ref()))), TEMPLATE_ICON);
        let _ = tray.set_tooltip(Some(format!("{PRODUCT}\n{text}")));
    }
}

fn poll(app: AppHandle) {
    std::thread::spawn(move || loop {
        let summary = read_token().and_then(|t| fetch_status(&t));
        if let Some(p) = summary.as_ref().and_then(|s| s.claude_login.clone()) {
            let state = app.state::<AppState>();
            let mut started = state.claude_started.lock().unwrap();
            if started.as_deref() != Some(p.id.as_str()) {
                *started = Some(p.id.clone());
                claude_login::run(p);
            }
        }
        if let Some(o) = summary.as_ref().and_then(|s| s.open_link.clone()) {
            let state = app.state::<AppState>();
            let mut handled = state.open_handled.lock().unwrap();
            if handled.as_deref() != Some(o.id.as_str()) {
                *handled = Some(o.id.clone());
                claude_login::open_pending(o);
            }
        }
        if let Some(g) = summary.as_ref().and_then(|s| s.gemini.as_ref()) {
            gemini::tick(&app, g);
        }
        if let Some(s) = &summary {
            workspaces::tick(s.workspace_action.clone());
            files::tick(s.file_action.clone());
        }
        if let Some(b) = summary.as_ref().and_then(|s| s.gemini.as_ref()).and_then(|g| g.browser.clone()) {
            let state = app.state::<AppState>();
            let mut handled = state.browser_handled.lock().unwrap();
            if handled.as_deref() != Some(b.id.as_str()) {
                *handled = Some(b.id.clone());
                gemini::open_browser(b);
            }
        }
        if let Some(o) = summary.as_ref().and_then(|s| s.otel.as_ref()) {
            app.state::<AppState>().otel.lock().unwrap().tick(o);
        }
        if let Some(s) = &summary {
            let state = app.state::<AppState>();
            let first = !state.status_seen.swap(true, Ordering::AcqRel);
            if let Some(req) = s.app_update.clone() {
                let mut handled = state.app_update_handled.lock().unwrap();
                match claim_app_update(&mut handled, first, &req) {
                    Claim::Run => handle_app_update_request(&app, req),
                    Claim::Stale => log!("忽略启动前留下的重启请求（{}）：那是给上一个进程的", req.id),
                    Claim::Done => {}
                }
            }
        }
        if let Some(s) = &summary {
            show_notices(&app, s);
            restart_outdated(&app, s);
        }
        apply_status(&app, summary);
        std::thread::sleep(POLL);
    });
}

/// 后台进程比应用旧（OTA 更新后上一个外壳的进程还在应答）：重启它，换成随新应用打包的版本。
/// 每个版本只重启一次，重启后还是旧的（打包出错）就不再反复重启，写日志。
fn restart_outdated(app: &AppHandle, s: &Summary) {
    let Some(old) = outdated(s) else { return };
    let state = app.state::<AppState>();
    {
        let mut done = state.outdated_restarted.lock().unwrap();
        if done.as_deref() == Some(old.as_str()) {
            return;
        }
        *done = Some(old.clone());
    }
    log!("后台进程（{old}）比应用（{APP_VERSION}）旧，重启后台进程");
    let h = app.clone();
    std::thread::spawn(move || {
        backend::restart();
        let handle = h.clone();
        let _ = h.run_on_main_thread(move || {
            if let Some(w) = main_window(&handle) {
                if w.url().map(|u| u.as_str().starts_with(SERVICE)).unwrap_or(false) {
                    // 回到启动页，等新进程就绪后再进入（新版界面）。
                    if let Some(page) = handle.state::<AppState>().start_page.lock().unwrap().clone() {
                        let _ = w.navigate(page);
                    }
                }
            }
        });
    });
}

#[derive(Debug, PartialEq)]
enum Claim {
    Run,   // 新请求，处理
    Stale, // 启动前就留在服务里的重启请求，不处理
    Done,  // 已处理过
}

/// 决定是否处理首页的更新请求。服务把请求一直留在内存里，外壳按 ID 去重，但去重记录随进程重启丢失：
/// 重启后又看到同一个 restart 请求就会再重启。后台服务没更新时（OTA 只换了应用），服务不重启、
/// 请求一直在，于是无限重启（BXC-D0-003）。
/// 所以本进程第一次读到状态时就已存在的 restart 请求一律视为过期：本进程还没装过任何更新，
/// 不可能是给它的。install 请求照常处理（外壳不在时用户点了更新，启动后接着做）。
fn claim_app_update(handled: &mut Option<String>, first_status: bool, req: &AppUpdateRequest) -> Claim {
    if handled.as_deref() == Some(req.id.as_str()) {
        return Claim::Done;
    }
    *handled = Some(req.id.clone());
    // 外壳刚启动时看到的重启请求是给上一个进程的（它已经重启过了）。
    if first_status && (req.action == "restart" || req.action == "restart_when_idle") {
        Claim::Stale
    } else {
        Claim::Run
    }
}

/// 处理首页发来的请求：install 下载安装（进度回报给服务），restart 重启应用。
fn handle_app_update_request(app: &AppHandle, req: AppUpdateRequest) {
    let app = app.clone();
    tauri::async_runtime::spawn(async move {
        match req.action.as_str() {
            "restart" => {
                log!("重启应用以完成更新（首页请求或强制更新到时）");
                app.restart();
            }
            // 静默更新已装好：等用户空闲 10 分钟再重启应用，读不到空闲时间就等下次退出或重启（S1.10，ADR-057）。
            "restart_when_idle" => {
                log!("静默更新已安装，等用户空闲后重启应用");
                std::thread::spawn(move || loop {
                    match idle::idle() {
                        Some(d) if d >= IDLE_RESTART => {
                            log!("用户已空闲 {} 秒，重启应用以完成静默更新", d.as_secs());
                            app.restart();
                        }
                        Some(_) => {}
                        None => {
                            log!("读不到空闲时间，静默更新在下次退出或重启后生效");
                            return;
                        }
                    }
                    std::thread::sleep(Duration::from_secs(30));
                });
            }
            "install" => {
                report_progress(&req.id, "checking", "", 0, 0, "");
                let updater = match build_updater(&app) {
                    Ok(u) => u,
                    Err(e) => return report_progress(&req.id, "failed", "", 0, 0, &e),
                };
                match updater.check().await {
                    Ok(Some(update)) => install_update(&app, update, Some(req.id), false).await,
                    Ok(None) => report_progress(&req.id, "none", "", 0, 0, ""),
                    Err(e) => {
                        update_failed(&app, "check", "", &e.to_string(), false);
                        report_progress(&req.id, "failed", "", 0, 0, &update_reason(&e.to_string()));
                    }
                }
            }
            other => log!("未知的更新请求：{other}"),
        }
    });
}

/// 把更新进度回报给服务（首页显示进度条）。失败只记日志。
fn report_progress(id: &str, phase: &str, version: &str, downloaded: u64, total: u64, error: &str) {
    let body = serde_json::json!({
        "id": id, "phase": phase, "version": version, "downloaded": downloaded, "total": total, "error": error,
    });
    if let Err(e) = call("POST", "/desktop/app-update/progress", body) {
        log!("回报更新进度失败：{e}");
    }
}

fn build_updater(app: &AppHandle) -> Result<tauri_plugin_updater::Updater, String> {
    let token = read_token().ok_or("读不到访问令牌")?;
    app.updater_builder()
        .header("Authorization", format!("Bearer {token}"))
        .and_then(|b| b.build())
        .map_err(|e| format!("更新检查初始化失败：{e}"))
}

/// 弹出新排进来的系统通知。外壳刚启动（或第一次连上后台进程）时只记下位置，之前的不补发。
/// TODO(verify:V33) macOS ad-hoc 签名下的通知授权与显示；Windows Toast 的应用名。
fn show_notices(app: &AppHandle, s: &Summary) {
    let state = app.state::<AppState>();
    let mut last = state.last_notice.lock().unwrap();
    let newest = s.notices.iter().map(|n| n.id).max().unwrap_or(0);
    let Some(seen) = *last else {
        *last = Some(newest);
        return;
    };
    for n in s.notices.iter().filter(|n| n.id > seen) {
        if let Err(e) = app.notification().builder().title(&n.title).body(&n.body).show() {
            log!("系统通知失败：{e}");
        }
    }
    *last = Some(newest.max(seen));
}

fn on_menu(app: &AppHandle, id: &str) {
    match id {
        "open" => show_window(app),
        "log" => {
            if let Err(e) = shell_log::open() {
                log!("{e}");
            }
        }
        "update" => {
            let app = app.clone();
            tauri::async_runtime::spawn(async move {
                if *app.state::<AppState>().restart_ready.lock().unwrap() {
                    log!("托盘菜单重启应用以完成更新");
                    app.restart();
                }
                let pending = app.state::<AppState>().pending.lock().unwrap().take();
                match pending {
                    Some(update) => install_update(&app, update, None, true).await,
                    None => check_update(&app, true).await,
                }
            });
        }
        "quit" => quit_app(app),
        _ => {}
    }
}

// ---- 更新（OTA） ----
//
// 更新包由管理服务托管，经本机后台服务转发（tauri.conf.json 的 updater.endpoints）。
// 外壳只带本机令牌，服务端地址、令牌和证书都在后台服务里。没有绑定管理服务时没有更新。
// 安装包有签名，公钥在 tauri.conf.json，私钥只在构建机上（~/.tauri/banbu-connect.key）。

fn set_update_text(app: &AppHandle, text: &str) {
    let state = app.state::<AppState>();
    *state.update_text.lock().unwrap() = Some(text.to_string());
    let items = state.items.lock().unwrap();
    if let Some(items) = items.as_ref() {
        let _ = items.update.set_text(text);
    }
}

/// 把更新错误翻成能看懂的原因；原文写进日志。
fn update_reason(err: &str) -> String {
    let e = err.to_lowercase();
    let reason = if e.contains("signature") {
        "签名校验失败：上传更新包时填的签名和安装包不是同一份（要用同一个 .app.tar.gz / 安装包对应的 .sig）"
    } else if e.contains("status: 404") || e.contains("not found") {
        "管理服务上找不到更新包（可能已删除或取消发布）"
    } else if e.contains("status: 401") || e.contains("status: 403") {
        "后台进程拒绝访问：令牌失效，请重启应用"
    } else if e.contains("status: 5") || e.contains("bad gateway") {
        "后台进程连不上管理服务，或转发更新包失败"
    } else if e.contains("permission denied") || e.contains("operation not permitted") {
        "没有权限替换应用：请把「Banbu Connect Lite」移到「应用程序」并确认当前用户可写"
    } else if e.contains("connection refused") || e.contains("error sending request") || e.contains("timed out") {
        "连不上本机后台进程"
    } else if e.contains("archive") || e.contains("tar") || e.contains("gzip") || e.contains("zip") || e.contains("invalid updater binary") {
        "更新包格式不对：macOS 要上传 .app.tar.gz，Windows 要上传 安装包（-setup.exe 或 .msi）"
    } else {
        ""
    };
    if reason.is_empty() {
        err.chars().take(120).collect()
    } else {
        reason.to_string()
    }
}

/// 更新失败：写日志、菜单显示原因、弹通知、回报给后台服务（进事件和诊断数据，管理员也能看到）。
fn update_failed(app: &AppHandle, stage: &str, version: &str, err: &str, notify: bool) {
    let what = match stage {
        "check" => "检查更新失败",
        _ => "更新失败",
    };
    log!("{what}（{stage} {version}）：{err}");
    let reason = update_reason(err);
    let short: String = reason.chars().take(40).collect();
    let more = if reason.chars().count() > 40 { "…" } else { "" };
    set_update_text(app, &format!("{what}：{short}{more}（点击重试）"));
    if notify {
        let log_hint = shell_log::path().map(|p| format!("\n详情见日志：{}", p.display())).unwrap_or_default();
        let _ = app.notification().builder().title(what).body(format!("{reason}{log_hint}")).show();
    }
    let body = serde_json::json!({ "stage": stage, "version": version, "error": err });
    std::thread::spawn(move || {
        if let Err(e) = call("POST", "/desktop/update/report", body) {
            log!("回报更新失败给后台服务失败：{e}");
        }
    });
}

async fn check_update(app: &AppHandle, manual: bool) {
    let updater = match build_updater(app) {
        Ok(u) => u,
        Err(e) => {
            log!("{e}");
            return;
        }
    };
    match updater.check().await {
        Ok(Some(update)) => {
            set_update_text(app, &format!("安装更新 {}（当前 {APP_VERSION}）", update.version));
            *app.state::<AppState>().pending.lock().unwrap() = Some(update);
        }
        Ok(None) => {
            if manual {
                set_update_text(app, &format!("已是最新版本 {APP_VERSION}"));
            }
        }
        Err(e) => {
            if manual {
                update_failed(app, "check", "", &e.to_string(), true);
            } else {
                log!("自动检查更新失败：{e}");
            }
        }
    }
}

/// 下载并安装更新，进度回报给服务（首页进度条）和托盘菜单。
/// request 是首页请求的 ID（托盘发起的为 None）；restart_after 为真时装完立即重启（托盘发起），
/// 否则等首页点「立即重启」。
async fn install_update(app: &AppHandle, update: Update, request: Option<String>, restart_after: bool) {
    let id = request.unwrap_or_default();
    let version = update.version.clone();
    set_update_text(app, &format!("正在下载 {version}…"));
    log!("开始下载更新 {version}（当前 {APP_VERSION}）");
    report_progress(&id, "downloading", &version, 0, 0, "");
    // 分开下载和安装，失败时能说清是哪一步。进度最多每 300 毫秒回报一次。
    let mut downloaded: u64 = 0;
    let mut last = Instant::now();
    let menu = app.clone();
    let progress_id = id.clone();
    let progress_version = version.clone();
    let result = update
        .download(
            move |chunk, total| {
                downloaded += chunk as u64;
                if last.elapsed() >= Duration::from_millis(300) {
                    last = Instant::now();
                    let total = total.unwrap_or(0);
                    if let Some(pct) = (downloaded * 100).checked_div(total) {
                        set_update_text(&menu, &format!("正在下载 {progress_version}… {pct}%"));
                    }
                    report_progress(&progress_id, "downloading", &progress_version, downloaded, total, "");
                }
            },
            || {},
        )
        .await;
    let bytes = match result {
        Ok(b) => b,
        Err(e) => {
            update_failed(app, "download", &version, &e.to_string(), id.is_empty());
            report_progress(&id, "failed", &version, 0, 0, &update_reason(&e.to_string()));
            *app.state::<AppState>().pending.lock().unwrap() = Some(update);
            return;
        }
    };
    log!("更新包已下载并通过签名校验（{} 字节），开始安装", bytes.len());
    let size = bytes.len() as u64;
    report_progress(&id, "installing", &version, size, size, "");
    set_update_text(app, &format!("正在安装 {version}…"));
    // Windows 的安装程序要替换 resources\boxctl.exe，正在运行的文件替换不了：先停掉后台进程。
    // 安装程序随后会结束并重新打开应用，新外壳再拉起新版后台进程。
    #[cfg(windows)]
    tauri::async_runtime::spawn_blocking(backend::stop).await.ok();
    match update.install(bytes) {
        Ok(()) => {
            log!("更新 {version} 安装完成");
            report_progress(&id, "installed", &version, size, size, "");
            if restart_after {
                app.restart();
            }
            // 首页发起的：等用户点「立即重启」。托盘菜单也可以重启。
            set_update_text(app, &format!("{version} 已安装，点击重启应用"));
            *app.state::<AppState>().restart_ready.lock().unwrap() = true;
        }
        Err(e) => {
            update_failed(app, "install", &version, &e.to_string(), id.is_empty());
            report_progress(&id, "failed", &version, size, size, &update_reason(&e.to_string()));
            *app.state::<AppState>().pending.lock().unwrap() = Some(update);
        }
    }
}

/// 静默检查一次：已有待安装的更新、正在检查或一分钟内刚检查过时跳过。
fn auto_check_update(app: &AppHandle) {
    let app = app.clone();
    tauri::async_runtime::spawn(async move {
        let state = app.state::<AppState>();
        let recent = state.last_check.lock().unwrap().is_some_and(|t| t.elapsed() < Duration::from_secs(60));
        if recent || state.pending.lock().unwrap().is_some() || state.checking.swap(true, Ordering::AcqRel) {
            return;
        }
        check_update(&app, false).await;
        *state.last_check.lock().unwrap() = Some(Instant::now());
        state.checking.store(false, Ordering::Release);
    });
}

/// 启动时检查一次（稍等几秒，开机自启时后台服务可能还没起来），之后每小时一次。
fn schedule_updates(app: AppHandle) {
    tauri::async_runtime::spawn(async move {
        tokio_sleep(Duration::from_secs(5)).await;
        loop {
            auto_check_update(&app);
            tokio_sleep(UPDATE_EVERY).await;
        }
    });
}

async fn tokio_sleep(d: Duration) {
    let _ = tauri::async_runtime::spawn_blocking(move || std::thread::sleep(d)).await;
}

// 左键（macOS 菜单栏单击、Windows 任务栏单击）弹出托盘面板；右键是精简的系统菜单。
fn build_tray(app: &AppHandle) -> tauri::Result<()> {
    let status = MenuItem::with_id(app, "status", "后台进程未连接", false, None::<&str>)?;
    let open = MenuItem::with_id(app, "open", "打开主界面", true, None::<&str>)?;
    let update = MenuItem::with_id(app, "update", "检查更新", true, None::<&str>)?;
    let log_item = MenuItem::with_id(app, "log", "查看日志", true, None::<&str>)?;
    let quit = MenuItem::with_id(app, "quit", "退出", true, None::<&str>)?;
    let sep1 = PredefinedMenuItem::separator(app)?;
    let sep2 = PredefinedMenuItem::separator(app)?;
    let sep3 = PredefinedMenuItem::separator(app)?;
    let menu = Menu::with_items(app, &[&status, &sep1, &open, &update, &sep2, &log_item, &sep3, &quit])?;
    TrayIconBuilder::with_id("main")
        .icon(tray_image(tray_status(None)))
        .icon_as_template(TEMPLATE_ICON)
        .tooltip(PRODUCT)
        .menu(&menu)
        .show_menu_on_left_click(false)
        .on_menu_event(|app, event| on_menu(app, event.id().as_ref()))
        .on_tray_icon_event(|tray, event| match event {
            TrayIconEvent::Click { button: MouseButton::Left, button_state: MouseButtonState::Up, rect, .. } => {
                tray_panel::toggle(tray.app_handle(), rect);
            }
            TrayIconEvent::DoubleClick { button: MouseButton::Left, .. } => {
                tray_panel::hide(tray.app_handle());
                show_window(tray.app_handle());
            }
            _ => {}
        })
        .build(app)?;
    tray_panel::create(app)?;
    let state = app.state::<AppState>();
    *state.items.lock().unwrap() = Some(TrayItems { status, update });
    Ok(())
}

// ---- 托盘面板调用的命令（ui/tray.html） ----

fn get_json(token: &str, path: &str) -> Option<serde_json::Value> {
    agent()
        .get(&format!("{SERVICE}/api/v1{path}"))
        .set("Authorization", &format!("Bearer {token}"))
        .call()
        .ok()?
        .into_json()
        .ok()
}

/// 面板要显示的数据：后台进程的状态原样转给页面，再加上外壳自己的版本和更新状态。
/// 后台进程没连上时 status 为 null，backend_error 是起不来的原因。
#[tauri::command]
async fn tray_data(app: AppHandle) -> serde_json::Value {
    let status = tauri::async_runtime::spawn_blocking(|| read_token().and_then(|t| get_json(&t, "/desktop/status")))
        .await
        .unwrap_or(None);
    let state = app.state::<AppState>();
    let update_text = state.update_text.lock().unwrap().clone();
    let restart_ready = *state.restart_ready.lock().unwrap();
    serde_json::json!({
        "app_version": APP_VERSION,
        "status": status,
        "backend_error": backend::error(),
        "update_text": update_text,
        "restart_ready": restart_ready,
        "platform": SHELL,
    })
}

/// 面板上的操作，和托盘菜单同一套（on_menu）。会离开面板的操作先把面板收起来。
#[tauri::command]
fn tray_action(app: AppHandle, id: String) {
    if matches!(id.as_str(), "open" | "log" | "quit" | "update") {
        tray_panel::hide(&app);
    }
    on_menu(&app, &id);
}

fn main() {
    tauri::Builder::default()
        .plugin(tauri_plugin_single_instance::init(|app, _, _| show_window(app)))
        .plugin(tauri_plugin_updater::Builder::new().build())
        .plugin(tauri_plugin_notification::init())
        // 每次页面加载完成后告诉页面：系统标题栏已去掉，要给 macOS 红绿灯让位或自绘 Windows 按钮。
        .on_page_load(|webview, payload| {
            if payload.event() == tauri::webview::PageLoadEvent::Finished
                && payload.url().as_str().starts_with(SERVICE)
            {
                let _ = webview.eval(SHELL_SCRIPT);
            }
        })
        .manage(AppState::default())
        .invoke_handler(tauri::generate_handler![
            service_url,
            backend_error,
            restart_backend,
            tray_data,
            tray_action,
            shell_open_full,
            shell_open_compact,
            shell_running_apps,
            shell_quit
        ])
        .setup(|app| {
            let handle = app.handle().clone();
            // 记下本地启动页的地址：后台进程不可用时从管理界面切回来。
            if let Some(url) = main_window(&handle).and_then(|w| w.url().ok()) {
                *handle.state::<AppState>().start_page.lock().unwrap() = Some(url);
            }
            backend::supervise(&handle);
            build_tray(&handle)?;
            poll(handle.clone());
            schedule_updates(handle.clone());
            // 还没绑定管理服务时打开窗口，引导绑定；已绑定时安静地待在托盘。
            std::thread::spawn(move || {
                for _ in 0..20 {
                    if let Some(s) = read_token().and_then(|t| fetch_status(&t)) {
                        if !s.server.bound {
                            let h = handle.clone();
                            let _ = handle.run_on_main_thread(move || show_window(&h));
                        }
                        return;
                    }
                    std::thread::sleep(Duration::from_millis(500));
                }
                // 10 秒还没起来：打开启动页显示原因。
                let h = handle.clone();
                let _ = handle.run_on_main_thread(move || show_window(&h));
            });
            Ok(())
        })
        .on_window_event(|window, event| {
            // 主窗口关窗只是隐藏，托盘继续运行；完整界面窗口直接关闭，下次从精简页重新打开。
            if let tauri::WindowEvent::CloseRequested { api, .. } = event {
                if window.label() != FULL_LABEL {
                    api.prevent_close();
                    let _ = window.hide();
                }
            }
        })
        .build(tauri::generate_context!())
        .expect("启动失败")
        .run(|app, event| match event {
            // macOS：点 Dock 图标、在「应用程序」或启动台里再次打开应用。
            // Windows 再次打开应用由 single-instance 插件处理。
            #[cfg(target_os = "macos")]
            tauri::RunEvent::Reopen { .. } => show_window(app),
            // 外壳退出时停掉后台进程（标准输入关闭它也会退出，这里让它退得干净）。
            tauri::RunEvent::Exit => backend::stop(),
            _ => {
                let _ = app;
            }
        });
}

#[cfg(test)]
mod version_tests {
    use super::{older, APP_VERSION};

    #[test]
    fn compares_versions() {
        assert!(older("0.1.0", "0.2.0"));
        assert!(older("0.9.0", "0.10.0"));
        assert!(older("0.2.0-beta", "0.2.0"));
        assert!(!older("0.2.0", "0.2.0"));
        assert!(!older("0.3.0", "0.2.0"));
        assert!(!older("v0.2.0", "0.2.0"));
    }

    #[test]
    fn app_version_comes_from_tauri_conf() {
        let conf: serde_json::Value = serde_json::from_str(include_str!("../tauri.conf.json")).unwrap();
        assert_eq!(conf["version"].as_str(), Some(APP_VERSION));
    }
}

#[cfg(test)]
mod status_tests {
    use super::parse_status;

    #[test]
    fn tolerates_null_lists() {
        let body = r#"{"version":"0.3.0","server":{"bound":true,"url":"https://x.example","last_error":""},
            "notices":null,
            "otel":{"enabled":true,"env":null,"force":false,"sync_request":0},"extra":{"anything":[1,2]}}"#;
        let s = parse_status(body).expect("should parse");
        assert!(s.notices.is_empty());
        assert_eq!(s.version, "0.3.0");
        assert!(s.server.bound);
        assert!(s.otel.unwrap().env.is_empty());
    }

    #[test]
    fn parse_error_is_reported() {
        let err = parse_status(r#"{"version":1}"#).err().expect("should fail");
        assert!(err.contains("解析失败"), "{err}");
    }

    #[test]
    fn host_of_url() {
        assert_eq!(super::host_of("https://connect.example.cn/x"), "connect.example.cn");
        assert_eq!(super::host_of("connect.example.cn"), "connect.example.cn");
    }
}

#[cfg(test)]
mod app_update_tests {
    use super::{claim_app_update, AppUpdateRequest, Claim};

    fn req(id: &str, action: &str) -> AppUpdateRequest {
        AppUpdateRequest { id: id.into(), action: action.into() }
    }

    #[test]
    fn stale_restart_after_relaunch_is_ignored() {
        // 重启后的新进程：去重记录是空的，服务里还留着上一个进程处理过的 restart。
        let mut handled = None;
        assert_eq!(claim_app_update(&mut handled, true, &req("r1", "restart")), Claim::Stale);
        // 之后的轮询也不再处理它：不会重启。
        assert_eq!(claim_app_update(&mut handled, false, &req("r1", "restart")), Claim::Done);
    }

    #[test]
    fn new_requests_run_once() {
        let mut handled = None;
        // 启动时就在的 install 照常做（外壳不在时用户点了更新）。
        assert_eq!(claim_app_update(&mut handled, true, &req("i1", "install")), Claim::Run);
        assert_eq!(claim_app_update(&mut handled, false, &req("i1", "install")), Claim::Done);
        // 本进程装完后用户点「立即重启」：新 ID，处理。
        assert_eq!(claim_app_update(&mut handled, false, &req("r2", "restart")), Claim::Run);
    }
}
