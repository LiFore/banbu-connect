-- 0025：客户端名称可由管理员修改。只有管理服务使用。
--
-- 原来客户端每次联系都用上报的电脑名覆盖 name，管理员改的名称会被马上改回去。
--   - reported_name：客户端最近一次上报的电脑名（恢复自动命名时用）；
--   - name_custom：管理员改过名称时为 1，之后上报的电脑名只记到 reported_name，不再覆盖 name。
-- 已有的客户端：name 就是上报的电脑名（或管理员审批时填的），先记一份到 reported_name。
ALTER TABLE devices ADD COLUMN reported_name TEXT NOT NULL DEFAULT '';
ALTER TABLE devices ADD COLUMN name_custom INTEGER NOT NULL DEFAULT 0;
UPDATE devices SET reported_name = name WHERE name NOT LIKE '待连接 #%';
