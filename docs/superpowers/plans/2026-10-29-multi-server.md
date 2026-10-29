# Multi-Server Client and Per-Server Proxy Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let one `hy2tun` client process connect to multiple independent servers, with independent mappings, credentials, reconnect loops, port hopping, and optional per-server SOCKS5/HTTP proxies controllable from the client web console.

**Architecture:** Add a `servers:` list to the client configuration while preserving the legacy single-server shape. A new client supervisor owns the shared web console, configuration persistence, and one `Client` worker per server. Existing connection/control/mapping code remains the worker implementation. A proxy apply path binds per-server listeners and the supervisor validates cross-server/web address conflicts. The web console resolves a selected server from the `server` query parameter and adds a server list plus per-server proxy controls.

**Tech Stack:** Go 1.26, `gopkg.in/yaml.v3`, existing Hysteria 2 core/extras, `net/http`, `testify/require`, embedded HTML/CSS/JS.

**Spec:** `docs/superpowers/specs/2026-10-29-multi-server-design.md`

## Global Constraints

- Preserve legacy single-server `client.yaml` behavior and YAML shape; do not auto-convert it to `servers:` on startup.
- A multi-server config has top-level `web` and `log` only; legacy per-server top-level fields are rejected when `servers` is non-empty.
- Every server has an independent client ID, name, revision, mappings, credentials generation, TLS/obfs/transport/bandwidth/control config, and reconnect loop.
- Per-server proxy defaults to disabled; only explicit `proxy.enabled: true` starts listeners.
- Proxy listener addresses must be valid `host:port` with port 1-65535; port 0 is rejected.
- No proxy listener may conflict with another enabled proxy listener or `web.listen`; exact duplicates are rejected at validation and OS bind conflicts are caught at apply/startup.
- A startup proxy bind failure disables only that server's proxy and surfaces an error; mappings and other servers continue.
- Hot proxy changes must not leave a half-open listener or half-written config.
- The client web console remains authenticated; mutations still require CSRF header and same-origin `Origin`.
- Do not change `hy2tund` protocol or server-side data plane.
- Run `go test ./...`, `go test -race ./...`, `go vet ./...`, and `make build` before completion.

## Review Focus

- Legacy config with no `client.id` still starts and persists an ID without changing its YAML shape.
- A server with proxy enabled but no addresses is rejected; a disabled proxy never binds.
- Two servers cannot bind the same proxy port; wildcard/specific conflicts are caught by bind preflight.
- A failed proxy bind at startup or hot apply leaves the previous listener set and config intact.
- Old single-server API endpoints return 404 when a multi-server config has no `default`, and never mutate the wrong server.

---

### Task 1: Multi-server config model and legacy compatibility

**Files:**
- Create: `internal/config/servers.go`
- Modify: `internal/config/client.go`
- Modify: `internal/config/config.go`
- Modify: `internal/config/check.go`
- Test: `internal/config/config_test.go`
- Test: `internal/config/servers_test.go`

**Interfaces:**
- Produces: `type ClientServerConfig struct { Name, Server, Auth string; TLS ClientTLSConfig; Obfs ObfsConfig; Bandwidth BandwidthConfig; Transport *ClientTransportConfig; Control ControlConfig; Client ClientIdentity; Credentials CredentialsConfig; Mappings []Mapping; Proxy ProxyConfig }`
- Produces: `func (c *ClientConfig) IsMulti() bool`
- Produces: `func (c *ClientConfig) ServerConfigs() []ClientServerConfig`
- Produces: `func (s ClientServerConfig) ToClientConfig(web WebConfig, log LogConfig, path string) *ClientConfig`
- Produces: `func (c *ClientConfig) Validate() error` (extended)
- Produces: `func (p ProxyConfig) Active() bool` (enabled or legacy address inference handled by config)
- Produces: `func validateProxyListen(field, value string) error`

- [ ] **Step 1: Write failing config tests**

Add table tests covering:
- `servers` with two entries parses and validates; names unique.
- `servers` plus top-level `server`/`auth`/`control` is rejected.
- legacy config still validates and `ServerConfigs()` returns one `default` server.
- empty server name, duplicate names, invalid proxy port 0, enabled proxy without addresses are rejected.
- legacy proxy addresses without `enabled` are active; a new server proxy with addresses but `enabled: false` is inactive.

Run: `go test ./internal/config -run 'TestClientMulti|TestClientLegacy' -count=1`
Expected: FAIL with undefined fields/methods.

- [ ] **Step 2: Add `ClientServerConfig` and `Servers` field**

Add `Servers []ClientServerConfig` to `ClientConfig`. Add `ClientServerConfig` with the fields from the interface. Add `ProxyConfig.Enabled bool` with `yaml:"enabled,omitempty"`.

- [ ] **Step 3: Implement validation and conversion**

Refactor the existing single-server validation body into `validateSingleServer()`. `Validate()` dispatches to multi validation when `Servers` is non-empty; otherwise validates legacy fields and infers legacy proxy enabled when addresses are present. Multi validation rejects legacy fields, validates each server with the same field rules, rejects duplicate names, and validates proxy addresses/conflicts.

`ServerConfigs()` returns a copy; for legacy it builds one `ClientServerConfig{Name:"default"}` from top-level fields and sets proxy active when addresses exist.

`ToClientConfig` maps all independent fields into a single-server `ClientConfig`, sets `Web` and `Log`, and clears `Servers`.

- [ ] **Step 4: Update `CheckClient` for each server**

Validate TLS CA files for every server, web TLS/password once, log path once. Use `ServerConfigs()`.

- [ ] **Step 5: Run tests**

Run: `go test ./internal/config -count=1`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/config
git commit -m "feat(config): add independent multi-server client config"
```

---

### Task 2: Per-server proxy configuration and conflict validation

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/servers.go`
- Test: `internal/config/servers_test.go`

**Interfaces:**
- Produces: `func (p ProxyConfig) Active() bool`
- Produces: `func proxyListenKey(value string) (string, error)` normalized `host:port`
- Produces: `func validateProxyConflicts(servers []ClientServerConfig, webListen string) error`

- [ ] **Step 1: Write failing tests**

Cover:
- enabled proxy with neither SOCKS5 nor HTTP -> error
- enabled proxy with port 0 -> error
- duplicate SOCKS5/HTTP across servers -> error
- same port on different specific loopback IPs -> allowed by config validation
- proxy address equal to `web.listen` -> error
- disabled proxy addresses do not conflict.

Run: `go test ./internal/config -run TestProxyConflict -count=1`
Expected: FAIL.

- [ ] **Step 2: Implement `Active()` and validation**

`Active()` returns `Enabled`. Legacy conversion sets `Enabled` when legacy addresses exist. Validate addresses with `validateHostPort(..., false, false)`, normalize with `net.SplitHostPort`, and compare normalized `host:port` keys. Compare against `web.listen` only when web listen is non-empty. Do not attempt OS binds here.

- [ ] **Step 3: Run tests and commit**

Run: `go test ./internal/config -count=1`
Expected: PASS.

```bash
git add internal/config
git commit -m "feat(config): validate per-server proxy listeners"
```

---

### Task 3: Worker config persistence and proxy apply

**Files:**
- Modify: `internal/client/client.go`
- Modify: `internal/client/configsync.go`
- Modify: `internal/client/control.go`
- Create: `internal/client/proxy_apply.go`
- Test: `internal/client/proxy_apply_test.go`
- Test: `internal/client/configsync_test.go`

**Interfaces:**
- Produces: `func (c *Client) SetPersist(func(*config.ClientConfig) error)` (or an internal `persist` field set by supervisor)
- Produces: `func (c *Client) ApplyProxy(cfg config.ProxyConfig) error`
- Produces: `func (c *Client) ProxyState() ProxyState`
- Produces: `type ProxyState struct { Enabled bool; SOCKS5, HTTP, Error string }`
- Modifies: all `writeConfigAtomic(c.configPath, next)` calls to use `c.writeConfig(next)`.

- [ ] **Step 1: Write failing tests**

Use a worker config with no web listener. Test:
- `ApplyProxy` with disabled config starts no listeners.
- `ApplyProxy` enabled binds the configured addresses and `ProxyState` reports them.
- invalid/occupied address returns error and leaves prior state.
- changing addresses closes old listeners and binds new.
- worker persistence callback receives the edited single-server config.

Run: `go test ./internal/client -run 'TestApplyProxy|TestWorkerPersist' -count=1`
Expected: FAIL.

- [ ] **Step 2: Add persistence callback**

Add `persist func(*config.ClientConfig) error` to `Client`. Add `writeConfig(next *config.ClientConfig) error` that calls `persist` when set, otherwise `config.SaveClient(c.configPath,next)`. Replace direct calls.

- [ ] **Step 3: Add proxy apply**

Add a `proxyMu sync.Mutex`. Extract binding into `bindProxies(cfg) ([]*proxy.Server,error)`. `ApplyProxy` validates `cfg.Active()`, closes current listeners, binds the new set, starts accept loops if the worker is started, and stores the new config under `configMu`. On bind error, attempt to restore the previous proxy set and return the error. `ProxyState` returns configured addresses and the last bind error.

- [ ] **Step 4: Run tests and commit**

Run: `go test ./internal/client -run 'TestApplyProxy|TestWorkerPersist' -count=1`
Expected: PASS.

```bash
git add internal/client
git commit -m "feat(client): support per-worker proxy apply and config persistence"
```

---

### Task 4: Client supervisor lifecycle

**Files:**
- Create: `internal/client/supervisor.go`
- Create: `internal/client/supervisor_test.go`
- Modify: `cmd/hy2tun/main.go`
- Modify: `internal/config/check.go` if needed

**Interfaces:**
- Produces: `type Supervisor struct`
- Produces: `func NewSupervisor(cfg *config.ClientConfig, log *slog.Logger) (*Supervisor,error)`
- Produces: `func (s *Supervisor) Start(ctx context.Context) error`
- Produces: `func (s *Supervisor) Close() error`
- Produces: `func (s *Supervisor) Servers() []ServerSummary` (clientui-facing)
- Produces: `func (s *Supervisor) Server(name string) (clientui.API, bool)`
- Produces: `func (s *Supervisor) UpdateProxy(name string, cfg config.ProxyConfig) error`

- [ ] **Step 1: Write failing supervisor tests**

Use two in-memory configs and a test server. Assert:
- two workers are created with distinct client IDs and names
- one worker connection failure does not stop the other
- server-specific mapping edits persist into the correct config block
- legacy config creates one `default` worker
- generated client IDs are persisted without changing legacy YAML shape.

Run: `go test ./internal/client -run TestSupervisor -count=1`
Expected: FAIL.

- [ ] **Step 2: Implement supervisor**

`NewSupervisor` validates config, generates missing per-server IDs, creates worker configs with web disabled and proxies disabled, creates workers, sets persistence callbacks, and binds startup proxies per server (failure marks that proxy disabled/error but does not fail the worker). Create the shared `clientui.Server` using the supervisor as API.

`Start` starts the web console and each worker in its own goroutine; it returns when the context is canceled or all workers stop. `Close` closes workers then web. Supervisor config persistence locks a supervisor config mutex, clones the full multi-server config, updates the target server block, validates, saves atomically, and only then installs the new global config.

- [ ] **Step 3: Switch CLI to supervisor**

`cmd/hy2tun/main.go` loads config, ensures web password, creates `client.NewSupervisor`, persists generated IDs (legacy shape preserved), logs each server, starts, and closes. Keep `--check-config`.

- [ ] **Step 4: Run tests and commit**

Run: `go test ./internal/client ./internal/config -count=1`
Expected: PASS.

```bash
git add internal/client cmd/hy2tun internal/config
git commit -m "feat(client): run independent workers under a multi-server supervisor"
```

---

### Task 5: Supervisor proxy conflict and hot-apply transactions

**Files:**
- Create: `internal/client/proxy_manager.go`
- Test: `internal/client/proxy_manager_test.go`
- Modify: `internal/client/supervisor.go`

**Interfaces:**
- Produces: `func (s *Supervisor) UpdateProxy(name string, cfg config.ProxyConfig) error`
- Produces: `type ProxyConflictError struct { Address string }`
- Produces: `func (s *Supervisor) proxyAddresses(exclude string) map[string]string`

- [ ] **Step 1: Write failing tests**

Cover:
- two servers cannot enable the same address
- proxy cannot use `web.listen`
- enabling one server does not disable another
- address change that fails bind keeps old proxy and config
- config save failure keeps old proxy
- disabling one server leaves other proxies running
- startup bind failure only disables that server's proxy.

Run: `go test ./internal/client -run TestProxyManager -count=1`
Expected: FAIL.

- [ ] **Step 2: Implement supervisor update transaction**

Validate the target server and addresses. Check normalized conflicts against every other active server and `web.listen`. Persist a candidate full config only after preflight; if bind/apply fails, restore the previous config and proxy. Call the target worker's `ApplyProxy`. On success, update supervisor config and return. Use a supervisor proxy mutex to serialize updates.

- [ ] **Step 3: Run tests and commit**

Run: `go test ./internal/client -run TestProxyManager -count=1`
Expected: PASS.

```bash
git add internal/client
git commit -m "feat(client): serialize per-server proxy changes"
```

---

### Task 6: Client web server selection and server list

**Files:**
- Modify: `internal/webui/clientui/handlers.go`
- Modify: `internal/webui/clientui/views.go`
- Create: `internal/webui/clientui/handlers_servers.go`
- Create: `internal/webui/clientui/templates/servers.html`
- Modify: `internal/webui/clientui/templates/layout.html`
- Modify: `internal/webui/clientui/static/app.js`
- Test: `internal/webui/clientui/handlers_test.go`

**Interfaces:**
- Produces: `type MultiAPI interface { Servers() []ServerSummary; Server(name string) (API, bool); UpdateProxy(name string, config.ProxyConfig) error }`
- Produces: `type ServerSummary struct { Name, Address string; Online bool; Mappings int; ProxyEnabled bool }`
- Produces: `func (s *Server) apiFor(r *http.Request) (API, bool)` resolving `?server=`.
- Produces: `GET /servers`, `GET /api/servers`.

- [ ] **Step 1: Write failing handler tests**

Use a fake multi API with two servers. Assert:
- `/servers` lists both
- `/?server=us` renders the US status/mappings
- unknown `?server` returns 404
- legacy API maps to `default`
- multi config without `default` and no `server` returns 404
- mutations use the selected server (fake records target).

Run: `go test ./internal/webui/clientui -run 'TestClientServers|TestClientServerSelection' -count=1`
Expected: FAIL.

- [ ] **Step 2: Add multi API and selection helper**

Keep the existing `API` for a single server. Add `MultiAPI` as an optional interface. `Server` stores `multi MultiAPI` when the API implements it. `apiFor` uses `server` query, defaults to `default` or the first server for legacy, and returns false for unknown names.

- [ ] **Step 3: Route server-scoped handlers**

Replace `s.api` in status/mappings/settings/name/credentials/mapping handlers with the selected API. Keep `/api/settings/web`, `/api/logout`, `/api/logs`, `/api/events`, `/metrics`, and `/healthz` process-level. Add `/servers` and `/api/servers`. Add a server selector link in the layout.

- [ ] **Step 4: Make the web JS preserve the selected server**

Add a helper that reads `server` from the page URL and appends it to server-scoped fetches/EventSource. Use it for status, mappings, settings, logs only where server-scoped, and settings mutations. Add a server list link.

- [ ] **Step 5: Run tests and commit**

Run: `go test ./internal/webui/clientui -count=1`
Expected: PASS.

```bash
git add internal/webui/clientui
git commit -m "feat(web): add server selection and server list"
```

---

### Task 7: Per-server proxy web API and settings card

**Files:**
- Modify: `internal/webui/clientui/handlers.go`
- Modify: `internal/webui/clientui/handlers_settings.go`
- Modify: `internal/webui/clientui/views.go`
- Modify: `internal/webui/clientui/templates/index.html`
- Modify: `internal/webui/clientui/static/app.js`
- Test: `internal/webui/clientui/handlers_test.go`

**Interfaces:**
- Produces: `type ProxyAPI interface { Proxy() ProxyView; UpdateProxy(config.ProxyConfig) error }`
- Produces: `type ProxyView struct { Enabled bool; SOCKS5, HTTP, Error string }`
- Produces: `GET /api/settings` includes `proxy`
- Produces: `PUT /api/settings/proxy` and `PUT /api/servers/{name}/proxy`.

- [ ] **Step 1: Write failing tests**

Assert:
- settings JSON includes proxy state
- update proxy on selected server calls the selected fake and persists
- invalid address returns 400
- conflict returns 409 with `proxy_conflict`
- CSRF is required
- disabling proxy succeeds without password re-entry
- proxy card appears in settings HTML.

Run: `go test ./internal/webui/clientui -run TestClientProxy -count=1`
Expected: FAIL.

- [ ] **Step 2: Implement proxy API**

Add `ProxyView` to `SettingsView`. Add `Proxy()`/`UpdateProxy` optional interface. Add handler for `PUT /api/settings/proxy` using `apiFor`; add `/api/servers/{name}/proxy` delegating to `MultiAPI.UpdateProxy`. Map validation/conflict/bind errors to 400/409/500 with stable codes. No current-password confirmation.

- [ ] **Step 3: Add settings card and JS**

Add enabled checkbox, SOCKS5/HTTP inputs, save button, current state/error. Submit to the selected server's proxy endpoint with CSRF headers. Reload settings/status on success. Default unchecked.

- [ ] **Step 4: Run tests and commit**

Run: `go test ./internal/webui/clientui -count=1`
Expected: PASS.

```bash
git add internal/webui/clientui
git commit -m "feat(web): control per-server proxies"
```

---

### Task 8: End-to-end coverage, docs, and release notes

**Files:**
- Create: `internal/e2e/multi_server_test.go`
- Modify: `README.md`
- Modify: `README.zh-CN.md`
- Modify: `examples/client.yaml`
- Create: `examples/client-multi.yaml`
- Modify: `CHANGELOG.md`

- [ ] **Step 1: Write failing E2E test**

Start two test servers, configure two client workers with different mappings (same local target, different public ports), and assert both mappings work. Stop one server and assert the other mapping still works. Enable one proxy and verify it binds; disable it and verify the port is released.

Run: `go test ./internal/e2e -run TestMultiServer -count=1`
Expected: FAIL.

- [ ] **Step 2: Implement test harness fixes**

Adjust supervisor/worker test hooks only as needed; do not add test-only production methods.

- [ ] **Step 3: Update docs and examples**

Document:
- `servers:` schema and legacy compatibility
- independent client IDs/credentials/mappings
- per-server proxy default off and Web controls
- port conflict rules
- old API `default`/404 behavior
- migration notes and no auto-rewrite.

- [ ] **Step 4: Full verification**

Run:

```bash
go test ./... -count=1
go test -race ./... -count=1
go vet ./...
make build
git diff --check
```

Expected: all pass.

- [ ] **Step 5: Commit**

```bash
git add internal/e2e README.md README.zh-CN.md examples CHANGELOG.md
git commit -m "test: cover multi-server mappings and proxy controls"
```
