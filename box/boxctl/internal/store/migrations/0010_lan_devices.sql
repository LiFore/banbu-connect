-- 局域网设备（路由盒）：备注名与「绕过内核」按 MAC（ADR-040），出口、断网、自定义规则按 IP（ADR-034、ADR-036、ADR-041）。
CREATE TABLE lan_devices (
    mac        TEXT PRIMARY KEY,               -- 小写、冒号分隔
    name       TEXT NOT NULL DEFAULT '',       -- 备注名
    bypass     INTEGER NOT NULL DEFAULT 0,     -- 1：完全不经过内核（TUN exclude-mac-address）
    updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);

CREATE TABLE lan_policies (
    ip           TEXT PRIMARY KEY,
    egress       TEXT NOT NULL DEFAULT 'default', -- default / direct / proxy / group
    egress_group TEXT NOT NULL DEFAULT '',        -- egress = group 时的代理组
    access       TEXT NOT NULL DEFAULT 'allow',   -- allow / block / schedule
    schedule     TEXT NOT NULL DEFAULT '[]',      -- access = schedule 时的放行时段（JSON，guard.Window 列表）
    rules        TEXT NOT NULL DEFAULT '[]',      -- 设备自定义规则（JSON 字符串数组，生成 SUB-RULE）
    updated_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);
