[English](./README.md) / 简体中文

### ！！必须先安装[MP4Box](https://gpac.io/downloads/gpac-nightly-builds/)，并确认[MP4Box](https://gpac.io/downloads/gpac-nightly-builds/)已正确添加到环境变量

### 添加功能

1. 支持内嵌封面和LRC歌词（需要`media-user-token`，获取方式看最后的说明）
2. 支持获取逐词与未同步歌词
3. 支持下载歌手 `go run . https://music.apple.com/us/artist/taylor-swift/159260351` `--all-album` 自动选择歌手的所有专辑
4. 下载解密部分更换为Sendy McSenderson的代码，实现边下载边解密,解决大文件解密时内存不足
5. MV下载，需要安装[mp4decrypt](https://www.bento4.com/downloads/)

### 特别感谢 `chocomint` 创建 `agent-arm64.js`
对于获取`aac-lc` `MV` `歌词` 必须填入有订阅的`media-user-token`

- `alac (audio-alac-stereo)`
- `ec3 (audio-atmos / audio-ec3)`
- `aac (audio-stereo)`
- `aac-lc (audio-stereo)`
- `aac-binaural (audio-stereo-binaural)`
- `aac-downmix (audio-stereo-downmix)`
- `MV`

# Apple Music ALAC/杜比全景声下载器

原脚本由 Sorrow 编写。本人已修改，包含一些修复和改进。

## 项目关系与上游来源

- 本仓库用于提供 Apple Music 下载能力，定位是“下载内核 + 多入口”。
- 上游脉络：`apple-music-downloader` -> `apple-music-downloader-bot` -> 本仓库（新增 AstrBot API 模式）。
- 本仓库保留 Telegram 机器人入口（`--bot`），并新增 AstrBot 服务入口（`--astrbot-api`）。

参考项目：

- [moeleak/apple-music-downloader-bot](https://github.com/moeleak/apple-music-downloader-bot)
- [zhaarey/apple-music-downloader](https://github.com/zhaarey/apple-music-downloader)

## AstrBot 插件配套说明

如果你是给 AstrBot/NapCat 使用，请搭配插件仓库：

- 插件仓库：[astrbot-plugin-applemusic](https://github.com/wuuduf/astrbot-plugin-applemusic)
- 服务端 AstrBot 文档：[README-ASTRBOT.md](./README-ASTRBOT.md)

核心原则：

1. 插件负责命令/会话/消息发送。
2. 服务端负责 Apple API、下载、解密、转码、缓存、队列。
3. 两者通过 HTTP API 通信，不要把下载核心直接塞进 AstrBot 插件进程。

## 能不能做成“一个插件”？

从工程上可以合包发布，但不建议做成单进程单插件：

1. 下载链路是长耗时重 I/O，放在服务端更容易做队列隔离与故障恢复。
2. 下载核心依赖 Go 与外部工具链（`MP4Box`/`mp4decrypt`），不适合绑定在 Python 插件运行时中。
3. Telegram 与 AstrBot 可复用同一套下载能力，服务化后维护成本更低。

## 使用方法
1. 确保解密程序 [wrapper](https://github.com/WorldObservationLog/wrapper) 正在运行
2. 开始下载部分专辑：`go run . https://music.apple.com/us/album/whenever-you-need-somebody-2022-remaster/1624945511`。
3. 开始下载单曲：`go run . --song https://music.apple.com/us/album/never-gonna-give-you-up-2022-remaster/1624945511?i=1624945512` 或 `go run . https://music.apple.com/us/song/you-move-me-2022-remaster/1624945520`。
4. 开始下载所选曲目：`go run . --select https://music.apple.com/us/album/whenever-you-need-somebody-2022-remaster/1624945511` 输入以空格分隔的数字。
5. 开始下载部分播放列表：`go run . https://music.apple.com/us/playlist/taylor-swift-essentials/pl.3950454ced8c45a3b0cc693c2a7db97b` 或 `go run . https://music.apple.com/us/playlist/hi-res-lossless-24-bit-192khz/pl.u-MDAWvpjt38370N`。
6. 对于杜比全景声 (Dolby Atmos)：`go run . --atmos https://music.apple.com/us/album/1989-taylors-version-deluxe/1713845538`。
7. 对于 AAC (AAC)：`go run . --aac https://music.apple.com/us/album/1989-taylors-version-deluxe/1713845538`。
8. 要查看音质：`go run . --debug https://music.apple.com/us/album/1989-taylors-version-deluxe/1713845538`。

[中文教程-详见方法三](https://telegra.ph/Apple-Music-Alac高解析度无损音乐下载教程-04-02-2)

## Docker
构建镜像：
```
docker build -t apple-music-dl .
```

运行机器人：
```
docker run --rm -it \
  -v "$PWD/config.yaml":/app/config.yaml \
  -v "$PWD/downloads":/downloads \
  -v "$PWD/telegram-cache.json":/app/telegram-cache.json \
  -e TELEGRAM_BOT_TOKEN=你的BotToken \
  apple-music-dl --bot
```

注意：
- 只有启用 `telegram-cache-file` 时才需要挂载 `telegram-cache.json`。
- 机器人使用长轮询，不需要映射端口。
- 不要提交包含真实密钥的 `config.yaml`；仓库默认模板应保持在 `config.example.yaml`。

## Telegram 机器人模式
1. 先执行 `cp config.example.yaml config.yaml`，再在 `config.yaml` 设置 `telegram-bot-token`（或导出 `TELEGRAM_BOT_TOKEN`）。
2. 可选：设置 `telegram-allowed-chat-ids` 限制使用者。
3. 可选：设置 `telegram-api-url` 修改 Telegram API 地址（建议 `https://`；使用 `http://` 会输出安全警告）。
4. 可选：按网络情况调整超时：
   - `telegram-http-timeout-sec`：发送/编辑/上传请求超时（默认 `180`）
   - `telegram-poll-timeout-sec`：`getUpdates` 长轮询超时（默认 `75`，必须大于 `30`）
5. 可选：代理控制（上传慢/节点异常时很有用）：
   - `telegram-proxy-url`：显式指定 Telegram 代理（例如 `http://127.0.0.1:7890`）
   - `telegram-no-proxy`：设为 `true` 可强制直连（忽略环境变量代理）
6. 启动：`go run . --bot`
7. 命令示例：
   - `/chatid`（显示当前会话 `chat_id`，用于配置 `telegram-allowed-chat-ids` 白名单）
   - `/search_song <关键词>`
   - `/search_album <关键词>`
   - `/search_artist <关键词>`
   - `/search <type> <关键词>`（`type`: `song|album|artist`）
   - `/url <apple-music-url>`
   - `/artistphoto <artist-url|artist-id>`（仅下载歌手主页照片）
   - `/cover <apple-music-url>` 或 `/cover <song|album|playlist|station|mv|artist> <id>`（仅下载封面）
   - `/animatedcover <apple-music-url>` 或 `/animatedcover <song|album|playlist|station> <id>`（仅下载动态封面）
   - `/lyrics <song-url|song-id|album-url|album <id>>`（导出歌词文件；格式由设置决定）
   - `/settings [alac|flac|aac|atmos|aac-lc|aac-binaural|aac-downmix|ac3|lrc|ttml|lyrics|cover|animated]`

8. 也支持直接发送 Apple Music 链接，机器人会自动识别：
   - `song`
   - `album`
   - `playlist`
   - `artist`
   - `station`
   - `music-video`

注意：
- 默认格式是 ALAC。`/settings` 已支持 ALAC/FLAC/AAC/Atmos，并可设置 AAC Profile 与 MV 音轨类型。
- `/settings` 也支持歌词格式（`lrc`/`ttml`）与自动附加项开关（`lyrics`/`cover`/`animated`，默认全开）。
- 艺人流程已支持二级选择：`Albums` 或 `Music Videos`。
- Song/Album/Playlist/Station 统一支持 `逐个发送` 与 `ZIP` 两种传输方式。
- song/album/playlist/station 的 ZIP 会缓存 Telegram `file_id`，重复请求可秒传。
- MV 支持优先 `video` 发送、失败回退 `document`，并支持 Telegram `file_id` 缓存复用。
- ZIP 超过 Telegram 限制时会自动回退为逐个发送。
- 下载目录超过限制会自动清理旧文件（默认 3GB，可设置 `telegram-download-max-gb`，不影响 Telegram 缓存）。
- ZIP 临时文件会优先写入下载目录（失败才回退系统临时目录）。可通过 `AMDL_TMPDIR=/path/to/dir` 强制指定临时目录。
- 超过限制的文件会在 FLAC 模式下重新压缩到 `telegram-max-file-mb`（音质可能下降）。
- `/animatedcover` 在目标没有动态封面时会明确提示。
- `/lyrics` 支持 song/album；album 导出支持逐个发送或 ZIP（ZIP 超限自动回退逐个发送）。
- `/lyrics` 遵循 `/settings` 中的歌词格式；`lrc` 在可用时包含翻译，`ttml` 保留翻译与音译信息。
- 旧 ID 命令（`/songid`、`/albumid`、`/playlistid`、`/stationid`、`/mvid`、`/artistid`、`/id`）仍可用，但默认不在 `/help` 展示。
- 如需中文搜索结果，可设置 `telegram-search-language`（例如 `zh-Hans`）或全局 `language`。
- 如需“秒传”复用 Telegram 缓存，可设置 `telegram-cache-file`（缓存 song audio + MV + ZIP 的 file_id）。
- 分享按钮需要在 BotFather 中开启 inline 模式。
- 如果界面长时间停在 `Uploading`，请优先看终端日志；现在会直接打印带文件上下文的网络超时错误。

## 下载歌词

1. 打开 [Apple Music](https://music.apple.com) 并登录
2. 打开开发者工具，点击“应用程序 -> 存储 -> Cookies -> https://music.apple.com”
3. 找到名为“media-user-token”的 Cookie 并复制其值
4. 将步骤 3 中获取的 Cookie 值粘贴到 config.yaml 文件中并保存
5. 正常启动脚本
