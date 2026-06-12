# tidb-ask

`tidb-ask` is a workspace for running a Lark/Feishu bot that answers TiDB,
TiKV, and PingCAP engineering questions through Codex. The bot can use local
documentation and source-code checkouts as grounding material, so answers can be
based on the same files you inspect during development.

The main runnable service lives in [`feishu-bot/`](feishu-bot/).

## What Is Included

- A Go-based Lark/Feishu bot service.
- Text, rich-text, and image message handling.
- Thread-aware context collection for follow-up questions.
- Codex execution through either the local Codex CLI or an OpenAI-compatible API.
- Optional local metrics recording and a small metrics dashboard.
- Codex skills for reading local TiDB documentation and source-code checkouts.

## Repository Layout

```text
.
|-- .codex/              # Local Codex configuration notes and workspace skills
|-- code/                # Source-code Git submodules, grouped by org
|   |-- pingcap/         # Public repo submodules from https://github.com/pingcap
|   `-- tikv/            # Public repo submodules from https://github.com/tikv
|-- doc/                 # Submodule: https://github.com/pingcap/docs.git
`-- feishu-bot/          # Go service that receives Feishu events and calls Codex
```

Important files under `feishu-bot/`:

```text
feishu-bot/
|-- cmd/feishu-bot/main.go        # HTTP/WebSocket service entry point
|-- internal/bot/                 # Feishu message handling and reply logic
|-- internal/codex/               # Codex CLI/API clients
|-- internal/config/              # Environment-variable based configuration
|-- internal/metrics/             # Metrics recorder, API, and dashboard assets
|-- start.sh                      # Local startup script
`-- README.md                     # Detailed bot configuration notes
```

## Git Submodules

`code/` and `doc/` are the local grounding corpus that Codex uses when answering
TiDB, TiKV, and PingCAP questions. They are tracked as Git submodules in this
repository, not copied source snapshots.

Configured submodule groups:

```text
doc                  -> https://github.com/pingcap/docs.git
code/pingcap/<repo>  -> public repositories from https://github.com/pingcap
code/tikv/<repo>     -> public repositories from https://github.com/tikv
```

The full pinned list is stored in [`.gitmodules`](.gitmodules). `pingcap/docs`
is pinned at `doc/`, so it is not duplicated under `code/pingcap/docs`.

After cloning this repository, initialize the submodules with:

```bash
git submodule update --init --recursive
```

To update the checked-out submodule commits intentionally:

```bash
git submodule update --remote --recursive
```

Review and commit the resulting gitlink changes in the parent repository when
you want to pin newer upstream commits.

## Prerequisites

- Go 1.25 or newer, matching `feishu-bot/go.mod`.
- A Lark/Feishu self-built app with bot capability enabled.
- `FEISHU_APP_ID` and `FEISHU_APP_SECRET` for that app.
- For `CODEX_MODE=cli`, the `codex` CLI must be installed and configured.
- For Feishu MCP support in CLI mode, `npx` must be available.
- For source-grounded TiDB answers, initialize the `./doc` and `./code`
  submodules.

## Quick Start

From the repository root:

```bash
cd feishu-bot
./start.sh
```

By default, `start.sh` loads `feishu-bot/env.local` if it exists. You can also
provide an explicit environment file:

```bash
cd feishu-bot
./start.sh --env-file ./env.local
```

To build a binary before running:

```bash
cd feishu-bot
./start.sh --build
```

## Minimal Configuration

At minimum, set the Feishu app credentials:

```bash
export FEISHU_APP_ID="cli_xxx"
export FEISHU_APP_SECRET="xxx"
```

For HTTP webhook mode, configure the Feishu event callback URL as:

```text
https://<your-domain>/webhook/event
```

Useful HTTP endpoints:

```text
GET  /healthz
POST /webhook/event
GET  /metrics/
GET  /metrics/api?range=24h&step_sec=600
GET  /metrics/live
```

If you do not have a public HTTPS callback endpoint, use Feishu WebSocket event
transport:

```bash
export FEISHU_EVENT_TRANSPORT=ws
cd feishu-bot
./start.sh
```

## Codex Modes

The service supports two Codex backends.

`CODEX_MODE=cli` is the default and recommended mode for this workspace. It
shells out to `codex exec`, which allows the bot to use local skills, MCP
servers, and the local `./doc` and `./code` directories.

```bash
export CODEX_MODE=cli
export CODEX_WORKDIR="$(pwd)"
```

`CODEX_MODE=api` calls an OpenAI-compatible HTTP API directly. Use this when you
do not need local filesystem grounding through the Codex CLI.

```bash
export CODEX_MODE=api
export CODEX_API_KEY="sk-..."
```

## Common Environment Variables

Feishu:

- `FEISHU_APP_ID`: Feishu app ID. Required.
- `FEISHU_APP_SECRET`: Feishu app secret. Required.
- `FEISHU_EVENT_TRANSPORT`: `http` or `ws`. Defaults to `http`.
- `FEISHU_VERIFICATION_TOKEN`: Token used for HTTP URL verification.
- `FEISHU_ENCRYPT_KEY`: Encrypt key if encrypted callbacks are enabled.
- `FEISHU_GROUP_MODE`: `mention` or `always`. Defaults to `mention`.
- `FEISHU_REPLY_FORMAT`: `post`, `markdown`, or `text`. Defaults to `post`.
- `FEISHU_TIDB_ONLY`: Whether to refuse non-TiDB questions. Defaults to `true`.

Codex:

- `CODEX_MODE`: `cli` or `api`. Defaults to `cli`.
- `CODEX_WORKDIR`: Workspace containing `./doc` and `./code`.
- `CODEX_EXEC_PATH`: Codex executable path. Defaults to `codex`.
- `CODEX_HOME_DIR`: HOME used by Codex in CLI mode.
- `CODEX_API_KEY`: Required only when `CODEX_MODE=api`.
- `CODEX_MODEL`: Model name for API mode.
- `CODEX_TIMEOUT`: Overall timeout per request.

Metrics:

- `METRICS_ENABLED`: Enables metrics collection. Defaults to `true`.
- `METRICS_FILE_PATH`: Metrics data file path.
- `METRICS_FLUSH_INTERVAL`: Metrics flush interval.
- `METRICS_ROTATE_MAX_MB`: Metrics file rotation threshold.

See [`feishu-bot/README.md`](feishu-bot/README.md) for the full configuration
reference.

## Development

Run tests from the Go service directory:

```bash
cd feishu-bot
go test ./...
```

Run the service locally:

```bash
cd feishu-bot
./start.sh
```

Check the health endpoint:

```bash
curl http://localhost:8080/healthz
```

## Security Notes

- Do not expose `/webhook/event` publicly without Feishu verification and, when
  applicable, encrypted callback validation.
- Keep app credentials, API keys, and runtime environment files out of version
  control.
- In CLI mode, the bot can execute the local `codex` command. Review
  `CODEX_BYPASS_APPROVALS_AND_SANDBOX`, `CODEX_ISOLATE_DOC_CODE`,
  `CODEX_RUN_AS_USER`, and filesystem permissions before production use.
- Treat `./doc` and `./code` as the grounding corpus that Codex will inspect
  when answering TiDB-related questions.
