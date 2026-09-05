#!/usr/bin/env bash
# Prove the sandbox image: user, tools, env, no network in the agent shape.
set -euo pipefail
IMG="${1:-localhost/autophage-sandbox:latest}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
FIXTURES_DIR="$SCRIPT_DIR/fixtures/warm"
# Read the pinned pnpm from the Containerfile rather than restating it, and
# hold the fixture to the same pin: the offline corepack assertion below is
# only meaningful while the fixture asks for the version the image bakes.
PNPM_VERSION="$(sed -n 's/^ARG PNPM_VERSION=//p' "$SCRIPT_DIR/Containerfile")"

# The two container shapes the daemon runs, kept identical to the flags
# internal/sandbox/podman.go passes (Start for the agent phase, warm for the
# prep phase). That file is the source of truth; this script is only worth
# anything if what it proves is the shape production actually gets.
AGENT_FLAGS=(--read-only --tmpfs /tmp:rw,size=1g
  --mount type=tmpfs,destination=/home/agent,tmpfs-size=268435456,tmpfs-mode=0700,U=true
  --cap-drop=all --security-opt=no-new-privileges
  --userns=keep-id:uid=1000,gid=1000 --network=none)
PREP_FLAGS=(--cap-drop=all --security-opt=no-new-privileges
  --userns=keep-id:uid=1000,gid=1000
  -e GOPROXY=https://proxy.golang.org -e GOFLAGS=-mod=readonly -e GOSUMDB=sum.golang.org)

run() { podman run --rm "${AGENT_FLAGS[@]}" "$IMG" "$@"; }
fail() { echo "FAIL: $*" >&2; exit 1; }

[ -n "$PNPM_VERSION" ] || fail "no ARG PNPM_VERSION in the Containerfile"
grep -q "\"pnpm@$PNPM_VERSION\"" "$FIXTURES_DIR/pnpm/package.json" \
  || fail "the pnpm fixture's packageManager does not pin the image's pnpm $PNPM_VERSION"

# Scratch tmpdirs and named cache volumes are throwaway and removed on exit,
# pass or fail.
TMPDIRS=()
VOLS=()
cleanup() {
  local d v
  for d in ${TMPDIRS[@]+"${TMPDIRS[@]}"}; do rm -rf "$d"; done
  for v in ${VOLS[@]+"${VOLS[@]}"}; do podman volume rm -f "$v" >/dev/null 2>&1 || true; done
}
trap cleanup EXIT

# scratch sets SCRATCH_TMP and SCRATCH_VOL to a fresh, empty workspace and
# cache pair, both registered for cleanup. It assigns rather than prints
# because a command substitution would run it in a subshell, where the
# cleanup arrays it appends to are the subshell's copies and the volumes it
# created would outlive the run.
SCRATCH_TMP=""
SCRATCH_VOL=""
scratch() {
  local name=$1
  SCRATCH_TMP="$(mktemp -d)"; TMPDIRS+=("$SCRATCH_TMP")
  SCRATCH_VOL="autophage-imgtest-$name-$$"; VOLS+=("$SCRATCH_VOL")
  podman volume create "$SCRATCH_VOL" >/dev/null
}

# agent runs a command in the agent shape against one workspace and cache.
# The ":z" on the cache mirrors internal/sandbox/podman.go: a cache written by
# one container is unreadable by the next without it.
agent() {
  local tmp=$1 vol=$2; shift 2
  podman run --rm "${AGENT_FLAGS[@]}" -v "$tmp:/work:Z" -v "$vol:/cache:z" "$IMG" "$@"
}

[ "$(run id -u)" = "1000" ] || fail "uid is not 1000"
run go version | grep -q 'go1.26' || fail "go 1.26 missing"
run node --version | grep -q '^v22' || fail "node 22 missing"
run python3 --version | grep -q '3.12' || fail "python 3.12 missing"
run uv --version >/dev/null || fail "uv missing"
# The runtime COREPACK_HOME is a cache volume that is empty until warm-deps
# seeds it, so the baked pnpm is proved through the seed directory here and
# through the real path (seed, then the shim offline) after the pnpm fixture.
run env COREPACK_HOME=/opt/corepack pnpm --version | grep -q "^${PNPM_VERSION}\$" || fail "baked pnpm $PNPM_VERSION missing"
run git --version >/dev/null || fail "git missing"
run make --version >/dev/null || fail "make missing"
run gcc --version >/dev/null || fail "gcc missing"
run autophage-toolbox -h 2>&1 | grep -q workdir || fail "toolbox missing"
run sh -c 'echo $GOMODCACHE' | grep -q '^/cache/go/mod$' || fail "GOMODCACHE not set"
run sh -c 'test -w /tmp && test -w "$HOME"' || fail "/tmp or the agent home is not writable"
if run sh -c 'curl -sS -m 3 https://proxy.golang.org >/dev/null 2>&1'; then fail "network reachable with --network=none"; fi

# Everything a phase writes has to land on a mount: the root filesystem is
# read-only, so this has to be checked with the volumes attached, the way the
# daemon runs it.
scratch write
agent "$SCRATCH_TMP" "$SCRATCH_VOL" sh -c 'test -w /work && test -w /cache/go && test -w /cache/npm && test -w /cache/uv' \
  || fail "/work or /cache not writable by agent"

# warm-deps end to end, per package manager: warm a throwaway copy of each
# deploy/fixtures/warm/<name> fixture in a networked prep container (same
# shape warm-deps normally runs in), then prove the agent phase resolves the
# dependency from the same /work and /cache with --network=none, no
# lockfile-only mocking.
warm_fixture() {
  local name=$1; shift
  local out
  scratch "$name"
  WARM_TMP="$SCRATCH_TMP"
  WARM_VOL="$SCRATCH_VOL"
  cp -a "$FIXTURES_DIR/$name/." "$WARM_TMP/"

  out="$(podman run --rm "${PREP_FLAGS[@]}" -v "$WARM_TMP:/work:Z" -v "$WARM_VOL:/cache:z" "$IMG" warm-deps 2>&1)" || true
  echo "$out" | grep -q "warm-deps: .* failed" && fail "warm-deps failed to warm the $name fixture:"$'\n'"$out"

  agent "$WARM_TMP" "$WARM_VOL" "$@" >/dev/null \
    || fail "$name fixture did not resolve offline after warm-deps"
}

warm_fixture npm  node -e "require('left-pad')"
warm_fixture pnpm node -e "require('left-pad')"
# The pnpm fixture pins the baked version in packageManager, so the seeded
# copy on the cache volume is what corepack has to resolve, offline.
got="$(agent "$WARM_TMP" "$WARM_VOL" corepack pnpm --version | tr -d '\r')"
[ "$got" = "$PNPM_VERSION" ] || fail "corepack pnpm --version offline printed '$got', want $PNPM_VERSION"
warm_fixture uv   uv run --frozen --offline python -c "import six"

echo "✓ image ok"
