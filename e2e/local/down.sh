#!/usr/bin/env bash
# Stops the local GitLab stand.
#
#   e2e/local/down.sh           # stop containers, keep data (up.sh brings it back)
#   e2e/local/down.sh --purge   # also delete volumes (all GitLab data), .env.local and .state
#
# CI job containers started by the runner are removed by the runner itself;
# leftovers can be listed with: docker ps -a --filter label=com.gitlab.gitlab-runner.type
set -euo pipefail
# shellcheck source=lib.sh
. "$(dirname "$0")/lib.sh"

need docker
purge=
case "${1:-}" in
  "") ;;
  --purge) purge=1 ;;
  -h | --help) sed -n '2,9p' "$0"; exit 0 ;;
  *) die "unknown argument: $1 (use --purge)" ;;
esac

if [ -n "$purge" ]; then
  step "Removing containers, network and volumes"
  compose --profile tracing down -v --remove-orphans
  rm -f "$ENV_FILE"
  rm -rf "$STATE_DIR"
  log "removed GitLab volumes, $ENV_FILE and $STATE_DIR"
  log "tokens existed only in this GitLab instance, nothing to revoke elsewhere"
else
  step "Stopping containers (data kept; e2e/local/up.sh starts them again)"
  compose --profile tracing down --remove-orphans
fi
