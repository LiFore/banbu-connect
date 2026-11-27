-- 0023：AI 账号在线时长（每台客户端用每个账号的时段）。只有管理服务使用。
--
-- 客户端每分钟（以及变化时立即）上报此刻在用的账号：Gemini 是本机当前账号，Claude / Codex 是有进程在运行的工作区
-- （拿不到进程数的 Windows 按「设为默认」的账号算）。管理服务按上报维护时段：
--   - 账号出现在上报里 → 开一段（start_at）；之后每次上报刷新 last_seen；
--   - 账号不在上报里（切走了 / 进程退出了）→ 这一段在这次上报时结束；
--   - 客户端超过几分钟没有上报（关机、断网）→ 这一段在最后一次上报时结束，重新上报后另开一段。
-- end_at 为空表示还在进行。account_id 按 kind 指向 claude_accounts（claude / codex）或 gemini_accounts（gemini）；
-- email 是开段时的快照，账号删掉后统计里仍能显示。
CREATE TABLE ai_online_sessions (
    id         INTEGER PRIMARY KEY,
    device_id  INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    kind       TEXT NOT NULL,
    account_id INTEGER NOT NULL,
    email      TEXT NOT NULL DEFAULT '',
    start_at   TEXT NOT NULL,
    last_seen  TEXT NOT NULL,
    end_at     TEXT NOT NULL DEFAULT ''
);
CREATE INDEX ai_online_open ON ai_online_sessions (device_id, end_at);
CREATE INDEX ai_online_time ON ai_online_sessions (start_at);
CREATE INDEX ai_online_account ON ai_online_sessions (kind, account_id, start_at);
