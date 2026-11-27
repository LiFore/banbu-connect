// 自动配置 Claude Code 遥测（OpenTelemetry）：把后台服务给出的 env 写进用户的 Claude Code settings.json，
// 让 Claude Code 把指标、事件、trace 发到本机接收器（boxctl desktop 的 otel 服务），再由服务上报到管理服务。
// 外壳运行在用户会话里，才能写当前用户的 ~/.claude；后台服务以 SYSTEM/root 运行，不能代劳。
//
// 规则：
// - 先检查这台电脑上是否已经配置过 OTel（不是本客户端写的）：用户的 settings.json、企业托管配置、环境变量。
//   有的话默认不写，回报给服务；服务据此不自动开启，在首页提示（用户可以选择仍然开启）。
// - 用户明确开启（force）时覆盖 settings.json 里的同名键，原值记下来，关闭时恢复；托管配置优先级最高，覆盖不了。
// - 只增删自己写入的键（按记下的值识别），写前备份，解析失败不写。
// settings.json 的修改只对之后新开的 Claude Code 会话生效。

use std::collections::BTreeMap;
use std::path::{Path, PathBuf};
use std::time::{Duration, Instant};

use serde::{Deserialize, Serialize};
use serde_json::{Map, Value};

use crate::{read_token, token_path, SERVICE};

/// 分信号端点优先级高于 OTEL_EXPORTER_OTLP_ENDPOINT，设了就会把该信号发到别处。
const SIGNAL_ENDPOINT_KEYS: [&str; 3] = [
    "OTEL_EXPORTER_OTLP_LOGS_ENDPOINT",
    "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT",
    "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
];
/// 期望值没变时也定期核对一次：用户可能改了 settings.json。
const RECHECK: Duration = Duration::from_secs(60);

/// 与遥测有关的配置项。
fn is_telemetry_key(k: &str) -> bool {
    k == "CLAUDE_CODE_ENABLE_TELEMETRY" || k.starts_with("OTEL_")
}

#[derive(Deserialize, Clone, PartialEq, Default, Debug)]
pub struct OTelBrief {
    #[serde(default)]
    pub enabled: bool,
    #[serde(default, deserialize_with = "crate::null_default")]
    pub env: BTreeMap<String, String>,
    #[serde(default)]
    pub force: bool,
    /// 页面上点「立即写入配置」时变化；和其他字段一样，变了就立即核对（见 Syncer::tick）。
    #[serde(default)]
    pub sync_request: i64,
}

#[derive(Serialize, Clone, PartialEq, Debug, Default)]
pub struct Existing {
    key: String,
    value: String,
    /// settings（用户的 settings.json）、managed（企业托管配置）、env（环境变量）
    source: String,
}

#[derive(Serialize, Clone, PartialEq, Debug, Default)]
pub struct Report {
    configured: bool,
    settings_path: String,
    existing: Vec<Existing>,
    #[serde(skip_serializing_if = "String::is_empty")]
    error: String,
}

/// 记下自己写入的键和值，以及覆盖掉的原值，下次据此识别、移除和恢复。
#[derive(Serialize, Deserialize, Default, Clone)]
struct Record {
    #[serde(default)]
    env: BTreeMap<String, String>,
    #[serde(default)]
    replaced: BTreeMap<String, String>,
}

/// 一次核对需要的输入；拆出来便于测试。
pub struct Inputs<'a> {
    pub settings: &'a Path,
    pub record: &'a Path,
    pub managed: &'a [PathBuf],
    pub process_env: Vec<(String, String)>,
}

#[derive(Default)]
pub struct Syncer {
    last: Option<(OTelBrief, Instant)>,
}

impl Syncer {
    /// 每次轮询调用：期望值变了或过了 RECHECK 才真正核对、写入并回报。
    pub fn tick(&mut self, brief: &OTelBrief) {
        if let Some((prev, at)) = &self.last {
            if prev == brief && at.elapsed() < RECHECK {
                return;
            }
        }
        self.last = Some((brief.clone(), Instant::now()));
        let (Some(settings), Some(record)) = (settings_path(), record_path()) else {
            return;
        };
        let inputs = Inputs {
            settings: &settings,
            record: &record,
            managed: &managed_paths(),
            process_env: std::env::vars().filter(|(k, _)| is_telemetry_key(k)).collect(),
        };
        let report = sync(&inputs, &brief.env, brief.force);
        if !report.error.is_empty() {
            log!("配置 Claude Code 遥测失败：{}", report.error);
        }
        send(&report);
        // 多开的每个工作区也写一份（BXC-F0-035），用量按账号区分；工作区是本客户端建的，直接覆盖。
        // TODO(verify:V-multi-claude-otel) <CLAUDE_CONFIG_DIR>/settings.json 的 env 生效。
        for dir in crate::workspaces::claude_dirs() {
            let (settings, record) = (dir.join("settings.json"), dir.join(".banbu-otel.json"));
            let ws = Inputs { settings: &settings, record: &record, managed: inputs.managed, process_env: Vec::new() };
            let r = sync(&ws, &brief.env, true);
            if !r.error.is_empty() {
                log!("配置工作区 {} 的 Claude Code 遥测失败：{}", dir.display(), r.error);
            }
        }
    }
}

/// Claude Code 的用户配置：CLAUDE_CONFIG_DIR 优先，否则 ~/.claude。
fn settings_path() -> Option<PathBuf> {
    if let Some(dir) = std::env::var_os("CLAUDE_CONFIG_DIR").filter(|d| !d.is_empty()) {
        return Some(PathBuf::from(dir).join("settings.json"));
    }
    #[cfg(windows)]
    let home = std::env::var_os("USERPROFILE")?;
    #[cfg(not(windows))]
    let home = std::env::var_os("HOME")?;
    Some(PathBuf::from(home).join(".claude").join("settings.json"))
}

/// 企业托管配置（优先级最高）。
fn managed_paths() -> Vec<PathBuf> {
    if cfg!(target_os = "macos") {
        vec![PathBuf::from("/Library/Application Support/ClaudeCode/managed-settings.json")]
    } else {
        vec![
            PathBuf::from(r"C:\Program Files\ClaudeCode\managed-settings.json"),
            PathBuf::from(r"C:\ProgramData\ClaudeCode\managed-settings.json"),
        ]
    }
}

fn record_path() -> Option<PathBuf> {
    Some(token_path()?.parent()?.join("claude-otel.json"))
}

fn send(report: &Report) {
    let Some(token) = read_token() else { return };
    let body = serde_json::to_string(report).unwrap_or_default();
    let result = ureq::AgentBuilder::new()
        .timeout(Duration::from_secs(5))
        .build()
        .post(&format!("{SERVICE}/api/v1/desktop/otel/claude"))
        .set("Authorization", &format!("Bearer {token}"))
        .set("Content-Type", "application/json")
        .send_string(&body);
    if let Err(e) = result {
        log!("回报 Claude Code 遥测配置失败：{e}");
    }
}

/// 核对并写入。desired 为空表示关闭：只移除自己写入、且没被改过的键，并恢复覆盖掉的原值。
pub fn sync(inputs: &Inputs, desired: &BTreeMap<String, String>, force: bool) -> Report {
    let mut report = Report { settings_path: inputs.settings.display().to_string(), ..Default::default() };
    if let Err(e) = apply(inputs, desired, force, &mut report) {
        report.error = e;
        report.configured = false;
    }
    report
}

fn value_text(v: &Value) -> String {
    v.as_str().map(str::to_string).unwrap_or_else(|| v.to_string())
}

fn env_of(settings: &Map<String, Value>) -> Result<Map<String, Value>, String> {
    match settings.get("env") {
        None => Ok(Map::new()),
        Some(Value::Object(m)) => Ok(m.clone()),
        Some(_) => Err("settings.json 里的 env 不是对象，未做修改".into()),
    }
}

fn read_settings(file: &Path) -> Result<Map<String, Value>, String> {
    match std::fs::read_to_string(file) {
        Ok(text) if text.trim().is_empty() => Ok(Map::new()),
        Ok(text) => match serde_json::from_str::<Value>(&text) {
            Ok(Value::Object(m)) => Ok(m),
            _ => Err(format!("{} 不是合法的 JSON 对象，为避免破坏配置，未做修改", file.display())),
        },
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(Map::new()),
        Err(e) => Err(format!("读取 {} 失败：{e}", file.display())),
    }
}

fn apply(inputs: &Inputs, desired: &BTreeMap<String, String>, force: bool, report: &mut Report) -> Result<(), String> {
    let mut record: Record = std::fs::read_to_string(inputs.record)
        .ok()
        .and_then(|s| serde_json::from_str(&s).ok())
        .unwrap_or_default();
    let mut settings = read_settings(inputs.settings)?;
    let original = settings.clone();
    let mut env = env_of(&settings)?;
    let current = |env: &Map<String, Value>, k: &str| env.get(k).map(value_text);
    let ours = |env: &Map<String, Value>, k: &str| current(env, k).is_some_and(|v| record.env.get(k) == Some(&v));

    // 已有的 OTel 配置（不是自己写的）。
    for (k, v) in &env {
        if is_telemetry_key(k) && !ours(&env, k) {
            report.existing.push(Existing { key: k.clone(), value: value_text(v), source: "settings".into() });
        }
    }
    let mut managed_keys = BTreeMap::new();
    for path in inputs.managed {
        // 托管配置读不到或格式不对时忽略：它不归我们管。
        let Ok(m) = read_settings(path) else { continue };
        for (k, v) in env_of(&m).unwrap_or_default() {
            if is_telemetry_key(&k) {
                managed_keys.insert(k.clone(), value_text(&v));
                report.existing.push(Existing { key: k, value: value_text(&v), source: "managed".into() });
            }
        }
    }
    for (k, v) in &inputs.process_env {
        report.existing.push(Existing { key: k.clone(), value: v.clone(), source: "env".into() });
    }

    // 已有配置且用户没有明确开启：什么都不写，交给服务决定（首页提示）。
    if !desired.is_empty() && !report.existing.is_empty() && !force {
        return Ok(());
    }

    // 移除自己以前写入、现在不再需要（或值已变）的键；覆盖过的恢复原值。
    let stale: Vec<String> = record.env.iter().filter(|(k, v)| desired.get(*k) != Some(v)).map(|(k, _)| k.clone()).collect();
    for k in stale {
        if ours(&env, &k) {
            match record.replaced.get(&k) {
                Some(orig) if !desired.contains_key(&k) => env.insert(k.clone(), Value::String(orig.clone())),
                _ => env.remove(&k),
            };
        }
    }
    if desired.is_empty() {
        // 关闭：被删掉的分信号端点也恢复。
        for (k, orig) in &record.replaced {
            if !env.contains_key(k) && !record.env.contains_key(k) {
                env.insert(k.clone(), Value::String(orig.clone()));
            }
        }
        record = Record::default();
    } else {
        let endpoint = desired.get("OTEL_EXPORTER_OTLP_ENDPOINT").cloned().unwrap_or_default();
        for (k, want) in desired {
            if let Some(cur) = current(&env, k) {
                if cur != *want && !ours(&env, k) {
                    record.replaced.entry(k.clone()).or_insert(cur);
                }
            }
            env.insert(k.clone(), Value::String(want.clone()));
        }
        for k in SIGNAL_ENDPOINT_KEYS {
            if let Some(cur) = current(&env, k) {
                if !cur.starts_with(&endpoint) {
                    record.replaced.entry(k.into()).or_insert(cur);
                    env.remove(k);
                }
            }
        }
        record.env = desired.clone();
    }

    if env.is_empty() {
        settings.remove("env");
    } else {
        settings.insert("env".into(), Value::Object(env.clone()));
    }
    if settings != original {
        write_settings(inputs.settings, &settings)?;
    }
    write_record(inputs.record, &record)?;
    // 托管配置把同名键设成别的值时，写了也不生效。
    report.configured = !desired.is_empty()
        && desired.iter().all(|(k, v)| current(&env, k).as_deref() == Some(v.as_str()) && managed_keys.get(k).is_none_or(|m| m == v));
    // 覆盖之后，已经是自己的值的键不再算「已有配置」。
    report.existing.retain(|e| e.source != "settings" || !(record.env.get(&e.key) == current(&env, &e.key).as_ref()));
    Ok(())
}

fn write_settings(file: &Path, settings: &Map<String, Value>) -> Result<(), String> {
    if let Some(dir) = file.parent() {
        std::fs::create_dir_all(dir).map_err(|e| format!("创建 {} 失败：{e}", dir.display()))?;
    }
    if file.exists() {
        // 只留最近一份备份，避免每次核对都多一个文件。
        let backup = file.with_extension("json.banbu-bak");
        std::fs::copy(file, &backup).map_err(|e| format!("备份 settings.json 失败：{e}"))?;
    }
    let text = serde_json::to_string_pretty(&Value::Object(settings.clone())).map_err(|e| e.to_string())? + "\n";
    atomic_write(file, &text)
}

fn write_record(file: &Path, record: &Record) -> Result<(), String> {
    if record.env.is_empty() && record.replaced.is_empty() {
        return match std::fs::remove_file(file) {
            Err(e) if e.kind() != std::io::ErrorKind::NotFound => Err(e.to_string()),
            _ => Ok(()),
        };
    }
    let text = serde_json::to_string_pretty(record).map_err(|e| e.to_string())?;
    atomic_write(file, &text)
}

fn atomic_write(file: &Path, text: &str) -> Result<(), String> {
    let tmp = file.with_extension("tmp-banbu");
    std::fs::write(&tmp, text).map_err(|e| format!("写入 {} 失败：{e}", tmp.display()))?;
    std::fs::rename(&tmp, file).map_err(|e| format!("写入 {} 失败：{e}", file.display()))
}

#[cfg(test)]
mod tests {
    use super::*;

    fn desired() -> BTreeMap<String, String> {
        [
            ("CLAUDE_CODE_ENABLE_TELEMETRY", "1"),
            ("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:19182"),
            ("OTEL_LOGS_EXPORTER", "otlp"),
        ]
        .into_iter()
        .map(|(k, v)| (k.to_string(), v.to_string()))
        .collect()
    }

    struct Env {
        dir: PathBuf,
        settings: PathBuf,
        record: PathBuf,
        managed: Vec<PathBuf>,
    }

    impl Env {
        fn new(name: &str) -> Env {
            let dir = std::env::temp_dir().join(format!("banbu-otel-test-{name}-{}", std::process::id()));
            let _ = std::fs::remove_dir_all(&dir);
            std::fs::create_dir_all(&dir).unwrap();
            Env { settings: dir.join("settings.json"), record: dir.join("record.json"), managed: vec![dir.join("managed.json")], dir }
        }
        fn sync(&self, desired: &BTreeMap<String, String>, force: bool) -> Report {
            self.sync_env(desired, force, vec![])
        }
        fn sync_env(&self, desired: &BTreeMap<String, String>, force: bool, process_env: Vec<(String, String)>) -> Report {
            let inputs = Inputs { settings: &self.settings, record: &self.record, managed: &self.managed, process_env };
            sync(&inputs, desired, force)
        }
        fn read(&self) -> Value {
            serde_json::from_str(&std::fs::read_to_string(&self.settings).unwrap()).unwrap()
        }
    }

    #[test]
    fn writes_keeps_other_settings_and_removes_on_disable() {
        let e = Env::new("roundtrip");
        std::fs::write(&e.settings, r#"{"model":"opus","env":{"FOO":"bar"},"hooks":{}}"#).unwrap();

        let rep = e.sync(&desired(), false);
        assert!(rep.configured && rep.existing.is_empty(), "{rep:?}");
        let v = e.read();
        assert_eq!(v["model"], "opus");
        assert_eq!(v["env"]["FOO"], "bar");
        assert_eq!(v["env"]["OTEL_EXPORTER_OTLP_ENDPOINT"], "http://127.0.0.1:19182");
        assert!(e.dir.join("settings.json.banbu-bak").exists());

        // 自己写入的旧值（换端口、旧版本多写的键）直接替换或移除，不算已有配置。
        let mut old = desired();
        old.insert("OTEL_LOG_USER_PROMPTS".into(), "1".into());
        e.sync(&old, false);
        assert_eq!(e.read()["env"]["OTEL_LOG_USER_PROMPTS"], "1");
        let rep = e.sync(&desired(), false);
        assert!(rep.configured && rep.existing.is_empty(), "{rep:?}");
        assert!(e.read()["env"].get("OTEL_LOG_USER_PROMPTS").is_none());

        // 用户手动改过的键在关闭时保留。
        let mut v = e.read();
        v["env"]["OTEL_LOGS_EXPORTER"] = "console".into();
        std::fs::write(&e.settings, v.to_string()).unwrap();
        let rep = e.sync(&BTreeMap::new(), false);
        assert!(!rep.configured && rep.error.is_empty());
        let v = e.read();
        assert_eq!(v["env"]["FOO"], "bar");
        assert_eq!(v["env"]["OTEL_LOGS_EXPORTER"], "console");
        assert!(v["env"].get("CLAUDE_CODE_ENABLE_TELEMETRY").is_none());
        assert!(!e.record.exists());
    }

    #[test]
    fn existing_config_is_reported_and_left_alone() {
        let e = Env::new("existing");
        let before = r#"{"env":{"CLAUDE_CODE_ENABLE_TELEMETRY":"1","OTEL_EXPORTER_OTLP_ENDPOINT":"https://collector.corp:4318"}}"#;
        std::fs::write(&e.settings, before).unwrap();
        let rep = e.sync(&desired(), false);
        assert!(!rep.configured);
        // 值和期望相同的也算已有配置：是用户自己配的。
        assert_eq!(rep.existing.len(), 2, "{rep:?}");
        assert_eq!(std::fs::read_to_string(&e.settings).unwrap(), before);
    }

    #[test]
    fn managed_and_process_env_count_as_existing() {
        let e = Env::new("managed");
        std::fs::write(&e.managed[0], r#"{"env":{"OTEL_EXPORTER_OTLP_ENDPOINT":"https://corp"}}"#).unwrap();
        let rep = e.sync(&desired(), false);
        assert_eq!(rep.existing.iter().map(|x| x.source.as_str()).collect::<Vec<_>>(), ["managed"]);
        assert!(!e.settings.exists());

        let e = Env::new("procenv");
        let rep = e.sync_env(&desired(), false, vec![("OTEL_METRICS_EXPORTER".into(), "console".into())]);
        assert_eq!(rep.existing.len(), 1);
        assert!(!e.settings.exists());
    }

    #[test]
    fn force_overwrites_then_restores_on_disable() {
        let e = Env::new("force");
        let before = r#"{"env":{"OTEL_EXPORTER_OTLP_ENDPOINT":"https://corp","OTEL_EXPORTER_OTLP_TRACES_ENDPOINT":"https://x","OTEL_EXPORTER_OTLP_HEADERS":"a=b"}}"#;
        std::fs::write(&e.settings, before).unwrap();
        let rep = e.sync(&desired(), true);
        assert!(rep.configured, "{rep:?}");
        let v = e.read();
        assert_eq!(v["env"]["OTEL_EXPORTER_OTLP_ENDPOINT"], "http://127.0.0.1:19182");
        assert!(v["env"].get("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT").is_none());
        // 再核对一次，覆盖过的键不再算已有配置；用户其他的键还在。
        let rep = e.sync(&desired(), true);
        assert_eq!(rep.existing.iter().map(|x| x.key.as_str()).collect::<Vec<_>>(), ["OTEL_EXPORTER_OTLP_HEADERS"]);

        e.sync(&BTreeMap::new(), false);
        let v = e.read();
        assert_eq!(v["env"]["OTEL_EXPORTER_OTLP_ENDPOINT"], "https://corp");
        assert_eq!(v["env"]["OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"], "https://x");
        assert_eq!(v["env"]["OTEL_EXPORTER_OTLP_HEADERS"], "a=b");
        assert!(v["env"].get("CLAUDE_CODE_ENABLE_TELEMETRY").is_none());
        assert!(!e.record.exists());
    }

    #[test]
    fn invalid_json_is_not_touched_and_missing_file_is_created() {
        let e = Env::new("invalid");
        std::fs::write(&e.settings, "{ not json").unwrap();
        let rep = e.sync(&desired(), false);
        assert!(!rep.error.is_empty());
        assert_eq!(std::fs::read_to_string(&e.settings).unwrap(), "{ not json");

        let e = Env::new("fresh");
        let rep = e.sync(&desired(), false);
        assert!(rep.configured, "{rep:?}");
        assert_eq!(e.read()["env"]["OTEL_LOGS_EXPORTER"], "otlp");
    }
}
