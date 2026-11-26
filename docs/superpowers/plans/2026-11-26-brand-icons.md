# hy2tun Brand Icons Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add one coherent “twin-node tunnel” identity across the Web consoles, Windows executables, OpenWrt/LuCI package, and social preview assets.

**Architecture:** A hand-authored SVG master defines the mark; a small generation script rasterizes the required Web/Windows/OG sizes. Web consoles serve their own embedded copies of the same assets and render the mark in the topbar. Windows command packages embed a generated `.syso`; the LuCI package carries a 32px PNG and exposes it when the theme supports a menu icon.

**Tech Stack:** SVG, ImageMagick `convert`, `windres`, Go embed/Web templates, LuCI Lua packaging

**Spec:** `docs/superpowers/specs/2026-11-26-brand-icons-design.md`

## Global Constraints

- Visual symbol: rounded badge with two endpoint nodes and a negative-space tunnel arch.
- Palette: light accent `#2f6b52`, dark accent `#78b493`, white symbol, dark symbol `#0f1511`, neutral `#62665f`, surface `#f6f6f4`.
- Flat vector only: no gradients, shadows, photographic elements, or text inside the app icon.
- App icons: full-bleed accent square with 12% symbol padding and no transparency.
- Web/favicon/social: rounded badge treatment.
- Mark must remain legible at 16 px.
- Do not add theme-specific LuCI CSS/JS hacks.
- Preserve existing UI typography; only the lockup/OG use DejaVu Sans.
- Do not modify production, publish releases, or create a version tag as part of this plan.

## Review Focus

- A 16px favicon can become a green blob; every generated PNG/ICO must be checked at 16px, not only at 512px.
- Dark-mode topbar contrast can fail if the mark keeps the light badge colors; the dark variant must use `#78b493` with `#0f1511`.
- Windows builds must not silently omit the icon; verify both `hy2tun.exe` and `hy2tund.exe`, not just the client.
- LuCI themes without per-entry icons must still load the package and menu; the icon is best-effort, not a reason to fail the controller.
- Web static assets are embedded independently; both serverui and clientui need the full set.

---

### Task 1: Master mark and raster asset generation

**Files:**
- Create: `assets/brand/mark.svg`
- Create: `assets/brand/mark-dark.svg`
- Create: `assets/brand/mark-mono.svg`
- Create: `assets/brand/app-icon.svg`
- Create: `assets/brand/lockup-light.svg`
- Create: `assets/brand/lockup-dark.svg`
- Create: `assets/brand/og-1200x630.svg`
- Create: `assets/brand/social-preview.png` (generated)
- Create: `scripts/icons/build-icons.sh`
- Test: `scripts/icons/test-icons.sh`

**Interfaces:**
- Produces SVG/PNG source assets in `assets/brand/` and a reproducible generation script.
- `scripts/icons/build-icons.sh` must be idempotent and only write into `assets/brand/` and the Web/Windows destinations named in later tasks.

- [ ] **Step 1: Write the asset verification test first**

Create `scripts/icons/test-icons.sh` that fails while assets are absent and later asserts:

```sh
#!/bin/sh
set -eu
root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
for f in mark.svg mark-dark.svg mark-mono.svg app-icon.svg \
         lockup-light.svg lockup-dark.svg og-1200x630.svg social-preview.png; do
  test -s "$root/assets/brand/$f" || { echo "missing assets/brand/$f" >&2; exit 1; }
done
for f in favicon.svg favicon-16.png favicon-32.png favicon.ico \
         apple-touch-icon.png icon-192.png icon-512.png site.webmanifest; do
  test -s "$root/internal/webui/serverui/static/$f" || { echo "missing server asset $f" >&2; exit 1; }
  test -s "$root/internal/webui/clientui/static/$f" || { echo "missing client asset $f" >&2; exit 1; }
done
identify -format '%f %wx%h\n' "$root/assets/brand/social-preview.png" | grep '1200x630' >/dev/null
for f in favicon-16.png favicon-32.png apple-touch-icon.png icon-192.png icon-512.png; do
  identify "$root/internal/webui/serverui/static/$f" >/dev/null
done
```

Run it and confirm it fails with a missing-asset message.

- [ ] **Step 2: Author the master SVG geometry**

Use a 512×512 viewBox. The master `mark.svg` uses a 40px inset rounded badge (`x=40 y=40 width=432 height=432 rx=96`) filled `#2f6b52`. Draw the symbol as a white arch from `(160,365)` up to `(160,275)`, over the top to `(352,275)`, down to `(352,365)`, with `stroke-width="44"` and round caps; add white circles at `(160,365)` and `(352,365)` with `r=34`. Keep 12% safe padding.

Create variants by changing only the palette/background:

- `mark-dark.svg`: badge `#78b493`, symbol `#0f1511`.
- `mark-mono.svg`: transparent background, symbol `#2f6b52`.
- `app-icon.svg`: 512×512 full-bleed `#2f6b52` square, no rounded background; same symbol scaled to the 12% safe area.
- `lockup-light.svg` / `lockup-dark.svg`: horizontal mark + `hy2tun` wordmark using `font-family="DejaVu Sans"`, `font-weight="600"`, with the wordmark color from the palette.
- `og-1200x630.svg`: surface background, mark at left, `hy2tun` wordmark, tagline `Reverse tunnel · HY2 + TCP`; no text below 24px.

- [ ] **Step 3: Implement the generation script**

`scripts/icons/build-icons.sh` must:

- render `mark.svg` → `favicon.svg` (or copy the badge SVG), `favicon-16.png`, `favicon-32.png`, `favicon.ico` with 16/32/48 sizes;
- render `app-icon.svg` → `apple-touch-icon.png` (180), `icon-192.png`, `icon-512.png` (full-bleed, as the design requires for installed/app icons);
- render `og-1200x630.svg` → `social-preview.png` at exactly 1200×630;
- copy the Web assets into both `internal/webui/serverui/static/` and `internal/webui/clientui/static/`;
- write `site.webmanifest` with `name`/`short_name` `hy2tun`, `theme_color` `#2f6b52`, background `#f6f6f4`, and 192/512 icons;
- fail clearly if `convert` is unavailable.

Use `convert -background none` for transparency, and `-resize`/`-define icon:auto-resize` for the ICO. Do not generate the Web copies in a way that changes bytes between runs.

- [ ] **Step 4: Run the generation and verify assets**

Run:

```sh
scripts/icons/build-icons.sh
scripts/icons/test-icons.sh
```

Expected: all assets exist; `social-preview.png` is 1200×630; all Web PNGs decode; no missing files.

- [ ] **Step 5: Commit**

```sh
git add assets/brand scripts/icons internal/webui/serverui/static internal/webui/clientui/static
git commit -m "feat(brand): add tunnel mark and generated icon assets"
```

---

### Task 2: Web consoles use the new mark, favicon, and manifest

**Files:**
- Modify: `internal/webui/serverui/templates/layout.html`
- Modify: `internal/webui/clientui/templates/layout.html`
- Modify: `internal/webui/serverui/static/app.css`
- Modify: `internal/webui/clientui/static/app.css`
- Test: `internal/webui/serverui/handlers_test.go`
- Test: `internal/webui/clientui/handlers_test.go`

**Interfaces:**
- Consumes assets from Task 1 in both `static/` trees.
- Produces identical icon metadata and mark rendering in both consoles.
- `brand-mark` remains an 18×18 visual element but is now an image/SVG, not a CSS pseudo-element.

- [ ] **Step 1: Add failing page assertions**

Add a serverui test asserting the rendered `/login` or authenticated page contains:

```go
require.Contains(t, body, `rel="icon"`)
require.Contains(t, body, `/static/favicon.svg`)
require.Contains(t, body, `/static/apple-touch-icon.png`)
require.Contains(t, body, `/static/site.webmanifest`)
require.Contains(t, body, `class="brand-mark"`)
```

Add the same assertions for the client login page/authenticated page. The test must fail before template changes.

- [ ] **Step 2: Add the head links and mark markup**

In both `layout.html` files add, in the existing `<head>`:

```html
<link rel="icon" href="/static/favicon.ico" sizes="any">
<link rel="icon" type="image/svg+xml" href="/static/favicon.svg">
<link rel="apple-touch-icon" href="/static/apple-touch-icon.png">
<link rel="manifest" href="/static/site.webmanifest">
<meta name="theme-color" content="#2f6b52">
```

Replace the empty `<span class="brand-mark" aria-hidden="true"></span>` with a decorative `<picture>`:

```html
<picture aria-hidden="true">
  <source srcset="/static/mark-dark.svg" media="(prefers-color-scheme: dark)">
  <img class="brand-mark" src="/static/mark.svg" width="18" height="18" alt="">
</picture>
```

- [ ] **Step 3: Update the CSS**

In both `app.css` files, remove the `background` and `::after` rules from `.brand-mark`; keep it 18×18 and non-shrinking:

```css
.brand-mark {
  width: 18px;
  height: 18px;
  flex: none;
  display: block;
}
```

Ensure the existing `.brand` gap/structure remains unchanged.

- [ ] **Step 4: Run Web tests and static checks**

```sh
go test ./internal/webui/serverui ./internal/webui/clientui -count=1
node --check internal/webui/serverui/static/app.js
node --check internal/webui/clientui/static/app.js
```

Expected: PASS; the rendered page contains all icon links and the mark.

- [ ] **Step 5: Commit**

```sh
git add internal/webui/serverui/templates/layout.html internal/webui/clientui/templates/layout.html internal/webui/serverui/static/app.css internal/webui/clientui/static/app.css internal/webui/serverui/handlers_test.go internal/webui/clientui/handlers_test.go
git commit -m "feat(web): use hy2tun brand icons in both consoles"
```

---

### Task 3: Windows executable icons

**Files:**
- Create: `assets/brand/windows/hy2tun.ico` (generated)
- Create: `assets/brand/windows/hy2tun.rc`
- Create: `assets/brand/windows/hy2tund.rc`
- Create: `cmd/hy2tun/hy2tun_windows_amd64.syso` (generated)
- Create: `cmd/hy2tund/hy2tund_windows_amd64.syso` (generated)
- Modify: `scripts/icons/build-icons.sh`
- Test: `scripts/icons/test-icons.sh`

**Interfaces:**
- Windows resource object files are named for `GOOS=windows GOARCH=amd64` so Go links them only for Windows/amd64.
- Both command packages embed the same icon and resource ID.
- `scripts/icons/build-icons.sh` regenerates `.ico` and `.syso` from the SVG master.

- [ ] **Step 1: Add failing Windows asset checks**

Extend `scripts/icons/test-icons.sh` to assert:

```sh
for f in cmd/hy2tun/hy2tun_windows_amd64.syso cmd/hy2tund/hy2tund_windows_amd64.syso; do
  test -s "$root/$f" || { echo "missing $f" >&2; exit 1; }
done
file "$root/cmd/hy2tun/hy2tun_windows_amd64.syso" | grep -E 'COFF|object' >/dev/null
file "$root/cmd/hy2tund/hy2tund_windows_amd64.syso" | grep -E 'COFF|object' >/dev/null
```

Run it; expect failure.

- [ ] **Step 2: Add the Windows resource files**

Create `hy2tun.rc`:

```rc
1 ICON "hy2tun.ico"
```

Create `hy2tund.rc` with the same icon reference. Generate the multi-size `.ico` from `app-icon.svg` with 16/24/32/48/64/128/256 layers.

- [ ] **Step 3: Generate `.syso` resources**

In `scripts/icons/build-icons.sh`, generate:

```sh
x86_64-w64-mingw32-windres -I assets/brand/windows -i assets/brand/windows/hy2tun.rc -o cmd/hy2tun/hy2tun_windows_amd64.syso
x86_64-w64-mingw32-windres -I assets/brand/windows -i assets/brand/windows/hy2tund.rc -o cmd/hy2tund/hy2tund_windows_amd64.syso
```

Run the script; verify the `.syso` files are non-empty COFF objects.

- [ ] **Step 4: Verify Windows builds and resources**

```sh
GOOS=windows GOARCH=amd64 go build ./...
scripts/icons/test-icons.sh
```

Expected: PASS; both Windows commands build and both `.syso` files exist. Inspect the `.exe` with `strings`/resource viewer if available; otherwise record that the resource object was linked.

- [ ] **Step 5: Commit**

```sh
git add assets/brand/windows cmd/hy2tun/hy2tun_windows_amd64.syso cmd/hy2tund/hy2tund_windows_amd64.syso scripts/icons
git commit -m "feat(windows): embed hy2tun application icon"
```

---

### Task 4: OpenWrt/LuCI package icon

**Files:**
- Create: `scripts/openwrt/luci-app-hy2tun/htdocs/luci-static/resources/icons/hy2tun.png` (32×32 generated)
- Modify: `scripts/openwrt/build-luci-ipk.sh`
- Modify: `scripts/openwrt/luci-app-hy2tun/luasrc/controller/hy2tun.lua`
- Modify: `scripts/icons/build-icons.sh`
- Test: `scripts/openwrt/test-ipk.sh`
- Test: `scripts/openwrt/test-luci-controller.sh`
- Test: `scripts/openwrt/test-luci-controller.lua`
- Test: `scripts/icons/test-icons.sh`

**Interfaces:**
- The package installs `/www/luci-static/resources/icons/hy2tun.png`; the source lives at `htdocs/luci-static/resources/icons/hy2tun.png` for standard LuCI packaging conventions.
- The controller sets an icon on the existing Services entry when the running LuCI theme supports per-entry icons; older themes must ignore the field without error.
- No theme-specific CSS/JS.

- [ ] **Step 1: Add failing package assertions**

Extend `scripts/openwrt/test-ipk.sh` to assert the LuCI IPK contains:

```sh
grep -q './www/luci-static/resources/icons/hy2tun.png' "$work/luci/files.txt"
```

Extend `scripts/icons/test-icons.sh` to assert the LuCI source PNG is exactly 32×32 after generation. Extend the Lua harness (`test-luci-controller.lua`) so its `entry` stub captures `icon`, and assert the page route has `icon == "icons/hy2tun.png"` after `controller.index()`. Run tests; expect failure.

- [ ] **Step 2: Generate and package the 32px icon**

In `scripts/icons/build-icons.sh`, render the full-color rounded badge mark to the LuCI source path at 32×32. Update `build-luci-ipk.sh` to install the LuCI static asset into the package's `/www` root (the source `htdocs` prefix is a LuCI source convention, not an install path):

```sh
install -d "$pkg/www/luci-static/resources/icons"
install -m 644 "$src/htdocs/luci-static/resources/icons/hy2tun.png" \
	"$pkg/www/luci-static/resources/icons/hy2tun.png"
```

- [ ] **Step 3: Set the menu icon best-effort**

In `hy2tun.lua`, assign the icon to the existing `admin/services/hy2tun` entry without changing its title/order/dependency:

```lua
local page = entry({"admin", "services", "hy2tun"}, template("hy2tun/status"), _("hy2tun"), 60)
page.dependent = false
page.icon = "icons/hy2tun.png"
```

The icon path is relative to `/luci-static/resources/`, so the installed file is `/www/luci-static/resources/icons/hy2tun.png`. If an installed legacy LuCI schema ignores the extra field, the entry must still load. Do not add per-theme CSS/JS.

- [ ] **Step 4: Run OpenWrt checks**

```sh
scripts/openwrt/test-ipk.sh
scripts/openwrt/test-luci-controller.sh
```

Expected: PASS; the icon path is present in the IPK, and the controller still loads.

- [ ] **Step 5: Commit**

```sh
git add scripts/openwrt scripts/icons
git commit -m "feat(openwrt): ship hy2tun LuCI icon"
```

---

### Task 5: Social preview, docs, and whole-project verification

**Files:**
- Create: `docs/assets/social-preview.png` (copy of the generated asset)
- Create: `assets/brand/README.md`
- Modify: `README.md`
- Modify: `README.zh-CN.md`
- Test: `scripts/icons/test-icons.sh`

**Interfaces:**
- `docs/assets/social-preview.png` is the operator-uploadable GitHub social preview.
- `assets/brand/README.md` documents source files, regeneration, and the LuCI theme limitation.
- README shows the mark/wordmark without adding unsupported product claims.

- [ ] **Step 1: Add the social preview and documentation**

Copy `assets/brand/social-preview.png` to `docs/assets/social-preview.png`. Add a short `assets/brand/README.md` covering:

- the master SVG and generated variants;
- `scripts/icons/build-icons.sh` and `scripts/icons/test-icons.sh`;
- the LuCI theme limitation;
- the manual GitHub social-preview upload step;
- the palette and no-gradient/no-shadow rules.

Add the mark/lockup to the README intro if it fits the existing layout; keep the change small and do not add marketing claims.

- [ ] **Step 2: Run whole-project verification**

```sh
scripts/icons/test-icons.sh
go test ./... -count=1
GOOS=windows GOARCH=amd64 go build ./...
node --check internal/webui/serverui/static/app.js
node --check internal/webui/clientui/static/app.js
scripts/openwrt/test-ipk.sh
scripts/openwrt/test-luci-controller.sh
git diff --check
```

Expected: all pass.

- [ ] **Step 3: Commit**

```sh
git add assets/brand docs/assets README.md README.zh-CN.md
git commit -m "docs(brand): document icon assets and social preview"
```

---

## Execution Handoff

After the plan is approved, use `superpowers:subagent-driven-development` (recommended: assets and platform integrations are independently reviewable) or `superpowers:executing-plans`. Do not publish or upgrade production as part of this plan; release authorization is separate.