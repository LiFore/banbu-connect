-- 0011：Gemini（Antigravity）账号联网轮换（S1.7，ADR-046）。路由盒和桌面客户端也会建这些表，但不使用。
--
-- 管理服务保存每个账号的权威凭据（Antigravity 的凭据 JSON，含 refresh token，secret.Box 加密），
-- 按设备分配；客户端上传刷新后的凭据时带上它拿到的版本号，版本号不对就拒绝（BXC-F0-017）。
CREATE TABLE gemini_accounts (
    id              INTEGER PRIMARY KEY,
    email           TEXT NOT NULL UNIQUE,      -- 小写的 Google 账号
    note            TEXT NOT NULL DEFAULT '',
    max_devices     INTEGER NOT NULL DEFAULT 2, -- 最多分配给几台客户端
    credential      BLOB NOT NULL DEFAULT x'',  -- 凭据 JSON，secret.Box 加密；空表示还没登录
    cred_version    INTEGER NOT NULL DEFAULT 0, -- 每接受一次新凭据加 1
    cred_updated_at TEXT NOT NULL DEFAULT '',
    cred_source     TEXT NOT NULL DEFAULT '',   -- 最近一次凭据来自哪台客户端、怎么来的
    created_at      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
    updated_at      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);

-- 账号分配给哪些客户端：只有分配到的客户端能拿到凭据。
CREATE TABLE gemini_assignments (
    account_id INTEGER NOT NULL REFERENCES gemini_accounts(id) ON DELETE CASCADE,
    device_id  INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
    PRIMARY KEY (account_id, device_id)
);

-- 每台客户端当前用的账号，以及管理页面下发、还没执行的切换指令（BXC-F0-018、BXC-F0-023）。
CREATE TABLE gemini_device_state (
    device_id         INTEGER PRIMARY KEY REFERENCES devices(id) ON DELETE CASCADE,
    active_account_id INTEGER NOT NULL DEFAULT 0,
    switch_id         TEXT NOT NULL DEFAULT '',
    switch_to         INTEGER NOT NULL DEFAULT 0,
    reported_at       TEXT NOT NULL DEFAULT ''
);

-- 额度采样（BXC-F0-022）：bucket 是额度的一项（Antigravity 按模型给剩余比例），不写死窗口类型。
CREATE TABLE gemini_quota (
    id         INTEGER PRIMARY KEY,
    account_id INTEGER NOT NULL REFERENCES gemini_accounts(id) ON DELETE CASCADE,
    bucket     TEXT NOT NULL,
    label      TEXT NOT NULL DEFAULT '',
    remaining  REAL NOT NULL,               -- 剩余比例 0~1
    reset_at   TEXT NOT NULL DEFAULT '',    -- RFC 3339，空表示不知道
    device_id  INTEGER NOT NULL DEFAULT 0,
    sampled_at TEXT NOT NULL
);
CREATE INDEX gemini_quota_account ON gemini_quota (account_id, sampled_at);
