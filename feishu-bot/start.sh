#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
Usage:
  ./start.sh [--env-file PATH] [--build]

Options:
  --env-file PATH   Load environment variables from a file (default: ./env.local if exists, else none)
  --build           Build a binary and run it (default: go run)

Required env vars (Feishu):
  FEISHU_APP_ID
  FEISHU_APP_SECRET
  FEISHU_ENCRYPT_KEY (optional; only required if callback encryption enabled)

Optional env vars (Feishu):
  FEISHU_EVENT_TRANSPORT (default: http)  -> http: webhook callback; ws: websocket (no public IP required)
  FEISHU_VERIFICATION_TOKEN (optional; only used for http URL verification token check)
  FEISHU_PROGRESS_UPDATES (default: true) -> send a placeholder "running" reply, then replace it with the final answer (no intermediate output shown to users)
  FEISHU_PROCESSING_INDICATOR (default: message) -> message: placeholder reply/update; reaction: add/remove an emoji reaction on the user's message while processing (no placeholder message)
  FEISHU_PROCESSING_REACTION_EMOJI_TYPE (default: THINKING) -> Feishu reaction emoji_type (fallback tries THINKING/DONE if the configured type fails)
  FEISHU_REPLY_FORMAT (default: post)     -> text: plain text; post: rich text (Feishu "post"); markdown: interactive card with lark_md
  FEISHU_TIDB_ONLY (default: true)        -> when true, refuse non-TiDB questions to avoid abuse
  CODEX_LOG_OUTPUT (default: FEISHU_DEBUG) -> print Codex stdout/stderr to backend logs for debugging (never shown to users)

Codex mode:
  CODEX_MODE=cli (default)  -> shell out to `codex exec` (uses local skills/MCP and ./doc + ./code)
  CODEX_MODE=api            -> call OpenAI-compatible HTTP API directly

If CODEX_MODE=cli (recommended):
  CODEX_EXEC_PATH (default: codex)
  CODEX_BYPASS_APPROVALS_AND_SANDBOX (default: true) -> run `codex --dangerously-bypass-approvals-and-sandbox exec ...`
  CODEX_ISOLATE_DOC_CODE (default: true) -> Linux+root: isolate workspace view to only ./doc and ./code via mount namespace (allows git-based version switching inside them)
  CODEX_RUN_AS_USER (default: empty) -> optionally run codex under this OS user via `su` (Linux only; requires root)
  CODEX_HOME_DIR    (default: /var/lib/feishu-bot-codex-home) -> HOME for codex (must contain .codex/config.toml & skills)
  CODEX_SANDBOX     (default: read-only; only used when bypass=false)
  CODEX_WORKDIR   (default: parent dir of feishu-bot/, i.e. workspace containing ./doc and ./code)
  CODEX_TIMEOUT  (default: cli=10m, api=90s) -> overall time budget per question

If CODEX_MODE=api:
  CODEX_API_KEY (required)
  CODEX_BASE_URL / CODEX_MODEL / CODEX_API ... (optional; see README.md)
EOF
}

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BOT_DIR="$SCRIPT_DIR"
WORKSPACE_DIR="$(cd "$BOT_DIR/.." && pwd)"

ENV_FILE=""
BUILD="0"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --env-file)
      ENV_FILE="${2:-}"
      shift 2
      ;;
    --build)
      BUILD="1"
      shift 1
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "[ERROR] unknown argument: $1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

if [[ -z "$ENV_FILE" && -f "$BOT_DIR/env.local" ]]; then
  ENV_FILE="$BOT_DIR/env.local"
fi

if [[ -n "$ENV_FILE" ]]; then
  if [[ ! -f "$ENV_FILE" ]]; then
    echo "[ERROR] env file not found: $ENV_FILE" >&2
    exit 2
  fi
  # shellcheck disable=SC1090
  set -a
  source "$ENV_FILE"
  set +a
fi

export CODEX_MODE="${CODEX_MODE:-cli}"
export CODEX_EXEC_PATH="${CODEX_EXEC_PATH:-codex}"
export CODEX_SANDBOX="${CODEX_SANDBOX:-read-only}"
export CODEX_BYPASS_APPROVALS_AND_SANDBOX="${CODEX_BYPASS_APPROVALS_AND_SANDBOX:-true}"
export CODEX_ISOLATE_DOC_CODE="${CODEX_ISOLATE_DOC_CODE:-true}"
export CODEX_RUN_AS_USER="${CODEX_RUN_AS_USER:-}"
export CODEX_HOME_DIR="${CODEX_HOME_DIR:-/var/lib/feishu-bot-codex-home}"
export CODEX_WORKDIR="${CODEX_WORKDIR:-$WORKSPACE_DIR}"
export FEISHU_EVENT_TRANSPORT="${FEISHU_EVENT_TRANSPORT:-http}"

mask() {
  local s="${1:-}"
  if [[ -z "$s" ]]; then
    echo ""
    return
  fi
  if [[ ${#s} -le 6 ]]; then
    echo "***"
    return
  fi
  echo "${s:0:3}***${s: -3}"
}

require_env() {
  local name="$1"
  if [[ -z "${!name:-}" ]]; then
    echo "[ERROR] missing env var: $name" >&2
    exit 2
  fi
}

require_cmd() {
  local bin="$1"
  if ! command -v "$bin" >/dev/null 2>&1; then
    echo "[ERROR] missing command in PATH: $bin" >&2
    exit 2
  fi
}

require_cmd go

require_env FEISHU_APP_ID
require_env FEISHU_APP_SECRET
if [[ "${FEISHU_EVENT_TRANSPORT:-http}" == "http" && -z "${FEISHU_VERIFICATION_TOKEN:-}" ]]; then
  echo "[WARN] FEISHU_VERIFICATION_TOKEN is not set; URL verification token check will be skipped (http transport)." >&2
fi

# If you want Codex to use the Feishu MCP server (via `npx @larksuiteoapi/lark-mcp ...`),
# it expects APP_ID / APP_SECRET. Default them to the bot app credentials unless explicitly set.
export APP_ID="${APP_ID:-$FEISHU_APP_ID}"
export APP_SECRET="${APP_SECRET:-$FEISHU_APP_SECRET}"

if [[ "$CODEX_MODE" == "cli" ]]; then
  require_cmd "$CODEX_EXEC_PATH"
  require_cmd npx

  if [[ ! -d "$CODEX_WORKDIR/doc" || ! -d "$CODEX_WORKDIR/code" ]]; then
    echo "[ERROR] CODEX_WORKDIR must contain ./doc and ./code directories (CODEX_WORKDIR=$CODEX_WORKDIR)" >&2
    exit 2
  fi

  # If isolation or su-user execution is enabled, we need root.
  if [[ "$CODEX_ISOLATE_DOC_CODE" == "true" ]]; then
    if [[ "$(id -u)" -ne 0 ]]; then
      echo "[ERROR] CODEX_ISOLATE_DOC_CODE=true requires running start.sh as root (mount namespace + bind mounts)" >&2
      exit 2
    fi
    require_cmd unshare
    require_cmd mount
    require_cmd umount
  fi
  if [[ -n "${CODEX_RUN_AS_USER:-}" && "${CODEX_RUN_AS_USER:-}" != "$(id -un)" ]]; then
    if [[ "$(id -u)" -ne 0 ]]; then
      echo "[ERROR] CODEX_RUN_AS_USER=$CODEX_RUN_AS_USER requires running start.sh as root (so the bot can use su)" >&2
      exit 2
    fi
  fi

  # Prepare a dedicated Codex HOME so the bot can run with a minimal config and
  # only the required skills/MCP servers (best-effort).
  if [[ -n "${CODEX_HOME_DIR:-}" ]]; then
    mkdir -p "$CODEX_HOME_DIR/.codex"
    CODEX_CONFIG_DST="$CODEX_HOME_DIR/.codex/config.toml"
    CODEX_CONFIG_SRC="$BOT_DIR/codex-bot-config.toml"
    if [[ ! -f "$CODEX_CONFIG_DST" ]]; then
      echo "[INFO] Installing minimal Codex config at: $CODEX_CONFIG_DST"
      cp "$CODEX_CONFIG_SRC" "$CODEX_CONFIG_DST"
    elif ! cmp -s "$CODEX_CONFIG_SRC" "$CODEX_CONFIG_DST"; then
      ts="$(date +%Y%m%d-%H%M%S)"
      echo "[INFO] Syncing Codex config template to runtime config (backup: $CODEX_CONFIG_DST.bak.$ts)"
      cp "$CODEX_CONFIG_DST" "$CODEX_CONFIG_DST.bak.$ts"
      cp "$CODEX_CONFIG_SRC" "$CODEX_CONFIG_DST"
    fi

    mkdir -p "$CODEX_HOME_DIR/.codex/skills"
    SKILLS_SRC="${SKILLS_SRC:-$HOME/.codex/skills}"
    for skill in read-local-doc read-local-code; do
      if [[ -d "$SKILLS_SRC/$skill" ]]; then
        rm -rf "$CODEX_HOME_DIR/.codex/skills/$skill"
        cp -a "$SKILLS_SRC/$skill" "$CODEX_HOME_DIR/.codex/skills/$skill"
      fi
    done

    chmod -R u+rwX,go-rwx "$CODEX_HOME_DIR"

    if [[ -n "${CODEX_RUN_AS_USER:-}" && "${CODEX_RUN_AS_USER:-}" != "$(id -un)" ]]; then
      if ! chown -R "$CODEX_RUN_AS_USER":"$CODEX_RUN_AS_USER" "$CODEX_HOME_DIR" 2>/dev/null; then
        grp="$(id -gn "$CODEX_RUN_AS_USER" 2>/dev/null || true)"
        if [[ -n "$grp" ]]; then
          chown -R "$CODEX_RUN_AS_USER":"$grp" "$CODEX_HOME_DIR" 2>/dev/null || chown -R "$CODEX_RUN_AS_USER" "$CODEX_HOME_DIR"
        else
          chown -R "$CODEX_RUN_AS_USER" "$CODEX_HOME_DIR"
        fi
      fi

      # Verify the target user can run Codex with this HOME (fast sanity check).
      su -s /bin/bash "$CODEX_RUN_AS_USER" -c "HOME=$CODEX_HOME_DIR \"$CODEX_EXEC_PATH\" --version" >/dev/null
    else
      # Root-owned HOME for root-run codex.
      chown -R "$(id -un)":"$(id -gn)" "$CODEX_HOME_DIR" 2>/dev/null || true
    fi
  fi
elif [[ "$CODEX_MODE" == "api" ]]; then
  require_env CODEX_API_KEY
else
  echo "[ERROR] invalid CODEX_MODE=$CODEX_MODE (expected cli or api)" >&2
  exit 2
fi

echo "[INFO] Starting feishu-bot"
echo "  - BOT_DIR:      $BOT_DIR"
echo "  - LISTEN_ADDR:  ${LISTEN_ADDR:-:8080}"
echo "  - FEISHU_APP_ID: $(mask "${FEISHU_APP_ID:-}")"
echo "  - FEISHU_EVENT_TRANSPORT: ${FEISHU_EVENT_TRANSPORT:-http}"
echo "  - FEISHU_PROGRESS_UPDATES: ${FEISHU_PROGRESS_UPDATES:-true}"
echo "  - CODEX_LOG_OUTPUT: ${CODEX_LOG_OUTPUT:-<follow FEISHU_DEBUG>}"
echo "  - CODEX_MODE:   $CODEX_MODE"
if [[ -n "${CODEX_TIMEOUT:-}" ]]; then
  echo "  - CODEX_TIMEOUT: $CODEX_TIMEOUT"
else
  if [[ "$CODEX_MODE" == "cli" ]]; then
    echo "  - CODEX_TIMEOUT: (default) 10m"
  else
    echo "  - CODEX_TIMEOUT: (default) 90s"
  fi
fi
if [[ "$CODEX_MODE" == "cli" ]]; then
  echo "  - CODEX_EXEC_PATH: $CODEX_EXEC_PATH"
  echo "  - CODEX_BYPASS_APPROVALS_AND_SANDBOX: $CODEX_BYPASS_APPROVALS_AND_SANDBOX"
  echo "  - CODEX_ISOLATE_DOC_CODE: $CODEX_ISOLATE_DOC_CODE"
  echo "  - CODEX_RUN_AS_USER: $CODEX_RUN_AS_USER"
  echo "  - CODEX_HOME_DIR:   $CODEX_HOME_DIR"
  echo "  - CODEX_SANDBOX:   $CODEX_SANDBOX"
  echo "  - CODEX_WORKDIR:   $CODEX_WORKDIR"
fi

cd "$BOT_DIR"

if [[ "$BUILD" == "1" ]]; then
  mkdir -p "$BOT_DIR/bin"
  BIN="$BOT_DIR/bin/feishu-bot"
  echo "[INFO] Building binary: $BIN"
  go build -o "$BIN" ./cmd/feishu-bot
  echo "[INFO] Running: $BIN"
  exec "$BIN"
fi

echo "[INFO] Running: go run ./cmd/feishu-bot"
exec go run ./cmd/feishu-bot
