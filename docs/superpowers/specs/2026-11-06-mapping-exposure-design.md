# Mapping exposure control design

- Date: 2026-11-06
- Status: draft for approval
- Scope: server-side per-mapping public/local exposure, Web/API/CLI controls, and removal of redundant Web-password re-authentication

## 1. Intent

The server operator may run a reverse proxy on the same host as `hy2tund`.
Some mappings should not be reachable directly from the public internet; the
reverse proxy should connect to them over loopback instead. The client still
owns the mapping definition, but the server owns whether that mapping is
publicly exposed.

This is a per-mapping, server-side switch:

- `public`: listen on the public dual-stack addresses, preserving current
  behavior and the default for new mappings.
- `local`: listen only on `127.0.0.1` and `::1`, so a reverse proxy on the same
  host can reach the mapping while the public internet cannot.
- Switching to `local` closes existing public connections immediately.
- Switching in either direction rebuilds the mapping listeners; existing
  connections are closed rather than migrated.

## 2. Decisions already made

- Default for a new mapping: `public`.
- State keyed by client ID + mapping ID; persisted in the server state file.
- Restart and client reconnect retain the exposure state.
- Deleting a mapping clears its state; recreating the same ID starts `public`.
- Both modes prefer dual-stack:
  - `public`: IPv4 and IPv6 public listeners.
  - `local`: `127.0.0.1` and `::1` listeners.
- If one address family cannot bind, bind the other family and report which
  family is unavailable. Surface the warning in the API, CLI, server UI, and
  client read-only view; never fail silently or pretend both are active.
- If neither family can bind, the bind fails. A live switch then rolls back to
  the previous successfully active exposure; if rollback also fails, keep the
  mapping closed. A `public -> local` rollback may therefore leave the mapping
  briefly public again; it must be reported, never hidden.
- Offline, disabled, or already-closed mappings store the requested exposure
  immediately for the next bind; there is no live listener to roll back.
- Applies to TCP and UDP mappings.
- Web console, HTTP API, and a `hy2tund` CLI subcommand are all required.
- CLI authenticates with the current Web password, entered interactively; it
  is never passed on the command line.
- Client console shows the server-selected exposure read-only.
- Except for changing the Web administrator username/password, all settings
  and credential mutations stop asking for the current Web password. Session
  authentication and CSRF protection remain.
- Changing the Web administrator still requires the current Web password.

## 3. Exposure semantics

`view.MappingView` gets an `Exposure` string field:

- `"public"` — public dual-stack listeners.
- `"local"` — loopback-only dual-stack listeners.
- empty — legacy/unknown; treat as `public`.

The field is a server read model, not part of the client YAML mapping. A client
must never write it back to its configuration.

Public mode:

- If `listen_host` is empty, bind the IPv4 wildcard and IPv6 wildcard on the
  assigned port. The IPv6 wildcard socket must be explicitly v6-only so it does
  not conflict with the IPv4 wildcard socket.
- If `listen_host` is a specific address, bind that address only. The operator
  has explicitly opted out of automatic dual-stack in that case.
- The assigned port and its reservation behavior are unchanged.

Local mode:

- Ignore `listen_host`.
- Bind `127.0.0.1` and `::1` for TCP and UDP on the same assigned port.
- The logical port remains reserved exactly as in public mode; another mapping
  cannot claim it.
- No public listener remains after the switch completes.

Switching a live mapping:

1. Serialize exposure changes per mapping.
2. Close the old listeners and active TCP connections/UDP wrappers immediately.
3. Bind the new listener set, trying both families; keep whichever family
   binds and record the unavailable family as a warning.
4. Persist the new exposure state.
5. Start accepting on the available listeners.
6. Notify capable online clients of the new exposure and any unavailable
   family.

Failure handling for a live switch: if neither family can bind, restore the
previous listener set and previous exposure. If the restore also fails, keep
the mapping closed and report the actual state. A `public -> local` total bind
or persistence failure can therefore leave the mapping briefly public again;
that is an explicit, reported rollback, not a silent downgrade. A
`local -> public` total failure returns to local or, if that fails, closed.

Offline, disabled, or already-closed mappings: persist the requested state
immediately and apply it on the next bind; there is no live listener to roll
back. If that next bind fails completely, keep the mapping closed and keep the
requested state for the next retry. New mappings default to public. If a
mapping disappears from a client registration, remove its stored exposure. If
the same ID returns later, it starts public.

## 4. Server state and runtime

Add to `state.ClientState`:

```go
Exposures map[string]string `json:"exposures,omitempty"`
```

Absent or empty means public. Only `local` needs an entry, but explicit
`public` may also be stored; both must survive a restart. State methods should
provide get/set/delete-by-mapping helpers; deletion of a client removes its
exposures. State files written by older versions remain readable.

`mappingRuntime` gets an exposure value and a listener set instead of one TCP
listener and one UDP socket. The listener set owns the IPv4/IPv6 TCP and UDP
sockets for the selected mode and closes active TCP/UDP sessions when closed.
It binds families independently: a family that fails is recorded as
unavailable and must not prevent the other family from serving. If no family
binds, the mapping remains closed/failed. Keep the existing single public port
semantics, active-connection accounting, and mapping status read model.

Add a stable read-only field such as
`UnavailableFamilies []string` (`"ipv4"` / `"ipv6"`) to the mapping read model.
It is runtime diagnostics, not persisted state, and contains no raw OS error
text. The server logs the underlying error; the API/UI/CLI use the stable
family code. The warning is recomputed on every bind attempt and clears when
that family binds again.

The mapping manager is responsible for:

- applying stored exposure on every bind path: initial registration, client
  updates, server mapping pushes, enabling a disabled mapping, and retrying a
  failed bind. It must not be possible to reopen a local mapping publicly just
  because it was bound through an update path;
- defaulting new mappings to public;
- exposing a `SetExposure(clientID, mappingID, exposure)` operation used by the
  Web API and CLI;
- persisting/removing exposure on mapping changes;
- rejecting unknown clients/mappings and serializing toggles per mapping.

For an online mapping with a live listener, the stored value is the last
successfully active exposure. For an offline, disabled, or closed mapping, the
stored value is the desired exposure for the next bind.

## 5. Client visibility and protocol

`view.MappingView.Exposure` is populated on the server. The client needs a
read-only copy without treating it as configuration:

- Add an opt-in capability to the client `Hello` (for example,
  `capabilities: ["exposure-v1"]`). Old servers ignore the extra JSON field.
- Include exposure and unavailable families in `control.Result` so the initial
  `HelloAck` and mapping pushes carry them. Old clients ignore the extra
  fields.
- Add a new control message type, sent only to clients that advertised the
  capability, when the server changes exposure or address-family availability
  at runtime. Old clients never receive it, so they cannot fail on an unknown
  message.
- The client stores the values in its mapping read model only; it must not
  write them to `client.yaml` or increment the mapping revision.
- The client Web console renders a read-only `公网`/`仅本机` badge per mapping,
  plus a warning such as `IPv6 不可用` when the server reports one. If the
  server did not report a value, display nothing or `—`, not a guessed state.

New client + old server and old client + new server must remain compatible.

## 6. HTTP API

Add:

```text
PUT /api/clients/{clientID}/mappings/{mappingID}/exposure
```

Request body:

```json
{"exposure":"public"}
```

or

```json
{"exposure":"local"}
```

- Requires an authenticated Web session and the existing CSRF header.
- Does not require the current Web password.
- Returns the updated mapping read model (including `exposure`) on success.
- `400` for an invalid exposure value or malformed body.
- `404` for unknown client or mapping.
- `409` when no address family can bind or persistence fails; the response
  must say which exposure and listener state remains active after rollback
  (or that the mapping is closed).
- A successful partial bind is `200` with `unavailable_families` set; it is
  not an error, but the warning must be visible to the caller.
- `500` for unexpected internal failures.

Existing `GET /api/clients/{id}` and `GET /api/mappings` include the exposure
field automatically.

## 7. CLI

Add a `hy2tund` subcommand:

```text
hy2tund exposure --client <client-id> --mapping <mapping-id> --mode public|local
```

- Reads the server config (default `server.yaml`; allow the existing config
  flag path) to discover the Web listener and username.
- Resolves the Web target from `web.listen`: a wildcard bind uses `127.0.0.1`
  first and `::1` as fallback; a specific address or hostname is used as
  configured. It never invents a remote host.
- Prompts for the current Web password interactively. The password must not
  appear in argv, environment, logs, or shell history.
- Logs in to the Web console, uses the session cookie and the
  `X-Requested-With: hy2tun` CSRF header, and calls the new API.
- Prints the resulting exposure and any unavailable address family, and exits
  non-zero on error.
- Does not start or stop the server.
- If the Web console is configured with TLS, use the configured scheme and
  normal certificate verification with the configured host/SNI. A self-signed
  Web certificate requires an explicit opt-in flag; do not silently disable
  verification.

## 8. Web console

Server console:

- Add a `公网/仅本机` control to each mapping in the mappings list and client
  detail.
- The control is a deliberate action with a short confirmation that switching
  will immediately close existing connections.
- No current-password field. Use the existing session/CSRF protections.
- Show the result and any fail-closed error clearly.

Client console:

- Show a read-only `公网/仅本机` badge in the mapping table.
- The client cannot change it.
- If unknown/legacy, show `—`.

## 9. Remove redundant Web-password re-authentication

Keep the current-password check only for:

- client Web administrator username/password changes;
- server Web administrator username/password changes.

Remove the current-password requirement and the corresponding form fields from:

- client transport mode;
- client failover timing;
- client name;
- client credential rotation and secret reveal;
- server transport mode;
- server trust-proxy;
- server credential rotation, revoke, and secret reveal;
- the new exposure API/UI/CLI.

Keep:

- login authentication;
- session cookies;
- CSRF header;
- login and Web-admin rate limiting;
- all existing validation and atomic-write behavior.

Update tests to assert that non-Web-admin mutations succeed with a valid session
and CSRF without a password, while Web-admin changes still reject a missing or
wrong current password.

## 10. Compatibility and security

- Old state files without `exposures` remain valid and behave as public.
- Old clients and servers continue to work; exposure defaults to public and
  visibility is optional.
- The server state file contains no credentials; exposure metadata is safe to
  persist.
- Local mode relies on loopback binding, not on firewall rules. The server
  operator's reverse proxy is expected to connect to `127.0.0.1` or `::1`.
- Removing re-authentication weakens defense against a stolen authenticated
  session. Session/CSRF protections remain, and the user explicitly requested
  this trade-off. Document it in the changelog.
- No command-line password exposure.
- A partial dual-stack bind is reported as a warning, never hidden. A total
  failure reports and rolls back; a brief return to public on a failed
  `public -> local` switch is possible and must be reported. Exposure itself
  never changes silently.

## 11. Verification

- Unit: default public; state round-trip; delete clears; recreation defaults
  public; local binds only loopback; public binds dual-stack; one-family
  partial bind keeps the other and reports the missing family; `listen_host`
  behavior; mapping offline persistence; rollback on total failure.
- API: auth, CSRF, no password required, 400/404/409 paths, partial-bind 200
  with `unavailable_families`, response exposure.
- CLI: password prompt (no argv), login, CSRF, success/error output, self-signed
  Web TLS opt-in behavior.
- Web: server toggle confirmation, client read-only badge, no password fields
  on non-Web-admin forms, Web-admin still requires current password.
- E2E: public mapping reachable on both families; local mapping refused on
  public addresses but reachable via loopback; switch closes existing
  connections immediately; client sees updated exposure; restart/reconnect
  retains state; old client/server compatibility.
- `go test ./...`, `-race`, `vet`, `staticcheck`, `node --check`, and OpenWrt
  package/LuCI tests.

## 12. Non-goals

- No per-mapping custom bind address beyond `public` / loopback-only.
- No automatic reverse-proxy configuration or domain routing.
- No approval workflow, multi-user roles, audit log, or TOTP.
- No automatic public re-exposure when a bind fails.
- No migration that writes exposure into client YAML.

## 13. Left to the builder

- Exact listener abstraction and v6-only socket setup.
- State helper names and whether explicit `public` entries are stored or removed.
- Exact control-message name/fields and capability string.
- API response shape after a successful toggle.
- CLI flag placement and Web-TLS opt-in flag name.
- Confirmation-dialog wording.
- Whether disabled/closed mappings return the requested exposure immediately
  or a separate pending flag; the spec requires the requested state to survive
  for the next bind.
- How `listen_host` values `0.0.0.0` and `::` are classified: treat explicit
  wildcard values as wildcard/dual-stack, and specific addresses as
  single-address choices.
- CLI Web-TLS certificate/SNI defaults for wildcard listeners.