#!/usr/bin/env bash
# Prove the sandbox image: user, tools, env, no network in the agent shape.
set -euo pipefail
IMG="${1:-localhost/autophage-sandbox:latest}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
FIXTURES_DIR="$SCRIPT_DIR/fixtures/warm"
run() { podman run --rm --network=none --userns=keep-id "$IMG" "$@"; }
fail() { echo "FAIL: $*" >&2; exit 1; }

[ "$(run id -u)" = "1000" ] || fail "uid is not 1000"
run go version | grep -q 'go1.26' || fail "go 1.26 missing"
run node --version | grep -q '^v22' || fail "node 22 missing"
run python3 --version | grep -q '3.12' || fail "python 3.12 missing"
run uv --version >/dev/null || fail "uv missing"
run pnpm --version >/dev/null || fail "pnpm missing"
run git --version >/dev/null || fail "git missing"
run make --version >/dev/null || fail "make missing"
run gcc --version >/dev/null || fail "gcc missing"
run autophage-toolbox -h 2>&1 | grep -q workdir || fail "toolbox missing"
run sh -c 'echo $GOMODCACHE' | grep -q '^/cache/go/mod$' || fail "GOMODCACHE not set"
run sh -c 'test -w /work && test -w /cache/go && test -w /cache/npm && test -w /cache/uv' || fail "/work or /cache not writable by agent"
if run sh -c 'curl -sS -m 3 https://proxy.golang.org >/dev/null 2>&1'; then fail "network reachable with --network=none"; fi

# warm-deps end to end, per package manager: warm a throwaway copy of each
# deploy/fixtures/warm/<name> fixture in a networked prep container (same
# shape warm-deps normally runs in), then prove the agent phase resolves the
# dependency from the same /work and /cache with --network=none, no
# lockfile-only mocking. Scratch tmpdirs and named cache volumes are
# throwaway and removed on exit, pass or fail.
WARM_TMP=()
WARM_VOLS=()
warm_cleanup() {
  local d v
  for d in ${WARM_TMP[@]+"${WARM_TMP[@]}"}; do rm -rf "$d"; done
  for v in ${WARM_VOLS[@]+"${WARM_VOLS[@]}"}; do podman volume rm -f "$v" >/dev/null 2>&1 || true; done
}
trap warm_cleanup EXIT

warm_fixture() {
  local name=$1; shift
  local tmp vol out
  tmp="$(mktemp -d)"; WARM_TMP+=("$tmp")
  cp -a "$FIXTURES_DIR/$name/." "$tmp/"
  vol="autophage-imgtest-$name-$$"; WARM_VOLS+=("$vol")
  podman volume create "$vol" >/dev/null

  out="$(podman run --rm --userns=keep-id -v "$tmp:/work:Z" -v "$vol:/cache" "$IMG" warm-deps 2>&1)" || true
  echo "$out" | grep -q "failed" && fail "warm-deps failed to warm the $name fixture:"$'\n'"$out"

  podman run --rm --network=none --userns=keep-id -v "$tmp:/work:Z" -v "$vol:/cache" "$IMG" "$@" >/dev/null \
    || fail "$name fixture did not resolve offline after warm-deps"
}

warm_fixture npm  node -e "require('left-pad')"
warm_fixture pnpm node -e "require('left-pad')"
warm_fixture uv   uv run --frozen --offline python -c "import six"

echo "✓ image ok"
