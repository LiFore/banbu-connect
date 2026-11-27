-- 0013：管理服务登记的账号按类型区分（BXC-F0-045，ADR-053）：claude（读 Anthropic 登录邮件里的 magic link）、
-- codex（读 OpenAI 登录邮件里的 6 位验证码）。同一个 Gmail 可以同时是 Claude 和 Codex 账号，
-- 所以唯一约束从 email 改为 (kind, email)。SQLite 不能删约束，按官方做法重建表。
CREATE TABLE claude_accounts_new (
    id                INTEGER PRIMARY KEY,
    kind              TEXT NOT NULL DEFAULT 'claude',
    email             TEXT NOT NULL,
    note              TEXT NOT NULL DEFAULT '',
    devices           TEXT NOT NULL DEFAULT '[]',
    last_login_at     TEXT NOT NULL DEFAULT '',
    last_login_device TEXT NOT NULL DEFAULT '',
    last_error        TEXT NOT NULL DEFAULT '',
    created_at        TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
    updated_at        TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
    gmail_token       BLOB NOT NULL DEFAULT x'',
    UNIQUE (kind, email)
);
INSERT INTO claude_accounts_new (id, kind, email, note, devices, last_login_at, last_login_device, last_error, created_at, updated_at, gmail_token)
    SELECT id, 'claude', email, note, devices, last_login_at, last_login_device, last_error, created_at, updated_at, gmail_token FROM claude_accounts;
DROP TABLE claude_accounts;
ALTER TABLE claude_accounts_new RENAME TO claude_accounts;
