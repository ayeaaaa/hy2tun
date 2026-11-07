# Changelog

[English](CHANGELOG.md) | [简体中文](CHANGELOG.zh-CN.md)

All notable changes to this project are documented here.

## Unreleased

### Added

- Per-mapping server-side exposure control: `public` (default) or `local`
  (`127.0.0.1` and `::1` only). Exposure is stored in the server state file,
  survives restart and client reconnect, and is cleared when the mapping is
  deleted; recreating the same ID starts `public`.
- `PUT /api/clients/{clientID}/mappings/{mappingID}/exposure` and the
  `hy2tund exposure --client <id> --mapping <id> --mode public|local` CLI.
- Server console `公网` / `仅本机` controls, and a read-only client console
  badge with `IPv4`/`IPv6` unavailable-family warnings.
- `Exposure` and `UnavailableFamilies` in mapping read models, plus an opt-in
  `exposure_v1` control capability and `exposure_update` message. Old clients
  never receive the new message; old servers omit the fields and new clients
  show `—`.

### Changed

- Mapping listeners now bind IPv4 and IPv6 independently. One unavailable
  family keeps the other serving and is reported; both failing is an error.
- Exposure persistence failures now return `409 exposure_conflict` with the
  retained `active_exposure`, `unavailable_families`, and `closed` state
  instead of `500`.
- A live exposure switch closes existing TCP connections, replaces the public
  UDP sockets, and rebuilds the listeners. A failed switch rolls back to the
  previous active exposure and reports it; if rollback also fails the mapping
  stays closed.
- Native HY2 reverse UDP sessions are paused and reattached to the new public
  sockets during a listener switch, so datagrams may drop briefly. In `tcp`
  mode, or while `hy2+tcp` is using TCP, UDP-over-TCP sessions may be closed
  and re-established, so a brief client reconnect/resync is possible.
- Current-password re-entry was removed from transport mode, failover timing,
  client name, trust-proxy, credential rotation, revoke, and reveal. Only
  changing the Web administrator username/password still requires it. Session
  authentication and CSRF remain mandatory.
- Security note: removing re-authentication means a stolen authenticated Web
  session can perform every non-Web-admin mutation, including credential
  rotation/reveal and enabling `web.trust_proxy`. Only Web administrator
  username/password changes still require the current password, and failed
  Web-admin confirmations remain rate-limited. Because `trust_proxy` is itself
  a session-authenticated mutation, keep it off unless the console is behind a
  trusted proxy that overwrites both forwarding headers.

## v1.6.4 - 2026-11-06

### Changed

- Client and server traffic statistics now use **客户端 → 服务端** and
  **服务端 → 客户端** instead of the ambiguous 上行/下行 labels. Mapping tables
  keep their existing columns and show the two directions on separate lines.
- Server metrics help text now describes the actual relay direction; metric
  names and values are unchanged.
- The `tx_bytes` / `rx_bytes` JSON fields keep their names and values:
  `tx_bytes` is client-to-server payload, `rx_bytes` is server-to-client
  payload.
- Updated `github.com/andybalholm/brotli` to v1.2.7 and
  `github.com/klauspost/compress` to v1.20.2 for GO-2026-6888 and
  GO-2026-6865. GO-2026-5288 remains allowlisted because hy2tun does not
  enable Hysteria sniffing and upstream has no fixed release.

## v1.6.3 - 2026-11-05

### Added

- The client Web console now has a per-server **Failover timing** form for
  `connect_timeout`, `heartbeat_interval`, `heartbeat_timeout`,
  `probe_interval`, `min_tcp_hold`, and `recovery_successes`. Saving requires
  CSRF and the current Web password, writes the YAML atomically, and hot-applies
  the new policy without restarting the client.
- APIs that do not implement the optional failover mutation report `501` and
  the form stays hidden.

### Changed

- Documentation and examples now describe the Web console path and the YAML
  path. LuCI's basic form still keeps `failover` as an advanced field.

## v1.6.2 - 2026-11-05

### Added

- Per-server `failover` tuning for `hy2+tcp`: `connect_timeout`,
  `heartbeat_interval`, `heartbeat_timeout`, `probe_interval`,
  `min_tcp_hold`, and `recovery_successes`. Single-server clients set it at
  the top level; multi-server clients set it independently under each
  `servers[]` entry. Omitted fields keep the conservative defaults.
- Documented the faster middle profile: `8s` connect, `5s` heartbeat, `20s`
  dead timeout, `15s` probe, `30s` TCP hold, and 3 recovery successes.

### Changed

- The basic config editor recognizes `failover` as a known advanced key and
  preserves it without exposing it in the basic form.
- Examples and README document the per-server tuning and the middle profile.

## v1.6.1 - 2026-11-05

### Fixed

- Forced reconnect now clears the control owner synchronously before closing
  the stream. A Push already queued by the old reader can no longer be applied
  after invalidation and change the client revision or mappings.

### Changed

- Documentation now clarifies that the client console has a recent-log view,
  while server logs go to systemd journal or the configured log file.
- Added a forum-ready Chinese feature overview at
  `docs/feature-overview.zh-CN.md`.
- Release notes are generated outside the build tree, so future release
  binaries no longer report `vcs.modified=true` / `+dirty` metadata.
- Hardened E2E polling and flood cleanup (test-only).

## v1.6.0 - 2026-11-05

### Added

- TLS 1.3 TCP transport for the hy2tun control stream, reverse TCP streams,
  reverse UDP mappings, and forward proxy streams. It reuses the existing HY2
  password/control token and the existing `tls.ca`, `tls.sni`, `tls.insecure`,
  and `tls.pin_sha256` policy. TLS 1.3 is required, and a certificate pin is
  still checked when insecure verification is enabled.
- Three transport modes: `hy2` (the default), `tcp`, and `hy2+tcp`. The mode is
  global on the server and independent per server entry on a multi-server
  client. Pure modes are mutually exclusive: `hy2` starts no TCP listener or
  fallback, while `tcp` starts no UDP HY2 listener and rejects ranges, port
  lists, and UDP hop intervals.
- Same-port dual listeners in `hy2+tcp`: the UDP HY2 listener and the TCP
  listener use the same numeric port but are separate sockets. The TCP endpoint
  is derived from the first port item, preserving input order when ranges or
  port hopping are used. The firewall must allow both TCP and UDP on that port.
- Automatic fallback and recovery in `hy2+tcp`: a 10-second connect/handshake
  budget, a 10-second heartbeat, a 30-second dead-session timeout, probes every
  30 seconds with ±20% jitter, and failback only after three consecutive
  successful probes and at least 60 seconds on TCP.
- UDP over TCP framing for UDP mappings while TCP is active, with a 4096-byte
  payload limit and a bounded 32-packet per-mapping send queue. Reliable
  ordering adds latency and head-of-line cost, and datagrams may be dropped
  under queue pressure.
- Fallback eligibility includes a local firewall or sandbox rejecting UDP I/O
  with `EPERM`/`EACCES`; filesystem permission errors and authentication, TLS,
  certificate-pin, and protocol failures remain non-fallback.
- Server and client Web mode controls, per-server controls for multi-server
  clients, a LuCI mode field, and `hy2tun config get|set` mode support.
- Diagnostics for the configured mode, actual transport, switch reason and
  count, last switch, last probe, probe error, server-side last actual
  transport, and the client metrics `hy2tun_client_transport_active` and
  `hy2tun_client_transport_switches_total`.

### Changed

- `luci-app-hy2tun` now depends on `hy2tun (>= 1.6.0)`.
- Configurations without `mode` continue to default to `hy2`; loading or
  upgrading does not require adding the field. The default `server.yaml`,
  `client.yaml`, and `client-multi.yaml` examples still omit `mode`; the LuCI
  package's basic example keeps an explicit mode field.
- A mode switch that changes the active transport closes the current session:
  existing TCP and proxy connections drop, UDP mapping state is closed, and
  queued datagrams may be lost. There is no seamless migration, exactly-once
  delivery, or replay.
- Only recognized network failures trigger fallback. Authentication, TLS,
  certificate-pin, protocol, configuration, queue-full, and cancellation errors
  are surfaced and never cause a downgrade to TCP.

### Compatibility

- Existing `hy2` deployments and standard Hysteria 2 clients are unchanged.
  `hy2+tcp` keeps HY2 available; a pure `tcp` server does not accept standard
  Hysteria 2 clients.
- An old client without mode support uses HY2 and cannot fall back. A new
  `hy2+tcp` client against an old HY2-only server stays on HY2; if UDP is
  blocked, fallback is unavailable until the server is upgraded and TCP is
  opened.
- The TCP transport is hy2tun-specific. It is not FRP wire-compatible, and this
  release does not add P2P, Hysteria Realms, or UDP hole punching.

### Verification

- Local gates: `go test ./... -count=1`, `go test -race ./... -count=1`,
  `go vet ./...`, `staticcheck ./...`, `make build`,
  `scripts/openwrt/test-ipk.sh`, and
  `scripts/openwrt/test-luci-controller.sh`.
- Cross-builds for Linux amd64/arm64/arm/mips/mipsle and Windows amd64.
- Local QEMU user-mode checks for Linux arm64/arm/mips/mipsle: `-version`,
  `--check-config`, and a real TLS/TCP frame round-trip test.
- OpenWrt 24.10.2 x86_64 real-device acceptance with the v1.6.0 package:
  HY2 active, both-direction UDP DROP fallback in 9 seconds, recovery to HY2
  after 99 seconds, TCP and UDP mappings over the TCP fallback, wrong-auth
  rejection without fallback, TCP port-conflict rollback, LuCI page/mode and
  start-stop checks, and rollback to v1.5.4 with no leftover mappings.
  Windows hardware acceptance remains external; only the Windows amd64
  cross-build is verified.

## v1.5.4 - 2026-10-31

### Added

- A GitHub project link in the client and server web consoles and the OpenWrt
  LuCI page.

## v1.5.3 - 2026-10-30

### Added

- Optional `web.metrics_token` Bearer authentication for `/metrics`; empty
  keeps the historical unauthenticated endpoint for loopback-only setups.
- `staticcheck` in CI.
- OpenWrt package post-install now verifies that the installed binary actually
  runs on the target CPU.

### Changed

- systemd units use stricter filesystem, device, kernel, and address-family
  sandboxing while keeping `/etc/hy2tun` and `/var/lib/hy2tun` writable.

## v1.5.2 - 2026-10-30

### Changed

- Docker examples now use a writable config directory and a writable state
  directory; the previous read-only YAML mounts broke generated passwords,
  client IDs, assigned ports, and hot settings.

### Fixed

- Configuration files with group/other permissions are tightened to 0600 when
  loaded.
- Server resource limits now bound control clients, mappings per client,
  concurrent public TCP connections per mapping, and pending data-stream
  tokens.
- Reverse UDP peers use a 4097-byte read buffer instead of 64 KiB, avoiding
  up to 64 MiB of buffers per mapping at the peer limit.
- Firewall command execution has a 10-second timeout, so a hung nft/iptables
  invocation cannot block startup indefinitely.
- Web console password changes over bcrypt's 72-byte limit return a validation
  error instead of an internal error.
- `log.file` rotates at 10 MiB with five backups instead of growing forever.
- Offline clients older than seven days are pruned from the server state file
  at startup.

## v1.5.1 - 2026-10-30

### Added

- A committed LuCI controller integration harness with fake CLI/init scripts;
  it covers route/CSRF registration, save/save-start, service actions, logs,
  and config read failures without root or a router.

### Fixed

- The LuCI basic-config save path now returns a validation error for Web
  passwords over bcrypt's 72-byte limit instead of an internal error.
- The LuCI page rejects HY2 auth shorter than 8 bytes and control tokens
  shorter than 16 bytes before sending a request.
- Service status polling is bounded to the requested five seconds.
- Controller payload temp files are created with mode 600 directly instead of
  being opened and chmodded afterward.
- `hy2tun config` init-status checks have a timeout, and `config set` rejects
  trailing data after the JSON object.

## v1.5.0 - 2026-10-30

### Added

- LuCI **Services → hy2tun** page with a basic single-server configuration
  form, save/save-and-start, start/stop/restart, enable/disable, the most
  recent 50 log lines, and a link to the client Web console.
- `hy2tun config get|set` helper for the LuCI page: SHA-256 revision checks,
  an exclusive configuration lock, atomic writes, bcrypt hashing of the Web
  password, and stable JSON errors and exit codes.
- Configuration validation before saving: invalid basic fields are rejected
  with field errors, and advanced-field failures are reported without
  overwriting the existing file.
- While the service is running, the LuCI form is read-only; saves require the
  service to be stopped, and a stale revision is rejected as a conflict.
- Safe server migration from the basic form: changing the server requires new
  HY2 authentication and control token and clears the stored client ID and
  revision.
- Advanced single-server fields are preserved when the basic form is saved.
  Multi-server configurations and unknown top-level fields remain read-only or
  protected in LuCI; rotated credentials are managed from the client console.

### Changed

- The OpenWrt package no longer creates `/etc/hy2tun/client.yaml` or enables the
  service during installation. The shipped `client.yaml.example` contains only
  the basic single-server fields, and first-run configuration happens in LuCI
  (install → fill the form → save → start → open the console).
- `luci-app-hy2tun` now depends on `hy2tun (>= 1.5.0)`.

## v1.4.4 - 2026-10-29

### Fixed

- The LuCI status page now loads instead of returning `502 Bad Gateway`:
  the controller loads `nixio.fs` before use, and the service-status command
  no longer writes `inactive` into the CGI response. The Services menu entry
  is available after the LuCI menu cache refresh.

## v1.4.3 - 2026-10-29

### Fixed

- OpenWrt `.ipk` files now use the gzip-tar container format expected by
  `opkg`; the previous ar-based packages were rejected as malformed. Verified
  with `opkg install` on ImmortalWrt 24.10.2 x86_64, including the LuCI
  package and a live reverse TCP mapping.

## v1.4.2 - 2026-10-29

### Added

- Bilingual GitHub Release notes generated from `CHANGELOG.md` and
  `CHANGELOG.zh-CN.md`; the release workflow fails if either language is
  missing.

## v1.4.1 - 2026-10-29

### Added

- OpenWrt `.ipk` packages for the client, including the procd init script and
  example configuration.
- `luci-app-hy2tun` package with a Services status page and client Web-console
  link.
- Release and CI packaging checks for OpenWrt packages.

## v1.4.0 - 2026-10-29

### Added

- Multi-server client configuration: one process connects to independent
  servers with separate client IDs, mappings, credentials, reconnect loops,
  port hopping, and proxies.
- Per-server SOCKS5/HTTP proxy listeners, default disabled, editable from the
  client web console with port-conflict validation.
- Proxy listeners cancel in-flight dials during close or address changes, and
  configuration writes distinguish a committed rename from a pre-commit save
  failure.
- Client server list and server-scoped web APIs; legacy single-server APIs
  remain mapped to `default`.

### Changed

- Legacy single-server YAML remains supported without automatic format
  conversion.
- Multi-server configs use top-level `web` and `log` only; legacy per-server
  fields are rejected when `servers` is present.

## v1.3.3 - 2026-10-29

### Security

- Restore current-password confirmation for trust-proxy changes after review
  found that passwordless changes could bypass IP-keyed rate limiting. Port
  policy changes remain passwordless.
- Serve embedded static assets with `Cache-Control: no-cache` so an upgraded
  browser cannot run stale JavaScript against new settings forms.
- Make offline-client deletion transactional: pre-rename save failures restore
  the in-memory client, while a committed rename whose directory sync fails is
  treated as committed and logged instead of being rolled back in memory.

## v1.3.2 - 2026-10-28

### Changed

- Server settings no longer require the current web password for routine port
  policy and trust-proxy changes. Web administrator and HY2 credential
  operations still require confirmation.

## v1.3.1 - 2026-10-28

### Added

- Server web console can delete an offline client, removing its persisted
  mappings and soft port reservations. Online clients must disconnect first.

## v1.3.0 - 2026-10-28

### Added

- `--check-config` for both binaries, including TLS, web TLS, bcrypt, and
  runtime directory validation.
- `/healthz` and Prometheus `/metrics` endpoints on both web consoles.
- Automatic Linux server-side nftables/iptables redirect when `listen` is a
  UDP port range; rules are removed on shutdown.
- Dockerfile targets, Docker Compose example, and release install script.
- CI workflow, Dependabot, security policy, issue/PR templates, contribution
  guidance, and automatic tagged-release publishing.
- Benchmarks for control frames and port policy, plus fuzz targets for frame,
  range, and listen parsing.

### Changed

- Server `listen` now accepts `host:start-end` (and comma-separated ranges);
  the first port is the actual bind port and all range ports are reserved.
- Updated `golang.org/x/crypto`, `golang.org/x/net`, `golang.org/x/sys`, and
  `golang.org/x/text` to address known module vulnerabilities.
- The client port-hopping warning now mentions range listening as well as
  redirects.
- Split large server mapping and web handler files into focused files; behavior
  is unchanged.

## v1.2.0 - 2026-10-28

### Added

- Client-side Hysteria 2 UDP port hopping.
- `server` accepts single ports, comma-separated ports, ranges, and mixed lists.
- `transport.udp.hop_interval`, `min_hop_interval`, and `max_hop_interval`.
- Salamander obfs composition for every hop socket.
- Port-hopping deployment guide with nftables/iptables examples.
- Simplified Chinese README (`README.zh-CN.md`).

### Changed

- Single-port configurations keep their existing YAML shape and behavior.
- One-unique-port sets such as `443,443` normalize to a single port.
- Release archives include both READMEs.

## v1.1.0 - 2026-10-28

### Added

- Hot web configuration editing for web administrators, HY2 credentials,
  port policy, trust-proxy, and client name.
- Generation-based credential rotation with a retained previous pair.
- Auto-push to capable online clients and manual client sync.
- Explicit previous-credential revoke.
- Authenticated secret reveal with masked values and `Cache-Control: no-store`.

## v1.0.0 - 2026-10-27

### Added

- HY2 proxy server and reverse tunnel client.
- Reverse TCP mappings over HY2 streams.
- Reverse UDP mappings over native HY2 datagrams.
- Server and client web consoles.
- Mapping management, live status, logs, and credential handling.
- Linux, OpenWrt, and Windows client builds.
