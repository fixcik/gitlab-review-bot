# shellcheck shell=bash
# Shared helpers of e2e/local scripts. Sourced, not executed.
#
# Portable to macOS (bash 3.2, BSD userland) and Linux: no associative
# arrays, no `sed -i`, no GNU-only flags.
#
# Secrets: tokens are passed to curl through a process-substitution file
# (`-H @<(printf ...)`, printf is a builtin, so they never appear in `ps`),
# to git through GIT_ASKPASS reading an environment variable, and they are
# never printed.

LOCAL_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd "$LOCAL_DIR/../.." && pwd)
ENV_FILE="$LOCAL_DIR/.env.local"
STATE_DIR="$LOCAL_DIR/.state"
COMPOSE_FILE="$LOCAL_DIR/docker-compose.yml"

# Host-side URL for the setup scripts (does not need the /etc/hosts entry).
# An explicit GITLAB_HOST_URL in the environment wins over .env.local.
RB_HOST_URL_ENV=${GITLAB_HOST_URL:-}
GITLAB_HOST_URL=${GITLAB_HOST_URL:-http://localhost:${GITLAB_PORT:-8929}}
GITLAB_API="$GITLAB_HOST_URL/api/v4"

log() { printf '%s\n' "$*" >&2; }
step() { printf '\n==> %s\n' "$*" >&2; }
die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

compose() { docker compose -f "$COMPOSE_FILE" "$@"; }

need() {
  local c
  for c in "$@"; do
    command -v "$c" >/dev/null 2>&1 || die "'$c' is required (see e2e/local/README.md)"
  done
}

load_env() {
  [ -f "$ENV_FILE" ] || die "$ENV_FILE not found: run e2e/local/up.sh first"
  set -a
  # shellcheck disable=SC1090
  . "$ENV_FILE"
  set +a
  if [ -n "$RB_HOST_URL_ENV" ]; then GITLAB_HOST_URL=$RB_HOST_URL_ENV; fi
  GITLAB_API="$GITLAB_HOST_URL/api/v4"
}

# urlencode <string>: path segments for /projects/:id and /groups/:id.
urlencode() { jq -rn --arg s "$1" '$s|@uri'; }

# api <token> <METHOD> <path> [curl args...]
# Sets API_STATUS (HTTP code) and API_BODY. Never fails by itself.
API_STATUS=0
API_BODY=
api() {
  local tok=$1 method=$2 path=$3 out
  shift 3
  out=$(curl -sS -X "$method" \
    -H @<(printf 'PRIVATE-TOKEN: %s\n' "$tok") \
    -w '\n%{http_code}' "$@" "$GITLAB_API$path" 2>/dev/null) || true
  API_STATUS=${out##*$'\n'}
  API_BODY=${out%$'\n'*}
  case $API_STATUS in '' | *[!0-9]*) API_STATUS=0 ;; esac
}

# api_json <token> <METHOD> <path> <json>
api_json() {
  local tok=$1 method=$2 path=$3 body=$4
  api "$tok" "$method" "$path" -H 'Content-Type: application/json' --data-binary "$body"
}

api_ok() { [ "$API_STATUS" -ge 200 ] && [ "$API_STATUS" -lt 300 ]; }

# api_message: GitLab's error text of the last call (no secrets in it).
api_message() { printf '%s' "$API_BODY" | jq -r '(.message // .error // .) | tostring' 2>/dev/null | head -c 400; }

# git with the token from RB_GIT_TOKEN supplied by GIT_ASKPASS. The empty
# credential.helper resets the helper list, so the token is never stored in a
# credential helper (osxkeychain is configured by default on macOS).
git_auth() {
  GIT_ASKPASS="$LOCAL_DIR/askpass.sh" GIT_TERMINAL_PROMPT=0 git -c credential.helper= "$@"
}

# Portable in-place substitution: subst_file <file> <placeholder> <value>
subst_file() {
  local f=$1 from=$2 to=$3 tmp
  tmp=$(mktemp "${f}.XXXXXX")
  awk -v from="$from" -v to="$to" '{
    out = ""; s = $0
    while ((i = index(s, from)) > 0) { out = out substr(s, 1, i - 1) to; s = substr(s, i + length(from)) }
    print out s
  }' "$f" >"$tmp" && cat "$tmp" >"$f"
  rm -f "$tmp"
}
