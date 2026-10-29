#!/usr/bin/env bash
# Seeds the local GitLab with the test projects.
#
#   e2e/local/seed.sh                      # create missing projects (existing ones are skipped)
#   e2e/local/seed.sh --reset              # delete and re-create (all, or --only ones)
#   e2e/local/seed.sh --only rb-py,rb-php  # just these projects
#   e2e/local/seed.sh --no-mrs             # push branches, do not open MRs
#   e2e/local/seed.sh --open rb-web:feat/order-history-view   # open the MR of a pushed branch
#
# For each project: fixtures/<project>/base/ is pushed as `main`, then every
# fixtures/<project>/branches/<x>/ (extra non-MR branches, .branch.json) and
# fixtures/<project>/mr/<x>/ (overlay + .mr.json) becomes a branch with one
# commit; MRs with "open": false are pushed but opened live during the session
# (seed.sh --open). The scenario of every MR: e2e/local/SCENARIOS.md.
#
# Environment: RB_BIG_MB (default 24) - size of generated data in rb-big
# (random base64, so the repository stays bigger than review.context.max_repo_mb: 10).
#
# Requires e2e/local/.env.local (up.sh). Nothing secret is written to git or stdout.
set -euo pipefail
# shellcheck source=lib.sh
. "$(dirname "$0")/lib.sh"

need curl jq git awk iconv
FIXTURES="$LOCAL_DIR/fixtures"
RB_BIG_MB=${RB_BIG_MB:-24}

# project              group                   threads must be resolved
PROJECTS="
rb-backend             reviewbot-sandbox       true
rb-frontend            reviewbot-sandbox       true
rb-py                  reviewbot-sandbox       false
rb-php                 reviewbot-sandbox       true
rb-consumer            reviewbot-sandbox       true
rb-big                 reviewbot-sandbox       true
rb-orders-py           reviewbot-sandbox       true
rb-web                 reviewbot-sandbox       true
rb-billing-php         reviewbot-sandbox       true
rb-mig-doctrine        reviewbot-sandbox       true
rb-mig-laravel         reviewbot-sandbox       true
rb-mig-alembic         reviewbot-sandbox       true
rb-mig-django          reviewbot-sandbox       true
rb-mig-sql             reviewbot-sandbox       true
rb-excluded            reviewbot-sandbox       true
rb-noconfig            reviewbot-sandbox       true
ext-client             reviewbot-sandbox-ext   false
ext-secret             reviewbot-sandbox-ext   false
"

reset=
only=
no_mrs=
open_spec=
while [ $# -gt 0 ]; do
  case $1 in
    --reset) reset=1 ;;
    --only) shift; only=${1:-} ;;
    --only=*) only=${1#--only=} ;;
    --no-mrs) no_mrs=1 ;;
    --open) shift; open_spec=${1:-} ;;
    -h | --help) sed -n '2,20p' "$0"; exit 0 ;;
    *) die "unknown argument: $1 (see --help)" ;;
  esac
  shift
done

load_env

wanted() {
  [ -z "$only" ] && return 0
  case ",$only," in *",$1,"*) return 0 ;; esac
  return 1
}

token_of() {
  case $1 in
    rb-dev) printf '%s' "$RB_DEV_TOKEN" ;;
    rb-guest) printf '%s' "$RB_GUEST_TOKEN" ;;
    root | "") printf '%s' "$GITLAB_SERVICE_TOKEN" ;;
    *) die "unknown MR author: $1" ;;
  esac
}

identity_of() {
  case $1 in
    rb-dev) printf 'RB Developer|rb-dev@example.com' ;;
    rb-guest) printf 'RB Guest|rb-guest@example.com' ;;
    *) printf 'Reviewbot Seed|seed@example.com' ;;
  esac
}

project_id_of() { # project_id_of <group/path> -> echoes id or nothing
  api "$GITLAB_SERVICE_TOKEN" GET "/projects/$(urlencode "$1")"
  if api_ok; then printf '%s' "$API_BODY" | jq -r .id; fi
}

delete_project() { # delete_project <group/path>
  local full=$1 id cur i
  id=$(project_id_of "$full")
  [ -n "$id" ] || return 0
  log "  deleting $full (id $id)"
  api "$GITLAB_SERVICE_TOKEN" DELETE "/projects/$id"
  api_ok || die "DELETE $full: HTTP $API_STATUS $(api_message)"
  # GitLab 18 marks projects for deletion first (renamed path); remove it for good.
  api "$GITLAB_SERVICE_TOKEN" GET "/projects/$id"
  if api_ok; then
    cur=$(printf '%s' "$API_BODY" | jq -r .path_with_namespace)
    api "$GITLAB_SERVICE_TOKEN" DELETE "/projects/$id" \
      -G --data-urlencode permanently_remove=true --data-urlencode "full_path=$cur"
    api_ok || log "  permanent removal: HTTP $API_STATUS $(api_message) (continuing)"
  fi
  for i in $(seq 1 60); do
    api "$GITLAB_SERVICE_TOKEN" GET "/projects/$(urlencode "$full")"
    [ "$API_STATUS" = 404 ] && return 0
    sleep 3
  done
  die "$full still exists after deletion; delete it in the UI (Admin -> Projects) and re-run"
}

# Copies an overlay directory into the current directory, without the
# control files (.mr.json, .branch.json, .base.json).
overlay() {
  (cd "$1" && tar --exclude .mr.json --exclude .branch.json --exclude .base.json -cf - .) | tar -xf -
}

# Applies the "delete", "symlinks" and "generate" keys of a control file.
apply_controls() {
  local ctl=$1 p kind bytes text target
  [ -f "$ctl" ] || return 0
  jq -r '.delete[]?' "$ctl" | while IFS= read -r p; do
    if [ -n "$p" ]; then git rm -r -q --ignore-unmatch -- "$p" >/dev/null; fi
  done
  jq -r '(.symlinks // {}) | to_entries[] | "\(.key)\t\(.value)"' "$ctl" | while IFS="$(printf '\t')" read -r p target; do
    mkdir -p "$(dirname "$p")"
    rm -rf "$p"
    ln -s "$target" "$p"
  done
  jq -c '.generate[]?' "$ctl" | while IFS= read -r item; do
    kind=$(printf '%s' "$item" | jq -r .kind)
    p=$(printf '%s' "$item" | jq -r .path)
    mkdir -p "$(dirname "$p")"
    case $kind in
      random_binary)
        bytes=$(printf '%s' "$item" | jq -r '.bytes // 1024')
        head -c "$bytes" /dev/urandom >"$p" ;;
      latin1)
        text=$(printf '%s' "$item" | jq -r .text)
        printf '%s\n' "$text" | iconv -f UTF-8 -t ISO-8859-1 >"$p" ;;
      no_newline)
        text=$(printf '%s' "$item" | jq -r .text)
        printf '%s' "$text" >"$p" ;;
      lines)
        # head, then count lines of the "line" template ({n} = 1..count), then tail
        # (a big generated-looking file that no ignore glob matches; S7).
        {
          printf '%s' "$item" | jq -r '.head // empty'
          seq 1 "$(printf '%s' "$item" | jq -r .count)" | awk -v t="$(printf '%s' "$item" | jq -r .line)" \
            '{ s = t; out = ""; while ((i = index(s, "{n}")) > 0) { out = out substr(s, 1, i - 1) $1; s = substr(s, i + 3) } print out s }'
          printf '%s' "$item" | jq -r '.tail // empty'
        } >"$p" ;;
      *) die "unknown generate kind '$kind' in $ctl" ;;
    esac
  done
}

# Replaces placeholders of fake secrets (kept out of this repository so that
# secret scanners of the bot's own repo stay quiet) in the working tree.
FAKE_AWS_KEY=
replace_placeholders() {
  local f
  if [ -z "$FAKE_AWS_KEY" ]; then
    # Matches gitleaks' aws-access-token rule (AKIA + 16 of [A-Z2-7]); random,
    # not a real key, and without the EXAMPLE suffix gitleaks allowlists.
    # (Finite input: no SIGPIPE under pipefail.)
    FAKE_AWS_KEY="AKIA$(head -c 2048 /dev/urandom | LC_ALL=C tr -dc 'A-Z2-7' | cut -c1-16)"
    [ ${#FAKE_AWS_KEY} -eq 20 ] || die "could not generate the fake key"
  fi
  { grep -rl --exclude-dir=.git '__RB_FAKE_AWS_ACCESS_KEY_ID__' . 2>/dev/null || true; } | while IFS= read -r f; do
    subst_file "$f" '__RB_FAKE_AWS_ACCESS_KEY_ID__' "$FAKE_AWS_KEY"
  done
}

commit_as() { # commit_as <author> <message> [age_days] [allow_empty]
  local who=$1 msg=$2 age=${3:-0} empty=${4:-false} id name email date= flags="-q --no-verify"
  id=$(identity_of "$who")
  name=${id%%|*}
  email=${id#*|}
  if [ "$age" -gt 0 ] 2>/dev/null; then
    date="$(($(date +%s) - age * 86400)) +0000"
  fi
  git add -A
  if git rev-parse -q --verify HEAD >/dev/null && git diff --cached --quiet; then
    # Nothing changed (a plain branch such as "release"): no commit unless asked (C12).
    [ "$empty" = true ] || return 0
    flags="$flags --allow-empty"
  fi
  if [ -n "$date" ]; then
    # shellcheck disable=SC2086
    GIT_AUTHOR_DATE=$date GIT_COMMITTER_DATE=$date \
      git -c user.name="$name" -c user.email="$email" commit $flags -m "$msg"
  else
    # shellcheck disable=SC2086
    git -c user.name="$name" -c user.email="$email" commit $flags -m "$msg"
  fi
}

push() { # push <branch...>
  local plog
  plog=$(mktemp "${TMPDIR:-/tmp}/rb-push.XXXXXX")
  if ! RB_GIT_USER=root RB_GIT_TOKEN=$GITLAB_SERVICE_TOKEN \
    git_auth -c http.postBuffer=524288000 push -q origin "$@" >"$plog" 2>&1; then
    grep -v '^remote: *$' "$plog" | tail -n 20 >&2
    rm -f "$plog"
    die "git push $* failed"
  fi
  rm -f "$plog"
}

open_mr() { # open_mr <project_id> <full_path> <ctl>
  local pid=$1 full=$2 ctl=$3 branch target title author draft scen desc body i
  branch=$(jq -r .branch "$ctl")
  target=$(jq -r '.target // "main"' "$ctl")
  title=$(jq -r .title "$ctl")
  author=$(jq -r '.author // "rb-dev"' "$ctl")
  draft=$(jq -r '.draft // false' "$ctl")
  scen=$(jq -r '.scenarios // ""' "$ctl")
  desc=$(jq -r '.description // ""' "$ctl")
  [ "$draft" = true ] && title="Draft: $title"
  api "$(token_of "$author")" GET "/projects/$pid/merge_requests" -G \
    --data-urlencode state=opened --data-urlencode "source_branch=$branch"
  if api_ok && [ "$(printf '%s' "$API_BODY" | jq 'length')" != 0 ]; then
    log "    MR for $branch already open: !$(printf '%s' "$API_BODY" | jq -r '.[0].iid')"
    return 0
  fi
  body=$(jq -n --arg s "$branch" --arg t "$target" --arg title "$title" \
    --arg d "Scenario: $scen

$desc" '{source_branch: $s, target_branch: $t, title: $title, description: $d, remove_source_branch: false}')
  for i in 1 2 3 4 5; do
    api_json "$(token_of "$author")" POST "/projects/$pid/merge_requests" "$body"
    api_ok && break
    sleep 2
  done
  api_ok || die "open MR $full $branch: HTTP $API_STATUS $(api_message)"
  log "    MR !$(printf '%s' "$API_BODY" | jq -r .iid) $branch -> $target ($scen) by $author"
}

generate_big() {
  local n i
  n=$((RB_BIG_MB / 2))
  [ "$n" -ge 1 ] || n=1
  mkdir -p data
  for i in $(seq 1 "$n"); do
    # 1.5 MiB random -> 2 MiB base64 text; random data does not compress in git.
    head -c 1572864 /dev/urandom | base64 | fold -w 100 >"data/chunk-$(printf '%03d' "$i").txt"
  done
  log "  generated ${n} x 2 MiB in data/ (RB_BIG_MB=$RB_BIG_MB)"
}

push_branch_overlay() { # push_branch_overlay <dir> <ctl> <default author>
  local dir=$1 ctl=$2 branch from author age msg empty
  branch=$(jq -r .branch "$ctl")
  empty=$(jq -r '.allow_empty // false' "$ctl")
  from=$(jq -r '.from // "main"' "$ctl")
  author=$(jq -r --arg a "$3" '.author // $a' "$ctl")
  age=$(jq -r '.commit_age_days // 0' "$ctl")
  msg=$(jq -r '.commit_message // .title // .branch' "$ctl")
  git checkout -q -B "$branch" "$from"
  overlay "$dir"
  apply_controls "$ctl"
  replace_placeholders
  commit_as "$author" "$msg" "$age" "$empty"
  push "$branch"
}

seed_project() { # seed_project <name> <group> <resolve>
  local name=$1 group=$2 resolve=$3 full=$2/$1 fx="$FIXTURES/$1" pid gid work d ctl n i pass behind
  [ -d "$fx/base" ] || die "no fixtures for $name ($fx/base)"
  step "$full"
  [ -n "$reset" ] && delete_project "$full"

  pid=$(project_id_of "$full")
  if [ -n "$pid" ]; then
    log "  exists (id $pid), skipped (--reset to re-create)"
  else
    api "$GITLAB_SERVICE_TOKEN" GET "/groups/$(urlencode "$group")"
    api_ok || die "group $group not found (did up.sh run?)"
    gid=$(printf '%s' "$API_BODY" | jq -r .id)
    api_json "$GITLAB_SERVICE_TOKEN" POST /projects "$(jq -n --arg n "$name" --argjson g "$gid" \
      '{name: $n, path: $n, namespace_id: $g, visibility: "private", initialize_with_readme: false,
        auto_devops_enabled: false}')"
    api_ok || die "create $full: HTTP $API_STATUS $(api_message)"
    pid=$(printf '%s' "$API_BODY" | jq -r .id)
    log "  created (id $pid)"
    # up.sh turns Auto DevOps off instance-wide, but GitLab caches settings for a
    # while: the first pushes of a fresh stand started red Auto DevOps pipelines.

    work=$(mktemp -d "${TMPDIR:-/tmp}/rb-seed.XXXXXX")
    (
      cd "$work"
      git init -q
      git checkout -q -b main
      git remote add origin "$GITLAB_HOST_URL/$full.git"
      overlay "$fx/base"
      apply_controls "$fx/base/.base.json"
      if [ "$name" = rb-big ]; then generate_big; fi
      replace_placeholders
      commit_as seed "Initial import"
      push main

      for d in "$fx"/branches/*/; do
        [ -f "$d.branch.json" ] || continue
        push_branch_overlay "$d" "$d.branch.json" seed
        log "  branch $(jq -r .branch "$d.branch.json")"
      done

      # Branches that need main to move on afterwards (S2: "N commits behind")
      # go first, so that the other MRs start from the final main.
      for pass in behind rest; do
        for d in "$fx"/mr/*/; do
          ctl="$d.mr.json"
          [ -f "$ctl" ] || continue
          behind=$(jq -r '.base_commits_after // 0' "$ctl")
          if [ "$pass" = behind ] && [ "$behind" -eq 0 ]; then continue; fi
          if [ "$pass" = rest ] && [ "$behind" -gt 0 ]; then continue; fi
          push_branch_overlay "$d" "$ctl" rb-dev
          log "  branch $(jq -r .branch "$ctl")"
          if [ "$behind" -gt 0 ]; then
            git checkout -q main
            for i in $(seq 1 "$behind"); do
              printf -- '- maintenance %s\n' "$i" >>CHANGELOG.md
              commit_as seed "chore: maintenance $i"
            done
            push main
            log "  main moved $behind commits ahead"
          fi
        done
      done
      git checkout -q main
    )
    rm -rf "$work"

    if [ -z "$no_mrs" ]; then
      for d in "$fx"/mr/*/; do
        ctl="$d.mr.json"
        [ -f "$ctl" ] || continue
        if [ "$(jq -r '.open // true' "$ctl")" = false ]; then
          log "    $(jq -r .branch "$ctl"): pushed, MR is opened live (seed.sh --open $name:$(jq -r .branch "$ctl"))"
          continue
        fi
        open_mr "$pid" "$full" "$ctl"
      done
    fi
  fi

  api_json "$GITLAB_SERVICE_TOKEN" PUT "/projects/$pid" \
    "$(jq -n --argjson r "$resolve" '{only_allow_merge_if_all_discussions_are_resolved: $r}')"
  api_ok || die "settings of $full: HTTP $API_STATUS $(api_message)"
  log "  all threads must be resolved: $resolve"

  if [ "$name" = ext-client ]; then
    api "$GITLAB_SERVICE_TOKEN" POST "/projects/$pid/members" \
      --data-urlencode "user_id=$BOT_ID" --data-urlencode access_level=20
    if api_ok || [ "$API_STATUS" = 409 ]; then
      log "  bot $BOT_USER: Reporter on $full"
    else
      die "add bot to $full: HTTP $API_STATUS $(api_message)"
    fi
  fi
}

# --- main -------------------------------------------------------------------

api "$GITLAB_SERVICE_TOKEN" GET /user
api_ok || die "GitLab at $GITLAB_HOST_URL does not accept GITLAB_SERVICE_TOKEN (HTTP $API_STATUS); run up.sh"
api "$GITLAB_TOKEN" GET /user
api_ok || die "GITLAB_TOKEN (bot) does not work: HTTP $API_STATUS"
BOT_ID=$(printf '%s' "$API_BODY" | jq -r .id)
BOT_USER=$(printf '%s' "$API_BODY" | jq -r .username)

if [ -n "$open_spec" ]; then
  name=${open_spec%%:*}
  branch=${open_spec#*:}
  [ -n "$name" ] && [ -n "$branch" ] && [ "$name" != "$open_spec" ] || die "--open expects <project>:<branch>"
  group=$(printf '%s\n' "$PROJECTS" | awk -v n="$name" '$1 == n { print $2 }')
  [ -n "$group" ] || die "unknown project $name"
  ctl=
  for f in "$FIXTURES/$name"/mr/*/.mr.json; do
    if [ -f "$f" ] && [ "$(jq -r .branch "$f")" = "$branch" ]; then ctl=$f; break; fi
  done
  [ -n "$ctl" ] || die "no fixture MR with branch $branch in $name"
  pid=$(project_id_of "$group/$name")
  [ -n "$pid" ] || die "$group/$name does not exist; run seed.sh first"
  open_mr "$pid" "$group/$name" "$ctl"
  exit 0
fi

# fd 3: the loop body (git, curl) must not consume the project list on stdin.
while read -r name group resolve <&3; do
  [ -n "$name" ] || continue
  wanted "$name" || continue
  seed_project "$name" "$group" "$resolve"
done 3<<EOF
$PROJECTS
EOF

step "Skills (review.skills.dir: ./e2e/local/skills)"
for s in go-errors pg-migrations broken-skill; do
  if [ -f "$LOCAL_DIR/skills/$s/SKILL.md" ]; then log "  $s"; else die "missing e2e/local/skills/$s/SKILL.md"; fi
done

step "Done. MRs and their scenarios: e2e/local/SCENARIOS.md"
log "MRs opened before the bot's first start are only baselined by polling (B1):"
log "start a review with a comment from rb-dev: @$BOT_USER review"
