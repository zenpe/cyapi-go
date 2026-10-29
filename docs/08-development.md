# 开发与运行（首批实现）

当前实现：CLI、严格 YAML 配置、根路径、存活/就绪检查、可信代理 IP 解析、CORS、结构化日志、优雅退出、SQLite/PostgreSQL 版本化迁移。账号注册/登录/重置密码/Token 校验已实现；收藏和本地歌单已实现；本地音源分发、sync-sources/status/rollback 已实现；其他业务路由及 backup 尚未实现；import-legacy 不在首期范围，未知命令会明确失败；不会用空成功响应代替功能。

## 本地启动

需要 Go 1.27.3（本次使用的工具链）；NAS 运行编译产物无需安装 Go。

```powershell
Copy-Item configs/config.example.yaml config.yaml
$env:CYAPI_AUTH_SECRET = '<自行生成并安全保存的随机签名密钥，至少32字节>'
go run ./cmd/cyapi migrate --config config.yaml
go run ./cmd/cyapi schema-status --config config.yaml
go run ./cmd/cyapi serve --config config.yaml
```

根路径保留旧 `{ "status": 200, "data": "OK" }`；GET /health/live 检查进程，GET /health/ready 检查迁移版本与校验和。serve 不自动建表，缺密钥或 schema 不匹配时拒绝启动。migrate 不要求 AUTH_SECRET，但拒绝未纳管的非空数据库。

使用全新空库，用户重新注册；不迁移旧账号、密码哈希、会话及关联用户数据，不开发 D1 导入器。初始结构参考旧仓库功能模型，但不是旧库兼容承诺。扫码登录暂不实现，0002 移除初始结构中的二维码会话表；0001 校验和保持不变。

## 配置

相对文件路径以配置文件目录为基准。未知 YAML 字段、多个 YAML 文档和超过 1 MiB 的配置被拒绝；错误不回显 YAML 内容。数据库 DSN 和配置整体不输出到日志。

当前支持的覆盖变量：CYAPI_LISTEN、CYAPI_PUBLIC_BASE_URL、CYAPI_DATABASE_DRIVER、CYAPI_DATABASE_DSN。`database.dsn_env` 指定读取其他环境变量；CYAPI_DATABASE_DSN 优先。其他配置字段暂不提供通用环境变量映射。

`auth.secret_env` 指向密钥环境变量；migrate/schema-status/version 不读取此密钥。mail 配置已用于 Brevo 验证码邮件；sources 已用于本地音源分发与 Release ZIP 同步，cache/providers 仍为后续模块预留。

应用只从可信直连代理接受单一 X-Real-IP，忽略未信任来源及伪造 X-Forwarded-For/CF-Connecting-IP。入口必须覆盖 X-Real-IP，trusted_proxies 使用实际直接连接地址的 CIDR。CORS 只允许精确配置的 HTTP(S) origin，默认不允许跨域网页请求；无 Origin 的原生客户端不受此配置限制。

PostgreSQL 设置 driver=postgres、dsn_env=CYAPI_PG_DSN；使用专用空数据库/专用 schema。SQLite 使用普通本地文件路径，不接受 file: URI、:memory: 或用户附加 PRAGMA。

## 验证

```powershell
go mod verify
go vet ./...
go test ./... -count=1
$env:CYAPI_TEST_POSTGRES_DSN = 'postgres://user:password@127.0.0.1:5432/testdb?sslmode=disable'
go test ./... -count=1 -v
```

PostgreSQL 测试要求测试账户有 CREATE SCHEMA 权限，仅在随机命名的自有 schema 内测试并清理。未配置测试 DSN 时该测试显式 skip，不等于 PostgreSQL 已验证。CI 提供独立 PostgreSQL 17 服务并运行该测试和 race 检查。

SQLite 使用 GORM 官方方言，指定 DriverName=sqlite 接入 modernc 纯 Go 驱动；不是默认的 sqlite3 CGO 驱动。每次物理连接打开通过 _pragma 设置 foreign_keys、busy_timeout、synchronous。本地测试覆盖重建连接、外键、锁等待、迁移互斥、checksum、事务回滚等。

## 构建与容器

```powershell
$env:CGO_ENABLED = '0'
$env:GOOS = 'linux'
$env:GOARCH = 'amd64' # arm64 NAS 改为 arm64
go build -trimpath -o dist/cyapi-linux-amd64 ./cmd/cyapi
```

发布前仍需在目标 NAS 实际运行。Docker 构建入口为 `docker build -f packaging/Dockerfile -t cyapi-go:dev .`；镜像内用户 UID/GID 为 10001，挂载目录需授权该用户读写。配置挂载 /config/config.yaml，使用绝对数据库路径 /data/cyapi.db 和 sources.directory=/data/sources；容器监听需配置为 0.0.0.0:8080，宿主发布范围按 frpc 网络限定。默认 CMD 不执行 migrate，必须先用相同新镜像执行一次 migrate。

升级用新版本绝对路径执行迁移后再切换，具体见部署文档。容器、Linux 运行及 NAS 资源指标以实施状态中的实测记录为准。

## 账号模块

已接入 GET /auth/registration-status、POST /auth/register/send-code、POST /auth/register、POST /auth/login、POST /auth/reset-password/send-code、POST /auth/reset-password、GET /auth/validate-token。POST 支持 JSON 字符串字段或 URL 编码表单，最大 16 KiB。校验 Token 使用 Authorization: Bearer <token>。

注册沿用 QQ 邮箱（5–11 位数字@qq.com）和 4–20 位字母/数字/下划线用户名；密码至少 8 个 Unicode 字符、不超过 72 个 UTF-8 字节。注册字段为 email/username/password/code；登录为 account/password；重置为 email/newPassword/code。

在 config.yaml 的 mail.sender_email 填写已验证的 Brevo 发件地址，环境变量 BREVO_API_KEY 提供 API Key；sender_name/app_name 可配置显示名称。未配置邮件时注册状态 enabled=false，发码返回 503；已有新账号仍可密码登录。自动化测试仅使用内存模拟发件器和本地 HTTP 服务，不发送真实邮件。

密码使用 bcrypt cost=10，同进程最多两个并行哈希任务。验证码 10 分钟有效，仅保存 HMAC 摘要，发送成功后生效；消费验证码与账号修改在同一事务中。重置密码增加 token_version，使该账号全部旧 Token 失效。新 Token 使用 cy1 格式，不识别旧版格式，默认 30 天到期；修改 AUTH_SECRET 会注销所有会话并使待用验证码失效。

数据库持久化固定窗口限流：发码每邮箱 1 次/分钟、每 IP 10 次/分钟；登录/验证码提交每标识 10 次/15分钟、每 IP 100 次/15分钟。登录成功返还本次账号额度，IP 额度保留；429 返回 Retry-After。用户名与邮箱登录按各自标识计数。应用只信任 server.trusted_proxies 中的代理提供的 X-Real-IP。

并发消费验证码、唯一键冲突回滚、登录限流和窗口退款等契约已在 SQLite 与 PostgreSQL 上测试。邮件失败返回通用错误；不回显上游响应。账号资料修改、第三方登录和远程歌单同步仍待实现。

## 收藏与本地歌单

收藏：POST /favorites、GET /favorites、DELETE /favorites/:trackId/:source。歌曲字段沿用 trackId/name/artists/album/picUrl/source/sourceIds，sourceIds 保留 fileHash/emixSongId/albumAudioId。重复收藏返回 400；取消不存在的收藏返回 404。

歌单：GET/POST /playlists、GET /playlists/check-track、PUT/DELETE /playlists/:playlistId、POST /playlists/:playlistId/delete；歌曲列表/添加 GET/POST /playlists/:playlistId/tracks，批量添加 POST /playlists/:playlistId/tracks/batch；删除 DELETE /playlists/:playlistId/items/:itemId，批量删除 POST /playlists/:playlistId/items/batch-remove，换源 POST /playlists/:playlistId/items/:itemId/replace，排序 POST /playlists/:playlistId/items/reorder。

所有接口必须提供 Bearer Token，用户 ID 从验证后的会话获取，忽略请求提供的 userId。访问他人的歌单/条目返回 404。GET /playlists 自动确保唯一的“我的收藏”默认本地歌单，该歌单不可改名/删除。旧 favorites 表和默认歌单沿用原项目的独立模型，不自动互相镜像。

创建和歌曲变更必须附带 operationId（最长 128 字节）。记录按用户隔离，成功及业务失败结果保存 24 小时；相同 ID、相同操作范围重放原结果，相同 ID 用于其他范围返回 409。重试同一操作应使用原 ID；更改请求内容或解决业务冲突后应换新 ID。数据库/超时等基础设施失败完整回滚，不留下 pending 记录，可以使用原 ID 重试。改名和删除歌单不要求 operationId，保留旧客户端行为。

单首添加/换源支持 track 对象或平铺字段；批量添加使用 tracks 数组；批量删除/排序使用 itemIds 数组。批量添加按 source+trackId 去重并保持输入顺序，插入歌单头部。重复添加已有条目不增加版本号；换源保留 itemId 和位置，若目标已在歌单中则返回 409。增删成员增加 contentRevision/orderRevision，换源增加 contentRevision，排序增加 orderRevision。排序必须提交完整 itemIds 和 expectedOrderRevision；版本过期或成员不一致返回 409，整笔拒绝，不覆盖其他请求。

POST/PUT/带请求体的 DELETE 支持 JSON 或 URL 编码表单；表单的 track/tracks/itemIds/sourceIds 字段使用 JSON 文本。请求体最大 1 MiB，批量增删最多 500 条，单歌单最多 10000 首，名称最多 200 个字符。排序使用有界 CASE 批次并在同一事务内完成。歌曲目录写锁按来源/歌曲 ID 固定顺序获取，歌单成员与版本写入在同一事务中完成。过期操作记录按分钟、每次最多 100 条清理。

本轮复用现有基线表，无新增迁移。/playlists/follow、/:playlistId/sync、/:playlistId/copy 仍未实现，等待平台接口与远程快照模块。数据库维护/孤立目录清理及真实 NAS 资源测试仍属后续运维工作。

## 本地音源

已实现 GET /lx/sources、GET /lx/script，以及 sync-sources / sources status / sources rollback 命令。来源为用户旧任务确认的 guoyue2010/lxmusic- 最新正式 Release 首个 ZIP；全部文件镜像至本地，公开接口仅提供 JS。详细配置、预算、NAS 计划任务和故障恢复见 [本地音源与 GitHub 同步](04-source-sync.md)。不再需要 sources.manifest_file 或独立 sources.yaml，也不需要 R2 凭据。

```sh
cyapi sync-sources --config /path/config.yaml
cyapi sources status --config /path/config.yaml
cyapi sources rollback --config /path/config.yaml --generation g-...
```

同步/状态/回滚不依赖业务数据库或 AUTH_SECRET；serve 使用相同 sources.directory，发布后自动可见，无需重启。公网下载地址取 server.public_base_url。默认同步预算要求至少 3.5 GiB 可用空间；这不是常驻内存或预分配磁盘占用，可按真实 ZIP 大小调整 max_zip_bytes/max_total_bytes/reserve_bytes。
