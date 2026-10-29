# Changelog

All notable changes to this project are documented here.

## Unreleased

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
