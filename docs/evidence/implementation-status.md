# 实施状态

已实现 Go 基础服务、严格配置、健康检查、可信代理、CORS、脱敏日志、优雅退出和 SQLite/PostgreSQL 迁移。已增加账号注册、密码登录、找回密码、Token 校验和数据库限流。

本次范围调整：旧账号、密码哈希、Token、验证码、二维码及旧账号关联数据不导入；用户重新注册。扫码登录暂不实现，接口不提供。数据库采用新增 0002 移除暂不使用的二维码表，0001 校验和不变。

历史基础验证：Go 1.27.3、CGO_ENABLED=0；17 项测试（包括本机 PostgreSQL 17.11）通过，Windows amd64 与 Linux amd64/arm64 构建、可执行文件启动与健康检查通过。

本次范围调整后 18 项测试通过，包含临时 PostgreSQL 17.11 和 SQLite 的真实基础契约，以及扫码路由 404 和二维码表移除校验。go vet 通过。目标 NAS、Docker/frp、race 检查和线上联调尚未实测。未连接或修改旧数据库。

## 账号模块实现（2026-10-29）

已接入 7 个账号接口，业务/HTTP 与 GORM 持久化分离，接入 Brevo。仅支持新注册账号，不读取旧凭据，扫码接口仍为 404。使用全新数据库，不连接或导入旧数据库。

实测包含：注册/登录/重置密码完整流程、JSON/表单请求、旧 Token 拒绝、验证码摘要存储/过期/用途隔离/重复消费、邮件失败不可用、唯一冲突事务回滚、8 路并发重置仅一次提交、30 路并发限流恰好放行 10 次、旧窗口退款不影响新窗口、登录限流与成功退款。核心账号与并发契约在本机临时 SQLite 和 PostgreSQL 17.11 上均通过。邮件仅用模拟发件器及本地 HTTP 服务验证模板转义、错误脱敏、拒绝重定向，未发真实邮件。

本轮最终验证：go test ./... -count=1（配置真实 PostgreSQL 测试 DSN）、go vet ./...、go mod verify 全部通过；CGO_ENABLED=0 构建 Windows amd64 和 Linux amd64/arm64 通过。本轮仓库无首个提交且运行账户触发 Git ownership 限制，本地构建使用 -buildvcs=false；未修改全局 Git 信任配置。新 Windows 二进制的 migrate/schema-status/serve 冒烟通过，验证健康检查、账号路由、无邮件配置的503和扫码404。Linux/NAS运行、真实邮件投递及race仍未本机实测。

## 收藏与本地歌单（2026-10-29）

新增 16 个收藏/本地歌单接口，接入主程序；包含默认歌单、成员查询、单首与批量增删、换源、完整排序、版本冲突检查。复用现有表，无新依赖/迁移。未实现远程歌单 follow/sync/copy。

实际验证：完整 HTTP 操作链在 SQLite 与临时 PostgreSQL 17.11 均通过，包括签名鉴权、userId 注入无效、跨用户读写拒绝、默认歌单保护、重复歌曲/重复操作去重、sourceIds 保留、顺序/版本一致和删除级联。并发测试覆盖 12 个同 ID 创建请求、12 个不同 ID 添加同一歌曲，以及两个同版本排序请求恰好一个成功。数据库触发器模拟批量插入中途失败，验证目录/成员/版本/操作记录全部回滚、错误脱敏和相同 operationId 可重试。500 首最大批次添加、跨 200 条 CASE 分块的逆序排序及过期 operationId 重新执行均在两种数据库上通过。

本轮最终验证：带真实临时 PostgreSQL DSN 的 go test ./... -count=1、go vet ./...、go mod verify 全部通过；CGO_ENABLED=0 构建 Windows amd64、Linux amd64/arm64 通过。新 Windows 二进制实际启动，验证鉴权、默认歌单、创建/幂等重放、收藏、添加/读取/排序/移除歌曲和删除歌单，远程 follow 仍为404。二进制已更新至 bin/cyapi.exe；未在真实 NAS/Linux 或 Docker/frp 环境运行，race 未本机实测。

## 本地音源与 Release 同步（2026-10-29）

依据用户提供的 NAS sync.py，已实现 guoyue2010/lxmusic- 最新正式 Release 首 ZIP 同步、中文名修复、共同目录裁切、全文件本地镜像和仅 JS 的公开分发。新增 sync-sources（含 force/dry-run）、sources status/rollback，不开数据库，不读旧 R2 env。移除从未实现的 manifest_file 预留配置，改为 config.yaml 的 sources.repo 和预算参数。

已通过本机真实 ZIP/文件系统及模拟 GitHub 的测试：正式版本/首 ZIP 选择、公开字段、路径编码/穿越、CRC/数量/大小/磁盘预算、digest、只解码一次、中文与嵌套路径、增改删与回滚、完整性漂移重建、并发进程锁、连续 10 次下载失败与 10 次子进程崩溃后的回收、发布失败保留旧指针、读取租约保护 GC、Token 不流向下载主机、拒绝重定向与429有限重试。命令测试验证 status/rollback 不创建数据库且不要求 AUTH_SECRET。符号链接创建权限不足时单项测试显式 skip，未将其记为通过。无真实 R2/NAS 操作，未下载真实 GitHub 大附件。

本轮最终验证：含真实临时 PostgreSQL 的 go test ./... -count=1、go vet ./... 和 go mod verify 通过；CGO_ENABLED=0 的 Windows amd64/Linux amd64/arm64 构建通过，产物更新至 bin/。新 Windows 二进制实际冒烟验证 source status/rollback 不开数据库、不读 AUTH_SECRET、被占用锁时同步 skipped、公开列表/中文路径/原始脚本字节/ETag/非法路径拒绝/损坏503，未发 GitHub/R2 请求。Linux/NAS运行与真实上游附件仍待联调，race未本机实测。
