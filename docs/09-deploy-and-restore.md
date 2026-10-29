# 部署与恢复操作

当前默认 SQLite 单实例。以下为待目标环境验证的可执行操作说明；未自动连接 NAS、发送真实邮件或切换线上入口。赞助与支付暂缓；扫码、旧账号导入及已排除平台继续不实现。

## 二进制部署

执行 scripts/release.sh 或 scripts/release.ps1 生成带版本/commit 的 Linux amd64、arm64 和 Windows amd64 产物及 SHA256SUMS。选择 NAS CPU 对应产物，校验摘要。生产采用 configs/config.example.yaml，设置绝对 database.dsn、sources.directory、public_base_url 和独立 CYAPI_AUTH_SECRET。以同一非特权用户运行 serve 和计划任务，数据库与 sources 目录只允许该用户写。

顺序：先 `cyapi migrate --config /absolute/config.yaml`，再 `cyapi serve --config /absolute/config.yaml`。systemd 可参考 packaging/cyapi.service，调整用户、二进制、数据和配置路径。没有 systemd 的 NAS 使用自带任务/应用管理器运行同样命令；选择异常自动重启和开机启动。未知 schema 或未迁移时启动失败是正常保护。

NAS 定时任务运行 `cyapi sync-sources --config /absolute/config.yaml`，沿用原任务周期；停止旧 R2 上传任务应在新客户端流程验证后由操作人员执行。不要让旧 Python 上传任务同时写新的 sources 目录。

## 容器

packaging/compose.yaml 已提供构建、数据/备份卷、日志轮转和非特权运行配置。复制 packaging/config.container.example.yaml 为同目录 config.yaml、.env.example 为 .env，填写新密钥及已验证邮箱发件人。按镜像 UID/GID 10001 准备 data/backups 的写权限，并限制 .env 读取权限。

```sh
docker compose -f packaging/compose.yaml build
docker compose -f packaging/compose.yaml run --rm cyapi migrate --config /config/config.yaml
docker compose -f packaging/compose.yaml up -d
docker compose -f packaging/compose.yaml run --rm cyapi sync-sources --config /config/config.yaml
docker compose -f packaging/compose.yaml run --rm cyapi backup --config /config/config.yaml --output /backups/backup-20261029
```

容器只向宿主回环地址映射8080；容器内127.0.0.1不代表NAS宿主。如果 frpc 运行于其他容器，需通过专用 Docker 网络连接 cyapi:8080 并调整实际代理信任配置，不暴露公网监听。

## frp 与 HTTPS

packaging/frpc.example.toml、frps.example.toml、Caddyfile.example 分别为NAS、VPS和HTTPS入口模板。填写实际VPS/域名，FRP_TOKEN 通过进程环境提供；frps 转发端口仅绑定VPS回环地址，不向公网开放18080。Caddy覆盖 X-Real-IP，后端仅信任实际直接代理地址，不信任任意私网或用户原始转发头。

原生同机 frpc 可设置 trusted_proxies 为127.0.0.1/32（有IPv6时加::1/128）；Docker桥接场景先确认实际直连地址，不能照抄。frp版本需支持模板中的TOML/环境变量模板和TLS选项。实际验收必须包含两个公网出口分别限流、伪造X-Real-IP无效、Authorization和中文音源路径正常。

## 备份

```sh
cyapi backup --config /absolute/config.yaml --output /absolute/backups/backup-20261029
```

output 必须是尚不存在的目录。SQLite 使用 VACUUM INTO 得到一致性数据库副本并执行 integrity_check，不直接复制活跃WAL数据库。PostgreSQL使用外部 pg_dump（版本与数据库匹配，置于PATH），自定义格式，无owner/ACL。PostgreSQL备份DSN必须为postgres:// URL，密码仅通过子进程环境传入，不在命令行或结果中展示；支持sslmode、证书路径和search_path，其他DSN选项明确拒绝。

备份先写同目录临时目录，完整后重命名发布，包含 database.sqlite/database.dump、SHA256、程序版本、commit、驱动、当前音源完整版本及其manifest和current.json。持有音源读取租约，逐文件校验后复制，损坏时不发布备份。尚无音源版本时manifest明确没有音源，已有指针损坏则失败。数据库与音源分别一致，不承诺跨系统同一时刻事务。

配置文件、环境密钥不自动复制，需另行受限备份；不能把.env连同普通下载文件公开。每日及升级前运行备份，外部NAS任务负责保留策略和异机副本。服务用户只应访问自己控制的备份父目录。

## 恢复与升级

先停止API和同步任务，校验备份manifest的数据库SHA256及音源文件校验值。恢复到全新空目录，保留现有数据目录，不覆盖运行中的SQLite文件。

SQLite将database.sqlite复制到新的配置指定文件路径；PostgreSQL用匹配的pg_restore恢复database.dump到新建空库，按目标环境设置所有者，不恢复到生产原库。音源恢复备份内sources整个目录；配置使用独立备份并检查sources.directory、database.dsn和密钥。数据库恢复不负责账号迁移或自动搬迁到其他数据库类型。

选择与备份schema兼容的程序，执行schema-status；升级时先备份、再由新二进制migrate。成功后启动并验证ready、登录、收藏、歌单、播放状态、统计和音源下载，再切换入口。迁移失败时保持旧数据和旧入口，恢复备份到独立目标；禁止旧程序直接连接未知新schema。

备份/恢复、容器运行、升级失败恢复、真实邮件和NAS资源压测仍需在目标环境验收；提供脚本和本机测试不能替代这些结果。
