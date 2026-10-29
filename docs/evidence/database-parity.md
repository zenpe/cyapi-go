# 数据库基础验证

SQLite：GORM 官方方言 v1.6.0，显式 DriverName=sqlite 接入 modernc.org/sqlite v1.61.0，纯 Go 构建。PostgreSQL：GORM 方言 v1.6.0。

基础契约覆盖空库建表、迁移版本、生成 ID、唯一约束、NULL/零值、事务回滚、外键、小数听歌时长；SQLite 还覆盖连接替换、锁等待、迁移互斥、checksum 和未来版本保护、DDL 失败回滚及拒绝接管旧非空库。

本次以新增 0002 移除首期不用的 qr_login_sessions，不修改已存在的 0001 校验和。共享契约增加“二维码表不存在”校验，HTTP 测试增加暂缓扫码路由返回 404 的校验。测试执行结果见 implementation-status.md。

首期不实施旧账号或 D1 数据迁移；将来 SQLite 切换 PostgreSQL 的迁移能力仍保留。
