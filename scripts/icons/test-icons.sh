#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
brand="$root/assets/brand"
server="$root/internal/webui/serverui/static"
client="$root/internal/webui/clientui/static"
luci_icon="$root/scripts/openwrt/luci-app-hy2tun/htdocs/luci-static/resources/icons/hy2tun.png"

fail() {
  echo "test-icons: $*" >&2
  exit 1
}

# Master sources.
for f in mark.svg mark-dark.svg mark-mono.svg app-icon.svg \
         lockup-light.svg lockup-dark.svg og-1200x630.svg social-preview.png; do
  test -s "$brand/$f" || fail "missing assets/brand/$f"
done

# Web assets must exist in both independently embedded static trees.
for f in mark.svg mark-dark.svg favicon.svg favicon-16.png favicon-32.png favicon.ico \
         apple-touch-icon.png icon-192.png icon-512.png site.webmanifest; do
  test -s "$server/$f" || fail "missing server asset $f"
  test -s "$client/$f" || fail "missing client asset $f"
  cmp -s "$server/$f" "$client/$f" || fail "server/client copies differ: $f"
done
cmp -s "$brand/mark.svg" "$server/mark.svg" || fail "server mark.svg must match the master"
cmp -s "$brand/mark-dark.svg" "$server/mark-dark.svg" || fail "server mark-dark.svg must match the master"
cmp -s "$brand/mark.svg" "$server/favicon.svg" || fail "favicon.svg must be the rounded badge mark"

# Raster dimensions.
[ "$(identify -format '%wx%h' "$brand/social-preview.png")" = "1200x630" ] ||
  fail "social-preview.png must be 1200x630"

check_png_size() {
  path=$1
  want=$2
  got=$(identify -format '%wx%h' "$path")
  [ "$got" = "$want" ] || fail "expected $want for ${path#"$root"/}, got $got"
}
check_png_size "$server/favicon-16.png" 16x16
check_png_size "$server/favicon-32.png" 32x32
check_png_size "$server/apple-touch-icon.png" 180x180
check_png_size "$server/icon-192.png" 192x192
check_png_size "$server/icon-512.png" 512x512
test -s "$luci_icon" || fail "missing LuCI icon htdocs/luci-static/resources/icons/hy2tun.png"
check_png_size "$luci_icon" 32x32

for f in favicon-16.png favicon-32.png apple-touch-icon.png icon-192.png icon-512.png; do
  identify "$server/$f" >/dev/null || fail "cannot decode server asset $f"
  identify "$client/$f" >/dev/null || fail "cannot decode client asset $f"
done

# favicon.ico must carry 16/32/48 layers.
ico_sizes=$(identify -format '%wx%h\n' "$server/favicon.ico")
for size in 16x16 32x32 48x48; do
  printf '%s\n' "$ico_sizes" | grep -qx "$size" || fail "favicon.ico missing $size layer"
done
[ "$(printf '%s\n' "$ico_sizes" | wc -l | tr -d ' ')" = "3" ] ||
  fail "favicon.ico must contain exactly 16/32/48 layers"

# Windows executables share one multi-size icon and resource script.
for f in assets/brand/windows/hy2tun.ico \
         assets/brand/windows/hy2tun.rc \
         assets/brand/windows/hy2tund.rc; do
  test -s "$root/$f" || fail "missing $f"
done
windows_ico_sizes=$(identify -format '%wx%h\n' "$brand/windows/hy2tun.ico")
for size in 16x16 24x24 32x32 48x48 64x64 128x128 256x256; do
  printf '%s\n' "$windows_ico_sizes" | grep -qx "$size" ||
    fail "hy2tun.ico missing $size layer"
done
[ "$(printf '%s\n' "$windows_ico_sizes" | wc -l | tr -d ' ')" = "7" ] ||
  fail "hy2tun.ico must contain exactly 16/24/32/48/64/128/256 layers"
grep -qx '1 ICON "hy2tun.ico"' "$brand/windows/hy2tun.rc" ||
  fail "hy2tun.rc must reference the hy2tun.ico icon as resource ID 1"
grep -qx '1 ICON "hy2tun.ico"' "$brand/windows/hy2tund.rc" ||
  fail "hy2tund.rc must reference the hy2tun.ico icon as resource ID 1"

# Go links the Windows/amd64 resource objects; each must be a real COFF object
# carrying an .rsrc section, not merely a non-empty file.
command -v x86_64-w64-mingw32-objdump >/dev/null 2>&1 ||
  fail "x86_64-w64-mingw32-objdump is required to inspect Windows resources"
for f in cmd/hy2tun/hy2tun_windows_amd64.syso \
         cmd/hy2tund/hy2tund_windows_amd64.syso; do
  test -s "$root/$f" || fail "missing $f"
  file "$root/$f" | grep -E 'COFF|object' >/dev/null ||
    fail "$f is not a COFF object"
  x86_64-w64-mingw32-objdump -h "$root/$f" | grep -q '\.rsrc' ||
    fail "$f missing .rsrc section"
done

# Favicon marks are rounded badges with transparency; app icons are opaque full-bleed.
[ "$(identify -format '%[opaque]' "$server/favicon-16.png")" = "false" ] ||
  fail "favicon-16.png must keep transparent corners"
for f in apple-touch-icon.png icon-192.png icon-512.png; do
  [ "$(identify -format '%[opaque]' "$server/$f")" = "true" ] ||
    fail "$f must have an opaque full-bleed background"
done

# Master geometry and palette sanity.
grep -q 'viewBox="0 0 512 512"' "$brand/mark.svg" || fail "mark.svg must use a 512 viewBox"
grep -q 'rx="96"' "$brand/mark.svg" || fail "mark.svg must use the rounded badge"
grep -q '#2f6b52' "$brand/mark.svg" || fail "mark.svg missing light accent"
grep -q '#78b493' "$brand/mark-dark.svg" || fail "mark-dark.svg missing dark accent"
grep -q '#0f1511' "$brand/mark-dark.svg" || fail "mark-dark.svg missing dark symbol ink"
if grep -q '<text' "$brand/app-icon.svg"; then
  fail "app-icon.svg must not contain text"
fi
for f in lockup-light.svg lockup-dark.svg; do
  grep -q 'hy2tun' "$brand/$f" || fail "$f missing hy2tun wordmark"
  grep -q 'font-family="DejaVu Sans"' "$brand/$f" || fail "$f must use DejaVu Sans"
  grep -q 'font-weight="600"' "$brand/$f" || fail "$f must use font-weight 600"
done
grep -q 'hy2tun' "$brand/og-1200x630.svg" || fail "og-1200x630.svg missing wordmark"
grep -q 'Reverse tunnel · HY2 + TCP' "$brand/og-1200x630.svg" ||
  fail "og-1200x630.svg missing tagline"
for size in $(grep -o 'font-size="[0-9]*"' "$brand/og-1200x630.svg" | sed 's/[^0-9]//g'); do
  [ "$size" -ge 24 ] || fail "og-1200x630.svg contains text below 24px"
done

# The manifest must be valid JSON with the required identity and icons.
python3 - "$server/site.webmanifest" <<'PY'
import json
import sys

with open(sys.argv[1], encoding="utf-8") as handle:
    manifest = json.load(handle)

assert manifest.get("name") == "hy2tun", manifest.get("name")
assert manifest.get("short_name") == "hy2tun", manifest.get("short_name")
assert manifest.get("theme_color") == "#2f6b52", manifest.get("theme_color")
assert manifest.get("background_color") == "#f6f6f4", manifest.get("background_color")
icons = manifest.get("icons", [])
assert {"192x192", "512x512"} <= {icon.get("sizes") for icon in icons}, icons
assert {icon.get("src") for icon in icons} == {"icon-192.png", "icon-512.png"}, icons
for icon in icons:
    assert icon.get("type") == "image/png", icon
PY

echo "test-icons: all icon asset checks passed"
