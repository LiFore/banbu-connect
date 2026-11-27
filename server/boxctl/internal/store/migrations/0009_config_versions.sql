-- 路由盒配置版本（M8-04）：内核配置、guard 策略、dnsmasq 片段作为同一个版本记录，可查看和对比。
-- 内核配置含节点凭据，不进数据库：存在数据目录 versions/<id>.yaml（0600），这里只记摘要。
CREATE TABLE config_versions (
    id           INTEGER PRIMARY KEY,
    config_hash  TEXT NOT NULL,             -- 内核配置内容摘要（core.ConfigHash）
    guard_policy TEXT NOT NULL,             -- guard 策略（JSON，不含运行状态：阻断、内核是否在运行）
    dnsmasq      TEXT NOT NULL,             -- dnsmasq 片段
    status       TEXT NOT NULL DEFAULT 'pending', -- pending / confirmed / unconfirmed / rolled_back / failed
    reason       TEXT NOT NULL DEFAULT '',
    created_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);
CREATE INDEX config_versions_hash ON config_versions (config_hash);
