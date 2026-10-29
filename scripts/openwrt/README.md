# OpenWrt client packages

The release publishes two OpenWrt packages:

- `hy2tun_<version>_<arch>.ipk` — the static client binary, procd init script,
  and example configuration.
- `luci-app-hy2tun_<version>_all.ipk` — a LuCI Services page with status,
  configuration path, and a link to the client Web console.

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

The post-install script copies the example to `/etc/hy2tun/client.yaml` if it
does not exist and enables the service. It does not start the service until the
configuration is valid.

## Configure and start

Edit `/etc/hy2tun/client.yaml` and set at least:

```yaml
server: "your-server.example.com:24443"
auth: "your-hy2-token"
control:
  token: "0123456789abcdef0123456789abcdef"
web:
  listen: "0.0.0.0:7501"
```

Then:

```bash
/etc/init.d/hy2tun start
logread -f | grep hy2tun
```

Open **LuCI → Services → hy2tun** for service status and the client Web console
link. The console is at port `7501` by default. Service enable/disable is also
available under **System → Startup**.

The client owns `/etc/hy2tun/client.yaml` and rewrites it when it receives
server pushes or web edits. Do not configure procd to watch this file; use
`/etc/init.d/hy2tun restart` after external edits.

## Build locally

From the repository root:

```bash
scripts/openwrt/build-ipk.sh ./hy2tun arm64 v1.4.0 dist
scripts/openwrt/build-luci-ipk.sh v1.4.0 dist
```

`build-ipk.sh` sets `Architecture: all` so opkg accepts the package on any
target, but the binary inside is architecture-specific. The filename records
the Go architecture; ship the matching file.

The package requires `ca-bundle` for TLS. The server (`hy2tund`) is intended for
a normal public Linux host, not OpenWrt.

## Scope

The LuCI app currently provides status and a Web-console entry point. Advanced
client configuration remains in `/etc/hy2tun/client.yaml` and the built-in
client console. There is no `.ipk`-based server package and no LuCI YAML editor.
