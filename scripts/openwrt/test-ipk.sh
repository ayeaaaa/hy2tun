#!/usr/bin/env bash
# Build both package types with a harmless binary and inspect their metadata.
set -euo pipefail
root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
work=$(mktemp -d "${TMPDIR:-/tmp}/hy2tun-ipk-test.XXXXXX")
trap 'rm -rf "$work"' EXIT INT TERM

"$root/scripts/openwrt/build-ipk.sh" /bin/true arm64 v0.0.0-test "$work" >/dev/null
"$root/scripts/openwrt/build-luci-ipk.sh" v0.0.0-test "$work" >/dev/null

mkdir -p "$work/client" "$work/luci"
(cd "$work/client" && tar -xzf ../hy2tun_0.0.0-test_arm64.ipk && tar -xzf control.tar.gz && tar -tzf data.tar.gz > files.txt)
(cd "$work/luci" && tar -xzf ../luci-app-hy2tun_0.0.0-test_all.ipk && tar -xzf control.tar.gz && tar -tzf data.tar.gz > files.txt)

grep -q '^Package: hy2tun$' "$work/client/control"
grep -q '^Architecture: all$' "$work/client/control"
grep -q './usr/bin/hy2tun' "$work/client/files.txt"
grep -q './etc/init.d/hy2tun' "$work/client/files.txt"
grep -q '^Package: luci-app-hy2tun$' "$work/luci/control"
grep -q '^Depends: luci-base, luci-compat, hy2tun$' "$work/luci/control"
grep -q './usr/lib/lua/luci/controller/hy2tun.lua' "$work/luci/files.txt"
grep -q './usr/lib/lua/luci/view/hy2tun/status.htm' "$work/luci/files.txt"

# Guard the LuCI regressions that caused a 502 Bad Gateway page:
# the controller must load nixio.fs, and the status command must not write
# its "inactive" output into the CGI response.
tar -xOzf "$work/luci/data.tar.gz" ./usr/lib/lua/luci/controller/hy2tun.lua |
	grep -q 'local fs = require "nixio.fs"'
tar -xOzf "$work/luci/data.tar.gz" ./usr/lib/lua/luci/view/hy2tun/status.htm |
	grep -q 'status >/dev/null 2>&1'
echo "OpenWrt package checks passed"
