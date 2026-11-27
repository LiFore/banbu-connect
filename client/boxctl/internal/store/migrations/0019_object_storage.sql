-- 0019：对象存储与云盘（S1.11）。
-- 更新包记下存在哪里：local（数据目录 releases/<平台>/<版本>/<文件>，object_key 为空）或 oss（object_key 是完整对象路径）。
ALTER TABLE releases ADD COLUMN storage TEXT NOT NULL DEFAULT 'local';
ALTER TABLE releases ADD COLUMN object_key TEXT NOT NULL DEFAULT '';

-- 云盘文件：管理端上传，所有客户端可见（发布的）。置顶的排在最前，组内按 sort 从小到大。
CREATE TABLE files (
    id          INTEGER PRIMARY KEY,
    name        TEXT NOT NULL,              -- 显示的标题
    file_name   TEXT NOT NULL,              -- 下载保存的文件名
    storage     TEXT NOT NULL,              -- local / oss
    object_key  TEXT NOT NULL,
    size        INTEGER NOT NULL,
    description TEXT NOT NULL DEFAULT '',   -- 简短说明（Markdown）
    pinned      INTEGER NOT NULL DEFAULT 0,
    sort        INTEGER NOT NULL DEFAULT 0,
    published   INTEGER NOT NULL DEFAULT 1,
    created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
    updated_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
);
