# 开发与运行（首批实现）

当前实现：基础服务、配置/日志/健康检查、SQLite/PostgreSQL迁移、账号注册/登录/重置/资料、播放状态和统计、收藏/本地与远程歌单、本地音源分发/同步/回滚、网易云/QQ/酷狗、首页与发现、一致性备份。占位接口显式兼容旧行为；赞助和支付暂缓且不注册；扫码暂缓，旧账号/数据不迁移。实际完成状态与目标环境验收见契约矩阵和实施记录。

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

当前支持的覆盖变量：CYAPI_LISTEN、CYAPI_DATABASE_DRIVER、CYAPI_DATABASE_DSN。`database.dsn_env` 指定读取其他环境变量；CYAPI_DATABASE_DSN 优先。其他配置字段暂不提供通用环境变量映射。

`auth.secret_env` 指向密钥环境变量；migrate/schema-status/version 不读取此密钥。mail 配置已用于 Brevo 验证码邮件；sources 已用于本地音源分发与 Release ZIP 同步，cache 已用于音乐响应缓存，providers 已接入网易云；QQ/酷狗配置为后续模块预留。

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

并发消费验证码、唯一键冲突回滚、登录限流和窗口退款等契约已在 SQLite 与 PostgreSQL 上测试。邮件失败返回通用错误；不回显上游响应。账号资料修改已实现，LinuxDO维持旧未配置契约。

## 收藏与本地歌单

收藏：POST /favorites、GET /favorites、DELETE /favorites/:trackId/:source。歌曲字段沿用 trackId/name/artists/album/picUrl/source/sourceIds，sourceIds 保留 fileHash/emixSongId/albumAudioId。重复收藏返回 400；取消不存在的收藏返回 404。

歌单：GET/POST /playlists、GET /playlists/check-track、PUT/DELETE /playlists/:playlistId、POST /playlists/:playlistId/delete；歌曲列表/添加 GET/POST /playlists/:playlistId/tracks，批量添加 POST /playlists/:playlistId/tracks/batch；删除 DELETE /playlists/:playlistId/items/:itemId，批量删除 POST /playlists/:playlistId/items/batch-remove，换源 POST /playlists/:playlistId/items/:itemId/replace，排序 POST /playlists/:playlistId/items/reorder。

所有接口必须提供 Bearer Token，用户 ID 从验证后的会话获取，忽略请求提供的 userId。访问他人的歌单/条目返回 404。GET /playlists 自动确保唯一的“我的收藏”默认本地歌单，该歌单不可改名/删除。旧 favorites 表和默认歌单沿用原项目的独立模型，不自动互相镜像。

创建和歌曲变更必须附带 operationId（最长 128 字节）。记录按用户隔离，成功及业务失败结果保存 24 小时；相同 ID、相同操作范围重放原结果，相同 ID 用于其他范围返回 409。重试同一操作应使用原 ID；更改请求内容或解决业务冲突后应换新 ID。数据库/超时等基础设施失败完整回滚，不留下 pending 记录，可以使用原 ID 重试。改名和删除歌单不要求 operationId，保留旧客户端行为。

单首添加/换源支持 track 对象或平铺字段；批量添加使用 tracks 数组；批量删除/排序使用 itemIds 数组。批量添加按 source+trackId 去重并保持输入顺序，插入歌单头部。重复添加已有条目不增加版本号；换源保留 itemId 和位置，若目标已在歌单中则返回 409。增删成员增加 contentRevision/orderRevision，换源增加 contentRevision，排序增加 orderRevision。排序必须提交完整 itemIds 和 expectedOrderRevision；版本过期或成员不一致返回 409，整笔拒绝，不覆盖其他请求。

POST/PUT/带请求体的 DELETE 支持 JSON 或 URL 编码表单；表单的 track/tracks/itemIds/sourceIds 字段使用 JSON 文本。请求体最大 1 MiB，批量增删最多 500 条，单歌单最多 10000 首，名称最多 200 个字符。排序使用有界 CASE 批次并在同一事务内完成。歌曲目录写锁按来源/歌曲 ID 固定顺序获取，歌单成员与版本写入在同一事务中完成。过期操作记录按分钟、每次最多 100 条清理。

本轮复用现有基线表，无新增迁移。远程歌单已实现，见下文。数据库维护/孤立目录清理及真实 NAS 资源测试仍属后续运维工作。

## 本地音源

已实现 GET /lx/sources、GET /lx/script，以及 sync-sources / sources status / sources rollback 命令。来源为用户旧任务确认的 guoyue2010/lxmusic- 最新正式 Release 首个 ZIP；全部文件镜像至本地，公开接口仅提供 JS。详细配置、预算、NAS 计划任务和故障恢复见 [本地音源与 GitHub 同步](04-source-sync.md)。不再需要 sources.manifest_file 或独立 sources.yaml，也不需要 R2 凭据。

```sh
cyapi sync-sources --config /path/config.yaml
cyapi sources status --config /path/config.yaml
cyapi sources rollback --config /path/config.yaml --generation g-...
```

同步/状态/回滚不依赖业务数据库或 AUTH_SECRET；serve 使用相同 sources.directory，发布后自动可见，无需重启。音源下载地址跟随当前请求地址；HTTPS 终止于代理时，由可信代理传递 X-Forwarded-Proto。默认同步预算要求至少 3.5 GiB 可用空间；这不是常驻内存或预分配磁盘占用，可按真实 ZIP 大小调整 max_zip_bytes/max_total_bytes/reserve_bytes。

## 网易云音乐（首批）

已实现 POST /search、POST /song、GET /lyrics/netease、GET /playlist、GET /album，全部要求新系统 Bearer Token。POST 支持 JSON 或 URL 编码表单，最大 16 KiB；GET 重复参数拒绝。服务只请求固定网易云端点，不直接请求用户提供的 URL；只接受正整数 ID 或 music.163.com 的带 id 链接（包括 #/song?id=...），短链接暂不解析。

- /search：keywords 必填，最长 200 个字符；page 默认1、范围1–1000；limit 默认20、范围1–50。保留 status/result/page/resultKind/hasMore/pageFingerprint，指纹为原始 ID 数组 JSON 的 MD5。明确空结果返回空数组/null 指纹；上游失败返回 SEARCH_UPSTREAM_FAILED(502)，超时 SEARCH_TIMEOUT(504)，不伪装空成功。
- /song：ids 或 url，level 和 type 必填。音质支持 standard/exhigh/lossless/hires/sky/jyeffect/jymaster；type=down 返回302，text 返回转义后的旧格式 HTML，其他非空 type 返回旧 JSON 字段。eapi AES 请求与旧实现的独立 Node 样本一致，不绕过平台账号的播放权限。平台返回无可用地址时为422 SONG_UNAVAILABLE；其他上游失败502/超时504，无原始异常回显。返回媒体地址只接受网易云音乐/CDN HTTP(S) 域名，输出升级 HTTPS，不代理音频。
- /lyrics/netease：id/ids/url 均可，保留 ready/empty/unavailable 与 retryable；仅明确 nolyric 才确认 empty。上游401/403映射502，不误报为应用会话失效；上游429保留429及 Retry-After。正文为空但未确认无歌词时返回 unavailable 且不缓存。
- /playlist：id 必填，limit 可省略（完整列表，上限10000）或0–10000。细节按100首分批读取，再按 trackIds 恢复顺序及重复项；sourcePosition 不因缺失详情而压缩。返回 expectedTrackCount/resolvedTrackCount/complete，部分失败仍按旧接口返回 complete=false，禁止缓存或将其视为完整同步快照。平台响应的歌单 ID 必须与请求一致。
- /album：id 必填，保留 status/success/data.album 及歌曲元数据；平台返回专辑 ID 必须匹配。

平台 Cookie 仅从 providers.netease_cookie_env（默认 NETEASE_COOKIE）读取，不接受客户端传入的 Cookie，不回显/记录。未配置时使用匿名访问，部分曲目/歌单可能受平台登录、版权和出口网络限制；旧平台账号绑定尚未接入。设置 Cookie 后音乐缓存按应用用户隔离；无 Cookie 时允许复用同一进程内的公开查询。HTTP 响应始终 private/no-store，不暴露上游 Cookie/Set-Cookie。

复用 cache.max_bytes（默认16 MiB）、max_entries（2000）与 max_item_bytes（1 MiB）限制缓存；权重包含响应字节、键和固定元数据预算，不等于整个 Go 进程内存上限。LRU 淘汰、TTL 失效；超大成功响应照常返回但不缓存。搜索 TTL 30秒，歌词 ready 10分钟/confirmed empty 5分钟，完整歌单10分钟，专辑15分钟；临时播放地址仅在平台明确提供剩余有效期时缓存至多30秒，并预留5秒余量。失败、模糊空歌词、未完成歌单和无有效期地址不缓存。

相同缓存键的并发请求合并，最多32个不同的共享任务；最后一个等待者取消时终止对应上游任务，单一等待者取消不影响其他人。Cache-Control: no-cache/no-store 绕过读取与写入；X-Cache 返回 HIT/MISS/COALESCED/BYPASS。独立 Provider HTTP 并发默认4（providers.max_concurrent，1–16），单次请求默认8秒（request_timeout，1–20秒），响应解压后最大8 MiB，不自动重试或跟随重定向，避免 Cookie 泄露及额外请求放大。

每应用用户默认120请求/分钟，每可信解析 IP 默认300请求/分钟（providers.user_requests_per_minute、ip_requests_per_minute）；缓存命中同样计数，429包含 Retry-After。状态仅在内存，最多4096个限流键，重启重置；账号认证限流仍使用数据库。业务总预算为搜索12秒、歌词7秒、歌曲15秒、歌单/专辑20秒；网易云路由显式使用25秒业务鉴权包装，账号查验独立限制5秒，避免将旧通用5秒预算误套到平台请求。

QQ/酷狗已接入，远程歌单支持网易云和QQ。

## 远程歌单关注、同步和复制

三个接口均要求新系统 Bearer Token，支持 JSON/表单、最大1 MiB，必填 operationId（最多128字节）。仅操作当前用户拥有的歌单；跨用户返回404，客户端 userId 不参与授权。名称上限200字符。

| 接口 | 请求字段 | 结果 |
| --- | --- | --- |
| POST /playlists/follow | source=netease或qq、sourcePlaylistId（正整数ID）、operationId、可选name | 取得完整来源后创建只读 synced 歌单；同用户同来源重复关注409 SOURCE_ALREADY_FOLLOWED |
| POST /playlists/{playlistId}/sync | operationId | 重新读取平台，不经过查询缓存；发布成功返回 playlist，contentRevision 加1 |
| POST /playlists/{playlistId}/copy | operationId、可选name | 将最近一次完整快照复制为可编辑 local 歌单；返回 playlist 和 copiedCount，无需访问平台 |

关注和同步必须取得 complete=true、数量完全匹配、最多10000条且顺序连续的来源快照，保留来源顺序和重复曲目。不完整、无效数据或上游失败返回422 SOURCE_SNAPSHOT_INCOMPLETE；错误内容脱敏。关注失败不会留下空订阅；同步失败设置 syncStatus=error，保留活动快照、contentRevision、lastSyncedAt。空且完整的来源合法。已有歌单的手动名称在同步时保留。

拉取发生在数据库事务外，进程最多4个远程任务，满载返回429 SOURCE_BUSY；同一网易云客户端的请求并发仍受 providers.max_concurrent 限制。远程拉取预算20秒，提交/失败落库使用独立5秒预算，取消请求后也尝试释放同步状态。请求处理未启动后台常驻定时同步：需要刷新时调用 sync。

operationId 按用户/操作作用域记录：成功和已落库的业务失败重放24小时；重放不访问上游，同ID换用途409 OPERATION_SCOPE_MISMATCH。失败结果需要主动重试时使用新的 operationId。进行中同ID返回409 OPERATION_PENDING；同歌单不同操作ID同步返回409 SYNC_IN_PROGRESS。数据库写入异常回滚后尝试移除本次 pending 记录，允许原ID重试。进程异常退出后 pending/同步状态可在5分钟后重新获取；同步版本、绑定版本及操作起始标识共同阻止旧任务覆盖新任务或复活已删除歌单。数据库本身不可用时清理可能延迟至恢复后的租期检查。

目录、快照条目、活动指针、版本和操作结果在事务内原子提交，目录锁按来源/歌曲ID排序，批量写入每100条。保留当前快照和上一份历史快照；更早快照随成功同步删除。操作记录复用按分钟、每次最多100条的过期清理。

同步歌单继续只读；复制使用已有完整快照，即使最近同步失败也可复制。副本按第一次出现的顺序去重（本地歌单不允许重复曲目），保留歌曲来源与来源标识，初始内容/排序版本均为1。副本后续修改独立于远程歌单成员变化，歌曲元数据仍复用全局目录。没有完整快照时复制409；本地歌单 sync/copy 返回409。

示例请求：

```json
{"source":"netease","sourcePlaylistId":"123456","operationId":"follow-001"}
```

本轮无新增依赖和数据库迁移，未启用旧账号迁移或扫码登录。真实平台、Cookie、NAS/frp 的可用性和资源占用仍需目标环境联调。

## 集中补齐模块

- 播放状态：POST /playback/save、GET /playback/last、DELETE /playback/clear。position为0–604800整数秒；同曲目/来源且元数据相同、4秒内变化小于3秒的重复保存由数据库条件合并。清除后可以立即保存，不残留内存写入屏障。
- 统计：POST /stats/listening-time（每次0<seconds<=86400，可小数）、POST /stats/play-count；GET /stats返回全部累计总量及前10；/stats/records页码1–100000、每页1–50，稳定排名和limit+1判断hasMore；/stats/song-memory查询当前用户曲目记录。数据库原子累加，私有no-store，不从客户端userId选择账号。
- 账号：POST /auth/update-username保持4–20位字母数字下划线与唯一约束；POST /auth/update-location校验IP与location；GET /accounts/bindings在无平台绑定创建能力时返回未绑定状态，服务器Cookie不等于用户绑定；两个unbind仅删除当前用户对应绑定，不回显凭据。
- QQ：GET /qq/search、/qq/song、/qq/playlist与/lyrics/qq。搜索初始页完整查询失败/空结果时串行回退建议，后续页不回退；保留分页/MD5/hasMore，建议结果无下一页。歌词通过歌曲详情再取base64文本；歌曲返回三档可用地址，缺少地址不伪造，地址只接受腾讯音乐域名并升级HTTPS。完整歌单可关注与同步，保留QQ字符串mid和重复项；上游当前单次最多1000项，未读全时complete=false且禁止用于快照。
- 酷狗：GET /kugou/search、/kugou/song、/lyrics/kugou。沿用旧签名盐与按键排序的MD5协议；歌曲/歌词需要KUGOU_COOKIE包含mid/kg_mid、dfid/kg_dfid、KugooID、token/t。请求凭据仅在固定上游域名发送，异常不回显；播放地址只允许酷狗媒体域名。
- 首页发现：GET /toplists、/netease/top/playlist、/netease/playlist/highquality/tags、POST /artist/search、GET /v1/home/charts、/v1/discover/playlists、/v1/discover/tags。保留四榜顺序、部分结果标志、分类与base64url分页游标。榜单目前不缓存部分组装结果；普通列表/标签5分钟、搜索30秒、明确歌词10分钟；QQ/酷狗临时歌曲地址不缓存。所有新增平台查询复用音乐服务的同一LRU容量和用户/IP限流；缓存按应用用户隔离，Cookie不进入缓存键。每个平台独立限制HTTP并发，三个平台合计可能达到各自上限之和。
- 旧固定响应：推荐/FM、用户平台歌单、部分艺人详情/百科、酷我/Apple、天气、音乐品味、LinuxDO等维持旧占位/未配置语义，静态接口标注X-Compatibility-Mode: placeholder。兼容注册不代表真实功能；通用媒体代理仍501，赞助/支付及扫码404。
- 备份：`cyapi backup --config /absolute/config.yaml --output /absolute/new-backup-directory`，见 [部署与恢复](09-deploy-and-restore.md)。默认SQLite无需额外服务；PostgreSQL备份需匹配的pg_dump，恢复需pg_restore。默认不复制配置与秘密；外部计划任务管理保留周期和异机副本。

以上为集中代码实现；本地模拟验证、真实上游可用性、NAS资源、frp链路和运维演练分别记录。旧阶段章节中的历史描述不作为最新状态，最新范围以契约矩阵为准。
