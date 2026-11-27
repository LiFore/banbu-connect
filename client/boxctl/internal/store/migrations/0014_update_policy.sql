-- 0014：客户端更新方式（S1.10，ADR-057）。
-- mode：normal（打开首页时提示，原来的做法）/ push（后台发现后发系统通知）/ silent（后台自动下载安装，退出或空闲时生效）；
-- min_version：低于它的客户端必须更新（强制：安装后倒计时约 10 分钟自动重启，可推迟一次）。只对桌面客户端（Q-020）。
ALTER TABLE releases ADD COLUMN mode TEXT NOT NULL DEFAULT 'normal';
ALTER TABLE releases ADD COLUMN min_version TEXT NOT NULL DEFAULT '';

-- 各客户端最近一次更新进度（BXC-F0-058），客户端在进度阶段变化时上报。
CREATE TABLE device_updates (
    device_id  INTEGER PRIMARY KEY REFERENCES devices(id) ON DELETE CASCADE,
    version    TEXT NOT NULL DEFAULT '',
    mode       TEXT NOT NULL DEFAULT '',
    phase      TEXT NOT NULL DEFAULT '',
    error      TEXT NOT NULL DEFAULT '',
    updated_at TEXT NOT NULL
);
