# hy2tun brand assets

Source artwork and generated icon variants for hy2tun. The mark is a flat
"twin-node tunnel": a rounded badge with two endpoint nodes joined by a
negative-space arch. The SVG masters are canonical; generated rasters are
build outputs, not edit sources.

## Masters

| File | Purpose |
| --- | --- |
| `mark.svg` | Primary 512x512 rounded badge (`#2f6b52` badge, white symbol). |
| `mark-dark.svg` | Dark-mode badge (`#78b493` badge, `#0f1511` symbol). |
| `mark-mono.svg` | Single-color transparent mark for monochrome use. |
| `app-icon.svg` | Full-bleed opaque square for installed/app rasters; the OS applies its outer mask. |
| `lockup-light.svg`, `lockup-dark.svg` | Horizontal mark plus outlined `hy2tun` wordmark for the READMEs. |
| `og-1200x630.svg` | Source for the 1200x630 social preview. |

The `hy2tun` wordmark in the lockups and the OG wordmark/tagline are outlined
DejaVu Sans paths, so they render identically without any font installed on
the viewer's machine. The canonical target is DejaVu Sans SemiBold; that face
is not installed in the build environment, so the outlines use DejaVu Sans
Bold, the locally available closest weight (and the same face the generated
rasters already used). The Web consoles keep rendering their wordmark as live
text.

## Generated variants

`scripts/icons/build-icons.sh` renders the masters into:

- `assets/brand/social-preview.png` and the identical
  `assets/brand/og-1200x630.png` (both 1200x630), plus the identical
  `docs/assets/social-preview.png` operator copy;
- Web favicons, Apple touch icon, PWA icons, `favicon.svg`,
  `site.webmanifest`, and copies of `mark.svg` / `mark-dark.svg` in both
  `internal/webui/serverui/static/` and `internal/webui/clientui/static/`;
- `assets/brand/windows/hy2tun.ico`, the checked-in `hy2tun.rc` /
  `hy2tund.rc` resource scripts, and the generated
  `cmd/hy2tun/hy2tun_windows_amd64.syso` and
  `cmd/hy2tund/hy2tund_windows_amd64.syso` resources;
- the 32x32 LuCI PNG at
  `scripts/openwrt/luci-app-hy2tun/htdocs/luci-static/resources/icons/hy2tun.png`.

Run the generator and its checks from the repository root:

```sh
scripts/icons/build-icons.sh
scripts/icons/test-icons.sh
```

Required host tools: ImageMagick (`convert`, `identify`) and mingw-w64 binutils
(`x86_64-w64-mingw32-windres`, plus `x86_64-w64-mingw32-objdump` for the
checks). The check script also uses `file` and `python3` to inspect the Windows
resources and validate the web manifest. On Debian/Ubuntu these are
`imagemagick`, `binutils-mingw-w64-x86-64`, `file`, and `python3`. The outlined
SVG sources need no font package or font tooling at build or render time.

## LuCI theme limitation

The package always ships the 32x32 PNG and sets the menu entry's icon. Themes
that support per-entry menu icons display it; legacy themes that render only
category icons show the normal Services icon. This is expected. No
theme-specific CSS or JavaScript is added to force the icon.

## Social preview

`assets/brand/social-preview.png`, `assets/brand/og-1200x630.png`, and
`docs/assets/social-preview.png` are the same 1200x630 render. The
`docs/assets` copy is the operator-uploadable file.

Uploading the GitHub repository social preview is manual: repository
**Settings → General → Social preview → Upload image**. GitHub does not expose
a stable API for that setting, so this repository cannot automate it.
Regenerating the PNG does not update GitHub; re-upload it after changes.

## Palette and construction rules

| Token | Light | Dark | Use |
| --- | --- | --- | --- |
| Accent | `#2f6b52` | `#78b493` | Badge / primary mark |
| Symbol | `#ffffff` | `#0f1511` | Negative-space symbol |
| Neutral | `#62665f` | `#a4aaa5` | Secondary wordmark text |
| Surface (as built) | `#f6f6f4` | `#161917` | OG/manifest background and surfaces |

The OG image and Web manifest use the as-built warm surface `#f6f6f4`. The
style is flat vector only: no gradients, shadows, bevels, photographic
texture, or text inside the app mark. Keep the symbol centered with 12% padding
and legible at 16 px. The light app icon uses a white symbol on `#2f6b52`; dark
mode uses `#0f1511` on `#78b493`.
