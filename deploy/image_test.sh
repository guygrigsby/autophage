#!/usr/bin/env bash
# Prove the sandbox image: user, tools, env, no network in the agent shape.
set -euo pipefail
IMG="${1:-localhost/autophage-sandbox:latest}"
run() { podman run --rm --network=none --userns=keep-id "$IMG" "$@"; }
fail() { echo "FAIL: $*" >&2; exit 1; }

[ "$(run id -u)" = "1000" ] || fail "uid is not 1000"
run go version | grep -q 'go1.26' || fail "go 1.26 missing"
run node --version | grep -q '^v22' || fail "node 22 missing"
run python3 --version | grep -q '3.12' || fail "python 3.12 missing"
run uv --version >/dev/null || fail "uv missing"
run git --version >/dev/null || fail "git missing"
run make --version >/dev/null || fail "make missing"
run gcc --version >/dev/null || fail "gcc missing"
run autophage-toolbox -h 2>&1 | grep -q workdir || fail "toolbox missing"
run sh -c 'echo $GOMODCACHE' | grep -q '^/cache/go/mod$' || fail "GOMODCACHE not set"
run sh -c 'test -w /work && test -w /cache/go && test -w /cache/npm && test -w /cache/uv' || fail "/work or /cache not writable by agent"
if run sh -c 'curl -sS -m 3 https://proxy.golang.org >/dev/null 2>&1'; then fail "network reachable with --network=none"; fi
echo "✓ image ok"
