# 数据库兼容、事务与数据迁移

## 1. 支持范围与切换承诺

首期支持 SQLite 和 PostgreSQL 两种方言。常规业务使用一套 GORM Repository；数据库初始化、DDL 和少量特定操作集中于 persistence/dialect。只有两种数据库的契约测试都通过，才标记对应模块支持配置切换。

切换 driver/dsn 只选择目标数据库，绝不自动导出源库或清空目标库。目标必须先完成建表、数据导入、校验和序列修正。运行时不双写两库。

## 2. ORM 使用规则

- 使用显式表名、列名、复合主键、索引、NULL 和默认值定义；避免直接嵌入 gorm.Model 导致额外软删除字段和契约变化。
- 普通查询用 GORM；更新零值/false/null 时显式选择列或用 map，不能依赖 Updates(struct) 的零值过滤。
- 使用参数化查询；排序列名来自固定映射，不拼接客户端 SQL。
- 普通业务不依赖 rowid、changes()、last_insert_rowid、INSERT OR IGNORE 或 SQLite 日期函数。需要生成 ID 时使用驱动/ORM 返回结果；批量插入后的 ID 与实际插入数需要测试。
- json_each 批量写入改为 Go 解析后 CreateInBatches 或显式分批事务；source_ids_json 等暂作不透明文本，避免提前绑定 JSONB 查询。
- 数据库错误映射为 AlreadyExists、NotFound、Conflict、Busy 等业务语义；不通过匹配任意错误字符串实现兼容。
- 不采用“RowsAffected=0 一律等于不存在”。更新相同值、版本冲突、缺失行需明确区分；两种库用相同测试证明。

## 3. 模型与数据范围

| 数据域 | 现有相关表 | 保留要点 |
| --- | --- | --- |
| 账号 | users、verification_codes、auth_rate_limits | 新账号 ID、密码哈希、token_version、期限和一次性消费 |
| 收藏与绑定 | favorites、account_bindings | source_ids_json、平台 ID、用户隔离 |
| 歌单 | playlists、playlist_tracks_catalog、local_playlist_items | 默认歌单唯一性、复合唯一、rank 和版本 |
| 同步快照 | playlist_snapshots、playlist_snapshot_items | active_snapshot_id、远端顺序与重复项 |
| 幂等 | playlist_operations | 用户/操作 ID 唯一、scope、状态及原响应 |
| 播放与统计 | playback_state、listening_stats、play_counts | 累加语义、时间及排序稳定性 |
| 赞助 | donations 及 users 赞助字段 | 保留数据，不自动启用不受约束的写入口 |

Go 业务层时间统一 UTC；API 输出按客户端契约格式化。首期无历史账号数据导入，也不需要兼容旧密码或旧日期数据。金额用精确表示或保留原值并明确映射，不能直接强制取整。听歌时长当前允许有限正数，不得擅自改为只接受整数。

跨库保持 NULL、空字符串、大小写和唯一性语义。本系统将来换库时保留其已有 ID；客户端 JSON 数字不得超过既有安全整数范围；平台歌曲 ID 用字符串。

SQLite 和 PostgreSQL 均可用部分唯一索引，但写法和谓词必须按实际 schema 编写迁移，并测试默认歌单唯一、远端来源唯一及 CHECK 约束。

## 4. 事务与并发不变量

1. 验证码并发消费最多成功一次；改密、token_version 递增和相关会话失效按同一业务原子边界处理。bcrypt 计算不得长时间占用写事务。
2. 登录限流保留“预占额度、成功只退还本次账号额度、不退 IP 额度”的窗口语义；跨窗口退款不能扣减新窗口，计数不为负。
3. 本地歌单修改与 revision 更新原子提交，operationId 重放返回已保存结果，scope 不同报冲突。崩溃后的 pending 操作必须有恢复策略，不能无限悬挂或重复应用。
4. 远程歌单同步：申请 sync_revision → 事务外请求上游 → 分批构建 staging 快照 → 核验数量 → 条件切换 active_snapshot_id。binding_version/sync_revision 不匹配时旧请求不得覆盖新结果。
5. 排序依靠显式 revision 和条件更新，不能依赖 SQLite 单写者这一偶然条件来保证 PostgreSQL 下正确。
6. 统计使用原子累加，首期请求成功即已提交。不增加内存延迟落盘以免改变断电数据丢失语义。
7. PostgreSQL 事务内遇到唯一冲突后不能继续当正常事务查询；使用正确的 ON CONFLICT、savepoint 或重开事务。SQLite 成功不代表此路径可移植。

上游网络和发送邮件不放在数据库写事务中。数据库事务不能保证外部邮件 exactly-once：失败补偿、限流和重试规则需明确测试。

## 5. SQLite 运行策略

- 本地磁盘，优先 SSD；不使用 SMB/NFS 数据文件。数据库创建/维护阶段设置并核对持久化的 journal_mode=WAL；连接级 PRAGMA 单独逐连接初始化，不在每次查询中重复切换日志模式。
- 初期限制最大连接数为 1，但连接仍可能因空闲回收、生命周期或驱动错误而重建。从首期起，通过所选驱动支持的 DSN 或连接初始化钩子，对每个新建物理连接设置 foreign_keys=ON、busy_timeout 和 synchronous=FULL；初始化在事务开始前完成，校验失败则关闭该连接并报告错误，不将其交给业务查询。禁止仅在启动时对连接池执行一次 PRAGMA 就认为全程有效。扩展读写池时沿用同一初始化规则并明确各池职责。
- 初始 busy timeout 可取 5 秒，但仍受请求总 deadline 约束；没有无限重试或无界队列。
- 初期优先同步耐久性，synchronous=FULL；如后续改 NORMAL，必须记录突然断电时最近提交可能丢失的取舍。
- 定期观察 WAL 大小、checkpoint、慢查询和 Busy，不在高峰强制 VACUUM。
- 在线备份用驱动支持的快照/备份能力，或停服并正确 checkpoint 后备份；禁止运行中只复制 .db 主文件而漏掉 WAL。

## 6. 版本化迁移

每种数据库有独立递增迁移与校验和，嵌入二进制。migrate 子命令串行执行并记录版本；serve 检查版本，不隐式执行破坏性 DDL。PostgreSQL 使用数据库锁防并发迁移；SQLite 迁移时安排维护窗口并用数据库事务/部署互斥保护。

旧 migrations/0006_playlist_v2.sql 含 DROP TABLE：不能将 0001—0009 对已有线上数据重新执行。旧迁移仅作为理解当前结构的材料，新项目为最终结构创建新的 baseline。

现有 D1 导出可能包含 SQLite/D1 专属 DDL 与迁移表。导出内容先在隔离环境检查；不得对 PostgreSQL 直接执行 D1 SQL，也不得未经转换写入新版空库。

## 7. 全新账号与数据库初始化

首期直接创建空的 SQLite 或 PostgreSQL 数据库，不实施 D1 数据导入，不开发 import-legacy，也不以线上 D1 schema 或快照作为开发/上线前置条件。

- 用户重新通过邮箱验证码注册，重新建立本系统会话。旧账号、密码哈希、Token、验证码、二维码会话、旧限流状态均不导入。
- 旧账号关联的收藏、歌单、播放状态、统计和赞助状态不自动迁移，不将旧 user_id 映射到新注册账号。
- 独立生成并保存新 AUTH_SECRET，不复用旧系统签名密钥。旧 Token 即使结构相同也不能被新系统接受。
- 空库运行 migrate 后验证无用户数据、无扫码登录表及路由，再完成新注册—登录—改密—会话撤销测试。
- 不修改已生成的 0001 baseline 校验和；新增 0002_remove_qr_login.sql 删除首期不使用的 qr_login_sessions 表。该表仅来自初始结构，Go 尚未实现写入；生产 migrate 必须按正常升级流程显式执行。
- 旧系统数据仅保留在原处；不执行清库、删除 Cloudflare 资源或用户数据的操作。将来若需要导入历史数据，另行设计账号映射与验证流程。

## 8. SQLite → PostgreSQL 后续迁移

先运行相同版本的 PostgreSQL migrations；停写、做一致性导出，按显式依赖顺序导入；修正 identity/sequence，验证约束、计数、摘要和关键事务。修改 driver/dsn、启动、完成只读验证后放开写入。

恢复旧库的回滚必须考虑 PostgreSQL 接收的新写入；需要验证过的反向导入/增量回放或接受明确的数据丢失窗口。仅改回配置不能保证数据回滚。

## 9. 共同契约测试

两种数据库必须覆盖：NULL/零值、唯一约束、外键删除、批量返回值、事务回滚、并发默认歌单创建、验证码消费、限流窗口、幂等重放、过期同步隔离、排序冲突、统计并发累加、导入后的序列。SQLite 必须用文件库测试 WAL 与多连接，不仅依赖 :memory:。P1 必须强制关闭并重建物理连接，在替代连接上核验 foreign_keys、busy_timeout、synchronous，并实际尝试写入孤儿外键以证明仍被拒绝；另测锁竞争受 busy timeout 与请求 deadline 限制。不能仅依赖一次启动自检。