# 接口兼容与功能迁移规则

## 1. 契约来源与记录格式

当前 src/routes、src/core/middleware.ts 和 tests 是首要依据。README 有过时音源说明：实际下载路由为 /lx/script?path=...，且代码与测试支持子目录，不应按旧 README 的 /lx/scripts/:filename 或“仅顶层”实现。

[路由清单](07-route-inventory.md)枚举当前静态注册的 HTTP 方法与路径。P0 为每项补充：鉴权、输入/默认值、HTTP 状态、响应 JSON、响应头、重定向、缓存、上游副作用、实现状态及测试样本。所有旧路由都要归类为首期范围或明确排除；首期路由不可遗漏。用户确认排除 B 站、抖音及 MPD，不要求保留这些路由。

响应不强制改成统一 envelope：当前 status/code、data/result、msg/message 存在差异，保留路由既有行为。区分 null、空数组、缺失字段、数字和字符串。保持 JSON/form 请求兼容；Go ServeMux 的 HEAD、路径清理、尾斜杠重定向与 Hono 的差异要单独检查。

## 2. 模块映射

| 当前文件 | Go 模块 | 重点 |
| --- | --- | --- |
| routes/auth.ts、core/auth*.ts、middleware.ts | auth + server | 新会话、邮箱验证码、限流；扫码暂不实现 |
| favorites.ts、accounts.ts | library | 用户隔离、来源 ID、绑定删除 |
| playlists_v2.ts | playlist | 幂等、排序版本、快照和同步 fencing |
| playback.ts、stats.ts | playback | 累加、稳定分页、私有缓存头 |
| music.ts、lyrics.ts、content.ts | music + provider | 搜索回退、歌词状态、发现页游标 |
| bili.ts、douyin.ts | 首期排除 | 仅保留旧代码盘点记录 |
| lx.ts | sources | 公开列表/下载、路径与原始字节 |
| compat.ts、health.ts | compatibility + 业务模块 | 区分真实能力、占位与管理能力 |

## 3. 账号与鉴权必须保留的细节

账号全新注册，不迁移旧账号、密码哈希或会话；不保留旧四段/五段 Token 的验证兼容代码。客户端继续将新登录返回的 Token 当作不透明字符串，通过 Authorization: Bearer 发送。新系统只签发并接受一种 HMAC 格式，使用独立新密钥，校验期限、签名和 token_version；改密撤销当前系统已签发会话。

新注册密码使用 Go bcrypt，定义 UTF-8 字节长度边界并测试 Unicode；超出 bcrypt 支持范围的密码明确拒绝，不静默截断，不为旧 bcryptjs 行为添加兼容分支。

`/auth/qr-login/create|poll|scan|confirm|cancel` 首期不注册处理器，不创建或消费二维码会话，客户端扫码入口需要隐藏。兼容文件中的 `/login/qr/*` 与 `/kugou/login/qr/*` 同样暂不实现，不以固定“等待扫码”响应宣称可用。公开路径和客户端页面只展示已实现的邮箱注册、密码登录、验证码找回密码和会话校验。

登录每账号 15 分钟最多 10 次失败，每 IP 100 次请求；验证码验证每账号 10 次/15分钟、每 IP 100 次；发信每账号 1 次/分钟、每 IP 10 次。登录成功只退本次账号预占，保留其他失败及 IP 消耗。保留 Retry-After 和 retryAfterSeconds。

公开白名单仅包含首期实际实现且无需登录的接口，移除所有暂不实现的扫码路径；OPTIONS 放行。新增 /health/live、/health/ready 为无敏感信息健康接口；其他诊断和管理能力仅本机/私网访问。CORS 按实际客户端配置允许来源，不机械保留 wildcard + credentials 的无效浏览器组合。

## 4. 搜索、歌词与缓存

- 搜索 page 默认 1、limit 默认 20；范围 page 1..1000、limit 1..50，非法输入在请求上游前拒绝。
- 保留 resultKind、hasMore、pageFingerprint；指纹依赖原始 ID 列表的 JSON 编码和 MD5。Go 默认 JSON HTML 转义等差异需要固定样本测试。
- QQ 首次搜索的 full → suggestions 回退、后续页不回退和共享总预算遵循当前代码/测试，不能改成并行请求制造额外流量。
- 上游错误返回 SEARCH_UPSTREAM_FAILED/SEARCH_TIMEOUT 等契约；不能用 200 空数组掩盖失败。
- 歌词保留 ready/empty/unavailable/unsupported 和 retryable；只有明确无歌词才负缓存；平台返回 401 不能使用户误以为应用会话失效。
- /v1/discover/* 游标的 base64url、分页、revision、无效游标处理保持一致。
- 仅共享公共内容，统计响应为 private/no-store。缓存失效或容量满不得破坏上游成功响应。

## 5. 歌单与统计

保留 local/synced 区别，同步歌单只读、可复制成本地。sourceIds、rank、contentRevision、orderRevision、binding_version、sync_revision、operationId 的用途不可合并或省略。

远端重复歌曲按 source_position/occurrence_index 保留；不要全局去重导致曲数和顺序变化。失败或旧同步不得覆盖有效快照。分页排序保留显式平局规则。

统计 /stats 只返回前 10 排名但总量涵盖全部记录；/stats/records 默认 20、最大 50，以 limit+1 判断 hasMore；排名按 play_count、last_played_at、track_id、source 稳定排列。听歌时长累加不得变为覆盖写，响应成功不得仅表示写入内存队列。

## 6. 媒体与音源

歌曲解析大部分返回 URL，/song 的 type=down 重定向保持。首期不实现 B 站、抖音、MPD 或媒体代理；frp 只改变入站访问，平台请求默认走 NAS 出口。

/lx/sources 和 /lx/script 为公开路由，列表包括 path/name/directory/downloadUrl，支持嵌套路径和编码文件名；下载原始字节。默认保持 no-store，ETag 用内容摘要稳定生成，不承诺与 R2 旧 ETag 相同。

## 7. 占位能力与明确差异

compat.ts 中平台扫码、推荐、部分艺人/百科、酷我/Apple、天气、音乐品味、通用音频代理/解析及支付包含固定响应或未实现行为。逐路由标注，不可把它们宣传为真实实现；迁移保留现有占位契约，新增功能另立任务。

必须登记的安全差异：可信代理真实 IP、CORS 修正、sponsors/create 限权或关闭。限制请求/响应体积可能影响超大输入，也应列入客户端联调。错误消息不得回显数据库地址、Cookie 或完整内部异常。

## 8. 测试组织

现有 tests/auth、search、lyrics、stats、lx、cache.test.mjs 是语义参考，并非可以直接验证 Go 的现成测试集；需移植为 Go 测试或 HTTP 黑盒测试。基线没有独立歌单测试文件，P2 必须补充快照、幂等与冲突测试。

保存脱敏请求、平台响应样本和期望响应；对随机 ID、时间戳、traceId 做受控归一化，不忽略核心字段。确定性对比使用相同 mock 上游，真实平台请求只做单独 smoke test，避免时效 URL 和风控干扰回归。