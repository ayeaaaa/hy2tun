#!/usr/bin/env bash
# Build an OpenWrt .ipk for the hy2tun client.
#
# Usage: build-ipk.sh <hy2tun-binary> <goarch> <version> <output-dir>
#
# The package uses Architecture: all so it can be installed on any OpenWrt
# target; the caller must ship the binary matching the router CPU. The archive
# is named with the Go architecture to make that choice explicit.
set -euo pipefail

if [ "$#" -ne 4 ]; then
	echo "usage: $0 <hy2tun-binary> <goarch> <version> <output-dir>" >&2
	exit 2
fi

binary=$1
goarch=$2
version=${3#v}
outdir=$4
root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)

for tool in tar gzip; do
	command -v "$tool" >/dev/null 2>&1 || {
		echo "build-ipk: $tool is required" >&2
		exit 1
	}
done
[ -x "$binary" ] || { echo "build-ipk: binary is not executable: $binary" >&2; exit 1; }
mkdir -p "$outdir"
outdir=$(CDPATH= cd -- "$outdir" && pwd)

work=$(mktemp -d "${TMPDIR:-/tmp}/hy2tun-ipk.XXXXXX")
trap 'rm -rf "$work"' EXIT INT TERM

pkg="$work/data"
control="$work/control"
mkdir -p "$pkg/usr/bin" "$pkg/etc/init.d" "$pkg/etc/hy2tun" "$control"

install -m 0755 "$binary" "$pkg/usr/bin/hy2tun"
install -m 0755 "$root/scripts/openwrt/hy2tun.init" "$pkg/etc/init.d/hy2tun"
install -m 0600 "$root/examples/client-basic.yaml" "$pkg/etc/hy2tun/client.yaml.example"

cat > "$control/control" <<CONTROL
Package: hy2tun
Version: $version
Architecture: all
Maintainer: hy2tun contributors
Section: net
Priority: optional
Depends: ca-bundle
Installed-Size: $(du -sk "$pkg" | awk '{print $1}')
Description: HY2 reverse port-forwarding client
 Static OpenWrt client for hy2tun. Select the .ipk whose filename
 matches the router CPU architecture (mips, mipsle, arm, arm64,
 amd64). The package installs the binary, procd init script, and a
 basic example configuration. It does not create an active
 configuration or start the service. The server (hy2tund) is
 intended for a normal public Linux host.
CONTROL

cat > "$control/postinst" <<'POSTINST'
#!/bin/sh
# The package is Architecture: all because OpenWrt target names vary, but the
# binary is CPU-specific. Fail early instead of leaving an unusable install.
if ! /usr/bin/hy2tun -version >/dev/null 2>&1; then
	echo "hy2tun: binary does not run on this CPU; install the matching architecture package" >&2
	exit 1
fi
# Never create /etc/hy2tun/client.yaml and never start it at boot.
# Only tighten permissions when an active configuration already exists.
[ -f /etc/hy2tun/client.yaml ] && chmod 600 /etc/hy2tun/client.yaml 2>/dev/null
exit 0
POSTINST
chmod 0755 "$control/postinst"

(cd "$pkg" && tar --owner=0 --group=0 -czf "$work/data.tar.gz" .)
(cd "$control" && tar --owner=0 --group=0 -czf "$work/control.tar.gz" .)
printf '2.0\n' > "$work/debian-binary"

(
	cd "$work"
	tar --owner=0 --group=0 -czf "$outdir/hy2tun_${version}_${goarch}.ipk" \
		./debian-binary ./data.tar.gz ./control.tar.gz
)
echo "$outdir/hy2tun_${version}_${goarch}.ipk"
