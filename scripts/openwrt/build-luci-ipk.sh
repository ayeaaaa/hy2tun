#!/usr/bin/env bash
# Build the architecture-independent LuCI menu/status package for hy2tun.
#
# Usage: build-luci-ipk.sh <version> <output-dir>
set -euo pipefail

if [ "$#" -ne 2 ]; then
	echo "usage: $0 <version> <output-dir>" >&2
	exit 2
fi

version=${1#v}
outdir=$2
root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
src="$root/scripts/openwrt/luci-app-hy2tun"

for tool in tar gzip; do
	command -v "$tool" >/dev/null 2>&1 || {
		echo "build-luci-ipk: $tool is required" >&2
		exit 1
	}
done
mkdir -p "$outdir"
outdir=$(CDPATH= cd -- "$outdir" && pwd)

work=$(mktemp -d "${TMPDIR:-/tmp}/hy2tun-luci-ipk.XXXXXX")
trap 'rm -rf "$work"' EXIT INT TERM
pkg="$work/data"
control="$work/control"
mkdir -p "$pkg/usr/lib/lua/luci" "$control"
cp -R "$src/luasrc/controller" "$pkg/usr/lib/lua/luci/"
cp -R "$src/luasrc/view" "$pkg/usr/lib/lua/luci/"

cat > "$control/control" <<CONTROL
Package: luci-app-hy2tun
Version: $version
Architecture: all
Maintainer: hy2tun contributors
Section: luci
Priority: optional
Depends: luci-base, luci-compat, hy2tun
Installed-Size: $(du -sk "$pkg" | awk '{print $1}')
Description: LuCI status page for the hy2tun client
 Adds a Services -> hy2tun page with service status, configuration
 location, and a link to the client web console.
CONTROL

cat > "$control/postinst" <<'POSTINST'
#!/bin/sh
rm -f /tmp/luci-indexcache* 2>/dev/null || true
exit 0
POSTINST
chmod 0755 "$control/postinst"

(cd "$pkg" && tar --owner=0 --group=0 -czf "$work/data.tar.gz" .)
(cd "$control" && tar --owner=0 --group=0 -czf "$work/control.tar.gz" .)
printf '2.0\n' > "$work/debian-binary"
(
	cd "$work"
	tar --owner=0 --group=0 -czf "$outdir/luci-app-hy2tun_${version}_all.ipk" \
		./debian-binary ./data.tar.gz ./control.tar.gz
)
echo "$outdir/luci-app-hy2tun_${version}_all.ipk"
