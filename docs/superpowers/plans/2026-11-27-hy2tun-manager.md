# hy2tun-manager Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build a single-administrator, multi-server control plane that aggregates existing `hy2tund` consoles and safely writes the server/client operations exposed by their current APIs.

**Architecture:** A new `hy2tun-manager` binary in the existing Go module owns a JSON-backed server inventory, encrypted upstream administrator credentials, a server-rendered Web UI, and one HTTP adapter per `hy2tund` endpoint. Existing `hy2tund`/`hy2tun` binaries and data-plane behavior remain unchanged; the manager only calls the server's existing session/CSRF APIs.

**Tech Stack:** Go 1.26, `html/template`, `net/http`, `gopkg.in/yaml.v3`, AES-256-GCM, bcrypt, existing `internal/view` and `internal/config` types where their JSON shape is stable

**Spec:** `docs/superpowers/specs/2026-11-27-hy2tun-manager-design.md`

## Global Constraints

- Manager-only project: do not modify `cmd/hy2tun`, `cmd/hy2tund`, `internal/server`, `internal/client`, `internal/control`, or the data plane.
- Do not add/remove client-local `servers:` entries or edit client-local Web/proxy/failover/TLS/log configuration.
- Do not push mappings to offline clients; offline exposure changes are allowed because exposure is server-side state.
- Do not embed or iframe the native consoles; use their JSON APIs.
- Single manager administrator; no roles or multi-user accounts.
- Upstream administrator passwords are encrypted at rest with AES-256-GCM; the key is in `manager.key`, never in state or logs.
- Mutations require a manager session, `X-Requested-With: hy2tun`, and a same-origin `Origin` header.
- Audit append failure blocks mutations (fail closed).
- Mapping edits use the full mapping list plus fetched base revision; never auto-retry a revision conflict.
- No write queue and no cross-server transaction; failures surface to the operator.
- Manager release targets are linux/amd64 and linux/arm64 only.
- Base URL/path reverse proxying is the operator's responsibility; manager supports a configured `base_path` and a dedicated cookie name.
- The manager must be able to start against servers from v1.7.0 onward; optional endpoints that return `501` are marked unsupported, and v1.7.4 is the recommended baseline.

## Review Focus

- A reverse-proxied upstream URL with a path prefix must retain the prefix when joining `/api/...` paths.
- A concurrent refresh and mapping write must serialize per server so cookie/login state and revisions cannot race.
- A new enabled `local` mapping must never bind publicly: use the existing `exposures` field on the mapping push.
- Audit disk failure must prevent the mutation, not merely log a warning.
- Same-manager-host cookie collision must be impossible: use `hy2tun_manager_session`, never `hy2tun_session`.
- Offline client mapping edits must be disabled, but exposure remains server-side editable.

---

### Task 1: Manager config, encrypted state, and audit log

**Files:**
- Create: `cmd/hy2tun-manager/main.go`
- Create: `internal/manager/config.go`
- Create: `internal/manager/state.go`
- Create: `internal/manager/secret.go`
- Create: `internal/manager/audit.go`
- Test: `internal/manager/config_test.go`
- Test: `internal/manager/state_test.go`
- Test: `internal/manager/secret_test.go`
- Test: `internal/manager/audit_test.go`

**Interfaces:**
- `LoadConfig(path string) (Config, error)` parses YAML and defaults `listen`, `base_path`, `refresh_interval`, and `trust_proxy`.
- `OpenStore(statePath, keyPath string) (*Store, error)` loads or creates state and the encryption key.
- `Store.Update(func(*State) error) error` atomically persists a mutation under a mutex.
- `Store.State() State` returns a deep-safe copy for reads.
- `Store.SetServerPassword(id string, plaintext []byte) error` encrypts a server password and persists it.
- `Store.ServerPassword(id string) ([]byte, error)` decrypts only for an in-memory upstream session.
- `EncryptSecret(key, plaintext []byte) (string, error)` and `DecryptSecret(key []byte, encoded string) ([]byte, error)` use AES-256-GCM and a fresh nonce.
- `AuditLog.Append(AuditEntry) error` appends one JSON line and fsyncs; `AuditEntry` records time, action, server ID/name, target, result, code, and message.
- `State` contains `Admin` and `Servers []ServerRecord`.
- `ServerRecord` contains `ID`, `Name`, `BaseURL`, `UpstreamUsername`, `UpstreamPasswordEnc`, `TLSSkipVerify`, `CreatedAt`, `UpdatedAt`.

- [ ] **Step 1: Write the failing tests**

Write `internal/manager/config_test.go`:

```go
func TestLoadConfigDefaultsAndValidation(t *testing.T) {
    dir := t.TempDir()
    path := filepath.Join(dir, "manager.yaml")
    require.NoError(t, os.WriteFile(path, []byte("listen: 127.0.0.1:7600\n"), 0o600))
    cfg, err := LoadConfig(path)
    require.NoError(t, err)
    require.Equal(t, "/", cfg.BasePath)
    require.Equal(t, 30*time.Second, cfg.RefreshInterval)
    require.False(t, cfg.TrustProxy)

    require.NoError(t, os.WriteFile(path, []byte("listen: \"\"\n"), 0o600))
    _, err = LoadConfig(path)
    require.ErrorContains(t, err, "listen")
}
```

Write `internal/manager/state_test.go`:

```go
func TestStoreRoundTripsEncryptedServerAndRejectsTamper(t *testing.T) {
    dir := t.TempDir()
    store, err := OpenStore(filepath.Join(dir, "state.json"), filepath.Join(dir, "manager.key"))
    require.NoError(t, err)
    require.NoError(t, store.Update(func(s *State) error {
        s.Servers = append(s.Servers, ServerRecord{
            ID: "srv_1", Name: "us", BaseURL: "https://us.example/hy2tund",
            UpstreamUsername: "admin",
        })
        return nil
    }))
    require.Equal(t, "us", store.State().Servers[0].Name)
    require.Equal(t, 0o600, fileMode(t, filepath.Join(dir, "state.json")))
    require.Equal(t, 0o600, fileMode(t, filepath.Join(dir, "manager.key")))

    // A different key must not decrypt existing records.
    other, err := OpenStore(filepath.Join(dir, "other.json"), filepath.Join(dir, "other.key"))
    require.NoError(t, err)
    require.ErrorContains(t, other.SetServerPassword("srv_1", []byte("pw")), "unknown server")
}
```

Write `internal/manager/secret_test.go`:

```go
func TestEncryptDecryptSecretAndTamper(t *testing.T) {
    key := []byte("0123456789abcdef0123456789abcdef")
    encoded, err := EncryptSecret(key, []byte("upstream-password"))
    require.NoError(t, err)
    plain, err := DecryptSecret(key, encoded)
    require.NoError(t, err)
    require.Equal(t, "upstream-password", string(plain))

    encoded = encoded[:len(encoded)-1] + "x"
    _, err = DecryptSecret(key, encoded)
    require.Error(t, err)
}
```

Write `internal/manager/audit_test.go`:

```go
func TestAuditAppendsJSONLineAndFailsClosed(t *testing.T) {
    dir := t.TempDir()
    log := NewAuditLog(filepath.Join(dir, "audit.jsonl"))
    require.NoError(t, log.Append(AuditEntry{Action: "mapping.update", ServerName: "us", Target: "m1", Result: "ok"}))
    data, err := os.ReadFile(filepath.Join(dir, "audit.jsonl"))
    require.NoError(t, err)
    require.Contains(t, string(data), `"action":"mapping.update"`)
    require.NoError(t, log.Append(AuditEntry{Action: "rotate", ServerName: "us", Result: "error", Code: "invalid_credentials"}))
    require.Error(t, NewAuditLog(filepath.Join(dir, "missing", "audit.jsonl")).Append(AuditEntry{Action: "x"}))
}
```

- [ ] **Step 2: Run the tests to capture RED**

Run:

```sh
go test ./internal/manager -count=1
```

Expected: FAIL with undefined `LoadConfig`, `OpenStore`, `EncryptSecret`, and `NewAuditLog`.

- [ ] **Step 3: Implement config and state**

Implement `LoadConfig` with YAML fields:

```yaml
listen: "127.0.0.1:7600"
base_path: "/"
state_file: "/var/lib/hy2tun-manager/state.json"
key_file: "/var/lib/hy2tun-manager/manager.key"
audit_file: "/var/lib/hy2tun-manager/audit.jsonl"
refresh_interval: "30s"
trust_proxy: false
```

Validate non-empty `listen`, `state_file`, `key_file`, `audit_file`; normalize `base_path` to start/end with `/`; require `refresh_interval >= 1s`.

Implement `OpenStore`:

- If key file is missing, generate 32 random bytes and write mode `0600`.
- If state file is missing, initialize admin with username `admin` and a generated password hash; print the generated password once to stderr.
- Otherwise load and validate state without silently resetting credentials.
- Encrypt/decrypt upstream passwords with `EncryptSecret`/`DecryptSecret`.
- `Update` holds a mutex, mutates a copy, writes a temp file with mode `0600`, `fsync`, and `os.Rename`.
- Corrupt state or an undecryptable key returns a startup error; no automatic reset.

Implement `AuditLog` with an `O_APPEND|O_CREATE|O_WRONLY` file mode `0600`, one JSON object per line, and `Sync` after each append.

- [ ] **Step 4: Wire a minimal command and run GREEN**

`cmd/hy2tun-manager/main.go` must parse `-config` (default `/etc/hy2tun/manager.yaml`), call `LoadConfig`, `OpenStore`, `NewAuditLog`, and exit after printing a startup line. The full HTTP server comes in Task 2.

Run:

```sh
go test ./internal/manager -count=1
go build ./cmd/hy2tun-manager
```

Expected: PASS and build exit 0.

- [ ] **Step 5: Commit**

```sh
git add cmd/hy2tun-manager internal/manager/config.go internal/manager/state.go internal/manager/secret.go internal/manager/audit.go internal/manager/*_test.go
git commit -m "feat(manager): add config, encrypted state, and audit log"
```

---

### Task 2: Manager login, session, and CSRF-protected Web shell

**Files:**
- Create: `internal/manager/session.go`
- Create: `internal/manager/login.go`
- Create: `internal/manager/web.go`
- Create: `internal/manager/templates/login.html`
- Create: `internal/manager/templates/layout.html`
- Create: `internal/manager/templates/overview.html`
- Create: `internal/manager/static/app.css`
- Create: `internal/manager/static/app.js`
- Test: `internal/manager/session_test.go`
- Test: `internal/manager/login_test.go`

**Interfaces:**
- `NewSessionManager(secret []byte, ttl time.Duration) (*SessionManager, error)`
- `SessionManager.Issue(w http.ResponseWriter, r *http.Request, username string) error`
- `SessionManager.Valid(r *http.Request) bool`
- `SessionManager.Clear(w http.ResponseWriter, r *http.Request)`
- `NewLoginHandler(sessions *SessionManager, limiter *shared.LoginLimiter, username string, passwordHash string) *LoginHandler`
- `NewWebServer(store *Store, audit *AuditLog, inventory *Inventory, cfg Config, log *slog.Logger) (*WebServer, error)` creates the manager HTTP handler. Task 2 may pass a nil inventory for the shell; Task 4 supplies it.
- The manager cookie is `hy2tun_manager_session`; it must never use `hy2tun_session`.

- [ ] **Step 1: Write failing tests**

`session_test.go`:

```go
func TestManagerSessionUsesDistinctCookieAndRejectsTamper(t *testing.T) {
    sessions, err := NewSessionManager([]byte("0123456789abcdef0123456789abcdef"), time.Hour)
    require.NoError(t, err)
    req := httptest.NewRequest(http.MethodGet, "/", nil)
    rr := httptest.NewRecorder()
    require.NoError(t, sessions.Issue(rr, req, "admin"))
    cookie := rr.Result().Cookies()[0]
    require.Equal(t, "hy2tun_manager_session", cookie.Name)
    require.True(t, cookie.HttpOnly)
    req.AddCookie(cookie)
    require.True(t, sessions.Valid(req))
    cookie.Value += "x"
    req2 := httptest.NewRequest(http.MethodGet, "/", nil)
    req2.AddCookie(cookie)
    require.False(t, sessions.Valid(req2))
}
```

`login_test.go`:

```go
func TestLoginAndCrossOriginMutationRejected(t *testing.T) {
    store := testStore(t) // creates admin/admin hash with test password
    web, err := NewWebServer(store, testAudit(t), nil, testConfig(), slog.Default())
    require.NoError(t, err)
    ts := httptest.NewServer(web.Handler())
    defer ts.Close()

    resp, err := ts.Client().PostForm(ts.URL+"/api/login", url.Values{
        "username": {"admin"}, "password": {"pw"},
    })
    require.NoError(t, err)
    require.Equal(t, http.StatusNoContent, resp.StatusCode)
    cookie := resp.Cookies()[0]

    req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/servers", strings.NewReader(`{}`))
    req.AddCookie(cookie)
    req.Header.Set("X-Requested-With", "hy2tun")
    req.Header.Set("Origin", "http://evil.example")
    resp, err = ts.Client().Do(req)
    require.NoError(t, err)
    require.Equal(t, http.StatusForbidden, resp.StatusCode)
}
```

- [ ] **Step 2: Run RED**

Run `go test ./internal/manager -count=1`; expect undefined session/login/server types.

- [ ] **Step 3: Implement session, login, and shell**

- Use HMAC-SHA256 over a random session ID and username, base64url encoded, with an in-memory revocation set and TTL.
- Cookie: `hy2tun_manager_session`, `Path=/`, `HttpOnly`, `SameSite=Lax`, `Secure` when the request is TLS or trusted `X-Forwarded-Proto: https`.
- Login form accepts form or JSON `username`, `password`; use `shared.NewLoginLimiter(5, time.Minute, time.Minute)` and bcrypt.
- Mount `POST /api/login`, `POST /api/logout`, `GET /login`, and an authenticated `GET /` overview shell.
- Use `shared.RequireCSRF` for manager mutations. Set the manager's own CSRF header in the rendered page and JS; the manager must not require an external token.
- Templates use an embedded `templates/*.html` and `static/*`; the overview shell shows the manager title and a placeholder inventory panel. It must include the manager's own login/logout controls and no native console iframe.

- [ ] **Step 4: Run GREEN**

```sh
go test ./internal/manager -count=1
go build ./cmd/hy2tun-manager
```

Expected: PASS; build succeeds.

- [ ] **Step 5: Commit**

```sh
git add internal/manager cmd/hy2tun-manager
git commit -m "feat(manager): add single-admin session and web shell"
```

---

### Task 3: Upstream server client and API adapter

**Files:**
- Create: `internal/manager/upstream/client.go`
- Create: `internal/manager/upstream/types.go`
- Create: `internal/manager/upstream/errors.go`
- Test: `internal/manager/upstream/client_test.go`

**Interfaces:**
- `NewClient(ServerRecord, []byte) (*Client, error)` receives decrypted upstream password bytes.
- `Client.Login(ctx context.Context) error`
- `Client.Overview(ctx) (Overview, error)`
- `Client.Clients(ctx) ([]ClientView, error)`
- `Client.Client(ctx, id string) (ClientView, error)`
- `Client.Settings(ctx) (Settings, error)`
- `Client.PushMappings(ctx, clientID string, baseRevision int64, mappings []config.Mapping, exposures map[string]view.Exposure) error`
- `Client.SetExposure(ctx, clientID, mappingID string, exposure view.Exposure) error`
- `Client.UpdateMode(ctx, mode config.TransportMode) error`
- `Client.UpdatePorts(ctx, allow, deny []string, autoRange string, offlineReserve time.Duration) error`
- `Client.UpdateWebAdmin(ctx, username, currentPassword, newPassword string) error`
- `Client.SetTrustProxy(ctx, trust bool) error`
- `Client.RotateCredentials(ctx, auth, controlToken, syncMode string) (RotateResult, error)`
- `Client.RevokeCredentials(ctx) error`
- `Client.CloseIdleConnections()`
- `APIError` carries `Status`, `Code`, `Message`, and optional `RetryAfter`.

**Upstream URL rules:** `base_url` may include a path; API paths are joined below it. The `Origin` header is exactly the base URL origin (scheme + host, no path).

- [ ] **Step 1: Write failing adapter tests**

Use a fake `httptest.Server` with:
- `POST /api/login` returning `204` and a `hy2tun_session` cookie;
- `GET /api/overview`, `/api/clients`, `/api/clients/{id}`, `/api/settings`;
- mutation endpoints requiring `X-Requested-With: hy2tun` and `Origin` matching `ts.URL`, returning `403` otherwise;
- a first `/api/clients` request returning `401`, second returning `200`;
- a mapping push returning `409` with `code: "conflict"` and a separate case for `code: "client_offline"`;
- a local-mapping push test asserting `exposures` is present in the JSON body;
- a base URL with a `/hy2tund` path prefix and a test asserting requests arrive at `/hy2tund/api/...`.

Tests must assert:
- login cookies are retained;
- one re-login occurs after 401 and no infinite loop;
- `PushMappings` sends the full mapping list and `base_revision`;
- `SetExposure` sends `{"exposure":"local"}`;
- `UpdateWebAdmin` sends `current_password` and `new_password`;
- `APIError.Code` preserves the server's code.

- [ ] **Step 2: Run RED**

Run `go test ./internal/manager/upstream -count=1`; expect undefined client/types.

- [ ] **Step 3: Implement client**

Use `http.Client` with a per-client `cookiejar`, `Timeout: 30s`, and a TLS config from `TLSSkipVerify`. Implement login with form or JSON; retain cookies. For mutations set `Content-Type: application/json`, `X-Requested-With: hy2tun`, and `Origin: origin(baseURL)`. On 401, clear cookies, login once, and retry once. Decode JSON error `{error, code, results, ...}` into `APIError`.

Add `types.go` with minimal DTOs for the manager UI. Reuse `internal/config.Mapping` and `internal/view.Exposure` where their JSON shape is stable; do not import `internal/serverui` or `internal/server`.

- [ ] **Step 4: Run GREEN**

```sh
go test ./internal/manager/upstream -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit**

```sh
git add internal/manager/upstream
git commit -m "feat(manager): add upstream server API adapter"
```

---

### Task 4: Server inventory, refresh, add/test/remove, and fleet overview

**Files:**
- Create: `internal/manager/inventory.go`
- Create: `internal/manager/refresh.go`
- Create: `internal/manager/handlers_servers.go`
- Create: `internal/manager/views_servers.go`
- Create: `internal/manager/templates/servers.html`
- Create: `internal/manager/templates/server.html`
- Test: `internal/manager/inventory_test.go`
- Test: `internal/manager/handlers_servers_test.go`

**Interfaces:**
- `NewInventory(store *Store, audit *AuditLog, factory ClientFactory, log *slog.Logger) *Inventory`
- `type ClientFactory func(ServerRecord, []byte) (*upstream.Client, error)`; the production factory is `upstream.NewClient`.
- `Inventory.Add(ctx, AddServerRequest) (ServerView, error)` validates URL/name, tests login, encrypted password, persists.
- `Inventory.Update(ctx, id string, UpdateServerRequest) error`
- `Inventory.Remove(id string) error`
- `Inventory.Refresh(ctx, id string) (ServerView, error)`
- `Inventory.RefreshAll(ctx)`
- `Inventory.List() []ServerView`
- `Inventory.Get(id string) (ServerView, bool)`
- `ServerView` contains configured name/base URL, state (`online`, `auth_error`, `unreachable`, `unsupported`), version, mode, clients count, mappings count, last refresh, last error.

- [ ] **Step 1: Write failing tests**

`inventory_test.go`:

```go
func TestInventoryAddTestsLoginEncryptsPasswordAndRefreshes(t *testing.T) {
    upstream := newFakeUpstream(t) // overview: version v1.7.4, 2 clients
    inv := testInventory(t)
    view, err := inv.Add(context.Background(), AddServerRequest{
        Name: "us", BaseURL: upstream.URL, Username: "admin", Password: "upstream-pw",
    })
    require.NoError(t, err)
    require.Equal(t, "online", view.State)
    require.Equal(t, "v1.7.4", view.Version)
    require.Equal(t, 2, view.ClientCount)
    require.NotContains(t, storeRaw(t, inv), "upstream-pw")
}

func TestInventoryRejectsDuplicateNameAndUnreachableServer(t *testing.T) {
    // duplicate name must return a validation error;
    // a refused URL must produce state unreachable without saving a server.
}
```

`handlers_servers_test.go` tests the manager HTTP routes with an authenticated manager cookie:
- add server form/API returns 201/200 and a server row;
- test connection does not mutate existing credentials on failure;
- remove server requires confirmation (POST/DELETE) and writes audit;
- list page shows configured server even when upstream unreachable.

- [ ] **Step 2: Run RED**

`go test ./internal/manager -run 'Inventory|Server' -count=1`; expect undefined inventory/handler types.

- [ ] **Step 3: Implement inventory and server handlers**

- Add requires name, `http`/`https` base URL, username, password; test login + `GET /api/overview` before saving.
- Encrypt password with `EncryptSecret`; update `UpstreamPasswordEnc`.
- Refresh one server with a 15s context; one in-flight refresh per server via mutex/state.
- Background `RefreshAll` every configured interval; bounded concurrency, one request per server.
- On login failure, set `auth_error`; on transport failure, set `unreachable`; never remove inventory.
- Server list/overview page shows state, version, mode, client count, mapping count, last refresh, and last error.
- Add/edit forms are write-only for passwords; remove uses a confirmation page/button.

- [ ] **Step 4: Run GREEN**

```sh
go test ./internal/manager -run 'Inventory|Server' -count=1
go test ./internal/manager/upstream -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit**

```sh
git add internal/manager
git commit -m "feat(manager): aggregate server inventory and refresh"
```

---

### Task 5: Client detail and mapping editor

**Files:**
- Create: `internal/manager/handlers_clients.go`
- Create: `internal/manager/views_clients.go`
- Create: `internal/manager/templates/clients.html`
- Create: `internal/manager/templates/client.html`
- Test: `internal/manager/handlers_clients_test.go`

**Interfaces:**
- `GET /servers/{id}/clients` renders the selected server's client table.
- `GET /servers/{id}/clients/{clientID}` renders mapping detail.
- `PUT /api/servers/{id}/clients/{clientID}/mappings` accepts `{base_revision, mappings, exposures}` and forwards the same shape.
- `PUT /api/servers/{id}/clients/{clientID}/mappings/{mappingID}/exposure` accepts `{exposure}`.
- Mapping list updates are full replacements; the manager always fetches fresh client detail on page load and on conflict.

- [ ] **Step 1: Write failing tests**

```go
func TestClientMappingPageAndSuccessfulPush(t *testing.T) {
    // fake upstream has an online client with one mapping at revision 7.
    // authenticated GET /servers/srv/clients/c1 renders the mapping row.
    // PUT /api/.../mappings with base_revision 7 and the edited list reaches upstream.
    // response 200 and a refreshed view.
}

func TestClientMappingConflictAndOfflineRules(t *testing.T) {
    // upstream returns code conflict: manager returns 409 and does not retry.
    // upstream returns code client_offline: mapping edit controls are disabled in HTML.
    // offline exposure PUT is still allowed and reaches the upstream.
}

func TestConcurrentRefreshAndMappingWriteSerializePerServer(t *testing.T) {
    // start a blocking refresh response and a mapping PUT for the same server.
    // the second operation must not run until the first upstream request returns;
    // both complete without sharing a stale cookie or revision.
}
```

- [ ] **Step 2: Run RED**

`go test ./internal/manager -run 'Client|Mapping' -count=1`; expect undefined handlers.

- [ ] **Step 3: Implement clients and mapping handlers**

- Fetch client detail via upstream `Client` on every page/action.
- Render mapping rows with type, remote port, local address, enabled state, note, exposure, and status.
- Add/edit/delete/enable forms use a confirmation for deletion.
- Send local new mappings with the `exposures` field; rely on the server's three-step transaction.
- On `conflict`/`partial_apply`/`exposure_conflict`, discard the stale form and render the server's message plus a reload action.
- On `client_offline`, render read-only mapping rows and a clear notice; exposure remains available.
- Audit each successful/failed mapping mutation with server/client/mapping IDs and result code.

- [ ] **Step 4: Run GREEN**

```sh
go test ./internal/manager -run 'Client|Mapping' -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit**

```sh
git add internal/manager
git commit -m "feat(manager): add client mapping management"
```

---

### Task 6: Server settings and credential operations

**Files:**
- Create: `internal/manager/handlers_settings.go`
- Create: `internal/manager/views_settings.go`
- Create: `internal/manager/templates/server_settings.html`
- Test: `internal/manager/handlers_settings_test.go`

**Interfaces:**
- `PUT /api/servers/{id}/settings/mode` forwards `{mode}`.
- `PUT /api/servers/{id}/settings/ports` forwards `{allow, deny, auto_range, offline_reserve_seconds}`.
- `PUT /api/servers/{id}/settings/web` forwards `{username, current_password, new_password, confirm_password}`.
- `PUT /api/servers/{id}/settings/trust-proxy` forwards `{trust_proxy}`.
- `PUT /api/servers/{id}/settings/credentials` forwards `{auth, control_token, sync_mode}`.
- `POST /api/servers/{id}/settings/credentials/revoke` forwards an empty JSON body.
- Successful mutations return the refreshed upstream settings; failures preserve operator form input in the rendered error response.

- [ ] **Step 1: Write failing tests**

```go
func TestServerSettingsMutationsForwardAndAudit(t *testing.T) {
    // fake upstream records mode, ports, trust-proxy, and rotation bodies.
    // each manager mutation returns success and appends an audit entry.
}

func TestWebAdminPasswordUpdateUpdatesStoredSecret(t *testing.T) {
    // upstream accepts /api/settings/web with current password and new password.
    // manager re-logs in, stores the new encrypted upstream password,
    // and the old secret is no longer present.
}

func TestSettingsFailuresAreAuditedAndNoSecretLeak(t *testing.T) {
    // upstream returns 403 invalid_password for web/admin and 422 for rotation.
    // response HTML/JSON contains no upstream password or token;
    // audit entry records the error code.
}

func TestSettingsMutationIsBlockedWhenAuditAppendFails(t *testing.T) {
    // use an audit log whose append returns an error;
    // the handler returns an error and the fake upstream records zero mutations.
}
```

- [ ] **Step 2: Run RED**

`go test ./internal/manager -run 'Settings|Credential|WebAdmin' -count=1`; expect undefined handlers.

- [ ] **Step 3: Implement settings and credentials**

- Render tabs for mode, ports, Web administrator, trust-proxy, credentials.
- Web admin update sends the stored upstream password as `current_password`; after a 204/200, store the new upstream password and re-login.
- Credential rotate sends auth/control token/sync mode; render per-client results. Revoke requires explicit confirmation.
- Audit fail-closed: append the intended action (with no secrets) before calling upstream; if append fails, return an error and do not call upstream.
- Map upstream JSON code to operator-friendly messages; never render raw secrets.

- [ ] **Step 4: Run GREEN**

```sh
go test ./internal/manager -run 'Settings|Credential|WebAdmin' -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit**

```sh
git add internal/manager
git commit -m "feat(manager): manage server settings and credentials"
```

---

### Task 7: Command wiring, release artifact, systemd example, and whole-project verification

**Files:**
- Modify: `cmd/hy2tun-manager/main.go`
- Modify: `Makefile`
- Create: `scripts/release-manager.sh`
- Create: `scripts/systemd/hy2tun-manager.service`
- Create: `docs/hy2tun-manager.md` (operator setup and base URL/reverse-proxy notes)
- Test: `internal/manager/main_test.go` or `cmd/hy2tun-manager/main_test.go`

**Interfaces:**
- `make build` continues to build only `hy2tund` and `hy2tun`.
- `make build-manager` builds `bin/hy2tun-manager`.
- `scripts/release-manager.sh` builds linux/amd64 and linux/arm64 archives containing the manager binary, example config, systemd unit, and README.
- `hy2tun-manager -config <path>` starts the HTTP server; `-reset-admin` runs the password reset and exits.

- [ ] **Step 1: Write failing command and release tests**

- A config parsing test runs the manager with a temp YAML and asserts it starts listening on `127.0.0.1:0` and shuts down on SIGTERM.
- A `-reset-admin` test writes a state, runs the reset path with a new password, and verifies only the bcrypt hash changed and the old manager session is invalid.
- A shell test (or Makefile target test) asserts `scripts/release-manager.sh` produces two archives with names `hy2tun-manager_<version>_linux_amd64.tar.gz` and `...linux_arm64...`, and each archive contains `hy2tun-manager`, `manager.example.yaml`, `hy2tun-manager.service`, and `README.md`.

- [ ] **Step 2: Run RED**

```sh
go test ./cmd/hy2tun-manager ./internal/manager -count=1
VERSION=test ./scripts/release-manager.sh --dry-run
```

Expected: command/release tests fail before implementation.

- [ ] **Step 3: Implement wiring, release, and docs**

- Wire config, store, audit, inventory, and WebServer in `main.go`; handle `SIGTERM` with `http.Server.Shutdown`.
- Implement `-reset-admin` with a write-only new password flag or prompt; it must not reset on normal startup.
- Add `build-manager` target without changing existing `build`.
- Add `scripts/release-manager.sh` that uses `CGO_ENABLED=0`, `-trimpath -ldflags "-s -w -X hy2tun/internal/version.Version=$VERSION..."`, only linux/amd64 and linux/arm64, and a `--dry-run` that prints planned outputs without building.
- Add an example manager config, systemd unit, and operator docs covering reverse proxy, base URL, and credential custody.
- Do not modify `scripts/release.sh` or the existing `hy2tund`/`hy2tun` archives.

- [ ] **Step 4: Run full verification**

```sh
go test ./... -count=1
go test -race ./internal/manager/...
go build ./cmd/hy2tun-manager
GOOS=windows GOARCH=amd64 go build ./...
scripts/icons/test-icons.sh
scripts/openwrt/test-ipk.sh
scripts/openwrt/test-luci-controller.sh
git diff --check
```

Expected: all pass; the manager does not alter existing binaries.

- [ ] **Step 5: Commit**

```sh
git add cmd/hy2tun-manager internal/manager Makefile scripts/release-manager.sh scripts/systemd/hy2tun-manager.service docs/hy2tun-manager.md
git commit -m "feat(manager): add command, release artifact, and docs"
```

---

## Execution Handoff

After the plan is approved, use `superpowers:subagent-driven-development` (recommended: state/security, upstream adapter, inventory, mapping UI, and settings are independently reviewable) or `superpowers:executing-plans`. Do not deploy the manager to production as part of this plan; deployment requires a separate host and the operator's reverse proxy.