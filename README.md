# hy2tun

[English](README.md) | [简体中文](README.zh-CN.md)

[![CI](https://github.com/ayeaaaa/hy2tun/actions/workflows/ci.yml/badge.svg)](https://github.com/ayeaaaa/hy2tun/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

`hy2tun` is a reverse port-forwarding tunnel built on the official
[Hysteria 2](https://github.com/apernet/hysteria) core. A client inside a
private network connects out to a public server and exposes selected TCP and
UDP services on server-side public ports. The same server remains a normal
HY2 proxy server, so existing Hysteria 2 clients keep working without any
changes.

- `hy2tund` — the public server: HY2 proxy endpoint, reverse-mapping public
  listeners, control endpoint, and management console.
- `hy2tun` — the private-network client: outbound HY2 connection, reverse TCP
  streams, reverse UDP datagrams, optional local SOCKS5/HTTP proxy, and a
  management console.

Both binaries are statically linked Go programs with embedded web assets. They
do not need a database, Node.js, a CDN, or any runtime dependency.

## What it does

- Standard HY2 proxy server for TCP and UDP, compatible with the official
  Hysteria 2 client.
- FRP-style reverse TCP mappings: a server public port forwards to a TCP
  service reachable from the client.
- Reverse UDP mappings using native HY2 datagrams, not a reliable stream, so
  real-time UDP is not head-of-line blocked.
- Client configuration is authoritative. The server console can push mapping
  changes to an online client; the client applies them, writes its YAML file
  atomically, and acknowledges the new revision.
- Port whitelist with single ports, ranges, deny rules, automatic allocation,
  and offline port reservations.
- One client process can connect to multiple independent servers. Each server
  has its own identity, mappings, credentials, reconnect loop, port hopping,
  and optional local SOCKS5/HTTP proxy.
- Optional client-side Hysteria 2 UDP port hopping across a port set, with
  automatic Linux server-side nftables/iptables redirect when `listen` is a
  range; a manual redirect remains supported for single-port listeners.
- Server and client web consoles with login, CSRF protection, live status,
  mapping management, and logs.
- Hot configuration editing from both consoles: web administrator, HY2
  credentials, server port policy, server trust-proxy, and client name are
  validated, written atomically, and applied without restarting the process.
- Generation-based credential rotation with a retained previous pair,
  automatic push to capable online clients or manual sync from the client
  console, and explicit revoke.
- Authenticated secret reveal for the current (and, on the server, previous)
  HY2 credential pair; values are masked by default and responses are
  `no-store`.
- Optional local SOCKS5 and HTTP CONNECT proxies on the client, using the same
  HY2 connection.
- Linux, OpenWrt, and Windows client builds from one source tree.

## When to use hy2tun

hy2tun fits when you have a public HY2 server and one or more clients behind
NAT, CGNAT, or a firewall, and you want to expose selected private services
without opening inbound ports on the private network.

Typical scenarios:

- Expose SSH, RDP, HTTP, a NAS, a home dashboard, or an internal API from a
  home or office network through a VPS.
- Expose UDP services such as WireGuard, DNS, game servers, voice, or MQTT
  where a reliable TCP tunnel would add head-of-line blocking.
- Use a public VPS as an egress for a client's local SOCKS5/HTTP proxy, while
  keeping the reverse mappings on the same HY2 connection.
- Run one client process against several independent servers, with separate
  mappings and optional proxies for each server.
- Use client-side UDP port hopping when a provider rate-limits or blocks a
  single UDP port.
- Run on Linux, OpenWrt, or Windows with one static binary and an embedded web
  console.

It is intentionally narrower than a general-purpose reverse-proxy platform.
If you need many proxy types, a plugin ecosystem, large multi-tenant
orchestration, or advanced routing/ACL features, FRP may be a better fit.
For the focused case above, hy2tun keeps the data path HY2-native, handles UDP
without converting it to a reliable stream, and includes the server/client
consoles and multi-server controls in the same binaries.

### Why HY2/UDP for lossy links

The primary design goal is not only NAT traversal. It is to move the lossy
server-to-client segment off a TCP tunnel and onto Hysteria 2/QUIC:

```text
public user --TCP--> hy2tund --HY2/QUIC over UDP (BBR)--> hy2tun --TCP--> private service
```

- The public user's TCP connection terminates at `hy2tund`; the long-haul
  server-to-client segment is carried by QUIC streams over UDP.
- Hysteria 2 uses BBR-style congestion control, which generally tolerates
  packet loss and high RTT better than a traditional TCP tunnel.
- QUIC streams are independent, so one stalled public TCP connection does not
  block unrelated mappings.
- Reverse UDP mappings use native HY2 datagrams, so real-time UDP is not
  converted into a reliable TCP stream.

A TCP mapping has three independent legs:

1. **Public user → `hy2tund`**: a normal TCP connection. Its loss,
   retransmission, and congestion control are handled by the user's and
   server's TCP stacks. If a packet is lost on this leg, it never reaches
   `hy2tund`, so HY2 cannot retransmit or repair it.
2. **`hy2tund` → `hy2tun`**: the HY2/QUIC-over-UDP tunnel. This is the leg
   that Hysteria 2/BBR improves, especially under loss and high RTT.
3. **`hy2tun` → private service**: another normal TCP connection. Its loss is
   handled by the client and the private service, not by HY2.

HY2 does not “repair TCP” end to end. It replaces the transport of leg 2. A
lossy public access network, a bad user-to-server route, or a lossy local
service link can still dominate the user's experience; server placement and
the endpoint TCP stacks matter there. UDP mappings are best-effort on every
leg: a datagram lost before `hy2tund` or after `hy2tun` is simply lost, and
HY2's native datagrams do not retransmit it either.

UDP blocking/QoS, MTU limits, CPU cost, and QUIC retransmission still apply. On
a clean, low-latency path a plain TCP tunnel may have less overhead; the
advantage is largest when leg 2 has meaningful loss or high RTT.

## What it does not do (v1)

- No multi-user accounts, approval workflow, tenant isolation, quotas, or
  billing. There is one HY2 authentication configuration and one control token
  per server; password auth also retains the previous pair during rotation.
- No P2P, Hysteria Realms, or UDP hole punching. The client always connects to
  the server.
- No TUN mode, transparent proxy, domain-based routing, ACLs, load balancing,
  or CDN fronting.
- The server console edits only the settings listed in
  [Hot configuration editing](#hot-configuration-editing); listen addresses,
  TLS, obfs, bandwidth, QUIC, web listener/TLS, logging, the state file, and
  the authentication mode/user table still require a file edit and restart.
- The client console does not edit the server address, port-hopping transport,
  TLS, obfs, bandwidth, client ID, proxy authentication, or web listener/TLS;
  those stay in `client.yaml`. It can rename the client, rotate the HY2 pair,
  and enable/disable or change each server's proxy listener addresses.
- Offline clients' mapping configuration is read-only in the server console;
  their detail page can delete the client and its persisted mappings and
  reservations. Their credentials can still be updated locally on the client
  host.
- No automatic update, tray application, or native Windows service. Windows
  uses Task Scheduler.
- Server-side range listening is Linux-only and requires root or
  `CAP_NET_ADMIN` plus nftables or iptables. Non-Linux servers can use only a
  single listen port with an external redirect; see
  [Port hopping](#port-hopping).
- SOCKS5 UDP ASSOCIATE is not implemented; the local SOCKS5 proxy supports
  TCP CONNECT only. HTTP CONNECT supports TCP only.
- No UPX packing.

## Architecture

```text
standard HY2 client ────── QUIC / HY2 ──────┐
                                            │
hy2tun client ──────────── QUIC / HY2 ──────┤
  │ control stream (register / sync / open)  │
  │ TCP data streams                        ├── hy2tund (public Linux server)
  │ native datagrams (UDP)                  │     ├── HY2 proxy egress (TCP/UDP)
  │                                         │     ├── public TCP listener per mapping
  ├── local web console (LAN)               │     ├── public UDP socket per mapping
  ├── local SOCKS5 / HTTP proxy (optional)  │     └── management web + API
  └── private-network TCP/UDP services      │
```

### Reverse TCP path

1. The client opens one long-lived control stream to the server.
2. A public user connects to a mapped server port.
3. The server creates a one-time 128-bit token and sends an `open` message on
   the control stream.
4. The client opens a data stream to `_hy2tun_data_<token>:0` and dials the
   local target.
5. The server's `Outbound.TCP` recognizes the virtual address, claims the
   waiting public connection, and lets the HY2 core copy bytes in both
   directions.
6. Closing either side closes the other.

### Reverse UDP path

1. The client opens one HY2 UDP session per mapping and sends a probe to
   `_hy2tun_udp_<token>:0`.
2. The server's `Outbound.UDP` maps the token to that mapping's public UDP
   socket.
3. A public datagram is delivered to the client with its public peer address.
4. The client keeps one local UDP socket per public peer, writes to the target,
   and sends replies back through the same HY2 UDP session.
5. A 20-second keepalive keeps the HY2 UDP session alive; idle peers are
   reaped after 60 seconds.

The virtual `_hy2tun_*` addresses are only used by the bundled client. A
standard HY2 client never opens them, so the wire protocol stays compatible.

## Requirements

- Go 1.26.10 or newer to build (standard-library security fixes). The `go.mod` file pins the HY2 core at v2.13.0.
- A public server with a TLS certificate (self-signed is supported when the
  client is configured with the matching CA, optionally combined with a
  certificate pin).
- UDP reachability to the server's HY2 port. TCP reachability to every public
  mapping port you use.
- Linux with systemd, OpenWrt with procd, or Windows with Task Scheduler for
  the provided deployment examples.

## Build

```bash
make build      # bin/hy2tund and bin/hy2tun
make test       # go test ./...
make race       # go test -race ./...
make release    # ./scripts/release.sh, artifacts in dist/
```

`make build` and the release script use `CGO_ENABLED=0`,
`-trimpath -ldflags "-s -w"`, so the binaries are static and stripped. The
release matrix is:

| OS | Architecture | Extra |
| --- | --- | --- |
| linux | amd64 | |
| linux | arm64 | |
| linux | arm | `GOARM=7` |
| linux | mipsle | `GOMIPS=softfloat` |
| linux | mips | `GOMIPS=softfloat` |
| windows | amd64 | `.exe` |

Each archive is named `hy2tun_<version>_<os>_<arch>.tar.gz` (or `.zip` for
Windows) and contains:

- `hy2tund` and `hy2tun` (with `.exe` on Windows), ready to install without
  renaming;
- `examples/server.yaml` and `examples/client.yaml`;
- `README.md` and `README.zh-CN.md`;
- the complete `scripts/` tree with systemd, OpenWrt, and Windows deployment
  notes;
- `SHA256SUMS` covering the two binaries.

`dist/SHA256SUMS` contains the SHA-256 of every archive. Set `VERSION`,
`COMMIT`, or `DATE` to override the values embedded in the binaries:

```bash
VERSION=1.0.0 make release
```

## Docker

The `Dockerfile` has `server` and `client` targets:

```bash
docker build --target server -t hy2tun-server .
docker build --target client -t hy2tun-client .
```

Use host networking so the HY2 UDP port and public mapping ports work:

```bash
docker run --network host --restart unless-stopped \
  -v "$PWD/server.yaml:/etc/hy2tun/server.yaml:ro" \
  -v "$PWD/certs:/etc/hy2tun/certs:ro" \
  -v hy2tund-state:/var/lib/hy2tun \
  hy2tun-server
```

See [`docker-compose.yml`](docker-compose.yml) for a two-service example. The
image runs as `nonroot`; use ports above 1024 or grant
`CAP_NET_BIND_SERVICE` if you need a privileged port.

## Release install

From an extracted release archive:

```bash
sudo ./scripts/install.sh server
sudo ./scripts/install.sh client
```

The script installs the binary and systemd unit, creates `/etc/hy2tun` and
`/var/lib/hy2tun`, and copies the example config only when it does not already
exist. Edit the config before starting the service.

## Configuration check

Validate a configuration without binding sockets or writing state:

```bash
hy2tund --check-config -c /etc/hy2tun/server.yaml
hy2tun --check-config -c /etc/hy2tun/client.yaml
```

The check validates YAML, required fields, TLS certificate/key pairs, web TLS,
bcrypt hashes, and log/state parent directories. It prints `configuration OK`
on success.

## Health and metrics

When the web console is enabled, both binaries expose:

- `GET /healthz` — returns `200` and `ok` when the process is serving.
- `GET /metrics` — Prometheus text metrics for aggregate traffic, mappings,
  connection state, credential generation, and uptime.

These endpoints are intentionally unauthenticated for local monitoring. Keep
the web listener on loopback, a VPN, or a trusted reverse proxy; do not expose
it directly to the public internet.

## Quick start

The complete example files are in [`examples/`](examples). They are valid
configurations but contain placeholders; replace every `change-me` value and
every path.

### 1. Server certificate

For a public deployment, use a normal certificate (for example Let's Encrypt)
and copy `fullchain.pem` and `privkey.pem` to `/etc/hy2tun/cert.pem` and
`/etc/hy2tun/key.pem`.

For a self-signed test certificate:

```bash
install -d -m 0755 /etc/hy2tun /var/lib/hy2tun
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 \
  -nodes -days 3650 -subj "/CN=hy2tun.example.com" \
  -addext "subjectAltName=DNS:hy2tun.example.com" \
  -keyout /etc/hy2tun/key.pem -out /etc/hy2tun/cert.pem
chmod 600 /etc/hy2tun/key.pem
```

Copy `/etc/hy2tun/cert.pem` to the client and set `tls.ca` to that path. If
the certificate is signed by a public CA, leave `tls.ca` empty and rely on the
system trust store.

### 2. Server configuration

Copy `examples/server.yaml` to `/etc/hy2tun/server.yaml` and edit it:

```yaml
listen: ":443"

auth:
  type: password
  password: "a-long-random-password"

tls:
  cert: "/etc/hy2tun/cert.pem"
  key: "/etc/hy2tun/key.pem"

control:
  token: "a-different-long-random-token"   # openssl rand -hex 32

credentials:
  generation: 1   # advanced automatically by credential rotation

ports:
  allow: ["2222", "8000-8100", "51820"]
  auto_range: "20000-29999"

web:
  listen: "127.0.0.1:7500"
  username: "admin"
  password_hash: ""

state_file: "/var/lib/hy2tun/state.json"
```

Start it:

```bash
sudo ./bin/hy2tund -c /etc/hy2tun/server.yaml   # release archive: ./hy2tund
```

If `web.password_hash` is empty, `hy2tund` generates a 20-character password,
prints it once to stderr, and writes its bcrypt hash into `server.yaml`. The
same happens for the client console. Save the printed password; it cannot be
recovered from the hash.

### 3. Client configuration

Copy `examples/client.yaml` to `/etc/hy2tun/client.yaml` and edit it:

```yaml
server: "hy2tun.example.com:443"
auth: "a-long-random-password"

tls:
  sni: "hy2tun.example.com"
  ca: "/etc/hy2tun/cert.pem"   # empty for a public CA
  insecure: false
  pin_sha256: ""

control:
  token: "a-different-long-random-token"

credentials:
  generation: 1   # updated by server push or manual sync

client:
  id: ""
  name: "home-router"
  revision: 1

mappings:
  - id: "m-ssh"
    type: tcp
    remote_port: 2222
    local_addr: "192.168.1.10:22"
    note: "NAS SSH"
    enabled: true
  - id: "m-wireguard"
    type: udp
    remote_port: 51820
    local_addr: "192.168.1.10:51820"
    note: "WireGuard"
    enabled: true

web:
  listen: "0.0.0.0:7501"
  username: "admin"
  password_hash: ""
```

Start it:

```bash
sudo ./bin/hy2tun -c /etc/hy2tun/client.yaml   # release archive: ./hy2tun
```

Open `http://<client-lan-ip>:7501/` and `http://127.0.0.1:7500/` on the server
(or use an SSH tunnel, see below). The first console login uses the generated
password printed at startup.

Both consoles have a **Settings** section for hot updates. See
[Hot configuration editing](#hot-configuration-editing) before changing
credentials or port policy.

### 4. Install as a service

**systemd (Linux server and client):**

From a source build:

```bash
sudo install -m 0755 bin/hy2tund bin/hy2tun /usr/local/bin/
```

From a release archive (run in the extracted directory):

```bash
sudo install -m 0755 hy2tund hy2tun /usr/local/bin/
```

Then install the units and enable the service:

```bash
sudo install -d -m 0755 /etc/hy2tun /var/lib/hy2tun
sudo install -m 0644 scripts/systemd/hy2tund.service /etc/systemd/system/
sudo install -m 0644 scripts/systemd/hy2tun.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now hy2tund    # on the server
sudo systemctl enable --now hy2tun     # on the client
sudo journalctl -u hy2tund -f
```

**OpenWrt (procd):**

Run these commands from the extracted Linux archive root:

```bash
opkg update && opkg install ca-bundle
cp hy2tun /usr/bin/hy2tun
chmod +x /usr/bin/hy2tun
install -d -m 0755 /etc/hy2tun
cp examples/client.yaml /etc/hy2tun/client.yaml
chmod 600 /etc/hy2tun/client.yaml
cp scripts/openwrt/hy2tun.init /etc/init.d/hy2tun
chmod +x /etc/init.d/hy2tun
/etc/init.d/hy2tun enable
/etc/init.d/hy2tun start
```

After editing `/etc/hy2tun/client.yaml` externally, apply the change with
`service hy2tun restart`. Do not watch that file with procd: the client
rewrites it on startup, assigned-port write-back, server push, and web edits,
so file watching would restart the tunnel on every write.

For MIPS routers use the `linux/mipsle` or `linux/mips` build; for ARM
routers use `linux/arm` or `linux/arm64`. The MIPS builds already use
`GOMIPS=softfloat`. Check `uname -m` on the router if you are unsure. If flash
space is tight, deploy only the client binary, or use extroot.

**Windows:** see [`scripts/windows/README.md`](scripts/windows/README.md) for
Task Scheduler instructions and firewall notes.

## Multiple servers

Use `servers` when one client process must connect to several independent
servers:

```yaml
web:
  listen: "0.0.0.0:7501"
log:
  level: "info"

servers:
  - name: "us"
    server: "us.example.com:24443"
    auth: "us-password"
    tls:
      ca: "/etc/hy2tun/us-ca.pem"
    control:
      token: "0123456789abcdef0123456789abcdef"
    client:
      id: ""                 # generated and persisted per server
      name: "home-us"
    mappings:
      - id: "ssh"
        type: "tcp"
        remote_port: 25022
        local_addr: "127.0.0.1:22"
        enabled: true
    proxy:
      enabled: false
      socks5: "127.0.0.1:1080"
      http: "127.0.0.1:8080"

  - name: "sg"
    server: "sg.example.com:24443"
    auth: "sg-password"
    control:
      token: "abcdef0123456789abcdef0123456789"
    client:
      id: ""
      name: "home-sg"
    mappings:
      - id: "ssh"
        type: "tcp"
        remote_port: 35022
        local_addr: "127.0.0.1:22"
        enabled: true
```

Rules:

- `servers` entries are completely independent; mappings are not inherited or
  shared. Configure the same local service separately on each server when you
  want it exposed through both.
- `servers[].name` must be unique. `servers[].client.id` is generated and
  persisted separately for each entry.
- Top-level `web` and `log` are shared by the process. When `servers` is used,
  the legacy single-server fields must be empty.
- A legacy top-level `server` configuration remains valid and appears in the
  console as the `default` server. It is not automatically rewritten to the
  multi-server shape.
- Each `servers[].proxy` defaults to disabled. Enable it from the client Web
  console or with `enabled: true`; at least one SOCKS5/HTTP address is required.
- Proxy listener ports must not conflict with another enabled proxy or
  `web.listen`. A failed bind disables only that server's proxy; its mappings
  and other servers continue.
- The old single-server API targets `default`. With a multi-server config that
  has no `default`, old server-scoped API calls return `404` rather than
  guessing a server.
- Proxy changes are hot-applied. Closing or changing a listener cancels
  in-flight proxy dials and closes affected active connections; unchanged
  listeners are reused. If a config rename succeeds but the following
  directory sync fails, the write is treated as committed and a warning is
  logged; runtime state follows the new file instead of rolling back.

## Configuration reference

### Server (`server.yaml`)

| Field | Meaning |
| --- | --- |
| `listen` | HY2 UDP listen address, for example `:443`, or a range such as `:30000-40000`. For a range, the first port is bound and the remaining ports are redirected automatically on Linux; see [Port hopping](#port-hopping). |
| `listen_host` | Optional address for public mapping listeners; empty binds all interfaces. |
| `auth.type` | `password` or `userpass`. |
| `auth.password` | HY2 password for `password` auth. |
| `auth.users` | Map of username to password for `userpass` auth. |
| `tls.cert`, `tls.key` | Server certificate and private key. Both are required. |
| `obfs.type` | Empty or `salamander`. |
| `obfs.salamander.password` | Shared obfuscation password. |
| `bandwidth.up`, `bandwidth.down` | HY2 bandwidth limits; `0` is unlimited. |
| `bandwidth.ignore_client` | Ignore the client's bandwidth settings. |
| `quic.*` | QUIC stream/connection windows, idle timeout, and incoming stream limit. |
| `control.token` | Shared control secret, at least 16 bytes. Use 32 random bytes. |
| `credentials.generation` | Current credential generation. Missing or `0` means `1`; rotation writes it. |
| `credentials.previous_generation` | Previous generation retained during rotation; `0` means none. |
| `credentials.previous_auth` | Previous HY2 password accepted until revoke or a later rotation replaces it. Required with `previous_generation`. |
| `credentials.previous_control_token` | Previous control token accepted until revoke or a later rotation replaces it. Required with `previous_generation`. |
| `ports.allow` | Ports or ranges clients may request. Empty means all free ports. |
| `ports.deny` | Ports or ranges that are always rejected. Deny wins over allow. |
| `ports.auto_range` | Range used for `remote_port: 0` when `allow` is empty. Default `20000-29999`. |
| `ports.offline_reserve` | How long an offline client keeps a soft reservation. Default `24h`. Hot updates require at least `1s`; `0` is rejected. |
| `web.listen` | Management address. The example uses `127.0.0.1:7500`. |
| `web.username` | Console username. Default `admin` when `web.listen` is set. |
| `web.password_hash` | bcrypt hash. Empty generates a password on first start. |
| `web.trust_proxy` | Trust `X-Forwarded-For`/`X-Real-IP` for login rate limiting. Enable only behind a proxy that overwrites both headers with the direct client address. Default `false`. |
| `web.tls.cert`, `web.tls.key` | Optional HTTPS certificate pair for the console. |
| `state_file` | Persistent state JSON. Empty keeps state in memory. |
| `log.level` | `debug`, `info`, `warn`, or `error`. |
| `log.file` | Optional log file. Empty logs to stderr/journal. |

### Client (`client.yaml`)

| Field | Meaning |
| --- | --- |
| `server` | Legacy single-server `host:port`, or a port set such as `host:443,8443` or `host:30000-40000` for port hopping. Use `servers` for multiple independent servers. |
| `servers` | Optional list of independent server entries; see [Multiple servers](#multiple-servers). Cannot be combined with legacy per-server fields. |
| `auth` | HY2 password, or `user:password` for userpass auth. |
| `tls.sni` | TLS server name. Empty uses the host part of `server`. |
| `tls.insecure` | Skip certificate verification. Do not use on untrusted networks. |
| `tls.pin_sha256` | SHA-256 of the server certificate's DER bytes. |
| `tls.ca` | CA certificate bundle for a private/self-signed server certificate. |
| `obfs.type` | Must match the server. Empty or `salamander`. |
| `bandwidth.up`, `bandwidth.down` | Optional client-side HY2 limits. |
| `transport.udp.hop_interval` | Fixed port-hop interval for a multi-port `server`. Default 30s; minimum 5s. |
| `transport.udp.min_hop_interval`, `max_hop_interval` | Random hop interval; both required, minimum 5s, mutually exclusive with `hop_interval`. |
| `control.token` | Must match `control.token` on the server. |
| `credentials.generation` | Client credential generation. Missing or `0` means `1`; server push or manual sync writes it. The client ignores the `previous_*` fields; they are reserved. |
| `client.id` | Stable client ID. Empty generates one on first start. |
| `client.name` | Display name in the server console. |
| `client.revision` | Configuration revision. The client updates it automatically. |
| `mappings` | List of reverse mappings (see below). |
| `web.listen` | Client console address. The example uses `0.0.0.0:7501`. |
| `web.username`, `web.password_hash` | Console login. Empty hash generates a password. |
| `web.trust_proxy` | Trust `X-Forwarded-For`/`X-Real-IP` for login rate limiting. Enable only behind a proxy that overwrites both headers with the direct client address. Default `false`. |
| `web.tls.cert`, `web.tls.key` | Optional HTTPS for the client console. |
| `proxy.enabled` | Per-server proxy switch, default `false`. Can also be changed from the client console. |
| `proxy.socks5` | Optional local SOCKS5 listen address, for example `127.0.0.1:1080`. Editable from the client console. |
| `proxy.socks5_auth` | Optional `user:password` for SOCKS5. File-only. |
| `proxy.http` | Optional local HTTP CONNECT listen address. Editable from the client console. |
| `proxy.http_auth` | Optional `user:password` for HTTP CONNECT. File-only. |
| `log.level`, `log.file` | Logging configuration. |

`credentials.generation` is a status label, not an authentication factor; do
not decrement it. The server's `credentials.previous_*` fields are maintained
by rotation and revoke. They may be set by hand to pre-seed a pair, but the
server requires `previous_generation` to be `0` or a positive value less than
`generation`, and requires both previous secrets when it is non-zero. Like the
current secrets, they live in `server.yaml` and its `.bak`, both mode `0600`.

### Mapping fields

```yaml
- id: "m-ssh"                 # unique within this client; generated if empty
  type: tcp                   # tcp or udp
  remote_port: 2222           # 0 = server allocates from the allowed range
  local_addr: "192.168.1.10:22"
  note: "NAS SSH"             # optional, shown in both consoles
  enabled: true               # false keeps it in the file but does not register it
```

Changing a mapping's type, public port, or local target closes active public
connections for that mapping. Changing only the note keeps connections open.
Deleting a mapping closes its listener/socket and any active connections.

### Port whitelist syntax

`ports.allow` and `ports.deny` are YAML string lists. Each item is one of:

- a single port: `"2222"`
- an inclusive range: `"8000-8100"`

Rules:

- An empty `allow` list permits every free port.
- A non-empty `allow` list restricts both explicit requests and automatic
  allocation to those ranges.
- `deny` always wins, even if the port is also in `allow`.
- `remote_port: 0` allocates from `allow` when it is non-empty, otherwise from
  `ports.auto_range`.
- The server reserves its own HY2 listen port and web port, and skips ports
  already bound by another mapping.
- TCP and UDP are checked independently: TCP 2222 and UDP 2222 may coexist.
- Offline reservations are soft: automatic allocation skips them for
  `offline_reserve`, but another client may explicitly request the port.

### Generating a web password hash

The simplest path is to leave `web.password_hash: ""`. On first start the
program generates a 20-character password, prints it once to stderr, and saves
only the bcrypt hash in the YAML file. Restarting does not print it again.

To choose your own password, generate a bcrypt hash with `htpasswd`:

```bash
htpasswd -bnBC 12 "" 'your-password' | tr -d ':\n'
# $2y$12$...
```

Or with Python's `bcrypt` package:

```bash
python3 -c 'import bcrypt; print(bcrypt.hashpw(b"your-password", bcrypt.gensalt()).decode())'
```

Paste the output into `web.password_hash`. The plaintext is never written to
disk or logged. Restart the process after changing the hash.

### TLS pinning

`tls.pin_sha256` pins the server certificate itself, independent of the CA
chain:

```bash
openssl x509 -in cert.pem -outform der | sha256sum
```

The value is the 64-character hex digest (an optional `sha256:` prefix and
colons are accepted). Pinning does not replace normal certificate
verification: keep `tls.ca` configured, or set `tls.insecure: true` only if you
understand the trade-off. When both a CA and a pin are configured, both must
match.

## Hot configuration editing

Both web consoles have a **Settings** section. A change is validated, written
to the YAML file atomically with mode `0600` (the previous file is kept as
`.bak`), and only then applied to the running process. If the write fails, the
running configuration is unchanged and the API returns an error. No restart is
needed for the settings marked online below.

Changing the web administrator credentials, the trust-proxy setting, and
revealing/rotating/revoking HY2 credentials require the current web
administrator password. Routine port policy changes require only a valid
session and the CSRF header/Origin; they do not ask for the password again.
Pushing or deleting a mapping from a client detail page also requires no
password re-entry. Failed password confirmations are rate-limited to five per
minute per client IP. All mutations require the CSRF header and same-origin
`Origin` described in [Security model](#security-model).

### Which settings are online-editable

**Server (`hy2tund`)**

| Setting | Online editing |
| --- | --- |
| `web.username`, `web.password_hash` | Yes. The password can only be reset, never shown. Saving revokes every web session. |
| `auth.password`, `control.token` | Yes, when `auth.type: password`. Rotation only; see below. |
| `ports.allow`, `ports.deny`, `ports.auto_range`, `ports.offline_reserve` | Yes. Mappings already running are not closed when a new allow list no longer covers them. `offline_reserve` must be at least `1s`. |
| `web.trust_proxy` | Yes. Applies immediately to login and settings-confirmation rate limiting. |
| Mappings of online clients | Yes. Push an authoritative list or delete a mapping. Offline clients stay read-only. |
| Offline clients | Yes. Delete from the client detail page; removes the persisted client, mappings, and soft port reservations. Online clients must disconnect first. |
| `credentials.generation`, `credentials.previous_*` | Managed by rotation and revoke. Not edited directly in the console. |
| `listen`, `listen_host` | No. Edit the file and restart. |
| `tls.*`, `obfs.*`, `bandwidth.*`, `quic.*` | No. Edit the file and restart. |
| `web.listen`, `web.tls.*` | No. Edit the file and restart. |
| `auth.type`, `auth.users` | No. `userpass` cannot rotate credentials online; edit the file and restart. |
| `state_file`, `log.*` | No. Edit the file and restart. |

**Client (`hy2tun`)**

| Setting | Online editing |
| --- | --- |
| `web.username`, `web.password_hash` | Yes. Password reset only; saving revokes every web session. |
| `client.name` | Yes. Written to `client.yaml`, revision bumped, and sent to the server immediately. |
| `auth`, `control.token` | Yes, through manual credential sync only. The new generation must be greater than the local one; the client reconnects. |
| `mappings` | Yes. Add, update, and delete; the server can also push a list. |
| `client.revision` | Updated automatically when mappings, assigned ports, or the client name change. |
| `credentials.generation` | Managed by server push or manual sync. |
| `server`, `transport.*`, `tls.*`, `obfs.*`, `bandwidth.*`, `client.id` | No. Edit the file and restart. |
| `web.listen`, `web.trust_proxy`, `web.tls.*` | No. Edit the file and restart. |
| `proxy.enabled`, `proxy.socks5`, `proxy.http` | Yes, per server from the client console; addresses must not conflict. |
| `proxy.socks5_auth`, `proxy.http_auth`, `log.*` | No. Edit the file and restart. |

The client `web.trust_proxy` setting is file-only in this release; only the
server console can toggle trust-proxy at runtime.

### Web administrator password

The web administrator username is shown in the console. The password is stored
only as a bcrypt hash and cannot be revealed; the console can only reset it.
A reset requires the current password, a new password of at least 8 characters,
and confirmation. Saving updates `web.password_hash`, keeps the YAML file and
live login in sync, and revokes all existing web sessions, so log in again with
the new credentials. Changing the username has the same session effect.

### Credential generations

For password authentication, the server's current HY2 pair is
`auth.password` plus `control.token`, and the client's current pair is `auth`
plus `control.token`. `credentials.generation` labels that pair. A missing or
`0` generation is treated as `1`; this keeps older configuration files
working.

When the server rotates credentials it increments `generation` by one and
moves the old current pair into `credentials.previous_generation`,
`credentials.previous_auth`, and `credentials.previous_control_token`. The
server accepts both the current and the previous HY2 password and control
token until the previous pair is revoked or replaced by another rotation. On
the server, the generation is used only for status reporting and for closing
stale control sessions; it is not an authentication factor. A capable client
that reports the previous generation is reported as `pending`.

The client only tracks `credentials.generation`.
`previous_generation`, `previous_auth`, and `previous_control_token` are
reserved and ignored in `client.yaml`; the client never retains or uses
previous credentials. A pushed or manually applied rotation replaces its
current pair. The server's previous pair is accepted for authentication only;
it is never pushed to clients.

### Rotating credentials from the server console

1. Open **Settings → HY2 凭据**.
2. Enter a new HY2 password, a new control token, or both. A blank field keeps
   the current value. The HY2 password must be at least 8 bytes; the control
   token must be at least 16 bytes.
3. Choose a sync mode:
   - **自动推送 / auto** sends a `credential_update` to each online client that
     advertises `credential_update_v1`, waits up to five seconds per client for
     its acknowledgement, and returns a result per client.
   - **手动同步 / manual** sends no push. Every online client stays `pending`
     until it is updated from its own console.
4. Confirm with the current web administrator password.

The server writes the new generation to `server.yaml` before swapping the live
credentials, so a failed write leaves the old pair active. A rotation with both
fields blank, or with values identical to the current pair, is rejected as
`no_change`. Rotating only one field is allowed; the retained previous pair is
the old current pair.

Auto-push results are:

- `synced` — the client wrote the new pair and acknowledged it; the server
  then closes the old control stream so the client reconnects.
- `pending` — the client has not confirmed the new generation (manual mode,
  not ready, or a write/ack failure).
- `unsupported` — the client did not advertise `credential_update_v1`; it was
  not sent the message and must be updated manually.
- `timeout` — no matching acknowledgement arrived within five seconds.

Auto-push never retries automatically. The settings page polls
`GET /api/settings` and shows the current status of every online client; offline
clients are listed with an unknown generation. Offline clients are not pushed;
clients still on the previous generation keep working until it is revoked or
superseded by a later rotation.

A second rotation while a previous pair exists replaces that previous pair.
Control sessions on generations older than the new previous generation are
closed immediately, because those clients can no longer authenticate after a
reconnect; update them manually.

### Manual client sync

If a client is offline, old, or was not auto-pushed, update it from its own
console:

1. On the server **Settings → HY2 凭据**, note the current generation (or
   reveal the current pair).
2. On the client **Settings → HY2 凭据**, enter a new generation greater than
   the local generation, plus the new HY2 password and control token.
3. Confirm with the client's current web administrator password. The client
   writes `client.yaml` atomically, closes the current connection, and
   reconnects with the new pair.

The client page shows the server generation from the last `hello_ack` as a
hint. A value that does not match is allowed, but the sync badge may stay
`pending` until the next handshake reports the server's current generation.
The generation is display-only; authentication still depends on the password
and token matching.

You can also edit `auth`, `control.token`, and `credentials.generation`
directly in `client.yaml` and restart the client.

### Revoking the previous pair

**撤销旧凭据 / Revoke previous credentials** clears
`credentials.previous_generation`, `credentials.previous_auth`, and
`credentials.previous_control_token` from `server.yaml` and makes only the
current pair valid. The server closes every control session that is not on the
current generation. Those clients keep trying to reconnect with the old pair
and fail until they are updated manually.

For the standard Hysteria 2 proxy path, a new connection using the old password
fails immediately after revoke. A connection that was already authenticated
can continue until it closes naturally, because Hysteria authenticates per
connection. This is expected.

### Revealing secrets

The settings pages show only a fixed mask (`********` on the server and
`••••••••` on the client); the mask does not reveal the secret's length.
`GET /api/settings` never contains plaintext passwords or tokens.

To show the plaintext:

1. Enter the current web administrator password in the reveal field.
2. Click **显示凭据 / Show credentials**.

The server returns its current pair and, when present, the previous pair; the
client returns only its current pair. Reveal requires a valid web session and
the current password, returns `Cache-Control: no-store`, is never written to
logs or URLs, and is rate-limited after five failed password confirmations per
minute per client IP. The page automatically re-masks after 30 seconds, and
plaintext is not kept after a refresh.

The web administrator password is not part of this endpoint and can never be
shown.

### Compatibility and old clients

New clients send `credential_generation` and
`capabilities: ["credential_update_v1"]` in the control `hello`. The server
uses the capability to gate auto-push and to mark non-capable clients as
`unsupported`; it never sends them a `credential_update` message:

- **Old client, new server** — after a rotation, a client that omits
  `credential_update_v1` is never sent a `credential_update` message, so it
  does not disconnect on an unknown message. It is marked `unsupported` and
  keeps working with the previous credentials until they are revoked or
  superseded by a later rotation. Update the client binary, or edit
  `client.yaml` manually and restart it.
- **Old client, missing generation** — a `hello` without
  `credential_generation` is treated as generation `1`. If the server is still
  on generation `1`, the client is reported as `synced`.
- **New client, old server** — the old server ignores the new `hello` fields
  and its `hello_ack` has no generation. The client shows the server generation
  as unknown and stays `pending`; the tunnel still works, but server-driven
  rotation is unavailable. Edit both files manually.
- **Old configuration files** — a missing `credentials` section is generation
  `1` with no previous pair, so existing deployments upgrade in place.
- **Standard Hysteria 2 clients** — they do not use the hy2tun control protocol
  and are unaffected except that the server accepts the current or previous
  HY2 password until revoke or until a later rotation replaces the previous
  pair, as described above.

### Write order and failure behavior

Every hot update follows the same order: validate the input, build a new
configuration, write it atomically (temporary file, fsync, rename, and `.bak`),
then swap the runtime state. If validation or the file write fails, the
running configuration, credential set, port policy, and web login are left
unchanged. A client credential update is written to `client.yaml` before the
`credential_ack` is sent, so an acknowledged rotation is durable.

## Client management

The server keeps offline clients in its state file so their mappings and port
reservations survive restarts. To forget a client that will not return, open its
detail page and choose **Delete client**. This removes the persisted client, all
of its mappings, and its soft port reservations. The action cannot be undone.

The button is available only while the client is offline. If it is online,
disconnect it first by stopping the client or its control stream. Deleting a
client does not revoke its HY2 credentials; if the same client reconnects, it
registers again. Rotate credentials or remove its configuration when it must not
return.

The equivalent API call is `DELETE /api/clients/{id}` with a valid session,
`X-Requested-With: hy2tun`, and a same-origin `Origin`. It returns `204` on
success, `409 client_online` for a live client, and `404 client_not_found` for
an unknown or already deleted client.

## Port hopping

Hysteria 2 supports UDP port hopping: instead of a single fixed port, the
client sends its HY2 connection to a set of UDP ports and moves to another port
at a fixed or random interval. It helps with per-port UDP rate limiting or
blocking; it does not hide the server, defeat a blanket UDP block, or replace
TLS.

Port hopping affects only the client-to-server HY2 UDP connection. The server's
public reverse-mapping ports (for example `2222/tcp` and `51820/udp`) do not
hop and keep their configured ports.

### Client configuration

`server` accepts the Hysteria 2 port-set syntax. Each of these is valid:

- `hy2tun.example.com:443` — single port, no hopping
- `hy2tun.example.com:443,8443` — two ports
- `hy2tun.example.com:30000-40000` — inclusive range
- `hy2tun.example.com:443,30000-30100,8443` — mixed list

Configure the interval under `transport.udp`:

```yaml
server: "hy2tun.example.com:30000-40000"

transport:
  udp:
    hop_interval: "30s"        # fixed interval, minimum 5s

# Or a random interval; both fields are required:
# transport:
#   udp:
#     min_hop_interval: "15s"
#     max_hop_interval: "45s"
```

- A multi-port `server` without an interval uses 30s. The default is applied at
  runtime and is not written back to `client.yaml`.
- `hop_interval`, `min_hop_interval`, and `max_hop_interval` must each be at
  least 5s and use Go duration syntax (`30s`, `1m`, `1m30s`).
- `hop_interval` is mutually exclusive with `min_hop_interval` and
  `max_hop_interval`. The random form requires both fields and `min <= max`.
- A single-port `server` with any interval is rejected. A port set that
  normalizes to one unique port (`443-443`, `443,443`) is treated as a single
  port.
- `transport` is file-only; it is not editable from the web console. Restart
  the client after changing `server` or `transport`.
- Salamander obfs wraps every hop socket, so obfs configuration is unchanged.

When hopping is enabled the client logs a warning at startup:

```text
port hopping enabled; server must listen on the range or redirect it to the HY2 listen port
```

The warning is logged at `Warn`, so `log.level: error` suppresses it. When
shown, it is expected: the client cannot verify the server's firewall rules.

### Server listener and redirect

Set `listen` to a single port or a range:

```yaml
listen: ":30000-40000"
```

The server binds the first port (`30000`) and redirects the remaining ports
(`30001-40000`) to it at startup. On Linux it prefers nftables and falls back
to iptables/ip6tables. Force a backend with
`HY2TUN_FIREWALL_BACKEND=nft` or `HY2TUN_FIREWALL_BACKEND=iptables`. The rules
are removed when the server closes. The server needs root or
`CAP_NET_ADMIN`; if the redirect cannot be installed, startup fails.

Point the client at the same range:

```yaml
server: "hy2tun.example.com:30000-40000"
```

The first port is the actual HY2 listener; the remaining ports are redirects.
A single-port `listen: ":24443"` does not install any redirect. Use the manual
fallback below if you want client-side hopping with a single listen port.

On non-Linux systems, use a single listen port and an external redirect.

#### Manual redirect fallback

If you keep `listen: ":24443"` and want the client to hop over
`30000-40000`, install the redirect yourself. Use nftables or iptables, not
both.

nftables:

```nft
table inet hy2tun_porthop {
  chain prerouting {
    type nat hook prerouting priority dstnat; policy accept;
    udp dport 30000-40000 counter redirect to :24443
  }
}
```

Load the file with `nft -f`, or create the table, chain, and rule incrementally.

Add:

```bash
nft add table inet hy2tun_porthop
nft add chain inet hy2tun_porthop prerouting \
  '{ type nat hook prerouting priority dstnat; policy accept; }'
nft add rule inet hy2tun_porthop prerouting \
  udp dport 30000-40000 counter redirect to :24443
```

Verify:

```bash
nft list table inet hy2tun_porthop
```

Delete:

```bash
nft delete table inet hy2tun_porthop
```

iptables and ip6tables. The `table inet` nftables example above covers IPv4 and
IPv6 with one rule; the iptables path needs both commands.

Add:

```bash
iptables -t nat -A PREROUTING -p udp --dport 30000:40000 \
  -j REDIRECT --to-ports 24443
ip6tables -t nat -A PREROUTING -p udp --dport 30000:40000 \
  -j REDIRECT --to-ports 24443
```

Verify:

```bash
iptables -t nat -L PREROUTING -n -v
ip6tables -t nat -L PREROUTING -n -v
```

Delete:

```bash
iptables -t nat -D PREROUTING -p udp --dport 30000:40000 \
  -j REDIRECT --to-ports 24443
ip6tables -t nat -D PREROUTING -p udp --dport 30000:40000 \
  -j REDIRECT --to-ports 24443
```

Requirements:

- A Linux server with nftables or iptables/ip6tables.
- root or `CAP_NET_ADMIN` to install the rule.
- The cloud security group must allow the whole UDP range to the instance,
  because it filters before the host's NAT. The host firewall only needs to
  allow the first listen port in its INPUT chain, because `REDIRECT` happens in
  PREROUTING.
- If clients may use IPv4 and IPv6, allow the range and the listen port for
  both protocols.
- Automatic redirects are runtime rules and do not need persistence. Manual
  redirects must be persisted across reboot (distribution firewall
  configuration, `nftables.service`, `iptables-persistent`, or a systemd unit).

### Port conflicts

Keep the hop range outside every UDP port the server may use for reverse
mappings:

- every explicit UDP mapping (`type: udp`, including `remote_port: 0`
  allocations);
- `ports.allow`: every entry that covers a UDP port. When `ports.allow` is
  non-empty it is also the source of automatic `remote_port: 0` allocations, so
  the whole list matters;
- `ports.auto_range`, used for automatic allocation when `ports.allow` is empty;
  the default is `20000-29999`.

With the default `auto_range`, use `30000-40000` rather than `20000-30000`.
TCP mapping ports may overlap the UDP hop range because the protocols are
independent, but keeping them separate is easier to audit. A UDP mapping inside
the hop range would have its traffic redirected to the HY2 listener and stop
working.

The server reserves every port in `listen`, including all ports in a range, and
rejects mappings that try to use them. Keep the hop range outside
`ports.allow` and `ports.auto_range` as well: reserved ports are skipped during
automatic allocation, but a clean separation makes explicit mappings and audits
predictable.

### Verifying the redirect

1. Confirm the rule exists and its packet counter increases:
   `nft list tables | grep hy2tun_porthop_` for automatic redirects, or
   `nft list ruleset` / `iptables -t nat -L PREROUTING -n -v` for manual rules.
2. On the server, watch conntrack while the client runs:

   ```bash
   conntrack -L -p udp | grep 'sport=30000'
   ```

   The reply tuple's source port is the first listen port (`30000` in the
   example), while the original destination port is from the hop range. Over
   several intervals you should see more than one original destination port.
3. Alternatively, capture arriving packets:

   ```bash
   tcpdump -ni any 'udp and dst portrange 30000-40000'
   ```

   `tcpdump` sees packets before NAT, so the destination port is the hop port.
4. Keep a TCP mapping and a UDP mapping busy while hopping. Both should keep
   working; only the HY2 connection changes ports. UDP is not retransmitted, so
   an in-flight datagram can be lost when the client switches sockets and a UDP
   mapping may show a brief gap at a hop. TCP streams and the HY2 connection
   recover.
5. Remove the redirect and confirm the client eventually fails after moving to
   an unredirected port; restore the rule to recover.

`udphop` chooses a random port on every interval and may choose the current
port again, so a single interval without a visible change is normal. Check for
at least one change across several intervals.

If hopping does not work, check in this order:

- The client startup log contains the port hopping warning. A missing warning
  means either `server` is a single port or `log.level` / `log.file` hides it;
  check both `server` and `log.level`.
- The cloud security group allows the whole UDP range and the host firewall
  allows the listen port.
- The redirect target matches the first port in `listen`, and the rule is in
  the `nat` PREROUTING hook.
- `conntrack` shows reply tuples with the listen port as the source port.
- IPv6 clients are not hitting an IPv4-only rule.

### Compatibility

- Single-port clients behave exactly as before; old configuration files need
  no changes.
- Standard Hysteria 2 clients can use the same `host:port,port-range` syntax
  when the server has a redirect or range listener. hy2tun uses snake_case
  interval fields; standard Hysteria 2 configuration uses `hopInterval`,
  `minHopInterval`, and `maxHopInterval`, so do not copy that block verbatim.
- A standard Hysteria 2 server with a manual redirect works with a hy2tun
  client, and a standard Hysteria 2 client works against a hy2tun server with a
  range listener or a manual redirect.
- Windows and OpenWrt clients can hop because they only make outbound UDP
  connections; automatic server redirect management requires Linux, while a
  manual redirect can be provided by any Linux NAT host.
- With Salamander obfs enabled, every hop socket is wrapped; both ends must use
  the same obfs password.

## Deployment notes

### Firewall and NAT

On the server, allow:

- UDP to the `listen` port (usually 443) from the internet;
- TCP to every explicit public mapping port (for example 2222);
- UDP to every explicit public UDP mapping port (for example 51820);
- TCP to the web console only from your management network, VPN, or reverse
  proxy. The example `127.0.0.1:7500` is not reachable from outside the host.

Automatic ports (from `allow`, or from `auto_range` when `allow` is empty) also
need to be reachable. Open the whole range, or pin mappings to explicit ports
and open only those. The client makes only outbound connections, so it does not
need router port forwarding.

With port hopping, also allow the whole UDP hop range in the cloud security
group. Let the server install the redirect with a range `listen`, or install a
manual redirect as described in [Port hopping](#port-hopping). The host INPUT
chain only needs the first `listen` port, because the redirect happens in
PREROUTING.

On the client host, allow outbound UDP to the server's HY2 port. Windows may
show a firewall prompt for the client web console; allow it on private
networks only.

### Exposing the server web console safely

The example server console binds to `127.0.0.1:7500`. Prefer one of:

1. **SSH tunnel** from your workstation:

   ```bash
   ssh -L 7500:127.0.0.1:7500 root@hy2tun.example.com
   # then open http://127.0.0.1:7500/
   ```

2. **VPN / Tailscale / WireGuard**: bind `web.listen` to the VPN address, or
   keep loopback and reach it through the VPN.

3. **TLS reverse proxy** such as nginx or Caddy. The proxy must preserve the
   `Host` header and set `X-Forwarded-Proto`, because the CSRF check compares
   the `Origin` header with the request host and scheme. Disable proxy
   buffering for the SSE endpoint.

   ```nginx
   server {
       listen 443 ssl;
       server_name hy2tun.example.com;

       ssl_certificate     /etc/letsencrypt/live/hy2tun.example.com/fullchain.pem;
       ssl_certificate_key /etc/letsencrypt/live/hy2tun.example.com/privkey.pem;

       location / {
           proxy_pass http://127.0.0.1:7500;
           proxy_set_header Host $host;
           proxy_set_header X-Forwarded-Proto $scheme;
           proxy_set_header X-Forwarded-For $remote_addr;
           proxy_set_header X-Real-IP $remote_addr;
           proxy_buffering off;
           proxy_read_timeout 1h;
       }
   }
   ```

   The console uses `X-Forwarded-Proto` to mark the session cookie `Secure` and
   to send HSTS, so proxied HTTPS deployments get both. Set
   `web.trust_proxy: true` only when this proxy overwrites **both**
   `X-Forwarded-For` and `X-Real-IP` with the direct client address (the
   example sets both to `$remote_addr`). Prefer overwriting over
   `$proxy_add_x_forwarded_for`: overwriting removes client-supplied entries
   entirely, while an appending proxy leaves the result dependent on proxy
   behavior and chain length. When trusted, the limiter prefers `X-Real-IP` and
   otherwise uses the last `X-Forwarded-For` entry, so the nearest proxy's
   value wins. Leave `trust_proxy` `false` when clients can reach the console
   directly, because forwarded headers are otherwise spoofable and would let an
   attacker evade the limiter.

Do not expose the console directly on a public interface over plain HTTP. The
console has its own login, rate limiting, and CSRF protection, but TLS protects
the password and session cookie in transit.

### Security model

- HY2 authentication and the control token are separate secrets. Leaking the
  HY2 password does not let someone register reverse mappings, and leaking the
  control token does not let someone use the proxy.
- Control tokens are compared in constant time.
- Data-stream and UDP tokens are 128-bit random values, are never derived from
  mapping IDs, and are never logged.
- The server enforces the port whitelist; a client cannot bypass it.
- Web passwords are stored as bcrypt hashes. Session cookies are HMAC-signed,
  `HttpOnly`, and `SameSite=Lax`, and are marked `Secure` (with HSTS) when the
  request is HTTPS directly or through a TLS reverse proxy; mutating APIs
  require `X-Requested-With: hy2tun` and a matching `Origin`.
- Web administrator passwords are reset-only and never returned by any API.
  A reset requires the current password and revokes every web session.
- HY2 credentials are masked in `GET /api/settings`. The reveal endpoint
  requires the current web administrator password, returns `Cache-Control:
  no-store`, never writes plaintext to logs or URLs, and rate-limits failed
  confirmations.
- Hot settings are persisted before they take effect. A failed validation or
  file write leaves the running credential set, port policy, and web login
  unchanged.
- Rotation keeps the previous HY2 pair valid until an explicit revoke or a
  later rotation replaces it; revoke closes control sessions that are not on
  the current generation. Standard Hysteria 2 proxy connections that were
  already authenticated can survive until they close naturally, because HY2
  authenticates per connection.
- Login rate limiting keys on the direct peer by default. Set
  `web.trust_proxy: true` only behind a proxy that overwrites both
  `X-Forwarded-For` and `X-Real-IP`; otherwise forwarded headers are ignored so
  a client cannot forge a fresh IP per attempt. When trusted, `X-Real-IP` wins
  and the last `X-Forwarded-For` entry is the fallback.
- Configuration and state files are written with mode `0600` and atomic
  rename. Previous contents are kept as `.bak`.
- Passwords, tokens, and private keys are never logged.
- The client console uses HTTP unless `web.tls` is configured. On a LAN,
  enable `web.tls` or use a trusted VPN; otherwise a LAN attacker can sniff the
  management session.

## Troubleshooting

### `control handshake rejected: unauthorized`

The client's `control.token` does not match the server's. Copy the exact value
from `server.yaml` to `client.yaml` (at least 16 bytes; 32 random bytes is
recommended). The token is never printed by either process.

### TLS errors

- `x509: certificate is valid for ...` — the `server` address or `tls.sni` does
  not match a SAN in the certificate. Set `tls.sni` to a name in the
  certificate, or issue a certificate for the address you use.
- `x509: certificate signed by unknown authority` — for a self-signed server,
  copy its certificate to the client and set `tls.ca`; do not use
  `tls.insecure` on an untrusted network.
- `server certificate pin mismatch` — `tls.pin_sha256` is not the SHA-256 of
  the current server certificate. Regenerate it after renewing the
  certificate.
- A reverse proxy in front of the console must preserve `Host`; otherwise
  mutations fail with `403 forbidden`.

### Firewall or connectivity

- The client log should show a completed control handshake. If it repeatedly
  logs `connect HY2`, UDP to the server port is blocked, the address is wrong,
  or the server is not listening.
- Test UDP reachability from the client network:

  ```bash
  nc -vzu hy2tun.example.com 443
  ```

  A UDP "success" only means a packet was sent; check the server log for the
  HY2 listener and the client log for the handshake.
- Public mapping ports are opened on the server, not on the client. If a
  mapping is `running` in both consoles but unreachable, check the server's
  cloud firewall/security group and any NAT in front of the server.
- A mapping in `failed` state shows a stable error code such as
  `port_in_use`, `port_not_allowed`, `port_reserved`, or `no_free_port`.
  `port_not_allowed` means the requested port is outside `ports.allow` or
  matched `ports.deny`.
- With port hopping, a client that connects on one port but stalls after the
  first hop usually has a missing or partial server redirect. Check the
  `conntrack`/`tcpdump` steps in [Port hopping](#port-hopping) and the cloud
  security group range.

### UDP MTU and large datagrams

Reverse UDP uses HY2 datagrams. Each HY2 UDP message has a fixed internal
buffer of 4096 bytes including the address header, so payloads larger than
roughly 4 KB are dropped. QUIC also performs path-MTU discovery, and networks
with a smaller MTU can drop large datagrams before they arrive.

- WireGuard's default 1420-byte packets normally fit. If handshakes work but
  bulk traffic stalls, lower the tunnel MTU (for example to 1280) on both
  ends.
- Test with increasing payload sizes and watch for loss:

  ```bash
  iperf3 -c <server-ip> -p <mapped-udp-port> -u -b 100M -t 30 -l 1200
  iperf3 -c <server-ip> -p <mapped-udp-port> -u -b 100M -t 30 -l 1400
  ```

- UDP is intentionally not retransmitted. A lost datagram is lost; use TCP
  mappings for protocols that need reliability.

### Web console login

- If no password was printed, `web.password_hash` was already set. Use that
  password or replace the hash with a new one and restart.
- Five failed logins from one IP trigger a one-minute lockout. Wait and retry,
  or restart the process (the limiter is in memory). Behind a TLS reverse
  proxy, all users otherwise share the proxy's IP; set `web.trust_proxy: true`
  when the proxy overwrites both `X-Forwarded-For` and `X-Real-IP` with the
  direct client address so each client is limited separately.
- If the console is reachable but pages redirect to `/login`, the session
  cookie is missing or expired. Log in again; restarting the process
  invalidates sessions but not the persisted signing key.
- If `web_secret` exists but is unreadable or corrupt, the process cannot
  start. Delete it and restart to generate a new key; this invalidates all
  existing web sessions. Check that the configuration directory is writable
  by the process user.

### Credential rotation and sync

- `unsupported` in the server sync table means the client predates
  `credential_update_v1`. It was not sent the new pair and stays connected on
  the previous credentials until they are revoked or superseded by a later
  rotation. Update the client, or edit `client.yaml` and restart it.
- `pending` or `timeout` after auto-push means the client did not confirm the
  new generation (not ready, a config write/ack failure, or no acknowledgement
  within five seconds). The previous pair is still valid, so the tunnel keeps
  working. The server has already persisted the new pair: submitting the same
  values again returns `no_change`, and there is no re-push endpoint. Use
  manual client sync, or rotate again to a different pair. A second rotation
  replaces the previous pair, so clients on even older generations are
  disconnected immediately.
- `no_change` means both fields were blank or matched the current pair; no new
  generation was created.
- `unsupported_auth_type` means the server uses `auth.type: userpass`, which
  cannot rotate online. Edit `server.yaml` and restart.
- `credential generation ... must be greater than current generation` on the
  client means the manual sync form did not advance the generation. Enter the
  server's current generation (or any larger number).
- An offline client is shown as `pending` with an unknown generation because
  the state file does not persist the last credential generation. Reconnect it
  to refresh the status.
- After revoke, an old client cannot reconnect. This is expected; update its
  `auth`, `control.token`, and `credentials.generation` and restart it.

### Ports and restarts

- A deleted mapping closes its public listener immediately. The port may
  remain in `TIME_WAIT` briefly; `SO_REUSEADDR` allows a new listener to bind
  it.
- After a client disconnects, its ports stay soft-reserved for
  `ports.offline_reserve`. Another client can still request the port
  explicitly.
- The state file keeps the last-known clients and mappings. Use the client
  detail page's **Delete client** action to forget one offline client and its
  reservations. Delete the state file only as a last resort for a full reset.

## Performance testing with iperf3

Use the same server, client, and network path for every comparison, and run
the tests close together so the line conditions are comparable.

1. Start `iperf3` on the private-network target:

   ```bash
   iperf3 -s -p 5201
   ```

2. Map a public TCP port to it, for example `remote_port: 2222` with
   `local_addr: "192.168.1.10:5201"`.

3. From a host on the server side, measure the reverse mapping:

   ```bash
   iperf3 -c <server-public-ip> -p 2222 -t 30
   iperf3 -c <server-public-ip> -p 2222 -t 30 -P 4
   ```

4. For UDP, map a UDP port to the same target and measure:

   ```bash
   iperf3 -c <server-public-ip> -p 51820 -u -b 100M -t 60
   iperf3 -c <server-public-ip> -p 51820 -u -b 100M -t 60 -l 1400
   ```

   Record the reported loss, jitter, and out-of-order counts. Compare 1200,
   1400, and 4000-byte payloads to find the path's usable MTU.

5. Compare against the native HY2 proxy path. Run `iperf3 -s` on the server
   host, enable the client's SOCKS5 listener (`proxy.socks5:
   "127.0.0.1:1080"`), and use a SOCKS-aware wrapper from the client LAN:

   ```bash
   proxychains4 -q iperf3 -c 127.0.0.1 -p 5201 -t 30
   ```

   Alternatively, use an HTTP CONNECT-aware client through `proxy.http`.

6. If you have an FRP deployment on the same path, run the identical iperf3
   commands through it and record the numbers.

Acceptance is comparative, not a fixed number: TCP reverse-mapping throughput
should be close to the native HY2 proxy on the same path, and UDP at 100 Mbps
should not show sustained loss or obvious jitter. Record the client, server,
kernel, HY2 version, MTU, and time of day with the results.

### Tuning checklist

- Start with the example QUIC windows (`initial_stream_receive_window` and
  `max_stream_receive_window` around 8 MB, `max_connection_receive_window`
  around 20 MB). Increase them only when the path has a high bandwidth-delay
  product and both hosts have memory to spare; oversized windows increase
  bufferbloat and memory use.
- `quic.max_incoming_streams` bounds concurrent TCP mappings and control
  streams. Keep the default unless you have measured a need; each stream
  consumes memory.
- `bandwidth.up` and `bandwidth.down` enforce server-side limits. Leave them
  empty for line-rate testing; set them deliberately for production fairness.
- `bandwidth.ignore_client` controls whether the client's advertised bandwidth
  is trusted. Test both settings when comparing throughput.
- UDP reverse mappings carry datagrams up to roughly 4 KB; larger payloads are
  dropped. Lower the tunnel MTU (for example WireGuard to 1280) when large
  packets stall.
- Run `make bench` before and after data-plane changes. The control-frame and
  port-policy benchmarks are local regression checks; use iperf3 for
  end-to-end throughput.

## Manual acceptance checklist (OpenWrt and Windows)

The Go tests run on Linux only. OpenWrt and Windows real-hardware execution is
external acceptance and is **not** performed by CI or claimed by the test
suite. Run this checklist on real devices before declaring a platform
supported, and record the device, firmware/OS build, architecture, binary
version, and date with the result.

### OpenWrt

1. Install the archive matching `uname -m` (`mipsle`/`mips` are softfloat;
   `arm` is GOARM=7). Confirm `hy2tun` starts without extra shared libraries.
2. Copy `examples/client.yaml` to `/etc/hy2tun/client.yaml`, set the server,
   credentials, and at least one TCP and one UDP mapping, then `chmod 600`.
3. Install `scripts/openwrt/hy2tun.init`, enable it, and start the service.
   Confirm `logread` shows a completed control handshake and no crash loop.
4. Log in to the client console from the LAN, add a mapping, and verify it
   appears in the server console.
5. From outside the LAN, connect through the mapped TCP port and send a UDP
   datagram through the mapped UDP port; verify both replies.
6. Restart the service and reboot the router. Confirm the client reconnects,
   mappings return, and the config file (including server-assigned ports)
   survives.
7. Delete and disable a mapping from the console. Confirm the public listener
   closes and active connections drop.

### Windows

1. Extract the `windows/amd64` archive and run `hy2tun.exe -c client.yaml` from
   PowerShell. Confirm the firewall prompt appears and allow private networks
   only.
2. Configure a TCP and a UDP mapping, then verify both public ports from
   outside the client network.
3. Log in to the client console at `http://<lan-ip>:7501/`, add/update/delete a
   mapping, and confirm the YAML file changes.
4. Register the client with Task Scheduler (see
   [`scripts/windows/README.md`](scripts/windows/README.md)), reboot Windows,
   and confirm it reconnects without an interactive login.
5. Stop and restart the task. Confirm mappings return and the config file is
   unchanged except for expected assigned ports/revisions.
6. Confirm passwords, control tokens, UDP tokens, and session keys do not
   appear in the console log file or Windows event output.

## Development

Run benchmarks and fuzz targets:

```bash
make bench
go test ./internal/control -run '^$' -fuzz=FuzzReadMessage -fuzztime=30s
go test ./internal/ports -run '^$' -fuzz=FuzzParseRanges -fuzztime=30s
go test ./internal/config -run '^$' -fuzz=FuzzParseListen -fuzztime=30s
```

Benchmarks cover control-frame encode/decode and port policy parsing/allocation;
they are intended for local regression checks, not as a cross-machine SLA.

```bash
go test ./...                 # unit + integration + end-to-end tests
go test -race ./...           # race detector
go test ./internal/e2e -v     # only the acceptance test
```

The end-to-end test starts a real server and client with a self-signed
certificate, verifies reverse TCP and UDP, connects a standard HY2 core client,
then logs in to the server console to push and delete a mapping, rotate both
credentials with auto-push, confirm the client persists the new pair and
reconnects, and revoke the previous pair. It also confirms that a new client
using the revoked credentials is rejected. It uses only loopback addresses and
ephemeral ports.

The repository intentionally does not use a Node/CDN build. Web templates,
CSS, and JavaScript are embedded with `go:embed`.

## License

MIT. See [`LICENSE`](LICENSE). Security reporting is described in
[`SECURITY.md`](SECURITY.md).
