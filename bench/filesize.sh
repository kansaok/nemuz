#!/usr/bin/env bash
# Fail if any Go source file exceeds the size budget.
set -euo pipefail
LIMIT=${LIMIT:-800}
fail=0
while read -r count file; do
  if [ "$count" -gt "$LIMIT" ]; then
    printf 'FAIL  %s is %s lines (limit %s)\n' "$file" "$count" "$LIMIT"
    fail=1
  fi
done < <(find . -name '*.go' -not -path './vendor/*' -exec wc -l {} + | grep -v ' total$' | awk '{print $1, $2}')
if [ "$fail" -eq 0 ]; then
  printf 'ok    no Go file exceeds %s lines\n' "$LIMIT"
fi
exit "$fail"
