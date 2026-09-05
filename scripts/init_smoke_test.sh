#!/usr/bin/env bash
# Smoke test: a fresh checkout, renamed via init.sh, must pass the full quality
# gate (`make check`) in both web and headless modes. Runs in temp dirs; never
# mutates this repo. Running the real gate (not just build+vet) is what catches
# a mode-specific breakage such as web steps leaking into a --no-web build.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

# Only the rookery template itself re-scaffolds. A generated app carries this
# script but must never run it (its module is no longer rookery), so skip there
# and keep `make test` green in derived apps.
if ! grep -q '^module github.com/guygrigsby/rookery$' "$ROOT/go.mod"; then
  echo "✓ init smoke: skipped (not the rookery template)"
  exit 0
fi

run_case() {
  local label="$1"; shift
  local tmp; tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  git -C "$ROOT" archive --format=tar HEAD | (cd "$tmp" && tar xf -)
  (
    cd "$tmp"
    git init -q && git add -A
    git -c user.email=t@t -c user.name=t commit -qm init
    bash scripts/init.sh demoapp "$@"
    go mod tidy >/dev/null 2>&1
    # The full gate, exactly as CI runs it, so a mode-specific gate breakage
    # (e.g. web-build invoked in a --no-web app) fails the smoke test.
    make check
    test -f README.md && test ! -e README.app.md
    grep -q '^# demoapp$' README.md
    ! grep -qi rookery README.md
  )
  echo "✓ init smoke: $label"
}

run_case "with web"
run_case "headless" --no-web
echo "✓ init.sh smoke test passed"
