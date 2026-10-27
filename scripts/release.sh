#!/bin/sh
# Build the release matrix: linux/amd64, linux/arm64, linux/arm (GOARM=7),
# linux/mipsle, linux/mips (both softfloat), and windows/amd64.
#
# Every archive contains the stripped, statically linked server and client
# binaries, the example configurations, README, and the matching deployment
# files. dist/SHA256SUMS covers every archive.
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$ROOT"

GO=${GO:-go}
VERSION=${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}
COMMIT=${COMMIT:-$(git rev-parse --short HEAD 2>/dev/null || echo none)}
DATE=${DATE:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}
DIST=${DIST:-dist}

command -v "$GO" >/dev/null 2>&1 || {
	echo "release: $GO not found" >&2
	exit 1
}
for tool in tar sha256sum zip; do
	command -v "$tool" >/dev/null 2>&1 || {
		echo "release: $tool is required" >&2
		exit 1
	}
done

case "$DIST" in
	/*) ;;
	*) DIST="$ROOT/$DIST" ;;
esac

WORK="$DIST/.work"
LDFLAGS="-s -w -X hy2tun/internal/version.Version=$VERSION -X hy2tun/internal/version.Commit=$COMMIT -X hy2tun/internal/version.Date=$DATE"

rm -rf "$DIST"
mkdir -p "$WORK"

cleanup() {
	rm -rf "$WORK"
}
trap cleanup EXIT INT TERM

package() {
	os=$1
	arch=$2
	out=$3
	ext=$4

	pkg="$WORK/pkg-$os-$arch"
	mkdir -p "$pkg/examples"
	cp "$out/hy2tund-$os-$arch$ext" "$pkg/"
	cp "$out/hy2tun-$os-$arch$ext" "$pkg/"
	cp examples/server.yaml examples/client.yaml "$pkg/examples/"
	cp README.md "$pkg/"

	if [ "$os" = "linux" ]; then
		mkdir -p "$pkg/scripts/systemd" "$pkg/scripts/openwrt"
		cp scripts/systemd/hy2tund.service scripts/systemd/hy2tun.service "$pkg/scripts/systemd/"
		cp scripts/openwrt/hy2tun.init "$pkg/scripts/openwrt/"
	else
		mkdir -p "$pkg/scripts/windows"
		cp scripts/windows/README.md "$pkg/scripts/windows/"
	fi

	name="hy2tun_${VERSION}_${os}_${arch}"
	if [ "$os" = "windows" ]; then
		(cd "$pkg" && zip -qr "$DIST/$name.zip" .)
	else
		tar -C "$pkg" -czf "$DIST/$name.tar.gz" .
	fi
}

build() {
	os=$1
	arch=$2
	shift 2

	echo "==> building $os/$arch"
	out="$WORK/$os-$arch"
	mkdir -p "$out"
	ext=""
	if [ "$os" = "windows" ]; then
		ext=".exe"
	fi

	env CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" "$@" "$GO" \
		build -trimpath -ldflags "$LDFLAGS" \
		-o "$out/hy2tund-$os-$arch$ext" ./cmd/hy2tund
	env CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" "$@" "$GO" \
		build -trimpath -ldflags "$LDFLAGS" \
		-o "$out/hy2tun-$os-$arch$ext" ./cmd/hy2tun

	package "$os" "$arch" "$out" "$ext"
}

build linux amd64
build linux arm64
build linux arm GOARM=7
build linux mipsle GOMIPS=softfloat
build linux mips GOMIPS=softfloat
build windows amd64

# Checksum every archive, not the temporary build tree.
cd "$DIST"
: > SHA256SUMS
for f in hy2tun_*.tar.gz hy2tun_*.zip; do
	if [ -f "$f" ]; then
		sha256sum "$f" >> SHA256SUMS
	fi
done
sort -k2 -o SHA256SUMS SHA256SUMS

cleanup
trap - EXIT INT TERM

echo
echo "release $VERSION built in $DIST"
ls -lh "$DIST"
