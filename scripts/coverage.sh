#!/usr/bin/env bash
# Coverage of the whole module, counting cross-package tests (-coverpkg): the
# review features are exercised through a whole reviewer (review/reviewtest).
# Test-support packages (internal/review/reviewtest) are not counted.
#
#   scripts/coverage.sh [out-dir]   # writes cover.out, cover.html, coverage.txt
#                                   # (total, e.g. 90.0), packages.md and coverage.svg;
#                                   # fails when a critical group (scripts/coverage-groups.txt)
#                                   # is below its floor
set -euo pipefail
out="${1:-.cache/coverage}"
mkdir -p "$out"
go test -count=1 -coverpkg=./... -coverprofile="$out/cover.out" ./... >"$out/test.log" || { cat "$out/test.log"; exit 1; }
go tool cover -html="$out/cover.out" -o "$out/cover.html"

# One pass over the profile: a block counts as covered if any test binary hit it
# (the profile lists a block once per test binary that compiled it).
python3 - "$out" <<'PY'
import collections, sys
out = sys.argv[1]
mod = open("go.mod").readline().split()[1] + "/"
blocks = {}
for line in open(f"{out}/cover.out"):
    if line.startswith("mode:"):
        continue
    key, stmts, count = line.rsplit(" ", 2)
    prev = blocks.get(key, (0, 0))[1]
    blocks[key] = (int(stmts), max(int(count), prev))
pkgs = collections.defaultdict(lambda: [0, 0])
for key, (stmts, count) in blocks.items():
    pkg = key.split(":")[0].rsplit("/", 1)[0].replace(mod, "")
    if pkg.endswith("/reviewtest"):
        continue
    pkgs[pkg][0] += stmts
    pkgs[pkg][1] += stmts if count else 0
total = sum(v[0] for v in pkgs.values())
covered = sum(v[1] for v in pkgs.values())
pct = covered / total * 100 if total else 0.0
open(f"{out}/coverage.txt", "w").write(f"{pct:.1f}\n")
with open(f"{out}/packages.md", "w") as f:
    f.write(f"### Coverage: {pct:.1f}% ({covered}/{total} statements)\n\n| Package | Coverage | Statements |\n|---|---:|---:|\n")
    for pkg, (n, c) in sorted(pkgs.items(), key=lambda kv: kv[1][1] / kv[1][0] if kv[1][0] else 1):
        f.write(f"| `{pkg}` | {c / n * 100 if n else 0:.1f}% | {n} |\n")
color = "#4c1" if pct >= 90 else "#97ca00" if pct >= 80 else "#dfb317" if pct >= 70 else "#fe7d37" if pct >= 60 else "#e05d44"
value = f"{pct:.1f}%"
lw, vw = 61, 7 * len(value) + 10
svg = f"""<svg xmlns="http://www.w3.org/2000/svg" width="{lw + vw}" height="20" role="img" aria-label="coverage: {value}">
<title>coverage: {value}</title>
<linearGradient id="s" x2="0" y2="100%"><stop offset="0" stop-color="#bbb" stop-opacity=".1"/><stop offset="1" stop-opacity=".1"/></linearGradient>
<clipPath id="r"><rect width="{lw + vw}" height="20" rx="3" fill="#fff"/></clipPath>
<g clip-path="url(#r)"><rect width="{lw}" height="20" fill="#555"/><rect x="{lw}" width="{vw}" height="20" fill="{color}"/><rect width="{lw + vw}" height="20" fill="url(#s)"/></g>
<g fill="#fff" text-anchor="middle" font-family="Verdana,Geneva,DejaVu Sans,sans-serif" font-size="11">
<text x="{lw / 2}" y="15" fill="#010101" fill-opacity=".3">coverage</text><text x="{lw / 2}" y="14">coverage</text>
<text x="{lw + vw / 2}" y="15" fill="#010101" fill-opacity=".3">{value}</text><text x="{lw + vw / 2}" y="14">{value}</text>
</g></svg>
"""
open(f"{out}/coverage.svg", "w").write(svg)
print(f"coverage: {value} ({covered}/{total} statements)")

# Floors for the critical groups (scripts/coverage-groups.txt).
import re
files = collections.defaultdict(lambda: [0, 0])
for key, (stmts, count) in blocks.items():
    f = key.split(":")[0].replace(mod, "")
    files[f][0] += stmts
    files[f][1] += stmts if count else 0
failed = []
rows = []
for line in open("scripts/coverage-groups.txt"):
    line = line.strip()
    if not line or line.startswith("#"):
        continue
    floor, name, rx = (part.strip() for part in line.split("|", 2))
    rx = re.compile(rx)
    n = c = 0
    for f, (a, b) in files.items():
        if rx.search(f) and not f.endswith("_test.go"):
            n += a
            c += b
    p = c / n * 100 if n else 0.0
    ok = n > 0 and p >= float(floor)
    rows.append(f"| {name} | {p:.1f}% | {floor}% | {n - c} of {n} | {'✅' if ok else '❌'} |")
    print(f"{'ok  ' if ok else 'FAIL'} {p:5.1f}% (floor {floor}%) {name}")
    if not ok:
        failed.append(name)
with open(f"{out}/packages.md") as f:
    body = f.read()
head, _, rest = body.partition("\n\n")
groups = "| Critical group | Coverage | Floor | Uncovered | |\n|---|---:|---:|---:|---|\n" + "\n".join(rows) + "\n"
with open(f"{out}/packages.md", "w") as f:
    f.write(head + "\n\n" + groups + "\n" + rest)
if failed:
    print("coverage below the floor: " + "; ".join(failed), file=sys.stderr)
    sys.exit(1)
PY
