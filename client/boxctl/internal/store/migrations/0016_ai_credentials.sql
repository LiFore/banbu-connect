-- 0016：管理服务托管 Claude / Codex 的 OAuth 凭据并下发给分配到的客户端（ADR-061）。
-- credential 是 secret.Box 加密的 JSON（access / refresh token 等），版本号同 Gemini：每次接受新凭据加 1。
-- quota 是客户端按账号上报的额度（JSON），管理页面显示。
ALTER TABLE claude_accounts ADD COLUMN credential BLOB NOT NULL DEFAULT x'';
ALTER TABLE claude_accounts ADD COLUMN cred_version INTEGER NOT NULL DEFAULT 0;
ALTER TABLE claude_accounts ADD COLUMN cred_updated_at TEXT NOT NULL DEFAULT '';
ALTER TABLE claude_accounts ADD COLUMN cred_source TEXT NOT NULL DEFAULT '';
ALTER TABLE claude_accounts ADD COLUMN quota TEXT NOT NULL DEFAULT '';
ALTER TABLE claude_accounts ADD COLUMN quota_at TEXT NOT NULL DEFAULT '';
ALTER TABLE claude_accounts ADD COLUMN quota_device TEXT NOT NULL DEFAULT '';
