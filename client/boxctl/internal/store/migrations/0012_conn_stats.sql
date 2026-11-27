-- 0012：代理连接统计（S1.8，ADR-048）。只有管理服务使用。
-- 客户端（桌面端、路由盒）每分钟上报一批：分钟汇总、关闭的连接、未归属流量，按（客户端, 序号）去重。
-- 保留：连接记录 7 天，分钟汇总与未归属 30 天，小时用量长期保留（BXC-F0-053）。

CREATE TABLE conn_batches (
    device_id   INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    seq         INTEGER NOT NULL,
    received_at TEXT NOT NULL,
    PRIMARY KEY (device_id, seq)
);

-- source_* 只在路由盒上有：来源 IP 与它的局域网设备。path 是 JSON 数组：代理组 → … → 实际出站节点。
CREATE TABLE conn_minutes (
    id          INTEGER PRIMARY KEY,
    device_id   INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    minute      TEXT NOT NULL,
    path        TEXT NOT NULL,
    exit_node   TEXT NOT NULL,  -- path 的最后一个，按节点筛选和排行用
    grp         TEXT NOT NULL,  -- path 的第一个（规则选中的代理组）
    host        TEXT NOT NULL,
    rule        TEXT NOT NULL,
    payload     TEXT NOT NULL,
    process     TEXT NOT NULL DEFAULT '',
    source_ip   TEXT NOT NULL DEFAULT '',
    source_mac  TEXT NOT NULL DEFAULT '',
    source_name TEXT NOT NULL DEFAULT '',
    upload      INTEGER NOT NULL,
    download    INTEGER NOT NULL,
    conns       INTEGER NOT NULL
);
CREATE INDEX conn_minutes_time ON conn_minutes (minute);
CREATE INDEX conn_minutes_device ON conn_minutes (device_id, minute);

CREATE TABLE conn_unattributed (
    device_id INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    minute    TEXT NOT NULL,
    upload    INTEGER NOT NULL,
    download  INTEGER NOT NULL,
    PRIMARY KEY (device_id, minute)
);

CREATE TABLE conn_records (
    id          INTEGER PRIMARY KEY,
    device_id   INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    start_at    TEXT NOT NULL,
    end_at      TEXT NOT NULL,
    network     TEXT NOT NULL,
    host        TEXT NOT NULL,
    destination TEXT NOT NULL,
    rule        TEXT NOT NULL,
    payload     TEXT NOT NULL,
    path        TEXT NOT NULL,
    exit_node   TEXT NOT NULL,
    grp         TEXT NOT NULL,
    process     TEXT NOT NULL DEFAULT '',
    source_ip   TEXT NOT NULL DEFAULT '',
    source_mac  TEXT NOT NULL DEFAULT '',
    source_name TEXT NOT NULL DEFAULT '',
    upload      INTEGER NOT NULL,
    download    INTEGER NOT NULL
);
CREATE INDEX conn_records_time ON conn_records (end_at);
CREATE INDEX conn_records_device ON conn_records (device_id, end_at);

-- 每小时的代理流量（按客户端，路由盒细到来源设备），长期保留。source_key 是 MAC，没有时为来源 IP，桌面端为空。
CREATE TABLE conn_hours (
    device_id   INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    hour        TEXT NOT NULL,
    source_key  TEXT NOT NULL,
    source_name TEXT NOT NULL DEFAULT '',
    upload      INTEGER NOT NULL,
    download    INTEGER NOT NULL,
    conns       INTEGER NOT NULL,
    PRIMARY KEY (device_id, hour, source_key)
);
CREATE INDEX conn_hours_time ON conn_hours (hour);

-- 桌面客户端上报的自己的局域网地址（BXC-F0-027）：路由盒统计里 MAC 相同的流量归到这台客户端。
CREATE TABLE device_lan (
    device_id  INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    mac        TEXT NOT NULL,
    ip         TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (device_id, mac)
);
CREATE INDEX device_lan_mac ON device_lan (mac);
