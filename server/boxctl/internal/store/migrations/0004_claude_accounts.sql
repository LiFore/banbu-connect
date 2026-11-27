-- 0004：管理服务登记的 Claude 账号（BXC-F1-083）。客户端一键发起 Claude Code 登录，
-- 服务端从该账号的 Gmail（IMAP + 应用专用密码，ADR-024）取登录邮件里的 magic link 交给客户端。
-- 不保存 Claude 的密码和登录凭据；路由盒和桌面客户端也会建这张表，但不使用。
CREATE TABLE claude_accounts (
    id                INTEGER PRIMARY KEY,
    email             TEXT NOT NULL UNIQUE,  -- 小写；也是 Gmail 地址（每个账号一个 Gmail）
    note              TEXT NOT NULL DEFAULT '',
    app_password      BLOB NOT NULL,         -- Gmail 应用专用密码，secret.Box 加密
    devices           TEXT NOT NULL DEFAULT '[]', -- 允许使用的客户端 id；空数组表示全部
    last_login_at     TEXT NOT NULL DEFAULT '',
    last_login_device TEXT NOT NULL DEFAULT '',
    last_error        TEXT NOT NULL DEFAULT '',
    created_at        TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
    updated_at        TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);
