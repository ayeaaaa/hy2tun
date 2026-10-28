# hy2tun Mimic Mode Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add an explicit normal/Mimic transport mode to hy2tun and hy2tund, with file and Web configuration, systemd-backed Web restart, and no silent fallback.

**Architecture:** A new `internal/mimic` package starts/stops the external `mimic` child process on Linux, builds filters from the HY2 listen/server addresses, and waits for its lock file. Server/client runtime starts Mimic before HY2, sets QUIC `DisableGSO`, and stops Mimic on close. Web settings write the `mimic` config without hot-swapping runtime; a separate systemd-only restart endpoint exits the process and lets `Restart=always` restart it.

**Tech Stack:** Go 1.26, `log/slog`, `exec.CommandContext`, `syscall`, existing `config.Duration`, `testify/require`, existing Web UI templates/JS.

**Spec:** `docs/superpowers/specs/2026-10-28-mimic-mode-design.md`

## Global Constraints

- `mimic.enabled` defaults to `false`; existing configs and behavior must not change.
- Normal mode keeps port hopping; Mimic mode rejects port hopping and any multi-port `server`.
- Mimic is Linux-only; Windows/OpenWrt builds must compile but return a clear startup error when enabled.
- Both server and client must enable Mimic; no coexistence, no dual-port, no automatic fallback.
- Mimic mode disables QUIC GSO on both ends.
- Mimic binary is an external GPL-2.0-only dependency; do not bundle it.
- Web save writes YAML only; it does not start/stop Mimic or restart the process.
- Web restart requires current admin password + CSRF + same-origin, only works under systemd, and only exits the current process.
- No shell commands, arbitrary unit names, or secrets in logs/errors.
- `interface`/`path` filesystem checks happen only when `enabled: true`.
- Keep `README.md` and `README.zh-CN.md` in sync.
- `go test ./...`, `go test -race ./...`, and `make build` must pass.

## Review Focus

1. **Silent fallback after Mimic exits** — the client must not reconnect in normal mode; `Start` must return a fatal error. Test in Tasks 3/4.
2. **Disabled config with stale interface/path** — `enabled: false` must not validate filesystem fields or alter YAML. Test in Task 1.
3. **Specific-IP server interface selection** — Mimic must attach to the NIC owning `listen` IP, not the default route. Test in Task 2.
4. **Every Mimic socket path gets GSO disabled** — both server and client core configs must set `DisableGSO` only in Mimic mode. Test in Tasks 3/4.
5. **Web restart escaping its sandbox** — only the current process may exit; no unit name/command input; non-systemd returns 409. Test in Tasks 5/6.
6. **Standard clients after server switch** — Web/startup must warn that ordinary HY2 clients cannot connect. Test/docs in Tasks 5/7.

---

### Task 1: Config schema and validation

**Files:**
- Modify: `internal/config/server.go`
- Modify: `internal/config/client.go`
- Modify: `internal/config/config.go`
- Test: `internal/config/config_test.go`

**Interfaces:**
- Produces:
  - `type MimicConfig struct { Enabled bool; Interface string; XDPMode string; Path string; ExtraArgs []string }`
  - `func (m *MimicConfig) Validate() error`
  - `func (m *MimicConfig) Clone() *MimicConfig`
  - `ServerConfig.Mimic *MimicConfig`
  - `ClientConfig.Mimic *MimicConfig`

- [ ] **Step 1: Write failing tests**

Add table tests to `internal/config/config_test.go`:

```go
func TestMimicConfigDefaultsAndValidation(t *testing.T) {
	cfg := validClientConfig()
	require.NoError(t, cfg.Validate())
	require.Nil(t, cfg.Mimic)

	for _, mode := range []string{"", "native", "skb"} {
		cfg := validClientConfig()
		cfg.Mimic = &MimicConfig{Enabled: true, XDPMode: mode}
		require.NoError(t, cfg.Validate(), mode)
	}
	for _, mode := range []string{"bad", "native ", "SKB"} {
		cfg := validClientConfig()
		cfg.Mimic = &MimicConfig{Enabled: true, XDPMode: mode}
		require.ErrorContains(t, cfg.Validate(), "mimic.xdp_mode", mode)
	}
}

func TestMimicRejectsPortHopping(t *testing.T) {
	cfg := validClientConfig()
	cfg.Server = "127.0.0.1:30000-30001"
	cfg.Mimic = &MimicConfig{Enabled: true}
	require.ErrorContains(t, cfg.Validate(), "mimic cannot be used with port hopping")

	cfg = validClientConfig()
	cfg.Mimic = &MimicConfig{Enabled: true}
	cfg.Transport = &ClientTransportConfig{UDP: &ClientTransportUDPConfig{
		HopInterval: Duration(30 * time.Second),
	}}
	require.ErrorContains(t, cfg.Validate(), "mimic cannot be used with port hopping")
}

func TestMimicDisabledIgnoresStaleRuntimeFields(t *testing.T) {
	cfg := validClientConfig()
	cfg.Mimic = &MimicConfig{
		Enabled:   false,
		Interface: "does-not-exist",
		Path:      "/does/not/exist",
	}
	require.NoError(t, cfg.Validate())
}
```

Add server tests for the same `Mimic` field and XDP validation. Add a round-trip test proving `mimic.enabled: false` is omitted or harmless and `mimic.enabled: true` persists.

- [ ] **Step 2: Run tests to verify RED**

```bash
go test ./internal/config -run 'TestMimic' -v
```

Expected: FAIL because `MimicConfig` does not exist.

- [ ] **Step 3: Implement config types**

In `internal/config/config.go`:

```go
type MimicConfig struct {
	Enabled   bool     `yaml:"enabled" json:"enabled"`
	Interface string   `yaml:"interface,omitempty" json:"interface,omitempty"`
	XDPMode   string   `yaml:"xdp_mode,omitempty" json:"xdp_mode,omitempty"`
	Path      string   `yaml:"path,omitempty" json:"path,omitempty"`
	ExtraArgs []string `yaml:"extra_args,omitempty" json:"extra_args,omitempty"`
}

func (m *MimicConfig) Clone() *MimicConfig {
	if m == nil {
		return nil
	}
	out := *m
	out.ExtraArgs = append([]string(nil), m.ExtraArgs...)
	return &out
}

func (m *MimicConfig) Validate() error {
	if m == nil {
		return nil
	}
	switch m.XDPMode {
	case "", "native", "skb":
	default:
		return fmt.Errorf("mimic.xdp_mode must be empty, native, or skb")
	}
	for _, arg := range m.ExtraArgs {
		if strings.ContainsAny(arg, "\x00\r\n") {
			return fmt.Errorf("mimic.extra_args must not contain NUL or newlines")
		}
	}
	return nil
}
```

Add `Mimic *MimicConfig` to `ServerConfig` and `ClientConfig` with `yaml:"mimic,omitempty" json:"mimic,omitempty"`.

In `ServerConfig.Validate` call `c.Mimic.Validate()`.

In `ClientConfig.Validate`:

```go
if err := c.Mimic.Validate(); err != nil {
	return err
}
if c.Mimic != nil && c.Mimic.Enabled {
	if c.ServerPortHopping() {
		return fmt.Errorf("mimic cannot be used with port hopping")
	}
	if c.Transport != nil && c.Transport.UDP != nil &&
		(c.Transport.UDP.HopInterval != 0 ||
			c.Transport.UDP.MinHopInterval != 0 ||
			c.Transport.UDP.MaxHopInterval != 0) {
		return fmt.Errorf("mimic cannot be used with port hopping")
	}
}
```

Do not check `Interface`/`Path` existence in config validation; those are startup checks in Task 2.

- [ ] **Step 4: Run config tests**

```bash
go test ./internal/config -run 'TestMimic' -v
go test ./internal/config
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/config/config.go internal/config/server.go internal/config/client.go internal/config/config_test.go
git commit -m "feat(config): add Mimic mode configuration"
```

---

### Task 2: Mimic child-process manager

**Files:**
- Create: `internal/mimic/mimic.go`
- Create: `internal/mimic/mimic_linux.go`
- Create: `internal/mimic/mimic_other.go`
- Test: `internal/mimic/mimic_linux_test.go` (`//go:build linux`)
- Test: `internal/mimic/mimic_test.go` (`//go:build linux`)

**Interfaces:**
- Produces:
  - `type Config struct { Enabled bool; Interface string; XDPMode string; Path string; ExtraArgs []string }`
  - `type Role int` with `RoleClient`, `RoleServer`
  - `func Start(cfg Config, role Role, addrs []*net.UDPAddr, log *slog.Logger, onExit func(error)) (*Instance, error)`
  - `func (i *Instance) Close() error`

- [ ] **Step 1: Write failing filter/unit tests**

In `internal/mimic/mimic_test.go`:

```go
func TestFiltersForClientAndServer(t *testing.T) {
	client := filtersFor(RoleClient, []*net.UDPAddr{
		{IP: net.ParseIP("203.0.113.10"), Port: 24443},
		{IP: net.ParseIP("2001:db8::10"), Port: 24443},
	})
	require.Equal(t, []string{
		"remote=203.0.113.10:24443",
		"remote=[2001:db8::10]:24443",
	}, client)

	server := filtersFor(RoleServer, []*net.UDPAddr{{Port: 24443}})
	require.Equal(t, []string{
		"local=0.0.0.0:24443,handshake=0:3",
		"local=[::]:24443,handshake=0:3",
	}, server)

	specific := filtersFor(RoleServer, []*net.UDPAddr{{IP: net.ParseIP("203.0.113.5"), Port: 24443}})
	require.Equal(t, []string{"local=203.0.113.5:24443,handshake=0:3"}, specific)
}

func TestRunArgs(t *testing.T) {
	args := runArgs("eth0", []string{"remote=203.0.113.10:24443"}, Config{
		XDPMode:   "skb",
		ExtraArgs: []string{"--padding=32"},
	})
	require.Equal(t, []string{
		"run", "eth0",
		"-f", "remote=203.0.113.10:24443",
		"--xdp-mode", "skb",
		"--padding=32",
	}, args)
}
```

- [ ] **Step 2: Run tests to verify RED**

```bash
go test ./internal/mimic -v
```

Expected: FAIL because the package does not exist.

- [ ] **Step 3: Implement common types and Linux manager**

`mimic.go`:

```go
package mimic

import (
	"net"
	"log/slog"
)

type Config struct {
	Enabled   bool
	Interface string
	XDPMode   string
	Path      string
	ExtraArgs []string
}

type Role int

const (
	RoleClient Role = iota
	RoleServer
)

type Instance struct {
	// unexported fields
}
```

`mimic_linux.go` implements:

- `runtimeDir` package variable defaulting to `/run/mimic`; allow `MIMIC_RUNTIME_DIR` env override for tests.
- `resolveBinary(path)` with `os.Stat`/`exec.LookPath`.
- `resolveInterface(configured, role, addr)`:
  - explicit interface exists;
  - client uses UDP dial to peer;
  - server wildcard uses default-route probe;
  - server specific IP uses the interface owning that IP; error if none.
- `filtersFor(role, addrs)` as tested.
- `Start`:
  - `!Enabled` → `(nil,nil)`;
  - no addrs → error;
  - resolve binary/interface/filters;
  - if a live Mimic lock exists on the interface → error telling the user to stop it (no reuse);
  - `exec.CommandContext` with `mimic run <iface> -f ... [--xdp-mode ...] extra...`;
  - `SysProcAttr{Pdeathsig: syscall.SIGTERM}`;
  - stderr pipe, line limit 4 KiB;
  - wait for lock file `/run/mimic/*_<ifindex>.lock` with our PID, 10s;
  - on failure close child and return error.
- `Instance.Close` cancels, waits for exit, idempotent.
- `Instance.wait`: call `cmd.Wait`, close `exited` **before** calling `onExit` so a callback that calls `Close` cannot deadlock; invoke `onExit` only if not intentionally stopped.
- `mimic_other.go` returns `errors.New("mimic is only supported on Linux")` when enabled.

- [ ] **Step 4: Add fake-binary lifecycle tests**

In `internal/mimic/mimic_linux_test.go`:

- Create a temporary executable shell script that:
  - reads the interface argument;
  - computes ifindex from `/sys/class/net/<iface>/ifindex`;
  - writes `$MIMIC_RUNTIME_DIR/<pid>_<ifindex>.lock` containing `pid=$$`;
  - sleeps until killed.
- Set `t.Setenv("MIMIC_RUNTIME_DIR", t.TempDir())`.
- Start with explicit `Interface: "lo"`, `Role: RoleServer`, a loopback address.
- Assert `Start` returns an instance and args contain `run lo`, `-f local=...`.
- `Close` returns and no child remains.
- A second test uses a script that exits immediately; assert `Start` errors and `onExit` is not called for a startup failure.
- A third test starts a script, kills it externally, and asserts `onExit` is called exactly once.

- [ ] **Step 5: Run Mimic tests**

```bash
go test ./internal/mimic -v
go test ./internal/mimic -race -v
```

Expected: PASS on Linux; non-Linux test skips/returns the expected error.

- [ ] **Step 6: Commit**

```bash
git add internal/mimic
git commit -m "feat(mimic): manage external Mimic child process"
```

---

### Task 3: Server runtime integration

**Files:**
- Modify: `internal/server/server.go`
- Modify: `internal/server/state.go` (only if overview needs mode; otherwise skip)
- Test: `internal/server/server_test.go`
- Test: `internal/server/hot_settings_test.go` (settings view only)

**Interfaces:**
- Consumes `internal/mimic.Start`, `MimicConfig`.
- Produces:
  - `type mimicProcess interface { Close() error }`
  - `type mimicStarter func(mimic.Config, mimic.Role, []*net.UDPAddr, *slog.Logger, func(error)) (mimicProcess, error)`
  - `Server.newMimic mimicStarter` (default wraps `mimic.Start`)
  - `Server.mimicInst mimicProcess`
  - `Server.activeMimic config.MimicConfig`
  - `Server.mimicActive bool`
  - `Server.mimicErr error`
  - `func (s *Server) startMimic() error`
  - `func (s *Server) stopMimic()`
  - `func (s *Server) UpdateMimic(cfg config.MimicConfig) error`

- [ ] **Step 1: Write failing server tests**

Add tests that:
- `Server.Start` with `Mimic.Enabled` calls `srv.newMimic` (set in the test to return a fake `mimicProcess`) before `hyServer.Serve`.
- On Mimic startup failure, `Start` returns the error and does not call `hyServer.Serve`.
- `UpdateMimic` writes the config but does not start/stop Mimic.
- `Settings().Mimic.Active` and `RestartRequired` reflect config vs runtime.
- `Close` stops Mimic exactly once.
- `QUICConfig.DisableGSO` is true when Mimic is enabled and false otherwise (extract a helper `quicConfigFor(cfg)` to make it testable).

- [ ] **Step 2: Run tests to verify RED**

```bash
go test ./internal/server -run 'Mimic|GSO' -v
```

Expected: FAIL because the fields/methods do not exist.

- [ ] **Step 3: Implement server integration**

In `internal/server/server.go`:

- Add fields:
```go
mimicMu     sync.Mutex
mimicInst   mimicProcess
activeMimic config.MimicConfig
mimicActive bool
mimicErr    error
newMimic    mimicStarter
```
- In `New`, set `newMimic` to a wrapper around `mimic.Start` returning `mimicProcess`.
- Add `startMimic`:
  - if config disabled, set active false and return nil;
  - `s.newMimic(mimic.Config{...}, mimic.RoleServer, []*net.UDPAddr{s.listenAddr.(*net.UDPAddr)}, s.log, func(err error){ s.setMimicError(err); go s.Close() })`;
  - set active config and true.
- In `Start`, call `s.startMimic()` before `s.hyServer.Serve`; on error return.
- In `Close`, call `s.stopMimic()` after HY2/packet conn close; clear active.
- In `Start`'s `serveErr` branch, if `s.mimicErr != nil`, return it.
- Set `DisableGSO: cfg.Mimic != nil && cfg.Mimic.Enabled` in `hyserver.QUICConfig`.
- Add `UpdateMimic` using `settingsMu`, `cloneConfig`, `next.Mimic = cfg.Clone()`, `next.Validate()`, `persistConfig`, `s.cfg = next`; no runtime changes.
- Deep-copy `Mimic` in `cloneConfig`.
- Add `Mimic` to `Settings()` via a helper `mimicSettingsFor(cfg, active, activeConfig)`; add `RestartSupported` from `internal/restart.Supported()`.

- [ ] **Step 4: Run server tests**

```bash
go test ./internal/server -run 'Mimic|GSO' -v
go test ./internal/server
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/server
git commit -m "feat(server): start Mimic before HY2 and expose mode"
```

---

### Task 4: Client runtime integration

**Files:**
- Modify: `internal/client/client.go`
- Modify: `internal/client/configsync.go` (deep-copy Mimic)
- Modify: `internal/client/control.go` (GSO)
- Test: `internal/client/client_test.go`
- Test: `internal/client/settings_test.go`

**Interfaces:**
- Consumes `internal/mimic.Start`, `MimicConfig`.
- Produces:
  - `type mimicProcess interface { Close() error }`
  - `type mimicStarter func(mimic.Config, mimic.Role, []*net.UDPAddr, *slog.Logger, func(error)) (mimicProcess, error)`
  - `Client.newMimic mimicStarter` (default wraps `mimic.Start`)
  - `Client.mimicInst mimicProcess`
  - `Client.activeMimic config.MimicConfig`
  - `Client.mimicActive bool`
  - `Client.mimicErr error`
  - `func (c *Client) startMimic() error`
  - `func (c *Client) stopMimic()`
  - `func (c *Client) UpdateMimic(cfg config.MimicConfig) error`

- [ ] **Step 1: Write failing client tests**

Add tests that:
- `Client.Start` with `Mimic.Enabled` calls a fake Mimic start before the first HY2 connection.
- Mimic startup failure returns from `Start` and does not connect.
- Mimic unexpected exit sets `mimicErr`, closes the client, and `Start` returns the error (no reconnect loop).
- `UpdateMimic` writes config but does not start/stop Mimic.
- `Settings().Mimic.Active` / `RestartRequired` are correct.
- `buildHY2Config`/`runOnce` sets `QUICConfig.DisableGSO` when Mimic is enabled.
- `cloneClientConfig` deep-copies `Mimic`.

- [ ] **Step 2: Run tests to verify RED**

```bash
go test ./internal/client -run 'Mimic|GSO' -v
```

Expected: FAIL because the fields/methods do not exist.

- [ ] **Step 3: Implement client integration**

In `internal/client/client.go`:

- Add the same runtime fields as the server, including `newMimic mimicStarter`; set it to a wrapper around `mimic.Start` in `New`.
- `startMimic`:
  - resolve `cfg.Server` to `[]*net.UDPAddr` (single port only; config validation already rejects hopping);
  - `c.newMimic(..., mimic.RoleClient, addrs, c.log, onExit)`;
  - onExit sets `mimicErr` and calls `go c.Close()`.
- In `Start`, call `startMimic` before web/proxy/loop; on error return.
- At every loop exit, return `c.mimicError()` if set; do not return nil after unexpected Mimic exit.
- In `Close`, call `stopMimic` after HY2/control/proxy close.
- In `runOnce`, set:
```go
QUICConfig: hyclient.QUICConfig{
    DisableGSO: cfg.Mimic != nil && cfg.Mimic.Enabled,
},
```
- Add `UpdateMimic` using `configMu`, `cloneClientConfig`, `next.Mimic = cfg.Clone()`, `next.Validate()`, `config.SaveClient`, update `c.cfg`; no runtime changes.
- Add `Mimic` to `Settings()` and `RestartSupported`.
- Deep-copy `Mimic` in `cloneClientConfig`.

- [ ] **Step 4: Run client tests**

```bash
go test ./internal/client -run 'Mimic|GSO' -v
go test ./internal/client
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/client
git commit -m "feat(client): start Mimic before HY2 and expose mode"
```

---

### Task 5: Server Web Mimic settings and restart

**Files:**
- Create: `internal/restart/restart.go`
- Test: `internal/restart/restart_test.go`
- Modify: `internal/webui/serverui/views.go`
- Modify: `internal/webui/serverui/handlers.go`
- Modify: `internal/webui/serverui/templates/settings.html`
- Modify: `internal/webui/serverui/static/app.js`
- Modify: `internal/webui/serverui/handlers_test.go`
- Modify: `internal/webui/serverui/views_test.go` (or existing tests)

**Interfaces:**
- Produces:
  - `restart.Supported() bool`
  - `restart.Schedule(log *slog.Logger)`
  - `serverui.MimicSettingsView`
  - `serverui.API.UpdateMimic(config.MimicConfig) error`
  - `PUT /api/settings/mimic`
  - `POST /api/restart`

- [ ] **Step 1: Write failing Web tests**

Add tests for:
- `GET /api/settings` includes `mimic` and `restart_supported`.
- `PUT /api/settings/mimic` requires password and CSRF; writes via API; returns settings; does not call runtime.
- Invalid Mimic + port hopping returns 400 and leaves config unchanged.
- `POST /api/restart` requires password/CSRF; returns 409 when `restart.Supported` is false; returns 202 and calls an injected schedule function when true.
- Settings page contains the Mimic form and restart button when supported.

- [ ] **Step 2: Run tests to verify RED**

```bash
go test ./internal/webui/serverui -run 'Mimic|Restart' -v
go test ./internal/restart -v
```

Expected: FAIL because the types/endpoints do not exist.

- [ ] **Step 3: Implement restart helper**

`internal/restart/restart.go`:

```go
func Supported() bool {
	return os.Getenv("INVOCATION_ID") != ""
}

func Schedule(log *slog.Logger) {
	go func() {
		time.Sleep(300 * time.Millisecond)
		log.Info("restart requested; exiting for systemd")
		os.Exit(0)
	}()
}
```

Tests cover `Supported` with/without `INVOCATION_ID`; do not call `Schedule` in tests (it exits).

- [ ] **Step 4: Implement server Web API/UI**

- Add `MimicSettingsView` and `RestartSupported` to `SettingsView`.
- Add `UpdateMimic` to `API`.
- Add routes:
```go
mux.Handle("PUT /api/settings/mimic", s.auth(shared.RequireCSRF(http.HandlerFunc(s.handleUpdateMimic))))
mux.Handle("POST /api/restart", s.auth(shared.RequireCSRF(http.HandlerFunc(s.handleRestart))))
```
- `handleUpdateMimic` decodes:
```go
struct {
    passwordRequest
    Enabled   bool     `json:"enabled"`
    Interface string   `json:"interface"`
    XDPMode   string   `json:"xdp_mode"`
    Path      string   `json:"path"`
    ExtraArgs []string `json:"extra_args"`
}
```
  confirms password, calls `api.UpdateMimic(config.MimicConfig{...})`, returns `s.api.Settings()`.
- `handleRestart` confirms password, checks `restart.Supported()`, returns 409 `restart_unavailable` if false, otherwise writes `202 {"restarting":true}` and calls an injectable `scheduleRestart(s.log)`.
- Add Mimic panel to `settings.html`:
  - checkbox, interface, xdp mode select, path, extra args textarea;
  - current/pending mode text;
  - warning for server mode;
  - restart button only when `RestartSupported`, with password input and confirm.
- Add JS form submit and restart button; update mode text; show restart required.
- Add `restart_supported` to settings JSON.

- [ ] **Step 5: Run server Web tests**

```bash
go test ./internal/webui/serverui -run 'Mimic|Restart' -v
go test ./internal/webui/serverui
```

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/restart internal/webui/serverui
git commit -m "feat(serverui): edit Mimic mode and restart server"
```

---

### Task 6: Client Web Mimic settings and restart

**Files:**
- Modify: `internal/webui/clientui/views.go`
- Modify: `internal/webui/clientui/handlers.go`
- Modify: `internal/webui/clientui/templates/index.html`
- Modify: `internal/webui/clientui/static/app.js`
- Modify: `internal/webui/clientui/handlers_test.go`
- Modify: `internal/webui/clientui/views_test.go` (or existing tests)

**Interfaces:**
- Produces:
  - `clientui.MimicSettingsView`
  - `clientui.API.UpdateMimic(config.MimicConfig) error`
  - `PUT /api/settings/mimic`
  - `POST /api/restart`

- [ ] **Step 1: Write failing client Web tests**

Mirror Task 5:
- settings JSON includes `mimic`, `restart_supported`;
- update requires password/CSRF; writes via API; no runtime;
- invalid config returns 400;
- restart 409 without systemd, 202 with injected schedule;
- settings page contains Mimic form and restart button.

- [ ] **Step 2: Run tests to verify RED**

```bash
go test ./internal/webui/clientui -run 'Mimic|Restart' -v
```

Expected: FAIL.

- [ ] **Step 3: Implement client Web API/UI**

- Add `MimicSettingsView` and `RestartSupported` to `SettingsView`.
- Add `UpdateMimic` to `API`.
- Add routes/handlers identical to Task 5, using `s.api.UpdateMimic`.
- Add Mimic panel in `index.html`:
  - warning: both server and client must enable;
  - fields and restart button;
  - current/pending mode.
- Add JS form/restart handlers.
- Reuse `internal/restart`.

- [ ] **Step 4: Run client Web tests**

```bash
go test ./internal/webui/clientui -run 'Mimic|Restart' -v
go test ./internal/webui/clientui
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/webui/clientui
git commit -m "feat(clientui): edit Mimic mode and restart client"
```

---

### Task 7: Documentation, examples, and acceptance

**Files:**
- Modify: `README.md`
- Modify: `README.zh-CN.md`
- Modify: `examples/server.yaml`
- Modify: `examples/client.yaml`
- Create: `scripts/mimic/README.md`
- No code changes unless a doc check reveals a bug.

**Interfaces:**
- No new interfaces. Documents `mimic.enabled`, Web save/restart, systemd, GSO/MTU, firewall, licensing, switch procedure.

- [ ] **Step 1: Add example config**

In both example files, add a commented block:

```yaml
# Optional Mimic mode (Linux only; external GPL-2.0 mimic binary required).
# Both server and client must enable it. It cannot be combined with port
# hopping. Changing this requires a restart.
# mimic:
#   enabled: false
#   interface: ""
#   xdp_mode: ""
#   path: ""
#   extra_args: []
```

- [ ] **Step 2: Add README sections**

Both languages:
- Normal vs Mimic table.
- Config fields.
- Web save/restart instructions; two-step switch.
- Systemd `Restart=always` requirement; non-systemd 409.
- Server warning that ordinary clients disconnect.
- Mimic install commands for Ubuntu 24.04 amd64/arm64; headers/DKMS; GPL.
- GSO/MTU 12-byte overhead; firewall TCP+UDP.
- No coexistence; rollback procedure.
- `scripts/mimic/README.md` link.

- [ ] **Step 3: Add `scripts/mimic/README.md`**

Include:
- install `mimic` and `mimic-dkms` packages;
- kernel >=6.1 and matching headers;
- `mimic run` example/filter explanation;
- `nft`/firewall TCP+UDP note;
- `xdp_mode=skb` troubleshooting;
- GPL note and no bundling.

- [ ] **Step 4: Run docs checks**

```bash
git diff --check
python3 - <<'PY'
from pathlib import Path
for name in ("README.md", "README.zh-CN.md"):
    n = Path(name).read_text().count(chr(96) * 3)
    assert n % 2 == 0, (name, n)
print("markdown fences OK")
PY
```

- [ ] **Step 5: Commit**

```bash
git add README.md README.zh-CN.md examples scripts/mimic/README.md
git commit -m "docs: document Mimic mode and Web switching"
```

---

### Task 8: Full verification and isolated acceptance

**Files:**
- No source changes unless a failure requires a fix.

**Interfaces:**
- Consumes all prior tasks.

- [ ] **Step 1: Full tests**

```bash
go test ./... -count=1
go test -race ./... -count=1
make build
```

Expected: PASS.

- [ ] **Step 2: Isolated Mimic experiment**

Use a separate test port, not production `24443`:

1. Install `mimic`/`mimic-dkms` and matching headers on server and client.
2. Start a test `hy2tund` on a non-production port.
3. Enable Mimic on both test ends.
4. Verify control handshake, TCP mapping, UDP mapping.
5. Verify `tcpdump` shows TCP-shaped packets.
6. Test `obfs.salamander + mimic`.
7. Test MTU 1420/1408/1280.
8. Compare throughput/CPU with normal mode.
9. Verify Web save + restart on both ends; confirm `RestartRequired` and 409 outside systemd.
10. Confirm normal mode and port hopping still work after rollback.

- [ ] **Step 3: Record results and stop**

If all pass, report the test port, Mimic version, kernel, NIC/XDP mode, throughput, and any limitations. Do not enable Mimic on production until the user explicitly asks.
