-- 0001：核心实体。
-- 约定：协议相关的可变字段放 JSON 列（spec/options），需要查询/约束的字段单独成列；
-- *_secret 列存放加密后的凭据（M1-08），永不存明文。

CREATE TABLE subscriptions (
    id            TEXT PRIMARY KEY,
    name          TEXT NOT NULL UNIQUE,
    display_name  TEXT NOT NULL DEFAULT '',
    url_secret    BLOB,
    user_agent    TEXT NOT NULL DEFAULT '',
    fetch_via     TEXT NOT NULL DEFAULT 'direct',      -- 'direct' 或代理组 id（M2-02）
    enabled       INTEGER NOT NULL DEFAULT 1,
    options       TEXT NOT NULL DEFAULT '{}',          -- 过滤/重命名/更新周期（M2-05..07, M2-11）
    userinfo      TEXT NOT NULL DEFAULT '{}',          -- subscription-userinfo 解析结果（M2-04）
    last_fetch_at TEXT,
    last_error    TEXT NOT NULL DEFAULT '',
    created_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
    updated_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);

CREATE TABLE proxy_nodes (
    id              TEXT PRIMARY KEY,                  -- 订阅节点为稳定哈希（M2-08），手工节点为随机 id
    subscription_id TEXT REFERENCES subscriptions(id) ON DELETE CASCADE,
    kind            TEXT NOT NULL CHECK (kind IN ('subscription', 'manual', 'isp')),
    protocol        TEXT NOT NULL,
    display_name    TEXT NOT NULL,
    region          TEXT NOT NULL DEFAULT '',
    server          TEXT NOT NULL,
    port            INTEGER NOT NULL,
    enabled         INTEGER NOT NULL DEFAULT 1,
    spec            TEXT NOT NULL DEFAULT '{}',        -- 非敏感协议字段
    secret          BLOB,                              -- 密码/UUID 等
    dialer_ref      TEXT NOT NULL DEFAULT '',          -- 前置代理：节点或组 id（M3-06）
    tags            TEXT NOT NULL DEFAULT '[]',
    created_at      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
    updated_at      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);
CREATE INDEX idx_proxy_nodes_sub ON proxy_nodes(subscription_id);

CREATE TABLE proxy_groups (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL UNIQUE,
    type       TEXT NOT NULL CHECK (type IN ('select', 'url-test', 'fallback', 'load-balance')),
    strict     INTEGER NOT NULL DEFAULT 0,             -- 严格组：任何路径不可达 DIRECT（M4-05）
    sort_order INTEGER NOT NULL DEFAULT 0,
    members    TEXT NOT NULL DEFAULT '{}',             -- {explicit:[ref...], filter:{...}}（M4-02/03）
    options    TEXT NOT NULL DEFAULT '{}',             -- url/interval/tolerance/strategy/空组兜底
    icon       TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
    updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);

CREATE TABLE rule_providers (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL UNIQUE,
    behavior   TEXT NOT NULL CHECK (behavior IN ('domain', 'ipcidr', 'classical')),
    format     TEXT NOT NULL DEFAULT 'yaml',
    url        TEXT NOT NULL,
    interval   INTEGER NOT NULL DEFAULT 86400,
    enabled    INTEGER NOT NULL DEFAULT 1,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
    updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);

CREATE TABLE rules (
    id         TEXT PRIMARY KEY,
    section    TEXT NOT NULL CHECK (section IN ('custom', 'service', 'direct', 'proxy', 'final')),
    sort_order INTEGER NOT NULL DEFAULT 0,
    spec       TEXT NOT NULL,                          -- {type, payload, no_resolve, children...}（M5-01/02）
    target     TEXT NOT NULL,                          -- 组 id 或 DIRECT/REJECT
    enabled    INTEGER NOT NULL DEFAULT 1,
    note       TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
    updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);
CREATE INDEX idx_rules_order ON rules(section, sort_order);

-- 一次「配置版本」：三种产物同版本号，一起应用、一起回滚（M8-04）
CREATE TABLE config_revisions (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    status       TEXT NOT NULL CHECK (status IN ('candidate', 'applied', 'failed', 'rolled_back')),
    mihomo_yaml  TEXT NOT NULL,
    guard_policy TEXT NOT NULL,
    dnsmasq_conf TEXT NOT NULL,
    note         TEXT NOT NULL DEFAULT '',
    error        TEXT NOT NULL DEFAULT '',
    created_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);

-- 单例配置：DNS 模板、高级参数、guard 模式等，key 为配置域
CREATE TABLE settings (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);
