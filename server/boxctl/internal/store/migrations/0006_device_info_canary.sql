-- 0006：客户端上报设备名称和系统版本（取代管理员手写的名称）；更新包灰度。
ALTER TABLE devices ADD COLUMN os_version TEXT NOT NULL DEFAULT '';
-- 灰度名单：能收到这个更新包的客户端 id；空数组表示全部客户端。
ALTER TABLE releases ADD COLUMN devices TEXT NOT NULL DEFAULT '[]';
