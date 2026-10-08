#!/usr/bin/env bash
# End-to-end propagation check against a real OTLP backend.
#
# Phase 1 (join):    target up, one Parallax pass. Each probe's client span
#                    should parent a parallax-gate-target server span.
# Phase 2 (absence): Parallax walks every 15s for PHASE2_MINUTES while
#                    gate-dead never answers; halfway through, the target is
#                    stopped too, so a node that WAS answering stops answering.
#
# Usage:
#   OTLP_ENDPOINT=otlp.example.com:443 OTLP_HEADERS='api-key=...' scripts/propagation-check.sh
# Optional: PHASE2_MINUTES (default 6), OTLP_INSECURE=1 for a plaintext receiver.
# Writes every trace id it produced to ./propagation-check-<timestamp>.log.
# Every pass uses ONE service.instance.id: a facility is one per instance, so
# distinct ids would split Parallax into several callers on the grid.
set -euo pipefail

: "${OTLP_ENDPOINT:?set OTLP_ENDPOINT (host:port)}"
: "${OTLP_HEADERS:?set OTLP_HEADERS (k=v,k=v)}"
PHASE2_MINUTES="${PHASE2_MINUTES:-6}"
INSECURE=()
[[ "${OTLP_INSECURE:-}" == "1" ]] && INSECURE=(-insecure)

cd "$(dirname "$0")/.."
out="propagation-check-$(date -u +%Y%m%dT%H%M%SZ).log"
bin="$(mktemp -d)"
trap 'kill ${TARGET_PID:-} 2>/dev/null || true; rm -rf "$bin"' EXIT

go build -o "$bin/gatetarget" ./tools/gatetarget
go build -o "$bin/parallax" ./cmd/parallax
export OTEL_EXPORTER_OTLP_HEADERS="$OTLP_HEADERS" # read by both binaries; never echoed.

log() { echo "[$(date -u +%H:%M:%SZ)] $*" | tee -a "$out"; }

log "starting gatetarget (service.name parallax-gate-target)"
"$bin/gatetarget" -http 127.0.0.2:18080 -grpc 127.0.0.2:19090 -endpoint "$OTLP_ENDPOINT" "${INSECURE[@]}" >>"$out" 2>&1 &
TARGET_PID=$!
sleep 2

log "PHASE 1: one pass, target up"
"$bin/parallax" -manifest examples/gate-target.yaml -once -log-level debug \
  -endpoint "$OTLP_ENDPOINT" "${INSECURE[@]}" -instance-id gate >>"$out" 2>&1 || true

log "PHASE 2: a pass every 15s for ${PHASE2_MINUTES}m; gate-dead never answers"
# A loop of -once passes rather than one long ambient process: each pass exits
# and flushes its spans, and nothing depends on delivering SIGINT (which native
# Windows processes under Git Bash may not receive).
passes=$((PHASE2_MINUTES * 4))
half=$((passes / 2))
for ((i = 1; i <= passes; i++)); do
  if ((i == half + 1)); then
    log "PHASE 2: stopping gatetarget; gate-target should now go unanswered too"
    kill "$TARGET_PID" 2>/dev/null || true
    wait "$TARGET_PID" 2>/dev/null || true
    TARGET_PID=
  fi
  start=$(date +%s)
  "$bin/parallax" -manifest examples/gate-target.yaml -once -log-level debug \
    -endpoint "$OTLP_ENDPOINT" "${INSECURE[@]}" -instance-id gate >>"$out" 2>&1 || true
  sleep $((15 - ($(date +%s) - start) > 0 ? 15 - ($(date +%s) - start) : 0))
done

log "done. Trace ids:"
grep -oE 'call="[^"]+".*trace_id=[0-9a-f]{32}' "$out" | sed -E 's/detail=.*trace_id=/trace_id=/' | sort -u | tee -a "$out.ids"
log "full log: $out ; trace ids: $out.ids"
