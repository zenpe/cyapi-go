# 音源检测资源预算与测量

## 行为边界

自动维护保留既有选源、故障复测和串行子进程隔离流程。本次只减少重复检测与明确错误响应的无效读取，不引入事件总线、进程池或客户端故障上报。

### 新鲜报告复用

- 仅在自动调度准备启动实际批次时查找；不在每个 Tick 扫报告内容。
- 每次最多读取最近 16 个报告文件、累计 16 MiB，逐个解码。文件太大、损坏、查找失败或不匹配都回到真实检测。
- 必须匹配脚本路径与 SHA256、平台、128k 音质、当前完整样例集版本、检测器版本和受限 worker 隔离标记；同一报告必须包含该脚本该平台的完整样例组，禁止跨报告拼凑。
- 优先采用最新匹配报告，包括失败结果；不能跳过新失败寻找旧成功。样例观察时间必须落在该报告起止时间内，且最早一项距今小于 `min(15 分钟, evidence_max_age, watch_interval)`。15 分钟是保守的初始窗口，不是性能测量得出的最优值。
- 复用保留原 RunID 和检查时间，下一次正常巡检从最早样例观察时间起算，不延长证据寿命、不重复累计任务和资源流量，不把同一 RunID 当成第二次失败。
- 一分钟故障复测、替换批次、回退目标首次检查、管理员“立即巡检”、手动单项/全部检测都真实执行。管理员立即巡检按平台保存待办，重启不丢失第二个平台的强制检查意图。
- 手动完成只更新报告。自动维护在自己的调度点才可能使用该证据，发布仍校验样例、目录哈希及控制版本。

### 分段读取

单个 HTTP 响应按 64 KiB 读取，保持 8 MiB 上限、总传输预算和原任务超时。新增 `resource.readStop`：`eof`、`read_limit`、`non_audio_response`、`read_error`；`resource.bytes` 是实际交给检测器的响应体字节数，不是歌曲标称大小，也不包括脚本接口流量及 TCP/TLS 开销。

仅明确 HTML 或完整 JSON 错误响应可提前终止并关闭连接。普通音频、未知格式及疑似加密资源继续读取。结构识别不代表播放验证，不能据此提前成功。完整响应才计算文件 SHA256；完整 MP3 帧时长继续优先于 Xing/VBRI，Ogg EOS、MP4 尾部和重复资源判定保持不变。上限与提前结束都不冒充完整文件。小于首段的响应正常按 EOF 处理。

检测器更新为 `goja-probe-v6`，旧报告保留可查，但不用于此次复用及新推荐的检测证据。

## 可复现基线

在同一机器、同一 Go 版本下比较修改前后，避免同时运行其他构建：

```sh
go test ./internal/sourcecheck -run '^$' -bench BenchmarkResourceFixtures -benchmem -count 3
go test ./internal/sourcecheck -run '^$' -bench BenchmarkIsolatedWorkerLifecycle -benchmem -count 3
```

固定合成样本包括正常 FLAC、171072 字节短 MP3、未知/疑似加密二进制和 HTML 错误。完整资源重复哈希、网络错误、取消、超时和总预算另由回归测试覆盖。没有第三方网络波动、实际解码或音轨身份验证。

2026-11-17 本地 Windows/amd64、i5-14400、Go 1.27.3 三次运行中位数：

| 样本 | 修改前读入字节 | 修改后读入字节 | 修改前 B/op | 修改后 B/op | 修改前耗时 | 修改后耗时 |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| 2 MiB 正常 FLAC | 2097152 | 2097152 | 14163141 | 4200331 | 19.28 ms | 3.65 ms |
| 171072 字节短 MP3 | 171072 | 171072 | 1214728 | 527368 | 3.60 ms | 0.36 ms |
| 2 MiB 未知二进制 | 2097152 | 2097152 | 8903455 | 4199385 | 15.89 ms | 4.00 ms |
| 2 MiB HTML 错误 | 2097152 | 65536 | 6802899 | 133214 | 10.13 ms | 0.060 ms |

以上 B/op 为每次操作累计分配量，不是存活堆、RSS 或 NAS 内存峰值。普通音频读取流量不减少；内存与 CPU 改善也包括取消对整段音频做大小写转换。不要据此推算生产扫描节省比例。

本地无网络的隔离 worker 初始化失败样本，总生命周期约 21.8–24.1 ms，worker 内部计时约 4.0–4.3 ms。差值包含启动、隔离、IPC、调度与计时精度，不等于纯 fork/exec 成本；不能外推 ARM NAS。基准的 B/op 只计父测试进程分配，不含子进程内存。Linux 必须提供 cgroup v2 委派，否则此基准跳过；不会为测量关闭生产隔离。

## NAS 实测与内存配置

在 NAS 空闲、当前推荐日常巡检、手动全量检测、取消/超时及正常 API 请求并发时分别测量。先运行测量，再从管理端触发检测；采样期间不要重启目标 PID：

```sh
python3 scripts/measure_source_memory.py --pid PID --seconds 300 --interval 0.1 > memory.json
```

脚本仅用 Python 标准库读取 Linux `/proc`，不读取凭证、进程命令或环境。分别输出父进程、所有后代进程、同时合计的采样 RSS 峰值，以及父进程生命周期 VmHWM。采样可能漏掉短进程，RSS 相加会重复计算共享页，不等于 cgroup 内存计费。需同看委派服务/容器根 cgroup 的 `memory.current`、`memory.peak`、`memory.events`，并确认没有 `oom`/`oom_kill`。容器内需 PID 可见及 `/proc` 读取权限。

当前没有目标 NAS 的内存测量结果，因此不新增默认内存硬限制：

- worker 继续 `GOMEMLIMIT=384MiB`、`GOMAXPROCS=1`、512 MiB cgroup/Job 硬上限及串行执行；不会继承服务进程的软限制。
- 主进程 `GOMEMLIMIT` 只约束 Go 管理的内存，不是进程 RSS，也不限制 worker。应根据父进程负载测量设置，并为非 Go 内存留余量；过低会使 GC 频繁运行。
- 容器或 systemd `MemoryMax` 覆盖整个服务及后代。预算至少容纳主服务实测占用、一个 512 MiB worker 及文件缓存/运行时余量；不能把容器设成 256 MiB 却保留现有 worker 预算。
- 可选 Compose 覆盖文件为 `packaging/compose.memory.example.yaml`，必须显式设置 `CYAPI_MAIN_GOMEMLIMIT` 和 `CYAPI_CONTAINER_MEMORY_MAX`，没有未经测量的默认值。不使用它则部署行为不变。
- systemd 可在 `secrets.env` 中设置主进程 `GOMEMLIMIT`；如需硬限制，另建 drop-in 设置经过测量的 `MemoryMax`。保留 `Delegate=memory`。Compose 内存配置不能替代 cgroup 委派，隔离不可用时仍拒绝执行 worker。
- 不设置 `GODEBUG=madvdontneed=1`，不修改 SQLite 的 WAL、`synchronous=FULL` 或连接数。

完成 NAS 测量后再确定具体预算；记录设备、Go 版本、脚本数量、任务规模、主/子 RSS 与根 cgroup 峰值，不能只记录一个总内存数字。
