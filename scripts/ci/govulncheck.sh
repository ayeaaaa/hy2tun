#!/usr/bin/env bash
# Run govulncheck and allow only the documented, unreachable-in-this-project
# Hysteria sniff vulnerability. Any other reachable vulnerability fails CI.
set -u

run_govulncheck() {
  if command -v govulncheck >/dev/null 2>&1; then
    govulncheck -scan=symbol ./...
  else
    go run golang.org/x/vuln/cmd/govulncheck@latest -scan=symbol ./...
  fi
}

out=$(mktemp)
trap 'rm -f "$out"' EXIT

set +e
run_govulncheck >"$out" 2>&1
status=$?
set -e
cat "$out"

if [ "$status" -eq 0 ]; then
  exit 0
fi

count=$(grep -c '^Vulnerability #' "$out" || true)
if [ "$status" -ne 0 ] && [ "$count" -eq 1 ] && grep -q 'GO-2026-5288' "$out"; then
  echo "Accepted: GO-2026-5288 is in Hysteria core v2.13.0; it requires sniff, which hy2tun does not enable, and upstream has no fixed release."
  exit 0
fi

exit "$status"
