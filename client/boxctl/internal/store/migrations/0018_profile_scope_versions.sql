-- 0018：方案扩展（ADR-064）。
-- 1. 方案除了规则和远程规则集，还决定拥有哪些节点、代理组和线路选择；nodes / proxy_groups 为 null 表示全部。
-- 2. 「默认」也成为一行方案（is_default = 1），原来存在 settings 里的默认规则、远程规则集、线路选择迁过来。
-- 3. 每个方案有唯一的随机代码 code；每次内容变化记一个版本「code-时间戳」，可以回滚。
-- 4. 远程规则集的下载结果缓存在 rule_set_cache，页面查看和规则预览都先读缓存。
ALTER TABLE rule_profiles ADD COLUMN code TEXT NOT NULL DEFAULT '';
ALTER TABLE rule_profiles ADD COLUMN version TEXT NOT NULL DEFAULT '';
ALTER TABLE rule_profiles ADD COLUMN nodes TEXT NOT NULL DEFAULT 'null';
ALTER TABLE rule_profiles ADD COLUMN proxy_groups TEXT NOT NULL DEFAULT 'null';
ALTER TABLE rule_profiles ADD COLUMN selections TEXT NOT NULL DEFAULT '{}';
ALTER TABLE rule_profiles ADD COLUMN is_default INTEGER NOT NULL DEFAULT 0;

INSERT INTO rule_profiles (name, rules, rule_sets, selections, is_default) VALUES (
    '默认',
    COALESCE((SELECT value FROM settings WHERE key = 'dist_rules'), '[]'),
    COALESCE((SELECT value FROM settings WHERE key = 'dist_rule_sets'), 'null'),
    COALESCE((SELECT value FROM settings WHERE key = 'dist_selections'), '{}'),
    1
);
DELETE FROM settings WHERE key IN ('dist_rules', 'dist_rule_sets', 'dist_selections');

UPDATE rule_profiles SET code = lower(hex(randomblob(3)));
UPDATE rule_profiles SET version = code || '-' || strftime('%Y%m%d%H%M%S', 'now', 'localtime');
CREATE UNIQUE INDEX rule_profiles_code ON rule_profiles (code);
CREATE UNIQUE INDEX rule_profiles_one_default ON rule_profiles (is_default) WHERE is_default = 1;

CREATE TABLE rule_profile_versions (
    id         INTEGER PRIMARY KEY,
    profile_id INTEGER NOT NULL REFERENCES rule_profiles (id) ON DELETE CASCADE,
    version    TEXT NOT NULL,
    body       TEXT NOT NULL, -- JSON：nodes、groups、selections、rules、rule_sets
    note       TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
    UNIQUE (profile_id, version)
);
INSERT INTO rule_profile_versions (profile_id, version, body, note)
SELECT id, version,
       json_object('nodes', json(nodes), 'groups', json(proxy_groups), 'selections', json(selections),
                   'rules', json(rules), 'rule_sets', json(rule_sets)),
       '初始版本'
FROM rule_profiles;

CREATE TABLE rule_set_cache (
    url        TEXT NOT NULL,
    format     TEXT NOT NULL DEFAULT '',
    rules      TEXT NOT NULL, -- 解析后的规则，一行一条
    count      INTEGER NOT NULL,
    bytes      INTEGER NOT NULL,
    fetched_at TEXT NOT NULL,
    PRIMARY KEY (url, format)
);
