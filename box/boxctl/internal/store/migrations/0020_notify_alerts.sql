-- 0020：消息推送与用量提醒（S1.12）。

-- 每小时经过每个代理组 / 节点的流量（路径上的每一跳都算，链式代理的中间节点也统计），长期保留，
-- 用于「节点流量超过阈值」按日、按月累计。hour 是 UTC 整点（RFC 3339）。
CREATE TABLE traffic_hours (
    hour     TEXT NOT NULL,
    name     TEXT NOT NULL,
    upload   INTEGER NOT NULL,
    download INTEGER NOT NULL,
    PRIMARY KEY (hour, name)
);

-- 从还在保留期内的分钟汇总回填。
INSERT INTO traffic_hours (hour, name, upload, download)
SELECT substr(m.minute, 1, 13) || ':00:00Z', j.value, SUM(m.upload), SUM(m.download)
FROM conn_minutes m, json_each(m.path) j
WHERE j.value <> ''
GROUP BY 1, 2;

-- 已经提醒过的事项：同一个 key 只提醒一次，条件恢复后删除，下次再超过时重新提醒。
CREATE TABLE alert_state (
    key      TEXT PRIMARY KEY,
    fired_at TEXT NOT NULL
);

-- 推送记录（最近的，管理页面查看）。
CREATE TABLE notify_log (
    id         INTEGER PRIMARY KEY,
    kind       TEXT NOT NULL,              -- node_traffic / sub_usage / sub_expire / ai_quota / sub_update / test
    title      TEXT NOT NULL,
    body       TEXT NOT NULL,
    result     TEXT NOT NULL DEFAULT '',   -- 各渠道的结果，空表示全部成功
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);
