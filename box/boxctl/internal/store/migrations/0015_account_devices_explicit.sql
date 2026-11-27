-- 0015：AI 账号（Claude / Codex）的可用客户端改为必须明确分配（ADR-060）：名单为空表示没有客户端能用。
-- 原来名单为空表示所有客户端都能用；为了不影响现有客户端，把这些账号的名单补成现在所有未吊销的客户端。
-- 之后新加入的客户端默认没有任何账号，要在管理服务「AI 账号」里勾选。
UPDATE claude_accounts
SET devices = (SELECT json_group_array(id) FROM (SELECT id FROM devices WHERE revoked = 0 ORDER BY id))
WHERE devices IN ('', '[]');
