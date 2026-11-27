-- 客户端上报的诊断数据：出口探测结果和系统事件，排查连接问题用。
-- 不含访问记录、Wi-Fi 名称、开关机等使用情况（见 internal/dist/diagnostics.go）。
CREATE TABLE device_diagnostics (
    id          INTEGER PRIMARY KEY,
    device_id   INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    seq         INTEGER NOT NULL, -- 客户端生成，单调递增；重传时据此去重
    time        TEXT NOT NULL,
    kind        TEXT NOT NULL,
    level       TEXT NOT NULL,
    message     TEXT NOT NULL,
    fields      TEXT NOT NULL DEFAULT '{}',
    received_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
    UNIQUE (device_id, seq)
);
CREATE INDEX device_diagnostics_time ON device_diagnostics (device_id, time);

ALTER TABLE devices ADD COLUMN diag_snapshot TEXT NOT NULL DEFAULT '';
ALTER TABLE devices ADD COLUMN diag_at TEXT NOT NULL DEFAULT '';
