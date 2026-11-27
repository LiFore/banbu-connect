-- 0017：规则方案（ADR-062）。默认的规则和远程规则集（settings 表）下发给所有客户端；
-- 分配了方案的客户端改用方案里的整份规则和远程规则集。一台客户端最多属于一个方案。
CREATE TABLE rule_profiles (
    id         INTEGER PRIMARY KEY,
    name       TEXT NOT NULL UNIQUE,
    rules      TEXT NOT NULL DEFAULT '[]',   -- JSON，带分段标记，和 dist_rules 相同
    rule_sets  TEXT NOT NULL DEFAULT 'null', -- JSON；null 表示客户端用内置默认
    devices    TEXT NOT NULL DEFAULT '[]',   -- JSON，客户端 ID
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
    updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);
