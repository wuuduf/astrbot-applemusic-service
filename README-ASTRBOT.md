# AstrBot API Mode (`--astrbot-api`)

这个文档用于 AstrBot/NapCat 场景。  
如果你只使用命令行或 Telegram Bot，请看 [`README-CN.md`](./README-CN.md) / [`README.md`](./README.md)。

## 上游来源与定位

- 本项目从 `apple-music-downloader-bot` 体系演进而来，核心下载能力沿用原有 Apple Music 下载逻辑。
- 在此基础上新增了 AstrBot 机器调用接口（`--astrbot-api` + `/v1/*`），用于和 AstrBot 插件解耦集成。
- `--bot`（Telegram）入口仍保留，目标是继续兼容原 Telegram 使用方式。

上游参考：

- [moeleak/apple-music-downloader-bot](https://github.com/moeleak/apple-music-downloader-bot)
- [zhaarey/apple-music-downloader](https://github.com/zhaarey/apple-music-downloader)

## 和插件的关系

- 服务端仓库（本仓库）：下载、解密、转码、缓存、队列、导出资源。
- 插件仓库：[astrbot-plugin-applemusic](https://github.com/wuuduf/astrbot-plugin-applemusic)：命令解析、会话、QQ 消息发送、回推通知。

两者通过 HTTP API 通信，不共享 UI 层逻辑。

## 为什么不合并成“一个 AstrBot 插件”？

可以合并成一个发行包，但不建议把下载核心塞进 AstrBot 插件进程：

1. 运行时差异：AstrBot 插件是 Python，下载核心是 Go + 外部工具链。
2. 任务特性：下载/解密/ZIP 是长耗时和高 I/O，独立服务更容易做队列隔离。
3. 复用需求：同一套下载能力还服务 Telegram，不应绑定单一聊天平台。
4. 运维弹性：服务端可单独扩容、重启、限流，不拖垮聊天机器人主进程。

推荐模式：**两个仓库协同部署**（可同机，也可分机）。

## 快速启动

1. 准备配置：

```bash
cp config.example.yaml config.yaml
# 编辑 config.yaml，填入 Apple 相关配置
```

2. 启动 API：

```bash
go run . --astrbot-api --astrbot-api-listen 127.0.0.1:27198
```

或编译后启动：

```bash
go build -o amdl .
./amdl --astrbot-api --astrbot-api-listen 127.0.0.1:27198
```

## 安全建议（重要）

- 默认建议监听回环地址：`127.0.0.1:27198`。
- 若监听 `0.0.0.0` 或其他非 loopback 地址，必须设置：

```bash
export ASTRBOT_API_TOKEN='replace-with-a-strong-token'
```

然后插件侧配置 `service_token` 为相同值。

服务会拒绝无 token 的非 loopback 监听，这是预期安全行为。

## 健康检查

```bash
curl http://127.0.0.1:27198/healthz
```

`/healthz` 现在会附带基础运行指标（上传成功/失败、retry_after 命中、外部命令超时、清理删除计数），便于做可观测性接入。

## API 列表

- `POST /v1/search`
- `POST /v1/resolve-url`
- `POST /v1/artist-children`
- `POST /v1/download`
- `GET /v1/jobs/{job_id}`
- `POST /v1/artwork`
- `POST /v1/lyrics`

## AstrBot Artifact 治理

`--astrbot-api` 会把导出产物写到系统临时目录下的 `amdl-astrbot-api`。  
现在由后台 janitor 定时清理，不再依赖每次请求触发扫描。

默认策略：

- 最大保留时长：`24h`
- 最大总占用：`2048 MB`
- janitor 间隔：`120s`
- 新文件保护窗口：`120s`（避免误删刚生成/刚返回给上层的文件）

对应配置（`config.yaml`）：

- `astrbot-artifact-max-age-hours`
- `astrbot-artifact-max-size-mb`
- `astrbot-artifact-janitor-interval-sec`
- `astrbot-artifact-protect-sec`

也支持环境变量覆盖：

- `ASTRBOT_ARTIFACT_MAX_AGE_HOURS`
- `ASTRBOT_ARTIFACT_MAX_SIZE_MB`
- `ASTRBOT_ARTIFACT_JANITOR_INTERVAL_SEC`
- `ASTRBOT_ARTIFACT_PROTECT_SEC`

## 常见部署拓扑

### 1) 服务端与 AstrBot 同机（非容器）

- 服务端监听：`127.0.0.1:27198`
- 插件 `service_base_url`：`http://127.0.0.1:27198`

### 2) 服务端在宿主机，AstrBot 在容器

- 服务端监听：`0.0.0.0:27198`（并启用 `ASTRBOT_API_TOKEN`）
- 插件 `service_base_url`：宿主机可达地址（例如 `http://host.docker.internal:27198`）
- 插件 `service_token`：与 `ASTRBOT_API_TOKEN` 一致

## 文件发送路径注意事项

在 NapCat/OneBot 发送本地文件时，读取文件的通常是平台侧进程。  
因此需要满足：

1. AstrBot/NapCat 都能看到同一个下载路径（挂载路径一致）。
2. 运行用户有读取权限（避免 `EACCES`）。
3. 路径真实存在（避免 `ENOENT`）。

## 故障排查

- `All connection attempts failed`
  - 基本是 `service_base_url` 不可达、监听地址不对、容器网络不可达或端口未放行。
- `refusing non-loopback bind ... without ASTRBOT_API_TOKEN`
  - 你在监听非本地地址，但没有设置 token。
- `ENOENT / EACCES` during send file
  - 不是搜索失败，而是文件路径可见性或权限问题。
