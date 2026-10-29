# 本地音源与 GitHub Release 同步（已实现）

## 来源与旧任务对应关系

来源由用户提供的 NAS `lxmusic-r2/sync.py` 确认：默认仓库 `guoyue2010/lxmusic-`，读取 Releases API 前 20 条，按 API 顺序跳过 draft/prerelease，选择最新正式 Release 中第一个名称以 .zip 结尾的附件。不是 Git 分支/tree 扫描，不回退到较旧 Release 的 ZIP。没有正式版本或最新正式版本没有 ZIP 时明确失败，保留已有版本。

镜像 ZIP 内全部普通文件，不只同步 JS；公开接口仍只列出/下载 .js（忽略扩展名大小写）。修复常见 GBK/GB18030 中文文件名，统一 ZIP 反斜杠分隔符，处理无害 ./ 和重复分隔符。所有文件都有相同顶层目录时去掉该目录；根目录单文件保留文件名，修复旧脚本单文件被错误裁切的问题。

不读取旧 R2 env 文件，不依赖 boto3/requests、R2 SDK、Cloudflare 凭据或 Python。可选 GITHUB_TOKEN 仅用于 GitHub API 限额；仓库/附件按公开仓库流程访问。本次未在真实 GitHub 最新附件或用户 NAS 上联调。

## 命令与配置

```sh
cyapi sync-sources --config /path/config.yaml
cyapi sync-sources --config /path/config.yaml --dry-run
cyapi sync-sources --config /path/config.yaml --force
cyapi sources status --config /path/config.yaml
cyapi sources rollback --config /path/config.yaml --generation g-...
```

这些命令不打开业务数据库，不要求 AUTH_SECRET。普通失败退出码非零，避免旧脚本捕获异常后仍 exit 0 使 NAS 任务误判成功；已被其他进程持锁的 sync-sources 记录 skipped 并 exit 0。成功输出 JSON，含 generation/tag/files/unchanged/dryRun。status 流式验证当前全部文件；rollback 校验指定版本后切换。下一次同步仍会按最新 Release 更新，手工回滚不会永久钉住旧版本。

```yaml
server:
  public_base_url: "https://music.example.com"
sources:
  directory: "/vol1/appdata/cyapi/data/sources"
  repo: "guoyue2010/lxmusic-"
  github_token_env: "GITHUB_TOKEN"
  allow_empty: false
  max_zip_bytes: 536870912       # 512 MiB，沿用旧脚本
  max_file_bytes: 67108864      # 64 MiB，新增单文件预算，可调
  max_total_bytes: 2147483648   # 解压文件合计 2 GiB，沿用旧脚本
  max_files: 10000              # 沿用旧脚本
  reserve_bytes: 1073741824     # 预留 1 GiB，可按 NAS 调整
  timeout: "10m"
```

仓库直接配置在 config.yaml；先前预留且从未实现的 sources.manifest_file/sources.yaml 不再使用，旧示例配置需删除该字段。相对目录以 config.yaml 所在目录解析。支持 Linux amd64/arm64 与 Windows；NAS 建议服务和计划任务使用相同用户，音源目录为该用户独占读写，避免向其他容器开放写权限。

## 发布目录与一致性

```text
sources/
  sync.lock
  current.json
  staging/
    attempt-<random>/
      download.zip
      files/...
  versions/
    g-<timestamp>-<random>/
      manifest.json
      .read.lock
      files/...
```

全程持有 OS 文件锁；进程退出/崩溃由内核释放锁。先回收上次崩溃留下的 staging/attempt-*；不识别的文件、目录链接或清理失败会停止本轮同步，不开始新下载。正常失败/取消清理本次 attempt。staging/versions/current.json 必须位于同一文件系统。

每轮重新下载并校验 ZIP，与原任务每轮 reconcile 的原则一致，不仅依赖本地 state 或 HTTP 304。流式下载/解压，校验附件大小、GitHub 提供的 SHA-256（如有）、ZIP CRC、单文件/总量/数量预算；拒绝父路径、绝对路径、链接、特殊文件、跨平台非法文件名，以及修复/大小写/Unicode 规范化后的重名和文件/目录冲突。上游 bytes 原样保留，不执行脚本。

manifest 记录仓库、Release ID、asset ID/更新时间、ZIP 摘要、同步规则摘要、发布时间，以及所有文件的路径/大小/SHA-256。密钥和 Token 不参与同步配置摘要，不落入 manifest。输出集合、元数据、配置一致且当前版本文件集合/大小/摘要均验证通过时报告 unchanged；文件缺失/篡改、manifest/current 损坏或多出文件会在新 generation 中重建，禁止原地覆盖。

所有文件成功后写 manifest 并 flush；先 rename 完整 attempt 至 versions，再通过临时指针原子替换 current.json。Linux 同步文件及相关目录，Windows 使用 MoveFileEx replace/write-through。指针切换前失败不影响旧 current；指针已切换后若目录持久化发生 IO 错误，需用 sources status 检查实际发布状态，不能把物理存储故障视为可完全撤销的普通失败。

默认拒绝没有 JS 的 ZIP，显式 allow_empty 才可发布空集合。完整成功发布后上游删除文件从当前列表消失，旧下载路径返回 404；同步失败不触发线上删除。GitHub 不可达时原有效版本继续分发；本地文件损坏返回 503，等待同步修复或手动回滚。

## 资源与清理

下载和解压串行流式执行，避免把 ZIP/整套脚本常驻内存。staging 最多占一份受限 ZIP 加一代受限解压结果；默认预算 512 MiB + 2 GiB。开始同步前要求可用空间至少覆盖两个预算加 reserve_bytes，默认合计 3.5 GiB；实际不预分配这些空间，可根据真实附件大小调低预算。运行中每约 1 MiB 重新检查余量，任何超限整轮失败，绝不截断后发布。

成功发布后 GC 保护 current，保留最近两个 generation，并对其他版本提供至少 24 小时宽限；下载/列表/status/rollback 对固定版本持共享读取锁，GC 仅清理能获得独占锁的版本。旧版本/孤立版本回收失败记录 cleanupWarnings，不把已提交的新版本回滚。未变化轮次不额外创建版本；失败残留在下次持独占同步锁时优先处理。

只允许 HTTPS 的 api.github.com/github.com/objects.githubusercontent.com/release-assets.githubusercontent.com，初始附件 URL 必须匹配已配置仓库的 releases/download 路径。最多 5 次重定向；跨主机去除 Authorization。429/5xx 最多 3 次请求，Retry-After/退避最长单次 30 秒，受整个任务 timeout 控制。

## 接口与 NAS 调度

GET /lx/sources 保留 status/sources 及 path/name/directory/downloadUrl 字段，稳定排序，只暴露 .js。下载地址由 server.public_base_url 生成，不使用客户端 Host；未配置公网地址时列表返回 503。GET /lx/script?path=... 只解码一次、只接受一个 path 参数，仅可访问 manifest 中的脚本；缺失返回 404，损坏/未初始化返回 503，非法路径返回 400。下载前校验大小和摘要，随后从已打开文件流式响应；Content-Type 为 application/javascript，no-store，摘要作为 ETag，支持标准条件/范围请求。不会暴露配置、数据库、manifest、锁或 staging。

首次可手工执行 sync-sources，再启动/刷新 API；后续无需重启服务。建议在 NAS 计划任务中每 6 小时执行一次同一二进制，例如：

```sh
/vol1/appdata/cyapi/cyapi sync-sources --config /vol1/appdata/cyapi/config.yaml
```

具体周期未由旧脚本确认，应按用户现有任务配置；不要继续向 R2 写入作为新服务的运行依赖。本次未改动用户 NAS 的旧任务或 R2 内容，正式切换时再替换计划任务。

## 已验证与待验证

自动化使用模拟 GitHub HTTP 响应和真实 ZIP/文件系统：正式版本与首 ZIP 选择、GB18030 文件名、全部文件镜像/只公开 JS、增改删、unchanged、配置变化、本地损坏重建、force/dry-run、回滚、路径穿越/重名/CRC/大小限制、磁盘余量、重定向与 Token 隔离、限流重试、跨进程互斥、连续 10 次下载失败和 10 次进程崩溃后的回收、活动读者与 GC。Windows 无创建符号链接权限时该单项显式 skip；Linux CI 将执行。

仍需在真实 NAS 验证 GitHub 网络、当前最新 ZIP 文件集/大小、Linux 文件系统及磁盘预算、实际并发下载和任务调度。断电恢复、Docker/frp 实际部署与 race 结果以最终运行环境为准，不能用本机编译通过代替。
