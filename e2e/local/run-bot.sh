#!/usr/bin/env bash
# Runs reviewbot against the local stand.
#
#   e2e/local/run-bot.sh               # bot on the host: go run ... serve (foreground; Ctrl-C drains:
#                                      # running jobs finish within queue.shutdown_grace, Ctrl-C again cancels them)
#   e2e/local/run-bot.sh --container   # bot in a container (profile "full") on the reviewbot-local network
#   e2e/local/run-bot.sh --stop        # stop the bot container and the egress proxy
#
# Secrets: GitLab ones from e2e/local/.env.local; Claude ones from the file in
# REVIEWBOT_ENV (default: .env in the repository root) - CLAUDE_CODE_OAUTH_TOKEN
# or ANTHROPIC_API_KEY, optionally USAGE_TOKEN, DIAG_TOKEN, MAP_TOKEN,
# METRICS_TOKEN, LOG_LEVEL. Only these names are passed to the bot (the rb-dev /
# rb-guest tokens and the root password are not).
#
# RB_CONFIG: another config, relative to the repository root, host mode only
# (default e2e/local/config.yaml).
#
# Requires the sandbox image: docker build -f Dockerfile.sandbox -t reviewbot-sandbox:latest .
set -euo pipefail
# shellcheck source=lib.sh
. "$(dirname "$0")/lib.sh"

mode=host
case "${1:-}" in
  "") ;;
  --container) mode=container ;;
  --stop) mode=stop ;;
  -h | --help) sed -n '2,18p' "$0"; exit 0 ;;
  *) die "unknown argument: $1" ;;
esac

RB_CONFIG=${RB_CONFIG:-e2e/local/config.yaml}
RB_CONFIG=${RB_CONFIG#./}
if [ "$mode" = container ] && [ "$RB_CONFIG" != e2e/local/config.yaml ]; then
  die "RB_CONFIG is supported only for the bot on the host"
fi
[ "$mode" = stop ] || [ -f "$REPO_ROOT/$RB_CONFIG" ] || die "config $RB_CONFIG not found (relative to the repository root)"

ROOT_COMPOSE=(docker compose -f "$REPO_ROOT/docker-compose.yml" -f "$LOCAL_DIR/compose.bot.yml")

if [ "$mode" = stop ]; then
  (cd "$REPO_ROOT" && "${ROOT_COMPOSE[@]}" --profile full down)
  exit 0
fi

need docker
[ -f "$ENV_FILE" ] || die "$ENV_FILE not found: run e2e/local/up.sh"

# Read the files into shell variables (not exported), then export by name.
# shellcheck disable=SC1090
. "$ENV_FILE"
REVIEWBOT_ENV=${REVIEWBOT_ENV:-$REPO_ROOT/.env}
if [ -f "$REVIEWBOT_ENV" ]; then
  # `sourcing` a malformed line (a space after "=", a bare token) makes the shell
  # run its text as a command and print it, token included: check the format
  # first and name only the line numbers.
  bad=$(grep -nvE '^[[:space:]]*(#.*|[A-Za-z_][A-Za-z0-9_]*=[^[:space:]]*)?[[:space:]]*$' "$REVIEWBOT_ENV" | cut -d: -f1 | tr '\n' ' ' || true)
  [ -z "$bad" ] || die "$REVIEWBOT_ENV: lines $bad are not NAME=value (no spaces around '=', no bare tokens); the values are not shown"
  # shellcheck disable=SC1090
  . "$REVIEWBOT_ENV"
else
  log "note: $REVIEWBOT_ENV not found (set REVIEWBOT_ENV to the file with the Claude token)"
fi
[ -n "${CLAUDE_CODE_OAUTH_TOKEN:-}${ANTHROPIC_API_KEY:-}" ] || die "CLAUDE_CODE_OAUTH_TOKEN or ANTHROPIC_API_KEY is required (in $REVIEWBOT_ENV)"
[ -z "${CLAUDE_CODE_OAUTH_TOKEN:-}" ] || [ -z "${ANTHROPIC_API_KEY:-}" ] || die "set only one of CLAUDE_CODE_OAUTH_TOKEN and ANTHROPIC_API_KEY"
for v in GITLAB_URL GITLAB_TOKEN GITLAB_SERVICE_TOKEN GITLAB_WEBHOOK_SECRET CLAUDE_CODE_OAUTH_TOKEN ANTHROPIC_API_KEY USAGE_TOKEN DIAG_TOKEN MAP_TOKEN METRICS_TOKEN LOG_LEVEL; do
  if [ -n "${!v:-}" ]; then export "${v?}"; fi
done
unset RB_DEV_TOKEN RB_GUEST_TOKEN GITLAB_ROOT_PASSWORD

docker image inspect reviewbot-sandbox:latest >/dev/null 2>&1 ||
  die "image reviewbot-sandbox:latest not found: docker build -f Dockerfile.sandbox -t reviewbot-sandbox:latest ."
docker network inspect reviewbot-local >/dev/null 2>&1 || die "network reviewbot-local not found: run e2e/local/up.sh"

if [ "$mode" = host ]; then
  need go curl
  code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 http://gitlab:8929/users/sign_in || true)
  [ "$code" = 200 ] || die "http://gitlab:8929 is not reachable from the host: add '127.0.0.1 gitlab' to /etc/hosts (sudo) and check up.sh"
  step "Egress proxy (root docker-compose.yml, allowlist from $RB_CONFIG)"
  # The bot runs on the host: the egress MCP forward (review.mcp, on by
  # default) must reach the host's :8091, not the "reviewbot" container.
  (cd "$REPO_ROOT" && REVIEWBOT_CONFIG="./$RB_CONFIG" REVIEWBOT_MCP_TARGET="${REVIEWBOT_MCP_TARGET:-host.docker.internal:8091}" docker compose up -d egress)
  step "reviewbot serve (Ctrl-C to stop); http://localhost:8080/runs /usage /map"
  cd "$REPO_ROOT"
  exec go run ./cmd/reviewbot serve -config "$RB_CONFIG"
fi

# Container: data_dir must be the same absolute path on the host and in the
# container (the bot passes clone paths to the host daemon as -v <dir>:/work).
mkdir -p "$LOCAL_DIR/data" "$STATE_DIR"
chmod 0777 "$LOCAL_DIR/data" # uid 1000 in the container; local stand only
cfg="$STATE_DIR/config.container.yaml"
awk -v dd="$LOCAL_DIR/data" '
  /^data_dir:/ { print "data_dir: \"" dd "\""; next }
  /^    dir: "\.\/e2e\/local\/skills"/ { print "    dir: \"/etc/reviewbot/skills\""; next }
  { print }' "$LOCAL_DIR/config.yaml" >"$cfg"
chmod 0644 "$cfg"
grep -q '/etc/reviewbot/skills' "$cfg" || die "could not rewrite review.skills.dir in $cfg"

if [ -z "${DOCKER_GID:-}" ]; then
  if command -v getent >/dev/null 2>&1 && getent group docker >/dev/null 2>&1; then
    DOCKER_GID=$(getent group docker | cut -d: -f3)
  else
    DOCKER_GID=0 # Docker Desktop (macOS): the socket in containers is root-owned
  fi
fi
export DOCKER_GID
export REVIEWBOT_CONFIG="$cfg" REVIEWBOT_SKILLS="$LOCAL_DIR/skills" REVIEWBOT_DATA_DIR="$LOCAL_DIR/data"
step "reviewbot in a container (profile full) on network reviewbot-local"
cd "$REPO_ROOT"
"${ROOT_COMPOSE[@]}" --profile full up -d --build
log "logs: docker compose -f docker-compose.yml -f e2e/local/compose.bot.yml logs -f reviewbot"
log "webhook URL for GitLab (events: webhook/both): http://reviewbot:8080/webhook"
