-- 0003：管理服务（boxctl server）向 Banbu Connect 客户端下发配置和更新包。
-- 路由盒和桌面客户端也会建这几张表，但不使用。

-- 每次发布一个版本；body 是加密后的 dist.Profile（含节点凭据）。
CREATE TABLE profiles (
    version    INTEGER PRIMARY KEY,
    note       TEXT NOT NULL DEFAULT '',
    body       BLOB NOT NULL,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);

-- 绑定的客户端。只存令牌的 SHA-256。
CREATE TABLE devices (
    id              INTEGER PRIMARY KEY,
    name            TEXT NOT NULL,
    token_hash      TEXT NOT NULL UNIQUE,
    created_at      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
    last_seen       TEXT NOT NULL DEFAULT '',
    last_ip         TEXT NOT NULL DEFAULT '',
    app_version     TEXT NOT NULL DEFAULT '',
    platform        TEXT NOT NULL DEFAULT '',
    profile_version INTEGER NOT NULL DEFAULT 0,
    revoked         INTEGER NOT NULL DEFAULT 0
);

-- 客户端更新包（Tauri updater 的产物和签名）。文件保存在数据目录的 releases/ 下。
CREATE TABLE releases (
    id         INTEGER PRIMARY KEY,
    version    TEXT NOT NULL,
    target     TEXT NOT NULL,  -- darwin-aarch64、darwin-x86_64、windows-x86_64
    notes      TEXT NOT NULL DEFAULT '',
    file_name  TEXT NOT NULL,
    signature  TEXT NOT NULL,
    size       INTEGER NOT NULL,
    published  INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
    UNIQUE (version, target)
);
