# Changelog

[English](CHANGELOG.md) | [简体中文](CHANGELOG.zh-CN.md)

All notable changes to this project are documented here.

## v1.5.1 - 2026-10-30

### Added

- A committed LuCI controller integration harness with fake CLI/init scripts;
  it covers route/CSRF registration, save/save-start, service actions, logs,
  and config read failures without root or a router.

### Fixed

- Web console passwords over bcrypt's 72-byte limit now return a validation
  error instead of an internal error.
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
