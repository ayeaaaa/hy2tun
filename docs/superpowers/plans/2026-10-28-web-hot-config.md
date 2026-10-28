# hy2tun Web Hot Config Editing Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add hot-reloadable web config editing to hy2tund and hy2tun: web admin credentials, HY2 password, control token, port policy, trust proxy, client name, and secret reveal, with generation-based credential rotation and auto-push/manual sync.

**Architecture:** Add a shared `internal/credentials` package holding a generation-based current/previous credential set. The server uses a dynamic Hysteria authenticator and a dynamic control-token check, both reading an atomic store. Hot config changes write the YAML atomically, then swap runtime atomics. Clients receive `credential_update` over the control stream (if they advertise the capability), write their config, ack, and reconnect. Manual sync uses the client web form. Old credentials remain valid until an explicit revoke.

**Tech Stack:** Go 1.26, existing hy2tun codebase, `log/slog`, `html/template`, `embed`, `golang.org/x/crypto/bcrypt`, `sync/atomic`, `gopkg.in/yaml.v3`.

**Spec:** `/root/opencode-task/docs/superpowers/specs/2026-10-28-web-hot-config-design.md`

## Global Constraints

- Project root: `/root/opencode-task/hy2tun`, branch `main`.
- Hot fields only. Do not add web editing for `listen`, `listen_host`, TLS, obfs, QUIC, web listen/TLS, log file, or state_file.
- `auth.type` must be `password` for credential rotation; `userpass` returns `unsupported_auth_type`.
- Credential model: current + previous generations; both accepted until revoke.
- Either HY2 password or control token may be rotated alone; blank means keep current; both blank or unchanged returns 400.
- Auto-push waits at most 5 seconds per online capable client; returns `synced`, `pending`, `unsupported`, or `timeout`.
- Old clients without `credential_update_v1` must not receive `credential_update`; mark `unsupported`.
- All config writes are atomic (temp → fsync → rename → `.bak`), mode 0600. Write first, then swap runtime; on write failure runtime stays unchanged.
- Secrets never appear in logs, URLs, or error messages. Reveal responses use `Cache-Control: no-store`.
- Sensitive web mutations require the current web password plus the existing CSRF checks.
- Tests use `github.com/stretchr/testify/require`; `go test ./...` and `go test -race ./...` must pass.
- Commit after each task with a conventional commit message.

## Review Focus

1. **Old credentials after rotation** — current and previous both authenticate; after revoke only current works. Test in Task 3/5.
2. **Old client capability** — a client without `credential_update_v1` is not sent `credential_update`, stays connected, and is marked `unsupported`. Test in Task 3/6.
3. **Config write failure** — runtime credentials/ports stay unchanged when the YAML write fails. Test in Task 5/6.
4. **Web admin change** — changing username/password revokes existing web sessions; old cookies get 401. Test in Task 6/8.
5. **Reveal authorization** — wrong current password is rejected; correct password returns secrets with `no-store`; no secret is logged. Test in Task 6/8.

---

## Task 1: Credential Model and Config Fields

**Files:**
- Create: `internal/credentials/credentials.go`
- Create: `internal/credentials/credentials_test.go`
- Modify: `internal/config/config.go`
- Modify: `internal/config/server.go`
- Modify: `internal/config/client.go`
- Modify: `internal/config/config_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `credentials.Set` with `Generation int64`, `Auth string`, `ControlToken string`, `PreviousGeneration int64`, `PreviousAuth string`, `PreviousControlToken string`; JSON tags `generation`, `auth`, `control_token`, `previous_generation`, `previous_auth`, `previous_control_token`
  - `credentials.Store` with `NewStore(*Set) *Store`, `Load() *Set`, `Store(*Set)`
  - `(*Set).AcceptAuth(auth string) bool`
  - `(*Set).AcceptControlToken(token string) (generation int64, ok bool)`
  - `(*Set).Synced(generation int64) bool`
  - `config.CredentialsConfig` with `Generation`, `PreviousGeneration`, `PreviousAuth`, `PreviousControlToken`
  - `ServerConfig.Credentials` and `ClientConfig.Credentials`

- [ ] **Step 1: Write the failing credential tests**

Create `internal/credentials/credentials_test.go`:
```go
func TestAcceptCurrentAndPrevious(t *testing.T) {
    s := &Set{
        Generation: 2, Auth: "new", ControlToken: "new-token",
        PreviousGeneration: 1, PreviousAuth: "old", PreviousControlToken: "old-token",
    }
    require.True(t, s.AcceptAuth("new"))
    require.True(t, s.AcceptAuth("old"))
    require.False(t, s.AcceptAuth("other"))
    gen, ok := s.AcceptControlToken("new-token")
    require.True(t, ok)
    require.Equal(t, int64(2), gen)
    gen, ok = s.AcceptControlToken("old-token")
    require.True(t, ok)
    require.Equal(t, int64(1), gen)
    require.True(t, s.Synced(2))
    require.False(t, s.Synced(1))
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/credentials -run TestAcceptCurrentAndPrevious -v`
Expected: FAIL because the package does not exist.

- [ ] **Step 3: Implement the credentials package**

Implement the types and methods. `AcceptControlToken` returns the generation that matched; current first, then previous. `Synced` is `generation == s.Generation`. `Store` uses `atomic.Pointer[Set]`; `Load` returns a copy or a pointer that callers must not mutate. Add a `Clone()` method if needed.

- [ ] **Step 4: Write the failing config round-trip test**

Add to `internal/config/config_test.go`:
```go
func TestServerCredentialRoundTrip(t *testing.T) {
    dir := t.TempDir()
    path := filepath.Join(dir, "server.yaml")
    cfg := validServerConfig()
    cfg.Credentials = CredentialsConfig{
        Generation: 2, PreviousGeneration: 1,
        PreviousAuth: "old-auth", PreviousControlToken: "old-token",
    }
    require.NoError(t, SaveServer(path, cfg))
    got, err := LoadServer(path)
    require.NoError(t, err)
    require.Equal(t, int64(2), got.Credentials.Generation)
    require.Equal(t, int64(1), got.Credentials.PreviousGeneration)
    require.Equal(t, "old-auth", got.Credentials.PreviousAuth)
    require.Equal(t, "old-token", got.Credentials.PreviousControlToken)
}
```

- [ ] **Step 5: Run the test to verify it fails**

Run: `go test ./internal/config -run TestServerCredentialRoundTrip -v`
Expected: FAIL because the field does not exist.

- [ ] **Step 6: Add config fields and validation**

Add `CredentialsConfig` with YAML tags and `json:"-"` on previous secrets. Add `Credentials` to `ServerConfig` and `ClientConfig`. In `applyDefaults`, set `Generation` to 1 when 0. In `ServerConfig.Validate`:

- If `PreviousGeneration < 0` or `PreviousGeneration >= Generation` → error.
- If `PreviousGeneration > 0`, both `PreviousAuth` and `PreviousControlToken` must be non-empty.
- If `PreviousGeneration == 0`, previous fields must be empty.
- Do not apply the 8-byte/16-byte minimums to previous values.

In `ClientConfig.Validate`, require `Credentials.Generation >= 1`.

- [ ] **Step 7: Run config and credential tests**

Run: `go test ./internal/config ./internal/credentials -v`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/credentials internal/config
git commit -m "feat: add generation-based credential config model"
```

---

## Task 2: Control Protocol Additions

**Files:**
- Modify: `internal/control/message.go`
- Modify: `internal/control/message_test.go`
- Modify: `internal/control/frame.go` (type switch)
- Modify: `internal/control/frame_test.go`

**Interfaces:**
- Consumes: `config.Mapping`.
- Produces:
  - `control.Hello.CredentialGeneration int64`
  - `control.Hello.Capabilities []string`
  - `control.HelloAck.CredentialGeneration int64`
  - `control.HelloAck.CredentialSynced bool`
  - `control.Update.Name string`
  - `control.CredentialUpdate{Generation int64, Auth string, ControlToken string}`
  - `control.CredentialAck{Generation int64, OK bool, Error string}`
  - Constants `TypeCredentialUpdate`, `TypeCredentialAck`
  - `control.CapabilityCredentialUpdate = "credential_update_v1"`

- [ ] **Step 1: Write the failing protocol round-trip tests**

Add to `internal/control/message_test.go`:
```go
func TestCredentialMessagesRoundTrip(t *testing.T) {
    cases := []any{
        &Hello{Type: TypeHello, Protocol: 1, CredentialGeneration: 2, Capabilities: []string{CapabilityCredentialUpdate}},
        &HelloAck{Type: TypeHelloAck, OK: true, CredentialGeneration: 2, CredentialSynced: true},
        &Update{Type: TypeUpdate, Revision: 3, Name: "new-name"},
        &CredentialUpdate{Type: TypeCredentialUpdate, Generation: 2, Auth: "a", ControlToken: "t"},
        &CredentialAck{Type: TypeCredentialAck, Generation: 2, OK: true},
    }
    for _, in := range cases {
        var buf bytes.Buffer
        require.NoError(t, WriteMessage(&buf, in))
        out, err := ReadMessage(&buf)
        require.NoError(t, err)
        require.Equal(t, in, out)
    }
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/control -run TestCredentialMessagesRoundTrip -v`
Expected: FAIL because the fields/types do not exist.

- [ ] **Step 3: Add fields, types, constants, and decode cases**

Add the fields and types with JSON tags:
```go
type CredentialUpdate struct {
    Type         string `json:"type"`
    Generation   int64  `json:"generation"`
    Auth         string `json:"auth"`
    ControlToken string `json:"control_token"`
}
type CredentialAck struct {
    Type       string `json:"type"`
    Generation int64  `json:"generation"`
    OK         bool   `json:"ok"`
    Error      string `json:"error"`
}
```
Add the new cases to the decode switch in `frame.go`. Keep `ProtocolVersion = 1`; capabilities are additive.

- [ ] **Step 4: Run all control tests**

Run: `go test ./internal/control -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/control
git commit -m "feat: add credential generation control messages"
```

---

## Task 3: Server Dynamic Credentials and Session Generations

**Files:**
- Create: `internal/server/credentials.go`
- Create: `internal/server/credentials_test.go`
- Modify: `internal/server/server.go`
- Modify: `internal/server/control.go`
- Modify: `internal/server/mapping.go`
- Modify: `internal/view/view.go`
- Modify: `internal/server/control_test.go`

**Interfaces:**
- Consumes: `credentials`, `config`, `control`, `view`.
- Produces:
  - `Server.credentials *credentials.Store`
  - `Server.currentCredentials() *credentials.Set`
  - `clientSession.credentialGeneration int64`
  - `clientSession.credentialSynced bool`
  - `clientSession.canCredentialUpdate bool`
  - `clientSession.credentialAck chan int64`
  - `view.ClientView.CredentialGeneration int64`
  - `view.ClientView.CredentialSynced bool`
  - `view.ClientView.CredentialCapable bool`
  - `view.CredentialResult{ClientID, Name string; Generation int64; Status string; Error string}`
  - `newDynamicAuthenticator(store *credentials.Store, fallback hyserver.Authenticator) hyserver.Authenticator`

- [ ] **Step 1: Write the failing dynamic-authenticator test**

Create `internal/server/credentials_test.go`:
```go
func TestDynamicAuthenticatorAcceptsCurrentAndPrevious(t *testing.T) {
    store := credentials.NewStore(&credentials.Set{
        Generation: 2, Auth: "new",
        PreviousGeneration: 1, PreviousAuth: "old",
    })
    a := newDynamicAuthenticator(store, nil)
    ok, id := a.Authenticate(nil, "new", 0)
    require.True(t, ok)
    require.Equal(t, "user", id)
    ok, _ = a.Authenticate(nil, "old", 0)
    require.True(t, ok)
    ok, _ = a.Authenticate(nil, "other", 0)
    require.False(t, ok)
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/server -run TestDynamicAuthenticatorAcceptsCurrentAndPrevious -v`
Expected: FAIL because the type does not exist.

- [ ] **Step 3: Implement the dynamic authenticator and server store**

Add `internal/server/credentials.go`:
- `dynamicAuthenticator` with `store *credentials.Store` and optional fallback.
- `Authenticate` reads the store; accepts current/previous auth; returns id `"user"`.
- `newDynamicAuthenticator`.

In `server.New`:
- Build `credSet` from `cfg` before creating the authenticator.
- For `auth.type == "password"`, use `newDynamicAuthenticator(credSet, nil)`.
- For `userpass`, use the static authenticator; credential editing will be rejected by the API.
- Store `s.credentials = credentials.NewStore(credSet)`.

In `controlHandler.Serve`:
- Replace the direct `h.server.cfg.Control.Token` comparison with `set := h.server.credentials.Load(); gen, ok := set.AcceptControlToken(hello.Token)`.
- If not ok, reject unauthorized.
- Set `sess.credentialGeneration = hello.CredentialGeneration` (default 1 if 0), `sess.credentialSynced = set.Synced(sess.credentialGeneration)`, `sess.canCredentialUpdate = hasCapability(hello.Capabilities, control.CapabilityCredentialUpdate)`.
- Add `credentialAck: make(chan int64, 1)`.

In `control.go` loop, add `case *control.CredentialAck`: if `message.Generation == sess.credentialGeneration` or any, non-blocking send to `sess.credentialAck`.

Also update `handleUpdate`: if `message.Name != ""`, set `sess.name = message.Name` and persist the state after a successful update; a name-only update should not require mapping changes.

In `mapping.go`:
- Add fields to `clientSession`.
- In `clientViews`, populate `CredentialGeneration`, `CredentialSynced`, `CredentialCapable`.

In `view.go`, add the three fields to `ClientView`.

- [ ] **Step 4: Run the focused tests**

Run: `go test ./internal/server -run 'TestDynamicAuthenticator|TestClientRegisters' -v`
Expected: PASS.

- [ ] **Step 5: Write the failing hello-generation test**

Add to `internal/server/control_test.go`:
```go
func TestHelloCarriesCredentialGeneration(t *testing.T) {
    pair := startTestPair(t, []config.Mapping{
        {ID: "tcp1", Type: "tcp", RemotePort: 0, LocalAddr: "127.0.0.1:9001", Enabled: true},
    })
    defer pair.Stop()
    require.NoError(t, waitFor(t, 5*time.Second, func() bool {
        v := pair.Server.Clients()
        return len(v) == 1 && v[0].CredentialGeneration == 1 && v[0].CredentialSynced
    }))
}
```
Adjust the helper's client config to set `Credentials.Generation = 1`.

- [ ] **Step 6: Run the test**

Run: `go test ./internal/server -run TestHelloCarriesCredentialGeneration -v`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/server internal/view
git commit -m "feat: add dynamic server credentials and session generations"
```

---

## Task 4: Server Hot Ports, Offline Reserve, and Trust Proxy

**Files:**
- Modify: `internal/server/mapping.go`
- Modify: `internal/server/server.go`
- Modify: `internal/state/store.go`
- Modify: `internal/webui/shared/auth.go`
- Modify: `internal/webui/serverui/handlers.go`
- Modify: `internal/webui/clientui/handlers.go`
- Create: `internal/server/hot_settings_test.go`

**Interfaces:**
- Consumes: `ports`, `state`, `config`.
- Produces:
  - `(*mappingManager).SetPolicy(*ports.Policy)`
  - `(*state.Store).SetOfflineReserve(time.Duration)`
  - `(*Server).UpdatePorts(allow, deny []string, autoRange string, offlineReserve time.Duration) error`
  - `(*Server).SetTrustProxy(bool) error`
  - `(*Server).currentConfig() *config.ServerConfig`
  - `Server.settingsMu sync.RWMutex`
  - `(*serverui.Server).SetTrustProxy(bool)`

- [ ] **Step 1: Write the failing hot-port test**

Create `internal/server/hot_settings_test.go`:
```go
func TestHotPortPolicyUpdate(t *testing.T) {
    pair := startTestPair(t, []config.Mapping{
        {ID: "tcp1", Type: "tcp", RemotePort: 25022, LocalAddr: "127.0.0.1:9001", Enabled: true},
    })
    defer pair.Stop()
    require.NoError(t, waitFor(t, 5*time.Second, func() bool { return len(pair.Server.Clients()) == 1 }))

    require.NoError(t, pair.Server.UpdatePorts([]string{"25022", "25023"}, nil, "25000-25099", time.Hour))
    // Existing mapping still runs.
    require.Equal(t, "running", pair.Server.Clients()[0].Mappings[0].Status)

    // New disallowed port is rejected on the next update.
    _, err := pair.Server.PushMappings(pair.Server.Clients()[0].ID, pair.Server.Clients()[0].Revision, []config.Mapping{
        {ID: "tcp2", Type: "tcp", RemotePort: 25024, LocalAddr: "127.0.0.1:9002", Enabled: true},
    })
    var pushErr *PushError
    require.ErrorAs(t, err, &pushErr)
    require.Equal(t, "port_not_allowed", pushErr.Results[0].Error)
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/server -run TestHotPortPolicyUpdate -v`
Expected: FAIL because `UpdatePorts` does not exist.

- [ ] **Step 3: Implement hot ports and offline reserve**

- Change `mappingManager.policy` to `atomic.Pointer[ports.Policy]`; `newMappingManager` stores the initial policy; `bindLocked` loads it.
- Add `SetPolicy(*ports.Policy)`.
- Add `state.Store.SetOfflineReserve(time.Duration)` using an atomic int64; `SetOnline(false)` reads it.
- Add `Server.UpdatePorts`: build a new policy with `ports.NewPolicy`, validate ranges, copy config, set `Ports.Allow/Deny/AutoRange/OfflineReserve`, `config.SaveServer`, then `m.mappings.SetPolicy` and `s.store.SetOfflineReserve`. On save failure, return before swapping.
- Add `Server.SetTrustProxy`: copy config, set `Web.TrustProxy`, save, then `s.trustProxy.Store`.
- Add `trustProxy atomic.Bool` to `Server`; use it in `ConfigSummary`/web setup.
- Add `settingsMu sync.RWMutex` and `currentConfig()`; every hot update builds a copy, saves it, then replaces `s.cfg` under `settingsMu`. Readers (ConfigSummary, settings handlers) use `currentConfig()`.
- Add `SetTrustProxy` to `serverui.Server`; the server web auth/limiter code reads it. Client `web.trust_proxy` remains file-only in v1.1.

- [ ] **Step 4: Run the focused test**

Run: `go test ./internal/server -run TestHotPortPolicyUpdate -v`
Expected: PASS.

- [ ] **Step 5: Write the failing trust-proxy test**

Add to `internal/server/hot_settings_test.go`:
```go
func TestTrustProxyToggle(t *testing.T) {
    srv, stop := startTestServer(t)
    defer stop()
    require.NoError(t, srv.SetTrustProxy(true))
    require.True(t, srv.trustProxy.Load())
    require.NoError(t, srv.SetTrustProxy(false))
    require.False(t, srv.trustProxy.Load())
}
```
Also add a `shared.ClientIPWithProxy` test (in `internal/webui/shared/auth_test.go`) that proves the first XFF entry is ignored and the last entry / X-Real-IP is used when trust is enabled.

- [ ] **Step 6: Run all server/web tests**

Run: `go test ./internal/server ./internal/webui/... -v`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/server internal/state internal/webui
git commit -m "feat: hot-reload port policy, offline reserve, and trust proxy"
```

---

## Task 5: Server Credential Rotation, Revoke, and Sync Results

**Files:**
- Modify: `internal/server/server.go`
- Modify: `internal/server/control.go`
- Modify: `internal/server/mapping.go`
- Create: `internal/server/rotation.go`
- Create: `internal/server/rotation_test.go`

**Interfaces:**
- Consumes: `credentials`, `control`, `config`, `view`.
- Produces:
  - `Server.saveConfig func(path string, cfg *config.ServerConfig) error` (default `config.SaveServer`; test seam)
  - `(*Server).RotateCredentials(auth, controlToken, syncMode string) (int64, []view.CredentialResult, error)`
  - `(*Server).RevokePreviousCredentials() error`
  - `(*Server).CredentialsSnapshot() *credentials.Set`
  - `(*Server).CredentialStatuses() []view.CredentialResult`
  - `(*Server).Settings() serverui.SettingsView` (implemented in Task 6)

- [ ] **Step 1: Write the failing rotation tests**

Create `internal/server/rotation_test.go`:
```go
func TestRotateCredentialsAutoPush(t *testing.T) {
    pair := startTestPair(t, []config.Mapping{
        {ID: "tcp1", Type: "tcp", RemotePort: 0, LocalAddr: "127.0.0.1:9001", Enabled: true},
    })
    defer pair.Stop()
    require.NoError(t, waitFor(t, 5*time.Second, func() bool { return len(pair.Server.Clients()) == 1 }))

    gen, results, err := pair.Server.RotateCredentials("new-auth", "new-control-token", "auto")
    require.NoError(t, err)
    require.Equal(t, int64(2), gen)
    require.Len(t, results, 1)
    require.Equal(t, "synced", results[0].Status)

    require.NoError(t, waitFor(t, 5*time.Second, func() bool {
        v := pair.Server.Clients()
        return len(v) == 1 && v[0].CredentialGeneration == 2 && v[0].CredentialSynced
    }))
    saved, err := config.LoadClient(pair.ClientConfigPath)
    require.NoError(t, err)
    require.Equal(t, int64(2), saved.Credentials.Generation)
    require.Equal(t, "new-auth", saved.Auth)
    require.Equal(t, "new-control-token", saved.Control.Token)
}
```

Add a manual test, a revoke test, and a single-field rotation test.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/server -run 'TestRotateCredentials|TestRevokePrevious' -v`
Expected: FAIL because the methods do not exist.

- [ ] **Step 3: Implement rotation**

`RotateCredentials`:

1. If `cfg.Auth.Type != "password"` → `unsupported_auth_type`.
2. Load current credentials from the store.
3. If both new values are empty → `no_change`.
4. If new value equals current and the other is blank → `no_change`.
5. Build new config copy:
   - `PreviousGeneration = current.Generation`
   - `PreviousAuth = current.Auth`
   - `PreviousControlToken = current.ControlToken`
   - `Generation = current.Generation + 1`
   - `Auth.Password = newAuth` (or current if blank)
   - `Control.Token = newControlToken` (or current if blank)
6. `config.SaveServer(cfg.ConfigPath, newCfg)`.
7. Replace `s.cfg` with `newCfg` under `settingsMu`; store the new `credentials.Set`.
8. If `syncMode == "auto"`, iterate sessions; only push to sessions whose `ready` channel is closed and that are `canCredentialUpdate`; for each, send `CredentialUpdate` and wait up to 5s on `credentialAck`; collect status. Not-ready/incapable sessions are `pending`/`unsupported`.
9. Return generation and results.

`RevokePreviousCredentials`:
1. Load current set; if `PreviousGeneration == 0`, return nil.
2. Build config with previous fields cleared.
3. Save.
4. Store new set.
5. Close all sessions with `credentialGeneration != current.Generation` via `sess.abortStream()`.
6. Return.

`CredentialsSnapshot` returns a copy of the current set.

`CredentialStatuses` returns per-session `ClientID`, `Name`, `Generation`, `Status` (`synced`, `pending`, `unsupported`, `unknown`), `Error`.

- [ ] **Step 4: Run the rotation tests**

Run: `go test ./internal/server -run 'TestRotateCredentials|TestRevokePrevious|TestSingleFieldRotation' -v`
Expected: PASS.

- [ ] **Step 5: Write the failing config-write-failure test**

Add a `saveConfig` field to `Server`, initialized to `config.SaveServer` in `server.New`. In the test, set it to a function that returns `errors.New("write failed")`, call `RotateCredentials`, assert the error, and assert `CredentialsSnapshot().Generation` still equals the old generation. Do the same for `UpdatePorts`/`UpdateWebConfig` if they use the same seam.

- [ ] **Step 6: Run all server tests**

Run: `go test ./internal/server -v`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/server
git commit -m "feat: add credential rotation, revoke, and sync results"
```

---

## Task 6: Web Session Admin Update and Server Settings API/UI

**Files:**
- Modify: `internal/webui/shared/auth.go`
- Modify: `internal/webui/shared/auth_test.go`
- Modify: `internal/webui/serverui/handlers.go`
- Modify: `internal/webui/serverui/handlers_test.go`
- Modify: `internal/webui/serverui/views.go`
- Modify: `internal/webui/serverui/templates/settings.html`
- Modify: `internal/webui/serverui/static/app.js`
- Modify: `internal/server/server.go`

**Interfaces:**
- Consumes: `credentials`, `config`, `view`.
- Produces:
  - `(*shared.SessionManager).UpdateAdmin(username, passwordHash string)`
  - `(*shared.SessionManager).RevokeAll()`
  - `(*shared.SessionManager).VerifyPassword(password string) bool`
  - `serverui.ServerAPI` additions:
    ```go
    Settings() serverui.SettingsView
    UpdateWebConfig(username, passwordHash string) error
    UpdatePorts(allow, deny []string, autoRange string, offlineReserve time.Duration) error
    RotateCredentials(auth, controlToken, syncMode string) (int64, []view.CredentialResult, error)
    RevokePreviousCredentials() error
    CredentialsSnapshot() *credentials.Set
    SetTrustProxy(bool) error
    ```
  - `serverui.SettingsView` with web username, trust proxy, ports, generations, masked flags, client statuses, read-only list.
  - Routes: `GET /api/settings`, `PUT /api/settings/web`, `PUT /api/settings/ports`, `PUT /api/settings/credentials`, `POST /api/settings/credentials/revoke`, `POST /api/settings/credentials/reveal`.

- [ ] **Step 1: Write the failing session-admin tests**

Add to `internal/webui/shared/auth_test.go`:
```go
func TestUpdateAdminRevokesOldSessions(t *testing.T) {
    m, err := NewSessionManager([]byte("0123456789abcdef0123456789abcdef"), time.Hour)
    require.NoError(t, err)
    rr := httptest.NewRecorder()
    req := httptest.NewRequest(http.MethodPost, "/login", nil)
    require.NoError(t, m.Issue(rr, req, "admin"))
    cookie := rr.Result().Cookies()[0]

    m.UpdateAdmin("root", "$2a$10$...") // valid bcrypt hash
    m.RevokeAll()

    req2 := httptest.NewRequest(http.MethodGet, "/", nil)
    req2.AddCookie(cookie)
    require.False(t, m.Valid(req2))
    require.True(t, m.VerifyPassword("..."))
}
```
Use a real bcrypt hash generated in the test.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/webui/shared -run TestUpdateAdminRevokesOldSessions -v`
Expected: FAIL because the methods do not exist.

- [ ] **Step 3: Implement session admin methods**

`SessionManager` stores `username`, `passwordHash`, `sessions`. `UpdateAdmin` sets them and calls `RevokeAll`. `VerifyPassword` compares bcrypt. `RevokeAll` clears the session map. Ensure login handlers use the current values.

- [ ] **Step 4: Implement server settings API**

Add the `ServerAPI` methods on `*server.Server`. Add handlers:
- `GET /api/settings`: return masked settings and client credential statuses; no plaintext secrets.
- `PUT /api/settings/web`: verify current password; hash new password; call `UpdateWebConfig`; `sessions.UpdateAdmin`; `sessions.RevokeAll`; 204.
- `PUT /api/settings/ports`: verify current password; call `UpdatePorts`; 200.
- `PUT /api/settings/credentials`: verify current password; call `RotateCredentials`; return generation/results.
- `POST /api/settings/credentials/revoke`: verify current password; call `RevokePreviousCredentials`; 204.
- `POST /api/settings/credentials/reveal`: verify current password; return snapshot with `Cache-Control: no-store`.
- All mutations require the existing CSRF header/Origin.

- [ ] **Step 5: Update settings template/JS**

Replace the read-only settings page with sections:
- Web 管理员 (username, current password, new password, confirm)
- HY2 凭据 (generation, masked current/previous, reveal button, new auth/token, sync mode, rotate, revoke, client status table)
- 端口白名单 (allow, deny, auto_range, offline_reserve)
- 代理信任 (trust_proxy)
- Read-only fields list.

Use the existing form/JSON fetch style. Show a warning when rotating with previous credentials or changing ports.

- [ ] **Step 6: Write the failing server web settings tests**

Add tests for:
- Login, GET `/api/settings` returns masked values.
- `PUT /api/settings/credentials` with auto returns generation/results; fake API receives the call.
- `POST /api/settings/credentials/reveal` with wrong password 401/403, correct password 200 with no-store.
- `PUT /api/settings/web` revokes the session (old cookie 401).
- CSRF missing Origin 403.

- [ ] **Step 7: Run web tests**

Run: `go test ./internal/webui/shared ./internal/webui/serverui -v`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/webui internal/server
git commit -m "feat: add server web hot settings and credential reveal"
```

---

## Task 7: Client Hot Credentials, Name, and Status

**Files:**
- Modify: `internal/client/client.go`
- Modify: `internal/client/control.go`
- Modify: `internal/client/configsync.go`
- Modify: `internal/view/view.go`
- Modify: `internal/client/client_test.go`
- Create: `internal/client/rotation_test.go`

**Interfaces:**
- Consumes: `credentials`, `config`, `control`.
- Produces:
  - `(*Client).RotateLocalCredentials(generation int64, auth, controlToken string) error`
  - `(*Client).UpdateName(name string) error`
  - `(*Client).UpdateWebConfig(username, passwordHash string) error`
  - `(*Client).CredentialsSnapshot() *credentials.Set`
  - `(*Client).ServerCredentialGeneration() int64`
  - `(*Client).CredentialSynced() bool`
  - `(*Client).Settings() clientui.SettingsView` (implemented in Task 8)
  - `view.ClientStatus.CredentialGeneration int64`
  - `view.ClientStatus.ServerCredentialGeneration int64`
  - `view.ClientStatus.CredentialSynced bool`

- [ ] **Step 1: Write the failing client rotation test**

Create `internal/client/rotation_test.go`:
```go
func TestClientRotateLocalCredentialsWritesConfigAndReconnects(t *testing.T) {
    pair := startTestPair(t, []config.Mapping{
        {ID: "tcp1", Type: "tcp", RemotePort: 0, LocalAddr: "127.0.0.1:9001", Enabled: true},
    })
    defer pair.Stop()
    require.NoError(t, waitFor(t, 5*time.Second, func() bool { return len(pair.Server.Clients()) == 1 }))

    gen := pair.Server.CredentialsSnapshot().Generation + 1
    // Manual server rotation first, so the server accepts the new values.
    _, _, err := pair.Server.RotateCredentials("new-auth", "new-control-token", "manual")
    require.NoError(t, err)
    require.NoError(t, pair.Client.RotateLocalCredentials(gen, "new-auth", "new-control-token"))

    require.NoError(t, waitFor(t, 5*time.Second, func() bool {
        v := pair.Server.Clients()
        return len(v) == 1 && v[0].CredentialGeneration == gen && v[0].CredentialSynced
    }))
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/client -run TestClientRotateLocalCredentials -v`
Expected: FAIL because the method does not exist.

- [ ] **Step 3: Implement client rotation and name**

- Add `credentialsGeneration int64`, `serverCredentialGeneration int64`, `credentialSynced bool` to `Client`.
- In hello, set `CredentialGeneration` and `Capabilities: []string{control.CapabilityCredentialUpdate}`.
- In hello_ack, read `CredentialGeneration`/`CredentialSynced`.
- `RotateLocalCredentials`: validate generation > current; update config under `configMu`; call `config.SaveClient`; update memory; call `forceReconnect` (close control/HY2; let the loop reconnect).
- `UpdateName`: bump revision, write config, update memory, send `Update` with `Name` if connected; otherwise the next hello carries it. A name-only update must be accepted by the server and persist the new name.
- `UpdateWebConfig`: write config; no reconnect.
- `CredentialsSnapshot`: return current values.
- `Status`: add the new fields.
- `handleCredentialUpdate` in `control.go`: validate generation; write config; send `CredentialAck`; force reconnect.

- [ ] **Step 4: Run the client tests**

Run: `go test ./internal/client -run 'TestClientRotate|TestClientName|TestClientStatus' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/client internal/view
git commit -m "feat: add client credential rotation, name update, and status"
```

---

## Task 8: Client Web Settings API/UI

**Files:**
- Modify: `internal/webui/clientui/handlers.go`
- Modify: `internal/webui/clientui/handlers_test.go`
- Modify: `internal/webui/clientui/views.go`
- Modify: `internal/webui/clientui/templates/index.html`
- Modify: `internal/webui/clientui/static/app.js`

**Interfaces:**
- Consumes: `client.Client`, `shared.SessionManager`.
- Produces:
  - `clientui.ClientAPI` additions:
    ```go
    Settings() clientui.SettingsView
    UpdateWebConfig(username, passwordHash string) error
    UpdateName(name string) error
    RotateLocalCredentials(generation int64, auth, controlToken string) error
    CredentialsSnapshot() *credentials.Set
    ```
  - Routes: `GET /api/settings`, `PUT /api/settings/web`, `PUT /api/settings/name`, `PUT /api/settings/credentials`, `POST /api/settings/credentials/reveal`.

- [ ] **Step 1: Write the failing client-web settings test**

Add to `internal/webui/clientui/handlers_test.go`:
```go
func TestClientWebRotateCredentials(t *testing.T) {
    api := &fakeClientAPI{}
    web, err := NewClient(api, config.WebConfig{
        Listen: "127.0.0.1:0", Username: "admin", PasswordHash: mustHash("pw"),
    }, testSessionKey, slog.Default())
    require.NoError(t, err)
    ts := httptest.NewServer(web.Handler())
    defer ts.Close()

    client := ts.Client()
    resp, err := client.PostForm(ts.URL+"/api/login", url.Values{"username": {"admin"}, "password": {"pw"}})
    require.NoError(t, err)
    require.Equal(t, http.StatusNoContent, resp.StatusCode)

    body := `{"current_password":"pw","generation":2,"auth":"new","control_token":"new-token"}`
    req, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/settings/credentials", strings.NewReader(body))
    req.Header.Set("Content-Type", "application/json")
    req.Header.Set("X-Requested-With", "hy2tun")
    req.Header.Set("Origin", ts.URL)
    req.AddCookie(resp.Cookies()[0])
    resp, err = client.Do(req)
    require.NoError(t, err)
    require.Equal(t, http.StatusAccepted, resp.StatusCode)
    require.Equal(t, int64(2), api.rotation.Generation)
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/webui/clientui -run TestClientWebRotateCredentials -v`
Expected: FAIL because the route/API do not exist.

- [ ] **Step 3: Implement client settings API/UI**

- Extend `fakeClientAPI` with `settings`, `rotation`, `name`, `web` fields.
- Add handlers and routes.
- `PUT /api/settings/credentials`: verify current password; call `api.RotateLocalCredentials`; return 202.
- `POST /api/settings/credentials/reveal`: verify current password; return snapshot with no-store.
- `PUT /api/settings/name`: verify current password; call `api.UpdateName`.
- `PUT /api/settings/web`: verify current password; hash new password; call `api.UpdateWebConfig`; update session manager; revoke all; 204.
- Update settings template with sections: Web 管理员, HY2 凭据 (generation, server generation, masked auth/token, reveal, new values, save & reconnect), 客户端名称, read-only fields.

- [ ] **Step 4: Run client-web tests**

Run: `go test ./internal/webui/clientui -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/webui/clientui
git commit -m "feat: add client web hot settings and manual credential sync"
```

---

## Task 9: Integration and E2E

**Files:**
- Create: `internal/server/rotation_integration_test.go`
- Modify: `internal/e2e/e2e_test.go`
- Modify: `internal/webui/serverui/handlers_test.go`
- Modify: `internal/webui/clientui/handlers_test.go`

**Interfaces:**
- Consumes: all previous tasks.
- Produces: end-to-end proof.

- [ ] **Step 1: Write the failing auto-push integration test**

Add a test that:
1. Starts a server/client pair with a TCP mapping.
2. Calls `Server.RotateCredentials(newAuth, newToken, "auto")`.
3. Waits for the client config to update and the server view to show `synced`.
4. Verifies the TCP mapping still works.
5. Calls `RevokePreviousCredentials`.
6. Starts a second client with the old auth/token and asserts it cannot register.

- [ ] **Step 2: Run it to verify it fails or exposes gaps**

Run: `go test ./internal/server -run TestCredentialRotationEndToEnd -v`
Expected: FAIL until the integration path is complete.

- [ ] **Step 3: Add the manual and web-admin E2E tests**

- Manual rotation: server rotates with `"manual"`, client config unchanged, client web API rotates, reconnect, synced.
- Web admin: server web changes username/password; old cookie 401; new login 204.
- Ports: server web changes allow list; new disallowed mapping fails; existing mapping works.
- Name: client web changes name; server view shows it.
- Reveal: wrong password 401/403; correct password 200 + `Cache-Control: no-store`.

- [ ] **Step 4: Extend the existing E2E**

In `internal/e2e/e2e_test.go`, after the existing push/delete flow:
- Rotate credentials via the server web API with auto push.
- Wait for the client to reconnect.
- Verify TCP/UDP mappings still work.
- Revoke old credentials.
- Verify a client with old credentials fails.

- [ ] **Step 5: Run the full suite and race detector**

Run:
```bash
go test ./... -count=1
go test -race ./... -count=1
```
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/server internal/e2e internal/webui
git commit -m "test: add credential rotation and hot settings integration coverage"
```

---

## Task 10: Documentation and Final Verification

**Files:**
- Modify: `README.md`
- Modify: `examples/server.yaml`
- Modify: `examples/client.yaml`
- Modify: `docs/superpowers/specs/2026-10-28-web-hot-config-design.md` only if implementation reveals a spec gap.

- [ ] **Step 1: Update README**

Document:
- Which settings are hot-editable and which remain file-only.
- Credential rotation: auto-push vs manual, current/previous, revoke.
- Secret reveal: masked by default, current password required, no-store.
- Web admin password cannot be shown; reset only.
- `credentials.generation` fields.
- Capability `credential_update_v1` and old-client behavior.

- [ ] **Step 2: Update examples**

Add `credentials.generation: 1` to both examples; add commented previous fields to the server example. Do not put real secrets.

- [ ] **Step 3: Run full verification**

Run:
```bash
go test ./... -count=1
go test -race ./... -count=1
make build
make release
```
Expected: PASS; archives and checksums generated.

- [ ] **Step 4: Commit**

```bash
git add README.md examples
git commit -m "docs: document hot config editing and credential rotation"
```

---

## Self-Review Notes

- **Spec coverage:** Tasks 1–5 cover the credential model, protocol, server dynamic auth, hot ports/trust proxy, rotation/revoke. Tasks 6–8 cover the two web UIs and session admin. Task 9 covers integration/E2E. Task 10 covers docs.
- **Non-goals preserved:** listen/TLS/obfs/QUIC/web listen/log/state_file remain file-only; `userpass` and client connection settings remain file-only.
- **Type consistency:** `credentials.Set` is the single runtime credential type; config uses `CredentialsConfig`; `view.ClientView`/`ClientStatus` carry generation/sync fields; `control` carries generation/capability.
- **Test seams:** `startTestPair`, `fakeServerAPI`, `fakeClientAPI`, `mustHash`, `testSessionKey` already exist; extend them.
- **Known implementation choices:** exact UI layout, error strings, audit log format, whether to add a dedicated `settings` package. These do not change the product behavior.
