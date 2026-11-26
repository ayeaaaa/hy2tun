# hy2tun brand icons design

- Date: 2026-11-26
- Status: draft for approval
- Scope: master mark, Web favicons/PWA icons, Web topbar mark, Windows executable icons, OpenWrt/LuCI menu icon, social preview image

## 1. Intent

Give hy2tun a consistent, recognizable identity across the parts users actually
see: browser tabs and both Web consoles, Windows executables, the OpenWrt/LuCI
menu, and the GitHub README/social preview. The symbol should communicate
“tunnel / connection” at a glance without relying on text, and should still be
legible at 16 px.

The product is infrastructure software: calm, precise, and restrained. The
icon should feel like a tool, not a consumer app.

## 2. Goals and non-goals

Goals:

- One master symbol used across every surface.
- Instantly readable as two endpoints connected through a tunnel/passage.
- Works in full color, monochrome, light mode, and dark mode.
- Reads at 16 px, 32 px, 180 px, and 512 px.
- Fits the existing Web palette: deep green accent (`#2f6b52`), bright green
  dark-mode accent (`#78b493`), slate neutrals, warm off-white surfaces.
- Flat vector construction: no gradients, shadows, photographic elements, or
  baked-in text in the app mark.
- No trademark imitation and no stock “network cloud” clichés.

Non-goals:

- Redesigning the application layout or typography.
- Creating a full marketing site.
- Adding animated icons.
- Changing product names, domains, or release branding.

## 3. Visual direction

### Selected approach: Twin-node tunnel

A rounded-square badge containing two endpoint nodes connected by a negative-space
tunnel arch. The arch is the dominant shape; the nodes anchor the “connection”
reading. The badge gives the mark a stable silhouette at small sizes and keeps
the symbol usable as an app icon.

Concretely:

- 512×512 master artboard.
- Rounded-square badge, corner radius about 22% of the short side.
- Two circular endpoint nodes, optically balanced left and right.
- A thick negative-space arch/passage between the nodes; the opening runs from
  the lower edge upward into the badge.
- White (or `#ffffff`) symbol on the accent-green badge.
- Monochrome variant: single solid color on transparent background.
- The symbol is centered with 12% padding; no text inside the favicon/app icon.

Why not the alternatives:

- **H-shaped bridge**: clearer letterform, but reads as “H” before it reads as a
  tunnel at 16 px.
- **Arrow through tunnel**: strongest motion cue, but the arrow becomes mushy at
  favicon sizes and feels more generic.

### Palette

| Token | Light | Dark | Use |
| --- | --- | --- | --- |
| Accent | `#2f6b52` | `#78b493` | Badge / primary mark |
| Symbol | `#ffffff` | `#0f1511` | Negative-space symbol (dark uses deep ink for contrast) |
| Neutral | `#62665f` | `#a4aaa5` | Wordmark secondary text |
| Surface | `#ffffff` | `#161917` | Icon background where needed |

The master app icon uses the accent badge with a white symbol for maximum
contrast. The Web topbar uses the same mark; dark mode uses the bright green
variant. A monochrome transparent mark exists for favicon fallback and LuCI.

### Typography

Wordmark: `hy2tun`, lowercase, slightly tightened letter spacing. The Web UI
continues to render the wordmark as text. The canonical lockup and OG asset use
DejaVu Sans SemiBold converted to outlined paths, so the asset does not depend
on fonts installed on the viewer's machine and carries no bundled font file.

## 4. Asset matrix

Master assets in `assets/brand/`:

- `mark.svg` — 512×512 full-color mark.
- `mark-dark.svg` — 512×512 dark-mode mark.
- `mark-mono.svg` — 512×512 single-color mark on transparent background.
- `lockup-light.svg` — horizontal mark + `hy2tun` wordmark, light surface.
- `lockup-dark.svg` — horizontal mark + wordmark, dark surface.
- `og-1200x630.png` — social preview image.

Derived Web assets (served by both Web consoles):

- `favicon.svg` — scalable favicon (full-color mark).
- `favicon-16.png`, `favicon-32.png` — raster fallbacks.
- `favicon.ico` — multi-size 16/32/48 icon.
- `apple-touch-icon.png` — 180×180 with solid background and safe padding.
- `icon-192.png`, `icon-512.png` — PWA/app icons.
- `site.webmanifest` — name, short name, theme/background colors, 192/512 icons.
- `og-image.png` — optional Web `<meta property="og:image">` asset if served.

Platform integration:

- **Web consoles (server + client)**: add favicon/link/manifest tags to both
  layouts; use the same mark in both topbars. Replace the current CSS-only
  `.brand-mark` square with an inline SVG or an `<img>` that preserves the
  current 18 px optical size.
- **Windows**: generate `cmd/hy2tun/hy2tun_windows_amd64.syso` and
  `cmd/hy2tund/hy2tund_windows_amd64.syso` from the multi-size `.ico` with the
  available `x86_64-w64-mingw32-windres`; Go links `.syso` automatically for
  `GOOS=windows`.
- **OpenWrt/LuCI**: add a 32×32 PNG under
  `scripts/openwrt/luci-app-hy2tun/htdocs/luci-static/resources/icons/hy2tun.png`
  and set the LuCI menu entry icon where the theme supports it. The package
  ships the icon unconditionally; no theme-specific CSS/JS override is added.
  On themes whose menu schema has no per-entry icon, the menu may show the
  normal Services icon; this is a documented LuCI theme limitation, not a
  failure of the package.
- **Social preview**: `og-1200x630.png` in `assets/brand/` and
  `docs/assets/`; GitHub repository social preview is uploaded by the operator
  because GitHub does not expose a stable API for that setting. The file is
  also suitable for release/README use.

## 5. Behaviour and constraints

- Favicon must be recognizable at 16 px; no thin strokes below 1.5 px at 16 px.
- Do not place text inside the square app icon.
- The mark must remain legible in monochrome and at 12% padding.
- App icon backgrounds are solid; no transparency behind the badge for Windows
  and Apple touch assets.
- Dark-mode Web mark uses the bright accent; it must not disappear against the
  dark topbar.
- No gradients, shadows, bevels, or photographic texture.
- Keep files small: prefer SVG for Web and master sources; raster assets only
  where the platform requires them.

## 6. Acceptance criteria

- `favicon` appears in both server and client console tabs.
- Web topbar shows the same mark at the current 18 px visual size in light and
  dark modes.
- Windows `hy2tun.exe` and `hy2tund.exe` show the icon in Explorer and taskbar.
- OpenWrt/LuCI package includes the 32×32 icon; the menu shows it on themes that
  support per-entry icons and degrades to the normal Services icon otherwise.
- 16/32/180/512 px renders remain legible and centered.
- Social preview asset is 1200×630 and contains the mark + wordmark, with no
  text smaller than 24 px.
- No generated raster is used as the only source; the SVG master remains the
  canonical asset.

## 7. Implementation approach

1. Author the master SVG geometry by hand (flat vector; no external image
   generation required). Use the selected palette and 12% safe padding.
2. Generate PNG/ICO derivatives with ImageMagick (`convert`) and package the
   multi-size ICO.
3. Add the SVG/PNG/manifest assets to both Web UI static trees and update both
   layout templates and the brand markup.
4. Generate Windows `.syso` resources with `x86_64-w64-mingw32-windres` and
   verify both Windows executables embed the icon.
5. Add the LuCI PNG and menu icon path; verify the IPK includes it.
6. Add the social preview asset and documentation; upload to GitHub repository
   settings is an operator step.
7. Add tests where the build system can assert file presence and generated
   resource behavior; visually inspect the raster matrix before release.

## 8. Builder-check resolutions

A fresh builder reviewed this document against the current Web consoles,
OpenWrt packaging, release matrix, and installed fonts. The remaining choices
are settled as follows:

- **LuCI**: ship the 32×32 PNG in the package and expose it through the menu
  entry when the theme supports per-entry icons. Do not add theme-specific
  CSS/JS hacks. Legacy themes that only render category icons keep the normal
  Services icon; document this limitation.
- **Raster composition**: Windows `.ico`, Apple touch, and PWA icons use a
  full-bleed accent square with the symbol at 12% padding and no transparency;
  the OS applies the outer mask. Web mark/favicon/social use the rounded badge
  on transparent/white backgrounds.
- **Wordmark font**: use outlined DejaVu Sans SemiBold paths for the canonical
  lockup and OG image; the Web UI keeps its existing font stack for live text.
- **Dark symbol**: use `#0f1511` on the bright dark-mode accent for legibility;
  the light app icon keeps the white symbol on deep green.
- **Web topbar**: implement with inline SVG when possible so the mark follows
  CSS color variables; otherwise provide light/dark SVG assets.
- **Social preview**: include the 1200×630 asset in `assets/brand/` and
  `docs/assets/`; uploading it to GitHub repository settings is an operator
  step, not an API/build step.

These decisions do not change the approved visual direction.