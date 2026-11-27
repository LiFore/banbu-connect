-- 0024：客户端接入状态（经路由盒、受信 Wi-Fi 直连、本机代理、阻断、未使用代理）。只有管理服务使用。
--
-- 客户端在状态变化时立即上报，没变化时每分钟上报一次，与诊断数据分开（不受「诊断上报」开关影响）。
--   - device_access：每台客户端最近一次上报的状态快照（JSON），「客户端」页的接入状态卡片用它；
--   - device_access_sessions：按接入方式分段的历史。方式（或路由盒）变了就结束上一段、开新的一段；
--     客户端超过几分钟没有上报（关机、断网）时，上一段在最后一次上报时结束。end_at 为空表示还在进行。
CREATE TABLE device_access (
    device_id   INTEGER PRIMARY KEY REFERENCES devices(id) ON DELETE CASCADE,
    mode        TEXT NOT NULL,
    state       TEXT NOT NULL,
    since       TEXT NOT NULL,
    reported_at TEXT NOT NULL
);
CREATE TABLE device_access_sessions (
    id        INTEGER PRIMARY KEY,
    device_id INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    mode      TEXT NOT NULL,
    box_id    TEXT NOT NULL DEFAULT '',
    start_at  TEXT NOT NULL,
    last_seen TEXT NOT NULL,
    end_at    TEXT NOT NULL DEFAULT ''
);
CREATE INDEX device_access_open ON device_access_sessions (device_id, end_at);
CREATE INDEX device_access_time ON device_access_sessions (device_id, start_at);
