# NAS 原生安装与运维

这套安装方式无需 Docker、Go 运行时、外部数据库或 crontab。下载 NAS CPU 对应的 Linux 二进制，改名为 cyapi。仅支持 Linux amd64/arm64 的自动服务安装；Windows 可测试 install --service none、帮助和诊断，但不注册 Windows 服务。备份/音源/账号逻辑复用原实现。

## 首次安装

```sh
mkdir -p /vol1/appdata/cyapi
cd /vol1/appdata/cyapi
chmod +x ./cyapi
sudo ./cyapi install
```

默认使用执行时的当前目录；也可 --dir 指定目录。安装将目录转成绝对路径，生成 config.yaml、secrets.env、data/、backups/、logs/ 和 start.sh。配置/密钥仅在缺失时创建，已有数据库却缺少原密钥时拒绝生成替代密钥。已有配置不被命令行 listen 参数覆盖；这些参数只设置新配置，请修改已有配置后重启服务。

首个账号交互输入用户名、QQ 邮箱和两次隐藏密码，不发送邮件；账号与公开注册采用相同字段校验。本地创建仅限 users 表为空，不提供公网免验证码入口。无人值守使用 --non-interactive --username NAME --email ADDRESS，密码仅从 CYAPI_INSTALL_PASSWORD 环境读取，不接受命令行密码参数，也不保存明文密码。未配置邮件时可用首个账号登录，公开注册/找回密码保持关闭。

Linux 有运行中的 systemd 且以 root 安装时，创建 cyapi 普通用户（也可 --user 已有普通用户），注册以安装路径摘要命名的服务，开机启动和失败自动重启。应用不以 root 运行。只调整安装目录内受管文件的权限，不递归改变父目录权限。路径位于 NAS 的 ACL 管理目录时，应确保服务用户可以穿过上级目录；健康检查失败会提示检查日志。

没有 systemd 或暂不注册服务时：

```sh
./cyapi install --service none
./start.sh
```

此模式明确报告“未配置开机启动”；需要 NAS 任务管理器调用 start.sh 并配置重启。不要把当前 shell 的后台进程当作开机服务。非 root 的 auto 模式也只生成文件/脚本，不假装注册成功。已有 frpc 容器的网络路径需要按实际拓扑配置，install 不自动修改 frp、VPS、防火墙或 DNS。

默认监听 127.0.0.1:8080，适合同机 frpc。手机局域网测试可在新安装时传 --listen 0.0.0.0:8080。公网入口使用 HTTPS。填好邮件发送人、Brevo API key 和必要平台 Cookie 后重启服务生效；secrets.env 使用字面量 KEY=VALUE，不执行 shell 或变量展开，环境变量优先。密钥应稳定保存，不要把测试机器的账号数据库直接当作生产安装。

音源下载链接自动跟随客户端访问地址，无需设置公网 URL。反向代理应保留 Host；HTTPS 在代理终止时，覆盖 X-Forwarded-Proto 为 https。如需转发外部域名，覆盖 X-Forwarded-Host。仅信任 server.trusted_proxies 中的实际直接代理地址，不默认信任本机或任意私网。

从 v0.0.1 升级前，删除已有 config.yaml 中的 server.public_base_url 字段；配置解析仍严格拒绝未知字段。安装参数 --public-url 和环境变量 CYAPI_PUBLIC_BASE_URL 已移除。

## 帮助、状态和诊断

```sh
./cyapi
./cyapi --help
./cyapi help install
./cyapi install --help
./cyapi --version
./cyapi status
./cyapi doctor
./cyapi doctor --network
```

帮助/版本无需配置、数据库或密钥。doctor 只读检查配置、密钥长度、目录权限、数据库 schema、音源完整性、服务及维护状态，不创建缺失数据库或发送测试邮件。--network 才额外检查 GitHub。Windows 的权限诊断仅检查目录存在，真实 ACL 写入能力由启动验证；Linux 使用实际目录访问权限检查。

FAIL 返回非零退出码；WARN 说明功能受限，例如缺少邮件/Cookie/音源。status 检查当前 API readiness 和最近定时任务结果，API 不就绪返回非零。doctor 不强制服务已运行，因此能在故障停机时诊断。

## 自动维护

install 生成的配置启用 maintenance；旧配置和示例默认关闭，避免升级后无意启动后台网络/备份工作。可配置同步周期（默认 6h）、备份周期（默认 24h）、备份目录及保留数量（默认 7）。

服务启动后后台执行首次音源同步和首次备份，之后按持久化的上次成功时间运行。任务串行、不重叠，失败至少间隔 5 分钟再重试。停机时取消并等待任务结束，不让后台数据库操作在连接关闭后继续。外网失败不阻塞 HTTP 启动；已有音源继续服务，没有音源时列表暂不可用。

自动备份包含数据库、当前音源、配置和 secrets.env，并记录恢复文件校验值。只在成功发布新备份后清理 auto- 前缀、合法命名且具有备份 manifest 的旧目录，不清理手动备份或 upgrade- 备份，不跟随符号链接。凭据仅从进程环境提供时不会出现在恢复包，需独立保存。SQLite 必须放本机文件系统，不使用 SMB/NFS 文件路径。

任务结果位于 data/maintenance.json；正常日志位于 logs/server.log，按 10 MiB 轮转，最多当前文件加 3 份历史。stdout/stderr 中的启动前错误由系统服务日志捕获。音源复用原版本回收策略，保留当前/最近版本及短期读者租约。

## 升级

```sh
sudo ./cyapi upgrade --file ./cyapi-new --sha256 发布方提供的SHA256
```

仅管理本程序注册的 Linux systemd 服务，不在线下载可执行文件。先在安装目录暂存候选文件，校验摘要、版本和目标架构，再停止服务；使用旧程序生成 upgrade- 恢复备份，使用候选程序执行显式 migrate，保存带时间戳的 previous 程序，替换并启动健康检查。备份包含受限配置和密钥。建议始终提供发布方摘要。

普通 serve 永远不自动迁移 schema。备份失败不更新程序/数据库，并尝试恢复旧服务；进入迁移阶段后任何失败都保持服务停止，不自动让旧程序连接可能已升级的数据库。升级状态记录在 data/upgrade.json，日志给出备份路径。保留的 previous 程序和 upgrade- 备份不自动清理，验收成功后由管理员归档。

非 systemd 环境按手动流程：停止 API 和外部任务、用旧程序备份、新程序 migrate、替换并启动、检查 ready。schema 不兼容时不能直接回退旧二进制。

## 故障恢复

1. 停止 systemd 服务（名称可从 status 或 data/install.json 获取）和维护任务，保留故障现场。
2. 校验备份 manifest 中 DatabaseSHA256 和 RecoveryFiles；音源文件按各代 manifest 的 SHA256 校验。
3. 将 database.sqlite 恢复到全新的数据目录，sources 恢复到对应新目录；不覆盖运行中的 SQLite/WAL 文件。
4. 使用恢复包里的配置与原密钥，将 SQLite、音源和 runtime_directory 改为新目录的绝对路径，确认程序版本与备份 schema 匹配。
5. 使用保留的 previous 程序执行 schema-status，再前台 serve 验证登录/收藏/歌单/音源，随后更新系统服务指向并启动。
6. PostgreSQL 使用匹配版本的 pg_restore 恢复到新建空库，不覆盖原数据库；pg_dump/pg_restore 需要自行安装。

同盘备份不能防止磁盘损坏，请由 NAS 备份工具另做异机副本。安装、备份、状态和升级流程有本机自动测试；目标 NAS 的 systemd/ACL、隐藏输入终端、真实断电、frp/HTTPS 和 PostgreSQL dump/restore 仍需目标环境验收。
