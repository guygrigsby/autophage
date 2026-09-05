#!/usr/bin/env bash
# Backend dev loop: build appd, run it, rebuild + bounce on changes under
# cmd/ or internal/ or *.go at the root. macOS-portable; polls mtimes every
# 2s, no fswatch/entr needed. Logs to /tmp/app-dev.log.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LOG="/tmp/app-dev.log"
PID_FILE="/tmp/app-dev.pid"

cd "$ROOT"

build() {
  if go build -o appd ./cmd/appd >>"$LOG" 2>&1; then return 0; fi
  echo "BUILD FAILED — keeping previous binary running" | tee -a "$LOG" >&2
  return 1
}

stop() {
  if [[ -f "$PID_FILE" ]]; then
    local pid; pid="$(cat "$PID_FILE" 2>/dev/null || true)"
    [[ -n "$pid" ]] && kill "$pid" 2>/dev/null || true
    rm -f "$PID_FILE"
  fi
}

start() {
  ./appd >>"$LOG" 2>&1 &
  echo $! >"$PID_FILE"
}

trap 'stop; exit 0' EXIT INT TERM

snapshot() { find cmd internal *.go -name '*.go' -exec stat -f '%m %N' {} + 2>/dev/null | sort; }

build && start
echo "dev-watch: appd running (logs: $LOG)" >&2
last="$(snapshot)"
while true; do
  sleep 2
  now="$(snapshot)"
  if [[ "$now" != "$last" ]]; then
    echo "change detected, rebuilding" >&2
    last="$now"
    if build; then stop; start; echo "bounced" >&2; fi
  fi
done
