# Mapping Exposure Control Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add per-mapping server-controlled `public`/`local` exposure with dual-stack listeners, Web/API/CLI controls, persisted state, client read-only visibility, and remove redundant current-password confirmation from all settings except Web-admin changes.

**Architecture:** The client remains authoritative for mapping definitions; the server stores a per-client/per-mapping exposure override and applies it on every bind path. Mapping listeners become a family-aware set (IPv4/IPv6 TCP and UDP), with partial-family bindings reported as warnings. Exposure changes rebuild listeners and close existing connections. Web/API/CLI all call one server operation; the client learns exposure through an opt-in control capability and a new control message.

**Tech Stack:** Go 1.26, existing control protocol, `golang.org/x/term` for CLI password input, existing server state store, `net.ListenConfig`/`net.ListenUDP`, Web UI templates/JS, testify.

**Spec:** `docs/superpowers/specs/2026-11-06-mapping-exposure-design.md`

## Global Constraints

- Exposure values are only `public` and `local`; default for new mappings is `public`.
- State is keyed by client ID + mapping ID, persists across restart/reconnect, and is cleared on mapping deletion; recreation starts public.
- `public` prefers IPv4 and IPv6; `local` binds `127.0.0.1` and `::1`.
- One family may fail: bind the other and report `unavailable_families` (`ipv4`/`ipv6`); both failing is a failure.
- `public -> local` closes existing public connections immediately. A failed live switch rolls back to the previous active exposure and reports it; a brief public return is allowed only as an explicit, reported rollback.
- A failed switch must never silently change exposure or silently serve only one family.
- Every bind path applies stored exposure: registration, client update, server push, enable, and retry.
- Web/API/CLI exposure controls require session authentication and CSRF, not the current Web password.
- Only changing the Web administrator username/password keeps the current-password check. Remove it from all other settings and credential actions.
- Client console shows exposure and unavailable-family warnings read-only; it never writes them to YAML.
- Old state files, old clients, and old servers remain compatible. Exposure visibility is opt-in via a Hello capability.
- No secrets in logs, metrics labels, or command-line arguments.

## Review Focus

- A live `public -> local` switch that fails binding must roll back to public and report it, not leave a half-bound mapping.
- A mapping set to local must stay local after client update, server push, disable/re-enable, reconnect, and restart.
- A one-family bind failure must keep the other family serving and show the warning in API, Web, CLI, and client view.
- Old clients must never receive an unknown exposure control message, and old servers must still work with new clients.
- Removing re-authentication must not remove session auth, CSRF, login rate limiting, or the Web-admin current-password check.

---

### Task 1: Exposure state and read model

**Files:**
- Modify: `internal/state/store.go`
- Test: `internal/state/store_test.go`
- Modify: `internal/view/view.go`

**Interfaces:**
- Produces:
  ```go
  // internal/view
  type Exposure string
  const (
      ExposurePublic Exposure = "public"
      ExposureLocal  Exposure = "local"
  )
  func (e Exposure) Valid() bool

  // view.MappingView additions
  Exposure            Exposure
  UnavailableFamilies []string
  ```
- Produces:
  ```go
  // state.ClientState
  Exposures map[string]string `json:"exposures,omitempty"`

  // state.Store
  func (s *Store) Exposure(clientID, mappingID string) string
  func (s *Store) SetExposure(clientID, mappingID, exposure string) error
  func (s *Store) DeleteExposure(clientID, mappingID string) error
  ```
  `SetExposure`/`DeleteExposure` persist atomically and restore in-memory state on save failure. Empty/`public` is the default; storing explicit `local` is required.

- [ ] **Step 1: Write failing state tests**

In `internal/state/store_test.go`, add `TestExposureStatePersists`, `TestExposureDelete`, and `TestExposureSaveFailure`. They assert:
- open a store, `SetExposure("c1","m1","local")`, reopen, assert `Exposure` is `local`;
- assert absent exposure is empty/public;
- delete the client and assert exposure is gone;
- `DeleteExposure` clears local;
- a save failure (inject `syncDirFn`) leaves the in-memory value unchanged and returns an error.

Use a temp state path and the existing store test helpers.

- [ ] **Step 2: Run state tests to verify failure**

Run: `go test ./internal/state -run TestExposure -count=1`
Expected: FAIL with undefined `SetExposure`/`Exposure`.

- [ ] **Step 3: Implement state and view types**

Add `Exposures` to `ClientState`; clone it in `clone`; preserve it in `Upsert` when the input is nil. Implement get/set/delete with `saveLocked` and rollback on non-committed errors, mirroring `DeleteAndSave`. Add `view.Exposure`, `Valid`, and the two `MappingView` fields. Keep zero/empty exposure omitted from JSON.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/state ./internal/view -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/state/store.go internal/state/store_test.go internal/view/view.go
git commit -m "feat(state): persist per-mapping exposure"
```

---

### Task 2: Family-aware TCP and UDP listener sets

**Files:**
- Create: `internal/server/mapping_listeners.go`
- Modify: `internal/server/mapping_runtime.go`
- Modify: `internal/server/mapping_bind.go`
- Modify: `internal/server/udp.go`
- Test: `internal/server/mapping_listeners_test.go`
- Test: `internal/server/udp_test.go`

**Interfaces:**
- Produces:
  ```go
  type mappingListenerSet struct {
      TCP []net.Listener
      UDP *multiUDPConn
      UnavailableFamilies []string
  }
  func (s *mappingListenerSet) Close() error
  func (s *mappingListenerSet) Port() int
  func (s *mappingListenerSet) Warnings() []string
  ```
- `mappingRuntime` replaces `tcpListener net.Listener` and `udpConn *net.UDPConn` with `listeners *mappingListenerSet`; add `exposure view.Exposure`.
- `publicUDPConn` wraps a `udpPacketConn` interface instead of `*net.UDPConn`.
- `bindLocked` binds a set for the runtime's exposure; `assignedPort` comes from the first successful socket.

- [ ] **Step 1: Write failing listener tests**

Add `TestLocalTCPListeners`, `TestPublicDualStack`, `TestPartialFamilyWarning`, `TestBothFamiliesFail`, and `TestUDPDualStack`:
- local TCP binds `127.0.0.1` and `::1` on the same port; a public dial fails;
- public wildcard TCP binds IPv4 and IPv6; both accept;
- when one family is pre-occupied, the other binds and `Warnings()` contains the missing family;
- when both are occupied, binding fails;
- local UDP receives datagrams from both loopback families and replies from the matching socket;
- public UDP dual-stack receives both families;
- close releases all sockets.

Use `net.ListenConfig`/`net.ListenUDP` with `127.0.0.1:0` and `[::1]:0` fixtures; skip IPv6 cases only if the host reports no IPv6 support via a helper, never silently.

- [ ] **Step 2: Run tests to verify failure**

Run: `go test ./internal/server -run 'TestMappingListener|TestDualStack' -count=1`
Expected: FAIL with undefined listener-set types.

- [ ] **Step 3: Implement the listener set**

Use separate `tcp4`/`tcp6` and `udp4`/`udp6` sockets. For public wildcard, bind `0.0.0.0` and `[::]` with explicit `IPV6_V6ONLY=1` on the IPv6 socket so both can coexist. For local, bind `127.0.0.1` and `::1`. If `listen_host` is a specific address, bind only that address and do not warn about the other family. Bind families independently; collect errors and keep successes.

Implement `multiUDPConn` as a `udpPacketConn`:
- fan-in reads from all sockets with a goroutine per socket and a select loop;
- `WriteTo` chooses the socket by the destination address family;
- `SetReadDeadline` applies to all sockets;
- `Close` closes all and joins readers.

- [ ] **Step 4: Wire bind/close**

Update `mapping_bind.go` to allocate the port, bind the set, set `assignedPort`, start one accept loop per TCP listener, and return a `control.Result` with `UnavailableFamilies`. Update `mappingRuntime.close` and `publicUDPConn`/`attachPublicUDPConn` to use the set/interface. Keep the existing single-token UDP semantics.

- [ ] **Step 5: Run tests**

Run: `go test ./internal/server -count=1`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/server/mapping_listeners.go internal/server/mapping_listeners_test.go internal/server/mapping_runtime.go internal/server/mapping_bind.go internal/server/udp.go internal/server/udp_test.go
git commit -m "feat(server): add family-aware mapping listeners"
```

---

### Task 3: Apply exposure on every bind path and implement the switch transaction

**Files:**
- Create: `internal/server/mapping_exposure.go`
- Modify: `internal/server/mapping.go`
- Modify: `internal/server/mapping_bind.go`
- Modify: `internal/server/mapping_session.go`
- Test: `internal/server/mapping_exposure_test.go`
- Test: `internal/server/mapping_test.go`

**Interfaces:**
- Produces:
  ```go
  func (m *mappingManager) exposureFor(clientID, mappingID string) view.Exposure
  func (m *mappingManager) SetExposure(clientID, mappingID string, exposure view.Exposure) (view.MappingView, error)
  func (s *Server) SetMappingExposure(clientID, mappingID string, exposure view.Exposure) (view.MappingView, error)
  ```
- `SetExposure` validates the value, finds the live mapping or persisted state, closes/rebinds a live mapping, persists after a successful bind, rolls back on total failure, and returns the updated read model.
- `bindLocked`/`applyLocked`/`updateLocked`/push paths all call `exposureFor`; a mapping set to local must not reopen public via any path.

- [ ] **Step 1: Write failing exposure tests**

Add `TestMappingExposureDefaultPublic`, `TestMappingExposureSwitchLocal`, `TestMappingExposureRollback`, `TestMappingExposurePersistsAcrossReconnect`, and `TestMappingExposureAllBindPaths`:
- new mapping defaults public;
- set local on a live TCP mapping closes an existing public connection and binds only loopback;
- set public from local closes loopback connections and binds dual-stack;
- a failed public->local bind (both families occupied) restores public and returns the previous exposure;
- a failed local->public bind restores local;
- one-family failure keeps the other family and returns `UnavailableFamilies`;
- offline mapping stores local, reconnect applies local;
- disable/re-enable, client update, and server push keep local;
- mapping deletion clears stored exposure; recreation is public;
- concurrent toggles serialize and leave one coherent state.

- [ ] **Step 2: Run tests to verify failure**

Run: `go test ./internal/server -run 'TestMappingExposure' -count=1`
Expected: FAIL with undefined `SetExposure`.

- [ ] **Step 3: Implement exposure lookup and transaction**

Add `exposureFor` with default public. Pass the exposure into `newRuntime`/`bindLocked` on every path. Update `persistLocked` to carry the existing `Exposures` map into the upserted `state.ClientState` so ordinary mapping edits do not erase exposure. Implement `SetExposure` with the per-mapping serialization already used by control updates (`sess.controlMu` for live sessions; manager lock for state-only). For a live switch:
1. capture old exposure/listeners;
2. close old listeners and active connections;
3. bind the new set;
4. persist state;
5. start serving;
6. on failure, close new partial listeners, restore old listeners/exposure, and return a typed error containing the active exposure.

For offline/disabled/closed mappings, persist the requested exposure and return the read model. Use `state.Store.SetExposure` and `Save` errors explicitly; do not use the best-effort `saveState` for this transaction.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/server -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/server/mapping_exposure.go internal/server/mapping_exposure_test.go internal/server/mapping.go internal/server/mapping_bind.go internal/server/mapping_session.go internal/server/mapping_test.go
git commit -m "feat(server): apply and switch mapping exposure"
```

---

### Task 4: Control protocol and client read-only exposure

**Files:**
- Modify: `internal/control/message.go`
- Modify: `internal/control/frame.go`
- Modify: `internal/server/control.go`
- Modify: `internal/server/mapping.go`
- Modify: `internal/client/control.go`
- Modify: `internal/client/control_reader.go`
- Modify: `internal/client/client.go`
- Test: `internal/control/frame_test.go`
- Test: `internal/client/control_persistence_test.go`
- Test: `internal/server/control_test.go`

**Interfaces:**
- `const CapabilityExposure = "exposure_v1"`.
- `control.Result` adds `Exposure string` and `UnavailableFamilies []string`.
- New message:
  ```go
  const TypeExposureUpdate = "exposure_update"
  type ExposureUpdate struct {
      Type                string   `json:"type"`
      ID                  string   `json:"id"`
      Exposure            string   `json:"exposure"`
      UnavailableFamilies []string `json:"unavailable_families,omitempty"`
  }
  ```
- Server stores `canExposureUpdate` from Hello capabilities and sends `ExposureUpdate` only to capable clients after a successful toggle.
- Client `Hello` advertises `CapabilityExposure`; `mappingViews`/`helloViews` copy exposure and warnings; `handleExposureUpdate` updates the client mapping read model without writing config or bumping revision.

- [ ] **Step 1: Write failing protocol tests**

Add `TestExposureCapability`, `TestExposureUpdateOldClient`, and `TestExposureClientView`:
- encode/decode `ExposureUpdate`;
- old `control.Result` JSON without new fields still decodes;
- a client with the capability sends it in Hello;
- a client without it never receives `ExposureUpdate`;
- new client + old server and old client + new server fixtures remain compatible;
- client mapping view shows exposure and warnings after HelloAck and after an update;
- config file revision and YAML do not change.

- [ ] **Step 2: Run tests to verify failure**

Run: `go test ./internal/control ./internal/client ./internal/server -run 'Exposure|Capability' -count=1`
Expected: FAIL with undefined fields/types.

- [ ] **Step 3: Implement protocol and wiring**

Add the capability, result fields, message type and decode case. In server registration, read `Capabilities`; in `SetExposure` success, send `ExposureUpdate` to a capable online client under the existing control writer/CSRF-free internal path. In client control reader, accept the new type only when the session advertised the capability; update the stored view under `c.mu`, no config write.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/control ./internal/client ./internal/server -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/control/message.go internal/control/frame.go internal/control/frame_test.go internal/server/control.go internal/server/mapping.go internal/server/control_test.go internal/client/control.go internal/client/control_reader.go internal/client/client.go internal/client/control_persistence_test.go
git commit -m "feat(control): propagate mapping exposure to capable clients"
```

---

### Task 5: Server HTTP API

**Files:**
- Create: `internal/webui/serverui/handlers_exposure.go`
- Modify: `internal/webui/serverui/handlers.go`
- Modify: `internal/webui/serverui/views.go`
- Test: `internal/webui/serverui/handlers_exposure_test.go`

**Interfaces:**
- `PUT /api/clients/{clientID}/mappings/{mappingID}/exposure`
- Optional API interface:
  ```go
  type ExposureAPI interface {
      SetMappingExposure(clientID, mappingID string, exposure view.Exposure) (view.MappingView, error)
  }
  ```
- Body: `{"exposure":"public"|"local"}`
- Success: `200` with the updated `view.MappingView` (including `Exposure` and `UnavailableFamilies`).
- Errors: `400 invalid_exposure`, `404 client/mapping not found`, `409 exposure_conflict` with the active exposure, `500 internal`.
- Session + CSRF required; no current password.

- [ ] **Step 1: Write failing API tests**

Add `TestExposureAPICSRFAndPassword`, `TestExposureAPINotFound`, `TestExposureAPIInvalidAndConflict`, and `TestExposureAPISuccess`. Use the existing serverui fake API and request helpers. Assert:
- missing CSRF is `403` and does not call the API;
- valid session + CSRF succeeds without a current password;
- unknown client/mapping is `404`;
- invalid value is `400`;
- a `409` from the API is surfaced with the active exposure;
- success returns exposure and `unavailable_families`.

- [ ] **Step 2: Run tests to verify failure**

Run: `go test ./internal/webui/serverui -run TestExposureAPI -count=1`
Expected: FAIL with 404/undefined handler.

- [ ] **Step 3: Implement the handler and route**

Add an optional interface to the serverui API (or extend the existing API with `SetMappingExposure`), decode with the existing bounded JSON helper, validate the value, call the server, and write the read model. Add the route with `s.auth(shared.RequireCSRF(...))`. Add `Exposure`/`UnavailableFamilies` to the serverui mapping read model if needed.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/webui/serverui -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/webui/serverui/handlers_exposure.go internal/webui/serverui/handlers_exposure_test.go internal/webui/serverui/handlers.go internal/webui/serverui/views.go
git commit -m "feat(web): add mapping exposure API"
```

---

### Task 6: Server and client Web UI

**Files:**
- Modify: `internal/webui/serverui/templates/mappings.html`
- Modify: `internal/webui/serverui/templates/client_detail.html`
- Modify: `internal/webui/serverui/static/app.js`
- Modify: `internal/webui/serverui/views.go`
- Modify: `internal/webui/clientui/templates/index.html`
- Modify: `internal/webui/clientui/static/app.js`
- Modify: `internal/webui/clientui/views.go`
- Test: `internal/webui/serverui/handlers_exposure_test.go`
- Test: `internal/webui/clientui/traffic_labels_test.go` or a new exposure test

**Interfaces:**
- Server mapping rows render a `公网/仅本机` control and warning; clicking asks for confirmation, calls the API, and refreshes the row.
- Client mapping rows render read-only `公网/仅本机` and `IPv4/IPv6 不可用` warnings.
- No current-password field on either exposure control.

- [ ] **Step 1: Write failing page tests**

Add `TestExposurePageControl`, `TestExposurePageWarning`, and `TestExposureClientBadge`. Assert the server page contains the exposure control and warning text, and the client page contains the read-only badge/warning and no exposure control. Assert the confirmation text mentions closing existing connections.

- [ ] **Step 2: Run tests to verify failure**

Run: `go test ./internal/webui/serverui ./internal/webui/clientui -run 'ExposurePage|ExposureBadge' -count=1`
Expected: FAIL with missing elements.

- [ ] **Step 3: Implement templates and JS**

Add a compact control (select or two-state button) to each server mapping row; use `fetch` with `X-Requested-With: hy2tun` and the selected server's API. Show a warning badge from `unavailable_families`. Client rows are static/read-only. Keep existing table columns and responsive behavior.

- [ ] **Step 4: Run tests and JS checks**

Run: `go test ./internal/webui/... -count=1 && node --check internal/webui/clientui/static/app.js && node --check internal/webui/serverui/static/app.js`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/webui/serverui/templates/mappings.html internal/webui/serverui/templates/client_detail.html internal/webui/serverui/static/app.js internal/webui/serverui/views.go internal/webui/clientui/templates/index.html internal/webui/clientui/static/app.js internal/webui/clientui/views.go internal/webui/serverui/handlers_exposure_test.go internal/webui/clientui/traffic_labels_test.go
git commit -m "feat(web): show and control mapping exposure"
```

---

### Task 7: `hy2tund exposure` CLI

**Files:**
- Modify: `cmd/hy2tund/main.go`
- Create: `cmd/hy2tund/exposure.go`
- Test: `cmd/hy2tund/exposure_test.go`
- Modify: `go.mod`/`go.sum` if `golang.org/x/term` is not already direct.

**Interfaces:**
- Command: `hy2tund exposure --client <id> --mapping <id> --mode public|local`
- Reads `-c`/default `server.yaml`, resolves `web.listen` (wildcard -> 127.0.0.1 then ::1; specific -> as configured), prompts for the Web password interactively, logs in, sends `X-Requested-With: hy2tun`, calls the API, prints exposure and warnings, exits nonzero on error.
- No password in argv/env/logs.

- [ ] **Step 1: Write failing CLI tests**

Add `TestExposureCLIParsesAndSends`, `TestExposureCLIRejectsMissingPassword`, `TestExposureCLILoginFailure`, and `TestExposureCLIConflict`. Use an `httptest.Server` fake for login/API. Assert:
- command parses and sends the correct request;
- missing password fails without calling the API;
- login failure/401 is reported;
- API 409 prints the active exposure and exits nonzero;
- success prints `public`/`local` and warnings;
- the password is not present in captured argv/log output.

- [ ] **Step 2: Run tests to verify failure**

Run: `go test ./cmd/hy2tund -run TestExposureCLI -count=1`
Expected: FAIL with undefined command.

- [ ] **Step 3: Implement the CLI**

Add a subcommand dispatch in `main` before the existing flag parse. Implement a testable `runExposure(args, stdin, stdout, stderr)` with `flag.NewFlagSet`. Use `term.ReadPassword` for a terminal; fall back to a single line read for non-terminal tests. Resolve the target and TLS scheme; add an explicit `--insecure` opt-in for self-signed Web TLS. Use the existing CSRF header value `hy2tun`.

- [ ] **Step 4: Run tests**

Run: `go test ./cmd/hy2tund -count=1 && go test ./... -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/hy2tund/main.go cmd/hy2tund/exposure.go cmd/hy2tund/exposure_test.go go.mod go.sum
git commit -m "feat(cli): add hy2tund exposure command"
```

---

### Task 8: Remove client Web-password re-authentication

**Files:**
- Modify: `internal/webui/clientui/handlers_mode.go`
- Modify: `internal/webui/clientui/handlers_failover.go`
- Modify: `internal/webui/clientui/handlers_settings.go`
- Modify: `internal/webui/clientui/templates/index.html`
- Modify: `internal/webui/clientui/static/app.js`
- Test: `internal/webui/clientui/handlers_mode_test.go`
- Test: `internal/webui/clientui/handlers_failover_test.go`
- Test: `internal/webui/clientui/handlers_test.go`

**Interfaces:**
- Remove `confirmCurrentPassword` calls from mode, failover, name, credential rotation, and credential reveal.
- Keep it only in `handleUpdateWebSettings` (Web administrator username/password).
- Remove corresponding current-password inputs and JS checks.
- Session + CSRF remain.

- [ ] **Step 1: Write failing tests**

Update `TestModeAPICSRFAndPassword`, `TestFailoverAPICSRFAndPassword`, `TestClientSettingsRequireSession`, and the name/credential tests to assert:
- mode/failover/name/rotate/reveal succeed with session + CSRF and no password;
- missing CSRF still fails;
- Web admin change still rejects missing/wrong current password and succeeds with correct password.

- [ ] **Step 2: Run tests to verify failure**

Run: `go test ./internal/webui/clientui -run 'ModeAPI|FailoverAPI|Name|Rotate|Reveal|WebSettings' -count=1`
Expected: FAIL on missing-password cases.

- [ ] **Step 3: Implement removals**

Remove the calls and password fields; keep the request structs compatible by ignoring any submitted password. Keep `settingsLimiter` for Web-admin confirmation. Update page templates and JS. Do not remove CSRF.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/webui/clientui -count=1 && node --check internal/webui/clientui/static/app.js`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/webui/clientui/handlers_mode.go internal/webui/clientui/handlers_failover.go internal/webui/clientui/handlers_settings.go internal/webui/clientui/templates/index.html internal/webui/clientui/static/app.js internal/webui/clientui/handlers_mode_test.go internal/webui/clientui/handlers_failover_test.go internal/webui/clientui/handlers_test.go
git commit -m "feat(web): remove redundant client password confirmation"
```

---

### Task 9: Remove server Web-password re-authentication

**Files:**
- Modify: `internal/webui/serverui/handlers_mode.go`
- Modify: `internal/webui/serverui/handlers_settings.go`
- Modify: `internal/webui/serverui/templates/settings.html`
- Modify: `internal/webui/serverui/templates/client_detail.html`
- Modify: `internal/webui/serverui/static/app.js`
- Test: `internal/webui/serverui/handlers_test.go`
- Test: `internal/webui/serverui/handlers_mode_test.go`

**Interfaces:**
- Remove `confirmCurrentPassword` from mode, trust-proxy, credential rotation, revoke, and reveal.
- Keep it only in `handleUpdateWebSettings`.
- Remove corresponding password inputs and JS checks; keep session + CSRF.

- [ ] **Step 1: Write failing tests**

Update `TestModeAPICSRFAndPassword`, `TestSettingsMutations`, and credential tests to assert mode/trust/rotate/revoke/reveal succeed with session + CSRF and no password; Web admin still requires the current password; missing CSRF still fails.

- [ ] **Step 2: Run tests to verify failure**

Run: `go test ./internal/webui/serverui -run 'ModeAPI|Trust|Rotate|Revoke|Reveal|WebSettings' -count=1`
Expected: FAIL on missing-password cases.

- [ ] **Step 3: Implement removals**

Remove the calls and password fields. Keep the Web-admin form's current-password field. Keep the settings limiter for Web-admin confirmation. Update templates/JS and tests.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/webui/serverui -count=1 && node --check internal/webui/serverui/static/app.js`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/webui/serverui/handlers_mode.go internal/webui/serverui/handlers_settings.go internal/webui/serverui/templates/settings.html internal/webui/serverui/templates/client_detail.html internal/webui/serverui/static/app.js internal/webui/serverui/handlers_test.go internal/webui/serverui/handlers_mode_test.go
git commit -m "feat(web): remove redundant server password confirmation"
```

---

### Task 10: E2E, compatibility, docs, and release handoff

**Files:**
- Create: `internal/e2e/exposure_test.go`
- Modify: `README.md`, `README.zh-CN.md`, `CHANGELOG.md`, `CHANGELOG.zh-CN.md`, `docs/feature-overview.zh-CN.md`
- Modify: `examples/server.yaml` if documenting `listen_host` behavior is needed
- Test: existing e2e helpers

**Interfaces:**
- E2E: public mapping reachable on available families; local mapping refused on public addresses but reachable via `127.0.0.1` and `::1`; one-family failure reports warning; switch closes existing public connections; restart/reconnect retains local; client read-only view updates; old client/server compatibility.
- Docs: default public, per-mapping server control, Web/API/CLI, no re-auth except Web admin, partial-family warning, rollback behavior.

- [ ] **Step 1: Write failing E2E tests**

Add `TestMappingExposureE2E`, `TestMappingExposurePartialFamily`, and `TestMappingExposureReconnect` using the existing real server/client helpers:
- start a mapping public, connect from a public address, switch local, assert existing connection closes and new public dials fail while loopback succeeds;
- switch back public, assert public works and local connections are rebuilt;
- simulate one family unavailable with a fixture (or unit-level helper) and assert warning propagation;
- restart/reconnect and assert local persists;
- old v1.5.4 client/server compatibility remains green.

- [ ] **Step 2: Run tests to verify failure**

Run: `go test ./internal/e2e -run 'TestMappingExposure|TestOldVersionCompatibility' -count=1`
Expected: FAIL with missing API/behavior.

- [ ] **Step 3: Implement fixtures and docs**

Add the E2E fixture helpers and any test-only address-family gate. Update README/CHANGELOG/feature overview with exact behavior, default, rollback warning, and the removal of redundant password prompts. Update examples only if needed.

- [ ] **Step 4: Run full verification**

Run:
```bash
go test ./... -count=1
go test -race ./... -count=1
go vet ./...
staticcheck ./...
node --check internal/webui/clientui/static/app.js
node --check internal/webui/serverui/static/app.js
scripts/openwrt/test-ipk.sh
scripts/openwrt/test-luci-controller.sh
git diff --check
```
Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/e2e/exposure_test.go README.md README.zh-CN.md CHANGELOG.md CHANGELOG.zh-CN.md docs/feature-overview.zh-CN.md examples/server.yaml
git commit -m "test(e2e): verify mapping exposure and document controls"
```

---

## Execution Handoff

After the plan is approved, use `superpowers:subagent-driven-development` (recommended for this multi-surface change) or `superpowers:executing-plans`. Do not release or deploy until the whole-branch review and real-device checks pass.
