#!/bin/sh
# Build the manager-only release artifacts for linux/amd64 and linux/arm64.
#
# This intentionally does not touch the hy2tund/hy2tun release matrix in
# scripts/release.sh. Every manager archive contains the stripped manager
# binary, the example config, the systemd unit, and the operator docs as
# README.md, with deterministic metadata.
set -eu
umask 022

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$ROOT"

GO=${GO:-go}
VERSION=${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}
COMMIT=${COMMIT:-$(git rev-parse --short HEAD 2>/dev/null || echo none)}
DATE=${DATE:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}
DIST=${DIST:-dist}

case "$DIST" in
	/*) ;;
	*) DIST="$ROOT/$DIST" ;;
esac

ARCHIVES="hy2tun-manager_${VERSION}_linux_amd64.tar.gz hy2tun-manager_${VERSION}_linux_arm64.tar.gz"

DRY_RUN=false
for arg in "$@"; do
	case "$arg" in
		--dry-run)
			DRY_RUN=true
			;;
		*)
			echo "release-manager: unknown argument: $arg" >&2
			exit 2
			;;
	esac
done

if [ "$DRY_RUN" = true ]; then
	echo "hy2tun-manager release $VERSION (dry run)"
	for archive in $ARCHIVES; do
		echo "  $DIST/$archive"
	done
	exit 0
fi

command -v "$GO" >/dev/null 2>&1 || {
	echo "release-manager: $GO not found" >&2
	exit 1
}
for tool in tar gzip; do
	command -v "$tool" >/dev/null 2>&1 || {
		echo "release-manager: $tool is required" >&2
		exit 1
	}
done
for source in examples/manager.yaml scripts/systemd/hy2tun-manager.service docs/hy2tun-manager.md; do
	[ -f "$source" ] || {
		echo "release-manager: missing $source" >&2
		exit 1
	}
done

LDFLAGS="-s -w -X hy2tun/internal/version.Version=$VERSION -X hy2tun/internal/version.Commit=$COMMIT -X hy2tun/internal/version.Date=$DATE"

WORK="$DIST/.work-manager"
mkdir -p "$DIST"
rm -rf "$WORK"
rm -f "$DIST"/hy2tun-manager_*.tar.gz
mkdir -p "$WORK"

cleanup() {
	rm -rf "$WORK"
}
trap cleanup EXIT INT TERM

package() {
	os=$1
	arch=$2
	out="$WORK/$os-$arch"
	pkg="$WORK/pkg-$os-$arch"

	mkdir -p "$pkg"
	cp "$out/hy2tun-manager" "$pkg/hy2tun-manager"
	cp examples/manager.yaml "$pkg/manager.example.yaml"
	cp scripts/systemd/hy2tun-manager.service "$pkg/hy2tun-manager.service"
	cp docs/hy2tun-manager.md "$pkg/README.md"
	chmod 0755 "$pkg/hy2tun-manager"
	chmod 0644 "$pkg/manager.example.yaml" "$pkg/hy2tun-manager.service" "$pkg/README.md"

	name="hy2tun-manager_${VERSION}_${os}_${arch}"
	# Normalize ownership, modes, order, and timestamps so repeated builds of
	# the same commit produce byte-identical archives. gzip -n omits its own
	# timestamp header.
	(cd "$pkg" && tar --sort=name --mtime='@0' --owner=0 --group=0 --numeric-owner -cf - .) |
		gzip -n >"$DIST/$name.tar.gz"
	echo "==> packaged $DIST/$name.tar.gz"
}

build() {
	os=$1
	arch=$2

	echo "==> building $os/$arch"
	out="$WORK/$os-$arch"
	mkdir -p "$out"
	env CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" "$GO" \
		build -trimpath -ldflags "$LDFLAGS" \
		-o "$out/hy2tun-manager" ./cmd/hy2tun-manager
	package "$os" "$arch"
}

build linux amd64
build linux arm64

cleanup
trap - EXIT INT TERM

echo
echo "release manager $VERSION built in $DIST"
ls -lh "$DIST"/hy2tun-manager_*.tar.gz