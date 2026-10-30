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
grep -q './etc/hy2tun/client.yaml.example' "$work/client/files.txt"

# The package ships an example only. Post-install must never copy an active
# configuration or start the service at boot; it may only fix permissions on
# a configuration that already exists.
grep -q 'chmod 600 /etc/hy2tun/client.yaml' "$work/client/postinst"
! grep -q 'cp' "$work/client/postinst"
! grep -q 'enable' "$work/client/postinst"

# The bundled example must stay within the single-server basic fields.
tar -xOzf "$work/client/data.tar.gz" ./etc/hy2tun/client.yaml.example \
	> "$work/client/example.yaml"
grep -q '^server: ' "$work/client/example.yaml"
grep -q '^auth: ' "$work/client/example.yaml"
grep -q '^control:' "$work/client/example.yaml"
grep -q '^mappings:' "$work/client/example.yaml"
! grep -q '^  ca:' "$work/client/example.yaml"
! grep -q '^obfs:' "$work/client/example.yaml"
! grep -q '^bandwidth:' "$work/client/example.yaml"
! grep -q '^proxy:' "$work/client/example.yaml"
! grep -q '^transport:' "$work/client/example.yaml"

grep -q '^Package: luci-app-hy2tun$' "$work/luci/control"
grep -q '^Depends: luci-base, luci-compat, hy2tun (>= 1.5.0)$' "$work/luci/control"
grep -q './usr/lib/lua/luci/controller/hy2tun.lua' "$work/luci/files.txt"
grep -q './usr/lib/lua/luci/view/hy2tun/status.htm' "$work/luci/files.txt"

# Guard the LuCI regressions that caused a 502 Bad Gateway page: the
# controller must load nixio.fs and own the service status redirection.
# The view must not run status commands itself.
tar -xOzf "$work/luci/data.tar.gz" ./usr/lib/lua/luci/controller/hy2tun.lua \
	> "$work/luci/controller.lua"
tar -xOzf "$work/luci/data.tar.gz" ./usr/lib/lua/luci/view/hy2tun/status.htm \
	> "$work/luci/status.htm"
grep -q 'local fs = require "nixio.fs"' "$work/luci/controller.lua"
grep -q 'status >/dev/null 2>&1' "$work/luci/controller.lua"
! grep -q 'status >/dev/null 2>&1' "$work/luci/status.htm"

# The action endpoint must be POST-only (post("action") sets post = true)
# and the page must send the CSRF token with its form posts.
grep -q 'post("action")' "$work/luci/controller.lua"
grep -q 'id="hy2tun-csrf-token"' "$work/luci/status.htm"

# The page must expose the basic form, mapping table, and save control.
grep -q 'config get' "$work/luci/controller.lua"
grep -q 'id="hy2tun-server"' "$work/luci/status.htm"
grep -q 'id="hy2tun-mappings"' "$work/luci/status.htm"
grep -q 'id="hy2tun-save"' "$work/luci/status.htm"
echo "OpenWrt package checks passed"
