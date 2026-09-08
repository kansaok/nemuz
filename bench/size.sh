#!/usr/bin/env bash
# Fail if the binary exceeds its size budget.
set -euo pipefail
LIMIT_MB=${LIMIT_MB:-40}
BIN=${BIN:-bin/nemuz}
[ -f "$BIN" ] || { echo "FAIL  $BIN not built"; exit 1; }
bytes=$(stat -c %s "$BIN")
mb=$((bytes / 1024 / 1024))
if [ "$mb" -gt "$LIMIT_MB" ]; then
  printf 'FAIL  binary is %s MB (limit %s MB)\n' "$mb" "$LIMIT_MB"
  exit 1
fi
printf 'ok    binary %s MB (limit %s MB)\n' "$mb" "$LIMIT_MB"
