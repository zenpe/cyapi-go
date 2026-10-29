# NAS、frp、配置和运维

## 1. 部署边界

VPS 上 Caddy/Nginx 终结 HTTPS，转发到仅监听 VPS 回环地址的 frps 业务转发端口；NAS frpc 连接 frps，再转发到 NAS Go 服务。必须用实际 frp 版本支持的配置限制业务代理监听地址，不能误将 remotePort 开放在公网所有网卡。

frps 通信端口按需要开放，配置认证与隧道 TLS/证书校验；监控/管理端口不公开。Go 服务只对 frpc 所在网络开放，NAS 管理入口不通过此业务隧道发布。HTTPS 与 frp 隧道加密是两段独立链路，不能只配其中一段。

选择同地域/合适线路 VPS；评估 NAS 上行、VPS 出口和流量套餐。普通音乐解析返回平台 URL，首期没有媒体代理；音源下载仍消耗 NAS 上行。平台请求默认走 NAS 出口，frp 不自动将其改为 VPS 出口。

## 2. 原生与容器

优先交付 linux/amd64 与 linux/arm64 的二进制，按真实 NAS 选择；32 位 ARM 不承诺支持，需额外验证驱动和工具链。原生运行交给 NAS 服务管理或 systemd；容器使用非 root 用户、持久化 data 卷和 restart 策略。两种方式保持相同配置语义。

最小镜像仍需 CA 证书以访问 HTTPS；纯 Go 不代表不需要系统证书。CGO 产物必须验证 libc/动态依赖，不能将开发机可执行当作 NAS 通用产物。不复制 Node、Wrangler、编译器或旧 .dev.vars 到发行包。

原生部署示例布局：

```text
/volume1/apps/cyapi/
  cyapi                  # Linux 可执行文件
  config.yaml            # 非秘密配置，可引用环境变量名
  data/
    cyapi.db
    sources/             # 版本目录与指针，详见音源文档
  backups/               # 本地快速恢复，另有异机副本
```

Compose 镜像内仅包含程序和运行必需文件，宿主 data 挂载至 /data；升级不能删除卷。SQLite 文件使用本地文件系统。日志默认 stdout，由系统/容器轮转。

## 3. 配置契约（实际配置见 configs/config.example.yaml）

```yaml
server:
  listen: "127.0.0.1:8080"       # 容器内按网络模式改为适当地址
  public_base_url: "https://api.example.com"
  trusted_proxies: []           # 填入实际可信的直接代理地址/CIDR
  cors_allowed_origins: []      # 按实际 Web 客户端填写
  shutdown_grace: "30s"
database:
  driver: "sqlite"              # 可选 postgres
  dsn: "data/cyapi.db"          # postgres 使用 dsn_env 指定环境变量
  dsn_env: ""
  busy_timeout: "5s"
auth:
  secret_env: "CYAPI_AUTH_SECRET"
  token_max_age: "720h"
cache:
  max_bytes: 16777216
  max_entries: 2000
  max_item_bytes: 1048576
sources:
  directory: "data/sources"
  repo: "guoyue2010/lxmusic-" # 最新正式 Release 的第一个 ZIP
  github_token_env: "GITHUB_TOKEN"
  allow_empty: false
mail:
  provider: "brevo"
  api_key_env: "BREVO_API_KEY"
  sender_email: ""              # 部署时填写已验证发信人
providers:
  netease_cookie_env: "NETEASE_COOKIE"
  qq_cookie_env: "QQ_COOKIE"
  kugou_cookie_env: "KUGOU_COOKIE"
```

相对路径统一以配置文件所在目录解析，不依赖进程工作目录。`*_env` 明确表示读取指定环境变量，不做任意 shell 展开。无 AUTH_SECRET 时 serve 拒绝启动；邮箱未配置时发信相关功能明确不可用，不伪造成功；sync-sources 不要求业务鉴权密钥。

配置优先级定义为显式 CLI > 专用 CYAPI_* 覆盖 > 配置文件 > 默认值；秘密只从明确的秘密来源加载，日志展示脱敏结果。配置校验错误要在启动时返回清楚原因。

旧 CACHE_TTL_* 的每路由值需从 wrangler.toml 映射到新配置并保持初始行为；LOG_LEVEL、APP_NAME、Token 生命周期、Brevo 发件配置及平台 Cookie 逐项核对。LX_SOURCE_PREFIX 属于旧 R2 语义，不直接当作 NAS 绝对路径。

## 4. 真实 IP、域名与代理

公网入口必须覆盖而不是盲目保留客户端 X-Forwarded-For/X-Real-IP/CF-Connecting-IP。定义一个经验证的真实 IP 传递契约；后端仅当直接连接来自 trusted_proxies 时接受该契约。若有 CDN 或多层反代，逐跳配置可信范围；不默认信任任意私网段。

frp TCP 隧道不会自动生成正确的 HTTP 原始客户端 IP。必须测试两个公网客户端分属不同限流桶，以及伪造请求头不能切换限流身份。禁止公网绕过入口直连 frps 业务转发端口。

外部绝对 URL 优先使用固定 public_base_url，不依赖任意 Host。保留 Authorization 和必要查询参数；不记录二维码 query。反向代理只对明确白名单内容缓存，不绕过后端鉴权缓存用户请求。

普通 API 超时大于已定义的业务总预算；验证客户端解析和音源下载。首期不开放媒体代理，不能只用根路径 200 判定业务部署成功。

## 5. 管理命令（已实现，实用部署步骤见09文档）

| 命令 | 行为 |
| --- | --- |
| cyapi serve | 校验配置/schema，启动 API |
| cyapi migrate | 显式执行当前驱动的版本迁移 |
| cyapi sync-sources | GitHub 同步并原子发布 |
| cyapi sources status / rollback | 查看和切换音源版本 |
| cyapi backup | 一致性备份并生成版本/校验元数据 |
| cyapi version | 显示构建版本与 commit |

系统服务自动重启、开机启动；NAS 计划任务运行音源同步和定期备份。同一二进制子命令不意味着一个常驻调度守护进程。

## 6. 备份、监测与升级

建议初始 RPO 为 24 小时、RTO 为 1 小时，属于待用户按业务确认的目标，不是已经达到的保证。备份每天一次、升级前额外一次，至少保留最近 7 个日备份；保留一份异机/异盘副本。包含数据库、音源 manifest/可恢复版本和配置，秘密单独受限保存。

监测进程、磁盘余量、数据库大小/WAL、Busy/慢查询、请求 P95/错误、平台超时、frp 重连、音源最后成功时间。初期用结构化日志和本地状态命令即可，不要求额外部署监控集群。

升级必须使用新版本二进制所嵌入的迁移，按以下顺序执行：

1. 将新发布包解压到独立的 `releases/<version>/`，核对摘要、目标架构和 `version` 输出，保留当前程序路径；此时不切换运行入口。
2. 检查版本迁移兼容说明，暂停会写共享数据的定时任务；进入维护模式、停止 API 并等待在途写结束。创建一致性备份，记录旧程序版本、旧 schema 版本、配置和数据位置，确认可恢复。
3. 通过新程序的明确绝对路径执行迁移，例如 `/volume1/apps/cyapi/releases/<new-version>/cyapi migrate --config /volume1/apps/cyapi/config.yaml`。不得调用 PATH 中或当前运行链接指向的旧 cyapi。容器方式使用明确的新镜像版本/摘要运行一次性 migrate 任务，并挂载同一持久化数据与配置。
4. 迁移成功并校验目标 schema/校验和后，切换服务执行路径或原子更新 current 链接；容器更新为同一个已验证新镜像。启动新服务，执行就绪与关键流程验证，通过后恢复入口和计划任务。
5. 迁移或启动失败时保持维护状态并保存错误。只有旧程序明确兼容当前 schema 时才能直接切回；否则恢复升级前的一致性备份到受控目标，并核验后启动旧程序。禁止将旧程序直接连到未知或部分迁移的新 schema。开放写入后的回滚需处理新写入，见下一节。

以上命令已接入，部署与恢复操作见09-deploy-and-restore.md。原生部署和容器部署都需演练至少一次包含新增迁移的升级、一次迁移失败以及一次启动失败恢复。

## 7. 全新账号上线与入口切换

1. 创建独立空库，使用新版本运行 migrate，核对 schema；生成新 AUTH_SECRET，不复制旧账号数据或旧密钥。扫码路由不提供。
2. 测试新注册、密码登录、找回密码、Token 撤销、用户隔离、歌单与统计，再验证音源首次同步、NAS 出口和 frp 链路。
3. 明确告知用户需要重新注册，旧账号及收藏/歌单/统计不自动继承；客户端隐藏扫码登录入口。
4. 约定入口切换窗口，切换域名或客户端后端地址并处理 DNS TTL/旧连接。若旧系统继续运行，应明确其是独立账号/数据，禁止暗示两边自动同步。
5. 放开新系统注册与写入前完成一致性备份恢复演练。放开后若回切旧入口，新系统账号和新写入仍保留在新库，但不会出现在旧系统；先冻结写入、保留新库备份并明确用户影响，不能承诺透明回滚或丢弃新账号数据。
6. 观察期建议至少 7 天，覆盖备份恢复、音源更新和重启；旧系统数据保留，Cloudflare 任务停用/资源归档或删除另行安排。

首期没有 D1 导入器、旧账号映射或旧凭据兼容。旧系统数据不作为迁移源，也不因启用 Go 服务被删除。
