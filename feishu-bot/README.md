# Feishu Bot (Go) -> Codex Answer

一个最小可用的飞书（Lark/Feishu）机器人服务：收到用户消息后，调用 Codex（OpenAI 兼容接口）生成回答，并把回答回复到原消息里。

## 功能

- 支持 `text` / `post` / `image` 消息（图片会下载并作为输入交给 Codex 做识别）
- 话题/线程（thread）里 `@` 机器人时，会自动带上同一话题最近若干条消息作为上下文；若当前消息没有图片但话题里最近有图片，也会自动附带最近图片用于理解上下文
- 默认只给用户返回最终结论；处理中会先发一条占位消息，完成后更新为最终答案（可用 `FEISHU_PROGRESS_UPDATES=false` 关闭）

## 目录结构

- `cmd/feishu-bot/main.go`：HTTP 服务入口，注册飞书事件回调
- `internal/bot/`：消息处理逻辑（异步调用 Codex + 回复）
- `internal/codex/`：Codex 客户端（支持 `chat/completions` 或 `responses`）
- `internal/config/`：环境变量配置读取

## 环境变量

必填（飞书）：

- `FEISHU_APP_ID`
- `FEISHU_APP_SECRET`
- `FEISHU_ENCRYPT_KEY`（可选；如果你在飞书控制台启用了加密回调，需要设置；不启用可留空）

可选（飞书）：

- `FEISHU_EVENT_TRANSPORT`：`http`（默认；事件回调）或 `ws`（长连接/WebSocket；无公网 IP 时推荐）
- `FEISHU_VERIFICATION_TOKEN`（URL Verification 的 token 校验；不设置则跳过校验）
- `FEISHU_PROGRESS_UPDATES`：`true/false`（默认 `true`；是否先发送一条“正在处理”的占位回复，并在生成完成后把这条消息更新为最终答案；不会把中间运行数据展示给用户。如果你的应用没有消息编辑权限可设为 `false`）
- `FEISHU_REPLY_FORMAT`：`post`（默认；富文本消息，带可点击链接）/ `markdown`（交互卡片 + lark_md 渲染）/ `text`（纯文本）
- `FEISHU_TIDB_ONLY`：`true/false`（默认 `true`；只回答 TiDB/TiKV/PingCAP 相关问题；非相关问题直接拒绝，避免滥用）

必填（Codex）：

- 当 `CODEX_MODE=cli`（默认）：不需要在这里提供 API Key（`codex` 会读取 `~/.codex/config.toml` 和相关环境变量）
- 当 `CODEX_MODE=api`：需要 `CODEX_API_KEY`

可选：

- `CODEX_LOG_OUTPUT`：`true/false`（默认：跟随 `FEISHU_DEBUG`；是否在机器人后台日志里打印 Codex 的 stdout/stderr 输出。仅用于排障，不会发给用户）
- `LISTEN_ADDR`：默认 `:8080`
- `FEISHU_BASE_URL`：默认 `https://open.feishu.cn`
- `FEISHU_GROUP_MODE`：`mention`（默认）或 `always`
- `FEISHU_DEBUG`：`true/false`
- `CODEX_BASE_URL`：默认 `https://api.openai.com/v1`
- `CODEX_MODEL`：默认 `gpt-5.2-codex`
- `CODEX_API`：`chat`（默认，调用 `/chat/completions`）或 `responses`（调用 `/responses`）
- `CODEX_MODE`：`cli`（默认，shell 调用 `codex exec`）或 `api`
- `CODEX_WORKDIR`：仅 `cli` 模式使用，指定包含 `./doc` 和 `./code` 的目录
- `CODEX_EXEC_PATH`：仅 `cli` 模式使用，默认 `codex`
- `CODEX_BYPASS_APPROVALS_AND_SANDBOX`：仅 `cli` 模式使用，默认 `true`（使用 `codex --dangerously-bypass-approvals-and-sandbox exec ...`，避免执行过程中卡住；但会关闭 Codex 内置 sandbox）
- `CODEX_ISOLATE_DOC_CODE`：仅 `cli` 模式使用，默认 `true`（Linux+root：通过 mount namespace 把工作区隔离成只暴露 `./doc` + `./code` 的视图，避免 Codex 读写其他目录；同时允许在这两个目录内执行必要的 git 版本切换以便查证）
- `CODEX_RUN_AS_USER`：仅 `cli` 模式使用，默认空（可选：通过 `su` 以低权限用户运行 Codex）
- `CODEX_HOME_DIR`：仅 `cli` 模式使用，默认 `/var/lib/feishu-bot-codex-home`（作为 Codex 进程的 `HOME`，需要包含 `~/.codex/config.toml` 和 skills）
- 启动时会将 `feishu-bot/codex-bot-config.toml` 同步到 `CODEX_HOME_DIR/.codex/config.toml`；如果运行时文件与模板不同，会先备份再覆盖
- `CODEX_SANDBOX`：仅 `cli` 模式使用，默认 `read-only`（仅当 `CODEX_BYPASS_APPROVALS_AND_SANDBOX=false` 时生效）
- `CODEX_MAX_TOKENS`：默认 `1024`
- `CODEX_TEMPERATURE`：默认 `0.2`
- `CODEX_TIMEOUT`：默认 `cli=10m`、`api=90s`
- `CODEX_SYSTEM_PROMPT`：系统提示词（默认强制英文单语输出；当 `FEISHU_TIDB_ONLY=true` 时要求仅回答 TiDB 相关问题，非相关请求必须拒绝。需要自定义策略可设置此项）

可选（Metrics / 统计面板）：

- `METRICS_ENABLED`：`true/false`（默认 `true`；是否启用本地统计）
- `METRICS_FILE_PATH`：默认 `metrics.dat`（统计数据文件；建议配置成绝对路径，例如 `/data/metrics.dat`）
- `METRICS_FLUSH_INTERVAL`：默认 `1m`（每分钟落盘一次，减少丢失风险）
- `METRICS_ROTATE_MAX_MB`：默认 `50`（文件超过该大小后 rotate；命名为 `<METRICS_FILE_PATH>.<n>`，例如 `metrics.dat.0`、`metrics.dat.1`）
- `METRICS_PRICE_TOTAL_USD_PER_1M`：总 token 单价（美元 / 1,000,000 tokens；用于用“固定单价 × 总 tokens”估算费用；默认：`(input + output) / 2`）
- `METRICS_PRICE_INPUT_USD_PER_1M`：输入 token 单价（美元 / 1,000,000 tokens，用于估算费用；默认 `1.75`）
- `METRICS_PRICE_CACHED_INPUT_USD_PER_1M`：缓存输入 token 单价（美元 / 1,000,000 tokens；默认 `0.175`）
- `METRICS_PRICE_OUTPUT_USD_PER_1M`：输出 token 单价（美元 / 1,000,000 tokens；默认 `14.0`）

启用后会额外提供：

- Dashboard：`GET /metrics/`（两块面板：请求/失败、token/费用，默认展示最近 24h，10 分钟粒度）
- API：`GET /metrics/api?range=24h&step_sec=600`（返回 `points` 时间序列 + `totals` 区间总量）
- Live：`GET /metrics/live`（实时 gauge，目前提供 `in_flight`：正在处理中的请求数）

如果你希望 Codex 在回答时还能调用你配置的 Feishu MCP（例如检索飞书文档/知识库），`@larksuiteoapi/lark-mcp` 默认读取环境变量 `APP_ID`/`APP_SECRET`。

- `start.sh` 会自动把 `APP_ID`/`APP_SECRET` 兜底设置为 `FEISHU_APP_ID`/`FEISHU_APP_SECRET`（除非你显式设置了 `APP_ID`/`APP_SECRET`）
- 出于安全考虑，`feishu-bot/codex-bot-config.toml` 默认只启用 Feishu MCP 的只读工具（搜索 + 读取文档/知识库节点），不包含导入/权限修改等写入能力
- 如果你看到类似 `MCP startup failed ... initialize response` / `connection closed` 的报错，通常是 Feishu MCP 进程启动后立刻退出（最常见原因：没有正确提供 AppID/AppSecret，或网络/代理导致 npx 拉包失败）。优先检查 `APP_ID`/`APP_SECRET` 是否在 bot 进程环境里可见。

## 运行

```bash
cd feishu-bot
./start.sh
```

健康检查：`GET /healthz`

飞书事件回调（HTTP 模式）：`POST /webhook/event`

## 无公网 IP（推荐：WebSocket 长连接模式）

如果你没有公网 IP / 无法提供飞书服务器可访问的 HTTPS 回调地址，可以用长连接模式接收事件：

1. 设置环境变量：`FEISHU_EVENT_TRANSPORT=ws`
2. 在飞书开放平台控制台里启用 WebSocket/长连接的事件订阅能力（并订阅 `im.message.receive_v1`）
3. 启动机器人：`./start.sh`

此模式下不需要配置 `https://.../webhook/event` 回调地址（仍会启动本地 `:8080` 仅用于 `/healthz`）。

## 飞书侧配置要点（高层提示）

1. 在飞书开放平台创建自建应用，启用机器人能力
2. 订阅事件：`im.message.receive_v1`（接收消息 v2.0）
3. HTTP 模式：事件订阅请求地址填：`https://<你的域名>/webhook/event`（需要公网 HTTPS）
4. WebSocket 模式：按控制台提示启用长连接/WS 事件订阅（不需要回调 URL）
5. 把飞书控制台里对应的 `Verification Token` / `Encrypt Key` 写入环境变量
