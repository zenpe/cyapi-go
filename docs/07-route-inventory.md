# 现有路由清单

从 F:\cyapi-workers\src\routes 的静态 app 路由注册读取，日期 2026-10-29。此表是覆盖基线，不是完整 OpenAPI，也不表示全部路由都有真实上游实现。鉴权以 core/middleware.ts 为准；响应细节在 P0 补齐。

这是旧仓库盘点，并非全部迁移承诺。B 站、MPD、抖音明确排除，应用和音乐平台扫码路由暂缓；其余状态见 evidence/contract-matrix.md。冒号参数迁移到 net/http 时转换成对应路径变量，保持对外路径不变。

| 方法 | 路径 | 源码位置（旧项目相对路径） |
| --- | --- | --- |
| GET | `/accounts/bindings` | `src/routes/accounts.ts:5` |
| POST | `/accounts/netease/unbind` | `src/routes/accounts.ts:21` |
| POST | `/accounts/kugou/unbind` | `src/routes/accounts.ts:25` |
| GET | `/auth/registration-status` | `src/routes/auth.ts:192` |
| GET | `/auth/linuxdo-status` | `src/routes/auth.ts:193` |
| POST | `/auth/linuxdo/login` | `src/routes/auth.ts:194` |
| POST | `/auth/qr-login/create` | `src/routes/auth.ts:196` |
| GET | `/auth/qr-login/poll` | `src/routes/auth.ts:230` |
| POST | `/auth/qr-login/scan` | `src/routes/auth.ts:297` |
| POST | `/auth/qr-login/confirm` | `src/routes/auth.ts:350` |
| POST | `/auth/qr-login/cancel` | `src/routes/auth.ts:403` |
| POST | `/auth/register/send-code` | `src/routes/auth.ts:433` |
| POST | `/auth/register` | `src/routes/auth.ts:481` |
| POST | `/auth/login` | `src/routes/auth.ts:521` |
| POST | `/auth/reset-password/send-code` | `src/routes/auth.ts:587` |
| POST | `/auth/reset-password` | `src/routes/auth.ts:623` |
| POST | `/auth/update-location` | `src/routes/auth.ts:647` |
| GET | `/auth/validate-token` | `src/routes/auth.ts:668` |
| POST | `/auth/update-username` | `src/routes/auth.ts:689` |
| GET | `/bili/ranking` | `src/routes/bili.ts:24` |
| GET | `/bili/cid` | `src/routes/bili.ts:34` |
| GET | `/bili/playurl` | `src/routes/bili.ts:45` |
| GET | `/bili/pgc_season` | `src/routes/bili.ts:61` |
| GET | `/bili/pgc_playurl` | `src/routes/bili.ts:72` |
| GET | `/bili/danmaku` | `src/routes/bili.ts:91` |
| GET | `/bili/search` | `src/routes/bili.ts:103` |
| GET | `/bili/comments` | `src/routes/bili.ts:114` |
| GET | `/bili/proxy` | `src/routes/bili.ts:125` |
| GET | `/mpd/:name` | `src/routes/bili.ts:137` |
| GET | `/version/latest` | `src/routes/compat.ts:18` |
| GET | `/toplists` | `src/routes/compat.ts:30` |
| GET | `/playlist` | `src/routes/compat.ts:41` |
| GET | `/album` | `src/routes/compat.ts:62` |
| GET | `/netease/top/playlist` | `src/routes/compat.ts:80` |
| GET | `/netease/playlist/highquality/tags` | `src/routes/compat.ts:103` |
| GET | `/login/qr/key` | `src/routes/compat.ts:114` |
| GET | `/login/qr/create` | `src/routes/compat.ts:118` |
| GET | `/login/qr/check` | `src/routes/compat.ts:122` |
| GET | `/kugou/login/qr/key` | `src/routes/compat.ts:124` |
| GET | `/kugou/login/qr/check` | `src/routes/compat.ts:128` |
| GET | `/recommend/songs` | `src/routes/compat.ts:130` |
| GET | `/recommend/resource` | `src/routes/compat.ts:131` |
| GET | `/recommend/for_you` | `src/routes/compat.ts:132` |
| GET | `/personal_fm` | `src/routes/compat.ts:143` |
| POST | `/fm_trash` | `src/routes/compat.ts:144` |
| GET | `/personalized` | `src/routes/compat.ts:145` |
| GET | `/personalized/newsong` | `src/routes/compat.ts:146` |
| GET | `/netease/user/playlists` | `src/routes/compat.ts:147` |
| POST | `/artist/search` | `src/routes/compat.ts:149` |
| GET | `/artist/detail` | `src/routes/compat.ts:163` |
| GET | `/artist/desc` | `src/routes/compat.ts:164` |
| GET | `/song/wiki/summary` | `src/routes/compat.ts:165` |
| GET | `/song/music/detail/get` | `src/routes/compat.ts:166` |
| GET | `/qq/playlist` | `src/routes/compat.ts:168` |
| GET | `/kuwo/search` | `src/routes/compat.ts:180` |
| GET | `/kuwo/song` | `src/routes/compat.ts:181` |
| GET | `/kuwo/playlist` | `src/routes/compat.ts:182` |
| GET | `/kugou/user/playlists` | `src/routes/compat.ts:183` |
| GET | `/kugou/playlist/tracks` | `src/routes/compat.ts:184` |
| GET | `/lyrics/kuwo` | `src/routes/compat.ts:186` |
| GET | `/apple/search` | `src/routes/compat.ts:188` |
| GET | `/apple/playlist` | `src/routes/compat.ts:189` |
| GET | `/apple/song` | `src/routes/compat.ts:190` |
| GET | `/apple/stream` | `src/routes/compat.ts:203` |
| GET | `/config/public` | `src/routes/compat.ts:205` |
| GET | `/info` | `src/routes/compat.ts:212` |
| GET | `/ip-location` | `src/routes/compat.ts:214` |
| GET | `/ip-location/query` | `src/routes/compat.ts:222` |
| GET | `/weather` | `src/routes/compat.ts:231` |
| POST | `/music-taste/generate` | `src/routes/compat.ts:233` |
| POST | `/music-taste/generate-sync` | `src/routes/compat.ts:236` |
| GET | `/audio-proxy/stream` | `src/routes/compat.ts:238` |
| GET | `/audio/proxy` | `src/routes/compat.ts:239` |
| GET | `/url/:source/:songId/:quality` | `src/routes/compat.ts:240` |
| POST | `/v1/parse` | `src/routes/compat.ts:241` |
| POST | `/pay/create` | `src/routes/compat.ts:243` |
| POST | `/pay/query` | `src/routes/compat.ts:253` |
| POST | `/sponsors/create` | `src/routes/compat.ts:264` |
| GET | `/sponsors/status/:userId` | `src/routes/compat.ts:285` |
| GET | `/sponsors/list` | `src/routes/compat.ts:310` |
| GET | `/v1/home/charts` | `src/routes/content.ts:38` |
| GET | `/v1/discover/playlists` | `src/routes/content.ts:53` |
| GET | `/v1/discover/tags` | `src/routes/content.ts:81` |
| GET | `/douyin` | `src/routes/douyin.ts:6` |
| POST | `/favorites` | `src/routes/favorites.ts:7` |
| GET | `/favorites` | `src/routes/favorites.ts:24` |
| DELETE | `/favorites/:trackId/:source` | `src/routes/favorites.ts:45` |
| GET | `/` | `src/routes/health.ts:6` |
| GET | `/lx/sources` | `src/routes/lx.ts:21` |
| GET | `/lx/script` | `src/routes/lx.ts:62` |
| GET | `/lyrics/netease` | `src/routes/lyrics.ts:52` |
| GET | `/lyrics/qq` | `src/routes/lyrics.ts:58` |
| GET | `/lyrics/kugou` | `src/routes/lyrics.ts:64` |
| POST | `/search` | `src/routes/music.ts:10` |
| POST | `/song` | `src/routes/music.ts:25` |
| GET | `/qq/search` | `src/routes/music.ts:50` |
| GET | `/qq/song` | `src/routes/music.ts:65` |
| GET | `/kugou/search` | `src/routes/music.ts:77` |
| GET | `/kugou/song` | `src/routes/music.ts:92` |
| POST | `/playback/save` | `src/routes/playback.ts:10` |
| GET | `/playback/last` | `src/routes/playback.ts:52` |
| DELETE | `/playback/clear` | `src/routes/playback.ts:77` |
| GET | `/playlists` | `src/routes/playlists_v2.ts:459` |
| GET | `/playlists/check-track` | `src/routes/playlists_v2.ts:475` |
| POST | `/playlists` | `src/routes/playlists_v2.ts:498` |
| POST | `/playlists/follow` | `src/routes/playlists_v2.ts:517` |
| PUT | `/playlists/:playlistId` | `src/routes/playlists_v2.ts:563` |
| DELETE | `/playlists/:playlistId` | `src/routes/playlists_v2.ts:597` |
| POST | `/playlists/:playlistId/delete` | `src/routes/playlists_v2.ts:598` |
| GET | `/playlists/:playlistId/tracks` | `src/routes/playlists_v2.ts:600` |
| POST | `/playlists/:playlistId/tracks` | `src/routes/playlists_v2.ts:644` |
| POST | `/playlists/:playlistId/tracks/batch` | `src/routes/playlists_v2.ts:695` |
| DELETE | `/playlists/:playlistId/items/:itemId` | `src/routes/playlists_v2.ts:766` |
| POST | `/playlists/:playlistId/items/batch-remove` | `src/routes/playlists_v2.ts:796` |
| POST | `/playlists/:playlistId/items/:itemId/replace` | `src/routes/playlists_v2.ts:829` |
| POST | `/playlists/:playlistId/items/reorder` | `src/routes/playlists_v2.ts:885` |
| POST | `/playlists/:playlistId/sync` | `src/routes/playlists_v2.ts:936` |
| POST | `/playlists/:playlistId/copy` | `src/routes/playlists_v2.ts:974` |
| POST | `/stats/listening-time` | `src/routes/stats.ts:15` |
| POST | `/stats/play-count` | `src/routes/stats.ts:29` |
| GET | `/stats` | `src/routes/stats.ts:50` |
| GET | `/stats/records` | `src/routes/stats.ts:73` |
| GET | `/stats/song-memory` | `src/routes/stats.ts:89` |

共 123 项显式方法/路径注册。计数不含中间件、自动 OPTIONS/HEAD、404/错误处理；这些仍需单独验收。

## 需要新增的运维接口

- `/health/live`：进程存活，不依赖上游。
- `/health/ready`：数据库/schema 基础可用，不泄露内部信息。
- 原 `/` 的成功响应保持兼容；不把根路径成功等同于完整就绪。

## 旧接口能力分类要求

compat.ts 必须逐项分类为真实适配、固定占位、管理写入或未配置错误。音乐平台扫码占位与 auth.ts 的应用扫码登录是不同功能。`/sponsors/create` 必须限权或关闭；B 站、MPD、抖音不进入首期实现。其余具体差异见接口兼容文档。