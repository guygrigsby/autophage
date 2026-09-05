#!/usr/bin/env bash
# Rename the rookery template into a concrete app.
# Usage: scripts/init.sh <name> [--no-web]
# <name> must be a lowercase Go-identifier-safe word (a-z0-9, no leading digit).
set -euo pipefail

NAME="${1:-}"
NO_WEB=0
[[ "${2:-}" == "--no-web" ]] && NO_WEB=1

if [[ -z "$NAME" || ! "$NAME" =~ ^[a-z][a-z0-9]*$ ]]; then
  echo "usage: scripts/init.sh <name> [--no-web]   (name: ^[a-z][a-z0-9]*$)" >&2
  exit 2
fi

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
UPPER="$(echo "$NAME" | tr '[:lower:]' '[:upper:]')"

# sed -i compatibility: BSD sed (macOS) requires an explicit empty backup suffix
# argument; GNU sed (Linux, Homebrew) uses just -i. Detect by dry-running.
if sed --version >/dev/null 2>&1; then
  # GNU sed
  SED_INPLACE() { sed -i "$@"; }
  SED_INPLACE_E() { sed -i -E "$@"; }
else
  # BSD sed
  SED_INPLACE() { sed -i '' "$@"; }
  SED_INPLACE_E() { sed -i '' -E "$@"; }
fi

# Rewrite all tracked files (minus this script) in-place.
# Use a while-read loop instead of mapfile: macOS ships bash 3.2 (no mapfile).
# Tokens applied longest/most-specific first:
#   module path, launchd label, daemon token (appd), CLI source dir (cmd/app)
#   and CLI run examples (./app ) — both anchored so they can't clobber appd —
#   package clause, Makefile APP var, env prefix, perch app-id literal,
#   config/log dirs. The CLI binary itself is $(APP) in the Makefile, so it
#   needs no token here.
git ls-files | grep -v '^scripts/init.sh$' | while IFS= read -r f; do
  [[ -f "$f" ]] || continue
  SED_INPLACE \
    -e "s|github.com/guygrigsby/rookery|github.com/guygrigsby/${NAME}|g" \
    -e "s|dev\.grigsby\.appd|dev.grigsby.${NAME}d|g" \
    -e "s|appd|${NAME}d|g" \
    -e "s|cmd/app|cmd/${NAME}|g" \
    -e "s|\./app |./${NAME} |g" \
    -e "s|^package app\$|package ${NAME}|" \
    -e "s|^# app\$|# ${NAME}|" \
    -e "s|\"app CLI\"|\"${NAME} CLI\"|g" \
    -e "s|\"app\"|\"${NAME}\"|g" \
    -e "s|APP_|${UPPER}_|g" \
    -e "s|\.logs/app|.logs/${NAME}|g" \
    -e "s|\.config/app|.config/${NAME}|g" \
    "$f"
  # Makefile APP variable: use -E for extended regex to handle run of spaces.
  SED_INPLACE_E \
    -e "s|^APP[[:space:]]+\\?= app\$|APP         ?= ${NAME}|" \
    "$f"
done

# Rename cmd dirs and the plist template file.
git mv cmd/appd "cmd/${NAME}d"
git mv cmd/app "cmd/${NAME}"
git mv deploy/dev.grigsby.appd.plist.template "deploy/dev.grigsby.${NAME}d.plist.template"

# Swap the template's own README for the app README (the sed loop above already
# rewrote README.app.md's tokens, including its `# app` title).
if [[ -f README.app.md ]]; then
  mv -f README.app.md README.md
fi

# --no-web: drop web/ and replace embed.go with a no-embed stub.
if [[ "$NO_WEB" == "1" ]]; then
  rm -rf web
  cat > embed.go <<'EOF'
// Package PKG: headless variant. No web SPA is embedded; Static returns an
// empty filesystem so the API serves no static UI.
package PKG

import "io/fs"

// Static returns an empty filesystem (no web build in this app).
func Static() fs.FS { return emptyFS{} }

// HasIndex always reports false: there is no embedded index.html.
func HasIndex(fs.FS) bool { return false }

type emptyFS struct{}

func (emptyFS) Open(string) (fs.File, error) { return nil, fs.ErrNotExist }
EOF
  SED_INPLACE "s|package PKG|package ${NAME}|g; s|Package PKG|Package ${NAME}|g" embed.go
fi

# Remove template-only scaffolding. These build/test the rookery template
# itself; a generated app must not ship or run them (notably `make test` must
# not invoke init_smoke_test.sh). The running init.sh is already loaded into
# memory, so removing it on disk here is safe.
for tf in scripts/init_smoke_test.sh scripts/init.sh; do
  [[ -f "$tf" ]] || continue
  git rm -q -f "$tf" >/dev/null 2>&1 || rm -f "$tf"
done

# Fresh issue tracker.
rm -rf .beads
if command -v bd >/dev/null 2>&1; then
  bd init >/dev/null 2>&1 || echo "note: 'bd init' failed; initialize manually" >&2
else
  echo "note: bd not installed; skipping tracker init" >&2
fi

# Normalize formatting after all the surgery.
gofmt -w . 2>/dev/null || true

echo "✓ initialized '${NAME}'. Next: go mod tidy && make build && make test"
