-- 客户端接入申请：客户端提交设备信息，管理员授权后客户端用申请时拿到的密钥领取令牌，免去手动输入令牌。
CREATE TABLE enrollments (
    id          INTEGER PRIMARY KEY,
    secret_hash TEXT NOT NULL UNIQUE,      -- 申请时发给客户端的密钥（只存哈希），领取令牌时校验
    install_id  TEXT NOT NULL UNIQUE,      -- 客户端安装时生成的随机 ID：同一台重复申请时合并成一条
    name        TEXT NOT NULL,             -- 设备名称
    platform    TEXT NOT NULL DEFAULT '',  -- 如 darwin/arm64
    os_version  TEXT NOT NULL DEFAULT '',
    app_version TEXT NOT NULL DEFAULT '',
    ip          TEXT NOT NULL DEFAULT '',
    status      TEXT NOT NULL DEFAULT 'pending', -- pending / approved（待领取）/ claimed（已领取）/ rejected
    device_id   INTEGER REFERENCES devices(id) ON DELETE SET NULL,
    created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
    updated_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);
CREATE INDEX enrollments_status ON enrollments (status, ip);
