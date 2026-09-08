#!/usr/bin/env bash
# Fail if cold start exceeds its budget.
set -euo pipefail
LIMIT_MS=${LIMIT_MS:-100}
RUNS=${RUNS:-20}
BIN=${BIN:-bin/nemuz}
[ -f "$BIN" ] || { echo "FAIL  $BIN not built"; exit 1; }
start=$(date +%s%N)
for _ in $(seq "$RUNS"); do "$BIN" version >/dev/null; done
end=$(date +%s%N)
ms=$(( (end - start) / RUNS / 1000000 ))
if [ "$ms" -gt "$LIMIT_MS" ]; then
  printf 'FAIL  startup %s ms (limit %s ms)\n' "$ms" "$LIMIT_MS"
  exit 1
fi
printf 'ok    startup %s ms (limit %s ms)\n' "$ms" "$LIMIT_MS"
