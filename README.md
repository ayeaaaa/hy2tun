# hy2tun

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
- Server and client web consoles with login, CSRF protection, live status,
  mapping management, and logs.
- Optional local SOCKS5 and HTTP CONNECT proxies on the client, using the same
  HY2 connection.
- Linux, OpenWrt, and Windows client builds from one source tree.

## What it does not do (v1)

- No multi-user accounts, approval workflow, tenant isolation, quotas, or
  billing. There is one HY2 credential set and one control token per server.
- No P2P, Hysteria Realms, or UDP hole punching. The client always connects to
  the server.
- No TUN mode, transparent proxy, domain-based routing, ACLs, load balancing,
  or CDN fronting.
- One client connects to one server. One client cannot register with several
  servers at once.
- The server console does not edit the full server YAML; change the file and
  restart `hy2tund`.
- The client console does not edit the server address, TLS, obfs, or control
  token; those stay in `client.yaml`.
- Offline clients are read-only in the server console.
- No automatic update, tray application, or native Windows service. Windows
  uses Task Scheduler.
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

- Go 1.26 or newer to build. The `go.mod` file pins the HY2 core at v2.13.0.
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

- `hy2tund-<os>-<arch>` and `hy2tun-<os>-<arch>` (with `.exe` on Windows);
- `examples/server.yaml` and `examples/client.yaml`;
- `README.md`;
- systemd and OpenWrt files on Linux, or `scripts/windows/README.md` on
  Windows.

`dist/SHA256SUMS` contains the SHA-256 of every archive. Set `VERSION`,
`COMMIT`, or `DATE` to override the values embedded in the binaries:

```bash
VERSION=1.0.0 make release
```

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
sudo ./bin/hy2tund -c /etc/hy2tun/server.yaml
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
sudo ./bin/hy2tun -c /etc/hy2tun/client.yaml
```

Open `http://<client-lan-ip>:7501/` and `http://127.0.0.1:7500/` on the server
(or use an SSH tunnel, see below). The first console login uses the generated
password printed at startup.

### 4. Install as a service

**systemd (Linux server and client):**

```bash
sudo install -m 0755 bin/hy2tund bin/hy2tun /usr/local/bin/
sudo install -d -m 0755 /etc/hy2tun /var/lib/hy2tun
sudo install -m 0644 scripts/systemd/hy2tund.service /etc/systemd/system/
sudo install -m 0644 scripts/systemd/hy2tun.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now hy2tund    # on the server
sudo systemctl enable --now hy2tun     # on the client
sudo journalctl -u hy2tund -f
```

**OpenWrt (procd):**

```bash
opkg update && opkg install ca-bundle
cp hy2tun /usr/bin/hy2tun
chmod +x /usr/bin/hy2tun
install -d -m 0755 /etc/hy2tun
cp client.yaml /etc/hy2tun/client.yaml
chmod 600 /etc/hy2tun/client.yaml
cp hy2tun.init /etc/init.d/hy2tun
chmod +x /etc/init.d/hy2tun
/etc/init.d/hy2tun enable
/etc/init.d/hy2tun start
```

For MIPS routers use the `linux/mipsle` or `linux/mips` build; for ARM
routers use `linux/arm` or `linux/arm64`. The MIPS builds already use
`GOMIPS=softfloat`. Check `uname -m` on the router if you are unsure. If flash
space is tight, deploy only the client binary, or use extroot.

**Windows:** see [`scripts/windows/README.md`](scripts/windows/README.md) for
Task Scheduler instructions and firewall notes.

## Configuration reference

### Server (`server.yaml`)

| Field | Meaning |
| --- | --- |
| `listen` | HY2 UDP listen address, for example `:443`. |
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
| `ports.allow` | Ports or ranges clients may request. Empty means all free ports. |
| `ports.deny` | Ports or ranges that are always rejected. Deny wins over allow. |
| `ports.auto_range` | Range used for `remote_port: 0` when `allow` is empty. Default `20000-29999`. |
| `ports.offline_reserve` | How long an offline client keeps a soft reservation. Default `24h`. |
| `web.listen` | Management address. The example uses `127.0.0.1:7500`. |
| `web.username` | Console username. Default `admin` when `web.listen` is set. |
| `web.password_hash` | bcrypt hash. Empty generates a password on first start. |
| `web.tls.cert`, `web.tls.key` | Optional HTTPS certificate pair for the console. |
| `state_file` | Persistent state JSON. Empty keeps state in memory. |
| `log.level` | `debug`, `info`, `warn`, or `error`. |
| `log.file` | Optional log file. Empty logs to stderr/journal. |

### Client (`client.yaml`)

| Field | Meaning |
| --- | --- |
| `server` | `host:port` of the server's HY2 listener. |
| `auth` | HY2 password, or `user:password` for userpass auth. |
| `tls.sni` | TLS server name. Empty uses the host part of `server`. |
| `tls.insecure` | Skip certificate verification. Do not use on untrusted networks. |
| `tls.pin_sha256` | SHA-256 of the server certificate's DER bytes. |
| `tls.ca` | CA certificate bundle for a private/self-signed server certificate. |
| `obfs.type` | Must match the server. Empty or `salamander`. |
| `bandwidth.up`, `bandwidth.down` | Optional client-side HY2 limits. |
| `control.token` | Must match `control.token` on the server. |
| `client.id` | Stable client ID. Empty generates one on first start. |
| `client.name` | Display name in the server console. |
| `client.revision` | Configuration revision. The client updates it automatically. |
| `mappings` | List of reverse mappings (see below). |
| `web.listen` | Client console address. The example uses `0.0.0.0:7501`. |
| `web.username`, `web.password_hash` | Console login. Empty hash generates a password. |
| `web.tls.cert`, `web.tls.key` | Optional HTTPS for the client console. |
| `proxy.socks5` | Optional local SOCKS5 listen address, for example `127.0.0.1:1080`. |
| `proxy.socks5_auth` | Optional `user:password` for SOCKS5. |
| `proxy.http` | Optional local HTTP CONNECT listen address. |
| `proxy.http_auth` | Optional `user:password` for HTTP CONNECT. |
| `log.level`, `log.file` | Logging configuration. |

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
           proxy_set_header X-Real-IP $remote_addr;
           proxy_buffering off;
           proxy_read_timeout 1h;
       }
   }
   ```

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
  `HttpOnly`, and `SameSite=Lax`; mutating APIs require
  `X-Requested-With: hy2tun` and a matching `Origin`.
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
  or restart the process (the limiter is in memory).
- If the console is reachable but pages redirect to `/login`, the session
  cookie is missing or expired. Log in again; restarting the process
  invalidates sessions but not the persisted signing key.
- If `web_secret` exists but is unreadable or corrupt, the process cannot
  start. Delete it and restart to generate a new key; this invalidates all
  existing web sessions. Check that the configuration directory is writable
  by the process user.

### Ports and restarts

- A deleted mapping closes its public listener immediately. The port may
  remain in `TIME_WAIT` briefly; `SO_REUSEADDR` allows a new listener to bind
  it.
- After a client disconnects, its ports stay soft-reserved for
  `ports.offline_reserve`. Another client can still request the port
  explicitly.
- The state file keeps the last-known clients and mappings. Delete it only
  when you intentionally want to forget offline clients and reservations.

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

## Development

```bash
go test ./...                 # unit + integration + end-to-end tests
go test -race ./...           # race detector
go test ./internal/e2e -v     # only the acceptance test
```

The end-to-end test starts a real server and client with a self-signed
certificate, verifies reverse TCP and UDP, connects a standard HY2 core client,
then logs in to the server console to push and delete a mapping and confirm the
public port closes. It uses only loopback addresses and ephemeral ports.

The repository intentionally does not use a Node/CDN build. Web templates,
CSS, and JavaScript are embedded with `go:embed`.
