#!/usr/bin/env bash
# Run the LuCI controller integration harness in an isolated temporary tree.
set -euo pipefail

root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
lua=${LUA:-lua5.1}
controller_src="$root/scripts/openwrt/luci-app-hy2tun/luasrc/controller/hy2tun.lua"
harness="$root/scripts/openwrt/test-luci-controller.lua"

if ! command -v "$lua" >/dev/null 2>&1; then
	echo "Lua interpreter not found: $lua" >&2
	exit 1
fi
if ! "$lua" -e 'require("cjson")' >/dev/null 2>&1; then
	echo "lua-cjson is required (the cjson module must be loadable by $lua)" >&2
	exit 1
fi

work=$(mktemp -d "${TMPDIR:-/tmp}/hy2tun-luci-test.XXXXXX")
cleanup() {
	rm -rf -- "$work"
}
trap cleanup EXIT HUP INT TERM

mkdir -p "$work/controller"
controller="$work/controller/hy2tun.lua"
cp "$controller_src" "$controller"

# Rewrite the production constants in the copied controller. The real
# /etc/hy2tun, /usr/bin/hy2tun, and /etc/init.d/hy2tun are never used.
escaped_work=$(printf '%s' "$work" | sed 's/[\\&|]/\\&/g')
sed -i \
	-e "s|/etc/hy2tun/client.yaml|$escaped_work/client.yaml|g" \
	-e "s|/usr/bin/hy2tun|$escaped_work/hy2tun|g" \
	-e "s|/etc/init.d/hy2tun|$escaped_work/hy2tun.init|g" \
	"$controller"

if grep -qE '/etc/hy2tun/client\.yaml|/usr/bin/hy2tun|/etc/init\.d/hy2tun' "$controller"; then
	echo "failed to rewrite controller paths for the test sandbox" >&2
	exit 1
fi

LUA_PATH="${LUA_PATH:+$LUA_PATH;}$root/scripts/openwrt/?.lua;;" \
HY2TUN_CONTROLLER="$controller" \
HY2TUN_TEST_DIR="$work" \
"$lua" "$harness"
