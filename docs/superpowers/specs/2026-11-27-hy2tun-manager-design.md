# hy2tun-manager multi-server control plane design

- Date: 2026-11-27
- Status: approved
- Scope: a new manager-only service that aggregates existing `hy2tund` servers and their clients without changing `hy2tund` or `hy2tun`

## 1. Intent

The fleet currently has three `hy2tund` servers and several `hy2tun`
clients. Clients may connect to one, several, or all of those servers. The
operator wants one place to see and manage the fleet instead of opening each
server console separately.

The manager is an aggregation control plane. It does **not** replace the
server/client data plane and does **not** become the source of truth for
client configuration. Each `hy2tund` remains authoritative for its own
clients, mappings, exposure, credentials, and settings. The manager stores
only the server inventory and the operator's access to those servers.

## 2. Goals and non-goals

### Goals

- Add, edit, remove, and health-check multiple `hy2tund` endpoints.
- Show a unified server inventory with online state, version, mode, client
  count, mapping count, and last refresh time.
- Open a server and manage its existing clients and mappings.
- Support mapping add, edit, delete, enable/disable, and `public`/`local`
  exposure changes.
- Support server settings that the existing API exposes: transport mode,
  port policy, Web administrator credentials, and trust-proxy.
- Support credential rotation and revoke through the existing server API.
- Keep all writes synchronous and show upstream errors verbatim enough for an
  operator to fix them.
- Record an audit trail of manager actions without storing secret values.
- Keep a single administrator account and a simple deployment.

### Non-goals

- No changes to `hy2tund` or `hy2tun` source code.
- No client-local configuration editing (Web, proxy, failover, TLS, logs,
  `servers:` list).
- No adding or removing client-to-server connections.
- No offline client mapping push.
- No client deletion from server state in v1.
- No process restart, binary upgrade, or deployment orchestration.
- No multi-user accounts or roles.
- No embedding the existing server console in an iframe.
- No automatic cross-server transactions or write queues.
- No attempt to merge one physical client across multiple servers into a
  single identity.
- No server data-plane, protocol, or credential-format changes.

## 3. Current constraints

These are facts about the existing system that shape the design:

- A server's Web console is the only management interface. It exposes JSON
  APIs for overview, clients, mappings, settings, events, and credential
  operations.
- Mutations require an authenticated session plus
  `X-Requested-With: hy2tun` and an `Origin` header whose scheme/host match
  the request. There is no long-lived machine API token.
- Server Web sessions are HMAC-signed cookies with a seven-day TTL. There is
  one administrator account per server.
- The manager must therefore store each server's administrator username and
  password, log in to each server, and keep an in-memory cookie jar. The
  password is needed again after a server restart or session expiry.
- Server APIs push mappings only while the client is online. Offline clients
  are read-only at the mapping layer.
- A mapping update is a full-list replacement with a base revision. A stale
  revision must be surfaced as a conflict, not merged automatically.
- A client's server list is client-local configuration. The server cannot
  read or change it, so the manager cannot know that the same physical client
  connected to two servers.
- The existing consoles send `X-Frame-Options: DENY` and
  `Content-Security-Policy: frame-ancestors 'none'`; a manager cannot embed
  them.
- The operator provides a reachable base URL for each server through their
  own reverse proxy. The manager does not manage proxies, DNS, SSH tunnels, or
  certificates.
- The manager detects the upstream version and optional API capabilities from
  the server's existing summary/version data. A server older than the feature
  being used gets the affected actions marked “unsupported” rather than
  failing the whole inventory. Full feature coverage is validated against
  v1.7.0+; v1.7.4 is the recommended baseline.

## 4. Approaches considered

### A. Independent manager service with API adapters (selected)

A new `hy2tun-manager` process owns inventory, authentication, an upstream
`ServerClient` per server, and a server-rendered Web UI. It talks to the
existing server APIs over HTTP/HTTPS.

Trade-off: one more service and a copy of the operator's server admin
credentials, but clean boundaries, no data-plane risk, and no changes to
existing binaries.

### B. Embed the manager in one `hy2tund`

Rejected. The manager would be tied to one server's lifecycle, upgrade path,
and failure domain. A failure on that server would remove the control plane.

### C. Portal with links/iframes to existing consoles

Rejected. It is only a list of links, not unified management. The existing
security headers block iframes, and separate logins would remain.

## 5. Architecture

```text
browser ──HTTPS──> hy2tun-manager ──HTTPS──> reverse proxy ──> hy2tund A
                         │                                  └─> hy2tund B
                         │                                  └─> hy2tund C
                         ├── state.json (inventory, encrypted server passwords)
                         ├── manager.key (encryption key, mode 0600)
                         └── audit log (append-only, no secrets)

hy2tun clients ──HY2/TCP──> hy2tund servers (unchanged)
```

The manager is a separate binary in the same Go module:

- `cmd/hy2tun-manager`
- `internal/manager` — inventory, encrypted credentials, upstream adapter,
  Web handlers, templates, and static assets.

No existing server/client package behavior is changed. The manager may reuse
`internal/webui/shared` for session signing, login limiting, CSRF, and
template helpers, but it does not modify those packages' current callers.

## 6. Data model

The manager has a single state file with atomic replace, mode `0600`:

```json
{
  "admin": {
    "username": "admin",
    "password_hash": "$2b$..."
  },
  "servers": [
    {
      "id": "srv_<random>",
      "name": "us",
      "base_url": "https://us.example.net/hy2tund",
      "upstream_username": "admin",
      "upstream_password_enc": "<AES-GCM ciphertext>",
      "tls_skip_verify": false,
      "created_at": "2026-11-27T00:00:00Z",
      "updated_at": "2026-11-27T00:00:00Z"
    }
  ]
}
```

Properties:

- `id` is generated once; names are display labels and must be unique.
- `base_url` is an operator-provided URL. The manager treats it as an origin
  plus optional path prefix and joins API paths beneath it.
- `upstream_password_enc` is encrypted with AES-256-GCM. The key lives in
  `manager.key`, not in `state.json` or logs.
- The manager never returns upstream passwords to the browser; edit forms use
  write-only password fields and masked display.
- Audit entries are appended to a separate JSON-lines file. Each entry records
  timestamp, actor, server ID/name, action, target client/mapping ID, result,
  and an error code/message when applicable. Secrets are never recorded.
- Mutations are fail-closed on audit failure: if the audit append cannot be
  durably written, the manager does not send the upstream mutation and reports
  an operator-visible error. Failed/attempted mutations are also recorded when
  the audit file is available.

## 7. Manager authentication and security

- One administrator account. On first start, the manager generates a password,
  writes the bcrypt hash to state, and prints the generated password once to
  stderr/journal. An initial-password environment variable applies only when
  state is first created; an existing administrator is never silently reset.
  A `hy2tun-manager reset-admin` CLI command handles password reset after first
  start.
- The manager UI uses its own session manager and a distinct cookie name
  (`hy2tun_manager_session`) so it can share a host with a `hy2tund` console
  without evicting that console's `hy2tun_session`. Mutations require the same
  static `X-Requested-With: hy2tun` header and an Origin check.
- The manager config supports a `base_path` for reverse-proxy prefix
  deployment. It also has a `trust_proxy` switch for its login limiter; the
  default is false. Set it only when the manager's proxy overwrites
  `X-Forwarded-For` and `X-Real-IP`.
- The manager should normally use a dedicated hostname. Sharing a hostname is
  supported only through `base_path` and the distinct cookie name.
- Upstream server credentials are decrypted only in memory when creating an
  upstream session. They are never included in HTML, JSON responses, audit
  records, or error text.
- All manager mutations are explicit POST/PUT/DELETE handlers with a visible
  confirmation for destructive actions (delete mapping, rotate/revoke
  credentials, change server Web administrator).
- The manager warns when `tls_skip_verify` is enabled. The default is normal
  certificate verification. The operator's reverse proxy should present a
  trusted certificate.
- The manager listens on loopback by default. The operator must put HTTPS and
  authentication in front of it if it is exposed beyond a trusted network.

## 8. Upstream server adapter

One `ServerClient` is created per server. It owns:

- the base URL,
- a cookie jar,
- the upstream administrator username/password,
- a mutex around login and mutations for that server.

### Authentication lifecycle

1. Log in with the server's existing `POST /api/login` form using the stored
   administrator credentials.
2. Store the returned session cookie in the server's cookie jar.
3. Send `X-Requested-With: hy2tun` and `Origin: <base URL origin>` on every
   state-changing request.
4. On `401`, discard the cookie, log in once, and retry the request once.
5. On `403`, do not loop; report a CSRF/origin or permission error.
6. If login fails, mark the server as `auth_error` and keep the last known
   cached read data visible with its timestamp. Do not attempt destructive
   actions.

### Read operations used by the manager

- `GET /api/overview`
- `GET /api/clients`
- `GET /api/clients/{clientID}`
- `GET /api/mappings`
- `GET /api/settings`
- `GET /api/config/summary`
- optionally `GET /api/events` for a live activity feed; the first version may
  poll instead to keep the adapter small.

### Write operations used by the manager

- `PUT /api/clients/{clientID}/mappings` with the complete mapping list and
  the base revision fetched from the client detail. For a new `local` mapping,
  include the `exposures` map in this request so the existing server handler
  performs its safe disabled → local → enabled sequence. Do not implement a
  separate two-call exposure sequence.
- `PUT /api/clients/{clientID}/mappings/{mappingID}/exposure` for changing an
  existing mapping. This endpoint may be used for offline clients because
  exposure is server-side state; offline mapping edits remain disabled.
- `PUT /api/settings/mode`.
- `PUT /api/settings/ports`.
- `PUT /api/settings/web` (with a coordinated update of the stored upstream
  password when the Web administrator password changes).
- `PUT /api/settings/proxy` for trust-proxy.
- `PUT /api/settings/credentials` for rotation.
- `POST /api/settings/credentials/revoke`.

The adapter does not call client-local APIs and does not call `hy2tun` Web
endpoints directly. Clients behind NAT are intentionally out of its network
path.

### Error mapping

The adapter branches on the server's JSON `code`, not only on HTTP status:

- `401` from login or API: `auth_error`; show “upstream login failed” and an
  edit-credentials action.
- `409 conflict` on mapping push: show that the client changed on the server,
  discard the stale form, and refetch. Do not auto-retry.
- `409 client_offline`/`client_not_ready`: show that the client is offline and
  disable mapping edits; this is not a revision conflict.
- `409 push_rejected`/`partial_apply`: show the server's per-mapping results and
  refetch; never claim the whole push succeeded.
- `409 exposure_conflict`: show active exposure, unavailable families, and
  closed state from the response.
- `403` on a mutation: report CSRF/origin failure; `403` on Web-admin settings
  means the current Web password check failed and the manager must not update
  its stored credential.
- `429`: report the upstream rate limit and `Retry-After`; do not retry
  automatically.
- `404`: server/client/mapping disappeared; refetch the parent view.
- `501`/missing optional endpoint: mark that server capability unsupported.
- `5xx`/transport timeout: `unreachable`/`upstream_error`; show the last known
  data with timestamp and disable writes until a manual refresh succeeds.
- Upstream error bodies may be shown as operator-facing text after
  sanitization; they are never interpreted as HTML.

Request timeouts are per operation: 15 seconds for reads and mapping pushes
(the server's own client ack timeout is 10 seconds), and 30 seconds for
credential rotation so the server can report per-client results.

### Corrupt state and key recovery

The manager fails closed on startup if `state.json` is corrupt or if
`manager.key` is missing/does not match stored ciphertext. It prints a recovery
message and exits; it does not silently initialize a new key over existing
secrets. The operator must restore `state.json` and `manager.key` together.

## 9. Manager UI

The manager uses Go `html/template` with server-rendered pages and small
progressive-enhancement JavaScript for confirmations and inline refresh. No
SPA framework is required.

### Pages

- `/login` — manager administrator login.
- `/` — fleet overview:
  - server cards/table with name, base URL, state, version, mode, client
    count, mapping count, last refresh;
  - global refresh-all action;
  - clear offline/auth/conflict badges.
- `/servers` — add/edit/remove server inventory; test connection; masked
  upstream password; optional TLS-skip warning.
- `/servers/{id}` — selected server:
  - clients table with online/offline state and mapping count;
  - links to the client's mapping editor;
  - server settings tabs for mode, ports, Web admin, trust-proxy;
  - credential rotation/revoke;
  - read-only summary of config/version.
- `/servers/{id}/clients/{clientID}` — mapping editor:
  - existing mapping list;
  - add/edit/delete and enable/disable;
  - exposure selector for online clients;
  - revision conflict banner with reload;
  - offline clients show the mappings read-only.

The UI must show **server and client timestamps** next to cached data so an
operator knows when a view is stale.

### Server onboarding

Add server form fields:

- display name,
- base URL,
- upstream administrator username,
- upstream administrator password (write-only),
- `tls_skip_verify` (dangerous, default off).

“Test connection” performs login and `GET /api/overview` only. A failed test
does not save or overwrite existing credentials.

## 10. Mapping and settings semantics

- Mapping edits are full-list updates. The manager loads the current client
  detail, applies the user's edit, and sends the list with the fetched
  revision. If a `409` is returned, it does not retry with the new revision;
  it discards the stale form and asks the operator to reload.
- Enabling/disabling a mapping is a field change in the same list update.
- Adding a `local` mapping uses the `exposures` field on the same mapping push,
  so the server can keep it disabled until the local exposure is applied.
- Exposure is a separate server-side state mutation and remains independent
  of the mapping revision. It may be changed for offline clients because the
  server stores the desired value for the next bind.
- Offline clients: mapping edit controls are disabled; the manager shows the
  last known mappings and an explicit “离线客户端不可编辑” notice. Exposure
  remains available as a server-side state operation.
- Server settings writes are sequential per server. The manager shows the
  upstream response and refreshes the server after success.
- Changing the server Web administrator password requires a second field or
  confirmation. On success, the manager stores the new encrypted upstream
  password and then immediately re-authenticates. If the follow-up login fails,
  the manager marks the server `auth_error`; the new credential remains stored
  because the old password is no longer valid after the server accepted the
  change. The operator can correct it through the native console or the
  manager's edit-credentials form.
- Credential rotation requires the new HY2 auth/control token fields and the
  existing sync mode. The manager displays per-client results returned by the
  server. Revoke requires explicit confirmation.

## 11. Refresh and caching

- A background refresh polls each configured server every 30 seconds by
  default. This interval is configurable in manager config and can be
  refreshed manually per server/all servers.
- Cached data is for display only; every mutation starts with a fresh
  client-detail/read where the upstream API requires a revision.
- No mutation is queued while a server is offline. The UI disables writes and
  reports that the server is unreachable.
- Refresh runs with bounded concurrency (one in-flight request per server) so
  a slow server cannot block the whole manager.

## 12. Deployment and release

- New binary: `hy2tun-manager`.
- Manager config:

```yaml
listen: "127.0.0.1:7600"
base_path: "/"                 # optional reverse-proxy prefix
state_file: "/var/lib/hy2tun-manager/state.json"
key_file: "/var/lib/hy2tun-manager/manager.key"
audit_file: "/var/lib/hy2tun-manager/audit.jsonl"
refresh_interval: "30s"
trust_proxy: false             # set only behind a trusted reverse proxy
```

- The manager is not part of the `hy2tund`/`hy2tun` archives. It gets its own
  build target and release artifact. Initial release targets are
  linux/amd64 and linux/arm64; no Windows/mips manager build is required.
- The operator supplies HTTPS/reverse proxy and network access to each
  server's base URL.
- Running the manager does not alter existing server configs, credentials, or
  firewall rules.

## 13. Testing

- Unit tests for the upstream adapter using `httptest`:
  - login, cookie jar, Origin/CSRF headers;
  - 401 → one re-login;
  - local-mapping `exposures` push uses the server's safe sequence;
  - `409` classification for conflict, client offline, partial apply, and
    exposure conflict;
  - offline/timeout and 5xx error mapping;
  - mapping list replacement and exposure requests; offline exposure is allowed.
- State/security tests:
  - encrypted upstream password round-trip;
  - secrets never appear in JSON responses/audit;
  - audit failure blocks a mutation;
  - atomic state write, missing key, and corrupt-state handling;
  - distinct manager cookie and single-admin login/CSRF rejection.
- UI tests:
  - server add/edit/remove;
  - server overview with online/offline/auth-error states;
  - client mapping edit and conflict banner;
  - server settings forms and credential confirmations.
- End-to-end: run a real `hy2tund` test server with a fake client, point the
  manager at an `httptest`/reverse-proxy URL, and verify login, list, mapping
  push, exposure, and credential rotation.
- Existing `hy2tun` tests must remain unchanged and pass; the manager must not
  import or alter server runtime internals.

## 14. Acceptance criteria

- An operator can add the three production servers and see their status and
  client counts from one page.
- An operator can open any online server client and add/edit/delete/enable/
  disable a mapping through the manager, including a new `local` mapping that
  never binds publicly.
- A stale mapping edit shows a conflict and does not silently overwrite.
- An operator can change exposure for an existing mapping, including an
  offline client's stored exposure; offline mapping edits stay disabled.
- An operator can change server mode, port policy, Web admin, and trust-proxy.
- An operator can rotate and revoke server credentials and see per-client
  results.
- Offline clients and unreachable servers are clearly read-only.
- No manager response or audit entry contains upstream passwords, HY2
  passwords, control tokens, or certificate private material.
- Existing `hy2tund`/`hy2tun` tests and release artifacts remain functional.

## 15. Open decisions left to the builder

- Exact Go package split under `internal/manager`.
- Whether the first version includes the server event stream or only periodic
  polling.
- Whether the manager UI reuses the existing CSS files or starts with a small
  dedicated stylesheet; it must not depend on server templates.
- Whether credential reveal is included; rotation and revoke are required,
  reveal is optional and should remain masked by default.