#!/usr/bin/env bash
# Offline checks of the seed fixtures (no GitLab needed):
#
#   e2e/local/check-fixtures.sh                 # check every project and MR overlay
#   e2e/local/check-fixtures.sh rb-backend      # one project
#   e2e/local/check-fixtures.sh --write-index   # regenerate the table in SCENARIOS.md
#
# Each base, and base + each branch/MR overlay (with its deletions), is put
# together in a temp directory and checked with what is installed: gofmt,
# go vet/build (projects with go.mod), python3 py_compile, php -l, TypeScript
# syntax (if the typescript package is found), node --check, jq, ruby YAML.
# A missing tool is reported and skipped. An overlay may skip checks that it
# fails on purpose: "checks_skip": ["go-vet", "php-lint", ...] in .mr.json.
#
# .reviewbot.yaml files and e2e/local/config.yaml are validated by the Go test
# TestLocalStandConfigs (internal/config).
set -euo pipefail
LOCAL_DIR=$(cd "$(dirname "$0")" && pwd)
FIXTURES="$LOCAL_DIR/fixtures"
INDEX="$LOCAL_DIR/SCENARIOS.md"

fail=0
err() { printf 'FAIL %s\n' "$*" >&2; fail=1; }
have() { command -v "$1" >/dev/null 2>&1; }

TS_MOD=
if have node && have npm; then
  g=$(npm root -g 2>/dev/null || true)
  [ -n "$g" ] && [ -d "$g/typescript" ] && TS_MOD="$g/typescript"
fi
missing=
note_missing() { case " $missing " in *" $1 "*) ;; *) missing="$missing $1" ;; esac; }

# index_rows: markdown rows of every MR fixture (project, branch, title, scenarios, expect).
index_rows() {
  local d ctl proj
  for ctl in "$FIXTURES"/*/mr/*/.mr.json; do
    [ -f "$ctl" ] || continue
    d=$(dirname "$ctl")
    proj=$(basename "$(dirname "$(dirname "$d")")")
    jq -r --arg p "$proj" '
      def cell: tostring | gsub("\\|"; "\\|") | gsub("\n"; " ");
      "| `\($p)` | `\(.branch)` | \(.title | cell)\(if .draft then " (Draft)" else "" end)\(if .open == false then " *(открыть вживую)*" else "" end) | \(.scenarios | cell) | \(.expect | cell) |"' "$ctl"
  done
}

write_index() {
  local tmp begin='<!-- generated:begin (e2e/local/check-fixtures.sh --write-index) -->' end='<!-- generated:end -->'
  tmp=$(mktemp)
  {
    awk -v b="$begin" '{ print } index($0, b) == 1 { exit }' "$INDEX"
    echo '| Проект | Ветка | MR | Сценарии | Что ожидать |'
    echo '|---|---|---|---|---|'
    index_rows
    awk -v e="$end" 'index($0, e) == 1 { p = 1 } p { print }' "$INDEX"
  } >"$tmp"
  cat "$tmp" >"$INDEX"
  rm -f "$tmp"
}

if [ "${1:-}" = --write-index ]; then
  write_index
  echo "updated $INDEX"
  exit 0
fi

# materialize <dest> <project dir> [overlay dir]: base, then the branch the
# overlay starts from ("from", default main), then the overlay and deletions.
apply_dir() { # apply_dir <dest> <dir> <ctl>
  local p
  (cd "$2" && tar --exclude .mr.json --exclude .branch.json --exclude .base.json -cf - .) | (cd "$1" && tar -xf -)
  [ -f "$3" ] || return 0
  jq -r '.delete[]?' "$3" | while IFS= read -r p; do
    if [ -n "$p" ]; then rm -rf "${1:?}/$p"; fi
  done
}

materialize() {
  local dest=$1 fx=$2 ov=${3:-} ctl from bdir
  apply_dir "$dest" "$fx/base" "$fx/base/.base.json"
  [ -n "$ov" ] || return 0
  ctl="$ov/.mr.json"
  [ -f "$ctl" ] || ctl="$ov/.branch.json"
  from=$(jq -r '.from // "main"' "$ctl")
  if [ "$from" != main ]; then
    for bdir in "$fx"/branches/*/; do
      if [ -f "$bdir.branch.json" ] && [ "$(jq -r .branch "$bdir.branch.json")" = "$from" ]; then
        apply_dir "$dest" "$bdir" "$bdir.branch.json"
      fi
    done
  fi
  apply_dir "$dest" "$ov" "$ctl"
}

skipped() { # skipped <ctl> <check>
  [ -n "$1" ] && [ -f "$1" ] && jq -e --arg c "$2" '(.checks_skip // []) | index($c)' "$1" >/dev/null
}

check_tree() { # check_tree <label> <dir> [ctl]
  local label=$1 dir=$2 ctl=${3:-} f out
  # Go
  if [ -f "$dir/go.mod" ]; then
    if have gofmt; then
      out=$(cd "$dir" && gofmt -l . 2>&1 || true)
      [ -z "$out" ] || err "$label: gofmt: $out"
    else note_missing gofmt; fi
    if have go; then
      if ! skipped "$ctl" go-vet; then
        out=$(cd "$dir" && GOFLAGS=-mod=mod GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local go vet ./... 2>&1) || err "$label: go vet: $out"
      else
        out=$(cd "$dir" && GOFLAGS=-mod=mod GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local go build ./... 2>&1) || err "$label: go build: $out"
      fi
    else note_missing go; fi
  fi
  # Python, PHP, JS/TS, JSON, YAML
  while IFS= read -r f; do
    case $f in
      *.py)
        if have python3; then
          out=$(python3 -c 'import ast,sys; ast.parse(open(sys.argv[1],"rb").read(), sys.argv[1])' "$f" 2>&1) || err "$label: python: $out"
        else note_missing python3; fi ;;
      *.php)
        if skipped "$ctl" php-lint; then :
        elif have php; then
          out=$(php -l "$f" 2>&1) || err "$label: php -l: $out"
        else note_missing php; fi ;;
      *.ts | *.tsx)
        if [ -n "$TS_MOD" ]; then
          out=$(node -e '
            const ts = require(process.argv[1]); const fs = require("fs"); const f = process.argv[2];
            const sf = ts.createSourceFile(f, fs.readFileSync(f, "utf8"), ts.ScriptTarget.Latest, true,
              f.endsWith(".tsx") ? ts.ScriptKind.TSX : ts.ScriptKind.TS);
            const d = (sf.parseDiagnostics || []).map(x => ts.flattenDiagnosticMessageText(x.messageText, "\n"));
            if (d.length) { console.error(f + ": " + d.join("; ")); process.exit(1); }' "$TS_MOD" "$f" 2>&1) || err "$label: ts: $out"
        else note_missing typescript; fi ;;
      *.js | *.cjs)
        if have node; then out=$(node --check "$f" 2>&1) || err "$label: node --check: $out"; else note_missing node; fi ;;
      *.json)
        if have jq; then out=$(jq empty "$f" 2>&1) || err "$label: json: $out"; fi ;;
      *.yaml | *.yml)
        if have ruby; then
          out=$(ruby -ryaml -e 'YAML.load_file(ARGV[0])' "$f" 2>&1) || {
            skipped "$ctl" yaml || err "$label: yaml: $out"
          }
        else note_missing ruby; fi ;;
    esac
  done < <(find "$dir" -type f ! -path '*/.git/*')
}

check_ctl() { # check_ctl <ctl>
  local ctl=$1 k
  jq empty "$ctl" 2>/dev/null || { err "$ctl: invalid JSON"; return; }
  for k in branch title scenarios expect; do
    [ "$(jq -r --arg k "$k" '.[$k] // "" | tostring' "$ctl")" != "" ] || err "$ctl: missing \"$k\""
  done
}

only=${1:-}
for fx in "$FIXTURES"/*/; do
  fx=${fx%/}
  proj=$(basename "$fx")
  [ -z "$only" ] || [ "$only" = "$proj" ] || continue
  [ -d "$fx/base" ] || continue
  tmp=$(mktemp -d)
  materialize "$tmp" "$fx"
  check_tree "$proj/base" "$tmp"
  rm -rf "$tmp"
  for ov in "$fx"/branches/*/ "$fx"/mr/*/; do
    [ -d "$ov" ] || continue
    ov=${ov%/}
    ctl=
    [ -f "$ov/.mr.json" ] && ctl="$ov/.mr.json"
    [ -f "$ov/.branch.json" ] && ctl="$ov/.branch.json"
    [ -n "$ctl" ] || { err "$ov: no .mr.json/.branch.json"; continue; }
    [ -f "$ov/.mr.json" ] && check_ctl "$ctl"
    # An overlay file identical to the base one is a no-op (a failed edit).
    while IFS= read -r f; do
      rel=${f#"$ov"/}
      case $rel in .mr.json | .branch.json) continue ;; esac
      if [ -f "$fx/base/$rel" ] && cmp -s "$f" "$fx/base/$rel"; then err "$proj/${ov#"$fx"/}: $rel is identical to base"; fi
    done < <(find "$ov" -type f)
    tmp=$(mktemp -d)
    materialize "$tmp" "$fx" "$ov"
    check_tree "$proj/${ov#"$fx"/}" "$tmp" "$ctl"
    rm -rf "$tmp"
  done
done

# Branch names must be unique per project; SCENARIOS.md must be current.
dups=$(for ctl in "$FIXTURES"/*/mr/*/.mr.json "$FIXTURES"/*/branches/*/.branch.json; do
  [ -f "$ctl" ] || continue
  printf '%s %s\n' "$(basename "$(dirname "$(dirname "$(dirname "$ctl")")")")" "$(jq -r .branch "$ctl")"
done | sort | uniq -d)
[ -z "$dups" ] || err "duplicate branches: $dups"

if [ -z "$only" ] && [ -f "$INDEX" ]; then
  tmpi=$(mktemp)
  cp "$INDEX" "$tmpi"
  write_index
  if ! cmp -s "$INDEX" "$tmpi"; then
    err "SCENARIOS.md was out of date and has been regenerated: review and commit it"
  fi
  rm -f "$tmpi"
fi

[ -z "$missing" ] || echo "skipped checks (tool not installed):$missing" >&2
if [ "$fail" -ne 0 ]; then
  echo "fixtures: FAILED" >&2
  exit 1
fi
echo "fixtures: ok"
