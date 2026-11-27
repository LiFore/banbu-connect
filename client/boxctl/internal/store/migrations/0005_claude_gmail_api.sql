-- 0005：读取 Claude 登录邮件改用 Gmail API（ADR-025，取代 ADR-024 的 IMAP）。
-- 不再保存 Gmail 应用专用密码，改存 OAuth 授权得到的 refresh token（secret.Box 加密，空表示还没授权）。
-- Google OAuth 客户端（client_id/secret）保存在 settings 表的 gmail_oauth_client（加密）。
ALTER TABLE claude_accounts DROP COLUMN app_password;
ALTER TABLE claude_accounts ADD COLUMN gmail_token BLOB NOT NULL DEFAULT x'';
