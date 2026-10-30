# OpenWrt client packages

The release publishes two OpenWrt packages:

- `hy2tun_<version>_<arch>.ipk` — the static client binary, procd init script,
  and a basic example configuration.
- `luci-app-hy2tun_<version>_all.ipk` — a LuCI Services page with status, a
  basic single-server configuration form, logs, and a link to the client Web
  console. It depends on `hy2tun (>= 1.5.0)`.

## Install

Download both files from the same GitHub Release, choosing the `hy2tun` package
that matches the router CPU (`uname -m`):

| Router CPU | Package suffix |
| --- | --- |
| x86_64 | `amd64` |
| aarch64 | `arm64` |
| ARMv7 / ARMv6 | `arm` |
| MIPS little-endian | `mipsle` |
| MIPS big-endian | `mips` |

```bash
opkg update
opkg install ./hy2tun_<version>_<arch>.ipk
opkg install ./luci-app-hy2tun_<version>_all.ipk
```

The `hy2tun` package installs:

- `/usr/bin/hy2tun`
- `/etc/init.d/hy2tun`
- `/etc/hy2tun/client.yaml.example`

Installation does not create `/etc/hy2tun/client.yaml` and does not enable the
service. If an active configuration already exists, the post-install script only
tightens its permissions to `0600`.

## Configure in LuCI

1. Install both packages and open **LuCI → Services → hy2tun**.
2. Fill in the basic form:
   - server address as `host:port`,
   - HY2 authentication password,
   - control token (at least 16 characters),
   - client name,
   - optional TLS SNI and “skip certificate verification”,
   - port mappings,
   - Web console listen address, username, and password.
3. Select **保存** (save) or **保存并启动** (save and start). Saving requires the
   service to be stopped. The client validates the configuration before it
   starts.
4. Use the start, stop, restart, enable, and disable buttons on the same page.
   Status and the most recent 50 log lines refresh from the page.
5. Once the service is running, select **打开控制台** (open console) to reach the
   client Web console, by default on port `7501`. Set a Web console password in
   the form first; the page refuses to start while the console is enabled
   without one.

When no configuration exists yet, the page reports “未创建” and the form starts
empty. Saving creates `/etc/hy2tun/client.yaml` with mode `0600`.

## Multi-server and advanced configuration

The basic form manages a single server. Use the client Web console or edit
`/etc/hy2tun/client.yaml` over SSH for:

- multiple servers (`servers:`),
- a custom CA or certificate pin,
- obfs, bandwidth limits, or UDP port hopping,
- local SOCKS5/HTTP proxies,
- Web console TLS or a custom log file.

See `examples/client.yaml` and `examples/client-multi.yaml` in the repository
for the full shape. The LuCI page detects a multi-server configuration and
shows a read-only notice. For a single-server configuration it preserves
advanced fields when saving the basic form, but YAML comments may be lost.

The client owns `/etc/hy2tun/client.yaml` and rewrites it when it receives
server pushes or web edits. Stop the service before external edits, then use
`/etc/init.d/hy2tun restart` (or start from the page). Do not configure procd to
watch this file.

## Build locally

From the repository root:

```bash
scripts/openwrt/build-ipk.sh ./hy2tun arm64 v1.5.0 dist
scripts/openwrt/build-luci-ipk.sh v1.5.0 dist
```

`build-ipk.sh` sets `Architecture: all` so opkg accepts the package on any
target, but the binary inside is architecture-specific. The filename records
the Go architecture; ship the matching file.

The package requires `ca-bundle` for TLS. The server (`hy2tund`) is intended for
a normal public Linux host, not OpenWrt.

## Scope

The LuCI app provides service status and control, a basic single-server
configuration form, logs, and a Web-console entry point. Advanced client
configuration remains in `/etc/hy2tun/client.yaml` and the built-in client
console. There is no `.ipk`-based server package.
