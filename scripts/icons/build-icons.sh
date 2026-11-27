#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
brand="$root/assets/brand"
docs="$root/docs/assets"
server="$root/internal/webui/serverui/static"
client="$root/internal/webui/clientui/static"
luci_icons="$root/scripts/openwrt/luci-app-hy2tun/htdocs/luci-static/resources/icons"

if ! command -v convert >/dev/null 2>&1; then
  echo "build-icons: ImageMagick 'convert' is required but was not found in PATH" >&2
  echo "build-icons: install ImageMagick and re-run scripts/icons/build-icons.sh" >&2
  exit 1
fi

windres=x86_64-w64-mingw32-windres
if ! command -v "$windres" >/dev/null 2>&1; then
  echo "build-icons: x86_64-w64-mingw32-windres is required to build Windows resources but was not found in PATH" >&2
  echo "build-icons: install mingw-w64 binutils and re-run scripts/icons/build-icons.sh" >&2
  exit 1
fi

tmpdir=$(mktemp -d "${TMPDIR:-/tmp}/hy2tun-icons.XXXXXX")
trap 'rm -rf "$tmpdir"' EXIT HUP INT TERM

# Render an SVG source to an exact raster size. -strip plus an explicit 8-bit
# depth keeps PNG bytes stable across repeated runs and avoids volatile chunks.
render_png() {
  src=$1
  geometry=$2
  out=$3
  convert -background none "$src" -resize "$geometry" -strip -depth 8 "$out"
}

# Web favicon rasters come from the rounded badge mark.
render_png "$brand/mark.svg" 16x16 "$tmpdir/favicon-16.png"
render_png "$brand/mark.svg" 32x32 "$tmpdir/favicon-32.png"

convert -background none "$brand/mark.svg" \
  -define icon:auto-resize=16,32,48 \
  -strip -depth 8 "$tmpdir/favicon.ico"

# Installed/app icons are full-bleed accent squares (the OS applies its mask).
render_png "$brand/app-icon.svg" 180x180 "$tmpdir/apple-touch-icon.png"
render_png "$brand/app-icon.svg" 192x192 "$tmpdir/icon-192.png"
render_png "$brand/app-icon.svg" 512x512 "$tmpdir/icon-512.png"

# Windows executables use the full-bleed app icon. All layers Windows consumes
# are present; the 256px layer is PNG-compressed by the ICO encoder.
windows="$brand/windows"
install -d -m 755 "$windows"
convert -background none "$brand/app-icon.svg" \
  -define icon:auto-resize=16,24,32,48,64,128,256 \
  -strip -depth 8 "$tmpdir/hy2tun.ico"
install -m 644 "$tmpdir/hy2tun.ico" "$windows/hy2tun.ico"

# Go auto-links these objects only for GOOS=windows GOARCH=amd64. The resource
# scripts reference hy2tun.ico, resolved relative to the -I search directory.
# Compile both objects first, then install them together so a failed windres
# run cannot leave one tracked .syso updated and the other stale.
"$windres" -I "$windows" -i "$windows/hy2tun.rc" \
  -o "$tmpdir/hy2tun_windows_amd64.syso"
"$windres" -I "$windows" -i "$windows/hy2tund.rc" \
  -o "$tmpdir/hy2tund_windows_amd64.syso"
install -m 644 "$tmpdir/hy2tun_windows_amd64.syso" \
  "$root/cmd/hy2tun/hy2tun_windows_amd64.syso"
install -m 644 "$tmpdir/hy2tund_windows_amd64.syso" \
  "$root/cmd/hy2tund/hy2tund_windows_amd64.syso"

# LuCI serves per-entry menu icons from /luci-static/resources/icons. The
# package build maps this htdocs source tree onto /www.
install -d -m 755 "$luci_icons"
render_png "$brand/mark.svg" 32x32 "$tmpdir/hy2tun-luci.png"
install -m 644 "$tmpdir/hy2tun-luci.png" "$luci_icons/hy2tun.png"

# Social preview must be exactly 1200x630. The design asset matrix names both
# og-1200x630.png and social-preview.png; docs/assets is the operator copy.
render_png "$brand/og-1200x630.svg" '1200x630!' "$tmpdir/social-preview.png"
install -d -m 755 "$docs"
install -m 644 "$tmpdir/social-preview.png" "$brand/social-preview.png"
install -m 644 "$tmpdir/social-preview.png" "$brand/og-1200x630.png"
install -m 644 "$tmpdir/social-preview.png" "$docs/social-preview.png"
install -m 644 "$tmpdir/social-preview.png" "$docs/og-1200x630.png"

# favicon.svg is the rounded badge mark itself.
install -m 644 "$brand/mark.svg" "$tmpdir/favicon.svg"

cat > "$tmpdir/site.webmanifest" <<'EOF'
{
  "name": "hy2tun",
  "short_name": "hy2tun",
  "icons": [
    {
      "src": "icon-192.png",
      "sizes": "192x192",
      "type": "image/png"
    },
    {
      "src": "icon-512.png",
      "sizes": "512x512",
      "type": "image/png"
    }
  ],
  "theme_color": "#2f6b52",
  "background_color": "#f6f6f4",
  "display": "standalone"
}
EOF

# Both consoles embed their own static tree; install identical bytes in each.
for dir in "$server" "$client"; do
  install -m 644 "$brand/mark.svg" "$dir/mark.svg"
  install -m 644 "$brand/mark-dark.svg" "$dir/mark-dark.svg"
  install -m 644 "$tmpdir/favicon.svg" "$dir/favicon.svg"
  install -m 644 "$tmpdir/favicon-16.png" "$dir/favicon-16.png"
  install -m 644 "$tmpdir/favicon-32.png" "$dir/favicon-32.png"
  install -m 644 "$tmpdir/favicon.ico" "$dir/favicon.ico"
  install -m 644 "$tmpdir/apple-touch-icon.png" "$dir/apple-touch-icon.png"
  install -m 644 "$tmpdir/icon-192.png" "$dir/icon-192.png"
  install -m 644 "$tmpdir/icon-512.png" "$dir/icon-512.png"
  install -m 644 "$tmpdir/site.webmanifest" "$dir/site.webmanifest"
done
