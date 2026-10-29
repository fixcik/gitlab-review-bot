#!/usr/bin/env bash
# Checks the package layering against .go-arch-lint.yml with a pinned go-arch-lint release.
# Usage: scripts/arch-lint.sh            (downloads the release archive into .cache/ on first run)
#        GO_ARCH_LINT=/path/to/bin scripts/arch-lint.sh
#
# The archive's sha256 is pinned below (copied from the release's checksums.txt when the
# version was bumped), not taken from the release at run time. The cached archive is
# re-verified and the binary re-extracted from it on every run, so a tampered cache fails.
set -euo pipefail
VERSION=1.19.0
sha256_for() {
  case "$1" in
    linux_amd64)  echo b4454546abf95ab1b32c26af146bc7c73ba3856c534d66890ac0738bada5a03c ;;
    linux_arm64)  echo 50174a73916c767eda2b27cf18a4db80cd7f102eeb4c514a705239279fd2754b ;;
    darwin_amd64) echo 09b641e03b5314bfee70e98fa9caf91986a650d28b834713871c8d25867a6285 ;;
    darwin_arm64) echo 63a64239de9acdbd0ea3f0bf7b94705cc8a441b02b37c01147d3027e8f78611e ;;
    *) return 1 ;;
  esac
}
sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | cut -d' ' -f1
  else shasum -a 256 "$1" | cut -d' ' -f1; fi
}
root="$(cd "$(dirname "$0")/.." && pwd)"
bin="${GO_ARCH_LINT:-}"
if [ -z "$bin" ]; then
  os="$(uname -s | tr '[:upper:]' '[:lower:]')"
  arch="$(uname -m)"; case "$arch" in x86_64) arch=amd64 ;; aarch64|arm64) arch=arm64 ;; esac
  want="$(sha256_for "${os}_${arch}")" || { echo "arch-lint: no pinned go-arch-lint checksum for ${os}_${arch}; set GO_ARCH_LINT" >&2; exit 1; }
  dir="$root/.cache/go-arch-lint/$VERSION"
  asset="go-arch-lint_${VERSION}_${os}_${arch}.tar.gz"
  mkdir -p "$dir"
  if [ ! -f "$dir/$asset" ] || [ "$(sha256_of "$dir/$asset")" != "$want" ]; then
    rm -f "$dir/$asset" "$dir/go-arch-lint"
    curl -fsSL -o "$dir/$asset.tmp" "https://github.com/fe3dback/go-arch-lint/releases/download/v${VERSION}/$asset"
    mv "$dir/$asset.tmp" "$dir/$asset"
  fi
  got="$(sha256_of "$dir/$asset")"
  if [ "$got" != "$want" ]; then
    rm -f "$dir/$asset" "$dir/go-arch-lint"
    echo "arch-lint: $asset sha256 $got, want $want" >&2
    exit 1
  fi
  bin="$dir/go-arch-lint"
  rm -f "$bin"
  tar -xzf "$dir/$asset" -C "$dir" go-arch-lint
fi
exec "$bin" check --project-path "$root" "$@"
