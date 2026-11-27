-- 0022：AI 账号的套餐 / Team 标记，以及自建邮箱（IMAP，如 BillionMail）收信。
--
-- 套餐（plan）由管理员手动标记（如 Pro、Max 5x、Plus、Team），只用于显示和筛选，不向服务商查询。
-- Claude 账号区分个人号和 Team 号：account_type 为 personal / team；Team 号再标主号（owner）或成员号（member）、
-- Team 名称和 Team 的邮箱域名（不填时页面按账号邮箱的域名显示）。
ALTER TABLE claude_accounts ADD COLUMN plan TEXT NOT NULL DEFAULT '';
ALTER TABLE claude_accounts ADD COLUMN account_type TEXT NOT NULL DEFAULT 'personal';
ALTER TABLE claude_accounts ADD COLUMN team_role TEXT NOT NULL DEFAULT '';
ALTER TABLE claude_accounts ADD COLUMN team_name TEXT NOT NULL DEFAULT '';
ALTER TABLE claude_accounts ADD COLUMN team_domain TEXT NOT NULL DEFAULT '';
ALTER TABLE gemini_accounts ADD COLUMN plan TEXT NOT NULL DEFAULT '';

-- 自建邮箱收信：管理服务用 IMAP 登录账号的邮箱读登录邮件（不走 Gmail OAuth）。IMAP 服务器在 settings 表的
-- mail_imap_server 里统一设置；每个账号只存登录名（空表示用账号邮箱）和密码（secret.Box 加密，空表示不用 IMAP）。
-- 填了 IMAP 密码时优先用 IMAP，否则用 Gmail 授权。
ALTER TABLE claude_accounts ADD COLUMN mail_user TEXT NOT NULL DEFAULT '';
ALTER TABLE claude_accounts ADD COLUMN mail_password BLOB NOT NULL DEFAULT x'';
