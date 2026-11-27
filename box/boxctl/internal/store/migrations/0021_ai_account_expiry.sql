-- 0021：AI 账号的订阅到期日（管理员手动填写，只用于显示和提醒，不向服务商查询）。
-- 格式 YYYY-MM-DD，空串表示没填；按提醒规则的时区，当天结束算到期。
ALTER TABLE claude_accounts ADD COLUMN sub_expires TEXT NOT NULL DEFAULT '';
ALTER TABLE gemini_accounts ADD COLUMN sub_expires TEXT NOT NULL DEFAULT '';

-- 令牌集中续期（ADR-071）：Refresh Token 只留在管理服务（Codex 另有一台持有者客户端），客户端只拿 Access Token。
-- holder_device：Codex 持有 Refresh Token 的客户端，0 表示自动（分配列表里的第一台）。
-- cred_expires：托管的 Access Token 过期时间（Unix 毫秒，0 表示不知道），页面显示用。
-- refresh_at / refresh_error：最近一次续期成功的时间、最近一次失败的原因（成功后清空）。
ALTER TABLE claude_accounts ADD COLUMN holder_device INTEGER NOT NULL DEFAULT 0;
ALTER TABLE claude_accounts ADD COLUMN cred_expires INTEGER NOT NULL DEFAULT 0;
ALTER TABLE claude_accounts ADD COLUMN refresh_at TEXT NOT NULL DEFAULT '';
ALTER TABLE claude_accounts ADD COLUMN refresh_error TEXT NOT NULL DEFAULT '';
