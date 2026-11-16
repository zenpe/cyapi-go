# 推荐音源：发布与客户端使用

管理员在检测管理页选择一份确定的脚本发布，客户端选择固定的“推荐音源”入口后，在下次启动检查更新。发布只保存原始脚本字节，不生成聚合脚本；服务不依赖 Node、Redis 或额外运行时。检测使用现有 Goja worker，客户端执行仍使用 QuickJS。

## 启用与操作

在现有配置中增加以下字段并重启服务：

```yaml
admin:
  enabled: true
  user_ids: [1] # 替换为实际管理员账号 ID
source_recommendation:
  enabled: true
```

两个开关默认关闭。管理授权复用现有账号和 Bearer 认证；普通登录用户无权发布。打开 `/admin/source-management`，登录后在“音源与推荐”查看 QQ、网易逐首结果，点击“设为推荐”，核对脚本哈希及检测证据，填写说明后确认。未检测、过期或存在失败的脚本允许管理员明确发布，页面提示其证据边界。检测任务不会自行发布。

当前推荐卡片显示实际脚本、内容哈希、发布时间和说明；历史记录提供回退。回退把归档脚本作为一次新发布，产生新的 releaseId。发布时以 expectedReleaseId 校验当前版本，并核对候选快照 generation/path/sha256；其他管理员已经发布或快照已变化时返回 409，刷新后重新确认。相同内容不会重复发布。

公共入口关闭时仍可在后台准备发布，页面会提示尚未公开。关闭入口不删除记录或客户端本地脚本。当前没有自动检测调度，也没有 `source_checks` 或 `history_keep` 配置项。

## HTTP 契约

| 路径 | 行为 |
| --- | --- |
| `GET /lx/sources` | 在普通列表前加入 `推荐/recommended.js`，带 kind、releaseId、sha256、resolvedName 等元数据 |
| `GET /lx/recommendation` | 返回 status 与 recommendation，包含当前版本及 QQ/网易检测摘要；未发布 404，记录损坏 503 |
| `GET /lx/script?path=推荐/recommended.js` | 返回当前原始脚本字节；兼容旧列表下载方式，必须重新验证缓存 |
| `GET /lx/script?path=推荐/recommended.js&sha256=<64位小写哈希>` | 下载指定归档版本；未知 404，清理过的版本 410，成功可长期缓存 |
| `GET /admin/source-recommendations` | 管理员读取 current、history、recommendation 摘要和公共 enabled 状态 |
| `POST /admin/source-recommendations` | 管理员发布或回退，必须填写说明 |

普通发布输入为 expectedReleaseId、generation、path、sha256、note；首次 expectedReleaseId 为空。回退改用 expectedReleaseId、fromReleaseId、sha256、note。可选 reportRunId 须包含对应脚本哈希证据。

推荐描述 ETag 覆盖完整响应，报告变化会更新 ETag；脚本 ETag 是原始字节 SHA-256。摘要匹配脚本哈希、样例版本及检测器版本，分别统计可识别音频、疑似短资源、失败、未验证、不支持，24 小时后过期。该证据不能证明 Windows 解码或完整歌曲可播放。

推荐归档独立于同步快照；同步失败时已有推荐仍可下载。普通列表不因推荐未发布而整体失败。旧客户端可按普通脚本使用入口，但没有新版客户端的启动更新保证；未带明确推荐元数据的旧本地配置不会按名称强制迁移。

## 客户端行为

音源设置顶部显示推荐卡片、服务器当前推荐、检测摘要、本地实际版本及更新失败原因。用户主动选择推荐后，脚本绑定对应 backendOrigin；更换服务器不会把另一服务器的推荐自动覆盖进来。普通源、导入源及 Navidrome 保持用户原有选择。

启动恢复播放前，推荐源最多额外等待 3 秒选择最新版本。在线描述确认哈希后，可复用已校验缓存；下载限制 8 MiB、绝对 10 秒，并在解码前校验原始字节。选择窗口结束后目标固定，候选准备沿用原脚本初始化预算；3 秒不代表整个初始化耗时上限。

候选准备及配置保存成功才提交运行时。失败保留旧配置并加载旧脚本；推荐更新保存回退日志，写入两项配置期间进程退出，下次启动恢复旧配置。手动选源、服务器变化或提前播放会使未提交的启动更新失效。联网迟到结果及运行期检查只缓存，不自动更换正在使用的运行时。此功能不增加播放失败后的跨源轮询、播放器重试或音质降级。

## 文件、备份与恢复

```text
<runtime_directory>/source-recommendations/
  current.json
  releases/r-<32位随机标识>.json
  publish.lock
<sources.directory>/published/
  <sha256>.js
  <sha256>.gone
```

发布使用跨进程文件锁和原子替换，顺序为归档、不可变记录、当前指针；失败的孤立记录不进入历史。保留最近 20 次发布及其脚本。未引用脚本满 24 小时后，在后续发布清理时删除并留下过期标记。已缓冲的下载不占用发布锁。

`cyapi backup` 和定时备份显式复制同一锁定引用集合；manifest.json 新增可选 `RecommendationFiles` 哈希映射，备份格式仍为 1，旧备份无此字段可正常读取。公开开关关闭时也备份已有记录。

恢复时先停止服务和相关 CLI，逐项核对 RecommendationFiles SHA-256：

1. 将备份的 `source-recommendations/current.json` 及 `releases/` 恢复到 `<runtime_directory>/source-recommendations/`。
2. 将备份的 `published/*.js` 恢复到 `<sources.directory>/published/`。
3. 不恢复锁文件；确认当前和保留历史引用的所有脚本存在且哈希匹配，再启动服务。

指针或归档损坏会让推荐模块报告不可用，服务不会猜测另一个“当前推荐”。客户端已有本地脚本仍可用；旧后端或未启用端点的 404 视为无推荐能力。

## 已验证范围

Windows 后端全量测试，以及并发发布、历史保留、下载哈希、摘要缓存、备份恢复通过。真实浏览器验证后台发布 A、切换 B、回退 A，并检查手机布局。真实 Go HTTP + Windows QuickJS 验证客户端选择、运行期仅缓存、启动更新、回退和坏脚本失败保留。

上述联调使用独立的确定性脚本，证明版本与事务行为，不是公网音源播放率测试。Linux amd64/arm64 已完成 CGO=0 交叉编译；Linux/NAS 的 cgroup 权限、内存上限及实际网络出口仍须目标设备实测，不能以编译通过代替部署验收。
