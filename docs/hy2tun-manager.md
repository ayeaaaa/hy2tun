# hy2tun-manager operator guide

[English](hy2tun-manager.md) | [简体中文](hy2tun-manager.zh-CN.md)

`hy2tun-manager` is an optional, separate control plane for a fleet of existing
`hy2tund` servers. It aggregates server status and lets one administrator manage
clients, mappings, server settings, and credentials through one Web UI. It does
not replace the `hy2tund`/`hy2tun` data plane: every server remains authoritative
for its own clients and mappings, and client processes never connect to the
manager.

The manager keeps only:

- the server inventory (name, base URL, upstream admin username, TLS setting),
- each upstream admin password encrypted with AES-256-GCM,
- its own single administrator bcrypt hash,
- an append-only audit log without secret values.

This archive contains:

- `hy2tun-manager` — the static manager binary,
- `manager.example.yaml` — an example config,
- `hy2tun-manager.service` — an example systemd unit,
- `README.md` — this guide (English).
- `README.zh-CN.md` — the Simplified Chinese guide.

## Requirements

- A Linux amd64 or arm64 host (only these manager build targets are released).
- systemd is recommended; any supervisor that can restart a foreground process
  works too.
- HTTPS reachability from the manager host to each server's operator-provided
  base URL.
- An HTTPS reverse proxy, SSH tunnel, or VPN in front of the manager. The
  manager speaks plain HTTP on loopback by default and does not terminate TLS
  itself.
- The existing server Web consoles must already be reachable and have a known
  administrator username/password.

## Client-centric view

The primary view is **Clients**. After login, the manager reads the client
list from every configured server and groups records by their stable upstream
`client.id`:

- the same `client.id` on server A and server B is shown as one client with two
  server connections;
- the same display name with different IDs stays as two rows, so different
  machines are never silently merged;
- the same ID with different names is marked as a conflict;
- a record without a usable ID is kept visible as an ungrouped client.

The client detail page lists every connected server with its management URL,
online state, version, mapping count, and a link to that server's mapping
editor. The server inventory remains available under **Servers** and is still
the source of truth for upstream API access. Read the server/client data plane
unchanged; this is a manager-side aggregation rule.

## Install from the archive

```sh
tar -xzf hy2tun-manager_<version>_linux_amd64.tar.gz
sudo useradd --system --user-group --home-dir /var/lib/hy2tun-manager \
  --shell /usr/sbin/nologin hy2tun-manager
sudo install -m 0755 hy2tun-manager /usr/local/bin/hy2tun-manager
sudo install -d -m 0755 /etc/hy2tun
sudo install -m 0644 manager.example.yaml /etc/hy2tun/manager.yaml
sudo install -m 0644 hy2tun-manager.service /etc/systemd/system/hy2tun-manager.service
sudo systemctl daemon-reload
```

Review `/etc/hy2tun/manager.yaml` before the first start, then enable the
service:

```sh
sudo systemctl enable --now hy2tun-manager
```

The unit creates `/var/lib/hy2tun-manager` through `StateDirectory=` and runs the
manager as the `hy2tun-manager` user. The state, key, and audit files are created
with mode `0600` in that directory.

Before starting, review `/etc/hy2tun/manager.yaml`:

```yaml
listen: "127.0.0.1:7600"
base_path: "/"
state_file: "/var/lib/hy2tun-manager/state.json"
key_file: "/var/lib/hy2tun-manager/manager.key"
audit_file: "/var/lib/hy2tun-manager/audit.jsonl"
refresh_interval: "30s"
trust_proxy: false
```

- Keep `listen` on loopback (`127.0.0.1:...`) unless the host is reachable only
  from a trusted network. The manager has no built-in TLS listener.
- `base_path` may be a prefix such as `/hy2tun-manager/` when sharing a
  hostname. The manager normalizes it to leading/trailing slashes.
- `refresh_interval` is the fleet-wide background poll interval; the minimum is
  `1s` and the default is `30s`.
- Set `trust_proxy: true` only when a trusted reverse proxy overwrites both
  `X-Forwarded-For` and `X-Real-IP` with the direct client address. Otherwise
  login rate limiting would trust attacker-controlled headers or share one
  bucket for every user.

Check the service:

```sh
sudo systemctl status hy2tun-manager
sudo journalctl -u hy2tun-manager -n 50
```

The first login is `admin` / `admin`. On the first start only, the manager
stores the bcrypt hash of this fixed bootstrap password in `state.json`. Change
it immediately after the first login through the reset flow below; do not leave
the manager reachable with the default password.

To use a different bootstrap password instead, set
`HY2TUN_MANAGER_INITIAL_PASSWORD` in the service environment for the first
start only (for example through `systemctl edit hy2tun-manager` and an
`Environment=` line, or an `EnvironmentFile=`). The variable is ignored when
`state.json` already exists, so leaving it configured can never reset an
existing administrator.

Open the manager at `http://127.0.0.1:7600/` through an SSH tunnel for the first
login, or through the HTTPS reverse proxy below. Log in as `admin`.

## Docker

The repository Dockerfile has a `manager` target; the Compose file exposes it
as the `hy2tun-manager` service. The image runs as nonroot UID/GID `65532` and
uses host networking, matching the default loopback listener used by the native
install.

```sh
docker compose build hy2tun-manager
mkdir -p manager-config manager-state
cp examples/manager.yaml manager-config/manager.yaml
sudo chown -R 65532:65532 manager-config manager-state
docker compose up -d hy2tun-manager
```

Configuration lives in `manager-config/manager.yaml`; state, key, audit, and
the single-instance lock live in `manager-state/`. Back up the state and key
together. The manager remains reachable at the host's loopback `127.0.0.1:7600`
and should be published through the host's HTTPS reverse proxy or VPN.

The container must run on a Linux host because the Compose service uses host
networking and the manager is a Linux-only release. Do not run multiple manager
containers against the same state directory; the process lock is per container.

## Change or reset the administrator password

While logged in, open **修改密码** in the top bar. Enter the current password,
a new password of at least eight characters (at most 72 bytes), and confirm it.
On success the manager updates its live login hash, invalidates every existing
manager session, and redirects you to the login page. Log in again with the new
password.

If you are locked out, use the offline reset path. The manager snapshots the
administrator hash when its HTTP shell starts, so an offline reset is a file
change plus a service restart. The running service holds an exclusive state
lock; `reset-admin` takes the same lock and **fails without changing anything
while the service is running**. The required order is exactly:

```sh
sudo systemctl stop hy2tun-manager
sudo -u hy2tun-manager /usr/local/bin/hy2tun-manager \
  -config /etc/hy2tun/manager.yaml -reset-admin
sudo systemctl start hy2tun-manager
```

Do not run `reset-admin` against a live manager. If it reports that the state
is locked, stop the service and retry. The command refuses before prompting, so
it cannot consume a password while the lock is unavailable.

The equivalent subcommand form is also supported:

```sh
sudo -u hy2tun-manager /usr/local/bin/hy2tun-manager reset-admin \
  -config /etc/hy2tun/manager.yaml
```

The command prompts twice without echoing, requires a matching password of at
least eight characters and at most 72 bytes (the bcrypt limit), appends an
`admin.reset` audit entry, changes only `admin.password_hash` in the state file,
and exits. Run it as the service user: running it as root would replace the
0600 state file with a root-owned file that the service could no longer update.
A reset while the service is running is rejected by the state lock, so the
stop → reset → start sequence above is mandatory.

A running manager that has not been restarted keeps its old hash snapshot and
its in-memory sessions. After the restart, the new password is required and all
previously issued manager session cookies are invalid (the session signing key
is generated per process).

## Reverse proxy and TLS

Terminate HTTPS at a reverse proxy on the same host (or reach the loopback port
over a trusted tunnel). Do not forward a public port directly to the manager.
The manager has no login for an anonymous public network and relies on the
proxy for transport encryption.

Recommended: give the manager a dedicated hostname and proxy the root path:

```nginx
server {
    listen 443 ssl http2;
    server_name manager.example.net;

    ssl_certificate     /etc/letsencrypt/live/manager.example.net/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/manager.example.net/privkey.pem;

    location / {
        proxy_pass http://127.0.0.1:7600;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-Proto $scheme;
        # Only when trust_proxy: true, and only if these overwrite rather than
        # append client-supplied values:
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $remote_addr;
    }
}
```

The manager sends HSTS and marks its session cookie `Secure` when it sees
`X-Forwarded-Proto: https`, so let the proxy pass that header through.

Sharing a hostname with a `hy2tund` console is supported only with a distinct
`base_path`. The manager uses its own cookie name
(`hy2tun_manager_session`), so it does not evict a server console session. When
using a prefix, configure the same prefix in the manager and preserve it in the
proxy; the manager strips the prefix itself:

```yaml
base_path: "/hy2tun-manager/"
```

```nginx
location /hy2tun-manager/ {
    proxy_pass http://127.0.0.1:7600;   # no URI part: keep the prefix
    proxy_set_header Host $host;
    proxy_set_header X-Forwarded-Proto $scheme;
}
```

`/hy2tun-manager` without the trailing slash redirects to the prefix.

## Add upstream servers

Use **Servers → Add server** with:

- a unique display name,
- the server's base URL, including any reverse-proxy path prefix, for example
  `https://us.example.net/hy2tund`,
- the server Web administrator username,
- the server Web administrator password (write-only),
- `tls_skip_verify` only for a deliberately self-signed test endpoint.

"Test connection" logs in and reads the overview without saving anything. Add
saves the credentials encrypted and refreshes the inventory. The manager
supports the existing server API only; `v1.7.0+` is the minimum for mapping
exposure and `v1.7.4` is the recommended baseline for full coverage. Older
servers show unsupported actions instead of failing the whole inventory.

## Upstream credential custody

The manager can mutate servers, so it holds each server's administrator
password. Treat the manager host as security-sensitive:

- `upstream_password_enc` in `state.json` is AES-256-GCM ciphertext. The 32-byte
  key is in `manager.key`, never in `state.json`, HTML, JSON responses, audit
  entries, or logs.
- Back up `state.json` and `manager.key` together and protect the backup equally.
  If one is lost or the pair does not match, the manager fails closed at startup
  with a recovery message; it never silently generates a new key over existing
  secrets.
- The audit log records actions and sanitized upstream errors, never passwords,
  control tokens, session cookies, or certificate private material.
- A manager compromise means the stored upstream administrator credentials are
  compromised. Use a strong, unique server administrator password, restrict
  manager host access, keep the manager on loopback behind HTTPS, and rotate
  server credentials after any suspected compromise.
- The manager never sends upstream passwords to the browser. Edit forms use
  write-only password fields and masked display.

## Version limitations

The manager deliberately does not attempt to control client-local behavior:

- **No client-local configuration editing.** The client's `servers:` list,
  local proxies, failover tuning, TLS settings, logs, and connection targets are
  client-local YAML and are not visible to a server. Configure them on the
  client itself.
- **No adding or removing client-to-server connections.** The manager can manage
  a client that is already connected to a server; it cannot make a client join
  or leave a server, and it cannot merge one physical client seen by multiple
  servers into a single identity.
- **No connection scheduling.** There is no timed connect/disconnect, failover
  scheduling, or traffic policy in the manager.
- **Offline clients are read-only at the mapping layer.** Mapping edits require
  the online client to acknowledge a revision. Stored exposure can still be
  changed because it is server-side state.
- **No process restart, binary upgrade, deployment, or firewall management.**
- **No multi-user accounts or roles; one manager administrator.**
- **No server console embedding.** The manager uses the existing JSON APIs, not
  an iframe.

## Build and release

```sh
make build-manager                     # bin/hy2tun-manager
VERSION=1.0.0 ./scripts/release-manager.sh
```

`release-manager.sh` builds only `linux/amd64` and `linux/arm64` with
`CGO_ENABLED=0`, `-trimpath`, and `version.Version`, `version.Commit`, and
`version.Date` supplied through `-ldflags`. Its two archives are named
`hy2tun-manager_<version>_linux_amd64.tar.gz` and
`hy2tun-manager_<version>_linux_arm64.tar.gz`. Each archive contains exactly
`hy2tun-manager`, `manager.example.yaml`, `hy2tun-manager.service`,
`README.md`, and `README.zh-CN.md`; it never includes `hy2tund` or `hy2tun`.
`--dry-run` prints the
planned archive paths without building. `make build` and `scripts/release.sh`
continue to produce only the existing `hy2tund`/`hy2tun` artifacts.

## Troubleshooting

- **`manager.key does not match this state` or a decrypt error at startup**:
  restore `state.json` and `manager.key` from the same backup. Do not delete the
  key to force a new one.
- **Login keeps failing after a reset**: restart the service. The reset command
  does not hot-reload a running process.
- **State update fails after running reset as root**: the temp file inherited
  root ownership. Restore the service user's ownership or run the reset as the
  service user.
- **Server shows `auth_error`**: the stored upstream administrator password is
  wrong or the server session endpoint changed. Edit the server credentials.
- **Server shows `unreachable`**: check DNS, proxy path, TLS certificate, and
  the base URL from the manager host. Use `tls_skip_verify` only for a known
  self-signed test endpoint.
- **Mutations blocked with an audit error**: the manager fails closed when it
  cannot durably append the audit log. Fix permissions or disk space on
  `/var/lib/hy2tun-manager/audit.jsonl`.
- **Login rate limited behind a proxy**: leave `trust_proxy: false` unless the
  proxy overwrites the forwarded IP headers, then enable it and restart.