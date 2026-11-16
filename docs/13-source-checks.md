# 音源检测与管理

当前提供手动批次检测、默认样例、隔离脚本执行、音频结构检查和 JSON 报告。
检测不会修改当前音源快照或自动发布推荐。已提供管理页面与独立的手动推荐发布；发布及客户端行为见 [推荐音源](14-source-recommendations.md)。暂未提供自动调度。

## 使用

先通过现有 `sync-sources` 同步音源，再运行：

```sh
cyapi sources check --config config.yaml
cyapi sources check --config config.yaml --platform qq
cyapi sources check --config config.yaml --source-path '优质/独家音源-v7.js'
cyapi sources check-status --config config.yaml
```

`--source-path` 必须是当前 `/lx/sources` 列表中的真实路径。默认检查 QQ、网易标准音质（128k）；脚本不声明支持时记录 unsupported，不偷偷改成其他音质。

进度输出到 stderr，最终报告 JSON 输出到 stdout，同时持久保存：

```text
<runtime_directory>/source-checks/
  samples.json
  check.lock
  runs/<run-id>.json
```

`check-status` 只读最新批次，不执行脚本，不要求数据库打开或 HTTP 服务启动。后台服务可以正常运行时手动检查；检测批次使用独立的跨进程锁，不允许重复执行。

每个样例完成后保存一次报告。Ctrl+C 取消后保存 canceled；意外退出的 running 报告在下次执行时标为 interrupted。历史保留最近 30 天且最多 100 份。单首失败不使整个命令报错：completed 表示任务执行结束，不表示音源通过。

## 样例

首次运行复制内置样例到 `samples.json`，不覆盖用户已有文件。也可指定：

```sh
cyapi sources check --config config.yaml --samples ./my-samples.json
```

内置 QQ、网易各三首已核实详情和时长的样例。首次使用复制内置样例，升级不覆盖已有 samples.json。管理页面可增删、启停及刷新详情，最多 32 首。全部停用时可以保存，但不能启动检测。批次保存输入快照，修改只影响下次检测。

## 结果含义

| 字段 | 含义 |
| --- | --- |
| initialization | 脚本初始化状态或平台/音质不支持 |
| resolution | 地址解析成功、失败、超时或取消 |
| outcome | failed / unverified / recognized_audio / suspected_short_resource / unsupported / canceled；本次检测结论 |
| failureStage | sample / probe / initialization / resolution / resource；失败发生在哪一步 |
| resource.status | not_run / recognized_audio / invalid / unreachable / unverified |
| resource.durationMs | 能可靠读取时才提供，否则缺省 |
| resource.durationMethod | 时长来自哪种结构；不是下载或解码时长 |
| resource.assessment | unknown / suspected_short_resource / no_short_duration_detected |
| failureOrigin | source / probe / sample / resource；不把执行环境限制归咎于音源 |
| probeIssues | 本次执行遇到的检测策略或预算限制，与脚本最终错误分别保留 |
| resource | 原始/最终域名、HTTP 状态、类型、Content-Range、响应长度、实际读取字节、格式特征、编码及判定代码；不保存完整播放地址 |
| http | 仅域名、方法、状态和脱敏业务错误等有限诊断 |
| isolation | 实际使用的 worker 内存限制，或显式未隔离标识 |

初始化、解析或资源访问失败均计为失败；仅在已取得地址、资源检查未能确认格式时计为资源未验证。解析失败时资源状态为 not_run（资源未执行）。检测限制不会覆盖脚本最终错误：例如备用请求被策略拒绝、随后 QQ 返回无播放权限，保留两条证据，汇总为失败并注明涉及检测限制。失败计数表示本次尝试失败，不代表该音源在所有网络和客户端永久不可用。列表和推荐健康信息共用同一分类规则；“涉及检测限制”是失败的子集，不额外累加。

MP3 需要连续有效帧结构；时长只取 Xing/Info/VBRI 信息，不根据局部码率猜完整时长。FLAC 读取 STREAMINFO。M4A/MP4 使用纯 Go 的 go-mp4 库，检查音轨标记、mp4a 描述及媒体数据，并从音轨 mdhd 读取时长；支持这条路径中的 AAC 标记。遍历不展开媒体、封面或采样表，并限制元数据数量、深度和大小。只有 ftyp、只有视频轨、元数据在读取窗口外等情况保留为 unverified，不从扩展名或 Content-Type 推断成功。其他未支持格式仍为 unverified；返回 HTML/JSON 错误页为 invalid。recognized_audio 仅表示可识别结构，不是播放器解码通过或歌曲内容正确的证明。无须安装 ffmpeg、Node 或额外服务。

有可靠预期和实际时长时，实际不足预期 80% 且短了超过 15 秒，标记疑似短资源。播放地址不写入报告，资源验证在取得地址的同一 worker 立即执行，不依赖日志中的签名链接。

## 资源证据改进（goja-probe-v5）

本阶段继续使用 Goja worker 和纯 Go 格式检查，无新增部署依赖，不执行 FFmpeg/ffprobe 或实际音频解码。

- 在既有 MP3、FLAC、M4A/MP4 检查外，增加 Ogg Vorbis 和 Opus（mapping family 0）结构识别。校验 Ogg 页边界、CRC、序号、逻辑流、编码标识及音频包存在性；只在取得完整单逻辑流、有效 EOS 和采样位置时读取时长。串联、多逻辑流、其他编码与超出预算的元数据保留为未验证。
- 报告增加受限文件后缀和前 16 字节十六进制文件头。不保存签名 URL 或其查询参数。已识别的标准音频不因扩展名为 .mgg 而被否定；未知结构配合 QQ 加密格式后缀时，提示“疑似加密资源”，仍属于未验证，不能据此断言具体加密算法。
- 未知格式区分完整资源仍无法识别、部分读取证据不足，以及 Ogg 编码/结构/校验问题。HTTP 错误、HTML/JSON 错误页与解析失败继续沿用原有失败分类。
- 同一批次、脚本路径与版本、平台、音质下，只有完整下载且 SHA256 相同的不同歌曲样例才关联重复资源。相同 TrackID 或标题/歌手一致的样例不互报；不依据 URL、域名或大小推断相同内容。resource.duplicateSampleIds 保存关联样例，前面已经完成的结果也会更新并持久化。
- 重复资源是需人工核对的警告，不改变结构识别、短音频或失败结论。列表及推荐健康信息中的 Duplicate/duplicate 和 SuspectedEncrypted/suspectedEncrypted 是原有结果的子集，不能再加入总数。即使确有相同文件，也不能自动认定哪一首错误或整个音源不可用。
- 管理报告增加所选范围的平台摘要，直接列出疑似加密和重复资源；每项显示格式、时长依据与“解码：未执行”。“未发现明显偏短”沿用既有阈值，不等于精确时长匹配或歌曲身份已确认。

本阶段验证：后端 go test ./...、管理页面 6 项回归均通过；真实 Chrome 使用受控响应验证摘要、重复/疑似加密证据、逐项默认展开，以及桌面和手机布局。新增 Ogg 用例验证 Vorbis/Opus 时长、CRC 损坏、截断、未知编码与串联流边界；这些结构夹具不代表解码验证。尚未部署到线上或重新检测公共音源。

升级后旧版本报告保留原始证据，不进入 v5 健康汇总；需重新运行检测。此次没有增加检测并发、定时任务、自动推荐或客户端换源逻辑。

## 执行与部署边界

- 每个脚本/平台/歌曲一个 worker，串行运行；新安装默认六首、27 份脚本为 162 项；已有两首样例的安装仍为 54 项。
- 整个 worker 最多 40 秒；初始化 12 秒、解析 12 秒、资源读取 8 秒；同步 JS 轮次另有中断预算。
- 任务最多 64 个请求，最多 4 个同时执行；响应单个解压后 8 MiB、任务累计 32 MiB；音频最多读 8 MiB。
- stdout/stderr 各限制 256 KiB，超限终止。脚本总快照最多 64 MiB、单份最多 8 MiB。
- 只允许公网 HTTP(S)，每次连接校验并使用确定的公共地址；拒绝私网、环回、链路本地、基准测试网段等地址。重定向同样检查；不继承服务端 Cookie、Token 或代理。
- 如果本机代理使用 198.18.0.0/15 等 fake-IP DNS，可能被正确拒绝并记录 probe_policy。应在真实 DNS/合适的检测出口运行，不把此类拒绝当成音源停服，不通过关闭目的地址校验解决。
- Windows worker 使用 Job Object 的 512 MiB 进程内存限制；Linux 需要可写的委派 cgroup v2 memory controller；原生 systemd 安装单元配置 Delegate=memory，服务进程移入专用叶子组，worker 在同级叶子组设置 512 MiB 内存上限和零 swap。子进程隔离不等于完整 OS 安全沙箱。
- Linux 未配置硬限制时默认执行失败。仅对管理员手动诊断提供 `--allow-unconfined-worker` 显式选项，报告标注 explicitly_unconfined；该选项不是生产自动运行的默认值。GOMEMLIMIT 只作为软限制。

纯 Go，CGO_ENABLED=0。Linux 交叉构建成功不等于目标 NAS 已实测。正式自动检测开启前必须实测部署权限、内存上限、取消回收及对 API 的影响。

备份命令与定时备份现已包含检测目录，详见下文。

## 开发验证

```sh
go test ./internal/sourcecheck ./internal/sources ./cmd/cyapi
go test ./...
node --test internal/admin/web_test.cjs # 仅开发/CI 使用，不是服务部署依赖
```

测试包含真实 worker 子进程协议、HTTP/Promise/取消、加密契约、任务锁与中断报告、快照哈希校验、样例身份检查、音频错误页/短资源识别，以及网络目的地址和输出边界。不依赖公共音源持续在线。

## 管理界面

在配置文件启用管理功能并填写允许管理的现有账号 ID，然后重启服务：

```yaml
admin:
  enabled: true
  user_ids: [1] # 替换为实际账号 ID，不是用户名
```

默认关闭，不自动授权首个账号。账号 ID 可由登录响应 data.id 或登录后 GET /auth/validate-token 的 data.id 获取。浏览器打开 `/admin/source-management`，使用现有账号密码登录；无须额外前端构建或 Node 服务。令牌保存在当前标签页的 sessionStorage，按服务路径区分；刷新页面时向后端验证登录及管理权限后恢复，不保存密码。退出登录、令牌失效或管理权限撤销时清除保存的令牌；临时网络错误保留会话供刷新重试。关闭标签页通常结束会话，浏览器恢复标签页时可能恢复 sessionStorage，最终有效性仍由后端验证。浏览器禁用存储时仍可登录，但刷新后需重新登录。对外访问沿用已有 HTTPS 反向代理。

- **音源与推荐**：搜索脚本，选择 QQ、网易或两者，检测单个或全部；查看进度、取消和逐首报告。平台结果时间可打开对应批次，历史报告保留样例快照。
- 详情逐项默认展开，显示阶段耗时、资源响应和时长依据，可手动折叠。无法读取可靠时长时明确保留“未核实”。
- **测试歌曲**：选择平台、输入歌曲 ID，自动获取标题、歌手、专辑及详情时长；可启停、刷新详情、删除。上游获取失败不会修改样例。并发修改提示冲突并刷新数据。
- 关闭页面不取消任务。一次一个批次，与 CLI 共用跨进程锁；相同请求复用任务，其他请求返回忙碌。服务停止会取消子任务。
- 列表只汇总脚本哈希、样例版本、检测器版本匹配的记录，超过 24 小时标注过期。结果是服务器网络环境下的证据，不是 Windows 客户端播放保证。
- `/admin/source-checks/*` 复用账号认证并校验 admin.user_ids。管理接口不返回 CORS 跨域授权头；浏览器按实际访问 URL 执行同源限制。HTTPS 经多层 HTTP 反代时，登录不要求配置公网域名或代理信任；真实 IP 与下载链接的代理配置仍按既有规则处理。
- 页面使用强隔离 worker，不提供 allow-unconfined-worker 开关。Linux 须配置 cgroup v2 委派；启动批次前验证实际创建 worker 内存限制的权限，未满足时直接提示环境未就绪，不生成整批“未验证”报告。

当前不提供自动定时检测。管理员可根据报告主动发布推荐；客户端仅在主动选择推荐入口后接收后续版本，详见 [推荐音源](14-source-recommendations.md)。

### v2 检测器升级

检测器版本为 `goja-probe-v2`。升级保留旧报告原始证据，查看时提示版本差异；旧版数据不参与新版列表和推荐健康汇总，需要重新检测。不会用新规则悄悄改写旧报告。

回归样本包括 fish QQ 历史响应中的 M4A/AAC 元数据（音轨时长 217129ms，独立 libmpv 读取为约 217.129796 秒）。测试夹具只保留容器元数据，替换了媒体内容，不能用于证明解码成功；另以本地保存的完整采样前缀核对识别结果。未识别的 HYW 历史数据仍为未验证，不强行改为成功。本机新取得的约 60 秒 MP3 能标为疑似短资源；这不证明用户此前 Time Machine 响应也是同一格式或同一故障。

分类回归覆盖解析失败加策略拒绝、HTTP 404、未知格式、短资源、取消，以及列表/推荐计数一致性和历史版本隔离。真实浏览器使用受控 API 响应检查报告默认展开、资源证据和桌面/手机布局。以上不替代 NAS 网络出口和 Windows 实际播放验证。

## 备份与恢复检测数据

`cyapi backup` 和定时备份加入 `source-checks/samples.json` 及 `source-checks/runs/*.json`，manifest.json 的 SourceCheckFiles 保存各文件 SHA-256。旧备份没有该字段仍有效。

快照使用短文件发布锁，不等待整批网络检测；正在运行的报告在**备份副本**中标记 interrupted，线上任务不变。恢复时停止服务与 CLI 检测，核对 SourceCheckFiles 哈希，将备份 source-checks 内容恢复到 `<runtime_directory>/source-checks`，不要恢复锁文件。重启后可查看旧报告并重新检测，不续跑半个任务。

## 本阶段验证边界

Windows 全量 Go 测试通过。真实浏览器已验证登录、列表、样例启停、启动/取消、报告、退出，并检查桌面与手机布局。测试覆盖样例冲突、跨进程锁、请求取消隔离、运行中备份及恢复读取。歌曲详情解析契约使用本地上游响应验证。解除测试进程的沙箱网络限制后，真实管理 API 新增 QQ 000mkfVC48z4QV 返回 201、时长 212000ms，网易 2600493765 返回 201、时长 275912ms；随后删除均返回 200。Linux/NAS 仍须目标机器实测。

## 已有 Linux 安装启用检测

系统须使用 cgroup v2，且 `/sys/fs/cgroup/cgroup.controllers` 包含 memory。新版安装器与升级器生成 `Delegate=memory` 服务单元，并在创建 worker 前处理 cgroup 的空父组规则；无需给服务 root 权限。

从尚未生成委派权限的旧版升级时，必须由**新版本程序执行升级**，以更新 systemd 单元。将对应架构的新程序放为安装目录中的 `cyapi-new`，在安装目录运行：

```sh
chmod +x ./cyapi-new
sudo ./cyapi-new upgrade --config ./config.yaml --file ./cyapi-new
```

此操作沿用已有备份、迁移、程序替换和重启流程，保留账号和配置。只用旧版升级器替换二进制，无法补上新版生成的 systemd 权限。容器或非 systemd 安装仍须显式提供可写的 cgroup v2 委派；管理页面不静默退回无内存硬限制的执行模式。

发布门禁使用非 root 的真实 systemd 服务验证 worker 脚本执行、取消回收、512 MiB 内存与零 swap 限制，以及真实内核 OOM 后父进程继续服务。安装与旧版迁移测试也通过管理 API 执行六项隔离 worker，避免仅以服务存活或编译成功视为检测功能可用。
