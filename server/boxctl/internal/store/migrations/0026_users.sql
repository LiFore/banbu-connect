-- 0026：用户。一个用户（有姓名）可以有多台设备；AI 账号可以分配给设备，也可以分配给用户（用户名下所有设备都能用）。
--
--   - users：用户，只有姓名和备注；
--   - devices.user_id：设备所属的用户，0 表示不属于任何用户（一台设备只属于一个用户）；
--   - ai_account_users：账号分配给了哪些用户。scope 为 claude 表示 claude_accounts（Claude 和 Codex 共用这张表），
--     gemini 表示 gemini_accounts；账号删除时由程序清理（账号表之间 id 会重复，没有外键）；
--   - gemini_effective_assignments：Gemini 账号实际能用的设备 = 直接分配的设备 + 分配用户名下的设备。
-- Claude / Codex 账号原来的 devices（直接分配的设备）不变，生效的设备在程序里合并。
CREATE TABLE users (
    id         INTEGER PRIMARY KEY,
    name       TEXT NOT NULL,
    note       TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);
ALTER TABLE devices ADD COLUMN user_id INTEGER NOT NULL DEFAULT 0;
CREATE TABLE ai_account_users (
    scope      TEXT NOT NULL,
    account_id INTEGER NOT NULL,
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    PRIMARY KEY (scope, account_id, user_id)
);
CREATE VIEW gemini_effective_assignments AS
    SELECT account_id, device_id FROM gemini_assignments
    UNION
    SELECT u.account_id, d.id FROM ai_account_users u JOIN devices d ON d.user_id = u.user_id WHERE u.scope = 'gemini';
