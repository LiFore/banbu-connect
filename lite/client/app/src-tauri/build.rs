// 应用版本只以 tauri.conf.json 为准：Tauri updater 上报的版本、安装包和后台服务（scripts/build-app.sh）
// 都用它。外壳判断后台服务是否过期也必须用同一个值，不能用 Cargo.toml 的版本——两者不一致时，
// 外壳会一直认为后台进程是旧版，反复重启它。
fn main() {
    println!("cargo:rerun-if-changed=tauri.conf.json");
    let conf = std::fs::read_to_string("tauri.conf.json").expect("读不到 tauri.conf.json");
    let version = conf
        .lines()
        .find_map(|l| {
            let l = l.trim();
            let rest = l.strip_prefix("\"version\"")?.trim_start().strip_prefix(':')?.trim();
            Some(rest.trim_end_matches(',').trim_matches('"').to_string())
        })
        .expect("tauri.conf.json 里没有 version");
    println!("cargo:rustc-env=BANBU_APP_VERSION={version}");
    // 自定义命令要在能力里逐个授权（capabilities/*.json 的 allow-<命令>）：本地页面（启动页、托盘面板）用前五个；
    // 后台进程的网页只能用 shell_open_full、shell_open_compact、shell_running_apps、shell_quit（titlebar.json）。
    tauri_build::try_build(tauri_build::Attributes::new().app_manifest(tauri_build::AppManifest::new().commands(&[
        "service_url",
        "backend_error",
        "restart_backend",
        "tray_data",
        "tray_action",
        "shell_open_full",
        "shell_open_compact",
        "shell_running_apps",
        "shell_quit",
    ])))
    .expect("tauri build 失败")
}
